package common

import (
	"errors"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/stretchr/testify/require"
)

func TestBuildRequest_MatrixIDFromEmail(t *testing.T) {
	inst := &instance.Instance{Domain: "alicewonderland.stg.lin-saas.com"}

	t.Run("email localpart preserves dots and drives the matrix id", func(t *testing.T) {
		settings := &couchdb.JSONDoc{M: map[string]interface{}{
			"email": "al.ice.wonder.land@stg.lin-saas.com",
		}}
		req := buildRequest(inst, settings)
		require.Equal(t, "@al.ice.wonder.land:stg.lin-saas.com", req.Payload.MatrixID)
	})

	t.Run("email localpart is lowercased", func(t *testing.T) {
		settings := &couchdb.JSONDoc{M: map[string]interface{}{
			"email": "Al.Ice@stg.lin-saas.com",
		}}
		req := buildRequest(inst, settings)
		require.Equal(t, "@al.ice:stg.lin-saas.com", req.Payload.MatrixID)
	})

	t.Run("falls back to the domain slug when no email is present", func(t *testing.T) {
		settings := &couchdb.JSONDoc{M: map[string]interface{}{}}
		req := buildRequest(inst, settings)
		require.Equal(t, "@alicewonderland:stg.lin-saas.com", req.Payload.MatrixID)
	})

	t.Run("single-label domain yields no matrix id", func(t *testing.T) {
		local := &instance.Instance{Domain: "localhost"}
		settings := &couchdb.JSONDoc{M: map[string]interface{}{
			"email": "al.ice@example.org",
		}}
		req := buildRequest(local, settings)
		require.Empty(t, req.Payload.MatrixID)
	})
}

func TestUpdateCommonSettings_VersionMismatchReturnsTypedError(t *testing.T) {
	// Initialize test configuration
	config.UseTestFile(t)

	// Configure common settings to enable code path
	conf := config.GetConfig()
	conf.CommonSettings = map[string]config.CommonSettings{
		config.DefaultInstanceContext: {URL: "http://example.org", Token: "token"},
		"":                            {URL: "http://example.org", Token: "token"},
	}

	// Stub HTTP call to avoid network
	oldDo := DoCommonHTTP
	DoCommonHTTP = func(method, urlStr, token string, body []byte) error { return nil }
	defer func() { DoCommonHTTP = oldDo }()

	// Stub remote getter to simulate mismatch
	oldGet := GetRemoteCommonSettings
	GetRemoteCommonSettings = func(inst *instance.Instance) (*UserSettingsRequest, error) {
		return &UserSettingsRequest{Version: inst.CommonSettingsVersion + 1}, nil
	}
	defer func() { GetRemoteCommonSettings = oldGet }()

	inst := &instance.Instance{Domain: "foo.example", ContextName: config.DefaultInstanceContext}
	inst.CommonSettingsVersion = 1

	// Minimal settings doc with a common field to trigger path
	settings := &couchdb.JSONDoc{M: map[string]interface{}{"email": "a@b.c"}}

	updated, err := UpdateCommonSettings(inst, settings)
	require.Error(t, err)
	require.False(t, updated)
	require.True(t, errors.Is(err, ErrCommonSettingsVersionMismatch))
	require.Equal(t, 1, inst.CommonSettingsVersion, "version must not change on mismatch")
}

func TestUpdateAvatar_InSync_SuccessIncrementsAfterPut(t *testing.T) {
	config.UseTestFile(t)

	conf := config.GetConfig()
	conf.CommonSettings = map[string]config.CommonSettings{
		config.DefaultInstanceContext: {URL: "http://example.org", Token: "token"},
		"":                            {URL: "http://example.org", Token: "token"},
	}

	oldDo := DoCommonHTTP
	var called bool
	DoCommonHTTP = func(method, urlStr, token string, body []byte) error {
		called = true
		return nil
	}
	defer func() { DoCommonHTTP = oldDo }()

	oldGet := GetRemoteCommonSettings
	GetRemoteCommonSettings = func(inst *instance.Instance) (*UserSettingsRequest, error) {
		return &UserSettingsRequest{Version: inst.CommonSettingsVersion}, nil
	}
	defer func() { GetRemoteCommonSettings = oldGet }()

	inst := &instance.Instance{Domain: "foo.example", ContextName: config.DefaultInstanceContext}
	inst.CommonSettingsVersion = 1

	updated, err := UpdateAvatar(inst)
	require.NoError(t, err)
	require.True(t, updated)
	require.True(t, called)
	require.Equal(t, 2, inst.CommonSettingsVersion)
}

func TestUpdateAvatar_RemoteMismatch_UsesRemotePlusOne(t *testing.T) {
	config.UseTestFile(t)

	conf := config.GetConfig()
	conf.CommonSettings = map[string]config.CommonSettings{
		config.DefaultInstanceContext: {URL: "http://example.org", Token: "token"},
		"":                            {URL: "http://example.org", Token: "token"},
	}

	oldDo := DoCommonHTTP
	DoCommonHTTP = func(method, urlStr, token string, body []byte) error { return nil }
	defer func() { DoCommonHTTP = oldDo }()

	oldGet := GetRemoteCommonSettings
	GetRemoteCommonSettings = func(inst *instance.Instance) (*UserSettingsRequest, error) {
		return &UserSettingsRequest{Version: inst.CommonSettingsVersion + 2}, nil
	}
	defer func() { GetRemoteCommonSettings = oldGet }()

	inst := &instance.Instance{Domain: "foo.example", ContextName: config.DefaultInstanceContext}
	inst.CommonSettingsVersion = 1

	updated, err := UpdateAvatar(inst)
	require.NoError(t, err)
	require.True(t, updated)
	// remote was 3, next should be 4
	require.Equal(t, 4, inst.CommonSettingsVersion)
}

func TestUpdateAvatar_HttpFailure_DoesNotChangeVersion(t *testing.T) {
	config.UseTestFile(t)

	conf := config.GetConfig()
	conf.CommonSettings = map[string]config.CommonSettings{
		config.DefaultInstanceContext: {URL: "http://example.org", Token: "token"},
		"":                            {URL: "http://example.org", Token: "token"},
	}

	oldDo := DoCommonHTTP
	DoCommonHTTP = func(method, urlStr, token string, body []byte) error { return errors.New("boom") }
	defer func() { DoCommonHTTP = oldDo }()

	oldGet := GetRemoteCommonSettings
	GetRemoteCommonSettings = func(inst *instance.Instance) (*UserSettingsRequest, error) {
		return &UserSettingsRequest{Version: inst.CommonSettingsVersion}, nil
	}
	defer func() { GetRemoteCommonSettings = oldGet }()

	inst := &instance.Instance{Domain: "foo.example", ContextName: config.DefaultInstanceContext}
	inst.CommonSettingsVersion = 5

	updated, err := UpdateAvatar(inst)
	require.Error(t, err)
	require.False(t, updated)
	// Version should remain unchanged on failure
	require.Equal(t, 5, inst.CommonSettingsVersion)
}
