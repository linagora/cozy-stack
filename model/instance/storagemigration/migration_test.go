package storagemigration

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
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
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

// migrationPrefixer is a minimal vfs.Prefixer implementation local to this
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
	require.NoError(t, config.InitS3Connection(config.Fs{URL: mf.FsURL(), S3: config.FsS3{Buckets: map[string]config.FsS3Bucket{"default": {Name: "migration-storage"}}}}))

	s3Mutex := config.Lock().ReadWrite(db, "storagemigration-test-s3")
	dst, err := vfss3.New(db, index, &migrationDisk{}, s3Mutex)
	require.NoError(t, err)

	storage := config.GetS3Storage(config.S3StorageFiles)
	bucket, client := storage.Bucket, storage.Client
	keyPrefix := storage.Prefix + db.DBPrefix() + "/"
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

	rep, err := copyContent(fx.db, fx.src, fx.dst, fx.srcAv, fx.dstAv)
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

	rep, err := copyContent(fx.db, fx.src, fx.dst, fx.srcAv, fx.dstAv)
	require.NoError(t, err)

	require.NoError(t, verify(fx.db, fx.dst, fx.dstAv, rep))

	// Remove one known target object directly via the raw MinIO client, then
	// confirm Verify now detects the discrepancy.
	keyPrefix := "files/" + fx.db.DBPrefix() + "/"
	objKey := vfss3.MakeObjectKey(keyPrefix, file1.DocID, file1.InternalID)

	require.NoError(t, fx.minioClient.RemoveObject(context.Background(), fx.bucket, objKey, minio.RemoveObjectOptions{}))

	assert.Error(t, verify(fx.db, fx.dst, fx.dstAv, rep))
}

func TestCopyAvatarRemovesStaleTarget(t *testing.T) {
	src := vfsafero.NewAvatarFs(afero.NewMemMapFs())
	fs := afero.NewMemMapFs()
	dst := vfsafero.NewAvatarFs(fs)
	w, err := dst.CreateAvatar("image/png")
	require.NoError(t, err)
	_, err = w.Write([]byte("old avatar"))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	rep := &Report{}
	err = copyAvatar(src, vfsafero.NewAvatarFs(afero.NewReadOnlyFs(fs)), rep)
	require.ErrorContains(t, err, "remove target avatar")
	require.NoError(t, copyAvatar(src, dst, rep))
	_, _, err = dst.OpenAvatar()
	assert.ErrorIs(t, err, os.ErrNotExist)
	assert.False(t, rep.AvatarCopied)
	require.NoError(t, copyAvatar(src, dst, rep))
}

func captureMigrationLogs(t *testing.T) *logtest.Hook {
	t.Helper()
	oldHooks := logrus.StandardLogger().ReplaceHooks(make(logrus.LevelHooks))
	t.Cleanup(func() { logrus.StandardLogger().ReplaceHooks(oldHooks) })
	return logtest.NewGlobal()
}

