package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/diagnostics"
	"babytracker/server/internal/family"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
	"babytracker/server/internal/security"
)

const maxRequestBodyBytes int64 = 1 << 20

type Readiness interface {
	Check(context.Context) error
}

type AccountGate interface {
	Check(context.Context) error
}

type ServerState interface {
	ServerState(context.Context) (string, uint64, error)
}

type ChallengeService interface {
	Challenge(context.Context) (identity.Challenge, error)
}

type SessionService interface {
	Exchange(context.Context, identity.ExchangeRequest) (auth.SessionBundle, error)
	Authenticate(context.Context, string) (access.Principal, error)
	ResolveRefreshSubject(context.Context, string) (auth.RefreshSubject, error)
	Refresh(context.Context, string) (auth.SessionBundle, error)
	Logout(context.Context, access.Principal, string) (security.CommandResult, error)
}

type RecoverySessionService interface {
	ResolveRecoverableRefreshSubject(context.Context, auth.RecoverableRefreshInput) (auth.RefreshSubject, error)
	RefreshRecoverable(context.Context, auth.RecoverableRefreshInput) (auth.RecoveredSessionBundle, error)
	AcknowledgeRefresh(context.Context, access.Principal, string, identity.DeviceContinuityClaim) error
}

type AppleNotificationService interface {
	Handle(context.Context, string) error
}

type AccountLifecycleService interface {
	Delete(context.Context, access.Principal, string, bool) (security.CommandResult, error)
}

type SyncService interface {
	ApplyAndPull(context.Context, access.Principal, protocol.SyncRequest) (protocol.SyncResponse, error)
	FullSnapshot(context.Context, access.Principal, protocol.SnapshotRequest) (protocol.SnapshotResponse, error)
}

// LiveActivityService registers devices and cards. It is composed only while
// push is enabled, and its routes need the supervised worker to be running.
type LiveActivityService interface {
	RegisterDevice(context.Context, access.Principal, protocol.LiveActivityDeviceRegistration) (protocol.LiveActivityRegistrationReceipt, error)
	RegisterActivity(context.Context, access.Principal, protocol.LiveActivityTokenRegistration) (protocol.LiveActivityRegistrationReceipt, error)
}

// LiveActivityUnregistration revokes an installation's registration. It is the
// repository-only path — no worker, APNs key or push configuration — so a phone
// can withdraw while push is disabled or the worker is stopped.
type LiveActivityUnregistration interface {
	UnregisterDevice(context.Context, access.Principal, string, uint64) error
}

type ExportService interface {
	Create(context.Context, access.Principal, protocol.CreateExportRequest) (protocol.ExportStatus, bool, error)
	Status(context.Context, access.Principal, string) (protocol.ExportStatus, error)
	Download(context.Context, access.Principal, string, io.Writer) error
}

type ImportService interface {
	Resolve(context.Context, access.Principal, protocol.ImportResolveRequest) (protocol.ImportResolveResponse, error)
	StatusPage(context.Context, access.Principal, string, *string) (protocol.ImportResolveResponse, error)
	ResolveConflict(context.Context, access.Principal, string, protocol.ImportConflictRequest) (protocol.ImportResolveResponse, error)
}

type LiveFamilyAuthorizer interface {
	Authorize(context.Context, access.Principal, string, string) (access.Grant, error)
}

type FamilyService interface {
	Account(context.Context, access.Principal) (family.AccountView, error)
	UpdateProfile(context.Context, access.Principal, string, string, bool) (family.AccountView, error)
	Create(context.Context, access.Principal, string, string) (family.FamilyView, error)
	Current(context.Context, access.Principal) (family.FamilyView, error)
	Invite(context.Context, access.Principal, string, string) (family.InvitationSecret, error)
	Invitation(context.Context, access.Principal, string) (family.InvitationView, error)
	PendingInvitations(context.Context, access.Principal) ([]family.InvitationView, error)
	Preview(context.Context, access.Principal, string) (family.InvitationPreview, error)
	Redeem(context.Context, access.Principal, string, string, bool) (family.FamilyView, error)
	ChangeRole(context.Context, access.Principal, string, string, string) (security.CommandResult, error)
	RevokeInvitation(context.Context, access.Principal, string, string) (security.CommandResult, error)
	Transfer(context.Context, access.Principal, string, string) (security.CommandResult, error)
	Remove(context.Context, access.Principal, string, string) (security.CommandResult, error)
	Leave(context.Context, access.Principal, string) (security.CommandResult, error)
	RepairProfile(context.Context, access.Principal, string, string, bool) (family.FamilyView, error)
	ListActors(context.Context, access.Principal, string, string, int) (family.ActorLabelPage, error)
	ScheduleDeletion(context.Context, access.Principal, string, bool) (family.RecoveryStatus, error)
	RecoveryStatus(context.Context, access.Principal) (family.RecoveryStatus, error)
	Recover(context.Context, access.Principal, string, bool) (security.CommandResult, error)
	Forfeit(context.Context, access.Principal, string, bool) (security.CommandResult, error)
}

