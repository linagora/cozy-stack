package migrations

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/note"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/model/vfs/vfsswift"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/i18n"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/utils"
	multierror "github.com/hashicorp/go-multierror"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"

	"github.com/ncw/swift/v2"
)

const (
	swiftV1ToV2 = "swift-v1-to-v2"
	toSwiftV3   = "to-swift-v3"

	swiftV1ContainerPrefixCozy = "cozy-"
	swiftV1ContainerPrefixData = "data-"
	swiftV2ContainerPrefixCozy = "cozy-v2-"
	swiftV2ContainerPrefixData = "data-v2-"
	swiftV3ContainerPrefix     = "cozy-v3-"

	accountsToOrganization = "accounts-to-organization"
	notesMimeType          = "notes-mime-type"
	unwantedFolders        = "remove-unwanted-folders"
)

// maxSimultaneousCalls is the maximal number of simultaneous calls to Swift
// made by a single migration.
const maxSimultaneousCalls = 8

func init() {
	job.AddWorker(&job.WorkerConfig{
		WorkerType:   "migrations",
		Concurrency:  runtime.NumCPU(),
		MaxExecCount: 1,
		Reserved:     true,
		WorkerFunc:   worker,
		WorkerCommit: commit,
		Timeout:      6 * time.Hour,
	})
}

type message struct {
	Type string `json:"type"`
}

func worker(ctx *job.TaskContext) error {
	var msg message
	if err := ctx.UnmarshalMessage(&msg); err != nil {
		return err
	}

	logger.WithDomain(ctx.Instance.Domain).WithNamespace("migration").
		Infof("Start the migration %s", msg.Type)

	switch msg.Type {
	case toSwiftV3:
		return migrateToSwiftV3(ctx.Instance.Domain)
	case swiftV1ToV2:
		return fmt.Errorf("this migration type is no longer supported")
	case accountsToOrganization:
		return migrateAccountsToOrganization(ctx.Instance.Domain)
	case notesMimeType:
		return migrateNotesMimeType(ctx.Instance.Domain)
	case unwantedFolders:
		return removeUnwantedFolders(ctx.Instance.Domain)
	default:
		return fmt.Errorf("unknown migration type %q", msg.Type)
	}
}

func commit(ctx *job.TaskContext, err error) error {
	var msg message
	var migrationType string

	if msgerr := ctx.UnmarshalMessage(&msg); msgerr != nil {
		migrationType = ""
	} else {
		migrationType = msg.Type
	}

	log := logger.WithDomain(ctx.Instance.Domain).WithNamespace("migration")
	if err == nil {
		log.Infof("Migration %s success", migrationType)
	} else {
		log.Errorf("Migration %s error: %s", migrationType, err)
	}
	return err
}

func pushTrashJob(fs vfs.VFS) func(vfs.TrashJournal) error {
	return func(journal vfs.TrashJournal) error {
		return fs.EnsureErased(journal)
	}
}

func removeUnwantedFolders(domain string) error {
	inst, err := instance.Get(domain)
	if err != nil {
		return err
	}
	fs := inst.VFS()

	var errf error
	removeDir := func(dir *vfs.DirDoc, err error) {
		if errors.Is(err, os.ErrNotExist) {
			return
		} else if err != nil {
			errf = multierror.Append(errf, err)
			return
		}

		hasFiles := false
		err = vfs.WalkByID(fs, dir.ID(), func(name string, dir *vfs.DirDoc, file *vfs.FileDoc, err error) error {
			if err != nil {
				return err
			}
			if file != nil {
				hasFiles = true
			}
			return nil
		})
		if err != nil {
			errf = multierror.Append(errf, err)
			return
		}
		if hasFiles {
			// This is a guard to avoiding deleting files by mistake.
			return
		}
		push := pushTrashJob(fs)
		if err = fs.DestroyDirAndContent(dir, push); err != nil {
			errf = multierror.Append(errf, err)
		}
	}

	keepAdministrativeFolder := true
	keepPhotosFolder := true
	if ctxSettings, ok := inst.SettingsContext(); ok {
		if administrativeFolderParam, ok := ctxSettings["init_administrative_folder"]; ok {
			keepAdministrativeFolder = administrativeFolderParam.(bool)
		}
		if photosFolderParam, ok := ctxSettings["init_photos_folder"]; ok {
			keepPhotosFolder = photosFolderParam.(bool)
		}
	}

	if !keepPhotosFolder {
		name := inst.Translate("Tree Photos")
		folder, err := fs.DirByPath("/" + name)
		removeDir(folder, err)
	}

	if !keepAdministrativeFolder {
		name := inst.Translate("Tree Administrative")
		folder, err := fs.DirByPath("/" + name)
		if err != nil {
			name = i18n.Translate("Tree Administrative", consts.DefaultLocale, inst.ContextName)
			folder, err = fs.DirByPath("/" + name)
		}
		removeDir(folder, err)

		root, err := fs.DirByID(consts.RootDirID)
		if err != nil {
			return err
		}
		olddoc := root.Clone().(*vfs.DirDoc)
		was := len(root.ReferencedBy)
		root.AddReferencedBy(couchdb.DocReference{
			ID:   "io.cozy.apps/administrative",
			Type: consts.Apps,
		})
		if len(root.ReferencedBy) != was {
			if err := fs.UpdateDirDoc(olddoc, root); err != nil {
				errf = multierror.Append(errf, err)
			}
		}
	}

	return errf
}

