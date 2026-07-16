// Package instance is for the instance model, with domain, locale, settings,
// etc.
package instance

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/model/vfs/vfsafero"
	"github.com/cozy/cozy-stack/model/vfs/vfss3"
	"github.com/cozy/cozy-stack/model/vfs/vfsswift"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/i18n"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/lock"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/golang-jwt/jwt/v5"
	"github.com/spf13/afero"
)

// DefaultTemplateTitle represents the default template title. It could be
// overrided by configuring it in the instance context parameters
const DefaultTemplateTitle = "Twake Workplace"

// PBKDF2_SHA256 is the value of kdf for using PBKDF2 with SHA256 to hash the
// password on client side.
//
//lint:ignore ST1003 we prefer ALL_CAPS here
const PBKDF2_SHA256 = 0

// An Instance has the informations relatives to the logical cozy instance,
// like the domain, the locale or the access to the databases and files storage
// It is a couchdb.Doc to be persisted in couchdb.
type Instance struct {
	DocID           string   `json:"_id,omitempty"`  // couchdb _id
	DocRev          string   `json:"_rev,omitempty"` // couchdb _rev
	Domain          string   `json:"domain"`         // The main DNS domain, like example.twake.app
	DomainAliases   []string `json:"domain_aliases,omitempty"`
	OrgDomain       string   `json:"org_domain,omitempty"`       // The instance's organization custom domain, like mycompany.com
	OrgID           string   `json:"org_id,omitempty"`           // The instance's organization ID
	OldDomain       string   `json:"old_domain,omitempty"`       // The old DNS domain, like old.mycozy.cloud
	Prefix          string   `json:"prefix,omitempty"`           // Possible database prefix
	Locale          string   `json:"locale"`                     // The locale used on the server
	UUID            string   `json:"uuid,omitempty"`             // UUID associated with the instance
	OIDCID          string   `json:"oidc_id,omitempty"`          // An identifier to check authentication from OIDC
	FranceConnectID string   `json:"franceconnect_id,omitempty"` // An identifier to check authentication from FranceConnect
	ContextName     string   `json:"context,omitempty"`          // The context attached to the instance
	Sponsorships    []string `json:"sponsorships,omitempty"`     // The list of sponsorships for the instance
	TOSSigned       string   `json:"tos,omitempty"`              // Terms of Service signed version
	TOSLatest       string   `json:"tos_latest,omitempty"`       // Terms of Service latest version
	AuthMode        AuthMode `json:"auth_mode,omitempty"`        // 2 factor authentication
	MagicLink       bool     `json:"magic_link,omitempty"`       // Authentication via a link sent by email
	Deleting        bool     `json:"deleting,omitempty"`
	Moved           bool     `json:"moved,omitempty"`           // If the instance has been moved to a new place
	Blocked         bool     `json:"blocked,omitempty"`         // Whether or not the instance is blocked
	BlockingReason  string   `json:"blocking_reason,omitempty"` // Why the instance is blocked
	NoAutoUpdate    bool     `json:"no_auto_update,omitempty"`  // Whether or not the instance has auto updates for its applications

	OnboardingFinished bool  `json:"onboarding_finished,omitempty"` // Whether or not the onboarding is complete.
	PasswordDefined    *bool `json:"password_defined"`              // 3 possibles states: true, false, and unknown (for legacy reasons)

	BytesDiskQuota    int64 `json:"disk_quota,string,omitempty"` // The total size in bytes allowed to the user
	IndexViewsVersion int   `json:"indexes_version,omitempty"`

	CommonSettingsVersion int `json:"common_settings_version,omitempty"`

	// Swift layout number:
	// - 0 for layout v1
	// - 1 for layout v2
	// - 2 for layout v3
	// It is called swift_cluster in CouchDB and indexed from 0 for legacy reasons.
	// See model/vfs/vfsswift for more details.
	SwiftLayout int `json:"swift_cluster,omitempty"`

	// FsScheme, when non-empty, overrides the global fs.url scheme for this
	// instance. Used to migrate a single instance to another storage backend
	// (e.g. "s3") without changing the stack-wide default. Empty = global default.
	FsScheme string `json:"fs_scheme,omitempty"`

	CouchCluster int `json:"couch_cluster,omitempty"`

	// PassphraseHash is a hash of a hash of the user's passphrase: the
	// passphrase is first hashed in client-side to avoid sending it to the
	// server as it also used for encryption on client-side, and after that,
	// hashed on the server to ensure robustness. For more informations on the
	// server-side hashing, see crypto.GenerateFromPassphrase.
	PassphraseHash       []byte     `json:"passphrase_hash,omitempty"`
	PassphraseResetToken []byte     `json:"passphrase_reset_token,omitempty"`
	PassphraseResetTime  *time.Time `json:"passphrase_reset_time,omitempty"`

	// Secure assets

	// Register token is used on registration to prevent from stealing instances
	// waiting for registration. The registerToken secret is only shared (in
	// clear) with the instance's user.
	RegisterToken []byte `json:"register_token,omitempty"`
	// SessSecret is used to authenticate session cookies
	SessSecret []byte `json:"session_secret,omitempty"`
	// OAuthSecret is used to authenticate OAuth2 token
	OAuthSecret []byte `json:"oauth_secret,omitempty"`
	// CLISecret is used to authenticate request from the CLI
	CLISecret []byte `json:"cli_secret,omitempty"`

	// FeatureFlags is the feature flags that are specific to this instance
	FeatureFlags map[string]interface{} `json:"feature_flags,omitempty"`
	// FeatureSets is a list of feature sets from the manager
	FeatureSets []string `json:"feature_sets,omitempty"`

	// LastActivityFromDeletedOAuthClients is the date of the last activity for
	// OAuth clients that have been deleted
	LastActivityFromDeletedOAuthClients *time.Time `json:"last_activity_from_deleted_oauth_clients,omitempty"`

	vfs              vfs.VFS
	contextualDomain string
}

