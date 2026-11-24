package vfs

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"net/url"
	"strings"
	"sync"
	"time"

	// Packages image/... are not used explicitly in the code below,
	// but are imported for their initialization side-effects
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	// Same for image/webp
	_ "golang.org/x/image/webp"

	"github.com/bradfitz/latlong"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/shortcut"
	"github.com/cozy/goexif2/exif"
	"github.com/cozy/goexif2/tiff"
	"github.com/dhowden/tag"
)

// MetadataExtractorVersion is the version number of the metadata extractor.
// It will be used later to know which files can be re-examined to get more
// metadata when the extractor is improved.
const MetadataExtractorVersion = 2

// Metadata is a list of metadata specific to each mimetype:
// id3 for music, exif for jpegs, etc.
type Metadata map[string]interface{}

// NewMetadata returns a new metadata object, with the version field set
func NewMetadata() Metadata {
	m := Metadata{}
	m["extractor_version"] = MetadataExtractorVersion
	return m
}

// MergeMetadata takes a metadata map and merges it in the FileDoc
func MergeMetadata(doc *FileDoc, meta Metadata) {
	if doc.Metadata == nil {
		doc.Metadata = meta
	} else {
		for k, v := range meta {
			// XXX: do not overwrite the target metadata for sharing shortcuts
			if k != "target" || doc.Metadata[k] == nil {
				doc.Metadata[k] = v
			}
		}
	}
}

// RemoveFavoriteMetadata returns a metadata map where the favorite key has been
// removed. It can be useful for sharing, as favorite metadata are only valid
// localy.
func (m Metadata) RemoveFavoriteMetadata() Metadata {
	if len(m) == 0 {
		return Metadata{}
	}
	result := make(Metadata, len(m))
	for k, v := range m {
		if k == consts.FavoriteKey {
			continue
		}
		result[k] = v
	}
	return result
}

// RemoveCertifiedMetadata returns a metadata map where the keys that are
// certified have been removed. It can be useful for sharing, as certified
// metadata are only valid localy.
func (m Metadata) RemoveCertifiedMetadata() Metadata {
	if len(m) == 0 {
		return Metadata{}
	}
	result := make(Metadata, len(m))
	for k, v := range m {
		if k == consts.CarbonCopyKey || k == consts.ElectronicSafeKey {
			continue
		}
		result[k] = v
	}
	return result
}

// MetaExtractor is an interface for extracting metadata from a file
type MetaExtractor interface {
	io.WriteCloser
	Abort(error)
	Result() Metadata
}

// NewMetaExtractor returns an extractor for metadata if the mime type has one,
// or null else
func NewMetaExtractor(doc *FileDoc) *MetaExtractor {
	var e MetaExtractor
	switch doc.Mime {
	case "image/jpeg":
		e = NewExifExtractor(doc.CreatedAt, true)
	case "image/heic", "image/heif":
		e = NewExifExtractor(doc.CreatedAt, false)
	case "image/png", "image/gif":
		e = NewImageExtractor(doc.CreatedAt)
	case "audio/mp3", "audio/mpeg", "audio/ogg", "audio/x-m4a", "audio/flac":
		e = NewAudioExtractor()
	case consts.ShortcutMimeType:
		var instance string
		if doc.CozyMetadata != nil {
			instance = doc.CozyMetadata.CreatedOn
		}
		var target map[string]interface{}
		if doc.Metadata != nil {
			target, _ = doc.Metadata["target"].(map[string]interface{})
		}
		e = NewShortcutExtractor(instance, target)
	}
	if e != nil {
		return &e
	}
	return nil
}

// ImageExtractor is used to extract width/height from images
type ImageExtractor struct {
	w         *io.PipeWriter
	r         *io.PipeReader
	ch        chan interface{}
	createdAt time.Time
}

// NewImageExtractor returns an extractor for images
func NewImageExtractor(createdAt time.Time) *ImageExtractor {
	e := &ImageExtractor{createdAt: createdAt}
	e.r, e.w = io.Pipe()
	e.ch = make(chan interface{})
	go e.Start()
	return e
}

