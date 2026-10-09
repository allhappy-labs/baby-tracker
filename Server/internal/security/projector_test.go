package security_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/security"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestProjectionIdempotence(t *testing.T) {
	fixture := testpg.New(t)
	projector := security.NewProjector(fixture.Projector)
	if head, err := projector.Head(t.Context()); err != nil || head != (security.Head{Generation: 1}) {
		t.Fatalf("initial head=%+v err=%v", head, err)
	}
	candidate := projectionCandidate(t, "opaque-identity-a", time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC))
	if err := projector.Apply(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	if err := projector.Apply(t.Context(), candidate); err != nil {
		t.Fatalf("idempotent projection: %v", err)
	}
	var count int
	if err := fixture.Owner.QueryRow(t.Context(), "SELECT count(*) FROM security_audit_events").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("audit rows=%d", count)
	}
	if head, err := projector.Head(t.Context()); err != nil || head.Revision != 1 || head.EventHash != candidate.EventHash {
		t.Fatalf("projected head=%+v err=%v", head, err)
	}
	mismatch := projectionCandidate(t, "opaque-identity-b", time.Date(2026, 9, 20, 12, 0, 1, 0, time.UTC))
	if err := projector.Apply(t.Context(), mismatch); err == nil {
		t.Fatal("mismatched revision/hash was accepted")
	}
}

func TestSchemaSixProjectsFamilyCreatedWithMissingPresentationPlaceholder(t *testing.T) {
	const (
		owner  = "10000000-0000-4000-8000-000000000001"
		family = "20000000-0000-4000-8000-000000000001"
	)
	fixture := testpg.New(t)
	projector := security.NewProjector(fixture.Projector)
	account := projectionCandidate(t, "opaque-owner", time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC))
	if err := projector.Apply(t.Context(), account); err != nil {
		t.Fatal(err)
	}
	tx, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), "SELECT set_config('app.account_id',$1,true)", owner); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Owner')", owner); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	head := security.Head{Generation: 1, Revision: 1, EventHash: account.EventHash}
	created := projectionEventCandidate(t, head, "30000000-0000-4000-8000-000000000002", "family_created", []security.Effect{{
		Kind: "family_created", AccountID: owner, FamilyID: family, Role: "owner", Status: "active",
		ProjectionMaterialDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}}, time.Date(2026, 9, 20, 12, 1, 0, 0, time.UTC))
	if err := projector.Apply(t.Context(), created); err != nil {
		t.Fatalf("schema-six family projection: %v", err)
	}
	read, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback(t.Context())
	if _, err := read.Exec(t.Context(), "SELECT set_config('app.account_id',$1,true),set_config('app.family_id',$2,true)", owner, family); err != nil {
		t.Fatal(err)
	}
	var displayName, role, slotState string
	var profileRequired bool
	if err := read.QueryRow(t.Context(), `SELECT f.display_name,f.profile_required,m.role,s.state
		FROM families f JOIN memberships m ON m.family_id=f.id JOIN family_slots s ON s.family_id=f.id
		WHERE f.id=$1 AND m.account_id=$2 AND s.account_id=$2`, family, owner).
		Scan(&displayName, &profileRequired, &role, &slotState); err != nil {
		t.Fatal(err)
	}
	if displayName != "" || !profileRequired || role != "owner" || slotState != "active" {
		t.Fatalf("placeholder projection name=%q required=%t role=%q slot=%q", displayName, profileRequired, role, slotState)
	}
}

