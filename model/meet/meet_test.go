package meet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateRoom(t *testing.T) {
	var tokenRequest map[string]interface{}
	var roomAuthorization string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/external-api/v1.0/application/token/":
			require.NoError(t, json.NewDecoder(req.Body).Decode(&tokenRequest))
			_, _ = w.Write([]byte(`{"access_token": "jwt-for-alice", "token_type": "Bearer", "expires_in": 3600}`))
		case "/external-api/v1.0/rooms/":
			roomAuthorization = req.Header.Get("Authorization")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": "room-1", "slug": "abc-defg-hij", "url": "https://meet.example.net/abc-defg-hij", "access_level": "restricted"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	server := config.MeetServer{URL: srv.URL + "/", ClientID: "app-1", ClientSecret: "s3cr3t"}

	room, err := createRoom(context.Background(), server, "alice@example.net")
	require.NoError(t, err)
	assert.Equal(t, "https://meet.example.net/abc-defg-hij", room.URL)
	assert.Equal(t, "abc-defg-hij", room.Slug)
	assert.Equal(t, map[string]interface{}{
		"client_id":     "app-1",
		"client_secret": "s3cr3t",
		"grant_type":    "client_credentials",
		"scope":         "alice@example.net",
	}, tokenRequest, "the token acts on behalf of the user of the instance")
	assert.Equal(t, "Bearer jwt-for-alice", roomAuthorization)
}

func TestCreateRoomRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// The application is not authorized for the domain of the email.
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	server := config.MeetServer{URL: srv.URL, ClientID: "app-1", ClientSecret: "s3cr3t"}

	_, err := createRoom(context.Background(), server, "alice@example.net")
	assert.ErrorContains(t, err, "cannot get a token from Meet")
}
