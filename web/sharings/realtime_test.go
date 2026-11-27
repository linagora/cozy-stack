package sharings

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForwardEventsStopsWhenClientIsGone(t *testing.T) {
	ds := &realtime.Subscriber{Channel: make(realtime.EventsChan, 1)}
	ch := make(chan *wsResponse)
	ctx, cancel := context.WithCancel(context.Background())

	ds.Channel <- &realtime.Event{
		Verb: realtime.EventUpdate,
		Doc:  &realtime.JSONDoc{Type: consts.Files, M: map[string]interface{}{"_id": "foo"}},
	}

	done := make(chan struct{})
	go func() {
		forwardEvents(ctx, ds, ch, func(e *realtime.Event) *realtime.Event { return e })
		close(done)
	}()

	for len(ds.Channel) > 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("forwardEvents kept running after the websocket writer stopped")
	}
}

func TestDriveRealtimePathsAreRelativeToTheRoot(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	root, err := vfs.MkdirAll(fs, "/Private/Clients/Team")
	require.NoError(t, err)
	sub, err := vfs.Mkdir(fs, "/Private/Clients/Team/Meetings", nil)
	require.NoError(t, err)
	_, err = vfs.MkdirAll(fs, "/Private/Other")
	require.NoError(t, err)

	s := &sharing.Sharing{
		Drive: true,
		Rules: []sharing.Rule{{DocType: consts.Files, Values: []string{root.ID()}}},
	}

	ds := &realtime.Subscriber{Channel: make(realtime.EventsChan, 1)}
	ch := make(chan *wsResponse)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go filterMapEvents(ctx, ds, ch, inst, s)

	send := func(e *realtime.Event) (*wsResponse, string) {
		t.Helper()
		ds.Channel <- e
		select {
		case res := <-ch:
			body, err := json.Marshal(res)
			require.NoError(t, err)
			assert.NotContains(t, string(body), "Private")
			assert.NotContains(t, string(body), "Clients")
			assert.NotContains(t, string(body), "Other")
			return res, string(body)
		case <-time.After(5 * time.Second):
			require.FailNow(t, "no event was forwarded")
			return nil, ""
		}
	}
	docPath := func(res *wsResponse, key string) interface{} {
		t.Helper()
		var m map[string]interface{}
		body, err := json.Marshal(res.Payload.Doc)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &m))
		return m[key]
	}

	t.Run("DirDoc", func(t *testing.T) {
		res, _ := send(&realtime.Event{Verb: realtime.EventUpdate, Doc: sub.Clone().(*vfs.DirDoc)})
		assert.Equal(t, realtime.EventUpdate, res.Event)
		assert.Equal(t, "/Team/Meetings", docPath(res, "path"))
	})

	t.Run("RootRenamedAbove", func(t *testing.T) {
		moved := root.Clone().(*vfs.DirDoc)
		moved.Fullpath = "/Private/Renamed/Team"
		res, _ := send(&realtime.Event{Verb: realtime.EventUpdate, Doc: moved, OldDoc: root})
		assert.Equal(t, "/Team", docPath(res, "path"))
		// The root follows its events: it comes back for the next subtests
		res, _ = send(&realtime.Event{Verb: realtime.EventUpdate, Doc: root, OldDoc: moved})
		assert.Equal(t, "/Team", docPath(res, "path"))
	})

	t.Run("TrashedFileDoc", func(t *testing.T) {
		file := &vfs.FileDoc{
			Type: consts.FileType, DocID: "trashed-file", DocName: "a.txt", DirID: consts.TrashDirID,
			Trashed: true, RestorePath: sub.Fullpath,
		}
		res, _ := send(&realtime.Event{Verb: realtime.EventUpdate, Doc: file})
		assert.Equal(t, "/Team/Meetings", docPath(res, "restore_path"))
	})

	t.Run("JSONDoc", func(t *testing.T) {
		doc := &realtime.JSONDoc{Type: consts.Files, M: map[string]interface{}{
			"_id": sub.ID(), "type": consts.DirType, "name": "Meetings", "path": sub.Fullpath,
		}}
		res, _ := send(&realtime.Event{Verb: realtime.EventUpdate, Doc: doc})
		assert.Equal(t, "/Team/Meetings", docPath(res, "path"))

		before := &realtime.JSONDoc{Type: consts.Files, M: map[string]interface{}{
			"_id": "trashed-json", "type": consts.FileType, "name": "a.txt", "dir_id": sub.ID(),
		}}
		trashed := &realtime.JSONDoc{Type: consts.Files, M: map[string]interface{}{
			"_id": "trashed-json", "type": consts.FileType, "name": "a.txt", "dir_id": consts.TrashDirID,
			"trashed": true, "restore_path": sub.Fullpath,
		}}
		res, _ = send(&realtime.Event{Verb: realtime.EventUpdate, Doc: trashed, OldDoc: before})
		assert.Equal(t, realtime.EventUpdate, res.Event)
		assert.Equal(t, "/Team/Meetings", docPath(res, "restore_path"))

		typed := &vfs.FileDoc{
			Type: consts.FileType, DocID: "trashed-json", DocName: "a.txt", DirID: consts.TrashDirID,
			Trashed: true, RestorePath: sub.Fullpath,
		}
		typedRes, _ := send(&realtime.Event{Verb: realtime.EventUpdate, Doc: typed})
		assert.Equal(t, res.Event, typedRes.Event)
		assert.Equal(t, docPath(res, "restore_path"), docPath(typedRes, "restore_path"))
	})

	t.Run("MovedOutOfTheDrive", func(t *testing.T) {
		out := sub.Clone().(*vfs.DirDoc)
		out.Fullpath = "/Private/Other/Meetings"
		out.DirID = "private-other-id"
		res, _ := send(&realtime.Event{Verb: realtime.EventUpdate, Doc: out, OldDoc: sub})
		assert.Equal(t, realtime.EventDelete, res.Event)
		assert.Equal(t, sub.ID(), res.Payload.ID)
		assert.Nil(t, docPath(res, "path"))
		assert.Nil(t, docPath(res, "dir_id"))

		outJSON := &realtime.JSONDoc{Type: consts.Files, M: map[string]interface{}{
			"_id": sub.ID(), "type": consts.DirType, "name": "Meetings", "path": out.Fullpath,
		}}
		oldJSON := &realtime.JSONDoc{Type: consts.Files, M: map[string]interface{}{
			"_id": sub.ID(), "type": consts.DirType, "name": "Meetings", "path": sub.Fullpath,
		}}
		res, _ = send(&realtime.Event{Verb: realtime.EventUpdate, Doc: outJSON, OldDoc: oldJSON})
		assert.Equal(t, realtime.EventDelete, res.Event)
		assert.Nil(t, docPath(res, "path"))
	})

	// As in _changes, a trashed folder and its content leave the drive.
	t.Run("TrashedFolder", func(t *testing.T) {
		trashed := sub.Clone().(*vfs.DirDoc)
		trashed.Fullpath = vfs.TrashDirName + "/Meetings"
		trashed.DirID = consts.TrashDirID
		trashed.RestorePath = root.Fullpath
		res, _ := send(&realtime.Event{Verb: realtime.EventUpdate, Doc: trashed, OldDoc: sub})
		assert.Equal(t, realtime.EventDelete, res.Event)

		before := &vfs.FileDoc{Type: consts.FileType, DocID: "in-trashed-folder", DocName: "a.txt", DirID: sub.ID()}
		after := before.Clone().(*vfs.FileDoc)
		after.Trashed = true
		res, _ = send(&realtime.Event{Verb: realtime.EventUpdate, Doc: after, OldDoc: before})
		assert.Equal(t, realtime.EventDelete, res.Event)
		assert.Equal(t, "in-trashed-folder", res.Payload.ID)
	})
}

