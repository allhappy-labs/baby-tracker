package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/account"
	"babytracker/server/internal/admission"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/diagnostics"
	"babytracker/server/internal/family"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
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
	Logout(context.Context, access.Principal, string) (authority.Result, error)
	// AccessDigests is a bearer access token's lookup digests, with no
	// database work (the one-transaction sync and snapshot routes).
	AccessDigests(string) ([]authority.KeyedDigest, error)
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
	Delete(context.Context, access.Principal, string, bool) (account.DeletionResult, error)
}

// SyncService serves each sync and snapshot request in one authenticated
// transaction (the request's credential and checks are in the
// authority.Request).
type SyncService interface {
	ServeSync(context.Context, authority.Request, protocol.SyncRequest) (protocol.SyncResponse, error)
	ServeSnapshot(context.Context, authority.Request, protocol.SnapshotRequest) (protocol.SnapshotResponse, error)
}

// PushTokenService stores the authenticated session's own device's APNs
// token (pushtoken.Service), each call in one guarded request transaction
// with the request's checks in the authority.Request.
type PushTokenService interface {
	Register(context.Context, authority.Request, string, []byte) error
	Unregister(context.Context, authority.Request) error
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

// RequestAuthorizer authorizes one request in one pooled statement with no
// transaction (authority.Service.AuthorizeRequest): the credential, the
// route's own refusals (Admit), family policy and the in-memory charge
// (Charge), in that order. The live handshake is only that; the import and
// export routes then run their service's one guarded transaction.
type RequestAuthorizer interface {
	AuthorizeRequest(context.Context, authority.Request, string) (access.Principal, access.Grant, error)
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
	ChangeRole(context.Context, access.Principal, string, string, string) (authority.Result, error)
	RevokeInvitation(context.Context, access.Principal, string, string) (authority.Result, error)
	Transfer(context.Context, access.Principal, string, string) (authority.Result, error)
	Remove(context.Context, access.Principal, string, string) (authority.Result, error)
	Leave(context.Context, access.Principal, string) (authority.Result, error)
	RepairProfile(context.Context, access.Principal, string, string, bool) (family.FamilyView, error)
	ListActors(context.Context, access.Principal, string, string, int) (family.ActorLabelPage, error)
	ScheduleDeletion(context.Context, access.Principal, string, bool) (family.RecoveryStatus, error)
	RecoveryStatus(context.Context, access.Principal) (family.RecoveryStatus, error)
	Recover(context.Context, access.Principal, string, bool) (authority.Result, error)
	Forfeit(context.Context, access.Principal, string, bool) (authority.Result, error)
}

type Options struct {
	Admission *admission.Controller
	// BodyReads bounds the request bodies read before a credential is
	// checked, per client address and in all (nil: the defaults); see
	// admitAfterBody.
	BodyReads *admission.PerKey
	// BodyReadTimeout bounds one such body read (0: 5 s).
	BodyReadTimeout        time.Duration
	AdmissionMetrics       *diagnostics.CapacityCollector
	Logger                 *slog.Logger
	SyncDiagnosticRecorder diagnostics.Recorder
	Readiness              Readiness
	InstanceID             string
	RecoveryEpoch          uint64
	CanonicalOrigin        string
	SyncLimits             *protocol.Limits
	Gate                   AccountGate
	ServerState            ServerState
	AppleAuthorizationMode string
	SourceVersion          string
	SourceURL              string
	Capabilities           []string
	ChallengeService       ChallengeService
	SessionService         SessionService
	RecoveryService        RecoverySessionService
	RecoveryEnabled        bool
	RecoveryReadiness      Readiness
	AppleNotifications     AppleNotificationService
	AccountService         AccountLifecycleService
	FamilyService          FamilyService
	SyncService            SyncService
	ExportService          ExportService
	ImportService          ImportService
	// PushTokens, when non-nil, serves PUT and DELETE
	// /v1/devices/push-token and advertises push-token-v1 (a server with
	// APNs wakes configured). Nil: the routes are not registered (404).
	PushTokens                    PushTokenService
	RequestAuthorizer             RequestAuthorizer
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
	PushTokenDevicePolicy         limits.Policy
	PushTokenAccountPolicy        limits.Policy
	// LiveActivityService, when non-nil, serves PUT /v1/live-activities/device
	// and /v1/live-activities/activity (a server with Live Activity push
	// enabled). Nil: those routes are not registered (404).
	LiveActivityService LiveActivityService
	// LiveActivityUnregistration, when non-nil, serves DELETE
	// /v1/live-activities/device/{installation_id}, whether or not push is
	// enabled. Nil: the route is not registered (404).
	LiveActivityUnregistration LiveActivityUnregistration
	// LiveActivityReady is the Live Activity worker's readiness (nil: not
	// ready). Registration and the advertised live-activity-push-v1 need it
	// to answer nil.
	LiveActivityReady func(context.Context) error
	// LiveActivityAccountPolicy and LiveActivityDevicePolicy are the Live
	// Activity routes' budgets (zero: the default policies).
	LiveActivityAccountPolicy limits.Policy
	LiveActivityDevicePolicy  limits.Policy
	// BearerFailureIPPolicy is a client address's budget of failed bearer
	// checks on the routes that read a body before the credential (zero:
	// the default policy).
	BearerFailureIPPolicy limits.Policy
	// MCPHandler, when non-nil, serves exact /mcp for every method. It owns
	// its single MCP admission acquisition; the router adds no Sync slot.
	MCPHandler http.Handler
}

func advertisedCapabilities(options Options) []string {
	capabilities := options.Capabilities
	if capabilities == nil {
		capabilities = protocol.ServerCapabilities()
	}
	result := make([]string, 0, len(capabilities)+1)
	for _, capability := range capabilities {
		if capability == protocol.CapabilityRefreshRecoveryV1 {
			continue
		}
		switch capability {
		case protocol.CapabilityExportV1:
			if options.ExportService == nil {
				continue
			}
		case protocol.CapabilityPushTokenV1:
			if options.PushTokens == nil {
				continue
			}
		case protocol.CapabilityLiveActivityPushV1:
			continue // appended below when push is composed
		}
		result = append(result, capability)
	}
	if liveActivityConfigured(options) {
		result = append(result, protocol.CapabilityLiveActivityPushV1)
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
	{Method: http.MethodPut, Pattern: pushTokenRoute},
	{Method: http.MethodDelete, Pattern: pushTokenRoute},
	{Method: http.MethodPut, Pattern: liveActivityDeviceRoute},
	{Method: http.MethodPut, Pattern: liveActivityActivityRoute},
	{Method: http.MethodDelete, Pattern: liveActivityUnregistrationRoute},
}

// bodyFirstRoutes read their whole body before their credential is checked;
// each takes its own admission slot after the body (admitAfterBody).
var bodyFirstRoutes = map[Route]bool{
	{Method: http.MethodPost, Pattern: "/v1/sync"}:                                       true,
	{Method: http.MethodPost, Pattern: "/v1/family/import/resolve"}:                      true,
	{Method: http.MethodPost, Pattern: "/v1/family/import/{import_id}/resolve-conflict"}: true,
	{Method: http.MethodPut, Pattern: pushTokenRoute}:                                    true,
	{Method: http.MethodDelete, Pattern: pushTokenRoute}:                                 true,
	{Method: http.MethodPut, Pattern: liveActivityDeviceRoute}:                           true,
	{Method: http.MethodPut, Pattern: liveActivityActivityRoute}:                         true,
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
	if options.Admission == nil {
		options.Admission, _ = admission.New(admission.DefaultTotal, 0)
	}
	if options.BodyReads == nil {
		options.BodyReads, _ = admission.NewPerKey(defaultBodyReadTotal, defaultBodyReadsPerClient)
	}
	if options.BodyReadTimeout <= 0 {
		options.BodyReadTimeout = defaultBodyReadTimeout
	}
	if options.LiveActivityAccountPolicy == (limits.Policy{}) {
		options.LiveActivityAccountPolicy = limits.DefaultPolicies()[limits.ScopeLiveActivityAccount]
	}
	if options.LiveActivityDevicePolicy == (limits.Policy{}) {
		options.LiveActivityDevicePolicy = limits.DefaultPolicies()[limits.ScopeLiveActivityDevice]
	}
	if options.BearerFailureIPPolicy == (limits.Policy{}) {
		options.BearerFailureIPPolicy = limits.DefaultPolicies()[limits.ScopeBearerFailureIP]
	}
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
		{Method: http.MethodPost, Pattern: "/v1/export"}:                                     exportCreateHandler(options),
		{Method: http.MethodGet, Pattern: "/v1/export/{id}"}:                                 exportStatusHandler(options),
		{Method: http.MethodGet, Pattern: "/v1/export/{id}/download"}:                        exportDownloadHandler(options),
		{Method: http.MethodGet, Pattern: "/v1/live"}:                                        liveHandler(options),
		{Method: http.MethodPut, Pattern: pushTokenRoute}:                                    pushTokenHandler(options, true),
		{Method: http.MethodDelete, Pattern: pushTokenRoute}:                                 pushTokenHandler(options, false),
		{Method: http.MethodPut, Pattern: liveActivityDeviceRoute}:                           liveActivityRegistrationHandler(options, true),
		{Method: http.MethodPut, Pattern: liveActivityActivityRoute}:                         liveActivityRegistrationHandler(options, false),
		{Method: http.MethodDelete, Pattern: liveActivityUnregistrationRoute}:                liveActivityUnregistrationHandler(options),
	}
	for _, route := range RoutesForAuthorizationMode(options.AppleAuthorizationMode) {
		if route.Pattern == "/v1/auth/apple/notifications" {
			handlers[route] = appleNotificationHandler(options)
		}
		if route.Pattern == pushTokenRoute && options.PushTokens == nil {
			// A server without push does not serve the route (404).
			continue
		}
		if (route.Pattern == liveActivityDeviceRoute || route.Pattern == liveActivityActivityRoute) && options.LiveActivityService == nil {
			// A server with Live Activity push disabled does not serve
			// registration (404); unregistration needs none of it.
			continue
		}
		if route.Pattern == liveActivityUnregistrationRoute && options.LiveActivityUnregistration == nil {
			continue
		}
		var handler http.Handler = handlers[route]
		// Only operational probes bypass finite workload admission, and the
		// routes that read their body before their credential take their
		// slot themselves, once the body is read (admitAfterBody). Route-level
		// classification leaves an independent single-acquisition seam for MCP.
		if route.Pattern != "/healthz" && route.Pattern != "/readyz" && !bodyFirstRoutes[route] {
			handler = WithAdmission(options.Admission, admission.Sync, options.AdmissionMetrics, handler)
		}
		mux.Handle(route.Method+" "+route.Pattern, handler)
	}
	if options.MCPHandler != nil {
		// Exact path, every method: the MCP handler owns authentication,
		// its single MCP admission slot and the SDK's 405 for GET/DELETE.
		mux.Handle("/mcp", options.MCPHandler)
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
