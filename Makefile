BINARY := free-router
GO ?= go
NPM ?= npm
WEB_DIR := web
GIT_VERSION := $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')
VERSION ?= $(if $(strip $(GIT_VERSION)),$(strip $(GIT_VERSION)),dev)
BUILD_DIR ?= bin
GOBIN ?= $(shell $(GO) env GOBIN)

ifeq ($(strip $(GOBIN)),)
GOBIN := $(shell $(GO) env GOPATH)/bin
endif

LDFLAGS := -s -w -X main.buildVersion=$(VERSION)

.DEFAULT_GOAL := help
.PHONY: help build web-install web-build web-check web-test install uninstall daemon-install daemon-start daemon-stop daemon-restart daemon-status daemon-logs daemon-uninstall run validate-free-models verify-free-models test test-race test-cover vet fmt fmt-check version-check check tag tidy clean

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

web-test: ## Run React admin behavior tests
	@test -d $(WEB_DIR)/node_modules || $(MAKE) web-install
	cd $(WEB_DIR) && $(NPM) test

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

validate-free-models: ## Validate the generated free model manifest
	$(GO) run . validate-model-data internal/provider/free-models

verify-free-models: ## Probe maintained models with real minimal requests to confirm they are callable (needs keys in .env)
	$(GO) run . validate-models --free-models internal/provider/free-models

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

version-check: ## Verify the binary reports the git-derived version
	@actual="$$($(GO) run -ldflags="$(LDFLAGS)" . version)"; \
	if [ "$$actual" != "$(VERSION)" ]; then echo "version mismatch: expected=$(VERSION), binary=$$actual"; exit 1; fi

tag: ## Create and push a release tag (RELEASE_VERSION=x.y.z, defaults to next patch)
	@version="$(RELEASE_VERSION)"; \
	if [ -z "$$version" ]; then \
		base="$$(git describe --tags --abbrev=0 2>/dev/null || echo v0.0.0)"; \
		version="$$(printf '%s' "$$base" | sed 's/^v//' | awk -F. '{printf "%d.%d.%d", $$1, $$2, $$3 + 1}')"; \
	fi; \
	if git rev-parse "v$$version" >/dev/null 2>&1; then echo "tag v$$version already exists" >&2; exit 1; fi; \
	git tag -a "v$$version" -m "free-router $$version"; \
	git push origin "v$$version"; \
	echo "pushed v$$version, the Release workflow will build and publish it"

check: web-build fmt-check version-check vet test web-check web-test ## Run all required checks

tidy: ## Update and verify Go module metadata
	$(GO) mod tidy
	$(GO) mod verify

clean: ## Remove build and coverage artifacts
	rm -rf $(BUILD_DIR) coverage.out
