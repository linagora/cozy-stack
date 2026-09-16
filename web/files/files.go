// Package files is the HTTP frontend of the vfs package. It exposes
// an HTTP api to manipulate the filesystem and offer all the
// possibilities given by the vfs.
package files

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/note"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/assets/statik"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/limits"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/metadata"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/cozy/cozy-stack/worker/thumbnail"
	"github.com/labstack/echo/v4"
	"github.com/ncw/swift/v2"
)

type docPatch struct {
	docID   string
	docPath string

	Trash  bool `json:"move_to_trash,omitempty"`
	Delete bool `json:"permanent_delete,omitempty"`
	vfs.DocPatch
}

// TagSeparator is the character separating tags
const TagSeparator = ","

// ErrDocTypeInvalid is used when the document type sent is not
// recognized
var ErrDocTypeInvalid = errors.New("Invalid document type")

// SharedDrivesCreationHandler is the handler for POST /files/drives. It
// creates the directory where shared and external drives are saved if it
// doesn't exist, and return information about this directory.
func SharedDrivesCreationHandler(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	if err := middlewares.AllowWholeType(c, permission.POST, consts.Files); err != nil {
		return err
	}
	doc, err := inst.EnsureSharedDrivesDir()
	if err != nil {
		return wrapVfsError(err)
	}
	return jsonapi.Data(c, http.StatusOK, NewDir(doc, nil), nil)
}

// Create handle all POST requests on /files/:file-id
// aiming at creating a new document in the FS. Given the Type
// parameter of the request, it will either upload a new file or
// create a new directory.
func Create(c echo.Context, sharedDrive *sharing.Sharing) error {
	instance := middlewares.GetInstance(c)
	var doc jsonapi.Object
	var err error
	switch c.QueryParam("Type") {
	case consts.FileType:
		doc, err = createFileHandler(c, instance.VFS(), sharedDrive)
	case consts.DirType:
		doc, err = createDirHandler(c, instance.VFS(), sharedDrive)
	default:
		err = ErrDocTypeInvalid
	}

	if err != nil {
		return WrapVfsError(err)
	}

	return jsonapi.Data(c, http.StatusCreated, doc, nil)
}

func CreationHandler(c echo.Context) error {
	return Create(c, nil)
}

func resolveDirID(c echo.Context) (string, error) {
	if _, err := middlewares.GetPermission(c); err != nil {
		return "", err
	}
	id, err := url.PathUnescape(c.Param("file-id"))
	if err != nil {
		return "", jsonapi.InvalidParameter("file-id", err)
	}
	id, err = middlewares.GetInstance(c).ResolveDirID(id)
	if err != nil {
		return "", WrapVfsError(err)
	}
	return id, nil
}

func createFileHandler(c echo.Context, fs vfs.VFS, sharedDrive *sharing.Sharing) (createdFile *file, err error) {
	inst := middlewares.GetInstance(c)
	dirID, err := resolveDirID(c)
	if err != nil {
		return nil, err
	}
	name := c.QueryParam("Name")
	doc, err := FileDocFromReq(c, name, dirID)
	if err != nil {
		return nil, err
	}

	if created := c.QueryParam("CreatedAt"); created != "" {
		if at, err2 := time.Parse(time.RFC3339, created); err2 == nil {
			doc.CreatedAt = at
		}
	}
	if updated := c.QueryParam("UpdatedAt"); updated != "" {
		if at, err3 := time.Parse(time.RFC3339, updated); err3 == nil {
			doc.UpdatedAt = at
		}
	}
	doc.CozyMetadata, _ = CozyMetadataFromClaims(c, true)

	err = checkPerm(c, "POST", nil, doc)
	if err != nil {
		return nil, err
	}

	if filepath.Ext(doc.DocName) == ".cozy-note" {
		err := note.ImportFile(inst, doc, nil, c.Request().Body)
		if err != nil {
			inst.Logger().WithNamespace("files").
				Infof("Cannot import note: %s", err)
			return nil, WrapVfsError(err)
		}
		return NewFile(doc, inst, sharedDrive), nil
	}

	file, err := fs.CreateFile(doc, nil)
	if err != nil {
		return nil, err
	}

	n, err := io.Copy(file, c.Request().Body)
	if err != nil {
		inst.Logger().WithNamespace("files").
			Warnf("Error on uploading file (copy): %s (%d bytes written - expected %d)", err, n, doc.ByteSize)
	}
	if cerr := file.Close(); cerr != nil && (err == nil || errors.Is(err, io.ErrUnexpectedEOF)) {
		err = cerr
		inst.Logger().WithNamespace("files").
			Warnf("Error on uploading file (close): %s", err)
	}
	if err != nil {
		return nil, wrapVfsError(err)
	}
	maybeNotifyShareByLinkUpload(c, inst, doc.DocName, doc.ID(), doc.DirID, false)
	return NewFile(doc, inst, sharedDrive), nil
}

// maybeNotifyShareByLinkUpload sends a notification
// if the file/folder was uploaded via a share-by-link permission (public link).
func maybeNotifyShareByLinkUpload(c echo.Context, inst *instance.Instance, name, id, dirID string, isFolder bool) {
	perm, err := middlewares.GetPermission(c)
	if err != nil {
		return
	}
	if perm.Type == permission.TypeShareByLink {
		sharing.MaybeNotifyShareByLinkUpload(inst, name, id, dirID, isFolder)
	}
}

func createDirHandler(c echo.Context, fs vfs.VFS, sharedDrive *sharing.Sharing) (createdDir *dir, err error) {
	inst := middlewares.GetInstance(c)
	path := c.QueryParam("Path")
	tags := utils.SplitTrimString(c.QueryParam("Tags"), TagSeparator)

	var doc *vfs.DirDoc
	if path != "" {
		if c.QueryParam("Recursive") == "true" {
			doc, err = vfs.MkdirAll(fs, path)
		} else {
			doc, err = vfs.Mkdir(fs, path, tags)
		}
		if err != nil {
			return nil, err
		}
		maybeNotifyShareByLinkUpload(c, inst, doc.DocName, doc.ID(), doc.DirID, true)
		return NewDir(doc, sharedDrive), nil
	}

	dirID, err := resolveDirID(c)
	if err != nil {
		return nil, err
	}
	name := c.QueryParam("Name")
	doc, err = vfs.NewDirDoc(fs, name, dirID, tags)
	if err != nil {
		return nil, err
	}
	if date := c.Request().Header.Get("Date"); date != "" {
		if t, err2 := time.Parse(time.RFC1123, date); err2 == nil {
			doc.CreatedAt = t
			doc.UpdatedAt = t
		}
	}
	if created := c.QueryParam("CreatedAt"); created != "" {
		if at, err2 := time.Parse(time.RFC3339, created); err2 == nil {
			doc.CreatedAt = at
		}
	}

	if updated := c.QueryParam("UpdatedAt"); updated != "" {
		if at, err3 := time.Parse(time.RFC3339, updated); err3 == nil {
			doc.UpdatedAt = at
		}
	}

	if secret := c.QueryParam("MetadataID"); secret != "" {
		meta, err := vfs.GetStore().GetMetadata(inst, secret)
		if err != nil {
			return nil, err
		}
		doc.Metadata = *meta
	}

	if len(doc.Metadata) > 0 {
		if _, ok := doc.Metadata[consts.CarbonCopyKey]; ok {
			if err := middlewares.AllowWholeType(c, permission.POST, consts.CertifiedCarbonCopy); err != nil {
				delete(doc.Metadata, consts.CarbonCopyKey)
			}
		}
		if _, ok := doc.Metadata[consts.ElectronicSafeKey]; ok {
			if err := middlewares.AllowWholeType(c, permission.POST, consts.CertifiedElectronicSafe); err != nil {
				delete(doc.Metadata, consts.ElectronicSafeKey)
			}
		}
	}

	doc.CozyMetadata, _ = CozyMetadataFromClaims(c, false)

	err = checkPerm(c, "POST", doc, nil)
	if err != nil {
		return nil, err
	}

	if err = fs.CreateDir(doc); err != nil {
		return nil, err
	}

	maybeNotifyShareByLinkUpload(c, inst, doc.DocName, doc.ID(), doc.DirID, true)
	return NewDir(doc, sharedDrive), nil
}

// OverwriteFileContent handles PUT requests on /files/:file-id
// to overwrite the content of a file given its identifier.
func OverwriteFileContent(c echo.Context, sharedDrive *sharing.Sharing) error {
	instance := middlewares.GetInstance(c)

	fileID := c.Param("file-id")
	if fileID == "" {
		fileID = c.Param("docid") // Used by sharings.updateDocument
	}

	olddoc, err := instance.VFS().FileByID(fileID)
	if err != nil {
		return WrapVfsError(err)
	}

	newdoc, err := FileDocFromReq(c, olddoc.DocName, olddoc.DirID)
	if err != nil {
		return WrapVfsError(err)
	}

	if updated := c.QueryParam("UpdatedAt"); updated != "" {
		if at, err2 := time.Parse(time.RFC3339, updated); err2 == nil {
			newdoc.UpdatedAt = at
		}
	}

	newdoc.ReferencedBy = olddoc.ReferencedBy

	if err := CheckIfMatch(c, olddoc.Rev()); err != nil {
		return WrapVfsError(err)
	}

	if olddoc.CozyMetadata != nil {
		newdoc.CozyMetadata = olddoc.CozyMetadata.Clone()
	}
	UpdateFileCozyMetadata(c, newdoc, true)

	err = checkPerm(c, permission.PUT, nil, olddoc)
	if err != nil {
		return err
	}

	newdoc.SetID(olddoc.ID()) // The ID can be useful to check permissions
	err = checkPerm(c, permission.PUT, nil, newdoc)
	if err != nil {
		return err
	}

	if filepath.Ext(newdoc.DocName) == ".cozy-note" {
		err := note.ImportFile(instance, newdoc, olddoc, c.Request().Body)
		if err != nil {
			instance.Logger().WithNamespace("files").
				Infof("Cannot import note: %s", err)
			return WrapVfsError(err)
		}
		return FileData(c, http.StatusOK, newdoc, true, nil, sharedDrive)
	}

	file, err := instance.VFS().CreateFile(newdoc, olddoc)
	if err != nil {
		return WrapVfsError(err)
	}
	_, err = io.Copy(file, c.Request().Body)
	if cerr := file.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return WrapVfsError(err)
	}
	return FileData(c, http.StatusOK, newdoc, true, nil, sharedDrive)
}

func OverwriteFileContentHandler(c echo.Context) error {
	return OverwriteFileContent(c, nil)
}

