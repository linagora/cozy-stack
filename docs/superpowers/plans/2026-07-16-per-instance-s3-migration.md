# Per-instance storage backend & Swift→S3 migration — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an operator migrate a single live instance's file storage from Swift to the S3 backend, one instance at a time, via `cozy-stack instances migrate-storage <domain> --to s3`, with rollback.

**Architecture:** Add a per-instance `FsScheme` field that overrides the global `fs.url` scheme when building the instance's VFS. Run both the Swift and S3 connections during the transition. A server-side migration copies object-storage **content** (files, versions, avatar) source→target **without touching the shared CouchDB index**, inside an HTTP-blocked window, then flips `FsScheme`. Rollback reuses the same engine (`--to swift`) or an instant `--flag-only` flip while the source is retained.

**Tech Stack:** Go, CouchDB, minio-go/v7 (S3), ncw/swift (Swift), cobra (CLI), echo (admin API), MinIO testcontainer for tests.

## Global Constraints

- Backend key layout is identical across Swift-v3 and S3: object name = `docID[:22] + "/" + docID[22:27] + "/" + docID[27:] + "/" + internalID` (fallback `docID + "/" + internalID` when `len(docID)!=32 || len(internalID)!=16`). S3 prefixes it with `keyPrefix = DBPrefix() + "/"`. Use the exported builders `vfsswift.MakeObjectNameV3` and `vfss3.MakeObjectKey` — never re-derive by hand.
- The migration MUST NOT create or modify any CouchDB document (`io.cozy.files`, `io.cozy.files.versions`). Source and target VFS share the same `vfs.NewCouchdbIndexer(i)`. Only object bytes move.
- `FsScheme == ""` means "use global default" and MUST preserve today's behavior for every existing instance (zero data migration of the instance registry).
- No cosign trailers in commits (project convention).
- Only Swift-v3 (`SwiftLayout == 2`) is a supported migration source; reject v1/v2.
- Migrated-but-unverified state is never persisted: `FsScheme` flips only after verification passes.
- **S3 tests require a live MinIO container via Docker.** There is NO per-package `newTestS3VFS` fixture. The real, existing harness is `testutils.StartMinio(t) *MinioFixture` (`tests/testutils/minio_utils.go`) exposing `Client(t) *minio.Client` and `FsURL(bucketPrefix string) *url.URL`. Build an S3 VFS exactly like `makeS3FS` in `model/vfs/vfs_test.go:916`: `mf := testutils.StartMinio(t); config.InitS3Connection(config.Fs{URL: mf.FsURL("test")}); s3fs, _ := vfss3.New(db, index, &diskImpl{}, mutex)`. Any test in this plan that references a `newTestS3VFS`/`newTestAvatarS3`/`minioGetOpts` helper means "build the S3 VFS/avatarer with this StartMinio pattern" — reuse it, do not invent a new container fixture. Tests that must reach unexported `*s3VFS` fields go in `package vfss3` (or an external `vfss3_test` package if importing `tests/testutils` would cycle — the implementer verifies and picks).

---

## File structure

- `model/instance/instance.go` — add `FsScheme` field + `StorageScheme()` accessor; refactor the triplicated backend `switch` in `MakeVFS`/`AvatarFS`/`ThumbsFS` into one `buildVFS(kind)` helper reading `StorageScheme()`.
- `pkg/config/config/config.go` — parse optional `fs.migration_target` URL into `config.Fs`; add `MigrationTargetURL()` / `HasS3Target()`.
- `model/stack/main.go` — after the default connections, also init the S3 connection from the migration target when the global scheme is not S3.
- `model/vfs/vfs.go` — add `OpenAvatar() (io.ReadCloser, string, error)` to the `Avatarer` interface.
- `model/vfs/vfsswift/avatar_v3.go`, `model/vfs/vfss3/avatar.go`, `model/vfs/vfsafero/avatar.go` — implement `OpenAvatar`.
- `model/vfs/vfss3/impl.go` — add exported index-free `WriteContentAt(docID, internalID string, r io.Reader, size int64) error`.
- `model/instance/storagemigration/migration.go` (new package) — the engine: enumerate, copy, verify, orchestrate (`Migrate`, `Report`).
- `client/instances.go` — `AdminClient.MigrateStorage(opts)`.
- `web/instances/instances.go` — `migrateStorageHandler` + route.
- `cmd/instances.go` — `migrateStorageCmd` + flags.
- Tests colocated: `model/instance/storagemigration/migration_test.go`, plus small unit tests next to touched files.

---

## Task 1: Per-instance `FsScheme` field and effective-scheme accessor

**Files:**
- Modify: `model/instance/instance.go` (struct ~50-89; `MakeVFS` 250-276; `AvatarFS` 279-304; `ThumbsFS` 306-333)
- Test: `model/instance/instance_storage_scheme_test.go` (create)

**Interfaces:**
- Produces: `func (i *Instance) StorageScheme() string` — returns `i.FsScheme` if non-empty else `config.FsURL().Scheme`.
- Produces: struct field `FsScheme string json:"fs_scheme,omitempty"`.

- [ ] **Step 1: Write the failing test**

Create `model/instance/instance_storage_scheme_test.go`:
```go
package instance

import (
	"testing"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/stretchr/testify/assert"
)

func TestStorageSchemeFallsBackToGlobal(t *testing.T) {
	config.UseTestFile(t)
	i := &Instance{}
	assert.Equal(t, config.FsURL().Scheme, i.StorageScheme())
}

func TestStorageSchemeOverridesGlobal(t *testing.T) {
	config.UseTestFile(t)
	i := &Instance{FsScheme: config.SchemeS3}
	assert.Equal(t, config.SchemeS3, i.StorageScheme())
}
```

