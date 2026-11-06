package sharings_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/session"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/assets/dynamic"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web"
	"github.com/cozy/cozy-stack/web/files"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/cozy/cozy-stack/web/sharings"
	"github.com/cozy/cozy-stack/web/statik"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withSession injects a session in the routes, as a browser with a session
// would, so that session-bound app tokens stand for a person. The session is
// read at each request, to leave the acceptance flow without one.
func withSession(sess **session.Session, routes func(*echo.Group)) func(*echo.Group) {
	return func(g *echo.Group) {
		g.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				if *sess != nil {
					c.Set("session", *sess)
				}
				return next(c)
			}
		})
		routes(g)
	}
}

func findDriveRecentsRow(t *testing.T, inst *instance.Instance, id string) *recents.FileDirActivity {
	t.Helper()
	var doc recents.FileDirActivity
	err := couchdb.GetDoc(inst, consts.FilesRecents, id, &doc)
	if couchdb.IsNotFoundError(err) || couchdb.IsNoDatabaseError(err) {
		return nil
	}
	require.NoError(t, err)
	return &doc
}

// pinOwnerToTheStack makes the request of a recipient take the same stack
// shortcut of the proxy instead of an HTTP request.
func pinOwnerToTheStack(t *testing.T, recipient, owner *instance.Instance, sharingID string) {
	t.Helper()
	FakeOwnerInstanceForSharing(t, recipient, "http://"+owner.Domain, sharingID)
}

// pinMemberToTheStack gives a member of the owner's sharing the URL of its
// instance on this stack, and the status of a member who has accepted.
func pinMemberToTheStack(t *testing.T, owner, member *instance.Instance, sharingID, email string) {
	t.Helper()
	s, err := sharing.FindSharing(owner, sharingID)
	require.NoError(t, err)
	for i := range s.Members {
		if s.Members[i].Email == email {
			s.Members[i].Instance = "http://" + member.Domain
			s.Members[i].Status = sharing.MemberStatusReady
		}
	}
	s.Active = true
	require.NoError(t, couchdb.UpdateDoc(owner, s))
}

