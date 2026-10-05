package intent

import (
	"context"
	"testing"

	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

func TestIntents(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)

	ins := &instance.Instance{Domain: "cozy.example.net"}

	err := couchdb.ResetDB(ins, consts.Apps)
	require.NoError(t, err)

	g, _ := errgroup.WithContext(context.Background())
	couchdb.DefineIndexes(g, ins, couchdb.IndexesByDoctype(consts.Apps))
	require.NoError(t, g.Wait())

	t.Cleanup(func() {
		_ = couchdb.DeleteDB(ins, consts.Apps)
	})

	t.Run("GenerateHref", func(t *testing.T) {
		intent := &Intent{IID: "6fba9dd6-1487-11e7-b90d-130a5dedd6d6"}

		href := intent.GenerateHref(ins, "files", "/pick")
		assert.Equal(t, "https://files.cozy.example.net/pick?intent=6fba9dd6-1487-11e7-b90d-130a5dedd6d6", href)

		href = intent.GenerateHref(ins, "files", "/view")
		assert.Equal(t, "https://files.cozy.example.net/view?intent=6fba9dd6-1487-11e7-b90d-130a5dedd6d6", href)
	})

	t.Run("FillServices", func(t *testing.T) {
		files := &couchdb.JSONDoc{
			Type: consts.Apps,
			M: map[string]interface{}{
				"_id":  consts.Apps + "/files",
				"slug": "files",
				"intents": []app.Intent{
					{
						Action: "PICK",
						Types:  []string{"io.cozy.files", "image/gif"},
						Href:   "/pick",
					},
				},
			},
		}
		err := couchdb.CreateNamedDoc(ins, files)
		assert.NoError(t, err)
		photos := &couchdb.JSONDoc{
			Type: consts.Apps,
			M: map[string]interface{}{
				"_id":  consts.Apps + "/photos",
				"slug": "photos",
				"intents": []app.Intent{
					{
						Action: "PICK",
						Types:  []string{"image/*"},
						Href:   "/picker",
					},
					{
						Action: "VIEW",
						Types:  []string{"io.cozy.files"},
						Href:   "/viewer",
					},
				},
			},
		}
		err = couchdb.CreateNamedDoc(ins, photos)
		assert.NoError(t, err)

		intent := &Intent{
			IID:    "6b44d8d0-148b-11e7-a1cf-a38d75a77df6",
			Action: "PICK",
			Type:   "io.cozy.files",
		}
		err = intent.FillServices(ins)
		assert.NoError(t, err)
		assert.Len(t, intent.Services, 1)
		service := intent.Services[0]
		assert.Equal(t, "files", service.Slug)
		assert.Equal(t, "https://files.cozy.example.net/pick?intent=6b44d8d0-148b-11e7-a1cf-a38d75a77df6", service.Href)

		intent = &Intent{
			IID:    "6b44d8d0-148b-11e7-a1cf-a38d75a77df6",
			Action: "view",
			Type:   "io.cozy.files",
		}
		err = intent.FillServices(ins)
		assert.NoError(t, err)
		assert.Len(t, intent.Services, 1)
		service = intent.Services[0]
		assert.Equal(t, "photos", service.Slug)
		assert.Equal(t, "https://photos.cozy.example.net/viewer?intent=6b44d8d0-148b-11e7-a1cf-a38d75a77df6", service.Href)

		intent = &Intent{
			IID:    "6b44d8d0-148b-11e7-a1cf-a38d75a77df6",
			Action: "PICK",
			Type:   "image/gif",
		}
		err = intent.FillServices(ins)
		assert.NoError(t, err)
		assert.Len(t, intent.Services, 2)
		service = intent.Services[0]
		assert.Equal(t, "files", service.Slug)
		assert.Equal(t, "https://files.cozy.example.net/pick?intent=6b44d8d0-148b-11e7-a1cf-a38d75a77df6", service.Href)
		service = intent.Services[1]
		assert.Equal(t, "photos", service.Slug)
		assert.Equal(t, "https://photos.cozy.example.net/picker?intent=6b44d8d0-148b-11e7-a1cf-a38d75a77df6", service.Href)

		intent = &Intent{
			IID:    "6b44d8d0-148b-11e7-a1cf-a38d75a77df6",
			Action: "VIEW",
			Type:   "image/gif",
		}
		err = intent.FillServices(ins)
		assert.NoError(t, err)
		assert.Len(t, intent.Services, 0)
	})

	t.Run("GenerateHrefWithServiceURLFlag", func(t *testing.T) {
		calendar := &couchdb.JSONDoc{
			Type: consts.Apps,
			M: map[string]interface{}{
				"_id":              consts.Apps + "/calendar",
				"slug":             "calendar",
				"service_url_flag": "calendar_service_url",
				"intents": []app.Intent{
					{
						Action: "OPEN",
						Types:  []string{"io.cozy.calendar.events"},
						Href:   "/intents#/open",
					},
				},
			},
		}
		require.NoError(t, couchdb.CreateNamedDoc(ins, calendar))
		t.Cleanup(func() { ins.FeatureFlags = nil })

		intent := &Intent{IID: "abc"}

		// No flag value: fallback to the cozy subdomain
		ins.FeatureFlags = nil
		href := intent.GenerateHref(ins, "calendar", "/intents#/open")
		assert.Equal(t, "https://calendar.cozy.example.net/intents?intent=abc#/open", href)

		// No flag value and a target without path: unchanged cozy subdomain href
		href = intent.GenerateHref(ins, "calendar", "#/open")
		assert.Equal(t, "https://calendar.cozy.example.net/?intent=abc#/open", href)

		// Flag value that is not a string: fallback to the cozy subdomain
		ins.FeatureFlags = map[string]interface{}{"calendar_service_url": true}
		href = intent.GenerateHref(ins, "calendar", "/intents#/open")
		assert.Equal(t, "https://calendar.cozy.example.net/intents?intent=abc#/open", href)

		// Invalid flag value: fallback to the cozy subdomain
		ins.FeatureFlags = map[string]interface{}{"calendar_service_url": "not-a-url"}
		href = intent.GenerateHref(ins, "calendar", "/intents#/open")
		assert.Equal(t, "https://calendar.cozy.example.net/intents?intent=abc#/open", href)

		// Host with a CSP separator: fallback to the cozy subdomain
		ins.FeatureFlags = map[string]interface{}{"calendar_service_url": "https://x.example;sandbox"}
		href = intent.GenerateHref(ins, "calendar", "/intents#/open")
		assert.Equal(t, "https://calendar.cozy.example.net/intents?intent=abc#/open", href)

		// Scheme other than http(s): fallback to the cozy subdomain
		ins.FeatureFlags = map[string]interface{}{"calendar_service_url": "javascript://x"}
		href = intent.GenerateHref(ins, "calendar", "/intents#/open")
		assert.Equal(t, "https://calendar.cozy.example.net/intents?intent=abc#/open", href)

		// URL with userinfo: fallback to the cozy subdomain
		ins.FeatureFlags = map[string]interface{}{"calendar_service_url": "https://u:p@calendar.external.test/"}
		href = intent.GenerateHref(ins, "calendar", "/intents#/open")
		assert.Equal(t, "https://calendar.cozy.example.net/intents?intent=abc#/open", href)

		// Valid flag value without path
		ins.FeatureFlags = map[string]interface{}{"calendar_service_url": "https://calendar.external.test"}
		href = intent.GenerateHref(ins, "calendar", "/intents#/open")
		assert.Equal(t, "https://calendar.external.test/intents?intent=abc#/open", href)

		// Valid flag value with a path prefix and a trailing slash
		ins.FeatureFlags = map[string]interface{}{"calendar_service_url": "https://calendar.external.test/app/"}
		href = intent.GenerateHref(ins, "calendar", "/intents#/open")
		assert.Equal(t, "https://calendar.external.test/app/intents?intent=abc#/open", href)

		// Path prefix and a target without leading slash
		href = intent.GenerateHref(ins, "calendar", "intents#/open")
		assert.Equal(t, "https://calendar.external.test/app/intents?intent=abc#/open", href)

		// FillServices uses the external URL too
		in := &Intent{IID: "abc", Action: "OPEN", Type: "io.cozy.calendar.events"}
		require.NoError(t, in.FillServices(ins))
		require.Len(t, in.Services, 1)
		assert.Equal(t, "calendar", in.Services[0].Slug)
		assert.Equal(t, "https://calendar.external.test/app/intents?intent=abc#/open", in.Services[0].Href)
	})

	t.Run("FillAvailableWebapps", func(t *testing.T) {
		intent := &Intent{
			IID:    "6b44d8d0-148b-11e7-a1cf-a38d75a77df6",
			Action: "REDIRECT",
			Type:   "io.cozy.accounts",
		}
		err := intent.FillAvailableWebapps(ins)
		assert.NoError(t, err)

		// Should have Home
		assert.Equal(t, 1, len(intent.AvailableApps))

		res := map[string]interface{}{}
		for _, v := range intent.AvailableApps {
			res[v.Slug] = struct{}{}
		}

		assert.Contains(t, res, "home")
	})
}
