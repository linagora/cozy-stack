package sharing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/client/auth"
	"github.com/cozy/cozy-stack/client/request"
	"github.com/cozy/cozy-stack/model/bitwarden"
	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/permission"
	csettings "github.com/cozy/cozy-stack/model/settings"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/mail"
	"github.com/cozy/cozy-stack/pkg/metadata"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/labstack/echo/v4"
)

const (
	// MemberStatusOwner is the status for the member that is owner
	MemberStatusOwner = "owner"
	// MemberStatusMailNotSent is the initial status for a recipient, before
	// the mail invitation is sent
	MemberStatusMailNotSent = "mail-not-sent"
	// MemberStatusPendingInvitation is for a recipient that has not (yet)
	// seen the preview of the sharing, but the invitation mail was sent
	MemberStatusPendingInvitation = "pending"
	// MemberStatusSeen is for a recipient that has seen the preview of the
	// sharing, but not accepted it (yet)
	MemberStatusSeen = "seen"
	// MemberStatusReady is for recipient that have accepted the sharing
	MemberStatusReady = "ready"
	// MemberStatusRevoked is for a revoked member
	MemberStatusRevoked = "revoked"
)

const maximalNumberOfMembers = 90

func maxNumberOfMembers(inst *instance.Instance) int {
	if settings, ok := inst.SettingsContext(); ok {
		if max, ok := settings["max_members_per_sharing"].(float64); ok {
			return int(max)
		}
	}
	return maximalNumberOfMembers
}

// Member contains the information about a recipient (or the sharer) for a sharing
type Member struct {
	Status       string `json:"status"`
	Name         string `json:"name,omitempty"`
	PublicName   string `json:"public_name,omitempty"`
	Email        string `json:"email,omitempty"`
	Instance     string `json:"instance,omitempty"`
	ReadOnly     bool   `json:"read_only,omitempty"`
	OnlyInGroups bool   `json:"only_in_groups,omitempty"` // False if the member has been added as an io.cozy.contacts
	Groups       []int  `json:"groups,omitempty"`         // The indexes of the groups a member is part of
}

// PrimaryName returns the main name of this member
func (m *Member) PrimaryName() string {
	if m.Name != "" {
		return m.Name
	}
	if m.PublicName != "" {
		return m.PublicName
	}
	return m.Email
}

// InstanceHost returns the domain part of the Cozy URL of the member, which
// can be used to find the instance in CouchDB. It may includes the port.
func (m *Member) InstanceHost() string {
	u, err := url.Parse(m.Instance)
	if err != nil {
		return ""
	}
	return u.Host
}

// InstanceURL returns the parsed Cozy URL of the member.
func (m *Member) InstanceURL() (*url.URL, bool) {
	if m == nil || m.Instance == "" {
		return nil, false
	}
	u, err := url.Parse(m.Instance)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, false
	}
	return u, true
}

// Same returns true if the two members are the same.
func (m *Member) Same(other Member) bool {
	return m.Name == other.Name &&
		m.PublicName == other.PublicName &&
		m.Email == other.Email &&
		m.Instance == other.Instance
}

// Credentials is the struct with the secret stuff used for authentication &
// authorization.
type Credentials struct {
	// OAuth state to accept the sharing (authorize phase)
	State string `json:"state,omitempty"`

	// Information needed to send data to the member
	Client      *auth.Client      `json:"client,omitempty"`
	AccessToken *auth.AccessToken `json:"access_token,omitempty"`
	DriveToken  string            `json:"drive_token,omitempty"`

	// XorKey is used to transform file identifiers
	XorKey []byte `json:"xor_key,omitempty"`

	// InboundClientID is the OAuth ClientID used for authentifying incoming
	// requests from the member
	InboundClientID string `json:"inbound_client_id,omitempty"`
}

// AddGroupsAndContacts adds a list of contacts on the sharer cozy
func (s *Sharing) AddGroupsAndContacts(inst *instance.Instance, groupIDs, contactIDs []string, readOnly bool) error {
	for _, id := range contactIDs {
		if err := s.AddContact(inst, id, readOnly); err != nil {
			return err
		}
	}
	for _, id := range groupIDs {
		if err := s.AddGroup(inst, id, readOnly); err != nil {
			return err
		}
	}
	var err error
	var perms *permission.Permission
	if s.PreviewPath != "" {
		if perms, err = s.CreatePreviewPermissions(inst); err != nil {
			return err
		}
	}
	_ = couchdb.UpdateDoc(inst, s)
	if err = s.SendInvitations(inst, perms); err != nil {
		return err
	}
	cloned := s.Clone().(*Sharing)
	go cloned.NotifyRecipients(inst, nil)
	return nil
}

// AddContact adds the contact with the given identifier
func (s *Sharing) AddContact(inst *instance.Instance, contactID string, readOnly bool) error {
	c, err := contact.Find(inst, contactID)
	if err != nil {
		return err
	}
	m := buildMemberFromContact(c, readOnly)
	if m.Email == "" && m.Instance == "" {
		return contact.ErrNoMailAddress
	}
	_, _, err = s.addMember(inst, m)
	return err
}

func buildMemberFromContact(c *contact.Contact, readOnly bool) Member {
	var name, email string
	cozyURL := c.PrimaryCozyURL()
	addr, err := c.ToMailAddress()
	if err == nil {
		name = addr.Name
		email = addr.Email
	} else {
		name = c.PrimaryName()
	}
	return Member{
		Status:   MemberStatusMailNotSent,
		Name:     name,
		Email:    email,
		Instance: cozyURL,
		ReadOnly: readOnly,
	}
}