func TestLogCopyProgress(t *testing.T) {
	hook := captureMigrationLogs(t)
	db := &migrationPrefixer{domain: "progress.cozy.local"}
	rep := &Report{Files: 12, Versions: 3, Bytes: 456}
	lastLog := time.Now().Add(-time.Minute)
	logCopyProgress(db, rep, &lastLog)
	logCopyProgress(db, rep, &lastLog)

	var entries []*logrus.Entry
	for _, entry := range hook.AllEntries() {
		if entry.Data["nspace"] == "storagemigration" {
			entries = append(entries, entry)
		}
	}
	require.Len(t, entries, 1)
	assert.Equal(t, db.DomainName(), entries[0].Data["domain"])
	assert.Equal(t, "Copy progress: files=12 versions=3 bytes=456", entries[0].Message)
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
	require.NoError(t, config.InitS3Connection(config.Fs{URL: mf.FsURL(), S3: config.FsS3{Buckets: map[string]config.FsS3Bucket{"default": {Name: "migration-storage"}}}}))

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

func TestMigrateRejectsEquivalentSwiftSchemes(t *testing.T) {
	inst := &instance.Instance{FsScheme: config.SchemeSwiftSecure}
	for _, purge := range []bool{false, true} {
		_, err := Migrate(inst, Options{To: config.SchemeSwift, PurgeSource: purge})
		require.EqualError(t, err, "storagemigration: swift and swift+https refer to the same backend")
	}
}

func TestMigrateRejectsDryRunWithPurge(t *testing.T) {
	for _, scheme := range []string{config.SchemeSwift, config.SchemeS3} {
		inst := &instance.Instance{FsScheme: scheme}
		_, err := Migrate(inst, Options{To: config.SchemeS3, DryRun: true, PurgeSource: true})
		assert.EqualError(t, err, "storagemigration: dry-run cannot be combined with purge-source")
	}
}

func TestWaitForQuietStorage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changed string
		failAt  int32
	}{
		{name: "quiet"},
		{name: "files changed", changed: consts.Files},
		{name: "versions changed", changed: consts.FilesVersions},
		{name: "initial status unavailable", failAt: 1},
		{name: "final status unavailable", failAt: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config.UseTestFile(t)
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				n := requests.Add(1)
				if n == tc.failAt {
					w.WriteHeader(http.StatusForbidden)
					fmt.Fprint(w, `{"error":"forbidden","reason":"denied"}`)
					return
				}
				doctype := path.Base(r.URL.Path)
				assert.Contains(t, []string{couchdb.EscapeCouchdbName(consts.Files), couchdb.EscapeCouchdbName(consts.FilesVersions)}, doctype)
				seq := doctype + "-a"
				if n > 2 && doctype == couchdb.EscapeCouchdbName(tc.changed) {
					seq = doctype + "-b"
				}
				fmt.Fprintf(w, `{"update_seq":%q}`, seq)
			}))
			defer srv.Close()
			u, err := url.Parse(srv.URL + "/")
			require.NoError(t, err)
			cfg := config.GetConfig()
			previousCouch := cfg.CouchDB
			defer func() { cfg.CouchDB = previousCouch }()
			cfg.CouchDB.Clusters = []config.CouchDBCluster{{URL: u}}
			cfg.CouchDB.Client = srv.Client()

			err = waitForQuietStorage(&migrationPrefixer{prefix: "quiet-test"}, time.Millisecond)
			switch {
			case tc.failAt != 0:
				require.ErrorContains(t, err, "update sequence:")
				assert.Equal(t, tc.failAt, requests.Load())
			case tc.changed != "":
				require.ErrorContains(t, err, "file storage changed during the quiet period")
			default:
				require.NoError(t, err)
				assert.EqualValues(t, 4, requests.Load())
			}
		})
	}
}

type migrationTransport func(*http.Request) (*http.Response, error)

func (f migrationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestMigrateHoldsVFSLock(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failCopy bool
		blocked  bool
	}{
		{name: "success"},
		{name: "copy failure", failCopy: true},
		{name: "already blocked", blocked: true},
		{name: "already blocked copy failure", blocked: true, failCopy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := setupMigrateInstance(t)
			if tc.blocked {
				inst.Blocked = true
				inst.BlockingReason = instance.BlockedLoginFailed.Code
				require.NoError(t, instance.Update(inst))
			}
			blockingReason := inst.BlockingReason
			hook := captureMigrationLogs(t)
			mutex := config.Lock().ReadWrite(inst, "vfs")
			probe, ok := mutex.(interface{ TryLock() bool })
			require.True(t, ok, "the test must use an in-memory lock")

			cfg := config.GetConfig()
			previousClient := cfg.CouchDB.Client
			defer func() { cfg.CouchDB.Client = previousClient }()
			client := *previousClient
			transport := client.Transport
			if transport == nil {
				transport = http.DefaultTransport
			}
			var enumerations atomic.Int32
			client.Transport = migrationTransport(func(req *http.Request) (*http.Response, error) {
				if path.Base(req.URL.Path) == "_all_docs" && path.Base(path.Dir(req.URL.Path)) == couchdb.EscapeCouchdbName(consts.Files) {
					enumerations.Add(1)
					if probe.TryLock() {
						mutex.Unlock()
						t.Error("the instance VFS must stay locked during copying and verification")
					}
					if tc.failCopy {
						return nil, errors.New("injected copy failure")
					}
				}
				return transport.RoundTrip(req)
			})
			cfg.CouchDB.Client = &client

			_, err := Migrate(inst, Options{To: config.SchemeS3})
			if tc.failCopy {
				require.ErrorContains(t, err, "injected copy failure")
				assert.EqualValues(t, 1, enumerations.Load())
				assert.Empty(t, inst.FsScheme)
			} else {
				require.NoError(t, err)
				assert.EqualValues(t, 2, enumerations.Load())
				assert.Equal(t, config.SchemeS3, inst.FsScheme)
			}
			assert.Equal(t, tc.blocked, inst.Blocked)
			assert.Equal(t, blockingReason, inst.BlockingReason)
			reloaded, err := instance.Get(inst.Domain)
			require.NoError(t, err)
			assert.Equal(t, tc.blocked, reloaded.Blocked)
			assert.Equal(t, blockingReason, reloaded.BlockingReason)
			require.True(t, probe.TryLock(), "migration must release the VFS lock on return")
			mutex.Unlock()

			var messages []string
			for _, entry := range hook.AllEntries() {
				if entry.Data["nspace"] == "storagemigration" {
					assert.Equal(t, inst.Domain, entry.Data["domain"])
					messages = append(messages, entry.Message)
				}
			}
			require.GreaterOrEqual(t, len(messages), 2)
			assert.Equal(t, "Restoring instance block state", messages[len(messages)-2])
			if tc.failCopy {
				assert.Contains(t, messages[len(messages)-1], "Migration failed")
			} else {
				assert.Contains(t, messages[len(messages)-1], "Migration completed: backend=s3")
				assert.Contains(t, messages, "Copying files")
				assert.Contains(t, messages, "Verifying copied content")
			}
		})
	}
}

