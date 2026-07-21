package client

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/client/request"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/move"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/labstack/echo/v4"
)

// Instance is a struct holding the representation of an instance on the API.
type Instance struct {
	ID   string `json:"id"`
	Meta struct {
		Rev string `json:"rev"`
	} `json:"meta"`
	Attrs struct {
		Domain               string    `json:"domain"`
		DomainAliases        []string  `json:"domain_aliases,omitempty"`
		OrgDomain            string    `json:"org_domain,omitempty"`
		OrgID                string    `json:"org_id,omitempty"`
		OldDomain            string    `json:"old_domain,omitempty"`
		Prefix               string    `json:"prefix,omitempty"`
		Locale               string    `json:"locale"`
		UUID                 string    `json:"uuid,omitempty"`
		OIDCID               string    `json:"oidc_id,omitempty"`
		ContextName          string    `json:"context,omitempty"`
		Sponsorships         []string  `json:"sponsorships,omitempty"`
		FeatureSets          []string  `json:"feature_sets,omitempty"`
		TOSSigned            string    `json:"tos,omitempty"`
		TOSLatest            string    `json:"tos_latest,omitempty"`
		AuthMode             int       `json:"auth_mode,omitempty"`
		NoAutoUpdate         bool      `json:"no_auto_update,omitempty"`
		Blocked              bool      `json:"blocked,omitempty"`
		OnboardingFinished   bool      `json:"onboarding_finished"`
		PasswordDefined      *bool     `json:"password_defined"`
		MagicLink            bool      `json:"magic_link,omitempty"`
		BytesDiskQuota       int64     `json:"disk_quota,string,omitempty"`
		IndexViewsVersion    int       `json:"indexes_version"`
		CouchCluster         int       `json:"couch_cluster,omitempty"`
		SwiftLayout          int       `json:"swift_cluster,omitempty"`
		PassphraseResetToken []byte    `json:"passphrase_reset_token"`
		PassphraseResetTime  time.Time `json:"passphrase_reset_time"`
		RegisterToken        []byte    `json:"register_token,omitempty"`
	} `json:"attributes"`
}

// InstanceOptions contains the options passed on instance creation.
type InstanceOptions struct {
	Domain             string
	DomainAliases      []string
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
	Email              string
	PublicName         string
	Phone              string
	Settings           string
	BlockingReason     string
	SwiftLayout        int
	CouchCluster       int
	DiskQuota          int64
	Apps               []string
	Passphrase         string
	KdfIterations      int
	MagicLink          *bool
	Debug              *bool
	Blocked            *bool
	Deleting           *bool
	OnboardingFinished *bool
	Trace              *bool
}

// TokenOptions is a struct holding all the options to generate a token.
type TokenOptions struct {
	Domain   string
	Subject  string
	Audience string
	Scope    []string
	Expire   *time.Duration
}

// OAuthClientOptions is a struct holding all the options to generate an OAuth
// client associated to an instance.
type OAuthClientOptions struct {
	Domain                string
	RedirectURI           string
	ClientName            string
	SoftwareID            string
	AllowLoginScope       bool
	OnboardingSecret      string
	OnboardingApp         string
	OnboardingPermissions string
	OnboardingState       string
}

type ExportOptions struct {
	Domain    string
	LocalPath string
}

// ImportOptions is a struct with the options for importing a tarball.
type ImportOptions struct {
	ManifestURL string
}

// DBPrefix returns the database prefix for the instance
func (i *Instance) DBPrefix() string {
	if i.Attrs.Prefix != "" {
		return i.Attrs.Prefix
	}
	return i.Attrs.Domain
}

// GetInstance returns the instance associated with the specified domain.
func (ac *AdminClient) GetInstance(domain string) (*Instance, error) {
	res, err := ac.Req(&request.Options{
		Method: "GET",
		Path:   "/instances/" + domain,
	})
	if err != nil {
		return nil, err
	}
	return readInstance(res)
}

