// Package storagemigration implements the engine that copies an instance's
// object-storage content (files, versions, avatar) from one VFS backend to
// another, without touching the shared CouchDB index. It is used to move an
// instance's files between Swift and S3 — either direction, S3 to Swift as
// well as Swift to S3 — while all other instances sharing the same CouchDB
// cluster keep working against the same io.cozy.files /
// io.cozy.files.versions documents.
package storagemigration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/model/vfs/vfss3"
	"github.com/cozy/cozy-stack/model/vfs/vfsswift"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/s3util"
)

// contentWriter is implemented by target VFS backends that can write object
// bytes for a (docID, internalID) key without creating a CouchDB document.
type contentWriter interface {
	WriteContentAt(docID, internalID string, content io.Reader, size int64) error
}

// contentStater is implemented by target VFS backends that can report the
// byte size of the object backing a (docID, internalID) key without
// touching CouchDB. It returns os.ErrNotExist when the object is absent.
type contentStater interface {
	StatContentAt(docID, internalID string) (int64, error)
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

// Verify re-enumerates the same content that CopyContent copies (files,
// versions, avatar) and confirms each object exists on dst with a byte size
// matching the source CouchDB document, without creating or modifying any
// CouchDB document. It compares the counted totals against expected (the
// Report returned by CopyContent) and returns the FIRST discrepancy found as
// a descriptive error.
func Verify(db prefixer.Prefixer, dst vfs.VFS, dstAv vfs.Avatarer, expected *Report) error {
	stater, ok := dst.(contentStater)
	if !ok {
		return fmt.Errorf("storagemigration: target backend does not support index-free stats")
	}

	got := &Report{}

	if err := verifyFiles(db, stater, got); err != nil {
		return err
	}
	if err := verifyVersions(db, stater, got); err != nil {
		return err
	}

	if got.Files != expected.Files {
		return fmt.Errorf("storagemigration: verify: expected %d files, found %d on target", expected.Files, got.Files)
	}
	if got.Versions != expected.Versions {
		return fmt.Errorf("storagemigration: verify: expected %d versions, found %d on target", expected.Versions, got.Versions)
	}

	if expected.AvatarCopied {
		ar, _, err := dstAv.OpenAvatar()
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("storagemigration: verify: avatar missing on target")
		}
		if err != nil {
			return fmt.Errorf("storagemigration: verify: open target avatar: %w", err)
		}
		_ = ar.Close()
	}

	return nil
}

// sourceReport enumerates the instance's CouchDB documents (io.cozy.files
// and io.cozy.files.versions) and checks srcAv for an avatar, to compute the
// Report a FlagOnly flip expects the already-populated target to satisfy. It
// does not read or write any object-storage content itself: it describes
// what the source SHOULD have on the target, for Verify to confirm.
func sourceReport(db prefixer.Prefixer, srcAv vfs.Avatarer) (*Report, error) {
	rep := &Report{}

	err := couchdb.ForeachDocs(db, consts.Files, func(_ string, raw json.RawMessage) error {
		var doc vfs.FileDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("storagemigration: decode file doc: %w", err)
		}
		if doc.Type == consts.DirType {
			return nil
		}
		rep.Files++
		return nil
	})
	if err != nil {
		return nil, err
	}

	err = couchdb.ForeachDocs(db, consts.FilesVersions, func(_ string, _ json.RawMessage) error {
		rep.Versions++
		return nil
	})
	if err != nil {
		return nil, err
	}

	ar, _, err := srcAv.OpenAvatar()
	switch {
	case errors.Is(err, os.ErrNotExist):
		// No avatar on the source: rep.AvatarCopied stays false.
	case err != nil:
		return nil, fmt.Errorf("storagemigration: open source avatar: %w", err)
	default:
		_ = ar.Close()
		rep.AvatarCopied = true
	}

	return rep, nil
}

