package sharings_test

import (
	"strings"
	"testing"

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
	"github.com/cozy/cozy-stack/web/sharings"
	"github.com/cozy/cozy-stack/web/statik"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestSharedDriveWriteRecordsRecipientRecents(t *testing.T) {
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
	acme, acmeToken, tsA := newDriveOwnerTestServer(t, testName+"_acme",
		"acme@example.net", "ACME", render, nil)

	// The /sharings routes of the recipient do not load the session from a
	// cookie: inject Betty's session, as a browser with a session would, so
	// that her session-bound app tokens are accepted. It is set only once the
	// sharing is accepted, to leave the acceptance flow untouched.
	var bettySession *session.Session
	betty, serviceToken, tsB := newDriveRecipientTestServer(t, testName+"_betty",
		"betty@example.net", "Betty", render,
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

	sess, err := session.New(betty, session.LongRun, "")
	require.NoError(t, err)
	bettySession = sess
	driveToken := betty.BuildAppToken("drive", sess.ID())

	testutils.WithFlag(t, betty, "drive.recents.enabled", true)

	eB := newSharingExpect(t, tsB.URL)

	createInDrive := func(t *testing.T, name, token string) string {
		t.Helper()
		return eB.POST("/sharings/drives/"+sharingID+"/"+rootDirID).
			WithQuery("Name", name).
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()
	}
	findRow := func(t *testing.T, fileID string) error {
		t.Helper()
		var doc recents.FileDirActivity
		return couchdb.GetDoc(betty, consts.FilesRecents, recents.ResourceKey(acme, fileID), &doc)
	}

	t.Run("SessionTokenRecordsAnEdit", func(t *testing.T) {
		fileID := createInDrive(t, "by-betty.txt", driveToken)
		require.NoError(t, findRow(t, fileID))
	})

	t.Run("ServiceTokenRecordsNothing", func(t *testing.T) {
		// The drive token used on the owner's side is a share-interact code,
		// but the person is checked on Betty's original token.
		fileID := createInDrive(t, "by-a-service.txt", serviceToken)
		err := findRow(t, fileID)
		require.Error(t, err)
		require.True(t, couchdb.IsNotFoundError(err), "unexpected error: %v", err)
	})
}
