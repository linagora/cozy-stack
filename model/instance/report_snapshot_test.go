package instance

import (
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/stretchr/testify/require"
)

func TestReportFingerprint(t *testing.T) {
	file, err := vfs.NewFileDoc("report.csv", "reports", 4, nil, "text/csv", "text", time.Unix(1, 0).UTC(), false, false, false, nil)
	require.NoError(t, err)
	file.DocID = "id"
	file.DocRev = "1-first"
	file.Metadata = vfs.Metadata{"public_link_report": "key"}
	data, err := json.Marshal(file)
	require.NoError(t, err)
	var head map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &head))
	head["_rev"] = "2-scan"
	head["_revisions"] = map[string]interface{}{"start": 2, "ids": []string{"scan", "first"}}
	head["antivirus_scan"] = map[string]string{"status": "clean"}

	original := sha256.Sum256([]byte("abcd"))
	changed := sha256.Sum256([]byte("dcba"))
	fingerprint, err := reportFingerprint(file, original[:])
	require.NoError(t, err)
	actual, err := reportFingerprint(head, original[:])
	require.NoError(t, err)
	require.Equal(t, fingerprint, actual)
	actual, err = reportFingerprint(head, changed[:])
	require.NoError(t, err)
	require.NotEqual(t, fingerprint, actual)
	head["dir_id"] = "different-folder"
	actual, err = reportFingerprint(head, original[:])
	require.NoError(t, err)
	require.NotEqual(t, fingerprint, actual)
}
