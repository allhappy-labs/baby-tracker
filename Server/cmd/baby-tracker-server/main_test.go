package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/config"
	"babytracker/server/internal/family"
	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/migrate"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/recovery"
	"babytracker/server/internal/security"
	"babytracker/server/internal/security/localstore"
	"babytracker/server/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type readinessFunc func(context.Context) error

func (function readinessFunc) Check(ctx context.Context) error { return function(ctx) }

type credentialKeyInventoryFunc func(context.Context) (security.CredentialKeyInventory, error)

func (function credentialKeyInventoryFunc) ReferencedCredentialKeyIDs(ctx context.Context) (security.CredentialKeyInventory, error) {
	return function(ctx)
}

type familyAuthorizerFunc func(context.Context, access.Principal, string, string) (access.Grant, error)

func (function familyAuthorizerFunc) Authorize(ctx context.Context, principal access.Principal, familyID, action string) (access.Grant, error) {
	return function(ctx, principal, familyID, action)
}

func TestFamilyAuthorizationDecisionFailsClosedUntilCompositionCompletes(t *testing.T) {
	var target familyAuthorizer
	decision := familyAuthorizationDecision(func() familyAuthorizer { return target })
	principal := access.Principal{AccountID: "account", SessionID: "session", RecoveryEpoch: 1}

	if grant, err := decision(t.Context(), principal, "family", "read"); err == nil || security.Code(err) != "service_unavailable" || grant != (access.Grant{}) {
		t.Fatalf("unbound decision grant=%+v code=%q err=%v", grant, security.Code(err), err)
	}

	want := access.Grant{AccountID: "account", FamilyID: "family", Role: "caregiver", Revision: 7}
	target = familyAuthorizerFunc(func(_ context.Context, gotPrincipal access.Principal, familyID, action string) (access.Grant, error) {
		if gotPrincipal != principal || familyID != "family" || action != "read" {
			t.Fatalf("delegated principal=%+v family=%q action=%q", gotPrincipal, familyID, action)
		}
		return want, nil
	})
	if grant, err := decision(t.Context(), principal, "family", "read"); err != nil || grant != want {
		t.Fatalf("bound decision grant=%+v err=%v", grant, err)
	}
}

