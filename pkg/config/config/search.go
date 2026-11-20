package config

import (
	"fmt"
	"net/url"
	"regexp"
	"time"

	"github.com/cozy/cozy-stack/pkg/opensearch"
	"github.com/cozy/cozy-stack/pkg/tlsclient"
	"github.com/spf13/viper"
)

var indexPrefixPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func makeSearch(v *viper.Viper) (*opensearch.Client, error) {
	raw := v.GetString("search.opensearch.url")
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid search.opensearch.url config: %q", raw)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return nil, fmt.Errorf("invalid search.opensearch.url config: query strings are unsupported")
	}
	prefix := v.GetString("search.opensearch.index_prefix")
	if prefix != "" && !indexPrefixPattern.MatchString(prefix) {
		return nil, fmt.Errorf("invalid search.opensearch.index_prefix config: %q", prefix)
	}
	client, _, err := tlsclient.NewHTTPClient(tlsclient.HTTPEndpoint{
		Timeout:    10 * time.Second,
		RootCAFile: v.GetString("search.opensearch.root_ca"),
	})
	if err != nil {
		return nil, err
	}
	return opensearch.NewClient(opensearch.Options{
		URL:         u,
		Username:    v.GetString("search.opensearch.username"),
		Password:    v.GetString("search.opensearch.password"),
		IndexPrefix: prefix,
		HTTPClient:  client,
	}), nil
}
