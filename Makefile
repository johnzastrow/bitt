# BitTabby
#
# The build is deliberately small: generate templates, test, build one binary.

BINARY      := bittabby
PKG         := ./cmd/bittabby
VERSION     ?= dev
TEMPL       := $(shell go env GOPATH)/bin/templ
LDFLAGS     := -s -w -X main.version=$(VERSION)

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

.PHONY: clean
clean: ## Remove build output
	rm -f $(BINARY)
