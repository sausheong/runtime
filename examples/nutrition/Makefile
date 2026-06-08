# SG Nutrition Investigator — example agent.
#
# A worked example of deploying a harness-native agent into the runtime
# platform. This Makefile drives the whole loop: build the platform binaries,
# run the control plane with this agent, and invoke it (text or image).
#
# Prerequisites:
#   - Postgres reachable at PG_DSN (from the repo root: `make pg-up`, or use Postgres.app)
#   - LLM access via a LiteLLM proxy (or OpenAI): OPENAI_API_KEY + OPENAI_BASE_URL + OPENAI_MODEL.
#
# LLM credentials: copy .env.example to .env and fill in your proxy key. This
# Makefile auto-loads .env (gitignored), so `make run` points at the proxy
# without you exporting anything. .env wins over whatever is in your shell —
# important if you have a stray OPENAI_API_KEY set.
#
# Quick start:
#   cp .env.example .env        # then edit .env with your proxy key
#   make run                    # start the control plane hosting this agent
#   make demo-text              # (in another shell) investigate a pasted label
#   make demo-image IMAGE=label.jpeg   # investigate a label photo

# Repo root (this Makefile lives in examples/nutrition/).
ROOT := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/../..)

# Load local LLM credentials if present (gitignored). Assignments here override
# the inherited shell environment, so the proxy key in .env beats a stray
# OPENAI_API_KEY in your shell.
-include .env

# ---- Configuration (override on the command line) ----
BIN_DIR   ?= $(ROOT)/bin
PG_DSN    ?= postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable
CTL_ADDR  ?= :8080
BASE_URL  ?= http://localhost:8080
AGENT     ?= nutrition
CONFIG    ?= $(ROOT)/runtime.nutrition.yaml
DATA_DIR  ?= $(ROOT)/data
# Where this agent writes its cross-run memory (learned aliases + product verdicts).
# A label photo to investigate (override: make demo-image IMAGE=/path/to/label.jpeg).
IMAGE     ?= $(ROOT)/../agents_sdk/milo.jpeg

# ---- LLM config (LiteLLM proxy by default; override in .env or on the CLI) ----
OPENAI_BASE_URL ?= https://litellm-stg.aip.gov.sg
OPENAI_MODEL    ?= gpt-5.4
# OPENAI_API_KEY has no default — set it in .env (see check-env).

# Export so the agentd subprocess (spawned by runtimed) inherits them.
export OPENAI_API_KEY OPENAI_BASE_URL OPENAI_MODEL

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@echo "SG Nutrition Investigator — make targets:"
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

# ---- Build & test ----
.PHONY: build
build: ## Build the platform binaries (delegates to the repo-root Makefile)
	$(MAKE) -C $(ROOT) build

.PHONY: test
test: ## Run this agent's unit tests
	cd $(ROOT) && go test ./examples/nutrition/...

.PHONY: test-live
test-live: ## Run the live one-turn smoke (requires OPENAI_API_KEY)
	cd $(ROOT) && go test -tags live ./examples/nutrition/ -run TestLiveOneTurn -v

# ---- Run ----
.PHONY: check-env
check-env: ## Verify OPENAI_API_KEY is set (the agent needs a real model)
	@if [ -z "$(OPENAI_API_KEY)" ]; then \
		echo "error: OPENAI_API_KEY is not set."; \
		echo "  Fix: cp .env.example .env  and put your LiteLLM proxy key in it."; \
		echo "  (.env is auto-loaded; OPENAI_BASE_URL defaults to $(OPENAI_BASE_URL))"; \
		exit 1; \
	fi
	@echo "LLM config: model=$(OPENAI_MODEL) base_url=$(OPENAI_BASE_URL)"

.PHONY: run
run: build check-env ## Build, then run the control plane hosting the nutrition agent
	@mkdir -p $(DATA_DIR)
	RUNTIME_PG_DSN="$(PG_DSN)" \
	RUNTIME_CTL_ADDR="$(CTL_ADDR)" \
	RUNTIME_AGENTD_BIN="$(BIN_DIR)/agentd" \
	RUNTIME_CONFIG="$(CONFIG)" \
	RUNTIME_NUTRITION_DATA_DIR="$(DATA_DIR)" \
	$(BIN_DIR)/runtimed

# ---- Invoke (run these in a second shell, while `make run` is up) ----
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

# ---- Clean ----
.PHONY: clean
clean: ## Remove this agent's runtime data (memory file)
	rm -rf $(DATA_DIR)
