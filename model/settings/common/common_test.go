package common

import (
	"errors"
	"strings"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/stretchr/testify/require"
)

func TestBuildRequest_MatrixID(t *testing.T) {
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

	t.Run("the homeserver is the mail domain, not the instance domain", func(t *testing.T) {
		settings := &couchdb.JSONDoc{M: map[string]interface{}{
			"email": "al.ice@linagora.com",
		}}
		req := buildRequest(&instance.Instance{Domain: "alice.twake.linagora.com"}, settings)
		require.Equal(t, "@al.ice:linagora.com", req.Payload.MatrixID)
	})

	t.Run("no email yields no matrix id", func(t *testing.T) {
		settings := &couchdb.JSONDoc{M: map[string]interface{}{}}
		req := buildRequest(inst, settings)
		require.Empty(t, req.Payload.MatrixID)
	})

	t.Run("an email without a domain yields no matrix id", func(t *testing.T) {
		settings := &couchdb.JSONDoc{M: map[string]interface{}{
			"email": "alicewonderland",
		}}
		req := buildRequest(inst, settings)
		require.Empty(t, req.Payload.MatrixID)
	})

	t.Run("a stored matrix id wins over the email and is sent as is", func(t *testing.T) {
		settings := &couchdb.JSONDoc{M: map[string]interface{}{
			"matrix_id": "@al.ice:stg.lin-saas.com",
			"email":     "alicewonderland@stg.lin-saas.com",
		}}
		req := buildRequest(inst, settings)
		require.Equal(t, "@al.ice:stg.lin-saas.com", req.Payload.MatrixID)
	})

	t.Run("an empty stored matrix id falls back to the email", func(t *testing.T) {
		settings := &couchdb.JSONDoc{M: map[string]interface{}{
			"matrix_id": "",
			"email":     "al.ice@stg.lin-saas.com",
		}}
		req := buildRequest(inst, settings)
		require.Equal(t, "@al.ice:stg.lin-saas.com", req.Payload.MatrixID)
	})

	t.Run("a non-string stored matrix id falls back to the email", func(t *testing.T) {
		settings := &couchdb.JSONDoc{M: map[string]interface{}{
			"matrix_id": 42,
			"email":     "al.ice@stg.lin-saas.com",
		}}
		req := buildRequest(inst, settings)
		require.Equal(t, "@al.ice:stg.lin-saas.com", req.Payload.MatrixID)
	})

	t.Run("a malformed stored matrix id falls back to the email", func(t *testing.T) {
		settings := &couchdb.JSONDoc{M: map[string]interface{}{
			"matrix_id": "not-a-matrix-id",
			"email":     "al.ice@stg.lin-saas.com",
		}}
		req := buildRequest(inst, settings)
		require.Equal(t, "@al.ice:stg.lin-saas.com", req.Payload.MatrixID)
	})
}

func TestIsMatrixID(t *testing.T) {
	valid := []string{
		"@alice:example.org",
		"@al.ice:stg.example.org",
		"@alice:example.org:8448",
	}
	for _, id := range valid {
		require.True(t, IsMatrixID(id), id)
	}

	invalid := []string{
		"",
		"alice:example.org",          // no leading @
		"@alice",                     // no server part
		"@:example.org",              // empty localpart
		"@alice:",                    // empty server
		"alice@example.org",          // an email, not a matrix id
		"@alice example:example.org", // inner whitespace
		"@alice:example.org\n",       // trailing newline
		"@" + strings.Repeat("a", 300) + ":example.org", // over the length cap
	}
	for _, id := range invalid {
		require.False(t, IsMatrixID(id), id)
	}
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
