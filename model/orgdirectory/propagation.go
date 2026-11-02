package orgdirectory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
)

// defaultGroupPropagationDelay is how long directory changes are collected into
// one propagation run, unless the context sets org_groups_propagation_delay.
const defaultGroupPropagationDelay = 30 * time.Second

var errNoJobSystem = errors.New("organization group propagation: job system is not initialized")

// QueueGroupPropagation schedules a reconciliation of all organization groups
// on every member instance. Changes made within the propagation delay of the
// first one share a single run, as the run reads the current directory.
// Call it even when a redelivered event no longer changes the directory.
func QueueGroupPropagation(ctx context.Context, organizationID, organizationDomain string) error {
	scope, err := ResolveOrganizationInstances(organizationID, organizationDomain)
	if err != nil {
		return err
	}
	return debounceGroupPropagation(job.SystemUnsafe(), scope)
}

func debounceGroupPropagation(sys job.JobSystem, scope OrganizationInstances) error {
	i := slices.IndexFunc(scope.Instances, (*instance.Instance).IsOrganizationInstance)
	if i < 0 || !scope.Instances[i].HasCentralizedOrgDirectory() {
		return nil
	}
	org := scope.Instances[i]
	if sys == nil {
		return errNoJobSystem
	}
	infos := job.TriggerInfos{Type: "@in", WorkerType: job.OrgGroupsWorker, Arguments: groupPropagationDelay(org).String()}
	// shortcut: a change made while the trigger fires, before it is deleted, waits
	// for the next change; switch to an atomic Redis window if that shows up.
	if sys.HasTrigger(org, infos) {
		return nil
	}
	msg, err := job.NewMessage(job.OrgGroupsMessage{OrganizationID: scope.OrganizationID, FanOut: true})
	if err != nil {
		return err
	}
	t, err := job.NewTrigger(org, infos, msg)
	if err != nil {
		return err
	}
	return sys.AddTrigger(t)
}

func groupPropagationDelay(org *instance.Instance) time.Duration {
	settings, _ := org.SettingsContext()
	delay, _ := settings["org_groups_propagation_delay"].(string)
	if d, err := time.ParseDuration(delay); err == nil && d > 0 {
		return d
	}
	return defaultGroupPropagationDelay
}

// QueueGroupDeletion propagates a deleted group right away: only a job naming
// the group may revoke it, so it cannot share the debounced run.
func QueueGroupDeletion(ctx context.Context, organizationID, groupID string) error {
	scope, err := ResolveOrganizationInstances(organizationID, "")
	if err != nil {
		return err
	}
	return pushGroupJobs(ctx, job.SystemUnsafe(), scope, groupID)
}

// FanOutGroupPropagation runs on the organization instance when the debounced
// trigger fires, and schedules the reconciliation on every member instance.
func FanOutGroupPropagation(ctx context.Context, org *instance.Instance, organizationID string) error {
	if organizationID == "" || org.OrgID != organizationID || !org.IsOrganizationInstance() {
		return fmt.Errorf("organization group propagation: %s is not the organization instance", org.Domain)
	}
	scope, err := ResolveOrganizationInstances(organizationID, "")
	if err != nil {
		return err
	}
	return pushGroupJobs(ctx, job.SystemUnsafe(), scope, "")
}

func pushGroupJobs(ctx context.Context, broker job.Broker, scope OrganizationInstances, groupID string) error {
	msg, err := job.NewMessage(job.OrgGroupsMessage{OrganizationID: scope.OrganizationID, GroupID: groupID})
	if err != nil {
		return err
	}
	var errs []error
	for _, inst := range scope.Instances {
		if !inst.HasCentralizedOrgDirectory() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if broker == nil {
			return errNoJobSystem
		}
		if _, err := broker.PushJob(inst, &job.JobRequest{WorkerType: job.OrgGroupsWorker, Message: msg}); err != nil {
			errs = append(errs, fmt.Errorf("propagate groups to %s: %w", inst.Domain, err))
		}
	}
	return errors.Join(errs...)
}

// OwnsGroup validates a group against its directory and stable identifier.
func OwnsGroup(organizationID string, group *contact.Group) bool {
	meta := directoryMetadata(&group.JSONDoc)
	return meta.Managed && meta.Kind == metadataKindGroup && meta.OrganizationID == organizationID &&
		GroupDocID(organizationID, meta.ExternalID) == group.ID()
}
