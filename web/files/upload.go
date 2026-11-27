package files

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

// Resumable uploads, as defined by draft-ietf-httpbis-resumable-upload.
// Interop version 6 is draft -05 (spoken by URLSession on iOS 18.1+ and macOS,
// and tus-js-client), 10 is draft -15.

const (
	headerInteropVersion = "Upload-Draft-Interop-Version"
	headerUploadComplete = "Upload-Complete"
	headerUploadOffset   = "Upload-Offset"
	headerUploadLength   = "Upload-Length"
	headerUploadLimit    = "Upload-Limit"
	partialUploadType    = "application/partial-upload"
	problemType          = "application/problem+json"
)

// errResponseSent is returned when the response has already been written.
var errResponseSent = errors.New("response sent")

// exposeUploadHeaders lets the browsers read the headers of the protocol.
func exposeUploadHeaders(c echo.Context) {
	c.Response().Header().Set(echo.HeaderAccessControlExposeHeaders, strings.Join([]string{
		echo.HeaderLocation, headerInteropVersion, headerUploadComplete,
		headerUploadOffset, headerUploadLength, headerUploadLimit,
	}, ", "))
}

func interopVersion(c echo.Context) string {
	switch v := c.Request().Header.Get(headerInteropVersion); v {
	case "6", "10":
		return v
	}
	return ""
}

// parseSFBoolean parses a Structured Field boolean (RFC 8941), ignoring its
// parameters.
func parseSFBoolean(value string) (bool, bool) {
	value, _, _ = strings.Cut(value, ";")
	switch strings.TrimSpace(value) {
	case "?1":
		return true, true
	case "?0":
		return false, true
	}
	return false, false
}

