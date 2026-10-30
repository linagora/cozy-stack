// Package notes is about the documents of cozy-notes. The notes are persisted
// as files, but they also have some specific routes for enabling collaborative
// edition.
package notes

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/note"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/web/files"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

// CreateNote is the API handler for POST /notes. It creates a note, aka a file
// with a set of metadata to enable collaborative edition.
func CreateNote(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	doc := &note.Document{}
	if _, err := jsonapi.Bind(c.Request().Body, doc); err != nil {
		return err
	}
	doc.CreatedBy = getCreatedBy(c)

	// We first look if we have a permission on the whole doctype, as it is
	// cheap. If not, we look on more finer permissions, which is a bit more
	// complicated and costly, but is needed for creating a note in a shared by
	// link folder for example.
	if err := middlewares.AllowWholeType(c, permission.POST, consts.Files); err != nil {
		dirID, errd := doc.GetDirID(inst)
		if errd != nil {
			return err
		}
		fileDoc, errf := vfs.NewFileDoc(
			"tmp.cozy-note", // We don't care, but it can't be empty
			dirID,
			0,   // We don't care
			nil, // Let the VFS compute the md5sum
			consts.NoteMimeType,
			"text",
			time.Now(),
			false, // Not executable
			false, // Not trashed
			false, // Not encrypted
			nil,   // No tags
		)
		if errf != nil {
			return err
		}
		if err := middlewares.AllowVFS(c, permission.POST, fileDoc); err != nil {
			return err
		}
	}

	file, err := note.Create(inst, doc)
	if err != nil {
		return wrapError(err)
	}
	files.RecordActivity(c, file.ID(), recents.KindEdit)

	return files.FileData(c, http.StatusCreated, file, false, nil, nil)
}

// ListNotes is the API handler for GET /notes. It returns the list of the
// notes.
func ListNotes(c echo.Context) error {
	if err := middlewares.AllowWholeType(c, permission.GET, consts.Files); err != nil {
		return err
	}

	inst := middlewares.GetInstance(c)
	bookmark := c.QueryParam("page[cursor]")
	docs, bookmark, err := note.List(inst, bookmark)
	if err != nil {
		return wrapError(err)
	}

	var links jsonapi.LinksList
	if bookmark != "" {
		links.Next = "/notes?page[cursor]=" + bookmark
	}

	fp := vfs.NewFilePatherWithCache(inst.VFS())
	objs := make([]jsonapi.Object, len(docs))
	for i, doc := range docs {
		f := files.NewFile(doc, inst, nil)
		f.IncludePath(fp)
		objs[i] = f
	}
	return jsonapi.DataList(c, http.StatusOK, objs, &links)
}

// GetNote is the API handler for GET /notes/:id. It fetches the file with the
// given id, and also includes the changes in the content that have been
// accepted by the stack but not yet persisted on the file.
func GetNote(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	fileID := c.Param("id")
	file, err := inst.VFS().FileByID(fileID)
	if err != nil {
		return wrapError(err)
	}

	if err := middlewares.AllowVFS(c, permission.GET, file); err != nil {
		return err
	}

	file, err = note.GetFile(inst, file)
	if err != nil {
		return wrapError(err)
	}
	recordNoteOpen(c, file.ID())

	return files.FileData(c, http.StatusOK, file, false, nil, nil)
}

// recordNoteOpen records a personal open of the note: the notes editor loads
// the whole note with GET /notes/:id when it opens it, it never downloads its
// content. The caller is the notes app of the user, or a sharing member with
// the sharecode the open route gave them, read-only (share-preview) or not.
func recordNoteOpen(c echo.Context, fileID string) {
	pdoc, err := middlewares.GetPermission(c)
	if err != nil {
		return
	}
	switch pdoc.Type {
	case permission.TypeWebapp:
		if pdoc.SourceID != consts.Apps+"/"+consts.NotesSlug {
			return
		}
	case permission.TypeShareInteract, permission.TypeSharePreview:
	default:
		return
	}
	files.RecordActivity(c, fileID, recents.KindView)
}

func GetNoteText(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	fileID := c.Param("id")
	file, err := inst.VFS().FileByID(fileID)
	if err != nil {
		return wrapError(err)
	}

	if err := middlewares.AllowVFS(c, permission.GET, file); err != nil {
		return err
	}

	content, err := note.GetText(inst, file)
	if err != nil {
		return wrapError(err)
	}
	return c.String(http.StatusOK, content)
}

