package recents

import (
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/stretchr/testify/assert"
)

func TestWorkerMove(t *testing.T) {
	insts := setupInstances(t, "src", "dst", "bob", "carol")
	src, dst, bob, carol := insts["src"], insts["dst"], insts["bob"], insts["carol"]

	// A folder and its file move from a drive of src to a drive of dst. Bob is
	// a member of both drives, carol and dst of the source drive only.
	srcRoot := mkdir(t, src, "/Move/Src")
	srcSub := mkdir(t, src, "/Move/Src/sub")
	srcDrive := newDrive(t, src, srcRoot.ID(), writer(bob), writer(carol), writer(dst))
	oldFile := newFile(t, src, srcSub, "a.txt", "hello")
	dstRoot := mkdir(t, dst, "/Move/Dst")
	newSub := mkdir(t, dst, "/Move/Dst/sub")
	newDrive(t, dst, dstRoot.ID(), writer(bob))
	newF := newFile(t, dst, newSub, "a.txt", "hello")

	users := []*instance.Instance{src, dst, bob, carol}
	olds := make(map[string]*recents.FileDirActivity)
	for _, inst := range users {
		olds[inst.Domain] = putRow(t, inst, oldFile.ID())
	}
	putRow(t, bob, srcSub.ID())

	runWorker(t, src, WorkerMove, &job.RecentsMoveMessage{
		Pairs: []job.RecentsMovePair{
			{OldID: srcSub.ID(), NewID: newSub.ID()},
			{OldID: oldFile.ID(), NewID: newF.ID()},
		},
		DestOwnerDomain: dst.Domain,
		SourceDriveID:   srcDrive.SID,
	})

	// Every user of the source keeps the row, under the new ID, with what the
	// old row held. Carol is only on the source drive and the source owner is
	// not on the destination drive: they cannot open the destination, which is
	// for the list to tell.
	for _, inst := range users {
		assertNoRow(t, inst, oldFile.ID())
		old := olds[inst.Domain]
		got := getRow(t, inst, newF.ID())
		want := &recents.FileDirActivity{
			DocID: newF.ID(), DocRev: got.DocRev, OwnerDomain: dst.Domain,
			LastEditedAt: old.LastEditedAt, LastViewedAt: old.LastViewedAt, LastActivityAt: old.LastActivityAt,
			Name: old.Name, Type: old.Type, SchemaVersion: recents.SchemaVersion,
		}
		if inst == dst {
			// The owner of the destination holds the item.
			want.OwnerDomain = ""
		}
		assert.Equal(t, want, got)
	}
	assertNoRow(t, bob, srcSub.ID())
	assert.Equal(t, dst.Domain, getRow(t, bob, newSub.ID()).OwnerDomain)
}
