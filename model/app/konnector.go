package app

import (
	"encoding/json"
	"io"
	"net/url"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/appfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/metadata"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

// KonnManifest contains all the informations associated with an installed
// konnector.
type KonnManifest struct {
	doc *couchdb.JSONDoc
	err error

	val struct {
		// Fields that can be read and updated
		Slug             string                 `json:"slug"`
		Source           string                 `json:"source"`
		State            State                  `json:"state"`
		Version          string                 `json:"version"`
		AvailableVersion string                 `json:"available_version"`
		Checksum         string                 `json:"checksum"`
		Parameters       map[string]interface{} `json:"parameters"`
		CreatedAt        time.Time              `json:"created_at"`
		UpdatedAt        time.Time              `json:"updated_at"`
		Err              string                 `json:"error"`

		// Just readers
		Name            string `json:"name"`
		Icon            string `json:"icon"`
		Language        string `json:"language"`
		ClientSide      bool   `json:"clientSide"`
		OnDeleteAccount string `json:"on_delete_account"`

		// Fields with complex types
		Permissions   permission.Set `json:"permissions"`
		Terms         Terms          `json:"terms"`
		Notifications Notifications  `json:"notifications"`
	}
}

// ID is part of the Manifest interface
func (m *KonnManifest) ID() string { return m.doc.ID() }

// Rev is part of the Manifest interface
func (m *KonnManifest) Rev() string { return m.doc.Rev() }

// DocType is part of the Manifest interface
func (m *KonnManifest) DocType() string { return consts.Konnectors }

// Clone is part of the Manifest interface
func (m *KonnManifest) Clone() couchdb.Doc {
	cloned := *m
	cloned.doc = m.doc.Clone().(*couchdb.JSONDoc)
	cloned.val.Permissions = make(permission.Set, len(m.val.Permissions))
	copy(cloned.val.Permissions, m.val.Permissions)
	return &cloned
}

// SetID is part of the Manifest interface
func (m *KonnManifest) SetID(id string) { m.doc.SetID(id) }

// SetRev is part of the Manifest interface
func (m *KonnManifest) SetRev(rev string) { m.doc.SetRev(rev) }

// SetSlug is part of the Manifest interface
func (m *KonnManifest) SetSlug(slug string) { m.val.Slug = slug }

// SetSource is part of the Manifest interface
func (m *KonnManifest) SetSource(src *url.URL) { m.val.Source = src.String() }

// Source is part of the Manifest interface
func (m *KonnManifest) Source() string { return m.val.Source }

// Version is part of the Manifest interface
func (m *KonnManifest) Version() string { return m.val.Version }

// AvailableVersion is part of the Manifest interface
func (m *KonnManifest) AvailableVersion() string { return m.val.AvailableVersion }

// Checksum is part of the Manifest interface
func (m *KonnManifest) Checksum() string { return m.val.Checksum }

// Slug is part of the Manifest interface
func (m *KonnManifest) Slug() string { return m.val.Slug }

// State is part of the Manifest interface
func (m *KonnManifest) State() State { return m.val.State }

// LastUpdate is part of the Manifest interface
func (m *KonnManifest) LastUpdate() time.Time { return m.val.UpdatedAt }

// SetState is part of the Manifest interface
func (m *KonnManifest) SetState(state State) { m.val.State = state }

// SetVersion is part of the Manifest interface
func (m *KonnManifest) SetVersion(version string) { m.val.Version = version }

// SetAvailableVersion is part of the Manifest interface
func (m *KonnManifest) SetAvailableVersion(version string) { m.val.AvailableVersion = version }

// SetChecksum is part of the Manifest interface
func (m *KonnManifest) SetChecksum(shasum string) { m.val.Checksum = shasum }

// AppType is part of the Manifest interface
func (m *KonnManifest) AppType() consts.AppType { return consts.KonnectorType }

// Terms is part of the Manifest interface
func (m *KonnManifest) Terms() Terms { return m.val.Terms }

// Permissions is part of the Manifest interface
func (m *KonnManifest) Permissions() permission.Set { return m.val.Permissions }

// SetError is part of the Manifest interface
func (m *KonnManifest) SetError(err error) {
	m.SetState(Errored)
	m.val.Err = err.Error()
	m.err = err
}

// Error is part of the Manifest interface
func (m *KonnManifest) Error() error { return m.err }

// Fetch is part of the Manifest interface
func (m *KonnManifest) Fetch(field string) []string {
	switch field {
	case "slug":
		return []string{m.val.Slug}
	case "state":
		return []string{string(m.val.State)}
	}
	return nil
}

// Notifications returns the notifications properties for this konnector.
func (m *KonnManifest) Notifications() Notifications {
	return m.val.Notifications
}

// Parameters returns the parameters for executing the konnector.
func (m *KonnManifest) Parameters() map[string]interface{} {
	return m.val.Parameters
}

// Name returns the konnector name.
func (m *KonnManifest) Name() string { return m.val.Name }

// Icon returns the konnector icon path.
func (m *KonnManifest) Icon() string { return m.val.Icon }

// Language returns the programming language used for executing the konnector
// (only "node" for the moment).
func (m *KonnManifest) Language() string { return m.val.Language }

// ClientSide returns true for a konnector that runs on the client (flagship
// app), and false for a konnector that runs on the server (nodejs executed by
// the stack).
func (m *KonnManifest) ClientSide() bool { return m.val.ClientSide }

// OnDeleteAccount can be used to specify a file path which will be executed
// when an account associated with the konnector is deleted.
func (m *KonnManifest) OnDeleteAccount() string { return m.val.OnDeleteAccount }

// VendorLink returns the vendor link.
func (m *KonnManifest) VendorLink() interface{} {
	return m.doc.M["vendor_link"]
}

func (m *KonnManifest) MarshalJSON() ([]byte, error) {
	doc := m.doc.Clone().(*couchdb.JSONDoc)
	doc.Type = consts.Konnectors
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
	if m.val.Parameters == nil {
		delete(doc.M, "parameters")
	} else {
		doc.M["parameters"] = m.val.Parameters
	}
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
	return json.Marshal(doc)
}

func (m *KonnManifest) UnmarshalJSON(j []byte) error {
	if err := json.Unmarshal(j, &m.doc); err != nil {
		return err
	}
	if err := json.Unmarshal(j, &m.val); err != nil {
		return err
	}
	// Keys match case-insensitively (even "ſ" for "s"), so a key from the
	// package could override the source set by the installer: use the exact one.
	m.val.Source, _ = m.doc.M["source"].(string)
	return nil
}

// ReadManifest is part of the Manifest interface
func (m *KonnManifest) ReadManifest(r io.Reader, slug, sourceURL string) (Manifest, error) {
	var newManifest KonnManifest
	if err := json.NewDecoder(r).Decode(&newManifest); err != nil {
		return nil, ErrBadManifest
	}

	newManifest.SetID(consts.Konnectors + "/" + slug)
	newManifest.SetRev(m.Rev())
	newManifest.SetState(m.State())
	newManifest.val.CreatedAt = m.val.CreatedAt
	newManifest.val.Slug = slug
	newManifest.val.Source = sourceURL
	if newManifest.val.Parameters == nil {
		newManifest.val.Parameters = m.val.Parameters
	}

	return &newManifest, nil
}

// Create is part of the Manifest interface
func (m *KonnManifest) Create(db prefixer.Prefixer) error {
	m.SetID(consts.Konnectors + "/" + m.Slug())
	m.val.CreatedAt = time.Now()
	m.val.UpdatedAt = time.Now()
	if err := couchdb.CreateNamedDocWithDB(db, m); err != nil {
		return err
	}

	_, err := permission.CreateKonnectorSet(db, m.Slug(), m.Permissions(), m.Version())
	return err
}

// Update is part of the Manifest interface
func (m *KonnManifest) Update(db prefixer.Prefixer, extraPerms permission.Set) error {
	m.val.UpdatedAt = time.Now()
	err := couchdb.UpdateDoc(db, m)
	if err != nil {
		return err
	}

	perms := m.Permissions()

	// Merging the potential extra permissions
	if len(extraPerms) > 0 {
		perms, err = permission.MergeExtraPermissions(perms, extraPerms)
		if err != nil {
			return err
		}
	}
	_, err = permission.UpdateKonnectorSet(db, m.Slug(), perms)
	return err
}

// Delete is part of the Manifest interface
func (m *KonnManifest) Delete(db prefixer.Prefixer) error {
	err := permission.DestroyKonnector(db, m.Slug())
	if err != nil && !couchdb.IsNotFoundError(err) {
		return err
	}
	return couchdb.DeleteDoc(db, m)
}

// BuildTrigger builds a @cron trigger with the parameter from the konnector
// manifest (not yet persisted in CouchDB).
func (m *KonnManifest) BuildTrigger(db prefixer.Prefixer, accountID, createdByApp string) (job.Trigger, error) {
	var md *metadata.CozyMetadata
	if createdByApp == "" {
		md = metadata.New()
	} else {
		var err error
		md, err = metadata.NewWithApp(createdByApp, "", job.DocTypeVersionTrigger)
		if err != nil {
			return nil, err
		}
	}
	md.DocTypeVersion = "1"
	data := map[string]interface{}{
		"account":   accountID,
		"konnector": m.Slug(),
	}
	if m.hasFolderPath() {
		// XXX in theory, it is an ID, but we just put the yes string and let
		// the worker change it to the folder ID on the first run.
		data["folder_to_save"] = "yes"
	}
	msg, err := job.NewMessage(data)
	if err != nil {
		return nil, err
	}
	crontab := m.triggerCrontab()
	return job.NewCronTrigger(&job.TriggerInfos{
		Type:       "@cron",
		WorkerType: "konnector",
		Domain:     db.DomainName(),
		Prefix:     db.DBPrefix(),
		Arguments:  crontab,
		Message:    msg,
		Metadata:   md,
	})
}

// CreateTrigger creates a @cron trigger with the parameter from the konnector
// manifest (persisted in CouchDB).
func (m *KonnManifest) CreateTrigger(db prefixer.Prefixer, accountID, createdByApp string) (job.Trigger, error) {
	t, err := m.BuildTrigger(db, accountID, createdByApp)
	if err != nil {
		return nil, err
	}
	sched := job.System()
	if err = sched.AddTrigger(t); err != nil {
		return nil, err
	}
	return t, nil
}

func (m *KonnManifest) triggerCrontab() string {
	spec := job.NewPeriodicSpec()

	freq, _ := m.doc.M["frequency"].(string)
	switch freq {
	case "hourly":
		spec.Frequency = job.HourlyKind
	case "daily":
		spec.Frequency = job.DailyKind
	case "monthly":
		spec.Frequency = job.MonthlyKind
	default: // weekly
		spec.Frequency = job.WeeklyKind
	}

	min, max := 0, 5 // By default konnectors are run at random hour between 12:00PM and 05:00AM
	interval, ok := m.doc.M["time_interval"].([]int)
	if ok && len(interval) == 2 {
		min = interval[0]
		if interval[1] > min {
			max = interval[1]
		}
	}
	spec.AfterHour = min
	spec.BeforeHour = max

	return spec.ToRandomCrontab(m.Slug())
}

// Cf https://github.com/cozy/cozy-libs/blob/55b5f23f0adbc308c3b70fa287c3938ee1b0a4cc/packages/cozy-harvest-lib/src/helpers/konnectors.js#L213-L225
func (m *KonnManifest) hasFolderPath() bool {
	if _, ok := m.doc.M["folders"].([]interface{}); ok {
		return true
	}
	fields, ok := m.doc.M["fields"].(map[string]interface{})
	if !ok {
		return false
	}
	advanced, ok := fields["advanced_fields"].(map[string]interface{})
	if !ok {
		return false
	}
	return advanced["folderPath"] != nil
}

// GetKonnectorBySlug fetch the manifest of a konnector from the database given
// a slug.
func GetKonnectorBySlug(db prefixer.Prefixer, slug string) (*KonnManifest, error) {
	if slug == "" || !slugReg.MatchString(slug) {
		return nil, ErrInvalidSlugName
	}
	doc := &KonnManifest{}
	err := couchdb.GetDoc(db, consts.Konnectors, consts.Konnectors+"/"+slug, doc)
	if couchdb.IsNotFoundError(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return doc, nil
}

// GetKonnectorBySlugAndUpdate fetch the KonnManifest and perform an update of
// the konnector if necessary and if the konnector was installed from the
// registry.
func GetKonnectorBySlugAndUpdate(in *instance.Instance, slug string, copier appfs.Copier, registries []*url.URL) (*KonnManifest, error) {
	man, err := GetKonnectorBySlug(in, slug)
	if err != nil {
		return nil, err
	}
	return DoLazyUpdate(in, man, copier, registries).(*KonnManifest), nil
}

// ListKonnectorsWithPagination returns the list of installed konnectors with a
// pagination
func ListKonnectorsWithPagination(db prefixer.Prefixer, limit int, startKey string) ([]*KonnManifest, string, error) {
	var docs []*KonnManifest

	if limit == 0 {
		limit = defaultAppListLimit
	}

	req := &couchdb.AllDocsRequest{
		Limit:    limit + 1, // Also get the following document for the next key
		StartKey: startKey,
	}
	err := couchdb.GetAllDocs(db, consts.Konnectors, req, &docs)
	if err != nil {
		return nil, "", err
	}

	nextID := ""
	if len(docs) > 0 && len(docs) == limit+1 { // There are still documents to fetch
		nextDoc := docs[len(docs)-1]
		nextID = nextDoc.ID()
		docs = docs[:len(docs)-1]
	}

	return docs, nextID, nil
}

var _ Manifest = &KonnManifest{}
