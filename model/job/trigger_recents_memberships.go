package job

import (
	"encoding/json"
	"net/url"
	"time"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/realtime"
)

// recentsMembershipsDelay is how long the trigger gathers the membership
// changes of an instance before pushing a job.
const recentsMembershipsDelay = 10 * time.Second

// recentsMembershipsBatchSize is the maximum number of members of a job.
const recentsMembershipsBatchSize = 50

// memberStatusReady is the status of a member that has accepted the sharing. It
// mirrors sharing.MemberStatusReady, which this package cannot import: the
// sharing package depends on the job package.
const memberStatusReady = "ready"

// membershipSharing is the part of an io.cozy.sharings drive document that the
// trigger reads from an event. The trigger lives in the job package, which the
// sharing package depends on, so it cannot use the sharing types: it reads the
// document from its JSON instead.
type membershipSharing struct {
	Drive   bool               `json:"drive"`
	Owner   bool               `json:"owner"`
	Members []membershipMember `json:"members"`
}

// membershipMember is the part of a member entry that decides whether and how a
// drive covers an item for the member: the member must be ready to be a
// candidate, and read_only tells apart a reader from a writer.
type membershipMember struct {
	Status   string `json:"status"`
	Instance string `json:"instance"`
	ReadOnly bool   `json:"read_only"`
}

// memberAccess is the state of a member that the drive resolution depends on. A
// member that is not ready is not a candidate at all; among the ready ones, the
// read-only flag decides which drive wins. A change of this state is what moves
// the rows of a member, so the trigger compares it between the old and the new
// document of an event.
type memberAccess struct {
	ready    bool
	readOnly bool
}

// NewRecentsMembershipsTrigger creates the trigger that pushes the
// recents-memberships jobs for the drives of all the instances whose membership
// changes. It listens to the same firehose as the recents-sync trigger, so the
// same remarks on the lost events of a stopped process apply.
func NewRecentsMembershipsTrigger(broker Broker) *FirehoseTrigger {
	return newRecentsMembershipsTrigger(broker, recentsMembershipsDelay)
}

func newRecentsMembershipsTrigger(broker Broker, delay time.Duration) *FirehoseTrigger {
	t := &FirehoseTrigger{
		worker:    RecentsMembershipsWorker,
		delay:     delay,
		batchSize: recentsMembershipsBatchSize,
		collect:   collectAffectedMembers,
		message:   func(members []string) any { return &RecentsMembershipsMessage{Members: members} },
	}
	return t.init(broker)
}

// collectAffectedMembers gives the Cozy URL of each member affected by a change
// of a sharing, keyed by the host so that a member of several changed drives is
// pushed once.
func collectAffectedMembers(e *realtime.Event, add func(key, value string)) {
	if e.Doc == nil || e.Doc.DocType() != consts.Sharings {
		return
	}
	oldDoc := parseMembershipSharing(e.OldDoc)
	newDoc := parseMembershipSharing(e.Doc)
	// Only a drive owned here changes the rows on the items of this instance:
	// the copy of a drive of another owner, or a classic sharing, do not.
	ref := newDoc
	if ref == nil {
		ref = oldDoc
	}
	if ref == nil || !ref.Drive || !ref.Owner {
		return
	}
	for host, instance := range affectedMembers(oldDoc, newDoc) {
		add(host, instance)
	}
}

// parseMembershipSharing reads the fields of a sharing document from an event.
// It returns nil for a missing document, e.g. the old document of a creation.
func parseMembershipSharing(doc realtime.Doc) *membershipSharing {
	if doc == nil {
		return nil
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil
	}
	var s membershipSharing
	if err := json.Unmarshal(b, &s); err != nil {
		return nil
	}
	return &s
}

// affectedMembers returns the Cozy URL of each member whose drive-resolution
// state changed between the old and the new document, keyed by the host. A
// member is affected when it becomes or stops being ready, or when its
// read_only flag changes while ready: in every case the drive chosen for its
// items may change. A member on no instance (no Cozy URL) is left out: it holds
// no row on this stack.
func affectedMembers(oldDoc, newDoc *membershipSharing) map[string]string {
	// For each member, keyed by host, its state in the old and the new
	// document and its Cozy URL. A member missing from a document reads as the
	// zero state there, which is not ready: a creation and a deletion then
	// compare every ready member against nothing.
	type change struct {
		before, after memberAccess
		instance      string
	}
	changes := make(map[string]*change)
	collect := func(doc *membershipSharing, apply func(*change, memberAccess)) {
		if doc == nil {
			return
		}
		for _, m := range doc.Members {
			host := instanceHost(m.Instance)
			if host == "" {
				continue
			}
			c := changes[host]
			if c == nil {
				c = &change{}
				changes[host] = c
			}
			c.instance = m.Instance
			apply(c, memberAccess{ready: m.Status == memberStatusReady, readOnly: m.ReadOnly})
		}
	}
	collect(oldDoc, func(c *change, a memberAccess) { c.before = a })
	collect(newDoc, func(c *change, a memberAccess) { c.after = a })

	affected := make(map[string]string)
	for host, c := range changes {
		if c.before != c.after {
			affected[host] = c.instance
		}
	}
	return affected
}

func instanceHost(instance string) string {
	u, err := url.Parse(instance)
	if err != nil {
		return ""
	}
	return u.Host
}
