package auth

import (
	"fmt"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/limits"
)

// LoginRateExceeded blocks the instance after too many failed attempts to
// login
func LoginRateExceeded(i *instance.Instance) error {
	err := fmt.Errorf("Instance was blocked because of too many login failed attempts")
	i.Logger().WithNamespace("rate_limiting").Warn(err.Error())
	return lifecycle.Block(i, instance.BlockedLoginFailed.Code)
}

// CountFailedPassphrase counts a wrong passphrase, and blocks the instance
// past the limit, as for the login. Every check of the passphrase must call it.
func CountFailedPassphrase(i *instance.Instance) {
	err := config.GetRateLimiter().CheckRateLimit(i, limits.AuthType)
	if limits.IsLimitReachedOrExceeded(err) {
		if err = LoginRateExceeded(i); err != nil {
			i.Logger().WithNamespace("auth").Warn(err.Error())
		}
	}
}

// CountFailedTwoFactor counts a wrong 2FA passcode. Past the limit, a new
// passcode is sent, and too many of them block the instance, which the
// returned error tells. Every check of a 2FA passcode must call it.
func CountFailedTwoFactor(i *instance.Instance) error {
	if config.GetRateLimiter().CheckRateLimit(i, limits.TwoFactorType) != limits.ErrRateLimitExceeded {
		return nil
	}
	err := TwoFactorRateExceeded(i)
	if err != nil {
		i.Logger().WithNamespace("auth").Warn(err.Error())
	}
	return err
}

// TwoFactorRateExceeded regenerates a new 2FA passcode after too many failed
// attempts to login
func TwoFactorRateExceeded(i *instance.Instance) error {
	err := config.GetRateLimiter().CheckRateLimit(i, limits.TwoFactorGenerationType)
	if limits.IsLimitReachedOrExceeded(err) {
		return TwoFactorGenerationExceeded(i)
	}
	// Reset the key and send a new passcode to the user
	config.GetRateLimiter().ResetCounter(i, limits.TwoFactorType)
	_, err = lifecycle.SendTwoFactorPasscode(i)
	return err
}

// TwoFactorGenerationExceeded checks if there was too many attempts to
// regenerate a 2FA code within an hour
func TwoFactorGenerationExceeded(i *instance.Instance) error {
	err := fmt.Errorf("Instance was blocked because of too many 2FA passcode generations")
	i.Logger().WithNamespace("rate_limiting").Warn(err.Error())

	return lifecycle.Block(i, instance.BlockedLoginFailed.Code)
}
