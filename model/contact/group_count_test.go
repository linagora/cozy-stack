package contact

import (
	"testing"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/stretchr/testify/require"
)

func TestCountGroupMembers(t *testing.T) {
	config.UseTestFile(t)
	db := prefixer.NewPrefixer(0, "contact-count-groups", "contact-count-groups")
	require.NoError(t, couchdb.ResetDB(db, consts.Contacts))
	t.Cleanup(func() { _ = couchdb.DeleteDB(db, consts.Contacts) })
	require.NoError(t, couchdb.DefineView(db, couchdb.ContactGroupMemberCount))
	add := func(trashed bool, groups ...string) {
		refs := make([]interface{}, len(groups))
		for i, id := range groups {
			refs[i] = map[string]interface{}{"_id": id, "_type": consts.Groups}
		}
		c := New()
		c.M["trashed"] = trashed
		c.M["relationships"] = map[string]interface{}{"groups": map[string]interface{}{"data": refs}}
		require.NoError(t, couchdb.CreateDoc(db, c))
	}
	add(false, "one", "one", "two")
	add(false, "one")
	add(true, "one")
	counts, err := CountGroupMembers(db, []string{"one", "two", "empty"})
	require.NoError(t, err)
	require.Equal(t, 2, counts["one"])
	require.Equal(t, 1, counts["two"])
	require.Zero(t, counts["empty"])
}
