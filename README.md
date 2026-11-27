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

Prerequisites: Go 1.26 (the module requires 1.26.0), golangci-lint v2.14.0, Python 3.11 or newer (standard library only; used by `make golden-check` and the demo credentials), a Docker-compatible daemon (used by `make test-integration`) and, for the shell demo, `curl`. The browser console requires Node 24.12+ and pnpm 10.34.9. CI uses the Node version in `web/.node-version`. Browser tests also require Playwright Chromium.

Buf needs a newer Go than the module. `make proto` and `make proto-check` run Buf v1.75.0, which declares Go 1.26.7, while `go.mod` keeps `go 1.26.0`. With the default `GOTOOLCHAIN=auto`, Go downloads a 1.26.7 or later toolchain for those targets, so they need network access on a host with an older 1.26.x; with `GOTOOLCHAIN=local`, install Go 1.26.7 or later. The generators in `buf.gen.yaml` are pinned independently: `protoc-gen-go` v1.36.12 matches the `google.golang.org/protobuf` runtime in `go.mod`, while `protoc-gen-go-grpc` v1.6.2 is a separate module with its own version line. Its output needs `grpc.SupportPackageIsVersion9`, which the `google.golang.org/grpc` runtime in `go.mod` provides. Change a generator or a runtime together with `make proto` and a commit of the regenerated files.

- `make build`: build all binaries into `bin/` (or `make build-gateway`, `make build-engine`, `make build-crashctl`)
- `make test`: run tests with the race detector
- `make test-acceptance`: start PostgreSQL and the real services, play a two-player round, recover a lost cash-out receipt, and verify the settled v2 history. Requires Docker.
- `make load-baseline` measures single-round player bursts through one gateway and one engine against disposable PostgreSQL. Run on a quiet host with Docker. The target prints a fresh report directory under `bin/`. Set `ORVEX_BURST_OUT` to choose the output directory. Interpret the result as burst size and drain time under the recorded configuration. Sustained commands per second requires a separate workload.
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
- `crashctl ledger grant GRANT_KEY PLAYER_ID CREDITS` adds up to 10¹² demo credits per grant. A new grant accepts only player IDs of 1 to 64 characters of `a-z`, `0-9`, `_` and `-` (exit 2 otherwise, nothing written). Retrying the same grant succeeds, including one committed earlier under a former player ID spelling. Reusing a key with a different player or amount exits with code 7.

