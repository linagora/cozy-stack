// Package publiclink collects private public-link activity.
package publiclink

import (
	"time"

	"github.com/cozy/cozy-stack/model/feature"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

func enabled(inst *instance.Instance) bool {
	if value, ok := inst.FeatureFlags[publiclink.FlagEnabled]; ok {
		enabled, _ := value.(bool)
		return enabled
	}
	flags, err := feature.GetFlags(inst)
	if err != nil {
		return false
	}
	enabled, _ := flags.M[publiclink.FlagEnabled].(bool)
	return enabled
}

// Record must not change the result of the action if storage is unavailable.
func Record(c echo.Context, inst *instance.Instance, perm *permission.Permission, action publiclink.Action) {
	if perm == nil || perm.Type != permission.TypeShareByLink {
		return
	}
	isFileLink := false
	for _, rule := range perm.Permissions {
		if rule.Type == consts.Files {
			isFileLink = true
			break
		}
	}
	if !isFileLink || !enabled(inst) {
		return
	}
	event := &publiclink.Event{LinkID: perm.ID(), Action: action, OccurredAt: time.Now(), IP: utils.ClientIP(c.Request())}
	isTransportActor := false
	_, isSecretDownload := c.Get(downloadPermissionKey).(*permission.Permission)
	if action == publiclink.Downloaded && isSecretDownload {
		// A public secret can arrive with the proxy's drive token, not visitor credentials.
		requestPermission, _ := middlewares.GetPermission(c)
		isTransportActor = requestPermission != nil && requestPermission.Type == permission.TypeShareInteract
	}
	if actor, ok := middlewares.GetActor(c); ok && actor.Domain != "" && !isTransportActor {
		event.Identity = actor.Domain
	} else if sess, ok := middlewares.GetSession(c); ok && sess.BelongsTo(inst) {
		event.Identity = inst.Domain
	}
	if err := couchdb.CreateDoc(inst, event); err != nil {
		log := inst.Logger().WithNamespace("publiclink")
		if couchErr, ok := couchdb.IsCouchError(err); ok {
			log.Errorf("Could not store public link event: CouchDB status %d", couchErr.StatusCode)
		} else {
			log.Error("Could not store public link event: unexpected storage error")
		}
	}
}
