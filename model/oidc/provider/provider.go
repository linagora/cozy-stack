package provider

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"time"

	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/logger"
	jwt "github.com/golang-jwt/jwt/v5"
)

const backchannelLogoutEvent = "http://schemas.openid.net/event/backchannel-logout"

const cacheTTL = 24 * time.Hour

// Kind identifies the OIDC provider flavor used by the context configuration.
type Kind int

const (
	GenericProvider Kind = iota
	FranceConnectProvider
)

// ConfigRequirement describes a validation rule applied when loading config.
type ConfigRequirement int

const (
	RequireClientID ConfigRequirement = iota
	RequireClientSecret
	RequireScope
	RequireRedirectURI
	RequireAuthorizeURL
	RequireTokenURL
	RequireUserInfoURL
	RequireUserInfoMapping
	RequireIDTokenKeyURL
	RequireIssuerOrTokenURL
)

// Config is the config to log in a user with an OpenID Connect identity provider.
type Config struct {
	Provider            Kind
	ProviderKey         string
	AllowOAuthToken     bool
	AllowCustomInstance bool
	Issuer              string
	ClientID            string
	ClientSecret        string
	Scope               string
	RedirectURI         string
	AuthorizeURL        string
	TokenURL            string
	UserInfoURL         string
	UserInfoField       string
	UserInfoPrefix      string
	UserInfoSuffix      string
	IDTokenKeyURL       string
}

// Metadata represents the OpenID Connect configuration from the well-known endpoint.
type Metadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	EndSessionEndpoint    string `json:"end_session_endpoint"`
	JwksURI               string `json:"jwks_uri"`
}

// JWK is a single public key entry from the provider JWKS document.
type JWK struct {
	Alg  string `json:"alg"`
	Type string `json:"kty"`
	ID   string `json:"kid"`
	Use  string `json:"use"`
	E    string `json:"e"`
	N    string `json:"n"`
}

var discoveryClient = &http.Client{
	Timeout: 15 * time.Second,
}

var keysClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		DisableKeepAlives: true,
	},
}

func LoadConfig(context string, requirements ...ConfigRequirement) (*Config, error) {
	oidc, ok := config.GetOIDC(context)
	if !ok {
		return nil, errors.New("No OIDC is configured for this context")
	}

	allowOAuthToken, _ := oidc["allow_oauth_token"].(bool)
	allowCustomInstance, _ := oidc["allow_custom_instance"].(bool)
	providerKey, _ := oidc["provider_key"].(string)
	userInfoPrefix, _ := oidc["userinfo_instance_prefix"].(string)
	userInfoSuffix, _ := oidc["userinfo_instance_suffix"].(string)
	idTokenKeyURL, _ := oidc["id_token_jwk_url"].(string)
	issuer, _ := oidc["issuer"].(string)
	clientID, _ := oidc["client_id"].(string)
	clientSecret, _ := oidc["client_secret"].(string)
	scope, _ := oidc["scope"].(string)
	redirectURI, _ := oidc["redirect_uri"].(string)
	authorizeURL, _ := oidc["authorize_url"].(string)
	tokenURL, _ := oidc["token_url"].(string)
	userInfoURL, _ := oidc["userinfo_url"].(string)
	userInfoField, _ := oidc["userinfo_instance_field"].(string)

	conf := &Config{
		Provider:            GenericProvider,
		ProviderKey:         providerKey,
		AllowOAuthToken:     allowOAuthToken,
		AllowCustomInstance: allowCustomInstance,
		Issuer:              issuer,
		ClientID:            clientID,
		ClientSecret:        clientSecret,
		Scope:               scope,
		RedirectURI:         redirectURI,
		AuthorizeURL:        authorizeURL,
		TokenURL:            tokenURL,
		UserInfoURL:         userInfoURL,
		UserInfoField:       userInfoField,
		UserInfoPrefix:      userInfoPrefix,
		UserInfoSuffix:      userInfoSuffix,
		IDTokenKeyURL:       idTokenKeyURL,
	}
	if err := validateRequirements(conf, requirements...); err != nil {
		return nil, err
	}
	return conf, nil
}