// verifyFiles re-enumerates every io.cozy.files document and confirms the
// target object for each non-directory file exists with a matching size.
func verifyFiles(db prefixer.Prefixer, stater contentStater, got *Report) error {
	return couchdb.ForeachDocs(db, consts.Files, func(_ string, raw json.RawMessage) error {
		var doc vfs.FileDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("storagemigration: decode file doc: %w", err)
		}
		if doc.Type == consts.DirType {
			return nil
		}

		size, err := stater.StatContentAt(doc.DocID, doc.InternalID)
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("storagemigration: verify: file %s missing on target", doc.DocID)
		}
		if err != nil {
			return fmt.Errorf("storagemigration: verify: stat target file %s: %w", doc.DocID, err)
		}
		if size != doc.ByteSize {
			return fmt.Errorf("storagemigration: verify: file %s size mismatch: expected %d, got %d", doc.DocID, doc.ByteSize, size)
		}

		got.Files++
		return nil
	})
}

// verifyVersions re-enumerates every io.cozy.files.versions document and
// confirms the target object for each version exists with a matching size.
func verifyVersions(db prefixer.Prefixer, stater contentStater, got *Report) error {
	return couchdb.ForeachDocs(db, consts.FilesVersions, func(_ string, raw json.RawMessage) error {
		var ver vfs.Version
		if err := json.Unmarshal(raw, &ver); err != nil {
			return fmt.Errorf("storagemigration: decode version doc: %w", err)
		}

		fileID, internalID := splitVersionID(ver.DocID)

		size, err := stater.StatContentAt(fileID, internalID)
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("storagemigration: verify: version %s missing on target", ver.DocID)
		}
		if err != nil {
			return fmt.Errorf("storagemigration: verify: stat target version %s: %w", ver.DocID, err)
		}
		if size != ver.ByteSize {
			return fmt.Errorf("storagemigration: verify: version %s size mismatch: expected %d, got %d", ver.DocID, ver.ByteSize, size)
		}

		got.Versions++
		return nil
	})
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

// Options configures a call to Migrate.
type Options struct {
	// To is the target storage scheme: config.SchemeS3 or config.SchemeSwift.
	To string
	// DryRun copies and verifies the content on the target backend but does
	// not flip the instance's FsScheme: the instance keeps serving reads and
	// writes from its current (source) backend. Combined with FlagOnly, it
	// still verifies the already-populated target but likewise never flips.
	DryRun bool
	// FlagOnly switches the instance's FsScheme pointer to an already
	// populated target backend without copying anything. It is intended for
	// rollback (switching back to a backend that a previous migration left
	// populated) and requires Force, since any write performed against the
	// source since that previous cutover is lost.
	FlagOnly bool
	// Force is required together with FlagOnly, acknowledging the data-loss
	// risk described above.
	Force bool
	// PurgeSource deletes the source backend's objects after a successful
	// flip. It is a best-effort cleanup performed once the instance is
	// already fully served from the target: a failure here does not revert
	// the flip.
	//
	// If To already equals the instance's current storage scheme,
	// PurgeSource switches Migrate into purge-only mode: nothing is copied,
	// verified, or flipped, and only the OTHER backend's leftover data for
	// this instance is deleted. This is what makes a deferred reclaim
	// (running --purge-source well after the flip, or retrying an inline
	// purge that failed) possible.
	PurgeSource bool
}

// containerNamer is implemented by VFS backends (vfsswift's V3 layout) that
// expose the underlying object-storage container name(s) they use, so
// storagemigration can ensure the target container exists without
// hand-rolling the naming scheme itself.
type containerNamer interface {
	ContainerNames() map[string]string
}

