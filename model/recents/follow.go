package recents

import (
	"encoding/json"
	"errors"
	"net/url"
	"runtime"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/logger"
)

// WorkerSource is the worker that follows, in the rows of a user, a source
// instance that has moved or is deleted.
const WorkerSource = "recents-source"

func init() {
	job.AddWorker(&job.WorkerConfig{
		WorkerType:   WorkerSource,
		Concurrency:  runtime.NumCPU(),
		MaxExecCount: 2,
		Reserved:     true,
		Timeout:      10 * time.Minute,
		WorkerFunc:   workSource,
	})
}

// SourceMessage is the message of the recents-source jobs. The rows of the
// instance of the job on the items of the old source instance are migrated to
// the instance of NewDocID, or deleted when NewDocID is empty. The successor
// is given by its ID, as its domain may be given to another instance before
// the job runs.
type SourceMessage struct {
	OldDocID string `json:"old_docid"`
	NewDocID string `json:"new_docid,omitempty"`
}

func workSource(ctx *job.TaskContext) error {
	var msg SourceMessage
	if err := ctx.UnmarshalMessage(&msg); err != nil {
		return err
	}
	user := ctx.Instance
	if msg.NewDocID == "" {
		return DropSource(user, msg.OldDocID)
	}
	insts, err := instance.GetByIDs([]string{msg.NewDocID})
	if err != nil {
		return err
	}
	to := insts[msg.NewDocID]
	if to == nil {
		// The successor has been deleted since the move.
		return DropSource(user, msg.OldDocID)
	}
	return MigrateSource(user, msg.OldDocID, to)
}

// FollowMove pushes a recents-source job to each member, on this stack, of
// the sharings of an instance that has moved: their rows on its items are
// migrated to its successor, at the end of its chain of moves, or deleted when
// the target is not on this stack. When the target cannot be told, no job is
// pushed: the rows may still have a successor, and the reads hide them.
func FollowMove(inst *instance.Instance) error {
	push, err := sourceJobs(inst, followTarget)
	if err != nil {
		return err
	}
	push()
	return nil
}

// PrepareDestroy reads what is needed to follow the items of an instance being
// destroyed in the recents of the members of its sharings, and returns the
// function, never nil, that pushes their jobs. It reads the sharings and the
// settings of the instance, so it must be called before its databases are
// deleted, and the jobs pushed after. When the successor cannot be checked, no
// job is pushed, and the reads handle the rows.
func PrepareDestroy(inst *instance.Instance) func() {
	push, err := sourceJobs(inst, destroyTarget)
	if err != nil {
		inst.Logger().WithNamespace("recents").
			Warnf("Cannot find the successor of the instance: %s", err)
		return func() {}
	}
	return push
}

// sourceJobs returns the function that pushes a recents-source job to each
// member of the sharings of a source instance, for the successor given by
// target, which is only looked for when there are members. On an error, the
// successor may exist, and no job must be pushed.
func sourceJobs(src *instance.Instance, target func(*instance.Instance) (*instance.Instance, error)) (func(), error) {
	members := sourceMembers(src)
	if len(members) == 0 {
		return func() {}, nil
	}
	to, err := target(src)
	if err != nil {
		return nil, err
	}
	return func() { pushSourceJobs(members, src.DocID, to) }, nil
}

// followTarget returns the successor on this stack of a moved instance, to
// which the rows on its items are migrated, or nil when they are to be
// deleted, as the target is not on this stack. An error, errUnknownMove
// included, means that the rows must be left as they are.
func followTarget(inst *instance.Instance) (*instance.Instance, error) {
	next, err := followMoves(inst.DocID, inst, inst.Domain)
	if errors.Is(err, instance.ErrNotFound) {
		return nil, nil
	}
	return next, err
}

// destroyTarget returns the instance to which the rows on the items of an
// instance being destroyed are migrated: the successor on this stack of a
// moved instance, deleted by the manager a month after its move, so that the
// rows missed by the jobs of the move are not lost. It is nil when the rows
// are to be deleted: for an instance that has not moved, whose domain may be
// declared as old domain by an unrelated instance once it was given back, or
// once the successor can no longer be found. On an error, the successor may
// exist, and no job must be pushed.
func destroyTarget(inst *instance.Instance) (*instance.Instance, error) {
	if !inst.Moved {
		return nil, nil
	}
	target, err := followTarget(inst)
	if errors.Is(err, errUnknownMove) {
		return nil, nil
	}
	return target, err
}

// pushSourceJobs pushes a recents-source job to each member: their rows on the
// items of the source instance are migrated to the instance to, or deleted
// when to is nil.
func pushSourceJobs(members []*instance.Instance, srcDocID string, to *instance.Instance) {
	sched := job.SystemUnsafe()
	if sched == nil || len(members) == 0 {
		return
	}
	src := SourceMessage{OldDocID: srcDocID}
	if to != nil {
		src.NewDocID = to.DocID
	}
	msg, err := job.NewMessage(src)
	if err != nil {
		logger.WithNamespace("recents").
			Warnf("Cannot build the %s jobs of %s: %s", WorkerSource, srcDocID, err)
		return
	}
	for _, member := range members {
		_, err := sched.PushJob(member, &job.JobRequest{
			WorkerType: WorkerSource,
			Message:    msg,
		})
		if err != nil {
			member.Logger().WithNamespace("recents").
				Warnf("Cannot push the %s job of %s: %s", WorkerSource, srcDocID, err)
		}
	}
}

