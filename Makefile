# Orvex build orchestration (ADR-007). Add targets (proto, dev, audit) only
# when they become meaningful.

GO ?= go
BINARIES := gateway engine crashctl
BIN_DIR := bin
MODULE := github.com/juano-morello/orvex
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X $(MODULE)/internal/buildinfo.Version=$(VERSION)
GOTESTFLAGS ?= -race

# Independent golden oracles (Python 3.11+, standard library only): one for the
# Crash v1 math vectors, one for the audit golden histories (format v1), one
# for the format v2 histories with ledger rows.
PYTHON ?= python3
GOLDEN_GEN := internal/game/crash/v1/testdata/golden/generate.py
AUDIT_GOLDEN_GEN := internal/audit/verification/testdata/golden/generate.py
AUDIT_GOLDEN_V2_GEN := internal/audit/verification/testdata/golden-v2/generate.py

# Pinned linter version. Keep in sync with .github/workflows/ci.yml.
GOLANGCI_LINT ?= golangci-lint
GOLANGCI_LINT_VERSION := v2.14.0

# Fuzzing (ADR-006: nightly extended fuzz). go test -fuzz fuzzes one target
# per invocation, so every target runs alone. FUZZ_TARGETS holds each one as
# <package dir>:<FuzzName>, found in the _test.go files under cmd/ and
# internal/ (fuzz targets live in files without build tags).
FUZZTIME ?= 30s
FUZZ_TARGETS = $(shell grep -rEo --include='*_test.go' '^func Fuzz[A-Za-z0-9_]+' cmd internal | sed -E 's|/[^/]+_test\.go:func |:|' | LC_ALL=C sort)

# Repeat runs the specs require as flakiness checks, against PostgreSQL
# (Testcontainers, needs a Docker daemon): LBT-11 S-14, LBT-12 E-17, LBT-19.
# go test's default 10m timeout applies per package binary; -race on a small
# CI runner can approach it.
REPEAT_TIMEOUT ?= 60m

BUILD_TARGETS := $(addprefix build-,$(BINARIES))

.PHONY: all build $(BUILD_TARGETS) test test-integration test-repeat test-repeat-admission test-repeat-execution test-repeat-export lint golden-check fuzz fuzz-one fuzz-list fuzz-guard-check clean

all: build

build: $(BUILD_TARGETS)

# Static pattern rule: plain pattern rules are skipped for .PHONY targets.
$(BUILD_TARGETS): build-%:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$* ./cmd/$*

test:
	$(GO) test $(GOTESTFLAGS) ./...

# PostgreSQL integration tests (ADR-006): Testcontainers, needs a Docker daemon.
test-integration:
	$(GO) test $(GOTESTFLAGS) -tags integration -count=1 ./...

test-repeat: test-repeat-admission test-repeat-execution test-repeat-export

test-repeat-admission:
	$(GO) test $(GOTESTFLAGS) -tags integration -count=5 -timeout $(REPEAT_TIMEOUT) -run 'Concurrent|HotRound|ManyRounds' ./internal/runtime/admission/

test-repeat-execution:
	$(GO) test $(GOTESTFLAGS) -tags integration -count=5 -timeout $(REPEAT_TIMEOUT) -run 'Conflicting|StaleCache|CrossRound|Takeover|Crash' ./internal/runtime/execution/

test-repeat-export:
	$(GO) test $(GOTESTFLAGS) -tags integration -count=3 -timeout $(REPEAT_TIMEOUT) -run 'TestExport' ./cmd/crashctl/ ./internal/persistence/eventstore/

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
	$(PYTHON) $(AUDIT_GOLDEN_GEN) --check
	$(PYTHON) $(AUDIT_GOLDEN_V2_GEN) --check

fuzz:
	$(if $(FUZZ_TARGETS),,$(error no fuzz targets found under cmd/ or internal/))
	@failed=; for t in $(FUZZ_TARGETS); do \
		$(MAKE) --no-print-directory fuzz-one FUZZ_PKG="$${t%%:*}" FUZZ_TARGET="$${t##*:}" || failed="$$failed $$t"; \
	done; \
	if [ -n "$$failed" ]; then echo "fuzz: failing targets:$$failed" >&2; exit 1; fi

# fuzz-one compares <FUZZ_PKG>:<FUZZ_TARGET> with each listed target as an
# exact string in the shell, so a pattern character (Make's %, a glob, a space
# or a comma) cannot widen the match. It must refuse unknown names itself:
# go test -fuzz exits 0 when the name matches nothing. The + prefix runs the
# check under make -n too.
export FUZZ_PKG FUZZ_TARGET
fuzz-one:
	@+for t in $(FUZZ_TARGETS); do [ "$$t" = "$$FUZZ_PKG:$$FUZZ_TARGET" ] && exit 0; done; \
	printf "unknown fuzz target '%s'; run make fuzz-list\n" "$$FUZZ_PKG:$$FUZZ_TARGET" >&2; exit 2
	$(GO) test $(GOTESTFLAGS) -run '^$$' -fuzz '^$(FUZZ_TARGET)$$' -fuzztime $(FUZZTIME) ./$(FUZZ_PKG)

fuzz-list:
	$(if $(FUZZ_TARGETS),,$(error no fuzz targets found under cmd/ or internal/))
	@printf '%s\n' $(FUZZ_TARGETS)

# Regression check for the fuzz-one guard. GO=true stands in for go, so no Go
# command runs: every bad name must exit 2 with the refusal message, also
# under make -n, and the first listed target must still be accepted.
fuzz-guard-check:
	$(if $(FUZZ_TARGETS),,$(error no fuzz targets found under cmd/ or internal/))
	@reject() { \
		out="$$($(MAKE) --no-print-directory $$3 fuzz-one GO=true FUZZ_PKG="$$1" FUZZ_TARGET="$$2" 2>&1)"; rc=$$?; \
		case "$$rc:$$out" in \
		2:*"unknown fuzz target '$$1:$$2'"*) ;; \
		*) printf "fuzz-guard-check: '%s' not refused (exit %s): %s\n" "$$1:$$2" "$$rc" "$$out" >&2; exit 1 ;; \
		esac; \
	}; \
	reject internal/canonjson 'Fuzz%'; \
	reject 'internal/%' FuzzUnmarshal; \
	reject internal/canonjson 'Fuzz*'; \
	reject internal/canonjson FuzzNope; \
	reject internal/canonjson 'FuzzUnmarshal,FuzzNope'; \
	reject internal/canonjson 'FuzzUnmarshal FuzzNope'; \
	reject internal/canonjson 'Fuzz%' -n; \
	reject internal/canonjson FuzzNope -n; \
	reject '' ''; \
	first='$(firstword $(FUZZ_TARGETS))'; \
	$(MAKE) --no-print-directory fuzz-one GO=true FUZZ_PKG="$${first%%:*}" FUZZ_TARGET="$${first##*:}" >/dev/null || \
		{ echo "fuzz-guard-check: listed target $$first was refused" >&2; exit 1; }; \
	echo "fuzz-guard-check: ok"

clean:
	rm -rf $(BIN_DIR)
