// Package storagemigration implements the engine that copies an instance's
// object-storage content (files, versions, avatar) from one VFS backend to
// another, without touching the shared CouchDB index. It is used to move an
// instance's files between Swift and S3 — either direction, S3 to Swift as
// well as Swift to S3 — while all other instances sharing the same CouchDB
// cluster keep working against the same io.cozy.files /
// io.cozy.files.versions documents.
package storagemigration

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

// contentWriter is implemented by target VFS backends that can write object
// bytes for a (docID, internalID) key without creating a CouchDB document.
type contentWriter interface {
	WriteContentAt(docID, internalID string, content io.Reader, size int64) error
}

// Report summarizes a content copy performed by CopyContent.
type Report struct {
	Files        int
	Versions     int
	Bytes        int64
	AvatarCopied bool
}

// CopyContent copies all object-storage content (files including trashed
// ones, file versions, and the avatar) from src to dst. db is only used to
// enumerate the CouchDB documents (io.cozy.files and io.cozy.files.versions)
// that describe what content exists; CopyContent creates or modifies NO
// CouchDB document — it only reads from CouchDB and writes object bytes.
func CopyContent(db prefixer.Prefixer, src, dst vfs.VFS, srcAv, dstAv vfs.Avatarer) (*Report, error) {
	writer, ok := dst.(contentWriter)
	if !ok {
		return nil, fmt.Errorf("storagemigration: target backend does not support index-free writes")
	}

	rep := &Report{}

	if err := copyFiles(db, src, writer, rep); err != nil {
		return rep, err
	}
	if err := copyVersions(db, src, writer, rep); err != nil {
		return rep, err
	}
	if err := copyAvatar(srcAv, dstAv, rep); err != nil {
		return rep, err
	}

	return rep, nil
}

// copyFiles enumerates every io.cozy.files document (ForeachDocs is
// unfiltered, so trashed files are naturally included) and copies the
// content of each file (skipping directories) from src to dst.
func copyFiles(db prefixer.Prefixer, src vfs.VFS, writer contentWriter, rep *Report) error {
	return couchdb.ForeachDocs(db, consts.Files, func(_ string, raw json.RawMessage) error {
		var doc vfs.FileDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("storagemigration: decode file doc: %w", err)
		}
		if doc.Type == consts.DirType {
			return nil
		}

		r, err := src.OpenFile(&doc)
		if err != nil {
			return fmt.Errorf("storagemigration: open source file %s: %w", doc.DocID, err)
		}
		defer r.Close()

		if err := writer.WriteContentAt(doc.DocID, doc.InternalID, r, doc.ByteSize); err != nil {
			return fmt.Errorf("storagemigration: write target file %s: %w", doc.DocID, err)
		}

		rep.Files++
		rep.Bytes += doc.ByteSize
		return nil
	})
}

// copyVersions enumerates every io.cozy.files.versions document and copies
// the content of each version from src to dst.
func copyVersions(db prefixer.Prefixer, src vfs.VFS, writer contentWriter, rep *Report) error {
	return couchdb.ForeachDocs(db, consts.FilesVersions, func(_ string, raw json.RawMessage) error {
		var ver vfs.Version
		if err := json.Unmarshal(raw, &ver); err != nil {
			return fmt.Errorf("storagemigration: decode version doc: %w", err)
		}

		fileID, internalID := splitVersionID(ver.DocID)

		fileDoc, err := src.FileByID(fileID)
		if err != nil {
			return fmt.Errorf("storagemigration: file for version %s: %w", ver.DocID, err)
		}

		r, err := src.OpenFileVersion(fileDoc, &ver)
		if err != nil {
			return fmt.Errorf("storagemigration: open source version %s: %w", ver.DocID, err)
		}
		defer r.Close()

		if err := writer.WriteContentAt(fileID, internalID, r, ver.ByteSize); err != nil {
			return fmt.Errorf("storagemigration: write target version %s: %w", ver.DocID, err)
		}

		rep.Versions++
		rep.Bytes += ver.ByteSize
		return nil
	})
}

// copyAvatar copies the instance's avatar, if any, from srcAv to dstAv,
// setting rep.AvatarCopied on success. The absence of an avatar
// (os.ErrNotExist) is not an error.
func copyAvatar(srcAv, dstAv vfs.Avatarer, rep *Report) error {
	ar, ctype, err := srcAv.OpenAvatar()
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("storagemigration: open source avatar: %w", err)
	}
	defer ar.Close()

	w, err := dstAv.CreateAvatar(ctype)
	if err != nil {
		return fmt.Errorf("storagemigration: create target avatar: %w", err)
	}

	if _, err := io.Copy(w, ar); err != nil {
		_ = w.Close()
		return fmt.Errorf("storagemigration: copy avatar: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("storagemigration: finalize target avatar: %w", err)
	}

	rep.AvatarCopied = true
	return nil
}

// splitVersionID splits a io.cozy.files.versions document id
// ("<fileID>/<internalID>") into its fileID and internalID parts.
func splitVersionID(versionDocID string) (fileID, internalID string) {
	if i := strings.IndexByte(versionDocID, '/'); i >= 0 {
		return versionDocID[:i], versionDocID[i+1:]
	}
	return versionDocID, ""
}
