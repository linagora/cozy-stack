# Organization-drive group fanout

Measure native shared-drive auto-accept and group revocation across an organization.
This is a **recipient-count ramp**, not an upload benchmark or a VU/requests-per-second test.

Each point uses one fresh organization-owned drive, one ordinary contact group and
one small control file. A single group attach fans out to N recipients inside Stack.
After every recipient is ready and can read the file, a single group detach tests
revocation. Contact-group membership is not deleted. No managed LDAP/B2B directory
metadata is fabricated.

## Server prerequisites

Use a disposable Stack deployment. The bundled Stack (`make cozy-prepare`) has
the context below and its admin API on 127.0.0.1:16060, and Make points the
seeder at it ([SEEDING.md](SEEDING.md)). Elsewhere, configure a dedicated
context before creating the fixture, for example:

```yaml
contexts:
  load-org-sharing:
    common_contacts: true
    max_members_per_sharing: 1001.0
    sharing:
      auto_accept_trusted: true
      auto_accept_trusted_contacts: false
      trusted_domains: []
```

Same-organization auto-accept requires the recipient's `common_contacts` and
`sharing.auto_accept_trusted` flags, plus matching nonempty OrgIDs. Do not enable
broad domain trust to make the test pass. The runner checks effective context,
organization identities and the configured member cap before measured work.

The default native sharing cap is 90 **including owner and retained revoked
entries**. This Stack version reads its override as a floating-point context
value, hence `1001.0` above. Native group expansion is capped at 1,000 contacts;
keep cohort sizes within that bound. A configured cap is not a performance limit.

**Canonical instance URLs must resolve and reach Stack from Stack itself.**
Invitations, acceptance answers and notifications use HTTP even on one Stack.
The seeder's connection-origin + `Host` support helps the Node client only; it
cannot fix server-side DNS or a public port without an internal listener.
Configure suitable deployment routing before running this scenario. Do not
change host-wide DNS or trust settings merely for a disposable test.

## Run

Use the private admin/public connection origins and credentials described in
[SEEDING.md](SEEDING.md), with Node 22.23.4+ and `npm ci`.

```sh
export SEED_CONTEXT_NAME=load-org-sharing
export SEED_DOMAIN_SUFFIX=load.example.test:8443
# Also set SEED_ADMIN_URL, SEED_STACK_URL and private admin credentials.

make org-drive-fanout FANOUT_LEVELS=10,50,100
```

With no `FANOUT_ORG_DOMAIN`, the runner creates a fresh organization and enough
empty users for the largest requested level through the existing API-only seeder.
It does not populate personal files or drives. The organization instance owns
the test drives; fixture preparation is outside the measured attach/remove times.

To use a pre-created disposable fixture, including one restored from a baseline:

```sh
make seed-org SEED_USERS=100 SEED_CONTEXT_NAME=load-org-sharing
# Preserve the printed organization domain and complete server-side routing.
make org-drive-fanout FANOUT_ORG_DOMAIN='seed-<32-hex-id>.load.example.test:8443' \
  FANOUT_LEVELS=10,50,100 FANOUT_REPORT="$HOME/private/fanout.json"
```

Selecting an existing organization is explicit; the runner verifies fixture scope
and sufficient users rather than silently adopting an arbitrary organization.
Point drives/groups are fresh, so earlier revoked entries do not consume the next
sharing's member cap. Earlier point artifacts remain in the fixture for inspection;
this is not a cold-cache or history-free comparison.

## Shared DEV

