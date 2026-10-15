# `crashctl audit export ROUND_ID` (LBT-19)

Status: **APPROVED (2026-10-15).** The engineer approved X-1 to X-13 as written, with no edits, and
accepted every point of section 2: the settlement gate and `-unsettled`, exit codes 5 and 6,
`Export.RoundRecords` and `verification.Report.State`. Section 0 described the planning run and no
longer applies; implementation follows section 10.

- Linear: [LBT-19](https://linear.app/lbtworks/issue/LBT-19/crashctl-audit-export-round-id), milestone
  M2 · Durable Execution. Blocked by LBT-12. Related: LBT-9, LBT-10.
- Linear text: "Add `crashctl audit export ROUND_ID`. It writes a round's verifiable history file from
  PostgreSQL using `eventstore.ExportHistory` (LBT-10), now that LBT-12 writes snapshots. […] Acceptance:
  for a round executed against real PostgreSQL, `crashctl audit export` produces a file that `crashctl
  audit verify` accepts with the expected head hash; unknown round, database errors and I/O failures map
  to documented exit codes; the database URL comes from `ORVEX_DATABASE_URL`, as `crashctl migrate` does.
  Reference ADR-001/006, LBT-9 A-11/A-16, LBT-10 P-24."
- Governing ADRs: ADR-001 (authoritative history, anchoring limitation), ADR-006 (independent audit CLI,
  fail closed, real PostgreSQL tests), ADR-003 and ADR-004 (secret crash outcome, reveal after
  settlement), ADR-005 (system of record, isolation), ADR-007 (thin `cmd`, one module). Mirrors in
  `docs/adr/`.
- Approved inputs:
  - "LBT-9 spec": `docs/specs/8bebb70f_canonical-codec-audit-cli.md` (A-1 to A-20), merged.
  - "LBT-10 spec": `docs/specs/17380a6e_postgresql-schema-migrations.md` (P-1 to P-26), merged.
  - "LBT-11 spec": `docs/specs/5a3d52fc_admission-sequencer.md` (S-1 to S-14), merged.
  - "LBT-12 spec": the approved execution, ledger and outbox design (E-1 to E-17). It is being implemented
    and is not on `main`. The copy read for this plan is outside the repo and is not copied in.
  - LBT-18 (audit ledger stage, history format v2) is being specified in parallel. Nothing here depends on
    it; X-10 keeps the command open for it.

Decision IDs are `X-n`, so they do not clash with `D-n`, `A-n`, `P-n`, `S-n` or `E-n`.

## 0. What this run delivers

- This document is the deliverable. The engineer approves its copy under `docs/specs/`.
- **Builder, this run: make no repository changes.** Do not write Go code, tests or docs, and do not touch
  `go.mod`, `go.sum`, `Makefile`, `.golangci.yml`, `README.md`, `AGENTS.md`, `docs/adr/` or
  `docs/architecture/`. The gates must still pass on the unchanged tree: `make test`, `make lint`,
  `go vet ./...`, `make build`, `make golden-check`.
- After approval, section 10 is the implementation plan.
- **Implementation precondition: LBT-12 is merged on `main` and the worktree is based on it.** Check that
  these exist:
  - `internal/runtime/execution` with `New`, `DefaultConfig`, `Config.EngineBuild`, `Executor.CreateRound`,
    `ExecuteNext`, `Drain`, `Lease`, `Policy` (with `Descriptor()` and `Final(state any) (bool, error)`)
    and `CrashV1Policy()`;
  - LBT-12's migration (`0008_execution_ledger` or the next free number);
  - snapshot rows at round creation and on records that emit `RoundStarted`, `RoundCrashed` or
    `RoundSettled` (E-13).

  If LBT-12 is not merged, stop and report "blocked on LBT-12". If merged names differ from the LBT-12
  spec, use the merged names: this spec fixes behavior, not identifiers. If the merged snapshot cadence
  differs from E-13, recompute the section 8 expectations from the merged behavior and report it; never
  weaken an assertion to make a run pass.
- If LBT-18 merged first and changed `eventstore.ExportHistory`, `eventstore.Export` or
  `verification.Report`, apply the X-5 and X-7 additions to the merged shapes.

## 1. Current state (verified while planning)

- `main` is at `8b26867` (LBT-11 merged). Toolchain: go1.26.5. `go.mod` requires
  `github.com/cockroachdb/apd/v3 v3.2.3`, `github.com/jackc/pgx/v5 v5.11.0` and
  `github.com/testcontainers/testcontainers-go v0.44.0`. This plan adds no module.
- `cmd/crashctl`:
  - `main.go`: `run(args, stdin, stdout, stderr) int`; `crashctl audit verify|replay|chain [-expect-head HEX]
    FILE` (A-16, LBT-9 §7); the registry is built inline in `runAudit` as
    `game.NewRegistry(crashv1.Adapter())`; exit constants `exitOK 0`, `exitFailed 1`, `exitUsage 2`,
    `exitUnsupported 3`, `exitIO 4`. `audit <sub> -h` exits 0.
  - `migrate.go`: `crashctl migrate up|down|status` (P-6). It reads `ORVEX_DATABASE_URL`
    (`databaseURLEnv`), prints `<prefix>ORVEX_DATABASE_URL is not set` or `<prefix>invalid
    ORVEX_DATABASE_URL` (exit 2), sets a 10 s `ConnectTimeout` unless the DSN has one, connects with
    `pgx.ConnectConfig`, and prints every failure as one stderr line (`oneLine`) with the password replaced
    by `xxxxx`. A connect failure is exit 4.
  - `migrate_integration_test.go` declares the package's only `TestMain` (`pgtest.Run`).
    `main_test.go` defines `goldenDir`, `runAuditCmd`, `loadManifest`; `migrate_test.go` defines
    `unreachableDSN` and `runMigrateCmd`; `migrate_integration_test.go` defines
    `multilineFailingWriter`.
- `internal/persistence/eventstore/export.go`: `ExportHistory(ctx, db TxBeginner, roundID [16]byte, w)
  (Export, error)` in one `postgres.SnapshotReadTxOptions()` transaction (REPEATABLE READ, READ ONLY,
  always rolled back). `Export{RoundID, Records (S+1), Head, Checkpoints}`. Sentinels `ErrRoundNotFound`,
  `ErrNoCheckpoint`, `ErrCorrupt`; database errors come back wrapped (`eventstore: read secret: …`). It
  writes nothing before the header, so `ErrRoundNotFound` and `ErrNoCheckpoint` leave `w` empty. It reads
  `rounds`, `round_secrets`, `round_snapshots(sequence, state_hash)` and `chain_records(sequence, record,
  hash)`. It never reports how many records exist after `S`.
- `internal/audit/verification`: `Verify(r, Options{Mode, Registry, ExpectHead}) (Report, error)`,
  `Format = "orvex/audit-history/v1"`, `*Failure{Stage, Sequence, Unsupported, Err}`. `Report` has no
  replayed state. The replay stage's `visit` closure sees the state after genesis and after every record.
- `internal/runtime/admission` (LBT-11) is on `main`; `internal/runtime/execution` (LBT-12) is not.
  Migration `0007_admission_window` added `round_admission_heads.sealed_sequence`: after an admitted
  `SettleRound`, every later admission is rejected (`round_sealed`), so in production the settlement
  record is a round's last record.
- `pgtest`: `Run`, `EmptyDatabase`, `MigratedDatabase`, `Pool`, `WaitUntilBlocked`, `LoadHistory(t, pool,
  history, checkpoints)`. The container user `orvex` is a superuser.
- LBT-9 golden histories: header checkpoints `worked-example` `[0, 9, 22]`, `instant-crash` `[5]`,
  `empty-round` `[3]`, `in-progress` `[0, 3]`. Their phase records (checked during planning):

  | history | `RoundStarted` | `RoundCrashed` | `RoundSettled` | last record |
  |---|---|---|---|---|
  | worked-example | 9 | 19 | 21 | 22 (`PlaceBet` rejected `round_settled`) |
  | instant-crash | 2 | 4 | 5 | 5 |
  | empty-round | 1 | 2 | 3 | 3 |
  | in-progress | 2 | — | — | 3 (accepted `CashOut`) |

- `crash/v1` `Phase` values: `PhaseBetting`, `PhaseRunning`, `PhaseCrashed`, `PhaseSettled` (terminal).
  `State.Seed` is secret until `RoundSettled`; the crash outcome until `RoundCrashed`.

## 2. ADR alignment, conflicts and flagged interpretations

No decision contradicts an accepted ADR. These points need the engineer's explicit attention (AGENTS.md
"Flag conflicts explicitly"). Copy this section into the implementation PR.

