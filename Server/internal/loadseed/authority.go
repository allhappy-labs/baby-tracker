//go:build identitytest

package loadseed

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/config"
	"babytracker/server/internal/family"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/liveactivity"
	"babytracker/server/internal/loadtest"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/pushtoken"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/internal/testidentity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// services are the product's services the seeder runs every command
// through, composed over env.Runtime as the server composes them
// (cmd/baby-tracker-server authorityOptions, runtimeServices,
// applicationOptions), with the test identity issuer as Apple.
type services struct {
	cfg          config.Config
	mode         config.Mode
	authority    *authority.Service
	families     *family.Service
	identity     *identity.Service
	auth         *auth.Service
	sync         *familysync.Service
	pushTokens   *pushtoken.Service
	liveActivity *liveactivity.Service
	issuer       *testidentity.Issuer
	server       protocol.ServerInfo
	// runID names this seed's Apple subjects, so a second seed into the
	// same database signs in new accounts.
	runID  string
	logger *slog.Logger
}

// DefaultBundleID is the Live Activity bundle ID when the config names
// none: the load node's (deploy/load compose.yaml BT_APNS_BUNDLE_ID).
const DefaultBundleID = "com.example.babytracker.load"

func digestKeyring(source config.DigestKeyringConfig) identity.DigestKeyring {
	result := identity.DigestKeyring{Current: identity.DigestKey{ID: source.Current.ID, Secret: source.Current.Secret}}
	for _, key := range source.Previous {
		result.Previous = append(result.Previous, identity.DigestKey{ID: key.ID, Secret: key.Secret})
	}
	return result
}

// issuerApple is the hosted adapter's Apple code exchange
// (cmd/baby-tracker-server identityTestApple): the code is answered with a
// proof the test issuer signs for the expected identity.
type issuerApple struct {
	issuer   *testidentity.Issuer
	audience string
}

func (apple issuerApple) ExchangeCode(_ context.Context, request identity.AppleCodeExchangeRequest) (identity.AppleTokens, error) {
	if request.AuthorizationCode == "" {
		return identity.AppleTokens{}, identity.ErrInvalidAppleProof
	}
	proof, err := apple.issuer.Sign(apple.audience, request.ExpectedIdentity.Subject, request.ExpectedNonce)
	if err != nil {
		return identity.AppleTokens{}, err
	}
	return identity.AppleTokens{IdentityToken: proof, RefreshToken: "load-seed-refresh-" + request.AuthorizationCode}, nil
}

// StackMarker is the comment a disposable load stack's database carries
// (`COMMENT ON DATABASE … IS 'baby-tracker-load-stack'`, set by
// `load-node.sh up` after migrating). The seeder writes nothing to a
// database without it.
const StackMarker = "baby-tracker-load-stack"

// stackMarkerSQL reads the current database's comment.
const stackMarkerSQL = `SELECT current_database(), coalesce(shobj_description(oid, 'pg_database'), '') FROM pg_database WHERE datname = current_database()`

// CheckStack returns pool's database name, and refuses unless that database
// is marked as a disposable load stack (StackMarker). The seeder and the
// post-run audit run it before anything else touches the database.
func CheckStack(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	var name, comment string
	if err := pool.QueryRow(ctx, stackMarkerSQL).Scan(&name, &comment); err != nil {
		return "", fmt.Errorf("read the database's load-stack marker: %w", err)
	}
	if comment != StackMarker {
		return name, fmt.Errorf("database %q is not marked as a disposable load stack (COMMENT ON DATABASE … IS '%s')", name, StackMarker)
	}
	return name, nil
}

// checkStack refuses unless both pools reach the same database and that
// database is marked as a disposable load stack (StackMarker). It runs
// before anything is written: the target identifies itself, whatever the
// seeder's configuration says.
func checkStack(ctx context.Context, env Environment) error {
	var names [2]string
	for i, pool := range []*pgxpool.Pool{env.Runtime, env.Superuser} {
		name, err := CheckStack(ctx, pool)
		if err != nil {
			return fmt.Errorf("loadseed: refusing to seed: %w", err)
		}
		names[i] = name
	}
	if names[0] != names[1] {
		return fmt.Errorf("loadseed: the runtime and superuser pools reach different databases (%q, %q)", names[0], names[1])
	}
	return nil
}

