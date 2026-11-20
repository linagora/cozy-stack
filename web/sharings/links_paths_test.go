package sharings_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/assets/dynamic"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/cozy/cozy-stack/web/permissions"
	"github.com/cozy/cozy-stack/web/realtime"
	"github.com/cozy/cozy-stack/web/statik"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func watchFilePaths(t *testing.T, e *httpexpect.Expect, token, id string) *httpexpect.Websocket {
	t.Helper()
	ws := e.GET("/realtime/").WithWebsocketUpgrade().Expect().Status(http.StatusSwitchingProtocols).Websocket()
	t.Cleanup(func() { ws.Disconnect() })
	ws.WriteText(fmt.Sprintf(`{"method":"AUTH","payload":%q}`, token))
	ws.WriteText(fmt.Sprintf(`{"method":"SUBSCRIBE","payload":{"type":"io.cozy.files","id":%q}}`, id))
	time.Sleep(100 * time.Millisecond)
	return ws
}

func TestPublicLinkOnASharedDriveFolder(t *testing.T) {
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

	acme, acmeToken, tsA := newDriveOwnerTestServerWithOptions(t, strings.ReplaceAll(t.Name(), "/", "_")+"_acme", nil, render,
		map[string]func(*echo.Group){
			"/permissions": permissions.Routes,
			"/realtime":    realtime.Routes,
		})
	eA := newSharingExpect(t, tsA.URL)

	driveName := "Drive " + crypto.GenerateRandomString(10)
	sharingID, driveRootID, _ := createSharedDrive(t, DriveCreationMethodLegacy, acme, acmeToken, tsA.URL,
		driveName, "drive", nil)
	privateID := createRootDirectory(t, eA, "Private", acmeToken)
	clientsID := createDirectory(t, eA, privateID, "Clients", acmeToken)
	eA.PATCH("/files/"+driveRootID).
		WithHeader("Authorization", "Bearer "+acmeToken).
		WithHeader("Content-Type", "application/vnd.api+json").
		WithBytes([]byte(`{"data": {"type": "io.cozy.files", "id": "` + driveRootID + `", "attributes": {"dir_id": "` + clientsID + `"}}}`)).
		Expect().Status(http.StatusOK)
	meetingsID := createDirectory(t, eA, driveRootID, "Meetings", acmeToken)
	notesID := createDirectory(t, eA, meetingsID, "Notes", acmeToken)

	member := getSharedDriveInteractCode(t, acme, sharingID, "betty@example.net")
	payload := makeSharedDrivePermissionPayloadWithVerbs(t, consts.Files, []string{meetingsID}, []string{"GET", "POST", "PUT"}, nil)
	link := eA.POST("/permissions").WithQuery("codes", "link").
		WithHeader("Authorization", "Bearer "+acmeToken).
		WithHeader("Content-Type", jsonAPIContentType).
		WithBytes(payload).Expect().Status(http.StatusOK).
		JSON(httpexpect.ContentOpts{MediaType: jsonAPIContentType}).
		Object().Path("$.data.attributes.codes.link").String().NotEmpty().Raw()

	t.Run("ByPath", func(t *testing.T) {
		e := newSharingExpect(t, tsA.URL)
		path := e.GET("/files/"+notesID).
			WithHeader("Authorization", "Bearer "+link).
			Expect().Status(http.StatusOK).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.attributes.path").String().IsEqual("/Meetings/Notes").Raw()
		res := e.GET("/files/metadata").WithQuery("Path", path).
			WithHeader("Authorization", "Bearer "+link).
			Expect().Status(http.StatusOK)
		assert.NotContains(t, res.Body().Raw(), "Private")
		assert.NotContains(t, res.Body().Raw(), driveName)
		res.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().IsEqual(notesID)

		missing := e.GET("/files/metadata").WithQuery("Path", "/Meetings/Missing").
			WithHeader("Authorization", "Bearer "+link).
			Expect().Status(http.StatusNotFound).Body().Raw()
		for _, p := range []string{"/Private/Clients/" + driveName + "/Meetings", "/" + driveName + "/Meetings", "/" + driveName} {
			body := e.GET("/files/metadata").WithQuery("Path", p).
				WithHeader("Authorization", "Bearer "+link).
				Expect().Status(http.StatusNotFound).Body().Raw()
			assert.Equal(t, missing, body, p)
		}
	})

	t.Run("ReplaceExistingFileAfterAncestorRename", func(t *testing.T) {
		e := newSharingExpect(t, tsA.URL)
		jsonAPI := httpexpect.ContentOpts{MediaType: jsonAPIContentType}
		folderPath := func(token, expected string) string {
			return e.GET("/files/"+notesID).WithHeader("Authorization", "Bearer "+token).
				Expect().Status(http.StatusOK).JSON(jsonAPI).
				Object().Path("$.data.attributes.path").String().IsEqual(expected).Raw()
		}
		byPath := func(token, path string) *httpexpect.Object {
			return e.GET("/files/metadata").WithQuery("Path", path).
				WithHeader("Authorization", "Bearer "+token).Expect().Status(http.StatusOK).
				JSON(jsonAPI).Object().Value("data").Object()
		}
		linkPath := folderPath(link, "/Meetings/Notes")
		memberPath := folderPath(member, "/"+driveName+"/Meetings/Notes")
		const name = "agenda.txt"
		fileID := createFile(t, e, notesID, name, link)
		patchDirName(t, e, acmeToken, clientsID, "Accounts")

		byPath(member, memberPath).ValueEqual("id", notesID)
		byPath(link, linkPath).ValueEqual("id", notesID)
		e.POST("/files/"+notesID).WithQuery("Name", name).WithQuery("Type", "file").
			WithHeader("Authorization", "Bearer "+link).WithHeader("Content-Type", "text/plain").
			WithBytes([]byte("new agenda")).Expect().Status(http.StatusConflict)
		existing := byPath(link, linkPath+"/"+name)
		id := existing.Value("id").String().IsEqual(fileID).Raw()
		rev := existing.Path("$.meta.rev").String().NotEmpty().Raw()
		e.PUT("/files/"+id).WithHeader("If-Match", rev).
			WithHeader("Authorization", "Bearer "+link).WithHeader("Content-Type", "text/plain").
			WithBytes([]byte("new agenda")).Expect().Status(http.StatusOK)
		byPath(member, memberPath+"/"+name).ValueEqual("id", fileID)
		e.GET("/files/download/"+fileID).WithHeader("Authorization", "Bearer "+member).
			Expect().Status(http.StatusOK).Body().IsEqual("new agenda")
	})

	t.Run("Realtime", func(t *testing.T) {
		e := newSharingExpect(t, tsA.URL)
		visitorWS := watchFilePaths(t, e, link, meetingsID)
		memberWS := watchFilePaths(t, e, member, meetingsID)
		patchDirName(t, e, acmeToken, clientsID, "Renamed")

		msg := visitorWS.Expect().TextMessage()
		assert.NotContains(t, msg.Body().Raw(), "Private")
		assert.NotContains(t, msg.Body().Raw(), "Renamed")
		assert.NotContains(t, msg.Body().Raw(), driveName)
		msg.JSON().Object().Path("$.payload.doc.path").String().IsEqual("/Meetings")
		msg = memberWS.Expect().TextMessage()
		assert.NotContains(t, msg.Body().Raw(), "Private")
		assert.NotContains(t, msg.Body().Raw(), "Renamed")
		msg.JSON().Object().Path("$.payload.doc.path").String().IsEqual("/" + driveName + "/Meetings")

		meetings, err := acme.VFS().DirByID(meetingsID)
		require.NoError(t, err)
		_, err = vfs.ModifyDirMetadata(acme.VFS(), meetings, &vfs.DocPatch{DirID: &privateID})
		require.NoError(t, err)
		msg = memberWS.Expect().TextMessage()
		assert.NotContains(t, msg.Body().Raw(), "Private")
		msg.JSON().Object().Path("$.payload.doc").Object().NotContainsKey("path")
	})
}