// CreateInstance is used to create a new cozy instance of the specified domain
// and locale.
func (ac *AdminClient) CreateInstance(opts *InstanceOptions) (*Instance, error) {
	if !validDomain(opts.Domain) {
		return nil, fmt.Errorf("Invalid domain: %s", opts.Domain)
	}
	q := url.Values{
		"Domain":          {opts.Domain},
		"OldDomain":       {opts.OldDomain},
		"Locale":          {opts.Locale},
		"UUID":            {opts.UUID},
		"OIDCID":          {opts.OIDCID},
		"FranceConnectID": {opts.FranceConnectID},
		"TOSSigned":       {opts.TOSSigned},
		"Timezone":        {opts.Timezone},
		"ContextName":     {opts.ContextName},
		"Email":           {opts.Email},
		"PublicName":      {opts.PublicName},
		"Phone":           {opts.Phone},
		"Settings":        {opts.Settings},
		"SwiftLayout":     {strconv.Itoa(opts.SwiftLayout)},
		"CouchCluster":    {strconv.Itoa(opts.CouchCluster)},
		"DiskQuota":       {strconv.FormatInt(opts.DiskQuota, 10)},
		"Apps":            {strings.Join(opts.Apps, ",")},
		"Passphrase":      {opts.Passphrase},
		"KdfIterations":   {strconv.Itoa(opts.KdfIterations)},
	}
	if opts.DomainAliases != nil {
		q.Add("DomainAliases", strings.Join(opts.DomainAliases, ","))
	}
	if opts.Sponsorships != nil {
		q.Add("Sponsorships", strings.Join(opts.Sponsorships, ","))
	}
	if opts.MagicLink != nil && *opts.MagicLink {
		q.Add("MagicLink", "true")
	}
	if opts.Trace != nil && *opts.Trace {
		q.Add("Trace", "true")
	}
	res, err := ac.Req(&request.Options{
		Method:  "POST",
		Path:    "/instances",
		Queries: q,
	})
	if err != nil {
		return nil, err
	}
	return readInstance(res)
}

// CountInstances returns the number of instances.
func (ac *AdminClient) CountInstances() (int, error) {
	res, err := ac.Req(&request.Options{
		Method: "GET",
		Path:   "/instances/count",
	})
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	var data map[string]int
	if err = json.NewDecoder(res.Body).Decode(&data); err != nil {
		return 0, err
	}
	return data["count"], nil
}

