package vfss3

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/lock"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A frozen S3 accepts connections and never answers. The calls made with the
// VFS lock held must give up, so that the lock comes back.
func TestFrozenS3CallsGiveUp(t *testing.T) {
	defer func(d time.Duration) { s3CallTimeout = d }(s3CallTimeout)
	s3CallTimeout = time.Second

	var frozen atomic.Bool
	received := make(chan struct{}, 64)
	thaw := make(chan struct{})
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !frozen.Load() {
			return
		}
		select {
		case received <- struct{}{}:
		default:
		}
		<-thaw
	}))
	defer endpoint.Close()

	u, err := url.Parse(strings.Replace(endpoint.URL, "https:", "s3:", 1))
	require.NoError(t, err)
	u.RawQuery = url.Values{"access_key": {"KEY"}, "secret_key": {"SECRET"}, "region": {"us-east-1"}}.Encode()
	disabled := false
	require.NoError(t, config.InitS3Connection(config.Fs{
		URL: u, Transport: endpoint.Client().Transport,
		S3: config.FsS3{Buckets: map[string]config.FsS3Bucket{"default": {Name: "cozy-frozen"}}, AutoCreateBuckets: &disabled},
	}))
	db := &s3TestPrefixer{Prefixer: prefixer.NewPrefixer(0, "instance", "instance")}
	index := &frozenTestIndexer{}

	var pending sync.WaitGroup
	defer pending.Wait()
	defer close(thaw)

	doc := &vfs.FileDoc{DocID: "file", InternalID: "internal", DocName: "file", ByteSize: 1}
	cases := []struct {
		name string
		op   func(fs vfs.VFS) error
	}{
		{"open", func(fs vfs.VFS) error {
			f, err := fs.OpenFile(doc)
			if err == nil {
				_ = f.Close()
			}
			return err
		}},
		{"copy", func(fs vfs.VFS) error {
			return fs.CopyFile(doc, &vfs.FileDoc{DocName: "copy", ByteSize: 1})
		}},
		{"clear old versions", func(fs vfs.VFS) error {
			return fs.ClearOldVersions()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mu := lock.NewInMemory().ReadWrite(db, "vfs")
			fs, err := New(db, index, index, mu)
			require.NoError(t, err)
			frozen.Store(true)
			done := make(chan error, 1)
			pending.Add(1)
			go func() {
				defer pending.Done()
				done <- tc.op(fs)
			}()
			select {
			case <-received:
			case <-time.After(5 * time.Second):
				t.Fatal("the operation never called S3")
			}

			select {
			case err := <-done:
				assert.Error(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("the S3 call never gives up")
			}
			locked := make(chan struct{})
			go func() {
				if mu.Lock() == nil {
					mu.Unlock()
				}
				close(locked)
			}()
			select {
			case <-locked:
			case <-time.After(time.Second):
				t.Fatal("the VFS lock is still held once the S3 call gave up")
			}
		})
	}
}

type frozenTestIndexer struct{ s3TestIndexer }

func (i *frozenTestIndexer) DiskUsage() (int64, error)                { return 0, nil }
func (i *frozenTestIndexer) BatchDeleteVersions([]*vfs.Version) error { return nil }
func (i *frozenTestIndexer) AllVersions() ([]*vfs.Version, error) {
	return []*vfs.Version{{DocID: "file/old"}}, nil
}
