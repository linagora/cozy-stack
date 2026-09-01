#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
harness_dir="$(cd "$script_dir/.." && pwd)"
results_dir="$harness_dir/results"

file_size="${FILE_SIZE:-1K}"
base_url="${BASE_URL:-http://mock}"
latency_multiplier="${LATENCY_MULTIPLIER:-2}"
minimum_success_rate="${MIN_SUCCESS_RATE:-0.99}"
confirmation_iterations="${CONFIRMATION_ITERATIONS:-3}"
force_capacity_run="${FORCE_CAPACITY_RUN:-0}"
campaign_id="${CAPACITY_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-upload-capacity-$$}"
attempt_number=0
baseline_iterations=5
minimum_free_disk_bytes=$((5 * 1024 * 1024 * 1024))
report_path="$results_dir/${campaign_id}-upload-capacity.json"
temporary_report=""

cleanup_temporary_report() {
  if [[ -n "$temporary_report" ]] && [[ -f "$temporary_report" ]]; then
    rm -f "$temporary_report" "${temporary_report}.next"
  fi
}

trap cleanup_temporary_report EXIT

get_size_bytes() {
  case "$1" in
    1K) echo 1024 ;;
    100K) echo $((100 * 1024)) ;;
    1M) echo $((1024 * 1024)) ;;
    10M) echo $((10 * 1024 * 1024)) ;;
    100M) echo $((100 * 1024 * 1024)) ;;
    1G) echo $((1024 * 1024 * 1024)) ;;
    *) return 1 ;;
  esac
}

get_default_max_vus() {
  case "$1" in
    1K | 100K) echo 256 ;;
    1M) echo 128 ;;
    10M) echo 32 ;;
    100M) echo 8 ;;
    1G) echo 2 ;;
    *) return 1 ;;
  esac
}

get_file_size() {
  if stat -f '%z' "$1" >/dev/null 2>&1; then
    stat -f '%z' "$1"
  else
    stat -c '%s' "$1"
  fi
}

abort_campaign() {
  local reason="$1"

  if [[ -n "$temporary_report" ]] && [[ -f "$temporary_report" ]]; then
    jq --arg reason "$reason" \
      '.status = "infrastructure_failure" | .failure_reason = $reason' \
      "$temporary_report" > "${temporary_report}.next"
    mv "${temporary_report}.next" "$temporary_report"
    mv "$temporary_report" "$report_path"
    temporary_report=""
    echo "Capacity report: $report_path" >&2
  fi

  echo "Capacity campaign stopped: $reason" >&2
  exit 2
}

validate_inputs() {
  if ! command -v jq >/dev/null 2>&1; then
    echo 'jq is required to run an upload-capacity campaign' >&2
    exit 2
  fi

  if ! size_bytes="$(get_size_bytes "$file_size")"; then
    echo "Unknown FILE_SIZE $file_size. Expected one of: 1K 100K 1M 10M 100M 1G" >&2
    exit 2
  fi

  max_vus="${MAX_VUS:-$(get_default_max_vus "$file_size")}"

  if ! [[ "$max_vus" =~ ^[1-9][0-9]*$ ]]; then
    echo 'MAX_VUS must be a positive integer' >&2
    exit 2
  fi
  if ! [[ "$confirmation_iterations" =~ ^[1-9][0-9]*$ ]]; then
    echo 'CONFIRMATION_ITERATIONS must be a positive integer' >&2
    exit 2
  fi
  if ! jq -en --arg value "$latency_multiplier" \
    '$value | tonumber | isfinite and . > 0' >/dev/null; then
    echo 'LATENCY_MULTIPLIER must be a positive number' >&2
    exit 2
  fi
  if ! jq -en --arg value "$minimum_success_rate" \
    '$value | tonumber | isfinite and . > 0 and . <= 1' >/dev/null; then
    echo 'MIN_SUCCESS_RATE must be greater than 0 and at most 1' >&2
    exit 2
  fi
}

