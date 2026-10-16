package consts

import "time"

// This is the list of possible audience values for JWT.
const (
	AppAudience               = "app"          // used by client-side apps
	KonnectorAudience         = "konn"         // used by konnectors
	CLIAudience               = "cli"          // used by command line interface
	ShareAudience             = "share"        // used for share by links code
	RegistrationTokenAudience = "registration" // OAuth registration tokens
	AccessTokenAudience       = "access"       // OAuth access tokens
	RefreshTokenAudience      = "refresh"      // OAuth refresh tokens
)

// TokenValidityDuration is the duration where a token is valid in seconds (1 week)
var (
	DefaultValidityDuration = 24 * time.Hour

	AppTokenValidityDuration       = 24 * time.Hour
	IntentTokenValidityDuration    = 5 * time.Minute
	KonnectorTokenValidityDuration = 30 * time.Minute
	CLITokenValidityDuration       = 30 * time.Minute

	AccessTokenValidityDuration = 7 * 24 * time.Hour
)
