.PHONY: help build install run test test-coverage fmt fmt-check vet lint vuln check release-snapshot clean deps deps-list deps-update generate

BINARY := mycli
CMD_PATH := ./cmd/mycli
BIN_DIR := bin

GOLANGCI_LINT_VERSION := v2.12.2

help:
	@echo "Available targets:"
	@echo "  build            - Build the $(BINARY) binary with version information"
	@echo "  install          - Install to GOBIN with version information"
	@echo "  run              - Build and run (make run ARGS=\"example --name Dev\")"
	@echo "  test             - Run all tests with -race"
	@echo "  test-coverage    - Run tests with coverage report"
	@echo "  fmt              - Format code (rewrites files)"
	@echo "  fmt-check        - Check formatting without modifying files"
	@echo "  vet              - Run go vet"
	@echo "  lint             - Run golangci-lint (pinned version)"
	@echo "  vuln             - Scan dependencies for known vulnerabilities"
	@echo "  check            - Run all quality checks (fmt-check, vet, lint, test) — non-mutating"
	@echo "  release-snapshot - Verify GoReleaser config with a local snapshot build (no publish)"
	@echo "  clean            - Clean build artifacts"
	@echo "  deps             - Tidy and verify modules"
	@echo "  deps-list        - Show module dependencies"
	@echo "  deps-update      - Update all dependencies to latest"
	@echo "  generate         - Run go generate"

build:
	@mkdir -p $(BIN_DIR)
	@VERSION=$$(git describe --tags --always --dirty 2>/dev/null || echo "dev"); \
	COMMIT=$$(git rev-parse --short HEAD 2>/dev/null || echo "none"); \
	DATE=$$(date -u +"%Y-%m-%dT%H:%M:%SZ"); \
	go build -ldflags "-X main.version=$$VERSION -X main.commit=$$COMMIT -X main.date=$$DATE" \
		-o $(BIN_DIR)/$(BINARY) $(CMD_PATH) && \
	echo "Built $(BIN_DIR)/$(BINARY) ($$VERSION)"

install:
	@VERSION=$$(git describe --tags --always --dirty 2>/dev/null || echo "dev"); \
	COMMIT=$$(git rev-parse --short HEAD 2>/dev/null || echo "none"); \
	DATE=$$(date -u +"%Y-%m-%dT%H:%M:%SZ"); \
	go install -ldflags "-X main.version=$$VERSION -X main.commit=$$COMMIT -X main.date=$$DATE" $(CMD_PATH) && { \
		BINDIR=$$(go env GOBIN); \
		[ -n "$$BINDIR" ] || BINDIR="$$(go env GOPATH)/bin"; \
		echo "Installed $(BINARY) to $$BINDIR"; \
	}

run: build
	@./$(BIN_DIR)/$(BINARY) $(ARGS)

test:
	@go test -race ./...

test-coverage:
	@go test -race -covermode=atomic -coverprofile=coverage.out ./...
	@go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

fmt:
	@go fmt ./...
	@go tool goimports -w .
	@echo "Formatted all Go files"

fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "Files need formatting (run 'make fmt'):"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

vet:
	@go vet ./...

lint:
	@go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

vuln:
	@go run golang.org/x/vuln/cmd/govulncheck@latest ./...

check: fmt-check vet lint test

release-snapshot:
	@goreleaser release --snapshot --clean

clean:
	@rm -rf $(BIN_DIR) dist coverage.out coverage.html
	@echo "Cleaned build artifacts"

deps:
	@go mod tidy
	@go mod verify

deps-list:
	@go list -m all

deps-update:
	@go get -u ./...
	@go mod tidy

generate:
	@go generate ./...