func TestRunServerLifecycleJoinsLiveHubShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	shutdownStarted := make(chan struct{})
	releaseShutdown := make(chan struct{})
	returned := make(chan error, 1)
	go func() {
		returned <- runServerLifecycle(ctx, func(runCtx context.Context) error {
			if runCtx != ctx {
				t.Error("HTTP lifecycle received a different context")
			}
			return nil
		}, func(context.Context) error {
			close(shutdownStarted)
			<-releaseShutdown
			return nil
		})
	}()
	select {
	case <-shutdownStarted:
	case <-time.After(time.Second):
		t.Fatal("live shutdown did not start")
	}
	select {
	case err := <-returned:
		t.Fatalf("lifecycle returned before live shutdown completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseShutdown)
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("lifecycle error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("lifecycle did not return after live shutdown")
	}
}

func TestServerProcess(t *testing.T) {
	if os.Getenv("BT_TEST_SERVER_PROCESS") != "1" {
		return
	}
	os.Args = os.Args[:1]
	main()
	os.Exit(0)
}

func TestRuntimeGateRejectsSharedPool(t *testing.T) {
	pool := &pgxpool.Pool{}
	if _, err := runtimeGate(config.Config{}, pool, pool); err == nil {
		t.Fatal("runtime gate accepted one pool for runtime and projection")
	}
}

func TestComposeWiresActorIDPepperAndSixKeyrings(t *testing.T) {
	origin, err := url.Parse("https://sync.example.test")
	if err != nil {
		t.Fatal(err)
	}
	key := func(id string, value byte) config.DigestKeyConfig {
		var secret [32]byte
		secret[0] = value
		return config.DigestKeyConfig{ID: id, Secret: secret}
	}
	cfg := config.Config{
		Mode:             config.ModeSelfHosted,
		PublicOrigin:     origin,
		ServerInstanceID: "123e4567-e89b-42d3-a456-426614174000",
		Auth: config.AuthConfig{
			IdentityKeys:          config.DigestKeyringConfig{Current: key("identity.v2", 1), Previous: []config.DigestKeyConfig{key("identity.v1", 2)}},
			SessionTokenKeys:      config.DigestKeyringConfig{Current: key("session.v2", 3), Previous: []config.DigestKeyConfig{key("session.v1", 4)}},
			DeviceContinuityKeys:  config.DigestKeyringConfig{Current: key("device.v2", 5), Previous: []config.DigestKeyConfig{key("device.v1", 6)}},
			AppleNotificationKeys: config.DigestKeyringConfig{Current: key("notification.v2", 7), Previous: []config.DigestKeyConfig{key("notification.v1", 8)}},
			InvitationKeys:        config.DigestKeyringConfig{Current: key("invitation.v2", 10), Previous: []config.DigestKeyConfig{key("invitation.v1", 11)}},
			PageTokenKeys:         config.DigestKeyringConfig{Current: key("page.v2", 12), Previous: []config.DigestKeyConfig{key("page.v1", 13)}},
		},
		Limits: config.LimitsConfig{MaxCaregivers: 12, MaxInvitations: 15, InvitationTTL: 96 * time.Hour},
	}
	cfg.Auth.ActorIDPepper[0] = 9

	options := sessionServiceOptions(cfg, nil, nil, nil, nil)
	if options.ActorIDKey != cfg.Auth.ActorIDPepper {
		t.Fatal("session composition did not receive the permanent actor ID pepper")
	}
	for name, got := range map[string]identity.DigestKeyring{
		"session": options.TokenKeys, "device": options.DeviceKeys, "notification": options.NotificationKeys,
	} {
		if got.Current.ID == "" || len(got.Previous) != 1 || got.Current.Secret[0] == 0 || got.Previous[0].Secret[0] == 0 {
			t.Fatalf("%s keyring was not mapped completely: %+v", name, got)
		}
	}
	identityKeys := composeDigestKeyring(cfg.Auth.IdentityKeys)
	if identityKeys.Current.ID != "identity.v2" || len(identityKeys.Previous) != 1 || identityKeys.Previous[0].ID != "identity.v1" {
		t.Fatalf("identity keyring was not mapped completely: %+v", identityKeys)
	}
	invitationKeys := composeDigestKeyring(cfg.Auth.InvitationKeys)
	if invitationKeys.Current.ID != "invitation.v2" || len(invitationKeys.Previous) != 1 || invitationKeys.Previous[0].ID != "invitation.v1" {
		t.Fatalf("invitation keyring was not mapped completely: %+v", invitationKeys)
	}
	pageKeys := composeDigestKeyring(cfg.Auth.PageTokenKeys)
	if pageKeys.Current.ID != "page.v2" || len(pageKeys.Previous) != 1 || pageKeys.Previous[0].ID != "page.v1" {
		t.Fatalf("page keyring was not mapped completely: %+v", pageKeys)
	}
	// Mutating a mapped previous-key slice must not mutate configuration.
	options.TokenKeys.Previous[0].ID = "changed"
	if cfg.Auth.SessionTokenKeys.Previous[0].ID != "session.v1" {
		t.Fatal("composition retained the config keyring slice")
	}
	authority, repository := &security.Service{}, &family.Repository{}
	familyOptions := familyServiceOptions(cfg, authority, repository)
	if familyOptions.Security != authority || familyOptions.Repository != repository ||
		familyOptions.InvitationKeys.Current.ID != "invitation.v2" || len(familyOptions.InvitationKeys.Previous) != 1 ||
		familyOptions.MaxCaregivers != 12 || familyOptions.MaxInvitations != 15 || familyOptions.InvitationTTL != 96*time.Hour ||
		familyOptions.Origin != "https://sync.example.test" || familyOptions.Now == nil {
		t.Fatalf("family options=%+v", familyOptions)
	}
	if _, ok := familyOptions.Entitlements.(family.SelfHostedEntitlementPolicy); !ok {
		t.Fatalf("self-host entitlement policy=%T", familyOptions.Entitlements)
	}
	cfg.Mode = config.ModeHosted
	if _, ok := familyServiceOptions(cfg, authority, repository).Entitlements.(family.HostedEntitlementPolicy); !ok {
		t.Fatalf("hosted entitlement policy=%T", familyServiceOptions(cfg, authority, repository).Entitlements)
	}
}

func TestSecurityFamilyCompositionRequiresEntitlementOnlyInHostedMode(t *testing.T) {
	for _, test := range []struct {
		mode config.Mode
		want bool
	}{
		{mode: config.ModeDevelopment, want: false},
		{mode: config.ModeSelfHosted, want: false},
		{mode: config.ModeHosted, want: true},
	} {
		t.Run(string(test.mode), func(t *testing.T) {
			got := composeSecurityFamilySettings(config.Config{
				Mode: test.mode, Limits: config.LimitsConfig{MaxCaregivers: 12, MaxInvitations: 15},
			})
			if got.maxCaregivers != 12 || got.maxInvitations != 15 || got.requireEntitlement != test.want {
				t.Fatalf("settings=%+v, want entitlement=%t", got, test.want)
			}
		})
	}
}

func TestApplicationReadinessRejectsMissingCredentialKeysAndHostedGates(t *testing.T) {
	baseCalls := 0
	base := readinessFunc(func(context.Context) error { baseCalls++; return nil })
	inventory := credentialKeyInventoryFunc(func(context.Context) (security.CredentialKeyInventory, error) {
		return security.CredentialKeyInventory{
			Identity: []string{"identity.old"}, DeviceContinuity: []string{"device.old"}, Invitation: []string{"invitation.old"},
		}, nil
	})
	selfHosted := applicationReadiness{
		base: base, inventory: inventory, mode: config.ModeSelfHosted,
		identityKeyIDs:   map[string]struct{}{"identity.current": {}},
		deviceKeyIDs:     map[string]struct{}{"device.old": {}},
		invitationKeyIDs: map[string]struct{}{"invitation.current": {}},
	}
	if err := selfHosted.Check(t.Context()); err == nil || err.Error() != "credential_key_unavailable" {
		t.Fatalf("missing retained identity key error=%v", err)
	}
	selfHosted.identityKeyIDs["identity.old"] = struct{}{}
	if err := selfHosted.Check(t.Context()); err == nil || err.Error() != "credential_key_unavailable" {
		t.Fatalf("missing retained invitation key error=%v", err)
	}
	selfHosted.invitationKeyIDs["invitation.old"] = struct{}{}
	if err := selfHosted.Check(t.Context()); err != nil {
		t.Fatalf("self-host readiness rejected complete retained keys: %v", err)
	}

	hosted := selfHosted
	hosted.mode = config.ModeHosted
	if err := hosted.Check(t.Context()); err == nil || err.Error() != "hosted_adapter_unavailable" {
		t.Fatalf("hosted adapter gate error=%v", err)
	}
	hosted.hostedAdapterAvailable = true
	if err := hosted.Check(t.Context()); err == nil || err.Error() != "hosted_credential_rotation_unverified" {
		t.Fatalf("hosted live-rotation gate error=%v", err)
	}
	hosted.hostedCredentialRotationVerified = true
	if err := hosted.Check(t.Context()); err != nil {
		t.Fatalf("verified hosted readiness rejected: %v", err)
	}
	if baseCalls != 6 {
		t.Fatalf("base readiness checks=%d, want 6", baseCalls)
	}

	failingBase := errors.New("base-not-ready")
	hosted.base = readinessFunc(func(context.Context) error { return failingBase })
	if err := hosted.Check(t.Context()); !errors.Is(err, failingBase) {
		t.Fatalf("base readiness error was replaced: %v", err)
	}
}

func TestSelfHostedCompositionDoesNotConstructAppleTransport(t *testing.T) {
	cfg := config.Config{Mode: config.ModeSelfHosted}
	options := sessionServiceOptions(cfg, nil, nil, nil, nil)
	if options.Apple != nil || options.Credentials != nil {
		t.Fatal("self-host composition constructed hosted Apple dependencies")
	}
	if options.Mode != config.ModeSelfHosted {
		t.Fatalf("auth mode=%q", options.Mode)
	}
	var _ auth.Options = options
}

func TestProductionDependencyGraphExcludesTestIssuer(t *testing.T) {
	command := exec.Command("go", "list", "-deps", "./cmd/baby-tracker-server")
	command.Dir = "../.."
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("list production dependencies: %v: %s", err, output)
	}
	if strings.Contains(string(output), "babytracker/server/internal/testidentity") {
		t.Fatal("production server includes the test-only identity issuer")
	}
}