// addMember adds a member to the members of the sharing if they are not yet in
// this list, and also adds credentials for them. It returns the state
// parameter of the credentials if added, and the index of the member.
func (s *Sharing) addMember(inst *instance.Instance, m Member) (string, int, error) {
	idx := -1
	for i, member := range s.Members {
		if i == 0 {
			continue // Skip the owner
		}
		var found bool
		if m.Email != "" {
			found = m.Email == member.Email
		} else if m.Instance != "" {
			found = m.Instance == member.Instance
		}
		if !found {
			continue
		}
		if s.Members[i].ReadOnly != m.ReadOnly {
			if s.Members[i].OnlyInGroups {
				return "", -1, ErrMemberAlreadyInGroup
			} else if len(m.Groups) > 0 {
				return "", -1, ErrMemberAlreadyAdded
			}
		}
		if member.Status == MemberStatusReady {
			return "", i, nil
		}
		idx = i
		s.Members[i].Status = m.Status
		s.Members[i].Name = m.Name
		s.Members[i].Instance = m.Instance
		s.Members[i].ReadOnly = m.ReadOnly
		s.Members[i].OnlyInGroups = s.Members[i].OnlyInGroups && m.OnlyInGroups
		break
	}
	if idx < 1 {
		if len(s.Members) >= maxNumberOfMembers(inst) {
			return "", -1, ErrTooManyMembers
		}
		s.Members = append(s.Members, m)
	}
	state := crypto.Base64Encode(crypto.GenerateRandomBytes(StateLen))
	creds := Credentials{
		State:  string(state),
		XorKey: MakeXorKey(),
	}
	if idx < 1 {
		s.Credentials = append(s.Credentials, creds)
		idx = len(s.Credentials)
	} else {
		s.Credentials[idx-1] = creds
	}
	return creds.State, idx, nil
}

// APIDelegateAddContacts is used to serialize a request to add contacts to
// JSON-API
type APIDelegateAddContacts struct {
	sid     string
	members []Member
	groups  []Group
}

// ID returns the sharing qualified identifier
func (a *APIDelegateAddContacts) ID() string { return a.sid }

// Rev returns the sharing revision
func (a *APIDelegateAddContacts) Rev() string { return "" }

// DocType returns the sharing document type
func (a *APIDelegateAddContacts) DocType() string { return consts.Sharings }

// SetID changes the sharing qualified identifier
func (a *APIDelegateAddContacts) SetID(id string) {}

// SetRev changes the sharing revision
func (a *APIDelegateAddContacts) SetRev(rev string) {}

// Clone is part of jsonapi.Object interface
func (a *APIDelegateAddContacts) Clone() couchdb.Doc {
	panic("APIDelegateAddContacts must not be cloned")
}

// Links is part of jsonapi.Object interface
func (a *APIDelegateAddContacts) Links() *jsonapi.LinksList { return nil }

// Included is part of jsonapi.Object interface
func (a *APIDelegateAddContacts) Included() []jsonapi.Object { return nil }

// Relationships is part of jsonapi.Object interface
func (a *APIDelegateAddContacts) Relationships() jsonapi.RelationshipMap {
	return jsonapi.RelationshipMap{
		"recipients": jsonapi.Relationship{
			Data: a.members,
		},
		"groups": jsonapi.Relationship{
			Data: a.groups,
		},
	}
}

var _ jsonapi.Object = (*APIDelegateAddContacts)(nil)

// DelegateAddContactsAndGroups adds a list of contacts and groups on a
// recipient cozy. Part of the work is delegated to owner cozy, but the
// invitation mail is still sent from the recipient cozy.
func (s *Sharing) DelegateAddContactsAndGroups(inst *instance.Instance, groupIDs, contactIDs []string, readOnly bool) error {
	api := &APIDelegateAddContacts{}
	api.sid = s.SID

	for _, id := range contactIDs {
		c, err := contact.Find(inst, id)
		if err != nil {
			return err
		}
		var name, email string
		cozyURL := c.PrimaryCozyURL()
		addr, err := c.ToMailAddress()
		if err == nil {
			name = addr.Name
			email = addr.Email
		} else {
			if cozyURL == "" {
				return err
			}
			name = c.PrimaryName()
		}
		m := Member{
			Status:   MemberStatusMailNotSent,
			Name:     name,
			Email:    email,
			Instance: cozyURL,
			ReadOnly: readOnly,
		}
		api.members = append(api.members, m)
	}

	for _, groupID := range groupIDs {
		group, err := contact.FindGroup(inst, groupID)
		if err != nil {
			return err
		}
		g := Group{
			ID:       groupID,
			Name:     group.Name(),
			Color:    group.Color(),
			ReadOnly: readOnly,
		}
		api.groups = append(api.groups, g)

		contacts, err := group.GetAllContacts(inst)
		if err != nil {
			return err
		}
		groupIndex := len(s.Groups)
		for _, contact := range contacts {
			m := buildMemberFromContact(contact, readOnly)
			m.Groups = []int{groupIndex}
			m.OnlyInGroups = true
			api.members = append(api.members, m)
		}
	}

	return s.SendDelegated(inst, api)
}

// SendDelegated calls the delegated endpoint on the sharer to adds
// contacts/groups.
func (s *Sharing) SendDelegated(inst *instance.Instance, api *APIDelegateAddContacts) error {
	data, err := jsonapi.MarshalObject(api)
	if err != nil {
		return err
	}
	body, err := json.Marshal(jsonapi.Document{Data: &data})
	if err != nil {
		return err
	}
	u, err := url.Parse(s.Members[0].Instance)
	if err != nil {
		return err
	}
	if len(s.Credentials) == 0 {
		return ErrInvalidSharing
	}
	c := &s.Credentials[0]
	if c.AccessToken == nil {
		return ErrInvalidSharing
	}
	opts := &request.Options{
		Method: http.MethodPost,
		Scheme: u.Scheme,
		Domain: u.Host,
		Path:   "/sharings/" + s.SID + "/recipients/delegated",
		Headers: request.Headers{
			echo.HeaderAccept:        echo.MIMEApplicationJSON,
			echo.HeaderContentType:   jsonapi.ContentType,
			echo.HeaderAuthorization: "Bearer " + c.AccessToken.AccessToken,
		},
		Body:       bytes.NewReader(body),
		ParseError: ParseRequestError,
	}
	res, err := request.Req(opts)
	preRefreshRes, preRefreshErr := res, err
	if shouldRetryDelegatedRequest(res) {
		res, err = RefreshToken(inst, res, err, s, &s.Members[0], c, opts, body)
	}
	if err != nil {
		return preserveDelegatedResponseError(res, err, preRefreshRes, preRefreshErr)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return ErrInternalServerError
	}
	var states map[string]string
	if err = json.NewDecoder(res.Body).Decode(&states); err != nil {
		return err
	}

	// We can have conflicts when updating the sharing document, so we are
	// retrying when it is the case.
	maxRetries := 3
	i := 0
	for {
		for _, m := range api.members {
			found := false
			for i, member := range s.Members {
				if i == 0 {
					continue // skip the owner
				}
				if m.Email == "" {
					found = m.Instance == member.Instance
				} else {
					found = m.Email == member.Email
				}
				if found && member.Status != MemberStatusReady {
					s.Members[i].Status = m.Status
					s.Members[i].Name = m.Name
					s.Members[i].Instance = m.Instance
					s.Members[i].ReadOnly = m.ReadOnly
					break
				}
			}
			if !found {
				s.Members = append(s.Members, m)
			}
		}
		if err := couchdb.UpdateDoc(inst, s); err == nil {
			break
		}
		i++
		if i > maxRetries {
			return err
		}
		time.Sleep(1 * time.Second)
		s, err = FindSharing(inst, s.SID)
		if err != nil {
			return err
		}
	}
	return s.SendInvitationsToMembers(inst, api.members, states)
}

