package publiclink

import (
	"net/http"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

// rootID accepts only a single, explicit file target.
func rootID(p *permission.Permission) string {
	if p == nil || p.Type != permission.TypeShareByLink {
		return ""
	}
	var id string
	for _, rule := range p.Permissions {
		if rule.Type != consts.Files {
			continue
		}
		if (rule.Selector != "" && rule.Selector != "_id") || len(rule.Values) != 1 || id != "" {
			return ""
		}
		id = rule.Values[0]
	}
	return id
}

// TrackPreparedFile records the intent once the download secret has been created.
// Consuming or reusing the secret must not record another event.
func TrackPreparedFile(c echo.Context, inst *instance.Instance, id string) {
	p, _ := middlewares.GetPermission(c)
	if rootID(p) == id {
		Record(c, inst, p, publiclink.Downloaded)
	}
}

// TrackFile records the start of an attachment download for the shared root.
func TrackFile(c echo.Context, inst *instance.Instance, id string) {
	if c.Param("secret") != "" || c.Request().Method != http.MethodGet || c.QueryParam("Dl") != "1" || c.QueryParam("Page") != "" || c.Request().Header.Get("Range") != "" {
		return
	}
	p, _ := middlewares.GetPermission(c)
	if rootID(p) == id {
		Record(c, inst, p, publiclink.Downloaded)
	}
}

// TrackArchive accepts exactly one whole shared directory, including an empty one.
func TrackArchive(c echo.Context, inst *instance.Instance, entries []vfs.ArchiveEntry, pages []vfs.Page) {
	p, _ := middlewares.GetPermission(c)
	if c.Request().Method == http.MethodHead || c.Request().Header.Get("Range") != "" || len(pages) != 0 || len(entries) != 1 || entries[0].Dir == nil || rootID(p) != entries[0].Dir.ID() {
		return
	}
	Record(c, inst, p, publiclink.Downloaded)
}
