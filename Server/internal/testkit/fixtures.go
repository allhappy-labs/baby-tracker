package testkit

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/config"
	"babytracker/server/internal/family"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/importer"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/records"
	"babytracker/server/internal/security"
	"babytracker/server/internal/security/localstore"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/internal/tenant"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testOrigin = "https://sync.example.test"
const testInstance = "50000000-0000-4000-8000-000000000001"

const (
	TimerA  = "50000000-0000-4000-8000-000000000101"
	TimerB  = "50000000-0000-4000-8000-000000000102"
	RecordA = "20000000-0000-4000-8000-000000000101"
	RecordB = "20000000-0000-4000-8000-000000000102"
)

type Harness struct {
	t                                                   *testing.T
	Owner, Caregiver, Outsider                          access.Principal
	FamilyID                                            string
	Service                                             *familysync.Service
	Importer                                            *importer.Service
	Admin, Runtime, Migrator, Maintenance, LiveActivity *pgxpool.Pool
	Auth                                                *auth.Service
	Identity                                            *identity.Service
	Family                                              *family.Service
	Security                                            *security.Service
	Info                                                protocol.ServerInfo
	Limits                                              protocol.Limits
	Now                                                 time.Time
	ids                                                 atomic.Uint64
	bundles                                             map[string]auth.SessionBundle
	deviceCredentials                                   map[string]string
	commitObserver                                      familysync.CommitObserver
}

type PendingWake struct {
	FamilyID, DeviceID string
	Cursor             protocol.Cursor
	NotBefore          time.Time
	Attempts           uint32
	Generation         uint64
}

type hostedQueueObserver struct{ publish bool }

func (observer *hostedQueueObserver) Schedule(ctx context.Context, tx pgx.Tx, grant access.Grant, deviceID string, cursor protocol.Cursor) error {
	if _, err := tx.Exec(ctx, `SELECT app.schedule_family_wakes($1,$2,$3,$4,$5)`, grant.FamilyID, deviceID,
		cursor.ServerInstanceID, cursor.RecoveryEpoch, cursor.Sequence); err != nil {
		return err
	}
	payload, err := json.Marshal(protocol.Hint{FamilyID: grant.FamilyID, Cursor: cursor})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `SELECT pg_notify('family_cursor_v1',$1)`, string(payload))
	return err
}
func (*hostedQueueObserver) Publish(context.Context, string, protocol.Cursor) error { return nil }

type familyResolver struct{ service *family.Service }

func (r familyResolver) Current(ctx context.Context, p access.Principal) (string, error) {
	view, err := r.service.Current(ctx, p)
	return view.ID, err
}

func New(t *testing.T) *Harness {
	return newHarness(t, "", false)
}

// NewThrough builds the harness on a database migrated only up to version, so
// a test can hold state written at an older schema and then apply the next
// migrations itself as h.Migrator, the table-owning role production migrates as.
func NewThrough(t *testing.T, version int64) *Harness {
	return newHarnessOn(t, NewPostgresThrough(t, version), "", false)
}

func NewHosted(t *testing.T) *Harness {
	return newHarness(t, "", true)
}

func NewWithUnrefreshedOwner(t *testing.T) *Harness {
	return newHarness(t, "owner", false)
}

func NewWithUnrefreshedCaregiver(t *testing.T) *Harness {
	return newHarness(t, "caregiver", false)
}

func newHarness(t *testing.T, unrefreshedSubject string, hosted bool) *Harness {
	t.Helper()
	return newHarnessOn(t, NewPostgres(t), unrefreshedSubject, hosted)
}

