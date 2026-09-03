SHELL := /bin/bash
-include .env

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
LATENCY_MULTIPLIER ?= 2
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

export HOST_UID
export HOST_GID

.PHONY: help doctor install typecheck config pull observability-up up cozy-up cozy-prepare cozy-upload cozy-upload-soak cozy-capacity cozy-capacity-all cozy-status cozy-logs cozy-down upload-binaries upload-binary upload-smoke upload upload-soak upload-capacity upload-capacity-all status logs down dashboard-url

help:
	@echo "make install                                 Install the load-harness Node tools"
	@echo "make typecheck                               Typecheck k6 and Node TypeScript"
	@echo "make up                                      Start dashboards and the local mock"
	@echo "make upload-binaries                         Generate every upload binary"
	@echo "make upload-smoke                            Run two concurrent 1K mock uploads"
	@echo "make cozy-up                                 Build and start the local Cozy services"
	@echo "make cozy-prepare                            Prepare the local Cozy test instance"
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

typecheck: $(NODE_MODULES_MARKER)
	@npm run typecheck

config:
	@$(COMPOSE) config --quiet

pull:
	@$(COMPOSE) --profile runner pull

observability-up:
	@$(COMPOSE) up -d --remove-orphans prometheus grafana

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

cozy-capacity: cozy-prepare
	@$(MAKE) upload-capacity BASE_URL="$(COZY_BASE_URL)" FILE_SIZE="$(FILE_SIZE)" MAX_VUS="$(MAX_VUS)" LATENCY_MULTIPLIER="$(LATENCY_MULTIPLIER)" P95_LIMIT_MS="$(P95_LIMIT_MS)" MIN_SUCCESS_RATE="$(MIN_SUCCESS_RATE)" BASELINE_ITERATIONS="$(BASELINE_ITERATIONS)" CONFIRMATION_ITERATIONS="$(CONFIRMATION_ITERATIONS)" CONSISTENCY_CHUNK_SIZE_BYTES="$(CONSISTENCY_CHUNK_SIZE_BYTES)" UPLOAD_TARGET="$(UPLOAD_TARGET)" SHARED_DRIVE_ID="$(SHARED_DRIVE_ID)" SHARED_DRIVE_ROOT_ID="$(SHARED_DRIVE_ROOT_ID)" CAPACITY_RUN_ID="$(CAPACITY_RUN_ID)" FORCE_CAPACITY_RUN="$(FORCE_CAPACITY_RUN)"

cozy-capacity-all: cozy-prepare
	@$(MAKE) upload-capacity-all BASE_URL="$(COZY_BASE_URL)" LATENCY_MULTIPLIER="$(LATENCY_MULTIPLIER)" P95_LIMIT_MS="$(P95_LIMIT_MS)" MIN_SUCCESS_RATE="$(MIN_SUCCESS_RATE)" BASELINE_ITERATIONS="$(BASELINE_ITERATIONS)" CONFIRMATION_ITERATIONS="$(CONFIRMATION_ITERATIONS)" CONSISTENCY_CHUNK_SIZE_BYTES="$(CONSISTENCY_CHUNK_SIZE_BYTES)" UPLOAD_TARGET="$(UPLOAD_TARGET)" SHARED_DRIVE_ID="$(SHARED_DRIVE_ID)" SHARED_DRIVE_ROOT_ID="$(SHARED_DRIVE_ROOT_ID)" FORCE_CAPACITY_RUN="$(FORCE_CAPACITY_RUN)"

cozy-status:
	@$(COZY_COMPOSE) ps

cozy-logs:
	@$(COZY_COMPOSE) logs --tail=200 couchdb redis rabbitmq cozy-stack

cozy-down:
	@$(COZY_COMPOSE) stop cozy-stack rabbitmq redis couchdb

upload-binaries: $(NODE_MODULES_MARKER)
	@$(NODE_BIN)/tsx src/scripts/generate-upload-binaries.ts all

upload-binary: $(NODE_MODULES_MARKER)
	@$(NODE_BIN)/tsx src/scripts/generate-upload-binaries.ts "$(FILE_SIZE)"

upload-smoke: FILE_SIZE := 1K
upload-smoke: VUS := 2
upload-smoke: ITERATIONS_PER_VU := 1
upload-smoke: upload-binary up
	@mkdir -p data/results
	@$(COMPOSE) run --rm -e BASE_URL=http://mock -e COZY_ACCESS_TOKEN=mock-token -e FILE_SIZE="$(FILE_SIZE)" -e UPLOAD_VUS="$(VUS)" -e UPLOAD_MODE="iterations" -e ITERATIONS_PER_VU="$(ITERATIONS_PER_VU)" -e CONSISTENCY_CHUNK_SIZE_BYTES="$(CONSISTENCY_CHUNK_SIZE_BYTES)" -e UPLOAD_TARGET="$(UPLOAD_TARGET)" -e SHARED_DRIVE_ID="$(SHARED_DRIVE_ID)" -e SHARED_DRIVE_ROOT_ID="$(SHARED_DRIVE_ROOT_ID)" -e CAPACITY_RUN_ID="$(CAPACITY_RUN_ID)" -e CAPACITY_PHASE="smoke" -e MIN_SUCCESS_RATE="$(MIN_SUCCESS_RATE)" -e P95_LIMIT_MS="$(P95_LIMIT_MS)" -e EXECUTION_ID="$(EXECUTION_ID)" k6 run -o experimental-prometheus-rw --tag campaignid="$(or $(CAPACITY_RUN_ID),$(EXECUTION_ID))" --tag concurrency="$(VUS)" --tag executionid="$(EXECUTION_ID)" --tag filesize="$(FILE_SIZE)" --tag phase="smoke" --summary-export="/results/$(EXECUTION_ID)-summary.json" /scripts/concurrent-uploads.ts
	@echo "Upload smoke result: data/results/$(EXECUTION_ID)-summary.json"

