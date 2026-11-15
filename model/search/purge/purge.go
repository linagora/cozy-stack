// Package purge removes the search documents of an instance. It is apart from
// the search package so that the instance lifecycle can use it without an
// import cycle (search imports sharing, which imports the lifecycle).
package purge

import (
	"context"
	"time"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/opensearch"
)

// Names of the search indices, before the configured prefix.
const (
	FilesIndex  = "files_v1"
	ScopesIndex = "share_scopes_v1"
)

// CleanInstance deletes the documents owned by an instance from the search
// indices. It does nothing when search is not configured.
func CleanInstance(domain string) error {
	client := opensearch.New(config.GetConfig().Search)
	if client == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	query := map[string]any{"term": map[string]any{"owner_instance_id": domain}}
	for _, index := range []string{FilesIndex, ScopesIndex} {
		if err := client.DeleteByQuery(ctx, index, query); err != nil {
			return err
		}
	}
	return nil
}