// Start is used in a goroutine to start the metadata extraction
func (e *ImageExtractor) Start() {
	var cfg image.Config
	var err error
	defer func() {
		r := recover()
		if errc := e.r.Close(); err == nil {
			err = errc
		}
		if r != nil {
			e.ch <- fmt.Errorf("metadata: recovered from image decoding: %s", r)
		} else if err != nil {
			e.ch <- err
		} else {
			e.ch <- cfg
		}
	}()
	cfg, _, err = image.DecodeConfig(e.r)
}

// Write is called to push some bytes to the extractor
func (e *ImageExtractor) Write(p []byte) (n int, err error) {
	return e.w.Write(p)
}

// Close is called when all the bytes has been pushed, to finalize the extraction
func (e *ImageExtractor) Close() error {
	err := e.w.Close()
	if err != nil {
		<-e.ch
	}
	return err
}

// Abort is called when the extractor can be discarded
func (e *ImageExtractor) Abort(err error) {
	_ = e.w.CloseWithError(err)
	<-e.ch
}

// Result is called to get the extracted metadata
func (e *ImageExtractor) Result() Metadata {
	m := NewMetadata()
	m["datetime"] = e.createdAt
	cfg := <-e.ch

	if cfg, ok := cfg.(image.Config); ok {
		m["width"] = cfg.Width
		m["height"] = cfg.Height
	}

	return m
}

// ExifExtractor is used to extract EXIF metadata from jpegs
type ExifExtractor struct {
	w  *io.PipeWriter
	r  *io.PipeReader
	im *ImageExtractor
	ch chan interface{}
}

// NewExifExtractor returns an extractor for EXIF metadata
func NewExifExtractor(createdAt time.Time, withImageExtractor bool) *ExifExtractor {
	e := &ExifExtractor{}
	if withImageExtractor {
		e.im = NewImageExtractor(createdAt)
	}
	e.r, e.w = io.Pipe()
	e.ch = make(chan interface{})
	go e.Start()
	return e
}

// Start is used in a goroutine to start the metadata extraction
func (e *ExifExtractor) Start() {
	var x *exif.Exif
	var err error
	defer func() {
		r := recover()
		if errc := e.r.Close(); err == nil {
			err = errc
		}
		if r != nil {
			e.ch <- fmt.Errorf("metadata: recovered from exif extracting: %s", r)
		} else if err != nil {
			e.ch <- err
		} else {
			e.ch <- x
		}
	}()
	x, err = exif.Decode(e.r)
}

// Write is called to push some bytes to the extractor
func (e *ExifExtractor) Write(p []byte) (n int, err error) {
	if e.im != nil {
		_, _ = e.im.Write(p)
	}
	return e.w.Write(p)
}

// Close is called when all the bytes has been pushed, to finalize the extraction
func (e *ExifExtractor) Close() error {
	if e.im != nil {
		e.im.Close()
	}
	return e.w.Close()
}

// Abort is called when the extractor can be discarded
func (e *ExifExtractor) Abort(err error) {
	if e.im != nil {
		e.im.Abort(err)
	}
	_ = e.w.CloseWithError(err)
	<-e.ch
}