type Options struct {
	Logger                        *slog.Logger
	SyncDiagnosticRecorder        diagnostics.Recorder
	Readiness                     Readiness
	InstanceID                    string
	RecoveryEpoch                 uint64
	CanonicalOrigin               string
	SyncLimits                    *protocol.Limits
	Gate                          AccountGate
	ServerState                   ServerState
	AppleAuthorizationMode        string
	SourceVersion                 string
	SourceURL                     string
	Capabilities                  []string
	ChallengeService              ChallengeService
	SessionService                SessionService
	RecoveryService               RecoverySessionService
	RecoveryEnabled               bool
	RecoveryReadiness             Readiness
	AppleNotifications            AppleNotificationService
	AccountService                AccountLifecycleService
	FamilyService                 FamilyService
	SyncService                   SyncService
	ExportService                 ExportService
	LiveActivityService           LiveActivityService
	LiveActivityReadiness         Readiness // worker-local state only: cheap, no database work; callers add Readiness
	LiveActivityUnregistration    LiveActivityUnregistration
	ImportService                 ImportService
	LiveFamilyAuthorizer          LiveFamilyAuthorizer
	LiveHub                       *realtime.Hub
	Limiter                       limits.Limiter
	ClientIPResolver              *limits.ClientIPResolver
	ChallengePolicy               limits.Policy
	ExchangePolicy                limits.Policy
	RefreshIPPolicy               limits.Policy
	RefreshAccountPolicy          limits.Policy
	RefreshDevicePolicy           limits.Policy
	FamilyStructuralAccountPolicy limits.Policy
	FamilyStructuralDevicePolicy  limits.Policy
	InvitationCreateAccountPolicy limits.Policy
	InvitationCreateIPPolicy      limits.Policy
	InvitationRedeemAccountPolicy limits.Policy
	InvitationRedeemIPPolicy      limits.Policy
	SyncMutationDevicePolicy      limits.Policy
	SyncMutationAccountPolicy     limits.Policy
	SyncMutationFamilyPolicy      limits.Policy
	SyncPullDevicePolicy          limits.Policy
	SyncPullAccountPolicy         limits.Policy
	LiveAttemptIPPolicy           limits.Policy
	LiveAttemptDevicePolicy       limits.Policy
	LiveAttemptAccountPolicy      limits.Policy
	LiveAttemptFamilyPolicy       limits.Policy
	ExportIPPolicy                limits.Policy
	ExportAccountPolicy           limits.Policy
	ExportDevicePolicy            limits.Policy
}

func advertisedCapabilities(options Options) []string {
	capabilities := options.Capabilities
	if capabilities == nil {
		capabilities = protocol.ServerCapabilities()
	}
	result := make([]string, 0, len(capabilities)+1)
	for _, capability := range capabilities {
		if capability == protocol.CapabilityRefreshRecoveryV1 || (capability == protocol.CapabilityLiveActivityPushV1 && (options.LiveActivityService == nil || options.LiveActivityReadiness == nil)) {
			continue
		}
		if capability != protocol.CapabilityExportV1 || options.ExportService != nil {
			result = append(result, capability)
		}
	}
	if options.RecoveryEnabled && options.RecoveryService != nil && options.RecoveryReadiness != nil {
		result = append(result, protocol.CapabilityRefreshRecoveryV1)
	}
	return result
}

type Route struct {
	Method  string
	Pattern string
}

