package appfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/filetype"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/ncw/swift/v2"
	"github.com/spf13/afero"
)

// Copier is an interface defining a common set of functions for the installer
// to copy the application into an unknown storage.
type Copier interface {
	Exist(slug, version, shasum string) (exists bool, err error)
	Start(slug, version, shasum string) (exists bool, err error)
	Copy(stat os.FileInfo, src io.Reader) error
	Abort() error
	Commit() error
}

type swiftCopier struct {
	c           *swift.Connection
	appObj      string
	tmpObj      string
	container   string
	started     bool
	objectNames []string
	ctx         context.Context
}

type aferoCopier struct {
	fs      afero.Fs
	appDir  string
	tmpDir  string
	started bool
}

// NewSwiftCopier defines a Copier storing data into a swift container.
func NewSwiftCopier(conn *swift.Connection, appsType consts.AppType) Copier {
	return &swiftCopier{
		c:         conn,
		container: containerName(appsType),
		ctx:       context.Background(),
	}
}

func (f *swiftCopier) Exist(slug, version, shasum string) (bool, error) {
	f.appObj = path.Join(slug, version)
	if shasum != "" {
		f.appObj += "-" + shasum
	}
	_, _, err := f.c.Object(f.ctx, f.container, f.appObj)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, swift.ObjectNotFound) {
		return false, err
	}
	return false, nil
}

func (f *swiftCopier) Start(slug, version, shasum string) (bool, error) {
	exist, err := f.Exist(slug, version, shasum)
	if err != nil || exist {
		return exist, err
	}

	if _, _, err = f.c.Container(f.ctx, f.container); errors.Is(err, swift.ContainerNotFound) {
		if err = f.c.ContainerCreate(f.ctx, f.container, nil); err != nil {
			return false, err
		}
	}
	f.tmpObj = "tmp-" + utils.RandomString(20) + "/"
	f.objectNames = []string{}
	f.started = true
	return false, err
}

func (f *swiftCopier) Copy(stat os.FileInfo, src io.Reader) (err error) {
	if !f.started {
		panic("copier should call Start() before Copy()")
	}

	name, err := CleanFileName(stat.Name())
	if err != nil {
		return err
	}
	objName := path.Join(f.tmpObj, name)
	objMeta := swift.Metadata{
		"content-encoding":        "br",
		"original-content-length": strconv.FormatInt(stat.Size(), 10),
	}

	contentType := filetype.ByExtension(path.Ext(stat.Name()))
	if contentType == "" {
		contentType, src = filetype.FromReader(src)
	}

	f.objectNames = append(f.objectNames, objName)
	file, err := f.c.ObjectCreate(f.ctx, f.container, objName, true, "",
		contentType, objMeta.ObjectHeaders())
	if err != nil {
		return err
	}
	defer func() {
		if errc := file.Close(); errc != nil {
			err = errc
		}
	}()

	bw := brotli.NewWriter(file)
	_, err = io.Copy(bw, src)
	if errc := bw.Close(); errc != nil && err == nil {
		err = errc
	}
	return err
}

func (f *swiftCopier) Abort() error {
	_, err := f.c.BulkDelete(f.ctx, f.container, f.objectNames)
	return err
}

func (f *swiftCopier) Commit() (err error) {
	defer func() {
		_, errc := f.c.BulkDelete(f.ctx, f.container, f.objectNames)
		if errc != nil {
			logger.WithNamespace("appfs").Errorf("Cannot BulkDelete after commit: %s", errc)
		}
	}()
	// We check if the appObj has not been created concurrently by another
	// copier.
	_, _, err = f.c.Object(f.ctx, f.container, f.appObj)
	if err == nil {
		return nil
	}
	for _, srcObjectName := range f.objectNames {
		dstObjectName := path.Join(f.appObj, strings.TrimPrefix(srcObjectName, f.tmpObj))
		_, err = f.c.ObjectCopy(f.ctx, f.container, srcObjectName, f.container, dstObjectName, nil)
		if err != nil {
			logger.WithNamespace("appfs").Errorf("Cannot copy file: %s", err)
			return err
		}
	}
	return f.c.ObjectPutString(f.ctx, f.container, f.appObj, "", "text/plain")
}

// NewAferoCopier defines a copier using an afero.Fs filesystem to store the
// application data.
func NewAferoCopier(fs afero.Fs) Copier {
	return &aferoCopier{fs: fs}
}

func (f *aferoCopier) Exist(slug, version, shasum string) (bool, error) {
	appDir := path.Join("/", slug, version)
	if shasum != "" {
		appDir += "-" + shasum
	}
	return afero.DirExists(f.fs, appDir)
}

func (f *aferoCopier) Start(slug, version, shasum string) (bool, error) {
	f.appDir = path.Join("/", slug, version)
	if shasum != "" {
		f.appDir += "-" + shasum
	}
	exists, err := afero.DirExists(f.fs, f.appDir)
	if err != nil || exists {
		return exists, err
	}
	dir := path.Dir(f.appDir)
	if err = f.fs.MkdirAll(dir, 0755); err != nil {
		return false, err
	}
	f.tmpDir, err = afero.TempDir(f.fs, dir, "tmp")
	if err != nil {
		return false, err
	}
	f.started = true
	return false, nil
}

func (f *aferoCopier) Copy(stat os.FileInfo, src io.Reader) (err error) {
	if !f.started {
		panic("copier should call Start() before Copy()")
	}

	name, err := CleanFileName(stat.Name())
	if err != nil {
		return err
	}
	fullpath := path.Join(f.tmpDir, name) + ".br"
	dir := path.Dir(fullpath)
	if err = f.fs.MkdirAll(dir, 0755); err != nil {
		return err
	}

	dst, err := f.fs.Create(fullpath)
	if err != nil {
		return err
	}
	defer func() {
		if errc := dst.Close(); errc != nil {
			err = errc
		}
	}()

	bw := brotli.NewWriter(dst)
	_, err = io.Copy(bw, src)
	if errc := bw.Close(); errc != nil && err == nil {
		err = errc
	}
	return err
}

func (f *aferoCopier) Commit() error {
	return f.fs.Rename(f.tmpDir, f.appDir)
}

func (f *aferoCopier) Abort() error {
	return f.fs.RemoveAll(f.tmpDir)
}

// NewFileInfo returns an os.FileInfo
func NewFileInfo(name string, size int64, mode os.FileMode) os.FileInfo {
	return &fileInfo{
		name: name,
		size: size,
		mode: mode,
	}
}

type fileInfo struct {
	name string
	size int64
	mode os.FileMode
}

func (f *fileInfo) Name() string       { return f.name }
func (f *fileInfo) Size() int64        { return f.size }
func (f *fileInfo) Mode() os.FileMode  { return f.mode }
func (f *fileInfo) ModTime() time.Time { return time.Now() }
func (f *fileInfo) IsDir() bool        { return false }
func (f *fileInfo) Sys() interface{}   { return nil }

var ErrInvalidFileName = errors.New("appfs: invalid file name")

func CleanFileName(name string) (string, error) {
	for _, segment := range strings.Split(name, "/") {
		if segment == ".." {
			return "", fmt.Errorf("%w: %q", ErrInvalidFileName, name)
		}
	}
	clean := strings.TrimPrefix(path.Clean("/"+name), "/")
	if clean == "" {
		return "", fmt.Errorf("%w: %q", ErrInvalidFileName, name)
	}
	return clean, nil
}
