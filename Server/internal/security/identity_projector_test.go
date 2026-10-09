package security_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"babytracker/server/internal/security"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	identityAccount  = "10000000-0000-4000-8000-000000000001"
	identityDevice   = "40000000-0000-4000-8000-000000000001"
	identityInstance = "50000000-0000-4000-8000-000000000001"
	identitySession  = "60000000-0000-4000-8000-000000000001"
	identityKey      = "hmac-sha256:identity.v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	continuityKey    = "hmac-sha256:device.v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type exchangeStage struct {
	Version                 string  `json:"version"`
	Kind                    string  `json:"kind"`
	AccountID               string  `json:"account_id"`
	DeviceID                string  `json:"device_id"`
	SessionID               string  `json:"session_id"`
	LineageRootSessionID    string  `json:"lineage_root_session_id"`
	ServerInstanceID        string  `json:"server_instance_id"`
	CanonicalOrigin         string  `json:"canonical_origin"`
	TokenKeyID              string  `json:"token_key_id"`
	AccessDigest            string  `json:"access_digest"`
	RefreshDigest           string  `json:"refresh_digest"`
	RecoveryEpoch           uint64  `json:"recovery_epoch"`
	SecurityRevision        uint64  `json:"security_revision"`
	Scope                   string  `json:"scope"`
	FamilyID                *string `json:"family_id"`
	AccessExpiresAt         string  `json:"access_expires_at"`
	RefreshExpiresAt        string  `json:"refresh_expires_at"`
	ReauthenticateAt        string  `json:"reauthenticate_at"`
	AbsoluteExpiresAt       string  `json:"absolute_expires_at"`
	IdentityProvider        string  `json:"identity_provider"`
	IdentityKeyID           string  `json:"identity_key_id"`
	IdentityDigest          string  `json:"identity_digest"`
	IdentityAudience        string  `json:"identity_audience"`
	IdentitySubject         string  `json:"identity_subject"`
	DisplayName             *string `json:"display_name"`
	ProfileConfirmed        bool    `json:"profile_confirmed"`
	RegisterAccount         bool    `json:"register_account"`
	RegisterDevice          bool    `json:"register_device"`
	InstallationKeyID       *string `json:"installation_key_id"`
	InstallationDigest      *string `json:"installation_digest"`
	ContinuityKeyID         *string `json:"continuity_key_id"`
	ContinuityDigest        *string `json:"continuity_digest"`
	AppleCredentialID       *string `json:"apple_credential_id"`
	AppleCredentialKeyID    *string `json:"apple_credential_key_id"`
	AppleCredentialPurpose  *string `json:"apple_credential_purpose"`
	AppleCredentialObjectID *string `json:"apple_credential_object_id"`
}

type refreshStage struct {
	Version              string  `json:"version"`
	Kind                 string  `json:"kind"`
	AccountID            string  `json:"account_id"`
	DeviceID             string  `json:"device_id"`
	SessionID            string  `json:"session_id"`
	LineageRootSessionID string  `json:"lineage_root_session_id"`
	ServerInstanceID     string  `json:"server_instance_id"`
	CanonicalOrigin      string  `json:"canonical_origin"`
	TokenKeyID           string  `json:"token_key_id"`
	AccessDigest         string  `json:"access_digest"`
	RefreshDigest        string  `json:"refresh_digest"`
	RecoveryEpoch        uint64  `json:"recovery_epoch"`
	SecurityRevision     uint64  `json:"security_revision"`
	Scope                string  `json:"scope"`
	FamilyID             *string `json:"family_id"`
	AccessExpiresAt      string  `json:"access_expires_at"`
	RefreshExpiresAt     string  `json:"refresh_expires_at"`
	ReauthenticateAt     string  `json:"reauthenticate_at"`
	AbsoluteExpiresAt    string  `json:"absolute_expires_at"`
	ParentSessionID      string  `json:"parent_session_id"`
}

// Removing stage/effect binding or making projection non-atomic must fail this
// real PostgreSQL issue path.
func TestIdentitySessionIssuedFinalizesExactStageAtomically(t *testing.T) {
	fixture := testpg.New(t)
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	commandID := "30000000-0000-4000-8000-000000000001"
	stage := validExchangeStage(at, identitySession)
	bytes, digest := marshalStage(t, stage)
	stageMaterial(t, fixture, commandID, bytes, digest, at)
	candidate := identityIssueCandidate(t, security.Head{Generation: 1}, commandID, identitySession, digest, at)
	projector := security.NewProjector(fixture.Projector)
	if err := projector.Apply(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	if err := projector.Apply(t.Context(), candidate); err != nil {
		t.Fatalf("exact replay failed: %v", err)
	}
	setOwnerAccount(t, fixture, identityAccount)
	var accounts, devices, sessions, identities, active, projected int
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM accounts),(SELECT count(*) FROM devices),(SELECT count(*) FROM sessions),
		(SELECT count(*) FROM account_identities),(SELECT count(*) FROM sessions WHERE status='active'),
		(SELECT count(*) FROM auth_exchange_material WHERE projected_at IS NOT NULL)`).Scan(
		&accounts, &devices, &sessions, &identities, &active, &projected); err != nil {
		t.Fatal(err)
	}
	if accounts != 1 || devices != 1 || sessions != 1 || identities != 1 || active != 1 || projected != 1 {
		t.Fatalf("accounts=%d devices=%d sessions=%d identities=%d active=%d projected=%d", accounts, devices, sessions, identities, active, projected)
	}
	// An exact lost-response restage remains idempotent after projection has
	// scrubbed the material; it must not repopulate sensitive columns.
	stageMaterial(t, fixture, commandID, bytes, digest, at.Add(5*time.Minute))
	var sensitiveFields int
	var createdAt, expiresAt, projectedAt time.Time
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT num_nonnulls(token_key_id,access_digest,refresh_digest,
		identity_provider,identity_key_id,identity_digest,identity_audience,identity_subject,display_name,
		profile_confirmed,register_account,register_device,installation_key_id,installation_digest,
		continuity_key_id,continuity_digest,apple_credential_id,apple_credential_key_id,
		apple_credential_purpose,apple_credential_object_id),created_at,expires_at,projected_at
		FROM auth_exchange_material WHERE command_id=$1`, commandID).Scan(
		&sensitiveFields, &createdAt, &expiresAt, &projectedAt); err != nil {
		t.Fatal(err)
	}
	if sensitiveFields != 0 {
		t.Fatalf("projected stage retained %d sensitive fields", sensitiveFields)
	}
	if !createdAt.Equal(at) || !expiresAt.Equal(at.Add(10*time.Minute)) || !projectedAt.Equal(at) {
		t.Fatalf("exact restage changed lifecycle created=%s expires=%s projected=%s", createdAt, expiresAt, projectedAt)
	}
}

