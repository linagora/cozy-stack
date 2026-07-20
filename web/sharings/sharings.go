// Package sharings is the HTTP routes for the sharing. We have two types of
// routes, some routes are used by the clients to create, list, revoke sharings
// and add/remove recipients, and other routes are reserved for an internal
// usage, mostly to synchronize the documents between the Cozys of the members
// of the sharings.
package sharings

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/settings"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/avatar"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/safehttp"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/cozy/cozy-stack/web/middlewares"
	weboidc "github.com/cozy/cozy-stack/web/oidc"
	"github.com/hashicorp/go-multierror"
	"github.com/labstack/echo/v4"
)

var errMissingOIDCContextForOrgDomain = errors.New("organization domain instance has no context")

// CreateSharing initializes a new sharing (on the sharer)
func CreateSharing(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	var s sharing.Sharing
	obj, err := jsonapi.Bind(c.Request().Body, &s)
	if err != nil {
		return jsonapi.BadJSON()
	}

	slug, err := checkCreatePermissions(c, &s)
	if err != nil {
		return echo.NewHTTPError(http.StatusForbidden)
	}

	if err = s.BeOwner(inst, slug); err != nil {
		return wrapErrors(err)
	}

	if rel, ok := obj.GetRelationship("recipients"); ok {
		if data, ok := rel.Data.([]interface{}); ok {
			for _, ref := range data {
				if t, _ := ref.(map[string]interface{})["type"].(string); t == consts.Groups {
					if id, ok := ref.(map[string]interface{})["id"].(string); ok {
						if err = s.AddGroup(inst, id, false); err != nil {
							return err
						}
					}
				} else {
					if id, ok := ref.(map[string]interface{})["id"].(string); ok {
						if err = s.AddContact(inst, id, false); err != nil {
							return err
						}
					}
				}
			}
		}
	}

	if rel, ok := obj.GetRelationship("read_only_recipients"); ok {
		if data, ok := rel.Data.([]interface{}); ok {
			for _, ref := range data {
				if t, _ := ref.(map[string]interface{})["type"].(string); t == consts.Groups {
					if id, ok := ref.(map[string]interface{})["id"].(string); ok {
						if err = s.AddGroup(inst, id, true); err != nil {
							return err
						}
					}
				} else {
					if id, ok := ref.(map[string]interface{})["id"].(string); ok {
						if err = s.AddContact(inst, id, true); err != nil {
							return err
						}
					}
				}
			}
		}
	}

	perms, err := s.Create(inst)
	if err != nil {
		return wrapErrors(err)
	}
	if err = s.SendInvitations(inst, perms); err != nil {
		return wrapErrors(err)
	}
	as := &sharing.APISharing{
		Sharing:     &s,
		Credentials: nil,
		SharedDocs:  nil,
	}
	return jsonapi.Data(c, http.StatusCreated, as, nil)
}

// PutSharing creates a sharing request (on the recipient's cozy)
func PutSharing(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	log := inst.Logger().WithNamespace("sharing")

	var s sharing.Sharing
	obj, err := jsonapi.Bind(c.Request().Body, &s)
	if err != nil {
		return jsonapi.BadJSON()
	}
	s.SID = obj.ID
	s.ShortcutID = ""

	if err := s.CreateRequest(inst); err != nil {
		return wrapErrors(err)
	}

	if s.Drive && len(s.Members) > 0 {
		sender := &s.Members[0]
		if sharing.IsTrustedMember(inst, sender) {
			if c.QueryParam(sharing.InteractiveSharingQueryParam) == "true" {
				log.Debugf("Skipping auto-accept for interactive drive sharing %s from trusted sender %s", s.SID, sender.Instance)
			} else if len(s.Credentials) > 0 && s.Credentials[0].State != "" {
				state := s.Credentials[0].State
				log.Debugf("Auto-accepting drive sharing %s from trusted sender %s", s.SID, sender.Instance)
				if err := sharing.EnqueueAutoAccept(inst, s.SID, state); err != nil {
					log.Warnf("Cannot enqueue auto-accept: %s", err)
				}
			} else {
				log.Warnf("Drive sharing from trusted sender but State not available")
			}
		}
	}

	if c.QueryParam("shortcut") == "true" {
		if s.Drive {
			if err := s.CreateDriveShortcut(inst, false); err != nil {
				return wrapErrors(err)
			}
		} else {
			u := c.QueryParam("url")
			if err := s.CreateShortcut(inst, u, false); err != nil {
				return wrapErrors(err)
			}
		}
	}

	as := &sharing.APISharing{
		Sharing:     &s,
		Credentials: nil,
		SharedDocs:  nil,
	}
	return jsonapi.Data(c, http.StatusCreated, as, nil)
}

// jsonapiSharingWithDocs is an helper to send a JSON-API response for a
// sharing with its shared docs
func jsonapiSharingWithDocs(c echo.Context, s *sharing.Sharing) error {
	inst := middlewares.GetInstance(c)
	sharedDocs, err := sharing.GetSharedDocsBySharingIDs(inst, []string{s.SID})
	if err != nil {
		return wrapErrors(err)
	}
	docs := sharedDocs[s.SID]
	as := &sharing.APISharing{
		Sharing:     s,
		Credentials: nil,
		SharedDocs:  docs,
	}
	return jsonapi.Data(c, http.StatusOK, as, nil)
}

// GetSharing returns the sharing document associated to the given sharingID
// and which documents have been shared.
// The requester must have the permission on at least one doctype declared in
// the sharing document.
func GetSharing(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	sharingID := c.Param("sharing-id")
	s, err := sharing.FindSharing(inst, sharingID)
	if err != nil {
		return wrapErrors(err)
	}
	if err = checkGetPermissions(c, s); err != nil {
		return wrapErrors(err)
	}
	return jsonapiSharingWithDocs(c, s)
}

