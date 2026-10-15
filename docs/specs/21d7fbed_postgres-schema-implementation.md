# LBT-10 implementation plan: PostgreSQL schema and migration tooling

Run `21d7fbed`. Size **L**: a new schema with triggers and deferred constraints, concurrency tests, a
migration runner, a CLI subcommand, two new modules and CI changes.

## 0. Source of truth and how to read this plan

- The normative source is the approved spec. Step 0 adds it to the repo as
  `docs/specs/17380a6e_postgresql-schema-migrations.md`. Its source copy for this run is
  `/home/juano/Workspaces/LBTWorks/Orvex/odin/data/sessions/21d7fbed/context_handoff/lbt-10-approved-spec.md`
  (sha256 `b97dcae13e366508fc3e247d37440a5a0dcac86d2c79920b02e922b480376f97`). It is byte-identical to
  `/home/juano/Workspaces/LBTWorks/Orvex/requests/lbt-10-approved-spec.md`, so read whichever path you
  can reach.
- The engineer approved P-1 to P-26 as written, with no edits, and accepts every point in spec §2.
  Spec §0's "make no repository changes" no longer applies. Spec §11 is the implementation plan.
- This plan fixes the order of work, resolves the gaps found while planning (§2 "Refinements") and lists
  every required test (§11 checklist). Everything else follows the spec **verbatim**: table and column
  names, constraint names, trigger and function names, SQLSTATEs, API behavior and CLI output.
  Spec §§5–8 are normative. Section references like "spec §5.3" mean the approved spec.
- Do not deviate silently. If the spec turns out to be wrong in a way §2 below does not cover (an
  expected SQLSTATE or constraint name that PostgreSQL cannot produce, for example), stop and report
  it with evidence: the failing statement and the actual `PgError`. Never edit a golden fixture or a
  spec §5.3 byte template to match output.

## 1. Preconditions (verified while planning, 2026-10-14)

- The worktree is at `f7a8aed`, on `main`. **LBT-9 is merged**: `internal/canonjson/`,
  `internal/audit/chain/`, `internal/audit/verification/` and
  `internal/audit/verification/testdata/golden/worked-example.jsonl` exist. Not blocked.
- Environment:
  - go1.26.5 and golangci-lint 2.14.0;
  - Docker 29.8.2 (API 1.56). `postgres:18.6-trixie@sha256:74935e72241653ca55e0414067e6d8763aceb8a810eb51b452253ec3dcfc4336`
    and `testcontainers/ryuk:0.14.0` are already pulled;
  - `github.com/jackc/pgx/v5@v5.11.0` and `github.com/testcontainers/testcontainers-go@v0.44.0` are in
    the module cache.
  - testcontainers v0.44.0 has `testcontainers.Run`, `WithEnv`, `WithCmd`, `WithTmpfs`,
    `WithExposedPorts`, `WithWaitStrategy` and `TerminateContainer`, plus `wait.ForAll`,
    `wait.ForLog(...).WithOccurrence(2)` and `wait.ForListeningPort`.
- **Merged LBT-9 names to use** (spec §0: "use the merged names"):
  - `chain`:
    - `chain.Decode(line) (any, error)` returns a `chain.Genesis` or a `chain.Command`;
    - `chain.LinkHash(record) chain.Hash`, where `chain.Hash` is `[32]byte` with `.String()`;
    - constants `chain.Version` and `chain.RecordTag`;
    - `chain.EncodeGenesis` and `chain.EncodeCommand`;
    - field names: `Genesis{RoundID, Sequence, PrevHash, Game, RulesVersion, SchemaVersion,
      StateVersion, Config, CreatedAtUnixMicro, EngineBuild}`, `Command{RoundID, Sequence, PrevHash,
      CommandID, EffectiveTimeUnixMicro, EngineBuild, Payload, Result{Rejected, Reason},
      Events []Event{Sequence, Payload}}`.
  - `canonjson`: `Marshal`, `Unmarshal`, `Object`/`Array`/`String`, `Uint`/`ParseUint`, `Hex`/`ParseHex`,
    `UUID`/`ParseUUID`, `Text`, `Obj`, `List`.
  - `verification`:
    - `verification.Format == "orvex/audit-history/v1"` exists, so eventstore uses it and declares no
      `HistoryFormat`;
    - `verification.MaxLineBytes` is `64 << 20`;
    - `verification.Verify(r, verification.Options{Mode: verification.ModeVerify, Registry: reg})`,
      with `reg, err := game.NewRegistry(crashv1.Adapter())`.
- Golden histories (`internal/audit/verification/testdata/golden/manifest.json`):

  | History | Records | Checkpoints |
  |---|---|---|
  | worked-example | 23 | 0, 9, 22 |
  | instant-crash | 6 | 5 |
  | empty-round | 4 | 3 |
  | in-progress | 4 | 0, 3 |

  - Round IDs `0190f3a8-7b3c-7d4e-8f00-00000000000{1..4}` are distinct, and no command ID repeats
    across files. Several goldens can therefore load into one database. Tests that need two rounds load
    worked-example and instant-crash.
  - Events have the shape `{"event":{…,"type":"BetPlaced"},"eventSequence":"1"}`.
- Prototyped on the pinned image while planning:
  - A plain `TRUNCATE` of a table that another table references by foreign key fails with **`0A000`**
    *before* any `BEFORE TRUNCATE` trigger runs. `TRUNCATE … CASCADE` reaches the trigger and raises
    `OX001`.
  - `CHECK` constraints are evaluated in constraint-name order.
  - `CREATE DATABASE x ENCODING 'SQL_ASCII' TEMPLATE template0` succeeds as the superuser `orvex`. A
    UTF8 client connects to it, and the OX007 `DO` block raises there.
  - The `MigrationLockKey` derivation was checked: the first 8 bytes of SHA-256("orvex/migrate") read
    as a big-endian int64 give `7766444933939532367`.
