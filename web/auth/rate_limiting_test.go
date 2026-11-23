package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/limits"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setTestLimit(t *testing.T, ct limits.CounterType, limit int64) {
	old := limits.GetMaximumLimit(ct)
	limits.SetMaximumLimit(ct, limit)
	t.Cleanup(func() { limits.SetMaximumLimit(ct, old) })
}

func TestSessionCodeCountsFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	tryCode := func(inst *instance.Instance, passphrase, passcode string) canCreateSessionCodeResult {
		body := `{"passphrase":"` + passphrase + `","two_factor_token":"dG9rZW4=","two_factor_passcode":"` + passcode + `"}`
		req := httptest.NewRequest(http.MethodPost, "/auth/session_code", strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		c := echo.New().NewContext(req, httptest.NewRecorder())
		c.Set("instance", inst)
		result, _ := canCreateSessionCode(c, inst)
		return result
	}
	isBlocked := func(inst *instance.Instance) bool {
		reloaded, err := instance.Get(inst.Domain)
		require.NoError(t, err)
		return reloaded.Blocked
	}

	t.Run("Passphrase", func(t *testing.T) {
		setTestLimit(t, limits.AuthType, 1)
		setup := testutils.NewSetup(t, t.Name())
		inst := setup.GetTestInstance(&lifecycle.Options{Passphrase: "MyPassphrase", KdfIterations: 5000, Key: "xxx"})

		assert.Equal(t, cannotCreateSessionCode, tryCode(inst, "wrong", ""))
		assert.False(t, isBlocked(inst))
		assert.Equal(t, cannotCreateSessionCode, tryCode(inst, "wrong", ""))
		assert.True(t, isBlocked(inst))
	})

	t.Run("TwoFactor", func(t *testing.T) {
		// The second wrong passcode exceeds the limit, and as no new passcode
		// can be sent, the instance is blocked.
		setTestLimit(t, limits.TwoFactorType, 0)
		setTestLimit(t, limits.TwoFactorGenerationType, 0)
		setup := testutils.NewSetup(t, t.Name())
		inst := setup.GetTestInstance(&lifecycle.Options{Passphrase: "MyPassphrase", KdfIterations: 5000, Key: "xxx"})
		require.NoError(t, lifecycle.UpdateAuthMode(inst, instance.TwoFactorMail))

		// The request without a code only asks for one: it is not counted.
		assert.Equal(t, need2FAToCreateSessionCode, tryCode(inst, "MyPassphrase", ""))
		assert.Equal(t, need2FAToCreateSessionCode, tryCode(inst, "MyPassphrase", "000000"))
		assert.False(t, isBlocked(inst))
		assert.Equal(t, need2FAToCreateSessionCode, tryCode(inst, "MyPassphrase", "000000"))
		assert.True(t, isBlocked(inst))
	})
}
