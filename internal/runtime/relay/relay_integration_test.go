//go:build integration

package relay_test

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/canonjson"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest/roundtest"
	"github.com/juano-morello/orvex/internal/runtime/execution"
	"github.com/juano-morello/orvex/internal/runtime/relay"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

var ctx = context.Background()

// newRound creates a round, whose genesis record queues one outbox
// message, and returns the pool and the round ID.
func newRound(t *testing.T) (*pgxpool.Pool, [16]byte) {
	t.Helper()
	pool := pgtest.Pool(t, pgtest.MigratedDatabase(t))
	x := roundtest.NewExecutor(t, pool, nil, nil)
	round := [16]byte{0x0e, 6: 0x70, 15: 0x01}
	if _, err := x.CreateRound(ctx, execution.NewRound{
		RoundID: round, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber,
		Config:             v1.Config{RoundID: round, Seed: [32]byte{0xab, 31: 0xcd}, MinBetCredits: 1, MaxBetCredits: 100, MaxBets: 10},
		CreatedAtUnixMicro: time.Now().UnixMicro(),
	}); err != nil {
		t.Fatal(err)
	}
	return pool, round
}

func unpublished(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM orvex.outbox_messages WHERE published_at IS NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPublishMarksAndNotifiesAtCommit(t *testing.T) {
	pool, round := newRound(t)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+relay.Channel); err != nil {
		t.Fatal(err)
	}

	if n, err := relay.Publish(ctx, pool, 100); err != nil || n != 1 {
		t.Fatalf("Publish = %d, %v; want 1", n, err)
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	note, err := conn.Conn().WaitForNotification(wctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := string(canonjson.UUID(round)) + ":0"; note.Payload != want {
		t.Errorf("payload %q, want %q", note.Payload, want)
	}
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT attempts FROM orvex.outbox_messages WHERE round_id = $1 AND published_at IS NOT NULL`,
		round).Scan(&attempts); err != nil || attempts != 1 {
		t.Errorf("published row attempts %d, %v; want 1", attempts, err)
	}
	if n, err := relay.Publish(ctx, pool, 100); err != nil || n != 0 {
		t.Errorf("second Publish = %d, %v; want 0", n, err)
	}
}

func TestPublishSkipsLockedMessages(t *testing.T) {
	pool, _ := newRound(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM orvex.outbox_messages FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if n, err := relay.Publish(pctx, pool, 100); err != nil || n != 0 {
		t.Errorf("Publish while another relay holds the row = %d, %v; want 0 without waiting", n, err)
	}
	_ = tx.Rollback(ctx)
	if n, err := relay.Publish(ctx, pool, 100); err != nil || n != 1 || unpublished(t, pool) != 0 {
		t.Errorf("Publish after release = %d, %v; want 1 and nothing left", n, err)
	}
}

func TestListenWakesTheNotifiedRound(t *testing.T) {
	pool, round := newRound(t)
	other := [16]byte{0x0e, 6: 0x70, 15: 0x02}
	w := relay.NewWakeups()
	ready, releaseReady := w.Wait(round)
	defer releaseReady()
	lctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		relay.Listen(lctx, pool.Config().ConnConfig, w, slog.New(slog.DiscardHandler))
		close(done)
	}()
	defer func() { stop(); <-done }()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("Listen did not wake waiters after LISTEN")
	}

	woken, releaseWoken := w.Wait(round)
	defer releaseWoken()
	untouched, releaseUntouched := w.Wait(other)
	defer releaseUntouched()
	if _, err := pool.Exec(ctx, `SELECT pg_notify($1, $2)`, relay.Channel, string(canonjson.UUID(round))+":3"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-woken:
	case <-time.After(5 * time.Second):
		t.Fatal("NOTIFY did not wake the round's waiter")
	}
	select {
	case <-untouched:
		t.Error("NOTIFY of one round woke another round's waiter")
	default:
	}
}

// TestListenWakesEveryWaiterAfterReconnecting terminates the LISTEN
// connection. Listen connects again, wakes every waiter, since it may have
// missed notifications, and then delivers the next notification.
func TestListenWakesEveryWaiterAfterReconnecting(t *testing.T) {
	pool, round := newRound(t)
	w := relay.NewWakeups()
	ready, releaseReady := w.Wait(round)
	defer releaseReady()
	lctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		relay.Listen(lctx, pool.Config().ConnConfig, w, slog.New(slog.DiscardHandler))
		close(done)
	}()
	defer func() { stop(); <-done }()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("Listen did not wake waiters after LISTEN")
	}

	missed, releaseMissed := w.Wait(round)
	defer releaseMissed()
	var terminated int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE pg_terminate_backend(pid)) FROM pg_stat_activity
		WHERE datname = current_database() AND query = 'LISTEN `+relay.Channel+`'`).Scan(&terminated); err != nil || terminated != 1 {
		t.Fatalf("terminated %d LISTEN connections, %v; want 1", terminated, err)
	}
	select {
	case <-missed:
	case <-time.After(10 * time.Second):
		t.Fatal("Listen did not wake waiters after reconnecting")
	}

	woken, releaseWoken := w.Wait(round)
	defer releaseWoken()
	if _, err := pool.Exec(ctx, `SELECT pg_notify($1, $2)`, relay.Channel, string(canonjson.UUID(round))+":1"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-woken:
	case <-time.After(5 * time.Second):
		t.Fatal("NOTIFY after the reconnect did not wake the round's waiter")
	}
}
