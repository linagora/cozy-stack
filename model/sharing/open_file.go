package sharing

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cozy/cozy-stack/client/request"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/vfs"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/labstack/echo/v4"
)

// FileOpener can be used to find the parameters for opening a file (shared or
// not), when collaborative edition is possible (like for a note or an office
// document).
type FileOpener struct {
	Inst      *instance.Instance
	File      *vfs.FileDoc
	Sharing   *Sharing // can be nil
	Code      string
	ClientID  string
	MemberKey string
}

// NewFileOpener returns a FileOpener for the given file on the current instance.
func NewFileOpener(inst *instance.Instance, file *vfs.FileDoc) (*FileOpener, error) {
	// Looks if the document is shared
	opener := &FileOpener{Inst: inst, File: file}
	sharing, err := opener.getSharing(inst, file.ID())
	if err != nil {
		return nil, err
	}
	opener.Sharing = sharing
	return opener, nil
}

func (o *FileOpener) getSharing(inst *instance.Instance, fileID string) (*Sharing, error) {
	sid := consts.Files + "/" + fileID
	var ref SharedRef
	if err := couchdb.GetDoc(inst, consts.Shared, sid, &ref); err != nil {
		if couchdb.IsNotFoundError(err) {
			return nil, nil
		}
		return nil, err
	}

	for sharingID, info := range ref.Infos {
		if info.Removed {
			continue
		}
		var sharing Sharing
		if err := couchdb.GetDoc(inst, consts.Sharings, sharingID, &sharing); err != nil {
			return nil, err
		}
		if sharing.Active {
			return &sharing, nil
		}
	}
	return nil, nil
}

// AddShareByLinkCode can be used to give a sharecode that can be used to open
// the file, when the file is in a directory shared by link.
func (o *FileOpener) AddShareByLinkCode(code string) {
	o.Code = code
}

// CheckPermission takes the permission doc, and checks that the user has the
// right to open the file.
func (o *FileOpener) CheckPermission(pdoc *permission.Permission, sharingID string) error {
	// If a file is opened from a preview of a sharing, and nobody has accepted
	// the sharing until now, the io.cozy.shared document for the file has not
	// been created, and we need to fill the sharing by another way.
	if o.Sharing == nil && pdoc.Type == permission.TypeSharePreview {
		parts := strings.SplitN(pdoc.SourceID, "/", 2)
		if len(parts) != 2 {
			return ErrInvalidSharing
		}
		sharingID := parts[1]
		var sharing Sharing
		if err := couchdb.GetDoc(o.Inst, consts.Sharings, sharingID, &sharing); err != nil {
			return err
		}
		o.Sharing = &sharing
		preview, err := permission.GetForSharePreview(o.Inst, sharingID)
		if err != nil {
			return err
		}
		for k, v := range preview.Codes {
			if v == o.Code {
				o.MemberKey = k
			}
		}
	}

	// Same for shared drives
	if o.Sharing == nil && pdoc.Type == permission.TypeShareInteract {
		parts := strings.SplitN(pdoc.SourceID, "/", 2)
		if len(parts) != 2 {
			return ErrInvalidSharing
		}
		sharingID := parts[1]
		var sharing Sharing
		if err := couchdb.GetDoc(o.Inst, consts.Sharings, sharingID, &sharing); err != nil {
			return err
		}
		o.Sharing = &sharing
		preview, err := permission.GetForShareInteract(o.Inst, sharingID)
		if err != nil {
			return err
		}
		for k, v := range preview.Codes {
			if v == o.Code {
				o.MemberKey = k
			}
		}
	}

	// If a file is opened via a token for cozy-to-cozy sharing, then the file
	// must be in this sharing, or the stack should refuse to open the file.
	if sharingID != "" && o.Sharing != nil && o.Sharing.ID() == sharingID {
		o.ClientID = pdoc.SourceID
		return nil
	}

	fs := o.Inst.VFS()
	return vfs.Allows(fs, pdoc.Permissions, permission.GET, o.File)
}

