package sharings

import (
	"context"
	"encoding/json"
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
