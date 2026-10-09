package recovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/migrate"
	"babytracker/server/internal/security"
	"babytracker/server/internal/security/localstore"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testpg"
	"babytracker/server/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type realSchema struct {
	pool          *pgxpool.Pool
	run           *migrate.Runner
	compatibility migrate.Compatibility
}

type reservedSessionProjection struct {
	security.Projection
	owner *pgxpool.Pool
}

func (projection reservedSessionProjection) Apply(ctx context.Context, candidate security.Candidate) error {
	var event security.Event
	if err := json.Unmarshal(candidate.EventJSON, &event); err != nil {
		return err
	}
	if event.Kind != "session_granted" {
		return projection.Projection.Apply(ctx, candidate)
	}
	return applyFutureProjection(ctx, projection.owner, candidate, nil)
}

func TestPostgresMirrorRejectsUnsafeIntegerEpoch(t *testing.T) {
	database := testpg.New(t)
	mirror, err := NewPostgresMirror(database.Runtime, database.Projector)
	if err != nil {
		t.Fatal(err)
	}
	state := State{InstanceID: "123e4567-e89b-42d3-a456-426614174000", Epoch: 9007199254740992, TrustedHead: security.Head{Generation: 1}}
	if err := mirror.Update(t.Context(), state); err == nil {
		t.Fatal("mirror accepted unsafe integer epoch")
	}
}

