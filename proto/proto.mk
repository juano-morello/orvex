# Protobuf targets (ADR-007: Buf for protobuf). The Makefile includes this
# file. Buf and both plugins run through go run at pinned versions, so only
# Go is needed; the plugin versions live in buf.gen.yaml and must match
# google.golang.org/protobuf and google.golang.org/grpc in go.mod.

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
