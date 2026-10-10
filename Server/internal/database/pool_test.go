package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestOpenAndPingReturnsSafeStableErrors(t *testing.T) {
	const secret = "task3-open-secret-63d209e3"
	tests := []struct {
		name     string
		url      string
		category string
	}{
		{
			name:     "malformed URL",
			url:      "postgres://user:" + secret + "@127.0.0.1:5432/babytracker?application_name=" + secret + "%zz",
			category: "database_open_failed",
		},
		{
			name:     "unavailable database with secret parameter",
			url:      "postgres://user:" + secret + "@127.0.0.1:1/babytracker?application_name=" + secret,
			category: "database_unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			pool, err := OpenAndPing(ctx, tt.url)
			if pool != nil {
				pool.Close()
				t.Fatal("Open returned a pool for an invalid or unavailable database")
			}
			if err == nil {
				t.Fatal("Open returned nil error")
			}
			if got := err.Error(); got != tt.category {
				t.Errorf("Open error = %q, want stable category %q", got, tt.category)
			}
			for _, forbidden := range []string{secret, tt.url} {
				if strings.Contains(err.Error(), forbidden) {
					t.Errorf("Open error leaked %q: %q", forbidden, err)
				}
			}
		})
	}
}

func TestOpenReadOnlyWithMaxForcesReadOnlySessions(t *testing.T) {
	for _, dsn := range []string{
		"postgres://user:secret@127.0.0.1:1/test?sslmode=disable",
		"postgres://user:secret@127.0.0.1:1/test?sslmode=disable&default_transaction_read_only=on",
		"host=127.0.0.1 port=1 user=user password=secret dbname=test pool_max_conns=4",
	} {
		pool, err := OpenReadOnlyWithMax(context.Background(), dsn, 4)
		if err != nil {
			t.Fatalf("%v", err)
		}
		if !ReadOnlyByDefault(pool) || pool.Config().MaxConns != 4 {
			t.Fatalf("read-only=%v max=%d", ReadOnlyByDefault(pool), pool.Config().MaxConns)
		}
		pool.Close()
	}
	for _, dsn := range []string{
		"postgres://user:secret@127.0.0.1:1/test?default_transaction_read_only=off",
		"postgres://user:secret@127.0.0.1:1/test?default_transaction_read_only=false",
		"postgres://user:secret@127.0.0.1:1/test?pool_max_conns=5",
	} {
		pool, err := OpenReadOnlyWithMax(context.Background(), dsn, 4)
		if pool != nil {
			pool.Close()
		}
		if !errors.Is(err, ErrOpen) || strings.Contains(err.Error(), "secret") {
			t.Fatalf("conflicting read-only DSN accepted: %v", err)
		}
	}
	writable, err := OpenWithMax(context.Background(), "postgres://user@127.0.0.1:1/test", 4)
	if err != nil {
		t.Fatal(err)
	}
	defer writable.Close()
	if ReadOnlyByDefault(writable) || ReadOnlyByDefault(nil) {
		t.Fatal("ordinary pool reported read-only")
	}
}

