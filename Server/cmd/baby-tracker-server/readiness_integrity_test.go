package main

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/config"
	"babytracker/server/internal/recovery"
	"babytracker/server/internal/security"
	"babytracker/server/internal/security/localstore"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5"
)

type readinessFixture struct {
	readiness applicationReadiness
	db        *testpg.Fixture
	cfg       config.Config
	stores    localstore.Store
}

func newReadinessFixture(t *testing.T) readinessFixture {
	t.Helper()
	db := testpg.New(t)
	root := t.TempDir()
	origin, _ := url.Parse("https://sync.example.test")
	cfg := config.Config{Mode: config.ModeSelfHosted, PublicOrigin: origin, ServerInstanceID: "123e4567-e89b-42d3-a456-426614174000", RecoveryEpoch: 1, SecurityLedgerDir: filepath.Join(root, "ledger"), LedgerHeadDir: filepath.Join(root, "head")}
	cfg.Limits.MaxCaregivers = 10
	cfg.Limits.MaxInvitations = 10
	cfg.Limits.InvitationTTL = 24 * time.Hour
	cfg.Auth.ActorIDPepper[0] = 1
	cfg.Auth.InvitationKeys.Current = config.DigestKeyConfig{ID: "invitation.current", Secret: [32]byte{1}}
	stores, err := localstore.Open(cfg.SecurityLedgerDir, cfg.LedgerHeadDir)
	if err != nil {
		t.Fatal(err)
	}
	durable, err := recovery.NewVerifiedStore(cfg.LedgerHeadDir, stores.Fence, stores.Ledger, stores.Heads)
	if err != nil {
		t.Fatal(err)
	}
	if err = durable.InitializeAuthorityWithActorIDKey(t.Context(), stores, recovery.State{InstanceID: cfg.ServerInstanceID, Epoch: 1, TrustedHead: security.Head{Generation: 1}}, cfg.Auth.ActorIDPepper); err != nil {
		t.Fatal(err)
	}
	gate, authority, _, err := runtimeServices(cfg, db.Runtime, db.Projector)
	if err != nil {
		t.Fatal(err)
	}
	if err = gate.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	r := applicationReadiness{base: gate, inventory: authority, mode: cfg.Mode}
	if err = r.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	return readinessFixture{r, db, cfg, stores}
}

func TestReadinessWarmCacheStillRejectsDrift(t *testing.T) {
	for _, drift := range []string{"identity", "epoch", "head", "projection", "schema", "role", "outage", "keys"} {
		t.Run(drift, func(t *testing.T) {
			h := newReadinessFixture(t)
			for i := 0; i < 2; i++ {
				if err := h.readiness.Check(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			switch drift {
			case "identity", "epoch":
				files, err := filepath.Glob(filepath.Join(h.cfg.LedgerHeadDir, "*.json"))
				if err != nil {
					t.Fatal(err)
				}
				changed := false
				for _, path := range files {
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					text := string(data)
					if filepath.Base(path) == "identity.json" {
						if drift == "identity" {
							text = strings.ReplaceAll(text, h.cfg.ServerInstanceID, "123e4567-e89b-42d3-a456-426614174001")
						} else {
							text = strings.ReplaceAll(text, `"epoch":1`, `"epoch":2`)
						}
						if err = os.WriteFile(path, []byte(text), 0600); err != nil {
							t.Fatal(err)
						}
						changed = true
					}
				}
				if !changed {
					t.Fatal("recovery identity fixture not found")
				}
			case "head":
				if err := os.WriteFile(filepath.Join(h.cfg.LedgerHeadDir, "head.json"), []byte("{}\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "projection":
				if _, err := h.db.Owner.Exec(t.Context(), "UPDATE security_projection_head SET revision=1,event_hash=decode(repeat('01',32),'hex')"); err != nil {
					t.Fatal(err)
				}
			case "schema":
				if _, err := h.db.Owner.Exec(t.Context(), "DELETE FROM schema_migrations WHERE version=(SELECT max(version) FROM schema_migrations)"); err != nil {
					t.Fatal(err)
				}
			case "role":
				role := h.db.Runtime.Config().ConnConfig.User
				if _, err := h.db.Admin.Exec(t.Context(), "GRANT baby_tracker_projector TO "+pgx.Identifier{role}.Sanitize()); err != nil {
					t.Fatal(err)
				}
			case "outage":
				h.db.Runtime.Close()
			case "keys":
				real := h.readiness.inventory
				h.readiness.inventory = credentialKeyInventoryFunc(func(ctx context.Context) (security.CredentialKeyInventory, error) {
					k, err := real.ReferencedCredentialKeyIDs(ctx)
					k.SessionToken = []string{"required.receipt"}
					return k, err
				})
				h.readiness.sessionKeyIDs = map[string]struct{}{"required.receipt": {}}
				if err := h.readiness.Check(t.Context()); err != nil {
					t.Fatal(err)
				}
				delete(h.readiness.sessionKeyIDs, "required.receipt")
			}
			for i := 0; i < 2; i++ {
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				err := h.readiness.Check(ctx)
				cancel()
				if err == nil {
					t.Fatal("warm readiness accepted drift")
				}
			}
		})
	}
}

func TestSyntheticApplicationReadinessPerformanceProbe(t *testing.T) {
	if os.Getenv("BT_SYNC_PERF_PROBE") != "1" {
		t.Skip("opt-in synthetic application readiness probe")
	}
	h := newReadinessFixture(t)
	elapsed := make([]time.Duration, 30)
	for i := range elapsed {
		start := time.Now()
		if err := h.readiness.Check(t.Context()); err != nil {
			t.Fatal(err)
		}
		elapsed[i] = time.Since(start)
	}
	sort.Slice(elapsed, func(i, j int) bool { return elapsed[i] < elapsed[j] })
	t.Logf("PERF component=application-readiness samples=%d p50_ms=%.3f p95_ms=%.3f", len(elapsed), float64(elapsed[14])/float64(time.Millisecond), float64(elapsed[28])/float64(time.Millisecond))
}