func LoadFranceConnectConfig(context string, requirements ...ConfigRequirement) (*Config, error) {
	oidc, ok := config.GetFranceConnect(context)
	if !ok {
		return nil, errors.New("No FranceConnect is configured for this context")
	}

	providerKey, _ := oidc["provider_key"].(string)
	allowOAuthToken, _ := oidc["allow_oauth_token"].(bool)
	idTokenKeyURL, _ := oidc["id_token_jwk_url"].(string)
	issuer, _ := oidc["issuer"].(string)
	clientID, _ := oidc["client_id"].(string)
	clientSecret, _ := oidc["client_secret"].(string)
	scope, _ := oidc["scope"].(string)
	redirectURI, _ := oidc["redirect_uri"].(string)
	authorizeURL, ok := oidc["authorize_url"].(string)
	if !ok {
		authorizeURL = "https://app.franceconnect.gouv.fr/api/v1/authorize"
	}
	tokenURL, ok := oidc["token_url"].(string)
	if !ok {
		tokenURL = "https://app.franceconnect.gouv.fr/api/v1/token"
	}
	userInfoURL, ok := oidc["userinfo_url"].(string)
	if !ok {
		userInfoURL = "https://app.franceconnect.gouv.fr/api/v1/userinfo"
	}

	conf := &Config{
		Provider:            FranceConnectProvider,
		ProviderKey:         providerKey,
		AllowOAuthToken:     allowOAuthToken,
		AllowCustomInstance: true,
		Issuer:              issuer,
		ClientID:            clientID,
		ClientSecret:        clientSecret,
		Scope:               scope,
		RedirectURI:         redirectURI,
		AuthorizeURL:        authorizeURL,
		TokenURL:            tokenURL,
		UserInfoURL:         userInfoURL,
		IDTokenKeyURL:       idTokenKeyURL,
	}
	if err := validateRequirements(conf, requirements...); err != nil {
		return nil, err
	}
	return conf, nil
}

func ProviderKey(conf *Config, contextName string) string {
	if conf != nil && conf.ProviderKey != "" {
		return conf.ProviderKey
	}
	return contextName
}

func validateRequirements(conf *Config, requirements ...ConfigRequirement) error {
	for _, requirement := range requirements {
		switch requirement {
		case RequireClientID:
			if conf.ClientID == "" {
				return errors.New("The client_id is missing for this context")
			}
		case RequireClientSecret:
			if conf.ClientSecret == "" {
				return errors.New("The client_secret is missing for this context")
			}
		case RequireScope:
			if conf.Scope == "" {
				return errors.New("The scope is missing for this context")
			}
		case RequireRedirectURI:
			if conf.RedirectURI == "" {
				return errors.New("The redirect_uri is missing for this context")
			}
		case RequireAuthorizeURL:
			if conf.AuthorizeURL == "" {
				return errors.New("The authorize_url is missing for this context")
			}
		case RequireTokenURL:
			if conf.TokenURL == "" {
				return errors.New("The token_url is missing for this context")
			}
		case RequireUserInfoURL:
			if conf.UserInfoURL == "" {
				return errors.New("The userinfo_url is missing for this context")
			}
		case RequireUserInfoMapping:
			if !conf.AllowCustomInstance && conf.UserInfoField == "" {
				return errors.New("The userinfo_instance_field is missing for this context")
			}
		case RequireIDTokenKeyURL:
			if conf.IDTokenKeyURL == "" {
				return errors.New("The id_token_jwk_url is missing for this context")
			}
		case RequireIssuerOrTokenURL:
			if conf.Issuer == "" && conf.TokenURL == "" {
				return errors.New("The issuer or token_url is missing for this context")
			}
		default:
			return fmt.Errorf("unknown OIDC config requirement: %d", requirement)
		}
	}
	return nil
}

func GetMetadata(conf *Config) (*Metadata, error) {
	if conf == nil {
		return nil, errors.New("missing OIDC config")
	}
	return getMetadataFromTokenURL(conf.TokenURL)
}

func GetIssuer(contextName string, conf *Config) (string, error) {
	if conf.Issuer != "" {
		logger.WithNamespace("oidc").Debugf("Using configured issuer for OIDC context %s", contextName)
		return conf.Issuer, nil
	}

	logger.WithNamespace("oidc").Debugf("Resolving issuer from OIDC discovery for context %s", contextName)
	oidcConfig, err := GetMetadata(conf)
	if err != nil {
		return "", err
	}
	if oidcConfig.Issuer == "" {
		return "", fmt.Errorf("no issuer found for OIDC context %s", contextName)
	}
	logger.WithNamespace("oidc").Debugf("Using discovered issuer for OIDC context %s", contextName)
	return oidcConfig.Issuer, nil
}

func VerifyIDToken(raw string, conf *Config) (jwt.MapClaims, error) {
	keys, err := GetIDTokenKeys(conf.IDTokenKeyURL)
	if err != nil {
		return nil, err
	}

	token, err := jwt.Parse(raw, func(token *jwt.Token) (interface{}, error) {
		return ChooseKeyForIDToken(keys, token)
	})
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, errors.New("invalid token")
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, errors.New("invalid token")
	}
	return claims, nil
}

