package move

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/safehttp"
	multierror "github.com/hashicorp/go-multierror"
)

type importer struct {
	inst            *instance.Instance
	fs              vfs.VFS
	options         ImportOptions
	doc             *ExportDoc
	servicesInError map[string]bool // a map, not a slice, to have unique values
	tmpFile         string
	doctype         string
	docs            []interface{}
	triggers        []*job.TriggerInfos
}

func (im *importer) importPart(cursor string) error {
	defer func() {
		if im.tmpFile != "" {
			if err := os.Remove(im.tmpFile); err != nil {
				im.inst.Logger().WithNamespace("move").
					Warnf("Cannot remove temp file %s: %s", im.tmpFile, err)
			}
		}
	}()
	if err := im.downloadFile(cursor); err != nil {
		return err
	}
	zr, err := zip.OpenReader(im.tmpFile)
	if err != nil {
		return err
	}
	err = im.importZip(&zr.Reader)
	if errc := zr.Close(); err == nil {
		err = errc
	}
	return err
}

func (im *importer) downloadFile(cursor string) error {
	u, err := url.Parse(im.options.ManifestURL)
	if err != nil {
		return err
	}
	u.Path = strings.Replace(u.Path, "/move/exports/", "/move/exports/data/", 1)
	if cursor != "" {
		u.RawQuery = url.Values{"cursor": {cursor}}.Encode()
	}
	res, err := safehttp.ClientWithKeepAlive.Get(u.String())
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return ErrExportNotFound
	}
	f, err := os.CreateTemp("", "export-*")
	if err != nil {
		return err
	}
	im.tmpFile = f.Name()
	_, err = io.Copy(f, res.Body)
	if errc := f.Close(); err == nil {
		err = errc
	}
	return err
}

func (im *importer) importZip(zr *zip.Reader) error {
	var errm error

	for i, file := range zr.File {
		if !strings.HasPrefix(file.FileHeader.Name, ExportDataDir+"/") {
			continue
		}
		name := strings.TrimPrefix(file.FileHeader.Name, ExportDataDir+"/")
		parts := strings.SplitN(name, "/", 2)
		if len(parts) != 2 {
			continue // "instance.json" for example
		}
		doctype := parts[0]
		id := strings.TrimSuffix(parts[1], ".json")

		// Special cases
		switch doctype {
		case consts.Exports:
			// Importing exports would just be a mess, so skip them
			continue
		case consts.Sessions:
			// We don't want to import the sessions from another instance
			continue
		case consts.BitwardenCiphers, consts.BitwardenFolders, consts.BitwardenProfiles,
			consts.BitwardenOrganizations, consts.BitwardenContacts:
			// Bitwarden documents are encypted E2E, so they cannot be imported
			// as raw documents
			continue
		case consts.Sharings:
			// Sharings are imported only for a move
			if im.options.MoveFrom == nil {
				continue
			}
			if err := im.importSharing(file); err != nil {
				errm = multierror.Append(errm, err)
			}
			continue
		case consts.Shared:
			if im.options.MoveFrom == nil {
				continue
			}
		case consts.Permissions:
			if im.options.MoveFrom == nil {
				continue
			}
			if err := im.importPermission(file); err != nil {
				errm = multierror.Append(errm, err)
			}
			continue
		case consts.Settings:
			// Keep the email, public name and stuff related to the cloudery
			// from the destination Cozy. Same for the bitwarden settings
			// derived from the passphrase.
			if id == consts.InstanceSettingsID || id == consts.BitwardenSettingsID {
				continue
			}
		case consts.Apps, consts.Konnectors:
			im.installApp(id)
			continue
		case consts.Accounts:
			if err := im.importAccount(file); err != nil {
				errm = multierror.Append(errm, err)
			}
			continue
		case consts.Triggers:
			if err := im.importTrigger(file); err != nil {
				errm = multierror.Append(errm, err)
			}
			continue
		case consts.Files:
			var content *zip.File
			if i < len(zr.File)-1 {
				content = zr.File[i+1]
			}
			if err := im.importFile(file, content); err != nil {
				errm = multierror.Append(errm, err)
			}
			continue
		case consts.FilesVersions:
			if i >= len(zr.File)-1 {
				continue
			}
			if err := im.importFileVersion(file, zr.File[i+1]); err != nil {
				errm = multierror.Append(errm, err)
			}
			continue
		}

		// Normal documents
		if doctype != im.doctype || len(im.docs) >= 100 {
			if err := im.flush(); err != nil {
				errm = multierror.Append(errm, err)
				im.docs = nil
				im.doctype = ""
			}
		}
		doc, err := im.readDoc(file)
		if err != nil {
			errm = multierror.Append(errm, err)
			continue
		}
		delete(doc, "_rev")
		im.doctype = doctype
		im.docs = append(im.docs, doc)
	}

	if err := im.flush(); err != nil {
		errm = multierror.Append(errm, err)
	}

	// Import the triggers at the end to avoid creating many jobs when
	// importing the files.
	if err := im.importTriggers(); err != nil {
		errm = multierror.Append(errm, err)
	}

	// Reinject the email address from the destination Cozy in the myself
	// contact document
	if myself, err := contact.GetMyself(im.inst); err == nil {
		if email, err := im.inst.SettingsEMail(); err == nil && email != "" {
			addr, _ := myself.ToMailAddress()
			if addr == nil || addr.Email != email {
				myself.JSONDoc.M["email"] = []map[string]interface{}{
					{
						"address": email,
						"primary": true,
					},
				}
				_ = couchdb.UpdateDoc(im.inst, myself)
			}
		}
	}
	return errm
}

