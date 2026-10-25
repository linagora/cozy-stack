# Shared DEV capacity: upload, shared-drive activity and group fan-out capacity on the
# Cloudery fixture, under one shared DEV lock. The budgets are those agreed with the DEV owner; raise
# them only with that agreement. Needs DEV_INSTANCE and the Cloudery token and manifest (README, Shared DEV).
SHARED_DEV=1
campaign() {
  upload_capacity upload-capacity-1M FILE_SIZE=1M MAX_VUS=128 MAX_UPLOAD_BYTES=2560M MAX_RUNTIME=90m \
    MIN_SUCCESS_RATE=0.99 UPLOAD_TIMEOUT=60s || return $?
  shared_drive_ramp shared-drive-ramp RAMP_LEVELS=1,2,4,8,16 MAX_VUS=96 MAX_UPLOAD_BYTES=20M MAX_RUNTIME=10m || return $?
  org_drive_fanout org-drive-fanout FANOUT_LEVELS=1,2,4,8,16 MAX_RUNTIME=30m
}
