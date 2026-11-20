package files

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

const (
	recentsLimitParam  = "page[limit]"
	recentsCursorParam = "page[cursor]"
)

// recentsResource is the JSON:API resource of a recents row. Its attributes
// are all the stored fields of the row, without the version of its format,
// and it points to the item of the row with its `file` relationship.
type recentsResource struct {
	*recents.FileDirActivity

	// SchemaVersion shadows the field of the row, as a shallower field wins
	// over a deeper one of the same JSON name, and is never set.
	SchemaVersion *struct{} `json:"schema_version,omitempty"`

	// file is the item as the files routes give it. It is nil for an item
	// that the user cannot read: the relationship is then empty.
	file jsonapi.Object
	// gone is set for an item that is not found on its instance: its
	// resource is left out of the list.
	gone bool
}

func (r *recentsResource) Links() *jsonapi.LinksList { return nil }

func (r *recentsResource) Relationships() jsonapi.RelationshipMap {
	var data interface{}
	if r.file != nil {
		data = couchdb.DocReference{ID: r.file.ID(), Type: consts.Files}
	}
	return jsonapi.RelationshipMap{"file": jsonapi.Relationship{Data: data}}
}

func (r *recentsResource) Included() []jsonapi.Object {
	if r.file == nil {
		return nil
	}
	return []jsonapi.Object{r.file}
}

// ListRecentsHandler handles GET /files/recents: a page of the personal recents
// of the instance of the request, each with the current document of its item.
func ListRecentsHandler(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	if !recents.Enabled(inst) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err := middlewares.AllowWholeType(c, permission.GET, consts.Files); err != nil {
		return err
	}
	pdoc, err := middlewares.GetPermission(c)
	if err != nil {
		return err
	}
	switch pdoc.Type {
	case permission.TypeShareByLink, permission.TypeSharePreview, permission.TypeShareInteract:
		// A sharing member has no personal recents on the instance they act
		// on: they must never get the list of its owner.
		return middlewares.ErrForbidden
	}

	size, err := parseRecentsLimit(c)
	if err != nil {
		return err
	}
	after, err := parseRecentsCursor(c)
	if err != nil {
		return err
	}

	// The first page seeds the recents once, so that the first switch to the
	// server-side recents never shows an empty page. A deeper page is reached
	// through a cursor and never seeds. The seed is best effort: a failure
	// leaves the page to the stored rows without failing the read.
	if !c.QueryParams().Has(recentsCursorParam) {
		recents.Seed(inst)
	}

	// One more row than the page tells whether rows follow it.
	rows, err := recents.Candidates(inst, size+1, after)
	if err != nil {
		return err
	}
	var links *jsonapi.LinksList
	if len(rows) > size {
		// The cursor is the position of the last row read, whether it gives a
		// resource or not: the rows left out are not read again, and no more
		// rows are read to fill the page.
		rows = rows[:size]
		cursor, err := recents.PositionOf(&rows[size-1]).Cursor()
		if err != nil {
			return err
		}
		next := url.Values{
			recentsLimitParam:  {strconv.Itoa(size)},
			recentsCursorParam: {cursor},
		}
		links = &jsonapi.LinksList{Next: "/files/recents?" + next.Encode()}
	}

	resources, err := resolveRecents(inst, rows)
	if err != nil {
		return err
	}
	return jsonapi.DataListWithIncluded(c, http.StatusOK, resources, links)
}

// resolveRecents returns the resources of the rows of a page, in their order,
// without those of the items that are gone. The rows are grouped by the
// instance that holds their items, to read the items of each instance at once.
func resolveRecents(inst *instance.Instance, rows []recents.FileDirActivity) ([]jsonapi.Object, error) {
	all := make([]*recentsResource, len(rows))
	byOwner := make(map[string][]*recentsResource)
	for i := range rows {
		all[i] = &recentsResource{FileDirActivity: &rows[i]}
		owner := rows[i].OwnerDomain
		byOwner[owner] = append(byOwner[owner], all[i])
	}
	var member *sharing.Member
	for owner, group := range byOwner {
		var err error
		if owner == "" {
			err = resolveOwnRecents(inst, group)
		} else {
			if member == nil {
				member = readerMember(inst)
			}
			err = resolveDriveRecents(owner, member, group)
		}
		if err != nil {
			return nil, err
		}
	}
	resources := make([]jsonapi.Object, 0, len(all))
	for _, r := range all {
		if !r.gone {
			resources = append(resources, r)
		}
	}
	return resources, nil
}

