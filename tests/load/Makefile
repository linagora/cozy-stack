SHELL := /bin/bash
.DEFAULT_GOAL := help
-include .env

# Keep unset optional values unset so the Node CLI can apply its defaults.
SEED_SETTINGS := SEED_ADMIN_URL SEED_STACK_URL SEED_ADMIN_USERNAME SEED_ADMIN_PASSWORD SEED_DOMAIN_SUFFIX \
                 SEED_DOMAIN SEED_USERS SEED_FILES SEED_DRIVES SEED_CONTEXT_NAME SEED_REQUEST_TIMEOUT_MS
$(foreach setting,$(SEED_SETTINGS),$(if $(filter undefined,$(origin $(setting))),,$(eval export $(setting))))

FANOUT_SETTINGS := FANOUT_ORG_DOMAIN FANOUT_LEVELS FANOUT_TIMEOUT_MS FANOUT_POLL_MS \
                   FANOUT_POLL_CONCURRENCY FANOUT_STABLE_MS FANOUT_REPORT
$(foreach setting,$(FANOUT_SETTINGS),$(if $(filter undefined,$(origin $(setting))),,$(eval export $(setting))))

FIXTURE_PROJECT ?=
FIXTURE_SNAPSHOT ?=
FIXTURE_STACK_PORT ?= 18090
FIXTURE_ADMIN_PORT ?= 16090
export FIXTURE_PROJECT FIXTURE_SNAPSHOT FIXTURE_STACK_PORT FIXTURE_ADMIN_PORT

COMPOSE := docker compose
COZY_COMPOSE := $(COMPOSE) --profile cozy
NODE_BIN := ./node_modules/.bin
NODE_MODULES_MARKER := node_modules/.load-ready
BASE_URL ?= http://mock
VUS ?= 1
FILE_SIZE ?= 1K
UPLOAD_MODE ?= iterations
ITERATIONS_PER_VU ?= 1
DURATION ?= 10m
GRACEFUL_STOP ?= 30m
MAX_DURATION ?= 30m
UPLOAD_TIMEOUT ?= 30m
CONSISTENCY_CHUNK_SIZE_BYTES ?= 16777216
UPLOAD_TARGET ?= personal
SHARED_DRIVE_ID ?=
SHARED_DRIVE_ROOT_ID ?=
EXECUTION_ID ?= $(shell date -u +%Y%m%dT%H%M%SZ)
CAPACITY_RUN_ID ?=
CAPACITY_PHASE ?= single-run
P95_LIMIT_MS ?=
MAX_VUS ?=
MIN_DELIVERY_RATIO ?= 0.9
MIN_SUCCESS_RATE ?= 0.99
BASELINE_ITERATIONS ?= 50
CONFIRMATION_ITERATIONS ?= 20
FORCE_CAPACITY_RUN ?= 0
COZY_INSTANCE_DOMAIN ?= load.localhost:8080
COZY_BASE_URL ?= http://$(COZY_INSTANCE_DOMAIN)
COZY_CLIENT_SOFTWARE_ID ?= twake-drive-load-local
COZY_TOKEN_EXPIRATION ?= 24h
HOST_UID ?= $(shell id -u)
HOST_GID ?= $(shell id -g)
# Recorded with every campaign and tagged on k6 metrics: k6 and the stack
# share one host unless a separate load generator is used.
LOAD_TOPOLOGY ?= single-host
GRAFANA_URL ?= http://localhost:$(or $(GRAFANA_PORT),3000)
CAMPAIGN ?= quick
# Campaign steps set a command prefix that records a producer's own exit status:
# make reports every failed recipe as 2.
RECORD_EXIT ?=

export HOST_UID
export HOST_GID
# Compose reads the optional resource limits from the environment.
export COZY_STACK_CPUS COZY_STACK_MEMORY COUCHDB_CPUS COUCHDB_MEMORY
export LOAD_TOPOLOGY GRAFANA_URL

