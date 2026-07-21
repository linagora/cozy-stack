package couchdb

import (
	"context"
	"fmt"
	"time"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"golang.org/x/sync/errgroup"
)

// IndexViewsVersion is the version of current definition of views & indexes.
// This number should be incremented when this file changes.
const IndexViewsVersion int = 39

// Indexes is the index list required by an instance to run properly.
var Indexes = []*mango.Index{
	// Permissions
	mango.MakeIndex(consts.Permissions, "by-source-and-type", mango.IndexDef{Fields: []string{"source_id", "type"}}),

	// Used to lookup over the children of a directory
	mango.MakeIndex(consts.Files, "dir-children", mango.IndexDef{Fields: []string{"dir_id", "_id"}}),
	// Used to lookup a directory given its path
	mango.MakeIndex(consts.Files, "dir-by-path", mango.IndexDef{Fields: []string{"path"}}),
	// Used to find notes
	mango.MakeIndex(consts.Files, "by-mime-updated-at", mango.IndexDef{Fields: []string{"mime", "trashed", "updated_at"}}),
	// Used by the FSCK to detect conflicts
	mango.MakeIndex(consts.Files, "with-conflicts", mango.IndexDef{Fields: []string{"_conflicts"}}),
	// Used to count the shortuts to a sharing that have not been seen
	mango.MakeIndex(consts.Files, "by-sharing-status", mango.IndexDef{Fields: []string{"metadata.sharing.status"}}),
	// Used to find old files and directories in the trashed that should be deleted
	mango.MakeIndex(consts.Files, "by-dir-id-updated-at", mango.IndexDef{Fields: []string{"dir_id", "updated_at"}}),

	// Used to lookup a queued and running jobs
	mango.MakeIndex(consts.Jobs, "by-worker-and-state", mango.IndexDef{Fields: []string{"worker", "state"}}),
	mango.MakeIndex(consts.Jobs, "by-trigger-id", mango.IndexDef{Fields: []string{"trigger_id", "queued_at"}}),
	mango.MakeIndex(consts.Jobs, "by-queued-at", mango.IndexDef{Fields: []string{"queued_at"}}),

	// Used to lookup a trigger to see if it exists or must be created
	mango.MakeIndex(consts.Triggers, "by-worker-and-type", mango.IndexDef{Fields: []string{"worker", "type"}}),

	// Used to lookup oauth clients by name
	mango.MakeIndex(consts.OAuthClients, "by-client-name", mango.IndexDef{Fields: []string{"client_name"}}),
	mango.MakeIndex(consts.OAuthClients, "by-notification-platform", mango.IndexDef{Fields: []string{"notification_platform"}}),
	mango.MakeIndex(consts.OAuthClients, "connected-user-clients", mango.IndexDef{
		Fields: []string{"client_kind", "client_name"},
		PartialFilter: mango.And(
			mango.In("client_kind", []interface{}{"browser", "desktop", "mobile"}),
			mango.NotExists("pending"),
		),
	}),

	// Used to lookup login history by OS, browser, and IP
	mango.MakeIndex(consts.SessionsLogins, "by-os-browser-ip", mango.IndexDef{Fields: []string{"os", "browser", "ip"}}),

	// Used to lookup notifications by their source, ordered by their creation
	// date
	mango.MakeIndex(consts.Notifications, "by-source-id", mango.IndexDef{Fields: []string{"source_id", "created_at"}}),

	// Used to find the myself document
	mango.MakeIndex(consts.Contacts, "by-me", mango.IndexDef{Fields: []string{"me"}}),

	// Used to lookup the bitwarden ciphers
	mango.MakeIndex(consts.BitwardenCiphers, "by-folder-id", mango.IndexDef{Fields: []string{"folder_id"}}),
	mango.MakeIndex(consts.BitwardenCiphers, "by-organization-id", mango.IndexDef{Fields: []string{"organization_id"}}),

	// Used to find the contacts in a group
	mango.MakeIndex(consts.Contacts, "by-groups", mango.IndexDef{Fields: []string{"relationships.groups.data"}}),

	// Used to find the active sharings
	mango.MakeIndex(consts.Sharings, "active", mango.IndexDef{Fields: []string{"active"}}),

	// Used to detect an already in-flight Nextcloud migration when a user
	// tries to start a new one.
	mango.MakeIndex(consts.NextcloudMigrations, "by-status", mango.IndexDef{Fields: []string{"status"}}),
}

// DiskUsageView is the view used for computing the disk usage for files
var DiskUsageView = &View{
	Name:    "disk-usage",
	Doctype: consts.Files,
	Map: `
function(doc) {
  if (doc.type === 'file') {
    emit(doc.dir_id, +doc.size);
  }
}
`,
	Reduce: "_sum",
}

