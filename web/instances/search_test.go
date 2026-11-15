package instances

import (
	"errors"
	"net/http"
	"testing"

	"github.com/cozy/cozy-stack/model/search"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
)

func TestSearchError(t *testing.T) {
	var httpErr *echo.HTTPError
	assert.True(t, errors.As(searchError(search.ErrNotConfigured), &httpErr))
	assert.Equal(t, http.StatusServiceUnavailable, httpErr.Code)

	other := errors.New("boom")
	assert.Equal(t, other, searchError(other))
}
