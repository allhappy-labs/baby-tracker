package main

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/config"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/recovery"
	"babytracker/server/internal/security"
	"babytracker/server/internal/security/localstore"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testpg"
)

func TestVerifyOverlapFamilyResponseRequiresExactSemanticsAndStableID(t *testing.T) {
	directory := t.TempDir()
	bodyPath := filepath.Join(directory, "family.json")
	headerPath := filepath.Join(directory, "family.headers")
	familyIDPath := filepath.Join(directory, "family-id")
	const familyID = "94000000-0000-4000-8000-000000000001"
	body := `{"protocol_version":"v1","id":"` + familyID + `","display_name":"Overlap family","status":"active","profile_required":false,"members":[{"account_id":"93000000-0000-4000-8000-000000000001","display_name":"Overlap caregiver","role":"owner","status":"active"}]}`
	if err := os.WriteFile(bodyPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	writeHeaders := func(status string) {
		t.Helper()
		if err := os.WriteFile(headerPath, []byte("HTTP/1.1 "+status+"\r\nCache-Control: no-store\r\nContent-Type: application/json\r\n\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeHeaders("201 Created")
	if err := verifyOverlapFamilyResponse(bodyPath, headerPath, familyIDPath, 201, true); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(familyIDPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("family ID mode=%v err=%v", info.Mode().Perm(), err)
	}
	writeHeaders("200 OK")
	if err := verifyOverlapFamilyResponse(bodyPath, headerPath, familyIDPath, 200, false); err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(body, familyID, "94000000-0000-4000-8000-000000000002", 1)
	if err := os.WriteFile(bodyPath, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyOverlapFamilyResponse(bodyPath, headerPath, familyIDPath, 200, false); err == nil {
		t.Fatal("family ID drift accepted")
	}
	badCache := []byte("HTTP/1.1 200 OK\r\nCache-Control: public\r\n\r\n")
	if err := os.WriteFile(headerPath, badCache, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyOverlapFamilyResponse(bodyPath, headerPath, familyIDPath, 200, false); err == nil {
		t.Fatal("unsafe cache policy accepted")
	}
	writeHeaders("200 OK")
	for name, invalidBody := range map[string]string{
		"unknown field":     strings.TrimSuffix(body, "}") + `,"unknown":true}`,
		"trailing JSON":     body + `{}`,
		"semantic mismatch": strings.Replace(body, `"role":"owner"`, `"role":"caregiver"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(bodyPath, []byte(invalidBody), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := verifyOverlapFamilyResponse(bodyPath, headerPath, familyIDPath, 200, false); err == nil {
				t.Fatal("invalid family response accepted")
			}
		})
	}
	if err := os.WriteFile(bodyPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	writeHeaders("201 Created")
	if err := verifyOverlapFamilyResponse(bodyPath, headerPath, familyIDPath, 200, false); err == nil {
		t.Fatal("wrong HTTP status accepted")
	}
}

func TestVerifyOverlapInvitationAndRefreshedCaregiverSession(t *testing.T) {
	directory := t.TempDir()
	familyIDPath := filepath.Join(directory, "family-id")
	invitationIDPath := filepath.Join(directory, "invitation-id")
	bodyPath := filepath.Join(directory, "invitation.json")
	headerPath := filepath.Join(directory, "invitation.headers")
	const familyID = "94000000-0000-4000-8000-000000000001"
	const invitationID = "95000000-0000-4000-8000-000000000001"
	if err := os.WriteFile(familyIDPath, []byte(familyID), 0o600); err != nil {
		t.Fatal(err)
	}
	writeHeaders := func(status string) {
		t.Helper()
		if err := os.WriteFile(headerPath, []byte("HTTP/1.1 "+status+"\r\nCache-Control: no-store\r\nContent-Type: application/json\r\n\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	created := `{"protocol_version":"v1","id":"` + invitationID + `","family_id":"` + familyID + `","status":"active","expires_at":"2026-09-28T12:00:00Z","token":"` + strings.Repeat("A", 43) + `","link":"https://overlap.family.example/invite#token=` + strings.Repeat("A", 43) + `","secret_unavailable":false}`
	if err := os.WriteFile(bodyPath, []byte(created), 0o600); err != nil {
		t.Fatal(err)
	}
	writeHeaders("201 Created")
	if err := verifyOverlapInvitationResponse(bodyPath, headerPath, familyIDPath, invitationIDPath, 201, true); err != nil {
		t.Fatal(err)
	}
	read := `{"protocol_version":"v1","id":"` + invitationID + `","family_id":"` + familyID + `","status":"active","expires_at":"2026-09-28T12:00:00Z","secret_unavailable":true}`
	if err := os.WriteFile(bodyPath, []byte(read), 0o600); err != nil {
		t.Fatal(err)
	}
	writeHeaders("200 OK")
	if err := verifyOverlapInvitationResponse(bodyPath, headerPath, familyIDPath, invitationIDPath, 200, false); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(directory, "session.json")
	curlPath := filepath.Join(directory, "caregiver.curl")
	session := `{"protocol_version":"v1","access_token":"private-access","family_slot":{"family_id":"` + familyID + `","state":"active"}}`
	if err := os.WriteFile(sessionPath, []byte(session), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeOverlapSessionCurlConfig(sessionPath, curlPath, familyIDPath); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(curlPath)
	if err != nil || !strings.Contains(string(encoded), "private-access") {
		t.Fatalf("caregiver curl config err=%v", err)
	}
	wrong := strings.Replace(session, `"state":"active"`, `"state":"family_recovery"`, 1)
	if err := os.WriteFile(sessionPath, []byte(wrong), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeOverlapSessionCurlConfig(sessionPath, filepath.Join(directory, "wrong.curl"), familyIDPath); err == nil {
		t.Fatal("non-active caregiver session accepted")
	}
	errorBodyPath := filepath.Join(directory, "error.json")
	errorHeaderPath := filepath.Join(directory, "error.headers")
	if err := os.WriteFile(errorBodyPath, []byte(`{"error":{"code":"owner_required","message":"Access denied","request_id":"","retryable":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(errorHeaderPath, []byte("HTTP/1.1 403 Forbidden\r\nCache-Control: no-store\r\nContent-Type: application/json\r\n\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyOverlapErrorResponse(errorBodyPath, errorHeaderPath, 403, "owner_required"); err != nil {
		t.Fatal(err)
	}
	if err := verifyOverlapErrorResponse(errorBodyPath, errorHeaderPath, 403, "membership_revoked"); err == nil {
		t.Fatal("wrong role denial code accepted")
	}
}

func TestWriteOverlapCurlConfigIsExclusivePrivateAndKeepsBearerInFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "family-auth.curl")
	const token = "private-overlap-access-token"
	if err := writeOverlapCurlConfig(path, token); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("curl config mode=%v err=%v", info.Mode().Perm(), err)
	}
	content, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(content), "Authorization: Bearer "+token) || strings.Count(string(content), token) != 1 || strings.Contains(string(content), "Content-Type") {
		t.Fatalf("curl config did not contain exactly one bearer: err=%v", err)
	}
	if err := writeOverlapCurlConfig(path, "replacement"); err == nil {
		t.Fatal("exclusive curl config was overwritten")
	}
}

func TestOverlapIdentityVerifierRequiresExactAppleNonce(t *testing.T) {
	const origin = "https://overlap.family.example"
	const instanceID = "123e4567-e89b-42d3-a456-426614174000"
	const challenge = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"
	nonce, err := identity.AppleNonce(origin, instanceID, challenge)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	verifier := overlapIdentityVerifier("overlap-test", func() string { return nonce }, func() time.Time { return now })
	verified, err := verifier.Verify(t.Context(), overlapIdentitySubject, nonce)
	if err != nil || verified.Audience != "overlap-test" || verified.Subject != overlapIdentitySubject {
		t.Fatalf("verified=%+v err=%v", verified, err)
	}
	if _, err := verifier.Verify(t.Context(), overlapIdentitySubject, "wrong-nonce"); identity.Code(err) != "invalid_apple_proof" {
		t.Fatalf("wrong nonce err=%v code=%q", err, identity.Code(err))
	}
}

func TestIssueOverlapFamilySessionUsesVerifiedAuthorityAndAuthenticatesBearer(t *testing.T) {
	fixture := testpg.New(t)
	root := t.TempDir()
	ledger, head := filepath.Join(root, "ledger"), filepath.Join(root, "head")
	store, err := localstore.Open(ledger, head)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := url.Parse("https://overlap.family.example")
	if err != nil {
		t.Fatal(err)
	}
	actorKey := overlapTestSecret(0x66)
	cfg := config.Config{Mode: config.ModeDevelopment, DatabaseURL: fixture.Runtime.Config().ConnString(),
		ProjectorDatabaseURL: fixture.Projector.Config().ConnString(), PublicOrigin: origin,
		ServerInstanceID: "123e4567-e89b-42d3-a456-426614174000", RecoveryEpoch: 1,
		SecurityLedgerDir: ledger, LedgerHeadDir: head, AppleAudience: "overlap-test",
		Auth: config.AuthConfig{IdentityKeys: overlapTestKeyring("identity.v1", 0x11), SessionTokenKeys: overlapTestKeyring("session.v1", 0x22),
			DeviceContinuityKeys: overlapTestKeyring("device.v1", 0x33), AppleNotificationKeys: overlapTestKeyring("notification.v1", 0x44),
			InvitationKeys: overlapTestKeyring("invitation.v1", 0x55), ActorIDPepper: actorKey},
		Limits: config.LimitsConfig{MaxCaregivers: 10, MaxInvitations: 10}}
	durable, err := recovery.NewVerifiedStore(head, store.Fence, store.Ledger, store.Heads)
	if err != nil {
		t.Fatal(err)
	}
	if err := durable.InitializeAuthorityWithActorIDKey(t.Context(), store, recovery.State{InstanceID: cfg.ServerInstanceID,
		Epoch: 1, TrustedHead: security.Head{Generation: 1}}, actorKey); err != nil {
		t.Fatal(err)
	}
	bundle, err := issueOverlapFamilySession(t.Context(), cfg, fixture.Runtime, fixture.Projector, store)
	if err != nil || bundle.AccessToken == "" || bundle.Account.DisplayName != "Overlap caregiver" {
		t.Fatalf("bundle account=%+v token_present=%t err=%v", bundle.Account, bundle.AccessToken != "", err)
	}
	var sessions int
	if err := fixture.Owner.QueryRow(t.Context(), "SELECT count(*) FROM sessions WHERE account_id=$1 AND device_id=$2 AND status='active'",
		bundle.Account.ID, bundle.DeviceID).Scan(&sessions); err != nil || sessions != 1 {
		t.Fatalf("active sessions=%d err=%v", sessions, err)
	}
}

func TestAssertHTTPImportUnusedRejectsRowsForActualFamilyContext(t *testing.T) {
	fixture := testpg.New(t)
	const (
		accountID = "d2000000-0000-4000-8000-000000000001"
		familyID  = "d2000000-0000-4000-8000-000000000002"
		deviceID  = "d2000000-0000-4000-8000-000000000003"
		importID  = "d2000000-0000-4000-8000-000000000004"
	)
	now := time.Date(2026, 9, 21, 15, 0, 0, 0, time.UTC)
	setup, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), setup, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'HTTP owner')", accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO families(id,display_name) VALUES($1,'HTTP family')", familyID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO memberships(family_id,account_id,role) VALUES($1,$2,'owner')", familyID, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), `INSERT INTO import_sessions(
		family_id,import_id,initiating_account_id,initiating_device_id,import_mode,normalization_version,
		total_record_count,stored_record_count,next_chunk_index,manifest_complete,finalized_at,
		mapping_complete,mapping_completed_at,resolver_revision,created_at,expires_at,storage_bytes)
		VALUES($1,$2,$3,$4,'match_shared','nfc-trim-fold-nfc-v1',0,0,0,true,$5,true,$5,1,$5,$6,64)`,
		familyID, importID, accountID, deviceID, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := setup.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	accountIDPath := filepath.Join(directory, "account-id")
	familyIDPath := filepath.Join(directory, "family-id")
	if err := os.WriteFile(accountIDPath, []byte(accountID), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(familyIDPath, []byte(familyID), 0o600); err != nil {
		t.Fatal(err)
	}
	getenv := func(name string) string {
		if name == "BT_OVERLAP_RUNTIME_DATABASE_URL" {
			return fixture.Runtime.Config().ConnString()
		}
		return ""
	}
	if err := assertHTTPImportUnused(t.Context(), accountIDPath, familyIDPath, 29, getenv); err == nil || !strings.Contains(err.Error(), "used import tables") {
		t.Fatalf("completed import session was not detected: %v", err)
	}
}

func overlapTestSecret(fill byte) (secret [32]byte) {
	for index := range secret {
		secret[index] = fill
	}
	return secret
}

func overlapTestKeyring(id string, fill byte) config.DigestKeyringConfig {
	return config.DigestKeyringConfig{Current: config.DigestKeyConfig{ID: id, Secret: overlapTestSecret(fill)}}
}

func TestRunRejectsMissingOrSharedCredentialsBeforeConnecting(t *testing.T) {
	for _, values := range []map[string]string{
		{},
		{"BT_OVERLAP_RUNTIME_DATABASE_URL": "postgres://same", "BT_OVERLAP_PROJECTOR_DATABASE_URL": "postgres://same"},
	} {
		if err := run(context.Background(), "scenario", 4, func(key string) string { return values[key] }); err == nil {
			t.Fatal("unsafe overlap credentials accepted")
		}
	}
	if err := run(context.Background(), "", 4, func(string) string { return "postgres://different" }); err == nil {
		t.Fatal("empty scenario accepted")
	}
}

func TestRunTimerCompatibilitySchemaThirteenPredecessorScenario(t *testing.T) {
	fixture := testpg.New(t)
	const accountID = "93000000-0000-4000-8000-000000000001"
	tx, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), "SELECT set_config('app.account_id',$1,true),set_config('app.family_id',$2,true)", accountID, "94000000-0000-4000-8000-000000000001"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Before overlap')", accountID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"BT_OVERLAP_MIGRATOR_DATABASE_URL":  fixture.Owner.Config().ConnString(),
		"BT_OVERLAP_RUNTIME_DATABASE_URL":   fixture.Runtime.Config().ConnString(),
		"BT_OVERLAP_PROJECTOR_DATABASE_URL": fixture.Projector.Config().ConnString(),
	}
	if err := run(t.Context(), "timer-compatibility-schema-thirteen", 29, func(key string) string { return values[key] }); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), "timer-compatibility-schema-thirteen-retry", 29, func(key string) string { return values[key] }); err != nil {
		t.Fatalf("schema-thirteen overlap retry: %v", err)
	}
	read, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback(t.Context())
	if err := tenant.SetAccountLocal(t.Context(), read, accountID); err != nil {
		t.Fatal(err)
	}
	var displayName string
	if err := read.QueryRow(t.Context(), "SELECT display_name FROM app.current_account_profile($1)", accountID).Scan(&displayName); err != nil {
		t.Fatal(err)
	}
	if displayName != "Overlap caregiver" {
		t.Fatalf("authenticated account write=%q", displayName)
	}
}

func TestRunSchemaTwelveRejectsSnapshotTableWithoutForcedRLS(t *testing.T) {
	fixture := testpg.New(t)
	const accountID = "93000000-0000-4000-8000-000000000001"
	tx, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetAccountLocal(t.Context(), tx, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Before overlap')", accountID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Owner.Exec(t.Context(), "ALTER TABLE sync_snapshots NO FORCE ROW LEVEL SECURITY"); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"BT_OVERLAP_MIGRATOR_DATABASE_URL":  fixture.Owner.Config().ConnString(),
		"BT_OVERLAP_RUNTIME_DATABASE_URL":   fixture.Runtime.Config().ConnString(),
		"BT_OVERLAP_PROJECTOR_DATABASE_URL": fixture.Projector.Config().ConnString(),
	}
	if err := run(t.Context(), "snapshot-schema-without-force", 29, func(key string) string { return values[key] }); err == nil {
		t.Fatal("schema-twelve overlap accepted snapshot table without forced RLS")
	}
}

func TestRunSchemaTwelveRejectsTimerTableWithoutForcedRLS(t *testing.T) {
	fixture := testpg.New(t)
	const accountID = "93000000-0000-4000-8000-000000000001"
	tx, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetAccountLocal(t.Context(), tx, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Before overlap')", accountID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Owner.Exec(t.Context(), "ALTER TABLE active_timers NO FORCE ROW LEVEL SECURITY"); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"BT_OVERLAP_MIGRATOR_DATABASE_URL":  fixture.Owner.Config().ConnString(),
		"BT_OVERLAP_RUNTIME_DATABASE_URL":   fixture.Runtime.Config().ConnString(),
		"BT_OVERLAP_PROJECTOR_DATABASE_URL": fixture.Projector.Config().ConnString(),
	}
	if err := run(t.Context(), "timer-schema-without-force", 29, func(key string) string { return values[key] }); err == nil {
		t.Fatal("schema-twelve overlap accepted timer table without forced RLS")
	}
}

func TestRunSchemaTwelveRejectsImportTableWithoutForcedRLS(t *testing.T) {
	fixture := testpg.New(t)
	const accountID = "93000000-0000-4000-8000-000000000001"
	tx, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetAccountLocal(t.Context(), tx, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Before overlap')", accountID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Owner.Exec(t.Context(), "ALTER TABLE import_sessions NO FORCE ROW LEVEL SECURITY"); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"BT_OVERLAP_MIGRATOR_DATABASE_URL":  fixture.Owner.Config().ConnString(),
		"BT_OVERLAP_RUNTIME_DATABASE_URL":   fixture.Runtime.Config().ConnString(),
		"BT_OVERLAP_PROJECTOR_DATABASE_URL": fixture.Projector.Config().ConnString(),
	}
	if err := run(t.Context(), "import-schema-without-force", 29, func(key string) string { return values[key] }); err == nil {
		t.Fatal("schema-twelve overlap accepted import table without forced RLS")
	}
}

func TestSchemaTwelveImportPredecessorRejectsPopulatedImportTable(t *testing.T) {
	fixture := testpg.New(t)
	const accountID = "93000000-0000-4000-8000-000000000001"
	const scenario = "import-predecessor-populated"
	tx, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetAccountLocal(t.Context(), tx, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Before overlap')", accountID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"BT_OVERLAP_MIGRATOR_DATABASE_URL":  fixture.Owner.Config().ConnString(),
		"BT_OVERLAP_RUNTIME_DATABASE_URL":   fixture.Runtime.Config().ConnString(),
		"BT_OVERLAP_PROJECTOR_DATABASE_URL": fixture.Projector.Config().ConnString(),
	}
	if err := run(t.Context(), scenario, 29, func(key string) string { return values[key] }); err != nil {
		t.Fatalf("prepare schema-twelve scenario: %v", err)
	}
	familyID := overlapScenarioUUID("family", scenario)
	insert, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer insert.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), insert, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := insert.Exec(t.Context(), `INSERT INTO import_sessions(
		family_id,import_id,initiating_account_id,initiating_device_id,import_mode,normalization_version,
		total_record_count,stored_record_count,next_chunk_index,manifest_complete,finalized_at,mapping_complete,
		mapping_completed_at,resolver_revision,created_at,expires_at,storage_bytes)
		VALUES($1,'93000000-0000-4000-8000-000000009901',$2,'93000000-0000-4000-8000-000000009902',
		'match_shared','nfc-trim-fold-nfc-v1',0,0,1,true,'2026-09-20T12:30:00Z',true,
		'2026-09-20T12:30:00Z',1,'2026-09-20T12:00:00Z','2026-09-20T13:00:00Z',64)`, familyID, accountID); err != nil {
		t.Fatal(err)
	}
	if err := insert.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := verifySchemaTwelveImportPredecessor(t.Context(), fixture.Owner, fixture.Runtime, fixture.Projector, scenario); err == nil {
		t.Fatal("schema-twelve predecessor accepted populated import table")
	}
}

func TestSchemaTwelveImportPredecessorAllowsAnotherFamilyImport(t *testing.T) {
	fixture := testpg.New(t)
	seedOverlapAccountForTest(t, fixture)
	const accountID = "93000000-0000-4000-8000-000000000001"
	const sourceScenario = "import-predecessor-other-source"
	const targetScenario = "import-predecessor-other-target"
	values := map[string]string{
		"BT_OVERLAP_MIGRATOR_DATABASE_URL":  fixture.Owner.Config().ConnString(),
		"BT_OVERLAP_RUNTIME_DATABASE_URL":   fixture.Runtime.Config().ConnString(),
		"BT_OVERLAP_PROJECTOR_DATABASE_URL": fixture.Projector.Config().ConnString(),
	}
	if err := run(t.Context(), sourceScenario, 29, func(key string) string { return values[key] }); err != nil {
		t.Fatalf("prepare source scenario: %v", err)
	}
	familyID := overlapScenarioUUID("family", sourceScenario)
	insert, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer insert.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), insert, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := insert.Exec(t.Context(), `INSERT INTO import_sessions(
		family_id,import_id,initiating_account_id,initiating_device_id,import_mode,normalization_version,
		total_record_count,stored_record_count,next_chunk_index,manifest_complete,finalized_at,mapping_complete,
		mapping_completed_at,resolver_revision,created_at,expires_at,storage_bytes)
		VALUES($1,'93000000-0000-4000-8000-000000009911',$2,'93000000-0000-4000-8000-000000009912',
		'match_shared','nfc-trim-fold-nfc-v1',0,0,1,true,'2026-09-20T12:30:00Z',true,
		'2026-09-20T12:30:00Z',1,'2026-09-20T12:00:00Z','2026-09-20T13:00:00Z',64)`, familyID, accountID); err != nil {
		t.Fatal(err)
	}
	if err := insert.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), targetScenario, 29, func(key string) string { return values[key] }); err != nil {
		t.Fatalf("another family's durable import blocked overlap check: %v", err)
	}
}

func TestRunSchemaThirteenRejectsDeliveryTableWithoutForcedRLS(t *testing.T) {
	fixture := testpg.New(t)
	seedOverlapAccountForTest(t, fixture)
	if _, err := fixture.Owner.Exec(t.Context(), "ALTER TABLE delivery_pending NO FORCE ROW LEVEL SECURITY"); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"BT_OVERLAP_MIGRATOR_DATABASE_URL":  fixture.Owner.Config().ConnString(),
		"BT_OVERLAP_RUNTIME_DATABASE_URL":   fixture.Runtime.Config().ConnString(),
		"BT_OVERLAP_PROJECTOR_DATABASE_URL": fixture.Projector.Config().ConnString(),
	}
	if err := run(t.Context(), "delivery-schema-without-force", 29, func(key string) string { return values[key] }); err == nil {
		t.Fatal("schema-thirteen overlap accepted delivery_pending without forced RLS")
	}
}

func TestSchemaThirteenDeliveryPredecessorRejectsPopulatedDelivery(t *testing.T) {
	fixture := testpg.New(t)
	seedOverlapAccountForTest(t, fixture)
	const scenario = "delivery-predecessor-populated"
	values := map[string]string{
		"BT_OVERLAP_MIGRATOR_DATABASE_URL":  fixture.Owner.Config().ConnString(),
		"BT_OVERLAP_RUNTIME_DATABASE_URL":   fixture.Runtime.Config().ConnString(),
		"BT_OVERLAP_PROJECTOR_DATABASE_URL": fixture.Projector.Config().ConnString(),
	}
	if err := run(t.Context(), scenario, 29, func(key string) string { return values[key] }); err != nil {
		t.Fatalf("prepare schema-thirteen scenario: %v", err)
	}
	familyID := overlapScenarioUUID("family", scenario)
	deviceID := overlapScenarioUUID("device", scenario)
	insert, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer insert.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), insert, tenant.Context{AccountID: overlapAccountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := insert.Exec(t.Context(), `INSERT INTO delivery_pending(
		family_id,device_id,instance_id,recovery_epoch,sequence,generation,not_before,attempts,created_at,updated_at)
		VALUES($1,$2,'123e4567-e89b-42d3-a456-426614174000',1,1,1,now(),0,now(),now())`, familyID, deviceID); err != nil {
		t.Fatal(err)
	}
	if err := insert.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := verifyDeliveryPredecessorUnused(t.Context(), fixture.Owner, fixture.Runtime, fixture.Projector,
		overlapAccountID, familyID, 29); err == nil {
		t.Fatal("schema-thirteen predecessor accepted populated delivery_pending")
	}
}

func seedOverlapAccountForTest(t *testing.T, fixture *testpg.Fixture) {
	t.Helper()
	tx, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetAccountLocal(t.Context(), tx, overlapAccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Before overlap')", overlapAccountID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}
