# Protect round seeds at rest and restrict secret access (LBT-21)

Status: **APPROVED (2026-10-16).** The engineer approved K-1 to K-15 as written, with no edits, and
accepted every point of section 3, including item 8: `D` and `T0 + D` stay plaintext, a residual risk
tracked separately as LBT-26. Section 0 described the planning run and no longer applies;
implementation follows section 9.

- Linear: [LBT-21](https://linear.app/lbtworks/issue/LBT-21/protect-round-seeds-at-rest-and-restrict-secret-access),
  milestone M2 · Durable Execution. Related: LBT-10, LBT-14 (roles and grants), LBT-19.
- Linear text: "The round seed is the fairness secret until settlement (ADR-004, math spec D-10). […]
  Acceptance: seeds are encrypted at rest with a documented key-management approach; the export role
  cannot read the secret of an unsettled round without an explicit privileged path; `crashctl` never
  prints any part of the configured database URL in errors; integration tests prove each point.
  Coordinate with LBT-14 (roles and grants). Reference ADR-003/004/005."
- Governing ADRs (mirrors in `docs/adr/`): ADR-004 (256-bit seed, SHA-256 commitment before betting,
  reveal after settlement), ADR-003 (pure `Decide`/`Evolve`; the public projection never exposes the
  secret outcome), ADR-005 (PostgreSQL system of record, transactions A and B), ADR-001 (replay from
  recorded inputs), ADR-006 (real PostgreSQL integration tests, fail closed), ADR-007 (one module,
  `internal/` boundaries, minimal pinned dependencies).
- Approved inputs (all on `main`):
  - LBT-9 `docs/specs/8bebb70f_canonical-codec-audit-cli.md`: A-11 (`configSecret` is required in v1),
    A-12 (the commitment and the reveal).
  - LBT-10 `docs/specs/17380a6e_postgresql-schema-migrations.md`: P-6 (`crashctl migrate`), P-13
    (append-only tables, grants), P-25 (secret stored unencrypted, encryption deferred).
  - LBT-11 `docs/specs/5a3d52fc_admission-sequencer.md`: S-8 (the admission window).
  - LBT-12 `docs/specs/e0df34d3_execution-commit-ledger-outbox.md`: E-3 (cold load), E-13 (snapshots,
    `state` NULL), E-16 (round creation), and the engineer addendum of 2026-10-15 (the close offset `D`
    and the stored closure bound).
  - LBT-18 `docs/specs/ac81f88f_audit-ledger-stage.md`: v2 carries `configSecret` like v1.
  - LBT-19 `docs/specs/73dbe98d_audit-export-cli.md`: X-4, X-7, X-9, X-11, §2 item 9.
  - Math spec `docs/specs/e96d124d_crash-v1-math-spec.md`: D-9, D-10, §7.
- Decision IDs are `K-n`; no earlier spec uses that prefix.

## 0. What this run delivers

- This document is the deliverable. The engineer approves its copy at
  `docs/specs/87d8d90c_round-secret-protection.md`.
- **Builder, this run: make no repository changes.** The planner already wrote the spec file. Do not
  write Go code, SQL, migrations, tests or docs. Do not touch `go.mod`, `go.sum`, `Makefile`,
  `.golangci.yml`, `README.md`, `AGENTS.md`, `docs/adr/` or `docs/architecture/`. The gates must pass on
  the unchanged tree: `make test`, `make lint`, `make build`, `make golden-check`.
- After approval, a later run implements section 9.

## 1. Current state (verified while planning; `main` at `cd78841`)

- Toolchain go1.26.5. `go.mod` requires `cockroachdb/apd/v3 v3.2.3`, `jackc/pgx/v5 v5.11.0` and
  `testcontainers-go v0.44.0`. This plan adds no module: it uses only the standard library
  (`crypto/aes`, `crypto/cipher`, `crypto/rand`).
- `migrations/0002_rounds_admissions.up.sql:28-41`: `orvex.round_secrets(round_id uuid PK → rounds,
  config_secret bytea NOT NULL, CHECK octet_length >= 2)`, with append-only and no-truncate triggers
  (`orvex.forbid_mutation`, `OX001`). Its table comment reads "stored unencrypted".
- `migrations/0009_close_bound.up.sql`: adds `round_secrets.close_offset_micros` (the secret `D`) and
  `round_admission_heads.close_not_before_unix_micro` (`T0 + D`). `orvex.admit_command` is
  `SECURITY DEFINER`. At step 8 it reads `round_secrets.close_offset_micros` and never reads
  `config_secret`. `PUBLIC`'s `EXECUTE` on it is revoked.
- Production Go code touches `config_secret` in exactly three places:
  - `internal/runtime/execution/round.go:180` (`insertRound`, called by `CreateRound`) inserts the
    plaintext `secret` that `EncodeConfig` returns;
  - `internal/runtime/execution/load.go:35-38` (the cold load) reads it and passes it to `DecodeConfig`;
  - `internal/persistence/eventstore/export.go:78-79` and `export_v2.go:73-74` read it into the header
    `configSecret`.
- These tests insert or edit `round_secrets` directly: `pgtest/load.go:87` (a plaintext insert),
  `runtime/admission/helpers_integration_test.go:226`, `persistence/postgres/schema_integration_test.go:342`
  and `:421-425`, `persistence/postgres/close_bound_integration_test.go:36`,
  `cmd/crashctl/export_integration_test.go:339,351` (replica-mode delete and update), and
  `runtime/execution/round_integration_test.go:84-93`.
- Execution:
  - `execution.Config` has `{EngineBuild, LockTimeout, IdleInTransactionTimeout, MaxAttempts}`, every
    field validated, no zero-means-default.
  - `computeTransition` (`transition.go:190-193`) sets `tr.final` from `Policy.Final(tr.next)`.
  - `commit` (`commit.go:72-103`) queues one batch.
  - `CrashV1Policy().Final` is `Phase == PhaseSettled`. `Checkpoint` fires on `RoundStarted`,
    `RoundCrashed` and `RoundSettled`, so the settling record always gets a snapshot.
- crash/v1:
  - `EncodeConfig` returns the public config (it holds `commitment`) and the secret config
    `{"seed":"<64 hex>"}` (`codec.go:43-60`).
  - `RoundSettled` carries `seed` (`codec.go:215`, `events.go:52-59`). So once a round settles, its seed
    is already stored in plaintext in its chain record, and the auditor can read it.
  - `C`, `n_c` and `D` determine each other (math spec §7).
- crashctl:
  - `cmd/crashctl/database.go:44-51` `mask` replaces only `cfg.Password`.
  - A connect failure is a `*pgconn.ConnectError`. Its text starts with
    ``failed to connect to `user=<user> database=<db>`:`` (pgx v5.11.0 `pgconn/errors.go:67-75`), then
    host and address details. Server errors at connect repeat the user or the database (28P01, 3D000).
    After connect, a server error can name the database (42501 "permission denied for database …" when
    `migrate up` lacks `CREATE`), and network errors name the host and port.
  - `export.go` hands `err.Error()` strings to `fail`. `migrate.go` hands errors to `fail`.
  - `migrate status` prints `applied by <applied_by>`, where `applied_by` is `buildinfo.Version`
    (`migrate.go:252`). It does not come from the DSN.
- `-unsettled` (`export.go:79`) writes the file whatever the settlement check says. The test auditor
  grants include `SELECT (round_id, config_secret) ON orvex.round_secrets`
  (`export_helpers_integration_test.go:257-259`).
- Golden histories:
  - settled: `worked-example` (`RoundSettled` at 21, then record 22, a rejected `PlaceBet`),
    `instant-crash` (5), `empty-round` (3);
  - unsettled: `in-progress` (last record 3).

  The execution and crashctl test fixtures insert admissions directly instead of calling
  `admit_command`, so their admission windows stay `open`.
- `round_snapshots.state` is always NULL (E-13, `commit.go:34`).
- `cmd/engine` and `cmd/gateway` are scaffolds that load no configuration. LBT-14 wires the engine.
- Schema `orvex` holds 14 tables (`TestTruncateRejectedEverywhere`).

## 2. Threat model and goals

Assets:

- **S1**: the secret config of an unsettled round (crash/v1: the seed). It is secret until
  `RoundSettled` (ADR-004, D-10).
- **S2**: `D` and `T0 + D`. They give the crash tick and are secret until `RoundCrashed`.
- **S3**: the database connection parameters: credentials, host, port, database name and options.
- **S4**: the keyring (K-2).

Adversaries in scope:

- **A1**: someone holding a copy of the database: a logical dump, a physical backup, a WAL archive, a
  replica or a disk snapshot.
- **A2**: a database role with read grants other than the engine's: the auditor, reporting, the gateway.
- **A3**: someone reading crashctl output: CI logs, terminal captures, ticket attachments.

Out of scope:

- compromise of an engine process or host: it must hold the key and the plaintext;
- a database owner or superuser who also has the keyring;
- operator seed grinding (math spec §7);
- side channels.

Goals:

- **G1**: for every round created after migration 0010, no database copy (A1) and no non-engine role (A2)
  yields an unsettled round's secret config without the keyring.
- **G2**: the auditor reads only the secrets that settlement revealed. Any other secret needs an
  explicit privileged role plus the keyring.
- **G3**: crashctl never prints any part of `ORVEX_DATABASE_URL` (A3).
- **G4**: existing exports, audits and golden histories still verify byte-for-byte. History formats v1
  and v2 are unchanged.

S2 at rest is not a goal of this issue; see K-1 and R-1.

## 3. ADR alignment, conflicts and flagged deviations

No decision contradicts an accepted ADR, and no ADR amendment is needed. Each point below needs the
engineer's explicit attention (AGENTS.md "Flag conflicts explicitly"). Copy this section into the
implementation PR.

1. **LBT-10 P-25, "verbatim and unencrypted" (approved decision, superseded).** K-3 and K-5 replace it
   for every new row. P-25 itself names "AES-256-GCM in the engine with a key from the environment" as
   the next step. P-25's spec stays unchanged as the record; `docs/architecture/persistence.md` §6 is
   rewritten.
2. **LBT-10 P-13, `round_secrets` is append-only (kept, with a consequence).** No row is ever updated.
   Rows written before 0010 keep their plaintext (K-11, R-2), and key rotation never re-encrypts (K-4).
3. **LBT-10 P-13 grant matrix, `persistence.md` §8, and LBT-19 X-11 (approved decisions, superseded).**
   The auditor loses every privilege on `round_secrets` and gains `SELECT` on `round_secret_reveals`
   (K-10, K-14).
4. **LBT-19 X-7, "`-unsettled` writes the file whatever the check says" (approved decision,
   changed).** For a round whose secret is not revealed, `-unsettled` now needs the privileged path: a
   role plus the keyring (K-10). Revealed rounds behave as before.
5. **LBT-19 X-9 exit codes (extended, with one changed case).**
   - Without `-unsettled`, a round with no revealed secret is exit 6, before any replay.
   - Changed case: a round that cannot be replayed, such as `tamper/rules-version-2` (unknown rules), was
     exit 3 by default. It is now exit 6, because no engine executed it and it has no reveal.
     `-unsettled` still reports the old classification.
   - A keyring problem is exit 2; a failed authentication of the sealed secret is exit 1 (K-10).
6. **LBT-19 X-4 and LBT-10 P-6, "the password masked in every message" (approved decision,
   strengthened).** K-12 replaces masking with a provenance rule. Server messages are no longer printed,
   except Orvex-authored messages of class `OX`. SQLSTATE codes and constraint names are still printed.
   Output stays one line, and exit codes do not change. LBT-19's example
   `eventstore: read secret: ERROR: permission denied for table round_secrets (SQLSTATE 42501)` becomes
   `eventstore: read revealed secret: SQLSTATE 42501`.
7. **LBT-12 E-16, E-3 and E-6 (approved decisions, changed).** `CreateRound` writes the sealed form; the
   cold load uses the reveal or opens the sealed form (K-8). Transaction B's batch gains one statement,
   on the record that finalizes the round (K-6). `Policy` does not change.
8. **LBT-12 engineer addendum (kept; residual risk).** `close_offset_micros` and
   `close_not_before_unix_micro` stay plaintext, because `orvex.admit_command` reads them inside
   PostgreSQL. They give the crash tick (math spec §7), so a database copy still reveals the crash tick
   of a round that has not crashed yet. K-1 leaves this out of scope and proposes a follow-up (R-1).
   **The engineer must explicitly accept this.**
9. **ADR-004, "reveal after settlement" (aligned).** `round_secret_reveals` is the reveal, written in
   the same transaction as the record that settles the round (K-6). `RoundSettled` already carries the
   seed (A-12).
10. **ADR-003 purity (aligned).** Sealing and opening happen in the runtime (`CreateRound`, the cold
    load), never in `Decide`, `Evolve` or `InitialState`. The codec's secret bytes do not change.
11. **ADR-001 and ADR-005, PostgreSQL as system of record (consequence).**
    - An unsettled round's recorded deterministic input, its seed, can now be recovered from PostgreSQL
      only with the keyring.
    - If every copy of a key is lost while rounds sealed under it are unsettled, those rounds cannot be
      replayed and their holds stay stuck (R-4).
    - Settled rounds stay replayable from PostgreSQL alone (the reveal row and `RoundSettled`).
    - ADR-005's open "backup/restore tests" must now cover the keyring too (LBT-14, operations).
12. **LBT-9 A-11 (aligned).** Formats v1 and v2 are unchanged and `configSecret` is still required.
    Exported files are byte-identical.
13. **ADR-007 (aligned).**
    - New package `internal/persistence/sealing`. The proposed layout is "a starting convention", and
      `internal/ledger` and `internal/canonjson` already sit outside it.
    - No new module.
    - `cmd/engine` is untouched: LBT-14 wires the keyring into it.
    - `pgtest` gains imports of `execution`, crash/v1 and `replay`. This creates no import cycle, because
      every package that imports `pgtest` is an external test package.
14. **AGENTS.md, "merged migrations are immutable" (aligned).** All changes go into a new migration,
    `0010`. Migrations `0001` to `0009` are untouched.

## 4. Decision summary

| ID | Topic | Proposed value | Approve |
|---|---|---|---|
| K-1 | Scope | Seal the codec secret config of every round created from `0010` on. Once a round settles, its secret is public and stored in plaintext in `round_secret_reveals`. `D` and `T0 + D` stay plaintext (R-1, follow-up). | [x] |
| K-2 | Key source | A local keyring file named by `ORVEX_SECRET_KEYRING_FILE`: 256-bit keys with IDs, one active, a strict format and permissions, read once at process start. | [x] |
| K-3 | Where sealing happens | In Go, in the engine process: new package `internal/persistence/sealing`, standard-library AES-256-GCM. PostgreSQL never sees a key. | [x] |
| K-4 | Rotation | Never re-encrypt. Three phases: add the key, activate it, retire the old key once its rounds are revealed. Documented procedures for compromise and loss. | [x] |
| K-5 | Row format | Four new columns on `round_secrets`: `secret_scheme`, `secret_key_id`, `secret_nonce`, `secret_ciphertext`. `config_secret` becomes nullable and is used only by legacy rows. The AAD binds scheme, key ID and round ID; the nonce is a random 96-bit value. Three CHECKs, one of them `NOT VALID`. | [x] |
| K-6 | Reveal table | New append-only `orvex.round_secret_reveals(round_id, sequence, config_secret)`, written in Transaction B of the record that first makes `Policy.Final` true. Raises `OX010` unless the named record is an accepted command record. | [x] |
| K-7 | Who gets plaintext | Admission and gateway: never (`D` stays inside PostgreSQL). Engine: seals at creation; at cold load uses the reveal, else the legacy plaintext, else opens the sealed form. Auditor: reveal rows only. Privileged export: the sealed form plus the keyring. | [x] |
| K-8 | Execution API | `execution.Config.Keyring` is required. `CreateRound` seals. The cold load prefers the reveal. The finalizing record writes the reveal. | [x] |
| K-9 | Export API | A new `eventstore.Options{Unrevealed, Keyring}` parameter on `ExportHistory` and `ExportHistoryV2`. New errors `ErrNotRevealed`, `ErrKeyringRequired`. | [x] |
| K-10 | Row-level restriction and `-unsettled` | The auditor gets `SELECT` on `round_secret_reveals` only. The privileged path is role `orvex_secret_reader` plus the keyring. `-unsettled` maps to it, with defined exit codes. | [x] |
| K-11 | Migration and compatibility | `0010_sealed_secrets`: new columns and constraints, the reveal table, a backfill of reveals for legacy settled rounds. Legacy plaintext rows stay readable. Formats unchanged. The down refuses while data exists. | [x] |
| K-12 | DSN rule | crashctl prints no driver, network or server text, except Orvex-authored server messages (class `OX`). Server errors become SQLSTATE and constraint placeholders. Connect failures get a fixed message. | [x] |
| K-13 | crashctl keyring | Read only with `-unsettled`. Errors are exit 2 and never print key material or file contents. | [x] |
| K-14 | LBT-14 grants | The role matrix, keyring distribution, the rotation runbook, and the tests LBT-14 owes. | [x] |
| K-15 | SQLSTATE | `OX010`, Go constant `CodeSecretReveal`. | [x] |

## 5. Decisions

### K-1. Scope of protection

**Proposal.**

- **Sealed at rest:** the codec secret config of every round created after `0010` is applied: the
  `secret` that `EncodeConfig` returns, for crash/v1 `{"seed":"<64 hex>"}`. It is stored only in
  sealed form (K-5).
- **Public after settlement (ADR-004):** the same bytes in `orvex.round_secret_reveals` (K-6). The
  `RoundSettled` event in `chain_records` and `outbox_messages` already carries them (A-12).
- **Not sealed (unchanged):** `round_secrets.close_offset_micros` (`D`) and
  `round_admission_heads.close_not_before_unix_micro` (`T0 + D`). Only grants protect them: only the
  owner of `orvex.admit_command` reads them (R-1).
- **Unchanged:** `round_snapshots.state` stays NULL (E-13). Any future writer must seal it (R-5).

**Rationale.** The seed is the asset this issue names. `admit_command` must read `D` inside PostgreSQL
when it admits the start and close commands (the approved LBT-12 addendum). Sealing `D` in Go would make
it unreadable there; sealing it in the database would put a key into PostgreSQL (see K-3). The reveal
table lets auditors read a settled round's secret without a key.

**Alternatives considered.**

- *Also seal `D` and the bound with a key that PostgreSQL holds* (pgcrypto, a GUC or a key table): the
  key would sit in the same backups it is meant to protect, so nothing is gained against A1.
- *A commit-and-open closure guard:*
  - store `SHA-256("orvex/close-bound/v1:" || D || r)`, with a 256-bit nonce `r` kept in the sealed
    secret;
  - the closing command supplies `D` and `r`, and `admit_command` checks them against the hash before
    comparing times.

  This removes S2 at rest. But it changes `admit_command`'s arguments, the admission API and the approved
  addendum ("the bound is stored, not supplied by the caller"). Proposed as a follow-up issue.
- *Seal without a reveal, and let auditors decrypt:* every auditor would then hold a key that opens every
  unsettled seed. See K-6.

### K-2. Key source and keyring file

**Proposal.**

- A process reads its keyring from the file named by the environment variable
  `ORVEX_SECRET_KEYRING_FILE` (constant `sealing.KeyringFileEnv`). The variable holds a path, never key
  material.
- File format: ASCII text with LF or CRLF line endings.

  ```text
  # comments and blank lines are ignored
  active k2026-10
  key k2026-01 <base64>
  key k2026-10 <base64>
  ```

  - Fields are separated by one or more spaces or tabs. A line whose first non-blank character is `#`
    is a comment.
  - The file holds exactly one `active <id>` line and between 1 and 64 `key <id> <base64>` lines.
  - Key IDs are unique and match `^[a-z0-9][a-z0-9._-]{0,63}$`. The active ID must be listed.
  - `<base64>` is padded standard base64 (RFC 4648 §4) and decodes to exactly 32 bytes.
  - Any other line, or any extra field, is an error.
  - Errors wrap `sealing.ErrKeyring` and give the line number and the rule broken, never the line's
    content or any key byte.
- `sealing.LoadKeyringFile` checks the file before parsing it:
  - it follows symlinks, because Kubernetes Secret volumes are symlinks;
  - the target must be a regular file of at most 64 KiB;
  - its permission bits must satisfy `perm & 0o027 == 0`: no group write, nothing for others. So
    `0400`, `0440`, `0600` and `0640` pass, and the Kubernetes default `0644` fails.
- The keyring is read once at process start; changing it needs a restart (K-4).
- Key generation, documented in `persistence.md` §16:
  `printf 'active k1\nkey k1 %s\n' "$(openssl rand -base64 32)" > keyring && chmod 0400 keyring`.
- Who holds the keyring:
  - always: the engine;
  - break-glass only: an operator running `crashctl audit export -unsettled` on an unrevealed round;
  - never: the gateway, PostgreSQL, auditors. CI holds only the fixed `pgtest` key.
- In memory, `sealing.Keyring` keeps only key IDs and `cipher.AEAD` values. `String`, `GoString` and
  `Format` print the active ID and the number of keys, never key bytes.

**Rationale.**

- Mounted files are the Kubernetes-native way to deliver secrets, and they carry restrictive modes.
- Several keys per file allow rotation without downtime (K-4).
- Storing the key ID in each row lets every reader pick the right key, and lets operators count rows per
  key.

**Alternatives considered.**

- *The key value in an environment variable* (`ORVEX_SECRET_KEY=<base64>`): rejected. Any process with
  the same uid can read it from `/proc/<pid>/environ`; child processes inherit it; `docker inspect` and
  pod specs show it unless it is secret-backed; crash reporters capture it; and it holds only one key.
- *KMS envelope encryption (a data key per round, wrapped by a KMS key):* deferred.
  - There is no KMS in the stack (Compose locally, Kustomize for Kubernetes).
  - It adds a network call, and a new failure mode, to round creation and to every cold load.
  - Its main benefit, cheap re-wrapping on rotation, does not apply: a row is sensitive only until its
    round settles (K-4).

  `secret_scheme` leaves room for a later `kms-envelope/v1`, and `sealing` can gain a second
  implementation behind the same columns.
- *A passphrase plus a key-derivation function:* rejected. The input has low entropy, and the KDF
  parameters would have to be frozen.

### K-3. Where encryption and decryption happen

**Proposal.** Seal and open in Go, in the process that executes the round (the engine), in
`internal/persistence/sealing`, with `crypto/aes` and `cipher.NewGCM` from the standard library.
PostgreSQL receives only ciphertext. No SQL statement carries a key or an unsettled plaintext secret.

**Rationale.**

- The key never enters database memory, server logs, `pg_stat_statements` or backups.
- GCM authenticates the ciphertext.
- Standard library only (ADR-007).

**Alternatives considered.**

- *pgcrypto (`pgp_sym_encrypt`, `encrypt_iv`):* rejected.
  - It needs `CREATE EXTENSION`; migrations create only `orvex` objects
    (`TestMigrationsCreateOnlyOrvexObjects`), and the extension needs a superuser or trusted-extension
    setup.
  - The key travels to the server as a bind parameter, where `log_min_error_statement`, `auto_explain`
    or parameter logging can record it.
  - Decryption happens in database memory, and any role with `EXECUTE` and the key can decrypt.
- *Storage-level encryption (TDE, encrypted volumes):* useful and recommended as an operations baseline,
  but it stops neither A2 nor a logical dump (A1). It complements this design; it does not replace it.
- *Sealing in the gateway:* rejected. The gateway never needs the seed, so this would only add another
  key holder.

### K-4. Key IDs, rotation, compromise and loss

**Proposal.**

- Each sealed row stores its key ID. `Seal` uses the active key; `Open` uses whichever key in the keyring
  matches the row's ID.
- **No re-encryption, ever.** `round_secrets` stays append-only (P-13). A secret needs protection only
  until its round settles; after that, its reveal row (K-6) serves every reader without a key.
- Rotation runbook (`persistence.md` §16):
  1. Add `key <new>` to every holder's keyring, keeping the same active key. Restart, rolling.
  2. Switch every keyring to `active <new>`. Restart. New rounds are sealed with `<new>`.
  3. Retire `<old>` once this query returns 0, or once the operator explicitly abandons every remaining
     such round:

     ```sql
     SELECT count(*) FROM orvex.round_secrets s
     WHERE s.secret_key_id = '<old>'
       AND NOT EXISTS (SELECT 1 FROM orvex.round_secret_reveals v WHERE v.round_id = s.round_id);
     ```

  Steps 1 and 2 must be separate restarts: during a rolling restart, an engine still on the old keyring
  may cold-load a round sealed with the new key.
- Suggested cadence: at least yearly, and after any suspected exposure. With random nonces, AES-GCM stays
  far below the bound of 2^32 messages per key (NIST SP 800-38D §8.3) at any plausible round rate.
- Compromise: rotate at once (steps 1 and 2 together), and treat every unrevealed round sealed with the
  compromised key as exposed. Deciding what happens to those rounds (void or refund) is an operations
  matter outside this issue; credits are simulated (ADR-004).
- Loss: a round whose key is lost before its reveal can never be cold-loaded (`ErrCorrupt` wrapping
  `sealing.ErrUnknownKey`) or exported with `-unsettled`. Settled rounds are not affected. Back up the
  keyring separately from database backups, never in the same artifact.

**Alternatives considered.**

- *Re-encrypt rows on rotation:* this needs an `UPDATE` on an append-only table (conflicts with P-13) or
  a copy table, and gains nothing given how short the sensitive window is.
- *A single key without IDs:* rotation would need downtime, and readers could not tell which key a row
  used.

### K-5. Sealed row format

**Proposal.** Migration `0010` changes `round_secrets` as follows:

| Column | Type | Meaning |
|---|---|---|
| `config_secret` | `bytea`, now NULL-able | plaintext, only in rows written before `0010` |
| `secret_scheme` | `text` NULL | `aes-256-gcm/v1` |
| `secret_key_id` | `text` NULL | the ID of the sealing key |
| `secret_nonce` | `bytea` NULL | 12 random bytes |
| `secret_ciphertext` | `bytea` NULL | `AES-256-GCM(key, nonce, plaintext, AAD)`: the ciphertext followed by the 16-byte tag |
| `close_offset_micros` | unchanged | |

- **Plaintext:** exactly the codec's secret config bytes, which keeps the format game-agnostic. For
  crash/v1, 75 bytes of plaintext give 91 bytes of ciphertext.
- **AAD:** the ASCII bytes `orvex/round-secret/v1`, a 0x00 byte, the scheme in ASCII, a 0x00 byte, the
  key ID in ASCII, a 0x00 byte, then the 16 raw bytes of the round ID. This binds each ciphertext to its
  round, key and scheme: a ciphertext copied into another round's row fails to open.
- **Nonce:** 12 bytes from `crypto/rand` for each `Seal`. If `crypto/rand` fails, `CreateRound` fails,
  as seed generation does (D-10).
- **Constraints:**
  - `round_secrets_sealed_shape_check`: either all four sealed columns are NULL, or all four are set,
    with `secret_scheme = 'aes-256-gcm/v1'`, `secret_key_id` matching the K-2 pattern,
    `octet_length(secret_nonce) = 12` and `octet_length(secret_ciphertext) >= 18`. 18 is the
    `config_secret` minimum of 2 bytes plus the 16-byte tag.
  - `round_secrets_form_check`: `(config_secret IS NULL) <> (secret_ciphertext IS NULL)`, so exactly one
    form is present.
  - `round_secrets_sealed_check`: `CHECK (config_secret IS NULL) NOT VALID`. PostgreSQL enforces a
    `NOT VALID` check on every new row and does not check existing rows. From `0010` on, no role can
    insert plaintext, and legacy rows stay valid. Never run `VALIDATE CONSTRAINT` on it while legacy rows
    exist (R-2).
  - The existing `round_secrets_config_secret_check` (at least 2 bytes) passes on NULL.
    `round_secrets_close_offset_check` is unchanged.
  - CHECKs fire in name order (`close_offset`, `config_secret`, `form`, `sealed`, `sealed_shape`). A
    plaintext-only insert therefore fails on `round_secrets_sealed_check`, and a row with both forms
    fails on `round_secrets_form_check`.
- Go: `sealing.Sealed{Scheme, KeyID string; Nonce, Ciphertext []byte}` maps one-to-one onto the four
  columns.

**Rationale.** Separate columns let PostgreSQL check the shape, and let operators query by key ID
(K-4) without parsing anything. The scheme column leaves room for K-2's KMS alternative. `NOT VALID`
makes "no new plaintext" a declarative invariant that holds for every role, without touching existing
rows.

**Alternatives considered.**

- *One opaque envelope column* (version byte, key ID, nonce, ciphertext): PostgreSQL could check nothing,
  and the rotation query would have to parse bytes.
- *A separate table, `round_sealed_secrets`:* two tables and a join for the same one-to-one data.
- *Reuse `config_secret` for the ciphertext:* legacy and sealed rows would look the same.
- *A `BEFORE INSERT` trigger with a custom SQLSTATE instead of `NOT VALID`:* more code for the same
  effect.

### K-6. Revealed secrets table

**Proposal.**

- New table `orvex.round_secret_reveals`:

  | Column | Type and constraint |
  |---|---|
  | `round_id` | `uuid NOT NULL`, primary key `round_secret_reveals_pkey` |
  | `sequence` | `bigint NOT NULL`, `>= 1` (`round_secret_reveals_sequence_check`) |
  | `config_secret` | `bytea NOT NULL`, `octet_length >= 2` (`round_secret_reveals_config_secret_check`) |

  - Foreign keys: `round_secret_reveals_secret_fkey (round_id) → round_secrets (round_id)` and
    `round_secret_reveals_record_fkey (round_id, sequence) → chain_records (round_id, sequence)`.
  - Append-only: `round_secret_reveals_append_only` and `round_secret_reveals_no_truncate`
    (`orvex.forbid_mutation`, `OX001`).
  - Trigger `round_secret_reveals_record`, `BEFORE INSERT`, calls `orvex.check_secret_reveal()`
    (invoker rights, `SET search_path = pg_catalog`). If the named chain record exists and its
    `result_status` is not `accepted`, it raises `OX010`. A missing record is left to the foreign key,
    as the binding trigger does.
- **Writer:** the engine, in Transaction B of record `k`, when `Policy.Final` is true for the state after
  `k` and was false for the state before. That is exactly once per round, on the record that finalizes
  it: for crash/v1 the accepted `SettleRound`, whose record emits `RoundSettled` and gets a snapshot. The
  reveal insert is the last statement of the batch, so it commits or rolls back together with the
  record. Its value is the plaintext secret the executor holds (from `CreateRound` or the cold load).
- **Readers:** the auditor (the default export) and the engine (the cold load's first choice, K-7).
- PostgreSQL checks that a reveal names an accepted command record of its own round. It does not check
  the settlement rules: game rules live in Go, as they do for chain records. The verifier catches a wrong
  reveal: it checks the commitment, and `RoundSettled.seed` must equal the seed (A-12).

**Rationale.** Once the secret is sealed in Go, a row-level filter on `round_secrets` would have to give
the auditor the key, which puts every unsettled seed one query away, or put the key into the database.
Instead, the engine writes the revealed secret as its own row at the moment ADR-004 makes it public.
By construction, the auditor sees exactly the settled rounds, atomically with settlement, with no key
and no RLS.

**Alternatives considered.**

- *A view over `round_secrets` filtered by settlement:* its rows are ciphertext, so the auditor would
  still need the key.
- *An RLS policy on `round_secrets` with a settlement predicate:* the same key problem. The table owner
  bypasses RLS unless it is forced, and the predicate would encode game rules in SQL.
- *A `SECURITY DEFINER` function that returns plaintext for settled rounds:* it needs the plaintext or
  the key inside the database.
- *Derive `configSecret` from the `RoundSettled` event at export time:* this puts game-specific decoding
  into the game-agnostic exporter, or needs a new codec method, which changes the frozen game contract.
- *Bind the reveal to the admission window* (`sequence = sealed_sequence`), or require a reveal at the
  sealed record through a deferred trigger: game-agnostic and stronger. But every test fixture that
  inserts admissions directly has an `open` window, and `worked-example` admits a `PlaceBet` after
  `SettleRound`. The test harness would break for little gain over the verifier's check. Rejected.

### K-7. How each component obtains the plaintext it legitimately needs

| Component | Needs | Source | Holds the key |
|---|---|---|---|
| `orvex.admit_command` (admitting role) | `D` only, inside PostgreSQL | `round_secrets.close_offset_micros`, read with the owner's rights (`SECURITY DEFINER`) | no |
| gateway, admitting role | nothing | — | no |
| engine: `CreateRound` | the secret config | the caller's `Config` (seed from `crypto/rand`, D-10), then `Seal` with the active key | yes |
| engine: cold load | the secret config | the reveal row if present, else legacy `config_secret`, else `Open` of the sealed form | yes |
| engine: the finalizing record | the secret config | the entry cache, written to the reveal row | yes |
| `crashctl audit export` (default) | a revealed round's secret | `round_secret_reveals` | no |
| `crashctl audit export -unsettled` | any round's secret | the reveal if present, else `round_secrets` (privileged role) plus the keyring | only for unrevealed sealed rounds |
| `crashctl audit verify`, `replay`, `chain` | the file's secret | the file header | no |
| `pgtest` | — | the fixed test keyring | test key only |

### K-8. Execution changes

- `execution.Config` gains `Keyring *sealing.Keyring`. `New` returns `ErrInvalidConfig` when it is nil.
  `DefaultConfig()` leaves it nil, and callers set it (there is still no zero-means-default).
- `CreateRound`:
  - after validation, before any database call, it calls `Seal(roundID, secret)`;
  - a seal failure is `ErrInternal` ("seal secret config");
  - it inserts the four sealed columns and `close_offset_micros`, with `config_secret` NULL.

  Idempotency is unchanged and still compares genesis bytes. Each attempt draws a new nonce, which is
  harmless: the `rounds_pkey` conflict fires first.
- Cold load: the existing round query also reads `v.config_secret` (the reveal), `s.config_secret` (the
  legacy plaintext) and the four sealed columns. The first one present wins, in that order. Errors:

  | Condition | Error |
  |---|---|
  | no form at all | `ErrCorrupt`, with the existing message |
  | unknown scheme | `ErrCorrupt` wrapping `sealing.ErrScheme` |
  | key ID not in the keyring | `ErrCorrupt` wrapping `sealing.ErrUnknownKey` |
  | authentication failure | `ErrCorrupt` wrapping `sealing.ErrOpen` |

  Each of these halts the round like any other `ErrCorrupt` (E-14). No error text contains the
  plaintext, a key, the nonce or the ciphertext.
- The round's cache entry keeps the plaintext secret bytes next to the state.
- `computeTransition` sets `tr.reveal = tr.final && !finalBefore`, where `finalBefore =
  Policy.Final(st.state)`. An error from either call is a `*FailClosedError`, as for `final` today.
- When `tr.reveal` is set, `commit` queues the reveal insert as the last statement of the batch. `OX010`
  maps to `ErrInternal` under the existing rule for class `OX`.
- An adopted outcome needs nothing extra: the reveal committed together with the record.

### K-9. Export API and secret sources

- New parameter type: `eventstore.Options{Unrevealed bool; Keyring *sealing.Keyring}`.
  `ExportHistory(ctx, db, roundID, w, opts)` and `ExportHistoryV2(ctx, db, roundID, w, opts)` take it as
  a new last parameter. The zero `Options` reads revealed secrets only.
- The secret is read inside the existing snapshot transaction:
  1. `SELECT v.config_secret FROM orvex.rounds r LEFT JOIN orvex.round_secret_reveals v USING
     (round_id) WHERE r.round_id = $1`. No row: `ErrRoundNotFound`. A value: use it. This statement
     never references `round_secrets`, so the auditor's grants are enough.
  2. NULL with `Unrevealed` false: `ErrNotRevealed` (new); nothing is written.
  3. NULL with `Unrevealed` true: `SELECT config_secret, secret_scheme, secret_key_id, secret_nonce,
     secret_ciphertext FROM orvex.round_secrets WHERE round_id = $1`.
     - No row: `ErrCorrupt` ("round has no secret config").
     - Legacy plaintext: use it.
     - Sealed, with `Keyring` nil: `ErrKeyringRequired` (new).
     - Sealed, with a keyring: `Open`. Its errors are as in K-8, wrapped in `ErrCorrupt` with the
       `sealing` error kept in the chain.
- Database errors in statements 1 and 3 are wrapped as `eventstore: read revealed secret: %w` and
  `eventstore: read sealed secret: %w`.
- Everything else is unchanged: snapshots, `S`, the head, the verbatim records, the v2 ledger lines and
  the bytes written.

### K-10. Row-level restriction and `-unsettled`

**Proposal.**

- **Restriction:** the auditor role gets `SELECT` on `round_secret_reveals` and no privilege on
  `round_secrets`, not even on a single column. Reveal rows exist only for finalized rounds (K-6), so an
  unsettled round has nothing the auditor can read.
- **Privileged path:** both of these are needed to read an unrevealed round's sealed secret:
  - a separate `NOLOGIN` role, `orvex_secret_reader`, with `SELECT (round_id, config_secret,
    secret_scheme, secret_key_id, secret_nonce, secret_ciphertext) ON orvex.round_secrets` (never
    `close_offset_micros`). It is granted, together with `orvex_auditor`, to named break-glass login
    roles;
  - the keyring file.

  The role alone yields only ciphertext.
- How each export invocation maps onto the restriction:

  | Invocation | Round | Reads | Result |
  |---|---|---|---|
  | default | revealed, settled at `S` | the reveal | exit 0, as today |
  | default | revealed, not settled at `S` | the reveal | exit 6, X-7 message unchanged |
  | default | not revealed | the reveal (absent) | exit 6, new message, before any replay |
  | `-unsettled` | revealed | the reveal | exit 0, report as today, no key needed |
  | `-unsettled` | not revealed, legacy plaintext | the reveal, then `round_secrets` | exit 0 with the privileged role; exit 4 (42501) without it |
  | `-unsettled` | not revealed, sealed | the reveal, `round_secrets`, the keyring | exit 0 with role and keyring; exit 4 (42501) without the role; exit 2 without a keyring (`ErrKeyringRequired`) or when the key ID is missing (`ErrUnknownKey`); exit 1 on an authentication failure |

- The new exit 6 message: `round <id> has no revealed secret config: it is not settled; -unsettled
  exports it through the privileged path`.
- `-unsettled` help text: "write the history even if the round is not settled; for a round whose secret
  is not revealed, this reads the sealed secret (needs SELECT on orvex.round_secrets and
  ORVEX_SECRET_KEYRING_FILE); the file then discloses its secret config".
- The report is unchanged: `settled no` or `settled unknown`, followed by the warning.

**Alternatives considered.**

- *A `SECURITY DEFINER` function, `orvex.read_round_secret(uuid)`, executable by the privileged role:*
  narrower (one round per call, no bulk `SELECT`). But it adds a definer function that returns only
  ciphertext, which is useless without the key anyway. LBT-14 can add it if per-row auditing is wanted.
- *RLS on the reveal table:* not needed; its rows are public by definition.

### K-11. Migration `0010_sealed_secrets` and backward compatibility

**Proposal.**

- `migrations/0010_sealed_secrets.up.sql` starts with the line
  `-- 0010 sealed_secrets: sealed round secrets, revealed secrets, legacy reveal backfill (LBT-21 spec K-5, K-6, K-11)`
  and does the following, in order:
  1. On `round_secrets`: `config_secret DROP NOT NULL`; add the four columns; add the shape and form
     checks, which are validated against existing rows (legacy rows pass); add the sealed check as
     `NOT VALID`; update the table comment and add column comments.
  2. Create `round_secret_reveals` with its constraints, triggers and comments, and the function
     `orvex.check_secret_reveal()`.
  3. Backfill reveals for legacy settled rounds, in one `INSERT … SELECT`:
     - from `round_secrets` rows with `config_secret IS NOT NULL`;
     - joined to `round_admission_heads` with `admission_state = 'sealed'`;
     - joined to the chain record at `sealed_sequence` with `result_status = 'accepted'`;
     - inserting `(round_id, sealed_sequence, config_secret)`.

     The predicate is game-agnostic: the rules policy assigns the window classes at admission (S-8). For
     crash/v1, an accepted `SettleRound` is exactly what finalizes a round. Legacy rounds without a
     maintained window (seeded directly, or created before `0007`) get no backfill; they export through
     `-unsettled` with the privileged role, from plaintext, without a key.
- `migrations/0010_sealed_secrets.down.sql` starts with
  `-- 0010 sealed_secrets: drop sealed and revealed secrets, refusing when round_secret_reveals or sealed round_secrets rows exist (LBT-21 spec K-11)`.
  - It refuses with `OX003` while `round_secret_reveals` holds rows or any `round_secrets` row has
    `secret_ciphertext`.
  - Otherwise it drops the table, its function, the three constraints and the four columns, sets
    `config_secret` back to `NOT NULL`, and restores the table comment from `0002`.
- Legacy rows written before `0010` keep their plaintext; no `UPDATE` touches them (P-13). The engine
  and the privileged path still read them, and the `NOT VALID` check stops new ones. A deployment that
  must remove legacy plaintext has to let those rounds settle and then rebuild the database from
  exports; this issue provides no tooling for that (R-2). No deployment exists today: `cmd/engine` is a
  scaffold.
- History formats v1 and v2 are byte-for-byte unchanged. Golden vectors and golden histories do not
  change, and `make golden-check` is unaffected.
- Code and schema ship together, `0010` first. A new engine refuses to start without a keyring. An old
  engine running against a `0010` schema fails `CreateRound` with 23514 (`round_secrets_sealed_check`),
  so a mismatch fails closed.

**Alternatives considered.**

- *Refuse to apply `0010` while plaintext rows exist:* this forces a fresh database even for settled
  history, against P-26 retention.
- *Rewrite legacy rows with `session_replication_role = replica`:* violates P-13 and the immutability
  ADR-001 relies on.
- *A crash-specific backfill keyed on `domain_events.event_type = 'RoundSettled'`:* puts game-specific
  SQL into a migration.

### K-12. The database URL rule

**Proposal.**

1. **Scope:** every byte crashctl writes, to stdout, stderr or `FILE`, in every subcommand, once it has
   read `ORVEX_DATABASE_URL`.
2. **Guarantee:** the output never contains the DSN, or any value pgx derived from it or from the `PG*`
   environment variables, the passfile or the service file. That covers the user, password, hosts,
   ports, database, runtime parameters, TLS file paths and resolved addresses. The guarantee holds
   because crashctl never prints text produced outside Orvex: pgx and pgconn, `net`, `crypto/tls`,
   `x509`, and the server's message, detail, hint and context. The one exception is the server message
   of a class-`OX` error, which Orvex migrations author.
3. **Rendering:**
   - a. `pgx.ParseConfig` fails: `invalid ORVEX_DATABASE_URL`, as today. Exit 2.
   - b. `pgx.ConnectConfig` fails: `cannot connect to the database named by ORVEX_DATABASE_URL`,
     followed by `: SQLSTATE <code>` when the server answered with an error, or `: timeout` when
     `pgconn.Timeout(err)` holds or a `net.Error` timed out. Nothing else is added. Exit 4.
   - c. Any other error is rendered by walking its wrap tree (`Unwrap() error` and `Unwrap() []error`):
     - A node whose concrete type is defined in the Orvex module or in `fmt`, `errors`, `io/fs` or
       `context` keeps its own text. Each child's text inside it is replaced by that child's rendering.
       If a child's text does not appear verbatim inside its parent's text, the parent renders as its
       children's renderings joined by `: `.
     - A `*pgconn.PgError` renders as `SQLSTATE <code>`, plus ` constraint <name>` when the server
       names a constraint. For class `OX` it renders as `<message> (SQLSTATE <code>)`.
     - Any other node renders as `database driver error`, or `database driver error: timeout` for a
       network timeout, and its subtree is dropped.
   - d. Defense in depth: a final pass replaces any remaining occurrence of the raw DSN, and of the
     password if it is not empty, with `xxxxx`. This is today's `mask`.
4. One line per failure and every exit code stay as they are.
5. The Orvex code that crashctl reaches (`eventstore`, `postgres` migrations, `sealing`, `verification`)
   wraps foreign errors with `%w` only, so step c finds them. The implementation checks this (section
   9, step 6).

Expected messages:

| Situation | Today | After |
|---|---|---|
| connection refused | ``…: failed to connect to `user=u database=db`: 127.0.0.1:1 (127.0.0.1): dial error: …`` | `…: cannot connect to the database named by ORVEX_DATABASE_URL` |
| wrong password | `… password authentication failed for user "u" (SQLSTATE 28P01)` | `…: cannot connect to the database named by ORVEX_DATABASE_URL: SQLSTATE 28P01` |
| auditor without the grant | `eventstore: read secret: ERROR: permission denied for table round_secrets (SQLSTATE 42501)` | `eventstore: read revealed secret: SQLSTATE 42501` |
| `migrate up` without `CREATE` | `postgres: migration 0001 foundation up: ERROR: permission denied for database db (SQLSTATE 42501)` | `postgres: migration 0001 foundation up: SQLSTATE 42501` |
| `down` with data | `…: ERROR: down migration refused: orvex.round_secret_reveals holds rows (SQLSTATE OX003)` | `…: down migration refused: orvex.round_secret_reveals holds rows (SQLSTATE OX003)` |
| connection lost mid-export | `eventstore: read records: … read tcp 127.0.0.1:5->127.0.0.1:55432: … connection reset by peer` | `eventstore: read records: database driver error` |

`migrate status` keeps printing `applied by <build version>`. That value is the build string stored in
`orvex_migrate.schema_migrations`, not the database user.

**Alternatives considered.**

- *A. Redact DSN values from the messages:*
  - it misses resolved IP addresses;
  - it misses server-side echoes in other forms (identifier quoting, case folding);
  - it misses a password taken from `~/.pgpass`, which is not in the DSN;
  - it garbles every message when the database shares a name with the schema (`orvex`).
- *B. Drop all error text and print only the exit code and SQLSTATE:* this loses Orvex's own context,
  such as which read failed and which migration.
- *C. Print the server message with redaction applied:* has A's problems.

### K-13. crashctl keyring handling

- `crashctl audit export` reads `ORVEX_SECRET_KEYRING_FILE` only when `-unsettled` is given. If the
  variable is set, it loads the keyring after the flag and argument checks and before connecting. A
  failure is exit 2 with one line: `<prefix>ORVEX_SECRET_KEYRING_FILE: <reason>`. The reason comes from
  the `sealing` error: "unreadable", "not a regular file", "permissions too open (0644)", "larger than
  64 KiB", "line N: <rule>". It never includes the file's contents, key material or the path.
- With `-unsettled` and the variable unset, there is no keyring. A sealed, unrevealed round then fails
  with exit 2: `round <id> has a sealed secret config and no revealed one; set
  ORVEX_SECRET_KEYRING_FILE to export it`.
- `crashctl migrate` and `crashctl audit verify|replay|chain` never read the variable.

### K-14. How LBT-14's roles and grants build on this

Migrations still create no roles (P-13). LBT-14 receives this matrix, recorded in `persistence.md`
§8 and §16:

| Role | `round_secrets` | `round_secret_reveals` | Keyring |
|---|---|---|---|
| owner of `orvex.admit_command` | owner (reads `close_offset_micros`) | — | no |
| engine | `SELECT (round_id, config_secret, secret_scheme, secret_key_id, secret_nonce, secret_ciphertext)` and `INSERT` | `SELECT`, `INSERT` | yes |
| gateway (admitting) | nothing | nothing | no |
| `orvex_auditor` | nothing | `SELECT` | no |
| `orvex_secret_reader` (`NOLOGIN`, break-glass) | `SELECT` on the same six columns as the engine | (via `orvex_auditor`) | operator's copy, only while in use |

- The engine keeps every other §13 grant, and needs `SELECT` on `chain_records`, which
  `orvex.check_secret_reveal()` reads with invoker rights.
- Keyring distribution: one Kubernetes Secret, `orvex-secret-keyring`, mounted read-only into engine pods
  only, with `defaultMode: 0400` (or `0440` with `fsGroup`). It is never mounted into gateway pods.
  Engine pods disable core dumps (R-7).
- Rotation is K-4's runbook, run as two rolling restarts.
- LBT-14 owes an integration test that creates the roles above and proves the matrix end to end,
  including that the engine role can execute and reveal. LBT-21's tests cover the auditor, the
  privileged path and the admitting role.

### K-15. SQLSTATE `OX010`

`OX010`, with Go constant `postgres.CodeSecretReveal`: "a reveal row names a chain record that is not an
accepted command record of its round". It is added to the `persistence.md` §3 catalogue. Execution maps
it to `ErrInternal` (class `OX`).

## 6. Residual risks (state them in the PR)

- **R-1:** `D` and `T0 + D` stay plaintext. A database copy, or the `admit_command` owner role, can learn
  the crash tick of a round that has not crashed. Follow-up: the commit-and-open closure guard (K-1).
- **R-2:** rows written before `0010` keep their plaintext seed.
- **R-3:** whoever compromises an engine, or holds a keyring together with a database copy, reads every
  unsettled seed. This is inherent to the design.
- **R-4:** losing a key makes the unsettled rounds sealed under it unrecoverable. Back the keyring up
  separately (K-4).
- **R-5:** `round_snapshots.state` would contain the seed if it were ever written (E-13 keeps it NULL).
  A follow-up should forbid it (`CHECK (state IS NULL)`) or seal it.
- **R-6:** when the reveal is written depends on the engine's `Policy.Final`. PostgreSQL checks only that
  the reveal names an accepted record. A wrong policy could reveal early to auditors.
  `TestComputeTransitionReveal` covers it.
- **R-7:** the plaintext secret and the keys live in engine memory, and Go cannot reliably zero them.
  Disable core dumps on engine pods.
- **R-8:** a file written with `-unsettled` holds the seed. LBT-19's warning is unchanged.

## 7. Engineer approval checklist

- [x] K-1 to K-15 as written, or with edits noted inline.
- [x] Section 3 items 1 to 8, especially item 8 (`D` stays plaintext) and item 5 (exit 6 for unrevealed
  rounds).
- [ ] The residual risks R-1 to R-8, and the follow-ups in section 12.

## 8. Glossary

- **sealed:** encrypted per K-5.
- **revealed:** written to `round_secret_reveals` per K-6.
- **legacy row:** a `round_secrets` row written before `0010`, in plaintext.
- **privileged path:** the `orvex_secret_reader` grants plus the keyring (K-10).

## 9. Implementation plan (after approval)

Work in this order. Run builds and tests once at the end (section 11).

### Step 1. `internal/persistence/sealing` (new package)

- `sealing.go`:
  - constants `Scheme = "aes-256-gcm/v1"`, `KeyringFileEnv = "ORVEX_SECRET_KEYRING_FILE"`,
    `KeySize = 32`, `NonceSize = 12`, `TagSize = 16`, `MaxKeys = 64`, `MaxKeyringFileBytes = 64 << 10`;
  - the key ID pattern;
  - `type Sealed struct{ Scheme, KeyID string; Nonce, Ciphertext []byte }`;
  - an opaque `type Keyring` with `NewKeyring(active string, keys map[string][]byte)`,
    `ParseKeyring([]byte)`, `LoadKeyringFile(path)`, `(*Keyring).ActiveKeyID()`,
    `Seal(roundID [16]byte, plaintext []byte) (Sealed, error)`,
    `Open(roundID [16]byte, s Sealed) ([]byte, error)`, and `String`, `GoString`, `Format` that never
    print keys;
  - errors `ErrKeyring`, `ErrUnknownKey`, `ErrScheme`, `ErrOpen`;
  - the unexported `aad(scheme, keyID, roundID)` per K-5.

  The package doc cites LBT-21 K-2 to K-5. No error text contains key bytes, plaintext or base64.
- `sealing_test.go`:
  - `TestSealOpenRoundTrip`: the scheme, the active key ID, a 12-byte nonce, and
    `len(Ciphertext) == len(plaintext)+16`.
  - `TestSealRandomNonce`: two seals of the same input differ.
  - `TestSealedFormatMatchesSpec`: builds the AAD byte by byte from K-5, independently of the package,
    and opens a `Seal` output with `cipher.NewGCM` directly.
  - `TestOpenRejectsTampering`: a flipped ciphertext bit, a flipped nonce bit and another round ID fail
    with `ErrOpen`; another scheme fails with `ErrScheme`; an ID missing from the keyring fails with
    `ErrUnknownKey`; another key under the same ID fails with `ErrOpen`.
  - `TestOpenWithInactiveKey`: with `active k2`, rows sealed under `k1` still open.
  - `TestParseKeyring`: a table of cases. Valid, with comments, blank lines and CRLF. Errors for: no
    `active`; two `active` lines; active ID not listed; a duplicate ID; a bad ID; bad base64; 31-byte
    and 33-byte keys; an unknown directive; extra fields; 65 keys. Each error wraps `ErrKeyring`,
    names the line, and contains neither the key's base64 nor its bytes.
  - `TestLoadKeyringFile`: `0400`, `0600`, `0640` and a symlink to a `0400` file are accepted; `0604`,
    `0660`, `0644`, a directory, a missing file and a file over 64 KiB are rejected.
  - `TestKeyringFormatting`: `%v`, `%+v`, `%#v`, `%s` and `String()` contain the active ID and no key
    material.

### Step 2. Migration `0010` and the Go constant

- `migrations/0010_sealed_secrets.up.sql` and `.down.sql`, per K-5, K-6 and K-11 (LF line endings,
  enforced by `.gitattributes`).
- `internal/persistence/postgres/errors.go`: `CodeSecretReveal = "OX010"`. Update `errors_test.go` if it
  lists the codes.
- `internal/persistence/postgres/doc.go`: update the `round_secrets` bullet and add
  `round_secret_reveals`.
- `internal/persistence/postgres/schema_integration_test.go`:
  - `insertRoundRows` (around line 342) inserts the sealed form, using `pgtest.Seal`.
  - `TestRoundSecretsAppendOnly`: `UPDATE` of `secret_ciphertext`, `DELETE` and `TRUNCATE` fail with
    `OX001`.
  - New `TestRoundSecretsSealedForm`:
    - plaintext only: 23514 `round_secrets_sealed_check`;
    - both forms: 23514 `round_secrets_form_check`;
    - no form: 23514 `round_secrets_form_check`;
    - a bad scheme, a bad key ID, an 11-byte nonce, a 17-byte ciphertext, or `secret_ciphertext` set
      with another sealed column NULL: 23514 `round_secrets_sealed_shape_check`;
    - a valid sealed row is accepted.
  - New `TestRoundSecretReveals`:
    - sequence 0: 23514 `round_secret_reveals_sequence_check`;
    - a rejected command record: `OX010`;
    - an unknown record: 23503 `round_secret_reveals_record_fkey`;
    - no `round_secrets` row: 23503 `round_secret_reveals_secret_fkey`;
    - a second reveal: 23505 `round_secret_reveals_pkey`;
    - `UPDATE`, `DELETE`, `TRUNCATE`: `OX001`;
    - an accepted record: accepted.
  - `TestTruncateRejectedEverywhere`: 14 tables become 15.
- `internal/persistence/postgres/close_bound_integration_test.go:36`: insert the sealed form.
- `internal/persistence/postgres/migrate_integration_test.go`:
  - New `TestMigrate0010LegacySecrets`:
    - migrate to 9;
    - `pgtest.LoadLegacyHistory` `empty-round`, then move its admission head to `sealed` (started 1,
      closed 2, sealed 3);
    - `LoadLegacyHistory` `in-progress`;
    - migrate up;
    - assert: legacy rows unchanged; exactly one reveal (`empty-round`, sequence 3, the plaintext
      bytes); none for `in-progress`; a plaintext insert now fails with `round_secrets_sealed_check`;
    - `down -to 9` refuses with `OX003` and changes nothing.
  - `TestMigrateDownRefusesAuditData`: `0010` refuses first.
  - Every test that counts migrations now expects 10.

### Step 3. `pgtest`

- New `pgtest/keyring.go`:
  - `Keyring() *sealing.Keyring`: key ID `pgtest`, key `SHA-256("orvex/pgtest/keyring/v1")`;
  - `KeyringFile(t) string`: a temporary file, mode `0600`, in the K-2 format;
  - `Seal(t, roundID, secret) sealing.Sealed`.
- `pgtest/load.go`:
  - `LoadHistory` writes the sealed form, sealed with `Keyring()`.
  - It writes a reveal at the first record `r >= 1` whose replayed state is final, using `replay.Run`
    with the crash/v1 adapter and `execution.CrashV1Policy().Final`. A history with no policy for its
    descriptor, or one that fails to replay, gets no reveal.
  - `Loaded` gains `Secret []byte`, `Revealed bool` and `RevealSequence uint64`.
  - New `LoadLegacyHistory(t, db, history, checkpoints) Loaded`: the plaintext `config_secret` and no
    reveal, for databases at schema 9.
- New `pgtest/load_integration_test.go` (tag `integration`), `TestLoadHistorySealsAndReveals`:
  - `worked-example` is sealed and revealed at 21;
  - `in-progress` is sealed and has no reveal;
  - `tamper/rules-version-2` has no reveal.

### Step 4. `internal/runtime/execution`

- `executor.go`: `Config.Keyring` and its validation; the entry keeps the plaintext secret.
- `round.go`: seal before the batch; insert the sealed columns.
- `load.go`: the K-8 precedence and errors.
- `transition.go`: `tr.reveal`.
- `commit.go`: `insertRevealSQL`, queued last.
- `doc.go`: mention sealing and the reveal.
- Tests:
  - `executor_test.go`: new `TestNewRequiresKeyring`. Existing configs set `Keyring`.
  - `transition_test.go`: new `TestComputeTransitionReveal`. Over the `worked-example` admissions,
    `reveal` is true only at 21; false at 22 (final before and after), at every rejected record and at
    every earlier record.
  - `helpers_integration_test.go`: `cfg.Keyring = pgtest.Keyring()`.
  - `round_integration_test.go`, `TestCreateRound` and `requireCreatedRows`: `config_secret` is NULL;
    the sealed columns have the K-5 shape; `Open` returns the secret; the key ID is `pgtest`; there is
    no reveal row.
  - New `secrets_integration_test.go`:
    - `TestSeedNotStoredInPlaintext`:
      - create a crash/v1 round with a fresh random seed, then admit and drain through `StartRound`,
        bets and a cash-out, without settling;
      - as superuser, with `SET bytea_output = 'hex'`, for every table in `pg_tables WHERE schemaname IN
        ('orvex', 'orvex_migrate')`, count rows whose `t::text` contains the seed's lowercase hex:
        expect 0 everywhere;
      - then settle and drain: the seed now appears only in `round_secret_reveals`, `chain_records` and
        `outbox_messages`.
    - `TestRevealWrittenAtSettlement`: drain `worked-example`. There is no reveal before record 21; at 21
      there is exactly one, whose bytes equal the secret; record 22 adds nothing.
    - `TestColdLoadOpensSealedSecret`: `Forget`, then `ExecuteNext` succeeds through a cold load.
    - `TestColdLoadPrefersReveal`: after settlement, a new executor whose keyring lacks `pgtest` still
      cold-loads the round.
    - `TestColdLoadUnknownKey`: the same executor on an unsettled round fails with `ErrCorrupt` and
      `sealing.ErrUnknownKey`; the error text does not contain the seed hex.
    - `TestColdLoadTamperedSecret`: a replica-mode update flips a ciphertext byte; the cold load fails
      with `ErrCorrupt` and `sealing.ErrOpen`.
    - `TestLegacySecretRoundCompletes`: at schema 9, `pgtest.LoadLegacyHistory` `in-progress`; migrate
      up; admit the remaining commands; drain. The round settles and its reveal equals the legacy
      plaintext.
  - `failure_integration_test.go`: new `TestRevealAtomicWithSettleRecord`. Force a definite failure of
    the settling batch, as the existing tests do with a conflicting `ledger_transactions` row. Neither
    record 21 nor a reveal is committed. In the existing ambiguous-commit adoption test, the reveal then
    exists exactly once.

### Step 5. `internal/persistence/eventstore`

- `export.go` and `export_v2.go`: `Options`, a shared `readSecret(ctx, tx, roundID, opts)` per K-9, and
  the new sentinels `ErrNotRevealed` and `ErrKeyringRequired`. Update the package doc.
- `export_integration_test.go`:
  - Every existing call passes `eventstore.Options{}`.
  - `TestExportGoldenHistories` and `TestExportV2GoldenHistories`: the settled histories export
    byte-equal through the reveal. `in-progress` returns `ErrNotRevealed` with nothing written, then
    exports byte-equal with `Options{Unrevealed: true, Keyring: pgtest.Keyring()}`.
  - New `TestExportSecretSources`:
    - the reveal wins even when `Unrevealed` is set and the keyring is nil;
    - `Unrevealed` with a nil keyring on a sealed round: `ErrKeyringRequired`;
    - a wrong key under the same ID: `ErrCorrupt` and `sealing.ErrOpen`;
    - a keyring missing the ID: `sealing.ErrUnknownKey`;
    - a legacy plaintext row at schema 9 then migrated up: exported with `Unrevealed` and no keyring;
    - no `round_secrets` row: `ErrCorrupt`.

### Step 6. `cmd/crashctl`

- `database.go`: replace `mask` with the K-12 renderer (`describe(cfg, err)`, and `connectFailure(err)`
  for step b); keep `oneLine`.
- `export.go`:
  - the `exporter` type takes `eventstore.Options`;
  - with `-unsettled`, load the keyring per K-13;
  - exit mappings: `ErrNotRevealed` is 6; `ErrKeyringRequired` and `sealing.ErrUnknownKey` are 2;
    `sealing.ErrOpen` inside `ErrCorrupt` is 1;
  - `fail` takes errors, not strings;
  - the new messages and help text from K-10 and K-13.
- `migrate.go`: `fail` uses `describe`; a connect failure uses the step b message.
- `main.go`: update the usage text for `-unsettled`.
- Before handing off, check that `eventstore`, `postgres` and `sealing` wrap every foreign error with
  `%w`:
  `grep -rnE 'Errorf\([^)]*%[vs][^)]*\berr\b' internal/persistence internal/runtime cmd/crashctl` must
  show no foreign error wrapped with `%v` or `%s`.
- Unit tests:
  - New `database_test.go`, `TestDescribe`, a table:
    - a 42501 `PgError`;
    - a 23514 `PgError` with a constraint;
    - an `OX003` `PgError`;
    - `fmt` `%w` chains, including `%w: %w`;
    - a `*net.OpError`;
    - a timeout;
    - a `*pgconn.ConnectError`;
    - a parent whose text does not contain the child's;
    - output never contains sentinel host, user, database or password values placed inside the
      foreign errors.
  - `TestConnectFailureMessage`.
  - `export_test.go`:
    - `TestExportNeverPrintsDatabaseURL`, replacing `TestExportNeverPrintsPassword`. Sentinel DSNs in
      URL and key=value form: user `sentinel_user_q1`, password `sentinel_pw_q1`, database
      `sentinel_db_q1`, `application_name=sentinel_app_q1`, a host `sentinel-host-q1.invalid` (DNS
      failure), `127.0.0.1` port 1 (refused), `sslmode=verify-full sslrootcert=/sentinel_q1/root.crt`
      (missing file), and an unparsable variant. Run each across `audit export FILE`, `audit export -`
      and `audit export -unsettled`. stdout and stderr contain no sentinel, no raw DSN and no `:1`
      host:port. Exit codes stay 2 or 4.
    - `TestExportKeyringEnv`: with `-unsettled`, a missing file, a `0644` file and a malformed file are
      exit 2 with one line and no key material. Without `-unsettled`, an invalid
      `ORVEX_SECRET_KEYRING_FILE` is ignored.
    - `TestExportExitCodes`: the new mappings.
    - `TestExportUsage`: the help text.
  - `migrate_test.go`: `TestMigrateNeverPrintsDatabaseURL`, replacing `TestMigrateNeverPrintsPassword`,
    with the same sentinels across `up`, `down -to 0` and `status`.
- Integration tests:
  - New `database_integration_test.go`, `TestNeverPrintsDatabaseURLAgainstServer`. Uses a login role
    with a sentinel name and password, and the pgtest database, whose unique name is a sentinel:
    - wrong password: exit 4, `SQLSTATE 28P01`;
    - unknown database: exit 4, `SQLSTATE 3D000`;
    - a role without `CONNECT`: exit 4, `SQLSTATE 42501`;
    - `migrate up` as a role without `CREATE` on the database (the server text names it): exit 1,
      `SQLSTATE 42501`;
    - the auditor without the reveal grant: exit 4;
    - `-unsettled` as the auditor on an unrevealed round: exit 4.

    In every case, no output contains the role, password, database name, `host:port` or the raw DSN.
  - `export_helpers_integration_test.go`:
    - `seedGolden` sets `Keyring`;
    - `exportGrants` becomes the K-14 auditor set (`round_secret_reveals`, nothing on
      `round_secrets`);
    - new `secretReaderGrants`.
  - `export_integration_test.go`:
    - `TestExportMinimalGrants`: the settled export works as the auditor; `SELECT` of any
      `round_secrets` column as the auditor is 42501.
    - New `TestExportUnsettledNeedsPrivilegedPath`. On a drained but unsettled round (`in-progress`)
      and a sealed one:
      1. as the auditor: the default export is exit 6 with the new message and no file;
      2. as the auditor with `-unsettled` and a keyring: exit 4 (42501);
      3. as auditor plus secret reader with `-unsettled`, without a keyring: exit 2;
      4. with `pgtest.KeyringFile`: exit 0, `settled no`, and `audit verify` accepts the file;
      5. with a keyring holding another key under the ID `pgtest`: exit 1.
    - `TestExportDatabaseErrors`, "missing grant": drop the reveal grant; exit 4 (42501).
    - `TestExportCorruptRound`:
      - "reveal deleted" (replica mode): exit 6;
      - "secret deleted" with `-unsettled` and the privileged path: exit 1;
      - "secret replaced": a replica-mode update of the reveal's `config_secret`; exit 1, fairness
        failure, as today.
    - `TestExportUnsupportedRules`: the default export is exit 6. `-unsettled` with superuser and
      `KeyringFile` is exit 0 with `settled unknown`, and `audit verify` is exit 3.
    - `TestExportStdout` "not settled" and `TestExportFollowsSnapshots`: expect the not-revealed message
      before settlement. Their `-unsettled` steps set `ORVEX_SECRET_KEYRING_FILE`.
    - `TestExportExecutedRounds`: expectations unchanged.
  - `migrate_integration_test.go`: the `down` expectations, where `0010` now refuses first.

### Step 7. `internal/runtime/admission` tests

- `helpers_integration_test.go:226`: insert the sealed form, using `pgtest.Seal`.
- `role_integration_test.go`, `TestAdmittingRoleCannotReadCloseBound`: `SELECT secret_ciphertext FROM
  orvex.round_secrets` and `SELECT config_secret FROM orvex.round_secret_reveals` are 42501, and
  `has_table_privilege(name, 'orvex.round_secret_reveals', 'SELECT')` is false.

### Step 8. Docs

- `docs/architecture/persistence.md`:
  - §1: the `round_secrets` row and a new `round_secret_reveals` row; relationships.
  - §3: `OX010` and the new constraints.
  - §5: header lines "from `0010` `(LBT-21 spec K-n)`".
  - §6: rewritten as "Round secrets at rest": K-1, R-1, R-2.
  - §8: the K-14 matrix.
  - §12: the admitting role has nothing on the reveal table.
  - §13: round creation, the cold load, the reveal in the Transaction B batch, the engine grants.
  - §14: the secret sources, `-unsettled`, the new exit 6 and exit 2 cases, the K-12 rule replacing
    "password masked", the auditor grants.
  - §15: v2 uses the same secret sources.
  - New §16, "Round secret sealing (LBT-21)": the keyring format, the permission rule, generation, the
    rotation runbook and its query, compromise and loss, R-1 to R-8.
- `README.md`, line 40: `-unsettled` needs the privileged path and `ORVEX_SECRET_KEYRING_FILE`.
- No ADR changes; earlier specs stay unchanged as the record.

## 10. Acceptance and the tests that prove it

| Acceptance | Tests |
|---|---|
| seeds are not stored in plaintext | `TestSeedNotStoredInPlaintext`, `TestRoundSecretsSealedForm`, `TestCreateRound`, `TestSealedFormatMatchesSpec`, `TestOpenRejectsTampering` |
| the export role cannot read an unsettled round's secret without the privileged path | `TestExportUnsettledNeedsPrivilegedPath`, `TestExportMinimalGrants`, `TestRoundSecretReveals`, `TestRevealWrittenAtSettlement`, `TestComputeTransitionReveal`, `TestAdmittingRoleCannotReadCloseBound` |
| crashctl never prints any part of the configured database URL | `TestNeverPrintsDatabaseURLAgainstServer`, `TestExportNeverPrintsDatabaseURL`, `TestMigrateNeverPrintsDatabaseURL`, `TestDescribe` |
| existing exports and audits still verify | `TestExportGoldenHistories`, `TestExportV2GoldenHistories`, `TestExportExecutedRounds`, `TestMigrate0010LegacySecrets`, `TestLegacySecretRoundCompletes`, `TestExportSecretSources`, `TestAuditGolden` (unchanged), `make golden-check` |
| key management documented and enforced | `TestParseKeyring`, `TestLoadKeyringFile`, `TestKeyringFormatting`, `TestOpenWithInactiveKey`, `TestColdLoadPrefersReveal`, `TestColdLoadUnknownKey`, `TestExportKeyringEnv`, and `persistence.md` §16 |
| atomic with settlement (ADR-006 failure test) | `TestRevealAtomicWithSettleRecord` |

## 11. Verification

- This planning run, on the unchanged tree: `make test`, `make lint`, `make build`, `make golden-check`.
- The implementation run:
  - `make test` and `make lint`;
  - `make test-integration`, because the change touches `migrations/`, `internal/persistence/` and
    `internal/runtime/`;
  - `make golden-check`, which must show no diff;
  - `make build`;
  - the `%w` check from step 6;
  - `git diff --stat -- migrations/` must show only the two new `0010` files.

## 12. Follow-ups (outside LBT-21)

1. A commit-and-open closure guard, to remove `D` and `T0 + D` from rest (R-1). It needs an LBT-12
   addendum change.
2. `CHECK (round_snapshots.state IS NULL)`, or sealing of snapshot state (R-5).
3. A KMS-backed `secret_scheme` (K-2 alternative), once a KMS exists.
4. LBT-14: create the roles, distribute the keyring, run the rotation, test the role matrix, and add the
   keyring to the backup and restore tests (K-14, section 3 item 11).
5. Optional: a `crashctl secrets keygen` command; the `openssl` command covers it for now.
