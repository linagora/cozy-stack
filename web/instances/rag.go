package instances

import (
	"errors"
	"net/http"

	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/rag"
	"github.com/labstack/echo/v4"
)

// ragReset drops the rag-index checkpoint(s) and relaunches the trigger(s).
func ragReset(c echo.Context) error {
	inst, err := lifecycle.GetInstance(c.Param("domain"))
	if err != nil {
		return err
	}
	n, err := rag.Reset(inst, c.QueryParam("dir_id"))
	if errors.Is(err, rag.ErrNoIndexTrigger) {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, echo.Map{"triggers": n})
}

// ragPurge deletes from openRAG the files no rag-index trigger claims.
func ragPurge(c echo.Context) error {
	inst, err := lifecycle.GetInstance(c.Param("domain"))
	if err != nil {
		return err
	}
	res, err := rag.Purge(inst, inst.Logger().WithNamespace("rag"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, res)
}