// shouldRetryDelegatedRequest restricts replay to authentication and instance
// relocation responses. Retrying other client errors can duplicate invitations.
func shouldRetryDelegatedRequest(res *http.Response) bool {
	if res == nil {
		return false
	}
	switch res.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusGone:
		return true
	default:
		return false
	}
}

func preserveDelegatedResponseError(
	res *http.Response,
	err error,
	preRefreshRes *http.Response,
	preRefreshErr error,
) error {
	if res != nil && res.StatusCode/100 == 4 {
		return preserveDelegatedRequestError(res.StatusCode, err)
	}
	if preRefreshRes != nil && preRefreshRes.StatusCode/100 == 4 {
		return preserveDelegatedRequestError(preRefreshRes.StatusCode, preRefreshErr)
	}
	return err
}

func preserveDelegatedRequestError(status int, err error) error {
	reqErr, ok := err.(*request.Error)
	if !ok {
		return jsonapi.NewError(status, http.StatusText(status))
	}

	var payload struct {
		Errors jsonapi.ErrorList `json:"errors"`
	}
	if parseErr := json.Unmarshal([]byte(reqErr.Detail), &payload); parseErr == nil && len(payload.Errors) > 0 {
		if payload.Errors[0].Status == 0 {
			payload.Errors[0].Status = status
		}
		if payload.Errors[0].Title == "" {
			payload.Errors[0].Title = http.StatusText(status)
		}
		return payload.Errors[0]
	}

	if reqErr.Detail != "" {
		return jsonapi.NewError(status, reqErr.Detail)
	}
	return jsonapi.NewError(status, http.StatusText(status))
}

// AddDelegatedContact adds a contact on the owner cozy for a contact proposed
// by a recipient.
func (s *Sharing) AddDelegatedContact(inst *instance.Instance, m Member) (string, error) {
	m.Status = MemberStatusPendingInvitation
	if m.Instance != "" || m.Email == "" {
		m.Status = MemberStatusMailNotSent
	}
	state, _, err := s.addMember(inst, m)
	if err != nil {
		return "", err
	}
	if state == "" {
		return "", ErrAlreadyAccepted
	}
	return state, nil
}

// DelegateDiscovery delegates the POST discovery when a recipient has invited
// another person to a sharing, and this person accepts the sharing on the
// recipient cozy. The calls is delegated to the owner cozy.
func (s *Sharing) DelegateDiscovery(inst *instance.Instance, state, cozyURL, shortcut string) (string, error) {
	u, err := url.Parse(s.Members[0].Instance)
	if err != nil {
		return "", err
	}
	v := url.Values{}
	v.Add("state", state)
	v.Add("url", cozyURL)
	if shortcut != "" {
		v.Add("shortcut", shortcut)
	}
	body := []byte(v.Encode())
	if len(s.Credentials) == 0 {
		return "", ErrInvalidSharing
	}
	c := &s.Credentials[0]
	opts := &request.Options{
		Method: http.MethodPost,
		Scheme: u.Scheme,
		Domain: u.Host,
		Path:   "/sharings/" + s.SID + "/discovery",
		Headers: request.Headers{
			echo.HeaderAccept:        echo.MIMEApplicationJSON,
			echo.HeaderContentType:   echo.MIMEApplicationForm,
			echo.HeaderAuthorization: "Bearer " + c.AccessToken.AccessToken,
		},
		Body:       bytes.NewReader(body),
		ParseError: ParseRequestError,
	}
	res, err := request.Req(opts)
	if res != nil && res.StatusCode/100 == 4 {
		res, err = RefreshToken(inst, res, err, s, &s.Members[0], c, opts, body)
	}
	if err != nil {
		if res != nil && res.StatusCode == http.StatusBadRequest {
			return "", ErrInvalidURL
		}
		return "", err
	}
	defer res.Body.Close()
	var success map[string]string
	if err = json.NewDecoder(res.Body).Decode(&success); err != nil {
		return "", err
	}
	PersistInstanceURL(inst, success["email"], cozyURL)
	return success["redirect"], nil
}

// UpdateRecipients updates the lists of members and groups.
func (s *Sharing) UpdateRecipients(inst *instance.Instance, params PutRecipientsParams) error {
	for i, m := range params.Members {
		if i >= len(s.Members) {
			s.Members = append(s.Members, Member{})
		}
		if m.Email != s.Members[i].Email && m.Email != "" {
			if c, err := contact.FindByEmail(inst, m.Email); err == nil {
				s.Members[i].Name = c.PrimaryName()
			}
		}
		s.Members[i].Email = m.Email
		s.Members[i].PublicName = m.PublicName
		s.Members[i].Status = m.Status
		s.Members[i].ReadOnly = m.ReadOnly
	}
	s.Groups = params.Groups
	return couchdb.UpdateDoc(inst, s)
}

// PersistInstanceURL updates the io.cozy.contacts document with the Cozy
// instance URL, and fills the fullname if it was missing.
func PersistInstanceURL(inst *instance.Instance, email, cozyURL string) {
	if email == "" || cozyURL == "" {
		return
	}
	c, err := contact.FindByEmail(inst, email)
	if err != nil {
		return
	}
	if err := c.AddCozyURL(inst, cozyURL); err != nil {
		inst.Logger().WithNamespace("sharing").
			Warnf("Error on saving contact: %s", err)
	}
}

