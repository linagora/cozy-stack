SHELL := /bin/bash
COMPOSE := docker compose
SCENARIO ?= http.js
BASE_URL ?= http://mock
VUS ?= 1
DURATION ?= 10s
FILE_SIZE ?= 1K
ITERATIONS_PER_VU ?= 1
MAX_DURATION ?= 30m
UPLOAD_TIMEOUT ?= 30m
TEST_ID ?= $(shell date -u +%Y%m%dT%H%M%SZ)
HOST_UID ?= $(shell id -u)
HOST_GID ?= $(shell id -g)

export HOST_UID
export HOST_GID

.PHONY: help doctor config pull up fixtures upload-fixture smoke upload-smoke upload run status logs down dashboard-url

help:
	@echo "make up                                      Start dashboards and the local mock"
	@echo "make smoke                                   Run the one-iteration local smoke test"
	@echo "make fixtures                                Generate every upload fixture"
	@echo "make upload-smoke                            Run two concurrent 1K mock uploads"
	@echo "make upload BASE_URL=https://... FILE_SIZE=1M VUS=10"
	@echo "make run BASE_URL=https://... VUS=10 DURATION=1m"
	@echo "make status | make logs | make down"

doctor:
	@docker info >/dev/null
	@$(COMPOSE) version

config:
	@$(COMPOSE) config --quiet

pull:
	@$(COMPOSE) --profile runner pull

up:
	@$(COMPOSE) up -d --remove-orphans prometheus grafana mock

fixtures:
	@./scripts/generate-upload-fixtures.sh all

upload-fixture:
	@./scripts/generate-upload-fixtures.sh "$(FILE_SIZE)"

smoke: up
	@mkdir -p results
	@$(COMPOSE) run --rm -e BASE_URL=http://mock -e TEST_ID="$(TEST_ID)" k6 run -o experimental-prometheus-rw --tag testid="$(TEST_ID)" --summary-export="/results/$(TEST_ID)-summary.json" /scripts/hello-world.js
	@echo "Smoke result: results/$(TEST_ID)-summary.json"

upload-smoke: FILE_SIZE := 1K
upload-smoke: VUS := 2
upload-smoke: ITERATIONS_PER_VU := 1
upload-smoke: upload-fixture up
	@mkdir -p results
	@$(COMPOSE) run --rm -e BASE_URL=http://mock -e COZY_ACCESS_TOKEN=mock-token -e FILE_SIZE="$(FILE_SIZE)" -e UPLOAD_VUS="$(VUS)" -e ITERATIONS_PER_VU="$(ITERATIONS_PER_VU)" -e TEST_ID="$(TEST_ID)" k6 run -o experimental-prometheus-rw --tag testid="$(TEST_ID)" --summary-export="/results/$(TEST_ID)-summary.json" /scripts/concurrent-uploads.js
	@echo "Upload smoke result: results/$(TEST_ID)-summary.json"

upload: upload-fixture up
	@mkdir -p results
	@$(COMPOSE) run --rm -e BASE_URL="$(BASE_URL)" -e FILE_SIZE="$(FILE_SIZE)" -e UPLOAD_VUS="$(VUS)" -e ITERATIONS_PER_VU="$(ITERATIONS_PER_VU)" -e MAX_DURATION="$(MAX_DURATION)" -e UPLOAD_TIMEOUT="$(UPLOAD_TIMEOUT)" -e TEST_ID="$(TEST_ID)" k6 run -o experimental-prometheus-rw --tag testid="$(TEST_ID)" --summary-export="/results/$(TEST_ID)-summary.json" /scripts/concurrent-uploads.js
	@echo "Upload result: results/$(TEST_ID)-summary.json"

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
