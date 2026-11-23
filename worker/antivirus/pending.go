package antivirus

import (
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
)

const pendingScansBatchSize = 1000

// PushPendingScans pushes a scan job for every file whose antivirus status is
// still pending and that was last updated before the given time. Such a file
// has no verdict because its scan job failed or was lost, and it would stay
// pending forever. It returns the ids of the files for which a job was pushed.
func PushPendingScans(inst *instance.Instance, before time.Time) ([]string, error) {
	ids := []string{}
	cfg := config.GetAntivirusConfig(inst.ContextName)
	if cfg == nil || !cfg.Enabled {
		return ids, nil
	}

	sel := mango.And(
		mango.Equal("antivirus_scan.status", vfs.AVStatusPending),
		mango.Lt("updated_at", before),
	)
	bookmark := ""
	for {
		req := &couchdb.FindRequest{
			UseIndex: "by-antivirus-pending-updated-at",
			Selector: sel,
			Limit:    pendingScansBatchSize,
			Bookmark: bookmark,
		}
		var docs []*vfs.FileDoc
		res, err := couchdb.FindDocsRaw(inst, consts.Files, req, &docs)
		if err != nil {
			return ids, err
		}
		for _, doc := range docs {
			msg, err := job.NewMessage(&Message{FileID: doc.DocID})
			if err != nil {
				return ids, err
			}
			req := &job.JobRequest{WorkerType: "antivirus", Message: msg}
			if _, err := job.System().PushJob(inst, req); err != nil {
				return ids, err
			}
			ids = append(ids, doc.DocID)
		}
		if len(docs) < pendingScansBatchSize {
			return ids, nil
		}
		bookmark = res.Bookmark
	}
}
