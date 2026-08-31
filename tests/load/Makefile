SHELL := /bin/bash
COMPOSE := docker compose
SCENARIO ?= http.js
BASE_URL ?= http://mock
VUS ?= 1
DURATION ?= 10s
TEST_ID ?= $(shell date -u +%Y%m%dT%H%M%SZ)
HOST_UID ?= $(shell id -u)
HOST_GID ?= $(shell id -g)

export HOST_UID
export HOST_GID

.PHONY: help doctor config pull up smoke run status logs down dashboard-url

help:
	@echo "make up                                      Start dashboards and the local mock"
	@echo "make smoke                                   Run the one-iteration local smoke test"
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

smoke: up
	@mkdir -p results
	@$(COMPOSE) run --rm -e BASE_URL=http://mock -e TEST_ID="$(TEST_ID)" k6 run -o experimental-prometheus-rw --tag testid="$(TEST_ID)" --summary-export="/results/$(TEST_ID)-summary.json" /scripts/hello-world.js
	@echo "Smoke result: results/$(TEST_ID)-summary.json"

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
