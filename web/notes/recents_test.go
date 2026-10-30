package notes

import (
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/session"
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

func notesRecentsRows(t *testing.T, inst *instance.Instance) []recents.FileDirActivity {
	t.Helper()
	var docs []recents.FileDirActivity
	require.NoError(t, couchdb.GetAllDocs(inst, consts.FilesRecents, &couchdb.AllDocsRequest{}, &docs))
	return docs
}

func findNoteRecentsRow(t *testing.T, inst *instance.Instance, fileID string) *recents.FileDirActivity {
	t.Helper()
	key := recents.ResourceKey(inst, fileID)
	rows := notesRecentsRows(t, inst)
	for i := range rows {
		if rows[i].DocID == key {
			return &rows[i]
		}
	}
	return nil
}

// ageNoteRecentsRow moves the dates of a row two minutes back, past the
// minimal rewrite interval, so that the next activity is visible as a moved
// date.
func ageNoteRecentsRow(t *testing.T, inst *instance.Instance, row *recents.FileDirActivity) *recents.FileDirActivity {
	t.Helper()
	aged := *row
	for _, stamp := range []*string{&aged.LastEditedAt, &aged.LastViewedAt, &aged.LastActivityAt} {
		if *stamp == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, *stamp)
		require.NoError(t, err)
		*stamp = at.Add(-2 * time.Minute).Format(time.RFC3339)
	}
	require.NoError(t, couchdb.UpdateDoc(inst, &aged))
	return &aged
}