// UploadMetadataHandler accepts a metadata objet and persist it, so that it
// can be used in a future file upload.
func UploadMetadataHandler(c echo.Context) error {
	if err := checkPerm(c, permission.POST, nil, &vfs.FileDoc{}); err != nil {
		return err
	}

	meta := &vfs.Metadata{}
	if _, err := jsonapi.Bind(c.Request().Body, meta); err != nil {
		return err
	}

	instance := middlewares.GetInstance(c)
	secret, err := vfs.GetStore().AddMetadata(instance, meta)
	if err != nil {
		return WrapVfsError(err)
	}

	m := apiMetadata{
		Metadata: meta,
		secret:   secret,
	}
	return jsonapi.Data(c, http.StatusCreated, &m, nil)
}

// Copy a single file from an instance to itself using parameters from the echo Context:
// - url param: `file-id`: surce file's ID
// - url query param: `DirID`: optional destination folder's ID
// - url query param: `Name`: optional destination file name
func CopyFile(c echo.Context, inst *instance.Instance, sharedDrive *sharing.Sharing) error {
	fileID := c.Param("file-id")
	destinationDirID := c.QueryParam("DirID")
	destinationName := c.QueryParam("Name")

	fs := inst.VFS()

	olddoc, err := inst.VFS().FileByID(fileID)
	if err != nil {
		return WrapVfsError(err)
	}
	copyName := destinationName
	if copyName == "" {
		copyName = fileCopyName(inst, olddoc.DocName)
	}
	newdoc := vfs.CreateFileDocCopy(olddoc, destinationDirID, copyName)

	err = checkPerm(c, permission.POST, nil, newdoc)
	if err != nil {
		return err
	}

	exists, err := fs.GetIndexer().DirChildExists(newdoc.DirID, newdoc.DocName)
	if err != nil {
		return WrapVfsError(err)
	}
	if exists {
		newdoc.DocName = vfs.ConflictName(fs, newdoc.DirID, newdoc.DocName, true)
		exists, err = fs.GetIndexer().DirChildExists(newdoc.DirID, newdoc.DocName)
		if err != nil {
			return WrapVfsError(err)
		}
		if exists {
			return WrapVfsError(os.ErrExist)
		}
	}
	newdoc.ResetFullpath()
	UpdateFileCozyMetadata(c, newdoc, true)

	if olddoc.Mime == consts.NoteMimeType {
		// We need a special copy for notes because of their images
		err = note.CopyFile(inst, olddoc, newdoc)
	} else {
		err = fs.CopyFile(olddoc, newdoc)
	}
	if err != nil {
		return WrapVfsError(err)
	}

	return FileData(c, http.StatusCreated, newdoc, false, nil, sharedDrive)
}

// FileCopyHandler handles POST requests on /files/:file-id/copy
//
// It is used to duplicate the given file and its metadata except for
// relationships.
func FileCopyHandler(c echo.Context) error {
	return CopyFile(c, middlewares.GetInstance(c), nil)
}

// ModifyMetadataByIDHandler handles PATCH requests on /files/:file-id
//
// It can be used to modify the file or directory metadata, as well as
// moving and renaming it in the filesystem.
func ModifyMetadataByIDHandler(c echo.Context) error {
	patch, err := getPatch(c, c.Param("file-id"), "")
	if err != nil {
		return WrapVfsError(err)
	}
	i := middlewares.GetInstance(c)
	if err = applyPatch(c, i.VFS(), patch); err != nil {
		return WrapVfsError(err)
	}
	return nil
}

// ModifyMetadataByIDInBatchHandler handles PATCH requests on /files/.
//
// It can be used to modify many files or directories metadata, as well as
// moving and renaming it in the filesystem, in batch.
func ModifyMetadataByIDInBatchHandler(c echo.Context) error {
	patches, err := getPatches(c)
	if err != nil {
		return WrapVfsError(err)
	}
	i := middlewares.GetInstance(c)
	patchErrors, err := applyPatches(c, i.VFS(), patches)
	if err != nil {
		return err
	}
	if len(patchErrors) > 0 {
		return jsonapi.DataErrorList(c, patchErrors...)
	}
	return c.NoContent(http.StatusNoContent)
}

// ModifyMetadataByPathHandler handles PATCH requests on /files/metadata
//
// It can be used to modify the file or directory metadata, as well as
// moving and renaming it in the filesystem.
func ModifyMetadataByPathHandler(c echo.Context) error {
	patch, err := getPatch(c, "", c.QueryParam("Path"))
	if err != nil {
		return WrapVfsError(err)
	}
	i := middlewares.GetInstance(c)
	if err = applyPatch(c, i.VFS(), patch); err != nil {
		return WrapVfsError(err)
	}
	return nil
}

// ModifyFileVersionMetadata handles PATCH requests on /files/:file-id/:version-id
//
// It can be used to modify tags on an old version of a file.
func ModifyFileVersionMetadata(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	fileID := c.Param("file-id")
	_, file, err := inst.VFS().DirOrFileByID(fileID)
	if err != nil {
		return WrapVfsError(err)
	}
	if file == nil {
		return WrapVfsError(vfs.ErrConflict)
	}
	if err = checkPerm(c, permission.PATCH, nil, file); err != nil {
		return WrapVfsError(err)
	}
	docID := fileID + "/" + c.Param("version-id")
	version, err := vfs.FindVersion(inst, docID)
	if err != nil {
		return WrapVfsError(err)
	}
	var patch vfs.DocPatch
	if _, err = jsonapi.Bind(c.Request().Body, &patch); err != nil || patch.Tags == nil {
		return jsonapi.BadJSON()
	}
	version.Tags = *patch.Tags
	version.CozyMetadata.UpdatedAt = time.Now()
	if err = couchdb.UpdateDoc(inst, version); err != nil {
		return WrapVfsError(err)
	}
	return jsonapi.Data(c, http.StatusOK, version, nil)
}

// DeleteFileVersionMetadata handles DELETE requests on /files/:file-id/:version-id
//
// It can be used to delete an old version of a file.
func DeleteFileVersionMetadata(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	fs := inst.VFS()
	fileID := c.Param("file-id")
	_, file, err := fs.DirOrFileByID(fileID)
	if err != nil {
		return WrapVfsError(err)
	}
	if file == nil {
		return WrapVfsError(vfs.ErrConflict)
	}
	if err = checkPerm(c, permission.DELETE, nil, file); err != nil {
		return WrapVfsError(err)
	}
	docID := fileID + "/" + c.Param("version-id")
	version, err := vfs.FindVersion(inst, docID)
	if err != nil {
		return WrapVfsError(err)
	}
	if err := fs.CleanOldVersion(fileID, version); err != nil {
		return WrapVfsError(err)
	}
	return c.NoContent(http.StatusNoContent)
}

// CopyVersionHandler handles POST requests on /files/:file-id/versions.
//
// It can be used to create a new version of a file, with the same content but
// new metadata.
func CopyVersionHandler(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	fs := inst.VFS()
	fileID := c.Param("file-id")
	// Provide a clearer error when the target is a directory
	dir, olddoc, err := fs.DirOrFileByID(fileID)
	if err != nil {
		return WrapVfsError(err)
	}
	if dir != nil {
		return jsonapi.Errorf(http.StatusBadRequest, "cannot create versions for directories")
	}
	if olddoc == nil {
		return WrapVfsError(vfs.ErrConflict)
	}
	if err = checkPerm(c, permission.PUT, nil, olddoc); err != nil {
		return WrapVfsError(err)
	}

	meta := vfs.Metadata{}
	if _, err := jsonapi.Bind(c.Request().Body, &meta); err != nil {
		return err
	}

	// Manage the special cases of carbonCopy & electronicSafe
	if len(meta) > 0 {
		if _, ok := meta[consts.CarbonCopyKey]; ok {
			if err := middlewares.AllowWholeType(c, permission.POST, consts.CertifiedCarbonCopy); err != nil {
				delete(meta, consts.CarbonCopyKey)
			}
		}
		if _, ok := meta[consts.ElectronicSafeKey]; ok {
			if err := middlewares.AllowWholeType(c, permission.POST, consts.CertifiedElectronicSafe); err != nil {
				delete(meta, consts.ElectronicSafeKey)
			}
		}
	}
	keepCarbonCopy := true
	keepElectronicSafe := true
	for key := range meta {
		switch key {
		case consts.CarbonCopyKey:
			keepCarbonCopy = false
		case consts.ElectronicSafeKey:
			keepElectronicSafe = false
		case "qualification":
			//
		default:
			keepCarbonCopy = false
			keepElectronicSafe = false
		}
	}
	if value, ok := olddoc.Metadata[consts.CarbonCopyKey]; ok && keepCarbonCopy {
		meta[consts.CarbonCopyKey] = value
	}
	if value, ok := olddoc.Metadata[consts.ElectronicSafeKey]; ok && keepElectronicSafe {
		meta[consts.ElectronicSafeKey] = value
	}

	// For notes, preserve the prosemirror metadata
	if olddoc.Mime == consts.NoteMimeType {
		for _, name := range []string{"title", "content", "schema", "version"} {
			if meta[name] == nil {
				meta[name] = olddoc.Metadata[name]
			}
		}
	}

	newdoc := olddoc.Clone().(*vfs.FileDoc)
	newdoc.Metadata = meta
	newdoc.Tags = utils.SplitTrimString(c.QueryParam("Tags"), TagSeparator)
	UpdateFileCozyMetadata(c, newdoc, true)

	content, err := fs.OpenFile(olddoc)
	if err != nil {
		return WrapVfsError(err)
	}
	defer content.Close()

	file, err := fs.CreateFile(newdoc, olddoc)
	if err != nil {
		return WrapVfsError(err)
	}

	defer func() {
		if cerr := file.Close(); cerr != nil && err == nil {
			err = cerr
		}
		if err != nil {
			err = WrapVfsError(err)
			return
		}
		err = FileData(c, http.StatusOK, newdoc, true, nil, nil)
	}()

	_, err = io.Copy(file, content)
	return err
}

// ClearOldVersions is the handler for DELETE /files/versions.
// It deletes all the old versions of all files to make space for new files.
func ClearOldVersions(c echo.Context) error {
	if err := middlewares.AllowWholeType(c, permission.DELETE, consts.Files); err != nil {
		return err
	}

	fs := middlewares.GetInstance(c).VFS()
	if err := fs.ClearOldVersions(); err != nil {
		return WrapVfsError(err)
	}

	return c.NoContent(204)
}

func getPatch(c echo.Context, docID, docPath string) (*docPatch, error) {
	var patch docPatch
	obj, err := jsonapi.Bind(c.Request().Body, &patch)
	if err != nil {
		return nil, jsonapi.BadJSON()
	}
	patch.docID = docID
	patch.docPath = docPath
	patch.RestorePath = nil
	if rel, ok := obj.GetRelationship("parent"); ok {
		rid, ok := rel.ResourceIdentifier()
		if !ok {
			return nil, jsonapi.BadJSON()
		}
		patch.DirID = &rid.ID
	}
	return &patch, nil
}

