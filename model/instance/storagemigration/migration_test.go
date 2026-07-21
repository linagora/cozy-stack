package storagemigration_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"errors"
	"io"
	"net/url"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/storagemigration"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/model/vfs/vfsafero"
	"github.com/cozy/cozy-stack/model/vfs/vfss3"
	"github.com/cozy/cozy-stack/model/vfs/vfsswift"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/minio/minio-go/v7"
	swiftv2 "github.com/ncw/swift/v2"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

// migrationPrefixer is a minimal vfs.Prefixer (+ GetOrgID, required by
// vfss3.New's bucket-name derivation) implementation local to this external
// test package, mirroring model/vfs/vfs_test.go's contexter fixture.
type migrationPrefixer struct {
	cluster int
	domain  string
	prefix  string
	context string
}

func (p *migrationPrefixer) DBCluster() int         { return p.cluster }
func (p *migrationPrefixer) DomainName() string     { return p.domain }
func (p *migrationPrefixer) DBPrefix() string       { return p.prefix }
func (p *migrationPrefixer) GetContextName() string { return p.context }
func (p *migrationPrefixer) GetOrgID() string       { return "migrationtestorg" }

// migrationDisk is a minimal vfs.DiskThresholder (unlimited quota).
type migrationDisk struct{}

func (migrationDisk) DiskQuota() int64 { return 0 }

// migrationFixture bundles the source (afero) and target (s3) VFS + Avatarer
// pairs, plus the shared prefixer/db handle used to run CopyContent and to
// inspect CouchDB directly for the index-unchanged assertion.
type migrationFixture struct {
	db  *migrationPrefixer
	src vfs.VFS
	dst vfs.VFS

	srcAv vfs.Avatarer
	dstAv vfs.Avatarer

	// minioClient and bucket give tests raw access to the S3 target, e.g. to
	// delete an object behind the target VFS's back for negative-path checks.
	minioClient *minio.Client
	bucket      string
}

func setupMigrationFixture(t *testing.T) *migrationFixture {
	t.Helper()

	config.UseTestFile(t)

	db := &migrationPrefixer{
		cluster: 0,
		domain:  "io.cozy.storagemigration.test",
		prefix:  "io.cozy.storagemigration.test",
		context: "cozy_beta",
	}
	index := vfs.NewCouchdbIndexer(db)

	require.NoError(t, couchdb.ResetDB(db, consts.Files))
	require.NoError(t, couchdb.ResetDB(db, consts.FilesVersions))
	t.Cleanup(func() {
		_ = couchdb.DeleteDB(db, consts.Files)
		_ = couchdb.DeleteDB(db, consts.FilesVersions)
	})

	g, _ := errgroup.WithContext(context.Background())
	couchdb.DefineIndexes(g, db, couchdb.IndexesByDoctype(consts.Files))
	couchdb.DefineViews(g, db, couchdb.ViewsByDoctype(consts.Files))
	require.NoError(t, g.Wait())

	// Source: afero-backed VFS on a temp dir.
	tempdir := t.TempDir()
	aferoMutex := config.Lock().ReadWrite(db, "storagemigration-test-afero")
	aferoURL := &url.URL{Scheme: "file", Host: "localhost", Path: tempdir}
	src, err := vfsafero.New(db, index, &migrationDisk{}, aferoMutex, aferoURL, "io.cozy.vfs.test")
	require.NoError(t, err)
	require.NoError(t, src.InitFs())

	baseFS := afero.NewBasePathFs(afero.NewOsFs(), tempdir)
	srcAv := vfsafero.NewAvatarFs(baseFS)

	// Target: S3-backed VFS against a MinIO test container.
	mf := testutils.StartMinio(t)
	require.NoError(t, config.InitS3Connection(config.Fs{URL: mf.FsURL("test")}))

	s3Mutex := config.Lock().ReadWrite(db, "storagemigration-test-s3")
	dst, err := vfss3.New(db, index, &migrationDisk{}, s3Mutex)
	require.NoError(t, err)

	bucket := vfss3.BucketName(db.GetOrgID(), config.GetS3BucketPrefix())
	client := mf.Client(t)
	require.NoError(t, client.MakeBucket(context.Background(), bucket, minio.MakeBucketOptions{}))

	keyPrefix := db.DBPrefix() + "/"
	dstAv := vfss3.NewAvatarFs(client, bucket, keyPrefix)

	return &migrationFixture{
		db:          db,
		src:         src,
		dst:         dst,
		srcAv:       srcAv,
		dstAv:       dstAv,
		minioClient: client,
		bucket:      bucket,
	}
}

