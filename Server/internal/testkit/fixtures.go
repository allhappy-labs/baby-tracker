package testkit

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/care"
	"babytracker/server/internal/config"
	"babytracker/server/internal/family"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/importer"
	"babytracker/server/internal/protocol"
	familysync "babytracker/server/internal/sync"
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
	t                                     *testing.T
	Owner, Caregiver, Outsider            access.Principal
	FamilyID                              string
	Service                               *familysync.Service
	Importer                              *importer.Service
	Admin, Runtime, Migrator, Maintenance *pgxpool.Pool
	Auth                                  *auth.Service
	Identity                              *identity.Service
	Family                                *family.Service
	// Authority is the PostgreSQL security authority the services run on.
	Authority *authority.Service
	Info      protocol.ServerInfo
	Limits    protocol.Limits
	Now       time.Time
	ids       atomic.Uint64
	bundles   map[string]auth.SessionBundle
	// guard is what the sync and import services authorize through (the
	// Authority).
	guard             authority.RequestGuard
	deviceCredentials map[string]string
	pageKeys          identity.DigestKeyring
	// publisher receives each write's post-commit high water (SetPublisher).
	publisher care.Publisher
	// authorityOptions builds Authority; AuthorityOn reuses them on
	// another pool.
	authorityOptions authority.Options
	// composition is what Compose built the services with (identity, keys,
	// origin), every default filled in.
	composition Composition
}