func TestSchemaSixMemberRemovalRevokesAccountAndFamilySessions(t *testing.T) {
	const (
		owner     = "10000000-0000-4000-8000-000000000001"
		caregiver = "10000000-0000-4000-8000-000000000002"
		family    = "20000000-0000-4000-8000-000000000001"
		device    = "40000000-0000-4000-8000-000000000001"
		instance  = "50000000-0000-4000-8000-000000000001"
	)
	fixture := testpg.New(t)
	setup, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Rollback(t.Context())
	if _, err := setup.Exec(t.Context(), "SELECT set_config('app.family_id',$1,true),set_config('app.account_id',$2,true)", family, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Owner')", owner); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "SELECT set_config('app.account_id',$1,true)", caregiver); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Caregiver')", caregiver); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "SELECT set_config('app.account_id',$1,true)", owner); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO families(id,display_name) VALUES($1,'Family')", family); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO memberships(family_id,account_id,role) VALUES($1,$2,'owner'),($1,$3,'caregiver')", family, owner, caregiver); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "SELECT set_config('app.account_id',$1,true)", caregiver); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO family_slots(account_id,family_id,state) VALUES($1,$2,'active')", caregiver, family); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), `INSERT INTO devices(id,account_id,server_instance_id,canonical_origin,continuity_key_id,continuity_digest,created_at,updated_at)
		VALUES($1,$2,$3,'https://family.example','device.v1',decode(repeat('11',32),'hex'),'2026-09-20T11:00:00Z','2026-09-20T11:00:00Z')`, device, caregiver, instance); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), `INSERT INTO sessions(session_id,account_id,device_id,lineage_root_session_id,server_instance_id,recovery_epoch,security_revision,scope,family_id,
		token_key_id,access_digest,refresh_digest,access_expires_at,refresh_expires_at,reauthenticate_at,absolute_expires_at,status,consumed_at,created_at,updated_at)
		VALUES
		('70000000-0000-4000-8000-000000000001',$1,$2,'70000000-0000-4000-8000-000000000001',$3,1,1,'account',NULL,
		 'session.v1',decode(repeat('21',32),'hex'),decode(repeat('22',32),'hex'),'2026-09-20T12:10:00Z','2026-09-21T12:00:00Z','2026-10-20T12:00:00Z','2026-10-20T12:00:00Z','active',NULL,'2026-09-20T12:00:00Z','2026-09-20T12:00:00Z'),
		('70000000-0000-4000-8000-000000000002',$1,$2,'70000000-0000-4000-8000-000000000002',$3,1,1,'family',$4,
		 'session.v1',decode(repeat('23',32),'hex'),decode(repeat('24',32),'hex'),'2026-09-20T12:10:00Z','2026-09-21T12:00:00Z','2026-10-20T12:00:00Z','2026-10-20T12:00:00Z','consumed','2026-09-20T12:00:30Z','2026-09-20T12:00:00Z','2026-09-20T12:00:30Z')`, caregiver, device, instance, family); err != nil {
		t.Fatal(err)
	}
	if err := setup.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	candidate := projectionEventCandidate(t, security.Head{Generation: 1}, "30000000-0000-4000-8000-000000000001", "member_removed", []security.Effect{{
		Kind: "member_removed", AccountID: caregiver, FamilyID: family, Status: "removed",
	}}, time.Date(2026, 9, 20, 12, 1, 0, 0, time.UTC))
	if err := security.NewProjector(fixture.Projector).Apply(t.Context(), candidate); err != nil {
		t.Fatalf("project member removal: %v", err)
	}
	rows, err := fixture.Owner.Query(t.Context(), "SELECT scope,status,revoke_reason FROM sessions WHERE account_id=$1 ORDER BY scope", caregiver)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var scope, status string
		var reason *string
		if err := rows.Scan(&scope, &status, &reason); err != nil {
			t.Fatal(err)
		}
		if status != "revoked" || reason == nil || *reason != "membership_revoked" {
			t.Fatalf("%s session status=%q reason=%v", scope, status, reason)
		}
		seen++
	}
	if seen != 2 {
		t.Fatalf("revoked sessions=%d want=2", seen)
	}
}