func TestPostgresMirrorRejectsRuntimeAndProjectorOnDifferentDatabases(t *testing.T) {
	runtimeDatabase := testpg.New(t)
	projectorDatabase := testpg.New(t)
	root := t.TempDir()
	stores, err := localstore.Open(root+"/ledger", root+"/heads")
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.InitializeEmpty(t.Context()); err != nil {
		t.Fatal(err)
	}
	instance := "123e4567-e89b-42d3-a456-426614174000"
	durable, err := NewStore(root+"/heads", stores.Fence)
	if err != nil {
		t.Fatal(err)
	}
	state := State{InstanceID: instance, Epoch: 1, TrustedHead: security.Head{Generation: 1}}
	if err := durable.Initialize(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	bindGateTestActorIDKey(t, durable, instance)
	authority, err := security.New(security.Options{Ledger: stores.Ledger, Heads: stores.Heads, Projection: security.NewProjector(projectorDatabase.Projector), Fence: stores.Fence,
		Now: time.Now, Runtime: runtimeDatabase.Runtime,
		Decide: func(context.Context, access.Principal, string, string) (access.Grant, error) {
			return access.Grant{}, security.ErrUnavailable
		},
		Epoch: func(ctx context.Context) (uint64, error) {
			current, err := durable.LoadUnderFence(ctx)
			return current.Epoch, err
		}})
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := NewPostgresMirror(runtimeDatabase.Runtime, projectorDatabase.Projector)
	if err != nil {
		t.Fatal(err)
	}
	compatibility, err := migrate.EmbeddedCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	gate, err := NewGate(Options{Store: durable, Security: authority, Schema: realSchema{runtimeDatabase.Runtime, migrate.New(migrations.Files), compatibility},
		Mirror: mirror, ExpectedInstanceID: instance, ExpectedEpoch: 1, ActorIDKey: gateTestActorIDKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.Reconcile(t.Context()); !errors.Is(err, ErrQuarantine) {
		t.Fatalf("cross-database mirror reconciled: %v", err)
	}
	if _, exists, err := mirror.Load(t.Context()); err != nil || exists {
		t.Fatalf("runtime database unexpectedly observed projector mirror exists=%v err=%v", exists, err)
	}
	var count int
	if err := projectorDatabase.Owner.QueryRow(t.Context(), "SELECT count(*) FROM server_state").Scan(&count); err != nil || count != 1 {
		t.Fatalf("projector database mirror count=%d err=%v", count, err)
	}
}

func TestDatabaseAheadPostgresMirrorRemainsQuarantined(t *testing.T) {
	database := testpg.New(t)
	root := t.TempDir()
	stores, err := localstore.Open(root+"/ledger", root+"/heads")
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.InitializeEmpty(t.Context()); err != nil {
		t.Fatal(err)
	}
	instance := "123e4567-e89b-42d3-a456-426614174000"
	durable, err := NewStore(root+"/heads", stores.Fence)
	if err != nil {
		t.Fatal(err)
	}
	current := State{InstanceID: instance, Epoch: 1, TrustedHead: security.Head{Generation: 1}}
	if err := durable.Initialize(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	bindGateTestActorIDKey(t, durable, instance)
	projector := security.NewProjector(database.Projector)
	authority, err := security.New(security.Options{Ledger: stores.Ledger, Heads: stores.Heads, Projection: projector, Fence: stores.Fence,
		Now: time.Now, Runtime: database.Runtime,
		Decide: func(context.Context, access.Principal, string, string) (access.Grant, error) {
			return access.Grant{}, security.ErrUnavailable
		},
		Epoch: func(ctx context.Context) (uint64, error) {
			state, err := durable.LoadUnderFence(ctx)
			return state.Epoch, err
		}})
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := NewPostgresMirror(database.Runtime, database.Projector)
	if err != nil {
		t.Fatal(err)
	}
	ahead := current
	ahead.TrustedHead = security.Head{Generation: 1, Revision: 1, EventHash: sha256.Sum256([]byte("unexplained database head"))}
	if err := mirror.Update(t.Context(), ahead); err != nil {
		t.Fatal(err)
	}
	compatibility, err := migrate.EmbeddedCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	gate, err := NewGate(Options{Store: durable, Security: authority, Schema: realSchema{database.Runtime, migrate.New(migrations.Files), compatibility},
		Mirror: mirror, ExpectedInstanceID: instance, ExpectedEpoch: 1, ActorIDKey: gateTestActorIDKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.Reconcile(t.Context()); !errors.Is(err, ErrQuarantine) {
		t.Fatalf("database-ahead PostgreSQL mirror reconciled: %v", err)
	}
	persisted, exists, err := mirror.Load(t.Context())
	if err != nil || !exists || persisted != ahead {
		t.Fatalf("database-ahead row changed: state=%+v exists=%v err=%v", persisted, exists, err)
	}
	if projected, err := projector.Head(t.Context()); err != nil || projected != current.TrustedHead {
		t.Fatalf("database-ahead reconcile mutated projection: head=%+v err=%v", projected, err)
	}
}

func (schema realSchema) Check(ctx context.Context) error {
	return schema.run.CheckCompatibility(ctx, schema.pool, schema.compatibility)
}

func TestOlderPostgresRestoreCannotCrossRemovalBeforeReplay(t *testing.T) {
	database := testpg.New(t)
	root := t.TempDir()
	stores, err := localstore.Open(root+"/ledger", root+"/heads")
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.InitializeEmpty(t.Context()); err != nil {
		t.Fatal(err)
	}
	owner := "10000000-0000-4000-8000-000000000001"
	family := "20000000-0000-4000-8000-000000000001"
	device := "30000000-0000-4000-8000-000000000001"
	oldSession := "40000000-0000-4000-8000-000000000001"
	currentSession := "40000000-0000-4000-8000-000000000002"
	serverInstance := "50000000-0000-4000-8000-000000000001"
	// Seed the deliberately older database image through the isolated test
	// owner. Schema 6+ intentionally denies structural tenant-table DML to the
	// runtime role; this fixture is constructing a restored backup, not an
	// application request.
	seed, err := database.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), seed, tenant.Context{AccountID: owner, FamilyID: family}); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Owner')", owner); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(t.Context(), "INSERT INTO families(id,display_name) VALUES($1,'SAFE')", family); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(t.Context(), "INSERT INTO memberships(family_id,account_id,role) VALUES($1,$2,'owner')", family, owner); err != nil {
		t.Fatal(err)
	}
	if err := seed.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	prior := security.Head{Generation: 1}
	candidates := []security.Candidate{
		recoveryCandidate(t, prior, 1, "account_registered", []security.Effect{{Kind: "account_registered", AccountID: owner, IdentityKey: "opaque-owner", Status: "active"}}),
	}
	prior = security.Head{Generation: 1, Revision: 1, EventHash: candidates[0].EventHash}
	candidates = append(candidates, recoveryCandidate(t, prior, 2, "device_registered", []security.Effect{{Kind: "device_registered", AccountID: owner, DeviceID: device, ServerInstanceID: serverInstance, CanonicalOrigin: "https://family.example", DeviceCredentialDigest: "hmac-sha256:device-v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Status: "active"}}))
	prior = security.Head{Generation: 1, Revision: 2, EventHash: candidates[1].EventHash}
	candidates = append(candidates, recoveryCandidate(t, prior, 3, "session_granted", []security.Effect{{Kind: "session_granted", AccountID: owner, DeviceID: device, SessionID: oldSession, LineageRootSessionID: oldSession, RecoveryEpoch: 1, ExpiresAt: "2026-09-21T12:00:00Z", Status: "active"}}))
	prior = security.Head{Generation: 1, Revision: 3, EventHash: candidates[2].EventHash}
	candidates = append(candidates, recoveryCandidate(t, prior, 4, "family_created", []security.Effect{{Kind: "family_created", AccountID: owner, FamilyID: family, Role: "owner", Status: "active", ProjectionMaterialDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}))
	prior = security.Head{Generation: 1, Revision: 4, EventHash: candidates[3].EventHash}
	candidates = append(candidates, recoveryCandidate(t, prior, 5, "family_deletion_scheduled", []security.Effect{{Kind: "family_deletion_scheduled", AccountID: owner, FamilyID: family, Role: "owner", Status: "deletion_pending", PurgeAfter: "2026-10-20T12:05:00Z"}}))
	prior = security.Head{Generation: 1, Revision: 5, EventHash: candidates[4].EventHash}
	candidates = append(candidates, recoveryCandidate(t, prior, 6, "session_granted", []security.Effect{{Kind: "session_granted", AccountID: owner, DeviceID: device, SessionID: currentSession, LineageRootSessionID: currentSession, RecoveryEpoch: 1, ExpiresAt: "2026-09-21T13:00:00Z", Status: "active"}}))
	for index, candidate := range candidates {
		if err := stores.Ledger.AppendCandidate(t.Context(), candidate); err != nil {
			t.Fatal(err)
		}
		next := security.Head{Generation: 1, Revision: uint64(index + 1), EventHash: candidate.EventHash}
		if swapped, err := stores.Heads.CompareAndSwap(t.Context(), candidate.Prior, next); err != nil || !swapped {
			t.Fatalf("commit %d swapped=%v err=%v", index+1, swapped, err)
		}
	}
	postgresProjector := security.NewProjector(database.Projector)
	projector := reservedSessionProjection{Projection: postgresProjector, owner: database.Owner}
	if err := projector.Apply(t.Context(), candidates[0]); err != nil {
		t.Fatal(err)
	}
	seedFutureUninstalledProjection(t, database.Owner, candidates[1])
	seedFutureUninstalledProjection(t, database.Owner, candidates[2])
	seedFutureFamilyProjection(t, database.Owner, candidates[3], owner, family)
	restoredHead := security.Head{Generation: 1, Revision: 4, EventHash: candidates[3].EventHash}
	live := security.Head{Generation: 1, Revision: 6, EventHash: candidates[5].EventHash}
	durable, err := NewStore(root+"/heads", stores.Fence)
	if err != nil {
		t.Fatal(err)
	}
	instance := "123e4567-e89b-42d3-a456-426614174000"
	if err := durable.Initialize(t.Context(), State{InstanceID: instance, Epoch: 1, TrustedHead: live}); err != nil {
		t.Fatal(err)
	}
	bindGateTestActorIDKey(t, durable, instance)
	var authority *security.Service
	authority, err = security.New(security.Options{Ledger: stores.Ledger, Heads: stores.Heads, Projection: projector, Fence: stores.Fence,
		Runtime: database.Runtime, Now: func() time.Time { return time.Date(2026, 9, 20, 12, 3, 0, 0, time.UTC) },
		Epoch: func(ctx context.Context) (uint64, error) {
			state, err := durable.LoadUnderFence(ctx)
			return state.Epoch, err
		},
		Decide: func(ctx context.Context, principal access.Principal, familyID, action string) (access.Grant, error) {
			state, err := authority.ReadState(ctx)
			if err != nil {
				return access.Grant{}, err
			}
			view, ok := state.Family(familyID)
			if !ok || view.Status != "active" {
				return access.Grant{}, security.ErrMembershipRevoked
			}
			return access.Grant{AccountID: principal.AccountID, FamilyID: familyID, Revision: state.Head().Revision}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	compatibility, err := migrate.EmbeddedCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := NewPostgresMirror(database.Runtime, database.Projector)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := NewGate(Options{Store: durable, Security: authority, Schema: realSchema{database.Runtime, migrate.New(migrations.Files), compatibility},
		Mirror: mirror, ExpectedInstanceID: instance, ExpectedEpoch: 1, ActorIDKey: gateTestActorIDKey})
	if err != nil {
		t.Fatal(err)
	}
	principal := access.Principal{AccountID: owner, DeviceID: device, SessionID: currentSession, Scope: "family", RecoveryEpoch: 1, SecurityRevision: 6}
	callbackRan := false
	run := func(tx pgx.Tx, _ access.Grant) error {
		callbackRan = true
		_, err := tx.Exec(t.Context(), "UPDATE families SET display_name='STALE' WHERE id=$1", family)
		return err
	}
	if err := gate.Check(t.Context()); !errors.Is(err, ErrQuarantine) {
		t.Fatalf("older projection escaped quarantine: %v", err)
	}
	if err := authority.WithAuthorizedTransaction(t.Context(), principal, family, "read", run); err == nil || callbackRan {
		t.Fatalf("guard crossed removal before replay: err=%v callback=%v", err, callbackRan)
	}
	assertFamilyName(t, database.Runtime, owner, family, "SAFE")
	identityPath := root + "/heads/identity.json"
	wrongManifest := fmt.Sprintf("{\"instance_id\":\"%s\",\"epoch\":1,\"generation\":1,\"revision\":4,\"event_hash\":\"%064x\"}\n", instance, 7)
	if err := os.WriteFile(identityPath, []byte(wrongManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := gate.Reconcile(t.Context()); !errors.Is(err, ErrQuarantine) {
		t.Fatalf("changed trusted manifest reconciled: %v", err)
	}
	if head, err := projector.Head(t.Context()); err != nil || head != restoredHead {
		t.Fatalf("untrusted chain projected before validation: head=%+v err=%v", head, err)
	}
	validManifest := fmt.Sprintf("{\"instance_id\":\"%s\",\"epoch\":1,\"generation\":1,\"revision\":6,\"event_hash\":\"%s\"}\n", instance, hex.EncodeToString(live.EventHash[:]))
	if err := os.WriteFile(identityPath, []byte(validManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := gate.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if head, err := projector.Head(t.Context()); err != nil || head != live {
		t.Fatalf("projection head=%+v err=%v", head, err)
	}
	var mirroredRevision int64
	if err := database.Runtime.QueryRow(t.Context(), "SELECT revision FROM server_state WHERE singleton").Scan(&mirroredRevision); err != nil || mirroredRevision != 6 {
		t.Fatalf("mirrored revision=%d err=%v", mirroredRevision, err)
	}
	if err := authority.WithAuthorizedTransaction(t.Context(), principal, family, "read", run); security.Code(err) != "membership_revoked" || callbackRan {
		t.Fatalf("removed access denial code=%s err=%v callback=%v", security.Code(err), err, callbackRan)
	}
	assertFamilyName(t, database.Runtime, owner, family, "SAFE")
}

func seedFutureUninstalledProjection(t *testing.T, ownerPool *pgxpool.Pool, candidate security.Candidate) {
	t.Helper()
	seedFutureProjection(t, ownerPool, candidate, nil)
}

func seedFutureFamilyProjection(t *testing.T, ownerPool *pgxpool.Pool, candidate security.Candidate, owner, family string) {
	t.Helper()
	seedFutureProjection(t, ownerPool, candidate, func(tx pgx.Tx, event security.Event) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO family_security_state(family_id,owner_account_id,lifecycle,generation,revision,event_hash) VALUES($1,$2,'active',$3,$4,$5)`, family, owner, event.Generation, event.Revision, candidate.EventHash[:])
		return err
	})
}

func seedFutureProjection(t *testing.T, ownerPool *pgxpool.Pool, candidate security.Candidate, seed func(pgx.Tx, security.Event) error) {
	t.Helper()
	if err := applyFutureProjection(t.Context(), ownerPool, candidate, seed); err != nil {
		t.Fatal(err)
	}
}

func applyFutureProjection(ctx context.Context, ownerPool *pgxpool.Pool, candidate security.Candidate, seed func(pgx.Tx, security.Event) error) error {
	var event security.Event
	if err := json.Unmarshal(candidate.EventJSON, &event); err != nil {
		return err
	}
	commandHash, err := hex.DecodeString(event.CommandHash)
	if err != nil {
		return err
	}
	result := fmt.Sprintf(`{"command_id":%q,"outcome":"committed","code":"ok","revision":%d}`, event.CommandID, event.Revision)
	tx, err := ownerPool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if seed != nil {
		if err := seed(tx, event); err != nil {
			return err
		}
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO security_audit_events(generation,revision,event_hash,command_id,actor_account_id,kind,event_data,event_bytes) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8)`, []any{event.Generation, event.Revision, candidate.EventHash[:], event.CommandID, event.ActorAccountID, event.Kind, string(candidate.EventJSON), candidate.EventJSON}},
		{`INSERT INTO security_command_receipts(actor_account_id,command_id,command_hash,generation,revision,result) VALUES($1,$2,$3,$4,$5,$6::jsonb)`, []any{event.ActorAccountID, event.CommandID, commandHash, event.Generation, event.Revision, result}},
		{`UPDATE security_projection_head SET generation=$1,revision=$2,event_hash=$3 WHERE singleton`, []any{event.Generation, event.Revision, candidate.EventHash[:]}},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement.query, statement.args...); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func assertFamilyName(t *testing.T, pool *pgxpool.Pool, accountID, familyID, want string) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := tx.QueryRow(t.Context(), "SELECT display_name FROM families WHERE id=$1", familyID).Scan(&got); err != nil || got != want {
		t.Fatalf("family name=%q err=%v want=%q", got, err, want)
	}
}

func recoveryCandidate(t *testing.T, prior security.Head, revision uint64, kind security.EventKind, effects []security.Effect) security.Candidate {
	t.Helper()
	commandID := fmt.Sprintf("30000000-0000-4000-8000-%012d", revision)
	event := security.Event{Version: "v1", Generation: 1, Revision: revision, PreviousHash: hex.EncodeToString(prior.EventHash[:]),
		CommandID: commandID, CommandHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ActorAccountID: "10000000-0000-4000-8000-000000000001", Kind: kind,
		At: time.Date(2026, 9, 20, 12, int(revision), 0, 0, time.UTC).Format(time.RFC3339Nano), Effects: effects}
	encoded, err := security.EncodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	return security.Candidate{CommandID: commandID, Prior: prior, EventJSON: encoded, EventHash: sha256.Sum256(encoded)}
}