On shared DEV, the actors come from the Cloudery fixture
([SEEDING.md](SEEDING.md#shared-dev-through-the-cloudery)) instead of the Stack
admin API. Run it only from the designated load VM, under the shared DEV lock:

```sh
export SEED_CLOUDERY_TOKEN_FILE=~/private/cloudery-token.json
export SEED_CLOUDERY_MANIFEST=~/private/<prefix>-cloudery-fixture.json
make org-drive-fanout LOAD_TARGET=dev FANOUT_LEVELS=1,2,4,8,16 MAX_RUNTIME=30m
```

- Before any write, each instance must match its manifest UUID on the Stack, and
  each member's context must enable same-organization auto-accept, as above. The
  organization instance's own context needs neither flag.
- Every token is minted through the Cloudery for that instance alone, and minted
  again before a level once it is 10 minutes old (tokens last 30 minutes).
- Levels cannot exceed the fixture's members. `MAX_RUNTIME` includes preparation
  and bounds workload requests. A level starts only with time for both phases.
  Stopping between completed levels gives a lower bound (`budget_reached: runtime`);
  stopping before the first or during a level is inconclusive. Cleanup has a
  separate five-minute allowance.
- Drives are named `twake-load-fanout-<EXECUTION_ID>-<level>-<id>`. After the
  run, each of them waits until its roster stops changing, then its root is
  trashed, which revokes it everywhere, and destroyed. The run's groups and
  contacts are deleted too. Anything that cannot be removed is listed under
  `cleanup.retained` and leaves the run inconclusive (exit 2), keeping the levels
  as evidence. A lost setup response also leaves cleanup unverified: deleting
  known objects cannot prove that an unknown write created nothing else.
  The next DEV run first removes drives an earlier run left.

## Settings and success criteria

- `FANOUT_LEVELS`: increasing distinct recipient counts, default `1,2,4,8,16`.
- `FANOUT_TIMEOUT_MS`: per-operation convergence budget, default `60000`.
- `FANOUT_POLL_MS`: observation interval, default `500`.
- `FANOUT_POLL_CONCURRENCY`: bounded observer concurrency, default `8`.
- `FANOUT_STABLE_MS`: required stable success window, default `3000`.
- `FANOUT_REPORT`: new private JSON report; no overwrite or token output.
  The Make target defaults to `data/results/<EXECUTION_ID>-org-drive-fanout.json`
  for the [Capacity Overview](CAPACITY-OVERVIEW.md). The Node CLI itself keeps stdout-only mode available.

N is the number of recipients; client mutation concurrency is **1**. Observer
concurrency is separate and contributes read load. This first scenario does not
issue N competing writes to the same sharing document. A bulk-direct recipient
control or concurrent individual mutation test can be added separately.

Attach passes only when the owner reports exactly the expected ready members,
every recipient's own sharing is active/ready, and each can read the actual drive
root/control bytes. `pending`, `seen`, owner `active`, shortcut existence, HTTP 200
or an empty job queue alone are not acceptance proofs. Timers include the mutation
request and observation; late completion cannot count as meeting the budget.
An observation read still in flight when the budget ends is cut off and counts as
a missed deadline, the same as a late reply.
Recipient peer rosters can retain unresolved entries, so the runner checks each
recipient's own member and the owner's complete cohort, not every peer snapshot.

Detach is measured separately. The runner rediscovers the active group index,
checks owner group/member revocation, recipient inactive/revoked state and denied
file access. The empty organization drive must remain active on its owner.
The cohort starts with group-only grants: removing a group does not promise loss
of access when a recipient has another direct, group or ancestor-sharing grant.

The report retains per-instance outcomes and add/remove timings. Each phase's
`converged_ms` is when every check first passed, to within one observation round;
`elapsed_ms` also includes the `FANOUT_STABLE_MS` window, and `FANOUT_TIMEOUT_MS`
must cover both. Sparse sampled
levels produce a tested bracket `[last passing, first failing)`, not an exact
maximum. All passing levels give a lower bound. Configuration errors, uncertain
mutations, failed observations or unverified revocation make the result
inconclusive rather than a claimed capacity limit.

The runner exits 0 for a lower bound or a bracket, 1 when the first level
misses its deadline, and 2 when the result is inconclusive, as campaigns
classify it ([README](README.md#campaigns)).

## Retained state and cleanup

There are no write retries, hidden manual acceptance, instance auto-deletion or
resume jobs. On a failure, the ramp stops and retains the fixture/point identifiers.
A mutation timeout/disconnect does not prove server work ended; do not revoke or
clean up a potentially active fanout merely because its client stopped.

After establishing that accepted work has settled, use the explicit organization
cleanup from [ORGANIZATIONS.md](ORGANIZATIONS.md), or discard the entire owned
restored deployment. Never remove a shared bucket or another deployment's data.
Reports contain fixture identities but never admin passwords, bearer tokens or
sharing credentials. Keep reports and snapshot bundles private.
