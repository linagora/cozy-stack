# Per-instance storage backend & Swift→S3 migration — Design

Date: 2026-07-16
Status: Draft (design approved, spec under review)
Depends on: `feat/s3-vfs-backend` (adds `config.SchemeS3` and the `vfss3` VFS backend)

## Goal

Enable migrating a **single live production instance** from the Swift storage
backend to the S3-compatible backend, one instance at a time, without a
big-bang cutover of the whole fleet. The end state is a fleet fully on S3; the
per-instance mechanism is **transitional** and disappears (becomes a no-op)
once the global default is flipped to S3.

Non-goals (explicitly out of scope):

- Per-instance S3 **endpoints/credentials**. The target is a single global S3;
  we do not build a per-endpoint connection factory.
- Zero-downtime online migration with delta sync. A **read-only maintenance
  window** per instance is acceptable for v1.
- Migrating regenerable/ephemeral data (thumbnails, app assets, cache,
  exports, archives) — those regenerate or reinstall on the target.

## Current state (why this is needed)

Backend selection is **entirely global** today:

- `Instance.MakeVFS()`, `AvatarFS()`, `ThumbsFS()`
  (`model/instance/instance.go:250-333`) switch on
  `config.FsURL().Scheme` — the single global `fs.url` scheme.
- There is **no per-instance scheme field**. The only per-instance storage
  field is `SwiftLayout` (`swift_cluster`), which selects a Swift *sub-variant*,
  not a different backend.
- The S3 connection is a process-wide **singleton**
  (`pkg/config/config/s3.go`), keyed by the global `fs.url` query params.
  Per-instance isolation on S3 is achieved by bucket-per-org
  (`<bucket_prefix>-<orgId>`) + key-prefix-per-DBPrefix, all against one
  global endpoint.
- **No migration tooling between backends exists.** `cozy-stack swift
  ls-layouts` only counts; `Fsck` only verifies; `lifecycle` "move" is
  cozy-to-cozy server relocation, not backend migration.
- Backend is fixed at instance creation (`lifecycle/create.go:143-153`) and is
  **not changeable via patch**.

## Chosen approach: per-instance flag, in-place, dual-connection stack

Rejected alternative — *relocate instances to S3-global stacks via the
cozy-to-cozy move machinery*: no core VFS change, but far heavier per instance
(relocation, domains, tokens, routing) for what is only a backend swap. Poor
effort/benefit for a fleet backend migration.

The stack runs **both** the Swift and the S3 connection during the transition.
Each instance carries a flag selecting its backend; migration copies the
authoritative content to S3 and flips the flag inside a read-only window.

## Architecture

### 1. Configuration — dual connection during transition

`fs.url` stays the **global default** (`swift://…`). Add an optional S3
migration target so the S3 connection is initialized even while the default is
Swift:

```yaml
fs:
  url: swift://…                      # global default (unchanged)
  migration_target:
    url: s3://key:secret@endpoint/?region=…&bucket_prefix=…
```

- At startup, if `fs.migration_target` is present, call `InitS3Connection`
  (in addition to the default backend init) so both `GetSwiftConnection()` and
  `GetS3Client()` singletons are live.
- Add a `config.HasS3Target()` / accessor so the migration command can refuse
  to run when no S3 target is configured.
- **End of migration:** operator sets `fs.url` to the `s3://…` URL and removes
  `fs.migration_target`. The per-instance flag then equals the global default
  and becomes vestigial; migrated instances keep working unchanged.

The exact config shape (nested `migration_target` vs a flat `fs.s3_url`) is an
implementation detail; the invariant is: **both connections initialized during
the transition period**.

### 2. Per-instance field

Add to the `Instance` struct (`model/instance/instance.go`, next to
`SwiftLayout`):

```go
FsScheme string `json:"fs_scheme,omitempty"` // "" = global default; "s3" = migrated
```

- Stored on the `io.cozy.instances` doc (same place as `swift_cluster`).
- Empty for all existing instances → **zero regression**, no data migration of
  the instance registry.
- Add an accessor computing the **effective** scheme:

```go
func (i *Instance) storageScheme() string {
    if i.FsScheme != "" {
        return i.FsScheme
    }
    return config.FsURL().Scheme
}
```

### 3. Backend selection

`MakeVFS`, `AvatarFS`, `ThumbsFS` (`instance.go:250-333`) read
`i.storageScheme()` instead of `config.FsURL().Scheme`.

Refactor the currently-triplicated `switch` into a single builder, e.g.
`buildStorage(scheme, kind)` where `kind ∈ {main, avatars, thumbs}`. The
triplication is a real smell in code we are already modifying; consolidating it
keeps the three call sites in lock-step and is the only place the scheme→VFS
mapping lives.

### 4. Migration engine — object-storage level, index untouched

The VFS = **CouchDB index/tree** (`io.cozy.files`) + **object storage
content** (bytes keyed by file/version ID). Both backends address content by
ID. Therefore migration **does not copy the index** — only the content bytes,
object by object:

1. Iterate every content-bearing doc for the instance:
   - all `io.cozy.files` **including trashed** files;
   - all `io.cozy.files.versions` (old versions live in storage too);
   - **uploaded avatars** (authoritative user data — see Open Questions to
     confirm the exact storage/doctype).
