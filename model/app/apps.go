package app

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/model/vfs/vfsafero"
	"github.com/cozy/cozy-stack/pkg/appfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/spf13/afero"
)

const (
	// ManifestMaxSize is the manifest maximum size
	ManifestMaxSize = 2 << (2 * 10) // 2MB
	// WebappManifestName is the name of the manifest at the root of the
	// client-side application directory
	WebappManifestName = "manifest.webapp"
	// KonnectorManifestName is the name of the manifest at the root of the
	// konnector application directory
	KonnectorManifestName = "manifest.konnector"
)

// State is the state of the application
type State string

const (
	// Installing state
	Installing = "installing"
	// Upgrading state
	Upgrading = "upgrading"
	// Installed state, can be used to state that an application has been
	// installed but needs a user interaction to be activated and "ready".
	Installed = "installed"
	// Ready state
	Ready = "ready"
	// Errored state
	Errored = "errored"
)

// KonnectorArchiveName is the name of the archive created to store the
// konnectors sources.
const KonnectorArchiveName = "app.tar"

var (
	ErrInvalidAppType = errors.New("invalid app type")
)

// SubDomainer is an interface with a single method to build an URL from a slug
type SubDomainer interface {
	SubDomain(s string) *url.URL
}

// Manifest interface is used by installer to encapsulate the manifest metadata
// that can represent either a webapp or konnector manifest
type Manifest interface {
	couchdb.Doc
	Fetch(field string) []string
	ReadManifest(i io.Reader, slug, sourceURL string) (Manifest, error)

	Create(db prefixer.Prefixer) error
	Update(db prefixer.Prefixer, extraPerms permission.Set) error
	Delete(db prefixer.Prefixer) error

	AppType() consts.AppType
	Permissions() permission.Set
	Source() string
	Version() string
	AvailableVersion() string
	Checksum() string
	Slug() string
	State() State
	LastUpdate() time.Time
	Terms() Terms

	Name() string
	Icon() string
	Notifications() Notifications

	SetError(err error)
	Error() error

	SetSlug(slug string)
	SetSource(src *url.URL)
	SetState(state State)
	SetVersion(version string)
	SetAvailableVersion(version string)
	SetChecksum(shasum string)
}

// GetBySlug returns an app manifest identified by its slug
func GetBySlug(db prefixer.Prefixer, slug string, appType consts.AppType) (Manifest, error) {
	var man Manifest
	var err error
	switch appType {
	case consts.WebappType:
		man, err = GetWebappBySlug(db, slug)
	case consts.KonnectorType:
		man, err = GetKonnectorBySlug(db, slug)
	default:
		return nil, fmt.Errorf("%s, %w", appType, ErrInvalidAppType)
	}
	if err != nil {
		return nil, err
	}
	return man, nil
}

func routeMatches(path, ctx []string) bool {
	for i, part := range ctx {
		if path[i] != part {
			return false
		}
	}
	return true
}

// UpgradeInstalledState is used to force the legacy "installed" state to
// "ready" for a webapp or a konnector manifest
func UpgradeInstalledState(inst *instance.Instance, man Manifest) error {
	if man.State() == Installed {
		man.SetState(Ready)
		return man.Update(inst, nil)
	}
	return nil
}

// Copier returns the application copier associated with the specified
// application type
func Copier(appsType consts.AppType, inst *instance.Instance) appfs.Copier {
	fsURL := config.FsURL()
	switch fsURL.Scheme {
	case config.SchemeFile:
		var baseDirName string
		switch appsType {
		case consts.WebappType:
			baseDirName = vfs.WebappsDirName
		case consts.KonnectorType:
			baseDirName = vfs.KonnectorsDirName
		}
		baseFS := afero.NewBasePathFs(afero.NewOsFs(),
			path.Join(fsURL.Path, inst.DirName(), baseDirName))
		return appfs.NewAferoCopier(baseFS)
	case config.SchemeMem:
		baseFS := vfsafero.GetMemFS("apps")
		return appfs.NewAferoCopier(baseFS)
	case config.SchemeSwift, config.SchemeSwiftSecure:
		return appfs.NewSwiftCopier(config.GetSwiftConnection(), appsType)
	case config.SchemeS3:
		client := config.GetS3Client()
		bucket := appfs.S3AppsBucket(config.GetS3BucketPrefix(), appsType)
		return appfs.NewS3Copier(client, bucket)
	default:
		panic(fmt.Sprintf("instance: unknown storage provider %s", fsURL.Scheme))
	}
}

// AppsFileServer returns the web-application file server associated to this
// instance.
func AppsFileServer(i *instance.Instance) appfs.FileServer {
	fsURL := config.FsURL()
	switch fsURL.Scheme {
	case config.SchemeFile:
		baseFS := afero.NewBasePathFs(afero.NewOsFs(),
			path.Join(fsURL.Path, i.DirName(), vfs.WebappsDirName))
		return appfs.NewAferoFileServer(baseFS, nil)
	case config.SchemeMem:
		baseFS := vfsafero.GetMemFS("apps")
		return appfs.NewAferoFileServer(baseFS, nil)
	case config.SchemeSwift, config.SchemeSwiftSecure:
		return appfs.NewSwiftFileServer(config.GetSwiftConnection(), consts.WebappType)
	case config.SchemeS3:
		client := config.GetS3Client()
		bucket := appfs.S3AppsBucket(config.GetS3BucketPrefix(), consts.WebappType)
		return appfs.NewS3FileServer(client, bucket)
	default:
		panic(fmt.Sprintf("instance: unknown storage provider %s", fsURL.Scheme))
	}
}

// KonnectorsFileServer returns the web-application file server associated to this
// instance.
func KonnectorsFileServer(i *instance.Instance) appfs.FileServer {
	fsURL := config.FsURL()
	switch fsURL.Scheme {
	case config.SchemeFile:
		baseFS := afero.NewBasePathFs(afero.NewOsFs(),
			path.Join(fsURL.Path, i.DirName(), vfs.KonnectorsDirName))
		return appfs.NewAferoFileServer(baseFS, nil)
	case config.SchemeMem:
		baseFS := vfsafero.GetMemFS("apps")
		return appfs.NewAferoFileServer(baseFS, nil)
	case config.SchemeSwift, config.SchemeSwiftSecure:
		return appfs.NewSwiftFileServer(config.GetSwiftConnection(), consts.KonnectorType)
	case config.SchemeS3:
		client := config.GetS3Client()
		bucket := appfs.S3AppsBucket(config.GetS3BucketPrefix(), consts.KonnectorType)
		return appfs.NewS3FileServer(client, bucket)
	default:
		panic(fmt.Sprintf("instance: unknown storage provider %s", fsURL.Scheme))
	}
}
