package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/feature"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/notification"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/appfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/metadata"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/spf13/afero"
)

// defaultAppListLimit is the default limit for returned documents
const defaultAppListLimit = 100

// Route is a struct to serve a folder inside an app
type Route struct {
	Folder string `json:"folder"`
	Index  string `json:"index"`
	Public bool   `json:"public"`
}

// NotFound returns true for a blank route (ie not found by FindRoute)
func (c *Route) NotFound() bool { return c.Folder == "" }

// Routes is a map for routing inside an application.
type Routes map[string]Route

// Service is a struct to define a service executed by the stack.
type Service struct {
	name string

	Type           string `json:"type"`
	File           string `json:"file"`
	Debounce       string `json:"debounce"`
	TriggerOptions string `json:"trigger"`
	TriggerID      string `json:"trigger_id"`
}

// Services is a map to define services assciated with an application.
type Services map[string]*Service

// Notifications is a map to define the notifications properties used by the
// application.
type Notifications map[string]notification.Properties

// Intent is a declaration of a service for other client-side apps
type Intent struct {
	Action string   `json:"action"`
	Types  []string `json:"type"`
	Href   string   `json:"href"`
}

// Terms of an application/webapp
type Terms struct {
	URL     string `json:"url"`
	Version string `json:"version"`
}

// Locales is used for the translations of the application name.
// "fr" -> "name" -> "Cozy Drive"
type Locales map[string]map[string]interface{}

// WebappManifest contains all the informations associated with an installed web
// application.
type WebappManifest struct {
	doc *couchdb.JSONDoc
	err error

	val struct {
		// Fields that can be read and updated
		Slug             string    `json:"slug"`
		Source           string    `json:"source"`
		State            State     `json:"state"`
		Version          string    `json:"version"`
		AvailableVersion string    `json:"available_version"`
		Checksum         string    `json:"checksum"`
		CreatedAt        time.Time `json:"created_at"`
		UpdatedAt        time.Time `json:"updated_at"`
		Err              string    `json:"error"`

		// Just readers
		Name          string `json:"name"`
		NamePrefix    string `json:"name_prefix"`
		Icon          string `json:"icon"`
		Editor        string `json:"editor"`
		ClientURLFlag string `json:"client_url_flag"`

		// Fields with complex types
		Permissions   permission.Set `json:"permissions"`
		Terms         Terms          `json:"terms"`
		Intents       []Intent       `json:"intents"`
		Routes        Routes         `json:"routes"`
		Services      Services       `json:"services"`
		Locales       Locales        `json:"locales"`
		Notifications Notifications  `json:"notifications"`
	}

	FromAppsDir bool        `json:"-"` // Used in development
	Instance    SubDomainer `json:"-"` // Used for JSON-API links

	oldServices Services // Used to diff against when updating the app
}

// ID is part of the Manifest interface
func (m *WebappManifest) ID() string { return m.doc.ID() }

// Rev is part of the Manifest interface
func (m *WebappManifest) Rev() string { return m.doc.Rev() }

// DocType is part of the Manifest interface
func (m *WebappManifest) DocType() string { return consts.Apps }

// Clone implements couchdb.Doc
func (m *WebappManifest) Clone() couchdb.Doc {
	cloned := *m
	cloned.doc = m.doc.Clone().(*couchdb.JSONDoc)
	cloned.val.Permissions = make(permission.Set, len(m.val.Permissions))
	copy(cloned.val.Permissions, m.val.Permissions)
	return &cloned
}

// SetID is part of the Manifest interface
func (m *WebappManifest) SetID(id string) { m.doc.SetID(id) }

// SetRev is part of the Manifest interface
func (m *WebappManifest) SetRev(rev string) { m.doc.SetRev(rev) }

// SetSource is part of the Manifest interface
func (m *WebappManifest) SetSource(src *url.URL) { m.val.Source = src.String() }

// Source is part of the Manifest interface
func (m *WebappManifest) Source() string { return m.val.Source }

// Version is part of the Manifest interface
func (m *WebappManifest) Version() string { return m.val.Version }

// AvailableVersion is part of the Manifest interface
func (m *WebappManifest) AvailableVersion() string { return m.val.AvailableVersion }