- pgx v5.11.0 details:
  - `ConnectError.Error()` prints the user and database but never the password, and it can span
    several lines.
  - `ParseConfigError.Error()` echoes a redacted DSN, so the CLI must print only
    `invalid ORVEX_DATABASE_URL`.
  - `Exec` with no arguments uses the simple protocol (`conn.go:524`).

## 2. Refinements of the approved spec (no ADR conflict)

No change contradicts an ADR or any approved P-, A- or D-decision. Each one fixes test mechanics or an
identifier the spec left open, or is needed for the spec's own tests to compile and pass. Copy this list
into the PR description, after the spec §2 items 1–15. AGENTS.md "Flag conflicts explicitly" applies,
and there are no ADR conflicts to flag.

- **R-1. One `TestMain` per test package.** Spec §11 says every integration test file gets its own
  `TestMain`, but Go allows one per test binary, and `internal/persistence/postgres` has two
  integration files in `package postgres_test`. `TestMain` (calling `os.Exit(pgtest.Run(m))`) lives
  only in:
  - `internal/persistence/postgres/migrate_integration_test.go`;
  - `internal/persistence/eventstore/export_integration_test.go`;
  - `cmd/crashctl/migrate_integration_test.go`.

  `schema_integration_test.go` has none.
- **R-2. TRUNCATE assertions on referenced tables use `CASCADE`.** PostgreSQL raises `0A000` for a
  plain `TRUNCATE` of a table that another table references, before triggers fire (§1). The trigger
  design (P-13) is unchanged. In the tests:
  - The shared helper `requireTruncateRejected(t, pool, table)` runs `TRUNCATE orvex.<table> CASCADE`,
    requires `OX001` and checks that the table's row count is unchanged.
  - `TestChainRecordsAppendOnly` additionally asserts that a plain `TRUNCATE orvex.chain_records` fails
    with `0A000`.
  - `TestOutbox` keeps a plain `TRUNCATE` → `OX001`, because `outbox_messages` is unreferenced.
  - New `TestTruncateRejectedEverywhere` runs the helper for every table in schema `orvex`, read from
    `pg_tables`, so no table can miss its `_no_truncate` trigger.

  `docs/architecture/persistence.md` states the `0A000` behavior.
- **R-3. The binding trigger leaves missing or malformed inputs to the constraints.** `BEFORE INSERT`
  triggers run before `NOT NULL`, `CHECK` and foreign keys. Without this rule the spec's tests
  `TestChainRecordsRequireAdmission` (`23503`), `TestChainRecordsResultShape` (`23514`) and the
  non-zero-genesis case of `TestChainRecordsLinked` (`23514`) would get `OX008`.
  - `orvex.check_record_binding()` returns `NEW` **without raising** when:
    - `NEW.round_id`, `NEW.sequence`, `NEW.record` or `NEW.prev_hash` is null;
    - `NEW.sequence < 0`;
    - no `orvex.rounds` row exists;
    - `sequence = k ≥ 1` and no admission `(round_id, k)` exists;
    - `k ≥ 1` and `result_status` is not `accepted` or `rejected`;
    - `k ≥ 1`, `result_status = 'rejected'` and `result_reason` is null.
  - Otherwise it builds `P` and `S` exactly as spec §5.3 and raises `OX008` on any mismatch.
  - The accepted-result template `{"status":"accepted"}` ignores `result_reason`. A stray reason is
    `chain_records_result_check`'s to report.
  - Generated columns (`prev_sequence`, `admission_sequence`) are **null inside `BEFORE` triggers**.
    Derive everything from `NEW.sequence`.
- **R-4. CLI exit 3 also covers `ErrSchemaState`.** The P-6 exit table does not mention it.
  `TestMigrateCLI` step 6 inserts bookkeeping version 99 into a database at version 4. The rows
  `{1,2,3,4,99}` fail the contiguity check (`ErrSchemaState`) before `ErrSchemaTooNew` is reached, and
  the spec expects exit 3. Map `ErrSchemaTooNew`, `ErrChecksumMismatch` and `ErrSchemaState` to 3. This
  is fail-closed, as ADR-006 requires.
- **R-5. New `pgtest.CreateDatabase(t, options string) (dsn string)`.**
  `TestMigrateRequiresUTF8` needs a non-UTF8 database, and the spec's `pgtest` API has no way to make
  one. `EmptyDatabase(t)` is `CreateDatabase(t, "")`.
- **R-6. `pgtest.Loaded` gains `Hashes []chain.Hash`**: the link hash per sequence, index =
  sequence. `LoadHistory`'s `checkpoints` argument:
  - `nil` selects every header checkpoint;
  - an empty non-nil slice selects none;
  - a sequence not in the header calls `t.Fatalf`.
- **R-7. Hash-typed fields use `chain.Hash`** (same underlying `[32]byte`): `eventstore.Export.Head`,
  `eventstore.Checkpoint.StateHash` and `pgtest.Loaded.Head`.
- **R-8. CLI errors are flattened to one line.** pgx connect errors can span several lines, and P-6
  requires one stderr line. Collapse whitespace with `strings.Join(strings.Fields(msg), " ")`.

## 3. Step 0: add the approved spec to the repo

```sh
cp /home/juano/Workspaces/LBTWorks/Orvex/odin/data/sessions/21d7fbed/context_handoff/lbt-10-approved-spec.md \
   docs/specs/17380a6e_postgresql-schema-migrations.md
sed -i '168,193s/| \[ \] |$/| [x] |/' docs/specs/17380a6e_postgresql-schema-migrations.md
```

Then replace lines 3–4, the status paragraph that begins `Status: **PROPOSED, awaiting engineer
approval.**`, with exactly these two lines:

```
Status: **APPROVED** by the engineer on 2026-10-14: decisions P-1 to P-26 as written, with no edits,
and every point of section 2 accepted. Implementation (section 11) follows under LBT-10.
```

Make no other change to the file.

Acceptance:
- `diff <source copy> docs/specs/17380a6e_postgresql-schema-migrations.md` shows only the hunks
  `3,4c3,4` and `168,193c168,193`;