1. **LBT-9 A-11, "`configSecret` is required in v1. Exports are operator artifacts" (interpretation, needs
   approval).** Format v1 is unchanged: every exported file carries the secret config. The CLI adds a
   settlement gate (X-7): by default it writes a file only when the round is settled at the file's last
   record, so the file's own chain already reveals the secret. `-unsettled` lifts the gate. LBT-9 §12 open
   item 3 (an optional `configSecret` for public exports of unsettled rounds) stays open; it is a format
   change, not this issue.
2. **ADR-004 "reveal after settlement" and ADR-003 "the public projection must not expose secret crash
   outcomes" (alignment with an explicit operator override, needs approval).** The default never writes
   an unrevealed seed. `-unsettled` writes it on purpose, for halted or never-settling rounds (LBT-12 E-14,
   §2 item 14) and phase checkpoints. The flag adds no capability: the export's database grants (X-11)
   already allow reading `round_secrets`. Such a file must be kept private; the report says so.
3. **LBT-12 E-13 rationale, "phase checkpoints let an operator export a round at the end of betting and at
   the crash, before settlement" (consistent, through `-unsettled`).** Without the flag those exports are
   refused with exit 6.
4. **LBT-9 A-16 and LBT-10 P-6 exit codes (extension, needs approval).** Codes 0 to 4 keep their meanings.
   Export adds 5 (round not found) and 6 (round not settled). Database read errors are 4 ("cannot read
   input", A-16), whereas `migrate` maps an SQL failure to 1 ("a migration failed"): export only reads.
5. **LBT-10 P-24 and §7.1 `Export` API (extension, needs approval).** `Export` gains `RoundRecords` (the
   round's chain record count, read in the same snapshot). The algorithm and the file bytes do not change.
6. **LBT-9 A-17 `verification` API (extension, needs approval).** `Report` gains `State`, the replayed
   state after the last record (X-7). No stage, check, message or exit code changes.
7. **LBT-12 E-2 `Policy.Final` (dependency, interpretation).** "Settled" means the LBT-12 execution policy
   reports `Final` for the replayed state at the file's last record. For crash/v1 that is `PhaseSettled`,
   reached only through `RoundSettled`, which carries the seed (A-12).
8. **LBT-10 P-13 and §8 grant matrix; LBT-14 owns roles and grants (alignment).** X-11 records the grant
   set export needs in `docs/architecture/persistence.md`; an integration test proves it with a test-only
   role. No migration creates a role.
9. **LBT-10 P-25 seed exposure (open).** The export role can read every round's secret, unsettled ones
   included. Format v1 needs it. A row-level restriction (a view or RLS over settled rounds) is future
   work, with LBT-14's grants.
10. **ADR-001 anchoring (limitation).** The head the CLI prints comes from the same database as the file.
    It becomes an anchor only once recorded elsewhere. `-expect-head` compares the file's last record, so
    an earlier head cannot be checked against a longer re-export (open item 6).
11. **ADR-007 (alignment).** `crashctl` is the operator CLI (LBT-10 §2 item 3). It now imports
    `internal/persistence/eventstore` and `internal/runtime/execution` (policies only). Binaries do not
    import each other. No new module.
12. **LBT-10 P-23 rule 1 (alignment).** The export's only transaction is `SnapshotReadTxOptions()`. The
    session is also read-only by default (X-4).
13. **LBT-10 P-24 with LBT-12 E-13 (consequence).** A file ends at the newest snapshot. Production rounds
    are complete once settled (LBT-11 seals admission at `SettleRound`), but databases that seed
    admissions past settlement (tests: worked-example record 22) export one record short, and the report
    says so (X-6).
14. **LBT-12 approval status (input caveat).** The LBT-12 copy read for this plan is headed "PROPOSED" with
    unticked boxes. This spec treats E-1 to E-17 as approved, as the request states. If approval changed
    E-2 (`Policy`), E-13 or E-16, decisions X-6, X-7 and X-13 need review.
15. **LBT-18 (coordination).** `-format` (X-10) is the extension point for v2. The `Export` and `Report`
    additions are additive; the later of LBT-18 and LBT-19 to merge adapts to the other.
16. **AGENTS.md test commands.** The implementation changes `internal/persistence/eventstore`, so it must
    also run `make test-integration`.

| ADR or approved clause | Where satisfied |
|---|---|
| ADR-001: the immutable ordered history is authoritative; preserve commitments and reveals | X-5 (verbatim bytes), X-7 (settlement read from the chain) |
| ADR-001 limitation: tamper-evident only against a separately trusted anchor | X-8 |
| ADR-003: the public projection must not expose secret crash outcomes | X-7 |
| ADR-004: reveal the seed after settlement | X-7 |
| ADR-005: PostgreSQL is the system of record; export isolation | X-4, X-5 |
| ADR-006: independent audit CLI; fail closed on unknown artifacts; real PostgreSQL tests | X-7, X-9, X-13 |
| ADR-007: thin `cmd`, shared code in `internal/`, one module | X-12 |
| LBT-9 A-11 (format v1), A-14 (unsupported is exit 3), A-16 (CLI conventions) | X-1, X-3, X-9, X-10 |
| LBT-10 P-6 (`ORVEX_DATABASE_URL`, timeout, redaction), P-23, P-24, P-13, P-25 | X-4, X-5, X-6, X-11 |
| LBT-12 E-13 (snapshot cadence), E-16 (round creation), E-2 (`Policy.Final`) | X-6, X-7, X-13 |

## 3. Decision summary

Tick a box to approve, or edit the value. The implementation run uses approved values only.

| # | Decision | Proposed value | Approve |
|---|---|---|---|
| X-1 | Command syntax | `crashctl audit export [-unsettled] [-format NAME] ROUND_ID FILE`; both positionals required; `FILE` `-` writes stdout | [x] |
| X-2 | Round ID format | canonical D-11 UUID text only (36 lowercase characters, 8-4-4-4-12, `canonjson.ParseUUID`); nil UUID rejected; exit 2 before connecting | [x] |
| X-3 | Output handling | build the file in memory; `FILE` must not exist (checked before connecting, created with `O_EXCL`, mode `0600`); never overwrite; removed on any failure; exit 0 ⇔ complete file | [x] |
| X-4 | Database connection | `ORVEX_DATABASE_URL` exactly as `migrate` (shared helper); one connection; 10 s connect timeout; session `default_transaction_read_only=on` (forced), `application_name=crashctl`, `idle_in_transaction_session_timeout=60000` (DSN may override the last two) | [x] |
| X-5 | Read isolation | `ExportHistory`'s single REPEATABLE READ READ ONLY transaction; `Export.RoundRecords` read in the same snapshot; no other statement | [x] |
| X-6 | In-progress rounds | records `0..S` only (P-24); report `records S+1 of N+1` and a `note:` line when `S < N` | [x] |
| X-7 | Secret handling | write only if settled at `S` (`verification.Verify` in replay mode, then the LBT-12 policy's `Final`); otherwise exit 6; `-unsettled` writes anyway with a warning; `verification.Report.State` added | [x] |
| X-8 | Report and head | fixed-format report on stdout (stderr when `FILE` is `-`) with `head <hex64>`, the exact `-expect-head` value; no sidecar file | [x] |
| X-9 | Exit codes and errors | 0 ok, 1 failed, 2 usage, 3 unsupported, 4 I/O (all database errors), 5 round not found, 6 not settled; one stderr line, password masked, no secret | [x] |
| X-10 | Format selection (v2 readiness) | `-format`, default and only accepted value `orvex/audit-history/v1`; a well-formed unknown version is exit 3; LBT-18 adds v2 as an opt-in value | [x] |
| X-11 | Database role and grants | `SELECT` on `rounds`, `round_secrets`, `chain_records`; `SELECT (round_id, sequence, state_hash)` on `round_snapshots`; `USAGE` on schema `orvex`; `CONNECT`; nothing else; documented, created by LBT-14 | [x] |
| X-12 | Code layout | `cmd/crashctl/export.go`, `database.go` (shared with `migrate`); `eventstore.Export.RoundRecords`; `verification.Report.State`; no new module | [x] |
| X-13 | Test strategy | real PostgreSQL via `pgtest`; rounds created, seeded and executed by LBT-12's `Executor`; files checked with `crashctl audit verify -expect-head`; failure paths per exit code | [x] |

## 4. Decisions

### X-1. Command syntax

**Proposal.**

```
crashctl audit export [-unsettled] [-format NAME] ROUND_ID FILE
```

- It is a subcommand of `audit`: it produces the artifact that `audit verify|replay|chain` consume.
- `ROUND_ID` and `FILE` are positional and both required. Flags come first: Go's `flag` package stops at
  the first positional, so a flag after them is an extra argument and a usage error, as in `audit verify`.
- `FILE` `-` writes the history to standard output.
- Flags: `-unsettled` (X-7) and `-format` (X-10). No output flag, no database flag.
- `crashctl audit export -h` prints the subcommand usage and exits 0, like `audit verify -h`.

**Rationale.**
- The issue title fixes `audit export ROUND_ID`.
- A positional `FILE` with `-` mirrors `audit verify FILE|-`. Exports pipe straight into a verifier:
  `crashctl audit export R - | crashctl audit verify -`.
- A required `FILE` makes the safe path the default. With `crashctl audit export R > f`, the shell creates
  `f` even when the export fails, which is the "partial file the caller must discard" of P-24.

**Alternatives.**
- A top-level `crashctl export`: separates export from the commands that read its output.
- `-o FILE` with stdout as default: the redirection problem above.
- An optional `FILE` defaulting to `<round-id>.jsonl` in the working directory: an implicit name and one
  more rule to document.
- Several round IDs per call: out of scope.

### X-2. Round ID format

**Proposal.**
- `ROUND_ID` must be canonical D-11 UUID text: exactly 36 characters, lowercase hex groups 8-4-4-4-12.
  Parse it with `canonjson.ParseUUID(canonjson.String(arg))`.
- The nil UUID (`00000000-0000-0000-0000-000000000000`) is rejected; `rounds_round_id_check` (P-12) means
  it cannot exist.
- Any other text is exit 2 before any connection, with `ROUND_ID must be a canonical lowercase UUID other
  than the nil UUID`.

**Rationale.**
- One spelling: the one `audit verify` prints (`round <uuid>`), chain records hold (`roundId`) and
  operators copy. `-expect-head` is lowercase-only for the same reason.
- Validation before connecting turns typos into usage errors, not database round trips.

**Alternatives.**
- Accept uppercase, braces, `urn:uuid:` or unhyphenated forms: several spellings of one ID, unlike
  `-expect-head`.
- Pass the text to PostgreSQL's `uuid` input: it accepts all those forms and turns a typo into a database
  error (exit 4 instead of 2).

### X-3. Output handling

**Proposal.**
- `ExportHistory` writes into a `bytes.Buffer`. Nothing reaches `FILE` or stdout until the export
  transaction has ended and the settlement check (X-7) has passed or been waived.
- `FILE` other than `-`:
  - before connecting: `os.Lstat(FILE)` succeeding (a file, directory or symlink, dangling or not) is
    exit 2, `<FILE> exists; refusing to overwrite`; an `Lstat` error other than "not exist" is exit 4;
    `filepath.Dir(FILE)` must be an existing directory (`os.Stat`), otherwise exit 4;
  - after the check: `os.OpenFile(FILE, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)`. "Exists" (a race) is
    exit 2; any other error is exit 4. Then write the buffer, `Sync`, `Close`;
  - any failure after the open, including a failed report write (X-8), closes and removes `FILE`.
- Invariant: exit 0 if and only if `FILE` holds the complete history. Every other exit leaves no `FILE`
  that this run created. A process killed while writing can leave a truncated file; it never verifies,
  because its header names a head and a last checkpoint that its lines do not reach.
- `FILE` `-`: the buffer goes to stdout after the checks, the report to stderr (X-8).

**Rationale.**
- P-24: "on error, w may hold a partial file the caller must discard." The CLI discards it itself.
- Buffering keeps the REPEATABLE READ snapshot short and independent of a slow pipe or disk, which would
  otherwise hold back vacuum.
- A refused export (exit 6) never writes the unrevealed secret to disk.
- `O_EXCL` makes "never overwrite" atomic and refuses symlinks. An audit file is evidence; silently
  replacing an earlier export loses it.
- `0600`: the file can hold an unrevealed secret (X-7). Operators widen it on purpose for settled rounds.
- Memory: v1 rounds hold at most 10,000 bets (D-7); A-11 measured a 10,000-`BetLost` settlement record at
  about 1.5 MB; a full round is tens of MB. `verification.Verify` already holds whole files in memory.

**Alternatives.**
- Stream to a temporary file, then rename or link: the secret reaches disk before the settlement check.
- Stream directly into `FILE`: a long snapshot and partial files on failure.
- Overwrite like `pg_dump -f`, or a `-force` flag: loses earlier evidence; a flag can be added later.

### X-4. Database connection

**Proposal.**
- `ORVEX_DATABASE_URL`, exactly as P-6:
  - unset or empty: exit 2, `ORVEX_DATABASE_URL is not set`;
  - unparsable: exit 2, `invalid ORVEX_DATABASE_URL`, never the pgx error (it can echo the DSN);
  - a 10 s connect timeout unless the DSN sets `connect_timeout`;
  - the password masked in every message.
- The environment, parsing and masking code moves out of `migrate.go` into `cmd/crashctl/database.go`,
  shared by both commands. `migrate` keeps its exact messages, exit codes and behavior.
- One `*pgx.Conn` (it implements `eventstore.TxBeginner`), closed before the output is written.
- Session parameters, set in the startup packet through `pgx.ConnConfig.RuntimeParams`:
  - `default_transaction_read_only = on`, always, overriding the DSN: a write would fail with `25006`;
  - `application_name = crashctl`, unless the DSN sets one;
  - `idle_in_transaction_session_timeout = 60000` (milliseconds), unless the DSN sets it.
- The connection may point at a hot standby. REPEATABLE READ READ ONLY works there. The file is then a
  consistent prefix as of the standby's replay position; a recovery-conflict cancel (`40001`) is exit 4,
  and a retry is safe.

**Rationale.**
- The issue requires `ORVEX_DATABASE_URL` "as `crashctl migrate` does". P-6's rationale (no password in
  process listings or shell history) applies unchanged.
- The read-only session is defence in depth: `ExportHistory`'s transaction is already READ ONLY.
- `application_name` lets a DBA tell the export apart in `pg_stat_activity`.
- The idle timeout bounds how long a stopped process (`Ctrl-Z`) can hold its snapshot.

**Alternatives.**
- A separate `ORVEX_AUDIT_DATABASE_URL`: keeps an auditor DSN apart from the migration owner's; one more
  variable now. LBT-14 can add it.
- A `-database-url` flag: leaks into process listings (P-6).
- A pool: one transaction needs one connection.
- `statement_timeout`: would cap a large round's export by time.

### X-5. Read isolation

**Proposal.**
- Exactly one database transaction: `ExportHistory`'s `postgres.SnapshotReadTxOptions()` (REPEATABLE READ,
  READ ONLY), always rolled back (P-23, P-24). The CLI issues no statement of its own.
- `eventstore.Export` gains `RoundRecords int`, the round's chain record count `N + 1`. `ExportHistory`
  reads it in the same transaction, right after the head query, before writing anything:
  `SELECT max(sequence) FROM orvex.chain_records WHERE round_id = $1`. A NULL or a value below `S` is
  `ErrCorrupt`. Nothing else in `ExportHistory` changes; the file bytes are identical.
- The snapshot holds a consistent prefix of the round: Transaction B inserts record `k` and its snapshot
  row together (E-6, E-13), and record `k+1`'s previous-record foreign key requires `k` to be committed
  first.
- Locks: `AccessShare` only. The export neither blocks nor waits for admission (A), execution (B) or
  ownership takeovers.

**Rationale.**
- The "records left out" message (X-6) must agree with the file; a second transaction could see more
  records than the file holds.
- The query is a primary-key index lookup.

**Alternatives.**
- Read `round_execution_heads.processed_sequence`: equal by the deferred head foreign key, but mutable
  bookkeeping and one more grant.
- A second transaction: can disagree with the file.
- SERIALIZABLE READ ONLY DEFERRABLE: may wait, and buys nothing; per-round writes are serialized and
  append-only.
- No count: the user cannot tell that the file is partial.

### X-6. In-progress rounds

**Proposal.**
- The file holds records `0..S`, where `S` is the round's newest snapshot (P-24), never more. Records
  `S+1..N` are left out; a later export includes them once a snapshot covers them.
- With LBT-12's cadence (E-13), a crash/v1 export ends at:

  | round phase | newest snapshot `S` | the file holds |
  |---|---|---|
  | BETTING (created, bets executed) | 0, the genesis | genesis only |
  | RUNNING | the `StartRound` record | through `StartRound` |
  | CRASHED | the `CloseRound` record (`RoundCrashed`) | through `CloseRound` |
  | SETTLED | the `SettleRound` record (`RoundSettled`) | every record (production) |

- The user is told:
  - the report line `records <S+1> of <N+1> head <hex64>`;
  - when `S < N`, a line `note: the file ends at the newest snapshot, sequence <S>; sequences <S+1> to <N>
    are not exported`;
  - a refusal (exit 6, X-7) names `S` and `N`.
- In production LBT-11 seals admission at `SettleRound`, so a settled round's last record is its
  settlement record and its export is complete. Databases that seed admissions past settlement (tests)
  show the note.

**Rationale.** A-11 requires the last checkpoint to be the last record, and P-24 chose to stop at `S`. The
CLI makes the cut visible; it does not move it.

**Alternatives.**
- Fail when `S < N`: settled rounds seeded past settlement would fail, and operators who chose
  `-unsettled` want the file.
- Compute a checkpoint at `N` by replay: proves nothing (LBT-9 §11, P-24).
- Export `0..N` with the last checkpoint at `S`: invalid v1.

### X-7. Secret handling: the settlement gate

**Proposal.**
- Every v1 file carries `configSecret`, the seed (A-11). By default the CLI writes a file only if the round
  is **settled at `S`**:
  1. `verification.Verify(bytes.NewReader(buf), Options{Mode: ModeReplay, Registry: reg})` on the exported
     bytes: format, chain, rules, decode with fairness (`SHA-256(seed) == commitment`), state replay with
     every checkpoint hash;
  2. the execution policy whose `Descriptor()` equals `Report.Descriptor` reports `Final(Report.State)`.
     For crash/v1 that is `PhaseSettled`, which only `RoundSettled` reaches, and `RoundSettled` carries the
     seed (A-12): the file's own chain already reveals the secret it holds.
- `verification.Report` gains `State any`: the replayed state after the last record in `ModeReplay` and
  `ModeVerify`, nil in `ModeChain`. Callers must not mutate it.
- `crashctl` keeps its policy list next to its registry: `execution.CrashV1Policy()`. Historical policies
  are added with their adapters.
- Without `-unsettled`:

  | check result | exit | written |
  |---|---|---|
  | settled | 0 | yes |
  | replay passes, `Final` false | 6 | nothing |
  | `*verification.Failure` with `Unsupported`, or no policy for the descriptor | 3 | nothing |
  | any other `*verification.Failure`, or `Final` returns an error | 1 | nothing |

- With `-unsettled` the same check runs, for the report only, and the file is written whatever its
  result. The report says `settled no` or `settled unknown` and adds a warning line (§6).
- Never printed, in any report or message: `configSecret`, the seed, state, record bytes or the DSN.

**Rationale.**
- ADR-004 reveals the seed after settlement; ADR-003 keeps secret crash outcomes out of public view. A file
  exported while the round is RUNNING lets its holder predict the crash point while cash-outs are open.
- A-11 accepted the secret in v1 because exports were operator artifacts. The default now enforces that
  the secret leaves only after the chain has revealed it. Operators who must examine an unsettled round
  (halted, E-14; never settling, LBT-12 §2 item 14; phase checkpoints, E-13) say so explicitly.
- Settlement is read from the authoritative chain (ADR-001), with the approved per-rules-version `Final`
  predicate, not from bookkeeping columns or crash/v1 knowledge inside `cmd`.
- Replay mode also proves, before the secret is written, that it matches the published commitment and that
  the file passes `crashctl audit replay`.

**Alternatives.**
- No gate (A-11 as written): the default could leak a running round's crash point.
- Always refuse unsettled rounds: no way to export a halted round for investigation.
- LBT-11's window (`sealed_sequence` and its record's result): derived bookkeeping, and rounds loaded by
  `pgtest.LoadHistory` or seeded admissions are never sealed.