var routes = []Route{
	{Method: http.MethodPut, Pattern: "/v1/live-activities/device"},
	{Method: http.MethodPut, Pattern: "/v1/live-activities/activity"},
	{Method: http.MethodDelete, Pattern: "/v1/live-activities/device/{installation_id}"},
	{Method: http.MethodPost, Pattern: "/v1/family/import/resolve"},
	{Method: http.MethodGet, Pattern: "/v1/family/import/{import_id}"},
	{Method: http.MethodPost, Pattern: "/v1/family/import/{import_id}/resolve-conflict"},
	{Method: http.MethodGet, Pattern: "/healthz"},
	{Method: http.MethodGet, Pattern: "/readyz"},
	{Method: http.MethodGet, Pattern: "/v1/server"},
	{Method: http.MethodPost, Pattern: "/v1/auth/challenge"},
	{Method: http.MethodPost, Pattern: "/v1/auth/apple"},
	{Method: http.MethodPost, Pattern: "/v1/auth/refresh"},
	{Method: http.MethodPost, Pattern: "/v1/auth/refresh-recoverable"},
	{Method: http.MethodPost, Pattern: "/v1/auth/refresh-ack"},
	{Method: http.MethodPost, Pattern: "/v1/auth/logout"},
	{Method: http.MethodPost, Pattern: "/v1/auth/apple/notifications"},
	{Method: http.MethodGet, Pattern: "/v1/account"},
	{Method: http.MethodPatch, Pattern: "/v1/account"},
	{Method: http.MethodPost, Pattern: "/v1/account/delete"},
	{Method: http.MethodPost, Pattern: "/v1/family"},
	{Method: http.MethodGet, Pattern: "/v1/family"},
	{Method: http.MethodPatch, Pattern: "/v1/family"},
	{Method: http.MethodGet, Pattern: "/v1/family/actors"},
	{Method: http.MethodPost, Pattern: "/v1/family/invitations"},
	{Method: http.MethodGet, Pattern: "/v1/family/invitations"},
	{Method: http.MethodGet, Pattern: "/v1/family/invitations/{id}"},
	{Method: http.MethodPost, Pattern: "/v1/family/invitations/{id}/revoke"},
	{Method: http.MethodPost, Pattern: "/v1/family/invitations/preview"},
	{Method: http.MethodPost, Pattern: "/v1/family/invitations/redeem"},
	{Method: http.MethodPost, Pattern: "/v1/family/members/{account_id}/remove"},
	{Method: http.MethodPost, Pattern: "/v1/family/members/{account_id}/role"},
	{Method: http.MethodPost, Pattern: "/v1/family/ownership-transfer"},
	{Method: http.MethodPost, Pattern: "/v1/family/leave"},
	{Method: http.MethodPost, Pattern: "/v1/family/delete"},
	{Method: http.MethodGet, Pattern: "/v1/family/recovery"},
	{Method: http.MethodPost, Pattern: "/v1/family/recover"},
	{Method: http.MethodPost, Pattern: "/v1/family/recovery/forfeit"},
	{Method: http.MethodGet, Pattern: "/invite"},
	{Method: http.MethodPost, Pattern: "/v1/sync"},
	{Method: http.MethodGet, Pattern: "/v1/sync/snapshot"},
	{Method: http.MethodPost, Pattern: "/v1/export"},
	{Method: http.MethodGet, Pattern: "/v1/export/{id}"},
	{Method: http.MethodGet, Pattern: "/v1/export/{id}/download"},
	{Method: http.MethodGet, Pattern: "/v1/live"},
}

func Routes() []Route {
	return append([]Route(nil), routes...)
}

func RoutesForAuthorizationMode(mode string) []Route {
	selected := make([]Route, 0, len(routes))
	for _, route := range routes {
		if route.Pattern == "/v1/auth/apple/notifications" && mode != "code_exchange_required" {
			continue
		}
		selected = append(selected, route)
	}
	return selected
}

