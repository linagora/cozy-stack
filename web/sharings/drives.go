package sharings

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/web/editor"
	"github.com/cozy/cozy-stack/web/files"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/cozy/cozy-stack/web/notes"
	"github.com/cozy/cozy-stack/web/office"
	webperm "github.com/cozy/cozy-stack/web/permissions"
	"github.com/cozy/cozy-stack/web/shortcuts"
	"github.com/labstack/echo/v4"
)

type docPatch struct {
	docID string

	vfs.DocPatch
	Delete bool `json:"permanent_delete,omitempty"`
}

// ListSharedDrives returns the list of the shared drives.
func ListSharedDrives(c echo.Context) error {
	if err := middlewares.AllowWholeType(c, permission.GET, consts.Files); err != nil {
		return wrapErrors(err)
	}

	inst := middlewares.GetInstance(c)
	drives, err := sharing.ListDrives(inst)
	if err != nil {
		return wrapErrors(err)
	}

	objs := make([]jsonapi.Object, 0, len(drives))
	for _, drive := range drives {
		obj := &sharing.APISharing{
			Sharing:     drive,
			Credentials: nil,
		}
		objs = append(objs, obj)
	}
	return jsonapi.DataList(c, http.StatusOK, objs, nil)
}

// CreateSharedDrive creates a new shared drive from an existing folder or
// creates a new folder for it at the instance root.
// POST /sharings/drives
func CreateSharedDrive(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	var attrs struct {
		Description string `json:"description"`
		FolderID    string `json:"folder_id"`
		FileID      string `json:"file_id"`
		Name        string `json:"name"`
	}
	obj, err := jsonapi.Bind(c.Request().Body, &attrs)
	if err != nil {
		return jsonapi.BadJSON()
	}

	if attrs.FolderID == "" && attrs.FileID == "" && attrs.Name == "" {
		return jsonapi.BadRequest(errors.New("one of folder_id, file_id or name is required"))
	}
	if (attrs.FolderID != "" && attrs.FileID != "") ||
		(attrs.FolderID != "" && attrs.Name != "") ||
		(attrs.FileID != "" && attrs.Name != "") {
		return jsonapi.BadRequest(errors.New("folder_id, file_id and name are mutually exclusive"))
	}

	if attrs.Name != "" {
		newDir, err := vfs.Mkdir(inst.VFS(), path.Join("/", attrs.Name), nil)
		if err != nil {
			return wrapDriveNameErrors(err)
		}
		attrs.FolderID = newDir.DocID
	}

	rootID := attrs.FileID
	if rootID == "" {
		rootID = attrs.FolderID
	}
	newSharing, err := sharing.CreateDrive(inst, rootID, attrs.Description, "")
	if err != nil {
		return wrapDriveRootErrors(err)
	}

	// Check permissions using the existing function (validates against the rules)
	slug, err := checkCreatePermissions(c, newSharing)
	if err != nil {
		return echo.NewHTTPError(http.StatusForbidden)
	}

	// Set the app slug if obtained from permissions
	if slug != "" && newSharing.AppSlug == "" {
		newSharing.AppSlug = slug
	}
	newSharing.OrgDrive = inst.IsOrganizationInstance()

	// Extract recipient IDs from relationships
	rwGroupIDs, rwContactIDs := extractRecipientIDs(obj, "recipients")
	roGroupIDs, roContactIDs := extractRecipientIDs(obj, "read_only_recipients")

	// Create the sharing document first (drives can be created without recipients)
	if _, err = newSharing.Create(inst); err != nil {
		return wrapErrors(err)
	}

	// Add read-write recipients and send invitations
	if len(rwGroupIDs) > 0 || len(rwContactIDs) > 0 {
		if err = newSharing.AddGroupsAndContacts(inst, rwGroupIDs, rwContactIDs, false); err != nil {
			return wrapErrors(err)
		}
	}

	// Add read-only recipients and send invitations
	if len(roGroupIDs) > 0 || len(roContactIDs) > 0 {
		if err = newSharing.AddGroupsAndContacts(inst, roGroupIDs, roContactIDs, true); err != nil {
			return wrapErrors(err)
		}
	}

	as := &sharing.APISharing{
		Sharing:     newSharing,
		Credentials: nil,
		SharedDocs:  nil,
	}
	return jsonapi.Data(c, http.StatusCreated, as, nil)
}

func wrapDriveNameErrors(err error) error {
	switch err {
	case os.ErrExist:
		return jsonapi.Conflict(err)
	case vfs.ErrIllegalFilename, vfs.ErrIllegalPath:
		return jsonapi.InvalidParameter("name", err)
	default:
		return wrapErrors(err)
	}
}

func wrapDriveRootErrors(err error) error {
	switch err {
	case sharing.ErrDriveRootNotFound:
		return jsonapi.NotFound(err)
	case sharing.ErrFolderAlreadyShared, sharing.ErrFileAlreadyShared:
		return jsonapi.Conflict(err)
	case sharing.ErrParentReadOnly:
		return jsonapi.Forbidden(err)
	case sharing.ErrSystemFolder, sharing.ErrFileInTrash:
		return jsonapi.BadRequest(err)
	default:
		return wrapErrors(err)
	}
}

// extractRecipientIDs extracts group and contact IDs from a JSON:API relationship.
func extractRecipientIDs(obj *jsonapi.ObjectMarshalling, relationshipName string) (groupIDs, contactIDs []string) {
	rel, ok := obj.GetRelationship(relationshipName)
	if !ok {
		return nil, nil
	}
	data, ok := rel.Data.([]interface{})
	if !ok {
		return nil, nil
	}
	for _, ref := range data {
		refMap, ok := ref.(map[string]interface{})
		if !ok {
			continue
		}
		id, ok := refMap["id"].(string)
		if !ok {
			continue
		}
		if t, _ := refMap["type"].(string); t == consts.Groups {
			groupIDs = append(groupIDs, id)
		} else {
			contactIDs = append(contactIDs, id)
		}
	}
	return groupIDs, contactIDs
}

// Load either a DirDoc or a FileDoc from the given `file-id` param. The
// function also checks permissions.
func loadDirOrFileFromParam(c echo.Context, inst *instance.Instance, perm permission.Verb) (*vfs.DirDoc, *vfs.FileDoc, error) {
	dir, file, err := inst.VFS().DirOrFileByID(c.Param("file-id"))
	if err != nil {
		return nil, nil, files.WrapVfsError(err)
	}

	if dir != nil {
		err = middlewares.AllowVFS(c, perm, dir)
	} else {
		err = middlewares.AllowVFS(c, perm, file)
	}
	if err != nil {
		return nil, nil, files.WrapVfsError(err)
	}

	if dir != nil {
		return dir, nil, nil
	}
	return nil, file, nil
}

// Same as `loadDirOrFile` but intolerant of files, responds 422s
func loadDirFromParam(c echo.Context, inst *instance.Instance) (*vfs.DirDoc, error) {
	dir, file, err := loadDirOrFileFromParam(c, inst, permission.GET)
	if file != nil {
		return nil, jsonapi.InvalidParameter("file-id", errors.New("file-id: not a directory"))
	}
	return dir, err
}

// Same as `loadDirOrFile` but intolerant of directories, responds 422s
func loadFileFromParam(c echo.Context, inst *instance.Instance, perm permission.Verb) (*vfs.FileDoc, error) {
	dir, file, err := loadDirOrFileFromParam(c, inst, perm)
	if dir != nil {
		return nil, jsonapi.InvalidParameter("file-id", errors.New("file-id: not a file"))
	}
	return file, err
}

// HeadDirOrFile returns an error if the requested file or directory does not
// exist. It returns an empty body otherwise.
func HeadDirOrFile(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	_, _, err := loadDirOrFileFromParam(c, inst, permission.GET)
	if err != nil {
		return err
	}
	return nil
}

