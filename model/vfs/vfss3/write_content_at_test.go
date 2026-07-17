package vfss3_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/model/vfs/vfss3"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"
)

// writeContentAtPrefixer is a minimal vfs.Prefixer implementation, local to
// this test, so it can live in an external test package (package
// vfss3_test) without importing anything from the internal vfss3 test
// harness. It also exposes GetOrgID so vfss3.New can build the bucket name.
type writeContentAtPrefixer struct {
	cluster int
	domain  string
	prefix  string
	context string
}

func (p *writeContentAtPrefixer) DBCluster() int         { return p.cluster }
func (p *writeContentAtPrefixer) DomainName() string     { return p.domain }
func (p *writeContentAtPrefixer) DBPrefix() string       { return p.prefix }
func (p *writeContentAtPrefixer) GetContextName() string { return p.context }
func (p *writeContentAtPrefixer) GetOrgID() string       { return "wcatestorg" }

// writeContentAtDisk is a minimal vfs.DiskThresholder, unused by
// WriteContentAt itself but required by vfss3.New's signature.
type writeContentAtDisk struct{}

func (writeContentAtDisk) DiskQuota() int64 { return 0 }

// TestWriteContentAtPutsBytesWithoutIndex verifies that WriteContentAt is a
// pure object-storage primitive: it puts bytes at the object key derived
// from (docID, internalID) and does not touch CouchDB at all (no
// ResetDB/DefineIndexes/InitFs is performed in this test).
func TestWriteContentAtPutsBytesWithoutIndex(t *testing.T) {
	config.UseTestFile(t)

	mf := testutils.StartMinio(t)

	db := &writeContentAtPrefixer{
		cluster: 0,
		domain:  "io.cozy.vfss3.writecontentat.test",
		prefix:  "io.cozy.vfss3.writecontentat.test",
		context: "cozy_beta",
	}
	index := vfs.NewCouchdbIndexer(db)

	require.NoError(t, config.InitS3Connection(config.Fs{URL: mf.FsURL("test")}))

	mutex := config.Lock().ReadWrite(db, "vfs-s3-writecontentat-test")
	sfs, err := vfss3.New(db, index, &writeContentAtDisk{}, mutex)
	require.NoError(t, err)

	// WriteContentAt never creates its own bucket (that's InitFs's job, which
	// we deliberately skip here since it would also touch CouchDB through
	// Indexer.InitIndex). Create the bucket directly against the raw client.
	bucket := vfss3.BucketName(db.GetOrgID(), config.GetS3BucketPrefix())
	client := mf.Client(t)
	require.NoError(t, client.MakeBucket(context.Background(), bucket, minio.MakeBucketOptions{}))

	w := sfs.(interface {
		WriteContentAt(docID, internalID string, content io.Reader, size int64) error
	})

	docID := "0123456789012345678901234567890a" // 32 chars
	internalID := "abcdef0123456789"            // 16 chars
	payload := []byte("hello s3 migration")

	require.NoError(t, w.WriteContentAt(docID, internalID, bytes.NewReader(payload), int64(len(payload))))

	objKey := vfss3.MakeObjectKey(db.DBPrefix()+"/", docID, internalID)
	obj, err := client.GetObject(context.Background(), bucket, objKey, minio.GetObjectOptions{})
	require.NoError(t, err)
	got, err := io.ReadAll(obj)
	require.NoError(t, err)
	require.Equal(t, payload, got)
}
