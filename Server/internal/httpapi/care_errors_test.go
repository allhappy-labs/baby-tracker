package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/care"
	familysync "babytracker/server/internal/sync"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// errorResponse is one handler's error as the client receives it.
type errorResponse struct {
	status         int
	body, bearer   string
	retryAfterHint string
}

func writtenError(write func(http.ResponseWriter, *http.Request)) errorResponse {
	rec := httptest.NewRecorder()
	write(rec, httptest.NewRequest(http.MethodPost, "/v1/sync", nil))
	return errorResponse{status: rec.Code, body: rec.Body.String(), bearer: rec.Header().Get("WWW-Authenticate"), retryAfterHint: rec.Header().Get("Retry-After")}
}

// The care sentinels a sync request can return (care.ApplyBatch's Fatal
// refusals and the batch policy's) map to the same HTTP status and error
// body as the error the legacy path returned for the same event: its sync
// or authority twin where the twin still exists, otherwise the legacy
// code's frozen status and body. The removed legacy records package's
// sentinels were unmapped errors, so an unmapped error stands in for them.
func TestCareErrorsMapLikeTheirLegacyTwins(t *testing.T) {
	errUnmappedLegacy := errors.New("legacy records sentinel")
	options := Options{InstanceID: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", RecoveryEpoch: 1}
	syncError := func(err error) errorResponse {
		return writtenError(func(w http.ResponseWriter, r *http.Request) { writeSyncError(w, r, options, err) })
	}
	code := func(code string) errorResponse {
		return writtenError(func(w http.ResponseWriter, r *http.Request) { writeSyncCode(w, r, options, code) })
	}
	familyCode := func(code string) errorResponse {
		return writtenError(func(w http.ResponseWriter, r *http.Request) { writeSyncFamilyCode(w, r, options, code) })
	}
	for _, test := range []struct {
		name   string
		care   error
		legacy errorResponse // what the legacy path wrote
		status int
	}{
		// legacy: sync.ErrMutationIDReused (errReceiptCollision) -> writeSyncCode.
		{"mutation_id_reused", care.ErrMutationIDReused, code("mutation_id_reused"), http.StatusConflict},
		{"mutation_id_reused via sync", familysync.ErrMutationIDReused, code("mutation_id_reused"), http.StatusConflict},
		// legacy: sync.ErrFeatureUnavailable aliased the records package's ErrFeatureUnavailable, unmapped: a retryable 503.
		{"feature_unavailable", care.ErrFeatureUnavailable, syncError(errUnmappedLegacy), http.StatusServiceUnavailable},
		// legacy: the journal reservation past 2^53-1 failed with pgx.ErrNoRows: a retryable 503.
		{"sequence exhausted", care.ErrSequenceExhausted, syncError(pgx.ErrNoRows), http.StatusServiceUnavailable},
		// legacy: the per-mutation guard refused a viewer (authority.ErrReadOnlyCaregiver)
		// or the in-transaction gate did (sync.ErrReadOnlyCaregiver); both wrote this body.
		{"read_only_caregiver", care.ErrReadOnlyCaregiver, syncError(authority.ErrReadOnlyCaregiver), http.StatusForbidden},
		{"read_only_caregiver via sync", familysync.ErrReadOnlyCaregiver, code("read_only_caregiver"), http.StatusForbidden},
		// legacy: the per-mutation guard refused the action with the authority's codes.
		{"owner_required", authority.ErrOwnerRequired, familyCode("owner_required"), http.StatusForbidden},
		{"membership_revoked", authority.ErrMembershipRevoked, familyCode("membership_revoked"), http.StatusForbidden},
		// legacy: a quota refusal (the records package's ErrQuotaExceeded) was unmapped on the sync route.
		{"quota", care.ErrQuotaExceeded, syncError(errUnmappedLegacy), http.StatusServiceUnavailable},
		{"snapshot_required", familysync.ErrSnapshotRequired, code("snapshot_required"), http.StatusConflict},
		{"snapshot_limit", familysync.ErrSnapshotLimit, code("snapshot_limit"), http.StatusConflict},
		{"page_limit", familysync.ErrPageLimit, code("snapshot_limit"), http.StatusConflict},
		{"invalid_page_token", familysync.ErrInvalidPageToken, code("invalid_page_token"), http.StatusBadRequest},
		{"page_expired", familysync.ErrPageExpired, code("page_expired"), http.StatusGone},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := syncError(test.care)
			if got != test.legacy || got.status != test.status {
				t.Fatalf("care error %v wrote %+v, legacy %+v (want status %d)", test.care, got, test.legacy, test.status)
			}
			// A Fatal reaches the handler as returned; a wrapped one maps the same.
			if wrapped := syncError(fmt.Errorf("sync: %w", test.care)); wrapped != got {
				t.Fatalf("wrapped %v wrote %+v, want %+v", test.care, wrapped, got)
			}
		})
	}
	t.Run("import quota", func(t *testing.T) {
		got := writtenError(func(w http.ResponseWriter, r *http.Request) { writeImportError(w, r, options, care.ErrQuotaExceeded) })
		legacy := writtenError(func(w http.ResponseWriter, r *http.Request) {
			response := newErrorResponse("family_storage_limit", "Import request could not be completed", "", false)
			response.ServerInstanceID, response.RecoveryEpoch = options.InstanceID, options.RecoveryEpoch
			writeJSON(w, http.StatusConflict, response)
		})
		if got != legacy || got.status != http.StatusConflict {
			t.Fatalf("import quota wrote %+v, legacy %+v", got, legacy)
		}
	})
}

