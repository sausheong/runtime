# Runtime — on-prem durable agent platform.
#
# The `go` CLI is the source of truth in this repo. Dependencies are pinned in
# go.mod, so a standalone clone builds without a sibling source checkout.
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
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REVISION    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
IMAGE       ?= runtime

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

# ---- Container image (all binaries) ----
# CHART_APPVERSION is the chart's appVersion; the chart's default image.tag falls
# back to it, so we also tag the image with it — otherwise a default `helm install`
# (image.tag unset ⇒ runtime:<appVersion>) would reference a tag we never built.
CHART_APPVERSION ?= $(shell awk '/^appVersion:/{gsub(/"/,"",$$2); print $$2}' deploy/charts/runtime/Chart.yaml)
.PHONY: docker-image
docker-image: ## Build the all-binaries image from this repository
	docker build -f deploy/Dockerfile \
		--build-arg VERSION=$(VERSION) --build-arg REVISION=$(REVISION) \
		-t $(IMAGE):$(VERSION) -t $(IMAGE):$(CHART_APPVERSION) -t $(IMAGE):latest .

# ---- Helm chart ----
CHART ?= deploy/charts/runtime

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart
	helm lint $(CHART) --set secrets.pgDsn=postgres://x:x@h:5432/d?sslmode=disable

.PHONY: helm-template
helm-template: ## Render the chart with a dummy DSN (quick check)
	helm template r $(CHART) --set secrets.pgDsn=postgres://x:x@h:5432/d?sslmode=disable

.PHONY: helm-deps
helm-deps: ## Vendor + unpack chart dependencies (Postgres subchart)
	helm dependency update $(CHART)
	@# helm v4 requires the subchart UNPACKED as a dir, not just the .tgz, to
	@# render/install (a vendored .tgz alone fails 'missing in charts/ directory').
	cd $(CHART)/charts && for t in *.tgz; do [ -e "$$t" ] || continue; tar -xzf "$$t" && rm -f "$$t"; done

.PHONY: helm-package
helm-package: helm-deps ## Package the chart into dist/
	mkdir -p dist
	helm package $(CHART) -d dist

# ---- Sandbox image ----
.PHONY: sandbox-image
sandbox-image: ## Build the bundled sandbox image (runtime-sandbox:latest)
	docker build -f deploy/sandbox.Dockerfile -t runtime-sandbox:latest deploy/

.PHONY: browser-image
browser-image: ## Build the bundled browser image (runtime-browser:latest)
	docker build -f deploy/browser.Dockerfile -t runtime-browser:latest deploy/

# ---- Full stack (Postgres + control plane) ----
.PHONY: compose-up-dev
compose-up-dev: ## Bring up the dev stack (postgres+runtimed) via docker-compose.full.yml
	docker compose -f deploy/docker-compose.full.yml up --build

.PHONY: compose-down-dev
compose-down-dev: ## Tear down the dev stack
	docker compose -f deploy/docker-compose.full.yml down

# ---- Turnkey single-node compose (v1.0 surface) ----
COMPOSE_DIR ?= deploy/compose
FORCE       ?=

.PHONY: compose-init
compose-init: ## Generate deploy/compose/.env with fresh secrets (FORCE=--force to regenerate)
	$(COMPOSE_DIR)/init.sh $(FORCE)

.PHONY: compose-build
compose-build: ## Build all turnkey images (incl. sandbox/browser sibling images)
	cd $(COMPOSE_DIR) && docker compose --profile build-only build

.PHONY: compose-up
compose-up: ## Bring up the turnkey stack (all six pillars)
	cd $(COMPOSE_DIR) && docker compose up

.PHONY: compose-down
compose-down: ## Tear down the turnkey stack (PRESERVES data)
	cd $(COMPOSE_DIR) && docker compose down

.PHONY: compose-reset
compose-reset: ## Tear down the turnkey stack AND wipe data volumes
	cd $(COMPOSE_DIR) && docker compose down -v

# ---- Clean ----
.PHONY: clean
clean: ## Remove built binaries
	rm -rf $(BIN_DIR)
	go clean
