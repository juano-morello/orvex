package relay

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestWakeups(t *testing.T) {
	w := NewWakeups()
	a, b := [16]byte{1}, [16]byte{2}
	waitA1, releaseA1 := w.Wait(a)
	waitA2, releaseA2 := w.Wait(a)
	waitB, releaseB := w.Wait(b)
	if closed(waitA1) || closed(waitB) {
		t.Fatal("a wait channel is closed before any notify")
	}
	w.Notify(a)
	if !closed(waitA1) || !closed(waitA2) {
		t.Error("Notify(a) did not wake a's waiters")
	}
	if closed(waitB) {
		t.Error("Notify(a) woke b's waiter")
	}
	next, releaseNext := w.Wait(a)
	if closed(next) {
		t.Error("a wait after Notify is already closed")
	}
	releaseA1()
	releaseA2()
	w.Notify([16]byte{3})
	w.NotifyAll()
	if !closed(waitB) || !closed(next) {
		t.Error("NotifyAll did not wake every waiter")
	}
	releaseB()
	releaseNext()
	if n := w.rounds(); n != 0 {
		t.Errorf("%d rounds still tracked after every wait ended", n)
	}
}

// TestWakeupsReleaseForgetsTheRound: a round whose waiters all leave
// without a notify is not kept, and a waiter still holding the round keeps
// it.
func TestWakeupsReleaseForgetsTheRound(t *testing.T) {
	w := NewWakeups()
	round := [16]byte{9}
	ch1, release1 := w.Wait(round)
	ch2, release2 := w.Wait(round)
	if ch1 != ch2 {
		t.Fatal("two waits of one round got different channels")
	}
	release1()
	if w.rounds() != 1 {
		t.Fatal("releasing one of two waits forgot the round")
	}
	release2()
	if w.rounds() != 0 {
		t.Fatal("the round is still tracked after its last wait ended")
	}
	w.Notify(round)
	if closed(ch1) {
		t.Error("a forgotten wait was closed by a later Notify")
	}
}

func TestParsePayload(t *testing.T) {
	round, ok := parsePayload("0b7e6a1c-3c2f-4e0a-9a51-6d2f0c1e4b77:5")
	if !ok || round[0] != 0x0b || round[15] != 0x77 {
		t.Errorf("parsePayload = %x, %v", round, ok)
	}
	for _, p := range []string{"", "0b7e6a1c-3c2f-4e0a-9a51-6d2f0c1e4b77", "0B7E6A1C-3C2F-4E0A-9A51-6D2F0C1E4B77:5", "x:5"} {
		if _, ok := parsePayload(p); ok {
			t.Errorf("parsePayload(%q) accepted", p)
		}
	}
}

func TestDescribeKeepsNoText(t *testing.T) {
	secret := "password=hunter2 host=db.internal"
	for err, want := range map[error]string{
		errors.New(secret): "connection failure",
		fmt.Errorf("x: %w", &pgconn.PgError{Code: "57P01", Message: secret}): "sqlstate 57P01",
		fmt.Errorf("%s: %w", secret, context.Canceled):                       "context ended",
	} {
		if got := Describe(err); got != want {
			t.Errorf("Describe(%v) = %q, want %q", err, got, want)
		}
	}
}