// ShouldOpenLocally returns true if the file can be opened in the current
// instance, and false if it is a shared file created on another instance.
func (o *FileOpener) ShouldOpenLocally() bool {
	// For a shared drive, a file is always opened on the organization Cozy
	if o.Sharing != nil && o.Sharing.Drive {
		return o.Sharing.Owner
	}

	// Else, we try to open it where it was created
	if o.File.CozyMetadata == nil {
		return true
	}
	u, err := url.Parse(o.File.CozyMetadata.CreatedOn)
	if err != nil {
		return true
	}
	return o.Inst.HasDomain(u.Host) || o.Sharing == nil
}

// GetSharecode returns a sharecode that can be used to open the note with the
// permissions of the member.
func (o *FileOpener) GetSharecode(memberIndex int, readOnly bool) (string, error) {
	s := o.Sharing
	if s == nil || (!s.Drive && o.ClientID == "" && o.MemberKey == "") {
		return o.Code, nil
	}

	var member *Member
	var err error
	if o.MemberKey != "" {
		// Preview of a cozy-to-cozy sharing
		for i, m := range s.Members {
			if m.Instance == o.MemberKey || m.Email == o.MemberKey {
				member = &s.Members[i]
			}
		}
		if member == nil {
			return "", ErrMemberNotFound
		}
		if member.ReadOnly {
			readOnly = true
		} else {
			readOnly = s.ReadOnlyRules()
		}
	} else if s.Owner {
		member, err = s.FindMemberByInboundClientID(o.ClientID)
		if err != nil {
			return "", err
		}
		if member.ReadOnly {
			readOnly = true
		} else {
			readOnly = s.ReadOnlyRules()
		}
	} else {
		// Trust the owner
		if memberIndex < 0 || memberIndex >= len(s.Members) {
			return "", ErrMemberNotFound
		}
		member = &s.Members[memberIndex]
	}

	if readOnly {
		return o.getPreviewCode(member, memberIndex)
	}
	return o.Sharing.GetInteractCode(o.Inst, member, memberIndex)
}

// getPreviewCode returns a sharecode that can be used for reading the file. It
// uses a share-preview token.
func (o *FileOpener) getPreviewCode(member *Member, memberIndex int) (string, error) {
	preview, err := permission.GetForSharePreview(o.Inst, o.Sharing.ID())
	if err != nil {
		if couchdb.IsNotFoundError(err) {
			preview, err = o.Sharing.CreatePreviewPermissions(o.Inst)
		}
		if err != nil {
			return "", err
		}
	}

	indexKey := keyFromMemberIndex(memberIndex)
	for key, code := range preview.ShortCodes {
		if key == "" {
			continue
		}
		if key == member.Instance || key == member.Email || key == indexKey {
			return code, nil
		}
	}
	for key, code := range preview.Codes {
		if key == "" {
			continue
		}
		if key == member.Instance || key == member.Email || key == indexKey {
			return code, nil
		}
	}

	return "", ErrCannotOpenFile
}

// OpenFileParameters is the list of parameters for building the URL where the
// file can be opened in the browser.
type OpenFileParameters struct {
	FileID    string // ID of the file on the instance where the file can be edited
	Subdomain string
	Protocol  string
	Instance  string
	Sharecode string
}

// OpenLocalFile returns the parameters for opening the file on the local instance.
func (o *FileOpener) OpenLocalFile(code string) OpenFileParameters {
	params := OpenFileParameters{
		FileID:    o.File.ID(),
		Instance:  o.Inst.ContextualDomain(),
		Sharecode: code,
	}
	switch config.GetConfig().Subdomains {
	case config.FlatSubdomains:
		params.Subdomain = "flat"
	case config.NestedSubdomains:
		params.Subdomain = "nested"
	}
	params.Protocol = "https"
	if build.IsDevRelease() {
		params.Protocol = "http"
	}
	return params
}

// OpenLocalFileForMember returns the parameters for opening the file locally
// with a sharecode resolved for the member.
func (o *FileOpener) OpenLocalFileForMember(memberIndex int, readOnly bool) (OpenFileParameters, error) {
	code, err := o.GetSharecode(memberIndex, readOnly)
	if err != nil {
		return OpenFileParameters{}, err
	}
	return o.OpenLocalFile(code), nil
}