func GetTexts(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	ids := strings.Split(c.QueryParam("ids"), ",")
	texts := make(map[string]string)

	for _, id := range ids {
		file, err := inst.VFS().FileByID(id)
		if err != nil {
			return wrapError(err)
		}
		if err := middlewares.AllowVFS(c, permission.GET, file); err != nil {
			return err
		}
		content, err := note.GetText(inst, file)
		if err != nil {
			return wrapError(err)
		}
		texts[id] = content
	}

	return c.JSON(http.StatusOK, texts)
}

// GetSteps is the API handler for GET /notes/:id/steps?Version=xxx. It returns
// the steps since the given version. If the version is too old, and the steps
// are no longer available, it returns a 412 response with the whole document
// for the note.
func GetSteps(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	fileID := c.Param("id")
	file, err := inst.VFS().FileByID(fileID)
	if err != nil {
		return wrapError(err)
	}

	if err := middlewares.AllowVFS(c, permission.GET, file); err != nil {
		return err
	}

	rev, err := strconv.ParseInt(c.QueryParam("Version"), 10, 64)
	if err != nil {
		return jsonapi.InvalidParameter("Version", err)
	}
	steps, err := note.GetSteps(inst, file.DocID, rev)
	if errors.Is(err, note.ErrTooOld) {
		file, err = note.GetFile(inst, file)
		if err != nil {
			return wrapError(err)
		}
		return files.FileData(c, http.StatusPreconditionFailed, file, false, nil, nil)
	}
	if err != nil {
		return wrapError(err)
	}

	objs := make([]jsonapi.Object, len(steps))
	for i, step := range steps {
		objs[i] = step
	}

	return jsonapi.DataList(c, http.StatusOK, objs, nil)
}

// PatchNote is the API handler for PATCH /notes/:id. It applies some steps on
// the note document.
func PatchNote(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	fileID := c.Param("id")
	file, err := inst.VFS().FileByID(fileID)
	if err != nil {
		return wrapError(err)
	}

	if err := middlewares.AllowVFS(c, permission.PATCH, file); err != nil {
		return err
	}

	objs, err := jsonapi.BindCompound(c.Request().Body)
	if err != nil {
		return err
	}
	steps := make([]note.Step, len(objs))
	for i, obj := range objs {
		if obj.Attributes == nil {
			return jsonapi.BadJSON()
		}
		if err = json.Unmarshal(*obj.Attributes, &steps[i]); err != nil {
			return wrapError(err)
		}
	}

	ifMatch := c.Request().Header.Get("If-Match")
	if file, err = note.ApplySteps(inst, file, ifMatch, steps); err != nil {
		return wrapError(err)
	}
	files.RecordActivity(c, file.ID(), recents.KindEdit)

	return files.FileData(c, http.StatusOK, file, false, nil, nil)
}

// ChangeTitle is the API handler for PUT /notes/:id/title. It updates the
// title and renames the file.
func ChangeTitle(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	fileID := c.Param("id")
	file, err := inst.VFS().FileByID(fileID)
	if err != nil {
		return wrapError(err)
	}

	if err := middlewares.AllowVFS(c, permission.PUT, file); err != nil {
		return err
	}

	event := note.Event{}
	if _, err := jsonapi.Bind(c.Request().Body, &event); err != nil {
		return err
	}

	title, _ := event["title"].(string)
	sessID, _ := event["sessionID"].(string)
	if file, err = note.UpdateTitle(inst, file, title, sessID); err != nil {
		return wrapError(err)
	}

	return files.FileData(c, http.StatusOK, file, false, nil, nil)
}

// PutTelepointer is the API handler for PUT /notes/:id/telepointer. It updates
// the position of a pointer.
func PutTelepointer(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	fileID := c.Param("id")
	file, err := inst.VFS().FileByID(fileID)
	if err != nil {
		return wrapError(err)
	}

	if err := middlewares.AllowVFS(c, permission.PUT, file); err != nil {
		return err
	}

	pointer := note.Event{}
	if _, err := jsonapi.Bind(c.Request().Body, &pointer); err != nil {
		return err
	}
	pointer.SetID(file.ID())

	if err := note.PutTelepointer(inst, pointer); err != nil {
		return wrapError(err)
	}

	return c.NoContent(http.StatusNoContent)
}

