package job

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sharingDoc is an io.cozy.sharings document for the events of the tests, with
// the fields that the trigger reads from the JSON of the event.
type sharingDoc struct{ membershipSharing }

func (d *sharingDoc) ID() string      { return "sharing-id" }
func (d *sharingDoc) DocType() string { return consts.Sharings }

func drive(members ...membershipMember) *membershipSharing {
	return &membershipSharing{Drive: true, Owner: true, Members: members}
}

func ready(instance string) membershipMember {
	return membershipMember{Status: "ready", Instance: instance}
}

func readOnly(instance string) membershipMember {
	return membershipMember{Status: "ready", Instance: instance, ReadOnly: true}
}

func pending(instance string) membershipMember {
	return membershipMember{Status: "pending", Instance: instance}
}

const (
	membersAlice = "https://alice.example.net"
	membersBob   = "https://bob.example.net"
)

func TestAffectedMembers(t *testing.T) {
	alice, bob := membersAlice, membersBob
	for _, tt := range []struct {
		name          string
		before, after *membershipSharing
		want          []string
	}{
		{"Accepted", drive(pending(alice), ready(bob)), drive(ready(alice), ready(bob)), []string{alice}},
		{"RightsChanged", drive(ready(alice), ready(bob)), drive(readOnly(alice), ready(bob)), []string{alice}},
		{"Revoked", drive(ready(alice), ready(bob)), drive(ready(alice)), []string{bob}},
		{"Created", nil, drive(ready(alice), ready(bob), pending("https://carol.example.net")), []string{alice, bob}},
		{"Unchanged", drive(ready(alice), pending(bob)), drive(ready(alice), pending(bob)), nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := slices.Sorted(maps.Values(affectedMembers(tt.before, tt.after)))
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRecentsMembershipsTrigger(t *testing.T) {
	config.UseTestFile(t)
	hub := realtime.GetHub()
	db := prefixer.NewPrefixer(0, "recents-members-a.test", "recents-members-a")
	alice, bob := membersAlice, membersBob
	publish := func(doc, old *membershipSharing) {
		hub.Publish(db, realtime.EventUpdate, &sharingDoc{*doc}, &sharingDoc{*old})
	}

	t.Run("MembersOfSeveralChangedDrivesArePushedOnce", func(t *testing.T) {
		broker := startTrigger(t, newRecentsMembershipsTrigger, 300*time.Millisecond)
		// In the same window, Alice accepts a drive, and another one is created
		// with Bob and her.
		publish(drive(ready(alice)), drive(pending(alice)))
		hub.Publish(db, realtime.EventCreate, &sharingDoc{*drive(ready(bob), ready(alice))}, nil)
		require.Eventually(t, func() bool {
			return broker.count() > 0
		}, 10*time.Second, 10*time.Millisecond)
		time.Sleep(50 * time.Millisecond)
		assert.Equal(t, 1, broker.count())
		assert.Equal(t, [][]string{{alice, bob}}, broker.recorded(RecentsMembershipsWorker, db))
	})

	t.Run("NonOwnerAndNonDriveAndNoChangeAreIgnored", func(t *testing.T) {
		broker := startTrigger(t, newRecentsMembershipsTrigger, 200*time.Millisecond)
		// A copy of a drive of another owner.
		publish(&membershipSharing{Drive: true, Members: []membershipMember{ready(alice)}}, &membershipSharing{Drive: true})
		// A classic sharing.
		publish(&membershipSharing{Owner: true, Members: []membershipMember{ready(alice)}}, &membershipSharing{Owner: true})
		// A drive whose membership did not change.
		publish(drive(ready(alice)), drive(ready(alice)))

		time.Sleep(500 * time.Millisecond)
		assert.Zero(t, broker.count())
	})
}
