# Protobuf targets (ADR-007: Buf for protobuf). The Makefile includes this
# file. Buf and both plugins run through go run at pinned versions, so only
# Go is needed.
#
# Go version: Buf v1.75.0 declares Go 1.26.7 in its go.mod, while this
# module's go.mod keeps go 1.26.0. Under GOTOOLCHAIN=auto, go run fetches a
# 1.26.7 or later toolchain for these targets. Under GOTOOLCHAIN=local, they
# need Go 1.26.7 or later installed.
#
# Plugin versions live in buf.gen.yaml. protoc-gen-go must equal the
# google.golang.org/protobuf runtime in go.mod. protoc-gen-go-grpc has its own
# version line, independent of the google.golang.org/grpc runtime.

BUF_VERSION := v1.75.0
BUF ?= $(GO) run github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)

.PHONY: proto proto-lint proto-check

# proto: lint the schema and regenerate proto/orvex/v1/*.pb.go.
proto: proto-lint
	$(BUF) generate

proto-lint:
	$(BUF) lint

# proto-check: fail if the committed generated code differs from a fresh
# generation.
proto-check: proto
	@if [ -n "$$(git status --porcelain -- proto)" ]; then \
		echo "generated protobuf code is stale; run make proto and commit the result" >&2; \
		git status --porcelain -- proto >&2; \
		exit 1; \
	fi
