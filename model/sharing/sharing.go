package sharing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance/lifecycle"

	"github.com/cozy/cozy-stack/client/request"
	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/bitwarden"
	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/permission"
	csettings "github.com/cozy/cozy-stack/model/settings"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/couchdb/revision"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/metadata"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/cozy/cozy-stack/pkg/utils"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/labstack/echo/v4"
)

const (
	// StateLen is the number of bytes for the OAuth state parameter
	StateLen = 16

	// DriveRootTypeDirectory means the shared drive root is a directory.
	DriveRootTypeDirectory = "directory"
	// DriveRootTypeFile means the shared drive root is a single file.
	DriveRootTypeFile = "file"

	// AccessModeAdditive is the default access mode for nested shared folders:
	// parent access applies to children, and child scopes can add access.
	AccessModeAdditive = "additive"
	// AccessModeLimitedAccess is reserved for a future feature where a child
	// folder becomes a restrictive boundary. It is stored but not activated
	// in v1.
	AccessModeLimitedAccess = "limited_access"
)

// Triggers keep record of which triggers are active
type Triggers struct {
	TrackID     string   `json:"track_id,omitempty"` // Legacy
	TrackIDs    []string `json:"track_ids,omitempty"`
	ReplicateID string   `json:"replicate_id,omitempty"`
	UploadID    string   `json:"upload_id,omitempty"`
}