func newHarnessOn(t *testing.T, db Postgres, unrefreshedSubject string, hosted bool) *Harness {
	t.Helper()
	h := &Harness{t: t, Admin: db.Admin, Runtime: db.Runtime, Migrator: db.Migrator, Maintenance: db.Maintenance, LiveActivity: db.LiveActivity, Now: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC), bundles: map[string]auth.SessionBundle{}, deviceCredentials: map[string]string{}}
	store, err := localstore.Open(filepath.Join(t.TempDir(), "ledger"), filepath.Join(t.TempDir(), "head"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.InitializeEmpty(t.Context()); err != nil {
		t.Fatal(err)
	}
	verifier := identity.IdentityVerifierFunc(func(_ context.Context, token, nonce string) (identity.VerifiedIdentity, error) {
		subject, bound, ok := strings.Cut(token, "|")
		if !ok || subject == "" || bound != nonce {
			return identity.VerifiedIdentity{}, fmt.Errorf("nonce mismatch")
		}
		return identity.VerifiedIdentity{Provider: "apple", Audience: "com.example.babytracker", Subject: subject, IssuedAt: h.Now.Add(-time.Minute), ExpiresAt: h.Now.Add(time.Hour)}, nil
	})
	idRepo, err := identity.NewRepository(db.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	h.Identity, err = identity.New(identity.Options{Verifier: verifier, Repository: idRepo, Origin: testOrigin, InstanceID: testInstance, IdentityKeys: keyring("identity.v1", 0x11), Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	var familyService *family.Service
	authority, err := security.New(security.Options{Ledger: store.Ledger, Heads: store.Heads, Projection: security.NewProjector(db.Projector), Fence: store.Fence, Now: func() time.Time { return h.Now }, Runtime: db.Runtime, Epoch: func(context.Context) (uint64, error) { return 1, nil }, Decide: func(ctx context.Context, p access.Principal, familyID, action string) (access.Grant, error) {
		if familyService == nil {
			return access.Grant{}, security.ErrUnavailable
		}
		return familyService.Authorize(ctx, p, familyID, action)
	}, MaxCaregivers: 10, MaxInvitations: 10, RequireFamilySyncEntitlement: hosted})
	if err != nil {
		t.Fatal(err)
	}
	h.Security = authority
	authRepo, err := auth.NewRepository(db.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	var actor [32]byte
	for i := range actor {
		actor[i] = 0x55
	}
	h.Auth, err = auth.New(auth.Options{Identity: h.Identity, Verifier: verifier, Security: authority, Repository: authRepo, Mode: config.ModeSelfHosted, Origin: testOrigin, InstanceID: testInstance, TokenKeys: keyring("session.v1", 0x22), DeviceKeys: keyring("device.v1", 0x33), NotificationKeys: keyring("notification.v1", 0x44), ActorIDKey: actor, Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	familyRepo, err := family.NewRepository(db.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	var entitlementPolicy family.EntitlementPolicy = family.SelfHostedEntitlementPolicy{}
	if hosted {
		entitlementPolicy = family.HostedEntitlementPolicy{Security: authority}
	}
	familyService, err = family.New(family.Options{Security: authority, Repository: familyRepo, Entitlements: entitlementPolicy, InvitationKeys: keyring("invitation.v1", 0x66), Origin: testOrigin, Now: func() time.Time { return h.Now }, MaxCaregivers: 10, MaxInvitations: 10, InvitationTTL: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h.Family = familyService
	h.Owner = h.signIn("owner", "Owner")
	created, err := h.Family.Create(t.Context(), h.Owner, h.commandID(), "Family")
	if err != nil {
		t.Fatal(err)
	}
	h.FamilyID = created.ID
	h.Owner = h.authenticate("owner")
	if unrefreshedSubject != "owner" {
		h.Owner = h.refresh("owner")
	}
	h.Caregiver = h.signIn("caregiver", "Caregiver")
	invite, err := h.Family.Invite(t.Context(), h.Owner, h.commandID(), "caregiver")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Family.Redeem(t.Context(), h.Caregiver, h.commandID(), invite.Token, true); err != nil {
		t.Fatal(err)
	}
	h.Caregiver = h.authenticate("caregiver")
	if unrefreshedSubject != "caregiver" {
		h.Caregiver = h.refresh("caregiver")
	}
	h.Outsider = h.signIn("outsider", "Outsider")
	if hosted {
		head, headErr := authority.Head(t.Context())
		if headErr != nil {
			t.Fatal(headErr)
		}
		if _, mirrorErr := db.Projector.Exec(t.Context(), `SELECT app.update_server_state($1,$2,$3,$4,$5)`, testInstance, 1,
			head.Generation, head.Revision, head.EventHash[:]); mirrorErr != nil {
			t.Fatal(mirrorErr)
		}
	}
	limits := protocol.DefaultLimits()
	info := protocol.ServerInfo{InstanceID: testInstance, RecoveryEpoch: 1, CanonicalOrigin: testOrigin, APIVersions: []string{"v1"}, Capabilities: protocol.ServerCapabilities(), AppleAuthorizationMode: "identity_token_only", Limits: &limits}
	h.Info, h.Limits = info, limits
	if hosted {
		h.commitObserver = &hostedQueueObserver{publish: true}
	}
	h.Service = familysync.NewServiceWithPaging(authority, records.NewRepository(), info, limits, func() time.Time { return h.Now }, familyResolver{h.Family}, h.commitObserver, keyring("page.v1", 0x77))
	h.Importer = importer.NewService(authority, records.NewRepository(), familyResolver{h.Family}, info, limits, keyring("page.v1", 0x77), func() time.Time { return h.Now })
	h.Importer.SetCommitObserver(h.commitObserver)
	h.Service.SetImportAdmission(h.Importer)
	return h
}

func (h *Harness) authenticate(subject string) access.Principal {
	p, err := h.Auth.Authenticate(h.t.Context(), h.bundles[subject].AccessToken)
	if err != nil {
		h.t.Fatalf("authenticate %s: %v", subject, err)
	}
	return p
}

func keyring(id string, fill byte) identity.DigestKeyring {
	k := identity.DigestKey{ID: id}
	for i := range k.Secret {
		k.Secret[i] = fill
	}
	return identity.DigestKeyring{Current: k}
}
func (h *Harness) commandID() string {
	return fmt.Sprintf("30000000-0000-4000-8000-%012x", h.ids.Add(1))
}
func (h *Harness) CommandID() string { return h.commandID() }

// AccessToken returns a private fixture credential for real HTTP integration
// tests. Callers must never include it in test diagnostics.
func (h *Harness) AccessToken(subject string) string { return h.bundles[subject].AccessToken }

// RefreshToken returns a private fixture credential for protocol misuse tests.
// Callers must never include it in test diagnostics.
func (h *Harness) RefreshToken(subject string) string { return h.bundles[subject].RefreshToken }

// DeviceCredential returns private fixture continuity material for protocol
// client-state tests. Callers must never include it in test diagnostics.
func (h *Harness) DeviceCredential(subject string) string { return h.deviceCredentials[subject] }

// ConfigureSignedIdentity replaces only the identity/auth service adapters.
// Existing durable sessions remain valid because this uses the fixture's same
// repositories and key material, while subsequent exchanges exercise the
// supplied verifier.
func (h *Harness) ConfigureSignedIdentity(t *testing.T, verifier identity.IdentityVerifier) {
	t.Helper()
	identityRepository, err := identity.NewRepository(h.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	h.Identity, err = identity.New(identity.Options{Verifier: verifier, Repository: identityRepository, Origin: testOrigin,
		InstanceID: testInstance, IdentityKeys: keyring("identity.v1", 0x11), Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	authRepository, err := auth.NewRepository(h.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	var actor [32]byte
	for index := range actor {
		actor[index] = 0x55
	}
	h.Auth, err = auth.New(auth.Options{Identity: h.Identity, Verifier: verifier, Security: h.Security, Repository: authRepository,
		Mode: config.ModeSelfHosted, Origin: testOrigin, InstanceID: testInstance, TokenKeys: keyring("session.v1", 0x22),
		DeviceKeys: keyring("device.v1", 0x33), NotificationKeys: keyring("notification.v1", 0x44), ActorIDKey: actor,
		Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
}
func (h *Harness) signIn(subject, name string) access.Principal {
	challenge, err := h.Identity.Challenge(h.t.Context())
	if err != nil {
		h.t.Fatal(err)
	}
	nonce, err := identity.AppleNonce(testOrigin, testInstance, challenge.Challenge)
	if err != nil {
		h.t.Fatal(err)
	}
	bundle, err := h.Auth.Exchange(h.t.Context(), identity.ExchangeRequest{CommandID: h.commandID(), Challenge: challenge.Challenge, IdentityToken: subject + "|" + nonce, InstallationID: "installation-" + subject, Profile: &identity.ProfileInput{DisplayName: name, Confirmed: true}})
	if err != nil {
		h.t.Fatal(err)
	}
	h.bundles[subject] = bundle
	h.deviceCredentials[subject] = bundle.DeviceCredential
	p, err := h.Auth.Authenticate(h.t.Context(), bundle.AccessToken)
	if err != nil {
		h.t.Fatal(err)
	}
	return p
}
func (h *Harness) refresh(subject string) access.Principal {
	bundle, err := h.Auth.Refresh(h.t.Context(), h.bundles[subject].RefreshToken)
	if err != nil {
		h.t.Fatalf("refresh %s: %v", subject, err)
	}
	h.bundles[subject] = bundle
	p, err := h.Auth.Authenticate(h.t.Context(), bundle.AccessToken)
	if err != nil {
		h.t.Fatal(err)
	}
	return p
}
func (h *Harness) Refresh(subject string) access.Principal { return h.refresh(subject) }
func (h *Harness) Reauthenticate(subject string) (access.Principal, error) {
	old := h.bundles[subject]
	challenge, err := h.Identity.Challenge(h.t.Context())
	if err != nil {
		return access.Principal{}, err
	}
	nonce, err := identity.AppleNonce(testOrigin, testInstance, challenge.Challenge)
	if err != nil {
		return access.Principal{}, err
	}
	bundle, err := h.Auth.Exchange(h.t.Context(), identity.ExchangeRequest{CommandID: h.commandID(), Challenge: challenge.Challenge, IdentityToken: subject + "|" + nonce, InstallationID: "installation-" + subject, DeviceContinuity: &identity.DeviceContinuityClaim{DeviceID: old.DeviceID, Credential: h.deviceCredentials[subject]}})
	if err != nil {
		return access.Principal{}, err
	}
	h.bundles[subject] = bundle
	bundle, err = h.Auth.Refresh(h.t.Context(), bundle.RefreshToken)
	if err != nil {
		return access.Principal{}, err
	}
	h.bundles[subject] = bundle
	return h.Auth.Authenticate(h.t.Context(), bundle.AccessToken)
}
func (h *Harness) CrossAccountContinuity(subject, targetSubject string) error {
	challenge, err := h.Identity.Challenge(h.t.Context())
	if err != nil {
		return err
	}
	nonce, err := identity.AppleNonce(testOrigin, testInstance, challenge.Challenge)
	if err != nil {
		return err
	}
	target := h.bundles[targetSubject]
	_, err = h.Auth.Exchange(h.t.Context(), identity.ExchangeRequest{CommandID: h.commandID(), Challenge: challenge.Challenge,
		IdentityToken: subject + "|" + nonce, InstallationID: "installation-" + subject,
		DeviceContinuity: &identity.DeviceContinuityClaim{DeviceID: target.DeviceID, Credential: h.deviceCredentials[targetSubject]}})
	return err
}
func (h *Harness) NewDevice(subject string) (access.Principal, error) {
	challenge, err := h.Identity.Challenge(h.t.Context())
	if err != nil {
		return access.Principal{}, err
	}
	nonce, err := identity.AppleNonce(testOrigin, testInstance, challenge.Challenge)
	if err != nil {
		return access.Principal{}, err
	}
	bundle, err := h.Auth.Exchange(h.t.Context(), identity.ExchangeRequest{CommandID: h.commandID(), Challenge: challenge.Challenge, IdentityToken: subject + "|" + nonce, InstallationID: fmt.Sprintf("installation-%s-%d", subject, h.ids.Add(1))})
	if err != nil {
		return access.Principal{}, err
	}
	bundle, err = h.Auth.Refresh(h.t.Context(), bundle.RefreshToken)
	if err != nil {
		return access.Principal{}, err
	}
	return h.Auth.Authenticate(h.t.Context(), bundle.AccessToken)
}

func (h *Harness) AddCaregiver(t *testing.T, subject string) access.Principal {
	t.Helper()
	return h.AddMember(t, subject, "caregiver")
}

// AddMember signs a subject in and walks them through a real invitation for
// `role`, rather than writing a membership row behind the commands' back.
func (h *Harness) AddMember(t *testing.T, subject, role string) access.Principal {
	t.Helper()
	principal := h.signIn(subject, "Caregiver "+subject)
	invite, err := h.Family.Invite(t.Context(), h.Owner, h.commandID(), role)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Family.Redeem(t.Context(), principal, h.commandID(), invite.Token, true); err != nil {
		t.Fatal(err)
	}
	return h.refresh(subject)
}

func (h *Harness) Count(t *testing.T, table, familyID string) int {
	return count(t, h.Migrator, table, familyID, h.Owner.AccountID)
}
func (h *Harness) CountAs(t *testing.T, table, familyID, accountID string) int {
	return count(t, h.Migrator, table, familyID, accountID)
}
func (h *Harness) DeviceCount(t *testing.T, accountID string) int {
	t.Helper()
	var count int
	if err := h.Migrator.QueryRow(t.Context(), `SELECT count(*) FROM devices WHERE account_id=$1`, accountID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
func (h *Harness) PendingWakes(t *testing.T) []PendingWake {
	t.Helper()
	tx, err := h.Migrator.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err = tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: h.Owner.AccountID, FamilyID: h.FamilyID}); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(t.Context(), `SELECT family_id::text,device_id::text,instance_id::text,recovery_epoch,sequence,not_before,attempts,generation
		FROM delivery_pending WHERE family_id=$1 ORDER BY device_id`, h.FamilyID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []PendingWake
	for rows.Next() {
		var wake PendingWake
		var epoch, sequence, attempts, generation int64
		if err = rows.Scan(&wake.FamilyID, &wake.DeviceID, &wake.Cursor.ServerInstanceID, &epoch, &sequence,
			&wake.NotBefore, &attempts, &generation); err != nil {
			t.Fatal(err)
		}
		if epoch < 1 || sequence < 1 || attempts < 0 || attempts > int64(^uint32(0)) || generation < 1 {
			t.Fatalf("invalid pending wake counters epoch=%d sequence=%d attempts=%d generation=%d", epoch, sequence, attempts, generation)
		}
		wake.Cursor.RecoveryEpoch, wake.Cursor.Sequence, wake.Cursor.FamilyID = uint64(epoch), uint64(sequence), wake.FamilyID
		wake.Attempts, wake.Generation = uint32(attempts), uint64(generation)
		result = append(result, wake)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
func (h *Harness) SuppressHintPublisher(t *testing.T) {
	t.Helper()
	observer, ok := h.commitObserver.(*hostedQueueObserver)
	if !ok {
		t.Fatal("hint publisher suppression requires hosted harness")
	}
	observer.publish = false
}
func (h *Harness) RestartServices(t *testing.T) {
	t.Helper()
	h.RebuildSyncService(t)
}
func (h *Harness) Pull(t *testing.T, principal access.Principal, cursor *protocol.Cursor, pageToken *string) protocol.SyncResponse {
	t.Helper()
	response, err := h.Service.ApplyAndPull(t.Context(), principal, h.SyncRequest(cursor, pageToken))
	if err != nil {
		t.Fatal(err)
	}
	return response
}
func (h *Harness) ExecFamily(t *testing.T, query string, args ...any) {
	execFamily(t, h.Migrator, h.Owner.AccountID, h.FamilyID, query, args...)
}
func (h *Harness) FamilySequence(t *testing.T) uint64 {
	return familySequence(t, h.Migrator, h.Owner.AccountID, h.FamilyID)
}
func (h *Harness) FamilyUsage(t *testing.T) (int64, int64) {
	return familyUsage(t, h.Migrator, h.Owner.AccountID, h.FamilyID)
}

func (h *Harness) StartFeed(t *testing.T, principal access.Principal, id string) protocol.Outcome {
	t.Helper()
	m := MustMutation(t, "timer-feed.json")
	m.MutationID, m.EntityID = h.commandID(), id
	return h.applyTimer(t, principal, m)
}

func (h *Harness) StopFeed(t *testing.T, principal access.Principal, id, recordID string) protocol.Outcome {
	t.Helper()
	fixture := MustMutation(t, "timer-feed.json")
	p := fixture.Payload.(protocol.TimerStartPayload)
	fixture.MutationID, fixture.EntityID, fixture.Operation = h.commandID(), id, "timer_stop"
	fixture.Payload = protocol.TimerStopPayload{BabyID: p.BabyID, CompletedRecordID: recordID, StoppedAt: "2026-09-20T15:10:00.000000Z"}
	return h.applyTimer(t, principal, fixture)
}

func (h *Harness) applyTimer(t *testing.T, principal access.Principal, mutation protocol.Mutation) protocol.Outcome {
	t.Helper()
	outcome, err := h.Service.Apply(t.Context(), principal, h.FamilyID, mutation)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func (h *Harness) ReadFeed(t *testing.T, id string) protocol.CanonicalEntity {
	t.Helper()
	var found protocol.CanonicalEntity
	err := h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, _ access.Grant) error {
		return records.NewRepository().StreamCanonical(t.Context(), tx, h.FamilyID, func(entity protocol.CanonicalEntity) error {
			if entity.EntityKind == "feed" && entity.Meta.ID == id {
				found = entity
			}
			return nil
		})
	})
	if err != nil || found.Meta.ID == "" {
		t.Fatalf("read feed %s: entity=%+v err=%v", id, found, err)
	}
	return found
}

func (h *Harness) FamilyHighWater(t *testing.T) protocol.Cursor {
	t.Helper()
	return *h.Cursor(h.FamilySequence(t))
}

func (h *Harness) TimerIsLive(t *testing.T, id string) bool {
	t.Helper()
	live := false
	err := h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, _ access.Grant) error {
		return tx.QueryRow(t.Context(), `SELECT tombstoned_at IS NULL FROM active_timers WHERE family_id=$1 AND id=$2`, h.FamilyID, id).Scan(&live)
	})
	if err != nil {
		t.Fatalf("read timer %s: %v", id, err)
	}
	return live
}

func (h *Harness) TimerVersion(t *testing.T, id string) uint64 {
	t.Helper()
	var version uint64
	err := h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, _ access.Grant) error {
		return tx.QueryRow(t.Context(), `SELECT entity_version FROM active_timers WHERE family_id=$1 AND id=$2`, h.FamilyID, id).Scan(&version)
	})
	if err != nil {
		t.Fatalf("read timer version %s: %v", id, err)
	}
	return version
}

func (h *Harness) RebuildSyncService(t *testing.T) {
	t.Helper()
	h.RebuildSyncServiceWithPageKeys(t, keyring("page.v1", 0x77))

}

func (h *Harness) RebuildSyncServiceWithPageKeys(t *testing.T, keys identity.DigestKeyring) {
	t.Helper()
	h.Service = familysync.NewServiceWithPaging(h.Security, records.NewRepository(), h.Info, h.Limits,
		func() time.Time { return h.Now }, familyResolver{h.Family}, h.commitObserver, keys)
	h.Importer = importer.NewService(h.Security, records.NewRepository(), familyResolver{h.Family}, h.Info, h.Limits, keys, func() time.Time { return h.Now })
	h.Importer.SetCommitObserver(h.commitObserver)
	h.Service.SetImportAdmission(h.Importer)
}

func ImportRequest(i int, name, date string) protocol.ImportResolveRequest {
	return protocol.ImportResolveRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityRecordsV1, protocol.CapabilityImportResolverV1}}, ImportID: fmt.Sprintf("10000000-0000-4000-8000-%012d", i+1), ImportMode: "match_shared", NormalizationVersion: "nfc-trim-fold-nfc-v1", Babies: []protocol.ImportBaby{{LocalBabyID: fmt.Sprintf("20000000-0000-4000-8000-%012d", i+1), Name: name, BirthDate: date}}, Records: []protocol.RecordManifest{}, Finalize: true}
}
func (h *Harness) Import(t *testing.T, p access.Principal, id, name, date string) protocol.ImportResolveResponse {
	t.Helper()
	r := ImportRequest(0, name, date)
	r.ImportID = id
	r.Babies[0].LocalBabyID = id
	out, e := h.Importer.Resolve(t.Context(), p, r)
	if e != nil {
		t.Fatal(e)
	}
	return out
}

func PageKeyring(id string, fill byte, previous ...identity.DigestKey) identity.DigestKeyring {
	keys := keyring(id, fill)
	keys.Previous = append([]identity.DigestKey(nil), previous...)
	return keys
}

func (h *Harness) Cursor(sequence uint64) *protocol.Cursor {
	value := protocol.Cursor{ServerInstanceID: h.Info.InstanceID, RecoveryEpoch: h.Info.RecoveryEpoch, FamilyID: h.FamilyID, Sequence: sequence}
	return &value
}

func (h *Harness) SyncRequest(cursor *protocol.Cursor, pageToken *string) protocol.SyncRequest {
	return protocol.SyncRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityRecordsV1}}, Cursor: cursor, Mutations: []protocol.Mutation{}, PageToken: pageToken}
}

func (h *Harness) SeedBabies(t *testing.T, count int) []protocol.Outcome {
	t.Helper()
	result := make([]protocol.Outcome, 0, count)
	for index := 0; index < count; index++ {
		mutation := MustMutation(t, "baby.json")
		value := h.ids.Add(1)
		mutation.MutationID = fmt.Sprintf("01000000-0000-4000-8000-%012x", value)
		mutation.EntityID = fmt.Sprintf("10000000-0000-4000-8000-%012x", value)
		payload := mutation.Payload.(protocol.BabyPayload)
		payload.Name = fmt.Sprintf("Baby %d", value)
		mutation.Payload = payload
		outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, mutation)
		if err != nil || outcome.Status != "applied" {
			t.Fatalf("seed baby %d: outcome=%+v err=%v", index, outcome, err)
		}
		result = append(result, outcome)
	}
	return result
}

func (h *Harness) RenameBaby(t *testing.T, id, name string) protocol.Outcome {
	t.Helper()
	mutation := MustMutation(t, "baby.json")
	mutation.MutationID = h.commandID()
	mutation.EntityID = id
	mutation.Operation = "update"
	mutation.ObservedEntityVersion = 1
	payload := mutation.Payload.(protocol.BabyPayload)
	payload.Name = name
	mutation.Payload = payload
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, mutation)
	if err != nil || outcome.Status != "applied" {
		t.Fatalf("rename baby: outcome=%+v err=%v", outcome, err)
	}
	return outcome
}

func (h *Harness) SetBabyIcon(t *testing.T, id, icon string) protocol.Outcome {
	t.Helper()
	mutation := MustMutation(t, "baby.json")
	mutation.MutationID = h.commandID()
	mutation.EntityID = id
	mutation.Operation = "update"
	mutation.ObservedEntityVersion = 1
	payload := mutation.Payload.(protocol.BabyPayload)
	payload.Icon = &icon
	mutation.Payload = payload
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, mutation)
	if err != nil || outcome.Status != "applied" {
		t.Fatalf("set baby icon: outcome=%+v err=%v", outcome, err)
	}
	return outcome
}
