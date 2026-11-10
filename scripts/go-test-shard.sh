#!/usr/bin/env bash
set -euo pipefail

case "${1:-}" in
  [1-4]) shard=$1; shift ;;
  *) echo "Usage: $0 <1|2|3|4> [go test flags...]" >&2; exit 2 ;;
esac

module=github.com/cozy/cozy-stack
packages=$(go list ./...)
selected=()
while IFS= read -r package; do
  [[ -n "$package" ]] || continue
  case "$package" in
    "$module"/web/sharings|"$module"/web/sharings/*|"$module"/worker|"$module"/worker/*|"$module"/tests|"$module"/tests/*)
      package_shard=1 ;;
    "$module"/model/instance/storagemigration|"$module"/model/instance/storagemigration/*|"$module"/web|"$module"/web/*)
      package_shard=3 ;;
    "$module"/model|"$module"/model/*)
      package_shard=4 ;;
    *) package_shard=2 ;;
  esac
  if [[ "$package_shard" == "$shard" ]]; then
    selected+=("$package")
  fi
done <<< "$packages"

if [[ ${#selected[@]} -eq 0 ]]; then
  echo "No packages selected for shard $shard" >&2
  exit 1
fi

exec go test -p 1 -timeout 10m "$@" "${selected[@]}"