// resolveOwnRecents gives the resources of the items of the instance their
// documents, read in one request, as GetAllDocs does.
func resolveOwnRecents(inst *instance.Instance, group []*recentsResource) error {
	keys := make([]string, len(group))
	for i, r := range group {
		keys[i] = r.ID()
	}
	var results []vfs.DirOrFileDoc
	req := &couchdb.AllDocsRequest{Keys: keys}
	if err := couchdb.GetAllDocs(inst, consts.Files, req, &results); err != nil {
		return err
	}
	docs := make(map[string]*vfs.DirOrFileDoc, len(results))
	for i := range results {
		if results[i].DirDoc != nil {
			docs[results[i].ID()] = &results[i]
		}
	}

	fp := vfs.NewFilePatherWithCache(inst.VFS())
	var thumbs []*file
	for _, r := range group {
		doc, ok := docs[r.ID()]
		if !ok {
			r.gone = true
			continue
		}
		if d, f := doc.Refine(); d != nil {
			r.file = NewDir(d, nil)
		} else {
			file := NewFile(f, inst, nil)
			file.IncludePath(fp)
			if f.Class == "image" || f.Class == "pdf" {
				thumbs = append(thumbs, file)
			}
			r.file = file
		}
	}
	setThumbSecrets(inst, thumbs)
	return nil
}

// readerMember returns the reference of the user of the instance, to match
// the members of the drives of another instance (see
// sharing.Sharing.MemberMatching), as when their activity is recorded.
func readerMember(inst *instance.Instance) *sharing.Member {
	// Without an email, the member is matched by its instance only.
	email, _ := inst.SettingsEMail()
	u := url.URL{Scheme: inst.Scheme(), Host: inst.Domain}
	return &sharing.Member{Email: email, Instance: u.String()}
}

// fixedPath gives a file a path that is already known, or none.
type fixedPath string

func (p fixedPath) FilePath(*vfs.FileDoc) (string, error) {
	if p == "" {
		return "", os.ErrNotExist
	}
	return string(p), nil
}