// Migrate moves an instance's object-storage content (files, versions,
// avatar) from its current backend to opts.To, verifies the copy, and only
// then flips the instance's FsScheme to the target. The instance is blocked
// (instance.BlockedMoving) for the duration of the copy/verify and unblocked
// on every return path.
//
// FsScheme is updated ONLY after Verify succeeds (or, for FlagOnly, after the
// target backend has been validated); a DryRun or a failed Verify always
// leaves FsScheme unchanged.
//
// If opts.To already equals the instance's current storage scheme AND
// opts.PurgeSource is set, Migrate runs in purge-only mode instead: see
// purgeOnly. Without PurgeSource, opts.To == the current scheme is still an
// error.
func Migrate(inst *instance.Instance, opts Options) (*Report, error) {
	switch opts.To {
	case config.SchemeS3, config.SchemeSwift:
	default:
		return nil, fmt.Errorf("storagemigration: unsupported target scheme %q", opts.To)
	}

	srcScheme := inst.StorageScheme()
	if opts.To == srcScheme {
		if opts.PurgeSource {
			// Purge-only mode: the instance is already on opts.To (either
			// because a previous migration flipped it, or the caller is
			// retrying an inline purge that failed after that flip). There
			// is nothing to copy, verify, or flip: just reclaim the OTHER
			// backend's leftover data for this instance.
			return purgeOnly(inst, opts.To)
		}
		return nil, fmt.Errorf("storagemigration: instance %s already uses %q as its storage scheme", inst.DomainName(), opts.To)
	}

	if opts.To == config.SchemeS3 && !config.HasS3Client() {
		return nil, errors.New("storagemigration: cannot migrate to s3: no S3 client is configured")
	}
	if opts.To == config.SchemeSwift && !config.HasSwiftConnection() {
		return nil, errors.New("storagemigration: cannot migrate to swift: no swift connection is configured")
	}

	if srcScheme == config.SchemeSwift || srcScheme == config.SchemeSwiftSecure {
		if inst.SwiftLayout != 2 {
			return nil, fmt.Errorf("storagemigration: source swift layout %d is not supported, only layout 2 (v3) can be migrated", inst.SwiftLayout)
		}
	}

	if opts.FlagOnly && !opts.Force {
		return nil, errors.New("storagemigration: flag-only migration requires Force: any write performed against the source since the previous cutover would be lost")
	}

	// Build the SOURCE from the instance's current backend before touching
	// anything (the instance already knows how to build it for its current
	// scheme).
	src := inst.VFS()
	srcAv := inst.AvatarFS()

	dst, dstAv, err := buildTarget(inst, opts.To)
	if err != nil {
		return nil, err
	}

	if err := lifecycle.Block(inst, instance.BlockedMoving.Code); err != nil {
		return nil, fmt.Errorf("storagemigration: block instance: %w", err)
	}
	defer func() {
		_ = lifecycle.Unblock(inst)
	}()

	if opts.FlagOnly {
		// FlagOnly does not copy anything, but it must not flip onto a
		// target that does not already hold the source's content: an
		// unpopulated (or merely-existing, e.g. freshly EnsureBucket'd)
		// target would otherwise silently strand the instance on zero
		// files. Compute the expected counts from the source and verify
		// the target really has them before flipping.
		expected, err := sourceReport(inst, srcAv)
		if err != nil {
			return nil, err
		}
		if err := Verify(inst, dst, dstAv, expected); err != nil {
			return expected, err
		}
		if opts.DryRun {
			return expected, nil
		}
		return flip(inst, opts, srcScheme, expected)
	}

	rep, err := CopyContent(inst, src, dst, srcAv, dstAv)
	if err != nil {
		return rep, err
	}
	if err := Verify(inst, dst, dstAv, rep); err != nil {
		return rep, err
	}

	if opts.DryRun {
		return rep, nil
	}

	return flip(inst, opts, srcScheme, rep)
}

// buildTarget constructs the VFS + Avatarer pair for the target scheme,
// ensuring the underlying bucket/container exists, without touching the
// CouchDB index (no InitFs: the index is shared with the source and must not
// be reinitialized).
func buildTarget(inst *instance.Instance, to string) (vfs.VFS, vfs.Avatarer, error) {
	index := vfs.NewCouchdbIndexer(inst)
	disk := vfs.DiskThresholder(inst)
	mutex := config.Lock().ReadWrite(inst, "vfs-migration-target")

	switch to {
	case config.SchemeS3:
		dst, err := vfss3.New(inst, index, disk, mutex)
		if err != nil {
			return nil, nil, fmt.Errorf("storagemigration: build s3 target: %w", err)
		}
		bucket := vfss3.BucketName(inst.GetOrgID(), config.GetS3BucketPrefix())
		if err := s3util.EnsureBucket(context.Background(), config.GetS3Client(), bucket, config.GetS3Region()); err != nil {
			return nil, nil, fmt.Errorf("storagemigration: ensure target bucket: %w", err)
		}
		dstAv := vfss3.NewAvatarFs(config.GetS3Client(), bucket, inst.DBPrefix()+"/")
		return dst, dstAv, nil

	case config.SchemeSwift:
		dst, err := vfsswift.NewV3(inst, index, disk, mutex)
		if err != nil {
			return nil, nil, fmt.Errorf("storagemigration: build swift target: %w", err)
		}
		if cn, ok := dst.(containerNamer); ok {
			if container, ok := cn.ContainerNames()["container"]; ok && container != "" {
				if err := config.GetSwiftConnection().ContainerCreate(context.Background(), container, nil); err != nil {
					return nil, nil, fmt.Errorf("storagemigration: ensure target container: %w", err)
				}
			}
		}
		dstAv := vfsswift.NewAvatarFsV3(config.GetSwiftConnection(), inst)
		return dst, dstAv, nil

	default:
		return nil, nil, fmt.Errorf("storagemigration: unsupported target scheme %q", to)
	}
}

