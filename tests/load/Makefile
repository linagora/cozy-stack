SHELL := /bin/bash
.DEFAULT_GOAL := help
-include .env

# Keep unset optional values unset so the Node CLI can apply its defaults.
SEED_SETTINGS := SEED_ADMIN_URL SEED_STACK_URL SEED_ADMIN_USERNAME SEED_ADMIN_PASSWORD SEED_DOMAIN_SUFFIX \
                 SEED_DOMAIN SEED_USERS SEED_FILES SEED_DRIVES SEED_CONTEXT_NAME SEED_REQUEST_TIMEOUT_MS \
                 SEED_MANIFEST ACTORS_DOMAIN SEED_PROFILE SEED_GENERATION_SEED
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
ITERATIONS_PER_VU ?= 1
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
# Upload campaign budgets (README, Upload campaigns); shared DEV requires all of them.
MAX_UPLOAD_BYTES ?=
MAX_RUNTIME ?=
LOAD_TARGET ?= local
COZY_INSTANCE_DOMAIN ?= load.localhost:8080
COZY_BASE_URL ?= http://$(COZY_INSTANCE_DOMAIN)
COZY_STACK_URL ?= http://$(or $(COZY_STACK_BIND_ADDRESS),127.0.0.1):$(or $(COZY_STACK_PORT),8080)
# Where the host reaches the Stack when k6 uses a Compose-internal BASE_URL: the bundled Stack's port
# when BASE_URL is that Stack, as make cozy-prepare writes it to .env.
STACK_URL ?= $(if $(filter $(COZY_BASE_URL),$(BASE_URL)),$(COZY_STACK_URL))
# Organization fixtures seed the bundled Stack through its loopback admin port, in the load-org-sharing
# context (compose.yaml, config/cozy/cozy.yaml). Shared DEV seeds through the Cloudery (SEEDING.md).
ifeq ($(LOAD_TARGET)$(SEED_CLOUDERY_TOKEN_FILE),local)
SEED_ADMIN_URL ?= http://127.0.0.1:$(or $(COZY_ADMIN_PORT),16060)
SEED_STACK_URL ?= $(COZY_STACK_URL)
SEED_ADMIN_PASSWORD ?= cozy
SEED_DOMAIN_SUFFIX ?= localhost:8080
SEED_CONTEXT_NAME ?= load-org-sharing
export SEED_ADMIN_URL SEED_STACK_URL SEED_ADMIN_PASSWORD SEED_DOMAIN_SUFFIX SEED_CONTEXT_NAME
endif
COZY_CLIENT_SOFTWARE_ID ?= twake-drive-load-local
COZY_TOKEN_EXPIRATION ?= 24h
HOST_UID ?= $(shell id -u)
HOST_GID ?= $(shell id -g)
# Recorded with every campaign and tagged on k6 metrics: k6 and the stack
# share one host unless a separate load generator is used.
LOAD_TOPOLOGY ?= single-host
GRAFANA_URL ?= http://localhost:$(or $(GRAFANA_PORT),3000)
CAMPAIGN ?= smoke
# Campaign steps set a command prefix that records a producer's own exit status:
# make reports every failed recipe as 2.
RECORD_EXIT ?=

export HOST_UID
export HOST_GID
# Compose reads the optional resource limits from the environment.
export COZY_STACK_CPUS COZY_STACK_MEMORY COUCHDB_CPUS COUCHDB_MEMORY
export LOAD_TOPOLOGY GRAFANA_URL

.PHONY: help doctor install typecheck node-checks config pull observability-up up cozy-up cozy-prepare cozy-shared-drive-prepare shared-drive-ramp shared-drive-ramp-run campaign cozy-status cozy-logs cozy-down upload-binaries upload-binary upload-smoke upload upload-capacity upload-regression status logs down dashboard-url

