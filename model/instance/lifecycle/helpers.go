package lifecycle

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/utils"
	multierror "github.com/hashicorp/go-multierror"
	"golang.org/x/net/idna"
	"golang.org/x/sync/errgroup"
)

func update(inst *instance.Instance) error {
	if err := instance.Update(inst); err != nil {
		inst.Logger().Errorf("Could not update: %s", err.Error())
		return err
	}
	return nil
}

func installApp(inst *instance.Instance, slug string) error {
	source := "registry://" + slug + "/stable"
	installer, err := app.NewInstaller(inst, app.Copier(consts.WebappType, inst), &app.InstallerOptions{
		Operation:  app.Install,
		Type:       consts.WebappType,
		SourceURL:  source,
		Slug:       slug,
		Registries: inst.Registries(),
	})
	if err != nil {
		return err
	}
	_, err = installer.RunSync()
	return err
}

// DefineViewsAndIndex can be used to ensure that the CouchDB views and indexes
// used by the stack are correctly set. It expects that most index/view don't
// exist. It is faster when creating a new instance for example.
func DefineViewsAndIndex(inst *instance.Instance) error {
	g, _ := errgroup.WithContext(context.Background())
	couchdb.DefineIndexes(g, inst, couchdb.Indexes)
	couchdb.DefineViews(g, inst, couchdb.Views)
	if err := g.Wait(); err != nil {
		return err
	}
	inst.IndexViewsVersion = couchdb.IndexViewsVersion
	return nil
}

// UpdateViewsAndIndex can be used to ensure that the CouchDB views and indexes
// used by the stack are correctly set. It has the same effect as
// DefineViewsAndIndex, but it expect most index/views already exist.
func UpdateViewsAndIndex(inst *instance.Instance) error {
	err := couchdb.UpdateIndexesAndViews(inst, couchdb.Indexes, couchdb.Views)
	if err != nil {
		return err
	}
	inst.IndexViewsVersion = couchdb.IndexViewsVersion
	return nil
}

func createDefaultFilesTree(inst *instance.Instance) error {
	var errf error
	fs := inst.VFS()

	createDir := func(dir *vfs.DirDoc, err error) {
		if err != nil {
			errf = multierror.Append(errf, err)
			return
		}
		dir.CozyMetadata = vfs.NewCozyMetadata(inst.PageURL("/", nil))
		err = fs.CreateDir(dir)
		if err != nil && !os.IsExist(err) {
			errf = multierror.Append(errf, err)
		}
	}

	// Check if we create the "Administrative" and "Photos" folders. By
	// default, we are creating it, but some contexts may not want to create them.
	createAdministrativeFolder := true
	createPhotosFolder := true
	if ctxSettings, ok := inst.SettingsContext(); ok {
		if administrativeFolderParam, ok := ctxSettings["init_administrative_folder"]; ok {
			createAdministrativeFolder = administrativeFolderParam.(bool)
		}
		if photosFolderParam, ok := ctxSettings["init_photos_folder"]; ok {
			createPhotosFolder = photosFolderParam.(bool)
		}
	}

	administrativeReference := couchdb.DocReference{
		ID:   "io.cozy.apps/administrative",
		Type: consts.Apps,
	}
	if createAdministrativeFolder {
		name := inst.Translate("Tree Administrative")
		administrativeFolder, err := vfs.NewDirDocWithPath(name, consts.RootDirID, "/", nil)
		if err == nil {
			administrativeFolder.AddReferencedBy(administrativeReference)
		}
		createDir(administrativeFolder, err)
	} else {
		root, err := fs.DirByID(consts.RootDirID)
		if err != nil {
			return err
		}
		olddoc := root.Clone().(*vfs.DirDoc)
		root.AddReferencedBy(administrativeReference)
		if err := fs.UpdateDirDoc(olddoc, root); err != nil {
			errf = multierror.Append(errf, err)
		}
	}

	// The Photos folder is referenced as io.cozy.apps/photos, the magic
	// folder that cozy-client and the flagship app look for.
	if createPhotosFolder {
		name := inst.Translate("Tree Photos")
		photosFolder, err := vfs.NewDirDocWithPath(name, consts.RootDirID, "/", nil)
		if err == nil {
			photosFolder.AddReferencedBy(couchdb.DocReference{
				ID:   "io.cozy.apps/photos",
				Type: consts.Apps,
			})
		}
		createDir(photosFolder, err)
	}

	return errf
}

func checkAliases(inst *instance.Instance, aliases []string) ([]string, error) {
	if len(aliases) == 0 {
		return nil, nil
	}
	aliases = utils.UniqueStrings(aliases)
	kept := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		alias, err := validateDomain(alias)
		if err != nil {
			return nil, err
		}
		if alias == inst.Domain {
			return nil, instance.ErrExists
		}
		other, err := instance.Get(alias)
		if !errors.Is(err, instance.ErrNotFound) {
			if err != nil {
				return nil, err
			}
			if other.ID() != inst.ID() {
				return nil, instance.ErrExists
			}
		}
		kept = append(kept, alias)
	}
	return kept, nil
}

const illegalChars = " /,;&?#@|='\"\t\r\n\x00"
const illegalFirstChars = "."

func validateDomain(domain string) (string, error) {
	var err error
	if domain, err = idna.ToUnicode(domain); err != nil {
		return "", instance.ErrIllegalDomain
	}
	domain = strings.TrimSpace(domain)
	if domain == "" || domain == ".." || domain == "." {
		return "", instance.ErrIllegalDomain
	}
	if strings.ContainsAny(domain, illegalChars) {
		return "", instance.ErrIllegalDomain
	}
	if strings.ContainsAny(domain[:1], illegalFirstChars) {
		return "", instance.ErrIllegalDomain
	}
	domain = strings.ToLower(domain)
	if config.GetConfig().Subdomains == config.FlatSubdomains {
		parts := strings.SplitN(domain, ".", 2)
		if strings.Contains(parts[0], "-") {
			return "", instance.ErrIllegalDomain
		}
	}
	return domain, nil
}
