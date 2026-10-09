package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/account"
	"babytracker/server/internal/family"
	"babytracker/server/internal/security"
)

type accountLifecycleStub struct {
	delete func(context.Context, access.Principal, string, bool) (security.CommandResult, error)
}

func (stub accountLifecycleStub) Delete(ctx context.Context, principal access.Principal, commandID string, confirmed bool) (security.CommandResult, error) {
	return stub.delete(ctx, principal, commandID, confirmed)
}

func TestLifecycleRoutesMapNarrowContractsAndNeverExposeCredentials(t *testing.T) {
	principal := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001", Scope: "family_recovery"}
	commandID := "10000000-0000-4000-8000-000000000009"
	familyID := "30000000-0000-4000-8000-000000000001"
	purgeAfter := time.Date(2026, 10, 21, 12, 0, 0, 123, time.UTC)
	command := security.CommandResult{CommandID: commandID, Outcome: "applied", Code: "ok", Revision: 9}
	var calls []string
	families := familyServiceStub{
		schedule: func(_ context.Context, got access.Principal, gotCommand string, confirmed bool) (family.RecoveryStatus, error) {
			calls = append(calls, "delete")
			if got != principal || gotCommand != commandID || !confirmed {
				t.Fatalf("schedule principal=%+v command=%q confirmed=%t", got, gotCommand, confirmed)
			}
			return family.RecoveryStatus{FamilyID: familyID, State: "deletion_pending", PurgeAfter: purgeAfter, CanRecover: true}, nil
		},
		recovery: func(_ context.Context, got access.Principal) (family.RecoveryStatus, error) {
			calls = append(calls, "status")
			if got != principal {
				t.Fatalf("status principal=%+v", got)
			}
			return family.RecoveryStatus{FamilyID: familyID, State: "deletion_pending", PurgeAfter: purgeAfter, CanRecover: true}, nil
		},
		recover: func(_ context.Context, got access.Principal, gotCommand string, confirmed bool) (security.CommandResult, error) {
			calls = append(calls, "recover")
			if got != principal || gotCommand != commandID || !confirmed {
				t.Fatalf("recover principal=%+v command=%q confirmed=%t", got, gotCommand, confirmed)
			}
			return command, nil
		},
		forfeit: func(_ context.Context, got access.Principal, gotCommand string, confirmed bool) (security.CommandResult, error) {
			calls = append(calls, "forfeit")
			if got != principal || gotCommand != commandID || !confirmed {
				t.Fatalf("forfeit principal=%+v command=%q confirmed=%t", got, gotCommand, confirmed)
			}
			return command, nil
		},
	}
	accounts := accountLifecycleStub{delete: func(_ context.Context, got access.Principal, gotCommand string, confirmed bool) (security.CommandResult, error) {
		calls = append(calls, "account")
		if got != principal || gotCommand != commandID || !confirmed {
			t.Fatalf("account principal=%+v command=%q confirmed=%t", got, gotCommand, confirmed)
		}
		return command, nil
	}}
	options := familyHandlerOptions(t, principal, families, nil)
	options.AccountService = accounts
	handler := New(options)
	body := familyBody(commandID, `"confirmed":true`)
	recoveryBody := strings.Replace(body, `"family-lifecycle-v1"`, `"recovery-v1"`, 1)
	for _, test := range []struct {
		method, path, body, contains string
	}{
		{http.MethodPost, "/v1/family/delete", body, `"family_id":"` + familyID + `"`},
		{http.MethodGet, "/v1/family/recovery", "", `"purge_after":"2026-10-21T12:00:00.000000123Z"`},
		{http.MethodPost, "/v1/family/recover", recoveryBody, `"revision":9`},
		{http.MethodPost, "/v1/family/recovery/forfeit", recoveryBody, `"outcome":"applied"`},
		{http.MethodPost, "/v1/account/delete", body, `"apple_authorization_revocation":"manual_required"`},
	} {
		t.Run(test.path, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer access")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" || !strings.Contains(recorder.Body.String(), test.contains) {
				t.Fatalf("status=%d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
			}
			for _, forbidden := range []string{"access_token", "refresh_token", "authorization_code", "credential", "apple_subject"} {
				if strings.Contains(recorder.Body.String(), forbidden) {
					t.Fatalf("response leaked %q: %s", forbidden, recorder.Body.String())
				}
			}
		})
	}
	if got := strings.Join(calls, ","); got != "delete,status,recover,forfeit,account" {
		t.Fatalf("calls=%q", got)
	}

	options.AppleAuthorizationMode = "code_exchange_required"
	hosted := httptest.NewRequest(http.MethodPost, "/v1/account/delete", strings.NewReader(body))
	hosted.Header.Set("Authorization", "Bearer access")
	recorder := httptest.NewRecorder()
	New(options).ServeHTTP(recorder, hosted)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"apple_authorization_revocation":"pending"`) {
		t.Fatalf("hosted status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestFamilyRecoveryMutationsRequireRecoveryCapability(t *testing.T) {
	principal := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", Scope: "family_recovery"}
	calls := 0
	service := familyServiceStub{recover: func(context.Context, access.Principal, string, bool) (security.CommandResult, error) {
		calls++
		return security.CommandResult{CommandID: "10000000-0000-4000-8000-000000000009", Outcome: "applied", Code: "ok", Revision: 9}, nil
	}}
	handler := New(familyHandlerOptions(t, principal, service, nil))
	for _, test := range []struct {
		capability string
		status     int
	}{
		{capability: "family-lifecycle-v1", status: http.StatusConflict},
		{capability: "recovery-v1", status: http.StatusOK},
	} {
		body := `{"protocol_version":"v1","capabilities":["` + test.capability + `"],"command_id":"10000000-0000-4000-8000-000000000009","confirmed":true}`
		request := httptest.NewRequest(http.MethodPost, "/v1/family/recover", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer access")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != test.status {
			t.Fatalf("capability=%q status=%d body=%s", test.capability, recorder.Code, recorder.Body.String())
		}
	}
	if calls != 1 {
		t.Fatalf("recover calls=%d", calls)
	}
}

func TestLifecycleRoutesRequireExplicitConfirmationAndMapSafeErrors(t *testing.T) {
	principal := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001"}
	service := familyServiceStub{schedule: func(context.Context, access.Principal, string, bool) (family.RecoveryStatus, error) {
		return family.RecoveryStatus{}, family.ErrConfirmationRequired
	}}
	options := familyHandlerOptions(t, principal, service, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/family/delete", strings.NewReader(familyBody("10000000-0000-4000-8000-000000000009", `"confirmed":false`)))
	request.Header.Set("Authorization", "Bearer access")
	recorder := httptest.NewRecorder()
	New(options).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":"confirmation_required"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	options.AccountService = accountLifecycleStub{delete: func(context.Context, access.Principal, string, bool) (security.CommandResult, error) {
		return security.CommandResult{}, account.ErrOwnershipTransferRequired
	}}
	request = httptest.NewRequest(http.MethodPost, "/v1/account/delete", strings.NewReader(familyBody("10000000-0000-4000-8000-000000000009", `"confirmed":true`)))
	request.Header.Set("Authorization", "Bearer access")
	recorder = httptest.NewRecorder()
	New(options).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), `"code":"ownership_transfer_required"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	for code, want := range map[string]int{
		"recovery_only": http.StatusForbidden, "owner_identity_unavailable": http.StatusForbidden, "operator_policy_required": http.StatusForbidden,
		"recovery_expired": http.StatusGone, "recovery_forfeited": http.StatusGone,
		"stale_security_revision": http.StatusConflict, "invalid_confirmation": http.StatusConflict,
	} {
		t.Run(code, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeFamilyCode(recorder, httptest.NewRequest(http.MethodGet, "/", nil), code)
			if recorder.Code != want || !strings.Contains(recorder.Body.String(), `"code":"`+code+`"`) || strings.Contains(recorder.Body.String(), errors.New("private").Error()) {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}