func TestIdentitySessionIssuedIsAtomicAcrossProjectionCrash(t *testing.T) {
	fixture := testpg.New(t)
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	commandID := "30000000-0000-4000-8000-000000000009"
	stage := validExchangeStage(at, identitySession)
	bytes, digest := marshalStage(t, stage)
	stageMaterial(t, fixture, commandID, bytes, digest, at)
	if _, err := fixture.Owner.Exec(t.Context(), `CREATE FUNCTION public.fail_identity_session_insert() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected projection crash'; END $$;
		CREATE TRIGGER fail_identity_session_insert BEFORE INSERT ON public.sessions
		FOR EACH ROW EXECUTE FUNCTION public.fail_identity_session_insert()`); err != nil {
		t.Fatal(err)
	}
	candidate := identityIssueCandidate(t, security.Head{Generation: 1}, commandID, identitySession, digest, at)
	if err := security.NewProjector(fixture.Projector).Apply(t.Context(), candidate); err == nil {
		t.Fatal("injected projection crash unexpectedly committed")
	}
	setOwnerAccount(t, fixture, identityAccount)
	var accounts, devices, sessions, identities, audits, revision, unprojected int
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM accounts),(SELECT count(*) FROM devices),(SELECT count(*) FROM sessions),
		(SELECT count(*) FROM account_identities),(SELECT count(*) FROM security_audit_events),
		(SELECT revision FROM security_projection_head WHERE singleton),
		(SELECT count(*) FROM auth_exchange_material WHERE command_id=$1 AND projected_at IS NULL)`, commandID).Scan(
		&accounts, &devices, &sessions, &identities, &audits, &revision, &unprojected); err != nil {
		t.Fatal(err)
	}
	if accounts != 0 || devices != 0 || sessions != 0 || identities != 0 || audits != 0 || revision != 0 || unprojected != 1 {
		t.Fatalf("partial projection accounts=%d devices=%d sessions=%d identities=%d audits=%d revision=%d unprojected=%d",
			accounts, devices, sessions, identities, audits, revision, unprojected)
	}
}

func TestIdentitySessionIssuedRejectsStageEffectMismatch(t *testing.T) {
	fixture := testpg.New(t)
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	commandID := "30000000-0000-4000-8000-000000000010"
	bytes, digest := marshalStage(t, validExchangeStage(at, identitySession))
	stageMaterial(t, fixture, commandID, bytes, digest, at)
	wrongDigest := sha256.Sum256([]byte("mismatched projection material"))
	candidate := identityIssueCandidate(t, security.Head{Generation: 1}, commandID, identitySession, wrongDigest, at)
	if err := security.NewProjector(fixture.Projector).Apply(t.Context(), candidate); err == nil {
		t.Fatal("projection accepted a mismatched staged digest")
	}
	var revision, projected, sessions int
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT
		(SELECT revision FROM security_projection_head WHERE singleton),
		(SELECT count(*) FROM auth_exchange_material WHERE command_id=$1 AND projected_at IS NOT NULL),
		(SELECT count(*) FROM sessions)`, commandID).Scan(&revision, &projected, &sessions); err != nil {
		t.Fatal(err)
	}
	if revision != 0 || projected != 0 || sessions != 0 {
		t.Fatalf("mismatched stage changed revision=%d projected=%d sessions=%d", revision, projected, sessions)
	}
}

func TestIdentitySessionIssuedRejectsStagedSecurityRevisionMismatch(t *testing.T) {
	fixture := testpg.New(t)
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	commandID := "30000000-0000-4000-8000-000000000013"
	stage := validExchangeStage(at, identitySession)
	stage.SecurityRevision = 2
	bytes, digest := marshalStage(t, stage)
	stageMaterial(t, fixture, commandID, bytes, digest, at)
	candidate := identityIssueCandidate(t, security.Head{Generation: 1}, commandID, identitySession, digest, at)
	if err := security.NewProjector(fixture.Projector).Apply(t.Context(), candidate); err == nil {
		t.Fatal("projection accepted a staged security revision different from the event revision")
	}
	var revision, sessions int
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT
		(SELECT revision FROM security_projection_head WHERE singleton),(SELECT count(*) FROM sessions)`).Scan(&revision, &sessions); err != nil {
		t.Fatal(err)
	}
	if revision != 0 || sessions != 0 {
		t.Fatalf("revision mismatch changed revision=%d sessions=%d", revision, sessions)
	}
}

// Deleting staged material during an older database restore must never
// reconstruct a bearer credential.
func TestIdentitySessionIssuedMissingStageCreatesNoBearer(t *testing.T) {
	fixture := testpg.New(t)
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	candidate := identityIssueCandidate(t, security.Head{Generation: 1}, "30000000-0000-4000-8000-000000000011", identitySession,
		sha256.Sum256([]byte("lost stage")), at)
	if err := security.NewProjector(fixture.Projector).Apply(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	setOwnerAccount(t, fixture, identityAccount)
	var profileRequired, missing bool
	var access, refresh []byte
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT a.profile_required,s.status='material_missing',s.access_digest,s.refresh_digest
		FROM accounts a JOIN sessions s ON s.account_id=a.id`).Scan(&profileRequired, &missing, &access, &refresh); err != nil {
		t.Fatal(err)
	}
	if !profileRequired || !missing || access != nil || refresh != nil {
		t.Fatalf("profile_required=%t missing=%t access=%x refresh=%x", profileRequired, missing, access, refresh)
	}
}