help:
	@echo "make install                                 Install the load-harness Node tools"
	@echo "make typecheck                               Typecheck k6 and Node TypeScript"
	@echo "make node-checks                             Run npm ci, tests and typecheck in the Compose Node image"
	@echo "make seed-scale SEED_FILES=1000 SEED_DRIVES=10  Build a disposable fixture through Stack APIs"
	@echo "make seed-scale-cleanup SEED_DOMAIN=...        Delete that disposable fixture instance"
	@echo "make seed-org SEED_USERS=50                    Build an organization with empty user instances"
	@echo "make seed-org-cleanup SEED_DOMAIN=...          Delete the organization fixture and its users"
	@echo "make seed-verify SEED_DOMAIN=...               Check a seeded fixture against its manifest"
	@echo "make org-drive-fanout FANOUT_LEVELS=10,50,100   Check group auto-accept and revoke convergence"
	@echo "Runners target BASE_URL (make cozy-prepare sets the bundled Stack), or shared DEV with LOAD_TARGET=dev,"
	@echo "DEV_INSTANCE and the MAX_VUS, MAX_UPLOAD_BYTES and MAX_RUNTIME budgets agreed with the DEV owner."
	@echo "make fixture-up FIXTURE_PROJECT=load-fixture-seed"
	@echo "make fixture-snapshot FIXTURE_PROJECT=... FIXTURE_SNAPSHOT=/private/baseline"
	@echo "make fixture-restore FIXTURE_PROJECT=... FIXTURE_SNAPSHOT=/private/baseline"
	@echo "make fixture-discard FIXTURE_PROJECT=...       Remove only a marked disposable deployment"
	@echo "make capacity-dashboard-url                  Open the shared load-test overview"
	@echo "make up                                      Start dashboards and the local mock"
	@echo "make upload-binaries                         Generate every upload binary"
	@echo "make upload-smoke                            Run two concurrent 1K mock uploads"
	@echo "make cozy-up                                 Build and start the local Cozy services"
	@echo "make cozy-prepare                            Prepare the local Cozy test instance"
	@echo "make cozy-shared-drive-prepare               Prepare a local shared drive for load testing"
	@echo "make shared-drive-ramp                       Find the highest sustained shared drive level"
	@echo "make campaign CAMPAIGN=smoke|capacity        Run every runner on short or full ladders (LOAD_TARGET=dev for shared DEV)"
	@echo "make upload BASE_URL=https://... FILE_SIZE=1M VUS=10"
	@echo "make upload UPLOAD_TARGET=shared-drive SHARED_DRIVE_ID=... SHARED_DRIVE_ROOT_ID=..."
	@echo "make upload-capacity BASE_URL=https://... FILE_SIZE=10M"
	@echo "make upload-regression BASE_URL=https://... VUS=2 ITERATIONS_PER_VU=5"
	@echo "make mixed-capacity MIXED_DOMAIN=... MIXED_RATES=1,2,4,8  The highest workflow rate a tree fixture sustains"
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

.PHONY: seed-scale seed-scale-cleanup seed-verify
# A Cloudery token file seeds, verifies and removes one tree profile instance through the Cloudery
# instead (SEEDING.md).
seed-scale: $(NODE_MODULES_MARKER)
ifdef SEED_CLOUDERY_TOKEN_FILE
	@$(NODE_BIN)/tsx src/scripts/seed-cloudery.ts seed-tree
else
	@$(NODE_BIN)/tsx src/scripts/seed-scale-data.ts seed $${SEED_DOMAIN:+"$$SEED_DOMAIN"}
endif

seed-scale-cleanup: $(NODE_MODULES_MARKER)
ifdef SEED_CLOUDERY_TOKEN_FILE
	@$(NODE_BIN)/tsx src/scripts/seed-cloudery.ts teardown-tree
else
	@$(NODE_BIN)/tsx src/scripts/seed-scale-data.ts cleanup "$$SEED_DOMAIN"
endif

seed-verify: $(NODE_MODULES_MARKER)
ifdef SEED_CLOUDERY_TOKEN_FILE
	@$(NODE_BIN)/tsx src/scripts/seed-cloudery.ts verify-tree
else
	@$(NODE_BIN)/tsx src/scripts/seed-scale-data.ts verify "$$SEED_DOMAIN"
