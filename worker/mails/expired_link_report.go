package mails

import (
	"encoding/csv"
	"errors"
	"io"
	netmail "net/mail"
	"slices"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/feature"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/pkg/assets"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/mail"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/cozy/gomail"
)

const expiredReportTemplate = "expired_link_report"

// Keep this template out of the sendmail API, which accepts app-provided data.
var expiredReportTemplater = MailTemplater{expiredReportTemplate: subjectEntry{"Mail Link Report Subject", nil}}

func init() {
	job.AddWorker(&job.WorkerConfig{
		WorkerType: publiclink.ReportWorker, WorkerFunc: expiredLinkReport,
		Reserved: true, Concurrency: 2, MaxExecCount: 1, Timeout: 10 * time.Minute,
	})
}

func expiredLinkReport(ctx *job.TaskContext) error {
	return expiredLinkReportWithClock(ctx, time.Now)
}

func expiredLinkReportWithClock(ctx *job.TaskContext, now func() time.Time) error {
	var msg publiclink.ReportMessage
	at := now()
	if err := ctx.UnmarshalMessage(&msg); err != nil || ctx.Instance == nil || msg.LinkID == "" || msg.Expiry.IsZero() || msg.Expiry.After(at) {
		return errors.New("invalid link report job")
	}
	if !publiclink.ReportWindowOpen(at) {
		return nil
	}
	if err := deliverExpiredLinkReport(ctx, msg, now); err != nil {
		// Internal errors can contain recipients, event data or storage details.
		return errors.New("link report attempt failed")
	}
	return nil
}

func deliverExpiredLinkReport(ctx *job.TaskContext, msg publiclink.ReportMessage, now func() time.Time) error {
	inst := ctx.Instance
	key := publiclink.ReportID(msg.LinkID, msg.Expiry)
	mu := config.Lock().LongOperation(inst, "public-link-report/"+key)
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	if !publiclink.ReportWindowOpen(now()) {
		return nil
	}
	state := &publiclink.Delivery{DocID: publiclink.DeliveryID(key)}
	err := couchdb.GetDoc(inst, consts.PublicLinkReports, state.ID(), state)
	if err == nil && !state.Due(now()) {
		return nil
	}
	if err != nil && (!couchdb.IsNotFoundError(err) || couchdb.IsDeletedError(err)) {
		return err
	}
	create := err != nil
	settings, err := inst.SettingsDocument()
	if err != nil {
		return err
	}
	address, _ := settings.M["email"].(string)
	address = strings.TrimSpace(address)
	enabled, err := reportFeatureEnabled(inst)
	if err != nil || !enabled {
		return err
	}
	at := now()
	if !publiclink.ReportWindowOpen(at) {
		return nil
	}
	if address == "" {
		state.Status = publiclink.DeliveryFileOnly
	} else if create {
		state.Status = publiclink.DeliveryPending
	}
	// Persist the next eligible night before doing any work. The revision check
	// also prevents two consumers from starting the same attempt on stale reads.
	_, end := publiclink.ReportWindow(at)
	state.NextAttempt, _ = publiclink.ReportWindow(end)
	if create {
		err = couchdb.CreateNamedDocWithDB(inst, state)
	} else {
		err = couchdb.UpdateDoc(inst, state)
	}
	if err != nil || !publiclink.ReportWindowOpen(now()) {
		return err
	}
	// Only admission is limited to the window; the report may finish after it.
	if state.Status == publiclink.DeliveryFileOnly {
		if _, err := inst.GenerateExpiredLinkReport(ctx, msg.LinkID, msg.Expiry); err != nil {
			return err
		}
		return setReportStatus(inst, state, publiclink.DeliveryNoEmail)
	}

	owner, err := netmail.ParseAddress(address)
	if err != nil {
		return err
	}
	if name, _ := settings.M["public_name"].(string); owner.Name == "" {
		owner.Name = name
	}
	location := reportMailLocation(settings.M["tz"])
	var rows [][]string
	file, check, err := inst.ReadExpiredLinkReport(ctx, msg.LinkID, msg.Expiry, func(src io.Reader) error {
		var err error
		rows, err = readReportPreview(src, location)
		return err
	})
	if err != nil {
		return err
	}

	u := inst.SubDomain(consts.DriveSlug)
	u.Fragment = "/folder/" + file.DirID + "/file/" + file.ID()
	opts := &mail.Options{
		Mode: mail.ModeFromStack,
		To:   []*mail.Address{{Name: owner.Name, Email: owner.Address}},
	}
	opts.From, opts.ReplyTo = configureInstanceMail(inst, opts)
	smtpOptions := mailDialerOptions(opts)
	if smtpOptions.Host == "-" {
		return errors.New("mail transport disabled")
	}
	opts.Subject, opts.Parts, err = expiredReportTemplater.execute(ctx, expiredReportTemplate, "layout-expired-link-report", inst.Locale, map[string]interface{}{
		"Expiry": formatReportMailTime(msg.Expiry, location), "ReportURL": u.String(), "Events": rows,
	}, true)
	if err != nil {
		return err
	}
	email, err := composeMail(ctx, opts, inst.Domain)
	if err != nil {
		return err
	}
	email.SetHeader("Message-ID", "<link-report-"+key+"@"+utils.StripPort(inst.Domain)+">")
	if err := embedReportImages(email, inst.ContextName); err != nil {
		return err
	}
	// Connect/authenticate without disclosing the envelope or body. Rendering,
	// SMTP setup and the claim all precede the last authorization checks.
	client, transport, err := dialReportSMTP(ctx, smtpOptions)
	if err != nil {
		return err
	}
	defer transport.Close()
	if err := setReportStatus(inst, state, publiclink.DeliverySubmitting); err != nil {
		return err
	}
	err = check(func() error {
		enabled, err := reportFeatureEnabled(inst)
		if err != nil {
			return err
		}
		if !enabled {
			return errors.New("link reports disabled")
		}
		current, err := inst.SettingsDocument()
		if err != nil {
			return err
		}
		if current.Rev() != settings.Rev() {
			return errors.New("report recipient settings changed")
		}
		return nil
	})
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		// No message was submitted. Release the claim for the next night only.
		return errors.Join(err, setReportStatus(inst, state, publiclink.DeliveryPending))
	}

	// A transport error need not mean the relay rejected the message. Leave
	// the claim in place on any unknown outcome, including completion failure.
	if err := gomail.Send(gomail.SendFunc(func(from string, to []string, message io.WriterTo) error {
		return submitReportMail(client, from, to, message)
	}), email); err != nil {
		return err
	}
	defer quitReportSMTP(ctx, client, transport)
	return setReportStatus(inst, state, publiclink.DeliverySent)
}