// FindMemberByState returns the member that is linked to the sharing by
// the given state
func (s *Sharing) FindMemberByState(state string) (*Member, error) {
	if !s.Owner {
		return nil, ErrInvalidSharing
	}
	for i, c := range s.Credentials {
		if c.State == state {
			if len(s.Members) <= i+1 {
				return nil, ErrInvalidSharing
			}
			return &s.Members[i+1], nil
		}
	}
	return nil, ErrMemberNotFound
}

// MemberFor returns the member of the sharing that corresponds to the given
// instance (the owner entry on the owner's Cozy, the matching recipient
// entry on a recipient's Cozy), or nil if the instance is not a member.
// Revoked members are not returned.
func (s *Sharing) MemberFor(inst *instance.Instance) *Member {
	if s.Owner {
		if len(s.Members) == 0 {
			return nil
		}
		return &s.Members[0]
	}
	currDomain := inst.Domain
	if i := strings.IndexByte(currDomain, ':'); i >= 0 {
		currDomain = currDomain[:i]
	}
	currEmail, _ := inst.SettingsEMail()
	for i := range s.Members {
		m := &s.Members[i]
		if m.Status == MemberStatusRevoked {
			continue
		}
		memberHost := m.InstanceHost()
		if memberHost == "" {
			continue
		}
		if memberHost == inst.Domain || memberHost == currDomain {
			return m
		}
		if currEmail != "" && m.Email == currEmail {
			return m
		}
	}
	return nil
}

// FindMemberBySharecode returns the member that is linked to the sharing by
// the given sharecode
func (s *Sharing) FindMemberBySharecode(db prefixer.Prefixer, sharecode string) (*Member, error) {
	perms, err := permission.GetForSharePreview(db, s.SID)
	if err != nil {
		return nil, err
	}
	return s.FindMemberByCode(perms, sharecode)
}

// FindMemberByInteractCode returns the member that is linked to the sharing by
// the given sharecode via a share-interact permission.
func (s *Sharing) FindMemberByInteractCode(db prefixer.Prefixer, sharecode string) (*Member, error) {
	perms, err := permission.GetForShareInteract(db, s.SID)
	if err != nil {
		return nil, err
	}
	return s.FindMemberByCode(perms, sharecode)
}

// FindMemberByCode returns the member that is linked to the sharing by the
// given code.
func (s *Sharing) FindMemberByCode(perms *permission.Permission, sharecode string) (*Member, error) {
	var emailOrInstance string
	for e, code := range perms.Codes {
		if code == sharecode {
			emailOrInstance = e
			break
		}
	}
	if emailOrInstance == "" {
		for e, code := range perms.ShortCodes {
			if code == sharecode {
				emailOrInstance = e
				break
			}
		}
	}
	if emailOrInstance == "" {
		return nil, ErrMemberNotFound
	}

	for i, m := range s.Members {
		if m.Email == emailOrInstance {
			return &s.Members[i], nil
		}
	}
	for i, m := range s.Members {
		if m.Instance == emailOrInstance {
			return &s.Members[i], nil
		}
	}
	if strings.HasPrefix(emailOrInstance, "index:") {
		i, err := strconv.Atoi(strings.TrimPrefix(emailOrInstance, "index:"))
		if err == nil && i > 0 && i < len(s.Members) {
			return &s.Members[i], nil
		}
	}
	return nil, ErrMemberNotFound
}

// FindMemberByInboundClientID returns the member that have used this client
// ID to make a request on the given sharing
func (s *Sharing) FindMemberByInboundClientID(clientID string) (*Member, error) {
	if s.Owner {
		for i, c := range s.Credentials {
			if c.InboundClientID == clientID {
				return &s.Members[i+1], nil
			}
		}
		return nil, ErrMemberNotFound
	}

	if s.Credentials[0].InboundClientID == clientID {
		return &s.Members[0], nil
	}
	return nil, ErrMemberNotFound
}

// FindCredentials returns the credentials for the given member
func (s *Sharing) FindCredentials(m *Member) *Credentials {
	if len(s.Credentials) == 0 {
		return nil
	}

	if s.Owner {
		for i, member := range s.Members {
			if i > 0 && m.Same(member) {
				return &s.Credentials[i-1]
			}
		}
		return nil
	}

	if m.Same(s.Members[0]) {
		return &s.Credentials[0]
	}
	return nil
}

// Refresh will refresh the access token, and persist the new access token in
// the sharing
func (c *Credentials) Refresh(inst *instance.Instance, s *Sharing, m *Member) error {
	if c.Client == nil || c.AccessToken == nil {
		return ErrNoOAuthClient
	}
	u, ok := m.InstanceURL()
	if !ok {
		return ErrInvalidSharing
	}
	r := &auth.Request{
		Scheme: u.Scheme,
		Domain: u.Host,
	}
	token, err := r.RefreshToken(c.Client, c.AccessToken)
	if err != nil {
		return err
	}
	c.AccessToken.AccessToken = token.AccessToken
	// Also update the refresh token if a new one is provided (e.g., when the
	// instance domain has changed and a new refresh token is issued)
	if token.RefreshToken != "" {
		c.AccessToken.RefreshToken = token.RefreshToken
	}
	if err = couchdb.UpdateDoc(inst, s); err != nil && !couchdb.IsConflictError(err) {
		return err
	}
	return nil
}

// requireOAuthCredentials checks that an incoming credentials payload carries
// the embedded credentials, the OAuth client, and a non-empty access token —
// the fields that downstream code dereferences without further guards.
func requireOAuthCredentials(creds *APICredentials) error {
	if creds == nil || creds.Credentials == nil ||
		creds.Client == nil ||
		creds.AccessToken == nil ||
		creds.AccessToken.AccessToken == "" {
		return ErrInvalidSharing
	}
	return nil
}

