package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"syscall"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/account"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/config"
	"babytracker/server/internal/database"
	"babytracker/server/internal/delivery"
	"babytracker/server/internal/diagnostics"
	familyexport "babytracker/server/internal/export"
	"babytracker/server/internal/family"
	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/importer"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/liveactivity"
	"babytracker/server/internal/migrate"
	"babytracker/server/internal/probe"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
	"babytracker/server/internal/records"
	"babytracker/server/internal/recovery"
	"babytracker/server/internal/security"
	"babytracker/server/internal/security/localstore"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/migrations"

	"github.com/jackc/pgx/v5/pgxpool"
)

const maxHeaderBytes = 1 << 20

const identityTestHostedAdapterEnv = "BT_IDENTITYTEST_HOSTED_ADAPTER"

var sourceVersion = "unknown"

const sourceURL = "https://github.com/allhappy-labs/baby-tracker"

var (
	errCredentialKeyUnavailable           = errors.New("credential_key_unavailable")
	errHostedAdapterUnavailable           = errors.New("hosted_adapter_unavailable")
	errHostedCredentialRotationUnverified = errors.New("hosted_credential_rotation_unverified")
)

type credentialKeyInventory interface {
	ReferencedCredentialKeyIDs(context.Context) (security.CredentialKeyInventory, error)
}

type familyAuthorizer interface {
	Authorize(context.Context, access.Principal, string, string) (access.Grant, error)
}

type syncFamilyResolver struct{ service httpapi.FamilyService }

func (resolver syncFamilyResolver) Current(ctx context.Context, principal access.Principal) (string, error) {
	view, err := resolver.service.Current(ctx, principal)
	return view.ID, err
}

func familyAuthorizationDecision(current func() familyAuthorizer) security.Decision {
	return func(ctx context.Context, principal access.Principal, familyID, action string) (access.Grant, error) {
		if current == nil {
			return access.Grant{}, security.ErrUnavailable
		}
		target := current()
		if target == nil {
			return access.Grant{}, security.ErrUnavailable
		}
		return target.Authorize(ctx, principal, familyID, action)
	}
}

type authorityCredentialKeyInventory struct{ authority *security.Service }

func (inventory authorityCredentialKeyInventory) ReferencedCredentialKeyIDs(ctx context.Context) (security.CredentialKeyInventory, error) {
	if inventory.authority == nil {
		return security.CredentialKeyInventory{}, errors.New("security authority is required")
	}
	return inventory.authority.ReferencedCredentialKeyIDs(ctx)
}

type applicationReadiness struct {
	sessionKeyIDs                                  map[string]struct{}
	base                                           httpapi.Readiness
	inventory                                      credentialKeyInventory
	mode                                           config.Mode
	identityKeyIDs, deviceKeyIDs, invitationKeyIDs map[string]struct{}
	hostedAdapterAvailable                         bool
	hostedCredentialRotationVerified               bool
}

func (readiness applicationReadiness) Check(ctx context.Context) error {
	if readiness.base == nil || readiness.inventory == nil {
		return errors.New("service_unavailable")
	}
	if err := readiness.base.Check(ctx); err != nil {
		return err
	}
	referenced, err := readiness.inventory.ReferencedCredentialKeyIDs(ctx)
	if err != nil {
		return err
	}
	if !containsEveryKeyID(readiness.identityKeyIDs, referenced.Identity) ||
		!containsEveryKeyID(readiness.invitationKeyIDs, referenced.Invitation) {
		return errCredentialKeyUnavailable
	}
	for _, ids := range []struct {
		configured map[string]struct{}
		referenced []string
	}{
		{readiness.sessionKeyIDs, referenced.SessionToken}, {readiness.deviceKeyIDs, referenced.DeviceContinuity},
	} {
		for _, id := range ids.referenced {
			if _, ok := ids.configured[id]; !ok {
				return fmt.Errorf("%w: %s", errCredentialKeyUnavailable, id)
			}
		}
	}
	if readiness.mode == config.ModeHosted {
		if !readiness.hostedAdapterAvailable {
			return errHostedAdapterUnavailable
		}
		if !readiness.hostedCredentialRotationVerified {
			return errHostedCredentialRotationUnverified
		}
	}
	return nil
}

