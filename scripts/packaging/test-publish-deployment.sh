#!/bin/bash
set -euo pipefail

publisher=$(cd "$(dirname "$0")" && pwd)/publish-deployment.sh
check_root=$(mktemp -d)
cleanup() {
  local status=$?
  if (( status != 0 )) && [[ -f $check_root/output.log ]]; then
    cat "$check_root/output.log" >&2
  fi
  rm -rf -- "$check_root"
  exit "$status"
}
trap cleanup EXIT
export CHECK_ROOT=$check_root REAL_GIT="$(command -v git)"
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
export GH_TOKEN=test-token DEBIAN_VERSION='1:1.2~20260904~bookworm.build2'
export RETRY_RELEASE_TAG= GH_MODE=lost-response APT_MODE=ok FORBID_GIT_PUSH=false
export PACKAGE_VERSION='1:1.2~20260904~bookworm.build2' PACKAGE_ARCHITECTURE=amd64
mkdir "$check_root/bin"
printf 'original Debian package' > "$check_root/package.deb"
echo '[]' > "$check_root/assets.json"

cat > "$check_root/bin/mock-command" <<'MOCK'
#!/bin/bash
set -euo pipefail
case ${0##*/} in
  git)
    if [[ $FORBID_GIT_PUSH == true && $1 == push ]]; then
      echo 'Retry must never push a tag' >&2
      exit 1
    fi
    exec "$REAL_GIT" "$@"
    ;;
  apt-get)
    if [[ " $* " == *' download '* ]]; then
      [[ ${!#} == 'cozy-stack:amd64=1:1.2~20260904~bookworm.build2' ]]
      [[ $APT_MODE != missing ]] || { echo 'Package unavailable' >&2; exit 100; }
      cp "$CHECK_ROOT/package.deb" cozy-stack.deb
    fi
    ;;
  dpkg-deb)
    printf 'Package: cozy-stack\nVersion: %s\nArchitecture: %s\n' \
      "${PACKAGE_VERSION:-1:1.2~20260904~bookworm.build2}" "${PACKAGE_ARCHITECTURE:-amd64}"
    ;;
  sleep) [[ $1 == 10 ]] ;;
  curl)
    output= url= data=
    while (( $# > 0 )); do
      case $1 in
        --output) output=$2; shift 2 ;;
        --data-binary) data=$2; shift 2 ;;
        https://*) url=$1; shift ;;
        *) shift ;;
      esac
    done
    IFS= read -r authorization
    [[ $authorization == 'Authorization: Bearer test-token' ]]
    printf '%s\n' "$url" >> "$CHECK_ROOT/requests.log"
    case $url in
      https://api.github.com/repos/linagora/cozy-stack)
        echo '{}' > "$output"
        if [[ $GH_MODE == unauthorized ]]; then printf 401; else printf 200; fi
        ;;
      https://api.github.com/repos/linagora/cozy-stack/releases/tags/1.6.60)
        if [[ ! -f $CHECK_ROOT/release-created || $GH_MODE == never-created ]]; then
          touch "$CHECK_ROOT/release-created"
          echo '{}' > "$output"
          printf 404
        else
          jq -n --slurpfile assets "$CHECK_ROOT/assets.json" \
            '{id: 7, tag_name: "1.6.60", draft: false, assets: $assets[0]}' > "$output"
          printf 200
        fi
        ;;
      'https://uploads.github.com/repos/linagora/cozy-stack/releases/7/assets?name=deployment.json')
        [[ $data == @deployment.json ]]
        jq -e 'length == 0' "$CHECK_ROOT/assets.json" > /dev/null
        digest="sha256:$(sha256sum deployment.json | cut -d ' ' -f 1)"
        jq -n --arg digest "$digest" \
          '{name: "deployment.json", state: "uploaded", digest: $digest}' > "$output"
        jq -s . "$output" > "$CHECK_ROOT/assets.json"
        [[ $GH_MODE != lost-response ]] || { echo 'Simulated lost upload response' >&2; exit 52; }
        printf 201
        ;;
      *) echo "Unexpected GitHub request: $url" >&2; exit 1 ;;
    esac
    ;;
  *) exit 1 ;;
esac
MOCK
chmod +x "$check_root/bin/mock-command"
for command in git apt-get dpkg-deb curl sleep; do
  ln -s mock-command "$check_root/bin/$command"
done
export PATH="$check_root/bin:$PATH"