check_resource_guards() {
  local available_disk_bytes
  local docker_memory_bytes
  local fixture_bytes=0
  local fixture_path="$harness_dir/fixtures/$(printf '%s' "$file_size" | tr '[:upper:]' '[:lower:]').bin"
  local projected_disk_bytes
  local projected_memory_bytes

  available_disk_bytes="$(df -Pk "$harness_dir" | awk 'NR == 2 { printf "%.0f", $4 * 1024 }')"

  if [[ -f "$fixture_path" ]] && [[ "$(get_file_size "$fixture_path")" -eq "$size_bytes" ]]; then
    fixture_bytes=0
  else
    fixture_bytes="$size_bytes"
  fi

  projected_disk_bytes=$((
    fixture_bytes + size_bytes * max_vus * confirmation_iterations
  ))

  if [[ "$force_capacity_run" != "1" ]] && \
    (( available_disk_bytes - projected_disk_bytes < minimum_free_disk_bytes )); then
    echo "Projected campaign data would leave less than 5 GiB free." >&2
    echo "Available bytes: $available_disk_bytes; projected bytes: $projected_disk_bytes" >&2
    echo 'Set FORCE_CAPACITY_RUN=1 only after verifying load-generator and Cozy storage.' >&2
    exit 2
  fi

  if ! docker_memory_bytes="$(docker info --format '{{.MemTotal}}' 2>/dev/null)" || \
    ! [[ "$docker_memory_bytes" =~ ^[1-9][0-9]*$ ]]; then
    echo 'Could not determine Docker memory. Is Docker running?' >&2
    exit 2
  fi

  projected_memory_bytes=$((1024 * 1024 * 1024 + 3 * size_bytes * max_vus))

  if [[ "$force_capacity_run" != "1" ]] && \
    (( projected_memory_bytes > docker_memory_bytes )); then
    echo 'Projected k6 memory exceeds the memory available to Docker.' >&2
    echo "Docker bytes: $docker_memory_bytes; projected bytes: $projected_memory_bytes" >&2
    echo 'Set FORCE_CAPACITY_RUN=1 only after verifying the load generator.' >&2
    exit 2
  fi
}

initialize_report() {
  mkdir -p "$results_dir"
  temporary_report="$(mktemp "$results_dir/.${campaign_id}.XXXXXX")"

  jq -n \
    --arg campaign_id "$campaign_id" \
    --arg base_url "$base_url" \
    --arg file_size "$file_size" \
    --argjson size_bytes "$size_bytes" \
    --argjson max_vus "$max_vus" \
    --argjson latency_multiplier "$latency_multiplier" \
    --argjson minimum_success_rate "$minimum_success_rate" \
    --argjson confirmation_iterations "$confirmation_iterations" \
    '{
      campaign_id: $campaign_id,
      target: $base_url,
      file_size: $file_size,
      file_size_bytes: $size_bytes,
      configured_max_vus: $max_vus,
      criteria: {
        minimum_success_rate: $minimum_success_rate,
        maximum_dropped_iterations: 0,
        p95_baseline_multiplier: $latency_multiplier,
        confirmation_iterations_per_vu: $confirmation_iterations
      },
      status: "running",
      attempts: []
    }' > "$temporary_report"
}

