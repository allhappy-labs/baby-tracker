package testkit

import (
	"babytracker/server/internal/database"
	"babytracker/server/internal/migrate"
	"babytracker/server/migrations"
	"os"
	"sort"
	"testing"
	"time"
)

// These probes isolate database subphases. End-to-end applicationReadiness.Check
// is measured by TestSyntheticApplicationReadinessPerformanceProbe in cmd/baby-tracker-server.
func TestSyntheticReadinessPerformanceProbe(t *testing.T) {
	if os.Getenv("BT_SYNC_PERF_PROBE") != "1" {
		t.Skip("opt-in synthetic performance probe")
	}
	db := NewPostgres(t)
	compatibility, err := migrate.EmbeddedCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	runner := migrate.New(migrations.Files)
	for _, mode := range []string{"schema-compatibility", "runtime-role", "database-readiness"} {
		elapsed := make([]time.Duration, 15)
		for i := range elapsed {
			start := time.Now()
			ctx := t.Context()
			switch mode {
			case "schema-compatibility":
				_, err = runner.CompatibleVersion(ctx, db.Runtime, compatibility)
			case "runtime-role":
				err = database.VerifyRuntimeRole(ctx, db.Runtime, compatibility.Max)
			case "database-readiness":
				err = db.Runtime.Ping(ctx)
				if err == nil {
					var version int64
					version, err = runner.CompatibleVersion(ctx, db.Runtime, compatibility)
					if err == nil {
						err = database.VerifyRuntimeRole(ctx, db.Runtime, version)
					}
					if err == nil {
						_, err = runner.CompatibleVersion(ctx, db.Runtime, compatibility)
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			elapsed[i] = time.Since(start)
		}
		sort.Slice(elapsed, func(i, j int) bool { return elapsed[i] < elapsed[j] })
		t.Logf("PERF component=%s schema=%d samples=15 p50_ms=%.3f p95_ms=%.3f", mode, compatibility.Max, float64(elapsed[7])/float64(time.Millisecond), float64(elapsed[14])/float64(time.Millisecond))
	}
}
