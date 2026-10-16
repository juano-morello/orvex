# LBT-25: `make audit` target (export + verify)

- Linear: [LBT-25](https://linear.app/lbtworks/issue/LBT-25) "make audit target (export + verify)". Related: LBT-9
  (deferred `make audit`, `docs/specs/8bebb70f_canonical-codec-audit-cli.md` §2 item 9), LBT-19
  (`crashctl audit export`, `docs/specs/73dbe98d_audit-export-cli.md`), LBT-20 (Makefile, merged), LBT-21
  (edits README.md and AGENTS.md in parallel).
- Linear acceptance: "`make audit ROUND_ID=…` works against the local Compose database; usage and failure
  behaviour are documented in README.md and AGENTS.md; an integration test or a documented smoke run proves it."
- Governing ADRs: ADR-007 (lists `make audit`; one module; thin `cmd`), ADR-001 (anchoring limitation),
  ADR-006 (real PostgreSQL tests), ADR-004 and LBT-19 X-7 (unrevealed secret only with `-unsettled`).
- Size: M. Four files change: `Makefile`, one new integration test file, `README.md`, `AGENTS.md`.

## 0. Rules for the builder

- Do not change `crashctl` (`cmd/crashctl/*.go` other than the new test file), CI workflows, migrations,
  `go.mod`/`go.sum`, `.golangci.yml`, `docs/adr/`, `docs/architecture/`, `docs/changes/`. No new module.
- No seed protection work (LBT-21).
- Doc edits are additive only: one new section at the end of `README.md`, one new bullet in `AGENTS.md`. Touch no
  other line of those files: LBT-21 edits them in parallel.
- Recipe lines in the Makefile start with a TAB. Copy the block in section 3.1 exactly, including `$$` escapes.

## 1. Current state (verified while planning)

- `main` is at `3c66059` (LBT-20 merged). GNU Make 4.4.1 here; `/bin/sh` is dash. CI (`ci.yml`) runs
  `make test-integration` on `ubuntu-latest`, so `make` is available where integration tests run.
- `Makefile`: header comment says "Add targets (proto, dev, audit) only when they become meaningful."
  `BIN_DIR := bin`, `GO ?= go`; `build-crashctl` is a static pattern rule
  (`$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$* ./cmd/$*`). Command-line `BIN_DIR=` and `GO=`
  override them. Precedent for env-passed recipe variables: `export FUZZ_PKG FUZZ_TARGET` and `$$FUZZ_PKG`.
- `crashctl audit export [-unsettled] [-format NAME] ROUND_ID FILE` (`cmd/crashctl/export.go`):
  - validates `ROUND_ID` (exit 2) before reading `ORVEX_DATABASE_URL` (unset or empty: exit 2);
  - refuses an existing `FILE` (exit 2), creates it `O_EXCL` with mode `0600`, removes it on any failure;
  - on success prints to stdout a report with the line `records <S+1> of <N+1> head <hex64>`; the hex is the exact
    `-expect-head` value (LBT-19 X-8);
  - exit codes: 0 ok, 1 failed, 2 usage, 3 unsupported, 4 I/O and every database error, 5 round not found, 6 not
    settled. Only `orvex/audit-history/v1` is writable; a well-formed unknown format (e.g. `.../v99`) is exit 3.
- `crashctl audit verify -expect-head HEX FILE` prints `chain ok records <n> head <hex>` … `ok` (exit 0), or fails
  (1 failed, 3 unsupported, 4 I/O).
- Integration helpers in package `main` (`//go:build integration`) usable by the new test:
  `exportDatabase(t) (dsn, pool)` (fresh migrated DB, also `t.Setenv(ORVEX_DATABASE_URL)`),
  `drained(t, pool, file) roundtest.Golden` (created, seeded and executed through
  `internal/persistence/postgres/pgtest/roundtest`), `executedRounds` (file, `s`, `n`, `head`, `settled`),
  `wantExportReport(round, s, n, head, settledVerdict)`, `notSettledMessage(round, s, n)`,
  `replicaExec(t, pool, sql, args...)`. Untagged helpers: `requireNoFile`, `exportRoundID`, `unreachableDSN`
  (`migrate_test.go`), `databaseURLEnv`, the `exit*` constants, `settledYes`/`settledNo`.
  `TestMain` (`pgtest.Run`) is in `migrate_integration_test.go`; do not add another.
- `executedRounds` facts used below: worked-example `s=21 n=22` head `3959ed4c…`; instant-crash `s=n=5`;
  empty-round `s=n=3`; in-progress `s=2 n=3` head `d0df1ee2…`, not settled.

## 2. ADR alignment and flagged interpretations

No change contradicts an ADR. Copy these points into the PR description and the agent report.

1. **"Propagates crashctl's exit code" (interpretation of the request, not an ADR).** GNU make exits 2 whenever a
   recipe fails; it cannot exit with an arbitrary code. The recipe shell exits with `crashctl`'s exact code,
   prints `audit: crashctl audit export|verify failed with exit code N`, and make reports it as
   `make: *** [Makefile:<line>: audit] Error N`. `make audit` itself exits 0 on success and 2 on every failure.
   Missing-variable failures use recipe code 2 (usage), matching `crashctl`'s usage code. Callers that need the
   raw code run `crashctl` directly. Verified while planning with GNU Make 4.4.1 and dash.
2. **ADR-001 anchoring (limitation, LBT-19 §2 item 10).** The `-expect-head` value comes from the same export, from
   the same database. The target proves the export is complete and self-consistent end to end; it is not an
   independently anchored head. README says so in one sentence.
3. **ADR-004 / LBT-19 X-7 (`AUDIT_UNSETTLED=1`).** It passes `-unsettled`, so the temporary file may hold an
   unrevealed seed. The file lives in a fresh `mktemp -d` directory (mode 0700), is created 0600 by `crashctl`, and
   the directory is removed on every exit, including HUP/INT/TERM. A SIGKILL can leave the 0700 directory under
   `$TMPDIR`. `crashctl audit verify` does not print the seed. `AUDIT_UNSETTLED` accepts only `1` or empty
   (fail closed: `AUDIT_UNSETTLED=0` must not silently mean "on").
4. **ADR-007 (alignment).** Adds the `make audit` target ADR-007 lists, resolving LBT-9 §2 item 9. No binary,
   module or directory is added. The Makefile header comment drops `audit` from its "add later" list.
5. **ADR-006 (alignment).** The target is proven against real PostgreSQL (Testcontainers) with rounds created
   through `pgtest/roundtest`, in the existing `make test-integration` run (CI already runs it).
6. **AGENTS.md test rules.** No change to `internal/persistence/`, `internal/runtime/` or the export code path,
   so no `make test-repeat-*` target is required; the new test is integration-tagged, so `make test-integration`
   is mandatory. The new tests are named `TestMakeAudit*`, which the `test-repeat-export` filter `TestExport`
   does not select.
7. **`ORVEX_DATABASE_URL` handling.** The Makefile never references it as a Make variable (`$(…)`), only as the
   shell's `$$ORVEX_DATABASE_URL` in an emptiness test, and never prints it. Make passes environment values
   through unchanged, so a `$` in a password survives (verified while planning). `crashctl` reads it, as for
   `migrate`.

## 3. Changes

### 3.1 `Makefile`

1. Line 1-2 header comment: change `# Orvex build orchestration (ADR-007). Add targets (proto, dev, audit) only`
   to `# Orvex build orchestration (ADR-007). Add targets (proto, dev) only` (keep the second line
   `# when they become meaningful.` as is).
2. `.PHONY`: add `audit` after `golden-check`.
3. Insert this block after the `golden-check` recipe and before `fuzz:` (recipe lines start with a TAB):

```make
# make audit ROUND_ID=<uuid> (ADR-007, LBT-25): build crashctl, export the
# round from the database named by ORVEX_DATABASE_URL with crashctl audit
# export into a new private directory (mktemp -d, mode 0700; crashctl creates
# the file with mode 0600), then verify the file with crashctl audit verify
# -expect-head set to the head the export printed. The directory is removed
# on every exit. AUDIT_FORMAT=NAME passes -format NAME and AUDIT_UNSETTLED=1
# passes -unsettled. ROUND_ID and ORVEX_DATABASE_URL are checked before the
# build. A failed crashctl step exits with crashctl's code, which make reports
# as "Error N"; make itself then exits 2, as for any failed recipe. The
# Makefile never expands ORVEX_DATABASE_URL: crashctl reads it from the
# environment, so a "$" in it survives.
ROUND_ID ?=
AUDIT_FORMAT ?=
AUDIT_UNSETTLED ?=
export ROUND_ID AUDIT_FORMAT AUDIT_UNSETTLED

audit:
	@[ -n "$$ROUND_ID" ] || { echo "audit: ROUND_ID is not set; usage: make audit ROUND_ID=<uuid> [AUDIT_FORMAT=NAME] [AUDIT_UNSETTLED=1]" >&2; exit 2; }
	@[ -n "$$ORVEX_DATABASE_URL" ] || { echo "audit: ORVEX_DATABASE_URL is not set; it names the database to export from" >&2; exit 2; }
	@case "$$AUDIT_UNSETTLED" in ''|1) ;; *) echo "audit: AUDIT_UNSETTLED must be 1 or empty, got '$$AUDIT_UNSETTLED'" >&2; exit 2 ;; esac
	@$(MAKE) --no-print-directory build-crashctl
	@dir=$$(mktemp -d "$${TMPDIR:-/tmp}/orvex-audit.XXXXXXXX") || { echo "audit: cannot create a temporary directory" >&2; exit 4; }; \
	trap 'rm -rf "$$dir"' EXIT; trap 'exit 129' HUP; trap 'exit 130' INT; trap 'exit 143' TERM; \
	file="$$dir/round.jsonl"; \
	set --; \
	if [ -n "$$AUDIT_UNSETTLED" ]; then set -- "$$@" -unsettled; fi; \
	if [ -n "$$AUDIT_FORMAT" ]; then set -- "$$@" -format "$$AUDIT_FORMAT"; fi; \
	report=$$("$(BIN_DIR)/crashctl" audit export "$$@" "$$ROUND_ID" "$$file"); rc=$$?; \
	[ -z "$$report" ] || printf '%s\n' "$$report"; \
	if [ "$$rc" -ne 0 ]; then echo "audit: crashctl audit export failed with exit code $$rc" >&2; exit "$$rc"; fi; \
	expect=$$(printf '%s\n' "$$report" | sed -n 's/^records [0-9][0-9]* of [0-9][0-9]* head \([0-9a-f]\{64\}\)$$/\1/p'); \
	if [ -z "$$expect" ]; then echo "audit: crashctl audit export printed no head" >&2; exit 1; fi; \
	"$(BIN_DIR)/crashctl" audit verify -expect-head "$$expect" "$$file"; rc=$$?; \
	if [ "$$rc" -ne 0 ]; then echo "audit: crashctl audit verify failed with exit code $$rc" >&2; exit "$$rc"; fi; \
	echo "audit: round $$ROUND_ID verified, head $$expect"
```

Design notes (why, so the builder does not "simplify" it):

- The three checks run before the build, so a missing variable fails fast and builds nothing. `build-crashctl` is
  invoked by recursive `$(MAKE)` (not as a prerequisite) to keep that order under `-j`. Command-line overrides
  (`BIN_DIR=`, `GO=`) reach the sub-make through `MAKEFLAGS`.
- The variables reach the shell through the environment (`export`, `$$ROUND_ID`), as `fuzz-one` does, never
  through `'$(ROUND_ID)'` quoting.
- `crashctl audit export` refuses an existing `FILE`, so the recipe creates a directory, not a file; `crashctl`
  creates `round.jsonl` inside it with `O_EXCL` and mode 0600.
- `set --` builds the optional flags without word-splitting `AUDIT_FORMAT`.
- The export report is printed verbatim (command substitution strips its final newline; `printf '%s\n'` restores
  it). The head is the 64-hex field of the `records … head …` line; a report without it is exit 1.
- The `EXIT` trap removes the directory; the HUP/INT/TERM traps turn signals into exits so the `EXIT` trap runs
  under dash too. `exit "$rc"` inside an `EXIT`-trapped shell keeps `rc` (verified with dash).
- Planning validated this exact recipe with dash and GNU Make 4.4.1 against a stub `crashctl`: all three checks
  (no build line printed), flag mapping `-unsettled -format NAME ROUND_ID FILE`, directory `drwx------`, file
  `-rw-------`, export failure `Error 6`, missing head `Error 1`, verify failure `Error 1`, `ROUND_ID` from the
  environment, and an empty `$TMPDIR` after every run.

### 3.2 New test file `cmd/crashctl/make_audit_integration_test.go`

`//go:build integration`, `package main`. Header comment: integration tests of `make audit` (LBT-25) run as an
operator runs it, against rounds created, seeded and executed through `pgtest/roundtest` in real PostgreSQL;
each run builds `crashctl` into a per-test `BIN_DIR`, so the repository's `bin/` is untouched; none is parallel
(they set `ORVEX_DATABASE_URL` through `exportDatabase`). No sleeps.

Helpers (in this file):

- `makeEnvDrop`: names removed from the inherited environment: `MAKEFLAGS`, `MFLAGS`, `MAKELEVEL`,
  `MAKEOVERRIDES`, `GNUMAKEFLAGS`, `MAKE_TERMOUT`, `MAKE_TERMERR`, `ROUND_ID`, `AUDIT_FORMAT`, `AUDIT_UNSETTLED`,
  `databaseURLEnv`, `TMPDIR`, `GOTMPDIR`. Comment why: `make test-integration` exports its flags, its level, a
  possible jobserver and (through the new `export` line) empty audit variables to `go test`; they must not leak
  into the inner make.
- `type makeAuditResult struct { code int; stdout, stderr string }` with `code` = make's exit status.
- `makeAudit(t, binDir, tmp string, env []string, args ...string) makeAuditResult`:
  - repository root = `filepath.Abs(filepath.Join("..", ".."))`; `t.Fatal` unless `<root>/Makefile` exists;
  - `exec.LookPath("make")`, `t.Fatalf("make audit tests need make on PATH: %v", err)` on failure (integration
    tests run through `make test-integration`; do not skip);
  - `exec.CommandContext(t.Context(), makePath, append([]string{"--no-print-directory", "audit", "BIN_DIR=" + binDir}, args...)...)`
    with `//nolint:gosec // G204: the make path and arguments are fixed by this test` (gosec is enabled;
    follow the existing `//nolint:gosec // G304: …` style);
  - `cmd.Dir = root`; `cmd.Env` = `os.Environ()` minus `makeEnvDrop` (compare the name before the first `=`),
    plus `TMPDIR=<tmp>`, `GOTMPDIR=<t.TempDir()>` (keeps the Go toolchain's work files out of `tmp`), plus `env`;
  - capture stdout and stderr separately; `errors.As(err, &exitErr)` → `exitErr.ExitCode()`; any other error is
    `t.Fatal`.
- `requireEmptyDir(t, dir)`: `os.ReadDir`; `t.Errorf` naming the leftover entries unless empty (proves the
  temporary directory was removed).
- `requireMakeFailure(t, res, code int, want ...string)`: `res.code == 2`, `res.stderr` contains
  `"] Error " + strconv.Itoa(code) + "\n"` (matches GNU make 3.81 and 4.x formats) and every `want` substring;
  on failure print both streams.
- `binDir` comes from the top-level test's `t.TempDir()` (a top-level test name has only identifier characters;
  `BIN_DIR` must not contain whitespace, a Makefile limitation of `-o $(BIN_DIR)/$*`). `tmp` is each subtest's own
  `t.TempDir()`.

Tests (each name is required):

1. `TestMakeAuditMissingVariables`, table subtests, each with a fresh `binDir := t.TempDir()` and `tmp`; no
   database is created:

   | subtest | env | args | stderr must contain |
   |---|---|---|---|
   | `no-round-id` | `ORVEX_DATABASE_URL=`+`unreachableDSN` | — | `audit: ROUND_ID is not set; usage: make audit ROUND_ID=<uuid> [AUDIT_FORMAT=NAME] [AUDIT_UNSETTLED=1]\n` |
   | `empty-round-id` | same | `ROUND_ID=` | same |
   | `no-database-url` | — | `ROUND_ID=`+`exportRoundID` | `audit: ORVEX_DATABASE_URL is not set; it names the database to export from\n` |
   | `empty-database-url` | `ORVEX_DATABASE_URL=` | `ROUND_ID=`+`exportRoundID` | same |
   | `bad-audit-unsettled` | `ORVEX_DATABASE_URL=`+`unreachableDSN` | `ROUND_ID=`+`exportRoundID`, `AUDIT_UNSETTLED=yes` | `audit: AUDIT_UNSETTLED must be 1 or empty, got 'yes'\n` |

   Each: `requireMakeFailure(t, res, exitUsage, want)`; `res.stdout == ""` (no build line, nothing exported);
   `requireNoFile(t, filepath.Join(binDir, "crashctl"))` (nothing built); `requireEmptyDir(t, tmp)`.

2. `TestMakeAuditSettledRounds`: `binDir := t.TempDir()` once; one subtest per `executedRounds` entry with
   `settled == true` (worked-example, instant-crash, empty-round), named `strings.TrimSuffix(r.file, ".jsonl")`.
   Each: `dsn, pool := exportDatabase(t)`; `g := drained(t, pool, r.file)`; env `databaseURLEnv+"="+dsn`; pass
   `ROUND_ID=`+`g.UUID()` as a make argument, except for empty-round, which passes it in `env` instead (covers
   the environment source). Require:
   - `res.code == 0` (else `t.Fatalf` with both streams);
   - `res.stdout` contains `wantExportReport(g.UUID(), r.s, r.n, r.head, settledYes)` and
     `fmt.Sprintf("chain ok records %d head %s\n", r.s+1, r.head)`, the export report before the
     `verifier ` line, and ends with `"ok\naudit: round " + g.UUID() + " verified, head " + r.head + "\n"`;
   - `res.stderr` contains neither `audit: ` nor `Error`;
   - `requireEmptyDir(t, tmp)`; `<binDir>/crashctl` exists after the run.

3. `TestMakeAuditUnsettled`: `binDir` once; round = the `executedRounds` entry whose file is `in-progress.jsonl`
   (look it up by file, not index); one database and one `drained` round shared by both subtests.
   - `default`: `requireMakeFailure(t, res, exitNotSettled, notSettledMessage(g.UUID(), r.s, r.n),
     "audit: crashctl audit export failed with exit code 6\n")`; `res.stdout` has no `"\nrecords "`;
     `requireEmptyDir(t, tmp)`.
   - `audit-unsettled`: `AUDIT_UNSETTLED=1`: `res.code == 0`; stdout contains
     `wantExportReport(g.UUID(), r.s, r.n, r.head, settledNo)` and `chain ok records 3 head <r.head>`, ends with
     the `audit: round … verified, head …` line; `requireEmptyDir(t, tmp)`.

4. `TestMakeAuditFormat`: one database, drained worked-example.
   - `v1`: `AUDIT_FORMAT=`+`verification.Format`: `res.code == 0`; stdout contains
     `wantExportReport(g.UUID(), 21, 22, <worked-example head>, settledYes)`.
   - `unsupported`: `AUDIT_FORMAT=orvex/audit-history/v99`: `requireMakeFailure(t, res, exitUnsupported,
     "crashctl: audit export: format orvex/audit-history/v99 unsupported",
     "audit: crashctl audit export failed with exit code 3\n")`; `requireEmptyDir`.

5. `TestMakeAuditFailures` (exit-code propagation, each subtest with its own `tmp`):
   - `unknown-round`: `exportDatabase` only (migrated, no round); `ROUND_ID=0190f3a8-7b3c-7d4e-8f00-0000000000ff`;
     `requireMakeFailure(…, exitNotFound, "crashctl: audit export: round 0190f3a8-7b3c-7d4e-8f00-0000000000ff not found\n",
     "audit: crashctl audit export failed with exit code 5\n")`.
   - `malformed-round-id`: env `unreachableDSN`, `ROUND_ID=not-a-uuid`: `requireMakeFailure(…, exitUsage,
     "ROUND_ID must be a canonical lowercase UUID other than the nil UUID",
     "audit: crashctl audit export failed with exit code 2\n")` (the recipe does not validate the ID itself).
   - `unreachable-database`: env `unreachableDSN`, `ROUND_ID=`+`exportRoundID`: `requireMakeFailure(…, exitIO,
     "audit: crashctl audit export failed with exit code 4\n")`; neither stream contains `u:p@`.
   - `verify-fails`: `exportDatabase`, `drained(…, "worked-example.jsonl")`, then
     `replicaExec(t, pool, "UPDATE orvex.round_secrets SET config_secret = $2 WHERE round_id = $1", g.RoundID,
     []byte(`{"seed":"`+strings.Repeat("f", 64)+`"}`))` (as `TestExportCorruptRound` "secret replaced");
     `AUDIT_UNSETTLED=1` so the export still writes the file (`settled unknown`):
     `requireMakeFailure(…, exitFailed, "fairness failed", "audit: crashctl audit verify failed with exit code 1\n")`;
     stdout contains `"\nsettled unknown\n"`; `requireEmptyDir(t, tmp)` (removed after a verify failure).

Use `t.Context()` (Go 1.24+), `errors.As` (errorlint is enabled), and the module-local import grouping
(goimports `local-prefixes`). Import `verification` only if used (`TestMakeAuditFormat`).

### 3.3 `README.md`

Append at the end of the file (after the `See [AGENTS.md]…` line, one blank line between), nothing else:

```markdown
## Audit

`make audit ROUND_ID=<uuid>` builds `crashctl`, exports the round from the database named by `ORVEX_DATABASE_URL` with `crashctl audit export` into a new private temporary directory (the file has mode 0600), verifies it with `crashctl audit verify -expect-head` set to the head the export printed, and removes the directory.

- `AUDIT_FORMAT=NAME` passes `-format NAME`. `AUDIT_UNSETTLED=1` passes `-unsettled`: it audits a round that is not settled at its newest snapshot, and the temporary file then holds the round's unrevealed secret config.
- A missing `ROUND_ID` or `ORVEX_DATABASE_URL`, or an `AUDIT_UNSETTLED` other than `1`, stops before the build with an `audit:` message. A failed step prints `audit: crashctl audit export failed with exit code N` (or `verify`); make reports `Error N` and exits 2. `N` is `crashctl`'s code: 1 failed, 2 usage, 3 unsupported, 4 I/O or database, 5 round not found, 6 round not settled.
- The expected head comes from the same database as the file, so the run checks the export end to end but is not an independent anchor (ADR-001). Keep the printed head elsewhere to anchor it.
- Local Compose database: `ORVEX_DATABASE_URL='postgres://orvex:orvex@127.0.0.1:55432/orvex?sslmode=disable' make audit ROUND_ID=<uuid>`.
```

### 3.4 `AGENTS.md`

Insert one bullet in `## Commands`, immediately after the `make test-repeat` bullet and before the `- CI:` bullet;
change no other line:

```markdown
- `make audit ROUND_ID=<uuid>`: build `crashctl`, export the round from the database named by `ORVEX_DATABASE_URL` into a private temporary directory and verify it with `crashctl audit verify -expect-head` set to the head the export printed; `AUDIT_FORMAT=NAME` and `AUDIT_UNSETTLED=1` map to `-format` and `-unsettled`. A failed step names `crashctl`'s exit code (make reports `Error N` and exits 2). The `TestMakeAudit*` integration tests in `cmd/crashctl` cover it: run `make test-integration` when you change the target.
```

## 4. Verification

Run each command alone, from the repository root, one at a time. A command that moves to a background job must be
waited on until it exits; report each exact command with its exit status and wall-clock duration.

1. `make build` (exit 0).
2. `make test` (exit 0).
3. `make lint` (exit 0; requires golangci-lint v2.14.0). Expect no new gosec finding beyond the annotated G204.
4. `make golden-check` (exit 0; no golden change is expected).
5. `make test-integration` (exit 0; needs Docker). Confirm the new tests ran:
   `go test -tags integration -count=1 -run 'TestMakeAudit' -v ./cmd/crashctl/` (exit 0) and that its output lists
   `TestMakeAuditMissingVariables`, `TestMakeAuditSettledRounds`, `TestMakeAuditUnsettled`, `TestMakeAuditFormat`
   and `TestMakeAuditFailures` with every subtest `PASS`.
6. Manual checks (report output and status): `make audit` with no variables in a shell where `ORVEX_DATABASE_URL`
   is unset prints the `ROUND_ID is not set` line and exits 2; `make -n audit ROUND_ID=x` does not run the
   export (dry run).
7. Optional smoke against the local Compose database (Linear acceptance), only if port 55432 is free; otherwise
   say it was skipped and why: `docker compose -f deployments/docker/compose.yaml up -d --wait`, `make build-crashctl`,
   `ORVEX_DATABASE_URL='postgres://orvex:orvex@127.0.0.1:55432/orvex?sslmode=disable' ./bin/crashctl migrate up`, then
   the same URL with `make audit ROUND_ID=0190f3a8-7b3c-7d4e-8f00-0000000000ff`: expect crashctl's `not found` line,
   `audit: crashctl audit export failed with exit code 5`, `Error 5`, make exit 2; finally
   `docker compose -f deployments/docker/compose.yaml down -v`. The database has no executor, so the success path
   is proven by the integration tests, not by this smoke.
8. `git status --short` lists exactly: `M Makefile`, `M README.md`, `M AGENTS.md`,
   `?? cmd/crashctl/make_audit_integration_test.go` (plus this spec under `docs/specs/`). `bin/` is ignored.

## 5. Acceptance mapping

| Requirement | Where |
|---|---|
| builds `crashctl`, exports to a temporary 0600 file, removes it | §3.1 (`build-crashctl`, `mktemp -d`, crashctl `O_EXCL` 0600, `EXIT` trap); tests 2-5 `requireEmptyDir` |
| verifies with `-expect-head` = exported head | §3.1 `sed` + `audit verify -expect-head`; tests 2-3 `chain ok records … head …` |
| clear non-zero failure for missing `ROUND_ID` / `ORVEX_DATABASE_URL` | §3.1 checks; test 1 |
| propagates `crashctl`'s exit code | §3.1 `exit "$rc"`, make `Error N` (§2 item 1); tests 3-5 (codes 1, 2, 3, 4, 5, 6) |
| `AUDIT_FORMAT` → `-format`, `AUDIT_UNSETTLED` → `-unsettled` | §3.1 `set --`; tests 3, 4, 5 `verify-fails`, test 1 `bad-audit-unsettled` |
| real PostgreSQL round via `pgtest/roundtest` | tests 2-5 use `drained` → `roundtest.SeedGolden` + executor `Drain` |
| README and AGENTS.md document usage and failures | §3.3, §3.4 |

## 6. Out of scope

`crashctl` behaviour, CI workflows, seed protection and error masking (LBT-21), history format v2 export,
`docs/changes/` write-ups, `docs/architecture/`, any other README/AGENTS.md line.