// OldVersionsDiskUsageView is the view used for computing the disk usage for
// the old versions of file contents.
var OldVersionsDiskUsageView = &View{
	Name:    "old-versions-disk-usage",
	Doctype: consts.FilesVersions,
	Map: `
function(doc) {
  emit(doc._id, +doc.size);
}
`,
	Reduce: "_sum",
}

// DirNotSynchronizedOnView is the view used for fetching directories that are
// not synchronized on a given device.
var DirNotSynchronizedOnView = &View{
	Name:    "not-synchronized-on",
	Doctype: consts.Files,
	Reduce:  "_count",
	Map: `
function(doc) {
  if (doc.type === "directory" && isArray(doc.not_synchronized_on)) {
    for (var i = 0; i < doc.not_synchronized_on.length; i++) {
      emit([doc.not_synchronized_on[i].type, doc.not_synchronized_on[i].id]);
    }
  }
}`,
}

// FilesReferencedByView is the view used for fetching files referenced by a
// given document
var FilesReferencedByView = &View{
	Name:    "referenced-by",
	Doctype: consts.Files,
	Reduce:  "_count",
	Map: `
function(doc) {
  if (isArray(doc.referenced_by)) {
    for (var i = 0; i < doc.referenced_by.length; i++) {
      emit([doc.referenced_by[i].type, doc.referenced_by[i].id]);
    }
  }
}`,
}

// ReferencedBySortedByDatetimeView is the view used for fetching files referenced by a
// given document, sorted by the datetime
var ReferencedBySortedByDatetimeView = &View{
	Name:    "referenced-by-sorted-by-datetime",
	Doctype: consts.Files,
	Reduce:  "_count",
	Map: `
function(doc) {
  if (isArray(doc.referenced_by)) {
    for (var i = 0; i < doc.referenced_by.length; i++) {
      var datetime = (doc.metadata && doc.metadata.datetime) || '';
      emit([doc.referenced_by[i].type, doc.referenced_by[i].id, datetime]);
    }
  }
}`,
}

// FilesByParentView is the view used for fetching files referenced by a
// given document
var FilesByParentView = &View{
	Name:    "by-parent-type-name",
	Doctype: consts.Files,
	Map: `
function(doc) {
  emit([doc.dir_id, doc.type, doc.name])
}`,
	Reduce: "_count",
}

// PermissionsShareByCView is the view for fetching the permissions associated
// to a document via a token code.
var PermissionsShareByCView = &View{
	Name:    "byToken",
	Doctype: consts.Permissions,
	Map: `
function(doc) {
  if (doc.type && doc.type.slice(0, 5) === "share" && doc.codes) {
    Object.keys(doc.codes).forEach(function(k) {
      emit(doc.codes[k]);
    })
  }
}`,
}

// PermissionsShareByShortcodeView is the view for fetching the permissions associated
// to a document via a token code.
var PermissionsShareByShortcodeView = &View{
	Name:    "by-short-code",
	Doctype: consts.Permissions,
	Map: `
function(doc) {
	if(doc.shortcodes) {
		for(var idx in doc.shortcodes) {
			emit(doc.shortcodes[idx], idx);
		}
	}
}`,
}

// PermissionsShareByDocView is the view for fetching a list of permissions
// associated to a list of IDs.
var PermissionsShareByDocView = &View{
	Name:    "byDoc",
	Doctype: consts.Permissions,
	Map: `
function(doc) {
  if (doc.type === "share" && doc.permissions) {
    Object.keys(doc.permissions).forEach(function(k) {
      var p = doc.permissions[k];
      var selector = p.selector || "_id";
      for (var i=0; i<p.values.length; i++) {
        emit([p.type, selector, p.values[i]], p.verbs);
      }
    });
  }
}`,
}

// PermissionsByDoctype returns a list of permissions that have at least one
// rule for the given doctype.
var PermissionsByDoctype = &View{
	Name:    "permissions-by-doctype",
	Doctype: consts.Permissions,
	Map: `
function(doc) {
  if (doc.permissions) {
    Object.keys(doc.permissions).forEach(function(k) {
	  emit([doc.permissions[k].type, doc.type]);
	});
  }
}
`,
}

// SharedDocsBySharingID is the view for fetching a list of shared doctype/id
// associated with a sharingid
var SharedDocsBySharingID = &View{
	Name:    "shared-docs-by-sharingid",
	Doctype: consts.Shared,
	Map: `
function(doc) {
  if (doc.infos) {
    Object.keys(doc.infos).forEach(function(k) {
      emit(k, doc._id);
    });
  }
}`,
}