expect_failure() {
  local message=$1
  if bash "$publisher" > "$check_root/output.log" 2>&1; then
    echo "Expected failure: $message" >&2
    exit 1
  fi
  grep -F "$message" "$check_root/output.log" > /dev/null
}
publish() {
  bash "$publisher" > "$check_root/output.log" 2>&1
}
upload_count() {
  grep -c '^https://uploads.github.com/' "$check_root/requests.log"
}

git init -q --bare --initial-branch=master "$check_root/remote.git"
git init -q --initial-branch=master "$check_root/work"
cd "$check_root/work"
git config user.name 'Local test'
git config user.email test@example.invalid
git commit -q --allow-empty -m 'Test deployment'
export GIT_STACK_COMMIT="$(git rev-parse HEAD)"
commit=$GIT_STACK_COMMIT
git tag 1.6.59
git tag non-release-tag
git remote add origin "$check_root/remote.git"
git push -q origin master refs/tags/1.6.59 refs/tags/non-release-tag
git tag --annotate other-local-tag --message 'Unrelated tag'
git config push.followTags true

expect_failure 'Simulated lost upload response'
jq -e --arg commit "$commit" --arg version "$DEBIAN_VERSION" \
  --arg sha "$(sha256sum "$check_root/package.deb" | cut -d ' ' -f 1)" \
  '. == {release_tag: "1.6.60", git_commit: $commit, debian_version: $version, package_sha256: $sha}' \
  deployment.json > /dev/null
cp deployment.json "$check_root/expected.json"
git ls-remote --tags origin > "$check_root/tags.before"
! grep -F other-local-tag "$check_root/tags.before"
[[ $(upload_count) == 1 ]]

# Retry with a clean checkout and a newer deployment record, without fetching a package or pushing.
git clone -q "$check_root/remote.git" "$check_root/retry"
cd "$check_root/retry"
export RETRY_RELEASE_TAG=1.6.60 GIT_STACK_COMMIT=wrong DEBIAN_VERSION=wrong
export GH_MODE=ok APT_MODE=missing FORBID_GIT_PUSH=true
publish
cmp deployment.json "$check_root/expected.json"
[[ $(upload_count) == 1 ]]
export RETRY_RELEASE_TAG=1.6.59
expect_failure 'Tag has no deployment annotation'
jq '.release_tag = "1.6.61" | .git_commit += "\n"' "$check_root/expected.json" > "$check_root/invalid.json"
git -c user.name='Local test' -c user.email=test@example.invalid \
  tag --annotate 1.6.61 "$commit" --file "$check_root/invalid.json"
export RETRY_RELEASE_TAG=1.6.61
expect_failure 'Invalid deployment manifest'
git tag --delete 1.6.61 > /dev/null

# A failed upload that left no asset can also be retried successfully.
echo '[]' > "$check_root/assets.json"
export RETRY_RELEASE_TAG=1.6.60
publish
[[ $(upload_count) == 2 ]]
export GH_MODE=never-created
expect_failure 'was not created within the wait limit'
export GH_MODE=ok RETRY_RELEASE_TAG= GIT_STACK_COMMIT=$commit
export DEBIAN_VERSION='1:1.2~20260904~bookworm.build2' APT_MODE=ok FORBID_GIT_PUSH=false
publish
[[ $(upload_count) == 2 ]]

jq '.[0].digest = "sha256:conflicting"' "$check_root/assets.json" > "$check_root/conflict.json"
mv "$check_root/conflict.json" "$check_root/assets.json"
expect_failure 'Existing deployment.json differs'
printf 'replacement package under the same version' > "$check_root/package.deb"
expect_failure 'another checksum'
export PACKAGE_ARCHITECTURE=arm64
expect_failure 'Downloaded package does not match'
export PACKAGE_ARCHITECTURE=amd64 PACKAGE_VERSION=wrong
expect_failure 'Downloaded package does not match'
export APT_MODE=missing
expect_failure 'Package unavailable'
export GH_MODE=unauthorized
expect_failure 'GitHub authentication check returned HTTP 401'
export GIT_STACK_COMMIT=--all
expect_failure 'Invalid Git commit'
git ls-remote --tags origin > "$check_root/tags.after"
cmp "$check_root/tags.before" "$check_root/tags.after"
[[ $(upload_count) == 2 ]]

export GIT_STACK_COMMIT= GH_TOKEN=
requests_before_skip=$(wc -l < "$check_root/requests.log")
publish
[[ $(wc -l < "$check_root/requests.log") == "$requests_before_skip" && ! -e deployment.json ]]
echo 'PASS: package identity, tag persistence, fresh-checkout retries, conflicts, timeout, and preflight failures'
