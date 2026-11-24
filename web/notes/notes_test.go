package notes

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/note"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/cozy/cozy-stack/web/files"
	webRealtime "github.com/cozy/cozy-stack/web/realtime"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNotes(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	var noteID, otherNoteID string
	var version int64

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	_, token := setup.GetTestClient(consts.Files)

	ts := setup.GetTestServerMultipleRoutes(map[string]func(*echo.Group){
		"/files":    files.Routes,
		"/notes":    Routes,
		"/realtime": webRealtime.Routes,
	})
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	t.Run("CreateNote", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/notes").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
        "data": {
          "type": "io.cozy.notes.documents",
          "attributes": {
            "title": "A super note",
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
			Object()

		assertInitialNote(t, obj)

		noteID = obj.Path("$.data.id").String().NotEmpty().Raw()
	})

	t.Run("GetNote", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.GET("/notes/"+noteID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		assertInitialNote(t, obj)
	})

	t.Run("OpenNote", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.GET("/notes/"+noteID+"/open").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.HasValue("type", consts.NotesURL)
		data.HasValue("id", noteID)

		attrs := data.Value("attributes").Object()
		attrs.HasValue("note_id", noteID)
		attrs.HasValue("subdomain", "nested")
		attrs.HasValue("protocol", "https")
		attrs.HasValue("instance", inst.Domain)
		attrs.Value("public_name").String().NotEmpty()
	})

	t.Run("ChangeTitleAndSync", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.PUT("/notes/"+noteID+"/title").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{
        "data": {
          "type": "io.cozy.notes.documents",
          "attributes": {
            "sessionID": "543781490137",
            "title": "A new title"
          }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.HasValue("type", "io.cozy.files")
		data.HasValue("id", noteID)

		attrs := data.Value("attributes").Object()
		meta := attrs.Value("metadata").Object()

		meta.HasValue("title", "A new title")
		meta.HasValue("version", 0)
		meta.Value("schema").Object().NotEmpty()
		meta.Value("content").Object().NotEmpty()

		// The change was only made in cache, but we have to force persisting the
		// change to the VFS to check that renaming the file works.
		e.POST("/notes/"+noteID+"/sync").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(204)

		obj = e.GET("/notes/"+noteID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data = obj.Value("data").Object()
		data.HasValue("type", "io.cozy.files")
		data.HasValue("id", noteID)

		attrs = data.Value("attributes").Object()
		attrs.HasValue("name", "A new title.cozy-note")

		meta = attrs.Value("metadata").Object()
		meta.HasValue("title", "A new title")
		meta.HasValue("version", 0)
		meta.Value("schema").Object().NotEmpty()
		meta.Value("content").Object().NotEmpty()
	})

	t.Run("ListNotes", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Change the title
		e.PUT("/notes/"+noteID+"/title").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{
        "data": {
          "type": "io.cozy.notes.documents",
          "attributes": {
            "sessionID": "543781490137",
            "title": "A title in cache"
          }
        }
      }`)).
			Expect().Status(200)

		// The title has been changed in cache, but we don't wait that the file has been renamed
		obj := e.GET("/notes").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Array()
		data.Length().IsEqual(1)

		doc := data.Value(0).Object()
		doc.HasValue("type", "io.cozy.files")
		doc.HasValue("id", noteID)

		attrs := doc.Value("attributes").Object()
		attrs.HasValue("name", "A new title.cozy-note")
		attrs.Value("path").String().HasSuffix("/A new title.cozy-note")
		attrs.HasValue("mime", "text/vnd.cozy.note+markdown")

		meta := attrs.Value("metadata").Object()
		meta.HasValue("title", "A title in cache")
		meta.HasValue("version", 0)
		meta.Value("schema").Object().NotEmpty()
		meta.Value("content").Object().NotEmpty()
	})

	t.Run("PatchNote", func(t *testing.T) {
		body := []byte(`{
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
        }, {
          "type": "io.cozy.notes.steps",
          "attributes": {
            "sessionID": "543781490137",
            "stepType": "replace",
            "from": 2,
            "to": 2,
            "slice": {
              "content": [{ "type": "text", "text": "ello" }]
            }
          }
        }]
      }`)

		t.Run("Success", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.PATCH("/notes/"+noteID).
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/vnd.api+json").
				WithHeader("If-Match", "0").
				WithBytes(body).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			data := obj.Value("data").Object()
			data.HasValue("type", "io.cozy.files")
			data.HasValue("id", noteID)

			attrs := data.Value("attributes").Object()
			meta := attrs.Value("metadata").Object()

			version = int64(meta.Value("version").Number().Gt(0).Raw())
			meta.Value("schema").Object().NotEmpty()
			meta.Value("content").Object().NotEmpty()
		})

		t.Run("WithInvalidIfMatchHeader", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.PATCH("/notes/"+noteID).
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/vnd.api+json").
				WithHeader("If-Match", "0").
				WithBytes(body).
				Expect().Status(409)
		})
	})

	t.Run("GetSteps", func(t *testing.T) {
		var lastVersion int

		body := []byte(`{
      "data": [{
        "type": "io.cozy.notes.steps",
        "attributes": {
          "sessionID": "543781490137",
          "stepType": "replace",
          "from": 6,
          "to": 6,
          "slice": {
            "content": [{ "type": "text", "text": " " }]
          }
        }
      }, {
        "type": "io.cozy.notes.steps",
        "attributes": {
          "sessionID": "543781490137",
          "stepType": "replace",
          "from": 7,
          "to": 7,
          "slice": {
            "content": [{ "type": "text", "text": "world" }]
          }
        }
      }]
    }`)

		t.Run("Success", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.PATCH("/notes/"+noteID).
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/vnd.api+json").
				WithHeader("If-Match", strconv.Itoa(int(version))).
				WithBytes(body).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			lastVersion = int(obj.Path("$.data.attributes.metadata.version").Number().Gt(0).Raw())
		})

		t.Run("GetStepsFromCurrentVersion", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.GET("/notes/"+noteID+"/steps").
				WithQuery("Version", int(version)).
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/vnd.api+json").
				WithHeader("If-Match", strconv.Itoa(int(version))).
				WithBytes(body).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			obj.Path("$.meta.count").Number().IsEqual(2)
			obj.Path("$.data").Array().Length().IsEqual(2)

			first := obj.Path("$.data[0]").Object()
			first.Value("id").String().NotEmpty()

			attrs := first.Value("attributes").Object()
			attrs.HasValue("sessionID", "543781490137")
			attrs.HasValue("stepType", "replace")
			attrs.HasValue("from", 6)
			attrs.HasValue("to", 6)
			attrs.Value("version").Number()

			second := obj.Path("$.data[1]").Object()
			second.Value("id").String().NotEmpty()

			attrs = second.Value("attributes").Object()
			attrs.HasValue("sessionID", "543781490137")
			attrs.HasValue("stepType", "replace")
			attrs.HasValue("from", 7)
			attrs.HasValue("to", 7)
			attrs.HasValue("version", lastVersion)
		})

		t.Run("GetStepsFromLastVersion", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.GET("/notes/"+noteID+"/steps").
				WithQuery("Version", lastVersion).
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/vnd.api+json").
				WithHeader("If-Match", strconv.Itoa(int(version))).
				WithBytes(body).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			obj.Path("$.meta.count").Number().IsEqual(0)
			obj.Path("$.data").Array().IsEmpty()

			version = int64(lastVersion)
		})
	})

	t.Run("PutSchema", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.PUT("/notes/"+noteID+"/schema").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
        "data": {
          "type": "io.cozy.notes.documents",
          "attributes": {
            "schema": {
              "nodes": [
                ["doc", { "content": "block+" }],
                [
                  "panel",
                  {
                    "content": "(paragraph | heading | bullet_list | ordered_list)+",
                    "group": "block",
                    "attrs": { "panelType": { "default": "info" } }
                  }
                ],
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
              "version": 2,
              "topNode": "doc"
            }
          }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.HasValue("type", "io.cozy.files")
		data.HasValue("id", noteID)

		schema := obj.Path("$.data.attributes.metadata.schema").Object()
		schema.HasValue("version", 2)
		schema.Path("$.nodes[1][0]").IsEqual("panel")

		// TODO: add an explanation why we need this sleep period
		time.Sleep(1 * time.Second)

		e.GET("/notes/"+noteID+"/steps").
			WithQuery("Version", version).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(412)

		version = int64(obj.Path("$.data.attributes.metadata.version").Number().Raw())
	})

	t.Run("PutSchema only purges steps for the updated note", func(t *testing.T) {
		schemaNote, err := note.Create(inst, &note.Document{
			Title:      "Schema note",
			SchemaSpec: note.DefaultSchemaSpecs(),
		})
		require.NoError(t, err)

		stepsNote, err := note.Create(inst, &note.Document{
			Title:      "Steps note",
			SchemaSpec: note.DefaultSchemaSpecs(),
		})
		require.NoError(t, err)

		stepsFile, err := inst.VFS().FileByID(stepsNote.ID())
		require.NoError(t, err)

		steps := []note.Step{{
			"sessionID": "regression",
			"stepType":  "replace",
			"from":      1,
			"to":        1,
			"slice": map[string]interface{}{
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "A"},
				},
			},
		}}
		_, err = note.ApplySteps(inst, stepsFile, "0", steps)
		require.NoError(t, err)

		existing, err := note.GetSteps(inst, stepsNote.ID(), 0)
		require.NoError(t, err)
		require.Len(t, existing, 1)

		schemaFile, err := inst.VFS().FileByID(schemaNote.ID())
		require.NoError(t, err)

		_, err = note.UpdateSchema(inst, schemaFile, note.DefaultSchemaSpecs())
		require.NoError(t, err)

		time.Sleep(1 * time.Second)

		remaining, err := note.GetSteps(inst, stepsNote.ID(), 0)
		require.NoError(t, err)
		require.Len(t, remaining, 1)
	})

	t.Run("PutTelepointer", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		wg := sync.WaitGroup{}
		wg.Add(1)
		go func() {
			sub := realtime.GetHub().Subscriber(inst)
			sub.Subscribe(consts.NotesEvents)

			// Suscribtion ok, unlock the first wait
			wg.Done()

			e := <-sub.Channel
			assert.Equal(t, "UPDATED", e.Verb)
			assert.Equal(t, noteID, e.Doc.ID())
			doc, ok := e.Doc.(note.Event)
			assert.True(t, ok)
			assert.Equal(t, consts.NotesTelepointers, doc["doctype"])
			assert.Equal(t, "543781490137", doc["sessionID"])
			assert.Equal(t, "textSelection", doc["type"])
			assert.EqualValues(t, 7, doc["anchor"])
			assert.EqualValues(t, 12, doc["head"])

			// Event received and validated, unlock the second wait.
			wg.Done()
		}()

		// Wait that the goroutine has subscribed to the realtime
		wg.Wait()

		wg.Add(1)
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

		// Wait that the goroutine has received the telepointer update
		wg.Wait()
	})

	t.Run("NoteMarkdown", func(t *testing.T) {
		// Force the changes to the VFS
		err := note.Update(inst, noteID)
		assert.NoError(t, err)
		doc, err := inst.VFS().FileByID(noteID)
		assert.NoError(t, err)
		file, err := inst.VFS().OpenFile(doc)
		assert.NoError(t, err)
		defer file.Close()
		buf, err := io.ReadAll(file)
		assert.NoError(t, err)
		assert.Equal(t, "Hello world", string(buf))
	})

	t.Run("NoteRealtime", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		ws := e.GET("/realtime/").
			WithWebsocketUpgrade().
			Expect().Status(http.StatusSwitchingProtocols).
			Websocket()
		defer ws.Disconnect()

		ws.WriteText(fmt.Sprintf(`{"method": "AUTH", "payload": "%s"}`, token))

		ws.WriteText(`{"method": "SUBSCRIBE", "payload": { "type": "io.cozy.notes.events", "id": "` + noteID + `" }}`)

		// To check that the realtime has made the subscription, we send a fake
		// message and wait for its response.
		ws.WriteText(`{"method": "PING"}`).
			Expect().TextMessage().
			JSON()

		pointer := note.Event{
			"sessionID": "543781490137",
			"anchor":    7,
			"head":      12,
			"type":      "textSelection",
		}
		pointer.SetID(noteID)
		err := note.PutTelepointer(inst, pointer)
		assert.NoError(t, err)

		obj := ws.Expect().TextMessage().
			JSON().Object()

		obj.HasValue("event", "UPDATED")
		payload := obj.Value("payload").Object()
		payload.HasValue("id", noteID)
		payload.HasValue("type", "io.cozy.notes.events")

		doc := payload.Value("doc").Object()
		doc.HasValue("doctype", "io.cozy.notes.telepointers")
		doc.HasValue("sessionID", "543781490137")
		doc.HasValue("anchor", 7)
		doc.HasValue("head", 12)
		doc.HasValue("type", "textSelection")

		file, err := inst.VFS().FileByID(noteID)
		require.NoError(t, err)
		file, err = note.UpdateTitle(inst, file, "A very new title", "543781490137")
		require.NoError(t, err)

		obj = ws.Expect().TextMessage().
			JSON().Object()

		obj.HasValue("event", "UPDATED")
		payload = obj.Value("payload").Object()
		payload.HasValue("id", noteID)
		payload.HasValue("type", "io.cozy.notes.events")

		doc = payload.Value("doc").Object()
		doc.HasValue("doctype", "io.cozy.notes.documents")
		doc.HasValue("title", "A very new title")
		doc.HasValue("sessionID", "543781490137")

		slice := map[string]interface{}{
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": "X"},
			},
		}
		steps := []note.Step{
			{"sessionID": "543781490137", "stepType": "replace", "from": 2, "to": 2, "slice": slice},
			{"sessionID": "543781490137", "stepType": "replace", "from": 3, "to": 3, "slice": slice},
		}
		file, err = note.ApplySteps(inst, file, fmt.Sprintf("%d", version), steps)
		require.NoError(t, err)

		obj = ws.Expect().TextMessage().
			JSON().Object()

		obj.HasValue("event", "UPDATED")
		payload = obj.Value("payload").Object()
		payload.HasValue("id", noteID)
		payload.HasValue("type", "io.cozy.notes.events")

		doc4 := payload.Value("doc").Object()

		obj = ws.Expect().TextMessage().
			JSON().Object()

		obj.HasValue("event", "UPDATED")
		payload = obj.Value("payload").Object()
		payload.HasValue("id", noteID)
		payload.HasValue("type", "io.cozy.notes.events")
		doc5 := payload.Value("doc").Object()

		// // In some cases, the steps can be received in the bad order because of the
		// // concurrency between the goroutines in the realtime hub.
		if doc4.Value("version").Number().Raw() > doc5.Value("version").Number().Raw() {
			doc4, doc5 = doc5, doc4
		}

		doc4.HasValue("doctype", "io.cozy.notes.steps")
		doc4.HasValue("sessionID", "543781490137")
		doc4.HasValue("stepType", "replace")
		doc4.HasValue("from", 2)
		doc4.HasValue("to", 2)
		vers4 := int(doc4.Value("version").Number().Gt(0).Raw())

		doc5.HasValue("doctype", "io.cozy.notes.steps")
		doc5.HasValue("sessionID", "543781490137")
		doc5.HasValue("stepType", "replace")
		doc5.HasValue("from", 3)
		doc5.HasValue("to", 3)
		vers5 := int(doc5.Value("version").Number().
			NotEqual(0).
			NotEqual(vers4).
			Raw())

		assert.EqualValues(t, file.Metadata["version"], vers5)
	})

	t.Run("CreateNote with a content", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/notes").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
        "data": {
          "type": "io.cozy.notes.documents",
          "attributes": {
            "title": "A note with some content",
            "schema": {
              "nodes": [
                ["doc", { "content": "block+" }],
                ["paragraph", { "content": "inline*", "group": "block" }],
                ["text", { "group": "inline" }],
                ["bullet_list", { "content": "list_item+", "group": "block" }],
                ["list_item", { "content": "paragraph block*" }]
              ],
              "marks": [
                ["em", {}],
                ["strong", {}]
              ],
              "topNode": "doc"
            },
            "content": {
              "content": [
                {
                  "content": [{ "text": "Hello world", "type": "text" }],
                  "type": "paragraph"
                }
              ],
              "type": "doc"
            }
          }
        }
      }`)).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()

		data.HasValue("type", "io.cozy.files")
		otherNoteID = data.Value("id").String().NotEmpty().Raw()

		attrs := data.Value("attributes").Object()
		attrs.HasValue("type", "file")
		attrs.HasValue("name", "A note with some content.cozy-note")
		attrs.HasValue("mime", "text/vnd.cozy.note+markdown")

		meta := attrs.Value("metadata").Object()
		meta.HasValue("title", "A note with some content")
		meta.HasValue("version", 0)
		meta.Value("schema").Object().NotEmpty()

		expected := map[string]interface{}{
			"content": []interface{}{
				map[string]interface{}{
					"content": []interface{}{
						map[string]interface{}{"text": "Hello world", "type": "text"},
					},
					"type": "paragraph",
				},
			},
			"type": "doc",
		}
		meta.Value("content").Object().IsEqual(expected)
	})

	t.Run("UploadImage", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		rawFile, err := os.ReadFile("../../tests/fixtures/wet-cozy_20160910__M4Dz.jpg")
		require.NoError(t, err)

		for i := 0; i < 3; i++ {
			obj := e.POST("/notes/"+noteID+"/images").
				WithQuery("Name", "wet.jpg").
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "image/jpeg").
				WithBytes(rawFile).
				Expect().Status(201).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			data := obj.Value("data").Object()
			data.HasValue("type", consts.NotesImages)
			data.Value("id").String().NotEmpty()
			data.Value("meta").Object().NotEmpty()

			attrs := data.Value("attributes").Object()
			if i == 0 {
				attrs.HasValue("name", "wet.jpg")
			} else {
				attrs.HasValue("name", fmt.Sprintf("wet (%d).jpg", i+1))
			}

			attrs.Value("cozyMetadata").Object().NotEmpty()
			attrs.HasValue("mime", "image/jpeg")
			attrs.HasValue("width", 440)
			attrs.HasValue("height", 294)

			data.Path("$.links.self").String().NotEmpty()
		}
	})

	t.Run("CopyImage", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		rawFile, err := os.ReadFile("../../tests/fixtures/wet-cozy_20160910__M4Dz.jpg")
		require.NoError(t, err)

		obj := e.POST("/notes/"+noteID+"/images").
			WithQuery("Name", "tobecopied.jpg").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "image/jpeg").
			WithBytes(rawFile).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		imgID := data.Value("id").String().NotEmpty().Raw()

		obj = e.POST("/notes/"+imgID+"/copy").
			WithQuery("To", otherNoteID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data = obj.Value("data").Object()
		data.HasValue("type", consts.NotesImages)
		data.Value("id").String().NotEmpty().NotEqual(imgID)
		data.Value("meta").Object().NotEmpty()

		attrs := data.Value("attributes").Object()
		attrs.HasValue("name", "tobecopied.jpg")

		attrs.Value("cozyMetadata").Object().NotEmpty()
		attrs.HasValue("mime", "image/jpeg")
		attrs.HasValue("width", 440)
		attrs.HasValue("height", 294)

		data.Path("$.links.self").String().NotEmpty()
	})

	t.Run("GetImage", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		rawFile, err := os.ReadFile("../../tests/fixtures/wet-cozy_20160910__M4Dz.jpg")
		require.NoError(t, err)

		obj := e.POST("/notes/"+noteID+"/images").
			WithQuery("Name", "wet.jpg").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "image/jpeg").
			WithBytes(rawFile).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.HasValue("type", consts.NotesImages)
		data.Value("id").String().NotEmpty()
		data.Value("meta").Object().NotEmpty()

		link := data.Path("$.links.self").String().NotEmpty().Raw()

		e.GET(link).
			Expect().Status(200).
			Body().IsEqual(string(rawFile))

		obj = e.GET("/files/"+noteID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		image := obj.Value("included").Array().
			Find(func(_ int, value *httpexpect.Value) bool {
				value.Object().NotHasValue("type", consts.FilesVersions)
				return true
			}).
			Object()

		image.HasValue("type", consts.NotesImages)
		image.Value("id").String().NotEmpty()
		image.Value("meta").Object().NotEmpty()

		attrs := image.Value("attributes").Object()
		attrs.Value("name").String().NotEmpty()
		attrs.Value("cozyMetadata").Object().NotEmpty()
		attrs.HasValue("mime", "image/jpeg")

		data.Path("$.links.self").String().NotEmpty()
	})

	t.Run("ImportNotes", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/files/io.cozy.files.root-dir").
			WithQuery("Type", "file").
			WithQuery("Name", "An imported note.cozy-note").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "text/plain").
			WithBytes([]byte(`
        # Title

        Text with **bold** and [underlined]{.underlined}.
      `)).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.HasValue("type", "io.cozy.files")
		fileID := data.Value("id").String().NotEmpty().Raw()

		attrs := data.Value("attributes").Object()
		attrs.HasValue("type", "file")
		attrs.HasValue("name", "An imported note.cozy-note")
		attrs.HasValue("mime", "text/vnd.cozy.note+markdown")

		meta := attrs.Value("metadata").Object()
		meta.HasValue("title", "An imported note")
		meta.Value("schema").Object().NotEmpty()
		meta.Value("content").Object().NotEmpty()

		obj = e.GET("/notes/"+fileID+"/open").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data = obj.Value("data").Object()
		data.HasValue("id", fileID)
		data.Path("$.attributes.instance").IsEqual(inst.Domain)
	})

	t.Run("ImportNoteBiggerThanTheMarkdownCap", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)
		body := strings.Repeat("a", note.MaxMarkdownSize+1<<20)

		e.POST("/files/io.cozy.files.root-dir").
			WithQuery("Type", "file").
			WithQuery("Name", "A big imported note.cozy-note").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "text/plain").
			WithBytes([]byte(body)).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Path("$.data.attributes.size").IsEqual(fmt.Sprint(len(body)))
	})

	t.Run("CopyNoteWithAnImage", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		toImport, err := os.ReadFile("../../tests/fixtures/note-with-an-image.cozy-note")
		require.NoError(t, err)

		obj := e.POST("/files/io.cozy.files.root-dir").
			WithQuery("Type", "file").
			WithQuery("Name", "Note with an image.cozy-note").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "text/plain").
			WithBytes(toImport).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.HasValue("type", "io.cozy.files")
		srcID := data.Value("id").String().NotEmpty().Raw()

		obj = e.GET("/files/"+srcID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		image := obj.Value("included").Array().
			Find(func(_ int, value *httpexpect.Value) bool {
				value.Object().NotHasValue("type", consts.FilesVersions)
				return true
			}).
			Object()

		image.HasValue("type", consts.NotesImages)
		image.Value("id").String().NotEmpty()
		image.Value("meta").Object().NotEmpty()

		attrs := image.Value("attributes").Object()
		attrs.Value("name").String().NotEmpty()
		attrs.Value("cozyMetadata").Object().NotEmpty()
		attrs.HasValue("mime", "image/png")

		link := data.Path("$.links.self").String().NotEmpty().Raw()

		e.GET(link).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		obj = e.POST("/files/"+srcID+"/copy").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data = obj.Value("data").Object()
		data.HasValue("type", "io.cozy.files")
		dstID := data.Value("id").String().NotEmpty().Raw()

		obj = e.GET("/files/"+dstID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		image = obj.Value("included").Array().
			Find(func(_ int, value *httpexpect.Value) bool {
				value.Object().NotHasValue("type", consts.FilesVersions)
				return true
			}).
			Object()

		image.HasValue("type", consts.NotesImages)
		image.Value("id").String().NotEmpty()
		image.Value("meta").Object().NotEmpty()

		attrs = image.Value("attributes").Object()
		attrs.Value("name").String().NotEmpty()
		attrs.Value("cozyMetadata").Object().NotEmpty()
		attrs.HasValue("mime", "image/png")

		link = data.Path("$.links.self").String().NotEmpty().Raw()

		e.GET(link).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)
	})
}

func assertInitialNote(t *testing.T, obj *httpexpect.Object) {
	data := obj.Value("data").Object()

	data.HasValue("type", "io.cozy.files")
	data.Value("id").String().NotEmpty()

	attrs := data.Value("attributes").Object()
	attrs.HasValue("type", "file")
	attrs.HasValue("name", "A super note.cozy-note")
	attrs.HasValue("mime", "text/vnd.cozy.note+markdown")

	fcm := attrs.Value("cozyMetadata").Object()
	fcm.Value("createdAt").String().AsDateTime(time.RFC3339)
	fcm.Value("createdOn").String().NotEmpty()

	meta := attrs.Value("metadata").Object()
	meta.HasValue("title", "A super note")
	meta.HasValue("version", 0)
	meta.Value("schema").Object().NotEmpty()
	meta.Value("content").Object().NotEmpty()
}