- Check the event type at `S` in `crashctl`: crash/v1 knowledge in a binary.
- Omit `configSecret` for unsettled rounds: invalid v1 (A-11, A-20); a format change (LBT-9 open item 3).
- `ModeVerify` for the check: decision replay costs about ten times more (LBT-12 E-3) and is the auditor's
  separate step.
- The gate inside `eventstore`: the store stays game-agnostic, and P-24's API is what LBT-12 tests use.

### X-8. Success report and head

**Proposal.**
- After the file is closed (or the history fully written to stdout), print the §6 report: to stdout, or to
  stderr when `FILE` is `-`.
- Its `records … head <hex64>` line names the head: the file header's `head`, `chain_records.hash` at `S`,
  in exactly the form `-expect-head` accepts. The head is printed nowhere else; there is no sidecar file.
- Trust (ADR-001): the printed head comes from the same database as the file. It is an anchor only once
  recorded outside it (a ticket, a write-once store, another system) at export time. A later export of
  the settled round, checked with `crashctl audit verify -expect-head <recorded head>`, then detects any
  rewrite of records `0..S`. For an unsettled round the head moves with `S`.

**Rationale.**
- The `head` token matches `audit verify`'s `chain ok records <n> head <hex64>`, so one `grep` serves both.
- A sidecar has the same trust as the file and is one more file to keep with it.