func TestProjectorAcceptsGoCanonicalOpaqueStrings(t *testing.T) {
	tests := []struct {
		name     string
		identity string
		encoded  string
	}{
		{"less than", "<", `\u003c`},
		{"greater than", ">", `\u003e`},
		{"ampersand", "&", `\u0026`},
		{"line separator", "\u2028", `\u2028`},
		{"paragraph separator", "\u2029", `\u2029`},
		{"quote", `"`, `\"`},
		{"backslash", `\`, `\\`},
		{"control escapes", "\b\f\n\r\t\u0001", `\b\f\n\r\t\u0001`},
		{"ascii 4096 bytes", strings.Repeat("x", 4096), ""},
		{"multibyte 4095 bytes", strings.Repeat("€", 1365), ""},
		{"multibyte 4096 bytes", strings.Repeat("é", 2048), ""},
		{"four-byte 4096 bytes", strings.Repeat("😀", 1024), ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := testpg.New(t)
			candidate := projectionCandidate(t, test.identity, time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC))
			if test.encoded != "" && !bytes.Contains(candidate.EventJSON, []byte(`"identity_key":"`+test.encoded+`"`)) {
				t.Fatalf("Go encoding missing golden %q in %s", test.encoded, candidate.EventJSON)
			}
			if err := security.NewProjector(fixture.Projector).Apply(t.Context(), candidate); err != nil {
				t.Fatalf("Go-canonical projector apply: %v", err)
			}
			var identity string
			if err := fixture.Owner.QueryRow(t.Context(), `SELECT identity_key FROM account_security_state`).Scan(&identity); err != nil || identity != test.identity {
				t.Fatalf("identity bytes=%d want=%d err=%v", len(identity), len(test.identity), err)
			}
		})
	}
}

func TestDirectProjectorRejectsOversizedOpaqueStringsWithoutMutation(t *testing.T) {
	for _, size := range []int{4097, 5000} {
		t.Run(fmt.Sprintf("%d bytes", size), func(t *testing.T) {
			fixture := testpg.New(t)
			event := projectionEvent("30000000-0000-4000-8000-000000000001", strings.Repeat("x", size))
			raw, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(raw)
			_, err = fixture.Projector.Exec(t.Context(), `SELECT app.project_security_event($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9::jsonb,$10)`, 1, 1, make([]byte, 32), sum[:], event.ActorAccountID, event.CommandID, bytes.Repeat([]byte{0xaa}, 32), validProjectionResult(), string(raw), raw)
			if err == nil {
				t.Fatal("direct projector accepted oversized opaque field")
			}
			assertEmptyProjection(t, fixture)
		})
	}
}

func TestDirectProjectorRejectsNoncanonicalGoStringAlternatives(t *testing.T) {
	canonical := projectionCandidate(t, "<\u2028", time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC))
	tests := map[string][]byte{
		"literal html":      bytes.Replace(canonical.EventJSON, []byte(`\u003c`), []byte(`<`), 1),
		"uppercase hex":     bytes.Replace(canonical.EventJSON, []byte(`\u003c`), []byte(`\u003C`), 1),
		"literal separator": bytes.Replace(canonical.EventJSON, []byte(`\u2028`), []byte("\u2028"), 1),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := testpg.New(t)
			sum := sha256.Sum256(raw)
			_, err := fixture.Projector.Exec(t.Context(), `SELECT app.project_security_event($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9::jsonb,$10)`, 1, 1, make([]byte, 32), sum[:], "10000000-0000-4000-8000-000000000001", canonical.CommandID, bytes.Repeat([]byte{0xaa}, 32), validProjectionResult(), string(raw), raw)
			if err == nil {
				t.Fatal("direct projector accepted noncanonical Go string encoding")
			}
			assertEmptyProjection(t, fixture)
		})
	}
}

func TestProjectorCannotWriteProjectionTablesDirectly(t *testing.T) {
	fixture := testpg.New(t)
	_, err := fixture.Projector.Exec(t.Context(), "UPDATE security_projection_head SET revision = revision + 1")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("error=%v, want SQLSTATE 42501", err)
	}
}

func TestDirectProjectorRejectsUnboundOrIncompleteEvents(t *testing.T) {
	candidate := projectionCandidate(t, "opaque-identity", time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC))
	var base map[string]any
	if err := json.Unmarshal(candidate.EventJSON, &base); err != nil {
		t.Fatal(err)
	}
	var baseEvent security.Event
	if err := json.Unmarshal(candidate.EventJSON, &baseEvent); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(map[string]any) ([]byte, []byte, string){
		"zero actor device": func(event map[string]any) ([]byte, []byte, string) {
			mutated := baseEvent
			mutated.ActorDeviceID = "00000000-0000-0000-0000-000000000000"
			raw, _ := json.Marshal(mutated)
			sum := sha256.Sum256(raw)
			return raw, sum[:], validProjectionResult()
		},
		"noncanonical effect id": func(event map[string]any) ([]byte, []byte, string) {
			mutated := baseEvent
			mutated.Effects = append([]security.Effect(nil), baseEvent.Effects...)
			mutated.Effects[0].AccountID = "10000000-0000-4000-8000-00000000000A"
			raw, _ := json.Marshal(mutated)
			sum := sha256.Sum256(raw)
			return raw, sum[:], validProjectionResult()
		},
		"zero effect id": func(event map[string]any) ([]byte, []byte, string) {
			mutated := baseEvent
			mutated.Effects = append([]security.Effect(nil), baseEvent.Effects...)
			mutated.Effects[0].AccountID = "00000000-0000-0000-0000-000000000000"
			raw, _ := json.Marshal(mutated)
			sum := sha256.Sum256(raw)
			return raw, sum[:], validProjectionResult()
		},
		"noncanonical timestamp": func(event map[string]any) ([]byte, []byte, string) {
			mutated := baseEvent
			mutated.At = "2026-09-20T12:00:00.000Z"
			raw, _ := json.Marshal(mutated)
			sum := sha256.Sum256(raw)
			return raw, sum[:], validProjectionResult()
		},
		"noncanonical bytes": func(event map[string]any) ([]byte, []byte, string) {
			raw := append([]byte(" "), candidate.EventJSON...)
			sum := sha256.Sum256(raw)
			return raw, sum[:], validProjectionResult()
		},
		"invalid actor device": func(event map[string]any) ([]byte, []byte, string) {
			event["actor_device_id"] = "not-a-canonical-uuid"
			raw, _ := json.Marshal(event)
			sum := sha256.Sum256(raw)
			return raw, sum[:], validProjectionResult()
		},
		"missing version": func(event map[string]any) ([]byte, []byte, string) {
			delete(event, "version")
			raw, _ := json.Marshal(event)
			sum := sha256.Sum256(raw)
			return raw, sum[:], validProjectionResult()
		},
		"empty effects": func(event map[string]any) ([]byte, []byte, string) {
			event["effects"] = []any{}
			raw, _ := json.Marshal(event)
			sum := sha256.Sum256(raw)
			return raw, sum[:], validProjectionResult()
		},
		"wrong known field": func(event map[string]any) ([]byte, []byte, string) {
			effect := event["effects"].([]any)[0].(map[string]any)
			effect["device_id"] = "40000000-0000-4000-8000-000000000001"
			raw, _ := json.Marshal(event)
			sum := sha256.Sum256(raw)
			return raw, sum[:], validProjectionResult()
		},
		"arbitrary result": func(event map[string]any) ([]byte, []byte, string) {
			raw, _ := json.Marshal(event)
			sum := sha256.Sum256(raw)
			return raw, sum[:], `{"outcome":"forged"}`
		},
		"arbitrary hash": func(event map[string]any) ([]byte, []byte, string) {
			raw, _ := json.Marshal(event)
			return raw, bytes.Repeat([]byte{0xff}, 32), validProjectionResult()
		},
		"known unimplemented": func(event map[string]any) ([]byte, []byte, string) {
			event["kind"] = "device_registered"
			effect := event["effects"].([]any)[0].(map[string]any)
			effect["kind"] = "device_registered"
			raw, _ := json.Marshal(event)
			sum := sha256.Sum256(raw)
			return raw, sum[:], validProjectionResult()
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := testpg.New(t)
			copiedBytes, _ := json.Marshal(base)
			var event map[string]any
			_ = json.Unmarshal(copiedBytes, &event)
			raw, eventHash, result := mutate(event)
			_, err := fixture.Projector.Exec(t.Context(), `SELECT app.project_security_event($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9::jsonb,$10)`, 1, 1, make([]byte, 32), eventHash, "10000000-0000-4000-8000-000000000001", "30000000-0000-4000-8000-000000000001", bytes.Repeat([]byte{0xaa}, 32), result, string(raw), raw)
			if err == nil {
				t.Fatal("direct projector accepted invalid event")
			}
			head, headErr := security.NewProjector(fixture.Projector).Head(t.Context())
			if headErr != nil || head != (security.Head{Generation: 1}) {
				t.Fatalf("head=%+v err=%v", head, headErr)
			}
			var audit, receipts, accounts, families int
			if err := fixture.Owner.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM security_audit_events),(SELECT count(*) FROM security_command_receipts),(SELECT count(*) FROM account_security_state),(SELECT count(*) FROM family_security_state)`).Scan(&audit, &receipts, &accounts, &families); err != nil {
				t.Fatal(err)
			}
			if audit != 0 || receipts != 0 || accounts != 0 || families != 0 {
				t.Fatalf("audit=%d receipts=%d accounts=%d families=%d", audit, receipts, accounts, families)
			}
		})
	}
}