func getPatches(c echo.Context) ([]*docPatch, error) {
	req := c.Request()
	objs, err := jsonapi.BindCompound(req.Body)
	if err != nil {
		return nil, jsonapi.BadJSON()
	}
	patches := make([]*docPatch, len(objs))
	for i, obj := range objs {
		var patch docPatch
		if obj.Attributes == nil {
			return nil, jsonapi.BadJSON()
		}
		if err = json.Unmarshal(*obj.Attributes, &patch); err != nil {
			return nil, err
		}
		patch.docID = obj.ID
		patch.docPath = ""
		patch.RestorePath = nil
		if rel, ok := obj.GetRelationship("parent"); ok {
			rid, ok := rel.ResourceIdentifier()
			if !ok {
				return nil, jsonapi.BadJSON()
			}
			patch.DirID = &rid.ID
		}
		patches[i] = &patch
	}
	return patches, nil
}

func applyPatch(c echo.Context, fs vfs.VFS, patch *docPatch) (err error) {
	var file *vfs.FileDoc
	var dir *vfs.DirDoc
	if patch.docID != "" {
		dir, file, err = fs.DirOrFileByID(patch.docID)
	} else {
		dir, file, err = fs.DirOrFileByPath(patch.docPath)
	}
	if err != nil {
		return err
	}

	var rev string
	if dir != nil {
		rev = dir.Rev()
	} else {
		rev = file.Rev()
	}

	if err = CheckIfMatch(c, rev); err != nil {
		return err
	}

	if err = checkPerm(c, permission.PATCH, dir, file); err != nil {
		return err
	}

	if err = checkMoveDestination(c, fs, patch, dir, file); err != nil {
		return err
	}

	if patch.Delete {
		if dir != nil {
			inst := middlewares.GetInstance(c)
			err = fs.DestroyDirAndContent(dir, pushTrashJob(inst))
		} else {
			err = fs.DestroyFile(file)
		}
	} else if patch.Trash {
		if dir != nil {
			UpdateDirTrashCozyMetadata(c, dir)
			dir, err = vfs.TrashDir(fs, dir)
		} else {
			UpdateFileTrashCozyMetadata(c, file)
			file, err = vfs.TrashFile(fs, file)
		}
	} else {
		if dir != nil {
			oldDirName := dir.DocName
			UpdateDirCozyMetadata(c, dir)
			dir, err = vfs.ModifyDirMetadata(fs, dir, &patch.DocPatch)
			if err == nil && patch.Name != nil && oldDirName != *patch.Name {
				sharing.UpdateSharingDescriptionIfNeeded(middlewares.GetInstance(c), dir.ReferencedBy, dir.DocName)
			}
		} else {
			oldFileName := file.DocName
			UpdateFileCozyMetadata(c, file, false)
			file, err = vfs.ModifyFileMetadata(fs, file, &patch.DocPatch)
			if err == nil && patch.Name != nil && oldFileName != *patch.Name {
				sharing.UpdateSharingDescriptionIfNeeded(middlewares.GetInstance(c), file.ReferencedBy, file.DocName)
			}
		}
	}
	if err != nil {
		return err
	}

	if dir != nil {
		return DirData(c, http.StatusOK, dir, nil)
	}
	return FileData(c, http.StatusOK, file, false, nil, nil)
}

func applyPatches(c echo.Context, fs vfs.VFS, patches []*docPatch) (errors []*jsonapi.Error, err error) {
	for _, patch := range patches {
		dir, file, errf := fs.DirOrFileByID(patch.docID)
		if errf != nil {
			jsonapiError := wrapVfsErrorJSONAPI(errf)
			jsonapiError.Source.Parameter = "_id"
			jsonapiError.Source.Pointer = patch.docID
			errors = append(errors, jsonapiError)
			continue
		}
		if err = checkPerm(c, permission.PATCH, dir, file); err != nil {
			return
		}
		if err = checkMoveDestination(c, fs, patch, dir, file); err != nil {
			return
		}
		var errp error
		if patch.Delete {
			if dir != nil {
				inst := middlewares.GetInstance(c)
				errp = fs.DestroyDirAndContent(dir, pushTrashJob(inst))
			} else if file != nil {
				errp = fs.DestroyFile(file)
			}
		} else if patch.Trash {
			if dir != nil {
				UpdateDirTrashCozyMetadata(c, dir)
				_, errp = vfs.TrashDir(fs, dir)
			} else if file != nil {
				UpdateFileTrashCozyMetadata(c, file)
				_, errp = vfs.TrashFile(fs, file)
			}
		} else if dir != nil {
			oldDirName := dir.DocName
			UpdateDirCozyMetadata(c, dir)
			dir, errp = vfs.ModifyDirMetadata(fs, dir, &patch.DocPatch)
			if errp == nil && patch.Name != nil && oldDirName != *patch.Name {
				sharing.UpdateSharingDescriptionIfNeeded(middlewares.GetInstance(c), dir.ReferencedBy, dir.DocName)
			}
		} else if file != nil {
			oldFileName := file.DocName
			UpdateFileCozyMetadata(c, file, false)
			file, errp = vfs.ModifyFileMetadata(fs, file, &patch.DocPatch)
			if errp == nil && patch.Name != nil && oldFileName != *patch.Name {
				sharing.UpdateSharingDescriptionIfNeeded(middlewares.GetInstance(c), file.ReferencedBy, file.DocName)
			}
		}
		if errp != nil {
			jsonapiError := wrapVfsErrorJSONAPI(errp)
			jsonapiError.Source.Parameter = "_id"
			jsonapiError.Source.Pointer = patch.docID
			errors = append(errors, jsonapiError)
		}
	}

	return
}

// ReadMetadataFromIDHandler handles all GET requests on /files/:file-
// id aiming at getting file metadata from its id.
func ReadMetadataFromIDHandler(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	perm, err := middlewares.GetPermission(c)
	if err != nil {
		return err
	}

	fileID, err := resolveDirID(c)
	if err != nil {
		return err
	}

	dir, file, err := instance.VFS().DirOrFileByID(fileID)
	if err != nil {
		return WrapVfsError(err)
	}

	if err := checkPerm(c, permission.GET, dir, file); err != nil {
		return err
	}

	// Limiting the number of public share link consultations
	if perm.Type == permission.TypeShareByLink {
		err = config.GetRateLimiter().CheckRateLimitKey(fileID, limits.SharingPublicLinkType)
		if limits.IsLimitReachedOrExceeded(err) {
			return err
		}
	}

	if dir != nil {
		return DirData(c, http.StatusOK, dir, nil)
	}
	return FileData(c, http.StatusOK, file, true, nil, nil)
}

// GetChildrenHandler returns a list of children of a folder
func GetChildrenHandler(c echo.Context) error {
	instance := middlewares.GetInstance(c)

	fileID, err := resolveDirID(c)
	if err != nil {
		return err
	}

	dir, file, err := instance.VFS().DirOrFileByID(fileID)
	if err != nil {
		return WrapVfsError(err)
	}

	if err := checkPerm(c, permission.GET, dir, file); err != nil {
		return err
	}

	if file != nil {
		return jsonapi.Errorf(http.StatusBadRequest, "cant read children of file %v", fileID)
	}

	return dirDataList(c, http.StatusOK, dir)
}

type ApiDiskSize struct {
	DocID string `json:"id,omitempty"`
	Size  int64  `json:"size,string"`
}

func (d *ApiDiskSize) ID() string                             { return d.DocID }
func (d *ApiDiskSize) Rev() string                            { return "" }
func (d *ApiDiskSize) DocType() string                        { return consts.DirSizes }
func (d *ApiDiskSize) Clone() couchdb.Doc                     { return d }
func (d *ApiDiskSize) SetID(id string)                        { d.DocID = id }
func (d *ApiDiskSize) SetRev(_ string)                        {}
func (d *ApiDiskSize) Relationships() jsonapi.RelationshipMap { return nil }
func (d *ApiDiskSize) Included() []jsonapi.Object             { return nil }
func (d *ApiDiskSize) Links() *jsonapi.LinksList              { return nil }

// GetDirSize returns the size of a directory (the sum of the size of the files
// in this directory, including those in subdirectories).
func GetDirSize(c echo.Context) error {
	fs := middlewares.GetInstance(c).VFS()
	fileID, err := resolveDirID(c)
	if err != nil {
		return err
	}

	dir, err := fs.DirByID(fileID)
	if err != nil {
		return WrapVfsError(err)
	}
	if err := checkPerm(c, permission.GET, dir, nil); err != nil {
		return err
	}

	size, err := fs.DirSize(dir)
	if err != nil {
		return WrapVfsError(err)
	}

	result := ApiDiskSize{DocID: fileID, Size: size}
	return jsonapi.Data(c, http.StatusOK, &result, nil)
}

// ReadMetadataFromPathHandler handles all GET requests on
// /files/metadata aiming at getting file metadata from its path.
func ReadMetadataFromPathHandler(c echo.Context) error {
	return ReadMetadataFromPath(c, nil)
}

func ReadMetadataFromPath(c echo.Context, sharedDrive *sharing.Sharing) error {
	var err error
	instance := middlewares.GetInstance(c)

	dir, file, err := instance.VFS().DirOrFileByPath(c.QueryParam("Path"))
	if err != nil {
		return WrapVfsError(err)
	}

	if err := checkPerm(c, permission.GET, dir, file); err != nil {
		return err
	}

	if dir != nil {
		return DirData(c, http.StatusOK, dir, sharedDrive)
	}
	return FileData(c, http.StatusOK, file, true, nil, sharedDrive)
}

// ReadFileContentFromIDHandler handles all GET requests on /files/:file-id
// aiming at downloading a file given its ID. It serves the file in inline
// mode.
// TODO: use SendFileFromDoc?
func ReadFileContentFromIDHandler(c echo.Context) error {
	instance := middlewares.GetInstance(c)

	doc, err := instance.VFS().FileByID(c.Param("file-id"))
	if err != nil {
		return WrapVfsError(err)
	}

	err = checkPerm(c, permission.GET, nil, doc)
	if err != nil {
		return err
	}

	if err = CheckAntivirusAction(instance, doc, ActionDownload); err != nil {
		return err
	}

	disposition := "inline"
	if c.QueryParam("Dl") == "1" {
		disposition = "attachment"
	}

	if page := c.QueryParam("Page"); page != "" {
		p, errp := strconv.Atoi(page)
		if errp != nil {
			return jsonapi.InvalidParameter("Page", errp)
		}
		err = vfs.ServePDFPage(instance.VFS(), doc, disposition, p, c.Request(), c.Response())
	} else {
		err = vfs.ServeFileContent(instance.VFS(), doc, nil, "", disposition, c.Request(), c.Response())
	}
	if err != nil {
		return WrapVfsError(err)
	}

	return nil
}

