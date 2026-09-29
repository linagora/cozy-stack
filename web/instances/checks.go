package instances

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/labstack/echo/v4"
)

func fsckHandler(c echo.Context) (err error) {
	domain := c.Param("domain")
	i, err := lifecycle.GetInstance(domain)
	if err != nil {
		return wrapError(err)
	}

	indexIntegrityCheck, _ := strconv.ParseBool(c.QueryParam("IndexIntegrity"))
	filesConsistencyCheck, _ := strconv.ParseBool(c.QueryParam("FilesConsistency"))
	failFast, _ := strconv.ParseBool(c.QueryParam("FailFast"))

	logCh := make(chan *vfs.FsckLog)
	go func() {
		fs := i.VFS()
		if indexIntegrityCheck {
			err = fs.CheckIndexIntegrity(func(log *vfs.FsckLog) { logCh <- log }, failFast)
		} else if filesConsistencyCheck {
			err = fs.CheckFilesConsistency(func(log *vfs.FsckLog) { logCh <- log }, failFast)
		} else {
			err = fs.Fsck(func(log *vfs.FsckLog) { logCh <- log }, failFast)
		}
		close(logCh)
	}()

	w := c.Response().Writer
	w.WriteHeader(200)
	encoder := json.NewEncoder(w)
	for log := range logCh {
		// XXX do not serialize to JSON the children and the cozyMetadata, as
		// it can take more than 64ko and scanner will ignore such lines.
		if log.FileDoc != nil {
			log.FileDoc.DirsChildren = nil  // It can be filled on type mismatch
			log.FileDoc.FilesChildren = nil // Idem
			log.FileDoc.FilesChildrenSize = 0
			log.FileDoc.Metadata = nil
		}
		if log.DirDoc != nil {
			log.DirDoc.DirsChildren = nil
			log.DirDoc.FilesChildren = nil
			log.DirDoc.FilesChildrenSize = 0
			log.DirDoc.Metadata = nil
		}
		if log.VersionDoc != nil {
			log.VersionDoc.Metadata = nil
		}
		if errenc := encoder.Encode(log); errenc != nil {
			i.Logger().WithNamespace("fsck").
				Warnf("Cannot encode to JSON: %s (%v)", errenc, log)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	if err != nil {
		log := map[string]string{"error": err.Error()}
		if couchdb.IsNotFoundError(err) {
			log = map[string]string{"type": "no_database", "error": err.Error()}
		}
		if errenc := encoder.Encode(log); errenc != nil {
			i.Logger().WithNamespace("fsck").
				Warnf("Cannot encode to JSON: %s (%v)", errenc, log)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	return nil
}

func checkTriggers(c echo.Context) error {
	domain := c.Param("domain")
	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return wrapError(err)
	}

	type TriggerInfo struct {
		TID        string          `json:"_id"`
		Type       string          `json:"type"`
		WorkerType string          `json:"worker"`
		Arguments  string          `json:"arguments"`
		Debounce   string          `json:"debounce"`
		Message    json.RawMessage `json:"message"`
	}
	var triggers []*TriggerInfo
	err = couchdb.ForeachDocs(inst, consts.Triggers, func(_ string, data json.RawMessage) error {
		var t *TriggerInfo
		if err := json.Unmarshal(data, &t); err != nil {
			return err
		}
		triggers = append(triggers, t)
		return nil
	})
	if err != nil {
		if couchdb.IsNotFoundError(err) {
			return c.JSON(http.StatusOK, []map[string]interface{}{
				{"type": "no_database", "error": err.Error()},
			})
		}
		return wrapError(err)
	}

	results := []map[string]interface{}{}
	for i, left := range triggers {
		for j, right := range triggers {
			if i >= j {
				continue
			}
			if left.Type == "@in" {
				// It doesn't make sense to compare @in triggers as they can
				// have scheduled at different times
				continue
			}
			if left.Type != right.Type {
				continue
			}
			if left.WorkerType != right.WorkerType {
				continue
			}
			if left.Arguments != right.Arguments {
				continue
			}
			if left.Debounce != right.Debounce {
				continue
			}
			lHasMessage := left.Message != nil
			rHasMessage := right.Message != nil
			if lHasMessage != rHasMessage {
				continue
			}
			if lHasMessage && rHasMessage {
				if !bytes.Equal(left.Message, right.Message) {
					continue
				}
			}
			results = append(results, map[string]interface{}{
				"type":      "duplicate",
				"_id":       left.TID,
				"other_id":  right.TID,
				"trigger":   left.Type,
				"worker":    left.WorkerType,
				"arguments": left.Arguments,
				"debounce":  left.Debounce,
				"message":   string(left.Message),
			})
			break
		}
	}

	for _, t := range triggers {
		if t.Type == "@event" && t.WorkerType == "service" {
			var msg map[string]interface{}
			err := json.Unmarshal(t.Message, &msg)
			if err != nil {
				results = append(results, map[string]interface{}{
					"type":    "invalid_message",
					"_id":     t.TID,
					"trigger": t.Type,
					"worker":  t.WorkerType,
					"message": string(t.Message),
				})
				continue
			}
			if name, _ := msg["name"].(string); name == "" {
				results = append(results, map[string]interface{}{
					"type":    "missing_name",
					"_id":     t.TID,
					"trigger": t.Type,
					"worker":  t.WorkerType,
					"message": string(t.Message),
				})
			}
			if slug, _ := msg["slug"].(string); slug == "" {
				results = append(results, map[string]interface{}{
					"type":    "missing_slug",
					"_id":     t.TID,
					"trigger": t.Type,
					"worker":  t.WorkerType,
					"message": string(t.Message),
				})
			}
		}
	}

	return c.JSON(http.StatusOK, results)
}

func checkShared(c echo.Context) error {
	domain := c.Param("domain")
	i, err := lifecycle.GetInstance(domain)
	if err != nil {
		return wrapError(err)
	}

	results, err := sharing.CheckShared(i)
	if err != nil {
		if couchdb.IsNotFoundError(err) {
			return c.JSON(http.StatusOK, []map[string]interface{}{
				{"type": "no_database", "error": err.Error()},
			})
		}
		if _, ok := err.(*json.SyntaxError); ok {
			return c.JSON(http.StatusOK, []map[string]interface{}{
				{"type": "invalid_json", "error": err.Error()},
			})
		}
		return wrapError(err)
	}
	return c.JSON(http.StatusOK, results)
}

func checkSharings(c echo.Context) error {
	domain := c.Param("domain")
	i, err := lifecycle.GetInstance(domain)
	if err != nil {
		return wrapError(err)
	}

	skipFSConsistency, _ := strconv.ParseBool(c.QueryParam("SkipFSConsistency"))

	results, err := sharing.CheckSharings(i, skipFSConsistency)
	if err != nil {
		if couchdb.IsNotFoundError(err) {
			return c.JSON(http.StatusOK, []map[string]interface{}{
				{"type": "no_database", "error": err.Error()},
			})
		}
		return wrapError(err)
	}
	return c.JSON(http.StatusOK, results)
}

func checkInternalEmails(c echo.Context) error {
	check, err := lifecycle.CheckInternalEmails()
	if err != nil {
		return wrapError(err)
	}
	return c.JSON(http.StatusOK, check)
}
