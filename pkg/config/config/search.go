package config

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/cozy/cozy-stack/pkg/tlsclient"
	"github.com/spf13/viper"
)

// Search contains the configuration of the OpenSearch cluster used for the
// server-side file search. Search is off when URL is nil.
type Search struct {
	URL         *url.URL
	Username    string
	Password    string
	IndexPrefix string
	Client      *http.Client
}

func makeSearch(v *viper.Viper) (Search, error) {
	raw := v.GetString("search.opensearch.url")
	if raw == "" {
		return Search{}, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Search{}, fmt.Errorf("invalid search.opensearch.url config: %q", raw)
	}
	client, _, err := tlsclient.NewHTTPClient(tlsclient.HTTPEndpoint{
		Timeout:    10 * time.Second,
		RootCAFile: v.GetString("search.opensearch.root_ca"),
	})
	if err != nil {
		return Search{}, err
	}
	return Search{
		URL:         u,
		Username:    v.GetString("search.opensearch.username"),
		Password:    v.GetString("search.opensearch.password"),
		IndexPrefix: v.GetString("search.opensearch.index_prefix"),
		Client:      client,
	}, nil
}
