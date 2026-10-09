package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestExpiredConsumedRefreshStillRevokesLineageAndRecordsReceipt(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	successor, err := h.Service.Refresh(t.Context(), original.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	matched, found, err := h.Service.lookupRefreshAcrossKeys(t.Context(), original.RefreshToken)
	if err != nil || !found {
		t.Fatalf("consumed parent found=%t err=%v", found, err)
	}
	reuseCommand, err := DeriveRefreshReuseCommand(RefreshDerivationInput{ServerInstanceID: h.instance, AccountID: matched.row.AccountID,
		DeviceID: matched.row.DeviceID, ParentSessionID: matched.row.SessionID, LineageRootSessionID: matched.row.LineageRootSessionID,
		RecoveryEpoch: matched.row.RecoveryEpoch, ParentKey: matched.key, ParentDigest: matched.digest})
	if err != nil {
		t.Fatal(err)
	}
	successorDigest := tokenDigest(h.tokenKeys.Current, "access", successor.AccessToken)
	successorRow, found, err := h.Service.repository.LookupAccessSession(t.Context(), successorDigest.KeyID, successorDigest.Sum)
	if err != nil || !found {
		t.Fatalf("successor found=%t err=%v", found, err)
	}
	h.Clock.Advance(24 * time.Hour)
	if _, err := h.Service.Refresh(t.Context(), original.RefreshToken); Code(err) != "refresh_reused" {
		t.Fatalf("code=%s err=%v", Code(err), err)
	}
	state, err := h.Service.security.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Receipt(matched.row.AccountID, reuseCommand); !ok {
		t.Fatal("expired consumed refresh did not record the stable reuse receipt")
	}
	var status, reason string
	if err := h.db.Owner.QueryRow(t.Context(), `SELECT status,revoke_reason FROM sessions WHERE session_id=$1`, successorRow.SessionID).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
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

func TestExistingHundredYearSessionIsCappedFromOriginalSignIn(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	h.Service.absoluteAge = 100 * 365 * 24 * time.Hour
	h.Service.refreshTTL = h.Service.absoluteAge
	issuedAt := h.Clock.Now()
	bundle := h.Issue()
	h.Service.absoluteAge = defaultSelfHostedAge
	h.Service.refreshTTL = defaultSelfHostedAge
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

func TestRefreshCASLossRepreparesAtNewRevision(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	competitor := h.newCompetingService()
	challenge, err := h.Identity.Challenge(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	request := identity.ExchangeRequest{CommandID: h.nextID(3), Challenge: challenge.Challenge, IdentityToken: "subject-one",
		InstallationID: "installation-subject-one", DeviceContinuity: &identity.DeviceContinuityClaim{DeviceID: original.DeviceID, Credential: original.DeviceCredential}}
	h.FailHeadCASOnce(func() error {
		if _, exchangeErr := competitor.Exchange(t.Context(), request); exchangeErr != nil {
			return exchangeErr
		}
		h.Clock.Advance(time.Second)
		return nil
	})
	successor, err := h.Service.Refresh(t.Context(), original.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if head, err := h.store.Heads.Load(t.Context()); err != nil || head.Revision != 3 {
		t.Fatalf("head=%+v err=%v", head, err)
	}
	var total, commands, projected, inert, finalDigestInInert int
	var minimumRevision, maximumRevision uint64
	var firstAccessExpiry, finalAccessExpiry time.Time
	finalDigest := tokenDigest(h.tokenKeys.Current, "access", successor.AccessToken)
	if err := h.db.Owner.QueryRow(t.Context(), `SELECT count(*),count(DISTINCT command_id),count(*) FILTER (WHERE projected_at IS NOT NULL),
		count(*) FILTER (WHERE projected_at IS NULL),count(*) FILTER (WHERE projected_at IS NULL AND access_digest=$1),
		min(security_revision),max(security_revision),min(access_expires_at),max(access_expires_at)
		FROM auth_exchange_material WHERE material_kind='refresh'`, finalDigest.Sum[:]).Scan(&total, &commands, &projected, &inert,
		&finalDigestInInert, &minimumRevision, &maximumRevision, &firstAccessExpiry, &finalAccessExpiry); err != nil {
		t.Fatal(err)
	}
	if total != 2 || commands != 2 || projected != 1 || inert != 1 || finalDigestInInert != 0 ||
		minimumRevision != 2 || maximumRevision != 3 || finalAccessExpiry.Sub(firstAccessExpiry) != time.Second {
		t.Fatalf("refresh stages total=%d commands=%d projected=%d inert=%d final_in_inert=%d revisions=%d..%d access=%s..%s",
			total, commands, projected, inert, finalDigestInInert, minimumRevision, maximumRevision, firstAccessExpiry, finalAccessExpiry)
	}
	row, found, err := h.Service.repository.LookupAccessSession(t.Context(), finalDigest.KeyID, finalDigest.Sum)
	if err != nil || !found || row.SecurityRevision != 3 {
		t.Fatalf("successor=%+v found=%t err=%v", row, found, err)
	}
}

func TestResolveRefreshSubjectIsReadOnlyAndEpochBound(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	headBefore, err := h.store.Heads.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	subject, err := h.Service.ResolveRefreshSubject(t.Context(), original.RefreshToken)
	if err != nil || subject.AccountID != original.Account.ID || subject.DeviceID != original.DeviceID {
		t.Fatalf("subject=%+v err=%v", subject, err)
	}
	headAfter, err := h.store.Heads.Load(t.Context())
	if err != nil || headAfter != headBefore {
		t.Fatalf("read-only lookup changed head: before=%+v after=%+v err=%v", headBefore, headAfter, err)
	}
	h.SetEpoch(2)
	if _, err := h.Service.ResolveRefreshSubject(t.Context(), original.RefreshToken); Code(err) != "epoch_changed" {
		t.Fatalf("old epoch subject code=%s err=%v", Code(err), err)
	}
}

func TestRefreshAuthorityDriftRetryIsBounded(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	calls := 0
	var reject func() error
	reject = func() error {
		calls++
		h.FailHeadCASOnce(reject)
		return nil
	}
	h.FailHeadCASOnce(reject)
	if _, err := h.Service.Refresh(t.Context(), original.RefreshToken); Code(err) != "security_busy" {
		t.Fatalf("code=%s err=%v", Code(err), err)
	}
	if calls != maxRefreshPrepareAttempts {
		t.Fatalf("CAS attempts=%d want=%d", calls, maxRefreshPrepareAttempts)
	}
	if head, err := h.store.Heads.Load(t.Context()); err != nil || head.Revision != 1 {
		t.Fatalf("head=%+v err=%v", head, err)
	}
}

func TestRefreshRotationAcrossCurrentKeyChange(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	old := h.tokenKeys.Current
	next := testKeyring("session.v2", 0x66).Current
	rotated := h.tokenKeys
	rotated.Current = next
	rotated.Previous = []identity.DigestKey{old}
	h.tokenKeys = rotated
	h.Service.tokenKeys = cloneKeyring(rotated)
	if _, err := h.Service.Authenticate(t.Context(), original.AccessToken); err != nil {
		t.Fatalf("retained access key was not read: %v", err)
	}
	successor, err := h.Service.Refresh(t.Context(), original.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := h.Service.repository.LookupAccessSession(t.Context(), next.ID,
		tokenDigest(next, "access", successor.AccessToken).Sum); err != nil || !found {
		t.Fatalf("successor was not written with current key: found=%t err=%v", found, err)
	}
}

func TestRefreshReturnsProjectedAccountView(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	owner, err := h.db.Owner.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()
	if _, err := owner.Exec(t.Context(), `SELECT set_config('app.account_id',$1,false)`, original.Account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), `UPDATE accounts SET display_name='Updated Parent',profile_required=true WHERE id=$1`, original.Account.ID); err != nil {
		t.Fatal(err)
	}
	successor, err := h.Service.Refresh(t.Context(), original.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if successor.Account.ID != original.Account.ID || successor.Account.DisplayName != "Updated Parent" || !successor.Account.ProfileRequired {
		t.Fatalf("account=%+v", successor.Account)
	}
}

func TestRefreshFailsClosedWhenAccountProjectionIsMissing(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	before := h.authMutationSnapshot()
	owner, err := h.db.Owner.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()
	if _, err := owner.Exec(t.Context(), `SELECT set_config('app.account_id',$1,false)`, original.Account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), `DELETE FROM account_identities WHERE account_id=$1`, original.Account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.Refresh(t.Context(), original.RefreshToken); Code(err) != "service_unavailable" {
		t.Fatalf("code=%s err=%v", Code(err), err)
	}
	after := h.authMutationSnapshot()
	if after != before {
		t.Fatalf("missing account projection mutated auth state: before=%+v after=%+v", before, after)
	}
}

func TestOnlyDigestsPersist(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	successor, err := h.Service.Refresh(t.Context(), original.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{original.AccessToken, original.RefreshToken, successor.AccessToken, successor.RefreshToken} {
		var found bool
		if err := h.db.Owner.QueryRow(t.Context(), `SELECT EXISTS(
			SELECT 1 FROM sessions AS value WHERE to_jsonb(value)::text LIKE '%' || $1 || '%'
			UNION ALL SELECT 1 FROM session_refresh_history AS value WHERE to_jsonb(value)::text LIKE '%' || $1 || '%'
			UNION ALL SELECT 1 FROM auth_exchange_material AS value WHERE to_jsonb(value)::text LIKE '%' || $1 || '%')`, raw).Scan(&found); err != nil {
			t.Fatal(err)
		}
		if found {
			t.Fatal("raw bearer persisted in PostgreSQL")
		}
		if err := filepath.WalkDir(h.root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() {
				return walkErr
			}
			contents, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if bytes.Contains(contents, []byte(raw)) {
				t.Fatalf("raw bearer persisted in ledger file %s", path)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
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

func TestLogoutIsLedgerBacked(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	bundle := h.Issue()
	principal, err := h.Service.Authenticate(t.Context(), bundle.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	h.FailProjectionOnce()
	commandID := h.nextID(7)
	if _, err := h.Service.Logout(t.Context(), principal, commandID); Code(err) != "service_unavailable" {
		t.Fatalf("logout code=%s err=%v", Code(err), err)
	}
	h.RestartAndReconcile()
	result, err := h.Service.Logout(t.Context(), principal, commandID)
	if err != nil || result.CommandID != commandID || result.Outcome != "committed" {
		t.Fatalf("logout replay result=%+v err=%v", result, err)
	}
	if _, err := h.Service.Authenticate(t.Context(), bundle.AccessToken); Code(err) != "session_revoked" {
		t.Fatalf("logout did not survive projection replay: code=%s err=%v", Code(err), err)
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

// Authenticate's unavailable errors carry their database cause for the log
// line; to the client they are still service_unavailable.
func TestUnavailableWrappedWithItsCauseKeepsItsCode(t *testing.T) {
	wrapped := fmt.Errorf("%w: %w", ErrUnavailable, errors.New("conn closed"))
	if Code(wrapped) != "service_unavailable" || !errors.Is(wrapped, ErrUnavailable) {
		t.Fatalf("wrapped unavailable lost its code: %v", wrapped)
	}
}
