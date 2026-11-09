package mails

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	stdmail "net/mail"
	"net/textproto"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/cozy/cozy-stack/pkg/mail"
	"github.com/cozy/gomail"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAttachmentFilenameEncoding(t *testing.T) {
	for _, name := range []string{"notes.txt", "résumé.pdf", `x".txt`, "a.txt\r\nX-Evil: 1\r\n\r\nbody", "dirI\xff\x7fp.d"} {
		for _, withBody := range []bool{false, true} {
			t.Run(name+"/body="+strconv.FormatBool(withBody), func(t *testing.T) {
				email := gomail.NewMessage()
				if withBody {
					email.SetBody("text/plain", "message")
				}
				addAttachment(email, &mail.Attachment{Filename: name, Content: []byte("attachment content")})
				var wire bytes.Buffer
				_, err := email.WriteTo(&wire)
				require.NoError(t, err)
				message, err := stdmail.ReadMessage(&wire)
				require.NoError(t, err)
				assert.Empty(t, message.Header.Get("X-Evil"))
				headers, body := textproto.MIMEHeader(message.Header), message.Body
				if withBody {
					_, params, err := mime.ParseMediaType(headers.Get("Content-Type"))
					require.NoError(t, err)
					reader := multipart.NewReader(body, params["boundary"])
					_, err = reader.NextPart()
					require.NoError(t, err)
					attachment, err := reader.NextPart()
					require.NoError(t, err)
					headers, body = attachment.Header, attachment
				}
				for header, parameter := range map[string]string{"Content-Type": "name", "Content-Disposition": "filename"} {
					_, params, err := mime.ParseMediaType(headers.Get(header))
					require.NoError(t, err)
					assert.Equal(t, filepath.Base(name), params[parameter])
				}
				content, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, body))
				require.NoError(t, err)
				assert.Equal(t, "attachment content", string(content))
			})
		}
	}
}
