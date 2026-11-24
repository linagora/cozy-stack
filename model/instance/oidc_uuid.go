package instance

import (
	"github.com/gofrs/uuid/v5"
)

// RememberOIDCUUID keeps the uuid claim of an identity the SSO verified as
// the owner of the instance: the LDAP entryUUID, which Twake apps use to name
// people in their events. The stack only learns it at login or at a token
// exchange, so saving it is best effort, and an error is only logged.
func RememberOIDCUUID(inst *Instance, claims map[string]interface{}) {
	raw, _ := claims["uuid"].(string)
	id, err := uuid.FromString(raw)
	if err != nil {
		return
	}
	value := id.String()
	if value == inst.OIDCUUID {
		return
	}
	inst.OIDCUUID = value
	if err := Update(inst); err != nil {
		inst.Logger().WithNamespace("oidc").
			Warnf("Cannot save the uuid of the owner: %s", err)
	}
}