// authorizeDriveTarget resolves the effective access of the given drive
// member on the target; 404 hides targets outside every sharing scope, to
// avoid revealing which paths exist on the instance.
func authorizeDriveTarget(inst *instance.Instance, member *sharing.Member, targetID string) (*sharing.EffectiveAccess, error) {
	ea, err := sharing.NewAccessResolver(inst).ResolveForMember(targetID, member)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, jsonapi.NotFound(errors.New("shared drive target not found"))
		}
		return nil, wrapErrors(err)
	}
	if !ea.CanRead {
		return nil, jsonapi.NotFound(errors.New("shared drive target not found"))
	}
	return ea, nil
}

// checkDriveMemberRead asserts the calling drive member (if any) has
// effective read access on the target. No-op for non-member tokens (owner,
// share-by-link).
func checkDriveMemberRead(c echo.Context, inst *instance.Instance, targetID string) error {
	member := GetSharedDriveMember(c)
	if member == nil {
		return nil
	}
	_, err := authorizeDriveTarget(inst, member, targetID)
	return err
}

// driveOpenReadOnly says whether an open route (note, office, editor) must be
// forced read-only for the calling drive member: the interact token carries
// ALL verbs, so the member's read-only restriction is enforced at the
// application layer. It also requires effective read on the target. Returns
// false for non-member tokens (owner, share-by-link): the VFS check in the
// open handlers governs them.
func driveOpenReadOnly(c echo.Context, inst *instance.Instance, fileID string) (bool, error) {
	member := GetSharedDriveMember(c)
	if member == nil {
		return false, nil
	}
	ea, err := authorizeDriveTarget(inst, member, fileID)
	if err != nil {
		return false, err
	}
	return !ea.Can(permission.PUT), nil
}

// ReadMetadataFromPath allows to get file/dir information for a path.
func ReadMetadataFromPath(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	// Drive tokens are checked against the member's effective read access on
	// the resolved target; other tokens keep the VFS permission check done
	// by files.ReadMetadataFromPath. A target outside every sharing scope is
	// answered 404 to avoid revealing which paths exist on the instance.
	if GetSharedDriveMember(c) != nil && c.QueryParam("Path") != "" {
		dir, file, err := inst.VFS().DirOrFileByPath(c.QueryParam("Path"))
		if err != nil {
			return jsonapi.NotFound(errors.New("shared drive target not found"))
		}
		var targetID string
		if dir != nil {
			targetID = dir.DocID
		} else {
			targetID = file.DocID
		}
		if err := checkDriveMemberRead(c, inst, targetID); err != nil {
			return err
		}
	}
	return files.ReadMetadataFromPath(c, s)
}

// GetDirOrFileData handles all GET requests on aiming at getting a file or
// directory metadata from its id.
// TODO: reuse files.ReadMetadataFromIDHandler?
func GetDirOrFileData(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	dir, file, err := loadDirOrFileFromParam(c, inst, permission.GET)
	if err != nil {
		return err
	}
	if dir != nil {
		return files.DirData(c, http.StatusOK, dir, s)
	}
	return files.FileData(c, http.StatusOK, file, true, nil, s)
}

// ReadFileContentFromIDHandler handles all GET requests aiming at downloading
// a file given its ID. It serves the file in inline mode.
// TODO: reuse files.ReadMetadataFromIDHandler?
func ReadFileContentFromIDHandler(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	file, err := loadFileFromParam(c, inst, permission.GET)
	if err != nil {
		return err
	}
	return files.SendFileFromDoc(inst, c, file, false)
}

// ReadFileContentFromVersion handles the download of an old version of the
// file content.
func ReadFileContentFromVersion(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	return files.ReadFileContentFromVersion(c)
}

// CopyVersionHandler handles POST requests on /sharings/drives/:id/:file-id/versions.
//
// It can be used to create a new version of a file, with the same content but
// new metadata.
func CopyVersionHandler(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	return files.CopyVersionHandler(c)
}

// DeleteFileVersionMetadata handles DELETE requests on
// /sharings/drives/:id/:file-id/:version-id.
//
// It can be used to delete an old version of a file.
func DeleteFileVersionMetadata(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	return files.DeleteFileVersionMetadata(c)
}

// GetDirSize returns the size of a directory (the sum of the size of the files
// in this directory, including those in subdirectories).
func GetDirSize(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	if err := ensureDirectoryBackedSharedDrive(s); err != nil {
		return err
	}
	fs := inst.VFS()
	dir, err := loadDirFromParam(c, inst)
	if err != nil {
		return err
	}

	size, err := fs.DirSize(dir)
	if err != nil {
		return files.WrapVfsError(err)
	}

	result := files.ApiDiskSize{DocID: dir.DocID, Size: size}
	return jsonapi.Data(c, http.StatusOK, &result, nil)
}

// ModifyMetadataByIDHandler handles PATCH requests used to modify the file or
// directory metadata, as well as moving and renaming it in the shared drive's filesystem.
func ModifyMetadataByIDHandler(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	patch, err := getPatch(c, c.Param("file-id"))
	if err != nil {
		return files.WrapVfsError(err)
	}
	if patch.Delete {
		if rootID, err := s.DriveRootID(); err == nil && c.Param("file-id") == rootID {
			if member := GetSharedDriveMember(c); member != nil {
				return jsonapi.Forbidden(errors.New("only the owner can destroy the root of a shared drive"))
			}
		}
	}
	if patch.DirID != nil {
		rootID, err := s.DriveRootID()
		if err == nil && c.Param("file-id") == rootID {
			return jsonapi.NewError(http.StatusUnprocessableEntity, "cannot move the root of a shared drive")
		}
		// ponytail: destination check lives in the handler, not in
		// guardSharedDriveRouteForMember, because the guard cannot read the
		// request body without consuming it.
		if member := GetSharedDriveMember(c); member != nil {
			if err := checkEffectiveAccessForMember(inst, *patch.DirID, permission.POST, member); err != nil {
				return err
			}
		}
	}
	if err = applyPatch(c, inst.VFS(), patch); err != nil {
		return files.WrapVfsError(err)
	}
	return nil
}

