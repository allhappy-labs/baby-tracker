package httpapi

import (
	"net/http"

	"babytracker/server/internal/account"
	"babytracker/server/internal/family"
	"babytracker/server/internal/protocol"
)

func accountDeleteHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		principal, ok := authenticateFamilyRequest(w, request, options)
		if !ok {
			return
		}
		if options.AccountService == nil {
			writeFamilyCode(w, request, "service_unavailable")
			return
		}
		if !takeFamilyLimits(w, request, options, principal, true, "", "") {
			return
		}
		var input protocol.ConfirmedCommandRequest
		if !decodeFamilyRequest(w, request, &input) {
			return
		}
		result, err := options.AccountService.Delete(request.Context(), principal, input.CommandID, input.Confirmed)
		if err != nil {
			// The code is the account service's; a 5xx's fault line
			// carries err.
			writeFamilyFault(w, request, account.Code(err), err)
			return
		}
		// The account service reports the Apple revocation state; Phase 1
		// keeps no Apple refresh token, so it is manual_required in every
		// mode (hosted ruling).
		writeJSON(w, http.StatusOK, protocol.AccountDeletionResponse{
			CommandResponse:              commandResponse(result.Result),
			AppleAuthorizationRevocation: result.AppleAuthorizationRevocation,
		})
	}
}

func accountHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		principal, ok := authenticateFamilyRequest(w, request, options)
		if !ok {
			return
		}
		view, err := options.FamilyService.Account(request.Context(), principal)
		if err != nil {
			writeFamilyError(w, request, err)
			return
		}
		writeJSON(w, http.StatusOK, accountResponse(view))
	}
}

func updateAccountHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		principal, ok := authenticateFamilyRequest(w, request, options)
		if !ok || !takeFamilyLimits(w, request, options, principal, true, "", "") {
			return
		}
		var input protocol.UpdateAccountRequest
		if !decodeFamilyRequest(w, request, &input) {
			return
		}
		view, err := options.FamilyService.UpdateProfile(request.Context(), principal, input.CommandID, input.DisplayName, input.Confirmed)
		if err != nil {
			writeFamilyError(w, request, err)
			return
		}
		writeJSON(w, http.StatusOK, accountResponse(view))
	}
}

func accountResponse(view family.AccountView) protocol.AccountResponse {
	response := protocol.AccountResponse{ProtocolVersion: "v1", ID: view.ID, DisplayName: view.DisplayName,
		Status: view.Status, ProfileRequired: view.ProfileRequired}
	if view.FamilySlot != nil {
		response.FamilySlot = &protocol.FamilySlotResponse{FamilyID: view.FamilySlot.FamilyID, State: view.FamilySlot.State}
	}
	return response
}
