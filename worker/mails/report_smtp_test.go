package mails

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/http/httptest"
	netmail "net/mail"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cozy/gomail"
	"github.com/stretchr/testify/require"
)

type capturedReportMail struct{ recipient, raw string }

// Use real TCP and collect the SMTP envelope and MIME before sending the final ACK.
func reportSMTP(t *testing.T, onHello func(), loseAck bool) (*gomail.DialerOptions, <-chan capturedReportMail) {
	t.Helper()
	return reportSMTPWithFaults(t, reportSMTPFaults{onHello: func(int) {
		if onHello != nil {
			onHello()
		}
	}, loseAck: loseAck})
}

type reportSMTPFaults struct {
	onHello    func(int)
	dropMail   func(int) bool
	onEOF      func()
	onQuit     func()
	dataReply  string
	loseAck    bool
	ignoreQuit bool
	tlsConfig  *tls.Config
	nativeTLS  bool
}

func reportSMTPWithFaults(t *testing.T, faults reportSMTPFaults) (*gomail.DialerOptions, <-chan capturedReportMail) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	messages := make(chan capturedReportMail, 30)
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for number := 1; ; number++ {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
				secure := faults.nativeTLS
				if secure {
					conn = tls.Server(conn, faults.tlsConfig)
				}
				r := textproto.NewReader(bufio.NewReader(conn))
				w := bufio.NewWriter(conn)
				reply := func(s string) { _, _ = fmt.Fprintf(w, "%s\r\n", s); _ = w.Flush() }
				reply("220 report test")
				var to string
				for {
					line, err := r.ReadLine()
					if err != nil {
						if err == io.EOF && faults.onEOF != nil {
							faults.onEOF()
						}
						return
					}
					switch {
					case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
						if faults.onHello != nil {
							faults.onHello(number)
						}
						if faults.tlsConfig != nil && !secure {
							reply("250-report test\r\n250 STARTTLS")
						} else {
							reply("250 report test")
						}
					case line == "STARTTLS" && faults.tlsConfig != nil && !secure:
						reply("220 proceed")
						conn = tls.Server(conn, faults.tlsConfig)
						r = textproto.NewReader(bufio.NewReader(conn))
						w = bufio.NewWriter(conn)
						secure = true
					case strings.HasPrefix(line, "MAIL FROM:"):
						if faults.dropMail != nil && faults.dropMail(number) {
							return
						}
						reply("250 OK")
					case strings.HasPrefix(line, "RCPT TO:"):
						to = line
						reply("250 OK")
					case line == "DATA":
						reply("354 Continue")
						raw, err := r.ReadDotBytes()
						if err != nil {
							return
						}
						select {
						case messages <- capturedReportMail{to, string(raw)}:
						case <-done:
							return
						}
						if faults.loseAck {
							return
						}
						if faults.dataReply != "" {
							reply(faults.dataReply)
						} else {
							reply("250 Accepted")
						}
					case line == "QUIT":
						if faults.onQuit != nil {
							faults.onQuit()
						}
						if !faults.ignoreQuit {
							reply("221 Bye")
						}
						// Keep reading so the test can observe client-side closure.
					default:
						reply("250 OK")
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { close(done); ln.Close(); wg.Wait() })
	addr := ln.Addr().(*net.TCPAddr)
	return &gomail.DialerOptions{Host: "127.0.0.1", Port: addr.Port, DisableTLS: true}, messages
}

func TestExpiredLinkReportSMTPRequiresConfiguredTLS(t *testing.T) {
	// Borrow the standard httptest certificate; the SMTP exchange itself uses
	// a separate TCP fixture. The certificate is intentionally not trusted.
	certSource := httptest.NewTLSServer(nil)
	tlsConfig := certSource.TLS
	certSource.Close()

	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("nativeTLS=%t", native), func(t *testing.T) {
			opts, messages := reportSMTPWithFaults(t, reportSMTPFaults{
				tlsConfig: tlsConfig, nativeTLS: native,
			})
			opts.DisableTLS, opts.NativeTLS = false, native
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _, err := dialReportSMTP(ctx, opts)
			require.ErrorContains(t, err, "certificate")
			opts.SkipCertificateValidation = true
			client, transport, err := dialReportSMTP(ctx, opts)
			require.NoError(t, err)
			defer transport.Close()
			state, ok := client.TLSConnectionState()
			require.True(t, ok)
			require.True(t, state.HandshakeComplete)
			require.NoError(t, submitReportMail(client, "stack@example.test", []string{"owner@example.test"}, strings.NewReader("Subject: test\r\n\r\nfixture")))
			require.Equal(t, "RCPT TO:<owner@example.test>", receiveReportMail(t, messages).recipient)
		})
	}

	t.Run("no STARTTLS", func(t *testing.T) {
		opts, messages := reportSMTP(t, nil, false)
		opts.DisableTLS = false
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _, err := dialReportSMTP(ctx, opts)
		require.ErrorContains(t, err, "did not advertise STARTTLS")
		requireNoReportMail(t, messages)
	})
}

type reportMailLeaf struct {
	header textproto.MIMEHeader
	body   []byte
}

func reportMailParts(t *testing.T, raw string) (netmail.Header, []reportMailLeaf) {
	t.Helper()
	msg, err := netmail.ReadMessage(strings.NewReader(raw))
	require.NoError(t, err)
	var leaves []reportMailLeaf
	var walk func(textproto.MIMEHeader, io.Reader)
	walk = func(header textproto.MIMEHeader, body io.Reader) {
		media, params, err := mime.ParseMediaType(header.Get("Content-Type"))
		require.NoError(t, err)
		if strings.HasPrefix(media, "multipart/") {
			parts := multipart.NewReader(body, params["boundary"])
			for {
				part, err := parts.NextRawPart()
				if err == io.EOF {
					return
				}
				require.NoError(t, err)
				walk(part.Header, part)
			}
		}
		switch header.Get("Content-Transfer-Encoding") {
		case "quoted-printable":
			body = quotedprintable.NewReader(body)
		case "base64":
			body = base64.NewDecoder(base64.StdEncoding, body)
		}
		b, err := io.ReadAll(body)
		require.NoError(t, err)
		leaves = append(leaves, reportMailLeaf{header, b})
	}
	walk(textproto.MIMEHeader(msg.Header), msg.Body)
	return msg.Header, leaves
}
