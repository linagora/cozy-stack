package lifecycle

import (
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

// GetInstance retrieves the instance for a request by its host.
func GetInstance(domain string) (*instance.Instance, error) {
	var err error
	domain, err = validateDomain(domain)
	if err != nil {
		return nil, err
	}
	i, err := instance.Get(domain)
	if err != nil {
		return nil, err
	}

	// This retry-loop handles the probability to hit an Update conflict from
	// this version update, since the instance document may be updated different
	// processes at the same time.
	for {
		if i.IndexViewsVersion == couchdb.IndexViewsVersion {
			break
		}

		i.Logger().Debugf("Indexes outdated: wanted %d; got %d", couchdb.IndexViewsVersion, i.IndexViewsVersion)
		if err = UpdateViewsAndIndex(i); err != nil {
			i.Logger().Errorf("Could not re-define indexes and views: %s", err.Error())
			return nil, err
		}

		if err = update(i); err == nil {
			break
		}

		if !couchdb.IsConflictError(err) {
			return nil, err
		}

		i, err = instance.Get(domain)
		if err != nil {
			return nil, err
		}
	}

	if err = i.MakeVFS(); err != nil {
		return nil, err
	}
	return i, nil
}

// ListOrgInstances retrieves all the instances of an organization
func ListOrgInstances(orgDomain string) ([]*instance.Instance, error) {
	return instance.ListByOrgDomain(orgDomain)
}

// ListOrgInstancesByID retrieves all the instances of an organization by its
// organization identifier.
func ListOrgInstancesByID(orgID string) ([]*instance.Instance, error) {
	return instance.ListByOrgID(orgID)
}

// GetOrgInstanceByOrgDomain retrieves the organization instance of an
// organization domain, without listing every member instance.
func GetOrgInstanceByOrgDomain(orgDomain string) (*instance.Instance, error) {
	var members []*instance.Instance
	err := couchdb.FindDocs(prefixer.GlobalPrefixer, consts.Instances, &couchdb.FindRequest{
		UseIndex: "by-orgdomain",
		Selector: mango.Equal("org_domain", orgDomain),
		Limit:    1,
	}, &members)
	if err != nil {
		return nil, err
	}
	if len(members) == 0 || members[0].OrgID == "" {
		return nil, instance.ErrNotFound
	}
	orgID := members[0].OrgID

	var docs []*instance.Instance
	err = couchdb.FindDocs(prefixer.GlobalPrefixer, consts.Instances, &couchdb.FindRequest{
		UseIndex: "by-orgid",
		Selector: mango.And(mango.Equal("org_id", orgID), mango.StartWith("domain", orgID+".")),
		Limit:    1,
	}, &docs)
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, instance.ErrNotFound
	}
	return GetInstance(docs[0].Domain)
}
