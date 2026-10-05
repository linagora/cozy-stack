package instance

import (
	"errors"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

// ErrEmailTaken is returned when the email is already on another instance.
var ErrEmailTaken = errors.New("email is already on another instance")

// SetEmail stores the email of the instance owner. Organization instances
// keep no email. The lock on the email keeps two instances from taking it at
// the same time.
//
// Deprecated: Use [InstanceService.SetEmail] instead.
func SetEmail(inst *Instance, email string) error {
	return service.SetEmail(inst, email)
}

// FindByEmail returns at most two instances, enough to spot a duplicate.
func FindByEmail(email string) ([]*Instance, error) {
	var docs []*Instance
	err := couchdb.FindDocs(prefixer.GlobalPrefixer, consts.Instances, &couchdb.FindRequest{
		UseIndex: "by-email",
		Selector: mango.Equal("email", email),
		Limit:    2,
	}, &docs)
	return docs, err
}
