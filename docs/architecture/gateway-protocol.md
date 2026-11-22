# Gateway protocol

Players reach the engine through the HTTP gateway. The gateway authenticates bearer tokens, submits commands to the engine over gRPC, and streams public round events over Server-Sent Events. The engine writes round data to PostgreSQL. Operators use `crashctl` to issue demo credit grants. The protocol is defined in `proto/orvex/v1/engine.proto`.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `ORVEX_GATEWAY_ADDR` | `127.0.0.1:8080` | HTTP listen address. |
| `ORVEX_ENGINE_ADDR` | Gateway defaults to `127.0.0.1:7001`. The engine requires an explicit value. | Set both services to the same loopback address. The gRPC connection carries a bearer token over plaintext. |
| `ORVEX_GATEWAY_TOKEN_FILE` | required | The service token shared with the engine: 32 to 512 printable ASCII bytes, no spaces. |
| `ORVEX_PLAYERS_FILE` | required | A JSON object mapping each bearer token (16 to 512 bytes) to a player ID (`^[a-z0-9_-]{1,64}$`). |

Both files load like the round-secret keyring: a regular file with `perm & 0o027 == 0`, read once at start. No error names a path or quotes a token. Once listening, the gateway prints `ready <address>` on stdout.

## Identity

A player's identity comes only from `Authorization: Bearer <token>`. No request body names a player or an authority: the gateway refuses unknown, mixed-case and repeated JSON keys, so a body with `player` is a 400. The gateway sends the player to the engine as the call's principal, and the engine trusts it only because the call carries the service token (`authorization: Bearer <service token>` metadata, compared by SHA-256 digest in constant time). The engine builds every command itself with player authority, so the wire cannot express StartRound, CloseRound or SettleRound.

The engine takes the first 16 bytes of SHA-256(`"orvex/command-id/v1"` 0x00 player 0x00 client ID) and sets the RFC 9562 variant and version 8 bits. Two players who pick the same `clientCommandId` get separate admissions. Each can look up their own result. The gateway exposes the client ID. The audit record contains the derived ID, whose encoding is pinned by `TestCommandIDGolden`.

## Endpoints

JSON is camelCase. Every 64-bit integer is a decimal string. UUIDs are canonical lowercase text.

| Request | Auth | Success |
|---|---|---|
| `GET /v1/rounds/current[?game=crash]` | none | 200 `{"roundId","game":"crash","rulesVersion":"1"}`, the newest crash/v1 round in any phase |
| `POST /v1/rounds/{roundId}/commands` | bearer | 202 admission receipt |
| `GET /v1/commands/{clientCommandId}` | bearer | 200 command status |
| `GET /v1/rounds/{roundId}/stream[?after=N]` | none | 200 `text/event-stream` |

A submission body is `{"clientCommandId":"<uuid>","placeBet":{"betCredits":"100"}}` or `{"clientCommandId":"<uuid>","cashOut":{}}`, at most 4096 bytes. Every object holds exactly these keys, spelled with this case, each once, so a repeated `clientCommandId` or `betCredits` is a 400 rather than a choice between two values. A refused body never reaches the engine and writes nothing.

The 202 receipt is `{"clientCommandId","roundId","sequence","effectiveTimeUnixMicro","duplicate","status":"admitted"}`, for a new admission and for a duplicate alike. It says the command was durably admitted at that sequence and effective time (ADR-002), not that it was executed. Execution outcomes come only from the status lookup. Earlier revisions of this pre-release API put `"status":"pending"` in the receipt.

A status is `{"clientCommandId","roundId","sequence","effectiveTimeUnixMicro","status","reason"?,"events":[...]}` with `status` one of `pending`, `accepted` or `rejected`. `events` holds this command's own events: `BetPlaced`, or `BetCashedOut` with `payoutCredits`. A bet lost at settlement appears in the stream, not here. A 404 is not definitive for a command still being admitted.

## Errors

Every error body is `{"code":"..."}`.

| Status | Codes | Meaning |
|---|---|---|
| 400 | `invalid_request`, `invalid_cursor` | Malformed request, or a cursor above the round's execution head. |
| 401 | `unauthenticated` | Missing or unknown bearer token. |
| 404 | `round_not_found`, `command_not_found`, `no_current_round`, `unknown_game`, `not_found` | `not_found` is a path the API does not serve. |
| 405 | `method_not_allowed` | A served path with another method. `Allow` names the methods: `POST` for submission, `GET, HEAD` for the others. |
| 409 | `round_closed`, `round_sealed`, `insufficient_credits`, `command_id_reused` | A definite rejection at admission. Nothing was written. |
| 413 | `request_too_large` | An authenticated submission body over 4096 bytes. Nothing was written. |
| 503 | `outcome_unknown`, `not_admitted`, `unavailable` | Resubmit the identical request (`Retry-After: 1`). |
| 500 | `internal` | Engine fault or gateway misconfiguration. |

A submission that fails for a retryable reason is `outcome_unknown`. The admission sequencer retries a lost COMMIT reply, and a lock timeout, a database outage or a cancellation on a later attempt does not show that the earlier COMMIT failed. `not_admitted` is reserved for a failure the engine can prove admitted nothing in any attempt; the current engine never sends it. Treat both codes alike.

Recovery from a lost response is always "resubmit the identical request with the same `clientCommandId`". A command that was admitted returns 202 with `duplicate: true` and its original sequence and effective time, even after the round settles, so a cash-out's eligibility never changes. The same `clientCommandId` with a different command is 409 `command_id_reused`. The gateway adds no retries of its own; the admission sequencer already retries.

## Stream

```
id: 5
event: record
data: {"sequence":"5","effectiveTimeUnixMicro":"1763800006210000","events":[{"type":"BetCashedOut","playerId":"alice",...}]}
```

Record 0 carries `"genesis":{"commitment","minBetCredits","maxBetCredits","maxBets"}` instead of `events`. A command the rules rejected appears with `"events":[]`, so sequences stay gapless; its command, reason and ID stay in the audit record. Event types are `BetPlaced`, `RoundStarted`, `BetCashedOut`, `RoundCrashed`, `BetLost` and `RoundSettled`; the seed appears only in `RoundSettled`.

After the round's last record the gateway sends `event: end` and closes the response. A client must not reconnect to that round. An idle stream carries `: ping` comments every 15 seconds.

The cursor is the last sequence the client holds: `Last-Event-ID`, or `?after=N` without it. The stream resumes at N+1, and without a cursor it starts at genesis. Within one connection, sequences are strictly increasing and gapless. Across reconnects delivery is at least once, so drop any frame whose sequence is at or below your cursor. If a connection ends without `event: end`, reconnect with `Last-Event-ID`.

## Engine side

`engineapi.Serve(ctx, listener, pool, sequencer, gatewayToken)` serves the gRPC API and runs two loops:

- **The relay.** It claims due `orvex.outbox_messages` rows with `FOR UPDATE SKIP LOCKED`, marks them published and sends one `NOTIFY orvex_round_records, '<round>:<sequence>'` per round, in one transaction. A NOTIFY is delivered at COMMIT, so a row is published exactly when its notification is.
- **The LISTEN loop.** It wakes the round's subscribers on each notification, and wakes every subscriber after each reconnect.

A subscriber registers its wakeup, reads the records after its cursor from one snapshot, sends them, then waits for a wakeup or a 1-second poll. The database is the truth: a duplicate or lost notification costs an extra or a later read, never a duplicate or a gap. The subscribe and lookup paths read `chain_records`, `rounds` and the heads, never `round_secrets`, `round_secret_reveals` or the keyring (LBT-21).
