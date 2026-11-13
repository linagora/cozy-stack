package recents

import (
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/stretchr/testify/require"
)

// putOwnerRow crafts the row of a member on an item of the owner, with the
// owner_domain that the owner_domain index selects and stale metadata.
func putOwnerRow(t *testing.T, member, owner *instance.Instance, id, driveID string) *recents.FileDirActivity {
	t.Helper()
	row := putRow(t, member, id)
	row.OwnerDomain = owner.Domain
	row.DriveID = driveID
	require.NoError(t, couchdb.UpdateDoc(member, row))
	return row
}

func TestWorkerMemberships(t *testing.T) {
	insts := setupInstances(t, "owner", "member", "noflag")
	owner, member, noFlag := insts["owner"], insts["member"], insts["noflag"]
	runMembers := func(t *testing.T, members ...*instance.Instance) {
		t.Helper()
		urls := make([]string, 0, len(members))
		for _, inst := range members {
			urls = append(urls, "https://"+inst.Domain)
		}
		runWorker(t, owner, WorkerMemberships, &job.RecentsMembershipsMessage{Members: urls})
	}

	t.Run("ReaderOfOuterWriterOfNestedMovesRowToNested", func(t *testing.T) {
		outer := mkdir(t, owner, "/outer")
		nested := mkdir(t, owner, "/outer/nested")
		file := newFile(t, owner, nested, "f.txt", "hello")
		driveX := newDrive(t, owner, outer.ID(), reader(member))
		driveY := newDrive(t, owner, nested.ID(), writer(member))

		// A stale row that still points to the outer drive.
		putOwnerRow(t, member, owner, file.ID(), driveX.SID)
		runMembers(t, member)

		// The writer drive wins, with the path from its root, and the dates of
		// the activity are kept.
		assertRow(t, member, memberRow(file, "/f.txt", driveY, owner))
	})

	t.Run("RowsOfANonMemberOrWithoutTheRecentsAreLeft", func(t *testing.T) {
		dir := mkdir(t, owner, "/dir")
		file := newFile(t, owner, dir, "f.txt", "hello")
		// The drive has a member, with the recents disabled, who is not ours.
		newDrive(t, owner, dir.ID(), writer(noFlag))
		left := putOwnerRow(t, member, owner, file.ID(), "stale-drive")
		disabled := putOwnerRow(t, noFlag, owner, file.ID(), "stale-drive")

		runMembers(t, member, noFlag)
		assertUntouched(t, member, left)
		assertUntouched(t, noFlag, disabled)
	})
}
