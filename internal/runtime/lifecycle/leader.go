package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// schedulerLockKey is the session advisory lock key of the crash/v1
// scheduler: the first 8 bytes of SHA-256("orvex/engine/v1/scheduler:crash")
// as a big-endian int64.
var schedulerLockKey = func() int64 {
	sum := sha256.Sum256([]byte("orvex/engine/v1/scheduler:crash"))
	return int64(binary.BigEndian.Uint64(sum[:8])) //nolint:gosec // G115: the key is any 64-bit pattern; wrapping is the intent
}()

// Connection bounds of the leadership connection.
const (
	connectTimeout = 10 * time.Second
	pingTimeout    = 5 * time.Second
	closeTimeout   = 5 * time.Second
)

// leadership is the scheduler's advisory lock, held by the session of a
// dedicated connection. The lock lasts exactly as long as the session.
type leadership struct {
	mu   sync.Mutex
	conn *pgx.Conn
}

// tryLead opens a connection and takes the lock without waiting. It returns
// nil and no error when another session holds it.
func tryLead(ctx context.Context, cfg *pgx.ConnConfig) (*leadership, error) {
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(cctx, cfg)
	if err != nil {
		return nil, err
	}
	var held bool
	if err := conn.QueryRow(cctx, `SELECT pg_try_advisory_lock($1)`, schedulerLockKey).Scan(&held); err != nil || !held {
		closeConn(conn)
		return nil, err
	}
	return &leadership{conn: conn}, nil
}

// check reports whether the session, and with it the lock, is still alive.
func (l *leadership) check(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	return l.conn.Ping(cctx)
}

// watch checks the session every interval until ctx ends, and calls lost
// once a check fails.
func (l *leadership) watch(ctx context.Context, every time.Duration, lost func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := l.check(ctx); err != nil {
				lost()
				return
			}
		}
	}
}

// release ends the session, which releases the lock.
func (l *leadership) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	closeConn(l.conn)
}

func closeConn(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	_ = conn.Close(ctx)
}