**Alternatives.**
- A sidecar `FILE.head`: see above.
- A JSON report: A-16 deferred JSON output.
- No head output: users would read the header or run `audit verify`; the issue asks for it.

### X-9. Exit codes and error output

**Proposal.**

| Code | Meaning |
|---|---|
| 0 | the history was written and the report printed |
| 1 | export failed: the stored round violates an export invariant (`ErrCorrupt`, `ErrNoCheckpoint`), or the settlement check's replay failed (format, chain, decode, fairness, state replay) or `Final` returned an error |
| 2 | usage: missing or extra arguments, unknown flag, bad `ROUND_ID`, malformed `-format`, `ORVEX_DATABASE_URL` unset, empty or unparsable, `FILE` exists |
| 3 | unsupported: `-format` names a version this build does not write; the settlement check met an unknown format, chain, game, rules, schema or state version, or a rules version without a policy (fail closed, ADR-006) |
| 4 | I/O: `FILE`'s directory is missing or unreadable; cannot connect; any database error while reading (connection loss, `42501`, `42P01`, `40001`, …); cannot create, write or sync `FILE`; cannot write stdout or the report |
| 5 | round not found (`ErrRoundNotFound`) |
| 6 | round not settled at its newest snapshot, and `-unsettled` was not given |

- Checks run in §5 order; the first failure decides.
- Failure output: nothing on stdout (with `FILE` `-`, stdout receives bytes only on success) and exactly
  one stderr line, `crashctl: audit export: <message>`, flattened (`oneLine`) and password-masked. §6 lists
  the messages.

**Rationale.**
- 0 to 4 keep their A-16 and P-6 meanings. Export reads; A-16 calls an unreadable input 4.
- A script exporting rounds as they settle must tell "not yet" (6) and "no such round" (5) from "broken
  data" (1) and "retry later" (4).
- `ErrNoCheckpoint` is a broken writer obligation (P-20, E-16 write snapshot 0 at creation), so it is 1.

