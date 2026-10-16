# Go module and repository scaffold (LBT-5, ADR-007)

## What changed
The repository now has a single Go module (`github.com/juano-morello/orvex`, `go 1.26.0`) with three independent binaries, a Makefile, a pinned linter, a CI workflow, a mirror of ADR-001 to ADR-007, and agent guidance. No game, runtime or persistence logic exists yet, and the module has no third-party dependencies.

## Where it lives
- `cmd/gateway`, `cmd/engine`, `cmd/crashctl`: thin scaffold binaries. Each accepts `--version` and prints `<name> <version>`. With no flag it prints "not implemented yet" and exits 1. Unexpected arguments or bad flags exit 2. Each has a `main_test.go`. None imports another.
- `internal/buildinfo`: `Version` (default `dev`) and `String(binary)`. The Makefile stamps the version with `-ldflags -X`, using `git describe --tags --always --dirty`.
- `Makefile`: targets `build`, `build-gateway`, `build-engine`, `build-crashctl`, `test`, `lint`, `clean`. Binaries go to `bin/`, which `.gitignore` now ignores. `test` runs `go test -race ./...`. `lint` fails with install instructions unless golangci-lint is exactly v2.14.0.
- `.golangci.yml`: golangci-lint v2, `standard` linters, plus the `gofmt` and `goimports` formatters (local prefix is the module path).
- `.github/workflows/ci.yml`: runs on push to `main` and on pull requests. Three jobs: `test` (`make test`), `lint` (installs golangci-lint v2.14.0, then `make lint`), and `build` (a matrix that runs `make build-<binary>` for each of the three binaries). There is no monorepo orchestrator.
- `docs/adr/0001` to `0007`: one file per ADR, mirroring the source text.
- `AGENTS.md`: ADR table with links, which ADRs to read per area, the rule to flag ADR conflicts explicitly (citing ADR number and section), repository conventions and commands.
- `README.md`: layout and development commands.
- `docs/specs/ebb8f044_go-module-scaffold.md`: the build spec.

## Verify
```
make build     # bin/gateway, bin/engine, bin/crashctl
make test      # use `make test GOTESTFLAGS=` if cgo is unavailable
make lint      # needs golangci-lint v2.14.0
./bin/gateway --version
```

## Things to know
- The linter version is pinned in two places, `Makefile` and `.github/workflows/ci.yml`. Change both together.
- `proto/`, `web/`, `migrations/`, `tests/`, `deployments/` and the targets `proto`, `dev`, `audit` and `test-integration` are intentionally absent, as ADR-007 allows.
- I did not run the build, tests or lint. This write-up comes from reading the diff.
