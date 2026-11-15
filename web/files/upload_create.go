package files

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

// startUpload handles a request that creates a file (olddoc is nil) or
// replaces its content, when it asks for a resumable upload. It returns false
// when the request must go through the usual upload. The returned document is
// nil when the response has already been written.
func startUpload(c echo.Context, fs vfs.VFS, newdoc, olddoc *vfs.FileDoc, oldRev string) (bool, *vfs.FileDoc, error) {
	interop := interopVersion(c)
	if interop == "" || !vfs.SupportsUploads(fs) {
		return false, nil, nil
	}
	req := c.Request()
	complete, ok := parseSFBoolean(req.Header.Get(headerUploadComplete))
	if !ok {
		return false, nil, nil
	}

	length := int64(-1)
	if v := req.Header.Get(headerUploadLength); v != "" {
		if length = parseSFInteger(v); length < 0 {
			return true, nil, uploadError(c, vfs.ErrUploadLength)
		}
	}
	if complete && req.ContentLength >= 0 {
		if length >= 0 && length != req.ContentLength {
			return true, nil, uploadError(c, vfs.ErrUploadLength)
		}
		length = req.ContentLength
	}
	// A content sent in one request that fits in one part takes the usual way.
	if complete && length >= 0 && length <= vfs.UploadPartSize() {
		return false, nil, nil
	}
	newdoc.ByteSize = length
	exposeUploadHeaders(c)

	u, err := vfs.NewUpload(fs, newdoc, olddoc, oldRev, length)
	if errors.Is(err, vfs.ErrTooManyUploads) && complete {
		newdoc.ByteSize = req.ContentLength
		return false, nil, nil
	}
	if err != nil {
		return true, nil, uploadError(c, err)
	}
	inst := middlewares.GetInstance(c)
	job.EnsureCleanUploadsTrigger(inst)

	sendInterimResponse(c, fs, u, interop)
	created, err := vfs.AppendUpload(fs, u, req.Body, complete)
	if err != nil {
		inst.Logger().WithNamespace("files").
			Infof("Resumable upload %s stopped at %d bytes: %s", u.UploadID, u.Offset, err)
		return true, nil, uploadError(c, err)
	}
	c.Response().Header().Set(headerInteropVersion, interop)
	c.Response().Header().Set(headerUploadComplete, sfBoolean(complete))
	if complete {
		return true, created, nil
	}
	c.Response().Header().Set(echo.HeaderLocation, uploadLocation(u))
	c.Response().Header().Set(headerUploadOffset, strconv.FormatInt(u.Offset, 10))
	setUploadLimit(c, fs, u, interop)
	return true, nil, c.NoContent(http.StatusCreated)
}
