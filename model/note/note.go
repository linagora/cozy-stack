// Package note is the glue between the prosemirror models, the VFS, redis, the
// hub for realtime, etc.
package note

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/prosemirror-go/model"
	"github.com/hashicorp/go-multierror"
)

const (
	persistenceDebouce = "3m"
	cacheDuration      = 30 * time.Minute
	cleanStepsAfter    = 24 * time.Hour
)

// Document is the note document in memory. It is persisted to the VFS as a
// file, but with a debounce: the intermediate states are saved in Redis.
type Document struct {
	DocID      string                 `json:"_id"`
	DocRev     string                 `json:"_rev,omitempty"`
	CreatedBy  string                 `json:"-"`
	DirID      string                 `json:"dir_id,omitempty"` // Only used at creation
	Title      string                 `json:"title"`
	Version    int64                  `json:"version"`
	SchemaSpec map[string]interface{} `json:"schema"`
	RawContent map[string]interface{} `json:"content"`

	// Use cache for some computed properties
	schema  *model.Schema
	content *model.Node
}

// ID returns the document qualified identifier
func (d *Document) ID() string { return d.DocID }

// Rev returns the document revision
func (d *Document) Rev() string { return d.DocRev }

// DocType returns the document type
func (d *Document) DocType() string { return consts.NotesDocuments }

// Clone implements couchdb.Doc
func (d *Document) Clone() couchdb.Doc {
	cloned := *d
	// XXX The schema and the content are supposed to be immutable and, as
	// such, are not cloned.
	return &cloned
}

// SetID changes the document qualified identifier
func (d *Document) SetID(id string) { d.DocID = id }

// SetRev changes the document revision
func (d *Document) SetRev(rev string) { d.DocRev = rev }

// Metadata returns the file metadata for this note.
func (d *Document) Metadata() map[string]interface{} {
	return map[string]interface{}{
		"title":   d.Title,
		"content": d.RawContent,
		"version": d.Version,
		"schema":  d.SchemaSpec,
	}
}

// Schema returns the prosemirror schema for this note
func (d *Document) Schema() (*model.Schema, error) {
	if d.schema == nil {
		spec := model.SchemaSpecFromJSON(d.SchemaSpec)
		schema, err := model.NewSchema(&spec)
		if err != nil {
			return nil, ErrInvalidSchema
		}
		d.schema = schema
	}
	return d.schema, nil
}

// SetContent updates the content of this note, and clears the cache.
func (d *Document) SetContent(content *model.Node) {
	d.RawContent = content.ToJSON()
	d.content = content
}

// Content returns the prosemirror content for this note.
func (d *Document) Content() (*model.Node, error) {
	if d.content == nil {
		if len(d.RawContent) == 0 {
			return nil, ErrInvalidFile
		}
		schema, err := d.Schema()
		if err != nil {
			return nil, err
		}
		content, err := model.NodeFromJSON(schema, d.RawContent)
		if err != nil {
			return nil, err
		}
		d.content = content
	}
	return d.content, nil
}

// Markdown returns a markdown serialization of the content.
func (d *Document) Markdown(images []*Image) ([]byte, error) {
	content, err := d.Content()
	if err != nil {
		return nil, err
	}
	md := markdownSerializer(images).Serialize(content)
	return []byte(md), nil
}

func (d *Document) Text() (string, error) {
	content, err := d.Content()
	if err != nil {
		return "", err
	}
	text := textSerializer().Serialize(content)
	return text, nil
}

// GetDirID returns the ID of the directory where the note will be created.
func (d *Document) GetDirID(inst *instance.Instance) (string, error) {
	if d.DirID != "" {
		return d.DirID, nil
	}
	parent, err := ensureNotesDir(inst)
	if err != nil {
		return "", err
	}
	d.DirID = parent.ID()
	return d.DirID, nil
}

func (d *Document) asFile(inst *instance.Instance, old *vfs.FileDoc) *vfs.FileDoc {
	now := time.Now()
	file := old.Clone().(*vfs.FileDoc)
	vfs.MergeMetadata(file, d.Metadata())
	file.Mime = consts.NoteMimeType
	file.MD5Sum = nil // Let the VFS compute the md5sum

	// If the file was renamed manually before, we will keep its name. Else, we
	// can rename with the new title.
	newTitle := titleToFilename(inst, d.Title, old.CreatedAt)
	oldTitle, _ := old.Metadata["title"].(string)
	rename := d.Title != "" && titleToFilename(inst, oldTitle, old.CreatedAt) == old.DocName
	if old.DocName == "" {
		rename = true
	}
	if strings.Contains(old.DocName, " - conflict - ") && oldTitle != newTitle {
		rename = true
	}
	if rename {
		file.DocName = newTitle
		file.ResetFullpath()
		_, _ = file.Path(inst.VFS()) // Prefill the fullpath
	}

	file.UpdatedAt = now
	file.CozyMetadata.UpdatedAt = file.UpdatedAt
	return file
}

