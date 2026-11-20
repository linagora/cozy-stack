package mails

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	messages := make(chan capturedReportMail, 30)
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
				r := textproto.NewReader(bufio.NewReader(conn))
				w := bufio.NewWriter(conn)
				reply := func(s string) { _, _ = fmt.Fprintf(w, "%s\r\n", s); _ = w.Flush() }
				reply("220 report test")
				var to string
				for {
					line, err := r.ReadLine()
					if err != nil {
						return
					}
					switch {
					case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
						if onHello != nil {
							onHello()
						}
						reply("250 report test")
					case strings.HasPrefix(line, "MAIL FROM:"):
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
						if loseAck {
							return
						}
						reply("250 Accepted")
					case line == "QUIT":
						reply("221 Bye")
						return
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