// Result is called to get the extracted metadata
func (e *ExifExtractor) Result() Metadata {
	var m Metadata
	if e.im != nil {
		m = e.im.Result()
	} else {
		m = NewMetadata()
	}
	select {
	case x := <-e.ch:
		if x, ok := x.(*exif.Exif); ok {
			localTZ := false
			if dt, err := x.DateTime(); err == nil {
				m["datetime"] = dt
				localTZ = dt.Location() == time.Local
			}
			if flash, err := x.Flash(); err == nil {
				m["flash"] = flash
			}
			if lat, long, err := x.LatLong(); err == nil {
				if !math.IsNaN(lat) && !math.IsNaN(long) {
					m["gps"] = map[string]float64{
						"lat":  lat,
						"long": long,
					}
					if localTZ {
						if loc := lookupLocation(latlong.LookupZoneName(lat, long)); loc != nil {
							if t, err := exifDateTimeInLocation(x, loc); err == nil {
								m["datetime"] = t
							}
						}
					}
				}
			}
			if _, ok := m["width"]; !ok {
				if xDimension, err := x.Get("PixelXDimension"); err == nil {
					if width, err := xDimension.Int(0); err == nil {
						m["width"] = width
					}
				}
			}
			if _, ok := m["height"]; !ok {
				if yDimension, err := x.Get("PixelYDimension"); err == nil {
					if height, err := yDimension.Int(0); err == nil {
						m["height"] = height
					}
				}
			}
			if o, err := x.Get("Orientation"); err == nil {
				if orientation, err := o.Int(0); err == nil {
					m["orientation"] = orientation
				}
			}
		}
	case <-time.After(1 * time.Minute):
		// Timeout when the exif parser is blocked waiting for more bytes but
		// there are no more bytes to read.
	}
	return m
}

// Code taken from perkeep
// https://github.com/perkeep/perkeep/blob/7f17c0483f2e86575ed87aac35fb75154b16b7f4/pkg/schema/schema.go#L1043-L1094

// This is basically a copy of the exif.Exif.DateTime() method, except:
//   - it takes a *time.Location to assume
//   - the caller already assumes there's no timezone offset or GPS time
//     in the EXIF, so any of that code can be ignored.
func exifDateTimeInLocation(x *exif.Exif, loc *time.Location) (time.Time, error) {
	tag, err := x.Get(exif.DateTimeOriginal)
	if err != nil {
		tag, err = x.Get(exif.DateTime)
		if err != nil {
			return time.Time{}, err
		}
	}
	if tag.Format() != tiff.StringVal {
		return time.Time{}, errors.New("DateTime[Original] not in string format")
	}
	const exifTimeLayout = "2006:01:02 15:04:05"
	dateStr := strings.TrimRight(string(tag.Val), "\x00")
	return time.ParseInLocation(exifTimeLayout, dateStr, loc)
}

var zoneCache struct {
	sync.RWMutex
	m map[string]*time.Location
}

func lookupLocation(zone string) *time.Location {
	if zone == "" {
		return nil
	}
	zoneCache.RLock()
	l, ok := zoneCache.m[zone]
	zoneCache.RUnlock()
	if ok {
		return l
	}
	loc, err := time.LoadLocation(zone)
	zoneCache.Lock()
	if zoneCache.m == nil {
		zoneCache.m = make(map[string]*time.Location)
	}
	zoneCache.m[zone] = loc // even if nil
	zoneCache.Unlock()
	if err != nil {
		return nil
	}
	return loc
}

// AudioExtractor is used to extract album/artist/etc. from audio
type AudioExtractor struct {
	w  *io.PipeWriter
	r  *io.PipeReader
	ch chan interface{}
}

// NewAudioExtractor returns an extractor for audio
func NewAudioExtractor() *AudioExtractor {
	e := &AudioExtractor{}
	e.r, e.w = io.Pipe()
	e.ch = make(chan interface{})
	go e.Start()
	return e
}

// maxAudioMetadataSize is how much of an audio file is kept in memory to read
// its tags.
// shortcut: tags after this size (ID3v1, MP4 moov at the end) are lost, read
// from the stored file if users miss them.
const maxAudioMetadataSize = 32 << 20