// Rotation must consume/create/history in the projector transaction, and one
// reuse event must close the complete lineage rather than enumerating it.
func TestRefreshProjectionIsAtomicAndReuseRevokesLineage(t *testing.T) {
	fixture := testpg.New(t)
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	issueBytes, issueDigest := marshalStage(t, validExchangeStage(at, identitySession))
	stageMaterial(t, fixture, "30000000-0000-4000-8000-000000000021", issueBytes, issueDigest, at)
	issue := identityIssueCandidate(t, security.Head{Generation: 1}, "30000000-0000-4000-8000-000000000021", identitySession, issueDigest, at)
	projector := security.NewProjector(fixture.Projector)
	if err := projector.Apply(t.Context(), issue); err != nil {
		t.Fatal(err)
	}
	next := "60000000-0000-4000-8000-000000000002"
	refresh := refreshStage{
		Version: "v1", Kind: "refresh", AccountID: identityAccount, DeviceID: identityDevice, SessionID: next,
		LineageRootSessionID: identitySession, ServerInstanceID: identityInstance, CanonicalOrigin: "https://sync.example.test",
		TokenKeyID: "session.v1", AccessDigest: repeatHex("33"), RefreshDigest: repeatHex("44"), RecoveryEpoch: 1,
		SecurityRevision: 2, Scope: "account", AccessExpiresAt: at.Add(20 * time.Minute).Format(time.RFC3339Nano),
		RefreshExpiresAt: at.Add(2 * time.Hour).Format(time.RFC3339Nano), ReauthenticateAt: at.Add(24 * time.Hour).Format(time.RFC3339Nano),
		AbsoluteExpiresAt: at.Add(24 * time.Hour).Format(time.RFC3339Nano), ParentSessionID: identitySession,
	}
	refreshBytes, refreshDigest := marshalStage(t, refresh)
	stageMaterial(t, fixture, "30000000-0000-4000-8000-000000000022", refreshBytes, refreshDigest, at.Add(time.Minute))
	head1 := security.Head{Generation: 1, Revision: 1, EventHash: issue.EventHash}
	rotate := projectionEventCandidate(t, head1, "30000000-0000-4000-8000-000000000022", "session_rotated", []security.Effect{{
		Kind: "session_rotated", AccountID: identityAccount, DeviceID: identityDevice, SessionID: next, ParentSessionID: identitySession,
		LineageRootSessionID: identitySession, ProjectionMaterialDigest: hex.EncodeToString(refreshDigest[:]), RecoveryEpoch: 1,
		ExpiresAt: at.Add(24 * time.Hour).Format(time.RFC3339Nano), Status: "active",
	}}, at.Add(time.Minute))
	if err := projector.Apply(t.Context(), rotate); err != nil {
		t.Fatal(err)
	}
	var consumed, active, history int
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM sessions WHERE status='consumed'),(SELECT count(*) FROM sessions WHERE status='active'),
		(SELECT count(*) FROM session_refresh_history)`).Scan(&consumed, &active, &history); err != nil {
		t.Fatal(err)
	}
	if consumed != 1 || active != 1 || history != 1 {
		t.Fatalf("consumed=%d active=%d history=%d", consumed, active, history)
	}
	head2 := security.Head{Generation: 1, Revision: 2, EventHash: rotate.EventHash}
	revoke := projectionEventCandidate(t, head2, "30000000-0000-4000-8000-000000000023", "session_revoked", []security.Effect{{
		Kind: "session_revoked", AccountID: identityAccount, DeviceID: identityDevice, SessionID: identitySession,
		LineageRootSessionID: identitySession, RecoveryEpoch: 1, ExpiresAt: at.Add(24 * time.Hour).Format(time.RFC3339Nano), Status: "revoked",
	}}, at.Add(2*time.Minute))
	if err := projector.Apply(t.Context(), revoke); err != nil {
		t.Fatal(err)
	}
	var revoked, reused int
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM sessions WHERE status='revoked'),
		(SELECT count(*) FROM session_refresh_history WHERE reuse_detected_at IS NOT NULL)`).Scan(&revoked, &reused); err != nil {
		t.Fatal(err)
	}
	if revoked != 2 || reused != 1 {
		t.Fatalf("revoked=%d reused=%d", revoked, reused)
	}
}

func TestRefreshProjectionMissingStageCreatesNoBearer(t *testing.T) {
	fixture := testpg.New(t)
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	issueBytes, issueDigest := marshalStage(t, validExchangeStage(at, identitySession))
	stageMaterial(t, fixture, "30000000-0000-4000-8000-000000000031", issueBytes, issueDigest, at)
	issue := identityIssueCandidate(t, security.Head{Generation: 1}, "30000000-0000-4000-8000-000000000031", identitySession, issueDigest, at)
	projector := security.NewProjector(fixture.Projector)
	if err := projector.Apply(t.Context(), issue); err != nil {
		t.Fatal(err)
	}
	next := "60000000-0000-4000-8000-000000000003"
	missingDigest := sha256.Sum256([]byte("lost refresh stage"))
	rotate := projectionEventCandidate(t, security.Head{Generation: 1, Revision: 1, EventHash: issue.EventHash},
		"30000000-0000-4000-8000-000000000032", "session_rotated", []security.Effect{{
			Kind: "session_rotated", AccountID: identityAccount, DeviceID: identityDevice, SessionID: next,
			ParentSessionID: identitySession, LineageRootSessionID: identitySession,
			ProjectionMaterialDigest: hex.EncodeToString(missingDigest[:]), RecoveryEpoch: 1,
			ExpiresAt: at.Add(24 * time.Hour).Format(time.RFC3339Nano), Status: "active",
		}}, at.Add(time.Minute))
	if err := projector.Apply(t.Context(), rotate); err != nil {
		t.Fatal(err)
	}
	var status string
	var tokenKey *string
	var access, refresh []byte
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT status,token_key_id,access_digest,refresh_digest
		FROM sessions WHERE session_id=$1`, next).Scan(&status, &tokenKey, &access, &refresh); err != nil {
		t.Fatal(err)
	}
	if status != "material_missing" || tokenKey != nil || access != nil || refresh != nil {
		t.Fatalf("missing refresh stage status=%s token=%v access=%x refresh=%x", status, tokenKey, access, refresh)
	}
	var consumed bool
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT status='consumed' FROM sessions WHERE session_id=$1`, identitySession).Scan(&consumed); err != nil || !consumed {
		t.Fatalf("parent consumed=%t err=%v", consumed, err)
	}
}

