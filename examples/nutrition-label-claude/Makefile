# SG Nutrition Investigator (Claude Agent SDK) — hosted on runtime via the shim.
#
# The THIRD implementation of the investigator, and the C1-M2 reuse proof: the
# control plane execs `uv run python serve.py` (this dir) as a supervised agent
# speaking the HTTP/SSE contract, reusing the runtime_contract library UNCHANGED.
#
# Prerequisites:
#   - Postgres reachable at PG_DSN (from the repo root: `make pg-up`, or Postgres.app).
#     (Required by runtimed/control-plane startup; this agent itself uses SQLite.)
#   - LLM access: ANTHROPIC_API_KEY + ANTHROPIC_BASE_URL + ANTHROPIC_MODEL
#     (LiteLLM proxy or api.anthropic.com).
#
# LLM credentials: copy .env.example to .env and fill in your key. This Makefile
# auto-loads .env (gitignored) and exports it, so the serve.py subprocess (spawned
# by runtimed) inherits it. .env wins over a stray ANTHROPIC_* in your shell.
#
# Quick start:
#   cp .env.example .env        # then edit .env with your proxy key
#   make run                    # start the control plane hosting this agent
#   make demo-text              # (second shell) investigate a pasted label
#   make demo-image IMAGE=milo.jpeg   # investigate a label photo

# Repo root (this Makefile lives in examples/nutrition-label-claude/).
ROOT := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/../..)

# Load local LLM credentials if present (gitignored). Assignments here override
# the inherited shell environment, so the proxy key in .env beats a stray one.
-include .env

# ---- Configuration (override on the command line) ----
BIN_DIR   ?= $(ROOT)/bin
PG_DSN    ?= postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable
CTL_ADDR  ?= :8080
BASE_URL  ?= http://localhost:8080
AGENT     ?= nutrition-claude
CONFIG    ?= $(ROOT)/examples/nutrition-label-claude/runtime.nutrition-claude.yaml
IMAGE     ?= $(ROOT)/examples/nutrition-label-claude/milo.jpeg

# ---- LLM config (LiteLLM proxy by default; override in .env or on the CLI) ----
ANTHROPIC_BASE_URL ?= https://your-llm-proxy.example.com
ANTHROPIC_MODEL    ?= claude-sonnet-4-6
# ANTHROPIC_API_KEY has no default — set it in .env (see check-env).

# Export so the serve.py subprocess (spawned by runtimed) inherits them.
export ANTHROPIC_API_KEY ANTHROPIC_BASE_URL ANTHROPIC_MODEL

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@echo "SG Nutrition Investigator (Claude Agent SDK) — make targets:"
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

.PHONY: demo-text
demo-text: ## Investigate a pasted label (text); streams the verdict
	$(BIN_DIR)/runtimectl invoke --agent $(AGENT) \
		"Investigate this label (text): Product: Milo UHT. Ingredients: water, skimmed milk, sugar, cocoa, malt extract, soy lecithin (E322), vitamins. Sugar 6g/100ml, saturated fat 1.5g/100ml. It is a beverage."

.PHONY: demo-image
demo-image: ## Investigate a label PHOTO (IMAGE=path); posts base64 + streams the verdict
	@test -f "$(IMAGE)" || { echo "error: image not found: $(IMAGE) (override with IMAGE=/path/to/label.jpeg)"; exit 1; }
	@echo "Posting $(IMAGE) to $(AGENT)..."
	@IMG=$$(base64 -i "$(IMAGE)"); \
	SID=$$(curl -s $(BASE_URL)/agents/$(AGENT)/sessions \
		-d "{\"message\":\"Investigate this label.\",\"image_b64\":\"$$IMG\",\"image_mime\":\"image/jpeg\"}" \
		| sed -E 's/.*"session_id":"([^"]+)".*/\1/'); \
	test -n "$$SID" || { echo "error: no session id returned (is the control plane running? make run)"; exit 1; }; \
	echo "session: $$SID"; \
	curl -sN "$(BASE_URL)/agents/$(AGENT)/sessions/$$SID/stream?since=0"

.PHONY: sessions
sessions: ## List this agent's sessions
	$(BIN_DIR)/runtimectl sessions --agent $(AGENT)

.PHONY: clean
clean: ## Remove this agent's runtime data (memory + shim db + SDK transcripts)
	rm -f agent_memory.json shim.db shim.db-wal shim.db-shm
	rm -rf claude-config