**Alternatives.**
- Not found and not settled as 1: scripts would parse messages.
- One code for both 5 and 6: cannot tell "never" from "not yet".
- Database errors as 1, like `migrate`: mixes retryable outages with corrupt data.
- A schema-version check before exporting (P-6's 3 for a newer schema): needs `SELECT` on
  `orvex_migrate.schema_migrations`; a missing table or column already fails as 4.

### X-10. Format selection and v2 readiness

**Proposal.**
- `-format NAME`, default `orvex/audit-history/v1` (`verification.Format`). This build accepts exactly that
  value.
- A well-formed name `^orvex/audit-history/v[1-9][0-9]*$` that this build does not write is exit 3,
  `format <NAME> unsupported: this build writes orvex/audit-history/v1`. Any other value is exit 2. Both
  are checked before connecting.
- `export.go` maps format names to exporter functions: `verification.Format` →
  `eventstore.ExportHistory`. LBT-18 adds v2 as another entry. The default stays v1 until an approved
  decision changes it; scripts can pin `-format` today.
- The report's `format` line names what was written; the settlement check uses the verifier for that
  format. Any extra grants a v2 export needs (ledger tables) belong to LBT-18.

**Rationale.** v2 becomes an opt-in value, so no existing invocation changes meaning when it ships, and
pinning is possible from the first release. Unknown formats are "unsupported", as in A-14.

**Alternatives.**
- No flag until LBT-18: also non-breaking if the default stays v1, but nobody can pin now.
- Default "the newest format": upgrading `crashctl` would silently change files, and older verifiers would
  report them unsupported.
- Short names (`v1`): a second vocabulary next to the header value.
- A separate `export-v2` subcommand.

### X-11. Database role and grants

**Proposal.** The export reads four tables. Recommended grants, for LBT-14 to create (P-13: migrations
create no roles):

```sql
CREATE ROLE orvex_auditor NOLOGIN;
GRANT CONNECT ON DATABASE orvex TO orvex_auditor;
GRANT USAGE ON SCHEMA orvex TO orvex_auditor;
GRANT SELECT ON orvex.rounds, orvex.round_secrets, orvex.chain_records TO orvex_auditor;
GRANT SELECT (round_id, sequence, state_hash) ON orvex.round_snapshots TO orvex_auditor;
-- login roles: CREATE ROLE <name> LOGIN IN ROLE orvex_auditor;
```

- Nothing else: no `INSERT`, `UPDATE`, `DELETE` or `TRUNCATE`; no `admissions`, heads, ledger, outbox,
  `domain_events` or `orvex_migrate`.
- `round_snapshots.state` (secret-bearing, P-20, P-25) is excluded by the column list.
- `round_secrets` is readable for every round (§2 item 9). The role is as sensitive as the seeds.
- `docs/architecture/persistence.md` §8 gains the row; the integration test `TestExportMinimalGrants`
  proves the set is sufficient, and `TestExportDatabaseErrors` that a missing grant is exit 4.

**Rationale.** The least privilege that `ExportHistory` and the X-5 count need. Read access to the
secret is inherent in format v1.

**Alternatives.**
- The application role (P-13 matrix): it can write.
- Table-level `SELECT` on `round_snapshots`: exposes `state`.
- A `SECURITY DEFINER` function that returns only settled rounds' secrets: needs a migration and
  crash-specific SQL; future work with LBT-14.

### X-12. Code layout

**Proposal.**
- `cmd/crashctl/main.go`: usage text, package comment, exit constants `exitNotFound = 5` and
  `exitNotSettled = 6`, the `audit export` dispatch, and `newRegistry()` and `settlementPolicies()` shared
  by `audit` and `export`.
- `cmd/crashctl/database.go` (new): `ORVEX_DATABASE_URL` reading and parsing, the default connect timeout,
  and message masking, moved from `migrate.go`.
- `cmd/crashctl/export.go` (new): argument parsing, the §5 algorithm, exit-code mapping, the settlement
  check and the report. It composes library calls; it holds no game rules.
- `internal/persistence/eventstore/export.go`: `Export.RoundRecords` (X-5).
- `internal/audit/verification/verification.go`: `Report.State` (X-7).
- No new module, no migration, no change to `internal/game`, `internal/runtime`, `internal/canonjson`,
  golden fixtures, `Makefile` or CI.

**Rationale.** ADR-007: `cmd` stays a thin composition of `internal/` packages; the two library additions
are each a few lines and additive.

**Alternatives.**
- The settlement check in `internal/audit/verification`: the audit package would depend on
  `internal/runtime/execution`.
- Duplicating the DSN code in `export.go`: two copies of the password masking.

### X-13. Test strategy

**Proposal.**
- Real PostgreSQL through `pgtest` (P-7), in `cmd/crashctl/*_integration_test.go` (`//go:build
  integration`), one migrated database per test. The package keeps its single `TestMain`.
- Rounds are created, seeded and executed by LBT-12's `Executor` (`CreateRound`, then admissions inserted
  directly with LBT-11 S-10 holds, then `Drain` or `ExecuteNext`), reproducing the four LBT-9 golden
  histories, as LBT-12's own golden tests do. Expected heads come from the golden fixture bytes, never from
  the export.
- Each produced file is checked through the CLI: `crashctl audit verify -expect-head <head>`.
- Failure paths use `pgtest.LoadHistory`, superuser corruption under `session_replication_role = replica`,
  an unmigrated database, a restricted test role, and failing writers.
- Tests call `run` in-process. `t.Setenv` rules out `t.Parallel()`.
- Tests assert exit codes, exact stdout reports, one-line stderr prefixes, SQLSTATE codes in stderr, and
  bytes. They never sleep.

**Rationale.** The acceptance names "a round executed against real PostgreSQL". Seeding admissions
directly gives exact effective times and golden bytes; LBT-11's sequencer would need real-time waits.

**Alternatives.**
- Only `pgtest.LoadHistory` rounds: no executor-written snapshots, so the E-13 cadence would be untested.
- Admitting through `admission.Sequencer`: database-clock effective times, waits for the crash deadline,
  and no byte comparison with the goldens.

## 5. Normative algorithm

`runExport(args []string, stdout, stderr io.Writer) int`. `prefix` is `crashctl: audit export: `. Every
failure prints one line `prefix + message` and returns the code shown.

1. Parse flags with a `flag.FlagSet` named `crashctl audit export`, output to stderr: `-unsettled` (bool),
   `-format` (string, default `verification.Format`). `flag.ErrHelp` → usage, 0. Other parse errors → 2.
2. Exactly two positionals, else 2 (`want ROUND_ID and FILE, got <n> arguments`, then the usage).
3. `ROUND_ID` per X-2, else 2.
4. `-format` per X-10: equal to a supported format; else well-formed → 3; else 2.
5. Database configuration per X-4 (shared helper), then apply the session parameters. Unset, empty or
   unparsable → 2.
6. If `FILE` is not `-`: the X-3 pre-checks (exists → 2; `Lstat` error → 4; directory → 4).
7. Connect with `pgx.ConnectConfig` → 4 on error. `context.Background()`, as `migrate`.
8. `exp, err := exporter(ctx, conn, roundID, &buf)`. Close the connection. Map `err`:
   `ErrRoundNotFound` → 5; `ErrNoCheckpoint`, `ErrCorrupt` → 1; anything else → 4.
9. Settlement check (X-7) on `buf.Bytes()` with `newRegistry()` and `settlementPolicies()`, giving
   `settled` ∈ {yes, no, unknown}. Without `-unsettled`: no → 6 (`errNotSettled`); unknown → 3 or 1 per
   the X-7 table.
10. Write: `FILE` per X-3 (`O_EXCL` open, write, `Sync`, `Close`; exists → 2, other errors → 4, and remove
    on failure), or `-` to stdout (error → 4).
11. Print the §6 report to stdout, or stderr for `-`. On error: remove `FILE` (if a path) and return 4.
12. Return 0.

`exportExitCode(err error) int` holds the mapping of steps 8 to 10, so that it can be unit-tested.

## 6. Normative output

**Report** (one `\n`-terminated line each, in this order; bracketed lines are conditional):

```
exporter crashctl <buildinfo.Version>
format <format>
round <uuid>
records <S+1> of <N+1> head <hex64>
[S < N] note: the file ends at the newest snapshot, sequence <S>; sequences <S+1> to <N> are not exported
settled yes|no|unknown
[settled is not yes] warning: the file holds the round's secret config before settlement; keep it private
ok
```

The first line is `"exporter " + buildinfo.String("crashctl")`, as `audit` prints `verifier …`.
`settled no` and `settled unknown` appear only with `-unsettled`.

**Stderr messages** (after the prefix):

