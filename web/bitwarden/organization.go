package bitwarden

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/cozy/cozy-stack/model/bitwarden"
	"github.com/cozy/cozy-stack/model/bitwarden/settings"
	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/metadata"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/gofrs/uuid/v5"
	"github.com/labstack/echo/v4"
)

// https://github.com/bitwarden/jslib/blob/master/common/src/models/request/organizationCreateRequest.ts
type organizationRequest struct {
	Name           string `json:"name"`
	Key            string `json:"key"`
	CollectionName string `json:"collectionName"`
}

func (r *organizationRequest) toOrganization(inst *instance.Instance) *bitwarden.Organization {
	md := metadata.New()
	md.DocTypeVersion = bitwarden.DocTypeVersion
	settings, err := inst.SettingsDocument()
	if err != nil {
		settings = &couchdb.JSONDoc{M: map[string]interface{}{}}
	}
	email, _ := settings.M["email"].(string)
	name, _ := settings.M["public_name"].(string)
	return &bitwarden.Organization{
		Name: r.Name,
		Members: map[string]bitwarden.OrgMember{
			inst.Domain: {
				UserID: inst.ID(),
				Email:  email,
				Name:   name,
				OrgKey: r.Key,
				Status: bitwarden.OrgMemberConfirmed,
				Owner:  true,
			},
		},
		Collection: bitwarden.Collection{
			Name: r.CollectionName,
		},
		Metadata: *md,
	}
}

// https://github.com/bitwarden/jslib/blob/master/common/src/models/response/profileOrganizationResponse.ts
type organizationResponse struct {
	ID             string  `json:"Id"`
	Identifier     *string `json:"Identifier"`
	Name           string  `json:"Name"`
	Key            string  `json:"Key"`
	Email          string  `json:"BillingEmail"`
	Plan           string  `json:"Plan"`
	PlanType       int     `json:"PlanType"`
	Seats          int     `json:"Seats"`
	MaxCollections int     `json:"MaxCollections"`
	MaxStorage     int     `json:"MaxStorageGb"`
	SelfHost       bool    `json:"SelfHost"`
	Use2fa         bool    `json:"Use2fa"`
	UseDirectory   bool    `json:"UseDirectory"`
	UseEvents      bool    `json:"UseEvents"`
	UseGroups      bool    `json:"UseGroups"`
	UseTotp        bool    `json:"UseTotp"`
	UseAPI         bool    `json:"UseApi"`
	UsePolicies    bool    `json:"UsePolicies"`
	UseSSO         bool    `json:"UseSSO"`
	UseResetPass   bool    `json:"UseResetPassword"`
	HasKeys        bool    `json:"HasPublicAndPrivateKeys"`
	ResetPass      bool    `json:"ResetPasswordEnrolled"`
	Premium        bool    `json:"UsersGetPremium"`
	Enabled        bool    `json:"Enabled"`
	Status         int     `json:"Status"`
	Type           int     `json:"Type"`
	Object         string  `json:"Object"`
}

func newOrganizationResponse(inst *instance.Instance, org *bitwarden.Organization) *organizationResponse {
	m := org.Member(inst)
	typ := 2 // User
	if m.Owner {
		typ = 0 // Owner
	}
	email := inst.PassphraseSalt()
	return &organizationResponse{
		ID:             org.ID(),
		Identifier:     nil, // Not supported by us
		Name:           org.Name,
		Key:            m.OrgKey,
		Email:          string(email),
		Plan:           "TeamsAnnually",
		PlanType:       9,  // TeamsAnnually plan
		Seats:          10, // The value doesn't matter
		MaxCollections: 1,
		MaxStorage:     1,
		SelfHost:       true,
		Use2fa:         true,
		UseDirectory:   false,
		UseEvents:      false,
		UseGroups:      false,
		UseTotp:        true,
		UseAPI:         false,
		UsePolicies:    false,
		UseSSO:         false,
		UseResetPass:   false,
		HasKeys:        false, // The public/private keys are used for the Admin Reset Password feature, not implemented by us
		ResetPass:      false,
		Premium:        true,
		Enabled:        true,
		Status:         int(m.Status),
		Type:           typ,
		Object:         "profileOrganization",
	}
}

// https://github.com/bitwarden/jslib/blob/master/common/src/models/response/collectionResponse.ts
// We deviate from the Bitwarden's protocol by adding ReadOnly field
// On Bitwarden's protocol this field is present only on collectionDetailsResponse
// but we merged both structs in a single one
// Bitwarden app uses this struct only for exporting ciphers, so they don't need ReadOnly member
// Cozy app uses this struct for realtime syncing and so it needs to have the ReadOnly state
type collectionResponse struct {
	ID             string `json:"Id"`
	OrganizationID string `json:"OrganizationId"`
	Name           string `json:"Name"`
	Object         string `json:"Object"`
	ReadOnly       bool   `json:"ReadOnly"`
}

