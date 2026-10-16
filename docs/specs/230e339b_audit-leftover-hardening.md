# LBT-23: Test and defence-in-depth hardening (M1/M2 audit leftovers)

Run `230e339b`. Source: Linear LBT-23 (post-M2 audit, 2026-10-15). Five small items. Each item gets a
test that fails without its change. The frozen crash/v1 math, its constants, and every golden vector
and fixture stay byte-identical.

## 0. ADRs and conflicts

Read before editing: ADR-001, ADR-003 and ADR-004 (item 1), ADR-004 D-7 (item 2), ADR-002 and
ADR-005 (items 3 and 4), ADR-006 (all tests), ADR-007 (layout, no new modules).

**Conflicts: none.** Reasons:

- Item 1 makes crash/v1 `Evolve` stricter only for states that `InitialState` + `Evolve` can never
  build. `InitialState` sets `Commitment = Commitment(cfg.Seed)` and
  `CrashPointScaled = CrashPoint(cfg.Seed, cfg.RoundID)`, and no event changes `Seed`, `Commitment`,
  `RoundID` or `CrashPointScaled`. So for every history crash/v1 can produce, `Decide`, `Evolve`, the
  replay results, the golden vectors, the audit fixtures and the canonical bytes all stay the same.
  ADR-001/ADR-007 immutability holds, and no rules-version bump is needed. crash/v1 has no release
  yet anyway (no tags; ADR-007 l.42 applies "after the first stable rules release").
- The rejected event wraps `ErrEventNotApplicable`, as the request says. Every caller fails closed on
  any `Evolve` error: `execution/transition.go` uses `failClosed`, `audit/replay` uses `Mismatch`.
- Item 3 adds a named constant equal to today's bound, so admission behaviour does not change.
- Items 2, 4 and 5 are test-only.
- No ADR, migration, golden file, Makefile, CI, `.golangci.yml` or `go.mod` change.

## 1. Scope

In scope: exactly the five items below.

Out of scope (do not touch):
- CI workflows, `.golangci.yml`, the `Makefile` (LBT-20);
- shared or cross-package test helpers (LBT-24). Small helpers stay as closures inside the new test
  functions;
- seed protection and DSN or password masking behaviour (LBT-21);
- `docs/changes/` (written by the documenter phase), older specs, ADRs;
- any `migrations/` file, even temporarily;
- making `Decide` repeat item 1's checks; changing `crashctl` exit-code behaviour.

The frozen files must not change: `internal/game/crash/v1/{params,curve,payout,fairness,codec}.go`,
everything under `internal/game/crash/v1/testdata/`, `internal/audit/verification/testdata/`, and
`migrations/`.

## 2. Item 1: `RoundSettled` rechecks the reveal (LBT-9 A-12 defence in depth)

### 2.1 `internal/game/crash/v1/evolve.go`, `evolveRoundSettled`

Add two cases to the `switch`, directly after `case e.Seed != s.Seed:` and before
`case e.SettledAtUnixMicro < 0:`. Keep that order, so the wrong-seed error text and every existing
tamper outcome stay the same:

```go
	case e.Seed != s.Seed:
		return State{}, notApplicable(name, "seed does not match")
	case !VerifyCommitment(e.Seed, s.Commitment):
		return State{}, notApplicable(name, "commitment does not match the seed")
	case CrashPoint(e.Seed, s.RoundID) != s.CrashPointScaled:
		return State{}, notApplicable(name, "crash point does not match the seed")
	case e.SettledAtUnixMicro < 0:
```

- The reason strings are fixed text. They must not format any seed, commitment, crash point or other
  value (math spec §7).
- Neither string may contain the substring `seed does not match`, because `purity_test.go` uses that
  substring to identify the wrong-seed error.
- Add a doc comment on `evolveRoundSettled`, for example: "evolveRoundSettled also rechecks the
  reveal against the state (LBT-9 A-12, defence in depth): the seed must open the round's commitment
  and derive its crash point. InitialState guarantees both, so only a state that InitialState and
  Evolve did not build fails them."
- No new imports. `VerifyCommitment` and `CrashPoint` are in-package (`fairness.go`), and
  `TestNoIOImports` must stay green.

### 2.2 Tests