func New(t *testing.T) *Harness {
	return newHarness(t, "", false)
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

// NewAtFixtureClock is New built and run at FixtureClock, for a test that
// replays data written against that instant: the legacy corpora and
// goldens (whose family and audit times are the harness's creation time),
// the recorded legacy decisions, and timer times an hour or a day before it.
// Its sessions are issued at FixtureClock, so the database's own clock
// stops honouring them for refresh a year later (2027-09-21): such a test
// must not depend on the database judging a session (app.push_targets,
// app.prune_push_tokens, app.live_activity_authorized). Use New for those.
func NewAtFixtureClock(t *testing.T) *Harness {
	t.Helper()
	return newHarnessOn(t, NewPostgres(t), "", false, func(*testing.T, Postgres) time.Time { return FixtureClock })
}

// NewAtDatabaseClock is New with the harness clock at the database's
// clock_timestamp() to the second rather than its latest noon, for a test
// whose services stamp rows the database then compares with its own clock
// within minutes (a Live Activity registration's lease and retry times
// against clock_timestamp()). The test may keep the clocks together with
// h.Now = h.DatabaseNow() without outrunning the harness's ten-minute access
// tokens.
func NewAtDatabaseClock(t *testing.T) *Harness {
	t.Helper()
	return newHarnessOn(t, NewPostgres(t), "", false, databaseSecond)
}

func newHarness(t *testing.T, unrefreshedSubject string, hosted bool) *Harness {
	t.Helper()
	return newHarnessOn(t, NewPostgres(t), unrefreshedSubject, hosted, databaseNoon)
}

// NewBound is New: every harness's authority is bound to the fixture's
// instance and epoch, which server_state holds, as production composition
// does. Integration credentials and readers use it.
func NewBound(t *testing.T) *Harness {
	t.Helper()
	return New(t)
}

// InstanceID is the fixture's server instance, the one server_state holds.
const InstanceID = testInstance

// RecoveryEpoch is the fixture's recovery epoch.
const RecoveryEpoch uint64 = 1

// databaseNoon is the latest 12:00:00 UTC at or before db's
// clock_timestamp(): the database's date at the fixture's time of day. The
// harness clock starts there, so it follows the database by less than a day
// and keeps FixtureClock's alignment: on the hour (a fixed-window limiter's
// first refusal says the whole window) and mid-afternoon in Europe/Zurich
// (no test straddles a local midnight).
func databaseNoon(t *testing.T, db Postgres) time.Time {
	t.Helper()
	return databaseSecond(t, db).Add(-12 * time.Hour).Truncate(24 * time.Hour).Add(12 * time.Hour)
}

// databaseSecond is db's clock_timestamp() in UTC, truncated to the second.
func databaseSecond(t *testing.T, db Postgres) time.Time {
	t.Helper()
	var now time.Time
	if err := db.Admin.QueryRow(t.Context(), `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatalf("database clock: %v", err)
	}
	return now.UTC().Truncate(time.Second)
}

// newHarnessOn builds the harness on db with its clock, Now, at start(db);
// the clock moves only when a test moves it. The database judges sessions by
// its own clock (app.push_targets, app.prune_push_tokens,
// app.live_activity_authorized), so New starts at databaseNoon: sessions
// issued against a fixed calendar date would stop counting once the real
// calendar passed that date plus the refresh window.
func newHarnessOn(t *testing.T, db Postgres, unrefreshedSubject string, hosted bool, start func(*testing.T, Postgres) time.Time) *Harness {
	t.Helper()
	now := start(t, db)
	authorityExec(t, db.Migrator, `INSERT INTO server_state(singleton,instance_id,recovery_epoch) VALUES (true,$1,$2)`, testInstance, int64(RecoveryEpoch))
	h := Compose(t, db, Composition{Now: now, Hosted: hosted})
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
	return h
}

// AuthorityOn is an authority over pool with the harness authority's
// options (round-trip tests run it on a counted pool).
func (h *Harness) AuthorityOn(t *testing.T, pool *pgxpool.Pool) *authority.Service {
	t.Helper()
	options := h.authorityOptions
	options.Pool = pool
	service, err := authority.New(options)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// AuthorityOnWith is AuthorityOn with the harness authority's options
// changed by change first (another clock or recovery epoch, for refusal
// tests).
func (h *Harness) AuthorityOnWith(t *testing.T, pool *pgxpool.Pool, change func(*authority.Options)) *authority.Service {
	t.Helper()
	options := h.authorityOptions
	options.Pool = pool
	change(&options)
	service, err := authority.New(options)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// AuthorityExec runs one statement as the table owner inside an authority
// transaction (the authority flag raised): a test's direct state change,
// such as a family status the commands would only reach through deletion.
func (h *Harness) AuthorityExec(t *testing.T, statement string, args ...any) {
	t.Helper()
	authorityExec(t, h.Migrator, statement, args...)
}

// ImporterOn is the harness import service (limits, keys) guarded by
// guard (round-trip tests pass an AuthorityOn a counted pool).
func (h *Harness) ImporterOn(guard authority.FamilyGuard) *importer.Service {
	return importer.NewService(guard, h.Info, h.Limits, h.pageKeys, func() time.Time { return h.Now })
}

// SyncServiceOn is the harness sync service (limits, keys, importer)
// guarded by guard.
func SyncServiceOn(t *testing.T, h *Harness, guard authority.RequestGuard) *familysync.Service {
	t.Helper()
	return SyncServiceWith(t, h, guard, nil)
}

// SyncServiceWith is SyncServiceOn reporting each completed request's
// progress to progress (nil: none), as production reports it to the wake
// tracker.
func SyncServiceWith(t *testing.T, h *Harness, guard authority.RequestGuard, progress familysync.Progress) *familysync.Service {
	t.Helper()
	service := familysync.New(familysync.Options{Guard: guard, Server: h.Info, Limits: h.Limits, Now: func() time.Time { return h.Now },
		Publisher: h.publisher, PageKeys: h.pageKeys, Allowed: family.Allowed, Progress: progress})
	service.SetImportAdmission(h.Importer)
	return service
}

// SetPublisher rebuilds the sync and import services publishing each
// write's post-commit high water to p (a live hub's delivery.Observer, as
// production composes it).
func (h *Harness) SetPublisher(t *testing.T, p care.Publisher) {
	t.Helper()
	h.publisher = p
	h.RebuildSyncServiceWithPageKeys(t, h.pageKeys)
}

// Decoded is response as a client reads it: encoded as the HTTP handler
// writes it and decoded with protocol.SyncResponse.UnmarshalJSON, so
// Changes holds the served changes.
func Decoded(t *testing.T, response protocol.SyncResponse) protocol.SyncResponse {
	t.Helper()
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var decoded protocol.SyncResponse
	if err := decoded.UnmarshalJSON(raw); err != nil {
		t.Fatalf("decode sync response: %v", err)
	}
	return decoded
}

// DecodedSnapshot is Decoded for a snapshot page: Entities holds the served
// entities.
func DecodedSnapshot(t *testing.T, response protocol.SnapshotResponse) protocol.SnapshotResponse {
	t.Helper()
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var decoded protocol.SnapshotResponse
	if err := decoded.UnmarshalJSON(raw); err != nil {
		t.Fatalf("decode snapshot response: %v", err)
	}
	return decoded
}

// setServices builds Info, Limits and the sync and import services over
// h.guard.
func (h *Harness) setServices() {
	limits := protocol.DefaultLimits()
	c := h.composition
	info := protocol.ServerInfo{InstanceID: c.InstanceID, RecoveryEpoch: c.RecoveryEpoch, CanonicalOrigin: c.Origin, APIVersions: []string{"v1"}, Capabilities: protocol.ServerCapabilities(), AppleAuthorizationMode: "identity_token_only", Limits: &limits}
	h.Info, h.Limits = info, limits
	h.RebuildSyncServiceWithPageKeys(h.t, c.PageKeys)
}

// configureIdentity builds the identity and auth services over h.Authority
// with verifier.
func (h *Harness) configureIdentity(t *testing.T, verifier identity.IdentityVerifier) {
	t.Helper()
	clock := func() time.Time { return h.Now }
	c := h.composition
	var err error
	h.Identity, err = identity.New(identity.Options{Verifier: verifier, Authority: h.Authority.IdentityAuthority(), Origin: c.Origin,
		InstanceID: c.InstanceID, IdentityKeys: c.IdentityKeys, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	h.Auth, err = auth.New(auth.Options{Identity: h.Identity, Verifier: verifier, Authority: h.Authority, Mode: config.ModeSelfHosted,
		Origin: c.Origin, InstanceID: c.InstanceID, TokenKeys: c.TokenKeys, DeviceKeys: c.ContinuityKeys, ActorIDKey: c.ActorKey, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
}

// authorityExec runs one statement as the table owner inside an authority
// transaction (the authority flag raised), as authority seeds are written.
func authorityExec(t *testing.T, pool *pgxpool.Pool, statement string, args ...any) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `SELECT set_config('app.authority','1',true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), statement, args...); err != nil {
		t.Fatalf("authority seed %q: %v", statement, err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
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
// authority and key material, while subsequent exchanges exercise the
// supplied verifier.
func (h *Harness) ConfigureSignedIdentity(t *testing.T, verifier identity.IdentityVerifier) {
	t.Helper()
	h.configureIdentity(t, verifier)
}
func (h *Harness) signIn(subject, name string) access.Principal {
	challenge, err := h.Identity.Challenge(h.t.Context())
	if err != nil {
		h.t.Fatal(err)
	}
	nonce, err := identity.AppleNonce(h.composition.Origin, h.composition.InstanceID, challenge.Challenge)
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
	nonce, err := identity.AppleNonce(h.composition.Origin, h.composition.InstanceID, challenge.Challenge)
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
	nonce, err := identity.AppleNonce(h.composition.Origin, h.composition.InstanceID, challenge.Challenge)
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
	p, _, err := h.NewDeviceSession(subject)
	return p, err
}

// NewDeviceSession is NewDevice that also returns the new device's
// refreshed access token (the subject's own token stays its first
// device's).
func (h *Harness) NewDeviceSession(subject string) (access.Principal, string, error) {
	challenge, err := h.Identity.Challenge(h.t.Context())
	if err != nil {
		return access.Principal{}, "", err
	}
	nonce, err := identity.AppleNonce(h.composition.Origin, h.composition.InstanceID, challenge.Challenge)
	if err != nil {
		return access.Principal{}, "", err
	}
	bundle, err := h.Auth.Exchange(h.t.Context(), identity.ExchangeRequest{CommandID: h.commandID(), Challenge: challenge.Challenge, IdentityToken: subject + "|" + nonce, InstallationID: fmt.Sprintf("installation-%s-%d", subject, h.ids.Add(1))})
	if err != nil {
		return access.Principal{}, "", err
	}
	bundle, err = h.Auth.Refresh(h.t.Context(), bundle.RefreshToken)
	if err != nil {
		return access.Principal{}, "", err
	}
	p, err := h.Auth.Authenticate(h.t.Context(), bundle.AccessToken)
	return p, bundle.AccessToken, err
}

// SignInDevice signs subject in on a new installation and returns the
// session bundle (its refresh token and device credential included). It
// touches no harness map, so goroutines may call it while others run (the
// harness's own sign-ins, Refresh and AccessToken may not).
func (h *Harness) SignInDevice(ctx context.Context, subject string) (auth.SessionBundle, error) {
	challenge, err := h.Identity.Challenge(ctx)
	if err != nil {
		return auth.SessionBundle{}, err
	}
	nonce, err := identity.AppleNonce(h.composition.Origin, h.composition.InstanceID, challenge.Challenge)
	if err != nil {
		return auth.SessionBundle{}, err
	}
	return h.Auth.Exchange(ctx, identity.ExchangeRequest{CommandID: h.commandID(), Challenge: challenge.Challenge, IdentityToken: subject + "|" + nonce,
		InstallationID: fmt.Sprintf("installation-%s-%d", subject, h.ids.Add(1)), Profile: &identity.ProfileInput{DisplayName: subject, Confirmed: true}})
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
func (h *Harness) RestartServices(t *testing.T) {
	t.Helper()
	h.RebuildSyncService(t, h.Limits)
}

// Pull is one pull page as the client decodes it (Decoded).
func (h *Harness) Pull(t *testing.T, principal access.Principal, cursor *protocol.Cursor, pageToken *string) protocol.SyncResponse {
	t.Helper()
	response, err := h.Service.ApplyAndPull(t.Context(), principal, h.SyncRequest(cursor, pageToken))
	if err != nil {
		t.Fatal(err)
	}
	return Decoded(t, response)
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
	// A breast timer's stop names the part it completes; the part is
	// derived from the record so a stop is reproducible.
	fixture.Payload = protocol.TimerStopPayload{BabyID: p.BabyID, CompletedRecordID: recordID, StoppedAt: "2026-09-20T15:10:00.000000Z",
		ComponentID: "4" + recordID[1:]}
	return h.applyTimer(t, principal, fixture)
}

// applyTimer applies a timer command and fails the test unless it was
// applied: a rejected start or stop would otherwise let a test that waits
// for its effect pass without it.
func (h *Harness) applyTimer(t *testing.T, principal access.Principal, mutation protocol.Mutation) protocol.Outcome {
	t.Helper()
	outcome, err := h.Service.Apply(t.Context(), principal, h.FamilyID, mutation)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != "applied" {
		t.Fatalf("%s %s was not applied: %+v", mutation.Operation, mutation.EntityID, outcome)
	}
	return outcome
}

func (h *Harness) ReadFeed(t *testing.T, id string) protocol.CanonicalEntity {
	t.Helper()
	var found protocol.CanonicalEntity
	err := h.guard.WithFamilyTx(t.Context(), h.Owner, "read", func(tx pgx.Tx, _ access.Grant) error {
		row, err := care.Get(t.Context(), tx, h.FamilyID, id)
		if err != nil {
			return err
		}
		if row.Kind != "feed" {
			return fmt.Errorf("%s is a %s", id, row.Kind)
		}
		found, err = row.Entity()
		return err
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
	err := h.guard.WithFamilyTx(t.Context(), h.Owner, "read", func(tx pgx.Tx, _ access.Grant) error {
		return tx.QueryRow(t.Context(), `SELECT tombstoned_at IS NULL FROM care_entities WHERE family_id=$1 AND id=$2 AND kind=5`, h.FamilyID, id).Scan(&live)
	})
	if err != nil {
		t.Fatalf("read timer %s: %v", id, err)
	}
	return live
}

func (h *Harness) TimerVersion(t *testing.T, id string) uint64 {
	t.Helper()
	var version uint64
	err := h.guard.WithFamilyTx(t.Context(), h.Owner, "read", func(tx pgx.Tx, _ access.Grant) error {
		return tx.QueryRow(t.Context(), `SELECT entity_version FROM care_entities WHERE family_id=$1 AND id=$2 AND kind=5`, h.FamilyID, id).Scan(&version)
	})
	if err != nil {
		t.Fatalf("read timer version %s: %v", id, err)
	}
	return version
}

// RebuildSyncService rebuilds the sync and import services under limits
// (small page sizes for paging tests), which become h.Limits.
func (h *Harness) RebuildSyncService(t *testing.T, limits protocol.Limits) {
	t.Helper()
	h.Limits = limits
	h.RebuildSyncServiceWithPageKeys(t, h.composition.PageKeys)
}

func (h *Harness) RebuildSyncServiceWithPageKeys(t *testing.T, keys identity.DigestKeyring) {
	t.Helper()
	h.pageKeys = keys
	h.Importer = importer.NewService(h.guard, h.Info, h.Limits, keys, func() time.Time { return h.Now })
	if h.publisher != nil {
		h.Importer.SetPublisher(h.publisher)
	}
	h.Service = SyncServiceOn(t, h, h.guard)
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
