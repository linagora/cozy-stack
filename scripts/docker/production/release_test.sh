#!/bin/bash
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/release.sh"
test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT
export DOCKER_CALLS="$test_dir/docker-calls"
export GITHUB_OUTPUT="$test_dir/github-output"

cat > "$test_dir/git" <<'EOF'
#!/bin/bash
case "$*" in
  'describe --tags') echo 1.2.3 ;;
  *) exit 1 ;;
esac
EOF
cat > "$test_dir/docker" <<'EOF'
#!/bin/bash
printf '%s\n' "$*" >> "$DOCKER_CALLS"
test "${FAIL_BUILD:-false}" != true || test "$1" != build
EOF
chmod +x "$test_dir/git" "$test_dir/docker"
export PATH="$test_dir:$PATH"

bash "$script" > "$test_dir/output" 2>&1
cat > "$test_dir/expected" <<'EOF'
build -t cozy/cozy-stack:1.2.3 -f scripts/docker/production/Dockerfile .
push cozy/cozy-stack:1.2.3
tag cozy/cozy-stack:1.2.3 cozy/cozy-stack:latest
push cozy/cozy-stack:latest
EOF
diff -u "$test_dir/expected" "$DOCKER_CALLS"
: > "$GITHUB_OUTPUT"
: > "$DOCKER_CALLS"
bash "$script" --pr 5119 'Chore/VFS locks' > "$test_dir/output" 2>&1
cat > "$test_dir/expected" <<'EOF'
build -t cozy/cozy-stack:e2e-pr5119-chore-vfs-locks -f scripts/docker/production/Dockerfile .
push cozy/cozy-stack:e2e-pr5119-chore-vfs-locks
EOF
diff -u "$test_dir/expected" "$DOCKER_CALLS"
test "$(cat "$GITHUB_OUTPUT")" = 'image=cozy/cozy-stack:e2e-pr5119-chore-vfs-locks'

: > "$DOCKER_CALLS"
bash "$script" --pr 5119 "$(printf 'x%.0s' {1..200})" > "$test_dir/output" 2>&1
tag=$(sed -n 's/^push cozy\/cozy-stack://p' "$DOCKER_CALLS")
test "${#tag}" = 128
: > "$DOCKER_CALLS"
bash "$script" --pr 5119 '///' > "$test_dir/output" 2>&1
grep -q '^push cozy/cozy-stack:e2e-pr5119-branch$' "$DOCKER_CALLS"

for mode in release pr; do
  args=()
  if [[ "$mode" == pr ]]; then args+=(--pr 5119 branch); fi
  : > "$DOCKER_CALLS"
  if FAIL_BUILD=true bash "$script" "${args[@]}" > "$test_dir/output" 2>&1; then
    echo 'A failed build must fail the script' >&2
    exit 1
  fi
  test "$(wc -l < "$DOCKER_CALLS" | tr -d ' ')" = 1
  if grep -q '^push ' "$DOCKER_CALLS"; then exit 1; fi
done

for arg in --invalid ''; do
  : > "$DOCKER_CALLS"
  if bash "$script" "$arg" > "$test_dir/output" 2>&1; then exit 1; fi
  test ! -s "$DOCKER_CALLS"
done
for number in 0 -1 '12/34' 0001; do
  : > "$DOCKER_CALLS"
  if bash "$script" --pr "$number" branch > "$test_dir/output" 2>&1; then exit 1; fi
  test ! -s "$DOCKER_CALLS"
done

echo 'Release publishing, PR tag validation and failure checks passed'
