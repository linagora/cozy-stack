package config

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/safehttp"
	"github.com/cozy/gomail"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigUnmarshal(t *testing.T) {
	require.NoError(t, Setup("./testdata/full_config.yaml"))

	cfg := GetConfig()

	assert.Equal(t, cfg.Host, "0.0.0.0")
	assert.Equal(t, cfg.Port, 8080)
	assert.Equal(t, cfg.AdminHost, "127.0.0.1")
	assert.Equal(t, cfg.AdminPort, 0)
	assert.Equal(t, cfg.AlertAddr, "foo2@bar.baz")
	assert.Equal(t, cfg.NoReplyAddr, "foo@bar.baz")
	assert.Equal(t, cfg.NoReplyName, "My Twake")
	assert.Equal(t, cfg.ReplyTo, "support@cozycloud.cc")
	assert.Equal(t, cfg.GeoDB, "/geo/db/path")
	assert.Equal(t, cfg.PasswordResetInterval, time.Hour)

	// Assets
	assert.Equal(t, true, cfg.AssetsPollingDisabled)
	assert.Equal(t, time.Hour, cfg.AssetsPollingInterval)
	assert.Equal(t, "some/assets/path", cfg.Assets)
	assert.Equal(t, map[string]string{"bank_classifier": "https://some-remote-assets-url"}, cfg.RemoteAssets)

	// FS
	fsURL, err := url.Parse("https://some-url")
	require.NoError(t, err)
	assert.Equal(t, fsURL, cfg.Fs.URL)
	assert.Equal(t, 2, cfg.Fs.DefaultLayout)
	assert.Equal(t, true, cfg.Fs.CanQueryInfo)
	assert.Equal(t, FsVersioning{
		MaxNumberToKeep:            4,
		MinDelayBetweenTwoVersions: time.Minute,
	}, cfg.Fs.Versioning)

	// Jobs
	one := 1
	oneHour := time.Hour
	assert.Equal(t, "some-cmd", cfg.Jobs.ImageMagickConvertCmd)
	assert.Equal(t, "1H", cfg.Jobs.DefaultDurationToKeep)
	assert.Equal(t, true, cfg.Jobs.AllowList)
	assert.EqualValues(t, []Worker{
		{
			WorkerType:   "zip",
			Concurrency:  &one,
			MaxExecCount: &one,
			Timeout:      &oneHour,
		},
	}, cfg.Jobs.Workers)

	assert.Equal(t, "some-cmd", cfg.Konnectors.Cmd)
	assert.Equal(t, "http://some-url", cfg.Move.URL)

	// Notifications
	assert.EqualValues(t, Notifications{
		Development:            true,
		IOSCertificateKeyPath:  "cert-key-path",
		IOSCertificatePassword: "cert-password",
		IOSKeyID:               "key-id",
		IOSTeamID:              "team-id",
		HuaweiGetTokenURL:      "huawei-token",
		HuaweiSendMessagesURL:  "huawei-message",
		Contexts: map[string]SMS{
			"my-context": {
				Provider: "notif-provider",
				URL:      "https://some-notif-url",
				Token:    "some-token",
			},
		},
	}, cfg.Notifications)

	// Email
	assert.EqualValues(t, &gomail.DialerOptions{
		Host:                      "localhost",
		Port:                      25,
		Username:                  "some-username",
		Password:                  "some-password",
		NativeTLS:                 false,
		DisableTLS:                true,
		SkipCertificateValidation: true,
		LocalName:                 "some.host",
	}, cfg.Mail)
	assert.EqualValues(t, map[string]interface{}{
		"my-context": map[string]interface{}{"host": "-"},
	}, cfg.MailPerContext)

	assert.EqualValues(t, &gomail.DialerOptions{
		Host:                      "smtp.localhost",
		Port:                      57,
		Username:                  "campaign-username",
		Password:                  "campaign-password",
		NativeTLS:                 true,
		DisableTLS:                false,
		SkipCertificateValidation: false,
		LocalName:                 "smtp.localhost",
	}, cfg.CampaignMail)
	assert.EqualValues(t, map[string]interface{}{
		"my-context": map[string]interface{}{"host": "-"},
	}, cfg.CampaignMailPerContext)

	// CommonSettings
	assert.EqualValues(t, map[string]CommonSettings{
		"default": {
			URL:   "https://common-settings-default-url",
			Token: "default-token",
		},
		"my-context": {
			URL:   "https://common-settings-context-url",
			Token: "context-token",
		},
	}, cfg.CommonSettings)

	// Test GetSharingConfig for my-context
	myContextSharing := GetSharingConfig("my-context")
	assert.Equal(t, true, myContextSharing.AutoAcceptTrusted)
	assert.Equal(t, []string{"linagora.com"}, myContextSharing.TrustedDomains)
	assert.Equal(t, true, myContextSharing.AllowWritersToManageLinks)

	// Test GetAntivirusConfig for my-context (includes duration parsing)
	myContextAntivirus := GetAntivirusConfig("my-context")
	assert.Equal(t, true, myContextAntivirus.Enabled)
	assert.Equal(t, "localhost:3310", myContextAntivirus.Address)
	assert.Equal(t, 5*time.Minute, myContextAntivirus.Timeout)
	assert.Equal(t, int64(104857600), myContextAntivirus.MaxFileSize)
	assert.Equal(t, "warn", myContextAntivirus.OnInfected)
	assert.Equal(t, true, myContextAntivirus.Notifications.EmailOnInfected)
	assert.Equal(t, []string{"download", "preview"}, myContextAntivirus.Actions["pending"])
	assert.Equal(t, []string{"delete"}, myContextAntivirus.Actions["infected"])

	// Test GetAntivirusConfig for non-existent context returns disabled
	nonExistentAntivirus := GetAntivirusConfig("non-existent")
	assert.Equal(t, false, nonExistentAntivirus.Enabled)

	// Test GetSharingConfig for default context
	defaultSharing := GetSharingConfig("default")
	assert.Equal(t, true, defaultSharing.AutoAcceptTrusted)
	assert.Equal(t, false, defaultSharing.AllowWritersToManageLinks)

	// Test GetSharingConfig for non-existent context falls back to default
	fallbackSharing := GetSharingConfig("non-existent")
	assert.Equal(t, true, fallbackSharing.AutoAcceptTrusted)
	assert.Equal(t, false, fallbackSharing.AllowWritersToManageLinks)

	// Contexts
	assert.EqualValues(t, map[string]interface{}{
		"my-context": map[string]interface{}{
			"onboarded_redirection":    "home/intro",
			"default_redirection":      "home/",
			"help_link":                "https://cozy.io/fr/support",
			"enable_premium_links":     false,
			"claudy_actions":           []interface{}{"desktop", "support"},
			"additional_platform_apps": []interface{}{"some-app"},
			"features": []interface{}{
				map[string]interface{}{"hide_konnector_errors": true},
				map[string]interface{}{"home.konnectors.hide-errors": true},
				map[string]interface{}{"home_hidden_apps": []interface{}{"foobar"}},
			},
			"sharing": map[string]interface{}{
				"auto_accept_trusted":           true,
				"allow_writers_to_manage_links": true,
				"trusted_domains":               []interface{}{"linagora.com"},
			},
			"antivirus": map[string]interface{}{
				"enabled":       true,
				"address":       "localhost:3310",
				"timeout":       "5m",
				"max_file_size": 104857600,
				"on_infected":   "warn",
				"notifications": map[string]interface{}{
					"email_on_infected": true,
				},
				"actions": map[string]interface{}{
					"pending":  []interface{}{"download", "preview"},
					"clean":    []interface{}{"download", "share", "preview", "delete"},
					"infected": []interface{}{"delete"},
				},
			},
			"logos": map[string]interface{}{
				"coachco2": map[string]interface{}{
					"light": []interface{}{
						map[string]interface{}{
							"src": "/logos/main_cozy.png",
							"alt": "Cozy Cloud",
						},
					},
					"dark": []interface{}{
						map[string]interface{}{
							"src": "/logos/main_cozy.png",
							"alt": "Cozy Cloud",
						},
					},
				},
				"home": map[string]interface{}{
					"light": []interface{}{
						map[string]interface{}{
							"src":  "/logos/main_cozy.png",
							"alt":  "Cozy Cloud",
							"type": "main",
						},
						map[string]interface{}{
							"src":  "/logos/1_partner.png",
							"alt":  "Partner n°1",
							"type": "secondary",
						},
					},
					"dark": []interface{}{
						map[string]interface{}{
							"src":  "/logos/main_cozy.png",
							"alt":  "Cozy Cloud",
							"type": "main",
						},
						map[string]interface{}{
							"src":  "/logos/1_partner.png",
							"alt":  "Partner n°1",
							"type": "secondary",
						},
					},
				},
			},
		},
		"default": map[string]interface{}{
			"sharing": map[string]interface{}{
				"auto_accept_trusted": true,
			},
		},
	}, cfg.Contexts)

	// Authentication
	assert.EqualValues(t, map[string]interface{}{
		"example_oidc": map[string]interface{}{
			"disable_password_authentication": true,
			"oidc": map[string]interface{}{
				"client_id":                "some-id",
				"client_secret":            "some-secret",
				"scope":                    "openid",
				"redirect_uri":             "https://some-redirect-uri",
				"authorize_url":            "https://some-authorize-url",
				"token_url":                "https://some-token-url",
				"userinfo_url":             "https://some-user-info-url",
				"logout_url":               "https://some-logout-url",
				"userinfo_instance_field":  "instance-field",
				"allow_app_token_exchange": true,
				"app_token_exchange": map[string]interface{}{
					"twake-mail-web": map[string]interface{}{
						"software_id":    "registry://mail",
						"instance_claim": "workplaceFqdn",
					},
				},
			},
		},
	}, cfg.Authentication)

	// Office
	assert.EqualValues(t, map[string]Office{
		"foo": {
			OnlyOfficeURL: "https://onlyoffice-url",
			InboxSecret:   "inbox_secret",
			OutboxSecret:  "outbox_secret",
		},
	}, cfg.Office)

	// Registries
	u1, _ := url.Parse("https://registry-url-1")
	u2, _ := url.Parse("https://registry-url-2")
	assert.EqualValues(t, map[string][]*url.URL{
		"default": {},
		"example": {u1, u2},
	}, cfg.Registries)

	// Clouderies
	assert.EqualValues(t, map[string]ClouderyConfig{
		"default": {
			API: ClouderyAPI{
				URL:   "https://some-url",
				Token: "some-token",
			},
		},
		"my-context": {
			API: ClouderyAPI{
				URL:   "https://manager-url",
				Token: "manager-token",
			},
		},
	}, cfg.Clouderies)

	// CSPs
	assert.Equal(t, true, cfg.CSPDisabled)
	assert.EqualValues(t, map[string]string{
		"connect": "https://url-1 https://url-2",
		"font":    "https://fonts.gstatic.com/",
		"style":   "https://fonts.googleapis.com/",
	}, cfg.CSPAllowList)
	assert.EqualValues(t, map[string]map[string]string{
		"my-context": {
			"img":     "https://img-url",
			"script":  "https://script-url",
			"frame":   "https://frame-url",
			"connect": "https://connect-url",
		},
	}, cfg.CSPPerContext)

	// SafeHTTP
	assert.Equal(t, []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}, cfg.SafeHTTPTrustedNetworks)
}