endif

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
# LOAD_TARGET=dev: actors from the Cloudery fixture, under the shared DEV lock (ORG-DRIVE-FANOUT.md).
org-drive-fanout: $(NODE_MODULES_MARKER)
	@LOAD_TARGET="$(LOAD_TARGET)" EXECUTION_ID="$(EXECUTION_ID)" MAX_RUNTIME="$(MAX_RUNTIME)" $(RECORD_EXIT) \
		$(if $(filter dev,$(LOAD_TARGET)),bash campaigns/lib/shared-dev-lock.sh "$(DEV_LOCK_FILE)" node --import tsx,$(NODE_BIN)/tsx) \
		src/scripts/org-drive-fanout.ts

.PHONY: fixture-up fixture-snapshot fixture-restore fixture-discard
fixture-up fixture-discard: $(NODE_MODULES_MARKER)
	@$(NODE_BIN)/tsx src/scripts/compose-fixture.ts $(patsubst fixture-%,%,$@) "$$FIXTURE_PROJECT"

fixture-snapshot fixture-restore: $(NODE_MODULES_MARKER)
	@$(NODE_BIN)/tsx src/scripts/compose-fixture.ts $(patsubst fixture-%,%,$@) "$$FIXTURE_PROJECT" \
		"$$FIXTURE_SNAPSHOT"

typecheck: $(NODE_MODULES_MARKER)
	@npm run typecheck

# On a copy of the sources, so the checkout, its node_modules and .env stay untouched; --rm also
# removes the container's anonymous volumes.
node-checks:
	@image=$$($(COMPOSE) config --images | grep -m1 '^node:') && echo "Node image: $$image" && \
	docker run --rm -v "$$PWD":/src:ro "$$image" sh -euc '\
		apk add --no-cache make bash > /dev/null; mkdir /app; \
		tar -C /src --exclude=./node_modules --exclude=./data --exclude=./.env -cf - . | tar -C /app -xf -; \
		cd /app; node --version; npm ci --no-audit --no-fund; npm test; npm run typecheck'

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

RAMP_LEVELS ?= 1,2,4,8,16
RAMP_STEP_DURATION ?= 1m
RAMP_WRITERS_PER_LEVEL ?= 1
RAMP_WS_READERS_PER_LEVEL ?= 3
RAMP_CHANGES_READERS_PER_LEVEL ?= 2

cozy-shared-drive-prepare: cozy-prepare
	@COZY_INSTANCE_DOMAIN="$(COZY_INSTANCE_DOMAIN)" COZY_BASE_URL="$(COZY_BASE_URL)" $(NODE_BIN)/tsx src/scripts/prepare-shared-drive.ts

# k6 alone; shared-drive-ramp runs it with the target's token in the environment and the drive.
shared-drive-ramp-run: upload-binary observability-up
	@mkdir -p data/results
	@$(RECORD_EXIT) $(COMPOSE) run --rm \
		-e BASE_URL="$(BASE_URL)" \
		-e WRITER_BASE_URL="$(WRITER_BASE_URL)" \
		-e READER_BASE_URL="$(READER_BASE_URL)" \
		-e ACTOR_HOSTS="$(ACTOR_HOSTS)" \
		-e CONTENT_CHECKS_PER_LEVEL="$(CONTENT_CHECKS_PER_LEVEL)" \
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
		--tag load_target="$(LOAD_TARGET)" \
		--tag mode=capacity \
		--tag topology="$(LOAD_TOPOLOGY)" \
		--tag filesize="$(FILE_SIZE)" \
		--summary-export="/results/$(EXECUTION_ID)-shared-drive-ramp-summary.json" \
		/scripts/shared-drive-ramp.ts

