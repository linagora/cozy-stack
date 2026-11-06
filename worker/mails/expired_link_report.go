package mails

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/mail"
)

var errReportCancelled = errors.New("link report cancelled")

func init() {
	job.AddWorker(&job.WorkerConfig{
		WorkerType: "expired-link-report", WorkerFunc: expiredLinkReport,
		Reserved: true, Concurrency: 2, MaxExecCount: 1, Timeout: 10 * time.Minute,
	})
}

func expiredLinkReport(ctx *job.TaskContext) error {
	var msg publiclink.ReportMessage
	if err := ctx.UnmarshalMessage(&msg); err != nil {
		return errors.New("invalid link report job")
	}
	if msg.LinkID == "" || msg.Expiry.IsZero() {
		return errors.New("invalid link report identifiers")
	}
	inst := ctx.Instance
	key := publiclink.ReportID(msg.LinkID, msg.Expiry)
	mu := config.Lock().LongOperation(inst, "public-link-report/"+key)
	if err := mu.Lock(); err != nil {
		return errors.New("link report lock unavailable")
	}
	defer mu.Unlock()

	state := &publiclink.Report{}
	err := couchdb.GetDoc(inst, consts.PublicLinkReports, key, state)
	if couchdb.IsNotFoundError(err) || couchdb.IsNoDatabaseError(err) {
		if err = revalidateReport(ctx, msg); err != nil {
			return nil
		}
		settings, serr := inst.SettingsDocument()
		if serr != nil {
			return errors.New("link report settings unavailable")
		}
		email, _ := settings.M["email"].(string)
		state = &publiclink.Report{
			DocID: key, ReportMessage: msg, FileID: "link-report-" + key,
			Status: "pending", NoEmail: strings.TrimSpace(email) == "",
		}
		if err = couchdb.CreateNamedDocWithDB(inst, state); err != nil {
			return errors.New("link report checkpoint unavailable")
		}
	} else if err != nil {
		return errors.New("link report checkpoint unavailable")
	}
	if state.Terminal() || state.NextAttempt.After(time.Now()) {
		return nil
	}
	if state.Attempts >= 8 {
		state.Status = "failed"
		return couchdb.UpdateDoc(inst, state)
	}
	state.Attempts++
	delay := time.Duration(1<<state.Attempts) * 5 * time.Minute
	if delay > 6*time.Hour {
		delay = 6 * time.Hour
	}
	state.NextAttempt = time.Now().Add(delay)
	// CAS before any side effects fences duplicate jobs, including expired leases.
	if err := couchdb.UpdateDoc(inst, state); err != nil {
		return errors.New("link report checkpoint conflict")
	}
	err = generateAndSendReport(ctx, state)
	if errors.Is(err, errReportCancelled) {
		state.Status = "cancelled"
	} else if err != nil {
		if state.Attempts >= 8 {
			state.Status = "failed"
		}
		// No activity, email addresses, sharecodes or raw transport errors in jobs.
		if saveErr := couchdb.UpdateDoc(inst, state); saveErr != nil {
			return errors.New("link report retry checkpoint failed")
		}
		return errors.New("link report attempt failed")
	}
	if err := couchdb.UpdateDoc(inst, state); err != nil {
		return errors.New("link report completion checkpoint failed")
	}
	return nil
}

func revalidateReport(ctx *job.TaskContext, msg publiclink.ReportMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := permission.GetPermissionByIDIncludingExpired(ctx.Instance, msg.LinkID)
	if couchdb.IsNotFoundError(err) {
		return errReportCancelled
	}
	if err != nil {
		return err
	}
	at, enabled := p.ActivityReportExpiry()
	if !enabled || !at.Equal(msg.Expiry) || at.After(time.Now()) {
		return errReportCancelled
	}
	return nil
}

func generateAndSendReport(ctx *job.TaskContext, state *publiclink.Report) error {
	if err := revalidateReport(ctx, state.ReportMessage); err != nil {
		return err
	}
	inst, fs := ctx.Instance, ctx.Instance.VFS()
	file, err := fs.FileByID(state.FileID)
	if os.IsNotExist(err) {
		file, err = createReportFile(ctx, state)
	}
	if err != nil {
		return err
	}
	// Never recreate a file deliberately moved, trashed, deleted or overwritten
	// after its publication. A crash between file close and checkpoint is harmless.
	if file.Trashed || file.DirID != state.DirectoryID || file.Metadata["public_link_report"] != state.ID() || (state.FileRev != "" && file.Rev() != state.FileRev) {
		return errReportCancelled
	}
	if err := checkReportDestination(fs, file); err != nil {
		return err
	}
	if err := revalidateReport(ctx, state.ReportMessage); err != nil {
		return err
	}
	if !state.NoEmail {
		settings, err := inst.SettingsDocument()
		if err != nil {
			return err
		}
		email, _ := settings.M["email"].(string)
		state.NoEmail = strings.TrimSpace(email) == ""
	}
	if state.NoEmail {
		state.Status = "no-email"
		return nil
	}
	last, err := reportPreview(ctx, fs, file)
	if err != nil {
		return err
	}
	u := inst.SubDomain(consts.DriveSlug)
	u.Fragment = "/folder/" + file.DirID + "/file/" + file.ID()
	opts := &mail.Options{
		Mode: mail.ModeFromStack, RequireDelivery: true,
		MessageID:      "<link-report-" + state.ID() + "@" + strings.Split(inst.Domain, ":")[0] + ">",
		TemplateName:   "expired_link_report",
		Layout:         "layout-expired-link-report",
		TemplateValues: map[string]interface{}{"Expiry": state.Expiry.UTC().Format("2006-01-02 15:04 UTC"), "ReportURL": u.String(), "Events": last},
	}
	// The existing sender resolves settings on the hosting instance, not creator
	// metadata. Event previews are constructed here and never stored in a job.
	if err := revalidateReport(ctx, state.ReportMessage); err != nil {
		return err
	}
	if err := couchdb.UpdateDoc(inst, state); err != nil {
		return err
	}
	// Re-read the published document after reading its potentially large CSV.
	// A move/edit during that I/O must not mail a stale snapshot.
	current, err := fs.FileByID(file.ID())
	if os.IsNotExist(err) {
		return errReportCancelled
	}
	if err != nil {
		return err
	}
	if current.Rev() != file.Rev() || current.Trashed {
		return errReportCancelled
	}
	if err := checkReportDestination(fs, current); err != nil {
		return err
	}
	if err := revalidateReport(ctx, state.ReportMessage); err != nil {
		return err
	}
	if err := sendInstanceMail(ctx, opts); err != nil {
		return err
	}
	state.Status = "sent"
	return nil
}

