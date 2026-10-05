package app

import (
	"testing"

	"github.com/cozy/cozy-stack/model/feature"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExternalServiceURLFromFlags(t *testing.T) {
	config.UseTestFile(t)
	ins := &instance.Instance{Domain: "cozy.example.net"}
	const flagKey = "calendar_service_url"
	assert.Nil(t, ExternalServiceURLFromFlags(ins, "calendar", flagKey, nil))
	assert.Nil(t, ExternalServiceURLFromFlags(ins, "calendar", "", &feature.Flags{}))
	for _, tc := range []struct {
		name  string
		value interface{}
		want  string
	}{
		{"missing", nil, ""},
		{"not_a_string", true, ""},
		{"not_a_url", "not-a-url", ""},
		{"malformed", "https://%", ""},
		{"csp_separator", "https://x.example;sandbox", ""},
		{"scheme", "javascript://x", ""},
		{"userinfo", "https://u:p@calendar.external.test/", ""},
		{"cozy_host", "https://calendar.cozy.example.net/app/", ""},
		{"external", "https://calendar.external.test", "https://calendar.external.test"},
		{"path_prefix", "https://calendar.external.test/app/", "https://calendar.external.test/app/"},
		{"http_with_port", "http://localhost:3000/app/", "http://localhost:3000/app/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flags := &feature.Flags{M: map[string]interface{}{flagKey: tc.value}}
			got := ExternalServiceURLFromFlags(ins, "calendar", flagKey, flags)
			if tc.want == "" {
				assert.Nil(t, got)
			} else {
				require.NotNil(t, got)
				assert.Equal(t, tc.want, got.String())
			}
		})
	}
}

func TestFindRoute(t *testing.T) {
	manifest := &WebappManifest{}
	manifest.val.Routes = make(Routes)
	manifest.val.Routes["/foo"] = Route{Folder: "/foo", Index: "index.html"}
	manifest.val.Routes["/foo/bar"] = Route{Folder: "/bar", Index: "index.html"}
	manifest.val.Routes["/foo/qux"] = Route{Folder: "/qux", Index: "index.html"}
	manifest.val.Routes["/public"] = Route{Folder: "/public", Index: "public.html", Public: true}
	manifest.val.Routes["/admin"] = Route{Folder: "/admin", Index: "admin.html"}
	manifest.val.Routes["/admin/special"] = Route{Folder: "/special", Index: "admin.html"}

	ctx, rest := manifest.FindRoute("/admin")
	assert.Equal(t, "/admin", ctx.Folder)
	assert.Equal(t, "admin.html", ctx.Index)
	assert.Equal(t, false, ctx.Public)
	assert.Equal(t, "", rest)

	ctx, rest = manifest.FindRoute("/public/")
	assert.Equal(t, "/public", ctx.Folder)
	assert.Equal(t, "public.html", ctx.Index)
	assert.Equal(t, true, ctx.Public)
	assert.Equal(t, "", rest)

	ctx, rest = manifest.FindRoute("/public")
	assert.Equal(t, "/public", ctx.Folder)
	assert.Equal(t, "", rest)

	ctx, rest = manifest.FindRoute("/public/app.js")
	assert.Equal(t, "/public", ctx.Folder)
	assert.Equal(t, "app.js", rest)

	ctx, rest = manifest.FindRoute("/foo/admin/special")
	assert.Equal(t, "/foo", ctx.Folder)
	assert.Equal(t, "admin/special", rest)

	ctx, rest = manifest.FindRoute("/admin/special/foo")
	assert.Equal(t, "/special", ctx.Folder)
	assert.Equal(t, "foo", rest)

	ctx, rest = manifest.FindRoute("/foo/bar.html")
	assert.Equal(t, "/foo", ctx.Folder)
	assert.Equal(t, "bar.html", rest)

	ctx, rest = manifest.FindRoute("/foo/baz")
	assert.Equal(t, "/foo", ctx.Folder)
	assert.Equal(t, "baz", rest)

	ctx, rest = manifest.FindRoute("/foo/bar")
	assert.Equal(t, "/bar", ctx.Folder)
	assert.Equal(t, "", rest)

	ctx, _ = manifest.FindRoute("/")
	assert.Equal(t, "", ctx.Folder)
}

func TestNoRegression217(t *testing.T) {
	var man WebappManifest
	man.val.Routes = make(Routes)
	man.val.Routes["/"] = Route{
		Folder: "/",
		Index:  "index.html",
		Public: false,
	}

	ctx, rest := man.FindRoute("/any/path")
	assert.Equal(t, "/", ctx.Folder)
	assert.Equal(t, "any/path", rest)
}

func TestFindIntent(t *testing.T) {
	var man WebappManifest
	found := man.FindIntent("PICK", "io.cozy.files")
	assert.Nil(t, found)

	man.val.Intents = []Intent{
		{
			Action: "PICK",
			Types:  []string{"io.cozy.contacts", "io.cozy.calendars"},
			Href:   "/pick",
		},
		{
			Action: "OPEN",
			Types:  []string{"io.cozy.files", "image/gif"},
			Href:   "/open",
		},
		{
			Action: "EDIT",
			Types:  []string{"image/*"},
			Href:   "/open",
		},
	}
	found = man.FindIntent("PICK", "io.cozy.files")
	assert.Nil(t, found)
	found = man.FindIntent("OPEN", "io.cozy.contacts")
	assert.Nil(t, found)
	found = man.FindIntent("PICK", "io.cozy.contacts")
	assert.NotNil(t, found)
	assert.Equal(t, "PICK", found.Action)
	found = man.FindIntent("OPEN", "io.cozy.files")
	assert.NotNil(t, found)
	assert.Equal(t, "OPEN", found.Action)
	found = man.FindIntent("open", "io.cozy.files")
	assert.NotNil(t, found)
	assert.Equal(t, "OPEN", found.Action)

	found = man.FindIntent("OPEN", "image/gif")
	assert.NotNil(t, found)
	assert.Equal(t, "OPEN", found.Action)
	found = man.FindIntent("EDIT", "image/gif")
	assert.NotNil(t, found)
	assert.Equal(t, "EDIT", found.Action)

	man.val.Intents = []Intent{
		{
			Action: "PICK",
			Href:   "/pick",
		},
	}
	found = man.FindIntent("PICK", "io.cozy.files")
	assert.Nil(t, found)
}

func Test_GetBySlug(t *testing.T) {
	t.Run("with an invalid appType", func(t *testing.T) {
		man, err := GetBySlug(nil, "some-slug", consts.AppType(0))
		assert.Nil(t, man)
		assert.ErrorIs(t, err, ErrInvalidAppType)
	})
}
