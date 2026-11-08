//go:build identitytest

package capacity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/config"
	"babytracker/server/internal/database"
	"babytracker/server/internal/family"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/migrate"
	"babytracker/server/internal/protocol"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/internal/testidentity"
	"babytracker/server/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FixtureDevice is private harness state, never included in FixtureManifest.
type FixtureDevice struct {
	FamilyID         string
	BabyID           string
	DeviceCredential string
	Session          auth.SessionBundle
}

// FixtureIntegration is a private MCP credential for one family's owner.
// Grants are issued in every fixture, whatever cells later use it.
type FixtureIntegration struct {
	FamilyID   string
	BabyID     string
	GrantID    string
	Credential string
}
type fixturePrivate struct {
	Config            config.Config
	Devices           []FixtureDevice
	Integrations      []FixtureIntegration
	AuthorityEvents   uint64
	SourceInputSHA256 string
}
type fixtureSeal struct {
	Spec      FixtureSpec
	Manifest  FixtureManifest
	Artifacts map[string]string
}

// RestoredFixture owns only fresh disposable resources. Close must succeed
// before the caller can report a successful workload or completed cleanup.
type RestoredFixture struct {
	Config                  config.Config `json:"-"`
	Manifest                FixtureManifest
	Spec                    FixtureSpec
	SourceInputSHA256       string
	Devices                 []FixtureDevice      `json:"-"`
	Integrations            []FixtureIntegration `json:"-"`
	Runtime, Maintenance    *pgxpool.Pool        `json:"-"`
	Authority               *authority.Service   `json:"-"`
	Families                *family.Service      `json:"-"`
	Auth                    *auth.Service        `json:"-"`
	Sync                    *familysync.Service  `json:"-"`
	RestoredAuthorityEvents uint64
	db                      *ownedPostgres
	root                    string
	identity                *identity.Service
	mcpReader               *authority.IntegrationReader
	readiness               fixtureReadiness
	credentialDeadline      time.Time
}

func newFixtureConfig() (config.Config, error) {
	origin, _ := url.Parse("https://sync.example.test")
	cfg := config.Config{Mode: config.ModeSelfHosted, PublicOrigin: origin, ServerInstanceID: fixtureUUID(), RecoveryEpoch: 1, AppleAudience: "com.example.capacity"}
	cfg.Limits = config.LimitsConfig{Policies: limits.DefaultPolicies(), MaxCaregivers: 10, MaxInvitations: 10, InvitationTTL: 7 * 24 * time.Hour}
	cfg.Sync.Limits = protocol.DefaultLimits()
	for _, ring := range []*config.DigestKeyringConfig{&cfg.Auth.IdentityKeys, &cfg.Auth.SessionTokenKeys, &cfg.Auth.DeviceContinuityKeys, &cfg.Auth.AppleNotificationKeys, &cfg.Auth.InvitationKeys, &cfg.Auth.PageTokenKeys} {
		ring.Current.ID = "capacity.v1"
		if _, err := rand.Read(ring.Current.Secret[:]); err != nil {
			return cfg, err
		}
	}
	if _, err := rand.Read(cfg.Auth.ActorIDPepper[:]); err != nil {
		return cfg, err
	}
	if _, err := rand.Read(cfg.Auth.RateLimitPepper[:]); err != nil {
		return cfg, err
	}
	return cfg, nil
}
func fixtureUUID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return ""
	}
	value[6] = value[6]&15 | 64
	value[8] = value[8]&63 | 128
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}
func fixtureKeyring(r config.DigestKeyringConfig) identity.DigestKeyring {
	return identity.DigestKeyring{Current: identity.DigestKey{ID: r.Current.ID, Secret: r.Current.Secret}}
}

// fixtureReadiness is the server's database readiness for a fixture: a
// compatible schema, the runtime login's exact manifest privileges and
// server_state equal to the fixture's instance, epoch and actor-ID key.
type fixtureReadiness struct {
	runtime *pgxpool.Pool
	cfg     config.Config
}

