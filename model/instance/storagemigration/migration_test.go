package storagemigration_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"io"
	"net/url"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/storagemigration"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/model/vfs/vfsafero"
	"github.com/cozy/cozy-stack/model/vfs/vfss3"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/minio/minio-go/v7"
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
