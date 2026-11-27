package files

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

// refuseResumableUpload answers 501 to a request without the content
// (Upload-Complete: ?0) where only the usual upload is possible: it would
// create an empty file. It returns true when the response has been written.
func refuseResumableUpload(c echo.Context) (bool, error) {
	complete, ok := parseSFBoolean(c.Request().Header.Get(headerUploadComplete))
	if !ok || complete {
		return false, nil
	}
	return true, uploadProblem(c, http.StatusNotImplemented, "about:blank", "Resumable upload unavailable",
		"The upload cannot be resumable here: send the whole content in one request.", nil)
}

// startUpload handles a request that creates a file (olddoc is nil) or
// replaces its content, when it asks for a resumable upload. It returns false
// when the request must go through the usual upload. The returned document is
// nil when the response has already been written.
func startUpload(c echo.Context, fs vfs.VFS, newdoc, olddoc *vfs.FileDoc, oldRev string) (bool, *vfs.FileDoc, error) {
	req := c.Request()
	complete, ok := parseSFBoolean(req.Header.Get(headerUploadComplete))
	if !ok {
		return false, nil, nil
	}
	interop := interopVersion(c)
	if interop == "" || !vfs.SupportsUploads(fs) {
		refused, err := refuseResumableUpload(c)
		return refused, nil, err
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

	newUpload := vfs.NewUpload
	if perm, err := middlewares.GetPermission(c); err == nil && perm.Type == permission.TypeShareByLink {
		newUpload = vfs.NewUploadByLink
	}
	u, err := newUpload(fs, newdoc, olddoc, oldRev, length)
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
