# Plan: LBT-21, protect round seeds at rest and restrict secret access (implementation)

Run `76544c17`. Issue: [LBT-21](https://linear.app/lbtworks/issue/LBT-21/protect-round-seeds-at-rest-and-restrict-secret-access).
Base: `main` at `3c66059` (LBT-20 gosec/errorlint and LBT-24 `pgtest/roundtest` are merged).

The normative design is the approved LBT-21 spec (run `87d8d90c`). It is not in this worktree. An
exact copy is at:

`/home/juano/Workspaces/LBTWorks/Orvex/odin/data/sessions/76544c17/context_handoff/lbt-21-spec.original.md`

(sha256 `fd3ce5b8c13d1380118597bd236dbcfa5d4c407fa500f39006fa8cd5bc80d4bd`, identical to
`/home/juano/Workspaces/LBTWorks/Orvex/requests/lbt-21-approved-spec.md`). This plan calls it "the
spec". Its decisions are cited as K-n, its sections as "spec §n". Read the spec in full before you start.
This plan carries the spec's section 9 over to today's tree and resolves the gaps that only show against
real code. Where the plan and the spec differ, the plan says so in §2 and gives the reason.

The engineer approved K-1 to K-15 as written, with no edits. The engineer also accepted every point of
spec §3, including item 8: `D` and `T0 + D` stay plaintext (residual risk R-1, tracked as LBT-26).

Spec §0 ("Builder, this run: make no repository changes") applied to the planning run `87d8d90c` only.
It does not apply to this run.

Governing ADRs: ADR-001, ADR-003, ADR-004, ADR-005, ADR-006, ADR-007. **No ADR conflict and no ADR
amendment.** Spec §3 lists the approved decisions of earlier specs that this change supersedes (LBT-10
P-25, P-13 grant matrix, LBT-19 X-4, X-7, X-9, X-11, LBT-12 E-3, E-6, E-16). Copy spec §3 and spec §6
(R-1 to R-8) into the PR description verbatim.

---

## 1. Facts this plan relies on (verified on `3c66059`)

- One module, Go 1.26. `go.mod` gains nothing: sealing uses only `crypto/aes`, `crypto/cipher`,
  `crypto/rand` and `encoding/base64`.
- `.golangci.yml` enables `errorlint` and `gosec` on top of `standard`, with `build-tags: [integration]`,
  so integration tests are linted too. The repository convention (LBT-20,
  `docs/specs/2cd18d70_ci-fuzz-repeat-runs.md` §5.3): fix the code first. If a finding is a false
  positive, append a trailing `//nolint:gosec // G<nnn>: <specific reason>` or
  `//nolint:errorlint // <reason>` to the reported line. Never put a nolint on a block, a function or a
  file, and never use `#nosec`. Run `gofmt -w` on every file you touch.
- Production code touches `config_secret` in exactly four places: `internal/runtime/execution/round.go:180`,
  `internal/runtime/execution/load.go:35-41`, `internal/persistence/eventstore/export.go:77-87` and
  `internal/persistence/eventstore/export_v2.go:72-82`.
- Test code that writes `round_secrets` directly:
  - `internal/persistence/postgres/pgtest/load.go:87`
  - `internal/persistence/postgres/schema_integration_test.go:342` (`insertRoundRows`) and `:421-425`
  - `internal/persistence/postgres/close_bound_integration_test.go:36`
  - `internal/runtime/admission/helpers_integration_test.go:226` (`createRoundWith`)
  - `cmd/crashctl/export_integration_test.go:340,352` (replica mode)
  - `internal/runtime/execution/round_integration_test.go:85-95` (reads it)
- Every executor that tests build goes through `roundtest.NewExecutor`
  (`internal/persistence/postgres/pgtest/roundtest/roundtest.go:49`) or through the in-package
  `execution.New` calls in `internal/runtime/execution/executor_test.go` (lines 79, 87, 112, 129, 136,
  142, 200).
- Import graph. `pgtest` imports `chain`, `verification`, `canonjson`, `postgres` and `migrations`.
  `go list -deps` of `execution`, `eventstore`, `crash/v1` and `replay` contains no `pgtest`. Every
  package that imports `pgtest` is an external test package (`*_test`) or `cmd/crashctl` (`package
  main`). So `pgtest` may import `execution`, `crash/v1`, `replay` and `sealing`. **The in-package tests
  of `execution` (`package execution`) must never import `pgtest`.** That would be an import cycle.
- `pgx.ConnConfig` exposes `Password` and `ConnString()`, the original DSN. `*pgconn.ConnectError` has
  an unexported `err` field, so a unit test cannot build one by hand and must get a real one from a
  failed connect. `pgconn.Timeout(err)` reports pgconn timeouts.
- `eventstore.ExportHistoryV2` is not wired into crashctl. `exporters` maps only v1 (`export.go:50`).
- The golden histories: `worked-example` (settles at 21; 22 is a rejected `PlaceBet`), `instant-crash`
  (5), `empty-round` (3) and `in-progress` (unsettled, last record 3). `golden-v2/in-progress.jsonl`
  carries the `round_pool` account and the ledger rows. It has no `house` account.
- Migration first lines must match `migrate_test.go:147`:
  `^-- ([0-9]{4}) ([a-z][a-z0-9_]*): .+ \(LBT-[0-9]+ spec [A-Z]-[0-9]+[^)]*\)$`.
- The catalog fingerprint (`pgtest/fingerprint.go`) covers comments. `TestMigrateEachStepReversible`
  therefore requires 0010's down to restore every comment exactly.
- `make test-repeat-export` runs `-run 'TestExport'` in `cmd/crashctl` and `internal/persistence/eventstore`,
  so every new test named `TestExport*` runs there 3 times. `make test-repeat-execution` runs
  `'Conflicting|StaleCache|CrossRound|Takeover|Crash'`.

## 2. Precisions and interpretations (flag these in the PR and in your report)

None of these changes an approved decision's intent. Each one resolves something the spec text leaves
open or gets wrong against the real code.

- **I-1. Local file errors in `crashctl audit export` keep their current text.** K-12 step c is applied to
  every error that crosses the database driver: `pgx.ConnectConfig` (step b), the `eventstore` export
  and the migration runner. Some errors come from the operator's own `FILE` path instead (the
  pre-checks `os.Lstat`/`os.Stat`, the `O_EXCL` create, the write, sync and close) or from writing
  stdout, stderr or the report. These keep today's messages, built by crashctl from the path and the
  OS error. Step d (the DSN/password scrub and `oneLine`) still runs on every line printed. Reason:
  step c would render the `syscall.Errno` inside an `*fs.PathError` as `database driver error` and
  mislabel a local I/O failure. These errors carry no value derived from the DSN, `PG*`, the passfile or
  the service file, so the K-12 guarantee (item 2) holds. Exit codes do not change.
  `TestExportMissingDirectory` and `TestExportOutputFailures` keep passing unchanged.
- **I-2. `TestSeedNotStoredInPlaintext` searches for two needles.** With `bytea_output = 'hex'`, a bytea
  holding `{"seed":"<hex>"}` prints as the hex of those ASCII bytes, not as `<hex>`. A row matches if
  `t::text` contains the seed's lowercase hex **or** the lowercase hex encoding of the ASCII bytes of
  that hex string. Without the second needle the test could not see a plaintext `config_secret` at all.
- **I-3. `TestExportExecutedRounds`, row `in-progress`:** the default export is still exit 6, but its
  stderr becomes the K-10 not-revealed message (spec §3 item 5). The `-unsettled` step and the direct
  `eventstore.ExportHistory` call need the privileged path. The spec's "expectations unchanged" holds
  for the settled rows, every report and every exit code.
- **I-4. `orvex.check_secret_reveal()` leaves NULL inputs and `sequence < 1` to the constraints**, as
  `check_record_binding` does. `BEFORE` triggers run before `CHECK`s. Without the early return, a reveal
  at sequence 0 would hit the genesis record (`result_status` NULL) and raise `OX010` instead of the
  23514 `round_secret_reveals_sequence_check` that the spec's `TestRoundSecretReveals` requires.
- **I-5. `TestRoundSecretsAppendOnly`:** `round_secret_reveals` now references `round_secrets`, so a
  plain `TRUNCATE orvex.round_secrets` fails with `0A000` before any trigger runs (persistence.md §10).
  Use `requireTruncateRejected` (`TRUNCATE … CASCADE`, `OX001`) there.
- **I-6. The CHECKs are NULL-safe.** A `CHECK` passes when its expression is NULL. The sealed-shape
  check therefore states `IS NOT NULL` for every sealed column in its "all set" branch (§6.1).
