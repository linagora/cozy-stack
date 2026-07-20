package sharing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/cozy/cozy-stack/client/request"
	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/labstack/echo/v4"
)

// Group contains the information about a group of members of the sharing.
type Group struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
	Color    string `json:"color,omitempty"`
	AddedBy  int    `json:"addedBy"` // The index of the member who added the group
	ReadOnly bool   `json:"read_only"`
	Revoked  bool   `json:"revoked,omitempty"`
}

// AddGroup adds a group of contacts identified by its ID to the members of the
// sharing.
func (s *Sharing) AddGroup(inst *instance.Instance, groupID string, readOnly bool) error {
	for _, g := range s.Groups {
		if g.ID == groupID && !g.Revoked {
			return ErrGroupCannotBeAddedTwice
		}
	}

	group, err := contact.FindGroup(inst, groupID)
	if err != nil {
		return err
	}
	contacts, err := group.GetAllContacts(inst)
	if err != nil {
		return err
	}

	groupIndex := len(s.Groups)
	for _, contact := range contacts {
		m := buildMemberFromContact(contact, readOnly)
		m.Groups = []int{groupIndex}
		m.OnlyInGroups = true
		_, idx, err := s.addMember(inst, m)
		if err != nil {
			return err
		}
		pos := sort.SearchInts(s.Members[idx].Groups, groupIndex)
		if pos == len(s.Members[idx].Groups) || s.Members[idx].Groups[pos] != groupIndex {
			s.Members[idx].Groups = append(s.Members[idx].Groups, groupIndex)
			sort.Ints(s.Members[idx].Groups)
		}
	}

	g := Group{
		ID:       groupID,
		Name:     group.Name(),
		Color:    group.Color(),
		AddedBy:  0,
		ReadOnly: readOnly,
	}
	s.Groups = append(s.Groups, g)
	return nil
}

// RevokeGroup revokes a group of members on the sharer Cozy. After that, the
// sharing is disabled if there are no longer any active recipient.
func (s *Sharing) RevokeGroup(inst *instance.Instance, index int) error {
	if !s.Owner {
		return ErrInvalidSharing
	}

	var errm error
	for i, m := range s.Members {
		inGroup := false
		for _, idx := range m.Groups {
			if idx == index {
				inGroup = true
			}
		}
		if !inGroup {
			continue
		}
		if len(m.Groups) == 1 {
			s.Members[i].Groups = nil
		} else {
			var groups []int
			for _, idx := range m.Groups {
				if idx != index {
					groups = append(groups, idx)
				}
			}
			s.Members[i].Groups = groups
		}
		if m.OnlyInGroups && len(s.Members[i].Groups) == 0 {
			if err := s.RevokeRecipient(inst, i); err != nil {
				errm = multierror.Append(errm, err)
			}
		}
	}

	s.Groups[index].Revoked = true
	if s.Drive {
		if errm != nil {
			if err := couchdb.UpdateDoc(inst, s); err != nil {
				errm = multierror.Append(errm, err)
			}
			return errm
		}
		return s.NoMoreRecipient(inst)
	}

	if err := couchdb.UpdateDoc(inst, s); err != nil {
		errm = multierror.Append(errm, err)
	}
	return errm
}

// UpdateGroups is called when a contact is added or removed to a group. It
// finds the sharings for this group, and adds or removes the member to those
// sharings.
func UpdateGroups(inst *instance.Instance, msg job.ShareGroupMessage) error {
	if msg.RenamedGroup != nil {
		return updateGroupMetadata(inst, msg.RenamedGroup)
	}

	var c *contact.Contact
	if msg.DeletedDoc != nil {
		c = &contact.Contact{JSONDoc: *msg.DeletedDoc}
	} else {
		doc, err := contact.Find(inst, msg.ContactID)
		if err != nil {
			return err
		}
		c = doc
	}

	sharings, err := FindActive(inst)
	if err != nil {
		return err
	}

	var errm error
	for _, s := range sharings {
		for _, added := range msg.GroupsAdded {
			for idx, group := range s.Groups {
				if group.ID == added {
					if s.Owner {
						if err := s.AddMemberToGroup(inst, idx, c); err != nil {
							errm = multierror.Append(errm, err)
						}
					} else {
						if err := s.DelegateAddMemberToGroup(inst, idx, c); err != nil {
							errm = multierror.Append(errm, err)
						}
					}
				}
			}
		}
		for _, removed := range msg.GroupsRemoved {
			for idx, group := range s.Groups {
				if group.ID == removed {
					if s.Owner {
						if err := s.RemoveMemberFromGroup(inst, idx, c); err != nil {
							errm = multierror.Append(errm, err)
						}
					} else {
						if err := s.DelegateRemoveMemberFromGroup(inst, idx, c); err != nil {
							errm = multierror.Append(errm, err)
						}
					}
				}
			}
		}

		if msg.BecomeInvitable {
			if err := s.AddInvitationForContact(inst, c); err != nil {
				errm = multierror.Append(errm, err)
			}
		}
	}

	return errm
}

