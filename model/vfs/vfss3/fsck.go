package vfss3

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"strings"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/minio/minio-go/v7"
)

func (sfs *s3VFS) Fsck(accumulate func(log *vfs.FsckLog), failFast bool) error {
	entries := make(map[string]*vfs.TreeFile, 1024)
	tree, err := sfs.BuildTree(func(f *vfs.TreeFile) {
		if !f.IsDir {
			entries[f.DocID+"/"+f.InternalID] = f
		}
	})
	if err != nil {
		return err
	}
	if err = sfs.CheckTreeIntegrity(tree, accumulate, failFast); err != nil {
		if errors.Is(err, vfs.ErrFsckFailFast) {
			return nil
		}
		return err
	}
	return sfs.checkFiles(entries, accumulate, failFast)
}

func (sfs *s3VFS) CheckFilesConsistency(accumulate func(log *vfs.FsckLog), failFast bool) error {
	entries := make(map[string]*vfs.TreeFile, 1024)
	_, err := sfs.BuildTree(func(f *vfs.TreeFile) {
		if !f.IsDir {
			entries[f.DocID+"/"+f.InternalID] = f
		}
	})
	if err != nil {
		return err
	}
	return sfs.checkFiles(entries, accumulate, failFast)
}

func (sfs *s3VFS) checkFiles(
	entries map[string]*vfs.TreeFile,
	accumulate func(log *vfs.FsckLog),
	failFast bool,
) error {
	versions := make(map[string]*vfs.Version, 1024)
	err := couchdb.ForeachDocs(sfs, consts.FilesVersions, func(_ string, data json.RawMessage) error {
		v := &vfs.Version{}
		if erru := json.Unmarshal(data, v); erru != nil {
			return erru
		}
		versions[v.DocID] = v
		return nil
	})
	if err != nil {
		return err
	}

	images := make(map[string]struct{})
	err = couchdb.ForeachDocs(sfs, consts.NotesImages, func(_ string, data json.RawMessage) error {
		img := make(map[string]interface{})
		if erru := json.Unmarshal(data, &img); erru != nil {
			return erru
		}
		id, _ := img["_id"].(string)
		images[id] = struct{}{}
		return nil
	})
	if err != nil && !couchdb.IsNoDatabaseError(err) {
		return err
	}

	fileIDs := make(map[string]struct{}, len(entries))
	for _, f := range entries {
		fileIDs[f.DocID] = struct{}{}
	}

	// List all objects under our key prefix
	for obj := range sfs.client.ListObjects(sfs.ctx, sfs.bucket, minio.ListObjectsOptions{
		Prefix:    sfs.keyPrefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return obj.Err
		}

		// Strip the key prefix to get the object name
		objName := strings.TrimPrefix(obj.Key, sfs.keyPrefix)

		if objName == "avatar" {
			continue
		}
		if strings.HasPrefix(objName, "thumbs/") {
			thumbName := strings.TrimPrefix(objName, "thumbs/")
			idx := strings.LastIndex(thumbName, "-")
			if idx < 0 {
				continue
			}
			thumbName = thumbName[0:idx] // Remove -format suffix
			fileID, _ := makeDocID(thumbName)
			if _, ok := fileIDs[fileID]; !ok {
				if _, ok := images[fileID]; !ok {
					accumulate(&vfs.FsckLog{
						Type:   vfs.ThumbnailWithNoFile,
						IsFile: true,
						FileDoc: &vfs.TreeFile{
							DirOrFileDoc: vfs.DirOrFileDoc{
								DirDoc: &vfs.DirDoc{
									Type:    consts.FileType,
									DocID:   fileID,
									DocName: objName,
								},
							},
						},
					})
					if failFast {
						return nil
					}
				}
			}
			continue
		}

		docID, internalID := makeDocID(objName)
		if v, ok := versions[docID+"/"+internalID]; ok {
			// ETag from S3 may or may not be an MD5 (multipart uploads use composite ETags).
			etag := strings.Trim(obj.ETag, "\"")
			if !strings.Contains(etag, "-") {
				md5sum, err := hex.DecodeString(etag)
				if err == nil {
					if !bytes.Equal(md5sum, v.MD5Sum) || v.ByteSize != obj.Size {
						accumulate(&vfs.FsckLog{
							Type:       vfs.ContentMismatch,
							IsVersion:  true,
							VersionDoc: v,
							ContentMismatch: &vfs.FsckContentMismatch{
								SizeFile:    obj.Size,
								SizeIndex:   v.ByteSize,
								MD5SumFile:  md5sum,
								MD5SumIndex: v.MD5Sum,
							},
						})
						if failFast {
							return nil
						}
					}
				}
			}
			delete(versions, v.DocID)
			continue
		}
		f, ok := entries[docID+"/"+internalID]
		if !ok {
			accumulate(&vfs.FsckLog{
				Type:    vfs.IndexMissing,
				IsFile:  true,
				FileDoc: objectToFileDoc(obj),
			})
			if failFast {
				return nil
			}
		} else {
			etag := strings.Trim(obj.ETag, "\"")
			if !strings.Contains(etag, "-") {
				md5sum, err := hex.DecodeString(etag)
				if err == nil {
					if !bytes.Equal(md5sum, f.MD5Sum) || f.ByteSize != obj.Size {
						accumulate(&vfs.FsckLog{
							Type:    vfs.ContentMismatch,
							IsFile:  true,
							FileDoc: f,
							ContentMismatch: &vfs.FsckContentMismatch{
								SizeFile:    obj.Size,
								SizeIndex:   f.ByteSize,
								MD5SumFile:  md5sum,
								MD5SumIndex: f.MD5Sum,
							},
						})
						if failFast {
							return nil
						}
					}
				}
			}
			delete(entries, docID+"/"+internalID)
		}
	}

	// entries should contain only data that does not contain an associated
	// object in S3.
	for _, f := range entries {
		accumulate(&vfs.FsckLog{
			Type:    vfs.FSMissing,
			IsFile:  true,
			FileDoc: f,
		})
		if failFast {
			return nil
		}
	}

	for _, v := range versions {
		accumulate(&vfs.FsckLog{
			Type:       vfs.FSMissing,
			IsVersion:  true,
			VersionDoc: v,
		})
		if failFast {
			return nil
		}
	}

	return nil
}

func objectToFileDoc(obj minio.ObjectInfo) *vfs.TreeFile {
	md5sum, _ := hex.DecodeString(strings.Trim(obj.ETag, "\""))
	name := "unknown"
	mime, class := vfs.ExtractMimeAndClass(obj.ContentType)
	// Strip any key prefix — we need to find the object name portion
	// which is just the last segments of the key.
	objName := obj.Key
	if idx := strings.Index(objName, "/"); idx >= 0 {
		// The first segment is the key prefix (db prefix); skip it
		objName = objName[idx+1:]
	}
	fileID, internalID := makeDocID(objName)
	return &vfs.TreeFile{
		DirOrFileDoc: vfs.DirOrFileDoc{
			DirDoc: &vfs.DirDoc{
				Type:      consts.FileType,
				DocID:     fileID,
				DocName:   name,
				DirID:     "",
				CreatedAt: obj.LastModified,
				UpdatedAt: obj.LastModified,
				Fullpath:  path.Join(vfs.OrphansDirName, name),
			},
			ByteSize:   obj.Size,
			Mime:       mime,
			Class:      class,
			MD5Sum:     md5sum,
			InternalID: internalID,
		},
	}
}