func TestApplicationOptionsAdvertiseAuthorizationModeAndDependencies(t *testing.T) {
	origin, err := url.Parse("https://sync.example.test")
	if err != nil {
		t.Fatal(err)
	}
	var identityPepper, sessionPepper, devicePepper, notificationPepper, invitationPepper, pagePepper, actorPepper, ratePepper [32]byte
	identityPepper[0], sessionPepper[0], devicePepper[0], notificationPepper[0], invitationPepper[0], pagePepper[0], actorPepper[0], ratePepper[0] = 1, 2, 3, 4, 5, 6, 7, 8
	cfg := config.Config{
		Mode: config.ModeHosted, PublicOrigin: origin, ServerInstanceID: "123e4567-e89b-42d3-a456-426614174000",
		AppleAudience: "com.example.babytracker", TrustedProxyCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
		Auth: config.AuthConfig{
			IdentityKeys:          config.DigestKeyringConfig{Current: config.DigestKeyConfig{ID: "identity.v1", Secret: identityPepper}},
			SessionTokenKeys:      config.DigestKeyringConfig{Current: config.DigestKeyConfig{ID: "session.v1", Secret: sessionPepper}},
			DeviceContinuityKeys:  config.DigestKeyringConfig{Current: config.DigestKeyConfig{ID: "device.v1", Secret: devicePepper}},
			AppleNotificationKeys: config.DigestKeyringConfig{Current: config.DigestKeyConfig{ID: "notification.v1", Secret: notificationPepper}},
			InvitationKeys:        config.DigestKeyringConfig{Current: config.DigestKeyConfig{ID: "invitation.v1", Secret: invitationPepper}},
			PageTokenKeys:         config.DigestKeyringConfig{Current: config.DigestKeyConfig{ID: "page.v1", Secret: pagePepper}},
			ActorIDPepper:         actorPepper, RateLimitPepper: ratePepper,
		},
		Limits: config.LimitsConfig{Policies: limits.DefaultPolicies()},
		Sync:   config.SyncConfig{Limits: protocol.DefaultLimits()},
	}
	familyService := &family.Service{}
	options, err := applicationOptions(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)), &recovery.Gate{}, &security.Service{}, familyService, &pgxpool.Pool{})
	if err != nil {
		t.Fatal(err)
	}
	if options.AppleAuthorizationMode != "code_exchange_required" || options.ChallengeService == nil || options.Limiter == nil || options.ClientIPResolver == nil ||
		options.SessionService != nil || options.AppleNotifications == nil || options.FamilyService != familyService {
		t.Fatalf("options=%+v", options)
	}
	// Unregistering a Live Activity installation needs no push configuration:
	// the repository-only path is composed whether or not push is enabled,
	// while the registration service and capability are not.
	if options.LiveActivityUnregistration == nil || options.LiveActivityService != nil || options.LiveActivityReadiness != nil {
		t.Fatalf("live activity options=%+v", options)
	}
	policies := cfg.Limits.Policies
	if options.FamilyStructuralAccountPolicy != policies[limits.ScopeFamilyStructuralAccount] ||
		options.FamilyStructuralDevicePolicy != policies[limits.ScopeFamilyStructuralDevice] ||
		options.InvitationCreateAccountPolicy != policies[limits.ScopeInvitationCreateAccount] ||
		options.InvitationCreateIPPolicy != policies[limits.ScopeInvitationCreateIP] ||
		options.InvitationRedeemAccountPolicy != policies[limits.ScopeInvitationRedeemAccount] ||
		options.InvitationRedeemIPPolicy != policies[limits.ScopeInvitationRedeemIP] {
		t.Fatalf("family policies=%+v", options)
	}
	cfg.Mode = config.ModeSelfHosted
	options, err = applicationOptions(cfg, nil, &recovery.Gate{}, &security.Service{}, familyService, &pgxpool.Pool{})
	if err != nil || options.AppleAuthorizationMode != "identity_token_only" || options.SessionService == nil || options.AppleNotifications != nil {
		t.Fatalf("self-host mode=%q err=%v", options.AppleAuthorizationMode, err)
	}
}

