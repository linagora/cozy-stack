SHELL := /bin/bash
-include .env

COMPOSE := docker compose
NODE_BIN := ./node_modules/.bin
NODE_MODULES_MARKER := node_modules/.load-ready
SCENARIO ?= http.ts
BASE_URL ?= http://mock
VUS ?= 1
DURATION ?= 10s
FILE_SIZE ?= 1K
ITERATIONS_PER_VU ?= 1
MAX_DURATION ?= 30m
UPLOAD_TIMEOUT ?= 30m
TEST_ID ?= $(shell date -u +%Y%m%dT%H%M%SZ)
CAPACITY_RUN_ID ?=
CAPACITY_PHASE ?= single-run
P95_LIMIT_MS ?=
MAX_VUS ?=
LATENCY_MULTIPLIER ?= 2
MIN_SUCCESS_RATE ?= 0.99
CONFIRMATION_ITERATIONS ?= 3
FORCE_CAPACITY_RUN ?= 0
HOST_UID ?= $(shell id -u)
HOST_GID ?= $(shell id -g)

export HOST_UID
export HOST_GID

.PHONY: help doctor install typecheck config pull up fixtures upload-fixture smoke upload-smoke upload upload-capacity upload-capacity-all run status logs down dashboard-url

help:
	@echo "make install                                 Install the load-harness Node tools"
	@echo "make typecheck                               Typecheck k6 and Node TypeScript"
	@echo "make up                                      Start dashboards and the local mock"
	@echo "make smoke                                   Run the one-iteration local smoke test"
	@echo "make fixtures                                Generate every upload fixture"
	@echo "make upload-smoke                            Run two concurrent 1K mock uploads"
	@echo "make upload BASE_URL=https://... FILE_SIZE=1M VUS=10"
	@echo "make upload-capacity BASE_URL=https://... FILE_SIZE=10M"
	@echo "make upload-capacity-all BASE_URL=https://..."
	@echo "make run BASE_URL=https://... VUS=10 DURATION=1m"
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

up:
	@$(COMPOSE) up -d --remove-orphans prometheus grafana mock

fixtures: $(NODE_MODULES_MARKER)
	@$(NODE_BIN)/tsx scripts/generate-upload-fixtures.ts all

upload-fixture: $(NODE_MODULES_MARKER)
	@$(NODE_BIN)/tsx scripts/generate-upload-fixtures.ts "$(FILE_SIZE)"

smoke: up
	@mkdir -p results
	@$(COMPOSE) run --rm -e BASE_URL=http://mock -e TEST_ID="$(TEST_ID)" k6 run -o experimental-prometheus-rw --tag testid="$(TEST_ID)" --summary-export="/results/$(TEST_ID)-summary.json" /scripts/hello-world.ts
	@echo "Smoke result: results/$(TEST_ID)-summary.json"

upload-smoke: FILE_SIZE := 1K
upload-smoke: VUS := 2
upload-smoke: ITERATIONS_PER_VU := 1
upload-smoke: upload-fixture up
	@mkdir -p results
	@$(COMPOSE) run --rm -e BASE_URL=http://mock -e COZY_ACCESS_TOKEN=mock-token -e FILE_SIZE="$(FILE_SIZE)" -e UPLOAD_VUS="$(VUS)" -e ITERATIONS_PER_VU="$(ITERATIONS_PER_VU)" -e CAPACITY_RUN_ID="$(CAPACITY_RUN_ID)" -e CAPACITY_PHASE="smoke" -e MIN_SUCCESS_RATE="$(MIN_SUCCESS_RATE)" -e P95_LIMIT_MS="$(P95_LIMIT_MS)" -e TEST_ID="$(TEST_ID)" k6 run -o experimental-prometheus-rw --tag campaignid="$(or $(CAPACITY_RUN_ID),$(TEST_ID))" --tag concurrency="$(VUS)" --tag filesize="$(FILE_SIZE)" --tag phase="smoke" --tag testid="$(TEST_ID)" --summary-export="/results/$(TEST_ID)-summary.json" /scripts/concurrent-uploads.ts
	@echo "Upload smoke result: results/$(TEST_ID)-summary.json"

