package note

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/prosemirror-go/model"
	"github.com/cozy/prosemirror-go/transform"
)

// Step is a patch to apply on a note.
type Step map[string]interface{}

// ID returns the step qualified identifier
func (s Step) ID() string {
	id, _ := s["_id"].(string)
	return id
}

// Rev returns the step revision
func (s Step) Rev() string {
	rev, _ := s["_rev"].(string)
	return rev
}

// DocType returns the document type
func (s Step) DocType() string { return consts.NotesSteps }

// Clone implements couchdb.Doc
func (s Step) Clone() couchdb.Doc {
	cloned := make(Step)
	for k, v := range s {
		cloned[k] = v
	}
	return cloned
}

// SetID changes the step qualified identifier
func (s Step) SetID(id string) {
	if id == "" {
		delete(s, "_id")
	} else {
		s["_id"] = id
	}
}

// SetRev changes the step revision
func (s Step) SetRev(rev string) {
	if rev == "" {
		delete(s, "_rev")
	} else {
		s["_rev"] = rev
	}
}

// Included is part of the jsonapi.Object interface
func (s Step) Included() []jsonapi.Object { return nil }

// Links is part of the jsonapi.Object interface
func (s Step) Links() *jsonapi.LinksList { return nil }

// Relationships is part of the jsonapi.Object interface
func (s Step) Relationships() jsonapi.RelationshipMap { return nil }

func (s Step) timestamp() int64 {
	switch t := s["timestamp"].(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	}
	return 0
}

