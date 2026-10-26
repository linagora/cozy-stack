package instances

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestOrgContactsFixerRequiresExpectedCount(t *testing.T) {
	for _, tc := range []struct{ name, count string }{
		{name: "missing"},
		{name: "negative", count: "-1"},
		{name: "not a number", count: "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/?since=2026-10-07T08:00:00Z&expected_contacts="+tc.count, nil)
			c := echo.New().NewContext(req, httptest.NewRecorder())
			var httpErr *echo.HTTPError
			require.ErrorAs(t, orgContactsFixer(c), &httpErr)
			require.Equal(t, http.StatusBadRequest, httpErr.Code)
		})
	}
}