func TestMigrateReportsBlockRestorationFailure(t *testing.T) {
	for _, failCopy := range []bool{false, true} {
		t.Run(fmt.Sprintf("copy_failure=%t", failCopy), func(t *testing.T) {
			inst := setupMigrateInstance(t)
			hook := captureMigrationLogs(t)
			cfg := config.GetConfig()
			previousClient := cfg.CouchDB.Client
			defer func() { cfg.CouchDB.Client = previousClient }()
			client := *previousClient
			transport := client.Transport
			if transport == nil {
				transport = http.DefaultTransport
			}
			client.Transport = migrationTransport(func(req *http.Request) (*http.Response, error) {
				if failCopy && path.Base(req.URL.Path) == "_all_docs" && path.Base(path.Dir(req.URL.Path)) == couchdb.EscapeCouchdbName(consts.Files) {
					return nil, errors.New("injected copy failure")
				}
				if req.Method == http.MethodPut && path.Base(req.URL.Path) == inst.ID() {
					body, err := io.ReadAll(req.Body)
					if err != nil {
						return nil, err
					}
					req.Body = io.NopCloser(bytes.NewReader(body))
					var doc instance.Instance
					if err := json.Unmarshal(body, &doc); err != nil {
						return nil, err
					}
					if !doc.Blocked {
						return nil, errors.New("injected restoration failure")
					}
				}
				return transport.RoundTrip(req)
			})
			cfg.CouchDB.Client = &client

			_, err := Migrate(inst, Options{To: config.SchemeS3})
			require.ErrorContains(t, err, "restore instance block state")
			assert.ErrorContains(t, err, "injected restoration failure")
			if failCopy {
				assert.ErrorContains(t, err, "injected copy failure")
			}
			reloaded, err := instance.Get(inst.Domain)
			require.NoError(t, err)
			assert.True(t, reloaded.Blocked)
			assert.Equal(t, instance.BlockedMoving.Code, reloaded.BlockingReason)
			var failureLogged bool
			for _, entry := range hook.AllEntries() {
				if entry.Data["nspace"] == "storagemigration" {
					assert.NotContains(t, entry.Message, "Migration completed")
					if strings.HasPrefix(entry.Message, "Migration failed") {
						failureLogged = true
						assert.Equal(t, logrus.ErrorLevel, entry.Level)
						assert.Contains(t, entry.Message, "injected restoration failure")
					}
				}
			}
			assert.True(t, failureLogged)
		})
	}
}