func TestSharingCodesPaths(t *testing.T) {
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

	acme, acmeToken, tsA := newDriveOwnerTestServerWithOptions(t, strings.ReplaceAll(t.Name(), "/", "_")+"_acme", nil, render,
		map[string]func(*echo.Group){"/realtime": realtime.Routes})
	eA := newSharingExpect(t, tsA.URL)

	privateID := createRootDirectory(t, eA, "Private", acmeToken)
	clientsID := createDirectory(t, eA, privateID, "Clients", acmeToken)
	docsID := createDirectory(t, eA, clientsID, "Docs", acmeToken)
	createDirectory(t, eA, docsID, "Sub", acmeToken)

	s := createTestSharing(t, acme, docsID, false)
	preview, err := s.CreatePreviewPermissions(acme)
	require.NoError(t, err)

	for name, code := range map[string]string{
		"Preview":  preview.Codes["bob@example.net"],
		"Interact": getSharedDriveInteractCode(t, acme, s.SID, "bob@example.net"),
	} {
		require.NotEmpty(t, code)
		t.Run(name, func(t *testing.T) {
			t.Run("ByID", func(t *testing.T) {
				e := newSharingExpect(t, tsA.URL)
				res := e.GET("/files/"+docsID).WithHeader("Authorization", "Bearer "+code).
					Expect().Status(http.StatusOK)
				assert.NotContains(t, res.Body().Raw(), "Private")
				assert.NotContains(t, res.Body().Raw(), "Clients")
				obj := res.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).Object()
				obj.Path("$.data.attributes.path").String().IsEqual("/Docs")
				obj.Path("$.included[0].attributes.path").String().IsEqual("/Docs/Sub")
			})

			t.Run("ByPath", func(t *testing.T) {
				e := newSharingExpect(t, tsA.URL)
				e.GET("/files/metadata").WithQuery("Path", "/Docs/Sub").
					WithHeader("Authorization", "Bearer "+code).
					Expect().Status(http.StatusOK).
					JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
					Object().Path("$.data.attributes.path").String().IsEqual("/Docs/Sub")
				e.GET("/files/metadata").WithQuery("Path", "/Private/Clients/Docs").
					WithHeader("Authorization", "Bearer "+code).
					Expect().Status(http.StatusNotFound)
			})

			t.Run("Realtime", func(t *testing.T) {
				e := newSharingExpect(t, tsA.URL)
				ws := watchFilePaths(t, e, code, docsID)

				newName := "Renamed" + name
				e.PATCH("/files/"+clientsID).
					WithHeader("Authorization", "Bearer "+acmeToken).
					WithHeader("Content-Type", "application/vnd.api+json").
					WithBytes([]byte(`{"data": {"type": "io.cozy.files", "id": "` + clientsID + `", "attributes": {"name": "` + newName + `"}}}`)).
					Expect().Status(http.StatusOK)

				msg := ws.Expect().TextMessage()
				assert.NotContains(t, msg.Body().Raw(), "Private")
				assert.NotContains(t, msg.Body().Raw(), newName)
				msg.JSON().Object().Path("$.payload.doc.path").String().IsEqual("/Docs")
			})
		})
	}
	t.Run("MultipleInteractRoots", func(t *testing.T) {
		e := newSharingExpect(t, tsA.URL)
		otherID := createDirectory(t, e, clientsID, "Other", acmeToken)
		multi := createTestSharing(t, acme, docsID, false)
		multi.Rules[0].Values = []string{docsID, otherID}
		require.NoError(t, couchdb.UpdateDoc(acme, multi))
		code := getSharedDriveInteractCode(t, acme, multi.SID, "bob@example.net")
		for _, id := range multi.Rules[0].Values {
			attrs := e.GET("/files/"+id).WithHeader("Authorization", "Bearer "+code).
				Expect().Status(http.StatusOK).JSON(httpexpect.ContentOpts{MediaType: jsonAPIContentType}).
				Object().Path("$.data.attributes").Object()
			attrs.NotContainsKey("path")
			attrs.NotContainsKey("restore_path")
		}
		e.GET("/files/metadata").WithQuery("Path", "/Docs/Sub").WithHeader("Authorization", "Bearer "+code).
			Expect().Status(http.StatusNotFound)
		ws := watchFilePaths(t, e, code, docsID)
		patchDirName(t, e, acmeToken, docsID, "Documents")
		msg := ws.Expect().TextMessage().JSON().Object()
		msg.Path("$.payload.id").String().IsEqual(docsID)
		msg.Path("$.payload.doc").Object().NotContainsKey("path")
	})
}
