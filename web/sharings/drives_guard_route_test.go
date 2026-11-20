package sharings

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestGuardCopySuffixInFileIDIsNotACopy(t *testing.T) {
	e := echo.New()
	drivesRoutes(e.Group("/sharings"))

	var inst *instance.Instance
	if !testing.Short() {
		config.UseTestFile(t)
		testutils.NeedCouchdb(t)
		inst = testutils.NewSetup(t, t.Name()).GetTestInstance()
	}
	drive := &sharing.Sharing{Drive: true, DriveRootType: sharing.DriveRootTypeFile}
	member := &sharing.Member{Status: sharing.MemberStatusReady, Email: "bob@x.test", ReadOnly: true}

	for _, fileID := range []string{"f-sub%2Fcopy", "copy"} {
		t.Run(fileID, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/sharings/drives/sharing-file/"+fileID, nil)
			c := e.NewContext(req, httptest.NewRecorder())
			e.Router().Find(req.Method, echo.GetPath(req), c)
			require.Equal(t, "/sharings/drives/:id/:file-id", c.Path())
			require.Equal(t, fileID, c.Param("file-id"))

			t.Run("MemberAccess", func(t *testing.T) {
				if testing.Short() {
					t.Skip("checking access requires CouchDB")
				}
				SetSharedDriveMember(c, member)
				var apiErr *jsonapi.Error
				require.ErrorAs(t, guardSharedDriveRouteForMember(c, inst, drive), &apiErr)
				require.Equal(t, http.StatusNotFound, apiErr.Status)
			})
		})
	}
}
