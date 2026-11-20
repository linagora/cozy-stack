package mails

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/cozy/gomail"
)

// Keep ownership of the transport so aborting a report never needs a QUIT
// exchange, even during DATA. gomail closes with QUIT, not a raw close.
func dialReportSMTP(ctx context.Context, opts *gomail.DialerOptions) (_ *smtp.Client, _ net.Conn, err error) {
	if opts.NativeTLS && opts.DisableTLS {
		return nil, nil, errors.New("conflicting SMTP TLS options")
	}
	var dialer net.Dialer
	transport, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port)))
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			_ = transport.Close()
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		if err := transport.SetDeadline(deadline); err != nil {
			return nil, nil, err
		}
	}
	tlsConfig := &tls.Config{ServerName: opts.Host, InsecureSkipVerify: opts.SkipCertificateValidation}
	conn := transport
	if opts.NativeTLS {
		conn = tls.Client(transport, tlsConfig)
	}
	client, err := smtp.NewClient(conn, opts.Host)
	if err != nil {
		return nil, nil, err
	}
	name := opts.LocalName
	if name == "" {
		name = "localhost"
	}
	if err := client.Hello(name); err != nil {
		return nil, nil, err
	}
	if !opts.DisableTLS && !opts.NativeTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return nil, nil, errors.New("SMTP server did not advertise STARTTLS")
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return nil, nil, err
		}
	}
	if opts.Username != "" || opts.Password != "" {
		if ok, mechanisms := client.Extension("AUTH"); ok {
			if err := client.Auth(reportSMTPAuth(opts, mechanisms)); err != nil {
				return nil, nil, err
			}
		}
	}
	return client, transport, nil
}

func reportSMTPAuth(opts *gomail.DialerOptions, mechanisms string) smtp.Auth {
	// Preserve the configured authentication behavior of the ordinary sender.
	switch {
	case strings.Contains(mechanisms, "CRAM-MD5"):
		return smtp.CRAMMD5Auth(opts.Username, opts.Password)
	case strings.Contains(mechanisms, "LOGIN") && !strings.Contains(mechanisms, "PLAIN"):
		return reportLoginAuth{opts.Username, opts.Password, opts.Host}
	default:
		return smtp.PlainAuth("", opts.Username, opts.Password, opts.Host)
	}
}

// net/smtp does not support LOGIN. Match the pinned gomail behavior.
type reportLoginAuth struct{ username, password, host string }

func (a reportLoginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if server.Name != a.host {
		return "", nil, errors.New("unexpected SMTP host")
	}
	if !server.TLS {
		for _, mechanism := range server.Auth {
			if mechanism == "LOGIN" {
				return "LOGIN", nil, nil
			}
		}
		return "", nil, errors.New("SMTP LOGIN not advertised on plaintext connection")
	}
	return "LOGIN", nil, nil
}

func (a reportLoginAuth) Next(challenge []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch string(challenge) {
	case "Username:":
		return []byte(a.username), nil
	case "Password:":
		return []byte(a.password), nil
	default:
		return nil, errors.New("unexpected SMTP LOGIN challenge")
	}
}

func submitReportMail(client *smtp.Client, from string, to []string, message io.WriterTo) error {
	if err := client.Mail(from); err != nil {
		return err
	}
	for _, addr := range to {
		if err := client.Rcpt(addr); err != nil {
			return err
		}
	}
	data, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := message.WriteTo(data); err != nil {
		// Closing data would send the terminator for a partial message.
		return err
	}
	return data.Close()
}

// The caller always closes the raw transport. After acceptance, give the
// relay a bounded opportunity to acknowledge QUIT without retaining a worker.
func quitReportSMTP(ctx context.Context, client *smtp.Client, transport net.Conn) {
	if ctx.Err() != nil {
		return
	}
	deadline := time.Now().Add(time.Second)
	if jobDeadline, ok := ctx.Deadline(); ok && jobDeadline.Before(deadline) {
		deadline = jobDeadline
	}
	if transport.SetDeadline(deadline) != nil {
		return
	}
	id, err := client.Text.Cmd("QUIT")
	if err == nil {
		client.Text.StartResponse(id)
		defer client.Text.EndResponse(id)
		_, _, _ = client.Text.ReadResponse(221)
	}
}
