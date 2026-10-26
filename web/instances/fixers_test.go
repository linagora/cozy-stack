package instances

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestOrgContactsFixerRequiresSnapshot(t *testing.T) {
	for _, tc := range []struct{ name, count, body string }{
		{name: "missing"},
		{name: "negative", count: "-1"},
		{name: "not a number", count: "invalid"},
		{name: "missing body", count: "0"},
		{name: "missing paths", count: "0", body: "{}"},
		{name: "null paths", count: "0", body: `{"paths":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/?since=2026-10-07T08:00:00Z&expected_contacts="+tc.count, strings.NewReader(tc.body))
			c := echo.New().NewContext(req, httptest.NewRecorder())
			var httpErr *echo.HTTPError
			require.ErrorAs(t, orgContactsFixer(c), &httpErr)
			require.Equal(t, http.StatusBadRequest, httpErr.Code)
		})
	}
}