| Exit | Message |
|---|---|
| 2 | `want ROUND_ID and FILE, got <n> arguments` (followed by the subcommand usage) |
| 2 | `ROUND_ID must be a canonical lowercase UUID other than the nil UUID` |
| 2 | `-format must be a history format such as orvex/audit-history/v1` |
| 3 | `format <NAME> unsupported: this build writes orvex/audit-history/v1` |
| 2 | `ORVEX_DATABASE_URL is not set` / `invalid ORVEX_DATABASE_URL` (as `migrate`) |
| 2 | `<FILE> exists; refusing to overwrite` |
| 4 | `<FILE>: directory <dir> is not usable: <error>` / the `Lstat` or open error |
| 4 | the masked connection or database error, e.g. `eventstore: read secret: ERROR: permission denied for table round_secrets (SQLSTATE 42501)` |
| 5 | `round <uuid> not found` |
| 1 | `round <uuid> has no snapshot to end an export at` / the `ErrCorrupt` error text |
| 1 or 3 | `cannot establish that round <uuid> is settled: <verification failure>; -unsettled exports it anyway` |
| 3 | `cannot establish that round <uuid> is settled: no settlement policy for <game> rules <n>; -unsettled exports it anyway` |
| 6 | `round <uuid> is not settled at its newest snapshot, sequence <S> of <N>: the file would disclose its unrevealed secret config; -unsettled exports it anyway` |
| 4 | `write <FILE>: <error>` / `write stdout: <error>` / `write report: <error>` |

`<verification failure>` is the `*verification.Failure` text, e.g. `fairness failed at sequence 0: …`.

**Usage** (`main.go`):

```
usage: crashctl [-version]
       crashctl audit verify [-expect-head HEX] FILE
       crashctl audit replay [-expect-head HEX] FILE
       crashctl audit chain  [-expect-head HEX] FILE
       crashctl audit export [-unsettled] [-format NAME] ROUND_ID FILE
       crashctl migrate up [-to N]
       crashctl migrate down -to N
       crashctl migrate status
FILE "-" reads standard input (export: writes standard output).
ORVEX_DATABASE_URL names the database for migrate and export (a PostgreSQL URL or key=value connection string).
```

## 7. Grants

§X-11's SQL is normative for the documentation and for `TestExportMinimalGrants`, which uses a LOGIN role
holding those grants directly (role name `orvex_export_<n>`, dropped at cleanup: `DROP OWNED BY` in the
test database, then `DROP ROLE`).

## 8. Expectations on executed goldens

Rounds are created by `CreateRound` from the golden genesis (round ID, config with the header's seed,
`createdAtUnixMicro`), with `EngineBuild "golden"`, players funded with the sum of their S-10 holds, and
every golden admission seeded before execution, exactly as LBT-12 §8. Snapshot sequences follow E-13.
Link hashes were computed during planning with Python's `hashlib` over `"orvex/audit/v1/record:" ‖ line`.

| history | snapshots | `S` | `N` | default exit | report `records` line | settled |
|---|---|---|---|---|---|---|
| worked-example | 0, 9, 19, 21 | 21 | 22 | 0 | `records 22 of 23 head 3959ed4ca0f2651071b0a325ffae6f5951d132e2e922ca8669974a0031d7fd40` (+ note) | yes |
| instant-crash | 0, 2, 4, 5 | 5 | 5 | 0 | `records 6 of 6 head feea2c86497a5af3450c768129b71626619ae594c5f933b41ca7613e1a6d64da` | yes |
| empty-round | 0, 1, 2, 3 | 3 | 3 | 0 | `records 4 of 4 head 700d572f193400c59c64b24e3989d8a4477b9791a689ecfdfc0e47de35229bd6` | yes |
| in-progress | 0, 2 | 2 | 3 | 6 (0 with `-unsettled`) | `records 3 of 4 head d0df1ee21f9abf0bcbc75ef3f3f05911e081c0e9ef9e6bdd9e336f01c9d703d3` (+ note) | no |

Other worked-example link hashes: record 0 `cfdd053761b51e0dd2c301c785c2bca451a31ed10cdb62f8c1da6cc6b8ecc284`
(LBT-9 §10), 9 `507826d44f608a6733aa3c579967ce4f4179546298372b8134a390c20aaa9bc0`, 19
`f0829a16ccf2511037175c4e0f8851285da331bef1172423c4b1fd732bcc9645`, 22 (the manifest head)
`7cf8cc4b1807099cdd1f16cbc0c790f1a877f30b696ebc0de751997d73ac3da7`. in-progress record 3 (its manifest head):
`f041d95d5bb917ce0f0d51d30c47738c1b97932bebd26ac7f117f98a279c46f6`.

Tests compute these with `chain.LinkHash` over the golden line and also assert the literals above. If they
disagree, find which side contradicts this spec or LBT-12; never edit a fixture.

## 9. LBT-19 acceptance as tests

| Acceptance | Tests (§10 names the files) |
|---|---|
| A round executed against real PostgreSQL exports to a file that `crashctl audit verify` accepts with the expected head | `TestExportExecutedRounds`, `TestExportFollowsSnapshots`, `TestExportStdout` |
| Unknown round maps to a documented code | `TestExportUnknownRound` (5) |
| Database errors map to documented codes | `TestExportConnectError` (4), `TestExportDatabaseErrors` (4), `TestExportNoSnapshot` (1), `TestExportCorruptRound` (1), `TestExportUnsupportedRules` (3), `TestExportExitCodes` |
| I/O failures map to documented codes | `TestExportFileExists` (2), `TestExportMissingDirectory` (4), `TestExportOutputFailures` (4) |
| `ORVEX_DATABASE_URL` as `migrate` | `TestExportUsage`, `TestExportNeverPrintsPassword`, `TestExportConnConfig`; existing `TestMigrate*` unchanged |
| Secret only after settlement (X-7) | `TestExportFollowsSnapshots`, `TestExportExecutedRounds` (in-progress), `TestSettlement`, `TestExportCorruptRound` |
| Read-only, least privilege (X-4, X-11) | `TestExportConnConfig`, `TestExportMinimalGrants` |
| Records after the newest snapshot are reported (X-5, X-6) | `TestExportGoldenHistories`, `TestExportStopsAtLatestSnapshot`, `TestExportFollowsSnapshots`, `TestExportReport` |

## 10. Implementation plan (after approval)

Rules for the implementation run:
- goimports grouping with the `github.com/juano-morello/orvex` local prefix (`.golangci.yml`).
- Error strings lowercase, no trailing punctuation, prefixed `crashctl: `, `eventstore: ` or
  `verification: `.
- `cmd/crashctl` tests are `package main`. Integration files start with `//go:build integration`; do not add
  a second `TestMain`. No `t.Parallel()` in tests that call `t.Setenv`.
- Never edit a golden fixture, `internal/game/**`, `internal/canonjson/**`, `internal/runtime/**`,
  migrations or `docs/adr/`. Never weaken an assertion to make a run pass.
- Reuse existing helpers (`goldenDir`, `loadManifest`, `runAuditCmd`, `unreachableDSN`,
  `multilineFailingWriter`, `oneLine`). If LBT-12 merged an importable helper that seeds golden
  admissions, use it instead of writing `seedGolden`.

### Files

| File | Action |
|---|---|
| `internal/audit/verification/verification.go` | edit: `Report.State` (X-7) |
| `internal/audit/verification/verification_test.go` | edit: `TestReportState` |
| `internal/persistence/eventstore/export.go` | edit: `Export.RoundRecords`, the count query (X-5), doc comments |
| `internal/persistence/eventstore/export_integration_test.go` | edit: assert `RoundRecords` |
| `cmd/crashctl/database.go` | new: shared `ORVEX_DATABASE_URL` config and masking (X-4) |
| `cmd/crashctl/migrate.go` | edit: use `database.go`; behavior unchanged |
| `cmd/crashctl/export.go` | new: `runExport`, `exportConnConfig`, `exportExitCode`, `settlement`, `exportReport`, the exporter table |
| `cmd/crashctl/main.go` | edit: usage, package comment, exit constants 5 and 6, `audit export` dispatch, `newRegistry`, `settlementPolicies` |
| `cmd/crashctl/main_test.go` | edit: `TestRun` row |
| `cmd/crashctl/export_test.go` | new (unit) |
| `cmd/crashctl/export_helpers_integration_test.go` | new: `seedGolden`, `fundPlayer`, `seedAdmission`, `restrictedRole`, `replicaExec`, `exportCmd` |
| `cmd/crashctl/export_integration_test.go` | new (integration) |
| `docs/architecture/persistence.md` | edit: new last section "History export CLI (LBT-19)"; §8 grant matrix row "auditor (`crashctl audit export`)"; §2 export bullet names `RoundRecords` and the CLI |
| `README.md` | edit: Status adds LBT-19; Layout `cmd/crashctl` line adds `audit export` (LBT-19); Development adds the `crashctl audit export` bullet |