// DocType implements couchdb.Doc
func (i *Instance) DocType() string { return consts.Instances }

// ID implements couchdb.Doc
func (i *Instance) ID() string { return i.DocID }

// SetID implements couchdb.Doc
func (i *Instance) SetID(v string) { i.DocID = v }

// Rev implements couchdb.Doc
func (i *Instance) Rev() string { return i.DocRev }

// SetRev implements couchdb.Doc
func (i *Instance) SetRev(v string) { i.DocRev = v }

// Clone implements couchdb.Doc
func (i *Instance) Clone() couchdb.Doc {
	cloned := *i

	cloned.DomainAliases = make([]string, len(i.DomainAliases))
	copy(cloned.DomainAliases, i.DomainAliases)

	cloned.PassphraseHash = make([]byte, len(i.PassphraseHash))
	copy(cloned.PassphraseHash, i.PassphraseHash)

	cloned.PassphraseResetToken = make([]byte, len(i.PassphraseResetToken))
	copy(cloned.PassphraseResetToken, i.PassphraseResetToken)

	if i.PassphraseResetTime != nil {
		tmp := *i.PassphraseResetTime
		cloned.PassphraseResetTime = &tmp
	}

	cloned.RegisterToken = make([]byte, len(i.RegisterToken))
	copy(cloned.RegisterToken, i.RegisterToken)

	cloned.SessSecret = make([]byte, len(i.SessSecret))
	copy(cloned.SessSecret, i.SessSecret)

	cloned.OAuthSecret = make([]byte, len(i.OAuthSecret))
	copy(cloned.OAuthSecret, i.OAuthSecret)

	cloned.CLISecret = make([]byte, len(i.CLISecret))
	copy(cloned.CLISecret, i.CLISecret)
	return &cloned
}

