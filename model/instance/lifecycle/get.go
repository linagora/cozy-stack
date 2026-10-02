package lifecycle

import (
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/couchdb"
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