// ReadFileContentFromVersion handles the download of an old version of the
// file content.
func ReadFileContentFromVersion(c echo.Context) error {
	instance := middlewares.GetInstance(c)

	doc, err := instance.VFS().FileByID(c.Param("file-id"))
	if err != nil {
		return WrapVfsError(err)
	}

	err = checkPerm(c, permission.GET, nil, doc)
	if err != nil {
		return err
	}

	if err = CheckAntivirusAction(instance, doc, ActionDownload); err != nil {
		return err
	}

	version, err := vfs.FindVersion(instance, doc.DocID+"/"+c.Param("version-id"))
	if err != nil {
		return WrapVfsError(err)
	}

	disposition := "inline"
	if c.QueryParam("Dl") == "1" {
		disposition = "attachment"
	}
	err = vfs.ServeFileContent(instance.VFS(), doc, version, "", disposition, c.Request(), c.Response())
	if err != nil {
		return WrapVfsError(err)
	}

	return nil
}

// RevertFileVersion restores an old version of the file content.
func RevertFileVersion(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	doc, err := inst.VFS().FileByID(c.Param("file-id"))
	if err != nil {
		return WrapVfsError(err)
	}

	if err = checkPerm(c, permission.POST, nil, doc); err != nil {
		return err
	}

	version, err := vfs.FindVersion(inst, doc.DocID+"/"+c.Param("version-id"))
	if err != nil {
		return WrapVfsError(err)
	}

	if err = inst.VFS().RevertFileVersion(doc, version); err != nil {
		return WrapVfsError(err)
	}

	return FileData(c, http.StatusOK, doc, true, nil, nil)
}

// HeadDirOrFile handles HEAD requests on directory or file to check their
// existence
func HeadDirOrFile(c echo.Context) error {
	instance := middlewares.GetInstance(c)

	dir, file, err := instance.VFS().DirOrFileByID(c.Param("file-id"))
	if err != nil {
		return WrapVfsError(err)
	}

	if dir != nil {
		err = checkPerm(c, permission.GET, dir, nil)
	} else {
		err = checkPerm(c, permission.GET, nil, file)
	}
	if err != nil {
		return err
	}

	return nil
}

// ThumbnailHandler serves thumbnails of the images/photos
func ThumbnailHandler(c echo.Context) error {
	instance := middlewares.GetInstance(c)

	secret := c.Param("secret")
	fileID, err := vfs.GetStore().GetThumb(instance, secret)
	if err != nil {
		return WrapVfsError(err)
	}
	if c.Param("file-id") != fileID {
		return jsonapi.NewError(http.StatusBadRequest, "Wrong download token")
	}

	doc, err := instance.VFS().FileByID(fileID)
	if err != nil {
		return WrapVfsError(err)
	}

	if err = CheckAntivirusAction(instance, doc, ActionPreview); err != nil {
		return err
	}

	fs := instance.ThumbsFS()
	format := c.Param("format")
	err = fs.ServeThumbContent(c.Response(), c.Request(), doc, format)
	if err != nil {
		if !errors.Is(err, os.ErrInvalid) {
			msg, _ := job.NewMessage(thumbnail.ImageMessage{
				File:   doc,
				Format: format,
			})
			_, _ = job.System().PushJob(instance, &job.JobRequest{
				WorkerType: "thumbnail",
				Message:    msg,
			})
		}
		return serveThumbnailPlaceholder(c.Response(), c.Request(), doc, format)
	}
	return nil
}

func serveThumbnailPlaceholder(res http.ResponseWriter, req *http.Request, doc *vfs.FileDoc, format string) error {
	if !utils.IsInArray(format, vfs.ThumbnailFormatNames) {
		return echo.NewHTTPError(http.StatusNotFound, "Format does not exist")
	}
	f := statik.GetAsset("/placeholders/thumbnail-" + format + ".png")
	if f == nil {
		return os.ErrNotExist
	}
	etag := f.Etag
	if utils.CheckPreconditions(res, req, etag) {
		return nil
	}
	res.Header().Set("Etag", etag)
	res.WriteHeader(http.StatusNotFound)
	_, err := io.Copy(res, f.Reader())
	return err
}

// SendFileFromDoc sends the file content to the client.
// It does permissions, inlining, CSP rules, and PDF single page serving
func SendFileFromDoc(instance *instance.Instance, c echo.Context, doc *vfs.FileDoc, checkPermission bool) error {
	var err error
	if checkPermission {
		err = middlewares.Allow(c, permission.GET, doc)
		if err != nil {
			return err
		}
	}

	if err = CheckAntivirusAction(instance, doc, ActionDownload); err != nil {
		return err
	}

	// Forbid extracting autofilled passwords on an HTML page hosted in the Cozy
	if !config.GetConfig().CSPDisabled {
		middlewares.AppendCSPRule(c, "form-action", "'none'")
	}

	disposition := "inline"
	if c.QueryParam("Dl") == "1" {
		disposition = "attachment"
	} else if !checkPermission {
		addCSPRuleForDirectLink(c, doc.Class, doc.Mime)
	}

	if page := c.QueryParam("Page"); page != "" {
		p, errp := strconv.Atoi(page)
		if errp != nil {
			return jsonapi.InvalidParameter("Page", errp)
		}
		err = vfs.ServePDFPage(instance.VFS(), doc, disposition, p, c.Request(), c.Response())
	} else {
		err = vfs.ServeFileContent(instance.VFS(), doc, nil, "", disposition, c.Request(), c.Response())
	}
	if err != nil {
		return WrapVfsError(err)
	}

	return nil
}

func sendFileFromPath(c echo.Context, path string, checkPermission bool) error {
	instance := middlewares.GetInstance(c)

	doc, err := instance.VFS().FileByPath(path)
	if err != nil {
		return WrapVfsError(err)
	}
	return SendFileFromDoc(instance, c, doc, checkPermission)
}

func addCSPRuleForDirectLink(c echo.Context, class, mime string) {
	if config.GetConfig().CSPDisabled {
		return
	}
	// Allow some files to be displayed by the browser in the client-side apps
	if mime == "text/plain" || class == "image" || class == "audio" || class == "video" || mime == "application/pdf" {
		middlewares.AppendCSPRule(c, "frame-ancestors", "*")
	}
}

// ReadFileContentFromPathHandler handles all GET request on /files/download
// aiming at downloading a file given its path. It serves the file in in
// attachment mode.
func ReadFileContentFromPathHandler(c echo.Context) error {
	return sendFileFromPath(c, c.QueryParam("Path"), true)
}

func ReadFileContentFromSharingFileID(c echo.Context, fileID string) error {
	return sendFileFromPath(c, c.QueryParam("Path"), true)
}

// ArchiveDownload stores the archive parameters into a secret usable for the
// download handler below. When sharedDrive is not nil, the returned link
// points to the shared drive archive endpoint instead of /files/archive.
func ArchiveDownload(c echo.Context, sharedDrive *sharing.Sharing) error {
	archive := &vfs.Archive{}
	if _, err := jsonapi.Bind(c.Request().Body, archive); err != nil {
		return err
	}
	if len(archive.Files) == 0 && len(archive.IDs) == 0 && len(archive.Pages) == 0 {
		return c.JSON(http.StatusBadRequest, "Can't create an archive with no files")
	}
	if strings.Contains(archive.Name, "/") {
		return c.JSON(http.StatusBadRequest, "The archive filename can't contain a /")
	}
	if archive.Name == "" {
		archive.Name = "archive"
	}
	instance := middlewares.GetInstance(c)

	entries, err := archive.GetEntries(instance.VFS())
	if err != nil {
		return WrapVfsError(err)
	}

	for _, e := range entries {
		err = checkPerm(c, permission.GET, e.Dir, e.File)
		if err != nil {
			return err
		}
		// Check antivirus status for files
		if e.File != nil {
			if err = CheckAntivirusAction(instance, e.File, ActionDownload); err != nil {
				return err
			}
		}
	}

	// if accept header is application/zip, send the archive immediately
	if c.Request().Header.Get(echo.HeaderAccept) == "application/zip" {
		return archive.Serve(instance.VFS(), c.Response())
	}

	secret, err := vfs.GetStore().AddArchive(instance, archive)
	if err != nil {
		return WrapVfsError(err)
	}
	archive.Secret = secret

	fakeName := url.PathEscape(archive.Name)

	var links *jsonapi.LinksList
	if sharedDrive != nil {
		links = &jsonapi.LinksList{
			Related: "/sharings/drives/" + sharedDrive.ID() + "/archive/" + secret + "/" + fakeName + ".zip",
		}
	} else {
		links = &jsonapi.LinksList{
			Related: "/files/archive/" + secret + "/" + fakeName + ".zip",
		}
	}

	return jsonapi.Data(c, http.StatusOK, &apiArchive{archive}, links)
}

// ArchiveDownloadCreateHandler handles requests to /files/archive and stores the
// paremeters with a secret to be used in download handler below.s
func ArchiveDownloadCreateHandler(c echo.Context) error {
	return ArchiveDownload(c, nil)
}

// FileDownload stores the required path into a secret
// usable for download handler below.
func FileDownload(c echo.Context, sharedDrive *sharing.Sharing) error {
	instance := middlewares.GetInstance(c)
	var doc *vfs.FileDoc
	var err error
	var path string
	var versionID string

	if path = c.QueryParam("Path"); path != "" {
		if doc, err = instance.VFS().FileByPath(path); err != nil {
			return WrapVfsError(err)
		}
	} else if id := c.QueryParam("Id"); id != "" {
		if doc, err = instance.VFS().FileByID(id); err != nil {
			return WrapVfsError(err)
		}
		if path, err = doc.Path(instance.VFS()); err != nil {
			return WrapVfsError(err)
		}
	} else if versionID = c.QueryParam("VersionId"); versionID != "" {
		docID := strings.Split(versionID, "/")[0]
		if doc, err = instance.VFS().FileByID(docID); err != nil {
			return WrapVfsError(err)
		}
	}

	err = checkPerm(c, "GET", nil, doc)
	if err != nil {
		return err
	}

	if err = CheckAntivirusAction(instance, doc, ActionShare); err != nil {
		return err
	}

	var secret string
	if versionID == "" {
		secret, err = vfs.GetStore().AddFile(instance, path)
	} else {
		secret, err = vfs.GetStore().AddVersion(instance, versionID)
		secret = "v-" + secret
	}
	if err != nil {
		return WrapVfsError(err)
	}

	filename := c.QueryParam("Filename")
	if filename == "" {
		filename = doc.DocName
	}
	var links *jsonapi.LinksList
	if sharedDrive != nil {
		links = &jsonapi.LinksList{
			Related: "/sharings/drives/" + sharedDrive.ID() + "/downloads/" + secret + "/" + filename,
		}
	} else {
		links = &jsonapi.LinksList{
			Related: "/files/downloads/" + secret + "/" + filename,
		}
	}

	return FileData(c, http.StatusOK, doc, false, links, sharedDrive)
}

func FileDownloadCreateHandler(c echo.Context) error {
	return FileDownload(c, nil)
}

