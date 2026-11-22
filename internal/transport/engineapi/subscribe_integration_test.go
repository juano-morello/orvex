//go:build integration

package engineapi

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/eventstore"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest/roundtest"
	"github.com/juano-morello/orvex/internal/runtime/admission"
	"github.com/juano-morello/orvex/internal/runtime/execution"
	"github.com/juano-morello/orvex/internal/runtime/relay"
	orvexv1 "github.com/juano-morello/orvex/proto/orvex/v1"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

// rig serves SubscribeRound over a migrated database. Nothing notifies its
// wakeups, as if every notification were lost, so a subscriber sees a new
// record only by polling. active counts SubscribeRound handlers still
// running, and reads the transactions the server began.
type rig struct {
	pool   *pgxpool.Pool
	x      *execution.Executor
	seq    *admission.Sequencer
	client orvexv1.EngineServiceClient
	stop   func()
	active atomic.Int64
	reads  atomic.Int64
	// down makes every read fail as a lost connection does.
	down atomic.Bool
}

// rigDB counts the transactions the server begins and fails them while
// the rig is down.
type rigDB struct {
	*pgxpool.Pool
	r *rig
}

func (d rigDB) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	d.r.reads.Add(1)
	if d.r.down.Load() {
		return nil, errors.New("connection refused")
	}
	return d.Pool.BeginTx(ctx, opts)
}

