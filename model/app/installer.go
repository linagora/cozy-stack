package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	semver "github.com/Masterminds/semver/v3"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/appfs"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/cozy/cozy-stack/pkg/registry"
	"github.com/cozy/cozy-stack/pkg/utils"
)

var slugReg = regexp.MustCompile(`^[a-z0-9\-]+$`)

var ErrInvalidManifestTypes = errors.New("Manifest type is unknown")
var ErrInvalidManifestForWebapp = errors.New("Manifest type is not valid for a webapp. Maybe you want to install a konnector?")
var ErrInvalidManifestForKonnector = errors.New("Manifest type is not valid for a konnector. Maybe you want to install a webapp?")

// Operation is the type of operation the installer is created for.
type Operation int

const (
	// Install operation for installing an application
	Install Operation = iota + 1
	// Update operation for updating an application
	Update
	// Delete operation for deleting an application
	Delete
)

// Installer is used to install or update applications.
type Installer struct {
	fetcher  Fetcher
	op       Operation
	fs       appfs.Copier
	db       prefixer.Prefixer
	endState State

	overridenParameters map[string]interface{}
	permissionsAcked    bool

	man     Manifest
	src     *url.URL
	slug    string
	context string

	manc chan Manifest
	log  logger.Logger
}

// InstallerOptions provides the slug name of the application along with the
// source URL.
type InstallerOptions struct {
	Type             consts.AppType
	Operation        Operation
	Manifest         Manifest
	Slug             string
	SourceURL        string
	Deactivated      bool
	PermissionsAcked bool
	Registries       []*url.URL

	// Used to override the "Parameters" field of konnectors during installation.
	// This modification is useful to allow the parameterization of a konnector
	// at its installation as we do not have yet a registry up and running.
	OverridenParameters map[string]interface{}
}

// Fetcher interface should be implemented by the underlying transport
// used to fetch the application data.
type Fetcher interface {
	// FetchManifest should returns an io.ReadCloser to read the
	// manifest data
	FetchManifest(src *url.URL) (io.ReadCloser, error)
	// Fetch should download the application and install it in the given
	// directory.
	Fetch(src *url.URL, fs appfs.Copier, man Manifest) error
}

// NewInstaller creates a new Installer
func NewInstaller(in *instance.Instance, fs appfs.Copier, opts *InstallerOptions) (*Installer, error) {
	man, err := initManifest(in, opts)
	if err != nil {
		return nil, err
	}

	var src *url.URL
	switch opts.Operation {
	case Install:
		if opts.SourceURL == "" {
			return nil, ErrMissingSource
		}
		src, err = url.Parse(opts.SourceURL)
	case Update, Delete:
		var srcString string
		if opts.SourceURL == "" {
			srcString = man.Source()
		} else {
			srcString = opts.SourceURL
		}
		src, err = url.Parse(srcString)
	default:
		panic("Unknown installer operation")
	}
	if err != nil {
		return nil, err
	}

	var endState State
	if opts.Deactivated || man.State() == Installed {
		endState = Installed
	} else {
		endState = Ready
	}

	var installType string
	switch opts.Operation {
	case Install:
		installType = "install"
	case Update:
		installType = "update"
	case Delete:
		installType = "delete"
	}

	log := logger.WithDomain(in.DomainName()).WithFields(logger.Fields{
		"nspace":        "apps",
		"slug":          man.Slug(),
		"version_start": man.Version(),
		"type":          installType,
	})

	var manFilename string
	switch man.AppType() {
	case consts.WebappType:
		manFilename = WebappManifestName
	case consts.KonnectorType:
		manFilename = KonnectorManifestName
	}

	var fetcher Fetcher
	switch src.Scheme {
	case "git", "git+ssh", "ssh+git", "git+https":
		fetcher = newGitFetcher(manFilename, log)
	case "http", "https":
		fetcher = newHTTPFetcher(manFilename, log)
	case "registry":
		fetcher = newRegistryFetcher(opts.Registries, log)
	case "file":
		fetcher = newFileFetcher(manFilename, log)
	default:
		return nil, ErrNotSupportedSource
	}

	return &Installer{
		fetcher:  fetcher,
		op:       opts.Operation,
		db:       in,
		fs:       fs,
		endState: endState,

		overridenParameters: opts.OverridenParameters,
		permissionsAcked:    opts.PermissionsAcked,

		man:     man,
		src:     src,
		slug:    man.Slug(),
		context: in.ContextName,

		manc: make(chan Manifest, 2),
		log:  log,
	}, nil
}