// ArchiveDownloadHandler handles requests to /files/archive/:secret/whatever.zip
// and creates on the fly zip archive from the parameters linked to secret.
func ArchiveDownloadHandler(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	secret := c.Param("secret")
	archive, err := vfs.GetStore().GetArchive(instance, secret)
	if err != nil {
		return WrapVfsError(err)
	}

	// Check antivirus status for all files in the archive
	entries, err := archive.GetEntries(instance.VFS())
	if err != nil {
		return WrapVfsError(err)
	}
	for _, entry := range entries {
		if entry.File != nil {
			if err = CheckAntivirusAction(instance, entry.File, ActionDownload); err != nil {
				return err
			}
		}
	}

	if err := archive.Serve(instance.VFS(), c.Response()); err != nil {
		return WrapVfsError(err)
	}
	return nil
}

// FileDownloadHandler send a file that have previously be defined
// through FileDownloadCreateHandler
func FileDownloadHandler(c echo.Context) error {
	secret := c.Param("secret")
	if strings.HasPrefix(secret, "v-") {
		return versionDownloadHandler(c, strings.TrimPrefix(secret, "v-"))
	}
	instance := middlewares.GetInstance(c)
	path, err := vfs.GetStore().GetFile(instance, secret)
	if err != nil {
		return WrapVfsError(err)
	}
	return sendFileFromPath(c, path, false)
}

func versionDownloadHandler(c echo.Context, secret string) error {
	instance := middlewares.GetInstance(c)
	versionID, err := vfs.GetStore().GetVersion(instance, secret)
	if err != nil {
		return WrapVfsError(err)
	}

	fileID := strings.Split(versionID, "/")[0]
	doc, err := instance.VFS().FileByID(fileID)
	if err != nil {
		return WrapVfsError(err)
	}

	if err = CheckAntivirusAction(instance, doc, ActionDownload); err != nil {
		return err
	}

	version, err := vfs.FindVersion(instance, versionID)
	if err != nil {
		return WrapVfsError(err)
	}

	disposition := "inline"
	if c.QueryParam("Dl") == "1" {
		disposition = "attachment"
	} else {
		addCSPRuleForDirectLink(c, doc.Class, doc.Mime)
	}

	filename := c.Param("fake-name")
	err = vfs.ServeFileContent(instance.VFS(), doc, version, filename, disposition, c.Request(), c.Response())
	if err != nil {
		return WrapVfsError(err)
	}
	return nil
}

// Trash handles all DELETE requests on /files/:file-id and
// moves the file or directory with the specified file-id to the
// trash.
func Trash(c echo.Context, sharedDrive *sharing.Sharing) error {
	instance := middlewares.GetInstance(c)

	fileID := c.Param("file-id")
	dir, file, err := instance.VFS().DirOrFileByID(fileID)
	if err != nil {
		return WrapVfsError(err)
	}

	err = checkPerm(c, permission.PATCH, dir, file)
	if err != nil {
		return err
	}

	// Check antivirus status for files
	if file != nil {
		if err = CheckAntivirusAction(instance, file, ActionDelete); err != nil {
			return err
		}
	}

	var rev string
	if dir != nil {
		rev = dir.Rev()
	} else {
		rev = file.Rev()
	}

	if err := CheckIfMatch(c, rev); err != nil {
		return WrapVfsError(err)
	}

	ensureCleanOldTrashedTrigger(instance)

	if dir != nil {
		UpdateDirTrashCozyMetadata(c, dir)
		doc, errt := vfs.TrashDir(instance.VFS(), dir)
		if errt != nil {
			return WrapVfsError(errt)
		}
		return DirData(c, http.StatusOK, doc, sharedDrive)
	}

	UpdateFileTrashCozyMetadata(c, file)
	doc, errt := vfs.TrashFile(instance.VFS(), file)
	if errt != nil {
		return WrapVfsError(errt)
	}
	return FileData(c, http.StatusOK, doc, false, nil, sharedDrive)
}

func TrashHandler(c echo.Context) error {
	return Trash(c, nil)
}

// ReadTrashFilesHandler handle GET requests on /files/trash and return the
// list of trashed files and directories
func ReadTrashFilesHandler(c echo.Context) error {
	instance := middlewares.GetInstance(c)

	trash, err := instance.VFS().DirByID(consts.TrashDirID)
	if err != nil {
		return WrapVfsError(err)
	}

	err = checkPerm(c, permission.GET, trash, nil)
	if err != nil {
		return err
	}

	return dirDataList(c, http.StatusOK, trash)
}

// Restore handle POST requests on /files/trash/file-id and
// can be used to restore a file or directory from the trash.
//
// driveMemberAuthorized reports whether the request carries a shared-drive
// member token reaching this handler via a drive route: the route guard in
// web/sharings/drives.go has already authorized it, and the VFS permission
// check cannot succeed on a trashed file (no referenced_by, path in the
// trash), so it is skipped.
func driveMemberAuthorized(c echo.Context, sharedDrive *sharing.Sharing) bool {
	if sharedDrive == nil {
		return false
	}
	pdoc, err := middlewares.GetPermission(c)
	return err == nil && pdoc.Type == permission.TypeShareInteract
}

func Restore(c echo.Context, sharedDrive *sharing.Sharing) error {
	instance := middlewares.GetInstance(c)

	fileID := c.Param("file-id")

	dir, file, err := instance.VFS().DirOrFileByID(fileID)
	if err != nil {
		return WrapVfsError(err)
	}

	if !driveMemberAuthorized(c, sharedDrive) {
		err = checkPerm(c, permission.PATCH, dir, file)
		if err != nil {
			return err
		}
	}

	if dir != nil {
		UpdateDirCozyMetadata(c, dir)
		doc, errt := vfs.RestoreDir(instance.VFS(), dir)
		if errt != nil {
			return WrapVfsError(errt)
		}
		return DirData(c, http.StatusOK, doc, nil)
	}

	UpdateFileCozyMetadata(c, file, false)
	doc, errt := vfs.RestoreFile(instance.VFS(), file)
	if errt != nil {
		return WrapVfsError(errt)
	}
	return FileData(c, http.StatusOK, doc, false, nil, nil)
}

func RestoreTrashFileHandler(c echo.Context) error {
	return Restore(c, nil)
}

// ClearTrashHandler handles DELETE request to clear the trash
func ClearTrashHandler(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	fs := inst.VFS()
	trash, err := fs.DirByID(consts.TrashDirID)
	if err != nil {
		return WrapVfsError(err)
	}

	err = checkPerm(c, permission.DELETE, trash, nil)
	if err != nil {
		return err
	}

	files, _ := fs.FilesUsage()
	versions, _ := fs.VersionsUsage()
	quota := fs.DiskQuota()
	freeSpace := quota - files - versions
	inTrash, _ := fs.TrashUsage()

	err = fs.DestroyDirContent(trash, pushTrashJob(inst))
	if err != nil {
		return WrapVfsError(err)
	}

	// As a rule of thumb if the freed space (= inTrash) was more than the free
	// space, we want to ping other instances with common sharing to tell them
	// to try reuploading files that have may have been blocked because of the
	// quota.
	if inTrash > freeSpace {
		go func() {
			i := inst.Clone().(*instance.Instance)
			if err := sharing.AskReupload(i); err != nil {
				i.Logger().WithNamespace("files").
					Warnf("sharing.AskReupload failed with %s", err)
			}
		}()
	}

	return c.NoContent(204)
}

// DestroyFileHandler handles DELETE request to clear one element from the trash
func DestroyFileHandler(c echo.Context) error {
	return Destroy(c, nil)
}

func Destroy(c echo.Context, sharedDrive *sharing.Sharing) error {
	inst := middlewares.GetInstance(c)

	fileID := c.Param("file-id")

	dir, file, err := inst.VFS().DirOrFileByID(fileID)
	if err != nil {
		return WrapVfsError(err)
	}

	// Same skip as Restore: see driveMemberAuthorized.
	if !driveMemberAuthorized(c, sharedDrive) {
		err = checkPerm(c, permission.DELETE, dir, file)
		if err != nil {
			return err
		}
	}

	// Check antivirus status for files
	if file != nil {
		if err = CheckAntivirusAction(inst, file, ActionDelete); err != nil {
			return err
		}
	}

	var rev string
	if dir != nil {
		rev = dir.Rev()
	} else {
		rev = file.Rev()
	}

	if err = CheckIfMatch(c, rev); err != nil {
		return WrapVfsError(err)
	}

	if dir != nil {
		err = inst.VFS().DestroyDirAndContent(dir, pushTrashJob(inst))
	} else {
		err = inst.VFS().DestroyFile(file)
	}
	if err != nil {
		return WrapVfsError(err)
	}

	return c.NoContent(204)
}

// GetAllDocs is the handler for POST /files/_all_docs
func GetAllDocs(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	if err := middlewares.AllowWholeType(c, permission.GET, consts.Files); err != nil {
		return err
	}
	var allDocsReq struct {
		Keys []string `json:"keys"`
	}
	if err := json.NewDecoder(c.Request().Body).Decode(&allDocsReq); err != nil {
		return jsonapi.Errorf(http.StatusBadRequest, "%s", err)
	}

	req := &couchdb.AllDocsRequest{Keys: allDocsReq.Keys}
	var results []vfs.DirOrFileDoc
	if err := couchdb.GetAllDocs(inst, consts.Files, req, &results); err != nil {
		return err
	}

	out := make([]jsonapi.Object, 0)
	fp := vfs.NewFilePatherWithCache(inst.VFS())
	for _, result := range results {
		if result.DirDoc == nil {
			continue
		}
		if result.ID() == consts.TrashDirID {
			continue
		}

		d, f := result.Refine()
		if d != nil {
			out = append(out, NewDir(d, nil))
		} else {
			file := NewFile(f, inst, nil)
			file.IncludePath(fp)
			out = append(out, file)
		}
	}
	return jsonapi.DataList(c, http.StatusOK, out, nil)
}

