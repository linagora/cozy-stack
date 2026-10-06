package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/settings/common"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/utils"
	"golang.org/x/sync/errgroup"
)

// Options holds the parameters to create a new instance.
type Options struct {
	Domain             string
	DomainAliases      []string
	OrgDomain          string
	OrgID              string
	OldDomain          string
	Locale             string
	UUID               string
	OIDCID             string
	FranceConnectID    string
	TOSSigned          string
	TOSLatest          string
	Timezone           string
	ContextName        string
	Sponsorships       []string
	FeatureSets        []string
	Email              string
	PublicName         string
	Phone              string
	Settings           string
	SettingsObj        *couchdb.JSONDoc
	AuthMode           string
	Passphrase         string
	Key                string
	KdfIterations      int
	SwiftLayout        int
	CouchCluster       int
	DiskQuota          int64
	Apps               []string
	AutoUpdate         *bool
	MagicLink          *bool
	Debug              *bool
	Traced             *bool
	OnboardingFinished *bool
	Blocked            *bool
	BlockingReason     string
	FromCloudery       bool // Do not call the cloudery when the changes come from it
}

func (opts *Options) trace(name string, do func()) {
	if opts.Traced != nil && *opts.Traced {
		t := time.Now()
		defer func() {
			elapsed := time.Since(t)
			logger.
				WithDomain("admin").
				WithNamespace("trace").
				Infof("%s: %v", name, elapsed)
		}()
	}
	do()
}

