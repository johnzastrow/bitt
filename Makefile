# BitTabby
#
# The build is deliberately small: generate templates, test, build one binary.

BINARY      := bittabby
PKG         := ./cmd/bittabby
TEMPL       := $(shell go env GOPATH)/bin/templ
# The version number itself lives in internal/version; the build only injects
# provenance. A plain `go build` still produces a correctly versioned binary.
VERSIONPKG  := github.com/johnzastrow/bitt/internal/version
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE  := $(shell date -u +%Y-%m-%d 2>/dev/null || echo unknown)
LDFLAGS     := -s -w -X $(VERSIONPKG).Commit=$(COMMIT) -X $(VERSIONPKG).Date=$(BUILD_DATE)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-14s %s\n", $$1, $$2}'

.PHONY: tools
tools: ## Install the templ generator
	go install github.com/a-h/templ/cmd/templ@latest

.PHONY: generate
generate: ## Regenerate templ templates
	$(TEMPL) generate

.PHONY: fmt
fmt: generate ## Format all Go source
	gofmt -w ./cmd ./internal

.PHONY: vet
vet: generate ## Run go vet
	go vet ./...

.PHONY: test
test: generate ## Run the test suite
	go test ./...

.PHONY: race
race: generate ## Run the test suite under the race detector
	go test -race ./...

.PHONY: cover
cover: generate ## Report test coverage per package
	go test -cover ./...

.PHONY: build
build: generate ## Build a static binary into ./bittabby
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) $(PKG)

.PHONY: run
run: generate ## Run locally on :8080 over plain HTTP
	BITT_SECURE_COOKIES=false go run $(PKG)

.PHONY: check
check: fmt vet test ## Format, vet, and test -- run before committing

.PHONY: image
image: ## Build the container image as bittabby:dev
	docker build \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg BUILD_DATE=$(BUILD_DATE) \
	  -t bittabby:dev .

.PHONY: up
up: ## Bring the compose stack up (see docs/DEPLOY.md for the secret it needs)
	docker compose up -d

.PHONY: down
down: ## Stop the compose stack, keeping the data volume
	docker compose down

.PHONY: clean
clean: ## Remove build output
	rm -f $(BINARY)
