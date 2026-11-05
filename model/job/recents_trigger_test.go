package job

import (
	"testing"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/stretchr/testify/require"
)

func TestRecentsEventTrigger(t *testing.T) {
	config.UseTestFile(t)
	db := prefixer.NewPrefixer(0, "trigger-test", "trigger-test")
	for _, args := range []string{
		consts.FilesRecents,
		"io.cozy.foos:CREATED " + consts.FilesRecents + ":UPDATED",
	} {
		_, err := NewTrigger(db, TriggerInfos{Type: "@event", Arguments: args}, nil)
		require.Error(t, err, args)
	}

	// A trigger saved before is not given the events of the recents either.
	row := &couchdb.JSONDoc{Type: consts.FilesRecents, M: map[string]interface{}{"_id": "row"}}
	event := &realtime.Event{Verb: realtime.EventCreate, Doc: row}
	require.False(t, eventMatchRule(event, &permission.Rule{Type: consts.FilesRecents}))
}
