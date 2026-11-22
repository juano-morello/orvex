# Orvex

Orvex is a deterministic, audit-first multiplayer Crash game platform. It uses simulated credits only.

Status: scaffold ([LBT-5](https://linear.app/lbtworks/issue/LBT-5/scaffold-go-module-and-repository-conventions)), the game contract ([LBT-6](https://linear.app/lbtworks/issue/LBT-6/implement-generic-game-contract-and-versioned-adapter)), the frozen Crash v1 math ([LBT-7](https://linear.app/lbtworks/issue/LBT-7/freeze-crash-mathematical-specification-and-golden-vectors)), the Crash v1 round rules ([LBT-8](https://linear.app/lbtworks/issue/LBT-8/implement-crash-decideevolve-and-lifecycle-invariants)), the canonical codec, hash chain and audit CLI ([LBT-9](https://linear.app/lbtworks/issue/LBT-9/build-canonical-event-codec-and-audit-replay-cli)), the PostgreSQL schema and migration tooling ([LBT-10](https://linear.app/lbtworks/issue/LBT-10/implement-postgresql-schema-and-migration-tooling)), the durable per-round admission sequencer ([LBT-11](https://linear.app/lbtworks/issue/LBT-11/implement-durable-per-round-postgresql-admission-sequencer)), the atomic execution commit with the simulated-credit ledger, outbox and round creation ([LBT-12](https://linear.app/lbtworks/issue/LBT-12/implement-atomic-execution-commit-ledger-and-outbox)), the audit history export CLI ([LBT-19](https://linear.app/lbtworks/issue/LBT-19/crashctl-audit-export-round-id)) and the audit ledger stage with history format v2 ([LBT-18](https://linear.app/lbtworks/issue/LBT-18/audit-ledger-stage-history-format-v2)). The first playable milestone adds a single-engine round loop, a gRPC boundary, an HTTP gateway, and operator-issued demo credits.

## Layout

- `cmd/gateway`: client-facing HTTP gateway for commands, results and the public round stream
- `cmd/engine`: game execution service and round scheduler
- `cmd/crashctl` runs audit, migration and demo credit grant commands.
- `internal/game/crash/v1`: Crash v1 rules: frozen arithmetic, fairness derivation and golden vectors (ADR-004, LBT-7); round lifecycle Decide/Evolve, command authority and public projection (ADR-003, LBT-8); canonical codec (ADR-001, LBT-9)
- `internal/buildinfo`: build identifier stamped at link time
- `internal/canonjson`: Orvex Canonical JSON (RFC 8785 subset): encoder and strict decoder (LBT-9)
- `internal/audit/chain`: chain records, link hash and state hash
- `internal/audit/replay`: state replay and decision replay through the rules registry
- `internal/audit/verification`: history reader (formats v1 and v2), staged verifier with the ledger stage, golden histories
- `internal/persistence/postgres`: PostgreSQL migration runner, SQLSTATE constants and transaction options (ADR-005, LBT-10); `pgtest`: Testcontainers harness, catalog fingerprint and audit-history loader for integration tests; `pgtest/roundtest`: crash/v1 round and golden-history seeding through the executor for integration tests (LBT-24)
- `internal/persistence/eventstore`: audit history export from the database (v1 LBT-10, v2 with ledger rows LBT-18)
- `internal/runtime/admission`: durable per-round admission sequencer (Transaction A): dedupe, sequence and effective time, admission window and closure barrier, credit holds, crash/v1 admission policy (ADR-002, ADR-005, LBT-11)
- `internal/runtime/lifecycle`: round creation, scheduling and restart recovery
- `internal/runtime/relay`: transactional outbox notifications for live streams
- `internal/transport`: [player HTTP/SSE and internal gRPC protocol](docs/architecture/gateway-protocol.md)
- `internal/runtime/execution`: per-round command executor (Transaction B): atomic commit of the chain record, events, ledger postings, hold releases, outbox message and snapshot under a compare-and-set and fencing token; cold load by chain replay; round creation; crash/v1 execution policy (ADR-005, LBT-12)
- `internal/ledger`: pure simulated-credit posting rules and deterministic ledger IDs (ADR-004, LBT-12)
- `internal/game`: typed game contract, canonical codec contract, deterministic input envelope, versioned adapter and rules registry (ADR-003)
- `migrations`: embedded PostgreSQL schema migrations (immutable once merged)
- `deployments/docker`: local PostgreSQL for development (Docker Compose)
- `docs/adr`: architecture decision records ADR-001 to ADR-007
- `docs/architecture`: durable design documentation ([persistence](docs/architecture/persistence.md))

## Development

Prerequisites: Go 1.26, golangci-lint v2.14.0, Python 3.11 or newer (standard library only; used by `make golden-check`) and a Docker-compatible daemon (used by `make test-integration`).

- `make build`: build all binaries into `bin/` (or `make build-gateway`, `make build-engine`, `make build-crashctl`)
- `make test`: run tests with the race detector
- `make test-acceptance`: start PostgreSQL and the real services, play a two-player round, recover a lost cash-out receipt, and verify the settled v2 history. Requires Docker.
- `make test-integration`: run the unit tests plus the PostgreSQL integration tests (Testcontainers, pinned PostgreSQL 18 image); fails without a Docker daemon. On hosts without cgo: `make test-integration GOTESTFLAGS=`
- `make lint`: run golangci-lint
- `make proto-check` checks the protobuf schema and verifies that regeneration reproduces the committed Go files.
- `make golden-check`: regenerate the Crash v1 golden vectors and the audit golden histories with their independent Python oracles and compare them with the committed files
- `make fuzz`: fuzz every Go fuzz target for `FUZZTIME` each (default `30s`), one target per `go test` run; `make fuzz-list` lists them, `make fuzz-one FUZZ_PKG=internal/canonjson FUZZ_TARGET=FuzzUnmarshal` runs one and `make fuzz-guard-check` checks that `fuzz-one` refuses unknown names; on Go 1.26.x a run can fail when `FUZZTIME` ends with only `context deadline exceeded` and no failing input (golang/go#75804, fixed in Go 1.27.0), so the nightly fuzz jobs use Go 1.27.x; locally, prefix the command with `GOTOOLCHAIN=go1.27.2`
- `make test-repeat`: the specs' repeated PostgreSQL runs (`make test-repeat-admission`, `make test-repeat-execution`, `make test-repeat-export`); needs a Docker daemon
- CI: `.github/workflows/ci.yml` on every push and pull request; `.github/workflows/nightly.yml` nightly and on demand (every fuzz target, on Go 1.27.x, and the repeat runs)
- `make clean`: remove build output
- `crashctl migrate up [-to N]`, `crashctl migrate down -to N`, `crashctl migrate status`: apply, revert or list schema migrations on the database named by `ORVEX_DATABASE_URL`
- `crashctl audit export [-format NAME] [-unsettled] ROUND_ID FILE` exports v1 by default. Select `orvex/audit-history/v2` to include the ledger. An unsettled export requires access to `orvex.round_secrets` and the keyring named by `ORVEX_SECRET_KEYRING_FILE`.
- `crashctl ledger grant GRANT_KEY PLAYER_ID CREDITS` adds up to 10¹² demo credits per grant. Retrying the same grant succeeds. Reusing a key with a different player or amount exits with code 7.

Local PostgreSQL:

- `docker compose -f deployments/docker/compose.yaml up -d --wait`
- `export ORVEX_DATABASE_URL='postgres://orvex:orvex@127.0.0.1:55432/orvex?sslmode=disable'`, then `./bin/crashctl migrate up`
- `docker compose -f deployments/docker/compose.yaml down -v`

See [AGENTS.md](AGENTS.md) for contributor and agent guidance and [docs/adr/](docs/adr/) for the architectural decisions.

## Play a local round

Start the local PostgreSQL instance described above, set `ORVEX_DATABASE_URL`, then run:

```sh
make build
./bin/crashctl migrate up
./bin/crashctl ledger grant demo-alice-1 alice 1000
./bin/crashctl ledger grant demo-bob-1 bob 1000
```

Keep the keyring with the demo database across restarts. The engine needs the original key to
resume an unsettled round. If the key is lost, restore it from backup or start a fresh demo
database. The following commands reuse the credential files on later runs.

```sh
demo_dir="${XDG_STATE_HOME:-$HOME/.local/state}/orvex-demo"
mkdir -p -m 700 "$demo_dir"
export ORVEX_SECRET_KEYRING_FILE="$demo_dir/keyring"
export ORVEX_GATEWAY_TOKEN_FILE="$demo_dir/gateway-token"
export ORVEX_PLAYERS_FILE="$demo_dir/players.json"
(
  umask 077
  python3 - <<'PY'
import base64
import json
import os
import secrets
from pathlib import Path

key = base64.b64encode(secrets.token_bytes(32)).decode("ascii")
players = {secrets.token_hex(32): name for name in ("alice", "bob")}
files = {
    "ORVEX_SECRET_KEYRING_FILE": f"active local\nkey local {key}\n",
    "ORVEX_GATEWAY_TOKEN_FILE": secrets.token_hex(32) + "\n",
    "ORVEX_PLAYERS_FILE": json.dumps(players),
}
for variable, contents in files.items():
    try:
        with Path(os.environ[variable]).open("x", encoding="ascii") as file:
            file.write(contents)
    except FileExistsError:
        pass
PY
)
export ORVEX_ENGINE_ADDR=127.0.0.1:7070 ORVEX_GATEWAY_ADDR=127.0.0.1:8080
export ORVEX_BETTING_WINDOW=30s
./bin/engine &
engine_pid=$!
env -u ORVEX_DATABASE_URL -u ORVEX_SECRET_KEYRING_FILE ./bin/gateway &
gateway_pid=$!
```

After both services report ready, the following Bash commands place Alice's bet and follow the
round. They use `curl`, `jq` and Python. Bob can use the same commands with his own token.

```sh
token=$(jq -r 'to_entries[] | select(.value == "alice") | .key' "$ORVEX_PLAYERS_FILE")
api="http://$ORVEX_GATEWAY_ADDR/v1"
auth="Authorization: Bearer $token"
round=$(curl -fsS --retry 8 --retry-delay 1 --retry-all-errors "$api/rounds/current" | jq -er .roundId)
bet_id=$(python3 -c 'import uuid; print(uuid.uuid4())')
curl -sS -H "$auth" -H 'Content-Type: application/json' \
  -d "{\"clientCommandId\":\"$bet_id\",\"placeBet\":{\"betCredits\":\"100\"}}" "$api/rounds/$round/commands"
curl -NsS "$api/rounds/$round/stream" &
```

The stream prints `BetPlaced` when the engine accepts the bet. The current round can already
be running. If the bet lookup returns `wrong_phase` or submission returns `round_closed` or
`round_sealed`, wait for the next round ID and place a new bet.

When the stream prints `RoundStarted`, submit a cash-out. Cash-outs must reach admission before
the crash deadline. Keep the request body so a lost response can be retried with the same ID.

```sh
cash_id=$(python3 -c 'import uuid; print(uuid.uuid4())')
cash_body=$(jq -nc --arg id "$cash_id" '{clientCommandId:$id,cashOut:{}}')
curl -sS -H "$auth" -H 'Content-Type: application/json' \
  -d "$cash_body" "$api/rounds/$round/commands"
curl -sS -H "$auth" "$api/commands/$cash_id"
```

The lookup returns `pending` until execution records the outcome. After settlement,
run `make audit ROUND_ID="$round" AUDIT_FORMAT=orvex/audit-history/v2` to include the ledger
in verification. Stop the local services with `kill "$gateway_pid" "$engine_pid"`.

## Audit

`make audit ROUND_ID=<uuid>` builds `crashctl`, exports the round from the database named by `ORVEX_DATABASE_URL` with `crashctl audit export` into a new private temporary directory (the file has mode 0600), verifies it with `crashctl audit verify -expect-head` set to the head the export printed, and removes the directory.

- `AUDIT_FORMAT=NAME` passes `-format NAME`. `AUDIT_UNSETTLED=1` passes `-unsettled`: it audits a round that is not settled at its newest snapshot, and the temporary file then holds the round's unrevealed secret config. For a round whose secret config is not revealed, it needs the privileged path (LBT-21): a role with the `orvex_secret_reader` grants in `ORVEX_DATABASE_URL` and the keyring named by `ORVEX_SECRET_KEYRING_FILE`, set in the environment or on the command line (`make audit ROUND_ID=<uuid> AUDIT_UNSETTLED=1 ORVEX_SECRET_KEYRING_FILE=<path>`).
- A missing `ROUND_ID` or `ORVEX_DATABASE_URL`, or an `AUDIT_UNSETTLED` other than `1`, stops before the build with an `audit:` message. A failed step prints `audit: crashctl audit export failed with exit code N` (or `verify`); make reports `Error N` and exits 2. `N` is `crashctl`'s code: 1 failed, 2 usage or a keyring problem, 3 unsupported, 4 I/O or database, 5 round not found, 6 round not settled or its secret config not revealed.
- The expected head comes from the same database as the file, so the run checks the export end to end but is not an independent anchor (ADR-001). Keep the printed head elsewhere to anchor it.
- Local Compose database: `ORVEX_DATABASE_URL='postgres://orvex:orvex@127.0.0.1:55432/orvex?sslmode=disable' make audit ROUND_ID=<uuid>`.

### Parallel worktree previews

On a Linux Docker host, create a linked Git worktree for each requirement and run
these commands from that checkout. The host launcher needs Bash, Git and Docker
Compose with support for the `!reset` tag. Git operations stay on the host.

| Command | Effect |
|---|---|
| `./workspace shell` | Open a shell in the worktree's Go development container. |
| `./workspace exec make test` | Run a command in that same container. |
| `./workspace devcontainer` | Start it through the optional Dev Container CLI and report its container ID for attachment. |
| `./workspace preview` | Start private PostgreSQL, build this checkout's crashctl, apply its migrations and show schema status. |
| `./workspace stop` | Stop this project's containers and retain the database. |
| `./workspace dispose` | Remove this project's containers, network and database volume immediately. |

Run the agent's editing and shell tools inside the development container to
scope its file access to the mounted checkout. A host agent using a worktree
still has its host permissions. Each project resolves its own database at
`postgres:5432`. PostgreSQL has no published host port. The development
container runs as the host user and has no host Docker socket. Testcontainers
integration tests run separately in CI or on a Docker-enabled host.
The preview starts the database and runs migrations. To play, start the engine and gateway as described in [Play a local round](#play-a-local-round).

Run `deployments/docker/tests/worktree-isolation.sh` from the repository to check
concurrent previews. It uses a temporary clone, applies different migrations at
the same version, removes one worktree, runs collection twice, and checks
both the removed resources and the surviving deployment.

#### Cleanup after worktree removal

Run `./workspace gc` from the primary checkout after `git worktree remove`.
If a tool deletes just the directory, Git must first prune the worktree
registration. A merged branch with a registered worktree retains its preview.
Each registered deployment belongs to its original Docker engine. Collection
removes its containers, network and database volume once its registration is
gone. A failed teardown remains eligible for retry.

For automatic cleanup on a Linux development host with a systemd user manager,
run this once from the primary checkout using the same user's Docker context:

```sh
systemctl --user link "$PWD"/deployments/systemd/orvex-preview-gc@.{service,timer}
timer=$(systemd-escape --path --template=orvex-preview-gc@.timer "$PWD")
systemctl --user enable --now "$timer"
```

The timer checks every five minutes while the user manager is running. Use
`systemctl --user disable --now "$timer"` to stop it. The primary checkout
must remain at its configured path.