- [ ] **Step 2: Run it, verify it fails**

Run: `go test ./model/instance/ -run TestStorageScheme -v`
Expected: FAIL — `i.StorageScheme undefined` / `i.FsScheme undefined`.

- [ ] **Step 3: Add the field**

In the `Instance` struct (`model/instance/instance.go`), directly under `SwiftLayout int json:"swift_cluster,omitempty"`:
```go
	// FsScheme, when non-empty, overrides the global fs.url scheme for this
	// instance. Used to migrate a single instance to another storage backend
	// (e.g. "s3") without changing the stack-wide default. Empty = global default.
	FsScheme string `json:"fs_scheme,omitempty"`
```

- [ ] **Step 4: Add the accessor**

Near `DBPrefix()` (`model/instance/instance.go:183`):
```go
// StorageScheme returns the storage backend scheme effective for this instance:
// the per-instance FsScheme override when set, otherwise the global fs.url scheme.
func (i *Instance) StorageScheme() string {
	if i.FsScheme != "" {
		return i.FsScheme
	}
	return config.FsURL().Scheme
}
```

- [ ] **Step 5: Run test, verify pass**

Run: `go test ./model/instance/ -run TestStorageScheme -v`
Expected: PASS.

- [ ] **Step 6: Refactor the triplicated switch to use `StorageScheme()`**

In `MakeVFS`, `AvatarFS`, and `ThumbsFS`, replace every `fsURL := config.FsURL()` + `switch fsURL.Scheme` with `switch i.StorageScheme()`. Keep `config.FsURL()` only where the afero branch still needs the URL/path (afero uses `fsURL` and `i.DirName()`), fetching it inside that branch:
```go
	switch i.StorageScheme() {
	case config.SchemeFile, config.SchemeMem:
		i.vfs, err = vfsafero.New(i, index, disk, mutex, config.FsURL(), i.DirName())
	case config.SchemeSwift, config.SchemeSwiftSecure:
		switch i.SwiftLayout {
		case 2:
			i.vfs, err = vfsswift.NewV3(i, index, disk, mutex)
		default:
			err = ErrInvalidSwiftLayout
		}
	case config.SchemeS3:
		i.vfs, err = vfss3.New(i, index, disk, mutex)
	default:
		err = fmt.Errorf("instance: unknown storage provider %s", i.StorageScheme())
	}
```
Apply the equivalent change to `AvatarFS` (which builds `vfsafero.NewAvatarFs`/`vfsswift.NewAvatarFsV3`/`vfss3.NewAvatarFs`) and `ThumbsFS`.

- [ ] **Step 7: Build + full instance package tests**

Run: `go build ./... && go test ./model/instance/ -run 'TestStorageScheme|TestMakeVFS' -v`
Expected: build OK, tests PASS.

- [ ] **Step 8: Commit**

```bash
git add model/instance/instance.go model/instance/instance_storage_scheme_test.go
git commit -m "feat(instance): add per-instance FsScheme override for storage backend"
```

---

## Task 2: Initialize the S3 connection from a migration target

**Files:**
- Modify: `pkg/config/config/config.go` (`Fs` struct 225-235; `UseViper` ~830/1156; add accessors near `FsURL` 497)
- Modify: `model/stack/main.go` (83-91)
- Test: `pkg/config/config/s3_target_test.go` (create)

**Interfaces:**
- Consumes: `config.InitS3Connection(fs Fs) error` (`pkg/config/config/s3.go:23`), which populates the S3 globals when `fs.URL.Scheme == SchemeS3`.
- Produces: `func MigrationTargetURL() *url.URL`; `func HasS3Target() bool`.

- [ ] **Step 1: Write the failing test**

Create `pkg/config/config/s3_target_test.go`:
```go
package config

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrationTargetInitsS3WhenGlobalIsSwift(t *testing.T) {
	swiftURL, _ := url.Parse("swift://openstack/")
	s3URL, _ := url.Parse("s3://key:secret@localhost:9000/?bucket_prefix=cozy&use_ssl=false")
	config = &Config{Fs: Fs{URL: swiftURL, MigrationTarget: s3URL}}

	require.True(t, HasS3Target())
	// Init the S3 globals from the target even though the global scheme is swift.
	require.NoError(t, InitS3Connection(Fs{URL: MigrationTargetURL()}))
	assert.NotNil(t, GetS3Client())
	assert.Equal(t, "cozy", GetS3BucketPrefix())
}
```

- [ ] **Step 2: Run it, verify it fails**

Run: `go test ./pkg/config/config/ -run TestMigrationTargetInitsS3 -v`
Expected: FAIL — `Fs has no field MigrationTarget` / `HasS3Target undefined` / `MigrationTargetURL undefined`.

- [ ] **Step 3: Add the config field + accessors**

In the `Fs` struct (`config.go:225`) add:
```go
	// MigrationTarget, when set, is an alternate storage URL (e.g. s3://...)
	// whose connection is initialized alongside the default one, so instances
	// can be migrated to it while the global scheme stays unchanged.
	MigrationTarget *url.URL
```
Near `FsURL()` (`config.go:497`):
```go
// MigrationTargetURL returns the configured storage migration target URL, or nil.
func MigrationTargetURL() *url.URL {
	return config.Fs.MigrationTarget
}

// HasS3Target reports whether an S3 storage migration target is configured.
func HasS3Target() bool {
	u := config.Fs.MigrationTarget
	return u != nil && u.Scheme == SchemeS3
}
```

