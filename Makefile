# hvmc build entry points. Tools are pinned in tools/go.mod.

GO        ?= go
TOOL      := $(GO) tool -modfile=$(CURDIR)/tools/go.mod
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X github.com/matfax/hvmc/internal/version.Version=$(VERSION)
GOFLAGS_BUILD := -trimpath -ldflags "$(LDFLAGS)"
BUF       := $(TOOL) buf
# Input `buf breaking` compares against: a git ref or a directory holding the
# old tree (CI passes an extracted copy of main).
BUF_AGAINST ?= .git\#branch=main

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
proto: ## Generate gRPC code from proto/ into internal/agentapi/ (buf).
	rm -rf internal/agentapi/hvmc
	$(BUF) generate

##@ Verify

.PHONY: test
test: ## Run unit tests with the race detector.
	CGO_ENABLED=1 $(GO) test -race ./...

.PHONY: lint
lint: lint-go lint-proto ## Run all linters.

.PHONY: lint-go
lint-go: ## Run golangci-lint.
	$(TOOL) golangci-lint run ./...

.PHONY: lint-proto
lint-proto: ## Run buf lint on proto/.
	$(BUF) lint

.PHONY: proto-breaking
proto-breaking: ## Check proto/ for breaking changes against BUF_AGAINST.
	$(BUF) breaking --against '$(BUF_AGAINST)'

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