// flip persists the FsScheme change to the target scheme and, if requested,
// purges the source backend's objects on a best-effort basis. It never
// reverts the flip: once the instance points at the target, the target is
// the source of truth for the instance's content.
func flip(inst *instance.Instance, opts Options, srcScheme string, rep *Report) (*Report, error) {
	inst.FsScheme = opts.To
	if err := instance.Update(inst); err != nil {
		return rep, fmt.Errorf("storagemigration: persist storage scheme flip: %w", err)
	}

	if opts.PurgeSource {
		if err := purgeSource(inst, srcScheme); err != nil {
			return rep, fmt.Errorf("storagemigration: purge source after flip: %w", err)
		}
	}

	return rep, nil
}

// purgeSource best-effort deletes the source backend's objects after a
// successful flip. The instance already fully serves reads/writes from the
// target at this point, so a purge failure is reported but never reverts the
// flip.
func purgeSource(inst *instance.Instance, srcScheme string) error {
	switch srcScheme {
	case config.SchemeS3:
		bucket := vfss3.BucketName(inst.GetOrgID(), config.GetS3BucketPrefix())
		prefix := inst.DBPrefix() + "/"
		return s3util.DeletePrefixObjects(context.Background(), config.GetS3Client(), bucket, prefix)
	case config.SchemeSwift, config.SchemeSwiftSecure:
		// The v3 swift layout uses a single, per-instance container (see
		// vfsswift.NewV3), so purging the source is just deleting that
		// container: build the same source VFS instance destroy/reset use
		// (see lifecycle.destroy/reset calling inst.VFS().Delete()) and
		// reuse its Delete(), which marks the container to-be-deleted and
		// removes all its objects before removing the container itself.
		index := vfs.NewCouchdbIndexer(inst)
		disk := vfs.DiskThresholder(inst)
		mutex := config.Lock().ReadWrite(inst, "vfs-migration-purge-source")
		src, err := vfsswift.NewV3(inst, index, disk, mutex)
		if err != nil {
			return fmt.Errorf("storagemigration: build swift source for purge: %w", err)
		}
		return src.Delete()
	default:
		return fmt.Errorf("storagemigration: purging source scheme %q is not implemented", srcScheme)
	}
}

// purgeOnly implements Migrate's purge-only mode: opts.To already equals the
// instance's current storage scheme, so there is nothing to copy, verify, or
// flip. It only deletes the OTHER backend's (still-retained) leftover data
// for this instance, which is what makes the documented deferred reclaim
// step (running --purge-source well after the flip) work, and also gives a
// retry path when an inline purge failed after a previous flip. The active
// backend (to) is never touched and the instance is not blocked, since reads
// and writes against it are unaffected.
func purgeOnly(inst *instance.Instance, to string) (*Report, error) {
	var otherScheme string
	switch to {
	case config.SchemeS3:
		otherScheme = config.SchemeSwift
	case config.SchemeSwift:
		otherScheme = config.SchemeS3
	default:
		return nil, fmt.Errorf("storagemigration: unsupported target scheme %q", to)
	}

	switch otherScheme {
	case config.SchemeS3:
		if !config.HasS3Client() {
			return nil, errors.New("storagemigration: cannot purge s3: no S3 client is configured")
		}
	case config.SchemeSwift:
		if !config.HasSwiftConnection() {
			return nil, errors.New("storagemigration: cannot purge swift: no swift connection is configured")
		}
	}

	if err := purgeSource(inst, otherScheme); err != nil {
		return nil, fmt.Errorf("storagemigration: purge-only: %w", err)
	}

	return &Report{}, nil
}