// DBCluster returns the index of the CouchDB cluster where the databases for
// this instance can be found.
func (i *Instance) DBCluster() int {
	return i.CouchCluster
}

// DBPrefix returns the prefix to use in database naming for the
// current instance
func (i *Instance) DBPrefix() string {
	if i.Prefix != "" {
		return i.Prefix
	}
	return i.Domain
}

// StorageScheme returns the storage backend scheme effective for this instance:
// the per-instance FsScheme override when set, otherwise the global fs.url scheme.
func (i *Instance) StorageScheme() string {
	if i.FsScheme != "" {
		return i.FsScheme
	}
	return config.FsURL().Scheme
}

// DomainName returns the main domain name of the instance.
func (i *Instance) DomainName() string {
	return i.Domain
}

// GetContextName returns the name of the context.
func (i *Instance) GetContextName() string {
	return i.ContextName
}

// GetOrgID returns the organization ID of the instance.
func (i *Instance) GetOrgID() string {
	return i.OrgID
}

// SessionSecret returns the session secret.
func (i *Instance) SessionSecret() []byte {
	// The prefix is here to invalidate all the sessions that were created on
	// an instance where the password was not hashed on client-side. It force
	// the user to log in again and migrate its passphrase to be hashed on the
	// client. It is simpler/safer and, in particular, it avoids that he/she
	// can try to changed its pass in settings (which would fail).
	secret := make([]byte, 2+len(i.SessSecret))
	secret[0] = '2'
	secret[1] = ':'
	copy(secret[2:], i.SessSecret)
	return secret
}

// SlugAndDomain returns the splitted slug and domain of the instance
// Ex: foobar.mycozy.cloud => ["foobar", "mycozy.cloud"]
func (i *Instance) SlugAndDomain() (string, string) {
	splitted := strings.SplitN(i.Domain, ".", 2)
	return splitted[0], splitted[1]
}

// IsOrganizationInstance returns true when the instance's canonical slug
// matches its organization ID.
func (i *Instance) IsOrganizationInstance() bool {
	if i == nil || i.OrgID == "" {
		return false
	}
	slug, _, found := strings.Cut(i.Domain, ".")
	return found && slug != "" && slug == i.OrgID
}

// Logger returns the logger associated with the instance
func (i *Instance) Logger() *logger.Entry {
	return logger.WithDomain(i.Domain)
}

// VFS returns the storage provider where the binaries for the current instance
// are persisted
func (i *Instance) VFS() vfs.VFS {
	if i.vfs == nil {
		panic("instance: calling VFS() before makeVFS()")
	}
	return i.vfs
}

// MakeVFS is used to initialize the VFS linked to this instance
func (i *Instance) MakeVFS() error {
	if i.vfs != nil {
		return nil
	}
	mutex := config.Lock().ReadWrite(i, "vfs")
	index := vfs.NewCouchdbIndexer(i)
	disk := vfs.DiskThresholder(i)
	var err error
	switch i.StorageScheme() {
	case config.SchemeFile, config.SchemeMem:
		i.vfs, err = vfsafero.New(i, index, disk, mutex, config.FsURL(), i.DirName())
	case config.SchemeSwift, config.SchemeSwiftSecure:
		switch i.SwiftLayout {
		case 2:
			i.vfs, err = vfsswift.NewV3(i, index, disk, mutex)
		default:
			err = ErrInvalidSwiftLayout
		}
	case config.SchemeS3:
		i.vfs, err = vfss3.New(i, index, disk, mutex)
	default:
		err = fmt.Errorf("instance: unknown storage provider %s", i.StorageScheme())
	}
	return err
}

