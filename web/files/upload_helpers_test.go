package files

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"strconv"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	weberrors "github.com/cozy/cozy-stack/web/errors"
	"github.com/labstack/echo/v4"
	"github.com/minio/minio-go/v7"
	"github.com/ncw/swift/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const uploadTestPartSize = 6 << 20

// uploadWebBackend configures the stack for a storage and tells what an
// upload has left in it.
type uploadWebBackend struct {
	name      string
	configure func(t *testing.T)
	leftovers func(t *testing.T, inst *instance.Instance, uploadID string) []string
}

func uploadWebBackends() []uploadWebBackend {
	var aferoDir string
	return []uploadWebBackend{
		{
			name: "afero",
			configure: func(t *testing.T) {
				aferoDir = t.TempDir()
				config.GetConfig().Fs.URL = &url.URL{Scheme: "file", Host: "localhost", Path: aferoDir}
			},
			leftovers: func(t *testing.T, inst *instance.Instance, id string) []string {
				p := path.Join(aferoDir, inst.DirName(), vfs.UploadsDirName, id)
				if _, err := os.Stat(p); err == nil {
					return []string{p}
				}
				return nil
			},
		},
		{
			name: "s3",
			configure: func(t *testing.T) {
				fixture := testutils.StartMinio(t)
				fs := config.Fs{
					URL: fixture.FsURL(),
					S3:  config.FsS3{Buckets: map[string]config.FsS3Bucket{"default": {Name: "upload-web"}}},
				}
				require.NoError(t, config.InitS3Connection(fs))
				config.GetConfig().Fs.URL = fs.URL
				config.GetConfig().Fs.S3 = fs.S3
			},
			leftovers: func(t *testing.T, inst *instance.Instance, id string) []string {
				storage := config.GetS3Storage(config.S3StorageFiles)
				var names []string
				for obj := range storage.Client.ListObjects(context.Background(), storage.Bucket, minio.ListObjectsOptions{
					Prefix:    storage.Prefix + inst.DBPrefix() + "/" + vfs.UploadsObjectPrefix + id + "/",
					Recursive: true,
				}) {
					require.NoError(t, obj.Err)
					names = append(names, obj.Key)
				}
				return names
			},
		},
		{
			name: "swift",
			configure: func(t *testing.T) {
				fixture := testutils.StartSwift(t)
				require.NoError(t, config.InitSwiftConnection(config.Fs{URL: fixture.FsURL()}))
				config.GetConfig().Fs.URL = fixture.FsURL()
			},
			leftovers: func(t *testing.T, inst *instance.Instance, id string) []string {
				names, err := config.GetSwiftConnection().ObjectNamesAll(context.Background(),
					"cozy-v3-"+inst.DBPrefix(), &swift.ObjectsOpts{Prefix: vfs.UploadsObjectPrefix + id + "/"})
				require.NoError(t, err)
				return names
			},
		},
	}
}

// uploadClient speaks the resumable upload protocol, like URLSession does.
type uploadClient struct {
	t     *testing.T
	base  string
	token string
	// announced receives the Location of the 104 responses, when set.
	announced chan string
}

type uploadResponse struct {
	*http.Response
	body []byte
	// interim is the Location announced by a 104 interim response.
	interim string
}

func (c *uploadClient) do(method, target string, body io.Reader, size int64, headers map[string]string) (*uploadResponse, error) {
	res := &uploadResponse{}
	trace := &httptrace.ClientTrace{
		Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
			if code == 104 {
				res.interim = header.Get("Location")
				if c.announced != nil {
					c.announced <- res.interim
				}
			}
			return nil
		},
	}
	ctx := httptrace.WithClientTrace(context.Background(), trace)
	req, err := http.NewRequestWithContext(ctx, method, c.base+target, body)
	require.NoError(c.t, err)
	req.ContentLength = size
	if body == nil {
		req.ContentLength = 0
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()
	res.Response = resp
	res.body, err = io.ReadAll(resp.Body)
	require.NoError(c.t, err)
	return res, nil
}

func (c *uploadClient) head(location string) *uploadResponse {
	res, err := c.do(http.MethodHead, location, nil, 0, map[string]string{"Upload-Draft-Interop-Version": "6"})
	require.NoError(c.t, err)
	return res
}

func (c *uploadClient) patch(location string, offset int64, content []byte, complete bool) *uploadResponse {
	res, err := c.do(http.MethodPatch, location, bytes.NewReader(content), int64(len(content)), map[string]string{
		"Upload-Draft-Interop-Version": "6",
		"Upload-Complete":              sfBoolean(complete),
		"Upload-Offset":                strconv.FormatInt(offset, 10),
		"Content-Type":                 "application/partial-upload",
	})
	require.NoError(c.t, err)
	return res
}

func (r *uploadResponse) fileAttrs(t *testing.T) (string, map[string]interface{}) {
	var doc struct {
		Data struct {
			ID         string                 `json:"id"`
			Attributes map[string]interface{} `json:"attributes"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(r.body, &doc), string(r.body))
	return doc.Data.ID, doc.Data.Attributes
}

func (r *uploadResponse) problem(t *testing.T) map[string]interface{} {
	assert.Equal(t, "application/problem+json", r.Header.Get("Content-Type"))
	var p map[string]interface{}
	require.NoError(t, json.Unmarshal(r.body, &p), string(r.body))
	return p
}

type cutBody struct {
	r io.Reader
	n int64
}

func (c *cutBody) Read(p []byte) (int, error) {
	if c.n <= 0 {
		return 0, errors.New("connection lost")
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	n, err := c.r.Read(p)
	c.n -= int64(n)
	return n, err
}

func randomBytes(size int, seed int64) []byte {
	buf := make([]byte, size)
	_, _ = rand.New(rand.NewSource(seed)).Read(buf)
	return buf
}

func md5Base64(content []byte) string {
	sum := md5.Sum(content)
	return base64.StdEncoding.EncodeToString(sum[:])
}

func newUploadServer(t *testing.T, inst *instance.Instance) *httptest.Server {
	handler := echo.New()
	group := handler.Group("/files", func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set("instance", inst)
			return next(c)
		}
	})
	Routes(group)
	handler.HTTPErrorHandler = weberrors.ErrorHandler
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

func uploadIDOf(location string) string {
	return path.Base(location)
}

func waitUploadOffset(t *testing.T, inst *instance.Instance, id string, offset int64) {
	require.Eventually(t, func() bool {
		u := &vfs.Upload{}
		if err := couchdb.GetDoc(inst, consts.FilesUploads, id, u); err != nil {
			return false
		}
		return u.Offset == offset
	}, 30*time.Second, 50*time.Millisecond)
}

func expireWebUpload(t *testing.T, inst *instance.Instance, id string) {
	u := &vfs.Upload{}
	require.NoError(t, couchdb.GetDoc(inst, consts.FilesUploads, id, u))
	u.ExpiresAt = time.Now().Add(-time.Minute)
	require.NoError(t, couchdb.UpdateDoc(inst, u))
}
