package orgdirectory

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/stretchr/testify/require"
)

type propagationBroker struct {
	job.Broker
	push func(prefixer.Prefixer, *job.JobRequest) (*job.Job, error)
}

func (b propagationBroker) PushJob(db prefixer.Prefixer, req *job.JobRequest) (*job.Job, error) {
	return b.push(db, req)
}

func TestPushGroupJobs(t *testing.T) {
	config.UseTestFile(t)
	conf := config.GetConfig()
	previous := conf.Contexts
	conf.Contexts = map[string]interface{}{
		"central":    map[string]interface{}{"centralized_org_directory": true, "org_groups_propagation_delay": "45s"},
		"replicated": map[string]interface{}{},
	}
	t.Cleanup(func() { conf.Contexts = previous })
	scope := OrganizationInstances{OrganizationID: "org", Instances: []*instance.Instance{
		{Domain: "alice.example", ContextName: "central"},
		{Domain: "bob.example", ContextName: "central"},
		{Domain: "carol.example", ContextName: "replicated"},
	}}
	groupID := GroupDocID("org", "team")
	wantErr := errors.New("queue unavailable")
	var domains []string
	broker := propagationBroker{push: func(db prefixer.Prefixer, req *job.JobRequest) (*job.Job, error) {
		domains = append(domains, db.DomainName())
		require.Equal(t, job.OrgGroupsWorker, req.WorkerType)
		var msg job.OrgGroupsMessage
		require.NoError(t, req.Message.Unmarshal(&msg))
		require.Equal(t, job.OrgGroupsMessage{OrganizationID: "org", GroupID: groupID}, msg)
		if db.DomainName() == "alice.example" {
			return nil, wantErr
		}
		return &job.Job{}, nil
	}}
	for range 2 {
		domains = nil
		require.ErrorIs(t, pushGroupJobs(t.Context(), broker, scope, groupID), wantErr)
		require.Equal(t, []string{"alice.example", "bob.example"}, domains, "a failed target does not prevent scheduling another")
	}
}

type triggerRecorder struct {
	job.JobSystem
	added []job.Trigger
	count int
}

func (r *triggerRecorder) GetAllTriggers(prefixer.Prefixer) ([]job.Trigger, error) {
	return slices.Clone(r.added), nil
}

func (r *triggerRecorder) DeleteTrigger(_ prefixer.Prefixer, id string) error {
	r.added = slices.DeleteFunc(r.added, func(t job.Trigger) bool { return t.ID() == id })
	return nil
}

func (r *triggerRecorder) AddTrigger(t job.Trigger) error {
	r.count++
	t.Infos().TID = fmt.Sprintf("trigger-%d", r.count)
	r.added = append(r.added, t)
	return nil
}

func TestDebounceGroupPropagation(t *testing.T) {
	config.UseTestFile(t)
	conf := config.GetConfig()
	previous := conf.Contexts
	conf.Contexts = map[string]interface{}{
		"central":    map[string]interface{}{"centralized_org_directory": true, "org_groups_propagation_delay": "45s"},
		"replicated": map[string]interface{}{},
	}
	t.Cleanup(func() { conf.Contexts = previous })
	scope := func(orgContext string) OrganizationInstances {
		return OrganizationInstances{OrganizationID: "org", Instances: []*instance.Instance{
			{Domain: "alice.example", OrgID: "org", ContextName: orgContext},
			{Domain: "org.example", OrgID: "org", ContextName: orgContext},
		}}
	}

	sys := &triggerRecorder{}
	for range 3 {
		require.NoError(t, debounceGroupPropagation(sys, scope("central")))
	}
	require.Len(t, sys.added, 1, "events within the window share one run")
	trigger := sys.added[0].Infos()
	require.Equal(t, "org.example", trigger.Domain, "the organization instance fans out")
	require.Equal(t, "@in", trigger.Type)
	require.Equal(t, "45s", trigger.Arguments)
	require.Equal(t, defaultGroupPropagationDelay, groupPropagationDelay(&instance.Instance{ContextName: "replicated"}))
	var msg job.OrgGroupsMessage
	require.NoError(t, trigger.Message.Unmarshal(&msg))
	require.Equal(t, job.OrgGroupsMessage{OrganizationID: "org", FanOut: true}, msg)

	// A trigger that should have fired long ago lost its Redis entry.
	trigger.Metadata.CreatedAt = time.Now().Add(-staleTriggerDelays * 45 * time.Second)
	require.NoError(t, debounceGroupPropagation(sys, scope("central")))
	require.Len(t, sys.added, 1)
	require.NotEqual(t, trigger.TID, sys.added[0].ID(), "a stale trigger is replaced")

	sys = &triggerRecorder{}
	require.NoError(t, debounceGroupPropagation(sys, scope("replicated")))
	noOrgInstance := scope("central")
	noOrgInstance.Instances = noOrgInstance.Instances[:1]
	require.NoError(t, debounceGroupPropagation(sys, noOrgInstance))
	require.Empty(t, sys.added, "nothing to propagate without a centralized directory")
	require.ErrorIs(t, debounceGroupPropagation(nil, scope("central")), errNoJobSystem)
}

// Directory tests inspect writes and enqueueing without executing sharing workers.
func startDirectoryTestJobs(t *testing.T) {
	t.Helper()
	if job.SystemUnsafe() == nil {
		broker := propagationBroker{Broker: job.NewMemBroker(), push: func(prefixer.Prefixer, *job.JobRequest) (*job.Job, error) {
			return &job.Job{}, nil
		}}
		require.NoError(t, job.SystemStart(broker, job.NewMemScheduler(), nil))
	}
}