// createSourceFile creates a file of the given name/content on the source
// VFS and returns its FileDoc.
func createSourceFile(t *testing.T, fx *migrationFixture, name string, content []byte) *vfs.FileDoc {
	t.Helper()

	doc, err := vfs.NewFileDoc(name, "", int64(len(content)), nil, "text/plain", "text", time.Now(), false, false, false, []string{})
	require.NoError(t, err)

	f, err := fx.src.CreateFile(doc, nil)
	require.NoError(t, err)

	_, err = io.Copy(f, bytes.NewReader(content))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	got, err := fx.src.FileByPath("/" + name)
	require.NoError(t, err)
	return got
}

func TestCopyContentMovesFilesVersionsAndAvatar(t *testing.T) {
	fx := setupMigrationFixture(t)

	// 2 live files.
	file1 := createSourceFile(t, fx, "file1.txt", []byte("hello from file 1"))
	file2 := createSourceFile(t, fx, "file2.txt", []byte("hello from file 2, a bit longer"))

	// 1 file that gets trashed (content copy must still include it, since
	// ForeachDocs is unfiltered).
	file3 := createSourceFile(t, fx, "file3.txt", []byte("this one goes to the trash"))
	file3, err := vfs.TrashFile(fx.src, file3)
	require.NoError(t, err)

	// 1 extra version on file1.
	versionPayload := []byte("an older revision of file 1")
	sum := md5.Sum(versionPayload)
	internalID := utils.RandomString(16)
	version := &vfs.Version{
		DocID:    file1.DocID + "/" + internalID,
		ByteSize: int64(len(versionPayload)),
		MD5Sum:   sum[:],
	}
	version.Rels.File.Data.ID = file1.DocID
	require.NoError(t, fx.src.ImportFileVersion(version, io.NopCloser(bytes.NewReader(versionPayload))))

	// Avatar.
	avatarPayload := []byte("fake png bytes for the avatar")
	aw, err := fx.srcAv.CreateAvatar("image/png")
	require.NoError(t, err)
	_, err = aw.Write(avatarPayload)
	require.NoError(t, err)
	require.NoError(t, aw.Close())

	// Capture revs before the copy: CopyContent must not touch the index.
	revBefore1 := file1.Rev()
	revBefore2 := file2.Rev()
	revBefore3 := file3.Rev()

	rep, err := storagemigration.CopyContent(fx.db, fx.src, fx.dst, fx.srcAv, fx.dstAv)
	require.NoError(t, err)

	assert.Equal(t, 3, rep.Files) // 2 live + 1 trashed
	assert.Equal(t, 1, rep.Versions)
	assert.True(t, rep.AvatarCopied)

	// Every source file's bytes are now readable from the target VFS.
	assertFileContentOn(t, fx.dst, file1, []byte("hello from file 1"))
	assertFileContentOn(t, fx.dst, file2, []byte("hello from file 2, a bit longer"))
	assertFileContentOn(t, fx.dst, file3, []byte("this one goes to the trash"))

	// The version is readable via the target VFS.
	vr, err := fx.dst.OpenFileVersion(file1, version)
	require.NoError(t, err)
	gotVersion, err := io.ReadAll(vr)
	require.NoError(t, err)
	require.NoError(t, vr.Close())
	assert.Equal(t, versionPayload, gotVersion)

	// The avatar is readable via the target avatarer. Note: the source
	// avatarer is afero-backed, which does not persist a content-type on
	// disk and always reports "application/octet-stream" from OpenAvatar
	// (see vfsafero's OpenAvatar); CopyContent faithfully forwards whatever
	// content-type srcAv.OpenAvatar() reports to dstAv.CreateAvatar(), so
	// that is what ends up stored on the target too.
	ar, ctype, err := fx.dstAv.OpenAvatar()
	require.NoError(t, err)
	gotAvatar, err := io.ReadAll(ar)
	require.NoError(t, err)
	require.NoError(t, ar.Close())
	assert.Equal(t, "application/octet-stream", ctype)
	assert.Equal(t, avatarPayload, gotAvatar)

	// The CouchDB index is unchanged: same revs as before the copy.
	reread1 := &vfs.FileDoc{}
	require.NoError(t, couchdb.GetDoc(fx.db, consts.Files, file1.DocID, reread1))
	reread2 := &vfs.FileDoc{}
	require.NoError(t, couchdb.GetDoc(fx.db, consts.Files, file2.DocID, reread2))
	reread3 := &vfs.FileDoc{}
	require.NoError(t, couchdb.GetDoc(fx.db, consts.Files, file3.DocID, reread3))

	assert.Equal(t, revBefore1, reread1.Rev())
	assert.Equal(t, revBefore2, reread2.Rev())
	assert.Equal(t, revBefore3, reread3.Rev())
}

