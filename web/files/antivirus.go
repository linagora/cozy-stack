package files

import (
	"net/http"
	"strings"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
)

const (
	ActionDownload = "download"
	ActionShare    = "share"
	ActionPreview  = "preview"
	ActionDelete   = "delete"
)

// CheckAntivirusAction checks if the requested action is allowed based on the file's
// antivirus scan status and the instance's antivirus configuration.
// Returns nil if allowed, or a jsonapi.Error with status 451 if blocked.
func CheckAntivirusAction(inst *instance.Instance, file *vfs.FileDoc, action string) error {
	if file == nil {
		return nil
	}
	avConfig := config.GetAntivirusConfig(inst.ContextName)
	if avConfig == nil || !avConfig.Enabled {
		return nil
	}
	status := getFileAntivirusStatus(file)
	if isActionAllowed(avConfig, status, action) {
		return nil
	}
	return newAntivirusBlockedError(file, action, status)
}

// FilterAntivirusBlockedDocs hides from a changes feed the files that the
// antivirus configuration forbids to download, so that sync clients do not
// try to fetch content that would be refused with a 451.
//
// A blocked file is dropped when the client could not have downloaded its
// blocked content, so that it keeps its previous version. Otherwise it is
// replaced by a tombstone, so that the client removes a content it may have
// downloaded while the scan was running. A blocked file moved to the trash is
// kept as is: only the download is forbidden, and trashing a file triggers
// none on the clients.
func FilterAntivirusBlockedDocs(inst *instance.Instance, changes *couchdb.ChangesResponse) {
	avConfig := config.GetAntivirusConfig(inst.ContextName)
	if avConfig == nil || !avConfig.Enabled || len(changes.Results) == 0 {
		return
	}

	pendingBlocked := !isActionAllowed(avConfig, vfs.AVStatusPending, ActionDownload)
	results := make([]couchdb.Change, 0, len(changes.Results))
	for _, change := range changes.Results {
		if change.Doc.M["type"] != consts.FileType {
			results = append(results, change)
			continue
		}
		status := getChangeAntivirusStatus(change.Doc)
		if isActionAllowed(avConfig, status, ActionDownload) {
			results = append(results, change)
			continue
		}
		trashed, _ := change.Doc.M["trashed"].(bool)
		switch decideBlockedChange(status, trashed, pendingBlocked) {
		case keepBlockedChange:
			results = append(results, change)
		case tombstoneBlockedChange:
			results = append(results, makeAntivirusBlockedChange(change))
		case dropBlockedChange:
		}
	}
	changes.Results = results
}

// filterAntivirusBlockedFiles leaves out of a listing the files that the
// antivirus configuration forbids to download, so that sync clients do not try
// to fetch them. A listing has no deletion to tell: no tombstone is needed. A
// file in the trash is kept, as in the changes feeds.
func filterAntivirusBlockedFiles(inst *instance.Instance, results []vfs.DirOrFileDoc) []vfs.DirOrFileDoc {
	avConfig := config.GetAntivirusConfig(inst.ContextName)
	if avConfig == nil || !avConfig.Enabled {
		return results
	}

	kept := make([]vfs.DirOrFileDoc, 0, len(results))
	for _, dof := range results {
		_, file := dof.Refine()
		if file != nil && !file.Trashed &&
			!isActionAllowed(avConfig, getFileAntivirusStatus(file), ActionDownload) {
			continue
		}
		kept = append(kept, dof)
	}
	return kept
}

// blockedChangeOutcome is what a changes feed shows of a blocked file.
type blockedChangeOutcome int

const (
	keepBlockedChange blockedChangeOutcome = iota
	dropBlockedChange
	tombstoneBlockedChange
)

// decideBlockedChange tells whether a blocked file is kept as is, dropped from
// a changes feed or replaced by a tombstone.
func decideBlockedChange(status string, trashed, pendingBlocked bool) blockedChangeOutcome {
	switch {
	case trashed:
		// Trashing a file triggers no download on the clients.
		return keepBlockedChange
	case status == vfs.AVStatusPending:
		// Every content change sets the file back to pending: a tombstone
		// would make the clients delete and download it again each time.
		return dropBlockedChange
	case status == vfs.AVStatusSkipped:
		// Skipped is written with the content: it was never downloadable.
		return dropBlockedChange
	case pendingBlocked:
		// The verdict follows a pending state, during which the clients
		// could not download the content.
		return dropBlockedChange
	default:
		// The clients may have downloaded the content during the scan.
		return tombstoneBlockedChange
	}
}

// makeAntivirusBlockedChange hides a file whose antivirus verdict forbids the
// download by telling the sync clients it was deleted.
func makeAntivirusBlockedChange(change couchdb.Change) couchdb.Change {
	var rev string
	if len(change.Changes) > 0 {
		rev = change.Changes[0].Rev
	}
	return *couchdb.MakeChangeForDeletion(change.DocID, rev, change.Seq)
}

// getChangeAntivirusStatus returns the antivirus status of a file document
// from a changes feed, with the same "clean" default as getFileAntivirusStatus.
func getChangeAntivirusStatus(doc couchdb.JSONDoc) string {
	scan, _ := doc.M["antivirus_scan"].(map[string]interface{})
	status, _ := scan["status"].(string)
	if status == "" {
		return vfs.AVStatusClean
	}
	return status
}

// getFileAntivirusStatus returns the antivirus status of a file.
// If the file has no AntivirusScan data, it returns "clean" as the default status.
func getFileAntivirusStatus(file *vfs.FileDoc) string {
	if file == nil {
		return vfs.AVStatusClean
	}
	if file.AntivirusScan == nil {
		return vfs.AVStatusClean
	}
	if file.AntivirusScan.Status == "" {
		return vfs.AVStatusClean
	}
	return file.AntivirusScan.Status
}

// isActionAllowed checks if the given action is in the list of allowed actions
// for the given antivirus status.
func isActionAllowed(cfg *config.AntivirusContextConfig, status, action string) bool {
	if cfg == nil || cfg.Actions == nil {
		return true
	}

	allowedActions, ok := cfg.Actions[status]
	if !ok {
		// Status not configured - default to allow for safety
		return true
	}

	for _, allowed := range allowedActions {
		if strings.EqualFold(allowed, action) {
			return true
		}
	}
	return false
}

// newAntivirusBlockedError creates a jsonapi.Error with status 451
// (Unavailable For Legal Reasons) for blocked antivirus actions.
func newAntivirusBlockedError(file *vfs.FileDoc, action, status string) *jsonapi.Error {
	detail := "Action '" + action + "' is blocked for files with antivirus status '" + status + "'"
	if status == vfs.AVStatusInfected && file != nil && file.AntivirusScan != nil && file.AntivirusScan.VirusName != "" {
		detail += " (detected: " + file.AntivirusScan.VirusName + ")"
	}

	return &jsonapi.Error{
		Status: http.StatusUnavailableForLegalReasons, // 451
		Title:  "Unavailable For Legal Reasons",
		Code:   "antivirus_blocked",
		Detail: detail,
	}
}