**`internal/game/crash/v1/evolve_test.go`, `TestEvolveValidatesApplicability`:**

- Next to `settleWith`, add a closure that tampers a copy of the consistent `lost` state:

  ```go
  stateWith := func(f func(*State)) State { s := lost; f(&s); return s }
  ```

- In `cases`, after `"RoundSettled wrong seed"`, add:
  - `{"RoundSettled commitment does not match the seed", stateWith(func(s *State) { s.Commitment[0] ^= 0x01 }), goodSettle}`
  - `{"RoundSettled crash point does not match the seed", stateWith(func(s *State) { s.CrashPointScaled++ }), goodSettle}`

The existing loop then requires `ErrEventNotApplicable`, `State{}` and an unchanged input for each
case.

The two cases are independent:
- Without the commitment check, the first case settles. Its crash point is untouched, so the
  crash-point check passes.
- Without the crash-point check, the second case settles. Its commitment is untouched.

**`internal/game/crash/v1/purity_test.go`, `TestRoundErrorsDoNotLeakSecrets`.** The new error paths
must also be covered by the no-secret-leak check (math spec §7). After `collect(nil, seedErr)`, add:

- `badCommitment := lost; badCommitment.Commitment[0] ^= 1`, then
  `_, commitErr := r.Evolve(badCommitment, goodSettle)`. Require
  `errors.Is(commitErr, ErrEventNotApplicable)` and
  `strings.Contains(errText(commitErr), "commitment does not match the seed")` (`t.Fatalf` otherwise).
  Then `collect(nil, commitErr)`.
- `badCrash := lost; badCrash.CrashPointScaled++`. Run the same pattern with
  `"crash point does not match the seed"`, then `collect(nil, crashErr)`.

The existing `secrets` scan then checks that neither error text contains the worked crash point,
deadline or seed. The reason-substring checks also pin each case to its own guard.

**Fails without the change (required evidence).** Write the tests first. Run
`go test -race -count=1 -run 'TestEvolveValidatesApplicability|TestRoundErrorsDoNotLeakSecrets' ./internal/game/crash/v1/`
before editing `evolve.go`. Both new subtests and the leak test must fail. Then edit `evolve.go` and
re-run: they pass. Report both runs.

## 3. Item 2: `MaxPayoutCredits` is the reachable maximum (math spec D-7)

**`internal/game/crash/v1/payout_test.go`.** Add `TestMaxPayoutCreditsIsReachableMaximum`. Use only
the package's real exported names:

1. `last, err := MultiplierAtTick(MaxTick - 1)`; `t.Fatal` on error.
2. `got, err := Payout(MaxBetCredits, last)`. Require `err == nil && got == MaxPayoutCredits`, with a
   message naming both values.
3. Reachability. `tick, eligible, err := CashOutTiming(0, MaxDurationMicros-1, MaxTick)` must give
   `err == nil`, `eligible`, and `tick == MaxTick-1`. The latest eligible cash-out of the longest
   round lands on `MaxTick-1`.
4. Maximality. `capM, err := MultiplierAtTick(MaxTick)` must give `err == nil` and
   `capM >= MaxCrashPointScaled`, and `Payout(MaxBetCredits, capM)` must fail with
   `errors.Is(err, ErrOutOfRange)`. `MaxTick` itself is never payable.

Add a doc comment citing D-7: payable ticks are `0..MaxTick-1`, `M` is non-decreasing, and `Payout`
is monotone in `m`.

`errors` is already imported. Do not change `params.go` or any vector.

## 4. Item 3: pin the S-10 admission bet ceiling

**`internal/runtime/admission/crashv1.go`:**

- Add an unexported constant, with a doc comment along these lines:

  ```go
  // crashV1MaxHoldCredits is the LBT-11 S-10 admission bet ceiling: the
  // largest player PlaceBet amount that gets a hold. It is written out rather
  // than read from v1.MaxBetCredits so that no other change widens admission
  // silently; TestCrashV1HoldCeiling requires it to be 1,000,000,000 and to
  // equal v1.MaxBetCredits (math spec D-7), so every bet Decide can accept
  // still has a hold.
  const crashV1MaxHoldCredits int64 = 1_000_000_000
  ```