upload: upload-binary observability-up
	@mkdir -p data/results
	@$(COMPOSE) run --rm -e BASE_URL="$(BASE_URL)" $(if $(filter http://mock,$(BASE_URL)),-e COZY_ACCESS_TOKEN=mock-token,) -e FILE_SIZE="$(FILE_SIZE)" -e UPLOAD_VUS="$(VUS)" -e UPLOAD_MODE="$(UPLOAD_MODE)" -e ITERATIONS_PER_VU="$(ITERATIONS_PER_VU)" -e DURATION="$(DURATION)" -e GRACEFUL_STOP="$(GRACEFUL_STOP)" -e CONSISTENCY_CHUNK_SIZE_BYTES="$(CONSISTENCY_CHUNK_SIZE_BYTES)" -e UPLOAD_TARGET="$(UPLOAD_TARGET)" -e SHARED_DRIVE_ID="$(SHARED_DRIVE_ID)" -e SHARED_DRIVE_ROOT_ID="$(SHARED_DRIVE_ROOT_ID)" -e CAPACITY_RUN_ID="$(CAPACITY_RUN_ID)" -e CAPACITY_PHASE="$(CAPACITY_PHASE)" -e MIN_SUCCESS_RATE="$(MIN_SUCCESS_RATE)" -e P95_LIMIT_MS="$(P95_LIMIT_MS)" -e MAX_DURATION="$(MAX_DURATION)" -e UPLOAD_TIMEOUT="$(UPLOAD_TIMEOUT)" -e EXECUTION_ID="$(EXECUTION_ID)" k6 run -o experimental-prometheus-rw --tag campaignid="$(or $(CAPACITY_RUN_ID),$(EXECUTION_ID))" --tag concurrency="$(VUS)" --tag executionid="$(EXECUTION_ID)" --tag filesize="$(FILE_SIZE)" --tag phase="$(CAPACITY_PHASE)" --summary-export="/results/$(EXECUTION_ID)-summary.json" /scripts/concurrent-uploads.ts
	@echo "Upload result: data/results/$(EXECUTION_ID)-summary.json"

upload-soak:
	@$(MAKE) upload UPLOAD_MODE=soak CAPACITY_PHASE=soak

upload-capacity: $(NODE_MODULES_MARKER)
	@BASE_URL="$(BASE_URL)" FILE_SIZE="$(FILE_SIZE)" MAX_VUS="$(MAX_VUS)" LATENCY_MULTIPLIER="$(LATENCY_MULTIPLIER)" P95_LIMIT_MS="$(P95_LIMIT_MS)" MIN_SUCCESS_RATE="$(MIN_SUCCESS_RATE)" BASELINE_ITERATIONS="$(BASELINE_ITERATIONS)" CONFIRMATION_ITERATIONS="$(CONFIRMATION_ITERATIONS)" CONSISTENCY_CHUNK_SIZE_BYTES="$(CONSISTENCY_CHUNK_SIZE_BYTES)" UPLOAD_TARGET="$(UPLOAD_TARGET)" SHARED_DRIVE_ID="$(SHARED_DRIVE_ID)" SHARED_DRIVE_ROOT_ID="$(SHARED_DRIVE_ROOT_ID)" CAPACITY_RUN_ID="$(CAPACITY_RUN_ID)" FORCE_CAPACITY_RUN="$(FORCE_CAPACITY_RUN)" $(NODE_BIN)/tsx src/scripts/find-upload-capacity.ts

upload-capacity-all:
	@for size in 1K 100K 1M 10M 100M 1G; do \
		$(MAKE) upload-capacity BASE_URL="$(BASE_URL)" FILE_SIZE="$$size" LATENCY_MULTIPLIER="$(LATENCY_MULTIPLIER)" P95_LIMIT_MS="$(P95_LIMIT_MS)" MIN_SUCCESS_RATE="$(MIN_SUCCESS_RATE)" BASELINE_ITERATIONS="$(BASELINE_ITERATIONS)" CONFIRMATION_ITERATIONS="$(CONFIRMATION_ITERATIONS)" CONSISTENCY_CHUNK_SIZE_BYTES="$(CONSISTENCY_CHUNK_SIZE_BYTES)" UPLOAD_TARGET="$(UPLOAD_TARGET)" SHARED_DRIVE_ID="$(SHARED_DRIVE_ID)" SHARED_DRIVE_ROOT_ID="$(SHARED_DRIVE_ROOT_ID)" FORCE_CAPACITY_RUN="$(FORCE_CAPACITY_RUN)" || exit $$?; \
	done

status:
	@$(COMPOSE) ps

logs:
	@$(COMPOSE) logs --tail=200

down:
	@$(COMPOSE) down

dashboard-url:
	@port="$$( $(COMPOSE) port grafana 3000 | sed 's/.*://' )"; echo "http://localhost:$$port/"
