package testkit

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Wire counts what one pooled connection sends: network round trips (a
// client write after the client last read server bytes starts a new one)
// and statements (pgx query tracing; batch statements included). It is the
// authority round-trip counter (authority/roundtrip_test.go) for every
// package's tests.
type Wire struct {
	mu         sync.Mutex
	read       bool
	trips      int
	statements []string
}

func (w *Wire) wrote() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.read {
		w.trips++
		w.read = false
	}
}

func (w *Wire) got() {
	w.mu.Lock()
	w.read = true
	w.mu.Unlock()
}

// Reset zeroes the counts.
func (w *Wire) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.trips, w.statements = 0, nil
}

// Trips is the number of round trips since the last Reset.
func (w *Wire) Trips() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.trips
}

// Statements is the SQL sent since the last Reset, in order.
func (w *Wire) Statements() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.statements...)
}

func (w *Wire) record(sql string) {
	w.mu.Lock()
	w.statements = append(w.statements, sql)
	w.mu.Unlock()
}

func (w *Wire) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	w.record(data.SQL)
	return ctx
}
func (w *Wire) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func (w *Wire) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}
func (w *Wire) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	w.record(data.SQL)
}
func (w *Wire) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

type countedConn struct {
	net.Conn
	w *Wire
}

func (c countedConn) Write(p []byte) (int, error) {
	c.w.wrote()
	return c.Conn.Write(p)
}

func (c countedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.w.got()
	}
	return n, err
}

// CountedPool is a one-connection copy of base whose connection is counted.
// It never pings on acquire, so only the code under test reaches the wire;
// the connection's statement cache persists across transactions, so a test
// warms it once before it measures.
func CountedPool(t *testing.T, base *pgxpool.Pool) (*pgxpool.Pool, *Wire) {
	t.Helper()
	w := &Wire{}
	config := base.Config().Copy()
	config.MaxConns, config.MinConns = 1, 0
	config.ShouldPing = func(context.Context, pgxpool.ShouldPingParams) bool { return false }
	config.ConnConfig.Tracer = w
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	config.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return countedConn{Conn: conn, w: w}, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, w
}
