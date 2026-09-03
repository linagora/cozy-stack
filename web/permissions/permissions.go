// Package permissions is the HTTP handlers for managing the permissions on a
// Cozy (creating a share by link for example).
package permissions

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/metadata"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/gofrs/uuid/v5"
	"github.com/justincampbell/bigduration"
	"github.com/labstack/echo/v4"
)

// ErrPatchCodeOrSet is returned when an attempt is made to patch both
// code & set in one request
var ErrPatchCodeOrSet = echo.NewHTTPError(http.StatusBadRequest,
	"The patch doc should have property 'codes' or 'permissions', not both")

// ContextPermissionSet is the key used in echo context to store permissions set
const ContextPermissionSet = "permissions_set"

// ContextClaims is the key used in echo context to store claims
const ContextClaims = "token_claims"

// APIPermission is the struct that will be used to serialized a permission to
// JSON-API
type APIPermission struct {
	*permission.Permission
	included []jsonapi.Object
}

// MarshalJSON implements jsonapi.Doc
func (p *APIPermission) MarshalJSON() ([]byte, error) {
	return json.Marshal(p.Permission)
}

// Relationships implements jsonapi.Doc
func (p *APIPermission) Relationships() jsonapi.RelationshipMap { return nil }

// Included implements jsonapi.Doc
func (p *APIPermission) Included() []jsonapi.Object { return p.included }

// Links implements jsonapi.Doc
func (p *APIPermission) Links() *jsonapi.LinksList {
	links := &jsonapi.LinksList{Self: "/permissions/" + p.PID}
	parts := strings.SplitN(p.SourceID, "/", 2)
	if parts[0] == consts.Sharings {
		links.Related = "/sharings/" + parts[1]
	}
	return links
}

type apiMember struct {
	*sharing.Member
}

func (m *apiMember) ID() string                             { return "" }
func (m *apiMember) Rev() string                            { return "" }
func (m *apiMember) SetID(id string)                        {}
func (m *apiMember) SetRev(rev string)                      {}
func (m *apiMember) DocType() string                        { return consts.SharingsMembers }
func (m *apiMember) Clone() couchdb.Doc                     { cloned := *m; return &cloned }
func (m *apiMember) Relationships() jsonapi.RelationshipMap { return nil }
func (m *apiMember) Included() []jsonapi.Object             { return nil }
func (m *apiMember) Links() *jsonapi.LinksList              { return nil }

type getPermsFunc func(db prefixer.Prefixer, id string) (*permission.Permission, error)

func displayPermissions(c echo.Context) error {
	doc, err := middlewares.GetPermission(c)
	if err != nil {
		return err
	}

	// Include the sharing member (when relevant)
	var included []jsonapi.Object
	if doc.Type == permission.TypeSharePreview || doc.Type == permission.TypeShareInteract {
		inst := middlewares.GetInstance(c)
		sharingID := strings.TrimPrefix(doc.SourceID, consts.Sharings+"/")
		if s, err := sharing.FindSharing(inst, sharingID); err == nil {
			sharecode := middlewares.GetRequestToken(c)
			if member, err := s.FindMemberByCode(doc, sharecode); err == nil {
				included = []jsonapi.Object{&apiMember{member}}
			}
		}
	}

	// XXX hides the codes and password hash in the response
	doc.Codes = nil
	doc.ShortCodes = nil
	if doc.Password != nil {
		doc.Password = true
	}
	return jsonapi.Data(c, http.StatusOK, &APIPermission{Permission: doc, included: included}, nil)
}

func createPermission(c echo.Context) error {
	parent, err := middlewares.GetPermission(c)
	if err != nil {
		return err
	}
	// Share-interact tokens are limited to shared-drive permission endpoints.
	if parent != nil && parent.Type == permission.TypeShareInteract {
		return permission.ErrOnlyAppCanCreateSubSet
	}

	inst := middlewares.GetInstance(c)
	opts := CreateShareByLinkOptions{}
	if claims, ok := c.Get("claims").(permission.Claims); ok && claims.Issuer != "" {
		domain := claims.Issuer
		opts.CreatorDomain = &domain
	}
	return HandleCreateShareByLink(c, inst, opts)
}

// ValidatePermissionsFn is a function that validates permission rules.
// Used by HandleCreateShareByLink to allow additional validation on top of
// the standard permission checks (e.g., verifying files are within a shared drive).
type ValidatePermissionsFn func(perms permission.Set) error

