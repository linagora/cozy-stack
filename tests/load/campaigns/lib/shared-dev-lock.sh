#!/usr/bin/env bash
# Runs a command while holding the runner's shared DEV lock, and exits 2 when another run holds it.
# The command and its children inherit the lock until the last of them exits.
#
# Usage: shared-dev-lock.sh <lock file> <command> [args...]
lock=${1:?usage: shared-dev-lock.sh <lock file> <command> [args...]}
shift
command -v flock > /dev/null || { echo "flock is required to share DEV" >&2; exit 2; }
exec 9>> "$lock" || exit 2
if ! flock -n 9; then
  echo "Shared DEV is busy: another run holds $lock" >&2
  exit 2
fi
exec "$@"
