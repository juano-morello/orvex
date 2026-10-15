# `crashctl audit export` (LBT-19)

## What changed

`crashctl audit export [-unsettled] [-format NAME] ROUND_ID FILE` reads one round's audit history from PostgreSQL (`ORVEX_DATABASE_URL`) and writes a file that `crashctl audit verify -expect-head` accepts. Until now the history could be verified but not produced by a CLI. The approved design is `docs/specs/73dbe98d_audit-export-cli.md` (added in this change, marked approved; decisions X-1 to X-13). `docs/specs/2105c14c_audit-export-implementation.md` is also added.

Behaviour, as shown in `cmd/crashctl/export.go`:

- `ROUND_ID` must be a canonical lowercase UUID, not nil. `FILE` of `-` writes the history to stdout and the report to stderr.
- `-format` accepts only `orvex/audit-history/v1` (default). A well-formed other version (`orvex/audit-history/vN`) exits 3; anything else exits 2.
- Output file: refused if the path exists (dangling symlink included) or its directory is unusable, both checked before connecting. It is created `O_EXCL`, mode 0600, synced, and removed on any write failure. The history is buffered in memory before anything is written.
- Session: one connection, `default_transaction_read_only=on` always, `application_name=crashctl` and `idle_in_transaction_session_timeout=60000` unless the DSN sets them. The only transaction is `ExportHistory`'s read-only snapshot.
- Settlement gate: every v1 file contains `configSecret` (the seed). The exported bytes are replayed and the `execution` policy for the file's rules version (`execution.CrashV1Policy()`, from `settlementPolicies()` in `main.go`) is asked whether the final state is settled. Not settled exits 6. If settlement cannot be established (replay failure, no policy, `Final` error) the command refuses too. `-unsettled` exports anyway and the report prints a "keep it private" warning.
- The file ends at the round's newest snapshot. If records exist after it, the report has a `note:` naming the omitted sequences.
- Report (stdout, or stderr for `-`): `exporter`, `format`, `round`, `records N of M head HEX`, optional note, `settled yes|no|unknown`, optional warning, `ok`.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | written |
| 1 | export or settlement check failed (no snapshot, corrupt round, verification failure) |
| 2 | usage, bad config, output file exists |
| 3 | unsupported format or rules version (no settlement policy) |
| 4 | I/O, including every database error |
| 5 | round not found (new) |
| 6 | round not settled (new) |

## Supporting changes

- `cmd/crashctl/database.go` (new): `databaseConfig`, `mask`, `oneLine` moved out of `migrate.go` and shared by `migrate` and `export` (10 s connect timeout default, password masked, DSN parse errors never echoed). `migrate.go` now calls them.
- `cmd/crashctl/main.go`: dispatches `audit export`, adds `exitNotFound`/`exitNotSettled`, usage text, and `newRegistry()` shared with `audit verify`.
- `internal/persistence/eventstore/export.go`: `Export.RoundRecords` (round's total chain records, read in the same snapshot via `max(sequence)`). Written bytes are unchanged. `ErrCorrupt` if the round has no records or the newest is before the snapshot.
- `internal/audit/verification/verification.go`: `Report.State`, the replayed state in replay/verify mode (nil in chain mode). Additive.
- `docs/architecture/persistence.md`: new section 14 (CLI, reading, settlement gate) and an auditor grant row in the section 8 matrix: `SELECT` on `rounds` and `chain_records`, column `SELECT` on `round_snapshots (round_id, sequence, state_hash)` and `round_secrets (round_id, config_secret)`. Note that this exposes every round's seed.
- `README.md`: lists the command.

History format v1 and golden fixtures are not touched.

## Tests

- `cmd/crashctl/export_test.go`: usage, format, file-exists, missing directory, connect errors, password masking, session config, exit-code mapping, settlement, policies cover the registry, report.
- `cmd/crashctl/export_integration_test.go` and `export_helpers_integration_test.go` (integration tag, real PostgreSQL): executed rounds export and verify with `-expect-head`, snapshots, stdout, unknown round, no snapshot, corrupt round, unsupported rules, database errors, minimal grants, output failures.
- `main_test.go`, `verification_test.go`, `eventstore/export_integration_test.go`: usage line, `Report.State`, `RoundRecords`.

## Verify

```
make test
make test-integration   # needs Docker
make lint
make build
make golden-check
ORVEX_DATABASE_URL=... bin/crashctl audit export ROUND_ID out.json && bin/crashctl audit verify -expect-head HEX out.json
```

I did not run these; the diff does not record results. The spec requires some checks with `-count=N`; see section 3 of the spec for the exact commands.