// recipientTarget returns what the owner needs to call a recipient:
// the recipient member, the credentials to authenticate with, and the recipient's URL.
// It validates everything up front so callers don't hit nil pointer panics later.
func (s *Sharing) recipientTarget(index int) (*Member, *Credentials, *url.URL, error) {
	if index <= 0 || index >= len(s.Members) || index-1 >= len(s.Credentials) {
		return nil, nil, nil, ErrMemberNotFound
	}
	m := &s.Members[index]
	u, ok := m.InstanceURL()
	if !ok {
		return nil, nil, nil, ErrInvalidSharing
	}
	c := &s.Credentials[index-1]
	if c.AccessToken == nil {
		return nil, nil, nil, ErrInvalidSharing
	}
	return m, c, u, nil
}

func (s *Sharing) shouldUpdateReadOnlyFlagWithoutRecipientSync(index int) bool {
	switch s.Members[index].Status {
	case MemberStatusMailNotSent, MemberStatusPendingInvitation, MemberStatusSeen:
		return true
	default:
		return false
	}
}

// ownerTarget returns what a recipient needs to call back to the owner:
// the owner member, the credentials to authenticate with, and the owner's URL.
func (s *Sharing) ownerTarget(index int) (*Member, *Credentials, *url.URL, error) {
	if index <= 0 || index >= len(s.Members) {
		return nil, nil, nil, ErrMemberNotFound
	}
	if len(s.Credentials) != 1 {
		return nil, nil, nil, ErrInvalidSharing
	}
	m := &s.Members[0]
	u, ok := m.InstanceURL()
	if !ok {
		return nil, nil, nil, ErrInvalidSharing
	}
	c := &s.Credentials[0]
	if c.AccessToken == nil {
		return nil, nil, nil, ErrInvalidSharing
	}
	return m, c, u, nil
}

// AddReadOnlyFlag adds the read-only flag of a recipient, and send
// an access token with a short validity to let it synchronize its last
// changes.
func (s *Sharing) AddReadOnlyFlag(inst *instance.Instance, index int) error {
	if index <= 0 || index >= len(s.Members) {
		return ErrMemberNotFound
	}
	if s.ReadOnlyFlag() {
		return ErrInvalidSharing
	}
	if s.Members[index].ReadOnly {
		return nil
	}
	if s.shouldUpdateReadOnlyFlagWithoutRecipientSync(index) {
		s.Members[index].ReadOnly = true
		return couchdb.UpdateDoc(inst, s)
	}
	m, c, u, err := s.recipientTarget(index)
	if err != nil {
		return err
	}

	ac := APICredentials{
		CID:         s.SID,
		Credentials: &Credentials{},
	}
	// We can't just revoke the tokens for the recipient (they are persisted
	// on this cozy), so we have to revoke the client. And we create a new
	// client for the temporary access token used to synchronize the last
	// changes (the recipient won't have a refresh token).
	cli, err := CreateOAuthClient(inst, m)
	if err != nil {
		return err
	}
	c.InboundClientID = cli.ClientID
	ac.Credentials.Client = ConvertOAuthClient(cli)
	scope := consts.Sharings + ":ALL:" + s.SID
	issuedAt := time.Now().Add(1*time.Hour - consts.AccessTokenValidityDuration)
	access, err := inst.MakeJWT(consts.AccessTokenAudience, cli.ClientID, scope, "", issuedAt)
	if err != nil {
		return err
	}
	ac.Credentials.AccessToken = &auth.AccessToken{
		TokenType:   "bearer",
		AccessToken: access,
		// No refresh token
		Scope: scope,
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
		Path:   "/sharings/" + s.SID + "/recipients/self/readonly",
		Headers: request.Headers{
			echo.HeaderAccept:        jsonapi.ContentType,
			echo.HeaderContentType:   jsonapi.ContentType,
			echo.HeaderAuthorization: "Bearer " + c.AccessToken.AccessToken,
		},
		Body:       bytes.NewReader(body),
		ParseError: ParseRequestError,
	}
	res, err := request.Req(opts)
	if res != nil && res.StatusCode/100 == 4 {
		res, err = RefreshToken(inst, res, err, s, m, c, opts, body)
	}
	if err != nil {
		if res != nil {
			return ErrRequestFailed
		}
		return err
	}
	defer res.Body.Close()

	m.ReadOnly = true
	return couchdb.UpdateDoc(inst, s)
}

// DelegateAddReadOnlyFlag is used by a recipient to ask the sharer to
// add the read-only falg for another member of the sharing.
func (s *Sharing) DelegateAddReadOnlyFlag(inst *instance.Instance, index int) error {
	m, c, u, err := s.ownerTarget(index)
	if err != nil {
		return err
	}
	opts := &request.Options{
		Method: http.MethodPost,
		Scheme: u.Scheme,
		Domain: u.Host,
		Path:   fmt.Sprintf("/sharings/%s/recipients/%d/readonly", s.SID, index),
		Headers: request.Headers{
			echo.HeaderAuthorization: "Bearer " + c.AccessToken.AccessToken,
		},
		ParseError: ParseRequestError,
	}
	res, err := request.Req(opts)
	if res != nil && res.StatusCode/100 == 4 {
		res, err = RefreshToken(inst, res, err, s, m, c, opts, nil)
	}
	if err != nil {
		if res != nil && res.StatusCode == http.StatusBadRequest {
			return ErrInvalidURL
		}
		return err
	}
	res.Body.Close()
	return nil
}

// DowngradeToReadOnly is used to receive credentials on a read-write instance
// to sync the last changes before going to read-only mode.
func (s *Sharing) DowngradeToReadOnly(inst *instance.Instance, creds *APICredentials) error {
	if s.Owner || len(s.Credentials) != 1 {
		return ErrInvalidSharing
	}
	if err := requireOAuthCredentials(creds); err != nil {
		return err
	}

	for i, m := range s.Members {
		if i > 0 && m.Instance != "" {
			s.Members[i].ReadOnly = true
			break
		}
	}

	s.Credentials[0].AccessToken = creds.AccessToken
	s.Credentials[0].Client = creds.Client

	if err := removeSharingTrigger(inst, s.Triggers.ReplicateID); err != nil {
		return err
	}
	s.Triggers.ReplicateID = ""
	if err := removeSharingTrigger(inst, s.Triggers.UploadID); err != nil {
		return err
	}
	s.Triggers.UploadID = ""

	if err := couchdb.UpdateDoc(inst, s); err != nil {
		return err
	}

	s.pushJob(inst, "share-replicate")
	if s.FirstFilesRule() != nil {
		s.pushJob(inst, "share-upload")
	}
	return nil
}