// Sharing contains all the information about a sharing.
type Sharing struct {
	SID  string `json:"_id,omitempty"`
	SRev string `json:"_rev,omitempty"`

	Triggers      Triggers  `json:"triggers"`
	Drive         bool      `json:"drive,omitempty"`
	DriveRootType string    `json:"drive_root_type,omitempty"`
	OrgDrive      bool      `json:"org_drive,omitempty"` // True for drives created on an organization instance
	Active        bool      `json:"active,omitempty"`
	Owner         bool      `json:"owner,omitempty"`
	Open          bool      `json:"open_sharing,omitempty"`
	AccessMode    string    `json:"access_mode,omitempty"` // additive (default) | limited_access (reserved)
	Description   string    `json:"description,omitempty"`
	AppSlug       string    `json:"app_slug"`
	PreviewPath   string    `json:"preview_path,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	NbFiles       int       `json:"initial_number_of_files_to_sync,omitempty"`
	Initial       bool      `json:"initial_sync,omitempty"`
	ShortcutID    string    `json:"shortcut_id,omitempty"`
	MovedFrom     string    `json:"moved_from,omitempty"`

	Rules []Rule `json:"rules"`

	// Members[0] is the owner, Members[1...] are the recipients
	Members []Member `json:"members"`
	Groups  []Group  `json:"groups,omitempty"`

	// On the owner, credentials[i] is associated to members[i+1]
	// On a recipient, there is only credentials[0] (for the owner)
	Credentials []Credentials `json:"credentials,omitempty"`
}

// ID returns the sharing qualified identifier
func (s *Sharing) ID() string { return s.SID }

// Rev returns the sharing revision
func (s *Sharing) Rev() string { return s.SRev }

// DocType returns the sharing document type
func (s *Sharing) DocType() string { return consts.Sharings }

// DocReference returns the CouchDB reference for the sharing.
func (s *Sharing) DocReference() couchdb.DocReference {
	return couchdb.DocReference{
		ID:   s.SID,
		Type: s.DocType(),
	}
}

// SetID changes the sharing qualified identifier
func (s *Sharing) SetID(id string) { s.SID = id }

// SetRev changes the sharing revision
func (s *Sharing) SetRev(rev string) { s.SRev = rev }

// Clone implements couchdb.Doc
func (s *Sharing) Clone() couchdb.Doc {
	cloned := *s
	cloned.Rules = make([]Rule, len(s.Rules))
	copy(cloned.Rules, s.Rules)
	for i := range cloned.Rules {
		cloned.Rules[i].Values = make([]string, len(s.Rules[i].Values))
		copy(cloned.Rules[i].Values, s.Rules[i].Values)
	}
	cloned.Members = make([]Member, len(s.Members))
	copy(cloned.Members, s.Members)
	cloned.Credentials = make([]Credentials, len(s.Credentials))
	copy(cloned.Credentials, s.Credentials)
	for i := range s.Credentials {
		if s.Credentials[i].Client != nil {
			cloned.Credentials[i].Client = s.Credentials[i].Client.Clone()
		}
		if s.Credentials[i].AccessToken != nil {
			cloned.Credentials[i].AccessToken = s.Credentials[i].AccessToken.Clone()
		}
		cloned.Credentials[i].XorKey = make([]byte, len(s.Credentials[i].XorKey))
		copy(cloned.Credentials[i].XorKey, s.Credentials[i].XorKey)
	}
	return &cloned
}

// NormalizedDriveRootType returns the stored drive root type, defaulting
// legacy drive documents with no explicit type to a directory root.
func (s *Sharing) NormalizedDriveRootType() string {
	if s.DriveRootType == "" {
		return DriveRootTypeDirectory
	}
	return s.DriveRootType
}

// HasDirectoryDriveRoot returns true when the drive root is a directory.
func (s *Sharing) HasDirectoryDriveRoot() bool {
	return s.NormalizedDriveRootType() == DriveRootTypeDirectory
}

// HasFileDriveRoot returns true when the drive root is a single file.
func (s *Sharing) HasFileDriveRoot() bool {
	return s.NormalizedDriveRootType() == DriveRootTypeFile
}

// DriveRootID returns the identifier of the shared-drive root target.
func (s *Sharing) DriveRootID() (string, error) {
	rule := s.FirstFilesRule()
	if rule == nil || rule.Selector == couchdb.SelectorReferencedBy || len(rule.Values) == 0 {
		return "", ErrDriveRootNotFound
	}
	return rule.Values[0], nil
}

// DriveTargetURL returns the Drive URL that opens the shared drive root.
func (s *Sharing) DriveTargetURL(inst *instance.Instance) *url.URL {
	u := inst.SubDomain(consts.DriveSlug)

	rootID, err := s.DriveRootID()
	if err != nil {
		u.Fragment = "/folder/" + consts.SharedDrivesDirID
		return u
	}

	sharingID := url.PathEscape(s.SID)
	rootID = url.PathEscape(rootID)
	if s.HasFileDriveRoot() {
		u.Fragment = "/sharings/shareddrive/" + sharingID + "/file/" + rootID
	} else {
		u.Fragment = "/shareddrive/" + sharingID + "/" + rootID
	}
	return u
}

// GetFileDriveRoot returns the root file of a file-root shared drive.
func (s *Sharing) GetFileDriveRoot(inst *instance.Instance) (*vfs.FileDoc, error) {
	rootID, err := s.DriveRootID()
	if err != nil {
		return nil, err
	}
	file, err := inst.VFS().FileByID(rootID)
	if err != nil {
		return nil, ErrDriveRootNotFound
	}
	return file, nil
}

// IsValidDriveRootType returns true when the given drive root type is one of
// the supported values. The empty string is accepted for legacy documents and
// normalized to a directory root.
func IsValidDriveRootType(rootType string) bool {
	switch rootType {
	case "", DriveRootTypeDirectory, DriveRootTypeFile:
		return true
	default:
		return false
	}
}

// ReadOnlyFlag returns true only if the given instance is declared a read-only
// member of the sharing.
func (s *Sharing) ReadOnlyFlag() bool {
	if !s.Owner {
		for i, m := range s.Members {
			if i == 0 {
				continue // skip owner
			}
			if m.Instance != "" {
				return m.ReadOnly
			}
		}
	}
	return false
}

// ReadOnlyRules returns true if the rules forbid that a change on the
// recipient's cozy instance can be propagated to the sharer's cozy.
func (s *Sharing) ReadOnlyRules() bool {
	// For Drive sharings, rules are "none" but that doesn't mean read-only.
	// The member's ReadOnly flag determines access, not the rules.
	if s.Drive {
		return false
	}
	for _, rule := range s.Rules {
		if rule.HasSync() {
			return false
		}
	}
	return true
}

// ReadOnly returns true if the member has the read-only flag, or if the rules
// forces a read-only mode.
func (s *Sharing) ReadOnly() bool {
	return s.ReadOnlyFlag() || s.ReadOnlyRules()
}

// EffectiveAccessMode returns the sharing's access mode, defaulting to
// AccessModeAdditive when the field is empty for backwards compatibility.
func (s *Sharing) EffectiveAccessMode() string {
	if s.AccessMode == "" {
		return AccessModeAdditive
	}
	return s.AccessMode
}

// BeOwner initializes a sharing on the cozy of its owner
func (s *Sharing) BeOwner(inst *instance.Instance, slug string) error {
	s.Active = true
	s.Owner = true
	if s.AppSlug == "" {
		s.AppSlug = slug
	}
	if s.Drive || s.AppSlug == "" {
		s.PreviewPath = ""
	}
	s.CreatedAt = time.Now()
	s.UpdatedAt = s.CreatedAt

	name, err := csettings.PublicName(inst)
	if err != nil {
		return err
	}
	email, err := inst.SettingsEMail()
	if err != nil {
		return err
	}

	s.Members = make([]Member, 1)
	s.Members[0].Status = MemberStatusOwner
	s.Members[0].PublicName = name
	s.Members[0].Email = email
	s.Members[0].Instance = inst.PageURL("", nil)

	return nil
}

// CreatePreviewPermissions creates the permissions doc for previewing this sharing,
// or updates it with the new codes if the document already exists
func (s *Sharing) CreatePreviewPermissions(inst *instance.Instance) (*permission.Permission, error) {
	doc, _ := permission.GetForSharePreview(inst, s.SID)

	codes := make(map[string]string, len(s.Members)-1)
	shortcodes := make(map[string]string, len(s.Members)-1)

	for i, m := range s.Members {
		if i == 0 {
			continue
		}
		var err error
		var previousCode, previousShort string
		var okCode, okShort bool
		key := m.Email
		if key == "" {
			key = m.Instance
		}
		if key == "" {
			key = keyFromMemberIndex(i)
		}

		// Checks that we don't already have a sharing code
		if doc != nil {
			previousCode, okCode = doc.Codes[key]
			previousShort, okShort = doc.ShortCodes[key]
		}

		if !okCode {
			// Use a scoped subject to avoid generating identical share codes
			// for different permission types or sharings within the same second.
			codes[key], err = inst.CreateShareCode("preview:" + s.SID + ":" + key)
			if err != nil {
				return nil, err
			}
		} else {
			codes[key] = previousCode
		}
		if !okShort {
			shortcodes[key] = crypto.GenerateRandomString(consts.ShortCodeLen)
		} else {
			shortcodes[key] = previousShort
		}
	}

	set := make(permission.Set, len(s.Rules))
	getVerb := permission.VerbSplit("GET")
	for i, rule := range s.Rules {
		set[i] = permission.Rule{
			Type:     rule.DocType,
			Title:    rule.Title,
			Verbs:    getVerb,
			Selector: rule.Selector,
			Values:   rule.Values,
		}
	}

	if doc == nil {
		md := metadata.New()
		md.CreatedByApp = s.AppSlug
		subdoc := permission.Permission{
			Permissions: set,
			Metadata:    md,
		}
		return permission.CreateSharePreviewSet(inst, s.SID, codes, shortcodes, subdoc)
	}

	if doc.Metadata != nil {
		err := doc.Metadata.UpdatedByApp(s.AppSlug, "")
		if err != nil {
			return nil, err
		}
	}
	doc.Codes = codes
	doc.ShortCodes = shortcodes
	if err := couchdb.UpdateDoc(inst, doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func keyFromMemberIndex(index int) string {
	return fmt.Sprintf("index:%d", index)
}

// GetInteractCode returns a sharecode that can be used for reading and writing
// the file. It uses a share-interact token.
func (s *Sharing) GetInteractCode(inst *instance.Instance, member *Member, memberIndex int) (string, error) {
	interact, err := permission.GetForShareInteract(inst, s.ID())
	if err != nil {
		if couchdb.IsNotFoundError(err) {
			return s.createInteractPermissions(inst, member)
		}
		return "", err
	}

	// Check if the sharing has not been revoked and accepted again, in which
	// case, we need to update the permission set.
	needUpdate := false
	set := s.CreateInteractSet()
	if !set.HasSameRules(interact.Permissions) {
		interact.Permissions = set
		needUpdate = true
	}

	// If we already have a code for this member, let's use it
	indexKey := keyFromMemberIndex(memberIndex)
	for key, code := range interact.Codes {
		if key == "" {
			continue
		}
		if key == member.Instance || key == member.Email || key == indexKey {
			if needUpdate {
				_, err := permission.CreateShareInteractSet(inst, s.SID, nil, permission.Permission{
					Permissions: set,
					Metadata:    interact.Metadata,
				})
				if err != nil {
					return "", err
				}
			}
			return code, nil
		}
	}

	// Else, create a code and add it to the permission doc
	key := member.Email
	if key == "" {
		key = member.Instance
	}
	if key == "" {
		key = indexKey
	}
	// Use a scoped subject to avoid collisions with preview codes or other sharings
	code, err := inst.CreateShareCode("interact:" + s.SID + ":" + key)
	if err != nil {
		return "", err
	}

	updated, err := permission.CreateShareInteractSet(inst, s.SID, map[string]string{key: code}, permission.Permission{
		Permissions: set,
		Metadata:    interact.Metadata,
	})
	if err != nil {
		return "", err
	}
	if stored, ok := updated.Codes[key]; ok {
		return stored, nil
	}
	return "", fmt.Errorf("share-interact code for %s was not stored in sharing %s", key, s.SID)
}

func (s *Sharing) createInteractPermissions(inst *instance.Instance, m *Member) (string, error) {
	key := m.Email
	if key == "" {
		key = m.Instance
	}
	// Use a scoped subject to avoid collisions with preview codes or other sharings
	code, err := inst.CreateShareCode("interact:" + s.SID + ":" + key)
	if err != nil {
		return "", err
	}
	codes := map[string]string{key: code}
	set := s.CreateInteractSet()

	md := metadata.New()
	md.CreatedByApp = s.AppSlug
	doc := permission.Permission{
		Permissions: set,
		Metadata:    md,
	}

	interact, err := permission.CreateShareInteractSet(inst, s.SID, codes, doc)
	if err != nil {
		return "", err
	}
	if stored, ok := interact.Codes[key]; ok {
		return stored, nil
	}
	return code, nil
}

// CreateInteractSet returns a set of permissions that can be used for
// share-interact.
func (s *Sharing) CreateInteractSet() permission.Set {
	set := make(permission.Set, len(s.Rules))
	getVerb := permission.ALL
	for i, rule := range s.Rules {
		set[i] = permission.Rule{
			Type:     rule.DocType,
			Title:    rule.Title,
			Verbs:    getVerb,
			Selector: rule.Selector,
			Values:   rule.Values,
		}
	}
	return set
}

// Create checks that the sharing is OK and it persists it in CouchDB if it is the case.
func (s *Sharing) Create(inst *instance.Instance) (*permission.Permission, error) {
	if err := s.ValidateRules(); err != nil {
		return nil, err
	}
	if s.Drive {
		if !IsValidDriveRootType(s.DriveRootType) {
			return nil, ErrInvalidRule
		}
		if err := s.inferDriveRootType(inst); err != nil {
			return nil, err
		}
	}
	if !s.Drive && len(s.Members) < 2 {
		return nil, ErrNoRecipients
	}

	if err := couchdb.CreateDoc(inst, s); err != nil {
		return nil, err
	}
	if rule := s.FirstFilesRule(); rule != nil && rule.Selector != couchdb.SelectorReferencedBy {
		if err := s.AddReferenceForSharing(inst, rule); err != nil {
			inst.Logger().WithNamespace("sharing").
				Warnf("Error on referenced_by for the sharing root (%s): %s", s.SID, err)
		}
	}

	if s.Owner && s.PreviewPath != "" {
		return s.CreatePreviewPermissions(inst)
	}
	return nil, nil
}

func (s *Sharing) inferDriveRootType(inst *instance.Instance) error {
	rule := s.FirstFilesRule()
	if rule == nil || rule.Selector == couchdb.SelectorReferencedBy || len(rule.Values) == 0 {
		return ErrDriveRootNotFound
	}
	rootID := rule.Values[0]

	dir, file, err := inst.VFS().DirOrFileByID(rootID)
	if err != nil {
		return err
	}
	switch {
	case file != nil:
		s.DriveRootType = DriveRootTypeFile
		rule.Mime = file.Mime
	case dir != nil:
		s.DriveRootType = DriveRootTypeDirectory
		rule.Mime = ""
	default:
		return ErrDriveRootNotFound
	}
	return nil
}

// CreateDrive creates a new shared drive from an existing unshared folder or file.
func CreateDrive(inst *instance.Instance, rootID, description, appSlug string) (*Sharing, error) {
	dir, file, err := ValidateDriveRoot(inst, rootID)
	if err != nil {
		return nil, err
	}

	rootType := DriveRootTypeDirectory
	rootName := ""
	if dir != nil {
		rootName = dir.DocName
	}
	rootMime := ""
	if file != nil {
		rootType = DriveRootTypeFile
		rootName = file.DocName
		rootMime = file.Mime
	}

	s := &Sharing{
		Drive:         true,
		DriveRootType: rootType,
		Description:   description,
		Rules: []Rule{{
			Title:   rootName,
			DocType: consts.Files,
			Mime:    rootMime,
			Values:  []string{rootID},
			Add:     ActionRuleNone,
			Update:  ActionRuleNone,
			Remove:  ActionRuleNone,
		}},
	}

	if s.Description == "" {
		s.Description = rootName
	}

	// Initialize as owner
	if err := s.BeOwner(inst, appSlug); err != nil {
		return nil, err
	}

	return s, nil
}

// CreateRequest prepares a sharing as just a request that the user will have to
// accept before it does anything.
func (s *Sharing) CreateRequest(inst *instance.Instance) error {
	if err := s.ValidateRules(); err != nil {
		return err
	}
	if !s.Drive && len(s.Members) < 2 {
		return ErrNoRecipients
	}

	s.Active = false
	s.Owner = false
	s.UpdatedAt = time.Now()
	if !s.Drive {
		s.Credentials = make([]Credentials, 1)
	}

	for i, m := range s.Members {
		if m.Email != "" {
			if c, err := contact.FindByEmail(inst, m.Email); err == nil {
				s.Members[i].Name = c.PrimaryName()
			}
		}
	}

	err := couchdb.CreateNamedDocWithDB(inst, s)
	if couchdb.IsConflictError(err) {
		old, errb := FindSharing(inst, s.SID)
		if errb != nil {
			return errb
		}
		if old.Owner {
			return ErrInvalidSharing
		}
		if old.Active {
			return ErrAlreadyAccepted
		}
		s.ShortcutID = old.ShortcutID
		s.SRev = old.SRev
		err = couchdb.UpdateDoc(inst, s)
	}
	return err
}

// Revoke remove the credentials for all members, contact them, removes the
// triggers and set the active flag to false.
func (s *Sharing) Revoke(inst *instance.Instance) error {
	return s.revoke(inst, revokeOptions{removeRootReference: true})
}

// revokeOptions keeps the revoke steps explicit for special callers. When the
// shared root is already being trashed, the VFS update removes referenced_by,
// so revoke must not update the root document again.
type revokeOptions struct {
	removeRootReference bool
}

func (s *Sharing) revoke(inst *instance.Instance, opts revokeOptions) error {
	var errm error

	if !s.Owner {
		return ErrInvalidSharing
	}
	for i := range s.Credentials {
		if err := s.RevokeMember(inst, i+1); err != nil {
			errm = multierror.Append(errm, err)
		}
		if err := s.ClearLastSequenceNumbers(inst, &s.Members[i+1]); err != nil {
			return err
		}
	}
	if err := s.RemoveTriggers(inst); err != nil {
		return err
	}
	if err := RemoveSharedRefs(inst, s.SID); err != nil {
		return err
	}
	if opts.removeRootReference {
		if err := s.RemoveReferenceForSharing(inst, s.FirstFilesRule()); err != nil {
			return err
		}
	}
	if s.PreviewPath != "" {
		if err := s.RevokePreviewPermissions(inst); err != nil {
			return err
		}
	}
	if err := s.RevokeInteractPermissions(inst); err != nil {
		return err
	}
	if rule := s.FirstBitwardenOrganizationRule(); rule != nil && len(rule.Values) > 0 {
		if err := s.RemoveAllBitwardenMembers(inst, rule.Values[0]); err != nil {
			return err
		}
	}
	s.Active = false
	if err := couchdb.UpdateDoc(inst, s); err != nil {
		return err
	}
	if s.Drive && errm == nil {
		return s.deleteRevokedDriveSharing(inst)
	}
	return errm
}

// RevokePreviewPermissions ensure that the permissions for the preview page
// are no longer valid.
func (s *Sharing) RevokePreviewPermissions(inst *instance.Instance) error {
	perms, err := permission.GetForSharePreview(inst, s.SID)
	if err != nil {
		return err
	}
	now := time.Now()
	perms.ExpiresAt = &now
	return couchdb.UpdateDoc(inst, perms)
}

// RevokeInteractPermissions ensure that the permissions for interact tokens
// are no longer valid.
func (s *Sharing) RevokeInteractPermissions(inst *instance.Instance) error {
	perms, err := permission.GetForShareInteract(inst, s.SID)
	if err != nil {
		if couchdb.IsNotFoundError(err) {
			return nil
		}
		return err
	}
	now := time.Now()
	perms.ExpiresAt = &now
	return couchdb.UpdateDoc(inst, perms)
}

func (s *Sharing) RemoveInteractPermissionsForAMember(inst *instance.Instance, m *Member, memberIndex int) error {
	interact, err := permission.GetForShareInteract(inst, s.SID)
	if err != nil {
		if couchdb.IsNotFoundError(err) {
			return nil
		}
		return err
	}

	indexKey := keyFromMemberIndex(memberIndex)
	for key := range interact.Codes {
		if key == "" {
			continue
		}
		if key == m.Instance || key == m.Email || key == indexKey {
			delete(interact.Codes, key)
			return couchdb.UpdateDoc(inst, interact)
		}
	}
	return nil
}

// RevokeRecipient revoke only one recipient on the sharer. After that, if the
// sharing has still at least one active member, we keep it as is. Else, we
// disable the sharing.
func (s *Sharing) RevokeRecipient(inst *instance.Instance, index int) error {
	return s.revokeRecipient(inst, index, nil)
}

func (s *Sharing) revokeRecipientAfterRemovingGroup(inst *instance.Instance, index, groupIndex int) error {
	return s.revokeRecipient(inst, index, &groupIndex)
}

func (s *Sharing) revokeRecipient(inst *instance.Instance, index int, removedGroupIndex *int) error {
	if !s.Owner {
		return ErrInvalidSharing
	}
	if err := s.revokeMember(inst, index, removedGroupIndex); err != nil {
		return err
	}
	m := &s.Members[index]
	if err := s.ClearLastSequenceNumbers(inst, m); err != nil {
		return err
	}
	if rule := s.FirstBitwardenOrganizationRule(); rule != nil && len(rule.Values) > 0 {
		if err := s.RemoveBitwardenMember(inst, m, rule.Values[0]); err != nil {
			return err
		}
	}
	return s.NoMoreRecipient(inst)
}

// RevokeRecipientBySelf revoke the sharing on the recipient side
func (s *Sharing) RevokeRecipientBySelf(inst *instance.Instance, sharingDirTrashed bool) error {
	if s.Owner || len(s.Members) == 0 {
		return ErrInvalidSharing
	}
	if err := s.RevokeOwner(inst); err != nil {
		return err
	}
	if err := s.RemoveTriggers(inst); err != nil {
		return err
	}
	if err := s.ClearLastSequenceNumbers(inst, &s.Members[0]); err != nil {
		return err
	}
	if err := RemoveSharedRefs(inst, s.SID); err != nil {
		inst.Logger().WithNamespace("sharing").
			Warnf("RevokeRecipientBySelf failed to remove shared refs (%s)': %s", s.ID(), err)
	}
	if !sharingDirTrashed {
		if err := s.FixRevokedNotes(inst); err != nil {
			inst.Logger().WithNamespace("sharing").
				Warnf("RevokeRecipientBySelf failed to fix notes for revoked sharing %s: %s", s.ID(), err)
		}

		if rule := s.FirstFilesRule(); rule != nil && rule.Mime == "" {
			if err := s.RemoveSharingDir(inst); err != nil {
				inst.Logger().WithNamespace("sharing").
					Warnf("RevokeRecipientBySelf failed to delete dir %s: %s", s.ID(), err)
			}
		}
	}
	if rule := s.FirstBitwardenOrganizationRule(); rule != nil && len(rule.Values) > 0 {
		if err := s.RemoveBitwardenOrganization(inst, rule.Values[0]); err != nil {
			return err
		}
	}
	s.Active = false

	for i, m := range s.Members {
		if i > 0 && m.Instance != "" {
			s.Members[i].Status = MemberStatusRevoked
			break
		}
	}

	return couchdb.UpdateDoc(inst, s)
}

// RemoveTriggers remove all the triggers associated to this sharing
func (s *Sharing) RemoveTriggers(inst *instance.Instance) error {
	if err := removeSharingTrigger(inst, s.Triggers.TrackID); err != nil {
		return err
	}
	for _, id := range s.Triggers.TrackIDs {
		if err := removeSharingTrigger(inst, id); err != nil {
			return err
		}
	}
	if err := removeSharingTrigger(inst, s.Triggers.ReplicateID); err != nil {
		return err
	}
	if err := removeSharingTrigger(inst, s.Triggers.UploadID); err != nil {
		return err
	}
	s.Triggers = Triggers{}
	return nil
}

func removeSharingTrigger(inst *instance.Instance, triggerID string) error {
	if triggerID != "" {
		err := job.System().DeleteTrigger(inst, triggerID)
		if err != nil && !errors.Is(err, job.ErrNotFoundTrigger) {
			return err
		}
	}
	return nil
}

// RemoveBitwardenOrganization remove the shared bitwarden organization and the
// ciphers inside it. It is called on the recipient instance when the sharing
// is revoked for them.
func (s *Sharing) RemoveBitwardenOrganization(inst *instance.Instance, orgID string) error {
	org := &bitwarden.Organization{}
	if err := couchdb.GetDoc(inst, consts.BitwardenOrganizations, orgID, org); err != nil {
		if couchdb.IsNotFoundError(err) {
			return nil
		}
		return err
	}
	return org.Delete(inst)
}

// RevokeByNotification is called on the recipient side, after a revocation
// performed by the sharer
func (s *Sharing) RevokeByNotification(inst *instance.Instance) error {
	if s.Owner {
		return ErrInvalidSharing
	}
	if s.Drive {
		s.cleanShortcutID(inst)
	} else {
		if err := DeleteOAuthClient(inst, &s.Members[0], &s.Credentials[0]); err != nil {
			return err
		}
		if err := s.RemoveTriggers(inst); err != nil {
			return err
		}
		if err := s.ClearLastSequenceNumbers(inst, &s.Members[0]); err != nil {
			return err
		}
		if err := s.FixRevokedNotes(inst); err != nil {
			inst.Logger().WithNamespace("sharing").
				Warnf("RevokeByNotification failed to fix notes for revoked sharing %s: %s", s.ID(), err)
		}
		if rule := s.FirstFilesRule(); rule != nil && rule.Mime == "" {
			if err := s.RemoveSharingDir(inst); err != nil {
				return err
			}
		}
		if err := RemoveSharedRefs(inst, s.SID); err != nil {
			return err
		}
		if rule := s.FirstBitwardenOrganizationRule(); rule != nil && len(rule.Values) > 0 {
			if err := s.RemoveBitwardenOrganization(inst, rule.Values[0]); err != nil {
				return err
			}
		}
	}

	var err error
	for i := 0; i < 3; i++ {
		s.Triggers = Triggers{}
		s.Credentials = nil
		s.Active = false

		for i, m := range s.Members {
			if i > 0 && m.Instance != "" {
				s.Members[i].Status = MemberStatusRevoked
				break
			}
		}

		err := couchdb.UpdateDoc(inst, s)
		if err == nil || !couchdb.IsConflictError(err) {
			break
		}

		// In case of conflict (409 from CouchDB), reload the document and try again
		if errb := couchdb.GetDoc(inst, consts.Sharings, s.ID(), s); errb != nil {
			break
		}
	}
	return err
}

// RevokeRecipientByNotification is called on the sharer side, after a
// revocation performed by the recipient
func (s *Sharing) RevokeRecipientByNotification(inst *instance.Instance, m *Member) error {
	if !s.Owner {
		return ErrInvalidSharing
	}
	c := s.FindCredentials(m)
	if err := DeleteOAuthClient(inst, m, c); err != nil {
		return err
	}
	if err := s.ClearLastSequenceNumbers(inst, m); err != nil {
		return err
	}
	if rule := s.FirstBitwardenOrganizationRule(); rule != nil && len(rule.Values) > 0 {
		if err := s.RemoveBitwardenMember(inst, m, rule.Values[0]); err != nil {
			return err
		}
	}
	m.Status = MemberStatusRevoked
	*c = Credentials{}

	return s.NoMoreRecipient(inst)
}

// NoMoreRecipient cleans up the sharing if there is no more active recipient
func (s *Sharing) NoMoreRecipient(inst *instance.Instance) error {
	if s.Drive {
		if s.hasRemainingRecipients() {
			return couchdb.UpdateDoc(inst, s)
		}
		if err := s.RemoveTriggers(inst); err != nil {
			return err
		}
		if err := RemoveSharedRefs(inst, s.SID); err != nil {
			return err
		}
		if err := s.RemoveReferenceForSharing(inst, s.FirstFilesRule()); err != nil {
			return err
		}
		if err := s.RevokeInteractPermissions(inst); err != nil {
			return err
		}
		s.Active = false
		if err := couchdb.UpdateDoc(inst, s); err != nil {
			return err
		}
		return s.deleteRevokedDriveSharing(inst)
	}
	for _, m := range s.Members {
		if m.Status == MemberStatusReady {
			return couchdb.UpdateDoc(inst, s)
		}
	}
	if err := s.RemoveTriggers(inst); err != nil {
		return err
	}
	if err := s.RemoveReferenceForSharing(inst, s.FirstFilesRule()); err != nil {
		return err
	}
	s.Active = false
	if err := couchdb.UpdateDoc(inst, s); err != nil {
		return err
	}
	return RemoveSharedRefs(inst, s.SID)
}

func (s *Sharing) hasRemainingRecipients() bool {
	for i, m := range s.Members {
		if i > 0 && m.Status != MemberStatusRevoked {
			return true
		}
	}
	for _, g := range s.Groups {
		if !g.Revoked {
			return true
		}
	}
	return false
}

func (s *Sharing) deleteRevokedDriveSharing(inst *instance.Instance) error {
	if !s.Drive || s.OrgDrive {
		return nil
	}
	err := couchdb.DeleteDoc(inst, s)
	if couchdb.IsNotFoundError(err) {
		return nil
	}
	return err
}

// FindSharing retrieves a sharing document from its ID
func FindSharing(db prefixer.Prefixer, sharingID string) (*Sharing, error) {
	res := &Sharing{}
	err := couchdb.GetDoc(db, consts.Sharings, sharingID, res)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// FindSharings retrieves an array of sharing documents from their IDs
func FindSharings(db prefixer.Prefixer, sharingIDs []string) ([]*Sharing, error) {
	req := &couchdb.AllDocsRequest{
		Keys: sharingIDs,
	}
	var res []*Sharing
	err := couchdb.GetAllDocs(db, consts.Sharings, req, &res)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// FindActive returns the list of active sharings.
func FindActive(db prefixer.Prefixer) ([]*Sharing, error) {
	req := &couchdb.FindRequest{
		UseIndex: "active",
		Selector: mango.Equal("active", true),
		Limit:    1000,
	}
	var res []*Sharing
	err := couchdb.FindDocs(db, consts.Sharings, req, &res)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// ListDrives returns the list of the active sharings with drive: true.
func ListDrives(db prefixer.Prefixer) ([]*Sharing, error) {
	req := &couchdb.FindRequest{
		UseIndex: "active",
		Selector: mango.And(
			mango.Equal("active", true),
			mango.Equal("drive", true),
		),
		Limit: 1000,
	}
	var res []*Sharing
	err := couchdb.FindDocs(db, consts.Sharings, req, &res)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// GetSharingsByDocType returns all the sharings for the given doctype
func GetSharingsByDocType(inst *instance.Instance, docType string) (map[string]*Sharing, error) {
	req := &couchdb.ViewRequest{
		Key:         docType,
		IncludeDocs: true,
	}
	var res couchdb.ViewResponse
	err := couchdb.ExecView(inst, couchdb.SharingsByDocTypeView, req, &res)
	if err != nil {
		return nil, err
	}
	sharings := make(map[string]*Sharing, len(res.Rows))

	for _, row := range res.Rows {
		var doc Sharing
		err := json.Unmarshal(row.Doc, &doc)
		if err != nil {
			return nil, err
		}
		// Avoid duplicates, i.e. a set a rules having the same doctype
		sID := row.Value.(string)
		if _, ok := sharings[sID]; !ok {
			sharings[sID] = &doc
		}
	}
	return sharings, nil
}

func findIntentForRedirect(inst *instance.Instance, webapp *app.WebappManifest, doctype string) (*app.Intent, string) {
	action := "SHARING"
	if webapp != nil {
		if intent := webapp.FindIntent(action, doctype); intent != nil {
			return intent, webapp.Slug()
		}
	}
	var mans []app.WebappManifest
	err := couchdb.GetAllDocs(inst, consts.Apps, &couchdb.AllDocsRequest{}, &mans)
	if err != nil {
		return nil, ""
	}
	for _, man := range mans {
		if intent := man.FindIntent(action, doctype); intent != nil {
			return intent, man.Slug()
		}
	}
	return nil, ""
}

// RedirectAfterAuthorizeURL returns the URL for the redirection after a user
// has authorized a sharing.
func (s *Sharing) RedirectAfterAuthorizeURL(inst *instance.Instance) *url.URL {
	if s.Drive {
		return s.DriveTargetURL(inst)
	}

	doctype := s.Rules[0].DocType
	webapp, _ := app.GetWebappBySlug(inst, s.AppSlug)

	if intent, slug := findIntentForRedirect(inst, webapp, doctype); intent != nil {
		u := inst.SubDomain(slug)
		parts := strings.SplitN(intent.Href, "#", 2)
		if len(parts[0]) > 0 {
			u.Path = parts[0]
		}
		if len(parts) == 2 && len(parts[1]) > 0 {
			u.Fragment = parts[1]
		}
		u.RawQuery = "sharing=" + s.SID
		return u
	}

	if webapp == nil {
		return inst.DefaultRedirection()
	}
	u := inst.SubDomain(webapp.Slug())
	u.RawQuery = "sharing=" + s.SID
	return u
}

// EndInitial is used to finish the initial sync phase of a sharing
func (s *Sharing) EndInitial(inst *instance.Instance) error {
	if s.NbFiles == 0 {
		return nil
	}
	s.NbFiles = 0
	s.Initial = false
	if err := couchdb.UpdateDoc(inst, s); err != nil {
		return err
	}
	doc := couchdb.JSONDoc{
		Type: consts.SharingsInitialSync,
		M:    map[string]interface{}{"_id": s.SID},
	}
	realtime.GetHub().Publish(inst, realtime.EventDelete, &doc, nil)
	if rule := s.FirstFilesRule(); rule != nil && rule.Mime == "" {
		if _, err := s.GetSharingDir(inst); err != nil {
			return err
		}
	}
	return nil
}

// GetSharecode returns a sharecode for the given client that can be used to
// preview the sharing.
func GetSharecode(inst *instance.Instance, sharingID, clientID string) (string, error) {
	var s Sharing
	if err := couchdb.GetDoc(inst, consts.Sharings, sharingID, &s); err != nil {
		return "", err
	}
	member, err := s.FindMemberByInboundClientID(clientID)
	if err != nil {
		return "", err
	}
	preview, err := permission.GetForSharePreview(inst, sharingID)
	if err != nil {
		if couchdb.IsNotFoundError(err) {
			preview, err = s.CreatePreviewPermissions(inst)
		}
		if err != nil {
			return "", err
		}
	}

	for key, code := range preview.ShortCodes {
		if key == member.Instance || key == member.Email {
			return code, nil
		}
	}
	for key, code := range preview.Codes {
		if key == member.Instance || key == member.Email {
			return code, nil
		}
	}
	return "", ErrMemberNotFound
}

var _ couchdb.Doc = &Sharing{}

// GetSharecodeFromShortcut returns the sharecode from the shortcut for this sharing.
func (s *Sharing) GetSharecodeFromShortcut(inst *instance.Instance) (string, error) {
	key := []string{consts.Sharings, s.SID}
	end := []string{key[0], key[1], couchdb.MaxString}
	req := &couchdb.ViewRequest{
		StartKey:    key,
		EndKey:      end,
		IncludeDocs: true,
	}
	var res couchdb.ViewResponse
	err := couchdb.ExecView(inst, couchdb.FilesReferencedByView, req, &res)
	if err != nil {
		return "", ErrInternalServerError
	}
	if len(res.Rows) == 0 {
		return "", ErrInvalidSharing
	}

	fs := inst.VFS()
	file, err := fs.FileByID(res.Rows[0].ID)
	if err != nil || file.Mime != consts.ShortcutMimeType {
		return "", ErrInvalidSharing
	}
	f, err := fs.OpenFile(file)
	if err != nil {
		return "", ErrInternalServerError
	}
	defer f.Close()
	var buf bytes.Buffer
	_, err = buf.ReadFrom(f)
	if err != nil {
		return "", ErrInternalServerError
	}
	u, err := url.Parse(buf.String())
	if err != nil {
		return "", ErrInternalServerError
	}
	code := u.Query().Get("sharecode")
	if code == "" {
		return "", ErrInvalidSharing
	}
	return code, nil
}

func (s *Sharing) cleanShortcutID(inst *instance.Instance) string {
	fs := inst.VFS()
	// remove a shortcut by id, as it before
	parentID, deletedByID, err := destroyShortcutByID(fs, s.ShortcutID)
	if err != nil {
		return ""
	}
	shouldClearShortcutID := deletedByID || s.ShortcutID != ""

	// remove shortcut reference from sharing
	referencedParentID, deletedByRef, err := destroyReferencedShortcutFiles(inst, fs, s.SID)
	if err == nil && parentID == "" {
		parentID = referencedParentID
	}
	if err == nil && deletedByRef {
		shouldClearShortcutID = true
	}

	if shouldClearShortcutID {
		s.ShortcutID = ""
		_ = couchdb.UpdateDoc(inst, s)
	}
	return parentID
}

func destroyShortcutByID(fs vfs.VFS, shortcutID string) (string, bool, error) {
	if shortcutID == "" {
		return "", false, nil
	}
	file, err := fs.FileByID(shortcutID)
	if err != nil {
		return "", false, nil
	}
	if err := fs.DestroyFile(file); err != nil {
		return "", false, err
	}
	return file.DirID, true, nil
}

func destroyReferencedShortcutFiles(inst *instance.Instance, fs vfs.VFS, sharingID string) (string, bool, error) {
	key := []string{consts.Sharings, sharingID}
	req := &couchdb.ViewRequest{
		StartKey: key,
		EndKey:   []string{key[0], key[1], couchdb.MaxString},
	}
	var res couchdb.ViewResponse
	if err := couchdb.ExecView(inst, couchdb.FilesReferencedByView, req, &res); err != nil {
		return "", false, err
	}

	var parentID string
	var deleted bool
	for _, row := range res.Rows {
		file, err := fs.FileByID(row.ID)
		if err != nil || !isShortcutFile(file) {
			continue
		}
		if err := fs.DestroyFile(file); err != nil {
			return "", false, err
		}
		if parentID == "" {
			parentID = file.DirID
		}
		deleted = true
	}
	return parentID, deleted, nil
}

func isShortcutFile(file *vfs.FileDoc) bool {
	return file.Type == consts.FileType &&
		(file.Mime == consts.ShortcutMimeType || file.Class == "shortcut")
}

// GetPreviewURL asks the owner's Cozy the URL for previewing the sharing.
func (s *Sharing) GetPreviewURL(inst *instance.Instance, state string) (string, error) {
	u, err := url.Parse(s.Members[0].Instance)
	if s.Members[0].Instance == "" || err != nil {
		return "", ErrInvalidSharing
	}
	body, err := json.Marshal(map[string]interface{}{"state": state})
	if err != nil {
		return "", err
	}
	res, err := request.Req(&request.Options{
		Method: http.MethodPost,
		Scheme: u.Scheme,
		Domain: u.Host,
		Path:   "/sharings/" + s.SID + "/preview-url",
		Headers: request.Headers{
			echo.HeaderAccept:      echo.MIMEApplicationJSON,
			echo.HeaderContentType: echo.MIMEApplicationJSON,
		},
		Body: bytes.NewReader(body),
	})
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	var data map[string]interface{}
	if err = json.NewDecoder(res.Body).Decode(&data); err != nil {
		return "", ErrRequestFailed
	}

	previewURL, ok := data["url"].(string)
	if !ok || previewURL == "" {
		return "", ErrRequestFailed
	}
	return previewURL, nil
}

// PatchDescription saves a new description for a sharing.
func (s *Sharing) PatchDescription(inst *instance.Instance, description string) error {
	s.Description = description
	if s.Drive {
		if rule := s.FirstFilesRule(); rule != nil {
			rule.Title = description
		}
	}
	if err := couchdb.UpdateDoc(inst, s); err != nil {
		return err
	}
	if s.Owner {
		sharingID := s.SID
		domain := inst.Domain
		// Make replication asynchronous to avoid blocking the main request
		go func(id, dom string) {
			defer func() {
				if r := recover(); r != nil {
					logger.WithDomain(dom).WithNamespace("sharing").
						Errorf("Panic in ReplicateDescriptionChange: %v", r)
				}
			}()
			// Get a fresh instance inside the goroutine to avoid concurrent access issues
			inst, err := lifecycle.GetInstance(dom)
			if err != nil {
				logger.WithDomain(dom).WithNamespace("sharing").
					Warnf("Unable to load instance %s for sharing %s description replication: %s", dom, id, err)
				return
			}
			refreshed, err := FindSharing(inst, id)
			if err != nil {
				inst.Logger().WithNamespace("sharing").
					Warnf("Unable to load sharing %s for description replication: %s", id, err)
				return
			}
			refreshed.ReplicateDescriptionChange(inst)
		}(sharingID, domain)
	}
	return nil
}

// ReplicateDescriptionChange sends the new sharing description to the non
// revoked recipients so they can update it on their side.
func (s *Sharing) ReplicateDescriptionChange(inst *instance.Instance) {
	if !s.Owner {
		return
	}

	var errors []error
	successCount := 0
	totalRecipients := 0

	// Fetch preview codes once before the loop to avoid repeated database calls
	var previewCodes map[string]string
	perms, err := permission.GetForSharePreview(inst, s.SID)
	if err == nil && perms != nil {
		previewCodes = perms.Codes
	}

	for i, m := range s.Members {
		if !shouldReplicateDescriptionChange(i, m) {
			continue
		}
		totalRecipients++

		u, err := url.Parse(m.Instance)
		if err != nil {
			inst.Logger().WithNamespace("sharing").
				Warnf("Invalid instance URL %s: %s", m.Instance, err)
			errors = append(errors, fmt.Errorf("invalid URL for %s: %w", m.Instance, err))
			continue
		}

		c := &s.Credentials[i-1]
		var token string

		// Use the AccessToken that was exchanged during the OAuth flow
		// This token has scope io.cozy.sharings:ALL:{sharingID} which grants
		// access to replicator endpoints
		if m.Status == MemberStatusReady && c.AccessToken != nil {
			token = c.AccessToken.AccessToken
		}

		// Fall back to preview codes if we don't have an access token
		if token == "" && previewCodes != nil {
			token = previewCodes[m.Email]
		}

		if token == "" {
			inst.Logger().WithNamespace("sharing").
				Warnf("No token available for member %s", m.Instance)
			errors = append(errors, fmt.Errorf("no token available for %s", m.Instance))
			continue
		}

		// Use the replicator endpoint which accepts sharing tokens
		params := map[string]string{
			"description": s.Description,
		}
		body, err := json.Marshal(params)
		if err != nil {
			inst.Logger().WithNamespace("sharing").
				Warnf("Can't serialize the updated description for %s: %s", s.SID, err)
			errors = append(errors, fmt.Errorf("serialization failed for %s: %w", m.Instance, err))
			continue
		}

		if recipientInst := LocalInstanceFromURL(m.Instance); recipientInst != nil {
			recipientSharing, localErr := FindSharing(recipientInst, s.SID)
			if localErr == nil {
				localErr = recipientSharing.PatchDescription(recipientInst, s.Description)
			}
			if localErr == nil {
				successCount++
				continue
			}
			inst.Logger().WithNamespace("sharing").
				Warnf("Same-stack update failed for %s, falling through to HTTP: %s", m.Instance, localErr)
		}

		opts := &request.Options{
			Method: http.MethodPut,
			Scheme: u.Scheme,
			Domain: u.Host,
			Path:   "/sharings/" + s.SID + "/metadata",
			Headers: request.Headers{
				echo.HeaderContentType:   "application/json",
				echo.HeaderAuthorization: "Bearer " + token,
			},
			Body:       bytes.NewReader(body),
			ParseError: ParseRequestError,
		}

		res, err := request.Req(opts)
		if res != nil && res.StatusCode/100 == 4 {
			res, err = RefreshToken(inst, res, err, s, &s.Members[i], c, opts, body)
		}
		if err != nil {
			inst.Logger().WithNamespace("sharing").
				Warnf("Can't notify %s about the updated description: %s", m.Instance, err)
			errors = append(errors, fmt.Errorf("failed to notify %s: %w", m.Instance, err))
			continue
		}
		res.Body.Close()
		successCount++
	}

	// Log summary of replication results
	if len(errors) > 0 {
		inst.Logger().WithNamespace("sharing").
			Warnf("Description replication completed with %d/%d successes, %d failures for sharing %s",
				successCount, totalRecipients, len(errors), s.SID)
		for _, err := range errors {
			inst.Logger().WithNamespace("sharing").Debugf("Replication error: %v", err)
		}
	} else {
		inst.Logger().WithNamespace("sharing").
			Infof("Description replication successful for all %d recipients of sharing %s",
				successCount, s.SID)
	}
}

func shouldReplicateDescriptionChange(i int, m Member) bool {
	// Only notify members who have completed the OAuth flow and are ready
	return i > 0 && m.Status == MemberStatusReady && m.Instance != ""
}

// LocalInstanceFromURL returns the local instance for the given URL, or nil.
func LocalInstanceFromURL(rawURL string) *instance.Instance {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil
	}
	return LocalInstanceFromHost(u.Host)
}

// LocalInstanceFromHost returns the local instance for the given host, or nil.
// The host may include a port (e.g. "example.com:8080"). The exact host is
// tried first, then a normalized host without port is tried as a fallback.
func LocalInstanceFromHost(host string) *instance.Instance {
	if host == "" {
		return nil
	}
	if inst, err := lifecycle.GetInstance(host); err == nil {
		return inst
	}

	domain := utils.ExtractInstanceHost(host)
	if domain == "" || domain == host {
		return nil
	}
	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return nil
	}
	return inst
}

// AddShortcut creates a shortcut for this sharing on the local instance.
func (s *Sharing) AddShortcut(inst *instance.Instance, state string) error {
	if s.Drive {
		return s.CreateDriveShortcut(inst, true)
	} else {
		previewURL, err := s.GetPreviewURL(inst, state)
		if err != nil {
			return err
		}
		return s.CreateShortcut(inst, previewURL, true)
	}
}

// CountNewShortcuts returns the number of shortcuts to a sharing that have not
// been seen.
func CountNewShortcuts(inst *instance.Instance) (int, error) {
	count := 0
	perPage := 1000
	list := make([]couchdb.JSONDoc, 0, perPage)
	var bookmark string
	for {
		req := &couchdb.FindRequest{
			UseIndex: "by-sharing-status",
			Selector: mango.Equal("metadata.sharing.status", "new"),
			Limit:    perPage,
			Bookmark: bookmark,
		}
		res, err := couchdb.FindDocsRaw(inst, consts.Files, req, &list)
		if err != nil {
			return 0, err
		}
		count += len(list)
		if len(list) < perPage {
			return count, nil
		}
		bookmark = res.Bookmark
	}
}

// SendPublicKey can be used to send the public key after it has been
// created/changed to the sharing owners.
func SendPublicKey(inst *instance.Instance, publicKey string) error {
	sharings, err := GetSharingsByDocType(inst, consts.BitwardenOrganizations)
	if err != nil {
		return err
	}
	var errm error
	for _, s := range sharings {
		if s.Owner || !s.Active || s.Credentials == nil {
			continue
		}
		if err := s.sendPublicKeyToOwner(inst, publicKey); err != nil {
			errm = multierror.Append(errm, err)
		}
	}
	return errm
}

func (s *Sharing) sendPublicKeyToOwner(inst *instance.Instance, publicKey string) error {
	u, err := url.Parse(s.Members[0].Instance)
	if err != nil {
		return err
	}
	ac := APICredentials{
		Bitwarden: &APIBitwarden{
			UserID:    inst.ID(),
			PublicKey: publicKey,
		},
	}
	data, err := jsonapi.MarshalObject(&ac)
	if err != nil {
		return err
	}
	body, err := json.Marshal(jsonapi.Document{Data: &data})
	if err != nil {
		return err
	}
	opts := &request.Options{
		Method: http.MethodPost,
		Scheme: u.Scheme,
		Domain: u.Host,
		Path:   "/sharings/" + s.SID + "/public-key",
		Headers: request.Headers{
			echo.HeaderContentType:   jsonapi.ContentType,
			echo.HeaderAuthorization: "Bearer " + s.Credentials[0].AccessToken.AccessToken,
		},
		Body:       bytes.NewReader(body),
		ParseError: ParseRequestError,
	}
	res, err := request.Req(opts)
	if res != nil && res.StatusCode/100 == 4 {
		res, err = RefreshToken(inst, res, err, s, &s.Members[0], &s.Credentials[0], opts, body)
	}
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return nil
}

// CheckSharings will scan all the io.cozy.sharings documents and check their
// triggers and members/credentials.
func CheckSharings(inst *instance.Instance, skipFSConsistency bool) ([]map[string]interface{}, error) {
	checks := []map[string]interface{}{}
	err := couchdb.ForeachDocs(inst, consts.Sharings, func(_ string, data json.RawMessage) error {
		s := &Sharing{}
		if err := json.Unmarshal(data, s); err != nil {
			return err
		}

		if err := s.ValidateRules(); err != nil {
			checks = append(checks, map[string]interface{}{
				"id":    s.SID,
				"type":  "invalid_rules",
				"error": err.Error(),
			})
			return nil
		}

		accepted := false
		for _, m := range s.Members {
			if m.Status == MemberStatusReady {
				accepted = true
			}
		}

		membersChecks, validMembers := s.checkSharingMembers()
		checks = append(checks, membersChecks...)

		var triggersChecks, credentialsChecks []map[string]interface{}
		if !s.Drive {
			triggersChecks = s.checkSharingTriggers(inst, accepted)
			checks = append(checks, triggersChecks...)

			credentialsChecks = s.checkSharingCredentials()
			checks = append(checks, credentialsChecks...)
		}

		if len(membersChecks) == 0 && len(triggersChecks) == 0 && len(credentialsChecks) == 0 {
			if !s.Owner || !s.Active {
				return nil
			}

			parentSharingID, err := findParentFileSharingID(inst, s)
			if err != nil {
				return err
			} else if parentSharingID != "" {
				checks = append(checks, map[string]interface{}{
					"id":             s.SID,
					"type":           "sharing_in_sharing",
					"instance":       inst.Domain,
					"parent_sharing": parentSharingID,
				})
				return nil
			}

			if s.Initial || s.ReadOnly() {
				return nil
			}

			isSharingReady := false
			for _, m := range s.Members {
				if m.Status == MemberStatusReady {
					isSharingReady = true
					break
				}
			}
			if !isSharingReady {
				return nil
			}

			rule := s.FirstFilesRule()
			if rule == nil {
				return nil
			}

			ownerDocs, err := FindMatchingDocs(inst, *rule)
			if err != nil {
				checks = append(checks, map[string]interface{}{
					"id":    s.SID,
					"type":  "missing_matching_docs_for_owner",
					"error": err.Error(),
				})
				return nil
			}

			for _, m := range validMembers {
				ms, err := FindSharing(m, s.ID())
				if err != nil {
					checks = append(checks, map[string]interface{}{
						"id":     s.SID,
						"type":   "missing_sharing_for_member",
						"member": m.Domain,
						"error":  err.Error(),
					})
					continue
				}

				if !ms.Active {
					continue
				}

				parentSharingID, err := findParentFileSharingID(m, ms)
				if err != nil {
					return err
				} else if parentSharingID != "" {
					checks = append(checks, map[string]interface{}{
						"id":             ms.SID,
						"type":           "sharing_in_sharing",
						"instance":       m.Domain,
						"parent_sharing": parentSharingID,
					})
					continue
				}

				if !skipFSConsistency {
					checks = append(checks, s.checkSharingTreesConsistency(inst, ownerDocs, m, ms)...)
				}
			}
		}

		return nil
	})
	return checks, err
}

// findParentFileSharingID returns the first sharing found accepting the root of
// the given sharing.
//
// Since have a sharing within another one will generate unexpected behavior,
// the goal is to find these situations.
func findParentFileSharingID(inst *instance.Instance, sharing *Sharing) (string, error) {
	// Inactive sharings are not an issue so we skip them
	if !sharing.Active {
		return "", nil
	}

	// 1. Get all root files for the sharing being checked
	sharingRule := sharing.FirstFilesRule()
	if sharingRule == nil {
		return "", nil
	}

	var sharingRoots []couchdb.JSONDoc
	for _, id := range sharingRule.Values {
		var sharingRoot couchdb.JSONDoc
		if err := couchdb.GetDoc(inst, consts.Files, id, &sharingRoot); err != nil {
			// We can ignore the error here. It will be reported as
			// missing_matching_docs_for_owner or missing_matching_docs_for_member
			// later.
			return "", nil
		}
		sharingRoots = append(sharingRoots, sharingRoot)
	}

	// 2. Get all other file sharings on inst
	fileSharings, err := GetSharingsByDocType(inst, consts.Files)
	if err != nil {
		return "", err
	}

	var sharingIDs []string
	for _, fileSharing := range fileSharings {
		// Do not add sharing in sharing error for inactive sharings or the
		// sharing currently checked.
		if !fileSharing.Active || fileSharing.ID() == sharing.ID() {
			continue
		}

		sharingIDs = append(sharingIDs, fileSharing.ID())
	}

	sharedDocsBySharingID, err := GetSharedDocsBySharingIDs(inst, sharingIDs)
	if err != nil {
		return "", err
	}

	// 3. Check if one of the shared roots is part of another sharing
	for _, sharedRoot := range sharingRoots {
		for sid, sharedDocs := range sharedDocsBySharingID {
			for _, sharedDoc := range sharedDocs {
				if sharedRoot.ID() == sharedDoc.ID {
					return sid, nil
				}
			}
		}
	}

	return "", nil
}

func (s *Sharing) checkSharingTriggers(inst *instance.Instance, accepted bool) (checks []map[string]interface{}) {
	if s.Active && accepted {
		if s.Triggers.TrackID == "" && len(s.Triggers.TrackIDs) == 0 {
			checks = append(checks, map[string]interface{}{
				"id":      s.SID,
				"type":    "missing_trigger_on_active_sharing",
				"trigger": "track",
			})
		} else if s.Triggers.TrackID != "" {
			err := couchdb.GetDoc(inst, consts.Triggers, s.Triggers.TrackID, nil)
			if couchdb.IsNotFoundError(err) {
				checks = append(checks, map[string]interface{}{
					"id":         s.SID,
					"type":       "missing_trigger_on_active_sharing",
					"trigger":    "track",
					"trigger_id": s.Triggers.TrackID,
				})
			}
		} else {
			for _, id := range s.Triggers.TrackIDs {
				err := couchdb.GetDoc(inst, consts.Triggers, id, nil)
				if couchdb.IsNotFoundError(err) {
					checks = append(checks, map[string]interface{}{
						"id":         s.SID,
						"type":       "missing_trigger_on_active_sharing",
						"trigger":    "track",
						"trigger_id": id,
					})
				}
			}
		}

		if s.Owner || !s.ReadOnly() {
			if s.Triggers.ReplicateID == "" {
				checks = append(checks, map[string]interface{}{
					"id":      s.SID,
					"type":    "missing_trigger_on_active_sharing",
					"trigger": "replicate",
				})
			} else {
				err := couchdb.GetDoc(inst, consts.Triggers, s.Triggers.ReplicateID, nil)
				if couchdb.IsNotFoundError(err) {
					checks = append(checks, map[string]interface{}{
						"id":         s.SID,
						"type":       "missing_trigger_on_active_sharing",
						"trigger":    "replicate",
						"trigger_id": s.Triggers.ReplicateID,
					})
				}
			}

			if s.FirstFilesRule() != nil {
				if s.Triggers.UploadID == "" {
					checks = append(checks, map[string]interface{}{
						"id":      s.SID,
						"type":    "missing_trigger_on_active_sharing",
						"trigger": "upload",
					})
				} else {
					err := couchdb.GetDoc(inst, consts.Triggers, s.Triggers.UploadID, nil)
					if couchdb.IsNotFoundError(err) {
						checks = append(checks, map[string]interface{}{
							"id":         s.SID,
							"type":       "missing_trigger_on_active_sharing",
							"trigger":    "upload",
							"trigger_id": s.Triggers.UploadID,
						})
					}
				}
			}
		}
	} else {
		if s.Triggers.TrackID != "" || len(s.Triggers.TrackIDs) > 0 {
			id := s.Triggers.TrackID
			if id == "" {
				id = s.Triggers.TrackIDs[0]
			}
			checks = append(checks, map[string]interface{}{
				"id":         s.SID,
				"type":       "trigger_on_inactive_sharing",
				"trigger":    "track",
				"trigger_id": id,
			})
		}
		if s.Triggers.ReplicateID != "" {
			checks = append(checks, map[string]interface{}{
				"id":         s.SID,
				"type":       "trigger_on_inactive_sharing",
				"trigger":    "replicate",
				"trigger_id": s.Triggers.ReplicateID,
			})
		}
		if s.Triggers.UploadID != "" {
			checks = append(checks, map[string]interface{}{
				"id":         s.SID,
				"type":       "trigger_on_inactive_sharing",
				"trigger":    "upload",
				"trigger_id": s.Triggers.UploadID,
			})
		}
	}

	return checks
}

func (s *Sharing) checkSharingMembers() (checks []map[string]interface{}, validMembers []*instance.Instance) {
	if !s.Drive && len(s.Members) < 2 {
		checks = append(checks, map[string]interface{}{
			"id":         s.SID,
			"type":       "not_enough_members",
			"nb_members": len(s.Members),
		})
		return checks, nil
	}

	var ownerDomain string
	for i, m := range s.Members {
		if m.Status == MemberStatusRevoked && !s.Active {
			continue
		}

		isFirst := i == 0
		isOwner := m.Status == MemberStatusOwner

		if isFirst != isOwner {
			checks = append(checks, map[string]interface{}{
				"id":     s.SID,
				"type":   "invalid_member_status",
				"member": i,
				"status": m.Status,
			})
		}

		if isOwner {
			ownerDomain = strings.SplitN(m.Instance, ".", 2)[1]
		}
	}

	for _, m := range s.Members {
		if m.Status == MemberStatusMailNotSent {
			if len(m.Groups) == 0 {
				checks = append(checks, map[string]interface{}{
					"id":     s.SID,
					"type":   "mail_not_sent",
					"member": m.Instance,
				})
			}
			continue
		}

		if m.Status != MemberStatusReady {
			continue
		}

		if !s.Owner && m.Instance == "" {
			continue
		}

		u, err := url.ParseRequestURI(m.Instance)
		if err != nil {
			checks = append(checks, map[string]interface{}{
				"id":     s.SID,
				"type":   "invalid_instance_for_member",
				"member": m.Instance,
			})
			continue
		}

		domain := strings.ToLower(u.Hostname())
		if u.Port() != "" {
			domain += ":" + u.Port()
		}

		member, err := instance.Get(domain)
		if err != nil {
			// If the member's instance cannot be found and doesn't share the
			// owner's instance domain, they're probably on different
			// environments so we simply skip this member.
			if !strings.HasSuffix(m.Instance, ownerDomain) {
				continue
			}

			checks = append(checks, map[string]interface{}{
				"id":     s.SID,
				"type":   "missing_instance_for_member",
				"member": domain,
			})
			continue
		}

		validMembers = append(validMembers, member)
	}

	return checks, validMembers
}

func (s *Sharing) checkSharingCredentials() (checks []map[string]interface{}) {
	if !s.Active {
		return checks
	}

	if s.Owner {
		if len(s.Credentials)+1 != len(s.Members) {
			checks = append(checks, map[string]interface{}{
				"id":         s.SID,
				"type":       "invalid_number_of_credentials",
				"owner":      true,
				"nb_members": len(s.Credentials),
			})
			return checks
		}

		for i, m := range s.Members {
			if i == 0 || m.Status != MemberStatusReady {
				continue
			}
			if s.Credentials[i-1].Client == nil {
				checks = append(checks, map[string]interface{}{
					"id":     s.SID,
					"type":   "missing_oauth_client",
					"member": i,
					"owner":  true,
				})
			}
			if s.Credentials[i-1].AccessToken == nil {
				checks = append(checks, map[string]interface{}{
					"id":     s.SID,
					"type":   "missing_access_token",
					"member": i,
					"owner":  true,
				})
			}
			if m.Instance == "" {
				checks = append(checks, map[string]interface{}{
					"id":     s.SID,
					"type":   "missing_instance_for_member",
					"member": i,
				})
			}
		}
	} else {
		if len(s.Credentials) != 1 {
			checks = append(checks, map[string]interface{}{
				"id":         s.SID,
				"type":       "invalid_number_of_credentials",
				"owner":      false,
				"nb_members": len(s.Credentials),
			})
			return checks
		}

		if s.Credentials[0].Client == nil {
			checks = append(checks, map[string]interface{}{
				"id":    s.SID,
				"type":  "missing_oauth_client",
				"owner": false,
			})
		}

		if s.Credentials[0].AccessToken == nil {
			checks = append(checks, map[string]interface{}{
				"id":    s.SID,
				"type":  "missing_access_token",
				"owner": false,
			})
		}

		if s.Credentials[0].InboundClientID == "" {
			checks = append(checks, map[string]interface{}{
				"id":    s.SID,
				"type":  "missing_inbound_client_id",
				"owner": false,
			})
		}

		if !s.ReadOnly() && s.Members[0].Instance == "" {
			checks = append(checks, map[string]interface{}{
				"id":     s.SID,
				"type":   "missing_instance_for_member",
				"member": 0,
			})
		}
	}

	return checks
}

func (s *Sharing) checkSharingTreesConsistency(inst *instance.Instance, ownerDocs []couchdb.JSONDoc, m *instance.Instance, ms *Sharing) (checks []map[string]interface{}) {
	// We checked earlier that this rule exists
	ownerRule := s.FirstFilesRule()

	memberRule := ms.FirstFilesRule()
	if memberRule == nil {
		checks = append(checks, map[string]interface{}{
			"id":     s.SID,
			"type":   "missing_files_rule_for_member",
			"member": m.Domain,
		})
		return checks
	}

	memberDocs, err := FindMatchingDocs(m, *memberRule)
	if err != nil {
		checks = append(checks, map[string]interface{}{
			"id":     s.SID,
			"type":   "missing_matching_docs_for_member",
			"member": m.Domain,
			"error":  err.Error(),
		})
		return checks
	}

	if len(ms.Credentials) != 1 {
		checks = append(checks, map[string]interface{}{
			"id":         s.SID,
			"type":       "invalid_number_of_credentials",
			"instance":   m.Domain,
			"nb_members": len(ms.Credentials),
		})
		return checks
	}

	// Build a map of owner docs with their member's counterpart ids
	ownerKey := ms.Credentials[0].XorKey
	ownerDocsByID := make(map[string]couchdb.JSONDoc)
	for _, doc := range ownerDocs {
		ownerDocsByID[doc.ID()] = doc
	}

	for _, memberDoc := range memberDocs {
		ownerID := XorID(memberDoc.ID(), ownerKey)

		if ownerDoc, found := ownerDocsByID[ownerID]; found {
			if ownerDoc.Rev() != memberDoc.Rev() {
				if revision.Generation(ownerDoc.Rev()) < revision.Generation(memberDoc.Rev()) && ms.ReadOnly() {
					checks = append(checks, map[string]interface{}{
						"id":     s.SID,
						"type":   "read_only_member",
						"member": m.Domain,
					})
				} else if wasUpdatedRecently(ownerDoc) || wasUpdatedRecently(memberDoc) {
					// If the latest change happened less than 5 minutes ago, we'll
					// assume the sharing synchronization is still in progress and
					// that would explain the difference between the 2 revisions.
					// In this case, we do nothing.
				} else if revision.Generation(ownerDoc.Rev()) > revision.Generation(memberDoc.Rev()) && isFileTooBigForInstance(m, ownerDoc) {
					checks = append(checks, map[string]interface{}{
						"id":       s.SID,
						"type":     "disk_quota_exceeded",
						"instance": m.Domain,
						"file":     ownerDoc,
					})
				} else if revision.Generation(ownerDoc.Rev()) < revision.Generation(memberDoc.Rev()) && isFileTooBigForInstance(inst, memberDoc) {
					checks = append(checks, map[string]interface{}{
						"id":       s.SID,
						"type":     "disk_quota_exceeded",
						"instance": inst.Domain,
						"file":     memberDoc,
					})
				} else {
					checks = append(checks, map[string]interface{}{
						"id":        s.SID,
						"type":      "invalid_doc_rev",
						"member":    m.Domain,
						"ownerDoc":  ownerDoc,
						"memberRev": memberDoc.Rev(),
						"memberID":  memberDoc.ID(),
					})
				}
			} else {
				// It's unnecessary to run these checks if both docs don't
				// have the same revision in the first place.

				if ownerDoc.M["name"] != memberDoc.M["name"] {
					checks = append(checks, map[string]interface{}{
						"id":         s.SID,
						"type":       "invalid_doc_name",
						"member":     m.Domain,
						"ownerDoc":   ownerDoc,
						"memberName": memberDoc.M["name"],
						"memberRev":  memberDoc.Rev(),
						"memberID":   memberDoc.ID(),
					})
				}

				if ownerDoc.M["type"] == consts.FileType && ownerDoc.M["checksum"] != memberDoc.M["checksum"] {
					checks = append(checks, map[string]interface{}{
						"id":             s.SID,
						"type":           "invalid_doc_checksum",
						"member":         m.Domain,
						"ownerDoc":       ownerDoc,
						"memberChecksum": memberDoc.M["checksum"],
						"memberRev":      memberDoc.Rev(),
						"memberID":       memberDoc.ID(),
					})
				}

				isSharingRoot := false
				for _, v := range ownerRule.Values {
					if ownerDoc.ID() == v {
						isSharingRoot = true
						break
					}
				}

				// Sharing roots are expected not to have the same parent
				if !isSharingRoot {
					memberDirID := memberDoc.M["dir_id"].(string)
					ownerDirID := ownerDoc.M["dir_id"].(string)
					if ownerDirID != XorID(memberDirID, ownerKey) {
						checks = append(checks, map[string]interface{}{
							"id":           s.SID,
							"type":         "invalid_doc_parent",
							"member":       m.Domain,
							"ownerDoc":     ownerDoc,
							"memberParent": memberDirID,
							"memberRev":    memberDoc.Rev(),
							"memberID":     memberDoc.ID(),
						})
					}
				}
			}

			delete(ownerDocsByID, ownerID)
		} else {
			if ms.ReadOnly() {
				checks = append(checks, map[string]interface{}{
					"id":     s.SID,
					"type":   "read_only_member",
					"member": m.Domain,
				})
				continue
			}

			if wasUpdatedRecently(memberDoc) {
				// If the document was created less than 5 minutes ago, we'll
				// assume the sharing synchronization is still in progress and
				// that would explain why it's missing on the other instance.
				// In this case, we do nothing.
				continue
			}

			if isFileTooBigForInstance(inst, memberDoc) {
				checks = append(checks, map[string]interface{}{
					"id":       s.SID,
					"type":     "disk_quota_exceeded",
					"instance": inst.Domain,
					"file":     memberDoc,
				})
				continue
			}

			checks = append(checks, map[string]interface{}{
				"id":      s.SID,
				"type":    "missing_matching_doc_for_owner",
				"member":  m.Domain,
				"missing": memberDoc,
			})
		}
	}

	// The only docs left in the map do not exist on the member's instance
	for _, ownerDoc := range ownerDocsByID {
		if wasUpdatedRecently(ownerDoc) {
			// If the document was created less than 5 minutes ago, we'll
			// assume the sharing synchronization is still in progress and
			// that would explain why it's missing on the other instance.
			// In this case, we do nothing.
			continue
		}

		if isFileTooBigForInstance(m, ownerDoc) {
			checks = append(checks, map[string]interface{}{
				"id":       s.SID,
				"type":     "disk_quota_exceeded",
				"instance": m.Domain,
				"file":     ownerDoc,
			})
			break
		}

		checks = append(checks, map[string]interface{}{
			"id":         s.SID,
			"type":       "missing_matching_doc_for_member",
			"member":     m.Domain,
			"missing":    ownerDoc,
			"ownerDocID": ownerDoc.ID(),
		})
	}

	return checks
}

// isFileTooBigForInstance returns true if the given doc represents a file and
// its size is greater than the available space on the given instance.
// If said instance does not have any defined quota, it returns false.
func isFileTooBigForInstance(inst *instance.Instance, doc couchdb.JSONDoc) bool {
	if docType, ok := doc.M["type"].(string); !ok || docType == "" || docType == consts.DirType {
		return false
	}

	var file *vfs.FileDoc

	fileJSON, err := json.Marshal(doc)
	if err != nil {
		return false
	}

	if err := json.Unmarshal(fileJSON, &file); err != nil {
		return false
	}

	_, _, _, err = vfs.CheckAvailableDiskSpace(inst.VFS(), file)
	return errors.Is(err, vfs.ErrFileTooBig) || errors.Is(err, vfs.ErrMaxFileSize)
}

// wasUpdatedRecently returns true if the given document's latest update, given
// by its `cozyMetadata.updatedAt` attribute, happened less than 5 minutes ago.
// If the attribute is missing or does not represent a valid date, we consider
// the latest update happened before that.
func wasUpdatedRecently(doc couchdb.JSONDoc) bool {
	cozyMetadata, ok := doc.M["cozyMetadata"].(map[string]interface{})
	if !ok || cozyMetadata == nil {
		return false
	}
	if updatedAt, ok := cozyMetadata["updatedAt"].(time.Time); ok {
		return time.Since(updatedAt) < 5*time.Minute
	}
	return false
}
