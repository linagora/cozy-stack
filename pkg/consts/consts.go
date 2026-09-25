// Package consts is only for declaring some constants used by the stack, like
// some slugs, IDs, doctypes, etc.
package consts

const (
	// StoreSlug is the slug of the store application: it can install
	// konnectors and applications.
	StoreSlug = "store"
	// HomeSlug is the slug of the default app, where the user is redirected
	// after login.
	HomeSlug = "home"
	// SettingsSlug is the slug of the settings application.
	SettingsSlug = "settings"
	// DriveSlug is the slug of the drive app, where the user can be sent if
	// the disk quota alert is raised.
	DriveSlug = "drive"
	// NotesSlug is the slug of notes app, which is used by the stack for
	// referencing a directory that contains the notes with collaborative
	// edition.
	NotesSlug = "notes"
	// PassSlug is the slug of cozy-pass webapp, which is used by the stack for
	// linking the bitwarden OAuth clients.
	PassSlug = "passwords"
	// DataProxySlug is the slug of the dataproxy webapp, which is used for
	// embedding a PouchDB in the client (used by the search for example).
	DataProxySlug = "dataproxy"
)

const (
	// BitwardenSettingsID is the id of the settings document for bitwarden
	BitwardenSettingsID = "io.cozy.settings.bitwarden"
	// ContextSettingsID is the id of the settings JSON-API response for the context
	ContextSettingsID = "io.cozy.settings.context"
	// ClientsUsageID is the id of the settings JSON-API response for clients-usage
	ClientsUsageID = "io.cozy.settings.clients-usage"
	// DiskUsageID is the id of the settings JSON-API response for disk-usage
	DiskUsageID = "io.cozy.settings.disk-usage"
	// InstanceSettingsID is the id of settings document for the instance
	InstanceSettingsID = "io.cozy.settings.instance"
	// CapabilitiesSettingsID is the id of the settings document with the
	// capabilities for a given instance
	CapabilitiesSettingsID = "io.cozy.settings.capabilities"
	// ExternalTiesID is the id of the settings document with the external ties
	// for a given instance
	ExternalTiesID = "io.cozy.settings.external-ties"
	// PassphraseParametersID is the id of settings document for the passphrase
	// parameters used to hash the master password on client side.
	PassphraseParametersID = "io.cozy.settings.passphrase"
	// FlagsSettingsID is the id of settings document with the feature flags.
	FlagsSettingsID = "io.cozy.settings.flags"
	// InstanceFlagsSettingsID is the id of the settings documents with the
	// feature flags specific to this instance.
	InstanceFlagsSettingsID = "io.cozy.settings.flags.instance"
	// ManagerFlagsSettingsID is the id of the settings documents with the
	// feature flags coming from the manager (via the feature sets).
	ManagerFlagsSettingsID = "io.cozy.settings.flags.manager"
	// ConfigFlagsSettingsID is the id of the settings documents with the
	// feature flags defined in the contexts section of the configuration file.
	ConfigFlagsSettingsID = "io.cozy.settings.flags.config"
	// ContextFlagsSettingsID is the id of the settings documents with the
	// feature flags defined with a ratio on the contexts.
	ContextFlagsSettingsID = "io.cozy.settings.flags.context"
	// DefaultFlagsSettingsID is the id of the settings documents with the
	// default feature flags.
	DefaultFlagsSettingsID = "io.cozy.settings.flags.default"
)

const (
	// BitwardenCozyOrganizationName is the name of the organization used to
	// share passwords between Cozy and Bitwarden clients.
	BitwardenCozyOrganizationName = "Cozy"
	// BitwardenCozyCollectionName is the name of the collection used to
	// share passwords between Cozy and Bitwarden clients.
	BitwardenCozyCollectionName = "Cozy Connectors"

	// BitwardenProtocol is the name of the bitwarden protocol
	BitwardenProtocol = "Bitwarden"
	// BitwardenCipherRelationship is the name of the account-cipher relationship
	BitwardenCipherRelationship = "vaultCipher"
)

// MaxItemsPerPageForMango is the maximal value accepted for the limit
// parameter used for mango pagination
const MaxItemsPerPageForMango = 1000

// ShortCodeLen is the number of chars for the shortcode
const ShortCodeLen = 12

// TinyCodeLen is the number of digits for the tinycode
const TinyCodeLen = 6

// KnownFlatDomains is a list of top-domains that can hosts cozy instances with
// flat sub-domains.
var KnownFlatDomains = []string{
	"mycozy.cloud",
	"cozy.rocks",
}

// DefaultLocale is the default locale when creating an instance and for i18n.
const DefaultLocale = "en"

// SupportedLocales is the list of supported locales tags.
var SupportedLocales = []string{"en", "fr"}

// PlatformApps is the list of applications that can be updated automatically
// even if their permissions have changed.
var PlatformApps = []string{
	"admin",
	"banks",
	"contacts",
	"drive",
	"home",
	"notes",
	"passwords",
	"photos",
	"settings",
	"store",
	"coachco2",
	"mespapiers",
	"dataproxy",
}