func newRig(t *testing.T) *rig {
	t.Helper()
	pool := pgtest.Pool(t, pgtest.MigratedDatabase(t))
	reg := roundtest.Registry(t)
	seq, err := admission.New(pool, reg, admission.DefaultConfig(), admission.CrashV1Policy())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	r := &rig{pool: pool, x: roundtest.NewExecutor(t, pool, nil, reg), seq: seq, stop: sync.OnceFunc(func() { close(done) })}
	srv := grpc.NewServer(grpc.StreamInterceptor(func(s any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
		r.active.Add(1)
		defer r.active.Add(-1)
		return h(s, ss)
	}))
	orvexv1.RegisterEngineServiceServer(srv, &server{db: rigDB{pool, r}, wakeups: relay.NewWakeups(), done: done, log: slog.New(slog.DiscardHandler)})
	ln := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	t.Cleanup(r.stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return ln.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	r.client = orvexv1.NewEngineServiceClient(conn)
	return r
}

func newID(t *testing.T) [16]byte {
	t.Helper()
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	id[6], id[8] = id[6]&0x0f|0x40, id[8]&0x3f|0x80
	return id
}

func (r *rig) admit(t *testing.T, round [16]byte, cmd v1.Command) error {
	t.Helper()
	_, err := r.seq.Admit(context.Background(), admission.Request{
		RoundID: round, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber, CommandID: newID(t), Command: cmd,
	})
	return err
}

func (r *rig) drain(t *testing.T, round [16]byte, n int) {
	t.Helper()
	for n > 0 {
		got, err := r.x.Drain(context.Background(), execution.Lease{RoundID: round}, n)
		if err != nil || got == 0 {
			t.Fatalf("Drain = %d, %v with %d admissions left", got, err, n)
		}
		n -= got
	}
}

// cashOuts appends n records: cash-outs the rules reject, since no bet
// exists.
func (r *rig) cashOuts(t *testing.T, round [16]byte, n int) {
	t.Helper()
	for range n {
		if err := r.admit(t, round, v1.CashOut{Authority: v1.AuthorityPlayer, Player: "viewer"}); err != nil {
			t.Fatal(err)
		}
	}
	r.drain(t, round, n)
}

// settle appends the round's last three records: StartRound, CloseRound
// once due, and SettleRound.
func (r *rig) settle(t *testing.T, round [16]byte) {
	t.Helper()
	if err := r.admit(t, round, v1.StartRound{Authority: v1.AuthoritySystem}); err != nil {
		t.Fatal(err)
	}
	r.drain(t, round, 1)
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(10 * time.Millisecond) {
		err := r.admit(t, round, v1.CloseRound{Authority: v1.AuthoritySystem})
		if err == nil {
			break
		}
		if !errors.Is(err, admission.ErrClosureNotDue) || time.Now().After(deadline) {
			t.Fatal(err)
		}
	}
	r.drain(t, round, 1)
	if err := r.admit(t, round, v1.SettleRound{Authority: v1.AuthoritySystem}); err != nil {
		t.Fatal(err)
	}
	r.drain(t, round, 1)
}

// round creates a round of n records. A live round holds genesis and
// cash-outs; a final one ends with settle's three records.
func (r *rig) round(t *testing.T, n int, final bool) [16]byte {
	t.Helper()
	round := newID(t)
	cfg := v1.Config{RoundID: round, Seed: roundtest.SeedWithDeadline(t, round, 0, 20*time.Millisecond),
		MinBetCredits: 1, MaxBetCredits: 1_000_000_000, MaxBets: 100}
	if _, err := r.x.CreateRound(context.Background(), execution.NewRound{
		RoundID: round, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber, Config: cfg, CreatedAtUnixMicro: time.Now().UnixMicro(),
	}); err != nil {
		t.Fatal(err)
	}
	if final {
		r.cashOuts(t, round, n-4)
		r.settle(t, round)
	} else {
		r.cashOuts(t, round, n-1)
	}
	return round
}

// committed reads the round's chain records with plain SQL and maps each
// to the frame a subscriber must receive.
func (r *rig) committed(t *testing.T, round [16]byte) []*orvexv1.SubscribeRoundResponse {
	t.Helper()
	rows, err := r.pool.Query(context.Background(), `SELECT sequence, record FROM orvex.chain_records WHERE round_id = $1 ORDER BY sequence`, round)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []*orvexv1.SubscribeRoundResponse
	for rows.Next() {
		var seq int64
		var rec []byte
		if err := rows.Scan(&seq, &rec); err != nil {
			t.Fatal(err)
		}
		f, err := publicRecord(round, eventstore.StoredRecord{Sequence: uint64(seq), Record: rec}) //nolint:gosec // G115: sequence >= 0
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// result is what one subscription delivered: whether the engine sent
// headers, the frames, and the final status (nil for OK).
type result struct {
	header bool
	frames []*orvexv1.SubscribeRoundResponse
	err    error
}

func (res result) check(t *testing.T, want []*orvexv1.SubscribeRoundResponse, code codes.Code) {
	t.Helper()
	if res.header != (code == codes.OK || code == codes.Unavailable) || status.Code(res.err) != code {
		t.Errorf("header %v, status %v; want %v", res.header, res.err, code)
	}
	if len(res.frames) != len(want) {
		t.Fatalf("%d frames, want %d", len(res.frames), len(want))
	}
	for i, f := range res.frames {
		if !proto.Equal(f, want[i]) {
			t.Fatalf("frame %d = %v, want committed record %v", i, f, want[i])
		}
	}
}

// follower reads one subscription in the background.
type follower struct {
	header chan struct{}
	frames atomic.Int64
	done   chan result
	cancel context.CancelFunc
}

func (r *rig) follow(t *testing.T, round [16]byte, after *uint64) *follower {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	sub, err := r.client.SubscribeRound(ctx, &orvexv1.SubscribeRoundRequest{RoundId: round[:], AfterSequence: after})
	if err != nil {
		t.Fatal(err)
	}
	f := &follower{header: make(chan struct{}), done: make(chan result, 1), cancel: cancel}
	go func() {
		md, err := sub.Header()
		res := result{header: err == nil && md != nil}
		if res.header {
			close(f.header)
		}
		for {
			frame, err := sub.Recv()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					res.err = err
				}
				f.done <- res
				return
			}
			res.frames = append(res.frames, frame)
			f.frames.Add(1)
		}
	}()
	return f
}

func (f *follower) wait(t *testing.T) result {
	t.Helper()
	select {
	case res := <-f.done:
		return res
	case <-time.After(30 * time.Second):
		t.Fatalf("subscription did not end; %d frames so far", f.frames.Load())
		return result{}
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
	}
}

// TestSubscribeAcrossBatchBoundaries subscribes to complete rounds whose
// records end one before, at and one after a batch boundary, from every
// cursor that matters.
func TestSubscribeAcrossBatchBoundaries(t *testing.T) {
	r := newRig(t)
	for _, n := range []int{batchLimit - 1, batchLimit, batchLimit + 1} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			round := r.round(t, n, true)
			want := r.committed(t, round)
			if len(want) != n {
				t.Fatalf("%d committed records, want %d", len(want), n)
			}
			last := uint64(n - 1) //nolint:gosec // G115: n > 0
			for _, tc := range []struct {
				name  string
				after *uint64
				from  int
				code  codes.Code
			}{
				{"from genesis", nil, 0, codes.OK},
				{"after genesis", proto.Uint64(0), 1, codes.OK},
				{"before the last record", proto.Uint64(last - 1), n - 1, codes.OK},
				{"at the last record", proto.Uint64(last), n, codes.OutOfRange},
				{"above the head", proto.Uint64(last + 1), n, codes.FailedPrecondition},
			} {
				t.Run(tc.name, func(t *testing.T) {
					r.follow(t, round, tc.after).wait(t).check(t, want[tc.from:], tc.code)
				})
			}
		})
	}
	eventually(t, "subscriptions still running after they ended", func() bool { return r.active.Load() == 0 })
}

