package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
)

// refusingSync is a sync service whose request transaction refuses every
// bearer (session_revoked), as the authority's does for one no session
// holds. It runs no hook, so a held body refusal never shows.
type refusingSync struct{ SyncService }

func (refusingSync) ServeSync(context.Context, authority.Request, protocol.SyncRequest) (protocol.SyncResponse, error) {
	return protocol.SyncResponse{}, authority.ErrSessionRevoked
}

// refusingBearers is a BearerAuthenticator that refuses every bearer.
type refusingBearers struct{}

func (refusingBearers) Authenticate(context.Context, []authority.KeyedDigest) (access.Principal, error) {
	return access.Principal{}, authority.ErrSessionRevoked
}

// BenchmarkRefusedFakeBearerSyncBody is the app CPU of one POST /v1/sync
// with a 1 MiB body and a bearer no session holds (Task 19's bad-bearer
// cell), through the whole handler stack: before (body/read-then-refuse),
// the body read and strictly decoded, then the request transaction's 401;
// after (check-first), the credential check's 401 with the body unread. The
// database's side of either statement is not in it (both are one
// app.request_authority lookup by digest).
func BenchmarkRefusedFakeBearerSyncBody(b *testing.B) {
	effective := protocol.DefaultLimits()
	prefix, suffix := `{"protocol_version":"v1","capabilities":["records-v1"],"mutations":[],"page_token":"`, `"}`
	body := []byte(prefix + strings.Repeat("a", effective.SyncBodyBytes-len(prefix)-len(suffix)) + suffix)
	limiter, err := limits.New(limits.Options{Pepper: [32]byte{23}, Now: time.Now})
	if err != nil {
		b.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		check BearerAuthenticator
	}{{"body-then-refuse", nil}, {"check-first", refusingBearers{}}} {
		b.Run(c.name, func(b *testing.B) {
			handler := New(Options{Readiness: stackReady{}, SyncLimits: &effective, SessionService: sessionServiceStub{},
				SyncService: refusingSync{}, BearerAuthenticator: c.check, Limiter: limiter,
				BearerFailureIPPolicy: limits.Policy{Count: limits.PolicyMaximum(limits.ScopeBearerFailureIP), Window: time.Second}})
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				req := httptest.NewRequest(http.MethodPost, "/v1/sync", bytes.NewReader(body))
				req.Header.Set("Authorization", "Bearer access")
				req.Header.Set("X-Baby-Tracker-Protocol", "v1")
				req.Header.Set("X-Baby-Tracker-Capabilities", "records-v1")
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != http.StatusUnauthorized {
					b.Fatalf("%d %s", rec.Code, rec.Body.String())
				}
			}
		})
	}
}