.PHONY: help doctor install typecheck config pull observability-up up cozy-up cozy-prepare cozy-shared-drive-prepare cozy-shared-drive-load cozy-shared-drive-ramp campaign cozy-upload cozy-upload-soak cozy-capacity cozy-capacity-all cozy-status cozy-logs cozy-down upload-binaries upload-binary upload-smoke upload upload-soak upload-capacity upload-capacity-all status logs down dashboard-url

help:
	@echo "make install                                 Install the load-harness Node tools"
	@echo "make typecheck                               Typecheck k6 and Node TypeScript"
	@echo "make seed-scale SEED_FILES=1000 SEED_DRIVES=10  Build a disposable fixture through Stack APIs"
	@echo "make seed-scale-cleanup SEED_DOMAIN=...        Delete that disposable fixture instance"
	@echo "make seed-org SEED_USERS=50                    Build an organization with empty user instances"
	@echo "make seed-org-cleanup SEED_DOMAIN=...          Delete the organization fixture and its users"
	@echo "make org-drive-fanout FANOUT_LEVELS=10,50,100   Check group auto-accept and revoke convergence"
	@echo "make dev-upload DEV_INSTANCE=... VUS=2 ITERATIONS_PER_VU=5 FILE_SIZE=1M  Bounded uploads on shared DEV"
	@echo "make fixture-up FIXTURE_PROJECT=load-fixture-seed"
	@echo "make fixture-snapshot FIXTURE_PROJECT=... FIXTURE_SNAPSHOT=/private/baseline"
	@echo "make fixture-restore FIXTURE_PROJECT=... FIXTURE_SNAPSHOT=/private/baseline"
	@echo "make fixture-discard FIXTURE_PROJECT=...       Remove only a marked disposable deployment"
	@echo "make capacity-dashboard-url                  Open the Stack Capacity Overview"
	@echo "make up                                      Start dashboards and the local mock"
	@echo "make upload-binaries                         Generate every upload binary"
	@echo "make upload-smoke                            Run two concurrent 1K mock uploads"
	@echo "make cozy-up                                 Build and start the local Cozy services"
	@echo "make cozy-prepare                            Prepare the local Cozy test instance"
	@echo "make cozy-shared-drive-prepare               Prepare a local shared drive for load testing"
	@echo "make cozy-shared-drive-load                  Run the multi-scenario shared drive load test"
	@echo "make cozy-shared-drive-ramp                  Find the highest sustained shared drive level"
	@echo "make campaign CAMPAIGN=quick                 Run campaigns/<name>.sh and summarize it"
	@echo "make cozy-upload FILE_SIZE=1K VUS=1          Prepare and test the local Cozy stack"
	@echo "make cozy-upload-soak FILE_SIZE=1M VUS=10 DURATION=10m"
	@echo "make cozy-capacity FILE_SIZE=1K MAX_VUS=8    Measure the local Cozy stack"
	@echo "make upload BASE_URL=https://... FILE_SIZE=1M VUS=10"
	@echo "make upload-soak BASE_URL=https://... FILE_SIZE=1M VUS=10 DURATION=10m"
	@echo "make upload UPLOAD_TARGET=shared-drive SHARED_DRIVE_ID=... SHARED_DRIVE_ROOT_ID=..."
	@echo "make upload-capacity BASE_URL=https://... FILE_SIZE=10M"
	@echo "make upload-capacity-all BASE_URL=https://..."
	@echo "make status | make logs | make down"

doctor:
	@docker info >/dev/null
	@$(COMPOSE) version
	@node --version
	@npm --version

$(NODE_MODULES_MARKER): package.json package-lock.json
	@npm ci
	@touch "$@"

install: $(NODE_MODULES_MARKER)

