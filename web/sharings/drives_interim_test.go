package sharings

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOwnerUploadsCache(t *testing.T) {
	cache := newOwnerUploadsCache()
	now := time.Now()
	cache.put("a", true, now)
	accepted, ok := cache.get("a", now.Add(ownerUploadsTTL-time.Second))
	assert.True(t, ok)
	assert.True(t, accepted)
	_, ok = cache.get("a", now.Add(ownerUploadsTTL+time.Second))
	assert.False(t, ok)

	// An entry that has expired is removed by the next one.
	cache.put("b", false, now.Add(ownerUploadsTTL+time.Second))
	assert.Len(t, cache.entries, 1)
}

func TestRelayInterimResponse(t *testing.T) {
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/sharings/drives/x/uploads/y")
		w.WriteHeader(104)
		w.Header().Del("Location")
		w.Header().Set(echo.HeaderAccessControlAllowOrigin, r.Header.Get(echo.HeaderOrigin))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	}))
	t.Cleanup(owner.Close)
	target, err := url.Parse(owner.URL)
	require.NoError(t, err)

	relay := func(wrap bool) (int, string, http.Header, string) {
		e := echo.New()
		e.POST("/", func(c echo.Context) error {
			var w http.ResponseWriter = c.Response()
			if wrap {
				w = interimWriter{c.Response()}
			}
			httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, c.Request())
			return nil
		})
		member := httptest.NewServer(e)
		defer member.Close()

		var interim string
		trace := &httptrace.ClientTrace{
			Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
				interim = header.Get("Location")
				return nil
			},
		}
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace),
			http.MethodPost, member.URL, nil)
		require.NoError(t, err)
		req.Header.Set(echo.HeaderOrigin, "https://drive.example")
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		return res.StatusCode, interim, res.Header, string(body)
	}

	status, interim, header, body := relay(true)
	assert.Equal(t, http.StatusCreated, status)
	assert.Equal(t, "/sharings/drives/x/uploads/y", interim)
	assert.Equal(t, "https://drive.example", header.Get(echo.HeaderAccessControlAllowOrigin))
	assert.Empty(t, header.Get("Location"))
	assert.Equal(t, "created", body)

	// Without it, echo takes the 104 for the final status.
	status, interim, _, _ = relay(false)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "/sharings/drives/x/uploads/y", interim)
}
