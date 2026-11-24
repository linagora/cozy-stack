package session

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
)

var delegatedInst *instance.Instance

func TestDelegated(t *testing.T) {
	var JWTSecret = []byte("foobar")

	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	conf := config.GetConfig()
	conf.Authentication = make(map[string]interface{})
	confAuth := make(map[string]interface{})
	confAuth["jwt_secret"] = base64.StdEncoding.EncodeToString(JWTSecret)
	conf.Authentication[config.DefaultInstanceContext] = confAuth

	delegatedInst = &instance.Instance{Domain: "external.notmycozy.net"}

	t.Run("GoodCheckDelegatedJWT", func(t *testing.T) {
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, ExternalClaims{
			RegisteredClaims: jwt.RegisteredClaims{
				Subject:   "sruti",
				IssuedAt:  jwt.NewNumericDate(time.Now()),
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			},
			Name:  "external.notmycozy.net",
			Email: "sruti@external.notmycozy.net",
			Code:  "student",
		})
		signed, err := token.SignedString(JWTSecret)
		assert.NoError(t, err)
		err = CheckDelegatedJWT(delegatedInst, signed)
		assert.NoError(t, err)
	})

	t.Run("BadExpiredCheckDelegatedJWT", func(t *testing.T) {
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, ExternalClaims{
			RegisteredClaims: jwt.RegisteredClaims{
				Subject:   "sruti",
				IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(-1 * time.Hour)),
			},
			Name:  "external.notmycozy.net",
			Email: "sruti@external.notmycozy.net",
			Code:  "student",
		})
		signed, err := token.SignedString(JWTSecret)
		assert.NoError(t, err)
		err = CheckDelegatedJWT(delegatedInst, signed)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "expired")
	})

	t.Run("BadNoExpCheckDelegatedJWT", func(t *testing.T) {
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, ExternalClaims{
			RegisteredClaims: jwt.RegisteredClaims{
				Subject:  "sruti",
				IssuedAt: jwt.NewNumericDate(time.Now()),
			},
			Name:  "external.notmycozy.net",
			Email: "sruti@external.notmycozy.net",
			Code:  "student",
		})
		signed, err := token.SignedString(JWTSecret)
		assert.NoError(t, err)
		err = CheckDelegatedJWT(delegatedInst, signed)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "expired")
	})

	t.Run("BadIssuerCheckDelegatedJWT", func(t *testing.T) {
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, ExternalClaims{
			RegisteredClaims: jwt.RegisteredClaims{
				Subject:   "sruti",
				IssuedAt:  jwt.NewNumericDate(time.Now()),
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			},
			Name:  "evil.notmycozy.net",
			Email: "sruti@external.notmycozy.net",
			Code:  "student",
		})
		signed, err := token.SignedString(JWTSecret)
		assert.NoError(t, err)
		err = CheckDelegatedJWT(delegatedInst, signed)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "Issuer")
	})
}
