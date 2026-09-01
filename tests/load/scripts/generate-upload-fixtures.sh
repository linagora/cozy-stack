#!/usr/bin/env bash

set -euo pipefail

fixture_dir="${FIXTURE_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/fixtures}"
requested_size="${1:-all}"

fixture_labels=(1K 100K 1M 10M 100M 1G)

get_fixture_block_size() {
  case "$1" in
    1K | 100K) echo 1024 ;;
    1M | 10M | 100M | 1G) echo 1048576 ;;
    *) return 1 ;;
  esac
}

get_fixture_block_count() {
  case "$1" in
    1K) echo 1 ;;
    100K) echo 100 ;;
    1M) echo 1 ;;
    10M) echo 10 ;;
    100M) echo 100 ;;
    1G) echo 1024 ;;
    *) return 1 ;;
  esac
}

get_fixture_filename() {
  echo "$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]').bin"
}

get_file_size() {
  if stat -f '%z' "$1" >/dev/null 2>&1; then
    stat -f '%z' "$1"
  else
    stat -c '%s' "$1"
  fi
}

generate_fixture() {
  local label="$1"
  local block_size
  local block_count
  local expected_size
  local fixture_path
  local temporary_path

  block_size="$(get_fixture_block_size "$label")"
  block_count="$(get_fixture_block_count "$label")"
  expected_size=$((block_size * block_count))
  fixture_path="$fixture_dir/$(get_fixture_filename "$label")"

  if [[ -f "$fixture_path" ]] && [[ "$(get_file_size "$fixture_path")" -eq "$expected_size" ]]; then
    echo "Fixture $label already exists: $fixture_path"
    return
  fi

  temporary_path="${fixture_path}.tmp.$$"
  trap 'rm -f "$temporary_path"' EXIT
  echo "Generating $label upload fixture: $fixture_path"
  dd if=/dev/urandom of="$temporary_path" bs="$block_size" count="$block_count" status=none
  mv "$temporary_path" "$fixture_path"
  trap - EXIT
}

mkdir -p "$fixture_dir"

if [[ "$requested_size" == "all" ]]; then
  for label in "${fixture_labels[@]}"; do
    generate_fixture "$label"
  done
  exit 0
fi

for label in "${fixture_labels[@]}"; do
  if [[ "$requested_size" == "$label" ]]; then
    generate_fixture "$label"
    exit 0
  fi
done

echo "Unknown fixture size: $requested_size. Expected one of: ${fixture_labels[*]}" >&2
exit 1