// RemoveReadOnlyFlag removes the read-only flag of a recipient, and send
// credentials to their cozy so that it can push its changes.
func (s *Sharing) RemoveReadOnlyFlag(inst *instance.Instance, index int) error {
	if index <= 0 || index >= len(s.Members) {
		return ErrMemberNotFound
	}
	if s.ReadOnlyFlag() {
		return ErrInvalidSharing
	}
	if !s.Members[index].ReadOnly {
		return nil
	}
	if s.shouldUpdateReadOnlyFlagWithoutRecipientSync(index) {
		s.Members[index].ReadOnly = false
		return couchdb.UpdateDoc(inst, s)
	}
	m, c, u, err := s.recipientTarget(index)
	if err != nil {
		return err
	}

	ac := APICredentials{
		CID: s.SID,
		Credentials: &Credentials{
			XorKey: c.XorKey,
		},
	}
	// Create the credentials for the recipient
	cli, err := CreateOAuthClient(inst, m)
	if err != nil {
		return err
	}
	c.InboundClientID = cli.ClientID
	ac.Credentials.Client = ConvertOAuthClient(cli)
	token, err := CreateAccessToken(inst, cli, s.SID, permission.ALL)
	if err != nil {
		return err
	}
	ac.Credentials.AccessToken = token

	data, err := jsonapi.MarshalObject(&ac)
	if err != nil {
		return err
	}
	body, err := json.Marshal(jsonapi.Document{Data: &data})
	if err != nil {
		return err
	}
	opts := &request.Options{
		Method: http.MethodDelete,
		Scheme: u.Scheme,
		Domain: u.Host,
		Path:   "/sharings/" + s.SID + "/recipients/self/readonly",
		Headers: request.Headers{
			echo.HeaderAccept:        jsonapi.ContentType,
			echo.HeaderContentType:   jsonapi.ContentType,
			echo.HeaderAuthorization: "Bearer " + c.AccessToken.AccessToken,
		},
		Body:       bytes.NewReader(body),
		ParseError: ParseRequestError,
	}
	res, err := request.Req(opts)
	if res != nil && res.StatusCode/100 == 4 {
		res, err = RefreshToken(inst, res, err, s, m, c, opts, body)
	}
	if err != nil {
		return err
	}
	res.Body.Close()
	m.ReadOnly = false
	return couchdb.UpdateDoc(inst, s)
}

// UpgradeToReadWrite is used to receive credentials on a read-only instance
// to upgrade it to read-write.
func (s *Sharing) UpgradeToReadWrite(inst *instance.Instance, creds *APICredentials) error {
	if s.Owner || len(s.Credentials) != 1 {
		return ErrInvalidSharing
	}
	if err := requireOAuthCredentials(creds); err != nil {
		return err
	}

	for i, m := range s.Members {
		if i > 0 && m.Instance != "" {
			s.Members[i].ReadOnly = false
			break
		}
	}

	if err := s.SetupReceiver(inst); err != nil {
		return err
	}

	s.Credentials[0].XorKey = creds.XorKey
	s.Credentials[0].AccessToken = creds.AccessToken
	s.Credentials[0].Client = creds.Client
	return couchdb.UpdateDoc(inst, s)
}

// DelegateRemoveReadOnlyFlag is used by a recipient to ask the sharer to
// remove the read-only falg for another member of the sharing.
func (s *Sharing) DelegateRemoveReadOnlyFlag(inst *instance.Instance, index int) error {
	m, c, u, err := s.ownerTarget(index)
	if err != nil {
		return err
	}
	opts := &request.Options{
		Method: http.MethodDelete,
		Scheme: u.Scheme,
		Domain: u.Host,
		Path:   fmt.Sprintf("/sharings/%s/recipients/%d/readonly", s.SID, index),
		Headers: request.Headers{
			echo.HeaderAuthorization: "Bearer " + c.AccessToken.AccessToken,
		},
		ParseError: ParseRequestError,
	}
	res, err := request.Req(opts)
	if res != nil && res.StatusCode/100 == 4 {
		res, err = RefreshToken(inst, res, err, s, m, c, opts, nil)
	}
	if err != nil {
		if res != nil && res.StatusCode == http.StatusBadRequest {
			return ErrInvalidURL
		}
		return err
	}
	res.Body.Close()
	return nil
}

// DelegateRevokeRecipient is used by a recipient to ask the sharer to revoke
// another member of the sharing.
func (s *Sharing) DelegateRevokeRecipient(inst *instance.Instance, index int) error {
	u, err := url.Parse(s.Members[0].Instance)
	if err != nil {
		return err
	}
	if len(s.Credentials) == 0 {
		return ErrInvalidSharing
	}
	c := &s.Credentials[0]
	if c.AccessToken == nil {
		return ErrInvalidSharing
	}
	opts := &request.Options{
		Method: http.MethodDelete,
		Scheme: u.Scheme,
		Domain: u.Host,
		Path:   fmt.Sprintf("/sharings/%s/recipients/%d", s.SID, index),
		Headers: request.Headers{
			echo.HeaderAuthorization: "Bearer " + c.AccessToken.AccessToken,
		},
		ParseError: ParseRequestError,
	}
	res, err := request.Req(opts)
	preRefreshRes, preRefreshErr := res, err
	if res != nil && res.StatusCode/100 == 4 {
		res, err = RefreshToken(inst, res, err, s, &s.Members[0], c, opts, nil)
	}
	if err != nil {
		if preRefreshRes != nil && preRefreshRes.StatusCode == http.StatusForbidden {
			return preserveDelegatedRequestError(preRefreshRes.StatusCode, preRefreshErr)
		}
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		return ErrInternalServerError
	}
	return nil
}

// RevokeMember revoke the access granted to a member and contact it
func (s *Sharing) RevokeMember(inst *instance.Instance, index int) error {
	return s.revokeMember(inst, index, nil)
}

