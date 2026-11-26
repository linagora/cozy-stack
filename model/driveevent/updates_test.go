package driveevent

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func useRedis(t *testing.T) *redis.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("a redis is required for this test: test skipped due to the use of --short flag")
	}
	url := os.Getenv("COZY_TEST_REDIS_URL")
	if url == "" {
		url = "redis://localhost:6379/15"
	}
	opt, err := redis.ParseURL(url)
	require.NoError(t, err)
	cli := redis.NewClient(opt)
	require.NoError(t, cli.Ping(context.Background()).Err())
	previous := config.GetConfig().SessionStorage
	config.GetConfig().SessionStorage = cli
	t.Cleanup(func() { config.GetConfig().SessionStorage = previous })
	return cli
}

// recordSchedules replaces the jobs by a list, or by err when it is set.
func recordSchedules(t *testing.T, err error) *[]UpdatesMessage {
	t.Helper()
	var jobs []UpdatesMessage
	previous := scheduleUpdates
	scheduleUpdates = func(_ *instance.Instance, msg *UpdatesMessage, _ time.Duration) error {
		jobs = append(jobs, *msg)
		return err
	}
	t.Cleanup(func() { scheduleUpdates = previous })
	return &jobs
}

func uniqueFile(t *testing.T, cli *redis.Client, inst *instance.Instance) *vfs.FileDoc {
	t.Helper()
	file := &vfs.FileDoc{DocID: "grouped-" + time.Now().Format("150405.000000000"), MD5Sum: []byte{2}}
	t.Cleanup(func() {
		keys, _ := cli.Keys(context.Background(), "drive-event:"+inst.DBPrefix()+"/"+file.DocID+":*").Result()
		if len(keys) > 0 {
			cli.Del(context.Background(), keys...)
		}
	})
	return file
}

func TestGroupedUpdates(t *testing.T) {
	config.UseTestFile(t)
	cli := useRedis(t)
	fileEventsContext(t, true)
	ctx := context.Background()
	inst := owner()

	t.Run("OneJobPerWindow", func(t *testing.T) {
		pub := publishTo(t, nil)
		jobs := recordSchedules(t, nil)
		file := uniqueFile(t, cli, inst)
		FileUpdated(ctx, inst, nil, file, false)
		FileUpdated(ctx, inst, nil, file, true)
		FileUpdated(ctx, inst, nil, file, false)

		assert.Empty(t, pub.requests, "nothing is published before the end of the window")
		require.Len(t, *jobs, 1, "the first update opens the window")
		msg := (*jobs)[0]
		assert.Equal(t, file.DocID, msg.FileID)
		ttl, err := cli.TTL(ctx, windowKey(inst, file.DocID)).Result()
		require.NoError(t, err)
		assert.InDelta(t, defaultWindow.Seconds(), ttl.Seconds(), 2)
		byOwner, err := cli.Get(ctx, byOwnerKey(inst, file.DocID, msg.Window)).Result()
		require.NoError(t, err)
		assert.Equal(t, "1", byOwner, "one of the updates was made by the owner")
	})

	t.Run("AWindowWithoutJobIsClosed", func(t *testing.T) {
		pub := publishTo(t, nil)
		recordSchedules(t, errors.New("no job system"))
		file := uniqueFile(t, cli, inst)
		FileUpdated(ctx, inst, nil, file, true)
		FileUpdated(ctx, inst, nil, file, true)
		assert.Len(t, pub.requests, 2, "each update is published at once")
		n, err := cli.Exists(ctx, windowKey(inst, file.DocID)).Result()
		require.NoError(t, err)
		assert.Zero(t, n)
	})
}