- `grep -c '| \[x\] |$'` returns 26, and `grep -c '\[ \]'` returns 0.

## 4. Step 1: dependencies and migration SQL

### 4.1 Modules

- `go get github.com/jackc/pgx/v5@v5.11.0 github.com/testcontainers/testcontainers-go@v0.44.0`, then
  `go mod tidy` once code imports them.
- Only these two may appear as new **direct** requirements. If tidy adds another direct requirement,
  stop and report (spec §11 Step 1).
- `go 1.26.0` stays, and no `toolchain` line is added.
- The PR justifies each module, per AGENTS.md:
  - pgx: P-2;
  - testcontainers: P-7, used only by `pgtest` and tests.

### 4.2 `.gitattributes` (new)

```
/migrations/*.sql text eol=lf
```

### 4.3 `migrations/migrations.go` (new)

- `package migrations`, with a package doc naming ADR-005, ADR-007, LBT-10 P-3 and P-4. The doc says
  merged files are immutable and checksums enforce it.
- `//go:embed *.sql` and `var FS embed.FS`.

### 4.4 Migration files

Six pairs, `migrations/0001_foundation`, `0002_rounds_admissions`, `0003_chain_execution`,
`0004_snapshots`, `0005_ledger` and `0006_outbox`, each with `.up.sql` and `.down.sql`, per spec §5.

Rules for every file. The runner enforces the first three, and `TestEmbeddedMigrations` fails if a file
breaks them:
- Line 1 is `-- NNNN name: <purpose> (LBT-10 spec P-n)`.
- No line consisting of `BEGIN;`, `COMMIT;`, `ROLLBACK;` or starting `START TRANSACTION`
  (case-insensitive).
  - PL/pgSQL `BEGIN` without a semicolon is fine.
  - Do not write `END; $$` on a `BEGIN;`-like line either.