// Checksum is part of the Manifest interface
func (m *WebappManifest) Checksum() string { return m.val.Checksum }

// Slug is part of the Manifest interface
func (m *WebappManifest) Slug() string { return m.val.Slug }

// State is part of the Manifest interface
func (m *WebappManifest) State() State { return m.val.State }

// LastUpdate is part of the Manifest interface
func (m *WebappManifest) LastUpdate() time.Time { return m.val.UpdatedAt }

// SetSlug is part of the Manifest interface
func (m *WebappManifest) SetSlug(slug string) { m.val.Slug = slug }

// SetState is part of the Manifest interface
func (m *WebappManifest) SetState(state State) { m.val.State = state }

// SetVersion is part of the Manifest interface
func (m *WebappManifest) SetVersion(version string) { m.val.Version = version }

// SetAvailableVersion is part of the Manifest interface
func (m *WebappManifest) SetAvailableVersion(version string) { m.val.AvailableVersion = version }

// SetChecksum is part of the Manifest interface
func (m *WebappManifest) SetChecksum(shasum string) { m.val.Checksum = shasum }

// AppType is part of the Manifest interface
func (m *WebappManifest) AppType() consts.AppType { return consts.WebappType }

// Terms is part of the Manifest interface
func (m *WebappManifest) Terms() Terms { return m.val.Terms }

// Permissions is part of the Manifest interface
func (m *WebappManifest) Permissions() permission.Set { return m.val.Permissions }

// Name returns the webapp name.
func (m *WebappManifest) Name() string { return m.val.Name }

// Icon returns the webapp icon path.
func (m *WebappManifest) Icon() string { return m.val.Icon }

// Editor returns the webapp editor.
func (m *WebappManifest) Editor() string { return m.val.Editor }

// ClientURLFlag returns the name of the feature flag whose value is the
// client URL to use for postMessage in intents, when the app is not hosted
// on a cozy subdomain.
func (m *WebappManifest) ClientURLFlag() string { return m.val.ClientURLFlag }

// ResolveClientURL returns the client URL for an app slug: the value of the
// feature flag referenced by the app's "client_url_flag" manifest key when present
// and valid, falling back to the cozy subdomain otherwise. It is used for
// postMessage targetOrigin in intents and for CSP frame-ancestors.
func ResolveClientURL(ins *instance.Instance, slug string) string {
	manifest, err := GetWebappBySlug(ins, slug)
	if err != nil {
		return DefaultClientURL(ins, slug)
	}
	flagKey := manifest.ClientURLFlag()
	if flagKey == "" {
		return DefaultClientURL(ins, slug)
	}
	flags, err := feature.GetFlags(ins)
	if err != nil {
		return DefaultClientURL(ins, slug)
	}
	flagValue, ok := flags.M[flagKey].(string)
	if !ok {
		return DefaultClientURL(ins, slug)
	}
	if _, err := url.ParseRequestURI(flagValue); err != nil {
		return DefaultClientURL(ins, slug)
	}
	return flagValue
}

func DefaultClientURL(ins *instance.Instance, slug string) string {
	u := ins.SubDomain(slug)
	u.Path = ""
	return u.String()
}

// NamePrefix returns the webapp name prefix.
func (m *WebappManifest) NamePrefix() string { return m.val.NamePrefix }

// Notifications returns the notifications properties for this webapp.
func (m *WebappManifest) Notifications() Notifications {
	return m.val.Notifications
}

func (m *WebappManifest) Services() Services {
	return m.val.Services
}

// SetError is part of the Manifest interface
func (m *WebappManifest) SetError(err error) {
	m.SetState(Errored)
	m.val.Err = err.Error()
	m.err = err
}

// Error is part of the Manifest interface
func (m *WebappManifest) Error() error { return m.err }

// Fetch is part of the Manifest interface
func (m *WebappManifest) Fetch(field string) []string {
	switch field {
	case "slug":
		return []string{m.val.Slug}
	case "state":
		return []string{string(m.val.State)}
	}
	return nil
}

// NameLocalized returns the name of the app in the given locale
func (m *WebappManifest) NameLocalized(locale string) string {
	if m.val.Locales != nil && locale != "" {
		if dict, ok := m.val.Locales[locale]; ok {
			if v, ok := dict["name"].(string); ok && v != "" {
				return v
			}
		}
	}
	return m.val.Name
}