- **I-7. `CreateRound` seals once per call**, after validation and before the retry loop (K-8: "before
  any database call"). The spec's "each attempt draws a new nonce" then means each `CreateRound` call. A
  duplicate call's new sealed form is never stored, because `rounds_pkey` conflicts first.
- **I-8. Where crashctl loads the keyring:** after every flag, argument, `ORVEX_DATABASE_URL` and `FILE`
  pre-check, immediately before `pgx.ConnectConfig` (K-13: "after the flag and argument checks and
  before connecting"). An unset **or empty** `ORVEX_SECRET_KEYRING_FILE` means no keyring.
- **I-9. Exit-code order.** K-9 wraps `sealing` errors in `eventstore.ErrCorrupt`. `exportExitCode` must
  therefore test `ErrKeyringRequired`, `sealing.ErrUnknownKey` and `sealing.ErrKeyring` (exit 2)
  **before** `ErrCorrupt` (exit 1).
- **I-10. The approved spec's §7 checklist:** tick items 1 and 2, the ones the engineer approved
  explicitly. Leave item 3 ("The residual risks R-1 to R-8, and the follow-ups in section 12")
  unticked. The request accepts R-1 explicitly but does not mention R-2 to R-8 or the follow-ups. The
  engineer can tick it.
- **I-11. In TestExportCorruptRound, "secret deleted" deletes the reveal row and the `round_secrets`
  row**, both in replica mode. If the reveal stayed, it would win (K-9 step 1) and the test would not
  reach the missing secret.
- **I-12. New test helpers:** `pgtest.MigrateUpTo` and `pgtest.ReplicaExec` (§7.2). Several new tests
  need a schema-9 database or a replica-mode write. crashctl's own `replicaExec` stays as it is: no
  drive-by refactor.

## 3. Files (complete list)

New:
- `docs/specs/87d8d90c_round-secret-protection.md` (the approved spec, §4)
- `internal/persistence/sealing/sealing.go`
- `internal/persistence/sealing/sealing_test.go`
- `migrations/0010_sealed_secrets.up.sql`
- `migrations/0010_sealed_secrets.down.sql`
- `internal/persistence/postgres/pgtest/keyring.go`
- `internal/persistence/postgres/pgtest/load_integration_test.go`
- `internal/runtime/execution/secrets_integration_test.go`
- `cmd/crashctl/database_test.go`
- `cmd/crashctl/database_integration_test.go`

Edited, production:
- `internal/persistence/postgres/errors.go`, `internal/persistence/postgres/doc.go`
- `internal/persistence/postgres/pgtest/load.go`, `internal/persistence/postgres/pgtest/pgtest.go`
  (package doc and the new `MigrateUpTo` and `ReplicaExec`)
- `internal/persistence/postgres/pgtest/roundtest/roundtest.go`
- `internal/runtime/execution/executor.go`, `round.go`, `load.go`, `transition.go`, `commit.go`, `doc.go`
- `internal/persistence/eventstore/export.go`, `internal/persistence/eventstore/export_v2.go`
- `cmd/crashctl/database.go`, `cmd/crashctl/export.go`, `cmd/crashctl/migrate.go`, `cmd/crashctl/main.go`

Edited, tests:
- `internal/persistence/postgres/errors_test.go`, `schema_integration_test.go`,
  `close_bound_integration_test.go`, `migrate_integration_test.go`
- `internal/runtime/execution/executor_test.go`, `transition_test.go`, `helpers_integration_test.go`,
  `ledger_audit_integration_test.go`, `round_integration_test.go`, `failure_integration_test.go`
- `internal/persistence/eventstore/export_integration_test.go`
- `internal/runtime/admission/helpers_integration_test.go`, `internal/runtime/admission/role_integration_test.go`
- `cmd/crashctl/export_test.go`, `migrate_test.go`, `main_test.go`, `export_helpers_integration_test.go`,
  `export_integration_test.go`, `migrate_integration_test.go`

Edited, docs: `docs/architecture/persistence.md`, `README.md`, `AGENTS.md`.

Plus this plan's own copy, `docs/specs/76544c17_round-secret-sealing.md`, which already exists. Do not
edit it.

**Must not change:** `go.mod`, `go.sum`, `Makefile`, `.golangci.yml`, `.github/`, `docs/adr/`,
migrations `0001` to `0009`, anything under any `testdata/` (golden vectors, golden histories, fuzz
corpora), `internal/game/` (crash/v1 math, codec, contract), `internal/audit/` (the verifier, formats
v1/v2), `cmd/engine`, `cmd/gateway`, `deployments/`, and every other file under `docs/specs/`.

## 4. Step 0: add the approved spec

1. `cp /home/juano/Workspaces/LBTWorks/Orvex/odin/data/sessions/76544c17/context_handoff/lbt-21-spec.original.md docs/specs/87d8d90c_round-secret-protection.md`
2. Replace lines 3 to 6 (the `Status:` paragraph) with exactly these four lines:

   ```text
   Status: **APPROVED (2026-10-16).** The engineer approved K-1 to K-15 as written, with no edits, and
   accepted every point of section 3, including item 8: `D` and `T0 + D` stay plaintext, a residual risk
   tracked separately as LBT-26. Section 0 described the planning run and no longer applies;
   implementation follows section 9.
   ```
3. In the decision table (lines 206 to 220), change each trailing `| [ ] |` to `| [x] |` (15 rows).
4. In section 7, change `- [ ]` to `- [x]` on the first two items ("K-1 to K-15 as written…" and
   "Section 3 items 1 to 8…"). Leave the third item unticked (I-10).
5. Check: `diff <original> docs/specs/87d8d90c_round-secret-protection.md` shows changes only to
   lines 3 to 6, 206 to 220, 761 and 762. Nothing else in the file changes.

## 5. Step 1: package `internal/persistence/sealing` (K-2 to K-5)

### 5.1 `sealing.go`

Package doc: "Package sealing seals and opens round secret configs with AES-256-GCM under a keyring of
256-bit keys (LBT-21, docs/specs/87d8d90c_round-secret-protection.md, decisions K-2 to K-5)." It also
says that PostgreSQL never sees a key, and that no error text, `String`, `GoString` or `Format` output
contains key bytes, base64, plaintext, a nonce or ciphertext.

Exported API:

```go
const (
    Scheme              = "aes-256-gcm/v1"
    KeyringFileEnv      = "ORVEX_SECRET_KEYRING_FILE"
    KeySize             = 32
    NonceSize           = 12
    TagSize             = 16
    MaxKeys             = 64
    MaxKeyringFileBytes = 64 << 10
)

var (
    ErrKeyring    = errors.New("sealing: invalid keyring")
    ErrUnknownKey = errors.New("sealing: the keyring has no key with the sealed secret's key id")
    ErrScheme     = errors.New("sealing: unknown sealing scheme")
    ErrOpen       = errors.New("sealing: sealed secret failed authentication")
)

// KeyringError is a keyring that cannot be used: Reason is "unreadable",
// "not a regular file", "permissions too open (0644)", "larger than 64 KiB",
// "line N: <rule>" or a whole-file rule. It never holds file contents, key
// material or the path. errors.Is(err, ErrKeyring) holds.
type KeyringError struct{ Reason string }
func (e *KeyringError) Error() string { return "sealing: invalid keyring: " + e.Reason }
func (e *KeyringError) Unwrap() error { return ErrKeyring }

// Sealed maps one-to-one onto round_secrets' four sealed columns (K-5).
type Sealed struct {
    Scheme, KeyID     string
    Nonce, Ciphertext []byte // Ciphertext includes the 16-byte tag
}

type Keyring struct { active string; aeads map[string]cipher.AEAD } // opaque

func NewKeyring(active string, keys map[string][]byte) (*Keyring, error)
func ParseKeyring(b []byte) (*Keyring, error)
func LoadKeyringFile(path string) (*Keyring, error)
func (k *Keyring) ActiveKeyID() string
func (k *Keyring) Seal(roundID [16]byte, plaintext []byte) (Sealed, error)
func (k *Keyring) Open(roundID [16]byte, s Sealed) ([]byte, error)
func (k Keyring) String() string               // value receivers: see below
func (k Keyring) GoString() string
func (k Keyring) Format(f fmt.State, verb rune)
```

Rules:
- The key ID pattern is `^[a-z0-9][a-z0-9._-]{0,63}$`.
- `NewKeyring` requires 1 to `MaxKeys` keys, every ID matching the pattern, every key exactly
  `KeySize` bytes, and `active` among the IDs. Otherwise it returns a `*KeyringError`. It builds one
  `cipher.NewGCM(aes.NewCipher(key))` per key and keeps only the IDs and the AEADs, never the raw
  bytes.
- `ParseKeyring` implements K-2 exactly:
  - ASCII only, LF or CRLF (strip one trailing `\r` per line);
  - fields separated by one or more spaces or tabs, with leading blanks allowed;
  - blank lines and lines whose first non-blank character is `#` are ignored;
  - exactly one `active <id>` line and 1 to 64 `key <id> <base64>` lines;
  - unique IDs; the active ID must be listed;
  - `<base64>` decoded with `base64.StdEncoding.Strict()` (padded, RFC 4648 §4) to exactly 32 bytes;
  - any other directive or field count is an error.

  Line errors are `&KeyringError{Reason: fmt.Sprintf("line %d: %s", n, rule)}`. The rule texts are
  fixed strings such as "not ASCII", "unknown directive", "wrong number of fields", "invalid key id",
  "duplicate key id", "second active line", "key is not padded standard base64", "key is not 32 bytes"
  and "more than 64 keys". The whole-file rules are "no active line", "no key lines" and "active key id
  is not listed". **No reason includes a key ID, a line's content, base64 or key bytes.**
- `LoadKeyringFile` (no TOCTOU):
  1. `os.Stat(path)` follows symlinks and must report a regular file. A non-regular file is "not a
     regular file", and this check runs before the open, so a FIFO cannot block the open.
  2. `os.Open(path)`. Add `//nolint:gosec // G304: ORVEX_SECRET_KEYRING_FILE names the operator's own
     keyring; mode and size are checked on the opened file before parsing`.
  3. `f.Stat()` must satisfy `os.SameFile` with step 1's result, must be regular, and must have
     `perm & 0o027 == 0`. Otherwise the reason is `fmt.Sprintf("permissions too open (%04o)", perm)`.
     The size must be at most `MaxKeyringFileBytes`, else "larger than 64 KiB".
  4. Read at most `MaxKeyringFileBytes+1` bytes (`io.LimitReader`), then `ParseKeyring`.

  Every stat, open or read error is `&KeyringError{Reason: "unreadable"}`. Do not wrap the OS error: its
  text contains the path.
- `Seal`:
  - draw the nonce with `crypto/rand.Read(nonce)` (gosec G407 recognizes `rand.Read`; return its error
    if it ever has one);
  - set `Scheme`, `KeyID = active` and `Ciphertext = aead.Seal(nil, nonce, plaintext, aad(Scheme, active, roundID))`;
  - on a nil `*Keyring`, return an error wrapping `ErrKeyring`.
- `Open` checks in this order:
  1. `s.Scheme != Scheme` is `ErrScheme`;
  2. a key ID that is not in the keyring (or a nil keyring) is `ErrUnknownKey`;
  3. `len(s.Nonce) != NonceSize` or a failed `aead.Open` is `ErrOpen`.

  Wrap each with `fmt.Errorf("%w", …)` or return the sentinel directly. The message may name the key
  ID, which is not secret, and nothing else.
- `aad(scheme, keyID string, roundID [16]byte) []byte` = `"orvex/round-secret/v1"`, 0x00, scheme, 0x00,
  keyID, 0x00, the 16 round ID bytes (K-5).
- `String`, `GoString` and `Format` print only the active ID and the key count, for example
  `sealing.Keyring{active: "k2026-10", keys: 2}`. They use **value receivers**, so that printing either
  a `Keyring` value or a `*Keyring` goes through them. With the default formatting, a struct value
  would print the AEADs and their expanded key schedules. `Format` handles every verb (`%v`, `%+v`,
  `%#v`, `%s`, `%q`) with that same text.

### 5.2 `sealing_test.go` (package `sealing_test`, no build tag)

Write the tests the spec names in §9 step 1, exactly as specified there:

- `TestSealOpenRoundTrip`
- `TestSealRandomNonce`
- `TestSealedFormatMatchesSpec`: builds the AAD byte by byte in the test and opens with
  `cipher.NewGCM` directly.
- `TestOpenRejectsTampering`
- `TestOpenWithInactiveKey`
- `TestParseKeyring`: a table. Each error case asserts `errors.Is(err, sealing.ErrKeyring)`, that the
  message names the line (for line rules), and that it contains neither the key's base64 nor
  `string(key)`.
- `TestLoadKeyringFile`: accepts `0400`, `0600`, `0640` and a symlink to a `0400` file. Rejects `0604`,
  `0660`, `0644`, a directory, a missing file and a file over 64 KiB, each with the reason K-13 lists,
  and the path never appears in the message. Write the file with `0o600`, then `os.Chmod` to the mode
  under test. Each over-wide `Chmod` needs `//nolint:gosec // G302: the test sets a too-open mode to
  check that LoadKeyringFile rejects it`.
- `TestKeyringFormatting`: `%v`, `%+v`, `%#v`, `%s`, `String()` and `GoString()` of both `k` and `*k`
  contain the active ID and no key base64, raw key bytes or hex of the key.
- Add `TestNewKeyring`: the `NewKeyring` rules (no keys, 65 keys, a 31-byte key, a bad ID, active not
  listed: each `ErrKeyring`).

## 6. Step 2: migration `0010_sealed_secrets` and the Go constant (K-5, K-6, K-11, K-15)

### 6.1 `migrations/0010_sealed_secrets.up.sql` (LF line endings)

The first line, exactly:
`-- 0010 sealed_secrets: sealed round secrets, revealed secrets, legacy reveal backfill (LBT-21 spec K-5, K-6, K-11)`

Then, in this order (style: like `0009`; every object schema-qualified; functions `LANGUAGE plpgsql`
with `SET search_path = pg_catalog`):

```sql
-- 1. round_secrets: the sealed form (K-5).
ALTER TABLE orvex.round_secrets
    ALTER COLUMN config_secret DROP NOT NULL,
    ADD COLUMN secret_scheme     text,
    ADD COLUMN secret_key_id     text,
    ADD COLUMN secret_nonce      bytea,
    ADD COLUMN secret_ciphertext bytea,
    ADD CONSTRAINT round_secrets_sealed_shape_check CHECK (
        (secret_scheme IS NULL AND secret_key_id IS NULL AND secret_nonce IS NULL AND secret_ciphertext IS NULL)
        OR (secret_scheme IS NOT NULL AND secret_key_id IS NOT NULL
            AND secret_nonce IS NOT NULL AND secret_ciphertext IS NOT NULL
            AND secret_scheme = 'aes-256-gcm/v1'
            AND secret_key_id ~ '^[a-z0-9][a-z0-9._-]{0,63}$'
            AND octet_length(secret_nonce) = 12
            AND octet_length(secret_ciphertext) >= 18)),
    ADD CONSTRAINT round_secrets_form_check CHECK ((config_secret IS NULL) <> (secret_ciphertext IS NULL));

-- NOT VALID: enforced on every new row, never checked on rows written before
-- 0010 (legacy plaintext). Never VALIDATE it while legacy rows exist (R-2).
ALTER TABLE orvex.round_secrets
    ADD CONSTRAINT round_secrets_sealed_check CHECK (config_secret IS NULL) NOT VALID;
```

- Comments:
  - Replace the table comment (today: "…stored unencrypted…") with one stating:
    - from 0010 the engine stores the secret config only sealed with AES-256-GCM in the `secret_*`
      columns;
    - `config_secret` holds plaintext only in rows written before 0010;
    - `close_offset_micros` is the secret close offset (ADR-004, ADR-005; LBT-21).
  - Add column comments on `config_secret` and on the four new columns. No comment may contain a key
    or a secret.
- `CREATE FUNCTION orvex.check_secret_reveal() RETURNS trigger`, with invoker rights (no `SECURITY
  DEFINER`):
  - `IF NEW.round_id IS NULL OR NEW.sequence IS NULL OR NEW.sequence < 1 THEN RETURN NEW; END IF;` (I-4);
  - select `c.result_status` from `orvex.chain_records c` at `(NEW.round_id, NEW.sequence)`;
    `IF NOT FOUND THEN RETURN NEW;` (the foreign key reports it);
  - `IF v_status IS DISTINCT FROM 'accepted'`, raise
    `'orvex.round_secret_reveals: record % of round % is not an accepted command record'` with
    `ERRCODE = 'OX010'`;
  - otherwise `RETURN NEW`.

  Its comment: "Raises OX010 when a reveal names a chain record that is not an accepted command record of
  its round (ADR-004, ADR-005)."
- The reveal table:

  ```sql
  CREATE TABLE orvex.round_secret_reveals (
      round_id      uuid   NOT NULL,
      sequence      bigint NOT NULL,
      config_secret bytea  NOT NULL,
      CONSTRAINT round_secret_reveals_pkey PRIMARY KEY (round_id),
      CONSTRAINT round_secret_reveals_secret_fkey FOREIGN KEY (round_id) REFERENCES orvex.round_secrets (round_id),
      CONSTRAINT round_secret_reveals_record_fkey FOREIGN KEY (round_id, sequence)
          REFERENCES orvex.chain_records (round_id, sequence),
      CONSTRAINT round_secret_reveals_sequence_check CHECK (sequence >= 1),
      CONSTRAINT round_secret_reveals_config_secret_check CHECK (octet_length(config_secret) >= 2)
  );
  ```

  Add a table comment: "Secret config revealed at settlement: written in the Transaction B of the
  record that finalizes the round; public from then on (ADR-004, ADR-005)." Then three triggers:
  - `round_secret_reveals_record`: `BEFORE INSERT … FOR EACH ROW EXECUTE FUNCTION
    orvex.check_secret_reveal()`;
  - `round_secret_reveals_append_only`: `BEFORE UPDATE OR DELETE … FOR EACH ROW EXECUTE FUNCTION
    orvex.forbid_mutation()`;
  - `round_secret_reveals_no_truncate`: `BEFORE TRUNCATE … FOR EACH STATEMENT EXECUTE FUNCTION
    orvex.forbid_mutation()`.
- The backfill (K-11 step 3), one statement:

  ```sql
  INSERT INTO orvex.round_secret_reveals (round_id, sequence, config_secret)
  SELECT s.round_id, h.sealed_sequence, s.config_secret
      FROM orvex.round_secrets s
      JOIN orvex.round_admission_heads h ON h.round_id = s.round_id AND h.admission_state = 'sealed'
      JOIN orvex.chain_records c ON c.round_id = s.round_id AND c.sequence = h.sealed_sequence
          AND c.result_status = 'accepted'
      WHERE s.config_secret IS NOT NULL
      ORDER BY s.round_id;
  ```
- No `GRANT`, `REVOKE` or `CREATE ROLE` (P-13, K-14: LBT-14 owns them).

### 6.2 `migrations/0010_sealed_secrets.down.sql`

The first line, exactly:
`-- 0010 sealed_secrets: drop sealed and revealed secrets, refusing when round_secret_reveals or sealed round_secrets rows exist (LBT-21 spec K-11)`

1. A `DO $$ … $$` block, in the style of `0009`'s down:
   - if `orvex.round_secret_reveals` holds rows, raise
     `'down migration refused: orvex.round_secret_reveals holds rows'` with `OX003`;
   - if any `orvex.round_secrets` row has `secret_ciphertext IS NOT NULL`, raise
     `'down migration refused: orvex.round_secrets holds sealed rows'` with `OX003`.
2. `DROP TABLE orvex.round_secret_reveals;` and `DROP FUNCTION orvex.check_secret_reveal();`
3. `ALTER TABLE orvex.round_secrets`: drop the three constraints and the four columns, then
   `ALTER COLUMN config_secret SET NOT NULL`.
4. `COMMENT ON COLUMN orvex.round_secrets.config_secret IS NULL;` and restore 0002's table comment
   byte for byte: `'Codec secret config bytes (the seed), stored unencrypted outside chain records (ADR-004, ADR-005).'`
   `TestMigrateEachStepReversible` compares fingerprints, comments included.

### 6.3 Go side of the schema

- `internal/persistence/postgres/errors.go`: add `CodeSecretReveal = "OX010"` with the comment "a reveal
  row names a chain record that is not an accepted command record of its round (LBT-21 K-15)".
- `internal/persistence/postgres/errors_test.go`: `TestCodes` gains `{CodeSecretReveal, "OX010"}`.
- `internal/persistence/postgres/doc.go`:
  - the `rounds, round_secrets` bullet: the secret config is sealed from 0010; legacy rows keep
    plaintext;
  - a new bullet: `round_secret_reveals`, the secret config revealed by the record that finalizes a
    round (OX010).

### 6.4 Tests in `internal/persistence/postgres`

- `schema_integration_test.go`:
  - `insertRoundRows`: insert the sealed form: `s := pgtest.Seal(t, g.RoundID, <same secret bytes>)`,
    then `INSERT … (round_id, secret_scheme, secret_key_id, secret_nonce, secret_ciphertext)`.
  - `TestRoundSecretsAppendOnly`:
    - `UPDATE … SET secret_ciphertext = '\x00'::bytea` and the `DELETE` are `OX001`;
    - replace the plain `TRUNCATE` line with `requireTruncateRejected(t, pool, "round_secrets")` (I-5);
    - the row count stays 1.
  - New `TestRoundSecretsSealedForm`. Build each case on a fresh `rounds` row and insert one
    `round_secrets` row. Expect:
    - plaintext only: 23514 `round_secrets_sealed_check`;
    - both forms: 23514 `round_secrets_form_check`;
    - no form: 23514 `round_secrets_form_check`;
    - each of these is 23514 `round_secrets_sealed_shape_check`: scheme `aes-256-gcm/v2`, key ID
      `Bad`, an 11-byte nonce, a 17-byte ciphertext, and `secret_ciphertext` set with
      `secret_nonce` NULL;
    - a valid `pgtest.Seal` row commits.
  - New `TestRoundSecretReveals`. Use `loadGolden(t, pool, "worked-example")` (pgtest reveals it at 21)
    and `loadGolden(t, pool, "in-progress")` (no reveal). Expect:
    - in-progress at sequence 0: 23514 `round_secret_reveals_sequence_check`;
    - worked-example at 22 (a rejected record): `OX010`, raised by the `BEFORE` trigger ahead of the
      primary key;
    - in-progress at 99: 23503 `round_secret_reveals_record_fkey`;
    - in-progress after deleting its `round_secrets` row with `pgtest.ReplicaExec`: 23503
      `round_secret_reveals_secret_fkey`. Use a second in-progress-like round, or run this case last;
    - worked-example at 21 again: 23505 `round_secret_reveals_pkey`;
    - `UPDATE`, `DELETE` and `requireTruncateRejected(t, pool, "round_secret_reveals")`: `OX001`;
    - in-progress at 1 (an accepted record), with a fresh in-progress load if needed: commits.
  - `TestTruncateRejectedEverywhere`: 14 tables become 15.
- `close_bound_integration_test.go` (`TestCloseBoundColumns`, `insertSecret`): insert the sealed form of
  `{}` (`pgtest.Seal`) with `close_offset_micros`. The expectations do not change.
- `migrate_integration_test.go`:
  - `TestMigrateDownRefusesAuditData`: the comment becomes "0010 refuses first: round_secret_reveals
    holds the loaded round's reveal". The version stays `len(ms)`, so replace the literal 9 with
    `len(ms)`.
  - New `TestMigrate0010LegacySecrets` (the spec's name):
    1. Start from an empty database, `up(t, pool, ms, 9)`.
    2. `pgtest.LoadLegacyHistory(empty-round)`. In one `UPDATE`, set its head to `admission_state =
       'sealed', started_sequence = 1, closed_sequence = 2, sealed_sequence = 3`.
    3. `pgtest.LoadLegacyHistory(in-progress)`.
    4. `up(t, pool, ms, len(ms))`.
    5. Assert:
       - both legacy rows are unchanged (`config_secret` equals each `Loaded.Secret`, the sealed
         columns are NULL);
       - exactly one reveal, for empty-round at sequence 3, whose bytes equal its plaintext;
       - none for in-progress;
       - a new plaintext-only insert fails with 23514 `round_secrets_sealed_check`;
       - `MigrateDown(…, 9)` fails with `OX003`, the version stays `len(ms)` and the row counts do
         not change.
  - Every literal schema version 9 in this file that means "the newest" becomes `len(ms)`.

## 7. Step 3: `pgtest` and `roundtest`

### 7.1 `internal/persistence/postgres/pgtest/keyring.go` (no build tag)

- `const KeyID = "pgtest"`.
- `func Keyring() *sealing.Keyring`: `sync.OnceValue`. Key ID `pgtest`, key
  `sha256.Sum256([]byte("orvex/pgtest/keyring/v1"))`. It panics on error, which cannot happen.
- `func KeyringFile(t testing.TB) string`: writes `active pgtest\nkey pgtest <base64>\n` to
  `t.TempDir()/keyring` with mode `0o600` and returns the path.
- `func Seal(t testing.TB, roundID [16]byte, secret []byte) sealing.Sealed`: `Keyring().Seal`, and
  `t.Fatal` on error.

### 7.2 `pgtest.go` additions

- `func MigrateUpTo(t testing.TB, dsn string, version int)`: connects, runs `postgres.MigrateUp(ctx,
  conn, ms, version, "pgtest")` and fails the test on error.
- `func ReplicaExec(t testing.TB, db *pgxpool.Pool, sql string, args ...any)`: one committed
  transaction with `SET LOCAL session_replication_role = replica`. It is the same as crashctl's helper,
  which stays.
- Update the package doc: it also seals round secrets with a fixed test keyring and writes their
  reveals.

### 7.3 `pgtest/load.go`

- Factor the body into `load(t, db, history, checkpoints, legacy bool) Loaded`.
  `LoadHistory = load(…, false)`. The new
  `LoadLegacyHistory(t, db, history, checkpoints) Loaded = load(…, true)` is "for databases at schema
  9: the plaintext `config_secret` and no reveal".
- `Loaded` gains `Secret []byte` (the canonical secret config bytes), `Revealed bool` and
  `RevealSequence uint64`.
- In non-legacy mode:
  - insert `round_secrets` sealed with `Keyring()` (four sealed columns, `config_secret` NULL);
  - after the chain records are inserted, compute the reveal sequence and, if there is one, insert
    `(round_id, sequence, secret)` into `orvex.round_secret_reveals`.

  The reveal sequence is the first record `r >= 1` whose replayed state is final:
  - if `execution.CrashV1Policy().Descriptor()` differs from the genesis descriptor (game, rules,
    schema, state version), there is no reveal;
  - otherwise `a := crashv1.Adapter()`; `DecodeConfig(roundID, genesis.Config, secret)`;
    `InitialState`; decode each command record's events with `a.DecodeEvent` into `replay.Record`s;
  - run `replay.Run(a, initial, records, replay.StateOnly, visit)`, where visit records the first
    `seq >= 1` with `Policy.Final(state)` true;
  - any decode or replay error means no reveal: do not fail the test, because tamper histories load
    too.
- The legacy mode inserts `(round_id, config_secret)` exactly as today and never touches the reveal
  table.

### 7.4 `pgtest/load_integration_test.go` (new; `//go:build integration`; `package pgtest_test`)

- `func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }`. The package has no other `TestMain`.
  `pgtest_test.go`'s `run` tests never set the `server` global on failure.
- `TestLoadHistorySealsAndReveals`:
  - `worked-example`:
    - sealed: `config_secret` NULL, key ID `pgtest`;
    - `pgtest.Keyring().Open` returns `Loaded.Secret`;
    - one reveal at 21, whose bytes are `Loaded.Secret`;
    - `Revealed`, `RevealSequence == 21`.
  - `in-progress`: sealed, no reveal, `!Revealed`.
  - `tamper/rules-version-2`: no reveal.

### 7.5 `roundtest/roundtest.go`

`NewExecutor`: set `cfg.Keyring = pgtest.Keyring()` before `mutate`, so a mutate can replace it. Update
its doc comment.

## 8. Step 4: `internal/runtime/execution` (K-7, K-8)

### 8.1 Code

- `executor.go`:
  - `Config` gains `Keyring *sealing.Keyring` with the doc "seals each new round's secret config and
    opens sealed secrets at cold load (LBT-21 K-8). Required."
  - `New` returns `fmt.Errorf("%w: nil Keyring", ErrInvalidConfig)` when it is nil. Check it in `New`,
    after `cfg.validate()`, so `DefaultConfig().validate()` keeps passing.
  - `DefaultConfig()` stays as it is (Keyring nil) and its doc says callers set `Keyring`.
  - `entry` gains `secret []byte`: the plaintext secret config bytes, kept next to the state (K-8).
- `round.go` (`CreateRound`):
  - after the close-offset validation and before the retry loop: `sealed, err :=
    e.cfg.Keyring.Seal(r.RoundID, secret)`. An error is `fmt.Errorf("%w: seal secret config: %w",
    ErrInternal, err)` (I-7).
  - `insertRound` takes `sealed sealing.Sealed` instead of `secret` and queues
    `INSERT INTO orvex.round_secrets (round_id, secret_scheme, secret_key_id, secret_nonce, secret_ciphertext, close_offset_micros) VALUES ($1, $2, $3, $4, $5, $6)`.
  - Step 5 (seeding the entry) also sets `en.secret = secret`.
  - Doc comment: "its sealed secret config".
- `load.go` (cold load, step 1):
  - The query becomes:

    ```sql
    SELECT r.game, r.rules_version, r.schema_version, r.state_version,
        v.config_secret, s.config_secret, s.secret_scheme, s.secret_key_id, s.secret_nonce, s.secret_ciphertext,
        h.processed_sequence, h.fencing_token
        FROM orvex.rounds r
        LEFT JOIN orvex.round_secret_reveals v USING (round_id)
        LEFT JOIN orvex.round_secrets s USING (round_id)
        LEFT JOIN orvex.round_execution_heads h USING (round_id)
        WHERE r.round_id = $1
    ```
  - Keep the existing `ErrCorrupt` "no secret config or execution head" check when no form is present
    (reveal, legacy and ciphertext all NULL), or the head is missing.
  - After the `ErrFenced` check, choose the secret: the reveal, else the legacy plaintext, else
    `e.cfg.Keyring.Open(lease.RoundID, sealing.Sealed{…})`. An open error is `fmt.Errorf("%w: secret
    config: %w", ErrCorrupt, err)`, so `errors.Is(err, sealing.ErrUnknownKey | ErrScheme | ErrOpen)`
    holds, and `ExecuteNext` halts the round as for any `ErrCorrupt`.
  - Step 7 sets `en.secret`.
  - No error text contains the plaintext, a key, the nonce or the ciphertext.
- `transition.go`:
  - `transition` gains `reveal bool` ("this record first makes the round final; Transaction B writes
    the reveal").
  - In step 7: `finalBefore, err := r.policy.Final(st.state)`, failing closed on error like `final`;
    then `tr.reveal = tr.final && !finalBefore`.
- `commit.go`:
  - add `insertRevealSQL = INSERT INTO orvex.round_secret_reveals (round_id, sequence, config_secret) VALUES ($1, $2, $3)`;
  - queue it last in the batch, after the snapshot, when `tr.reveal`, with `en.secret`;
  - update the comment on Transaction B's statements.

  `OX010` and any class-23 error there already map to `ErrInternal` (`classifyStatementError` →
  `stmtInternal`). Do not add a mapping. An adopted outcome needs nothing (the reveal committed with
  the record).
- `doc.go`: two bullets.
  - `CreateRound` stores the secret config sealed with `Config.Keyring` (AES-256-GCM, LBT-21).
  - The cold load reads the reveal, else legacy plaintext, else opens the sealed form. The record that
    first makes the round final writes the reveal in its Transaction B.

### 8.2 Tests

In-package tests (`package execution`) must not import `pgtest` (§1). Build their keyring with
`sealing.NewKeyring("test", map[string][]byte{"test": bytes.Repeat([]byte{7}, 32)})` in a
`testKeyring(t)` helper in `executor_test.go`.

- `executor_test.go`:
  - In `TestNewValidation`, `base` and both `edges` set `Keyring: testKeyring(t)`, so each `bad` case
    fails for its own reason.
  - In `TestExecuteNextValidation` and `TestContextErrorWrapsCtxErr`, the configs set `Keyring`.
  - `TestDefaultConfig` stays as it is.
  - New `TestNewRequiresKeyring`: `base` with `Keyring` nil fails with `ErrInvalidConfig`; with a
    keyring, `New` succeeds.
- `transition_test.go`, new `TestComputeTransitionReveal`:
  - Walk `loadGolden(t, "worked-example")` as `TestTransitionReproducesGoldenRecords` does.
  - `tr.reveal` is true only at 21. It is false at 22 (final before and after), at every rejected
    record and at every earlier record.
  - Also check that `tr.final` is true at 21 and 22.
  - Plus: a `rulesEntry` whose policy's `Final` returns an error for the pre-record state gives a
    `*FailClosedError` (`errors.Is(err, ErrFailClosed)`).
- `helpers_integration_test.go`:
  - `exportHistory` passes `eventstore.Options{Unrevealed: true, Keyring: pgtest.Keyring()}`. Its
    callers export unsettled rounds as well, and the reveal still wins for settled ones.
- `ledger_audit_integration_test.go`: `exportHistoryV2` does the same.
- `round_integration_test.go`:
  - `requireCreatedRows` reads `config_secret` and the four sealed columns. It asserts:
    - `config_secret` is NULL;
    - scheme `aes-256-gcm/v1`, key ID `pgtest`, a 12-byte nonce, `len(ciphertext) == len(wantSecret)+16`;
    - `pgtest.Keyring().Open(round, sealed)` equals `wantSecret`;
    - `close_offset_micros` is unchanged;
    - no `round_secret_reveals` row.
- New `secrets_integration_test.go` (`//go:build integration`, `package execution_test`):
  - `TestSeedNotStoredInPlaintext`:
    1. A crash/v1 round with a seed drawn from `crypto/rand`, redrawn until `500ms <= D <= 2s`
       (helper `randomSeedWithDeadline`, at most 10 000 draws).
    2. Use `newSequencer` and `roundtest.FundPlayer`. Admit two `PlaceBet`s and `StartRound`, drain,
       wait until T0 + 1, admit a `CashOut` and drain (the pattern of `TestAdmitAndExecuteRound`).
    3. Then, as the superuser, in one transaction with `SET LOCAL bytea_output = 'hex'`, for every
       `pg_tables` row with `schemaname IN ('orvex', 'orvex_migrate')`, count the rows `t` where
       `strpos(t::text, $1) > 0 OR strpos(t::text, $2) > 0` (I-2). Expect 0 everywhere.
    4. Then admit and drain `CloseRound` (resubmit until due) and `SettleRound`. The seed now appears
       only in `round_secret_reveals`, `chain_records` and `outbox_messages`.
  - `TestRevealWrittenAtSettlement`:
    - `SeedGolden(worked-example)`, then `ExecuteNext` one at a time;
    - before 21 there are 0 reveals;
    - after 21 exactly one, at sequence 21, whose bytes equal the golden header's canonical
      `configSecret`;
    - after 22 still that one row.
  - `TestColdLoadOpensSealedSecret`: drain worked-example to 10, `x.Forget`, then `ExecuteNext` executes
    11 through a cold load that opens the sealed form.
  - `TestColdLoadPrefersReveal`:
    - drain worked-example to 21 with `x`;
    - `y := roundtest.NewExecutor(t, pool, func(c *execution.Config) { c.Keyring = other }, nil)`,
      where `other` has only the key ID `other`;
    - `y.ExecuteNext` executes 22 through a cold load (the reveal).
  - `TestColdLoadUnknownKey`:
    - the same `y` on a drained `in-progress` round;
    - `ExecuteNext` returns `ErrCorrupt` and `sealing.ErrUnknownKey`;
    - the error text does not contain the seed's hex;
    - a second call returns the same halt.
  - `TestColdLoadTamperedSecret`:
    - `pgtest.ReplicaExec` with `UPDATE orvex.round_secrets SET secret_ciphertext =
      set_byte(secret_ciphertext, 0, get_byte(secret_ciphertext, 0) # 1) WHERE round_id = $1`;
    - a fresh executor's `ExecuteNext` returns `ErrCorrupt` and `sealing.ErrOpen`.
  - `TestLegacySecretRoundCompletes`:
    1. Start from `pgtest.EmptyDatabase` and `pgtest.MigrateUpTo(…, 9)`.
    2. `pgtest.LoadLegacyHistory` the **format v2** `golden-v2/in-progress.jsonl`. It carries the
       `round_pool` account and the ledger rows.
    3. Insert the `house` account (`ledger.HouseAccountID()`, `kind 'system'`, `name 'house'`,
       `round_id NULL`, `floor_enforced false`).
    4. `pgtest.MigrateUpTo(…, 10)`.
    5. Seed `CloseRound` and `SettleRound` directly with `roundtest.SeedAdmission` (the window is
       `open`). Their effective time is the `StartRound` record's effective time plus
       `InitialState(config).CrashDeadlineOffsetMicros`.
    6. Drain. The round settles (`Outcome.Final`), and its reveal equals the legacy plaintext.
- `failure_integration_test.go`:
  - New `TestRevealAtomicWithSettleRecord`. `installFaults`, `SeedGolden(worked-example)`, drain 20,
    `before := effects(…)`. Then two cases:
    - (a) `enableFault(t, pool, "round_secret_reveals", "23514", "round_secret_reveals_config_secret_check", 1_000_000)`:
      - `ExecuteNext` returns `ErrInternal`;
      - `effects` equal `before`: no record 21, no ledger transaction, no outbox row, no snapshot;
      - 0 reveals.
    - (b) the same, with the fault on `ledger_postings` (the settlement posting) instead: 0 reveals and
      `effects` unchanged.

    After `disableFaults`, `ExecuteNext` commits 21 with exactly one reveal.
  - `TestCrashAfterCommit`: new subtest `"settle record adopted"`. Over a `faultDB`, drain worked-example
    to 20, set `afterCommit` to return `io.ErrUnexpectedEOF` once, then `ExecuteNext`. The result is
    `Adopted`, sequence 21 and `Final`, with exactly one reveal row.

## 9. Step 5: `internal/persistence/eventstore` (K-9)

### 9.1 Code

- In `export.go`, beside the `Export` types:

  ```go
  // Options selects where ExportHistory and ExportHistoryV2 read the round's
  // secret config (LBT-21 K-9). The zero Options reads revealed secrets only.
  type Options struct {
      // Unrevealed reads round_secrets when the round has no reveal: the
      // privileged path (SELECT on round_secrets, and Keyring for a sealed row).
      Unrevealed bool
      // Keyring opens a sealed secret config; nil opens none.
      Keyring *sealing.Keyring
  }
  ```

  New sentinels:
  - `ErrNotRevealed = errors.New("eventstore: round has no revealed secret config")`
  - `ErrKeyringRequired = errors.New("eventstore: the round's secret config is sealed and no keyring was given")`
- `ExportHistory(ctx, db, roundID, w, opts Options)` and `ExportHistoryV2(…, opts Options)`: the new
  last parameter.
- Shared `readSecret(ctx context.Context, tx pgx.Tx, roundID [16]byte, opts Options) ([]byte, error)`.
  It implements K-9 steps 1 to 3 exactly, inside the existing snapshot transaction:
  - statement 1 never names `round_secrets`;
  - database errors are `eventstore: read revealed secret: %w` and `eventstore: read sealed secret: %w`;
  - a sealed row with a keyring is opened, and its error becomes `fmt.Errorf("%w: secret config: %w",
    ErrCorrupt, err)`;
  - no row is `fmt.Errorf("%w: round has no secret config", ErrCorrupt)`.

  `ExportHistory` replaces its inline secret query with it. `readExportBase` takes `opts` and calls it.
  Nothing else in either function changes, and neither do the written bytes.
- Package doc: add one paragraph on the secret sources and `Options`.

### 9.2 Tests (`export_integration_test.go`)

- Every existing call passes `eventstore.Options{}`, except where a round has no reveal.
- `TestExportGoldenHistories` and `TestExportV2GoldenHistories`:
  - when `l.Revealed`, export with `Options{}` and expect the bytes equal;
  - otherwise (`in-progress`):
    - `Options{}` returns `ErrNotRevealed` with `out.Len() == 0`;
    - then `Options{Unrevealed: true, Keyring: pgtest.Keyring()}` exports byte-equal, with every
      existing assertion kept.
- `TestExportV1UnchangedWithLedgerRows`: use the privileged options when `!l.Revealed`.
- New `TestExportSecretSources`:
  - the reveal wins with `Unrevealed` set and a nil keyring (worked-example);
  - `Unrevealed` with a nil keyring on sealed in-progress returns `ErrKeyringRequired` and writes
    nothing;
  - a keyring with another key under ID `pgtest`: `ErrCorrupt` and `sealing.ErrOpen`;
  - a keyring without `pgtest`: `sealing.ErrUnknownKey`;
  - legacy: on a schema-9 database (`EmptyDatabase` and `MigrateUpTo` 9), `LoadLegacyHistory`
    in-progress, then `MigrateUpTo` 10:
    - `Options{}` returns `ErrNotRevealed`;
    - `Options{Unrevealed: true}` with no keyring exports byte-equal to the golden;
  - no `round_secrets` row (`ReplicaExec` delete on in-progress), with `Unrevealed`: `ErrCorrupt`.

## 10. Step 6: `cmd/crashctl` (K-10, K-12, K-13)

### 10.1 `database.go`: the K-12 renderer

Remove `mask` and keep `oneLine`. Add:

- `connectFailure(err error) string` (step b): `"cannot connect to the database named by " +
  databaseURLEnv`. It appends `": SQLSTATE " + code` when `errors.As` finds a `*pgconn.PgError`.
  Otherwise it appends `": timeout"` when `pgconn.Timeout(err)` holds or an `errors.As` `net.Error` has
  `Timeout()`. Nothing else.
- `render(err error) string` (step c), node by node over `Unwrap() error` and `Unwrap() []error`:
  - A `*pgconn.PgError` renders as `"SQLSTATE " + Code`, plus `" constraint " + ConstraintName` when
    set. For a code starting with `OX` it renders as `Message + " (SQLSTATE " + Code + ")"`.
  - The concrete type's package (`reflect.TypeOf`, pointer elem, `PkgPath()`) is one of:
    `github.com/juano-morello/orvex` or below, `main` (crashctl itself), `fmt`, `errors`, `io/fs` or
    `context`. Then:
    - take the node's own `Error()` text;
    - for each child, replace every verbatim occurrence of the child's `Error()` text with the child's
      rendering;
    - if some child's text does not occur, render the node as its children's renderings joined by
      `": "`;
    - a child with empty text is skipped.
  - Any other node renders as `database driver error`, or `database driver error: timeout` when it
    (or its chain) is a network timeout. Its subtree is dropped and never printed.

  These deliberate node inspections may trip errorlint. If they do, add a line-level
  `//nolint:errorlint // K-12 renders each node of the wrap tree itself`.
- `redact(cfg *pgx.ConnConfig, s string) string` (step d): `oneLine(s)`, then replace
  `cfg.ConnString()` (if not empty) and `cfg.Password` (if not empty) with `xxxxx`.
- `describe(cfg *pgx.ConnConfig, err error) string` = `redact(cfg, render(err))`.

Step d replaces every occurrence of the password, exactly as today's `mask`. Tests that compare
exact stderr text must therefore use a DSN whose password occurs nowhere in the expected text.
`unreachableDSN`'s password is `p`, and the pgtest superuser's password is `orvex`, which turns
`orvex.<table>` into `xxxxx.<table>`. Under those DSNs, match on substrings that contain neither.

### 10.2 `export.go`

- `type exporter func(ctx context.Context, db eventstore.TxBeginner, roundID [16]byte, w io.Writer, opts eventstore.Options) (eventstore.Export, error)`.
- The `-unsettled` help text is exactly K-10's: `write the history even if the round is not settled; for a round whose secret is not revealed, this reads the sealed secret (needs SELECT on orvex.round_secrets and ORVEX_SECRET_KEYRING_FILE); the file then discloses its secret config`.
- Two print helpers, both one stderr line and both ending in step d:
  - `fail(err error, msg string) int`: Orvex-authored messages and local file errors (I-1). It prints
    `prefix + redact(cfg, msg)` and returns `exportExitCode(err)`.
  - `failDB(err error) int`: prints `prefix + describe(cfg, err)` and returns `exportExitCode(err)`.
- The keyring (K-13, I-8), immediately before `pgx.ConnectConfig`. Only with `-unsettled`, and only
  when `os.Getenv(sealing.KeyringFileEnv) != ""`, call `sealing.LoadKeyringFile`. On error:
  - print `prefix + sealing.KeyringFileEnv + ": " + ke.Reason` (`errors.As` to
    `*sealing.KeyringError`);
  - return exit 2.

  `opts := eventstore.Options{Unrevealed: *unsettled, Keyring: kr}`.
- Connect failure: `fail(err, connectFailure(err))`, exit 4 (the default class).
- Export errors:
  - `ErrRoundNotFound` and `ErrNoCheckpoint` keep their messages;
  - `ErrNotRevealed`: `fail(err, "round "+round+" has no revealed secret config: it is not settled; -unsettled exports it through the privileged path")`, exit 6;
  - `ErrKeyringRequired`: `fail(err, "round "+round+" has a sealed secret config and no revealed one; set ORVEX_SECRET_KEYRING_FILE to export it")`, exit 2;
  - anything else: `failDB(err)`.

  The `newRegistry` error also goes through `describe`. The settlement messages and the file and
  stdout errors stay as today (I-1).
- `exportExitCode`, in this order (I-9):
  1. nil
  2. `errOutputExists` → 2
  3. `errFormatUnsupported` → 3
  4. `errNotSettled` or `eventstore.ErrNotRevealed` → 6
  5. `ErrRoundNotFound` → 5
  6. `eventstore.ErrKeyringRequired`, `sealing.ErrUnknownKey` or `sealing.ErrKeyring` → 2
  7. `ErrNoCheckpoint` or `ErrCorrupt` → 1 (`sealing.ErrOpen` and `ErrScheme` arrive here)
  8. then the existing cases unchanged.
- Update the `runExport` doc comment (exit codes, LBT-21).

### 10.3 `migrate.go` and `main.go`

- `migrate.go`:
  - `fail := func(err error) { … prefix + describe(cfg, err) }`;
  - a connect failure prints `prefix + redact(cfg, connectFailure(err))`, exit 4;
  - it never reads `ORVEX_SECRET_KEYRING_FILE`.
- `main.go` `usage`: after the `ORVEX_DATABASE_URL` line, add
  `ORVEX_SECRET_KEYRING_FILE names the keyring audit export -unsettled reads to open a sealed, unrevealed round secret.`

### 10.4 Unit tests (no build tag)

- New `database_test.go`:
  - `TestDescribe`, a table. Expected outputs:
    - a 42501 `PgError` whose `Message` holds `sentinel_db_q1` → `SQLSTATE 42501`;
    - a 23514 with constraint `round_secrets_sealed_check` → `SQLSTATE 23514 constraint round_secrets_sealed_check`;
    - an `OX003` → `down migration refused: orvex.round_secret_reveals holds rows (SQLSTATE OX003)`;
    - `fmt.Errorf("eventstore: read records: %w", pgErr)` → `eventstore: read records: SQLSTATE 42501`;
    - a `fmt.Errorf("%w: %w", a, b)` chain → both renderings kept;
    - a `*net.OpError` whose address is `sentinel-host-q1.invalid:5432` → `…: database driver error`;
    - a custom `net.Error` with `Timeout() == true` → `database driver error: timeout`;
    - a real `*pgconn.ConnectError` from `pgconn.Connect` to
      `postgres://sentinel_user_q1:sentinel_pw_q1@127.0.0.1:1/sentinel_db_q1?connect_timeout=2`,
      wrapped by `fmt.Errorf` → `…: database driver error`;
    - an Orvex-package error type whose text omits its child's → the children's renderings;
    - step d: a message holding the raw DSN or the password → `xxxxx`.

    Every output must contain none of `sentinel_user_q1`, `sentinel_pw_q1`, `sentinel_db_q1`,
    `sentinel-host-q1` or `127.0.0.1:1`.
  - `TestConnectFailureMessage`:
    - a real refused connect → exactly `cannot connect to the database named by ORVEX_DATABASE_URL`;
    - a wrapped `PgError{Code: "28P01"}` → `…: SQLSTATE 28P01`;
    - a timeout `net.Error` → `…: timeout`.
- `export_test.go`:
  - Replace `TestExportNeverPrintsPassword` with `TestExportNeverPrintsDatabaseURL`.
    - Sentinel DSNs, each with `connect_timeout=2` where it parses:
      - URL and key=value forms with user `sentinel_user_q1`, password `sentinel_pw_q1`, database
        `sentinel_db_q1` and `application_name=sentinel_app_q1`;
      - host `sentinel-host-q1.invalid` (a DNS failure);
      - `127.0.0.1` port 1 (refused);
      - `sslmode=verify-full sslrootcert=/sentinel_q1/root.crt` (missing file, so exit 2 at parse);
      - an unparsable variant.
    - Run each across `audit export ROUND FILE`, `audit export ROUND -` and `audit export -unsettled
      ROUND FILE`.
    - stdout and stderr contain no `sentinel_` substring, no raw DSN and no `127.0.0.1:1`. The exit
      code is 2 or 4.
    - Each sentinel DSN literal needs `//nolint:gosec // G101: dummy credentials for a host or port
      nothing serves, not a secret`.
  - New `TestExportKeyringEnv`. Add a helper `writeKeyringFile(t, content string, mode os.FileMode) string`
    in this file, so the integration tests can use it too. Use the DSN
    `postgres://u:sentinel-kr-pw@127.0.0.1:1/db?connect_timeout=2` (with a G101 nolint), not
    `unreachableDSN`: step d would scrub its password `p` out of "permissions". Then:
    - with `-unsettled`:
      - a missing file: exactly `crashctl: audit export: ORVEX_SECRET_KEYRING_FILE: unreadable\n`,
        exit 2;
      - a valid file at `0644`: `…: permissions too open (0644)`, exit 2;
      - a `0600` file holding `active k1\nkey k1 !!!\n`: `…: line 2: key is not padded standard
        base64`, exit 2;
      - each: stdout empty, no output file, and stderr contains neither the path nor any key base64;
      - an empty `ORVEX_SECRET_KEYRING_FILE`: exit 4 (connect), i.e. no keyring;
    - without `-unsettled`, the variable pointing at the `0644` file: exit 4 (ignored).
  - `TestExportExitCodes`, new rows:
    - `eventstore.ErrNotRevealed` → 6;
    - `eventstore.ErrKeyringRequired` → 2;
    - `fmt.Errorf("%w: secret config: %w", eventstore.ErrCorrupt, sealing.ErrUnknownKey)` → 2;
    - the same with `sealing.ErrOpen` → 1, and with `sealing.ErrScheme` → 1;
    - `&sealing.KeyringError{Reason: "unreadable"}` → 2;
    - `fmt.Errorf("eventstore: read sealed secret: %w", &pgconn.PgError{Code: "42501"})` → 4.

    Rename the existing `wrapped` helper's text to `read revealed secret`.
  - `TestExportUsage`: the `-h` output contains the K-10 help text and `ORVEX_SECRET_KEYRING_FILE`.
- `migrate_test.go`:
  - Replace `TestMigrateNeverPrintsPassword` with `TestMigrateNeverPrintsDatabaseURL`: the same sentinel
    DSNs across `up`, `down -to 0` and `status`, with the same assertions.
  - New `TestMigrateIgnoresKeyringEnv`: `ORVEX_SECRET_KEYRING_FILE` pointing at a `0644` file, then
    `migrate status` against `unreachableDSN` is exit 4 and never mentions the variable (K-13).
- `main_test.go`, `TestRun`: a row "usage names the keyring" with `wantStderr`
  `ORVEX_SECRET_KEYRING_FILE`.

### 10.5 Integration tests (`//go:build integration`)

- `export_helpers_integration_test.go`:
  - `exportGrants` becomes the K-14 auditor set, with the reveal grant on its own line:

    ```go
    `GRANT CONNECT ON DATABASE %[2]s TO %[1]s`,
    `GRANT USAGE ON SCHEMA orvex TO %[1]s`,
    `GRANT SELECT ON orvex.rounds, orvex.chain_records TO %[1]s`,
    `GRANT SELECT ON orvex.round_secret_reveals TO %[1]s`,
    `GRANT SELECT (round_id, sequence, state_hash) ON orvex.round_snapshots TO %[1]s`,
    ```
  - New `secretReaderGrants = []string{"GRANT SELECT (round_id, config_secret, secret_scheme, secret_key_id, secret_nonce, secret_ciphertext) ON orvex.round_secrets TO %[1]s"}`.
    Granting `exportGrants` plus `secretReaderGrants` to one login role models membership in both
    `orvex_auditor` and `orvex_secret_reader`.
  - Split `restrictedRole` into `loginRole(t, superDSN, role, password string, grants []string) string`
    with the same cleanup. `restrictedRole` keeps its signature and calls it with
    `orvex_export_<n>` and `exportRolePassword`.
  - Update the comments (X-11 is superseded by K-14).
- `export_integration_test.go`:
  - Add `notRevealedMessage(round string) string`.
  - `TestExportExecutedRounds` (I-3):
    - in-progress's default export expects `notRevealedMessage`;
    - set `t.Setenv(sealing.KeyringFileEnv, pgtest.KeyringFile(t))` before its `-unsettled` step;
    - the direct `eventstore.ExportHistory` call uses the privileged options when `!r.settled`, and
      `Options{}` otherwise.
  - `TestExportFollowsSnapshots`:
    - for `k < 21` expect `notRevealedMessage` (exit 6, no file);
    - set the keyring env once at the top for the `-unsettled` steps.

    All other expectations stay.
  - `TestExportStdout` "not settled": `notRevealedMessage`.
  - `TestExportCorruptRound`:
    - "reveal deleted": `replicaExec` deletes the reveal; the default export is exit 6 with
      `notRevealedMessage`;
    - "secret deleted" (I-11): delete the reveal, then the `round_secrets` row. `-unsettled` as the
      superuser with the keyring env set is exit 1 with "violates an export invariant", one line and no
      file;
    - "secret replaced": `UPDATE orvex.round_secret_reveals SET config_secret = …` (was
      `round_secrets`). The expectations stay: exit 1 with a fairness failure and the hint, then
      `-unsettled` gives exit 0 and `settled unknown`, and `audit verify` gives exit 1.
  - `TestExportUnsupportedRules`:
    - the default export is exit 6 with `notRevealedMessage` and no file;
    - `-unsettled` as the superuser with `pgtest.KeyringFile` gives exit 0 and `settled unknown`;
    - `audit verify` gives exit 3.
  - `TestExportDatabaseErrors`:
    - "missing grant" drops the `round_secret_reveals` line (filter on `round_secret_reveals`): exit 4,
      stderr contains `eventstore: read revealed secret: SQLSTATE 42501`, no password and no role
      name;
    - "no schema" still expects `42P01`.
  - `TestExportMinimalGrants`: the settled export works as the auditor. Then, as the auditor, `SELECT
    <c> FROM orvex.round_secrets LIMIT 1` is 42501 for each of the seven columns (`round_id`,
    `config_secret`, the four sealed columns, `close_offset_micros`). The two other denied queries stay.
  - New `TestExportUnsettledNeedsPrivilegedPath` (spec §9 step 6, steps 1 to 5), on a drained
    `in-progress` round (sealed, unrevealed):
    1. as the auditor, the default export: exit 6, `notRevealedMessage`, no file;
    2. as the auditor with `-unsettled` and `pgtest.KeyringFile`: exit 4, stderr contains
       `eventstore: read sealed secret: SQLSTATE 42501`;
    3. as auditor plus secret reader with `-unsettled` and the env empty: exit 2, exactly
       `…round <id> has a sealed secret config and no revealed one; set ORVEX_SECRET_KEYRING_FILE to export it\n`;
    4. with `pgtest.KeyringFile`: exit 0, the report `wantExportReport(…, 2, 3, head, settledNo)`, and
       `verifyFile` accepts the file;
    5. with `writeKeyringFile(t, "active pgtest\nkey pgtest <other base64>\n", 0o600)`: exit 1, one
       line, no file.

    Also add step 6 (the K-10 row "`-unsettled`, revealed"): a drained worked-example, as the auditor,
    `-unsettled`, env empty: exit 0, `settled yes`.
- New `database_integration_test.go`, `TestNeverPrintsDatabaseURLAgainstServer`:
  - Create a login role `sentinel_role_q2_<n>` with password `sentinel_pw_q2` via `loginRole`. The
    pgtest database's unique name `orvex_test_<n>` is the database sentinel.
  - Cases:
    - wrong password (`audit export` and `migrate status`): exit 4, `SQLSTATE 28P01`;
    - unknown database: exit 4, `SQLSTATE 3D000`;
    - `REVOKE CONNECT ON DATABASE <db> FROM PUBLIC` with no grant to the role: exit 4,
      `SQLSTATE 42501`;
    - `migrate up` on an `EmptyDatabase` as the role, which has no `CREATE` on the database: exit 1,
      `SQLSTATE 42501`;
    - the auditor (role plus `exportGrants` without the reveal line), default export of a drained
      worked-example: exit 4;
    - the auditor with `-unsettled` and the keyring env on a drained in-progress: exit 4.
  - In every case, stdout and stderr contain none of: the role name, the password, the database name,
    the server's `host:port`, the raw DSN.
- `migrate_integration_test.go`:
  - `TestMigrateCLI`:
    - `names` gains `"0010 sealed_secrets"`;
    - `schema version 10`, 11 status lines, `down -to 10` is a no-op;
    - the "down with data" comment becomes "0010 refuses before any step completes".
  - `TestMigrateCLIStdoutFailure`: `schema version 10`, and the comment "the down refuses at 0010".
  - `TestMigrateCLIExitCodes` keeps its substring expectations (`42P07` and `0003 chain_execution up`,
    `3D000`, `28P01`, the runner phrases). They hold with K-12.

## 11. Step 7: `internal/runtime/admission` tests

- `helpers_integration_test.go` (`createRoundWith`): insert `round_secrets` sealed with
  `pgtest.Seal(t, roundID, secret)` and `close_offset_micros`. Update the doc comment.
- `role_integration_test.go` (`TestAdmittingRoleCannotReadCloseBound`):
  - add `SELECT secret_ciphertext FROM orvex.round_secrets` and `SELECT config_secret FROM
    orvex.round_secret_reveals` to the 42501 list;
  - add `SELECT has_table_privilege($1, 'orvex.round_secret_reveals', 'SELECT')` with want `false`.

## 12. Step 8: docs

- `docs/architecture/persistence.md`:
  - §1:
    - the `round_secrets` row: sealed from 0010, legacy plaintext, the three CHECKs;
    - a new row: `round_secret_reveals | append-only | the secret config revealed by the finalizing
      record | one per round; names an accepted command record (OX010)`;
    - relationships: `round_secrets` 1–0..1 `round_secret_reveals`, which references the record it
      was revealed at.
  - §3:
    - the `OX010 | CodeSecretReveal | …` row;
    - the constraints `round_secrets_sealed_check`, `round_secrets_form_check`,
      `round_secrets_sealed_shape_check`, `round_secret_reveals_pkey`, `_secret_fkey`,
      `_record_fkey` and `_sequence_check`.
  - §5: "from `0010` `(LBT-21 spec K-n)`".
  - §6 is rewritten as "Round secrets at rest" (K-1):
    - what is sealed and what is revealed;
    - `D` and `T0 + D` stay plaintext, protected by grants only: R-1, accepted, tracked as LBT-26;
    - legacy rows keep plaintext (R-2);
    - `round_snapshots.state` stays NULL (R-5).
  - §8: the K-14 matrix replaces the table and the engine paragraph.
  - §12: the admitting role has nothing on `round_secret_reveals`.
  - §13:
    - round creation writes the sealed form;
    - the cold load's source order and its errors;
    - the reveal as the last statement of B on the finalizing record;
    - `OX010` in the errors table;
    - the engine grants per K-14, including `SELECT` on `chain_records` for
      `orvex.check_secret_reveal()`.
  - §14:
    - the secret sources;
    - `-unsettled` and the privileged path;
    - exit codes: 6 adds "no revealed secret", 2 adds the keyring cases, 1 adds a sealed secret that
      fails to open;
    - the K-12 rule replaces "the password masked in every message";
    - the auditor grant SQL per K-14 (with `orvex_secret_reader`);
    - drop the "row-level restriction is future work" bullet.
  - §15: v2 uses the same secret sources and `Options`.
  - New §16 "Round secret sealing (LBT-21)":
    - the keyring format;
    - the permission rule;
    - the generation command from K-2;
    - distribution (K-14);
    - the rotation runbook with its query (K-4);
    - compromise and loss;
    - R-1 to R-8.
- `README.md`: replace the `crashctl audit export` bullet with:
  "`crashctl audit export [-unsettled] ROUND_ID FILE`: write a settled round's audit history file from
  the database named by `ORVEX_DATABASE_URL`; for a round whose secret config is not yet revealed,
  `-unsettled` needs the privileged path: `SELECT` on `orvex.round_secrets` and the keyring named by
  `ORVEX_SECRET_KEYRING_FILE`".
- `AGENTS.md`:
  - "Before changing anything": add "- round secrets, sealing, the keyring, the reveal table: ADR-004,
    ADR-005 and `docs/specs/87d8d90c_round-secret-protection.md` (LBT-21)".
  - "Repository conventions": add two bullets:
    1. "Round secret configs are sealed at rest (LBT-21). New rounds store only the AES-256-GCM form
       (`internal/persistence/sealing`), and the record that settles a round writes the plaintext to
       `orvex.round_secret_reveals`. No SQL statement, log line or error text may carry a key, keyring
       content or an unrevealed secret config. Tests seal with `pgtest.Keyring()`; no other key belongs
       in the repository."
    2. "`crashctl` never prints any part of `ORVEX_DATABASE_URL` (LBT-21 K-12): render database errors
       with `describe`, never with `err.Error()`."
- No ADR change. Earlier specs stay unchanged.

## 13. Acceptance → tests

| LBT-21 acceptance | Proven by (real PostgreSQL unless noted) |
|---|---|
| seeds of new rounds are not stored in plaintext | `TestSeedNotStoredInPlaintext`, `TestRoundSecretsSealedForm`, `TestCreateRound`, `TestLoadHistorySealsAndReveals`; unit: `TestSealedFormatMatchesSpec`, `TestOpenRejectsTampering` |
| the export role cannot read an unsettled round's secret without the privileged path | `TestExportUnsettledNeedsPrivilegedPath`, `TestExportMinimalGrants`, `TestRoundSecretReveals`, `TestRevealWrittenAtSettlement`, `TestRevealAtomicWithSettleRecord`, `TestAdmittingRoleCannotReadCloseBound`; unit: `TestComputeTransitionReveal` |
| `crashctl` never prints any part of the configured database URL | `TestNeverPrintsDatabaseURLAgainstServer`; unit: `TestExportNeverPrintsDatabaseURL`, `TestMigrateNeverPrintsDatabaseURL`, `TestDescribe`, `TestConnectFailureMessage` |
| existing exports, audits and golden histories still verify byte for byte | `TestExportGoldenHistories`, `TestExportV2GoldenHistories`, `TestExportV1UnchangedWithLedgerRows`, `TestExportExecutedRounds`, `TestExportSecretSources`, `TestMigrate0010LegacySecrets`, `TestLegacySecretRoundCompletes`, `TestExecuteReproducesGoldenHistories`, `TestAuditGolden` (unchanged), `make golden-check` |
| key management documented and enforced | `TestParseKeyring`, `TestNewKeyring`, `TestLoadKeyringFile`, `TestKeyringFormatting`, `TestOpenWithInactiveKey`, `TestColdLoadPrefersReveal`, `TestColdLoadUnknownKey`, `TestColdLoadTamperedSecret`, `TestExportKeyringEnv`, `TestMigrateIgnoresKeyringEnv`, persistence.md §16 |

## 14. Verification (run one at a time; report each command, exit status and duration)

Your shell tool moves any command that runs past 60 s into a background job. When that happens, wait
on that job until it exits, and never report before you have its final exit status. Judge every
command by its exit status. On a failure, fix the cause and rerun that target, and report every run.

1. `make build`
2. `make test`
3. `make lint`
4. `make golden-check`
5. `make test-integration`
6. `make test-repeat`

Then these checks:

- `grep -rnE 'fmt\.Errorf\(.*%[vsq].*\berr\b' --include=*.go internal/persistence internal/runtime cmd/crashctl | grep -v _test.go`:
  every hit must wrap `err` with `%w`. Today's 5 hits all do. errorlint enforces this as well.
- `git status --porcelain -- migrations/` shows exactly `?? migrations/0010_sealed_secrets.up.sql` and
  `?? migrations/0010_sealed_secrets.down.sql`.
- `git status --porcelain -- go.mod go.sum Makefile .golangci.yml .github docs/adr internal/game internal/audit cmd/engine cmd/gateway`
  and `git status --porcelain | grep testdata` print nothing.
- `diff` of the approved spec copy against the original shows only the lines listed in §4 step 5.

The report lists every changed or new file by its exact path, plus §2's interpretations I-1 to I-12.

## 15. Out of scope

- protecting `D` and `T0 + D` at rest (LBT-26);
- LBT-14: roles, grants, leases, the engine binary, keyring distribution and its role-matrix test
  (`cmd/engine` stays untouched);
- `make audit` (LBT-25, in parallel: do not touch the `Makefile`);
- KMS, re-encryption tooling, `crashctl secrets keygen`, sealing `round_snapshots.state`
  (spec §12 follow-ups);
- wiring `ExportHistoryV2` into crashctl;
- any change to history formats v1 and v2.
