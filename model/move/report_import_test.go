package move

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/require"
)

func TestImportReportAuthority(t *testing.T) {
	if testing.Short() {
		t.Skip("requires an instance")
	}
	config.UseTestFile(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	for _, move := range []bool{false, true} {
		for i, alias := range []func(string) string{
			func(s string) string { return s },
			couchdb.EscapeCouchdbName,
			func(s string) string { return strings.ToUpper(couchdb.EscapeCouchdbName(s)) },
			strings.ToUpper,
		} {
			for _, partial := range []bool{false, true} {
				name := fmt.Sprintf("move-%t-alias-%d-partial-%t", move, i, partial)
				t.Run(name, func(t *testing.T) {
					var buf bytes.Buffer
					zw := zip.NewWriter(&buf)
					add := func(doctype, id, body string) {
						w, err := zw.Create(ExportDataDir + "/" + doctype + "/" + id + ".json")
						require.NoError(t, err)
						_, err = w.Write([]byte(body))
						require.NoError(t, err)
					}
					for _, expiry := range []string{"2000-01-01T00:00:00Z", "2100-01-01T00:00:00Z"} {
						id := fmt.Sprintf("%s-%s", name, expiry[:4])
						add(alias(consts.Permissions), id, fmt.Sprintf(`{"_id":%q,"type":"share","activity_report_enabled":true,"expires_at":%q,"codes":{"link":"old"}}`, id, expiry))
					}
					add(alias(consts.PublicLinkReports), name, fmt.Sprintf(`{"_id":%q,"status":"pending"}`, name))
					add(consts.PublicLinkEvents, name, fmt.Sprintf(`{"_id":%q,"action":"opened"}`, name))
					if partial {
						add("io.cozy.foos", name, "{")
					}
					require.NoError(t, zw.Close())
					zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
					require.NoError(t, err)
					im := &importer{inst: inst, fs: inst.VFS(), servicesInError: make(map[string]bool)}
					if move {
						im.options.MoveFrom = &FromOptions{URL: "https://source.example"}
					}
					err = im.importZip(zr)
					if partial {
						require.Error(t, err)
					} else {
						require.NoError(t, err)
					}
					for _, year := range []string{"2000", "2100"} {
						var p permission.Permission
						err := couchdb.GetDoc(inst, consts.Permissions, name+"-"+year, &p)
						if move {
							require.NoError(t, err)
							require.True(t, strings.HasPrefix(p.Rev(), "1-"), "unexpected revision: %s", p.Rev())
							require.Nil(t, p.ActivityReportEnabled)
						} else {
							require.True(t, couchdb.IsNotFoundError(err), "%v", err)
						}
					}
					var doc couchdb.JSONDoc
					err = couchdb.GetDoc(inst, consts.PublicLinkReports, name, &doc)
					require.True(t, couchdb.IsNotFoundError(err), "%v", err)
					require.NoError(t, couchdb.GetDoc(inst, consts.PublicLinkEvents, name, &doc))
				})
			}
		}
	}
}
