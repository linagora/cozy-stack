package vfs_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"runtime/debug"
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

	// The containers of a normal M4A are still read.
	album := []byte("\x00\x00\x00\x14ftypM4A \x00\x00\x00\x00M4A " +
		"\x00\x00\x00\x08moov\x00\x00\x00\x08udta\x00\x00\x00\x0cmeta\x00\x00\x00\x00\x00\x00\x00\x08ilst" +
		"\x00\x00\x00\x1d\xa9alb" +
		"\x00\x00\x00\x15data\x00\x00\x00\x01\x00\x00\x00\x00Album")
	assert.Equal(t, "Album", extract(bytes.NewReader(album))["album"])

	// The lib recurses once per moov header: without the bound, this file
	// overflows the stack, kept small here so that a regression fails fast.
	defer debug.SetMaxStack(debug.SetMaxStack(8 << 20))
	deep := append(m4a[:20:20], bytes.Repeat([]byte("\x00\x00\x00\x08moov"), 100_000)...)
	assert.NotContains(t, extract(bytes.NewReader(deep)), "album")
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