func PatchSharing(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	sharingID := c.Param("sharing-id")
	s, err := sharing.FindSharing(inst, sharingID)
	if err != nil {
		return wrapErrors(err)
	}
	if _, err = checkCreatePermissions(c, s); err != nil {
		return wrapErrors(err)
	}

	req := sharing.APISharing{}
	if _, err := jsonapi.Bind(c.Request().Body, &req); err != nil {
		return wrapErrors(err)
	}
	if err := s.PatchDescription(inst, req.Description); err != nil {
		return wrapErrors(err)
	}

	return jsonapiSharingWithDocs(c, s)
}

// CountNewShortcuts returns the number of shortcuts to a sharing that have not
// been seen.
func CountNewShortcuts(c echo.Context) error {
	if _, err := middlewares.GetPermission(c); err != nil {
		return echo.NewHTTPError(http.StatusForbidden)
	}

	inst := middlewares.GetInstance(c)
	count, err := sharing.CountNewShortcuts(inst)
	if err != nil {
		return wrapErrors(err)
	}
	body := map[string]interface{}{
		"meta": map[string]int{
			"count": count,
		},
	}
	return c.JSON(http.StatusOK, body)
}

// GetSharingsInfoByDocType returns, for a given doctype, all the sharing
// information, i.e. the involved sharings and the shared documents
func GetSharingsInfoByDocType(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	docType := c.Param("doctype")
	withSharedDocs, err := strconv.ParseBool(c.QueryParam("shared_docs"))
	if err != nil {
		withSharedDocs = true
	}
	sharings, err := sharing.GetSharingsByDocType(inst, docType)
	if err != nil {
		inst.Logger().WithNamespace("sharing").Errorf("GetSharingsByDocType error: %s", err)
		return wrapErrors(err)
	}
	if err := middlewares.AllowWholeType(c, permission.GET, docType); err != nil {
		return wrapErrors(err)
	}
	if len(sharings) == 0 {
		return jsonapi.DataList(c, http.StatusOK, nil, nil)
	}
	sharingIDs := make([]string, 0, len(sharings))
	for sID := range sharings {
		sharingIDs = append(sharingIDs, sID)
	}

	var sDocs map[string][]couchdb.DocReference
	if withSharedDocs {
		sDocs, err = sharing.GetSharedDocsBySharingIDs(inst, sharingIDs)
		if err != nil {
			inst.Logger().WithNamespace("sharing").Errorf("GetSharedDocsBySharingIDs error: %s", err)
			return wrapErrors(err)
		}
	}

	res := make([]*sharing.APISharing, 0, len(sharings))
	for sID, s := range sharings {
		as := &sharing.APISharing{
			Sharing:     s,
			SharedDocs:  sDocs[sID],
			Credentials: nil,
		}
		res = append(res, as)
	}
	return sharing.InfoByDocTypeData(c, http.StatusOK, res)
}

// AnswerSharing is used to exchange credentials between 2 cozys, after the
// recipient has accepted a sharing.
func AnswerSharing(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	sharingID := c.Param("sharing-id")
	s, err := sharing.FindSharing(inst, sharingID)
	if err != nil {
		return wrapErrors(err)
	}
	var creds sharing.APICredentials
	if _, err = jsonapi.Bind(c.Request().Body, &creds); err != nil {
		return jsonapi.BadJSON()
	}
	ac, err := s.ProcessAnswer(inst, &creds)
	if err != nil {
		return wrapErrors(err)
	}
	return jsonapi.Data(c, http.StatusOK, ac, nil)
}

// ReceivePublicKey is used to receive the public key of a sharing member. It can
// be used when the member has delegated authentication, and didn't have a
// password when they accepted the sharing: this route is called when the user
// choose a password a bit later in cozy pass web.
func ReceivePublicKey(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	sharingID := c.Param("sharing-id")
	s, err := sharing.FindSharing(inst, sharingID)
	if err != nil {
		return wrapErrors(err)
	}
	member, err := requestMember(c, s)
	if err != nil {
		return wrapErrors(err)
	}
	var creds sharing.APICredentials
	if _, err = jsonapi.Bind(c.Request().Body, &creds); err != nil || creds.Bitwarden == nil {
		return jsonapi.BadJSON()
	}
	if err := s.SaveBitwarden(inst, member, creds.Bitwarden); err != nil {
		return wrapErrors(err)
	}
	return c.NoContent(http.StatusNoContent)
}

// ChangeCozyAddress is called when a Cozy has been moved to a new address.
func ChangeCozyAddress(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	sharingID := c.Param("sharing-id")
	s, err := sharing.FindSharing(inst, sharingID)
	if err != nil {
		return wrapErrors(err)
	}
	var moved sharing.APIMoved
	if _, err = jsonapi.Bind(c.Request().Body, &moved); err != nil {
		return jsonapi.BadJSON()
	}

	member, err := requestMember(c, s)
	if err != nil {
		return wrapErrors(err)
	}

	if s.Owner {
		err = s.ChangeMemberAddress(inst, member, moved)
	} else {
		err = s.ChangeOwnerAddress(inst, moved)
	}
	if err != nil {
		return wrapErrors(err)
	}
	return c.NoContent(http.StatusNoContent)
}

