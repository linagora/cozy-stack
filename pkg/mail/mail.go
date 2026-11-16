package mail

import (
	"time"

	"github.com/cozy/gomail"
)

const (
	// ModeFromStack is the no-reply mode of a mail, to send mail "to" the
	// user's mail, as a noreply@
	ModeFromStack = "noreply"
	// ModePendingEmail is used to send an email to confirm the new email
	// address of the user
	ModePendingEmail = "pending"
	// ModeFromUser is the "from" mode of a mail, to send mail "from" the user's
	// mail.
	ModeFromUser = "from"
	// ModeSupport is used to send both a request to the support and a
	// confirmation to the user.
	ModeSupport = "support"
	// ModeCampaign is used to send a non transactional email to the user
	ModeCampaign = "campaign"

	// DefaultLayout defines the default MJML layout to use
	DefaultLayout = "layout"
	// CozyCloudLayout defines the alternative MJML layout
	CozyCloudLayout = "layout-cozycloud"
)

// Address contains the name and mail of a mail recipient.
type Address struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Attachment is for attaching a file to the mail
type Attachment struct {
	Filename string `json:"filename"`
	Content  []byte `json:"content"`
}

// Options should be used as the options of a mail with manually defined
// content: body and body content-type. It is used as the input of the
// "sendmail" worker.
type Options struct {
	// Internal delivery requirements cannot be set by API callers.
	RequireDelivery bool                   `json:"-"`
	MessageID       string                 `json:"-"`
	Mode            string                 `json:"mode"`
	Subject         string                 `json:"subject"`
	From            *Address               `json:"from,omitempty"`
	To              []*Address             `json:"to,omitempty"`
	ReplyTo         *Address               `json:"reply_to,omitempty"`
	Dialer          *gomail.DialerOptions  `json:"dialer,omitempty"`
	Date            *time.Time             `json:"date,omitempty"`
	Parts           []*Part                `json:"parts,omitempty"`
	RecipientName   string                 `json:"recipient_name,omitempty"`
	TemplateName    string                 `json:"template_name,omitempty"`
	TemplateValues  map[string]interface{} `json:"template_values,omitempty"`
	Attachments     []*Attachment          `json:"attachments,omitempty"`
	Locale          string                 `json:"locale,omitempty"`
	Layout          string                 `json:"layout,omitempty"`
}

// Part represent a part of the content of the mail. It has a type
// specifying the content type of the part, and a body.
type Part struct {
	Type string `json:"type"`
	Body string `json:"body"`
}
