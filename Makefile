.PHONY: build test check-test smoke lint fmt fmt-check vet tidy check clean help

BINARY_NAME := costfluent-mcp
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-s -w -X main.version=$(VERSION)"

GOBIN := $(shell go env GOPATH)/bin
GOLANGCI_LINT_VERSION := v2.14.0
GOLANGCI_LINT_TOOLCHAIN := go1.27.2
GOLANGCI_LINT := $(GOBIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)

## build: Build the binary into bin/
build:
	CGO_ENABLED=0 go build -trimpath $(LDFLAGS) -o bin/$(BINARY_NAME) ./cmd/costfluent-mcp

## test: Run tests with the race detector and a coverage profile
test:
	go test -race -coverprofile=coverage.out ./...

## check-test: Run tests without leaving a coverage file
check-test:
	go test -race ./...

## smoke: Call every tool against a deployed API (needs COSTFLUENT_MCP_SMOKE_API_TOKEN; COSTFLUENT_MCP_SMOKE_BASE_URL defaults to dev)
smoke:
	@test -n "$$COSTFLUENT_MCP_SMOKE_API_TOKEN" || { echo "COSTFLUENT_MCP_SMOKE_API_TOKEN is not set"; exit 1; }
	go test -count=1 -run '^TestSmokeAgainstADeployedAPI$$' -v ./cmd/costfluent-mcp

## lint: Run the linter
lint: $(GOLANGCI_LINT)
	GOTOOLCHAIN=$(GOLANGCI_LINT_TOOLCHAIN) $(GOLANGCI_LINT) run ./...

$(GOLANGCI_LINT):
	@mkdir -p $(dir $(GOLANGCI_LINT)); \
		tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
		GOTOOLCHAIN=$(GOLANGCI_LINT_TOOLCHAIN) GOBIN="$$tmp" go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION); \
		mv "$$tmp/golangci-lint" $(GOLANGCI_LINT)

## fmt: Format code
fmt:
	gofmt -s -w .

## fmt-check: Fail when Go source is not formatted, without rewriting it
fmt-check:
	@test -z "$$(gofmt -s -l .)" || { gofmt -s -l .; exit 1; }

## vet: Run go vet
vet:
	go vet ./...

## tidy: Tidy and verify dependencies
tidy:
	go mod tidy
	go mod verify

## check: Run non-mutating build, format, vet, lint, and tests
check: fmt-check vet lint check-test
	go build ./...

## clean: Remove build artifacts
clean:
	rm -rf bin/ dist/
	rm -f coverage.out

## help: Show this help
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
