package sharing

import (
	"context"
	"errors"
	"fmt"
	"reflect"
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
		// Only owners reconcile: a recipient cannot trust the groups the owner sent it.
		if !s.Owner || !slices.ContainsFunc(s.Groups, func(g Group) bool {
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

// ReconcileOrgGroups refreshes one owned sharing from the current organization
// directory. It notifies recipients when the sharing changed, and on every retry,
// as an earlier attempt may have saved a change without notifying them.
func ReconcileOrgGroups(ctx context.Context, inst *instance.Instance, msg job.OrgGroupsMessage, retry bool) error {
	if msg.OrganizationID == "" || inst.OrgID != msg.OrganizationID || msg.SharingID == "" {
		return fmt.Errorf("organization group propagation: invalid sharing scope")
	}
	if !inst.HasCentralizedOrgDirectory() {
		return nil
	}
	org, err := findOrgInstance(inst)
	if err != nil {
		return err
	}
	if org == nil {
		return fmt.Errorf("organization group propagation: %w", instance.ErrNotFound)
	}
	s, notify, err := reconcileOrgGroupsLocked(ctx, inst, org, msg, retry)
	if notify {
		// Notify outside the lock, which replication also takes. An unreachable
		// recipient must not fail the run, as a retry would notify everyone again.
		if errn := s.notifyRecipients(inst, nil); errn != nil {
			inst.Logger().WithNamespace("sharing").
				Warnf("Cannot notify the recipients of sharing %s: %s", s.SID, errn)
		}
	}
	return err
}

// reconcileOrgGroupsLocked reconciles the sharing under its lock, and reports
// whether its recipients must be notified.
func reconcileOrgGroupsLocked(ctx context.Context, inst, org *instance.Instance, msg job.OrgGroupsMessage, retry bool) (*Sharing, bool, error) {
	mu := config.Lock().LongOperation(inst, "sharings/"+msg.SharingID)
	if err := mu.Lock(); err != nil {
		return nil, false, err
	}
	defer mu.Unlock()
	var before PutRecipientsParams
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		s, err := FindSharing(inst, msg.SharingID)
		if couchdb.IsNotFoundError(err) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		if attempt == 0 {
			before = s.recipientsParams()
		}
		matched, err := s.reconcileOrgGroups(ctx, inst, org, msg.GroupID, retry)
		if couchdb.IsConflictError(err) {
			if attempt < 3 {
				continue
			}
			return nil, false, err
		}
		// Saving pending invitations again changes the revision, not what recipients see.
		changed := !reflect.DeepEqual(before, s.recipientsParams())
		return s, matched && s.Active && (retry || changed), err
	}
}

// reconcileOrgGroups reports whether the sharing has a group to reconcile.
func (s *Sharing) reconcileOrgGroups(ctx context.Context, inst, org *instance.Instance, groupID string, retry bool) (bool, error) {
	if !s.Owner || !s.Active {
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
		// A recipient outside the organization must not get its directory groups expanded.
		if group.AddedBy != 0 && !s.memberInOrg(inst, group.AddedBy) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return matched, err
		}
		directoryGroup, err := contact.FindGroup(org, group.ID)
		if couchdb.IsNotFoundError(err) && !couchdb.IsNoDatabaseError(err) {
			// A scoped job, or the tombstone left in the directory, proves the group was deleted.
			if groupID == "" && !couchdb.IsDeletedError(err) {
				continue
			}
			matched = true
			if err := s.RevokeGroup(inst, index); err != nil {
				return matched, err
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
		contacts, err := directoryGroup.GetAllContacts(org)
		if err != nil {
			return matched, err
		}
		if err := s.reconcileGroupMembers(ctx, inst, index, contacts, retry); err != nil {
			if couchdb.IsConflictError(err) {
				return matched, err
			}
			errs = append(errs, err)
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

// memberInOrg reports whether the member at index is on an instance of inst's organization.
func (s *Sharing) memberInOrg(inst *instance.Instance, index int) bool {
	if index <= 0 || index >= len(s.Members) {
		return false
	}
	member := LocalInstanceFromURL(s.Members[index].Instance)
	return member != nil && member.OrgID == inst.OrgID
}

// reconcileGroupMembers indexes members and contacts once, and saves and sends
// invitations once, so a run stays linear in the group and sharing sizes. It
// stops on a conflict, as the sharing in memory is stale.
func (s *Sharing) reconcileGroupMembers(ctx context.Context, inst *instance.Instance, index int, contacts []*contact.Contact, retry bool) error {
	self := newPeople()
	self.add(s.Members[0].Email, s.Members[0].Instance, 0)
	selfEmail, _ := inst.SettingsEMail()
	self.add(selfEmail, inst.PageURL("", nil), 0)
	inGroup := s.groupPeople(index)
	inDirectory := newPeople()
	added, pending := false, false
	var errs []error
	for _, c := range contacts {
		if err := ctx.Err(); err != nil {
			return err
		}
		m := buildMemberFromContact(c, false)
		// Without an email or a Cozy URL, a contact cannot be matched to a member later.
		if m.Email == "" && m.Instance == "" {
			continue
		}
		inDirectory.add(m.Email, m.Instance, 0)
		if self.find(m.Email, m.Instance) >= 0 {
			continue
		}
		if i := inGroup.find(m.Email, m.Instance); i >= 0 {
			pending = pending || s.Members[i].Status == MemberStatusMailNotSent
			continue
		}
		switch err := s.addGroupMember(inst, index, c); {
		case errors.Is(err, ErrMemberAlreadyInGroup), errors.Is(err, ErrTooManyMembers):
			// Retrying cannot fix a person whose groups give different roles, or a full sharing.
			inst.Logger().WithNamespace("sharing").
				Warnf("Cannot add contact %s to group %d of sharing %s: %s", c.ID(), index, s.SID, err)
		case err != nil:
			errs = append(errs, err)
		default:
			added = true
		}
	}
	if added {
		if err := couchdb.UpdateDoc(inst, s); err != nil {
			return err
		}
	}
	// A pending invitation is sent again by a job retry, or along a new member's.
	if added || retry && pending {
		if err := s.inviteMembers(inst); couchdb.IsConflictError(err) {
			return err
		} else if err != nil {
			errs = append(errs, err)
		}
	}

	for memberIndex, member := range s.Members {
		// Like a contact without an email or a Cozy URL, such a member cannot be matched.
		if memberIndex == 0 || !slices.Contains(member.Groups, index) || (member.Email == "" && member.Instance == "") ||
			inDirectory.find(member.Email, member.Instance) >= 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		err := s.DelegatedRemoveMemberFromGroup(inst, index, memberIndex)
		if couchdb.IsConflictError(err) {
			return err
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
