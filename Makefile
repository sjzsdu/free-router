BINARY := free-router
GO ?= go
NPM ?= npm
WEB_DIR := web
VERSION := $(strip $(shell cat VERSION))
BUILD_DIR ?= bin
GOBIN ?= $(shell $(GO) env GOBIN)

ifeq ($(strip $(GOBIN)),)
GOBIN := $(shell $(GO) env GOPATH)/bin
endif

LDFLAGS := -s -w

.DEFAULT_GOAL := help
.PHONY: help build web-install web-build web-check install uninstall daemon-install daemon-start daemon-stop daemon-restart daemon-status daemon-logs daemon-uninstall run discover-free-models validate-free-models test test-formula test-race test-cover vet fmt fmt-check version-check check tidy clean

help: ## Show available targets
	@awk 'BEGIN {FS = ":.*## "; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z0-9_-]+:.*## / {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: web-build ## Build React admin and bin/free-router
	@mkdir -p $(BUILD_DIR)
	$(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) .

web-install: ## Install locked React admin dependencies
	cd $(WEB_DIR) && $(NPM) ci

web-build: ## Build React admin into the Go embedded assets
	@test -d $(WEB_DIR)/node_modules || $(MAKE) web-install
	cd $(WEB_DIR) && $(NPM) run build

web-check: ## Type-check the React admin
	@test -d $(WEB_DIR)/node_modules || $(MAKE) web-install
	cd $(WEB_DIR) && $(NPM) run typecheck

install: web-build ## Install free-router into GOBIN (defaults to GOPATH/bin)
	@mkdir -p $(GOBIN)
	GOBIN=$(GOBIN) $(GO) install -trimpath -ldflags="$(LDFLAGS)" .
	@echo "installed $(BINARY) $(VERSION) to $(GOBIN)/$(BINARY)"

daemon-install: install ## Install binary and start it as a daemon
	$(GOBIN)/$(BINARY) daemon install

daemon-start: ## Start the installed daemon
	$(GOBIN)/$(BINARY) daemon start

daemon-stop: ## Stop the installed daemon
	$(GOBIN)/$(BINARY) daemon stop

daemon-restart: ## Restart the installed daemon
	$(GOBIN)/$(BINARY) daemon restart

daemon-status: ## Show daemon status
	$(GOBIN)/$(BINARY) daemon status

daemon-logs: ## Follow daemon logs
	$(GOBIN)/$(BINARY) daemon logs --follow

daemon-uninstall: ## Stop and remove the daemon
	$(GOBIN)/$(BINARY) daemon uninstall

uninstall: ## Remove the installed binary
	rm -f $(GOBIN)/$(BINARY)

run: web-build ## Run the service
	$(GO) run -ldflags="$(LDFLAGS)" . serve

discover-free-models: ## Concurrently research providers and update the free model manifest
	tt formula run discover-free-models --dir .tt/formulas

validate-free-models: ## Validate the generated free model manifest
	$(GO) run . validate-model-data internal/provider/free-models.json

test: ## Run unit and integration tests
	$(GO) test ./...

test-formula: ## Test free-model discovery normalization and merge rules
	sh scripts/test-free-model-formula.sh

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

check: fmt-check version-check vet test test-formula web-check ## Run all required checks

tidy: ## Update and verify Go module metadata
	$(GO) mod tidy
	$(GO) mod verify

clean: ## Remove build and coverage artifacts
	rm -rf $(BUILD_DIR) coverage.out
