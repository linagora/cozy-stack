package vfsswift_test

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"testing"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/model/vfs/vfsswift"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/ncw/swift/v2/swifttest"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

// writeContentAtPrefixer is a minimal vfs.Prefixer implementation, local to
// this test, so it can live in an external test package (package
// vfsswift_test) without importing anything from the internal vfsswift test
// harness.
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

// writeContentAtDisk is a minimal vfs.DiskThresholder, unused by
// WriteContentAt itself but required by vfsswift.NewV3's signature.
type writeContentAtDisk struct{}

func (writeContentAtDisk) DiskQuota() int64 { return 0 }

// TestWriteContentAtPutsBytesWithoutIndex verifies that WriteContentAt is a
// pure object-storage primitive: it puts bytes at the object key derived
// from (docID, internalID) and does not touch CouchDB (no document is
// created for the write itself).
func TestWriteContentAtPutsBytesWithoutIndex(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	db := &writeContentAtPrefixer{
		cluster: 0,
		domain:  "io.cozy.vfsswift.writecontentat.test",
		prefix:  "io.cozy.vfsswift.writecontentat.test",
		context: "cozy_beta",
	}
	index := vfs.NewCouchdbIndexer(db)

	swiftSrv, err := swifttest.NewSwiftServer("localhost")
	require.NoError(t, err, "failed to create swift server")
	t.Cleanup(func() { swiftSrv.Close() })

	require.NoError(t, config.InitSwiftConnection(config.Fs{
		URL: &url.URL{
			Scheme:   "swift",
			Host:     "localhost",
			RawQuery: "UserName=swifttest&Password=swifttest&AuthURL=" + url.QueryEscape(swiftSrv.AuthURL),
		},
	}))

	mutex := config.Lock().ReadWrite(db, "vfs-swiftv3-writecontentat-test")
	sfs, err := vfsswift.NewV3(db, index, &writeContentAtDisk{}, mutex)
	require.NoError(t, err)

	require.NoError(t, couchdb.ResetDB(db, consts.Files))
	t.Cleanup(func() { _ = couchdb.DeleteDB(db, consts.Files) })

	g, _ := errgroup.WithContext(context.Background())
	couchdb.DefineIndexes(g, db, couchdb.IndexesByDoctype(consts.Files))
	couchdb.DefineViews(g, db, couchdb.ViewsByDoctype(consts.Files))
	require.NoError(t, g.Wait())

	require.NoError(t, sfs.InitFs())

	w := sfs.(interface {
		WriteContentAt(docID, internalID string, content io.Reader, size int64) error
	})

	docID := "0123456789012345678901234567890a" // 32 chars
	internalID := "abcdef0123456789"            // 16 chars
	payload := []byte("hello swift migration")

	require.NoError(t, w.WriteContentAt(docID, internalID, bytes.NewReader(payload), int64(len(payload))))

	cn := sfs.(interface{ ContainerNames() map[string]string })
	container := cn.ContainerNames()["container"]
	objName := vfsswift.MakeObjectNameV3(docID, internalID)

	conn := config.GetSwiftConnection()
	obj, _, err := conn.ObjectOpen(context.Background(), container, objName, false, nil)
	require.NoError(t, err)
	defer obj.Close()

	got, err := io.ReadAll(obj)
	require.NoError(t, err)
	require.Equal(t, payload, got)
}