- The word `CONCURRENTLY` appears nowhere, not even in comments. No line starts with `\` (watch long
  bytea literals that wrap).
- Every object is schema-qualified (`orvex.…`), including inside function bodies.
- Every function is `LANGUAGE plpgsql` with `SET search_path = pg_catalog`.
- Every generated column is `GENERATED ALWAYS AS (…) STORED`.
- Every table and the view get a one-sentence `COMMENT ON TABLE`/`COMMENT ON VIEW` naming its ADR. The
  schema gets a `COMMENT ON SCHEMA`.
- Constraint names are exactly as spec §5 gives them. Unnamed ones follow P-11:
  `<table>_<what>_{pkey|key|fkey|check}`, for example `chain_records_sequence_check` and
  `chain_records_fencing_token_check`.
  - Remember that `CHECK`s run in name order. Spec §11's "change exactly one input" tests rely on each
    other check passing.
- Trigger names:
  - `<table>_append_only`: `BEFORE UPDATE OR DELETE … FOR EACH ROW EXECUTE FUNCTION
    orvex.forbid_mutation()`;
  - `<table>_no_truncate`: `BEFORE TRUNCATE … FOR EACH STATEMENT EXECUTE FUNCTION
    orvex.forbid_mutation()`, on **every** table in `orvex`;
  - `<table>_no_delete`: `BEFORE DELETE … FOR EACH ROW EXECUTE FUNCTION orvex.forbid_mutation()`, on
    `round_admission_heads`, `round_execution_heads` and `ledger_accounts`, which are mutable but
    undeletable;
  - the named triggers of spec §5: `round_admission_heads_forward`, `round_execution_heads_forward`,
    `chain_records_binding`, `chain_records_fenced`, `ledger_accounts_guard`, `ledger_postings_apply`,
    `ledger_holds_apply`, `ledger_hold_releases_apply` and `outbox_messages_guard`.
- Down files:
  - First, one `DO` block that raises `OX003` with message
    `down migration refused: orvex.<table> holds rows` for the first table it drops that has a row
    (`IF EXISTS (SELECT 1 FROM orvex.<t>)`).
  - Then drop the view, the tables (which drops their triggers) and the functions, in reverse
    dependency order.
  - `0001` down: `DROP FUNCTION orvex.forbid_mutation()`, then `DROP SCHEMA orvex` (default
    `RESTRICT`).

Per-file notes. Spec §5 and §5.8 list every column and constraint; these are the points that need
care:
- **0001**:
  - The first statement is the `DO` block raising `OX007` with message `orvex requires a UTF8 database`
    when `current_setting('server_encoding') <> 'UTF8'`.
  - Then `CREATE SCHEMA orvex` and `orvex.forbid_mutation()`. It raises `OX001` with
    `orvex.<TG_TABLE_NAME> is append-only: <TG_OP> rejected` and serves both row-level and
    statement-level triggers.
- **0002**: per spec §5.2.
  - `orvex.check_admission_head()` (`OX005`): `round_id` changed or `admitted_sequence` decreased.
  - `round_admission_heads_head_fkey` is `DEFERRABLE INITIALLY DEFERRED`.
- **0003**: per spec §5.3, with R-3 for the binding.
  - Compare bytes with `substring(NEW.record FROM 1 FOR octet_length(p)) = p` and
    `substring(NEW.record FROM octet_length(NEW.record) - octet_length(s) + 1) = s`, after the length
    guard `octet_length(record) >= octet_length(p) + octet_length(s)`.
  - Convert text with `convert_to(…, 'UTF8')`, **never** `::bytea` on text.
  - Formatting: `uuid::text`, `bigint::text`, `encode(prev_hash, 'hex')`.
  - Write the zero genesis hash as `decode(repeat('00', 32), 'hex')`, or keep a `'\x00…'` literal on
    the same line as its keyword.
  - `orvex.check_record_fenced()`:
    - `SELECT fencing_token, processed_sequence FROM orvex.round_execution_heads WHERE round_id =
      NEW.round_id FOR SHARE`;
    - a missing row, a token ≠ `NEW.fencing_token`, or `processed_sequence < NEW.sequence` → `OX004`.
  - Create `chain_records_fenced` with `CREATE CONSTRAINT TRIGGER … AFTER INSERT ON orvex.chain_records
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW`, after both tables exist.
  - `orvex.check_execution_head()` enforces the P-19 rules (`OX005`):
    - `round_id` is immutable;
    - `processed_sequence` and `fencing_token` never decrease;
    - `owner_id IS DISTINCT FROM OLD.owner_id` requires `fencing_token > OLD.fencing_token`.
  - View `orvex.command_results` exactly as spec §5.3.
- **0004**: per spec §5.4.
- **0005**: per spec §5.6.
  - **Ledger totals are updated relatively** (`SET held = held + NEW.amount`,
    `SET debits_posted = debits_posted + NEW.amount`), never computed from an earlier `SELECT`.
    READ COMMITTED re-evaluates a blocked `UPDATE` on the newest row version. That is what makes
    `TestLedgerConcurrentCrossRoundHolds` fail the second hold with `23514`.
  - `apply_ledger_posting` updates the account with the lower `account_id` first: two `UPDATE`s
    ordered by `uuid` comparison.
  - `apply_ledger_hold_release` subtracts the hold's `amount` it looks up by primary key.
  - `guard_ledger_account` (`OX002`):
    - on insert, all three totals must be 0;
    - on update, `pg_trigger_depth() >= 2` is required, and every non-total column must be
      `IS NOT DISTINCT FROM` its old value.
  - `ledger_accounts_system_name_key` is a **partial unique index** `ON orvex.ledger_accounts
    (round_id, name) NULLS NOT DISTINCT WHERE kind = 'system'`, not a table constraint (spec §1).
- **0006**: per spec §5.7.
  - `guard_outbox_message` (`OX006`):
    - an `UPDATE` changing `message_id`, `round_id`, `sequence`, `message_index`, `topic`, `payload`
      or `created_at` is rejected;
    - so is one that lowers `attempts`, or that changes anything when `OLD.published_at IS NOT NULL`;
    - a `DELETE` with `OLD.published_at IS NULL` is rejected.
  - On a permitted `DELETE` the guard returns `OLD`, and on `UPDATE` it returns `NEW`.

## 5. Step 2: `internal/persistence/postgres` (spec §8, P-11, P-23)

Files:
- `doc.go`: the package doc. It summarizes the tables, the isolation table of P-23 and the error
  classes of spec §6 item 4, and points to `docs/architecture/persistence.md`.
- `migrate.go`: `Migration`, `Beginner`, `MigrationState`, `MigrationLockKey`, `LoadMigrations`,
  `MigrateUp`, `MigrateDown`, `MigrationStatus` and the six sentinel errors, exactly as spec §8.
- `errors.go`: the eight `Code*` constants of P-11, `SQLState(err) string` (uses `errors.As` with
  `*pgconn.PgError`) and `IsRetryable(err) bool` (`40001`, `40P01`).
- `tx.go`: `AdmissionTxOptions()` and `ExecutionTxOptions()` return
  `pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}`. `SnapshotReadTxOptions()`
  returns `{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}`.

Behavior beyond spec §8 text:
- `LoadMigrations`:
  - reads the root of `fsys` and ignores entries that are not regular `.sql` files;
  - a `.sql` name failing `^([0-9]{4})_([a-z][a-z0-9_]*)\.(up|down)\.sql$` is `ErrMigrationFiles`;
  - applies every P-4 rule (contiguous from 0001, one name per version, both files, the content
    rules);
  - wraps errors as `fmt.Errorf("%w: <file>: <reason>", ErrMigrationFiles)`;
  - sets the checksums to `sha256.Sum256` of the raw bytes.
- Bookkeeping DDL (P-4) runs after the lock in every step: `CREATE SCHEMA IF NOT EXISTS orvex_migrate`
  and `CREATE TABLE IF NOT EXISTS orvex_migrate.schema_migrations(…)`, with
  `schema_migrations_up_sha256_check`/`…_down_sha256_check` (`octet_length = 32`).
- `MigrateUp` and `MigrateDown`:
  - loop one transaction per step, as spec §8 lays out;
  - begin each step with `db.Begin(ctx)`, which is READ COMMITTED by default; the first statement is
    `SELECT pg_advisory_xact_lock($1)` with `MigrationLockKey`;
  - run the file with `tx.Exec(ctx, sql)`, no arguments;
  - on error, roll back and return the **already completed** steps together with the error
    (`TestMigrateDownRefusesAuditData` and the CLI rely on this).

  Error text: `postgres: migration 0003 chain_execution up: %w`, which keeps `*pgconn.PgError`
  reachable through `errors.As`.
- `MigrationStatus`:
  - takes no lock and runs in `SnapshotReadTxOptions()`;
  - treats `to_regclass('orvex_migrate.schema_migrations') IS NULL` as version 0;
  - otherwise applies the same validation as a step;
  - returns one `MigrationState` per known migration.

Unit tests (untagged, `package postgres`):
- `migrate_test.go`:
  - **`TestLoadMigrations`**: the full table of spec §11 Step 2, using `fstest.MapFS`.
  - **`TestEmbeddedMigrations`**: per spec §11 Step 2.
  - **`TestMigrationLockKey`**: `MigrationLockKey` equals the big-endian int64 of
    `sha256("orvex/migrate")[:8]` (pins P-4's derivation).
- `errors_test.go`: **`TestSQLState`** and **`TestIsRetryable`**, per spec §11.
- `tx_test.go`: **`TestTxOptions`**, the three values above.

## 6. Step 3: `internal/persistence/postgres/pgtest` (P-7, spec §7.3)

Untagged package, compiled by `make test` and `make lint`. Files `pgtest.go`, `fingerprint.go` and
`load.go`. The API is spec P-7 plus R-5 and R-6:

```go
const Image = "postgres:18.6-trixie@sha256:74935e72241653ca55e0414067e6d8763aceb8a810eb51b452253ec3dcfc4336"
func Run(m *testing.M) int
func CreateDatabase(t testing.TB, options string) (dsn string)  // R-5
func EmptyDatabase(t testing.TB) (dsn string)
func MigratedDatabase(t testing.TB) (dsn string)
func Pool(t testing.TB, dsn string) *pgxpool.Pool
type Querier interface{ Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) }
func Fingerprint(ctx context.Context, db Querier, exclude ...string) (string, error)
type Loaded struct{ RoundID [16]byte; Head chain.Hash; Records int; CommandIDs [][16]byte; Hashes []chain.Hash }
func LoadHistory(t testing.TB, db *pgxpool.Pool, history []byte, checkpoints []uint64) Loaded
func WaitUntilBlocked(t testing.TB, db *pgxpool.Pool, pid uint32)
```

Implementation notes:
- `Run`:
  - starts the container with `testcontainers.Run(ctx, Image, …)` and these options:
    - `WithEnv` setting `POSTGRES_USER`, `POSTGRES_PASSWORD` and `POSTGRES_DB` to `orvex`;
    - `WithCmd("postgres", "-c", "fsync=off")`;
    - `WithTmpfs(map[string]string{"/var/lib/postgresql": "rw"})`;
    - `WithExposedPorts("5432/tcp")`;
    - `WithWaitStrategy(wait.ForAll(wait.ForLog("database system is ready to accept connections").WithOccurrence(2), wait.ForListeningPort("5432/tcp")))`;
  - gives startup a generous deadline (about 3 min), which covers the first image pull in CI;
  - on any start error, prints `pgtest: integration tests need a Docker-compatible daemon: <err>` to
    stderr and returns 1. It **never skips**;
  - opens an admin pool on database `orvex`, runs `m.Run()`, then closes the pool, calls
    `testcontainers.TerminateContainer` and returns the code.
- `CreateDatabase`:
  - calls `t.Fatal` if `Run` was not called;
  - picks a unique name `orvex_test_<n>` from an atomic counter;
  - runs `CREATE DATABASE <name> <options>` on the admin pool;
  - registers `t.Cleanup` `DROP DATABASE IF EXISTS <name> WITH (FORCE)`;
  - returns `postgres://orvex:orvex@<host>:<port>/<name>?sslmode=disable`.