// sharingMembers is the part of an io.cozy.sharings document that lists the
// Cozy URLs of its members.
type sharingMembers struct {
	Members []struct {
		Instance string `json:"instance,omitempty"`
	} `json:"members"`
}

// sourceMembers returns the instances of this stack that are members of a
// sharing of a source instance, the only ones whose rows can hold its items,
// and whose recents are enabled. A former member removed from the sharings is
// not found, and the rows of a member whose recents are disabled are not
// served: the reads still migrate or delete them.
func sourceMembers(src *instance.Instance) []*instance.Instance {
	log := src.Logger().WithNamespace("recents")
	hosts := make(map[string]bool)
	err := couchdb.ForeachDocs(src, consts.Sharings, func(_ string, doc json.RawMessage) error {
		var s sharingMembers
		if err := json.Unmarshal(doc, &s); err != nil {
			return nil
		}
		for _, m := range s.Members {
			if u, err := url.Parse(m.Instance); err == nil && u.Host != "" {
				hosts[u.Host] = true
			}
		}
		return nil
	})
	if err != nil && !couchdb.IsNoDatabaseError(err) {
		log.Warnf("Cannot list the members of the sharings: %s", err)
	}

	domains := make([]string, 0, len(hosts))
	for host := range hosts {
		if !src.HasDomain(host) {
			domains = append(domains, host)
		}
	}
	insts, err := instance.GetByDomains(domains)
	if err != nil {
		log.Warnf("Cannot load the instances of the members: %s", err)
		return nil
	}

	var members []*instance.Instance
	seen := make(map[string]bool) // a member may be listed under several hosts
	for _, member := range insts {
		if seen[member.DocID] {
			continue
		}
		seen[member.DocID] = true
		if Enabled(member) {
			members = append(members, member)
		}
	}
	return members
}

// sourceBatchSize is the number of rows of a source read and written at once.
const sourceBatchSize = 100

// MigrateSource rewrites the rows of a user on the items of a source instance
// that has moved under the keys of its successor, to which the file IDs are
// kept. The dates of a row already stored under the new key are merged with
// their maximum, so that calling it again is a no-op. The user's own rows are
// never migrated, but when the user is the successor, the migrated rows become
// rows on their own items, which they now are.
func MigrateSource(user *instance.Instance, srcDocID string, to *instance.Instance) error {
	if srcDocID == user.DocID || srcDocID == to.DocID {
		return nil
	}
	return eachSourceBatch(user, srcDocID, func(batch []*FileDirActivity) error {
		fileIDs := make([]string, len(batch))
		keys := make([]string, len(batch))
		for i, row := range batch {
			_, fileIDs[i], _ = ParseResourceKey(row.DocID)
			keys[i] = ResourceKey(to, fileIDs[i])
		}
		existing, err := GetRows(user, keys)
		if err != nil {
			return err
		}
		var repairs Repairs
		for i, row := range batch {
			repairs.Migrate(row, existing[keys[i]], to, fileIDs[i])
		}
		return repairs.write(user)
	})
}

// DropSource deletes the rows of a user on the items of a source instance that
// is no longer on this stack and has no successor on it: they can never be
// shown again. The user's own rows are never deleted.
func DropSource(user *instance.Instance, srcDocID string) error {
	if srcDocID == user.DocID {
		return nil
	}
	return eachSourceBatch(user, srcDocID, func(batch []*FileDirActivity) error {
		var repairs Repairs
		for _, row := range batch {
			repairs.Delete(row)
		}
		return repairs.write(user)
	})
}

// eachSourceBatch calls fn with the rows of a user on the items of a source
// instance, by batches read from the range of their keys in the primary
// index: the keys of a source share the prefix of its ID. fn deletes or
// rewrites the rows it is given, and a row whose change has failed is not
// given again.
func eachSourceBatch(user *instance.Instance, srcDocID string, fn func([]*FileDirActivity) error) error {
	if srcDocID == "" {
		return nil
	}
	prefix := srcDocID + ":"
	start := prefix
	for {
		req := &couchdb.AllDocsRequest{
			StartKey: start,
			EndKey:   prefix + couchdb.MaxString,
			Limit:    sourceBatchSize,
		}
		var docs []*FileDirActivity
		err := couchdb.GetAllDocs(user, consts.FilesRecents, req, &docs)
		if couchdb.IsNoDatabaseError(err) {
			return nil
		}
		if err != nil {
			return err
		}

		batch := make([]*FileDirActivity, 0, len(docs))
		for _, doc := range docs {
			// The range starts with the last row of the previous batch: it is
			// still there when its change has failed.
			if doc == nil || doc.DocID == start {
				continue
			}
			if _, _, ok := ParseResourceKey(doc.DocID); ok {
				batch = append(batch, doc)
			}
		}
		if len(batch) > 0 {
			if err := fn(batch); err != nil {
				return err
			}
		}
		if len(docs) < sourceBatchSize {
			return nil
		}
		start = docs[len(docs)-1].DocID
	}
}
