package permissions

import (
	"fmt"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/gavv/httpexpect/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPermissions(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())

	testInstance := setup.GetTestInstance()
	scopes := "io.cozy.contacts io.cozy.files:GET io.cozy.events"
	clientVal, token := setup.GetTestClient(scopes)
	clientID := clientVal.ClientID

	ts := setup.GetTestServer("/permissions", Routes)
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	t.Run("CreateShareSetByMobileRevokeByLinkedApp", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create OAuthLinkedClient
		oauthLinkedClient := &oauth.Client{
			ClientName:   "test-linked-shareset",
			RedirectURIs: []string{"https://foobar"},
			SoftwareID:   "registry://drive",
		}
		oauthLinkedClient.Create(testInstance)

		// Install the app
		installer, err := app.NewInstaller(testInstance, app.Copier(consts.WebappType, testInstance), &app.InstallerOptions{
			Operation:  app.Install,
			Type:       consts.WebappType,
			SourceURL:  "registry://drive",
			Slug:       "drive",
			Registries: testInstance.Registries(),
		})
		assert.NoError(t, err)
		_, err = installer.RunSync()
		assert.NoError(t, err)

		// Generate a token for the client
		tok, err := testInstance.MakeJWT(consts.AccessTokenAudience,
			oauthLinkedClient.ClientID, "@io.cozy.apps/drive", "", time.Now())
		assert.NoError(t, err)

		// Request to create a permission
		obj := e.POST("/permissions").
			WithQuery("codes", "email").
			WithHost(testInstance.Domain).
			WithHeader("Authorization", "Bearer "+tok).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(fmt.Sprintf(`{
          "data": {
            "id": "%s",
            "type": "io.cozy.permissions",
            "attributes": {
              "permissions": {
                "files": {
                  "type": "io.cozy.files",
                  "verbs": ["GET"]
                }
              }
            }
          }
        }`, oauthLinkedClient.ClientID))).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		// Assert the permission received does not have the clientID as source_id
		obj.Path("$.data.attributes.source_id").String().NotEqual(oauthLinkedClient.ClientID)
		permID := obj.Path("$.data.id").String().NotEmpty().Raw()

		// Create a webapp token
		webAppToken, err := testInstance.MakeJWT(consts.AppAudience, "drive", "", "", time.Now())
		assert.NoError(t, err)

		// Login to webapp and try to delete the shared link
		e.DELETE("/permissions/"+permID).
			WithHost(testInstance.Domain).
			WithHeader("Authorization", "Bearer "+webAppToken).
			Expect().Status(204)

		// Cleaning
		oauthLinkedClient, err = oauth.FindClientBySoftwareID(testInstance, "registry://drive")
		assert.NoError(t, err)
		oauthLinkedClient.Delete(testInstance)

		uninstaller, err := app.NewInstaller(testInstance, app.Copier(consts.WebappType, testInstance),
			&app.InstallerOptions{
				Operation:  app.Delete,
				Type:       consts.WebappType,
				Slug:       "drive",
				SourceURL:  "registry://drive",
				Registries: testInstance.Registries(),
			},
		)
		assert.NoError(t, err)

		_, err = uninstaller.RunSync()
		assert.NoError(t, err)
	})

	t.Run("CreateShareSetByLinkedAppRevokeByMobile", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create a webapp token
		webAppToken, err := testInstance.MakeJWT(consts.AppAudience, "drive", "", "", time.Now())
		assert.NoError(t, err)

		// Install the app
		installer, err := app.NewInstaller(testInstance, app.Copier(consts.WebappType, testInstance), &app.InstallerOptions{
			Operation:  app.Install,
			Type:       consts.WebappType,
			SourceURL:  "registry://drive",
			Slug:       "drive",
			Registries: testInstance.Registries(),
		})
		assert.NoError(t, err)
		_, err = installer.RunSync()
		assert.NoError(t, err)

		// Request to create a permission
		obj := e.POST("/permissions").
			WithQuery("codes", "email").
			WithHost(testInstance.Domain).
			WithHeader("Authorization", "Bearer "+webAppToken).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
          "data": {
            "id": "io.cozy.apps/drive",
            "type": "io.cozy.permissions",
            "attributes": {
              "permissions": {
                "files": {
                  "type": "io.cozy.files",
                  "verbs": ["GET"]
                }
              }
            }
          }
        }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		permSourceID := obj.Path("$.data.attributes.source_id").String().NotEmpty().Raw()
		permID := obj.Path("$.data.id").String().NotEmpty().Raw()

		// Create OAuthLinkedClient
		oauthLinkedClient := &oauth.Client{
			ClientName:   "test-linked-shareset2",
			RedirectURIs: []string{"https://foobar"},
			SoftwareID:   "registry://drive",
		}
		oauthLinkedClient.Create(testInstance)

		// Generate a token for the client
		tok, err := testInstance.MakeJWT(consts.AccessTokenAudience,
			oauthLinkedClient.ClientID, "@io.cozy.apps/drive", "", time.Now())
		assert.NoError(t, err)

		// Assert the permission received does not have the clientID as source_id
		assert.NotEqual(t, permSourceID, oauthLinkedClient.ClientID)

		// Login to webapp and try to delete the shared link
		e.DELETE("/permissions/"+permID).
			WithHost(testInstance.Domain).
			WithHeader("Authorization", "Bearer "+tok).
			Expect().Status(204)

		// Cleaning
		oauthLinkedClient, err = oauth.FindClientBySoftwareID(testInstance, "registry://drive")
		assert.NoError(t, err)
		oauthLinkedClient.Delete(testInstance)

		uninstaller, err := app.NewInstaller(testInstance, app.Copier(consts.WebappType, testInstance),
			&app.InstallerOptions{
				Operation:  app.Delete,
				Type:       consts.WebappType,
				Slug:       "drive",
				SourceURL:  "registry://drive",
				Registries: testInstance.Registries(),
			},
		)
		assert.NoError(t, err)

		_, err = uninstaller.RunSync()
		assert.NoError(t, err)
	})

	t.Run("GetPermissions", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.GET("/permissions/self").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		perms := obj.Path("$.data.attributes.permissions").Object()

		for key, r := range perms.Iter() {
			switch key {
			case "rule1":
				r.Object().ValueEqual("type", "io.cozy.files")
				r.Object().ValueEqual("verbs", []interface{}{"GET"})
			case "rule0":
				r.Object().ValueEqual("type", "io.cozy.contacts")
			default:
				r.Object().ValueEqual("type", "io.cozy.events")
			}
		}
	})

	t.Run("GetPermissionsForRevokedClient", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		tok, err := testInstance.MakeJWT(consts.AccessTokenAudience,
			"revoked-client",
			"io.cozy.contacts io.cozy.files:GET",
			"", time.Now())
		assert.NoError(t, err)

		res := e.GET("/permissions/self").
			WithHeader("Authorization", "Bearer "+tok).
			Expect().Status(400)

		res.Text().Equal(`Invalid JWT token`)
		res.Header("WWW-Authenticate").Equal(`Bearer error="invalid_token"`)
	})

	t.Run("GetPermissionsForExpiredToken", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		pastTimestamp := time.Now().Add(-30 * 24 * time.Hour) // in seconds

		tok, err := testInstance.MakeJWT(consts.AccessTokenAudience,
			clientID, "io.cozy.contacts io.cozy.files:GET", "", pastTimestamp)
		assert.NoError(t, err)

		res := e.GET("/permissions/self").
			WithHeader("Authorization", "Bearer "+tok).
			Expect().Status(400)

		res.Text().Equal("Expired token")
		res.Header("WWW-Authenticate").Equal(`Bearer error="invalid_token" error_description="The access token expired"`)
	})

	t.Run("BadPermissionsBearer", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.GET("/permissions/self").
			WithHeader("Authorization", "Bearer barbage").
			Expect().Status(400)
	})

	t.Run("CreateSubPermission", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		id, codes, err := createTestSubPermissions(e, token, "alice,bob")
		require.NoError(t, err)

		aCode := codes.Value("alice").String().NotEmpty().Raw()
		bCode := codes.Value("bob").String().NotEmpty().Raw()
		otherID, otherCodes, err := createTestSubPermissions(e, token, "alice")
		require.NoError(t, err)
		otherCode := otherCodes.Value("alice").String().NotEmpty().Raw()

		assert.NotEqual(t, aCode, token)
		assert.NotEqual(t, bCode, token)
		assert.NotEqual(t, aCode, bCode)
		assert.NotEqual(t, id, otherID)
		assert.NotEqual(t, aCode, otherCode)
		for _, share := range []struct {
			permissionID string
			code         string
		}{{id, aCode}, {id, bCode}, {otherID, otherCode}} {
			var claims permission.Claims
			err := crypto.ParseJWT(share.code, func(_ *jwt.Token) (interface{}, error) {
				return testInstance.PickKey(consts.ShareAudience)
			}, &claims)
			require.NoError(t, err)
			assert.Equal(t, share.permissionID, claims.ID)
		}

		obj := e.GET("/permissions/self").
			WithHeader("Authorization", "Bearer "+aCode).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		perms := obj.Path("$.data.attributes.permissions").Object()
		perms.Keys().Length().Equal(2)
		perms.Path("$.whatever.type").String().Equal("io.cozy.files")

		e.GET("/permissions/self").
			WithHeader("Authorization", "Bearer "+otherCode).
			Expect().Status(200)
	})

	t.Run("CreateSubSubFail", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		_, codes, err := createTestSubPermissions(e, token, "eve")
		require.NoError(t, err)

		eveCode := codes.Value("eve").String().NotEmpty().Raw()

		e.POST("/permissions").
			WithQuery("codes", codes).
			WithHeader("Authorization", "Bearer "+eveCode).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
      "data": {
        "type": "io.cozy.permissions",
        "attributes": {
          "permissions": {
            "whatever": {
              "type":   "io.cozy.files",
              "verbs":  ["GET"],
              "values": ["io.cozy.music"]
            },
            "otherrule": {
              "type":   "io.cozy.files",
              "verbs":  ["GET"],
              "values":  ["some-other-dir"]
            }
          }
        }
      }
    }`)).
			Expect().Status(403)
	})

	t.Run("PatchNoopFail", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		id, _, err := createTestSubPermissions(e, token, "pierre")
		require.NoError(t, err)

		e.PATCH("/permissions/"+id).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
		  "data": {
		    "id": "a340d5e0-d647-11e6-b66c-5fc9ce1e17c6",
		    "type": "io.cozy.permissions",
		    "attributes": { }
		    }
		  }
    }`)).
			Expect().Status(400)
	})

	t.Run("BadPatchAddRuleForbidden", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		id, _, err := createTestSubPermissions(e, token, "jacque")
		require.NoError(t, err)

		e.PATCH("/permissions/"+id).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
        "data": {
          "attributes": {
              "permissions": {
                "otherperm": {
                  "type":"io.cozy.token.cant.do.this"
                }
              }
            }
          }
      }`)).
			Expect().Status(403)
	})

	t.Run("PatchAddRule", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		id, _, err := createTestSubPermissions(e, token, "paul")
		require.NoError(t, err)

		obj := e.PATCH("/permissions/"+id).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
		  "data": {
		    "attributes": {
						"permissions": {
							"otherperm": {
								"type":"io.cozy.contacts"
							}
						}
					}
		    }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		perms := obj.Path("$.data.attributes.permissions").Object()
		perms.Keys().Length().Equal(3)
		perms.Path("$.whatever.type").String().Equal("io.cozy.files")
		perms.Path("$.otherperm.type").String().Equal("io.cozy.contacts")
	})

	t.Run("PatchRemoveRule", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		id, _, err := createTestSubPermissions(e, token, "paul")
		require.NoError(t, err)

		obj := e.PATCH("/permissions/"+id).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
		  "data": {
		    "attributes": {
						"permissions": {
							"otherrule": { }
						}
					}
		    }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		perms := obj.Path("$.data.attributes.permissions").Object()
		perms.Keys().Length().Equal(1)
		perms.Path("$.whatever.type").String().Equal("io.cozy.files")
	})

	t.Run("PatchChangesCodes", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		id, codes, err := createTestSubPermissions(e, token, "john,jane")
		require.NoError(t, err)

		codes.Value("john").String().NotEmpty()
		janeToken := codes.Value("jane").String().NotEmpty().Raw()

		e.PATCH("/permissions/"+id).
			WithHeader("Authorization", "Bearer "+janeToken).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
			"data": {
				"attributes": {
						"codes": {
							"john": "set-token"
						}
					}
				}
      }`)).
			Expect().Status(403)

		obj := e.PATCH("/permissions/"+id).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
		  "data": {
		    "attributes": {
						"codes": {
							"john": "set-token"
						}
					}
		    }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.data.id").String().Equal(id)

		codes = obj.Path("$.data.attributes.codes").Object()
		codes.Value("john").String().NotEmpty()
		codes.NotContainsKey("jane")
	})

	t.Run("Revoke", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		id, codes, err := createTestSubPermissions(e, token, "igor")
		require.NoError(t, err)

		igorToken := codes.Value("igor").String().NotEmpty().Raw()

		e.DELETE("/permissions/"+id).
			WithHeader("Authorization", "Bearer "+igorToken).
			WithHeader("Content-Type", "application/json").
			Expect().Status(403)

		e.DELETE("/permissions/"+id).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			Expect().Status(204)
	})

	t.Run("RevokeByAnotherApp", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		id, _, err := createTestSubPermissions(e, token, "roger")
		require.NoError(t, err)

		installer, err := app.NewInstaller(testInstance, app.Copier(consts.WebappType, testInstance), &app.InstallerOptions{
			Operation:  app.Install,
			Type:       consts.WebappType,
			SourceURL:  "registry://notes",
			Slug:       "notes",
			Registries: testInstance.Registries(),
		})
		assert.NoError(t, err)
		_, err = installer.RunSync()
		require.NoError(t, err)

		notesToken, err := testInstance.MakeJWT(consts.AppAudience, "notes", "", "", time.Now())
		assert.NoError(t, err)

		e.DELETE("/permissions/"+id).
			WithHeader("Authorization", "Bearer "+notesToken).
			Expect().Status(204)

		// Cleaning
		uninstaller, err := app.NewInstaller(testInstance, app.Copier(consts.WebappType, testInstance),
			&app.InstallerOptions{
				Operation:  app.Delete,
				Type:       consts.WebappType,
				Slug:       "notes",
				SourceURL:  "registry://notes",
				Registries: testInstance.Registries(),
			},
		)
		assert.NoError(t, err)
		_, err = uninstaller.RunSync()
		assert.NoError(t, err)
	})

	t.Run("GetPermissionsWithShortCode", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		id, _, _ := createTestSubPermissions(e, token, "daniel")
		perm, _ := permission.GetByID(testInstance, id)

		assert.NotNil(t, perm.ShortCodes)

		e.GET("/permissions/self").
			WithHeader("Authorization", "Bearer "+perm.ShortCodes["daniel"]).
			Expect().Status(200)
	})

	t.Run("GetPermissionsWithBadShortCode", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		id, _, _ := createTestSubPermissions(e, token, "alice")
		perm, _ := permission.GetByID(testInstance, id)

		assert.NotNil(t, perm.ShortCodes)

		e.GET("/permissions/self").
			WithHeader("Authorization", "Bearer foobar").
			Expect().Status(400)
	})

	t.Run("GetTokenFromShortCode", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		id, _, _ := createTestSubPermissions(e, token, "alice")
		perm, _ := permission.GetByID(testInstance, id)

		tok, _ := permission.GetTokenFromShortcode(testInstance, perm.ShortCodes["alice"])
		assert.Equal(t, perm.Codes["alice"], tok)
	})

	t.Run("GetBadShortCode", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		_, _, err := createTestSubPermissions(e, token, "alice")
		assert.NoError(t, err)
		shortcode := "coincoin"

		tok, err := permission.GetTokenFromShortcode(testInstance, shortcode)
		assert.Empty(t, tok)
		assert.NotNil(t, err)
		assert.Contains(t, err.Error(), "no permission doc for shortcode")
	})

	t.Run("GetMultipleShortCode", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		id, _, _ := createTestSubPermissions(e, token, "alice")
		id2, _, _ := createTestSubPermissions(e, token, "alice")
		perm, _ := permission.GetByID(testInstance, id)
		perm2, _ := permission.GetByID(testInstance, id2)

		perm2.ShortCodes["alice"] = perm.ShortCodes["alice"]
		assert.NoError(t, couchdb.UpdateDoc(testInstance, perm2))

		_, err := permission.GetTokenFromShortcode(testInstance, perm.ShortCodes["alice"])

		assert.NotNil(t, err)
		assert.Contains(t, err.Error(), "several permission docs for shortcode")
	})

	t.Run("CannotFindToken", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		id, _, _ := createTestSubPermissions(e, token, "alice")
		perm, _ := permission.GetByID(testInstance, id)
		perm.Codes = map[string]string{}
		assert.NoError(t, couchdb.UpdateDoc(testInstance, perm))

		_, err := permission.GetTokenFromShortcode(testInstance, perm.ShortCodes["alice"])
		assert.NotNil(t, err)
		assert.Contains(t, err.Error(), "Cannot find token for shortcode")
	})

	t.Run("TinyShortCodeOK", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		id, codes, _ := createTestTinyCode(e, token, "elise", "30m")
		code := codes.Value("elise").String().NotEmpty().Raw()
		assert.Len(t, code, 6)

		perm, _ := permission.GetByID(testInstance, id)
		assert.Equal(t, code, perm.ShortCodes["elise"])

		assert.NotNil(t, perm.ShortCodes)

		e.GET("/permissions/self").
			WithHeader("Authorization", "Bearer "+perm.ShortCodes["elise"]).
			Expect().Status(200)

		tok, _ := permission.GetTokenFromShortcode(testInstance, perm.ShortCodes["elise"])
		assert.Equal(t, perm.Codes["elise"], tok)
	})

	t.Run("TinyShortCodeInvalid", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		_, codes, _ := createTestTinyCode(e, token, "fanny", "24h")

		code := codes.Value("fanny").String().NotEmpty().Raw()
		assert.Len(t, code, 12)
	})

	t.Run("GetForOauth", func(t *testing.T) {
		// Install app
		installer, err := app.NewInstaller(testInstance, app.Copier(consts.WebappType, testInstance), &app.InstallerOptions{
			Operation:  app.Install,
			Type:       consts.WebappType,
			SourceURL:  "registry://settings",
			Slug:       "settings",
			Registries: testInstance.Registries(),
		})
		assert.NoError(t, err)
		installer.Run()

		// Get app manifest
		manifest, err := app.GetBySlug(testInstance, "settings", consts.WebappType)
		assert.NoError(t, err)

		// Create OAuth client
		var oauthClient oauth.Client

		u := "https://example.org/oauth/callback"

		oauthClient.RedirectURIs = []string{u}
		oauthClient.ClientName = "cozy-test-2"
		oauthClient.SoftwareID = "registry://settings"
		oauthClient.Create(testInstance)

		parent, err := middlewares.GetForOauth(testInstance, &permission.Claims{
			RegisteredClaims: jwt.RegisteredClaims{
				Audience: jwt.ClaimStrings{consts.AccessTokenAudience},
				Issuer:   testInstance.Domain,
				IssuedAt: jwt.NewNumericDate(time.Now()),
				Subject:  clientID,
			},
			Scope: "@io.cozy.apps/settings",
		}, &oauthClient)
		assert.NoError(t, err)
		assert.True(t, parent.Permissions.HasSameRules(manifest.Permissions()))
	})

	t.Run("ListPermission", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		ev1, _ := createTestEvent(testInstance)
		ev2, _ := createTestEvent(testInstance)
		ev3, _ := createTestEvent(testInstance)

		parent, _ := middlewares.GetForOauth(testInstance, &permission.Claims{
			RegisteredClaims: jwt.RegisteredClaims{
				Audience: jwt.ClaimStrings{consts.AccessTokenAudience},
				Issuer:   testInstance.Domain,
				IssuedAt: jwt.NewNumericDate(time.Now()),
				Subject:  clientID,
			},
			Scope: "io.cozy.events",
		}, clientVal)

		p1 := permission.Set{
			permission.Rule{
				Type:   "io.cozy.events",
				Verbs:  permission.Verbs(permission.DELETE, permission.PATCH),
				Values: []string{ev1.ID()},
			},
		}
		p2 := permission.Set{
			permission.Rule{
				Type:   "io.cozy.events",
				Verbs:  permission.Verbs(permission.GET),
				Values: []string{ev2.ID()},
			},
		}

		perm1 := permission.Permission{
			Permissions: p1,
		}
		perm2 := permission.Permission{
			Permissions: p2,
		}
		codes := map[string]string{"bob": "secret"}
		_, _ = permission.CreateShareSet(testInstance, parent, parent.SourceID, codes, nil, perm1, nil, false)
		_, _ = permission.CreateShareSet(testInstance, parent, parent.SourceID, codes, nil, perm2, nil, false)

		obj := e.POST("/permissions/exists").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
        "data": [
          { "type": "io.cozy.events", "id": "` + ev1.ID() + `" },
          { "type": "io.cozy.events", "id": "` + ev2.ID() + `" },
          { "type": "io.cozy.events", "id": "non-existing-id" },
          { "type": "io.cozy.events", "id": "another-fake-id" },
          { "type": "io.cozy.events", "id": "` + ev3.ID() + `" }
        ]	
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Array()
		data.Length().Equal(2)

		res := data.Find(func(_ int, value *httpexpect.Value) bool {
			value.Object().ValueEqual("id", ev1.ID())
			return true
		})
		res.Object().ValueEqual("type", "io.cozy.events")
		res.Object().ValueEqual("verbs", []string{"PATCH", "DELETE"})

		res = data.Find(func(_ int, value *httpexpect.Value) bool {
			value.Object().ValueEqual("id", ev2.ID())
			return true
		})
		res.Object().ValueEqual("type", "io.cozy.events")
		res.Object().ValueEqual("verbs", []string{"GET"})

		obj = e.GET("/permissions/doctype/io.cozy.events/shared-by-link").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data = obj.Value("data").Array()
		data.Length().Equal(2)
		data.Element(0).Object().Value("id").String().
			NotEqual(data.Element(1).Object().Value("id").String().Raw())

		obj = e.GET("/permissions/doctype/io.cozy.events/shared-by-link").
			WithQuery("page[limit]", 1).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data = obj.Value("data").Array()
		data.Length().Equal(1)
		obj.Path("$.links.next").String().NotEmpty()
	})

	t.Run("ShowPermissions", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)
		id, _, _ := createTestSubPermissions(e, token, "alice")

		obj := e.GET("/permissions/"+id).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		perms := obj.Path("$.data.attributes.permissions").Object()

		for _, r := range perms.Iter() {
			r.Object().ValueEqual("type", "io.cozy.files")
			r.Object().ValueEqual("verbs", []interface{}{"GET"})
		}
	})

	t.Run("ShowPermissionsFail", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)
		id, _, _ := createTestSubPermissions(e, token, "alice")
		_, otherToken := setup.GetTestClient("io.cozy.tags")

		e.GET("/permissions/"+id).
			WithHeader("Authorization", "Bearer "+otherToken).
			Expect().Status(403)
	})

	t.Run("CreatePermissionWithoutMetadata", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Install the app
		installer, err := app.NewInstaller(testInstance, app.Copier(consts.WebappType, testInstance), &app.InstallerOptions{
			Operation:  app.Install,
			Type:       consts.WebappType,
			SourceURL:  "registry://drive",
			Slug:       "drive",
			Registries: testInstance.Registries(),
		})
		assert.NoError(t, err)
		_, err = installer.RunSync()
		assert.NoError(t, err)

		tok, err := testInstance.MakeJWT(permission.TypeWebapp,
			"drive", "io.cozy.files", "", time.Now())
		assert.NoError(t, err)

		// Request to create a permission
		obj := e.POST("/permissions").
			WithHeader("Authorization", "Bearer "+tok).
			WithHeader("Content-Type", "application/json").
			WithHost(testInstance.Domain).
			WithBytes([]byte(`{
        "data": {
          "type": "io.cozy.permissions",
          "attributes": {
            "permissions": {
              "files": {
                "type": "io.cozy.files",
                "verbs": ["GET"]
              }
            }
          }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

			// Assert a cozyMetadata has been added
		meta := obj.Path("$.data.attributes.cozyMetadata").Object()
		meta.ValueEqual("createdByApp", "drive")
		meta.ValueEqual("doctypeVersion", "1")
		meta.ValueEqual("metadataVersion", 1)
		meta.Value("createdAt").String().AsDateTime(time.RFC3339).
			InRange(time.Now().Add(-5*time.Second), time.Now().Add(5*time.Second))

		// Clean
		uninstaller, err := app.NewInstaller(testInstance, app.Copier(consts.WebappType, testInstance),
			&app.InstallerOptions{
				Operation:  app.Delete,
				Type:       consts.WebappType,
				Slug:       "drive",
				SourceURL:  "registry://drive",
				Registries: testInstance.Registries(),
			},
		)
		assert.NoError(t, err)

		_, err = uninstaller.RunSync()
		assert.NoError(t, err)
	})

	t.Run("CreatePermissionWithMetadata", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Install the app
		installer, err := app.NewInstaller(testInstance, app.Copier(consts.WebappType, testInstance), &app.InstallerOptions{
			Operation:  app.Install,
			Type:       consts.WebappType,
			SourceURL:  "registry://drive",
			Slug:       "drive",
			Registries: testInstance.Registries(),
		})
		assert.NoError(t, err)
		_, err = installer.RunSync()
		assert.NoError(t, err)

		tok, err := testInstance.MakeJWT(permission.TypeWebapp,
			"drive", "io.cozy.files", "", time.Now())
		assert.NoError(t, err)

		// Request to create a permission
		obj := e.POST("/permissions").
			WithHeader("Authorization", "Bearer "+tok).
			WithHeader("Content-Type", "application/json").
			WithHost(testInstance.Domain).
			WithBytes([]byte(`{
        "data": {
          "type":"io.cozy.permissions",
          "attributes":{
            "permissions":{
              "files":{
                "type":"io.cozy.files",
                "verbs":["GET"]
              }
            },
            "cozyMetadata":{"createdByApp":"foobar"}
          }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		// Assert a cozyMetadata has been added
		meta := obj.Path("$.data.attributes.cozyMetadata").Object()
		meta.ValueEqual("createdByApp", "foobar")
		meta.ValueEqual("doctypeVersion", "1")
		meta.ValueEqual("metadataVersion", 1)

		// Clean
		uninstaller, err := app.NewInstaller(testInstance, app.Copier(consts.WebappType, testInstance),
			&app.InstallerOptions{
				Operation:  app.Delete,
				Type:       consts.WebappType,
				Slug:       "drive",
				SourceURL:  "registry://drive",
				Registries: testInstance.Registries(),
			},
		)
		assert.NoError(t, err)

		_, err = uninstaller.RunSync()
		assert.NoError(t, err)
	})
}

func createTestSubPermissions(e *httpexpect.Expect, tok string, codes string) (string, *httpexpect.Object, error) {
	obj := e.POST("/permissions").
		WithQuery("codes", codes).
		WithHeader("Authorization", "Bearer "+tok).
		WithHeader("Content-Type", "application/json").
		WithBytes([]byte(`{
      "data": {
        "type": "io.cozy.permissions",
        "attributes": {
          "permissions": {
            "whatever": {
              "type":   "io.cozy.files",
              "verbs":  ["GET"],
              "values": ["io.cozy.music"]
            },
            "otherrule": {
              "type":   "io.cozy.files",
              "verbs":  ["GET"],
              "values":  ["some-other-dir"]
            }
          }
        }
      }
    }`)).
		Expect().Status(200).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object()

	data := obj.Value("data").Object()
	id := data.Value("id").String()
	result := obj.Path("$.data.attributes.codes").Object()

	return id.Raw(), result, nil
}

func createTestTinyCode(e *httpexpect.Expect, tok string, codes string, ttl string) (string, *httpexpect.Object, error) {
	obj := e.POST("/permissions").
		WithQuery("codes", codes).
		WithQuery("tiny", true).
		WithQuery("ttl", ttl).
		WithHeader("Authorization", "Bearer "+tok).
		WithHeader("Content-Type", "application/json").
		WithBytes([]byte(`{
      "data": {
        "type": "io.cozy.permissions",
        "attributes": {
          "permissions": {
            "whatever": {
              "type":   "io.cozy.files",
              "verbs":  ["GET"],
              "values": ["id.` + codes + `"]
            }
          }
        }
      }
    }`)).
		Expect().Status(200).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object()

	data := obj.Value("data").Object()
	id := data.Value("id").String()
	result := obj.Path("$.data.attributes.shortcodes").Object()

	return id.Raw(), result, nil
}

func createTestEvent(i *instance.Instance) (*couchdb.JSONDoc, error) {
	e := &couchdb.JSONDoc{
		Type: "io.cozy.events",
		M:    map[string]interface{}{"test": "value"},
	}
	err := couchdb.CreateDoc(i, e)
	return e, err
}
