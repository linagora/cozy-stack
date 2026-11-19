package lifecycle_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type listCounter struct {
	next        http.RoundTripper
	lists       atomic.Int64
	denyDeletes atomic.Bool
	onDelete    func()
}

func (c *listCounter) RoundTrip(r *http.Request) (*http.Response, error) {
	q := r.URL.Query()
	if r.Method == http.MethodGet && (q.Has("list-type") || q.Has("prefix")) {
		c.lists.Add(1)
	}
	if c.onDelete != nil && r.Method == http.MethodPost && q.Has("delete") {
		c.onDelete()
	}
	if c.denyDeletes.Load() && r.Method == http.MethodPost && q.Has("delete") {
		// 403 rather than 5xx, which minio-go would retry.
		body := "<Error><Code>AccessDenied</Code><Message>denied</Message></Error>"
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     http.Header{"Content-Type": {"application/xml"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	}
	return c.next.RoundTrip(r)
}

// testDestroyS3InstanceWithoutListing runs inside TestLifecycle, which starts
// the stack.
func testDestroyS3InstanceWithoutListing(t *testing.T) {
	off := false
	config.GetConfig().Fs.S3.DeleteInstancesByListing = &off
	t.Cleanup(func() { config.GetConfig().Fs.S3.DeleteInstancesByListing = nil })

	mf := testutils.StartMinio(t)
	counter := &listCounter{next: http.DefaultTransport}
	require.NoError(t, config.InitS3Connection(config.Fs{
		URL:       mf.FsURL(),
		Transport: counter,
		S3:        config.FsS3{Buckets: map[string]config.FsS3Bucket{"default": {Name: "destroy-test"}}},
	}))
	storage := config.GetS3Storage(config.S3StorageFiles)
	ctx := context.Background()

	inst, err := createInstance(t, &lifecycle.Options{Domain: "s3-destroy.cozycloud.cc", Locale: "en"})
	require.NoError(t, err)
	inst.FsScheme = config.SchemeS3
	require.NoError(t, instance.Update(inst))
	inst, err = instance.Get(inst.Domain)
	require.NoError(t, err)

	doc, err := vfs.NewFileDoc("photo.jpg", "", -1, nil, "image/jpeg", "image", time.Now(), false, false, false, nil)
	require.NoError(t, err)
	f, err := inst.VFS().CreateFile(doc, nil)
	require.NoError(t, err)
	_, err = io.Copy(f, bytes.NewReader([]byte("photo")))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	th, err := inst.ThumbsFS().CreateThumb(doc, "small")
	require.NoError(t, err)
	_, err = th.Write([]byte("thumb"))
	require.NoError(t, err)
	require.NoError(t, th.Commit())
	av, err := inst.AvatarFS().CreateAvatar("image/png")
	require.NoError(t, err)
	_, err = av.Write([]byte("avatar"))
	require.NoError(t, err)
	require.NoError(t, av.Close())

	listObjects := func() []string {
		var keys []string
		prefix := storage.Prefix + inst.DBPrefix() + "/"
		for obj := range storage.Client.ListObjects(ctx, storage.Bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
			require.NoError(t, obj.Err)
			keys = append(keys, obj.Key)
		}
		return keys
	}
	require.Len(t, listObjects(), 3)

	counter.denyDeletes.Store(true)
	// avatar, photo and its 4 thumbnail formats
	require.ErrorContains(t, lifecycle.Destroy(inst.Domain), "6 objects not deleted: denied")
	counter.denyDeletes.Store(false)
	_, err = inst.VFS().FileByID(doc.ID())
	require.NoError(t, err, "a failed S3 deletion must leave CouchDB untouched")
	require.Len(t, listObjects(), 3)

	var deletes, deletesWhileOpen atomic.Int64
	counter.onDelete = func() {
		deletes.Add(1)
		if i, err := instance.Get(inst.Domain); err == nil && !i.Deleting {
			deletesWhileOpen.Add(1)
		}
	}
	before := counter.lists.Load()
	require.NoError(t, lifecycle.Destroy(inst.Domain))
	assert.Equal(t, before, counter.lists.Load(), "Destroy must not list the bucket")
	assert.Positive(t, deletes.Load())
	assert.Zero(t, deletesWhileOpen.Load(), "the instance must stay closed to requests while its files are deleted")
	assert.Empty(t, listObjects())
}