func createReportFile(ctx *job.TaskContext, state *publiclink.Report) (*vfs.FileDoc, error) {
	inst, fs := ctx.Instance, ctx.Instance.VFS()
	if state.Status == "file-ready" {
		return nil, errReportCancelled
	}
	dirID, err := inst.ResolveDirID(consts.Apps + "/reports")
	if err != nil {
		return nil, err
	}
	dir, err := fs.DirByID(dirID)
	if err != nil {
		return nil, err
	}
	if err := checkReportDestination(fs, dir); err != nil {
		return nil, err
	}
	state.DirectoryID = dirID
	if err := couchdb.UpdateDoc(inst, state); err != nil {
		return nil, err
	}

	tmp, err := os.CreateTemp("", "cozy-link-report-*.csv")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	// As for the interactive export, a crash must not leave a named CSV with
	// visitor identities on the local temporary filesystem.
	if err := os.Remove(tmp.Name()); err != nil {
		return nil, err
	}
	if err := publiclink.WriteReportCSV(ctx, tmp, inst, state.LinkID, state.Expiry); err != nil {
		return nil, err
	}
	info, err := tmp.Stat()
	if err != nil {
		return nil, err
	}
	if _, err = tmp.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	name := "link-report-" + state.Expiry.UTC().Format("20060102T150405Z") + "-" + state.ID()[:12] + ".csv"
	file, err := vfs.NewFileDoc(name, dirID, info.Size(), nil, "text/csv", "text", time.Now(), false, false, false, nil)
	if err != nil {
		return nil, err
	}
	file.DocID = state.FileID
	file.Metadata = vfs.Metadata{"public_link_report": state.ID()}
	if err = revalidateReport(ctx, state.ReportMessage); err != nil {
		return nil, err
	}
	if err = checkReportDestination(fs, file); err != nil {
		return nil, err
	}
	out, err := fs.CreateFile(file, nil)
	if err != nil {
		return nil, err
	}
	upload, ok := out.(interface{ Abort() error })
	if !ok {
		return nil, errors.New("report upload cannot be safely aborted")
	}
	abort := func() { _ = upload.Abort() }
	if _, err = io.Copy(out, tmp); err != nil {
		abort()
		return nil, err
	}
	// Revalidate after the potentially long export/upload, before Close
	// publishes the VFS document.
	if err = revalidateReport(ctx, state.ReportMessage); err != nil {
		abort()
		return nil, err
	}
	if err = checkReportDestination(fs, file); err != nil {
		abort()
		return nil, err
	}
	if err = couchdb.UpdateDoc(inst, state); err != nil {
		abort()
		return nil, err
	}
	if err = out.Close(); err != nil {
		return nil, err
	}
	state.Status = "file-ready"
	state.FileRev = file.Rev()
	if err = couchdb.UpdateDoc(inst, state); err != nil {
		return nil, err
	}
	return file, nil
}

// checkReportDestination keeps reports out of the trash, including when an
// ancestor was trashed during generation. Sharing the destination is allowed.
func checkReportDestination(fs vfs.VFS, target vfs.Fetcher) error {
	cur := target
	for {
		switch d := cur.(type) {
		case *vfs.DirDoc:
			if d.ID() == consts.TrashDirID || strings.HasPrefix(d.Fullpath, vfs.TrashDirName) {
				return vfs.ErrFileInTrash
			}
		case *vfs.FileDoc:
			if d.Trashed {
				return vfs.ErrFileInTrash
			}
		}
		if cur.ID() == consts.RootDirID {
			return nil
		}
		parent, err := cur.Parent(fs)
		if err != nil {
			return err
		}
		cur = parent
	}
}

func reportPreview(ctx context.Context, fs vfs.VFS, file *vfs.FileDoc) ([][]string, error) {
	in, err := fs.OpenFile(file)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	reader := csv.NewReader(in)
	header, err := reader.Read()
	if err != nil || strings.Join(header, ",") != "occurred_at,IP,identity,action" {
		return nil, errors.New("invalid report CSV")
	}
	var last [][]string
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row, err := reader.Read()
		if err == io.EOF {
			return formatReportPreview(last), nil
		}
		if err != nil {
			return nil, err
		}
		last = append(last, row)
		if len(last) > 3 {
			last = last[1:]
		}
	}
}

func formatReportPreview(rows [][]string) [][]string {
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	for _, row := range rows {
		if len(row) == 4 {
			if at, err := time.Parse(time.RFC3339Nano, row[0]); err == nil {
				row[0] = at.UTC().Format("2006-01-02 15:04 UTC")
			}
		}
	}
	return rows
}
