package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/identity"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	repositoryAccount  = "10000000-0000-4000-8000-000000000071"
	repositoryOther    = "10000000-0000-4000-8000-000000000072"
	repositoryDevice   = "40000000-0000-4000-8000-000000000071"
	repositorySession  = "60000000-0000-4000-8000-000000000071"
	repositoryInstance = "50000000-0000-4000-8000-000000000071"
	repositoryOrigin   = "https://sync.example.test"
)

func TestAuthRepositoryHasOnlyTypedOperations(t *testing.T) {
	if _, err := NewRepository(nil); err == nil {
		t.Fatal("nil runtime pool accepted")
	}
	want := []string{"AcknowledgeRecovery", "ConsumeChallenge", "CreateChallenge", "LineageStartedAt", "LookupAccessSession", "LookupAuthIdentity", "LookupDeviceBinding", "LookupRecoveryAcknowledgements", "LookupRecoveryChild", "LookupRecoveryReceipt", "LookupRefreshSession", "RecoveryKeyCandidates", "ReferencedRecoveryKeyIDs", "ReserveAppleNotification", "StageAuthMaterial"}
	typeOfRepository := reflect.TypeOf((*Repository)(nil))
	got := make([]string, 0, typeOfRepository.NumMethod())
	for index := 0; index < typeOfRepository.NumMethod(); index++ {
		got = append(got, typeOfRepository.Method(index).Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("repository methods=%v want=%v", got, want)
	}
}

func TestAuthRepositoryCallsFrozenRuntimeFunctions(t *testing.T) {
	fixture := testpg.New(t)
	now := time.Date(2026, 9, 20, 12, 0, 0, 123456789, time.UTC)
	data := seedAuthRepository(t, fixture, now)
	repository, err := NewRepository(fixture.Runtime)
	if err != nil {
		t.Fatal(err)
	}

	challengeDigest := sha256.Sum256([]byte("raw-challenge-secret"))
	challenge := ChallengeRecord{Digest: challengeDigest, CanonicalOrigin: repositoryOrigin, ServerInstanceID: repositoryInstance, ExpiresAt: now.Add(5 * time.Minute), Now: now}
	challengeID, err := repository.CreateChallenge(t.Context(), challenge)
	if err != nil || challengeID == "" {
		t.Fatalf("challenge id=%q err=%v", challengeID, err)
	}
	consumedID, err := repository.ConsumeChallenge(t.Context(), ChallengeConsumption{Digest: challengeDigest, CanonicalOrigin: repositoryOrigin, ServerInstanceID: repositoryInstance, Now: now.Add(time.Second)})
	if err != nil || consumedID != challengeID {
		t.Fatalf("consumed id=%q want=%q err=%v", consumedID, challengeID, err)
	}
	if _, err := repository.ConsumeChallenge(t.Context(), ChallengeConsumption{Digest: challengeDigest, CanonicalOrigin: repositoryOrigin, ServerInstanceID: repositoryInstance, Now: now.Add(2 * time.Second)}); err == nil {
		t.Fatal("consumed challenge was reusable")
	}

	identityRow, found, err := repository.LookupAuthIdentity(t.Context(), "apple", "identity.v1", data.identityDigest)
	if err != nil || !found || identityRow.AccountID != repositoryAccount || identityRow.AuthorityStatus != "active" || identityRow.DisplayName != "Parent" || identityRow.ProfileRequired {
		t.Fatalf("identity=%+v found=%t err=%v", identityRow, found, err)
	}
	device, found, err := repository.LookupDeviceBinding(t.Context(), repositoryAccount, repositoryDevice, repositoryInstance, repositoryOrigin)
	if err != nil || !found || device.ContinuityKeyID != "device.v1" || device.ContinuityDigest != data.continuityDigest || device.Revoked {
		t.Fatalf("device=%+v found=%t err=%v", device, found, err)
	}
	if _, found, err := repository.LookupDeviceBinding(t.Context(), repositoryOther, repositoryDevice, repositoryInstance, repositoryOrigin); err != nil || found {
		t.Fatalf("cross-account device found=%t err=%v", found, err)
	}

	access, found, err := repository.LookupAccessSession(t.Context(), "session.v1", data.accessDigest)
	if err != nil || !found || access.SessionID != repositorySession || access.AccountID != repositoryAccount || access.DeviceID != repositoryDevice ||
		access.LineageRootSessionID != repositorySession || access.RecoveryEpoch != 1 || access.SecurityRevision != 7 || access.Status != "active" || access.FamilyID != nil {
		t.Fatalf("access=%+v found=%t err=%v", access, found, err)
	}
	if access.AccessExpiresAt.Location() != time.UTC || access.RefreshExpiresAt.Location() != time.UTC ||
		access.ReauthenticateAt.Location() != time.UTC || access.AbsoluteExpiresAt.Location() != time.UTC {
		t.Fatalf("access deadlines are not normalized to UTC: %+v", access)
	}
	refresh, found, err := repository.LookupRefreshSession(t.Context(), "session.v1", data.refreshDigest)
	if err != nil || !found || refresh.SessionID != repositorySession || refresh.ParentSessionID != nil || refresh.Status != "active" || refresh.RefreshConsumedAt != nil {
		t.Fatalf("refresh=%+v found=%t err=%v", refresh, found, err)
	}
	if refresh.AccessExpiresAt.Location() != time.UTC || refresh.RefreshExpiresAt.Location() != time.UTC ||
		refresh.ReauthenticateAt.Location() != time.UTC || refresh.AbsoluteExpiresAt.Location() != time.UTC {
		t.Fatalf("refresh deadlines are not normalized to UTC: %+v", refresh)
	}

	stage := validExchangeStage()
	stage.AccountID, stage.DeviceID, stage.SessionID, stage.LineageRootSessionID = repositoryAccount, repositoryDevice,
		"60000000-0000-4000-8000-000000000072", "60000000-0000-4000-8000-000000000072"
	stage.ServerInstanceID, stage.CanonicalOrigin = repositoryInstance, repositoryOrigin
	stage.SecurityRevision = 8
	stage.AccessExpiresAt = now.Add(10 * time.Minute).Format(time.RFC3339Nano)
	stage.RefreshExpiresAt = now.Add(time.Hour).Format(time.RFC3339Nano)
	stage.ReauthenticateAt = now.Add(24 * time.Hour).Format(time.RFC3339Nano)
	stage.AbsoluteExpiresAt = stage.ReauthenticateAt
	stage.IdentityDigest = hex.EncodeToString(data.identityDigest[:])
	stage.RegisterDevice = true
	stage.ContinuityKeyID = repositoryStringPointer("device.v1")
	stage.ContinuityDigest = repositoryStringPointer(hex.EncodeToString(data.continuityDigest[:]))
	material, materialDigest, err := EncodeExchangeStage(stage)
	if err != nil {
		t.Fatal(err)
	}
	commandHash := sha256.Sum256([]byte("authority-command"))
	staged := AuthMaterialStage{CommandID: "30000000-0000-4000-8000-000000000071", CommandHash: commandHash,
		ProjectionMaterialDigest: materialDigest, Material: append([]byte(nil), material...), Now: now}
	if err := repository.StageAuthMaterial(t.Context(), staged); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(staged.Material, material) {
		t.Fatal("repository mutated exact stage bytes")
	}
	var storedDigest, storedAccess []byte
	var storedKind string
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT projection_material_digest,access_digest,material_kind FROM auth_exchange_material WHERE command_id=$1`, staged.CommandID).
		Scan(&storedDigest, &storedAccess, &storedKind); err != nil || !reflect.DeepEqual(storedDigest, materialDigest[:]) || storedKind != "exchange" {
		t.Fatalf("stored digest=%x access=%x kind=%q err=%v", storedDigest, storedAccess, storedKind, err)
	}
	refreshStage := RefreshStage{StageCommon: stage.StageCommon, ParentSessionID: repositorySession}
	refreshStage.Kind = "refresh"
	refreshStage.SessionID = "60000000-0000-4000-8000-000000000073"
	refreshStage.LineageRootSessionID = repositorySession
	refreshBytes, refreshMaterialDigest, err := EncodeRefreshStage(refreshStage)
	if err != nil {
		t.Fatal(err)
	}
	refreshStaged := AuthMaterialStage{CommandID: "30000000-0000-4000-8000-000000000076", CommandHash: sha256.Sum256([]byte("refresh-command")),
		ProjectionMaterialDigest: refreshMaterialDigest, Material: append([]byte(nil), refreshBytes...), Now: now}
	if err := repository.StageAuthMaterial(t.Context(), refreshStaged); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT projection_material_digest,material_kind FROM auth_exchange_material WHERE command_id=$1`, refreshStaged.CommandID).
		Scan(&storedDigest, &storedKind); err != nil || !reflect.DeepEqual(storedDigest, refreshMaterialDigest[:]) || storedKind != "refresh" {
		t.Fatalf("stored refresh digest=%x kind=%q err=%v", storedDigest, storedKind, err)
	}

	notificationCurrent := identity.KeyedDigest{KeyID: "notify.v2", Sum: sha256.Sum256([]byte("notification-current"))}
	notificationPrevious := identity.KeyedDigest{KeyID: "notify.v1", Sum: sha256.Sum256([]byte("notification-previous"))}
	commandID := "30000000-0000-4000-8000-000000000072"
	gotCommand, fresh, err := repository.ReserveAppleNotification(t.Context(), []identity.KeyedDigest{notificationCurrent, notificationPrevious}, commandID, now)
	if err != nil || !fresh || gotCommand != commandID {
		t.Fatalf("notification command=%q fresh=%t err=%v", gotCommand, fresh, err)
	}

	restarted, err := NewRepository(fixture.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	if got, fresh, err := restarted.ReserveAppleNotification(t.Context(), []identity.KeyedDigest{notificationCurrent, notificationPrevious},
		"30000000-0000-4000-8000-000000000073", now.Add(time.Second)); err != nil || fresh || got != commandID {
		t.Fatalf("restart reservation command=%q fresh=%t err=%v", got, fresh, err)
	}
	if got, found, err := restarted.LookupRefreshSession(t.Context(), "session.v1", data.refreshDigest); err != nil || !found || got.SessionID != repositorySession {
		t.Fatalf("restart refresh=%+v found=%t err=%v", got, found, err)
	}
	if err := restarted.StageAuthMaterial(t.Context(), refreshStaged); err != nil {
		t.Fatalf("exact staged material was not restart-idempotent: %v", err)
	}
	changed := refreshStaged
	changed.Material = append([]byte(nil), refreshStaged.Material...)
	changed.Material[len(changed.Material)-2] ^= 1
	changed.ProjectionMaterialDigest = sha256.Sum256(changed.Material)
	if err := restarted.StageAuthMaterial(t.Context(), changed); err == nil {
		t.Fatal("changed stage bytes reused command ID")
	}

	var persisted string
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT concat_ws('|',
		coalesce((SELECT encode(token_digest,'hex') FROM auth_challenges WHERE id=$1),''),
		coalesce((SELECT row_to_json(m)::text FROM auth_exchange_material m WHERE command_id=$2),''),
		coalesce((SELECT row_to_json(s)::text FROM sessions s WHERE session_id=$3),''))`, challengeID, staged.CommandID, repositorySession).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"raw-challenge-secret", data.rawAccess, data.rawRefresh, data.rawContinuity} {
		if strings.Contains(persisted, raw) {
			t.Fatalf("raw secret %q persisted", raw)
		}
	}
}

func TestAuthRepositoryProjectorCannotCallRuntimeFunctions(t *testing.T) {
	fixture := testpg.New(t)
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	repository, err := NewRepository(fixture.Projector)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("projector-denied"))
	stage := validExchangeStage()
	material, materialDigest, err := EncodeExchangeStage(stage)
	if err != nil {
		t.Fatal(err)
	}
	commandHash := sha256.Sum256([]byte("projector-command"))
	calls := map[string]func() error{
		"create challenge": func() error {
			_, err := repository.CreateChallenge(t.Context(), ChallengeRecord{Digest: digest, CanonicalOrigin: repositoryOrigin, ServerInstanceID: repositoryInstance, ExpiresAt: now.Add(5 * time.Minute), Now: now})
			return err
		},
		"consume challenge": func() error {
			_, err := repository.ConsumeChallenge(t.Context(), ChallengeConsumption{Digest: digest, CanonicalOrigin: repositoryOrigin, ServerInstanceID: repositoryInstance, Now: now})
			return err
		},
		"identity lookup": func() error {
			_, _, err := repository.LookupAuthIdentity(t.Context(), "apple", "identity.v1", digest)
			return err
		},
		"device lookup": func() error {
			_, _, err := repository.LookupDeviceBinding(t.Context(), repositoryAccount, repositoryDevice, repositoryInstance, repositoryOrigin)
			return err
		},
		"access lookup": func() error {
			_, _, err := repository.LookupAccessSession(t.Context(), "session.v1", digest)
			return err
		},
		"refresh lookup": func() error {
			_, _, err := repository.LookupRefreshSession(t.Context(), "session.v1", digest)
			return err
		},
		"stage material": func() error {
			return repository.StageAuthMaterial(t.Context(), AuthMaterialStage{CommandID: "30000000-0000-4000-8000-000000000074", CommandHash: commandHash, ProjectionMaterialDigest: materialDigest, Material: material, Now: now})
		},
		"reserve notification": func() error {
			_, _, err := repository.ReserveAppleNotification(t.Context(), []identity.KeyedDigest{{KeyID: "notify.v1", Sum: digest}}, "30000000-0000-4000-8000-000000000075", now)
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			var databaseError *pgconn.PgError
			if err := call(); !errors.As(err, &databaseError) || databaseError.Code != "42501" {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

type repositorySeed struct {
	identityDigest, continuityDigest, accessDigest, refreshDigest [32]byte
	rawAccess, rawRefresh, rawContinuity                          string
}

func seedAuthRepository(t *testing.T, fixture *testpg.Fixture, now time.Time) repositorySeed {
	t.Helper()
	data := repositorySeed{rawAccess: "raw-access-token", rawRefresh: "raw-refresh-token", rawContinuity: "raw-continuity-secret"}
	data.identityDigest = sha256.Sum256([]byte("identity-material"))
	data.continuityDigest = sha256.Sum256([]byte(data.rawContinuity))
	data.accessDigest = sha256.Sum256([]byte(data.rawAccess))
	data.refreshDigest = sha256.Sum256([]byte(data.rawRefresh))
	authorityIdentity := "hmac-sha256:identity.v1:" + hex.EncodeToString(data.identityDigest[:])
	owner, err := fixture.Owner.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()
	if _, err := owner.Exec(t.Context(), `SELECT set_config('app.account_id',$1,false)`, repositoryAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), `INSERT INTO accounts(id,display_name,profile_required,created_at,updated_at)
		VALUES($1,'Parent',false,$2,$2)`, repositoryAccount, now); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), `INSERT INTO account_security_state(account_id,identity_key,status,generation,revision,event_hash)
		VALUES($1,$2,'active',1,7,decode(repeat('71',32),'hex'))`, repositoryAccount, authorityIdentity); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), `INSERT INTO account_identities(provider,identity_key_id,identity_digest,account_id,identity_audience,identity_subject,created_at,updated_at)
		VALUES('apple','identity.v1',$2,$1,'com.example.babytracker','subject',$3,$3)`, repositoryAccount, data.identityDigest[:], now); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), `INSERT INTO devices(id,account_id,server_instance_id,canonical_origin,continuity_key_id,continuity_digest,created_at,updated_at)
		VALUES($1,$2,$3,$4,'device.v1',$5,$6,$6)`, repositoryDevice, repositoryAccount, repositoryInstance, repositoryOrigin, data.continuityDigest[:], now); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), `INSERT INTO sessions(session_id,account_id,device_id,lineage_root_session_id,parent_session_id,server_instance_id,
		recovery_epoch,security_revision,scope,family_id,token_key_id,access_digest,refresh_digest,access_expires_at,refresh_expires_at,
		reauthenticate_at,absolute_expires_at,status,created_at,updated_at)
		VALUES($1,$2,$3,$1,NULL,$4,1,7,'account',NULL,'session.v1',$5,$6,$7,$8,$9,$9,'active',$10,$10)`,
		repositorySession, repositoryAccount, repositoryDevice, repositoryInstance, data.accessDigest[:], data.refreshDigest[:],
		now.Add(10*time.Minute), now.Add(time.Hour), now.Add(24*time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), `SELECT set_config('app.account_id',$1,false)`, repositoryOther); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), `INSERT INTO accounts(id,display_name,profile_required,created_at,updated_at)
		VALUES($1,'Other',false,$2,$2)`, repositoryOther, now); err != nil {
		t.Fatal(err)
	}
	return data
}

func repositoryStringPointer(value string) *string { return &value }