func TestDriveRealtimeSurvivesAnAncestorRename(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	root, err := vfs.MkdirAll(fs, "/Private/Clients/Team")
	require.NoError(t, err)
	sub, err := vfs.Mkdir(fs, "/Private/Clients/Team/Meetings", nil)
	require.NoError(t, err)
	other, err := vfs.Mkdir(fs, "/Private/Clients/Team/Notes", nil)
	require.NoError(t, err)

	s := &sharing.Sharing{
		Drive: true,
		Rules: []sharing.Rule{{DocType: consts.Files, Values: []string{root.ID()}}},
	}
	ds := &realtime.Subscriber{Channel: make(realtime.EventsChan, 1)}
	ch := make(chan *wsResponse)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go filterMapEvents(ctx, ds, ch, inst, s)

	clients, err := fs.DirByPath("/Private/Clients")
	require.NoError(t, err)
	renamed := "Renamed"
	_, err = vfs.ModifyDirMetadata(fs, clients, &vfs.DocPatch{Name: &renamed})
	require.NoError(t, err)

	for _, old := range []*vfs.DirDoc{sub, other} {
		moved := old.Clone().(*vfs.DirDoc)
		moved.Fullpath = "/Private/Renamed/Team/" + old.DocName
		ds.Channel <- &realtime.Event{Verb: realtime.EventUpdate, Doc: moved, OldDoc: old}
		select {
		case res := <-ch:
			assert.Equal(t, realtime.EventUpdate, res.Event)
			assert.Equal(t, old.ID(), res.Payload.ID)
			body, err := json.Marshal(res.Payload.Doc)
			require.NoError(t, err)
			assert.Contains(t, string(body), `"path":"/Team/`+old.DocName+`"`)
			assert.NotContains(t, string(body), "Private")
		case <-time.After(5 * time.Second):
			require.FailNow(t, "no event was forwarded")
		}
	}
}

