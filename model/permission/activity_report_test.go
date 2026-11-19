package permission

import (
	"encoding/json"
	"testing"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/stretchr/testify/require"
)

func TestActivityReportOption(t *testing.T) {
	for _, raw := range []string{"null", `"true"`, "1", "{}", "[]"} {
		var p Permission
		require.Error(t, json.Unmarshal([]byte(`{"activity_report_enabled":`+raw+`}`), &p))
	}
	var p Permission
	require.NoError(t, json.Unmarshal([]byte(`{"activity_report_enabled":false}`), &p))
	require.NotNil(t, p.ActivityReportEnabled)
	require.False(t, *p.ActivityReportEnabled)
	clone := p.Clone().(*Permission)
	*clone.ActivityReportEnabled = true
	require.False(t, *p.ActivityReportEnabled)
	require.NoError(t, json.Unmarshal([]byte(`{"type":"share"}`), &p))
	require.False(t, *p.ActivityReportEnabled)
	require.Error(t, json.Unmarshal([]byte(`{"ACTIVITY_REPORT_ENABLED":null}`), &p))
	require.Error(t, json.Unmarshal([]byte(`{"activity_report_enabled":null,"activity_report_enabled":true}`), &p))
	require.Error(t, CheckReadable(consts.PublicLinkReports))
	require.Error(t, CheckWritable(consts.PublicLinkReports))
}
