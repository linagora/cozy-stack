package data

import (
	"testing"

	"github.com/cozy/cozy-stack/model/orgdirectory"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/stretchr/testify/require"
)

func TestRejectManagedDirectoryCreate(t *testing.T) {
	managed := &couchdb.JSONDoc{
		Type: consts.Groups,
		M: map[string]interface{}{
			orgdirectory.DirectoryMetadataKey: map[string]interface{}{
				"managed": true,
			},
		},
	}
	require.Error(t, rejectManagedDirectoryCreate(managed))

	unmanaged := &couchdb.JSONDoc{
		Type: consts.Groups,
		M:    map[string]interface{}{"name": "Engineering"},
	}
	require.NoError(t, rejectManagedDirectoryCreate(unmanaged))
}

func TestRejectManagedDirectoryUpdate(t *testing.T) {
	strippedIncoming := &couchdb.JSONDoc{
		Type: consts.Groups,
		M:    map[string]interface{}{"name": "Engineering"},
	}
	storedManaged := &couchdb.JSONDoc{
		Type: consts.Groups,
		M: map[string]interface{}{
			orgdirectory.DirectoryMetadataKey: map[string]interface{}{
				"managed": true,
			},
		},
	}
	require.Error(t, rejectManagedDirectoryUpdate(strippedIncoming, storedManaged))

	incomingManaged := &couchdb.JSONDoc{
		Type: consts.Groups,
		M: map[string]interface{}{
			orgdirectory.DirectoryMetadataKey: map[string]interface{}{
				"managed": true,
			},
		},
	}
	storedUnmanaged := &couchdb.JSONDoc{
		Type: consts.Groups,
		M:    map[string]interface{}{"name": "Engineering"},
	}
	require.Error(t, rejectManagedDirectoryUpdate(incomingManaged, storedUnmanaged))
	require.NoError(t, rejectManagedDirectoryUpdate(storedUnmanaged, storedUnmanaged))
}

func TestRejectManagedDirectoryDelete(t *testing.T) {
	managed := &couchdb.JSONDoc{
		Type: consts.Groups,
		M: map[string]interface{}{
			orgdirectory.DirectoryMetadataKey: map[string]interface{}{
				"managed": true,
			},
		},
	}
	require.Error(t, rejectManagedDirectoryDelete(managed))

	unmanaged := &couchdb.JSONDoc{
		Type: consts.Groups,
		M:    map[string]interface{}{"name": "Engineering"},
	}
	require.NoError(t, rejectManagedDirectoryDelete(unmanaged))
}

func TestIsManagedDirectoryDoctype(t *testing.T) {
	require.True(t, orgdirectory.IsManagedDirectoryDoctype(consts.Contacts))
	require.True(t, orgdirectory.IsManagedDirectoryDoctype(consts.Groups))
	require.False(t, orgdirectory.IsManagedDirectoryDoctype(consts.Files))
}