# k6 alone; mixed-workload runs it against a verified tree fixture with the instance's token.
mixed-run: FILE_SIZE := 1K
mixed-run: upload-binary observability-up
	@mkdir -p data/results
	@$(RECORD_EXIT) $(COMPOSE) run --rm \
		-e BASE_URL="$(BASE_URL)" \
		-e ACTOR_HOSTS="$(ACTOR_HOSTS)" \
		-e FILE_SIZE=1K \
		-e MIXED_RATE="$(MIXED_RATE)" \
		-e MIXED_DURATION="$(MIXED_DURATION)" \
		-e MIXED_WARMUP="$(MIXED_WARMUP)" \
		-e MIXED_MAX_VUS="$(MIXED_MAX_VUS)" \
		-e MIXED_OBSERVERS="$(MIXED_OBSERVERS)" \
		-e MIN_SUCCESS_RATE="$(MIN_SUCCESS_RATE)" \
		-e P95_LIMIT_MS="$(P95_LIMIT_MS)" \
		-e EXECUTION_ID="$(EXECUTION_ID)" \
		k6 run -o experimental-prometheus-rw \
		--tag campaignid="$(or $(CAPACITY_RUN_ID),$(EXECUTION_ID))" \
		--tag executionid="$(EXECUTION_ID)" \
		--tag load_target="$(LOAD_TARGET)" \
		--tag mode="$(or $(MIXED_MODE),regression)" \
		--tag topology="$(LOAD_TOPOLOGY)" \
		/scripts/mixed-workload.ts

# A fixed arrival rate of workflows against a tree fixture (README, Mixed workload); mixed-soak keeps it
# up for longer between a warm-up and a drain, and mixed-capacity steps through MIXED_RATES. LOAD_TARGET=dev
# runs against the Cloudery tree fixture under the shared DEV lock.
.PHONY: mixed-workload mixed-soak mixed-capacity
mixed-soak: MIXED_PROFILE := soak
mixed-capacity: MIXED_PROFILE := capacity
mixed-workload mixed-soak mixed-capacity: $(NODE_MODULES_MARKER)
	@MIXED_PROFILE="$(MIXED_PROFILE)" MIXED_RATES="$(MIXED_RATES)" MIXED_WARMUP="$(MIXED_WARMUP)" MIXED_DRAIN="$(MIXED_DRAIN)" SOAK_TRIAL="$(SOAK_TRIAL)" SOAK_MAX_GOROUTINE_GROWTH="$(SOAK_MAX_GOROUTINE_GROWTH)" SOAK_MAX_HEAP_GROWTH="$(SOAK_MAX_HEAP_GROWTH)" MIXED_DOMAIN="$(MIXED_DOMAIN)" MIXED_RATE="$(MIXED_RATE)" MIXED_DURATION="$(MIXED_DURATION)" MIXED_MAX_VUS="$(MIXED_MAX_VUS)" MIXED_OBSERVERS="$(MIXED_OBSERVERS)" MIN_SUCCESS_RATE="$(MIN_SUCCESS_RATE)" P95_LIMIT_MS="$(P95_LIMIT_MS)" MAX_VUS="$(MAX_VUS)" MAX_REQUESTS="$(MAX_REQUESTS)" MAX_UPLOAD_BYTES="$(MAX_UPLOAD_BYTES)" MAX_RUNTIME="$(MAX_RUNTIME)" LOAD_TARGET="$(LOAD_TARGET)" EXECUTION_ID="$(EXECUTION_ID)" \
		$(RECORD_EXIT) $(if $(filter dev,$(LOAD_TARGET)),bash campaigns/lib/shared-dev-lock.sh "$(DEV_LOCK_FILE)") node --import tsx src/scripts/mixed-campaign.ts

