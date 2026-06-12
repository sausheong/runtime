# Runtime — on-prem durable agent platform.
#
# The `go` CLI is the source of truth in this repo (the module uses a
# `replace github.com/sausheong/harness => ../harness` directive, which confuses
# IDE/LSP tooling but not `go build`/`go test`). These targets wrap the commands
# you actually run day to day.
#
# Quick start:
#   make build           # build agentd, runtimed, runtimectl, sandboxd into ./bin
#   make test            # hermetic unit tests
#   make test-integration  # needs Postgres (make pg-up)
#   make run             # build + run the control plane locally

# ---- Configuration (override on the command line, e.g. `make run CTL_ADDR=:9090`) ----
BIN_DIR     ?= bin
PG_DSN      ?= postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable
CTL_ADDR    ?= :8080
CONFIG      ?= runtime.yaml
COMPOSE     ?= docker compose -f deploy/docker-compose.yml
GOFLAGS     ?=

BINS := agentd runtimed runtimectl sandboxd

.DEFAULT_GOAL := help

# ---- Help ----
.PHONY: help
help: ## Show this help
	@echo "Runtime — make targets:"
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

# ---- Build ----
.PHONY: build
build: $(addprefix $(BIN_DIR)/,$(BINS)) ## Build all binaries into ./bin

$(BIN_DIR)/%: FORCE
	@mkdir -p $(BIN_DIR)
	go build $(GOFLAGS) -o $@ ./cmd/$*

FORCE:

.PHONY: install
install: ## go install all commands into GOBIN/GOPATH
	go install $(GOFLAGS) ./cmd/...

# ---- Test ----
.PHONY: test
test: ## Run hermetic unit tests (no network, no DB)
	go test $(GOFLAGS) ./...

.PHONY: test-integration
test-integration: ## Run integration tests (requires Postgres at PG_DSN; see pg-up)
	go test $(GOFLAGS) -tags integration ./test/ -count=1 -timeout 300s

.PHONY: test-live
test-live: ## Run live tests against a real LLM (requires OPENAI_API_KEY)
	go test $(GOFLAGS) -tags live ./... -count=1 -v

.PHONY: test-all
test-all: test test-integration ## Run unit + integration tests

.PHONY: cover
cover: ## Unit tests with a coverage summary
	go test $(GOFLAGS) -cover ./...

# ---- Quality ----
.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: fmt
fmt: ## Format all Go source
	gofmt -w .

.PHONY: fmt-check
fmt-check: ## Fail if any Go source is not gofmt-clean
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: tidy
tidy: ## go mod tidy
	go mod tidy

.PHONY: check
check: fmt-check vet test ## fmt-check + vet + unit tests (CI gate)

# ---- Run ----
.PHONY: run
run: build ## Build, then run the control plane locally (RUNTIME_CONFIG=$(CONFIG))
	RUNTIME_PG_DSN="$(PG_DSN)" \
	RUNTIME_CTL_ADDR="$(CTL_ADDR)" \
	RUNTIME_AGENTD_BIN="./$(BIN_DIR)/agentd" \
	RUNTIME_CONFIG="$(CONFIG)" \
	./$(BIN_DIR)/runtimed

# ---- Postgres (local dev via Docker; Postgres.app users can skip these) ----
.PHONY: pg-up
pg-up: ## Start a local Postgres (pgvector) for tests/dev
	$(COMPOSE) up -d postgres

.PHONY: pg-down
pg-down: ## Stop the local Postgres
	$(COMPOSE) down

# ---- Sandbox image ----
.PHONY: sandbox-image
sandbox-image: ## Build the bundled sandbox image (runtime-sandbox:latest)
	docker build -f deploy/sandbox.Dockerfile -t runtime-sandbox:latest deploy/

.PHONY: browser-image
browser-image: ## Build the bundled browser image (runtime-browser:latest)
	docker build -f deploy/browser.Dockerfile -t runtime-browser:latest .

# ---- Full stack (Postgres + control plane) ----
.PHONY: compose-up
compose-up: ## Bring up the full stack (Postgres + runtimed) via Docker
	docker compose -f deploy/docker-compose.full.yml up --build

.PHONY: compose-down
compose-down: ## Tear down the full stack
	docker compose -f deploy/docker-compose.full.yml down

# ---- Clean ----
.PHONY: clean
clean: ## Remove built binaries
	rm -rf $(BIN_DIR)
	go clean
