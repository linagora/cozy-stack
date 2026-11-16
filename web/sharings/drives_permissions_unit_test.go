package sharings_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/permission"
	modelsharing "github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/metadata"
	sharingsweb "github.com/cozy/cozy-stack/web/sharings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertJSONAPIError(t *testing.T, err error, status int, detailContains string) {
	t.Helper()
	require.Error(t, err)
	var apiErr *jsonapi.Error
	require.True(t, errors.As(err, &apiErr), "expected jsonapi.Error, got %T", err)
	assert.Equal(t, status, apiErr.Status)
	if detailContains != "" {
		assert.Contains(t, apiErr.Detail, detailContains)
	}
}

func permissionWithCreatorDomain(domain string) *permission.Permission {
	return &permission.Permission{
		Metadata: &metadata.CozyMetadata{
			UpdatedByApps: []*metadata.UpdatedByAppEntry{
				{Instance: domain},
			},
		},
	}
}

func TestValidateSharedDrivePermissionPatch(t *testing.T) {
	t.Run("AcceptsPermissionsField", func(t *testing.T) {
		patch := permission.Permission{
			Permissions: permission.Set{
				{Type: "io.cozy.files"},
			},
		}
		err := sharingsweb.ValidateSharedDrivePermissionPatch(patch)
		require.NoError(t, err)
	})

	t.Run("RejectsCodesField", func(t *testing.T) {
		patch := permission.Permission{
			Codes: map[string]string{"foo": "bar"},
		}
		err := sharingsweb.ValidateSharedDrivePermissionPatch(patch)
		assertJSONAPIError(t, err, http.StatusBadRequest, "codes cannot be modified")
	})

	t.Run("RejectsNoAttributes", func(t *testing.T) {
		err := sharingsweb.ValidateSharedDrivePermissionPatch(permission.Permission{})
		assertJSONAPIError(t, err, http.StatusBadRequest, "password, expires_at, permissions, or activity_report_enabled must be provided")
	})

	t.Run("RejectsInvalidPasswordType", func(t *testing.T) {
		err := sharingsweb.ValidateSharedDrivePermissionPatch(permission.Permission{Password: true})
		assertJSONAPIError(t, err, http.StatusBadRequest, "password must be a string")
	})

	t.Run("RejectsInvalidExpiresAtType", func(t *testing.T) {
		err := sharingsweb.ValidateSharedDrivePermissionPatch(permission.Permission{ExpiresAt: true})
		assertJSONAPIError(t, err, http.StatusBadRequest, "expires_at must be a string")
	})

	t.Run("AcceptsPasswordAndExpiresAtStrings", func(t *testing.T) {
		err := sharingsweb.ValidateSharedDrivePermissionPatch(permission.Permission{
			Password:  "secret",
			ExpiresAt: "2030-01-01T00:00:00Z",
		})
		require.NoError(t, err)
	})
}

func TestApplySharedDrivePermissionPatch(t *testing.T) {
	t.Run("SetsPasswordHash", func(t *testing.T) {
		perm := &permission.Permission{}
		err := sharingsweb.ApplySharedDrivePermissionPatch(perm, permission.Permission{Password: "secret"})
		require.NoError(t, err)
		require.NotNil(t, perm.Password)
		_, ok := perm.Password.([]byte)
		assert.True(t, ok)
	})

	t.Run("ClearsPassword", func(t *testing.T) {
		perm := &permission.Permission{Password: []byte("hash")}
		err := sharingsweb.ApplySharedDrivePermissionPatch(perm, permission.Permission{Password: ""})
		require.NoError(t, err)
		assert.Nil(t, perm.Password)
	})

	t.Run("SetsExpiresAt", func(t *testing.T) {
		perm := &permission.Permission{}
		err := sharingsweb.ApplySharedDrivePermissionPatch(perm, permission.Permission{ExpiresAt: "2031-01-01T00:00:00Z"})
		require.NoError(t, err)
		_, ok := perm.ExpiresAt.(time.Time)
		assert.True(t, ok)
	})

	t.Run("ClearsExpiresAt", func(t *testing.T) {
		perm := &permission.Permission{ExpiresAt: time.Now()}
		err := sharingsweb.ApplySharedDrivePermissionPatch(perm, permission.Permission{ExpiresAt: ""})
		require.NoError(t, err)
		assert.Nil(t, perm.ExpiresAt)
	})

	t.Run("RejectsInvalidExpiresAtFormat", func(t *testing.T) {
		perm := &permission.Permission{}
		err := sharingsweb.ApplySharedDrivePermissionPatch(perm, permission.Permission{ExpiresAt: "not-a-date"})
		assertJSONAPIError(t, err, http.StatusBadRequest, "RFC3339")
	})
}