func newCollectionResponse(inst *instance.Instance, org *bitwarden.Organization, coll *bitwarden.Collection) *collectionResponse {
	m := org.Member(inst)

	return &collectionResponse{
		ID:             coll.ID(),
		OrganizationID: org.ID(),
		Name:           coll.Name,
		Object:         "collection",
		ReadOnly:       m.ReadOnly,
	}
}

// CreateOrganization is the route used to create an organization (with a
// collection).
func CreateOrganization(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	if err := middlewares.AllowWholeType(c, permission.POST, consts.BitwardenOrganizations); err != nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "invalid token",
		})
	}

	var req organizationRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": "invalid JSON",
		})
	}
	if req.Name == "" {
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": "missing name",
		})
	}

	org := req.toOrganization(inst)
	collID, err := uuid.NewV7()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err.Error(),
		})
	}
	org.Collection.DocID = collID.String()
	if err := couchdb.CreateDoc(inst, org); err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err.Error(),
		})
	}

	_ = settings.UpdateRevisionDate(inst, nil)
	res := newOrganizationResponse(inst, org)
	return c.JSON(http.StatusOK, res)
}

// GetOrganization is the route for getting information about an organization.
func GetOrganization(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	if err := middlewares.AllowWholeType(c, permission.GET, consts.BitwardenOrganizations); err != nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "invalid token",
		})
	}

	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusNotFound, echo.Map{
			"error": "missing id",
		})
	}

	org := &bitwarden.Organization{}
	if err := couchdb.GetDoc(inst, consts.BitwardenOrganizations, id, org); err != nil {
		if couchdb.IsNotFoundError(err) {
			return c.JSON(http.StatusNotFound, echo.Map{
				"error": "not found",
			})
		}
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err.Error(),
		})
	}

	res := newOrganizationResponse(inst, org)
	return c.JSON(http.StatusOK, res)
}

type collectionsList struct {
	Data   []*collectionResponse `json:"Data"`
	Object string                `json:"Object"`
}

// GetCollections is the route for getting information about the collections
// inside an organization.
func GetCollections(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	if err := middlewares.AllowWholeType(c, permission.GET, consts.BitwardenOrganizations); err != nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "invalid token",
		})
	}

	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusNotFound, echo.Map{
			"error": "missing id",
		})
	}

	org := &bitwarden.Organization{}
	if err := couchdb.GetDoc(inst, consts.BitwardenOrganizations, id, org); err != nil {
		if couchdb.IsNotFoundError(err) {
			return c.JSON(http.StatusNotFound, echo.Map{
				"error": "not found",
			})
		}
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err.Error(),
		})
	}

	coll := newCollectionResponse(inst, org, &org.Collection)
	res := &collectionsList{Object: "list"}
	res.Data = []*collectionResponse{coll}
	return c.JSON(http.StatusOK, res)
}

// https://github.com/bitwarden/jslib/blob/master/common/src/models/request/passwordVerificationRequest.ts
type passwordVerificationRequest struct {
	Hash string `json:"masterPasswordHash"`
}

// DeleteOrganization is the route for deleting an organization by its owner.
func DeleteOrganization(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	if err := middlewares.AllowWholeType(c, permission.DELETE, consts.BitwardenOrganizations); err != nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "invalid token",
		})
	}

	var verification passwordVerificationRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&verification); err != nil {
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": "invalid JSON",
		})
	}
	if err := instance.CheckPassphrase(inst, []byte(verification.Hash)); err != nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "invalid password",
		})
	}

	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusNotFound, echo.Map{
			"error": "missing id",
		})
	}

	org := &bitwarden.Organization{}
	if err := couchdb.GetDoc(inst, consts.BitwardenOrganizations, id, org); err != nil {
		if couchdb.IsNotFoundError(err) {
			return c.JSON(http.StatusNotFound, echo.Map{
				"error": "not found",
			})
		}
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err.Error(),
		})
	}

	if m := org.Member(inst); !m.Owner {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "only the Owner can call this endpoint",
		})
	}

	if err := org.Delete(inst); err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err.Error(),
		})
	}

	_ = settings.UpdateRevisionDate(inst, nil)
	return c.NoContent(http.StatusOK)
}

// https://github.com/bitwarden/jslib/blob/master/common/src/models/response/organizationUserResponse.ts
type userDetailsResponse struct {
	ID        string                    `json:"Id"`
	UserID    string                    `json:"UserId"`
	Type      int                       `json:"Type"`
	Status    bitwarden.OrgMemberStatus `json:"Status"`
	AccessAll bool                      `json:"AccessAll"`
	Name      string                    `json:"Name"`
	Email     string                    `json:"Email"`
	Object    string                    `json:"Object"`
}

func newUserDetailsResponse(m *bitwarden.OrgMember) *userDetailsResponse {
	typ := 2 // User
	if m.Owner {
		typ = 0 // Owner
	}
	return &userDetailsResponse{
		ID:        m.UserID,
		UserID:    m.UserID,
		Type:      typ,
		Status:    m.Status,
		AccessAll: true,
		Name:      m.Name,
		Email:     m.Email,
		Object:    "organizationUserUserDetails",
	}
}