- In `Classify`, replace `c.BetCredits <= v1.MaxBetCredits` with
  `c.BetCredits <= crashV1MaxHoldCredits`. The lower bound stays `v1.MinBetCredits`.
- Update the `CrashV1Policy` doc comment bullet to
  `v1.MinBetCredits <= BetCredits <= crashV1MaxHoldCredits (1,000,000,000)`.

**`internal/runtime/admission/crashv1_test.go` (package `admission`).** Add `TestCrashV1HoldCeiling`.
Use a table of `{name string; got, want int64}` rows so that no linter sees a constant condition:

- `{"S-10 ceiling", crashV1MaxHoldCredits, 1_000_000_000}`
- `{"D-7 MaxBetCredits", crashV1MaxHoldCredits, v1.MaxBetCredits}`

`t.Errorf` on any mismatch.

Leave `TestCrashV1Classify` unchanged. Its literal amounts `1_000_000_000` and `1_000_000_001` keep
pinning the behaviour at the boundary.

**Fails without the change (required evidence).** Add the test first. `go test ./internal/runtime/admission/`
does not compile (`undefined: crashV1MaxHoldCredits`). Add the constant and it passes. Report both.

## 5. Item 4: no 23514 in `TestAdmitConcurrentCrossRoundHolds`

**`internal/runtime/admission/concurrency_integration_test.go`.**

In `TestAdmitConcurrentCrossRoundHolds`, at the top of the `for i, err := range errsOf` loop, before
the existing `switch`, add:

```go
if code := postgres.SQLState(err); code == "23514" {
	floorViolations++
	t.Errorf("goroutine %d: SQLSTATE 23514 on %q: the floor CHECK refused the hold, not the S-11 account-lock check",
		i, constraintOf(err))
}
```

- Declare `floorViolations := 0` next to `ok, rejected`. After the loop, add
  `if floorViolations != 0 { t.Errorf("%d admissions surfaced SQLSTATE 23514, want 0", floorViolations) }`.
- `postgres.SQLState(nil)` is `""`.
- `constraintOf` already exists in `helpers_integration_test.go`, same package `admission_test`.
- Add the import `"github.com/juano-morello/orvex/internal/persistence/postgres"`, grouped with the
  local imports (goimports local prefix).
- Add a doc comment on the test: concurrent cross-round bets on one account must be refused by the
  S-11 check under the account row lock (`FOR NO KEY UPDATE` in `orvex.admit_command`). No
  admission may surface the `ledger_accounts_floor_check` backstop, which the sequencer would also
  report as `insufficient_credits` (S-11, S-12).
- Keep every existing assertion.

**Why the assertion bites.** A floor refusal reaches the caller as
`fmt.Errorf("%w: %w", &RejectedError{insufficient_credits}, pgErr)` (`sequencer.go` `callFailed`).
That passes the existing `ReasonInsufficientCredits` branch, but the PgError stays reachable, and
`sequencer_test.go`'s `"floor check"` case already proves that `SQLState == "23514"` for that error.
So without the new assertion, a removed or broken S-11 check would still pass on the counts alone.
Do **not** edit any migration to show this, not even temporarily. Cite this reasoning in the report.

## 6. Item 5: `crashctl migrate` exit codes (LBT-10 P-6)

The P-6 table: 0 done; 1 a migration failed (SQL error, `OX003`, `OX007`); 2 usage (including
`ORVEX_DATABASE_URL` unset or unparsable, and a target on the wrong side); 3 unsupported (schema
newer, checksum differs); 4 I/O (cannot connect, cannot write stdout).

Already covered through `run`: usage args, URL unset/empty/`::bad`, port-1 connect refused,
`OX003` down refusal, bookkeeping version 99 (`ErrSchemaState`), stdout failure, and `up` on the
wrong side.

The tests below add the rows that are not exercised end to end. Behaviour does not change. If any
new assertion fails on current code, stop and report it; do not change `migrate.go`.

### 6.1 `cmd/crashctl/migrate_test.go` (unit, no database)