.PHONY: seed-scale seed-scale-cleanup
seed-scale: $(NODE_MODULES_MARKER)
	@$(NODE_BIN)/tsx src/scripts/seed-scale-data.ts seed $${SEED_DOMAIN:+"$$SEED_DOMAIN"}

seed-scale-cleanup: $(NODE_MODULES_MARKER)
	@$(NODE_BIN)/tsx src/scripts/seed-scale-data.ts cleanup "$$SEED_DOMAIN"

.PHONY: seed-org seed-org-cleanup
# A Cloudery token file seeds through the Cloudery partner API instead (SEEDING.md).
seed-org: $(NODE_MODULES_MARKER)
ifdef SEED_CLOUDERY_TOKEN_FILE
	@$(NODE_BIN)/tsx src/scripts/seed-cloudery.ts seed
else
	@$(NODE_BIN)/tsx src/scripts/seed-scale-data.ts seed-org $${SEED_DOMAIN:+"$$SEED_DOMAIN"}
endif

seed-org-cleanup: $(NODE_MODULES_MARKER)
ifdef SEED_CLOUDERY_TOKEN_FILE
	@$(NODE_BIN)/tsx src/scripts/seed-cloudery.ts teardown
else
	@$(NODE_BIN)/tsx src/scripts/seed-scale-data.ts cleanup-org "$$SEED_DOMAIN"
endif

.PHONY: org-drive-fanout
# Defaulted here only: a global default would also be exported by `make campaign`, under the
# campaign's EXECUTION_ID, and every fanout step would write to that one path.
org-drive-fanout: export FANOUT_REPORT ?= data/results/$(EXECUTION_ID)-org-drive-fanout.json
org-drive-fanout: $(NODE_MODULES_MARKER)
	@$(RECORD_EXIT) $(NODE_BIN)/tsx src/scripts/org-drive-fanout.ts

.PHONY: dev-upload
# One shared DEV run at a time: every checkout, user and container on the runner locks this path.
DEV_LOCK_FILE ?= /tmp/cozy-load-shared-dev.lock
dev-upload: $(NODE_MODULES_MARKER)
	@VUS="$(VUS)" ITERATIONS_PER_VU="$(ITERATIONS_PER_VU)" FILE_SIZE="$(FILE_SIZE)" EXECUTION_ID="$(EXECUTION_ID)" \
		bash campaigns/lib/shared-dev-lock.sh "$(DEV_LOCK_FILE)" $(NODE_BIN)/tsx src/scripts/dev-upload.ts

.PHONY: fixture-up fixture-snapshot fixture-restore fixture-discard
fixture-up fixture-discard: $(NODE_MODULES_MARKER)
	@$(NODE_BIN)/tsx src/scripts/compose-fixture.ts $(patsubst fixture-%,%,$@) "$$FIXTURE_PROJECT"

fixture-snapshot fixture-restore: $(NODE_MODULES_MARKER)
	@$(NODE_BIN)/tsx src/scripts/compose-fixture.ts $(patsubst fixture-%,%,$@) "$$FIXTURE_PROJECT" \
		"$$FIXTURE_SNAPSHOT"

typecheck: $(NODE_MODULES_MARKER)
	@npm run typecheck

config:
	@$(COMPOSE) config --quiet

pull:
	@$(COMPOSE) --profile runner pull

observability-up:
	@mkdir -p data/results
	@$(COMPOSE) up -d capacity-results prometheus grafana

up: observability-up
	@$(COMPOSE) up -d --remove-orphans mock

cozy-up:
	@$(COZY_COMPOSE) up -d --build --wait couchdb redis rabbitmq cozy-stack

cozy-prepare: $(NODE_MODULES_MARKER) cozy-up
	@COZY_INSTANCE_DOMAIN="$(COZY_INSTANCE_DOMAIN)" COZY_BASE_URL="$(COZY_BASE_URL)" COZY_CLIENT_SOFTWARE_ID="$(COZY_CLIENT_SOFTWARE_ID)" COZY_TOKEN_EXPIRATION="$(COZY_TOKEN_EXPIRATION)" $(NODE_BIN)/tsx src/scripts/prepare-local-cozy-instance.ts