// ForceNoteSync is the API handler for POST /notes/:id/sync. It forces writing
// the note to the VFS
func ForceNoteSync(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	fileID := c.Param("id")
	file, err := inst.VFS().FileByID(fileID)
	if err != nil {
		return wrapError(err)
	}

	if err := middlewares.AllowVFS(c, permission.PUT, file); err != nil {
		return err
	}

	if err := note.Update(inst, file.ID()); err != nil {
		return wrapError(err)
	}

	return c.NoContent(http.StatusNoContent)
}

// OpenNoteURL is the API handler for GET /notes/:id/open. It returns the
// parameters to build the URL where the note can be opened.
func OpenNoteURL(c echo.Context) error {
	return OpenNoteURLWithReadOnly(c, c.QueryParam("ReadOnly") == "true")
}

// OpenNoteURLWithReadOnly is like OpenNoteURL, with the read-only flag given
// explicitly instead of read from the query.
func OpenNoteURLWithReadOnly(c echo.Context, readOnly bool) error {
	inst := middlewares.GetInstance(c)
	fileID := c.Param("file-id")
	open, err := sharing.OpenNote(inst, fileID)
	if err != nil {
		return wrapError(err)
	}

	pdoc, err := middlewares.GetPermission(c)
	if err != nil {
		return err
	}
	memberIndex, _ := strconv.Atoi(c.QueryParam("MemberIndex"))

	// If a directory is shared by link and contains a note, the note can be
	// opened with the same sharecode as the directory. The sharecode is also
	// used to identify the member that previews a sharing.
	if pdoc.Type == permission.TypeShareByLink ||
		pdoc.Type == permission.TypeSharePreview ||
		pdoc.Type == permission.TypeShareInteract {
		code := middlewares.GetRequestToken(c)
		open.AddShareByLinkCode(code)
	}

	sharingID := c.QueryParam("SharingID") // Cozy to Cozy sharing
	if err := open.CheckPermission(pdoc, sharingID); err != nil {
		return middlewares.ErrForbidden
	}

	doc, err := open.GetResult(memberIndex, readOnly)
	if err != nil {
		return wrapError(err)
	}

	return jsonapi.Data(c, http.StatusOK, doc, nil)
}

// UpdateNoteSchema is the API handler for PUT /notes/:id:/schema. It updates
// the schema of the note and invalidates the previous steps.
func UpdateNoteSchema(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	doc := &note.Document{}
	if _, err := jsonapi.Bind(c.Request().Body, doc); err != nil {
		return err
	}

	fileID := c.Param("id")
	file, err := inst.VFS().FileByID(fileID)
	if err != nil {
		return wrapError(err)
	}

	if err := middlewares.AllowVFS(c, permission.PUT, file); err != nil {
		return err
	}

	file, err = note.UpdateSchema(inst, file, doc.SchemaSpec)
	if err != nil {
		return wrapError(err)
	}

	return files.FileData(c, http.StatusOK, file, false, nil, nil)
}

// UploadImage is the API handler for POST /notes/:id/images. It uploads an
// image for the note.
func UploadImage(c echo.Context) error {
	// Check permission
	inst := middlewares.GetInstance(c)
	doc, err := inst.VFS().FileByID(c.Param("id"))
	if err != nil {
		return wrapError(err)
	}
	if err := middlewares.AllowVFS(c, permission.POST, doc); err != nil {
		return err
	}

	// Check that the uploaded file is an image
	contentType := c.Request().Header.Get(echo.HeaderContentType)
	if !strings.HasPrefix(contentType, "image/") {
		err := errors.New("Only images are accepted")
		return jsonapi.InvalidParameter(echo.HeaderContentType, err)
	}

	// Check the VFS quota
	quota := inst.DiskQuota()
	if quota > 0 {
		size := c.Request().ContentLength
		if size <= 0 {
			err := errors.New("The Content-Length header is mandatory")
			return jsonapi.InvalidParameter(echo.HeaderContentLength, err)
		}
		if size > note.MaxImageWeight {
			return jsonapi.Errorf(http.StatusRequestEntityTooLarge, "%s", vfs.ErrMaxFileSize)
		}
		used, err := inst.VFS().FilesUsage()
		if err != nil {
			return jsonapi.InternalServerError(errors.New("Cannot check quota"))
		}
		if used+size > quota {
			return jsonapi.Errorf(http.StatusRequestEntityTooLarge, "%s", vfs.ErrFileTooBig)
		}
	}

	// Create the image document
	name := c.QueryParam("Name")
	upload, err := note.NewImageUpload(inst, doc, name, contentType)
	if err != nil {
		inst.Logger().WithNamespace("notes").Infof("Image upload has failed: %s", err)
		return jsonapi.BadRequest(errors.New("Upload has failed"))
	}

	// Manage the content upload
	_, err = io.Copy(upload, c.Request().Body)
	if cerr := upload.Close(); cerr != nil && (err == nil || errors.Is(err, io.ErrUnexpectedEOF)) {
		err = cerr
	}
	if err != nil {
		inst.Logger().WithNamespace("notes").Infof("Image upload has failed: %s", err)
		return jsonapi.BadRequest(errors.New("Upload has failed"))
	}

	image := files.NewNoteImage(inst, upload.Image)
	return jsonapi.Data(c, http.StatusCreated, image, nil)
}

