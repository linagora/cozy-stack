package intent

import (
	"context"
	"net/url"
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

func TestGenerateHref(t *testing.T) {
	in := &Intent{IID: "abc"}
	for _, tc := range []struct {
		name, base, target, want string
	}{
		{"pick", "https://files.cozy.example.net/", "/pick", "https://files.cozy.example.net/pick?intent=abc"},
		{"view", "https://files.cozy.example.net/", "/view", "https://files.cozy.example.net/view?intent=abc"},
		{"fragment_only", "https://calendar.cozy.example.net/", "#/open", "https://calendar.cozy.example.net/?intent=abc#/open"},
		{"external", "https://calendar.external.test", "/intents#/open", "https://calendar.external.test/intents?intent=abc#/open"},
		{"path_prefix", "https://calendar.external.test/app/", "/intents#/open", "https://calendar.external.test/app/intents?intent=abc#/open"},
		{"relative_path", "https://calendar.external.test/app/", "intents#/open", "https://calendar.external.test/app/intents?intent=abc#/open"},
		{"query_and_fragment", "https://calendar.external.test/app/?old=value#old", "/intents#/open", "https://calendar.external.test/app/intents?intent=abc#/open"},
		{"clear_fragment", "https://calendar.external.test/app/#old", "/intents", "https://calendar.external.test/app/intents?intent=abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, err := url.Parse(tc.base)
			require.NoError(t, err)
			assert.Equal(t, tc.want, in.GenerateHref(*base, tc.target))
			assert.Equal(t, tc.base, base.String())
		})
	}
}

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

	t.Run("FillServicesWithServiceURLFlag", func(t *testing.T) {
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

		for _, tc := range []struct {
			name, base, want string
		}{
			{"fallback", "", "https://calendar.cozy.example.net/intents?intent=abc#/open"},
			{"external", "https://calendar.external.test", "https://calendar.external.test/intents?intent=abc#/open"},
			{"path_prefix", "https://calendar.external.test/app/", "https://calendar.external.test/app/intents?intent=abc#/open"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ins.FeatureFlags = map[string]interface{}{"calendar_service_url": tc.base}
				in := &Intent{IID: "abc", Action: "OPEN", Type: "io.cozy.calendar.events"}
				require.NoError(t, in.FillServices(ins))
				assert.Equal(t, []Service{{Slug: "calendar", Href: tc.want}}, in.Services)
			})
		}
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