func migrateNotesMimeType(domain string) error {
	inst, err := instance.Get(domain)
	if err != nil {
		return err
	}
	log := inst.Logger().WithNamespace("migration")

	var docs []*vfs.FileDoc
	req := &couchdb.FindRequest{
		UseIndex: "by-mime-updated-at",
		Selector: mango.And(
			mango.Equal("mime", "text/markdown"),
			mango.Exists("updated_at"),
		),
		Limit: 1000,
	}
	_, err = couchdb.FindDocsRaw(inst, consts.Files, req, &docs)
	if err != nil {
		return err
	}
	log.Infof("Found %d markdown files", len(docs))
	for _, doc := range docs {
		if _, ok := doc.Metadata["version"]; !ok {
			log.Infof("Skip file %#v", doc)
			continue
		}
		if err := note.Update(inst, doc.ID()); err != nil {
			log.Warnf("Cannot change mime-type for note %s: %s", doc.ID(), err)
		}
	}

	return nil
}

func migrateToSwiftV3(domain string) error {
	c := config.GetSwiftConnection()
	inst, err := instance.Get(domain)
	if err != nil {
		return err
	}
	log := inst.Logger().WithNamespace("migration")

	var srcContainer, migratedFrom string
	switch inst.SwiftLayout {
	case 0: // layout v1
		srcContainer = swiftV1ContainerPrefixCozy + inst.DBPrefix()
		migratedFrom = "v1"
	case 1: // layout v2
		srcContainer = swiftV2ContainerPrefixCozy + inst.DBPrefix()
		switch inst.DBPrefix() {
		case inst.Domain:
			migratedFrom = "v2a"
		case inst.Prefix:
			migratedFrom = "v2b"
		default:
			return instance.ErrInvalidSwiftLayout
		}
	case 2: // layout v3
		return nil // Nothing to do!
	default:
		return instance.ErrInvalidSwiftLayout
	}

	log.Infof("Migrating from swift layout %s to swift layout v3", migratedFrom)

	vfs := inst.VFS()
	root, err := vfs.DirByID(consts.RootDirID)
	if err != nil {
		return err
	}

	mutex := config.Lock().LongOperation(inst, "vfs")
	if err = mutex.Lock(); err != nil {
		return err
	}
	defer mutex.Unlock()

	ctx := context.Background()
	dstContainer := swiftV3ContainerPrefix + inst.DBPrefix()
	if _, _, err = c.Container(ctx, dstContainer); !errors.Is(err, swift.ContainerNotFound) {
		log.Errorf("Destination container %s already exists or something went wrong. Migration canceled.", dstContainer)
		return errors.New("Destination container busy")
	}
	if err = c.ContainerCreate(ctx, dstContainer, nil); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if err := vfsswift.DeleteContainer(ctx, c, dstContainer); err != nil {
				log.Errorf("Failed to delete v3 container %s: %s", dstContainer, err)
			}
		}
	}()

	if err = copyTheFilesToSwiftV3(inst, ctx, c, root, srcContainer, dstContainer); err != nil {
		return err
	}

	meta := &swift.Metadata{"cozy-migrated-from": migratedFrom}
	_ = c.ContainerUpdate(ctx, dstContainer, meta.ContainerHeaders())
	if in, err := instance.Get(domain); err == nil {
		inst = in
	}
	inst.SwiftLayout = 2
	if err = instance.Update(inst); err != nil {
		return err
	}

	// Migration done. Now clean-up oldies.

	// WARNING: Don't call `err` any error below in this function or the defer func
	//          will delete the new container even if the migration was successful

	if deleteErr := vfs.Delete(); deleteErr != nil {
		log.Errorf("Failed to delete old %s containers: %s", migratedFrom, deleteErr)
	}
	return nil
}