func TestGetOIDCContextByDomain(t *testing.T) {
	oldConfig := config
	t.Cleanup(func() {
		config = oldConfig
	})

	config = &Config{
		Authentication: map[string]interface{}{
			"acme": map[string]interface{}{
				"oidc": map[string]interface{}{
					"domain": "acme.fr.",
				},
			},
			"beta": map[string]interface{}{
				"oidc": map[string]interface{}{
					"domain": "beta.example",
				},
			},
		},
	}

	contextName, err := GetOIDCContextByDomain(" ACME.FR ")
	require.NoError(t, err)
	assert.Equal(t, "acme", contextName)

	contextName, err = GetOIDCContextByDomain("beta.example")
	require.NoError(t, err)
	assert.Equal(t, "beta", contextName)

	contextName, err = GetOIDCContextByDomain("unknown.example")
	require.NoError(t, err)
	assert.Empty(t, contextName)
}

func TestGetOIDCContextByDomainConflict(t *testing.T) {
	oldConfig := config
	t.Cleanup(func() {
		config = oldConfig
	})

	config = &Config{
		Authentication: map[string]interface{}{
			"first": map[string]interface{}{
				"oidc": map[string]interface{}{
					"domain": "example.com",
				},
			},
			"second": map[string]interface{}{
				"oidc": map[string]interface{}{
					"domain": "example.com",
				},
			},
		},
	}

	contextName, err := GetOIDCContextByDomain("example.com")
	assert.ErrorIs(t, err, ErrOIDCDomainConflict)
	assert.Empty(t, contextName)
}