func (s fixtureReadiness) Check(ctx context.Context) error {
	compatibility, err := migrate.EmbeddedCompatibility()
	if err != nil {
		return err
	}
	version, err := migrate.New(migrations.Files).CompatibleVersion(ctx, s.runtime, compatibility)
	if err != nil {
		return err
	}
	if err := database.VerifyRuntimeRole(ctx, s.runtime, version); err != nil {
		return err
	}
	state, err := authority.LoadServerState(ctx, s.runtime)
	if err != nil {
		return err
	}
	return state.Check(s.cfg.ServerInstanceID, s.cfg.RecoveryEpoch, s.cfg.Auth.ActorIDPepper)
}

// authorityEvents is the number of audit rows every authority command has
// appended, across all scopes (the fixture's authority history).
func (r *RestoredFixture) authorityEvents(ctx context.Context) (uint64, error) {
	supervisor, err := r.db.open(ctx, "postgres", 1)
	if err != nil {
		return 0, err
	}
	defer supervisor.Close()
	var events uint64
	err = supervisor.QueryRow(ctx, "SELECT count(*) FROM security_audit_events").Scan(&events)
	return events, err
}

func composeFixture(ctx context.Context, cfg config.Config, db *ownedPostgres, initialize bool) (*RestoredFixture, *testidentity.Issuer, error) {
	if initialize {
		// baby-tracker-admin authority init, as the table owner.
		if _, err := authority.InitServerState(ctx, db.Owner, cfg.ServerInstanceID, cfg.Auth.ActorIDPepper); err != nil {
			return nil, nil, err
		}
	}
	authorityService, err := authority.New(authority.Options{Pool: db.Runtime, Now: time.Now, InstanceID: cfg.ServerInstanceID,
		RecoveryEpoch: cfg.RecoveryEpoch, TokenKeys: fixtureKeyring(cfg.Auth.SessionTokenKeys),
		MaxCaregivers: cfg.Limits.MaxCaregivers, MaxInvitations: cfg.Limits.MaxInvitations,
		Allowed: family.Allowed, EntitlementExempt: authority.LegacyEntitlementExempt})
	if err != nil {
		return nil, nil, err
	}
	families, err := family.New(family.Options{Authority: authorityService, InvitationKeys: fixtureKeyring(cfg.Auth.InvitationKeys),
		Origin: cfg.PublicOrigin.String(), Now: time.Now, InvitationTTL: cfg.Limits.InvitationTTL})
	if err != nil {
		return nil, nil, err
	}
	readiness := fixtureReadiness{runtime: db.Runtime, cfg: cfg}
	if err := readiness.Check(ctx); err != nil {
		return nil, nil, err
	}
	issuer, err := testidentity.New("capacity-issuer", time.Now)
	if err != nil {
		return nil, nil, err
	}
	verifier, err := issuer.Verifier(cfg.AppleAudience)
	if err != nil {
		return nil, nil, err
	}
	identityService, err := identity.New(identity.Options{Verifier: verifier, Authority: authorityService.IdentityAuthority(), Origin: cfg.PublicOrigin.String(), InstanceID: cfg.ServerInstanceID, IdentityKeys: fixtureKeyring(cfg.Auth.IdentityKeys), Now: time.Now})
	if err != nil {
		return nil, nil, err
	}
	sessions, err := auth.New(auth.Options{Identity: identityService, Verifier: verifier, Authority: authorityService, Mode: config.ModeSelfHosted, Origin: cfg.PublicOrigin.String(), InstanceID: cfg.ServerInstanceID, TokenKeys: fixtureKeyring(cfg.Auth.SessionTokenKeys), DeviceKeys: fixtureKeyring(cfg.Auth.DeviceContinuityKeys), ActorIDKey: cfg.Auth.ActorIDPepper, Now: time.Now})
	if err != nil {
		return nil, nil, err
	}
	info := protocol.ServerInfo{InstanceID: cfg.ServerInstanceID, RecoveryEpoch: cfg.RecoveryEpoch, CanonicalOrigin: cfg.PublicOrigin.String(), APIVersions: []string{"v1"}, Capabilities: protocol.ServerCapabilities(), AppleAuthorizationMode: "identity_token_only", Limits: &cfg.Sync.Limits}
	syncService := familysync.New(familysync.Options{Guard: authorityService, Server: info, Limits: cfg.Sync.Limits, Now: time.Now,
		PageKeys: fixtureKeyring(cfg.Auth.PageTokenKeys), Allowed: family.Allowed})
	// Credential checks use their own small read-only pool, never the
	// runtime pool, as the server's dedicated MCP pool.
	readerPool, err := db.openReadOnly(ctx, "capacity_runtime", 1)
	if err != nil {
		return nil, nil, err
	}
	mcpReader, err := authority.NewIntegrationReader(authorityService, readerPool)
	if err != nil {
		return nil, nil, err
	}
	result := &RestoredFixture{Config: cfg, Runtime: db.Runtime, Maintenance: db.Maintenance, Authority: authorityService, Families: families, Auth: sessions, Sync: syncService, db: db, mcpReader: mcpReader, readiness: readiness}
	// Test issuer service is used only during offline provisioning.
	result.identity = identityService
	return result, issuer, nil
}

