package lifecycle

import (
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/cloudery"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/settings/common"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/labstack/echo/v4"
)

var managerHTTPClient = &http.Client{Timeout: 30 * time.Second}

// AskReupload is the function that will be called when the disk quota is
// increased to ask reuploading files from the sharings. A package variable is
// used to avoid a dependency on the model/sharing package (which would lead to
// circular import issue).
var AskReupload func(*instance.Instance) error

// RefreshBanners is the function called when a change on the instance can make
// its platform banners stale. A package variable is used to avoid a dependency
// on the model/banner package (which would lead to circular import issue).
var RefreshBanners func(domain string)

// Patch updates the given instance with the specified options if necessary. It
// can also update the settings document if provided in the options.
func Patch(i *instance.Instance, opts *Options) error {
	opts.Domain = i.Domain
	settings, err := buildSettings(i, opts)
	if err != nil {
		return err
	}

	needBannersRefresh := false

	for {
		var err error
		if i == nil {
			i, err = GetInstance(opts.Domain)
			if err != nil {
				return err
			}
		}

		needUpdate := false
		needSharingReupload := false

		if opts.Locale != "" && opts.Locale != i.Locale {
			i.Locale = opts.Locale
			needBannersRefresh = true
			needUpdate = true
		}

		if opts.Blocked != nil && *opts.Blocked != i.Blocked {
			i.Blocked = *opts.Blocked
			needUpdate = true
		}

		if opts.BlockingReason != "" && opts.BlockingReason != i.BlockingReason {
			i.BlockingReason = opts.BlockingReason
			needUpdate = true
		}

		if aliases := opts.DomainAliases; aliases != nil {
			i.DomainAliases, err = checkAliases(i, aliases)
			if err != nil {
				return err
			}
			needUpdate = true
		}

		if opts.OldDomain != "" && opts.OldDomain != i.OldDomain {
			i.OldDomain = opts.OldDomain
			needUpdate = true
		}

		if opts.UUID != "" && opts.UUID != i.UUID {
			i.UUID = opts.UUID
			needUpdate = true
		}

		if opts.OIDCID != "" && opts.OIDCID != i.OIDCID {
			i.OIDCID = opts.OIDCID
			needUpdate = true
		}

		if opts.FranceConnectID != "" && opts.FranceConnectID != i.FranceConnectID {
			i.FranceConnectID = opts.FranceConnectID
			needUpdate = true
		}

		if opts.MagicLink != nil && *opts.MagicLink != i.MagicLink {
			i.MagicLink = *opts.MagicLink
			needUpdate = true
		}

		if opts.ContextName != "" && opts.ContextName != i.ContextName {
			i.ContextName = opts.ContextName
			needUpdate = true
		}

		if len(opts.Sponsorships) != 0 {
			i.Sponsorships = opts.Sponsorships
			needUpdate = true
		}

		if opts.AuthMode != "" {
			var authMode instance.AuthMode
			authMode, err = instance.StringToAuthMode(opts.AuthMode)
			if err != nil {
				return err
			}
			if i.AuthMode != authMode {
				i.AuthMode = authMode
				needUpdate = true
			}
		}

		if opts.FeatureSets != nil {
			i.FeatureSets = opts.FeatureSets
			needUpdate = true
		}

		if opts.DiskQuota != 0 {
			needBannersRefresh = true
		}

		if opts.DiskQuota > 0 && opts.DiskQuota != i.BytesDiskQuota {
			needUpdate = true
			needSharingReupload = opts.DiskQuota > i.BytesDiskQuota
			i.BytesDiskQuota = opts.DiskQuota
		} else if opts.DiskQuota == -1 {
			i.BytesDiskQuota = 0
			needUpdate = true
		}

		if opts.AutoUpdate != nil && !(*opts.AutoUpdate) != i.NoAutoUpdate {
			i.NoAutoUpdate = !(*opts.AutoUpdate)
			needUpdate = true
		}

		if opts.OnboardingFinished != nil && *opts.OnboardingFinished != i.OnboardingFinished {
			i.OnboardingFinished = *opts.OnboardingFinished
			needUpdate = true
		}

		if opts.TOSLatest != "" {
			if _, date, ok := instance.ParseTOSVersion(opts.TOSLatest); !ok || date.IsZero() {
				return instance.ErrBadTOSVersion
			}
			if i.TOSLatest != opts.TOSLatest {
				if i.CheckTOSNotSigned(opts.TOSLatest) {
					i.TOSLatest = opts.TOSLatest
					needUpdate = true
				}
			}
		}

		if opts.TOSSigned != "" {
			if _, _, ok := instance.ParseTOSVersion(opts.TOSSigned); !ok {
				return instance.ErrBadTOSVersion
			}
			if i.TOSSigned != opts.TOSSigned {
				i.TOSSigned = opts.TOSSigned
				if !i.CheckTOSNotSigned() {
					i.TOSLatest = ""
				}
				needUpdate = true
			}
		}

		if !needUpdate {
			break
		}

		err = update(i)
		if couchdb.IsConflictError(err) {
			i = nil
			continue
		}
		if err != nil {
			return err
		}

		if needSharingReupload && AskReupload != nil {
			go func() {
				inst := i.Clone().(*instance.Instance)
				if err := AskReupload(inst); err != nil {
					inst.Logger().WithNamespace("lifecycle").
						Warnf("sharing.AskReupload failed with %s", err)
				}
			}()
		}
		break
	}

	// A downgrade changes the share of the quota the usage represents, and a
	// language change leaves the banners in the previous one. Outside the loop
	// because re-pushing an unchanged quota is how the Cloudery converges it,
	// and that updates no document.
	if needBannersRefresh && RefreshBanners != nil {
		RefreshBanners(i.Domain)
	}

	// Update the settings doc
	if ok := needsSettingsUpdate(i, settings.M); ok {
		if err := couchdb.UpdateDoc(i, settings); err != nil {
			return err
		}

		if !opts.FromCloudery {
			email, _ := settings.M["email"].(string)
			publicName, _ := settings.M["public_name"].(string)

			err = cloudery.SaveInstance(i, &cloudery.SaveCmd{
				Locale:     i.Locale,
				Email:      email,
				PublicName: publicName,
			})
			if err != nil {
				i.Logger().Errorf("Error during cloudery settings update %s", err)
			}
		}
	}

	var updated bool
	updated, err = common.UpdateCommonSettings(i, settings)

	if err != nil {
		i.Logger().Errorf("Failed to update common settings: %s", err)
	}
	if updated {
		i.Logger().Info("Common settings updated")
		// We need to persist the common settings version
		if err = update(i); err != nil {
			return err
		}
	}

	if debug := opts.Debug; debug != nil {
		var err error
		if *debug {
			err = logger.AddDebugDomain(i.Domain, 24*time.Hour)
		} else {
			err = logger.RemoveDebugDomain(i.Domain)
		}
		if err != nil {
			return err
		}
	}

	return nil
}