// VerifyIDTokenForClient checks that the identity provider of the context
// issued the id_token to the client_id of the context, so that an id_token of
// another relying party cannot open a session.
func VerifyIDTokenForClient(raw, contextName string, conf *Config) (jwt.MapClaims, error) {
	claims, err := VerifyIDToken(raw, conf)
	if err != nil {
		return nil, err
	}
	expectedIssuer, err := GetIssuer(contextName, conf)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve issuer for OIDC context %s: %w", contextName, err)
	}
	if issuer, _ := claims.GetIssuer(); issuer == "" || issuer != expectedIssuer {
		return nil, errors.New("id_token issuer mismatch")
	}
	// azp is not checked: a token asked by another client (the admin app for
	// example) is accepted when the provider also lists our client_id in aud.
	aud, err := claims.GetAudience()
	if conf.ClientID == "" || err != nil || !audienceContains(aud, conf.ClientID) {
		return nil, errors.New("id_token audience mismatch")
	}
	return claims, nil
}

func VerifyLogoutToken(raw, contextName string, conf *Config) (jwt.MapClaims, error) {
	if conf.IDTokenKeyURL == "" {
		return nil, errors.New("id_token_jwk_url is not configured")
	}

	claims, err := VerifyIDToken(raw, conf)
	if err != nil {
		return nil, err
	}

	expectedIssuer, err := GetIssuer(contextName, conf)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve issuer for OIDC context %s: %w", contextName, err)
	}
	if err := ValidateLogoutTokenClaims(claims, conf, expectedIssuer, true); err != nil {
		return nil, err
	}
	return claims, nil
}

func ValidateLogoutTokenClaims(claims jwt.MapClaims, conf *Config, expectedIssuer string, requireIssuerMatch bool) error {
	issuer, err := claims.GetIssuer()
	if err != nil || issuer == "" {
		return errors.New("logout token is missing iss")
	}
	if requireIssuerMatch && expectedIssuer != issuer {
		return fmt.Errorf("logout token issuer mismatch: %s", issuer)
	}

	aud, err := claims.GetAudience()
	if err != nil || !audienceContains(aud, conf.ClientID) {
		return errors.New("logout token audience mismatch")
	}

	iat, err := claims.GetIssuedAt()
	if err != nil || iat == nil {
		return errors.New("logout token is missing iat")
	}
	if iat.Time.After(time.Now().Add(5 * time.Minute)) {
		return errors.New("logout token iat is in the future")
	}

	jti, _ := claims["jti"].(string)
	if jti == "" {
		return errors.New("logout token is missing jti")
	}

	events, ok := claims["events"].(map[string]interface{})
	if !ok {
		if rawEvents, ok := claims["events"].(jwt.MapClaims); ok {
			events = rawEvents
		} else {
			return errors.New("logout token is missing events")
		}
	}
	if _, ok := events[backchannelLogoutEvent]; !ok {
		return errors.New("logout token is missing backchannel logout event")
	}

	sid, _ := claims["sid"].(string)
	sub, _ := claims["sub"].(string)
	if sid == "" && sub == "" {
		return errors.New("logout token must contain sid or sub")
	}

	return nil
}

// GetIDTokenKeys returns the keys that can be used to verify that an OIDC id_token is valid.
func GetIDTokenKeys(keyURL string) ([]*JWK, error) {
	cache := config.GetConfig().CacheStorage
	cacheKey := "oidc-jwk:" + keyURL

	data, ok := cache.Get(cacheKey)
	if !ok {
		var err error
		data, err = getKeysFromHTTP(keyURL)
		if err != nil {
			return nil, err
		}
	}

	var keys struct {
		Keys []*JWK `json:"keys"`
	}
	if err := json.Unmarshal(data, &keys); err != nil {
		return nil, err
	}
	if !ok {
		cache.Set(cacheKey, data, cacheTTL)
	}
	return keys.Keys, nil
}

// ChooseKeyForIDToken can be used to check an id_token as a JWT.
func ChooseKeyForIDToken(keys []*JWK, token *jwt.Token) (interface{}, error) {
	if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
		return nil, fmt.Errorf("Unexpected signing method: %v", token.Header["alg"])
	}

	var key *JWK
	for _, k := range keys {
		if k.Use != "sig" || k.Type != "RSA" {
			continue
		}
		if k.ID == token.Header["kid"] {
			return loadKey(k)
		}
		key = k
	}
	if key == nil {
		return nil, errors.New("Key not found")
	}
	return loadKey(key)
}

