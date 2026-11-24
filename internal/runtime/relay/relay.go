// Package relay turns committed chain records into in-process wakeups for
// round subscribers (ADR-005: the outbox delivers at least once and
// consumers deduplicate).
//
// Publish claims due rows of orvex.outbox_messages with FOR UPDATE SKIP
// LOCKED, marks them published and sends one NOTIFY per round on Channel,
// all in one transaction. PostgreSQL delivers a NOTIFY only at COMMIT, so a
// row is published exactly when its notification is. Listen holds a LISTEN
// connection and feeds a Wakeups value. A wakeup carries no record: a
// subscriber reads the database after each one, so a duplicate or lost
// notification costs an extra or a later read, never a duplicate or a gap.
package relay

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
)

// Channel is the NOTIFY channel. A payload is "<round UUID>:<sequence>",
// the round's highest published record sequence.
const Channel = "orvex_round_records"

// Wakeups wakes the waiters of a round. It is safe for concurrent use.
type Wakeups struct {
	mu      sync.Mutex
	waiters map[[16]byte]*waiters
}

// waiters is one round's pending wait: a channel closed on wakeup and the
// number of callers that have not released it.
type waiters struct {
	ch   chan struct{}
	held int
}

// NewWakeups returns a Wakeups with no waiters.
func NewWakeups() *Wakeups {
	return &Wakeups{waiters: make(map[[16]byte]*waiters)}
}

// Wait returns a channel that is closed by the next Notify of round or
// NotifyAll, and a release function to call exactly once when the caller
// stops waiting. Call Wait before reading the round, so that a record
// committed during the read still wakes the caller.
func (w *Wakeups) Wait(round [16]byte) (<-chan struct{}, func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	e, ok := w.waiters[round]
	if !ok {
		e = &waiters{ch: make(chan struct{})}
		w.waiters[round] = e
	}
	e.held++
	return e.ch, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if e.held--; e.held == 0 && w.waiters[round] == e {
			delete(w.waiters, round)
		}
	}
}

// Notify wakes every waiter of round.
func (w *Wakeups) Notify(round [16]byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if e, ok := w.waiters[round]; ok {
		close(e.ch)
		delete(w.waiters, round)
	}
}

// NotifyAll wakes every waiter, for when notifications may have been lost.
func (w *Wakeups) NotifyAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for round, e := range w.waiters {
		close(e.ch)
		delete(w.waiters, round)
	}
}

// rounds returns how many rounds have a pending wait.
func (w *Wakeups) rounds() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.waiters)
}

// DB starts transactions; *pgxpool.Pool implements it.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// claimSQL marks up to $1 due messages published and returns each claimed
// round's highest sequence. SKIP LOCKED lets several relays share the
// outbox; the ORDER BY matches outbox_messages_pending_idx.
const claimSQL = `WITH claimed AS (
		SELECT message_id FROM orvex.outbox_messages
		WHERE published_at IS NULL AND next_attempt_at <= now()
		ORDER BY next_attempt_at, message_id
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	), published AS (
		UPDATE orvex.outbox_messages o
		SET published_at = now(), attempts = o.attempts + 1
		FROM claimed c
		WHERE o.message_id = c.message_id
		RETURNING o.round_id, o.sequence
	)
	SELECT round_id::text || ':' || max(sequence)::text, count(*) FROM published GROUP BY round_id`

// Publish publishes up to limit due outbox messages in one transaction and
// returns how many it published.
func Publish(ctx context.Context, db DB, limit int) (int, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, claimSQL, limit)
	if err != nil {
		return 0, err
	}
	var (
		payloads []string
		n        int64
	)
	for rows.Next() {
		var p string
		var c int64
		if err := rows.Scan(&p, &c); err != nil {
			rows.Close()
			return 0, err
		}
		payloads, n = append(payloads, p), n+c
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(payloads) == 0 {
		return 0, nil
	}
	b := &pgx.Batch{}
	for _, p := range payloads {
		b.Queue(`SELECT pg_notify($1, $2)`, Channel, p)
	}
	if err := tx.SendBatch(ctx, b).Close(); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int(n), nil
}

// Relay tuning.
const (
	publishLimit    = 100
	publishInterval = 100 * time.Millisecond
	minBackoff      = 100 * time.Millisecond
	maxBackoff      = 5 * time.Second
)

// RunPublisher calls Publish until ctx is done: again at once after a full
// batch, otherwise every 100ms. Errors are logged and retried with backoff.
func RunPublisher(ctx context.Context, db DB, log *slog.Logger) {
	backoff := minBackoff
	for {
		n, err := Publish(ctx, db, publishLimit)
		wait := publishInterval
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			log.Warn("relay: publish failed", "error", Describe(err))
			wait, backoff = backoff, min(2*backoff, maxBackoff)
		case n == publishLimit:
			wait, backoff = 0, minBackoff
		default:
			backoff = minBackoff
		}
		if !sleep(ctx, wait) {
			return
		}
	}
}

// Listen LISTENs on Channel over its own connection, made from cfg, and
// wakes the notified rounds in w until ctx is done. It reconnects with
// backoff and calls w.NotifyAll after every LISTEN, so waiters recover
// notifications sent while it was disconnected.
func Listen(ctx context.Context, cfg *pgx.ConnConfig, w *Wakeups, log *slog.Logger) {
	backoff := minBackoff
	for {
		err := listenOnce(ctx, cfg, w, func() { backoff = minBackoff })
		if ctx.Err() != nil {
			return
		}
		log.Warn("relay: listen failed", "error", Describe(err))
		if !sleep(ctx, backoff) {
			return
		}
		backoff = min(2*backoff, maxBackoff)
	}
}

func listenOnce(ctx context.Context, cfg *pgx.ConnConfig, w *Wakeups, listening func()) error {
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_ = conn.Close(cctx)
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return err
	}
	listening()
	w.NotifyAll()
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		round, ok := parsePayload(n.Payload)
		if !ok {
			w.NotifyAll()
			continue
		}
		w.Notify(round)
	}
}

func parsePayload(p string) ([16]byte, bool) {
	id, _, ok := strings.Cut(p, ":")
	if !ok {
		return [16]byte{}, false
	}
	round, err := canonjson.ParseUUID(canonjson.String(id))
	return round, err == nil
}

// Describe names an error's class without its text, which can carry
// connection details: the SQLSTATE of a PostgreSQL error, "context ended",
// or "connection failure".
func Describe(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "context ended"
	}
	if code := postgres.SQLState(err); code != "" {
		return "sqlstate " + code
	}
	return "connection failure"
}

// sleep waits d or until ctx is done and reports whether ctx is still live.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