// issueFixtureIntegration issues one private MCP grant through the real
// operator authority command, exactly as baby-tracker-admin does.
func issueFixtureIntegration(ctx context.Context, r *RestoredFixture, accountID, familyID, babyID string) (FixtureIntegration, error) {
	grantID := fixtureUUID()
	secret, err := authority.NewIntegrationSecret()
	if err != nil {
		return FixtureIntegration{}, err
	}
	credential, err := authority.FormatIntegrationCredential(grantID, secret)
	if err != nil {
		return FixtureIntegration{}, err
	}
	_, verifier, ok := authority.IntegrationCredentialVerifier(credential)
	if !ok {
		return FixtureIntegration{}, ErrFixtureInvalid
	}
	revision, err := r.Authority.FamilyRevision(ctx, familyID)
	if err != nil {
		return FixtureIntegration{}, err
	}
	digest := sha256.Sum256([]byte("capacity-mcp-grant\x00" + grantID))
	input := authority.OperatorIntegration{CommandID: fixtureUUID(), OperatorID: fixtureUUID(), Operation: "grant", GrantID: grantID,
		FamilyID: familyID, AccountID: accountID, ServerInstanceID: r.Config.ServerInstanceID, RecoveryEpoch: r.Config.RecoveryEpoch,
		PolicyID: "Capacity.Policy-v1", EvidenceDigest: hex.EncodeToString(digest[:]), ReasonCode: "integration_issued",
		ExpectedRevision: revision, ConfirmationExpiresAt: time.Now().UTC().Add(10 * time.Minute).Truncate(time.Second),
		Name: "Capacity reader", Scope: authority.IntegrationScopeFamilyRead, Verifier: verifier, ValiditySeconds: authority.IntegrationValiditySeconds}
	if _, err := r.Authority.ExecuteOperatorIntegration(ctx, input); err != nil {
		return FixtureIntegration{}, err
	}
	return FixtureIntegration{FamilyID: familyID, BabyID: babyID, GrantID: grantID, Credential: credential}, nil
}