# The shared-drive ramp through the campaign runner: LOAD_TARGET=dev holds the shared DEV lock, as for
# uploads. The runner creates or reuses the test drive itself.
SHARED_DRIVE_CAMPAIGN = LOAD_TARGET="$(LOAD_TARGET)" BASE_URL="$(BASE_URL)" STACK_URL="$(STACK_URL)" FILE_SIZE="$(FILE_SIZE)" RAMP_LEVELS="$(RAMP_LEVELS)" RAMP_STEP_DURATION="$(RAMP_STEP_DURATION)" RAMP_WRITERS_PER_LEVEL="$(RAMP_WRITERS_PER_LEVEL)" RAMP_WS_READERS_PER_LEVEL="$(RAMP_WS_READERS_PER_LEVEL)" RAMP_CHANGES_READERS_PER_LEVEL="$(RAMP_CHANGES_READERS_PER_LEVEL)" MIN_SUCCESS_RATE="$(MIN_SUCCESS_RATE)" P95_LIMIT_MS="$(P95_LIMIT_MS)" MIN_DELIVERY_RATIO="$(MIN_DELIVERY_RATIO)" MAX_VUS="$(MAX_VUS)" MAX_UPLOAD_BYTES="$(MAX_UPLOAD_BYTES)" MAX_RUNTIME="$(MAX_RUNTIME)" EXECUTION_ID="$(EXECUTION_ID)" \
	$(RECORD_EXIT) $(if $(filter dev,$(LOAD_TARGET)),bash campaigns/lib/shared-dev-lock.sh "$(DEV_LOCK_FILE)") node --import tsx src/scripts/shared-drive-campaign.ts
shared-drive-ramp: export COZY_ACCESS_TOKEN := $(COZY_ACCESS_TOKEN)
shared-drive-ramp: $(NODE_MODULES_MARKER)
	@$(SHARED_DRIVE_CAMPAIGN)

campaign: $(NODE_MODULES_MARKER)
	@LOAD_TARGET="$(LOAD_TARGET)" bash campaigns/run-campaign.sh "$(CAMPAIGN)"

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
	@$(COMPOSE) run --rm -e BASE_URL=http://mock -e COZY_ACCESS_TOKEN=mock-token -e FILE_SIZE="$(FILE_SIZE)" -e UPLOAD_VUS="$(VUS)" -e ITERATIONS_PER_VU="$(ITERATIONS_PER_VU)" -e CONSISTENCY_CHUNK_SIZE_BYTES="$(CONSISTENCY_CHUNK_SIZE_BYTES)" -e UPLOAD_TARGET="$(UPLOAD_TARGET)" -e SHARED_DRIVE_ID="$(SHARED_DRIVE_ID)" -e SHARED_DRIVE_ROOT_ID="$(SHARED_DRIVE_ROOT_ID)" -e CAPACITY_RUN_ID="$(CAPACITY_RUN_ID)" -e CAPACITY_PHASE="smoke" -e MIN_SUCCESS_RATE="$(MIN_SUCCESS_RATE)" -e P95_LIMIT_MS="$(P95_LIMIT_MS)" -e EXECUTION_ID="$(EXECUTION_ID)" k6 run -o experimental-prometheus-rw --tag campaignid="$(or $(CAPACITY_RUN_ID),$(EXECUTION_ID))" --tag topology="$(LOAD_TOPOLOGY)" --tag concurrency="$(VUS)" --tag executionid="$(EXECUTION_ID)" --tag filesize="$(FILE_SIZE)" --tag phase="smoke" --summary-export="/results/$(EXECUTION_ID)-summary.json" /scripts/concurrent-uploads.ts
	@echo "Upload smoke result: data/results/$(EXECUTION_ID)-summary.json"

