BINARY_DIR := bin
ORCHESTRATOR_BIN := $(BINARY_DIR)/orchestrator
ROUTER_BIN := $(BINARY_DIR)/router

.PHONY: all build test test-unit test-integration vet lint clean docker-build otm install-otm otm-release test-cli

all: build otm

## build: compile both binaries
build:
	@mkdir -p $(BINARY_DIR)
	go build -o $(ORCHESTRATOR_BIN) ./cmd/orchestrator
	go build -o $(ROUTER_BIN) ./cmd/router
	@echo "✅ Built: $(ORCHESTRATOR_BIN), $(ROUTER_BIN)"

## test: run all tests (unit + integration)
test:
	go test ./... -timeout 120s

## test-unit: run unit tests only (no Docker required)
test-unit:
	go test ./... -short -timeout 30s

## test-integration: run integration tests (requires Docker)
test-integration:
	go test ./internal/integration/... -v -timeout 120s

## test-coverage: run tests with coverage report
test-coverage:
	go test ./... -coverprofile=coverage.out -timeout 120s
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

## vet: run go vet
vet:
	go vet ./...

## tidy: tidy go modules
tidy:
	go mod tidy

## clean: remove build artifacts
clean:
	rm -rf $(BINARY_DIR) coverage.out coverage.html

## docker-build: build Docker images
docker-build:
	docker build -f Dockerfile.orchestrator -t orchestrator:latest .
	docker build -f Dockerfile.router -t router:latest .

## run-orchestrator: run orchestrator locally (requires env vars)
run-orchestrator: build
	$(ORCHESTRATOR_BIN)

## run-router: run router locally (requires env vars)
run-router: build
	$(ROUTER_BIN)

help:
	@grep -E '^## ' Makefile | sed 's/## //'

OTM_BIN := $(BINARY_DIR)/otm
VERSION := v0.1.0
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS := -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)"

## otm: build CLI binary
otm:
	@mkdir -p $(BINARY_DIR)
	go build $(LDFLAGS) -o $(OTM_BIN) ./cmd/otm
	@echo "✅ Built: $(OTM_BIN)"

## install-otm: install CLI to $GOPATH/bin or /usr/local/bin
install-otm: otm
	@if [ -n "$(GOPATH)" ]; then \
		mkdir -p $(GOPATH)/bin && \
		cp $(OTM_BIN) $(GOPATH)/bin/otm && \
		echo "✅ Installed to $(GOPATH)/bin/otm"; \
	else \
		sudo cp $(OTM_BIN) /usr/local/bin/otm && \
		echo "✅ Installed to /usr/local/bin/otm"; \
	fi

## otm-release: build multi-platform binaries
otm-release:
	@mkdir -p $(BINARY_DIR)/release
	GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o $(BINARY_DIR)/release/otm-darwin-amd64 ./cmd/otm
	GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o $(BINARY_DIR)/release/otm-darwin-arm64 ./cmd/otm
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BINARY_DIR)/release/otm-linux-amd64 ./cmd/otm
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o $(BINARY_DIR)/release/otm-linux-arm64 ./cmd/otm
	@echo "✅ Release binaries built in $(BINARY_DIR)/release/"
	@ls -lh $(BINARY_DIR)/release/

## test-cli: run CLI tests
test-cli:
	go test ./cmd/otm/... ./internal/cli/... -v -timeout 30s
