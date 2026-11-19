package vfss3

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/minio/minio-go/v7"
)

// deleteIndexedObjects removes the objects of the instance whose keys can be
// derived from CouchDB, without listing the bucket. It must run before the
// CouchDB databases of the instance are deleted. Objects that CouchDB no
// longer references are left behind.
func (sfs *s3VFS) deleteIndexedObjects() error {
	sfs.log.Infof("Deleting the objects indexed in CouchDB with prefix %q in bucket %q", sfs.keyPrefix, sfs.bucket)
	objectsCh := make(chan minio.ObjectInfo)
	var walkErr error
	go func() {
		defer close(objectsCh)
		walkErr = sfs.walkObjectKeys(func(key string) {
			objectsCh <- minio.ObjectInfo{Key: key}
		})
	}()
	// A failed batch reports every key: keep the first error and a count.
	var first error
	failed := 0
	for e := range sfs.client.RemoveObjects(sfs.ctx, sfs.bucket, objectsCh, minio.RemoveObjectsOptions{}) {
		if e.Err == nil || minio.ToErrorResponse(e.Err).Code == "NoSuchKey" {
			continue
		}
		if first == nil {
			first = e.Err
		}
		failed++
	}
	if first != nil {
		first = fmt.Errorf("vfss3: %d objects not deleted: %w", failed, first)
	}
	return errors.Join(walkErr, first)
}

// walkObjectKeys pushes the key of every object the stack may have written
// under the instance prefix: the avatar, the content and thumbnails of each
// file, the old versions, the note images, and the objects and thumbnails of
// the trash-files jobs that have not been done yet.
func (sfs *s3VFS) walkObjectKeys(push func(key string)) error {
	thumbs := &thumbsS3{keyPrefix: sfs.keyPrefix}
	push((&avatarS3{keyPrefix: sfs.keyPrefix}).avatarKey())

	err := sfs.foreach(consts.Files, func(id string, data json.RawMessage) error {
		var doc struct {
			Type       string `json:"type"`
			InternalID string `json:"internal_vfs_id"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			return err
		}
		if doc.Type != consts.FileType {
			return nil
		}
		push(MakeObjectKey(sfs.keyPrefix, id, doc.InternalID))
		// The thumbnail route can generate thumbnails for any class.
		for _, format := range vfs.ThumbnailFormatNames {
			push(thumbs.makeName(id, format))
		}
		return nil
	})
	if err != nil {
		return err
	}

	err = sfs.foreach(consts.FilesVersions, func(id string, _ json.RawMessage) error {
		if parts := strings.SplitN(id, "/", 2); len(parts) == 2 {
			push(MakeObjectKey(sfs.keyPrefix, parts[0], parts[1]))
		}
		return nil
	})
	if err != nil {
		return err
	}

	err = sfs.foreach(consts.NotesImages, func(id string, _ json.RawMessage) error {
		push(thumbs.makeName(id, consts.NoteImageOriginalFormat))
		push(thumbs.makeName(id, consts.NoteImageThumbFormat))
		return nil
	})
	if err != nil {
		return err
	}

	return sfs.foreach(consts.Jobs, func(_ string, data json.RawMessage) error {
		var j struct {
			Worker  string          `json:"worker"`
			State   string          `json:"state"`
			Message json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(data, &j); err != nil {
			return err
		}
		if j.Worker != "trash-files" || j.State == "done" {
			return nil
		}
		var journal vfs.TrashJournal
		if err := json.Unmarshal(j.Message, &journal); err != nil {
			return err
		}
		for _, key := range journal.ObjectNames {
			// The bucket is shared by all the instances.
			if strings.HasPrefix(key, sfs.keyPrefix) {
				push(key)
			}
		}
		for _, id := range journal.FileIDs {
			for _, format := range vfs.ThumbnailFormatNames {
				push(thumbs.makeName(id, format))
			}
		}
		return nil
	})
}

func (sfs *s3VFS) foreach(doctype string, fn func(id string, data json.RawMessage) error) error {
	err := couchdb.ForeachDocs(sfs, doctype, fn)
	if couchdb.IsNoDatabaseError(err) {
		return nil
	}
	return err
}
