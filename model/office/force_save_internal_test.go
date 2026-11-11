package office

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/pkg/config/config"
	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecoveredTurnsAPanicIntoAnError(t *testing.T) {
	res, err := recovered(func() (interface{}, error) {
		panic("boom")
	})()
	assert.Nil(t, res)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

func TestTheCommandTokenExpires(t *testing.T) {
	tokens := make(chan string, 1)
	ds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Token string `json:"token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		tokens <- body.Token
		_, _ = w.Write([]byte(`{"error":4}`))
	}))
	t.Cleanup(ds.Close)
	cfg := &config.Office{OnlyOfficeURL: ds.URL + "/", InboxSecret: "inbox"}

	saving, err := requestForceSave(context.Background(), cfg, "a-key")
	require.NoError(t, err)
	assert.False(t, saving)

	claims := jwt.MapClaims{}
	_, err = jwt.ParseWithClaims(<-tokens, claims, func(*jwt.Token) (interface{}, error) {
		return []byte("inbox"), nil
	})
	require.NoError(t, err)
	assert.Equal(t, "forcesave", claims["c"])
	assert.Equal(t, "a-key", claims["key"])
	exp, err := claims.GetExpirationTime()
	require.NoError(t, err)
	require.NotNil(t, exp)
	assert.WithinDuration(t, time.Now().Add(commandTokenTTL), exp.Time, 5*time.Second)
	iat, err := claims.GetIssuedAt()
	require.NoError(t, err)
	require.NotNil(t, iat)
}