// CopyImage is the API handler for POST /notes/:id/:image-id/copy. It copies
// an existing image to another note.
func CopyImage(c echo.Context) error {
	// Check permission
	inst := middlewares.GetInstance(c)
	srcDoc, err := inst.VFS().FileByID(c.Param("id"))
	if err != nil {
		return wrapError(err)
	}
	if err := middlewares.AllowVFS(c, permission.POST, srcDoc); err != nil {
		return err
	}

	dstDoc, err := inst.VFS().FileByID(c.QueryParam("To"))
	if err != nil {
		return wrapError(err)
	}
	if err := middlewares.AllowVFS(c, permission.POST, dstDoc); err != nil {
		return err
	}

	imageID := c.Param("id") + "/" + c.Param("image-id")
	image, err := note.CopyImageToAnotherNote(inst, imageID, dstDoc)
	if err != nil {
		inst.Logger().WithNamespace("notes").Infof("Image copy has failed: %s", err)
		return wrapError(err)
	}

	apiImage := files.NewNoteImage(inst, image)
	return jsonapi.Data(c, http.StatusCreated, apiImage, nil)
}

// GetImage returns the image for a note, possibly resized.
func GetImage(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	_, err := inst.VFS().FileByID(c.Param("id"))
	if err != nil {
		return wrapError(err)
	}

	imageID := c.Param("id") + "/" + c.Param("image-id")
	secret := c.Param("secret")
	thumbID, err := vfs.GetStore().GetThumb(inst, secret)
	if err != nil {
		return wrapError(err)
	}
	if imageID != thumbID {
		return jsonapi.NewError(http.StatusBadRequest, "Wrong download token")
	}

	return inst.ThumbsFS().ServeNoteThumbContent(c.Response(), c.Request(), imageID)
}

// Routes sets the routing for the collaborative edition of notes.
func Routes(router *echo.Group) {
	router.POST("", CreateNote)
	router.GET("", ListNotes)
	router.GET("/:id", GetNote)
	router.GET("/:id/steps", GetSteps)
	router.GET("/:id/text", GetNoteText)
	router.GET("/texts", GetTexts)
	router.PATCH("/:id", PatchNote)
	router.PUT("/:id/title", ChangeTitle)
	router.PUT("/:id/telepointer", PutTelepointer)
	router.POST("/:id/sync", ForceNoteSync)
	router.GET("/:file-id/open", OpenNoteURL)
	router.PUT("/:id/schema", UpdateNoteSchema)
	router.POST("/:id/images", UploadImage)
	router.POST("/:id/:image-id/copy", CopyImage)
	router.GET("/:id/images/:image-id/:secret", GetImage)
}

func wrapError(err error) *jsonapi.Error {
	switch err {
	case note.ErrInvalidSchema:
		return jsonapi.InvalidAttribute("schema", err)
	case note.ErrInvalidFile, sharing.ErrCannotOpenFile:
		return jsonapi.NotFound(err)
	case note.ErrNoSteps, note.ErrInvalidSteps:
		return jsonapi.BadRequest(err)
	case note.ErrCannotApply:
		return jsonapi.Conflict(err)
	case os.ErrNotExist, vfs.ErrParentDoesNotExist, vfs.ErrParentInTrash:
		return jsonapi.NotFound(err)
	case vfs.ErrFileTooBig, vfs.ErrMaxFileSize:
		return jsonapi.Errorf(http.StatusRequestEntityTooLarge, "%s", err)
	case sharing.ErrMemberNotFound:
		return jsonapi.NotFound(err)
	}
	return jsonapi.InternalServerError(err)
}

func getCreatedBy(c echo.Context) string {
	if claims, ok := c.Get("claims").(permission.Claims); ok {
		switch claims.AudienceString() {
		case consts.AppAudience, consts.KonnectorAudience:
			return claims.Subject
		}
	}
	return ""
}