func addRecipientsToSharing(inst *instance.Instance, s *sharing.Sharing, rel *jsonapi.Relationship, readOnly bool) error {
	var err error
	if data, ok := rel.Data.([]interface{}); ok {
		var contactIDs, groupIDs []string
		for _, ref := range data {
			if id, ok := ref.(map[string]interface{})["id"].(string); ok {
				if t, _ := ref.(map[string]interface{})["type"].(string); t == consts.Groups {
					groupIDs = append(groupIDs, id)
				} else {
					contactIDs = append(contactIDs, id)
				}
			}
		}
		if s.Owner {
			err = s.AddGroupsAndContacts(inst, groupIDs, contactIDs, readOnly)
		} else {
			err = s.DelegateAddContactsAndGroups(inst, groupIDs, contactIDs, readOnly)
		}
	}
	return err
}

// AddRecipients is used to add a member to a sharing
func AddRecipients(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	sharingID := c.Param("sharing-id")
	s, err := sharing.FindSharing(inst, sharingID)
	if err != nil {
		return wrapErrors(err)
	}
	if _, err = checkCreatePermissions(c, s); err != nil {
		return wrapErrors(err)
	}
	var body sharing.Sharing
	obj, err := jsonapi.Bind(c.Request().Body, &body)
	if err != nil {
		return jsonapi.BadJSON()
	}
	if rel, ok := obj.GetRelationship("recipients"); ok {
		if err = addRecipientsToSharing(inst, s, rel, false); err != nil {
			return wrapErrors(err)
		}
	}
	if rel, ok := obj.GetRelationship("read_only_recipients"); ok {
		if err = addRecipientsToSharing(inst, s, rel, true); err != nil {
			return wrapErrors(err)
		}
	}
	return jsonapiSharingWithDocs(c, s)
}

type delegatedAddRecipientsBody struct {
	Data struct {
		Type          string `json:"type"`
		ID            string `json:"id"`
		Relationships struct {
			Groups struct {
				Data []sharing.Group `json:"data"`
			} `json:"groups"`
			Recipients struct {
				Data []sharing.Member `json:"data"`
			} `json:"recipients"`
		} `json:"relationships"`
	} `json:"data"`
}

func authorizeDelegatedRecipientAddition(c echo.Context, s *sharing.Sharing, member *sharing.Member, body *delegatedAddRecipientsBody) error {
	if s.Open {
		return hasSharingWritePermissions(c)
	}
	if !s.Drive {
		return echo.NewHTTPError(http.StatusForbidden)
	}
	if !member.ReadOnly {
		return nil
	}
	for _, g := range body.Data.Relationships.Groups.Data {
		if !g.ReadOnly {
			return echo.NewHTTPError(http.StatusForbidden)
		}
	}
	for _, m := range body.Data.Relationships.Recipients.Data {
		if !m.ReadOnly {
			return echo.NewHTTPError(http.StatusForbidden)
		}
	}
	return nil
}

// AddRecipientsDelegated is used to add members and groups to a sharing on the
// owner's cozy when it's the recipient's cozy that sends the mail invitation.
func AddRecipientsDelegated(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	sharingID := c.Param("sharing-id")
	s, err := sharing.FindSharing(inst, sharingID)
	if err != nil {
		return wrapErrors(err)
	}
	if !s.Owner || (!s.Open && !s.Drive) {
		return echo.NewHTTPError(http.StatusForbidden)
	}
	member, err := requestMember(c, s)
	if err != nil {
		return wrapErrors(err)
	}
	memberIndex := -1
	for i, m := range s.Members {
		if m.Instance == member.Instance {
			memberIndex = i
		}
	}
	if memberIndex == -1 {
		return jsonapi.InternalServerError(sharing.ErrInvalidSharing)
	}

	var body delegatedAddRecipientsBody
	if err = json.NewDecoder(c.Request().Body).Decode(&body); err != nil {
		return jsonapi.BadJSON()
	}
	if err = authorizeDelegatedRecipientAddition(c, s, member, &body); err != nil {
		return err
	}

	for _, g := range body.Data.Relationships.Groups.Data {
		g.AddedBy = memberIndex
		s.Groups = append(s.Groups, g)
	}

	states := make(map[string]string)
	for _, m := range body.Data.Relationships.Recipients.Data {
		state, err := s.AddDelegatedContact(inst, m)
		if err != nil {
			// Adding recipients is idempotent: keep processing the batch when a
			// recipient is already active.
			if errors.Is(err, sharing.ErrAlreadyAccepted) {
				continue
			}
			if len(m.Groups) > 0 {
				continue
			}
			return wrapErrors(err)
		}
		// If we have an URL for the Cozy, we can create a shortcut as an invitation
		if m.Instance != "" {
			states[m.Instance] = state
			var perms *permission.Permission
			if s.PreviewPath != "" {
				if perms, err = s.CreatePreviewPermissions(inst); err != nil {
					return wrapErrors(err)
				}
			}
			if err = s.SendInvitations(inst, perms); err != nil {
				return wrapErrors(err)
			}
		} else if m.Email != "" {
			states[m.Email] = state
		}
	}

	if err := couchdb.UpdateDoc(inst, s); err != nil {
		return wrapErrors(err)
	}
	cloned := s.Clone().(*sharing.Sharing)
	go cloned.NotifyRecipients(inst, nil)
	return c.JSON(http.StatusOK, states)
}

