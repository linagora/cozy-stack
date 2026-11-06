package mails

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	text "text/template"

	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/pkg/assets"
	"github.com/cozy/cozy-stack/pkg/i18n"
	"github.com/cozy/cozy-stack/pkg/mail"
)

const templateTitleVar = "template_title"

func initMailTemplates() {
	mailTemplater = MailTemplater{
		"passphrase_hint":              subjectEntry{"Mail Hint Subject", nil},
		"passphrase_reset":             subjectEntry{"Mail Reset Passphrase Subject", nil},
		"expired_link_report":          subjectEntry{"Mail Link Report Subject", nil},
		"archiver":                     subjectEntry{"Mail Archive Subject", nil},
		"import_success":               subjectEntry{"Mail Import Success Subject", nil},
		"import_error":                 subjectEntry{"Mail Import Error Subject", nil},
		"export_error":                 subjectEntry{"Mail Export Error Subject", nil},
		"move_confirm":                 subjectEntry{"Mail Move Confirm Subject", nil},
		"move_success":                 subjectEntry{"Mail Move Success Subject", nil},
		"move_error":                   subjectEntry{"Mail Move Error Subject", nil},
		"magic_link":                   subjectEntry{"Mail Magic Link Subject", nil},
		"two_factor":                   subjectEntry{"Mail Two Factor Subject", nil},
		"two_factor_mail_confirmation": subjectEntry{"Mail Two Factor Mail Confirmation Subject", []string{templateTitleVar}},
		"confirm_flagship":             subjectEntry{"Mail Confirm Flagship Subject", nil},
		"alert_account":                subjectEntry{"Mail Alert Account Subject", nil},
		"support_request":              subjectEntry{"Mail Support Confirmation Subject", nil},
		"sharing_request":              subjectEntry{"Mail Sharing Request Subject", []string{"SharerPublicName", "TitleType"}},
		"sharing_to_confirm":           subjectEntry{"Mail Sharing Member To Confirm Subject", nil},
		"sharing_file_changed":         subjectEntry{"Mail Sharing File Changed Subject", []string{"SharingDescription"}},
		"notifications_sharing":        subjectEntry{"Notification Sharing Subject", []string{"SharerPublicName", "TitleType"}},
		"notifications_diskquota":      subjectEntry{"Notifications Disk Quota Subject", nil},
		"notifications_oauthclients":   subjectEntry{"Notifications OAuth Clients Subject", nil},
		"notifications_antivirus":      subjectEntry{"Mail Antivirus Alert Subject", nil},
		"update_email":                 subjectEntry{"Mail Update Email Subject", nil},
	}
}

// RenderMail returns a rendered mail for the given template name with the
// specified locale, recipient name and template data values.
func RenderMail(ctx *job.TaskContext, name, layout, locale, recipientName string, templateValues map[string]interface{}) (string, []*mail.Part, error) {
	return mailTemplater.Execute(ctx, name, layout, locale, recipientName, templateValues)
}

// MailTemplater is the list of templates for emails.
type MailTemplater map[string]subjectEntry

// subjectEntry is a i18n key for the subject message, and some optional
// variable names.
type subjectEntry struct {
	Key       string
	Variables []string
}

// Execute will execute the HTML and text templates for the template with the
// specified name. It returns the mail parts that should be added to the sent
// mail.
func (m MailTemplater) Execute(ctx *job.TaskContext, name, layout, locale string, recipientName string, data map[string]interface{}) (string, []*mail.Part, error) {
	entry, ok := m[name]
	if !ok {
		err := fmt.Errorf("Could not find email named %q", name)
		return "", nil, err
	}

	var vars []interface{}
	for _, name := range entry.Variables {
		if name == templateTitleVar {
			vars = append(vars, ctx.Instance.TemplateTitle())
		} else {
			vars = append(vars, data[name])
		}
	}

	context := ctx.Instance.ContextName
	assets.LoadContextualizedLocale(context, locale)
	subject := i18n.Translate(entry.Key, locale, context, vars...)
	if data == nil {
		data = map[string]interface{}{"Locale": locale}
	} else {
		data["Locale"] = locale
	}
	if ctx.Instance != nil {
		data["InstanceURL"] = ctx.Instance.PageURL("/", nil)
	}

	txt, err := buildText(name, context, locale, data)
	if err != nil {
		return "", nil, err
	}
	parts := []*mail.Part{
		{Body: txt, Type: "text/plain"},
	}

	// If we can generate the HTML, we should still send the mail with the text
	// part.
	if html, err := buildHTML(name, layout, ctx, context, locale, data); err == nil {
		parts = append(parts, &mail.Part{Body: html, Type: "text/html"})
	} else {
		ctx.Logger().Errorf("Cannot generate HTML mail: %s", err)
	}
	return subject, parts, nil
}

func buildText(name, context, locale string, data map[string]interface{}) (string, error) {
	buf := new(bytes.Buffer)
	b, err := loadTemplate("/mails/"+name+".text", context)
	if err != nil {
		return "", err
	}
	funcMap := text.FuncMap{"t": i18n.Translator(locale, context)}
	t, err := text.New("text").Funcs(funcMap).Parse(string(b))
	if err != nil {
		return "", err
	}
	if err := t.Execute(buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func buildHTML(name string, layout string, ctx *job.TaskContext, context, locale string, data map[string]interface{}) (string, error) {
	buf := new(bytes.Buffer)
	b, err := loadTemplate("/mails/"+name+".mjml", context)
	if err != nil {
		return "", err
	}
	funcMap := template.FuncMap{
		"t":     i18n.Translator(locale, context),
		"tHTML": i18n.TranslatorHTML(locale, context),
	}
	t, err := template.New("content").Funcs(funcMap).Parse(string(b))
	if err != nil {
		return "", err
	}
	b, err = loadTemplate("/mails/"+layout+".mjml", context)
	if err != nil {
		return "", err
	}
	t, err = t.New("layout").Funcs(funcMap).Parse(string(b))
	if err != nil {
		return "", err
	}
	if err := t.Execute(buf, data); err != nil {
		return "", err
	}
	html, err := execMjml(ctx, buf.Bytes())
	if err != nil {
		return "", err
	}
	return string(html), nil
}

func loadTemplate(name, context string) ([]byte, error) {
	f, err := assets.Open(name, context)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}