func TestUseViper(t *testing.T) {
	cfg := viper.New()
	cfg.Set("couchdb.url", "http://db:1234")
	assert.NoError(t, UseViper(cfg))
	assert.Equal(t, "http://db:1234/", CouchCluster(prefixer.GlobalCouchCluster).URL.String())
}

func TestGetOIDCAppTokenExchange(t *testing.T) {
	t.Run("loads configured linked apps", func(t *testing.T) {
		UseTestFile(t)
		GetConfig().Authentication = map[string]interface{}{
			"mail-context": map[string]interface{}{
				"oidc": map[string]interface{}{
					"allow_app_token_exchange": true,
					"app_token_exchange": map[string]interface{}{
						"twake-mail-web": map[string]interface{}{
							"software_id":    " registry://mail ",
							"instance_claim": " workplaceFqdn ",
						},
					},
				},
			},
		}

		cfg, err := GetOIDCAppTokenExchange("mail-context")
		require.NoError(t, err)
		require.True(t, cfg.Enabled)
		require.Contains(t, cfg.Apps, "twake-mail-web")
		assert.Equal(t, "twake-mail-web", cfg.Apps["twake-mail-web"].Audience)
		assert.Equal(t, "registry://mail", cfg.Apps["twake-mail-web"].SoftwareID)
		assert.Equal(t, "workplaceFqdn", cfg.Apps["twake-mail-web"].InstanceClaim)
	})

	t.Run("disabled when missing", func(t *testing.T) {
		UseTestFile(t)
		GetConfig().Authentication = map[string]interface{}{}

		cfg, err := GetOIDCAppTokenExchange("missing-context")
		require.NoError(t, err)
		assert.False(t, cfg.Enabled)
		assert.Nil(t, cfg.Apps)
	})

	t.Run("requires software id", func(t *testing.T) {
		UseTestFile(t)
		GetConfig().Authentication = map[string]interface{}{
			"mail-context": map[string]interface{}{
				"oidc": map[string]interface{}{
					"allow_app_token_exchange": true,
					"app_token_exchange": map[string]interface{}{
						"twake-mail-web": map[string]interface{}{},
					},
				},
			},
		}

		_, err := GetOIDCAppTokenExchange("mail-context")
		require.EqualError(t, err, `software_id is required for app token exchange audience "twake-mail-web"`)
	})

	t.Run("requires linked app software id", func(t *testing.T) {
		UseTestFile(t)
		GetConfig().Authentication = map[string]interface{}{
			"mail-context": map[string]interface{}{
				"oidc": map[string]interface{}{
					"allow_app_token_exchange": true,
					"app_token_exchange": map[string]interface{}{
						"twake-mail-web": map[string]interface{}{
							"software_id": "mail",
						},
					},
				},
			},
		}

		_, err := GetOIDCAppTokenExchange("mail-context")
		require.EqualError(t, err, `software_id for app token exchange audience "twake-mail-web" must be a linked app`)
	})
}

