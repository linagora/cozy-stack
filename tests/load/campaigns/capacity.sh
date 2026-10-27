# Every runner's capacity, the highest level it sustains, on the local Stack or on shared DEV
# (make campaign CAMPAIGN=capacity LOAD_TARGET=dev). smoke.sh runs the same steps on short ladders.
# Locally, members is the organization fixture's size: the largest fan-out batch, and at least the
# ramp's writer and reader.
if [ "$CAMPAIGN" = smoke ]; then
  upload=(MAX_VUS=2 BASELINE_ITERATIONS=4 CONFIRMATION_ITERATIONS=2)
  ramp=(RAMP_LEVELS=1,2 RAMP_STEP_DURATION=30s)
  fanout=(FANOUT_LEVELS=1,2)
  members=2
  mixed=(MIXED_RATES=1,2 MIXED_DURATION=30s)
elif [ "${SHARED_DEV:-}" = 1 ]; then
  upload=(BASELINE_ITERATIONS=50 CONFIRMATION_ITERATIONS=20)
  ramp=(RAMP_LEVELS=1,2,4,8,16)
  fanout=(FANOUT_LEVELS=1,2,4,8,16)
else
  # Past the limits seen on the load VM, so each search ends on a failing level.
  upload=(BASELINE_ITERATIONS=50 CONFIRMATION_ITERATIONS=20)
  ramp=(RAMP_LEVELS=1,2,4,8,16,32,64)
  fanout=(FANOUT_LEVELS=1,2,4,8,16,32,64,128)
  members=128
  mixed=(MIXED_RATES=1,2,4,8,16,32)
fi
# Shared DEV budgets, agreed with the DEV owner: raise them only with that agreement. The ladders come
# last, so a smoke ladder stays below them. DEV needs DEV_INSTANCE and the Cloudery token and manifest
# (README, Shared DEV).
if [ "${SHARED_DEV:-}" = 1 ]; then
  upload=(MAX_VUS=128 MAX_UPLOAD_BYTES=2560M MAX_RUNTIME=90m UPLOAD_TIMEOUT=60s "${upload[@]}")
  ramp=(MAX_VUS=96 MAX_UPLOAD_BYTES=20M MAX_RUNTIME=10m "${ramp[@]}")
  fanout=(MAX_RUNTIME=30m "${fanout[@]}")
fi

# A fresh seeded fixture's domain on the bundled Stack.
fixture_domain() {
  echo "seed-$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n').${SEED_DOMAIN_SUFFIX:-localhost:8080}"
}

campaign() {
  if [ "${SHARED_DEV:-}" = 1 ]; then
    upload_capacity upload-capacity FILE_SIZE=100K "${upload[@]}"
    shared_drive_ramp shared-drive-ramp "${ramp[@]}"
    org_drive_fanout org-drive-fanout "${fanout[@]}"
    return
  fi
  # Locally, every step uses the bundled Stack and the instance and token this writes to .env.
  run_step cozy-prepare prepare '' cozy-prepare || return
  upload_capacity upload-capacity FILE_SIZE=100K "${upload[@]}"
  # One organization fixture: the ramp's writer and reader members, and the fan-out's recipients.
  local org tree
  org=$(fixture_domain)
  echo "Organization fixture: $org (make seed-org-cleanup SEED_DOMAIN=$org removes it)"
  run_step seed-org prepare '' seed-org SEED_DOMAIN="$org" SEED_USERS="$members"
  shared_drive_ramp shared-drive-ramp ACTORS_DOMAIN="$org" "${ramp[@]}"
  org_drive_fanout org-drive-fanout FANOUT_ORG_DOMAIN="$org" "${fanout[@]}"
  run_step seed-org-cleanup cleanup '' seed-org-cleanup SEED_DOMAIN="$org"
  # The mixed workload reads and writes its own tree fixture; shared DEV has no tree fixture yet.
  tree=$(fixture_domain)
  echo "Tree fixture: $tree (make seed-scale-cleanup SEED_DOMAIN=$tree removes it)"
  run_step seed-tree prepare '' seed-scale SEED_PROFILE=tree SEED_DOMAIN="$tree"
  mixed_capacity mixed-capacity MIXED_DOMAIN="$tree" "${mixed[@]}"
  run_step seed-tree-cleanup cleanup '' seed-scale-cleanup SEED_DOMAIN="$tree"
}