func TestStagedRefreshCannotReconstructBearerForOpaqueParent(t *testing.T) {
	fixture := testpg.New(t)
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	issueDigest := sha256.Sum256([]byte("missing parent stage"))
	issue := identityIssueCandidate(t, security.Head{Generation: 1}, "30000000-0000-4000-8000-000000000071",
		identitySession, issueDigest, at)
	projector := security.NewProjector(fixture.Projector)
	if err := projector.Apply(t.Context(), issue); err != nil {
		t.Fatal(err)
	}

	next := "60000000-0000-4000-8000-000000000071"
	refresh := validRefreshStage(at.Add(time.Minute), next, identitySession, 2)
	refreshBytes, refreshDigest := marshalStage(t, refresh)
	commandID := "30000000-0000-4000-8000-000000000072"
	stageMaterial(t, fixture, commandID, refreshBytes, refreshDigest, at.Add(time.Minute))
	rotate := projectionEventCandidate(t, security.Head{Generation: 1, Revision: 1, EventHash: issue.EventHash},
		commandID, "session_rotated", []security.Effect{{
			Kind: "session_rotated", AccountID: identityAccount, DeviceID: identityDevice, SessionID: next,
			ParentSessionID: identitySession, LineageRootSessionID: identitySession,
			ProjectionMaterialDigest: hex.EncodeToString(refreshDigest[:]), RecoveryEpoch: 1,
			ExpiresAt: at.Add(24 * time.Hour).Format(time.RFC3339Nano), Status: "active",
		}}, at.Add(time.Minute))
	if err := projector.Apply(t.Context(), rotate); err == nil {
		t.Fatal("staged refresh reconstructed bearer material for an opaque parent")
	}
	var revision, children, history, projected int
	var parentStatus string
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT
		(SELECT revision FROM security_projection_head WHERE singleton),
		(SELECT status FROM sessions WHERE session_id=$1),
		(SELECT count(*) FROM sessions WHERE session_id=$2),
		(SELECT count(*) FROM session_refresh_history),
		(SELECT count(*) FROM auth_exchange_material WHERE command_id=$3 AND projected_at IS NOT NULL)`,
		identitySession, next, commandID).Scan(&revision, &parentStatus, &children, &history, &projected); err != nil {
		t.Fatal(err)
	}
	if revision != 1 || parentStatus != "material_missing" || children != 0 || history != 0 || projected != 0 {
		t.Fatalf("opaque staged rotation revision=%d parent=%s children=%d history=%d projected=%d",
			revision, parentStatus, children, history, projected)
	}
}

func TestSessionRotationRejectsStagedSecurityRevisionMismatch(t *testing.T) {
	fixture := testpg.New(t)
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	issueBytes, issueDigest := marshalStage(t, validExchangeStage(at, identitySession))
	issueCommand := "30000000-0000-4000-8000-000000000075"
	stageMaterial(t, fixture, issueCommand, issueBytes, issueDigest, at)
	issue := identityIssueCandidate(t, security.Head{Generation: 1}, issueCommand, identitySession, issueDigest, at)
	projector := security.NewProjector(fixture.Projector)
	if err := projector.Apply(t.Context(), issue); err != nil {
		t.Fatal(err)
	}

	next := "60000000-0000-4000-8000-000000000075"
	refresh := validRefreshStage(at.Add(time.Minute), next, identitySession, 3)
	refreshBytes, refreshDigest := marshalStage(t, refresh)
	commandID := "30000000-0000-4000-8000-000000000076"
	stageMaterial(t, fixture, commandID, refreshBytes, refreshDigest, at.Add(time.Minute))
	rotate := projectionEventCandidate(t, security.Head{Generation: 1, Revision: 1, EventHash: issue.EventHash},
		commandID, "session_rotated", []security.Effect{{
			Kind: "session_rotated", AccountID: identityAccount, DeviceID: identityDevice, SessionID: next,
			ParentSessionID: identitySession, LineageRootSessionID: identitySession,
			ProjectionMaterialDigest: hex.EncodeToString(refreshDigest[:]), RecoveryEpoch: 1,
			ExpiresAt: at.Add(24 * time.Hour).Format(time.RFC3339Nano), Status: "active",
		}}, at.Add(time.Minute))
	if err := projector.Apply(t.Context(), rotate); err == nil {
		t.Fatal("rotation accepted a staged security revision different from the event revision")
	}
	var revision, active, children, history, projected int
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT
		(SELECT revision FROM security_projection_head WHERE singleton),
		(SELECT count(*) FROM sessions WHERE session_id=$1 AND status='active'),
		(SELECT count(*) FROM sessions WHERE session_id=$2),
		(SELECT count(*) FROM session_refresh_history),
		(SELECT count(*) FROM auth_exchange_material WHERE command_id=$3 AND projected_at IS NOT NULL)`,
		identitySession, next, commandID).Scan(&revision, &active, &children, &history, &projected); err != nil {
		t.Fatal(err)
	}
	if revision != 1 || active != 1 || children != 0 || history != 0 || projected != 0 {
		t.Fatalf("rotation mismatch revision=%d active=%d children=%d history=%d projected=%d",
			revision, active, children, history, projected)
	}
}

