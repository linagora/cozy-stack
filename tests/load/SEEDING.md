# Build a reusable dataset fixture

Generate an instance once through existing Stack APIs, then reuse a consistent
snapshot for isolated test runs. Generation is deliberately sequential: it is
fixture preparation, not an upload-throughput benchmark.

The Node builder uses the existing instance admin API, dedicated file-upload API
and the harness's `createSharedDrive()`. It does not write CouchDB documents or S3
objects directly. No new Go package, admin endpoint or Stack configuration flag is
needed. Generic data APIs can be added for suitable document types later; files
and sharings must keep their dedicated creation APIs.

## Organization fixtures

For configurable user counts with optional per-user files and shared drives, see
[ORGANIZATIONS.md](ORGANIZATIONS.md). Organization mode defaults to empty users;
the single-instance defaults below are unchanged.

## Shared DEV through the Cloudery

Where the Stack admin API is internal, a Cloudery partner token seeds one
long-lived organization through the Cloudery API instead. It never calls the
Stack admin API. Keep the token file (`id`, `public`, `manager_url`), the domain
and the prefix private.

```sh
export SEED_CLOUDERY_TOKEN_FILE=~/private/cloudery-token.json
export SEED_CLOUDERY_DOMAIN=<instance domain> SEED_CLOUDERY_PREFIX=<prefix>
make seed-org SEED_USERS=20   # organization <prefix> and members <prefix>01..20
make seed-org-cleanup         # deletes them; the organization record stays
```

The organization instance is named after the organization ID: that is how the
Stack recognizes an organization's own instance, whose drives are organization
drives. New instances get the email `<slug>@spam.cozycloud.cc`, a test-only
mailbox domain; re-runs leave existing instances' emails unchanged. Re-runs reuse
existing instances and add missing members. Only names built from
the prefix are reused, recorded or deleted, and only after the Stack confirms
their organization and UUID. Teardown keeps any other match and exits 2, and
releases only the names of instances it deleted or the manifest recorded. A
smoke that reads back different content exits 1; any other failure exits 2.
Every request uses a token minted for that instance, refreshed
before it expires. Members auto-accept the organization's drives because they
share its organization ID, so each member's context must enable `common_contacts`
and `sharing.auto_accept_trusted`, or the seed fails. The seed ends with a
1 KiB smoke test on the first member and writes the instance identities to
`data/results/<prefix>-cloudery-fixture.json` (`SEED_CLOUDERY_MANIFEST`), without
tokens. Offers default to `b2b_org_default` and `b2b_twake_default`
(`SEED_CLOUDERY_ORG_OFFER`, `SEED_CLOUDERY_MEMBER_OFFER`).

## Build once

Use Node 22 or newer and run `npm ci` in `tests/load`. The target must be a
**disposable test deployment**, with nested subdomains and private admin access.
Do not generate fixtures on production or reuse the normal load-test instance.

```sh
export SEED_ADMIN_URL=https://admin.example.test:6443
export SEED_STACK_URL=https://stack.load.example.test:8443
export SEED_DOMAIN_SUFFIX=load.example.test:8443
read -rsp 'Admin passphrase: ' SEED_ADMIN_PASSWORD; echo
export SEED_ADMIN_PASSWORD

make seed-scale SEED_FILES=1000 SEED_DRIVES=10
# Seed instance: seed-<32-hex-id>.load.example.test:8443
```

`SEED_ADMIN_URL` and `SEED_STACK_URL` are explicit HTTP(S) connection origins,
without credentials, paths, queries or fragments. Public requests connect to
`SEED_STACK_URL` while sending the generated instance domain as the exact `Host`
header, including its public port. This also supports loopback/tunnel endpoints
without adding every generated instance to DNS. HTTPS certificates must validate
for the connection hostname; TLS verification stays enabled. Private CAs can use
`NODE_EXTRA_CA_CERTS`.

The CLI prints its disposable domain before the first API request. It refuses an
existing instance. Optional `SEED_ADMIN_USERNAME` defaults to `admin`. An explicit
`SEED_DOMAIN` can select a fresh `seed-<32 lowercase hex>.<DNS suffix>[:port]` domain
for Make; otherwise `SEED_DOMAIN_SUFFIX` is required. The hyphenated label requires
`subdomains: nested`, not flat mode. Existing admin access rules apply; keep
credentials in environment variables, never in URLs or command-line arguments.

Equivalent direct commands:

```sh
node --import tsx src/scripts/seed-scale-data.ts seed
node --import tsx src/scripts/seed-scale-data.ts seed "$SEED_DOMAIN"
node --import tsx src/scripts/seed-scale-data.ts cleanup "$SEED_DOMAIN"
```

Files contain 1 KiB of deterministic data. Shared drives have distinct empty
roots, created as owner drives without invited recipients. Defaults are 100 files
and zero drives. Bounds are 0..1,000,000 files and 0..100,000 drives, with at least
one requested object. Those bounds are not a performance guarantee. The builder
never changes the harness's normal `.env` or prints access tokens.

