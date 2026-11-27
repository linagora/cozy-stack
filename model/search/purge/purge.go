// Package purge removes the search documents of an instance. It is apart from
// the search package so that the instance lifecycle can use it without an
// import cycle (search imports sharing, which imports the lifecycle).
package purge

import (
	"context"
	"errors"

	"github.com/cozy/cozy-stack/pkg/config/config"
)

// Names of the search indices, before the configured prefix.
const (
	FilesIndex  = "files_v1"
	ScopesIndex = "share_scopes_v1"
)

// purgeRate is the number of documents per second deleted from an index by a
// purge: the indices are shared by all the instances.
const purgeRate = 1000

// CleanInstance starts deleting the documents owned by an instance from the
// search indices, in OpenSearch tasks that it does not wait for. It does
// nothing when search is not configured.
func CleanInstance(ctx context.Context, domain string) error {
	client := config.GetConfig().Search
	if client == nil {
		return nil
	}
	query := map[string]any{"term": map[string]any{"owner_instance_id": domain}}
	var errs []error
	for _, index := range []string{FilesIndex, ScopesIndex} {
		if err := client.DeleteByQuery(ctx, index, query, purgeRate); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