upload: upload-binary observability-up
	@mkdir -p data/results
	@$(RECORD_EXIT) $(COMPOSE) run --rm -e BASE_URL="$(BASE_URL)" $(if $(filter http://mock,$(BASE_URL)),-e COZY_ACCESS_TOKEN=mock-token,) -e FILE_SIZE="$(FILE_SIZE)" -e UPLOAD_VUS="$(VUS)" -e ITERATIONS_PER_VU="$(ITERATIONS_PER_VU)" -e CONSISTENCY_CHUNK_SIZE_BYTES="$(CONSISTENCY_CHUNK_SIZE_BYTES)" -e UPLOAD_TARGET="$(UPLOAD_TARGET)" -e SHARED_DRIVE_ID="$(SHARED_DRIVE_ID)" -e SHARED_DRIVE_ROOT_ID="$(SHARED_DRIVE_ROOT_ID)" -e CAPACITY_RUN_ID="$(CAPACITY_RUN_ID)" -e CAPACITY_PHASE="$(CAPACITY_PHASE)" -e MIN_SUCCESS_RATE="$(MIN_SUCCESS_RATE)" -e P95_LIMIT_MS="$(P95_LIMIT_MS)" -e MAX_DURATION="$(MAX_DURATION)" -e UPLOAD_TIMEOUT="$(UPLOAD_TIMEOUT)" -e EXECUTION_ID="$(EXECUTION_ID)" k6 run -o experimental-prometheus-rw --tag campaignid="$(or $(CAPACITY_RUN_ID),$(EXECUTION_ID))" --tag topology="$(LOAD_TOPOLOGY)" --tag load_target="$(LOAD_TARGET)" $(if $(MODE),--tag mode="$(MODE)") --tag concurrency="$(VUS)" --tag executionid="$(EXECUTION_ID)" --tag filesize="$(FILE_SIZE)" --tag phase="$(CAPACITY_PHASE)" --summary-export="/results/$(EXECUTION_ID)-summary.json" /scripts/concurrent-uploads.ts
	@echo "Upload result: data/results/$(EXECUTION_ID)-summary.json"

# One runner for every upload campaign: MODE picks the fixed regression workload or the capacity
# search. LOAD_TARGET=dev holds the shared DEV lock, which every checkout, user and container on the
# runner uses. node --import tsx keeps the runner in the process holding that lock; the tsx CLI would
# run it in a child that outlives its killed parent.
DEV_LOCK_FILE ?= /tmp/cozy-load-shared-dev.lock
UPLOAD_CAMPAIGN = LOAD_TARGET="$(LOAD_TARGET)" BASE_URL="$(BASE_URL)" STACK_URL="$(STACK_URL)" FILE_SIZE="$(FILE_SIZE)" VUS="$(VUS)" ITERATIONS_PER_VU="$(ITERATIONS_PER_VU)" MAX_VUS="$(MAX_VUS)" MAX_UPLOAD_BYTES="$(MAX_UPLOAD_BYTES)" MAX_RUNTIME="$(MAX_RUNTIME)" UPLOAD_TIMEOUT="$(UPLOAD_TIMEOUT)" P95_LIMIT_MS="$(P95_LIMIT_MS)" MIN_SUCCESS_RATE="$(MIN_SUCCESS_RATE)" BASELINE_ITERATIONS="$(BASELINE_ITERATIONS)" CONFIRMATION_ITERATIONS="$(CONFIRMATION_ITERATIONS)" CONSISTENCY_CHUNK_SIZE_BYTES="$(CONSISTENCY_CHUNK_SIZE_BYTES)" UPLOAD_TARGET="$(UPLOAD_TARGET)" SHARED_DRIVE_ID="$(SHARED_DRIVE_ID)" SHARED_DRIVE_ROOT_ID="$(SHARED_DRIVE_ROOT_ID)" CAPACITY_RUN_ID="$(CAPACITY_RUN_ID)" EXECUTION_ID="$(EXECUTION_ID)" FORCE_CAPACITY_RUN="$(FORCE_CAPACITY_RUN)" \
	$(RECORD_EXIT) $(if $(filter dev,$(LOAD_TARGET)),bash campaigns/lib/shared-dev-lock.sh "$(DEV_LOCK_FILE)") node --import tsx src/scripts/upload-campaign.ts

# The runner reads the local token from its environment, never from a command line.
upload-capacity upload-regression: export COZY_ACCESS_TOKEN := $(COZY_ACCESS_TOKEN)
upload-capacity: $(NODE_MODULES_MARKER)
	@MODE=capacity $(UPLOAD_CAMPAIGN)

upload-regression: $(NODE_MODULES_MARKER)
	@MODE=regression $(UPLOAD_CAMPAIGN)

status:
	@$(COMPOSE) ps

logs:
	@$(COMPOSE) logs --tail=200

down:
	@$(COMPOSE) down

.PHONY: capacity-dashboard-url
capacity-dashboard-url:
	@printf '%s/d/twake-drive-capacity\n' "$(GRAFANA_URL)"

dashboard-url:
	@port="$$( $(COMPOSE) port grafana 3000 | sed 's/.*://' )"; echo "http://localhost:$$port/"
