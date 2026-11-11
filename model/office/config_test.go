package office

import (
	"testing"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetConfig(t *testing.T) {
	config.UseTestFile(t)
	complete := config.Office{OnlyOfficeURL: "https://documentserver.example", InboxSecret: "inbox", OutboxSecret: "outbox"}
	withoutOutbox := config.Office{OnlyOfficeURL: "https://documentserver.example", InboxSecret: "inbox"}
	config.GetConfig().Office = map[string]config.Office{
		config.DefaultInstanceContext: complete,
		"incomplete":                  withoutOutbox,
	}
	t.Cleanup(func() { config.GetConfig().Office = nil })

	cfg := GetConfig(config.DefaultInstanceContext)
	require.NotNil(t, cfg)
	assert.Equal(t, complete, *cfg)

	cfg = GetConfig("unknown")
	require.NotNil(t, cfg, "a context without a section uses the default one")
	assert.Equal(t, complete, *cfg)

	assert.Nil(t, GetConfig("incomplete"), "a context without the outbox secret has no document server, and does not use the default one")

	config.GetConfig().Office = map[string]config.Office{config.DefaultInstanceContext: withoutOutbox}
	assert.Nil(t, GetConfig("unknown"))
	assert.Nil(t, GetConfig(config.DefaultInstanceContext))
}
