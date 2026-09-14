# Jenkins release metadata publication

`publish-deployment.sh` extends `gozy-bookworm-prod-04-publish-release`. It
downloads the exact original Bookworm/amd64 production package with APT, checks
its control fields, and hashes the `.deb`. APT verifies the repository signature
and package checksum using the agent's existing trusted keys. No package is
installed.

The next numeric release tag is an **annotated tag containing `deployment.json`**.
The annotation preserves the package mapping even when Jenkins discards its
workspace. GitHub's existing tag-triggered workflow still creates the release.
The publisher waits for that exact release, then uploads the manifest. Repeating
the same commit/package publication reuses its annotated tag. An explicit retry
reads the annotation and only uploads metadata, ignoring the latest deployment
record. A conflicting existing asset fails without overwriting it.

## Proposed Jenkins changes (not applied)

First merge the script into `master`, which this job already checks out. Confirm
that the Bookworm agent has Bash, `curl`, `jq`, Git, APT, `dpkg-deb`, `sha256sum`,
the internal APT repository's trusted signing key, and its Git tagger name/email
configured.
The agent must reach `http://apt.int.cozycloud.cc/` and GitHub. Signature checking
must succeed; do not use `trusted=yes` or allow unauthenticated packages.

Then change only `gozy-bookworm-prod-04-publish-release`:

1. Add a string parameter `RETRY_RELEASE_TAG`, default empty. Description:
   "Metadata-only retry for an existing release created by this publisher. Leave
   empty for normal release publication."
2. Bind a reviewed Jenkins secret-text credential to `GH_TOKEN`. Its GitHub token
   must have `Contents: write` on `linagora/cozy-stack` for release asset uploads.
   The existing SSH credential used for Git pushes remains in use.
3. Keep the existing artifact-copy and environment-injection steps. Replace the
   entire current Execute shell command with:

   ```bash
   #!/bin/bash
   set -eu
   bash scripts/packaging/publish-deployment.sh
   ```

4. Add **Archive the artifacts**, pattern `deployment.json`. Leave **Archive
   artifacts only if build is successful** unchecked. Enable **Do not fail the
   build if archiving returns nothing**, since an empty commit intentionally skips
   publication and preflight failures may occur before a manifest exists.

No deployment, promotion, or version-record jobs change in this patch.
`PUBLISH_RELEASE` and selecting the exact upstream deployment record belong to
the later rollback-enablement step.

## Retry

After a failure, inspect the archived manifest and confirm that its tag exists
on GitHub. Run this publication job with `RETRY_RELEASE_TAG` set to that tag.
Retry mode never creates or pushes a tag. Existing lightweight tags have no
embedded manifest and are rejected; their backfill is a separate task.

## Local check

```sh
bash -n scripts/packaging/publish-deployment.sh scripts/packaging/test-publish-deployment.sh
bash scripts/packaging/test-publish-deployment.sh
```

The check requires Bash, Git, `jq`, and `sha256sum`. It uses temporary local Git
repositories and simulated APT, `dpkg-deb`, and GitHub commands. It covers fresh
checkout retries, lost upload responses, conflicting assets, invalid packages,
release creation timeouts, authentication failure, and skipped publication.
It does not contact Jenkins, GitHub, or the internal package server.

API behavior follows the [GitHub release asset API](https://docs.github.com/en/rest/releases/assets).
Package verification uses [APT's download and strict update commands](https://manpages.debian.org/bookworm/apt/apt-get.8).