func TestFileDriveRealtimeHidesTheRestorePath(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()

	s := &sharing.Sharing{
		Drive:         true,
		DriveRootType: sharing.DriveRootTypeFile,
		Rules:         []sharing.Rule{{DocType: consts.Files, Values: []string{"root-file"}}},
	}
	ds := &realtime.Subscriber{Channel: make(realtime.EventsChan, 1)}
	ch := make(chan *wsResponse)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go filterMapEvents(ctx, ds, ch, inst, s)

	file := &vfs.FileDoc{
		Type: consts.FileType, DocID: "root-file", DocName: "a.txt", DirID: consts.TrashDirID,
		Trashed: true, RestorePath: "/Private/Clients",
	}
	ds.Channel <- &realtime.Event{Verb: realtime.EventUpdate, Doc: file}
	select {
	case res := <-ch:
		body, err := json.Marshal(res)
		require.NoError(t, err)
		assert.NotContains(t, string(body), "Private")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "no event was forwarded")
	}
}

func TestDriveRealtimeFollowsARenamedRoot(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	root, err := vfs.MkdirAll(fs, "/Private/Team")
	require.NoError(t, err)
	s := &sharing.Sharing{
		Drive: true,
		Rules: []sharing.Rule{{DocType: consts.Files, Values: []string{root.ID()}}},
	}
	ds := &realtime.Subscriber{Channel: make(realtime.EventsChan, 1)}
	ch := make(chan *wsResponse)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go filterMapEvents(ctx, ds, ch, inst, s)

	receive := func() *wsResponse {
		t.Helper()
		select {
		case res := <-ch:
			return res
		case <-time.After(5 * time.Second):
			require.FailNow(t, "no event was forwarded")
			return nil
		}
	}

	name := "Renamed"
	renamed, err := vfs.ModifyDirMetadata(fs, root.Clone().(*vfs.DirDoc), &vfs.DocPatch{Name: &name})
	require.NoError(t, err)
	ds.Channel <- &realtime.Event{Verb: realtime.EventUpdate, Doc: renamed, OldDoc: root}
	assert.Equal(t, root.ID(), receive().Payload.ID)

	child, err := vfs.Mkdir(fs, "/Private/Renamed/New", nil)
	require.NoError(t, err)
	ds.Channel <- &realtime.Event{Verb: realtime.EventCreate, Doc: child}
	res := receive()
	assert.Equal(t, child.ID(), res.Payload.ID)
	assert.Equal(t, "/Renamed/New", res.Payload.Doc.(*vfs.DirDoc).Fullpath)
}