func provisionFixture(ctx context.Context, r *RestoredFixture, issuer *testidentity.Issuer, spec FixtureSpec) ([]FixtureDevice, error) {
	var devices []FixtureDevice
	for f := 0; f < spec.Families; f++ {
		subject := fmt.Sprintf("capacity-family-%d", f)
		var familyID string
		var familyDevices []FixtureDevice
		for d := 0; d < spec.DevicesPerFamily; d++ {
			identityService := r.identity
			challenge, err := identityService.Challenge(ctx)
			if err != nil {
				return nil, err
			}
			nonce, err := identity.AppleNonce(r.Config.PublicOrigin.String(), r.Config.ServerInstanceID, challenge.Challenge)
			if err != nil {
				return nil, err
			}
			proof, err := issuer.Sign(r.Config.AppleAudience, subject, nonce)
			if err != nil {
				return nil, err
			}
			bundle, err := r.Auth.Exchange(ctx, identity.ExchangeRequest{CommandID: fixtureUUID(), Challenge: challenge.Challenge, IdentityToken: proof, InstallationID: fmt.Sprintf("capacity-%d-%d", f, d), Profile: &identity.ProfileInput{DisplayName: "Synthetic parent", Confirmed: true}})
			if err != nil {
				return nil, err
			}
			if d == 0 {
				p, err := r.Auth.Authenticate(ctx, bundle.AccessToken)
				if err != nil {
					return nil, err
				}
				view, err := r.Families.Create(ctx, p, fixtureUUID(), "Synthetic family")
				if err != nil {
					return nil, err
				}
				familyID = view.ID
			}
			continuity := bundle.DeviceCredential
			// A genuine refresh upgrades the persisted account session to family scope.
			bundle, err = r.Auth.Refresh(ctx, bundle.RefreshToken)
			if err != nil {
				return nil, err
			}
			familyDevices = append(familyDevices, FixtureDevice{FamilyID: familyID, DeviceCredential: continuity, Session: bundle})
		}
		p, err := r.Auth.Authenticate(ctx, familyDevices[0].Session.AccessToken)
		if err != nil {
			return nil, err
		}
		babyID := fixtureUUID()
		mutation := protocol.Mutation{MutationID: fixtureUUID(), EntityID: babyID, EntityKind: "baby", Operation: "create", PayloadVersion: 1, ClientMutationTime: time.Now().UTC().Format(time.RFC3339Nano), Payload: protocol.BabyPayload{Name: "Synthetic baby", BirthDate: "2026-01-01", SortOrder: 0}}
		outcome, err := r.Sync.Apply(ctx, p, familyID, mutation)
		if err != nil {
			return nil, err
		}
		if outcome.Status != "applied" {
			return nil, errors.New("capacity_baby_not_applied")
		}
		for c := 0; c < spec.CareEntriesPerBaby; c++ {
			mutation = protocol.Mutation{MutationID: fixtureUUID(), EntityID: fixtureUUID(), EntityKind: "diaper", Operation: "create", PayloadVersion: 1, ClientMutationTime: time.Now().UTC().Format(time.RFC3339Nano), Payload: protocol.DiaperPayload{BabyID: babyID, StartedAt: time.Now().Add(-time.Duration(c) * time.Minute).UTC().Format(time.RFC3339Nano), TimeZoneID: "UTC", Contents: "wet"}}
			outcome, err = r.Sync.Apply(ctx, p, familyID, mutation)
			if err != nil {
				return nil, err
			}
			if outcome.Status != "applied" {
				return nil, errors.New("capacity_care_not_applied")
			}
		}
		for i := range familyDevices {
			familyDevices[i].BabyID = babyID
		}
		devices = append(devices, familyDevices...)
		// Issued before the authority-event top-up, so every cell restored
		// from this fixture shares the same grant and authority history.
		for g := 0; g < spec.MCPGrantsPerFamily; g++ {
			integration, err := issueFixtureIntegration(ctx, r, p.AccountID, familyID, babyID)
			if err != nil {
				return nil, err
			}
			r.Integrations = append(r.Integrations, integration)
		}
	}
	// Legitimate family commands age history: the first family's owner
	// invites and revokes the invitation (two family-scope rows, nothing left
	// pending). A routine refresh appends no audit row (owner decision D1,
	// 2026-10-16), so rotations no longer can. Natural history is never
	// rewritten if provisioning already exceeds the requested minimum event
	// count.
	var owner access.Principal
	for {
		events, err := r.authorityEvents(ctx)
		if err != nil {
			return nil, err
		}
		if events >= uint64(spec.AuthorityEvents) {
			break
		}
		if owner.AccountID == "" {
			if owner, err = r.Auth.Authenticate(ctx, devices[0].Session.AccessToken); err != nil {
				return nil, err
			}
		}
		invitation, err := r.Families.Invite(ctx, owner, fixtureUUID(), "caregiver")
		if err != nil {
			return nil, err
		}
		if _, err := r.Families.RevokeInvitation(ctx, owner, fixtureUUID(), invitation.Invitation.ID); err != nil {
			return nil, err
		}
	}
	return devices, nil
}

