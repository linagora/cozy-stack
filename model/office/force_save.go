package office

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
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

// ForceSaveDebounce is how long after a forcesave the readers of the same
// document get the saved revision without asking OnlyOffice again. A shared
// download link, opened by mail scanners, would otherwise make OnlyOffice
// assemble the document on each request.
var ForceSaveDebounce = 3 * time.Second

const forceSavePollInterval = 100 * time.Millisecond

// DocumentServerParam marks the URL the document server downloads a file
// from to open it: that download must not ask it to save the same session.
const DocumentServerParam = "DocumentServer"

// forceSaves lets the readers of a document during an editing session share
// one forcesave instead of each asking OnlyOffice for its own.
var forceSaves singleflight.Group

// lastForceSaves keeps, by session, when the last forcesave was confirmed.
// Each confirmation drops the entries older than ForceSaveDebounce. The entry
// of a closed session is not read again: its next session has a new key.
var lastForceSaves = struct {
	sync.Mutex
	at map[string]time.Time
}{at: make(map[string]time.Time)}

func sessionID(db prefixer.Prefixer, key string) string {
	return db.DBPrefix() + "/" + key
}

func recentlyForceSaved(id string) bool {
	lastForceSaves.Lock()
	defer lastForceSaves.Unlock()
	at, ok := lastForceSaves.at[id]
	return ok && time.Since(at) < ForceSaveDebounce
}

func markForceSaved(id string) {
	lastForceSaves.Lock()
	defer lastForceSaves.Unlock()
	now := time.Now()
	for other, at := range lastForceSaves.at {
		if now.Sub(at) >= ForceSaveDebounce {
			delete(lastForceSaves.at, other)
		}
	}
	lastForceSaves.at[id] = now
}

// unconfirmed records that a file is served during an editing session
// without a confirmed forcesave, so maybe without the last changes.
func unconfirmed(inst *instance.Instance, file *vfs.FileDoc, reason string, err error) *vfs.FileDoc {
	metrics.OfficeForceSaveUnconfirmed.WithLabelValues(reason).Inc()
	inst.Logger().WithNamespace("office").
		Warnf("Serving %s without a confirmed forcesave (%s): %s", file.ID(), reason, err)
	return file
}

// SaveOpenSession asks OnlyOffice to save the document of the editing session
// opened on the file, and returns the file once the stack has written it.
// Without a session, or when OnlyOffice has nothing new to save, or past
// ForceSaveTimeout, the file is returned as is.
func SaveOpenSession(ctx context.Context, inst *instance.Instance, file *vfs.FileDoc) *vfs.FileDoc {
	cfg := GetConfig(inst.ContextName)
	if cfg == nil || cfg.OnlyOfficeURL == "" {
		return file
	}
	key, err := GetStore().GetSecretByID(inst, file.ID())
	if err != nil {
		return unconfirmed(inst, file, "store", err)
	}
	if key == "" {
		return file
	}
	id := sessionID(inst, key)
	if recentlyForceSaved(id) {
		return file
	}

	ctx, cancel := context.WithTimeout(ctx, ForceSaveTimeout)
	defer cancel()

	// The forcesave has its own deadline: a reader that leaves must not cancel
	// it for the others.
	flight := forceSaves.DoChan(id, func() (interface{}, error) {
		flightCtx, cancel := context.WithTimeout(context.Background(), ForceSaveTimeout)
		defer cancel()
		return forceSave(flightCtx, inst, cfg, key)
	})
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
			return file
		}
	}

	updated, err := inst.VFS().FileByID(file.ID())
	if err != nil {
		return unconfirmed(inst, file, "reread", err)
	}
	return updated
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
			markForceSaved(sessionID(inst, key))
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
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims(payload)).
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
