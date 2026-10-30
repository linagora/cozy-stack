package sharings_test

import (
	"strings"
	"testing"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/session"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/assets/dynamic"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/cozy/cozy-stack/web/notes"
	"github.com/cozy/cozy-stack/web/permissions"
	"github.com/cozy/cozy-stack/web/sharings"
	"github.com/cozy/cozy-stack/web/statik"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestSharedDriveOpenRecordsRecipientRecents(t *testing.T) {
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

	testName := strings.ReplaceAll(t.Name(), "/", "_")
	acme, acmeToken, tsA := newDriveOwnerTestServer(
		t,
		testName+"_acme",
		"acme@example.net",
		"ACME",
		render,
		map[string]func(*echo.Group){
			"/notes":       notes.Routes,
			"/permissions": permissions.Routes,
		},
	)

	// The /sharings routes of the recipient do not load the session from a
	// cookie: inject Betty's session, as a browser with a session would, so
	// that her session-bound app tokens are accepted. It is set only once the
	// sharing is accepted, to leave the acceptance flow untouched.
	var bettySession *session.Session
	betty, _, tsB := newDriveRecipientTestServer(
		t,
		testName+"_betty",
		"betty@example.net",
		"Betty",
		render,
		map[string]func(*echo.Group){
			"/sharings": func(g *echo.Group) {
				g.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
					return func(c echo.Context) error {
						if bettySession != nil {
							c.Set("session", bettySession)
						}
						return next(c)
					}
				})
				sharings.Routes(g)
			},
		},
	)

	sharingID, rootDirID, _ := createSharedDrive(t, DriveCreationMethodLegacy, acme, acmeToken, tsA.URL,
		"Recents shared drive", "Shared drive for recents tests", nil)
	acceptSharedDriveForBetty(t, acme, betty, tsA.URL, tsB.URL, sharingID)

	// Betty's copy points to the owner's domain, so that the request takes the
	// same stack shortcut instead of the HTTP proxy.
	bettySharing, err := sharing.FindSharing(betty, sharingID)
	require.NoError(t, err)
	bettySharing.Members[0].Instance = "http://" + acme.Domain
	require.NoError(t, couchdb.UpdateDoc(betty, bettySharing))

	// The owner knows Betty by the URL of her test server (discovery). Point
	// it to her domain, as on a real stack, so that a sharecode resolves to
	// her instance. The sharecodes are keyed by her email, not by this URL.
	ownerSharing, err := sharing.FindSharing(acme, sharingID)
	require.NoError(t, err)
	bettyIndex := findSharingMemberIndexByEmail(t, acme, sharingID, "betty@example.net")
	ownerSharing.Members[bettyIndex].Instance = "http://" + betty.Domain
	require.NoError(t, couchdb.UpdateDoc(acme, ownerSharing))

	sess, err := session.New(betty, session.LongRun, "")
	require.NoError(t, err)
	bettySession = sess

	// The drive app already exists on Betty's instance (test server setup).
	driveToken := betty.BuildAppToken("drive", sess.ID())

	testutils.WithFlag(t, betty, "drive.recents.enabled", true)

	eA := newSharingExpect(t, tsA.URL)
	eB := newSharingExpect(t, tsB.URL)

	findRow := func(t *testing.T, fileID string) (*recents.FileDirActivity, error) {
		t.Helper()
		var doc recents.FileDirActivity
		err := couchdb.GetDoc(betty, consts.FilesRecents, recents.ResourceKey(acme, fileID), &doc)
		if err != nil {
			return nil, err
		}
		return &doc, nil
	}
	requireNoRow := func(t *testing.T, fileID string) {
		t.Helper()
		_, err := findRow(t, fileID)
		require.Error(t, err)
		require.True(t, couchdb.IsNotFoundError(err), "unexpected error: %v", err)
	}

	t.Run("DownloadRecordsNothing", func(t *testing.T) {
		fileID := createFile(t, eA, rootDirID, "downloaded.excalidraw", acmeToken)

		eB.GET("/sharings/drives/"+sharingID+"/download/"+fileID).
			WithHeader("Authorization", "Bearer "+driveToken).
			Expect().Status(200)

		requireNoRow(t, fileID)
	})

	// Drive opens a drawing or a PDF file of a shared drive with the editor
	// route, before reading its content.
	for _, open := range []struct {
		name     string
		readOnly bool
	}{
		{"drawing.excalidraw", false},
		{"read-only.excalidraw", true},
		{"document.pdf", false},
	} {
		t.Run("EditorOpenRecordsAView/"+open.name, func(t *testing.T) {
			fileID := createFile(t, eA, rootDirID, open.name, acmeToken)

			req := eB.GET("/sharings/drives/" + sharingID + "/editor/" + fileID + "/open")
			if open.readOnly {
				req = req.WithQuery("ReadOnly", "true")
			}
			req.WithHeader("Authorization", "Bearer "+driveToken).
				Expect().Status(200)

			row, err := findRow(t, fileID)
			require.NoError(t, err)
			require.NotEmpty(t, row.LastViewedAt)

			// The row is in the recipient's recents, never in the owner's.
			var ownerDoc recents.FileDirActivity
			err = couchdb.GetDoc(acme, consts.FilesRecents, recents.ResourceKey(acme, fileID), &ownerDoc)
			require.Error(t, err)
		})
	}

	// createNote creates a note in the drive on the owner's side, so that
	// Betty has no activity on it before she opens it.
	createNote := func(t *testing.T, title string) string {
		t.Helper()
		return eA.POST("/notes").
			WithHeader("Authorization", "Bearer "+acmeToken).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
				"data": {
					"type": "io.cozy.notes.documents",
					"attributes": {
						"title": "` + title + `",
						"dir_id": "` + rootDirID + `",
						"schema": {
							"nodes": [
								["doc", { "content": "block+" }],
								["paragraph", { "content": "inline*", "group": "block" }],
								["text", { "group": "inline" }]
							],
							"marks": [],
							"topNode": "doc"
						}
					}
				}
			}`)).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()
	}

	// openNote opens the note as Betty from her instance, checks the kind of
	// the sharecode it gets, then loads the note on the owner's instance with
	// that sharecode, as the notes app does.
	openNote := func(t *testing.T, noteID string, readOnly bool, codeType string) {
		t.Helper()
		req := eB.GET("/sharings/drives/" + sharingID + "/notes/" + noteID + "/open")
		if readOnly {
			req = req.WithQuery("ReadOnly", "true")
		}
		attrs := req.
			WithHeader("Authorization", "Bearer "+driveToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.attributes").Object()
		attrs.Value("instance").String().IsEqual(acme.Domain)
		sharecode := attrs.Value("sharecode").String().NotEmpty().Raw()

		eA.GET("/permissions/self").
			WithHeader("Authorization", "Bearer "+sharecode).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.attributes.type").String().IsEqual(codeType)

		requireNoRow(t, noteID)
		eA.GET("/notes/"+noteID).
			WithHeader("Authorization", "Bearer "+sharecode).
			Expect().Status(200)
	}

	requireNoteViewed := func(t *testing.T, noteID string) {
		t.Helper()
		row, err := findRow(t, noteID)
		require.NoError(t, err)
		require.NotEmpty(t, row.LastViewedAt)

		var ownerDoc recents.FileDirActivity
		err = couchdb.GetDoc(acme, consts.FilesRecents, recents.ResourceKey(acme, noteID), &ownerDoc)
		require.Error(t, err)
		require.True(t, couchdb.IsNotFoundError(err), "unexpected error: %v", err)
	}

	t.Run("ReadOnlyNoteOpenRecordsAView", func(t *testing.T) {
		noteID := createNote(t, "Read-only note")
		openNote(t, noteID, true, permission.TypeSharePreview)
		requireNoteViewed(t, noteID)
	})

	t.Run("WritableNoteOpenRecordsAView", func(t *testing.T) {
		noteID := createNote(t, "Writable note")
		openNote(t, noteID, false, permission.TypeShareInteract)
		requireNoteViewed(t, noteID)
	})
}
