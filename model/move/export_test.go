package move

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"math/rand"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
)

type Stats struct {
	TotalSize int64
	Dirs      map[string]struct{}
	Files     map[string][]byte
}

func TestExport(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	seed := time.Now().UTC().Unix()
	t.Logf("seed = %d\n", seed)
	rand.Seed(seed)
	config.UseTestFile(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()

	t.Run("ExportFiles", func(t *testing.T) {
		fs := inst.VFS()

		// The partsSize is voluntary really small to have a lot of parts,
		// which can help to test the edge cases
		exportDoc := &ExportDoc{
			PartsSize: 10,
		}

		nbFiles := rand.Intn(100)
		root, err := fs.DirByID(consts.RootDirID)
		assert.NoError(t, err)
		populateTree(t, fs, root, nbFiles)

		nbVersions, err := couchdb.CountNormalDocs(inst, consts.FilesVersions)
		assert.NoError(t, err)

		// /* Uncomment this section for debug */
		// vfs.Walk(fs, root.Fullpath, func(fpath string, dir *vfs.DirDoc, file *vfs.FileDoc, err error) error {
		// 	if err != nil {
		// 		return err
		// 	}
		// 	if fpath == root.Fullpath {
		// 		return nil
		// 	}
		// 	level := strings.Count(fpath, "/")
		// 	for i := 0; i < level; i++ {
		// 		if i == level-1 {
		// 			_, err = t.Logf("└── ")
		// 		} else {
		// 			_, err = t.Logf("|  ")
		// 		}
		// 		if err != nil {
		// 			return err
		// 		}
		// 	}
		// 	if dir != nil {
		// 		_, err = t.Log(dir.DocName)
		// 	} else {
		// 		_, err = t.Loff("%s (%d)\n", file.DocName, file.ByteSize)
		// 	}
		// 	return err
		// })
		// t.Logf("nb files = %d\n", nbFiles)

		// Build the cursors
		_, err = exportFiles(inst, exportDoc, nil)
		assert.NoError(t, err)

		// Check files
		cursors := append(exportDoc.PartsCursors, "")
		fileIDs := map[string]bool{}
		for _, c := range cursors {
			cursor, err := ParseCursor(exportDoc, c)
			assert.NoError(t, err)
			list, err := listFilesFromCursor(inst, exportDoc, cursor)
			assert.NoError(t, err)
			for _, f := range list {
				assert.False(t, fileIDs[f.DocID])
				fileIDs[f.DocID] = true
			}
		}
		assert.Len(t, fileIDs, nbFiles)

		// Check file versions
		versionsIDs := map[string]bool{}
		for _, c := range cursors {
			cursor, err := ParseCursor(exportDoc, c)
			assert.NoError(t, err)
			list, err := listVersionsFromCursor(inst, exportDoc, cursor)
			assert.NoError(t, err)
			for _, v := range list {
				assert.False(t, versionsIDs[v.DocID])
				versionsIDs[v.DocID] = true
			}
		}
		assert.Len(t, versionsIDs, nbVersions)
	})

	t.Run("ExportWithoutRecents", func(t *testing.T) {
		err := couchdb.CreateNamedDocWithDB(inst, &couchdb.JSONDoc{
			Type: consts.FilesRecents,
			M: map[string]interface{}{
				"_id":              "resource-key-one",
				"resource_key":     "resource-key-one",
				"last_activity_at": "2026-10-26T10:00:00Z",
			},
		})
		assert.NoError(t, err)

		exportDoc := &ExportDoc{PartsSize: 10}
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_, err = exportDocuments(inst, exportDoc, time.Now(), tw)
		assert.NoError(t, err)
		assert.NoError(t, tw.Close())

		tr := tar.NewReader(&buf)
		for {
			header, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			assert.NoError(t, err)
			assert.False(t, strings.HasPrefix(header.Name, consts.FilesRecents),
				"the recents must not be exported, found %s", header.Name)
		}
	})
}

func createFile(t *testing.T, fs vfs.VFS, parent *vfs.DirDoc) {
	size := 1 + rand.Intn(25)
	name := crypto.GenerateRandomString(8)
	doc, err := vfs.NewFileDoc(name, parent.DocID, -1, nil, "application/octet-stream", "application", time.Now(), false, false, false, nil)
	assert.NoError(t, err)
	doc.CozyMetadata = vfs.NewCozyMetadata("")
	file, err := fs.CreateFile(doc, nil)
	assert.NoError(t, err)
	buf := make([]byte, size)
	_, err = file.Write(buf)
	assert.NoError(t, err)
	assert.NoError(t, file.Close())

	// Create some file versions
	nb := rand.Intn(3)
	for i := 0; i < nb; i++ {
		size = 1 + rand.Intn(25)
		olddoc := doc.Clone().(*vfs.FileDoc)
		doc.CozyMetadata = olddoc.CozyMetadata.Clone()
		doc.CozyMetadata.UpdatedAt = doc.CozyMetadata.UpdatedAt.Add(1 * time.Hour)
		doc.MD5Sum = nil
		doc.ByteSize = int64(size)
		file, err = fs.CreateFile(doc, olddoc)
		assert.NoError(t, err)
		buf := make([]byte, size)
		_, err = file.Write(buf)
		assert.NoError(t, err)
		assert.NoError(t, file.Close())
	}
}

func populateTree(t *testing.T, fs vfs.VFS, parent *vfs.DirDoc, nb int) {
	nbDirs := rand.Intn(5)
	if nbDirs > nb {
		nbDirs %= (nb + 1)
	}

	// Create the sub-directories
	for i := 0; i < nbDirs; i++ {
		name := crypto.GenerateRandomString(6)
		fullpath := path.Join(parent.Fullpath, name)
		dir, err := vfs.Mkdir(fs, fullpath, nil)
		assert.NoError(t, err)
		nbFiles := rand.Intn(nb)
		populateTree(t, fs, dir, nbFiles)
		nb -= nbFiles
	}

	// Create some files
	for j := 0; j < nb; j++ {
		createFile(t, fs, parent)
	}
}