func TestDirectProjectorRejectsNoncanonicalDeletionEffects(t *testing.T) {
	const (
		owner      = "10000000-0000-4000-8000-000000000001"
		caregiver  = "10000000-0000-4000-8000-000000000002"
		family     = "20000000-0000-4000-8000-000000000001"
		other      = "20000000-0000-4000-8000-000000000002"
		purgeAfter = "2026-09-22T12:00:00Z"
	)
	baseEffects := []security.Effect{
		{Kind: "family_deletion_scheduled", AccountID: owner, FamilyID: family, Role: "owner", Status: "deletion_pending", PurgeAfter: purgeAfter},
		{Kind: "family_deletion_scheduled", AccountID: caregiver, FamilyID: family, Role: "caregiver", Status: "former", PurgeAfter: purgeAfter},
	}
	tests := map[string]func([]security.Effect) []security.Effect{
		"out of order": func(effects []security.Effect) []security.Effect {
			return []security.Effect{effects[1], effects[0]}
		},
		"duplicate account": func(effects []security.Effect) []security.Effect {
			effects[1].AccountID = owner
			return effects
		},
		"mixed families": func(effects []security.Effect) []security.Effect {
			effects[1].FamilyID = other
			return effects
		},
		"mixed purge deadlines": func(effects []security.Effect) []security.Effect {
			effects[1].PurgeAfter = "2026-09-23T12:00:00Z"
			return effects
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := testpg.New(t)
			projector := security.NewProjector(fixture.Projector)
			account := projectionCandidate(t, "opaque-owner", time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC))
			if err := projector.Apply(t.Context(), account); err != nil {
				t.Fatal(err)
			}
			head1 := security.Head{Generation: 1, Revision: 1, EventHash: account.EventHash}
			created := projectionEventCandidate(t, head1, "30000000-0000-4000-8000-000000000002", "family_created", []security.Effect{{Kind: "family_created", AccountID: owner, FamilyID: family, Role: "owner", Status: "active", ProjectionMaterialDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}, time.Date(2026, 9, 20, 12, 1, 0, 0, time.UTC))
			seedFutureFamilyProjection(t, fixture.Owner, created, owner, family)
			head2 := security.Head{Generation: 1, Revision: 2, EventHash: created.EventHash}
			effects := mutate(append([]security.Effect(nil), baseEffects...))
			event := security.Event{Version: "v1", Generation: 1, Revision: 3, PreviousHash: hex.EncodeToString(head2.EventHash[:]), CommandID: "30000000-0000-4000-8000-000000000003", CommandHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ActorAccountID: owner, ActorDeviceID: "", Kind: "family_deletion_scheduled", At: "2026-09-20T12:02:00Z", Effects: effects}
			raw, _ := json.Marshal(event)
			eventHash := sha256.Sum256(raw)
			result := `{"command_id":"30000000-0000-4000-8000-000000000003","outcome":"committed","code":"ok","revision":3}`
			_, err := fixture.Projector.Exec(t.Context(), `SELECT app.project_security_event($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9::jsonb,$10)`, 1, 3, head2.EventHash[:], eventHash[:], owner, event.CommandID, bytes.Repeat([]byte{0xaa}, 32), result, string(raw), raw)
			if err == nil {
				t.Fatal("direct projector accepted invalid deletion effects")
			}
			if head, err := projector.Head(t.Context()); err != nil || head != head2 {
				t.Fatalf("head=%+v err=%v", head, err)
			}
			var audit, receipts, accounts, families int
			if err := fixture.Owner.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM security_audit_events),(SELECT count(*) FROM security_command_receipts),(SELECT count(*) FROM account_security_state),(SELECT count(*) FROM family_security_state)`).Scan(&audit, &receipts, &accounts, &families); err != nil {
				t.Fatal(err)
			}
			if audit != 2 || receipts != 2 || accounts != 1 || families != 1 {
				t.Fatalf("audit=%d receipts=%d accounts=%d families=%d", audit, receipts, accounts, families)
			}
			var lifecycle string
			if err := fixture.Owner.QueryRow(t.Context(), `SELECT lifecycle FROM family_security_state WHERE family_id=$1`, family).Scan(&lifecycle); err != nil || lifecycle != "active" {
				t.Fatalf("lifecycle=%q err=%v", lifecycle, err)
			}
		})
	}
}

func seedFutureFamilyProjection(t *testing.T, ownerPool *pgxpool.Pool, candidate security.Candidate, owner, family string) {
	t.Helper()
	var event security.Event
	if err := json.Unmarshal(candidate.EventJSON, &event); err != nil {
		t.Fatal(err)
	}
	commandHash, err := hex.DecodeString(event.CommandHash)
	if err != nil {
		t.Fatal(err)
	}
	result := fmt.Sprintf(`{"command_id":%q,"outcome":"committed","code":"ok","revision":%d}`, event.CommandID, event.Revision)
	tx, err := ownerPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `INSERT INTO family_security_state(family_id,owner_account_id,lifecycle,generation,revision,event_hash) VALUES($1,$2,'active',$3,$4,$5)`, family, owner, event.Generation, event.Revision, candidate.EventHash[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO security_audit_events(generation,revision,event_hash,command_id,actor_account_id,kind,event_data,event_bytes) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8)`, event.Generation, event.Revision, candidate.EventHash[:], event.CommandID, event.ActorAccountID, event.Kind, string(candidate.EventJSON), candidate.EventJSON); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO security_command_receipts(actor_account_id,command_id,command_hash,generation,revision,result) VALUES($1,$2,$3,$4,$5,$6::jsonb)`, event.ActorAccountID, event.CommandID, commandHash, event.Generation, event.Revision, result); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE security_projection_head SET generation=$1,revision=$2,event_hash=$3 WHERE singleton`, event.Generation, event.Revision, candidate.EventHash[:]); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func validProjectionResult() string {
	return `{"command_id":"30000000-0000-4000-8000-000000000001","outcome":"committed","code":"ok","revision":1}`
}