func New(options Options) http.Handler {
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(discardWriter{}, nil))
	}

	mux := http.NewServeMux()
	handlers := map[Route]http.HandlerFunc{
		{Method: http.MethodPost, Pattern: "/v1/family/import/resolve"}:                      importHandler(options, "resolve"),
		{Method: http.MethodGet, Pattern: "/v1/family/import/{import_id}"}:                   importHandler(options, "status"),
		{Method: http.MethodPost, Pattern: "/v1/family/import/{import_id}/resolve-conflict"}: importHandler(options, "conflict"),
		{Method: http.MethodGet, Pattern: "/healthz"}:                                        healthHandler,
		{Method: http.MethodGet, Pattern: "/readyz"}:                                         readyHandler(options.Readiness),
		{Method: http.MethodGet, Pattern: "/v1/server"}:                                      serverHandler(options),
		{Method: http.MethodPost, Pattern: "/v1/auth/challenge"}:                             challengeHandler(options),
		{Method: http.MethodPost, Pattern: "/v1/auth/apple"}:                                 appleExchangeHandler(options),
		{Method: http.MethodPost, Pattern: "/v1/auth/refresh"}:                               refreshHandler(options),
		{Method: http.MethodPost, Pattern: "/v1/auth/refresh-recoverable"}:                   recoverableRefreshHandler(options),
		{Method: http.MethodPost, Pattern: "/v1/auth/refresh-ack"}:                           refreshAckHandler(options),
		{Method: http.MethodPost, Pattern: "/v1/auth/logout"}:                                logoutHandler(options),
		{Method: http.MethodGet, Pattern: "/v1/account"}:                                     accountHandler(options),
		{Method: http.MethodPatch, Pattern: "/v1/account"}:                                   updateAccountHandler(options),
		{Method: http.MethodPost, Pattern: "/v1/account/delete"}:                             accountDeleteHandler(options),
		{Method: http.MethodPost, Pattern: "/v1/family"}:                                     createFamilyHandler(options),
		{Method: http.MethodGet, Pattern: "/v1/family"}:                                      currentFamilyHandler(options),
		{Method: http.MethodPatch, Pattern: "/v1/family"}:                                    repairFamilyProfileHandler(options),
		{Method: http.MethodGet, Pattern: "/v1/family/actors"}:                               familyActorsHandler(options),
		{Method: http.MethodPost, Pattern: "/v1/family/invitations"}:                         createInvitationHandler(options),
		{Method: http.MethodGet, Pattern: "/v1/family/invitations"}:                          pendingInvitationsHandler(options),
		{Method: http.MethodGet, Pattern: "/v1/family/invitations/{id}"}:                     invitationHandler(options),
		{Method: http.MethodPost, Pattern: "/v1/family/invitations/{id}/revoke"}:             revokeInvitationHandler(options),
		{Method: http.MethodPost, Pattern: "/v1/family/invitations/preview"}:                 previewInvitationHandler(options),
		{Method: http.MethodPost, Pattern: "/v1/family/invitations/redeem"}:                  redeemInvitationHandler(options),
		{Method: http.MethodPost, Pattern: "/v1/family/members/{account_id}/remove"}:         removeMemberHandler(options),
		{Method: http.MethodPost, Pattern: "/v1/family/members/{account_id}/role"}:           memberRoleHandler(options),
		{Method: http.MethodPost, Pattern: "/v1/family/ownership-transfer"}:                  transferOwnershipHandler(options),
		{Method: http.MethodPost, Pattern: "/v1/family/leave"}:                               leaveFamilyHandler(options),
		{Method: http.MethodPost, Pattern: "/v1/family/delete"}:                              familyDeleteHandler(options),
		{Method: http.MethodGet, Pattern: "/v1/family/recovery"}:                             familyRecoveryStatusHandler(options),
		{Method: http.MethodPost, Pattern: "/v1/family/recover"}:                             familyRecoverHandler(options),
		{Method: http.MethodPost, Pattern: "/v1/family/recovery/forfeit"}:                    familyForfeitHandler(options),
		{Method: http.MethodGet, Pattern: "/invite"}:                                         invitationHandoffHandler,
		{Method: http.MethodPost, Pattern: "/v1/sync"}:                                       syncHandler(options),
		{Method: http.MethodGet, Pattern: "/v1/sync/snapshot"}:                               snapshotHandler(options),
		{Method: http.MethodPut, Pattern: "/v1/live-activities/device"}:                      liveActivityDeviceHandler(options),
		{Method: http.MethodPut, Pattern: "/v1/live-activities/activity"}:                    liveActivityTokenHandler(options),
		{Method: http.MethodDelete, Pattern: "/v1/live-activities/device/{installation_id}"}: liveActivityDeleteHandler(options),
		{Method: http.MethodPost, Pattern: "/v1/export"}:                                     exportCreateHandler(options),
		{Method: http.MethodGet, Pattern: "/v1/export/{id}"}:                                 exportStatusHandler(options),
		{Method: http.MethodGet, Pattern: "/v1/export/{id}/download"}:                        exportDownloadHandler(options),
		{Method: http.MethodGet, Pattern: "/v1/live"}:                                        liveHandler(options),
	}
	for _, route := range RoutesForAuthorizationMode(options.AppleAuthorizationMode) {
		if route.Pattern == "/v1/auth/apple/notifications" {
			handlers[route] = appleNotificationHandler(options)
		}
		mux.HandleFunc(route.Method+" "+route.Pattern, handlers[route])
	}

	return withMiddleware(logger, mux)
}

func Run(ctx context.Context, server *http.Server) error {
	if server == nil {
		return errors.New("http server is required")
	}

	serveResult := make(chan error, 1)
	go func() {
		serveResult <- server.ListenAndServe()
	}()

	select {
	case err := <-serveResult:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			return err
		}
		err := <-serveResult
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
