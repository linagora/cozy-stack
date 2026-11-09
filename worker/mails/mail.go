package mails

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/mail"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/cozy/gomail"
)

func init() {
	job.AddWorker(&job.WorkerConfig{
		WorkerType:  "sendmail",
		Concurrency: runtime.NumCPU(),
		WorkerFunc:  SendMail,
	})
	initMailTemplates()
}

// var for testability
var mailTemplater MailTemplater
var sendMail = doSendMail

// SendMail is the sendmail worker function.
func SendMail(ctx *job.TaskContext) error {
	opts := mail.Options{}
	err := ctx.UnmarshalMessage(&opts)
	if err != nil {
		return err
	}

	from := config.GetConfig().NoReplyAddr
	name := config.GetConfig().NoReplyName
	replyTo := config.GetConfig().ReplyTo
	if from == "" {
		from = "noreply@" + utils.StripPort(ctx.Instance.Domain)
	}
	if ctxSettings, ok := ctx.Instance.SettingsContext(); ok {
		if addr, ok := ctxSettings["noreply_address"].(string); ok && addr != "" {
			from = addr
		}
		if nname, ok := ctxSettings["noreply_name"].(string); ok && nname != "" {
			name = nname
		}
		if reply, ok := ctxSettings["reply_to"].(string); ok && reply != "" {
			replyTo = reply
		}
	}

	var cfgPerContext map[string]interface{}
	if opts.Mode == mail.ModeCampaign {
		cfgPerContext = config.GetConfig().CampaignMailPerContext
	} else {
		cfgPerContext = config.GetConfig().MailPerContext
	}

	ctxName := ctx.Instance.ContextName
	if ctxConfig, ok := cfgPerContext[ctxName].(map[string]interface{}); ok {
		if host, ok := ctxConfig["host"].(string); ok && host != "" {
			port, _ := ctxConfig["port"].(int)
			username, _ := ctxConfig["username"].(string)
			password, _ := ctxConfig["username"].(string)
			UseSSL, _ := ctxConfig["use_ssl"].(bool)
			disableTLS, _ := ctxConfig["disable_tls"].(bool)
			skipCertValid, _ := ctxConfig["skip_certificate_validation"].(bool)
			LocalName, _ := ctxConfig["local_name"].(string)

			opts.Dialer = &gomail.DialerOptions{
				Host:                      host,
				Port:                      port,
				Username:                  username,
				Password:                  password,
				NativeTLS:                 UseSSL,
				DisableTLS:                disableTLS,
				SkipCertificateValidation: skipCertValid,
				LocalName:                 LocalName,
			}
		}
	}

	switch opts.Mode {
	case mail.ModeFromStack, mail.ModeCampaign:
		toAddr, err := addressFromInstance(ctx.Instance)
		if err != nil {
			return err
		}
		opts.To = []*mail.Address{toAddr}
		opts.From = &mail.Address{Name: name, Email: from}
		if replyTo != "" {
			opts.ReplyTo = &mail.Address{Name: name, Email: replyTo}
		}
		opts.RecipientName = toAddr.Name
	case mail.ModePendingEmail:
		toAddr, err := pendingAddress(ctx.Instance)
		if err != nil {
			return err
		}
		opts.To = []*mail.Address{toAddr}
		opts.From = &mail.Address{Name: name, Email: from}
		if replyTo != "" {
			opts.ReplyTo = &mail.Address{Name: name, Email: replyTo}
		}
		opts.RecipientName = toAddr.Name
	case mail.ModeFromUser:
		sender, err := addressFromInstance(ctx.Instance)
		if err != nil {
			return err
		}
		name = sender.Name
		opts.ReplyTo = sender
		opts.From = &mail.Address{Name: name, Email: from}
	case mail.ModeSupport:
		toAddr, err := addressFromInstance(ctx.Instance)
		if err != nil {
			return err
		}
		opts.To = []*mail.Address{toAddr}
		opts.From = &mail.Address{Name: name, Email: from}
		if replyTo != "" {
			opts.ReplyTo = &mail.Address{Name: name, Email: replyTo}
		}
		opts.RecipientName = toAddr.Name
		if err := sendSupportMail(ctx, &opts, ctx.Instance.Domain); err != nil {
			return err
		}
	default:
		return fmt.Errorf("Mail sent with unknown mode %s", opts.Mode)
	}
	if opts.TemplateName != "" && opts.Locale == "" {
		opts.Locale = ctx.Instance.Locale
	}
	if err = sendMail(ctx, &opts, ctx.Instance.Domain); err != nil {
		ctx.Logger().Warnf("sendmail has failed: %s", err)
	}
	return err
}

func pendingAddress(i *instance.Instance) (*mail.Address, error) {
	doc, err := i.SettingsDocument()
	if err != nil {
		return nil, err
	}
	email, ok := doc.M["pending_email"].(string)
	if !ok {
		return nil, fmt.Errorf("Domain %s has no pending email in its settings", i.Domain)
	}
	publicName, _ := doc.M["public_name"].(string)
	return &mail.Address{
		Name:  publicName,
		Email: email,
	}, nil
}

func addressFromInstance(i *instance.Instance) (*mail.Address, error) {
	doc, err := i.SettingsDocument()
	if err != nil {
		return nil, err
	}
	email, ok := doc.M["email"].(string)
	if !ok {
		return nil, fmt.Errorf("Domain %s has no email in its settings", i.Domain)
	}
	publicName, _ := doc.M["public_name"].(string)
	return &mail.Address{
		Name:  publicName,
		Email: email,
	}, nil
}