// AddInvitationDelegated is when a member has been added to a sharing via a
// group, but is invited only later (no email or Cozy instance known when they
// was added).
func AddInvitationDelegated(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	sharingID := c.Param("sharing-id")
	s, err := sharing.FindSharing(inst, sharingID)
	if err != nil {
		return wrapErrors(err)
	}
	if !s.Owner || !s.Open {
		return echo.NewHTTPError(http.StatusForbidden)
	}

	memberIndex, err := strconv.Atoi(c.Param("member-index"))
	if err != nil || memberIndex <= 0 || memberIndex >= len(s.Members) {
		return jsonapi.InvalidParameter("member-index", errors.New("invalid member-index parameter"))
	}

	var body struct {
		Data struct {
			Type   string         `json:"type"`
			Member sharing.Member `json:"attributes"`
		}
	}
	if err = json.NewDecoder(c.Request().Body).Decode(&body); err != nil {
		return jsonapi.BadJSON()
	}

	states := make(map[string]string)
	m := s.Members[memberIndex]
	if m.Status == sharing.MemberStatusMailNotSent {
		m.Instance = body.Data.Member.Instance
		m.Email = body.Data.Member.Email
		state64 := crypto.Base64Encode(crypto.GenerateRandomBytes(sharing.StateLen))
		state := string(state64)
		creds := sharing.Credentials{
			State:  state,
			XorKey: sharing.MakeXorKey(),
		}
		s.Credentials[memberIndex-1] = creds
		s.Members[memberIndex] = m
		// If we have an URL for the Cozy, we can create a shortcut as an invitation
		if m.Instance != "" {
			states[m.Instance] = state
			var perms *permission.Permission
			if s.PreviewPath != "" {
				if perms, err = s.CreatePreviewPermissions(inst); err != nil {
					return wrapErrors(err)
				}
			}
			if err = s.SendInvitations(inst, perms); err != nil {
				return wrapErrors(err)
			}
		} else if m.Email != "" {
			states[m.Email] = state
			s.Members[memberIndex].Status = sharing.MemberStatusReady
		}
	}

	if err := couchdb.UpdateDoc(inst, s); err != nil {
		return wrapErrors(err)
	}
	cloned := s.Clone().(*sharing.Sharing)
	go cloned.NotifyRecipients(inst, nil)
	return c.JSON(http.StatusOK, states)
}

// RemoveMemberFromGroup is used to remove a member from a group (delegated).
func RemoveMemberFromGroup(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	sharingID := c.Param("sharing-id")
	s, err := sharing.FindSharing(inst, sharingID)
	if err != nil {
		return wrapErrors(err)
	}
	if !s.Owner || !s.Open {
		return echo.NewHTTPError(http.StatusForbidden)
	}

	member, err := requestMember(c, s)
	if err != nil {
		return wrapErrors(err)
	}
	addedBy := -1
	for i, m := range s.Members {
		if m.Instance == member.Instance {
			addedBy = i
		}
	}
	if addedBy == -1 {
		return jsonapi.InternalServerError(sharing.ErrInvalidSharing)
	}

	groupIndex, err := strconv.Atoi(c.Param("group-index"))
	if err != nil || groupIndex < 0 || groupIndex >= len(s.Groups) {
		return jsonapi.InvalidParameter("group-index", errors.New("invalid group-index parameter"))
	}
	if s.Groups[groupIndex].AddedBy != addedBy {
		return echo.NewHTTPError(http.StatusForbidden)
	}

	memberIndex, err := strconv.Atoi(c.Param("member-index"))
	if err != nil || memberIndex <= 0 || memberIndex >= len(s.Members) {
		return jsonapi.InvalidParameter("member-index", errors.New("invalid member-index parameter"))
	}

	if err := s.DelegatedRemoveMemberFromGroup(inst, groupIndex, memberIndex); err != nil {
		return wrapErrors(err)
	}
	return c.NoContent(http.StatusNoContent)
}

// PutRecipients is used to update the members list on the recipients cozy
func PutRecipients(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	sharingID := c.Param("sharing-id")
	s, err := sharing.FindSharing(inst, sharingID)
	if err != nil {
		return wrapErrors(err)
	}

	if s.Active {
		// If the sharing is active, we check the access token for a permission
		// on the sharing
		if err := hasSharingWritePermissions(c); err != nil {
			return err
		}
	} else {
		// If there is no synchronization, it means that we have a shortcut for
		// this sharing, and we can check the sharecode.
		token := middlewares.GetRequestToken(c)
		sharecode, err := s.GetSharecodeFromShortcut(inst)
		if err != nil || token != sharecode {
			return middlewares.ErrForbidden
		}
	}

	var params sharing.PutRecipientsParams
	if err = json.NewDecoder(c.Request().Body).Decode(&params); err != nil {
		return wrapErrors(err)
	}
	if err = s.UpdateRecipients(inst, params); err != nil {
		return wrapErrors(err)
	}
	return c.NoContent(http.StatusNoContent)
}

func renderAlreadyAccepted(c echo.Context, inst *instance.Instance, cozyURL string) error {
	return c.Render(http.StatusBadRequest, "error.html", echo.Map{
		"Domain":         inst.ContextualDomain(),
		"ContextName":    inst.ContextName,
		"Locale":         inst.Locale,
		"Title":          inst.TemplateTitle(),
		"Favicon":        middlewares.Favicon(inst),
		"ErrorTitle":     "Error Sharing already accepted Title",
		"Error":          "Error Sharing already accepted",
		"Button":         "Error Sharing already accepted Button",
		"ButtonURL":      cozyURL,
		"SupportEmail":   inst.SupportEmailAddress(),
		"SupportPageURL": inst.SupportPageURL(),
	})
}

func renderInvalidSharing(c echo.Context, inst *instance.Instance) error {
	return c.Render(http.StatusBadRequest, "error.html", echo.Map{
		"Domain":         inst.ContextualDomain(),
		"ContextName":    inst.ContextName,
		"Locale":         inst.Locale,
		"Title":          inst.TemplateTitle(),
		"Favicon":        middlewares.Favicon(inst),
		"Illustration":   "/images/generic-error.svg",
		"ErrorTitle":     "Error Invalid sharing Title",
		"Error":          "Error Invalid sharing",
		"SupportEmail":   inst.SupportEmailAddress(),
		"SupportPageURL": inst.SupportPageURL(),
	})
}

