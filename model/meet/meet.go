// Package meet creates video meeting rooms on a LaSuite Meet server, with the
// external API of Meet: an application exchanges its credentials for a token
// on behalf of the email address of the instance, then creates the room.
package meet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
)

// ErrNotConfigured is returned when no Meet server is configured for the
// context of the instance.
var ErrNotConfigured = errors.New("no Meet server configured")

// ErrNoEmail is returned when the instance has no email address, on behalf of
// which the room is created.
var ErrNoEmail = errors.New("the instance has no email address")

// Room is a video meeting room created on Meet.
type Room struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	URL         string `json:"url"`
	AccessLevel string `json:"access_level,omitempty"`
}

var meetHTTPClient = &http.Client{Timeout: 30 * time.Second}

// IsConfigured tells whether rooms can be created for the instance.
func IsConfigured(inst *instance.Instance) bool {
	server := inst.MeetServer()
	return server.URL != "" && server.ClientID != "" && server.ClientSecret != ""
}

// CreateRoom creates a room on the Meet server of the instance, owned by the
// user of the instance.
func CreateRoom(ctx context.Context, inst *instance.Instance) (*Room, error) {
	if !IsConfigured(inst) {
		return nil, ErrNotConfigured
	}
	email, err := inst.SettingsEMail()
	if err != nil {
		return nil, err
	}
	if email == "" {
		return nil, ErrNoEmail
	}
	return createRoom(ctx, inst.MeetServer(), email)
}

func createRoom(ctx context.Context, server config.MeetServer, email string) (*Room, error) {
	token, err := fetchToken(ctx, server, email)
	if err != nil {
		return nil, err
	}
	var room Room
	err = callMeet(ctx, server, "/external-api/v1.0/rooms/", token, map[string]interface{}{}, &room)
	if err != nil {
		return nil, fmt.Errorf("cannot create the room: %w", err)
	}
	if room.URL == "" {
		return nil, errors.New("cannot create the room: no URL in the answer of Meet")
	}
	return &room, nil
}

func fetchToken(ctx context.Context, server config.MeetServer, email string) (string, error) {
	payload := map[string]interface{}{
		"client_id":     server.ClientID,
		"client_secret": server.ClientSecret,
		"grant_type":    "client_credentials",
		// The scope of Meet's application tokens is the user they act for.
		"scope": email,
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	err := callMeet(ctx, server, "/external-api/v1.0/application/token/", "", payload, &token)
	if err != nil {
		return "", fmt.Errorf("cannot get a token from Meet: %w", err)
	}
	if token.AccessToken == "" {
		return "", errors.New("cannot get a token from Meet: no access token")
	}
	return token.AccessToken, nil
}

func callMeet(ctx context.Context, server config.MeetServer, path, token string, payload interface{}, out interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	u := strings.TrimSuffix(server.URL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := meetHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, res.Body)
		return fmt.Errorf("POST %s: status code %d", path, res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(out)
}
