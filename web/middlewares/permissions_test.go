package middlewares

import (
	"net/http/httptest"
	"testing"

	"github.com/cozy/cozy-stack/model/permission"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
)

func TestAllowInstallAppFromFile(t *testing.T) {
	oldMode := build.BuildMode
	build.BuildMode = build.ModeProd
	t.Cleanup(func() { build.BuildMode = oldMode })

	maximal := permission.Set{permission.Rule{Type: "*"}}
	tests := []struct {
		name    string
		pdoc    *permission.Permission
		source  string
		allowed bool
	}{
		{"maximal token from file", &permission.Permission{Type: permission.TypeOauth, Permissions: maximal}, "file:///var/lib/cozy", false},
		{"maximal token from FILE", &permission.Permission{Type: permission.TypeOauth, Permissions: maximal}, "FILE:///var/lib/cozy", false},
		{"maximal token from registry", &permission.Permission{Type: permission.TypeOauth, Permissions: maximal}, "registry://drive", true},
		{"CLI from file", &permission.Permission{Type: permission.TypeCLI, Permissions: maximal}, "file:///var/lib/cozy", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := echo.New().NewContext(httptest.NewRequest("POST", "/", nil), httptest.NewRecorder())
			c.Set(contextPermissionDoc, tt.pdoc)
			err := AllowInstallApp(c, consts.WebappType, tt.source, permission.POST)
			if tt.allowed {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, ErrForbidden)
			}
		})
	}
}