func TestMigrateFlipsSchemeAfterVerify(t *testing.T) {
	inst := setupMigrateInstance(t)

	rep, err := Migrate(inst, Options{To: config.SchemeS3})
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

func TestMigrateDryRunIsReadOnly(t *testing.T) {
	inst := setupMigrateInstance(t)

	file, err := inst.VFS().FileByPath("/migrate-file1.txt")
	require.NoError(t, err)
	versionPayload := []byte("an older revision of file 1")
	sum := md5.Sum(versionPayload)
	version := &vfs.Version{
		DocID:    file.DocID + "/" + utils.RandomString(16),
		ByteSize: int64(len(versionPayload)),
		MD5Sum:   sum[:],
	}
	version.Rels.File.Data.ID = file.DocID
	require.NoError(t, inst.VFS().ImportFileVersion(version, io.NopCloser(bytes.NewReader(versionPayload))))

	inst.Blocked = true
	inst.BlockingReason = instance.BlockedLoginFailed.Code
	require.NoError(t, instance.Update(inst))
	revBefore := inst.Rev()
	seqsBefore, err := storageSequences(inst)
	require.NoError(t, err)

	mutex := config.Lock().ReadWrite(inst, "vfs")
	require.NoError(t, mutex.Lock())
	defer mutex.Unlock()

	for _, flagOnly := range []bool{false, true} {
		for range 2 {
			rep, err := Migrate(inst, Options{To: config.SchemeS3, DryRun: true, FlagOnly: flagOnly, Force: flagOnly})
			require.NoError(t, err)
			assert.Equal(t, &Report{
				Files:        2,
				Versions:     1,
				Bytes:        int64(len("hello from migrate file 1") + len("hello from migrate file 2, a bit longer") + len(versionPayload)),
				AvatarCopied: true,
			}, rep)
			assert.Empty(t, inst.FsScheme)
			assert.True(t, inst.Blocked)
			assert.Equal(t, instance.BlockedLoginFailed.Code, inst.BlockingReason)
			assert.Equal(t, revBefore, inst.Rev())
		}
	}

	reloaded, err := instance.Get(inst.Domain)
	require.NoError(t, err)
	assert.Equal(t, revBefore, reloaded.Rev())
	seqsAfter, err := storageSequences(inst)
	require.NoError(t, err)
	assert.Equal(t, seqsBefore, seqsAfter)

	storage := config.GetS3Storage(config.S3StorageFiles)
	for obj := range storage.Client.ListObjects(context.Background(), storage.Bucket, minio.ListObjectsOptions{
		Prefix: storage.Prefix + inst.DBPrefix() + "/", Recursive: true,
	}) {
		require.NoError(t, obj.Err)
		t.Errorf("dry-run created target object %s", obj.Key)
	}
}

func TestMigrateDryRunDoesNotCreateSwiftContainer(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	setup := testutils.NewSetup(t, t.Name())
	setup.SetupSwiftTest()
	inst := setup.GetTestInstance()
	revBefore := inst.Rev()

	rep, err := Migrate(inst, Options{To: config.SchemeSwift, DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, &Report{}, rep)
	assert.Empty(t, inst.FsScheme)
	assert.False(t, inst.Blocked)
	assert.Equal(t, revBefore, inst.Rev())

	_, _, err = config.GetSwiftConnection().Container(context.Background(), swiftContainerName(t, inst))
	assert.ErrorIs(t, err, swiftv2.ContainerNotFound)
}

func TestMigrateFlagOnlyRequiresForce(t *testing.T) {
	inst := setupMigrateInstance(t)

	_, err := Migrate(inst, Options{To: config.SchemeS3, FlagOnly: true})
	require.Error(t, err)
	assert.Equal(t, "", inst.FsScheme)
}

func TestMigrateFlagOnlyFlipsWhenTargetPopulated(t *testing.T) {
	inst := setupMigrateInstance(t)

	_, err := Migrate(inst, Options{To: config.SchemeS3})
	require.NoError(t, err)
	require.Equal(t, config.SchemeS3, inst.FsScheme)

	inst.FsScheme = ""

	rep, err := Migrate(inst, Options{To: config.SchemeS3, FlagOnly: true, Force: true})
	require.NoError(t, err)
	require.NotNil(t, rep)
	assert.Equal(t, config.SchemeS3, inst.FsScheme)
	assert.Equal(t, 2, rep.Files)
	assert.True(t, rep.AvatarCopied)
}

func TestMigrateFlagOnlyFailsWhenTargetEmpty(t *testing.T) {
	inst := setupMigrateInstance(t)

	_, err := Migrate(inst, Options{To: config.SchemeS3, FlagOnly: true, Force: true})
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
	require.NoError(t, config.InitS3Connection(config.Fs{URL: mf.FsURL(), S3: config.FsS3{Buckets: map[string]config.FsS3Bucket{"default": {Name: "migration-storage"}}}}))

	createInstanceFile(t, inst, "purge-file1.txt", []byte("hello from purge file 1"))
	createInstanceFile(t, inst, "purge-file2.txt", []byte("hello from purge file 2, a bit longer"))

	// Step 1: migrate mem -> swift for real, so the swift container backing
	// this instance is genuinely populated.
	_, err := Migrate(inst, Options{To: config.SchemeSwift})
	require.NoError(t, err)
	require.Equal(t, config.SchemeSwift, inst.FsScheme)

	containerName := swiftContainerName(t, inst)

	// Sanity check: the container really exists before the purge.
	_, _, err = config.GetSwiftConnection().Container(context.Background(), containerName)
	require.NoError(t, err, "the swift container must exist after the first migration")

	// Step 2: migrate swift -> S3 with PurgeSource, exercising the swift
	// source purge implementation.
	_, err = Migrate(inst, Options{To: config.SchemeS3, PurgeSource: true})
	require.NoError(t, err)
	assert.Equal(t, config.SchemeS3, inst.FsScheme)

	// The swift container must be gone now: purgeSource must have actually
	// deleted it, not returned a "not implemented" error after a
	// successful (and now unrevertable) flip.
	_, _, err = config.GetSwiftConnection().Container(context.Background(), containerName)
	assert.True(t, errors.Is(err, swiftv2.ContainerNotFound), "expected the swift container to be gone after purge, got err=%v", err)

	// Exercise the S3 source under the same VFS lock by copying back to Swift.
	_, err = Migrate(inst, Options{To: config.SchemeSwift})
	require.NoError(t, err)
	assert.Equal(t, config.SchemeSwift, inst.FsScheme)
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
	require.NoError(t, config.InitS3Connection(config.Fs{URL: mf.FsURL(), S3: config.FsS3{Buckets: map[string]config.FsS3Bucket{"default": {Name: "migration-storage"}}}}))

	createInstanceFile(t, inst, "purge-only-file1.txt", []byte("hello from purge-only file 1"))
	createInstanceFile(t, inst, "purge-only-file2.txt", []byte("hello from purge-only file 2, a bit longer"))

	// Step 1: migrate mem -> swift for real, so the swift container backing
	// this instance is genuinely populated.
	_, err := Migrate(inst, Options{To: config.SchemeSwift})
	require.NoError(t, err)
	require.Equal(t, config.SchemeSwift, inst.FsScheme)

	containerName := swiftContainerName(t, inst)

	// Step 2: migrate swift -> S3 WITHOUT PurgeSource, so the instance ends
	// on S3 while the swift source is deliberately retained, exactly as
	// docs/s3.md's rollback window describes.
	_, err = Migrate(inst, Options{To: config.SchemeS3})
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
	rep, err := Migrate(inst, Options{To: config.SchemeS3, PurgeSource: true})
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

	_, err := Migrate(inst, Options{To: config.SchemeS3})
	require.NoError(t, err)
	require.Equal(t, config.SchemeS3, inst.FsScheme)

	_, err = Migrate(inst, Options{To: config.SchemeS3})
	require.Error(t, err)
	assert.Equal(t, config.SchemeS3, inst.FsScheme)
}

func TestS3MigrationUsesConfiguredBucketAndScopesPurge(t *testing.T) {
	config.UseTestFile(t)
	mf := testutils.StartMinio(t)
	client := mf.Client(t)
	ctx := context.Background()
	bucket := "migration-files"
	require.NoError(t, client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}))
	require.NoError(t, client.MakeBucket(ctx, "migration-other", minio.MakeBucketOptions{}))
	disabled := false
	require.NoError(t, config.InitS3Connection(config.Fs{
		URL: mf.FsURL(),
		S3: config.FsS3{AutoCreateBuckets: &disabled, Buckets: map[string]config.FsS3Bucket{
			"default":             {Name: "migration-other"},
			config.S3StorageFiles: {Name: bucket},
		}},
	}))

	inst := &instance.Instance{Domain: "alice"}
	dst, avatar, err := buildTarget(inst, config.SchemeS3)
	require.NoError(t, err)
	docID, internalID := "0123456789012345678901234567890a", "abcdef0123456789"
	require.NoError(t, dst.(contentWriter).WriteContentAt(docID, internalID, bytes.NewReader([]byte("file")), 4))
	w, err := avatar.CreateAvatar("image/png")
	require.NoError(t, err)
	_, err = w.Write([]byte("avatar"))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	fileKey := vfss3.MakeObjectKey("files/alice/", docID, internalID)
	for _, key := range []string{fileKey, "files/alice/avatar"} {
		_, err := client.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
		require.NoError(t, err)
	}
	preserved := []string{"files/bob/file", "files/alice-other/file", "assets/alice/icon", "exports/alice/archive"}
	for _, key := range preserved {
		_, err := client.PutObject(ctx, bucket, key, bytes.NewReader([]byte("keep")), 4, minio.PutObjectOptions{})
		require.NoError(t, err)
	}
	_, err = client.PutObject(ctx, "migration-other", fileKey, bytes.NewReader([]byte("keep")), 4, minio.PutObjectOptions{})
	require.NoError(t, err)

	require.NoError(t, purgeSource(inst, config.SchemeS3))
	var remaining []string
	for obj := range client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
		require.NoError(t, obj.Err)
		remaining = append(remaining, obj.Key)
	}
	assert.ElementsMatch(t, preserved, remaining)
	_, err = client.StatObject(ctx, "migration-other", fileKey, minio.StatObjectOptions{})
	require.NoError(t, err)
}