// MoveDir publishes the folders below a moved folder before saving it: the
// events of that window must tell a moving root from a folder leaving the drive.
func TestDriveRealtimeWhileTheRootMoves(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	root, err := vfs.MkdirAll(fs, "/Private/Team")
	require.NoError(t, err)
	draft, err := vfs.MkdirAll(fs, "/Private/Team/Meetings/Draft")
	require.NoError(t, err)
	meetings, err := fs.DirByPath("/Private/Team/Meetings")
	require.NoError(t, err)
	inner, err := vfs.MkdirAll(fs, "/Private/Team/Out/Inner")
	require.NoError(t, err)
	out, err := fs.DirByPath("/Private/Team/Out")
	require.NoError(t, err)
	private, err := fs.DirByPath("/Private")
	require.NoError(t, err)
	report, err := vfs.NewFileDoc("report.txt", meetings.ID(), -1, nil, "text/plain", "text", time.Now(), false, false, false, nil)
	require.NoError(t, err)
	f, err := fs.CreateFile(report, nil)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	s := &sharing.Sharing{
		Drive: true,
		Rules: []sharing.Rule{{DocType: consts.Files, Values: []string{root.ID()}}},
	}

	// moved gives the event of a folder after the move
	moved := func(t *testing.T, doc *vfs.DirDoc) *realtime.Event {
		t.Helper()
		newDoc, err := fs.DirByID(doc.ID())
		require.NoError(t, err)
		return &realtime.Event{Verb: realtime.EventUpdate, Doc: newDoc, OldDoc: doc}
	}

	// watch connects a member before the move, and gives what it receives for
	// each event, in order
	watch := func(t *testing.T) func(e *realtime.Event) *wsResponse {
		ds := &realtime.Subscriber{Channel: make(realtime.EventsChan, 1)}
		ch := make(chan *wsResponse)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go filterMapEvents(ctx, ds, ch, inst, s)
		return func(e *realtime.Event) *wsResponse {
			t.Helper()
			ds.Channel <- e
			select {
			case res := <-ch:
				return res
			case <-time.After(5 * time.Second):
				require.FailNow(t, "no event was forwarded")
				return nil
			}
		}
	}

	t.Run("FolderLeavingTheDrive", func(t *testing.T) {
		send := watch(t)
		require.NoError(t, fs.GetIndexer().MoveDir(out.Fullpath, "/Private/Out"))
		assert.Equal(t, realtime.EventDelete, send(moved(t, inner)).Event)

		moved := out.Clone().(*vfs.DirDoc)
		moved.Fullpath, moved.DirID = "/Private/Out", private.ID()
		require.NoError(t, fs.GetIndexer().UpdateDirDoc(out, moved))
	})

	t.Run("RootMoving", func(t *testing.T) {
		send := watch(t)
		require.NoError(t, fs.GetIndexer().MoveDir(root.Fullpath, "/Private/Renamed"))
		// In the order of MoveDir
		for _, doc := range []*vfs.DirDoc{meetings, draft} {
			res := send(moved(t, doc))
			assert.Equal(t, realtime.EventUpdate, res.Event)
			assert.Equal(t, strings.Replace(doc.Fullpath, "/Private/Team", "/Renamed", 1), res.Payload.Doc.(*vfs.DirDoc).Fullpath)
		}

		// A file changed before the move, with its parent already moved
		file, err := fs.FileByID(report.ID())
		require.NoError(t, err)
		res := send(&realtime.Event{Verb: realtime.EventUpdate, Doc: file, OldDoc: file})
		assert.Equal(t, realtime.EventUpdate, res.Event)
		assert.Equal(t, report.ID(), res.Payload.ID)
	})
}

