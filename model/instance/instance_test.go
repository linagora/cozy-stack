package instance_test

import (
	"encoding/json"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/crypto"
	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstance(t *testing.T) {
	config.UseTestFile(t)

	t.Run("ResolveOrdinaryDirID", func(t *testing.T) {
		inst := &instance.Instance{}
		for _, id := range []string{"", "existing-id", "io.cozy.apps/unknown"} {
			resolved, err := inst.ResolveDirID(id)
			require.NoError(t, err)
			assert.Equal(t, id, resolved)
		}
	})

	t.Run("Subdomain", func(t *testing.T) {
		inst := &instance.Instance{
			Domain: "foo.example.com",
		}
		cfg := config.GetConfig()
		was := cfg.Subdomains
		defer func() { cfg.Subdomains = was }()

		cfg.Subdomains = config.NestedSubdomains
		u := inst.SubDomain("calendar")
		assert.Equal(t, "https://calendar.foo.example.com/", u.String())

		cfg.Subdomains = config.FlatSubdomains
		u = inst.SubDomain("calendar")
		assert.Equal(t, "https://foo-calendar.example.com/", u.String())
	})

	t.Run("BuildAppToken", func(t *testing.T) {
		inst := &instance.Instance{
			Domain:     "test-ctx-token.example.com",
			SessSecret: crypto.GenerateRandomBytes(64),
		}

		tokenString := inst.BuildAppToken("my-app", "sessionid")
		token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
			_, ok := token.Method.(*jwt.SigningMethodHMAC)
			assert.True(t, ok, "The signing method should be HMAC")
			return inst.SessionSecret(), nil
		})
		assert.NoError(t, err)
		assert.True(t, token.Valid)

		claims, ok := token.Claims.(jwt.MapClaims)
		assert.True(t, ok, "Claims can be parsed as standard claims")
		assert.Equal(t, []interface{}{"app"}, claims["aud"])
		assert.Equal(t, "test-ctx-token.example.com", claims["iss"])
		assert.Equal(t, "my-app", claims["sub"])
	})

	t.Run("GetContextWithSponsorships", func(t *testing.T) {
		cfg := config.GetConfig()
		was := cfg.Contexts
		defer func() { cfg.Contexts = was }()

		cfg.Contexts = map[string]interface{}{
			"context": map[string]interface{}{
				"logos": map[string]interface{}{
					"coachco2": map[string]interface{}{
						"light": []interface{}{
							map[string]interface{}{"src": "/logos/main_cozy.png", "alt": "Cozy Cloud"},
						},
					},
					"home": map[string]interface{}{
						"light": []interface{}{
							map[string]interface{}{"src": "/logos/main_cozy.png", "alt": "Cozy Cloud", "type": "main"},
							map[string]interface{}{"src": "/logos/partner1.png", "alt": "Partner1", "type": "secondary"},
						},
						"dark": []interface{}{
							// no main
							map[string]interface{}{"src": "/logos/partner1.png", "alt": "Partner1", "type": "secondary"},
						},
					},
				},
			},
			"sponsor1": map[string]interface{}{
				"move_url": "http://move.cozy.beta/",
				"logos": map[string]interface{}{
					"coachco2": map[string]interface{}{
						"dark": []interface{}{
							map[string]interface{}{"src": "/logos/main_cozy.png", "alt": "Cozy Cloud"},
						},
					},
					"home": map[string]interface{}{
						"light": []interface{}{
							map[string]interface{}{"src": "/logos/main_cozy.png", "alt": "Cozy Cloud", "type": "main"},
							map[string]interface{}{"src": "/logos/partner1.png", "alt": "Partner1", "type": "secondary"},
							map[string]interface{}{"src": "/logos/partner2.png", "alt": "Partner2", "type": "secondary"},
						},
						"dark": []interface{}{
							map[string]interface{}{"src": "/logos/main_cozy.png", "alt": "Cozy Cloud", "type": "main"},
							map[string]interface{}{"src": "/logos/partner2.png", "alt": "Partner2"},
							map[string]interface{}{"src": "/logos/partner1.png", "alt": "Partner1"},
						},
					},
				},
			},
			"sponsor2": map[string]interface{}{
				"logos": map[string]interface{}{
					"mespapiers": map[string]interface{}{
						"dark": []interface{}{
							map[string]interface{}{"src": "/logos/main_cozy.png", "alt": "Cozy Cloud"},
						},
					},
					"home": map[string]interface{}{
						"light": []interface{}{
							map[string]interface{}{"src": "/logos/main_cozy.png", "alt": "Cozy Cloud", "type": "main"},
							map[string]interface{}{"src": "/logos/partner3.png", "alt": "Partner3", "type": "secondary"},
							map[string]interface{}{"src": "/logos/partner2.png", "alt": "Partner2", "type": "secondary"},
						},
					},
				},
			},
		}

		inst := &instance.Instance{
			Domain:       "foo.example.com",
			ContextName:  "context",
			Sponsorships: []string{"sponsor1", "sponsor2"},
		}
		result := inst.GetContextWithSponsorships()
		bytes, err := json.MarshalIndent(result, "", "  ")
		require.NoError(t, err)
		expected := `{
  "logos": {
    "coachco2": {
      "light": [
        {
          "src": "/logos/main_cozy.png",
          "alt": "Cozy Cloud"
        }
      ]
    },
    "home": {
      "light": [
        {
          "src": "/logos/main_cozy.png",
          "alt": "Cozy Cloud",
          "type": "main"
        },
        {
          "src": "/logos/partner1.png",
          "alt": "Partner1",
          "type": "secondary"
        },
        {
          "src": "/ext/sponsor1/logos/partner2.png",
          "alt": "Partner2",
          "type": "secondary"
        },
        {
          "src": "/ext/sponsor2/logos/partner3.png",
          "alt": "Partner3",
          "type": "secondary"
        }
      ],
      "dark": [
        {
          "src": "/logos/partner1.png",
          "alt": "Partner1",
          "type": "secondary"
        },
        {
          "src": "/ext/sponsor1/logos/partner2.png",
          "alt": "Partner2"
        }
      ]
    }
  }
}`
		assert.Equal(t, expected, string(bytes))
	})

	t.Run("IsOrganizationInstance", func(t *testing.T) {
		t.Run("TrueWhenSlugMatchesOrgID", func(t *testing.T) {
			inst := &instance.Instance{
				Domain: "alice.twake.app",
				OrgID:  "alice",
			}
			assert.True(t, inst.IsOrganizationInstance())
		})

		t.Run("FalseWhenSlugDoesNotMatchOrgID", func(t *testing.T) {
			inst := &instance.Instance{
				Domain: "bob.twake.app",
				OrgID:  "alice",
			}
			assert.False(t, inst.IsOrganizationInstance())
		})

		t.Run("FalseWhenOrgIDIsEmpty", func(t *testing.T) {
			inst := &instance.Instance{Domain: "alice.twake.app"}
			assert.False(t, inst.IsOrganizationInstance())
		})
	})

	t.Run("BannerSettings", func(t *testing.T) {
		cfg := config.GetConfig()
		was := cfg.Contexts
		defer func() { cfg.Contexts = was }()
		cfg.Contexts = map[string]interface{}{
			"banner-all": map[string]interface{}{
				"banner": map[string]interface{}{
					"enabled":            true,
					"command_categories": []interface{}{"*"},
					"cta_hosts":          []interface{}{"*"},
				},
			},
			"banner-listed": map[string]interface{}{
				"banner": map[string]interface{}{
					"enabled":            true,
					"command_categories": []interface{}{"billing"},
					"cta_hosts":          []interface{}{"manager.example.org"},
				},
			},
			"banner-malformed": map[string]interface{}{
				"banner": map[string]interface{}{"enabled": "yes"},
			},
		}

		all := (&instance.Instance{ContextName: "banner-all"}).BannerSettings()
		assert.True(t, all.Enabled)
		assert.True(t, all.AllowsCategory("trial"))
		assert.False(t, all.AllowsCTAHost("evil.example"), "CTA hosts have no wildcard")

		listed := (&instance.Instance{ContextName: "banner-listed"}).BannerSettings()
		assert.True(t, listed.AllowsCategory("billing"))
		assert.False(t, listed.AllowsCategory("trial"))
		assert.True(t, listed.AllowsCTAHost("manager.example.org"))

		assert.False(t, (&instance.Instance{ContextName: "banner-malformed"}).BannerSettings().Enabled)
		assert.False(t, (&instance.Instance{ContextName: "no-such-context"}).BannerSettings().Enabled)
	})
}
