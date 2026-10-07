package app_test

import (
	"testing"

	"github.com/cozy/cozy-stack/model/app"
	"github.com/stretchr/testify/assert"
)

func TestFrameAncestorOrigins(t *testing.T) {
	t.Run("GivesOriginsAsBrowsersSerializeThem", func(t *testing.T) {
		assert.Equal(t,
			[]string{"https://mail.example.com", "http://alice-mail.example.com"},
			app.FrameAncestorOrigins("https://Mail.Example.com:443/app/", "HTTP://alice-mail.example.com:80"))
	})

	t.Run("KeepsAPortThatIsNotTheDefaultOne", func(t *testing.T) {
		assert.Equal(t,
			[]string{"https://mail.example.com:8443", "http://localhost:8080"},
			app.FrameAncestorOrigins("https://mail.example.com:8443", "http://localhost:8080/"))
	})

	t.Run("WritesAnInternationalizedDomainInPunycode", func(t *testing.T) {
		assert.Equal(t,
			[]string{"https://xn--ml-via.example.com"},
			app.FrameAncestorOrigins("https://mäl.example.com"))
	})

	t.Run("GivesEachOriginOnce", func(t *testing.T) {
		assert.Equal(t,
			[]string{"https://mail.example.com"},
			app.FrameAncestorOrigins("https://mail.example.com", "https://MAIL.example.com:443/"))
	})

	t.Run("LeavesOutWhatIsNoHTTPURL", func(t *testing.T) {
		assert.Equal(t,
			[]string{},
			app.FrameAncestorOrigins("not-a-url", "ftp://files.example.com", ""))
	})
}
