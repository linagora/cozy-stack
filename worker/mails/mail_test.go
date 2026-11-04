package mails

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"mime/quotedprintable"
	"net"
	stdmail "net/mail"
	"net/textproto"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/mail"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/gomail"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const serverString = `220 hello world
502 EH?
250 smtp.me at your service
250 Sender ok
250 Receiver ok
354 Go ahead
250 Data ok
221 Goodbye
`

func TestMails(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)

	setup := testutils.NewSetup(t, t.Name())

	inst := setup.GetTestInstance(&lifecycle.Options{Email: "me@me"})

	t.Run("mail send server", func(t *testing.T) {
		clientStrings := []string{`EHLO localhost
HELO localhost
MAIL FROM:<me@me>
RCPT TO:<you1@you>
DATA
Hey !!!
.
QUIT
`}

		expectedHeaders := map[string]string{
			"From":                      "me@me",
			"To":                        "you1@you",
			"Subject":                   "Up?",
			"Date":                      "Mon, 01 Jan 0001 00:00:00 +0000",
			"Content-Transfer-Encoding": "quoted-printable",
			"Content-Type":              "text/plain; charset=UTF-8",
			"Mime-Version":              "1.0",
			"X-Cozy":                    "cozy.example.com",
		}

		mailServer(t, serverString, clientStrings, expectedHeaders, func(host string, port int) error {
			msg := &mail.Options{
				From: &mail.Address{Email: "me@me"},
				To: []*mail.Address{
					{Email: "you1@you"},
				},
				Date:    &time.Time{},
				Subject: "Up?",
				Dialer: &gomail.DialerOptions{
					Host:       host,
					Port:       port,
					DisableTLS: true,
				},
				Parts: []*mail.Part{
					{
						Body: "Hey !!!",
						Type: "text/plain",
					},
				},
				Locale: "en",
			}
			j := &job.Job{JobID: "1", Domain: "cozy.example.com"}
			ctx, cancel := job.NewTaskContext("0", j, nil)
			defer cancel()
			return sendMail(ctx, msg, "cozy.example.com")
		})
	})

	t.Run("send template mail", func(t *testing.T) {
		clientStrings := []string{`EHLO localhost
HELO localhost
MAIL FROM:<me@me>
RCPT TO:<you1@you>
DATA
<!DOCTYPE html>
<html>
  <head>
    <meta charset=3D"UTF-8">
    <title>My page</title>
  </head>
  <body>
    <div>My photos</div><div>My blog</div>
  </body>
</html>
.
QUIT
`}

		expectedHeaders := map[string]string{
			"From":                      "me@me",
			"To":                        "you1@you",
			"Subject":                   "Up?",
			"Date":                      "Mon, 01 Jan 0001 00:00:00 +0000",
			"Content-Transfer-Encoding": "quoted-printable",
			"Content-Type":              "text/html; charset=UTF-8",
			"Mime-Version":              "1.0",
			"X-Cozy":                    "cozy.example.com",
		}

		mailBody := `<!DOCTYPE html>
<html>
  <head>
    <meta charset="UTF-8">
    <title>My page</title>
  </head>
  <body>
    <div>My photos</div><div>My blog</div>
  </body>
</html>
`

		mailServer(t, serverString, clientStrings, expectedHeaders, func(host string, port int) error {
			msg := &mail.Options{
				From: &mail.Address{Email: "me@me"},
				To: []*mail.Address{
					{Email: "you1@you"},
				},
				Date:    &time.Time{},
				Subject: "Up?",
				Dialer: &gomail.DialerOptions{
					Host:       host,
					Port:       port,
					DisableTLS: true,
				},
				Parts: []*mail.Part{
					{Body: mailBody, Type: "text/html"},
				},
				Locale: "en",
			}
			j := &job.Job{JobID: "1", Domain: "cozy.example.com"}
			ctx, cancel := job.NewTaskContext("0", j, nil)
			defer cancel()
			return sendMail(ctx, msg, "cozy.example.com")
		})
	})

	t.Run("with missing subject", func(t *testing.T) {
		msg := &mail.Options{
			From:   &mail.Address{Email: "me@me"},
			To:     []*mail.Address{{Email: "you@you"}},
			Locale: "en",
		}
		j := &job.Job{JobID: "1", Domain: "cozy.example.com"}
		ctx, cancel := job.NewTaskContext("0", j, nil)
		defer cancel()
		err := sendMail(ctx, msg, "cozy.example.com")
		if assert.Error(t, err) {
			assert.Equal(t, "Missing mail subject", err.Error())
		}
	})

	t.Run("with bad body type", func(t *testing.T) {
		msg := &mail.Options{
			From:    &mail.Address{Email: "me@me"},
			To:      []*mail.Address{{Email: "you@you"}},
			Subject: "Up?",
			Parts: []*mail.Part{
				{
					Type: "text/qsdqsd",
					Body: "foo",
				},
			},
			Locale: "en",
		}
		j := &job.Job{JobID: "1", Domain: "cozy.example.com"}
		ctx, cancel := job.NewTaskContext("0", j, nil)
		defer cancel()
		err := sendMail(ctx, msg, "cozy.example.com")
		if assert.Error(t, err) {
			assert.Equal(t, "Unknown body content-type text/qsdqsd", err.Error())
		}
	})

	t.Run("send with NoReply", func(t *testing.T) {
		sendMail = func(_ *job.TaskContext, opts *mail.Options, domain string) error {
			assert.NotNil(t, opts.From)
			assert.NotNil(t, opts.To)
			assert.Len(t, opts.To, 1)
			assert.Equal(t, "me@me", opts.To[0].Email)
			assert.Equal(t, "noreply@"+inst.Domain, opts.From.Email)
			assert.Equal(t, inst.Domain, domain)
			return errors.New("yes")
		}
		defer func() {
			sendMail = doSendMail
		}()
		msg, _ := job.NewMessage(mail.Options{
			Mode:    "noreply",
			Subject: "Up?",
			Parts: []*mail.Part{
				{
					Type: "text/plain",
					Body: "foo",
				},
			},
			Locale: "en",
		})
		j := job.NewJob(inst, &job.JobRequest{
			Message:    msg,
			WorkerType: "sendmail",
		})
		ctx, cancel := job.NewTaskContext("123", j, inst)
		defer cancel()
		err := SendMail(ctx)
		if assert.Error(t, err) {
			assert.Equal(t, "yes", err.Error())
		}
	})

	t.Run("send with From", func(t *testing.T) {
		sendMail = func(_ *job.TaskContext, opts *mail.Options, domain string) error {
			assert.NotNil(t, opts.From)
			assert.NotNil(t, opts.To)
			assert.Len(t, opts.To, 1)
			assert.Equal(t, "you@you", opts.To[0].Email)
			assert.Equal(t, "noreply@"+inst.Domain, opts.From.Email)
			assert.Equal(t, "me@me", opts.ReplyTo.Email)
			assert.Equal(t, inst.Domain, domain)
			return errors.New("yes")
		}
		defer func() {
			sendMail = doSendMail
		}()
		msg, _ := job.NewMessage(mail.Options{
			Mode:    "from",
			Subject: "Up?",
			To:      []*mail.Address{{Email: "you@you"}},
			Parts: []*mail.Part{
				{
					Type: "text/plain",
					Body: "foo",
				},
			},
			Locale: "en",
		})
		j := job.NewJob(inst, &job.JobRequest{
			Message:    msg,
			WorkerType: "sendmail",
		})
		ctx, cancel := job.NewTaskContext("123", j, inst)
		defer cancel()
		err := SendMail(ctx)
		if assert.Error(t, err) {
			assert.Equal(t, "yes", err.Error())
		}
	})

	t.Run("send campaign email", func(t *testing.T) {
		sendMail = func(_ *job.TaskContext, opts *mail.Options, domain string) error {
			assert.NotNil(t, opts.From)
			assert.NotNil(t, opts.To)
			assert.Len(t, opts.To, 1)
			assert.Equal(t, "me@me", opts.To[0].Email)
			assert.Equal(t, "noreply@"+inst.Domain, opts.From.Email)
			assert.Equal(t, inst.Domain, domain)
			return errors.New("yes")
		}
		defer func() {
			sendMail = doSendMail
		}()
		msg, _ := job.NewMessage(mail.Options{
			Mode:    mail.ModeCampaign,
			Subject: "Awesome content",
			Parts: []*mail.Part{
				{
					Type: "text/plain",
					Body: "foo",
				},
			},
			Locale: "en",
		})
		j := job.NewJob(inst, &job.JobRequest{
			Message:    msg,
			WorkerType: "sendmail",
		})
		ctx, cancel := job.NewTaskContext("123", j, inst)
		defer cancel()
		err := SendMail(ctx)
		if assert.Error(t, err) {
			assert.Equal(t, "yes", err.Error())
		}
	})
}

