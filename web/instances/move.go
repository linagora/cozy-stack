package instances

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/move"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/labstack/echo/v4"
)

func exporter(c echo.Context) error {
	domain := c.Param("domain")
	adminReq, err := strconv.ParseBool(c.QueryParam("admin-req"))
	if err != nil {
		return wrapError(err)
	}

	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return wrapError(err)
	}

	options := move.ExportOptions{
		ContextualDomain: domain,
		AdminReq:         adminReq,
	}
	msg, err := job.NewMessage(options)
	if err != nil {
		return wrapError(err)
	}

	j, err := job.System().PushJob(inst, &job.JobRequest{
		WorkerType: "export",
		Message:    msg,
	})
	if err != nil {
		return wrapError(err)
	}

	return c.JSON(http.StatusAccepted, j)
}

func dataExporter(c echo.Context) error {
	domain := c.Param("domain")
	exportID := c.Param("export-id")

	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return wrapError(err)
	}

	var exportDoc move.ExportDoc
	if err := couchdb.GetDoc(prefixer.GlobalPrefixer, consts.Exports, exportID, &exportDoc); err != nil {
		if couchdb.IsNotFoundError(err) || couchdb.IsNoDatabaseError(err) {
			return wrapError(move.ErrExportNotFound)
		}
		return wrapError(err)
	}
	if exportDoc.HasExpired() {
		return wrapError(move.ErrExportExpired)
	}

	cursor, err := move.ParseCursor(&exportDoc, c.QueryParam("cursor"))
	if err != nil {
		return wrapError(err)
	}

	w := c.Response()
	w.Header().Set(echo.HeaderContentType, "application/zip")
	filename := domain + ".zip"
	if len(exportDoc.PartsCursors) > 0 {
		filename = fmt.Sprintf("%s - part%03d.zip", domain, cursor.Number)
	}
	w.Header().Set(echo.HeaderContentDisposition, fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.WriteHeader(http.StatusOK)

	archiver := move.SystemArchiver()
	return move.ExportCopyData(w, inst, &exportDoc, archiver, cursor)
}

func importer(c echo.Context) error {
	domain := c.Param("domain")
	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return wrapError(err)
	}

	err = move.ScheduleImportFromManifest(c.Request().Context(), inst, move.ImportOptions{
		ManifestURL: c.QueryParam("manifest_url"),
	})
	if err != nil {
		return wrapError(err)
	}

	return c.NoContent(http.StatusNoContent)
}