func TestVerifySucceedsAfterCopyAndFailsWhenObjectMissing(t *testing.T) {
	fx := setupMigrationFixture(t)

	file1 := createSourceFile(t, fx, "file1.txt", []byte("hello from file 1"))
	_ = createSourceFile(t, fx, "file2.txt", []byte("hello from file 2, a bit longer"))

	rep, err := storagemigration.CopyContent(fx.db, fx.src, fx.dst, fx.srcAv, fx.dstAv)
	require.NoError(t, err)

	require.NoError(t, storagemigration.Verify(fx.db, fx.dst, fx.dstAv, rep))

	// Remove one known target object directly via the raw MinIO client, then
	// confirm Verify now detects the discrepancy.
	keyPrefix := fx.db.DBPrefix() + "/"
	objKey := vfss3.MakeObjectKey(keyPrefix, file1.DocID, file1.InternalID)

	require.NoError(t, fx.minioClient.RemoveObject(context.Background(), fx.bucket, objKey, minio.RemoveObjectOptions{}))

	assert.Error(t, storagemigration.Verify(fx.db, fx.dst, fx.dstAv, rep))
}

func assertFileContentOn(t *testing.T, fs vfs.VFS, doc *vfs.FileDoc, want []byte) {
	t.Helper()
	r, err := fs.OpenFile(doc)
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	assert.Equal(t, want, got)
}

// setupMigrateInstance creates a real instance (via testutils, on the global
// test backend, "mem") and populates it with a couple of files and an
// avatar, then starts a MinIO test server and wires up the global S3 client
// so config.HasS3Client() is true and Migrate can build an S3 target.
func setupMigrateInstance(t *testing.T) *instance.Instance {
	t.Helper()

	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()

	mf := testutils.StartMinio(t)
	require.NoError(t, config.InitS3Connection(config.Fs{URL: mf.FsURL("test")}))

	createInstanceFile(t, inst, "migrate-file1.txt", []byte("hello from migrate file 1"))
	createInstanceFile(t, inst, "migrate-file2.txt", []byte("hello from migrate file 2, a bit longer"))

	aw, err := inst.AvatarFS().CreateAvatar("image/png")
	require.NoError(t, err)
	_, err = aw.Write([]byte("fake png bytes for the migrate avatar"))
	require.NoError(t, err)
	require.NoError(t, aw.Close())

	return inst
}

// createInstanceFile creates a file of the given name/content on the
// instance's current VFS.
func createInstanceFile(t *testing.T, inst *instance.Instance, name string, content []byte) *vfs.FileDoc {
	t.Helper()

	doc, err := vfs.NewFileDoc(name, "", int64(len(content)), nil, "text/plain", "text", time.Now(), false, false, false, []string{})
	require.NoError(t, err)

	f, err := inst.VFS().CreateFile(doc, nil)
	require.NoError(t, err)

	_, err = io.Copy(f, bytes.NewReader(content))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	got, err := inst.VFS().FileByPath("/" + name)
	require.NoError(t, err)
	return got
}

