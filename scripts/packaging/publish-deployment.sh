#!/bin/bash
set -euo pipefail
set +x

fail() {
  echo "Release publication failed: $*" >&2
  exit 1
}

validate_manifest() {
  jq -se '
    length == 1 and (.[0] |
      type == "object" and
      keys == ["debian_version", "git_commit", "package_sha256", "release_tag"] and
      all(.[]; type == "string") and
      (.release_tag | test("\\A[0-9]+\\.[0-9]+\\.[0-9]+\\z")) and
      (.git_commit | test("\\A[0-9a-f]{40}\\z")) and
      (.debian_version | test("\\A([0-9]+:)?[0-9][0-9A-Za-z.+:~-]*~bookworm\\.build[0-9]+\\z")) and
      (.package_sha256 | test("\\A[0-9a-f]{64}\\z")))
  ' "$1" > /dev/null || fail "Invalid deployment manifest"
}

read_tag_manifest() {
  local tag=$1
  [[ $tag =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "Invalid release tag"
  [[ $(git cat-file -t "refs/tags/$tag") == tag ]] || fail "Tag has no deployment annotation"
  git for-each-ref --format='%(contents)' "refs/tags/$tag" > "$release_workdir/tag.json"
  validate_manifest "$release_workdir/tag.json"
  [[ $(jq -r .release_tag "$release_workdir/tag.json") == "$tag" &&
     $(jq -r .git_commit "$release_workdir/tag.json") == "$(git rev-parse "refs/tags/$tag^{commit}")" ]] ||
    fail "Tag and deployment manifest disagree"
}

github() {
  http_status=$(curl --silent --show-error --connect-timeout 10 --max-time 30 \
    --proto '=https' --header @- \
    --header 'Accept: application/vnd.github+json' \
    --header 'Content-Type: application/json' \
    --header 'X-GitHub-Api-Version: 2022-11-28' \
    --output "$release_workdir/response.json" --write-out '%{http_code}' \
    "$@" <<< "Authorization: Bearer $GH_TOKEN")
}

cleanup() {
  local status=$?
  rm -rf -- "$release_workdir"
  if (( status != 0 )) && [[ -s deployment.json ]]; then
    echo "Manifest preserved. If its tag was pushed, retry with RETRY_RELEASE_TAG=$(jq -r .release_tag deployment.json)." >&2
  fi
  exit "$status"
}

rm -f -- deployment.json
retry_tag=${RETRY_RELEASE_TAG:-}
git_commit=${GIT_STACK_COMMIT:-}
debian_version=${DEBIAN_VERSION:-}
if [[ -z $retry_tag && -z $git_commit ]]; then
  echo "No commit id in state file, skipping release"
  exit 0
fi
if [[ -z $retry_tag ]]; then
  [[ $git_commit =~ ^[0-9a-f]{40}$ ]] || fail "Invalid Git commit"
  [[ $debian_version =~ ^([0-9]+:)?[0-9][0-9A-Za-z.+:~-]*~bookworm\.build[0-9]+$ ]] ||
    fail "Invalid Debian version"
else
  [[ $retry_tag =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "Invalid release tag"
fi
: "${GH_TOKEN:?Bind a GitHub release upload credential to GH_TOKEN}"
release_workdir=$(mktemp -d)
trap cleanup EXIT
github_api=https://api.github.com/repos/linagora/cozy-stack
github "$github_api"
[[ $http_status == 200 ]] || fail "GitHub authentication check returned HTTP $http_status"
git fetch origin --tags

if [[ -n $retry_tag ]]; then
  read_tag_manifest "$retry_tag"
  jq . "$release_workdir/tag.json" > "$release_workdir/deployment.json"
else
  [[ $(git rev-parse "$git_commit^{commit}") == "$git_commit" ]] || fail "Invalid deployment commit"

  # A private cache uses the agent's trusted keys without changing its APT sources.
  mkdir -p "$release_workdir/lists/partial" "$release_workdir/cache/archives/partial"
  echo 'deb [arch=amd64] http://apt.int.cozycloud.cc/ bookworm prod' > "$release_workdir/sources.list"
  apt_options=(
    -o "Dir::Etc::sourcelist=$release_workdir/sources.list"
    -o 'Dir::Etc::sourceparts=-'
    -o "Dir::State::lists=$release_workdir/lists"
    -o "Dir::Cache=$release_workdir/cache"
    -o 'APT::Architecture=amd64'
    -o 'Acquire::AllowInsecureRepositories=false'
    -o 'APT::Get::AllowUnauthenticated=false'
  )
  (
    cd "$release_workdir"
    apt-get "${apt_options[@]}" --error-on=any update
    apt-get "${apt_options[@]}" download "cozy-stack:amd64=$debian_version"
  )
  shopt -s nullglob
  packages=("$release_workdir"/*.deb)
  (( ${#packages[@]} == 1 )) || fail "Expected exactly one original Debian package"
  [[ $(dpkg-deb --field "${packages[0]}" Package Version Architecture) == \
     "$(printf 'Package: cozy-stack\nVersion: %s\nArchitecture: amd64' "$debian_version")" ]] ||
    fail "Downloaded package does not match the deployment record"
  package_sha256=$(sha256sum "${packages[0]}" | cut -d ' ' -f 1)

  # The annotation survives workspace cleanup, so a repeated build reuses its tag.
  release_tag=
  commit_tags=$(git tag --points-at "$git_commit")
  while IFS= read -r tag; do
    [[ $tag =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || continue
    tag_type=$(git cat-file -t "refs/tags/$tag")
    [[ $tag_type == tag ]] || continue
    annotation=$(git for-each-ref --format='%(contents)' "refs/tags/$tag")
    [[ $annotation == \{* ]] || continue
    read_tag_manifest "$tag"
    if [[ $(jq -r .debian_version "$release_workdir/tag.json") == "$debian_version" ]]; then
      [[ $(jq -r .package_sha256 "$release_workdir/tag.json") == "$package_sha256" ]] ||
        fail "An existing release maps this package version to another checksum"
      release_tag=$tag
      break
    fi
  done <<< "$commit_tags"

  if [[ -z $release_tag ]]; then
    latest_tag=
    tags=$(git tag --list --sort=version:refname)
    while IFS= read -r tag; do
      if [[ $tag =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        latest_tag=$tag
      fi
    done <<< "$tags"
    [[ -n $latest_tag ]] || fail "No existing release version to increment"
    IFS=. read -r major minor patch <<< "$latest_tag"
    [[ ${#patch} -le 9 ]] || fail "Release patch number is too large"
    release_tag="$major.$minor.$((10#$patch + 1))"
  fi
  jq -n --arg release_tag "$release_tag" --arg git_commit "$git_commit" \
    --arg debian_version "$debian_version" --arg package_sha256 "$package_sha256" \
    '{release_tag: $release_tag, git_commit: $git_commit,
      debian_version: $debian_version, package_sha256: $package_sha256}' > "$release_workdir/deployment.json"
  validate_manifest "$release_workdir/deployment.json"
fi
mv -- "$release_workdir/deployment.json" deployment.json
release_tag=$(jq -r .release_tag deployment.json)
echo "Release $release_tag: $(jq -r .debian_version deployment.json) at $(jq -r .git_commit deployment.json)"
if [[ -z $retry_tag ]]; then
  if [[ -z $(git tag --list "$release_tag") ]]; then
    git tag --annotate "$release_tag" "$git_commit" --file deployment.json
  fi
  git push --no-follow-tags origin "refs/tags/$release_tag:refs/tags/$release_tag"
fi
remote_tag=$(git ls-remote --exit-code origin "refs/tags/$release_tag")
read -r remote_object remote_ref <<< "$remote_tag"
[[ $remote_ref == "refs/tags/$release_tag" && $remote_object == "$(git rev-parse "refs/tags/$release_tag")" ]] ||
  fail "Remote release tag differs from the selected local tag"

for attempt in {1..60}; do
  github "$github_api/releases/tags/$release_tag"
  case $http_status in
    200) break ;;
    404)
      (( attempt < 60 )) || fail "GitHub release $release_tag was not created within the wait limit"
      sleep 10
      ;;
    *) fail "GitHub release lookup returned HTTP $http_status" ;;
  esac
done
jq -e --arg tag "$release_tag" '
  .tag_name == $tag and .draft == false and (.assets | type == "array") and
  (.id | type == "number" and . > 0 and . == floor)
' "$release_workdir/response.json" > /dev/null || fail "Expected a published release for the selected tag"
digest="sha256:$(sha256sum deployment.json | cut -d ' ' -f 1)"
if jq -e 'any(.assets[]; .name == "deployment.json")' "$release_workdir/response.json" > /dev/null; then
  jq -e --arg digest "$digest" '
    [.assets[] | select(.name == "deployment.json")] |
    length == 1 and .[0].digest == $digest and .[0].state == "uploaded"
  ' "$release_workdir/response.json" > /dev/null ||
    fail "Existing deployment.json differs or cannot be verified; refusing to replace it"
  echo "Release $release_tag already has the matching deployment.json"
  exit 0
fi
release_id=$(jq -r .id "$release_workdir/response.json")
github "https://uploads.github.com/repos/linagora/cozy-stack/releases/$release_id/assets?name=deployment.json" \
  --data-binary @deployment.json
[[ $http_status == 201 ]] || fail "Manifest upload returned HTTP $http_status"
jq -e --arg digest "$digest" '
  .name == "deployment.json" and .digest == $digest and .state == "uploaded"
' "$release_workdir/response.json" > /dev/null || fail "GitHub did not confirm the uploaded manifest checksum"
echo "Published deployment.json for release $release_tag"