func containsEveryKeyID(configured map[string]struct{}, referenced []string) bool {
	for _, keyID := range referenced {
		if _, ok := configured[keyID]; !ok {
			return false
		}
	}
	return true
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "probe" {
		probeCommand(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "live-activity-preflight" {
		liveActivityPreflightCommand(os.Args[2:])
		return
	}
	if len(os.Args) > 1 {
		fmt.Fprintln(os.Stderr, "usage: baby-tracker-server [probe --url <health-url> | live-activity-preflight [--environment sandbox|production|both] [--skip-apple] [--skip-database]]")
		os.Exit(2)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := loadServerConfig()
	if err != nil {
		logger.Error("load configuration", slog.String("error", err.Error()))
		os.Exit(1)
	}
	pool, err := database.Open(context.Background(), cfg.DatabaseURL)
	if err != nil {
		logger.Error("open database", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer pool.Close()
	projectorPool, err := database.Open(context.Background(), cfg.ProjectorDatabaseURL)
	if err != nil {
		logger.Error("open projector database", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer projectorPool.Close()
	var exportPool *pgxpool.Pool
	if cfg.Export.Enabled {
		exportPool, err = database.Open(context.Background(), cfg.Export.WorkerDatabaseURL)
		if err != nil {
			logger.Error("open export worker database")
			os.Exit(1)
		}
		defer exportPool.Close()
		if err := verifyExportPool(context.Background(), exportPool, "baby_tracker_export_app"); err != nil {
			logger.Error("verify export worker database role")
			os.Exit(1)
		}
	}

	var recoveryPool *pgxpool.Pool
	if cfg.RecoveryCleanup.Enabled {
		if cfg.Export.Enabled && cfg.RecoveryCleanup.WorkerDatabaseURL == cfg.Export.WorkerDatabaseURL {
			recoveryPool = exportPool
		} else {
			recoveryPool, err = database.Open(context.Background(), cfg.RecoveryCleanup.WorkerDatabaseURL)
			if err != nil {
				logger.Error("open recovery maintenance database")
				os.Exit(1)
			}
			defer recoveryPool.Close()
			if err = verifyExportPool(context.Background(), recoveryPool, "baby_tracker_export_app"); err != nil {
				logger.Error("verify recovery maintenance database role")
				os.Exit(1)
			}
		}
	}

	gate, authority, familyService, err := runtimeServices(cfg, pool, projectorPool)
	if err != nil {
		logger.Error("construct runtime gate", slog.String("error", err.Error()))
		os.Exit(1)
	}
	options, err := applicationOptions(cfg, logger, gate, authority, familyService, pool)
	if err != nil {
		logger.Error("construct application services", slog.String("error", err.Error()))
		os.Exit(1)
	}
	if cfg.RecoveryCleanup.Enabled {
		options.RecoveryReadiness = recoveryFunctionReadiness{pool: recoveryPool}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Enabled push reconciles recovery before its first activation attempt, so
	// a healthy boot activates at once. Not ready yet (database or migrations
	// pending, recovery not reconcilable) is not an exit: the server starts not
	// ready, like a disabled one, the lazy reconcile loop runs as it does for
	// disabled servers, and supervision activates push once readiness passes.
	reconciledAtBoot := false
	if cfg.LiveActivity.Enabled {
		startup, cancelStartup := context.WithTimeout(ctx, 10*time.Second)
		err := gate.Reconcile(startup)
		cancelStartup()
		reconciledAtBoot = err == nil
		if err != nil {
			logger.Warn("live activity application readiness deferred")
		}
	}
	// Only malformed configuration or composition fails here; activation that
	// is not yet possible is deferred to the worker's supervision. A shutdown
	// signal during the boot attempt is a clean stop, not a failure.
	shutdownLiveActivities, err := configureLiveActivities(ctx, cfg, authority, pool, &options,
		newDatabasePoolBudget(pool, projectorPool, exportPool, recoveryPool))
	if liveActivityShutdownRequested(ctx, err) {
		logger.Info("shutdown requested during live activity push activation")
		return
	}
	if err != nil {
		logger.Error("configure live activity push", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer shutdownLiveActivities()
	var exportWorker *familyexport.Worker
	if cfg.Export.Enabled {
		identity := protocol.ServerInfo{InstanceID: cfg.ServerInstanceID, RecoveryEpoch: cfg.RecoveryEpoch,
			CanonicalOrigin: cfg.PublicOrigin.String(), APIVersions: []string{"v1"}, Capabilities: options.Capabilities,
			AppleAuthorizationMode: options.AppleAuthorizationMode, Limits: options.SyncLimits}
		options.ExportService, exportWorker, err = familyexport.New(familyexport.Options{Security: authority,
			Resolver: syncFamilyResolver{service: familyService}, Identity: identity, Runtime: pool,
			WorkerPool: exportPool, Keys: cfg.Export.Keys, ActiveKeyID: cfg.Export.ActiveKeyID, Now: time.Now})
		if err != nil {
			logger.Error("construct export service")
			os.Exit(1)
		}
	}
	server := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           wrapIdentityTestHandler(httpapi.New(options), cfg.AppleAudience),
		ConnContext:       realtime.TransportContext,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    maxHeaderBytes,
	}
	if recoveryPool != nil {
		cleanupContext, cancelCleanup := context.WithCancel(ctx)
		cleanupDone := make(chan struct{})
		go func() {
			defer close(cleanupDone)
			runRecoveryCleanup(cleanupContext, gate, recoveryCleanup{pool: recoveryPool, now: time.Now}, logger, recoveryCleanupInterval)
		}()
		defer func() { cancelCleanup(); <-cleanupDone }()
	}
	if exportWorker != nil {
		go runExportWorker(ctx, options.Readiness, exportWorker, logger)
	}
	listener, err := realtime.StartPostgresListener(ctx, realtime.PostgresOptions{Pool: pool, Hub: options.LiveHub,
		InstanceID: cfg.ServerInstanceID, RecoveryEpoch: cfg.RecoveryEpoch, Resyncer: syncCursorResyncer{service: options.SyncService}})
	if err != nil {
		logger.Error("construct live listener", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer listener.Close()
	if !reconciledAtBoot {
		go reconcileLoop(ctx, gate)
	}
	if err := runServerLifecycle(ctx, func(runContext context.Context) error {
		return httpapi.Run(runContext, server)
	}, options.LiveHub.Shutdown); err != nil {
		logger.Error("server stopped", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

type syncCursorResyncer struct{ service httpapi.SyncService }

func (resyncer syncCursorResyncer) Current(ctx context.Context, lease realtime.AuthorizationLease) (protocol.Cursor, error) {
	response, err := resyncer.service.ApplyAndPull(ctx, lease.Principal, protocol.SyncRequest{RequestMeta: protocol.RequestMeta{
		ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityRecordsV1}}, Mutations: []protocol.Mutation{}})
	return response.HighWater, err
}

func applicationOptions(cfg config.Config, logger *slog.Logger, gate *recovery.Gate, authority *security.Service,
	familyService *family.Service, runtimePool *pgxpool.Pool) (httpapi.Options, error) {
	if gate == nil || authority == nil || familyService == nil || runtimePool == nil {
		return httpapi.Options{}, errors.New("runtime gate, security authority, family service, and database pool are required")
	}
	identityRepository, err := identity.NewRepository(runtimePool)
	if err != nil {
		return httpapi.Options{}, err
	}
	verifier, err := newConfiguredIdentityVerifier(cfg)
	if err != nil {
		return httpapi.Options{}, err
	}
	identityService, err := identity.New(identity.Options{
		Verifier: verifier, Repository: identityRepository, Origin: cfg.PublicOrigin.String(), InstanceID: cfg.ServerInstanceID,
		IdentityKeys: composeDigestKeyring(cfg.Auth.IdentityKeys), Now: time.Now,
	})
	if err != nil {
		return httpapi.Options{}, err
	}
	authRepository, err := auth.NewRepository(runtimePool)
	if err != nil {
		return httpapi.Options{}, err
	}
	var sessionService httpapi.SessionService
	var recoverySessions *auth.Service
	var notificationService httpapi.AppleNotificationService
	hostedAdapterAvailable := false
	if cfg.Mode != config.ModeHosted {
		created, createErr := auth.New(sessionServiceOptions(cfg, identityService, verifier, authority, authRepository))
		err = createErr
		if err != nil {
			return httpapi.Options{}, err
		}
		sessionService = created
		recoverySessions = created
	} else {
		sessionOptions := sessionServiceOptions(cfg, identityService, verifier, authority, authRepository)
		hostedAdapterAvailable, err = configureIdentityTestHostedAuth(cfg, &sessionOptions)
		if err != nil {
			return httpapi.Options{}, err
		}
		if hostedAdapterAvailable {
			created, createErr := auth.New(sessionOptions)
			if createErr != nil {
				return httpapi.Options{}, createErr
			}
			sessionService = created
			recoverySessions = created
		}
		created, createErr := identity.NewNotificationService(identity.NotificationOptions{
			Audience: cfg.AppleAudience, InstanceID: cfg.ServerInstanceID,
			IdentityKeys: composeDigestKeyring(cfg.Auth.IdentityKeys), NotificationKeys: composeDigestKeyring(cfg.Auth.AppleNotificationKeys),
			Repository: identityRepository, Security: authority, Now: time.Now,
		})
		err = createErr
		if err != nil {
			return httpapi.Options{}, err
		}
		notificationService = created
	}
	limitRepository, err := limits.NewRepository(runtimePool)
	if err != nil {
		return httpapi.Options{}, err
	}
	limiter, err := limits.New(limits.Options{Repository: limitRepository, Pepper: cfg.Auth.RateLimitPepper, Now: time.Now})
	if err != nil {
		return httpapi.Options{}, err
	}
	resolver, err := limits.NewClientIPResolver(cfg.TrustedProxyCIDRs)
	if err != nil {
		return httpapi.Options{}, err
	}
	authorizationMode := "identity_token_only"
	if cfg.Mode == config.ModeHosted {
		authorizationMode = "code_exchange_required"
	}
	challengePolicy, ok := cfg.Limits.Policies[limits.ScopeAuthChallengeIP]
	if !ok {
		return httpapi.Options{}, errors.New("challenge rate-limit policy is required")
	}
	exchangePolicy, exchangeOK := cfg.Limits.Policies[limits.ScopeAuthExchangeIP]
	refreshIPPolicy, refreshIPOK := cfg.Limits.Policies[limits.ScopeAuthRefreshIP]
	refreshAccountPolicy, refreshAccountOK := cfg.Limits.Policies[limits.ScopeAuthRefreshAccount]
	refreshDevicePolicy, refreshDeviceOK := cfg.Limits.Policies[limits.ScopeAuthRefreshDevice]
	if !exchangeOK || !refreshIPOK || !refreshAccountOK || !refreshDeviceOK {
		return httpapi.Options{}, errors.New("session rate-limit policies are required")
	}
	familyStructuralAccountPolicy, familyStructuralAccountOK := cfg.Limits.Policies[limits.ScopeFamilyStructuralAccount]
	familyStructuralDevicePolicy, familyStructuralDeviceOK := cfg.Limits.Policies[limits.ScopeFamilyStructuralDevice]
	invitationCreateAccountPolicy, invitationCreateAccountOK := cfg.Limits.Policies[limits.ScopeInvitationCreateAccount]
	invitationCreateIPPolicy, invitationCreateIPOK := cfg.Limits.Policies[limits.ScopeInvitationCreateIP]
	invitationRedeemAccountPolicy, invitationRedeemAccountOK := cfg.Limits.Policies[limits.ScopeInvitationRedeemAccount]
	invitationRedeemIPPolicy, invitationRedeemIPOK := cfg.Limits.Policies[limits.ScopeInvitationRedeemIP]
	syncMutationDevicePolicy, syncMutationDeviceOK := cfg.Limits.Policies[limits.ScopeSyncMutationDevice]
	syncMutationAccountPolicy, syncMutationAccountOK := cfg.Limits.Policies[limits.ScopeSyncMutationAccount]
	syncMutationFamilyPolicy, syncMutationFamilyOK := cfg.Limits.Policies[limits.ScopeSyncMutationFamily]
	syncPullDevicePolicy, syncPullDeviceOK := cfg.Limits.Policies[limits.ScopeSyncPullDevice]
	syncPullAccountPolicy, syncPullAccountOK := cfg.Limits.Policies[limits.ScopeSyncPullAccount]
	liveAttemptIPPolicy, liveAttemptIPOK := cfg.Limits.Policies[limits.ScopeLiveAttemptIP]
	liveAttemptDevicePolicy, liveAttemptDeviceOK := cfg.Limits.Policies[limits.ScopeLiveAttemptDevice]
	liveAttemptAccountPolicy, liveAttemptAccountOK := cfg.Limits.Policies[limits.ScopeLiveAttemptAccount]
	liveAttemptFamilyPolicy, liveAttemptFamilyOK := cfg.Limits.Policies[limits.ScopeLiveAttemptFamily]
	exportIPPolicy, exportIPOK := cfg.Limits.Policies[limits.ScopeExportIP]
	exportAccountPolicy, exportAccountOK := cfg.Limits.Policies[limits.ScopeExportAccount]
	exportDevicePolicy, exportDeviceOK := cfg.Limits.Policies[limits.ScopeExportDevice]
	if !familyStructuralAccountOK || !familyStructuralDeviceOK || !invitationCreateAccountOK || !invitationCreateIPOK ||
		!invitationRedeemAccountOK || !invitationRedeemIPOK {
		return httpapi.Options{}, errors.New("family rate-limit policies are required")
	}
	if !syncMutationDeviceOK || !syncMutationAccountOK || !syncMutationFamilyOK || !syncPullDeviceOK || !syncPullAccountOK {
		return httpapi.Options{}, errors.New("sync rate-limit policies are required")
	}
	if !liveAttemptIPOK || !liveAttemptDeviceOK || !liveAttemptAccountOK || !liveAttemptFamilyOK {
		return httpapi.Options{}, errors.New("live rate-limit policies are required")
	}
	if !exportIPOK || !exportAccountOK || !exportDeviceOK {
		return httpapi.Options{}, errors.New("export rate-limit policies are required")
	}
	capabilities := protocol.ServerCapabilities()
	if !cfg.Export.Enabled {
		capabilities = slices.DeleteFunc(capabilities, func(value string) bool { return value == protocol.CapabilityExportV1 })
	}
	serverInfo := protocol.ServerInfo{InstanceID: cfg.ServerInstanceID, RecoveryEpoch: cfg.RecoveryEpoch, CanonicalOrigin: cfg.PublicOrigin.String(),
		APIVersions: []string{"v1"}, Capabilities: capabilities, AppleAuthorizationMode: authorizationMode, Limits: &cfg.Sync.Limits}
	liveAuthorizer, err := realtime.NewAuthorityAuthorizer(authority, deliveryQueueHosted(cfg.Mode), time.Now)
	if err != nil {
		return httpapi.Options{}, err
	}
	liveHub, err := realtime.NewHub(context.Background(), realtime.HubOptions{Limits: cfg.Sync.Limits, Authorizer: liveAuthorizer, Invalidations: authority})
	if err != nil {
		return httpapi.Options{}, err
	}
	observer := delivery.NewObserver(delivery.NewRepository(deliveryQueueHosted(cfg.Mode)), liveHub)
	syncService := familysync.NewServiceWithPaging(authority, records.NewRepository(), serverInfo, cfg.Sync.Limits, time.Now,
		syncFamilyResolver{service: familyService}, observer, composeDigestKeyring(cfg.Auth.PageTokenKeys))
	importService := importer.NewService(authority, records.NewRepository(), syncFamilyResolver{service: familyService}, serverInfo, cfg.Sync.Limits, composeDigestKeyring(cfg.Auth.PageTokenKeys), time.Now)
	importService.SetCommitObserver(observer)
	syncService.SetImportAdmission(importService)
	readiness := applicationReadiness{
		base: gate, inventory: authorityCredentialKeyInventory{authority: authority}, mode: cfg.Mode,
		sessionKeyIDs:  configuredKeyIDs(cfg.Auth.SessionTokenKeys),
		identityKeyIDs: configuredKeyIDs(cfg.Auth.IdentityKeys), deviceKeyIDs: configuredKeyIDs(cfg.Auth.DeviceContinuityKeys),
		invitationKeyIDs: configuredKeyIDs(cfg.Auth.InvitationKeys), hostedAdapterAvailable: hostedAdapterAvailable,
		hostedCredentialRotationVerified: hostedAdapterAvailable,
	}
	// Explicit process-local opt-in. The default is nil, so production records
	// nothing; enabled output contains only the fixed phase and bounded time.
	var syncRecorder diagnostics.Recorder
	if os.Getenv("BT_SYNC_DIAGNOSTICS") == "1" {
		syncRecorder = func(_ context.Context, phase diagnostics.Phase, elapsed time.Duration) {
			logger.Info("sync diagnostic", slog.String("phase", string(phase)), slog.Int64("elapsed_ns", elapsed.Nanoseconds()))
			// Cumulative process/runtime-pool counters; consumers take non-overlapping
			// time-window deltas, never sum overlapping per-request samples.
			var memory runtime.MemStats
			runtime.ReadMemStats(&memory)
			var usage syscall.Rusage
			if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) == nil {
				pool := runtimePool.Stat()
				rss := int64(usage.Maxrss) * 1024 // Linux server container reports KiB.
				if runtime.GOOS == "darwin" {
					rss = int64(usage.Maxrss)
				}
				cpu := float64(usage.Utime.Sec+usage.Stime.Sec) + float64(usage.Utime.Usec+usage.Stime.Usec)/1e6
				logger.Info("sync resource diagnostic", slog.Uint64("alloc_total_bytes", memory.TotalAlloc), slog.Float64("cpu_total_seconds", cpu), slog.Int64("rss_peak_bytes", rss), slog.Int64("runtime_pool_acquire_ns", pool.AcquireDuration().Nanoseconds()), slog.Int64("runtime_pool_acquire_count", pool.AcquireCount()), slog.Int64("runtime_pool_wait_ns", pool.EmptyAcquireWaitTime().Nanoseconds()), slog.Int("runtime_pool_total_connections", int(pool.TotalConns())))
			}

		}
	}
	return httpapi.Options{
		Logger:                        logger,
		SyncDiagnosticRecorder:        syncRecorder,
		Readiness:                     readiness,
		InstanceID:                    cfg.ServerInstanceID,
		RecoveryEpoch:                 cfg.RecoveryEpoch,
		CanonicalOrigin:               cfg.PublicOrigin.String(),
		SyncLimits:                    &cfg.Sync.Limits,
		Gate:                          gate,
		ServerState:                   gate,
		AppleAuthorizationMode:        authorizationMode,
		SourceVersion:                 sourceVersion,
		SourceURL:                     sourceURL,
		Capabilities:                  capabilities,
		ChallengeService:              identityService,
		SessionService:                sessionService,
		RecoveryService:               recoverySessions,
		RecoveryEnabled:               cfg.RecoveryCleanup.Enabled,
		AppleNotifications:            notificationService,
		AccountService:                &account.Service{Security: authority},
		FamilyService:                 familyService,
		SyncService:                   syncService,
		ImportService:                 importService,
		LiveActivityUnregistration:    liveactivity.NewUnregistration(authority, syncFamilyResolver{service: familyService}),
		LiveFamilyAuthorizer:          familyService,
		LiveHub:                       liveHub,
		Limiter:                       limiter,
		ClientIPResolver:              resolver,
		ChallengePolicy:               challengePolicy,
		ExchangePolicy:                exchangePolicy,
		RefreshIPPolicy:               refreshIPPolicy,
		RefreshAccountPolicy:          refreshAccountPolicy,
		RefreshDevicePolicy:           refreshDevicePolicy,
		FamilyStructuralAccountPolicy: familyStructuralAccountPolicy,
		FamilyStructuralDevicePolicy:  familyStructuralDevicePolicy,
		InvitationCreateAccountPolicy: invitationCreateAccountPolicy,
		InvitationCreateIPPolicy:      invitationCreateIPPolicy,
		InvitationRedeemAccountPolicy: invitationRedeemAccountPolicy,
		InvitationRedeemIPPolicy:      invitationRedeemIPPolicy,
		SyncMutationDevicePolicy:      syncMutationDevicePolicy,
		SyncMutationAccountPolicy:     syncMutationAccountPolicy,
		SyncMutationFamilyPolicy:      syncMutationFamilyPolicy,
		SyncPullDevicePolicy:          syncPullDevicePolicy,
		SyncPullAccountPolicy:         syncPullAccountPolicy,
		LiveAttemptIPPolicy:           liveAttemptIPPolicy,
		LiveAttemptDevicePolicy:       liveAttemptDevicePolicy,
		LiveAttemptAccountPolicy:      liveAttemptAccountPolicy,
		LiveAttemptFamilyPolicy:       liveAttemptFamilyPolicy,
		ExportIPPolicy:                exportIPPolicy,
		ExportAccountPolicy:           exportAccountPolicy,
		ExportDevicePolicy:            exportDevicePolicy,
	}, nil
}

// Development sessions use the same lifetime policy as self-hosted sessions.
func authenticationMode(mode config.Mode) config.Mode {
	if mode == config.ModeDevelopment {
		return config.ModeSelfHosted
	}
	return mode
}

func sessionServiceOptions(cfg config.Config, identityService *identity.Service, verifier identity.IdentityVerifier,
	authority *security.Service, repository *auth.Repository) auth.Options {
	origin := ""
	if cfg.PublicOrigin != nil {
		origin = cfg.PublicOrigin.String()
	}
	mode := authenticationMode(cfg.Mode)
	return auth.Options{
		Identity: identityService, Verifier: verifier, Security: authority, Repository: repository, Mode: mode,
		Origin: origin, InstanceID: cfg.ServerInstanceID, TokenKeys: composeDigestKeyring(cfg.Auth.SessionTokenKeys),
		DeviceKeys: composeDigestKeyring(cfg.Auth.DeviceContinuityKeys), NotificationKeys: composeDigestKeyring(cfg.Auth.AppleNotificationKeys),
		ActorIDKey: cfg.Auth.ActorIDPepper, Now: time.Now,
	}
}

func composeDigestKeyring(source config.DigestKeyringConfig) identity.DigestKeyring {
	result := identity.DigestKeyring{Current: identity.DigestKey{ID: source.Current.ID, Secret: source.Current.Secret}}
	result.Previous = make([]identity.DigestKey, len(source.Previous))
	for index, key := range source.Previous {
		result.Previous[index] = identity.DigestKey{ID: key.ID, Secret: key.Secret}
	}
	return result
}

func configuredKeyIDs(ring config.DigestKeyringConfig) map[string]struct{} {
	result := make(map[string]struct{}, len(ring.Previous)+1)
	result[ring.Current.ID] = struct{}{}
	for _, key := range ring.Previous {
		result[key.ID] = struct{}{}
	}
	return result
}

func familyServiceOptions(cfg config.Config, authority *security.Service, repository *family.Repository) family.Options {
	var entitlements family.EntitlementPolicy = family.SelfHostedEntitlementPolicy{}
	if deliveryQueueHosted(cfg.Mode) {
		entitlements = family.HostedEntitlementPolicy{Security: authority}
	}
	origin := ""
	if cfg.PublicOrigin != nil {
		origin = cfg.PublicOrigin.String()
	}
	return family.Options{
		Security: authority, Repository: repository, Entitlements: entitlements,
		InvitationKeys: composeDigestKeyring(cfg.Auth.InvitationKeys), Origin: origin, Now: time.Now,
		MaxCaregivers: cfg.Limits.MaxCaregivers, MaxInvitations: cfg.Limits.MaxInvitations, InvitationTTL: cfg.Limits.InvitationTTL,
	}
}

func deliveryQueueHosted(mode config.Mode) bool { return mode == config.ModeHosted }

type securityFamilySettings struct {
	maxCaregivers, maxInvitations int
	requireEntitlement            bool
}

func composeSecurityFamilySettings(cfg config.Config) securityFamilySettings {
	return securityFamilySettings{
		maxCaregivers: cfg.Limits.MaxCaregivers, maxInvitations: cfg.Limits.MaxInvitations,
		requireEntitlement: deliveryQueueHosted(cfg.Mode),
	}
}

func runtimeGate(cfg config.Config, runtimePool, projectorPool *pgxpool.Pool) (*recovery.Gate, error) {
	gate, _, _, err := runtimeServices(cfg, runtimePool, projectorPool)
	return gate, err
}

func runtimeServices(cfg config.Config, runtimePool, projectorPool *pgxpool.Pool) (*recovery.Gate, *security.Service, *family.Service, error) {
	if runtimePool == nil || projectorPool == nil || runtimePool == projectorPool {
		return nil, nil, nil, errors.New("distinct runtime and projector pools are required")
	}
	// No concrete provider factory is registered yet. Hosted mode must fail
	// before local paths are opened; readiness-only rejection would still leave
	// a filesystem authority running behind an unhealthy endpoint.
	if cfg.Mode == config.ModeHosted && !identityTestHostedAdapterEnabled(cfg) {
		return nil, nil, nil, errHostedAdapterUnavailable
	}
	stores, err := localstore.Open(cfg.SecurityLedgerDir, cfg.LedgerHeadDir)
	if err != nil {
		return nil, nil, nil, err
	}
	durable, err := recovery.NewVerifiedStore(cfg.LedgerHeadDir, stores.Fence, stores.Ledger, stores.Heads)
	if err != nil {
		return nil, nil, nil, err
	}
	recoveryRepository, err := auth.NewRepository(runtimePool)
	if err != nil {
		return nil, nil, nil, err
	}
	recoveryReferences, err := auth.NewRecoveryKeyReferences(recoveryRepository, authenticationMode(cfg.Mode), time.Now)
	if err != nil {
		return nil, nil, nil, err
	}
	var familyAuthorizerTarget familyAuthorizer
	familySettings := composeSecurityFamilySettings(cfg)
	authority, err := security.New(security.Options{RecoveryKeyReferences: func(ctx context.Context, view security.AuthorityView) (security.RecoveryKeyReferencesResult, error) {
		result, err := recoveryReferences(ctx, view)
		return security.RecoveryKeyReferencesResult{Inventory: security.CredentialKeyInventory{SessionToken: result.Inventory.SessionToken, DeviceContinuity: result.Inventory.DeviceContinuity}, RevalidateAt: result.RevalidateAt, Cacheable: result.Cacheable}, err
	}, Ledger: stores.Ledger, Heads: stores.Heads, Projection: security.NewProjector(projectorPool), Fence: stores.Fence, Now: time.Now,
		Runtime: runtimePool, Decide: familyAuthorizationDecision(func() familyAuthorizer { return familyAuthorizerTarget }),
		Binding: func(ctx context.Context) (security.AuthorityBinding, error) {
			state, err := durable.LoadUnderFence(ctx)
			return security.AuthorityBinding{InstanceID: state.InstanceID, Epoch: state.Epoch, TrustedHead: state.TrustedHead}, err
		},
		Epoch: func(ctx context.Context) (uint64, error) {
			state, err := durable.LoadUnderFence(ctx)
			return state.Epoch, err
		}, MaxCaregivers: familySettings.maxCaregivers, MaxInvitations: familySettings.maxInvitations,
		RequireFamilySyncEntitlement: familySettings.requireEntitlement})
	if err != nil {
		return nil, nil, nil, err
	}
	familyRepository, err := family.NewRepository(runtimePool)
	if err != nil {
		return nil, nil, nil, err
	}
	familyService, err := family.New(familyServiceOptions(cfg, authority, familyRepository))
	if err != nil {
		return nil, nil, nil, err
	}
	familyAuthorizerTarget = familyService
	compatibility, err := migrate.EmbeddedCompatibility()
	if err != nil {
		return nil, nil, nil, err
	}
	mirror, err := recovery.NewPostgresMirror(runtimePool, projectorPool)
	if err != nil {
		return nil, nil, nil, err
	}
	gate, err := recovery.NewGate(recovery.Options{Store: durable, Security: authority,
		Schema: databaseReadiness{pool: runtimePool, projector: projectorPool, runner: migrate.New(migrations.Files), compatibility: compatibility}, Mirror: mirror,
		ExpectedInstanceID: cfg.ServerInstanceID, ExpectedEpoch: cfg.RecoveryEpoch, ActorIDKey: cfg.Auth.ActorIDPepper})
	if err != nil {
		return nil, nil, nil, err
	}
	return gate, authority, familyService, nil
}

func reconcileLoop(ctx context.Context, gate *recovery.Gate) {
	delay := 100 * time.Millisecond
	for {
		attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := gate.Reconcile(attempt)
		cancel()
		if err == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
			if delay < 5*time.Second {
				delay *= 2
			}
		}
	}
}

type databaseReadiness struct {
	pool          *pgxpool.Pool
	projector     *pgxpool.Pool
	runner        *migrate.Runner
	compatibility migrate.Compatibility
}

func (r databaseReadiness) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := r.pool.Ping(ctx); err != nil {
		return database.ErrUnavailable
	}
	version, err := r.runner.CompatibleVersion(ctx, r.pool, r.compatibility)
	if err != nil {
		return database.ErrUnavailable
	}
	if err := database.VerifyRoleSeparation(ctx, r.pool, r.projector, version); err != nil {
		return database.ErrUnavailable
	}
	confirmedVersion, err := r.runner.CompatibleVersion(ctx, r.pool, r.compatibility)
	if err != nil || confirmedVersion != version {
		return database.ErrUnavailable
	}
	return nil
}

func probeCommand(arguments []string) {
	flags := flag.NewFlagSet("probe", flag.ExitOnError)
	flags.SetOutput(os.Stderr)
	healthURL := flags.String("url", "", "health endpoint URL")
	flags.Parse(arguments)
	if *healthURL == "" {
		fmt.Fprintln(os.Stderr, "probe requires --url <health-url>")
		os.Exit(2)
	}
	if err := probe.Check(context.Background(), *healthURL); err != nil {
		fmt.Fprintln(os.Stderr, "probe failed")
		os.Exit(1)
	}
}