func TestHTTPAuthCompositionHasNoProjectorCapability(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	_, signature, found := strings.Cut(string(source), "func applicationOptions(")
	if !found {
		t.Fatal("applicationOptions composition entry point is missing")
	}
	signature, _, found = strings.Cut(signature, ") (httpapi.Options, error)")
	if !found || strings.Count(signature, "*pgxpool.Pool") != 1 || !strings.Contains(signature, "runtimePool *pgxpool.Pool") || strings.Contains(strings.ToLower(signature), "projector") {
		t.Fatalf("applicationOptions retained projector capability: %s", signature)
	}
	optionsType := reflect.TypeOf(httpapi.Options{})
	for index := 0; index < optionsType.NumField(); index++ {
		field := optionsType.Field(index)
		if strings.Contains(strings.ToLower(field.Name+field.Type.String()), "projector") {
			t.Fatalf("HTTP options expose projector capability through %s %s", field.Name, field.Type)
		}
	}
}

// A listening but silent dependency catches unbounded connection handshakes as
// well as a startup ping before the HTTP listener exists.
func TestServerStartsWithoutDatabase(t *testing.T) {
	address, _ := dependencyProxy(t, "")
	server := startServer(t, "postgres://runtime:private-test-password@"+address+"/test?sslmode=disable",
		"postgres://projector:private-test-password@"+address+"/test?sslmode=disable")
	base := server.base
	assertStatus(t, base, "/healthz", 200)
	started := time.Now()
	assertStatus(t, base, "/readyz", 503)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("database readiness exceeded its two-second budget: %v", elapsed)
	}
	assertStatus(t, base, "/healthz", 200)
}

