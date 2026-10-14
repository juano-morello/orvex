# Orvex

Orvex is a deterministic, audit-first multiplayer Crash game platform. It uses simulated credits only.

Status: scaffold ([LBT-5](https://linear.app/lbtworks/issue/LBT-5/scaffold-go-module-and-repository-conventions)) plus the game contract ([LBT-6](https://linear.app/lbtworks/issue/LBT-6/implement-generic-game-contract-and-versioned-adapter)). No Crash rules, runtime or persistence logic exists yet.

## Layout

- `cmd/gateway`: client-facing gateway service (scaffold)
- `cmd/engine`: game execution service (scaffold)
- `cmd/crashctl`: replay/audit CLI (scaffold)
- `internal/game/crash/v1`: frozen Crash v1 arithmetic, fairness derivation and golden vectors (ADR-004, LBT-7); the Crash lifecycle is not implemented yet
- `internal/buildinfo`: build identifier stamped at link time
- `internal/game`: typed game contract, deterministic input envelope, versioned adapter and rules registry (ADR-003)
- `docs/adr`: architecture decision records ADR-001 to ADR-007

## Development

Prerequisites: Go 1.26, golangci-lint v2.14.0 and Python 3.11 or newer (standard library only; used by `make golden-check`).

- `make build`: build all binaries into `bin/` (or `make build-gateway`, `make build-engine`, `make build-crashctl`)
- `make test`: run tests with the race detector
- `make lint`: run golangci-lint
- `make golden-check`: regenerate the Crash v1 golden vectors with the independent Python oracle and compare them with the committed files
- `make clean`: remove build output

See [AGENTS.md](AGENTS.md) for contributor and agent guidance and [docs/adr/](docs/adr/) for the architectural decisions.