func doSendMail(ctx *job.TaskContext, opts *mail.Options, domain string) error {
	if opts.TemplateName == "" && opts.Subject == "" {
		return errors.New("Missing mail subject")
	}
	if len(opts.To) == 0 {
		return errors.New("Missing mail recipient")
	}
	if opts.From == nil {
		return errors.New("Missing mail sender")
	}
	email := gomail.NewMessage()
	dialerOptions := opts.Dialer
	if dialerOptions == nil {
		if opts.Mode == mail.ModeCampaign {
			dialerOptions = config.GetConfig().CampaignMail
		} else {
			dialerOptions = config.GetConfig().Mail
		}
	}
	if dialerOptions.Host == "-" {
		return nil
	}
	var date time.Time
	if opts.Date == nil {
		date = time.Now()
	} else {
		date = *opts.Date
	}
	toAddresses := make([]string, len(opts.To))
	for i, to := range opts.To {
		// See https://tools.ietf.org/html/rfc5322#section-3.4
		// We want to use an email address in the "display-name <addr-spec>"
		// format. If it is the case, the address is taken as is. Else, gomail
		// is used to format it.
		to.Email = strings.TrimSpace(to.Email)
		if strings.HasSuffix(to.Email, ">") {
			toAddresses[i] = to.Email
		} else {
			toAddresses[i] = email.FormatAddress(to.Email, to.Name)
		}
	}

	var parts []*mail.Part
	var err error

	if opts.TemplateName != "" {
		// Defining the master layout which will wrap the content
		layout := opts.Layout
		if layout == "" {
			layout = mail.DefaultLayout
		}
		opts.Subject, parts, err = RenderMail(ctx, opts.TemplateName, layout, opts.Locale, opts.RecipientName, opts.TemplateValues)
		if err != nil {
			return err
		}
	} else {
		parts = opts.Parts
	}

	headers := map[string][]string{
		"From":    {email.FormatAddress(opts.From.Email, opts.From.Name)},
		"To":      toAddresses,
		"Subject": {opts.Subject},
		"X-Cozy":  {domain},
	}
	if opts.ReplyTo != nil {
		headers["Reply-To"] = []string{
			email.FormatAddress(opts.ReplyTo.Email, opts.ReplyTo.Name),
		}
	}
	email.SetHeaders(headers)
	email.SetDateHeader("Date", date)

	for _, part := range parts {
		if err = addPart(email, part); err != nil {
			return err
		}
	}

	for _, attachment := range opts.Attachments {
		addAttachment(email, attachment)
	}

	dialer := gomail.NewDialer(dialerOptions)
	if deadline, ok := ctx.Deadline(); ok {
		dialer.SetDeadline(deadline)
	}
	return dialer.DialAndSend(email)
}

func addAttachment(email *gomail.Message, attachment *mail.Attachment) {
	name := filepath.Base(attachment.Filename)
	contentType, params, err := mime.ParseMediaType(mime.TypeByExtension(filepath.Ext(name)))
	if err != nil {
		contentType, params = "application/octet-stream", make(map[string]string)
	}
	params["name"] = name
	headers := map[string][]string{
		"Content-Type":        {mime.FormatMediaType(contentType, params)},
		"Content-Disposition": {mime.FormatMediaType("attachment", map[string]string{"filename": name})},
	}
	email.Attach(name, gomail.SetHeader(headers), gomail.SetCopyFunc(func(w io.Writer) error {
		_, err := w.Write(attachment.Content)
		return err
	}))
}

func addPart(mail *gomail.Message, part *mail.Part) error {
	contentType := part.Type
	if contentType != "text/plain" && contentType != "text/html" {
		return fmt.Errorf("Unknown body content-type %s", contentType)
	}
	mail.AddAlternative(contentType, part.Body)
	return nil
}

func sendSupportMail(ctx *job.TaskContext, opts *mail.Options, domain string) error {
	email := gomail.NewMessage()
	dialerOptions := opts.Dialer
	if dialerOptions == nil {
		dialerOptions = config.GetConfig().Mail
	}
	if dialerOptions.Host == "-" {
		return nil
	}
	var date time.Time
	if opts.Date == nil {
		date = time.Now()
	} else {
		date = *opts.Date
	}
	headers := map[string][]string{
		"From":    {email.FormatAddress(opts.To[0].Email, opts.To[0].Name)},
		"To":      {email.FormatAddress(opts.ReplyTo.Email, opts.ReplyTo.Name)},
		"Subject": {opts.Subject},
		"X-Cozy":  {domain},
	}
	if opts.ReplyTo != nil {
		headers["Reply-To"] = []string{
			email.FormatAddress(opts.ReplyTo.Email, opts.ReplyTo.Name),
		}
	}
	email.SetHeaders(headers)
	email.SetDateHeader("Date", date)

	intro := fmt.Sprintf("Demande de support pour %s:\n\n", domain)
	body, _ := opts.TemplateValues["Body"].(string)
	email.AddAlternative("text/plain", intro+body+"\n")

	dialer := gomail.NewDialer(dialerOptions)
	if deadline, ok := ctx.Deadline(); ok {
		dialer.SetDeadline(deadline)
	}
	return dialer.DialAndSend(email)
}
