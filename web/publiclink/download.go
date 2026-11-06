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

const downloadPermissionKey = "publiclink.downloadPermission"

func downloadPermission(c echo.Context) *permission.Permission {
	if p, ok := c.Get(downloadPermissionKey).(*permission.Permission); ok {
		return p
	}
	p, _ := middlewares.GetPermission(c)
	return p
}

// AttributeSecret stores only provenance, after the download's access checks.
func AttributeSecret(c echo.Context, inst *instance.Instance, secret string) {
	p, _ := middlewares.GetPermission(c)
	if rootID(p) == "" {
		return
	}
	link := vfs.DownloadLink{PermissionID: p.ID(), RootID: rootID(p)}
	if err := vfs.GetStore().SetDownloadLink(inst, secret, link); err != nil {
		inst.Logger().WithNamespace("publiclink").Error("Could not attribute download")
	}
}

// ResolveSecret never alters the permissions used to authorize a request.
// Old secrets remain usable without producing an unattributed event.
func ResolveSecret(c echo.Context, inst *instance.Instance, secret string) {
	var p *permission.Permission
	if link, err := vfs.GetStore().GetDownloadLink(inst, secret); err == nil && link.PermissionID != "" && link.RootID != "" {
		// Keep the authorization-time target even if the link changes before
		// this existing capability is used. This permission is for attribution only.
		p = &permission.Permission{PID: link.PermissionID, Type: permission.TypeShareByLink,
			Permissions: permission.Set{{Type: consts.Files, Values: []string{link.RootID}}}}
	}
	c.Set(downloadPermissionKey, p)
}

// TrackFile records the start of an attachment download for the shared root.
func TrackFile(c echo.Context, inst *instance.Instance, id string) {
	if c.Request().Method != http.MethodGet || c.QueryParam("Dl") != "1" || c.QueryParam("Page") != "" || c.Request().Header.Get("Range") != "" {
		return
	}
	p := downloadPermission(c)
	if rootID(p) == id {
		Record(c, inst, p, publiclink.Downloaded)
	}
}

// TrackArchive accepts exactly one whole shared directory, including an empty one.
func TrackArchive(c echo.Context, inst *instance.Instance, entries []vfs.ArchiveEntry, pages []vfs.Page) {
	p := downloadPermission(c)
	if c.Request().Method == http.MethodHead || c.Request().Header.Get("Range") != "" || len(pages) != 0 || len(entries) != 1 || entries[0].Dir == nil || rootID(p) != entries[0].Dir.ID() {
		return
	}
	Record(c, inst, p, publiclink.Downloaded)
}