func writeFixtureJSON(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0600)
}
func readFixtureJSON(path string, value any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ErrFixtureInvalid
	}
	if len(raw) > 32<<20 {
		return ErrFixtureInvalid
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil {
		return ErrFixtureInvalid
	}
	return nil
}

// BuildFixture creates and validates a sealed logical database snapshot. All
// database connections are generated locally, never supplied by the caller.
func BuildFixture(ctx context.Context, spec FixtureSpec, destination string) (manifest FixtureManifest, resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := spec.validate(); err != nil {
		return manifest, err
	}
	if err := disposableDestination(destination); err != nil {
		return manifest, err
	}
	source, sourceInput, err := fixtureSourceIdentity(ctx)
	if err != nil {
		return manifest, err
	}
	if err := localDocker(ctx); err != nil {
		return manifest, err
	}
	db, err := reservePostgres(destination, "")
	if err != nil {
		return manifest, err
	}
	defer func() { resultErr = errors.Join(resultErr, db.Close()) }()
	if err := os.Mkdir(destination, 0700); err != nil {
		return manifest, err
	}
	success := false
	defer func() {
		if !success {
			resultErr = errors.Join(resultErr, os.RemoveAll(destination))
		}
	}()
	cfg, err := newFixtureConfig()
	if err != nil {
		return manifest, err
	}
	_, err = startPostgres(ctx, db)
	if err != nil {
		return manifest, err
	}
	if err := db.migrate(ctx); err != nil {
		return manifest, err
	}
	r, issuer, err := composeFixture(ctx, cfg, db, true)
	if err != nil {
		return manifest, err
	}
	devices, err := provisionFixture(ctx, r, issuer, spec)
	if err != nil {
		return manifest, fmt.Errorf("capacity_provision_failed: %w", err)
	}
	r.Devices = devices
	r.Spec = spec
	if len(r.Integrations) != spec.Families*spec.MCPGrantsPerFamily {
		return manifest, ErrFixtureInvalid
	}
	events, err := r.authorityEvents(ctx)
	if err != nil {
		return manifest, err
	}
	if err := verifyFixtureState(ctx, r, events); err != nil {
		return manifest, err
	}
	expiry := r.credentialDeadline
	manifest = FixtureManifest{Version: 1, SourceSHA: source, InputSHA256: fixtureInputDigest(spec, source), Families: spec.Families, Devices: len(devices), MCPGrants: len(r.Integrations), ActualAuthorityEvents: events, CreatedAt: time.Now().UTC(), ExpiresAt: expiry}
	if err := writeFixtureJSON(filepath.Join(destination, "private.json"), fixturePrivate{Config: cfg, Devices: devices, Integrations: r.Integrations, AuthorityEvents: events, SourceInputSHA256: sourceInput}); err != nil {
		return manifest, err
	}
	if err := db.dump(ctx, filepath.Join(destination, "database.dump")); err != nil {
		return manifest, err
	}
	artifacts, err := fixtureArtifactDigests(destination)
	if err != nil {
		return manifest, err
	}
	if err := writeFixtureJSON(filepath.Join(destination, "seal.json"), fixtureSeal{spec, manifest, artifacts}); err != nil {
		return manifest, err
	}
	if err := writeFixtureJSON(filepath.Join(destination, "manifest.json"), manifest); err != nil {
		return manifest, err
	}
	// Verify a wholly fresh restore before returning a reusable fixture.
	verifyRoot := filepath.Join(db.root, "verify")
	restored, err := RestoreFixture(ctx, destination, verifyRoot)
	if err != nil {
		return manifest, err
	}
	if err := restored.Close(); err != nil {
		return manifest, err
	}
	db.receipt.FixtureSealed = true
	if err := db.writeReceipt(); err != nil {
		return manifest, err
	}
	success = true
	return manifest, nil
}