func (m *WebappManifest) MarshalJSON() ([]byte, error) {
	doc := m.doc.Clone().(*couchdb.JSONDoc)
	doc.Type = consts.Apps
	doc.M["slug"] = m.val.Slug
	doc.M["source"] = m.val.Source
	doc.M["state"] = m.val.State
	doc.M["version"] = m.val.Version
	if m.val.AvailableVersion == "" {
		delete(doc.M, "available_version")
	} else {
		doc.M["available_version"] = m.val.AvailableVersion
	}
	doc.M["checksum"] = m.val.Checksum
	doc.M["created_at"] = m.val.CreatedAt
	doc.M["updated_at"] = m.val.UpdatedAt
	if m.val.Err == "" {
		delete(doc.M, "error")
	} else {
		doc.M["error"] = m.val.Err
	}
	// XXX: keep the weird UnmarshalJSON of permission.Set
	perms, err := m.val.Permissions.MarshalJSON()
	if err != nil {
		return nil, err
	}
	doc.M["permissions"] = json.RawMessage(perms)
	doc.M["terms"] = m.val.Terms
	doc.M["intents"] = m.val.Intents
	doc.M["routes"] = m.val.Routes
	doc.M["services"] = m.val.Services
	doc.M["locales"] = m.val.Locales
	doc.M["notifications"] = m.val.Notifications
	return json.Marshal(doc)
}

func (m *WebappManifest) UnmarshalJSON(j []byte) error {
	if err := json.Unmarshal(j, &m.doc); err != nil {
		return err
	}
	if err := json.Unmarshal(j, &m.val); err != nil {
		return err
	}
	return nil
}

// ReadManifest is part of the Manifest interface
func (m *WebappManifest) ReadManifest(r io.Reader, slug, sourceURL string) (Manifest, error) {
	var newManifest WebappManifest
	if err := json.NewDecoder(r).Decode(&newManifest); err != nil {
		return nil, ErrBadManifest
	}

	newManifest.SetID(consts.Apps + "/" + slug)
	newManifest.SetRev(m.Rev())
	newManifest.SetState(m.State())
	newManifest.val.CreatedAt = m.val.CreatedAt
	newManifest.val.Slug = slug
	newManifest.val.Source = sourceURL
	newManifest.Instance = m.Instance
	newManifest.oldServices = m.val.Services
	if newManifest.val.Routes == nil {
		newManifest.val.Routes = make(Routes)
		newManifest.val.Routes["/"] = Route{
			Folder: "/",
			Index:  "index.html",
			Public: false,
		}
	}

	return &newManifest, nil
}

// Create is part of the Manifest interface
func (m *WebappManifest) Create(db prefixer.Prefixer) error {
	m.SetID(consts.Apps + "/" + m.val.Slug)
	m.val.CreatedAt = time.Now()
	m.val.UpdatedAt = time.Now()
	if err := couchdb.CreateNamedDocWithDB(db, m); err != nil {
		return err
	}

	if len(m.val.Services) > 0 {
		if err := diffServices(db, m.Slug(), nil, m.val.Services); err != nil {
			return err
		}
		_ = couchdb.UpdateDoc(db, m)
	}

	_, err := permission.CreateWebappSet(db, m.Slug(), m.Permissions(), m.Version())
	return err
}

// Update is part of the Manifest interface
func (m *WebappManifest) Update(db prefixer.Prefixer, extraPerms permission.Set) error {
	if err := diffServices(db, m.Slug(), m.oldServices, m.val.Services); err != nil {
		return err
	}
	m.val.UpdatedAt = time.Now()
	if err := couchdb.UpdateDoc(db, m); err != nil {
		return err
	}

	var err error
	perms := m.Permissions()

	// Merging the potential extra permissions
	if len(extraPerms) > 0 {
		perms, err = permission.MergeExtraPermissions(perms, extraPerms)
		if err != nil {
			return err
		}
	}

	_, err = permission.UpdateWebappSet(db, m.Slug(), perms)
	return err
}

