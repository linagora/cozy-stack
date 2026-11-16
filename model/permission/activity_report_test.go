package permission

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/stretchr/testify/require"
)

func TestActivityReportOption(t *testing.T) {
	for _, raw := range []string{"null", `"true"`, "1", "{}", "[]"} {
		var p Permission
		require.Error(t, json.Unmarshal([]byte(`{"activity_report_enabled":`+raw+`}`), &p))
	}
	var p Permission
	require.NoError(t, json.Unmarshal([]byte(`{"activity_report_enabled": false}`), &p))
	require.NotNil(t, p.ActivityReportEnabled)
	require.False(t, *p.ActivityReportEnabled)
	clone := p.Clone().(*Permission)
	*clone.ActivityReportEnabled = true
	require.False(t, *p.ActivityReportEnabled)
	require.NoError(t, json.Unmarshal([]byte(`{"type":"share","codes":{"link":"secret"},"activity_report_enabled":true,"expires_at":"2026-01-02T04:04:05+01:00","permissions":{"files":{"type":"io.cozy.files"}}}`), &p))
	at, ok := p.ActivityReportExpiry()
	require.True(t, ok)
	require.Equal(t, "2026-01-02T03:04:05Z", at.Format(time.RFC3339))
	for _, mutate := range []func(*Permission){
		func(p *Permission) { p.ActivityReportEnabled = nil },
		func(p *Permission) { p.Type = TypeWebapp },
		func(p *Permission) { p.Codes = nil },
		func(p *Permission) { p.ExpiresAt = "invalid" },
		func(p *Permission) { p.ExpiresAt = nil },
		func(p *Permission) { p.Permissions = Set{{Type: consts.Contacts}} },
	} {
		q := p.Clone().(*Permission)
		mutate(q)
		_, ok := q.ActivityReportExpiry()
		require.False(t, ok)
	}
	require.Error(t, CheckReadable(consts.PublicLinkReports))
	require.Error(t, CheckWritable(consts.PublicLinkReports))
}