- `MigratedDatabase`:
  - the first call, under `sync.Once` with the error stored, creates `orvex_template`, runs
    `LoadMigrations(migrations.FS)` and `MigrateUp(…, len(ms), "pgtest")` on one `pgx.Conn`, closes
    the conn, then runs `ALTER DATABASE orvex_template ALLOW_CONNECTIONS false`;
  - each call runs `CREATE DATABASE <name> TEMPLATE orvex_template`, with the same cleanup as
    `CreateDatabase`.
- `Pool`: `MaxConns = 8`, `t.Cleanup(pool.Close)`. It is registered after the database cleanup, so it
  closes first.
- `Fingerprint`: as the spec P-7 doc comment describes.
  - One sorted line per object; never OIDs or database names.
  - Always excluded: `pg_catalog`, `information_schema`, `pg_toast` and `pg_temp_*`/`pg_toast_temp_*`,
    plus the caller's `exclude` list.
- `LoadHistory`: spec §7.3.
  - Everything runs in one transaction opened with `postgres.ExecutionTxOptions()`.
  - Insert in this order: rounds, secret, both heads at N with token 0, admissions, chain records,
    events, snapshots (`state` null, `engine_build` `golden`).
  - Admissions' `command` is `Command.Payload`; `event_type` is the payload's `type` member.
  - Commit; any error, including commit-time errors, calls `t.Fatalf`.
- `WaitUntilBlocked`: polls `SELECT wait_event_type FROM pg_stat_activity WHERE pid = $1` every
  10 ms until it is `Lock`. After 10 s it calls `t.Fatalf`.

Unit test, in `pgtest_test.go`:
- **`TestComposeImageMatchesPinnedImage`**: reads `../../../../deployments/docker/compose.yaml` and
  requires the substring `"image: " + Image`.

## 7. Step 4: integration tests in `internal/persistence/postgres`

Every file starts with `//go:build integration` and uses `package postgres_test`. Assert
`postgres.SQLState(err)` and `PgError.ConstraintName`, never message text. "At commit" means the error
comes from `tx.Commit`. Use `t.Parallel()` where a test owns its database.

### 7.1 `migrate_integration_test.go`

This file holds `TestMain` (R-1). The fingerprint excludes `orvex_migrate`. Tests, each per spec §11
Step 4:
- **`TestMigrateUpDownClean`**
- **`TestMigrateEachStepReversible`**
- **`TestMigrateUpIdempotent`**
- **`TestMigrateConcurrentRunners`**
- **`TestMigrateChecksumMismatch`**
- **`TestMigrateSchemaTooNew`**
- **`TestMigrateWrongDirection`**: at version 3, `MigrateUp(target 2)` and `MigrateDown(target 5)`
  both return `ErrWrongDirection`, and the version stays 3.
- **`TestMigrateTargetRange`**: targets −1 and `len(ms)+1` return `ErrTarget` from both `MigrateUp`
  and `MigrateDown`. No bookkeeping is created.
- **`TestMigrationFailureRollsBack`**
- **`TestMigrateDownRefusesAuditData`**: the returned `reverted` holds `0006` and `0005`.
- **`TestMigrateRequiresUTF8`**: uses
  `pgtest.CreateDatabase(t, "ENCODING 'SQL_ASCII' TEMPLATE template0")` (R-5). Expect `OX007`;
  `MigrationStatus` then reports all pending.
- **`TestMigrationStatus`**
- **`TestMigrationsCreateOnlyOrvexObjects`**:
  - after up, `public` holds no relation, function or type;
  - every table and view in `orvex` has a comment;
  - every function in `orvex` has `proconfig = '{search_path=pg_catalog}'`.