func (s Step) version() int64 {
	switch v := s["version"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

func stepID(noteID string, version int64) string {
	return fmt.Sprintf("%s/%08d", noteID, version)
}

func startkey(noteID string) string {
	return fmt.Sprintf("%s/", noteID)
}

func endkey(noteID string) string {
	return fmt.Sprintf("%s/%s", noteID, couchdb.MaxString)
}

// GetSteps returns the steps for the given note, starting from the version.
func GetSteps(inst *instance.Instance, fileID string, version int64) ([]Step, error) {
	lock := inst.NotesLock()
	if err := lock.Lock(); err != nil {
		return nil, err
	}
	defer lock.Unlock()

	return getSteps(inst, fileID, version)
}

// getSteps is the same as GetSteps, but with the notes lock already acquired
func getSteps(db prefixer.Prefixer, fileID string, version int64) ([]Step, error) {
	var steps []Step
	req := couchdb.AllDocsRequest{
		Limit:    1000,
		StartKey: stepID(fileID, version),
		EndKey:   endkey(fileID),
	}
	if err := couchdb.GetAllDocs(db, consts.NotesSteps, &req, &steps); err != nil {
		return nil, err
	}

	// The first step plays the role of a sentinel: if it isn't here, the
	// version is too old. Same if we have too many steps.
	if len(steps) == 0 || len(steps) == req.Limit {
		return nil, ErrTooOld
	}

	if version > 0 && version == steps[0].version() {
		steps = steps[1:] // Discard the sentinel
	}
	return steps, nil
}

// ApplySteps takes a note and some steps, and tries to apply them. It is an
// all or nothing change: if there is one error, the note won't be changed.
func ApplySteps(inst *instance.Instance, file *vfs.FileDoc, lastVersion string, steps []Step) (*vfs.FileDoc, error) {
	lock := inst.NotesLock()
	if err := lock.Lock(); err != nil {
		return nil, err
	}
	defer lock.Unlock()

	if len(steps) == 0 {
		return nil, ErrNoSteps
	}

	doc, err := get(inst, file)
	if err != nil {
		return nil, err
	}
	if lastVersion != fmt.Sprintf("%d", doc.Version) {
		return nil, ErrCannotApply
	}

	if err := apply(inst, doc, steps); err != nil {
		return nil, err
	}
	if err := saveSteps(inst, steps); err != nil {
		return nil, err
	}
	publishSteps(inst, file.ID(), steps)

	if err := saveToCache(inst, doc); err != nil {
		return nil, err
	}
	return doc.asFile(inst, file), nil
}

func apply(inst *instance.Instance, doc *Document, steps []Step) error {
	schema, err := doc.Schema()
	if err != nil {
		inst.Logger().WithNamespace("notes").
			Infof("Cannot instantiate the schema: %s", err)
		return ErrInvalidSchema
	}

	content, err := doc.Content()
	if err != nil {
		inst.Logger().WithNamespace("notes").
			Infof("Cannot instantiate the document: %s", err)
		return ErrInvalidFile
	}

	now := time.Now().Unix()
	for i, s := range steps {
		step, err := transform.StepFromJSON(schema, s)
		if err != nil {
			inst.Logger().WithNamespace("notes").
				Infof("Cannot instantiate a step: %s", err)
			return ErrInvalidSteps
		}
		result := applyStep(step, content)
		if result.Failed != "" {
			inst.Logger().WithNamespace("notes").
				Infof("Cannot apply a step: %s (version=%d)", result.Failed, doc.Version)
			return ErrCannotApply
		}
		content = result.Doc
		doc.Version++
		steps[i].SetID(stepID(doc.ID(), doc.Version))
		steps[i]["version"] = doc.Version
		steps[i]["timestamp"] = now
	}
	doc.SetContent(content)
	return nil
}

// applyStep turns a panic of prosemirror-go, which does not fully validate
// the steps sent by clients, into a failed step.
func applyStep(step transform.Step, content *model.Node) (result transform.StepResult) {
	defer func() {
		if r := recover(); r != nil {
			result = transform.StepResult{Failed: fmt.Sprintf("panic: %v", r)}
		}
	}()
	return step.Apply(content)
}

func saveSteps(inst *instance.Instance, steps []Step) error {
	olds := make([]interface{}, len(steps))
	news := make([]interface{}, len(steps))
	for i, s := range steps {
		news[i] = s
	}
	return couchdb.BulkUpdateDocs(inst, consts.NotesSteps, news, olds)
}

func purgeOldSteps(inst *instance.Instance, fileID string) {
	var steps []Step
	req := couchdb.AllDocsRequest{
		Limit:    1000,
		StartKey: stepID(fileID, 0),
		EndKey:   endkey(fileID),
	}
	if err := couchdb.GetAllDocs(inst, consts.NotesSteps, &req, &steps); err != nil {
		if !couchdb.IsNoDatabaseError(err) {
			inst.Logger().WithNamespace("notes").
				Warnf("Cannot purge old steps for file %s: %s", fileID, err)
		}
		return
	}
	if len(steps) == 0 {
		return
	}

	limit := time.Now().Add(-cleanStepsAfter).Unix()
	docs := make([]couchdb.Doc, 0, len(steps))
	for i := range steps {
		if steps[i].timestamp() > limit {
			break
		}
		docs = append(docs, &steps[i])
	}
	if len(docs) == 0 {
		return
	}
	if err := couchdb.BulkDeleteDocs(inst, consts.NotesSteps, docs); err != nil {
		inst.Logger().WithNamespace("notes").
			Warnf("Cannot purge old steps for file %s: %s", fileID, err)
	}
}

func purgeAllSteps(inst *instance.Instance, fileID string) {
	var docs []couchdb.Doc
	prefix := startkey(fileID)
	err := couchdb.ForeachDocsWithCustomPagination(inst, consts.NotesSteps, 1000, func(_ string, raw json.RawMessage) error {
		var doc Step
		if err := json.Unmarshal(raw, &doc); err != nil {
			return err
		}
		if !strings.HasPrefix(doc.ID(), prefix) {
			return nil
		}
		docs = append(docs, doc)
		return nil
	})
	if err != nil {
		if !couchdb.IsNoDatabaseError(err) {
			inst.Logger().WithNamespace("notes").
				Warnf("Cannot purge all steps for file %s: %s", fileID, err)
		}
		return
	}
	if len(docs) == 0 {
		return
	}

	if err := couchdb.BulkDeleteDocs(inst, consts.NotesSteps, docs); err != nil {
		inst.Logger().WithNamespace("notes").
			Warnf("Cannot purge all steps for file %s: %s", fileID, err)
	}
}

var _ jsonapi.Object = &Step{}
