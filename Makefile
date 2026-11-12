.PHONY: build test check-test smoke smoke-writes lint fmt fmt-check vet tidy check clean help

BINARY_NAME := costfluent-mcp
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-s -w -X main.version=$(VERSION)"

GOBIN := $(shell go env GOPATH)/bin
GOLANGCI_LINT_VERSION := v2.14.0
GOLANGCI_LINT_TOOLCHAIN := go1.27.2
GOLANGCI_LINT := $(GOBIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)

# The stdio server's smoke and the CLI's, run by one go test over both packages.
SMOKE_TESTS := TestSmokeAgainstADeployedAPI TestCLISmokeAgainstADeployedAPI
SMOKE_RUN := ^(TestSmokeAgainstADeployedAPI|TestCLISmokeAgainstADeployedAPI)$$

## build: Build both binaries, the server and the CLI, into bin/
build:
	CGO_ENABLED=0 go build -trimpath $(LDFLAGS) -o bin/$(BINARY_NAME) ./cmd/costfluent-mcp
	CGO_ENABLED=0 go build -trimpath $(LDFLAGS) -o bin/costfluent ./cmd/costfluent

## test: Run tests with the race detector and a coverage profile
test:
	go test -race -coverprofile=coverage.out ./...

## check-test: Run tests without leaving a coverage file, unless COVERPROFILE names one to write
check-test:
	go test -race $(if $(COVERPROFILE),-coverprofile=$(COVERPROFILE)) ./...

## smoke: Call every tool, through the server and through the CLI, against a deployed API (needs COSTFLUENT_MCP_SMOKE_API_TOKEN, a full Read only token whose first workspace holds a saved report; COSTFLUENT_MCP_SMOKE_BASE_URL defaults to dev)
# go test passes a -run that selects nothing, as after a test is renamed, split or build-tagged,
# so the smoke counts only when each test's own pass line is in its output.
smoke:
	@test -n "$$COSTFLUENT_MCP_SMOKE_API_TOKEN" || { echo "COSTFLUENT_MCP_SMOKE_API_TOKEN is not set"; exit 1; }
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
		{ go test -count=1 -run '$(SMOKE_RUN)' -v ./cmd/costfluent-mcp ./cmd/costfluent 2>&1; echo $$? >"$$tmp/status"; } | tee "$$tmp/out"; \
		status=$$(cat "$$tmp/status"); test "$$status" = 0 || exit "$$status"; \
		for test in $(SMOKE_TESTS); do \
			grep -q "^--- PASS: $$test " "$$tmp/out" || { echo "make smoke: $$test did not run and pass, so no tool was called through it"; exit 1; }; \
		done

## smoke-writes: Call every write tool once against a deployed API and delete what it created (needs COSTFLUENT_MCP_SMOKE_WRITE_API_TOKEN, a token minted for the run, with every write capability, in an organization that lets AI agents create and change objects and holds an anomaly and an open savings finding; revoke it after)
smoke-writes:
	@test -n "$$COSTFLUENT_MCP_SMOKE_WRITE_API_TOKEN" || { echo "COSTFLUENT_MCP_SMOKE_WRITE_API_TOKEN is not set"; exit 1; }
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
		{ go test -count=1 -run '^TestSmokeWritesAgainstADeployedAPI$$' -v ./cmd/costfluent-mcp 2>&1; echo $$? >"$$tmp/status"; } | tee "$$tmp/out"; \
		status=$$(cat "$$tmp/status"); test "$$status" = 0 || exit "$$status"; \
		grep -q "^--- PASS: TestSmokeWritesAgainstADeployedAPI " "$$tmp/out" || { echo "make smoke-writes: the write smoke did not run and pass"; exit 1; }

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