## Profiles

`SEED_PROFILE` picks the dataset each populated instance gets:

- `flat` (default): `SEED_FILES` identical 1 KiB files at the root and
  `SEED_DRIVES` empty drives. Large counts stay opt-in, for isolated
  deployments.
- `tree` (version 1, `src/scripts/dataset-profile.ts`): directories three
  levels deep, seven files from 512 B to 1 MiB, and a shared drive holding a
  folder and two of the files, about 1.4 MiB per instance; `SEED_FILES` and
  `SEED_DRIVES` do not apply. Each file's bytes come from
  `SEED_GENERATION_SEED` (default `1`) and its path, so the same seed always
  gives the same structure and content, while resource IDs belong to each run.
  It fits shared DEV budgets: 20 members upload under 30 MiB.

```sh
make seed-org SEED_USERS=2 SEED_PROFILE=tree SEED_GENERATION_SEED=7
```

## Fixture manifest

Each seed records its fixture in a private manifest,
`data/results/seed-<32-hex-id>-fixture.json` (`SEED_MANIFEST` overrides the path).
It holds the profile (with the tree's version and generation seed), generation
parameters, the expected content checksum and every instance's domain, UUID and
role, with the directory, file and shared-drive IDs it confirmed and the root
entry count, and the preparation's time and uploaded bytes, kept apart from any
measured work. It never holds tokens. Instance UUIDs are chosen before any write, and the manifest is written
before the first instance is created, then after each step, so a failed seed
leaves a `failed` manifest with every planned identity and confirmed object. A
seed refuses to replace an existing manifest.

```sh
make seed-verify SEED_DOMAIN='seed-<32-hex-id>.load.example.test:8443'
```

Verify the fixture before using it and again after a run. Only a `complete`
manifest passes. Verification reads every recorded instance, directory, file and
drive root and fails if an identity, a count, a directory's or file's name or
parent, a file's checksum or size, a drive backlink, the root entry count or the
organization membership differs from the manifest. It never writes.

## Save and restore separately

Snapshot management depends on the deployment and is **not implemented by this
builder**. The separate [Compose fixture commands](SNAPSHOTS.md) provide
`snapshot`, `restore` and `discard` for this harness's isolated local topology.
They do not add another Stack seeding API or a generic backup framework.

The first workflow should use an isolated cold baseline:

1. Finish generation, verify the dataset and wait for background work to settle.
2. Stop that disposable Stack and its storage services cleanly. Capture consistent
   CouchDB state, including global instance metadata, actual file storage, and
   required configuration/key material. Keep credentials and archives private.
3. Pin compatible Stack/storage versions. Define which Redis/RabbitMQ state must
   be retained, or prove queues are idle and start those runtime services empty.
4. For each test run, restore the same immutable baseline into **new isolated
   storage**. Preserve its instance domains/IDs inside that isolated environment;
   do not restore over an active or shared deployment.
5. Read back fixture counts, file contents/checksums and shared-drive relationships.
   Mint fresh API tokens, define index/cache warm-up, then run the measured tests.
6. Discard only that restored environment and its owned storage. Keep the baseline
   unchanged for the next run.

A local prototype verified this flow with CouchDB, MinIO and fresh Redis/RabbitMQ
services, including two independent restores and removal of a test-added file on
reset. That proves the small-fixture mechanism, not large-dataset performance or
support for every deployment. Measure capture/restore/start-up cost on the target
fixture. Scenarios involving external systems may require a wider snapshot scope.

## Failures and explicit cleanup

There are no automatic retries, resume, cleanup or recovery jobs. A failure can
leave a partial instance. `SEED_REQUEST_TIMEOUT_MS` is a complete-response deadline
for **each request**, defaulting to 1,800,000 milliseconds (30 minutes); it is not a
deadline for the whole build. A timeout, Ctrl-C or disconnect does not prove an
accepted server operation has finished. Establish that writes stopped before
cleanup, especially after a lost instance-create/upload response.

```sh
# Replace the placeholder with the printed domain; run only after writes stopped.
export SEED_DOMAIN='seed-<32-hex-id>.load.example.test:8443'
make seed-scale-cleanup
```

Fixtures seeded before manifests existed have none, so cleanup refuses them:
remove them with the release that seeded them, or through the admin API once
you have checked they are yours.

Cleanup needs the fixture's manifest and deletes only the instances it records,
with the existing `DELETE /instances/:domain`. An instance is deleted only when it
still has the recorded UUID; this also covers a creation whose reply was lost.
Another UUID at a recorded domain is kept and cleanup fails. A missing, unreadable
or mismatched manifest fails before any request. Cleanup accepts already-absent
instances, confirms absence through the existing admin API and then removes the
manifest. It does not need `SEED_STACK_URL` or valid generation counts. Only
explicit disposable `seed-...` domains are accepted. For a restored test run,
discard the entire owned environment instead; never delete a shared bucket or
someone else's volumes.