cozy-upload: cozy-prepare
	@$(MAKE) upload BASE_URL="$(COZY_BASE_URL)" FILE_SIZE="$(FILE_SIZE)" VUS="$(VUS)" ITERATIONS_PER_VU="$(ITERATIONS_PER_VU)" CONSISTENCY_CHUNK_SIZE_BYTES="$(CONSISTENCY_CHUNK_SIZE_BYTES)" UPLOAD_TARGET="$(UPLOAD_TARGET)" SHARED_DRIVE_ID="$(SHARED_DRIVE_ID)" SHARED_DRIVE_ROOT_ID="$(SHARED_DRIVE_ROOT_ID)" EXECUTION_ID="$(EXECUTION_ID)"

cozy-upload-soak: cozy-prepare
	@$(MAKE) upload-soak BASE_URL="$(COZY_BASE_URL)" FILE_SIZE="$(FILE_SIZE)" VUS="$(VUS)" DURATION="$(DURATION)" GRACEFUL_STOP="$(GRACEFUL_STOP)" CONSISTENCY_CHUNK_SIZE_BYTES="$(CONSISTENCY_CHUNK_SIZE_BYTES)" UPLOAD_TARGET="$(UPLOAD_TARGET)" SHARED_DRIVE_ID="$(SHARED_DRIVE_ID)" SHARED_DRIVE_ROOT_ID="$(SHARED_DRIVE_ROOT_ID)" EXECUTION_ID="$(EXECUTION_ID)"

WRITER_VUS ?= 3
WS_READER_VUS ?= 5
CHANGES_READER_VUS ?= 3
RAMP_LEVELS ?= 1,2,4,8,16
RAMP_STEP_DURATION ?= 1m
RAMP_WRITERS_PER_LEVEL ?= 1
RAMP_WS_READERS_PER_LEVEL ?= 3
RAMP_CHANGES_READERS_PER_LEVEL ?= 2

cozy-shared-drive-prepare: cozy-prepare
	@COZY_INSTANCE_DOMAIN="$(COZY_INSTANCE_DOMAIN)" COZY_BASE_URL="$(COZY_BASE_URL)" $(NODE_BIN)/tsx src/scripts/prepare-shared-drive.ts

cozy-shared-drive-load: upload-binary observability-up
	@mkdir -p data/results
	@$(COMPOSE) run --rm \
		-e BASE_URL="$(COZY_BASE_URL)" \
		-e COZY_ACCESS_TOKEN="$(COZY_ACCESS_TOKEN)" \
		-e UPLOAD_TARGET=shared-drive \
		-e SHARED_DRIVE_ID="$(SHARED_DRIVE_ID)" \
		-e SHARED_DRIVE_ROOT_ID="$(SHARED_DRIVE_ROOT_ID)" \
		-e FILE_SIZE="$(FILE_SIZE)" \
		-e DURATION="$(DURATION)" \
		-e WRITER_VUS="$(WRITER_VUS)" \
		-e WS_READER_VUS="$(WS_READER_VUS)" \
		-e CHANGES_READER_VUS="$(CHANGES_READER_VUS)" \
		-e EXECUTION_ID="$(EXECUTION_ID)" \
		k6 run -o experimental-prometheus-rw \
		--tag campaignid="$(EXECUTION_ID)" \
		--tag topology="$(LOAD_TOPOLOGY)" \
		--tag filesize="$(FILE_SIZE)" \
		--summary-export="/results/$(EXECUTION_ID)-shared-drive-summary.json" \
		/scripts/shared-drive-activity.ts
	@echo "Shared drive activity result: data/results/$(EXECUTION_ID)-shared-drive-summary.json"