// CreateShareByLinkOptions configures HandleCreateShareByLink behavior.
type CreateShareByLinkOptions struct {
	// runs additional validations after request binding and
	ValidatePermissions ValidatePermissionsFn
	// controls whether CreateShareSet's standard permission validation is skipped.
	SkipValidation bool
	// overrides the app slug stored in metadata.
	CreatorSlug *string
	// overrides the creator domain stored in metadata.
	CreatorDomain *string
}

// HandleCreateShareByLink is the common handler for creating share-by-link permissions.
// It handles parsing, optional additional validation, creation via CreateShareSet, and response formatting.
func HandleCreateShareByLink(c echo.Context, inst *instance.Instance, opts CreateShareByLinkOptions) error {
	var subdoc permission.Permission
	if _, err := jsonapi.Bind(c.Request().Body, &subdoc); err != nil {
		return err
	}

	// Run additional validation if provided (e.g., for shared drives)
	if opts.ValidatePermissions != nil {
		if err := opts.ValidatePermissions(subdoc.Permissions); err != nil {
			return err
		}
	}

	parent, err := middlewares.GetPermission(c)
	if err != nil {
		return err
	}
	if parent == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "no parent")
	}

	var creatorDomain string
	if opts.CreatorDomain != nil {
		creatorDomain = *opts.CreatorDomain
	}

	var slug string
	if opts.CreatorSlug != nil {
		slug = *opts.CreatorSlug
	}
	sourceID := parent.SourceID
	// Check if the permission is linked to an OAuth Client
	if slug == "" && parent.Client != nil {
		oauthClient := parent.Client.(*oauth.Client)
		if slug = oauth.GetLinkedAppSlug(oauthClient.SoftwareID); slug != "" {
			// Changing the sourceID from the OAuth clientID to the classic
			// io.cozy.apps/slug one
			sourceID = consts.Apps + "/" + slug
		}
	}

	// Getting the slug from the token if it has not been retrieved before
	// with the linkedapp
	if slug == "" {
		if claims, ok := c.Get("claims").(permission.Claims); ok {
			switch claims.AudienceString() {
			case consts.AppAudience, consts.KonnectorAudience, consts.AccessTokenAudience:
				slug = claims.Subject
			}
		}
	}

	names := strings.Split(c.QueryParam("codes"), ",")
	ttl := c.QueryParam("ttl")
	tiny, _ := strconv.ParseBool(c.QueryParam("tiny"))
	permissionID, err := uuid.NewV7()
	if err != nil {
		return err
	}
	subdoc.SetID(permissionID.String())

	// Process TTL/expiration
	var expiresAt interface{}
	if ttl != "" {
		if d, errd := bigduration.ParseDuration(ttl); errd == nil {
			ex := time.Now().Add(d)
			expiresAt = &ex
			if d.Hours() > 1.0 && tiny {
				inst.Logger().Info("Tiny can not be set to true since duration > 1h")
				tiny = false
			}
		}
	} else {
		tiny = false
		if at, ok := subdoc.ExpiresAt.(string); ok {
			expires, err := time.Parse(time.RFC3339, at)
			if err != nil {
				return jsonapi.InvalidAttribute("expires_at", err)
			}
			expiresAt = &expires
		}
	}

	var codes map[string]string
	var shortcodes map[string]string

	if names != nil {
		codes = make(map[string]string, len(names))
		shortcodes = make(map[string]string, len(names))
		for _, name := range names {
			longcode, err := inst.CreateShareCode(name, subdoc.ID())
			if err != nil {
				return err
			}
			codes[name] = longcode
			shortcodes[name] = createShortCode(tiny)
		}
	}

	// Handles the metadata part
	md, err := metadata.NewWithApp(slug, "", permission.DocTypeVersion)
	if err == metadata.ErrSlugEmpty {
		md = metadata.New()
		md.DocTypeVersion = permission.DocTypeVersion
	} else if err != nil {
		return err
	}

	// Adding metadata if it does not exist
	if subdoc.Metadata == nil {
		subdoc.Metadata = md
	} else { // Otherwise, ensure we have all the needed fields
		subdoc.Metadata.EnsureCreatedFields(md)
	}

	// Track creator's instance domain for shared drive permissions.
	if subdoc.Metadata != nil && subdoc.Metadata.UpdatedByApps != nil && len(subdoc.Metadata.UpdatedByApps) > 0 {
		if creatorDomain != "" {
			subdoc.Metadata.UpdatedByApps[0].Instance = creatorDomain
		}
	}

	// Use CreateShareSet which handles password hashing and doc creation.
	// Pass skipValidation to control whether standard validation is performed.
	pdoc, err := permission.CreateShareSet(inst, parent, sourceID, codes, shortcodes, subdoc, expiresAt, opts.SkipValidation)
	if err != nil {
		return err
	}

	// Don't send the password hash to the client
	if pdoc.Password != nil {
		pdoc.Password = true
	}

	return jsonapi.Data(c, http.StatusOK, &APIPermission{Permission: pdoc}, nil)
}

