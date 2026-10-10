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
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/admission"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/config"
	"babytracker/server/internal/family"
	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/migrate"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/pushtoken"
	"babytracker/server/internal/testpg"
	"babytracker/server/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type readinessFunc func(context.Context) error

func (function readinessFunc) Check(ctx context.Context) error { return function(ctx) }

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

	options := sessionServiceOptions(cfg, nil, nil, nil)
	if options.ActorIDKey != cfg.Auth.ActorIDPepper {
		t.Fatal("session composition did not receive the permanent actor ID pepper")
	}
	for name, got := range map[string]identity.DigestKeyring{
		"session": options.TokenKeys, "device": options.DeviceKeys, "notification": composeDigestKeyring(cfg.Auth.AppleNotificationKeys),
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
	// The authority digests tokens with exactly auth's keyring (same keys,
	// same order): auth.New refuses any other.
	authorityConfig := authorityOptions(cfg, &pgxpool.Pool{})
	if !reflect.DeepEqual(authorityConfig.TokenKeys, composeDigestKeyring(cfg.Auth.SessionTokenKeys)) ||
		authorityConfig.InstanceID != cfg.ServerInstanceID || authorityConfig.MaxCaregivers != 12 || authorityConfig.MaxInvitations != 15 ||
		authorityConfig.Allowed == nil || authorityConfig.EntitlementExempt == nil || authorityConfig.Now == nil {
		t.Fatalf("authority options=%+v", authorityConfig)
	}
	for _, action := range []string{"read_receipt", "export", "export_family", "delete_family", "recover_family", "forfeit_recovery"} {
		if !authorityConfig.EntitlementExempt(action) {
			t.Fatalf("legacy entitlement exemption %s missing", action)
		}
	}
	if authorityConfig.EntitlementExempt("mutate_record") || !authorityConfig.Allowed("owner", "manage_members") || authorityConfig.Allowed("viewer", "mutate_record") {
		t.Fatal("composition did not wire the family role policy")
	}
	cfg.Auth.ActorIDPepper[0] = 9
	cfg.RecoveryEpoch = 1
	authorityService, err := authority.New(authorityOptions(cfg, &pgxpool.Pool{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.New(auth.Options{Identity: &identity.Service{}, Verifier: identity.IdentityVerifierFunc(nil), Authority: authorityService,
		Mode: config.ModeSelfHosted, Origin: "https://sync.example.test", InstanceID: cfg.ServerInstanceID,
		TokenKeys: composeDigestKeyring(cfg.Auth.SessionTokenKeys), DeviceKeys: composeDigestKeyring(cfg.Auth.DeviceContinuityKeys),
		ActorIDKey: cfg.Auth.ActorIDPepper, Now: time.Now}); err != nil {
		t.Fatalf("auth refused the composed authority keyring: %v", err)
	}
}

func TestAuthorityFamilyCompositionRequiresEntitlementOnlyInHostedMode(t *testing.T) {
	for _, test := range []struct {
		mode config.Mode
		want bool
	}{
		{mode: config.ModeDevelopment, want: false},
		{mode: config.ModeSelfHosted, want: false},
		{mode: config.ModeHosted, want: true},
	} {
		t.Run(string(test.mode), func(t *testing.T) {
			got := authorityOptions(config.Config{
				Mode: test.mode, Limits: config.LimitsConfig{MaxCaregivers: 12, MaxInvitations: 15},
			}, &pgxpool.Pool{})
			if got.MaxCaregivers != 12 || got.MaxInvitations != 15 || got.RequireFamilySyncEntitlement != test.want {
				t.Fatalf("settings=%+v, want entitlement=%t", got, test.want)
			}
		})
	}
}

// Hosted mode has no Apple code exchanger outside identitytest builds with
// BT_IDENTITYTEST_HOSTED_ADAPTER=1: composition refuses it before anything
// is opened, so a hosted node never serves without code exchange.
func TestHostedCompositionWithoutCodeExchangerFailsClosed(t *testing.T) {
	t.Setenv(identityTestHostedAdapterEnv, "")
	cfg := config.Config{Mode: config.ModeHosted, ServerInstanceID: "123e4567-e89b-42d3-a456-426614174000", RecoveryEpoch: 1}
	if _, _, err := runtimeServices(cfg, &pgxpool.Pool{}); !errors.Is(err, errHostedAdapterUnavailable) {
		t.Fatalf("hosted runtime services: %v", err)
	}
	if _, _, err := runtimeServices(config.Config{}, nil); err == nil {
		t.Fatal("runtime services accepted a missing runtime pool")
	}
	if _, _, err := backgroundServices(config.Config{}, nil, nil); err == nil {
		t.Fatal("background services accepted a missing background pool")
	}
}

// The cached key inventory refuses readiness while a live credential
// references a key the node is not configured with, names the key, and
// refuses until it is first computed.
func TestKeyInventoryRejectsUnconfiguredReferencedKeys(t *testing.T) {
	referenced := authority.KeyInventory{
		SessionToken: []string{"session.parent", "session.output"}, DeviceContinuity: []string{"device.receipt"},
		Identity: []string{"identity.old"}, Invitation: []string{"invitation.old"},
	}
	readErr := error(nil)
	keys := config.AuthConfig{
		SessionTokenKeys:     config.DigestKeyringConfig{Current: config.DigestKeyConfig{ID: "session.output"}, Previous: []config.DigestKeyConfig{{ID: "session.parent"}}},
		DeviceContinuityKeys: config.DigestKeyringConfig{Current: config.DigestKeyConfig{ID: "device.receipt"}},
		IdentityKeys:         config.DigestKeyringConfig{Current: config.DigestKeyConfig{ID: "identity.current"}},
		InvitationKeys:       config.DigestKeyringConfig{Current: config.DigestKeyConfig{ID: "invitation.current"}},
	}
	inventory := newKeyInventory(keys, func(context.Context) (authority.KeyInventory, error) { return referenced, readErr })
	if err := inventory.Check(); !errors.Is(err, errKeyInventoryPending) || authority.CodeOf(err) != "service_unavailable" {
		t.Fatalf("uncomputed inventory: %v", err)
	}
	if err := inventory.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := inventory.Check(); !errors.Is(err, authority.ErrCredentialKeyUnavailable) || !strings.Contains(err.Error(), "identity.old") {
		t.Fatalf("missing retained identity key: %v", err)
	}
	inventory.identity["identity.old"] = struct{}{}
	if err := inventory.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := inventory.Check(); !errors.Is(err, authority.ErrCredentialKeyUnavailable) || !strings.Contains(err.Error(), "invitation.old") {
		t.Fatalf("missing retained invitation key: %v", err)
	}
	inventory.invitation["invitation.old"] = struct{}{}
	if err := inventory.Refresh(t.Context()); err != nil || inventory.Check() != nil {
		t.Fatalf("complete retained keys rejected: %v %v", err, inventory.Check())
	}
	// Refresh-recovery keys: a consumed parent's session key and the device
	// key a receipt proves with are retained too.
	for _, id := range []string{"session.parent", "session.output", "device.receipt"} {
		configured := inventory.session
		if id == "device.receipt" {
			configured = inventory.device
		}
		delete(configured, id)
		if err := inventory.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := inventory.Check(); !errors.Is(err, authority.ErrCredentialKeyUnavailable) || !strings.Contains(err.Error(), id) {
			t.Fatalf("missing %s: %v", id, err)
		}
		configured[id] = struct{}{}
	}
	// A failed read keeps the last verdict.
	if err := inventory.Refresh(t.Context()); err != nil || inventory.Check() != nil {
		t.Fatal("inventory did not recover")
	}
	readErr = errors.New("database down")
	if err := inventory.Refresh(t.Context()); err == nil || inventory.Check() != nil {
		t.Fatalf("failed refresh replaced the verdict: %v", inventory.Check())
	}
	// Clients see credential_key_unavailable, never the key ID.
	if code := authority.CodeOf(fmt.Errorf("%w: session_token key x", authority.ErrCredentialKeyUnavailable)); code != "credential_key_unavailable" {
		t.Fatalf("code=%q", code)
	}
}

func TestKeyInventoryLoopRetriesUntilComputedThenStops(t *testing.T) {
	calls := atomic.Int32{}
	inventory := newKeyInventory(config.AuthConfig{}, func(context.Context) (authority.KeyInventory, error) {
		if calls.Add(1) < 3 {
			return authority.KeyInventory{}, errors.New("not yet")
		}
		return authority.KeyInventory{}, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runKeyInventoryLoop(ctx, inventory, time.Hour, time.Millisecond, time.Hour, nil)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for inventory.Check() != nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if inventory.Check() != nil || calls.Load() != 3 {
		t.Fatalf("inventory=%v calls=%d", inventory.Check(), calls.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("inventory loop ignored cancellation")
	}
	stop := startAuthorityLoops(t.Context(), nil, inventory, nil)
	stop()
}

// A computed inventory first refreshes after the offset (half an interval
// out of phase with the verifier on the shared background connection), then
// every interval; a nudge computes at once. The loop is the only
// computation: a nudge never computes by itself.
func TestKeyInventoryLoopIsOffsetAndNudgedSingleFlight(t *testing.T) {
	calls := atomic.Int32{}
	read := func(context.Context) (authority.KeyInventory, error) {
		calls.Add(1)
		return authority.KeyInventory{}, nil
	}
	waitCalls := func(want int32) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for calls.Load() < want && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if got := calls.Load(); got != want {
			t.Fatalf("calls=%d want %d", got, want)
		}
	}
	run := func(inventory *keyInventory, interval, retry, offset time.Duration) func() {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); runKeyInventoryLoop(ctx, inventory, interval, retry, offset, nil) }()
		return func() { cancel(); <-done }
	}
	// Offset: computed at startup, first loop refresh after the offset, the
	// next only after the (hour-long) interval.
	computed := newKeyInventory(config.AuthConfig{}, read)
	if err := computed.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	stop := run(computed, time.Hour, time.Hour, 10*time.Millisecond)
	waitCalls(2)
	time.Sleep(50 * time.Millisecond)
	waitCalls(2)
	stop()
	// Nudge: never computed, retry an hour away; a nudge computes at once,
	// and nudging without the loop computes nothing.
	calls.Store(0)
	pending := newKeyInventory(config.AuthConfig{}, read)
	pending.Nudge()
	pending.Nudge()
	time.Sleep(20 * time.Millisecond)
	waitCalls(0)
	stop = run(pending, time.Hour, time.Hour, time.Hour)
	waitCalls(1)
	if pending.Check() != nil {
		t.Fatalf("nudged inventory: %v", pending.Check())
	}
	time.Sleep(50 * time.Millisecond)
	waitCalls(1) // two nudges, one pending slot: one computation
	stop()
}

func TestSelfHostedCompositionDoesNotConstructAppleTransport(t *testing.T) {
	cfg := config.Config{Mode: config.ModeSelfHosted}
	options := sessionServiceOptions(cfg, nil, nil, nil)
	if options.Apple != nil {
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

func testApplicationConfig(mode config.Mode) config.Config {
	origin, _ := url.Parse("https://sync.example.test")
	var identityPepper, sessionPepper, devicePepper, notificationPepper, invitationPepper, pagePepper, actorPepper, ratePepper [32]byte
	identityPepper[0], sessionPepper[0], devicePepper[0], notificationPepper[0], invitationPepper[0], pagePepper[0], actorPepper[0], ratePepper[0] = 1, 2, 3, 4, 5, 6, 7, 8
	return config.Config{
		Admission: config.AdmissionConfig{Total: 2, MCPMax: 1},
		Mode:      mode, PublicOrigin: origin, ServerInstanceID: "123e4567-e89b-42d3-a456-426614174000", RecoveryEpoch: 1,
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
}

// testComposition composes the authority, family service and readiness
// over pool (which need not be reachable: nothing connects until used).
func testComposition(t *testing.T, cfg config.Config, pool *pgxpool.Pool) (*authority.Service, *family.Service, *databaseReadiness) {
	t.Helper()
	authorityService, err := authority.New(authorityOptions(cfg, pool))
	if err != nil {
		t.Fatal(err)
	}
	familyService, err := family.New(family.Options{Authority: authorityService, InvitationKeys: composeDigestKeyring(cfg.Auth.InvitationKeys),
		Origin: cfg.PublicOrigin.String(), Now: time.Now, InvitationTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	// No budget: a zero pgxpool.Pool (unit compositions) has no config, and
	// readiness checks budgets only for an explicit profile. pool stands in
	// for the background and maintenance pools; nothing probes them.
	readiness, err := newDatabaseReadiness(cfg, readinessPools{Readiness: pool, Maintenance: []*pgxpool.Pool{pool}, MaintenanceLogin: maintenanceLogin},
		authorityService, nil)
	if err != nil {
		t.Fatal(err)
	}
	return authorityService, familyService, readiness
}

func TestReadinessCompositionRequiresEveryPool(t *testing.T) {
	cfg := testApplicationConfig(config.ModeSelfHosted)
	pool := &pgxpool.Pool{}
	authorityService, err := authority.New(authorityOptions(cfg, pool))
	if err != nil {
		t.Fatal(err)
	}
	for name, pools := range map[string]readinessPools{
		"no readiness pool":    {Maintenance: []*pgxpool.Pool{pool}, MaintenanceLogin: maintenanceLogin},
		"no maintenance":       {Readiness: pool, MaintenanceLogin: maintenanceLogin},
		"nil maintenance":      {Readiness: pool, Maintenance: []*pgxpool.Pool{nil}, MaintenanceLogin: maintenanceLogin},
		"no maintenance login": {Readiness: pool, Maintenance: []*pgxpool.Pool{pool}},
	} {
		if _, err := newDatabaseReadiness(cfg, pools, authorityService, nil); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if _, err := newDatabaseReadiness(cfg, readinessPools{Readiness: pool, Maintenance: []*pgxpool.Pool{pool}, MaintenanceLogin: maintenanceLogin}, nil, nil); err == nil {
		t.Fatal("no background authority accepted")
	}
}

func TestApplicationOptionsAdvertiseAuthorizationModeAndDependencies(t *testing.T) {
	cfg := testApplicationConfig(config.ModeSelfHosted)
	pool := &pgxpool.Pool{}
	authorityService, familyService, readiness := testComposition(t, cfg, pool)
	options, err := applicationOptions(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)), readiness, authorityService, familyService, pool, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if options.AppleAuthorizationMode != "identity_token_only" || options.ChallengeService == nil || options.Limiter == nil || options.ClientIPResolver == nil ||
		options.SessionService == nil || options.RecoveryService == nil || options.AppleNotifications != nil || options.FamilyService != familyService ||
		options.Readiness != readiness || options.Gate != readiness || options.ServerState != readiness || options.RecoveryReadiness != (nodeReadinessCovered{}) ||
		options.RequestAuthorizer != authorityService || options.AccountService == nil || options.LiveHub == nil {
		t.Fatalf("options=%+v", options)
	}
	if options.Admission == nil {
		t.Fatal("composition omitted admission controller")
	}
	mcpRelease, mcpOK := options.Admission.TryAcquire(admission.MCP)
	syncRelease, syncOK := options.Admission.TryAcquire(admission.Sync)
	if !mcpOK || !syncOK {
		t.Fatal("configured two-slot controller did not admit MCP and Sync")
	}
	if release, ok := options.Admission.TryAcquire(admission.Sync); ok {
		release()
		t.Fatal("composition ignored total limit")
	}
	mcpRelease()
	syncRelease()
	policies := cfg.Limits.Policies
	if options.FamilyStructuralAccountPolicy != policies[limits.ScopeFamilyStructuralAccount] ||
		options.FamilyStructuralDevicePolicy != policies[limits.ScopeFamilyStructuralDevice] ||
		options.InvitationCreateAccountPolicy != policies[limits.ScopeInvitationCreateAccount] ||
		options.InvitationCreateIPPolicy != policies[limits.ScopeInvitationCreateIP] ||
		options.InvitationRedeemAccountPolicy != policies[limits.ScopeInvitationRedeemAccount] ||
		options.InvitationRedeemIPPolicy != policies[limits.ScopeInvitationRedeemIP] {
		t.Fatalf("family policies=%+v", options)
	}
	// Refresh recovery stays opt-in through the maintenance credential.
	if options.RecoveryEnabled {
		t.Fatal("refresh recovery advertised without its opt-in")
	}
	cfg.RecoveryCleanup.Enabled = true
	if options, err = applicationOptions(cfg, nil, readiness, authorityService, familyService, pool, nil, nil); err != nil || !options.RecoveryEnabled {
		t.Fatalf("refresh recovery opt-in: %v", err)
	}
	// Hosted mode without the identitytest Apple exchanger has no code
	// exchange: composition fails closed instead of serving sign-in.
	t.Setenv(identityTestHostedAdapterEnv, "")
	hosted := testApplicationConfig(config.ModeHosted)
	hostedAuthority, hostedFamilies, hostedReadiness := testComposition(t, hosted, pool)
	if _, err := applicationOptions(hosted, nil, hostedReadiness, hostedAuthority, hostedFamilies, pool, nil, nil); !errors.Is(err, errHostedAdapterUnavailable) {
		t.Fatalf("hosted without code exchange: %v", err)
	}
	if _, err := applicationOptions(cfg, nil, nil, authorityService, familyService, pool, nil, nil); err == nil {
		t.Fatal("composition accepted a missing readiness gate")
	}
}

// The push-token route and push-token-v1 exist only with APNs wakes
// configured (decision D4): without them the route is not registered and
// the capability is dropped, as export-v1 is without export.
func TestApplicationOptionsComposePushTokensOnlyWithAPNSWakes(t *testing.T) {
	cfg := testApplicationConfig(config.ModeSelfHosted)
	pool := &pgxpool.Pool{}
	authorityService, familyService, readiness := testComposition(t, cfg, pool)
	options, err := applicationOptions(cfg, nil, readiness, authorityService, familyService, pool, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if options.PushTokens != nil || slices.Contains(options.Capabilities, protocol.CapabilityPushTokenV1) {
		t.Fatalf("push off: tokens %v capabilities %v", options.PushTokens, options.Capabilities)
	}
	put := httptest.NewRecorder()
	httpapi.New(options).ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/v1/devices/push-token", nil))
	if put.Code != http.StatusNotFound {
		t.Fatalf("push off: PUT %d", put.Code)
	}
	cfg.APNSWakes.Enabled = true
	if options, err = applicationOptions(cfg, nil, readiness, authorityService, familyService, pool, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := options.PushTokens.(*pushtoken.Service); !ok || !slices.Contains(options.Capabilities, protocol.CapabilityPushTokenV1) {
		t.Fatalf("push on: tokens %T capabilities %v", options.PushTokens, options.Capabilities)
	}
	if options.PushTokenDevicePolicy != cfg.Limits.Policies[limits.ScopePushTokenDevice] ||
		options.PushTokenAccountPolicy != cfg.Limits.Policies[limits.ScopePushTokenAccount] || options.PushTokenDevicePolicy.Count != 10 {
		t.Fatalf("push-token policies %+v %+v", options.PushTokenDevicePolicy, options.PushTokenAccountPolicy)
	}
	if options.BearerFailureIPPolicy != cfg.Limits.Policies[limits.ScopeBearerFailureIP] || options.BearerFailureIPPolicy.Count != 60 {
		t.Fatalf("failed-bearer policy %+v", options.BearerFailureIPPolicy)
	}
	put = httptest.NewRecorder()
	httpapi.New(options).ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/v1/devices/push-token", nil))
	if put.Code == http.StatusNotFound {
		t.Fatal("push on: the route is not registered")
	}
	delete(cfg.Limits.Policies, limits.ScopePushTokenAccount)
	if _, err := applicationOptions(cfg, nil, readiness, authorityService, familyService, pool, nil, nil); err == nil {
		t.Fatal("composition accepted a missing push-token policy")
	}
}

// A listening but silent dependency catches unbounded connection handshakes as
// well as a startup ping before the HTTP listener exists.
func TestServerStartsWithoutDatabase(t *testing.T) {
	address, _ := dependencyProxy(t, "")
	server := startServer(t, "postgres://runtime:private-test-password@"+address+"/test?sslmode=disable",
		"postgres://maintenance:private-test-password@"+address+"/test?sslmode=disable")
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
	databaseURL := testpg.RequireDatabase(t)
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
	runtimeRole := "liveness_runtime_" + suffix
	if _, err := admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{runtimeRole}.Sanitize()+" LOGIN PASSWORD 'disposable-test-only' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "GRANT baby_tracker_runtime TO "+pgx.Identifier{runtimeRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP ROLE "+pgx.Identifier{runtimeRole}.Sanitize()); err != nil {
			t.Error(err)
		}
	})
	// The maintenance credential is required (decision D3): the server
	// expects its login to be the isolated maintenance member.
	if _, err := admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{maintenanceLogin}.Sanitize()+" LOGIN PASSWORD 'disposable-test-only' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP ROLE "+pgx.Identifier{maintenanceLogin}.Sanitize()); err != nil {
			t.Error(err)
		}
	})
	if _, err := admin.Exec(ctx, "GRANT baby_tracker_maintenance TO "+pgx.Identifier{maintenanceLogin}.Sanitize()); err != nil {
		t.Fatal(err)
	}
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
	maintenanceURL := *u
	maintenanceURL.User = url.UserPassword(maintenanceLogin, "disposable-test-only")
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	address, available := dependencyProxy(t, u.Host)
	runtimeURL.Host, maintenanceURL.Host = address, address
	server := startServer(t, runtimeURL.String(), maintenanceURL.String())
	base := server.base
	assertStatus(t, base, "/healthz", 200)
	assertStatus(t, base, "/readyz", 503)
	available.Store(true)
	assertStatus(t, base, "/readyz", 503) // Reachable, but migrations are pending.
	if err := migrate.New(migrations.Files).Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, base, "/readyz", 503) // Migrated, but `authority init` has not run.
	if result, err := authority.InitServerState(ctx, pool, server.instanceID, server.actorKey); err != nil || result != authority.InitCreated {
		t.Fatalf("authority init: %s %v", result, err)
	}
	// Epoch 1 is what init writes; the server is configured for it.
	assertEventuallyStatus(t, base, "/readyz", 200)
	// The monitor's probe has a deadline, even with a healthy connection:
	// a probe stuck behind a lock fails readiness closed, while every
	// request still answers at once from the cached state.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "LOCK schema_compatibility IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	assertEventuallyStatus(t, base, "/readyz", 503)
	started := time.Now()
	assertStatus(t, base, "/readyz", 503)
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("cached readiness took %v to answer", elapsed)
	}
	assertStatus(t, base, "/healthz", 200)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	assertEventuallyStatus(t, base, "/readyz", 200)
	// A server_state whose actor-ID key commitment is not the configured
	// key's fails closed; restoring it recovers without a restart.
	var original []byte
	if err := pool.QueryRow(ctx, "SELECT actor_key_commitment FROM server_state WHERE singleton").Scan(&original); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE server_state SET actor_key_commitment=$1 WHERE singleton", bytes.Repeat([]byte{0xff}, 32)); err != nil {
		t.Fatal(err)
	}
	assertEventuallyStatus(t, base, "/readyz", 503)
	if _, err := pool.Exec(ctx, "UPDATE server_state SET actor_key_commitment=$1 WHERE singleton", original); err != nil {
		t.Fatal(err)
	}
	assertEventuallyStatus(t, base, "/readyz", 200)
	// A rotated epoch fails closed until the node restarts with it.
	if _, err := authority.RotateRecoveryEpoch(ctx, pool, server.instanceID, 1); err != nil {
		t.Fatal(err)
	}
	assertEventuallyStatus(t, base, "/readyz", 503)
	t.Log("same process: cold outage health=200 ready=503; reachable pending=503; migrated uninitialized=503; initialized=200; locked probe=503 (cached answers at once); actor key mismatch=503; restored=200; rotated epoch=503")
}

type startedServer struct {
	base       string
	instanceID string
	actorKey   [32]byte
}

func startServer(t *testing.T, databaseURL, maintenanceURL string) startedServer {
	t.Helper()
	maintenanceFile := filepath.Join(t.TempDir(), "maintenance-database-url")
	if err := os.WriteFile(maintenanceFile, []byte(maintenanceURL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const instanceID = "123e4567-e89b-12d3-a456-426614174000"
	sessionPepper := t.TempDir() + "/session-pepper"
	if err := os.WriteFile(sessionPepper, []byte(strings.Repeat("02", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	pagePepper := t.TempDir() + "/page-token-key"
	if err := os.WriteFile(pagePepper, []byte(strings.Repeat("08", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	actorPepper := filepath.Join(t.TempDir(), "actor-id-pepper")
	if err := os.WriteFile(actorPepper, []byte(strings.Repeat("05", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	var actorKey [32]byte
	for index := range actorKey {
		actorKey[index] = 0x05
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	command := exec.Command(os.Args[0], "-test.run=^TestServerProcess$")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "BT_") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "BT_TEST_SERVER_PROCESS=1", "BT_MODE=development",
		"BT_LISTEN_ADDRESS="+address, "BT_DATABASE_URL="+databaseURL, "BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE="+maintenanceFile,
		"BT_PUBLIC_ORIGIN=https://sync.example.test", "BT_SERVER_INSTANCE_ID="+instanceID,
		"BT_RECOVERY_EPOCH=1",
		"BT_APPLE_AUDIENCE=test", "BT_SESSION_SIGNING_KEY_FILE="+sessionPepper, "BT_TRUSTED_PROXY_CIDRS=127.0.0.1/32",
		"BT_IDENTITY_KEY_ID=identity.v1", "BT_IDENTITY_PEPPER="+strings.Repeat("01", 32),
		"BT_SESSION_TOKEN_KEY_ID=session.v1",
		"BT_DEVICE_CONTINUITY_KEY_ID=device.v1", "BT_DEVICE_CONTINUITY_PEPPER="+strings.Repeat("03", 32),
		"BT_APPLE_NOTIFICATION_KEY_ID=notification.v1", "BT_APPLE_NOTIFICATION_PEPPER="+strings.Repeat("04", 32),
		"BT_INVITATION_KEY_ID=invitation.v1", "BT_INVITATION_PEPPER="+strings.Repeat("07", 32),
		"BT_PAGE_TOKEN_KEY_ID=page.v1", "BT_PAGE_TOKEN_KEY_FILE="+pagePepper,
		"BT_ACTOR_ID_PEPPER_FILE="+actorPepper, "BT_RATE_LIMIT_PEPPER="+strings.Repeat("06", 32))
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
		if t.Failed() {
			t.Logf("server output:\n%s", output.String())
		}
	})
	base := "http://" + address
	client := &http.Client{Timeout: 100 * time.Millisecond}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(base + "/healthz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				return startedServer{base: base, instanceID: instanceID, actorKey: actorKey}
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