func CallEndSession(contextName, sessionID string) error {
	if sessionID == "" {
		return nil
	}

	conf, err := LoadConfig(contextName, RequireTokenURL)
	if err != nil {
		logger.WithNamespace("oidc").Warnf("Cannot load OIDC configuration for logout: %s", err)
		return err
	}

	oidcConfig, err := GetMetadata(conf)
	if err != nil {
		logger.WithNamespace("oidc").Warnf("Cannot fetch OpenID configuration for logout: %s", err)
		return err
	}

	if oidcConfig.EndSessionEndpoint == "" {
		logger.WithNamespace("oidc").Warnf("No end_session_endpoint found in OpenID configuration")
		return nil
	}

	endSessionURL, err := url.Parse(oidcConfig.EndSessionEndpoint)
	if err != nil {
		logger.WithNamespace("oidc").Warnf("Invalid end_session_endpoint URL: %s", err)
		return err
	}

	q := endSessionURL.Query()
	q.Add("session_id", sessionID)
	endSessionURL.RawQuery = q.Encode()

	req, err := http.NewRequest(http.MethodGet, endSessionURL.String(), nil)
	if err != nil {
		logger.WithNamespace("oidc").Warnf("Failed to create end_session request: %s", err)
		return err
	}
	req.Header.Add("User-Agent", build.UserAgent())

	res, err := discoveryClient.Do(req)
	if err != nil {
		logger.WithNamespace("oidc").Warnf("Error calling end_session_endpoint: %s", err)
		return err
	}
	defer res.Body.Close()

	if res.StatusCode >= http.StatusBadRequest {
		logger.WithNamespace("oidc").Warnf("end_session_endpoint returned status %d", res.StatusCode)
		return nil
	}

	logger.WithNamespace("oidc").Infof("Successfully called end_session_endpoint for OIDC logout")
	return nil
}

func getMetadataFromTokenURL(tokenURL string) (*Metadata, error) {
	parsedURL, err := url.Parse(tokenURL)
	if err != nil {
		return nil, fmt.Errorf("invalid OIDC token URL: %w", err)
	}

	wellKnownURL := (&url.URL{
		Scheme: parsedURL.Scheme,
		Host:   parsedURL.Host,
		Path:   "/.well-known/openid-configuration",
	}).String()

	cache := config.GetConfig().CacheStorage
	cacheKey := "oidc-config:" + wellKnownURL

	data, ok := cache.Get(cacheKey)
	if !ok {
		req, err := http.NewRequest(http.MethodGet, wellKnownURL, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Add("User-Agent", build.UserAgent())

		res, err := discoveryClient.Do(req)
		if err != nil {
			logger.WithNamespace("oidc").Errorf("Error fetching OpenID configuration: %s", err)
			return nil, fmt.Errorf("failed to fetch OpenID configuration: %w", err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			logger.WithNamespace("oidc").Warnf("Cannot fetch OpenID configuration: %d", res.StatusCode)
			return nil, fmt.Errorf("unexpected status code: %d", res.StatusCode)
		}

		data, err = io.ReadAll(res.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %w", err)
		}

		cache.Set(cacheKey, data, cacheTTL)
	}

	var oidcConfig Metadata
	if err := json.Unmarshal(data, &oidcConfig); err != nil {
		return nil, fmt.Errorf("failed to parse OpenID configuration: %w", err)
	}
	return &oidcConfig, nil
}

func getKeysFromHTTP(keyURL string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, keyURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add("User-Agent", build.UserAgent())
	res, err := keysClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		logger.WithNamespace("oidc").Warnf("getKeys cannot fetch jwk: %d", res.StatusCode)
		return nil, errors.New("cannot fetch jwk")
	}
	return io.ReadAll(res.Body)
}

func loadKey(raw *JWK) (interface{}, error) {
	var n, e big.Int
	nn, err := base64.RawURLEncoding.DecodeString(raw.N)
	if err != nil {
		return nil, err
	}
	n.SetBytes(nn)
	ee, err := base64.RawURLEncoding.DecodeString(raw.E)
	if err != nil {
		return nil, err
	}
	e.SetBytes(ee)

	var key rsa.PublicKey
	key.N = &n
	key.E = int(e.Int64())
	return &key, nil
}

func audienceContains(audiences []string, clientID string) bool {
	for _, audience := range audiences {
		if audience == clientID {
			return true
		}
	}
	return false
}