func updateGroupMetadata(inst *instance.Instance, doc *couchdb.JSONDoc) error {
	sharings, err := FindActive(inst)
	if err != nil {
		return err
	}

	updatedGroup := &contact.Group{JSONDoc: *doc}
	var errm error
	for _, s := range sharings {
		for idx, group := range s.Groups {
			if group.ID == doc.ID() {
				group.Name = updatedGroup.Name()
				group.Color = updatedGroup.Color()
				s.Groups[idx] = group
				if err := couchdb.UpdateDoc(inst, s); err != nil {
					errm = multierror.Append(errm, err)
				}
				cloned := s.Clone().(*Sharing)
				go cloned.NotifyRecipients(inst, nil)
			}
		}
	}

	return errm
}

// AddMemberToGroup adds a contact to a sharing via a group (on the owner).
func (s *Sharing) AddMemberToGroup(inst *instance.Instance, groupIndex int, contact *contact.Contact) error {
	readOnly := s.Groups[groupIndex].ReadOnly
	m := buildMemberFromContact(contact, readOnly)
	m.OnlyInGroups = true
	_, idx, err := s.addMember(inst, m)
	if err != nil {
		return err
	}
	s.Members[idx].Groups = append(s.Members[idx].Groups, groupIndex)
	sort.Ints(s.Members[idx].Groups)

	// We can ignore the error as we will try again to save the sharing
	// after sending the invitation.
	_ = couchdb.UpdateDoc(inst, s)
	var perms *permission.Permission
	if s.PreviewPath != "" {
		if perms, err = s.CreatePreviewPermissions(inst); err != nil {
			return err
		}
	}
	if err = s.SendInvitations(inst, perms); err != nil {
		return err
	}
	cloned := s.Clone().(*Sharing)
	go cloned.NotifyRecipients(inst, nil)
	return nil
}

// DelegateAddMemberToGroup adds a contact to a sharing via a group (on a recipient).
func (s *Sharing) DelegateAddMemberToGroup(inst *instance.Instance, groupIndex int, contact *contact.Contact) error {
	readOnly := s.Groups[groupIndex].ReadOnly
	m := buildMemberFromContact(contact, readOnly)
	m.OnlyInGroups = true
	m.Groups = []int{groupIndex}
	api := &APIDelegateAddContacts{
		sid:     s.ID(),
		members: []Member{m},
	}
	return s.SendDelegated(inst, api)
}

// RemoveMemberFromGroup removes a member of a group.
func (s *Sharing) RemoveMemberFromGroup(inst *instance.Instance, groupIndex int, contact *contact.Contact) error {
	var email string
	if addr, err := contact.ToMailAddress(); err == nil {
		email = addr.Email
	}
	cozyURL := contact.PrimaryCozyURL()

	matchMember := func(m Member) bool {
		if m.Email != "" && m.Email == email {
			return true
		}
		if m.Instance != "" && m.Instance == cozyURL {
			return true
		}
		return false
	}

	for i, m := range s.Members {
		if !matchMember(m) {
			continue
		}

		var groups []int
		for _, idx := range m.Groups {
			if idx != groupIndex {
				groups = append(groups, idx)
			}
		}
		s.Members[i].Groups = groups

		if m.OnlyInGroups && len(s.Members[i].Groups) == 0 {
			return s.RevokeRecipient(inst, i)
		} else {
			return couchdb.UpdateDoc(inst, s)
		}
	}

	return nil
}

// DelegateRemoveMemberFromGroup removes a member from a sharing group (on a recipient).
func (s *Sharing) DelegateRemoveMemberFromGroup(inst *instance.Instance, groupIndex int, contact *contact.Contact) error {
	var email string
	if addr, err := contact.ToMailAddress(); err == nil {
		email = addr.Email
	}
	cozyURL := contact.PrimaryCozyURL()

	for i, m := range s.Members {
		if m.Email != "" && m.Email == email {
			return s.SendRemoveMemberFromGroup(inst, groupIndex, i)
		}
		if m.Instance != "" && m.Instance == cozyURL {
			return s.SendRemoveMemberFromGroup(inst, groupIndex, i)
		}
	}
	return ErrMemberNotFound
}