func (im *importer) flush() error {
	if len(im.docs) == 0 {
		return nil
	}

	olds := make([]interface{}, len(im.docs))
	if err := couchdb.BulkUpdateDocs(im.inst, im.doctype, im.docs, olds); err != nil {
		// XXX CouchDB can be overloaded sometimes when importing lots of documents.
		// Let's wait a bit and retry...
		for i := 0; i < 12; i++ {
			time.Sleep(5 * time.Minute)
			err = couchdb.BulkUpdateDocs(im.inst, im.doctype, im.docs, olds)
			if err == nil {
				break
			}
		}
		if err != nil {
			return err
		}
	}
	// XXX Not too fast, CouchDB can be easily overloaded...
	time.Sleep(100 * time.Millisecond)

	im.doctype = ""
	im.docs = nil
	return nil
}

func (im *importer) readDoc(zf *zip.File) (map[string]interface{}, error) {
	r, err := zf.Open()
	if err != nil {
		return nil, err
	}
	var doc map[string]interface{}
	err = json.NewDecoder(r).Decode(&doc)
	if errc := r.Close(); errc != nil {
		return nil, errc
	}
	if err != nil {
		return nil, err
	}
	return doc, nil
}

func (im *importer) importAccount(zf *zip.File) error {
	doc, err := im.readDoc(zf)
	if err != nil {
		return err
	}

	// Note: the slug will be empty for aggregator accounts, and it won't be
	// imported as an aggregator account is used by other accounts with a hook
	// on deletion.
	slug, _ := doc["account_type"].(string)
	man, err := app.GetKonnectorBySlug(im.inst, slug)
	if errors.Is(err, app.ErrNotFound) {
		im.installApp(consts.Konnectors + "/" + slug)
		man, err = app.GetKonnectorBySlug(im.inst, slug)
	}
	if err != nil || man.OnDeleteAccount() != "" {
		im.servicesInError[slug] = true
		return nil
	}

	docs := []interface{}{doc}
	olds := make([]interface{}, len(docs))
	if err := couchdb.EnsureDBExist(im.inst, consts.Accounts); err != nil {
		return err
	}
	return couchdb.BulkUpdateDocs(im.inst, consts.Accounts, docs, olds)
}

func (im *importer) readTrigger(zf *zip.File) (*job.TriggerInfos, error) {
	r, err := zf.Open()
	if err != nil {
		return nil, err
	}
	doc := &job.TriggerInfos{}
	err = json.NewDecoder(r).Decode(doc)
	if errc := r.Close(); errc != nil {
		return nil, errc
	}
	if err != nil {
		return nil, err
	}
	return doc, nil
}

func (im *importer) importTrigger(zf *zip.File) error {
	doc, err := im.readTrigger(zf)
	if err != nil {
		return err
	}
	switch doc.WorkerType {
	case "share-track", "share-replicate", "share-upload":
		// The share-* triggers are imported only for a move
		if im.options.MoveFrom == nil {
			return nil
		}
	case "konnector":
		// OK, import it
	default:
		return nil
	}
	// We don't import triggers now, but wait after files has been imported to
	// avoid creating many jobs when importing shared files.
	im.triggers = append(im.triggers, doc)
	return nil
}

func (im *importer) importTriggers() error {
	var errm error
	for _, doc := range im.triggers {
		doc.SetRev("")
		t, err := job.NewTrigger(im.inst, *doc, nil)
		if err != nil {
			errm = multierror.Append(errm, err)
			continue
		}
		if err = job.System().AddTrigger(t); err != nil && !couchdb.IsConflictError(err) {
			errm = multierror.Append(errm, err)
		}
	}
	return errm
}

func (im *importer) installApp(id string) {
	parts := strings.SplitN(id, "/", 2)
	if len(parts) != 2 {
		return
	}
	doctype := parts[0]
	apptype := consts.WebappType
	if doctype == consts.Konnectors {
		apptype = consts.KonnectorType
	}
	slug := parts[1]
	source := "registry://" + slug + "/stable"

	installer, err := app.NewInstaller(im.inst, app.Copier(apptype, im.inst),
		&app.InstallerOptions{
			Operation:  app.Install,
			Type:       apptype,
			SourceURL:  source,
			Slug:       slug,
			Registries: im.inst.Registries(),
		},
	)
	if err == nil {
		_, err = installer.RunSync()
	}
	if err != nil && !errors.Is(err, app.ErrAlreadyExists) {
		im.servicesInError[slug] = true
	}
}

