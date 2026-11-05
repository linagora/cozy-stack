package vfs

import (
	"context"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestDownloadLinkStore(t *testing.T) {
	alice := prefixer.NewPrefixer(0, "download-link-alice", "download-link-alice")
	bob := prefixer.NewPrefixer(0, "download-link-bob", "download-link-bob")
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			var store Store
			var expire func(string)
			if backend == "memory" {
				s := newMemStore().(*memStore)
				store = s
				expire = func(key string) {
					s.mu.Lock()
					s.vals[alice.DBPrefix()+":"+key].exp = time.Now().Add(50 * time.Millisecond)
					s.mu.Unlock()
				}
			} else {
				if testing.Short() {
					t.Skip("requires isolated Redis on localhost:6379")
				}
				cli := redis.NewClient(&redis.Options{Addr: "localhost:6379", DB: 15})
				t.Cleanup(func() { _ = cli.Close() })
				store = newRedisStore(cli)
				expire = func(key string) {
					require.NoError(t, cli.PExpire(context.Background(), alice.DBPrefix()+":"+key, 50*time.Millisecond).Err())
				}
			}
			key, err := store.AddFile(alice, "/root/file")
			require.NoError(t, err)
			_, err = store.GetDownloadLink(alice, key)
			require.Error(t, err, "legacy secrets must not invent provenance")
			require.NoError(t, store.SetDownloadLink(alice, key, DownloadLink{PermissionID: "opaque-permission-id", RootID: "root"}))
			path, err := store.GetFile(alice, key)
			require.NoError(t, err)
			require.Equal(t, "/root/file", path, "legacy value is unchanged")
			id, err := store.GetDownloadLink(alice, key)
			require.NoError(t, err)
			require.Equal(t, &DownloadLink{PermissionID: "opaque-permission-id", RootID: "root"}, id)
			_, err = store.GetDownloadLink(bob, key)
			require.Error(t, err)
			require.Error(t, store.SetDownloadLink(bob, key, DownloadLink{PermissionID: "other", RootID: "root"}))
			require.Error(t, store.SetDownloadLink(alice, "missing", DownloadLink{PermissionID: "other", RootID: "root"}))

			archive := &Archive{Name: "whole", IDs: []string{"root"}}
			archiveKey, err := store.AddArchive(alice, archive)
			require.NoError(t, err)
			require.NoError(t, store.SetDownloadLink(alice, archiveKey, DownloadLink{PermissionID: "archive-permission", RootID: "root"}))
			got, err := store.GetArchive(alice, archiveKey)
			require.NoError(t, err)
			require.Equal(t, archive.IDs, got.IDs)
			id, err = store.GetDownloadLink(alice, archiveKey)
			require.NoError(t, err)
			require.Equal(t, &DownloadLink{PermissionID: "archive-permission", RootID: "root"}, id)

			expire(key)
			require.NoError(t, store.SetDownloadLink(alice, key, DownloadLink{PermissionID: "short-lived", RootID: "root"}))
			time.Sleep(100 * time.Millisecond)
			_, err = store.GetFile(alice, key)
			require.Error(t, err)
			_, err = store.GetDownloadLink(alice, key)
			require.Error(t, err)
			require.Error(t, store.SetDownloadLink(alice, key, DownloadLink{PermissionID: "expired", RootID: "root"}))
		})
	}
}