func (s *Sharing) revokeMember(inst *instance.Instance, index int, removedGroupIndex *int) error {
	m := &s.Members[index]

	// skip if member is already revoked
	if m.Status == MemberStatusRevoked {
		return nil
	}

	c := &s.Credentials[index-1]

	// No need to contact the revoked member if the sharing is not ready
	if m.Status == MemberStatusReady || s.Drive {
		if err := s.NotifyMemberRevocation(inst, m, c); err != nil {
			inst.Logger().WithNamespace("sharing").
				Warnf("Error on revocation notification: %s", err)
		}
	}

	if m.Status == MemberStatusReady {
		if err := DeleteOAuthClient(inst, m, c); err != nil {
			return err
		}
	}

	if err := s.RemoveInteractPermissionsForAMember(inst, m, index); err != nil {
		return err
	}

	// We may have concurrency issues where RevokeMember is called several
	// times on different goroutines/processes. So, we may need to retry the
	// operation several times.
	leftRetries := 3
	for {
		if removedGroupIndex != nil {
			removeGroupFromMember(m, *removedGroupIndex)
		}
		m.Status = MemberStatusRevoked
		// Do not remove the credentials from the array to preserve the members /
		// credentials order, just empty them
		*c = Credentials{}

		err := couchdb.UpdateDoc(inst, s)
		if !couchdb.IsConflictError(err) || leftRetries == 0 {
			return err
		}

		if errg := couchdb.GetDoc(inst, consts.Sharings, s.SID, s); errg != nil {
			return err
		}
		m = &s.Members[index]
		c = &s.Credentials[index-1]
		leftRetries--
	}
}

// RevokeOwner revoke the access granted to the owner and notify it
func (s *Sharing) RevokeOwner(inst *instance.Instance) error {
	if s.Credentials == nil { // Already revoked
		return nil
	}

	m := &s.Members[0]
	c := &s.Credentials[0]

	if err := s.NotifyMemberRevocation(inst, m, c); err != nil {
		inst.Logger().WithNamespace("sharing").
			Warnf("Error on revocation notification: %s", err)
	}
	if err := DeleteOAuthClient(inst, m, c); err != nil {
		return err
	}
	m.Status = MemberStatusRevoked
	s.Credentials = nil
	s.Active = false
	return couchdb.UpdateDoc(inst, s)
}

// NotifyMemberRevocation send a notification to this member that he/she was
// revoked from this sharing
func (s *Sharing) NotifyMemberRevocation(inst *instance.Instance, m *Member, c *Credentials) error {
	u, err := url.Parse(m.Instance)
	if m.Instance == "" || err != nil {
		return ErrInvalidSharing
	}
	var token string
	if s.Drive {
		if m.Status == MemberStatusOwner {
			// Recipient notifying owner - use DriveToken from credentials
			if c == nil || c.DriveToken == "" {
				return ErrNoOAuthClient
			}
			token = c.DriveToken
		} else {
			// Owner notifying recipient - use interact codes
			interact, err := permission.GetForShareInteract(inst, s.ID())
			if err != nil {
				return err
			}
			if code, ok := interact.Codes[m.Instance]; ok {
				token = code
			} else if code, ok := interact.Codes[m.Email]; ok {
				token = code
			}
		}
	} else {
		if c.Client == nil || c.AccessToken == nil {
			return ErrNoOAuthClient
		}
		token = c.AccessToken.AccessToken
	}

	var path string
	if m.Status == MemberStatusOwner {
		path = "/sharings/" + s.SID + "/answer"
	} else {
		path = "/sharings/" + s.SID
	}

	opts := &request.Options{
		Method: http.MethodDelete,
		Scheme: u.Scheme,
		Domain: u.Host,
		Path:   path,
		Headers: request.Headers{
			echo.HeaderAuthorization: "Bearer " + token,
		},
		ParseError: ParseRequestError,
	}
	res, err := request.Req(opts)
	if res != nil && res.StatusCode/100 == 4 {
		res, err = RefreshToken(inst, res, err, s, m, c, opts, nil)
	}
	if err != nil {
		if res != nil && res.StatusCode/100 == 5 {
			return ErrInternalServerError
		}
		return err
	}
	res.Body.Close()
	return nil
}

// PutRecipientsParams is the body of the request for updating the list of
// members and groups on the active recipients of a sharing.
type PutRecipientsParams struct {
	Members []Member `json:"data"`
	Groups  []Group  `json:"included"`
}

// NotifyRecipients will push the updated list of members of the sharing to the
// active recipients. It is meant to be used in a goroutine, errors are just
// logged (nothing critical here).
func (s *Sharing) NotifyRecipients(inst *instance.Instance, except *Member) {
	if !s.Owner {
		return
	}
	if len(s.Members) != len(s.Credentials)+1 {
		return
	}

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
			log := inst.Logger().WithNamespace("sharing").WithField("panic", true)
			log.Errorf("PANIC RECOVER %s: %s", err.Error(), stack[:length])
		}
	}()

	// XXX Wait a bit to avoid pressure on recipients cozy after delegated operations
	time.Sleep(3 * time.Second)

	active := false
	for i := range s.Members {
		if shouldNotifyMember(s, i, except) {
			active = true
			break
		}
	}
	if !active {
		return
	}

	var params PutRecipientsParams
	params.Members = make([]Member, len(s.Members))
	for i, m := range s.Members {
		params.Members[i] = Member{
			Status:     m.Status,
			PublicName: m.PublicName,
			Email:      m.Email,
			ReadOnly:   m.ReadOnly,
			// Instance and name are private
		}
	}
	params.Groups = make([]Group, len(s.Groups))
	copy(params.Groups, s.Groups)
	body, err := json.Marshal(params)
	if err != nil {
		inst.Logger().WithNamespace("sharing").
			Warnf("Can't serialize the updated members list for %s: %s", s.SID, err)
		return
	}

	for i, m := range s.Members {
		if !shouldNotifyMember(s, i, except) {
			continue
		}
		u, err := url.Parse(m.Instance)
		if m.Instance == "" || err != nil {
			inst.Logger().WithNamespace("sharing").
				Infof("Invalid instance URL %s: %s", m.Instance, err)
			continue
		}
		c := &s.Credentials[i-1]
		var token string
		if m.Status == MemberStatusReady {
			token = c.AccessToken.AccessToken
		} else {
			perms, err := permission.GetForSharePreview(inst, s.SID)
			if err == nil {
				token = perms.Codes[m.Email]
			}
			if token == "" {
				continue
			}
		}
		opts := &request.Options{
			Method: http.MethodPut,
			Scheme: u.Scheme,
			Domain: u.Host,
			Path:   "/sharings/" + s.SID + "/recipients",
			Headers: request.Headers{
				echo.HeaderAccept:        jsonapi.ContentType,
				echo.HeaderContentType:   jsonapi.ContentType,
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
				Debugf("Can't notify %#v about the updated members list: %s", m, err)
			continue
		}
		res.Body.Close()
	}
}

