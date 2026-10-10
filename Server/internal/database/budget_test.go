package database

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"strings"
	"testing"
	"time"
)

func TestEffectivePoolBudget(t *testing.T) {
	standard := []NamedPoolBudget{{"runtime", 16}, {"projector", 4}, {"maintenance-1", 2}}
	tests := []struct {
		name          string
		pools         []NamedPoolBudget
		max, reserved int32
		bad           bool
	}{
		{"standard", standard, 64, 8, false},
		{"shared", append(append([]NamedPoolBudget{}, standard...), NamedPoolBudget{"maintenance-1", 2}), 24, 2, false},
		{"distinct", append(append([]NamedPoolBudget{}, standard...), NamedPoolBudget{"maintenance-2", 2}), 24, 2, true},
		{"above 56", []NamedPoolBudget{{"runtime", 51}, {"projector", 4}, {"maintenance", 2}}, 64, 8, true},
		{"boundary at server max minus reserved", []NamedPoolBudget{{"runtime", 50}, {"projector", 4}, {"maintenance", 2}}, 64, 8, false},
		{"zero", []NamedPoolBudget{{"runtime", 0}}, 64, 8, true},
		{"negative", []NamedPoolBudget{{"runtime", -1}}, 64, 8, true},
		{"conflicting identity", []NamedPoolBudget{{"runtime", 16}, {"runtime", 17}}, 64, 8, true},
		{"missing identity", []NamedPoolBudget{{"", 16}}, 64, 8, true},
		{"overflow", []NamedPoolBudget{{"runtime", 2147483647}, {"projector", 4}}, 2147483647, 0, true},
		{"invalid reserve", standard, 64, -1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateBudget(tc.pools, tc.max, tc.reserved); (err != nil) != tc.bad {
				t.Fatalf("error=%v want error %v", err, tc.bad)
			}
		})
	}
}
func TestOpenWithMaxPreservesDSNControlsAndLazyOpening(t *testing.T) {
	for _, dsn := range []string{
		"postgres://user:secret@127.0.0.1:1/test?sslmode=disable&pool_max_conns=16&pool_min_conns=1&pool_max_conn_lifetime=2m&application_name=test",
		"host=127.0.0.1 port=1 user=user password='secret' dbname=test sslmode=disable pool_max_conns=16 pool_min_conns=1 pool_max_conn_lifetime=2m application_name=test",
		"postgres://user:secret@127.0.0.1:1/test?sslmode=disable",
	} {
		t.Run(dsn[:8], func(t *testing.T) {
			pool, err := OpenWithMax(context.Background(), dsn, 16)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			cfg := pool.Config()
			if cfg.MaxConns != 16 {
				t.Fatalf("max=%d", cfg.MaxConns)
			}
			if strings.Contains(dsn, "pool_min_conns") && (cfg.MinConns != 1 || cfg.MaxConnLifetime != 2*time.Minute || cfg.ConnConfig.RuntimeParams["application_name"] != "test") {
				t.Fatal("supported controls discarded")
			}
		})
	}
	for _, dsn := range []string{"postgres://user:secret@localhost/test?pool_max_conns=17", "host=localhost password=secret pool_max_conns=17", "postgres://user:secret@localhost/test?pool_max_conns=invalid", "postgres://user:secret@localhost/test?pool_min_conns=17", "postgres://user:secret@localhost/test?pool_min_idle_conns=17"} {
		pool, err := OpenWithMax(context.Background(), dsn, 16)
		if pool != nil {
			pool.Close()
		}
		if !errors.Is(err, ErrOpen) || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe conflict err=%v", err)
		}
	}
	if _, err := OpenWithMax(context.Background(), "postgres://localhost/test", 0); !errors.Is(err, ErrOpen) {
		t.Fatal("zero max accepted")
	}
	legacy, err := Open(context.Background(), "postgres://localhost/test?pool_max_conns=7&pool_min_conns=2")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	parsed, _ := pgxpool.ParseConfig("postgres://localhost/test?pool_max_conns=7&pool_min_conns=2")
	if legacy.Config().MaxConns != parsed.MaxConns || legacy.Config().MinConns != 2 {
		t.Fatal("legacy settings changed")
	}
}