// TestSubscribeLiveRoundByPolling follows a live round with every wakeup
// lost: one subscriber from genesis, one at the live head and one that
// never reads. The round grows past a batch boundary and completes.
func TestSubscribeLiveRoundByPolling(t *testing.T) {
	r := newRig(t)
	round := r.round(t, batchLimit, false)
	fromGenesis := r.follow(t, round, nil)
	atHead := r.follow(t, round, proto.Uint64(batchLimit-1))
	stopped, cancel := context.WithCancel(context.Background())
	if _, err := r.client.SubscribeRound(stopped, &orvexv1.SubscribeRoundRequest{RoundId: round[:]}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-atHead.header:
	case <-time.After(15 * time.Second):
		t.Fatal("a subscriber at the live head was not accepted")
	}
	eventually(t, "the genesis subscriber did not reach the head", func() bool { return fromGenesis.frames.Load() == batchLimit })
	eventually(t, "three subscriptions should be running", func() bool { return r.active.Load() == 3 })
	cancel()
	eventually(t, "a cancelled subscriber that never read still runs", func() bool { return r.active.Load() == 2 })

	r.cashOuts(t, round, 2)
	r.settle(t, round)
	want := r.committed(t, round)
	if len(want) != batchLimit+5 {
		t.Fatalf("%d committed records, want %d", len(want), batchLimit+5)
	}
	fromGenesis.wait(t).check(t, want, codes.OK)
	atHead.wait(t).check(t, want[batchLimit:], codes.OK)
	eventually(t, "subscriptions still running after the round completed", func() bool { return r.active.Load() == 0 })
}

// TestSubscribeEndsWhenTheEngineStops ends an idle subscription when Serve
// stops, without the end of the round.
func TestSubscribeEndsWhenTheEngineStops(t *testing.T) {
	r := newRig(t)
	round := r.round(t, 3, false)
	f := r.follow(t, round, nil)
	eventually(t, "the subscriber did not reach the head", func() bool { return f.frames.Load() == 3 })
	r.stop()
	f.wait(t).check(t, r.committed(t, round), codes.Unavailable)
	eventually(t, "a subscription still runs after the engine stopped", func() bool { return r.active.Load() == 0 })
}

// TestSubscribersShareOneRead follows one live round with many
// subscribers. While nothing changes, the database work is the round's
// one poll per interval however many subscribers it has, and every
// subscriber still gets each new record.
func TestSubscribersShareOneRead(t *testing.T) {
	const subscribers = 100
	r := newRig(t)
	round := r.round(t, 3, false)
	followers := make([]*follower, subscribers)
	for i := range followers {
		followers[i] = r.follow(t, round, nil)
	}
	eventually(t, "the subscribers did not reach the head", func() bool {
		for _, f := range followers {
			if f.frames.Load() != 3 {
				return false
			}
		}
		return true
	})

	before := r.reads.Load()
	const idle = 3 * pollInterval
	time.Sleep(idle)
	if reads := r.reads.Load() - before; reads > int64(idle/pollInterval)+2 {
		t.Errorf("%d reads in %v with %d idle subscribers of one round, want one per %v", reads, idle, subscribers, pollInterval)
	}

	r.cashOuts(t, round, 2)
	r.settle(t, round)
	want := r.committed(t, round)
	for _, f := range followers {
		f.wait(t).check(t, want, codes.OK)
	}
	eventually(t, "subscriptions still running after the round completed", func() bool { return r.active.Load() == 0 })
}

// TestSubscribeJudgesTheCursorOnAFreshRead subscribes beside a subscriber
// whose shared read predates the newest records. A cursor at the new head
// is accepted, a cursor above it is refused, and once the round completes
// a cursor at its last record gets OUT_OF_RANGE at once, as if each
// subscriber had read the round itself.
func TestSubscribeJudgesTheCursorOnAFreshRead(t *testing.T) {
	r := newRig(t)
	round := r.round(t, 3, false)
	first := r.follow(t, round, nil)
	eventually(t, "the first subscriber did not reach the head", func() bool { return first.frames.Load() == 3 })

	r.cashOuts(t, round, 1)
	atHead := r.follow(t, round, proto.Uint64(3))
	select {
	case <-atHead.header:
	case res := <-atHead.done:
		t.Fatalf("a subscriber at the new head was refused: %v", res.err)
	case <-time.After(15 * time.Second):
		t.Fatal("a subscriber at the new head was not accepted")
	}
	r.follow(t, round, proto.Uint64(4)).wait(t).check(t, nil, codes.FailedPrecondition)

	r.settle(t, round)
	want := r.committed(t, round)
	last := uint64(len(want) - 1) //nolint:gosec // G115: a round has records
	r.follow(t, round, proto.Uint64(last)).wait(t).check(t, nil, codes.OutOfRange)
	first.wait(t).check(t, want, codes.OK)
	atHead.wait(t).check(t, want[4:], codes.OK)
	eventually(t, "subscriptions still running after the round completed", func() bool { return r.active.Load() == 0 })
}