// needsSettingsUpdate compares the old instance io.cozy.settings with the new
// bunch of settings and tells if it needs an update
func needsSettingsUpdate(inst *instance.Instance, newSettings map[string]interface{}) bool {
	oldSettings, err := inst.SettingsDocument()
	if err != nil {
		return false
	}

	if oldSettings.M == nil {
		return true
	}

	for k, newValue := range newSettings {
		if k == "_id" || k == "_rev" {
			continue
		}
		// Check if we have the key in old settings and the value is different,
		// or if we don't have the key at all
		if oldValue, ok := oldSettings.M[k]; !ok || !reflect.DeepEqual(oldValue, newValue) {
			return true
		}
	}

	// Handles if a key was removed in the new settings but exists in the old
	// settings, and therefore needs an update
	for oldKey := range oldSettings.M {
		if _, ok := newSettings[oldKey]; !ok {
			return true
		}
	}

	return false
}

// Block function blocks an instance with an optional reason parameter
func Block(inst *instance.Instance, reason ...string) error {
	var r string
	if len(reason) == 1 {
		r = reason[0]
	} else {
		r = instance.BlockedUnknown.Code
	}
	inst.Blocked = true
	inst.BlockingReason = r
	return update(inst)
}

// Unblock reverts the blocking of an instance
func Unblock(inst *instance.Instance) error {
	inst.Blocked = false
	inst.BlockingReason = ""
	return update(inst)
}

// ManagerSignTOS make a request to the manager in order to finalize the TOS
// signing flow.
func ManagerSignTOS(inst *instance.Instance, originalReq *http.Request) error {
	if inst.TOSLatest == "" {
		return nil
	}
	split := strings.SplitN(inst.TOSLatest, "-", 2)
	if len(split) != 2 {
		return nil
	}
	u, err := inst.ManagerURL(instance.ManagerTOSURL)
	if err != nil {
		return Patch(inst, &Options{TOSSigned: inst.TOSLatest})
	}
	form := url.Values{"version": {split[0]}}
	res, err := doManagerRequest(http.MethodPut, u, form, originalReq)
	if err != nil {
		return err
	}
	return res.Body.Close()
}

func doManagerRequest(method string, url string, form url.Values, originalReq *http.Request) (*http.Response, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	if form != nil {
		req.Header.Add(echo.HeaderContentType, echo.MIMEApplicationForm)
	}
	if originalReq != nil {
		var ip string
		if forwardedFor := req.Header.Get(echo.HeaderXForwardedFor); forwardedFor != "" {
			ip = strings.TrimSpace(strings.SplitN(forwardedFor, ",", 2)[0])
		}
		if ip == "" {
			ip = req.RemoteAddr
		}
		req.Header.Set(echo.HeaderXForwardedFor, ip)
	}
	return managerHTTPClient.Do(req)
}
