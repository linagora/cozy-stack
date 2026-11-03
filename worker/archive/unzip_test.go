package archive

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_archive(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	setup := testutils.NewSetup(t, t.Name())

	inst := setup.GetTestInstance()

	t.Run("entry paths", func(t *testing.T) {
		fs := inst.VFS()
		dst, err := vfs.Mkdir(fs, "/path-check", nil)
		require.NoError(t, err)
		for i, tc := range []struct {
			name  string
			root  bool
			valid bool
		}{
			{"../escaped.txt", false, false},
			{"nested/../../escaped-nested.txt", false, false},
			{"../../escaped-dir/file.txt", false, false},
			{"/absolute/file.txt", false, false},
			{"../escaped-root.txt", true, false},
			{"nested/file.txt", false, true},
			{"./dot.txt", false, true},
			{"nested/../clean.txt", false, true},
			{"root.txt", true, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var buf bytes.Buffer
				writer := zip.NewWriter(&buf)
				entry, err := writer.Create(tc.name)
				require.NoError(t, err)
				_, err = io.WriteString(entry, "contents")
				require.NoError(t, err)
				require.NoError(t, writer.Close())
				doc, err := vfs.NewFileDoc(fmt.Sprintf("paths-%d.zip", i), consts.RootDirID, -1, nil,
					"application/zip", "application", time.Now(), false, false, false, nil)
				require.NoError(t, err)
				file, err := fs.CreateFile(doc, nil)
				require.NoError(t, err)
				_, err = io.Copy(file, &buf)
				require.NoError(t, err)
				require.NoError(t, file.Close())
				destination := dst.ID()
				if tc.root {
					destination = consts.RootDirID
				}
				err = unzip(fs, doc.ID(), destination)
				if tc.valid {
					require.NoError(t, err)
					base := dst.Fullpath
					if tc.root {
						base = "/"
					}
					extracted, err := fs.FileByPath(path.Join(base, tc.name))
					require.NoError(t, err)
					assert.Equal(t, int64(len("contents")), extracted.ByteSize)
				} else {
					require.Error(t, err)
				}
			})
		}
		for _, name := range []string{"/escaped.txt", "/escaped-nested.txt", "/escaped-dir", "/escaped-root.txt", "/path-check/absolute"} {
			_, err := fs.FileByPath(name)
			assert.Error(t, err, name)
			_, err = fs.DirByPath(name)
			assert.Error(t, err, name)
		}
	})

	t.Run("unzip keeps the files with a non UTF-8 name", func(t *testing.T) {
		fs := inst.VFS()
		_, err := vfs.Mkdir(fs, "/legacy", nil)
		require.NoError(t, err)

		// Names in the DOS code page (CP437), and a valid UTF-8 name, all
		// without the UTF-8 flag
		var buf bytes.Buffer
		w := zip.NewWriter(&buf)
		for _, name := range []string{"photos/\xe1", "\x82.txt", "\x8a.txt", "café.txt"} {
			f, err := w.CreateHeader(&zip.FileHeader{Name: name, NonUTF8: true})
			require.NoError(t, err)
			_, err = f.Write([]byte("content"))
			require.NoError(t, err)
		}
		require.NoError(t, w.Close())
		doc := uploadRawZip(t, fs, "legacy.zip", buf.Bytes())

		dst, err := fs.DirByPath("/legacy")
		require.NoError(t, err)
		require.NoError(t, unzip(fs, doc.ID(), dst.ID()))
		for _, name := range []string{"/legacy/photos/ß", "/legacy/é.txt", "/legacy/è.txt", "/legacy/café.txt"} {
			file, err := fs.FileByPath(name)
			require.NoError(t, err, name)
			assert.Equal(t, int64(7), file.ByteSize, name)
		}
	})

	t.Run("unzip does not keep an entry with a bad checksum", func(t *testing.T) {
		fs := inst.VFS()
		dst, err := vfs.Mkdir(fs, "/crc", nil)
		require.NoError(t, err)

		var buf bytes.Buffer
		w := zip.NewWriter(&buf)
		f, err := w.CreateHeader(&zip.FileHeader{Name: "a.txt", Method: zip.Store})
		require.NoError(t, err)
		_, err = f.Write([]byte("hello"))
		require.NoError(t, err)
		require.NoError(t, w.Close())
		raw := buf.Bytes()
		raw[14] ^= 0xff                                        // CRC-32 of the local header
		raw[bytes.Index(raw, []byte("PK\x01\x02"))+16] ^= 0xff // CRC-32 of the central directory
		doc := uploadRawZip(t, fs, "crc.zip", raw)

		assert.ErrorIs(t, unzip(fs, doc.ID(), dst.ID()), zip.ErrChecksum)
		_, err = fs.FileByPath("/crc/a.txt")
		assert.ErrorIs(t, err, os.ErrNotExist)
	})

	t.Run("test unzip", func(t *testing.T) {
		fs := inst.VFS()
		dst, err := vfs.Mkdir(fs, "/destination", nil)
		assert.NoError(t, err)
		_, err = vfs.Mkdir(fs, "/destination/foo", nil)
		assert.NoError(t, err)

		fd, err := os.Open("../../tests/fixtures/logos.zip")
		assert.NoError(t, err)
		defer fd.Close()
		zip, err := vfs.NewFileDoc("logos.zip", consts.RootDirID, -1, nil, "application/zip", "application", time.Now(), false, false, false, nil)
		assert.NoError(t, err)
		file, err := fs.CreateFile(zip, nil)
		assert.NoError(t, err)
		_, err = io.Copy(file, fd)
		assert.NoError(t, err)
		assert.NoError(t, file.Close())

		_, err = fs.OpenFile(zip)
		assert.NoError(t, err)

		err = unzip(fs, zip.ID(), dst.ID())
		assert.NoError(t, err)

		blue, err := fs.FileByPath("/destination/blue.svg")
		assert.NoError(t, err)
		assert.Equal(t, int64(2029), blue.ByteSize)

		white, err := fs.FileByPath("/destination/white.svg")
		assert.NoError(t, err)
		assert.Equal(t, int64(2030), white.ByteSize)

		baz, err := fs.FileByPath("/destination/foo/bar/baz")
		assert.NoError(t, err)
		assert.Equal(t, int64(4), baz.ByteSize)
	})

	t.Run("test zip", func(t *testing.T) {
		fs := inst.VFS()
		src, err := vfs.Mkdir(fs, "/src", nil)
		assert.NoError(t, err)
		dst, err := vfs.Mkdir(fs, "/dst", nil)
		assert.NoError(t, err)

		fd, err := os.Open("../../tests/fixtures/wet-cozy_20160910__M4Dz.jpg")
		assert.NoError(t, err)
		defer fd.Close()
		one, err := vfs.NewFileDoc("wet-cozy.jpg", src.ID(), -1, nil, "image/jpeg", "image", time.Now(), false, false, false, nil)
		assert.NoError(t, err)
		file, err := fs.CreateFile(one, nil)
		assert.NoError(t, err)
		_, err = io.Copy(file, fd)
		assert.NoError(t, err)
		assert.NoError(t, file.Close())

		at := time.Date(2019, 6, 15, 1, 2, 3, 4, time.UTC)
		two, err := vfs.NewFileDoc("hello.txt", src.ID(), -1, nil, "text/plain", "text", at, false, false, false, nil)
		assert.NoError(t, err)
		file, err = fs.CreateFile(two, nil)
		assert.NoError(t, err)
		_, err = file.Write([]byte("world"))
		assert.NoError(t, err)
		assert.NoError(t, file.Close())

		files := map[string]interface{}{
			"wet-cozy.jpg": one.ID(),
			"hello.txt":    two.ID(),
		}

		err = createZip(fs, files, src.ID(), "archive.zip")
		assert.NoError(t, err)

		zipDoc, err := fs.FileByPath("/src/archive.zip")
		assert.NoError(t, err)

		err = unzip(fs, zipDoc.ID(), dst.ID())
		assert.NoError(t, err)

		f, err := fs.FileByPath("/dst/wet-cozy.jpg")
		assert.NoError(t, err)
		assert.Equal(t, one.ByteSize, f.ByteSize)
		assert.Equal(t, one.MD5Sum, f.MD5Sum)
		// The zip archive has only a precision of a second for the modified field
		assert.Equal(t, one.UpdatedAt.Unix(), f.UpdatedAt.Unix())

		f, err = fs.FileByPath("/dst/hello.txt")
		assert.NoError(t, err)
		assert.Equal(t, two.ByteSize, f.ByteSize)
		assert.Equal(t, two.MD5Sum, f.MD5Sum)
		// The zip archive has only a precision of a second for the modified field
		assert.Equal(t, two.UpdatedAt.Unix(), f.UpdatedAt.Unix())
	})
}

func uploadRawZip(t *testing.T, fs vfs.VFS, name string, raw []byte) *vfs.FileDoc {
	doc, err := vfs.NewFileDoc(name, consts.RootDirID, int64(len(raw)), nil,
		"application/zip", "application", time.Now(), false, false, false, nil)
	require.NoError(t, err)
	file, err := fs.CreateFile(doc, nil)
	require.NoError(t, err)
	_, err = file.Write(raw)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	return doc
}