func TestRuntimeCannotProject(t *testing.T) {
	fixture := testpg.New(t)
	projector := security.NewProjector(fixture.Runtime)
	err := projector.Apply(t.Context(), projectionCandidate(t, "opaque-identity", time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("error=%v, want SQLSTATE 42501", err)
	}
}

func projectionCandidate(t *testing.T, identity string, at time.Time) security.Candidate {
	t.Helper()
	prior := security.Head{Generation: 1}
	event := projectionEvent("30000000-0000-4000-8000-000000000001", identity)
	event.At = at.Format(time.RFC3339Nano)
	encoded, err := security.EncodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	return security.Candidate{CommandID: event.CommandID, Prior: prior, EventJSON: encoded, EventHash: sha256.Sum256(encoded)}
}

func projectionEvent(commandID, identity string) security.Event {
	return security.Event{
		Version: "v1", Generation: 1, Revision: 1, PreviousHash: strings.Repeat("0", 64),
		CommandID: commandID, CommandHash: strings.Repeat("a", 64),
		ActorAccountID: "10000000-0000-4000-8000-000000000001", ActorDeviceID: "", Kind: "account_registered", At: "2026-09-20T12:00:00Z",
		Effects: []security.Effect{{Kind: "account_registered", AccountID: "10000000-0000-4000-8000-000000000001", IdentityKey: identity, Status: "active"}},
	}
}

func assertEmptyProjection(t *testing.T, fixture *testpg.Fixture) {
	t.Helper()
	if head, err := security.NewProjector(fixture.Projector).Head(t.Context()); err != nil || head != (security.Head{Generation: 1}) {
		t.Fatalf("head=%+v err=%v", head, err)
	}
	var audit, receipts, accounts, families int
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM security_audit_events),(SELECT count(*) FROM security_command_receipts),(SELECT count(*) FROM account_security_state),(SELECT count(*) FROM family_security_state)`).Scan(&audit, &receipts, &accounts, &families); err != nil {
		t.Fatal(err)
	}
	if audit != 0 || receipts != 0 || accounts != 0 || families != 0 {
		t.Fatalf("audit=%d receipts=%d accounts=%d families=%d", audit, receipts, accounts, families)
	}
}

func projectionEventCandidate(t *testing.T, prior security.Head, commandID string, kind security.EventKind, effects []security.Effect, at time.Time) security.Candidate {
	t.Helper()
	event := security.Event{
		Version: "v1", Generation: prior.Generation, Revision: prior.Revision + 1, PreviousHash: hex.EncodeToString(prior.EventHash[:]),
		CommandID: commandID, CommandHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ActorAccountID: "10000000-0000-4000-8000-000000000001", ActorDeviceID: "", Kind: kind, At: at.Format(time.RFC3339Nano), Effects: effects,
	}
	encoded, err := security.EncodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	return security.Candidate{CommandID: commandID, Prior: prior, EventJSON: encoded, EventHash: sha256.Sum256(encoded)}
}
