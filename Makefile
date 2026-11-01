# hvmc build entry points. Tools are pinned in tools/go.mod.

GO        ?= go
TOOL      := $(GO) tool -modfile=$(CURDIR)/tools/go.mod
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X github.com/matfax/hvmc/internal/version.Version=$(VERSION)
GOFLAGS_BUILD := -trimpath -ldflags "$(LDFLAGS)"

export CGO_ENABLED := 0

.PHONY: all
all: generate lint test build

##@ Generate

.PHONY: generate
generate: manifests proto ## Run all code generators.
	$(TOOL) controller-gen object paths="./..."

.PHONY: manifests
manifests: ## Generate CRDs and RBAC (filled in by WP 1.2).

.PHONY: proto
proto: ## Generate gRPC code (filled in by WP 1.4).

##@ Verify

.PHONY: test
test: ## Run unit tests with the race detector.
	CGO_ENABLED=1 $(GO) test -race ./...

LEDGER_COVERAGE_MIN ?= 95

.PHONY: test-ledger-coverage
test-ledger-coverage: ## Fail when internal/ledger statement coverage is below 95 %.
	@mkdir -p $(CURDIR)/bin
	$(GO) test -covermode=atomic -coverprofile=$(CURDIR)/bin/ledger.cover ./internal/ledger/
	@$(GO) tool cover -func=$(CURDIR)/bin/ledger.cover | awk -v min=$(LEDGER_COVERAGE_MIN) \
		'/^total:/ { sub("%", "", $$3); if ($$3 + 0 < min) { printf "internal/ledger coverage %s%% is below %s%%\n", $$3, min; exit 1 } \
		printf "internal/ledger coverage %s%% (minimum %s%%)\n", $$3, min }'

.PHONY: lint
lint: ## Run golangci-lint.
	$(TOOL) golangci-lint run ./...

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart (from WP 2.0).
	@if [ -d charts/hvmc ]; then helm lint charts/hvmc; else echo "no chart yet"; fi

##@ Build

.PHONY: build
build: ## Build hvmc for the host platform.
	$(GO) build $(GOFLAGS_BUILD) -o bin/hvmc ./cmd/hvmc

.PHONY: build-agent-windows
build-agent-windows: ## Build hvmc.exe for Windows agents.
	GOOS=windows GOARCH=amd64 $(GO) build $(GOFLAGS_BUILD) -o bin/hvmc.exe ./cmd/hvmc

.PHONY: build-agent-linux
build-agent-linux: ## Build hvmc for Linux agents.
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS_BUILD) -o bin/hvmc-linux-amd64 ./cmd/hvmc

.PHONY: ko-build
ko-build: ## Build the coordinator image locally without pushing.
	KO_DOCKER_REPO=hvmc.invalid/hvmc VERSION=$(VERSION) $(TOOL) ko build --push=false --bare --platform=linux/amd64 ./cmd/hvmc

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ { printf "  %-22s %s\n", $$1, $$2 } /^##@/ { printf "\n%s\n", substr($$0, 5) }' $(MAKEFILE_LIST)
