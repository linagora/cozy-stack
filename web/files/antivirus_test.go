package files

import (
	"net/http"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckAntivirusAction(t *testing.T) {
	config.UseTestFile(t)

	t.Run("NilFile", func(t *testing.T) {
		inst := &instance.Instance{}
		err := CheckAntivirusAction(inst, nil, ActionDownload)
		assert.NoError(t, err)
	})

	t.Run("AntivirusDisabled", func(t *testing.T) {
		inst := &instance.Instance{ContextName: "disabled-context"}
		file := &vfs.FileDoc{
			AntivirusScan: &vfs.AntivirusScan{
				Status: vfs.AVStatusInfected,
			},
		}
		// No antivirus config means disabled - should allow all actions
		err := CheckAntivirusAction(inst, file, ActionDownload)
		assert.NoError(t, err)
	})

	t.Run("FileWithNoScanInfo", func(t *testing.T) {
		// Configure antivirus that blocks download for pending but allows for clean
		conf := config.GetConfig()
		if conf.Contexts == nil {
			conf.Contexts = make(map[string]interface{})
		}
		conf.Contexts["no-scan-test"] = map[string]interface{}{
			"antivirus": map[string]interface{}{
				"enabled": true,
				"actions": map[string]interface{}{
					"pending": []interface{}{},                                         // Block all for pending
					"clean":   []interface{}{"download", "share", "preview", "delete"}, // Allow all for clean
				},
			},
		}
		t.Cleanup(func() {
			delete(conf.Contexts, "no-scan-test")
		})

		inst := &instance.Instance{ContextName: "no-scan-test"}
		file := &vfs.FileDoc{} // No AntivirusScan - defaults to "clean"

		// Should be allowed because default is "clean"
		err := CheckAntivirusAction(inst, file, ActionDownload)
		assert.NoError(t, err)
	})

	t.Run("CleanFileAllowsAllActions", func(t *testing.T) {
		conf := config.GetConfig()
		if conf.Contexts == nil {
			conf.Contexts = make(map[string]interface{})
		}
		conf.Contexts["clean-test"] = map[string]interface{}{
			"antivirus": map[string]interface{}{
				"enabled": true,
				"actions": map[string]interface{}{
					"clean": []interface{}{"download", "share", "preview", "delete"},
				},
			},
		}
		t.Cleanup(func() {
			delete(conf.Contexts, "clean-test")
		})

		inst := &instance.Instance{ContextName: "clean-test"}
		file := &vfs.FileDoc{
			AntivirusScan: &vfs.AntivirusScan{
				Status: vfs.AVStatusClean,
			},
		}

		assert.NoError(t, CheckAntivirusAction(inst, file, ActionDownload))
		assert.NoError(t, CheckAntivirusAction(inst, file, ActionShare))
		assert.NoError(t, CheckAntivirusAction(inst, file, ActionPreview))
		assert.NoError(t, CheckAntivirusAction(inst, file, ActionDelete))
	})

	t.Run("InfectedFileBlocksDownload", func(t *testing.T) {
		conf := config.GetConfig()
		if conf.Contexts == nil {
			conf.Contexts = make(map[string]interface{})
		}
		conf.Contexts["infected-test"] = map[string]interface{}{
			"antivirus": map[string]interface{}{
				"enabled": true,
				"actions": map[string]interface{}{
					"infected": []interface{}{"delete"}, // Only delete allowed
				},
			},
		}
		t.Cleanup(func() {
			delete(conf.Contexts, "infected-test")
		})

		inst := &instance.Instance{ContextName: "infected-test"}
		file := &vfs.FileDoc{
			AntivirusScan: &vfs.AntivirusScan{
				Status:    vfs.AVStatusInfected,
				VirusName: "EICAR-Test-File",
			},
		}

		// Download should be blocked
		err := CheckAntivirusAction(inst, file, ActionDownload)
		require.Error(t, err)
		jsonErr, ok := err.(*jsonapi.Error)
		require.True(t, ok)
		assert.Equal(t, http.StatusUnavailableForLegalReasons, jsonErr.Status)
		assert.Equal(t, "antivirus_blocked", jsonErr.Code)
		assert.Contains(t, jsonErr.Detail, "download")
		assert.Contains(t, jsonErr.Detail, "infected")
		assert.Contains(t, jsonErr.Detail, "EICAR-Test-File")

		// Share should be blocked
		err = CheckAntivirusAction(inst, file, ActionShare)
		require.Error(t, err)

		// Preview should be blocked
		err = CheckAntivirusAction(inst, file, ActionPreview)
		require.Error(t, err)

		// Delete should be allowed
		err = CheckAntivirusAction(inst, file, ActionDelete)
		assert.NoError(t, err)
	})

	t.Run("PendingFileConfigurable", func(t *testing.T) {
		conf := config.GetConfig()
		if conf.Contexts == nil {
			conf.Contexts = make(map[string]interface{})
		}
		conf.Contexts["pending-test"] = map[string]interface{}{
			"antivirus": map[string]interface{}{
				"enabled": true,
				"actions": map[string]interface{}{
					"pending": []interface{}{"preview", "delete"}, // Only preview and delete allowed
				},
			},
		}
		t.Cleanup(func() {
			delete(conf.Contexts, "pending-test")
		})

		inst := &instance.Instance{ContextName: "pending-test"}
		file := &vfs.FileDoc{
			AntivirusScan: &vfs.AntivirusScan{
				Status: vfs.AVStatusPending,
			},
		}

		// Download blocked
		err := CheckAntivirusAction(inst, file, ActionDownload)
		require.Error(t, err)

		// Share blocked
		err = CheckAntivirusAction(inst, file, ActionShare)
		require.Error(t, err)

		// Preview allowed
		err = CheckAntivirusAction(inst, file, ActionPreview)
		assert.NoError(t, err)

		// Delete allowed
		err = CheckAntivirusAction(inst, file, ActionDelete)
		assert.NoError(t, err)
	})

	t.Run("UnconfiguredStatusAllowsAll", func(t *testing.T) {
		conf := config.GetConfig()
		if conf.Contexts == nil {
			conf.Contexts = make(map[string]interface{})
		}
		conf.Contexts["unconfigured-test"] = map[string]interface{}{
			"antivirus": map[string]interface{}{
				"enabled": true,
				"actions": map[string]interface{}{
					"clean": []interface{}{"download"}, // Only clean is configured
				},
			},
		}
		t.Cleanup(func() {
			delete(conf.Contexts, "unconfigured-test")
		})

		inst := &instance.Instance{ContextName: "unconfigured-test"}
		file := &vfs.FileDoc{
			AntivirusScan: &vfs.AntivirusScan{
				Status: vfs.AVStatusSkipped, // Not configured in actions
			},
		}

		// Should allow all actions when status not configured
		assert.NoError(t, CheckAntivirusAction(inst, file, ActionDownload))
		assert.NoError(t, CheckAntivirusAction(inst, file, ActionShare))
		assert.NoError(t, CheckAntivirusAction(inst, file, ActionPreview))
		assert.NoError(t, CheckAntivirusAction(inst, file, ActionDelete))
	})
}

func TestFilterAntivirusBlockedDocs(t *testing.T) {
	config.UseTestFile(t)

	setAntivirusContext := func(t *testing.T, name string, antivirus map[string]interface{}) *instance.Instance {
		conf := config.GetConfig()
		if conf.Contexts == nil {
			conf.Contexts = make(map[string]interface{})
		}
		conf.Contexts[name] = map[string]interface{}{"antivirus": antivirus}
		t.Cleanup(func() {
			delete(conf.Contexts, name)
		})
		return &instance.Instance{ContextName: name}
	}

	makeChange := func(id, seq, docType, status string) couchdb.Change {
		doc := map[string]interface{}{
			"_id":  id,
			"_rev": "1-" + id,
			"type": docType,
			"name": id,
		}
		if status != "" {
			doc["antivirus_scan"] = map[string]interface{}{"status": status}
		}
		change := couchdb.Change{
			DocID: id,
			Seq:   seq,
			Doc:   couchdb.JSONDoc{M: doc, Type: consts.Files},
		}
		change.Changes = append(change.Changes, struct {
			Rev string `json:"rev"`
		}{Rev: "1-" + id})
		return change
	}

	makeTrashedChange := func(id, seq, status string) couchdb.Change {
		change := makeChange(id, seq, consts.FileType, status)
		change.Doc.M["trashed"] = true
		return change
	}

	makeDeletedChange := func(id, seq string) couchdb.Change {
		return *couchdb.MakeChangeForDeletion(id, "2-"+id, seq)
	}

	assertTombstone := func(t *testing.T, change couchdb.Change, id, seq string) {
		t.Helper()
		assert.True(t, change.Deleted)
		assert.Equal(t, id, change.DocID)
		assert.Equal(t, seq, change.Seq)
		assert.Equal(t, "1-"+id, change.Doc.M["_rev"])
		assert.Equal(t, true, change.Doc.M["_deleted"])
		assert.NotContains(t, change.Doc.M, "name")
		assert.NotContains(t, change.Doc.M, "antivirus_scan")
	}

	t.Run("DefaultActions", func(t *testing.T) {
		inst := setAntivirusContext(t, "filter-default", map[string]interface{}{
			"enabled": true,
		})
		changes := &couchdb.ChangesResponse{
			LastSeq: "5",
			Pending: 2,
			Results: []couchdb.Change{
				makeChange("infected", "1", consts.FileType, vfs.AVStatusInfected),
				makeChange("pending", "2", consts.FileType, vfs.AVStatusPending),
				makeChange("error", "3", consts.FileType, vfs.AVStatusError),
				makeChange("clean", "4", consts.FileType, vfs.AVStatusClean),
				makeChange("noscan", "5", consts.FileType, ""),
			},
		}

		FilterAntivirusBlockedDocs(inst, changes)

		require.Len(t, changes.Results, 5)
		assertTombstone(t, changes.Results[0], "infected", "1")
		assert.Equal(t, makeChange("pending", "2", consts.FileType, vfs.AVStatusPending), changes.Results[1])
		assert.Equal(t, makeChange("error", "3", consts.FileType, vfs.AVStatusError), changes.Results[2])
		assert.Equal(t, makeChange("clean", "4", consts.FileType, vfs.AVStatusClean), changes.Results[3])
		assert.Equal(t, makeChange("noscan", "5", consts.FileType, ""), changes.Results[4])
		assert.Equal(t, "5", changes.LastSeq)
		assert.Equal(t, 2, changes.Pending)
	})

	t.Run("BlockedPendingIsRemoved", func(t *testing.T) {
		inst := setAntivirusContext(t, "filter-pending", map[string]interface{}{
			"enabled": true,
			"actions": map[string]interface{}{
				"pending":  []interface{}{"delete"},
				"infected": []interface{}{"delete"},
			},
		})
		changes := &couchdb.ChangesResponse{
			LastSeq: "4",
			Results: []couchdb.Change{
				makeChange("before", "1", consts.FileType, vfs.AVStatusClean),
				makeChange("pending", "2", consts.FileType, vfs.AVStatusPending),
				makeChange("infected", "3", consts.FileType, vfs.AVStatusInfected),
				makeChange("after", "4", consts.FileType, ""),
			},
		}

		FilterAntivirusBlockedDocs(inst, changes)

		require.Len(t, changes.Results, 2)
		assert.Equal(t, makeChange("before", "1", consts.FileType, vfs.AVStatusClean), changes.Results[0])
		assert.Equal(t, makeChange("after", "4", consts.FileType, ""), changes.Results[1])
		assert.Equal(t, "4", changes.LastSeq)
	})

	t.Run("BlockedErrorIsRemovedWhenPendingIsBlocked", func(t *testing.T) {
		inst := setAntivirusContext(t, "filter-error-pending-blocked", map[string]interface{}{
			"enabled": true,
			"actions": map[string]interface{}{
				"pending": []interface{}{"delete"},
				"error":   []interface{}{"delete"},
			},
		})
		changes := &couchdb.ChangesResponse{
			Results: []couchdb.Change{
				makeChange("error", "1", consts.FileType, vfs.AVStatusError),
			},
		}

		FilterAntivirusBlockedDocs(inst, changes)

		assert.Empty(t, changes.Results)
	})

	t.Run("BlockedErrorIsTombstonedWhenPendingIsAllowed", func(t *testing.T) {
		inst := setAntivirusContext(t, "filter-error", map[string]interface{}{
			"enabled": true,
			"actions": map[string]interface{}{
				"error": []interface{}{"delete"},
			},
		})
		changes := &couchdb.ChangesResponse{
			Results: []couchdb.Change{
				makeChange("error", "1", consts.FileType, vfs.AVStatusError),
			},
		}

		FilterAntivirusBlockedDocs(inst, changes)

		require.Len(t, changes.Results, 1)
		assertTombstone(t, changes.Results[0], "error", "1")
	})

	t.Run("BlockedSkippedIsRemoved", func(t *testing.T) {
		inst := setAntivirusContext(t, "filter-skipped", map[string]interface{}{
			"enabled": true,
			"actions": map[string]interface{}{
				"skipped": []interface{}{"delete"},
			},
		})
		changes := &couchdb.ChangesResponse{
			Results: []couchdb.Change{
				makeChange("skipped", "1", consts.FileType, vfs.AVStatusSkipped),
			},
		}

		FilterAntivirusBlockedDocs(inst, changes)

		assert.Empty(t, changes.Results)
	})

	t.Run("TrashedBlockedIsKeptWhenPendingIsBlocked", func(t *testing.T) {
		inst := setAntivirusContext(t, "filter-trashed-pending-blocked", map[string]interface{}{
			"enabled": true,
			"actions": map[string]interface{}{
				"pending":  []interface{}{"delete"},
				"infected": []interface{}{"delete"},
			},
		})
		changes := &couchdb.ChangesResponse{
			Results: []couchdb.Change{
				makeTrashedChange("infected", "1", vfs.AVStatusInfected),
				makeTrashedChange("pending", "2", vfs.AVStatusPending),
			},
		}

		FilterAntivirusBlockedDocs(inst, changes)

		require.Len(t, changes.Results, 2)
		assert.Equal(t, makeTrashedChange("infected", "1", vfs.AVStatusInfected), changes.Results[0])
		assert.Equal(t, makeTrashedChange("pending", "2", vfs.AVStatusPending), changes.Results[1])
	})

	t.Run("TrashedBlockedIsKeptWhenPendingIsAllowed", func(t *testing.T) {
		inst := setAntivirusContext(t, "filter-trashed-default", map[string]interface{}{
			"enabled": true,
		})
		changes := &couchdb.ChangesResponse{
			Results: []couchdb.Change{
				makeTrashedChange("infected", "1", vfs.AVStatusInfected),
			},
		}

		FilterAntivirusBlockedDocs(inst, changes)

		require.Len(t, changes.Results, 1)
		assert.Equal(t, makeTrashedChange("infected", "1", vfs.AVStatusInfected), changes.Results[0])
	})

	t.Run("AntivirusDisabled", func(t *testing.T) {
		inst := setAntivirusContext(t, "filter-disabled", map[string]interface{}{
			"enabled": false,
			"actions": map[string]interface{}{
				"infected": []interface{}{"delete"},
			},
		})
		changes := &couchdb.ChangesResponse{
			Results: []couchdb.Change{
				makeChange("infected", "1", consts.FileType, vfs.AVStatusInfected),
			},
		}

		FilterAntivirusBlockedDocs(inst, changes)

		require.Len(t, changes.Results, 1)
		assert.Equal(t, makeChange("infected", "1", consts.FileType, vfs.AVStatusInfected), changes.Results[0])
	})

	t.Run("NonFileChangesAreKept", func(t *testing.T) {
		inst := setAntivirusContext(t, "filter-non-file", map[string]interface{}{
			"enabled": true,
		})
		changes := &couchdb.ChangesResponse{
			Results: []couchdb.Change{
				makeChange("dir", "1", consts.DirType, vfs.AVStatusInfected),
				makeDeletedChange("deleted", "2"),
			},
		}

		FilterAntivirusBlockedDocs(inst, changes)

		require.Len(t, changes.Results, 2)
		assert.Equal(t, makeChange("dir", "1", consts.DirType, vfs.AVStatusInfected), changes.Results[0])
		assert.Equal(t, makeDeletedChange("deleted", "2"), changes.Results[1])
	})
}