func TestMigrateFlipsSchemeAfterVerify(t *testing.T) {
	inst := setupMigrateInstance(t)

	rep, err := storagemigration.Migrate(inst, storagemigration.Options{To: config.SchemeS3})
	require.NoError(t, err)
	require.NotNil(t, rep)

	assert.Equal(t, config.SchemeS3, inst.FsScheme)
	assert.Greater(t, rep.Files, 0)
	assert.False(t, inst.Blocked, "instance must be unblocked after a successful migration")

	// Reads are now served from S3: build a fresh S3 VFS for the instance
	// (mirroring what inst.VFS() would now build) and confirm the migrated
	// files are readable from it.
	index := vfs.NewCouchdbIndexer(inst)
	disk := vfs.DiskThresholder(inst)
	mutex := config.Lock().ReadWrite(inst, "vfs-migrate-test-read")
	s3fs, err := vfss3.New(inst, index, disk, mutex)
	require.NoError(t, err)

	doc1, err := s3fs.FileByPath("/migrate-file1.txt")
	require.NoError(t, err)
	assertFileContentOn(t, s3fs, doc1, []byte("hello from migrate file 1"))

	doc2, err := s3fs.FileByPath("/migrate-file2.txt")
	require.NoError(t, err)
	assertFileContentOn(t, s3fs, doc2, []byte("hello from migrate file 2, a bit longer"))
}

func TestMigrateDryRunDoesNotFlip(t *testing.T) {
	inst := setupMigrateInstance(t)

	rep, err := storagemigration.Migrate(inst, storagemigration.Options{To: config.SchemeS3, DryRun: true})
	require.NoError(t, err)
	require.NotNil(t, rep)
	assert.Greater(t, rep.Files, 0)

	assert.Equal(t, "", inst.FsScheme)
	assert.False(t, inst.Blocked, "instance must be unblocked after a dry-run migration")
}

func TestMigrateFlagOnlyRequiresForce(t *testing.T) {
	inst := setupMigrateInstance(t)

	_, err := storagemigration.Migrate(inst, storagemigration.Options{To: config.SchemeS3, FlagOnly: true})
	require.Error(t, err)
	assert.Equal(t, "", inst.FsScheme)
}

// TestMigrateFlagOnlyFlipsWhenTargetPopulated covers the CRITICAL fix: a
// FlagOnly+Force flip must succeed (and actually flip) once the target
// backend genuinely already holds the source's content.
func TestMigrateFlagOnlyFlipsWhenTargetPopulated(t *testing.T) {
	inst := setupMigrateInstance(t)

	// Populate the S3 target for real once, so it already contains the
	// instance's full content (2 files + avatar) by the time the flag-only
	// flip below relies on it.
	_, err := storagemigration.Migrate(inst, storagemigration.Options{To: config.SchemeS3})
	require.NoError(t, err)
	require.Equal(t, config.SchemeS3, inst.FsScheme)

	// Simulate a rollback scenario: the instance is pointed back at its
	// (still fully intact, never purged) previous scheme, and we now want
	// to flip it back onto the S3 target without recopying anything, since
	// that target is already fully populated from the migration above.
	inst.FsScheme = ""

	rep, err := storagemigration.Migrate(inst, storagemigration.Options{To: config.SchemeS3, FlagOnly: true, Force: true})
	require.NoError(t, err)
	require.NotNil(t, rep)

	assert.Equal(t, config.SchemeS3, inst.FsScheme)
	assert.Equal(t, 2, rep.Files)
	assert.True(t, rep.AvatarCopied)
}

// TestMigrateFlagOnlyFailsWhenTargetEmpty covers the CRITICAL fix's negative
// path: a FlagOnly+Force flip against a target that only exists (e.g. an
// empty bucket created by buildTarget's EnsureBucket call) but does not
// actually hold the source's content must fail, and must NOT flip
// FsScheme.
func TestMigrateFlagOnlyFailsWhenTargetEmpty(t *testing.T) {
	inst := setupMigrateInstance(t)

	_, err := storagemigration.Migrate(inst, storagemigration.Options{To: config.SchemeS3, FlagOnly: true, Force: true})
	require.Error(t, err)
	assert.Equal(t, "", inst.FsScheme)
}