// The events of a member can wait while the owner keeps moving folders: each
// one is formatted for the state of its own move.
func TestDriveRealtimeQueuedMoves(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	root, err := vfs.MkdirAll(fs, "/Private/Team")
	require.NoError(t, err)
	draft, err := vfs.MkdirAll(fs, "/Private/Team/Meetings/Draft")
	require.NoError(t, err)
	meetings, err := fs.DirByPath("/Private/Team/Meetings")
	require.NoError(t, err)
	inner, err := vfs.MkdirAll(fs, "/Private/Team/Out/Inner")
	require.NoError(t, err)
	out, err := fs.DirByPath("/Private/Team/Out")
	require.NoError(t, err)
	private, err := fs.DirByPath("/Private")
	require.NoError(t, err)
	report, err := vfs.NewFileDoc("report.txt", meetings.ID(), -1, nil, "text/plain", "text", time.Now(), false, false, false, nil)
	require.NoError(t, err)
	f, err := fs.CreateFile(report, nil)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	s := &sharing.Sharing{
		Drive: true,
		Rules: []sharing.Rule{{DocType: consts.Files, Values: []string{root.ID()}}},
	}

	ds := realtime.GetHub().Subscriber(inst)
	defer ds.Close()
	ds.Subscribe(consts.Files)
	ch := make(chan *wsResponse)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// As through Redis, the documents keep only their JSON: a file has no path
	redis := &realtime.Subscriber{Channel: make(realtime.EventsChan, 100)}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case e := <-ds.Channel:
				redis.Channel <- &realtime.Event{Verb: e.Verb, Doc: asRedisDoc(e.Doc), OldDoc: asRedisDoc(e.OldDoc)}
			}
		}
	}()
	go filterMapEvents(ctx, redis, ch, inst, s)

	// drain gives the events forwarded after the writer has waited on the event
	// of a new folder while the owner made the changes
	drain := func(t *testing.T, blocker string, changes func()) map[string][]string {
		t.Helper()
		_, err := vfs.Mkdir(fs, blocker, nil)
		require.NoError(t, err)
		changes()
		received := map[string][]string{}
		for {
			select {
			case res := <-ch:
				body, err := json.Marshal(res)
				require.NoError(t, err)
				assert.NotContains(t, string(body), "Private")
				got := res.Event
				if p, _ := res.Payload.Doc.(*realtime.JSONDoc).M["path"].(string); p != "" {
					got = p
				}
				received[res.Payload.ID] = append(received[res.Payload.ID], got)
			case <-time.After(time.Second):
				return received
			}
		}
	}

	t.Run("FolderLeftTheDrive", func(t *testing.T) {
		received := drain(t, "/Private/Team/Blocker", func() {
			privateID := private.ID()
			_, err := vfs.ModifyDirMetadata(fs, out, &vfs.DocPatch{DirID: &privateID})
			require.NoError(t, err)
		})
		assert.Equal(t, []string{realtime.EventDelete}, received[inner.ID()])
		assert.Equal(t, []string{realtime.EventDelete}, received[out.ID()])
	})

	t.Run("RootRenamedTwice", func(t *testing.T) {
		received := drain(t, "/Private/Team/Blocker2", func() {
			for _, name := range []string{"Spring", "Summer"} {
				current, err := fs.DirByID(root.ID())
				require.NoError(t, err)
				_, err = vfs.ModifyDirMetadata(fs, current, &vfs.DocPatch{Name: &name})
				require.NoError(t, err)
				if name == "Spring" {
					// A file changed between the two renames
					doc, err := fs.FileByID(report.ID())
					require.NoError(t, err)
					tags := []string{"reviewed"}
					_, err = vfs.ModifyFileMetadata(fs, doc, &vfs.DocPatch{Tags: &tags})
					require.NoError(t, err)
				}
			}
		})
		assert.Equal(t, []string{realtime.EventUpdate}, received[report.ID()])
		assert.Equal(t, []string{"/Spring/Meetings", "/Summer/Meetings"}, received[meetings.ID()])
		assert.Equal(t, []string{"/Spring/Meetings/Draft", "/Summer/Meetings/Draft"}, received[draft.ID()])
		assert.Equal(t, []string{"/Spring", "/Summer"}, received[root.ID()])
	})
}

func asRedisDoc(doc realtime.Doc) realtime.Doc {
	if doc == nil {
		return nil
	}
	m := map[string]interface{}{}
	if buf, err := json.Marshal(doc); err == nil {
		_ = json.Unmarshal(buf, &m)
	}
	return &realtime.JSONDoc{Type: doc.DocType(), M: m}
}

func TestParentsReachTheDeepestFolders(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	fs := setup.GetTestInstance().VFS()

	root, err := vfs.MkdirAll(fs, "/Team")
	require.NoError(t, err)
	deep, err := vfs.MkdirAll(fs, "/Team"+strings.Repeat("/d", 300))
	require.NoError(t, err)
	in, err := parentsReach(fs.GetIndexer(), root.ID(), deep.ID())
	require.NoError(t, err)
	assert.True(t, in)
}
