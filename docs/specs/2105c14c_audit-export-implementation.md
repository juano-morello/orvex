# LBT-19 implementation plan: `crashctl audit export ROUND_ID`

- Run: `2105c14c`. Linear: [LBT-19](https://linear.app/lbtworks/issue/LBT-19/crashctl-audit-export-round-id).
- Normative design: the approved spec written by run `73dbe98d`. Step 0 adds it to the repository as
  `docs/specs/73dbe98d_audit-export-cli.md`. Later references to "spec §n" or "X-n" mean that file.
  Decisions X-1 to X-13 are approved as written, and every point of spec §2 is accepted: the settlement
  gate and `-unsettled`, exit codes 5 and 6, `Export.RoundRecords` and `verification.Report.State`.
- This plan is spec §10 applied to the current `main`. Where `main` differs from what the spec assumed,
  §2 below says what to do. Section numbers in this plan are this plan's own unless they say "spec".
- Size: **L**. The change spans the CLI, two library packages, secret handling, database grants and a
  large real-PostgreSQL test suite.

## 1. Preconditions (checked while planning)

- The worktree is on `main` at `e74bc19`, which includes LBT-11 and LBT-12. Toolchain go1.26.5,
  golangci-lint 2.14.0. The Docker daemon is running and port 55432 is free.
- LBT-12 is merged, so the spec's §0 precondition holds:
  - `internal/runtime/execution` has `New(db, reg, cfg, policies...)`, `DefaultConfig()`,
    `Config.EngineBuild`, `Executor.CreateRound(ctx, NewRound{RoundID, Game, RulesVersion, Config,
    CreatedAtUnixMicro}) (Created, error)`, `ExecuteNext(ctx, Lease) (Outcome, error)`,
    `Drain(ctx, Lease, limit) (int, error)`, `Lease{RoundID, FencingToken}`, a `Policy` interface with
    `Descriptor()` and `Final(state any) (bool, error)`, and `CrashV1Policy()`.
  - Migrations `0008_execution_ledger` and `0009_close_bound` exist.
  - The snapshot cadence follows E-13: a snapshot at sequence 0 and after every record that emits
    `RoundStarted`, `RoundCrashed` or `RoundSettled`. `TestExecuteReproducesGoldenHistories` in
    `internal/runtime/execution/execute_integration_test.go` asserts these snapshot sequences: {0, 9, 19, 21}
    for worked-example, {0, 2, 4, 5} for instant-crash, {0, 1, 2, 3} for empty-round and {0, 2} for
    in-progress. These match spec §8.
- I recomputed every link hash in spec §8 with Python's `hashlib`. They match:
  - worked-example: 0 `cfdd0537…`, 9 `507826d4…`, 19 `f0829a16…`, 21 `3959ed4c…`, 22 `7cf8cc4b…`;
  - instant-crash: 5 `feea2c86…`;
  - empty-round: 3 `700d572f…`;
  - in-progress: 2 `d0df1ee2…`, 3 `f041d95d…`.
- The phase records of the goldens match spec §1: worked-example 9, 19 and 21; instant-crash 2, 4 and 5;
  empty-round 1, 2 and 3; in-progress 2 only. The last records are 22, 5, 3 and 3.
- The LBT-12 spec on `main` (`docs/specs/e0df34d3_execution-commit-ledger-outbox.md`) is APPROVED
  (2026-10-15). Spec §2 item 14 (an LBT-12 spec still marked proposed) is therefore resolved.
- LBT-18 is not on `main`. Keep every change in `internal/audit/verification`,
  `internal/persistence/eventstore` and `crashctl audit` additive (§2.6).

## 2. Differences from the spec's assumptions: follow `main` and flag each in the report

None of these contradicts an ADR. Copy this list and spec §2 into the agent report.

1. **X-11 grant on `round_secrets`: column-level, not table-level.**
   - The spec predates migration `0009_close_bound`. That migration added
     `round_secrets.close_offset_micros`, the secret crash deadline offset D.
   - `docs/architecture/persistence.md` §6 and §8 on `main` require that only `orvex.admit_command`
     reads that column ("no … export carries them"). §13 grants the engine role column-level
     `SELECT (round_id, config_secret)` on `round_secrets` for the same reason.
   - A table-level `SELECT` on `round_secrets` would contradict that document. Use
     `GRANT SELECT (round_id, config_secret) ON orvex.round_secrets` everywhere X-11's SQL appears: in the
     docs and in the test roles.
   - This is enough for `ExportHistory`. Its query `… FROM orvex.rounds r LEFT JOIN orvex.round_secrets s
     USING (round_id)` reads only `round_id` and `config_secret`.
   - It is strictly less privilege than X-11 and matches X-11's own "least privilege" rationale. D is
     derivable from the seed, so the auditor's effective exposure is unchanged.
   - `TestExportMinimalGrants` also asserts that `close_offset_micros` is denied (`42501`).
   - The approved spec copy in `docs/specs/` is **not** edited. The report states this deviation and why.
2. **The golden-seeding helpers cannot be imported.**
   - LBT-12's `seedGolden`, `seedAdmission`, `fundPlayer` and `holdFor` live in package `execution_test`,
     in `internal/runtime/execution/helpers_integration_test.go`.
   - Copy the parts you need into `cmd/crashctl/export_helpers_integration_test.go`, adapted.
   - Take `fundPlayer` from the **execution** helpers, the version proven with executed goldens. Spec
     X-13 named the admission package's copy.
   - Spec §11 open item 7 (a shared test-support package) stays open.
3. **`docs/architecture/persistence.md`** now ends with §12 Admission (LBT-11) and §13 Execution (LBT-12).
   The new export section is **§14**.
4. **`round_secrets.close_offset_micros` and `round_admission_heads.close_not_before_unix_micro`** (0009)
   change nothing in the export algorithm. `ExportHistory` never reads them, and the file never carries
   them.
5. **`pgtest.LoadHistory` rounds have no `close_offset_micros`.** The column is nullable and the export
   never reads it, so failure-path tests that use `LoadHistory` are unaffected.
6. **LBT-18 coordination.**
   - `Report.State` is one new field plus one assignment in `replayHistory`'s `visit`.
     `Export.RoundRecords` is one new field plus one query.
   - `-format` accepts only `orvex/audit-history/v1`; `orvex/audit-history/v2` is exit 3 (X-10).
   - Whichever of LBT-18 and LBT-19 merges second adapts to the other (spec §2 item 15).

## 3. Scope

Spec §10 "Files" and its "Unchanged" list fix the scope.

| File | Action |
|---|---|
| `docs/specs/73dbe98d_audit-export-cli.md` | new: the approved spec, status and checkboxes only (Step 0) |
| `internal/audit/verification/verification.go` | edit: `Report.State` |
| `internal/audit/verification/verification_test.go` | edit: add `TestReportState` |
| `internal/persistence/eventstore/export.go` | edit: `Export.RoundRecords`, the count query, doc comments |
| `internal/persistence/eventstore/export_integration_test.go` | edit: assert `RoundRecords` |
| `cmd/crashctl/database.go` | new: shared `ORVEX_DATABASE_URL` config, masking, `oneLine` |
| `cmd/crashctl/migrate.go` | edit: use `database.go`; behavior byte-for-byte unchanged |
| `cmd/crashctl/export.go` | new: `runExport` and its helpers |
| `cmd/crashctl/main.go` | edit: package comment, usage, exit constants 5 and 6, `export` dispatch, `newRegistry`, `settlementPolicies` |
| `cmd/crashctl/main_test.go` | edit: one `TestRun` row |
| `cmd/crashctl/export_test.go` | new: unit tests |
| `cmd/crashctl/export_helpers_integration_test.go` | new: integration helpers |
| `cmd/crashctl/export_integration_test.go` | new: integration tests |
| `docs/architecture/persistence.md` | edit: §2 bullet, §8 row and text, new §14 |
| `README.md` | edit: Status, Layout `cmd/crashctl` line, Development bullet |

Do not change any of these: `go.mod`, `go.sum`, `Makefile`, `.github/`, `.golangci.yml`, `AGENTS.md`,
`migrations/`, `docs/adr/`, every golden fixture or `testdata/`, `internal/game/**`,
`internal/canonjson/**`, `internal/runtime/**`, `internal/persistence/postgres/**` (including `pgtest`),
`docs/changes/` (the documenter writes that) and every other existing spec under `docs/specs/`.

Out of scope, as in spec §11:
- history format v2 and the ledger stage (LBT-18);
- multi-round export;
- any server, API or scheduled exporter;
- creating roles or grants in a deployment (LBT-14);
- external anchoring of heads;
- encryption of secrets at rest;
- a public, secret-less export format.

## 4. Conventions (AGENTS.md, ADR-007, `.golangci.yml`, spec §10 rules)

- goimports grouping with local prefix `github.com/juano-morello/orvex`. gofmt.
- Error strings are lowercase, without trailing punctuation, and prefixed `crashctl: `, `eventstore: `
  or `verification: `.
- `cmd/crashctl` tests are `package main`. Integration files start with `//go:build integration`. The
  package keeps its single `TestMain`, in `migrate_integration_test.go`. Do not add another.
- No `t.Parallel()` in any test that calls `t.Setenv`. That covers every `cmd/crashctl` export test.
- Avoid identifiers that `package main` test files already declare:
  - `goldenDir`, `failingWriter`, `failingReader`, `manifest`, `loadManifest`, `runAuditCmd`;
  - `unreachableDSN`, `runMigrateCmd`;
  - `statusApplied`, `lines`, `multilineFailingWriter`, `runMigrateFailingStdout`.
- Do not declare a package-level `ctx` in `package main`: `migrate.go` declares a local `ctx`. Use
  `context.Background()`, or `t.Context()` outside cleanups.
- Unit test helpers in `export_test.go` are visible to the integration files, so names must be unique
  across both.
- Never edit a golden fixture. Never weaken an assertion to make a run pass. If a test contradicts the
  spec, find which side is wrong and report it.
- Before changing anything else, re-read the ADRs this touches: ADR-001, ADR-003, ADR-004, ADR-005,
  ADR-006 and ADR-007.

## 5. Steps

### Step 0: add the approved spec to the repository

1. Copy the approved spec byte for byte. The source is
   `/home/juano/Workspaces/LBTWorks/Orvex/requests/lbt-19-approved-spec.md`. An identical copy is staged
   at `/home/juano/Workspaces/LBTWorks/Orvex/odin/data/sessions/2105c14c/context_handoff/lbt-19-approved-spec.md`.
   Both have sha256 `c0ccadfcb9c835c085e80ce9c133e5ff87a89ffc016f32cb6ce0cd71fd6cccc3` and 958 lines.

   `cp <source> docs/specs/73dbe98d_audit-export-cli.md`

2. Replace lines 3–4. The old text is:

   ```
   Status: **PROPOSED, awaiting engineer approval.** Approve, edit or reject each decision in the section 3
   table. Implementation (section 10) starts only after approval, in a later run.
   ```

   The new text is:

   ```
   Status: **APPROVED (2026-10-15).** The engineer approved X-1 to X-13 as written, with no edits, and
   accepted every point of section 2: the settlement gate and `-unsettled`, exit codes 5 and 6,
   `Export.RoundRecords` and `verification.Report.State`. Section 0 described the planning run and no
   longer applies; implementation follows section 10.
   ```

3. In the section 3 table (lines 175–187, rows X-1 to X-13), change each row's final `| [ ] |` to
   `| [x] |`.
4. Change nothing else. Check with `diff <source> docs/specs/73dbe98d_audit-export-cli.md`. The only
   hunks must be the status paragraph and the 13 checkbox rows. `diff` exits 1 because the files differ;
   judge the result by the hunks, not the exit status.

### Step 1: `verification.Report.State` (X-7)

`internal/audit/verification/verification.go`:

- Add a field to `Report`, after `Checkpoints`:

  ```go
  // State is the replayed state after the last record in ModeReplay and
  // ModeVerify; nil in ModeChain. Callers must not mutate it.
  State any
  ```

- In `replayHistory`'s `visit` closure, make `rep.State = s` the first statement, before the checkpoint
  early return. `replay.Run` calls `visit` after genesis and after every record, so `rep.State` ends as
  the state after the last record.
- Nothing else changes: no stage, check, message or exit code.

`internal/audit/verification/verification_test.go`, new `TestReportState`:

- worked-example, `ModeReplay` and `ModeVerify`: `rep.State` is a `v1.State` with
  `Phase == v1.PhaseSettled`;
- in-progress, `ModeReplay`: `Phase == v1.PhaseRunning`;
- worked-example, `ModeChain`: `rep.State == nil`.

Every existing verification test must pass unchanged.

### Step 2: `eventstore.Export.RoundRecords` (X-5)

`internal/persistence/eventstore/export.go`:

- `Export` gains `RoundRecords int` with this doc comment: "RoundRecords is N + 1, the round's chain
  records in the same snapshot; Records ≤ RoundRecords."
- Add a query in the same transaction, right after the head query and before anything is written:

  ```sql
  SELECT max(sequence) FROM orvex.chain_records WHERE round_id = $1
  ```

  - Scan it into a `*int64`.
  - A database error is `fmt.Errorf("eventstore: read record count: %w", err)`.
  - NULL is `fmt.Errorf("%w: round has no chain records", ErrCorrupt)`.
  - A value below `last` is `fmt.Errorf("%w: newest record %d before snapshot %d", ErrCorrupt, max, last)`.
    Both of these branches are defensive. The schema's foreign keys make them unreachable once the head
    query has found record S, so they need no test.
  - Set `RoundRecords = int(max) + 1` in the returned `Export`.
- Update the `ExportHistory` doc comment to say that it also reports the round's record count from the
  same snapshot. The written bytes do not change.

`internal/persistence/eventstore/export_integration_test.go`:

- `TestExportGoldenHistories`: also require `exp.RoundRecords == h.Records`.
- `TestExportStopsAtLatestSnapshot`: also require `exp.RoundRecords == 23`; `exp.Records == 10` is
  already asserted.
- The other tests are unchanged.

### Step 3: `cmd/crashctl/database.go` (X-4, shared with `migrate`)

New file. Move these from `migrate.go`:

- `databaseURLEnv` and `defaultConnectTimeout`;
- `oneLine`;
- the configuration and masking code, as:

```go
var (
	errDatabaseURLUnset   = errors.New(databaseURLEnv + " is not set")
	errDatabaseURLInvalid = errors.New("invalid " + databaseURLEnv)
)

// databaseConfig reads ORVEX_DATABASE_URL (unset or empty: errDatabaseURLUnset;
// unparsable: errDatabaseURLInvalid, never the pgx error, which can echo the
// DSN) and applies defaultConnectTimeout unless the DSN sets connect_timeout.
func databaseConfig() (*pgx.ConnConfig, error)

// mask returns err as one line with cfg's password, if any, replaced by xxxxx.
func mask(cfg *pgx.ConnConfig, err error) string
```

`migrate.go` calls them. Keep the order of its checks and every message and exit code exactly:

- On a config error, print `prefix + err.Error()` and return `exitUsage`. This gives the existing
  `crashctl: migrate up: invalid ORVEX_DATABASE_URL` and `… ORVEX_DATABASE_URL is not set` lines.
- `fail` becomes `fmt.Fprintln(stderr, prefix+mask(cfg, err))`.
- Every `TestMigrate*` test, unit and integration, passes unchanged. Do not edit them.

### Step 4: `cmd/crashctl/export.go` and `main.go` (X-1 to X-10, X-12)

Implement spec §5 (the algorithm, steps 1–12) and spec §6 (report, stderr messages and usage) exactly.
Suggested shape; names follow spec §10 step 4:

```go
type settledVerdict int // settledYes, settledNo, settledUnknown; String(): "yes", "no", "unknown"

type exporter func(ctx context.Context, db eventstore.TxBeginner, roundID [16]byte, w io.Writer) (eventstore.Export, error)

// exporters maps -format values to writers (X-10); LBT-18 adds v2 here.
var exporters = map[string]exporter{verification.Format: eventstore.ExportHistory}

var historyFormatName = regexp.MustCompile(`^orvex/audit-history/v[1-9][0-9]*$`)

var (
	errOutputExists      = errors.New("crashctl: output file exists")
	errFormatUnsupported = errors.New("crashctl: history format unsupported")
	errNotSettled        = errors.New("crashctl: round not settled")
	errNoPolicy          = errors.New("crashctl: no settlement policy")
	errSettlementCheck   = errors.New("crashctl: settlement check failed")
)

func runExport(args []string, stdout, stderr io.Writer) int
func exportConnConfig(cfg *pgx.ConnConfig)
func settlement(history []byte, reg *game.Registry, policies []execution.Policy) (settledVerdict, error)
func exportExitCode(err error) int
func exportReport(format string, exp eventstore.Export, settled settledVerdict) []byte
```

Notes for each part:

- **Prefix.** Every failure prints exactly one line: `crashctl: audit export: ` + the spec §6 message,
  passed through `mask(cfg, …)` (or `oneLine` before a config exists). It returns the code that
  `exportExitCode` gives for the classified error. The sentinels drive the exit code; the printed text is
  the spec §6 message, not the sentinel's text.
- **Flags.** `flag.NewFlagSet("crashctl audit export", ContinueOnError)` with output to stderr. Its usage
  function prints `usage: crashctl audit export [-unsettled] [-format NAME] ROUND_ID FILE`, then
  `PrintDefaults`.
  - `flag.ErrHelp` exits 0.
  - Other parse errors exit 2.
  - The wrong number of positionals prints `want ROUND_ID and FILE, got <n> arguments`, then the usage,
    and exits 2.
- **`ROUND_ID`.** Parse with `canonjson.ParseUUID(canonjson.String(arg))`. The nil UUID is rejected.
  Either failure prints `ROUND_ID must be a canonical lowercase UUID other than the nil UUID` and exits 2.
- **`-format`.** A key of `exporters` is accepted. A value that matches `historyFormatName` but is not a
  key exits 3 with `format <NAME> unsupported: this build writes orvex/audit-history/v1`. Anything else
  exits 2 with `-format must be a history format such as orvex/audit-history/v1`. Both checks run before
  `databaseConfig()`.
- **Database config.** `databaseConfig()` errors exit 2 with the message as is. Then call
  `exportConnConfig(cfg)`:
  - it sets `RuntimeParams["default_transaction_read_only"] = "on"`, always, overriding the DSN;
  - it sets `application_name` to `crashctl` and `idle_in_transaction_session_timeout` to `60000`, each
    only if the key is absent from `cfg.RuntimeParams`;
  - if `RuntimeParams` is nil, allocate it.

  Only `export` uses `exportConnConfig`; `migrate` is untouched.
- **`FILE` pre-checks** (only when `FILE != "-"`), in this order:
  - `os.Lstat` succeeds (a file, directory or symlink, dangling or not): exit 2 with
    `<FILE> exists; refusing to overwrite`.
  - `os.Lstat` fails with something other than not-exist: exit 4 with the error.
  - `os.Stat(filepath.Dir(FILE))` fails or is not a directory: exit 4 with
    `<FILE>: directory <dir> is not usable: <error or "not a directory">`.
- **Connect and export.**
  - Connect with `pgx.ConnectConfig(context.Background(), cfg)`; an error exits 4, masked.
  - Run `exporters[format](ctx, conn, roundID, &buf)` into a `bytes.Buffer`, then close the connection
    before any check or output.
  - Map the export error:
    - `ErrRoundNotFound` exits 5 with `round <uuid> not found`;
    - `ErrNoCheckpoint` exits 1 with `round <uuid> has no snapshot to end an export at`;
    - `ErrCorrupt` exits 1 with the error text;
    - anything else exits 4 with the masked error text.
- **Settlement (X-7).**
  - `settlement` runs `verification.Verify(bytes.NewReader(history), Options{Mode: ModeReplay,
    Registry: reg})`.
  - It looks up the policy whose `Descriptor()` equals `rep.Descriptor`. If there is none, it returns
    `settledUnknown` with `fmt.Errorf("%w: %w", errSettlementCheck, fmt.Errorf("%w for %s rules %d",
    errNoPolicy, d.Game, d.RulesVersion))`. The printed message is the spec §6 `no settlement policy for
    <game> rules <n>`; build the text accordingly.
  - It calls `Final(rep.State)`. The result is `settledYes` or `settledNo` with a nil error. If `Final`
    returns an error, or `Verify` returns any error (a `*verification.Failure` or not), the result is
    `settledUnknown` with that error wrapped as `fmt.Errorf("%w: %w", errSettlementCheck, err)`.
  - Without `-unsettled`:
    - `settledNo` exits 6 with `round <uuid> is not settled at its newest snapshot, sequence <S> of <N>:
      the file would disclose its unrevealed secret config; -unsettled exports it anyway`, where
      `S = exp.Records-1` and `N = exp.RoundRecords-1`;
    - `settledUnknown` exits 3 or 1 with `cannot establish that round <uuid> is settled: <reason>;
      -unsettled exports it anyway`. `<reason>` is the `*verification.Failure` text, `no settlement
      policy for <game> rules <n>`, or the `Final` error. Nothing is written.
  - With `-unsettled`, the file is written whatever the verdict, and the verdict goes into the report.
- **Write.**
  - `FILE` path: `os.OpenFile(FILE, O_WRONLY|O_CREATE|O_EXCL, 0o600)`. `errors.Is(err, fs.ErrExist)`
    exits 2 with `<FILE> exists; refusing to overwrite`; any other error exits 4 with the error text.
    Then `Write` the buffer, `Sync` and `Close`. Any failure after the open closes and `os.Remove`s the
    file and exits 4 with `write <FILE>: <error>`.
  - `-`: write the buffer to stdout. An error exits 4 with `write stdout: <error>`.
- **Report.** The report goes to stdout, or to stderr when `FILE` is `-`. A write error removes `FILE`
  (path case) and exits 4 with `write report: <error>`.
- **`exportExitCode`** checks in this order:
  1. nil → 0;
  2. `errOutputExists` → 2;
  3. `errFormatUnsupported` → 3;
  4. `errNotSettled` → 6;
  5. `eventstore.ErrRoundNotFound` → 5;
  6. `ErrNoCheckpoint` or `ErrCorrupt` → 1;
  7. `*verification.Failure` → 3 if `Unsupported`, else 1;
  8. `errNoPolicy` → 3;
  9. `errSettlementCheck` → 1;
  10. anything else → 4.
- **`exportReport`.** Exactly spec §6, every line `\n`-terminated, in this order:
  1. `exporter ` + `buildinfo.String("crashctl")`;
  2. `format <format>`;
  3. `round <canonjson.UUID>`;
  4. `records <Records> of <RoundRecords> head <Head>`;
  5. only when `Records < RoundRecords`: `note: the file ends at the newest snapshot, sequence <S>;
     sequences <S+1> to <N> are not exported`;
  6. `settled yes|no|unknown`;
  7. only when not yes: `warning: the file holds the round's secret config before settlement; keep it
     private`;
  8. `ok`.
- **Never print** `configSecret`, the seed, state, record bytes or the DSN.

`main.go`:

- The package comment mentions `crashctl audit export` (LBT-19, `docs/specs/73dbe98d_audit-export-cli.md`).
- The usage is exactly spec §6 "Usage". Its new lines are:
  - `crashctl audit export [-unsettled] [-format NAME] ROUND_ID FILE`;
  - `FILE "-" reads standard input (export: writes standard output).`;
  - `ORVEX_DATABASE_URL names the database for migrate and export (a PostgreSQL URL or key=value
    connection string).`
- Add `exitNotFound = 5` and `exitNotSettled = 6` to the exit constants. Update `run`'s doc comment.
- In `runAudit`, add `if sub == "export" { return runExport(args[1:], stdout, stderr) }` before the mode
  switch.
- `newRegistry() (*game.Registry, error)` returns `game.NewRegistry(crashv1.Adapter())` and replaces the
  inline call in `runAudit`. `export` treats a registry error as exit 1, as `audit` does.
- `settlementPolicies() []execution.Policy` returns `{execution.CrashV1Policy()}`.

`main_test.go`, `TestRun`: add `{name: "usage lists export", args: nil, wantCode: 2, wantStderr:
"crashctl audit export [-unsettled] [-format NAME] ROUND_ID FILE"}`.

Unit tests in `cmd/crashctl/export_test.go`, run by `make test`. Each is required:

- **`TestExportUsage`**, with `ORVEX_DATABASE_URL = unreachableDSN`, so exit 2 proves no connection was
  attempted. Each case requires exit 2, empty stdout and non-empty stderr:
  - `audit export`; one positional; three positionals; `-unsettled` after the positionals; `-bogus`;
  - `ROUND_ID` uppercase, in braces, with a `urn:uuid:` prefix, without hyphens, 35 characters, empty,
    and the nil UUID;
  - `-format ""`, `-format v1`, `-format orvex/audit-history/v01`;
  - `ORVEX_DATABASE_URL` unset, empty, and `::bad`. For `::bad`, stderr is exactly
    `crashctl: audit export: invalid ORVEX_DATABASE_URL\n`. These cases use a valid `ROUND_ID` and
    `FILE`.

  Separately, `audit export -h` and `audit export -help` exit 0, with empty stdout and the usage on
  stderr.
- **`TestExportFormatUnsupported`**: `-format orvex/audit-history/v2` with `unreachableDSN` exits 3, and
  `FILE` is not created.
- **`TestExportFileExists`**:
  - an existing file exits 2, its content unchanged;
  - a directory exits 2;
  - a dangling symlink exits 2, and its target is not created.

  Each prints a single stderr line containing `exists; refusing to overwrite`.
- **`TestExportMissingDirectory`**: `FILE` in a missing directory exits 4. Stderr names the directory and
  is not a connection error.
- **`TestExportConnectError`**: `unreachableDSN` with a path and with `-` exits 4, with empty stdout and
  one stderr line with the prefix. No `FILE` exists afterwards.
- **`TestExportNeverPrintsPassword`**: the three DSNs of `TestMigrateNeverPrintsPassword`, with the
  password `sentinel-pw`. Each exits 2 or 4, and neither stream contains the password.
- **`TestExportConnConfig`**:
  - A plain DSN, through `databaseConfig()` and `exportConnConfig`, gives
    `default_transaction_read_only=on`, `application_name=crashctl`,
    `idle_in_transaction_session_timeout=60000` and `ConnectTimeout == 10s`.
  - The DSN `…?default_transaction_read_only=off&application_name=x&idle_in_transaction_session_timeout=5000&connect_timeout=3`
    gives `on`, `x`, `5000` and 3 s.
- **`TestExportExitCodes`**, a table over `exportExitCode`:
  - nil → 0;
  - `ErrRoundNotFound` → 5;
  - wrapped `ErrNoCheckpoint` → 1, wrapped `ErrCorrupt` → 1;
  - wrapped `*pgconn.PgError` with codes `42501`, `42P01`, `40001` and `08006` → 4, and
    `io.ErrUnexpectedEOF` → 4;
  - `*verification.Failure{Unsupported: true}` → 3, both bare and wrapped in `errSettlementCheck`;
  - `errNoPolicy` wrapped in `errSettlementCheck` → 3;
  - `errFormatUnsupported` → 3;
  - `*verification.Failure{}` wrapped in `errSettlementCheck` → 1;
  - `errSettlementCheck` wrapping a `Final` error → 1;
  - `errNotSettled` → 6;
  - `errOutputExists` → 2.
- **`TestSettlement`**, on golden files with no database:
  - worked-example → yes;
  - in-progress → no;
  - worked-example cut after record 9 → no. Rebuild the header with `canonjson.Marshal`: checkpoints 0
    and 9 with the manifest state hashes, `head` the `chain.LinkHash` of record 9 (assert it equals
    `507826d4…`), and the golden `configSecret`;
  - `tamper/rules-version-2.jsonl` → unknown, with an `Unsupported` `*verification.Failure` at stage
    `rules`;
  - `tamper/commitment-edited.jsonl` → unknown, with a `*verification.Failure` at stage `fairness`;
  - worked-example with a nil policy list → unknown, with `errors.Is(err, errNoPolicy)`.
- **`TestSettlementPoliciesCoverRegistry`**:
  - for each policy in `settlementPolicies()`, `newRegistry()` has an adapter whose `Descriptor()` equals
    the policy's;
  - for `crashv1.Adapter().Descriptor()`, exactly one policy has an equal descriptor;
  - no two policies share a descriptor.
- **`TestExportReport`**: exact bytes for four cases:
  - settled with `S = N`;
  - settled with `S < N`, with the note;
  - `settled no`, with the note and the warning;
  - `settled unknown`, with the warning and without the note.

### Step 5: integration helpers (`cmd/crashctl/export_helpers_integration_test.go`)

`//go:build integration`, `package main`. Adapt them from `internal/runtime/execution/helpers_integration_test.go`
(§2.2):

- **`goldenRound`**: `{roundID [16]byte; lines [][]byte /* record lines 0..N */; exec *execution.Executor}`.
  `hash(seq)` is `chain.LinkHash(lines[seq])`.
- **`seedGolden(t, pool, file string) goldenRound`**:
  - Read the golden file, decode the genesis and the header's `configSecret`, and get the typed config
    with `crashv1.Adapter().DecodeConfig`.
  - Build the executor with `execution.New(pool, reg, cfg, execution.CrashV1Policy())`, where `reg` comes
    from `newRegistry()` and `cfg` is `DefaultConfig()` with `EngineBuild = "golden"`.
  - Call `CreateRound` with the genesis `Game`, `RulesVersion`, the config and `CreatedAtUnixMicro`.
    Require `Created.Genesis` to equal the golden record-0 line.
  - Fund each player with the sum of their S-10 holds, in sorted player order (`fundPlayer`).
  - Seed every command record's admission in order (`seedAdmission`) with its exact `CommandID`,
    `Payload`, `EffectiveTimeUnixMicro` and hold. Require each seeded sequence to equal the record's
    sequence.
  - Execute nothing.
- **`fundPlayer`, `seedAdmission`, `holdFor`**: copy them from the execution helpers. `holdFor` uses
  `admission.CrashV1Policy().Classify`.
- **`exportCmd(t, args ...string) (code int, stdout, stderr string)`**: runs `run(append([]string{"audit",
  "export"}, args...), strings.NewReader(""), …)`.
- **`replicaExec(t, pool, sql, args...)`**: one transaction that runs `SET LOCAL session_replication_role
  = replica`, then the statement, then commits.
- **`restrictedRole(t, superDSN string, grants []string) (roleDSN string)`**:
  - Create `orvex_export_<n>` (`n` from a package-level `atomic.Int64`) as `LOGIN PASSWORD
    'sentinel-pw'`.
  - Run each grant in the test database. The grants carry `%[1]s` for the role and `%[2]s` for the
    database; quote both with `pgx.Identifier{…}.Sanitize()`, and get the database from
    `SELECT current_database()`.
  - Return `superDSN` with the user and password replaced (via `net/url`).
  - At cleanup, on a fresh superuser connection to the test database (`context.Background()`), run
    `DROP OWNED BY <role>`, then `DROP ROLE <role>`. Register the cleanup after the database is created,
    so it runs before the database is dropped.
- **`exportGrants`**: the minimal grant set, following X-11 with §2.1:

  ```
  GRANT CONNECT ON DATABASE %[2]s TO %[1]s
  GRANT USAGE ON SCHEMA orvex TO %[1]s
  GRANT SELECT ON orvex.rounds, orvex.chain_records TO %[1]s
  GRANT SELECT (round_id, config_secret) ON orvex.round_secrets TO %[1]s
  GRANT SELECT (round_id, sequence, state_hash) ON orvex.round_snapshots TO %[1]s
  ```

- Do not redefine `lines`, `multilineFailingWriter`, `goldenDir`, `loadManifest` or `unreachableDSN`.
  Reuse them.

### Step 6: integration tests (`cmd/crashctl/export_integration_test.go`)

These run under `make test-integration`. Each test uses its own `pgtest.MigratedDatabase` unless stated
otherwise, `t.Setenv(databaseURLEnv, dsn)`, and output paths under `t.TempDir()`. No `t.Parallel()`. No
sleeps. Expected heads come from the golden bytes via `chain.LinkHash`, and are also asserted against the
spec §8 literals.

- **`TestExportExecutedRounds`** (acceptance). One subtest per spec §8 row. Run `seedGolden`, then
  `Drain(ctx, lease, 100)`, then `audit export ROUND_ID <path>`.
  - Exit code: as spec §8. For in-progress, exit 6. Stderr is exactly the spec §6 message naming
    `sequence 2 of 3`, and no file exists. Then a run with `-unsettled` to a new path exits 0.
  - The stdout report is exactly spec §6, with the spec §8 `records` line and the note, `settled` and
    warning lines as applicable. Use `buildinfo.String("crashctl")`.
  - The file's mode is `0600`. Its header's checkpoint sequences equal the spec §8 snapshot list. Its
    record lines equal golden lines `2..S+2`. Its bytes equal `eventstore.ExportHistory` read directly
    from the pool.
  - `audit verify -expect-head <spec §8 head>` on the file exits 0, and stdout contains
    `chain ok records <S+1> head <head>`.
  - When `S < N` (worked-example and in-progress), `audit verify -expect-head <manifest head>` exits 1,
    and stderr contains `chain failed at sequence <S>`.
- **`TestExportFollowsSnapshots`**: worked-example via `seedGolden`. Let `S_k` be the largest of
  {0, 9, 19, 21} that does not exceed `k`. Check `k = 0` (after `CreateRound` only), then after each
  `ExecuteNext`, up to `k = 22`:
  - Without `-unsettled`: for `k < 21`, exit 6, stderr names `sequence <S_k> of <k>`, and no file
    exists. For `k ≥ 21`, exit 0.
  - With `-unsettled`, to another path: exit 0; the report has
    `records <S_k+1> of <k+1> head <hash(S_k)>`; `settled no` below 21 and `settled yes` from 21; the note
    appears exactly when `S_k < k`.
  - `audit verify -expect-head <hash(S_k)>` on the written file exits 0.
- **`TestExportStdout`**:
  - Worked-example, drained. `export ROUND_ID -` writes to stdout exactly the bytes of a path export of
    the same round. Stderr is exactly the spec §6 report. `audit verify -expect-head <hash(21)> -`, with
    those bytes on stdin, exits 0.
  - In-progress, drained. `-` without `-unsettled` exits 6, with empty stdout.
- **`TestExportUnknownRound`**: `pgtest.LoadHistory(worked-example)`. An unknown UUID exits 5. Stderr is
  exactly `crashctl: audit export: round <uuid> not found\n`, stdout is empty and no file exists.
- **`TestExportNoSnapshot`**: `LoadHistory(worked-example, []uint64{})` exits 1, stderr contains
  `has no snapshot`, and no file exists.
- **`TestExportCorruptRound`**, two databases, each loaded with `LoadHistory(worked-example)`:
  1. `replicaExec` deletes the `round_secrets` row. The export exits 1, stderr contains
     `violates an export invariant`, and no file exists.
  2. `replicaExec` sets `config_secret` to `{"seed":"<64 × f>"}` as bytes.
     - Without the flag: exit 1. Stderr contains `fairness failed at sequence 0` and `-unsettled`. No file
       exists.
     - With `-unsettled`: exit 0. The report has `settled unknown` and the warning.
     - `audit verify` on that file exits 1, with stage `fairness`.
- **`TestExportUnsupportedRules`**: `LoadHistory(tamper/rules-version-2.jsonl)`.
  - Without the flag: exit 3, and stderr contains `rules unsupported at sequence 0`.
  - With `-unsettled`: exit 0, and the report has `settled unknown`.
  - `audit verify` on the file exits 3.
- **`TestExportDatabaseErrors`**:
  1. `pgtest.EmptyDatabase`, which has no schema: exit 4, stderr contains `42P01`, and no file exists.
  2. A `restrictedRole` with `exportGrants` minus the `round_secrets` grant, password `sentinel-pw`:
     exit 4, stderr contains `42501`, neither stream contains `sentinel-pw`, and no file exists.
- **`TestExportMinimalGrants`**: worked-example, drained through the superuser pool, and a
  `restrictedRole` with exactly `exportGrants`.
  - The export exits 0, and `audit verify -expect-head <hash(21)>` on its file exits 0.
  - Through the role's own connection, closed before cleanup, each of these gives `42501`:
    - `SELECT state FROM orvex.round_snapshots LIMIT 1`;
    - `SELECT 1 FROM orvex.admissions LIMIT 1`;
    - `SELECT close_offset_micros FROM orvex.round_secrets LIMIT 1` (§2.1).
- **`TestExportOutputFailures`**: worked-example, drained.
  1. `-` with `multilineFailingWriter{}` as stdout: exit 4, and one stderr line containing
     `write stdout: disk full: sector 7`.
  2. A path with `multilineFailingWriter{}` as stdout, so the report write fails: exit 4, one stderr line
     containing `write report`, and the file does not exist afterwards.

### Step 7: docs

`docs/architecture/persistence.md`:

- **§2.** Extend the last bullet. `eventstore.ExportHistory` also returns `RoundRecords`, the round's
  chain record count read in the same snapshot. `crashctl audit export` (§14) writes the file.
- **§8.** Add a matrix row: auditor (`crashctl audit export`).
  - Append-only tables: `SELECT` on `rounds` and `chain_records`, and column-level
    `SELECT (round_id, sequence, state_hash)` on `round_snapshots`.
  - Heads, `ledger_accounts`, `outbox_messages`: none.
  - `round_secrets`, `round_snapshots.state`: `SELECT (round_id, config_secret)` on `round_secrets` only
    (every round's seed, §6), with no `close_offset_micros` and no `round_snapshots.state`.

  Add one sentence after the engine paragraph pointing to §14 for the auditor grants.
- **New §14 "History export CLI (LBT-19)"** at the end. It links the approved spec
  `../specs/73dbe98d_audit-export-cli.md` and covers:
  - the usage line;
  - one `SnapshotReadTxOptions()` transaction, plus the session parameters (read-only forced,
    `application_name`, idle timeout);
  - records `0..S` and the `records S+1 of N+1` line with the note;
  - the settlement gate (replay, then `Policy.Final`) and `-unsettled`. A file written with
    `-unsettled` can disclose the crash point and must be kept private;
  - the output rules: `O_EXCL`, `0600`, never overwrite, removed on failure;
  - the head line and its ADR-001 trust caveat (an anchor only once recorded elsewhere);
  - the exit-code table 0–6 (spec X-9);
  - a "Grants the auditor role needs (input to LBT-14)" subsection with the SQL of §2.1 (X-11 with the
    column-level `round_secrets` grant), a note that the column list excludes `close_offset_micros` and
    `round_snapshots.state`, and that `TestExportMinimalGrants` proves the set.

`README.md`:

- **Status:** add "the audit history export CLI
  ([LBT-19](https://linear.app/lbtworks/issue/LBT-19/crashctl-audit-export-round-id))" to the list.
- **Layout:** the `cmd/crashctl` line reads `crashctl audit verify|replay|chain (LBT-9), crashctl audit
  export (LBT-19), crashctl migrate up|down|status (LBT-10)`.
- **Development:** add the bullet `crashctl audit export [-unsettled] ROUND_ID FILE: write a settled
  round's audit history file from the database named by ORVEX_DATABASE_URL`.

### Step 8: verification

Run every command in the foreground to completion, with a long tool timeout (up to 3600 s). Do not leave
any of them in the background. Record each exact command, its exit status and its wall-clock duration,
for example with `time`, and put them all in the report. Each must exit 0 unless noted.

```sh
make test
make test GOTESTFLAGS=
make lint
go vet ./...
go vet -tags integration ./...
make build
make golden-check
make test-integration
go test -race -tags integration -count=3 -run 'TestExport' ./cmd/crashctl/ ./internal/persistence/eventstore/
```

Smoke check against Compose. Run it after `make build`, and check first that port 55432 is free
(`docker ps`):

```sh
rm -f /tmp/lbt19-smoke.jsonl
docker compose -f deployments/docker/compose.yaml up -d --wait
ORVEX_DATABASE_URL='postgres://orvex:orvex@127.0.0.1:55432/orvex?sslmode=disable' ./bin/crashctl migrate up
ORVEX_DATABASE_URL='postgres://orvex:orvex@127.0.0.1:55432/orvex?sslmode=disable' ./bin/crashctl audit export 0190f3a8-7b3c-7d4e-8f00-000000000001 /tmp/lbt19-smoke.jsonl
docker compose -f deployments/docker/compose.yaml down -v
```

The `audit export` line must exit **5**. Its stderr must be the single line
`crashctl: audit export: round 0190f3a8-7b3c-7d4e-8f00-000000000001 not found`, and
`/tmp/lbt19-smoke.jsonl` must not exist afterwards. Always run `down -v`, even after a failure.

Final checks:

- `git diff --stat` and `git status --short` show no change to `go.mod`, `go.sum`, `migrations/`,
  `docs/adr/`, any `testdata/`, `internal/game/`, `internal/canonjson/`, `internal/runtime/`,
  `internal/persistence/postgres/`, `Makefile`, `.github/` or `.golangci.yml`. New and changed files are
  exactly those in §3.
- `make golden-check` passing, with `testdata/` untouched, shows that history format v1 and every golden
  fixture are unchanged.
- The `TestMigrate*` tests pass with their files unedited. `git diff cmd/crashctl/migrate_test.go
  cmd/crashctl/migrate_integration_test.go` must be empty.

## 6. LBT-19 acceptance as tests (spec §9)

| Acceptance | Tests |
|---|---|
| An executed round exports to a file that `crashctl audit verify -expect-head` accepts | `TestExportExecutedRounds`, `TestExportFollowsSnapshots`, `TestExportStdout` |
| Unknown round maps to a documented code | `TestExportUnknownRound` (5), `TestExportExitCodes` |
| Not settled maps to a documented code | `TestExportExecutedRounds` (in-progress, 6), `TestExportFollowsSnapshots`, `TestExportStdout`, `TestExportExitCodes` |
| Database errors map to documented codes | `TestExportConnectError` (4), `TestExportDatabaseErrors` (4), `TestExportNoSnapshot` (1), `TestExportCorruptRound` (1), `TestExportUnsupportedRules` (3), `TestExportExitCodes` |
| I/O failures map to documented codes | `TestExportFileExists` (2), `TestExportMissingDirectory` (4), `TestExportOutputFailures` (4) |
| `ORVEX_DATABASE_URL` as `migrate` | `TestExportUsage`, `TestExportNeverPrintsPassword`, `TestExportConnConfig`; existing `TestMigrate*` unchanged |
| Secret only after settlement (X-7) | `TestExportFollowsSnapshots`, `TestExportExecutedRounds`, `TestSettlement`, `TestExportCorruptRound`, `TestReportState` |
| Read-only, least privilege (X-4, X-11 with §2.1) | `TestExportConnConfig`, `TestExportMinimalGrants`, `TestExportDatabaseErrors` |
| Records after the newest snapshot are reported (X-5, X-6) | `TestExportGoldenHistories`, `TestExportStopsAtLatestSnapshot`, `TestExportFollowsSnapshots`, `TestExportReport` |
| Usage lists the command | `TestRun` "usage lists export", `TestSettlementPoliciesCoverRegistry` |

## 7. Report contents (for the builder)

- Each Step 8 command, with its exit status and duration.
- §2 of this plan, flagged as differences from the spec's assumptions that follow `main`. Item 1 changes
  X-11's SQL text to column-level on `round_secrets`, with the reasons.
- Spec §2 items 1–16, verbatim or summarized, for the PR description (AGENTS.md "Flag conflicts
  explicitly"). No ADR conflict.
- Confirmation that history format v1, golden fixtures and migrations are unchanged.
- Any test that disagreed with the spec, and which side was wrong.