// AvatarFS returns the hidden filesystem for storing the avatar.
func (i *Instance) AvatarFS() vfs.Avatarer {
	switch i.StorageScheme() {
	case config.SchemeFile:
		fsURL := config.FsURL()
		baseFS := afero.NewBasePathFs(afero.NewOsFs(),
			path.Join(fsURL.Path, i.DirName(), vfs.ThumbsDirName))
		return vfsafero.NewAvatarFs(baseFS)
	case config.SchemeMem:
		baseFS := vfsafero.GetMemFS(i.DomainName() + "-avatar")
		return vfsafero.NewAvatarFs(baseFS)
	case config.SchemeSwift, config.SchemeSwiftSecure:
		switch i.SwiftLayout {
		case 2:
			return vfsswift.NewAvatarFsV3(config.GetSwiftConnection(), i)
		default:
			panic(ErrInvalidSwiftLayout)
		}
	case config.SchemeS3:
		client := config.GetS3Client()
		bucket := vfss3.BucketName(i.GetOrgID(), config.GetS3BucketPrefix())
		keyPrefix := i.DBPrefix() + "/"
		return vfss3.NewAvatarFs(client, bucket, keyPrefix)
	default:
		panic(fmt.Sprintf("instance: unknown storage provider %s", i.StorageScheme()))
	}
}

// ThumbsFS returns the hidden filesystem for storing the thumbnails of the
// photos/image
func (i *Instance) ThumbsFS() vfs.Thumbser {
	switch i.StorageScheme() {
	case config.SchemeFile:
		fsURL := config.FsURL()
		baseFS := afero.NewBasePathFs(afero.NewOsFs(),
			path.Join(fsURL.Path, i.DirName(), vfs.ThumbsDirName))
		return vfsafero.NewThumbsFs(baseFS)
	case config.SchemeMem:
		baseFS := vfsafero.GetMemFS(i.DomainName() + "-thumbs")
		return vfsafero.NewThumbsFs(baseFS)
	case config.SchemeSwift, config.SchemeSwiftSecure:
		switch i.SwiftLayout {
		case 2:
			return vfsswift.NewThumbsFsV3(config.GetSwiftConnection(), i)
		default:
			panic(ErrInvalidSwiftLayout)
		}
	case config.SchemeS3:
		client := config.GetS3Client()
		bucket := vfss3.BucketName(i.GetOrgID(), config.GetS3BucketPrefix())
		keyPrefix := i.DBPrefix() + "/"
		return vfss3.NewThumbsFs(client, bucket, keyPrefix)
	default:
		panic(fmt.Sprintf("instance: unknown storage provider %s", i.StorageScheme()))
	}
}