func fixtureSourceIdentity(ctx context.Context) (string, string, error) {
	raw, err := exec.CommandContext(ctx, "git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "", "", errors.New("capacity_source_identity_unavailable")
	}
	source := strings.TrimSpace(string(raw))
	if !sourcePattern.MatchString(source) {
		return "", "", ErrFixtureInvalid
	}
	rootRaw, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", "", ErrFixtureInvalid
	}
	server := filepath.Join(strings.TrimSpace(string(rootRaw)), "Server")
	hash := sha256.New()
	err = filepath.WalkDir(server, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return ErrFixtureInvalid
		}
		ext := filepath.Ext(path)
		if ext != ".go" && ext != ".sql" && ext != ".mod" && ext != ".sum" && ext != ".json" {
			return nil
		}
		rel, _ := filepath.Rel(server, path)
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		fmt.Fprintf(hash, "%s\x00%x\n", filepath.ToSlash(rel), sum)
		return nil
	})
	if err != nil {
		return "", "", err
	}
	return source, hex.EncodeToString(hash.Sum(nil)), nil
}

func fixtureArtifactDigests(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "seal.json" || rel == "manifest.json" {
			return nil
		}
		if !d.Type().IsRegular() {
			return ErrFixtureInvalid
		}
		info, err := d.Info()
		if err != nil || info.Mode().Perm() != 0600 {
			return ErrFixtureInvalid
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Getuid()) {
			return ErrFixtureInvalid
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = hex.EncodeToString(hash.Sum(nil))
		return nil
	})
	return out, err
}