upload: upload-fixture up
	@mkdir -p results
	@$(COMPOSE) run --rm -e BASE_URL="$(BASE_URL)" $(if $(filter http://mock,$(BASE_URL)),-e COZY_ACCESS_TOKEN=mock-token,) -e FILE_SIZE="$(FILE_SIZE)" -e UPLOAD_VUS="$(VUS)" -e ITERATIONS_PER_VU="$(ITERATIONS_PER_VU)" -e CAPACITY_RUN_ID="$(CAPACITY_RUN_ID)" -e CAPACITY_PHASE="$(CAPACITY_PHASE)" -e MIN_SUCCESS_RATE="$(MIN_SUCCESS_RATE)" -e P95_LIMIT_MS="$(P95_LIMIT_MS)" -e MAX_DURATION="$(MAX_DURATION)" -e UPLOAD_TIMEOUT="$(UPLOAD_TIMEOUT)" -e TEST_ID="$(TEST_ID)" k6 run -o experimental-prometheus-rw --tag campaignid="$(or $(CAPACITY_RUN_ID),$(TEST_ID))" --tag concurrency="$(VUS)" --tag filesize="$(FILE_SIZE)" --tag phase="$(CAPACITY_PHASE)" --tag testid="$(TEST_ID)" --summary-export="/results/$(TEST_ID)-summary.json" /scripts/concurrent-uploads.ts
	@echo "Upload result: results/$(TEST_ID)-summary.json"

upload-capacity: $(NODE_MODULES_MARKER)
	@BASE_URL="$(BASE_URL)" FILE_SIZE="$(FILE_SIZE)" MAX_VUS="$(MAX_VUS)" LATENCY_MULTIPLIER="$(LATENCY_MULTIPLIER)" MIN_SUCCESS_RATE="$(MIN_SUCCESS_RATE)" CONFIRMATION_ITERATIONS="$(CONFIRMATION_ITERATIONS)" CAPACITY_RUN_ID="$(CAPACITY_RUN_ID)" FORCE_CAPACITY_RUN="$(FORCE_CAPACITY_RUN)" $(NODE_BIN)/tsx scripts/find-upload-capacity.ts

upload-capacity-all:
	@for size in 1K 100K 1M 10M 100M 1G; do \
		$(MAKE) upload-capacity BASE_URL="$(BASE_URL)" FILE_SIZE="$$size" LATENCY_MULTIPLIER="$(LATENCY_MULTIPLIER)" MIN_SUCCESS_RATE="$(MIN_SUCCESS_RATE)" CONFIRMATION_ITERATIONS="$(CONFIRMATION_ITERATIONS)" FORCE_CAPACITY_RUN="$(FORCE_CAPACITY_RUN)" || exit $$?; \
	done

run: up
	@test -f "scenarios/$(SCENARIO)" || { echo "Unknown scenario: scenarios/$(SCENARIO)" >&2; exit 1; }
	@mkdir -p results
	@$(COMPOSE) run --rm -e BASE_URL="$(BASE_URL)" -e K6_VUS="$(VUS)" -e K6_DURATION="$(DURATION)" -e TEST_ID="$(TEST_ID)" k6 run -o experimental-prometheus-rw --tag testid="$(TEST_ID)" --summary-export="/results/$(TEST_ID)-summary.json" "/scripts/$(SCENARIO)"
	@echo "Run result: results/$(TEST_ID)-summary.json"

status:
	@$(COMPOSE) ps

logs:
	@$(COMPOSE) logs --tail=200

down:
	@$(COMPOSE) down

dashboard-url:
	@port="$$( $(COMPOSE) port grafana 3000 | sed 's/.*://' )"; echo "http://localhost:$$port/"