// Every pool skips the acquire ping until a connection has idled for a
// minute, pins the per-connection statement cache (a DSN choosing another
// exec mode, or disabling the cache, cannot open), and the background pool
// is two connections on the runtime DSN whatever ceiling that DSN carries.
func TestPoolsSkipShortIdlePingsAndCacheStatements(t *testing.T) {
	const dsn = "postgres://user:secret@127.0.0.1:1/test?sslmode=disable"
	open := map[string]func(string) (*pgxpool.Pool, error){
		"open":       func(url string) (*pgxpool.Pool, error) { return Open(t.Context(), url) },
		"with max":   func(url string) (*pgxpool.Pool, error) { return OpenWithMax(t.Context(), url, 4) },
		"read-only":  func(url string) (*pgxpool.Pool, error) { return OpenReadOnlyWithMax(t.Context(), url, 4) },
		"background": func(url string) (*pgxpool.Pool, error) { return OpenBackground(t.Context(), url) },
		"readiness":  func(url string) (*pgxpool.Pool, error) { return OpenReadiness(t.Context(), url) },
	}
	for name, opener := range open {
		t.Run(name, func(t *testing.T) {
			pool, err := opener(dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			cfg := pool.Config()
			if cfg.ShouldPing == nil {
				t.Fatal("no ShouldPing: pgx pings every connection idle for a second")
			}
			if cfg.ShouldPing(t.Context(), pgxpool.ShouldPingParams{IdleDuration: 59 * time.Second}) {
				t.Fatal("pinged a connection idle for 59s")
			}
			if cfg.ShouldPing(t.Context(), pgxpool.ShouldPingParams{IdleDuration: time.Minute}) {
				t.Fatal("pinged a connection idle for exactly a minute")
			}
			if !cfg.ShouldPing(t.Context(), pgxpool.ShouldPingParams{IdleDuration: 61 * time.Second}) {
				t.Fatal("did not ping a connection idle for 61s")
			}
			if cfg.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeCacheStatement || cfg.ConnConfig.StatementCacheCapacity <= 0 {
				t.Fatalf("exec mode %v, statement cache %d", cfg.ConnConfig.DefaultQueryExecMode, cfg.ConnConfig.StatementCacheCapacity)
			}
			for _, suffix := range []string{"&default_query_exec_mode=simple_protocol", "&default_query_exec_mode=exec",
				"&default_query_exec_mode=cache_describe", "&statement_cache_capacity=0"} {
				refused, err := opener(dsn + suffix)
				if refused != nil {
					refused.Close()
				}
				if !errors.Is(err, ErrOpen) || strings.Contains(err.Error(), "secret") {
					t.Fatalf("%s accepted: %v", suffix, err)
				}
			}
			explicit, err := opener(dsn + "&default_query_exec_mode=cache_statement")
			if err != nil {
				t.Fatalf("explicit statement cache refused: %v", err)
			}
			explicit.Close()
		})
	}
	// The readiness and background pools are one connection each on the
	// runtime DSN (whatever ceiling it carries): readiness owns its own
	// connection, so the background jobs can never starve its probes.
	if ReadinessConns != 1 || BackgroundConns != 1 {
		t.Fatalf("readiness %d background %d, want 1 and 1 (2 in all)", ReadinessConns, BackgroundConns)
	}
	for name, opener := range map[string]func(context.Context, string) (*pgxpool.Pool, error){"readiness": OpenReadiness, "background": OpenBackground} {
		for _, url := range []string{dsn, dsn + "&pool_max_conns=16&pool_min_conns=4&pool_min_idle_conns=2"} {
			pool, err := opener(t.Context(), url)
			if err != nil {
				t.Fatal(err)
			}
			cfg := pool.Config()
			if cfg.MaxConns != 1 || cfg.MinConns != 0 || cfg.MinIdleConns != 0 || ReadOnlyByDefault(pool) {
				t.Fatalf("%s pool max %d min %d min idle %d", name, cfg.MaxConns, cfg.MinConns, cfg.MinIdleConns)
			}
			if cfg.ConnConfig.ConnectTimeout != 2*time.Second {
				t.Fatalf("%s connect timeout %v", name, cfg.ConnConfig.ConnectTimeout)
			}
			pool.Close()
		}
		// A DSN's own connect_timeout is kept.
		pool, err := opener(t.Context(), dsn+"&connect_timeout=7")
		if err != nil {
			t.Fatal(err)
		}
		if got := pool.Config().ConnConfig.ConnectTimeout; got != 7*time.Second {
			t.Fatalf("%s: DSN connect timeout replaced: %v", name, got)
		}
		pool.Close()
	}
}

// Maintenance workers take a slot before a connection, and there is one
// slot fewer than the pool has connections, so the readiness audit always
// finds one. A pool with fewer than two connections has no room for both.
func TestMaintenanceWorkerSlotsReserveTheAuditConnection(t *testing.T) {
	open := func(max int32) *pgxpool.Pool {
		pool, err := OpenWithMax(t.Context(), "postgres://user@127.0.0.1:1/test", max)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return pool
	}
	if slots, err := NewWorkerSlots(open(1)); err == nil || slots != nil {
		t.Fatal("a one-connection maintenance pool accepted")
	}
	if slots, err := NewWorkerSlots(nil); err == nil || slots != nil {
		t.Fatal("a nil maintenance pool accepted")
	}
	slots, err := NewWorkerSlots(open(3))
	if err != nil {
		t.Fatal(err)
	}
	if slots.Size() != 2 {
		t.Fatalf("slots %d, want pool size 3 - 1", slots.Size())
	}
	first, err := slots.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	second, err := slots.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	waiting, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := slots.Acquire(waiting); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a third worker got a slot: %v", err)
	}
	first()
	first() // releasing twice frees one slot only
	third, err := slots.Acquire(t.Context())
	if err != nil {
		t.Fatalf("a released slot was not reusable: %v", err)
	}
	blocked, cancelBlocked := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancelBlocked()
	if _, err := slots.Acquire(blocked); err == nil {
		t.Fatal("a double release freed two slots")
	}
	second()
	third()
	ran := 0
	if err := slots.Do(t.Context(), func(context.Context) error { ran++; return nil }); err != nil || ran != 1 {
		t.Fatalf("Do: %v ran %d", err, ran)
	}
	want := errors.New("worker failed")
	if err := slots.Do(t.Context(), func(context.Context) error { return want }); !errors.Is(err, want) {
		t.Fatalf("Do lost the worker's error: %v", err)
	}
	// Do released its slots: both are free again.
	a, errA := slots.Acquire(t.Context())
	b, errB := slots.Acquire(t.Context())
	if errA != nil || errB != nil {
		t.Fatalf("Do kept a slot: %v %v", errA, errB)
	}
	a()
	b()
}

// A cancelled context never takes a slot, even when one is free (a select
// over a free slot and a done context would pick either at random).
func TestWorkerSlotsRefuseACancelledContext(t *testing.T) {
	pool, err := OpenWithMax(t.Context(), "postgres://user@127.0.0.1:1/test", 3)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	slots, err := NewWorkerSlots(pool)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	ran := false
	for range 200 {
		if release, err := slots.Acquire(cancelled); !errors.Is(err, context.Canceled) || release != nil {
			t.Fatalf("a cancelled context took a slot: %v", err)
		}
		if err := slots.Do(cancelled, func(context.Context) error { ran = true; return nil }); !errors.Is(err, context.Canceled) {
			t.Fatalf("Do with a cancelled context: %v", err)
		}
	}
	if ran {
		t.Fatal("work ran with a cancelled context")
	}
	if len(slots.slots) != 0 {
		t.Fatalf("%d slots taken by cancelled contexts", len(slots.slots))
	}
}
