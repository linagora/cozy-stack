package instance

import (
	"encoding/json"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

type InstanceService struct {
	logger logger.Logger
}

const orgInstanceListPageSize = 1000

func NewService(logger logger.Logger) *InstanceService {
	return &InstanceService{
		logger: logger,
	}
}

// Get finds an instance from its domain by using CouchDB.
func (s *InstanceService) Get(domain string) (*Instance, error) {
	var res couchdb.ViewResponse
	err := couchdb.ExecView(prefixer.GlobalPrefixer, couchdb.DomainAndAliasesView, &couchdb.ViewRequest{
		Key:         domain,
		IncludeDocs: true,
		Limit:       1,
	}, &res)
	if couchdb.IsNoDatabaseError(err) {
		return nil, ErrNotFound
	}

	if err != nil {
		return nil, err
	}

	if len(res.Rows) == 0 {
		return nil, ErrNotFound
	}

	inst := &Instance{}
	err = json.Unmarshal(res.Rows[0].Doc, inst)
	if err != nil {
		return nil, err
	}

	if err = inst.MakeVFS(); err != nil {
		return nil, err
	}

	return inst, nil
}

func (s *InstanceService) ListByOrgDomain(orgDomain string) ([]*Instance, error) {
	return s.listByOrgField("by-orgdomain", "org_domain", orgDomain)
}

func (s *InstanceService) ListByOrgID(orgID string) ([]*Instance, error) {
	return s.listByOrgField("by-orgid", "org_id", orgID)
}

func (s *InstanceService) listByOrgField(indexName, fieldName, value string) ([]*Instance, error) {
	var docs []*Instance
	var bookmark string
	for {
		var page []*Instance
		req := &couchdb.FindRequest{
			UseIndex: indexName,
			Selector: mango.Equal(fieldName, value),
			Limit:    orgInstanceListPageSize,
			Bookmark: bookmark,
		}
		res, err := couchdb.FindDocsRaw(prefixer.GlobalPrefixer, consts.Instances, req, &page)
		if err != nil {
			return nil, err
		}
		for _, inst := range page {
			if inst != nil {
				docs = append(docs, inst)
			}
		}

		nextBookmark := ""
		if res != nil {
			nextBookmark = res.Bookmark
		}
		if len(page) < orgInstanceListPageSize || nextBookmark == "" || nextBookmark == bookmark {
			return docs, nil
		}
		bookmark = nextBookmark
	}
}

// Update saves the changes in CouchDB.
func (s *InstanceService) Update(inst *Instance) error {
	return couchdb.UpdateDoc(prefixer.GlobalPrefixer, inst)
}

// Delete removes the instance document in CouchDB.
func (s *InstanceService) Delete(inst *Instance) error {
	return couchdb.DeleteDoc(prefixer.GlobalPrefixer, inst)
}

// CheckPassphrase confirm an instance password
func (s *InstanceService) CheckPassphrase(inst *Instance, pass []byte) error {
	if len(pass) == 0 {
		return ErrMissingPassphrase
	}

	needUpdate, err := crypto.CompareHashAndPassphrase(inst.PassphraseHash, pass)
	if err != nil {
		return err
	}

	if !needUpdate {
		return nil
	}

	newHash, err := crypto.GenerateFromPassphrase(pass)
	if err != nil {
		return err
	}

	inst.PassphraseHash = newHash
	if err = s.Update(inst); err != nil {
		s.logger.WithDomain(inst.Domain).Errorf("Failed to update hash in db: %s", err)
	}
	return nil
}