- [ ] **Step 4: Parse `fs.migration_target` in UseViper**

In `UseViper` where `Fs{...}` is built (`config.go:1156`), parse the optional key and set the field:
```go
	var migrationTarget *url.URL
	if raw := v.GetString("fs.migration_target"); raw != "" {
		migrationTarget, err = url.Parse(raw)
		if err != nil {
			return err
		}
	}
```
and add `MigrationTarget: migrationTarget,` to the `Fs{...}` literal.

- [ ] **Step 5: Run test, verify pass**

Run: `go test ./pkg/config/config/ -run TestMigrationTargetInitsS3 -v`
Expected: PASS.

- [ ] **Step 6: Wire the target init at stack startup**

In `model/stack/main.go` after the existing `InitDefaultS3Connection()` block (line ~88-91):
```go
	// When a storage migration target is configured (e.g. migrating instances
	// to S3 while the global default is still Swift), init that connection too.
	if config.HasS3Target() {
		if err := config.InitS3Connection(config.Fs{URL: config.MigrationTargetURL()}); err != nil {
			return nil, nil, fmt.Errorf("failed to init the S3 migration target connection: %w", err)
		}
	}
```

- [ ] **Step 7: Build**

Run: `go build ./...`
Expected: OK.

- [ ] **Step 8: Commit**

```bash
git add pkg/config/config/config.go pkg/config/config/s3_target_test.go model/stack/main.go
git commit -m "feat(config): init S3 connection from an optional fs.migration_target"
```

---

## Task 3: Index-free content writer on the S3 backend

