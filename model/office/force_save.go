package office

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/metrics"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	jwt "github.com/golang-jwt/jwt/v5"
	"golang.org/x/sync/singleflight"
)

// ForceSaveTimeout bounds how long a reader waits for OnlyOffice to write the
// document of an open editing session, command and callback included.
var ForceSaveTimeout = 10 * time.Second

// ForceSaveDebounce is how long after a command to OnlyOffice, whatever its
// answer, the readers of the same document get the file as it is without
// asking again. A shared download link, opened by mail scanners, would
// otherwise send a command on each request.
const ForceSaveDebounce = 3 * time.Second

const forceSavePollInterval = 100 * time.Millisecond

// commandTokenTTL is how long the token of a command to OnlyOffice is valid:
// it is sent at once, the margin is for the clocks of the two servers.
const commandTokenTTL = time.Minute

// DocumentServerParam marks the URL the document server downloads a file
// from to open it: that download must not ask it to save the same session.
const DocumentServerParam = "DocumentServer"

// forceSaves lets the readers of a document in one process share one wait for
// the forcesave, instead of each asking the store. The store shares it between
// the stacks.
var forceSaves singleflight.Group

var forceSaveDebounce atomic.Int64

func init() {
	forceSaveDebounce.Store(int64(ForceSaveDebounce))
}

// SetForceSaveDebounce changes the delay of ForceSaveDebounce, for the tests.
func SetForceSaveDebounce(delay time.Duration) {
	forceSaveDebounce.Store(int64(delay))
}

func sessionID(db prefixer.Prefixer, key string) string {
	return db.DBPrefix() + "/" + key
}

// unconfirmed records that a file is served during an editing session
// without a confirmed forcesave, so maybe without the last changes.
func unconfirmed(inst *instance.Instance, file *vfs.FileDoc, reason string, err error) *vfs.FileDoc {
	metrics.OfficeForceSaveUnconfirmed.WithLabelValues(reason).Inc()
	inst.Logger().WithNamespace("office").
		Warnf("Serving %s without a confirmed forcesave (%s): %s", file.ID(), reason, err)
	return reread(inst, file)
}

// reread gives the current revision of the file: it may have been written
// while the reader was waiting, and its metadata must match its content.
func reread(inst *instance.Instance, file *vfs.FileDoc) *vfs.FileDoc {
	if current, err := inst.VFS().FileByID(file.ID()); err == nil {
		return current
	}
	return file
}

// SaveOpenSession asks OnlyOffice to save the document of the editing session
// opened on the file, and returns the file once the stack has written it.
// Without a session, or when OnlyOffice has nothing new to save, or past
// ForceSaveTimeout, the file is returned as is.
func SaveOpenSession(ctx context.Context, inst *instance.Instance, file *vfs.FileDoc) *vfs.FileDoc {
	cfg := GetConfig(inst.ContextName)
	if cfg == nil || cfg.OnlyOfficeURL == "" || !IsOfficeDocument(file) {
		return file
	}
	key, err := GetStore().GetSecretByID(inst, file.ID())
	if err != nil {
		return unconfirmed(inst, file, "store", err)
	}
	if key == "" {
		return file
	}
	if last, err := GetStore().LastForceSave(inst, key); err != nil {
		return unconfirmed(inst, file, "store", err)
	} else if last != nil {
		return file
	}

	ctx, cancel := context.WithTimeout(ctx, ForceSaveTimeout)
	defer cancel()

	// The forcesave has its own deadline: a reader that leaves must not cancel
	// it for the others.
	flight := forceSaves.DoChan(sessionID(inst, key), recovered(func() (interface{}, error) {
		flightCtx, cancel := context.WithTimeout(context.Background(), ForceSaveTimeout)
		defer cancel()
		return saveSession(flightCtx, inst, cfg, key)
	}))
	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.Canceled) {
			return file // the client has left, nobody gets this file
		}
		return unconfirmed(inst, file, "timeout", ctx.Err())
	case res := <-flight:
		if errors.Is(res.Err, context.DeadlineExceeded) {
			return unconfirmed(inst, file, "timeout", res.Err)
		}
		if res.Err != nil {
			return unconfirmed(inst, file, "error", res.Err)
		}
		if saved, _ := res.Val.(bool); !saved {
			return reread(inst, file)
		}
	}

	updated, err := inst.VFS().FileByID(file.ID())
	if err != nil {
		return unconfirmed(inst, file, "reread", err)
	}
	return updated
}