func TestCheckSharedDrivePermissionMutationAuthorization(t *testing.T) {
	t.Run("AllowsOwnerWithAppPermission", func(t *testing.T) {
		s := &modelsharing.Sharing{Owner: true}
		target := permissionWithCreatorDomain("creator.example")
		current := &permission.Permission{Type: permission.TypeWebapp}

		err := sharingsweb.CheckSharedDrivePermissionMutationAuthorization(
			s,
			target,
			current,
			"",
			false,
			false,
			false,
		)
		require.NoError(t, err)
	})

	t.Run("AllowsCreatorWithResolvedIdentity", func(t *testing.T) {
		s := &modelsharing.Sharing{Owner: false}
		target := permissionWithCreatorDomain("creator.example")
		current := &permission.Permission{Type: permission.TypeShareInteract}

		err := sharingsweb.CheckSharedDrivePermissionMutationAuthorization(
			s,
			target,
			current,
			"creator.example",
			true,
			false,
			false,
		)
		require.NoError(t, err)
	})

	t.Run("AllowsWritableMemberWithConfig", func(t *testing.T) {
		s := &modelsharing.Sharing{Owner: false}
		target := permissionWithCreatorDomain("creator.example")
		current := &permission.Permission{Type: permission.TypeShareInteract}

		err := sharingsweb.CheckSharedDrivePermissionMutationAuthorization(
			s,
			target,
			current,
			"writer.example",
			true,
			false,
			true,
		)
		require.NoError(t, err)
	})

	t.Run("AllowsReadOnlyCreatorOnReadonlyLink", func(t *testing.T) {
		s := &modelsharing.Sharing{Owner: false}
		target := permissionWithCreatorDomain("creator.example")
		target.Permissions = permission.Set{
			{Type: "io.cozy.files", Verbs: permission.Verbs(permission.GET)},
		}
		current := &permission.Permission{Type: permission.TypeShareInteract}

		err := sharingsweb.CheckSharedDrivePermissionMutationAuthorization(
			s,
			target,
			current,
			"creator.example",
			true,
			true,
			false,
		)
		require.NoError(t, err)
	})

	t.Run("RejectsReadOnlyCreatorOnWritableLink", func(t *testing.T) {
		s := &modelsharing.Sharing{Owner: false}
		target := permissionWithCreatorDomain("creator.example")
		target.Permissions = permission.Set{
			{Type: "io.cozy.files", Verbs: permission.Verbs(permission.GET, permission.POST)},
		}
		current := &permission.Permission{Type: permission.TypeShareInteract}

		err := sharingsweb.CheckSharedDrivePermissionMutationAuthorization(
			s,
			target,
			current,
			"creator.example",
			true,
			true,
			true,
		)
		assertJSONAPIError(t, err, http.StatusForbidden, "write access denied")
	})

	t.Run("RejectsPublicShareByLinkToken", func(t *testing.T) {
		s := &modelsharing.Sharing{Owner: true}
		target := permissionWithCreatorDomain("creator.example")
		current := &permission.Permission{Type: permission.TypeShareByLink}

		err := sharingsweb.CheckSharedDrivePermissionMutationAuthorization(
			s,
			target,
			current,
			"",
			false,
			false,
			true,
		)
		assertJSONAPIError(t, err, http.StatusForbidden, "public share token cannot modify")
	})

	t.Run("RejectsPublicSharePreviewToken", func(t *testing.T) {
		s := &modelsharing.Sharing{Owner: true}
		target := permissionWithCreatorDomain("creator.example")
		current := &permission.Permission{Type: permission.TypeSharePreview}

		err := sharingsweb.CheckSharedDrivePermissionMutationAuthorization(
			s,
			target,
			current,
			"",
			false,
			false,
			true,
		)
		assertJSONAPIError(t, err, http.StatusForbidden, "public share token cannot modify")
	})

	t.Run("RejectsUnresolvedShareInteractIdentity", func(t *testing.T) {
		s := &modelsharing.Sharing{Owner: false}
		target := permissionWithCreatorDomain("creator.example")
		current := &permission.Permission{Type: permission.TypeShareInteract}

		err := sharingsweb.CheckSharedDrivePermissionMutationAuthorization(
			s,
			target,
			current,
			"",
			false,
			false,
			true,
		)
		assertJSONAPIError(t, err, http.StatusForbidden, "cannot verify caller identity")
	})

	t.Run("RejectsNonOwnerNonCreatorWithoutConfig", func(t *testing.T) {
		s := &modelsharing.Sharing{Owner: false}
		target := permissionWithCreatorDomain("creator.example")
		current := &permission.Permission{Type: permission.TypeWebapp}

		err := sharingsweb.CheckSharedDrivePermissionMutationAuthorization(
			s,
			target,
			current,
			"other.example",
			true,
			false,
			false,
		)
		assertJSONAPIError(t, err, http.StatusForbidden, "only creator or owner can modify")
	})

	t.Run("RejectsReadOnlyNonCreatorWithConfig", func(t *testing.T) {
		s := &modelsharing.Sharing{Owner: false}
		target := permissionWithCreatorDomain("creator.example")
		target.Permissions = permission.Set{
			{Type: "io.cozy.files", Verbs: permission.Verbs(permission.GET)},
		}
		current := &permission.Permission{Type: permission.TypeShareInteract}

		err := sharingsweb.CheckSharedDrivePermissionMutationAuthorization(
			s,
			target,
			current,
			"readonly.example",
			true,
			true,
			true,
		)
		assertJSONAPIError(t, err, http.StatusForbidden, "only creator or owner can modify")
	})
}