func writeFile(t *testing.T, inst *instance.Instance, name string, olddoc *vfs.FileDoc) *vfs.FileDoc {
	t.Helper()
	content := []byte(name + time.Now().String())
	doc, err := vfs.NewFileDoc(name, consts.RootDirID, int64(len(content)), nil,
		"text/plain", "text", time.Now(), false, false, false, nil)
	require.NoError(t, err)
	if olddoc != nil {
		doc.SetID(olddoc.ID())
	}
	f, err := inst.VFS().CreateFile(doc, olddoc)
	require.NoError(t, err)
	_, err = f.Write(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return doc
}

func TestPublishUpdatesAtTheEndOfTheWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("a couchdb and a redis are required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	cli := useRedis(t)
	fileEventsContext(t, true)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	inst.OIDCUUID = ownerUUID
	ctx := context.Background()
	window := func(file *vfs.FileDoc, byOwner bool) *UpdatesMessage {
		msg := &UpdatesMessage{FileID: file.ID(), Window: "1000"}
		require.NoError(t, cli.Set(ctx, windowKey(inst, file.ID()), msg.Window, time.Minute).Err())
		if byOwner {
			require.NoError(t, cli.Set(ctx, byOwnerKey(inst, file.ID(), msg.Window), "1", time.Minute).Err())
		}
		return msg
	}

	t.Run("ClosesTheWindowAndTellsTheFileAsItIsThen", func(t *testing.T) {
		first := writeFile(t, inst, "report.txt", nil)
		last := writeFile(t, inst, "report.txt", first)
		msg := window(first, true)
		pub := &fakePublisher{}
		require.NoError(t, publishUpdates(ctx, pub, inst, msg))
		require.Len(t, pub.requests, 1)
		event := pub.requests[0].Payload.(rabbitmq.CloudEvent)
		assert.Equal(t, rabbitmq.RoutingKeyDriveFileUpdated, event.Type)
		assert.Equal(t, last.ID()+"/updated/"+last.Rev(), event.ID)
		assert.Equal(t, ownerUUID, event.TwakeActorID)
		assert.Equal(t, consts.RootDirID, event.Data.(Data).Object.Folder.ID)
		n, err := cli.Exists(ctx, windowKey(inst, first.ID()), byOwnerKey(inst, first.ID(), msg.Window)).Result()
		require.NoError(t, err)
		assert.Zero(t, n, "an update from now on opens the next window")
	})

	t.Run("LeavesTheNextWindowOpen", func(t *testing.T) {
		file := writeFile(t, inst, "next.txt", nil)
		msg := window(file, false)
		require.NoError(t, cli.Set(ctx, windowKey(inst, file.ID()), "2000", time.Minute).Err())
		require.NoError(t, publishUpdates(ctx, &fakePublisher{}, inst, msg))
		w, err := cli.Get(ctx, windowKey(inst, file.ID())).Result()
		require.NoError(t, err)
		assert.Equal(t, "2000", w)
	})

	t.Run("FailsWhenThePublishFailsSoThatTheJobIsTriedAgain", func(t *testing.T) {
		file := writeFile(t, inst, "retry.txt", nil)
		msg := window(file, true)
		err := publishUpdates(ctx, &fakePublisher{err: assert.AnError}, inst, msg)
		assert.ErrorIs(t, err, assert.AnError)
		pub := &fakePublisher{}
		require.NoError(t, publishUpdates(ctx, pub, inst, msg))
		require.Len(t, pub.requests, 1)
		assert.Equal(t, ownerUUID, pub.requests[0].Payload.(rabbitmq.CloudEvent).TwakeActorID,
			"the retry still knows who made the updates")
	})

	t.Run("TellsNothingOfAFileInTheTrashOrGone", func(t *testing.T) {
		file := writeFile(t, inst, "draft.txt", nil)
		msg := window(file, true)
		_, err := vfs.TrashFile(inst.VFS(), file)
		require.NoError(t, err)
		pub := &fakePublisher{}
		require.NoError(t, publishUpdates(ctx, pub, inst, msg))
		require.NoError(t, publishUpdates(ctx, pub, inst, &UpdatesMessage{FileID: "no-such-file", Window: "1"}))
		assert.Empty(t, pub.requests)
		n, err := cli.Exists(ctx, byOwnerKey(inst, file.ID(), msg.Window)).Result()
		require.NoError(t, err)
		assert.Zero(t, n, "the owner of the updates is forgotten with the window")
	})
}

func TestWindowOf(t *testing.T) {
	config.UseTestFile(t)
	assert.Equal(t, defaultWindow, windowOf(owner()))
	contexts := config.GetConfig().Contexts
	t.Cleanup(func() { config.GetConfig().Contexts = contexts })
	config.GetConfig().Contexts = map[string]interface{}{
		config.DefaultInstanceContext: map[string]interface{}{
			"drive_activity": map[string]interface{}{"window": "2m"},
		},
	}
	assert.Equal(t, 2*time.Minute, windowOf(owner()))
}