// TestSubscribeBeyondTheFeedBuffer follows a live round longer than a
// feed's buffer, so the feed has dropped its oldest frames when a second
// subscriber joins from genesis. That subscriber reads the dropped frames
// from the database, then the rest from the feed. Both get the complete
// round, gapless and without repeats.
func TestSubscribeBeyondTheFeedBuffer(t *testing.T) {
	const n = feedFrames + batchLimit + 1
	r := newRig(t)
	round := r.round(t, n, false)
	first := r.follow(t, round, nil)
	eventually(t, "the first subscriber did not reach the head", func() bool { return first.frames.Load() == n })
	late := r.follow(t, round, nil)
	eventually(t, "the late subscriber did not reach the head", func() bool { return late.frames.Load() == n })

	r.settle(t, round)
	want := r.committed(t, round)
	if len(want) != n+3 {
		t.Fatalf("%d committed records, want %d", len(want), n+3)
	}
	first.wait(t).check(t, want, codes.OK)
	late.wait(t).check(t, want, codes.OK)
	eventually(t, "subscriptions still running after the round completed", func() bool { return r.active.Load() == 0 })
}

// TestSubscribeUnknownRound refuses a round that does not exist before any
// header.
func TestSubscribeUnknownRound(t *testing.T) {
	r := newRig(t)
	r.follow(t, newID(t), nil).wait(t).check(t, nil, codes.NotFound)
	eventually(t, "a refused subscription still runs", func() bool { return r.active.Load() == 0 })
}

// TestSubscribeWhileTheDatabaseIsDown ends a following subscription when
// the round's shared read fails, as its own read did, and refuses a new
// one before any header with UNAVAILABLE, which the gateway retries. Once
// the database is back a new subscription resumes.
func TestSubscribeWhileTheDatabaseIsDown(t *testing.T) {
	r := newRig(t)
	round := r.round(t, 3, false)
	f := r.follow(t, round, nil)
	eventually(t, "the subscriber did not reach the head", func() bool { return f.frames.Load() == 3 })
	want := r.committed(t, round)

	r.down.Store(true)
	f.wait(t).check(t, want, codes.Unavailable)
	refused := r.follow(t, round, proto.Uint64(2)).wait(t)
	if refused.header || status.Code(refused.err) != codes.Unavailable || len(refused.frames) != 0 {
		t.Fatalf("subscription while the database is down: header %v, %d frames, status %v; want UNAVAILABLE before any header",
			refused.header, len(refused.frames), refused.err)
	}

	r.down.Store(false)
	resumed := r.follow(t, round, proto.Uint64(1))
	eventually(t, "the resumed subscriber did not reach the head", func() bool { return resumed.frames.Load() == 1 })
	r.settle(t, round)
	resumed.wait(t).check(t, r.committed(t, round)[2:], codes.OK)
	eventually(t, "subscriptions still running after the round completed", func() bool { return r.active.Load() == 0 })
}

// TestSubscribeReconnectsWithoutReads resubscribes one by one at cursors
// up to the head of a followed live round, as a reconnect burst does. The
// feed's newest read accepts them all, so they read nothing.
func TestSubscribeReconnectsWithoutReads(t *testing.T) {
	const subscribers = 100
	r := newRig(t)
	round := r.round(t, 3, false)
	first := r.follow(t, round, nil)
	eventually(t, "the first subscriber did not reach the head", func() bool { return first.frames.Load() == 3 })

	before := r.reads.Load()
	start := time.Now()
	for i := range subscribers {
		f := r.follow(t, round, proto.Uint64(uint64(i%3))) //nolint:gosec // G115: i >= 0
		select {
		case <-f.header:
		case res := <-f.done:
			t.Fatalf("subscriber %d refused: %v", i, res.err)
		case <-time.After(15 * time.Second):
			t.Fatalf("subscriber %d was not accepted", i)
		}
		f.cancel()
	}
	polls := int64(time.Since(start)/pollInterval) + 1
	if reads := r.reads.Load() - before; reads > polls {
		t.Errorf("%d subscribers at or below the head took %d reads, want only the feed's %d polls", subscribers, reads, polls)
	}
	first.cancel()
	eventually(t, "subscriptions still running after they were cancelled", func() bool { return r.active.Load() == 0 })
}
