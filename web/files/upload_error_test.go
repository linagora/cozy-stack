package files

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
)

func TestUploadErrorComplete(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		status   int
		complete string
	}{
		{"resumed elsewhere", vfs.ErrUploadTakenOver, http.StatusConflict, "?0"},
		{"storage error", errors.New("swift: 503 Service Unavailable"), http.StatusInternalServerError, "?0"},
		{"lost connection", io.ErrUnexpectedEOF, http.StatusInternalServerError, "?0"},
		{"wrong md5", &vfs.RejectedUploadError{Err: vfs.ErrInvalidHash}, http.StatusPreconditionFailed, "?1"},
		{"name taken", &vfs.RejectedUploadError{Err: errors.Join(vfs.ErrConflict, errors.New("cleanup failed"))}, http.StatusConflict, "?1"},
		{"wrong length", &vfs.RejectedUploadError{Err: vfs.ErrUploadLength}, http.StatusBadRequest, "?1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			rec := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequest(http.MethodPatch, "/files/uploads/x", nil), rec)
			err := uploadError(c, tc.err)
			status := rec.Code
			var jerr *jsonapi.Error
			if errors.As(err, &jerr) {
				status = jerr.Status
			} else if err != nil {
				// echo answers 500 for an untyped error.
				status = http.StatusInternalServerError
			}
			assert.Equal(t, tc.status, status)
			assert.Equal(t, tc.complete, rec.Header().Get(headerUploadComplete))
		})
	}
}
