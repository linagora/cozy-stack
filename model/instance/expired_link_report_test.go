package instance_test

import (
	"context"
	"encoding/csv"
	"os"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/assets/statik"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/i18n"
	"github.com/cozy/cozy-stack/tests/testutils"
	_ "github.com/cozy/cozy-stack/web/statik" // Register the embedded assets used in production.
	"github.com/stretchr/testify/require"
)

func TestGenerateExpiredLinkReport(t *testing.T) {
	if testing.Short() {
		t.Skip("requires CouchDB and the test Stack instance")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	po := statik.GetAsset("/locales/en.po")
	require.NotNil(t, po)
	i18n.LoadLocale("en", "", po.GetData())
	inst := testutils.NewSetup(t, "expired-link-report").GetTestInstance()
	fs := inst.VFS()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// User-controlled paths and references must not authorize a privileged write.
	legacy, err := vfs.NewDirDocWithPath("Reports", consts.RootDirID, "/", nil)
	require.NoError(t, err)
	legacy.AddReferencedBy(couchdb.DocReference{Type: consts.Apps, ID: consts.Apps + "/reports"})
	require.NoError(t, fs.CreateDir(legacy))

	expiry := time.Now().Add(-time.Hour).Truncate(time.Second).UTC()
	enabled := true
	link := &permission.Permission{
		Type: permission.TypeShareByLink, ExpiresAt: expiry.Format(time.RFC3339Nano),
		Codes: map[string]string{"link": "example-sharecode"}, ActivityReportEnabled: &enabled,
		Permissions: []permission.Rule{{Type: consts.Files}},
	}
	require.NoError(t, couchdb.CreateDoc(inst, link))
	for _, e := range []*publiclink.Event{
		{LinkID: link.ID(), OccurredAt: expiry.Add(-time.Nanosecond), Action: publiclink.Opened, Identity: "  =example"},
		{LinkID: link.ID(), OccurredAt: expiry, Action: publiclink.Downloaded, IP: "2001:db8::1"},
		{LinkID: link.ID(), OccurredAt: expiry.Add(time.Nanosecond), Action: publiclink.Opened},
		{LinkID: "other-link", OccurredAt: expiry, Action: publiclink.Updated},
	} {
		require.NoError(t, couchdb.CreateDoc(inst, e))
	}

	_, err = inst.GenerateExpiredLinkReport(ctx, link.ID(), expiry.Add(time.Nanosecond))
	require.Error(t, err)
	_, err = fs.DirByPath(legacy.Fullpath + " (1)")
	require.ErrorIs(t, err, os.ErrNotExist)

	type result struct {
		file *vfs.FileDoc
		err  error
	}
	start := make(chan struct{})
	results := make(chan result, 3)
	for j := 0; j < cap(results); j++ {
		go func() {
			<-start
			f, err := inst.GenerateExpiredLinkReport(ctx, link.ID(), expiry)
			results <- result{f, err}
		}()
	}
	close(start)
	var file *vfs.FileDoc
	var generationErrors []error
	for j := 0; j < cap(results); j++ {
		res := <-results
		if res.err != nil {
			// A contender can encounter a publication still in progress.
			require.Nil(t, res.file)
			generationErrors = append(generationErrors, res.err)
			continue
		}
		if file != nil {
			require.Equal(t, file.ID(), res.file.ID())
			require.Equal(t, file.Rev(), res.file.Rev())
		}
		file = res.file
	}
	require.NotNil(t, file, "concurrent report errors: %v", generationErrors)
	dir, err := fs.DirByID(file.DirID)
	require.NoError(t, err)
	require.Equal(t, legacy.Fullpath+" (1)", dir.Fullpath)
	require.NotEqual(t, legacy.ID(), dir.ID())
	var listedFiles []string
	require.NoError(t, vfs.Walk(fs, dir.Fullpath, func(_ string, _ *vfs.DirDoc, f *vfs.FileDoc, err error) error {
		if f != nil {
			listedFiles = append(listedFiles, f.ID())
		}
		return err
	}))
	require.Equal(t, []string{file.ID()}, listedFiles)
	n, err := couchdb.CountAllDocs(inst, consts.PublicLinkReports)
	require.NoError(t, err)
	require.Equal(t, 2, n) // directory binding and publication attestation
	var snapshot couchdb.JSONDoc
	require.NoError(t, couchdb.GetDoc(inst, consts.PublicLinkReports, file.Metadata["public_link_report"].(string), &snapshot))
	require.Subset(t, snapshot.M, map[string]interface{}{
		"link_id": link.ID(), "directory_id": dir.ID(), "file_id": file.ID(), "file_rev": file.Rev(),
	})
	require.Len(t, snapshot.M, 8) // IDs, expiry and fingerprint only
	in, err := fs.OpenFile(file)
	require.NoError(t, err)
	rows, err := csv.NewReader(in).ReadAll()
	require.NoError(t, in.Close())
	require.NoError(t, err)
	require.Equal(t, [][]string{
		{"occurred_at", "IP", "identity", "action"},
		{expiry.Add(-time.Nanosecond).Format(time.RFC3339Nano), "", "'  =example", "opened"},
		{expiry.Format(time.RFC3339Nano), "2001:db8::1", "", "downloaded"},
	}, rows)

	retry, err := inst.GenerateExpiredLinkReport(ctx, link.ID(), expiry.In(time.FixedZone("offset", 3600)))
	require.NoError(t, err)
	require.Equal(t, file.ID(), retry.ID())
	require.Equal(t, file.Rev(), retry.Rev())

	// An antivirus-only update does not invalidate the published snapshot,
	// even after CouchDB has actually discarded the publication revision body.
	originRev := file.Rev()
	file.AntivirusScan = &vfs.AntivirusScan{Status: vfs.AVStatusClean}
	require.NoError(t, couchdb.UpdateDoc(inst, file))
	require.NoError(t, couchdb.Compact(inst, consts.Files))
	require.Eventually(t, func() bool {
		var before couchdb.JSONDoc
		return couchdb.IsNotFoundError(couchdb.GetDocRev(inst, consts.Files, file.ID(), originRev, &before))
	}, 10*time.Second, 50*time.Millisecond)
	retry, err = inst.GenerateExpiredLinkReport(ctx, link.ID(), expiry)
	require.NoError(t, err)
	require.Equal(t, file.Rev(), retry.Rev())

	// Revocation leaves the existing Drive file alone but blocks reuse.
	link.ActivityReportEnabled = nil
	require.NoError(t, couchdb.UpdateDoc(inst, link))
	retry, err = inst.GenerateExpiredLinkReport(ctx, link.ID(), expiry)
	require.Error(t, err)
	require.Nil(t, retry)
	link.ActivityReportEnabled = &enabled
	require.NoError(t, couchdb.UpdateDoc(inst, link))

	file.Metadata["edited"] = true
	require.NoError(t, couchdb.UpdateDoc(inst, file))
	retry, err = inst.GenerateExpiredLinkReport(ctx, link.ID(), expiry)
	require.Error(t, err)
	require.Nil(t, retry)
	stillThere, err := fs.FileByID(file.ID())
	require.NoError(t, err)
	require.Equal(t, true, stillThere.Metadata["edited"])
	require.NoError(t, couchdb.DeleteDoc(inst, stillThere))
	retry, err = inst.GenerateExpiredLinkReport(ctx, link.ID(), expiry)
	require.Error(t, err)
	require.Nil(t, retry)
	_, err = fs.FileByID(file.ID())
	require.ErrorIs(t, err, os.ErrNotExist)

	trashed, err := vfs.TrashDir(fs, dir)
	require.NoError(t, err)
	other := &permission.Permission{
		Type: permission.TypeShareByLink, ExpiresAt: expiry.Format(time.RFC3339Nano),
		Codes: map[string]string{"link": "another-sharecode"}, ActivityReportEnabled: &enabled,
		Permissions: []permission.Rule{{Type: consts.Files}},
	}
	require.NoError(t, couchdb.CreateDoc(inst, other))
	retry, err = inst.GenerateExpiredLinkReport(ctx, other.ID(), expiry)
	require.ErrorIs(t, err, vfs.ErrFileInTrash)
	require.Nil(t, retry)
	_, err = fs.DirByPath(legacy.Fullpath + " (1)")
	require.ErrorIs(t, err, os.ErrNotExist)

	_, err = vfs.RestoreDir(fs, trashed)
	require.NoError(t, err)
	usage, err := fs.DiskUsage()
	require.NoError(t, err)
	quota := inst.BytesDiskQuota
	inst.BytesDiskQuota = usage + 1
	retry, err = inst.GenerateExpiredLinkReport(ctx, other.ID(), expiry)
	inst.BytesDiskQuota = quota
	require.ErrorIs(t, err, vfs.ErrFileTooBig)
	require.Nil(t, retry)
	n, err = couchdb.CountAllDocs(inst, consts.PublicLinkReports)
	require.NoError(t, err)
	require.Equal(t, 2, n)

	retry, err = inst.GenerateExpiredLinkReport(ctx, other.ID(), expiry)
	require.NoError(t, err)
	in, err = fs.OpenFile(retry)
	require.NoError(t, err)
	rows, err = csv.NewReader(in).ReadAll()
	require.NoError(t, in.Close())
	require.NoError(t, err)
	require.Equal(t, [][]string{{"occurred_at", "IP", "identity", "action"}}, rows)
}
