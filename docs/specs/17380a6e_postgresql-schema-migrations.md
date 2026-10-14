# PostgreSQL schema and migration tooling (LBT-10)

Status: **APPROVED** by the engineer on 2026-10-14: decisions P-1 to P-26 as written, with no edits,
and every point of section 2 accepted. Implementation (section 11) follows under LBT-10.

- Linear: [LBT-10](https://linear.app/lbtworks/issue/LBT-10/implement-postgresql-schema-and-migration-tooling),
  milestone M2 · Durable Execution. Blocked by LBT-9; blocks LBT-11 and LBT-12.
- Linear text: "Create migrations for rounds, admissions, command results, domain events, ledger, outbox,
  ownership/fencing and snapshot-ready metadata. Document constraints and transaction isolation.
  Acceptance: clean up/down migration, real Postgres Testcontainers integration. Reference ADR-005."
- Governing ADRs: ADR-005 (primary), ADR-001, ADR-002, ADR-006, ADR-007, plus ADR-003 and ADR-004
  (mirrors in `docs/adr/`).
- Approved inputs:
  - "LBT-9 spec": the approved canonical codec, hash chain and audit spec (decisions A-1 to A-20). It is
    not on `main` yet. The copy read for this plan is outside the repo and is not copied in.
  - "Math spec": `docs/specs/e96d124d_crash-v1-math-spec.md` (D-1 to D-14).
  - "Lifecycle spec": `docs/specs/712614a2_crash-lifecycle-rules.md`.

Decision IDs are `P-n`, so they do not clash with the math spec's `D-n` or the LBT-9 spec's `A-n`.

## 0. What this run delivers

- This document is the deliverable. The engineer approves its copy under `docs/specs/`.
- **Builder, this run: make no repository changes.** Do not write Go code, SQL, migrations, Compose
  files, or CI changes, and do not touch `go.mod`, `go.sum`, `Makefile`, `.golangci.yml`, `README.md`,
  `AGENTS.md` or `docs/adr/`. The gates must still pass on the unchanged tree: `make test`, `make lint`,
  `go vet ./...`, `make build`, `make golden-check`.
- After approval, section 11 is the implementation plan.
- **Implementation precondition: LBT-9 is merged on `main` and the worktree is based on it.** Check that
  `internal/canonjson/`, `internal/audit/chain/`, `internal/audit/verification/` and
  `internal/audit/verification/testdata/golden/worked-example.jsonl` exist. If they do not, stop and
  report "blocked on LBT-9". Do not copy, vendor or re-implement LBT-9 code or fixtures. If LBT-9's
  merged API names differ from the LBT-9 spec, use the merged names. This spec fixes behavior, not
  identifiers.

## 1. Current state (verified while planning)

- `main` is at `4da99af`. Toolchain: go1.26.5. Docker Engine 29.8.2 (API 1.55) runs on the planning
  host. `go.mod` requires only `github.com/cockroachdb/apd/v3 v3.2.3`.
- There is no persistence code, no `migrations/`, no `deployments/` and no `docs/architecture/` yet.
  `Makefile` says `test-integration` will be added "when meaningful". CI has `test`, `lint` and `build`
  jobs.
- `cmd/crashctl` is a scaffold. LBT-9 turns it into `crashctl audit {verify|replay|chain}`, with
  `run(args, stdin, stdout, stderr) int` and exit codes 0 ok, 1 failed, 2 usage, 3 unsupported, 4 I/O.
- What LBT-9 requires of LBT-10 (LBT-9 spec section 11):
  - one immutable row per chain record holding the exact OCJ bytes, the 32-byte link hash and the
    previous hash, keyed `(round_id, sequence)`, with sequence 0 as the genesis record;
  - never `jsonb` for canonical bytes;
  - derived query columns are never the source for hashing or export;
  - the export copies record bytes verbatim in sequence order;
  - the seed is stored outside record bytes and joined into the export header's `configSecret`;
  - snapshot state hashes come from the engine at commit time.
- Module versions available (`go list -m -versions`): `github.com/jackc/pgx/v5` up to **v5.11.0**;
  `github.com/testcontainers/testcontainers-go` up to **v0.44.0**; goose v3.29.0; golang-migrate
  v4.20.1; tern v2.4.3.
- Dependency weight of the candidate migration tools, measured from their `go.mod` files:
  - goose v3.29.0 requires drivers for ClickHouse, MySQL, MSSQL, YDB, libsql, Vertica and SQLite;
  - golang-migrate v4.20.1 requires cloud SDKs (Spanner, GCS, AWS) and a dozen database drivers;
  - tern v2.4.3 requires sprig and cobra.
  None of the three verifies checksums of applied migrations.
- PostgreSQL images on Docker Hub: the latest stable release is **18.6**; 19 is at beta4.
  - `postgres:18.6-trixie` has the multi-arch index digest
    `sha256:74935e72241653ca55e0414067e6d8763aceb8a810eb51b452253ec3dcfc4336` (amd64, arm64 and six
    more platforms).
- Prototypes on PostgreSQL 18.6 in throwaway containers, outside the repo:
  - `sha256(bytea)` is `IMMUTABLE`, so it is allowed in a `CHECK`. The SQL expression
    `sha256('orvex/audit/v1/record:'::bytea || record)` reproduces the LBT-9 §5.6 `hash_0`
    `cfdd0537…ecc284` from the worked-example genesis bytes.
  - These constructs behaved as specified in section 5:
    - the hash `CHECK`;
    - the previous-record foreign key built from a `STORED` generated column;
    - deferred head foreign keys;
    - the deferred fencing trigger;
    - forward-only head triggers;
    - append-only and `TRUNCATE` triggers with custom SQLSTATEs;
    - the `pg_trigger_depth()` guard on ledger totals;
    - a partial `NULLS NOT DISTINCT` unique index.
  - Found while prototyping: a table-level `UNIQUE NULLS NOT DISTINCT (round_id, name)` also makes two
    player accounts collide on `(NULL, NULL)`. Section 5.6 therefore uses a **partial** unique index.
  - The record-binding byte templates of §5.3 match the LBT-9 §5.6 genesis and record 1 bytes exactly.
  - `EXTRACT(EPOCH FROM timestamptz)` returns `numeric`. `(EXTRACT(EPOCH FROM ts) * 1000000)::bigint`
    is exact: `2025-10-09 08:53:20.123457+00` gives `1760000000123457`.
  - `default_transaction_isolation` is `read committed`.
- pgx v5.11.0 `Conn.Exec` with no arguments always uses the simple protocol (`conn.go:524`), so a
  multi-statement migration file runs in one `Exec`.
- testcontainers-go v0.44.0 (core module) with pgx v5.11.0 started the pinned image and ran queries in
  1.9 s on Docker 29.8.2 (Ryuk 0.14.0). That scratch module had 55 requirements and 152 `go.sum` lines.

## 2. ADR alignment, conflicts and flagged interpretations

No decision contradicts an accepted ADR, a math-spec decision (D-1 to D-14) or an LBT-9 decision
(A-1 to A-20). These points need the engineer's explicit attention (AGENTS.md "Flag conflicts
explicitly"). Copy them into the implementation PR.

1. **ADR-007 layout and AGENTS.md "shared code lives in `internal/`" (additions, need approval).**
   - `migrations/migrations.go` is a one-file Go package holding only `//go:embed *.sql`. Embedding
     needs a Go file in or above the SQL directory. ADR-007 puts `migrations/` at the root, so the SQL
     stays there.
   - `internal/persistence/postgres/pgtest/` is a new test-support subpackage.
   - `docs/architecture/` and `deployments/docker/` are in the ADR-007 layout but do not exist yet.
   - `.gitattributes` is a new root file (P-4).
   - Alternatives are in P-10.
2. **ADR-007 `tests/integration/` (interpretation).** PostgreSQL integration tests live next to the
   package they test, behind the `integration` build tag. `tests/integration/` stays reserved for
   multi-process tests (LBT-13 onwards).
3. **ADR-007 "replay/audit CLI" (extension).** `crashctl` gains `migrate` (P-6), which makes it the
   operator CLI.
4. **ADR-005 "expected version … advance processed/state versions" (interpretation).**
   - The round's optimistic version is `round_execution_heads.processed_sequence`. LBT-9 A-7 writes
     exactly one chain record per executed command, so a separate counter would be redundant.
   - This differs from ADR-003's `stateVersion` (the shape of the state), stored as
     `rounds.state_version`.
5. **ADR-005 "append-only domain events" and "terminal command results" (interpretation).**
   - The authoritative event and result bytes live inside `chain_records.record` (LBT-9 A-7, A-8).
   - `domain_events` is a derived index without a payload copy (P-18).
   - Results are columns that a trigger binds to the hashed bytes, plus a `command_results` view (P-17).
6. **ADR-005 open validation "deferred ledger balance enforcement" (resolved differently).** Each
   posting row moves one amount from one account to another, so every ledger transaction balances by
   construction. No deferred check is needed (P-21).
7. **ADR-005 open validations not closed here:**
   - backup/restore tests (LBT-14 or LBT-15);
   - the outbox relay and its retry loop (LBT-13 or later). LBT-10 provides only the retry columns.
8. **ADR-001 trade-off (limitation).** Triggers, the hash `CHECK` and the binding checks protect against
   application bugs and non-owner roles. They do not protect against the table owner or a superuser,
   who can disable triggers or rewrite rows with recomputed hashes. ADR-001's separately trusted anchor
   stays future work.
9. **ADR-002 (alignment).**
   - The effective time is stored as `bigint` microseconds, the `game.Input` type.
   - LBT-11 converts `clock_timestamp()` exactly (measured, §1).
   - A globally unique `command_id` serves ADR-002's "unknown commit outcomes": a retry finds the
     original admission.
10. **LBT-9 A-8 (refinement).** `command_id` is unique across all rounds and must not be the nil UUID.
    LBT-9 only requires uniqueness within a round, so this is stronger.
11. **LBT-9 A-11 and §11 (interpretation).** v1 requires the last checkpoint to be at the file's last
    record. An export therefore stops at the round's newest snapshot (P-24). Records after it appear
    once a later snapshot exists.
12. **LBT-9 A-4 and lifecycle spec item 10 (constraints on writers).**
    - `domain_events.event_type` is the event object's `type` member. crash/v1 has one on every event
      (A-4).
    - PostgreSQL `text` cannot hold U+0000, so `ledger_accounts.player_id` cannot store a player ID that
      contains it. LBT-11 and LBT-13 must reject such IDs at admission.
13. **Math spec §10 item 3 and ADR-004 fairness (open).** The secret config (the seed) is stored
    unencrypted in its own table (P-25). Encryption at rest is deferred.
14. **LBT-9 A-20 and AGENTS.md immutability.** The chain v1 hash tag and record layout are written into
    database constraints (P-16). A chain v2 needs a migration. v1 is frozen anyway.
15. **AGENTS.md dependencies.** P-2 and P-7 each add and justify one module. The migration tool adds
    none (P-3).

| ADR clause | Where satisfied |
|---|---|
| ADR-005: PostgreSQL is the system of record for rounds, admissions, results, events, ledger, outbox, snapshots | P-14 to P-22 |
| ADR-005 Transaction A: per-round serialization, dedupe, sequence, effective time, immutable admission | P-14, P-15, P-23 |
| ADR-005 Transaction B: expected version, next processed sequence, fencing token; events, one result, balanced postings, outbox | P-16, P-17, P-19, P-21, P-22, P-23 |
| ADR-005 failure semantics: no partial outcome; stale owner cannot commit; at-least-once outbox | P-19, P-22, P-23 |
| ADR-005 cross-round balances: account-level transactional debit/reservation checks | P-21, P-23 |
| ADR-001: immutable ordered history; per-round hash chains; preserve commands, effective times, versions, commitments, checkpoints, ledger references | P-13, P-15, P-16, P-20, P-21 |
| ADR-002: durable sequence and effective time; same serialization boundary for admission and closure | P-14, P-15, P-23 |
| ADR-006: real PostgreSQL integration tests with Testcontainers for Go; PR CI; critical failure tests | P-7, P-8, section 10 |
| ADR-007: `migrations/`, `internal/persistence/{postgres,eventstore}`, `make test-integration`, Docker Compose, pinned dependencies | P-1 to P-10 |
| LBT-9 §11: record rows, verbatim export, seed outside records, engine-computed snapshot hashes | P-16, P-20, P-24, P-25 |

## 3. Decision summary

Tick a box to approve, or edit the value. The implementation run uses approved values only.

| ID | Decision | Proposed value | Approve |
|---|---|---|---|
| P-1 | PostgreSQL version | PostgreSQL 18; image `postgres:18.6-trixie@sha256:74935e72…dcfc4336`; no extensions; UTF8 required | [x] |
| P-2 | Driver | `github.com/jackc/pgx/v5 v5.11.0`, native API (`pgx`, `pgxpool`), no `database/sql` | [x] |
| P-3 | Migration tool | In-repo runner in `internal/persistence/postgres`; SQL embedded from `migrations/`; no migration dependency | [x] |
| P-4 | Migration files and runner semantics | `NNNN_name.{up,down}.sql`, contiguous from 0001; one transaction per file under advisory lock `7766444933939532367`; SHA-256 checksums of both files; bookkeeping in `orvex_migrate.schema_migrations` | [x] |
| P-5 | Up/down policy | Every migration has an exact down; a down refuses (SQLSTATE `OX003`) when a table it drops holds rows; merged migrations never change | [x] |
| P-6 | Migration CLI | `crashctl migrate up [-to N] \| down -to N \| status`; database from `ORVEX_DATABASE_URL`; exit 0/1/2/3/4 | [x] |
| P-7 | Test harness | `github.com/testcontainers/testcontainers-go v0.44.0` core module (test-only); helper package `pgtest` | [x] |
| P-8 | How integration tests run | `//go:build integration`; `make test-integration`; **not** part of `make test`; new CI job `integration` on every PR; Docker required, and a missing Docker fails the run | [x] |
| P-9 | Local Compose | `deployments/docker/compose.yaml` with one `postgres` service on `127.0.0.1:55432`, same pinned image | [x] |
| P-10 | Layout | `migrations/`, `internal/persistence/postgres` (+ `pgtest`), `internal/persistence/eventstore`, `docs/architecture/persistence.md` | [x] |
| P-11 | Schema conventions | Schema `orvex`; schema-qualified SQL; explicit constraint names; SQLSTATE class `OX`; `COMMENT ON` every table and view | [x] |
| P-12 | Value types | `uuid` IDs (nil rejected); `bigint` µs for times that enter the game or the chain; `timestamptz` for operational times; `bigint` sequences; `bigint` uint32 versions with a range check; `bytea` canonical bytes and hashes; never `jsonb` | [x] |
| P-13 | Append-only enforcement | Shared trigger rejects `UPDATE`, `DELETE`, `TRUNCATE` (`OX001`) on immutable tables | [x] |
| P-14 | Round tables | `rounds` and `round_secrets` (immutable); `round_admission_heads` (Transaction A lock row) and `round_execution_heads` (Transaction B compare-and-set row) kept separate | [x] |
| P-15 | Admissions | `admissions` keyed `(round_id, sequence)`, gapless from 1 by foreign key, globally unique `command_id`, canonical command bytes | [x] |
| P-16 | Chain records | `chain_records`: exact OCJ `bytea`; `CHECK hash = sha256(tag ‖ record)`; previous-record foreign key on `(round_id, sequence-1, prev_hash)`; genesis at 0 with zero `prev_hash`; byte-template binding of columns to bytes (`OX008`) | [x] |
| P-17 | Command results | `result_status`/`result_reason` columns on command records plus view `command_results` | [x] |
| P-18 | Domain events | `domain_events` derived index `(round_id, event_sequence)`, contiguous by foreign key; no payload copy | [x] |
| P-19 | Ownership and fencing | `round_execution_heads(processed_sequence, fencing_token, owner_id, lease_expires_at)`; forward-only (`OX005`); records carry `fencing_token`; a deferred check at commit requires the current token (`OX004`) | [x] |
| P-20 | Snapshots | `round_snapshots(round_id, sequence, state_hash, state?, engine_build)` referencing a chain record | [x] |
| P-21 | Ledger | Accounts with trigger-maintained `debits_posted`/`credits_posted`/`held` and a floor `CHECK`; two-sided postings (balanced by construction); one ledger transaction per chain record; holds and releases for cross-round reservation | [x] |
| P-22 | Outbox | `outbox_messages` with dedupe key `(round_id, sequence, message_index)`; only the delivery columns are mutable (`OX006`); published rows may be deleted | [x] |
| P-23 | Transaction isolation | A: READ COMMITTED + round row lock; B: READ COMMITTED + compare-and-set + constraint-checked writes; export: REPEATABLE READ READ ONLY; retry only `40001`/`40P01` | [x] |
| P-24 | History export | `eventstore.ExportHistory`: one REPEATABLE READ snapshot; header from `round_snapshots`, `round_secrets` and the head hash; records `0..S` verbatim, where `S` is the newest snapshot; no CLI yet | [x] |
| P-25 | Seed secrecy at rest | Codec secret config bytes in `round_secrets`, unencrypted; access by grants (LBT-14); encryption deferred | [x] |
| P-26 | Retention | Keep every audit row; only published outbox rows may be deleted | [x] |

## 4. Decisions

### P-1. PostgreSQL 18, pinned image, no extensions

**Proposal.**
- Target PostgreSQL 18. Tests and Compose use exactly
  `postgres:18.6-trixie@sha256:74935e72241653ca55e0414067e6d8763aceb8a810eb51b452253ec3dcfc4336`.
- That string is the constant `pgtest.Image`, and the Compose file repeats it. A unit test keeps the
  two equal (§10).
- No extensions: `uuid` and `sha256()` are built in.
- Migration 0001 fails with `OX007` unless `server_encoding` is `UTF8`.

**Rationale.**
- 18 is the newest stable major (§1), which gives the longest support window.
- The schema needs `sha256()` (PG 11+), `STORED` generated columns (12+), exact `numeric` from
  `EXTRACT` (14+) and `NULLS NOT DISTINCT` (15+).
- PostgreSQL 18 makes generated columns `VIRTUAL` by default. Every generated column here is a foreign
  key or index column, so the spec writes `STORED` explicitly.
- Debian `trixie` (glibc) matches common production images. Alpine's musl collations differ.
- The digest pins the exact multi-arch artifact.
- UTF8 is required because `text` columns (player IDs, reasons) must hold the same strings the codec
  validates as UTF-8.

**Alternatives.**
- 17.x: mature and supported to 2029, but gains nothing over 18.
- 19 beta: not released.
- A tag without a digest: the image can change under the same tag.
- Alpine: smaller, but with different collation behavior.
- `pgcrypto` for hashing: an extension for something built in.

LBT-14 confirms availability on the chosen hosting.

### P-2. Driver: pgx v5.11.0, native API

**Proposal.**
- Add `github.com/jackc/pgx/v5 v5.11.0`, the latest release.
- Use the `pgx`, `pgxpool` and `pgconn` packages directly.
- Go types: `uuid` ↔ `[16]byte`, `bytea` ↔ `[]byte`, `bigint` ↔ `int64`, `timestamptz` ↔ `time.Time`.
  The prototype verified the `[16]byte` and `[]byte` mappings.

**Rationale.**
- It is the maintained native Go PostgreSQL driver: typed `*pgconn.PgError` with SQLSTATE and
  constraint name, transaction options, batches and `COPY`.
- Its no-argument `Exec` runs a multi-statement migration file in one round trip (§1).
- Its own requirements are small: `pgpassfile`, `pgservicefile`, `puddle/v2`, `x/sync` and
  `x/text`.

**Alternatives.**
- `database/sql` with `pgx/v5/stdlib`: loses `PgError` ergonomics and `pgx.TxOptions`. It is only
  needed by `database/sql`-based tools such as goose.
- `lib/pq`: in maintenance mode.
- Generated query layers (sqlc, ORMs): not needed for a schema issue. LBT-11 and LBT-12 may revisit.

### P-3. Migration tool: in-repo runner

**Proposal.** About 250 lines in `internal/persistence/postgres/migrate.go`:
- it loads the embedded SQL (`migrations.FS`);
- it applies or reverts one file per transaction under an advisory lock;
- it records SHA-256 checksums and refuses a database whose applied files differ from this build's.

Section 8 specifies the behavior.

**Rationale.**
- AGENTS.md: "Keep dependencies minimal". The runner adds no module.
- AGENTS.md treats released artifacts as immutable. The runner enforces that for migrations with
  checksums, which goose, golang-migrate and tern lack (§1).
- PostgreSQL DDL is transactional, so "one file, one transaction" leaves no half-applied state. The
  runner needs no "dirty" flag.

**Alternatives.**

| Option | Verdict |
|---|---|
| goose v3.29.0 | Mature, Go-embeddable. Its `go.mod` pulls in about ten database drivers' requirements, it needs `database/sql`, and it has no checksums. |
| golang-migrate v4.20.1 | The heaviest module graph (cloud SDKs). Its "dirty version" recovery model is unnecessary with transactional DDL. No checksums. |
| tern v2.4.3 | pgx-native and the closest fit, but its `migrate` package brings sprig templating. No checksums. |
| Atlas | Declarative and has checksums (`atlas.sum`). It is an external binary with its own release channel, which is more tooling than one schema needs. |
| Flyway or sqitch | Checksums, but a Java or Perl toolchain in a Go-first repo (ADR-007). |

### P-4. Migration files and runner semantics

**Proposal.**
- Files are `migrations/NNNN_name.up.sql` and `migrations/NNNN_name.down.sql`, matching
  `^([0-9]{4})_([a-z][a-z0-9_]*)\.(up|down)\.sql$`.
  - Versions are contiguous from `0001`, and each version has exactly one name and both files.
  - Any other `.sql` file is an error.
- `migrations/migrations.go` declares `package migrations` with `//go:embed *.sql` and `var FS embed.FS`.
- The runner rejects a file that:
  - is empty;
  - contains a transaction statement on its own line (`BEGIN;`, `COMMIT;`, `ROLLBACK;`,
    `START TRANSACTION`), matched case-insensitively. PL/pgSQL `BEGIN` without a semicolon is allowed;
  - uses `CONCURRENTLY`;
  - starts a line with a psql meta-command (`\`).
- Bookkeeping is created by the runner, not by a migration:
  `orvex_migrate.schema_migrations(version integer PK CHECK (version >= 1), name text NOT NULL,
  up_sha256 bytea NOT NULL, down_sha256 bytea NOT NULL, applied_at timestamptz NOT NULL DEFAULT now(),
  applied_by text NOT NULL)`. Both digest columns have `CHECK (octet_length(…) = 32)`, and `applied_by`
  holds `buildinfo.Version`.
- Each step is one READ COMMITTED transaction:
  1. `pg_advisory_xact_lock(7766444933939532367)`. The key is the first 8 bytes of
     `SHA-256("orvex/migrate")` as a big-endian `int64`.
  2. Create the bookkeeping schema and table if absent.
  3. Read and validate the bookkeeping rows.
  4. Run one file.
  5. Write or delete the bookkeeping row.
  6. Commit.
- Add `.gitattributes` with `/migrations/*.sql text eol=lf`, so a CRLF checkout cannot change the
  checksums.

**Rationale.**
- Sequential numbers order migrations totally. If two branches both add `0007`, the loader rejects the
  duplicate at test time and the later branch renumbers.
- The lock serializes concurrent runners: two engine replicas or a CI retry.
- Taking the lock before `CREATE … IF NOT EXISTS` makes bookkeeping creation race-free.

**Alternatives.**
- Timestamp-named files: fewer branch collisions, but versions can apply out of order.
- Checksumming only the up file: a changed down file would then go unnoticed until it runs.
- A session-level advisory lock held for the whole run: a crash mid-run then needs the session to end
  before another runner proceeds. The transaction lock releases at every step.

### P-5. Up/down policy

**Proposal.**
- Every migration has a down file that reverts its up file exactly. After a full down, the catalog
  fingerprint equals the pristine database's, excluding `orvex_migrate` (§10, `TestMigrateUpDownClean`).
- A down file first runs a `DO` block that raises SQLSTATE `OX003` (`down migration refused: <table>
  holds rows`) if any table it drops has a row.
  - Down files are for development and tests. On a database holding audit data they fail and change
    nothing.
  - Future downs that drop columns follow the same rule.
- A merged migration is never edited, even before the first release. Schema changes go into a new
  migration. Checksums enforce this (P-4).
- `crashctl migrate down` needs an explicit `-to N`.

**Rationale.**
- The Linear acceptance asks for a clean up/down.
- ADR-001 makes the event history authoritative. A down that could silently drop `chain_records` would
  contradict it. Refusing on data keeps downs useful in tests and harmless in production.
- Immutable migrations keep every database's history equal to the repository's.

**Alternatives.**
- Forward-only migrations: contradicts the acceptance.
- Down with a `-force` flag: one flag away from destroying audit history.
- Allowing pre-release edits with a "reset": developer databases drift silently.

### P-6. Migration CLI: `crashctl migrate`

**Proposal.**

```
usage: crashctl migrate up [-to N]
       crashctl migrate down -to N
       crashctl migrate status
ORVEX_DATABASE_URL names the database (a PostgreSQL URL or key=value connection string).
```

- `up` applies migrations up to `N` (default: the newest in this build). `down` reverts to `N`
  (`0 ≤ N`). `status` lists every known migration.
- Output goes to stdout:
  - one `applied NNNN name` or `reverted NNNN name` line per step;
  - for `status`, `NNNN name applied <RFC 3339 UTC> by <build>` or `NNNN name pending` per migration;
  - a final `schema version N`.
- Failure prints one stderr line, `crashctl: migrate <sub>: <error>`.
  - The DSN and password are never printed.
  - An unparsable `ORVEX_DATABASE_URL` prints only `invalid ORVEX_DATABASE_URL`, not the pgx error.
- The connection uses a 10 s connect timeout unless the DSN sets `connect_timeout`.
- Exit codes follow LBT-9's convention:

| Code | Meaning |
|---|---|
| 0 | done (including "already at target") |
| 1 | a migration failed: SQL error, `OX003` refusal, `OX007` encoding |
| 2 | usage: unknown subcommand, extra arguments, `down` without `-to`, `-to` not a non-negative integer or above the newest version, `ORVEX_DATABASE_URL` unset or unparsable, target on the wrong side of the current version |
| 3 | unsupported: database schema newer than this build, or an applied migration's checksum differs (fail closed, ADR-006) |
| 4 | I/O: cannot connect, or cannot write stdout |

`main.go` stays thin. Argument parsing and exit-code mapping live in `cmd/crashctl/migrate.go`, and
the work happens in `internal/persistence/postgres`.

**Rationale.**
- One operator binary already exists. Kubernetes (LBT-14) can run `crashctl migrate up` as a Job.
- An environment variable keeps the password out of process listings and shell history.

**Alternatives.**
- A fourth binary `cmd/migrate`: a layout addition for about 80 lines.
- `engine` migrating at startup: implicit schema changes on deploy, every replica races for the lock,
  and down is impossible.
- No CLI, with the library used only in tests: "migration tooling" would then have no operator entry
  point.
- An external tool binary: one more pinned tool to install.

### P-7. Test harness: testcontainers-go v0.44.0 core

**Proposal.**
- Add `github.com/testcontainers/testcontainers-go v0.44.0`, imported only by
  `internal/persistence/postgres/pgtest` and test files.
- Use `testcontainers.Run` with:
  - the pinned image;
  - user, password and database `orvex`;
  - command `postgres -c fsync=off`;
  - a tmpfs at `/var/lib/postgresql`;
  - wait strategy: the log line "database system is ready to accept connections" twice, plus port
    5432 listening.
- Do not add the `modules/postgres` submodule.

`pgtest` API (names may be refined, behavior may not):

```go
const Image = "postgres:18.6-trixie@sha256:74935e72241653ca55e0414067e6d8763aceb8a810eb51b452253ec3dcfc4336"

// Run starts one container for the test binary, runs m, terminates the
// container and returns the exit code. Use: func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }.
// Without a reachable Docker daemon it prints the reason and returns 1.
func Run(m *testing.M) int

func EmptyDatabase(t testing.TB) (dsn string)    // fresh database, dropped WITH (FORCE) at cleanup
func MigratedDatabase(t testing.TB) (dsn string) // clone of a template migrated once to the newest version
func Pool(t testing.TB, dsn string) *pgxpool.Pool // MaxConns 8, closed at cleanup

// Fingerprint returns a sorted, line-per-object description of every schema,
// relation, column (type, nullability, default, generated, identity),
// constraint (pg_get_constraintdef, deferrability), index (pg_get_indexdef),
// non-internal trigger (pg_get_triggerdef), function (md5 of pg_get_functiondef),
// view (md5 of pg_get_viewdef), user type and comment outside pg_catalog,
// information_schema, pg_toast and the excluded schemas.
func Fingerprint(ctx context.Context, db Querier, exclude ...string) (string, error)

// LoadHistory inserts an LBT-9 history file into a migrated database exactly as
// round creation, admission and execution would (section 7.3). checkpoints
// selects which of the header's checkpoints become snapshot rows (nil: all).
func LoadHistory(t testing.TB, db *pgxpool.Pool, history []byte, checkpoints []uint64) Loaded

// WaitUntilBlocked polls pg_stat_activity until backend pid waits on a lock.
func WaitUntilBlocked(t testing.TB, db *pgxpool.Pool, pid uint32)
```

**Rationale.**
- ADR-006 names "Testcontainers for Go".
- The core module covers everything needed. The postgres submodule would add `lib/pq` and
  `tlscert` for a convenience wrapper.
- One container per test package and one cloned database per test keep tests isolated and parallel.
  Cloning a migrated template avoids re-running migrations per test.
- `fsync=off` and tmpfs only lose data if the host OS crashes. No LBT-10 test depends on fsync.
  LBT-14 crash tests choose their own settings.

**Alternatives.**
- `modules/postgres`: more dependencies for a wrapper.
- `os/exec` `docker run`: no new module, but contradicts ADR-006's named tool and re-implements
  reaping and waiting.
- One shared container started by `make`: tests would need external setup.
- A transaction-rollback-per-test pattern: cannot test commit-time (deferred) constraints or
  concurrency.

### P-8. How integration tests run

**Proposal.**
- Integration test files are named `*_integration_test.go` and start with `//go:build integration`.
  `pgtest` itself is untagged, so `make test` and `make lint` compile it.
- Makefile:

```make
# PostgreSQL integration tests (ADR-006): Testcontainers, needs a Docker daemon.
test-integration:
	$(GO) test $(GOTESTFLAGS) -tags integration -count=1 ./...
```

- **Not part of `make test`.** `make test` stays hermetic: no Docker, no network.
- Locally:
  - needs a Docker-compatible daemon. Podman or Colima work through `DOCKER_HOST`, and
    `TESTCONTAINERS_RYUK_DISABLED=true` helps where Ryuk cannot run;
  - the first run pulls the pinned image and `testcontainers/ryuk`;
  - on hosts without cgo: `make test-integration GOTESTFLAGS=`.
- Without Docker, the tagged tests **fail** with "integration tests need a Docker-compatible daemon".
  They never skip, so a CI misconfiguration cannot pass silently.
- CI adds a job that runs on every push and pull request (ADR-006 "PR: … PostgreSQL integration").
  GitHub-hosted Ubuntu runners have Docker:

```yaml
  integration:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: actions/setup-go@v7
        with:
          go-version: ${{ env.GO_VERSION }}
          cache-dependency-path: go.mod
      - run: make test-integration
```

- `.golangci.yml` gains `run.build-tags: [integration]`, so `make lint` covers the tagged files.
- AGENTS.md: run `make test-integration` too when touching `migrations/` or `internal/persistence/`.

**Rationale.**
- Unit tests stay fast and runnable anywhere.
- The integration job is required on PRs, as ADR-006 asks.
- `-count=1` stops Go from caching results that depend on an external daemon.
- ADR-007's path-aware CI can later restrict the job.

**Alternatives.**
- Fold into `make test` and skip without Docker: a CI job could skip silently.
- An `ORVEX_INTEGRATION=1` environment gate: same concern, less discoverable than a build tag.
- `tests/integration/` only: black-box only, so it cannot test unexported helpers. See §2 item 2.

### P-9. Local Docker Compose

**Proposal.** `deployments/docker/compose.yaml`:
- project name `orvex`;
- one service `postgres` with the pinned image, `POSTGRES_USER`/`POSTGRES_PASSWORD`/`POSTGRES_DB` set
  to `orvex`, and port `127.0.0.1:55432:5432`;
- a named volume at `/var/lib/postgresql`;
- a healthcheck: `pg_isready -U orvex -d orvex` every 2 s, 30 retries;
- a header comment saying the image must equal `pgtest.Image`.

No Makefile target. README documents:
- `docker compose -f deployments/docker/compose.yaml up -d --wait`;
- `ORVEX_DATABASE_URL='postgres://orvex:orvex@127.0.0.1:55432/orvex?sslmode=disable'`;
- `docker compose -f deployments/docker/compose.yaml down -v`.

**Rationale.**
- ADR-007 names Docker Compose for local dependencies.
- It lets the engineer try `crashctl migrate` and inspect the schema with `psql`.
- Port 55432 on loopback avoids clashing with a local PostgreSQL and is not exposed on the network.

**Alternatives.**
- Defer until `make dev` runs services (LBT-11/13). That is cheaper now, but the CLI then has no
  documented local target.
- Add `make db-up` and `make db-down`: ADR-007 reserves `make dev` for the full stack.

### P-10. Layout

| Path | Holds |
|---|---|
| `migrations/NNNN_name.{up,down}.sql`, `migrations/migrations.go` | SQL; the embed-only package |
| `internal/persistence/postgres/` | migration runner, SQLSTATE constants and helpers, isolation options, package doc |
| `internal/persistence/postgres/pgtest/` | Testcontainers harness, catalog fingerprint, LBT-9 history loader (test support) |
| `internal/persistence/eventstore/` | `ExportHistory` |
| `cmd/crashctl/migrate.go` | CLI subcommand |
| `deployments/docker/compose.yaml` | local PostgreSQL |
| `docs/architecture/persistence.md` | the durable documentation of tables, constraints, isolation and policies |

**Alternatives.**
- SQL under `internal/persistence/postgres/migrations/`: no Go file outside `internal/`, but it leaves
  ADR-007's root `migrations/`.
- Reading SQL from disk at runtime: deployments would have to ship the files, and the binary would no
  longer carry its schema.
- Documentation only as `COMMENT ON`: not reviewable as a document.

### P-11. Schema conventions

**Proposal.**
- Every application object lives in schema `orvex`, created by `0001`. The runner's bookkeeping lives
  in `orvex_migrate`. `public` stays empty.
- All SQL is schema-qualified (`orvex.chain_records`), and Go code never relies on `search_path`.
- Every function is `LANGUAGE plpgsql` and declares `SET search_path = pg_catalog`.
- Constraint names are explicit, of the form `<table>_<what>_{pkey|key|fkey|check}`. Tests assert on
  `PgError.ConstraintName`.
- Custom SQLSTATEs (`RAISE … USING ERRCODE`), class `OX`, exported as Go constants:

| Code | Go constant | Raised when |
|---|---|---|
| `OX001` | `CodeAppendOnly` | `UPDATE`, `DELETE` or `TRUNCATE` on an append-only table, or `DELETE`/`TRUNCATE` on a head or account table |
| `OX002` | `CodeLedgerAccount` | an account is inserted with non-zero totals, or updated other than by a ledger trigger |
| `OX003` | `CodeDownRefused` | a down migration would drop a table holding rows |
| `OX004` | `CodeNotFenced` | at commit, a chain record was not written under the round's current execution head and fencing token |
| `OX005` | `CodeHeadRegression` | an admission or execution head moves backwards, or the owner changes without a new fencing token |
| `OX006` | `CodeOutboxImmutable` | an outbox update touches a non-delivery column, changes a published row, or deletes an unpublished row |
| `OX007` | `CodeEncoding` | migrating a database whose encoding is not UTF8 |
| `OX008` | `CodeRecordBinding` | chain record bytes disagree with its columns, its admission or its round (§5.3) |

- Every table and view gets a one-sentence `COMMENT ON TABLE` or `COMMENT ON VIEW` naming its ADR.

**Rationale.**
- A dedicated schema makes "clean down" checkable (the schema disappears) and keeps grants simple.
- Pinning `search_path` keeps trigger functions from resolving objects in another schema.
- Stable codes and names let LBT-11 and LBT-12 map errors without parsing messages.

**Alternatives.**
- Use `public`: no isolation, and the clean-down check gets harder.
- Standard SQLSTATEs (`23000` family): cannot tell an append-only violation from an ordinary integrity
  error.
- Unnamed constraints: generated names are long and change if columns are reordered.

### P-12. Value representations

| Value | Column type | Rule |
|---|---|---|
| Round ID, command ID (UUID, D-11) | `uuid` | never the nil UUID (lifecycle spec item 1; §2 item 10) |
| Times that enter the game or the chain: admission effective time, round creation time | `bigint` (`*_unix_micro`) | the int64 µs value of `game.Input` and the LBT-9 record, stored verbatim |
| Operational times: leases, `created_at`, outbox delivery, bookkeeping | `timestamptz` | never fed into the game or the chain |
| Sequences (admission, record, event; Go `uint64`) | `bigint` | `CHECK (≥ 0 or ≥ 1)`; values above 2⁶³−1 are unsupported, and writers check before converting |
| Rules, schema and state versions (Go `uint32`, non-zero) | `bigint` | `CHECK (BETWEEN 1 AND 4294967295)` |
| Canonical bytes (records, commands, secret config, state) | `bytea` | exact codec or OCJ bytes; never `jsonb` or `json` (LBT-9 §11) |
| Hashes | `bytea` | `CHECK (octet_length(...) = 32)` |
| Credits | `bigint` | integer simulated credits (D-7); PostgreSQL raises on overflow, which fails closed |

**Rationale.**
- The `bigint` µs columns avoid a second representation and any conversion in LBT-12 or the export.
- The canonical bytes stay untouched, so D-12's text forms live where they belong, in the bytes.

**Alternatives.**
- `timestamptz` for effective times: lossless in its range and readable in `psql`, but it needs a
  conversion on every read and allows timezone-dependent formatting mistakes.
- `numeric(20)` for `uint64` sequences: wider than needed and slower.
- `text` UUIDs: 36 bytes instead of 16, and no type check.

### P-13. Append-only enforcement

**Proposal.**
- `0001` creates `orvex.forbid_mutation()`. It raises `OX001` (`orvex.<table> is append-only: <op>
  rejected`).
- Every append-only table gets two triggers:
  - `<table>_append_only`: `BEFORE UPDATE OR DELETE … FOR EACH ROW`;
  - `<table>_no_truncate`: `BEFORE TRUNCATE … FOR EACH STATEMENT`.
- Append-only tables: `rounds`, `round_secrets`, `admissions`, `chain_records`, `domain_events`,
  `round_snapshots`, `ledger_transactions`, `ledger_postings`, `ledger_holds`, `ledger_hold_releases`.
- Mutable tables with guards:
  - `round_admission_heads` and `round_execution_heads`: forward-only `UPDATE` (`OX005`); `DELETE` and
    `TRUNCATE` raise `OX001`;
  - `ledger_accounts`: totals change only through ledger triggers (`OX002`); `DELETE` and `TRUNCATE`
    raise `OX001`;
  - `outbox_messages`: delivery columns only (`OX006`); `TRUNCATE` raises `OX001`.
- Migrations create no roles and grant nothing. LBT-14 owns roles and grants. Recommended grant
  matrix, recorded in `docs/architecture/persistence.md`:
  - the application role gets `SELECT` and `INSERT` only on append-only tables, and `UPDATE` only on
    heads, accounts and outbox;
  - a gateway role (if it admits commands) gets no `SELECT` on `round_secrets` or
    `round_snapshots.state`.

**Rationale.**
- Triggers enforce immutability for every role except owners and superusers, independent of
  application code.
- `DROP TABLE` does not fire them, so down migrations still work (with the `OX003` guard).

**Alternatives.**
- Privileges only (`REVOKE UPDATE, DELETE`): depends on deployment roles, and the owner role used in
  tests could still mutate rows.
- `ENABLE ALWAYS` triggers: also fire under `session_replication_role = replica`, but only superusers
  can set that anyway. Worth revisiting with LBT-14 replication.
- Rules (`CREATE RULE … DO INSTEAD NOTHING`): silently ignore writes instead of failing.

### P-14. Round tables

**Proposal.** Four tables (normative definitions in §5.2):
- `rounds`: immutable identity and the registry key (game, rules, schema and state versions), chain
  version and creation time. These are derived from the genesis record, for routing and recovery
  without parsing.
- `round_secrets`: the codec's secret config bytes (P-25).
- `round_admission_heads`: `admitted_sequence`. It is Transaction A's per-round lock row (ADR-002
  row-lock sequencing). LBT-11 adds its admission-window and closure-barrier columns in its own
  migration.
- `round_execution_heads`: `processed_sequence` plus ownership and fencing (P-19). It is Transaction
  B's compare-and-set row.

Consistency between heads and rows:
- deferred foreign keys make each head point to an existing row: the last admission and the last
  processed record;
- the round-creation transaction must therefore insert the genesis record, or commit fails.

**Rationale.**
- With one round row, every admission (`FOR UPDATE`) and every execution (`UPDATE`) would serialize
  on the same tuple, doubling hot-round contention (ADR-002, ADR-006 "one hot round"). Separate rows
  let Transactions A and B proceed in parallel. A still serializes with closure, and B with ownership
  changes.
- Separate rows also separate ownership of the logic: LBT-11 owns the admission head, LBT-12 and LBT-14
  own the execution head.
- `rounds` stays immutable, so its rows are never rewritten.

**Alternatives.**
- One mutable `rounds` row holding all heads: simpler, but contended, and a hot row that every write
  rewrites.
- A PostgreSQL `SEQUENCE` per round: not gapless on rollback, which LBT-9 A-8 rejects.
- No counters (`max(sequence) + 1` under the round lock): works, but LBT-9 §11 asks for a row-lock
  counter, and the counter gives LBT-11 an O(1) lock target.

### P-15. Admissions

**Proposal.** `admissions(round_id, sequence, command_id, effective_time_unix_micro, command)`:
- primary key `(round_id, sequence)`;
- sequences gapless from 1, enforced by a foreign key from `(round_id, sequence-1)` to the previous
  admission;
- `command_id` unique across all rounds;
- `command` holds the codec's canonical command bytes (LBT-9 §11).

Admission rows are immutable. Rejections at admission (window closed, insufficient credits) create no
row. Whether to keep evidence of them (ADR-002 "rejection evidence") is LBT-11's call, in its own table.

**Rationale.**
- The database itself guarantees gapless sequences, so a gap in admissions is impossible rather than
  merely detected.
- A global unique index gives LBT-11 its retry lookup by command ID alone and catches a client reusing
  an ID in another round.

**Alternatives.**
- Uniqueness per round `(round_id, command_id)`: matches LBT-9's minimum, but cross-round reuse goes
  undetected.
- A `command_id` primary key: the natural key `(round_id, sequence)` is what every other table
  references.

### P-16. Chain records: storing LBT-9 records

**Proposal.** `chain_records` holds one row per LBT-9 record: genesis at `sequence = 0`, command
records at `1..N`. §5.3 has the definition.
- `record bytea`: the exact OCJ line as hashed, without the trailing `\n`.
  - `CHECK` that it is at most 64 MiB (A-11) and contains no `0x0A` byte, so every row is one valid
    export line.
- `hash bytea`: `CHECK (hash = sha256('orvex/audit/v1/record:'::bytea || record))`. PostgreSQL
  recomputes the A-9 link hash on every insert, as a second SHA-256 implementation next to the
  engine's.
- `prev_hash bytea`:
  - `CHECK` that the genesis value is 32 zero bytes;
  - for `sequence ≥ 1`, foreign key `(round_id, prev_sequence, prev_hash)` →
    `(round_id, sequence, hash)`, where `prev_sequence` is a `STORED` generated column
    `NULLIF(sequence, 0) - 1`;
  - with the primary key `(round_id, sequence)` this makes a fork, a gap or a broken link impossible.
- A command record references its admission: foreign key `(round_id, admission_sequence)` →
  `admissions`.
- Binding (`OX008`, §5.3):
  - a `BEFORE INSERT` trigger compares the record's fixed prefix and suffix with byte templates built
    from the row's columns, its admission and its `rounds` row;
  - each LBT-9 record is a canonical object with sorted keys, so its start and end are fully determined
    by those fields;
  - the binding covers:
    - genesis: chain, game, versions, round ID and zero `prevHash`;
    - command records: command bytes, command ID, effective time, `prevHash`, result, round ID and
      sequence.
- `fencing_token`: the token the record was committed under (P-19).
- Append-only (P-13).

**Rationale.**
- LBT-9 §11 asks for exactly these rows.
- The database rejects:
  - a record whose hash is wrong;
  - a record that does not extend the previous hash;
  - a record whose columns disagree with its bytes;
  - a record whose effective time or command differs from what was durably admitted (ADR-002's core
    guarantee).
- Export and recovery can therefore trust the columns they read next to the bytes.
- The constraints run once per record. A 1.5 MB settlement record costs one SHA-256 and two prefix or
  suffix comparisons.

**Alternatives.**
- Hash computed only by the engine: one implementation, and a bug writes a broken chain that only
  `crashctl audit` would notice later.
- A `GENERATED … STORED` hash column: the database becomes the only hasher, and the engine cannot have
  its own value cross-checked.
- `jsonb` with re-encoding: forbidden by LBT-9.
- Parsing the JSON in a trigger to bind fields: much more code; the anchored templates are exact for
  OCJ.
- One row per event: contradicts A-7.

### P-17. Command results: columns plus a view

**Proposal.**
- Command records carry `result_status` (`accepted` | `rejected`) and `result_reason`. Genesis carries
  neither.
- A `CHECK` fixes their shape: an accepted result has no reason; a rejected one has a reason matching
  `^[a-z][a-z0-9_]*$` (LBT-9 §5.4).
- The binding trigger (P-16) ties both to the bytes.
- View `orvex.command_results(command_id, round_id, sequence, effective_time_unix_micro, status,
  reason)` joins `admissions` with `chain_records`. Pending admissions do not appear in it.

**Rationale.**
- One record per execution transaction holding exactly one terminal result (ADR-005, A-7) is then true
  by construction.
- Fail-closed `Decide` errors produce no record and therefore no result (A-7).

**Alternatives.**
- A separate `command_results` table: needs a deferred trigger to keep it 1:1 with records, and its
  values duplicate bytes that are already bound.

### P-18. Domain events: derived index without payloads

**Proposal.** `domain_events(round_id, event_sequence, sequence, event_type)`:
- primary key `(round_id, event_sequence)`;
- event sequences contiguous from 1 by a self foreign key `(round_id, event_sequence-1)`;
- foreign key `(round_id, sequence)` to the command record that emitted the event;
- `event_type` is the event object's `type` member (§2 item 12).

Writers insert one row per event in the record's transaction. The canonical event bytes live only in
`chain_records.record`.

**Rationale.**
- It gives ledger postings a foreign-key target `(round_id, event_sequence)` (LBT-9 §11 "every posting
  references a chained `(roundId, eventSequence)`").
- It makes event contiguity a database invariant and supports queries by type.
- No planned reader needs a second copy of the bytes:
  - recovery (LBT-14) and visual replay (LBT-16) decode chain records;
  - outbox messages carry their own payload.
  A copy could disagree with the hashed bytes.

**Alternatives.**
- A payload `bytea` copy: a duplicate that can drift. Binding it would need an O(record) search per
  event, about 15 GB scanned for a 10,000-event settlement.
- A `jsonb` copy for analytics: can be added later as an explicitly derived column.

### P-19. Ownership and fencing

**Proposal.**
- `round_execution_heads` has these columns:
  - `processed_sequence bigint`: the round's version and chain head;
  - `fencing_token bigint`: starts at 0, the round-creation token;
  - `owner_id text` (1–128 printable ASCII) and `lease_expires_at timestamptz`, both null or both set.
- Trigger `round_execution_heads_forward` (`OX005`):
  - `round_id` never changes;
  - `processed_sequence` and `fencing_token` never decrease;
  - any change of `owner_id`, including release to null, needs `fencing_token` to increase.
- `chain_records.fencing_token` records the token each record was committed under.
- Deferred constraint trigger `chain_records_fenced` (`OX004`), run at commit for each inserted record:
  1. lock the round's execution head `FOR SHARE`;
  2. require `fencing_token = NEW.fencing_token` and `processed_sequence ≥ NEW.sequence`.
- With the deferred foreign key from `processed_sequence` to `chain_records`, the head equals the last
  committed record at every commit.

**Rationale.**
- ADR-005 "stale owner cannot commit" is then a database property, independent of LBT-12's
  compare-and-set `WHERE` clause.
- `FOR SHARE` conflicts with a takeover's `UPDATE`. Either the takeover commits first and the stale
  writer fails (`OX004`), or the takeover waits for the writer's commit (prototype, §1).
- Leases use the database clock (`now()`) in LBT-14, so engines need not agree on time.
- Per-round ownership follows ADR-001 "each round is a consistency and ownership boundary".

**Alternatives.**
- A fencing check only in LBT-12's `UPDATE … WHERE fencing_token = $t`: correct if LBT-12 never
  forgets it, with nothing in the database to catch a mistake.
- A separate `round_owners` table: B would update two rows.
- Advisory locks for ownership: not durable across connections and invisible to audits.

### P-20. Snapshots

**Proposal.** `round_snapshots(round_id, sequence, state_hash, state, engine_build, created_at)`:
- primary key `(round_id, sequence)`, with a foreign key to the chain record it describes;
- `state_hash` holds 32 bytes: the LBT-9 A-10 state hash of the engine's state after that record;
- `state` is optional codec `EncodeState` bytes for LBT-14 recovery;
- `engine_build` is the build that computed the hash;
- append-only.

Requirements on writers (LBT-12; round creation):
- write sequence 0 in the round-creation transaction;
- write the record that settles a round in the same transaction;
- always compute hashes from the engine's state at commit, never by replaying at export time
  (LBT-9 §11).

A rejection leaves the state unchanged, so a snapshot for a later rejected record may reuse the last
computed hash at O(1).

**Rationale.**
- Snapshot rows are the export's checkpoints (P-24), and the anchor for LBT-14 recovery.
- `state` holds the seed and crash outcome, so it is secret-bearing, like `round_secrets`. It stays
  nullable until LBT-14 decides whether to store it.

**Alternatives.**
- Snapshot columns on the execution head: keeps only the latest snapshot and loses checkpoints.
- A state hash in every record: rejected by LBT-9 A-10 as O(n²).
- A database `CHECK` that `state` hashes to `state_hash`: possible later, since the state document is
  OCJ-concatenable. LBT-14 verifies at load in any case.

### P-21. Ledger

**Proposal.** Five tables (§5.6):
- `ledger_accounts`:
  - `kind` is `player` (one per `player_id`, floor enforced) or `system` (named; global or scoped to a
    round; may go negative);
  - totals `debits_posted`, `credits_posted` and `held`, all ≥ 0, maintained **only** by triggers
    (`OX002`);
  - balance = `credits_posted − debits_posted`; available = balance − `held`;
  - `CHECK (NOT floor_enforced OR debits_posted + held <= credits_posted)`.
- `ledger_transactions`:
  - one per business operation, with `kind`;
  - optional reference `(round_id, sequence)` to the chain record that caused it, unique, so **at most
    one ledger transaction per record**.
- `ledger_postings`:
  - each row moves `amount > 0` from `debit_account_id` to `credit_account_id`, which must differ;
  - key `(transaction_id, posting_index)`;
  - optional `(round_id, event_sequence)` reference to the chained event it settles;
  - a trigger adds `amount` to the debit account's `debits_posted` and the credit account's
    `credits_posted`, lower `account_id` first.
- `ledger_holds`: a reservation of `amount` on an account by admission `(round_id, sequence)`. A trigger
  adds it to `held`.
- `ledger_hold_releases`: the release of a hold, written once the admission's command record exists. A
  trigger subtracts the hold's amount from `held`.

**Rationale.**
- **Balanced double entry by construction.** Every posting row is one debit leg and one credit leg of
  equal amount. Every ledger transaction therefore balances, and
  Σ `debits_posted` = Σ `credits_posted` = Σ `amount` over all accounts. No deferred trigger is needed
  (§2 item 6).
- **Cross-round protection (ADR-005).**
  - A posting or hold updates the account row. That locks the row until commit, and the floor `CHECK`
    is evaluated on the newest version under READ COMMITTED.
  - Two rounds therefore cannot jointly overspend one player's credits:
    `TestLedgerConcurrentCrossRoundHolds`.
- **Reservation is needed.**
  - `Decide` is pure and cannot see balances. A bet that the rules accept but the ledger refuses would
    leave the round stuck, and inventing a rejection outside `Decide` breaks decision replay.
  - So LBT-11 reserves credits at admission with a hold, and LBT-12 posts and releases at execution.
  - Holds are append-only facts (hold, then release), which keeps the ledger auditable.
- **Duplicate payout impossible.** Records are unique per `(round_id, sequence)`, and at most one
  ledger transaction exists per record.
- Trigger-maintained totals cannot drift from the postings.
- **Hot rows.** Every posting rewrites both accounts. LBT-12 must avoid one global system account per
  posting, for example by using round-scoped system accounts settled once per round. LBT-15 measures.

**Alternatives.**
- A journal with signed per-account postings and a deferred "sum = 0" constraint trigger:
  - one check per posting row makes the work quadratic per transaction;
  - preventing later inserts into an already committed transaction needs transaction-ID tricks.
- Application-maintained balances: can drift.
- Balances computed by `SUM` on demand with no stored totals: a floor check would need `SERIALIZABLE`
  or explicit locks, and gets slow.
- Holds as a mutable `status` column: loses history.

LBT-12 designs the posting rules (accounts, amounts, which events). LBT-10 designs no posting logic.

### P-22. Outbox

**Proposal.** `outbox_messages`:
- `message_id` is an identity column and the primary key;
- `(round_id, sequence, message_index)` is unique, with a foreign key to the producing chain record.
  It is the consumers' **dedupe key**;
- `topic`, `payload bytea` (opaque; LBT-13 defines the format) and `created_at`;
- delivery columns `attempts`, `next_attempt_at`, `last_error` and `published_at`.

Guard trigger (`OX006`):
- An `UPDATE` may change only the delivery columns; `attempts` never decreases; a published row never
  changes again.
- A `DELETE` is allowed only for published rows (retention).
- `TRUNCATE` raises `OX001`.
- A partial index `(next_attempt_at, message_id) WHERE published_at IS NULL` serves a relay that uses
  `FOR UPDATE SKIP LOCKED`.

**Rationale.**
- ADR-005: at-least-once delivery; consumers deduplicate on a stable key. Because records are fenced
  and unique, a stable key is enough.
- `message_id` is not commit order. Relays must not treat it as a high-water mark; per-round order is
  `(sequence, message_index)`.
- ADR-005 open validation "outbox retry": the columns support backoff; the relay is later.

**Alternatives.**
- An append-only outbox plus a separate deliveries table: purer, but two inserts per attempt.
- `LISTEN/NOTIFY` only: not durable.
- Logical-decoding CDC: operationally heavier, and premature.

### P-23. Transaction isolation

**Proposal.** Section 6 is normative. Summary:

| Transaction | Isolation | Concurrency control |
|---|---|---|
| A: admission (LBT-11) | READ COMMITTED, read-write | first statement locks `round_admission_heads` for the round (`SELECT … FOR UPDATE` or `UPDATE … RETURNING`); then dedupe, window, `clock_timestamp()`, admission insert, holds |
| B: execution (LBT-12) | READ COMMITTED, read-write | first statement is the compare-and-set `UPDATE round_execution_heads … WHERE processed_sequence = k-1 AND fencing_token = t` (0 rows means a conflict); then inserts; deferred `OX004` and head foreign keys at commit; account floors at each statement |
| Round creation | READ COMMITTED | inserts only; deferred foreign keys require the genesis record |
| Ownership takeover (LBT-14) | READ COMMITTED | conditional `UPDATE` of the execution head |
| Export and other multi-statement reads | REPEATABLE READ, READ ONLY | one consistent snapshot |
| Migrations | READ COMMITTED | advisory transaction lock, one file per transaction |

- Go exposes these as `postgres.AdmissionTxOptions()`, `postgres.ExecutionTxOptions()` and
  `postgres.SnapshotReadTxOptions()`.
- `postgres.IsRetryable(err)` is true only for `40001` and `40P01`.

**Rationale.**
- Every invariant here is enforced by one of:
  - a lock on one row (heads, accounts);
  - a key or foreign key;
  - a `CHECK` on the newest row version;
  - a commit-time check that takes its own lock.
  READ COMMITTED evaluates all of them against the newest committed data.
- Under REPEATABLE READ or SERIALIZABLE, a lock wait on a concurrently updated row ends in `40001`.
  Every contended hot-round admission would retry (ADR-006 "retry storms").
- SERIALIZABLE adds predicate-lock overhead and false-positive aborts and protects nothing extra.
- ADR-002's "`clock_timestamp()` after lock acquisition" requires the lock to be the first statement.

**Alternatives.**
- SERIALIZABLE everywhere: simple to reason about, but aborts and retries under contention.
- REPEATABLE READ for B: every concurrent account update turns into a retry.
- `SELECT … FOR UPDATE` on all accounts up front: possible for LBT-12, but not required.

### P-24. History export from the tables

**Proposal.** `eventstore.ExportHistory(ctx, db, roundID, w) (Export, error)` runs in one REPEATABLE
READ READ ONLY transaction. §7 has the algorithm.

1. Load `round_secrets.config_secret`. A missing round gives `ErrRoundNotFound`.
2. `S` = the highest `round_snapshots.sequence`. No snapshot gives `ErrNoCheckpoint`.
3. `head` = `chain_records.hash` at `S`.
4. Write the header line. It is OCJ built with `canonjson` from:
   - `checkpoints` = every snapshot with `sequence ≤ S`, in order;
   - `configSecret` = the parsed secret bytes;
   - `format` = `orvex/audit-history/v1`;
   - `head`.
5. Write each `chain_records.record` for sequences `0..S` verbatim, each followed by `\n`.

There is no `crashctl` export command in LBT-10.

**Rationale.**
- Records are copied byte for byte and never re-encoded from columns (LBT-9 §11).
- Checkpoint hashes come from engine-written snapshot rows.
- A v1 file must end at its last checkpoint (A-11), so the export ends at `S`. For a settled round,
  `S` is the settlement record (P-20).
- One snapshot transaction gives a consistent file even while the round advances.
- `TestExportGoldenHistories` proves the round trip: every LBT-9 golden history loaded into the schema
  exports byte-identical and passes `verification.Verify`.

**Alternatives.**
- Recomputing state hashes by replay at export: proves nothing (LBT-9 §11).
- Exporting all records with a checkpoint only at `S < N`: invalid v1.
- A `crashctl audit export` command now: the export has no production caller until LBT-12 writes rows.
  It becomes a small follow-up (section 12).

### P-25. Seed secrecy at rest

**Proposal.**
- `round_secrets.config_secret` stores the codec's secret config part (`EncodeConfig`'s `secret`, for
  crash/v1 `{"seed":"<hex>"}`) verbatim and unencrypted, one row per round, append-only.
- It is never inside a chain record (A-12).
- Readers are the engine (to build `InitialState` on recovery) and the export.
- LBT-14 grants restrict who can read it.
- `docs/architecture/persistence.md` states the exposure: before settlement, anyone who can read
  `round_secrets`, `round_snapshots.state` or a backup can predict the round's crash point.

**Rationale.**
- Credits are simulated (ADR-004 exclusions).
- ADR-004 already notes that commit/reveal proves consistency, not impartial operation.
- Application-level encryption needs key management (KMS, rotation, a key per deployment), which no
  milestone has scoped.

**Alternatives.**
- AES-256-GCM in the engine with a key from the environment: real protection against database or backup
  readers, plus a key lifecycle to design. This is the natural next step (open item).
- `pgcrypto` with the key sent in SQL: the key appears in statements and logs.
- Deleting the secret after settlement: the export's `configSecret` needs it, and `RoundSettled`
  reveals it anyway.

### P-26. Retention

**Proposal.**
- Keep every row of every append-only table indefinitely. No deletion path exists (P-13).
- Published outbox rows may be deleted; when is an operations policy (LBT-14).
- Any future archival must first export and verify complete histories, and needs an ADR-001-level
  decision.

**Rationale.** ADR-001 asks for retention to be defined before claiming audit-grade behavior. "Keep
everything" is the only policy that needs no further proof. Volume is measured in LBT-15.

**Alternatives.**
- Time-based partitioning plus detach: later, if volume requires it.
- Deleting settled rounds after export: needs a trusted external archive (ADR-001 anchor).

## 5. Normative schema

Conventions for all tables:
- columns are `NOT NULL` unless marked "null";
- constraint names are given where tests assert on them, and follow P-11 elsewhere;
- "AO" means the P-13 append-only triggers;
- every generated column is `GENERATED ALWAYS AS (…) STORED`.

### 5.1 `0001_foundation`

- First statement: the UTF8 check (`OX007`, `orvex requires a UTF8 database`).
- `CREATE SCHEMA orvex`, with a comment.
- `orvex.forbid_mutation()` (P-13).
- Down: drop the function, then `DROP SCHEMA orvex` (default `RESTRICT`, so it fails if anything
  remains).

### 5.2 `0002_rounds_admissions`

**`orvex.rounds`** (AO)

| column | type | constraints |
|---|---|---|
| `round_id` | uuid | `rounds_pkey`; `rounds_round_id_check` ≠ `00000000-0000-0000-0000-000000000000` |
| `game` | text | `rounds_game_check` `~ '^[a-z][a-z0-9-]{0,31}$'` |
| `rules_version`, `schema_version`, `state_version` | bigint | `rounds_versions_check`: each `BETWEEN 1 AND 4294967295` |
| `chain` | text | `rounds_chain_check` `= 'orvex/audit/v1'` |
| `created_at_unix_micro` | bigint | equals genesis `createdAtUnixMicro` (written from the same value; not bound) |

**`orvex.round_secrets`** (AO)

| column | type | constraints |
|---|---|---|
| `round_id` | uuid | `round_secrets_pkey`; foreign key `rounds` |
| `config_secret` | bytea | `round_secrets_config_secret_check` `octet_length >= 2` |

**`orvex.admissions`** (AO)

| column | type | constraints |
|---|---|---|
| `round_id` | uuid | foreign key `rounds` (`admissions_round_fkey`) |
| `sequence` | bigint | `admissions_sequence_check` `>= 1` |
| `prev_sequence` | bigint, generated `NULLIF(sequence, 1) - 1`, null | `admissions_prev_fkey` `(round_id, prev_sequence)` → `admissions (round_id, sequence)` |
| `command_id` | uuid | `admissions_command_id_key` UNIQUE; `admissions_command_id_check` ≠ nil UUID |
| `effective_time_unix_micro` | bigint | |
| `command` | bytea | `admissions_command_check` `octet_length >= 2` |

Primary key `admissions_pkey (round_id, sequence)`.

**`orvex.round_admission_heads`** (mutable; `DELETE`/`TRUNCATE` raise `OX001`)

| column | type | constraints |
|---|---|---|
| `round_id` | uuid | primary key; foreign key `rounds` |
| `admitted_sequence` | bigint default 0 | `>= 0` |
| `admitted_ref` | bigint, generated `NULLIF(admitted_sequence, 0)`, null | `round_admission_heads_head_fkey` `(round_id, admitted_ref)` → `admissions`, `DEFERRABLE INITIALLY DEFERRED` |

Trigger `round_admission_heads_forward`, `BEFORE UPDATE`: raises `OX005` if `round_id` changes or
`admitted_sequence` decreases.

Down: refuse (`OX003`) if any of the four tables has rows, then drop them in reverse order.

### 5.3 `0003_chain_execution`

**`orvex.chain_records`** (AO)

| column | type | constraints |
|---|---|---|
| `round_id` | uuid | foreign key `rounds` |
| `sequence` | bigint | `>= 0` |
| `prev_sequence` | bigint, generated `NULLIF(sequence, 0) - 1`, null | `chain_records_prev_fkey` `(round_id, prev_sequence, prev_hash)` → `chain_records (round_id, sequence, hash)` |
| `admission_sequence` | bigint, generated `NULLIF(sequence, 0)`, null | `chain_records_admission_fkey` `(round_id, admission_sequence)` → `admissions` |
| `prev_hash` | bytea | `chain_records_prev_hash_check` length 32; `chain_records_genesis_check` `sequence <> 0 OR prev_hash = '\x00…00'` (32 zero bytes) |
| `hash` | bytea | `chain_records_hash_length_check` length 32; `chain_records_hash_check` `hash = sha256('orvex/audit/v1/record:'::bytea \|\| record)` |
| `record` | bytea | `chain_records_record_check` `octet_length(record) <= 67108864 AND position('\x0a'::bytea in record) = 0` |
| `result_status` | text, null | see `chain_records_result_check` |
| `result_reason` | text, null | |
| `fencing_token` | bigint | `>= 0` |

- `chain_records_result_check`:
  - sequence 0: both result columns null;
  - `accepted`: reason null;
  - `rejected`: reason `~ '^[a-z][a-z0-9_]*$'`;
  - any other status: false.
- Primary key `chain_records_pkey (round_id, sequence)`. Unique `chain_records_link_key
  (round_id, sequence, hash)` is the foreign-key target.

**Binding trigger** `chain_records_binding` (`BEFORE INSERT`, `orvex.check_record_binding()`, `OX008`).
- Notation: `‖` is byte concatenation. Text parts are converted with `convert_to(…, 'UTF8')`. Never
  cast text to `bytea` with `::bytea`, which would parse backslash escapes.
- Formatting: `uuid::text` (lowercase 8-4-4-4-12), `bigint::text` and `encode(…, 'hex')`.
- The record must start with prefix `P` and end with suffix `S`, and
  `octet_length(record) >= octet_length(P) + octet_length(S)`.
- Genesis (`sequence = 0`), with `g` the `rounds` row:
  - `P` = `{"chain":"` ‖ g.chain ‖ `","config":{`
  - `S` = `,"game":"` ‖ g.game ‖ `","kind":"genesis","prevHash":"` ‖ 64×`0` ‖ `","roundId":"` ‖
    round_id ‖ `","rulesVersion":"` ‖ g.rules_version ‖ `","schemaVersion":"` ‖ g.schema_version ‖
    `","sequence":"0","stateVersion":"` ‖ g.state_version ‖ `"}`
- Command record (`sequence = k ≥ 1`), with `a` the admission `(round_id, k)` (it exists, by foreign
  key):
  - `P` = `{"command":` ‖ a.command ‖ `,"commandId":"` ‖ a.command_id ‖ `","effectiveTimeUnixMicro":"`
    ‖ a.effective_time_unix_micro ‖ `","engineBuild":"`
  - `S` = `],"kind":"command","prevHash":"` ‖ hex(prev_hash) ‖ `","result":` ‖ R ‖ `,"roundId":"` ‖
    round_id ‖ `","sequence":"` ‖ k ‖ `"}`
  - `R` = `{"status":"accepted"}` or `{"reason":"` ‖ result_reason ‖ `","status":"rejected"}`
- These follow from LBT-9 §5.4's sorted keys:
  - command records: `command` < `commandId` < `effectiveTimeUnixMicro` < `engineBuild` < `events` <
    `kind` < `prevHash` < `result` < `roundId` < `sequence`;
  - genesis: … `engineBuild` < `game` < `kind` < … < `stateVersion`.
- Verified against the §5.6 literals on PostgreSQL 18.6 (§1).
- The anchored prefix and suffix are unambiguous: OCJ escapes `"` inside strings, so the text before
  `S` or after `P` cannot fake a boundary.

**`orvex.round_execution_heads`** (mutable; `DELETE`/`TRUNCATE` raise `OX001`)

| column | type | constraints |
|---|---|---|
| `round_id` | uuid | primary key; foreign key `rounds` |
| `processed_sequence` | bigint default 0 | `>= 0`; `round_execution_heads_head_fkey` `(round_id, processed_sequence)` → `chain_records`, `DEFERRABLE INITIALLY DEFERRED` |
| `fencing_token` | bigint default 0 | `>= 0` |
| `owner_id` | text, null | `round_execution_heads_owner_check`: `(owner_id IS NULL) = (lease_expires_at IS NULL)` and `owner_id ~ '^[!-~]{1,128}$'` when set |
| `lease_expires_at` | timestamptz, null | |

Triggers:
- `round_execution_heads_forward` (`BEFORE UPDATE`, `OX005`), per P-19.
- On `chain_records`: constraint trigger `chain_records_fenced` (`AFTER INSERT`, `DEFERRABLE INITIALLY
  DEFERRED`, `FOR EACH ROW`, `orvex.check_record_fenced()`, `OX004`), per P-19. It is created after
  both tables.

**`orvex.domain_events`** (AO)

| column | type | constraints |
|---|---|---|
| `round_id` | uuid | |
| `event_sequence` | bigint | `>= 1` |
| `prev_event_sequence` | bigint, generated `NULLIF(event_sequence, 1) - 1`, null | `domain_events_prev_fkey` → `domain_events (round_id, event_sequence)` |
| `sequence` | bigint | `>= 1`; `domain_events_record_fkey` `(round_id, sequence)` → `chain_records` |
| `event_type` | text | `domain_events_event_type_check` `~ '^[A-Z][A-Za-z0-9]{0,63}$'` |

Primary key `(round_id, event_sequence)`; index `domain_events_record_idx (round_id, sequence)`.

**View `orvex.command_results`**: `admissions a JOIN chain_records r USING (round_id, sequence)` →
`a.command_id, round_id, sequence, a.effective_time_unix_micro, r.result_status AS status,
r.result_reason AS reason`.

Down: refuse (`OX003`) on rows; drop the view, the triggers, functions and tables in reverse order.

### 5.4 `0004_snapshots`

**`orvex.round_snapshots`** (AO)

| column | type | constraints |
|---|---|---|
| `round_id`, `sequence` | uuid, bigint | primary key; `sequence >= 0`; `round_snapshots_record_fkey` → `chain_records` |
| `state_hash` | bytea | length 32 |
| `state` | bytea, null | |
| `engine_build` | text | `~ '^[!-~]{1,128}$'` |
| `created_at` | timestamptz default `now()` | |

### 5.5 Summary of 0002–0004 relationships

`rounds` 1–1 `round_secrets`, `round_admission_heads` and `round_execution_heads`. `rounds` 1–n
`admissions` 1–0..1 `chain_records` (sequence ≥ 1). A round has exactly one genesis record
(sequence 0). Each chain record has 0..n `domain_events` and 0..1 `round_snapshots` row.

### 5.6 `0005_ledger`

**`orvex.ledger_accounts`** (totals mutable only by triggers)

| column | type | constraints |
|---|---|---|
| `account_id` | uuid | primary key |
| `kind` | text | `IN ('player','system')` |
| `player_id` | text, null | `ledger_accounts_player_id_key` UNIQUE |
| `name` | text, null | `~ '^[a-z][a-z0-9_]{0,63}$'` |
| `round_id` | uuid, null | foreign key `rounds` |
| `floor_enforced` | boolean | |
| `debits_posted`, `credits_posted`, `held` | bigint default 0 | `ledger_accounts_totals_check` each `>= 0` |
| `created_at` | timestamptz default `now()` | |

- `ledger_accounts_kind_check`:
  - a player row has `player_id` set and non-empty, `name` null, `round_id` null and
    `floor_enforced = true`;
  - a system row has `player_id` null and `name` set.
- `ledger_accounts_floor_check`: `NOT floor_enforced OR debits_posted + held <= credits_posted`.
- Partial unique index `ledger_accounts_system_name_key ON (round_id, name) NULLS NOT DISTINCT WHERE
  kind = 'system'`. A table-level constraint is wrong here (§1).
- Trigger `ledger_accounts_guard` (`BEFORE INSERT OR UPDATE`, `OX002`):
  - an insert needs all three totals at 0;
  - an update needs `pg_trigger_depth() >= 2` (called from a ledger trigger) and may change only the
    three totals.
- `DELETE` and `TRUNCATE` raise `OX001`.

**`orvex.ledger_transactions`** (AO)

| column | type | constraints |
|---|---|---|
| `transaction_id` | uuid | primary key |
| `kind` | text | `~ '^[a-z][a-z0-9_]{0,63}$'` |
| `round_id`, `sequence` | uuid, bigint, both null or both set | `ledger_transactions_record_fkey` → `chain_records`; `ledger_transactions_record_key` UNIQUE `(round_id, sequence)` |
| `created_at` | timestamptz default `now()` | |

Also `ledger_transactions_round_key` UNIQUE `(transaction_id, round_id)`, the foreign-key target for
postings.

**`orvex.ledger_postings`** (AO)

| column | type | constraints |
|---|---|---|
| `transaction_id` | uuid | foreign key `ledger_transactions` |
| `posting_index` | integer | `>= 0` |
| `debit_account_id`, `credit_account_id` | uuid | foreign keys `ledger_accounts`; `ledger_postings_accounts_check` distinct |
| `amount` | bigint | `ledger_postings_amount_check` `> 0` |
| `round_id` | uuid, null | `ledger_postings_round_fkey` `(transaction_id, round_id)` → `ledger_transactions (transaction_id, round_id)` |
| `event_sequence` | bigint, null | `event_sequence IS NULL OR round_id IS NOT NULL`; `ledger_postings_event_fkey` `(round_id, event_sequence)` → `domain_events` |

- Primary key `(transaction_id, posting_index)`.
- Indexes `ledger_postings_debit_idx (debit_account_id)`, `ledger_postings_credit_idx
  (credit_account_id)`, and `ledger_postings_event_idx (round_id, event_sequence) WHERE event_sequence
  IS NOT NULL`.
- Trigger `ledger_postings_apply` (`AFTER INSERT`): updates the two accounts' totals, lower
  `account_id` first.

**`orvex.ledger_holds`** (AO)

| column | type | constraints |
|---|---|---|
| `round_id`, `sequence` | uuid, bigint | `ledger_holds_admission_fkey` → `admissions` |
| `account_id` | uuid | foreign key `ledger_accounts`; index `ledger_holds_account_idx` |
| `amount` | bigint | `> 0` |
| `created_at` | timestamptz default `now()` | |

Primary key `(round_id, sequence, account_id)`. Trigger `ledger_holds_apply` (`AFTER INSERT`): adds
`amount` to the account's `held`.

**`orvex.ledger_hold_releases`** (AO)

| column | type | constraints |
|---|---|---|
| `round_id`, `sequence`, `account_id` | uuid, bigint, uuid | primary key; `ledger_hold_releases_hold_fkey` → `ledger_holds`; `ledger_hold_releases_record_fkey` `(round_id, sequence)` → `chain_records` |
| `created_at` | timestamptz default `now()` | |

Trigger `ledger_hold_releases_apply` (`AFTER INSERT`): subtracts the hold's amount from `held`.

Foreign-key indexes are added only where a query path needs them. Referenced rows are never deleted,
so foreign-key enforcement needs none.

### 5.7 `0006_outbox`

**`orvex.outbox_messages`**

| column | type | constraints |
|---|---|---|
| `message_id` | bigint `GENERATED ALWAYS AS IDENTITY` | primary key |
| `round_id`, `sequence` | uuid, bigint | `outbox_messages_record_fkey` → `chain_records` |
| `message_index` | integer | `>= 0`; `outbox_messages_dedupe_key` UNIQUE `(round_id, sequence, message_index)` |
| `topic` | text | `~ '^[a-z][a-z0-9_.-]{0,127}$'` |
| `payload` | bytea | |
| `created_at` | timestamptz default `now()` | |
| `attempts` | integer default 0 | `>= 0` |
| `next_attempt_at` | timestamptz default `now()` | |
| `last_error` | text, null | |
| `published_at` | timestamptz, null | |

Partial index `outbox_messages_pending_idx (next_attempt_at, message_id) WHERE published_at IS NULL`.
Guard trigger `outbox_messages_guard` (`BEFORE UPDATE OR DELETE`, `OX006`) per P-22, plus a `TRUNCATE`
trigger (`OX001`).

### 5.8 Functions

All are `plpgsql` with `SET search_path = pg_catalog`:
- `forbid_mutation` (0001);
- `check_admission_head` (0002);
- `check_record_binding`, `check_record_fenced`, `check_execution_head` (0003);
- `guard_ledger_account`, `apply_ledger_posting`, `apply_ledger_hold`, `apply_ledger_hold_release`
  (0005);
- `guard_outbox_message` (0006).

## 6. Transaction isolation and locking (normative for LBT-11, LBT-12, LBT-14)

1. Every transaction states its isolation explicitly with the `postgres` options functions. None relies
   on the server default.
2. Lock order:
   1. the round's head row: `round_admission_heads` in A, `round_execution_heads` in B and takeovers;
   2. ledger accounts, through holds and postings.
   Never lock an execution head inside A or an admission head inside B. Admissions and chain records
   are immutable and need no locks to read.
3. Within a transaction, insert holds and postings so that accounts are first touched in ascending
   `account_id` where feasible. A deadlock (`40P01`) is retryable.
4. Error classes:
   - `40001` and `40P01`: retry the whole transaction from authoritative state.
   - Commit-time `OX004`, or `23503` on `round_execution_heads_head_fkey`, in B: an ownership or version
     conflict. Discard the computed transition and reload (ADR-005).
   - `23505` on `admissions_command_id_key` in A: a duplicate. Look up the original admission
     (ADR-005 "after admission commit: retry returns original admission").
   - `23514` on `ledger_accounts_floor_check`: insufficient available credits, a business outcome.
   - Any other `23xxx` or `OX`: a bug. Fail closed and do not retry.
5. Effective time (A): after the lock, `(EXTRACT(EPOCH FROM clock_timestamp()) * 1000000)::bigint`
   (exact, §1). Never `now()`, which is the transaction start time (ADR-002).
6. B commits only when its first statement, the compare-and-set update, affected exactly one row. The
   deferred checks are a backstop, not the protocol.

## 7. History export (normative)

### 7.1 API

```go
package eventstore

type TxBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}
type Checkpoint struct{ Sequence uint64; StateHash [32]byte }
type Export struct {
	RoundID     [16]byte
	Records     int // S + 1
	Head        [32]byte
	Checkpoints []Checkpoint
}

// ExportHistory writes round roundID as an "orvex/audit-history/v1" file
// (LBT-9 A-11) to w. On error, w may hold a partial file the caller must discard.
func ExportHistory(ctx context.Context, db TxBeginner, roundID [16]byte, w io.Writer) (Export, error)

var (
	ErrRoundNotFound = errors.New("eventstore: round not found")
	ErrNoCheckpoint  = errors.New("eventstore: round has no snapshot to end an export at")
	ErrCorrupt       = errors.New("eventstore: stored round data violates an export invariant")
)
```

Use LBT-9's exported constant for `orvex/audit-history/v1` if one exists. Otherwise declare
`HistoryFormat` here, and the tests cross-check it against `verification.Report.Format`.

### 7.2 Algorithm

1. Begin with `postgres.SnapshotReadTxOptions()`. Always roll back; the transaction is read-only.
2. Query `SELECT s.config_secret FROM orvex.rounds r LEFT JOIN orvex.round_secrets s USING (round_id)
   WHERE r.round_id = $1`.
   - No row: `ErrRoundNotFound`.
   - A null secret: `ErrCorrupt`.
3. `SELECT sequence, state_hash FROM orvex.round_snapshots WHERE round_id = $1 ORDER BY sequence`.
   - Empty: `ErrNoCheckpoint`.
   - Otherwise `S` = the last sequence.
4. `head` = `SELECT hash FROM orvex.chain_records WHERE round_id = $1 AND sequence = S`.
5. Header: `encodeHeader(head, checkpoints, configSecret)`.
   - `configSecret` passes `canonjson.Unmarshal`; on failure, `ErrCorrupt`.
   - The header is `canonjson.Marshal({"checkpoints": [{"sequence": Uint(s), "stateHash": Hex(h)}…],
     "configSecret": <object>, "format": "orvex/audit-history/v1", "head": Hex(head)})`.
   - Write it followed by `\n`.
6. `SELECT sequence, record FROM orvex.chain_records WHERE round_id = $1 AND sequence <= S ORDER BY
   sequence`.
   - Row `i` must have `sequence == i`.
   - The record must have no `\n` and be at most 64 MiB. A database `CHECK` already enforces both; the
     exporter rechecks.
   - A violation gives `ErrCorrupt`. Otherwise write the record followed by `\n`.
   - The count must be `S + 1`.
7. Flush the `bufio.Writer` and return `Export`.

### 7.3 Loader used by tests (`pgtest.LoadHistory`)

This mirrors what production writers must do. It runs in one READ COMMITTED transaction.

1. Parse the header with `canonjson.Unmarshal` and every record line with `chain.Decode`.
2. Insert:
   1. `rounds` from genesis;
   2. `round_secrets` = `canonjson.Marshal(header.configSecret)`;
   3. `round_admission_heads(admitted_sequence = N)` and `round_execution_heads(processed_sequence = N,
      fencing_token = 0)`;
   4. per command record, an `admissions` row (`command_id`, `sequence`, effective time, `command` = the
      record's command object bytes);
   5. per record, a `chain_records` row: the line bytes, `hash` = `chain.LinkHash(line)`, `prev_hash`,
      the result columns, token 0;
   6. per event, a `domain_events` row: `event_type` = the event object's `type` member;
   7. per selected header checkpoint, a `round_snapshots` row (`state` null, `engine_build` `golden`).
3. Commit. All deferred checks then run against real LBT-9 bytes.
4. Return `Loaded{RoundID, Head, Records, CommandIDs}`.

## 8. Migration runner (normative)

```go
package postgres

type Migration struct {
	Version              int
	Name                 string
	Up, Down             string
	UpSHA256, DownSHA256 [32]byte
}
type Beginner interface{ Begin(ctx context.Context) (pgx.Tx, error) } // *pgx.Conn, *pgxpool.Pool
type MigrationState struct {
	Version   int
	Name      string
	Applied   bool
	AppliedAt time.Time
	AppliedBy string
}

const MigrationLockKey int64 = 7766444933939532367

func LoadMigrations(fsys fs.FS) ([]Migration, error)
func MigrateUp(ctx context.Context, db Beginner, ms []Migration, target int, appliedBy string) (applied []Migration, err error)
func MigrateDown(ctx context.Context, db Beginner, ms []Migration, target int) (reverted []Migration, err error)
func MigrationStatus(ctx context.Context, db Beginner, ms []Migration) ([]MigrationState, error)

var (
	ErrMigrationFiles   = errors.New("postgres: invalid migration files")
	ErrTarget           = errors.New("postgres: migration target out of range")
	ErrWrongDirection   = errors.New("postgres: migration target is on the other side of the current version")
	ErrSchemaTooNew     = errors.New("postgres: database schema is newer than this build")
	ErrChecksumMismatch = errors.New("postgres: applied migration differs from this build")
	ErrSchemaState      = errors.New("postgres: migration bookkeeping is inconsistent")
)
```

- `target` must be in `[0, len(ms)]`, otherwise `ErrTarget`.
- Each step runs in its own transaction:
  1. take the lock;
  2. ensure the bookkeeping schema and table exist;
  3. read the rows ordered by version;
  4. validate them:
     - versions are exactly `1..v` (`ErrSchemaState`);
     - `v <= len(ms)` (`ErrSchemaTooNew`);
     - each row's name and both checksums equal `ms[i]` (`ErrChecksumMismatch`);
  5. decide:
     - `v == target`: commit and return;
     - wrong direction: `ErrWrongDirection`;
     - otherwise `Exec` the file, insert or delete the bookkeeping row, and commit.
- Errors wrap the step, e.g. `postgres: migration 0003 chain_execution up: <pg error>`, so
  `errors.As(err, **pgconn.PgError)` still works.
- `MigrationStatus` takes no lock. A missing bookkeeping table means version 0. It applies the same
  validation.

Other helpers in the package:
- `SQLState(err) string` returns the SQLSTATE of a wrapped `*pgconn.PgError`, or `""`.
- `IsRetryable(err) bool` is true for `40001` and `40P01`.
- The `Code*` constants of P-11.
- `AdmissionTxOptions()` and `ExecutionTxOptions()` return `{ReadCommitted, ReadWrite}`;
  `SnapshotReadTxOptions()` returns `{RepeatableRead, ReadOnly}`.
- `doc.go` holds the package documentation (tables, isolation, error classes, referencing
  `docs/architecture/persistence.md`).

## 9. What LBT-11 and LBT-12 build on (not designed here)

**LBT-11 (admission sequencer).**
- Lock target: `round_admission_heads` (P-14). Increment `admitted_sequence` and insert `admissions`
  in Transaction A (§6).
- Gapless sequences, the global command-ID unique index (retry lookup) and immutability are already
  enforced.
- Cross-round protection: insert `ledger_holds` at admission. The account floor `CHECK` refuses an
  overspend.
- Effective time is computed per §6 item 5.
- Its own migrations (`0007` onward) add the admission window, closure and barrier columns on
  `round_admission_heads`, and optionally an admission-rejection evidence table.
- It rejects player IDs containing U+0000 (§2 item 12) and command bytes that are not codec output.

**LBT-12 (execution commit, ledger, outbox).**
- Transaction B (§6):
  1. the compare-and-set on `round_execution_heads`;
  2. one `chain_records` row (`chain.EncodeCommand`, `chain.LinkHash`, result columns, the current
     token);
  3. one `domain_events` row per event;
  4. at most one `ledger_transactions` row with its postings, plus `ledger_hold_releases`;
  5. `outbox_messages`;
  6. `round_snapshots` per P-20.
- Commit-time `OX004`, `OX008` or `23503` mean conflict or bug, as classified in §6.
- It designs the posting rules, round-scoped system accounts (hot rows, P-21), snapshot cadence,
  outbox topics, and keeps secrets out of outbox payloads (ADR-003 public projection).
- Round creation (rounds, secret, both heads, genesis record and snapshot 0 in one transaction) has no
  Linear owner. Proposed owner: LBT-12, which already encodes chain records. Until then, LBT-11 tests
  use `pgtest.LoadHistory`.

**LBT-14.** Lease logic on `round_execution_heads`, roles and grants (P-13), the `state` bytes and
recovery from snapshots plus chain records, backup/restore tests, Kubernetes migration Job.
**LBT-13.** Outbox payload format and the relay. **LBT-15.** Hot rows, record sizes, TOAST settings.

## 10. LBT-10 acceptance as tests

| Acceptance | Tests (section 11 names the files) |
|---|---|
| Clean up/down migration | `TestMigrateUpDownClean`, `TestMigrateEachStepReversible`, `TestMigrateUpIdempotent`, `TestMigrationFailureRollsBack`, `TestMigrateDownRefusesAuditData`, `TestMigrateCLI` |
| Real PostgreSQL Testcontainers integration | every `*_integration_test.go`, run by `make test-integration` locally and by the CI `integration` job |
| Constraints documented and enforced (ADR-005) | the `schema_integration_test.go` tests; `TestMigrationsCreateOnlyOrvexObjects` (every table and view commented) |
| Isolation documented (ADR-005) | `TestTxOptions`, `TestExecutionCompareAndSet`, `TestLedgerConcurrentCrossRoundHolds` |
| Stores exactly what LBT-9 needs; export reproduces LBT-9 histories | `TestExportGoldenHistories`, `TestExportStopsAtLatestSnapshot`, `TestEncodeHeaderMatchesGolden` |
| ADR-006 critical failures (schema part) | duplicate admission `TestAdmissionsCommandIDGloballyUnique`; conflicting per-round commits `TestExecutionCompareAndSet`, `TestChainRecordsLinked`; cross-round overspending `TestLedgerConcurrentCrossRoundHolds`; stale fencing token `TestChainRecordsRequireCurrentFencingToken`; duplicate payout `TestLedgerOneTransactionPerRecord` |

## 11. Implementation plan (after approval)

Rules for new Go files:
- goimports grouping;
- error strings lowercase without trailing punctuation, prefixed `postgres: `, `pgtest: `,
  `eventstore: ` or `crashctl: `;
- integration tests in external test packages (`package postgres_test`, `eventstore_test`), so that
  `pgtest`, which imports `postgres`, causes no import cycle;
- every integration test file starts with `//go:build integration` and gets its own `TestMain` calling
  `pgtest.Run`;
- tests assert `SQLState` and `PgError.ConstraintName`, never message text.

### Files

| File | Action |
|---|---|
| `go.mod`, `go.sum` | add `github.com/jackc/pgx/v5 v5.11.0`, `github.com/testcontainers/testcontainers-go v0.44.0`; `go mod tidy` |
| `.gitattributes` | new: `/migrations/*.sql text eol=lf` |
| `migrations/migrations.go` | new: package doc + `//go:embed *.sql` `var FS embed.FS` |
| `migrations/0001_foundation.{up,down}.sql` … `0006_outbox.{up,down}.sql` | new, per section 5; first line of each file `-- NNNN name: <purpose> (LBT-10 spec P-n)` |
| `internal/persistence/postgres/doc.go`, `migrate.go`, `errors.go`, `tx.go` | new |
| `internal/persistence/postgres/migrate_test.go`, `errors_test.go`, `tx_test.go` | new (unit) |
| `internal/persistence/postgres/migrate_integration_test.go`, `schema_integration_test.go` | new (integration) |
| `internal/persistence/postgres/pgtest/pgtest.go`, `fingerprint.go`, `load.go` | new |
| `internal/persistence/postgres/pgtest/pgtest_test.go` | new (unit) |
| `internal/persistence/eventstore/export.go`, `export_test.go`, `export_integration_test.go` | new |
| `cmd/crashctl/migrate.go`, `migrate_test.go`, `migrate_integration_test.go` | new |
| `cmd/crashctl/main.go` | edit: dispatch `migrate`; usage text; package comment "replay/audit and operations CLI" |
| `Makefile` | edit: header comment; `test-integration` target; `.PHONY` |
| `.github/workflows/ci.yml` | edit: `integration` job (P-8) |
| `.golangci.yml` | edit: `run.build-tags: [integration]` |
| `deployments/docker/compose.yaml` | new (P-9) |
| `docs/architecture/persistence.md` | new: tables and invariants, authoritative vs derived data, constraint catalogue with SQLSTATEs, §6 isolation and lock order, migration policy, seed exposure, retention, grant matrix, ADR-001 limitation |
| `README.md` | edit: Status adds LBT-10; Layout adds `migrations/`, `internal/persistence/postgres` (+ `pgtest`), `internal/persistence/eventstore`, `deployments/docker`, `docs/architecture`; Development adds Docker as a prerequisite for `make test-integration`, the target, `crashctl migrate` and the Compose commands |
| `AGENTS.md` | edit: Commands gains `make test-integration` (Docker required); Repository conventions gains "merged migrations in `migrations/` are immutable; change the schema with a new migration (checksums are enforced)"; the hand-off line also requires `make test-integration` when `migrations/` or `internal/persistence/` changes |

Unchanged: `docs/adr/**`, `internal/game/**`, `internal/audit/**`, `internal/canonjson/**`, every
golden fixture, `cmd/gateway`, `cmd/engine`.

### Step 1: dependencies and migration files

- Add the two modules and run `go mod tidy`. If tidy adds a direct requirement other than these two,
  stop and report.
- Write the six migration pairs and `migrations.go` per section 5. Name every constraint that section 5
  names exactly as given.

### Step 2: `internal/persistence/postgres` (section 8, P-11, P-23)

Unit tests:
- `migrate_test.go`:
  - `TestLoadMigrations` (table over `fstest.MapFS`):
    - accepted: a valid set, sorted, with checksums equal to `sha256` of the bytes;
    - rejected with `ErrMigrationFiles`: a missing down; a missing up; a gap (`0001`, `0003`); a set
      that does not start at `0001`; a duplicate version with two names; bad names (`1_a.up.sql`,
      `0001_A.up.sql`, `0001-a.up.sql`, `0001_a.sql`); an empty file; `BEGIN;`, `commit;`,
      `ROLLBACK;`, `START TRANSACTION`, `CREATE INDEX CONCURRENTLY`, a `\set` line;
    - allowed: a PL/pgSQL body with a bare `BEGIN` line.
  - `TestEmbeddedMigrations`: `LoadMigrations(migrations.FS)` succeeds. The first six names are exactly
    `foundation`, `rounds_admissions`, `chain_execution`, `snapshots`, `ledger`, `outbox`, and versions
    are contiguous.
- `errors_test.go`:
  - `TestSQLState`: a `*pgconn.PgError`, wrapped once and twice; a non-pg error and nil give `""`.
  - `TestIsRetryable`: `40001` and `40P01` are true; `23505`, `OX001`, nil and a plain error are false.
- `tx_test.go`:
  - `TestTxOptions`: the three option values of P-23.

### Step 3: `pgtest` (P-7, §7.3)

- `pgtest_test.go`:
  - `TestComposeImageMatchesPinnedImage`: reads `../../../../deployments/docker/compose.yaml` and
    requires `image: ` + `Image`.

### Step 4: integration tests in `internal/persistence/postgres`

`migrate_integration_test.go` (the fingerprint excludes `orvex_migrate`):

- **`TestMigrateUpDownClean`**:
  1. take fingerprint F0 of an empty database;
  2. up to the newest version: F1;
  3. down to 0: F2 must equal F0, and `schema_migrations` must have 0 rows;
  4. up again: F3 must equal F1.
- **`TestMigrateEachStepReversible`**: for each `v`, go up to `v`, down to `v-1`, then up to `v`. Each
  fingerprint must equal the forward pass's fingerprint at that version.
- **`TestMigrateUpIdempotent`**: a second up applies nothing and returns an empty list.
- **`TestMigrateConcurrentRunners`**: four goroutines on separate connections all migrate up. There are
  no errors, each version is applied exactly once in total, and the bookkeeping has N rows.
- **`TestMigrateChecksumMismatch`**: apply a `MapFS` set, then change one byte of an applied up file,
  and separately of a down file. `MigrateUp`, `MigrateDown` and `MigrationStatus` all return
  `ErrChecksumMismatch` and change nothing.
- **`TestMigrateSchemaTooNew`**: apply two migrations, then run with the first one only:
  `ErrSchemaTooNew`.
- **`TestMigrateWrongDirection`** and **`TestMigrateTargetRange`**.
- **`TestMigrationFailureRollsBack`**: file `0002` creates a table, then runs `SELECT 1/0`. The error
  wraps `PgError` `22012`; the version is 1; the table does not exist.
- **`TestMigrateDownRefusesAuditData`**:
  1. migrate up and `LoadHistory(worked-example)`;
  2. `MigrateDown(0)` reverts `0006` and `0005` (empty), then fails at `0004` with `OX003`;
  3. the version is 4, and the chain-record and snapshot counts are unchanged;
  4. a new up succeeds.
- **`TestMigrateRequiresUTF8`**: `CREATE DATABASE … ENCODING 'SQL_ASCII' TEMPLATE template0`, then
  migrate up. It fails with `OX007` and leaves version 0.
- **`TestMigrationStatus`**: pending, then applied with `AppliedBy` and a non-zero `AppliedAt`.
- **`TestMigrationsCreateOnlyOrvexObjects`**: after up, nothing exists in `public`, and every table and
  view in `orvex` has a comment.

`schema_integration_test.go`. Each test starts from `MigratedDatabase`, plus `LoadHistory` where it needs
rows. Expected outcomes are `(SQLSTATE, constraint name)`; "at commit" means the error surfaces from
`Commit`.

Test-local helper `appendRejected(t, tx, roundID, k, prevHash, token)`:
- inserts admission `k` with a fixed `PlaceBet` command;
- inserts a well-formed rejected record `k` built from the §5.3 templates (`"events":[]`, reason
  `round_settled`, `engineBuild` `test`) with its correct hash and `fencing_token = token`;
- advances both heads to `k`.

Each negative test changes exactly one input, so the row satisfies every other constraint. Order
matters:
- `BEFORE INSERT` triggers (binding) run first;
- `CHECK` constraints run next, in alphabetical order by name;
- unique indexes follow, then foreign keys, then deferred checks at commit.

- Rounds and admissions:
  - `TestRoundsConstraints`:
    - check violations: nil UUID; game `Crash`; game of 33 characters; version 0; version 4294967296;
      chain `orvex/audit/v2`;
    - `UPDATE` and `DELETE` give `OX001`.
  - `TestRoundCreationRequiresGenesis`: rounds and heads without a genesis record fail at commit with
    `23503` `round_execution_heads_head_fkey`.
  - `TestRoundSecretsAppendOnly`.
  - `TestAdmissionsGapless`: sequence 2 without 1 gives `23503` `admissions_prev_fkey`; sequence 0 gives
    `23514`.
  - `TestAdmissionsCommandIDGloballyUnique`: the same `command_id` in a second round gives `23505`
    `admissions_command_id_key`; the nil UUID gives `23514`.
  - `TestAdmissionsAppendOnly`.
  - `TestRoundAdmissionHeads`:
    - a counter ahead of admissions fails at commit with `23503`;
    - a decrease gives `OX005`;
    - `DELETE` gives `OX001`.
- Chain records:
  - `TestChainRecordsHashChecked`: on worked-example, `appendRejected` record 23 with one byte of
    `engineBuild` flipped after hashing (binding still holds) gives `23514` `chain_records_hash_check`.
  - `TestChainRecordsLinked`: a wrong `prev_hash` gives `23503` `chain_records_prev_fkey`; a gap gives
    `23503`; a duplicate sequence (fork) gives `23505`; a non-zero genesis `prev_hash` gives `23514`.
  - `TestChainRecordsRequireAdmission`: record `k` without admission `k` gives `23503`
    `chain_records_admission_fkey`.
  - `TestChainRecordsBindAdmissionAndColumns`: each of these gives `OX008`:
    - effective time off by one;
    - command bytes differing in one byte;
    - another command ID;
    - an accepted column with rejected bytes, and the reverse;
    - a different reason;
    - `round_id` or `sequence` in the bytes differing from the columns;
    - a genesis whose versions differ from `rounds`.
  - `TestChainRecordsResultShape`: genesis with a result, rejected without a reason, accepted with a
    reason, and reason `Bad` each give `23514`.
  - `TestChainRecordsRecordCheck`: a record containing `0x0A` inside `engineBuild` gives `23514`
    `chain_records_record_check`, and so does a 64 MiB + 1 record (padded `engineBuild`). Both have a
    correct hash.
  - `TestChainRecordsAppendOnly`: `UPDATE`, `DELETE` and `TRUNCATE` give `OX001`.
- Execution heads and fencing:
  - `TestExecutionCompareAndSet` (concurrency):
    1. two transactions run `UPDATE … WHERE processed_sequence = 22 AND fencing_token = 0`;
    2. the second blocks (`WaitUntilBlocked`);
    3. the first commits with record 23;
    4. the second's update affects 0 rows.
  - `TestChainRecordsRequireCurrentFencingToken`:
    - after a takeover (token 1, owner `engine-b`), inserting record 23 with token 0 fails at commit
      with `OX004`;
    - a record without a head advance fails at commit with `OX004`;
    - token 1 succeeds.
  - `TestExecutionHeadsForwardOnly`: a lower `processed_sequence`, a lower token, an owner change with
    the same token, and owner release without a token increase each give `OX005`; a takeover with
    token + 1 and a lease renewal succeed.
- Results, events and snapshots:
  - `TestCommandResultsView`: worked-example gives 22 rows with the manifest's accepted and rejected
    counts; a new pending admission does not appear.
  - `TestDomainEventsContiguous`:
    - an event-sequence gap gives `23503` `domain_events_prev_fkey`;
    - an unknown record gives `23503` `domain_events_record_fkey`;
    - type `betPlaced` gives `23514`;
    - `UPDATE` gives `OX001`.
  - `TestRoundSnapshots`: an unknown record gives `23503`; a 31-byte hash gives `23514`; an empty
    `engine_build` gives `23514`; `UPDATE` gives `OX001`.
- Ledger:
  - `TestLedgerAccounts`:
    - `23514` cases: a player with a name; a system account without a name; a player with
      `floor_enforced = false`;
    - unique violations: a duplicate `player_id`; a duplicate global system name;
    - allowed: the same name scoped to a round;
    - `OX002` cases: an insert with non-zero totals; a direct `UPDATE` of `credits_posted`;
    - `DELETE` gives `OX001`.
  - `TestLedgerPostingsMaintainTotals`:
    - a grant moves faucet → alice 1,000;
    - a round posting moves alice → round pot 100, referencing event 1;
    - the totals are as expected;
    - Σ `debits_posted` = Σ `credits_posted` = Σ `amount`.
  - `TestLedgerFloorEnforced`: an alice overspend gives `23514` `ledger_accounts_floor_check`; a system
    account may go negative.
  - `TestLedgerPostingValidation`:
    - `23514` cases: amount 0; amount −1; the same account on both sides;
    - `23503` cases: an `event_sequence` of another round; a posting `round_id` different from its
      transaction's.
  - `TestLedgerOneTransactionPerRecord`: a second ledger transaction for the same `(round_id,
    sequence)` gives `23505` `ledger_transactions_record_key`.
  - `TestLedgerHolds`:
    - a hold reduces available credits;
    - a hold beyond available gives `23514`;
    - a release frees credits;
    - a second release gives `23505`;
    - a release before the record exists gives `23503` `ledger_hold_releases_record_fkey`.
  - `TestLedgerConcurrentCrossRoundHolds`:
    1. alice has 150 credits;
    2. two rounds each try to hold 100 in concurrent transactions;
    3. the second blocks (`WaitUntilBlocked`) until the first commits, then fails with `23514`
       `ledger_accounts_floor_check`;
    4. finally `held = 100`.
  - `TestLedgerAppendOnly`: transactions, postings, holds and releases give `OX001`.
- Outbox:
  - `TestOutbox`:
    - a duplicate dedupe key gives `23505`;
    - updating `attempts`, `next_attempt_at`, `last_error` and `published_at` succeeds;
    - updating `payload` or `topic`, decreasing `attempts`, or changing a published row gives `OX006`;
    - deleting an unpublished row gives `OX006`; deleting a published row succeeds;
    - `TRUNCATE` gives `OX001`;
    - an unknown record gives `23503`.

### Step 5: `internal/persistence/eventstore` (P-24, §7)

- `export_test.go` (unit, `package eventstore`):
  - `TestEncodeHeaderMatchesGolden`: for each of the four LBT-9 golden histories, parse line 1 and
    rebuild it with `encodeHeader`. The result must be byte-identical.
  - `TestEncodeHeaderRejectsBadSecret`: secret bytes that are not canonical, not an object, or empty
    give `ErrCorrupt`.
- `export_integration_test.go`:
  - **`TestExportGoldenHistories`**: each of the four golden histories is loaded with `LoadHistory` and
    exported.
    - The output is byte-identical to the file.
    - `verification.Verify(ModeVerify)` passes.
    - `Export` counts and the head equal the LBT-9 `manifest.json`.
  - **`TestExportStopsAtLatestSnapshot`**: load worked-example with checkpoints `[0, 9]`.
    - The output is the header (checkpoints 0 and 9, `head = hash_9`) plus golden lines 2–11.
    - `Verify(ModeVerify)` passes.
  - **`TestExportRoundNotFound`**, and **`TestExportNoCheckpoint`** (checkpoints `[]`).

### Step 6: `cmd/crashctl migrate` (P-6)

- `migrate_test.go` (unit):
  - `TestMigrateUsage`: exit 2 for:
    - `migrate`, `migrate sideways`, `migrate up extra`;
    - `migrate down` without `-to`, `-to -1`, `-to x`, `-to 99`;
    - `ORVEX_DATABASE_URL` unset, and `ORVEX_DATABASE_URL=::bad`.
  - `TestMigrateConnectError`: `postgres://u:p@127.0.0.1:1/db?connect_timeout=2` gives exit 4.
  - `TestMigrateNeverPrintsPassword`: in both previous cases with password `sentinel-pw`, stderr never
    contains it.
- `migrate_integration_test.go`:
  - **`TestMigrateCLI`**:
    1. `up` exits 0 and prints six `applied` lines and `schema version 6`;
    2. `status` exits 0 and prints six `applied` lines;
    3. `down -to 0` exits 0;
    4. `up` again exits 0;
    5. after `LoadHistory`, `down -to 0` exits 1 with `OX003` in stderr;
    6. after inserting bookkeeping version 99, `status` and `up` exit 3.
- Existing `TestRun` rows stay as LBT-9 left them. Add a usage row naming `migrate`.

### Step 7: build, CI, docs

Write the Makefile, CI, `.golangci.yml`, Compose, README, AGENTS.md and `docs/architecture/persistence.md`
edits from the files table.

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
docker compose -f deployments/docker/compose.yaml up -d --wait
ORVEX_DATABASE_URL='postgres://orvex:orvex@127.0.0.1:55432/orvex?sslmode=disable' ./bin/crashctl migrate up
ORVEX_DATABASE_URL='postgres://orvex:orvex@127.0.0.1:55432/orvex?sslmode=disable' ./bin/crashctl migrate status
ORVEX_DATABASE_URL='postgres://orvex:orvex@127.0.0.1:55432/orvex?sslmode=disable' ./bin/crashctl migrate down -to 0
docker compose -f deployments/docker/compose.yaml down -v
```

- `git diff --stat` must show no change to `docs/adr/`, `internal/game/`, `internal/audit/`,
  `internal/canonjson/` or any golden fixture.
- If a golden export differs from its file, find which side contradicts this spec or LBT-9. Never edit a
  fixture or a §5.3 template to match output.

## 12. Out of scope and open items

Out of scope:
- in this run: any Go, SQL, migration, Compose, CI or `go.mod` change;
- the logic of LBT-11 (sequencer, window, closure) and LBT-12 (execution commit, posting rules);
- ownership leases (LBT-14), Kubernetes, load testing (LBT-15);
- the outbox relay;
- encryption of secrets;
- a `crashctl audit export` command;
- backup/restore tests;
- partitioning.

Open for the engineer:
1. `crashctl` as the migration CLI host (P-6, §2 item 3) instead of a fourth binary.
2. Unencrypted secret config at rest (P-25). If encryption is wanted before LBT-12, it needs its own
   issue.
3. The export ends at the newest snapshot (P-24). That makes snapshot rows at creation and settlement
   an obligation on LBT-12 (P-20).
4. Round-creation ownership: proposed LBT-12 (section 9).
5. The chain v1 layout written into database constraints (P-16, `OX008`): stronger integrity, but chain
   v2 then needs a migration.
6. Global `command_id` uniqueness (P-15), stronger than LBT-9's per-round rule.
7. The Compose file now (P-9) or deferred to `make dev`.
8. The ledger model (P-21): two-sided postings, holds at admission and trigger-maintained totals.
9. Follow-up issue once LBT-12 writes rows: `crashctl audit export ROUND_ID` wrapping
   `eventstore.ExportHistory`.