func setReportStatus(inst *instance.Instance, state *publiclink.Delivery, status string) error {
	state.Status = status
	return couchdb.UpdateDoc(inst, state)
}

func reportFeatureEnabled(inst *instance.Instance) (bool, error) {
	// Re-read the flag sources after blocking work, rather than using the
	// instance snapshot loaded before the job began.
	var current instance.Instance
	if err := couchdb.GetDoc(prefixer.GlobalPrefixer, consts.Instances, inst.ID(), &current); err != nil {
		return false, err
	}
	return feature.Enabled(&current, publiclink.FlagEnabled), nil
}

func readReportPreview(src io.Reader, location *time.Location) ([][]string, error) {
	reader := csv.NewReader(src)
	header, err := reader.Read()
	if err != nil || !slices.Equal(header, []string{"occurred_at", "IP", "identity", "action"}) {
		return nil, errors.New("invalid report CSV")
	}
	rows := make([][]string, 0, 3)
	for {
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(rows) == cap(rows) {
			copy(rows, rows[1:])
			rows = rows[:len(rows)-1]
		}
		rows = append(rows, row)
	}
	slices.Reverse(rows)
	for _, row := range rows {
		at, err := time.Parse(time.RFC3339Nano, row[0])
		if err != nil {
			return nil, err
		}
		row[0] = formatReportMailTime(at, location)
		identity := row[2]
		// Report CSV escapes leading control whitespace with an apostrophe.
		if len(identity) > 1 && identity[0] == '\'' && strings.ContainsRune("\t\r\n", rune(identity[1])) {
			identity = identity[1:]
		}
		if strings.TrimSpace(identity) == "" {
			row[2] = ""
		}
	}
	return rows, nil
}

func reportMailLocation(value interface{}) *time.Location {
	name, _ := value.(string)
	if name != "" && name != "Local" {
		if location, err := time.LoadLocation(name); err == nil {
			return location
		}
	}
	return time.UTC
}

func formatReportMailTime(at time.Time, location *time.Location) string {
	return at.In(location).Format("2006-01-02 15:04 MST -07:00")
}

// CID images do not require remote fetches or SVG support in mail clients.
func embedReportImages(message *gomail.Message, context string) error {
	for _, name := range []string{"report-drive.png", "report-link.png", "report-clock.png"} {
		file, err := assets.Open("/mails/"+name, context)
		if err != nil {
			return err
		}
		message.Embed(name, gomail.SetCopyFunc(func(w io.Writer) error {
			_, err := io.Copy(w, file)
			return err
		}))
	}
	return nil
}
