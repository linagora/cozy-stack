package vfsswift

import (
	"errors"
	"path"
	"strings"

	"github.com/ncw/swift/v2"
)

// largeObjectPrefix starts the internal ID of the files created by a resumable
// upload. Their object can be a SLO manifest, and must be deleted with its
// segments. Internal IDs are made of letters only, so the prefix cannot
// collide with them.
const largeObjectPrefix = "slo-"

func isLargeObjectName(objName string) bool {
	return strings.HasPrefix(path.Base(objName), largeObjectPrefix)
}

// deleteObject removes an object, and its segments for a SLO manifest.
func (sfs *swiftVFSV3) deleteObject(objName string) error {
	if isLargeObjectName(objName) {
		err := sfs.c.LargeObjectDelete(sfs.ctx, sfs.container, objName)
		if errors.Is(err, swift.ObjectNotFound) {
			return nil
		}
		return err
	}
	return sfs.c.ObjectDelete(sfs.ctx, sfs.container, objName)
}

// deleteLargeObjects removes the objects that can be SLO manifests, with their
// segments, and returns the other objects, for a bulk delete.
func (sfs *swiftVFSV3) deleteLargeObjects(objNames []string) ([]string, error) {
	var others []string
	var errm error
	for _, objName := range objNames {
		if !isLargeObjectName(objName) {
			others = append(others, objName)
			continue
		}
		if err := sfs.deleteObject(objName); err != nil {
			errm = errors.Join(errm, err)
		}
	}
	return others, errm
}
