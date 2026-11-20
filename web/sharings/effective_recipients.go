package sharings

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

// apiEffectiveRecipient serializes a sharing.EffectiveRecipient as JSON-API.
type apiEffectiveRecipient struct {
	*sharing.EffectiveRecipient
	id string
}

func (a *apiEffectiveRecipient) ID() string                             { return a.id }
func (a *apiEffectiveRecipient) Rev() string                            { return "" }
func (a *apiEffectiveRecipient) DocType() string                        { return constsSharingsRecipients }
func (a *apiEffectiveRecipient) Clone() couchdb.Doc                     { c := *a; return &c }
func (a *apiEffectiveRecipient) SetID(id string)                        { a.id = id }
func (a *apiEffectiveRecipient) SetRev(_ string)                        {}
func (a *apiEffectiveRecipient) Links() *jsonapi.LinksList              { return nil }
func (a *apiEffectiveRecipient) Relationships() jsonapi.RelationshipMap { return nil }
func (a *apiEffectiveRecipient) Included() []jsonapi.Object             { return nil }

const constsSharingsRecipients = "io.cozy.sharings.recipients"

// GetEffectiveRecipients handles GET /sharings/recipients/:file-id. It
// returns the combined list of people who can access the file or folder,
// including recipients inherited from parent shared folders. The caller
// needs at least read access to the target, and public share-by-link tokens
// are rejected: a link holder must not be able to enumerate sharing members.
// Recipients of ancestor sharings are included by design when the caller can
// read their root: their additive access is real. A member's share-interact
// code does not see the sharings above its own scope.
func GetEffectiveRecipients(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	if _, _, err := loadDirOrFileFromParam(c, inst, permission.GET); err != nil {
		return err
	}
	return respondEffectiveRecipients(c, inst, c.Param("file-id"))
}

// GetDriveEffectiveRecipients handles GET
// /sharings/drives/:id/recipients/:file-id. It is wrapped in proxy(): the
// handler always runs on the owner instance of the drive (directly, or via
// the drive token for recipients), so the access check and the resolution
// see every sharing applying to the target. The target must belong to this
// drive. Recipients inherited from sharings above the drive root are
// included by design: their additive access is real.
func GetDriveEffectiveRecipients(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	if _, _, err := loadDirOrFileFromParam(c, inst, permission.GET); err != nil {
		return err
	}
	if err := checkFileInsideDrive(inst, s, c.Param("file-id")); err != nil {
		return err
	}
	return respondEffectiveRecipients(c, inst, c.Param("file-id"))
}

func respondEffectiveRecipients(c echo.Context, inst *instance.Instance, fileID string) error {
	// Public share tokens (share-by-link, share preview) are anonymous
	// access: they must not enumerate the members of the applicable
	// sharings, including unrelated ancestor sharings.
	pdoc, err := middlewares.GetPermission(c)
	if err != nil {
		return err
	}
	switch pdoc.Type {
	case permission.TypeShareByLink, permission.TypeSharePreview:
		return jsonapi.Forbidden(errors.New("public share token cannot list sharing members"))
	}
	member, err := interactCodeMember(c, inst, pdoc)
	if err != nil {
		return err
	}
	recipients, err := sharing.NewAccessResolver(inst).EffectiveRecipients(fileID, member)
	if err != nil {
		if os.IsNotExist(err) {
			return jsonapi.NotFound(err)
		}
		return wrapErrors(err)
	}
	objs := make([]jsonapi.Object, len(recipients))
	for i := range recipients {
		r := recipients[i]
		objs[i] = &apiEffectiveRecipient{EffectiveRecipient: &r, id: effectiveRecipientID(&r)}
	}
	return jsonapi.DataListWithMeta(c, http.StatusOK, jsonapi.Meta{FileID: fileID}, objs, nil)
}

// interactCodeMember gives the member a share-interact code belongs to, or nil
// for any other token.
func interactCodeMember(c echo.Context, inst *instance.Instance, pdoc *permission.Permission) (*sharing.Member, error) {
	if pdoc.Type != permission.TypeShareInteract {
		return nil, nil
	}
	_, sharingID, _ := strings.Cut(pdoc.SourceID, "/")
	s, err := sharing.FindSharing(inst, sharingID)
	if err != nil {
		return nil, wrapErrors(err)
	}
	member, err := s.FindMemberByInteractCode(inst, middlewares.GetRequestToken(c))
	if err != nil {
		return nil, wrapErrors(err)
	}
	return member, nil
}

// effectiveRecipientID builds a stable, URL-safe JSON-API id for a
// recipient from its first source. The member's real identity stays
// available in the instance/email attributes: the id only needs to be
// unique and must be safe to interpolate into a URL (a raw instance URL
// or email would not be).
func effectiveRecipientID(r *sharing.EffectiveRecipient) string {
	src := r.Sources[0]
	return src.SharingID + ":" + strconv.Itoa(src.MemberIndex)
}

// checkFileInsideDrive verifies that the target belongs to the shared drive
// rooted at the drive root directory. It runs on the owner instance, where
// the ID in the sharing rule is the local ID. A target outside the drive is
// reported as not found to avoid leaking its existence.
func checkFileInsideDrive(inst *instance.Instance, s *sharing.Sharing, fileID string) error {
	rootID, err := s.DriveRootID()
	if err != nil {
		return wrapErrors(err)
	}
	if s.HasFileDriveRoot() {
		if fileID != rootID {
			return jsonapi.NotFound(errors.New("file does not belong to this drive"))
		}
		return nil
	}
	root, err := s.GetSharingDir(inst)
	if err != nil {
		return jsonapi.NotFound(errors.New("shared drive root directory not found"))
	}
	if err := isWithinDirectory(inst.VFS(), fileID, root); err != nil {
		return jsonapi.NotFound(errors.New("file does not belong to this drive"))
	}
	return nil
}
