# Orvex

Orvex is a deterministic, audit-first multiplayer Crash game platform. It uses simulated credits only.

Status: scaffold ([LBT-5](https://linear.app/lbtworks/issue/LBT-5/scaffold-go-module-and-repository-conventions)). No game, runtime or persistence logic exists yet.

## Layout

- `cmd/gateway`: client-facing gateway service (scaffold)
- `cmd/engine`: game execution service (scaffold)
- `cmd/crashctl`: replay/audit CLI (scaffold)
- `internal/buildinfo`: build identifier stamped at link time
- `docs/adr`: architecture decision records ADR-001 to ADR-007

## Development

Prerequisites: Go 1.26 and golangci-lint v2.14.0.

- `make build`: build all binaries into `bin/` (or `make build-gateway`, `make build-engine`, `make build-crashctl`)
- `make test`: run tests with the race detector
- `make lint`: run golangci-lint
- `make clean`: remove build output

See [AGENTS.md](AGENTS.md) for contributor and agent guidance and [docs/adr/](docs/adr/) for the architectural decisions.