// Create the file in the VFS for this note.
func Create(inst *instance.Instance, doc *Document) (*vfs.FileDoc, error) {
	lock := inst.NotesLock()
	if err := lock.Lock(); err != nil {
		return nil, err
	}
	defer lock.Unlock()

	doc.Version = 0
	if len(doc.RawContent) > 0 {
		// Check that the content satisfies the schema
		_, err := doc.Content()
		if err != nil {
			return nil, err
		}
	} else {
		content, err := initialContent(inst, doc)
		if err != nil {
			return nil, err
		}
		doc.SetContent(content)
	}

	file, err := writeFile(inst, doc, nil)
	if err != nil {
		return nil, err
	}
	if err := SetupTrigger(inst, file.ID()); err != nil {
		return nil, err
	}
	return file, nil
}

func initialContent(inst *instance.Instance, doc *Document) (*model.Node, error) {
	schema, err := doc.Schema()
	if err != nil {
		inst.Logger().WithNamespace("notes").
			Infof("Cannot instantiate the schema: %s", err)
		return nil, ErrInvalidSchema
	}

	// Create an empty document that matches the schema constraints.
	typ, err := schema.NodeType(schema.Spec.TopNode)
	if err != nil {
		inst.Logger().WithNamespace("notes").
			Infof("The schema is invalid: %s", err)
		return nil, ErrInvalidSchema
	}
	node, err := typ.CreateAndFill()
	if err != nil {
		inst.Logger().WithNamespace("notes").
			Infof("The topNode cannot be created: %s", err)
		return nil, ErrInvalidSchema
	}
	return node, nil
}

func newFileDoc(inst *instance.Instance, doc *Document) (*vfs.FileDoc, error) {
	dirID, err := doc.GetDirID(inst)
	if err != nil {
		return nil, err
	}
	cm := vfs.NewCozyMetadata(inst.PageURL("/", nil))

	fileDoc, err := vfs.NewFileDoc(
		titleToFilename(inst, doc.Title, cm.UpdatedAt),
		dirID,
		0,
		nil, // Let the VFS compute the md5sum
		consts.NoteMimeType,
		"text",
		cm.UpdatedAt,
		false, // Not executable
		false, // Not trashed
		false, // Not encrypted
		nil,   // No tags
	)
	if err != nil {
		return nil, err
	}

	fileDoc.Metadata = doc.Metadata()
	fileDoc.CozyMetadata = cm
	fileDoc.CozyMetadata.CozyMetadata.CreatedByApp = doc.CreatedBy
	return fileDoc, nil
}

func titleToFilename(inst *instance.Instance, title string, updatedAt time.Time) string {
	name := strings.SplitN(title, "\n", 2)[0]
	if name == "" {
		name = inst.Translate("Notes New note")
		name += " " + updatedAt.Format(time.RFC3339)
	}
	// Create file with a name compatible with Windows/macOS to avoid
	// synchronization issues with the desktop client
	r := strings.NewReplacer("/", "-", ":", "-", "<", "-", ">", "-",
		`"`, "-", "'", "-", "?", "-", "*", "-", "|", "-", "\\", "-")
	name = r.Replace(name)
	// Avoid too long filenames for the same reason
	if len(name) > 240 {
		name = name[:240]
	}
	return name + ".cozy-note"
}

func ensureNotesDir(inst *instance.Instance) (*vfs.DirDoc, error) {
	ref := couchdb.DocReference{
		Type: consts.Apps,
		ID:   consts.Apps + "/" + consts.NotesSlug,
	}
	dirname := inst.Translate("Tree Notes")
	return vfs.EnsureReferencedDir(inst, inst.VFS(), ref, dirname, inst.PageURL("/", nil))
}

// DebounceMessage is used by the trigger for saving the note to the VFS with a
// debounce.
type DebounceMessage struct {
	NoteID string `json:"note_id"`
}