// An expected refusal reaching writeSyncError's fallback (an authority or
// policy code the client is told not to retry) is logged at Info (an
// expired access token, the routine refresh signal, at Debug), a quota
// refusal at Warn, and only an unexpected failure (a retryable 503 the
// server cannot explain) at Error, as the one fault line (logServerFault)
// whose cause names the failure without repeating its text, so removed
// caregivers retrying do not flood the error log. The response is
// unchanged (TestCareErrorsMapLikeTheirLegacyTwins).
func TestExpectedSyncRefusalsAreNotLoggedAsErrors(t *testing.T) {
	for _, test := range []struct {
		name         string
		err          error
		level, msg   string
		code, detail string
	}{
		{"owner_required", authority.ErrOwnerRequired, "INFO", "sync request refused", "owner_required", ""},
		{"membership_revoked", authority.ErrMembershipRevoked, "INFO", "sync request refused", "membership_revoked", ""},
		{"read_only_caregiver", authority.ErrReadOnlyCaregiver, "INFO", "sync request refused", "read_only_caregiver", ""},
		{"wrapped membership_revoked", fmt.Errorf("sync: %w", authority.ErrMembershipRevoked), "INFO", "sync request refused", "membership_revoked", ""},
		// An expired access token is every device's routine refresh signal,
		// once per token lifetime: Debug. Other authentication refusals stay
		// at Info.
		{"access_expired", authority.ErrAccessExpired, "DEBUG", "sync request refused", "access_expired", ""},
		{"session_revoked", authority.ErrSessionRevoked, "INFO", "sync request refused", "session_revoked", ""},
		{"epoch_changed", authority.ErrEpochChanged, "INFO", "sync request refused", "epoch_changed", ""},
		{"reauthentication_required", authority.ErrReauthenticationRequired, "INFO", "sync request refused", "reauthentication_required", ""},
		{"quota", care.ErrQuotaExceeded, "WARN", "sync request refused", "family_record_quota_exceeded", ""},
		// The fault line's cause names a sentinel or an error of unknown
		// origin by type, and a Postgres error by its SQLSTATE.
		{"sequence exhausted", care.ErrSequenceExhausted, "ERROR", "request failed", "service_unavailable", "*errors.errorString"},
		{"unexpected", errors.New("stored entity no longer decodes"), "ERROR", "request failed", "service_unavailable", "*errors.errorString"},
		{"database", fmt.Errorf("care: read: %w", &pgconn.PgError{Code: "57P01", Message: "terminating connection due to administrator command"}), "ERROR", "request failed", "service_unavailable", "sqlstate 57P01 terminating connection due to administrator command"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			options := Options{InstanceID: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", RecoveryEpoch: 1,
				Logger: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
			writtenError(func(w http.ResponseWriter, r *http.Request) { writeSyncError(w, r, options, test.err) })
			var entry map[string]any
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatalf("one log line, got %q: %v", logs.String(), err)
			}
			if entry["level"] != test.level || entry["msg"] != test.msg {
				t.Fatalf("logged %v %q, want %s %q", entry["level"], entry["msg"], test.level, test.msg)
			}
			if test.code != "" && entry["code"] != test.code {
				t.Fatalf("logged code %v, want %s", entry["code"], test.code)
			}
			if test.detail != "" && entry["cause"] != test.detail {
				t.Fatalf("logged cause %v, want %s", entry["cause"], test.detail)
			}
			if test.level == "ERROR" && test.detail != test.err.Error() && strings.Contains(logs.String(), test.err.Error()) {
				t.Fatalf("the fault line repeats the error's text: %s", logs.String())
			}
		})
	}
}
