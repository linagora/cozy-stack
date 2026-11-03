package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/pkg/appfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/labstack/echo/v4"
)

var httpClient = http.Client{
	Timeout: 60 * time.Second,
}

type httpFetcher struct {
	manFilename string
	prefix      string
	log         logger.Logger
}

func newHTTPFetcher(manFilename string, log logger.Logger) *httpFetcher {
	return &httpFetcher{
		manFilename: manFilename,
		log:         log,
	}
}

func (f *httpFetcher) FetchManifest(src *url.URL) (r io.ReadCloser, err error) {
	req, err := http.NewRequest(http.MethodGet, src.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			// Flush the body, so that the connecion can be reused by keep-alive
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	if resp.StatusCode != 200 {
		return nil, ErrManifestNotReachable
	}

	var reader io.Reader = resp.Body

	contentType := resp.Header.Get(echo.HeaderContentType)
	switch contentType {
	case
		"application/gzip",
		"application/x-gzip",
		"application/x-tgz",
		"application/tar+gzip":
		reader, err = gzip.NewReader(reader)
		if err != nil {
			return nil, ErrManifestNotReachable
		}
	}

	tarReader := tar.NewReader(reader)
	for {
		hdr, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil, ErrManifestNotReachable
		}
		if err != nil {
			return nil, ErrManifestNotReachable
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		baseName := path.Base(hdr.Name)
		if baseName != f.manFilename {
			continue
		}
		if baseName != hdr.Name {
			f.prefix = path.Dir(hdr.Name) + "/"
		}
		return utils.ReadCloser(tarReader, func() error {
			return resp.Body.Close()
		}), nil
	}
}

func (f *httpFetcher) Fetch(src *url.URL, fs appfs.Copier, man Manifest) (err error) {
	var shasum []byte
	if frag := src.Fragment; frag != "" {
		shasum, _ = hex.DecodeString(frag)
	}
	return fetchHTTP(src, shasum, fs, man, f.prefix)
}

func fetchHTTP(src *url.URL, shasum []byte, fs appfs.Copier, man Manifest, prefix string) (err error) {
	// Happy path: it exists and we don't need to acquire the lock.
	exists, err := fs.Exist(man.Slug(), man.Version(), man.Checksum())
	if err != nil || exists {
		return err
	}

	// For the lock key, we use the checksum when available, and else, we
	// fallback on the app name.
	mu := config.Lock().ReadWrite(prefixer.GlobalPrefixer, "app-"+man.Slug()+"-"+man.Checksum())
	if err = mu.Lock(); err != nil {
		log := logger.WithNamespace("fetcher")
		log.Infof("cannot get lock: %s", err)
		return err
	}
	defer mu.Unlock()

	// We need to check again if it exists, as the app can have been installed
	// while we were waiting for the lock.
	exists, err = fs.Start(man.Slug(), man.Version(), man.Checksum())
	if err != nil || exists {
		return err
	}
	defer func() {
		if err != nil {
			_ = fs.Abort()
		} else {
			err = fs.Commit()
		}
	}()

	req, err := http.NewRequest(http.MethodGet, src.String(), nil)
	if err != nil {
		return err
	}
	start := time.Now()
	resp, err := httpClient.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		log := logger.WithNamespace("fetcher")
		log.Infof("cannot fetch %s: %s", src.String(), err)
		return err
	}
	defer resp.Body.Close()
	if elapsed.Seconds() >= 10 {
		log := logger.WithNamespace("fetcher")
		log.Infof("slow request on %s (%s)", src.String(), elapsed)
	}
	if resp.StatusCode != 200 {
		return ErrSourceNotReachable
	}

	var reader io.Reader = resp.Body
	var h hash.Hash

	if len(shasum) > 0 {
		h = sha256.New()
		reader = io.TeeReader(reader, h)
	}

	contentType := resp.Header.Get(echo.HeaderContentType)
	switch contentType {
	case
		"application/gzip",
		"application/x-gzip",
		"application/x-tgz",
		"application/tar+gzip":
		reader, err = gzip.NewReader(reader)
		if err != nil {
			return err
		}
	case "application/octet-stream":
		if r, err := gzip.NewReader(reader); err == nil {
			reader = r
		}
	}

	tarReader := tar.NewReader(reader)
	for {
		hdr, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := hdr.Name
		if len(prefix) > 0 && strings.HasPrefix(path.Join("/", name), path.Join("/", prefix)) {
			name = name[len(prefix):]
		}
		fileinfo := appfs.NewFileInfo(name, hdr.Size, os.FileMode(hdr.Mode))
		err = fs.Copy(fileinfo, tarReader)
		if err != nil {
			return err
		}
	}
	if len(shasum) > 0 && !bytes.Equal(shasum, h.Sum(nil)) {
		return ErrBadChecksum
	}
	return nil
}
