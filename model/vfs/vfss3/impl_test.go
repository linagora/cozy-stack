package vfss3

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/lock"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrecreatedVFS(t *testing.T) {
	const content = "file copied between separate S3 connections"
	var requests atomic.Int64
	newEndpoint := func(bucket, key string) *httptest.Server {
		return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			assert.Contains(t, r.Header.Get("Authorization"), "Credential="+key+"/")
			assert.Empty(t, r.Header.Get("X-Amz-Copy-Source"), "separate connections require streaming")
			if r.Method == http.MethodHead && strings.Count(r.URL.Path, "/") == 2 {
				return
			}
			assert.True(t, strings.HasPrefix(r.URL.Path, "/"+bucket+"/files/instance/"), r.URL.Path)
			switch r.Method {
			case http.MethodHead, http.MethodGet:
				w.Header().Set("Content-Type", "text/plain")
				w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
				w.Header().Set("Etag", `"etag"`)
				w.Header().Set("Content-Length", strconv.Itoa(len(content)))
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, content)
				}
			case http.MethodPut:
				body, err := io.ReadAll(r.Body)
				assert.NoError(t, err)
				assert.Equal(t, content, string(body))
			default:
				t.Errorf("unexpected S3 request: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusForbidden)
			}
		}))
	}
	source := newEndpoint("cozy-source", "SOURCE_KEY")
	defer source.Close()
	destination := newEndpoint("cozy-default", "DESTINATION_KEY")
	defer destination.Close()
	connectionURL := func(endpoint, key string) *url.URL {
		u, err := url.Parse(strings.Replace(endpoint, "https:", "s3:", 1))
		require.NoError(t, err)
		u.RawQuery = url.Values{"access_key": {key}, "secret_key": {key + "_SECRET"}, "region": {"us-east-1"}}.Encode()
		return u
	}
	disabled := false
	newFS := func(bucket string, endpoint *httptest.Server, key string) vfs.VFS {
		require.NoError(t, config.InitS3Connection(config.Fs{
			URL: connectionURL(endpoint.URL, key), Transport: endpoint.Client().Transport,
			S3: config.FsS3{Buckets: map[string]config.FsS3Bucket{"default": {Name: bucket}}, AutoCreateBuckets: &disabled},
		}))
		db := &s3TestPrefixer{Prefixer: prefixer.NewPrefixer(0, "instance", "instance")}
		index := &s3TestIndexer{}
		before := requests.Load()
		fs, err := New(db, index, index, lock.NewInMemory().ReadWrite(db, "vfs"))
		require.NoError(t, err)
		require.NoError(t, fs.InitFs())
		assert.Equal(t, before, requests.Load(), "constructing and initializing a VFS must not probe S3")
		return fs
	}
	srcFS := newFS("cozy-source", source, "SOURCE_KEY")
	dstFS := newFS("cozy-default", destination, "DESTINATION_KEY")
	src := &vfs.FileDoc{DocID: "original", InternalID: "original-internal", ByteSize: int64(len(content)), Mime: "text/plain"}
	dst := &vfs.FileDoc{DocID: "copy", DocName: "copy.txt", ByteSize: src.ByteSize, Mime: src.Mime}
	require.NoError(t, dstFS.CopyFileFromOtherFS(dst, nil, srcFS, src))
}

type s3TestPrefixer struct {
	prefixer.Prefixer
}

func (p *s3TestPrefixer) GetContextName() string { return "test" }

type s3TestIndexer struct{ vfs.Indexer }

func (i *s3TestIndexer) InitIndex() error                            { return nil }
func (i *s3TestIndexer) DiskQuota() int64                            { return 0 }
func (i *s3TestIndexer) FilePath(doc *vfs.FileDoc) (string, error)   { return "/" + doc.DocName, nil }
func (i *s3TestIndexer) DirChildExists(string, string) (bool, error) { return false, nil }
func (i *s3TestIndexer) CreateNamedFileDoc(*vfs.FileDoc) error       { return nil }