func TestMissingStageLineageSupportsRotationAndEverySetWiseRevocation(t *testing.T) {
	for _, kind := range []string{"session_revoked", "device_binding_revoked", "identity_unavailable"} {
		t.Run(kind, func(t *testing.T) {
			fixture := testpg.New(t)
			at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
			projector, head, child := projectMissingIdentityAndRotation(t, fixture, at)
			var effects []security.Effect
			switch kind {
			case "session_revoked":
				effects = []security.Effect{{Kind: "session_revoked", AccountID: identityAccount, DeviceID: identityDevice,
					SessionID: child, LineageRootSessionID: identitySession, RecoveryEpoch: 1,
					ExpiresAt: at.Add(24 * time.Hour).Format(time.RFC3339Nano), Status: "revoked"}}
			case "device_binding_revoked":
				effects = []security.Effect{{Kind: "device_binding_revoked", AccountID: identityAccount, DeviceID: identityDevice,
					ServerInstanceID: identityInstance, CanonicalOrigin: "https://sync.example.test",
					DeviceCredentialDigest: continuityKey, Status: "revoked"}}
			case "identity_unavailable":
				effects = []security.Effect{{Kind: "identity_unavailable", AccountID: identityAccount,
					IdentityKey: identityKey, Status: "identity_unavailable"}}
			}
			revoked := projectionEventCandidate(t, head, "30000000-0000-4000-8000-000000000083", security.EventKind(kind), effects, at.Add(2*time.Minute))
			if err := projector.Apply(t.Context(), revoked); err != nil {
				t.Fatalf("opaque lineage revocation quarantined projection: %v", err)
			}
			reason := map[string]string{
				"session_revoked":        "refresh_reused",
				"device_binding_revoked": "device_revoked",
				"identity_unavailable":   "identity_unavailable",
			}[kind]
			var revokedSessions, changedStatus, revision int
			if err := fixture.Owner.QueryRow(t.Context(), `SELECT
				(SELECT count(*) FROM sessions WHERE lineage_root_session_id=$1 AND status='material_missing'
					AND revoked_at IS NOT NULL AND revoke_reason=$2),
				(SELECT count(*) FROM sessions WHERE lineage_root_session_id=$1 AND status<>'material_missing'),
				(SELECT revision FROM security_projection_head WHERE singleton)`, identitySession, reason).
				Scan(&revokedSessions, &changedStatus, &revision); err != nil {
				t.Fatal(err)
			}
			if revokedSessions != 2 || changedStatus != 0 || revision != 3 {
				t.Fatalf("opaque lineage revoked=%d changed_status=%d revision=%d", revokedSessions, changedStatus, revision)
			}
			if kind == "identity_unavailable" {
				restored := projectionEventCandidate(t, security.Head{Generation: 1, Revision: 3, EventHash: revoked.EventHash},
					"30000000-0000-4000-8000-000000000084", "identity_restored", []security.Effect{{
						Kind: "identity_restored", AccountID: identityAccount, IdentityKey: identityKey, Status: "active",
					}}, at.Add(3*time.Minute))
				if err := projector.Apply(t.Context(), restored); err != nil {
					t.Fatal(err)
				}
				if err := fixture.Owner.QueryRow(t.Context(), `SELECT count(*) FROM sessions
					WHERE lineage_root_session_id=$1 AND status='material_missing' AND revoked_at IS NOT NULL`, identitySession).
					Scan(&revokedSessions); err != nil {
					t.Fatal(err)
				}
				if revokedSessions != 2 {
					t.Fatalf("identity restore revived opaque sessions: revoked=%d", revokedSessions)
				}
			}
		})
	}
}