func getPatch(c echo.Context, docID string) (*docPatch, error) {
	var patch docPatch
	obj, err := jsonapi.Bind(c.Request().Body, &patch)
	if err != nil {
		return nil, jsonapi.BadJSON()
	}
	patch.docID = docID
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

func applyPatch(c echo.Context, fs vfs.VFS, patch *docPatch) (err error) {
	dir, file, err := fs.DirOrFileByID(patch.docID)
	if err != nil {
		return err
	}

	var rev string
	if dir != nil {
		rev = dir.Rev()
	} else {
		rev = file.Rev()
	}

	if err = files.CheckIfMatch(c, rev); err != nil {
		return err
	}

	if dir != nil {
		if err = middlewares.AllowVFS(c, permission.PATCH, dir); err != nil {
			return err
		}
	} else {
		if err = middlewares.AllowVFS(c, permission.PATCH, file); err != nil {
			return err
		}
	}

	if patch.Delete {
		if dir != nil {
			return fs.DestroyDirAndContent(dir, fs.EnsureErased)
		}
		return fs.DestroyFile(file)
	}

	if patch.DirID != nil {
		newParent, _, err := fs.DirOrFileByID(*patch.DirID)
		if err != nil {
			return err
		}
		if newParent == nil {
			return jsonapi.BadRequest(errors.New("destination directory does not exist"))
		}
		// XXX: This permission check ensures the new parent is in the shared drive.
		if err = middlewares.AllowVFS(c, permission.POST, newParent); err != nil {
			return err
		}
	}

	if dir != nil {
		oldDirName := dir.DocName
		files.UpdateDirCozyMetadata(c, dir)
		dir, err = vfs.ModifyDirMetadata(fs, dir, &patch.DocPatch)
		if err != nil {
			return err
		}
		if patch.Name != nil && oldDirName != *patch.Name {
			// Update sharing description if this directory is a sharing root
			sharing.UpdateSharingDescriptionIfNeeded(middlewares.GetInstance(c), dir.ReferencedBy, dir.DocName)
		}
	} else {
		oldFileName := file.DocName
		files.UpdateFileCozyMetadata(c, file, false)
		file, err = vfs.ModifyFileMetadata(fs, file, &patch.DocPatch)
		if err != nil {
			return err
		}
		if patch.Name != nil && oldFileName != *patch.Name {
			// Update sharing description if this file is a file-root sharing root
			sharing.UpdateSharingDescriptionIfNeeded(middlewares.GetInstance(c), file.ReferencedBy, file.DocName)
		}
	}

	if dir != nil {
		return files.DirData(c, http.StatusOK, dir, nil)
	}
	return files.FileData(c, http.StatusOK, file, false, nil, nil)
}

// ChangesFeed is the handler for CouchDB's changes feed requests with some
// additional options, like skip_trashed.
//
// Directory-root drives use the existing subtree changes feed. File-root drives
// use an exact root-file match and ignore unrelated file changes.
func ChangesFeed(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	// TODO: if owner then fail, shouldn't be accessing their own stuff, risk recursion download kinda thing
	// TODO: should this break if there ever is actually more than 1 directory ?
	if s.HasFileDriveRoot() {
		sharedFile, err := s.GetFileDriveRoot(inst)
		if err != nil {
			return jsonapi.NotFound(errors.New("shared drive not found"))
		}
		return files.ChangesFeedForSharedFile(c, inst, sharedFile)
	}

	sharedDir, err := s.GetSharingDir(inst)
	if err != nil {
		return jsonapi.NotFound(errors.New("shared drive not found"))
	}
	return files.ChangesFeed(c, inst, sharedDir)
}

// CopyFile copies a single file from a shared drive to itself using parameters
// from the echo Context:
// - url param: `file-id`: surce file's ID
// - url query param: `DirID`: optional destination folder's ID
// - url query param: `Name`: optional destination file name
func CopyFile(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	if err := ensureDirectoryBackedSharedDrive(s); err != nil {
		return err
	}
	return files.CopyFile(c, inst, s)
}

func CreationHandler(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	if err := ensureDirectoryBackedSharedDrive(s); err != nil {
		return err
	}
	return files.Create(c, s)
}

// DestroyFileHandler handles DELETE requests to clear one element from the
// trash.
func DestroyFileHandler(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	return files.Destroy(c, s)
}

// OverwriteFileContentHandler handles PUT requests to overwrite the content of
// a file given its identifier.
func OverwriteFileContentHandler(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	return files.OverwriteFileContent(c, s)
}

// RestoreTrashFileHandler handles POST requests to restore a file or directory
// from the trash.
func RestoreTrashFileHandler(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	return files.Restore(c, s)
}

// TrashHandler handles all DELETE requests to move the file or directory with
// the specified file-id to the trash.
func TrashHandler(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	return files.Trash(c, s)
}

// UploadMetadataHandler accepts a metadata objet and persists it, so that it
// can be used in a future file upload.
func UploadMetadataHandler(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	if err := ensureDirectoryBackedSharedDrive(s); err != nil {
		return err
	}
	return files.UploadMetadataHandler(c)
}

// ThumbnailHandler serves thumbnails of the images/photos.
func ThumbnailHandler(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	return files.ThumbnailHandler(c)
}

// FileDownloadCreateHandler stores the required path into a secret usable for
// the download handler below.
func FileDownloadCreateHandler(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	// TODO: The route contract can be broader than the drive root, especially
	// for owner requests. To fix it, we need explicit shared-drive scope check here.
	//
	// The guard cannot help here: the target lives in the query string, not
	// in the route. Check the member's effective read on it before minting
	// the temporary link.
	// Resolve the target with the same precedence as files.FileDownload
	// (Path -> Id -> VersionId), else a request with both Id and Path would
	// be validated on the wrong file and mint a link for the other one.
	var id string
	if p := c.QueryParam("Path"); p != "" {
		_, file, err := inst.VFS().DirOrFileByPath(p)
		if err != nil {
			return files.WrapVfsError(err)
		}
		if file == nil {
			return jsonapi.NotFound(errors.New("shared drive target not found"))
		}
		id = file.DocID
	} else if id = c.QueryParam("Id"); id == "" {
		if versionID := c.QueryParam("VersionId"); versionID != "" {
			id = strings.Split(versionID, "/")[0]
		}
	}
	if id == "" {
		return jsonapi.BadRequest(errors.New("missing file target"))
	}
	if err := checkDriveMemberRead(c, inst, id); err != nil {
		return err
	}
	return files.FileDownload(c, s)
}

// FileDownloadHandler sends the content of a file that has previously been
// prepared via a call to FileDownloadCreateHandler.
func FileDownloadHandler(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	return files.FileDownloadHandler(c)
}

// ArchiveDownloadCreateHandler creates a zip archive link for files in a shared drive.
func ArchiveDownloadCreateHandler(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	if err := ensureDirectoryBackedSharedDrive(s); err != nil {
		return err
	}
	// The guard cannot help here: the targets live in the request body. Check
	// the member's effective read on every target before minting the link.
	// files.ArchiveDownload binds the body again, so restore it.
	if GetSharedDriveMember(c) != nil {
		body, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return jsonapi.BadJSON()
		}
		c.Request().Body = io.NopCloser(bytes.NewReader(body))
		var payload struct {
			Data struct {
				Attributes struct {
					IDs   []string   `json:"ids"`
					Files []string   `json:"files"`
					Pages []vfs.Page `json:"pages"`
				} `json:"attributes"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return jsonapi.BadJSON()
		}
		for _, id := range payload.Data.Attributes.IDs {
			if err := checkDriveMemberRead(c, inst, id); err != nil {
				return err
			}
		}
		for _, p := range payload.Data.Attributes.Pages {
			if err := checkDriveMemberRead(c, inst, p.ID); err != nil {
				return err
			}
		}
		for _, f := range payload.Data.Attributes.Files {
			// The / and /files keys cover the whole account, outside any
			// drive scope; the VFS check in files.ArchiveDownload governs
			// them. The /trash key has no resolvable identifiers.
			// ponytail: members with access to a single nested folder can
			// still archive it via ids, files outside their scope get a 404.
			if f == "/" || f == "/files" {
				continue
			}
			if f == "/trash" {
				return jsonapi.Forbidden(errors.New("cannot archive the trash"))
			}
			// files entries are paths, not ids: resolve the target first.
			dir, file, err := inst.VFS().DirOrFileByPath(f)
			if err != nil {
				return files.WrapVfsError(err)
			}
			var targetID string
			if dir != nil {
				targetID = dir.DocID
			} else {
				targetID = file.DocID
			}
			if err := checkDriveMemberRead(c, inst, targetID); err != nil {
				return err
			}
		}
	}
	return files.ArchiveDownload(c, s)
}

// ArchiveDownloadHandler serves the zip archive prepared by ArchiveDownloadCreateHandler.
func ArchiveDownloadHandler(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	return files.ArchiveDownloadHandler(c)
}

// CreateNote allows to create a note inside a shared drive.
func CreateNote(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	if err := ensureDirectoryBackedSharedDrive(s); err != nil {
		return err
	}
	return notes.CreateNote(c)
}

// GetShortcut handles GET /sharings/drives/:id/shortcuts/:file-id.
func GetShortcut(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	fileID := c.Param("file-id")
	paramNames := append([]string(nil), c.ParamNames()...)
	paramValues := append([]string(nil), c.ParamValues()...)
	defer func() {
		c.SetParamNames(paramNames...)
		c.SetParamValues(paramValues...)
	}()

	c.SetParamNames("id")
	c.SetParamValues(fileID)
	return shortcuts.Get(c)
}

// OpenNoteURL returns the parameters to open a note inside a shared drive.
// The recipient is proxied to the owner by proxy(); this handler only runs
// on the owner.
func OpenNoteURL(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	forced, err := driveOpenReadOnly(c, inst, c.Param("file-id"))
	if err != nil {
		return err
	}
	readOnly := forced || c.QueryParam("ReadOnly") == "true"
	return notes.OpenNoteURLWithReadOnly(c, readOnly)
}

// OpenOffice returns the parameter to open an office document inside a shared
// drive. The recipient is proxied to the owner by proxy(); this handler only
// runs on the owner.
func OpenOffice(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	forced, err := driveOpenReadOnly(c, inst, c.Param("file-id"))
	if err != nil {
		return err
	}
	readOnly := forced || c.QueryParam("ReadOnly") == "true"
	return office.OpenWithReadOnly(c, readOnly)
}

// OpenEditor returns the parameters to open a file with an editor inside a
// shared drive. The recipient is proxied to the owner by proxy(); this
// handler only runs on the owner.
func OpenEditor(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	forced, err := driveOpenReadOnly(c, inst, c.Param("file-id"))
	if err != nil {
		return err
	}
	readOnly := forced || c.QueryParam("ReadOnly") == "true"
	return editor.OpenURLWithReadOnly(c, readOnly)
}

// CreateSharedDriveShareByLinkHandler creates a share-by-link permission for a file in a shared drive.
func CreateSharedDriveShareByLinkHandler(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	opts := webperm.CreateShareByLinkOptions{
		ValidatePermissions: func(perms permission.Set) error {
			callerReadOnly, err := isSharedDriveCallerReadOnly(c, inst, s)
			if err != nil {
				return err
			}
			if callerReadOnly && !isSharedDrivePermissionSetReadOnly(perms) {
				return jsonapi.Forbidden(errors.New("write access denied: read-only member"))
			}
			if err := validateSharedDrivePermission(c, inst, s, perms); err != nil {
				return err
			}
			return ensureNoSharedDriveShareByLinkConflict(inst, perms)
		},
	}
	if s.AppSlug != "" {
		opts.CreatorSlug = &s.AppSlug
	}
	if domain, ok := getSharedDriveCallerDomain(c, inst, s); ok {
		opts.CreatorDomain = &domain
	}
	return webperm.HandleCreateShareByLink(c, inst, opts)
}

func validateSharedDrivePermissionTargetID(
	c echo.Context,
	inst *instance.Instance,
	s *sharing.Sharing,
	fileID string,
) error {
	fs := inst.VFS()

	dir, file, err := fs.DirOrFileByID(fileID)
	if err != nil {
		return jsonapi.BadRequest(errors.New("file is not within the shared drive"))
	}

	// The caller must be able to read the target. Drive tokens are checked
	// against the member's effective access; other tokens keep the VFS
	// permission check.
	if member := GetSharedDriveMember(c); member != nil {
		if err := checkEffectiveAccessForMember(inst, fileID, permission.GET, member); err != nil {
			return err
		}
	} else if dir != nil {
		if err := middlewares.AllowVFS(c, permission.GET, dir); err != nil {
			return err
		}
	} else {
		if err := middlewares.AllowVFS(c, permission.GET, file); err != nil {
			return err
		}
	}

	if s.HasFileDriveRoot() {
		rootID, err := s.DriveRootID()
		if err != nil {
			return jsonapi.NotFound(errors.New("shared drive root file not found"))
		}
		if fileID != rootID {
			return jsonapi.BadRequest(errors.New("file is not within the shared drive"))
		}
		return nil
	}

	rootDir, err := s.GetSharingDir(inst)
	if err != nil {
		return jsonapi.NotFound(errors.New("shared drive root directory not found"))
	}
	if err := isWithinDirectory(fs, fileID, rootDir); err != nil {
		return jsonapi.BadRequest(errors.New("file is not within the shared drive"))
	}
	return nil
}

// validateSharedDrivePermission checks that all file IDs in the permission rules are:
// - expressed as a single io.cozy.files target,
// - accessible by the current caller token,
// - and located inside the shared drive.
func validateSharedDrivePermission(c echo.Context, inst *instance.Instance, s *sharing.Sharing, perms permission.Set) error {
	fileID, err := getSharedDrivePermissionTargetID(perms)
	if err != nil {
		return jsonapi.BadRequest(err)
	}
	return validateSharedDrivePermissionTargetID(c, inst, s, fileID)
}

func getSharedDrivePermissionTargetID(perms permission.Set) (string, error) {
	if len(perms) != 1 {
		return "", errors.New("shared drive permissions must target exactly one file or folder")
	}

	perm := perms[0]
	if perm.Type != consts.Files {
		return "", errors.New("shared drive permissions can only include files")
	}
	if perm.Selector != "" {
		return "", errors.New("shared drive permissions cannot use selectors")
	}
	if len(perm.Values) != 1 {
		return "", errors.New("shared drive permissions must target exactly one file or folder")
	}

	return perm.Values[0], nil
}

func ensureNoSharedDriveShareByLinkConflict(inst *instance.Instance, perms permission.Set) error {
	targetID, err := getSharedDrivePermissionTargetID(perms)
	if err != nil {
		return jsonapi.BadRequest(err)
	}

	existing, err := permission.GetShareByLinkPermissionsForTarget(inst, consts.Files, targetID)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		return jsonapi.Conflict(errors.New("share-by-link already exists for this file or folder"))
	}

	return nil
}

// ListSharedDriveShareByLinkPermissions returns the share-by-link permissions
// for the requested files/folders inside a shared drive. Read-only members can
// only see read-only links.
func ListSharedDriveShareByLinkPermissions(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	callerReadOnly, err := isSharedDriveCallerReadOnly(c, inst, s)
	if err != nil {
		return err
	}

	targetIDs, err := extractSharedDrivePermissionTargetIDs(c, inst, s)
	if err != nil {
		return err
	}

	permsByTarget, err := permission.GetShareByLinkPermissionsForTargets(inst, consts.Files, targetIDs)
	if err != nil {
		return err
	}

	out := make([]jsonapi.Object, 0)
	for _, targetID := range targetIDs {
		for _, perm := range permsByTarget[targetID] {
			if callerReadOnly && !isSharedDriveShareByLinkReadOnly(perm) {
				continue
			}

			if perm.Password != nil {
				perm.Password = true
			}
			permCopy := *perm
			out = append(out, &webperm.APIPermission{Permission: &permCopy})
		}
	}

	return jsonapi.DataList(c, http.StatusOK, out, nil)
}

func extractSharedDrivePermissionTargetIDs(c echo.Context, inst *instance.Instance, s *sharing.Sharing) ([]string, error) {
	rawIDs := strings.TrimSpace(c.QueryParam("ids"))
	if rawIDs == "" {
		return nil, jsonapi.InvalidParameter("ids", errors.New("ids is required"))
	}

	ids := make([]string, 0)
	for _, id := range strings.Split(rawIDs, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, jsonapi.InvalidParameter("ids", errors.New("ids must contain file or folder IDs"))
		}
		if err := validateSharedDrivePermissionTargetID(c, inst, s, id); err != nil {
			return nil, jsonapi.InvalidParameter("ids", errors.New("file is not within the shared drive"))
		}

		ids = append(ids, id)
	}

	return ids, nil
}

// getSharedDriveCallerDomain returns the domain of the user who initiated the
// request. For recipient requests, we resolve the member from the drive token.
// For owner requests, we use the current instance domain.
func getSharedDriveCallerDomain(c echo.Context, inst *instance.Instance, s *sharing.Sharing) (string, bool) {
	actorPerm, err := middlewares.GetPermission(c)
	if err != nil {
		return "", false
	}
	return getSharedDriveCallerDomainFromPermission(c, inst, s, actorPerm)
}

func getSharedDriveCallerDomainFromPermission(
	c echo.Context,
	inst *instance.Instance,
	s *sharing.Sharing,
	actorPerm *permission.Permission,
) (string, bool) {
	if actorPerm == nil {
		return "", false
	}
	switch actorPerm.Type {
	// Public share tokens represent anonymous/public access and must never be
	// treated as a caller identity for shared-drive permission mutations.
	case permission.TypeShareByLink, permission.TypeSharePreview:
		return "", false

	case permission.TypeShareInteract:
		token := middlewares.GetRequestToken(c)
		if token != "" {
			member, err := s.FindMemberByInteractCode(inst, token)
			if err == nil && member != nil {
				if domain := member.InstanceHost(); domain != "" {
					return domain, true
				}
			}
		}
		return "", false

	default:
		if inst.Domain != "" {
			return inst.Domain, true
		}
		return "", false
	}
}

func isSharePermissionToken(perm *permission.Permission) bool {
	if perm == nil {
		return false
	}
	switch perm.Type {
	case permission.TypeShareInteract, permission.TypeShareByLink, permission.TypeSharePreview:
		return true
	default:
		return false
	}
}

func isSharedDriveCallerReadOnly(c echo.Context, inst *instance.Instance, s *sharing.Sharing) (bool, error) {
	actorPerm, err := middlewares.GetPermission(c)
	if err != nil {
		return false, err
	}
	if actorPerm == nil {
		return false, echo.NewHTTPError(http.StatusUnauthorized, "no permission")
	}

	switch actorPerm.Type {
	case permission.TypeShareByLink, permission.TypeSharePreview:
		return false, jsonapi.Forbidden(errors.New("public share token cannot access shared-drive links"))

	case permission.TypeShareInteract:
		token := middlewares.GetRequestToken(c)
		if token == "" {
			return false, jsonapi.Forbidden(errors.New("missing shared-drive token"))
		}
		member, err := s.FindMemberByInteractCode(inst, token)
		if err != nil {
			return false, err
		}
		return member.ReadOnly, nil

	case permission.TypeWebapp, permission.TypeKonnector, permission.TypeOauth, permission.TypeCLI:
		return false, nil

	default:
		// Default to read-only for unknown permission types to avoid
		// accidentally leaking write access.
		return true, nil
	}
}

func isSharedDriveShareByLinkReadOnly(perm *permission.Permission) bool {
	if perm == nil {
		return false
	}
	return isSharedDrivePermissionSetReadOnly(perm.Permissions)
}

func isSharedDrivePermissionSetReadOnly(perms permission.Set) bool {
	if len(perms) == 0 {
		return false
	}
	for _, rule := range perms {
		if !rule.Verbs.ReadOnly() {
			return false
		}
	}
	return true
}

type sharedDrivePermissionMutationContext struct {
	existingPerm     *permission.Permission
	actorPerm        *permission.Permission
	callerReadOnly   bool
	callerDomain     string
	identityResolved bool
}

type sharedDrivePermissionPatchContext struct {
	mutation *sharedDrivePermissionMutationContext
	patch    permission.Permission
}

// CheckSharedDrivePermissionMutationAuthorization validates whether the caller can
// mutate a shared-drive share-by-link permission.
func CheckSharedDrivePermissionMutationAuthorization(
	s *sharing.Sharing,
	existingPerm *permission.Permission,
	actorPerm *permission.Permission,
	callerDomain string,
	identityResolved bool,
	callerReadOnly bool,
	allowWritersToManageLinks bool,
) error {
	// Public share tokens are never allowed to mutate permissions.
	if actorPerm != nil &&
		(actorPerm.Type == permission.TypeShareByLink || actorPerm.Type == permission.TypeSharePreview) {
		return jsonapi.Forbidden(errors.New("public share token cannot modify this permission"))
	}
	if actorPerm != nil &&
		actorPerm.Type == permission.TypeShareInteract &&
		callerReadOnly &&
		!isSharedDriveShareByLinkReadOnly(existingPerm) {
		return jsonapi.Forbidden(errors.New("write access denied: read-only member"))
	}

	isOwner := s.Owner && !isSharePermissionToken(actorPerm)
	isCreator := false
	if callerDomain != "" && existingPerm.Metadata != nil && len(existingPerm.Metadata.UpdatedByApps) > 0 {
		creatorDomain := existingPerm.Metadata.UpdatedByApps[0].Instance
		isCreator = creatorDomain == callerDomain
	}
	allowToManageLinks := allowWritersToManageLinks &&
		actorPerm != nil &&
		actorPerm.Type == permission.TypeShareInteract &&
		identityResolved &&
		!callerReadOnly

	if !isOwner && !isCreator && !allowToManageLinks {
		if actorPerm != nil && actorPerm.Type == permission.TypeShareInteract && !identityResolved {
			return jsonapi.Forbidden(errors.New("cannot verify caller identity for this shared-drive token"))
		}
		return jsonapi.Forbidden(errors.New("only creator or owner can modify this permission"))
	}

	return nil
}

func resolveSharedDriveMutationActor(
	c echo.Context,
	inst *instance.Instance,
	s *sharing.Sharing,
) (*sharedDrivePermissionMutationContext, error) {
	actorPerm, err := middlewares.GetPermission(c)
	if err != nil {
		return nil, err
	}

	ctx := &sharedDrivePermissionMutationContext{
		actorPerm: actorPerm,
	}
	if actorPerm != nil && actorPerm.Type == permission.TypeShareInteract {
		ctx.callerReadOnly, err = isSharedDriveCallerReadOnly(c, inst, s)
		if err != nil {
			return nil, err
		}
	}
	ctx.callerDomain, ctx.identityResolved = getSharedDriveCallerDomainFromPermission(c, inst, s, actorPerm)
	return ctx, nil
}

func ensureSharedDrivePermissionBelongsToDrive(
	c echo.Context,
	inst *instance.Instance,
	s *sharing.Sharing,
	perm *permission.Permission,
) error {
	if err := validateSharedDrivePermission(c, inst, s, perm.Permissions); err != nil {
		return jsonapi.Forbidden(errors.New("permission does not belong to this shared drive"))
	}
	return nil
}

func validateReadOnlySharedDrivePermissionPatch(
	actorPerm *permission.Permission,
	callerReadOnly bool,
	patch permission.Permission,
) error {
	if actorPerm == nil || actorPerm.Type != permission.TypeShareInteract || !callerReadOnly {
		return nil
	}
	if len(patch.Permissions) > 0 {
		return jsonapi.Forbidden(errors.New("write access denied: read-only member"))
	}
	return nil
}

func prepareSharedDrivePermissionMutation(
	c echo.Context,
	inst *instance.Instance,
	s *sharing.Sharing,
	permID string,
) (*sharedDrivePermissionMutationContext, error) {
	existingPerm, err := permission.GetPermissionByIDIncludingExpired(inst, permID)
	if err != nil {
		if couchdb.IsNotFoundError(err) {
			return nil, jsonapi.NotFound(errors.New("permission not found"))
		}
		return nil, err
	}

	if existingPerm.Type != permission.TypeShareByLink {
		return nil, jsonapi.BadRequest(errors.New("not a share-by-link permission"))
	}

	if err := ensureSharedDrivePermissionBelongsToDrive(c, inst, s, existingPerm); err != nil {
		return nil, err
	}

	ctx, err := resolveSharedDriveMutationActor(c, inst, s)
	if err != nil {
		return nil, err
	}
	ctx.existingPerm = existingPerm

	if err := CheckSharedDrivePermissionMutationAuthorization(
		s,
		existingPerm,
		ctx.actorPerm,
		ctx.callerDomain,
		ctx.identityResolved,
		ctx.callerReadOnly,
		config.GetSharingConfig(inst.ContextName).AllowWritersToManageLinks,
	); err != nil {
		return nil, err
	}

	return ctx, nil
}

func prepareSharedDrivePermissionPatch(
	c echo.Context,
	inst *instance.Instance,
	s *sharing.Sharing,
	permID string,
) (*sharedDrivePermissionPatchContext, error) {
	mutation, err := prepareSharedDrivePermissionMutation(c, inst, s, permID)
	if err != nil {
		return nil, err
	}

	var patch permission.Permission
	if _, err := jsonapi.Bind(c.Request().Body, &patch); err != nil {
		return nil, err
	}

	if err := ValidateSharedDrivePermissionPatch(patch); err != nil {
		return nil, err
	}
	if err := validateReadOnlySharedDrivePermissionPatch(mutation.actorPerm, mutation.callerReadOnly, patch); err != nil {
		return nil, err
	}
	if err := ValidateSharedDrivePermissionSetPatch(c, inst, s, mutation.actorPerm, mutation.existingPerm, patch); err != nil {
		return nil, err
	}

	return &sharedDrivePermissionPatchContext{
		mutation: mutation,
		patch:    patch,
	}, nil
}

// ValidateSharedDrivePermissionPatch validates the payload for patching a
// shared-drive share-by-link permission.
func ValidateSharedDrivePermissionPatch(patch permission.Permission) error {
	if len(patch.Codes) > 0 {
		return jsonapi.BadRequest(errors.New("codes cannot be modified"))
	}
	if patch.Password == nil && patch.ExpiresAt == nil && len(patch.Permissions) == 0 {
		return jsonapi.BadRequest(errors.New("password, expires_at, or permissions must be provided"))
	}
	if patch.Password != nil {
		if _, ok := patch.Password.(string); !ok {
			return jsonapi.BadRequest(errors.New("password must be a string"))
		}
	}
	if patch.ExpiresAt != nil {
		if _, ok := patch.ExpiresAt.(string); !ok {
			return jsonapi.BadRequest(errors.New("expires_at must be a string"))
		}
	}
	return nil
}

func ValidateSharedDrivePermissionSetPatch(
	c echo.Context,
	inst *instance.Instance,
	s *sharing.Sharing,
	actorPerm *permission.Permission,
	existingPerm *permission.Permission,
	patch permission.Permission,
) error {
	if len(patch.Permissions) == 0 {
		return nil
	}
	if actorPerm == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "no permission")
	}

	currentTargetID, err := getSharedDrivePermissionTargetID(existingPerm.Permissions)
	if err != nil {
		return jsonapi.BadRequest(errors.New("invalid existing shared-drive permission"))
	}
	patchTargetID, err := getSharedDrivePermissionTargetID(patch.Permissions)
	if err != nil {
		return jsonapi.BadRequest(err)
	}
	if patchTargetID != currentTargetID {
		return jsonapi.BadRequest(errors.New("shared-drive permission target cannot be changed"))
	}

	if err := validateSharedDrivePermission(c, inst, s, patch.Permissions); err != nil {
		return err
	}
	if err := permission.CheckSetPermissions(patch.Permissions, actorPerm); err != nil {
		return err
	}
	return nil
}

// ApplySharedDrivePermissionPatch applies password, expires_at, and permission
// updates for a shared-drive share-by-link permission.
func ApplySharedDrivePermissionPatch(perm *permission.Permission, patch permission.Permission) error {
	// Apply password change
	if patch.Password != nil {
		pass := patch.Password.(string)
		if pass == "" {
			perm.Password = nil
		} else {
			hash, err := crypto.GenerateFromPassphrase([]byte(pass))
			if err != nil {
				return err
			}
			perm.Password = hash
		}
	}

	// Apply TTL change
	if patch.ExpiresAt != nil {
		at := patch.ExpiresAt.(string)
		if at == "" {
			perm.ExpiresAt = nil
		} else {
			expiresAt, err := time.Parse(time.RFC3339, at)
			if err != nil {
				return jsonapi.BadRequest(errors.New("expires_at must be an RFC3339 date-time"))
			}
			perm.ExpiresAt = expiresAt
		}
	}
	if len(patch.Permissions) > 0 {
		perm.Permissions = patch.Permissions
	}
	return nil
}

// isWithinDirectory checks if the given file/directory ID is within the
// specified parent directory (including the parent itself).
func isWithinDirectory(fs vfs.VFS, fileID string, parent *vfs.DirDoc) error {
	if fileID == parent.ID() {
		return nil
	}

	dir, file, err := fs.DirOrFileByID(fileID)
	if err != nil {
		return err
	}

	var fullpath string
	if dir != nil {
		fullpath = dir.Fullpath
	} else {
		fullpath, err = file.Path(fs)
		if err != nil {
			return err
		}
	}

	// Check if path is within parent directory
	if strings.HasPrefix(fullpath, parent.Fullpath+"/") {
		return nil
	}

	return errors.New("file is not within the directory")
}

// RevokeSharedDrivePermission revokes a share-by-link permission for a file in a shared drive.
func RevokeSharedDrivePermission(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	permID := c.Param("perm-id")
	if permID == "" {
		return jsonapi.BadRequest(errors.New("missing permission ID"))
	}

	ctx, err := prepareSharedDrivePermissionMutation(c, inst, s, permID)
	if err != nil {
		return err
	}

	if err := ctx.existingPerm.Revoke(inst); err != nil {
		return err
	}

	return c.NoContent(http.StatusNoContent)
}

// PatchSharedDrivePermissionHandler modifies a share-by-link permission in a shared drive.
// Only the creator or the drive owner can modify. Codes are immutable,
// permission updates must keep the same target, and read-only recipients can
// only patch password / expiration on their own read-only links.
func PatchSharedDrivePermissionHandler(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	permID := c.Param("perm-id")
	if permID == "" {
		return jsonapi.BadRequest(errors.New("missing permission ID"))
	}

	ctx, err := prepareSharedDrivePermissionPatch(c, inst, s, permID)
	if err != nil {
		return err
	}
	if err := ApplySharedDrivePermissionPatch(ctx.mutation.existingPerm, ctx.patch); err != nil {
		return err
	}

	if ctx.mutation.existingPerm.Metadata != nil {
		ctx.mutation.existingPerm.Metadata.UpdatedAt = time.Now()
	}

	// Save
	if err := couchdb.UpdateDoc(inst, ctx.mutation.existingPerm); err != nil {
		return err
	}

	// Don't send the password hash to the client
	if ctx.mutation.existingPerm.Password != nil {
		ctx.mutation.existingPerm.Password = true
	}

	return jsonapi.Data(c, http.StatusOK, &webperm.APIPermission{Permission: ctx.mutation.existingPerm}, nil)
}

// drivesRoutes sets the routing for the shared drives
func drivesRoutes(router *echo.Group) {
	group := router.Group("/drives")
	group.GET("", ListSharedDrives)
	group.POST("", CreateSharedDrive)
	group.POST("/move", MoveHandler)

	drive := group.Group("/:id")

	drive.HEAD("/download/:file-id", proxy(ReadFileContentFromIDHandler, true))
	drive.GET("/download/:file-id", proxy(ReadFileContentFromIDHandler, true))

	drive.HEAD("/download/:file-id/:version-id", proxy(ReadFileContentFromVersion, true))
	drive.GET("/download/:file-id/:version-id", proxy(ReadFileContentFromVersion, true))
	drive.POST("/:file-id/versions", proxy(CopyVersionHandler, true))
	drive.DELETE("/:file-id/:version-id", proxy(DeleteFileVersionMetadata, true))

	drive.GET("/_changes", proxy(ChangesFeed, true))

	drive.HEAD("/:file-id", proxy(HeadDirOrFile, true))

	drive.GET("/metadata", proxy(ReadMetadataFromPath, true))
	drive.GET("/:file-id", proxy(GetDirOrFileData, true))
	drive.GET("/:file-id/size", proxy(GetDirSize, true))

	drive.PATCH("/:file-id", proxy(ModifyMetadataByIDHandler, true))

	drive.POST("/", proxy(CreationHandler, true))
	drive.POST("/:file-id", proxy(CreationHandler, true))
	drive.PUT("/:file-id", proxy(OverwriteFileContentHandler, true))
	drive.POST("/upload/metadata", proxy(UploadMetadataHandler, true))
	drive.POST("/:file-id/copy", proxy(CopyFile, true))

	drive.GET("/:file-id/thumbnails/:secret/:format", proxy(ThumbnailHandler, true))

	drive.POST("/downloads", proxy(FileDownloadCreateHandler, true))
	drive.GET("/downloads/:secret/:fake-name", proxy(FileDownloadHandler, false))

	drive.POST("/archive", proxy(ArchiveDownloadCreateHandler, true))
	drive.GET("/archive/:secret/:fake-name", proxy(ArchiveDownloadHandler, false))

	drive.POST("/trash/:file-id", proxy(RestoreTrashFileHandler, true))
	drive.DELETE("/trash/:file-id", proxy(DestroyFileHandler, true))

	drive.DELETE("/:file-id", proxy(TrashHandler, true))

	drive.POST("/notes", proxy(CreateNote, true))
	drive.GET("/notes/:file-id/open", proxy(OpenNoteURL, true))
	drive.GET("/recipients/:file-id", proxy(GetDriveEffectiveRecipients, true))
	drive.GET("/office/:file-id/open", proxy(OpenOffice, true))
	drive.GET("/editor/:file-id/open", proxy(OpenEditor, true))

	drive.GET("/shortcuts/:file-id", proxy(GetShortcut, true))

	drive.GET("/realtime", Ws)

	// Share-by-link (public link) endpoints for files in shared drives
	drive.GET("/permissions", proxy(ListSharedDriveShareByLinkPermissions, true))
	drive.POST("/permissions", proxy(CreateSharedDriveShareByLinkHandler, true))
	drive.PATCH("/permissions/:perm-id", proxy(PatchSharedDrivePermissionHandler, true))
	drive.DELETE("/permissions/:perm-id", proxy(RevokeSharedDrivePermission, true))
}

func proxy(fn func(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error, needsAuth bool) echo.HandlerFunc {
	return func(c echo.Context) error {
		inst := middlewares.GetInstance(c)
		s, err := sharing.FindSharing(inst, c.Param("id"))
		if err != nil {
			return wrapErrors(err)
		}
		if !s.Drive {
			return jsonapi.NotFound(errors.New("not a drive"))
		}
		if !s.Active {
			return jsonapi.Forbidden(middlewares.ErrForbidden)
		}

		if s.Owner {
			member, err := sharedDriveInteractMember(c, inst, s)
			if err != nil {
				return err
			}
			SetSharedDriveMember(c, member)
			if err := guardSharedDriveRouteForMember(c, inst, s); err != nil {
				return err
			}
			return fn(c, inst, s)
		}

		// On a recipient, we proxy the request to the owner
		// Some routes need to be publicly accessible but others should be
		// require an authorization token.
		if needsAuth {
			method := c.Request().Method
			if method == http.MethodHead {
				method = http.MethodGet
			}
			verb := permission.Verb(method)
			if err := middlewares.AllowWholeType(c, verb, consts.Files); err != nil {
				return err
			}
		}

		if len(s.Credentials) == 0 {
			return jsonapi.InternalServerError(errors.New("no credentials"))
		}
		token := s.Credentials[0].DriveToken
		u, err := url.Parse(s.Members[0].Instance)
		if err != nil {
			return jsonapi.InternalServerError(err)
		}

		// XXX Let's try to avoid one http request by cheating a bit. If the two
		// instances are on the same domain (same stack), we can call directly
		// the handler. We still rewrite the request to match the real proxied
		// request shape. It helps for performances.
		if owner, err := lifecycle.GetInstance(u.Host); err == nil {
			c.Request().Header.Set(echo.HeaderAuthorization, "Bearer "+token)
			middlewares.ForcePermission(c, nil)
			c.Set("claims", nil)
			middlewares.SetInstance(c, owner)
			ownerSharing, err := sharing.FindSharing(owner, c.Param("id"))
			if err != nil {
				return wrapErrors(err)
			}
			// The member must be resolved against the owner's instance and
			// sharing: the recipient-side member would authorize nothing here.
			ownerMember, err := sharedDriveInteractMember(c, owner, ownerSharing)
			if err != nil {
				return err
			}
			SetSharedDriveMember(c, ownerMember)
			if err := guardSharedDriveRouteForMember(c, owner, ownerSharing); err != nil {
				return err
			}
			return fn(c, owner, s)
		}

		director := func(req *http.Request) {
			req.URL = u
			req.URL.Path = c.Request().URL.Path
			req.URL.RawQuery = c.Request().URL.RawQuery
			req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
			req.Header.Del(echo.HeaderCookie)
			req.Header.Del("Host")
		}
		proxy := &httputil.ReverseProxy{Director: director}
		logger := inst.Logger().WithNamespace("drive-proxy").Writer()
		defer logger.Close()
		proxy.ErrorLog = log.New(logger, "", 0)
		proxy.ServeHTTP(c.Response(), c.Request())
		return nil
	}
}

// contextDriveMember is the echo context key where proxy stores the drive
// member resolved for the request, once, before the guard and handlers run.
const contextDriveMember = "drive-member"

// SetSharedDriveMember stores the resolved drive member in the echo context.
// Called by proxy right before the guard runs, on the instance that
// authorizes the request (the owner's). A nil member marks a resolved
// non-member request (owner's own token, share-by-link).
func SetSharedDriveMember(c echo.Context, member *sharing.Member) {
	c.Set(contextDriveMember, member)
}

// GetSharedDriveMember returns the drive member stored by proxy, or nil for
// non-member tokens. Only valid on drive routes, after proxy ran.
func GetSharedDriveMember(c echo.Context) *sharing.Member {
	member, _ := c.Get(contextDriveMember).(*sharing.Member)
	return member
}

// sharedDriveInteractMember resolves the sharing member behind the request
// token. It returns (nil, nil) when the request does not carry a drive token
// (share-interact): the owner's own tokens and public secret routes carry no
// member identity and are not constrained by effective access.
func sharedDriveInteractMember(c echo.Context, inst *instance.Instance, s *sharing.Sharing) (*sharing.Member, error) {
	pdoc, err := middlewares.GetPermission(c)
	if err != nil || pdoc.Type != permission.TypeShareInteract {
		return nil, nil
	}
	token := middlewares.GetRequestToken(c)
	member, err := s.FindMemberByInteractCode(inst, token)
	if err != nil {
		return nil, wrapErrors(err)
	}
	if member == nil {
		return nil, jsonapi.Forbidden(errors.New("not a member of this sharing"))
	}
	return member, nil
}

// guardSharedDriveRouteForMember authorizes a request reaching the owner of
// a shared drive against the effective access of the calling member on the
// target file or folder. Only drive tokens (share-interact) are constrained
// this way: the owner's own tokens have full rights, and public secret
// routes carry no member identity (AllowVFS in the handlers governs them).
// The member is resolved once by proxy and read from the context.
func guardSharedDriveRouteForMember(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	member := GetSharedDriveMember(c)
	if member == nil {
		return nil
	}

	method := c.Request().Method
	if method == http.MethodHead {
		method = http.MethodGet
	}
	verb := permission.Verb(method)
	reqPath := c.Request().URL.Path
	fileID := c.Param("file-id")

	// Only the owner can trash or destroy the root of a shared drive: the VFS
	// revocation hook would take the whole drive down with it. Deleting a
	// version of the root file is neither: it leaves the drive in place.
	if method == http.MethodDelete && c.Param("version-id") == "" {
		if rootID, err := s.DriveRootID(); err == nil && fileID == rootID {
			return jsonapi.Forbidden(errors.New("only the owner can trash or destroy the root of a shared drive"))
		}
	}

	// Share-by-link routes keep their own authorization layer. Match the
	// registered route pattern, not the raw URL, so a future route whose
	// path contains "/permissions" cannot silently bypass this guard.
	if strings.HasPrefix(c.Path(), "/sharings/drives/:id/permissions") {
		return nil
	}
	// Open routes (note, office, editor) are checked in the handler by
	// driveOpenReadOnly: the guard's 403 would leak the existence of a
	// target outside every scope, where the handler answers 404.
	if strings.HasSuffix(c.Path(), "/open") {
		return nil
	}
	// Trashed files are outside every sharing scope: the effective access
	// resolver cannot see them. Restore and destroy are authorized against
	// the effective write access on the folder the file would be restored
	// to (which is also where destroy removes it from, logically).
	if strings.Contains(reqPath, "/trash/") {
		if shouldCheck, requireWrite := sharedDrivePermissionCheck(method, reqPath); !shouldCheck || !requireWrite {
			return nil
		}
		if err := checkTrashEffectiveAccess(inst, fileID, member); err != nil {
			return err
		}
		// The guard has authorized the request: the handlers skip their VFS
		// permission check for share-interact tokens (it cannot succeed on a
		// trashed file, which has lost its referenced_by and whose path is
		// outside the sharing rules). See files.driveMemberAuthorized.
		return nil
	}

	// Write routes without a file target create content at the drive root:
	// they require effective write on the root folder.
	//
	// ponytail: POST /upload/metadata is checked against the drive root,
	// not the dir_id declared in its body; resolve the body dir_id when
	// limited_access lands.
	if fileID == "" {
		if shouldCheck, requireWrite := sharedDrivePermissionCheck(method, reqPath); !shouldCheck || !requireWrite {
			return nil
		}
		rootID, err := s.DriveRootID()
		if err != nil {
			return wrapErrors(err)
		}
		return checkEffectiveAccessForMember(inst, rootID, permission.POST, member)
	}

	// Copy needs effective read on the source and effective write on the
	// destination folder. File-root drives do not support copy: let the
	// handler answer 422.
	if method == http.MethodPost && strings.HasSuffix(reqPath, "/copy") {
		if s.HasFileDriveRoot() {
			return nil
		}
		if err := checkEffectiveAccessForMember(inst, fileID, permission.GET, member); err != nil {
			return err
		}
		destID := c.QueryParam("DirID")
		if destID == "" {
			file, err := inst.VFS().FileByID(fileID)
			if err != nil {
				return files.WrapVfsError(err)
			}
			destID = file.DirID
		}
		return checkEffectiveAccessForMember(inst, destID, permission.POST, member)
	}

	return checkEffectiveAccessForMember(inst, fileID, verb, member)
}

// checkEffectiveAccessForMember checks that the given sharing member has the
// effective access required by verb on the target file or folder.
func checkEffectiveAccessForMember(inst *instance.Instance, targetID string, verb permission.Verb, member *sharing.Member) error {
	ea, err := authorizeDriveTarget(inst, member, targetID)
	if err != nil {
		return err
	}
	if !ea.Can(verb) {
		return jsonapi.Forbidden(errors.New("insufficient access on the target file or folder"))
	}
	return nil
}

// checkTrashEffectiveAccess authorizes a restore or destroy of a trashed
// file against the member's effective write access on the folder the file
// would be restored to. A trashed file is outside every sharing scope, so
// the check walks up from the restore folder to the first existing ancestor
// (the restore recreates the missing hierarchy under it via MkdirAll) and
// resolves the effective access there. A file whose restore folder is in no
// sharing scope (e.g. trashed from outside any drive) is rejected.
func checkTrashEffectiveAccess(inst *instance.Instance, fileID string, member *sharing.Member) error {
	fs := inst.VFS()
	dir, file, err := fs.DirOrFileByID(fileID)
	if err != nil {
		return files.WrapVfsError(err)
	}

	var restorePath, docPath string
	if dir != nil {
		restorePath = dir.RestorePath
		docPath = dir.Fullpath
	} else {
		restorePath = file.RestorePath
		docPath, err = file.Path(fs)
		if err != nil {
			return files.WrapVfsError(err)
		}
	}

	// A file trashed with its parent hierarchy has no restore path of its
	// own: resolve it from the trashed root folder, like getRestoreDir does.
	if restorePath == "" {
		rel := strings.TrimPrefix(docPath, vfs.TrashDirName+"/")
		split := strings.Index(rel, "/")
		if split < 0 {
			// ponytail: no resolvable restore root; getRestoreDir would fall
			// back to the instance root, which is in no sharing scope.
			return jsonapi.Forbidden(errors.New("insufficient access on the target file or folder"))
		}
		root, err := fs.DirByPath(vfs.TrashDirName + "/" + rel[:split])
		if err != nil {
			return files.WrapVfsError(err)
		}
		rest := path.Dir(rel[split+1:])
		restorePath = path.Join(root.RestorePath, root.DocName, rest)
	}
	// The restore path is the folder the file or directory is restored into
	// (and the directory itself is recreated inside it): that is where the
	// write happens.
	parentPath := restorePath

	// Walk up to the first existing ancestor: the restore recreates the
	// missing hierarchy under it, so that ancestor is where the write
	// actually happens.
	for {
		ancestor, err := fs.DirByPath(parentPath)
		if err == nil {
			return checkEffectiveAccessForMember(inst, ancestor.ID(), permission.POST, member)
		}
		if !os.IsNotExist(err) {
			return files.WrapVfsError(err)
		}
		next := path.Dir(parentPath)
		if next == parentPath {
			return jsonapi.Forbidden(errors.New("insufficient access on the target file or folder"))
		}
		parentPath = next
	}
}

func sharedDrivePermissionCheck(method, path string) (shouldCheck bool, requireWrite bool) {
	if method != http.MethodPost &&
		method != http.MethodPut &&
		method != http.MethodPatch &&
		method != http.MethodDelete {
		return false, false
	}

	// POST /downloads creates a temporary link without mutating shared-drive data.
	if method == http.MethodPost && (strings.HasSuffix(path, "/downloads") || strings.HasSuffix(path, "/archive")) {
		return false, false
	}

	// Creating a share-by-link is allowed for read-only members as long as the
	// requested permission set stays read-only.
	if method == http.MethodPost && strings.HasSuffix(path, "/permissions") {
		return true, false
	}
	if (method == http.MethodPatch || method == http.MethodDelete) &&
		strings.Contains(path, "/permissions/") {
		return true, false
	}

	return true, true
}

// checkSharedDrivePermission checks if the current user has permission to access
// the specified shared drive. It verifies that:
// 1. The sharing exists and is a drive
// 2. The current user is a member of the sharing (by domain or email)
// Returns the sharing if the user has the required permissions.
func checkSharedDrivePermission(inst *instance.Instance, sharingID string) (*sharing.Sharing, error) {
	// Find the sharing by ID
	s, err := sharing.FindSharing(inst, sharingID)
	if err != nil {
		// CouchDB not_found: treat as no access
		if strings.Contains(err.Error(), "not_found") {
			return nil, jsonapi.Forbidden(errors.New("not a member of this sharing"))
		}
		return nil, wrapErrors(err)
	}

	// Check if it's a drive
	if !s.Drive {
		return nil, jsonapi.NotFound(errors.New("not a drive"))
	}

	// Check that current user is a member of the sharing and get their read-only status
	currDomain := inst.Domain
	if strings.Contains(currDomain, ":") {
		currDomain = strings.SplitN(currDomain, ":", 2)[0]
	}

	// Get current instance email for comparison
	currEmail, _ := inst.SettingsEMail()

	isMember := false

	// If this is the owner instance, they're a member with write access
	if s.Owner {
		isMember = true
	} else {
		// On a recipient's instance, their own member entry is typically at index 1
		// (index 0 is the owner). Check if there's a member with an Instance field set.
		for _, m := range s.Members {
			memberHost := m.InstanceHost()
			if memberHost == "" {
				continue
			}
			// Check by domain
			if memberHost == inst.Domain || memberHost == currDomain {
				isMember = true
				break
			}
			// Check by email
			if currEmail != "" && m.Email == currEmail {
				isMember = true
				break
			}
		}
	}

	if !isMember {
		return nil, jsonapi.Forbidden(errors.New("not a member of this sharing"))
	}

	return s, nil
}

func ensureDirectoryBackedSharedDrive(s *sharing.Sharing) error {
	if s.HasFileDriveRoot() {
		return jsonapi.NewError(http.StatusUnprocessableEntity, "file-root shared drives do not support this endpoint")
	}
	return nil
}
