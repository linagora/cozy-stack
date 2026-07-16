package dynamic

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	"github.com/cozy/cozy-stack/pkg/assets/model"
	"github.com/cozy/cozy-stack/pkg/config/config"
)

// DynamicAssetsFolderName is the folder name for dynamic assets
const DynamicAssetsFolderName = "dyn-assets"

var assetFS AssetsFS

// AssetsFS is the interface implemented by all the implementations handling assets.
//
// At the moment there two separate implementations:
// - [SwiftFS] allowing to manage assets via an OpenStack Swift API.
// - [AferoFS] with [NewOsFS] allowing to manage assets directly on the host filesystem.
// - [AferoFS] with [NewInMemory] allowing to manage assets directly in a in-memory session.
type AssetsFS interface {
	Add(string, string, *model.Asset) error
	Get(string, string) ([]byte, error)
	Remove(string, string) error
	List() (map[string][]*model.Asset, error)
	CheckStatus(ctx context.Context) (time.Duration, error)
}

// InitDynamicAssetFS initializes the dynamic asset FS.
func InitDynamicAssetFS(fsURL string) error {
	u, err := url.Parse(fsURL)
	if err != nil {
		return err
	}

	switch u.Scheme {
	case config.SchemeMem:
		assetFS = NewInMemoryFS()

	case config.SchemeFile:
		assetFS, err = NewOsFS(filepath.Join(u.Path, DynamicAssetsFolderName))
		if err != nil {
			return err
		}

	case config.SchemeSwift, config.SchemeSwiftSecure:
		assetFS, err = NewSwiftFS()
		if err != nil {
			return err
		}

	case config.SchemeS3:
		assetFS, err = NewS3FS()
		if err != nil {
			return err
		}

	default:
		return fmt.Errorf("Invalid scheme %s for dynamic assets FS", u.Scheme)
	}

	return nil
}
