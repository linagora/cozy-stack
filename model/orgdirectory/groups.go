package orgdirectory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

// GroupCreatedMessage is the payload for b2b.group.created.
type GroupCreatedMessage struct {
	Timestamp      time.Time     `json:"timestamp"`
	OrganizationID string        `json:"organizationId"`
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	Description    string        `json:"description"`
	Color          string        `json:"color"`
	CreatedAt      time.Time     `json:"createdAt"`
	Members        []GroupMember `json:"members"`
}

// GroupUpdatedMessage is the payload for b2b.group.updated.
type GroupUpdatedMessage struct {
	Timestamp      time.Time `json:"timestamp"`
	OrganizationID string    `json:"organizationId"`
	ID             string    `json:"id"`
	Name           *string   `json:"name,omitempty"`
	Description    *string   `json:"description,omitempty"`
	Color          *string   `json:"color,omitempty"`
}

// GroupDeletedMessage is the payload for b2b.group.deleted.
type GroupDeletedMessage struct {
	Timestamp      time.Time `json:"timestamp"`
	OrganizationID string    `json:"organizationId"`
	ID             string    `json:"id"`
}

// GroupMembersMessage is the payload for b2b.group.member.added and
// b2b.group.member.removed.
type GroupMembersMessage struct {
	Timestamp      time.Time     `json:"timestamp"`
	OrganizationID string        `json:"organizationId"`
	ID             string        `json:"id"`
	Members        []GroupMember `json:"members"`
}

// GroupMember describes a B2B user present in a group membership event.
type GroupMember struct {
	Username      string `json:"username"`
	Email         string `json:"email"`
	FirstName     string `json:"firstName"`
	LastName      string `json:"lastName"`
	WorkplaceFQDN string `json:"workplaceFqdn"`
}

// GroupPatch describes the local fields to create or update on a replicated
// organization-directory group.
type GroupPatch struct {
	OrganizationID string
	ExternalID     string
	Name           *string
	Description    *string
	Color          *string
	CreatedAt      *time.Time
}

// GroupDocID returns the stable local document ID used for a replicated B2B
// group. Raw external IDs stay in metadata for traceability.
func GroupDocID(organizationID, externalID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(organizationID) + "\x00" + strings.TrimSpace(externalID)))
	return "b2b-group-" + hex.EncodeToString(sum[:16])
}

// SyncGroupCreated replicates a B2B group and its initial members to every
// instance in the organization.
func SyncGroupCreated(ctx context.Context, msg GroupCreatedMessage) error {
	if err := validateGroupIdentity("b2b.group.created", msg.OrganizationID, msg.ID); err != nil {
		return err
	}
	name := strings.TrimSpace(msg.Name)
	if name == "" {
		return fmt.Errorf("b2b.group.created: missing name")
	}
	createdAt := msg.CreatedAt
	fields := GroupPatch{
		OrganizationID: strings.TrimSpace(msg.OrganizationID),
		ExternalID:     strings.TrimSpace(msg.ID),
		Name:           &name,
		Description:    nonEmptyStringPtr(msg.Description),
		Color:          nonEmptyStringPtr(msg.Color),
		CreatedAt:      &createdAt,
	}
	return forEachOrgInstance(ctx, "b2b.group.created", msg.OrganizationID, func(inst *instance.Instance) error {
		if err := upsertGroup(inst, fields); err != nil {
			return err
		}
		return addMembersToGroup(ctx, inst, msg.OrganizationID, msg.ID, msg.Members)
	})
}

// SyncGroupUpdated partially updates a B2B group in every instance in the
// organization.
func SyncGroupUpdated(ctx context.Context, msg GroupUpdatedMessage) error {
	if err := validateGroupIdentity("b2b.group.updated", msg.OrganizationID, msg.ID); err != nil {
		return err
	}
	fields := GroupPatch{
		OrganizationID: strings.TrimSpace(msg.OrganizationID),
		ExternalID:     strings.TrimSpace(msg.ID),
		Name:           cleanStringPtr(msg.Name),
		Description:    cleanStringPtr(msg.Description),
		Color:          cleanStringPtr(msg.Color),
	}
	return forEachOrgInstance(ctx, "b2b.group.updated", msg.OrganizationID, func(inst *instance.Instance) error {
		return upsertGroup(inst, fields)
	})
}