// Delete is part of the Manifest interface
func (m *WebappManifest) Delete(db prefixer.Prefixer) error {
	err := diffServices(db, m.Slug(), m.val.Services, nil)
	if err != nil {
		return err
	}
	err = permission.DestroyWebapp(db, m.Slug())
	if err != nil && !couchdb.IsNotFoundError(err) {
		return err
	}
	return couchdb.DeleteDoc(db, m)
}

func diffServices(db prefixer.Prefixer, slug string, oldServices, newServices Services) error {
	if oldServices == nil {
		oldServices = make(Services)
	}
	if newServices == nil {
		newServices = make(Services)
	}

	var deleted []*Service
	var created []*Service

	clone := make(Services)
	for newName, newService := range newServices {
		clone[newName] = newService
		newService.name = newName
	}

	for name, oldService := range oldServices {
		oldService.name = name
		newService, ok := newServices[name]
		if !ok {
			deleted = append(deleted, oldService)
			continue
		}
		delete(clone, name)
		if newService.File != oldService.File ||
			newService.Type != oldService.Type ||
			newService.TriggerOptions != oldService.TriggerOptions ||
			newService.Debounce != oldService.Debounce {
			deleted = append(deleted, oldService)
			created = append(created, newService)
		} else {
			*newService = *oldService
		}
		newService.name = name
	}
	for _, newService := range clone {
		created = append(created, newService)
	}

	sched := job.System()
	for _, service := range deleted {
		if service.TriggerID != "" {
			if err := sched.DeleteTrigger(db, service.TriggerID); err != nil && !errors.Is(err, job.ErrNotFoundTrigger) {
				return err
			}
		}
	}

	for _, service := range created {
		triggerID, err := CreateServiceTrigger(db, slug, service.name, service)
		if err != nil {
			return err
		}
		if triggerID != "" {
			service.TriggerID = triggerID
		}
	}

	return nil
}

// CreateServiceTrigger creates a trigger for the given service. It returns the
// id of the created trigger or an error.
func CreateServiceTrigger(db prefixer.Prefixer, slug, serviceName string, service *Service) (string, error) {
	var triggerType string
	var triggerArgs string
	triggerOpts := strings.SplitN(service.TriggerOptions, " ", 2)
	if len(triggerOpts) > 0 {
		triggerType = strings.TrimSpace(triggerOpts[0])
	}
	if len(triggerOpts) > 1 {
		triggerArgs = strings.TrimSpace(triggerOpts[1])
	}

	// Do not create triggers for services called programmatically
	if triggerType == "" || service.TriggerOptions == "@at 2000-01-01T00:00:00.000Z" {
		return "", nil
	}

	// Add metadata
	md, err := metadata.NewWithApp(slug, "", job.DocTypeVersionTrigger)
	if err != nil {
		return "", err
	}
	msg := map[string]string{
		"slug": slug,
		"name": serviceName,
	}
	trigger, err := job.NewTrigger(db, job.TriggerInfos{
		Type:       triggerType,
		WorkerType: "service",
		Debounce:   service.Debounce,
		Arguments:  triggerArgs,
		Metadata:   md,
	}, msg)
	if err != nil {
		return "", err
	}
	sched := job.System()
	if err = sched.AddTrigger(trigger); err != nil {
		return "", err
	}
	return trigger.ID(), nil
}

// FindRoute takes a path, returns the route which matches the best,
// and the part that remains unmatched
func (m *WebappManifest) FindRoute(vpath string) (Route, string) {
	parts := strings.Split(vpath, "/")
	lenParts := len(parts)

	var best Route
	rest := ""
	specificity := 0
	for key, ctx := range m.val.Routes {
		var keys []string
		if key == "/" {
			keys = []string{""}
		} else {
			keys = strings.Split(key, "/")
		}
		count := len(keys)
		if count > lenParts || count < specificity {
			continue
		}
		if routeMatches(parts, keys) {
			specificity = count
			best = ctx
			rest = path.Join(parts[count:]...)
		}
	}

	return best, rest
}

// FindIntent returns an intent for the given action and type if the manifest has one
func (m *WebappManifest) FindIntent(action, typ string) *Intent {
	for _, intent := range m.val.Intents {
		if !strings.EqualFold(action, intent.Action) {
			continue
		}
		for _, t := range intent.Types {
			if t == typ {
				return &intent
			}
			// Allow a joker for mime-types like image/*
			if strings.HasSuffix(t, "/*") {
				if strings.SplitN(t, "/", 2)[0] == strings.SplitN(typ, "/", 2)[0] {
					return &intent
				}
			}
		}
	}
	return nil
}