append_attempt() {
  local phase="$1"
  local vus="$2"
  local iterations_per_vu="$3"
  local summary_path="$4"
  local runner_exit_code="$5"
  local result="$6"
  local reason="$7"
  local p95_ms="${8:-null}"
  local success_rate="${9:-null}"
  local dropped_iterations="${10:-null}"
  local completed_iterations="${11:-null}"
  local cleanup_rate="${12:-null}"
  local attempt

  attempt="$(jq -n \
    --arg phase "$phase" \
    --argjson vus "$vus" \
    --argjson iterations_per_vu "$iterations_per_vu" \
    --arg summary_path "$summary_path" \
    --argjson runner_exit_code "$runner_exit_code" \
    --arg result "$result" \
    --arg reason "$reason" \
    --argjson p95_ms "$p95_ms" \
    --argjson success_rate "$success_rate" \
    --argjson dropped_iterations "$dropped_iterations" \
    --argjson completed_iterations "$completed_iterations" \
    --argjson cleanup_rate "$cleanup_rate" \
    '{
      phase: $phase,
      vus: $vus,
      iterations_per_vu: $iterations_per_vu,
      p95_ms: $p95_ms,
      success_rate: $success_rate,
      dropped_iterations: $dropped_iterations,
      completed_iterations: $completed_iterations,
      cleanup_rate: $cleanup_rate,
      passed: ($result == "passed"),
      result: $result,
      reason: $reason,
      runner_exit_code: $runner_exit_code,
      summary_path: $summary_path
    }')"

  jq --argjson attempt "$attempt" '.attempts += [$attempt]' \
    "$temporary_report" > "${temporary_report}.next"
  mv "${temporary_report}.next" "$temporary_report"
}

run_capacity_point() {
  local phase="$1"
  local vus="$2"
  local iterations_per_vu="$3"
  local p95_limit_ms="${4:-}"
  local test_id
  local summary_name
  local summary_path
  local runner_exit_code
  local expected_iterations=$((vus * iterations_per_vu))
  local reason=''

  attempt_number=$((attempt_number + 1))
  test_id="${campaign_id}-${phase}-v${vus}-a${attempt_number}"
  summary_name="${test_id}-summary.json"
  summary_path="$results_dir/$summary_name"

  echo
  echo "Running $phase point: $vus VUs x $iterations_per_vu upload(s)"

  if make -C "$harness_dir" upload \
    BASE_URL="$base_url" \
    FILE_SIZE="$file_size" \
    VUS="$vus" \
    ITERATIONS_PER_VU="$iterations_per_vu" \
    CAPACITY_RUN_ID="$campaign_id" \
    CAPACITY_PHASE="$phase" \
    MIN_SUCCESS_RATE="$minimum_success_rate" \
    P95_LIMIT_MS="$p95_limit_ms" \
    TEST_ID="$test_id"; then
    runner_exit_code=0
  else
    runner_exit_code=$?
  fi

  if [[ ! -s "$summary_path" ]]; then
    append_attempt "$phase" "$vus" "$iterations_per_vu" "results/$summary_name" \
      "$runner_exit_code" infrastructure_failure 'k6 produced no summary' \
      null null null null null
    return 2
  fi

  if ! point_p95_ms="$(jq -er \
    '(.metrics["http_req_duration{operation:file-upload}"]["p(95)"] // .metrics.http_req_duration["p(95)"])' \
    "$summary_path")" || \
    ! point_success_rate="$(jq -er \
      '(.metrics["upload_success{operation:file-upload}"].value // .metrics.upload_success.value)' \
      "$summary_path")" || \
    ! point_completed_iterations="$(jq -er '.metrics.iterations.count' "$summary_path")" || \
    ! point_cleanup_rate="$(jq -er '.metrics.cleanup_success.value' "$summary_path")"; then
    append_attempt "$phase" "$vus" "$iterations_per_vu" "results/$summary_name" \
      "$runner_exit_code" infrastructure_failure 'summary is missing required metrics' \
      null null null null null
    return 2
  fi

  point_dropped_iterations="$(jq -r '.metrics.dropped_iterations.count // 0' "$summary_path")"

  if ! jq -en \
    --argjson cleanup_rate "$point_cleanup_rate" \
    '$cleanup_rate == 1' >/dev/null; then
    reason='test-directory cleanup failed'
    append_attempt "$phase" "$vus" "$iterations_per_vu" "results/$summary_name" \
      "$runner_exit_code" infrastructure_failure "$reason" "$point_p95_ms" \
      "$point_success_rate" "$point_dropped_iterations" \
      "$point_completed_iterations" "$point_cleanup_rate"
    return 2
  fi

  if [[ "$point_completed_iterations" -eq 0 ]]; then
    reason='k6 completed no upload iterations'
    append_attempt "$phase" "$vus" "$iterations_per_vu" "results/$summary_name" \
      "$runner_exit_code" infrastructure_failure "$reason" "$point_p95_ms" \
      "$point_success_rate" "$point_dropped_iterations" \
      "$point_completed_iterations" "$point_cleanup_rate"
    return 2
  fi

  if [[ "$point_completed_iterations" -ne "$expected_iterations" ]] || \
    [[ "$point_dropped_iterations" -ne 0 ]]; then
    reason='iterations were dropped or incomplete'
  elif ! jq -en \
    --argjson actual "$point_success_rate" \
    --argjson minimum "$minimum_success_rate" \
    '$actual >= $minimum' >/dev/null; then
    reason='upload success rate is below the minimum'
  elif [[ -n "$p95_limit_ms" ]] && ! jq -en \
    --argjson actual "$point_p95_ms" \
    --argjson maximum "$p95_limit_ms" \
    '$actual <= $maximum' >/dev/null; then
    reason='upload p95 exceeds the baseline multiplier'
  fi

  if [[ -n "$reason" ]]; then
    append_attempt "$phase" "$vus" "$iterations_per_vu" "results/$summary_name" \
      "$runner_exit_code" failed "$reason" "$point_p95_ms" \
      "$point_success_rate" "$point_dropped_iterations" \
      "$point_completed_iterations" "$point_cleanup_rate"
    return 1
  fi

  if [[ "$runner_exit_code" -ne 0 ]]; then
    reason='k6 exited unsuccessfully even though measured criteria passed'
    append_attempt "$phase" "$vus" "$iterations_per_vu" "results/$summary_name" \
      "$runner_exit_code" infrastructure_failure "$reason" "$point_p95_ms" \
      "$point_success_rate" "$point_dropped_iterations" \
      "$point_completed_iterations" "$point_cleanup_rate"
    return 2
  fi

  append_attempt "$phase" "$vus" "$iterations_per_vu" "results/$summary_name" \
    "$runner_exit_code" passed '' "$point_p95_ms" "$point_success_rate" \
    "$point_dropped_iterations" "$point_completed_iterations" "$point_cleanup_rate"
  return 0
}