// Start is used in a goroutine to start the metadata extraction
func (e *AudioExtractor) Start() {
	var m Metadata
	var buf []byte
	var err error
	buf, err = io.ReadAll(io.LimitReader(e.r, maxAudioMetadataSize))
	if err == nil {
		_, err = io.Copy(io.Discard, e.r)
	}
	if err != nil {
		e.r.Close()
		e.ch <- err
		return
	}
	defer func() {
		r := recover()
		if errc := e.r.Close(); err == nil {
			err = errc
		}
		if r != nil {
			e.ch <- fmt.Errorf("metadata: recovered from audio extracting: %s", r)
		} else if err != nil {
			e.ch <- err
		} else {
			e.ch <- m
		}
	}()
	tags, err := tag.ReadFrom(bytes.NewReader(buf))
	if err != nil {
		return
	}
	// The getters can panic on hostile files, so they run under the recover.
	m = NewMetadata()
	if album := tags.Album(); album != "" {
		m["album"] = album
	}
	if artist := tags.Artist(); artist != "" {
		m["artist"] = artist
	}
	if composer := tags.Composer(); composer != "" {
		m["composer"] = composer
	}
	if genre := tags.Genre(); genre != "" {
		m["genre"] = genre
	}
	if title := tags.Title(); title != "" {
		m["title"] = title
	}
	if year := tags.Year(); year != 0 {
		m["year"] = year
	}
	if track, _ := tags.Track(); track != 0 {
		m["track"] = track
	}
}

// Write is called to push some bytes to the extractor
func (e *AudioExtractor) Write(p []byte) (n int, err error) {
	return e.w.Write(p)
}

// Close is called when all the bytes has been pushed, to finalize the extraction
func (e *AudioExtractor) Close() error {
	return e.w.Close()
}

// Abort is called when the extractor can be discarded
func (e *AudioExtractor) Abort(err error) {
	_ = e.w.CloseWithError(err)
	<-e.ch
}

// Result is called to get the extracted metadata
func (e *AudioExtractor) Result() Metadata {
	if m, ok := (<-e.ch).(Metadata); ok {
		return m
	}
	return NewMetadata()
}

// ShortcutExtractor is used to extract information from .url files
type ShortcutExtractor struct {
	w        *io.PipeWriter
	r        *io.PipeReader
	ch       chan interface{}
	instance string
	target   map[string]interface{}
}

// NewShortcutExtractor returns an extractor for .url files
func NewShortcutExtractor(instance string, target map[string]interface{}) *ShortcutExtractor {
	e := &ShortcutExtractor{}
	e.instance = instance
	e.target = target
	e.r, e.w = io.Pipe()
	e.ch = make(chan interface{})
	go e.Start()
	return e
}

// Start is used in a goroutine to start the metadata extraction
func (e *ShortcutExtractor) Start() {
	var link shortcut.Result
	var err error
	defer func() {
		r := recover()
		if errc := e.r.Close(); err == nil {
			err = errc
		}
		if r != nil {
			e.ch <- fmt.Errorf("metadata: recovered from shortcut decoding: %s", r)
		} else if err != nil {
			e.ch <- err
		} else {
			e.ch <- link
		}
	}()
	link, err = shortcut.Parse(e.r)
}

// Write is called to push some bytes to the extractor
func (e *ShortcutExtractor) Write(p []byte) (n int, err error) {
	return e.w.Write(p)
}

// Close is called when all the bytes has been pushed, to finalize the extraction
func (e *ShortcutExtractor) Close() error {
	err := e.w.Close()
	if err != nil {
		<-e.ch
	}
	return err
}

// Abort is called when the extractor can be discarded
func (e *ShortcutExtractor) Abort(err error) {
	_ = e.w.CloseWithError(err)
	<-e.ch
}

// Result is called to get the extracted metadata
func (e *ShortcutExtractor) Result() Metadata {
	m := NewMetadata()
	link := <-e.ch
	if link, ok := link.(shortcut.Result); ok {
		cozy, app := extractCozyLink(link, e.instance)
		if cozy != "" {
			target := e.target
			if target == nil {
				target = map[string]interface{}{}
			}
			target["cozyMetadata"] = map[string]interface{}{
				"instance": cozy,
			}
			if app != "" {
				target["app"] = app
			}
			m["target"] = target
		}
	}
	return m
}

func extractCozyLink(link shortcut.Result, instance string) (string, string) {
	if link.URL == "" {
		return "", ""
	}
	u, err := url.Parse(link.URL)
	if err != nil {
		return "", ""
	}
	v, err := url.Parse(instance)
	if err != nil {
		return "", ""
	}
	host, slug, _ := config.SplitCozyHost(u.Host)
	if host == v.Host {
		return host, slug
	}
	return "", ""
}