type userDetailsList struct {
	Data   []*userDetailsResponse `json:"Data"`
	Object string                 `json:"Object"`
}

// ListOrganizationUser is the route for listing users inside an organization.
func ListOrganizationUser(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	if err := middlewares.AllowWholeType(c, permission.GET, consts.BitwardenOrganizations); err != nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "invalid token",
		})
	}

	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusNotFound, echo.Map{
			"error": "missing id",
		})
	}

	org := &bitwarden.Organization{}
	if err := couchdb.GetDoc(inst, consts.BitwardenOrganizations, id, org); err != nil {
		if couchdb.IsNotFoundError(err) {
			return c.JSON(http.StatusNotFound, echo.Map{
				"error": "not found",
			})
		}
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err.Error(),
		})
	}

	list := &userDetailsList{Object: "list"}
	for _, m := range org.Members {
		list.Data = append(list.Data, newUserDetailsResponse(&m))
	}
	return c.JSON(http.StatusOK, list)
}

// https://github.com/bitwarden/jslib/blob/master/common/src/models/request/organizationUserConfirmRequest.ts
type userConfirmRequest struct {
	Key string `json:"key"`
}

// ConfirmUser is the route to confirm a user in an organization. It takes the
// organization key encrypted with the public key of this user as input.
func ConfirmUser(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	if err := middlewares.AllowWholeType(c, permission.POST, consts.BitwardenOrganizations); err != nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "invalid token",
		})
	}

	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusNotFound, echo.Map{
			"error": "missing id",
		})
	}
	org := &bitwarden.Organization{}
	if err := couchdb.GetDoc(inst, consts.BitwardenOrganizations, id, org); err != nil {
		if couchdb.IsNotFoundError(err) {
			return c.JSON(http.StatusNotFound, echo.Map{
				"error": "not found",
			})
		}
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err.Error(),
		})
	}
	if m := org.Member(inst); !m.Owner {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "only the Owner can call this endpoint",
		})
	}

	var confirm userConfirmRequest
	err := json.NewDecoder(c.Request().Body).Decode(&confirm)
	if err != nil || confirm.Key == "" {
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": "invalid JSON",
		})
	}

	userID := c.Param("user-id")
	var bwContact bitwarden.Contact
	if err := couchdb.GetDoc(inst, consts.BitwardenContacts, userID, &bwContact); err != nil {
		if couchdb.IsNotFoundError(err) {
			return c.JSON(http.StatusNotFound, echo.Map{
				"error": "not found",
			})
		}
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err.Error(),
		})
	}

	found := false
	for domain, member := range org.Members {
		if member.UserID != userID {
			continue
		}
		if member.Status == bitwarden.OrgMemberAccepted {
			member.Status = bitwarden.OrgMemberConfirmed
		} else if member.Status != bitwarden.OrgMemberInvited {
			return c.JSON(http.StatusBadRequest, echo.Map{
				"error": "User in invalid state",
			})
		}
		found = true
		member.OrgKey = confirm.Key
		org.Members[domain] = member
	}
	if !found {
		card, err := contact.FindByEmail(inst, bwContact.Email)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, echo.Map{
				"error": err.Error(),
			})
		}
		domain := card.PrimaryCozyURL()
		if domain == "" {
			return c.JSON(http.StatusInternalServerError, echo.Map{
				"error": "Unknown Cozy URL for this user",
			})
		}
		if u, err := url.Parse(domain); err == nil {
			domain = u.Host
		}
		org.Members[domain] = bitwarden.OrgMember{
			UserID:   bwContact.UserID,
			Email:    bwContact.Email,
			Name:     card.PrimaryName(),
			OrgKey:   confirm.Key,
			Status:   bitwarden.OrgMemberInvited,
			Owner:    false,
			ReadOnly: true, // it will be overwritten when the user accepts the sharing
		}
	}

	if !bwContact.Confirmed {
		bwContact.Confirmed = true
		bwContact.Metadata.UpdatedAt = time.Now()
		if err := couchdb.UpdateDoc(inst, &bwContact); err != nil {
			return c.JSON(http.StatusInternalServerError, echo.Map{
				"error": err.Error(),
			})
		}
	}

	if err := couchdb.UpdateDoc(inst, org); err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err.Error(),
		})
	}

	return c.NoContent(http.StatusOK)
}

// GetPublicKey returns the public key of a user.
func GetPublicKey(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	if err := middlewares.AllowWholeType(c, permission.GET, consts.BitwardenOrganizations); err != nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "invalid token",
		})
	}

	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusNotFound, echo.Map{
			"error": "missing id",
		})
	}
	var contact bitwarden.Contact
	if err := couchdb.GetDoc(inst, consts.BitwardenContacts, id, &contact); err != nil {
		if couchdb.IsNotFoundError(err) {
			return c.JSON(http.StatusNotFound, echo.Map{
				"error": "not found",
			})
		}
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err.Error(),
		})
	}

	return c.JSON(http.StatusOK, echo.Map{
		"UserId":    id,
		"PublicKey": contact.PublicKey,
	})
}