// Create builds an instance and initializes it
func Create(opts *Options) (*instance.Instance, error) {
	domain := opts.Domain
	var err error
	opts.trace("validate domain", func() {
		domain, err = validateDomain(domain)
	})
	if err != nil {
		return nil, err
	}
	opts.trace("check if instance already exist", func() {
		_, err = instance.Get(domain)
	})
	if !errors.Is(err, instance.ErrNotFound) {
		if err == nil {
			err = instance.ErrExists
		}
		return nil, err
	}

	locale := opts.Locale
	if locale == "" {
		locale = consts.DefaultLocale
	}

	if opts.SettingsObj == nil {
		opts.SettingsObj = &couchdb.JSONDoc{M: make(map[string]interface{})}
	}

	settings, err := buildSettings(nil, opts)
	if err != nil {
		return nil, err
	}
	prefix := sha256.Sum256([]byte(domain))
	i := &instance.Instance{}
	i.Domain = domain
	i.DomainAliases, err = checkAliases(i, opts.DomainAliases)
	if err != nil {
		return nil, err
	}
	i.OrgDomain = opts.OrgDomain
	i.OrgID = opts.OrgID
	i.OldDomain = opts.OldDomain
	i.Prefix = "cozy" + hex.EncodeToString(prefix[:16])
	i.Locale = locale
	i.UUID = opts.UUID
	i.OIDCID = opts.OIDCID
	i.FranceConnectID = opts.FranceConnectID
	i.TOSSigned = opts.TOSSigned
	i.TOSLatest = opts.TOSLatest
	i.ContextName = opts.ContextName
	i.Sponsorships = opts.Sponsorships
	i.FeatureSets = opts.FeatureSets
	i.BytesDiskQuota = opts.DiskQuota
	i.IndexViewsVersion = couchdb.IndexViewsVersion
	opts.trace("generate secrets", func() {
		i.RegisterToken = crypto.GenerateRandomBytes(instance.RegisterTokenLen)
		i.SessSecret = crypto.GenerateRandomBytes(instance.SessionSecretLen)
		i.OAuthSecret = crypto.GenerateRandomBytes(instance.OauthSecretLen)
		i.CLISecret = crypto.GenerateRandomBytes(instance.OauthSecretLen)
	})

	switch config.FsURL().Scheme {
	case config.SchemeSwift, config.SchemeSwiftSecure:
		switch opts.SwiftLayout {
		case 0:
			return nil, errors.New("Swift layout v1 disabled for instance creation")
		case 1, 2:
			i.SwiftLayout = opts.SwiftLayout
		default:
			i.SwiftLayout = config.GetConfig().Fs.DefaultLayout
		}
	}

	if opts.CouchCluster >= 0 {
		i.CouchCluster = opts.CouchCluster
	} else {
		clusters := config.GetConfig().CouchDB.Clusters
		i.CouchCluster, err = ChooseCouchCluster(clusters)
		if err != nil {
			return nil, err
		}
	}

	if opts.AuthMode != "" {
		var authMode instance.AuthMode
		if authMode, err = instance.StringToAuthMode(opts.AuthMode); err == nil {
			i.AuthMode = authMode
		}
	}

	opts.trace("init couchdb", func() {
		g, _ := errgroup.WithContext(context.Background())
		g.Go(func() error { return couchdb.CreateDB(i, consts.Files) })
		g.Go(func() error { return couchdb.CreateDB(i, consts.Apps) })
		g.Go(func() error { return couchdb.CreateDB(i, consts.Konnectors) })
		g.Go(func() error { return couchdb.CreateDB(i, consts.OAuthClients) })
		g.Go(func() error { return couchdb.CreateDB(i, consts.Jobs) })
		g.Go(func() error { return couchdb.CreateDB(i, consts.Triggers) })
		g.Go(func() error { return couchdb.CreateDB(i, consts.Permissions) })
		g.Go(func() error { return couchdb.CreateDB(i, consts.Sharings) })
		g.Go(func() error { return couchdb.CreateDB(i, consts.BitwardenCiphers) })
		g.Go(func() error { return couchdb.CreateDB(i, consts.SessionsLogins) })
		g.Go(func() error { return couchdb.CreateDB(i, consts.Notifications) })
		g.Go(func() error {
			var errg error
			if errg = couchdb.CreateNamedDocWithDB(i, settings); errg != nil {
				return errg
			}
			_, errg = contact.CreateMyself(i, settings)
			return errg
		})
		err = g.Wait()
	})
	if err != nil {
		return nil, err
	}

	opts.trace("define views and indexes", func() {
		err = DefineViewsAndIndex(i)
	})
	if err != nil {
		return nil, err
	}

	if magicLink := opts.MagicLink; magicLink != nil {
		i.MagicLink = *magicLink
	}

	passwordDefined := opts.Passphrase != ""

	// If the password authentication is disabled, we force a random password.
	// It won't be known by the user and cannot be used to authenticate. It
	// will only be used if the configuration is changed later: the user will
	// be able to reset the passphrase. Same when the user has used
	// FranceConnect to create their instance.
	if i.HasForcedOIDC() || i.FranceConnectID != "" || i.MagicLink {
		opts.Passphrase = utils.RandomString(instance.RegisterTokenLen)
		opts.KdfIterations = crypto.DefaultPBKDF2Iterations
	}

	if opts.Passphrase != "" {
		opts.trace("register passphrase", func() {
			err = registerPassphrase(i, i.RegisterToken, PassParameters{
				Pass:       []byte(opts.Passphrase),
				Iterations: opts.KdfIterations,
				Key:        opts.Key,
			})
		})
		if err != nil {
			return nil, err
		}
		// set the onboarding finished when specifying a passphrase. we totally
		// skip the onboarding in that case.
		i.OnboardingFinished = true
	}

	i.SetPasswordDefined(passwordDefined)
	if onboardingFinished := opts.OnboardingFinished; onboardingFinished != nil {
		i.OnboardingFinished = *onboardingFinished
	}

	if autoUpdate := opts.AutoUpdate; autoUpdate != nil {
		i.NoAutoUpdate = !(*opts.AutoUpdate)
	}

	if err = couchdb.CreateDoc(prefixer.GlobalPrefixer, i); err != nil {
		return nil, err
	}

	opts.trace("init VFS", func() {
		if err = i.MakeVFS(); err != nil {
			return
		}
		if err = i.VFS().InitFs(); err != nil {
			return
		}
		err = createDefaultFilesTree(i)
	})
	if err != nil {
		return nil, err
	}

	// The instance is usable without its email, so a failure here does not
	// fail the creation.
	email, _ := settings.M["email"].(string)
	if err := instance.SetEmail(i, email); err != nil {
		i.Logger().Warnf("Email not set: %s", err)
	}

	opts.trace("install apps", func() {
		done := make(chan struct{})
		for _, app := range opts.Apps {
			go func(app string) {
				if err := installApp(i, app); err != nil {
					i.Logger().Errorf("Failed to install %s: %s", app, err)
				}
				done <- struct{}{}
			}(app)
		}
		for range opts.Apps {
			<-done
		}
	})

	opts.trace("create common settings", func() {
		if err = common.CreateCommonSettings(i, settings); err != nil {
			i.Logger().Errorf("Failed to create common settings: %s", err)
		}
		// We need to persist the common settings version
		if err = instance.Update(i); err != nil {
			return
		}
	})

	return i, nil
}