cozy-shared-drive-ramp: upload-binary observability-up
	@mkdir -p data/results
	@$(RECORD_EXIT) $(COMPOSE) run --rm \
		-e BASE_URL="$(COZY_BASE_URL)" \
		-e COZY_ACCESS_TOKEN="$(COZY_ACCESS_TOKEN)" \
		-e UPLOAD_TARGET=shared-drive \
		-e SHARED_DRIVE_ID="$(SHARED_DRIVE_ID)" \
		-e SHARED_DRIVE_ROOT_ID="$(SHARED_DRIVE_ROOT_ID)" \
		-e FILE_SIZE="$(FILE_SIZE)" \
		-e RAMP_LEVELS="$(RAMP_LEVELS)" \
		-e RAMP_STEP_DURATION="$(RAMP_STEP_DURATION)" \
		-e RAMP_WRITERS_PER_LEVEL="$(RAMP_WRITERS_PER_LEVEL)" \
		-e RAMP_WS_READERS_PER_LEVEL="$(RAMP_WS_READERS_PER_LEVEL)" \
		-e RAMP_CHANGES_READERS_PER_LEVEL="$(RAMP_CHANGES_READERS_PER_LEVEL)" \
		-e MIN_SUCCESS_RATE="$(MIN_SUCCESS_RATE)" \
		-e P95_LIMIT_MS="$(P95_LIMIT_MS)" \
		-e MIN_DELIVERY_RATIO="$(MIN_DELIVERY_RATIO)" \
		-e EXECUTION_ID="$(EXECUTION_ID)" \
		k6 run -o experimental-prometheus-rw \
		--tag campaignid="$(EXECUTION_ID)" \
		--tag topology="$(LOAD_TOPOLOGY)" \
		--tag filesize="$(FILE_SIZE)" \
		--summary-export="/results/$(EXECUTION_ID)-shared-drive-ramp-summary.json" \
		/scripts/shared-drive-ramp.ts
	@echo "Shared drive ramp result: data/results/$(EXECUTION_ID)-shared-drive-ramp.json"

cozy-capacity: cozy-prepare
	@$(MAKE) upload-capacity BASE_URL="$(COZY_BASE_URL)" FILE_SIZE="$(FILE_SIZE)" MAX_VUS="$(MAX_VUS)" P95_LIMIT_MS="$(P95_LIMIT_MS)" MIN_SUCCESS_RATE="$(MIN_SUCCESS_RATE)" BASELINE_ITERATIONS="$(BASELINE_ITERATIONS)" CONFIRMATION_ITERATIONS="$(CONFIRMATION_ITERATIONS)" CONSISTENCY_CHUNK_SIZE_BYTES="$(CONSISTENCY_CHUNK_SIZE_BYTES)" UPLOAD_TARGET="$(UPLOAD_TARGET)" SHARED_DRIVE_ID="$(SHARED_DRIVE_ID)" SHARED_DRIVE_ROOT_ID="$(SHARED_DRIVE_ROOT_ID)" CAPACITY_RUN_ID="$(CAPACITY_RUN_ID)" FORCE_CAPACITY_RUN="$(FORCE_CAPACITY_RUN)"

cozy-capacity-all: cozy-prepare
	@$(MAKE) upload-capacity-all BASE_URL="$(COZY_BASE_URL)" P95_LIMIT_MS="$(P95_LIMIT_MS)" MIN_SUCCESS_RATE="$(MIN_SUCCESS_RATE)" BASELINE_ITERATIONS="$(BASELINE_ITERATIONS)" CONFIRMATION_ITERATIONS="$(CONFIRMATION_ITERATIONS)" CONSISTENCY_CHUNK_SIZE_BYTES="$(CONSISTENCY_CHUNK_SIZE_BYTES)" UPLOAD_TARGET="$(UPLOAD_TARGET)" SHARED_DRIVE_ID="$(SHARED_DRIVE_ID)" SHARED_DRIVE_ROOT_ID="$(SHARED_DRIVE_ROOT_ID)" FORCE_CAPACITY_RUN="$(FORCE_CAPACITY_RUN)"

