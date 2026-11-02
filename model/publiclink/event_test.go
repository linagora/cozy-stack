package publiclink

import (
	"context"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/stretchr/testify/require"
)

func TestPrivateDoctype(t *testing.T) {
	require.Error(t, permission.CheckReadable(consts.PublicLinkEvents))
	require.Error(t, permission.CheckWritable(consts.PublicLinkEvents))
}

func TestEventStorage(t *testing.T) {
	if testing.Short() {
		t.Skip("requires CouchDB")
	}
	config.UseTestFile(t)
	_, err := couchdb.CheckStatus(context.Background())
	require.NoError(t, err)
	suffix := utils.RandomString(10)
	a := prefixer.NewPrefixer(0, "test-a", "publiclink-a-"+suffix)
	b := prefixer.NewPrefixer(0, "test-b", "publiclink-b-"+suffix)
	for _, db := range []prefixer.Prefixer{a, b} {
		require.NoError(t, couchdb.CreateDB(db, consts.PublicLinkEvents))
		db := db
		t.Cleanup(func() { _ = couchdb.DeleteDB(db, consts.PublicLinkEvents) })
	}

	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	events := []*Event{
		{LinkID: "link-a", Action: Created, OccurredAt: at, IP: "2001:db8::1", Identity: "https://alice.example"},
		{LinkID: "link-b", Action: Opened, OccurredAt: at, IP: "192.0.2.1"},
		{LinkID: "link-a", Action: Downloaded, OccurredAt: at, IP: "192.0.2.1"},
		{LinkID: "link-a", Action: Updated, OccurredAt: at, IP: "192.0.2.1"},
	}
	for _, event := range events {
		require.NoError(t, couchdb.CreateDoc(a, event))
		require.NotEmpty(t, event.ID())
		var got Event
		require.NoError(t, couchdb.GetDoc(a, consts.PublicLinkEvents, event.ID(), &got))
		require.Equal(t, event, &got)
		require.Error(t, couchdb.GetDoc(b, consts.PublicLinkEvents, event.ID(), &got))
	}
	require.NotEqual(t, events[0].ID(), events[1].ID())
	var linkB []Event
	err = couchdb.FindDocsUnoptimized(a, consts.PublicLinkEvents, &couchdb.FindRequest{Selector: mango.Equal("link_id", "link-b")}, &linkB)
	require.NoError(t, err)
	require.Equal(t, []Event{*events[1]}, linkB)
}