func TestConsumedOpaqueSessionCannotRotateTwice(t *testing.T) {
	fixture := testpg.New(t)
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	projector, head, _ := projectMissingIdentityAndRotation(t, fixture, at)
	secondChild := "60000000-0000-4000-8000-000000000085"
	digest := sha256.Sum256([]byte("second missing rotation stage"))
	rotateAgain := projectionEventCandidate(t, head, "30000000-0000-4000-8000-000000000085", "session_rotated", []security.Effect{{
		Kind: "session_rotated", AccountID: identityAccount, DeviceID: identityDevice, SessionID: secondChild,
		ParentSessionID: identitySession, LineageRootSessionID: identitySession,
		ProjectionMaterialDigest: hex.EncodeToString(digest[:]), RecoveryEpoch: 1,
		ExpiresAt: at.Add(24 * time.Hour).Format(time.RFC3339Nano), Status: "active",
	}}, at.Add(2*time.Minute))
	if err := projector.Apply(t.Context(), rotateAgain); err == nil {
		t.Fatal("consumed opaque session rotated twice")
	}
	var revision, secondChildren int
	var status string
	var consumedAt time.Time
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT
		(SELECT revision FROM security_projection_head WHERE singleton),
		(SELECT count(*) FROM sessions WHERE session_id=$1),
		status,consumed_at FROM sessions WHERE session_id=$2`, secondChild, identitySession).
		Scan(&revision, &secondChildren, &status, &consumedAt); err != nil {
		t.Fatal(err)
	}
	if revision != 2 || secondChildren != 0 || status != "material_missing" || !consumedAt.Equal(at.Add(time.Minute)) {
		t.Fatalf("second rotation revision=%d children=%d parent=%s consumed=%s",
			revision, secondChildren, status, consumedAt)
	}
}

func TestSessionConstraintKeepsBearerShapeBoundToMaterialMissingStatus(t *testing.T) {
	for _, terminalStatus := range []string{"consumed", "revoked"} {
		t.Run("opaque_cannot_be_"+terminalStatus, func(t *testing.T) {
			fixture := testpg.New(t)
			at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
			digest := sha256.Sum256([]byte("missing constraint stage"))
			issue := identityIssueCandidate(t, security.Head{Generation: 1}, "30000000-0000-4000-8000-000000000086",
				identitySession, digest, at)
			if err := security.NewProjector(fixture.Projector).Apply(t.Context(), issue); err != nil {
				t.Fatal(err)
			}
			setOwnerAccount(t, fixture, identityAccount)
			_, err := fixture.Owner.Exec(t.Context(), `UPDATE sessions SET status=$1,consumed_at=CASE WHEN $1::text='consumed' THEN $2::timestamptz END,
				revoked_at=CASE WHEN $1::text='revoked' THEN $2::timestamptz END,revoke_reason=CASE WHEN $1::text='revoked' THEN 'test_revoked' END
				WHERE session_id=$3`, terminalStatus, at.Add(time.Minute), identitySession)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
				t.Fatalf("opaque terminal status error=%v, want SQLSTATE 23514", err)
			}
		})
	}

	t.Run("bearer_cannot_be_material_missing", func(t *testing.T) {
		fixture := testpg.New(t)
		at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
		projectInitialIdentitySession(t, fixture, "30000000-0000-4000-8000-000000000087", at)
		setOwnerAccount(t, fixture, identityAccount)
		if _, err := fixture.Owner.Exec(t.Context(), `UPDATE sessions SET status='material_missing' WHERE session_id=$1`, identitySession); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23514" {
				return
			}
			t.Fatalf("material-bearing status error=%v, want SQLSTATE 23514", err)
		}
		t.Fatal("material-bearing session accepted material_missing status")
	})
}

func TestFreshIdentityProofRepairsOnlyMaterialMissingProfileAndIdentity(t *testing.T) {
	fixture := testpg.New(t)
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	missingDigest := sha256.Sum256([]byte("missing initial stage"))
	issue := identityIssueCandidate(t, security.Head{Generation: 1}, "30000000-0000-4000-8000-000000000091",
		identitySession, missingDigest, at)
	projector := security.NewProjector(fixture.Projector)
	if err := projector.Apply(t.Context(), issue); err != nil {
		t.Fatal(err)
	}

	repairedName := "Recovered Parent"
	credentialID, credentialKey, credentialPurpose := "apple-reference", "vault.v1", "apple-refresh-revocation-v1"
	credentialObject := "90000000-0000-4000-8000-000000000091"
	repairedSession := "60000000-0000-4000-8000-000000000091"
	repairStage := validExchangeStage(at.Add(time.Minute), repairedSession)
	repairStage.SecurityRevision = 2
	repairStage.RegisterAccount, repairStage.RegisterDevice = false, false
	repairStage.ContinuityKeyID, repairStage.ContinuityDigest = nil, nil
	repairStage.DisplayName = &repairedName
	repairStage.AppleCredentialID, repairStage.AppleCredentialKeyID = &credentialID, &credentialKey
	repairStage.AppleCredentialPurpose, repairStage.AppleCredentialObjectID = &credentialPurpose, &credentialObject
	repairBytes, repairDigest := marshalStage(t, repairStage)
	repairCommand := "30000000-0000-4000-8000-000000000092"
	stageMaterial(t, fixture, repairCommand, repairBytes, repairDigest, at.Add(time.Minute))
	repair := sessionOnlyIdentityIssueCandidate(t, security.Head{Generation: 1, Revision: 1, EventHash: issue.EventHash},
		repairCommand, repairedSession, repairDigest, 1, at.Add(time.Minute))
	if err := projector.Apply(t.Context(), repair); err != nil {
		t.Fatal(err)
	}
	setOwnerAccount(t, fixture, identityAccount)
	var displayName, audience, subject string
	var profileRequired, materialMissing bool
	var gotCredentialID, gotCredentialKey, gotPurpose, gotObject string
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT a.display_name,a.profile_required,i.identity_audience,i.identity_subject,
		i.material_missing,i.apple_credential_id,i.apple_credential_key_id,i.apple_credential_purpose,i.apple_credential_object_id::text
		FROM accounts a JOIN account_identities i ON i.account_id=a.id WHERE a.id=$1`, identityAccount).Scan(
		&displayName, &profileRequired, &audience, &subject, &materialMissing,
		&gotCredentialID, &gotCredentialKey, &gotPurpose, &gotObject); err != nil {
		t.Fatal(err)
	}
	if displayName != repairedName || profileRequired || materialMissing || audience != repairStage.IdentityAudience || subject != repairStage.IdentitySubject ||
		gotCredentialID != credentialID || gotCredentialKey != credentialKey || gotPurpose != credentialPurpose || gotObject != credentialObject {
		t.Fatalf("repair profile=%q/%t identity=%q/%q missing=%t credential=%q/%q/%q/%q",
			displayName, profileRequired, audience, subject, materialMissing, gotCredentialID, gotCredentialKey, gotPurpose, gotObject)
	}

	changedName, changedSubject := "Must Not Replace", "must-not-replace-subject"
	changedCredentialID, changedCredentialKey, changedObject := "other-reference", "vault.v2", "90000000-0000-4000-8000-000000000092"
	nextSession := "60000000-0000-4000-8000-000000000092"
	completeStage := validExchangeStage(at.Add(2*time.Minute), nextSession)
	completeStage.AccessDigest, completeStage.RefreshDigest = repeatHex("21"), repeatHex("22")
	completeStage.SecurityRevision = 3
	completeStage.RegisterAccount, completeStage.RegisterDevice = false, false
	completeStage.ContinuityKeyID, completeStage.ContinuityDigest = nil, nil
	completeStage.DisplayName, completeStage.IdentitySubject = &changedName, changedSubject
	completeStage.AppleCredentialID, completeStage.AppleCredentialKeyID = &changedCredentialID, &changedCredentialKey
	completeStage.AppleCredentialPurpose, completeStage.AppleCredentialObjectID = &credentialPurpose, &changedObject
	completeBytes, completeDigest := marshalStage(t, completeStage)
	completeCommand := "30000000-0000-4000-8000-000000000093"
	stageMaterial(t, fixture, completeCommand, completeBytes, completeDigest, at.Add(2*time.Minute))
	complete := sessionOnlyIdentityIssueCandidate(t, security.Head{Generation: 1, Revision: 2, EventHash: repair.EventHash},
		completeCommand, nextSession, completeDigest, 1, at.Add(2*time.Minute))
	if err := projector.Apply(t.Context(), complete); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT a.display_name,i.identity_subject,i.apple_credential_id,i.apple_credential_key_id,
		i.apple_credential_object_id::text FROM accounts a JOIN account_identities i ON i.account_id=a.id WHERE a.id=$1`, identityAccount).
		Scan(&displayName, &subject, &gotCredentialID, &gotCredentialKey, &gotObject); err != nil {
		t.Fatal(err)
	}
	if displayName != repairedName || subject != repairStage.IdentitySubject || gotCredentialID != credentialID || gotCredentialKey != credentialKey || gotObject != credentialObject {
		t.Fatalf("complete material was overwritten profile=%q subject=%q credential=%q/%q/%q", displayName, subject, gotCredentialID, gotCredentialKey, gotObject)
	}
}

func projectMissingIdentityAndRotation(t *testing.T, fixture *testpg.Fixture, at time.Time) (security.Projection, security.Head, string) {
	t.Helper()
	issueDigest := sha256.Sum256([]byte("missing issue stage"))
	issue := identityIssueCandidate(t, security.Head{Generation: 1}, "30000000-0000-4000-8000-000000000081", identitySession, issueDigest, at)
	projector := security.NewProjector(fixture.Projector)
	if err := projector.Apply(t.Context(), issue); err != nil {
		t.Fatal(err)
	}
	child := "60000000-0000-4000-8000-000000000081"
	rotationDigest := sha256.Sum256([]byte("missing rotation stage"))
	rotate := projectionEventCandidate(t, security.Head{Generation: 1, Revision: 1, EventHash: issue.EventHash},
		"30000000-0000-4000-8000-000000000082", "session_rotated", []security.Effect{{
			Kind: "session_rotated", AccountID: identityAccount, DeviceID: identityDevice, SessionID: child,
			ParentSessionID: identitySession, LineageRootSessionID: identitySession,
			ProjectionMaterialDigest: hex.EncodeToString(rotationDigest[:]), RecoveryEpoch: 1,
			ExpiresAt: at.Add(24 * time.Hour).Format(time.RFC3339Nano), Status: "active",
		}}, at.Add(time.Minute))
	if err := projector.Apply(t.Context(), rotate); err != nil {
		t.Fatalf("missing-stage opaque rotation quarantined projection: %v", err)
	}
	var parentStatus, childStatus string
	var parentConsumed, parentClean, childClean bool
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT p.status,p.consumed_at=$3,p.revoked_at IS NULL,
		c.status,c.consumed_at IS NULL AND c.revoked_at IS NULL
		FROM sessions p JOIN sessions c ON c.session_id=$1 WHERE p.session_id=$2`, child, identitySession, at.Add(time.Minute)).
		Scan(&parentStatus, &parentConsumed, &parentClean, &childStatus, &childClean); err != nil {
		t.Fatal(err)
	}
	if parentStatus != "material_missing" || !parentConsumed || !parentClean || childStatus != "material_missing" || !childClean {
		t.Fatalf("opaque rotation parent=%s consumed=%t clean=%t child=%s clean=%t",
			parentStatus, parentConsumed, parentClean, childStatus, childClean)
	}
	return projector, security.Head{Generation: 1, Revision: 2, EventHash: rotate.EventHash}, child
}

