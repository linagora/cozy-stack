package intent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/registry"
	"github.com/cozy/cozy-stack/pkg/utils"
)

// Service is a struct for an app that can serve an intent
type Service struct {
	Slug string `json:"slug"`
	Href string `json:"href"`
}

// AvailableApp is a struct for the apps that are in the apps registry but not
// installed, and can be used for the intent.
type AvailableApp struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// Intent is a struct for a call from a client-side app to have another app do
// something for it
type Intent struct {
	IID           string         `json:"_id,omitempty"`
	IRev          string         `json:"_rev,omitempty"`
	Action        string         `json:"action"`
	Type          string         `json:"type"`
	Permissions   []string       `json:"permissions"`
	Client        string         `json:"client"`
	Services      []Service      `json:"services"`
	AvailableApps []AvailableApp `json:"availableApps"`
}

// ID is used to implement the couchdb.Doc interface
func (in *Intent) ID() string { return in.IID }

// Rev is used to implement the couchdb.Doc interface
func (in *Intent) Rev() string { return in.IRev }

// DocType is used to implement the couchdb.Doc interface
func (in *Intent) DocType() string { return consts.Intents }

// Clone implements couchdb.Doc
func (in *Intent) Clone() couchdb.Doc {
	cloned := *in
	cloned.Permissions = make([]string, len(in.Permissions))
	copy(cloned.Permissions, in.Permissions)
	cloned.Services = make([]Service, len(in.Services))
	copy(cloned.Services, in.Services)
	cloned.AvailableApps = make([]AvailableApp, len(in.AvailableApps))
	copy(cloned.AvailableApps, in.AvailableApps)
	return &cloned
}

// SetID is used to implement the couchdb.Doc interface
func (in *Intent) SetID(id string) { in.IID = id }

// SetRev is used to implement the couchdb.Doc interface
func (in *Intent) SetRev(rev string) { in.IRev = rev }

// Save will persist the intent in CouchDB
func (in *Intent) Save(instance *instance.Instance) error {
	if in.ID() != "" {
		return couchdb.UpdateDoc(instance, in)
	}
	return couchdb.CreateDoc(instance, in)
}

// GenerateHref creates the href where the service can be called for an intent
// The base URL is the cozy subdomain of the app, unless the app manifest has a
// "service_url_flag" whose feature flag value is a valid URL (external host).
func (in *Intent) GenerateHref(instance *instance.Instance, slug, target string) string {
	u := instance.SubDomain(slug)
	prefix := ""
	if base := app.ExternalServiceURL(instance, slug); base != nil {
		u = base
		prefix = strings.TrimSuffix(base.Path, "/")
		u.Path = prefix
		u.RawQuery = ""
		u.Fragment = ""
	}
	parts := strings.SplitN(target, "#", 2)
	if len(parts[0]) > 0 {
		p := parts[0]
		if prefix != "" && !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		u.Path = prefix + p
	}
	if len(parts) == 2 && len(parts[1]) > 0 {
		u.Fragment = parts[1]
	}
	u.RawQuery = "intent=" + in.ID()
	return u.String()
}

// FillServices looks at all the application that can answer this intent
// and save them in the services field
func (in *Intent) FillServices(instance *instance.Instance) error {
	res, _, err := app.ListWebappsWithPagination(instance, 0, "")
	if err != nil {
		return err
	}
	for _, man := range res {
		if intent := man.FindIntent(in.Action, in.Type); intent != nil {
			href := in.GenerateHref(instance, man.Slug(), intent.Href)
			service := Service{Slug: man.Slug(), Href: href}
			in.Services = append(in.Services, service)
		}
	}
	return nil
}

type jsonAPIWebapp struct {
	Data  []*app.WebappManifest `json:"data"`
	Count int                   `json:"count"`
}

// GetInstanceWebapps returns the list of available webapps for the instance by
// iterating over its registries
func GetInstanceWebapps(inst *instance.Instance) ([]string, error) {
	man := jsonAPIWebapp{}
	apps := []string{}

	for _, regURL := range inst.Registries() {
		url, err := url.Parse(regURL.String())
		if err != nil {
			return nil, err
		}
		url.Path = path.Join(url.Path, "registry")
		url.RawQuery = "filter[type]=webapp"

		req, err := http.NewRequest("GET", url.String(), nil)
		if err != nil {
			return nil, err
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}

		err = json.NewDecoder(res.Body).Decode(&man)
		if err != nil {
			return nil, err
		}

		for _, app := range man.Data {
			slug := app.Slug()
			if !utils.IsInArray(slug, apps) {
				apps = append(apps, slug)
			}
		}
	}

	return apps, nil
}

// FillAvailableWebapps finds webapps which can answer to the intent from
// non-installed instance webapps
func (in *Intent) FillAvailableWebapps(inst *instance.Instance) error {
	// Webapps to exclude
	installedWebApps, _, err := app.ListWebappsWithPagination(inst, 0, "")
	if err != nil {
		return err
	}

	endSlugs := []string{}
	webapps, err := GetInstanceWebapps(inst)
	if err != nil {
		return err
	}
	// Only appending the non-installed webapps
	for _, wa := range webapps {
		found := false
		for _, iwa := range installedWebApps {
			if wa == iwa.Slug() {
				found = true
				break
			}
		}
		if !found {
			endSlugs = append(endSlugs, wa)
		}
	}

	lastVersions := map[string]app.WebappManifest{}
	versionsChan := make(chan app.WebappManifest)
	errorsChan := make(chan error)

	registries := inst.Registries()
	for _, webapp := range endSlugs {
		go func(webapp string) {
			webappMan := app.WebappManifest{}
			v, err := registry.GetLatestVersion(webapp, "stable", registries)
			if err != nil {
				errorsChan <- fmt.Errorf("Could not get last version for %s: %s", webapp, err)
				return
			}
			err = json.NewDecoder(bytes.NewReader(v.Manifest)).Decode(&webappMan)
			if err != nil {
				errorsChan <- fmt.Errorf("Could not get decode manifest for %s: %s", webapp, err)
				return
			}

			versionsChan <- webappMan
		}(webapp)
	}

	for range endSlugs {
		select {
		case err := <-errorsChan:
			inst.Logger().WithNamespace("intents").Error(err.Error())
		case version := <-versionsChan:
			lastVersions[version.Slug()] = version
		}
	}
	close(versionsChan)
	close(errorsChan)

	for _, manif := range lastVersions {
		if intent := manif.FindIntent(in.Action, in.Type); intent != nil {
			availableApp := AvailableApp{
				Name: manif.Name(),
				Slug: manif.Slug(),
			}
			in.AvailableApps = append(in.AvailableApps, availableApp)
		}
	}

	return nil
}

var _ couchdb.Doc = (*Intent)(nil)