// TestMigratePurgeSourceRemovesSourceObjects covers the IMPORTANT fix: a
// swift source must actually be purged (not return a "not implemented"
// error) after a successful flip. It exercises the real swift-source purge
// path end-to-end: an instance is first migrated from mem to a real
// (in-memory swifttest server) swift backend, populating swift for real;
// it is then migrated from swift to S3 with PurgeSource, and the test
// confirms the swift container backing the instance is gone afterward.
func TestMigratePurgeSourceRemovesSourceObjects(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	setup := testutils.NewSetup(t, t.Name())
	setup.SetupSwiftTest()
	inst := setup.GetTestInstance()

	// GetTestInstance created this instance against the test config's
	// default (non-swift) scheme, so it was never assigned a swift layout.
	// Migrate requires layout v3 for any swift source (see the SwiftLayout
	// guard in Migrate), so set it explicitly here to simulate a real
	// swift-scheme instance, as would exist in production.
	inst.SwiftLayout = 2
	require.NoError(t, instance.Update(inst))

	mf := testutils.StartMinio(t)
	require.NoError(t, config.InitS3Connection(config.Fs{URL: mf.FsURL("test")}))

	createInstanceFile(t, inst, "purge-file1.txt", []byte("hello from purge file 1"))
	createInstanceFile(t, inst, "purge-file2.txt", []byte("hello from purge file 2, a bit longer"))

	// Step 1: migrate mem -> swift for real, so the swift container backing
	// this instance is genuinely populated.
	_, err := storagemigration.Migrate(inst, storagemigration.Options{To: config.SchemeSwift})
	require.NoError(t, err)
	require.Equal(t, config.SchemeSwift, inst.FsScheme)

	containerName := swiftContainerName(t, inst)

	// Sanity check: the container really exists before the purge.
	_, _, err = config.GetSwiftConnection().Container(context.Background(), containerName)
	require.NoError(t, err, "the swift container must exist after the first migration")

	// Step 2: migrate swift -> S3 with PurgeSource, exercising the swift
	// source purge implementation.
	_, err = storagemigration.Migrate(inst, storagemigration.Options{To: config.SchemeS3, PurgeSource: true})
	require.NoError(t, err)
	assert.Equal(t, config.SchemeS3, inst.FsScheme)

	// The swift container must be gone now: purgeSource must have actually
	// deleted it, not returned a "not implemented" error after a
	// successful (and now unrevertable) flip.
	_, _, err = config.GetSwiftConnection().Container(context.Background(), containerName)
	assert.True(t, errors.Is(err, swiftv2.ContainerNotFound), "expected the swift container to be gone after purge, got err=%v", err)
}

// swiftContainerName builds the same per-instance swift V3 container that
// buildTarget/purgeSource use, so tests can inspect it directly against the
// swift connection.
func swiftContainerName(t *testing.T, inst *instance.Instance) string {
	t.Helper()

	index := vfs.NewCouchdbIndexer(inst)
	disk := vfs.DiskThresholder(inst)
	mutex := config.Lock().ReadWrite(inst, "storagemigration-test-swift-container-name")

	sfs, err := vfsswift.NewV3(inst, index, disk, mutex)
	require.NoError(t, err)

	cn, ok := sfs.(interface{ ContainerNames() map[string]string })
	require.True(t, ok, "vfsswift.NewV3 must expose ContainerNames()")

	return cn.ContainerNames()["container"]
}