Local PostgreSQL on the host (a worktree preview has its own database; see [Play a local round](#play-a-local-round)):

- `docker compose -f deployments/docker/compose.yaml up -d --wait`
- `export ORVEX_DATABASE_URL='postgres://orvex:orvex@127.0.0.1:55432/orvex?sslmode=disable'`, then `./bin/crashctl migrate up`
- `docker compose -f deployments/docker/compose.yaml down -v`

See [AGENTS.md](AGENTS.md) for contributor and agent guidance and [docs/adr/](docs/adr/) for the architectural decisions.

### Single-engine burst baseline

See the [first measured baseline](docs/changes/single-engine-burst-baseline.md) for the hardware, six trials per workload and the observed burst limits.

`make load-baseline` runs a discarded warm-up, then three repeats at 10, 40 and 80 funded players. Each trial starts fresh services and a fresh database. All players place one bet over a 1.8-second window. Normal cash-outs span 800 ms. The deadline trial schedules cash-outs from 50 ms before to 50 ms after a controlled crash. The retry trial repeats each cash-out 20 ms later with the same identity and payload. The generator schedules each request independently of previous responses. There are no spectators. The engine pool has four connections.

Set `ORVEX_BURST_LEVELS=20,60` or `ORVEX_BURST_REPEATS=2` to vary the sweep. Counts range from 1 to the fixture's 100-bet limit. Each run needs an empty output directory. Keep the per-trial JSON alongside `burst-report.md` to inspect receipts, deadline outcomes and database samples.

Read the bracket per workload. Each repeat must meet the 10 ms generator-lag p99, 100 ms acknowledgment p99 and one-second drain budgets. These are experiment thresholds. The Go test fails on correctness violations, including missing admissions, mismatched receipts or failed settled-history verification. A latency miss is a measured result and appears in the report. Execution latency ends at the first database sample that observes the committed sequence. At 50 ms sampling intervals this gives an upper bound. Database lock waits are point samples, and WAL covers the PostgreSQL cluster.

To run the generator on Windows x64, build it on the controller with `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o bin/orvex-loadgen.exe ./tests/loadgen`. The controller needs a launcher executable that starts one generator process per trial on the other machine and forwards its stdin and stdout unchanged. Use an authenticated private connection for this stream because it carries temporary player credentials. Send launcher diagnostics to stderr.

Set `ORVEX_BURST_REMOTE_LAUNCHER` to the launcher's path and `ORVEX_BURST_GATEWAY_HOST` to the controller's private address reachable from the generator. Leave the database and engine on the controller. Start with `ORVEX_BURST_ARMS=retry ORVEX_BURST_LEVELS=1 ORVEX_BURST_REPEATS=1 make load-baseline`. Once the smoke round verifies, repeat with 80 players. Keep both machines awake throughout the run.

## Play a local round

Run the round in one of two places. The steps are the same. Only the database URL and the credential directory differ.

| | Host | Preview container |
|---|---|---|
| Start | `docker compose -f deployments/docker/compose.yaml up -d --wait` | `./workspace preview`, then `./workspace shell` (see [Parallel worktree previews](#parallel-worktree-previews)) |
| `ORVEX_DATABASE_URL` | You export `postgres://orvex:orvex@127.0.0.1:55432/orvex?sslmode=disable`. The Compose file publishes the port on the host loopback only. | Already set to `postgres://orvex:orvex@postgres:5432/orvex?sslmode=disable`. Leave it alone: inside the container `127.0.0.1` is the container itself, and the preview publishes no host port. |
| Credential directory (`demo_dir`) | `${XDG_STATE_HOME:-$HOME/.local/state}/orvex-demo` | `/var/lib/orvex-preview`, a Docker volume of this preview |
| Migrations and credentials | You run them below. | `./workspace preview` has run them and built only `crashctl`; run `make build` in the container for the engine and gateway. |

On the host, prepare the database and the credentials:

```sh
export ORVEX_DATABASE_URL='postgres://orvex:orvex@127.0.0.1:55432/orvex?sslmode=disable'
make build
./bin/crashctl migrate up
demo_dir="${XDG_STATE_HOME:-$HOME/.local/state}/orvex-demo"
unrevealed=$(docker compose -f deployments/docker/compose.yaml exec -T postgres \
  psql -XqAt -v ON_ERROR_STOP=1 -U orvex -d orvex < deployments/docker/unrevealed-rounds.sql) &&
  python3 deployments/docker/demo-credentials.py ensure "$demo_dir" --unrevealed "$unrevealed"
```

Inside the preview shell, build the binaries and select the credentials prepared by `./workspace preview`.

```sh
make build
demo_dir=/var/lib/orvex-preview
```

The credential helper creates a keyring, a gateway token and a players file with mode 0600 in a 0700 directory. It keeps existing files on repeated runs. Players `alice` and `bob` each get a random bearer token.

The keyring holds the key that seals each round's secret config until the round settles. The engine needs the original key to resume an unsettled round, so keep the directory with the database (a preview keeps both in Docker volumes that share a lifetime). If the key is lost, restore it from a backup kept apart from database backups, or discard the database and start fresh.

Fund both players. A grant key makes the grant idempotent: retry the same key with the same player and amount and the command reports the existing grant. The same key with another player or amount exits with code 7.

```sh
./bin/crashctl ledger grant demo-alice-1 alice 1000
./bin/crashctl ledger grant demo-bob-1 bob 1000
```

A new grant accepts the player IDs that authentication accepts: 1 to 64 characters of `a-z`, `0-9`, `_` and `-`. Any other spelling exits with code 2 and writes nothing. A grant committed earlier under a former spelling keeps working when you retry it with its original key and arguments. Never rewrite an existing player ID to fit the rule: a different spelling is a different account.

Start the engine and the gateway:

```sh
export ORVEX_SECRET_KEYRING_FILE="$demo_dir/keyring"
export ORVEX_GATEWAY_TOKEN_FILE="$demo_dir/gateway-token"
export ORVEX_PLAYERS_FILE="$demo_dir/players.json"
export ORVEX_ENGINE_ADDR=127.0.0.1:7070 ORVEX_GATEWAY_ADDR=127.0.0.1:8080
export ORVEX_BETTING_WINDOW=30s
./bin/engine &
engine_pid=$!
env -u ORVEX_DATABASE_URL -u ORVEX_SECRET_KEYRING_FILE ./bin/gateway &
gateway_pid=$!
```

In a preview, services started in a `./workspace shell` stop when that shell exits. Keep the shell open, or start them again as described under [Recreate and resume](#recreate-and-resume).

### Play in two browsers

With the funded players and services above running on the host, start the local console
from the repository root (Node 24.12+ and pnpm 10.34.9):

```sh
cd web
corepack enable pnpm
pnpm install --frozen-lockfile
pnpm dev
```

Open the loopback URL printed by Vite in two separate browser sessions. Enter Alice’s player token in one and Bob’s in the other, using the tokens from the operator’s private players file. Vite proxies `/v1` to the gateway at `127.0.0.1:8080`; set `ORVEX_GATEWAY_ADDR` before starting Vite if your gateway listens elsewhere. To serve the console under a subpath, set `ORVEX_WEB_BASE=/crash/` for both build and serve, and route that prefix’s `v1/` requests to the gateway.

Place both bets while betting is open. Cash out in Alice’s session after the round starts. Refresh the page to recover the latest command and its payout. The browser keeps the player token, round ID and latest command in that tab’s session storage. **Leave session** clears them. At settlement both players can see the outcome in round activity and choose **Join current round**. The current receipt remains available until a different round is found. A disconnected tab pauses its multiplier and resumes the feed from the last record. If status lookup pauses after admission, use **Retry command recovery**, supplying a refreshed token for the same player if needed.

To run the two-browser lost-response test, install the frontend dependencies and Chromium (`pnpm exec playwright install --with-deps chromium` from `web/`), then run `make test-browser` from the root. The test starts its own gateway, engine, PostgreSQL and Vite server. `pnpm build` typechecks and builds the console.

### Verify a player's settled receipt

Keep the player's tab on the finished round. The console retains the latest command in that tab. A command rejected during execution has a chain record and can be verified. An admission refusal, such as insufficient credits, has no chain record.

1. As the operator, run `./bin/crashctl audit export -format orvex/audit-history/v2 ROUND_ID round.jsonl` with the auditor database connection. Restrict this file to auditors authorized to see the entire round. It links player names to account IDs and includes all commands, rejection reasons, and ledger entries. Use synthetic accounts for the shared demo.
2. Supply the expected 64-character chain head through a separately trusted record or channel. For the local demo an auditor can read the latest committed hash directly from the settled database with the query below. A head copied from the JSONL header only checks internal consistency.
3. Under **Verify settled receipt**, enter the player ID and expected head, select the export, and click **Verify receipt**. The browser runs the Go audit verifier in a worker and matches the local claim to the replayed command and outcome. The current receipt includes the original sequence and effective time. Older saved receipts are located by the player-scoped command ID.

```sql
SELECT encode(hash, 'hex') FROM orvex.chain_records
WHERE round_id = :'round_id' ORDER BY sequence DESC LIMIT 1;
```

The verifier checks chain, fairness, decisions, state, and ledger against the supplied anchor. The operator controls that anchor in the local demo. The operator also supplies the verifier code. For independent verification, build `crashctl` from a separately trusted checkout and run `crashctl audit verify -expect-head HEAD round.jsonl`. This checks the entire export. Match the local receipt to its player command separately. For a distributed browser build, publish a SHA-256 manifest of the complete `web/dist` directory (generate it with `cd web/dist && find . -type f -print0 | sort -z | xargs -0 sha256sum`) through a trusted release channel and verify the files before serving them. Apply the preview response headers from `web/vite.config.mjs` on the deployment host, including worker responses. The file stays on the device. Large rounds can spend substantial time replaying historical rules. Use **Cancel verification** to stop the worker. The activity panel displays up to 100 entries per page. Browser verification accepts up to 32 MiB and 4,096 JSONL lines, including ledger entries. Use the native verifier for larger exports. The worker requires explicit success from the running Go verifier before matching a receipt. The build script selects Go 1.26.11 for the verifier and its JavaScript runtime. The first build may download that toolchain. The script honors the `GO` executable override.

### Play from the shell

After both services report ready, the following Bash commands place Alice's bet and follow the round. They use `curl` and Python. Bob can use the same commands with his own token.

```sh
token=$(python3 -c 'import json, os, sys
players = json.load(open(os.environ["ORVEX_PLAYERS_FILE"]))
print(next(token for token, player in players.items() if player == sys.argv[1]))' alice)
api="http://$ORVEX_GATEWAY_ADDR/v1"
auth="Authorization: Bearer $token"
round=$(curl -fsS --retry 8 --retry-delay 1 --retry-all-errors "$api/rounds/current" |
  python3 -c 'import json, sys; print(json.load(sys.stdin)["roundId"])')
bet_id=$(python3 -c 'import uuid; print(uuid.uuid4())')
curl -sS -H "$auth" -H 'Content-Type: application/json' \
  -d "{\"clientCommandId\":\"$bet_id\",\"placeBet\":{\"betCredits\":\"100\"}}" "$api/rounds/$round/commands"
curl -sS -H "$auth" "$api/commands/$bet_id"
curl -NsS "$api/rounds/$round/stream" &
```

A submission that returns `202` with `"status":"admitted"` was durably admitted at the sequence and time it reports. A duplicate with the same command ID returns the same receipt with `"duplicate":true`. The `202` does not say the command ran. Ask the lookup, `GET /v1/commands/{clientCommandId}`, for the outcome: it returns `pending` until execution records the result, then `accepted` or `rejected`. A bet that the round turns down shows `rejected` with a reason such as `wrong_phase`.

The stream prints `BetPlaced` when the engine accepts the bet. The current round can already be running. If the bet lookup returns `wrong_phase` or submission returns `round_closed` or `round_sealed`, wait for the next round ID and place a new bet with a new command ID. After a lost response, retry with the same command ID and body.

When the stream prints `RoundStarted`, submit a cash-out. Cash-outs must reach admission before the crash deadline. Keep the request body so a lost response can be retried with the same ID.

```sh
cash_id=$(python3 -c 'import uuid; print(uuid.uuid4())')
cash_body=$(python3 -c 'import json, sys
print(json.dumps({"clientCommandId": sys.argv[1], "cashOut": {}}, separators=(",", ":")))' "$cash_id")
curl -sS -H "$auth" -H 'Content-Type: application/json' \
  -d "$cash_body" "$api/rounds/$round/commands"
curl -sS -H "$auth" "$api/commands/$cash_id"
```

After settlement, run `make audit ROUND_ID="$round" AUDIT_FORMAT=orvex/audit-history/v2` to include the ledger in verification. It reads the database in `ORVEX_DATABASE_URL`, which is the right one in either place. Stop the local services with `kill "$gateway_pid" "$engine_pid"`.

### Recreate and resume

A round that is not settled survives a restart of the services. Stop the engine and the gateway, or recreate the preview container with `./workspace recreate`, then repeat the steps from the credential directory onward in a new shell. `ensure` keeps the existing files, the engine reads the same keyring, adopts the unsettled round and settles it, and the stream and the audit work as before.

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
| `./workspace devcontainer` | Start it through the optional Dev Container CLI (which must be installed) and report its container ID for attachment. |
| `./workspace preview` | Start private PostgreSQL, build this checkout's crashctl, apply its migrations, create the preview's demo credentials and show schema status. Run it again to upgrade an older preview. |
| `./workspace recreate` | Replace only the development container. The database and the credentials stay. |
| `./workspace restore-keyring FILE` | Restore a keyring into an empty slot after checking its format and opening every unsettled sealed round with it. |
| `./workspace stop` | Stop this project's containers and retain the database and the credentials. |
| `./workspace dispose` | Remove this project's containers, network, database volume and credentials volume immediately. |

Run the agent's editing and shell tools inside the development container to
scope its file access to the mounted checkout. A host agent using a worktree
still has its host permissions. Each project resolves its own database at
`postgres:5432`, and the container's `ORVEX_DATABASE_URL` already names it. The
host's `127.0.0.1:55432` URL does not apply here. PostgreSQL has no published
host port. The development container runs as the host user and has no host
Docker socket. Testcontainers integration tests run separately in CI or on a
Docker-enabled host. The preview starts the database and runs migrations. To
play, follow [Play a local round](#play-a-local-round) in `./workspace shell`.

#### Preview credentials

The keyring, the gateway token and the players file live in a Docker volume of
the preview, mounted at `/var/lib/orvex-preview` and outside the checkout. A
one-shot container gives the volume to the host user's UID and GID with mode
0700 on every start, and the files inside are mode 0600. The volume has the
preview's lifetime, like its database. `stop`, `recreate` and removing the
development container keep both. `dispose` and collection remove both. Two
previews never share credentials.

The keyring seals every unsettled round's secret config, so `./workspace
preview` never replaces a missing one while the database holds a sealed round
that has not settled. It exits with code 3 and writes nothing. An earlier
preview kept its credentials in the development container's `/tmp`. Running
`./workspace preview` or `./workspace recreate` on one copies them to the volume
before it replaces the container. If that keyring is elsewhere, the command
stops and leaves the container in place so you can copy the file out.

Back up the keyring apart from database backups, and restore it after a loss:

```sh
backup=$(mktemp -d "${XDG_STATE_HOME:-$HOME}/orvex-backup.XXXXXXXX")
(umask 077; ./workspace exec cat /var/lib/orvex-preview/keyring > "$backup/keyring")
```

To restore that backup, run this from the same preview's worktree.

```sh
./workspace restore-keyring "$backup/keyring"
```

Without the original key, a round that has not settled can never be loaded or
exported, and settled rounds are unaffected. Either restore the key, or run
`./workspace dispose` to discard the preview and its database and then
`./workspace preview` for a fresh one.

Run `deployments/docker/tests/worktree-isolation.sh` from the repository to check
concurrent previews. It uses a temporary clone, applies different migrations at
the same version, checks that each credentials volume has the host user's owner
and mode and keeps its own key, removes one worktree, runs collection twice,
and checks both the removed resources and the surviving deployment.

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