func SetupTrigger(inst *instance.Instance, fileID string) error {
	sched := job.System()
	infos := job.TriggerInfos{
		Type:       "@event",
		WorkerType: "notes-save",
		Arguments:  fmt.Sprintf("%s:UPDATED:%s", consts.NotesEvents, fileID),
		Debounce:   persistenceDebouce,
	}
	if sched.HasTrigger(inst, infos) {
		return nil
	}

	msg := &DebounceMessage{NoteID: fileID}
	t, err := job.NewTrigger(inst, infos, msg)
	if err != nil {
		return err
	}
	return sched.AddTrigger(t)
}

func writeFile(inst *instance.Instance, doc *Document, oldDoc *vfs.FileDoc) (fileDoc *vfs.FileDoc, err error) {
	images, _ := getImages(inst, doc.DocID)
	md, err := doc.Markdown(images)
	if err != nil {
		return nil, err
	}
	cleanImages(inst, images)

	if oldDoc == nil {
		fileDoc, err = newFileDoc(inst, doc)
		if err != nil {
			return nil, err
		}
	} else {
		fileDoc = doc.asFile(inst, oldDoc)
		// XXX if the name has changed, we have to rename the doc before
		// writing the content (2 revisions in CouchDB) to ensure that changes
		// are correctly propagated in Cozy to Cozy sharings.
		if fileDoc.DocName != oldDoc.DocName {
			oldDoc, err = forceRename(inst, oldDoc, fileDoc)
			if err != nil {
				return nil, err
			}
			fileDoc.DocName = oldDoc.DocName
			fileDoc.ResetFullpath()
		}
	}

	content := md
	if hasImages(images) {
		content, _ = buildArchive(inst, md, images)
	}
	fileDoc.ByteSize = int64(len(content))

	fs := inst.VFS()
	basename := fileDoc.DocName
	var file vfs.File
	for i := 2; i < 100; i++ {
		file, err = fs.CreateFile(fileDoc, oldDoc)
		if err == nil {
			break
		} else if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		filename := strings.TrimSuffix(path.Base(basename), path.Ext(basename))
		fileDoc.DocName = fmt.Sprintf("%s (%d).cozy-note", filename, i)
		fileDoc.ResetFullpath()
	}
	_, err = file.Write(content)
	if cerr := file.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err == nil {
		// XXX Write to the cache, not just clean it, to avoid the stack
		// fetching the rename but not the new content/metadata on next step
		// apply, as CouchDB is not strongly consistent.
		if doc, _ := fromMetadata(fileDoc); doc != nil {
			_ = saveToCache(inst, doc)
		}
	}
	return fileDoc, err
}

// forceRename will update the FileDoc in CouchDB with the new name (but the
// old content). It will return the updated doc, or an error if it fails.
func forceRename(inst *instance.Instance, old *vfs.FileDoc, file *vfs.FileDoc) (*vfs.FileDoc, error) {
	// We clone file, and not old, to keep the fullpath (1 less CouchDB request)
	tmp := file.Clone().(*vfs.FileDoc)
	tmp.ByteSize = old.ByteSize
	tmp.MD5Sum = make([]byte, len(old.MD5Sum))
	copy(tmp.MD5Sum, old.MD5Sum)
	tmp.Metadata = make(vfs.Metadata, len(old.Metadata))
	for k, v := range old.Metadata {
		tmp.Metadata[k] = v
	}

	basename := tmp.DocName
	for i := 2; i < 100; i++ {
		err := inst.VFS().UpdateFileDoc(old, tmp)
		if err == nil {
			break
		} else if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		filename := strings.TrimSuffix(path.Base(basename), path.Ext(basename))
		tmp.DocName = fmt.Sprintf("%s (%d).cozy-note", filename, i)
		tmp.ResetFullpath()
	}
	return tmp, nil
}

