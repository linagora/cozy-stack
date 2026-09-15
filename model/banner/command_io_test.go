package banner_test

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cozy/cozy-stack/model/banner"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type commandRoundTripper func(*http.Request) (*http.Response, error)

func (f commandRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCommandFanoutContinuesAfterStorageFailures(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	useCommandContexts(t)
	first := newInstance(t, commandContext, "en", "")
	org := "org-" + first.Domain
	first.OrgID = org
	// The instance helper registers cleanup; creation of the other members
	// uses the same org without assuming CouchDB's member ordering.
	require.NoError(t, couchdb.UpdateDoc(prefixer.GlobalPrefixer, first))
	newInstance(t, commandContext, "fr", org)
	newInstance(t, commandContext, "en", org)
	newInstance(t, commandContext, "en", org)
	members, err := lifecycle.ListOrgInstancesByID(org)
	require.NoError(t, err)
	require.Len(t, members, 4)
	cmd := fixture(t, "organization")
	cmd.OrgID = org
	failures := map[string]*atomic.Bool{}
	for _, inst := range members[1:3] {
		path := "/" + couchdb.EscapeCouchdbName(inst.DBPrefix()+"/"+consts.Banners) + "/banner-billing"
		failures[path] = &atomic.Bool{}
	}
	client := config.CouchClient()
	original := client.Transport
	t.Cleanup(func() { client.Transport = original })
	client.Transport = commandRoundTripper(func(r *http.Request) (*http.Response, error) {
		if failed := failures[r.URL.Path]; r.Method == http.MethodGet && failed != nil && failed.CompareAndSwap(false, true) {
			return nil, errors.New("simulated projection outage")
		}
		return original.RoundTrip(r)
	})
	err = banner.ApplyCommand(cmd)
	require.ErrorContains(t, err, "simulated projection outage")
	assert.NotErrorIs(t, err, banner.ErrInvalidCommand)
	for _, inst := range members[1:3] {
		assert.ErrorContains(t, err, inst.Domain, "every failed member must be reported")
	}
	before := storedBanner(t, members[0])
	require.NotNil(t, before)
	assert.Nil(t, storedBanner(t, members[1]))
	assert.Nil(t, storedBanner(t, members[2]))
	last := storedBanner(t, members[3])
	require.NotNil(t, last, "failures must not block later members")
	for _, inst := range members[1:3] {
		retained, err := banner.Stored(inst, banner.CategoryBilling)
		require.NoError(t, err)
		require.Nil(t, retained, "a failed member must not advance its revision")
	}
	require.NoError(t, banner.ApplyCommand(cmd))
	for _, inst := range members {
		require.NotNil(t, storedBanner(t, inst))
	}
	assert.Equal(t, before.DocRev, storedBanner(t, members[0]).DocRev)
	assert.Equal(t, last.DocRev, storedBanner(t, members[3]).DocRev)
}

func TestCommandClearRetriesProjectionFailure(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	useCommandContexts(t)
	inst := newInstance(t, commandContext, "en", "")
	old := materialize(t, inst, 1)
	require.NoError(t, banner.ApplyCommand(old))
	client := config.CouchClient()
	original := client.Transport
	t.Cleanup(func() { client.Transport = original })
	var failed atomic.Bool
	client.Transport = commandRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/banner-billing") && failed.CompareAndSwap(false, true) {
			return nil, errors.New("simulated clear outage")
		}
		return original.RoundTrip(r)
	})
	clear := clearCommand(t, inst, 2)
	require.ErrorContains(t, banner.ApplyCommand(clear), "simulated clear outage")
	retained, err := banner.Stored(inst, banner.CategoryBilling)
	require.NoError(t, err)
	require.False(t, retained.Cleared, "a failed clear must leave the previous decision intact")
	require.Equal(t, int64(1), retained.Revision)
	require.NoError(t, banner.ApplyCommand(old))
	require.NoError(t, banner.ApplyCommand(clear))
	assert.Nil(t, storedBanner(t, inst))
}

func TestCommandAppLookupFailureIsRetryable(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	useCommandContexts(t)
	inst := newInstance(t, commandContext, "en", "")
	manifest := &couchdb.JSONDoc{Type: consts.Apps, M: map[string]interface{}{
		"_id": consts.Apps + "/drive", "slug": "drive", "state": "ready",
	}}
	require.NoError(t, couchdb.CreateNamedDoc(inst, manifest))
	appsPath := "/" + couchdb.EscapeCouchdbName(inst.DBPrefix()+"/"+consts.Apps) + "/_all_docs"
	client := config.CouchClient()
	original := client.Transport
	t.Cleanup(func() { client.Transport = original })
	var failed atomic.Bool
	client.Transport = commandRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == appsPath && failed.CompareAndSwap(false, true) {
			return nil, errors.New("simulated app lookup outage")
		}
		return original.RoundTrip(r)
	})

	require.NoError(t, banner.ApplyCommand(materialize(t, inst, 1)))
	assert.False(t, failed.Load(), "explicit hosts must not require an app lookup")
	cmd := materialize(t, inst, 2)
	cmd.CTA.URL = inst.SubDomain("drive").String()
	err := banner.ApplyCommand(cmd)
	require.ErrorContains(t, err, "simulated app lookup outage")
	assert.NotErrorIs(t, err, banner.ErrInvalidCommand)
	assert.Equal(t, int64(1), storedState(t, inst).Revision)
	require.NoError(t, banner.ApplyCommand(cmd))
	assert.Equal(t, int64(2), storedState(t, inst).Revision)
}

func TestCommandRefreshContinuesAfterAppLookupFailure(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	useCommandContexts(t)
	inst := newInstance(t, commandContext, "en", "")
	manifest := &couchdb.JSONDoc{Type: consts.Apps, M: map[string]interface{}{
		"_id": consts.Apps + "/drive", "slug": "drive", "state": "ready",
	}}
	require.NoError(t, couchdb.CreateNamedDoc(inst, manifest))
	billing := materialize(t, inst, 1)
	billing.CTA.URL = inst.SubDomain("drive").String()
	require.NoError(t, banner.ApplyCommand(billing))
	before := storedState(t, inst)
	trial := materialize(t, inst, 1)
	trial.Category = banner.CategoryTrial
	trial.BannerID = "trial.reminder"
	require.NoError(t, banner.ApplyCommand(trial))

	appsPath := "/" + couchdb.EscapeCouchdbName(inst.DBPrefix()+"/"+consts.Apps) + "/_all_docs"
	client := config.CouchClient()
	original := client.Transport
	t.Cleanup(func() { client.Transport = original })
	var failed atomic.Bool
	client.Transport = commandRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == appsPath {
			failed.Store(true)
			return nil, errors.New("simulated app lookup outage")
		}
		return original.RoundTrip(r)
	})

	require.NoError(t, lifecycle.Patch(inst, &lifecycle.Options{Locale: "fr"}))
	assert.True(t, failed.Load())
	assert.Equal(t, before.DocRev, storedState(t, inst).DocRev, "the failed banner must be left unchanged")
	stored, err := banner.Stored(inst, banner.CategoryTrial)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "fr", stored.Lang, "later banners must still be refreshed")
}
