package sharings

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fileRootSharing() (*sharing.Sharing, *sharing.Member) {
	s := &sharing.Sharing{
		SID:           "sharing-file",
		Drive:         true,
		Active:        true,
		DriveRootType: sharing.DriveRootTypeFile,
		Rules: []sharing.Rule{{
			Title: "file", DocType: consts.Files, Values: []string{"root-file-id"},
		}},
	}
	member := &sharing.Member{Status: sharing.MemberStatusReady, Email: "bob@x.test", ReadOnly: true}
	return s, member
}

var copyLookalikeTargets = []string{
	"/sharings/drives/sharing-file/f-sub%2Fcopy",
	"/sharings/drives/sharing-file/copy",
}

// Without an instance, the guard cannot resolve any effective access: a
// request that gets past the copy shortcut panics on the nil instance, while
// the buggy shortcut returned nil without checking anything.
func TestGuardCopyLookalikeIsNotShortCircuited(t *testing.T) {
	s, member := fileRootSharing()
	for _, target := range copyLookalikeTargets {
		e := echo.New()
		called := false
		var guardErr error
		var checked bool
		g := e.Group("/sharings", func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				called = true
				SetSharedDriveMember(c, member)
				func() {
					defer func() { _ = recover() }()
					guardErr = guardSharedDriveRouteForMember(c, nil, s)
					checked = true // returned without panicking: no access resolution
				}()
				return nil
			}
		})
		drivesRoutes(g)

		req := httptest.NewRequest(http.MethodPost, target+"?Type=directory&Name=x", nil)
		e.ServeHTTP(httptest.NewRecorder(), req)
		require.True(t, called, target)
		assert.False(t, checked && guardErr == nil, "the guard must check access instead of waving through %s", target)
	}
}

func TestGuardCopySuffixInFileIDIsNotACopy(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()

	s, member := fileRootSharing()
	for _, target := range copyLookalikeTargets {
		e := echo.New()
		called := false
		var guardErr error
		g := e.Group("/sharings", func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				called = true
				SetSharedDriveMember(c, member)
				guardErr = guardSharedDriveRouteForMember(c, inst, s)
				return nil
			}
		})
		drivesRoutes(g)

		req := httptest.NewRequest(http.MethodPost, target+"?Type=directory&Name=x", nil)
		e.ServeHTTP(httptest.NewRecorder(), req)
		require.True(t, called, target)
		assert.Error(t, guardErr, "a creation in an unknown folder must not pass the guard: %s", target)
	}
}
