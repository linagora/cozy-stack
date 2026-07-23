package bitwarden

import (
	"errors"

	"github.com/cozy/cozy-stack/model/bitwarden/settings"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/metadata"
)

// OrgMemberStatus is a type for the status of an organization member
type OrgMemberStatus int

const (
	// OrgMemberInvited is used when the member is invited but has not yet
	// accepted the invitation.
	OrgMemberInvited OrgMemberStatus = 0
	// OrgMemberAccepted is used when the member is accepted but the owner has
	// not yet confirmed that the fingerprint is OK.
	OrgMemberAccepted OrgMemberStatus = 1
	// OrgMemberConfirmed is used when the member is confirmed, and has access
	// to the organization key to decrypt/encrypt ciphers.
	OrgMemberConfirmed OrgMemberStatus = 2
)

// OrgMember is a struct for describing a member of an organization.
type OrgMember struct {
	UserID   string          `json:"user_id"`
	Email    string          `json:"email"`
	Name     string          `json:"name"`
	OrgKey   string          `json:"key,omitempty"` // The organization key encrypted with the public key of the user
	Status   OrgMemberStatus `json:"status"`
	Owner    bool            `json:"owner,omitempty"`
	ReadOnly bool            `json:"read_only,omitempty"`
}

// Collection is used to regroup ciphers.
type Collection struct {
	DocID string `json:"_id"`
	Name  string `json:"name"`
}

// ID returns the collection identifier
func (c *Collection) ID() string { return c.DocID }

// Organization is used to make collections of ciphers and can be used for
// sharing them with other users with cryptography mechanisms.
type Organization struct {
	CouchID    string                `json:"_id,omitempty"`
	CouchRev   string                `json:"_rev,omitempty"`
	Name       string                `json:"name"`
	Members    map[string]OrgMember  `json:"members"` // the keys are the instances domains
	Collection Collection            `json:"defaultCollection"`
	Metadata   metadata.CozyMetadata `json:"cozyMetadata"`
}

// ID returns the organization identifier
func (o *Organization) ID() string { return o.CouchID }

// Rev returns the organization revision
func (o *Organization) Rev() string { return o.CouchRev }

// SetID changes the organization identifier
func (o *Organization) SetID(id string) { o.CouchID = id }

// SetRev changes the organization revision
func (o *Organization) SetRev(rev string) { o.CouchRev = rev }

// DocType returns the organization document type
func (o *Organization) DocType() string { return consts.BitwardenOrganizations }

// Clone implements couchdb.Doc
func (o *Organization) Clone() couchdb.Doc {
	cloned := *o
	cloned.Members = make(map[string]OrgMember, len(o.Members))
	for k, v := range o.Members {
		cloned.Members[k] = v
	}
	return &cloned
}

// Member returns the entry of the given instance in the members of this
// organization. The members are keyed by the instances domains, and an
// instance that has been migrated to a new domain still has its entry stored
// under its former domain: the old domain and the domain aliases are used as
// fallbacks so that such an instance keeps its access to the organization. It
// returns a zero value when the instance is not a member.
func (o *Organization) Member(inst *instance.Instance) OrgMember {
	if m, ok := o.Members[inst.Domain]; ok {
		return m
	}
	if inst.OldDomain != "" {
		if m, ok := o.Members[inst.OldDomain]; ok {
			return m
		}
	}
	for _, alias := range inst.DomainAliases {
		if m, ok := o.Members[alias]; ok {
			return m
		}
	}
	return OrgMember{}
}

// FindCiphers returns the ciphers for this organization.
func (o *Organization) FindCiphers(inst *instance.Instance) ([]*Cipher, error) {
	var ciphers []*Cipher
	req := &couchdb.FindRequest{
		UseIndex: "by-organization-id",
		Selector: mango.Equal("organization_id", o.CouchID),
		Limit:    1000,
	}
	err := couchdb.FindDocs(inst, consts.BitwardenCiphers, req, &ciphers)
	if err != nil {
		return nil, err
	}
	return ciphers, nil
}

// Delete will delete the organization and the ciphers inside it.
func (o *Organization) Delete(inst *instance.Instance) error {
	ciphers, err := o.FindCiphers(inst)
	if err != nil {
		return err
	}
	docs := make([]couchdb.Doc, len(ciphers))
	for i := range ciphers {
		docs[i] = ciphers[i].Clone()
	}
	if err := couchdb.BulkDeleteDocs(inst, consts.BitwardenCiphers, docs); err != nil {
		return err
	}

	return couchdb.DeleteDoc(inst, o)
}

// GetCozyOrganization returns the organization used to store the credentials
// for the konnectors running on the Cozy server.
func GetCozyOrganization(inst *instance.Instance, setting *settings.Settings) (*Organization, error) {
	if setting == nil || setting.PublicKey == "" {
		return nil, errors.New("No public key")
	}
	orgKey, err := setting.OrganizationKey()
	if err != nil {
		inst.Logger().WithNamespace("bitwarden").
			Infof("Cannot read the organization key: %s", err)
		return nil, err
	}
	key, err := crypto.EncryptWithRSA(setting.PublicKey, orgKey)
	if err != nil {
		inst.Logger().WithNamespace("bitwarden").
			Infof("Cannot encrypt with RSA: %s", err)
		return nil, err
	}

	iv := crypto.GenerateRandomBytes(16)
	payload := []byte(consts.BitwardenCozyCollectionName)
	name, err := crypto.EncryptWithAES256HMAC(orgKey[:32], orgKey[32:], payload, iv)
	if err != nil {
		inst.Logger().WithNamespace("bitwarden").
			Infof("Cannot encrypt with AES: %s", err)
		return nil, err
	}

	settings, err := inst.SettingsDocument()
	if err != nil {
		return nil, err
	}
	email, _ := settings.M["email"].(string)
	publicName, _ := settings.M["public_name"].(string)
	org := Organization{
		CouchID: setting.OrganizationID,
		Name:    consts.BitwardenCozyOrganizationName,
		Members: map[string]OrgMember{
			inst.Domain: {
				UserID: inst.ID(),
				Email:  email,
				Name:   publicName,
				OrgKey: key,
				Status: OrgMemberConfirmed,
				Owner:  true,
			},
		},
		Collection: Collection{
			DocID: setting.CollectionID,
			Name:  name,
		},
	}
	if setting.Metadata != nil {
		org.Metadata = *setting.Metadata
	}
	return &org, nil
}

// FindAllOrganizations returns all the organizations, including the Cozy one.
func FindAllOrganizations(inst *instance.Instance, setting *settings.Settings) ([]*Organization, error) {
	var orgs []*Organization
	req := &couchdb.AllDocsRequest{}
	if err := couchdb.GetAllDocs(inst, consts.BitwardenOrganizations, req, &orgs); err != nil {
		if couchdb.IsNoDatabaseError(err) {
			_ = couchdb.CreateDB(inst, consts.BitwardenOrganizations)
		} else {
			return nil, err
		}
	}

	cozy, err := GetCozyOrganization(inst, setting)
	if err != nil {
		return nil, err
	}
	orgs = append(orgs, cozy)
	return orgs, nil
}

var _ couchdb.Doc = &Organization{}
