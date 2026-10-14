# Orvex

Orvex is a deterministic, audit-first multiplayer Crash game platform. It uses simulated credits only.

Status: scaffold ([LBT-5](https://linear.app/lbtworks/issue/LBT-5/scaffold-go-module-and-repository-conventions)), the game contract ([LBT-6](https://linear.app/lbtworks/issue/LBT-6/implement-generic-game-contract-and-versioned-adapter)), the frozen Crash v1 math ([LBT-7](https://linear.app/lbtworks/issue/LBT-7/freeze-crash-mathematical-specification-and-golden-vectors)), the Crash v1 round rules ([LBT-8](https://linear.app/lbtworks/issue/LBT-8/implement-crash-decideevolve-and-lifecycle-invariants)) and the canonical codec, hash chain and audit CLI ([LBT-9](https://linear.app/lbtworks/issue/LBT-9/build-canonical-event-codec-and-audit-replay-cli)). No runtime, persistence or transport exists yet.

## Layout

- `cmd/gateway`: client-facing gateway service (scaffold)
- `cmd/engine`: game execution service (scaffold)
- `cmd/crashctl`: replay/audit CLI: `crashctl audit verify|replay|chain` (LBT-9)
- `internal/game/crash/v1`: Crash v1 rules: frozen arithmetic, fairness derivation and golden vectors (ADR-004, LBT-7); round lifecycle Decide/Evolve, command authority and public projection (ADR-003, LBT-8); canonical codec (ADR-001, LBT-9)
- `internal/buildinfo`: build identifier stamped at link time
- `internal/canonjson`: Orvex Canonical JSON (RFC 8785 subset): encoder and strict decoder (LBT-9)
- `internal/audit/chain`: chain records, link hash and state hash
- `internal/audit/replay`: state replay and decision replay through the rules registry
- `internal/audit/verification`: history reader, staged verifier and golden histories
- `internal/game`: typed game contract, canonical codec contract, deterministic input envelope, versioned adapter and rules registry (ADR-003)
- `docs/adr`: architecture decision records ADR-001 to ADR-007

## Development

Prerequisites: Go 1.26, golangci-lint v2.14.0 and Python 3.11 or newer (standard library only; used by `make golden-check`).

- `make build`: build all binaries into `bin/` (or `make build-gateway`, `make build-engine`, `make build-crashctl`)
- `make test`: run tests with the race detector
- `make lint`: run golangci-lint
- `make golden-check`: regenerate the Crash v1 golden vectors and the audit golden histories with their independent Python oracles and compare them with the committed files
- `make clean`: remove build output

See [AGENTS.md](AGENTS.md) for contributor and agent guidance and [docs/adr/](docs/adr/) for the architectural decisions.
