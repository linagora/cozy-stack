package vfs_test

import (
	"bytes"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDirSizeCountsEverySubdirectory(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	fs := makeAferoFS(t)

	// /sizes holds a 7-byte file and six subdirectories over three levels,
	// each holding a 1000-byte file.
	root, err := vfs.Mkdir(fs, "/sizes", nil)
	require.NoError(t, err)
	writeFile(t, fs, root, "top", 7)
	for _, name := range []string{"/sizes/a", "/sizes/a/b", "/sizes/a/b/c", "/sizes/d", "/sizes/e", "/sizes/e/f"} {
		dir, err := vfs.Mkdir(fs, name, nil)
		require.NoError(t, err)
		writeFile(t, fs, dir, "file", 1000)
	}
	// A sibling whose path starts like the directory's is not inside it.
	other, err := vfs.Mkdir(fs, "/sizes-other", nil)
	require.NoError(t, err)
	writeFile(t, fs, other, "file", 50)

	// Batches that divide the six subdirectories evenly end on an empty read.
	for _, batch := range []int{1, 2, 3, 4, 10000} {
		t.Run(fmt.Sprintf("%d at a time", batch), func(t *testing.T) {
			t.Cleanup(vfs.SetDirSizeBatchForTest(batch))

			size, count, err := fs.DirSize(root)
			require.NoError(t, err)
			assert.Equal(t, int64(6007), size)
			assert.Equal(t, int64(7), count)
		})
	}
}

func writeFile(t *testing.T, fs vfs.VFS, dir *vfs.DirDoc, name string, size int) {
	t.Helper()

	doc, err := vfs.NewFileDoc(name, dir.ID(), int64(size), nil, "application/octet-stream", "files", time.Now(), false, false, false, nil)
	require.NoError(t, err)
	f, err := fs.CreateFile(doc, nil)
	require.NoError(t, err)
	_, err = io.Copy(f, bytes.NewReader(make([]byte, size)))
	require.NoError(t, err)
	require.NoError(t, f.Close())
}
