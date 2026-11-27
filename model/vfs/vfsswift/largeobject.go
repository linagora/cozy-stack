package vfsswift

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	if !isLargeObjectName(objName) {
		return sfs.c.ObjectDelete(sfs.ctx, sfs.container, objName)
	}
	// Swift removes the segments of a manifest itself, even when the bulk
	// delete is refused (LargeObjectDelete then ignores the error). The answer
	// is 200 with the errors in its body.
	if !sfs.c.Authenticated() {
		if err := sfs.c.Authenticate(sfs.ctx); err != nil {
			return err
		}
	}
	resp, _, err := sfs.c.Call(sfs.ctx, sfs.c.StorageUrl, swift.RequestOpts{
		Container:  sfs.container,
		ObjectName: objName,
		Operation:  "DELETE",
		Parameters: url.Values{"multipart-manifest": {"delete"}},
		Headers:    swift.Headers{"Accept": "application/json"},
		OnReAuth:   func() (string, error) { return sfs.c.StorageUrl, nil },
	})
	var serr *swift.Error
	if errors.As(err, &serr) && serr.StatusCode == http.StatusNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || len(body) == 0 {
		return err
	}
	var res struct {
		Status string     `json:"Response Status"`
		Errors [][]string `json:"Errors"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return fmt.Errorf("swift: cannot read the deletion of %s: %w", objName, err)
	}
	for _, e := range res.Errors {
		// A copy of a manifest is a plain object, under the same kind of name.
		if len(e) == 2 && strings.Contains(e[1], "Not an SLO manifest") {
			err := sfs.c.ObjectDelete(sfs.ctx, sfs.container, objName)
			if errors.Is(err, swift.ObjectNotFound) {
				return nil
			}
			return err
		}
	}
	if len(res.Errors) > 0 || !strings.HasPrefix(res.Status, "200") {
		return fmt.Errorf("swift: cannot delete %s: %s %v", objName, res.Status, res.Errors)
	}
	return nil
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
