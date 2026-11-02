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
HELM      := $(TOOL) helm
CHART     := charts/hvmc
# Kubernetes version helm lint and helm template assume. Helm built by
# `go tool` lacks the ldflags of Helm's release builds and would assume v1.20,
# below the chart's kubeVersion; use the minor of the k8s.io modules instead.
HELM_KUBE_VERSION ?= $(shell $(GO) list -m -f '{{.Version}}' k8s.io/apimachinery | sed -E 's/^v0\.([0-9]+)\.([0-9]+).*/1.\1.\2/')

export CGO_ENABLED := 0

# envtest binaries (kube-apiserver, etcd) match the Kubernetes minor of the
# k8s.io modules in go.mod: k8s.io/apimachinery v0.37.x -> 1.37.x.
ENVTEST_K8S_VERSION ?= $(shell $(GO) list -m -f '{{.Version}}' k8s.io/apimachinery | sed -E 's/^v0\.([0-9]+)\..*/1.\1.x/')
ENVTEST_DIR         ?= $(CURDIR)/.envtest
SETUP_ENVTEST       := $(TOOL) setup-envtest use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_DIR) -p path

.PHONY: all
all: generate lint test build

##@ Generate

.PHONY: generate
generate: deepcopy manifests proto ## Run all code generators.

.PHONY: deepcopy
deepcopy: ## Generate DeepCopy methods for the API types.
	$(TOOL) controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: manifests
manifests: ## Generate CRDs into config/crd/bases and the Helm chart's templates.
	$(TOOL) controller-gen crd paths="./api/..." output:crd:artifacts:config=config/crd/bases
	sh hack/chart-crds.sh

.PHONY: proto
proto: ## Generate gRPC code from proto/ into internal/agentapi/ (buf).
	rm -rf internal/agentapi/hvmc
	$(BUF) generate

##@ Verify

.PHONY: test
test: ## Run unit and envtest tests with the race detector.
	@assets="$$($(SETUP_ENVTEST))" && echo "KUBEBUILDER_ASSETS=$$assets" && \
		KUBEBUILDER_ASSETS="$$assets" CGO_ENABLED=1 $(GO) test -race ./...

.PHONY: envtest
envtest: ## Download envtest binaries and print their directory (export as KUBEBUILDER_ASSETS).
	@$(SETUP_ENVTEST)

LEDGER_COVERAGE_MIN ?= 95

.PHONY: test-ledger-coverage
test-ledger-coverage: ## Fail when internal/ledger statement coverage is below 95 %.
	@mkdir -p $(CURDIR)/bin
	$(GO) test -covermode=atomic -coverprofile=$(CURDIR)/bin/ledger.cover ./internal/ledger/
	@$(GO) tool cover -func=$(CURDIR)/bin/ledger.cover | awk -v min=$(LEDGER_COVERAGE_MIN) \
		'/^total:/ { sub("%", "", $$3); if ($$3 + 0 < min) { printf "internal/ledger coverage %s%% is below %s%%\n", $$3, min; exit 1 } \
		printf "internal/ledger coverage %s%% (minimum %s%%)\n", $$3, min }'

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
helm-lint: ## Lint the Helm chart (default and ci/ values) and compare it with the golden files.
	$(HELM) lint --strict --kube-version $(HELM_KUBE_VERSION) $(CHART)
	@for f in $(CHART)/ci/*-values.yaml; do \
		echo "$(HELM) lint --strict --kube-version $(HELM_KUBE_VERSION) --values $$f $(CHART)"; \
		$(HELM) lint --strict --kube-version $(HELM_KUBE_VERSION) --values $$f $(CHART) || exit 1; \
	done
	$(GO) test -count=1 ./test/chart/

.PHONY: helm-golden
helm-golden: ## Regenerate the chart's golden files (test/chart/testdata).
	$(GO) test -count=1 ./test/chart/ -update

# ct runs helm, yamllint and yamale from PATH: helm is a wrapper around the
# pinned Go tool, yamllint and yamale come hash-pinned into a virtualenv.
CT_DIR := $(CURDIR)/bin/ct

$(CT_DIR)/venv/.installed: hack/chart-testing/requirements.txt
	rm -rf $(CT_DIR)/venv
	python3 -m venv $(CT_DIR)/venv
	$(CT_DIR)/venv/bin/pip install --quiet --require-hashes --no-deps -r $<
	@touch $@

.PHONY: ct-lint
ct-lint: $(CT_DIR)/venv/.installed ## Run chart-testing lint (yamllint, yamale, helm lint) on the chart.
	@mkdir -p $(CT_DIR)/bin
	@printf '#!/bin/sh\nexec %s "$$@"\n' '$(HELM)' >$(CT_DIR)/bin/helm && chmod +x $(CT_DIR)/bin/helm
	@etc="$$(cd tools && $(GO) list -m -f '{{.Dir}}' github.com/helm/chart-testing/v3)/etc" && \
		PATH="$(CT_DIR)/bin:$(CT_DIR)/venv/bin:$$PATH" $(TOOL) ct lint --config hack/chart-testing/ct.yaml \
			--lint-conf "$$etc/lintconf.yaml" --chart-yaml-schema "$$etc/chart_schema.yaml" \
			--helm-lint-extra-args "--strict --kube-version $(HELM_KUBE_VERSION)"

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
