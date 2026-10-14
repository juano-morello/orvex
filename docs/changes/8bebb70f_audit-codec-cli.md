# LBT-9: canonical event codec and audit replay CLI

Implements the approved spec `docs/specs/8bebb70f_canonical-codec-audit-cli.md` (decisions A-1..A-20, now marked approved). Crash v1 math, parameters and golden vectors are unchanged.

## What changed
- `internal/canonjson/`: canonical JSON encoder/strict decoder used for hashing and history files.
- `internal/audit/chain/`: hash chain over event records.
- `internal/audit/replay/`: replays a history through the game and reports the resulting state hash.
- `internal/audit/verification/`: verifier for exported JSONL histories (chain, ordering, rules consistency, strict format).
- `internal/game/` (`codec.go`, `contract.go`, `registry.go`): codec interface added to the game contract and registry (an LBT-6 contract change, flagged in spec section 2); `internal/game/crash/v1/codec.go` implements it for Crash v1 (small edits in `round.go`).
- `cmd/crashctl/main.go`: `crashctl audit verify|replay|chain [-expect-head HEX] FILE|-`. Exit codes: 0 ok, 1 verification failed, 2 usage, 3 unsupported, 4 I/O (A-16).
- Second golden oracle `internal/audit/verification/testdata/golden/generate.py` (Python stdlib), `manifest.json`, golden histories (`worked-example`, `empty-round`, `in-progress`, `instant-crash`) and 26 tamper cases under `testdata/golden/tamper/`.
- `Makefile`, `AGENTS.md`, `README.md`: `make golden-check` now covers both oracles; docs updated.
- Tests and fuzz tests alongside each package. Earlier spec drafts were also added under `docs/specs/8bebb70f_audit-codec-implementation*.md`.

## Verify
`make test`, `make lint`, `make build`, `make golden-check`. Try `bin/crashctl audit verify internal/audit/verification/testdata/golden/worked-example.jsonl` (expect exit 0) and a file from `tamper/` (expect exit 1).
