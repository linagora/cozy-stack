package sharing

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/orgdirectory"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
)

// QueueOrgGroupSharings splits a page into independently retryable sharing jobs.
func QueueOrgGroupSharings(ctx context.Context, inst *instance.Instance, msg job.OrgGroupsMessage, broker job.Broker) error {
	if msg.OrganizationID == "" || inst.OrgID != msg.OrganizationID {
		return fmt.Errorf("organization group propagation: instance is outside the organization")
	}
	if !inst.HasCentralizedOrgDirectory() {
		return nil
	}
	const pageSize = 100
	var sharings []*Sharing
	// Bookmarks can skip the next row when the previous page's last sharing is deleted.
	err := couchdb.FindDocsUnoptimized(inst, consts.Sharings, &couchdb.FindRequest{
		UseIndex: "_all_docs",
		Selector: mango.And(mango.Gt("_id", msg.AfterID), mango.Equal("active", true)),
		Limit:    pageSize,
	}, &sharings)
	if couchdb.IsNoDatabaseError(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, s := range sharings {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !slices.ContainsFunc(s.Groups, func(g Group) bool {
			return !g.Revoked && orgdirectory.IsManagedDirectoryGroupID(g.ID) && (msg.GroupID == "" || msg.GroupID == g.ID)
		}) {
			continue
		}
		next := msg
		next.SharingID, next.AfterID = s.ID(), ""
		if err := queueOrgGroups(broker, inst, next); err != nil {
			return err
		}
	}
	if len(sharings) == pageSize {
		msg.AfterID = sharings[len(sharings)-1].ID()
		return queueOrgGroups(broker, inst, msg)
	}
	return nil
}

func queueOrgGroups(broker job.Broker, inst *instance.Instance, msg job.OrgGroupsMessage) error {
	message, err := job.NewMessage(msg)
	if err != nil {
		return err
	}
	_, err = broker.PushJob(inst, &job.JobRequest{WorkerType: job.OrgGroupsWorker, Message: message})
	return err
}

// ReconcileOrgGroups refreshes one sharing from the current organization directory.
// The owner notifies recipients when the sharing changed, and on every retry, as
// an earlier attempt may have saved a change without notifying them.
func ReconcileOrgGroups(ctx context.Context, inst *instance.Instance, msg job.OrgGroupsMessage, retry bool) error {
	if msg.OrganizationID == "" || inst.OrgID != msg.OrganizationID || msg.SharingID == "" {
		return fmt.Errorf("organization group propagation: invalid sharing scope")
	}
	if !inst.HasCentralizedOrgDirectory() {
		return nil
	}
	mu := config.Lock().LongOperation(inst, "sharings/"+msg.SharingID)
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()
	org, err := orgdirectory.FindOrganizationInstance(ctx, msg.OrganizationID, "")
	if err != nil {
		return err
	}
	if org == nil {
		return fmt.Errorf("organization group propagation: %w", instance.ErrNotFound)
	}
	firstRev := ""
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		s, err := FindSharing(inst, msg.SharingID)
		if couchdb.IsNotFoundError(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if firstRev == "" {
			firstRev = s.Rev()
		}
		matched, err := s.reconcileOrgGroups(ctx, inst, org, msg.GroupID)
		if couchdb.IsConflictError(err) {
			if attempt < 3 {
				continue
			}
			return err
		}
		if matched && s.Owner && s.Active && (retry || s.Rev() != firstRev) {
			err = errors.Join(err, s.notifyRecipients(inst, nil))
		}
		return err
	}
}

// reconcileOrgGroups reports whether the sharing has a group to reconcile.
func (s *Sharing) reconcileOrgGroups(ctx context.Context, inst, org *instance.Instance, groupID string) (bool, error) {
	if !s.Active {
		return false, nil
	}
	if len(s.Members) == 0 {
		return false, ErrInvalidSharing
	}
	matched := false
	var errs []error
	for index, group := range s.Groups {
		if !orgdirectory.IsManagedDirectoryGroupID(group.ID) || (groupID != "" && group.ID != groupID) {
			continue
		}
		if group.Revoked {
			matched = matched || groupID != ""
			continue
		}
		// Only the recipient that added a group may delegate its changes.
		canChangeMembers := s.Owner || (group.AddedBy > 0 && group.AddedBy < len(s.Members) && s.MemberFor(inst) == &s.Members[group.AddedBy])
		if err := ctx.Err(); err != nil {
			return matched, err
		}
		directoryGroup, err := contact.FindGroup(org, group.ID)
		if couchdb.IsNotFoundError(err) && !couchdb.IsNoDatabaseError(err) {
			// Only an explicitly scoped job proves ownership of a deleted group.
			if groupID == "" {
				continue
			}
			matched = true
			if s.Owner {
				if err := s.RevokeGroup(inst, index); err != nil {
					return matched, err
				}
			} else {
				if canChangeMembers {
					if err := s.DelegateRevokeGroup(inst, index); err != nil {
						return matched, err
					}
				}
				s.Groups[index].Revoked = true
				if err := couchdb.UpdateDoc(inst, s); err != nil {
					return matched, err
				}
			}
			continue
		}
		if err != nil {
			return matched, err
		}
		if !orgdirectory.OwnsGroup(org.OrgID, directoryGroup) {
			continue
		}
		matched = true
		if canChangeMembers {
			contacts, err := directoryGroup.GetAllContacts(org)
			if err != nil {
				return matched, err
			}
			if err := s.reconcileGroupMembers(ctx, inst, index, contacts); err != nil {
				if couchdb.IsConflictError(err) {
					return matched, err
				}
				errs = append(errs, err)
			}
		}
		if !s.Active {
			return matched, errors.Join(errs...)
		}
		if group.Name != directoryGroup.Name() || group.Color != directoryGroup.Color() {
			s.Groups[index].Name = directoryGroup.Name()
			s.Groups[index].Color = directoryGroup.Color()
			if err := couchdb.UpdateDoc(inst, s); err != nil {
				return matched, err
			}
		}
	}
	return matched, errors.Join(errs...)
}

// reconcileGroupMembers indexes members and contacts once, so a run stays
// linear in the group and sharing sizes. It stops on a conflict, as the
// sharing in memory is stale.
func (s *Sharing) reconcileGroupMembers(ctx context.Context, inst *instance.Instance, index int, contacts []*contact.Contact) error {
	self := newPeople()
	self.add(s.Members[0].Email, s.Members[0].Instance, 0)
	selfEmail, _ := inst.SettingsEMail()
	self.add(selfEmail, inst.PageURL("", nil), 0)
	inGroup := s.groupPeople(index)
	inDirectory := newPeople()
	var errs []error
	for _, c := range contacts {
		if err := ctx.Err(); err != nil {
			return err
		}
		m := buildMemberFromContact(c, false)
		inDirectory.add(m.Email, m.Instance, 0)
		if self.find(m.Email, m.Instance) >= 0 {
			continue
		}
		// An existing member only needs another try at a pending invitation, on the owner.
		if i := inGroup.find(m.Email, m.Instance); i >= 0 && (!s.Owner || s.Members[i].Status != MemberStatusMailNotSent) {
			continue
		}
		var err error
		if s.Owner {
			err = s.AddMemberToGroup(inst, index, c)
		} else {
			err = s.DelegateAddMemberToGroup(inst, index, c)
		}
		switch {
		case couchdb.IsConflictError(err):
			return err
		case errors.Is(err, ErrMemberAlreadyInGroup):
			// Retrying cannot fix a person whose groups give different roles.
			inst.Logger().WithNamespace("sharing").
				Warnf("Cannot add contact %s to group %d of sharing %s: %s", c.ID(), index, s.SID, err)
		case err != nil:
			errs = append(errs, err)
		}
	}

	for memberIndex, member := range s.Members {
		if memberIndex == 0 || !slices.Contains(member.Groups, index) || inDirectory.find(member.Email, member.Instance) >= 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		if s.Owner {
			err = s.DelegatedRemoveMemberFromGroup(inst, index, memberIndex)
		} else {
			err = s.sendDelegationRequest(inst, "DELETE", fmt.Sprintf("/sharings/%s/groups/%d/%d", s.SID, index, memberIndex))
		}
		if couchdb.IsConflictError(err) {
			return err
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
