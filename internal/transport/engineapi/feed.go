package engineapi

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/juano-morello/orvex/internal/game"
	"github.com/juano-morello/orvex/internal/persistence/eventstore"
	orvexv1 "github.com/juano-morello/orvex/proto/orvex/v1"
)

// feedFrames bounds a feed's buffer. A crash/v1 round of 100 bets fits.
const feedFrames = 4 * batchLimit

// A feed is the shared tail of one round. While a round has subscribers,
// one reader reads its records from the database, a batch at a time after
// each wakeup or poll, and maps each record to its public frame once. Each
// subscriber sends from the feed's buffer at its own pace, so the
// live tail is read once per round, and a
// subscriber that stops reading blocks only its own stream. The buffer
// keeps the newest feedFrames frames; a subscriber whose cursor is older
// replays shared historical pages until it reaches the buffer.
type feed struct {
	round [16]byte
	// history pages contain immutable public frames. The server-wide historyRead
	// token protects this cache, including the fill on a miss.
	history [8]historyPage
	// kick asks the reader to start a read now.
	kick   chan struct{}
	cancel context.CancelFunc
	// refs counts the subscribers; server.feedsMu guards it.
	refs int

	mu sync.Mutex
	// frames are the public frames from sequence base on, gapless.
	base   uint64
	frames []*orvexv1.SubscribeRoundResponse
	// The round as of the last read that succeeded.
	game  game.ID
	rules game.RulesVersion
	head  uint64
	final bool
	// err is the last read's error, nil once a read succeeds. bad is the
	// error of a record with no public frame; the buffer ends before it.
	err, bad error
	// started counts the reads begun; done is the number of the last read
	// that finished.
	started, done uint64
	// changed is closed and replaced whenever a read finishes.
	changed chan struct{}
}

// joinFeed returns the feed of round, starting it at sequence next if the
// round has none. Pair it with leaveFeed.
func (s *server) joinFeed(round [16]byte, next uint64) *feed {
	s.feedsMu.Lock()
	defer s.feedsMu.Unlock()
	f := s.feeds[round]
	if f == nil {
		ctx, cancel := context.WithCancel(context.Background())
		f = &feed{round: round, kick: make(chan struct{}, 1), cancel: cancel, base: next, changed: make(chan struct{})}
		if s.feeds == nil {
			s.feeds = make(map[[16]byte]*feed)
			s.historyRead = make(chan struct{}, 1)
		}
		s.feeds[round] = f
		s.readers.Go(func() { s.readFeed(ctx, f) })
	}
	f.refs++
	return f
}

// leaveFeed stops the feed's reader after its last subscriber leaves.
func (s *server) leaveFeed(f *feed) {
	s.feedsMu.Lock()
	defer s.feedsMu.Unlock()
	if f.refs--; f.refs == 0 {
		delete(s.feeds, f.round)
		f.cancel()
	}
}

// readFeed is the feed's reader. It stops when the feed is complete, when
// the last subscriber leaves or when Serve stops.
func (s *server) readFeed(ctx context.Context, f *feed) {
	poll := time.NewTicker(pollInterval)
	defer poll.Stop()
	for {
		// Wait before the read: a commit during the read still wakes us.
		wake, release := s.wakeups.Wait(f.round)
		from := f.begin()
		tail, err := eventstore.ReadTail(ctx, s.db, f.round, from, batchLimit)
		more, complete := f.publish(tail, err, s.log)
		if !more && !complete {
			select {
			case <-wake:
			case <-poll.C:
			case <-f.kick:
			case <-ctx.Done():
			case <-s.done:
			}
		}
		release()
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		default:
		}
		if complete {
			return
		}
	}
}

// begin counts a read that starts now and returns the sequence it reads
// from.
func (f *feed) begin() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started++
	return f.base + uint64(len(f.frames))
}