func TestServerRecoversWithoutRestart(t *testing.T) {
	databaseURL := os.Getenv("BABY_TRACKER_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("BABY_TRACKER_TEST_DATABASE_URL is not set; requires disposable PostgreSQL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	bootstrap, err := os.ReadFile("../../deploy/postgres/init-roles.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, string(bootstrap)); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	name := "liveness_test_" + suffix
	runtimeRole, projectorRole := "liveness_runtime_"+suffix, "liveness_projector_"+suffix
	for _, role := range []string{runtimeRole, projectorRole} {
		if _, err := admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN PASSWORD 'disposable-test-only' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.Exec(ctx, "GRANT baby_tracker_runtime TO "+pgx.Identifier{runtimeRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "GRANT baby_tracker_projector TO "+pgx.Identifier{projectorRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, role := range []string{runtimeRole, projectorRole} {
			if _, err := admin.Exec(context.Background(), "DROP ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
				t.Error(err)
			}
		}
	})
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	runtimeURL := *u
	runtimeURL.User = url.UserPassword(runtimeRole, "disposable-test-only")
	projectorURL := *u
	projectorURL.User = url.UserPassword(projectorRole, "disposable-test-only")
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	address, available := dependencyProxy(t, u.Host)
	runtimeURL.Host = address
	projectorURL.Host = address
	server := startServer(t, runtimeURL.String(), projectorURL.String())
	base := server.base
	assertStatus(t, base, "/healthz", 200)
	assertStatus(t, base, "/readyz", 503)
	available.Store(true)
	assertStatus(t, base, "/readyz", 503) // Reachable, but migrations are pending.
	if err := migrate.New(migrations.Files).Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	assertEventuallyStatus(t, base, "/readyz", 200)
	// Migration checks also need a deadline, even with a healthy connection.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "LOCK schema_migrations IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	assertStatus(t, base, "/readyz", 503)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("migration readiness exceeded its two-second budget: %v", elapsed)
	}
	assertStatus(t, base, "/healthz", 200)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, base, "/readyz", 200)
	originalSidecar, err := os.ReadFile(server.actorSidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	backupSidecarPath := server.actorSidecarPath + ".original"
	if err := os.Rename(server.actorSidecarPath, backupSidecarPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server.actorSidecarPath, []byte(`{"version":"v1","instance_id":"123e4567-e89b-12d3-a456-426614174000","commitment":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertEventuallyStatus(t, base, "/readyz", 503)
	if err := os.Remove(server.actorSidecarPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backupSidecarPath, server.actorSidecarPath); err != nil {
		t.Fatal(err)
	}
	assertEventuallyStatus(t, base, "/readyz", 200)
	if restored, err := os.ReadFile(server.actorSidecarPath); err != nil || !bytes.Equal(restored, originalSidecar) {
		t.Fatalf("actor sidecar restore changed bytes: err=%v", err)
	}
	t.Log("same process: cold outage health=200 ready=503; reachable pending=503; migrated=200; locked migration check=503 within 3s; recovered=200")
}

type startedServer struct {
	base             string
	actorSidecarPath string
}

// serverCommand is the server process with the fixture's full configuration;
// extra environment entries are appended and win over the fixture's.
func serverCommand(t *testing.T, databaseURL, projectorDatabaseURL string, extra ...string) (command *exec.Cmd, address, headDir string) {
	t.Helper()
	ledgerDir, headDir := t.TempDir(), t.TempDir()
	sessionPepper := t.TempDir() + "/session-pepper"
	if err := os.WriteFile(sessionPepper, []byte(strings.Repeat("02", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	pagePepper := t.TempDir() + "/page-token-key"
	if err := os.WriteFile(pagePepper, []byte(strings.Repeat("08", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	stores, err := localstore.Open(ledgerDir, headDir)
	if err != nil {
		t.Fatal(err)
	}
	durable, err := recovery.NewVerifiedStore(headDir, stores.Fence, stores.Ledger, stores.Heads)
	if err != nil {
		t.Fatal(err)
	}
	actorPepper := filepath.Join(t.TempDir(), "actor-id-pepper")
	if err := os.WriteFile(actorPepper, []byte(strings.Repeat("05", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	actorKey, err := recovery.LoadActorIDPepperFile(actorPepper)
	if err != nil {
		t.Fatal(err)
	}
	if err := durable.InitializeAuthorityWithActorIDKey(t.Context(), stores, recovery.State{InstanceID: "123e4567-e89b-12d3-a456-426614174000", Epoch: 7, TrustedHead: security.Head{Generation: 1}}, actorKey); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address = listener.Addr().String()
	listener.Close()
	command = exec.Command(os.Args[0], "-test.run=^TestServerProcess$")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "BT_") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "BT_TEST_SERVER_PROCESS=1", "BT_MODE=development",
		"BT_LISTEN_ADDRESS="+address, "BT_DATABASE_URL="+databaseURL,
		"BT_PROJECTOR_DATABASE_URL="+projectorDatabaseURL,
		"BT_PUBLIC_ORIGIN=https://sync.example.test", "BT_SERVER_INSTANCE_ID=123e4567-e89b-12d3-a456-426614174000",
		"BT_RECOVERY_EPOCH=7", "BT_SECURITY_LEDGER_DIR="+ledgerDir, "BT_LEDGER_HEAD_DIR="+headDir,
		"BT_APPLE_AUDIENCE=test", "BT_SESSION_SIGNING_KEY_FILE="+sessionPepper, "BT_TRUSTED_PROXY_CIDRS=127.0.0.1/32",
		"BT_IDENTITY_KEY_ID=identity.v1", "BT_IDENTITY_PEPPER="+strings.Repeat("01", 32),
		"BT_SESSION_TOKEN_KEY_ID=session.v1",
		"BT_DEVICE_CONTINUITY_KEY_ID=device.v1", "BT_DEVICE_CONTINUITY_PEPPER="+strings.Repeat("03", 32),
		"BT_APPLE_NOTIFICATION_KEY_ID=notification.v1", "BT_APPLE_NOTIFICATION_PEPPER="+strings.Repeat("04", 32),
		"BT_INVITATION_KEY_ID=invitation.v1", "BT_INVITATION_PEPPER="+strings.Repeat("07", 32),
		"BT_PAGE_TOKEN_KEY_ID=page.v1", "BT_PAGE_TOKEN_KEY_FILE="+pagePepper,
		"BT_ACTOR_ID_PEPPER_FILE="+actorPepper, "BT_RATE_LIMIT_PEPPER="+strings.Repeat("06", 32))
	command.Env = append(command.Env, extra...)
	return command, address, headDir
}

func startServer(t *testing.T, databaseURL, projectorDatabaseURL string) startedServer {
	t.Helper()
	command, address, headDir := serverCommand(t, databaseURL, projectorDatabaseURL)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	base := "http://" + address
	client := &http.Client{Timeout: 100 * time.Millisecond}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(base + "/healthz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				return startedServer{base: base, actorSidecarPath: filepath.Join(headDir, "actor-id-key.json")}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("HTTP liveness never became available while the database was unavailable")
	return startedServer{}
}

func assertStatus(t *testing.T, base, path string, want int) {
	t.Helper()
	client := &http.Client{Timeout: 4 * time.Second}
	response, err := client.Get(base + path)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want {
		t.Fatalf("%s status=%d body=%s, want %d", path, response.StatusCode, body, want)
	}
	if want == 503 && !strings.Contains(string(body), `"code":"not_ready"`) {
		t.Fatalf("unsafe readiness response: %s", body)
	}
	for _, forbidden := range []string{"private-test-password", "postgres", "pgconn", "timeout", "schema_migrations"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("readiness leaked dependency detail: %s", body)
		}
	}
}

func assertEventuallyStatus(t *testing.T, base, path string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		client := &http.Client{Timeout: 3 * time.Second}
		response, err := client.Get(base + path)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == want {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	assertStatus(t, base, path, want)
}

// Each unavailable connection stalls until test cleanup or caller cancellation.
// Once enabled, new connections forward the real PostgreSQL wire protocol.
func dependencyProxy(t *testing.T, target string) (string, *atomic.Bool) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	available := &atomic.Bool{}
	done := make(chan struct{})
	t.Cleanup(func() { close(done); listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if !available.Load() {
					<-done
					return
				}
				upstream, err := net.DialTimeout("tcp", target, time.Second)
				if err != nil {
					return
				}
				defer upstream.Close()
				go func() { _, _ = io.Copy(upstream, conn); upstream.Close() }()
				_, _ = io.Copy(conn, upstream)
			}()
		}
	}()
	return listener.Addr().String(), available
}

func TestRecoveryKeyReadiness(t *testing.T) {
	readiness := applicationReadiness{base: readinessFunc(func(context.Context) error { return nil }), inventory: credentialKeyInventoryFunc(func(context.Context) (security.CredentialKeyInventory, error) {
		return security.CredentialKeyInventory{SessionToken: []string{"session.parent", "session.output"}, DeviceContinuity: []string{"device.receipt"}}, nil
	}), mode: config.ModeSelfHosted,
		sessionKeyIDs: map[string]struct{}{"session.parent": {}, "session.output": {}}, deviceKeyIDs: map[string]struct{}{"device.receipt": {}}}
	if err := readiness.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"session.parent", "session.output", "device.receipt"} {
		keys := readiness.sessionKeyIDs
		if id == "device.receipt" {
			keys = readiness.deviceKeyIDs
		}
		delete(keys, id)
		err := readiness.Check(t.Context())
		if !errors.Is(err, errCredentialKeyUnavailable) || !strings.Contains(err.Error(), id) {
			t.Fatalf("missing %s: %v", id, err)
		}
		keys[id] = struct{}{}
	}
}

// Malformed Live Activity configuration — an enabled flag without its APNs
// settings, or a key file that cannot be read — still fails fast: the process
// exits with status 1 before serving anything. Only readiness that is not yet
// ready is deferred, never a configuration fault.
func TestServerExitsOnMalformedLiveActivityConfiguration(t *testing.T) {
	address, _ := dependencyProxy(t, "")
	databaseURL := "postgres://runtime:private-test-password@" + address + "/test?sslmode=disable"
	projectorURL := "postgres://projector:private-test-password@" + address + "/test?sslmode=disable"
	for name, extra := range map[string][]string{
		"missing_apns_environment": {"BT_LIVE_ACTIVITY_PUSH_ENABLED=true"},
		"unreadable_key_file": {"BT_LIVE_ACTIVITY_PUSH_ENABLED=true", "BT_APNS_TEAM_ID=ABCDEFGHIJ", "BT_APNS_KEY_ID=KLMNOPQRST",
			"BT_APNS_BUNDLE_ID=com.example.babytracker", "BT_APNS_KEY_FILE=" + filepath.Join(t.TempDir(), "missing")},
	} {
		t.Run(name, func(t *testing.T) {
			command, listen, _ := serverCommand(t, databaseURL, projectorURL, extra...)
			var output bytes.Buffer
			command.Stdout, command.Stderr = &output, &output
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = command.Process.Kill() })
			exited := make(chan error, 1)
			go func() { exited <- command.Wait() }()
			select {
			case err := <-exited:
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 {
					t.Fatalf("malformed configuration exit=%v output=%s", err, output.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("malformed configuration kept the process running")
			}
			if !strings.Contains(output.String(), `"msg":"load configuration"`) {
				t.Fatalf("startup log=%s", output.String())
			}
			client := &http.Client{Timeout: 200 * time.Millisecond}
			if response, err := client.Get("http://" + listen + "/healthz"); err == nil {
				response.Body.Close()
				t.Fatal("malformed configuration served HTTP")
			}
		})
	}
}