func mailServer(t *testing.T, serverString string, clientStrings []string, expectedHeader map[string]string, send func(string, int) error) {
	serverString = strings.Join(strings.Split(serverString, "\n"), "\r\n")
	for i, s := range clientStrings {
		clientStrings[i] = strings.Join(strings.Split(s, "\n"), "\r\n")
	}

	var cmdbuf bytes.Buffer
	bcmdbuf := bufio.NewWriter(&cmdbuf)
	headers := make(map[string]string)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Unable to to create listener: %v", err)
	}
	defer l.Close()

	// prevent data race on bcmdbuf
	done := make(chan struct{})
	go func(data []string) {
		defer close(done)

		conn, err := l.Accept()
		if err != nil {
			t.Errorf("Accept error: %v", err)
			return
		}
		defer conn.Close()

		tc := textproto.NewConn(conn)
		readdata := false
		readhead := false
		for i := 0; i < len(data) && data[i] != ""; i++ {
			_ = tc.PrintfLine("%s", data[i])
			for len(data[i]) >= 4 && data[i][3] == '-' {
				i++
				_ = tc.PrintfLine("%s", data[i])
			}
			if data[i] == "221 Goodbye" {
				return
			}
			read := false
			for !read || data[i] == "354 Go ahead" {
				msg, err := tc.ReadLine()
				if readdata && msg != "." {
					if msg == "" {
						readhead = true
						read = true
						continue
					}
					// skip multipart --boundaries
					if readhead &&
						(len(msg) <= 1 || msg[0] != '-' || msg[1] != '-') {
						_, _ = bcmdbuf.Write([]byte(msg + "\r\n"))
					} else {
						parts := strings.SplitN(msg, ": ", 2)
						if len(parts) == 2 {
							headers[parts[0]] = parts[1]
						}
					}
				} else {
					if msg == "." {
						readdata = false
					}
					if msg == "DATA" {
						readdata = true
					}
					_, _ = bcmdbuf.Write([]byte(msg + "\r\n"))
					read = true
				}
				if err != nil {
					t.Errorf("Read error: %v", err)
					return
				}
				if data[i] == "354 Go ahead" && msg == "." {
					break
				}
			}
		}
	}(strings.Split(serverString, "\r\n"))

	host, port, _ := net.SplitHostPort(l.Addr().String())
	portI, _ := strconv.Atoi(port)
	if err := send(host, portI); err != nil {
		t.Errorf("%v", err)
	}

	<-done
	bcmdbuf.Flush()
	actualcmds := cmdbuf.String()
	for _, s := range clientStrings {
		assert.Contains(t, actualcmds, s)
	}
	assert.EqualValues(t, expectedHeader, headers)
}

func TestAddPartKeepsLineBreaks(t *testing.T) {
	for name, body := range map[string]string{
		"lone CR then non-ASCII": "a\ré\nb",
		"CRLF then non-ASCII":    "a\r\né\nb",
		"LF then non-ASCII":      "a\né\nb",
	} {
		t.Run(name, func(t *testing.T) {
			msg := gomail.NewMessage()
			msg.SetHeader("From", "noreply@cozy.example.com")
			msg.SetHeader("To", "you@example.com")
			require.NoError(t, addPart(msg, &mail.Part{Type: "text/plain", Body: body}))

			var buf bytes.Buffer
			_, err := msg.WriteTo(&buf)
			require.NoError(t, err)
			m, err := stdmail.ReadMessage(&buf)
			require.NoError(t, err)
			require.Equal(t, "quoted-printable", m.Header.Get("Content-Transfer-Encoding"))
			got, err := io.ReadAll(quotedprintable.NewReader(m.Body))
			require.NoError(t, err)
			assert.Equal(t, "a\né\nb", strings.ReplaceAll(string(got), "\r\n", "\n"))
		})
	}
}