// PrepareOpenFileRequest returns the parameters for opening the file on the
// instance where collaborative editing should happen. If Opts is nil, the file
// should be opened locally with MemberIndex and ReadOnly.
func (o *FileOpener) PrepareOpenFileRequest(memberIndex int, readOnly bool) (*PreparedRequest, error) {
	if o.ShouldOpenLocally() {
		return &PreparedRequest{
			MemberIndex: memberIndex,
			ReadOnly:    readOnly,
		}, nil
	}
	return o.PrepareRequestForSharedFile()
}

// PreparedRequest contains the parameters to make a request to another
// instance for opening a shared file. If it is not possible, Opts will be
// empty and the MemberIndex and ReadOnly fields can be used for opening
// locally the file.
type PreparedRequest struct {
	Opts    *request.Options // Can be nil
	XoredID string
	Creds   *Credentials
	Creator *Member
	// MemberIndex and ReadOnly can be used even if Opts is nil
	MemberIndex int
	ReadOnly    bool
}

// PrepareRequestForSharedFile returns the parameters for making a request to
// open the shared file on another instance.
func (o *FileOpener) PrepareRequestForSharedFile() (*PreparedRequest, error) {
	s := o.Sharing
	prepared := PreparedRequest{}

	if s.Owner {
		domain := o.File.CozyMetadata.CreatedOn
		for i, m := range s.Members {
			if i == 0 {
				continue // Skip the owner
			}
			// XXX Skip the not-ready members, as an instance can be listed
			// several times in members; with different email addresses and
			// status.
			if m.Status != MemberStatusReady {
				continue
			}
			if m.Instance == domain || m.Instance+"/" == domain {
				prepared.Creds = &s.Credentials[i-1]
				prepared.Creator = &s.Members[i]
			}
		}
		if o.ClientID != "" && !prepared.ReadOnly {
			for i, c := range s.Credentials {
				if c.InboundClientID == o.ClientID {
					prepared.MemberIndex = i + 1
					prepared.ReadOnly = s.Members[i+1].ReadOnly
				}
			}
		}
	} else {
		prepared.Creds = &s.Credentials[0]
		prepared.Creator = &s.Members[0]
	}

	if prepared.Creator == nil ||
		(prepared.Creator.Status != MemberStatusReady && prepared.Creator.Status != MemberStatusOwner) {
		// If the creator of the file is no longer in the sharing, the owner of
		// the sharing takes the lead, and if the sharing is revoked, any
		// member can edit the file on their instance.
		if o.ClientID == "" {
			o.Sharing = nil
		}
		return &prepared, nil
	}

	var token string
	if s.Drive {
		prepared.XoredID = o.File.ID()
		token = prepared.Creds.DriveToken
	} else {
		prepared.XoredID = XorID(o.File.ID(), prepared.Creds.XorKey)
		token = prepared.Creds.AccessToken.AccessToken
	}

	u, err := url.Parse(prepared.Creator.Instance)
	if err != nil {
		return nil, ErrCannotOpenFile
	}
	prepared.Opts = &request.Options{
		Method: http.MethodGet,
		Scheme: u.Scheme,
		Domain: u.Host,
		Queries: url.Values{
			"SharingID":   {s.ID()},
			"MemberIndex": {strconv.FormatInt(int64(prepared.MemberIndex), 10)},
			"ReadOnly":    {strconv.FormatBool(prepared.ReadOnly)},
		},
		Headers: request.Headers{
			echo.HeaderAccept:        jsonapi.ContentType,
			echo.HeaderAuthorization: "Bearer " + token,
		},
		ParseError: ParseRequestError,
	}
	return &prepared, nil
}

// RequestSharedFile executes a prepared open request on another instance and
// refreshes the sharing token if needed.
func (o *FileOpener) RequestSharedFile(prepared *PreparedRequest, path string) (*http.Response, error) {
	prepared.Opts.Path = path
	res, err := request.Req(prepared.Opts)
	if res != nil && res.StatusCode/100 == 4 {
		res, err = RefreshToken(o.Inst, res, err, o.Sharing, prepared.Creator,
			prepared.Creds, prepared.Opts, nil)
	}
	return res, err
}
