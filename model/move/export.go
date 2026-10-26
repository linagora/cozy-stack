package move

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"path"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/note"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/mail"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/realtime"
)

// ExportOptions contains the options for launching the export worker.
type ExportOptions struct {
	PartsSize        int64          `json:"parts_size"`
	MaxAge           time.Duration  `json:"max_age"`
	WithDoctypes     []string       `json:"with_doctypes,omitempty"`
	ContextualDomain string         `json:"contextual_domain,omitempty"`
	TokenSource      string         `json:"token_source,omitempty"`
	IgnoreVault      bool           `json:"ignore_vault,omitempty"`
	MoveTo           *MoveToOptions `json:"move_to,omitempty"`
	AdminReq         bool           `json:"admin_req,omitempty"`
}

// MoveToOptions is used when the export must be sent to another Cozy.
type MoveToOptions struct {
	URL          string `json:"url"`
	Token        string `json:"token"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// ImportsURL returns the URL on the target for sending the download link to
// the export tarballs.
func (m *MoveToOptions) ImportsURL() string {
	u, err := url.Parse(m.URL)
	if err != nil {
		u, err = url.Parse("https://" + m.URL)
	}
	if err != nil {
		return m.URL
	}
	u.Path = "/move/imports"
	return u.String()
}

// minimalPartsSize is the minimal size of a file bucket, to split the index
// into equal-sized parts.
const minimalPartsSize = 1024 * 1024 * 1024 // 1 GB

const (
	// ExportDataDir is the directory for storing the documents from CouchDB in
	// the export archive.
	ExportDataDir = "My Cozy/Data"
	// ExportFilesDir is the directory for storing the content of the files in
	// the export archive.
	ExportFilesDir = "My Cozy/Files"
	// ExportVersionsDir is the directory for storing the content of the old
	// versions of the files in the export archive.
	ExportVersionsDir = "My Cozy/Versions"
)

// ExportCopyData does an HTTP copy of a part of the file indexes.
func ExportCopyData(w io.Writer, inst *instance.Instance, exportDoc *ExportDoc, archiver Archiver, cursor Cursor) error {
	zw := zip.NewWriter(w)
	defer func() {
		_ = zw.Close()
	}()

	if cursor.Number == 0 {
		err := copyJSONData(zw, inst, exportDoc, archiver)
		if err != nil {
			return err
		}
	}

	if exportDoc.AcceptDoctype(consts.Files) {
		if err := copyFiles(zw, inst, exportDoc, cursor); err != nil {
			return err
		}
	}

	if exportDoc.AcceptDoctype(consts.FilesVersions) {
		if err := copyVersions(zw, inst, exportDoc, cursor); err != nil {
			return err
		}
	}

	return nil
}

func copyJSONData(zw *zip.Writer, inst *instance.Instance, exportDoc *ExportDoc, archiver Archiver) error {
	archive, err := archiver.OpenArchive(inst, exportDoc)
	if err != nil {
		return err
	}
	defer func() {
		_ = archive.Close()
	}()

	gr, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	now := time.Now()
	tr := tar.NewReader(gr)
	defer func() {
		_ = gr.Close()
	}()

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}

		zipHeader := &zip.FileHeader{
			Name:     path.Join(ExportDataDir, header.Name),
			Method:   zip.Deflate,
			Modified: now,
		}
		zipHeader.SetMode(0640)
		zipFileWriter, err := zw.CreateHeader(zipHeader)
		if err != nil {
			return err
		}
		_, err = io.Copy(zipFileWriter, tr)
		if err != nil {
			return err
		}
	}

	return nil
}

func copyFiles(zw *zip.Writer, inst *instance.Instance, exportDoc *ExportDoc, cursor Cursor) error {
	files, err := listFilesFromCursor(inst, exportDoc, cursor)
	if err != nil {
		return err
	}

	fs := inst.VFS()
	filepather := vfs.NewFilePatherWithCache(fs)

	for _, file := range files {
		metaHeader := &zip.FileHeader{
			Name:     path.Join(ExportDataDir, consts.Files, file.DocID+".json"),
			Method:   zip.Deflate,
			Modified: file.UpdatedAt,
		}
		metaHeader.SetMode(0640)
		metaWriter, err := zw.CreateHeader(metaHeader)
		if err != nil {
			return err
		}
		doc, err := json.Marshal(file)
		if err != nil {
			return err
		}
		if _, err = metaWriter.Write(doc); err != nil {
			return err
		}

		f, err := fs.OpenFile(file)
		if err != nil {
			// Ignore missing file, as it may happen that a file is deleted
			// while an export is running as we are not always locking the
			// VFS or blocking the instance (or the file system is not clean)
			continue
		}
		defer func() {
			_ = f.Close()
		}()
		fullpath, err := file.Path(filepather)
		if err != nil {
			return err
		}
		fileHeader := &zip.FileHeader{
			Name:     path.Join(ExportFilesDir, fullpath),
			Method:   zip.Deflate,
			Modified: file.UpdatedAt,
		}
		if file.Executable {
			fileHeader.SetMode(0750)
		} else {
			fileHeader.SetMode(0640)
		}
		zipFileWriter, err := zw.CreateHeader(fileHeader)
		if err != nil {
			return err
		}
		_, err = io.Copy(zipFileWriter, f)
		if err != nil {
			return err
		}
	}

	return nil
}

func copyVersions(zw *zip.Writer, inst *instance.Instance, exportDoc *ExportDoc, cursor Cursor) error {
	versions, err := listVersionsFromCursor(inst, exportDoc, cursor)
	if err != nil {
		return err
	}

	fs := inst.VFS()
	finder := newFileFinderWithCache(fs)

	for _, version := range versions {
		metaHeader := &zip.FileHeader{
			Name:     path.Join(ExportDataDir, consts.FilesVersions, version.DocID+".json"),
			Method:   zip.Deflate,
			Modified: version.UpdatedAt,
		}
		metaHeader.SetMode(0640)
		metaWriter, err := zw.CreateHeader(metaHeader)
		if err != nil {
			return err
		}
		doc, err := json.Marshal(version)
		if err != nil {
			return err
		}
		if _, err = metaWriter.Write(doc); err != nil {
			return err
		}

		file, err := finder.Find(version.DocID)
		if err != nil {
			// Ignore missing file, as it may happen that a file is deleted
			// while an export is running as we are not always locking the
			// VFS or blocking the instance (or the file system is not clean)
			continue
		}

		f, err := fs.OpenFileVersion(file, version)
		if err != nil {
			// Ignore missing version, as it may happen that a version is
			// deleted while an export is running as we are not always locking
			// the VFS or blocking the instance (or the file system is not clean)
			continue
		}
		defer func() {
			_ = f.Close()
		}()
		fileHeader := &zip.FileHeader{
			Name:     path.Join(ExportFilesDir, version.DocID),
			Method:   zip.Deflate,
			Modified: version.UpdatedAt,
		}
		fileHeader.SetMode(0640)
		zipFileWriter, err := zw.CreateHeader(fileHeader)
		if err != nil {
			return err
		}
		_, err = io.Copy(zipFileWriter, f)
		if err != nil {
			return err
		}
	}

	return nil
}

// CreateExport is used to create a tarball with the data from an instance.
//
// Note: the tarball is a .tar.gz and not a .zip to allow streaming from Swift
// to the stack, and from the stack to the client, as .tar.gz can be read
// sequentially and reading a .zip need to seek.
func CreateExport(i *instance.Instance, opts ExportOptions, archiver Archiver) (*ExportDoc, error) {
	exportDoc := prepareExportDoc(i, opts)
	if err := exportDoc.CleanPreviousExports(archiver); err != nil {
		return nil, err
	}

	if err := couchdb.CreateDoc(prefixer.GlobalPrefixer, exportDoc); err != nil {
		return nil, err
	}
	realtime.GetHub().Publish(i, realtime.EventCreate, exportDoc.Clone(), nil)

	size, err := writeArchive(i, exportDoc, archiver)
	old := exportDoc.Clone()
	errf := exportDoc.MarksAsFinished(i, size, err)
	realtime.GetHub().Publish(i, realtime.EventUpdate, exportDoc, old)
	if err != nil {
		return nil, err
	}
	return exportDoc, errf
}

func writeArchive(i *instance.Instance, exportDoc *ExportDoc, archiver Archiver) (int64, error) {
	out, err := archiver.CreateArchive(exportDoc)
	if err != nil {
		return 0, err
	}
	size, err := writeArchiveContent(i, exportDoc, out)
	if err != nil {
		return 0, err
	}
	return size, out.Close()
}

func writeArchiveContent(i *instance.Instance, exportDoc *ExportDoc, out io.Writer) (int64, error) {
	gw, err := gzip.NewWriterLevel(out, gzip.BestCompression)
	if err != nil {
		return 0, err
	}
	tw := tar.NewWriter(gw)
	size, err := writeDocuments(i, exportDoc, tw)
	if err != nil {
		return 0, err
	}
	if err := tw.Close(); err != nil {
		return 0, err
	}
	if err := gw.Close(); err != nil {
		return 0, err
	}
	return size, nil
}

func writeDocuments(i *instance.Instance, exportDoc *ExportDoc, tw *tar.Writer) (int64, error) {
	var size int64
	createdAt := exportDoc.CreatedAt

	n, err := writeInstanceDoc(i, "instance", createdAt, tw)
	if err != nil {
		return 0, err
	}
	size += n

	n, err = exportDocuments(i, exportDoc, createdAt, tw)
	if err != nil {
		return 0, err
	}
	size += n

	if exportDoc.AcceptDoctype(consts.Files) {
		n, err := exportFiles(i, exportDoc, tw)
		if err != nil {
			return 0, err
		}
		size += n
	}

	return size, nil
}

func exportFiles(i *instance.Instance, exportDoc *ExportDoc, tw *tar.Writer) (int64, error) {
	_ = note.FlushPendings(i)

	var size int64
	filesizes := make(map[string]int64)
	err := vfs.Walk(i.VFS(), "/", func(fullpath string, dir *vfs.DirDoc, file *vfs.FileDoc, err error) error {
		if err != nil {
			return err
		}
		if dir != nil {
			n, err := writeDoc(consts.Files, dir.DocID, dir, exportDoc.CreatedAt, tw)
			size += n
			return err
		}
		filesizes[file.DocID] = file.ByteSize
		return nil
	})
	if err != nil {
		return 0, err
	}

	versionsizes := make(map[string]int64)
	err = couchdb.ForeachDocs(i, consts.FilesVersions, func(id string, raw json.RawMessage) error {
		var doc vfs.Version
		if err := json.Unmarshal(raw, &doc); err != nil {
			return err
		}
		versionsizes[id] = doc.ByteSize
		return nil
	})
	if err != nil {
		return 0, err
	}

	remaining := exportDoc.PartsSize
	var cursors []string
	cursors, remaining = splitFiles(exportDoc.PartsSize, remaining, filesizes, consts.Files)
	exportDoc.PartsCursors = cursors
	cursors, _ = splitFiles(exportDoc.PartsSize, remaining, versionsizes, consts.FilesVersions)
	if len(cursors) > 0 {
		exportDoc.PartsCursors = append(exportDoc.PartsCursors, cursors...)
	}
	return size, nil
}

func exportDocuments(in *instance.Instance, doc *ExportDoc, now time.Time, tw *tar.Writer) (int64, error) {
	doctypes, err := couchdb.AllDoctypes(in)
	if err != nil {
		return 0, err
	}

	var size int64
	for _, doctype := range doctypes {
		if !doc.AcceptDoctype(doctype) {
			continue
		}
		switch doctype {
		case consts.Files, consts.FilesVersions:
			// we have code specific to those doctypes
			continue
		case consts.FilesRecents:
			// Recents are personal activity, not user data to export
			continue
		}
		dir := url.PathEscape(doctype)
		err := couchdb.ForeachDocs(in, doctype, func(id string, doc json.RawMessage) error {
			n, err := writeMarshaledDoc(dir, id, doc, now, tw)
			if err == nil {
				size += n
			}
			return err
		})
		if err != nil {
			return 0, err
		}
	}
	return size, nil
}

func writeInstanceDoc(in *instance.Instance, name string, now time.Time, tw *tar.Writer) (int64, error) {
	clone := in.Clone().(*instance.Instance)
	clone.PassphraseHash = nil
	clone.PassphraseResetToken = nil
	clone.PassphraseResetTime = nil
	clone.RegisterToken = nil
	clone.SessSecret = nil
	clone.OAuthSecret = nil
	clone.CLISecret = nil
	clone.SwiftLayout = 0
	clone.CouchCluster = 0
	clone.IndexViewsVersion = 0
	return writeDoc("", name, clone, now, tw)
}

func writeDoc(dir, name string, data interface{}, now time.Time, tw *tar.Writer) (int64, error) {
	doc, err := json.Marshal(data)
	if err != nil {
		return 0, err
	}
	return writeMarshaledDoc(dir, name, doc, now, tw)
}

func writeMarshaledDoc(dir, name string, doc json.RawMessage, now time.Time, tw *tar.Writer) (int64, error) {
	if tw == nil { // For testing purpose
		return 1, nil
	}

	hdr := &tar.Header{
		Name:     path.Join(dir, name+".json"),
		Mode:     0640,
		Size:     int64(len(doc)),
		Typeflag: tar.TypeReg,
		ModTime:  now,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return 0, err
	}
	n, err := tw.Write(doc)
	return int64(n), err
}

// SendExportFailureMail sends an email to the user when the export has failed.
func SendExportFailureMail(inst *instance.Instance) error {
	email := mail.Options{
		Mode:         mail.ModeFromStack,
		TemplateName: "export_error",
	}
	msg, err := job.NewMessage(&email)
	if err != nil {
		return err
	}
	_, err = job.System().PushJob(inst, &job.JobRequest{
		WorkerType: "sendmail",
		Message:    msg,
	})
	return err
}
