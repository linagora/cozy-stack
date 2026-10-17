package orgdrive

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEnsure(t *testing.T) {
	if testing.Short() {
		t.Skip("requires CouchDB")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	t.Run("StoresTheActivityFlag", func(t *testing.T) {
		org := newOrgInstance(t)
		cmd := &Command{Reference: Reference{Source: "app", ID: "object"}, Revision: 1, Name: "Activity", Activity: true}
		s, _, err := Ensure(org, cmd)
		require.NoError(t, err)
		assert.True(t, s.Reference.Activity)

		cmd.Activity = false
		_, _, err = Ensure(org, cmd)
		require.NoError(t, err)
		s, err = sharing.FindSharing(org, s.SID)
		require.NoError(t, err)
		assert.False(t, s.Reference.Activity)
	})

	t.Run("ConcurrentCreationUsesOneDrive", func(t *testing.T) {
		org := newOrgInstance(t)
		org.VFS()
		cmd := &Command{Reference: Reference{Source: "app", ID: "object"}, Revision: 1, Name: "Concurrent"}
		type result struct {
			drive *sharing.Sharing
			err   error
		}
		const workers = 8
		results := make(chan result, workers)
		start := make(chan struct{})
		for range workers {
			go func() {
				<-start
				s, _, err := Ensure(org, cmd)
				results <- result{s, err}
			}()
		}
		close(start)
		var id string
		for range workers {
			r := <-results
			require.NoError(t, r.err)
			require.NotNil(t, r.drive)
			if id == "" {
				id = r.drive.SID
			}
			assert.Equal(t, id, r.drive.SID)
		}
		assert.Equal(t, id, onlyDrive(t, org).SID)
	})

	t.Run("ReferencesAndOrganizationsAreIndependent", func(t *testing.T) {
		org := newOrgInstance(t)
		other := newOrgInstance(t)
		ids := make(map[string]bool)
		for _, ref := range []Reference{{Source: "app/a", ID: "b"}, {Source: "app", ID: "a/b"}, {Source: "app", ID: "b"}} {
			cmd := &Command{Reference: ref, Revision: 1, Name: "Independent"}
			for _, inst := range []*instance.Instance{org, other} {
				s, _, err := Ensure(inst, cmd)
				require.NoError(t, err)
				assert.False(t, ids[s.SID])
				ids[s.SID] = true
			}
		}
	})

	t.Run("DuplicatesBeyondFirstPageFailWithoutWrites", func(t *testing.T) {
		org := newOrgInstance(t)
		var seeded []*sharing.Sharing
		for i := range 102 {
			s := &sharing.Sharing{
				SID: fmt.Sprintf("reference-%03d", i), Owner: true, Drive: true, Active: i >= 100,
				Reference: &sharing.Reference{Source: "twake-space", ID: "duplicate", Revision: int64(i + 1)},
			}
			require.NoError(t, couchdb.CreateNamedDocWithDB(org, s))
			seeded = append(seeded, s)
		}
		pub := &fakePublisher{}
		for _, revision := range []int64{1, 102, 103} {
			err := NewHandler(pub).Handle(context.Background(), ensure(t, org, "duplicate", revision, "Changed"))
			require.ErrorIs(t, err, ErrMultipleActiveDrives)
			assert.NotErrorIs(t, err, rabbitmq.ErrMalformed)
			assert.ErrorContains(t, err, org.OrgID)
			assert.ErrorContains(t, err, "reference-100")
			assert.ErrorContains(t, err, "reference-101")
		}
		assert.Empty(t, pub.requests)
		stored, err := sharing.FindByReference(org, "twake-space", "duplicate")
		require.NoError(t, err)
		assert.Equal(t, seeded, stored)

		// The last page must also contribute to the revision check.
		for _, s := range seeded[100:] {
			s.Active = false
			require.NoError(t, couchdb.UpdateDoc(org, s))
		}
		require.NoError(t, NewHandler(pub).Handle(context.Background(), ensure(t, org, "duplicate", 101, "Stale")))
		assert.Empty(t, pub.requests)
	})

	t.Run("FailedDeactivationPreventsReplacement", func(t *testing.T) {
		org := newOrgInstance(t)
		cmd := &Command{Reference: Reference{Source: "app", ID: "missing"}, Revision: 1, Name: "Missing"}
		first, _, err := Ensure(org, cmd)
		require.NoError(t, err)
		require.NoError(t, couchdb.DeleteDoc(org, rootDir(t, org, first)))
		cmd.Revision = 2
		cmd.Name = "Replacement"
		client := config.CouchClient()
		original := client.Transport
		t.Cleanup(func() { client.Transport = original })
		client.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/"+first.SID) {
				return nil, errors.New("deactivation failed")
			}
			return original.RoundTrip(r)
		})
		_, _, err = Ensure(org, cmd)
		require.ErrorContains(t, err, "deactivation failed")
		assert.Equal(t, first.SID, onlyDrive(t, org).SID)
		client.Transport = original
		replacement, _, err := Ensure(org, cmd)
		require.NoError(t, err)
		assert.NotEqual(t, first.SID, replacement.SID)
		stored, err := sharing.FindSharing(org, first.SID)
		require.NoError(t, err)
		assert.False(t, stored.Active)
	})
}