### 7.2 `schema_integration_test.go`

No `TestMain`. Test-local helpers:
- `requireCode(t, err, sqlstate, constraint string)`; an empty constraint means "don't check".
- `requireTruncateRejected(t, pool, table)` (R-2).
- `commandRecord(...)` builds command-record bytes from the spec §5.3 templates. It takes arbitrary
  `engineBuild`, result, `prevHash`, `roundId`, sequence, command bytes, command ID and effective time,
  and writes `"events":[]` unless told otherwise.
- `appendRejected(t, tx, roundID, k, prevHash, token) chain.Hash`, per spec §11:
  - command `{"authority":"player","betCredits":"100","player":"alice","type":"PlaceBet"}`;
  - reason `round_settled`, `engineBuild` `test`;
  - inserts admission k and record k, advances both heads to k, and returns the record's link hash.
- `createRound(t, tx, g chain.Genesis, edit func(*roundRow))` inserts:
  - the `rounds` row from `g`, after `edit`;
  - the secret `{"seed":"<64 hex>"}`;
  - both heads at 0;
  - the genesis record from `chain.EncodeGenesis(g)` with token 0.

  `TestChainRecordsBindAdmissionAndColumns` (genesis versions) and the round-level tests use it.

Tests, each per spec §11 Step 4 with the refinements noted:
- Rounds and admissions:
  - **`TestRoundsConstraints`**
  - **`TestRoundCreationRequiresGenesis`**
  - **`TestRoundSecretsAppendOnly`**: `UPDATE`/`DELETE` → `OX001`; plain `TRUNCATE` → `OX001`.
  - **`TestAdmissionsGapless`**
  - **`TestAdmissionsCommandIDGloballyUnique`**: loads worked-example and instant-crash, then inserts
    instant-crash admission 6 with worked-example's command 1 ID.
  - **`TestAdmissionsAppendOnly`**: `UPDATE`/`DELETE` → `OX001`; `requireTruncateRejected`.
  - **`TestRoundAdmissionHeads`**
- Chain records:
  - **`TestChainRecordsHashChecked`**
  - **`TestChainRecordsLinked`**: for the non-zero genesis case, the bytes keep the zero `prevHash`
    and only the column changes.
  - **`TestChainRecordsRequireAdmission`**
  - **`TestChainRecordsBindAdmissionAndColumns`**
  - **`TestChainRecordsResultShape`**: the record bytes match the columns wherever the template can be
    built, so only the `CHECK` fails (R-3). For reason `Bad`, both the bytes and the column say `Bad`.
  - **`TestChainRecordsRecordCheck`**
  - **`TestChainRecordsAppendOnly`**: `UPDATE`/`DELETE` → `OX001`; plain `TRUNCATE` → `0A000`;
    `requireTruncateRejected` → `OX001` (R-2).
- Execution heads and fencing:
  - **`TestExecutionCompareAndSet`**: get the blocked backend's pid from `conn.Conn().PgConn().PID()`.
  - **`TestChainRecordsRequireCurrentFencingToken`**
  - **`TestExecutionHeadsForwardOnly`**
- Results, events and snapshots:
  - **`TestCommandResultsView`**
  - **`TestDomainEventsContiguous`**
  - **`TestRoundSnapshots`**
- Ledger:
  - **`TestLedgerAccounts`**
  - **`TestLedgerPostingsMaintainTotals`**
  - **`TestLedgerFloorEnforced`**
  - **`TestLedgerPostingValidation`**: for "an `event_sequence` of another round", use worked-example
    event 9 on an instant-crash posting, since instant-crash has 5 events.
  - **`TestLedgerOneTransactionPerRecord`**
  - **`TestLedgerHolds`**
  - **`TestLedgerConcurrentCrossRoundHolds`**: loads worked-example and instant-crash.
  - **`TestLedgerAppendOnly`**: `UPDATE`/`DELETE` on transactions, postings, holds and releases →
    `OX001`; `requireTruncateRejected` on each.
- Outbox and truncation:
  - **`TestOutbox`**
  - **`TestTruncateRejectedEverywhere`** (new, R-2): every `pg_tables` row with
    `schemaname = 'orvex'` → `requireTruncateRejected`.

## 8. Step 5: `internal/persistence/eventstore` (P-24, spec §7)

- `export.go`: API per spec §7.1, with R-7:
  - `TxBeginner`, `Checkpoint{Sequence uint64; StateHash chain.Hash}`,
    `Export{RoundID [16]byte; Records int; Head chain.Hash; Checkpoints []Checkpoint}`, `ExportHistory`;
  - errors `ErrRoundNotFound`, `ErrNoCheckpoint` and `ErrCorrupt`;
  - the algorithm is spec §7.2. It uses `postgres.SnapshotReadTxOptions()`, a `bufio.Writer`, and
    `verification.Format` and `verification.MaxLineBytes`.
  - `encodeHeader(head chain.Hash, cps []Checkpoint, configSecret []byte) ([]byte, error)` is
    unexported. It returns `ErrCorrupt`, wrapped, when `configSecret` fails `canonjson.Unmarshal`.
- `export_test.go` (unit, `package eventstore`):
  - **`TestEncodeHeaderMatchesGolden`**
  - **`TestEncodeHeaderRejectsBadSecret`**
- `export_integration_test.go` (`package eventstore_test`, holds `TestMain`):
  - **`TestExportGoldenHistories`**
  - **`TestExportStopsAtLatestSnapshot`**: `head == Loaded.Hashes[9]`.
  - **`TestExportRoundNotFound`**
  - **`TestExportNoCheckpoint`**: `LoadHistory(…, []uint64{})`.

  Golden dir: `../../audit/verification/testdata/golden`.

## 9. Step 6: `crashctl migrate` (P-6)

