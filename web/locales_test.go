package web

import (
	"io"
	"os"
	"path"
	"testing"

	"github.com/cozy/cozy-stack/pkg/assets"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/stretchr/testify/require"

	_ "github.com/cozy/cozy-stack/web/statik"
)

// TestEmbeddedLocalesMatchSources fails when a .po file has been edited without
// running `make assets`. The stack serves its catalogs from the embedded
// assets, while every other test loads them from disk, so without this the
// whole suite stays green and users get message ids where wording should be.
func TestEmbeddedLocalesMatchSources(t *testing.T) {
	entries, err := os.ReadDir("../assets/locales")
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	for _, entry := range entries {
		if path.Ext(entry.Name()) != ".po" {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			onDisk, err := os.ReadFile(path.Join("../assets/locales", entry.Name()))
			require.NoError(t, err)

			f, err := assets.Open("/locales/"+entry.Name(), config.DefaultInstanceContext)
			require.NoError(t, err, "not packed in the binary at all")
			embedded, err := io.ReadAll(f)
			require.NoError(t, err)

			require.Equal(t, string(onDisk), string(embedded), "run `make assets`")
		})
	}
}
