package files

// Links is used to generate a JSON-API link for the directory (part of
import (
	"encoding/json"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/note"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

const (
	defPerPage = 30
)

type apiArchive struct {
	*vfs.Archive
}

type apiMetadata struct {
	*vfs.Metadata
	secret string
}

type dir struct {
	doc      *vfs.DirDoc
	rel      jsonapi.RelationshipMap
	included []jsonapi.Object
	driveId  string
}

type dirJSON struct {
	*vfs.DirDoc
	DriveId string `json:"driveId,omitempty"`
}

type file struct {
	doc        *vfs.FileDoc
	instance   *instance.Instance
	versions   []*vfs.Version
	noteImages []jsonapi.Object
	// fileJSON is used for marshaling to JSON and we keep a reference here to
	// avoid many allocations.
	jsonDoc     *fileJSON
	thumbSecret string
	includePath bool
	driveId     string
}

type fileJSON struct {
	*vfs.FileDoc
	// XXX Hide the internal_vfs_id and referenced_by
	InternalID   *interface{} `json:"internal_vfs_id,omitempty"`
	ReferencedBy *interface{} `json:"referenced_by,omitempty"`
	// Include the path if asked for
	Fullpath string `json:"path,omitempty"`
	// Shared drive identifier if exist
	DriveId string `json:"driveId,omitempty"`
}

func NewDir(doc *vfs.DirDoc, sharedDrive *sharing.Sharing) *dir {
	rel := jsonapi.RelationshipMap{
		"referenced_by": jsonapi.Relationship{
			Links: &jsonapi.LinksList{
				Self: "/files/" + doc.ID() + "/relationships/references",
			},
			Data: doc.ReferencedBy,
		},
	}

	var driveId string
	if sharedDrive != nil {
		driveId = sharedDrive.ID()
	}

	return &dir{doc: doc, rel: rel, driveId: driveId}
}

func getDirData(c echo.Context, doc *vfs.DirDoc) (int, couchdb.Cursor, []vfs.DirOrFileDoc, error) {
	instance := middlewares.GetInstance(c)
	fs := instance.VFS()

	cursor, err := jsonapi.ExtractPaginationCursor(c, defPerPage, 0)
	if err != nil {
		return 0, nil, nil, err
	}

	count, err := fs.DirLength(doc)
	if err != nil {
		return 0, nil, nil, err
	}

	// Hide the trash folder when listing the root directory.
	var limit int
	if doc.ID() == consts.RootDirID {
		if count > 0 {
			count--
		}
		switch c := cursor.(type) {
		case *couchdb.StartKeyCursor:
			limit = c.Limit
			if c.NextKey == nil {
				c.Limit++
			}
		case *couchdb.SkipCursor:
			limit = c.Limit
			if c.Skip == 0 {
				c.Limit++
			} else {
				c.Skip++
			}
		}
	}

	children, err := fs.DirBatch(doc, cursor)
	if err != nil {
		return 0, nil, nil, err
	}

	if doc.ID() == consts.RootDirID {
		switch c := cursor.(type) {
		case *couchdb.StartKeyCursor:
			c.Limit = limit
		case *couchdb.SkipCursor:
			c.Limit = limit
			c.Skip--
		}
	}

	return count, cursor, children, nil
}

func DirData(c echo.Context, statusCode int, doc *vfs.DirDoc, sharedDrive *sharing.Sharing) error {
	instance := middlewares.GetInstance(c)
	count, cursor, children, err := getDirData(c, doc)
	if err != nil {
		return err
	}

	// Create secrets for thumbnail links in batch for performance reasons
	var thumbIDs []string
	for _, child := range children {
		_, f := child.Refine()
		if f != nil {
			if f.Class == "image" || f.Class == "pdf" {
				thumbIDs = append(thumbIDs, f.ID())
			}
		}
	}
	var secrets map[string]string
	if len(thumbIDs) > 0 {
		secrets, _ = vfs.GetStore().AddThumbs(instance, thumbIDs)
	}
	if secrets == nil {
		secrets = make(map[string]string)
	}

	relsData := make([]couchdb.DocReference, 0)
	included := make([]jsonapi.Object, 0)
	for _, child := range children {
		if child.ID() == consts.TrashDirID {
			continue
		}
		relsData = append(relsData, couchdb.DocReference{ID: child.ID(), Type: child.DocType()})
		d, f := child.Refine()
		if d != nil {
			included = append(included, NewDir(d, sharedDrive))
		} else {
			file := NewFile(f, instance, sharedDrive)
			if secret, ok := secrets[f.ID()]; ok {
				file.SetThumbSecret(secret)
			}
			included = append(included, file)
		}
	}

	var parent jsonapi.Relationship
	if doc.ID() != consts.RootDirID {
		parent = jsonapi.Relationship{
			Links: &jsonapi.LinksList{
				Self: "/files/" + doc.DirID,
			},
			Data: couchdb.DocReference{
				ID:   doc.DirID,
				Type: consts.Files,
			},
		}
	}
	rel := jsonapi.RelationshipMap{
		"parent": parent,
		"contents": jsonapi.Relationship{
			Meta: &jsonapi.Meta{Count: &count},
			Links: &jsonapi.LinksList{
				Self: "/files/" + doc.DocID + "/relationships/contents",
			},
			Data: relsData},
		"not_synchronized_on": jsonapi.Relationship{
			Links: &jsonapi.LinksList{
				Self: "/files/" + doc.ID() + "/relationships/not_synchronized_on",
			},
			Data: doc.NotSynchronizedOn,
		},
		"referenced_by": jsonapi.Relationship{
			Links: &jsonapi.LinksList{
				Self: "/files/" + doc.ID() + "/relationships/references",
			},
			Data: doc.ReferencedBy,
		},
	}

	var links jsonapi.LinksList
	if cursor.HasMore() {
		params, err := jsonapi.PaginationCursorToParams(cursor)
		if err != nil {
			return err
		}
		next := "/files/" + doc.DocID + "/relationships/contents?" + params.Encode()
		rel["contents"].Links.Next = next
		links.Next = "/files/" + doc.DocID + "?" + params.Encode()
	}

	d := NewDir(doc, sharedDrive)
	d.rel = rel
	d.included = included

	return jsonapi.Data(c, statusCode, d, &links)
}

func dirDataList(c echo.Context, statusCode int, doc *vfs.DirDoc) error {
	instance := middlewares.GetInstance(c)
	count, cursor, children, err := getDirData(c, doc)
	if err != nil {
		return err
	}

	included := make([]jsonapi.Object, 0)
	for _, child := range children {
		if child.ID() == consts.TrashDirID {
			continue
		}
		d, f := child.Refine()
		if d != nil {
			included = append(included, NewDir(d, nil))
		} else {
			included = append(included, NewFile(f, instance, nil))
		}
	}

	var links jsonapi.LinksList
	if cursor.HasMore() {
		params, err := jsonapi.PaginationCursorToParams(cursor)
		if err != nil {
			return err
		}
		next := c.Request().URL.EscapedPath() + "?" + params.Encode()
		links.Next = next
	}

	meta := jsonapi.Meta{Count: &count}
	return jsonapi.DataListWithMeta(c, statusCode, meta, included, &links)
}

// NewFile creates an instance of file struct from a vfs.FileDoc document.
func NewFile(doc *vfs.FileDoc, i *instance.Instance, sharedDrive *sharing.Sharing) *file {
	var driveId string
	if sharedDrive != nil {
		driveId = sharedDrive.ID()
	}
	return &file{doc, i, nil, nil, &fileJSON{}, "", false, driveId}
}

// FileData returns a jsonapi representation of the given file.
func FileData(c echo.Context, statusCode int, doc *vfs.FileDoc, withVersions bool, links *jsonapi.LinksList, sharedDrive *sharing.Sharing) error {
	instance := middlewares.GetInstance(c)
	f := NewFile(doc, instance, sharedDrive)
	if withVersions {
		if versions, err := vfs.VersionsFor(instance, doc.ID()); err == nil {
			f.versions = versions
		}
	}
	if doc.Mime == consts.NoteMimeType {
		images, err := note.GetImages(instance, doc.ID())
		if err == nil {
			for _, image := range images {
				noteImage := NewNoteImage(instance, image)
				f.noteImages = append(f.noteImages, noteImage)
			}
		}
	}

	return jsonapi.Data(c, statusCode, f, links)
}

var (
	_ jsonapi.Object = (*apiArchive)(nil)
	_ jsonapi.Object = (*apiMetadata)(nil)
	_ jsonapi.Object = (*dir)(nil)
	_ jsonapi.Object = (*file)(nil)
)

func (a *apiArchive) Relationships() jsonapi.RelationshipMap { return nil }
func (a *apiArchive) Included() []jsonapi.Object             { return nil }
func (a *apiArchive) MarshalJSON() ([]byte, error)           { return json.Marshal(a.Archive) }
func (a *apiArchive) Links() *jsonapi.LinksList {
	return &jsonapi.LinksList{Self: "/files/archive/" + a.Secret}
}

func (m *apiMetadata) ID() string                             { return m.secret }
func (m *apiMetadata) Rev() string                            { return "" }
func (m *apiMetadata) SetID(id string)                        { m.secret = id }
func (m *apiMetadata) SetRev(rev string)                      {}
func (m *apiMetadata) DocType() string                        { return consts.FilesMetadata }
func (m *apiMetadata) Clone() couchdb.Doc                     { cloned := *m; return &cloned }
func (m *apiMetadata) Relationships() jsonapi.RelationshipMap { return nil }
func (m *apiMetadata) Included() []jsonapi.Object             { return nil }
func (m *apiMetadata) MarshalJSON() ([]byte, error)           { return json.Marshal(m.Metadata) }
func (m *apiMetadata) Links() *jsonapi.LinksList              { return nil }

func (d *dir) ID() string                             { return d.doc.ID() }
func (d *dir) Rev() string                            { return d.doc.Rev() }
func (d *dir) SetID(id string)                        { d.doc.SetID(id) }
func (d *dir) SetRev(rev string)                      { d.doc.SetRev(rev) }
func (d *dir) DocType() string                        { return d.doc.DocType() }
func (d *dir) Clone() couchdb.Doc                     { cloned := *d; return &cloned }
func (d *dir) Relationships() jsonapi.RelationshipMap { return d.rel }
func (d *dir) Included() []jsonapi.Object             { return d.included }
func (d *dir) MarshalJSON() ([]byte, error) {
	return json.Marshal(dirJSON{
		d.doc, d.driveId,
	})
}
func (d *dir) Links() *jsonapi.LinksList {
	return &jsonapi.LinksList{Self: "/files/" + d.doc.DocID}
}

func (f *file) ID() string                   { return f.doc.ID() }
func (f *file) Rev() string                  { return f.doc.Rev() }
func (f *file) SetID(id string)              { f.doc.SetID(id) }
func (f *file) SetRev(rev string)            { f.doc.SetRev(rev) }
func (f *file) DocType() string              { return f.doc.DocType() }
func (f *file) Clone() couchdb.Doc           { cloned := *f; return &cloned }
func (f *file) SetThumbSecret(secret string) { f.thumbSecret = secret }

func (f *file) Relationships() jsonapi.RelationshipMap {
	rels := jsonapi.RelationshipMap{
		"parent": jsonapi.Relationship{
			Links: &jsonapi.LinksList{
				Related: "/files/" + f.doc.DirID,
			},
			Data: couchdb.DocReference{
				ID:   f.doc.DirID,
				Type: consts.Files,
			},
		},
		"referenced_by": jsonapi.Relationship{
			Links: &jsonapi.LinksList{
				Self: "/files/" + f.doc.ID() + "/relationships/references",
			},
			Data: f.doc.ReferencedBy,
		},
	}
	if len(f.versions) > 0 {
		data := make([]couchdb.DocReference, len(f.versions))
		for i, version := range f.versions {
			data[i] = couchdb.DocReference{
				ID:   version.DocID,
				Type: consts.FilesVersions,
			}
		}
		rels["old_versions"] = jsonapi.Relationship{
			Data: data,
		}
	}
	return rels
}

func (f *file) Included() []jsonapi.Object {
	var included []jsonapi.Object
	for _, version := range f.versions {
		included = append(included, version)
	}
	included = append(included, f.noteImages...)
	return included
}

func (f *file) MarshalJSON() ([]byte, error) {
	f.jsonDoc.FileDoc = f.doc
	if f.includePath {
		f.jsonDoc.Fullpath, _ = f.doc.Path(nil)
	}
	f.jsonDoc.DriveId = f.driveId
	res, err := json.Marshal(f.jsonDoc)
	return res, err
}

func (f *file) Links() *jsonapi.LinksList {
	links := jsonapi.LinksList{Self: "/files/" + f.doc.DocID}
	if f.doc.Class == "image" || f.doc.Class == "pdf" {
		if f.thumbSecret == "" {
			if secret, err := vfs.GetStore().AddThumb(f.instance, f.doc.DocID); err == nil {
				f.thumbSecret = secret
			}
		}
		if f.thumbSecret != "" {
			links.Tiny = "/files/" + f.doc.DocID + "/thumbnails/" + f.thumbSecret + "/tiny"
			links.Small = "/files/" + f.doc.DocID + "/thumbnails/" + f.thumbSecret + "/small"
			links.Medium = "/files/" + f.doc.DocID + "/thumbnails/" + f.thumbSecret + "/medium"
			links.Large = "/files/" + f.doc.DocID + "/thumbnails/" + f.thumbSecret + "/large"
		}
	}
	return &links
}

func (f *file) IncludePath(fp vfs.FilePather) {
	_, err := f.doc.Path(fp)
	f.includePath = err == nil
}

// findDir is used for the result of mango requests, where only some fields can
// have been requested
type findDir struct {
	*vfs.DirDoc
	// We may want to hide some fields from the JSON response if the fields has
	// not been requested to CouchDB, as they are blank
	CreatedAt *time.Time `json:"created_at,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

func (d *findDir) Relationships() jsonapi.RelationshipMap {
	return jsonapi.RelationshipMap{
		"referenced_by": jsonapi.Relationship{
			Links: &jsonapi.LinksList{
				Self: "/files/" + d.ID() + "/relationships/references",
			},
			Data: d.DirDoc.ReferencedBy,
		},
	}
}
func (d *findDir) Included() []jsonapi.Object { return nil }
func (d *findDir) Links() *jsonapi.LinksList  { return nil }

func newFindDir(doc *vfs.DirDoc, fields []string) *findDir {
	dir := &findDir{doc, nil, nil}
	if hasField(fields, "created_at") {
		dir.CreatedAt = &doc.CreatedAt
	}
	if hasField(fields, "updated_at") {
		dir.UpdatedAt = &doc.UpdatedAt
	}
	return dir
}

// findFile is used for the result of mango requests, where only some fields can
// have been requested
type findFile struct {
	*vfs.FileDoc
	file *file
	// We may want to hide some fields from the JSON response if the fields has
	// not been requested to CouchDB, as they are blank
	Fullpath   string     `json:"path,omitempty"`
	CreatedAt  *time.Time `json:"created_at,omitempty"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
	Executable *bool      `json:"executable,omitempty"`
	Encrypted  *bool      `json:"encrypted,omitempty"`
	// Hide the internal_vfs_id and referenced_by
	InternalID   *interface{} `json:"internal_vfs_id,omitempty"`
	ReferencedBy *interface{} `json:"referenced_by,omitempty"`
}

func (f *findFile) SetThumbSecret(secret string)           { f.file.SetThumbSecret(secret) }
func (f *findFile) Relationships() jsonapi.RelationshipMap { return f.file.Relationships() }
func (f *findFile) Included() []jsonapi.Object             { return f.file.Included() }
func (f *findFile) Links() *jsonapi.LinksList              { return f.file.Links() }

func newFindFile(doc *vfs.FileDoc, fields []string, i *instance.Instance) *findFile {
	f := NewFile(doc, i, nil)
	ff := &findFile{doc, f, "", nil, nil, nil, nil, nil, nil}
	if hasField(fields, "created_at") {
		ff.CreatedAt = &doc.CreatedAt
	}
	if hasField(fields, "updated_at") {
		ff.UpdatedAt = &doc.UpdatedAt
	}
	if hasField(fields, "executable") {
		ff.Executable = &doc.Executable
	}
	if hasField(fields, "encrypted") {
		ff.Encrypted = &doc.Encrypted
	}
	return ff
}

func hasField(fields []string, field string) bool {
	for _, f := range fields {
		if f == field {
			return true
		}
	}
	return false
}

func (f *findFile) IncludePath(fp vfs.FilePather) {
	f.Fullpath, _ = f.Path(fp)
}