// publish records the outcome of the read begin counted. more reports a
// full batch, so the reader should read again at once; complete reports
// that no record of the round remains to read.
func (f *feed) publish(t eventstore.Tail, err error, log *slog.Logger) (more, complete bool) {
	var frames []*orvexv1.SubscribeRoundResponse
	var bad error
	if err == nil {
		for _, r := range t.Records {
			frame, mapErr := publicRecord(f.round, r)
			if mapErr != nil {
				log.Error("engineapi: map record", "error", mapErr.Error())
				bad = mapErr
				break
			}
			frames = append(frames, frame)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.done, f.err = f.started, err
	close(f.changed)
	f.changed = make(chan struct{})
	if err != nil {
		return false, false
	}
	f.game, f.rules, f.head, f.final, f.bad = t.Game, t.RulesVersion, t.Head, t.Final, bad
	f.frames = append(f.frames, frames...)
	if drop := len(f.frames) - feedFrames; drop > 0 {
		f.frames = slices.Clone(f.frames[drop:])
		f.base += uint64(drop) //nolint:gosec // G115: drop > 0
	}
	if len(f.frames) == 0 && f.base > f.head+1 {
		// A first cursor above the head started the feed; nothing is lost
		// by starting at the head.
		f.base = f.head + 1
	}
	end := f.base + uint64(len(f.frames))
	return bad == nil && len(t.Records) == batchLimit, bad == nil && f.final && end == f.head+1
}

// request asks the reader for a read that begins after the call and
// returns that read's number.
func (f *feed) request() uint64 {
	f.mu.Lock()
	n := f.started + 1
	f.mu.Unlock()
	select {
	case f.kick <- struct{}{}:
	default:
	}
	return n
}

// roundState is a feed's view of its round for a new subscriber.
type roundState struct {
	game  game.ID
	rules game.RulesVersion
	head  uint64
	final bool
	err   error
}

// cached returns the round as of the newest read, and whether that read
// succeeded.
func (f *feed) cached() (roundState, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return roundState{game: f.game, rules: f.rules, head: f.head, final: f.final}, f.done > 0 && f.err == nil
}

// state returns the round as of read n or a later one, or false and a
// channel closed by the next read. A complete round's state is final, so
// it needs no new read.
func (f *feed) state(n uint64) (roundState, bool, <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done < n && !f.final {
		return roundState{}, false, f.changed
	}
	st := roundState{game: f.game, rules: f.rules, head: f.head, final: f.final}
	if !f.final {
		st.err = f.err
	}
	return st, true, nil
}

// view is what a subscriber whose next sequence is next can do now.
type view struct {
	// frames are buffered frames from next on.
	frames []*orvexv1.SubscribeRoundResponse
	// behind: next is older than the buffer.
	behind bool
	// complete: no record follows next.
	complete bool
	err      error
	// changed is closed by the next read.
	changed <-chan struct{}
}

func (f *feed) view(next uint64) view {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := uint64(len(f.frames))
	switch end := f.base + n; {
	case next < f.base:
		return view{behind: true}
	case next < end:
		i := next - f.base
		return view{frames: f.frames[i:min(i+batchLimit, n)]}
	case f.final && next == f.head+1:
		return view{complete: true}
	case f.bad != nil:
		return view{err: f.bad}
	case f.err != nil:
		return view{err: f.err}
	}
	return view{changed: f.changed}
}

// historyPage covers up to batchLimit records at an aligned sequence. A nil
// frame marks a record that could not be mapped. Readers stop at that record.
type historyPage struct {
	start  uint64
	frames []*orvexv1.SubscribeRoundResponse
}

// historyFrames shares an aligned page across subscribers, including those
// that reconnect at different offsets in the same page. Hold the token only
// while looking up or filling the cache. Network sends happen after release.
func (s *server) historyFrames(ctx context.Context, f *feed, next uint64) ([]*orvexv1.SubscribeRoundResponse, error) {
	select {
	case s.historyRead <- struct{}{}:
		defer func() { <-s.historyRead }()
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	case <-s.done:
		return nil, status.Error(codes.Unavailable, "engine shutting down")
	}
	pageNumber := next / batchLimit
	start := pageNumber * batchLimit
	offset := next - start
	slot := &f.history[pageNumber%uint64(len(f.history))]
	if slot.start != start || offset >= uint64(len(slot.frames)) {
		tail, err := eventstore.ReadTail(ctx, s.db, f.round, start, batchLimit)
		if err != nil {
			return nil, s.subscribeError(ctx, err)
		}
		if offset >= uint64(len(tail.Records)) {
			return nil, s.subscribeError(ctx, eventstore.ErrCorrupt)
		}
		frames := make([]*orvexv1.SubscribeRoundResponse, len(tail.Records))
		for i, r := range tail.Records {
			frame, err := publicRecord(f.round, r)
			if err != nil {
				s.log.Error("engineapi: map historical record", "error", err.Error())
				continue
			}
			frames[i] = frame
		}
		*slot = historyPage{start: start, frames: frames}
	}
	return slot.frames[offset:], nil
}