func (im *importer) importFile(zdoc, zcontent *zip.File) error {
	doc, err := im.readFileDoc(zdoc)
	if err != nil {
		return err
	}
	dirDoc, fileDoc := doc.Refine()
	if dirDoc != nil {
		dirDoc.SetRev("")
		if dirDoc.DocID == consts.RootDirID || dirDoc.DocID == consts.TrashDirID {
			return nil
		}
		return im.fs.CreateDir(dirDoc)
	}

	if zcontent == nil {
		return errors.New("No content for file")
	}
	fileDoc.SetRev("")
	// Do not trust carbon copy and electronic safe flags on import
	if fileDoc.Metadata != nil {
		delete(fileDoc.Metadata, consts.CarbonCopyKey)
		delete(fileDoc.Metadata, consts.ElectronicSafeKey)
	}
	f, err := im.fs.CreateFile(fileDoc, nil, vfs.AllowCreationInTrash)
	if err != nil {
		return err
	}

	content, err := zcontent.Open()
	if err != nil {
		return err
	}
	_, err = io.Copy(f, content)
	if errc := f.Close(); err == nil {
		err = errc
	}
	return err
}

func (im *importer) readFileDoc(zf *zip.File) (*vfs.DirOrFileDoc, error) {
	r, err := zf.Open()
	if err != nil {
		return nil, err
	}
	var doc vfs.DirOrFileDoc
	err = json.NewDecoder(r).Decode(&doc)
	if errc := r.Close(); errc != nil {
		return nil, errc
	}
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

func (im *importer) importFileVersion(zdoc, zcontent *zip.File) error {
	doc, err := im.readVersion(zdoc)
	if err != nil {
		return err
	}
	content, err := zcontent.Open()
	if err != nil {
		return err
	}
	doc.SetRev("")
	return im.fs.ImportFileVersion(doc, content)
}

func (im *importer) readVersion(zf *zip.File) (*vfs.Version, error) {
	r, err := zf.Open()
	if err != nil {
		return nil, err
	}
	var doc vfs.Version
	err = json.NewDecoder(r).Decode(&doc)
	if errc := r.Close(); errc != nil {
		return nil, errc
	}
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

func (im *importer) importSharing(zf *zip.File) error {
	s, err := im.readSharing(zf)
	if err != nil {
		return err
	}
	// XXX Do not import sharing for bitwarden stuff
	if s.FirstBitwardenOrganizationRule() != nil {
		return nil
	}
	s.Initial = false
	s.NbFiles = 0
	s.UpdatedAt = time.Now()
	s.SetRev("")
	if s.Owner {
		s.MovedFrom = s.Members[0].Instance
		s.Members[0].Instance = im.inst.PageURL("", nil)
	} else {
		targetURL := strings.TrimSuffix(im.options.MoveFrom.URL, "/")
		for i, m := range s.Members {
			if m.Instance == targetURL {
				s.MovedFrom = s.Members[i].Instance
				s.Members[i].Instance = im.inst.PageURL("", nil)
			}
		}
	}
	return couchdb.CreateNamedDoc(im.inst, s)
}

func (im *importer) readSharing(zf *zip.File) (*sharing.Sharing, error) {
	r, err := zf.Open()
	if err != nil {
		return nil, err
	}
	doc := &sharing.Sharing{}
	err = json.NewDecoder(r).Decode(doc)
	if errc := r.Close(); errc != nil {
		return nil, errc
	}
	if err != nil {
		return nil, err
	}
	return doc, nil
}

func (im *importer) importPermission(zf *zip.File) error {
	doc, err := im.readPermission(zf)
	if err != nil {
		return err
	}
	// We only import permission documents for sharings
	if doc.Type != permission.TypeShareByLink && doc.Type != permission.TypeSharePreview {
		return nil
	}
	// We need to remake the long codes with the new instance domain
	for name := range doc.Codes {
		longcode, err := im.inst.CreateShareCode(name, doc.ID())
		if err != nil {
			return err
		}
		doc.Codes[name] = longcode
	}
	doc.SetRev("")
	return couchdb.CreateNamedDoc(im.inst, doc)
}

func (im *importer) readPermission(zf *zip.File) (*permission.Permission, error) {
	r, err := zf.Open()
	if err != nil {
		return nil, err
	}
	doc := &permission.Permission{}
	err = json.NewDecoder(r).Decode(doc)
	if errc := r.Close(); errc != nil {
		return nil, errc
	}
	if err != nil {
		return nil, err
	}
	return doc, nil
}