func renderDiscoveryForm(c echo.Context, inst *instance.Instance, code int, sharingID, state, sharecode, shortcut string, m *sharing.Member) error {
	publicName, _ := settings.PublicName(inst)
	fqdn := strings.TrimPrefix(m.Instance, "https://")
	slug, domain := "", consts.KnownFlatDomains[0]
	if context, ok := inst.SettingsContext(); ok {
		if d, ok := context["sharing_domain"].(string); ok {
			domain = d
		}
	}
	if strings.HasPrefix(m.Instance, "http://") {
		slug, domain = m.Instance, ""
	} else if parts := strings.SplitN(fqdn, ".", 2); len(parts) == 2 {
		slug, domain = parts[0], parts[1]
	}
	// Show sender's OIDC button only if sender is not in public OIDC context
	// and has OIDC configured (to avoid showing two identical buttons)
	oidcLink := ""
	oidcDisplayName := ""
	oidcLogoURL := ""
	publicOIDCContext := config.GetPublicOIDCContext(inst.ContextName)
	if inst.ContextName != publicOIDCContext {
		if oidc, ok := config.GetOIDC(inst.ContextName); ok {
			if clientID, _ := oidc["client_id"].(string); clientID != "" {
				q := url.Values{
					"sharingID": {sharingID},
					"state":     {state},
				}
				oidcLink = inst.PageURL("/oidc/sharing", q)

				// Extract branding information for display
				if displayName, ok := oidc["display_name"].(string); ok && displayName != "" {
					oidcDisplayName = displayName
				}
				if logoURL, ok := oidc["logo_url"].(string); ok && logoURL != "" {
					oidcLogoURL = logoURL
				}
			}
		}
	}

	// Show "Login with Twake Account" button always if public OIDC is configured
	twakeOIDCLink := ""
	if publicOIDC, ok := config.GetPublicOIDC(inst.ContextName); ok {
		if clientID, _ := publicOIDC["client_id"].(string); clientID != "" {
			q := url.Values{
				"sharingID": {sharingID},
				"state":     {state},
			}
			twakeOIDCLink = inst.PageURL("/oidc/sharing/public", q)
		}
	}

	templateName := "sharing_discovery_sso.html"
	if oidcLink == "" && twakeOIDCLink == "" {
		templateName = "sharing_discovery.html"
	}

	// Add OIDC logo URL to CSP img-src if present
	if oidcLogoURL != "" {
		if logoURL, err := url.Parse(oidcLogoURL); err == nil && logoURL.Scheme != "" && logoURL.Host != "" {
			logoOrigin := logoURL.Scheme + "://" + logoURL.Host
			middlewares.AppendCSPRule(c, "img-src", logoOrigin)
		}
	}

	return c.Render(code, templateName, echo.Map{
		"Domain":          inst.ContextualDomain(),
		"ContextName":     inst.ContextName,
		"Locale":          inst.Locale,
		"Title":           inst.TemplateTitle(),
		"Favicon":         middlewares.Favicon(inst),
		"PublicName":      publicName,
		"RecipientSlug":   slug,
		"RecipientDomain": domain,
		"SharingID":       sharingID,
		"State":           state,
		"ShareCode":       sharecode,
		"Shortcut":        shortcut,
		"OIDCLink":        oidcLink,
		"OIDCDisplayName": oidcDisplayName,
		"OIDCLogoURL":     oidcLogoURL,
		"TwakeOIDCLink":   twakeOIDCLink,
		"Year":            time.Now().Year(),
		"URLError":        code == http.StatusBadRequest,
		"NotEmailError":   code == http.StatusPreconditionFailed,
	})
}

func discoveryStateForMember(s *sharing.Sharing, currentState string, m *sharing.Member) (string, error) {
	if currentState != "" {
		return currentState, nil
	}
	if m == nil {
		return "", sharing.ErrInvalidSharing
	}
	credentials := s.FindCredentials(m)
	if credentials == nil || credentials.State == "" {
		return "", sharing.ErrInvalidSharing
	}
	return credentials.State, nil
}

func domainFromEmail(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return utils.NormalizeDomain(email[at+1:])
}

func oidcContextForOrgDomain(orgDomain string) (string, error) {
	instances, err := instance.ListByOrgDomain(orgDomain)
	if err != nil {
		return "", err
	}
	if len(instances) == 0 {
		return "", nil
	}
	contextName := instances[0].ContextName
	if contextName == "" {
		return "", fmt.Errorf("%w: %s", errMissingOIDCContextForOrgDomain, instances[0].Domain)
	}
	return contextName, nil
}

func oidcContextForMemberEmail(email string) (string, error) {
	domain := domainFromEmail(email)
	if domain == "" {
		return "", nil
	}
	if contextName, err := oidcContextForOrgDomain(domain); err != nil || contextName != "" {
		return contextName, err
	}
	return config.GetOIDCContextByDomain(domain)
}