// FindFilesMango is the route POST /files/_find
// used to retrieve files and their metadata from a mango query.
func FindFilesMango(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	var findRequest map[string]interface{}

	if err := json.NewDecoder(c.Request().Body).Decode(&findRequest); err != nil {
		return jsonapi.Errorf(http.StatusBadRequest, "%s", err)
	}

	if err := middlewares.AllowWholeType(c, permission.GET, consts.Files); err != nil {
		return err
	}

	includePath := true
	if reqFields, ok := findRequest["fields"].([]interface{}); ok {
		includePath = false
		// Those fields are necessary for the JSON-API response
		fields := []string{"_id", "_rev", "type", "class", "size", "trashed"}
		for _, v := range reqFields {
			v := v.(string)
			if v == "path" {
				// path is not stored in database, but added by the stack, and
				// it requires the dir_id
				includePath = true
				fields = append(fields, "dir_id")
			}
			fields = append(fields, v)
		}
		findRequest["fields"] = fields
	}

	limit, hasLimit := findRequest["limit"].(float64)
	if !hasLimit || limit > consts.MaxItemsPerPageForMango {
		limit = 100
	}
	if pageLimit := c.QueryParam("page[limit]"); pageLimit != "" {
		if limitInt, err := strconv.Atoi(pageLimit); err == nil {
			limit = float64(limitInt)
		}
	}
	findRequest["limit"] = limit

	skip := 0
	if skipF64, ok := findRequest["skip"].(float64); ok {
		skip = int(skipF64)
	}
	if pageSkip := c.QueryParam("page[skip]"); pageSkip != "" {
		if skipInt, err := strconv.Atoi(pageSkip); err == nil {
			findRequest["skip"] = skipInt
			skip = skipInt
		}
	}

	// XXX page[cursor] should be preferred to cursor, but we still accept
	// cursor to keep compatibility with the past
	if bookmark := c.QueryParam("cursor"); bookmark != "" {
		findRequest["bookmark"] = bookmark
	}
	if bookmark := c.QueryParam("page[cursor]"); bookmark != "" {
		findRequest["bookmark"] = bookmark
	}

	var results []vfs.DirOrFileDoc
	resp, err := couchdb.FindDocsRaw(instance, consts.Files, &findRequest, &results)
	if err != nil {
		return err
	}

	// XXX: in theory, we should avoid pagination link for POST requests, but
	// it is here and used, so let's keep it for compatibility.
	var links jsonapi.LinksList
	if resp.Bookmark != "" && len(results) >= int(limit) {
		links.Next = "/files/_find?page[cursor]=" + resp.Bookmark
	}

	var total int
	if len(results) >= int(limit) {
		total = math.MaxInt32 - 1 // we dont know the actual number
	} else {
		total = skip + len(results) // let the client know its done.
	}

	// Create secrets for thumbnail links in batch for performance reasons
	var thumbIDs []string
	for _, dof := range results {
		_, f := dof.Refine()
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

	fp := vfs.NewFilePatherWithCache(instance.VFS())
	out := make([]jsonapi.Object, len(results))
	fields, ok := findRequest["fields"].([]string)
	for i, dof := range results {
		d, f := dof.Refine()
		if d != nil {
			if ok {
				out[i] = newFindDir(d, fields)
			} else {
				out[i] = NewDir(d, nil)
			}
		} else {
			if ok {
				file := newFindFile(f, fields, instance)
				if includePath {
					file.IncludePath(fp)
				}
				if secret, ok := secrets[f.ID()]; ok {
					file.SetThumbSecret(secret)
				}
				out[i] = file
			} else {
				file := NewFile(f, instance, nil)
				if includePath {
					file.IncludePath(fp)
				}
				if secret, ok := secrets[f.ID()]; ok {
					file.SetThumbSecret(secret)
				}
				out[i] = file
			}
		}
	}

	meta := jsonapi.Meta{
		Count:          &total,
		ExecutionStats: resp.ExecutionStats,
		Warning:        resp.Warning,
	}
	return jsonapi.DataListWithMeta(c, http.StatusOK, meta, out, &links)
}

var allowedChangesParams = map[string]bool{
	// supported by CouchDB
	"since":        true,
	"limit":        true,
	"include_docs": true,

	// custom
	"fields":            false,
	"include_file_path": false,
	"skip_deleted":      false,
	"skip_trashed":      false,
}

// changesFeed is the shared implementation for GET /files/_changes and the
// shared-drive changes feeds.
func changesFeed(
	c echo.Context,
	inst *instance.Instance,
	sharedDir *vfs.DirDoc,
	sharedFile *vfs.FileDoc,
) error {
	switch {
	case sharedDir != nil:
		if err := middlewares.AllowVFS(c, permission.GET, sharedDir); err != nil {
			return err
		}
	case sharedFile != nil:
		if err := middlewares.AllowVFS(c, permission.GET, sharedFile); err != nil {
			return err
		}
	default:
		if err := middlewares.AllowWholeType(c, permission.GET, consts.Files); err != nil {
			return err
		}
	}

	// Drop a clear error for parameters not supported by stack
	filter := &changesFilter{}
	for key := range c.QueryParams() {
		if byCouch, ok := allowedChangesParams[key]; !ok {
			return jsonapi.Errorf(http.StatusBadRequest, "Unsupported query parameter '%s'", key)
		} else if !byCouch {
			filter.Add(key, c.QueryParam(key))
		}
	}

	limitString := c.QueryParam("limit")
	limit := 0
	if limitString != "" {
		var err error
		if limit, err = strconv.Atoi(limitString); err != nil {
			return jsonapi.Errorf(http.StatusBadRequest, "Invalid limit value '%s': %s", limitString, err.Error())
		}
		if limit > 10000 {
			limit = 10000
		}
	}

	includeDocs := c.QueryParam("include_docs") == "true"
	if !includeDocs && (filter.IncludePath || filter.SkipTrashed) {
		return jsonapi.Errorf(http.StatusBadRequest, "Invalid options: include_docs should be set to true")
	}

	if sharedDir != nil {
		filter.SharedDir = sharedDir
		// These are required to check if documents are in the shared directory
		filter.IncludePath = true
		includeDocs = true
	} else if sharedFile != nil {
		filter.SharedFile = sharedFile
		includeDocs = true
	}

	// Use the VFS lock for the files to avoid sending the changed feed while
	// the VFS is moving a directory.
	mu := config.Lock().ReadWrite(inst, "vfs")
	if err := mu.Lock(); err != nil {
		return err
	}

	couchReq := &couchdb.ChangesRequest{
		DocType:     consts.Files,
		Since:       c.QueryParam("since"),
		Limit:       limit,
		IncludeDocs: includeDocs,
		Filter:      "_selector",
	}
	results, err := couchdb.PostChanges(inst, couchReq, filter)
	mu.Unlock()
	if err != nil {
		return err
	}

	if client, ok := middlewares.GetOAuthClient(c); ok {
		err = vfs.FilterNotSynchronizedDocs(inst.VFS(), client.ID(), results)
		if err != nil {
			return err
		}
	}

	c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	c.Response().WriteHeader(http.StatusOK)
	if err := filter.Stream(c.Response(), inst, results); err != nil {
		inst.Logger().WithNamespace("files").Warnf("error on _changes: %s", err)
		return err
	}
	return nil
}

// ChangesFeed is the handler for GET /files/_changes, and indirectly, the
// directory-root shared-drive changes feed.
func ChangesFeed(c echo.Context, inst *instance.Instance, sharedDir *vfs.DirDoc) error {
	return changesFeed(c, inst, sharedDir, nil)
}

// ChangesFeedForSharedFile is the file-root shared-drive changes feed.
func ChangesFeedForSharedFile(
	c echo.Context,
	inst *instance.Instance,
	sharedFile *vfs.FileDoc,
) error {
	return changesFeed(c, inst, nil, sharedFile)
}

func ChangesFeedForFiles(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	return changesFeed(c, inst, nil, nil)
}

type changesFilter struct {
	Fields      []string
	IncludePath bool
	SkipDeleted bool
	SkipTrashed bool
	SharedDir   *vfs.DirDoc
	SharedFile  *vfs.FileDoc
	reader      io.Reader
}

func (filter *changesFilter) Add(key, value string) {
	switch key {
	case "fields":
		filter.Fields = strings.Split(value, ",")
	case "include_file_path":
		filter.IncludePath = true
	case "skip_deleted":
		filter.SkipDeleted = true
	case "skip_trashed":
		filter.SkipTrashed = true
	}
}

func (filter *changesFilter) Reject(change *couchdb.Change) *couchdb.Change {
	if !filter.SkipDeleted && !filter.SkipTrashed {
		return change
	}

	if filter.SkipDeleted && change.Deleted {
		return nil
	}
	if filter.SkipTrashed {
		if change.Doc.M["type"] == "file" && change.Doc.M["trashed"] == true {
			return nil
		}
		if change.Doc.M["type"] == "directory" {
			path, _ := change.Doc.M["path"].(string)
			if path == vfs.TrashDirName {
				return nil
			}
			if strings.HasPrefix(path, vfs.TrashDirName+"/") {
				return nil
			}
		}
	}
	return change
}

// SharedDriveChange transforms changes-feed results when the request comes
// from a shared drive.
func (filter *changesFilter) SharedDriveChange(change *couchdb.Change) *couchdb.Change {
	if filter.SharedDir == nil && filter.SharedFile == nil {
		// This is a request from GET /files/_changes: No need to map the changes
		return change
	}
	if filter.SharedFile != nil {
		if change.DocID != filter.SharedFile.DocID {
			return nil
		}
		if !change.Deleted {
			change.Doc.M["driveId"] = filter.SharedFile.SharingID()
		}
		return change
	}
	if change.Deleted {
		return change
	}

	if change.DocID == consts.RootDirID {
		return nil
	}

	path := change.Doc.M["path"].(string)
	sharedDirPath := utils.EnsureHasSuffix(filter.SharedDir.Fullpath, "/")
	if strings.HasPrefix(path, sharedDirPath) {
		change.Doc.M["driveId"] = filter.SharedDir.SharingID()
		return change
	}
	return couchdb.MakeChangeForDeletion(change.DocID, change.Doc.M["_rev"].(string), change.Seq)
}

// Stream writes the changes to the writer in JSON format, after hydrating.
func (filter *changesFilter) Stream(
	w io.Writer,
	inst *instance.Instance,
	results *couchdb.ChangesResponse,
) error {
	first := fmt.Sprintf(`{"last_seq": %q, "pending": %d, "results": [`, results.LastSeq, results.Pending)
	if _, err := w.Write([]byte(first)); err != nil {
		return err
	}
	alreadyWroteAnElement := false
	fp := vfs.NewFilePatherWithCache(inst.VFS())
	for _, result := range results.Results {
		if filter.IncludePath && result.Doc.M != nil && result.Doc.M["type"] == "file" {
			dirID, _ := result.Doc.M["dir_id"].(string)
			name, _ := result.Doc.M["name"].(string)
			doc := &vfs.FileDoc{DirID: dirID, DocName: name}
			if pth, err := fp.FilePath(doc); err == nil {
				result.Doc.M["path"] = pth
			}
		}
		resultToWrite := filter.SharedDriveChange(&result)
		if resultToWrite == nil {
			continue
		}
		resultToWrite = filter.Reject(resultToWrite)
		if resultToWrite == nil {
			continue
		}
		buf, err := json.Marshal(resultToWrite)
		if err != nil {
			return err
		}
		if alreadyWroteAnElement {
			if _, err := w.Write([]byte(",")); err != nil {
				return err
			}
		} else {
			alreadyWroteAnElement = true
		}
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}

	_, err := w.Write([]byte("]}"))
	return err
}

func (filter *changesFilter) Body() []byte {
	selector := map[string]interface{}{
		"_id": map[string]interface{}{
			"$not": map[string]interface{}{
				"$regex": "^_design/",
			},
		},
	}
	payload := map[string]interface{}{
		"selector": selector,
	}

	// Cf https://github.com/apache/couchdb/discussions/3774#discussioncomment-1416510
	if len(filter.Fields) > 0 {
		if filter.IncludePath || filter.SkipTrashed {
			for _, mandatory := range []string{"type", "name", "dir_id"} {
				found := false
				for _, f := range filter.Fields {
					if f == mandatory {
						found = true
					}
				}
				if !found {
					filter.Fields = append(filter.Fields, mandatory)
				}
			}
		}
		payload["fields"] = filter.Fields
	}

	body, _ := json.Marshal(payload)
	return body
}

func (filter *changesFilter) Read(p []byte) (int, error) {
	if filter.reader == nil {
		filter.reader = bytes.NewReader(filter.Body())
	}
	return filter.reader.Read(p)
}

func (filter *changesFilter) Close() error {
	filter.reader = nil
	return nil
}

func fsckHandler(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	cacheStorage := config.GetConfig().CacheStorage

	if err := middlewares.AllowWholeType(c, permission.GET, consts.Files); err != nil {
		return err
	}

	noCache, _ := strconv.ParseBool(c.QueryParam("NoCache"))
	key := "fsck:" + instance.DBPrefix()
	if !noCache {
		if r, ok := cacheStorage.GetCompressed(key); ok {
			return c.Stream(http.StatusOK, echo.MIMEApplicationJSON, r)
		}
	}

	logs := make([]*vfs.FsckLog, 0)
	err := instance.VFS().CheckFilesConsistency(func(log *vfs.FsckLog) {
		if log.Type == vfs.ContentMismatch {
			logs = append(logs, log)
		}
	}, false)
	if err != nil {
		return err
	}

	logsData, err := json.Marshal(logs)
	if err != nil {
		return err
	}

	if !noCache {
		expiration := utils.DurationFuzzing(3*30*24*time.Hour, 0.10)
		cacheStorage.SetCompressed(key, logsData, expiration)
	}

	return c.JSONBlob(http.StatusOK, logsData)
}

// Routes sets the routing for the files service
func Routes(router *echo.Group) {
	router.HEAD("/download", ReadFileContentFromPathHandler)
	router.GET("/download", ReadFileContentFromPathHandler)
	router.HEAD("/download/:file-id", ReadFileContentFromIDHandler)
	router.GET("/download/:file-id", ReadFileContentFromIDHandler)

	router.HEAD("/download/:file-id/:version-id", ReadFileContentFromVersion)
	router.GET("/download/:file-id/:version-id", ReadFileContentFromVersion)
	router.POST("/revert/:file-id/:version-id", RevertFileVersion)
	router.PATCH("/:file-id/:version-id", ModifyFileVersionMetadata)
	router.DELETE("/:file-id/:version-id", DeleteFileVersionMetadata)
	router.POST("/:file-id/versions", CopyVersionHandler)
	router.DELETE("/versions", ClearOldVersions)

	router.POST("/_all_docs", GetAllDocs)
	router.POST("/_find", FindFilesMango)
	router.GET("/_changes", ChangesFeedForFiles)

	router.HEAD("/:file-id", HeadDirOrFile)

	router.GET("/metadata", ReadMetadataFromPathHandler)
	router.GET("/:file-id", ReadMetadataFromIDHandler)
	router.GET("/:file-id/relationships/contents", GetChildrenHandler)
	router.GET("/:file-id/size", GetDirSize)

	router.PATCH("/metadata", ModifyMetadataByPathHandler)
	router.PATCH("/:file-id", ModifyMetadataByIDHandler)
	router.PATCH("/", ModifyMetadataByIDInBatchHandler)

	router.POST("/shared-drives", SharedDrivesCreationHandler)
	router.POST("/", CreationHandler)
	router.POST("/:file-id", CreationHandler)
	router.PUT("/:file-id", OverwriteFileContentHandler)
	router.POST("/upload/metadata", UploadMetadataHandler)
	router.POST("/:file-id/copy", FileCopyHandler)

	router.GET("/:file-id/thumbnails/:secret/:format", ThumbnailHandler)

	router.POST("/archive", ArchiveDownloadCreateHandler)
	router.GET("/archive/:secret/:fake-name", ArchiveDownloadHandler)

	router.POST("/downloads", FileDownloadCreateHandler)
	router.GET("/downloads/:secret/:fake-name", FileDownloadHandler)

	router.POST("/:file-id/relationships/referenced_by", AddReferencedHandler)
	router.DELETE("/:file-id/relationships/referenced_by", RemoveReferencedHandler)

	router.POST("/:file-id/relationships/not_synchronized_on", AddNotSynchronizedOn)
	router.DELETE("/:file-id/relationships/not_synchronized_on", RemoveNotSynchronizedOn)

	router.GET("/trash", ReadTrashFilesHandler)
	router.DELETE("/trash", ClearTrashHandler)

	router.POST("/trash/:file-id", RestoreTrashFileHandler)
	router.DELETE("/trash/:file-id", DestroyFileHandler)

	router.DELETE("/:file-id", TrashHandler)
	router.GET("/fsck", fsckHandler)
}

// WrapVfsError returns a formatted error from a golang error emitted by the vfs
func WrapVfsError(err error) error {
	if errj := wrapVfsError(err); errj != nil {
		return errj
	}
	return err
}

func wrapVfsErrorJSONAPI(err error) *jsonapi.Error {
	if errj, ok := err.(*jsonapi.Error); ok {
		return errj
	}
	if he, ok := err.(*echo.HTTPError); ok {
		return jsonapi.Errorf(he.Code, "%v", he.Message)
	}
	if errj := wrapVfsError(err); errj != nil {
		return errj
	}
	return jsonapi.InternalServerError(err)
}

func wrapVfsError(err error) *jsonapi.Error {
	switch err {
	case ErrDocTypeInvalid:
		return jsonapi.InvalidAttribute("type", err)
	case os.ErrExist:
		return jsonapi.Conflict(err)
	case os.ErrNotExist, swift.ObjectNotFound:
		return jsonapi.NotFound(err)
	case vfs.ErrParentDoesNotExist:
		return jsonapi.NotFound(err)
	case vfs.ErrParentInTrash:
		return jsonapi.NotFound(err)
	case vfs.ErrForbiddenDocMove:
		return jsonapi.PreconditionFailed("dir-id", err)
	case vfs.ErrIllegalFilename:
		return jsonapi.InvalidParameter("name", err)
	case vfs.ErrIllegalPath:
		return jsonapi.InvalidParameter("path", err)
	case vfs.ErrIllegalMime:
		return jsonapi.InvalidParameter("mime", err)
	case vfs.ErrIllegalTime:
		return jsonapi.InvalidParameter("UpdatedAt", err)
	case vfs.ErrInvalidHash:
		return jsonapi.PreconditionFailed("Content-MD5", err)
	case vfs.ErrContentLengthMismatch:
		return jsonapi.PreconditionFailed("Content-Length", err)
	case vfs.ErrConflict:
		return jsonapi.Conflict(err)
	case vfs.ErrFileInTrash, vfs.ErrNonAbsolutePath,
		vfs.ErrDirNotEmpty:
		return jsonapi.BadRequest(err)
	case vfs.ErrFileTooBig, vfs.ErrMaxFileSize:
		return jsonapi.Errorf(http.StatusRequestEntityTooLarge, "%s", err)
	case vfs.ErrWrongToken:
		return jsonapi.BadRequest(err)
	case vfs.ErrInvalidMetadataID:
		return jsonapi.InvalidParameter("MetadataID", err)
	}
	if _, ok := err.(*jsonapi.Error); !ok {
		logger.WithNamespace("files").Warnf("Not wrapped error: %s", err)
	}
	return nil
}

// FileDocFromReq creates a FileDoc from an incoming request.
func FileDocFromReq(c echo.Context, name, dirID string) (*vfs.FileDoc, error) {
	header := c.Request().Header
	size := c.Request().ContentLength
	if size == -1 {
		if param := c.QueryParam("Size"); param != "" {
			if s, err := strconv.ParseInt(param, 10, 64); err == nil {
				size = s
			}
		}
	}

	var err error
	var md5Sum []byte
	if md5Str := header.Get("Content-MD5"); md5Str != "" {
		md5Sum, err = parseMD5Hash(md5Str)
	}
	if err != nil {
		err = jsonapi.InvalidParameter("Content-MD5", err)
		return nil, err
	}

	cdate := time.Now()
	if date := header.Get("Date"); date != "" {
		if t, err := time.Parse(time.RFC1123, date); err == nil {
			cdate = t
		}
	}

	var mime, class string
	contentType := header.Get(echo.HeaderContentType)
	if contentType == "" || contentType == echo.MIMEOctetStream {
		mime, class = vfs.ExtractMimeAndClassFromFilename(name)
	} else {
		ext := strings.ToLower(path.Ext(name))
		// Force the mime-type for .url files
		if ext == ".url" {
			contentType = consts.ShortcutMimeType
		}
		if contentType == "text/xml" && ext == "svg" {
			contentType = "image/svg+xml"
		}
		mime, class = vfs.ExtractMimeAndClass(contentType)
	}

	tags := strings.Split(c.QueryParam("Tags"), TagSeparator)
	executable := c.QueryParam("Executable") == "true"
	encrypted := c.QueryParam("Encrypted") == "true"
	trashed := false
	doc, err := vfs.NewFileDoc(
		name,
		dirID,
		size,
		md5Sum,
		mime,
		class,
		cdate,
		executable,
		trashed,
		encrypted,
		tags,
	)
	if err != nil {
		return nil, err
	}

	// This way to send metadata is deprecated, but is still here to ensure
	// compatibility with existing clients.
	if meta := c.QueryParam("Metadata"); meta != "" {
		if err := json.Unmarshal([]byte(meta), &doc.Metadata); err != nil {
			return nil, err
		}
	}

	if secret := c.QueryParam("MetadataID"); secret != "" {
		instance := middlewares.GetInstance(c)
		meta, err := vfs.GetStore().GetMetadata(instance, secret)
		if err != nil {
			return nil, err
		}
		doc.Metadata = *meta
	}

	if len(doc.Metadata) > 0 {
		if _, ok := doc.Metadata[consts.CarbonCopyKey]; ok {
			if err := middlewares.AllowWholeType(c, permission.POST, consts.CertifiedCarbonCopy); err != nil {
				delete(doc.Metadata, consts.CarbonCopyKey)
			}
		}
		if _, ok := doc.Metadata[consts.ElectronicSafeKey]; ok {
			if err := middlewares.AllowWholeType(c, permission.POST, consts.CertifiedElectronicSafe); err != nil {
				delete(doc.Metadata, consts.ElectronicSafeKey)
			}
		}
	}

	return doc, nil
}

// CheckIfMatch checks if the revision provided matches the revision number
// given in the request, in the header and/or the query.
func CheckIfMatch(c echo.Context, rev string) error {
	ifMatch := c.Request().Header.Get("If-Match")
	revQuery := c.QueryParam("rev")
	var wantedRev string
	if ifMatch != "" {
		wantedRev = ifMatch
	}
	if revQuery != "" && wantedRev == "" {
		wantedRev = revQuery
	}
	return checkIfMatch(rev, wantedRev)
}

func checkIfMatch(rev, wantedRev string) error {
	if wantedRev != "" && rev != wantedRev {
		return jsonapi.PreconditionFailed("If-Match", fmt.Errorf("Revision does not match"))
	}
	return nil
}

func checkPerm(c echo.Context, v permission.Verb, d *vfs.DirDoc, f *vfs.FileDoc) error {
	if d != nil {
		return middlewares.AllowVFS(c, v, d)
	}
	return middlewares.AllowVFS(c, v, f)
}

// checkMoveDestination enforces the extra authorization required when a
// PATCH behaves like a move (changes the parent directory): the destination
// must exist, the caller needs POST on it, and moves touching a shared drive
// are rejected here.
//
// ponytail: strict rejection — shared-drive moves must go through
// POST /sharings/drives/move; lift when the shared-aware move task
// implements delegation.
func checkMoveDestination(c echo.Context, fs vfs.VFS, patch *docPatch, dir *vfs.DirDoc, file *vfs.FileDoc) error {
	if patch.DirID == nil || patch.Trash || patch.Delete {
		return nil
	}
	var currentDirID string
	if dir != nil {
		currentDirID = dir.DirID
	} else {
		currentDirID = file.DirID
	}
	if *patch.DirID == currentDirID {
		return nil // dir_id set to its current value: not a move
	}
	newParent, _, err := fs.DirOrFileByID(*patch.DirID)
	if err != nil {
		return err
	}
	if newParent == nil {
		return jsonapi.BadRequest(errors.New("destination is not a directory"))
	}
	if err := middlewares.AllowVFS(c, permission.POST, newParent); err != nil {
		return err
	}
	resolver := sharing.NewAccessResolver(middlewares.GetInstance(c))
	if err := checkSharedDrive(resolver, dir, file); err != nil {
		return err
	}
	return checkSharedDrive(resolver, newParent, nil)
}

// checkSharedDrive rejects the move if the doc (dir or file) belongs to a
// shared drive.
func checkSharedDrive(r *sharing.AccessResolver, d *vfs.DirDoc, f *vfs.FileDoc) error {
	shared, err := r.HasDriveSharing(d, f)
	if err != nil {
		return err
	}
	if shared {
		return jsonapi.NewError(http.StatusUnprocessableEntity,
			"moving to or from a shared drive must use POST /sharings/drives/move")
	}
	return nil
}

func parseMD5Hash(md5B64 string) ([]byte, error) {
	// Encoded md5 hash in base64 should at least have 22 caracters in
	// base64: 16*3/4 = 21+1/3
	//
	// The padding may add up to 2 characters (non useful). If we are
	// out of these boundaries we know we don't have a good hash and we
	// can bail immediately.
	if len(md5B64) < 22 || len(md5B64) > 24 {
		return nil, fmt.Errorf("Given Content-MD5 is invalid")
	}

	md5Sum, err := base64.StdEncoding.DecodeString(md5B64)
	if err != nil || len(md5Sum) != 16 {
		return nil, fmt.Errorf("Given Content-MD5 is invalid")
	}

	return md5Sum, nil
}

func pushTrashJob(inst *instance.Instance) func(vfs.TrashJournal) error {
	return func(journal vfs.TrashJournal) error {
		msg, err := job.NewMessage(journal)
		if err != nil {
			return err
		}
		_, err = job.System().PushJob(inst, &job.JobRequest{
			WorkerType: "trash-files",
			Message:    msg,
		})
		return err
	}
}

func ensureCleanOldTrashedTrigger(inst *instance.Instance) {
	// 1. Check if we need a trigger for clean-old-trashed worker
	cfg := config.GetConfig().Fs.AutoCleanTrashedAfter
	after, ok := cfg[inst.ContextName]
	if !ok || after == "" {
		return
	}

	// 2. Check if the trigger already exists
	sched := job.System()
	infos := job.TriggerInfos{
		Type:       "@cron",
		WorkerType: "clean-old-trashed",
	}
	if sched.HasTrigger(inst, infos) {
		return
	}

	// 3. Create the trigger
	now := time.Now()
	hours := (now.Hour() + 12) % 24
	infos.Arguments = fmt.Sprintf("0 %d %d * * *", now.Minute(), hours)
	trigger, err := job.NewTrigger(inst, infos, nil)
	if err != nil {
		inst.Logger().Errorf("Cannot create clean-old-trashed trigger: %s", err)
		return
	}
	if err = sched.AddTrigger(trigger); err != nil {
		inst.Logger().Errorf("Cannot create clean-old-trashed trigger: %s", err)
	}
}

func instanceURL(c echo.Context) string {
	return middlewares.GetInstance(c).PageURL("/", nil)
}

func UpdateDirCozyMetadata(c echo.Context, dir *vfs.DirDoc) {
	fcm, _ := CozyMetadataFromClaims(c, false)
	if dir.CozyMetadata == nil {
		fcm.CreatedAt = dir.CreatedAt
		fcm.CreatedByApp = ""
		fcm.CreatedByAppVersion = ""
		dir.CozyMetadata = fcm
	} else {
		dir.CozyMetadata.UpdatedAt = fcm.UpdatedAt
		if len(fcm.UpdatedByApps) > 0 {
			dir.CozyMetadata.UpdatedByApp(fcm.UpdatedByApps[0])
		}
	}
}

func UpdateFileCozyMetadata(c echo.Context, file *vfs.FileDoc, setUploadFields bool) {
	var oldSourceAccount, oldSourceIdentifier string
	fcm, slug := CozyMetadataFromClaims(c, setUploadFields)
	if file.CozyMetadata == nil {
		fcm.CreatedAt = file.CreatedAt
		fcm.CreatedByApp = ""
		fcm.CreatedByAppVersion = ""
		uploadedAt := file.CreatedAt
		fcm.UploadedAt = &uploadedAt
		file.CozyMetadata = fcm
	} else {
		oldSourceAccount = file.CozyMetadata.SourceAccount
		oldSourceIdentifier = file.CozyMetadata.SourceIdentifier
		file.CozyMetadata.UpdatedAt = fcm.UpdatedAt
		if len(fcm.UpdatedByApps) > 0 {
			file.CozyMetadata.UpdatedByApp(fcm.UpdatedByApps[0])
		}
		if setUploadFields {
			file.CozyMetadata.UploadedAt = fcm.UploadedAt
			file.CozyMetadata.UploadedBy = fcm.UploadedBy
			file.CozyMetadata.UploadedOn = fcm.UploadedOn
		}
	}

	if setUploadFields {
		if oldSourceAccount == "" && fcm.SourceAccount != "" {
			file.CozyMetadata.SourceAccount = fcm.SourceAccount
			// To ease the transition to cozyMetadata for io.cozy.files, we fill
			// the CreatedByApp for konnectors that updates a file: the stack can
			// recognize that by the presence of the SourceAccount.
			if file.CozyMetadata.CreatedByApp == "" && slug != "" {
				file.CozyMetadata.CreatedByApp = slug
			}
		}
		if oldSourceIdentifier == "" && fcm.SourceIdentifier != "" {
			file.CozyMetadata.SourceIdentifier = fcm.SourceIdentifier
		}
	}
}

func UpdateDirTrashCozyMetadata(c echo.Context, dir *vfs.DirDoc) {
	UpdateDirCozyMetadata(c, dir)
	setTrashCozyMetadata(c, dir.CozyMetadata)
}

func UpdateFileTrashCozyMetadata(c echo.Context, file *vfs.FileDoc) {
	UpdateFileCozyMetadata(c, file, false)
	setTrashCozyMetadata(c, file.CozyMetadata)
}

func setTrashCozyMetadata(c echo.Context, fcm *vfs.FilesCozyMetadata) {
	if fcm == nil {
		return
	}
	trashedAt := fcm.UpdatedAt
	fcm.TrashedAt = &trashedAt
	if actor, ok := middlewares.GetActor(c); ok {
		fcm.TrashedBy = &vfs.TrashedByEntry{
			Kind:        actor.Kind,
			DisplayName: actor.DisplayName,
			Domain:      actor.Domain,
		}
		return
	}
	fcm.TrashedBy = nil
}

// CozyMetadataFromClaims returns a FilesCozyMetadata struct, with the app
// fields filled with information from the permission claims.
func CozyMetadataFromClaims(c echo.Context, setUploadFields bool) (*vfs.FilesCozyMetadata, string) {
	fcm := vfs.NewCozyMetadata(instanceURL(c))

	var slug, version string
	var client map[string]string
	if claims := c.Get("claims"); claims != nil {
		cl := claims.(permission.Claims)
		switch cl.AudienceString() {
		case consts.AppAudience, consts.KonnectorAudience:
			slug = cl.Subject
		case consts.AccessTokenAudience:
			if perms, err := middlewares.GetPermission(c); err == nil {
				if cli, ok := perms.Client.(*oauth.Client); ok {
					slug = oauth.GetLinkedAppSlug(cli.SoftwareID)
					// Special case for cozy-desktop: it is an OAuth app not linked
					// to a web app, so it has no slug, but we still want to keep
					// in cozyMetadata its changes, so we use a fake slug.
					if slug == "" && strings.Contains(cli.SoftwareID, "cozy-desktop") {
						slug = "cozy-desktop"
					}
					// Special case for the flagship app
					if slug == "" && cli.Flagship {
						slug = "cozy-flagship"
					}
					version = cli.SoftwareVersion
					client = map[string]string{
						"id":   cli.ID(),
						"kind": cli.ClientKind,
						"name": cli.ClientName,
					}
				}
			}
		}
	}

	if slug != "" {
		fcm.CreatedByApp = slug
		fcm.CreatedByAppVersion = version
		fcm.UpdatedByApps = []*metadata.UpdatedByAppEntry{
			{
				Slug:     slug,
				Version:  version,
				Date:     fcm.UpdatedAt,
				Instance: fcm.CreatedOn,
			},
		}
	}

	if setUploadFields {
		uploadedAt := fcm.CreatedAt
		fcm.UploadedAt = &uploadedAt
		fcm.UploadedOn = fcm.CreatedOn
		if slug != "" {
			fcm.UploadedBy = &vfs.UploadedByEntry{
				Slug:    slug,
				Version: version,
				Client:  client,
			}
		}
	}

	if account := c.QueryParam("SourceAccount"); account != "" {
		fcm.SourceAccount = account
	}
	if id := c.QueryParam("SourceAccountIdentifier"); id != "" {
		fcm.SourceIdentifier = id
	}

	return fcm, slug
}

func fileCopyName(inst *instance.Instance, name string) string {
	base, ext := name, ""
	ext = filepath.Ext(name)
	base = strings.TrimSuffix(base, ext)
	suffix := inst.Translate("File copy Suffix")

	return fmt.Sprintf("%s (%s)%s", base, suffix, ext)
}