func sessionOnlyIdentityIssueCandidate(t *testing.T, prior security.Head, commandID, sessionID string, digest [32]byte, epoch uint64, at time.Time) security.Candidate {
	t.Helper()
	return projectionEventCandidate(t, prior, commandID, "identity_session_issued", []security.Effect{{
		Kind: "session_granted", AccountID: identityAccount, DeviceID: identityDevice, SessionID: sessionID,
		LineageRootSessionID: sessionID, ServerInstanceID: identityInstance, CanonicalOrigin: "https://sync.example.test",
		IdentityKey: identityKey, DeviceCredentialDigest: continuityKey, ProjectionMaterialDigest: hex.EncodeToString(digest[:]),
		RecoveryEpoch: epoch, ExpiresAt: at.Add(24 * time.Hour).Format(time.RFC3339Nano), Status: "active",
	}}, at)
}

func TestIdentityAndDeviceRevocationAreImplicitSetWise(t *testing.T) {
	t.Run("identity revokes sessions but preserves device", func(t *testing.T) {
		fixture := testpg.New(t)
		at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
		issue := projectInitialIdentitySession(t, fixture, "30000000-0000-4000-8000-000000000041", at)
		revoke := projectionEventCandidate(t, security.Head{Generation: 1, Revision: 1, EventHash: issue.EventHash},
			"30000000-0000-4000-8000-000000000042", "identity_unavailable", []security.Effect{{
				Kind: "identity_unavailable", AccountID: identityAccount, IdentityKey: identityKey, Status: "identity_unavailable",
			}}, at.Add(time.Minute))
		if err := security.NewProjector(fixture.Projector).Apply(t.Context(), revoke); err != nil {
			t.Fatal(err)
		}
		var revoked int
		var bindingRevoked bool
		if err := fixture.Owner.QueryRow(t.Context(), `SELECT
			(SELECT count(*) FROM sessions WHERE account_id=$1 AND status='revoked'),
			(SELECT binding_revoked_at IS NOT NULL FROM devices WHERE id=$2)`, identityAccount, identityDevice).Scan(&revoked, &bindingRevoked); err != nil {
			t.Fatal(err)
		}
		if revoked != 1 || bindingRevoked {
			t.Fatalf("identity revocation sessions=%d binding_revoked=%t", revoked, bindingRevoked)
		}
	})

	t.Run("device revokes binding and every device session", func(t *testing.T) {
		fixture := testpg.New(t)
		at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
		issue := projectInitialIdentitySession(t, fixture, "30000000-0000-4000-8000-000000000051", at)
		revoke := projectionEventCandidate(t, security.Head{Generation: 1, Revision: 1, EventHash: issue.EventHash},
			"30000000-0000-4000-8000-000000000052", "device_binding_revoked", []security.Effect{{
				Kind: "device_binding_revoked", AccountID: identityAccount, DeviceID: identityDevice,
				ServerInstanceID: identityInstance, CanonicalOrigin: "https://sync.example.test",
				DeviceCredentialDigest: continuityKey, Status: "revoked",
			}}, at.Add(time.Minute))
		if err := security.NewProjector(fixture.Projector).Apply(t.Context(), revoke); err != nil {
			t.Fatal(err)
		}
		var revoked int
		var bindingRevoked bool
		if err := fixture.Owner.QueryRow(t.Context(), `SELECT
			(SELECT count(*) FROM sessions WHERE device_id=$1 AND status='revoked'),
			(SELECT binding_revoked_at IS NOT NULL FROM devices WHERE id=$1)`, identityDevice).Scan(&revoked, &bindingRevoked); err != nil {
			t.Fatal(err)
		}
		if revoked != 1 || !bindingRevoked {
			t.Fatalf("device revocation sessions=%d binding_revoked=%t", revoked, bindingRevoked)
		}
	})
}

