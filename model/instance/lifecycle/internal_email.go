package lifecycle

import (
	"fmt"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/utils"
)

// SetInternalEmail stores the internal email of the instance owner.
func SetInternalEmail(inst *instance.Instance, email string) error {
	email = utils.NormalizeEmail(email)
	if email == "" || email == inst.InternalEmail {
		return nil
	}
	inst.InternalEmail = email
	return update(inst)
}

// GetInstanceByInternalEmail retrieves an instance by its internal email.
func GetInstanceByInternalEmail(email string) (*instance.Instance, error) {
	var docs []*instance.Instance
	err := couchdb.FindDocs(prefixer.GlobalPrefixer, consts.Instances, &couchdb.FindRequest{
		UseIndex: "by-internalemail",
		Selector: mango.Equal("internal_email", utils.NormalizeEmail(email)),
		Limit:    2,
	}, &docs)
	if err != nil {
		return nil, err
	}
	switch len(docs) {
	case 0:
		return nil, instance.ErrNotFound
	case 1:
		return GetInstance(docs[0].Domain)
	default:
		return nil, fmt.Errorf("internal email %s is on several instances", email)
	}
}