campaign: $(NODE_MODULES_MARKER)
	@bash campaigns/run-campaign.sh "$(CAMPAIGN)"

cozy-status:
	@$(COZY_COMPOSE) ps

cozy-logs:
	@$(COZY_COMPOSE) logs --tail=200 couchdb redis rabbitmq minio cozy-stack

cozy-down:
	@$(COZY_COMPOSE) stop cozy-stack minio rabbitmq redis couchdb

upload-binaries: $(NODE_MODULES_MARKER)
	@$(NODE_BIN)/tsx src/scripts/generate-upload-binaries.ts all

upload-binary: $(NODE_MODULES_MARKER)
	@$(NODE_BIN)/tsx src/scripts/generate-upload-binaries.ts "$(FILE_SIZE)"

upload-smoke: FILE_SIZE := 1K
upload-smoke: VUS := 2
upload-smoke: ITERATIONS_PER_VU := 1
upload-smoke: upload-binary up
	@mkdir -p data/results
	@$(COMPOSE) run --rm -e BASE_URL=http://mock -e COZY_ACCESS_TOKEN=mock-token -e FILE_SIZE="$(FILE_SIZE)" -e UPLOAD_VUS="$(VUS)" -e UPLOAD_MODE="iterations" -e ITERATIONS_PER_VU="$(ITERATIONS_PER_VU)" -e CONSISTENCY_CHUNK_SIZE_BYTES="$(CONSISTENCY_CHUNK_SIZE_BYTES)" -e UPLOAD_TARGET="$(UPLOAD_TARGET)" -e SHARED_DRIVE_ID="$(SHARED_DRIVE_ID)" -e SHARED_DRIVE_ROOT_ID="$(SHARED_DRIVE_ROOT_ID)" -e CAPACITY_RUN_ID="$(CAPACITY_RUN_ID)" -e CAPACITY_PHASE="smoke" -e MIN_SUCCESS_RATE="$(MIN_SUCCESS_RATE)" -e P95_LIMIT_MS="$(P95_LIMIT_MS)" -e EXECUTION_ID="$(EXECUTION_ID)" k6 run -o experimental-prometheus-rw --tag campaignid="$(or $(CAPACITY_RUN_ID),$(EXECUTION_ID))" --tag topology="$(LOAD_TOPOLOGY)" --tag concurrency="$(VUS)" --tag executionid="$(EXECUTION_ID)" --tag filesize="$(FILE_SIZE)" --tag phase="smoke" --summary-export="/results/$(EXECUTION_ID)-summary.json" /scripts/concurrent-uploads.ts
	@echo "Upload smoke result: data/results/$(EXECUTION_ID)-summary.json"

