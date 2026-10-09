package httpapi

import (
	"net/http"
	"time"

	"babytracker/server/internal/auth"
	"babytracker/server/internal/family"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
)

func liveHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if err := authReadyError(request, options); err != nil {
			writeSyncFamilyFault(w, request, options, authReadinessCode(err), err)
			return
		}
		if _, ok := takeIPLimit(w, request, options, limits.ScopeLiveAttemptIP, options.LiveAttemptIPPolicy); !ok {
			return
		}
		token, err := auth.BearerToken(request)
		if err != nil {
			writeSyncFamilyCode(w, request, options, auth.Code(err))
			return
		}
		principal, err := options.SessionService.Authenticate(request.Context(), token)
		if err != nil {
			writeSyncFamilyCode(w, request, options, auth.Code(err))
			return
		}
		if principal.Scope != "family" || principal.SessionScope != "family" || !protocol.ValidUUID(principal.SessionFamilyID) {
			writeSyncFamilyCode(w, request, options, "access_expired")
			return
		}
		if _, ok := validateSyncHeaders(w, request, protocol.SurfaceLive, options); !ok {
			return
		}
		if options.FamilyService == nil || options.LiveFamilyAuthorizer == nil || options.LiveHub == nil {
			writeSyncFamilyCode(w, request, options, "service_unavailable")
			return
		}
		view, err := options.FamilyService.Current(request.Context(), principal)
		if err != nil || view.ID != principal.SessionFamilyID {
			if err == nil {
				writeSyncFamilyCode(w, request, options, "access_expired")
			} else {
				writeSyncFamilyError(w, request, options, err)
			}
			return
		}
		grant, err := options.LiveFamilyAuthorizer.Authorize(request.Context(), principal, view.ID, "read")
		if err != nil {
			writeSyncFamilyCode(w, request, options, family.Code(err))
			return
		}
		decision, err := options.Limiter.TakeMany(request.Context(), []limits.Charge{
			{Scope: limits.ScopeLiveAttemptDevice, Key: principal.DeviceID, Cost: 1, Policy: options.LiveAttemptDevicePolicy},
			{Scope: limits.ScopeLiveAttemptAccount, Key: principal.AccountID, Cost: 1, Policy: options.LiveAttemptAccountPolicy},
			{Scope: limits.ScopeLiveAttemptFamily, Key: grant.FamilyID, Cost: 1, Policy: options.LiveAttemptFamilyPolicy},
		})
		if err != nil {
			writeSyncFamilyFault(w, request, options, "service_unavailable", err)
			return
		}
		if !decision.Allowed {
			writeSyncRateLimited(w, request, options, decision)
			return
		}
		reservation, err := options.LiveHub.Reserve(request.Context(), principal, grant)
		if err != nil {
			if options.Logger != nil {
				options.Logger.Warn("live reservation rejected", "error", err.Error())
			}
			if err == realtime.ErrCapacity {
				writeSyncRateLimited(w, request, options, limits.Decision{Allowed: false, RetryAfter: 5 * time.Second})
			} else {
				writeSyncFamilyFault(w, request, options, "service_unavailable", err)
			}
			return
		}
		defer reservation.Release()
		if request.URL.RawQuery != "" || len(request.Header.Values("Cookie")) != 0 ||
			len(request.Header.Values("Sec-WebSocket-Protocol")) != 1 || request.Header.Get("Sec-WebSocket-Protocol") != realtime.Subprotocol {
			writeSyncCode(w, request, options, "invalid_request")
			return
		}
		origins := request.Header.Values("Origin")
		if len(origins) > 1 || len(origins) == 1 && origins[0] != options.CanonicalOrigin {
			writeSyncCode(w, request, options, "invalid_request")
			return
		}
		connection, err := realtime.Accept(w, request, options.CanonicalOrigin)
		if err != nil {
			return
		}
		defer connection.Abort()
		cleanup, err := reservation.Bind(connection)
		if err != nil {
			_ = connection.Close(realtime.CloseTryAgainLater, "capacity unavailable")
			return
		}
		defer cleanup()
		<-connection.ReadDone(options.LiveHub.Context()).Done()
	}
}
