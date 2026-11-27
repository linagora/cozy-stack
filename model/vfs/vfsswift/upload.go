package vfsswift

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/ncw/swift/v2"
)

// The content of an upload is a list of segments, and the file is a Static
// Large Object (SLO) manifest that points to them. A manifest accepts at most
// 1000 segments: the bytes received after the last segment are kept in a
// temporary object (the tail), and go in front of the next segment, so that
// the number of segments only depends on the size of the file.

func (sfs *swiftVFSV3) uploadPrefix(u *vfs.Upload) string {
	return vfs.UploadsObjectPrefix + u.UploadID + "/"
}

func (sfs *swiftVFSV3) uploadName(u *vfs.Upload) string {
	return MakeObjectNameV3(u.File.DocID, u.File.InternalID)
}

func (sfs *swiftVFSV3) StartUpload(u *vfs.Upload) error {
	u.File.InternalID = largeObjectPrefix + utils.RandomString(16-len(largeObjectPrefix))
	return nil
}

func (sfs *swiftVFSV3) putSegment(name string, content io.Reader, size int64) (*vfs.UploadPart, error) {
	headers, err := sfs.c.ObjectPut(sfs.ctx, sfs.container, name, content, false, "", "application/octet-stream", nil)
	if err != nil {
		return nil, err
	}
	return &vfs.UploadPart{
		Name: name,
		ETag: strings.Trim(headers["Etag"], `"`),
		Size: size,
	}, nil
}

// The segments and tails are named after their slot: two requests never write
// the same object.
func (sfs *swiftVFSV3) writeSegment(u *vfs.Upload, kind string, slot int, content io.Reader, size int64) (vfs.UploadPart, error) {
	part, err := sfs.putSegment(fmt.Sprintf("%s%s-%05d", sfs.uploadPrefix(u), kind, slot), content, size)
	if err != nil {
		return vfs.UploadPart{}, err
	}
	part.Number = slot
	return *part, nil
}

func (sfs *swiftVFSV3) WriteUploadPart(u *vfs.Upload, slot int, content io.Reader, size int64) (vfs.UploadPart, error) {
	return sfs.writeSegment(u, "part", slot, content, size)
}

func (sfs *swiftVFSV3) WriteUploadTail(u *vfs.Upload, slot int, content io.Reader, size int64) (vfs.UploadPart, error) {
	return sfs.writeSegment(u, "tail", slot, content, size)
}

func (sfs *swiftVFSV3) OpenUploadTail(u *vfs.Upload) (io.ReadCloser, error) {
	f, _, err := sfs.c.ObjectOpen(sfs.ctx, sfs.container, u.Tail.Name, false, nil)
	return f, err
}

func (sfs *swiftVFSV3) DeleteUploadPart(u *vfs.Upload, part vfs.UploadPart) error {
	err := sfs.c.ObjectDelete(sfs.ctx, sfs.container, part.Name)
	if errors.Is(err, swift.ObjectNotFound) {
		return nil
	}
	return err
}

type manifestSegment struct {
	Path      string `json:"path"`
	ETag      string `json:"etag"`
	SizeBytes int64  `json:"size_bytes"`
}

// AssembleUpload writes the manifest. Writing it again gives the same object.
func (sfs *swiftVFSV3) AssembleUpload(u *vfs.Upload) error {
	segments := make([]manifestSegment, len(u.Parts))
	for i, p := range u.Parts {
		segments[i] = manifestSegment{
			Path:      "/" + sfs.container + "/" + p.Name,
			ETag:      p.ETag,
			SizeBytes: p.Size,
		}
	}
	manifest, err := json.Marshal(segments)
	if err != nil {
		return err
	}
	if !sfs.c.Authenticated() {
		if err := sfs.c.Authenticate(sfs.ctx); err != nil {
			return err
		}
	}
	_, _, err = sfs.c.Call(sfs.ctx, sfs.c.StorageUrl, swift.RequestOpts{
		Container:  sfs.container,
		ObjectName: sfs.uploadName(u),
		Operation:  "PUT",
		Parameters: url.Values{"multipart-manifest": {"put"}},
		Headers:    swift.Headers{"Content-Type": u.File.Mime},
		Body:       bytes.NewReader(manifest),
		NoResponse: true,
		OnReAuth:   func() (string, error) { return sfs.c.StorageUrl, nil },
	})
	if err != nil {
		return err
	}

	inManifest := make(map[string]bool, len(u.Parts))
	for _, p := range u.Parts {
		inManifest[p.Name] = true
	}
	names, err := sfs.c.ObjectNamesAll(sfs.ctx, sfs.container, &swift.ObjectsOpts{Prefix: sfs.uploadPrefix(u)})
	if err != nil {
		sfs.log.Warnf("Cannot list the segments of upload %s: %s", u.UploadID, err)
		return nil
	}
	for _, name := range names {
		if !inManifest[name] {
			_ = sfs.c.ObjectDelete(sfs.ctx, sfs.container, name)
		}
	}
	return nil
}

func (sfs *swiftVFSV3) OpenUploadContent(u *vfs.Upload) (io.ReadCloser, error) {
	f, _, err := sfs.c.ObjectOpen(sfs.ctx, sfs.container, sfs.uploadName(u), false, nil)
	return f, err
}

func (sfs *swiftVFSV3) CommitUpload(u *vfs.Upload, olddoc *vfs.FileDoc, capsize int64, check func() error) error {
	return sfs.commitFile(u.File, olddoc, u.File.ByteSize, capsize, false, check)
}

func (sfs *swiftVFSV3) AbortUpload(u *vfs.Upload) error {
	// The manifest may have been written without the upload being recorded as
	// assembled.
	if err := sfs.deleteObject(sfs.uploadName(u)); err != nil {
		return err
	}
	names, err := sfs.c.ObjectNamesAll(sfs.ctx, sfs.container, &swift.ObjectsOpts{Prefix: sfs.uploadPrefix(u)})
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := sfs.c.ObjectDelete(sfs.ctx, sfs.container, name); err != nil && !errors.Is(err, swift.ObjectNotFound) {
			return err
		}
	}
	return nil
}

var _ vfs.UploadStorage = (*swiftVFSV3)(nil)