- **`TestMigrateUsage`, table:** add `{"unknown flag", []string{"up", "-bogus"}}`.
- **`TestMigrateUsage`, subtest `"ORVEX_DATABASE_URL unparsable"`:** turn it into a loop. Use nested
  `t.Run`s and call `t.Setenv` in each leaf.
  - DSNs:
    - `"::bad"` (syntax);
    - `"postgres://u:p@127.0.0.1:1/db?connect_timeout=x"` (bad parameter);
    - `"postgres://u:p@127.0.0.1:1/db?sslmode=bogus"` (bad sslmode);
    - `"host='unterminated"` (unterminated key=value quote).
  - Subcommands: `{"up"}`, `{"down","-to","0"}`, `{"status"}`.
  - Each run requires exit `exitUsage`, empty stdout, and stderr exactly
    `"crashctl: migrate " + sub[0] + ": invalid ORVEX_DATABASE_URL\n"`.
- **`TestMigrateConnectError`:** loop over two unreachable DSNs × the existing three subcommands.
  - The DSNs are `unreachableDSN` (URL form) and
    `"host=127.0.0.1 port=1 user=u password=p dbname=db connect_timeout=2"` (key=value form).
  - Keep the existing assertions: exit `exitIO`, empty stdout, one `crashctl: migrate <sub>: ` line.
- Leave `TestMigrateNeverPrintsPassword` and `TestMigrateExitCodes` unchanged (LBT-21 owns masking).

### 6.2 `cmd/crashctl/migrate_integration_test.go` (real PostgreSQL via `pgtest`)

Add `TestMigrateCLIExitCodes`.

- It is not parallel (it uses `t.Setenv`). Each subtest uses its own database and calls
  `t.Setenv(databaseURLEnv, dsn)`.
- Inside it, define one closure
  `requireLine := func(t *testing.T, stderr, sub, want string)`. It requires exactly one
  `\n`-terminated line, the prefix `"crashctl: migrate "+sub+": "`, and that the line contains
  `want`.
- Get `ms, err := postgres.LoadMigrations(migrations.FS)` once (`t.Fatal` on error). Use `len(ms)`
  rather than a hardcoded 9.
- Use `context.Background()` for the setup SQL through `pgtest.Pool(t, dsn)`.
- New imports: `net/url`, `github.com/juano-morello/orvex/internal/persistence/postgres`,
  `github.com/juano-morello/orvex/migrations`.
- Assertions on stderr use only SQLSTATEs or runner phrases without the substring `orvex`. The
  current masking replaces the test password `orvex` everywhere in the message, and changing that is
  LBT-21's work.

Subtests:

1. **`"1 sql error"`** (`EmptyDatabase`).
   - `up -to 1` gives exit 0.
   - `CREATE TABLE orvex.chain_records (x int)`.
   - `up` gives exit `exitFailed`, stdout exactly `"applied 0002 rounds_admissions\n"` (the committed
     step, no `schema version` line), and `requireLine(..., "up", "42P07")`. The line also contains
     `"0003 chain_execution up"`.
   - Then `status` gives exit 0, stdout contains `"0003 chain_execution pending\n"` and ends with
     `"schema version 2\n"`.
2. **`"1 encoding"`** (`pgtest.CreateDatabase(t, "ENCODING 'SQL_ASCII' TEMPLATE template0")`).
   - `up` gives exit `exitFailed`, stdout `""`, and `requireLine(..., "up", "OX007")`.
   - Then `status` gives exit 0 and ends with `"schema version 0\n"`.
3. **`"2 down target above current"`** (`EmptyDatabase`).
   - `up -to 2` gives exit 0.
   - `down -to 5` gives exit `exitUsage`, stdout `""`, and `requireLine(..., "down", "other side")`.
   - `status` still ends with `"schema version 2\n"`.
4. **`"3 checksum mismatch"`** (`EmptyDatabase`).
   - `up` gives exit 0.
   - `UPDATE orvex_migrate.schema_migrations SET up_sha256 = $1 WHERE version = 1` with
     `make([]byte, 32)`.
   - Each of `status`, `up` and `down -to 0` gives exit `exitUnsupported`, stdout `""`, and
     `requireLine(..., sub, "differs from this build")`.
5. **`"3 schema newer than build"`** (`EmptyDatabase`).
   - `up` gives exit 0.
   - `INSERT INTO orvex_migrate.schema_migrations (version, name, up_sha256, down_sha256, applied_by)
     VALUES ($1, 'future', $2, $2, 'test')` with `len(ms)+1` and `make([]byte, 32)`. The versions
     stay contiguous, so the runner reports `ErrSchemaTooNew`, not `ErrSchemaState`.
   - Each of `status`, `up` and `down -to 0` gives exit `exitUnsupported`, stdout `""`, and
     `requireLine(..., sub, "newer than this build")`.
