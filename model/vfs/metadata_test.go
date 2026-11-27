package vfs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
)

func TestMetadata(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	t.Run("ImageMetadataExtractor", func(t *testing.T) {
		doc := &vfs.FileDoc{Mime: "image/png"}
		extractor := vfs.NewMetaExtractor(doc)
		assert.NotNil(t, extractor)
		f, err := os.Open("../../assets/icon-192.png")
		assert.NoError(t, err)
		defer f.Close()
		_, err = io.Copy(*extractor, f)
		assert.True(t, err == nil || errors.Is(err, io.ErrClosedPipe))
		err = (*extractor).Close()
		assert.NoError(t, err)
		meta := (*extractor).Result()
		version, ok := meta["extractor_version"].(int)
		assert.True(t, ok, "extractor_version is present")
		assert.Equal(t, vfs.MetadataExtractorVersion, version)
		w, ok := meta["width"].(int)
		assert.True(t, ok, "width is present")
		assert.Equal(t, 192, w)
		h, ok := meta["height"].(int)
		assert.True(t, ok, "height is present")
		assert.Equal(t, 192, h)
	})

	t.Run("ExifMetadataExtractor", func(t *testing.T) {
		doc := &vfs.FileDoc{Mime: "image/jpeg"}
		extractor := vfs.NewMetaExtractor(doc)
		assert.NotNil(t, extractor)
		f, err := os.Open("../../tests/fixtures/wet-cozy_20160910__M4Dz.jpg")
		assert.NoError(t, err)
		defer f.Close()
		_, err = io.Copy(*extractor, f)
		_ = (*extractor).Close()
		assert.NoError(t, err)
		meta := (*extractor).Result()
		version, ok := meta["extractor_version"].(int)
		assert.True(t, ok, "extractor_version is present")
		assert.Equal(t, vfs.MetadataExtractorVersion, version)
		dt, ok := meta["datetime"].(time.Time)
		assert.True(t, ok, "datetime is present")
		year, month, day := dt.Date()
		assert.Equal(t, 2016, year)
		assert.Equal(t, time.September, month)
		assert.Equal(t, 10, day)
		w, ok := meta["width"].(int)
		assert.True(t, ok, "width is present")
		assert.Equal(t, 440, w)
		h, ok := meta["height"].(int)
		assert.True(t, ok, "height is present")
		assert.Equal(t, 294, h)
	})

	t.Run("ShortcutMetadataExtractor", func(t *testing.T) {
		doc := &vfs.FileDoc{
			Mime: consts.ShortcutMimeType,
			CozyMetadata: &vfs.FilesCozyMetadata{
				CreatedOn: "http://cozy.localhost:8080/",
			},
		}
		extractor := vfs.NewMetaExtractor(doc)
		assert.NotNil(t, extractor)
		f, err := os.Open("../../tests/fixtures/shortcut.url")
		assert.NoError(t, err)
		defer f.Close()
		_, err = io.Copy(*extractor, f)
		_ = (*extractor).Close()
		assert.NoError(t, err)
		meta := (*extractor).Result()
		version, ok := meta["extractor_version"].(int)
		assert.True(t, ok, "extractor_version is present")
		assert.Equal(t, vfs.MetadataExtractorVersion, version)
		target, ok := meta["target"].(map[string]interface{})
		assert.True(t, ok, "target is present in metadata")
		cm, ok := target["cozyMetadata"].(map[string]interface{})
		assert.True(t, ok, "target.cozyMetadata is present")
		cozy, ok := cm["instance"].(string)
		assert.True(t, ok, "target.cozyMetadata.instance is present")
		assert.Equal(t, "cozy.localhost:8080", cozy)
		app, ok := target["app"]
		assert.True(t, ok, "target.app is present")
		assert.Equal(t, "drive", app)
	})
}

func TestAudioExtractorHostileFiles(t *testing.T) {
	extract := func(r io.Reader) vfs.Metadata {
		e := vfs.NewAudioExtractor()
		_, err := io.Copy(e, r)
		assert.NoError(t, err)
		assert.NoError(t, e.Close())
		return e.Result()
	}

	// An M4A whose \xa9alb atom has class 21 (uint8): Album() panics in the lib.
	m4a := []byte("\x00\x00\x00\x14ftypM4A \x00\x00\x00\x00M4A " +
		"\x00\x00\x00\x21ilst" +
		"\x00\x00\x00\x19\xa9alb" +
		"\x00\x00\x00\x11data\x00\x00\x00\x15\x00\x00\x00\x00\x01")
	assert.NotContains(t, extract(bytes.NewReader(m4a)), "album")

	// A file larger than what is buffered must still be fully consumed.
	big := io.MultiReader(bytes.NewReader([]byte("ID3")), io.LimitReader(zeroReader{}, 40<<20))
	assert.NotContains(t, extract(big), "album")

	// A valid M4A with an album, after a free box padded with the names of
	// the containers the lib recurses into: the album is read up to 1000 of
	// these names (maxMP4Containers), and the file is skipped above.
	box := func(name string, payload ...[]byte) []byte {
		b := bytes.Join(payload, nil)
		return append(binary.BigEndian.AppendUint32(nil, uint32(8+len(b))), append([]byte(name), b...)...)
	}
	data := box("data", []byte("\x00\x00\x00\x01\x00\x00\x00\x00Album"))
	tagged := box("moov", box("udta", box("meta", []byte{0, 0, 0, 0}, box("ilst", box("\xa9alb", data)))))
	withPadding := func(extra string) []byte {
		pad := strings.Repeat("moovudtametailst", (1000-4)/4) + extra // 4 more names in tagged
		return bytes.Join([][]byte{m4a[:20], box("free", []byte(pad)), tagged}, nil)
	}
	assert.Equal(t, "Album", extract(bytes.NewReader(withPadding("")))["album"])
	for _, name := range []string{"moov", "udta", "meta", "ilst"} {
		assert.NotContains(t, extract(bytes.NewReader(withPadding(name))), "album", name)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