upload: upload-binary observability-up
	@mkdir -p data/results
	@$(COMPOSE) run --rm -e BASE_URL="$(BASE_URL)" $(if $(filter http://mock,$(BASE_URL)),-e COZY_ACCESS_TOKEN=mock-token,) -e FILE_SIZE="$(FILE_SIZE)" -e UPLOAD_VUS="$(VUS)" -e UPLOAD_MODE="$(UPLOAD_MODE)" -e ITERATIONS_PER_VU="$(ITERATIONS_PER_VU)" -e DURATION="$(DURATION)" -e GRACEFUL_STOP="$(GRACEFUL_STOP)" -e CONSISTENCY_CHUNK_SIZE_BYTES="$(CONSISTENCY_CHUNK_SIZE_BYTES)" -e UPLOAD_TARGET="$(UPLOAD_TARGET)" -e SHARED_DRIVE_ID="$(SHARED_DRIVE_ID)" -e SHARED_DRIVE_ROOT_ID="$(SHARED_DRIVE_ROOT_ID)" -e CAPACITY_RUN_ID="$(CAPACITY_RUN_ID)" -e CAPACITY_PHASE="$(CAPACITY_PHASE)" -e MIN_SUCCESS_RATE="$(MIN_SUCCESS_RATE)" -e P95_LIMIT_MS="$(P95_LIMIT_MS)" -e MAX_DURATION="$(MAX_DURATION)" -e UPLOAD_TIMEOUT="$(UPLOAD_TIMEOUT)" -e EXECUTION_ID="$(EXECUTION_ID)" k6 run -o experimental-prometheus-rw --tag campaignid="$(or $(CAPACITY_RUN_ID),$(EXECUTION_ID))" --tag topology="$(LOAD_TOPOLOGY)" --tag concurrency="$(VUS)" --tag executionid="$(EXECUTION_ID)" --tag filesize="$(FILE_SIZE)" --tag phase="$(CAPACITY_PHASE)" --summary-export="/results/$(EXECUTION_ID)-summary.json" /scripts/concurrent-uploads.ts
	@echo "Upload result: data/results/$(EXECUTION_ID)-summary.json"

upload-soak:
	@$(MAKE) upload UPLOAD_MODE=soak CAPACITY_PHASE=soak

upload-capacity: $(NODE_MODULES_MARKER)
	@BASE_URL="$(BASE_URL)" FILE_SIZE="$(FILE_SIZE)" MAX_VUS="$(MAX_VUS)" P95_LIMIT_MS="$(P95_LIMIT_MS)" MIN_SUCCESS_RATE="$(MIN_SUCCESS_RATE)" BASELINE_ITERATIONS="$(BASELINE_ITERATIONS)" CONFIRMATION_ITERATIONS="$(CONFIRMATION_ITERATIONS)" CONSISTENCY_CHUNK_SIZE_BYTES="$(CONSISTENCY_CHUNK_SIZE_BYTES)" UPLOAD_TARGET="$(UPLOAD_TARGET)" SHARED_DRIVE_ID="$(SHARED_DRIVE_ID)" SHARED_DRIVE_ROOT_ID="$(SHARED_DRIVE_ROOT_ID)" CAPACITY_RUN_ID="$(CAPACITY_RUN_ID)" FORCE_CAPACITY_RUN="$(FORCE_CAPACITY_RUN)" $(RECORD_EXIT) $(NODE_BIN)/tsx src/scripts/find-upload-capacity.ts

upload-capacity-all:
	@for size in 1K 100K 1M 10M 100M 1G; do \
		$(MAKE) upload-capacity BASE_URL="$(BASE_URL)" FILE_SIZE="$$size" P95_LIMIT_MS="$(P95_LIMIT_MS)" MIN_SUCCESS_RATE="$(MIN_SUCCESS_RATE)" BASELINE_ITERATIONS="$(BASELINE_ITERATIONS)" CONFIRMATION_ITERATIONS="$(CONFIRMATION_ITERATIONS)" CONSISTENCY_CHUNK_SIZE_BYTES="$(CONSISTENCY_CHUNK_SIZE_BYTES)" UPLOAD_TARGET="$(UPLOAD_TARGET)" SHARED_DRIVE_ID="$(SHARED_DRIVE_ID)" SHARED_DRIVE_ROOT_ID="$(SHARED_DRIVE_ROOT_ID)" FORCE_CAPACITY_RUN="$(FORCE_CAPACITY_RUN)" || exit $$?; \
	done

status:
	@$(COMPOSE) ps

logs:
	@$(COMPOSE) logs --tail=200

down:
	@$(COMPOSE) down

.PHONY: capacity-dashboard-url
capacity-dashboard-url:
	@printf '%s/d/twake-drive-capacity/stack-capacity-overview\n' "$(GRAFANA_URL)"

dashboard-url:
	@port="$$( $(COMPOSE) port grafana 3000 | sed 's/.*://' )"; echo "http://localhost:$$port/"