6. **`"4 database does not exist"`.** Parse `pgtest.EmptyDatabase(t)` with `url.Parse` and set
   `u.Path = "/no_such_database"`. For each of `status` and `up`: exit `exitIO`, stdout `""`, and
   `requireLine(..., sub, "3D000")`.
7. **`"4 wrong password"`.** Same DSN with `u.User = url.UserPassword(u.User.Username(), "wrong-password")`.
   For each of `status` and `up`: exit `exitIO`, stdout `""`, and `requireLine(..., sub, "28P01")`.
   - The pgtest container sets only `POSTGRES_USER`, `POSTGRES_PASSWORD` and `POSTGRES_DB`, so host
     connections use password authentication.
   - If this unexpectedly connects, report it; do not weaken the test.

Each subtest pins a P-6 row through `run`, a row that no test exercised end to end before. For
example, if `migrateExitCode` mapped `ErrChecksumMismatch` or `ErrSchemaTooNew` to 1, or the connect
branch returned 1 or 2, a subtest here would fail.

## 7. Files to change (complete list)

| Path | Change |
|---|---|
| `internal/game/crash/v1/evolve.go` | item 1 checks and doc comment |
| `internal/game/crash/v1/evolve_test.go` | item 1 cases |
| `internal/game/crash/v1/purity_test.go` | item 1 no-leak coverage of the two new errors |
| `internal/game/crash/v1/payout_test.go` | item 2 `TestMaxPayoutCreditsIsReachableMaximum` |
| `internal/runtime/admission/crashv1.go` | item 3 constant, `Classify`, doc |
| `internal/runtime/admission/crashv1_test.go` | item 3 `TestCrashV1HoldCeiling` |
| `internal/runtime/admission/concurrency_integration_test.go` | item 4 assertion, import, doc |
| `cmd/crashctl/migrate_test.go` | item 5 unit rows |
| `cmd/crashctl/migrate_integration_test.go` | item 5 `TestMigrateCLIExitCodes` |

The plan copy `docs/specs/230e339b_audit-leftover-hardening.md` is already in the tree; leave it as
is. No other file may change.

## 8. Verification

Run every command in the foreground, one at a time, to completion. Use bash's `time` keyword for the
duration (for example `time make test`), give `make test-integration` a timeout of at least 3600 s,
and never run checks async or in parallel. Judge each by its exit status. Report each exact command
with its exit status and duration.

Targeted runs first:

```sh
go test -race -count=1 -run 'TestEvolveValidatesApplicability|TestRoundErrorsDoNotLeakSecrets|TestMaxPayoutCreditsIsReachableMaximum' ./internal/game/crash/v1/
go test -race -count=1 -run 'TestCrashV1HoldCeiling|TestCrashV1Classify' ./internal/runtime/admission/
go test -race -count=1 -run 'TestMigrateUsage|TestMigrateConnectError' ./cmd/crashctl/
go test -race -count=1 -tags integration -run TestAdmitConcurrentCrossRoundHolds ./internal/runtime/admission/
go test -race -count=1 -tags integration -run 'TestMigrateCLIExitCodes|TestMigrateCLI' ./cmd/crashctl/
```

Gates (each must exit 0):

```sh
make test
make test-integration
make lint
make build
make golden-check
```

Frozen-artefact checks (each must exit 0):

```sh
git diff --quiet -- internal/game/crash/v1/params.go internal/game/crash/v1/curve.go internal/game/crash/v1/payout.go internal/game/crash/v1/fairness.go internal/game/crash/v1/codec.go internal/game/crash/v1/testdata internal/audit/verification/testdata migrations docs/adr Makefile .golangci.yml .github go.mod go.sum
git status --porcelain
```

`git status --porcelain` must list only the nine paths in §7 plus the spec copy.

Report:
- every changed path, exactly;
- the item 1 and item 3 before/after test runs (§2.2, §4);
- the item 4 reasoning (§5);
- confirmation that no ADR conflict arose.
