# Every runner's capacity, the highest level it sustains, on the local Stack or on shared DEV
# (make campaign CAMPAIGN=capacity LOAD_TARGET=dev). smoke.sh runs the same steps on short ladders.
if [ "$CAMPAIGN" = smoke ]; then
  upload=(MAX_VUS=2 BASELINE_ITERATIONS=4 CONFIRMATION_ITERATIONS=2)
  ramp=(RAMP_LEVELS=1,2 RAMP_STEP_DURATION=30s)
  fanout=(FANOUT_LEVELS=1,2)
else
  upload=(BASELINE_ITERATIONS=50 CONFIRMATION_ITERATIONS=20)
  ramp=(RAMP_LEVELS=1,2,4,8,16)
  fanout=(FANOUT_LEVELS=1,2,4,8,16)
fi
# Shared DEV budgets, agreed with the DEV owner: raise them only with that agreement. The ladders come
# last, so a smoke ladder stays below them. DEV needs DEV_INSTANCE and the Cloudery token and manifest
# (README, Shared DEV).
if [ "${SHARED_DEV:-}" = 1 ]; then
  upload=(MAX_VUS=128 MAX_UPLOAD_BYTES=2560M MAX_RUNTIME=90m UPLOAD_TIMEOUT=60s "${upload[@]}")
  ramp=(MAX_VUS=96 MAX_UPLOAD_BYTES=20M MAX_RUNTIME=10m "${ramp[@]}")
  fanout=(MAX_RUNTIME=30m "${fanout[@]}")
fi

campaign() {
  # Locally, every step uses the bundled Stack and the instance and token this writes to .env.
  if [ "${SHARED_DEV:-}" != 1 ]; then
    run_step cozy-prepare prepare '' cozy-prepare || return
  fi
  upload_capacity upload-capacity FILE_SIZE=100K "${upload[@]}"
  shared_drive_ramp shared-drive-ramp "${ramp[@]}"
  # The fan-out needs an organization: shared DEV has the Cloudery one, the local Stack none yet.
  if [ "${SHARED_DEV:-}" = 1 ]; then
    org_drive_fanout org-drive-fanout "${fanout[@]}"
  fi
}
