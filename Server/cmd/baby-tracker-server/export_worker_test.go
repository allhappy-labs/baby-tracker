package main

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/database"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestExportWorkerRequiresIsolatedMaintenanceRole(t *testing.T) {
	fixture := testpg.New(t)
	var maintenanceRole string
	if err := fixture.Maintenance.QueryRow(t.Context(), "SELECT current_user").Scan(&maintenanceRole); err != nil {
		t.Fatal(err)
	}
	if err := verifyMaintenancePool(t.Context(), fixture.Maintenance, maintenanceRole); err != nil {
		t.Fatalf("maintenance role rejected: %v", err)
	}
	if err := verifyMaintenancePool(t.Context(), fixture.Runtime, maintenanceRole); !errors.Is(err, errMaintenanceIsolation) {
		t.Fatalf("runtime credential was accepted as maintenance: %v", err)
	}
	if err := verifyMaintenancePool(t.Context(), fixture.Owner, maintenanceRole); !errors.Is(err, errMaintenanceIsolation) {
		t.Fatalf("owner credential was accepted as maintenance: %v", err)
	}
}

// A maintenance pool that cannot answer is reported as what it is: busy or
// unreachable (no verdict) or a refused credential, never as a login that
// is not isolated; and no error carries the database's own text.
func TestVerifyMaintenancePoolNamesTheFailure(t *testing.T) {
	fixture := testpg.New(t)
	var login string
	if err := fixture.Maintenance.QueryRow(t.Context(), "SELECT current_user").Scan(&login); err != nil {
		t.Fatal(err)
	}
	dsn := fixture.Maintenance.Config().ConnString()
	open := func(t *testing.T, raw string, max int32) *pgxpool.Pool {
		pool, err := database.OpenWithMax(t.Context(), raw, max)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return pool
	}
	t.Run("busy", func(t *testing.T) {
		pool := open(t, dsn, 1)
		held, err := pool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer held.Release()
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		if err := verifyMaintenancePool(ctx, pool, login); !errors.Is(err, errMaintenanceUnavailable) || !errors.Is(err, database.ErrUnavailable) {
			t.Fatalf("busy pool: %v", err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		u, _ := url.Parse(dsn)
		u.Host = "127.0.0.1:1"
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		if err := verifyMaintenancePool(ctx, open(t, u.String(), 2), login); !errors.Is(err, errMaintenanceUnavailable) {
			t.Fatalf("unreachable pool: %v", err)
		}
	})
	t.Run("refused credential", func(t *testing.T) {
		u, _ := url.Parse(dsn)
		u.User = url.UserPassword(login, "wrong-password-secret")
		err := verifyMaintenancePool(t.Context(), open(t, u.String(), 2), login)
		if !errors.Is(err, errMaintenanceAuthentication) || strings.Contains(err.Error(), "wrong-password-secret") || strings.Contains(err.Error(), login) {
			t.Fatalf("refused credential: %v", err)
		}
	})
}

type countingExportWorker struct{ runs atomic.Int32 }

func (w *countingExportWorker) RunOnce(context.Context, int) error {
	w.runs.Add(1)
	return nil
}

// The export worker runs each iteration inside a maintenance worker slot:
// while every slot is taken it waits, leaving the audit's connection free.
func TestExportWorkerRunsInsideAMaintenanceSlot(t *testing.T) {
	pool, err := database.OpenWithMax(t.Context(), "postgres://user@127.0.0.1:1/test", 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	slots, err := database.NewWorkerSlots(pool)
	if err != nil {
		t.Fatal(err)
	}
	taken, err := slots.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	worker := &countingExportWorker{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runExportWorker(ctx, readinessFunc(func(context.Context) error { return nil }), slots, worker, nil)
	}()
	time.Sleep(100 * time.Millisecond)
	if worker.runs.Load() != 0 {
		t.Fatal("the export worker ran without a slot")
	}
	taken()
	deadline := time.Now().Add(2 * time.Second)
	for worker.runs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if worker.runs.Load() == 0 {
		t.Fatal("the export worker never ran once a slot was free")
	}
	cancel()
	<-done
}

// A maintenance check the server answers with a SQLSTATE outside the
// connection classes is a verdict, never "unavailable": only an unreachable
// database reaches no verdict, and no message carries the server's text.
func TestMaintenanceQueryErrorsAreClassified(t *testing.T) {
	pg := func(code string) error {
		return &pgconn.PgError{Code: code, Message: "server text names baby_tracker_export_app"}
	}
	for _, c := range []struct {
		err  error
		want error
	}{
		{pg("28P01"), errMaintenanceAuthentication},
		{pg("28000"), errMaintenanceAuthentication},
		{pg("42501"), errMaintenanceRefused},
		{pg("42704"), errMaintenanceRefused},
		{pg("57P01"), errMaintenanceUnavailable},
		{pg("53300"), errMaintenanceUnavailable},
		{context.DeadlineExceeded, errMaintenanceUnavailable},
	} {
		got := maintenanceQueryError(c.err)
		if !errors.Is(got, c.want) || strings.Contains(got.Error(), "server text") {
			t.Fatalf("%v: %v want %v", c.err, got, c.want)
		}
		if errors.Is(got, errMaintenanceUnavailable) != errors.Is(c.want, errMaintenanceUnavailable) {
			t.Fatalf("%v: unavailable=%v", c.err, errors.Is(got, errMaintenanceUnavailable))
		}
	}
}