evaluate_burst_level() {
  local vus="$1"
  local first_result
  local second_result
  local third_result

  if run_capacity_point discovery "$vus" 1 "$p95_limit_ms"; then
    return 0
  else
    first_result=$?
  fi

  if [[ "$first_result" -eq 2 ]]; then
    return 2
  fi

  echo "Retrying failing discovery point at $vus VUs"
  if run_capacity_point discovery-retry "$vus" 1 "$p95_limit_ms"; then
    second_result=0
  else
    second_result=$?
  fi

  if [[ "$second_result" -eq 2 ]]; then
    return 2
  fi
  if [[ "$second_result" -eq "$first_result" ]]; then
    return "$first_result"
  fi

  echo "Running deciding discovery point at $vus VUs"
  if run_capacity_point discovery-decider "$vus" 1 "$p95_limit_ms"; then
    third_result=0
  else
    third_result=$?
  fi

  return "$third_result"
}

evaluate_confirmation_level() {
  local vus="$1"

  run_capacity_point confirmation "$vus" "$confirmation_iterations" "$p95_limit_ms"
}

finish_report() {
  local maximum_concurrency="$1"
  local bound="$2"

  jq \
    --argjson baseline_p95_ms "$baseline_p95_ms" \
    --argjson p95_limit_ms "$p95_limit_ms" \
    --argjson maximum_concurrency "$maximum_concurrency" \
    --arg bound "$bound" \
    '.status = "completed"
      | .baseline_p95_ms = $baseline_p95_ms
      | .p95_limit_ms = $p95_limit_ms
      | .maximum_concurrent_uploads = $maximum_concurrency
      | .result_bound = $bound' \
    "$temporary_report" > "${temporary_report}.next"
  mv "${temporary_report}.next" "$report_path"
  temporary_report=""

  if [[ "$bound" == "lower_bound" ]]; then
    echo
    echo "Result for $file_size: maximum concurrent uploads >= $maximum_concurrency"
  else
    echo
    echo "Result for $file_size: maximum concurrent uploads = $maximum_concurrency"
  fi
  echo "Capacity report: $report_path"
}

