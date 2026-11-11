package office

import "github.com/cozy/cozy-stack/pkg/config/config"

// GetConfig returns the document server of the context, or nil when the
// context has none or its configuration is incomplete.
func GetConfig(contextName string) *config.Office {
	configuration := config.GetConfig().Office
	c, ok := configuration[contextName]
	if !ok {
		c, ok = configuration[config.DefaultInstanceContext]
	}
	if !ok || c.Check() != nil {
		return nil
	}
	return &c
}