// resolveDriveRecents gives the resources of the items of another instance
// their documents, when a drive of the member gives them. The documents are
// those of the shared drive routes, with the drive of the item and without
// any path of the owner above the drive: the path is relative to the root of
// the drive, or under the trash. The number of requests does not depend on
// the number of resources, except for the trashed items.
func resolveDriveRecents(ownerDomain string, member *sharing.Member, group []*recentsResource) error {
	owner := sharing.LocalInstanceFromHost(ownerDomain)
	if owner == nil {
		return nil
	}
	ids := make([]string, len(group))
	for i, r := range group {
		ids[i] = r.ID()
	}
	docs, err := vfs.DirOrFileDocsByIDs(owner, ids)
	if err != nil {
		return err
	}
	var found []*recentsResource
	var targets []sharing.AccessTarget
	for _, r := range group {
		doc, ok := docs[r.ID()]
		if !ok {
			// Destroyed, or moved to another owner under a new ID.
			r.gone = true
			continue
		}
		d, f := doc.Refine()
		found = append(found, r)
		targets = append(targets, sharing.AccessTarget{Dir: d, File: f})
	}
	if len(targets) == 0 {
		return nil
	}
	resolver := sharing.NewAccessResolver(owner)
	scopes, err := resolver.DrivesForMember(targets, member)
	if err != nil {
		return err
	}

	var thumbs []*file
	for i, t := range targets {
		// The resolver has computed the path of the files it could resolve.
		var fullpath string
		if t.Dir != nil {
			fullpath = t.Dir.Fullpath
		} else {
			fullpath, _ = t.File.Path(fixedPath(""))
		}
		scope := scopes[i]
		trashed := fullpath == vfs.TrashDirName || strings.HasPrefix(fullpath, vfs.TrashDirName+"/")
		if trashed {
			scope, err = trashedItemDrive(resolver, owner.VFS(), t, member)
			if err != nil {
				return err
			}
		}
		if scope == nil {
			continue
		}
		// A trashed item keeps its path under the trash, which names only
		// what was in the drive and tells that a folder is trashed. The place
		// where it was may be above the root of the drive: it is not given.
		shown := fullpath
		if !trashed {
			if shown, err = scope.PathFromRoot(fullpath); err != nil {
				continue
			}
		}
		if t.Dir != nil {
			t.Dir.Fullpath = shown
			t.Dir.RestorePath = ""
			d := NewDir(t.Dir, scope.Sharing)
			// The path is already the one from the root of the scope
			d.paths.Drive = nil
			found[i].file = d
			continue
		}
		t.File.RestorePath = ""
		t.File.ResetFullpath()
		f := NewFile(t.File, owner, scope.Sharing)
		f.paths.Drive = nil
		f.IncludePath(fixedPath(shown))
		if t.File.Class == "image" || t.File.Class == "pdf" {
			thumbs = append(thumbs, f)
		}
		found[i].file = f
	}
	setThumbSecrets(owner, thumbs)
	return nil
}

// trashedItemDrive returns the drive that gives a trashed item to a member:
// the one of the folder it would be restored into, as the shared drive routes
// do to restore it. This is resolved for one item at a time.
func trashedItemDrive(resolver *sharing.AccessResolver, fs vfs.VFS, t sharing.AccessTarget, member *sharing.Member) (*sharing.SharingScope, error) {
	folder, err := sharing.RestoreFolder(fs, t.Dir, t.File)
	if err == nil {
		var scope *sharing.SharingScope
		if scope, err = resolver.DriveForMember(folder.ID(), member); err == nil {
			return scope, nil
		}
	}
	if errors.Is(err, sharing.ErrNoRestoreFolder) || errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return nil, err
}

// setThumbSecrets creates the secrets of the thumbnail links of files in
// batch, on the instance that serves them.
func setThumbSecrets(inst *instance.Instance, files []*file) {
	if len(files) == 0 {
		return
	}
	ids := make([]string, len(files))
	for i, f := range files {
		ids[i] = f.ID()
	}
	secrets, _ := vfs.GetStore().AddThumbs(inst, ids)
	for _, f := range files {
		if secret, ok := secrets[f.ID()]; ok {
			f.SetThumbSecret(secret)
		}
	}
}

// parseRecentsLimit returns the size of the page: an invalid value is rejected,
// and a value above the maximum is brought down to it, as for the other
// paginated routes of the stack.
func parseRecentsLimit(c echo.Context) (int, error) {
	if !c.QueryParams().Has(recentsLimitParam) {
		return recents.DefaultPageSize, nil
	}
	size, err := strconv.Atoi(c.QueryParam(recentsLimitParam))
	if err != nil || size < 1 {
		return 0, jsonapi.InvalidParameter(recentsLimitParam, errors.New("invalid page limit"))
	}
	return min(size, recents.MaxPageSize), nil
}

// parseRecentsCursor returns the position after which the page starts.
func parseRecentsCursor(c echo.Context) (recents.Position, error) {
	if !c.QueryParams().Has(recentsCursorParam) {
		return recents.Position{}, nil
	}
	after, err := recents.ParseCursor(c.QueryParam(recentsCursorParam))
	if err != nil {
		return recents.Position{}, jsonapi.InvalidParameter(recentsCursorParam, errors.New("invalid cursor"))
	}
	return after, nil
}