// appsdir is a map of slug -> directory used in development for webapps that
// are not installed in the Cozy but serve directly from a directory.
var appsdir map[string]string

// SetupAppsDir allow to load some webapps from directories for development.
func SetupAppsDir(apps map[string]string) {
	if appsdir == nil {
		appsdir = make(map[string]string)
	}
	for app, dir := range apps {
		appsdir[app] = dir
	}
}

// FSForAppDir returns a FS for the webapp in development.
func FSForAppDir(slug string) appfs.FileServer {
	base := baseFSForAppDir(slug)
	return appfs.NewAferoFileServer(base, func(_, _, _, file string) string {
		return path.Join("/", file)
	})
}

func baseFSForAppDir(slug string) afero.Fs {
	return afero.NewBasePathFs(afero.NewOsFs(), appsdir[slug])
}

// loadManifestFromDir returns a manifest for a webapp in development.
func loadManifestFromDir(slug string) (*WebappManifest, error) {
	dir, ok := appsdir[slug]
	if !ok {
		return nil, ErrNotFound
	}
	fs := baseFSForAppDir(slug)
	manFile, err := fs.Open(WebappManifestName)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("Could not find the manifest in your app directory %s", dir)
		}
		return nil, err
	}
	app := &WebappManifest{
		doc: &couchdb.JSONDoc{},
	}
	man, err := app.ReadManifest(manFile, slug, "file://localhost"+dir)
	if err != nil {
		return nil, fmt.Errorf("Could not parse the manifest: %s", err.Error())
	}
	app = man.(*WebappManifest)
	app.FromAppsDir = true
	app.val.State = Ready
	return app, nil
}

// GetWebappBySlug fetch the WebappManifest from the database given a slug.
func GetWebappBySlug(db prefixer.Prefixer, slug string) (*WebappManifest, error) {
	if slug == "" || !slugReg.MatchString(slug) {
		return nil, ErrInvalidSlugName
	}
	for app := range appsdir {
		if app == slug {
			return loadManifestFromDir(slug)
		}
	}
	man := &WebappManifest{}
	err := couchdb.GetDoc(db, consts.Apps, consts.Apps+"/"+slug, man)
	if couchdb.IsNotFoundError(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return man, nil
}

// GetWebappBySlugAndUpdate fetch the WebappManifest and perform an update of
// the application if necessary and if the application was installed from the
// registry.
func GetWebappBySlugAndUpdate(in *instance.Instance, slug string, copier appfs.Copier, registries []*url.URL) (*WebappManifest, error) {
	man, err := GetWebappBySlug(in, slug)
	if err != nil {
		return nil, err
	}
	return DoLazyUpdate(in, man, copier, registries).(*WebappManifest), nil
}

// ListWebappsWithPagination returns the list of installed web applications with
// a pagination
func ListWebappsWithPagination(db prefixer.Prefixer, limit int, startKey string) ([]*WebappManifest, string, error) {
	var docs []*WebappManifest

	if limit == 0 {
		limit = defaultAppListLimit
	}

	req := &couchdb.AllDocsRequest{
		Limit:    limit + 1, // Also get the following document for the next key
		StartKey: startKey,
	}
	err := couchdb.GetAllDocs(db, consts.Apps, req, &docs)
	if err != nil {
		return nil, "", err
	}

	nextID := ""
	if len(docs) > 0 && len(docs) == limit+1 { // There are still documents to fetch
		nextDoc := docs[len(docs)-1]
		nextID = nextDoc.ID()
		docs = docs[:len(docs)-1]
		return docs, nextID, nil
	}

	// If we get here, either :
	// - There are no more docs in couchDB
	// - There are no docs at all
	// We can load extra apps and append them safely to the list
	for slug := range appsdir {
		if man, err := loadManifestFromDir(slug); err == nil {
			docs = append(docs, man)
		}
	}

	return docs, nextID, nil
}

var _ Manifest = &WebappManifest{}
