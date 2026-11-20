package instance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

type reportSnapshot struct {
	file        *vfs.FileDoc
	revision    string
	fingerprint string
}

func (i *Instance) verifyReportSnapshot(ctx context.Context, fileID, dirID, originRev, fingerprint string, consume func(io.Reader) error, previous *reportSnapshot) (*reportSnapshot, error) {
	if originRev == "" || fingerprint == "" || (previous != nil && fingerprint != previous.fingerprint) {
		return nil, errReportSnapshot
	}
	if err := i.checkReportDirectory(dirID); err != nil {
		return nil, err
	}
	fs := i.VFS()
	file, err := fs.FileByID(fileID)
	if err != nil {
		return nil, err
	}
	if file.DirID != dirID || file.Trashed || (file.AntivirusScan != nil && file.AntivirusScan.Status == vfs.AVStatusInfected) {
		return nil, errReportSnapshot
	}
	head := &couchdb.JSONDoc{}
	if err := couchdb.GetDocWithRevs(i, consts.Files, fileID, head); err != nil {
		return nil, err
	}
	if head.Rev() != file.Rev() {
		return nil, errReportSnapshot
	}
	if previous != nil && head.Rev() != previous.revision {
		// Only a direct clean successor can replace the revision used for the preview.
		if file.AntivirusScan == nil || file.AntivirusScan.Status != vfs.AVStatusClean || !reportHasParent(head, previous.revision) {
			return nil, errReportSnapshot
		}
	}
	if err := i.checkReportLineage(ctx, head, originRev); err != nil {
		return nil, err
	}

	in, err := fs.OpenFile(file)
	if err != nil {
		return nil, err
	}
	digest := sha256.New()
	src := &io.LimitedReader{R: reportReader{ctx, in}, N: file.ByteSize + 1}
	if consume != nil {
		err = consume(io.TeeReader(src, digest))
	}
	var remaining int64
	if err == nil {
		remaining, err = io.Copy(digest, src)
	}
	closeErr := in.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if (consume != nil && remaining != 0) || src.N != 1 {
		return nil, errReportSnapshot
	}
	actual, err := reportFingerprint(head.M, digest.Sum(nil))
	if err != nil {
		return nil, err
	}
	if actual != fingerprint {
		return nil, errReportSnapshot
	}
	current, err := fs.FileByID(fileID)
	if err != nil {
		return nil, err
	}
	if current.Rev() != file.Rev() {
		return nil, errReportSnapshot
	}
	if err := i.checkReportDirectory(dirID); err != nil {
		return nil, err
	}
	return &reportSnapshot{file: current, revision: current.Rev(), fingerprint: actual}, nil
}

func reportHasParent(head *couchdb.JSONDoc, parent string) bool {
	revs, _ := head.M["_revisions"].(map[string]interface{})
	start, _ := revs["start"].(float64)
	ids, _ := revs["ids"].([]interface{})
	if start < 2 || len(ids) < 2 {
		return false
	}
	headHash, headOK := ids[0].(string)
	parentHash, parentOK := ids[1].(string)
	return headOK && parentOK && head.Rev() == fmt.Sprintf("%d-%s", int(start), headHash) &&
		parent == fmt.Sprintf("%d-%s", int(start)-1, parentHash)
}

// Allow antivirus-only changes. Compaction may remove old revision bodies, so
// still require the original revision in the lineage and the head fingerprint.
func (i *Instance) checkReportLineage(ctx context.Context, head *couchdb.JSONDoc, originRev string) error {
	if head.Rev() == originRev {
		return nil
	}
	revs, _ := head.M["_revisions"].(map[string]interface{})
	start, _ := revs["start"].(float64)
	ids, _ := revs["ids"].([]interface{})
	stableHead, err := reportFingerprint(head.M, nil)
	if err != nil {
		return err
	}
	for pos, hash := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		hash, ok := hash.(string)
		if !ok {
			return errReportSnapshot
		}
		rev := fmt.Sprintf("%d-%s", int(start)-pos, hash)
		if pos == 0 {
			if rev != head.Rev() {
				return errReportSnapshot
			}
			continue
		}
		doc := &couchdb.JSONDoc{}
		if err := couchdb.GetDocRev(i, consts.Files, head.ID(), rev, doc); err == nil {
			stable, err := reportFingerprint(doc.M, nil)
			if err != nil {
				return err
			}
			if stable != stableHead {
				return errReportSnapshot
			}
		} else if !couchdb.IsNotFoundError(err) || couchdb.IsDeletedError(err) {
			return err
		}
		if rev == originRev {
			return nil
		}
	}
	return errReportSnapshot
}

func reportFingerprint(doc interface{}, digest []byte) (string, error) {
	encoded, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	var stable map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&stable); err != nil {
		return "", err
	}
	delete(stable, "_rev")
	delete(stable, "_revisions")
	delete(stable, "antivirus_scan")
	metadata, err := json.Marshal(stable)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, _ = hash.Write(metadata)
	_, _ = hash.Write(digest)
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}