func createShortCode(tiny bool) string {
	if tiny {
		return crypto.GenerateRandomSixDigits()
	}
	return crypto.GenerateRandomString(consts.ShortCodeLen)
}

const (
	defaultPermissionsByDoctype = 30
	maxPermissionsByDoctype     = 100
)

func listPermissionsByDoctype(c echo.Context, route, permType string) error {
	ins := middlewares.GetInstance(c)
	doctype := c.Param("doctype")
	if doctype == "" {
		return jsonapi.NewError(http.StatusBadRequest, "Missing doctype")
	}

	current, err := middlewares.GetPermission(c)
	if err != nil {
		return err
	}

	if !current.Permissions.AllowWholeType(http.MethodGet, doctype) {
		return jsonapi.NewError(http.StatusForbidden,
			"You need GET permission on whole type to list its permissions")
	}

	cursor, err := jsonapi.ExtractPaginationCursor(c, defaultPermissionsByDoctype, maxPermissionsByDoctype)
	if err != nil {
		return err
	}

	perms, err := permission.GetPermissionsByDoctype(ins, permType, doctype, cursor)
	if err != nil {
		return err
	}

	links := &jsonapi.LinksList{}
	if cursor.HasMore() {
		params, err := jsonapi.PaginationCursorToParams(cursor)
		if err != nil {
			return err
		}
		links.Next = fmt.Sprintf("/permissions/doctype/%s/%s?%s",
			doctype, route, params.Encode())
	}

	out := make([]jsonapi.Object, len(perms))
	for i := range perms {
		perm := &perms[i]
		if perm.Password != nil {
			perm.Password = true
		}
		out[i] = &APIPermission{Permission: perm}
	}

	return jsonapi.DataList(c, http.StatusOK, out, links)
}

func listByLinkPermissionsByDoctype(c echo.Context) error {
	return listPermissionsByDoctype(c, "shared-by-link", permission.TypeShareByLink)
}

type refAndVerb struct {
	ID      string              `json:"id"`
	DocType string              `json:"type"`
	Verbs   *permission.VerbSet `json:"verbs"`
}

func listPermissions(c echo.Context) error {
	instance := middlewares.GetInstance(c)

	references, err := jsonapi.BindRelations(c.Request())
	if err != nil {
		return err
	}
	ids := make(map[string][]string)
	for _, ref := range references {
		idSlice, ok := ids[ref.Type]
		if !ok {
			idSlice = []string{}
		}
		ids[ref.Type] = append(idSlice, ref.ID)
	}

	var out []refAndVerb
	for doctype, idSlice := range ids {
		result, err2 := permission.GetPermissionsForIDs(instance, doctype, idSlice)
		if err2 != nil {
			return err2
		}
		for id, verbs := range result {
			out = append(out, refAndVerb{id, doctype, verbs})
		}
	}

	data, err := json.Marshal(out)
	if err != nil {
		return err
	}
	doc := jsonapi.Document{
		Data: (*json.RawMessage)(&data),
	}
	resp := c.Response()
	resp.Header().Set(echo.HeaderContentType, jsonapi.ContentType)
	resp.WriteHeader(http.StatusOK)
	return json.NewEncoder(resp).Encode(doc)
}

func showPermissions(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	current, err := middlewares.GetPermission(c)
	if err != nil {
		return err
	}

	doc, err := permission.GetByID(inst, c.Param("permdocid"))
	if err != nil {
		return err
	}

	if doc.ID() != current.ID() && doc.SourceID != current.SourceID {
		if err := middlewares.AllowMaximal(c); err != nil {
			return middlewares.ErrForbidden
		}
	}

	// XXX hides the codes and password hash in the response
	doc.Codes = nil
	doc.ShortCodes = nil
	if doc.Password != nil {
		doc.Password = true
	}
	return jsonapi.Data(c, http.StatusOK, &APIPermission{Permission: doc}, nil)
}

