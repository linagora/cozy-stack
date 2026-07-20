package sharings_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/assets/dynamic"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web"
	"github.com/cozy/cozy-stack/web/auth"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/cozy/cozy-stack/web/files"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/cozy/cozy-stack/web/permissions"
	"github.com/cozy/cozy-stack/web/sharings"
	"github.com/cozy/cozy-stack/web/statik"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const iocozytests = "io.cozy.tests"
const iocozytestswildcard = "io.cozy.tests.*"

// Things that live on Alice's Cozy
var charlieContact, daveContact, edwardContact *contact.Contact
var sharingID, state, aliceAccessToken string

// Bob's browser
var discoveryLink, authorizeLink string

func TestSharings(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	build.BuildMode = build.ModeDev
	config.GetConfig().Assets = "../../assets"
	_ = web.LoadSupportedLocales()
	testutils.NeedCouchdb(t)
	render, _ := statik.NewDirRenderer("../../assets")
	middlewares.BuildTemplates()

	// Prepare Alice's instance
	setup := testutils.NewSetup(t, t.Name()+"_alice")
	aliceInstance := setup.GetTestInstance(&lifecycle.Options{
		Email:      "alice@example.net",
		PublicName: "Alice",
	})
	aliceAppToken := generateAppToken(aliceInstance, "testapp", iocozytests)
	aliceAppTokenWildcard := generateAppToken(aliceInstance, "testapp2", iocozytestswildcard)
	charlieContact = createContact(t, aliceInstance, "Charlie", "charlie@example.net")
	daveContact = createContact(t, aliceInstance, "Dave", "dave@example.net")
	tsA := setup.GetTestServerMultipleRoutes(map[string]func(*echo.Group){
		"/sharings":    sharings.Routes,
		"/permissions": permissions.Routes,
	})
	tsA.Config.Handler.(*echo.Echo).Renderer = render
	tsA.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(tsA.Close)

	// Prepare Bob's instance
	bobSetup := testutils.NewSetup(t, t.Name()+"_bob")
	bobInstance := bobSetup.GetTestInstance(&lifecycle.Options{
		Email:         "bob@example.net",
		PublicName:    "Bob",
		Passphrase:    "MyPassphrase",
		KdfIterations: 5000,
		Key:           "xxx",
	})
	bobAppToken := generateAppToken(bobInstance, "testapp", iocozytests)
	tsB := bobSetup.GetTestServerMultipleRoutes(map[string]func(*echo.Group){
		"/auth": func(g *echo.Group) {
			g.Use(middlewares.LoadSession)
			auth.Routes(g)
		},
		"/sharings": sharings.Routes,
	})
	tsB.Config.Handler.(*echo.Echo).Renderer = render
	tsB.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(tsB.Close)

	require.NoError(t, dynamic.InitDynamicAssetFS(config.FsURL().String()), "Could not init dynamic FS")

	t.Run("CreateSharingSuccess", func(t *testing.T) {
		eA := httpexpect.Default(t, tsA.URL)

		bobContact := createBobContact(t, aliceInstance)
		assert.NotEmpty(t, aliceAppToken)
		assert.NotNil(t, bobContact)

		obj := eA.POST("/sharings/").
			WithHeader("Authorization", "Bearer "+aliceAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{
        "data": {
          "type": "` + consts.Sharings + `",
          "attributes": {
            "description":  "this is a test",
            "open_sharing": true,
            "rules": [{
                "title": "test one",
                "doctype": "` + iocozytests + `",
                "values": ["000000"],
                "add": "sync"
              }]
          },
          "relationships": {
            "recipients": {
              "data": [{"id": "` + bobContact.ID() + `", "type": "` + bobContact.DocType() + `"}]
            },
            "read_only_recipients": {
                "data": [{"id": "` + daveContact.ID() + `", "type": "` + daveContact.DocType() + `"}]
            }
          }
        }
      }`)).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		sharingID = obj.Value("data").Object().Value("id").String().NotEmpty().Raw()

		assertSharingIsCorrectOnSharer(t, obj, sharingID, aliceInstance)
		description := assertInvitationMailWasSent(t, aliceInstance, "Alice")
		assert.Equal(t, description, "this is a test")
		assert.Contains(t, discoveryLink, "/discovery?state=")
	})

	t.Run("GetSharing", func(t *testing.T) {
		eA := httpexpect.Default(t, tsA.URL)

		obj := eA.GET("/sharings/"+sharingID).
			WithHeader("Authorization", "Bearer "+aliceAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		assertSharingIsCorrectOnSharer(t, obj, sharingID, aliceInstance)
	})

	t.Run("Discovery", func(t *testing.T) {
		u, err := url.Parse(discoveryLink)
		assert.NoError(t, err)
		state = u.Query()["state"][0]

		// Take only the path and query from the `disoveryLink` and redirect
		// to the tsA host.
		eA := httpexpect.Default(t, tsA.URL)

		eA.GET(u.Path).
			WithQuery("state", state).
			Expect().Status(200).
			HasContentType("text/html", "utf-8").
			Body().
			Contains("Connect to your Twake").
			Contains(`<input type="hidden" name="state" value="` + state)

		redirectHeader := eA.POST(u.Path).
			WithFormField("state", state).
			WithFormField("slug", tsB.URL).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(302).Header("Location")

		authorizeLink = redirectHeader.NotEmpty().Raw()
		redirectHeader.Contains(tsB.URL)
		redirectHeader.Contains("/auth/authorize/sharing")

		assertSharingRequestHasBeenCreated(t, aliceInstance, bobInstance, tsB.URL)
	})

	t.Run("AuthorizeSharing", func(t *testing.T) {
		u, err := url.Parse(authorizeLink)
		assert.NoError(t, err)
		sharingID = u.Query()["sharing_id"][0]
		state := u.Query()["state"][0]

		eB := httpexpect.Default(t, tsB.URL)

		// Bob login
		token := eB.GET("/auth/login").
			Expect().Status(200).
			Cookie("_csrf").Value().NotEmpty().Raw()

		eB.POST("/auth/login").
			WithCookie("_csrf", token).
			WithFormField("passphrase", "MyPassphrase").
			WithFormField("csrf_token", token).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(303).
			Header("Location").Contains("home")
		// End bob login

		FakeOwnerInstance(t, bobInstance, tsA.URL)

		body := eB.GET(u.Path).
			WithQuery("sharing_id", sharingID).
			WithQuery("state", state).
			Expect().Status(200).
			HasContentType("text/html", "utf-8").
			Body()

		body.Contains("and you can collaborate by editing the document")
		body.Contains(`<input type="hidden" name="sharing_id" value="` + sharingID)
		body.Contains(`<input type="hidden" name="state" value="` + state)
		body.Contains(`<span class="filetype-other filetype">`)

		matches := body.Match(`<input type="hidden" name="csrf_token" value="(\w+)"`)
		matches.Length().IsEqual(2)

		eB.POST("/auth/authorize/sharing").
			WithFormField("state", state).
			WithFormField("sharing_id", sharingID).
			WithFormField("csrf_token", token).
			WithFormField("synchronize", true).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(303).
			Header("Location").Contains("testapp." + bobInstance.Domain)

		assertCredentialsHasBeenExchanged(t, aliceInstance, bobInstance, tsA.URL, tsB.URL)
	})

	t.Run("DelegateAddRecipientByCozyURL", func(t *testing.T) {
		assert.NotEmpty(t, bobAppToken)
		edwardContact = createContact(t, bobInstance, "Edward", "edward@example.net")

		eB := httpexpect.Default(t, tsB.URL)

		obj := eB.POST("/sharings/"+sharingID+"/recipients").
			WithHeader("Authorization", "Bearer "+bobAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{
        "data": {
          "type": "` + consts.Sharings + `",
          "relationships": {
            "recipients": {
              "data": [{"id": "` + edwardContact.ID() + `", "type": "` + edwardContact.DocType() + `"}]
            }
          }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		attrs := data.Value("attributes").Object()

		members := attrs.Value("members").Array()
		members.Length().IsEqual(4)

		owner := members.Value(0).Object()
		owner.HasValue("status", "owner")
		owner.HasValue("public_name", "Alice")
		owner.HasValue("email", "alice@example.net")

		bob := members.Value(1).Object()
		bob.HasValue("status", "ready")
		bob.HasValue("email", "bob@example.net")

		dave := members.Value(2).Object()
		dave.HasValue("status", "pending")
		dave.HasValue("email", "dave@example.net")
		dave.HasValue("read_only", true)

		edward := members.Value(3).Object()
		edward.HasValue("name", "Edward")
		edward.HasValue("email", "edward@example.net")
	})

	t.Run("CreateSharingWithGroup", func(t *testing.T) {
		eA := httpexpect.Default(t, tsA.URL)
		require.NotEmpty(t, aliceAppToken)

		group := createGroup(t, aliceInstance, "friends")
		require.NotNil(t, group)
		group.M["color"] = "#22AA55"
		require.NoError(t, couchdb.UpdateDoc(aliceInstance, group))
		fionaContact := addContactToGroup(t, aliceInstance, group, "Fiona")
		require.NotNil(t, fionaContact)
		gabyContact := addContactToGroup(t, aliceInstance, group, "Gaby")
		require.NotNil(t, gabyContact)

		obj := eA.POST("/sharings/").
			WithHeader("Authorization", "Bearer "+aliceAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{
        "data": {
          "type": "` + consts.Sharings + `",
          "attributes": {
            "description":  "this is a test with a group",
            "open_sharing": true,
            "rules": [{
                "title": "test group",
                "doctype": "` + iocozytests + `",
                "values": ["000001"],
                "add": "sync"
              }]
          },
          "relationships": {
            "recipients": {
              "data": [{"id": "` + group.ID() + `", "type": "` + consts.Groups + `"}]
            }
          }
        }
      }`)).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		attrs := data.Value("attributes").Object()
		attrs.HasValue("description", "this is a test with a group")
		members := attrs.Value("members").Array()
		members.Length().IsEqual(3)

		owner := members.Value(0).Object()
		owner.HasValue("status", "owner")
		owner.HasValue("public_name", "Alice")
		owner.HasValue("email", "alice@example.net")
		owner.HasValue("instance", "http://"+aliceInstance.Domain)

		recipient := members.Value(1).Object()
		recipient.HasValue("status", "pending")
		recipient.HasValue("name", "Fiona")
		recipient.HasValue("email", "fiona@example.net")
		recipient.HasValue("groups", []int{0})
		recipient.NotContainsKey("read_only")
		recipient.HasValue("only_in_groups", true)

		recipient = members.Value(2).Object()
		recipient.HasValue("status", "pending")
		recipient.HasValue("name", "Gaby")
		recipient.HasValue("email", "gaby@example.net")
		recipient.HasValue("groups", []int{0})
		recipient.NotContainsKey("read_only")
		recipient.HasValue("only_in_groups", true)

		groups := attrs.Value("groups").Array()
		groups.Length().IsEqual(1)
		g := groups.Value(0).Object()
		g.HasValue("id", group.ID())
		g.HasValue("name", "friends")
		g.HasValue("color", "#22AA55")
		g.HasValue("addedBy", 0)
	})

	t.Run("CreateSharingWithPreview", func(t *testing.T) {
		bobContact := createBobContact(t, aliceInstance)
		require.NotEmpty(t, aliceAppToken)
		require.NotNil(t, bobContact)

		eA := httpexpect.Default(t, tsA.URL)

		obj := eA.POST("/sharings/").
			WithHeader("Authorization", "Bearer "+aliceAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{
        "data": {
          "type": "` + consts.Sharings + `",
          "attributes": {
            "description":  "this is a test with preview",
            "preview_path": "/preview",
            "rules": [{
                "title": "test two",
                "doctype": "` + iocozytests + `",
                "values": ["foobaz"]
              }]
          },
          "relationships": {
            "recipients": {
              "data": [{"id": "` + bobContact.ID() + `", "type": "` + bobContact.DocType() + `"}]
            },
            "read_only_recipients": {
                "data": [{"id": "` + daveContact.ID() + `", "type": "` + daveContact.DocType() + `"}]
            }
          }
        }
      }`)).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.HasValue("type", consts.Sharings)
		sharingID = data.Value("id").String().NotEmpty().Raw()
		data.Value("meta").Object().Value("rev").String().NotEmpty()
		data.Value("links").Object().HasValue("self", "/sharings/"+sharingID)

		attrs := data.Value("attributes").Object()
		attrs.HasValue("description", "this is a test with preview")
		attrs.HasValue("app_slug", "testapp")
		attrs.HasValue("preview_path", "/preview")
		attrs.HasValue("owner", true)
		attrs.Value("created_at").String().AsDateTime(time.RFC3339)
		attrs.Value("updated_at").String().AsDateTime(time.RFC3339)
		attrs.NotContainsKey("credentials")

		members := attrs.Value("members").Array()
		assertSharingByAliceToBobAndDave(t, members, aliceInstance)

		rules := attrs.Value("rules").Array()
		rules.Length().IsEqual(1)
		rule := rules.Value(0).Object()
		rule.HasValue("title", "test two")
		rule.HasValue("doctype", iocozytests)
		rule.HasValue("values", []string{"foobaz"})

		description := assertInvitationMailWasSent(t, aliceInstance, "Alice")
		assert.Equal(t, description, "this is a test with preview")
		assert.Contains(t, discoveryLink, aliceInstance.Domain)
		assert.Contains(t, discoveryLink, "/preview?sharecode=")
	})

	t.Run("DiscoveryWithPreviewOIDCLinkUsesMemberState", func(t *testing.T) {
		u, err := url.Parse(discoveryLink)
		require.NoError(t, err)
		sharecode := u.Query().Get("sharecode")
		require.NotEmpty(t, sharecode)

		s, err := sharing.FindSharing(aliceInstance, sharingID)
		require.NoError(t, err)
		member, err := s.FindMemberBySharecode(aliceInstance, sharecode)
		require.NoError(t, err)
		credentials := s.FindCredentials(member)
		require.NotNil(t, credentials)
		require.NotEmpty(t, credentials.State)

		conf := config.GetConfig()
		if conf.Authentication == nil {
			conf.Authentication = make(map[string]interface{})
		}
		oidcContext := "preview-oidc-test"
		originalContext := aliceInstance.ContextName
		previousAuth, hadPreviousAuth := conf.Authentication[oidcContext]
		conf.Authentication[oidcContext] = map[string]interface{}{
			"oidc": map[string]interface{}{
				"client_id": "preview-oidc-client",
			},
		}
		aliceInstance.ContextName = oidcContext
		require.NoError(t, instance.Update(aliceInstance))
		t.Cleanup(func() {
			if hadPreviousAuth {
				conf.Authentication[oidcContext] = previousAuth
			} else {
				delete(conf.Authentication, oidcContext)
			}
			aliceInstance.ContextName = originalContext
			if err := instance.Update(aliceInstance); err != nil {
				t.Errorf("cannot restore Alice context: %s", err)
			}
		})

		eA := httpexpect.Default(t, tsA.URL)
		body := eA.GET("/sharings/"+sharingID+"/discovery").
			WithQuery("sharecode", sharecode).
			Expect().Status(200).
			Body().Raw()

		assert.Contains(t, body, "/oidc/sharing?")
		assert.Contains(t, body, "state="+credentials.State)
		assert.NotContains(t, body, "state=&")
	})

	t.Run("DiscoveryWithPreview", func(t *testing.T) {
		u, err := url.Parse(discoveryLink)
		assert.NoError(t, err)
		sharecode := u.Query()["sharecode"][0]

		eA := httpexpect.Default(t, tsA.URL)

		obj := eA.POST("/sharings/"+sharingID+"/discovery").
			WithHeader("Accept", "application/json").
			WithFormField("sharecode", sharecode).
			WithFormField("url", tsB.URL).
			Expect().Status(200).
			JSON().Object()

		redirectURI := obj.Value("redirect").String().Contains(tsB.URL).Raw()

		res, err := url.Parse(redirectURI)
		assert.NoError(t, err)
		assert.Equal(t, res.Path, "/auth/authorize/sharing")
		assert.Equal(t, res.Query()["sharing_id"][0], sharingID)
		assert.NotEmpty(t, res.Query()["state"][0])
	})

	t.Run("AddRecipient", func(t *testing.T) {
		require.NotEmpty(t, aliceAppToken)
		require.NotNil(t, charlieContact)

		eA := httpexpect.Default(t, tsA.URL)

		brothers := createGroup(t, aliceInstance, "brothers")
		require.NotNil(t, brothers)
		hugoContact := addContactToGroup(t, aliceInstance, brothers, "Hugo")
		require.NotNil(t, hugoContact)
		idrisContact := addContactToGroup(t, aliceInstance, brothers, "Idris")
		require.NotNil(t, idrisContact)

		obj := eA.POST("/sharings/"+sharingID+"/recipients").
			WithHeader("Authorization", "Bearer "+aliceAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{
        "data": {
          "type": "` + consts.Sharings + `",
          "relationships": {
            "recipients": {
              "data": [
			    {"id": "` + charlieContact.ID() + `", "type": "` + consts.Contacts + `"},
			    {"id": "` + brothers.ID() + `", "type": "` + consts.Groups + `"}
		      ]
            }
          }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		attrs := data.Value("attributes").Object()
		members := attrs.Value("members").Array()

		members.Length().IsEqual(6)
		owner := members.Value(0).Object()
		owner.HasValue("status", "owner")
		owner.HasValue("public_name", "Alice")
		owner.HasValue("email", "alice@example.net")
		owner.HasValue("instance", "http://"+aliceInstance.Domain)

		bob := members.Value(1).Object()
		bob.HasValue("status", "pending")
		bob.HasValue("name", "Bob")
		bob.HasValue("email", "bob@example.net")
		bob.NotContainsKey("only_in_groups")

		dave := members.Value(2).Object()
		dave.HasValue("status", "pending")
		dave.HasValue("name", "Dave")
		dave.HasValue("email", "dave@example.net")
		dave.HasValue("read_only", true)
		dave.NotContainsKey("only_in_groups")

		charlie := members.Value(3).Object()
		charlie.HasValue("status", "pending")
		charlie.HasValue("name", "Charlie")
		charlie.HasValue("email", "charlie@example.net")
		charlie.NotContainsKey("only_in_groups")

		hugo := members.Value(4).Object()
		hugo.HasValue("status", "pending")
		hugo.HasValue("name", "Hugo")
		hugo.HasValue("email", "hugo@example.net")
		hugo.HasValue("groups", []int{0})
		hugo.HasValue("only_in_groups", true)

		idris := members.Value(5).Object()
		idris.HasValue("status", "pending")
		idris.HasValue("name", "Idris")
		idris.HasValue("email", "idris@example.net")
		idris.HasValue("groups", []int{0})
		idris.HasValue("only_in_groups", true)

		groups := attrs.Value("groups").Array()
		groups.Length().IsEqual(1)
		g := groups.Value(0).Object()
		g.HasValue("id", brothers.ID())
		g.HasValue("name", "brothers")
		g.HasValue("addedBy", 0)
	})

	t.Run("RevokedSharingWithPreview", func(t *testing.T) {
		sharecode := strings.Split(discoveryLink, "=")[1]

		eA := httpexpect.Default(t, tsA.URL)

		obj := eA.GET("/permissions/self").
			WithHeader("Authorization", "Bearer "+sharecode).
			WithHeader("Content-Type", "application/vnd.api+json").
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		sourceID := obj.Value("data").Object().
			Value("attributes").Object().
			Value("source_id").String().NotEmpty().Raw()
		sharingID = strings.Split(sourceID, "/")[1]

		// Adding a new member to the sharing
		newMemberMail := "foo@bar.com"
		sharingDoc, err := sharing.FindSharing(aliceInstance, sharingID)
		require.NoError(t, err)

		m := sharing.Member{Email: newMemberMail, ReadOnly: true}
		_, err = sharingDoc.AddDelegatedContact(aliceInstance, m)
		require.NoError(t, err)
		perms, err := permission.GetForSharePreview(aliceInstance, sharingID)
		require.NoError(t, err)
		fooShareCode, err := aliceInstance.CreateShareCode(newMemberMail)
		require.NoError(t, err)

		// Adding its sharecode
		codes := perms.Codes
		codes[newMemberMail] = fooShareCode
		perms.PatchCodes(codes)
		assert.NoError(t, couchdb.UpdateDoc(aliceInstance, perms))
		assert.NoError(t, couchdb.UpdateDoc(aliceInstance, sharingDoc))

		// Assert he has access to the sharing preview
		eA.GET("/permissions/self").
			WithHeader("Authorization", "Bearer "+fooShareCode).
			WithHeader("Content-Type", "application/vnd.api+json").
			Expect().Status(200)

		// Check the member status has been updated to "seen"
		sharingDoc, err = sharing.FindSharing(aliceInstance, sharingID)
		assert.NoError(t, err)
		member, err := sharingDoc.FindMemberBySharecode(aliceInstance, fooShareCode)
		assert.NoError(t, err)
		assert.Equal(t, "seen", member.Status)

		// Now, revoking the fresh user from the sharing
		member, err = sharingDoc.FindMemberBySharecode(aliceInstance, fooShareCode)
		assert.NoError(t, err)
		index := 0
		for i := range sharingDoc.Members {
			if member == &sharingDoc.Members[i] {
				index = i
				break
			}
		}
		err = sharingDoc.RevokeMember(aliceInstance, index)
		assert.NoError(t, err)
		assert.Equal(t, "revoked", member.Status)

		// Try to get permissions/self, we should get a 400
		eA.GET("/permissions/self").
			WithHeader("Authorization", "Bearer "+fooShareCode).
			WithHeader("Content-Type", "application/vnd.api+json").
			Expect().Status(400).
			Body().Contains("Invalid JWT")
	})

	t.Run("CheckPermissions", func(t *testing.T) {
		bobContact := createBobContact(t, aliceInstance)
		assert.NotNil(t, bobContact)

		eA := httpexpect.Default(t, tsA.URL)

		eA.POST("/sharings").
			WithHeader("Authorization", "Bearer "+aliceAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{
        "data": {
          "type": "` + consts.Sharings + `",
          "attributes": {
            "description":  "this is a test",
            "preview_path": "/preview",
            "rules": [
              {
                "title": "test one",
                "doctype": "` + iocozytests + `",
                "values": ["000000"],
                "add": "sync"
              },{
                "title": "test two",
                "doctype": "` + consts.Contacts + `",
                "values": ["000000"],
                "add": "sync"
              }]
          },
          "relationships": {
            "recipients": {
              "data": [{"id": "` + bobContact.ID() + `", "type": "` + bobContact.DocType() + `"}]
            }
          }
        }
      }`)).
			Expect().Status(403)

		other := &sharing.Sharing{
			Description: "Another sharing",
			Rules: []sharing.Rule{
				{
					Title:   "a directory",
					DocType: consts.Files,
					Values:  []string{"6836cc06-33e9-11e8-8157-dfc1aca099b6"},
				},
			},
		}
		assert.NoError(t, other.BeOwner(aliceInstance, "home"))
		assert.NoError(t, other.AddContact(aliceInstance, bobContact.ID(), false))
		_, err := other.Create(aliceInstance)
		assert.NoError(t, err)

		eA.GET("/sharings/"+other.ID()).
			WithHeader("Authorization", "Bearer "+aliceAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			Expect().Status(403)
	})

	t.Run("CheckSharingInfoByDocType", func(t *testing.T) {
		sharedDocs1 := []string{"fakeid1", "fakeid2", "fakeid3"}
		sharedDocs2 := []string{"fakeid4", "fakeid5"}
		s1 := createSharing(t, aliceInstance, sharedDocs1, tsB.URL)
		s2 := createSharing(t, aliceInstance, sharedDocs2, tsB.URL)

		for _, id := range sharedDocs1 {
			sid := iocozytests + "/" + id
			sd, errs := createSharedDoc(aliceInstance, sid, s1.ID())
			assert.NoError(t, errs)
			assert.NotNil(t, sd)
		}
		for _, id := range sharedDocs2 {
			sid := iocozytests + "/" + id
			sd, errs := createSharedDoc(aliceInstance, sid, s2.ID())
			assert.NoError(t, errs)
			assert.NotNil(t, sd)
		}

		eA := httpexpect.Default(t, tsA.URL)

		obj := eA.GET("/sharings/doctype/"+iocozytests).
			WithHeader("Authorization", "Bearer "+aliceAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		s1Found := false
		s2Found := false

		for _, itm := range obj.Value("data").Array().Iter() {
			data := itm.Object()
			data.HasValue("type", consts.Sharings)
			sharingID = data.Value("id").String().NotEmpty().Raw()
			rel := data.Value("relationships").Object()
			sharedDocs := rel.Value("shared_docs").Object()

			if sharingID == s1.ID() {
				sharedDocsData := sharedDocs.Value("data").Array()
				sharedDocsData.Value(0).Object().Value("id").IsEqual("fakeid1")
				sharedDocsData.Value(1).Object().Value("id").IsEqual("fakeid2")
				sharedDocsData.Value(2).Object().Value("id").IsEqual("fakeid3")
				s1Found = true
			}

			if sharingID == s2.ID() {
				sharedDocsData := sharedDocs.Value("data").Array()
				sharedDocsData.Value(0).Object().Value("id").IsEqual("fakeid4")
				sharedDocsData.Value(1).Object().Value("id").IsEqual("fakeid5")
				s2Found = true
			}
		}

		assert.Equal(t, true, s1Found)
		assert.Equal(t, true, s2Found)

		eA.GET("/sharings/doctype/io.cozy.tests.notyet").
			WithHeader("Authorization", "Bearer "+aliceAppTokenWildcard).
			WithHeader("Content-Type", "application/vnd.api+json").
			Expect().Status(200)

		eA.GET("/sharings/doctype/"+iocozytests).
			WithHeader("Authorization", "Bearer "+aliceAppTokenWildcard).
			WithHeader("Content-Type", "application/vnd.api+json").
			Expect().Status(200)

		eA.GET("/sharings/doctype/io.cozy.things").
			WithHeader("Content-Type", "application/vnd.api+json").
			Expect().Status(401)
	})

	t.Run("RevokeSharing", func(t *testing.T) {
		sharedDocs := []string{"mygreatid1", "mygreatid2"}
		sharedRefs := []*sharing.SharedRef{}
		s := createSharing(t, aliceInstance, sharedDocs, tsB.URL)

		for _, id := range sharedDocs {
			sid := iocozytests + "/" + id
			sd, errs := createSharedDoc(aliceInstance, sid, s.SID)
			sharedRefs = append(sharedRefs, sd)
			assert.NoError(t, errs)
			assert.NotNil(t, sd)
		}

		cli, err := sharing.CreateOAuthClient(aliceInstance, &s.Members[1])
		assert.NoError(t, err)
		s.Credentials[0].Client = sharing.ConvertOAuthClient(cli)
		token, err := sharing.CreateAccessToken(aliceInstance, cli, s.SID, permission.ALL)
		assert.NoError(t, err)
		s.Credentials[0].AccessToken = token
		s.Members[1].Status = sharing.MemberStatusReady

		err = couchdb.UpdateDoc(aliceInstance, s)
		assert.NoError(t, err)

		err = s.AddTrackTriggers(aliceInstance)
		assert.NoError(t, err)
		err = s.AddReplicateTrigger(aliceInstance)
		assert.NoError(t, err)

		eA := httpexpect.Default(t, tsA.URL)

		eA.DELETE("/sharings/"+s.ID()+"/recipients").
			WithHeader("Authorization", "Bearer "+aliceAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			Expect().Status(204)

		var sRevoke sharing.Sharing
		err = couchdb.GetDoc(aliceInstance, s.DocType(), s.SID, &sRevoke)
		assert.NoError(t, err)

		assert.Equal(t, "", sRevoke.Triggers.TrackID)
		assert.Empty(t, sRevoke.Triggers.TrackIDs)
		assert.Equal(t, "", sRevoke.Triggers.ReplicateID)
		assert.Equal(t, "", sRevoke.Triggers.UploadID)
		assert.Equal(t, false, sRevoke.Active)

		var sdoc sharing.SharedRef
		err = couchdb.GetDoc(aliceInstance, sharedRefs[0].DocType(), sharedRefs[0].ID(), &sdoc)
		assert.EqualError(t, err, "CouchDB(not_found): deleted")
		err = couchdb.GetDoc(aliceInstance, sharedRefs[1].DocType(), sharedRefs[1].ID(), &sdoc)
		assert.EqualError(t, err, "CouchDB(not_found): deleted")
	})

	t.Run("RevokeRecipient", func(t *testing.T) {
		sharedDocs := []string{"mygreatid3", "mygreatid4"}
		sharedRefs := []*sharing.SharedRef{}
		s := createSharing(t, aliceInstance, sharedDocs, tsB.URL)

		for _, id := range sharedDocs {
			sid := iocozytests + "/" + id
			sd, errs := createSharedDoc(aliceInstance, sid, s.SID)
			sharedRefs = append(sharedRefs, sd)
			assert.NoError(t, errs)
			assert.NotNil(t, sd)
		}

		cli, err := sharing.CreateOAuthClient(aliceInstance, &s.Members[1])
		assert.NoError(t, err)
		s.Credentials[0].Client = sharing.ConvertOAuthClient(cli)
		token, err := sharing.CreateAccessToken(aliceInstance, cli, s.SID, permission.ALL)
		assert.NoError(t, err)
		s.Credentials[0].AccessToken = token
		s.Members[1].Status = sharing.MemberStatusReady

		s.Members = append(s.Members, sharing.Member{
			Status:   sharing.MemberStatusReady,
			Name:     "Charlie",
			Email:    "charlie@cozy.local",
			Instance: tsB.URL,
		})

		clientC, err := sharing.CreateOAuthClient(aliceInstance, &s.Members[2])
		assert.NoError(t, err)
		tokenC, err := sharing.CreateAccessToken(aliceInstance, clientC, s.SID, permission.ALL)
		assert.NoError(t, err)
		s.Credentials = append(s.Credentials, sharing.Credentials{
			Client:      sharing.ConvertOAuthClient(clientC),
			AccessToken: tokenC,
		})

		err = couchdb.UpdateDoc(aliceInstance, s)
		assert.NoError(t, err)

		err = s.AddTrackTriggers(aliceInstance)
		assert.NoError(t, err)
		err = s.AddReplicateTrigger(aliceInstance)
		assert.NoError(t, err)

		eA := httpexpect.Default(t, tsA.URL)

		eA.DELETE("/sharings/"+s.ID()+"/recipients/1").
			WithHeader("Authorization", "Bearer "+aliceAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			Expect().Status(204)

		assertOneRecipientIsRevoked(t, s, aliceInstance)

		eA.DELETE("/sharings/"+s.ID()+"/recipients/2").
			WithHeader("Authorization", "Bearer "+aliceAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			Expect().Status(204)

		assertLastRecipientIsRevoked(t, s, sharedRefs, aliceInstance)
	})

	t.Run("RevokeGroup", func(t *testing.T) {
		sharedDocs := []string{"forgroup1"}
		s := createSharing(t, aliceInstance, sharedDocs, tsB.URL)

		group := createGroup(t, aliceInstance, "friends")
		require.NotNil(t, group)
		fionaContact := addContactToGroup(t, aliceInstance, group, "Fiona")
		require.NotNil(t, fionaContact)
		gabyContact := addContactToGroup(t, aliceInstance, group, "Gaby")
		require.NotNil(t, gabyContact)

		eA := httpexpect.Default(t, tsA.URL)

		obj := eA.POST("/sharings/"+s.ID()+"/recipients").
			WithHeader("Authorization", "Bearer "+aliceAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{
        "data": {
          "type": "` + consts.Sharings + `",
          "relationships": {
            "recipients": {
              "data": [
			    {"id": "` + group.ID() + `", "type": "` + consts.Groups + `"}
		      ]
            }
          }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		attrs := data.Value("attributes").Object()
		members := attrs.Value("members").Array()
		members.Length().IsEqual(4)

		owner := members.Value(0).Object()
		owner.HasValue("status", "owner")
		owner.HasValue("public_name", "Alice")

		bob := members.Value(1).Object()
		bob.HasValue("name", "Bob")

		fiona := members.Value(2).Object()
		fiona.HasValue("status", "pending")
		fiona.HasValue("name", "Fiona")
		fiona.HasValue("groups", []int{0})
		fiona.HasValue("only_in_groups", true)

		gaby := members.Value(3).Object()
		gaby.HasValue("status", "pending")
		gaby.HasValue("name", "Gaby")
		gaby.HasValue("groups", []int{0})
		gaby.HasValue("only_in_groups", true)

		eA.DELETE("/sharings/"+s.ID()+"/groups/0").
			WithHeader("Authorization", "Bearer "+aliceAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			Expect().Status(204)

		obj = eA.GET("/sharings/"+s.ID()).
			WithHeader("Authorization", "Bearer "+aliceAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data = obj.Value("data").Object()
		attrs = data.Value("attributes").Object()
		members = attrs.Value("members").Array()
		members.Length().IsEqual(4)

		owner = members.Value(0).Object()
		owner.HasValue("status", "owner")
		owner.HasValue("public_name", "Alice")

		bob = members.Value(1).Object()
		bob.HasValue("name", "Bob")

		fiona = members.Value(2).Object()
		fiona.HasValue("status", "revoked")
		fiona.HasValue("name", "Fiona")

		gaby = members.Value(3).Object()
		gaby.HasValue("status", "revoked")
		gaby.HasValue("name", "Gaby")
	})

	t.Run("RevocationFromRecipient", func(t *testing.T) {
		sharedDocs := []string{"mygreatid5", "mygreatid6"}
		sharedRefs := []*sharing.SharedRef{}
		s := createSharing(t, aliceInstance, sharedDocs, tsB.URL)
		for _, id := range sharedDocs {
			sid := iocozytests + "/" + id
			sd, errs := createSharedDoc(aliceInstance, sid, s.SID)
			sharedRefs = append(sharedRefs, sd)
			assert.NoError(t, errs)
			assert.NotNil(t, sd)
		}

		cli, err := sharing.CreateOAuthClient(aliceInstance, &s.Members[1])
		assert.NoError(t, err)
		s.Credentials[0].InboundClientID = cli.ClientID
		s.Credentials[0].Client = sharing.ConvertOAuthClient(cli)
		token, err := sharing.CreateAccessToken(aliceInstance, cli, s.SID, permission.ALL)
		assert.NoError(t, err)
		s.Credentials[0].AccessToken = token
		s.Members[1].Status = sharing.MemberStatusReady

		s.Members = append(s.Members, sharing.Member{
			Status:   sharing.MemberStatusReady,
			Name:     "Charlie",
			Email:    "charlie@cozy.local",
			Instance: tsB.URL,
		})
		clientC, err := sharing.CreateOAuthClient(aliceInstance, &s.Members[2])
		assert.NoError(t, err)
		tokenC, err := sharing.CreateAccessToken(aliceInstance, clientC, s.SID, permission.ALL)
		assert.NoError(t, err)
		s.Credentials = append(s.Credentials, sharing.Credentials{
			Client:          sharing.ConvertOAuthClient(clientC),
			AccessToken:     tokenC,
			InboundClientID: clientC.ClientID,
		})

		err = couchdb.UpdateDoc(aliceInstance, s)
		assert.NoError(t, err)

		err = s.AddTrackTriggers(aliceInstance)
		assert.NoError(t, err)
		err = s.AddReplicateTrigger(aliceInstance)
		assert.NoError(t, err)

		eA := httpexpect.Default(t, tsA.URL)

		eA.DELETE("/sharings/"+s.ID()+"/answer").
			WithHeader("Authorization", "Bearer "+s.Credentials[0].AccessToken.AccessToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			Expect().Status(204)

		assertOneRecipientIsRevoked(t, s, aliceInstance)

		eA.DELETE("/sharings/"+s.ID()+"/answer").
			WithHeader("Authorization", "Bearer "+s.Credentials[1].AccessToken.AccessToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			Expect().Status(204)

		assertLastRecipientIsRevoked(t, s, sharedRefs, aliceInstance)
	})

	t.Run("ClearAppInURL", func(t *testing.T) {
		host := sharings.ClearAppInURL("https://example.mycozy.cloud/")
		assert.Equal(t, "https://example.mycozy.cloud/", host)
		host = sharings.ClearAppInURL("https://example-drive.mycozy.cloud/")
		assert.Equal(t, "https://example.mycozy.cloud/", host)
		host = sharings.ClearAppInURL("https://my-cozy.example.net/")
		assert.Equal(t, "https://my-cozy.example.net/", host)
	})

	t.Run("PatchSharing", func(t *testing.T) {
		eA := httpexpect.Default(t, tsA.URL)

		var before sharing.Sharing
		require.NoError(t, couchdb.GetDoc(aliceInstance, consts.Sharings, sharingID, &before))
		require.NotEmpty(t, before.Rules)
		originalRuleTitle := before.Rules[0].Title

		obj := eA.PATCH("/sharings/"+sharingID).
			WithHeader("Authorization", "Bearer "+aliceAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{
            "data": {
              "type": "` + consts.Sharings + `",
              "attributes": {
                "description":  "this is an updated description"
              }
            }
          }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		updated := obj.Value("data").Object().Value("attributes").Object().Value("description").String().NotEmpty().Raw()
		assert.Equal(t, updated, "this is an updated description")

		var s sharing.Sharing
		require.NoError(t, couchdb.GetDoc(aliceInstance, consts.Sharings, sharingID, &s))
		require.NotEmpty(t, s.Rules)
		assert.Equal(t, originalRuleTitle, s.Rules[0].Title)
	})

	t.Run("DiscoveryTemplateRendering", func(t *testing.T) {
		// Create a new sharing with preview to test the discovery page
		bobContact := createBobContact(t, aliceInstance)
		require.NotEmpty(t, aliceAppToken)
		require.NotNil(t, bobContact)

		eA := httpexpect.Default(t, tsA.URL)

		// Create sharing
		obj := eA.POST("/sharings/").
			WithHeader("Authorization", "Bearer "+aliceAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{
        "data": {
          "type": "` + consts.Sharings + `",
          "attributes": {
            "description":  "Test Discovery Template",
            "preview_path": "/preview",
            "rules": [{
                "title": "discovery test",
                "doctype": "` + iocozytests + `",
                "values": ["discovery123"]
              }]
          },
          "relationships": {
            "recipients": {
              "data": [{"id": "` + bobContact.ID() + `", "type": "` + bobContact.DocType() + `"}]
            }
          }
        }
      }`)).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		testSharingID := obj.Value("data").Object().Value("id").String().NotEmpty().Raw()

		// Get the sharing document to extract the state
		var testSharing sharing.Sharing
		err := couchdb.GetDoc(aliceInstance, consts.Sharings, testSharingID, &testSharing)
		assert.NoError(t, err)

		// Get the state from the credentials
		assert.Len(t, testSharing.Credentials, 1)
		testState := testSharing.Credentials[0].State

		// Test 1: Verify all template parameters are rendered correctly
		body := eA.GET("/sharings/"+testSharingID+"/discovery").
			WithQuery("state", testState).
			Expect().Status(200).
			HasContentType("text/html", "utf-8").
			Body()

		// Verify basic structure
		body.Contains("<!DOCTYPE html>")
		body.Contains(`lang="en"`) // Default locale

		// Verify title and meta tags
		body.Contains("<title>")
		body.Contains(`<meta charset="utf-8">`)
		body.Contains(`<meta name="viewport" content="width=device-width, initial-scale=1">`)
		body.Contains(`<meta name="theme-color" content="#fff">`)

		// Verify CSS links
		body.Contains("/fonts/fonts.css")
		body.Contains("/css/cozy-bs.min.css")
		body.Contains("/styles/theme.css")
		body.Contains("/styles/cirrus.css")

		// Verify form action and method
		body.Contains(`<form method="POST" action="/sharings/` + testSharingID + `/discovery"`)

		// Verify hidden inputs with correct values
		body.Contains(`<input type="hidden" name="state" value="` + testState + `"`)
		body.Contains(`<input type="hidden" name="sharecode" value=""`)
		body.Contains(`<input type="hidden" name="shortcut" value=""`)

		// Verify heading and public name
		body.Contains("<h1")
		body.Contains("Connect to")
		body.Contains("Alice") // PublicName in the intro text

		// Verify input fields structure
		body.Contains(`<input type="text"`)
		body.Contains(`name="slug"`)
		body.Contains(`placeholder="claude"`)
		body.Contains(`inputmode="url"`)
		body.Contains(`class="form-control`)

		// Verify domain selector
		body.Contains(`<select name="domain"`)
		body.Contains(`class="form-select`)
		body.Contains("mycozy.cloud") // Default domain
		body.Contains("Autre domaine")

		// Verify submit button
		body.Contains(`<button id="login-submit"`)
		body.Contains(`type="submit"`)
		body.Contains(`class="btn btn-primary`)

		// Verify footer links
		body.Contains("https://manager.cozycloud.cc/v2/cozy/remind")
		body.Contains("Discover Twake now!") // Actual link text

		// Verify scripts
		body.Contains("/scripts/cirrus.js")

		// Test 2: Verify error states - URL Error
		bodyWithURLError := eA.POST("/sharings/"+testSharingID+"/discovery").
			WithFormField("state", testState).
			WithFormField("slug", "invalid url format").
			Expect().Status(400).
			HasContentType("text/html", "utf-8").
			Body()

		// Verify error styling is applied
		bodyWithURLError.Contains(`class="form-control form-control-md-lg is-invalid"`)
		bodyWithURLError.Contains(`<div class="invalid-tooltip`)
		bodyWithURLError.Contains("The Twake URL you entered is incorrect") // Actual error message

		// Test 3: Verify error states - Not Email Error (when email is provided instead of URL)
		bodyWithEmailError := eA.POST("/sharings/"+testSharingID+"/discovery").
			WithFormField("state", testState).
			WithFormField("slug", "test@example.com").
			Expect().Status(412). // Precondition Failed
			HasContentType("text/html", "utf-8").
			Body()

		// Verify email error is displayed
		bodyWithEmailError.Contains(`class="form-control form-control-md-lg is-invalid"`)
		bodyWithEmailError.Contains(`<div class="invalid-tooltip`) // Error tooltip is shown

		// Test 4: Verify shortcut parameter
		bodyWithShortcut := eA.GET("/sharings/"+testSharingID+"/discovery").
			WithQuery("state", testState).
			WithQuery("shortcut", "true").
			Expect().Status(200).
			HasContentType("text/html", "utf-8").
			Body()

		bodyWithShortcut.Contains(`<input type="hidden" name="shortcut" value="true"`)
		// Note: Button text changes based on shortcut value in template logic
		// We verify the shortcut value is correctly passed

		// Test 5: Verify recipient slug and domain are pre-filled
		bodyPrefilled := eA.GET("/sharings/"+testSharingID+"/discovery").
			WithQuery("state", testState).
			Expect().Status(200).
			Body()

		// Check that the recipient slug input has a value attribute
		bodyPrefilled.Contains(`value=""`) // Empty since member instance is not set initially
	})

	t.Run("DiscoveryTemplateWithOIDCButtons", func(t *testing.T) {
		conf := config.GetConfig()
		if conf.Authentication == nil {
			conf.Authentication = make(map[string]interface{})
		}
		if conf.Contexts == nil {
			conf.Contexts = make(map[string]interface{})
		}

		// Helper to set public_oidc_context in a context
		setPublicOIDCContext := func(contextName, publicOIDCContext string) {
			ctxData, ok := conf.Contexts[contextName].(map[string]interface{})
			if !ok {
				ctxData = make(map[string]interface{})
			}
			ctxData["public_oidc_context"] = publicOIDCContext
			conf.Contexts[contextName] = ctxData
		}

		// Helper to remove public_oidc_context from a context
		clearPublicOIDCContext := func(contextName string) {
			if ctxData, ok := conf.Contexts[contextName].(map[string]interface{}); ok {
				delete(ctxData, "public_oidc_context")
			}
		}

		eA := httpexpect.Default(t, tsA.URL)
		originalContext := aliceInstance.ContextName

		// Scenario 1: Private domain, NO sender OIDC, NO public OIDC
		// Expected: No OIDC buttons shown
		t.Logf("Scenario 1: Private domain, NO sender OIDC, NO public OIDC")
		aliceInstance.ContextName = originalContext
		clearPublicOIDCContext(config.DefaultInstanceContext)
		require.NoError(t, instance.Update(aliceInstance))

		sharingID1, state1 := createSharingAndGetState(t, eA, aliceInstance, aliceAppToken, "scenario1")
		body1 := eA.GET("/sharings/"+sharingID1+"/discovery").
			WithQuery("state", state1).
			Expect().Status(200).
			HasContentType("text/html", "utf-8").
			Body()

		body1.NotContains(`/oidc/sharing/public`) // No public Twake OIDC
		body1.NotContains(`/oidc/sharing?`)       // No sender OIDC

		// Scenario 2: Private domain, NO sender OIDC, WITH public OIDC
		// Expected: Only Twake button shown
		t.Logf("Scenario 2: Private domain, NO sender OIDC, WITH public OIDC")
		publicContextName := "twake-public-test"
		conf.Authentication[publicContextName] = map[string]interface{}{
			"oidc": map[string]interface{}{
				"client_id":     "public-twake-client-id",
				"client_secret": "public-twake-secret",
				"authorize_url": "https://twake.app/oauth/authorize",
				"token_url":     "https://twake.app/oauth/token",
			},
		}
		setPublicOIDCContext(config.DefaultInstanceContext, publicContextName)

		sharingID2, state2 := createSharingAndGetState(t, eA, aliceInstance, aliceAppToken, "scenario2")
		body2 := eA.GET("/sharings/"+sharingID2+"/discovery").
			WithQuery("state", state2).
			Expect().Status(200).
			Body()

		body2.Contains(`/oidc/sharing/public`)   // Public Twake OIDC shown
		body2.Contains("Login to Twake account") // Button text
		body2.NotContains(`/oidc/sharing?`)      // No sender OIDC
		// Verify dynamic copyright year
		currentYear := time.Now().Year()
		body2.Contains("© 2000-" + strconv.Itoa(currentYear) + ", LINAGORA")

		// Scenario 3: Private domain, WITH sender OIDC, NO public OIDC
		// Expected: Only sender OIDC button shown (generic fallback text)
		t.Logf("Scenario 3: Private domain, WITH sender OIDC, NO public OIDC")
		clearPublicOIDCContext(config.DefaultInstanceContext)
		senderOIDCContext := "test-sender-oidc"
		conf.Authentication[senderOIDCContext] = map[string]interface{}{
			"oidc": map[string]interface{}{
				"client_id":     "sender-oidc-client",
				"client_secret": "sender-oidc-secret",
				"authorize_url": "https://sender-oidc.example.com/authorize",
				"token_url":     "https://sender-oidc.example.com/token",
			},
		}
		aliceInstance.ContextName = senderOIDCContext
		require.NoError(t, instance.Update(aliceInstance))

		sharingID3, state3 := createSharingAndGetState(t, eA, aliceInstance, aliceAppToken, "scenario3")
		body3 := eA.GET("/sharings/"+sharingID3+"/discovery").
			WithQuery("state", state3).
			Expect().Status(200).
			Body()

		body3.Contains(`/oidc/sharing?`)          // Sender OIDC shown
		body3.Contains("sharingID=" + sharingID3) // URL parameter
		body3.Contains("Connect with OIDC")       // Generic button text (no branding)
		body3.NotContains(`/oidc/sharing/public`) // No public Twake OIDC

		// Scenario 3b: Private domain, WITH sender OIDC WITH branding, NO public OIDC
		// Expected: Only sender OIDC button with company branding
		t.Logf("Scenario 3b: Private domain, WITH sender OIDC with branding, NO public OIDC")
		brandedOIDCContext := "test-branded-oidc"
		conf.Authentication[brandedOIDCContext] = map[string]interface{}{
			"oidc": map[string]interface{}{
				"client_id":     "branded-oidc-client",
				"client_secret": "branded-oidc-secret",
				"authorize_url": "https://branded-oidc.example.com/authorize",
				"token_url":     "https://branded-oidc.example.com/token",
				"display_name":  "ACME Corporation",
				"logo_url":      "https://acme.example.com/logo.svg",
			},
		}
		aliceInstance.ContextName = brandedOIDCContext
		require.NoError(t, instance.Update(aliceInstance))

		sharingID3b, state3b := createSharingAndGetState(t, eA, aliceInstance, aliceAppToken, "scenario3b")
		body3b := eA.GET("/sharings/"+sharingID3b+"/discovery").
			WithQuery("state", state3b).
			Expect().Status(200).
			Body()

		body3b.Contains(`/oidc/sharing?`)                    // Sender OIDC shown
		body3b.Contains("ACME Corporation")                  // Company name displayed
		body3b.Contains("https://acme.example.com/logo.svg") // Logo URL in img src
		body3b.Contains("Login with")                        // Translation key part 1
		body3b.Contains("account")                           // Translation key part 2
		body3b.NotContains("Connect with OIDC")              // Generic text not shown
		body3b.NotContains(`/oidc/sharing/public`)           // No public Twake OIDC

		// Scenario 4: Private domain, WITH sender OIDC (no branding), WITH public OIDC
		// Expected: Both buttons shown (sender button without branding)
		t.Logf("Scenario 4: Private domain, WITH sender OIDC (no branding), WITH public OIDC")
		setPublicOIDCContext(config.DefaultInstanceContext, publicContextName)
		// Reset to non-branded OIDC context
		aliceInstance.ContextName = senderOIDCContext
		require.NoError(t, instance.Update(aliceInstance))

		sharingID4, state4 := createSharingAndGetState(t, eA, aliceInstance, aliceAppToken, "scenario4")
		body4 := eA.GET("/sharings/"+sharingID4+"/discovery").
			WithQuery("state", state4).
			Expect().Status(200).
			Body()

		body4.Contains(`/oidc/sharing?`)         // Sender OIDC shown
		body4.Contains(`/oidc/sharing/public`)   // Public Twake OIDC shown
		body4.Contains("Login to Twake account") // Twake button text
		body4.Contains("Connect with OIDC")      // Sender button text (generic, no branding)

		// Scenario 5: Sender context IS public OIDC context
		// Expected: Only Twake button shown (avoid duplicate buttons)
		t.Logf("Scenario 5: Sender context IS public OIDC context")
		// Set sender's context to be the same as the public OIDC context
		aliceInstance.ContextName = publicContextName
		require.NoError(t, instance.Update(aliceInstance))

		sharingID5, state5 := createSharingAndGetState(t, eA, aliceInstance, aliceAppToken, "scenario5")
		body5 := eA.GET("/sharings/"+sharingID5+"/discovery").
			WithQuery("state", state5).
			Expect().Status(200).
			Body()

		body5.Contains(`/oidc/sharing/public`)   // Public Twake OIDC shown
		body5.Contains("Login to Twake account") // Button text
		body5.NotContains(`/oidc/sharing?`)      // Sender OIDC NOT shown (same as public OIDC)

		// Cleanup
		aliceInstance.ContextName = originalContext
		require.NoError(t, instance.Update(aliceInstance))
		clearPublicOIDCContext(config.DefaultInstanceContext)
	})

	t.Run("DiscoveryAutomaticallyRedirectsToCompanyOIDCContext", func(t *testing.T) {
		conf := config.GetConfig()
		if conf.Authentication == nil {
			conf.Authentication = make(map[string]interface{})
		}
		companyContextName := "company-domain-oidc"
		conf.Authentication[companyContextName] = map[string]interface{}{
			"oidc": map[string]interface{}{
				"domain":        "example.net",
				"client_id":     "company-client-id",
				"client_secret": "company-client-secret",
				"scope":         "openid profile email",
				"redirect_uri":  "https://oauthcallback.example.net/oidc/redirect",
				"authorize_url": "https://idp.company.example/authorize",
			},
		}
		t.Cleanup(func() {
			delete(conf.Authentication, companyContextName)
		})

		eA := httpexpect.Default(t, tsA.URL)
		testSharingID, testState := createSharingAndGetState(t, eA, aliceInstance, aliceAppToken, "company-domain")

		location := eA.GET("/sharings/"+testSharingID+"/discovery").
			WithQuery("state", testState).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(http.StatusSeeOther).
			Header("Location").NotEmpty().Raw()

		redirectURL, err := url.Parse(location)
		require.NoError(t, err)
		assert.Equal(t, "https", redirectURL.Scheme)
		assert.Equal(t, "idp.company.example", redirectURL.Host)
		assert.Equal(t, "/authorize", redirectURL.Path)

		query := redirectURL.Query()
		assert.Equal(t, "code", query.Get("response_type"))
		assert.Equal(t, "openid profile email", query.Get("scope"))
		assert.Equal(t, "company-client-id", query.Get("client_id"))
		assert.Equal(t, "https://oauthcallback.example.net/oidc/redirect", query.Get("redirect_uri"))
		assert.Equal(t, "bob@example.net", query.Get("login_hint"))
		assert.NotEmpty(t, query.Get("state"))
		assert.NotEmpty(t, query.Get("nonce"))
	})

	t.Run("DiscoveryAutomaticallyRedirectsToOrgDomainOIDCContext", func(t *testing.T) {
		conf := config.GetConfig()
		if conf.Authentication == nil {
			conf.Authentication = make(map[string]interface{})
		}
		orgContextName := "company-org-oidc"
		conf.Authentication[orgContextName] = map[string]interface{}{
			"oidc": map[string]interface{}{
				"client_id":     "org-client-id",
				"client_secret": "org-client-secret",
				"scope":         "openid profile email",
				"redirect_uri":  "https://oauthcallback.example.net/oidc/redirect",
				"authorize_url": "https://idp.org.example/authorize",
			},
		}

		originalOrgDomain := bobInstance.OrgDomain
		originalContext := bobInstance.ContextName
		bobInstance.OrgDomain = "example.net"
		bobInstance.ContextName = orgContextName
		require.NoError(t, instance.Update(bobInstance))
		t.Cleanup(func() {
			delete(conf.Authentication, orgContextName)
			bobInstance.OrgDomain = originalOrgDomain
			bobInstance.ContextName = originalContext
			require.NoError(t, instance.Update(bobInstance))
		})

		eA := httpexpect.Default(t, tsA.URL)
		testSharingID, testState := createSharingAndGetState(t, eA, aliceInstance, aliceAppToken, "org-domain")

		location := eA.GET("/sharings/"+testSharingID+"/discovery").
			WithQuery("state", testState).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(http.StatusSeeOther).
			Header("Location").NotEmpty().Raw()

		redirectURL, err := url.Parse(location)
		require.NoError(t, err)
		assert.Equal(t, "https", redirectURL.Scheme)
		assert.Equal(t, "idp.org.example", redirectURL.Host)
		assert.Equal(t, "/authorize", redirectURL.Path)

		query := redirectURL.Query()
		assert.Equal(t, "code", query.Get("response_type"))
		assert.Equal(t, "openid profile email", query.Get("scope"))
		assert.Equal(t, "org-client-id", query.Get("client_id"))
		assert.Equal(t, "https://oauthcallback.example.net/oidc/redirect", query.Get("redirect_uri"))
		assert.Equal(t, "bob@example.net", query.Get("login_hint"))
		assert.NotEmpty(t, query.Get("state"))
		assert.NotEmpty(t, query.Get("nonce"))
	})

	t.Run("DiscoveryFallsBackWhenOrgDomainContextIsEmpty", func(t *testing.T) {
		conf := config.GetConfig()
		if conf.Authentication == nil {
			conf.Authentication = make(map[string]interface{})
		}
		fallbackContextName := "company-domain-fallback-oidc"
		conf.Authentication[fallbackContextName] = map[string]interface{}{
			"oidc": map[string]interface{}{
				"domain":        "example.net",
				"client_id":     "fallback-client-id",
				"client_secret": "fallback-client-secret",
				"scope":         "openid profile email",
				"redirect_uri":  "https://oauthcallback.example.net/oidc/redirect",
				"authorize_url": "https://idp.fallback.example/authorize",
			},
		}

		originalOrgDomain := bobInstance.OrgDomain
		originalContext := bobInstance.ContextName
		bobInstance.OrgDomain = "example.net"
		bobInstance.ContextName = ""
		require.NoError(t, instance.Update(bobInstance))
		t.Cleanup(func() {
			delete(conf.Authentication, fallbackContextName)
			bobInstance.OrgDomain = originalOrgDomain
			bobInstance.ContextName = originalContext
			require.NoError(t, instance.Update(bobInstance))
		})

		eA := httpexpect.Default(t, tsA.URL)
		testSharingID, testState := createSharingAndGetState(t, eA, aliceInstance, aliceAppToken, "org-domain-empty-context")

		eA.GET("/sharings/"+testSharingID+"/discovery").
			WithQuery("state", testState).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(http.StatusOK).
			Body().NotContains("idp.fallback.example")
	})
}

func TestRevokeRecipientDelegationRejectsNonDriveSharing(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	build.BuildMode = build.ModeDev
	testutils.NeedCouchdb(t)

	setup := testutils.NewSetup(t, t.Name()+"_recipient")
	inst := setup.GetTestInstance(&lifecycle.Options{
		Email:      "recipient@example.net",
		PublicName: "Recipient",
	})
	token := generateAppToken(inst, "testapp", iocozytests)
	ts := setup.GetTestServerMultipleRoutes(map[string]func(*echo.Group){
		"/sharings": sharings.Routes,
	})
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	s := &sharing.Sharing{
		SID:    "non-open-delegated-revoke",
		Active: true,
		Open:   true,
		Owner:  false,
		Rules: []sharing.Rule{{
			Title:   "test",
			DocType: iocozytests,
			Values:  []string{"doc1"},
		}},
		Members: []sharing.Member{
			{Status: sharing.MemberStatusOwner, Email: "owner@example.net", Instance: "https://owner.example"},
			{Status: sharing.MemberStatusReady, Email: "recipient@example.net", Instance: "https://recipient.example"},
			{Status: sharing.MemberStatusReady, Email: "other@example.net", Instance: "https://other.example"},
		},
		Credentials: []sharing.Credentials{{}},
	}
	require.NoError(t, couchdb.CreateNamedDocWithDB(inst, s))

	e := httpexpect.Default(t, ts.URL)
	e.DELETE("/sharings/"+s.SID+"/recipients/2").
		WithHeader("Authorization", "Bearer "+token).
		Expect().Status(http.StatusForbidden)

	stored, err := sharing.FindSharing(inst, s.SID)
	require.NoError(t, err)
	require.Equal(t, sharing.MemberStatusReady, stored.Members[2].Status)
}

func TestReadOnlyRecipientCannotPromoteSelfToEditor(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	build.BuildMode = build.ModeDev
	testutils.NeedCouchdb(t)

	setup := testutils.NewSetup(t, t.Name()+"_owner")
	inst := setup.GetTestInstance(&lifecycle.Options{
		Email:      "owner@example.net",
		PublicName: "Owner",
	})
	ts := setup.GetTestServerMultipleRoutes(map[string]func(*echo.Group){
		"/sharings": sharings.Routes,
	})
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	var recipientNotified atomic.Bool
	recipientTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recipientNotified.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(recipientTS.Close)

	sharingID := "readonly-recipient-promote-self"
	recipient := sharing.Member{
		Status:   sharing.MemberStatusReady,
		Email:    "recipient@example.net",
		Instance: recipientTS.URL,
		ReadOnly: true,
	}
	cli, err := sharing.CreateOAuthClient(inst, &recipient)
	require.NoError(t, err)
	access, err := sharing.CreateAccessToken(inst, cli, sharingID, permission.ALL)
	require.NoError(t, err)

	s := &sharing.Sharing{
		SID:    sharingID,
		Active: true,
		Owner:  true,
		Drive:  true,
		Rules: []sharing.Rule{{
			Title:   "Shared drive",
			DocType: consts.Files,
			Values:  []string{"shared-drive-root"},
		}},
		Members: []sharing.Member{
			{
				Status:     sharing.MemberStatusOwner,
				PublicName: "Owner",
				Email:      "owner@example.net",
				Instance:   "https://owner.example",
			},
			recipient,
		},
		Credentials: []sharing.Credentials{{
			AccessToken:     access,
			InboundClientID: cli.ClientID,
			XorKey:          sharing.MakeXorKey(),
		}},
	}
	require.NoError(t, couchdb.CreateNamedDocWithDB(inst, s))

	e := httpexpect.Default(t, ts.URL)
	e.DELETE("/sharings/"+sharingID+"/recipients/1/readonly").
		WithHeader("Authorization", "Bearer "+access.AccessToken).
		Expect().Status(http.StatusForbidden)

	stored, err := sharing.FindSharing(inst, sharingID)
	require.NoError(t, err)
	require.True(t, stored.Members[1].ReadOnly)
	require.False(t, recipientNotified.Load(), "recipient should not receive upgrade credentials")
}

// createSharingAndGetState creates a sharing and returns its ID and state
func createSharingAndGetState(t *testing.T, e *httpexpect.Expect, inst *instance.Instance, token, value string) (string, string) {
	t.Helper()

	bobContact := createBobContact(t, inst)
	require.NotNil(t, bobContact)

	obj := e.POST("/sharings/").
		WithHeader("Authorization", "Bearer "+token).
		WithHeader("Content-Type", "application/vnd.api+json").
		WithBytes([]byte(`{
			"data": {
				"type": "` + consts.Sharings + `",
				"attributes": {
					"description": "Test Sharing",
					"preview_path": "/preview",
					"rules": [{
						"title": "test",
						"doctype": "` + iocozytests + `",
						"values": ["` + value + `"]
					}]
				},
				"relationships": {
					"recipients": {
						"data": [{"id": "` + bobContact.ID() + `", "type": "` + bobContact.DocType() + `"}]
					}
				}
			}
		}`)).
		Expect().Status(201).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object()

	sharingID := obj.Value("data").Object().Value("id").String().NotEmpty().Raw()

	var s sharing.Sharing
	err := couchdb.GetDoc(inst, consts.Sharings, sharingID, &s)
	require.NoError(t, err)
	require.Len(t, s.Credentials, 1)

	return sharingID, s.Credentials[0].State
}

func assertSharingByAliceToBobAndDave(t *testing.T, obj *httpexpect.Array, instance *instance.Instance) {
	t.Helper()

	obj.Length().IsEqual(3)

	owner := obj.Value(0).Object()
	owner.HasValue("status", "owner")
	owner.HasValue("public_name", "Alice")
	owner.HasValue("email", "alice@example.net")
	owner.HasValue("instance", "http://"+instance.Domain)

	recipient := obj.Value(1).Object()
	recipient.HasValue("status", "pending")
	recipient.HasValue("name", "Bob")
	recipient.HasValue("email", "bob@example.net")
	recipient.NotContainsKey("read_only")

	recipient2 := obj.Value(2).Object()
	recipient2.HasValue("status", "pending")
	recipient2.HasValue("name", "Dave")
	recipient2.HasValue("email", "dave@example.net")
	recipient2.HasValue("read_only", true)
}

func assertSharingIsCorrectOnSharer(t *testing.T, obj *httpexpect.Object, sharingID string, instance *instance.Instance) {
	t.Helper()

	data := obj.Value("data").Object()

	data.HasValue("type", consts.Sharings)
	data.Value("meta").Object().Value("rev").String().NotEmpty()
	data.Value("links").Object().HasValue("self", "/sharings/"+sharingID)

	attrs := data.Value("attributes").Object()
	attrs.HasValue("description", "this is a test")
	attrs.HasValue("app_slug", "testapp")
	attrs.HasValue("owner", true)
	attrs.Value("created_at").String().AsDateTime(time.RFC3339)
	attrs.Value("updated_at").String().AsDateTime(time.RFC3339)
	attrs.NotContainsKey("credentials")

	assertSharingByAliceToBobAndDave(t, attrs.Value("members").Array(), instance)

	rules := attrs.Value("rules").Array()
	rules.Length().IsEqual(1)
	rule := rules.Value(0).Object()
	rule.HasValue("title", "test one")
	rule.HasValue("doctype", iocozytests)
	rule.HasValue("values", []interface{}{"000000"})
}

func assertInvitationMailWasSent(t *testing.T, instance *instance.Instance, owner string) string {
	var jobs []job.Job
	couchReq := &couchdb.FindRequest{
		UseIndex: "by-worker-and-state",
		Selector: mango.And(
			mango.Equal("worker", "sendmail"),
			mango.Exists("state"),
		),
		Sort: mango.SortBy{
			mango.SortByField{Field: "worker", Direction: "desc"},
		},
		Limit: 2,
	}
	err := couchdb.FindDocs(instance, consts.Jobs, couchReq, &jobs)
	assert.NoError(t, err)
	assert.Len(t, jobs, 2)
	var msg map[string]interface{}
	// Ignore the mail sent to Dave
	err = json.Unmarshal(jobs[0].Message, &msg)
	assert.NoError(t, err)
	if msg["recipient_name"] == "Dave" {
		err = json.Unmarshal(jobs[1].Message, &msg)
		assert.NoError(t, err)
	}
	assert.Equal(t, msg["mode"], "from")
	assert.Equal(t, msg["template_name"], "sharing_request")
	values := msg["template_values"].(map[string]interface{})
	assert.Equal(t, values["SharerPublicName"], owner)
	discoveryLink = values["SharingLink"].(string)
	return values["Description"].(string)
}

// extractInvitationLink returns the latest invitation mail description and
// discovery link matching the given sharing and recipient.
func extractInvitationLink(
	t *testing.T,
	instance *instance.Instance,
	sharingID string,
	owner string,
	recipientName string,
) (string, string) {
	req := couchdb.AllDocsRequest{}
	var jobs []job.Job
	err := couchdb.GetAllDocs(instance, consts.Jobs, &req, &jobs)
	require.NoError(t, err)

	expectedPath := fmt.Sprintf("/sharings/%s/discovery", sharingID)
	var bestDescription string
	var bestLink string
	var bestQueuedAt time.Time
	found := false

	for _, j := range jobs {
		if j.WorkerType != "sendmail" {
			continue
		}

		var msg map[string]interface{}
		err = json.Unmarshal(j.Message, &msg)
		require.NoError(t, err)

		if msg["mode"] != "from" || msg["template_name"] != "sharing_request" {
			continue
		}
		if recipientName != "" && msg["recipient_name"] != recipientName {
			continue
		}

		values, ok := msg["template_values"].(map[string]interface{})
		require.True(t, ok)
		if values["SharerPublicName"] != owner {
			continue
		}

		link, ok := values["SharingLink"].(string)
		require.True(t, ok)
		u, err := url.Parse(link)
		require.NoError(t, err)
		if u.Path != expectedPath {
			continue
		}

		description, ok := values["Description"].(string)
		require.True(t, ok)
		if !found || j.QueuedAt.After(bestQueuedAt) {
			bestDescription = description
			bestLink = link
			bestQueuedAt = j.QueuedAt
			found = true
		}
	}

	require.Truef(
		t,
		found,
		"no invitation link found for sharing %s and recipient %q",
		sharingID,
		recipientName,
	)
	return bestDescription, bestLink
}

func assertSharingRequestHasBeenCreated(t *testing.T, instanceA, instanceB *instance.Instance, serverURL string) {
	var results []*sharing.Sharing
	req := couchdb.AllDocsRequest{}
	err := couchdb.GetAllDocs(instanceB, consts.Sharings, &req, &results)
	assert.NoError(t, err)
	assert.Len(t, results, 1)
	s := results[0]
	assert.Equal(t, s.SID, sharingID)
	assert.False(t, s.Active)
	assert.False(t, s.Owner)
	assert.Equal(t, s.Description, "this is a test")
	assert.Equal(t, s.AppSlug, "testapp")

	assert.Len(t, s.Members, 3)
	owner := s.Members[0]
	assert.Equal(t, owner.Status, "owner")
	assert.Equal(t, owner.PublicName, "Alice")
	assert.Equal(t, owner.Email, "alice@example.net")
	assert.Equal(t, owner.Instance, "http://"+instanceA.Domain)
	recipient := s.Members[1]
	assert.Equal(t, recipient.Status, "pending")
	assert.Equal(t, recipient.Email, "bob@example.net")
	assert.Equal(t, recipient.Instance, serverURL)
	recipient = s.Members[2]
	assert.Equal(t, recipient.Status, "pending")
	assert.Equal(t, recipient.Email, "dave@example.net")
	assert.Equal(t, recipient.ReadOnly, true)

	assert.Len(t, s.Rules, 1)
	rule := s.Rules[0]
	assert.Equal(t, rule.Title, "test one")
	assert.Equal(t, rule.DocType, iocozytests)
	assert.NotEmpty(t, rule.Values)
}

func FakeOwnerInstance(t *testing.T, instance *instance.Instance, serverURL string) {
	var results []*sharing.Sharing
	req := couchdb.AllDocsRequest{}
	err := couchdb.GetAllDocs(instance, consts.Sharings, &req, &results)
	assert.NoError(t, err)
	for _, s := range results {
		if len(s.Members) > 0 {
			s.Members[0].Instance = serverURL
			_ = couchdb.UpdateDoc(instance, s)
		}
	}
}

func FakeOwnerInstanceForSharing(t *testing.T, instance *instance.Instance, serverURL string, sharingID string) {
	var s sharing.Sharing
	err := couchdb.GetDoc(instance, consts.Sharings, sharingID, &s)
	assert.NoError(t, err)
	if len(s.Members) > 0 {
		s.Members[0].Instance = serverURL
		err = couchdb.UpdateDoc(instance, &s)
		assert.NoError(t, err)
	}
}

func assertCredentialsHasBeenExchanged(t *testing.T, instanceA, instanceB *instance.Instance, urlA, urlB string) {
	var resultsA []map[string]interface{}
	req := couchdb.AllDocsRequest{}
	err := couchdb.GetAllDocs(instanceB, consts.OAuthClients, &req, &resultsA)
	assert.NoError(t, err)
	assert.True(t, len(resultsA) > 0)
	clientA := resultsA[len(resultsA)-1]
	assert.Equal(t, clientA["client_kind"], "sharing")
	assert.Equal(t, clientA["client_uri"], urlA+"/")
	assert.Equal(t, clientA["client_name"], "Sharing Alice")

	var resultsB []map[string]interface{}
	err = couchdb.GetAllDocs(instanceA, consts.OAuthClients, &req, &resultsB)
	assert.NoError(t, err)
	assert.True(t, len(resultsB) > 0)
	clientB := resultsB[len(resultsB)-1]
	assert.Equal(t, clientB["client_kind"], "sharing")
	assert.Equal(t, clientB["client_uri"], urlB+"/")
	assert.Equal(t, clientB["client_name"], "Sharing Bob")

	var sharingsA []*sharing.Sharing
	err = couchdb.GetAllDocs(instanceA, consts.Sharings, &req, &sharingsA)
	assert.NoError(t, err)
	assert.True(t, len(sharingsA) > 0)
	assert.Len(t, sharingsA[0].Credentials, 2)
	credentials := sharingsA[0].Credentials[0]
	if assert.NotNil(t, credentials.Client) {
		assert.Equal(t, credentials.Client.ClientID, clientA["_id"])
	}
	if assert.NotNil(t, credentials.AccessToken) {
		assert.NotEmpty(t, credentials.AccessToken.AccessToken)
		assert.NotEmpty(t, credentials.AccessToken.RefreshToken)
		aliceAccessToken = credentials.AccessToken.AccessToken
	}
	assert.Equal(t, sharingsA[0].Members[1].Status, "ready")
	assert.Equal(t, sharingsA[0].Members[2].Status, "pending")

	var sharingsB []*sharing.Sharing
	err = couchdb.GetAllDocs(instanceB, consts.Sharings, &req, &sharingsB)
	assert.NoError(t, err)
	assert.True(t, len(sharingsB) > 0)
	assert.Len(t, sharingsB[0].Credentials, 1)
	credentials = sharingsB[0].Credentials[0]
	if assert.NotNil(t, credentials.Client) {
		assert.Equal(t, credentials.Client.ClientID, clientB["_id"])
	}
	if assert.NotNil(t, credentials.AccessToken) {
		assert.NotEmpty(t, credentials.AccessToken.AccessToken)
		assert.NotEmpty(t, credentials.AccessToken.RefreshToken)
	}
}

func assertOneRecipientIsRevoked(t *testing.T, s *sharing.Sharing, instance *instance.Instance) {
	var sRevoked sharing.Sharing
	err := couchdb.GetDoc(instance, s.DocType(), s.SID, &sRevoked)
	assert.NoError(t, err)

	assert.Equal(t, sharing.MemberStatusRevoked, sRevoked.Members[1].Status)
	assert.Equal(t, sharing.MemberStatusReady, sRevoked.Members[2].Status)
	assert.NotEmpty(t, sRevoked.Triggers.TrackIDs)
	assert.NotEmpty(t, sRevoked.Triggers.ReplicateID)
	assert.True(t, sRevoked.Active)
}

func assertLastRecipientIsRevoked(t *testing.T, s *sharing.Sharing, refs []*sharing.SharedRef, instance *instance.Instance) {
	var sRevoked sharing.Sharing
	err := couchdb.GetDoc(instance, s.DocType(), s.SID, &sRevoked)
	assert.NoError(t, err)

	assert.Equal(t, sharing.MemberStatusRevoked, sRevoked.Members[1].Status)
	assert.Equal(t, sharing.MemberStatusRevoked, sRevoked.Members[2].Status)
	assert.Empty(t, sRevoked.Triggers.TrackIDs)
	assert.Empty(t, sRevoked.Triggers.ReplicateID)
	assert.False(t, sRevoked.Active)

	var sdoc sharing.SharedRef
	err = couchdb.GetDoc(instance, refs[0].DocType(), refs[0].ID(), &sdoc)
	assert.EqualError(t, err, "CouchDB(not_found): deleted")
	err = couchdb.GetDoc(instance, refs[1].DocType(), refs[1].ID(), &sdoc)
	assert.EqualError(t, err, "CouchDB(not_found): deleted")
}

func createBobContact(t *testing.T, instance *instance.Instance) *contact.Contact {
	return createContact(t, instance, "Bob", "bob@example.net")
}

func createContact(t *testing.T, inst *instance.Instance, name, email string) *contact.Contact {
	t.Helper()
	mail := map[string]interface{}{"address": email}
	c := contact.New()
	c.M["fullname"] = name
	c.M["email"] = []interface{}{mail}
	require.NoError(t, couchdb.CreateDoc(inst, c))
	return c
}

func createGroup(t *testing.T, inst *instance.Instance, name string) *contact.Group {
	t.Helper()
	g := contact.NewGroup()
	g.M["name"] = name
	require.NoError(t, couchdb.CreateDoc(inst, g))
	return g
}

func addContactToGroup(t *testing.T, inst *instance.Instance, g *contact.Group, contactName string) *contact.Contact {
	t.Helper()
	email := strings.ToLower(contactName) + "@example.net"
	mail := map[string]interface{}{"address": email}
	c := contact.New()
	c.M["fullname"] = contactName
	c.M["email"] = []interface{}{mail}
	c.M["relationships"] = map[string]interface{}{
		"groups": map[string]interface{}{
			"data": []interface{}{
				map[string]interface{}{
					"_id":   g.ID(),
					"_type": consts.Groups,
				},
			},
		},
	}
	require.NoError(t, couchdb.CreateDoc(inst, c))
	return c
}

func createSharing(t *testing.T, inst *instance.Instance, values []string, serverURL string) *sharing.Sharing {
	bobContact := createBobContact(t, inst)
	assert.NotNil(t, bobContact)

	r := sharing.Rule{
		Title:   "test",
		DocType: iocozytests,
		Values:  values,
		Add:     sharing.ActionRuleSync,
	}
	mail, err := bobContact.ToMailAddress()
	assert.NoError(t, err)
	m := sharing.Member{
		Name:     bobContact.Get("fullname").(string),
		Email:    mail.Email,
		Instance: serverURL,
	}
	s := &sharing.Sharing{
		Owner: true,
		Rules: []sharing.Rule{r},
	}
	s.Credentials = append(s.Credentials, sharing.Credentials{})
	err = s.BeOwner(inst, "")
	assert.NoError(t, err)
	s.Members = append(s.Members, m)

	err = couchdb.CreateDoc(inst, s)
	assert.NoError(t, err)
	assert.NotNil(t, s)
	return s
}

func createSharedDoc(inst *instance.Instance, id, sharingID string) (*sharing.SharedRef, error) {
	ref := &sharing.SharedRef{
		SID:       id,
		Revisions: &sharing.RevsTree{Rev: "1-aaa"},
		Infos: map[string]sharing.SharedInfo{
			sharingID: {Rule: 0},
		},
	}
	err := couchdb.CreateNamedDocWithDB(inst, ref)
	if err != nil {
		return nil, err
	}
	return ref, nil
}

func generateAppToken(inst *instance.Instance, slug, doctype string) string {
	rules := permission.Set{
		permission.Rule{
			Type:  doctype,
			Verbs: permission.ALL,
		},
	}
	permReq := permission.Permission{
		Permissions: rules,
		Type:        permission.TypeWebapp,
		SourceID:    consts.Apps + "/" + slug,
	}
	err := couchdb.CreateDoc(inst, &permReq)
	if err != nil {
		return ""
	}
	manifest := &couchdb.JSONDoc{
		Type: consts.Apps,
		M: map[string]interface{}{
			"_id":         consts.Apps + "/" + slug,
			"slug":        slug,
			"permissions": rules,
		},
	}
	err = couchdb.CreateNamedDocWithDB(inst, manifest)
	if err != nil {
		return ""
	}
	return inst.BuildAppToken(slug, "")
}

func TestSharingFileChangeNotification(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	// Setup config with sharing notifications enabled
	config.UseTestFile(t)
	build.BuildMode = build.ModeDev
	cfg := config.GetConfig()
	cfg.Assets = "../../assets"
	cfg.Contexts = map[string]interface{}{
		config.DefaultInstanceContext: map[string]interface{}{
			"sharing_notifications": map[string]interface{}{
				"enabled": true,
			},
		},
	}
	_ = web.LoadSupportedLocales()
	testutils.NeedCouchdb(t)
	render, _ := statik.NewDirRenderer("../../assets")
	middlewares.BuildTemplates()

	// Create test instance
	setup := testutils.NewSetup(t, t.Name()+"_owner")
	ownerInstance := setup.GetTestInstance(&lifecycle.Options{
		Email:      "owner@example.net",
		PublicName: "Owner",
	})

	// Generate app token for file operations
	ownerAppToken := generateAppToken(ownerInstance, "drive", consts.Files)
	require.NotEmpty(t, ownerAppToken)

	// Set up test server with files and permissions routes (no sharing needed)
	ts := setup.GetTestServerMultipleRoutes(map[string]func(*echo.Group){
		"/files":       files.Routes,
		"/permissions": permissions.Routes,
	})
	ts.Config.Handler.(*echo.Echo).Renderer = render
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	require.NoError(t, dynamic.InitDynamicAssetFS(config.FsURL().String()))

	e := httpexpect.Default(t, ts.URL)

	// Create a folder
	folderObj := e.POST("/files/").
		WithQuery("Name", "Shared Folder").
		WithQuery("Type", "directory").
		WithHeader("Authorization", "Bearer "+ownerAppToken).
		Expect().Status(201).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object()
	sharedDirID := folderObj.Path("$.data.id").String().NotEmpty().Raw()

	// Create a share-by-link permission for the folder
	permObj := e.POST("/permissions").
		WithQuery("codes", "anonymous").
		WithHeader("Authorization", "Bearer "+ownerAppToken).
		WithHeader("Content-Type", "application/json").
		WithBytes([]byte(`{
			"data": {
				"type": "io.cozy.permissions",
				"attributes": {
					"permissions": {
						"files": {
							"type": "io.cozy.files",
							"verbs": ["GET", "POST", "PUT", "PATCH"],
							"values": ["` + sharedDirID + `"]
						}
					}
				}
			}
		}`)).
		Expect().Status(200).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object()
	sharecode := permObj.Path("$.data.attributes.codes.anonymous").String().NotEmpty().Raw()
	require.NotEmpty(t, sharecode, "sharecode should be generated")

	// Upload a file to the shared folder using the sharecode (anonymous access)
	fileObj := e.POST("/files/"+sharedDirID).
		WithQuery("Name", "uploaded-file.pdf").
		WithQuery("Type", "file").
		WithHeader("Content-Type", "application/pdf").
		WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
		WithHeader("Authorization", "Bearer "+sharecode). // Using sharecode for anonymous access
		WithBytes([]byte("foo")).
		Expect().Status(201).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object()
	uploadedFileID := fileObj.Path("$.data.id").String().NotEmpty().Raw()
	require.NotEmpty(t, uploadedFileID)

	// Wait for the async notification to be processed and verify sendmail job
	require.Eventually(t, func() bool {
		allJobs, err := job.GetAllJobs(ownerInstance)
		if err != nil {
			return false
		}
		sendmailJobs, err := job.GetLastsJobs(allJobs, "sendmail")
		if err != nil {
			return false
		}
		for _, j := range sendmailJobs {
			var msgData map[string]interface{}
			if err := json.Unmarshal(j.Message, &msgData); err != nil {
				continue
			}
			if msgData["template_name"] == "sharing_file_changed" {
				assert.Equal(t, "noreply", msgData["mode"]) // mail.ModeFromStack = "noreply"
				values := msgData["template_values"].(map[string]interface{})
				// For share-by-link, the description is the parent folder name
				assert.Equal(t, "Shared Folder", values["SharingDescription"])
				assert.Equal(t, "uploaded-file.pdf", values["FileName"])
				assert.Contains(t, values["FileURL"], "/folder/")
				assert.Contains(t, values["FileURL"], "/file/"+uploadedFileID)
				assert.Equal(t, false, values["IsFolder"])
				return true
			}
		}
		return false
	}, 5*time.Second, 100*time.Millisecond, "Expected to find a sendmail job with template 'sharing_file_changed'")
}

// TestSharingFolderChangeNotification tests that a notification email is sent
// when a folder is created in a shared folder via share-by-link (public access).
// This test uses pure share-by-link without creating a collaborative sharing.
func TestSharingFolderChangeNotification(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	// Setup config with sharing notifications enabled
	config.UseTestFile(t)
	build.BuildMode = build.ModeDev
	cfg := config.GetConfig()
	cfg.Assets = "../../assets"
	cfg.Contexts = map[string]interface{}{
		config.DefaultInstanceContext: map[string]interface{}{
			"sharing_notifications": map[string]interface{}{
				"enabled": true,
			},
		},
	}
	_ = web.LoadSupportedLocales()
	testutils.NeedCouchdb(t)
	render, _ := statik.NewDirRenderer("../../assets")
	middlewares.BuildTemplates()

	// Create test instance
	setup := testutils.NewSetup(t, t.Name()+"_owner")
	ownerInstance := setup.GetTestInstance(&lifecycle.Options{
		Email:      "owner@example.net",
		PublicName: "Owner",
	})

	// Generate app token for file operations
	ownerAppToken := generateAppToken(ownerInstance, "drive", consts.Files)
	require.NotEmpty(t, ownerAppToken)

	// Set up test server with files and permissions routes (no sharing needed)
	ts := setup.GetTestServerMultipleRoutes(map[string]func(*echo.Group){
		"/files":       files.Routes,
		"/permissions": permissions.Routes,
	})
	ts.Config.Handler.(*echo.Echo).Renderer = render
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	require.NoError(t, dynamic.InitDynamicAssetFS(config.FsURL().String()))

	e := httpexpect.Default(t, ts.URL)

	// Create a folder via API
	folderObj := e.POST("/files/").
		WithQuery("Name", "Shared Folder").
		WithQuery("Type", "directory").
		WithHeader("Authorization", "Bearer "+ownerAppToken).
		Expect().Status(201).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object()
	sharedDirID := folderObj.Path("$.data.id").String().NotEmpty().Raw()

	// Create a share-by-link permission for the folder (public access with sharecode)
	// No collaborative sharing is needed - this is a pure share-by-link scenario
	permObj := e.POST("/permissions").
		WithQuery("codes", "anonymous").
		WithHeader("Authorization", "Bearer "+ownerAppToken).
		WithHeader("Content-Type", "application/json").
		WithBytes([]byte(`{
			"data": {
				"type": "io.cozy.permissions",
				"attributes": {
					"permissions": {
						"files": {
							"type": "io.cozy.files",
							"verbs": ["GET", "POST", "PUT", "PATCH"],
							"values": ["` + sharedDirID + `"]
						}
					}
				}
			}
		}`)).
		Expect().Status(200).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object()
	sharecode := permObj.Path("$.data.attributes.codes.anonymous").String().NotEmpty().Raw()
	require.NotEmpty(t, sharecode, "sharecode should be generated")

	// Create a subfolder in the shared folder using the sharecode (anonymous access)
	// The notification is sent automatically by the folder creation handler
	subfolderObj := e.POST("/files/"+sharedDirID).
		WithQuery("Name", "New Subfolder").
		WithQuery("Type", "directory").
		WithHeader("Authorization", "Bearer "+sharecode). // Using sharecode for anonymous access
		Expect().Status(201).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object()
	createdFolderID := subfolderObj.Path("$.data.id").String().NotEmpty().Raw()
	require.NotEmpty(t, createdFolderID)

	// Wait for the async notification to be processed and verify sendmail job
	require.Eventually(t, func() bool {
		allJobs, err := job.GetAllJobs(ownerInstance)
		if err != nil {
			return false
		}
		sendmailJobs, err := job.GetLastsJobs(allJobs, "sendmail")
		if err != nil {
			return false
		}
		for _, j := range sendmailJobs {
			var msgData map[string]interface{}
			if err := json.Unmarshal(j.Message, &msgData); err != nil {
				continue
			}
			if msgData["template_name"] == "sharing_file_changed" {
				assert.Equal(t, "noreply", msgData["mode"]) // mail.ModeFromStack = "noreply"
				values := msgData["template_values"].(map[string]interface{})
				// For share-by-link, the description is the parent folder name
				assert.Equal(t, "Shared Folder", values["SharingDescription"])
				assert.Equal(t, "New Subfolder", values["FileName"])
				assert.Contains(t, values["FileURL"], "/folder/"+createdFolderID)
				assert.Equal(t, true, values["IsFolder"])
				return true
			}
		}
		return false
	}, 5*time.Second, 100*time.Millisecond, "Expected to find a sendmail job with template 'sharing_file_changed'")
}

// TestReadOnlyHandlers tests the AddReadOnly and RemoveReadOnly handlers
// that validate index parameters for readonly status management.
func TestReadOnlyHandlers(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	build.BuildMode = build.ModeDev
	config.GetConfig().Assets = "../../assets"
	_ = web.LoadSupportedLocales()
	testutils.NeedCouchdb(t)
	render, _ := statik.NewDirRenderer("../../assets")
	middlewares.BuildTemplates()
	require.NoError(t, dynamic.InitDynamicAssetFS(config.FsURL().String()))

	// Prepare Owner's instance
	ownerSetup := testutils.NewSetup(t, t.Name()+"_owner")
	ownerInstance := ownerSetup.GetTestInstance(&lifecycle.Options{
		Email:      "owner@example.net",
		PublicName: "Owner",
	})
	ownerAppToken := generateAppToken(ownerInstance, "drive", consts.Files)

	// Create a contact for the recipient
	recipientContact := createContact(t, ownerInstance, "Recipient", "recipient@example.net")

	tsOwner := ownerSetup.GetTestServerMultipleRoutes(map[string]func(*echo.Group){
		"/sharings": sharings.Routes,
		"/files":    files.Routes,
	})
	tsOwner.Config.Handler.(*echo.Echo).Renderer = render
	tsOwner.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(tsOwner.Close)

	eOwner := httpexpect.Default(t, tsOwner.URL)

	// Create a sharing with a read-write recipient
	dirID := eOwner.POST("/files/").
		WithQuery("Name", "Shared Folder").
		WithQuery("Type", "directory").
		WithHeader("Authorization", "Bearer "+ownerAppToken).
		Expect().Status(201).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object().Path("$.data.id").String().NotEmpty().Raw()

	sharingObj := eOwner.POST("/sharings/").
		WithHeader("Authorization", "Bearer "+ownerAppToken).
		WithHeader("Content-Type", "application/vnd.api+json").
		WithBytes([]byte(`{
			"data": {
				"type": "io.cozy.sharings",
				"attributes": {
					"description": "Test Sharing for Readonly",
					"open_sharing": true,
					"rules": [{
						"title": "Shared Folder",
						"doctype": "io.cozy.files",
						"values": ["` + dirID + `"],
						"add": "sync",
						"update": "sync",
						"remove": "sync"
					}]
				},
				"relationships": {
					"recipients": {
						"data": [{
							"id": "` + recipientContact.ID() + `",
							"type": "io.cozy.contacts"
						}]
					}
				}
			}
		}`)).
		Expect().Status(201).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object()

	testSharingID := sharingObj.Path("$.data.id").String().NotEmpty().Raw()

	t.Run("AddReadOnly_InvalidIndex", func(t *testing.T) {
		e := httpexpect.Default(t, tsOwner.URL)

		// Index 0 is the owner, should be invalid (returns 422 from jsonapi.InvalidParameter)
		e.POST("/sharings/"+testSharingID+"/recipients/0/readonly").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			Expect().Status(422)

		// Index out of range returns 422 Unprocessable Entity
		e.POST("/sharings/"+testSharingID+"/recipients/99/readonly").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			Expect().Status(422)

		// Non-numeric index (returns 422 from jsonapi.InvalidParameter)
		e.POST("/sharings/"+testSharingID+"/recipients/invalid/readonly").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			Expect().Status(422)
	})

	t.Run("RemoveReadOnly_InvalidIndex", func(t *testing.T) {
		e := httpexpect.Default(t, tsOwner.URL)

		// Index 0 is the owner, should be invalid (returns 422 from jsonapi.InvalidParameter)
		e.DELETE("/sharings/"+testSharingID+"/recipients/0/readonly").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			Expect().Status(422)

		// Index out of range returns 422 Unprocessable Entity
		e.DELETE("/sharings/"+testSharingID+"/recipients/99/readonly").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			Expect().Status(422)
	})
}
