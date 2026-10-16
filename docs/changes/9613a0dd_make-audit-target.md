# `make audit`: export and verify one round (LBT-25)

## What changed
`make audit ROUND_ID=<uuid>` is the audit target ADR-007 lists, which LBT-9 deferred until PostgreSQL exports existed. It builds `crashctl`, runs `crashctl audit export` for the round against the database in `ORVEX_DATABASE_URL`, then runs `crashctl audit verify -expect-head` with the head the export printed. `crashctl` itself is unchanged.

- The export goes into a private `mktemp -d` directory. `crashctl` creates the file with mode 0600. The directory is removed on every exit, including HUP, INT and TERM.
- `AUDIT_FORMAT=NAME` passes `-format NAME`. `AUDIT_UNSETTLED=1` passes `-unsettled`. The temporary file then holds the round's unrevealed secret config. Any other `AUDIT_UNSETTLED` value is rejected.
- A missing `ROUND_ID`, a missing `ORVEX_DATABASE_URL` or a bad `AUDIT_UNSETTLED` stops with an `audit:` message and exit 2, before the build.
- A failed step prints `audit: crashctl audit export|verify failed with exit code N`. The recipe exits with `crashctl`'s code, so make reports `Error N` and itself exits 2. If the export prints no head, the target exits 1.
- The expected head comes from the same database as the file. The run therefore checks the export end to end but is not an independent anchor (ADR-001). Keep the printed head elsewhere to anchor it.

## Files
- `Makefile`: the `audit` target, the `ROUND_ID`, `AUDIT_FORMAT` and `AUDIT_UNSETTLED` variables, and the `.PHONY` entry.
- `cmd/crashctl/make_audit_integration_test.go`: integration tests (`-tags integration`). They run `make audit` against real PostgreSQL rounds created through `pgtest/roundtest`, each with its own `BIN_DIR`. They are `TestMakeAuditMissingVariables`, `TestMakeAuditSettledRounds`, `TestMakeAuditUnsettled`, `TestMakeAuditFormat` and `TestMakeAuditFailures`.
- `README.md`: new "Audit" subsection.
- `AGENTS.md`: one line in the command list.
- `docs/specs/9613a0dd_make-audit-target.md`: the spec for this work.

## Use and verify
```
ORVEX_DATABASE_URL='postgres://orvex:orvex@127.0.0.1:55432/orvex?sslmode=disable' make audit ROUND_ID=<uuid>
make test-integration   # needs Docker; covers the TestMakeAudit* tests
```