// GetDiscovery displays a form where a recipient can give the address of their
// cozy instance
func GetDiscovery(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	sharingID := c.Param("sharing-id")
	state := c.QueryParam("state")
	sharecode := c.FormValue("sharecode")
	shortcut := c.QueryParam("shortcut")

	s, err := sharing.FindSharing(inst, sharingID)
	if err != nil {
		return renderInvalidSharing(c, inst)
	}

	m := &sharing.Member{}
	if s.Owner {
		if sharecode != "" {
			m, err = s.FindMemberBySharecode(inst, sharecode)
		} else {
			m, err = s.FindMemberByState(state)
		}
		if err != nil || m.Status == sharing.MemberStatusRevoked {
			return renderInvalidSharing(c, inst)
		}
		if m.Status != sharing.MemberStatusMailNotSent &&
			m.Status != sharing.MemberStatusPendingInvitation &&
			m.Status != sharing.MemberStatusSeen {
			return renderAlreadyAccepted(c, inst, m.Instance)
		}
		if state, err = discoveryStateForMember(s, state, m); err != nil {
			return renderInvalidSharing(c, inst)
		}
	}

	if m.Instance != "" {
		if m.Status != sharing.MemberStatusSeen {
			err = s.RegisterCozyURLInteractive(inst, m, m.Instance)
		}
		if err == nil {
			redirectURL, err := m.GenerateOAuthURL(s, shortcut)
			if err == nil {
				return c.Redirect(http.StatusFound, redirectURL)
			}
		}
	}

	if m.Instance == "" && m.Email != "" {
		contextName, err := oidcContextForMemberEmail(m.Email)
		if err != nil {
			inst.Logger().WithNamespace("oidc").Warnf("Cannot resolve sharing OIDC context from email %q: %s", m.Email, err)
		} else if contextName != "" {
			return weboidc.RedirectSharingWithContext(c, inst, contextName, sharingID, state, m.Email)
		}
	}

	return renderDiscoveryForm(c, inst, http.StatusOK, sharingID, state, sharecode, shortcut, m)
}

// PostDiscovery is called when the recipient has given its Cozy URL. Either an
// error is returned or the recipient will be redirected to their cozy.
//
// Note: we don't have an anti-CSRF system, we rely on shareCode being secret.
func PostDiscovery(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	sharingID := c.Param("sharing-id")
	state := c.FormValue("state")
	sharecode := c.FormValue("sharecode")
	shortcut := c.FormValue("shortcut")
	cozyURL := c.FormValue("url")
	if cozyURL == "" {
		cozyURL = c.FormValue("slug")
	}
	cozyURL = strings.TrimSuffix(cozyURL, ".")
	if !strings.HasPrefix(cozyURL, "http://") && !strings.HasPrefix(cozyURL, "https://") {
		cozyURL = "https://" + cozyURL
	}
	if domain := c.FormValue("domain"); domain != "" && !strings.Contains(cozyURL, ".") {
		if domain == "mycosy.cloud" {
			domain = "mycozy.cloud"
		}
		cozyURL = cozyURL + "." + domain
	}
	cozyURL = ClearAppInURL(cozyURL)

	s, err := sharing.FindSharing(inst, sharingID)
	if err != nil {
		return wrapErrors(err)
	}

	var redirectURL, email string

	if s.Owner {
		var member *sharing.Member
		if sharecode != "" {
			member, err = s.FindMemberBySharecode(inst, sharecode)
			if err != nil {
				return wrapErrors(err)
			}
		} else {
			member, err = s.FindMemberByState(state)
			if err != nil {
				return wrapErrors(err)
			}
		}
		if state, err = discoveryStateForMember(s, state, member); err != nil {
			return wrapErrors(err)
		}
		if strings.Contains(cozyURL, "@") {
			return renderDiscoveryForm(c, inst, http.StatusPreconditionFailed, sharingID, state, sharecode, shortcut, member)
		}
		email = member.Email
		if err = s.RegisterCozyURLInteractive(inst, member, cozyURL); err != nil {
			if c.Request().Header.Get(echo.HeaderAccept) == echo.MIMEApplicationJSON {
				return c.JSON(http.StatusBadRequest, echo.Map{"error": err.Error()})
			}
			if errors.Is(err, sharing.ErrAlreadyAccepted) {
				return renderAlreadyAccepted(c, inst, cozyURL)
			}
			return renderDiscoveryForm(c, inst, http.StatusBadRequest, sharingID, state, sharecode, shortcut, member)
		}
		redirectURL, err = member.GenerateOAuthURL(s, shortcut)
		if err != nil {
			return wrapErrors(err)
		}
		sharing.PersistInstanceURL(inst, member.Email, member.Instance)
	} else {
		redirectURL, err = s.DelegateDiscovery(inst, state, cozyURL, shortcut)
		if err != nil {
			if errors.Is(err, sharing.ErrInvalidURL) {
				if c.Request().Header.Get(echo.HeaderAccept) == echo.MIMEApplicationJSON {
					return c.JSON(http.StatusBadRequest, echo.Map{"error": err.Error()})
				}
				return renderDiscoveryForm(c, inst, http.StatusBadRequest, sharingID, state, sharecode, shortcut, &sharing.Member{})
			}
			return wrapErrors(err)
		}
	}

	if c.Request().Header.Get(echo.HeaderAccept) == echo.MIMEApplicationJSON {
		m := echo.Map{"redirect": redirectURL}
		if email != "" {
			m["email"] = email
		}
		return c.JSON(http.StatusOK, m)
	}
	return c.Redirect(http.StatusFound, redirectURL)
}

// GetPreviewURL returns the preview URL for the member identified by their
// state parameter.
func GetPreviewURL(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	s, err := sharing.FindSharing(inst, c.Param("sharing-id"))
	if err != nil {
		return wrapErrors(err)
	}
	if !s.Owner {
		return wrapErrors(sharing.ErrInvalidSharing)
	}

	args := struct {
		State string `json:"state"`
	}{}
	if err := c.Bind(&args); err != nil {
		return wrapErrors(err)
	}
	if args.State == "" {
		return jsonapi.BadJSON()
	}
	m, err := s.FindMemberByState(args.State)
	if err != nil {
		return wrapErrors(err)
	}

	if m.Status != sharing.MemberStatusMailNotSent &&
		m.Status != sharing.MemberStatusPendingInvitation &&
		m.Status != sharing.MemberStatusSeen {
		return wrapErrors(sharing.ErrAlreadyAccepted)
	}

	perm, err := permission.GetForSharePreview(inst, s.SID)
	if err != nil {
		return wrapErrors(err)
	}
	previewURL := m.InvitationLink(inst, s, args.State, perm)
	return c.JSON(http.StatusOK, map[string]string{"url": previewURL})
}

