package errors

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/pkg/appfs"
	"github.com/cozy/cozy-stack/pkg/lock"
	"github.com/labstack/echo/v4"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func handle(t *testing.T, err error) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/files/", nil)
	rec := httptest.NewRecorder()
	ErrorHandler(err, echo.New().NewContext(req, rec))
	return rec
}

func TestErrorHandler(t *testing.T) {
	t.Run("LockTimeoutIsServiceUnavailable", func(t *testing.T) {
		for _, err := range []error{
			lock.ErrTooManyRetries,
			fmt.Errorf("create file: %w", lock.ErrTooManyRetries),
		} {
			rec := handle(t, err)

			assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
			assert.Equal(t, lockRetryAfter, rec.Header().Get(echo.HeaderRetryAfter))

			var body struct {
				Errors []struct {
					Status string `json:"status"`
					Detail string `json:"detail"`
				} `json:"errors"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Len(t, body.Errors, 1)
			assert.Equal(t, "503", body.Errors[0].Status)
			assert.Equal(t, errInstanceBusy.Error(), body.Errors[0].Detail)
		}
	})

	t.Run("UnqualifiedErrorIsInternalServerError", func(t *testing.T) {
		rec := handle(t, fmt.Errorf("boom"))

		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Empty(t, rec.Header().Get(echo.HeaderRetryAfter))
	})
}

func TestHTMLErrorHandler(t *testing.T) {
	t.Run("ClientDisconnectDoesNotExceedContentLength", func(t *testing.T) {
		hook := logtest.NewGlobal()

		// ServeFileContent sends the body in a single Write, which net/http counts
		// in full even if it fails: any error page then exceeds the Content-Length.
		// The body is larger than the socket buffers, so that Write blocks until
		// the client resets the connection.
		fs := afero.NewMemMapFs()
		require.NoError(t, afero.WriteFile(fs, "/app/1.0.0/app.js", make([]byte, 32<<20), 0o644))
		files := appfs.NewAferoFileServer(fs, nil)

		committed := make(chan bool, 1)
		e := echo.New()
		e.HTTPErrorHandler = func(err error, c echo.Context) {
			HTMLErrorHandler(err, c)
			committed <- c.Response().Committed
		}
		e.GET("/*", func(c echo.Context) error {
			err := files.ServeFileContent(c.Response(), c.Request(), "app", "1.0.0", "", "app.js")
			if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, err)
			}
			return nil
		})
		srv := httptest.NewServer(e)
		defer srv.Close()

		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		require.NoError(t, err)
		defer conn.Close()
		_, err = fmt.Fprint(conn, "GET /app.js HTTP/1.1\r\nHost: localhost\r\n\r\n")
		require.NoError(t, err)
		status, err := bufio.NewReader(conn).ReadString('\n')
		require.NoError(t, err)
		require.Contains(t, status, "200")

		require.NoError(t, conn.(*net.TCPConn).SetLinger(0))
		require.NoError(t, conn.Close())

		select {
		case isCommitted := <-committed:
			require.True(t, isCommitted)
		case <-time.After(time.Minute):
			require.FailNow(t, "the write did not fail after the client disconnected")
		}

		for _, entry := range hook.AllEntries() {
			assert.NotContains(t, entry.Message, http.ErrContentLength.Error())
		}
	})
}