func patchPermission(getPerms getPermsFunc, paramName string) echo.HandlerFunc {
	return func(c echo.Context) error {
		instance := middlewares.GetInstance(c)
		current, err := middlewares.GetPermission(c)
		if err != nil {
			return err
		}

		var patch permission.Permission
		if _, err = jsonapi.Bind(c.Request().Body, &patch); err != nil {
			return err
		}

		patchSet := patch.Permissions != nil && len(patch.Permissions) > 0
		patchCodes := len(patch.Codes) > 0

		toPatch, err := getPerms(instance, c.Param(paramName))
		if err != nil {
			return err
		}

		if !patchSet && !current.CanUpdateShareByLink(toPatch) {
			return permission.ErrNotParent
		}

		if patchCodes == patchSet {
			if patchSet {
				return ErrPatchCodeOrSet
			}
			if patch.Password == nil && patch.ExpiresAt == nil {
				return ErrPatchCodeOrSet
			}
		}

		if pass, ok := patch.Password.(string); ok {
			if pass == "" {
				toPatch.Password = nil
			} else {
				hash, err := crypto.GenerateFromPassphrase([]byte(pass))
				if err != nil {
					return err
				}
				toPatch.Password = hash
			}
		}
		if at, ok := patch.ExpiresAt.(string); ok {
			if patch.ExpiresAt == "" {
				toPatch.ExpiresAt = nil
			} else {
				expiresAt, err := time.Parse(time.RFC3339, at)
				if err != nil {
					return jsonapi.InvalidAttribute("expires_at", err)
				}
				toPatch.ExpiresAt = expiresAt
			}
		}

		if patchCodes {
			toPatch.PatchCodes(patch.Codes)
		}

		if patchSet {
			for _, r := range patch.Permissions {
				if r.Type == "" {
					toPatch.RemoveRule(r)
				} else if err := permission.CheckDoctypeName(r.Type, true); err != nil {
					return err
				} else if current.Permissions.RuleInSubset(r) {
					toPatch.AddRules(r)
				} else {
					return permission.ErrNotSubset
				}
			}
		}

		// Handle metadata
		// If the metadata has been given in the body request, just apply it to
		// the patch
		if patch.Metadata != nil {
			toPatch.Metadata = patch.Metadata
			patch.Metadata.EnsureCreatedFields(toPatch.Metadata)
		} else if toPatch.Metadata != nil { // No metadata given in the request, but it does exist in the database: update it
			// Using the token Subject for update
			claims := c.Get("claims").(permission.Claims)
			err = toPatch.Metadata.UpdatedByApp(claims.Subject, "")
			if err != nil {
				return err
			}
		}

		if err = couchdb.UpdateDoc(instance, toPatch); err != nil {
			return err
		}

		// Don't send the password hash to the client
		if toPatch.Password != nil {
			toPatch.Password = true
		}

		return jsonapi.Data(c, http.StatusOK, &APIPermission{Permission: toPatch}, nil)
	}
}

func revokePermission(c echo.Context) error {
	instance := middlewares.GetInstance(c)

	current, err := middlewares.GetPermission(c)
	if err != nil {
		return err
	}

	toRevoke, err := permission.GetPermissionByIDIncludingExpired(instance, c.Param("permdocid"))
	if err != nil {
		return err
	}

	if !current.CanUpdateShareByLink(toRevoke) {
		return permission.ErrNotParent
	}

	err = toRevoke.Revoke(instance)
	if err != nil {
		return err
	}

	return c.NoContent(http.StatusNoContent)
}

// Routes sets the routing for the permissions service
func Routes(router *echo.Group) {
	// API Routes
	router.POST("", createPermission)
	router.GET("/self", displayPermissions)
	router.POST("/exists", listPermissions)
	router.GET("/:permdocid", showPermissions)
	router.PATCH("/:permdocid", patchPermission(permission.GetPermissionByIDIncludingExpired, "permdocid"))
	router.DELETE("/:permdocid", revokePermission)

	router.PATCH("/apps/:slug", patchPermission(permission.GetForWebapp, "slug"))
	router.PATCH("/konnectors/:slug", patchPermission(permission.GetForKonnector, "slug"))

	router.GET("/doctype/:doctype/shared-by-link", listByLinkPermissionsByDoctype)

	// Legacy routes, kept here for compatibility reasons
	router.GET("/doctype/:doctype/sharedByLink", listByLinkPermissionsByDoctype)
}