func copyTheFilesToSwiftV3(inst *instance.Instance, ctx context.Context, c *swift.Connection, root *vfs.DirDoc, src, dst string) error {
	log := logger.WithDomain(inst.Domain).
		WithNamespace("migration")
	sem := semaphore.NewWeighted(maxSimultaneousCalls)
	g, ctx := errgroup.WithContext(context.Background())
	fs := inst.VFS()

	var thumbsContainer string
	switch inst.SwiftLayout {
	case 0: // layout v1
		thumbsContainer = swiftV1ContainerPrefixData + inst.Domain
	case 1: // layout v2
		thumbsContainer = swiftV2ContainerPrefixData + inst.DBPrefix()
	default:
		return instance.ErrInvalidSwiftLayout
	}

	errw := vfs.WalkAlreadyLocked(fs, root, func(_ string, d *vfs.DirDoc, f *vfs.FileDoc, err error) error {
		if err != nil {
			return err
		}
		if f == nil {
			return nil
		}
		srcName := getSrcName(inst, f)
		dstName := getDstName(inst, f)
		if srcName == "" || dstName == "" {
			return fmt.Errorf("Unexpected copy: %q -> %q", srcName, dstName)
		}

		if err := sem.Acquire(ctx, 1); err != nil {
			return err
		}
		g.Go(func() error {
			defer sem.Release(1)
			err := utils.RetryWithExpBackoff(3, 200*time.Millisecond, func() error {
				_, err := c.ObjectCopy(ctx, src, srcName, dst, dstName, nil)
				return err
			})
			if err != nil {
				log.Warnf("Cannot copy file from %s %s to %s %s: %s",
					src, srcName, dst, dstName, err)
			}
			return err
		})

		// Copy the thumbnails
		if f.Class == "image" {
			srcTiny, srcSmall, srcMedium, srcLarge := getThumbsSrcNames(inst, f)
			dstTiny, dstSmall, dstMedium, dstLarge := getThumbsDstNames(inst, f)
			if err := sem.Acquire(ctx, 1); err != nil {
				return err
			}
			g.Go(func() error {
				defer sem.Release(1)
				_, err := c.ObjectCopy(ctx, thumbsContainer, srcSmall, dst, dstSmall, nil)
				if err != nil {
					log.Infof("Cannot copy thumbnail tiny from %s %s to %s %s: %s",
						thumbsContainer, srcTiny, dst, dstTiny, err)
				}
				_, err = c.ObjectCopy(ctx, thumbsContainer, srcSmall, dst, dstSmall, nil)
				if err != nil {
					log.Infof("Cannot copy thumbnail small from %s %s to %s %s: %s",
						thumbsContainer, srcSmall, dst, dstSmall, err)
				}
				_, err = c.ObjectCopy(ctx, thumbsContainer, srcMedium, dst, dstMedium, nil)
				if err != nil {
					log.Infof("Cannot copy thumbnail medium from %s %s to %s %s: %s",
						thumbsContainer, srcMedium, dst, dstMedium, err)
				}
				_, err = c.ObjectCopy(ctx, thumbsContainer, srcLarge, dst, dstLarge, nil)
				if err != nil {
					log.Infof("Cannot copy thumbnail large from %s %s to %s %s: %s",
						thumbsContainer, srcLarge, dst, dstLarge, err)
				}
				return nil
			})
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		return err
	}
	return errw
}

func getSrcName(inst *instance.Instance, f *vfs.FileDoc) string {
	srcName := ""
	switch inst.SwiftLayout {
	case 0: // layout v1
		srcName = f.DirID + "/" + f.DocName
	case 1: // layout v2
		srcName = vfsswift.MakeObjectName(f.DocID)
	}
	return srcName
}

// XXX the f FileDoc can be modified to add an InternalID
func getDstName(inst *instance.Instance, f *vfs.FileDoc) string {
	if f.InternalID == "" {
		old := f.Clone().(*vfs.FileDoc)
		f.InternalID = vfsswift.NewInternalID()
		if err := couchdb.UpdateDocWithOld(inst, f, old); err != nil {
			return ""
		}
	}
	return vfsswift.MakeObjectNameV3(f.DocID, f.InternalID)
}

func getThumbsSrcNames(inst *instance.Instance, f *vfs.FileDoc) (string, string, string, string) {
	var tiny, small, medium, large string
	switch inst.SwiftLayout {
	case 0: // layout v1
		tiny = fmt.Sprintf("thumbs/%s-tiny", f.DocID)
		small = fmt.Sprintf("thumbs/%s-small", f.DocID)
		medium = fmt.Sprintf("thumbs/%s-medium", f.DocID)
		large = fmt.Sprintf("thumbs/%s-large", f.DocID)
	case 1: // layout v2
		obj := vfsswift.MakeObjectName(f.DocID)
		tiny = fmt.Sprintf("thumbs/%s-tiny", obj)
		small = fmt.Sprintf("thumbs/%s-small", obj)
		medium = fmt.Sprintf("thumbs/%s-medium", obj)
		large = fmt.Sprintf("thumbs/%s-large", obj)
	}
	return tiny, small, medium, large
}

func getThumbsDstNames(inst *instance.Instance, f *vfs.FileDoc) (string, string, string, string) {
	obj := vfsswift.MakeObjectName(f.DocID)
	tiny := fmt.Sprintf("thumbs/%s-tiny", obj)
	small := fmt.Sprintf("thumbs/%s-small", obj)
	medium := fmt.Sprintf("thumbs/%s-medium", obj)
	large := fmt.Sprintf("thumbs/%s-large", obj)
	return tiny, small, medium, large
}
