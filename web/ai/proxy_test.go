package ai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAIProxyTenantScope(t *testing.T) {
	config.UseTestFile(t)
	forwarded := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		forwarded <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	config.GetConfig().RAGServers = map[string]config.RAGServer{
		config.DefaultInstanceContext: {URL: server.URL, APIKey: "test-key"},
	}
	for _, tt := range []struct {
		name, body, want string
		files            bool
		status           int
	}{
		{"other tenant", `{"model":"openrag-bob.example"}`, "", true, 403},
		{"all tenants", `{"model":"openrag-all"}`, "", true, 403},
		{"legacy other tenant", `{"model":"ragondin-bob.example"}`, "", true, 403},
		{"own tenant", `{"model":"openrag-alice.example","metadata":{"workspace_id":"other"}}`,
			`{"model":"openrag-alice.example"}`, true, 200},
		{"legacy own tenant", `{"model":"ragondin-alice.example"}`, `{"model":"ragondin-alice.example"}`, true, 200},
		{"files permission required", `{"model":"openrag-alice.example"}`, "", false, 403},
		{"direct LLM", `{"messages":[],"stream":false}`, `{"messages":[],"stream":false}`, false, 200},
		{"invalid JSON", `{`, "", true, 400},
		{"null", `null`, "", true, 400},
		{"invalid model type", `{"model":["openrag-all"]}`, "", true, 400},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := echo.New()
			e.HTTPErrorHandler = errors.ErrorHandler
			rec := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body)), rec)
			middlewares.SetInstance(c, &instance.Instance{Domain: "alice.example"})
			perms := permission.Set{{Type: consts.ChatConversations}}
			if tt.files {
				perms = append(perms, permission.Rule{Type: consts.Files})
			}
			middlewares.ForcePermission(c, &permission.Permission{Permissions: perms})
			if err := OpenAICompletion(c); err != nil {
				e.HTTPErrorHandler(err, c)
			}
			require.Equal(t, tt.status, rec.Code)
			if tt.status == http.StatusOK {
				assert.JSONEq(t, tt.want, <-forwarded)
			} else {
				assert.Empty(t, forwarded, "rejected requests must not reach openRAG")
			}
		})
	}
}
