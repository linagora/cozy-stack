package instance

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

var errReportIneligible = errors.New("expired link report is no longer eligible")
var errReportSnapshot = errors.New("expired link report snapshot is unavailable or changed")

// GenerateExpiredLinkReport writes or verifies the CSV for an opted-in file
// link and its expiry. This is a stack-internal operation, not an authorization
// check for a client or a guarantee about a subsequent read of the file.
func (i *Instance) GenerateExpiredLinkReport(ctx context.Context, linkID string, expiry time.Time) (*vfs.FileDoc, error) {
	expiry = expiry.UTC()
	permissionRev, err := i.reportPermissionRevision(ctx, linkID, expiry)
	if err != nil {
		return nil, err
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(linkID+"/"+expiry.Format(time.RFC3339Nano))))
	fileID := "link-report-" + key
	checkpoint := &couchdb.JSONDoc{Type: consts.PublicLinkReports}
	err = couchdb.GetDoc(i, consts.PublicLinkReports, key, checkpoint)
	if err == nil {
		fileRev, _ := checkpoint.M["file_rev"].(string)
		fingerprint, _ := checkpoint.M["file_fingerprint"].(string)
		dirID, _ := checkpoint.M["directory_id"].(string)
		if checkpoint.M["file_id"] != fileID || checkpoint.M["link_id"] != linkID {
			return nil, errReportSnapshot
		}
		file, err := i.verifyReportSnapshot(ctx, fileID, dirID, fileRev, fingerprint)
		if err != nil {
			return nil, err
		}
		if err := i.checkReportPermission(ctx, linkID, expiry, permissionRev); err != nil {
			return nil, err
		}
		return file, nil
	}
	if !couchdb.IsNotFoundError(err) {
		return nil, err
	}
	if couchdb.IsDeletedError(err) {
		return nil, errReportSnapshot
	}

	// A file without a checkpoint may be an interrupted publication or an
	// untrusted file. Do not adopt, overwrite or recreate it from a tombstone.
	err = couchdb.GetDoc(i, consts.Files, fileID, &couchdb.JSONDoc{})
	if err == nil || couchdb.IsDeletedError(err) {
		return nil, errReportSnapshot
	}
	if !couchdb.IsNotFoundError(err) {
		return nil, err
	}

	dirID, err := i.ensureReportsDirectory()
	if err != nil {
		return nil, err
	}
	file, fingerprint, err := i.writeExpiredLinkReport(ctx, linkID, expiry, permissionRev, key, dirID)
	if err != nil {
		return nil, err
	}
	published, err := i.verifyReportSnapshot(ctx, file.ID(), dirID, file.Rev(), fingerprint)
	if err != nil {
		return nil, err
	}
	if err := i.checkReportPermission(ctx, linkID, expiry, permissionRev); err != nil {
		return nil, err
	}

	// The deterministic named file and this insert-only attestation are unique
	// within the instance. Never checkpoint bytes from a competing upload.
	checkpoint.M = map[string]interface{}{
		"_id": key, "link_id": linkID, "expiry": expiry, "file_id": file.ID(),
		"file_rev": file.Rev(), "file_fingerprint": fingerprint, "directory_id": dirID,
	}
	if err := couchdb.CreateNamedDoc(i, checkpoint); err != nil {
		return nil, err
	}
	return published, nil
}

func (i *Instance) reportPermissionRevision(ctx context.Context, linkID string, expiry time.Time) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if linkID == "" || expiry.IsZero() || expiry.After(time.Now()) {
		return "", errReportIneligible
	}
	p, err := permission.GetPermissionByIDIncludingExpired(i, linkID)
	if couchdb.IsNotFoundError(err) {
		return "", errReportIneligible
	}
	if err != nil {
		return "", err
	}
	at, enabled := p.ActivityReportExpiry()
	if !enabled || !at.Equal(expiry) {
		return "", errReportIneligible
	}
	return p.Rev(), nil
}

func (i *Instance) checkReportPermission(ctx context.Context, linkID string, expiry time.Time, rev string) error {
	current, err := i.reportPermissionRevision(ctx, linkID, expiry)
	if err != nil {
		return err
	}
	if current != rev {
		return errReportIneligible
	}
	return nil
}

func (i *Instance) writeExpiredLinkReport(ctx context.Context, linkID string, expiry time.Time, permissionRev, key, dirID string) (*vfs.FileDoc, string, error) {
	tmp, err := os.CreateTemp("", "cozy-link-report-*.csv")
	if err != nil {
		return nil, "", err
	}
	defer tmp.Close()
	// A crash must not leave a named temporary file with visitor data.
	if err := os.Remove(tmp.Name()); err != nil {
		return nil, "", err
	}
	if err := publiclink.WriteReportCSV(ctx, tmp, i, linkID, expiry); err != nil {
		return nil, "", err
	}
	size, err := tmp.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, "", err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, "", err
	}
	name := "link-report-" + expiry.Format("20060102T150405Z") + "-" + key[:12] + ".csv"
	file, err := vfs.NewFileDoc(name, dirID, size, nil, "text/csv", "text", time.Now(), false, false, false, nil)
	if err != nil {
		return nil, "", err
	}
	file.DocID = "link-report-" + key
	file.Metadata = vfs.Metadata{"public_link_report": key}
	if err := i.checkReportDirectory(dirID); err != nil {
		return nil, "", err
	}
	if err := i.checkReportPermission(ctx, linkID, expiry, permissionRev); err != nil {
		return nil, "", err
	}
	digest := sha256.New()
	if err := i.publishReportCSV(ctx, file, io.TeeReader(tmp, digest), linkID, expiry, permissionRev); err != nil {
		return nil, "", err
	}
	// Bind the outgoing bytes to the original metadata, not to a document
	// discovered after publication.
	fingerprint, err := reportFingerprint(file, digest.Sum(nil))
	return file, fingerprint, err
}

func (i *Instance) publishReportCSV(ctx context.Context, file *vfs.FileDoc, src io.Reader, linkID string, expiry time.Time, permissionRev string) (err error) {
	out, err := i.VFS().CreateFile(file, nil)
	if err != nil {
		return err
	}
	finished := false
	defer func() {
		if !finished {
			err = errors.Join(err, out.Abort())
		}
	}()
	if _, err := io.Copy(out, reportReader{ctx, src}); err != nil {
		return err
	}
	if err := i.checkReportDirectory(file.DirID); err != nil {
		return err
	}
	if err := i.checkReportPermission(ctx, linkID, expiry, permissionRev); err != nil {
		return err
	}
	finished = true
	return out.Close()
}

func (i *Instance) checkReportDirectory(dirID string) error {
	current, err := i.reportsDirectory()
	if err != nil {
		return err
	}
	if dirID != current {
		return errReportSnapshot
	}
	return nil
}

type reportReader struct {
	ctx context.Context
	io.Reader
}

func (r reportReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}
