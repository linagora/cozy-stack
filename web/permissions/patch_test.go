package permissions

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPatchPermissionRejectsUnownedChanges(t *testing.T) {
	for _, caller := range []string{permission.TypeWebapp, permission.TypeOauth, permission.TypeKonnector,
		permission.TypeShareByLink, permission.TypeSharePreview, permission.TypeShareInteract} {
		for _, targetType := range []string{permission.TypeWebapp, permission.TypeKonnector, permission.TypeShareByLink} {
			for _, patch := range []string{
				`"permissions":{"files":{}}`,
				`"permissions":{"added":{"type":"io.cozy.files","verbs":["GET"]}},"password":""`,
				`"permissions":{"added":{"type":"io.cozy.files","verbs":["GET"]}},"expires_at":""`,
				`"permissions":{"added":{"type":"io.cozy.files","verbs":["GET"]}},"cozyMetadata":{}`,
			} {
				t.Run(caller+"/"+targetType+"/"+patch, func(t *testing.T) {
					target := &permission.Permission{
						Type: targetType, SourceID: "victim", Password: "protected", ExpiresAt: "2000-01-01T00:00:00Z",
						Permissions: permission.Set{{Title: "files", Type: consts.Files}},
					}
					before := target.Clone()
					c := echo.New().NewContext(httptest.NewRequest(http.MethodPatch, "/",
						strings.NewReader(`{"data":{"attributes":{`+patch+`}}}`)), httptest.NewRecorder())
					middlewares.SetInstance(c, &instance.Instance{})
					middlewares.ForcePermission(c, &permission.Permission{
						Type: caller, SourceID: "caller",
						Permissions: permission.Set{{Type: consts.Files, Verbs: permission.VerbSet{permission.GET: {}}}},
					})
					handler := patchPermission(func(_ prefixer.Prefixer, _ string) (*permission.Permission, error) {
						return target, nil
					}, "permdocid")
					err := handler(c)
					require.Error(t, err)
					assert.True(t, err == permission.ErrNotParent || err == middlewares.ErrForbidden)
					assert.Equal(t, before, target.Clone(), "a rejected patch must not mutate the permission")
				})
			}
		}
	}
}
