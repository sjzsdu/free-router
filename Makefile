BINARY := free-router
GO ?= go
VERSION := $(strip $(shell cat VERSION))
BUILD_DIR ?= bin
GOBIN ?= $(shell $(GO) env GOBIN)

ifeq ($(strip $(GOBIN)),)
GOBIN := $(shell $(GO) env GOPATH)/bin
endif

LDFLAGS := -s -w

.DEFAULT_GOAL := help
.PHONY: help build install uninstall run test test-race test-cover vet fmt fmt-check version-check check tidy clean

help: ## Show available targets
	@awk 'BEGIN {FS = ":.*## "; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z0-9_-]+:.*## / {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build bin/free-router
	@mkdir -p $(BUILD_DIR)
	$(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) .

install: ## Install free-router into GOBIN (defaults to GOPATH/bin)
	@mkdir -p $(GOBIN)
	GOBIN=$(GOBIN) $(GO) install -trimpath -ldflags="$(LDFLAGS)" .
	@echo "installed $(BINARY) $(VERSION) to $(GOBIN)/$(BINARY)"

uninstall: ## Remove the installed binary
	rm -f $(GOBIN)/$(BINARY)

run: ## Run the service
	$(GO) run -ldflags="$(LDFLAGS)" .

test: ## Run unit and integration tests
	$(GO) test ./...

test-race: ## Run tests with the race detector
	$(GO) test -race ./...

test-cover: ## Generate coverage.out and print coverage summary
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out

vet: ## Run go vet
	$(GO) vet ./...

fmt: ## Format Go source files
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

fmt-check: ## Fail if Go source files are not formatted
	@files="$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*'))"; \
	if [ -n "$$files" ]; then echo "files need gofmt:"; echo "$$files"; exit 1; fi

version-check: ## Verify the binary reports the VERSION file value
	@actual="$$($(GO) run . version)"; \
	if [ "$$actual" != "$(VERSION)" ]; then echo "version mismatch: VERSION=$(VERSION), binary=$$actual"; exit 1; fi

check: fmt-check version-check vet test ## Run all required checks

tidy: ## Update and verify Go module metadata
	$(GO) mod tidy
	$(GO) mod verify

clean: ## Remove build and coverage artifacts
	rm -rf $(BUILD_DIR) coverage.out