func TestSharedDriveRecents(t *testing.T) {
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

	var acmeSession, bettySession, daveSession *session.Session
	acme, acmeToken, tsA := newDriveOwnerTestServer(t, testName+"_acme",
		"acme@example.net", "ACME", render, map[string]func(*echo.Group){
			"/files":    withSession(&acmeSession, files.Routes),
			"/sharings": withSession(&acmeSession, sharings.Routes),
		})
	betty, bettyServiceToken, tsB := newDriveRecipientTestServer(t, testName+"_betty",
		"betty@example.net", "Betty", render, map[string]func(*echo.Group){
			"/sharings": withSession(&bettySession, sharings.Routes),
		})
	dave, _, tsD := newDriveRecipientTestServer(t, testName+"_dave",
		"dave@example.net", "Dave", render, map[string]func(*echo.Group){
			"/sharings": withSession(&daveSession, sharings.Routes),
		})

	// Betty can write in the drive, Dave can only read it. They can both write
	// in a nested drive. Acme, Betty and Dave have the recents enabled.
	eA := newSharingExpect(t, tsA.URL)
	eB := newSharingExpect(t, tsB.URL)
	eD := newSharingExpect(t, tsD.URL)
	driveID, rootID, _ := createSharedDrive(t, DriveCreationMethodFromFolder, acme, acmeToken, tsA.URL,
		"Team drive", "Team", []RecipientInfo{
			{Name: "Betty", Email: "betty@example.net", ReadOnly: false},
			{Name: "Dave", Email: "dave@example.net", ReadOnly: true},
		})
	subID := createDirectory(t, eA, rootID, "Sub", acmeToken)
	nestedRootID := createDirectory(t, eA, rootID, "Nested", acmeToken)
	nestedID := createDriveOnFolder(t, eA, acme, nestedRootID, acmeToken, []RecipientInfo{
		{Name: "Betty", Email: "betty@example.net", ReadOnly: false},
		{Name: "Dave", Email: "dave@example.net", ReadOnly: false},
	})
	for _, id := range []string{driveID, nestedID} {
		acceptSharedDrive(t, acme, betty, "Betty", tsA.URL, tsB.URL, id)
		acceptSharedDrive(t, acme, dave, "Dave", tsA.URL, tsD.URL, id)
		pinOwnerToTheStack(t, betty, acme, id)
		pinOwnerToTheStack(t, dave, acme, id)
	}

	var err error
	acmeSession, err = session.New(acme, session.LongRun, "")
	require.NoError(t, err)
	bettySession, err = session.New(betty, session.LongRun, "")
	require.NoError(t, err)
	daveSession, err = session.New(dave, session.LongRun, "")
	require.NoError(t, err)
	acmePerson := acme.BuildAppToken("drive", acmeSession.ID())
	bettyPerson := betty.BuildAppToken("drive", bettySession.ID())
	davePerson := dave.BuildAppToken("drive", daveSession.ID())

	testutils.WithFlag(t, acme, "drive.recents.enabled", true)
	testutils.WithFlag(t, betty, "drive.recents.enabled", true)
	testutils.WithFlag(t, dave, "drive.recents.enabled", true)

	createInDrive := func(t *testing.T, e *httpexpect.Expect, sharingID, dirID, name, token string) string {
		t.Helper()
		return e.POST("/sharings/drives/"+sharingID+"/"+dirID).
			WithQuery("Name", name).
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()
	}

	t.Run("MemberWriteRecordsOnTheirInstanceWithTheDrive", func(t *testing.T) {
		fileID := createInDrive(t, eB, driveID, subID, "by-betty.txt", bettyPerson)

		row := findDriveRecentsRow(t, betty, fileID)
		require.NotNil(t, row)
		assert.Equal(t, driveID, row.DriveID)
		assert.Equal(t, acme.Domain, row.OwnerDomain)
		assert.Equal(t, "/Sub/by-betty.txt", row.Path, "no name above the content of the drive")
		assert.Equal(t, "by-betty.txt", row.Name)
		assert.Equal(t, consts.FileType, row.Type)
		assert.Equal(t, "text/plain", row.Mime)
		assert.Equal(t, "3", row.Size)
		assert.Equal(t, subID, row.DirID)
		assert.NotEmpty(t, row.LastEditedAt)

		assert.Nil(t, findDriveRecentsRow(t, acme, fileID), "the owner did nothing")
		assert.Nil(t, findDriveRecentsRow(t, dave, fileID), "another member did nothing")
	})

	t.Run("AnotherPersonEditAddsNothingAndMovesNoRow", func(t *testing.T) {
		fileID := createInDrive(t, eB, driveID, subID, "shared-edit.txt", bettyPerson)
		bettyRow := findDriveRecentsRow(t, betty, fileID)
		require.NotNil(t, bettyRow)

		eA.PUT("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+acmePerson).
			WithHeader("Content-Type", "text/plain").
			WithBytes([]byte("from the owner")).
			Expect().Status(http.StatusOK)

		assert.Equal(t, bettyRow.DocRev, findDriveRecentsRow(t, betty, fileID).DocRev)
		assert.Nil(t, findDriveRecentsRow(t, dave, fileID))

		// For the owner, it is an item of their own instance, drive or not.
		acmeRow := findDriveRecentsRow(t, acme, fileID)
		require.NotNil(t, acmeRow)
		assert.Empty(t, acmeRow.DriveID)
		assert.Empty(t, acmeRow.OwnerDomain)
		assert.Equal(t, "/Team drive/Sub/shared-edit.txt", acmeRow.Path)
	})

	t.Run("ServiceTokenRecordsNothing", func(t *testing.T) {
		// The drive token used on the owner's side is a share-interact code,
		// but the person is checked on the original token of Betty.
		fileID := createInDrive(t, eB, driveID, subID, "by-a-service.txt", bettyServiceToken)
		assert.Nil(t, findDriveRecentsRow(t, betty, fileID))
	})

	t.Run("NestedDriveWhereTheMemberCanWriteComesFirst", func(t *testing.T) {
		// Dave can only read the outer drive: the row is on the nested one,
		// whatever the route.
		for _, route := range []string{nestedID, driveID} {
			fileID := createInDrive(t, eD, route, nestedRootID, "by-dave-"+route+".txt", davePerson)
			row := findDriveRecentsRow(t, dave, fileID)
			require.NotNil(t, row)
			assert.Equal(t, nestedID, row.DriveID)
			assert.Equal(t, "/by-dave-"+route+".txt", row.Path)
		}
	})

	t.Run("NestedDriveWithTheSameRightsGivesTheOutermost", func(t *testing.T) {
		fileID := createInDrive(t, eB, nestedID, nestedRootID, "by-betty-nested.txt", bettyPerson)
		row := findDriveRecentsRow(t, betty, fileID)
		require.NotNil(t, row)
		assert.Equal(t, driveID, row.DriveID)
		assert.Equal(t, "/Nested/by-betty-nested.txt", row.Path)
	})

	t.Run("ShareInteractCodeOfADriveRecords", func(t *testing.T) {
		pinMemberToTheStack(t, acme, betty, driveID, "betty@example.net")
		fileID := createFile(t, eA, subID, "by-code.txt", acmeToken)
		code := getSharedDriveInteractCode(t, acme, driveID, "betty@example.net")

		eA.PUT("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+code).
			WithHeader("Content-Type", "text/plain").
			WithBytes([]byte("from the editor")).
			Expect().Status(http.StatusOK)

		row := findDriveRecentsRow(t, betty, fileID)
		require.NotNil(t, row)
		assert.Equal(t, driveID, row.DriveID)
		assert.Equal(t, acme.Domain, row.OwnerDomain)
		assert.Equal(t, "/Sub/by-code.txt", row.Path)
		assert.Nil(t, findDriveRecentsRow(t, acme, fileID))
	})

	t.Run("ShareInteractCodeOfAClassicSharingRecordsNothing", func(t *testing.T) {
		classicDirID := createRootDirectory(t, eA, "Classic", acmeToken)
		fileID := createFile(t, eA, classicDirID, "classic.txt", acmeToken)
		contact := createContact(t, acme, "Betty", "betty@example.net")
		sharingID := eA.POST("/sharings/").
			WithHeader("Authorization", "Bearer "+acmeToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{"data": {
				"type": "` + consts.Sharings + `",
				"attributes": {
					"description": "a classic sharing",
					"preview_path": "/preview",
					"rules": [{
						"title": "folder",
						"doctype": "` + consts.Files + `",
						"values": ["` + classicDirID + `"],
						"add": "sync",
						"update": "sync",
						"remove": "sync"
					}]
				},
				"relationships": {"recipients": {"data": [
					{"id": "` + contact.ID() + `", "type": "` + contact.DocType() + `"}
				]}}
			}}`)).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()
		pinMemberToTheStack(t, acme, betty, sharingID, "betty@example.net")
		code := getSharedDriveInteractCode(t, acme, sharingID, "betty@example.net")

		eA.PUT("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+code).
			WithHeader("Content-Type", "text/plain").
			WithBytes([]byte("from the editor")).
			Expect().Status(http.StatusOK)

		assert.Nil(t, findDriveRecentsRow(t, betty, fileID))
		assert.Nil(t, findDriveRecentsRow(t, acme, fileID))
	})

	t.Run("ItemThatLeftTheRootRecordsNothing", func(t *testing.T) {
		dirID := createDirectory(t, eA, subID, "moved-meanwhile", acmeToken)
		dir, err := acme.VFS().DirByID(dirID)
		require.NoError(t, err)

		// A doc read before a move of the item to another drive: the drive is
		// computed on the current position, and the path of the doc is not under
		// its root. The full path must never be recorded in its place.
		stale := dir.Clone().(*vfs.DirDoc)
		stale.Fullpath = "/Other drive/moved-meanwhile"

		files.RecordActivityFor(betty, acme, stale, nil, recents.KindEdit)
		assert.Nil(t, findDriveRecentsRow(t, betty, dirID))

		files.RecordActivityFor(betty, acme, dir, nil, recents.KindEdit)
		assert.NotNil(t, findDriveRecentsRow(t, betty, dirID))
	})
}
