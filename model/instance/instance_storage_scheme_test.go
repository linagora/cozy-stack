package instance

import (
	"testing"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/stretchr/testify/assert"
)

func TestStorageSchemeFallsBackToGlobal(t *testing.T) {
	config.UseTestFile(t)
	i := &Instance{}
	assert.Equal(t, config.FsURL().Scheme, i.StorageScheme())
}

func TestStorageSchemeOverridesGlobal(t *testing.T) {
	config.UseTestFile(t)
	i := &Instance{FsScheme: config.SchemeS3}
	assert.Equal(t, config.SchemeS3, i.StorageScheme())
}
