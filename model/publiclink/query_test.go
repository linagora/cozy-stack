package publiclink

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/stretchr/testify/require"
)

func TestForEachEventPaginationAndInstanceIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("requires CouchDB")
	}
	config.UseTestFile(t)
	_, err := couchdb.CheckStatus(context.Background())
	require.NoError(t, err)
	suffix := utils.RandomString(10)
	a := prefixer.NewPrefixer(0, "test-a", "publiclink-query-a-"+suffix)
	b := prefixer.NewPrefixer(0, "test-b", "publiclink-query-b-"+suffix)
	for _, db := range []prefixer.Prefixer{a, b} {
		require.NoError(t, couchdb.CreateDB(db, consts.PublicLinkEvents))
		t.Cleanup(func() { _ = couchdb.DeleteDB(db, consts.PublicLinkEvents) })
		require.NoError(t, couchdb.DefineView(db, couchdb.PublicLinkEventsByTime))
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for i := 0; i < 1001; i++ {
		require.NoError(t, couchdb.CreateDoc(a, &Event{LinkID: "same-link", Action: Opened, OccurredAt: at}))
	}
	for _, instant := range []time.Time{at.Add(time.Nanosecond), at.Add(-time.Nanosecond), at.In(time.FixedZone("offset", 3600))} {
		require.NoError(t, couchdb.CreateDoc(a, &Event{LinkID: "same-link", Action: Opened, OccurredAt: instant}))
	}
	require.NoError(t, couchdb.CreateDoc(a, &Event{LinkID: "another-link", Action: Created, OccurredAt: at}))
	require.NoError(t, couchdb.CreateDoc(b, &Event{LinkID: "same-link", Action: Downloaded, OccurredAt: at}))
	var gotA []Event
	err = ForEachEvent(context.Background(), a, "same-link", func(event Event) error {
		gotA = append(gotA, event)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, gotA, 1004)
	seen := make(map[string]bool)
	for i, event := range gotA {
		require.Equal(t, Opened, event.Action)
		require.False(t, seen[event.ID()])
		seen[event.ID()] = true
		if i > 0 {
			require.False(t, event.OccurredAt.Before(gotA[i-1].OccurredAt))
			if event.OccurredAt.Equal(gotA[i-1].OccurredAt) {
				require.Less(t, gotA[i-1].ID(), event.ID())
			}
		}
	}
	require.True(t, gotA[0].OccurredAt.Equal(at.Add(-time.Nanosecond)))
	require.True(t, gotA[len(gotA)-1].OccurredAt.Equal(at.Add(time.Nanosecond)))
	var gotB []Event
	err = ForEachEvent(context.Background(), b, "same-link", func(event Event) error { gotB = append(gotB, event); return nil })
	require.NoError(t, err)
	require.Len(t, gotB, 1)
	require.Equal(t, Downloaded, gotB[0].Action)
	for _, instant := range []time.Time{time.Unix(0, 1), time.Unix(-1, 999999999), time.Unix(0, 0), {}} {
		require.NoError(t, couchdb.CreateDoc(a, &Event{LinkID: "historical", Action: Created, OccurredAt: instant}))
	}
	var historical []time.Time
	err = ForEachEvent(context.Background(), a, "historical", func(event Event) error {
		historical = append(historical, event.OccurredAt)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, historical, 4)
	require.True(t, historical[0].IsZero())
	require.True(t, historical[1].Equal(time.Unix(-1, 999999999)))
	require.True(t, historical[2].Equal(time.Unix(0, 0)))
	require.True(t, historical[3].Equal(time.Unix(0, 1)))

	count := 0
	err = ForEachEvent(context.Background(), a, "same-link", func(event Event) error {
		count++
		if count == 500 {
			return couchdb.DeleteDoc(a, &gotA[500])
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 1003, count, "deleting the next cursor must not skip the following event")

	err = ForEachEvent(context.Background(), b, "missing", func(Event) error { t.Fatal("unexpected event"); return nil })
	require.NoError(t, err)
	stop := errors.New("stop")
	require.ErrorIs(t, ForEachEvent(context.Background(), a, "same-link", func(Event) error { return stop }), stop)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, ForEachEvent(ctx, a, "same-link", func(Event) error { return nil }), context.Canceled)

	count = 0
	err = ForEachEvent(context.Background(), a, "same-link", func(Event) error {
		count++
		if count == 500 {
			return couchdb.DeleteDB(a, consts.PublicLinkEvents)
		}
		return nil
	})
	require.Error(t, err, "a failed later page must not be treated as a complete export")
	require.Equal(t, 500, count)
}
