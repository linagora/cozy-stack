package instances

import (
	"errors"
	"net/http"

	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/search"
	"github.com/labstack/echo/v4"
)

// searchEnable turns search on for an instance: indices, trigger, and the
// first indexing job.
func searchEnable(c echo.Context) error {
	inst, err := lifecycle.GetInstance(c.Param("domain"))
	if err != nil {
		return err
	}
	if err := search.Enable(c.Request().Context(), inst); err != nil {
		return searchError(err)
	}
	return c.NoContent(http.StatusNoContent)
}

// searchReset drops the search documents and checkpoint of an instance, and
// indexes it again.
func searchReset(c echo.Context) error {
	inst, err := lifecycle.GetInstance(c.Param("domain"))
	if err != nil {
		return err
	}
	if err := search.Reset(c.Request().Context(), inst); err != nil {
		return searchError(err)
	}
	return c.NoContent(http.StatusNoContent)
}

// searchStatus returns the indexing status of an instance.
func searchStatus(c echo.Context) error {
	inst, err := lifecycle.GetInstance(c.Param("domain"))
	if err != nil {
		return err
	}
	status, err := search.GetStatus(inst)
	if err != nil {
		return searchError(err)
	}
	return c.JSON(http.StatusOK, status)
}

func searchError(err error) error {
	if errors.Is(err, search.ErrNotConfigured) {
		return echo.NewHTTPError(http.StatusServiceUnavailable, err.Error())
	}
	return err
}