2. For each: stream content from the **source** storage and `PutObject` into
   the **target** (S3) storage under the same logical ID/key.

This bypasses the high-level `CreateFile` path (which would try to create index
docs) and needs two small primitives per backend:

- source: "open content by ID" (`vfsswift`/`vfsafero` already expose object
  open);
- target: "put content by ID" (`vfss3` already has `PutObject`).

The engine is **backend-agnostic** (source afero/swift → target s3), which also
makes it testable afero→s3 without a Swift server.

The migration **does not** copy thumbnails, app/konnector assets, cache,
exports, or archives — these regenerate or reinstall on first access against
the S3 backend after the flip.

### 5. Migration command

```
cozy-stack instances migrate-storage <domain> --to s3 [--dry-run] [--purge-source]
```

Flow:

1. Guards: S3 target configured (`config.HasS3Target()`); `--to` differs from
   the instance's current effective scheme.
2. **Maintenance ON** (read-only): reject writes for the duration. (Reuse the
   existing instance maintenance/blocking mechanism — pin the exact call in the
   plan.)
3. Copy content source→S3 (files + versions + trash + avatars).
4. **Verify**: re-walk; every object present in the target with matching size
   (and/or MD5); reconcile counts against the index.
5. On success only: set `FsScheme = "s3"`, persist the instance doc.
6. **Maintenance OFF**.

The flag flips **only after full verification**, so there is never a live,
half-migrated instance.

`--dry-run`: walk + report counts/sizes without writing or flipping.
`--purge-source`: NOT the default — see Rollback.

### 6. Rollback command

Because the source backend is **retained by default**, rollback reuses the same
generic `--to <scheme>` engine — there is no separate code path — with two
modes:

1. **Instant flip-back** —
   `cozy-stack instances migrate-storage <domain> --to swift --flag-only`:
   switches `fs_scheme` back **without copying**, pointing the instance at the
   still-present Swift snapshot. Runs in a read-only window and verifies the
   source objects are present before flipping.
   - Refuses if the source was already purged (`--purge-source` has run).
   - Any writes made on S3 **since the cutover are lost** (the Swift snapshot is
     stale), so it warns and requires `--force`. Intended for immediate
     post-cutover recovery, before real traffic writes to S3.

2. **Safe re-migration** —
   `cozy-stack instances migrate-storage <domain> --to swift`:
   the same engine in reverse (read-only window, copy S3→Swift, verify, flip).
   **No data loss**; use this once real writes have landed on S3.

`--flag-only` is a general primitive ("switch the pointer to a backend that is
already populated"); it is only meaningful for rollback since the forward
migration must copy first.

### 7. Safety & idempotence

- Source data is **kept** by default. `--purge-source` is a **separate,
  deferred** step run only after a confidence period.
- Read-only window ⇒ the copied snapshot is consistent (no concurrent writes).
- Copy is **idempotent**: re-running overwrites target objects; a failed run
  leaves the flag unchanged (instance still on the source), reopens the
  instance, and reports. Partial target objects are overwritten on retry or
  removed by `--purge-source` on the target if aborted.

### 8. Error handling

- Any failure in copy or verify ⇒ do **not** flip `FsScheme`; reopen the
  instance on its original backend; surface the error with the failing
  file/version ID.
- Guard against running when the S3 target is unconfigured, or when `--to`
  equals the current scheme (no-op).

### 9. Testing

- Reuse the MinIO testcontainer already in the VFS suite; use the afero
  backend as the migration **source** (no Swift server needed).
- Cases:
  - populate an instance with files, multiple versions, trashed files, and an
    uploaded avatar; run `migrate-storage --to s3`; assert every content object
    exists in S3 with matching size, the CouchDB index is **unchanged**, and
    reads are served from S3 after the flip;
  - `--dry-run` writes nothing and flips nothing;
  - idempotence: running twice yields the same result;
  - rollback: `FsScheme` cleared → reads served from source again (while source
    retained);
  - failure injection mid-copy leaves the instance on the source backend.

## Open questions to resolve during implementation

1. **Avatar authoritativeness & storage.** Confirm whether an uploaded avatar
   is stored only in `AvatarFS` (authoritative, must be copied) vs derivable.
   The design assumes it is authoritative and copies it; verify the exact
   doctype/key so the engine enumerates it correctly.
2. **Exact content-copy primitives.** Confirm the object-open (source) and
   object-put (target) signatures actually exposed by `vfsswift`, `vfsafero`,
   and `vfss3`, and whether a small shared interface is warranted vs
   backend-specific helpers.
3. **Maintenance mechanism.** Pin the exact API used to put the instance
   read-only for the window (instance blocking/maintenance) and confirm it
   rejects writes at the VFS layer, not just the UI.
4. **Config shape.** Decide nested `fs.migration_target.url` vs a flat
   `fs.s3_url`, and how `InitS3Connection` is invoked when the global scheme is
   still Swift.

## Rollout sequence (operational)

1. Deploy stack with `fs.migration_target` configured (both connections live).
2. Migrate instances one at a time with `migrate-storage`, verifying each.
3. After a confidence period, `--purge-source` per migrated instance.
4. Once the whole fleet is on S3, flip `fs.url` to the S3 URL, drop
   `fs.migration_target`; `fs_scheme` becomes a no-op that can later be removed.