// EnsureSharedDrivesDir returns the Shared Drives directory, and creates it if
// it doesn't exist
func (i *Instance) EnsureSharedDrivesDir() (*vfs.DirDoc, error) {
	fs := i.VFS()
	dir, err := fs.DirByID(consts.SharedDrivesDirID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if dir != nil {
		return dir, nil
	}

	name := i.Translate("Tree Shared Drives")
	dir, err = vfs.NewDirDocWithPath(name, consts.RootDirID, "/", nil)
	if err != nil {
		return nil, err
	}
	dir.DocID = consts.SharedDrivesDirID
	dir.CozyMetadata = vfs.NewCozyMetadata(i.PageURL("/", nil))
	err = fs.CreateDir(dir)
	if errors.Is(err, os.ErrExist) {
		dir, err = fs.DirByPath(dir.Fullpath)
	}
	if err != nil {
		return nil, err
	}
	return dir, nil
}

// NotesLock returns a mutex for the notes on this instance.
func (i *Instance) NotesLock() lock.ErrorRWLocker {
	return config.Lock().ReadWrite(i, "notes")
}

func (i *Instance) SetPasswordDefined(defined bool) {
	if (i.PasswordDefined == nil || !*i.PasswordDefined) && defined {
		doc := couchdb.JSONDoc{
			Type: consts.Settings,
			M:    map[string]interface{}{"_id": consts.PassphraseParametersID},
		}
		realtime.GetHub().Publish(i, realtime.EventCreate, &doc, nil)
	}

	i.PasswordDefined = &defined
}

// SettingsDocument returns the document with the settings of this instance
func (i *Instance) SettingsDocument() (*couchdb.JSONDoc, error) {
	doc := &couchdb.JSONDoc{}
	err := couchdb.GetDoc(i, consts.Settings, consts.InstanceSettingsID, doc)
	if err != nil {
		return nil, err
	}
	doc.Type = consts.Settings
	return doc, nil
}

// SettingsEMail returns the email address defined in the settings of this
// instance.
func (i *Instance) SettingsEMail() (string, error) {
	settings, err := i.SettingsDocument()
	if err != nil {
		return "", err
	}
	email, _ := settings.M["email"].(string)
	return email, nil
}

// SettingsPublicName returns the public name defined in the settings of this
// instance.
func (i *Instance) SettingsPublicName() (string, error) {
	settings, err := i.SettingsDocument()
	if err != nil {
		return "", err
	}
	name, _ := settings.M["public_name"].(string)
	return name, nil
}

// GetFromContexts returns the parameters specific to the instance context
func (i *Instance) GetFromContexts(contexts map[string]interface{}) (interface{}, bool) {
	if contexts == nil {
		return nil, false
	}

	if i.ContextName != "" {
		context, ok := contexts[i.ContextName]
		if ok {
			return context, true
		}
	}

	context, ok := contexts[config.DefaultInstanceContext]
	if ok && context != nil {
		return context, ok
	}

	return nil, false
}

// SettingsContext returns the map from the config that matches the context of
// this instance
func (i *Instance) SettingsContext() (map[string]interface{}, bool) {
	contexts := config.GetConfig().Contexts
	context, ok := i.GetFromContexts(contexts)
	if !ok {
		return nil, false
	}
	settings := context.(map[string]interface{})
	return settings, true
}

// HasSignup returns true when the instance's context is connected to the
// external signup (SaaS) application, i.e. when a signup_url is configured for
// the context. In that case the user profile (email, phone, recovery email) is
// managed by the signup flow, so the stack must not let those attributes be
// changed through its own settings endpoints.
func (i *Instance) HasSignup() bool {
	if ctxSettings, ok := i.SettingsContext(); ok {
		signupURL, _ := ctxSettings["signup_url"].(string)
		return signupURL != ""
	}
	return false
}

// SupportEmailAddress returns the email address that can be used to contact
// the support.
func (i *Instance) SupportEmailAddress() string {
	if ctxSettings, ok := i.SettingsContext(); ok {
		if email, ok := ctxSettings["support_address"].(string); ok {
			return email
		}
	}
	return "support@twake.app"
}

// SupportPageURL returns the URL of a page where users can find assistance (with
// answers to frequently asked questions for example).
func (i *Instance) SupportPageURL() string {
	if ctxSettings, ok := i.SettingsContext(); ok {
		if url, ok := ctxSettings["support_page_url"].(string); ok {
			return url
		}
	}
	return ""
}

// TemplateTitle returns the specific-context instance template title (if there
// is one). Otherwise, returns the default one
func (i *Instance) TemplateTitle() string {
	ctxSettings, ok := i.SettingsContext()
	if !ok {
		return DefaultTemplateTitle
	}
	if title, ok := ctxSettings["templates_title"].(string); ok && title != "" {
		return title
	}
	return DefaultTemplateTitle
}

// MoveURL returns URL for move wizard.
func (i *Instance) MoveURL() string {
	moveURL := config.GetConfig().Move.URL
	if settings, ok := i.SettingsContext(); ok {
		if u, ok := settings["move_url"].(string); ok {
			moveURL = u
		}
	}
	return moveURL
}

// Registries returns the list of registries associated with the instance.
func (i *Instance) Registries() []*url.URL {
	contexts := config.GetConfig().Registries
	var context []*url.URL
	var ok bool
	if i.ContextName != "" {
		context, ok = contexts[i.ContextName]
	}
	if !ok {
		context, ok = contexts[config.DefaultInstanceContext]
		if !ok {
			context = make([]*url.URL, 0)
		}
	}
	return context
}

// RAGServer returns the RAG server for the instance (AI features).
func (i *Instance) RAGServer() config.RAGServer {
	contexts := config.GetConfig().RAGServers
	if i.ContextName != "" {
		if server, ok := contexts[i.ContextName]; ok {
			return server
		}
	}
	return contexts[config.DefaultInstanceContext]
}

// HasForcedOIDC returns true only if the instance is in a context where the
// config says that the stack shouldn't allow to authenticate with the
// password.
func (i *Instance) HasForcedOIDC() bool {
	if i.ContextName == "" {
		return false
	}
	auth, ok := config.GetConfig().Authentication[i.ContextName].(map[string]interface{})
	if !ok {
		return false
	}
	disabled, ok := auth["disable_password_authentication"].(bool)
	if !ok {
		return false
	}
	return disabled
}

// PassphraseSalt computes the salt for the client-side hashing of the master
// password. The rule for computing the salt is to create a fake email address
// "me@<domain>". If OldDomain is set, it uses the old domain for backward
// compatibility during domain migration.
func (i *Instance) PassphraseSalt() []byte {
	domain := i.Domain
	if i.OldDomain != "" {
		domain = i.OldDomain
	}
	domain = strings.Split(domain, ":")[0] // Skip the optional port
	return []byte("me@" + domain)
}

// DiskQuota returns the number of bytes allowed on the disk to the user.
func (i *Instance) DiskQuota() int64 {
	return i.BytesDiskQuota
}

// WithContextualDomain the current instance context with the given hostname.
func (i *Instance) WithContextualDomain(domain string) *Instance {
	if i.HasDomain(domain) {
		i.contextualDomain = domain
	}
	return i
}

// Scheme returns the scheme used for URLs. It is https by default and http
// for development instances.
func (i *Instance) Scheme() string {
	if build.IsDevRelease() {
		return "http"
	}
	return "https"
}

// ContextualDomain returns the domain with regard to the current domain
// request.
func (i *Instance) ContextualDomain() string {
	if i.contextualDomain != "" {
		return i.contextualDomain
	}
	return i.Domain
}

// HasDomain returns whether or not the given domain name is owned by this
// instance, as part of its main domain name or its aliases.
func (i *Instance) HasDomain(domain string) bool {
	if domain == i.Domain {
		return true
	}
	for _, alias := range i.DomainAliases {
		if domain == alias {
			return true
		}
	}
	return false
}

// SubDomain returns the full url for a subdomain of this instance
// useful with apps slugs
func (i *Instance) SubDomain(s string) *url.URL {
	domain := i.ContextualDomain()
	if config.GetConfig().Subdomains == config.NestedSubdomains {
		domain = s + "." + domain
	} else {
		parts := strings.SplitN(domain, ".", 2)
		domain = parts[0] + "-" + s + "." + parts[1]
	}
	return &url.URL{
		Scheme: i.Scheme(),
		Host:   domain,
		Path:   "/",
	}
}

// ChangePasswordURL returns the URL of the settings page that can be used by
// the user to change their password.
func (i *Instance) ChangePasswordURL() string {
	u := i.SubDomain(consts.SettingsSlug)
	u.Fragment = "/profile/password"
	return u.String()
}

// DataProxyCleanURL returns the URL of the DataProxy iframe for cleaning
// PouchDB.
func (i *Instance) DataProxyCleanURL() string {
	u := i.SubDomain(consts.DataProxySlug)
	u.Path = "/reset"
	return u.String()
}

// FromURL normalizes a given url with the scheme and domain of the instance.
func (i *Instance) FromURL(u *url.URL) string {
	u2 := url.URL{
		Scheme:   i.Scheme(),
		Host:     i.ContextualDomain(),
		Path:     u.Path,
		RawQuery: u.RawQuery,
		Fragment: u.Fragment,
	}
	return u2.String()
}

// PageURL returns the full URL for a path on the cozy stack
func (i *Instance) PageURL(path string, queries url.Values) string {
	var query string
	if queries != nil {
		query = queries.Encode()
	}
	u := url.URL{
		Scheme:   i.Scheme(),
		Host:     i.ContextualDomain(),
		Path:     path,
		RawQuery: query,
	}
	return u.String()
}

func (i *Instance) parseRedirectAppAndRoute(redirect string) *url.URL {
	splits := strings.SplitN(redirect, "#", 2)
	parts := strings.SplitN(splits[0], "/", 2)
	u := i.SubDomain(parts[0])
	if len(parts) == 2 {
		u.Path = parts[1]
	}
	if len(splits) == 2 {
		u.Fragment = splits[1]
	}
	return u
}

// DefaultAppAndPath returns the default_redirection from the context, in the
// slug+path format (or use the home as the default application).
func (i *Instance) DefaultAppAndPath() string {
	context, ok := i.SettingsContext()
	if !ok {
		return consts.HomeSlug + "/"
	}
	redirect, ok := context["default_redirection"].(string)
	if !ok {
		return consts.HomeSlug + "/"
	}
	return redirect
}

func (i *Instance) redirection(key, defaultSlug string) *url.URL {
	context, ok := i.SettingsContext()
	if !ok {
		return i.SubDomain(defaultSlug)
	}
	redirect, ok := context[key].(string)
	if !ok {
		return i.SubDomain(defaultSlug)
	}
	return i.parseRedirectAppAndRoute(redirect)
}

// DefaultRedirection returns the URL where to redirect the user afer login
// (and in most other cases where we need a redirection URL)
func (i *Instance) DefaultRedirection() *url.URL {
	if doc, err := i.SettingsDocument(); err == nil {
		// XXX we had a bug where the default_redirection was filled by a full URL
		// instead of slug+path, and we should ignore the bad format here.
		if redirect, ok := doc.M["default_redirection"].(string); ok && !strings.HasPrefix(redirect, "http") {
			return i.parseRedirectAppAndRoute(redirect)
		}
	}

	return i.redirection("default_redirection", consts.HomeSlug)
}

// DefaultRedirectionFromContext returns the URL where to redirect the user
// after login from the context parameters. It can be overloaded by instance
// via the "default_redirection" setting.
func (i *Instance) DefaultRedirectionFromContext() *url.URL {
	return i.redirection("default_redirection", consts.HomeSlug)
}

// OnboardedRedirection returns the URL where to redirect the user after
// onboarding
func (i *Instance) OnboardedRedirection() *url.URL {
	return i.redirection("onboarded_redirection", consts.HomeSlug)
}

// Translate is used to translate a string to the locale used on this instance
func (i *Instance) Translate(key string, vars ...interface{}) string {
	return i18n.Translate(key, i.Locale, i.ContextName, vars...)
}

// List returns the list of declared instances.
func List() ([]*Instance, error) {
	var all []*Instance
	err := ForeachInstances(func(doc *Instance) error {
		all = append(all, doc)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}

// ForeachInstances execute the given callback for each instances.
func ForeachInstances(fn func(*Instance) error) error {
	return couchdb.ForeachDocsWithCustomPagination(prefixer.GlobalPrefixer, consts.Instances, 10000, func(_ string, data json.RawMessage) error {
		var doc *Instance
		if err := json.Unmarshal(data, &doc); err != nil {
			return err
		}
		return fn(doc)
	})
}

// PaginatedList can be used to list the instances, with pagination.
func PaginatedList(limit int, startKey string, skip int) ([]*Instance, string, error) {
	var docs []*Instance
	req := &couchdb.AllDocsRequest{
		// Also get the following document for the next key,
		// and a few more because of the design docs
		Limit:    limit + 10,
		StartKey: startKey,
		Skip:     skip,
	}
	err := couchdb.GetAllDocs(prefixer.GlobalPrefixer, consts.Instances, req, &docs)
	if err != nil {
		return nil, "", err
	}

	if len(docs) > limit { // There are still documents to fetch
		nextDoc := docs[limit]
		docs = docs[:limit]
		return docs, nextDoc.ID(), nil
	}
	return docs, "", nil
}

func FindByOldDomain(oldDomain string) (*Instance, error) {
	var docs []*Instance
	req := &couchdb.FindRequest{
		UseIndex: "by-olddomain",
		Selector: mango.Equal("old_domain", oldDomain),
		Limit:    1,
	}
	err := couchdb.FindDocs(prefixer.GlobalPrefixer, consts.Instances, req, &docs)
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, ErrNotFound
	}
	return docs[0], nil
}

// PickKey choose which of the Instance keys to use depending on token audience
func (i *Instance) PickKey(audience string) ([]byte, error) {
	switch audience {
	case consts.AppAudience, consts.KonnectorAudience:
		return i.SessionSecret(), nil
	case consts.RefreshTokenAudience, consts.AccessTokenAudience, consts.ShareAudience:
		return i.OAuthSecret, nil
	case consts.CLIAudience:
		return i.CLISecret, nil
	}
	return nil, permission.ErrInvalidAudience
}

// MakeJWT is a shortcut to create a JWT
func (i *Instance) MakeJWT(audience, subject, scope, sessionID string, issuedAt time.Time) (string, error) {
	secret, err := i.PickKey(audience)
	if err != nil {
		return "", err
	}
	return crypto.NewJWT(secret, permission.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Audience: jwt.ClaimStrings{audience},
			Issuer:   i.Domain,
			IssuedAt: jwt.NewNumericDate(issuedAt),
			Subject:  subject,
		},
		Scope:     scope,
		SessionID: sessionID,
	})
}