// GetAvatar returns the avatar of the given member of the sharing.
func GetAvatar(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	sharingID := c.Param("sharing-id")
	s, err := sharing.FindSharing(inst, sharingID)
	if err != nil {
		return wrapErrors(err)
	}

	index, err := strconv.Atoi(c.Param("index"))
	if err != nil {
		return jsonapi.InvalidParameter("index", err)
	}
	if index > len(s.Members) {
		return jsonapi.NotFound(errors.New("member not found"))
	}
	m := s.Members[index]

	// Use the local avatar
	if m.Instance == "" {
		return localAvatar(c, m)
	}
	if m.Instance == inst.PageURL("", nil) {
		err := inst.AvatarFS().ServeAvatarContent(c.Response(), c.Request())
		if err == os.ErrNotExist {
			return localAvatar(c, m)
		}
		return err
	}

	// Use the public avatar from the member's instance
	res, err := safehttp.DefaultClient.Get(m.Instance + "/public/avatar?fallback=404")
	if err != nil {
		return localAvatar(c, m)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound && c.QueryParam("fallback") != "404" {
		return localAvatar(c, m)
	}
	return c.Stream(res.StatusCode, res.Header.Get(echo.HeaderContentType), res.Body)
}

func localAvatar(c echo.Context, m sharing.Member) error {
	name := m.PublicName
	if name == "" {
		name = strings.Split(m.Email, "@")[0]
	}
	name = strings.ToUpper(name)
	var options []avatar.Options
	if m.Status == sharing.MemberStatusMailNotSent ||
		m.Status == sharing.MemberStatusPendingInvitation {
		options = append(options, avatar.GreyBackground)
	}
	img, mime, err := config.Avatars().GenerateInitials(name, nil, options...)
	if err != nil {
		return wrapErrors(err)
	}
	return c.Blob(http.StatusOK, mime, img)
}

// Routes sets the routing for the sharing service
func Routes(router *echo.Group) {
	// Create a sharing
	router.POST("/", CreateSharing)        // On the sharer
	router.PUT("/:sharing-id", PutSharing) // On a recipient
	router.GET("/:sharing-id", GetSharing)
	router.PATCH("/:sharing-id", PatchSharing)
	router.POST("/:sharing-id/answer", AnswerSharing)

	// Managing recipients
	router.POST("/:sharing-id/recipients", AddRecipients)
	router.PUT("/:sharing-id/recipients", PutRecipients)
	router.DELETE("/:sharing-id/recipients", RevokeSharing)          // On the sharer
	router.DELETE("/:sharing-id/recipients/:index", RevokeRecipient) // On the sharer
	router.DELETE("/:sharing-id/groups/:index", RevokeGroup)         // On the sharer
	router.POST("/:sharing-id/recipients/self/moved", ChangeCozyAddress)
	router.POST("/:sharing-id/recipients/:index/readonly", AddReadOnly)                                      // On the sharer
	router.POST("/:sharing-id/recipients/self/readonly", DowngradeToReadOnly, checkSharingWritePermissions)  // On the recipient
	router.DELETE("/:sharing-id/recipients/:index/readonly", RemoveReadOnly)                                 // On the sharer
	router.DELETE("/:sharing-id/recipients/self/readonly", UpgradeToReadWrite, checkSharingWritePermissions) // On the recipient
	router.DELETE("/:sharing-id", RevocationRecipientNotif)                                                  // On the recipient
	router.DELETE("/:sharing-id/recipients/self", RevokeRecipientBySelf)                                     // On the recipient
	router.DELETE("/:sharing-id/answer", RevocationOwnerNotif)
	router.POST("/:sharing-id/public-key", ReceivePublicKey)

	// Delegated routes for recipient-side sharing operations
	router.POST("/:sharing-id/recipients/delegated", AddRecipientsDelegated, checkSharingPermissions)
	router.POST("/:sharing-id/members/:index/invitation", AddInvitationDelegated, checkSharingWritePermissions)
	router.DELETE("/:sharing-id/groups/:group-index/:member-index", RemoveMemberFromGroup, checkSharingWritePermissions)

	// Misc
	router.GET("/news", CountNewShortcuts)
	router.GET("/doctype/:doctype", GetSharingsInfoByDocType)
	router.GET("/:sharing-id/recipients/:index/avatar", GetAvatar)

	// Register the URL of their Cozy for recipients
	router.GET("/:sharing-id/discovery", GetDiscovery)
	router.POST("/:sharing-id/discovery", PostDiscovery)
	router.POST("/:sharing-id/preview-url", GetPreviewURL)

	// Drives routes
	drivesRoutes(router)

	// Replicator routes
	replicatorRoutes(router)
}

func extractSlugFromSourceID(sourceID string) (string, error) {
	parts := strings.SplitN(sourceID, "/", 2)
	if len(parts) < 2 {
		return "", jsonapi.BadRequest(errors.New("Invalid request"))
	}
	slug := parts[1]
	return slug, nil
}

// checkCreatePermissions checks the sharer's token has all the permissions
// matching the ones defined in the sharing document
func checkCreatePermissions(c echo.Context, s *sharing.Sharing) (string, error) {
	requestPerm, err := middlewares.GetPermission(c)
	if err != nil {
		return "", err
	}
	if requestPerm.Type != permission.TypeWebapp &&
		requestPerm.Type != permission.TypeOauth &&
		requestPerm.Type != permission.TypeCLI {
		return "", permission.ErrInvalidAudience
	}
	for _, r := range s.Rules {
		pr := permission.Rule{
			Title:    r.Title,
			Type:     r.DocType,
			Verbs:    permission.ALL,
			Selector: r.Selector,
			Values:   r.Values,
		}
		if !requestPerm.Permissions.RuleInSubset(pr) {
			return "", echo.NewHTTPError(http.StatusForbidden)
		}
	}
	if requestPerm.Type == permission.TypeCLI {
		return "", nil
	}
	if requestPerm.Type == permission.TypeOauth {
		if requestPerm.Client != nil {
			oauthClient := requestPerm.Client.(*oauth.Client)
			if slug := oauth.GetLinkedAppSlug(oauthClient.SoftwareID); slug != "" {
				return slug, nil
			}
		}
		return "", nil
	}
	return extractSlugFromSourceID(requestPerm.SourceID)
}

// checkGetPermissions checks the requester's token has at least one doctype
// permission declared in the rules of the sharing document
func checkGetPermissions(c echo.Context, s *sharing.Sharing) error {
	requestPerm, err := middlewares.GetPermission(c)
	if err != nil {
		return err
	}

	if requestPerm.SourceID == consts.Sharings+"/"+s.SID {
		if requestPerm.Type == permission.TypeSharePreview ||
			requestPerm.Type == permission.TypeShareInteract {
			return nil
		}
	}
	if requestPerm.Type != permission.TypeWebapp &&
		requestPerm.Type != permission.TypeOauth &&
		requestPerm.Type != permission.TypeCLI {
		return permission.ErrInvalidAudience
	}

	for _, r := range s.Rules {
		pr := permission.Rule{
			Title:    r.Title,
			Type:     r.DocType,
			Verbs:    permission.Verbs(permission.GET),
			Selector: r.Selector,
			Values:   r.Values,
		}
		if requestPerm.Permissions.RuleInSubset(pr) {
			return nil
		}
	}
	return echo.NewHTTPError(http.StatusForbidden)
}

// ClearAppInURL will remove the app slug from the URL of a Cozy.
// Example: https://john-drive.mycozy.cloud/ -> https://john.mycozy.cloud/
func ClearAppInURL(cozyURL string) string {
	u, err := url.Parse(cozyURL)
	if err != nil {
		return cozyURL
	}
	knownDomain := false
	for _, domain := range consts.KnownFlatDomains {
		if strings.HasSuffix(u.Host, domain) {
			knownDomain = true
			break
		}
	}
	if !knownDomain {
		return cozyURL
	}
	parts := strings.SplitN(u.Host, ".", 2)
	sub := parts[0]
	domain := parts[1]
	parts = strings.SplitN(sub, "-", 2)
	u.Host = parts[0] + "." + domain
	return u.String()
}

// wrapErrors returns a formatted error
func wrapErrors(err error) error {
	if merr, ok := err.(*multierror.Error); ok {
		err = merr.WrappedErrors()[0]
	}
	switch err {
	case contact.ErrNoMailAddress:
		return jsonapi.InvalidAttribute("recipients", err)
	case sharing.ErrNoRecipients, sharing.ErrNoRules:
		return jsonapi.BadRequest(err)
	case sharing.ErrTooManyMembers:
		return jsonapi.BadRequest(err)
	case sharing.ErrInvalidURL:
		return jsonapi.InvalidParameter("url", err)
	case sharing.ErrInvalidSharing, sharing.ErrInvalidRule:
		return jsonapi.BadRequest(err)
	case sharing.ErrMemberNotFound:
		return jsonapi.NotFound(err)
	case sharing.ErrInvitationNotSent:
		return jsonapi.BadRequest(err)
	case sharing.ErrRequestFailed:
		return jsonapi.BadGateway(err)
	case sharing.ErrNoOAuthClient:
		return jsonapi.BadRequest(err)
	case sharing.ErrMissingID, sharing.ErrMissingRev:
		return jsonapi.BadRequest(err)
	case sharing.ErrInternalServerError:
		return jsonapi.InternalServerError(err)
	case sharing.ErrMissingFileMetadata:
		return jsonapi.NotFound(err)
	case sharing.ErrFolderNotFound:
		return jsonapi.NotFound(err)
	case sharing.ErrSafety:
		return jsonapi.BadRequest(err)
	case sharing.ErrAlreadyAccepted:
		return jsonapi.Conflict(err)
	case vfs.ErrInvalidHash:
		return jsonapi.InvalidParameter("md5sum", err)
	case vfs.ErrContentLengthMismatch:
		return jsonapi.PreconditionFailed("Content-Length", err)
	case vfs.ErrConflict:
		return jsonapi.Conflict(err)
	case vfs.ErrFileTooBig, vfs.ErrMaxFileSize:
		return jsonapi.Errorf(http.StatusRequestEntityTooLarge, "%s", err)
	case permission.ErrExpiredToken:
		return jsonapi.BadRequest(err)
	case sharing.ErrGroupCannotBeAddedTwice, sharing.ErrMemberAlreadyAdded, sharing.ErrMemberAlreadyInGroup:
		return jsonapi.BadRequest(err)
	case sharing.ErrFolderAlreadyShared:
		return jsonapi.Conflict(err)
	case sharing.ErrNotADirectory, sharing.ErrSystemFolder:
		return jsonapi.InvalidParameter("folder_id", err)
	}
	logger.WithNamespace("sharing").Warnf("Not wrapped error: %s", err)
	return err
}