func shouldNotifyMember(s *Sharing, i int, except *Member) bool {
	if i == 0 { // Don't notify the owner
		return false
	}
	if &s.Members[i] == except {
		return false
	}
	m := s.Members[i]
	if m.Status == MemberStatusReady {
		return true
	}
	if m.Status != MemberStatusPendingInvitation && m.Status != MemberStatusSeen {
		return false
	}
	return m.Instance != ""
}

// SaveBitwarden adds the sharing member to the bitwarden organization in the
// sharing rules.
func (s *Sharing) SaveBitwarden(inst *instance.Instance, m *Member, bw *APIBitwarden) error {
	rule := s.FirstBitwardenOrganizationRule()
	if rule == nil || len(rule.Values) == 0 {
		return nil
	}

	org := &bitwarden.Organization{}
	if err := couchdb.GetDoc(inst, consts.BitwardenOrganizations, rule.Values[0], org); err != nil {
		return err
	}
	domain := m.Instance
	if u, err := url.Parse(m.Instance); err == nil {
		domain = u.Host
	}
	orgKey := org.Members[domain].OrgKey
	if orgKey == "" && bw.UserID != "" {
		// The member can have been migrated to a new domain: its entry is
		// still stored under its former domain. The user keeps the same RSA
		// key pair, so the organization key can be reused. The entry of the
		// former domain is left as is, and Organization.Member gives the
		// precedence to the entry that has the key.
		for former, member := range org.Members {
			if former != domain && member.UserID == bw.UserID {
				orgKey = member.OrgKey
				break
			}
		}
	}

	if bw.PublicKey != "" {
		contact := &bitwarden.Contact{}
		err := couchdb.GetDoc(inst, consts.BitwardenContacts, bw.UserID, contact)
		if couchdb.IsNotFoundError(err) {
			md := metadata.New()
			md.DocTypeVersion = bitwarden.DocTypeVersion
			contact.UserID = bw.UserID
			contact.Email = m.Email
			contact.PublicKey = bw.PublicKey
			contact.Metadata = *md
			err = couchdb.CreateNamedDocWithDB(inst, contact)
		}
		if err != nil {
			return err
		}
		// The public key can have been changed if the member has reset their password
		if contact.PublicKey != bw.PublicKey {
			contact.UserID = bw.UserID
			contact.PublicKey = bw.PublicKey
			contact.Confirmed = false
			contact.Metadata.UpdatedAt = time.Now()
			if err := couchdb.UpdateDoc(inst, contact); err != nil {
				return err
			}
			// The organization key was encrypted with the wrong key and must be discarded
			orgKey = ""
		}
	}

	status := bitwarden.OrgMemberInvited
	if bw.PublicKey != "" {
		status = bitwarden.OrgMemberAccepted
	}
	if orgKey != "" {
		status = bitwarden.OrgMemberConfirmed
	}
	org.Members[domain] = bitwarden.OrgMember{
		UserID:   bw.UserID,
		Email:    m.Email,
		Name:     m.PrimaryName(),
		OrgKey:   orgKey,
		Status:   status,
		Owner:    false,
		ReadOnly: m.ReadOnly || s.ReadOnlyRules(),
	}
	if err := couchdb.UpdateDoc(inst, org); err != nil {
		return err
	}
	if status == bitwarden.OrgMemberAccepted {
		return s.sendContactConfirmationMail(inst, m)
	}
	return nil
}

func (s *Sharing) sendContactConfirmationMail(inst *instance.Instance, m *Member) error {
	publicName, _ := csettings.PublicName(inst)
	link := inst.SubDomain(s.AppSlug)
	msg, err := job.NewMessage(&mail.Options{
		Mode:         mail.ModeFromStack,
		TemplateName: "sharing_to_confirm",
		TemplateValues: map[string]interface{}{
			"PublicName": publicName,
			"MemberName": m.PrimaryName(),
			"Link":       link.String(),
		},
	})
	if err != nil {
		return err
	}
	_, err = job.System().PushJob(inst, &job.JobRequest{
		WorkerType: "sendmail",
		Message:    msg,
	})
	return err
}

// RemoveBitwardenMember removes a sharing member from the bitwarden
// organization. It is called when the owner revokes a member, or when the
// owner is notified that a member has left the sharing.
func (s *Sharing) RemoveBitwardenMember(inst *instance.Instance, m *Member, orgID string) error {
	org := &bitwarden.Organization{}
	if err := couchdb.GetDoc(inst, consts.BitwardenOrganizations, orgID, org); err != nil {
		if couchdb.IsNotFoundError(err) {
			return nil
		}
		return err
	}
	domain := m.Instance
	if u, err := url.Parse(m.Instance); err == nil {
		domain = u.Host
	}
	delete(org.Members, domain)
	return couchdb.UpdateDoc(inst, org)
}

// RemoveAllBitwardenMembers removes all the members from the bitwarden
// organization (except the owner of the sharing).
func (s *Sharing) RemoveAllBitwardenMembers(inst *instance.Instance, orgID string) error {
	org := &bitwarden.Organization{}
	if err := couchdb.GetDoc(inst, consts.BitwardenOrganizations, orgID, org); err != nil {
		if couchdb.IsNotFoundError(err) {
			return nil
		}
		return err
	}
	for domain := range org.Members {
		if domain != inst.Domain {
			delete(org.Members, domain)
		}
	}
	return couchdb.UpdateDoc(inst, org)
}