Unchanged: `go.mod`, `go.sum`, `Makefile`, `.github/workflows/ci.yml`, `.golangci.yml`, `AGENTS.md`,
`migrations/`, `docs/adr/`, golden fixtures, `internal/game/**`, `internal/canonjson/**`,
`internal/runtime/**`, `internal/persistence/postgres/**`.

### Step 1: `verification.Report.State`

- Add `State any` to `Report` with the X-7 doc comment. In `replayHistory`'s `visit`, assign
  `rep.State = s` before the checkpoint early return, so it ends as the state after the last record.
- `verification_test.go` `TestReportState`:
  - worked-example, `ModeReplay` and `ModeVerify`: `State` is a `crashv1.State` with `Phase ==
    crashv1.PhaseSettled`;
  - in-progress, `ModeReplay`: `Phase == crashv1.PhaseRunning`;
  - worked-example, `ModeChain`: `State == nil`;
  - every existing verification test passes unchanged.

### Step 2: `eventstore.Export.RoundRecords`

- After the head query, `SELECT max(sequence) FROM orvex.chain_records WHERE round_id = $1` into a
  `*int64`; NULL or `< S` → `ErrCorrupt` (`%w: newest record %d before snapshot %d`). Set `RoundRecords =
  max + 1`. Doc comment: "RoundRecords is N + 1, the round's chain records in the same snapshot;
  Records ≤ RoundRecords."
- `export_integration_test.go`:
  - `TestExportGoldenHistories`: `exp.RoundRecords == h.Records`;
  - `TestExportStopsAtLatestSnapshot`: `exp.Records == 10` and `exp.RoundRecords == 23`;
  - the other tests unchanged.

### Step 3: `cmd/crashctl/database.go`

- Move from `migrate.go`: `databaseURLEnv`, `defaultConnectTimeout`, a `databaseConfig() (*pgx.ConnConfig,
  error)` returning `errDatabaseURLUnset` or `errDatabaseURLInvalid` (callers print the existing messages
  and exit 2), and `mask(cfg, err) string` (`oneLine` plus password replacement).
- `migrate.go` calls them. Its messages, order of checks and exit codes stay exactly as they are; every
  `TestMigrate*` test passes unchanged.

### Step 4: `cmd/crashctl/export.go` and `main.go`

Implement §5, §6, X-1 to X-10 and X-12.
- `exportConnConfig(cfg *pgx.ConnConfig)` applies the X-4 runtime parameters.
- `settlement(history []byte, reg *game.Registry, policies []execution.Policy) (settledVerdict, error)`
  returns `settledYes`/`settledNo` with a nil error, or `settledUnknown` with the `*verification.Failure`,
  `errNoPolicy`, or the `Final` error (or a non-`Failure` `Verify` error) wrapped as
  `fmt.Errorf("%w: %w", errSettlementCheck, err)`.
- `exportExitCode` checks in this order: `errOutputExists` → 2; `errFormatUnsupported` → 3;
  `errNotSettled` → 6; `eventstore.ErrRoundNotFound` → 5; `ErrNoCheckpoint`, `ErrCorrupt` → 1;
  `*verification.Failure` → 3 if `Unsupported`, else 1; `errNoPolicy` → 3; `errSettlementCheck` → 1;
  anything else → 4.
- `exportReport(format string, exp eventstore.Export, settled settledVerdict) []byte` renders §6.
- `main.go`: `case "export"` in `runAudit` before the mode switch; `newRegistry()` replaces the inline
  registry; `settlementPolicies()` returns `[]execution.Policy{execution.CrashV1Policy()}`.

Unit tests (`make test`), `export_test.go`:
- **`TestExportUsage`** (`ORVEX_DATABASE_URL` = `unreachableDSN`, so exit 2 proves no connection was
  tried): exit 2, empty stdout, non-empty stderr for:
  - `audit export`; one positional; three positionals; `-unsettled` after the positionals; `-bogus`;
  - `ROUND_ID` uppercase, in braces, `urn:uuid:` prefixed, without hyphens, 35 characters, empty, and the
    nil UUID;
  - `-format ""`, `-format v1`, `-format orvex/audit-history/v01`;
  - `ORVEX_DATABASE_URL` unset, empty, and `::bad` (stderr exactly
    `crashctl: audit export: invalid ORVEX_DATABASE_URL\n`);
  - `audit export -h` and `-help` exit 0 with usage on stderr.
- **`TestExportFormatUnsupported`**: `-format orvex/audit-history/v2` with `unreachableDSN` → 3, `FILE`
  not created.
- **`TestExportFileExists`**: an existing file (content unchanged afterwards), a directory and a dangling
  symlink as `FILE` → 2 with `exists; refusing to overwrite`; the symlink target is not created.
- **`TestExportMissingDirectory`**: `FILE` in a missing directory → 4; stderr names the directory, not a
  connection error.
- **`TestExportConnectError`**: `unreachableDSN` with a path and with `-` → 4, empty stdout, one stderr line
  with the prefix, no `FILE` afterwards.
- **`TestExportNeverPrintsPassword`**: the three DSNs of `TestMigrateNeverPrintsPassword` with
  `sentinel-pw`: exit 2 or 4, and neither stream contains the password.
- **`TestExportConnConfig`**: a plain DSN gives `default_transaction_read_only=on`,
  `application_name=crashctl`, `idle_in_transaction_session_timeout=60000` and a 10 s timeout; a DSN with
  `default_transaction_read_only=off&application_name=x&idle_in_transaction_session_timeout=5000&connect_timeout=3`
  gives `on`, `x`, `5000`, 3 s.
- **`TestExportExitCodes`**, a table over `exportExitCode`: `ErrRoundNotFound` → 5; wrapped
  `ErrNoCheckpoint`, `ErrCorrupt` → 1; wrapped `*pgconn.PgError` `42501`, `42P01`, `40001`, `08006`,
  `io.ErrUnexpectedEOF` → 4; `*verification.Failure{Unsupported: true}`, `errNoPolicy`,
  `errFormatUnsupported` → 3; `*verification.Failure{}`, `errSettlementCheck` wrapping a `Final` error →
  1; `errNotSettled` → 6; `errOutputExists` → 2.
- **`TestSettlement`** (golden files, no database):
  - worked-example → yes; in-progress → no;
  - worked-example cut at record 9 (header rebuilt with `canonjson.Marshal`: checkpoints 0 and 9 with the
    manifest state hashes, `head` = `chain.LinkHash` of record 9, the golden `configSecret`) → no;
  - `tamper/rules-version-2.jsonl` → unknown with an unsupported `Failure`;
  - `tamper/commitment-edited.jsonl` → unknown with a `fairness` `Failure`;
  - a policy list without crash/v1 → unknown with `errNoPolicy`.
- **`TestSettlementPoliciesCoverRegistry`**: every adapter in `newRegistry()` (look up each descriptor of
  `settlementPolicies()` and `crashv1.Adapter().Descriptor()`) has exactly one policy with an equal
  descriptor.
- **`TestExportReport`**: exact bytes for (settled, `S = N`); (settled, `S < N`, with the note); (`settled
  no`, with note and warning); (`settled unknown`, with the warning).
- `main_test.go` `TestRun`: add `{name: "usage lists export", args: nil, wantCode: 2, wantStderr:
  "crashctl audit export [-unsettled] [-format NAME] ROUND_ID FILE"}`.

### Step 5: integration helpers (`export_helpers_integration_test.go`)

- `seedGolden(t, pool, file string) goldenRound`: reads the golden; decodes the header's `configSecret`
  and the genesis; `execution.New(pool, newRegistry(), cfg /* DefaultConfig with EngineBuild "golden" */,
  execution.CrashV1Policy())`; `CreateRound` with the decoded config and the genesis `createdAtUnixMicro`
  (require `Created.Genesis` equal to golden line 2); funds each player with the sum of their S-10 holds
  (`fundPlayer`, copied from `internal/runtime/admission/helpers_integration_test.go`); seeds every
  command record's admission in order (`seedAdmission`: in one transaction lock the admission head, insert
  `orvex.admissions(round_id, sequence, command_id, effective_time_unix_micro, command)` with the record's
  exact command bytes, insert the S-10 hold into `orvex.ledger_holds`, advance `admitted_sequence`). It
  executes nothing. `goldenRound{roundID, lines, exec, lease /* token 0 */}` with `hash(seq)` =
  `chain.LinkHash(lines[seq+1])`.
