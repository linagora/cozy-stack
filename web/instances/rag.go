package instances

import (
	"errors"
	"net/http"

	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/rag"
	"github.com/labstack/echo/v4"
)

// ragReset drops the rag-index checkpoint and pushes a rag-index job.
func ragReset(c echo.Context) error {
	inst, err := lifecycle.GetInstance(c.Param("domain"))
	if err != nil {
		return err
	}
	if err := rag.Reset(inst); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// ragReconcile pushes reconcile jobs (one folder, or every knowledge base
// folder).
func ragReconcile(c echo.Context) error {
	inst, err := lifecycle.GetInstance(c.Param("domain"))
	if err != nil {
		return err
	}
	n, err := rag.Reconcile(inst, c.QueryParam("dir_id"))
	if errors.Is(err, rag.ErrUnknownFolder) {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, echo.Map{"jobs": n})
}

// ragPrune deletes from openRAG what no knowledge base folder claims.
func ragPrune(c echo.Context) error {
	inst, err := lifecycle.GetInstance(c.Param("domain"))
	if err != nil {
		return err
	}
	res, err := rag.Prune(inst, inst.Logger().WithNamespace("rag"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, res)
}

// ragPurge deletes everything openRAG holds for the instance, and the
// rag-index checkpoint.
func ragPurge(c echo.Context) error {
	inst, err := lifecycle.GetInstance(c.Param("domain"))
	if err != nil {
		return err
	}
	if err := rag.Purge(inst, inst.Logger().WithNamespace("rag")); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
