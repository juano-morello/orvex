# Orvex build orchestration (ADR-007). Add targets (proto, dev, audit,
# test-integration) only when they become meaningful.

GO ?= go
BINARIES := gateway engine crashctl
BIN_DIR := bin
MODULE := github.com/juano-morello/orvex
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X $(MODULE)/internal/buildinfo.Version=$(VERSION)
GOTESTFLAGS ?= -race

# Independent Crash v1 golden-vector oracle (Python 3.11+, standard library only).
PYTHON ?= python3
GOLDEN_GEN := internal/game/crash/v1/testdata/golden/generate.py

# Pinned linter version. Keep in sync with .github/workflows/ci.yml.
GOLANGCI_LINT ?= golangci-lint
GOLANGCI_LINT_VERSION := v2.14.0

BUILD_TARGETS := $(addprefix build-,$(BINARIES))

.PHONY: all build $(BUILD_TARGETS) test lint golden-check clean

all: build

build: $(BUILD_TARGETS)

# Static pattern rule: plain pattern rules are skipped for .PHONY targets.
$(BUILD_TARGETS): build-%:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$* ./cmd/$*

test:
	$(GO) test $(GOTESTFLAGS) ./...

lint:
	@found="$$($(GOLANGCI_LINT) version --short 2>/dev/null)"; \
	if [ "v$$found" != "$(GOLANGCI_LINT_VERSION)" ]; then \
		echo "golangci-lint $(GOLANGCI_LINT_VERSION) required (found: '$${found:-none}')." >&2; \
		echo "Install: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)" >&2; \
		exit 1; \
	fi
	$(GOLANGCI_LINT) run ./...

golden-check:
	$(PYTHON) $(GOLDEN_GEN) --check

clean:
	rm -rf $(BIN_DIR)
