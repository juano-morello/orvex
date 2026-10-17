# Plan: LBT-27, nightly `FuzzSingleByteFlip` fails at the end of `-fuzztime` without a failing input

Run `b579ac21`. Issue: LBT-27. Base commit: `d09e173`.
Governing ADRs: ADR-006 (CI: "Nightly/release: extended fuzz"), ADR-007 (Makefile orchestration,
GitHub Actions, single module, pinned actions). Related spec: `docs/specs/2cd18d70_ci-fuzz-repeat-runs.md`
(LBT-20, which created `nightly.yml` and the Makefile fuzz targets).

## 0. Root cause (verified while planning)

**The verifier and the fuzz target are not at fault. The Go 1.26 fuzz coordinator has a race
(golang/go#75804) that can turn the normal end of `-fuzztime` into a test failure with the message
`context deadline exceeded`. It is fixed in Go 1.27.0 and was not backported to Go 1.26.**

### 0.1 What the failed job printed

`gh run view 38940164866 --repo juano-morello/orvex --log-failed` (the issue text left out the message line):

```
fuzz: elapsed: 4m57s, execs: 517821 (1731/sec), new interesting: 243 (total: 248)
fuzz: elapsed: 5m0s, execs: 523090 (1756/sec), new interesting: 244 (total: 249)
fuzz: elapsed: 5m1s, execs: 523090 (0/sec), new interesting: 244 (total: 249)
--- FAIL: FuzzSingleByteFlip (301.01s)
    context deadline exceeded
FAIL
exit status 1
FAIL	github.com/juano-morello/orvex/internal/audit/verification	301.022s
```

- The failure message is `context deadline exceeded`. It is not `Failing input written to ...`,
  `fuzzing process hung or terminated unexpectedly`, or `fuzzing process terminated by unexpected
  signal`.
- The exec rate held steady (1,708, then 1,731, then 1,756/s) through `elapsed: 5m0s`. The
  `5m1s ... (0/sec)` line is the coordinator's deferred final `logStats()` after the workers stop.
  It is normal. A passing local run ends the same way: see §0.4 run R1 and the scratch log
  `context_handoff/scratch/fuzzdebug30s.log`. In that log the workers stop about 1 s after the
  deadline (`workerTimeoutDuration`), which explains 301.01 s.

### 0.2 The race in the Go 1.26 toolchain (`go1.26.5`, `$(go env GOROOT)/src`)

- `internal/fuzz/fuzz.go` `CoordinateFuzzing`: `ctx` is `context.WithTimeout(ctx, opts.Timeout)` (the
  `-fuzztime`), and `fuzzCtx, cancelWorkers := context.WithCancel(ctx)`. The event loop's
  `case <-doneC:` calls `stop(ctx.Err())` (line 228). `stop` hides the error only
  `if err == fuzzCtx.Err() || isInterruptError(err)` (line 129).
- `context/context.go` `(*cancelCtx).cancel` (lines 549-579) stores `err`, closes `done`, and only
  then cancels the children. `(*cancelCtx).Err` (lines 463-471) is a lock-free atomic load. So the
  coordinator can wake on `ctx.Done()` and read `fuzzCtx.Err()` while it is still `nil`. Then
  `DeadlineExceeded != nil`, the error is not hidden, and it becomes `fuzzErr`.
- `testing/internal/testdeps/deps.go` `CoordinateFuzzing` drops only `err == ctx.Err()` of its own
  SIGINT context. `testing` then prints the error and fails the target. No input caused the failure,
  so none is written.
- Line 228 is the only `stop(...)` call in `fuzz.go` that passes a context error. Worker goroutine
  errors are dropped first when `fuzzCtx.Err() != nil` (fuzz.go lines 188-197). So no other path
  produces a bare `context deadline exceeded`.
- Upstream: golang/go#75804, "testing: Fuzz testing with `fuzztime` flag sometimes fails incorrectly
  with "context deadline exceeded"" (opened 2025-10-08 against go1.25.0). Fixed by CL 774140,
  master commit `5a957dc766ae` (2026-07-23), which adds `err == ctx.Err() ||` to the check. That fix
  is on `release-branch.go1.27` as `184ce1510b85` (2026-07-27) and is present in `go1.27.0`,
  `go1.27.1` and `go1.27.2`. It is absent from `go1.26.5` (local) and from `go1.26.9` (the latest
  1.26 patch). No 1.26 backport exists. Checked through the GitHub API on `golang/go`.
- The race depends on timing, not on the target. Any of the 11 nightly fuzz jobs can hit it.
  `FuzzSingleByteFlipV2` passed in the same run by chance. The issue scope therefore includes every
  fuzz target, which share one invocation, and the fix goes in that shared invocation.

### 0.3 Causes ruled out

| Hypothesis | Ruled out because |
|---|---|
| Slow or pathological input (quadratic work, very long line) | Every input is the 11,492-byte `worked-example.jsonl` with one byte XORed, so its length is fixed and `readLine` is linear and far below `MaxLineBytes`. Any flip inside a record line changes that line's `LinkHash`, so the chain stage rejects it before replay. A header flip costs at most one replay of the 23 fixed records. The work per input is bounded, and the exec rate stayed flat to the last tick. |
| Verifier hang | A hung input stalls one of 4 workers (rate drops by about ¼) and ends in `fuzzing process hung or terminated unexpectedly` or `waiting for fuzzing process to terminate...` with a crasher. Neither happened, and the job ended 1.01 s after the deadline, which is normal worker shutdown. |
| Test deadline or cleanup | `-timeout` (default 10m) is stopped before `runFuzzing` (LBT-20 §0), and a test timeout prints `panic: test timed out`. `FuzzSingleByteFlip` has no `t.Cleanup` or `TestMain`. |
| Runner resources (OOM) | A killed worker reports `fuzzing process terminated by unexpected signal; no crash will be recorded`. |
| `-fuzztime` and worker shutdown | Confirmed. This is the cause, inside the toolchain (§0.2). |

### 0.4 Local evidence (planning host: 32 cores, go1.26.5, scratch under `context_handoff/scratch/`)

- **R1**: `go test -race -run='^$' -fuzz='^FuzzSingleByteFlip$' -fuzztime=5m -parallel=4 ./internal/audit/verification`
  exited 0 after 302 s with 784,647 execs (about 2,000/s, flat). It ended with the same
  `elapsed: 5m1s ... (0/sec)` line, then `PASS`. The log is `context_handoff/repro1.log`. The bug is
  rare per run. It did not occur in this run.
- **Window measurement**: `scratch/ctxrace/main.go` copies the coordinator's context shape and check.
  `GOMAXPROCS=4 go run . -n 30000 -busy 3` printed
  `go1.26.5: go1.26 check: 5/30000 deadline errors not suppressed (0.017%); go1.27 check: 0/30000`.
  So the window exists under CPU contention, and the go1.27 check closes it.
- **Short fuzz loops** (`scratch/shortfuzz.sh`, a prebuilt `-race` test binary, `-test.fuzztime 1s`,
  4 workers). **This reproduces the failure locally:**
  - go1.26.5, unpinned: 150 runs, 148 passed, **2 failed** (runs 113 and 129). Both printed exactly
    `--- FAIL: FuzzSingleByteFlip (2.0xs)` / `    context deadline exceeded` / `FAIL`. Neither wrote a
    failing input (`git status` stayed clean).
  - go1.26.5, `taskset -c 0-3` (a 4-vCPU runner saturated by its 4 workers): **2 more failures**
    (runs 28 and 62), with the same two lines. The job ended before it printed its totals, so only
    the failures are certain.
  - go1.27.2 binary (`GOTOOLCHAIN=go1.27.2 go test -c -race`), same loop: no failure was recorded. The
    job ended before it printed its run count, so treat this as indicative only. The builder repeats
    the comparison as V7 (§6).
  - Logs are in `context_handoff/scratch/logs-race-126*/fail-*.log`. The loops ran a test binary
    built without `-fuzz`, so they print a "no coverage instrumentation" warning. That does not touch
    the coordinator's shutdown path.
- `GOTOOLCHAIN=go1.27.2` downloads and runs go1.27.2. `GOTOOLCHAIN=go1.27.2 go vet ./...` and
  `GOTOOLCHAIN=go1.27.2 go test -c -race ./internal/audit/verification` succeed. go.mod stays as it is.
- `GODEBUG=fuzzdebug=1` (an undocumented `internal/fuzz` switch) added 433 `DEBUG` lines to a
  30-second run. Most come from the baseline corpus pass, plus one line per new interesting input. It
  also logs why the coordinator stopped: `stop called at .../internal/fuzz/fuzz.go:228. stopping: false`
  (228 is the deadline path, 232 a worker exit, 288 a written crasher).
- Go starts fuzz workers with no `Stdout`/`Stderr` (`internal/fuzz/worker.go` never sets them), so a
  worker's own stderr goes to `/dev/null` and cannot be captured from outside. The coordinator's
  output, with `fuzzdebug=1`, is what can be captured.

## 1. Decision

1. **Fix in the invocation. Run the nightly fuzz jobs on Go 1.27.x**, the first release line with
   the coordinator fix, through a separate `FUZZ_GO_VERSION` in `nightly.yml`. The `fuzz` job also
   refuses a toolchain older than go1.27.0, so that a revert to 1.26 cannot quietly bring the flake
   back.
   - `go.mod` keeps `go 1.26.0`. With that `go` line, Go 1.27 compiles the module with Go 1.26
     language semantics and Go 1.26 GODEBUG defaults. Only the toolchain's `testing`/`internal/fuzz`
     harness changes for the code under test.
   - PR CI (`ci.yml`), the nightly `repeat` jobs, `make build` and `make test` stay on 1.26.x.
2. **Do not change the fuzz target, the verifier, or any frozen format, fixture or golden file.**
   The property ("any single-byte flip never verifies") stays exactly as it is.
3. **Surface why a fuzz job failed when no failing input was written.**
   - `GODEBUG=fuzzdebug=1` on the fuzz step.
   - The full output is kept as a log.
   - On failure, a `fuzz-debug-<id>` artifact holds that log and the target's fuzz cache corpus.
   - The step summary says whether a failing input was written. When none was, it quotes the end of
     the log.
4. **Document the toolchain issue for local `make fuzz` / `make fuzz-one`** in `AGENTS.md` and
   `README.md`, with the local remedy `GOTOOLCHAIN=go1.27.2`.

Rejected alternatives. Do not implement them:
- Matching the output text (`context deadline exceeded`) in the Makefile and passing the run: it
  scans text, is fragile, and can hide a real failure.
- Retrying a failed fuzz run: it hides real failures and doubles the cost.
- `-fuzztime=Nx` (an exec count): it avoids the timeout path but loses the time bound.
- Stopping the run with SIGINT instead of `-fuzztime`: it needs a prebuilt binary, a manual
  `-test.fuzzcachedir` and signal plumbing. It is too complex.
- Moving the whole repository to Go 1.27: that is a separate decision, out of scope here.

### ADR and convention check

There is no ADR conflict.
- ADR-006's nightly extended fuzz is kept, with the same targets, properties and time bound.
- ADR-007 names no toolchain version. The module, its `go` line and its dependencies stay unchanged.
- AGENTS.md's "Go 1.26" describes the module's `go` line, which does not change. The nightly fuzz jobs
  now build with a newer toolchain than PR CI. This plan, the AGENTS.md text and the PR description
  must say so explicitly.

## 2. Files to change (complete list)

- `.github/workflows/nightly.yml`
- `AGENTS.md`
- `README.md`
- this plan's copy, `docs/specs/b579ac21_nightly-fuzz-deadline-flake.md` (already written)

Must NOT change:
- `Makefile`. The `fuzz` and `fuzz-one` recipes are correct. The toolchain comes from the
  environment, and `make fuzz-guard-check` must keep passing as it does now.
- `go.mod` and `go.sum`.
- `.github/workflows/ci.yml`.
- Anything under `internal/` or `cmd/`.
- Any `testdata/`, golden file or migration.
- `docs/adr/`.
- The earlier specs in `docs/specs/`.

## 3. Step 1: `.github/workflows/nightly.yml`

Keep the triggers, the `permissions`, `concurrency` and `defaults` blocks, the `fuzz-targets` job,
the `repeat` job, every pinned `uses:` SHA, the matrix and `timeout-minutes` as they are.
`defaults.run.shell: bash` must stay: GitHub runs it as `bash --noprofile --norc -eo pipefail {0}`
(visible in the failed job's log), so `make ... | tee` keeps `make`'s exit status.

### 3.1 Workflow `env`

Replace the `env:` block with:

```yaml
env:
  GO_VERSION: "1.26.x"
  # Fuzz jobs only (LBT-27). Go 1.26 can end a fuzz run that found nothing with
  # a spurious "context deadline exceeded" failure when -fuzztime expires
  # (golang/go#75804). The fix shipped in Go 1.27.0 and was not backported.
  # go.mod keeps "go 1.26.0", so the code under test keeps Go 1.26 language
  # semantics and GODEBUG defaults.
  FUZZ_GO_VERSION: "1.27.x"
  FUZZTIME: ${{ inputs.fuzztime || '5m' }}
```

### 3.2 The `fuzz` job

Replace the `fuzz` job's `env:` and `steps:` with the following. `needs`, `name`, `runs-on`,
`timeout-minutes` and `strategy` stay as they are.

````yaml
    env:
      FUZZ_PKG: ${{ matrix.target.pkg }}
      FUZZ_TARGET: ${{ matrix.target.fn }}
      ARTIFACT: fuzz-crasher-${{ matrix.target.id }}
      DEBUG_ARTIFACT: fuzz-debug-${{ matrix.target.id }}
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
        with:
          go-version: ${{ env.FUZZ_GO_VERSION }}
          cache-dependency-path: go.mod
      - name: Fuzz
        env:
          # The coordinator logs its decisions, including why it stopped
          # ("stop called at .../internal/fuzz/fuzz.go:<line>").
          GODEBUG: fuzzdebug=1
        run: |
          go version
          if ! printf '%s\n' go1.27.0 "$(go env GOVERSION)" | sort -V -C; then
            echo "::error::fuzz toolchain $(go env GOVERSION) predates go1.27.0, which fixes golang/go#75804 (LBT-27)"
            exit 1
          fi
          mkdir -p "$RUNNER_TEMP/fuzz-debug"
          make fuzz-one FUZZ_PKG="$FUZZ_PKG" FUZZ_TARGET="$FUZZ_TARGET" FUZZTIME="$FUZZTIME" 2>&1 | tee "$RUNNER_TEMP/fuzz-debug/fuzz.log"
      - name: Upload failing input
        if: failure()
        uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7.0.2
        with:
          name: ${{ env.ARTIFACT }}
          path: ${{ matrix.target.pkg }}/testdata/fuzz/${{ matrix.target.fn }}/
          if-no-files-found: warn
          retention-days: 30
      - name: Collect fuzz diagnostics
        if: failure()
        run: |
          mkdir -p "$RUNNER_TEMP/fuzz-debug"
          if command -v go >/dev/null; then
            cache="$(go env GOCACHE)/fuzz/$(go list -m)/${FUZZ_PKG}/${FUZZ_TARGET}"
            if [ -d "$cache" ]; then
              cp -R "$cache" "$RUNNER_TEMP/fuzz-debug/corpus"
            fi
          fi
      - name: Upload fuzz diagnostics
        if: failure()
        uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7.0.2
        with:
          name: ${{ env.DEBUG_ARTIFACT }}
          path: ${{ runner.temp }}/fuzz-debug/
          if-no-files-found: warn
          retention-days: 30
      - name: Summarize the failure
        if: failure()
        run: |
          crashers="${FUZZ_PKG}/testdata/fuzz/${FUZZ_TARGET}"
          log="$RUNNER_TEMP/fuzz-debug/fuzz.log"
          {
            echo "### ${FUZZ_TARGET} (${FUZZ_PKG}) failed"
            echo ""
            if [ -d "$crashers" ] && [ -n "$(git status --porcelain --untracked-files=all -- "$crashers")" ]; then
              echo "The fuzzer wrote a failing input: artifact \`${ARTIFACT}\` holds it."
              echo "Copy it into \`${crashers}/\` and reproduce with"
              echo "\`go test -run=${FUZZ_TARGET}/<file> ./${FUZZ_PKG}\`."
            else
              echo "The fuzzer wrote no failing input, so no input of the target failed."
              echo "The end of the fuzz output below says why the run stopped. Artifact"
              echo "\`${DEBUG_ARTIFACT}\` holds the full output (with \`GODEBUG=fuzzdebug=1\`)"
              echo "and the fuzz cache corpus."
            fi
            if [ -f "$log" ]; then
              echo ""
              echo '```'
              tail -n 40 "$log"
              echo '```'
            fi
          } >> "$GITHUB_STEP_SUMMARY"
````

Design notes. Do not deviate from them.
- **Toolchain guard.** `sort -V -C` exits 0 only when `go1.27.0 <= GOVERSION`. The planner checked
  it: go1.26.5 and go1.26.9 are refused; go1.27.0, go1.27.2, go1.28.0 and go1.30.1 pass. `setup-go`
  sets `GOTOOLCHAIN=local`, so `go env GOVERSION` is the installed toolchain.
- **`GODEBUG` scope.** `GODEBUG` is set on the Fuzz step only. `fuzzdebug` is read only by
  `internal/fuzz`; cmd/go and the code under test ignore it, and an unknown key is ignored too. Expect
  roughly 1,000 extra log lines per 5-minute job.
- **New crasher check.** "Failing input written" is detected with `git status --porcelain
  --untracked-files=all` on the target's `testdata/fuzz/<FuzzName>/` directory, so committed
  regression inputs there never count as new. The `[ -d ... ]` guard keeps `git status` from warning
  `could not open directory` when the directory does not exist. Today no `testdata/fuzz/` directory
  is committed. The
  existing `Upload failing input` step stays unchanged, and so does its artifact name.
- **Corpus path.** The fuzz cache is `$(go env GOCACHE)/fuzz/<module path>/<package dir>/<FuzzName>`.
  The planner confirmed it locally:
  `~/.cache/go-build/fuzz/github.com/juano-morello/orvex/internal/audit/verification/FuzzSingleByteFlip/`,
  400 files of about 50 bytes each.
- **Robust failure steps.** Each `if: failure()` step must work when an earlier step (checkout,
  setup-go) failed: the log, the cache or `go` itself may be missing. Keep the guards (`command -v go`,
  `[ -d ... ]`, `[ -f ... ]`).
- **No `-v`.** Under `-fuzz` it adds only `=== RUN`/`=== FUZZ` lines. Do not add it.
- **No `GORACE=log_path`.** It would move coordinator race reports out of the console. Do not add it.

## 4. Step 2: `AGENTS.md`

Edit only the Commands section. Do not change the ADR table or the conventions.

- In the `make fuzz` bullet, after the sentence that ends `... and fix the cause.`, append:

  > With Go 1.26.x a fuzz run can fail when its `FUZZTIME` ends, with only `context deadline exceeded`
  > and no failing input. That is the toolchain bug golang/go#75804, fixed in Go 1.27.0 and not
  > backported. It is not a finding (LBT-27). The nightly fuzz jobs therefore build with Go 1.27.x
  > (`FUZZ_GO_VERSION` in `nightly.yml`; `go.mod` stays at `go 1.26.0`). Locally, prefix the command
  > with `GOTOOLCHAIN=go1.27.2` (or a later release), for example
  > `GOTOOLCHAIN=go1.27.2 make fuzz-one FUZZ_PKG=internal/audit/verification FUZZ_TARGET=FuzzSingleByteFlip`.

- In the `CI:` bullet, insert this text after the sentence ending ``the three `make test-repeat-*` suites.``
  and before `Pin every GitHub Action ...`:

  > A failed fuzz job uploads `fuzz-crasher-<target>` (any failing input) and `fuzz-debug-<target>`:
  > the full fuzz output, run with `GODEBUG=fuzzdebug=1`, and the target's fuzz cache corpus. Its step
  > summary says whether a failing input was written and quotes the end of the output. The nightly
  > fuzz jobs build with Go 1.27.x. PR CI and the repeat suites stay on Go 1.26.x.

## 5. Step 3: `README.md`

- Line 38, the `make fuzz` bullet, currently ends ``... `make fuzz-guard-check` checks that `fuzz-one` refuses unknown names``.
  Append exactly this:
  ``; on Go 1.26.x a run can fail when `FUZZTIME` ends with only `context deadline exceeded` and no failing input (golang/go#75804, fixed in Go 1.27.0), so the nightly fuzz jobs use Go 1.27.x; locally, prefix the command with `GOTOOLCHAIN=go1.27.2` ``
- Line 40, the `CI:` bullet: change `(every fuzz target and the repeat runs)` to
  `(every fuzz target, on Go 1.27.x, and the repeat runs)`.

## 6. Tests and verification

There is no Go code change, so there is no new Go test. The fuzz target's property and seeds stay as
they are, and `make test` still runs every seed corpus (ADR-006 PR "fuzz smoke"). A Go test cannot
exercise the shutdown race in cmd/go's fuzz coordinator. Each behaviour change is checked as follows.
Run the commands one at a time, and record the exact command, exit status and duration in the report.
Your shell moves a command that runs past 60 s into a background job: wait for it to finish, and
never report before you have its final exit status.

| # | Behaviour change | Check |
|---|---|---|
| V1 | `nightly.yml` is valid and its `run:` scripts are clean | `go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 .github/workflows/nightly.yml .github/workflows/ci.yml` exits 0. `shellcheck` is installed at `/usr/bin/shellcheck`, so actionlint also checks the `run:` scripts. `go run pkg@version` does not touch `go.mod`/`go.sum`; confirm with `git status --porcelain go.mod go.sum` (empty). |
| V2 | The toolchain guard refuses Go < 1.27.0 | Run the guard line in bash with `go env GOVERSION` replaced by each literal `go1.26.9`, `go1.27.0` and `go1.27.2`. Expect refused, ok, ok. |
| V3 | The summary and the collection steps work in both cases | Copy the `Collect fuzz diagnostics` and `Summarize the failure` `run:` bodies into a scratch script under the session's `context_handoff/` directory (not the repo). Run it with `FUZZ_PKG=internal/audit/verification FUZZ_TARGET=FuzzSingleByteFlip RUNNER_TEMP=<scratch dir> GITHUB_STEP_SUMMARY=<scratch file> DEBUG_ARTIFACT=x ARTIFACT=y`, from the repo root. (a) Write a `fuzz.log` and leave no crasher. The summary must say "wrote no failing input", end with the log's last 40 lines, and `corpus/` must be copied. (b) Create an untracked file `internal/audit/verification/testdata/fuzz/FuzzSingleByteFlip/deadbeef`. The summary must say "wrote a failing input". Delete the file and the empty directories afterwards, and make sure `git status` shows nothing new under `testdata/`. (c) Run it with no `fuzz.log`. It must exit 0 and print no code block. |
| V4 | The nightly fuzz command passes a 5-minute run on the fixed toolchain | `GOTOOLCHAIN=go1.27.2 GODEBUG=fuzzdebug=1 make fuzz-one FUZZ_PKG=internal/audit/verification FUZZ_TARGET=FuzzSingleByteFlip FUZZTIME=5m` exits 0. Then run the plain repo toolchain `make fuzz-one FUZZ_PKG=internal/audit/verification FUZZ_TARGET=FuzzSingleByteFlip FUZZTIME=5m` once, as the issue asks. It is expected to pass, because the race is rare per run (§0.4). If it fails with only `context deadline exceeded`, report it as a reproduction of golang/go#75804, not as a regression. |
| V5 | The make targets still behave | `make fuzz-guard-check` exits 0. `make -s fuzz-list` still prints the 11 targets. |
| V7 | The fixed toolchain removes the spurious failure (before/after) | Build `GOTOOLCHAIN=go1.27.2 go test -c -race -o <scratch>/v127.test ./internal/audit/verification` and `go test -c -race -o <scratch>/v126.test ./internal/audit/verification`. Run each 100 times from `internal/audit/verification` under `taskset -c 0-3`, with `-test.run '^$' -test.fuzz '^FuzzSingleByteFlip$' -test.fuzztime 1s -test.parallel 4 -test.fuzzcachedir <scratch>/cache`. `context_handoff/scratch/shortfuzz.sh <binary> 100 1s 4 <logdir>` does exactly this and prints `runs= pass= fail=`. Expect go1.27.2 `fail=0`. Report both counts; go1.26.5 is expected to show a few `context deadline exceeded` failures. Run the two loops one after the other, never at the same time. |
| V6 | Nothing else regressed | One at a time: `make build`, `make test`, `make lint`, `make golden-check`, `make test-integration` (needs Docker). All exit 0. |

After merge (a human, not the builder: dispatching a workflow is an external write), run
`gh workflow run nightly.yml -f fuzztime=1m` once. Check that each fuzz job logs `go version go1.27.x`
and the `DEBUG` lines. If a job fails, check that `fuzz-debug-<id>` is uploaded and the summary
appears.

## 7. Report and PR notes

- Root cause: §0.2, citing golang/go#75804 and upstream commits `5a957dc766ae` / `184ce1510b85`.
- State that there is no ADR deviation, and that the nightly fuzz jobs build with Go 1.27.x while
  `go.mod`, PR CI and the repeat jobs stay on Go 1.26.x (§1).
- Once the repository moves to Go ≥ 1.27, `FUZZ_GO_VERSION` can be folded back into `GO_VERSION`.
- Changed files: exactly those in §2.