// SharingsByDocTypeView is the view for fetching a list of sharings
// associated with a doctype
var SharingsByDocTypeView = &View{
	Name:    "sharings-by-doctype",
	Doctype: consts.Sharings,
	Map: `
function(doc) {
	if (isArray(doc.rules)) {
		for (var i = 0; i < doc.rules.length; i++) {
			if (!doc.rules[i].local) {
				emit(doc.rules[i].doctype, doc._id);
			}
		}
	}
}`,
}

// ContactByEmail is used to find a contact by its email address
var ContactByEmail = &View{
	Name:    "contacts-by-email",
	Doctype: consts.Contacts,
	Map: `
function(doc) {
	if (isArray(doc.email)) {
		for (var i = 0; i < doc.email.length; i++) {
			emit(doc.email[i].address, doc._id);
		}
	}
}
`,
}

// Views is the list of all views that are created by the stack.
var Views = []*View{
	DiskUsageView,
	OldVersionsDiskUsageView,
	DirNotSynchronizedOnView,
	FilesReferencedByView,
	ReferencedBySortedByDatetimeView,
	FilesByParentView,
	PermissionsShareByCView,
	PermissionsShareByDocView,
	PermissionsByDoctype,
	PermissionsShareByShortcodeView,
	SharedDocsBySharingID,
	SharingsByDocTypeView,
	ContactByEmail,
}

// ViewsByDoctype returns the list of views for a specified doc type.
func ViewsByDoctype(doctype string) []*View {
	var views []*View
	for _, view := range Views {
		if view.Doctype == doctype {
			views = append(views, view)
		}
	}
	return views
}

// IndexesByDoctype returns the list of indexes for a specified doc type.
func IndexesByDoctype(doctype string) []*mango.Index {
	var indexes []*mango.Index
	for _, index := range Indexes {
		if index.Doctype == doctype {
			indexes = append(indexes, index)
		}
	}
	return indexes
}

// globalIndexes is the index list required on the global databases to run
// properly.
var globalIndexes = []*mango.Index{
	mango.MakeIndex(consts.Exports, "by-domain", mango.IndexDef{Fields: []string{"domain", "created_at"}}),
	mango.MakeIndex(consts.Instances, "by-oidcid", mango.IndexDef{Fields: []string{"oidc_id"}}),
	mango.MakeIndex(consts.Instances, "by-olddomain", mango.IndexDef{Fields: []string{"old_domain"}}),
	mango.MakeIndex(consts.Instances, "by-orgdomain", mango.IndexDef{Fields: []string{"org_domain"}}),
	mango.MakeIndex(consts.Instances, "by-orgid", mango.IndexDef{Fields: []string{"org_id"}}),
}

// secretIndexes is the index list required on the secret databases to run
// properly
var secretIndexes = []*mango.Index{
	mango.MakeIndex(consts.AccountTypes, "by-slug", mango.IndexDef{Fields: []string{"slug"}}),
}

// DomainAndAliasesView defines a view to fetch instances by domain and domain
// aliases.
var DomainAndAliasesView = &View{
	Name:    "domain-and-aliases",
	Doctype: consts.Instances,
	Map: `
function(doc) {
  emit(doc.domain);
  if (isArray(doc.domain_aliases)) {
    for (var i = 0; i < doc.domain_aliases.length; i++) {
      emit(doc.domain_aliases[i]);
    }
  }
}
`,
}

// globalViews is the list of all views that are created by the stack on the
// global databases.
var globalViews = []*View{
	DomainAndAliasesView,
}

// InitGlobalDB defines views and indexes on the global databases. It is called
// on every startup of the stack.
func InitGlobalDB(ctx context.Context) error {
	var err error
	// Check that we can properly reach CouchDB.
	attempts := 8
	attemptsSpacing := 1 * time.Second
	for i := 0; i < attempts; i++ {
		_, err = CheckStatus(ctx)
		if err == nil {
			break
		}

		err = fmt.Errorf("could not reach Couchdb database: %w", err)
		if i < attempts-1 {
			logger.WithNamespace("stack").Warnf("%s, retrying in %v", err, attemptsSpacing)
			time.Sleep(attemptsSpacing)
		}
	}

	if err != nil {
		return fmt.Errorf("failed contact couchdb: %w", err)
	}

	g, _ := errgroup.WithContext(context.Background())

	DefineIndexes(g, prefixer.SecretsPrefixer, secretIndexes)
	DefineIndexes(g, prefixer.GlobalPrefixer, globalIndexes)
	DefineViews(g, prefixer.GlobalPrefixer, globalViews)

	return g.Wait()
}

// CheckDesignDocCanBeDeleted will return false for an index or view used by
// the stack.
func CheckDesignDocCanBeDeleted(doctype, name string) bool {
	for _, index := range Indexes {
		if doctype == index.Doctype && name == index.Request.DDoc {
			return false
		}
	}
	for _, view := range Views {
		if doctype == view.Doctype && name == view.Name {
			return false
		}
	}
	return true
}
