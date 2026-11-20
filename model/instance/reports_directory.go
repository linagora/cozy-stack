package instance

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

const reportsDirectoryBindingID = "reports-directory"

// reportsDirectory reads the private binding. Paths and file references are
// user-controlled and cannot designate the destination of a privileged write.
func (i *Instance) reportsDirectory() (string, error) {
	var binding couchdb.JSONDoc
	if err := couchdb.GetDoc(i, consts.PublicLinkReports, reportsDirectoryBindingID, &binding); err != nil {
		return "", err
	}
	id, _ := binding.M["directory_id"].(string)
	if id == "" || id == consts.RootDirID || id == consts.TrashDirID {
		return "", errors.New("invalid Reports directory binding")
	}
	// Check the actual ancestry too: a stored path can be stale while a
	// directory move is being applied.
	fs := i.VFS()
	seen := make(map[string]struct{})
	for parentID := id; parentID != consts.RootDirID; {
		if _, ok := seen[parentID]; ok {
			return "", errReportSnapshot
		}
		seen[parentID] = struct{}{}
		dir, err := fs.DirByID(parentID)
		if err != nil {
			return "", err
		}
		if dir.ID() == consts.TrashDirID || dir.Fullpath == vfs.TrashDirName || strings.HasPrefix(dir.Fullpath, vfs.TrashDirName+"/") {
			return "", vfs.ErrFileInTrash
		}
		parentID = dir.DirID
	}
	return id, nil
}

func (i *Instance) ensureReportsDirectory() (string, error) {
	id, err := i.reportsDirectory()
	if !couchdb.IsNotFoundError(err) || couchdb.IsDeletedError(err) {
		return id, err
	}
	mu := config.Lock().LongOperation(i, "reports-directory")
	if err := mu.Lock(); err != nil {
		return "", err
	}
	defer mu.Unlock()

	id, err = i.reportsDirectory()
	if !couchdb.IsNotFoundError(err) || couchdb.IsDeletedError(err) {
		return id, err
	}

	fs := i.VFS()
	name := i.Translate("Tree Reports")
	for suffix := 0; ; suffix++ {
		dirname := name
		if suffix > 0 {
			dirname = fmt.Sprintf("%s (%d)", name, suffix)
		}
		dir, err := vfs.NewDirDocWithPath(dirname, consts.RootDirID, "/", nil)
		if err != nil {
			return "", err
		}
		dir.CozyMetadata = vfs.NewCozyMetadata(i.PageURL("/", nil))
		if err := fs.CreateDir(dir); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return "", err
		}
		binding := &couchdb.JSONDoc{Type: consts.PublicLinkReports, M: map[string]interface{}{
			"_id": reportsDirectoryBindingID, "directory_id": dir.ID(),
		}}
		if err = couchdb.CreateNamedDocWithDB(i, binding); err != nil && !couchdb.IsConflictError(err) {
			return "", err
		}
		return i.reportsDirectory()
	}
}
