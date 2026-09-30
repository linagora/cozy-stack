package instance

import (
	"errors"
	"fmt"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/utils"
)

// ErrEmailTaken is returned when the email is already on another instance.
var ErrEmailTaken = errors.New("email is already on another instance")

// SetEmail stores the email of the instance owner. The lock on the email
// keeps two instances from taking it at the same time.
func SetEmail(inst *Instance, email string) error {
	email = utils.NormalizeEmail(email)
	if email == "" || email == inst.Email {
		return nil
	}
	mu := config.Lock().ReadWrite(prefixer.GlobalPrefixer, "instance-email/"+email)
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()

	docs, err := FindByEmail(email)
	if err != nil {
		return err
	}
	for _, doc := range docs {
		if doc.Domain != inst.Domain {
			return fmt.Errorf("%w: %s", ErrEmailTaken, doc.Domain)
		}
	}
	inst.Email = email
	return Update(inst)
}

// SyncEmail copies the settings email to the instance, except on
// organization instances. An email held by another instance is left out
// with a warning, as the settings update has already been saved.
func SyncEmail(inst *Instance, settingsEmail string) error {
	if inst.IsOrganizationInstance() {
		return nil
	}
	err := SetEmail(inst, settingsEmail)
	if errors.Is(err, ErrEmailTaken) {
		inst.Logger().WithNamespace("instance").Warnf("Email not synced: %s", err)
		return nil
	}
	return err
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