// ListInstances returns the list of instances recorded on the stack.
func (ac *AdminClient) ListInstances() ([]*Instance, error) {
	res, err := ac.Req(&request.Options{
		Method: "GET",
		Path:   "/instances",
	})
	if err != nil {
		return nil, err
	}
	var list []*Instance
	if err = readJSONAPI(res.Body, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// ModifyInstance is used to update an instance.
func (ac *AdminClient) ModifyInstance(opts *InstanceOptions) (*Instance, error) {
	domain := opts.Domain
	if !validDomain(domain) {
		return nil, fmt.Errorf("Invalid domain: %s", domain)
	}
	q := url.Values{
		"OldDomain":       {opts.OldDomain},
		"Locale":          {opts.Locale},
		"UUID":            {opts.UUID},
		"OIDCID":          {opts.OIDCID},
		"FranceConnectID": {opts.FranceConnectID},
		"TOSSigned":       {opts.TOSSigned},
		"TOSLatest":       {opts.TOSLatest},
		"Timezone":        {opts.Timezone},
		"ContextName":     {opts.ContextName},
		"Email":           {opts.Email},
		"PublicName":      {opts.PublicName},
		"Phone":           {opts.Phone},
		"Settings":        {opts.Settings},
		"DiskQuota":       {strconv.FormatInt(opts.DiskQuota, 10)},
	}
	if opts.DomainAliases != nil {
		q.Add("DomainAliases", strings.Join(opts.DomainAliases, ","))
	}
	if opts.Sponsorships != nil {
		q.Add("Sponsorships", strings.Join(opts.Sponsorships, ","))
	}
	if opts.MagicLink != nil {
		q.Add("MagicLink", strconv.FormatBool(*opts.MagicLink))
	}
	if opts.Debug != nil {
		q.Add("Debug", strconv.FormatBool(*opts.Debug))
	}
	if opts.Blocked != nil {
		q.Add("Blocked", strconv.FormatBool(*opts.Blocked))
		q.Add("BlockingReason", opts.BlockingReason)
	}
	if opts.Deleting != nil {
		q.Add("Deleting", strconv.FormatBool(*opts.Deleting))
	}
	if opts.OnboardingFinished != nil {
		q.Add("OnboardingFinished", strconv.FormatBool(*opts.OnboardingFinished))
	}
	res, err := ac.Req(&request.Options{
		Method:  "PATCH",
		Path:    "/instances/" + domain,
		Queries: q,
	})
	if err != nil {
		return nil, err
	}
	return readInstance(res)
}

// DestroyInstance is used to delete an instance and all its data.
func (ac *AdminClient) DestroyInstance(domain string) error {
	if !validDomain(domain) {
		return fmt.Errorf("Invalid domain: %s", domain)
	}
	_, err := ac.Req(&request.Options{
		Method:     "DELETE",
		Path:       "/instances/" + domain,
		NoResponse: true,
	})
	return err
}

// GetDebug is used to known if an instance has its logger in debug mode.
func (ac *AdminClient) GetDebug(domain string) (bool, error) {
	if !validDomain(domain) {
		return false, fmt.Errorf("Invalid domain: %s", domain)
	}
	_, err := ac.Req(&request.Options{
		Method:     "GET",
		Path:       "/instances/" + domain + "/debug",
		NoResponse: true,
	})
	if err != nil {
		if e, ok := err.(*request.Error); ok {
			if e.Title == http.StatusText(http.StatusNotFound) {
				return false, nil
			}
		}
		return false, err
	}
	return true, nil
}

// EnableDebug sets the logger of an instance in debug mode.
func (ac *AdminClient) EnableDebug(domain string, ttl time.Duration) error {
	if !validDomain(domain) {
		return fmt.Errorf("Invalid domain: %s", domain)
	}
	_, err := ac.Req(&request.Options{
		Method:     "POST",
		Path:       "/instances/" + domain + "/debug",
		NoResponse: true,
		Queries: url.Values{
			"TTL": {ttl.String()},
		},
	})
	return err
}

// CleanSessions delete the databases for io.cozy.sessions and io.cozy.sessions.logins
func (ac *AdminClient) CleanSessions(domain string) error {
	if !validDomain(domain) {
		return fmt.Errorf("Invalid domain: %s", domain)
	}
	_, err := ac.Req(&request.Options{
		Method:     "DELETE",
		Path:       "/instances/" + domain + "/sessions",
		NoResponse: true,
	})
	return err
}

// DisableDebug disables the debug mode for the logger of an instance.
func (ac *AdminClient) DisableDebug(domain string) error {
	if !validDomain(domain) {
		return fmt.Errorf("Invalid domain: %s", domain)
	}
	_, err := ac.Req(&request.Options{
		Method:     "DELETE",
		Path:       "/instances/" + domain + "/debug",
		NoResponse: true,
	})
	return err
}

// GetToken is used to generate a token with the specified options.
func (ac *AdminClient) GetToken(opts *TokenOptions) (string, error) {
	q := url.Values{
		"Domain":   {opts.Domain},
		"Subject":  {opts.Subject},
		"Audience": {opts.Audience},
		"Scope":    {strings.Join(opts.Scope, " ")},
	}
	if opts.Expire != nil {
		q.Add("Expire", opts.Expire.String())
	}
	res, err := ac.Req(&request.Options{
		Method:  "POST",
		Path:    "/instances/token",
		Queries: q,
	})
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// RegisterOAuthClient register a new OAuth client associated to the specified
// instance.
func (ac *AdminClient) RegisterOAuthClient(opts *OAuthClientOptions) (map[string]interface{}, error) {
	q := url.Values{
		"Domain":                {opts.Domain},
		"RedirectURI":           {opts.RedirectURI},
		"ClientName":            {opts.ClientName},
		"SoftwareID":            {opts.SoftwareID},
		"AllowLoginScope":       {strconv.FormatBool(opts.AllowLoginScope)},
		"OnboardingSecret":      {opts.OnboardingSecret},
		"OnboardingApp":         {opts.OnboardingApp},
		"OnboardingPermissions": {opts.OnboardingPermissions},
		"OnboardingState":       {opts.OnboardingState},
	}
	res, err := ac.Req(&request.Options{
		Method:  "POST",
		Path:    "/instances/oauth_client",
		Queries: q,
	})
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var client map[string]interface{}
	if err = json.NewDecoder(res.Body).Decode(&client); err != nil {
		return nil, err
	}
	return client, nil
}

// Export launch the creation of a tarball to export data from an instance.
func (ac *AdminClient) Export(opts *ExportOptions) error {
	if !validDomain(opts.Domain) {
		return fmt.Errorf("Invalid domain: %s", opts.Domain)
	}

	downloadArchives := opts.LocalPath != ""

	res, err := ac.Req(&request.Options{
		Method: "POST",
		Path:   "/instances/" + url.PathEscape(opts.Domain) + "/export",
		Queries: url.Values{
			"admin-req": []string{strconv.FormatBool(downloadArchives)},
		},
	})
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if downloadArchives {
		channel, err := ac.RealtimeClient(RealtimeOptions{
			DocTypes: []string{consts.Exports},
		})
		if err != nil {
			return err
		}
		defer channel.Close()

		var j job.Job
		if err = json.NewDecoder(res.Body).Decode(&j); err != nil {
			return err
		}

		for evt := range channel.Channel() {
			if evt.Event == "error" {
				return fmt.Errorf("realtime: %s", evt.Payload.Title)
			}
			if evt.Event == realtime.EventUpdate && evt.Payload.Type == consts.Exports {
				var exportDoc move.ExportDoc
				err := json.Unmarshal(evt.Payload.Doc, &exportDoc)
				if err != nil {
					return err
				}

				if exportDoc.Domain != opts.Domain {
					continue
				}
				if exportDoc.State == move.ExportStateError {
					return fmt.Errorf("Failed to export instance: %s", exportDoc.Error)
				}
				if exportDoc.State != move.ExportStateDone {
					continue
				}

				cursors := append([]string{""}, exportDoc.PartsCursors...)
				partsCount := len(cursors)
				for i, pc := range cursors {
					res, err := ac.Req(&request.Options{
						Method: "GET",
						Path:   "/instances/" + url.PathEscape(exportDoc.Domain) + "/exports/" + exportDoc.ID() + "/data",
						Queries: url.Values{
							"cursor": {pc},
						},
					})
					if err != nil {
						return err
					}
					defer res.Body.Close()

					filename := fmt.Sprintf("%s - part%03d.zip", opts.Domain, i)
					if _, params, err := mime.ParseMediaType(res.Header.Get(echo.HeaderContentDisposition)); err != nil && params["filename"] != "" {
						filename = params["filename"]
					}

					fmt.Fprintf(os.Stdout, "Exporting archive %d/%d (%s)... ", i+1, partsCount, filename)

					filepath := path.Join(opts.LocalPath, filename)
					f, err := os.OpenFile(filepath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
					if err != nil {
						if !os.IsExist(err) {
							return err
						}
						if err := os.Remove(filepath); err != nil {
							return err
						}
						f, err = os.OpenFile(filepath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
						if err != nil {
							return err
						}
					}
					defer f.Close()

					if _, err := io.Copy(f, res.Body); err != nil {
						return err
					}

					fmt.Println("✅")
				}

				return nil
			}
		}
	}

	return nil
}

// Import launch the import of a tarball with data to put in an instance.
func (ac *AdminClient) Import(domain string, opts *ImportOptions) error {
	if !validDomain(domain) {
		return fmt.Errorf("Invalid domain: %s", domain)
	}
	q := url.Values{
		"manifest_url": {opts.ManifestURL},
	}
	_, err := ac.Req(&request.Options{
		Method:     "POST",
		Path:       "/instances/" + url.PathEscape(domain) + "/import",
		Queries:    q,
		NoResponse: true,
	})
	return err
}

// RebuildRedis puts the triggers in redis.
func (ac *AdminClient) RebuildRedis() error {
	_, err := ac.Req(&request.Options{
		Method:     "POST",
		Path:       "/instances/redis",
		NoResponse: true,
	})
	return err
}

// DiskUsage returns the information about disk usage and quota
func (ac *AdminClient) DiskUsage(domain string, includeTrash bool) (map[string]interface{}, error) {
	var q map[string][]string
	if includeTrash {
		q = url.Values{
			"include": {"trash"},
		}
	}

	res, err := ac.Req(&request.Options{
		Method:  "GET",
		Path:    "/instances/" + url.PathEscape(domain) + "/disk-usage",
		Queries: q,
	})
	if err != nil {
		return nil, err
	}
	var info map[string]interface{}
	if err = json.NewDecoder(res.Body).Decode(&info); err != nil {
		return nil, err
	}
	return info, nil
}

func readInstance(res *http.Response) (*Instance, error) {
	in := &Instance{}
	if err := readJSONAPI(res.Body, &in); err != nil {
		return nil, err
	}
	return in, nil
}

func validDomain(domain string) bool {
	return !strings.ContainsAny(domain, " /?#@\t\r\n")
}