// parseSFInteger parses a non-negative Structured Field integer, ignoring its
// parameters. It returns -1 when the value is absent or invalid.
func parseSFInteger(value string) int64 {
	value, _, _ = strings.Cut(value, ";")
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

func sfBoolean(b bool) string {
	if b {
		return "?1"
	}
	return "?0"
}

func uploadLocation(u *vfs.Upload) string {
	return "/files/uploads/" + u.UploadID
}

// setUploadLimit announces the limits of the upload. The maximal size comes
// from the space left on the instance: it is not announced to the visitors of
// a sharing by link.
func setUploadLimit(c echo.Context, fs vfs.VFS, u *vfs.Upload, interop string) {
	value := ""
	if perm, err := middlewares.GetPermission(c); err == nil && perm.Type != permission.TypeShareByLink {
		limit, err := vfs.UploadSpaceLimit(fs, u)
		if err != nil {
			return
		}
		if limit < math.MaxInt64 {
			value = fmt.Sprintf("max-size=%d, ", max(limit, 1))
		}
	}
	age := max(int64(time.Until(u.ExpiresAt).Seconds()), 0)
	if interop == "6" {
		value += fmt.Sprintf("expires=%d", age)
	} else {
		value += fmt.Sprintf("max-age=%d", age)
	}
	c.Response().Header().Set(headerUploadLimit, value)
}

// sendInterimResponse announces the upload resource with a 104 (Upload
// Resumption Supported) interim response. It is written on the underlying
// writer, as echo would consider the response as sent.
func sendInterimResponse(c echo.Context, fs vfs.VFS, u *vfs.Upload, interop string) {
	h := c.Response().Header()
	h.Set(headerInteropVersion, interop)
	h.Set(echo.HeaderLocation, uploadLocation(u))
	setUploadLimit(c, fs, u, interop)
	c.Response().Writer.WriteHeader(104)
	h.Del(echo.HeaderLocation)
	h.Del(headerUploadLimit)
}

func uploadProblem(c echo.Context, status int, kind, title, detail string, extra map[string]interface{}) error {
	body := map[string]interface{}{
		"type":   kind,
		"title":  title,
		"status": status,
		"detail": detail,
	}
	for k, v := range extra {
		body[k] = v
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return c.Blob(status, problemType, data)
}

func uploadNotFound(c echo.Context) error {
	return uploadProblem(c, http.StatusNotFound, "about:blank", "Upload not found",
		"This upload does not exist: it has expired, it has been cancelled, or it has never existed. Start a new upload.", nil)
}

// uploadError writes the response for an error on a resumable upload.
//
// Upload-Complete is ?1 only when the upload has been rejected and removed:
// after the other errors (another request has resumed the upload, storage
// error, lost connection), the client must resume it
// (draft-ietf-httpbis-resumable-upload §4.2.1 and §11).
func uploadError(c echo.Context, err error) error {
	if errors.Is(err, errUploadHidden) {
		return c.NoContent(http.StatusNotFound)
	}
	if errors.Is(err, vfs.ErrUploadNotFound) {
		return uploadNotFound(c)
	}
	var rejected *vfs.RejectedUploadError
	complete := errors.As(err, &rejected)
	c.Response().Header().Set(headerUploadComplete, sfBoolean(complete))
	switch {
	case errors.Is(err, vfs.ErrUploadTakenOver):
		return uploadProblem(c, http.StatusConflict, "about:blank", "Upload resumed elsewhere",
			"Another request has resumed this upload. Retrieve the offset to continue.", nil)
	case errors.Is(err, vfs.ErrUploadLength):
		return uploadProblem(c, http.StatusBadRequest,
			"https://iana.org/assignments/http-problem-types#inconsistent-upload-length",
			"Inconsistent length values for the upload",
			"The content does not match the announced length. The upload has been cancelled.", nil)
	case errors.Is(err, vfs.ErrTooManyUploads):
		return uploadProblem(c, http.StatusTooManyRequests, "about:blank", "Too many uploads",
			"Too many uploads are in progress for this instance. Retry later.", nil)
	}
	for _, cause := range []error{
		os.ErrExist, os.ErrNotExist, vfs.ErrConflict, vfs.ErrFileTooBig, vfs.ErrMaxFileSize,
		vfs.ErrParentDoesNotExist, vfs.ErrParentInTrash, vfs.ErrIllegalFilename,
		vfs.ErrInvalidHash, vfs.ErrContentLengthMismatch,
	} {
		if errors.Is(err, cause) {
			return WrapVfsError(cause)
		}
	}
	return WrapVfsError(err)
}

// errUploadHidden is the answer for a completed upload whose file is out of
// the reach of the request: a 404 that tells nothing about it.
var errUploadHidden = errors.New("upload hidden")

func checkUploadPerm(c echo.Context, fs vfs.VFS, u *vfs.Upload) error {
	if u.Completed {
		// The file may have been moved or deleted since: the permissions
		// apply to its current state.
		doc, err := fs.FileByID(u.File.ID())
		if err != nil {
			return errUploadHidden
		}
		verb := permission.POST
		if u.OldFileID != "" {
			verb = permission.PUT
		}
		if checkPerm(c, verb, nil, doc) != nil {
			return errUploadHidden
		}
		return nil
	}
	if u.OldFileID == "" {
		return checkPerm(c, permission.POST, nil, u.File)
	}
	olddoc, err := fs.FileByID(u.OldFileID)
	if err != nil {
		return WrapVfsError(err)
	}
	return checkPerm(c, permission.PUT, nil, olddoc)
}

// respondUploadCompleted writes the response of the request that has
// completed an upload: the same as the request that has created it would have
// written for the whole content.
func respondUploadCompleted(c echo.Context, inst *instance.Instance, u *vfs.Upload, doc *vfs.FileDoc, notify bool) error {
	c.Response().Header().Set(headerUploadComplete, "?1")
	if u.OldFileID != "" {
		return FileData(c, http.StatusOK, doc, true, nil, nil)
	}
	if notify {
		maybeNotifyShareByLinkUpload(c, inst, doc.DocName, doc.ID(), doc.DirID, false)
	}
	return FileData(c, http.StatusCreated, doc, false, nil, nil)
}

// loadUpload loads the upload of the request and checks the permissions.
func loadUpload(c echo.Context) (*instance.Instance, *vfs.Upload, error) {
	exposeUploadHeaders(c)
	inst := middlewares.GetInstance(c)
	if _, err := middlewares.GetPermission(c); err != nil {
		return nil, nil, err
	}
	u, err := vfs.GetUpload(inst.VFS(), c.Param("upload-id"))
	if err != nil {
		return nil, nil, err
	}
	if err := checkUploadPerm(c, inst.VFS(), u); err != nil {
		return nil, nil, err
	}
	return inst, u, nil
}

// UploadOffsetHandler handles HEAD /files/uploads/:upload-id to retrieve the
// offset of an upload.
func UploadOffsetHandler(c echo.Context) error {
	inst, u, err := loadUpload(c)
	if err == nil && !u.Completed {
		u, err = vfs.ClaimUpload(inst.VFS(), u.UploadID)
	}
	if err != nil {
		if errors.Is(err, vfs.ErrUploadNotFound) || errors.Is(err, errUploadHidden) {
			return c.NoContent(http.StatusNotFound)
		}
		return err
	}
	h := c.Response().Header()
	if v := interopVersion(c); v != "" {
		h.Set(headerInteropVersion, v)
	}
	h.Set(headerUploadOffset, strconv.FormatInt(u.Offset, 10))
	h.Set(headerUploadComplete, sfBoolean(u.Completed))
	if u.Length >= 0 {
		h.Set(headerUploadLength, strconv.FormatInt(u.Length, 10))
	}
	if !u.Completed {
		setUploadLimit(c, inst.VFS(), u, interopVersion(c))
	}
	h.Set(echo.HeaderCacheControl, "no-store")
	return c.NoContent(http.StatusNoContent)
}

// UploadAppendHandler handles PATCH /files/uploads/:upload-id to append
// content to an upload.
func UploadAppendHandler(c echo.Context) error {
	req := c.Request()
	interop := interopVersion(c)
	if interop == "" {
		return uploadProblem(c, http.StatusBadRequest, "about:blank", "Unsupported interop version",
			"Upload-Draft-Interop-Version must be 6 or 10.", nil)
	}
	c.Response().Header().Set(headerInteropVersion, interop)
	complete, ok := parseSFBoolean(req.Header.Get(headerUploadComplete))
	if !ok {
		return uploadProblem(c, http.StatusBadRequest, "about:blank", "Missing Upload-Complete",
			"An append request must have the Upload-Complete header.", nil)
	}
	offset := parseSFInteger(req.Header.Get(headerUploadOffset))
	if offset < 0 {
		return uploadProblem(c, http.StatusBadRequest, "about:blank", "Missing Upload-Offset",
			"An append request must have the Upload-Offset header.", nil)
	}
	if ct, _, _ := strings.Cut(req.Header.Get(echo.HeaderContentType), ";"); strings.TrimSpace(ct) != partialUploadType {
		c.Response().Header().Set(headerUploadComplete, "?0")
		return uploadProblem(c, http.StatusUnsupportedMediaType, "about:blank", "Unsupported media type",
			"An append request must have the application/partial-upload content type.", nil)
	}
	if req.Header.Get(echo.HeaderContentEncoding) != "" {
		c.Response().Header().Set(headerUploadComplete, "?0")
		return uploadProblem(c, http.StatusUnsupportedMediaType, "about:blank", "Unsupported content coding",
			"An append request cannot have a content coding.", nil)
	}

	inst, u, err := loadUpload(c)
	if err != nil {
		return uploadError(c, err)
	}
	fs := inst.VFS()
	if u.Completed {
		// The final response may have been lost: send it again for a request
		// that completes the upload, rather than letting the client upload the
		// file a second time (§8.2).
		if !complete || (req.ContentLength >= 0 && offset+req.ContentLength != u.Offset) {
			c.Response().Header().Set(headerUploadComplete, "?0")
			return uploadProblem(c, http.StatusConflict, "about:blank", "Upload already completed",
				"This upload is complete: only the request that has completed it can be sent again.", nil)
		}
		doc, err := fs.FileByID(u.File.ID())
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return uploadProblem(c, http.StatusNotFound, "about:blank", "File not found",
					"This upload is complete, but its file has been deleted since.", nil)
			}
			return WrapVfsError(err)
		}
		return respondUploadCompleted(c, inst, u, doc, false)
	}

	length := parseSFInteger(req.Header.Get(headerUploadLength))
	if complete && req.ContentLength >= 0 {
		if length >= 0 && length != offset+req.ContentLength {
			return uploadError(c, vfs.ErrUploadLength)
		}
		length = offset + req.ContentLength
	}
	if length >= 0 && u.Length >= 0 && length != u.Length {
		return uploadError(c, vfs.ErrUploadLength)
	}

	if u, err = vfs.ClaimUpload(fs, u.UploadID); err != nil {
		return uploadError(c, err)
	}
	if offset != u.Offset {
		h := c.Response().Header()
		h.Set(headerUploadOffset, strconv.FormatInt(u.Offset, 10))
		h.Set(headerUploadComplete, "?0")
		return uploadProblem(c, http.StatusConflict,
			"https://iana.org/assignments/http-problem-types#mismatching-upload-offset",
			"offset from request does not match offset of resource",
			"Retrieve the offset with a HEAD request, and resume from it.",
			map[string]interface{}{"expected-offset": u.Offset, "provided-offset": offset})
	}
	if length >= 0 && u.Length < 0 {
		if err := vfs.SetUploadLength(fs, u, length); err != nil {
			return uploadError(c, err)
		}
	}

	created, err := vfs.AppendUpload(fs, u, req.Body, complete)
	if err != nil {
		inst.Logger().WithNamespace("files").
			Infof("Resumable upload %s stopped at %d bytes: %s", u.UploadID, u.Offset, err)
		return uploadError(c, err)
	}
	if complete {
		return respondUploadCompleted(c, inst, u, created, true)
	}
	h := c.Response().Header()
	h.Set(headerUploadComplete, "?0")
	h.Set(headerUploadOffset, strconv.FormatInt(u.Offset, 10))
	return c.NoContent(http.StatusNoContent)
}

// UploadCancelHandler handles DELETE /files/uploads/:upload-id to cancel an
// upload and free its storage.
func UploadCancelHandler(c echo.Context) error {
	inst, u, err := loadUpload(c)
	if err == nil {
		err = vfs.CancelUpload(inst.VFS(), u.UploadID)
	}
	if err != nil {
		return uploadError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}