**Files:**
- Modify: `model/vfs/vfss3/impl.go` (near `ImportFileVersion` 591-626)
- Test: `model/vfs/vfss3/write_content_at_test.go` (create; reuses the package's MinIO testcontainer harness)

**Interfaces:**
- Produces: `func (sfs *s3VFS) WriteContentAt(docID, internalID string, content io.Reader, size int64) error` — puts bytes at `MakeObjectKey(sfs.keyPrefix, docID, internalID)`, creating no CouchDB doc.

- [ ] **Step 1: Write the failing test**

Create `model/vfs/vfss3/write_content_at_test.go` (mirror the setup already used in the vfss3 suite to get an `*s3VFS` against MinIO; see the existing test harness in this package for the exact fixture helper name and reuse it):
```go
package vfss3

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteContentAtPutsBytesWithoutIndex(t *testing.T) {
	sfs := newTestS3VFS(t) // existing fixture in this package's tests
	s3 := sfs.(*s3VFS)

	docID := "0123456789012345678901234567890a" // 32 chars
	internalID := "abcdef0123456789"             // 16 chars
	payload := []byte("hello s3 migration")

	require.NoError(t, s3.WriteContentAt(docID, internalID, bytes.NewReader(payload), int64(len(payload))))

	objKey := MakeObjectKey(s3.keyPrefix, docID, internalID)
	obj, err := s3.client.GetObject(s3.ctx, s3.bucket, objKey, minioGetOpts())
	require.NoError(t, err)
	got, err := io.ReadAll(obj)
	require.NoError(t, err)
	require.Equal(t, payload, got)
}
```
(If the package lacks a `newTestS3VFS`/`minioGetOpts` helper, add minimal ones in the test file based on the container fixture already present in `model/vfs` S3 tests.)

- [ ] **Step 2: Run it, verify it fails**

Run: `go test ./model/vfs/vfss3/ -run TestWriteContentAt -v`
Expected: FAIL — `s3.WriteContentAt undefined`.

- [ ] **Step 3: Implement**

Add to `model/vfs/vfss3/impl.go`:
```go
// WriteContentAt streams content into the object backing the (docID, internalID)
// key, creating NO CouchDB document. It is used by storage migration, which
// preserves the shared index and only moves object bytes. size may be -1 when
// unknown (falls back to multipart).
func (sfs *s3VFS) WriteContentAt(docID, internalID string, content io.Reader, size int64) error {
	objKey := MakeObjectKey(sfs.keyPrefix, docID, internalID)
	_, err := sfs.client.PutObject(sfs.ctx, sfs.bucket, objKey, content, size, minio.PutObjectOptions{
		ContentType:    "application/octet-stream",
		SendContentMd5: true,
	})
	return err
}
```

- [ ] **Step 4: Run test, verify pass**

Run: `go test ./model/vfs/vfss3/ -run TestWriteContentAt -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add model/vfs/vfss3/impl.go model/vfs/vfss3/write_content_at_test.go
git commit -m "feat(vfss3): add index-free WriteContentAt for storage migration"
```

---

## Task 4: `OpenAvatar` on the Avatarer interface

**Files:**
- Modify: `model/vfs/vfs.go` (`Avatarer` interface 266-273)
- Modify: `model/vfs/vfsswift/avatar_v3.go`, `model/vfs/vfss3/avatar.go`, `model/vfs/vfsafero/avatar.go`
- Test: `model/vfs/vfss3/avatar_test.go` (add a case; or create)

**Interfaces:**
- Produces: `OpenAvatar() (io.ReadCloser, string, error)` on `vfs.Avatarer` — returns the stored avatar content reader and its content-type; `os.ErrNotExist` when no avatar exists.

- [ ] **Step 1: Write the failing test (S3 impl)**

In `model/vfs/vfss3/avatar_test.go`:
```go
func TestOpenAvatarRoundTrip(t *testing.T) {
	av := newTestAvatarS3(t) // fixture returning the s3 Avatarer
	require.NoError(t, av.CreateAvatar("image/png"). /* write */ , /* ... */)
	// (use the package's existing CreateAvatar test flow to store bytes first)

	r, ctype, err := av.OpenAvatar()
	require.NoError(t, err)
	defer r.Close()
	assert.Equal(t, "image/png", ctype)
	b, _ := io.ReadAll(r)
	assert.NotEmpty(t, b)
}
```
(Base the write half on the existing `CreateAvatar` test in this package; if none exists, store via `CreateAvatar` returning an `io.WriteCloser` per its current signature.)

- [ ] **Step 2: Run it, verify it fails**

Run: `go test ./model/vfs/vfss3/ -run TestOpenAvatar -v`
Expected: FAIL — `OpenAvatar undefined`.

- [ ] **Step 3: Extend the interface**

In `model/vfs/vfs.go` `Avatarer`:
```go
	// OpenAvatar returns a reader over the stored avatar content and its
	// content-type, or os.ErrNotExist if no avatar is stored.
	OpenAvatar() (io.ReadCloser, string, error)
```

- [ ] **Step 4: Implement for S3** (`model/vfs/vfss3/avatar.go`)
```go
func (a *avatarS3) OpenAvatar() (io.ReadCloser, string, error) {
	obj, err := a.client.GetObject(a.ctx, a.bucket, a.avatarKey(), minio.GetObjectOptions{})
	if err != nil {
		return nil, "", err
	}
	info, err := obj.Stat()
	if err != nil {
		if isNoSuchKey(err) { // existing helper used by OpenFile in this pkg
			return nil, "", os.ErrNotExist
		}
		return nil, "", err
	}
	return obj, info.ContentType, nil
}
```

- [ ] **Step 5: Implement for Swift v3** (`model/vfs/vfsswift/avatar_v3.go`)
```go
func (a *avatarV3) OpenAvatar() (io.ReadCloser, string, error) {
	f, headers, err := a.c.ObjectOpen(a.ctx, a.container, "avatar", false, nil)
	if err != nil {
		if errors.Is(err, swift.ObjectNotFound) {
			return nil, "", os.ErrNotExist
		}
		return nil, "", err
	}
	return f, headers["Content-Type"], nil
}
```

- [ ] **Step 6: Implement for afero** (`model/vfs/vfsafero/avatar.go`)
```go
func (a *avatarFS) OpenAvatar() (io.ReadCloser, string, error) {
	f, err := a.fs.Open(AvatarFilename)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", os.ErrNotExist
		}
		return nil, "", err
	}
	// content-type is not stored on disk; sniff from the name/content.
	return f, "application/octet-stream", nil
}
```

- [ ] **Step 7: Run tests + build**

Run: `go build ./... && go test ./model/vfs/vfss3/ -run TestOpenAvatar -v`
Expected: build OK (all three impls satisfy the interface), test PASS.

- [ ] **Step 8: Commit**

```bash
git add model/vfs/vfs.go model/vfs/vfsswift/avatar_v3.go model/vfs/vfss3/avatar.go model/vfs/vfsafero/avatar.go model/vfs/vfss3/avatar_test.go
git commit -m "feat(vfs): add OpenAvatar to the Avatarer interface"
```

---

## Task 5: Migration engine — enumerate and copy content

**Files:**
- Create: `model/instance/storagemigration/migration.go`
- Test: `model/instance/storagemigration/migration_test.go`

**Interfaces:**
- Consumes: `couchdb.ForeachDocs(db prefixer.Prefixer, doctype string, fn func(id string, doc json.RawMessage) error) error`; `vfs.VFS.OpenFile(*vfs.FileDoc)`, `vfs.VFS.OpenFileVersion(*vfs.FileDoc, *vfs.Version)`, `vfs.VFS.FileByID(string)`; `vfsswift.NewV3`, `vfss3.New`; the `contentWriter` interface (Task 3's `WriteContentAt`); `OpenAvatar` (Task 4).
- Produces: `type Report struct { Files, Versions int; Bytes int64; AvatarCopied bool }`; `func CopyContent(inst *instance.Instance, src vfs.VFS, dst vfs.VFS, srcAv, dstAv vfs.Avatarer) (*Report, error)`.

- [ ] **Step 1: Write the failing test (afero source → S3 target)**

Create `model/instance/storagemigration/migration_test.go`:
```go
package storagemigration

import (
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopyContentMovesFilesVersionsAndAvatar(t *testing.T) {
	// Fixture: an instance whose global backend is afero (test default),
	// populated with 2 files, 1 extra version, 1 trashed file, and an avatar.
	inst, src, dst, srcAv, dstAv := setupMigrationFixture(t)

	rep, err := CopyContent(inst, src, dst, srcAv, dstAv)
	require.NoError(t, err)
	assert.Equal(t, 3, rep.Files)     // 2 live + 1 trashed
	assert.Equal(t, 1, rep.Versions)
	assert.True(t, rep.AvatarCopied)

	// Every source file's bytes are now readable from the target VFS.
	assertAllContentReadableFrom(t, inst, dst)
	// The CouchDB index is unchanged (same rev on the files DB).
	assertIndexUnchanged(t, inst)
}
```
(`setupMigrationFixture`, `assertAllContentReadableFrom`, `assertIndexUnchanged` are helpers to write in this test file, building the instance with `lifecycle.Create`, writing files via the source VFS `CreateFile`, and building `dst` via `vfss3.New` against the test MinIO container.)

- [ ] **Step 2: Run it, verify it fails**

Run: `go test ./model/instance/storagemigration/ -run TestCopyContent -v`
Expected: FAIL — package/functions undefined.

- [ ] **Step 3: Implement the enumerator + copier**

Create `model/instance/storagemigration/migration.go`:
```go
package storagemigration

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

// contentWriter is implemented by target VFS backends that can write object
// bytes for a (docID, internalID) key without creating a CouchDB document.
type contentWriter interface {
	WriteContentAt(docID, internalID string, content io.Reader, size int64) error
}

// Report summarizes a content copy.
type Report struct {
	Files        int
	Versions     int
	Bytes        int64
	AvatarCopied bool
}

// CopyContent copies all object-storage content (files incl. trashed, versions,
// avatar) for inst from src to dst. It creates/modifies NO CouchDB document.
func CopyContent(inst *instance.Instance, src, dst vfs.VFS, srcAv, dstAv vfs.Avatarer) (*Report, error) {
	writer, ok := dst.(contentWriter)
	if !ok {
		return nil, fmt.Errorf("target backend does not support index-free writes")
	}
	rep := &Report{}

	// Files (including trashed).
	err := couchdb.ForeachDocs(inst, consts.Files, func(_ string, raw json.RawMessage) error {
		var doc vfs.FileDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			return err
		}
		if doc.Type == consts.DirType {
			return nil
		}
		r, err := src.OpenFile(&doc)
		if err != nil {
			return fmt.Errorf("open source file %s: %w", doc.DocID, err)
		}
		defer r.Close()
		if err := writer.WriteContentAt(doc.DocID, doc.InternalID, r, doc.ByteSize); err != nil {
			return fmt.Errorf("write target file %s: %w", doc.DocID, err)
		}
		rep.Files++
		rep.Bytes += doc.ByteSize
		return nil
	})
	if err != nil {
		return rep, err
	}

	// Versions.
	err = couchdb.ForeachDocs(inst, consts.FilesVersions, func(_ string, raw json.RawMessage) error {
		var ver vfs.Version
		if err := json.Unmarshal(raw, &ver); err != nil {
			return err
		}
		fileID, internalID := splitVersionID(ver.DocID)
		fileDoc, err := src.FileByID(fileID)
		if err != nil {
			return fmt.Errorf("file for version %s: %w", ver.DocID, err)
		}
		r, err := src.OpenFileVersion(fileDoc, &ver)
		if err != nil {
			return fmt.Errorf("open source version %s: %w", ver.DocID, err)
		}
		defer r.Close()
		if err := writer.WriteContentAt(fileID, internalID, r, ver.ByteSize); err != nil {
			return fmt.Errorf("write target version %s: %w", ver.DocID, err)
		}
		rep.Versions++
		rep.Bytes += ver.ByteSize
		return nil
	})
	if err != nil {
		return rep, err
	}

	// Avatar (single optional object).
	ar, ctype, err := srcAv.OpenAvatar()
	switch {
	case errors.Is(err, os.ErrNotExist):
		// no avatar, nothing to do
	case err != nil:
		return rep, fmt.Errorf("open source avatar: %w", err)
	default:
		defer ar.Close()
		w, err := dstAv.CreateAvatar(ctype)
		if err != nil {
			return rep, fmt.Errorf("create target avatar: %w", err)
		}
		if _, err := io.Copy(w, ar); err != nil {
			_ = w.Close()
			return rep, fmt.Errorf("copy avatar: %w", err)
		}
		if err := w.Close(); err != nil {
			return rep, fmt.Errorf("finalize target avatar: %w", err)
		}
		rep.AvatarCopied = true
	}

	return rep, nil
}

func splitVersionID(versionDocID string) (fileID, internalID string) {
	for i := 0; i < len(versionDocID); i++ {
		if versionDocID[i] == '/' {
			return versionDocID[:i], versionDocID[i+1:]
		}
	}
	return versionDocID, versionDocID
}
```
Notes for the implementer:
- Confirm the exact `vfs.FileDoc` field for the doc-type discriminator and the trashed flag; `consts.DirType`/`consts.FileType` are the type values. Directories are skipped; trashed files ARE included (they still carry content).
- Confirm `Avatarer.CreateAvatar` returns an `io.WriteCloser` (as used elsewhere in the code that stores avatars). If its signature differs, adapt the write half accordingly.

- [ ] **Step 4: Run test, verify pass**

Run: `go test ./model/instance/storagemigration/ -run TestCopyContent -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add model/instance/storagemigration/migration.go model/instance/storagemigration/migration_test.go
git commit -m "feat(storagemigration): copy files, versions and avatar between backends"
```

---

## Task 5b: Index-free content writer on the Swift backend

Rationale: the rollback design offers a full S3→Swift re-migration (`--to swift`), so Swift must also be a valid copy TARGET. Mirror Task 3 for vfsswift v3.

**Files:**
- Modify: `model/vfs/vfsswift/impl_v3.go` (near `ImportFileVersion`)
- Modify: `model/instance/storagemigration/migration.go` (package doc comment: Swift↔S3 is now accurate)
- Test: `model/vfs/vfsswift/write_content_at_v3_test.go` (external `package vfsswift_test`, MinIO not needed — Swift uses a swift test server; mirror the existing swift test setup in the package/`model/vfs/vfs_test.go` `makeSwiftFS`)

**Interfaces:**
- Produces: `func (sfs *swiftVFSV3) WriteContentAt(docID, internalID string, content io.Reader, size int64) error` — `ObjectCreate` at `MakeObjectNameV3(docID, internalID)` in the instance container, creating no CouchDB document.

- [ ] **Step 1: Write the failing test** in `package vfsswift_test`, building the swift VFS the way `makeSwiftFS` (`model/vfs/vfs_test.go:876`) does; write via the interface assertion `sfs.(interface{ WriteContentAt(...) })`; read back the object via the swift connection at `MakeObjectNameV3(docID, internalID)` and assert bytes. Use 32-char docID + 16-char internalID.

- [ ] **Step 2: Run it, verify RED** — `go test ./model/vfs/vfsswift/ -run TestWriteContentAt -timeout 120s -v` → `WriteContentAt` undefined.

- [ ] **Step 3: Implement** in `model/vfs/vfsswift/impl_v3.go` (receiver name and container/ctx access mirror `ImportFileVersion`/`CreateFile` in this file):
```go
// WriteContentAt streams content into the object backing the (docID, internalID)
// key in this instance's container, creating NO CouchDB document. Used by
// storage migration, which preserves the shared index and only moves bytes.
func (sfs *swiftVFSV3) WriteContentAt(docID, internalID string, content io.Reader, size int64) error {
	objName := MakeObjectNameV3(docID, internalID)
	f, err := sfs.c.ObjectCreate(sfs.ctx, sfs.container, objName, true, "", "application/octet-stream", nil)
	if err != nil {
		return err
	}
	if _, err = io.Copy(f, content); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
```
(Verify the exact receiver type name — `swiftVFSV3`/`swiftVFS` — and field names `c`/`ctx`/`container` against the file; adapt if they differ. `ObjectCreate` with an empty checksum skips server-side hash verification, matching the streaming path already used in this file.)

- [ ] **Step 4: Also update** the `storagemigration/migration.go` package doc comment so it no longer implies only-S3-target (Swift↔S3 both now supported as targets). Rebuild.

- [ ] **Step 5: Run test, verify GREEN**, `go build ./...` clean.

- [ ] **Step 6: Commit**
```bash
git add model/vfs/vfsswift/impl_v3.go model/vfs/vfsswift/write_content_at_v3_test.go model/instance/storagemigration/migration.go
git commit -m "feat(vfsswift): add index-free WriteContentAt for storage migration"
```

---

## Task 6: Verification pass

**Files:**
- Modify: `model/instance/storagemigration/migration.go`
- Test: `model/instance/storagemigration/migration_test.go` (add case)

**Interfaces:**
- Produces: `func Verify(inst *instance.Instance, dst vfs.VFS, dstAv vfs.Avatarer, expected *Report) error` — re-enumerates and confirms each file/version object is present on the target with a matching byte size; errors listing the first missing/mismatched ID.

- [ ] **Step 1: Write the failing test**

Add to `migration_test.go`:
```go
func TestVerifySucceedsAfterCopyAndFailsWhenObjectMissing(t *testing.T) {
	inst, src, dst, srcAv, dstAv := setupMigrationFixture(t)
	rep, err := CopyContent(inst, src, dst, srcAv, dstAv)
	require.NoError(t, err)
	require.NoError(t, Verify(inst, dst, dstAv, rep))

	deleteOneTargetObject(t, inst, dst) // helper: remove a known key
	assert.Error(t, Verify(inst, dst, dstAv, rep))
}
```

- [ ] **Step 2: Run it, verify it fails**

Run: `go test ./model/instance/storagemigration/ -run TestVerify -v`
Expected: FAIL — `Verify undefined`.

- [ ] **Step 3: Implement `Verify`**

Add to `migration.go`. Re-run the same enumeration, but instead of copying, stat the target object via a new `contentStater` capability (add `StatContentAt(docID, internalID string) (int64, error)` to BOTH `vfss3` and `vfsswift` v3, mirroring `WriteContentAt`, returning `os.ErrNotExist` when the object is absent) and compare sizes. Count files+versions and compare totals to `expected`. Return the first discrepancy as an error. (Add the `StatContentAt` methods + a `contentStater` interface here, symmetric to Task 3/5b.) Since either backend can be the target, both must implement it.

- [ ] **Step 4: Run test, verify pass**

Run: `go test ./model/instance/storagemigration/ -run TestVerify -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add model/instance/storagemigration/migration.go model/instance/storagemigration/migration_test.go model/vfs/vfss3/impl.go
git commit -m "feat(storagemigration): verify target objects after copy"
```

---

## Task 7: Orchestration — `Migrate` with block/flip/dry-run/flag-only/purge

**Files:**
- Modify: `model/instance/storagemigration/migration.go`
- Test: `model/instance/storagemigration/migration_test.go` (add cases)

**Interfaces:**
- Consumes: `lifecycle.Block(inst, reason...)`, `lifecycle.Unblock(inst)`; `instance.Update(inst)`; the instance's VFS constructors.
- Produces:
```go
type Options struct {
	To         string // target scheme, e.g. "s3" (or "swift" for rollback)
	DryRun     bool
	FlagOnly   bool // switch pointer to an already-populated backend, no copy
	Force      bool // required with FlagOnly (data written since cutover is lost)
	PurgeSource bool // delete source objects after a successful flip
}
func Migrate(inst *instance.Instance, opts Options) (*Report, error)
```

- [ ] **Step 1: Write the failing tests**

Add cases to `migration_test.go`:
```go
func TestMigrateFlipsSchemeAfterVerify(t *testing.T) {
	inst := setupInstanceOnAfero(t) // populated
	rep, err := Migrate(inst, Options{To: "s3"})
	require.NoError(t, err)
	assert.Equal(t, "s3", inst.FsScheme)
	assert.Greater(t, rep.Files, 0)
	// Reads now served from S3:
	assertReadsServedFrom(t, inst, "s3")
}

func TestMigrateDryRunDoesNotFlip(t *testing.T) {
	inst := setupInstanceOnAfero(t)
	_, err := Migrate(inst, Options{To: "s3", DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, "", inst.FsScheme)
}

func TestMigrateFlagOnlyRequiresForce(t *testing.T) {
	inst := setupInstanceOnAfero(t)
	_, err := Migrate(inst, Options{To: "s3", FlagOnly: true})
	require.Error(t, err) // refuses without --force
}
```

- [ ] **Step 2: Run, verify fail**

Run: `go test ./model/instance/storagemigration/ -run TestMigrate -v`
Expected: FAIL — `Migrate`/`Options` undefined.

- [ ] **Step 3: Implement `Migrate`**

Add to `migration.go`. Flow:
1. Guards: `opts.To != inst.StorageScheme()`; if `opts.To == config.SchemeS3` require `config.HasS3Target()` or global S3; Swift source must be `SwiftLayout == 2`. If `opts.FlagOnly && !opts.Force` return an error explaining data-loss risk.
2. Build `src` = VFS for the current scheme, `dst` = VFS for `opts.To`, and the two `Avatarer`s, via the same constructors `MakeVFS`/`AvatarFS` use (`vfsswift.NewV3`, `vfss3.New`, `vfsafero.New`) with a shared `index := vfs.NewCouchdbIndexer(inst)`, `disk := vfs.DiskThresholder(inst)`, `mutex := config.Lock().ReadWrite(inst, "vfs")`. Ensure the target S3 bucket exists via `config.GetS3Client().MakeBucket(...)` guarded by BucketExists (do NOT call `InitFs`, which also touches the index).
3. `FlagOnly`: skip copy; go to step 6 after a presence check (`Verify` with a nil expected, i.e. just confirm target objects exist).
4. `lifecycle.Block(inst, instance.BlockedMoving.Code)`; `defer lifecycle.Unblock(inst)` on all paths.
5. `rep, err := CopyContent(...)`; then `Verify(...)`. On error: return without flipping (instance stays on source), reopened by the deferred Unblock.
6. `DryRun`: return `rep` now without flipping.
7. Flip: `inst.FsScheme = opts.To`; `instance.Update(inst)`.
8. `Unblock` (deferred).
9. `PurgeSource`: after a successful flip, delete source objects (reuse `pkg/s3util.DeletePrefixObjects` for S3 sources, or the Swift container delete for Swift sources). Keep this behind the explicit flag; default off.

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./model/instance/storagemigration/ -run TestMigrate -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add model/instance/storagemigration/migration.go model/instance/storagemigration/migration_test.go
git commit -m "feat(storagemigration): orchestrate block, copy, verify, flip, rollback"
```

---

## Task 8: Admin API handler + route

**Files:**
- Modify: `web/instances/instances.go` (add `migrateStorageHandler`; register in `Routes` ~732)
- Modify: `client/instances.go` (add `MigrateStorage`; extend `InstanceOptions` if needed)
- Test: `web/instances/instances_test.go` (add a case) or an integration test under `tests/`

**Interfaces:**
- Produces (server): `POST /instances/:domain/migrate-storage` with query params `to`, `dry_run`, `flag_only`, `force`, `purge_source`; returns the JSON `Report`.
- Produces (client): a thin `client.MigrateStorageOptions` struct (mirrors the engine's `Options` fields; keeps the `client` package free of the model import) and `client.MigrateStorageReport`, plus `func (ac *AdminClient) MigrateStorage(domain string, opts MigrateStorageOptions) (*MigrateStorageReport, error)`.

- [ ] **Step 1: Write the failing handler test**

Add to `web/instances/instances_test.go` a test that creates an instance, POSTs `/instances/<domain>/migrate-storage?to=s3&dry_run=true`, and asserts 200 + a `Report` body with `files >= 0` and the instance's `fs_scheme` still empty (dry-run).

- [ ] **Step 2: Run, verify fail**

Run: `go test ./web/instances/ -run TestMigrateStorage -v`
Expected: FAIL — route/handler missing (404).

- [ ] **Step 3: Implement the handler + route**

In `web/instances/instances.go`, mirroring `modifyHandler` (166) and `fsckHandler`:
```go
func migrateStorageHandler(c echo.Context) error {
	domain := c.Param("domain")
	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return wrapError(err)
	}
	opts := storagemigration.Options{
		To:          c.QueryParam("to"),
		DryRun:      c.QueryParam("dry_run") == "true",
		FlagOnly:    c.QueryParam("flag_only") == "true",
		Force:       c.QueryParam("force") == "true",
		PurgeSource: c.QueryParam("purge_source") == "true",
	}
	rep, err := storagemigration.Migrate(inst, opts)
	if err != nil {
		return wrapError(err)
	}
	return c.JSON(http.StatusOK, rep)
}
```
Register in `Routes`: `router.POST("/:domain/migrate-storage", migrateStorageHandler)`.

- [ ] **Step 4: Implement the admin client method**

In `client/instances.go`, mirroring `ModifyInstance` (231): build `url.Values` from the options and `ac.Req(&request.Options{Method: "POST", Path: "/instances/" + domain + "/migrate-storage", Queries: q})`, decode the `Report`.

- [ ] **Step 5: Run test, verify pass**

Run: `go test ./web/instances/ -run TestMigrateStorage -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add web/instances/instances.go client/instances.go web/instances/instances_test.go
git commit -m "feat(web/instances): add migrate-storage admin endpoint and client"
```

---

## Task 9: CLI command

**Files:**
- Modify: `cmd/instances.go` (add `migrateStorageCmd`, flag vars, register in `init`)
- Test: manual smoke (documented) + reuse existing cmd test harness if present

**Interfaces:**
- Consumes: `AdminClient.MigrateStorage` (Task 8).

- [ ] **Step 1: Add flag vars** (near line 47)
```go
var flagMigrateTo string
var flagMigrateDryRun bool
var flagMigrateFlagOnly bool
var flagMigrateForce bool
var flagMigratePurgeSource bool
```

- [ ] **Step 2: Define the command**
```go
var migrateStorageCmd = &cobra.Command{
	Use:   "migrate-storage <domain>",
	Short: "Migrate an instance's file storage to another backend (e.g. s3)",
	Long: `cozy-stack instances migrate-storage copies an instance's files, file
versions and avatar to another storage backend and switches the instance to it.
The source data is kept unless --purge-source is given.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return cmd.Usage()
		}
		ac := newAdminClient()
		rep, err := ac.MigrateStorage(args[0], client.MigrateStorageOptions{
			To:          flagMigrateTo,
			DryRun:      flagMigrateDryRun,
			FlagOnly:    flagMigrateFlagOnly,
			Force:       flagMigrateForce,
			PurgeSource: flagMigratePurgeSource,
		})
		if err != nil {
			return err
		}
		fmt.Printf("migrated: %d files, %d versions, %d bytes, avatar=%v\n",
			rep.Files, rep.Versions, rep.Bytes, rep.AvatarCopied)
		return nil
	},
}
```
(Define a `client.MigrateStorageOptions` mirror struct in `client/instances.go` so the CLI does not import the model package.)

- [ ] **Step 3: Register + flags** (in `init`)
```go
	instanceCmdGroup.AddCommand(migrateStorageCmd)
	migrateStorageCmd.Flags().StringVar(&flagMigrateTo, "to", "s3", "Target storage scheme")
	migrateStorageCmd.Flags().BoolVar(&flagMigrateDryRun, "dry-run", false, "Report what would be copied without writing or switching")
	migrateStorageCmd.Flags().BoolVar(&flagMigrateFlagOnly, "flag-only", false, "Switch the backend pointer without copying (rollback to a retained source)")
	migrateStorageCmd.Flags().BoolVar(&flagMigrateForce, "force", false, "Required with --flag-only; writes since cutover are lost")
	migrateStorageCmd.Flags().BoolVar(&flagMigratePurgeSource, "purge-source", false, "Delete source objects after a successful switch")
```

- [ ] **Step 4: Build + smoke**

Run: `go build ./... && ./cozy-stack instances migrate-storage --help`
Expected: build OK; help lists the flags.

- [ ] **Step 5: Commit**

```bash
git add cmd/instances.go client/instances.go
git commit -m "feat(cmd): add instances migrate-storage command"
```

---

## Task 10: Docs

**Files:**
- Modify: `docs/config.md` (document `fs.migration_target`)
- Modify: `docs/cli/cozy-stack_instances_migrate-storage.md` (regenerate via the docs generator if the repo autogenerates cobra docs; otherwise add manually) and the S3 doc from the S3 PR (`docs/*s3*`)

- [ ] **Step 1: Document the config key and the command**

Add a `fs.migration_target` example to `docs/config.md` and a "Migrating an instance to S3" section describing: configure the target, run `migrate-storage`, verify, `--purge-source` later, and the fleet-wide flip of `fs.url` at the end.

- [ ] **Step 2: Regenerate CLI docs if applicable**

Run: `make docs` (or the repo's cobra-doc generator target) and stage the generated file.

- [ ] **Step 3: Commit**

```bash
git add docs/
git commit -m "docs: document fs.migration_target and instances migrate-storage"
```

---

## Self-review notes (resolved spec open-questions)

- **Avatar authoritativeness:** confirmed user-uploaded and non-regenerable; a single object at `<DBPrefix>/avatar` (S3) / `"avatar"` (Swift container). Copied in Task 5 via the new `OpenAvatar` (Task 4). Thumbs are derived → skipped (regenerate on target).
- **Content-copy primitives:** read via exported `OpenFile`/`OpenFileVersion`; write via new index-free `WriteContentAt` (Task 3). Keys via exported `MakeObjectNameV3`/`MakeObjectKey`. Index never rewritten.
- **Read-only mechanism:** `lifecycle.Block`/`Unblock` gate HTTP traffic only (`web/middlewares/instance.go`), not in-process/worker writes. Task 7 blocks with `BlockedMoving` during the window. **Known v1 limitation:** background workers/triggers can still write to the source during the window; run migrations during low activity, or extend later to pause the instance's jobs. Documented as a risk.
- **Config shape:** chose a flat `fs.migration_target` URL (Task 2); the S3 connection is initialized from it at stack startup even while the global scheme stays Swift.
```