func TestNotesRecents(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	config.GetConfig().Fs.URL = &url.URL{
		Scheme: "file",
		Host:   "localhost",
		Path:   t.TempDir(),
	}

	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	testutils.WithFlag(t, inst, "drive.recents.enabled", true)
	sess, err := session.New(inst, session.LongRun, "")
	require.NoError(t, err)

	_, err = permission.CreateWebappSet(inst, "notes", permission.Set{
		permission.Rule{Type: consts.Files, Verbs: permission.ALL},
	}, "1.0.0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = permission.DestroyWebapp(inst, "notes") })

	handler := echo.New()
	handler.HTTPErrorHandler = errors.ErrorHandler
	group := handler.Group("/notes", func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set("instance", inst)
			c.Set("session", sess)
			return next(c)
		}
	})
	Routes(group)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	e := testutils.CreateTestClient(t, ts.URL)
	token := inst.BuildAppToken("notes", sess.ID())

	// The note creation is an edit: its row is the baseline the next requests
	// must (not) move.
	noteID := e.POST("/notes").
		WithHeader("Authorization", "Bearer "+token).
		WithHeader("Content-Type", "application/json").
		WithBytes([]byte(`{
      "data": {
        "type": "io.cozy.notes.documents",
        "attributes": {
          "title": "Recents note",
          "schema": {
            "nodes": [
              ["doc", { "content": "block+" }],
              ["paragraph", { "content": "inline*", "group": "block" }],
              ["blockquote", { "content": "block+", "group": "block" }],
              ["horizontal_rule", { "group": "block" }],
              [
                "heading",
                {
                  "content": "inline*",
                  "group": "block",
                  "attrs": { "level": { "default": 1 } }
                }
              ],
              ["code_block", { "content": "text*", "marks": "", "group": "block" }],
              ["text", { "group": "inline" }],
              [
                "image",
                {
                  "group": "inline",
                  "inline": true,
                  "attrs": { "alt": {}, "src": {}, "title": {} }
                }
              ],
              ["hard_break", { "group": "inline", "inline": true }],
              [
                "ordered_list",
                {
                  "content": "list_item+",
                  "group": "block",
                  "attrs": { "order": { "default": 1 } }
                }
              ],
              ["bullet_list", { "content": "list_item+", "group": "block" }],
              ["list_item", { "content": "paragraph block*" }]
            ],
            "marks": [
              ["link", { "attrs": { "href": {}, "title": {} }, "inclusive": false }],
              ["em", {}],
              ["strong", {}],
              ["code", {}]
            ],
            "topNode": "doc"
          }
        }
      }
    }`)).
		Expect().Status(201).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object().Path("$.data.id").String().NotEmpty().Raw()

	created := findNoteRecentsRow(t, inst, noteID)
	require.NotNil(t, created, "note creation is an edit")
	require.NotEmpty(t, created.LastEditedAt)

	e.PUT("/notes/"+noteID+"/title").
		WithHeader("Authorization", "Bearer "+token).
		WithHeader("Content-Type", "application/vnd.api+json").
		WithBytes([]byte(`{
      "data": {
        "type": "io.cozy.notes.documents",
        "attributes": {
          "sessionID": "543781490137",
          "title": "Renamed note"
        }
      }
    }`)).
		Expect().Status(200)

	afterTitle := findNoteRecentsRow(t, inst, noteID)
	require.NotNil(t, afterTitle)
	assert.Equal(t, created.LastEditedAt, afterTitle.LastEditedAt,
		"a title change is not a content edit")

	e.PUT("/notes/"+noteID+"/telepointer").
		WithHeader("Authorization", "Bearer "+token).
		WithHeader("Content-Type", "application/json").
		WithBytes([]byte(`{
      "data": {
        "type": "io.cozy.notes.telepointers",
        "attributes": {
          "sessionID": "543781490137",
          "anchor": 7,
          "head": 12,
          "type": "textSelection"
        }
      }
    }`)).
		Expect().Status(204)

	afterPointer := findNoteRecentsRow(t, inst, noteID)
	require.NotNil(t, afterPointer)
	assert.Equal(t, created.LastEditedAt, afterPointer.LastEditedAt,
		"a telepointer move is not a content edit")

	// Activities closer than the minimal rewrite interval do not move the
	// row: age it so that a recorded edit is visible as a moved date.
	created = ageNoteRecentsRow(t, inst, afterPointer)

	e.PATCH("/notes/"+noteID).
		WithHeader("Authorization", "Bearer "+token).
		WithHeader("Content-Type", "application/vnd.api+json").
		WithHeader("If-Match", "0").
		WithBytes([]byte(`{
      "data": [{
        "type": "io.cozy.notes.steps",
        "attributes": {
          "sessionID": "543781490137",
          "stepType": "replace",
          "from": 1,
          "to": 1,
          "slice": {
            "content": [{ "type": "text", "text": "H" }]
          }
        }
      }]
    }`)).
		Expect().Status(200)

	afterPatch := findNoteRecentsRow(t, inst, noteID)
	require.NotNil(t, afterPatch)
	assert.Greater(t, afterPatch.LastEditedAt, created.LastEditedAt,
		"applying steps is a content edit: last_edited_at must move")

	// Another app reading the note does not open it.
	_, err = permission.CreateWebappSet(inst, "drive", permission.Set{
		permission.Rule{Type: consts.Files, Verbs: permission.ALL},
	}, "1.0.0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = permission.DestroyWebapp(inst, "drive") })
	e.GET("/notes/"+noteID).
		WithHeader("Authorization", "Bearer "+inst.BuildAppToken("drive", sess.ID())).
		Expect().Status(200)
	assert.Empty(t, findNoteRecentsRow(t, inst, noteID).LastViewedAt,
		"a read of the note by another app is not an open")

	// The notes editor loads the whole note when it opens it.
	e.GET("/notes/"+noteID).
		WithHeader("Authorization", "Bearer "+token).
		Expect().Status(200)
	opened := findNoteRecentsRow(t, inst, noteID)
	require.NotNil(t, opened)
	assert.NotEmpty(t, opened.LastViewedAt, "loading the note in the editor is an open")
	assert.Equal(t, afterPatch.LastEditedAt, opened.LastEditedAt)
}
