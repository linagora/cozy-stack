#!/usr/bin/env bash
# Runs a command while holding the runner's shared DEV lock, and exits 2 when another run holds it.
# The command and its children inherit the lock until the last of them exits; a shared DEV campaign
# (run-campaign.sh) takes it once for all its steps.
#
# Usage: shared-dev-lock.sh <lock file> <command> [args...]
lock=${1:?usage: shared-dev-lock.sh <lock file> <command> [args...]}
shift
# Inside a shared DEV campaign, the campaign holds this lock on descriptor 9, which its steps inherit.
if [ "${DEV_LOCK_HELD:-}" = "$lock" ] && [ /dev/fd/9 -ef "$lock" ] && flock -n 9 2> /dev/null; then
  exec "$@"
fi
command -v flock > /dev/null || { echo "flock is required to share DEV" >&2; exit 2; }
exec 9>> "$lock" || exit 2
if ! flock -n 9; then
  echo "Shared DEV is busy: another run holds $lock" >&2
  exit 2
fi
exec "$@"