func verifyFixtureState(ctx context.Context, r *RestoredFixture, wantEvents uint64) error {
	if err := r.readiness.Check(ctx); err != nil {
		return err
	}
	events, err := r.authorityEvents(ctx)
	if err != nil || events != wantEvents {
		return ErrFixtureInvalid
	}
	if len(r.Devices) != r.Spec.Families*r.Spec.DevicesPerFamily {
		return ErrFixtureInvalid
	}
	supervisor, err := r.db.open(ctx, "postgres", 1)
	if err != nil {
		return err
	}
	defer supervisor.Close()
	var accounts, families, devices, babies, care int
	if err := supervisor.QueryRow(ctx, `SELECT (SELECT count(*) FROM accounts),(SELECT count(*) FROM families),(SELECT count(*) FROM devices),(SELECT count(*) FROM care_entities WHERE kind=1),(SELECT count(*) FROM care_entities WHERE kind=4)`).Scan(&accounts, &families, &devices, &babies, &care); err != nil {
		return err
	}
	if accounts != r.Spec.Families || families != r.Spec.Families || devices != len(r.Devices) || babies != r.Spec.Families || care != r.Spec.Families*r.Spec.CareEntriesPerBaby {
		return ErrFixtureInvalid
	}

	var grants int
	if err := supervisor.QueryRow(ctx, "SELECT count(*) FROM mcp_integrations WHERE status='active'").Scan(&grants); err != nil ||
		grants != r.Spec.Families*r.Spec.MCPGrantsPerFamily || grants != len(r.Integrations) {
		return ErrFixtureInvalid
	}
	// Every private credential is live for its own family (no event written).
	for _, integration := range r.Integrations {
		identity, err := r.mcpReader.CheckIntegration(ctx, integration.Credential)
		if err != nil || identity.GrantID != integration.GrantID || identity.FamilyID != integration.FamilyID {
			return ErrFixtureInvalid
		}
	}
	var liveSessions int
	if err := supervisor.QueryRow(ctx, "SELECT count(*) FROM sessions WHERE status='active'").Scan(&liveSessions); err != nil || liveSessions != len(r.Devices) {
		return ErrFixtureInvalid
	}
	r.credentialDeadline = time.Time{}
	for _, device := range r.Devices {
		var reauthenticate, refresh, absolute time.Time
		var account, familyID string
		// deadline_at is both the reauthentication and the absolute deadline.
		if err := supervisor.QueryRow(ctx, "SELECT deadline_at,refresh_expires_at,deadline_at,account_id::text,family_id::text FROM sessions WHERE device_id=$1 AND status='active'", device.Session.DeviceID).Scan(&reauthenticate, &refresh, &absolute, &account, &familyID); err != nil {
			return err
		}
		if !reauthenticate.Equal(device.Session.ReauthenticateAt) || account != device.Session.Account.ID || familyID != device.FamilyID {
			return ErrFixtureInvalid
		}
		deadline := reauthenticate
		if refresh.Before(deadline) {
			deadline = refresh
		}
		if absolute.Before(deadline) {
			deadline = absolute
		}
		if !time.Now().Before(deadline) {
			return ErrFixtureInvalid
		}
		if r.credentialDeadline.IsZero() || deadline.Before(r.credentialDeadline) {
			r.credentialDeadline = deadline
		}
	}
	if !r.Manifest.ExpiresAt.IsZero() && !r.Manifest.ExpiresAt.Equal(r.credentialDeadline) {
		return ErrFixtureInvalid
	}
	seen := map[string]bool{}
	for _, device := range r.Devices {
		if seen[device.FamilyID] {
			continue
		}
		seen[device.FamilyID] = true
		p, err := r.Auth.Authenticate(ctx, device.Session.AccessToken)
		if auth.Code(err) == "access_expired" {
			continue
		} // reusable copies validate/refresh below
		if err != nil {
			return err
		}
		err = r.Authority.WithFamilyReadTx(ctx, p, "read", func(tx pgx.Tx, g access.Grant) error {
			if g.FamilyID != device.FamilyID {
				return ErrFixtureInvalid
			}
			var babies, care int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM care_entities WHERE family_id=$1 AND kind=1 AND tombstoned_at IS NULL", g.FamilyID).Scan(&babies); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM care_entities WHERE family_id=$1 AND kind=4 AND baby_id=$2 AND tombstoned_at IS NULL", g.FamilyID, device.BabyID).Scan(&care); err != nil {
				return err
			}
			if babies != 1 || care != r.Spec.CareEntriesPerBaby {
				return ErrFixtureInvalid
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if len(seen) != r.Spec.Families {
		return ErrFixtureInvalid
	}
	return nil
}

func loadFixture(directory string) (fixtureSeal, fixturePrivate, error) {
	var seal fixtureSeal
	var private fixturePrivate
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return seal, private, ErrFixtureInvalid
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return seal, private, ErrFixtureInvalid
	}
	if err := readFixtureJSON(filepath.Join(directory, "seal.json"), &seal); err != nil {
		return seal, private, err
	}
	if err := seal.Manifest.Validate(seal.Spec, seal.Manifest.SourceSHA, time.Now()); err != nil {
		return seal, private, err
	}
	var manifest FixtureManifest
	if err := readFixtureJSON(filepath.Join(directory, "manifest.json"), &manifest); err != nil || manifest != seal.Manifest {
		return seal, private, ErrFixtureInvalid
	}
	actual, err := fixtureArtifactDigests(directory)
	if err != nil || len(actual) != len(seal.Artifacts) {
		return seal, private, ErrFixtureInvalid
	}
	for path, digest := range seal.Artifacts {
		if actual[path] != digest {
			return seal, private, ErrFixtureInvalid
		}
	}
	if err := readFixtureJSON(filepath.Join(directory, "private.json"), &private); err != nil {
		return seal, private, err
	}
	if private.AuthorityEvents != manifest.ActualAuthorityEvents || len(private.Devices) != manifest.Devices || len(private.Integrations) != manifest.MCPGrants {
		return seal, private, ErrFixtureInvalid
	}
	for _, device := range private.Devices {
		if device.Session.ReauthenticateAt.Before(manifest.ExpiresAt) || !time.Now().Before(device.Session.ReauthenticateAt) {
			return seal, private, ErrFixtureInvalid
		}
	}
	return seal, private, nil
}

// RestoreFixture never changes the sealed input. Credentials are genuinely
// refreshed in this owned copy, and the resulting authority head is recorded.
func RestoreFixture(ctx context.Context, directory, destination string) (_ *RestoredFixture, resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := disposableDestination(destination); err != nil {
		return nil, err
	}
	seal, private, err := loadFixture(directory)
	if err != nil {
		return nil, err
	}
	if err := localDocker(ctx); err != nil {
		return nil, err
	}
	db, err := reservePostgres("", destination)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			resultErr = errors.Join(resultErr, db.Close())
		}
	}()
	if err := os.Mkdir(destination, 0700); err != nil {
		return nil, err
	}
	defer func() {
		if !success {
			resultErr = errors.Join(resultErr, os.RemoveAll(destination))
		}
	}()
	paths := make([]string, 0, len(seal.Artifacts))
	for path := range seal.Artifacts {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if path == "database.dump" || path == "private.json" {
			continue
		}
		if filepath.IsAbs(path) || filepath.Clean(path) != path || strings.HasPrefix(path, "../") {
			return nil, ErrFixtureInvalid
		}
		output := filepath.Join(destination, path)
		if err := os.MkdirAll(filepath.Dir(output), 0700); err != nil {
			return nil, err
		}
		raw, err := os.ReadFile(filepath.Join(directory, path))
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(output, raw, 0600); err != nil {
			return nil, err
		}
	}
	cfg := private.Config
	_, err = startPostgres(ctx, db)
	if err != nil {
		return nil, err
	}
	if err := db.restore(ctx, filepath.Join(directory, "database.dump")); err != nil {
		return nil, err
	}
	r, _, err := composeFixture(ctx, cfg, db, false)
	if err != nil {
		return nil, err
	}
	r.Config.DatabaseURL = db.Runtime.Config().ConnString()
	r.root = destination
	r.Manifest = seal.Manifest
	r.Spec = seal.Spec
	r.SourceInputSHA256 = private.SourceInputSHA256
	r.Devices = private.Devices
	r.Integrations = private.Integrations
	if err := verifyFixtureState(ctx, r, private.AuthorityEvents); err != nil {
		return nil, err
	}
	for i := range r.Devices {
		bundle, err := r.Auth.Refresh(ctx, r.Devices[i].Session.RefreshToken)
		if err != nil {
			return nil, err
		}
		r.Devices[i].Session = bundle
		p, err := r.Auth.Authenticate(ctx, bundle.AccessToken)
		if err != nil {
			return nil, err
		}
		view, err := r.Families.Current(ctx, p)
		if err != nil || view.ID != r.Devices[i].FamilyID {
			return nil, ErrFixtureInvalid
		}
	}
	events, err := r.authorityEvents(ctx)
	if err != nil {
		return nil, err
	}
	r.RestoredAuthorityEvents = events
	// A genuine refresh appends no audit row (owner decision D1, 2026-10-16):
	// the restored history is exactly the sealed one.
	if events != private.AuthorityEvents {
		return nil, ErrFixtureInvalid
	}
	if err := verifyFixtureState(ctx, r, events); err != nil {
		return nil, err
	}
	success = true
	return r, nil
}

func (r *RestoredFixture) Close() error {
	if r == nil {
		return nil
	}
	if err := r.db.Close(); err != nil {
		return err
	}
	if r.root != "" {
		if err := os.RemoveAll(r.root); err != nil {
			return errors.New("capacity_cleanup_private_material_remains")
		}
		if _, err := os.Lstat(r.root); !os.IsNotExist(err) {
			return errors.New("capacity_cleanup_private_material_remains")
		}
		r.root = ""
	}
	return nil
}
