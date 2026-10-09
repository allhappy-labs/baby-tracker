package database

import (
	"context"
	"strings"
	"testing"
	"time"
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
