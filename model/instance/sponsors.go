package instance

import (
	"maps"
	"path/filepath"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/go-viper/mapstructure/v2"
)

type AppLogos struct {
	Light []LogoItem `json:"light,omitempty"`
	Dark  []LogoItem `json:"dark,omitempty"`
}
type LogoItem struct {
	Src  string `json:"src"`
	Alt  string `json:"alt"`
	Type string `json:"type,omitempty"`
}

func (i *Instance) GetContextWithSponsorships() map[string]interface{} {
	settings, _ := i.SettingsContext()
	// Callers add keys to the returned map, so it must not be the global config.
	context := maps.Clone(settings)
	if context == nil {
		context = map[string]interface{}{}
	}
	if len(i.Sponsorships) == 0 {
		return context
	}

	var logos map[string]AppLogos
	if err := mapstructure.Decode(context["logos"], &logos); err != nil || logos == nil {
		logos = make(map[string]AppLogos)
	}
	context["logos"] = logos

	contexts := config.GetConfig().Contexts
	if contexts == nil {
		return context
	}
	for _, sponsor := range i.Sponsorships {
		if sponsorCtx, ok := contexts[sponsor].(map[string]interface{}); ok {
			addHomeLogosForSponsor(context, sponsorCtx, sponsor)
		}
	}
	return context
}

func addHomeLogosForSponsor(context, sponsorContext map[string]interface{}, sponsor string) {
	sponsorLogos, ok := sponsorContext["logos"].(map[string]interface{})
	if !ok {
		return
	}
	var newLogos AppLogos
	if err := mapstructure.Decode(sponsorLogos["home"], &newLogos); err != nil {
		return
	}

	contextLogos := context["logos"].(map[string]AppLogos)
	homeLogos := contextLogos["home"]

	for _, logo := range newLogos.Light {
		if logo.Type == "main" {
			continue
		}
		found := false
		for _, item := range homeLogos.Light {
			if filepath.Base(item.Src) == filepath.Base(logo.Src) {
				found = true
			}
		}
		if found {
			continue
		}
		logo.Src = "/ext/" + sponsor + logo.Src
		homeLogos.Light = append(homeLogos.Light, logo)
	}

	for _, logo := range newLogos.Dark {
		if logo.Type == "main" {
			continue
		}
		found := false
		for _, item := range homeLogos.Dark {
			if filepath.Base(item.Src) == filepath.Base(logo.Src) {
				found = true
			}
		}
		if found {
			continue
		}
		logo.Src = "/ext/" + sponsor + logo.Src
		homeLogos.Dark = append(homeLogos.Dark, logo)
	}

	contextLogos["home"] = homeLogos
}