func buildArchive(inst *instance.Instance, md []byte, images []*Image) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	// Add markdown to the archive
	hdr := &tar.Header{
		Name: "index.md",
		Mode: 0640,
		Size: int64(len(md)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return nil, err
	}
	if _, err := tw.Write(md); err != nil {
		return nil, err
	}

	// Add images to the archive
	fs := inst.ThumbsFS()
	for _, image := range images {
		if !image.seen {
			continue
		}
		th, err := fs.OpenNoteThumb(image.ID(), consts.NoteImageOriginalFormat)
		if err != nil {
			return nil, err
		}
		img, err := io.ReadAll(th)
		if errc := th.Close(); err == nil && errc != nil {
			err = errc
		}
		if err != nil {
			return nil, err
		}
		hdr := &tar.Header{
			Name: image.Name,
			Mode: 0640,
			Size: int64(len(img)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(img); err != nil {
			return nil, err
		}
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// List returns a list of notes sorted by descending updated_at. It uses
// pagination via a mango bookmark.
func List(inst *instance.Instance, bookmark string) ([]*vfs.FileDoc, string, error) {
	lock := inst.NotesLock()
	if err := lock.Lock(); err != nil {
		return nil, "", err
	}
	defer lock.Unlock()

	var docs []*vfs.FileDoc
	req := &couchdb.FindRequest{
		UseIndex: "by-mime-updated-at",
		Selector: mango.And(
			mango.Equal("mime", consts.NoteMimeType),
			mango.Equal("trashed", false),
			mango.Exists("updated_at"),
		),
		Sort: mango.SortBy{
			{Field: "mime", Direction: mango.Desc},
			{Field: "trashed", Direction: mango.Desc},
			{Field: "updated_at", Direction: mango.Desc},
		},
		Limit:    100,
		Bookmark: bookmark,
	}
	res, err := couchdb.FindDocsRaw(inst, consts.Files, req, &docs)
	if err != nil {
		return nil, "", err
	}

	UpdateMetadataFromCache(inst, docs)
	return docs, res.Bookmark, nil
}

// UpdateMetadataFromCache update the metadata for a file/note with the
// information in cache.
func UpdateMetadataFromCache(inst *instance.Instance, docs []*vfs.FileDoc) {
	keys := make([]string, len(docs))
	for i, doc := range docs {
		keys[i] = cacheKey(inst, doc.ID())
	}
	cache := config.GetConfig().CacheStorage
	bufs := cache.MultiGet(keys)
	for i, buf := range bufs {
		if len(buf) == 0 {
			continue
		}
		var note Document
		if err := json.Unmarshal(buf, &note); err == nil {
			vfs.MergeMetadata(docs[i], note.Metadata())
		}
	}
}

// GetFile takes a file from the VFS as a note and returns its last version. It
// is useful when some changes have not yet been persisted to the VFS.
func GetFile(inst *instance.Instance, file *vfs.FileDoc) (*vfs.FileDoc, error) {
	lock := inst.NotesLock()
	if err := lock.Lock(); err != nil {
		return nil, err
	}
	defer lock.Unlock()
	doc, err := get(inst, file)
	if err != nil {
		return nil, err
	}
	return doc.asFile(inst, file), nil
}

// get must be called with the notes lock already acquired. It will try to load
// the last version if a note from the cache, and if it fails, it will replay
// the new steps on the file from the VFS.
func get(inst *instance.Instance, file *vfs.FileDoc) (*Document, error) {
	if doc := getFromCache(inst, file.ID()); doc != nil {
		return doc, nil
	}
	version, _ := versionFromMetadata(file)
	steps, err := getSteps(inst, file.ID(), version)
	if err != nil && !errors.Is(err, ErrTooOld) && !couchdb.IsNoDatabaseError(err) {
		return nil, err
	}
	doc, err := fromMetadata(file)
	if err != nil {
		return nil, err
	}
	if len(steps) == 0 {
		return doc, nil
	}
	if version, ok := steps[0]["version"].(float64); ok {
		doc.Version = int64(version) - 1
	}
	if err := apply(inst, doc, steps); err != nil {
		return nil, err
	}
	_ = saveToCache(inst, doc)
	return doc, nil
}

func versionFromMetadata(file *vfs.FileDoc) (int64, error) {
	switch v := file.Metadata["version"].(type) {
	case float64:
		return int64(v), nil
	case int64:
		return v, nil
	}
	return 0, ErrInvalidFile
}

func fromMetadata(file *vfs.FileDoc) (*Document, error) {
	version, err := versionFromMetadata(file)
	if err != nil {
		return nil, err
	}
	title, _ := file.Metadata["title"].(string)
	schema, ok := file.Metadata["schema"].(map[string]interface{})
	if !ok {
		return nil, ErrInvalidFile
	}
	content, ok := file.Metadata["content"].(map[string]interface{})
	if !ok {
		return nil, ErrInvalidFile
	}
	return &Document{
		DocID:      file.ID(),
		Title:      title,
		Version:    version,
		SchemaSpec: schema,
		RawContent: content,
	}, nil
}

func getFromCache(inst *instance.Instance, noteID string) *Document {
	cache := config.GetConfig().CacheStorage
	buf, ok := cache.Get(cacheKey(inst, noteID))
	if !ok {
		return nil
	}
	var doc Document
	if err := json.Unmarshal(buf, &doc); err != nil {
		return nil
	}
	return &doc
}

func cacheKey(inst *instance.Instance, noteID string) string {
	return fmt.Sprintf("note:%s:%s", inst.Domain, noteID)
}

func saveToCache(inst *instance.Instance, doc *Document) error {
	cache := config.GetConfig().CacheStorage
	buf, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	cache.Set(cacheKey(inst, doc.ID()), buf, cacheDuration)
	return nil
}

func getListFromCache(inst *instance.Instance) []string {
	cache := config.GetConfig().CacheStorage
	prefix := fmt.Sprintf("note:%s:", inst.Domain)
	keys := cache.Keys(prefix)
	fileIDs := make([]string, len(keys))
	for i, key := range keys {
		fileIDs[i] = strings.TrimPrefix(key, prefix)
	}
	return fileIDs
}

func GetText(inst *instance.Instance, file *vfs.FileDoc) (string, error) {
	lock := inst.NotesLock()
	if err := lock.Lock(); err != nil {
		return "", err
	}
	defer lock.Unlock()

	doc, err := get(inst, file)
	if err != nil {
		return "", err
	}

	return doc.Text()
}

// UpdateTitle changes the title of a note and renames the associated file.
func UpdateTitle(inst *instance.Instance, file *vfs.FileDoc, title, sessionID string) (*vfs.FileDoc, error) {
	lock := inst.NotesLock()
	if err := lock.Lock(); err != nil {
		return nil, err
	}
	defer lock.Unlock()

	doc, err := get(inst, file)
	if err != nil {
		return nil, err
	}

	if doc.Title == title {
		return file, nil
	}
	doc.Title = title
	if err := saveToCache(inst, doc); err != nil {
		return nil, err
	}

	publishUpdatedTitle(inst, file.ID(), title, sessionID)
	return doc.asFile(inst, file), nil
}

// Update is used to persist changes on a note to its file in the VFS.
func Update(inst *instance.Instance, fileID string) error {
	lock := inst.NotesLock()
	if err := lock.Lock(); err != nil {
		return err
	}
	defer lock.Unlock()

	old, err := inst.VFS().FileByID(fileID)
	if err != nil {
		return err
	}
	doc, err := get(inst, old)
	if err != nil {
		return err
	}

	oldVersion, _ := old.Metadata["version"].(float64)
	if doc.Title == old.Metadata["title"] &&
		doc.Version == int64(oldVersion) &&
		consts.NoteMimeType == old.Mime {
		// Nothing to do
		return nil
	}

	_, err = writeFile(inst, doc, old)
	if err != nil {
		return err
	}
	purgeOldSteps(inst, fileID)
	return nil
}

// UpdateSchema updates the schema of a note, and invalidates the previous steps.
func UpdateSchema(inst *instance.Instance, file *vfs.FileDoc, schema map[string]interface{}) (*vfs.FileDoc, error) {
	lock := inst.NotesLock()
	if err := lock.Lock(); err != nil {
		return nil, err
	}
	defer lock.Unlock()

	doc, err := get(inst, file)
	if err != nil {
		return nil, err
	}

	doc.SchemaSpec = schema
	updated, err := writeFile(inst, doc, file)
	if err != nil {
		return nil, err
	}

	// Purging all steps can take a few seconds, so it is better to do that in
	// a goroutine to avoid blocking the user that wants to read their note.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				var err error
				switch r := r.(type) {
				case error:
					err = r
				default:
					err = fmt.Errorf("%v", r)
				}
				stack := make([]byte, 4<<10) // 4 KB
				length := runtime.Stack(stack, false)
				log := inst.Logger().WithNamespace("note").WithField("panic", true)
				log.Errorf("PANIC RECOVER %s: %s", err.Error(), stack[:length])
			}
		}()
		purgeAllSteps(inst, doc.ID())
	}()

	return updated, nil
}

// FlushPendings is used to persist all the notes before an export.
func FlushPendings(inst *instance.Instance) error {
	var errm error
	list := getListFromCache(inst)
	for _, fileID := range list {
		if err := Update(inst, fileID); err != nil {
			errm = multierror.Append(errm, err)
		}
	}
	return errm
}

var _ couchdb.Doc = &Document{}
