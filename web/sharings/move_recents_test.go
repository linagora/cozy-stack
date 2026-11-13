package sharings_test

import (
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// findRecentsMove returns the message of the recents-move job pushed on the
// instance of the source owner for the given old ID, with the new ID of each
// old ID.
func findRecentsMove(t *testing.T, src *instance.Instance, oldID string) (job.RecentsMoveMessage, map[string]string) {
	t.Helper()
	jobs, err := job.GetAllJobs(src)
	require.NoError(t, err)
	for _, j := range jobs {
		if j.WorkerType != job.RecentsMoveWorker {
			continue
		}
		var msg job.RecentsMoveMessage
		require.NoError(t, j.Message.Unmarshal(&msg))
		newIDs := make(map[string]string, len(msg.Pairs))
		for _, p := range msg.Pairs {
			newIDs[p.OldID] = p.NewID
		}
		if _, ok := newIDs[oldID]; ok {
			return msg, newIDs
		}
	}
	require.Fail(t, "no recents-move job for "+oldID)
	return job.RecentsMoveMessage{}, nil
}

// TestSharedDrivesMovePushesRecentsMove checks that a move of an item to the
// drive of another owner on the same stack pushes, on the instance of the
// source owner, the job that moves the recents rows to the new IDs.
func TestSharedDrivesMovePushesRecentsMove(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test")
	}
	env := setupSharedDrivesEnv(t)

	t.Run("File", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)
		fileID := createFile(t, eA, env.meetingsDirID, "recents-file.txt", env.acmeToken)
		destDirID := createRootDirectory(t, eB, "Recents File Dest", env.bettyToken)

		resp := postMove(t, eB, env.bettyToken, `{
			"source": {"instance": "https://`+env.acme.Domain+`", "sharing_id": "`+env.firstSharingID+`", "file_id": "`+fileID+`"},
			"dest": {"dir_id": "`+destDirID+`"}
		}`)
		movedID := assertMoveResponse(t, resp, "recents-file.txt", destDirID)

		move, newIDs := findRecentsMove(t, env.acme, fileID)
		assert.Equal(t, env.betty.Domain, move.DestOwnerDomain)
		assert.Equal(t, env.firstSharingID, move.SourceDriveID)
		assert.Equal(t, map[string]string{fileID: movedID}, newIDs)
	})

	t.Run("Directory", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)
		dirID := createDirectory(t, eA, env.firstRootDirID, "recents-dir", env.acmeToken)
		fileID := createFile(t, eA, dirID, "inside.txt", env.acmeToken)
		destDirID := createRootDirectory(t, eB, "Recents Dir Dest", env.bettyToken)

		postMove(t, eB, env.bettyToken, `{
			"source": {"instance": "https://`+env.acme.Domain+`", "sharing_id": "`+env.firstSharingID+`", "dir_id": "`+dirID+`"},
			"dest": {"dir_id": "`+destDirID+`"}
		}`)

		move, newIDs := findRecentsMove(t, env.acme, dirID)
		assert.Equal(t, env.betty.Domain, move.DestOwnerDomain)
		assert.NotEmpty(t, newIDs[dirID], "the moved directory is in the job")
		assert.NotEmpty(t, newIDs[fileID], "the file it contains is in the job")
	})
}