- `cmd/crashctl/main.go`:
  - The package comment becomes "replay/audit and operations CLI". It names `crashctl migrate` and
    cites the LBT-10 spec P-6.
  - The `usage` const gains these lines:

    ```
           crashctl migrate up [-to N]
           crashctl migrate down -to N
           crashctl migrate status
    ```

    plus a final line
    `ORVEX_DATABASE_URL names the database for migrate (a PostgreSQL URL or key=value connection string).`
  - `run` dispatches `case !*showVersion && fs.Arg(0) == "migrate": return runMigrate(fs.Args()[1:], stdout, stderr)`.
  - The existing audit behavior is unchanged.
- `cmd/crashctl/migrate.go`:
  1. Parse the subcommand (`up`, `down` or `status`) and its flags with a `flag.FlagSet`.
     - `-to` must be canonical non-negative decimal and `≤ len(ms)` from
       `postgres.LoadMigrations(migrations.FS)`.
     - `down` requires `-to`; `status` takes no flags or arguments; extra arguments are rejected.
  2. Read `ORVEX_DATABASE_URL`. If it is unset, exit 2. If `pgx.ParseConfig` fails, print only
     `invalid ORVEX_DATABASE_URL` and exit 2.
  3. If `ConnectTimeout == 0`, set it to 10 s. Then `pgx.ConnectConfig`; a failure exits 4.
  4. Run. On success, print the `applied NNNN name`/`reverted NNNN name` lines, or for `status`
     `NNNN name applied <RFC 3339 UTC> by <build>`/`NNNN name pending`, then `schema version N`.
     - On failure, print the steps completed before the failure, then one stderr line
       `crashctl: migrate <sub>: <flattened error>` (R-8). No `schema version` line.
     - A stdout write failure exits 4.

  Exit mapping is the helper `migrateExitCode(err) int`:
  - `ErrTarget`, `ErrWrongDirection` → 2;
  - `ErrSchemaTooNew`, `ErrChecksumMismatch`, `ErrSchemaState` → 3 (R-4);
  - anything else → 1.

  Connect failures return 4 at the connect call. `appliedBy` is `buildinfo.Version`. The DSN and
  password are never printed.
- `cmd/crashctl/migrate_test.go` (unit):
  - **`TestMigrateUsage`**, per spec §11 Step 6.
    - For the argument cases, set `ORVEX_DATABASE_URL` to the unreachable
      `postgres://u:p@127.0.0.1:1/db?connect_timeout=2`. A missing argument check then shows up as
      exit 4, not 2.
    - Also covers `migrate status extra`.
  - **`TestMigrateConnectError`**: exit 4.
  - **`TestMigrateNeverPrintsPassword`**: password `sentinel-pw` in both the unparsable DSN
    `postgres://u:sentinel-pw@127.0.0.1:1/db?connect_timeout=x` and the unreachable
    `postgres://u:sentinel-pw@127.0.0.1:1/db?connect_timeout=2`. stderr never contains `sentinel-pw`.
  - **`TestMigrateExitCodes`** (new): table over `migrateExitCode` with each sentinel wrapped by
    `fmt.Errorf("…: %w", …)` and `&pgconn.PgError{Code: "OX003"}` → 1.
- `cmd/crashctl/main_test.go`: **`TestRun`** gains one row,
  `{name: "usage lists migrate", args: nil, wantCode: 2, wantStderr: "crashctl migrate up [-to N]"}`.
  Leave the other rows as they are.
- `cmd/crashctl/migrate_integration_test.go` (`//go:build integration`, `package main`, holds
  `TestMain`): **`TestMigrateCLI`**, steps 1–6 of spec §11 Step 6.
  - Use `t.Setenv` with `pgtest.EmptyDatabase(t)`.
  - Step 6 inserts `(99, 'future', 32 zero bytes, 32 zero bytes, 'test')` into
    `orvex_migrate.schema_migrations`. It then expects exit 3 from both `status` and `up`.

## 10. Step 7: build, CI, Compose, docs

- `Makefile`:
  - In the header comment, drop `test-integration` from the "add … when meaningful" list.
  - Add the target from spec P-8:

    ```make
    # PostgreSQL integration tests (ADR-006): Testcontainers, needs a Docker daemon.
    test-integration:
    	$(GO) test $(GOTESTFLAGS) -tags integration -count=1 ./...
    ```

  - Add `test-integration` to `.PHONY`.
- `.github/workflows/ci.yml`: add the `integration` job exactly as spec P-8 shows it. The other jobs
  are unchanged.
- `.golangci.yml`: add `run:` → `build-tags: [integration]`. Nothing else changes.
- `deployments/docker/compose.yaml` (new), per P-9:
  - top-level `name: orvex`;
  - service `postgres` with `image: <pgtest.Image>`;
  - env `POSTGRES_USER`/`POSTGRES_PASSWORD`/`POSTGRES_DB` set to `orvex`;
  - `ports: ["127.0.0.1:55432:5432"]`;
  - a named volume `postgres-data` at `/var/lib/postgresql`;
  - healthcheck `["CMD", "pg_isready", "-U", "orvex", "-d", "orvex"]`, `interval: 2s`, `retries: 30`;
  - a header comment saying the image must equal `pgtest.Image`, enforced by
    `TestComposeImageMatchesPinnedImage`.
- `docs/architecture/persistence.md` (new). Sections, per spec §11's files table:
  1. tables and invariants;
  2. authoritative versus derived data (`chain_records.record` is authoritative; `domain_events`, the
     result columns and the view are derived and bound);
  3. a constraint catalogue with SQLSTATEs (`OX001`–`OX008` and the named constraints tests rely on);
  4. spec §6 isolation, lock order and the error classes;
  5. migration policy (P-4, P-5, checksums, immutability);
  6. seed exposure (P-25);
  7. retention (P-26);
  8. the grant matrix (P-13);
  9. the ADR-001 limitation (spec §2 item 8);
  10. the TRUNCATE note (R-2);
  11. local use (Compose and `crashctl migrate`).

  Link the spec and ADR-001/002/005/006.
