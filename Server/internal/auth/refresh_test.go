package auth

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/config"
	"babytracker/server/internal/identity"
)

func TestRefreshLostResponseRevokesLineage(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	successor, err := h.Service.Refresh(t.Context(), original.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.Refresh(t.Context(), original.RefreshToken); Code(err) != "refresh_reused" {
		t.Fatalf("code=%s err=%v", Code(err), err)
	}
	if _, err := h.Service.Authenticate(t.Context(), successor.AccessToken); Code(err) != "session_revoked" {
		t.Fatalf("successor survived lineage revocation: code=%s err=%v", Code(err), err)
	}
}

func TestRefreshCanonicalizesSubMicrosecondClockForPostgresRoundTrip(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	h.Clock.Advance(789 * time.Nanosecond)
	original := h.Issue()
	successor, err := h.Service.Refresh(t.Context(), original.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Service.Refresh(t.Context(), successor.RefreshToken); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredConsumedRefreshStillRevokesLineageAndAuditsTheRevocation(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	successor, err := h.Service.Refresh(t.Context(), original.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	parentID := h.sessionByRefreshToken(original.RefreshToken)
	successorID, _, _, _ := h.sessionByAccessToken(successor.AccessToken)
	h.Clock.Advance(24 * time.Hour)
	if _, err := h.Service.Refresh(t.Context(), original.RefreshToken); Code(err) != "refresh_reused" {
		t.Fatalf("code=%s err=%v", Code(err), err)
	}
	// The reuse revocation is an audited command of its own.
	var revocations int
	h.query(`SELECT count(*) FROM security_audit_events WHERE scope='account' AND scope_id=$1 AND kind='session_revoked' AND payload->>'session_id'=$2`,
		[]any{original.Account.ID, parentID}, &revocations)
	if revocations != 1 {
		t.Fatalf("reuse revocation audit rows=%d", revocations)
	}
	var status, reason string
	h.query(`SELECT status::text, revoke_reason FROM sessions WHERE id=$1`, []any{successorID}, &status, &reason)
	if status != "revoked" || reason != "refresh_reused" {
		t.Fatalf("successor status=%q reason=%q", status, reason)
	}
}

func TestSelfHostedSessionRefreshesPastOneDayWithoutAppleProof(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	bundle := h.Issue()
	if remaining := bundle.ReauthenticateAt.Sub(h.Clock.Now()); remaining != defaultSelfHostedAge {
		t.Fatalf("self-hosted session deadline=%s want=%s", remaining, defaultSelfHostedAge)
	}
	h.Clock.Advance(25 * time.Hour)
	refreshed, err := h.Service.Refresh(t.Context(), bundle.RefreshToken)
	if err != nil {
		t.Fatalf("refresh after one day requires another Apple sign-in: %v", err)
	}
	if _, err := h.Service.Authenticate(t.Context(), refreshed.AccessToken); err != nil {
		t.Fatalf("refreshed access token was refused: %v", err)
	}
}

func TestSelfHostedSessionExpiresOneYearAfterInitialSignIn(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	bundle := h.Issue()
	h.Clock.Advance(364 * 24 * time.Hour)
	refreshed, err := h.Service.Refresh(t.Context(), bundle.RefreshToken)
	if err != nil {
		t.Fatalf("session expired early: %v", err)
	}
	if _, err := h.Service.Authenticate(t.Context(), refreshed.AccessToken); err != nil {
		t.Fatalf("refreshed access token was refused: %v", err)
	}
	h.Clock.Advance(24 * time.Hour)
	if _, err := h.Service.Refresh(t.Context(), refreshed.RefreshToken); Code(err) != "reauthentication_required" {
		t.Fatalf("one-year refresh code=%s err=%v", Code(err), err)
	}
}

func TestSelfHostedAbsoluteDeadlineCannotRefresh(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	bundle := h.Issue()
	h.Clock.Advance(defaultSelfHostedAge)
	if _, err := h.Service.Refresh(t.Context(), bundle.RefreshToken); Code(err) != "reauthentication_required" {
		t.Fatalf("code=%s err=%v", Code(err), err)
	}
}

// A lineage whose deadline_at lies beyond the self-hosted year (sign-ins
// before the cap) is still capped one year after its original sign-in.
func TestExistingHundredYearSessionIsCappedFromOriginalSignIn(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	issuedAt := h.Clock.Now()
	bundle := h.Issue()
	if n := h.exec(`UPDATE sessions SET refresh_expires_at=$2, deadline_at=$2 WHERE account_id=$1`, bundle.Account.ID, issuedAt.Add(100*365*24*time.Hour)); n != 1 {
		t.Fatalf("updated %d sessions", n)
	}
	h.Clock.Advance(364 * 24 * time.Hour)
	refreshed, err := h.Service.Refresh(t.Context(), bundle.RefreshToken)
	if err != nil {
		t.Fatalf("existing session expired early: %v", err)
	}
	if want := issuedAt.Add(defaultSelfHostedAge); !refreshed.ReauthenticateAt.Equal(want) {
		t.Fatalf("client deadline=%s want=%s", refreshed.ReauthenticateAt, want)
	}
	h.Clock.Advance(24 * time.Hour)
	if _, err := h.Service.Refresh(t.Context(), refreshed.RefreshToken); Code(err) != "reauthentication_required" {
		t.Fatalf("existing session refreshed past one year: code=%s err=%v", Code(err), err)
	}
	if _, err := h.Service.Authenticate(t.Context(), refreshed.AccessToken); Code(err) != "reauthentication_required" {
		t.Fatalf("existing session authenticated past one year: code=%s err=%v", Code(err), err)
	}
}

func TestEpochInvalidatesAccessAndRefresh(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	bundle := h.Issue()
	h.SetEpoch(2)
	if _, err := h.Service.Authenticate(t.Context(), bundle.AccessToken); Code(err) != "epoch_changed" {
		t.Fatalf("access code=%s err=%v", Code(err), err)
	}
	if _, err := h.Service.Refresh(t.Context(), bundle.RefreshToken); Code(err) != "epoch_changed" {
		t.Fatalf("refresh code=%s err=%v", Code(err), err)
	}
}

func TestRefreshRace(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	start := make(chan struct{})
	type outcome struct {
		bundle SessionBundle
		err    error
	}
	outcomes := make(chan outcome, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			bundle, err := h.Service.Refresh(context.Background(), original.RefreshToken)
			outcomes <- outcome{bundle: bundle, err: err}
		}()
	}
	ready.Wait()
	close(start)
	first, second := <-outcomes, <-outcomes
	results := []outcome{first, second}
	successes, reuses := 0, 0
	var successor SessionBundle
	for _, result := range results {
		switch Code(result.err) {
		case "":
			successes++
			successor = result.bundle
		case "refresh_reused":
			reuses++
		default:
			t.Fatalf("unexpected refresh result: code=%s err=%v", Code(result.err), result.err)
		}
	}
	if successes != 1 || reuses != 1 {
		t.Fatalf("successes=%d reuses=%d", successes, reuses)
	}
	if _, err := h.Service.Authenticate(t.Context(), successor.AccessToken); Code(err) != "session_revoked" {
		t.Fatalf("race successor survived: code=%s err=%v", Code(err), err)
	}
}

func TestResolveRefreshSubjectIsReadOnlyAndEpochBound(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	before := h.authMutationSnapshot()
	subject, err := h.Service.ResolveRefreshSubject(t.Context(), original.RefreshToken)
	if err != nil || subject.AccountID != original.Account.ID || subject.DeviceID != original.DeviceID {
		t.Fatalf("subject=%+v err=%v", subject, err)
	}
	if after := h.authMutationSnapshot(); after != before {
		t.Fatalf("read-only lookup changed authority: before=%+v after=%+v", before, after)
	}
	if _, err := h.Service.Refresh(t.Context(), original.RefreshToken); err != nil {
		t.Fatalf("subject lookup consumed the token: %v", err)
	}
	h.SetEpoch(2)
	if _, err := h.Service.ResolveRefreshSubject(t.Context(), original.RefreshToken); Code(err) != "epoch_changed" {
		t.Fatalf("old epoch subject code=%s err=%v", Code(err), err)
	}
}

func TestRefreshRotationAcrossCurrentKeyChange(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	next := testKeyring("session.v2", 0x66).Current
	h.RotateTokenKeys(next)
	if _, err := h.Service.Authenticate(t.Context(), original.AccessToken); err != nil {
		t.Fatalf("retained access key was not read: %v", err)
	}
	successor, err := h.Service.Refresh(t.Context(), original.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, keyID, _ := h.sessionByAccessToken(successor.AccessToken); keyID != next.ID {
		t.Fatalf("successor was written with key %q, want %q", keyID, next.ID)
	}
	if _, err := h.Service.Authenticate(t.Context(), successor.AccessToken); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshReturnsProjectedAccountView(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	if n := h.exec(`UPDATE accounts SET display_name='Updated Parent',profile_required=true WHERE id=$1`, original.Account.ID); n != 1 {
		t.Fatalf("updated %d accounts", n)
	}
	successor, err := h.Service.Refresh(t.Context(), original.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if successor.Account.ID != original.Account.ID || successor.Account.DisplayName != "Updated Parent" || !successor.Account.ProfileRequired {
		t.Fatalf("account=%+v", successor.Account)
	}
}

// An account without a live identity binding has no presentation: refresh
// fails closed without rotating.
func TestRefreshFailsClosedWhenAccountProjectionIsMissing(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	if n := h.exec(`DELETE FROM account_identities WHERE account_id=$1`, original.Account.ID); n != 1 {
		t.Fatalf("deleted %d identities", n)
	}
	before := h.authMutationSnapshot()
	if _, err := h.Service.Refresh(t.Context(), original.RefreshToken); Code(err) != "service_unavailable" {
		t.Fatalf("code=%s err=%v", Code(err), err)
	}
	after := h.authMutationSnapshot()
	if after != before {
		t.Fatalf("missing account identity mutated auth state: before=%+v after=%+v", before, after)
	}
}

func TestOnlyDigestsPersist(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	successor, err := h.Service.Refresh(t.Context(), original.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	recoverable := recoveryRequest(successor)
	recoverable.DeviceContinuity.Credential = original.DeviceCredential
	recovered, err := h.Service.RefreshRecoverable(t.Context(), recoverable)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{original.AccessToken, original.RefreshToken, successor.AccessToken, successor.RefreshToken, recovered.AccessToken,
		recovered.RefreshToken, original.DeviceCredential} {
		var found bool
		h.query(`SELECT EXISTS(
			SELECT 1 FROM sessions AS value WHERE to_jsonb(value)::text LIKE '%' || $1 || '%'
			UNION ALL SELECT 1 FROM refresh_receipts AS value WHERE to_jsonb(value)::text LIKE '%' || $1 || '%' OR convert_from(value.material,'UTF8') LIKE '%' || $1 || '%'
			UNION ALL SELECT 1 FROM devices AS value WHERE to_jsonb(value)::text LIKE '%' || $1 || '%'
			UNION ALL SELECT 1 FROM security_audit_events AS value WHERE to_jsonb(value)::text LIKE '%' || $1 || '%'
			UNION ALL SELECT 1 FROM command_receipts AS value WHERE to_jsonb(value)::text LIKE '%' || $1 || '%')`, []any{raw}, &found)
		if found {
			t.Fatal("raw bearer or continuity credential persisted in PostgreSQL")
		}
	}
}

func TestAccessTokenLocations(t *testing.T) {
	const token = "secret-access-token"
	for _, test := range []struct {
		name  string
		build func(*http.Request)
	}{
		{name: "query", build: func(request *http.Request) { request.URL.RawQuery = "access_token=" + token }},
		{name: "cookie", build: func(request *http.Request) { request.AddCookie(&http.Cookie{Name: "access_token", Value: token}) }},
		{name: "websocket protocol", build: func(request *http.Request) { request.Header.Set("Sec-WebSocket-Protocol", "bearer."+token) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "https://sync.example.test/v1/families", nil)
			test.build(request)
			if _, err := BearerToken(request); Code(err) != "invalid_request" {
				t.Fatalf("code=%s err=%v", Code(err), err)
			}
		})
	}
	request := httptest.NewRequest(http.MethodGet, "https://sync.example.test/v1/families", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	got, err := BearerToken(request)
	if err != nil || got != token {
		t.Fatalf("header token=%q err=%v", got, err)
	}
}

func TestBearerTokenRejectsRequestWithoutURL(t *testing.T) {
	request := &http.Request{Header: make(http.Header)}
	request.Header.Set("Authorization", "Bearer secret-access-token")
	if _, err := BearerToken(request); Code(err) != "invalid_request" {
		t.Fatalf("code=%s err=%v", Code(err), err)
	}
}

func TestLogoutExactRetryReturnsReceiptAndNewCommandIsDenied(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	bundle := h.Issue()
	principal, err := h.Service.Authenticate(t.Context(), bundle.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	commandID := h.nextID(7)
	first, err := h.Service.Logout(t.Context(), principal, commandID)
	if err != nil {
		t.Fatal(err)
	}
	head := h.authMutationSnapshot()
	second, err := h.Service.Logout(t.Context(), principal, commandID)
	if err != nil || second != first {
		t.Fatalf("exact retry first=%+v second=%+v err=%v", first, second, err)
	}
	if after := h.authMutationSnapshot(); after != head {
		t.Fatalf("exact logout retry mutated state: before=%+v after=%+v", head, after)
	}
	if _, err := h.Service.Logout(t.Context(), principal, h.nextID(7)); err == nil {
		t.Fatal("new logout command was accepted against a revoked device")
	}
}

// The service's unavailable errors carry their cause for the log line; to
// the client they are still service_unavailable. proposedActor's is the one
// such site an input can reach: a service whose actor-ID key is unset
// cannot derive a new identity's account.
func TestUnavailableWrappedWithItsCauseKeepsItsCode(t *testing.T) {
	service := &Service{instanceID: "50000000-0000-4000-8000-000000000001"}
	_, _, err := service.proposedActor(identity.VerifiedExchange{
		Identity:  identity.VerifiedIdentity{Provider: "apple", Subject: "subject", Audience: "audience"},
		Authority: identity.IdentityAuthority{Status: "absent", NextActorGeneration: 1},
	})
	if Code(err) != "service_unavailable" || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("proposedActor: %v, want service_unavailable", err)
	}
	if !strings.Contains(err.Error(), "invalid actor ID input") {
		t.Fatalf("proposedActor dropped its cause: %v", err)
	}
}

// The other unavailable sites that hold an error (newDevice's and
// Exchange's crypto/rand reads) cannot be reached from a test: since Go
// 1.24 crypto/rand.Read never returns an error. Their wrapping is pinned
// by source instead: no `if err != nil` in the package returns a bare
// ErrUnavailable, so a cause is never dropped there.
func TestNoUnavailableSiteDropsItsCause(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	set := token.NewFileSet()
	wrapped := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(set, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			guard, ok := node.(*ast.IfStmt)
			if !ok || !isErrNotNil(guard.Cond) {
				return true
			}
			for _, statement := range guard.Body.List {
				ret, ok := statement.(*ast.ReturnStmt)
				if !ok || len(ret.Results) == 0 {
					continue
				}
				switch last := ret.Results[len(ret.Results)-1].(type) {
				case *ast.Ident:
					if last.Name == "ErrUnavailable" {
						t.Errorf("%s: returns a bare ErrUnavailable, dropping err", set.Position(ret.Pos()))
					}
				case *ast.CallExpr:
					if strings.Contains(types.ExprString(last), `"%w: %w", ErrUnavailable, err`) {
						wrapped++
					}
				}
			}
			return true
		})
	}
	if wrapped < 6 {
		t.Fatalf("%d wrapped ErrUnavailable sites, want at least the 6 of sessions.go and devices.go", wrapped)
	}
}

// isErrNotNil reports whether cond is `err != nil`.
func isErrNotNil(cond ast.Expr) bool {
	binary, ok := cond.(*ast.BinaryExpr)
	if !ok || binary.Op != token.NEQ {
		return false
	}
	left, lok := binary.X.(*ast.Ident)
	right, rok := binary.Y.(*ast.Ident)
	return lok && rok && left.Name == "err" && right.Name == "nil"
}
