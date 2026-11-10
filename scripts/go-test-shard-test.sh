#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
export SHARD_TEST_DIR="$tmp"
export PATH="$tmp:$PATH"

cat > "$tmp/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  list)
    [[ "$#" == 2 && "$2" == ./... ]]
    cat "$SHARD_TEST_DIR/packages"
    exit "${SHARD_LIST_EXIT:-0}"
    ;;
  test)
    printf '%s\n' "$@" > "$SHARD_TEST_DIR/args"
    exit "${SHARD_TEST_EXIT:-0}"
    ;;
  *) exit 99 ;;
esac
EOF
chmod +x "$tmp/go"

module=github.com/cozy/cozy-stack
cat > "$tmp/assignments" <<EOF
2 $module
2 $module/client
2 $module/cmd
2 $module/pkg/rabbitmq
2 $module/future/package
2 $module/webinar
1 $module/web/sharings
1 $module/web/sharings/future
1 $module/worker
1 $module/worker/mails
1 $module/tests
1 $module/tests/testutils
3 $module/model/instance/storagemigration
3 $module/model/instance/storagemigration/future
3 $module/web
3 $module/web/settings
4 $module/model
4 $module/model/rag
4 $module/model/instance
4 $module/model/instance/storagemigrationextra
EOF
cut -d ' ' -f 2 "$tmp/assignments" > "$tmp/packages"

for shard in 1 2 3 4; do
  "$script_dir/go-test-shard.sh" "$shard" -json -count=1 -run 'Test name with spaces'
  printf '%s\n' test -p 1 -timeout 10m -json -count=1 -run 'Test name with spaces' > "$tmp/expected"
  awk -v shard="$shard" '$1 == shard { print $2 }' "$tmp/assignments" >> "$tmp/expected"
  diff -u "$tmp/expected" "$tmp/args"
  tail -n +10 "$tmp/args" >> "$tmp/selected"
done
sort "$tmp/packages" > "$tmp/all-sorted"
sort "$tmp/selected" > "$tmp/selected-sorted"
diff -u "$tmp/all-sorted" "$tmp/selected-sorted"

expect_failure() {
  local expected=$1 actual=0
  shift
  "$script_dir/go-test-shard.sh" "$@" > "$tmp/output" 2>&1 || actual=$?
  if [[ "$actual" != "$expected" ]]; then
    echo "Expected exit $expected, got $actual: $*" >&2
    cat "$tmp/output" >&2
    exit 1
  fi
}

SHARD_TEST_EXIT=23 expect_failure 23 1
rm "$tmp/args"
expect_failure 2
expect_failure 2 0
expect_failure 2 5
expect_failure 2 invalid
SHARD_LIST_EXIT=17 expect_failure 17 1
test ! -e "$tmp/args"
echo "$module" > "$tmp/packages"
expect_failure 1 1
test ! -e "$tmp/args"

echo 'Go test shard checks passed'