- `README.md`, per spec §11's files table:
  - Status: add LBT-10 and change "No runtime, persistence or transport exists yet." to "No runtime or
    transport exists yet."
  - Layout: add `migrations/`, `internal/persistence/postgres` (+ `pgtest`),
    `internal/persistence/eventstore`, `deployments/docker` and `docs/architecture`; update
    `cmd/crashctl` to mention `migrate`.
  - Development: add Docker as a prerequisite for `make test-integration`, the target itself,
    `crashctl migrate`, and the three Compose/DSN commands of P-9.
- `AGENTS.md`, per spec §11's files table:
  - Commands gains `make test-integration` (needs a Docker daemon; `GOTESTFLAGS=` override as for
    `make test`).
  - Repository conventions gains "Merged migrations in `migrations/` are immutable; change the schema
    with a new migration (checksums are enforced)."
  - The hand-off line also requires `make test-integration` when `migrations/` or
    `internal/persistence/` changes.

## 11. Required test checklist

Every name below must exist and pass. Unit tests run under `make test`; the rest run under
`make test-integration`. Names marked (new) come from this plan's refinements; all others come from
spec §11.

- `internal/persistence/postgres` (unit): `TestLoadMigrations`, `TestEmbeddedMigrations`,
  `TestMigrationLockKey` (new), `TestSQLState`, `TestIsRetryable`, `TestTxOptions`.
- `internal/persistence/postgres/pgtest` (unit): `TestComposeImageMatchesPinnedImage`.
- `internal/persistence/postgres` (integration, migrations): `TestMigrateUpDownClean`,
  `TestMigrateEachStepReversible`, `TestMigrateUpIdempotent`, `TestMigrateConcurrentRunners`,
  `TestMigrateChecksumMismatch`, `TestMigrateSchemaTooNew`, `TestMigrateWrongDirection`,
  `TestMigrateTargetRange`, `TestMigrationFailureRollsBack`, `TestMigrateDownRefusesAuditData`,
  `TestMigrateRequiresUTF8`, `TestMigrationStatus`, `TestMigrationsCreateOnlyOrvexObjects`.
- `internal/persistence/postgres` (integration, schema):
  - rounds and admissions: `TestRoundsConstraints`, `TestRoundCreationRequiresGenesis`,
    `TestRoundSecretsAppendOnly`, `TestAdmissionsGapless`, `TestAdmissionsCommandIDGloballyUnique`,
    `TestAdmissionsAppendOnly`, `TestRoundAdmissionHeads`;
  - chain records: `TestChainRecordsHashChecked`, `TestChainRecordsLinked`,
    `TestChainRecordsRequireAdmission`, `TestChainRecordsBindAdmissionAndColumns`,
    `TestChainRecordsResultShape`, `TestChainRecordsRecordCheck`, `TestChainRecordsAppendOnly`;
  - heads and fencing: `TestExecutionCompareAndSet`, `TestChainRecordsRequireCurrentFencingToken`,
    `TestExecutionHeadsForwardOnly`;
  - results, events and snapshots: `TestCommandResultsView`, `TestDomainEventsContiguous`,
    `TestRoundSnapshots`;
  - ledger: `TestLedgerAccounts`, `TestLedgerPostingsMaintainTotals`, `TestLedgerFloorEnforced`,
    `TestLedgerPostingValidation`, `TestLedgerOneTransactionPerRecord`, `TestLedgerHolds`,
    `TestLedgerConcurrentCrossRoundHolds`, `TestLedgerAppendOnly`;
  - outbox and truncation: `TestOutbox`, `TestTruncateRejectedEverywhere` (new).
- `internal/persistence/eventstore`:
  - unit: `TestEncodeHeaderMatchesGolden`, `TestEncodeHeaderRejectsBadSecret`;
  - integration: `TestExportGoldenHistories`, `TestExportStopsAtLatestSnapshot`,
    `TestExportRoundNotFound`, `TestExportNoCheckpoint`.
- `cmd/crashctl`:
  - unit: `TestMigrateUsage`, `TestMigrateConnectError`, `TestMigrateNeverPrintsPassword`,
    `TestMigrateExitCodes` (new), and `TestRun` (one row added);
  - integration: `TestMigrateCLI`.

## 12. Verification

Every command must exit 0. Judge by exit status.

```sh
make test
make test GOTESTFLAGS=
make lint
go vet ./...
go vet -tags integration ./...
make build
make golden-check
make test-integration
go mod tidy && git diff --exit-code go.mod go.sum
docker compose -f deployments/docker/compose.yaml up -d --wait
ORVEX_DATABASE_URL='postgres://orvex:orvex@127.0.0.1:55432/orvex?sslmode=disable' ./bin/crashctl migrate up
ORVEX_DATABASE_URL='postgres://orvex:orvex@127.0.0.1:55432/orvex?sslmode=disable' ./bin/crashctl migrate status
ORVEX_DATABASE_URL='postgres://orvex:orvex@127.0.0.1:55432/orvex?sslmode=disable' ./bin/crashctl migrate down -to 0
docker compose -f deployments/docker/compose.yaml down -v
```

Also check:
- Step 0's `diff` acceptance.
- `git diff --stat main` shows no change under `docs/adr/`, `internal/game/`, `internal/audit/`
  (including every golden fixture), `internal/canonjson/`, `cmd/gateway/`, `cmd/engine/` or `odin/`.
- If a golden export differs from its file, find which side contradicts the spec or LBT-9 and report
  it. Never edit the fixture.

## 13. Out of scope

Everything in spec §12's out-of-scope list, except that the "in this run" bullet no longer applies:
- the LBT-11 admission sequencer logic;
- the LBT-12 execution commit and posting rules;
- the outbox relay;
- leases and Kubernetes (LBT-14), and load tests (LBT-15);
- backup/restore tests;
- encryption of secrets at rest;
- a `crashctl audit export` command;
- partitioning.

Also out of scope: no drive-by refactors of LBT-9 code, no `go.work`, no second module, and no new
modules beyond the two named. Do not edit `docs/adr/**`; no ADR amendment is needed.
