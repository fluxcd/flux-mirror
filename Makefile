# Copyright 2026 The Flux Authors
# SPDX-License-Identifier: Apache-2.0

# Makefile for building and testing the flux-mirror CLI.

DOCKER_IMAGE ?= ghcr.io/fluxcd/flux-mirror:latest-dev
VERSION_DEV ?=0.0.0-$(shell git rev-parse --abbrev-ref HEAD)-$(shell git rev-parse --short HEAD)-$(shell date +%s)
GO_TEST_ARGS ?=
GO_RUN_ARGS ?=

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

# Get the currently used golang install path
# (in GOPATH/bin, unless GOBIN is set).
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

.PHONY: all
all: test build ## Run test and build targets.

##@ Development

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: tidy
tidy: ## Run go mod tidy.
	go mod tidy

.PHONY: test
test: tidy fmt vet ## Run all unit tests.
	go test ./... $(GO_TEST_ARGS) -coverprofile cover.out

.PHONY: lint
lint: golangci-lint ## Run golangci linters.
	$(GOLANGCI_LINT) run

.PHONY: build
build: tidy fmt vet ## Build CLI binary.
	CGO_ENABLED=0 go build -ldflags="-s -w -X main.VERSION=$(VERSION_DEV)" -o ./bin/flux-mirror ./cmd/flux-mirror/

.PHONY: docker-build
docker-build: ## Build docker image with the CLI.
	docker build -t $(DOCKER_IMAGE) --build-arg VERSION=$(VERSION_DEV) -f Dockerfile .

# Local OCI registry for end-to-end smoke tests.
REGISTRY_NAME ?= flux-mirror-registry
REGISTRY_PORT ?= 5050

.PHONY: registry-up
registry-up: ## Start a local OCI registry (registry:3) on host port 5050.
	@if docker ps -a --format '{{.Names}}' | grep -qx $(REGISTRY_NAME); then \
		echo "Registry $(REGISTRY_NAME) already exists; restarting clean."; \
		docker rm -f $(REGISTRY_NAME) >/dev/null; \
	fi
	docker run -d --rm --name $(REGISTRY_NAME) -p $(REGISTRY_PORT):5000 \
		-e REGISTRY_STORAGE_DELETE_ENABLED=true registry:3
	@echo "Registry up at localhost:$(REGISTRY_PORT)"

.PHONY: registry-down
registry-down: ## Stop the local OCI registry.
	@if docker ps -a --format '{{.Names}}' | grep -qx $(REGISTRY_NAME); then \
		docker rm -f $(REGISTRY_NAME) >/dev/null; \
		echo "Registry $(REGISTRY_NAME) stopped."; \
	else \
		echo "Registry $(REGISTRY_NAME) is not running."; \
	fi

.PHONY: install
install: test lint build ## Test, lint, build and copy the binary to GOBIN.
	cp bin/flux-mirror $(GOBIN)

.PHONY: run
run: build ## Run CLI binary.
	./bin/flux-mirror $(GO_RUN_ARGS)

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)
GOVULNCHECK ?= $(LOCALBIN)/govulncheck

## Tool Versions
GOLANGCI_LINT_VERSION ?= v2.12.2

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

.PHONY: govulncheck
govulncheck: $(GOVULNCHECK) ## Run govulncheck.
$(GOVULNCHECK): $(LOCALBIN)
	$(call go-install-tool,$(GOVULNCHECK),golang.org/x/vuln/cmd/govulncheck,latest)
	@$(GOVULNCHECK) ./...

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary (ideally with version)
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f $(1) ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv "$$(echo "$(1)" | sed "s/-$(3)$$//")" $(1) ;\
}
endef

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
