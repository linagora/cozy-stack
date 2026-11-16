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

// ReportsDirectoryID reads the server-owned binding without creating or adopting
// a directory. File references and paths are not authority for privileged reports.
func (i *Instance) ReportsDirectoryID() (string, error) {
	var binding couchdb.JSONDoc
	if err := couchdb.GetDoc(i, consts.PublicLinkReports, reportsDirectoryBindingID, &binding); err != nil {
		return "", err
	}
	id, _ := binding.M["directory_id"].(string)
	if id == "" || id == consts.RootDirID || id == consts.TrashDirID {
		return "", errors.New("invalid Reports directory binding")
	}
	dir, err := i.VFS().DirByID(id)
	if err != nil {
		return "", err
	}
	if dir.Fullpath == vfs.TrashDirName || strings.HasPrefix(dir.Fullpath, vfs.TrashDirName+"/") {
		return "", vfs.ErrFileInTrash
	}
	return id, nil
}

func (i *Instance) resolveReportsDirectoryID() (string, error) {
	mu := config.Lock().LongOperation(i, "reports-directory")
	if err := mu.Lock(); err != nil {
		return "", err
	}
	defer mu.Unlock()

	id, err := i.ReportsDirectoryID()
	if err == nil || couchdb.IsDeletedError(err) || !couchdb.IsNotFoundError(err) {
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
		dir.AddReferencedBy(couchdb.DocReference{Type: consts.Apps, ID: consts.Apps + "/reports"})
		dir.CozyMetadata = vfs.NewCozyMetadata(i.PageURL("/", nil))
		if err = fs.CreateDir(dir); errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		binding := &couchdb.JSONDoc{Type: consts.PublicLinkReports, M: map[string]interface{}{
			"_id": reportsDirectoryBindingID, "directory_id": dir.ID(),
		}}
		// CAS remains authoritative if the distributed lock expires. An empty
		// losing directory must not be adopted or deleted after another writer wins.
		if err = couchdb.CreateNamedDocWithDB(i, binding); err != nil && !couchdb.IsConflictError(err) {
			return "", err
		}
		return i.ReportsDirectoryID()
	}
}
