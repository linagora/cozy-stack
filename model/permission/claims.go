package permission

import (
	"time"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/golang-jwt/jwt/v5"
)

// Claims is used for JWT used in OAuth2 flow and applications token
type Claims struct {
	jwt.RegisteredClaims
	Scope     string `json:"scope,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	SStamp    string `json:"stamp,omitempty"`
	Actor     *Actor `json:"act,omitempty"`
	IntentID  string `json:"intent_id,omitempty"`
}

// Actor identifies the service acting on behalf of the token subject (RFC 8693).
type Actor struct {
	Subject string `json:"sub"`
}

// IssuedAtUTC returns a time.Time struct of the IssuedAt field in UTC
// location.
func (claims *Claims) IssuedAtUTC() time.Time {
	return claims.IssuedAt.Time.UTC()
}

// AudienceString returns the audience as a string.
func (claims *Claims) AudienceString() string {
	if len(claims.Audience) == 0 {
		return ""
	}
	return claims.Audience[0]
}

// Expired returns true if a Claim is expired
func (claims *Claims) Expired() bool {
	if len(claims.Audience) != 1 {
		return true
	}

	var validityDuration time.Duration
	switch claims.Audience[0] {
	case consts.AppAudience:
		if claims.SessionID == "" {
			// an app token with no session association is used for services which
			// should have tokens that have the same properties as the konnector's
			// tokens
			validityDuration = consts.KonnectorTokenValidityDuration
		} else {
			validityDuration = consts.AppTokenValidityDuration
		}

	case consts.KonnectorAudience:
		validityDuration = consts.KonnectorTokenValidityDuration

	case consts.CLIAudience:
		validityDuration = consts.CLITokenValidityDuration

	case consts.AccessTokenAudience:
		validityDuration = consts.AccessTokenValidityDuration

	// Share, RefreshToken and RegistrationToken never expire
	case consts.ShareAudience, consts.RegistrationTokenAudience, consts.RefreshTokenAudience:
		return false

	default:
		validityDuration = consts.DefaultValidityDuration
	}
	if claims.IssuedAt == nil {
		return true
	}
	validUntil := claims.IssuedAtUTC().Add(validityDuration)
	return validUntil.Before(time.Now().UTC())
}

// BitwardenClaims are used for bitwarden clients. The bitwarden protocol
// expects some additional fields. Also, the subject must be the UserID, and
// the usual subject for Cozy OAuth clients are the id of the OAuth client
// which is not suitable here (the UserID must be the same for all bitwarden
// clients, as it is used to compute the user fingerprint). So, the client ID
// is saved in an additional field, client_id, and we are doing some tricks
// to make the stack accepts those JWT.
type BitwardenClaims struct {
	Claims
	ClientID string `json:"client_id"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Verified bool   `json:"email_verified"`
	Premium  bool   `json:"premium"`
}