// recovered turns a panic of fn into an error: singleflight would panic again
// in a goroutine of its own, out of reach of the recovery of the HTTP server.
func recovered(fn func() (interface{}, error)) func() (interface{}, error) {
	return func() (res interface{}, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("forcesave: %v", r)
			}
		}()
		return fn()
	}
}

// saveSession sends the forcesave of the session of the key, or, when another
// stack is sending it, waits for its outcome. The outcome is recorded for the
// readers of the next ForceSaveDebounce.
func saveSession(ctx context.Context, inst *instance.Instance, cfg *config.Office, key string) (saved bool, err error) {
	store := GetStore()
	owner := makeSecret()
	ticker := time.NewTicker(forceSavePollInterval)
	defer ticker.Stop()
	for {
		// The margin lets the owner record its outcome after its own deadline.
		acquired, err := store.AcquireForceSave(inst, key, owner, ForceSaveTimeout+time.Second)
		if err != nil {
			return false, err
		}
		if acquired {
			break
		}
		last, err := store.LastForceSave(inst, key)
		if err != nil {
			return false, err
		}
		if last != nil {
			return last.Saved, last.err()
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-ticker.C:
		}
	}

	defer func() {
		outcome := ForceSaveOutcome{Saved: saved}
		if err != nil {
			outcome.Error = err.Error()
		}
		delay := time.Duration(forceSaveDebounce.Load())
		_ = store.FinishForceSave(inst, key, owner, outcome, delay)
	}()
	return forceSave(ctx, inst, cfg, key)
}

// forceSave asks OnlyOffice to save the session of the key, and reports true
// once the stack has written a newer revision than the one the reader has.
func forceSave(ctx context.Context, inst *instance.Instance, cfg *config.Office, key string) (bool, error) {
	before, err := GetStore().GetDoc(inst, key)
	if err != nil {
		return false, err
	}
	if before == nil {
		// The last editor has just left: the final save has written the file.
		return true, nil
	}
	saving, err := requestForceSave(ctx, cfg, key)
	if err != nil || !saving {
		return false, err
	}

	// OnlyOffice answers before it calls back the stack with the document, so
	// wait for the callback to record the new revision in the store.
	ticker := time.NewTicker(forceSavePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-ticker.C:
		}
		after, err := GetStore().GetDoc(inst, key)
		if err != nil {
			return false, err
		}
		if after == nil || after.Rev != before.Rev {
			return true, nil
		}
	}
}

// requestForceSave sends the forcesave command for the key to the command
// service of OnlyOffice. It reports false when there is no session for the key
// or nothing new to save.
// Cf https://api.onlyoffice.com/docs/docs-api/additional-api/command-service/forcesave/
func requestForceSave(ctx context.Context, cfg *config.Office, key string) (bool, error) {
	payload := map[string]interface{}{"c": "forcesave", "key": key}
	if cfg.InboxSecret != "" {
		now := time.Now()
		claims := jwt.MapClaims{
			"c":   "forcesave",
			"key": key,
			"iat": now.Unix(),
			"exp": now.Add(commandTokenTTL).Unix(),
		}
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
			SignedString([]byte(cfg.InboxSecret))
		if err != nil {
			return false, err
		}
		payload["token"] = token
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}

	u := strings.TrimSuffix(cfg.OnlyOfficeURL, "/") + "/coauthoring/CommandService.ashx"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := docserverClient.Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return false, fmt.Errorf("command service returned status %d", res.StatusCode)
	}

	var result struct {
		Error int `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		return false, err
	}
	switch result.Error {
	case 0:
		return true, nil
	case 1, 4: // no session for this key, or nothing new to save
		return false, nil
	default:
		return false, fmt.Errorf("command service returned error %d", result.Error)
	}
}
