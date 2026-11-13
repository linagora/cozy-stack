package notes

import (
	"fmt"
	"testing"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/session"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const noteSchemaPayload = `{
  "data": {
    "type": "io.cozy.notes.documents",
    "attributes": {
      "title": "A recents note",
      "dir_id": "%s",
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
}`

func TestNotesRecordRecents(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	testutils.WithFlag(t, inst, "drive.recents.enabled", true)
	sess, err := session.New(inst, session.LongRun, "")
	require.NoError(t, err)
	_, err = permission.CreateWebappSet(inst, consts.NotesSlug, permission.Set{
		permission.Rule{Type: consts.Files, Verbs: permission.ALL},
	}, "1.0.0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = permission.DestroyWebapp(inst, consts.NotesSlug) })

	ts := setup.GetTestServer("/notes", Routes, func(r *echo.Echo) *echo.Echo {
		r.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				c.Set("session", sess)
				return next(c)
			}
		})
		return r
	})
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	e := testutils.CreateTestClient(t, ts.URL)
	notesToken := inst.BuildAppToken(consts.NotesSlug, sess.ID())
	// An empty dirID is the default folder of the notes.
	createNote := func(token, dirID string) string {
		return e.POST("/notes").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(fmt.Sprintf(noteSchemaPayload, dirID))).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()
	}

	var noteID string
	t.Run("CreateRecordsAnEditWithMetadata", func(t *testing.T) {
		noteID = createNote(notesToken, "")

		row := testutils.FindRecentsRow(t, inst, noteID)
		require.NotNil(t, row)
		assert.Contains(t, row.Name, "A recents note")
		assert.NotEmpty(t, row.LastEditedAt)
	})

	t.Run("GetRecordsAnOpen", func(t *testing.T) {
		e.GET("/notes/"+noteID).
			WithHeader("Authorization", "Bearer "+notesToken).
			Expect().Status(200)

		row := testutils.FindRecentsRow(t, inst, noteID)
		require.NotNil(t, row)
		assert.NotEmpty(t, row.LastViewedAt)
	})

	t.Run("PatchRecordsAnEdit", func(t *testing.T) {
		// A note created by a service has no row before the person edits it.
		id := createNote(inst.BuildAppToken(consts.NotesSlug, ""), "")
		require.Nil(t, testutils.FindRecentsRow(t, inst, id))

		e.PATCH("/notes/"+id).
			WithHeader("Authorization", "Bearer "+notesToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("If-Match", "0").
			WithBytes([]byte(`{"data": [{
				"type": "io.cozy.notes.steps",
				"attributes": {
					"sessionID": "543781490137",
					"stepType": "replace",
					"from": 1,
					"to": 1,
					"slice": {"content": [{"type": "text", "text": "H"}]}
				}
			}]}`)).
			Expect().Status(200)

		row := testutils.FindRecentsRow(t, inst, id)
		require.NotNil(t, row)
		assert.NotEmpty(t, row.LastEditedAt)
	})

	t.Run("ShareCodeRecordsAnOpenForTheMember", func(t *testing.T) {
		bob := testutils.NewSetup(t, t.Name()+"_bob").GetTestInstance()
		testutils.WithFlag(t, bob, "drive.recents.enabled", true)
		dir, err := vfs.Mkdir(inst.VFS(), "/Shared notes", nil)
		require.NoError(t, err)
		s := &sharing.Sharing{
			Active:        true,
			Owner:         true,
			Drive:         true,
			DriveRootType: sharing.DriveRootTypeDirectory,
			Members: []sharing.Member{
				{Status: sharing.MemberStatusOwner, Instance: "http://" + inst.Domain},
				{Status: sharing.MemberStatusReady, Email: "bob@example.net", Instance: "http://" + bob.Domain},
			},
			Rules: []sharing.Rule{{Title: "Shared notes", DocType: consts.Files, Values: []string{dir.ID()}}},
		}
		require.NoError(t, couchdb.CreateDoc(inst, s))
		require.NoError(t, s.AddReferenceForSharing(inst, &s.Rules[0]))
		interact, err := s.GetInteractCode(inst, &s.Members[1], 1)
		require.NoError(t, err)
		preview, err := s.CreatePreviewPermissions(inst)
		require.NoError(t, err)

		for name, code := range map[string]string{
			"share-interact": interact,
			"share-preview":  preview.Codes["bob@example.net"],
		} {
			id := createNote(notesToken, dir.ID())
			e.GET("/notes/"+id).
				WithHeader("Authorization", "Bearer "+code).
				Expect().Status(200)

			row := testutils.FindRecentsRow(t, bob, id)
			require.NotNil(t, row, name)
			assert.Equal(t, s.SID, row.DriveID, name)
			assert.NotEmpty(t, row.LastViewedAt, name)
		}
	})
}
