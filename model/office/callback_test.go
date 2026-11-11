package office

import (
	"testing"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/config/config"
	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckToken(t *testing.T) {
	cfg := &config.Office{OnlyOfficeURL: "https://documentserver.example", OutboxSecret: "outbox"}
	params := CallbackParameters{Key: "the-key", Status: StatusForceSaveRequested, URL: "https://documentserver.example/cache/file.docx"}
	sign := func(method jwt.SigningMethod, secret string, key string, status int, url string) string {
		claims := jwt.MapClaims{"payload": map[string]interface{}{"key": key, "status": status, "url": url}}
		token, err := jwt.NewWithClaims(method, claims).SignedString([]byte(secret))
		require.NoError(t, err)
		return token
	}
	withToken := func(token string) CallbackParameters {
		p := params
		p.Token = token
		return p
	}

	assert.NoError(t, checkToken(cfg, withToken(sign(jwt.SigningMethodHS256, "outbox", "the-key", 6, params.URL))))

	assert.ErrorIs(t, checkToken(nil, withToken(sign(jwt.SigningMethodHS256, "outbox", "the-key", 6, params.URL))), ErrNoServer)
	assert.ErrorIs(t, checkToken(&config.Office{OnlyOfficeURL: cfg.OnlyOfficeURL}, withToken(sign(jwt.SigningMethodHS256, "outbox", "the-key", 6, params.URL))), ErrNoServer)

	refused := map[string]string{
		"no token":        "",
		"other secret":    sign(jwt.SigningMethodHS256, "other", "the-key", 6, params.URL),
		"other method":    sign(jwt.SigningMethodHS512, "outbox", "the-key", 6, params.URL),
		"other key":       sign(jwt.SigningMethodHS256, "outbox", "other-key", 6, params.URL),
		"other status":    sign(jwt.SigningMethodHS256, "outbox", "the-key", 2, params.URL),
		"other url":       sign(jwt.SigningMethodHS256, "outbox", "the-key", 6, "https://documentserver.example/other"),
		"without payload": mustSign(t, jwt.MapClaims{"key": "the-key", "status": 6, "url": params.URL}, "outbox"),
	}
	for name, token := range refused {
		assert.ErrorIs(t, checkToken(cfg, withToken(token)), permission.ErrInvalidToken, name)
	}

	none, err := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"payload": map[string]interface{}{"key": "the-key", "status": 6, "url": params.URL}}).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)
	assert.ErrorIs(t, checkToken(cfg, withToken(none)), permission.ErrInvalidToken, "none")
}

func mustSign(t *testing.T, claims jwt.MapClaims, secret string) string {
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	require.NoError(t, err)
	return token
}
