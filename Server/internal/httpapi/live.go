package httpapi

import (
	"errors"
	"net/http"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
)

// liveHandler serves GET /v1/live. Its database work is one pooled
// statement with no transaction (RequestAuthorizer.AuthorizeRequest), after
// everything that needs none: readiness (cached), the IP attempt limit (in
// memory), the bearer token's digests and the headers. The header refusal is
// held and returned by Admit, after authentication, so a bad credential
// still answers 401 first, as the legacy handler's order had it; family
// policy follows it, then the device, account and family attempt limits
// (Charge). The socket checks and the upgrade are unchanged.
//
// The hub's invalidation mark is taken before the authorization read, so a
// revocation that commits after the read and reaches the hub before Bind
// never yields a bound connection: before the reservation, ReserveAfter
// refuses it (503, retryable; the next attempt meets the revocation itself);
// after it, Bind refuses and the upgraded socket is closed 1008. Nothing has
// read the socket yet, so the client's first ping goes unanswered and it
// handles the close like any failed upgrade (a retryable backoff); its next
// attempt meets the revocation itself.
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
			writeSyncFamilyFault(w, request, options, auth.Code(err), err)
			return
		}
		digests, err := options.SessionService.AccessDigests(token)
		if err != nil {
			// A refusal (session_revoked) logs nothing; any other failure
			// is a 5xx whose fault line carries err.
			writeSyncFamilyFault(w, request, options, auth.Code(err), err)
			return
		}
		if options.RequestAuthorizer == nil {
			writeSyncFamilyCode(w, request, options, "service_unavailable")
			return
		}
		_, refusal := syncHeaders(request, protocol.SurfaceLive, options)
		if refusal == "" && options.LiveHub == nil {
			refusal = "service_unavailable" // after the headers, as before
		}
		mark := options.LiveHub.InvalidationMark()
		principal, grant, err := options.RequestAuthorizer.AuthorizeRequest(request.Context(), authority.Request{Digests: digests,
			Admit: func(p access.Principal) error {
				// Every session that is not a refreshed family session is
				// refused here, so family policy never sees a recovery or
				// account session (and never answers recovery_only).
				if p.Scope != "family" || p.SessionScope != "family" || !protocol.ValidUUID(p.SessionFamilyID) {
					return syncRefusal("access_expired")
				}
				if refusal != "" {
					return syncRefusal(refusal)
				}
				return nil
			},
			Charge: func(p access.Principal, g access.Grant) error {
				if g.FamilyID != p.SessionFamilyID {
					// The legacy family read's check, before the charge; the
					// grant is the session's family for every session Admit
					// lets through.
					return syncRefusal("access_expired")
				}
				decision, err := options.Limiter.TakeMany(request.Context(), []limits.Charge{
					{Scope: limits.ScopeLiveAttemptDevice, Key: p.DeviceID, Cost: 1, Policy: options.LiveAttemptDevicePolicy},
					{Scope: limits.ScopeLiveAttemptAccount, Key: p.AccountID, Cost: 1, Policy: options.LiveAttemptAccountPolicy},
					{Scope: limits.ScopeLiveAttemptFamily, Key: g.FamilyID, Cost: 1, Policy: options.LiveAttemptFamilyPolicy},
				})
				if err != nil {
					return limiterUnavailable(err)
				}
				if !decision.Allowed {
					return rateLimited{decision}
				}
				return nil
			}}, "read")
		if err != nil {
			writeRequestError(w, request, options, err)
			return
		}
		reservation, err := options.LiveHub.ReserveAfter(request.Context(), mark, principal, grant)
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
		releaseUpgradeAdmission(request.Context())
		defer connection.Abort()
		cleanup, err := reservation.Bind(connection)
		if errors.Is(err, realtime.ErrAuthorizationChanged) {
			// Never bound: closed 1008 like a revoked connection, though a
			// client still awaiting its first pong sees a failed upgrade.
			_ = connection.Close(realtime.ClosePolicyViolation, "authorization changed")
			return
		}
		if err != nil {
			_ = connection.Close(realtime.CloseTryAgainLater, "capacity unavailable")
			return
		}
		defer cleanup()
		<-connection.ReadDone(options.LiveHub.Context()).Done()
	}
}
