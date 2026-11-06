package permissions

import (
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

func downloadPublicLinkStatistics(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	current, err := middlewares.GetPermission(c)
	if err != nil {
		return err
	}
	if current.Type != permission.TypeWebapp && current.Type != permission.TypeOauth {
		return middlewares.ErrForbidden
	}
	link, err := permission.GetPermissionByIDIncludingExpired(inst, c.Param("permdocid"))
	if err != nil {
		return err
	}
	if link.Type != permission.TypeShareByLink || len(link.Permissions) == 0 || !link.Permissions.IsSubSetOf(current.Permissions) {
		return middlewares.ErrForbidden
	}
	for _, rule := range link.Permissions {
		if rule.Type != consts.Files || rule.Selector != "" || len(rule.Values) == 0 {
			return middlewares.ErrForbidden
		}
	}
	file, err := os.CreateTemp("", "cozy-link-statistics-*.csv")
	if err != nil {
		return jsonapi.NewError(http.StatusInternalServerError, "Could not generate public link statistics")
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}()
	// Unlink before writing personal data so a crash cannot leave a named report.
	if err := os.Remove(file.Name()); err != nil {
		return jsonapi.NewError(http.StatusInternalServerError, "Could not generate public link statistics")
	}
	if err := publiclink.WriteCSV(c.Request().Context(), file, inst, link.ID()); err != nil {
		return jsonapi.NewError(http.StatusInternalServerError, "Could not generate public link statistics")
	}
	info, err := file.Stat()
	if err != nil {
		return jsonapi.NewError(http.StatusInternalServerError, "Could not generate public link statistics")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return jsonapi.NewError(http.StatusInternalServerError, "Could not generate public link statistics")
	}
	response := c.Response()
	response.Header().Set(echo.HeaderContentDisposition, `attachment; filename="public-link-statistics.csv"`)
	response.Header().Set(echo.HeaderCacheControl, "no-store")
	response.Header().Set(echo.HeaderContentLength, strconv.FormatInt(info.Size(), 10))
	return c.Stream(http.StatusOK, "text/csv; charset=utf-8", file)
}