func ChooseCouchCluster(clusters []config.CouchDBCluster) (int, error) {
	index := -1
	var count uint32 = 0
	for i, cluster := range clusters {
		if !cluster.Creation {
			continue
		}
		count++
		if rand.Uint32() <= math.MaxUint32/count {
			index = i
		}
	}
	if index < 0 {
		return index, errors.New("no CouchDB cluster available for creation")
	}
	return index, nil
}

func buildSettings(inst *instance.Instance, opts *Options) (*couchdb.JSONDoc, error) {
	var settings *couchdb.JSONDoc
	if opts.SettingsObj != nil {
		settings = opts.SettingsObj
	} else {
		var err error
		settings, err = inst.SettingsDocument()
		if err != nil {
			return nil, err
		}
	}

	settings.Type = consts.Settings
	settings.SetID(consts.InstanceSettingsID)

	for _, s := range strings.Split(opts.Settings, ",") {
		if parts := strings.SplitN(s, ":", 2); len(parts) == 2 {
			settings.M[parts[0]] = parts[1]
		}
	}

	// Handling global/instance settings
	if contextName, ok := settings.M["context"].(string); ok {
		opts.ContextName = contextName
		delete(settings.M, "context")
	}
	if sponsorships, ok := settings.M["sponsorships"].([]string); ok {
		opts.Sponsorships = sponsorships
		delete(settings.M, "sponsorships")
	}
	if featureSets, ok := settings.M["feature_sets"].([]string); ok {
		opts.FeatureSets = featureSets
		delete(settings.M, "feature_sets")
	}
	if locale, ok := settings.M["locale"].(string); ok {
		opts.Locale = locale
		delete(settings.M, "locale")
	}
	if onboardingFinished, ok := settings.M["onboarding_finished"].(bool); ok {
		opts.OnboardingFinished = &onboardingFinished
		delete(settings.M, "onboarding_finished")
	}
	if uuid, ok := settings.M["uuid"].(string); ok {
		opts.UUID = uuid
		delete(settings.M, "uuid")
	}
	if oidcID, ok := settings.M["oidc_id"].(string); ok {
		opts.OIDCID = oidcID
		delete(settings.M, "oidc_id")
	}
	if tos, ok := settings.M["tos"].(string); ok {
		opts.TOSSigned = tos
		delete(settings.M, "tos")
	}
	if tos, ok := settings.M["tos_latest"].(string); ok {
		opts.TOSLatest = tos
		delete(settings.M, "tos_latest")
	}
	if autoUpdate, ok := settings.M["auto_update"].(string); ok {
		if b, err := strconv.ParseBool(autoUpdate); err == nil {
			opts.AutoUpdate = &b
		}
		delete(settings.M, "auto_update")
	}
	if authMode, ok := settings.M["auth_mode"].(string); ok {
		opts.AuthMode = authMode
		delete(settings.M, "auth_mode")
	}

	// Handling instance settings document
	if tz := opts.Timezone; tz != "" {
		settings.M["tz"] = tz
	}
	if email := opts.Email; email != "" {
		settings.M["email"] = email
	}
	if name := opts.PublicName; name != "" {
		settings.M["public_name"] = name
	}
	if phone := opts.Phone; phone != "" {
		settings.M["phone"] = phone
	}

	if len(opts.TOSSigned) == 8 {
		opts.TOSSigned = "1.0.0-" + opts.TOSSigned
	}

	return settings, nil
}