func (s *Sharing) SendRemoveMemberFromGroup(inst *instance.Instance, groupIndex, memberIndex int) error {
	u, err := url.Parse(s.Members[0].Instance)
	if err != nil {
		return err
	}
	c := &s.Credentials[0]
	if c.AccessToken == nil {
		return ErrInvalidSharing
	}
	opts := &request.Options{
		Method: http.MethodDelete,
		Scheme: u.Scheme,
		Domain: u.Host,
		Path:   fmt.Sprintf("/sharings/%s/groups/%d/%d", s.SID, groupIndex, memberIndex),
		Headers: request.Headers{
			echo.HeaderAuthorization: "Bearer " + c.AccessToken.AccessToken,
		},
		ParseError: ParseRequestError,
	}
	res, err := request.Req(opts)
	if res != nil && res.StatusCode/100 == 4 {
		res, err = RefreshToken(inst, res, err, s, &s.Members[0], c, opts, nil)
	}
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		return ErrInternalServerError
	}
	return nil
}

func (s *Sharing) DelegatedRemoveMemberFromGroup(inst *instance.Instance, groupIndex, memberIndex int) error {
	var groups []int
	for _, idx := range s.Members[memberIndex].Groups {
		if idx != groupIndex {
			groups = append(groups, idx)
		}
	}
	s.Members[memberIndex].Groups = groups

	if s.Members[memberIndex].OnlyInGroups && len(s.Members[memberIndex].Groups) == 0 {
		return s.RevokeRecipient(inst, memberIndex)
	} else {
		return couchdb.UpdateDoc(inst, s)
	}
}

func (s *Sharing) AddInvitationForContact(inst *instance.Instance, contact *contact.Contact) error {
	var email string
	if addr, err := contact.ToMailAddress(); err == nil {
		email = addr.Email
	}
	cozyURL := contact.PrimaryCozyURL()
	name := contact.PrimaryName()
	groupIDs := contact.GroupIDs()

	matchMember := func(m Member) bool {
		if m.Name != name {
			return false
		}
		for _, gid := range groupIDs {
			for _, g := range m.Groups {
				if s.Groups[g].ID == gid {
					return true
				}
			}
		}
		return false
	}

	for i, m := range s.Members {
		if i == 0 || m.Status != MemberStatusMailNotSent {
			continue
		}
		if !matchMember(m) {
			continue
		}
		m.Email = email
		m.Instance = cozyURL
		s.Members[i] = m

		if !s.Owner {
			return s.DelegateAddInvitation(inst, i)
		}

		// We can ignore the error as we will try again to save the sharing
		// after sending the invitation.
		_ = couchdb.UpdateDoc(inst, s)
		var perms *permission.Permission
		var err error
		if s.PreviewPath != "" {
			if perms, err = s.CreatePreviewPermissions(inst); err != nil {
				return err
			}
		}
		if err = s.SendInvitations(inst, perms); err != nil {
			return err
		}
		cloned := s.Clone().(*Sharing)
		go cloned.NotifyRecipients(inst, nil)
		return nil
	}

	return nil
}

func (s *Sharing) DelegateAddInvitation(inst *instance.Instance, memberIndex int) error {
	body, err := json.Marshal(map[string]interface{}{
		"data": map[string]interface{}{
			"type":       consts.SharingsMembers,
			"attributes": s.Members[memberIndex],
		},
	})
	if err != nil {
		return err
	}
	u, err := url.Parse(s.Members[0].Instance)
	if err != nil {
		return err
	}
	c := &s.Credentials[0]
	if c.AccessToken == nil {
		return ErrInvalidSharing
	}
	opts := &request.Options{
		Method: http.MethodPost,
		Scheme: u.Scheme,
		Domain: u.Host,
		Path:   fmt.Sprintf("/sharings/%s/members/%d/invitation", s.ID(), memberIndex),
		Headers: request.Headers{
			echo.HeaderAccept:        echo.MIMEApplicationJSON,
			echo.HeaderContentType:   jsonapi.ContentType,
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
		return err
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
		s.Members[i].Status = MemberStatusReady
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
	return s.SendInvitationsToMembers(inst, []Member{s.Members[memberIndex]}, states)
}