func projectInitialIdentitySession(t *testing.T, fixture *testpg.Fixture, commandID string, at time.Time) security.Candidate {
	t.Helper()
	issueBytes, issueDigest := marshalStage(t, validExchangeStage(at, identitySession))
	stageMaterial(t, fixture, commandID, issueBytes, issueDigest, at)
	issue := identityIssueCandidate(t, security.Head{Generation: 1}, commandID, identitySession, issueDigest, at)
	if err := security.NewProjector(fixture.Projector).Apply(t.Context(), issue); err != nil {
		t.Fatal(err)
	}
	return issue
}

func validExchangeStage(at time.Time, sessionID string) exchangeStage {
	name, continuityID, continuityDigest := "Parent", "device.v1", repeatHex("bb")
	return exchangeStage{
		Version: "v1", Kind: "exchange", AccountID: identityAccount, DeviceID: identityDevice, SessionID: sessionID,
		LineageRootSessionID: sessionID, ServerInstanceID: identityInstance, CanonicalOrigin: "https://sync.example.test",
		TokenKeyID: "session.v1", AccessDigest: repeatHex("11"), RefreshDigest: repeatHex("12"), RecoveryEpoch: 1,
		SecurityRevision: 1, Scope: "account", AccessExpiresAt: at.Add(10 * time.Minute).Format(time.RFC3339Nano),
		RefreshExpiresAt: at.Add(time.Hour).Format(time.RFC3339Nano), ReauthenticateAt: at.Add(24 * time.Hour).Format(time.RFC3339Nano),
		AbsoluteExpiresAt: at.Add(24 * time.Hour).Format(time.RFC3339Nano), IdentityProvider: "apple", IdentityKeyID: "identity.v1",
		IdentityDigest: repeatHex("aa"), IdentityAudience: "com.example.babytracker", IdentitySubject: "subject-fixture",
		DisplayName: &name, ProfileConfirmed: true, RegisterAccount: true, RegisterDevice: true,
		ContinuityKeyID: &continuityID, ContinuityDigest: &continuityDigest,
	}
}

func validRefreshStage(at time.Time, sessionID, parentSessionID string, securityRevision uint64) refreshStage {
	return refreshStage{
		Version: "v1", Kind: "refresh", AccountID: identityAccount, DeviceID: identityDevice, SessionID: sessionID,
		LineageRootSessionID: identitySession, ServerInstanceID: identityInstance, CanonicalOrigin: "https://sync.example.test",
		TokenKeyID: "session.v1", AccessDigest: repeatHex("33"), RefreshDigest: repeatHex("44"), RecoveryEpoch: 1,
		SecurityRevision: securityRevision, Scope: "account", AccessExpiresAt: at.Add(19 * time.Minute).Format(time.RFC3339Nano),
		RefreshExpiresAt: at.Add(119 * time.Minute).Format(time.RFC3339Nano), ReauthenticateAt: at.Add(23*time.Hour + 59*time.Minute).Format(time.RFC3339Nano),
		AbsoluteExpiresAt: at.Add(23*time.Hour + 59*time.Minute).Format(time.RFC3339Nano), ParentSessionID: parentSessionID,
	}
}

func marshalStage(t *testing.T, value any) ([]byte, [32]byte) {
	t.Helper()
	bytes, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return bytes, sha256.Sum256(bytes)
}

func stageMaterial(t *testing.T, fixture *testpg.Fixture, commandID string, bytes []byte, digest [32]byte, at time.Time) {
	t.Helper()
	commandHash, _ := hex.DecodeString(repeatHex("aa"))
	if _, err := fixture.Runtime.Exec(t.Context(), "SELECT app.stage_auth_material($1,$2,$3,$4,$5)",
		commandID, commandHash, digest[:], bytes, at); err != nil {
		t.Fatal(err)
	}
}

func setOwnerAccount(t *testing.T, fixture *testpg.Fixture, accountID string) {
	t.Helper()
	if _, err := fixture.Owner.Exec(t.Context(), "SELECT set_config('app.account_id',$1,false)", accountID); err != nil {
		t.Fatal(err)
	}
}

func identityIssueCandidate(t *testing.T, prior security.Head, commandID, sessionID string, digest [32]byte, at time.Time) security.Candidate {
	t.Helper()
	return projectionEventCandidate(t, prior, commandID, "identity_session_issued", []security.Effect{
		{Kind: "account_registered", AccountID: identityAccount, IdentityKey: identityKey, Status: "active"},
		{Kind: "device_registered", AccountID: identityAccount, DeviceID: identityDevice, ServerInstanceID: identityInstance,
			CanonicalOrigin: "https://sync.example.test", DeviceCredentialDigest: continuityKey, Status: "active"},
		{Kind: "session_granted", AccountID: identityAccount, DeviceID: identityDevice, SessionID: sessionID,
			LineageRootSessionID: sessionID, ServerInstanceID: identityInstance, CanonicalOrigin: "https://sync.example.test",
			IdentityKey: identityKey, DeviceCredentialDigest: continuityKey, ProjectionMaterialDigest: hex.EncodeToString(digest[:]),
			RecoveryEpoch: 1, ExpiresAt: at.Add(24 * time.Hour).Format(time.RFC3339Nano), Status: "active"},
	}, at)
}

func repeatHex(pair string) string {
	result := ""
	for i := 0; i < 32; i++ {
		result += pair
	}
	return result
}