// SyncGroupDeleted removes a B2B group and first removes its relationship from
// replicated contacts so existing sharing-group triggers see membership deltas.
func SyncGroupDeleted(ctx context.Context, msg GroupDeletedMessage) error {
	if err := validateGroupIdentity("b2b.group.deleted", msg.OrganizationID, msg.ID); err != nil {
		return err
	}
	groupID := GroupDocID(msg.OrganizationID, msg.ID)
	return forEachOrgInstance(ctx, "b2b.group.deleted", msg.OrganizationID, func(inst *instance.Instance) error {
		if err := removeGroupFromManagedContacts(inst, msg.OrganizationID, groupID); err != nil {
			return err
		}
		group, err := contact.FindGroup(inst, groupID)
		if couchdb.IsNoDatabaseError(err) || couchdb.IsNotFoundError(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !IsManagedDirectoryDoc(&group.JSONDoc) {
			return fmt.Errorf("b2b.group.deleted: refusing to delete unmanaged group %s on %s", groupID, inst.Domain)
		}
		return couchdb.DeleteDoc(inst, group)
	})
}

// SyncGroupMembersAdded adds members to a B2B group in every instance in the
// organization.
func SyncGroupMembersAdded(ctx context.Context, msg GroupMembersMessage) error {
	if err := validateMembersMessage("b2b.group.member.added", msg); err != nil {
		return err
	}
	return forEachOrgInstance(ctx, "b2b.group.member.added", msg.OrganizationID, func(inst *instance.Instance) error {
		if err := ensureGroupExists(inst, msg.OrganizationID, msg.ID); err != nil {
			return err
		}
		return addMembersToGroup(ctx, inst, msg.OrganizationID, msg.ID, msg.Members)
	})
}

// SyncGroupMembersRemoved removes a B2B group relationship from member contacts.
func SyncGroupMembersRemoved(ctx context.Context, msg GroupMembersMessage) error {
	if err := validateMembersMessage("b2b.group.member.removed", msg); err != nil {
		return err
	}
	groupID := GroupDocID(msg.OrganizationID, msg.ID)
	return forEachOrgInstance(ctx, "b2b.group.member.removed", msg.OrganizationID, func(inst *instance.Instance) error {
		var errs []error
		for _, member := range msg.Members {
			if err := ctx.Err(); err != nil {
				return err
			}
			input := contactPatchFromMember(msg.OrganizationID, member)
			c, err := findManagedContact(inst, input)
			if errors.Is(err, contact.ErrNotFound) {
				continue
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
			changed, err := removeContactGroup(c, groupID)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if changed {
				if err := couchdb.UpdateDoc(inst, c); err != nil {
					errs = append(errs, err)
				}
			}
		}
		return errors.Join(errs...)
	})
}

// CopyOrgDirectoryFromOrgInstance copies current managed group/contact replicas
// from the organization instance to a newly created member instance. LDAP/B2B
// remains authoritative; the organization instance is only a local copy source
// for late onboarding.
func CopyOrgDirectoryFromOrgInstance(ctx context.Context, target *instance.Instance, organizationID string) error {
	organizationID = strings.TrimSpace(organizationID)
	if organizationID == "" || target == nil {
		return nil
	}
	orgInst, err := findOrganizationInstance(ctx, organizationID)
	if err != nil {
		return err
	}
	if orgInst == nil || orgInst.Domain == target.Domain {
		return nil
	}

	if err := copyManagedGroups(ctx, orgInst, target, organizationID); err != nil {
		return err
	}
	return copyManagedContactsWithGroups(ctx, orgInst, target, organizationID)
}

func copyManagedGroups(ctx context.Context, source, target *instance.Instance, organizationID string) error {
	groups, err := listManagedGroups(source, organizationID)
	if err != nil {
		return fmt.Errorf("copy org directory groups from %s: %w", source.Domain, err)
	}
	for _, g := range groups {
		if err := ctx.Err(); err != nil {
			return err
		}
		g.SetRev("")
		if err := couchdb.Upsert(target, g); err != nil {
			return fmt.Errorf("copy group %s to %s: %w", g.ID(), target.Domain, err)
		}
	}
	return nil
}

func copyManagedContactsWithGroups(ctx context.Context, source, target *instance.Instance, organizationID string) error {
	contacts, err := listManagedContacts(source, organizationID)
	if err != nil {
		return fmt.Errorf("copy org directory contacts from %s: %w", source.Domain, err)
	}
	for _, c := range contacts {
		if err := ctx.Err(); err != nil {
			return err
		}
		input := contactPatchFromContact(organizationID, c)
		if input.shouldSkipOwn(target) {
			continue
		}
		// Contacts are copied with their group refs to restore historical
		// memberships that existed before the target instance was created.
		stored, err := UpsertManagedContact(target, input)
		if err != nil {
			return fmt.Errorf("copy contact on %s: %w", target.Domain, err)
		}
		changed, err := addContactGroups(stored, c.GroupIDs())
		if err != nil {
			return fmt.Errorf("copy contact groups on %s: %w", target.Domain, err)
		}
		if changed {
			if err := couchdb.UpdateDoc(target, stored); err != nil {
				return fmt.Errorf("copy contact groups %s on %s: %w", stored.ID(), target.Domain, err)
			}
		}
	}
	return nil
}

func forEachOrgInstance(ctx context.Context, eventName, organizationID string, fn func(*instance.Instance) error) error {
	scope, err := ResolveOrganizationInstances(organizationID, "")
	if err != nil {
		return fmt.Errorf("%s: %w", eventName, err)
	}
	var errs []error
	for _, inst := range scope.Instances {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(inst); err != nil {
			errs = append(errs, fmt.Errorf("%s on %s: %w", eventName, inst.Domain, err))
		}
	}
	return errors.Join(errs...)
}

func upsertGroup(inst *instance.Instance, fields GroupPatch) error {
	groupID := GroupDocID(fields.OrganizationID, fields.ExternalID)
	doc := contact.NewGroup()
	doc.SetID(groupID)

	var existing contact.Group
	err := couchdb.GetDoc(inst, consts.Groups, groupID, &existing)
	if err == nil {
		if !IsManagedDirectoryDoc(&existing.JSONDoc) {
			return fmt.Errorf("group id %s already exists and is not managed", groupID)
		}
		doc.M = existing.M
		doc.Type = consts.Groups
		doc.SetRev(existing.Rev())
	} else if !couchdb.IsNoDatabaseError(err) && !couchdb.IsNotFoundError(err) {
		return err
	}

	if fields.Name != nil && *fields.Name != "" {
		doc.M["name"] = *fields.Name
	} else if doc.M["name"] == nil {
		doc.M["name"] = fields.ExternalID
	}
	applyOptionalStringField(doc.M, "description", fields.Description)
	applyOptionalStringField(doc.M, "color", fields.Color)
	if fields.CreatedAt != nil && !fields.CreatedAt.IsZero() {
		doc.M["createdAt"] = fields.CreatedAt.Format(time.RFC3339Nano)
	}
	setGroupDirectoryMetadata(&doc.JSONDoc, fields.OrganizationID, fields.ExternalID)

	if doc.Rev() == "" {
		return couchdb.CreateNamedDocWithDB(inst, doc)
	}
	return couchdb.UpdateDoc(inst, doc)
}

func ensureGroupExists(inst *instance.Instance, organizationID, externalID string) error {
	groupID := GroupDocID(organizationID, externalID)
	group, err := contact.FindGroup(inst, groupID)
	if err != nil {
		return err
	}
	if !IsManagedDirectoryDoc(&group.JSONDoc) {
		return fmt.Errorf("group %s exists but is not managed", groupID)
	}
	return nil
}

func addMembersToGroup(ctx context.Context, inst *instance.Instance, organizationID, externalGroupID string, members []GroupMember) error {
	groupID := GroupDocID(organizationID, externalGroupID)
	var errs []error
	for _, member := range members {
		if err := ctx.Err(); err != nil {
			return err
		}
		input := contactPatchFromMember(organizationID, member)
		if input.shouldSkipOwn(inst) {
			continue
		}
		c, err := UpsertManagedContact(inst, input)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		changed, err := addContactGroup(c, groupID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if changed {
			if err := couchdb.UpdateDoc(inst, c); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// UpsertManagedContact creates or updates a managed organization-directory
// contact, preserving any existing document ID matched by email or Cozy URL.
// Callers should pass values already normalized at their input boundary.
func UpsertManagedContact(inst *instance.Instance, input ContactPatch) (*contact.Contact, error) {
	if err := input.validate(); err != nil {
		return nil, err
	}
	c, err := findManagedContact(inst, input)
	if errors.Is(err, contact.ErrNotFound) {
		c = contact.New()
		applyManagedContactFields(c, input)
		if err := couchdb.CreateDoc(inst, c); err != nil {
			return nil, err
		}
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	applyManagedContactFields(c, input)
	if err := couchdb.UpdateDoc(inst, c); err != nil {
		return nil, err
	}
	return c, nil
}

func findManagedContact(inst *instance.Instance, input ContactPatch) (*contact.Contact, error) {
	if input.Email != "" {
		c, err := findManagedContactByEmail(inst, input.Email)
		if err == nil || !errors.Is(err, contact.ErrNotFound) {
			return c, err
		}
	}
	if input.CozyURL != "" {
		return findManagedContactByCozyURL(inst, input.CozyURL)
	}
	return nil, contact.ErrNotFound
}

func applyManagedContactFields(c *contact.Contact, input ContactPatch) {
	email := input.Email
	if email != "" {
		c.M["email"] = []map[string]interface{}{
			{"address": email, "primary": true},
		}
	}
	name := input.displayName()
	if name != "" {
		c.M["fullname"] = name
		c.M["displayName"] = name
	}
	if input.CozyURL != "" {
		c.M["cozy"] = []map[string]interface{}{
			{"url": input.CozyURL, "primary": true},
		}
	}
	if input.Phone != "" {
		c.M["phone"] = []map[string]interface{}{
			{"number": input.Phone, "primary": true},
		}
	}
	c.M["metadata"] = map[string]interface{}{"external": true}
	c.M[contact.TrustedForSharingKey] = true
	setContactDirectoryMetadata(&c.JSONDoc, input, email)
	index := email
	if input.CozyURL != "" {
		index = input.CozyURL
	}
	if index != "" {
		c.M["indexes"] = map[string]interface{}{
			"byFamilyNameGivenNameEmailCozyUrl": index,
		}
	}
}

func addContactGroup(c *contact.Contact, groupID string) (bool, error) {
	refs := contactGroupRefs(c)
	for _, ref := range refs {
		if refGroupID(ref) == groupID {
			return false, nil
		}
	}
	refs = append(refs, map[string]interface{}{
		"_id":   groupID,
		"_type": consts.Groups,
	})
	setContactGroupRefs(c, refs)
	return true, nil
}

func addContactGroups(c *contact.Contact, groupIDs []string) (bool, error) {
	var changed bool
	for _, groupID := range groupIDs {
		groupID = strings.TrimSpace(groupID)
		if groupID == "" {
			continue
		}
		added, err := addContactGroup(c, groupID)
		if err != nil {
			return false, err
		}
		changed = changed || added
	}
	return changed, nil
}

func removeContactGroup(c *contact.Contact, groupID string) (bool, error) {
	refs := contactGroupRefs(c)
	if len(refs) == 0 {
		return false, nil
	}
	next := make([]interface{}, 0, len(refs))
	var changed bool
	for _, ref := range refs {
		if refGroupID(ref) == groupID {
			changed = true
			continue
		}
		next = append(next, ref)
	}
	if changed {
		setContactGroupRefs(c, next)
	}
	return changed, nil
}

func contactGroupRefs(c *contact.Contact) []interface{} {
	rels, _ := c.M["relationships"].(map[string]interface{})
	groups, _ := rels["groups"].(map[string]interface{})
	data, _ := groups["data"].([]interface{})
	return data
}

func setContactGroupRefs(c *contact.Contact, refs []interface{}) {
	rels, _ := c.M["relationships"].(map[string]interface{})
	if rels == nil {
		rels = make(map[string]interface{})
	}
	groups, _ := rels["groups"].(map[string]interface{})
	if groups == nil {
		groups = make(map[string]interface{})
	}
	groups["data"] = refs
	rels["groups"] = groups
	c.M["relationships"] = rels
}

func refGroupID(ref interface{}) string {
	m, _ := ref.(map[string]interface{})
	if m == nil || m["_type"] != consts.Groups {
		return ""
	}
	id, _ := m["_id"].(string)
	return id
}

func removeGroupFromManagedContacts(inst *instance.Instance, organizationID, groupID string) error {
	docs, err := listManagedContacts(inst, organizationID)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range docs {
		changed, err := removeContactGroup(c, groupID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if changed {
			if err := couchdb.UpdateDoc(inst, c); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func validateGroupIdentity(eventName, organizationID, groupID string) error {
	if strings.TrimSpace(organizationID) == "" {
		return fmt.Errorf("%s: missing organizationId", eventName)
	}
	if strings.TrimSpace(groupID) == "" {
		return fmt.Errorf("%s: missing id", eventName)
	}
	return nil
}

func validateMembersMessage(eventName string, msg GroupMembersMessage) error {
	if err := validateGroupIdentity(eventName, msg.OrganizationID, msg.ID); err != nil {
		return err
	}
	if len(msg.Members) == 0 {
		return fmt.Errorf("%s: missing members", eventName)
	}
	return nil
}

func applyOptionalStringField(m map[string]interface{}, key string, value *string) {
	if value == nil {
		return
	}
	if *value == "" {
		delete(m, key)
		return
	}
	m[key] = *value
}

func nonEmptyStringPtr(v string) *string {
	cleaned := strings.TrimSpace(v)
	if cleaned == "" {
		return nil
	}
	return &cleaned
}

func cleanStringPtr(v *string) *string {
	if v == nil {
		return nil
	}
	cleaned := strings.TrimSpace(*v)
	return &cleaned
}