validate_inputs
check_resource_guards
initialize_report

echo "Capacity campaign: $campaign_id"
echo "File size: $file_size; maximum search level: $max_vus VUs"

if run_capacity_point baseline 1 "$baseline_iterations" ''; then
  baseline_p95_ms="$point_p95_ms"
else
  baseline_result=$?
  if [[ "$baseline_result" -eq 2 ]]; then
    abort_campaign 'baseline failed because of test infrastructure'
  fi

  baseline_p95_ms="$point_p95_ms"
  p95_limit_ms="$(jq -n \
    --argjson baseline "$baseline_p95_ms" \
    --argjson multiplier "$latency_multiplier" \
    '$baseline * $multiplier')"
  finish_report 0 exact
  exit 1
fi

p95_limit_ms="$(jq -n \
  --argjson baseline "$baseline_p95_ms" \
  --argjson multiplier "$latency_multiplier" \
  '$baseline * $multiplier')"

last_passing_vus=1
first_failing_vus=0
current_vus=2
reached_configured_cap=0

while (( current_vus <= max_vus )); do
  if (( current_vus > max_vus )); then
    current_vus="$max_vus"
  fi

  if evaluate_burst_level "$current_vus"; then
    last_passing_vus="$current_vus"
    if (( current_vus == max_vus )); then
      reached_configured_cap=1
      break
    fi
    current_vus=$((current_vus * 2))
    if (( current_vus > max_vus )); then
      current_vus="$max_vus"
    fi
  else
    burst_result=$?
    if [[ "$burst_result" -eq 2 ]]; then
      abort_campaign "discovery failed because of test infrastructure at $current_vus VUs"
    fi
    first_failing_vus="$current_vus"
    break
  fi
done

if (( first_failing_vus > 0 )); then
  search_low=$((last_passing_vus + 1))
  search_high=$((first_failing_vus - 1))

  while (( search_low <= search_high )); do
    search_mid=$(((search_low + search_high) / 2))
    if evaluate_burst_level "$search_mid"; then
      last_passing_vus="$search_mid"
      search_low=$((search_mid + 1))
    else
      burst_result=$?
      if [[ "$burst_result" -eq 2 ]]; then
        abort_campaign "binary discovery failed because of test infrastructure at $search_mid VUs"
      fi
      search_high=$((search_mid - 1))
    fi
  done
fi

candidate_vus="$last_passing_vus"

if evaluate_confirmation_level "$candidate_vus"; then
  if (( reached_configured_cap == 1 )) && (( candidate_vus == max_vus )); then
    finish_report "$candidate_vus" lower_bound
  else
    finish_report "$candidate_vus" exact
  fi
  exit 0
else
  confirmation_result=$?
  if [[ "$confirmation_result" -eq 2 ]]; then
    abort_campaign "confirmation failed because of test infrastructure at $candidate_vus VUs"
  fi
fi

confirmed_vus=0
search_low=1
search_high=$((candidate_vus - 1))

while (( search_low <= search_high )); do
  search_mid=$(((search_low + search_high) / 2))
  if evaluate_confirmation_level "$search_mid"; then
    confirmed_vus="$search_mid"
    search_low=$((search_mid + 1))
  else
    confirmation_result=$?
    if [[ "$confirmation_result" -eq 2 ]]; then
      abort_campaign "confirmation search failed because of test infrastructure at $search_mid VUs"
    fi
    search_high=$((search_mid - 1))
  fi
done

finish_report "$confirmed_vus" exact

if (( confirmed_vus == 0 )); then
  exit 1
fi