func initManifest(db prefixer.Prefixer, opts *InstallerOptions) (man Manifest, err error) {
	if man = opts.Manifest; man != nil {
		return man, nil
	}

	slug := opts.Slug
	if slug == "" || !slugReg.MatchString(slug) {
		return nil, ErrInvalidSlugName
	}

	if opts.Operation == Install {
		_, err = GetBySlug(db, slug, opts.Type)
		if err == nil {
			return nil, ErrAlreadyExists
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		switch opts.Type {
		case consts.WebappType:
			man = &WebappManifest{
				doc: &couchdb.JSONDoc{
					Type: consts.Apps,
					M: map[string]interface{}{
						"_id": consts.Apps + "/" + slug,
					},
				},
			}
		case consts.KonnectorType:
			man = &KonnManifest{
				doc: &couchdb.JSONDoc{
					Type: consts.Konnectors,
					M: map[string]interface{}{
						"_id": consts.Konnectors + "/" + slug,
					},
				},
			}
		}
		man.SetSlug(slug)
	} else {
		man, err = GetBySlug(db, slug, opts.Type)
		if err != nil {
			return nil, err
		}
	}

	if man == nil {
		panic("Bad or missing installer type")
	}

	return man, nil
}

// Slug return the slug of the application being installed.
func (i *Installer) Slug() string {
	return i.slug
}

// Domain return the domain of instance associated with the installer.
func (i *Installer) Domain() string {
	return i.db.DomainName()
}

// Run will install, update or delete the application linked to the installer,
// depending on specified operation. It will report its progress or error (see
// Poll method) and should be run asynchronously inside a new goroutine:
// `go installer.Run()`.
func (i *Installer) Run() {
	if err := i.run(); err != nil {
		i.man.SetError(err)
		realtime.GetHub().Publish(i.db, realtime.EventUpdate, i.man.Clone(), nil)
	}
	i.notifyChannel()
}

// RunSync does the same work as Run but can be used synchronously.
func (i *Installer) RunSync() (Manifest, error) {
	i.manc = nil
	if err := i.run(); err != nil {
		return nil, err
	}
	return i.man.Clone().(Manifest), nil
}

func (i *Installer) run() (err error) {
	if i.man == nil {
		panic("Manifest is nil")
	}
	mu := config.Lock().ReadWrite(i.db, "app-"+i.man.Slug())
	if err = mu.Lock(); err != nil {
		i.log.Errorf("Could not get lock: %s", err)
		return err
	}
	defer func() {
		mu.Unlock()
		if err != nil {
			i.log.Errorf("Could not commit installer process: %s", err)
		} else {
			i.log.Infof("Successful installer process: %s", i.man.Version())
		}
	}()
	i.log.Infof("Start installer process: %s", i.man.Version())
	switch i.op {
	case Install:
		return i.install()
	case Update:
		return i.update()
	case Delete:
		return i.delete()
	default:
		panic("Unknown operation")
	}
}

// install will perform the installation of an application. It returns the
// freshly fetched manifest from the source along with a possible error in case
// the installation went wrong.
//
// Note that the fetched manifest is returned even if an error occurred while
// upgrading.
func (i *Installer) install() error {
	newManifest, err := i.ReadManifest(Installing)
	if err != nil {
		i.log.Debugf("Could not read manifest")
		return err
	}
	i.man = newManifest
	i.sendRealtimeEvent()
	i.notifyChannel()
	if err := i.fetcher.Fetch(i.src, i.fs, i.man); err != nil {
		i.log.Debugf("Could not fetch tarball")
		return err
	}
	i.man.SetState(i.endState)
	return i.man.Create(i.db)
}

// checkSkipPermissions checks if the instance contexts is configured to skip
// permissions
func (i *Installer) checkSkipPermissions() (bool, error) {
	domain := i.Domain()
	if domain == prefixer.UnknownDomainName {
		return false, nil
	}

	inst, err := instance.Get(domain)
	if err != nil {
		return false, err
	}
	ctxSettings, ok := inst.SettingsContext()
	if !ok {
		return false, nil
	}

	sp, ok := ctxSettings["permissions_skip_verification"]
	if !ok {
		return false, nil
	}

	return sp.(bool), nil
}

// update will perform the update of an already installed application. It
// returns the freshly fetched manifest from the source along with a possible
// error in case the update went wrong.
//
// Note that the fetched manifest is returned even if an error occurred while
// upgrading.
func (i *Installer) update() error {
	// Reload the manifest from the database. It was loaded before this
	// goroutine obtains the lock, and it may happen that another goroutine has
	// made an update between the first load and the lock obtention.
	//
	// The first read is made before the lock to make the happy path (the app
	// is already up-to-date) faster.
	if i.man.AppType() == consts.WebappType {
		reloaded, err := GetWebappBySlug(i.db, i.man.Slug())
		if err != nil {
			return err
		}
		i.man = reloaded
	} else {
		reloaded, err := GetKonnectorBySlug(i.db, i.man.Slug())
		if err != nil {
			return err
		}
		i.man = reloaded
	}

	if err := i.checkState(i.man); err != nil {
		return err
	}

	oldManifest := i.man
	newManifest, err := i.ReadManifest(Upgrading)
	if err != nil {
		return err
	}

	if fetcher, ok := i.fetcher.(*registryFetcher); ok {
		newManifest.SetVersion(fetcher.appVersion())
	}

	// Fast path for registry:// and http:// sources: we do not need to go
	// further in the case where the fetched manifest has the same version has
	// the one in database.
	//
	// For git:// and file:// sources, it may be more complicated since we need
	// to actually fetch the data to extract the exact version of the manifest.
	makeUpdate := true
	availableVersion := ""
	switch i.src.Scheme {
	case "registry", "http", "https":
		makeUpdate = (newManifest.Version() != oldManifest.Version())
	}

	// Check the possible permissions changes before updating. If the
	// verifyPermissions flag is activated (for non manual updates for example),
	// we cancel out the update and mark the UpdateAvailable field of the
	// application instead of actually updating.
	if build.IsDevRelease() {
		// If we are in dev release we want to automatically accept any permissions set
		// inside the manifest. This allows bypassing the authorization acceptation
		// page, a tiresome step for local dev.
		switch newManifest.AppType() {
		case consts.WebappType:
			err = permission.ForceWebapp(i.db, newManifest.Slug(), newManifest.Permissions())
		case consts.KonnectorType:
			err = permission.ForceKonnector(i.db, newManifest.Slug(), newManifest.Permissions())
		default:
			err = fmt.Errorf("invalid app type: %q", newManifest.AppType())
		}
		if err != nil {
			return fmt.Errorf("failed to force the permission set: %w", err)
		}
	} else if makeUpdate && !isPlatformApp(oldManifest, i.context) {
		oldPermissions := oldManifest.Permissions()
		newPermissions := newManifest.Permissions()
		samePermissions := false

		if newPermissions != nil && oldPermissions != nil {
			samePermissions = newPermissions.HasSameRules(oldPermissions)

			// XXX the stack can auto-update konnectors if only a permission of
			// carbon copy or electronic safe is added, without asking
			// permission from the user.
			if !samePermissions && oldManifest.AppType() == consts.KonnectorType {
				diff := permission.Diff(oldPermissions, newPermissions)
				for _, rule := range diff {
					if rule.Type == consts.CertifiedCarbonCopy ||
						rule.Type == consts.CertifiedElectronicSafe {
						if !oldPermissions.RuleInSubset(rule) {
							oldPermissions = append(oldPermissions, rule)
							samePermissions = newPermissions.HasSameRules(oldPermissions)
						}
					}
				}
			}
		}

		if !samePermissions && !i.permissionsAcked {
			// Check if we are going to skip the permissions
			skip, err := i.checkSkipPermissions()
			if err != nil {
				return err
			}
			if !skip {
				makeUpdate = false
				availableVersion = newManifest.Version()
			}
		}
	}

	oldTermsVersion := oldManifest.Terms().Version
	newTermsVersion := newManifest.Terms().Version

	termsAdded := oldTermsVersion == "" && newTermsVersion != ""
	termsUpdated := oldTermsVersion != newTermsVersion

	if (termsAdded || termsUpdated) && !i.permissionsAcked {
		makeUpdate = false
		availableVersion = newManifest.Version()
	}

	extraPerms := permission.Set{}
	var alteredPerms *permission.Permission
	// The "extraPerms" set represents the post-install alterations of the
	// permissions between the oldManifest and the current permissions.
	//
	// Even if makeUpdate is false, we are going to update the manifest document
	// to set an AvailableVersion. In this case, the current webapp/konnector
	// perms will be reapplied and custom ones will be lost if we don't rewrite
	// them.
	inst, err := instance.Get(i.Domain())
	if err == nil {
		// Check if perms were added on the old manifest
		if i.man.AppType() == consts.WebappType {
			alteredPerms, err = permission.GetForWebapp(inst, i.man.Slug())
		} else if i.man.AppType() == consts.KonnectorType {
			alteredPerms, err = permission.GetForKonnector(inst, i.man.Slug())
		}
		if err != nil {
			return err
		}
	}

	if alteredPerms != nil {
		extraPerms = permission.Diff(oldManifest.Permissions(), alteredPerms.Permissions)
	}

	if makeUpdate {
		i.man = newManifest
		i.sendRealtimeEvent()
		i.notifyChannel()
		if err := i.fetcher.Fetch(i.src, i.fs, i.man); err != nil {
			return err
		}
		i.man.SetAvailableVersion("")
		i.man.SetState(i.endState)
	} else {
		if i.man.AppType() == consts.WebappType {
			i.man.(*WebappManifest).oldServices = i.man.(*WebappManifest).val.Services
		}
		i.man.SetSource(i.src)
		if availableVersion != "" {
			i.man.SetAvailableVersion(availableVersion)
		}
		i.sendRealtimeEvent()
		i.notifyChannel()
	}

	return i.man.Update(i.db, extraPerms)
}

func (i *Installer) notifyChannel() {
	if i.manc != nil {
		i.manc <- i.man.Clone().(Manifest)
	}
}

func (i *Installer) delete() error {
	if err := i.checkState(i.man); err != nil {
		return err
	}
	return i.man.Delete(i.db)
}

// checkState returns whether or not the manifest is in the right state to
// perform an update or deletion.
func (i *Installer) checkState(man Manifest) error {
	state := man.State()
	if state == Ready || state == Installed {
		return nil
	}
	if time.Since(man.LastUpdate()) > 15*time.Minute {
		return nil
	}
	return ErrBadState
}

// ReadManifest will fetch the manifest and read its JSON content into the
// passed manifest pointer.
//
// The State field of the manifest will be set to the specified state.
func (i *Installer) ReadManifest(state State) (Manifest, error) {
	r, err := i.fetcher.FetchManifest(i.src)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	var buf bytes.Buffer
	tee := io.TeeReader(r, &buf)

	newManifest, err := i.man.ReadManifest(io.LimitReader(tee, ManifestMaxSize), i.slug, i.src.String())
	if err != nil {
		return nil, err
	}
	// The version is a segment of the key under which the files are stored
	// for all the instances: with a slash, it could point inside the files of
	// another app.
	version := newManifest.Version()
	if version == "." || version == ".." || strings.Contains(version, "/") {
		return nil, ErrBadManifest
	}
	// The checksum is the last part of that key: only the fetcher sets it.
	newManifest.SetChecksum("")
	newManifest.SetState(state)

	set := newManifest.Permissions()
	for _, rule := range set {
		if err := permission.CheckDoctypeName(rule.Type, true); err != nil {
			return nil, err
		}
	}

	shouldOverrideParameters := (i.overridenParameters != nil &&
		i.man.AppType() == consts.KonnectorType &&
		i.src.Scheme != "registry")
	if shouldOverrideParameters {
		if m, ok := newManifest.(*KonnManifest); ok {
			m.val.Parameters = i.overridenParameters
		}
	}

	// Checking the new manifest apptype to prevent human mistakes (like asking
	// a konnector installation instead of a webapp)
	newAppType := struct {
		AppType string `json:"type"`
	}{}

	var newManifestAppType consts.AppType
	if err = json.Unmarshal(buf.Bytes(), &newAppType); err == nil {
		if newAppType.AppType == "konnector" {
			newManifestAppType = consts.KonnectorType
		}
		if newAppType.AppType == "webapp" {
			newManifestAppType = consts.WebappType
		}
	}

	appTypesEmpty := i.man.AppType() == 0 || newManifestAppType == 0
	appTypesMismatch := i.man.AppType() != newManifestAppType

	if !appTypesEmpty && appTypesMismatch {
		var typeError error
		switch i.man.AppType() {
		case consts.KonnectorType:
			typeError = ErrInvalidManifestForKonnector
		case consts.WebappType:
			typeError = ErrInvalidManifestForWebapp
		default:
			typeError = ErrInvalidManifestTypes
		}
		return nil, fmt.Errorf("[%s] %w", i.man.Slug(), typeError)
	}
	return newManifest, nil
}

func (i *Installer) sendRealtimeEvent() {
	realtime.GetHub().Publish(i.db, realtime.EventUpdate, i.man.Clone(), nil)
}

// Poll should be used to monitor the progress of the Installer.
func (i *Installer) Poll() (Manifest, bool, error) {
	man := <-i.manc
	done := false
	if s := man.State(); s == Ready || s == Installed || s == Errored {
		done = true
	}
	return man, done, man.Error()
}

// ManifestChannel returns the channel that can be listened to get updates
// about the installer run.
func (i *Installer) ManifestChannel() chan Manifest {
	return i.manc
}

// DoLazyUpdate tries to update an application before using it
func DoLazyUpdate(in *instance.Instance, man Manifest, copier appfs.Copier, registries []*url.URL) Manifest {
	// An app served from a local directory (--appdir) is not installed.
	if webapp, ok := man.(*WebappManifest); ok && webapp.FromAppsDir {
		return man
	}
	src, err := url.Parse(man.Source())
	if err != nil {
		return man
	}

	if src.Scheme == "registry" {
		var v *registry.Version
		channel, _ := getRegistryChannel(src)
		v, errv := registry.GetLatestVersion(man.Slug(), channel, registries)
		if errv != nil {
			return man
		}
		if v.Version == man.Version() {
			// In some cases, if the source had been altered mutiples times, the app
			// may currently be in a stale state.

			// Example:
			// - The version 1.0.0 of the "foobar" konnector is installed from
			// "stable" channel
			// - The use switches to "beta" channel, the version 1.0.1 is available,
			// but with extra perms
			// - The update is blocked because of these news perms, the
			// "available_version" is set to 1.0.1, the user switches back to "stable"
			// channel
			// - We are now on a stale state, no new version is available, but an
			// available_version is set

			// We ensure that we are not in this stale state by removing the
			// available version field from the manifest if the latest version is
			// the same as the current version
			if man.AvailableVersion() != "" {
				man.SetAvailableVersion("")
				_ = man.Update(in, nil)
			}
			return man
		}

		if man.AvailableVersion() != "" && v.Version == man.AvailableVersion() {
			return man
		}
		if channel == "stable" && !IsMoreRecent(man.Version(), v.Version) {
			return man
		}
	}

	inst, err := NewInstaller(in, copier, &InstallerOptions{
		Operation:        Update,
		Manifest:         man,
		Registries:       registries,
		SourceURL:        src.String(),
		Type:             man.AppType(),
		Slug:             man.Slug(),
		PermissionsAcked: false,
	})
	if err != nil {
		return man
	}
	newman, err := inst.RunSync()
	if err != nil {
		return man
	}
	return newman
}

// IsMoreRecent returns true if b is greater than a
func IsMoreRecent(a, b string) bool {
	vA, err := semver.NewVersion(a)
	if err != nil {
		return true
	}
	vB, err := semver.NewVersion(b)
	if err != nil {
		return false
	}
	return vB.GreaterThan(vA)
}

func isPlatformApp(man Manifest, contextName string) bool {
	if man.AppType() != consts.WebappType {
		return false
	}
	if utils.IsInArray(man.Slug(), consts.PlatformApps) {
		return true
	}

	contexts := config.GetConfig().Contexts
	if contexts == nil {
		return false
	}
	context, ok := contexts[contextName].(map[string]interface{})
	if !ok {
		context, ok = contexts[config.DefaultInstanceContext].(map[string]interface{})
	}
	if !ok {
		return false
	}
	additional, ok := context["additional_platform_apps"].([]interface{})
	if !ok {
		return false
	}
	for _, slug := range additional {
		if slug == man.Slug() {
			return true
		}
	}
	return false
}
