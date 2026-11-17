package mails

import (
	"bytes"
	"io"

	"github.com/cozy/cozy-stack/pkg/assets"
	"github.com/cozy/gomail"
)

// CID images do not require remote fetches or SVG support in mail clients.
func embedReportImages(message *gomail.Message, context string) error {
	for _, name := range []string{"report-drive.png", "report-link.png", "report-clock.png"} {
		file, err := assets.Open("/mails/"+name, context)
		if err != nil {
			return err
		}
		content, err := io.ReadAll(file)
		if err != nil {
			return err
		}
		message.Embed(name, gomail.SetCopyFunc(func(w io.Writer) error {
			_, err := io.Copy(w, bytes.NewReader(content))
			return err
		}))
	}
	return nil
}
