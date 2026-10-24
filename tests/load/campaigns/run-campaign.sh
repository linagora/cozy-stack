#!/usr/bin/env bash
# Runs the campaign defined in campaigns/<name>.sh and keeps everything about
# the run in data/results/campaigns/<run id>-<name>/: run.log, stats.log,
# environment.txt, steps.tsv, and summary.md.
#
# Exits 0 when every step completed in its declared mode, 1 on a proven
# correctness failure, 2 when the run is inconclusive (README, Campaigns).
#
# Usage: make campaign CAMPAIGN=quick
set -uo pipefail

HARNESS_DIR=$(cd "$(dirname "$0")/.." && pwd)
CAMPAIGN=${1:?usage: run-campaign.sh <campaign>}
DEFINITION="$HARNESS_DIR/campaigns/$CAMPAIGN.sh"

if [ ! -f "$DEFINITION" ] || [ "$CAMPAIGN" = run-campaign ]; then
  echo "Unknown campaign '$CAMPAIGN'. Available: $(cd "$HARNESS_DIR/campaigns" && ls *.sh | grep -v '^run-campaign' | sed 's/\.sh$//' | tr '\n' ' ')" >&2
  exit 2
fi

cd "$HARNESS_DIR"
RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)
RUN_DIR="data/results/campaigns/$RUN_ID-$CAMPAIGN"
mkdir -p "$RUN_DIR"
: > "$RUN_DIR/steps.tsv"
# tee outlives an interruption so the summary still reaches run.log.
exec > >(trap '' INT TERM; exec tee -a "$RUN_DIR/run.log") 2>&1

now() {
  date -u +%Y-%m-%dT%H:%M:%SZ
}

# CPU and memory limits as Docker applies them, after the stack is up.
effective_limits() {
  local service container values entries=()
  for service in cozy-stack couchdb; do
    container=$(docker compose --profile cozy ps -q "$service" 2>/dev/null)
    [ -n "$container" ] || continue
    values=$(docker inspect -f '{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}}' "$container")
    entries+=("$(echo "$values" | awk -v s="$service" '{
      cpus = $1 == 0 ? "unlimited" : $1 / 1e9
      mem = $2 == 0 ? "unlimited" : sprintf("%.1fGiB", $2 / 1073741824)
      printf "%s cpus=%s memory=%s", s, cpus, mem
    }')")
  done
  local IFS=';'
  echo "${entries[*]:-stack not running}"
}

# Container CPU and memory every 10s: shows whether k6 or the stack
# saturated first on a single host.
record_resources() {
  while :; do
    echo "--- $(now) load $(cut -d' ' -f1-3 /proc/loadavg 2>/dev/null || uptime)"
    docker stats --no-stream --format '{{.Name}} cpu={{.CPUPerc}} mem={{.MemUsage}}'
    sleep 10
  done
}

record_resources > "$RUN_DIR/stats.log" 2>&1 &
STATS_PID=$!
FINISHED=
on_exit() {
  local status=$?
  kill "$STATS_PID" 2>/dev/null
  # An abort before the summary (an unset variable in a definition) is
  # inconclusive, never a correctness failure.
  [ -n "$FINISHED" ] || exit 2
  exit "$status"
}
trap on_exit EXIT

# A signal stops the running step's whole process group and skips the
# remaining steps; the summary still runs and reports the run inconclusive.
INTERRUPTED=
trap 'INTERRUPTED=INT' INT
trap 'INTERRUPTED=TERM' TERM

{
  echo "campaign=$CAMPAIGN"
  echo "run_id=$RUN_ID"
  echo "topology=${LOAD_TOPOLOGY:-single-host}"
  echo "commit=$(git rev-parse --short HEAD 2>/dev/null) $(git log -1 --format=%s 2>/dev/null)"
  echo "host=$(hostname) cpus=$(getconf _NPROCESSORS_ONLN 2>/dev/null)"
  echo "configured_limits=COZY_STACK_CPUS=${COZY_STACK_CPUS:-} COZY_STACK_MEMORY=${COZY_STACK_MEMORY:-} COUCHDB_CPUS=${COUCHDB_CPUS:-} COUCHDB_MEMORY=${COUCHDB_MEMORY:-}"
} > "$RUN_DIR/environment.txt"

# Steps keep going after a failure; the definition decides what depends on
# what. Each step is recorded in steps.tsv for the summary, with make's exit
# and the producer's own exit (empty when the producer never ran).
run_step() {
  local name=$1 kind=$2 id=$3 started pid status= code signalled= producer exit_file
  shift 3
  if [ -n "$INTERRUPTED" ]; then
    echo "=== $(now) SKIP $name: campaign interrupted"
    return 2
  fi
  exit_file="$RUN_DIR/$name.exit"
  started=$(now)
  echo "=== $started START $name"
  # Its own process group lets an interruption reach the whole step; no step
  # reads the terminal from the background.
  set -m
  make "$@" RECORD_EXIT="bash campaigns/lib/record-exit.sh $exit_file" < /dev/null &
  pid=$!
  set +m
  # wait also returns when a trapped signal arrives.
  until [ -n "$status" ]; do
    if [ -n "$INTERRUPTED" ] && [ -z "$signalled" ]; then
      # One signal, once the step had time to start: a signal reaching it
      # before make runs is lost, and a second one aborts k6's graceful stop.
      sleep 1
      kill -TERM -- "-$pid" 2>/dev/null
      signalled=1
    fi
    wait "$pid"
    code=$?
    kill -0 "$pid" 2>/dev/null || status=$code
  done
  producer=$(cat "$exit_file" 2>/dev/null)
  rm -f "$exit_file"
  echo "=== $(now) END $name exit=$status${producer:+ producer_exit=$producer}"
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$name" "$kind" "$id" "$started" "$(now)" "$status" "$producer" >> "$RUN_DIR/steps.tsv"
  return "$status"
}

upload_capacity() {
  local name=$1
  shift
  run_step "$name" upload-capacity "$RUN_ID-$name" cozy-capacity CAPACITY_RUN_ID="$RUN_ID-$name" "$@"
}

shared_drive_prepare() {
  run_step shared-drive-prepare prepare '' cozy-shared-drive-prepare "$@"
}

shared_drive_ramp() {
  local name=$1
  shift
  run_step "$name" shared-drive-ramp "$RUN_ID-$name" cozy-shared-drive-ramp EXECUTION_ID="$RUN_ID-$name" "$@"
}

org_drive_fanout() {
  local name=$1
  shift
  run_step "$name" org-drive-fanout "$RUN_ID-$name" org-drive-fanout EXECUTION_ID="$RUN_ID-$name" "$@"
}

echo "Campaign $CAMPAIGN, run $RUN_ID, directory $RUN_DIR"
# shellcheck source=/dev/null
source "$DEFINITION"
campaign

echo "limits=$(effective_limits)" >> "$RUN_DIR/environment.txt"
[ -z "$INTERRUPTED" ] || echo "interrupted=$INTERRUPTED" >> "$RUN_DIR/environment.txt"
./node_modules/.bin/tsx src/scripts/campaign-summary.ts "$RUN_DIR"
outcome=$?
[ -s "$RUN_DIR/summary.md" ] || outcome=2
echo "Campaign directory: $RUN_DIR"
FINISHED=1
case $outcome in
  0 | 1 | 2) exit "$outcome" ;;
  *) exit 2 ;;
esac
