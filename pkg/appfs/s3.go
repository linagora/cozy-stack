package appfs

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/filetype"
	"github.com/cozy/cozy-stack/pkg/s3util"

	web_utils "github.com/cozy/cozy-stack/pkg/utils"
	"github.com/labstack/echo/v4"
	"github.com/minio/minio-go/v7"
)

// installedMarkerSuffix is appended to the app's base key to form the
// "installation complete" marker object. The suffix is chosen so the marker
// can't collide with any file under <slug>/<version>/... — S3 browsers
// would otherwise display the marker as a file sitting next to a folder of
// the same name.
const installedMarkerSuffix = ".cozy-installed"

// s3Copier implements the Copier interface backed by S3.
type s3Copier struct {
	client      *minio.Client
	bucket      string
	appObj      string
	started     bool
	objectNames []string
	ctx         context.Context
}

// NewS3Copier creates a Copier that stores app files in S3.
func NewS3Copier(client *minio.Client, bucket string) Copier {
	return &s3Copier{
		client: client,
		bucket: bucket,
		ctx:    context.Background(),
	}
}

func (f *s3Copier) Exist(slug, version, shasum string) (bool, error) {
	f.appObj = path.Join(slug, version)
	if shasum != "" {
		f.appObj += "-" + shasum
	}
	_, err := f.client.StatObject(f.ctx, f.bucket, f.appObj+installedMarkerSuffix, minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	if s3util.IsNotFound(err) {
		return false, nil
	}
	return false, err
}

func (f *s3Copier) Start(slug, version, shasum string) (bool, error) {
	exist, err := f.Exist(slug, version, shasum)
	if err != nil || exist {
		return exist, err
	}

	if err := s3util.EnsureBucket(f.ctx, f.client, f.bucket, ""); err != nil {
		return false, err
	}

	f.objectNames = []string{}
	f.started = true
	return false, nil
}

func (f *s3Copier) Copy(stat os.FileInfo, src io.Reader) error {
	if !f.started {
		return fmt.Errorf("appfs: copier must call Start() before Copy()")
	}

	// Write directly to the final location (appObj/filename).
	// Reject path traversal attempts in filenames.
	name := stat.Name()
	if strings.Contains(name, "..") {
		return fmt.Errorf("appfs: invalid filename %q", name)
	}
	objName := path.Join(f.appObj, name)

	contentType := filetype.ByExtension(path.Ext(stat.Name()))
	if contentType == "" {
		contentType, src = filetype.FromReader(src)
	}

	// Compress with brotli.
	var buf bytes.Buffer
	bw := brotli.NewWriter(&buf)
	if _, err := io.Copy(bw, src); err != nil {
		return err
	}
	if err := bw.Close(); err != nil {
		return err
	}

	meta := map[string]string{
		"X-Content-Encoding":      "br",
		"Original-Content-Length": strconv.FormatInt(stat.Size(), 10),
	}

	f.objectNames = append(f.objectNames, objName)
	_, err := f.client.PutObject(f.ctx, f.bucket, objName,
		bytes.NewReader(buf.Bytes()), int64(buf.Len()),
		minio.PutObjectOptions{
			ContentType:  contentType,
			UserMetadata: meta,
		})
	return err
}

func (f *s3Copier) Abort() error {
	return s3util.DeleteObjects(f.ctx, f.client, f.bucket, f.objectNames)
}

func (f *s3Copier) Commit() (err error) {
	// Create the marker object that signals the version is complete. The
	// suffix keeps it on a distinct key from the <slug>/<version>/... files,
	// so S3 browsers don't render a folder and a file with the same name.
	_, err = f.client.PutObject(f.ctx, f.bucket, f.appObj+installedMarkerSuffix,
		bytes.NewReader(nil), 0, minio.PutObjectOptions{
			ContentType: "text/plain",
		})
	return err
}

// s3Server implements the FileServer interface backed by S3.
type s3Server struct {
	client *minio.Client
	bucket string
	ctx    context.Context
}

// NewS3FileServer creates a FileServer that serves app files from S3.
func NewS3FileServer(client *minio.Client, bucket string) FileServer {
	return &s3Server{
		client: client,
		bucket: bucket,
		ctx:    context.Background(),
	}
}

func (s *s3Server) Open(slug, version, shasum, file string) (io.ReadCloser, error) {
	objName := s.makeObjectName(slug, version, shasum, file)
	obj, err := s.client.GetObject(s.ctx, s.bucket, objName, minio.GetObjectOptions{})
	if err != nil {
		return nil, s3util.WrapNotFound(err)
	}
	info, err := obj.Stat()
	if err != nil {
		obj.Close()
		return nil, s3util.WrapNotFound(err)
	}
	contentEncoding := info.UserMetadata["X-Content-Encoding"]
	if contentEncoding == "br" {
		return newBrotliReadCloser(obj)
	} else if contentEncoding == "gzip" {
		return newGzipReadCloser(obj)
	}
	return obj, nil
}

func (s *s3Server) ServeFileContent(w http.ResponseWriter, req *http.Request, slug, version, shasum, file string) error {
	objName := s.makeObjectName(slug, version, shasum, file)
	obj, err := s.client.GetObject(s.ctx, s.bucket, objName, minio.GetObjectOptions{})
	if err != nil {
		return s3util.WrapNotFound(err)
	}
	defer obj.Close()

	info, err := obj.Stat()
	if err != nil {
		return s3util.WrapNotFound(err)
	}

	if checkETag := req.Header.Get("Cache-Control") == ""; checkETag {
		etagVal := info.ETag
		if len(etagVal) > 10 {
			etagVal = etagVal[:10]
		}
		etag := fmt.Sprintf(`"%s"`, etagVal)
		if web_utils.CheckPreconditions(w, req, etag) {
			return nil
		}
		w.Header().Set("Etag", etag)
	}

	// Read the full object to handle brotli decompression.
	// Limit to 50 MiB to avoid unbounded memory allocation from corrupted objects.
	const maxAppFileSize = 50 << 20
	content, err := io.ReadAll(io.LimitReader(obj, maxAppFileSize))
	if err != nil {
		return err
	}

	var r io.Reader = bytes.NewReader(content)
	contentLength := info.Size
	contentType := info.ContentType

	contentEncoding := info.UserMetadata["X-Content-Encoding"]
	origContentLength := info.UserMetadata["Original-Content-Length"]
	if contentEncoding == "br" {
		if acceptBrotliEncoding(req) {
			w.Header().Set(echo.HeaderContentEncoding, "br")
		} else {
			if origContentLength != "" {
				contentLength, _ = strconv.ParseInt(origContentLength, 10, 64)
			}
			r = brotli.NewReader(bytes.NewReader(content))
		}
	} else if contentEncoding == "gzip" {
		if acceptGzipEncoding(req) {
			w.Header().Set(echo.HeaderContentEncoding, "gzip")
		} else {
			if origContentLength != "" {
				contentLength, _ = strconv.ParseInt(origContentLength, 10, 64)
			}
			gr, gerr := gzip.NewReader(bytes.NewReader(content))
			if gerr != nil {
				return gerr
			}
			defer gr.Close()
			r = gr
		}
	}

	ext := path.Ext(file)
	if contentType == "" {
		contentType = mime.TypeByExtension(ext)
	}
	if contentType == "text/xml" && ext == ".svg" {
		contentType = "image/svg+xml"
	}

	return serveContent(w, req, contentType, contentLength, r)
}

func (s *s3Server) ServeCodeTarball(w http.ResponseWriter, req *http.Request, slug, version, shasum string) error {
	objName := path.Join(slug, version)
	if shasum != "" {
		objName += "-" + shasum
	}
	objName += ".tgz"

	// Try to serve a pre-built tarball first.
	obj, err := s.client.GetObject(s.ctx, s.bucket, objName, minio.GetObjectOptions{})
	if err == nil {
		info, serr := obj.Stat()
		if serr == nil {
			defer obj.Close()
			return serveContent(w, req, info.ContentType, info.Size, obj)
		}
		obj.Close()
	}

	buf, err := prepareTarball(s, slug, version, shasum)
	if err != nil {
		return err
	}
	content, err := io.ReadAll(buf)
	if err != nil {
		return err
	}
	contentType := mime.TypeByExtension(".gz")

	// Store the tarball for future requests.
	_, _ = s.client.PutObject(s.ctx, s.bucket, objName,
		bytes.NewReader(content), int64(len(content)),
		minio.PutObjectOptions{ContentType: contentType})

	return serveContent(w, req, contentType, int64(len(content)), bytes.NewReader(content))
}

func (s *s3Server) makeObjectName(slug, version, shasum, file string) string {
	basepath := path.Join(slug, version)
	if shasum != "" {
		basepath += "-" + shasum
	}
	// Prevent path traversal
	if strings.Contains(file, "..") {
		return basepath + "/invalid"
	}
	return path.Join(basepath, file)
}

func (s *s3Server) FilesList(slug, version, shasum string) ([]string, error) {
	prefix := s.makeObjectName(slug, version, shasum, "") + "/"
	var names []string
	for obj := range s.client.ListObjects(s.ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		name := strings.TrimPrefix(obj.Key, prefix)
		if name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// S3AppsBucket returns the S3 bucket name used for storing applications of a
// given type. The bucket is shared across all instances (like Swift containers).
func S3AppsBucket(bucketPrefix string, appsType consts.AppType) string {
	switch appsType {
	case consts.WebappType:
		return bucketPrefix + "-apps-web"
	case consts.KonnectorType:
		return bucketPrefix + "-apps-konnectors"
	}
	panic("Unknown AppType")
}

// prepareTarball is reused from server.go via the FileServer interface (it
// calls Open and FilesList). The function is defined in server.go.
// We reference it here to document that s3Server satisfies prepareTarball's
// requirements.
var _ FileServer = (*s3Server)(nil)
