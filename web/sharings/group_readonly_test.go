package sharings_test

import (
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/assets/dynamic"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/cozy/cozy-stack/web/files"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/cozy/cozy-stack/web/sharings"
	"github.com/cozy/cozy-stack/web/statik"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestGroupReadOnlyHandlers(t *testing.T) {
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

	ownerSetup := testutils.NewSetup(t, t.Name()+"_owner")
	ownerInstance := ownerSetup.GetTestInstance(&lifecycle.Options{
		Email:      "owner@example.net",
		PublicName: "Owner",
	})
	ownerAppToken := generateAppToken(ownerInstance, "drive", consts.Files)

	// Create contacts and groups
	group := createContactGroup(t, ownerInstance, "Friends")
	createContactInGroup(t, ownerInstance, "Bob", group.ID())
	createContactInGroup(t, ownerInstance, "Charlie", group.ID())

	tsOwner := ownerSetup.GetTestServerMultipleRoutes(map[string]func(*echo.Group){
		"/sharings": sharings.Routes,
		"/files":    files.Routes,
	})
	tsOwner.Config.Handler.(*echo.Echo).Renderer = render
	tsOwner.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(tsOwner.Close)

	eOwner := httpexpect.Default(t, tsOwner.URL)

	// Create a shared directory
	dirID := eOwner.POST("/files/").
		WithQuery("Name", "Shared Folder").
		WithQuery("Type", "directory").
		WithHeader("Authorization", "Bearer "+ownerAppToken).
		Expect().Status(201).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object().Path("$.data.id").String().NotEmpty().Raw()

	// Create a sharing with a read-write group
	sharingObj := eOwner.POST("/sharings/").
		WithHeader("Authorization", "Bearer "+ownerAppToken).
		WithHeader("Content-Type", "application/vnd.api+json").
		WithBytes([]byte(`{
			"data": {
				"type": "io.cozy.sharings",
				"attributes": {
					"description": "Test Sharing for Group ReadOnly",
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
							"id": "` + group.ID() + `",
							"type": "io.cozy.contacts.groups"
						}]
					}
				}
			}
		}`)).
		Expect().Status(201).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object()

	testSharingID := sharingObj.Path("$.data.id").String().NotEmpty().Raw()

	t.Run("AddReadOnlyToGroup_InvalidIndex", func(t *testing.T) {
		e := httpexpect.Default(t, tsOwner.URL)

		// Negative group index returns 422
		e.POST("/sharings/"+testSharingID+"/groups/-1/readonly").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			Expect().Status(422)

		// Group index out of range returns 422
		e.POST("/sharings/"+testSharingID+"/groups/99/readonly").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			Expect().Status(422)

		// Non-numeric index returns 422
		e.POST("/sharings/"+testSharingID+"/groups/invalid/readonly").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			Expect().Status(422)
	})

	t.Run("RemoveReadOnlyFromGroup_InvalidIndex", func(t *testing.T) {
		e := httpexpect.Default(t, tsOwner.URL)

		// Negative group index returns 422
		e.DELETE("/sharings/"+testSharingID+"/groups/-1/readonly").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			Expect().Status(422)

		// Group index out of range returns 422
		e.DELETE("/sharings/"+testSharingID+"/groups/99/readonly").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			Expect().Status(422)

		// Non-numeric index returns 422
		e.DELETE("/sharings/"+testSharingID+"/groups/invalid/readonly").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			Expect().Status(422)
	})

	// Create a sharing with a read-only group to test per-member consistency
	roGroup := createContactGroup(t, ownerInstance, "RO Team")
	createContactInGroup(t, ownerInstance, "Dave", roGroup.ID())

	roSharingObj := eOwner.POST("/sharings/").
		WithHeader("Authorization", "Bearer "+ownerAppToken).
		WithHeader("Content-Type", "application/vnd.api+json").
		WithBytes([]byte(`{
			"data": {
				"type": "io.cozy.sharings",
				"attributes": {
					"description": "Test RO Group consistency",
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
					"read_only_recipients": {
						"data": [{
							"id": "` + roGroup.ID() + `",
							"type": "io.cozy.contacts.groups"
						}]
					}
				}
			}
		}`)).
		Expect().Status(201).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object()

	roSharingID := roSharingObj.Path("$.data.id").String().NotEmpty().Raw()

	t.Run("PerMemberReadOnly_BlockedByGroup", func(t *testing.T) {
		// Dave is member index 1, in a RO group. Both per-member readonly
		// routes should be blocked because he belongs to a group.
		e := httpexpect.Default(t, tsOwner.URL)

		// Upgrade to RW blocked (was RO, in a RO group)
		e.DELETE("/sharings/"+roSharingID+"/recipients/1/readonly").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			Expect().Status(400)

		// Downgrade to RO also blocked (in a group, even if already RO)
		e.POST("/sharings/"+roSharingID+"/recipients/1/readonly").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			Expect().Status(400)
	})
}

func createContactGroup(t *testing.T, inst *instance.Instance, name string) *couchdb.JSONDoc {
	t.Helper()
	g := couchdb.JSONDoc{
		Type: consts.Groups,
		M: map[string]interface{}{
			"name": name,
		},
	}
	require.NoError(t, couchdb.CreateDoc(inst, &g))
	return &g
}

func createContactInGroup(t *testing.T, inst *instance.Instance, contactName, groupID string) *couchdb.JSONDoc {
	t.Helper()
	email := contactName + "@example.net"
	c := couchdb.JSONDoc{
		Type: consts.Contacts,
		M: map[string]interface{}{
			"fullname": contactName,
			"email": []interface{}{map[string]interface{}{
				"address": email,
			}},
			"relationships": map[string]interface{}{
				"groups": map[string]interface{}{
					"data": []interface{}{
						map[string]interface{}{
							"_id":   groupID,
							"_type": consts.Groups,
						},
					},
				},
			},
		},
	}
	require.NoError(t, couchdb.CreateDoc(inst, &c))
	return &c
}
