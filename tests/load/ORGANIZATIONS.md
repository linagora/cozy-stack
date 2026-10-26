# Organization fixtures

Extend the [API-only seeder](SEEDING.md) with one organization instance and a
configurable number of user instances. No Docker dependency or new Stack API is
needed. This is Stack organization scope, not LDAP/Twake Console provisioning.
For recipient auto-accept and group add/remove convergence, see
[ORG-DRIVE-FANOUT.md](ORG-DRIVE-FANOUT.md).

## Create users, with or without data

Use the same private admin origin, credentials and disposable domain suffix as
in `SEEDING.md`. Empty organizations do not need `SEED_STACK_URL`; set the public
origin only when requesting files or drives. Run from `tests/load` with Node 22+
and `npm ci`.

```sh
# Empty users: choose SEED_USERS=10, 50 or 100 for the next workload.
make seed-org SEED_USERS=50 SEED_FILES=0 SEED_DRIVES=0

# Or populate each user with the same requested dataset.
make seed-org SEED_USERS=10 SEED_FILES=100 SEED_DRIVES=2
```

- `SEED_USERS` counts users only; the organization instance is additional.
  Default: 3. Range: 0..1,000.
- `SEED_FILES` and `SEED_DRIVES` are **per user**, not organization totals.
  Organization mode defaults both to 0 and accepts explicit zero/zero.
- With zero/zero, the builder does not request user tokens or call file/sharing
  APIs. Ordinary instance creation still initializes Stack's built-in directories
  and settings; “empty” means no additional seeded files or shared drives.
- The organization instance itself receives no seeded files or drives.
- Nonempty user data reuses the ordinary file/shared-drive helpers. Each user owns
  their own data; shared drives have no invited recipients. File payloads remain
  deterministic 1 KiB data, not a throughput benchmark.
- The existing single-instance `seed-scale` defaults and behavior are unchanged.
- Optional `SEED_CONTEXT_NAME` selects an existing server context for every new
  instance and verifies that selection in creation/readback responses. It does
  not create or change server configuration. Omit it to keep ordinary defaults.

The builder prints the organization and planned user domains before writes.
Keep the organization domain for cleanup and the generated user domains for your
workload. An explicit root can be supplied through `SEED_DOMAIN`; it must be a
fresh `seed-<32 lowercase hex>.<DNS suffix>[:port]` domain.

The organization root's first label becomes its unique `OrgID`. Every member has
a different disposable domain, the same `OrgID`, and the same synthetic logical
`OrgDomain` (`<OrgID>.seed.invalid`). The logical organization domain is not its
public URL. Existing fixture domains or organization scope are refused, not
adopted. Each created instance is read back before its data is populated.

Equivalent direct commands:

```sh
node --import tsx src/scripts/seed-scale-data.ts seed-org
node --import tsx src/scripts/seed-scale-data.ts seed-org "$SEED_DOMAIN"
node --import tsx src/scripts/seed-scale-data.ts cleanup-org "$SEED_DOMAIN"
node --import tsx src/scripts/seed-scale-data.ts verify "$SEED_DOMAIN"
```

The [fixture manifest](SEEDING.md#fixture-manifest) records the organization and
every member; `make seed-verify` checks them before and after a run.

## Explicit cleanup

```sh
# Use the printed ORGANIZATION domain, not an individual member domain.
export SEED_DOMAIN='seed-<32-hex-id>.load.example.test:8443'
make seed-org-cleanup
```

Cleanup enumerates the organization scope through the admin API, checks its
members and deletes the members the manifest records before the organization
instance. Members it does not record are kept, and so is the organization
instance, and cleanup fails. It reads back absence, preserves unrelated
organizations/instances and removes the manifest once everything is absent.
Generation counts and `SEED_STACK_URL` are not required for cleanup.

There are no retries, automatic cleanup, resume jobs or cross-operator locking.
A failed run leaves partial data. Stop generation and establish that accepted
server writes have finished before cleanup: timeout, disconnect or Ctrl-C does
not establish this. Do not add/move users or run concurrent operations against a
fixture while generating, checking, snapshotting or cleaning it.

## Reuse a baseline

Use the separate [deployment snapshot workflow](SEEDING.md#save-and-restore-separately).
Capture the whole disposable deployment, including all user databases/file data,
the global instance registry and required key material. After a fresh restore,
verify organization membership, original instance identities, per-user counts and
any file contents/shared-drive backlinks. Do not treat service health alone as
proof of a complete organization fixture. Keep snapshot bundles private.
