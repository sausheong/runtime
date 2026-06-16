# Minimal Claude Agent SDK assistant — hosted on runtime via the Python shim.
#
# The control plane execs `uv run python serve.py` (this dir) as a supervised
# agent speaking the HTTP/SSE contract, reusing the runtime_contract library.
#
# Prerequisites:
#   - Postgres reachable at PG_DSN (from the repo root: `make pg-up`, or Postgres.app).
#     (Required by runtimed/control-plane startup; this agent itself uses SQLite.)
#   - LLM access: ANTHROPIC_API_KEY + ANTHROPIC_BASE_URL + ANTHROPIC_MODEL
#     (LiteLLM proxy or api.anthropic.com).
#
# Quick start:
#   cp .env.example .env        # then edit .env with your proxy key
#   make run                    # start the control plane hosting this agent
#   make conformance            # (second shell) the contract acceptance gate
#   make demo                   # (second shell) say hello

# Repo root (this Makefile lives in examples/hello-claude/).
ROOT := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/../..)

# Load local LLM credentials if present (gitignored). Assignments here override
# the inherited shell environment, so the proxy key in .env beats a stray one.
-include .env

# ---- Configuration (override on the command line) ----
BIN_DIR   ?= $(ROOT)/bin
PG_DSN    ?= postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable
CTL_ADDR  ?= :8080
BASE_URL  ?= http://localhost:8080
AGENT     ?= hello-claude
CONFIG    ?= $(ROOT)/examples/hello-claude/runtime.hello-claude.yaml

# ---- LLM config (LiteLLM proxy by default; override in .env or on the CLI) ----
ANTHROPIC_BASE_URL ?= https://litellm-stg.aip.gov.sg
ANTHROPIC_MODEL    ?= claude-sonnet-4-6-asia-southeast1
# ANTHROPIC_API_KEY has no default — set it in .env (see check-env).

# Export so the serve.py subprocess (spawned by runtimed) inherits them.
export ANTHROPIC_API_KEY ANTHROPIC_BASE_URL ANTHROPIC_MODEL

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@echo "Minimal Claude Agent SDK assistant — make targets:"
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the platform binaries (delegates to the repo-root Makefile)
	$(MAKE) -C $(ROOT) build

.PHONY: sync
sync: ## Install Python deps for this example (incl. the runtime-contract path dep)
	uv sync

.PHONY: test
test: ## Run this example's hermetic unit tests (no key/network)
	uv run pytest -q

.PHONY: check-env
check-env: ## Verify ANTHROPIC_API_KEY is set (the agent needs a real model)
	@if [ -z "$(ANTHROPIC_API_KEY)" ]; then \
		echo "error: ANTHROPIC_API_KEY is not set."; \
		echo "  Fix: cp .env.example .env  and put your LiteLLM proxy key in it."; \
		echo "  (.env is auto-loaded; ANTHROPIC_BASE_URL defaults to $(ANTHROPIC_BASE_URL))"; \
		exit 1; \
	fi
	@echo "LLM config: model=$(ANTHROPIC_MODEL) base_url=$(ANTHROPIC_BASE_URL)"

.PHONY: run
run: build sync check-env ## Build, sync, then run the control plane hosting this agent
	cd $(ROOT) && \
	RUNTIME_PG_DSN="$(PG_DSN)" \
	RUNTIME_CTL_ADDR="$(CTL_ADDR)" \
	RUNTIME_AGENTD_BIN="$(BIN_DIR)/agentd" \
	RUNTIME_CONFIG="$(CONFIG)" \
	$(BIN_DIR)/runtimed

.PHONY: conformance
conformance: ## Run the contract conformance suite against this agent
	$(BIN_DIR)/runtimectl conformance --agent $(AGENT)

.PHONY: demo
demo: ## Say hello; streams the reply
	$(BIN_DIR)/runtimectl invoke --agent $(AGENT) "Say hello and tell me one fun fact."

.PHONY: sessions
sessions: ## List this agent's sessions
	$(BIN_DIR)/runtimectl sessions --agent $(AGENT)

.PHONY: clean
clean: ## Remove this agent's runtime data (shim db + SDK transcripts)
	rm -f shim.db shim.db-wal shim.db-shm
	rm -rf claude-config