// BuildAppToken is used to build a token to identify the app for requests made
// to the stack
func (i *Instance) BuildAppToken(slug, sessionID string) string {
	scope := "" // apps tokens don't have a scope
	now := time.Now()
	token, err := i.MakeJWT(consts.AppAudience, slug, scope, sessionID, now)
	if err != nil {
		return ""
	}
	return token
}

// BuildKonnectorToken is used to build a token to identify the konnector for
// requests made to the stack
func (i *Instance) BuildKonnectorToken(slug string) string {
	scope := "" // apps tokens don't have a scope
	token, err := i.MakeJWT(consts.KonnectorAudience, slug, scope, "", time.Now())
	if err != nil {
		return ""
	}
	return token
}

// CreateShareCode returns a new sharecode to put the codes field of a
// permissions document
func (i *Instance) CreateShareCode(subject string) (string, error) {
	scope := ""
	sessionID := ""
	return i.MakeJWT(consts.ShareAudience, subject, scope, sessionID, time.Now())
}

// MovedError is used to return an error when the instance has been moved to a
// new domain/hoster.
func (i *Instance) MovedError() *jsonapi.Error {
	if !i.Moved {
		return nil
	}
	jerr := jsonapi.Error{
		Status: http.StatusGone,
		Title:  "Cozy has been moved",
		Code:   "moved",
		Detail: i.Translate("The Cozy has been moved to a new address"),
	}
	doc, err := i.SettingsDocument()
	if err == nil {
		if to, ok := doc.M["moved_to"].(string); ok {
			jerr.Links = &jsonapi.LinksList{Related: to}
		}
	}
	return &jerr
}

func (i *Instance) HasPremiumLinksEnabled() bool {
	if ctxSettings, ok := i.SettingsContext(); ok {
		if enabled, ok := ctxSettings["enable_premium_links"].(bool); ok {
			return enabled
		}
	}
	return false
}

// ensure Instance implements couchdb.Doc
var (
	_ couchdb.Doc = &Instance{}
)