// refuseSeeded refuses a database that already holds families: a load
// stack is seeded once, fresh after `load-node.sh up` (a second seed would
// mix two seeds' histories and collide on their synthetic tokens). It
// reads as the superuser, past the tenant policies.
func refuseSeeded(ctx context.Context, env Environment) error {
	var families int64
	if err := env.Superuser.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM families LIMIT 1) f`).Scan(&families); err != nil {
		return fmt.Errorf("loadseed: count families: %w", err)
	}
	if families > 0 {
		return errors.New("loadseed: refusing to seed: the database already holds families (it was seeded before); seed a fresh load stack")
	}
	return nil
}

// compose builds the services and checks that the database is this node's:
// server_state names the config's instance, epoch and actor-ID key.
func compose(ctx context.Context, env Environment) (*services, error) {
	cfg := env.Config
	if env.Runtime == nil || env.Superuser == nil || cfg.PublicOrigin == nil {
		return nil, errors.New("loadseed: the environment needs the runtime and superuser pools and a public origin")
	}
	if err := checkStack(ctx, env); err != nil {
		return nil, err
	}
	mode := cfg.Mode
	if mode == config.ModeDevelopment {
		mode = config.ModeSelfHosted // as the server's authenticationMode
	}
	state, err := authority.LoadServerState(ctx, env.Runtime)
	if err != nil {
		return nil, fmt.Errorf("loadseed: server_state: %w", err)
	}
	if err := state.Check(cfg.ServerInstanceID, cfg.RecoveryEpoch, cfg.Auth.ActorIDPepper); err != nil {
		return nil, fmt.Errorf("loadseed: the database is not this node's: %w", err)
	}
	origin := cfg.PublicOrigin.String()
	s := &services{cfg: cfg, mode: mode, logger: env.Logger}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	s.authority, err = authority.New(authority.Options{Pool: env.Runtime, Now: time.Now, InstanceID: cfg.ServerInstanceID, RecoveryEpoch: cfg.RecoveryEpoch,
		TokenKeys: digestKeyring(cfg.Auth.SessionTokenKeys), MaxCaregivers: cfg.Limits.MaxCaregivers, MaxInvitations: cfg.Limits.MaxInvitations,
		RequireFamilySyncEntitlement: mode == config.ModeHosted, Allowed: family.Allowed, EntitlementExempt: authority.LegacyEntitlementExempt})
	if err != nil {
		return nil, err
	}
	ttl := cfg.Limits.InvitationTTL
	if ttl == 0 {
		ttl = 7 * 24 * time.Hour
	}
	s.families, err = family.New(family.Options{Authority: s.authority, InvitationKeys: digestKeyring(cfg.Auth.InvitationKeys), Origin: origin,
		Now: time.Now, InvitationTTL: ttl})
	if err != nil {
		return nil, err
	}
	s.issuer, err = testidentity.New("load-seed", time.Now)
	if err != nil {
		return nil, err
	}
	verifier, err := s.issuer.Verifier(cfg.AppleAudience)
	if err != nil {
		return nil, err
	}
	s.identity, err = identity.New(identity.Options{Verifier: verifier, Authority: s.authority.IdentityAuthority(), Origin: origin,
		InstanceID: cfg.ServerInstanceID, IdentityKeys: digestKeyring(cfg.Auth.IdentityKeys), Now: time.Now})
	if err != nil {
		return nil, err
	}
	options := auth.Options{Identity: s.identity, Verifier: verifier, Authority: s.authority, Mode: mode, Origin: origin,
		InstanceID: cfg.ServerInstanceID, TokenKeys: digestKeyring(cfg.Auth.SessionTokenKeys), DeviceKeys: digestKeyring(cfg.Auth.DeviceContinuityKeys),
		ActorIDKey: cfg.Auth.ActorIDPepper, Now: time.Now}
	authorization := "identity_token_only"
	if mode == config.ModeHosted {
		options.Apple, authorization = issuerApple{issuer: s.issuer, audience: cfg.AppleAudience}, "code_exchange_required"
	}
	s.auth, err = auth.New(options)
	if err != nil {
		return nil, err
	}
	limits := cfg.Sync.Limits
	s.server = protocol.ServerInfo{InstanceID: cfg.ServerInstanceID, RecoveryEpoch: cfg.RecoveryEpoch, CanonicalOrigin: origin,
		APIVersions: []string{"v1"}, Capabilities: protocol.ServerCapabilities(), AppleAuthorizationMode: authorization, Limits: &limits}
	s.sync = familysync.New(familysync.Options{Guard: s.authority, Server: s.server, Limits: limits, Now: time.Now,
		PageKeys: digestKeyring(cfg.Auth.PageTokenKeys), Allowed: family.Allowed})
	s.pushTokens = pushtoken.New(s.authority, time.Now)
	bundle := cfg.LiveActivity.Credentials.BundleID
	if bundle == "" {
		bundle = DefaultBundleID
	}
	s.liveActivity = liveactivity.NewService(s.authority, bundle+".push-type.liveactivity")
	var run [6]byte
	if _, err := rand.Read(run[:]); err != nil {
		return nil, err
	}
	s.runID = hex.EncodeToString(run[:])
	return s, nil
}

// familyState is a provisioned family and its devices (four; the hot
// family's ten).
type familyState struct {
	Index    int
	FamilyID string
	Viewer   bool
	Hot      bool
	Devices  []*loadtest.DeviceState
	// MCPGrantID is the owner's integration grant, if any.
	MCPGrantID string
}

// writers is the family's owner and (first) caregiver with their devices,
// the writers encodeFamily assigns changes to. The hot family's other
// caregivers write nothing in the seeded history.
func (f *familyState) writers() []writer {
	byRole := func(role string) writer {
		var w writer
		for _, d := range f.Devices {
			if d.Role == role && (w.AccountID == "" || w.AccountID == d.AccountID) {
				w.AccountID = d.AccountID
				w.DeviceIDs = append(w.DeviceIDs, d.DeviceID)
			}
		}
		return w
	}
	return []writer{byRole("owner"), byRole("caregiver")}
}

func newUUID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	return shapeUUIDv4(value[:])
}

// clientIPBase is the first address of the load's client range: device n
// is 100.64.0.0 + n.
var clientIPBase = netip.MustParseAddr("100.64.0.0")

func clientIP(device int) string {
	b := clientIPBase.As4()
	value := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	value += uint32(device)
	return netip.AddrFrom4([4]byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)}).String()
}

// signIn signs subject in on a new installation, as a phone does: a
// challenge, the Apple proof bound to its nonce (and, hosted, the
// authorization code), and the exchange, which registers a new device.
func (s *services) signIn(ctx context.Context, subject, installation, name string) (auth.SessionBundle, error) {
	challenge, err := s.identity.Challenge(ctx)
	if err != nil {
		return auth.SessionBundle{}, fmt.Errorf("challenge: %w", err)
	}
	nonce, err := identity.AppleNonce(s.cfg.PublicOrigin.String(), s.cfg.ServerInstanceID, challenge.Challenge)
	if err != nil {
		return auth.SessionBundle{}, err
	}
	proof, err := s.issuer.Sign(s.cfg.AppleAudience, subject, nonce)
	if err != nil {
		return auth.SessionBundle{}, err
	}
	request := identity.ExchangeRequest{CommandID: newUUID(), Challenge: challenge.Challenge, IdentityToken: proof, InstallationID: installation,
		Profile: &identity.ProfileInput{DisplayName: name, Confirmed: true}}
	if s.mode == config.ModeHosted {
		request.AuthorizationCode = "load-seed-" + installation
	}
	bundle, err := s.auth.Exchange(ctx, request)
	if err != nil {
		return auth.SessionBundle{}, fmt.Errorf("exchange: %w", err)
	}
	return bundle, nil
}

// device is the fleet state of a signed-in bundle.
func device(familyID, role string, bundle auth.SessionBundle, continuity string, index int) *loadtest.DeviceState {
	return &loadtest.DeviceState{FamilyID: familyID, AccountID: bundle.Account.ID, DeviceID: bundle.DeviceID, Role: role,
		AccessToken: bundle.AccessToken, RefreshToken: bundle.RefreshToken, DeviceCredential: continuity,
		AccessExpiresAt: bundle.ExpiresAt.UTC(), ReauthenticateAt: bundle.ReauthenticateAt.UTC(), ClientIP: clientIP(index)}
}

// refresh rotates d's session with its refresh token (a genuine refresh, as
// a phone's), keeping its device continuity.
func (s *services) refresh(ctx context.Context, d *loadtest.DeviceState) error {
	bundle, err := s.auth.Refresh(ctx, d.RefreshToken)
	if err != nil {
		return fmt.Errorf("refresh %s: %w", d.DeviceID, err)
	}
	d.AccessToken, d.RefreshToken = bundle.AccessToken, bundle.RefreshToken
	d.AccessExpiresAt, d.ReauthenticateAt = bundle.ExpiresAt.UTC(), bundle.ReauthenticateAt.UTC()
	return nil
}

// fresh refreshes d when its access token expires within margin, as a
// phone refreshes before a request.
func (s *services) fresh(ctx context.Context, d *loadtest.DeviceState, margin time.Duration) error {
	if time.Until(d.AccessExpiresAt) > margin {
		return nil
	}
	return s.refresh(ctx, d)
}

// principal authenticates d's access token.
func (s *services) principal(ctx context.Context, d *loadtest.DeviceState) (access.Principal, error) {
	return s.auth.Authenticate(ctx, d.AccessToken)
}

// request is d's credential as the request seam takes it (the route's own
// header and rate checks are the HTTP handler's, not the seeder's).
func (s *services) request(d *loadtest.DeviceState) (authority.Request, error) {
	digests, err := s.auth.AccessDigests(d.AccessToken)
	if err != nil {
		return authority.Request{}, err
	}
	return authority.Request{Digests: digests}, nil
}

// provision creates family i through the product's commands:
//   - the owner signs in (device 0), creates the family and, hosted, the
//     operator grants it family_sync;
//   - the owner signs in on a second device;
//   - the owner invites a caregiver, who signs in and redeems (device 2);
//   - a viewer family invites a viewer who does the same (device 3), else
//     the caregiver signs in on a second device;
//   - every device refreshes to family scope;
//   - an MCP family's owner is issued an integration grant.
func (s *services) provision(ctx context.Context, spec Spec, i int) (*familyState, error) {
	viewer, mcp := familyShape(spec, i)
	f := &familyState{Index: i, Viewer: viewer}
	subject := func(role string) string { return fmt.Sprintf("load-%s-%d-%s", s.runID, i, role) }
	installation := func(slot int) string { return fmt.Sprintf("load-%s-%d-%d", s.runID, i, slot) }
	base := i * DevicesPerFamily

	owner, err := s.signIn(ctx, subject("owner"), installation(0), fmt.Sprintf("Owner %d", i))
	if err != nil {
		return nil, err
	}
	p, err := s.auth.Authenticate(ctx, owner.AccessToken)
	if err != nil {
		return nil, err
	}
	view, err := s.families.Create(ctx, p, newUUID(), fmt.Sprintf("Family %d", i))
	if err != nil {
		return nil, fmt.Errorf("create family: %w", err)
	}
	f.FamilyID = view.ID
	if s.mode == config.ModeHosted {
		if err := s.grantEntitlement(ctx, f.FamilyID); err != nil {
			return nil, err
		}
	}
	f.Devices = append(f.Devices, device(f.FamilyID, "owner", owner, owner.DeviceCredential, base))
	second, err := s.signIn(ctx, subject("owner"), installation(1), fmt.Sprintf("Owner %d", i))
	if err != nil {
		return nil, err
	}
	f.Devices = append(f.Devices, device(f.FamilyID, "owner", second, second.DeviceCredential, base+1))
	if err := s.refresh(ctx, f.Devices[0]); err != nil {
		return nil, err
	}
	ownerPrincipal, err := s.principal(ctx, f.Devices[0])
	if err != nil {
		return nil, err
	}
	join := func(role string, slot int) (*loadtest.DeviceState, error) {
		invitation, err := s.families.Invite(ctx, ownerPrincipal, newUUID(), role)
		if err != nil {
			return nil, fmt.Errorf("invite %s: %w", role, err)
		}
		bundle, err := s.signIn(ctx, subject(role), installation(slot), fmt.Sprintf("%s %d", role, i))
		if err != nil {
			return nil, err
		}
		p, err := s.auth.Authenticate(ctx, bundle.AccessToken)
		if err != nil {
			return nil, err
		}
		if _, err := s.families.Redeem(ctx, p, newUUID(), invitation.Token, true); err != nil {
			return nil, fmt.Errorf("redeem %s: %w", role, err)
		}
		return device(f.FamilyID, role, bundle, bundle.DeviceCredential, base+slot), nil
	}
	caregiver, err := join("caregiver", 2)
	if err != nil {
		return nil, err
	}
	f.Devices = append(f.Devices, caregiver)
	if viewer {
		d, err := join("viewer", 3)
		if err != nil {
			return nil, err
		}
		f.Devices = append(f.Devices, d)
	} else {
		bundle, err := s.signIn(ctx, subject("caregiver"), installation(3), fmt.Sprintf("caregiver %d", i))
		if err != nil {
			return nil, err
		}
		f.Devices = append(f.Devices, device(f.FamilyID, "caregiver", bundle, bundle.DeviceCredential, base+3))
	}
	for _, d := range f.Devices[1:] {
		if err := s.refresh(ctx, d); err != nil {
			return nil, err
		}
	}
	if mcp {
		grantID, credential, err := s.grantIntegration(ctx, f.Devices[0].AccountID, f.FamilyID)
		if err != nil {
			return nil, err
		}
		f.MCPGrantID, f.Devices[0].MCPCredential = grantID, credential
	}
	return f, nil
}

// provisionHot creates the hot family, index i, through the product's
// commands: the owner signs in on two devices and creates the family (with
// family_sync, hosted); each of HotFamilyAccounts-1 caregivers is invited,
// signs in and redeems on its first device, then signs in on its second;
// every device refreshes to family scope. It has no viewer and no MCP grant.
func (s *services) provisionHot(ctx context.Context, i int) (*familyState, error) {
	f := &familyState{Index: i, Hot: true}
	subject := func(account int) string { return fmt.Sprintf("load-%s-hot-%d-%d", s.runID, i, account) }
	installation := func(slot int) string { return fmt.Sprintf("load-%s-hot-%d-%d", s.runID, i, slot) }
	base := i * DevicesPerFamily
	owner, err := s.signIn(ctx, subject(0), installation(0), "Hot owner")
	if err != nil {
		return nil, err
	}
	p, err := s.auth.Authenticate(ctx, owner.AccessToken)
	if err != nil {
		return nil, err
	}
	view, err := s.families.Create(ctx, p, newUUID(), "Hot family")
	if err != nil {
		return nil, fmt.Errorf("create the hot family: %w", err)
	}
	f.FamilyID = view.ID
	if s.mode == config.ModeHosted {
		if err := s.grantEntitlement(ctx, f.FamilyID); err != nil {
			return nil, err
		}
	}
	f.Devices = append(f.Devices, device(f.FamilyID, "owner", owner, owner.DeviceCredential, base))
	second, err := s.signIn(ctx, subject(0), installation(1), "Hot owner")
	if err != nil {
		return nil, err
	}
	f.Devices = append(f.Devices, device(f.FamilyID, "owner", second, second.DeviceCredential, base+1))
	if err := s.refresh(ctx, f.Devices[0]); err != nil {
		return nil, err
	}
	ownerPrincipal, err := s.principal(ctx, f.Devices[0])
	if err != nil {
		return nil, err
	}
	for account := 1; account < HotFamilyAccounts; account++ {
		invitation, err := s.families.Invite(ctx, ownerPrincipal, newUUID(), "caregiver")
		if err != nil {
			return nil, fmt.Errorf("invite hot caregiver %d: %w", account, err)
		}
		name := fmt.Sprintf("Hot caregiver %d", account)
		for slot := range HotFamilyDevicesPerAccount {
			n := account*HotFamilyDevicesPerAccount + slot
			bundle, err := s.signIn(ctx, subject(account), installation(n), name)
			if err != nil {
				return nil, err
			}
			if slot == 0 {
				p, err := s.auth.Authenticate(ctx, bundle.AccessToken)
				if err != nil {
					return nil, err
				}
				if _, err := s.families.Redeem(ctx, p, newUUID(), invitation.Token, true); err != nil {
					return nil, fmt.Errorf("redeem hot caregiver %d: %w", account, err)
				}
			}
			f.Devices = append(f.Devices, device(f.FamilyID, "caregiver", bundle, bundle.DeviceCredential, base+n))
		}
	}
	for _, d := range f.Devices[1:] {
		if err := s.refresh(ctx, d); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// loadSeedOperator is the operator the seeder's operator commands name.
const loadSeedOperator = "10ad5eed-0000-4000-8000-000000000001"

// grantEntitlement grants the family hosted family_sync, as
// `baby-tracker-admin` does (the operator entitlement command).
func (s *services) grantEntitlement(ctx context.Context, familyID string) error {
	revision, err := s.authority.FamilyRevision(ctx, familyID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	digest := sha256.Sum256([]byte("load-seed-entitlement\x00" + familyID))
	_, err = s.authority.ExecuteOperatorEntitlement(ctx, authority.OperatorEntitlement{CommandID: newUUID(), OperatorID: loadSeedOperator,
		FamilyID: familyID, Operation: "grant", Key: "family_sync", Source: "hosted_beta", ExpiresAt: now.Add(400 * 24 * time.Hour).Truncate(time.Second),
		PolicyID: "LoadSeed.Policy-v1", EvidenceDigest: hex.EncodeToString(digest[:]), ReasonCode: "beta_extension", ExpectedRevision: revision,
		ConfirmationExpiresAt: now.Add(10 * time.Minute).Truncate(time.Second)})
	if err != nil {
		return fmt.Errorf("grant family_sync: %w", err)
	}
	return nil
}

// grantIntegration issues one MCP integration grant to the owner through
// the operator command, as `baby-tracker-admin` does
// (internal/capacity issueFixtureIntegration).
func (s *services) grantIntegration(ctx context.Context, accountID, familyID string) (string, string, error) {
	grantID := newUUID()
	secret, err := authority.NewIntegrationSecret()
	if err != nil {
		return "", "", err
	}
	credential, err := authority.FormatIntegrationCredential(grantID, secret)
	if err != nil {
		return "", "", err
	}
	_, verifier, ok := authority.IntegrationCredentialVerifier(credential)
	if !ok {
		return "", "", errors.New("loadseed: integration credential has no verifier")
	}
	revision, err := s.authority.FamilyRevision(ctx, familyID)
	if err != nil {
		return "", "", err
	}
	digest := sha256.Sum256([]byte("load-seed-mcp-grant\x00" + grantID))
	_, err = s.authority.ExecuteOperatorIntegration(ctx, authority.OperatorIntegration{CommandID: newUUID(), OperatorID: loadSeedOperator,
		Operation: "grant", GrantID: grantID, FamilyID: familyID, AccountID: accountID, ServerInstanceID: s.cfg.ServerInstanceID,
		RecoveryEpoch: s.cfg.RecoveryEpoch, PolicyID: "LoadSeed.Policy-v1", EvidenceDigest: hex.EncodeToString(digest[:]),
		ReasonCode: "integration_issued", ExpectedRevision: revision, ConfirmationExpiresAt: time.Now().UTC().Add(10 * time.Minute).Truncate(time.Second),
		Name: "Load reader", Scope: authority.IntegrationScopeFamilyRead, Verifier: verifier, ValiditySeconds: authority.IntegrationValiditySeconds})
	if err != nil {
		return "", "", fmt.Errorf("grant integration: %w", err)
	}
	return grantID, credential, nil
}

// provisionAll provisions spec.Families families, spec.Workers at a time,
// and the hot family after them (spec.HotFamily).
func (s *services) provisionAll(ctx context.Context, spec Spec) ([]*familyState, error) {
	families := make([]*familyState, spec.SeededFamilies())
	var mu sync.Mutex
	done := 0
	err := forEach(ctx, spec.Workers, len(families), func(i int) error {
		var f *familyState
		var err error
		if i == spec.Families {
			f, err = s.provisionHot(ctx, i)
		} else {
			f, err = s.provision(ctx, spec, i)
		}
		if err != nil {
			return fmt.Errorf("loadseed: provision family %d: %w", i, err)
		}
		families[i] = f
		mu.Lock()
		done++
		if done%1000 == 0 {
			s.logger.Info("loadseed: families provisioned", "families", done)
		}
		mu.Unlock()
		return nil
	})
	return families, err
}

// liveSession is a seeded device's live session, as the backlog copies
// it.
type liveSession struct {
	AccountID, DeviceID, LineageRootID, Scope string
	FamilyID, Role                            *string
	LineageStartedAt, DeadlineAt              time.Time
}

func readLiveSessions(ctx context.Context, env Environment, devices []string) (map[string]liveSession, error) {
	rows, err := env.Superuser.Query(ctx, `SELECT account_id::text, device_id::text, lineage_root_id::text, scope::text, family_id::text, role::text,
		lineage_started_at, deadline_at FROM sessions WHERE status = 'active' AND device_id = ANY($1::uuid[])`, devices)
	if err != nil {
		return nil, fmt.Errorf("loadseed: live sessions: %w", err)
	}
	out := make(map[string]liveSession, len(devices))
	var s liveSession
	_, err = pgx.ForEachRow(rows, []any{&s.AccountID, &s.DeviceID, &s.LineageRootID, &s.Scope, &s.FamilyID, &s.Role, &s.LineageStartedAt, &s.DeadlineAt},
		func() error {
			if _, twice := out[s.DeviceID]; twice {
				return fmt.Errorf("loadseed: device %s has two live sessions", s.DeviceID)
			}
			out[s.DeviceID] = s
			return nil
		})
	if err != nil {
		return nil, err
	}
	if len(out) != len(devices) {
		return nil, fmt.Errorf("loadseed: %d of %d devices have a live session", len(out), len(devices))
	}
	return out, nil
}
