package authority_test

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// wire counts what one pooled connection sends: network round trips (a
// client write after the client last read server bytes starts a new one),
// statements (pgx query tracing; batch statements included) and batches.
type wire struct {
	mu         sync.Mutex
	read       bool
	trips      int
	statements []string
	batches    int
}

func (w *wire) wrote() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.read {
		w.trips++
		w.read = false
	}
}

func (w *wire) got() {
	w.mu.Lock()
	w.read = true
	w.mu.Unlock()
}

// snapshot returns the counts since the last reset.
func (w *wire) snapshot() (trips int, statements []string, batches int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.trips, append([]string(nil), w.statements...), w.batches
}

func (w *wire) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.trips, w.statements, w.batches = 0, nil, 0
}

func (w *wire) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	w.mu.Lock()
	w.statements = append(w.statements, data.SQL)
	w.mu.Unlock()
	return ctx
}
func (w *wire) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func (w *wire) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	w.mu.Lock()
	w.batches++
	w.mu.Unlock()
	return ctx
}
func (w *wire) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	w.mu.Lock()
	w.statements = append(w.statements, data.SQL)
	w.mu.Unlock()
}
func (w *wire) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

type countedConn struct {
	net.Conn
	w *wire
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

// countedPool is a one-connection copy of the fixture's runtime pool whose
// connection is counted. It never pings on acquire, so only the code under
// test reaches the wire. change adjusts the config (a read-only default).
func countedPool(t *testing.T, f *authoritytest.Fixture, change func(*pgxpool.Config)) (*pgxpool.Pool, *wire) {
	t.Helper()
	w := &wire{}
	config := f.Pool.Config().Copy()
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
	if change != nil {
		change(config)
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, w
}

// countedService is the fixture's Service over a counted pool.
func countedService(t *testing.T, f *authoritytest.Fixture) (*authority.Service, *wire) {
	t.Helper()
	pool, w := countedPool(t, f, nil)
	return serviceOn(t, f, pool, nil), w
}

// serviceOn is a Service over pool with the fixture's options, changed by change.
func serviceOn(t *testing.T, f *authoritytest.Fixture, pool *pgxpool.Pool, change func(*authority.Options)) *authority.Service {
	t.Helper()
	o := authority.Options{Pool: pool, Now: func() time.Time { return f.Now }, InstanceID: f.InstanceID, RecoveryEpoch: authoritytest.RecoveryEpoch,
		TokenKeys: f.TokenKeys, MaxCaregivers: 10, MaxInvitations: 10, Allowed: authoritytest.Allowed, EntitlementExempt: authoritytest.EntitlementExempt}
	if change != nil {
		change(&o)
	}
	s, err := authority.New(o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func statementsMatching(statements []string, fragment string) int {
	n := 0
	for _, s := range statements {
		if strings.Contains(s, fragment) {
			n++
		}
	}
	return n
}