// TestMigratePurgeOnlyReclaimsOtherBackend covers the CRITICAL fix: once an
// instance already sits on its target scheme (a previous migration flipped
// it, and the Swift source was deliberately retained for rollback, as
// docs/s3.md step 4 describes), a later call with PurgeSource and the SAME
// To must not hit the "already uses that scheme" guard. Instead it must run
// in purge-only mode: reclaim the other backend's leftover data without
// copying, verifying, or flipping anything.
func TestMigratePurgeOnlyReclaimsOtherBackend(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	setup := testutils.NewSetup(t, t.Name())
	setup.SetupSwiftTest()
	inst := setup.GetTestInstance()

	// See TestMigratePurgeSourceRemovesSourceObjects: a swift source requires
	// layout v3 to be migrated.
	inst.SwiftLayout = 2
	require.NoError(t, instance.Update(inst))

	mf := testutils.StartMinio(t)
	require.NoError(t, config.InitS3Connection(config.Fs{URL: mf.FsURL("test")}))

	createInstanceFile(t, inst, "purge-only-file1.txt", []byte("hello from purge-only file 1"))
	createInstanceFile(t, inst, "purge-only-file2.txt", []byte("hello from purge-only file 2, a bit longer"))

	// Step 1: migrate mem -> swift for real, so the swift container backing
	// this instance is genuinely populated.
	_, err := storagemigration.Migrate(inst, storagemigration.Options{To: config.SchemeSwift})
	require.NoError(t, err)
	require.Equal(t, config.SchemeSwift, inst.FsScheme)

	containerName := swiftContainerName(t, inst)

	// Step 2: migrate swift -> S3 WITHOUT PurgeSource, so the instance ends
	// on S3 while the swift source is deliberately retained, exactly as
	// docs/s3.md's rollback window describes.
	_, err = storagemigration.Migrate(inst, storagemigration.Options{To: config.SchemeS3})
	require.NoError(t, err)
	require.Equal(t, config.SchemeS3, inst.FsScheme)

	// Sanity check: the retained swift container still exists after the
	// flip, since PurgeSource was not requested.
	_, _, err = config.GetSwiftConnection().Container(context.Background(), containerName)
	require.NoError(t, err, "the swift container must still exist: PurgeSource was not requested on the flip")

	// Step 3 (the deferred reclaim, run later): call Migrate again with
	// To == the instance's CURRENT scheme (s3) and PurgeSource set. This
	// must not error out on the "already uses that scheme" guard; it must
	// instead purge the other backend (swift) and leave the instance as-is.
	rep, err := storagemigration.Migrate(inst, storagemigration.Options{To: config.SchemeS3, PurgeSource: true})
	require.NoError(t, err)
	require.NotNil(t, rep)
	assert.Equal(t, config.SchemeS3, inst.FsScheme, "purge-only must not change the instance's scheme")
	assert.False(t, inst.Blocked, "purge-only must not leave the instance blocked")

	// The swift container must be gone now.
	_, _, err = config.GetSwiftConnection().Container(context.Background(), containerName)
	assert.True(t, errors.Is(err, swiftv2.ContainerNotFound), "expected the swift container to be gone after purge-only, got err=%v", err)

	// The instance's S3 content (the active backend) must be untouched.
	index := vfs.NewCouchdbIndexer(inst)
	disk := vfs.DiskThresholder(inst)
	mutex := config.Lock().ReadWrite(inst, "vfs-migrate-test-purge-only-read")
	s3fs, err := vfss3.New(inst, index, disk, mutex)
	require.NoError(t, err)
	doc1, err := s3fs.FileByPath("/purge-only-file1.txt")
	require.NoError(t, err)
	assertFileContentOn(t, s3fs, doc1, []byte("hello from purge-only file 1"))
}

// TestMigratePurgeOnlyWithoutPurgeFlagStillErrors covers the guard that must
// still hold for a plain re-run against the current scheme without
// PurgeSource: purge-only mode is only entered when PurgeSource is set.
func TestMigratePurgeOnlyWithoutPurgeFlagStillErrors(t *testing.T) {
	inst := setupMigrateInstance(t)

	_, err := storagemigration.Migrate(inst, storagemigration.Options{To: config.SchemeS3})
	require.NoError(t, err)
	require.Equal(t, config.SchemeS3, inst.FsScheme)

	_, err = storagemigration.Migrate(inst, storagemigration.Options{To: config.SchemeS3})
	require.Error(t, err)
	assert.Equal(t, config.SchemeS3, inst.FsScheme)
}

// TestMigrateFlagOnlyDryRunDoesNotFlip covers the IMPORTANT fix: combining
// FlagOnly with DryRun must still verify the target, but must NOT flip
// FsScheme, even with Force set.
func TestMigrateFlagOnlyDryRunDoesNotFlip(t *testing.T) {
	inst := setupMigrateInstance(t)

	// Populate the S3 target for real once, so it already contains the
	// instance's full content by the time the flag-only dry-run below
	// relies on it.
	_, err := storagemigration.Migrate(inst, storagemigration.Options{To: config.SchemeS3})
	require.NoError(t, err)
	require.Equal(t, config.SchemeS3, inst.FsScheme)

	// Reset the scheme, as a rollback scenario would have it, then attempt a
	// flag-only flip back onto S3 as a dry run.
	inst.FsScheme = ""

	rep, err := storagemigration.Migrate(inst, storagemigration.Options{To: config.SchemeS3, FlagOnly: true, Force: true, DryRun: true})
	require.NoError(t, err)
	require.NotNil(t, rep)
	assert.Equal(t, 2, rep.Files)

	assert.Equal(t, "", inst.FsScheme, "a dry-run flag-only migration must not flip FsScheme")
	assert.False(t, inst.Blocked, "instance must be unblocked after a dry-run flag-only migration")
}
