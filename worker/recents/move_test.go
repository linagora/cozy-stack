package recents

import (
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/pkg/consts"
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
	dstDrive := newDrive(t, dst, dstRoot.ID(), writer(bob))
	newF := newFile(t, dst, newSub, "a.txt", "hello")

	users := []*instance.Instance{src, dst, bob, carol}
	for _, inst := range users {
		putRow(t, inst, oldFile.ID())
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

	// Bob is a member of the destination drive: his rows move to the new IDs,
	// with the destination drive and the dates of the old rows.
	assertRow(t, bob, memberRow(newF, "/sub/a.txt", dstDrive, dst))
	dirRow := getRow(t, bob, newSub.ID())
	assert.Equal(t, dstDrive.SID, dirRow.DriveID)
	assert.Equal(t, consts.DirType, dirRow.Type)
	assert.Equal(t, "/sub", dirRow.Path)
	// The owner of the destination keeps the row too, without a drive.
	assertRow(t, dst, fileRow(newF, "/Move/Dst/sub/a.txt"))

	// Carol is only on the source drive, and the source owner cannot open the
	// destination either: both lose the row.
	assertNoRow(t, carol, newF.ID())
	assertNoRow(t, src, newF.ID())
	for _, inst := range users {
		assertNoRow(t, inst, oldFile.ID())
	}
	assertNoRow(t, bob, srcSub.ID())
}