func TestSetup(t *testing.T) {
	tmpdir := t.TempDir()
	tmpfile, err := os.OpenFile(filepath.Join(tmpdir, "cozy.yaml"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	require.NoError(t, err)

	t.Setenv("OS_USERNAME", "os_username_val")
	t.Setenv("OS_PASSWORD", "os_password_val")
	t.Setenv("OS_PROJECT_NAME", "os_project_name_val")
	t.Setenv("OS_USER_DOMAIN_NAME", "os_user_domain_name_val")
	t.Setenv("MAIL_USERNAME", "mail_username_val")
	t.Setenv("MAIL_PASSWORD", "mail_password_val")

	_, err = tmpfile.Write([]byte(`
# cozy-stack configuration file

# server host - flags: --host
host: myhost
# server port - flags: --port -p
port: 1235


fs:
  # file system url - flags: --fs-url
  # default url is the directory relative to the binary: ./storage

  url: swift://openstack/?UserName={{ .Env.OS_USERNAME }}&Password={{ .Env.OS_PASSWORD }}&ProjectName={{ .Env.OS_PROJECT_NAME }}&UserDomainName={{ .Env.OS_USER_DOMAIN_NAME }}

mail:
  host: ssl0.ovh.net
  port: 465
  username: {{ .Env.MAIL_USERNAME }}
  password: {{ .Env.MAIL_PASSWORD }}


log:
    # logger level (debug, info, warning, panic, fatal) - flags: --log-level
    level: warning

registries:
  foo:
    - http://abc
    - http://def
  bar:
    - http://def
    - http://abc
  default:
    - https://default
`))
	require.NoError(t, err)

	err = Setup(tmpfile.Name())
	require.NoError(t, err)

	assert.Equal(t, "myhost", GetConfig().Host)
	assert.Equal(t, 1235, GetConfig().Port)
	assert.Equal(t, "swift://openstack/?UserName=os_username_val&Password=os_password_val&ProjectName=os_project_name_val&UserDomainName=os_user_domain_name_val", GetConfig().Fs.URL.String())
	assert.Equal(t, "ssl0.ovh.net", GetConfig().Mail.Host)
	assert.Equal(t, 465, GetConfig().Mail.Port)
	assert.Equal(t, "mail_username_val", GetConfig().Mail.Username)
	assert.Equal(t, "mail_password_val", GetConfig().Mail.Password)
	assert.Equal(t, logrus.GetLevel(), logrus.WarnLevel)

	assert.EqualValues(t, []string{"http://abc", "http://def", "https://default"}, regsToStrings(GetConfig().Registries["foo"]))
	assert.EqualValues(t, []string{"http://def", "http://abc", "https://default"}, regsToStrings(GetConfig().Registries["bar"]))
	assert.EqualValues(t, []string{"https://default"}, regsToStrings(GetConfig().Registries[DefaultInstanceContext]))
}

func regsToStrings(regs []*url.URL) []string {
	ss := make([]string, len(regs))
	for i, r := range regs {
		ss[i] = r.String()
	}
	return ss
}

func TestCommonSettingsHelpers(t *testing.T) {
	require.NoError(t, Setup("./testdata/full_config.yaml"))
	commonSettings := GetCommonSettings()
	assert.Len(t, commonSettings, 2)
}

func TestGetPublicDomainSuffix(t *testing.T) {
	oldConfig := config
	t.Cleanup(func() { config = oldConfig })

	config = &Config{
		Contexts: map[string]interface{}{
			DefaultInstanceContext: map[string]interface{}{
				"public_domain_suffix": " STG.LIN-SAAS.COM. ",
			},
			"custom": map[string]interface{}{
				"public_domain_suffix": "Prod.Lin-Saas.COM.",
			},
			"blank": map[string]interface{}{
				"public_domain_suffix": "   ",
			},
		},
	}

	assert.Equal(t, "prod.lin-saas.com", GetPublicDomainSuffix("custom"))
	assert.Equal(t, "stg.lin-saas.com", GetPublicDomainSuffix("blank"))
	assert.Equal(t, "stg.lin-saas.com", GetPublicDomainSuffix("missing"))
	assert.Equal(t, "stg.lin-saas.com", GetPublicDomainSuffix(""))

	config = &Config{}
	assert.Empty(t, GetPublicDomainSuffix("custom"))

	config = nil
	assert.Empty(t, GetPublicDomainSuffix("custom"))
}

func TestRabbitMQConfig(t *testing.T) {
	// Create a temporary YAML file with RabbitMQ configuration
	yamlContent := `
rabbitmq:
  enabled: true
  nodes:
    default:
      enabled: true
      url: "amqp://test:test@localhost:5672/"
  exchanges:
    - name: "user-password-updates"
      kind: "topic"
      durable: true
      dlx_name: "cozy.admin.dlx"
      dlq_name: "cozy.admin.dlq"
      queues:
        - name: "stack.user.password.updated"
          bindings: ["user.password.change"]
          prefetch: 32
          delivery_limit: 5
    
    - name: "user-settings-updates"
      kind: "topic"
      durable: true
      dlx_name: "cozy.admin.dlx_settings"
      dlq_name: "cozy.admin.dlq_settings"
      queues:
        - name: "user-settings-updates"
          bindings: ["settings.user.update"]
          prefetch: 16
          delivery_limit: 3
        - name: "settings-admin-events"
          bindings: ["settings.admin.*"]
          prefetch: 8
          delivery_limit: 2
`

	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "test-rabbitmq-config.yaml")
	err := os.WriteFile(tmpFile, []byte(yamlContent), 0644)
	require.NoError(t, err)

	require.NoError(t, Setup(tmpFile))

	cfg := GetConfig()
	rabbitmq := cfg.RabbitMQ

	assert.True(t, rabbitmq.Enabled)
	assert.Len(t, rabbitmq.Nodes, 1, "Should have exactly 1 node")
	node := rabbitmq.Nodes["default"]
	assert.NotNil(t, node, "Should have default node config")
	assert.True(t, node.Enabled)
	assert.Equal(t, "amqp://test:test@localhost:5672/", node.URL)

	require.Len(t, rabbitmq.Exchanges, 2, "Should have exactly 2 exchanges")

	exchange1 := rabbitmq.Exchanges[0]
	assert.Equal(t, "user-password-updates", exchange1.Name)
	assert.Equal(t, "topic", exchange1.Kind)
	assert.True(t, exchange1.Durable)
	assert.Equal(t, "cozy.admin.dlx", exchange1.DLXName)
	assert.Equal(t, "cozy.admin.dlq", exchange1.DLQName)

	require.Len(t, exchange1.Queues, 1, "First exchange should have 1 queue")
	queue1 := exchange1.Queues[0]
	assert.Equal(t, "stack.user.password.updated", queue1.Name)
	assert.Equal(t, []string{"user.password.change"}, queue1.Bindings)
	assert.Equal(t, 32, queue1.Prefetch)
	assert.Equal(t, 5, queue1.DeliveryLimit)

	exchange2 := rabbitmq.Exchanges[1]
	assert.Equal(t, "user-settings-updates", exchange2.Name)
	assert.Equal(t, "topic", exchange2.Kind)
	assert.True(t, exchange2.Durable)
	assert.Equal(t, "cozy.admin.dlx_settings", exchange2.DLXName)
	assert.Equal(t, "cozy.admin.dlq_settings", exchange2.DLQName)

	require.Len(t, exchange2.Queues, 2, "Second exchange should have 2 queues")

	queue2a := exchange2.Queues[0]
	assert.Equal(t, "user-settings-updates", queue2a.Name)
	assert.Equal(t, []string{"settings.user.update"}, queue2a.Bindings)
	assert.Equal(t, 16, queue2a.Prefetch)
	assert.Equal(t, 3, queue2a.DeliveryLimit)

	queue2b := exchange2.Queues[1]
	assert.Equal(t, "settings-admin-events", queue2b.Name)
	assert.Equal(t, []string{"settings.admin.*"}, queue2b.Bindings)
	assert.Equal(t, 8, queue2b.Prefetch)
	assert.Equal(t, 2, queue2b.DeliveryLimit)
}

func TestSafeHTTPConfig(t *testing.T) {
	t.Cleanup(func() { _ = safehttp.SetTrustedPrivateNetworks(nil) })

	t.Run("valid CIDRs map through UseViper", func(t *testing.T) {
		cfg := viper.New()
		cfg.Set("safe_http.trusted_private_networks", []string{"10.0.0.0/8", "172.16.0.0/12"})
		require.NoError(t, UseViper(cfg))
		assert.Equal(t, []string{"10.0.0.0/8", "172.16.0.0/12"}, GetConfig().SafeHTTPTrustedNetworks)
	})

	t.Run("invalid CIDR makes UseViper fail", func(t *testing.T) {
		cfg := viper.New()
		cfg.Set("safe_http.trusted_private_networks", []string{"not-a-cidr"})
		err := UseViper(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid safe_http.trusted_private_networks config")
	})

	t.Run("empty config keeps strict defaults", func(t *testing.T) {
		cfg := viper.New()
		require.NoError(t, UseViper(cfg))
		assert.Empty(t, GetConfig().SafeHTTPTrustedNetworks)
	})

	t.Run("missing key keeps strict defaults", func(t *testing.T) {
		cfg := viper.New()
		cfg.Set("host", "localhost")
		require.NoError(t, UseViper(cfg))
		assert.Empty(t, GetConfig().SafeHTTPTrustedNetworks)
	})
}
