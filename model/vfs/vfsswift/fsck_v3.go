package vfsswift

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"strings"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/ncw/swift/v2"
)

func (sfs *swiftVFSV3) Fsck(accumulate func(log *vfs.FsckLog), failFast bool) error {
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

func (sfs *swiftVFSV3) CheckFilesConsistency(accumulate func(log *vfs.FsckLog), failFast bool) error {
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

func (sfs *swiftVFSV3) checkFiles(
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

	opts := &swift.ObjectsOpts{Limit: 5_000}
	err = sfs.c.ObjectsWalk(sfs.ctx, sfs.container, opts, func(ctx context.Context, opts *swift.ObjectsOpts) (interface{}, error) {
		objs, err := sfs.c.Objects(sfs.ctx, sfs.container, opts)
		if err != nil {
			return nil, err
		}
		for _, obj := range objs {
			if obj.Name == "avatar" || strings.HasPrefix(obj.Name, vfs.UploadsObjectPrefix) {
				continue
			}
			if strings.HasPrefix(obj.Name, "thumbs/") {
				objName := strings.TrimPrefix(obj.Name, "thumbs/")
				idx := strings.LastIndex(objName, "-")
				objName = objName[0:idx] // Remove -format suffix
				fileID := makeDocID(objName)
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
										DocName: obj.Name,
									},
								},
							},
						})
						if failFast {
							return nil, errFailFast
						}
					}
				}
				continue
			}
			docID, internalID := makeDocIDV3(obj.Name)
			if v, ok := versions[docID+"/"+internalID]; ok {
				var md5sum []byte
				md5sum, err = hex.DecodeString(obj.Hash)
				if err != nil {
					return nil, err
				}
				if !sameContent(obj, md5sum, v.MD5Sum, v.ByteSize) {
					accumulate(&vfs.FsckLog{
						Type:       vfs.ContentMismatch,
						IsVersion:  true,
						VersionDoc: v,
						ContentMismatch: &vfs.FsckContentMismatch{
							SizeFile:    obj.Bytes,
							SizeIndex:   v.ByteSize,
							MD5SumFile:  md5sum,
							MD5SumIndex: v.MD5Sum,
						},
					})
					if failFast {
						return nil, errFailFast
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
					FileDoc: objectToFileDocV3(sfs.container, obj),
				})
				if failFast {
					return nil, errFailFast
				}
			} else {
				var md5sum []byte
				md5sum, err = hex.DecodeString(obj.Hash)
				if err != nil {
					return nil, err
				}
				if !sameContent(obj, md5sum, f.MD5Sum, f.ByteSize) {
					accumulate(&vfs.FsckLog{
						Type:    vfs.ContentMismatch,
						IsFile:  true,
						FileDoc: f,
						ContentMismatch: &vfs.FsckContentMismatch{
							SizeFile:    obj.Bytes,
							SizeIndex:   f.ByteSize,
							MD5SumFile:  md5sum,
							MD5SumIndex: f.MD5Sum,
						},
					})
					if failFast {
						return nil, errFailFast
					}
				}
				delete(entries, docID+"/"+internalID)
			}
		}
		return objs, nil
	})
	if err != nil {
		if errors.Is(err, errFailFast) {
			return nil
		}
		return err
	}

	// entries should contain only data that does not contain an associated
	// index.
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

// sameContent compares an object of the listing with the index. The hash of a
// SLO manifest in the listing is not the md5 of the content: only the size is
// compared for the objects of the resumable uploads.
func sameContent(obj swift.Object, md5sum, indexMD5 []byte, indexSize int64) bool {
	if obj.Bytes != indexSize {
		return false
	}
	return isLargeObjectName(obj.Name) || bytes.Equal(md5sum, indexMD5)
}

func objectToFileDocV3(container string, object swift.Object) *vfs.TreeFile {
	md5sum, _ := hex.DecodeString(object.Hash)
	name := "unknown"
	mime, class := vfs.ExtractMimeAndClass(object.ContentType)
	fileID, internalID := makeDocIDV3(object.Name)
	return &vfs.TreeFile{
		DirOrFileDoc: vfs.DirOrFileDoc{
			DirDoc: &vfs.DirDoc{
				Type:      consts.FileType,
				DocID:     fileID,
				DocName:   name,
				DirID:     "",
				CreatedAt: object.LastModified,
				UpdatedAt: object.LastModified,
				Fullpath:  path.Join(vfs.OrphansDirName, name),
			},
			ByteSize:   object.Bytes,
			Mime:       mime,
			Class:      class,
			MD5Sum:     md5sum,
			InternalID: internalID,
		},
	}
}