- `exportCmd(t, args ...string) (code int, stdout, stderr string)`: runs `run(append([]string{"audit",
  "export"}, args...), …)`.
- `replicaExec(t, pool, sql, args...)`: one transaction with `SET LOCAL session_replication_role =
  replica`, then the statement, committed.
- `restrictedRole(t, dsn string, grants []string) (roleDSN string)`: creates `orvex_export_<n>` LOGIN with
  a password, runs `grants` in the test database, returns the DSN with that user (via `net/url`), and at
  cleanup runs `DROP OWNED BY` in the test database, then `DROP ROLE`.

### Step 6: integration tests (`export_integration_test.go`, `make test-integration`)

Each test uses its own `pgtest.MigratedDatabase` (except where stated), `t.Setenv(databaseURLEnv, dsn)`,
and paths in `t.TempDir()`.

- **`TestExportExecutedRounds`** (acceptance), per §8 row: `seedGolden`, `Drain(ctx, lease, 100)`, then
  `audit export ROUND_ID <path>`:
  - exit as §8; for in-progress, exit 6 with the §6 message naming `sequence 2 of 3`, no file; then
    `-unsettled` → 0;
  - the stdout report exactly as §6 with the §8 `records` line, `settled`, note and warning;
  - the file's mode is `0600`; its header's checkpoint sequences equal §8's snapshot list; its record lines
    equal golden lines `2..S+2`; its bytes equal `eventstore.ExportHistory` output for the round read
    directly from the pool;
  - `audit verify -expect-head <head from §8>` on the file → 0, with `chain ok records <S+1> head <head>`;
  - when `S < N` (worked-example, in-progress): `audit verify -expect-head <manifest head>` → 1 with
    `chain failed at sequence <S>`.
- **`TestExportFollowsSnapshots`**: worked-example via `seedGolden`; for `k = 0` (only `CreateRound`) and
  after each `ExecuteNext` up to `k = 22`, with `S_k` the largest of {0, 9, 19, 21} not above `k`:
  - without `-unsettled`: `k < 21` → 6, stderr names `sequence <S_k> of <k>`, no file; `k ≥ 21` → 0;
  - with `-unsettled` (another path): 0; `records <S_k+1> of <k+1> head <hash(S_k)>`; `settled no` below
    21, `yes` from 21; the note exactly when `S_k < k`;
  - `audit verify -expect-head <hash(S_k)>` on the written file → 0.
- **`TestExportStdout`**: worked-example drained. `export ROUND_ID -`: stdout equals the bytes of a path
  export of the same round; stderr is the §6 report; `audit verify -expect-head <hash(21)> -` with those
  bytes on stdin → 0. in-progress drained, `-` without `-unsettled` → 6, stdout empty.
- **`TestExportUnknownRound`**: `LoadHistory(worked-example)`; an unknown UUID → 5, stderr exactly
  `crashctl: audit export: round <uuid> not found\n`, stdout empty, no file.
- **`TestExportNoSnapshot`**: `LoadHistory(worked-example, []uint64{})` → 1, `has no snapshot`, no file.
- **`TestExportCorruptRound`**, two databases with `LoadHistory(worked-example)`:
  1. `replicaExec` deletes the `round_secrets` row → 1, stderr contains `violates an export invariant`, no
     file;
  2. `replicaExec` sets `config_secret` to `{"seed":"<64 × f>"}` → 1, stderr contains `fairness failed at
     sequence 0` and `-unsettled`, no file; with `-unsettled` → 0, `settled unknown` and the warning;
     `audit verify` on that file → 1 at stage `fairness`.
- **`TestExportUnsupportedRules`**: `LoadHistory(tamper/rules-version-2.jsonl)` → 3, stderr contains
  `rules unsupported at sequence 0`; with `-unsettled` → 0, `settled unknown`; `audit verify` on the file →
  3.
- **`TestExportDatabaseErrors`**:
  1. `pgtest.EmptyDatabase` (no schema) → 4, stderr contains `42P01`, no file;
  2. a `restrictedRole` with the §7 grants except `round_secrets`, password `sentinel-pw` → 4, stderr
     contains `42501`; neither stream contains the password; no file.
- **`TestExportMinimalGrants`**: worked-example drained (superuser pool); a `restrictedRole` with exactly
  the §7 grants: the export → 0 and `audit verify -expect-head <hash(21)>` → 0. Through the role's own
  connection, `SELECT state FROM orvex.round_snapshots LIMIT 1` and `SELECT 1 FROM orvex.admissions LIMIT
  1` give `42501`.
- **`TestExportOutputFailures`**: worked-example drained:
  1. `-` with `multilineFailingWriter` as stdout → 4, one stderr line containing
     `write stdout: disk full: sector 7`;
  2. a path with `multilineFailingWriter` as stdout (the report) → 4, one stderr line containing
     `write report`, and the file does not exist afterwards.

### Step 7: docs

- `docs/architecture/persistence.md`, new last section "History export CLI (LBT-19)": usage; one REPEATABLE
  READ READ ONLY transaction and the session parameters; records `0..S` and the `records S+1 of N+1` line;
  the settlement gate and `-unsettled`; the head and its trust caveat; the exit-code table; the X-11 grants
  SQL. §8 grant matrix: a row "auditor (`crashctl audit export`)". §2: the export bullet mentions
  `RoundRecords` and the CLI.
- `README.md`: Status adds LBT-19 with its Linear link; Layout `cmd/crashctl` line reads `… crashctl audit
  verify|replay|chain (LBT-9), crashctl audit export (LBT-19), crashctl migrate up|down|status (LBT-10)`;
  Development adds `crashctl audit export [-unsettled] ROUND_ID FILE: write a settled round's audit history
  file from the database named by ORVEX_DATABASE_URL`.

### Verification for the implementation run

Each must exit 0:

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
docker compose -f deployments/docker/compose.yaml up -d --wait
ORVEX_DATABASE_URL='postgres://orvex:orvex@127.0.0.1:55432/orvex?sslmode=disable' ./bin/crashctl migrate up
docker compose -f deployments/docker/compose.yaml down -v
```

And this smoke check, between the `migrate up` and `down -v` lines, must exit 5 and print one line
`crashctl: audit export: round 0190f3a8-7b3c-7d4e-8f00-000000000001 not found`:

```sh
ORVEX_DATABASE_URL='postgres://orvex:orvex@127.0.0.1:55432/orvex?sslmode=disable' ./bin/crashctl audit export 0190f3a8-7b3c-7d4e-8f00-000000000001 /tmp/lbt19-smoke.jsonl
```

- `git diff --stat` shows no change to `go.mod`, `go.sum`, `migrations/`, `docs/adr/`, golden fixtures,
  `internal/game/`, `internal/canonjson/`, `internal/runtime/` or `internal/persistence/postgres/`.
- If a test contradicts this spec, find which side is wrong. Never weaken an assertion or edit a fixture to
  make a run pass.

## 11. Out of scope and open items

Out of scope:
- in this run: any Go, test, doc, `go.mod` or CI change;
- history format v2 and the ledger stage (LBT-18);
- exporting several rounds per call, multi-round files, cross-round audits;
- any server, API or scheduled exporter;
- creating roles or grants in a deployment (LBT-14);
- external anchoring of heads (ADR-001 future measure);
- encryption of secrets at rest (P-25).

Open for the engineer:
1. The settlement gate with the `-unsettled` override (X-7, §2 items 1 to 3), instead of no gate or an
   absolute refusal.
2. Exit codes 5 and 6, and database errors as 4 (X-9, §2 item 4).
3. The additive library changes `eventstore.Export.RoundRecords` and `verification.Report.State` (§2 items
   5 and 6).
4. `-format` now, with v1 as the only and default value (X-10).
5. The auditor role reads every round's secret (X-11, §2 item 9). A row-level restriction for unsettled
   rounds needs a view or RLS; follow-up with LBT-14.
6. Follow-up: a verifier option such as `-expect-record SEQ:HEX`, so a head recorded at one export can be
   checked against a longer later export (ADR-001 anchoring, §2 item 10).
7. Follow-up: a shared test-support package for seeding golden rounds through the executor, used by LBT-12
   and LBT-19 tests instead of two copies.
8. LBT-9 open item 3 stays open: a public, secret-less export of unsettled rounds would be a new format.
