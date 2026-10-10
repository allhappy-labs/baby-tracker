//go:build identitytest

package protocol_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/identity"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

var errDeviceContinuityRequired = errors.New("device_continuity_required")

func TestContinuityReauthenticationPreservesDeviceAndCrossAccountIsDenied(t *testing.T) {
	fixture := newProtocolFixture(t)
	if _, err := fixture.Owner().Sync([]protocol.Mutation{mutation(t, "baby.json", fixture.h.CommandID())}); err != nil {
		t.Fatal(err)
	}
	frozen := fixture.Owner().FrozenTimerStart(testkit.TimerA)
	fixture.DropNextSyncResponseAfterCommit()
	if _, err := fixture.Owner().SyncExact(frozen); err == nil {
		t.Fatal("expected committed response to be dropped")
	}
	original := fixture.h.Owner.DeviceID
	err := fixture.Owner().ReauthenticateWithContinuity()
	if err != nil || fixture.Owner().principal.DeviceID != original {
		t.Fatalf("device=%s err=%v", fixture.Owner().principal.DeviceID, err)
	}
	if err = fixture.crossAccountContinuity("outsider", fixture.Owner()); err == nil {
		t.Fatal("cross-account continuity accepted")
	}
	receipt, err := fixture.Owner().SyncExact(frozen)
	if err != nil || receipt.Outcomes[0].OriginalStatus == nil || *receipt.Outcomes[0].OriginalStatus != "applied" {
		t.Fatalf("owner receipt=%+v err=%v", receipt, err)
	}
}

func TestMissingContinuityIsDeniedForExistingInstallation(t *testing.T) {
	fixture := newProtocolFixture(t)
	client := fixture.Owner()
	if _, err := client.Sync([]protocol.Mutation{mutation(t, "baby.json", fixture.h.CommandID())}); err != nil {
		t.Fatal(err)
	}
	_ = client.FrozenTimerStart(testkit.TimerA)
	originalDevice := client.principal.DeviceID
	originalCredential := client.deviceCredential
	before := fixture.h.DeviceCount(t, client.principal.AccountID)
	journalBefore := int(fixture.h.FamilySequence(t))
	client.deviceCredential = ""
	if err := client.ReauthenticateWithContinuity(); !errors.Is(err, errDeviceContinuityRequired) {
		t.Fatalf("err=%v", err)
	}
	if client.principal.DeviceID != originalDevice || fixture.h.DeviceCount(t, client.principal.AccountID) != before || int(fixture.h.FamilySequence(t)) != journalBefore {
		t.Fatal("missing client continuity allocated or selected another device")
	}
	client.deviceCredential = strings.Repeat("A", 43)
	if err := client.ReauthenticateWithContinuity(); !errors.Is(err, errDeviceContinuityRequired) {
		t.Fatalf("corrupt continuity err=%v", err)
	}
	client.deviceCredential = originalCredential
}

func TestOrdinaryRecordReceiptRecoversAcrossContinuity(t *testing.T) {
	fixture := newProtocolFixture(t)
	client := fixture.Owner()
	value := mutation(t, "baby.json", fixture.h.CommandID())
	raw, err := client.syncRequestBytes([]protocol.Mutation{value})
	if err != nil {
		t.Fatal(err)
	}
	fixture.DropNextSyncResponseAfterCommit()
	if _, err = client.SyncExact(raw); err == nil {
		t.Fatal("expected committed response to be dropped")
	}
	client = fixture.RestartClient(client)
	if err = client.ReauthenticateWithContinuity(); err != nil {
		t.Fatal(err)
	}
	replayed, err := client.SyncExact(raw)
	if err != nil || len(replayed.Outcomes) != 1 || replayed.Outcomes[0].OriginalStatus == nil || *replayed.Outcomes[0].OriginalStatus != "applied" || fixture.h.CareCount(t, "baby", fixture.h.FamilyID) != 1 {
		t.Fatalf("replay=%+v err=%v", replayed, err)
	}
}

func (client *ProtocolClient) ReauthenticateWithContinuity() error {
	originalAccount, originalDevice := client.principal.AccountID, client.principal.DeviceID
	continuity := &protocol.DeviceContinuityClaim{DeviceID: client.principal.DeviceID, Credential: client.deviceCredential}
	response, err := client.fixture.exchangeSigned(client.subject, "installation-"+client.subject, continuity)
	if err != nil {
		if strings.Contains(err.Error(), "device_continuity_invalid") {
			return errDeviceContinuityRequired
		}
		return err
	}
	if response.Account.ID != originalAccount || response.DeviceID != originalDevice || response.FamilySlot == nil || response.FamilySlot.FamilyID != client.fixture.h.FamilyID {
		return fmt.Errorf("continued exchange identity changed: account=%q device=%q family_slot=%+v", response.Account.ID, response.DeviceID, response.FamilySlot)
	}
	var refreshed protocol.SessionResponse
	err = client.fixture.postAuth("/v1/auth/refresh", protocol.RefreshRequest{RequestMeta: protocol.RequestMeta{
		ProtocolVersion: "v1", Capabilities: protocol.IdentityCapabilities()}, RefreshToken: response.RefreshToken}, &refreshed)
	if err != nil {
		return err
	}
	if refreshed.Account.ID != originalAccount || refreshed.DeviceID != originalDevice || refreshed.FamilySlot == nil || refreshed.FamilySlot.FamilyID != client.fixture.h.FamilyID {
		return fmt.Errorf("continued refresh identity changed: account=%q device=%q family_slot=%+v", refreshed.Account.ID, refreshed.DeviceID, refreshed.FamilySlot)
	}
	return client.adoptSession(refreshed)
}

func TestRefreshReuseRevokesLineageAndFreshProofRecoversContinuity(t *testing.T) {
	fixture := newProtocolFixture(t)
	originalDevice := fixture.h.Owner.DeviceID
	consumed := fixture.Owner().refreshToken
	if err := fixture.Owner().refreshHTTP(consumed); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Owner().refreshHTTP(consumed); err == nil || !strings.Contains(err.Error(), "refresh_reused") {
		t.Fatalf("refresh reuse err=%v", err)
	}
	if _, err := fixture.Owner().Sync(nil); err == nil {
		t.Fatal("successor access remained valid after refresh reuse")
	}
	err := fixture.Owner().ReauthenticateWithContinuity()
	if err != nil || fixture.Owner().principal.DeviceID != originalDevice {
		t.Fatalf("device=%q err=%v", fixture.Owner().principal.DeviceID, err)
	}
}

func TestRefreshHTTPTracksActualRotatedSessionIdentity(t *testing.T) {
	fixture := newProtocolFixture(t)
	client := fixture.Owner()
	oldSession, oldAccess, oldRefresh := client.principal.SessionID, client.token, client.refreshToken
	if err := client.refreshHTTP(oldRefresh); err != nil {
		t.Fatal(err)
	}
	authenticated, err := fixture.h.Auth.Authenticate(t.Context(), client.token)
	if err != nil {
		t.Fatal(err)
	}
	if client.token == oldAccess || client.refreshToken == oldRefresh || client.principal.SessionID == oldSession {
		t.Fatalf("refresh did not rotate credentials/session: principal=%+v", client.principal)
	}
	if client.principal != authenticated {
		t.Fatalf("tracked principal=%+v authenticated principal=%+v", client.principal, authenticated)
	}
}

func TestAbsoluteAgeRequiresFreshProofWithoutLosingContinuity(t *testing.T) {
	fixture := newProtocolFixture(t)
	original := fixture.h.Owner.DeviceID
	fixture.h.Now = fixture.h.Now.Add(365*24*time.Hour + time.Hour)
	if err := fixture.Owner().refreshHTTP(fixture.Owner().refreshToken); err == nil || !strings.Contains(err.Error(), "reauthentication_required") {
		t.Fatalf("refresh err=%v", err)
	}
	err := fixture.Owner().ReauthenticateWithContinuity()
	if err != nil || fixture.Owner().principal.DeviceID != original {
		t.Fatalf("device=%s err=%v", fixture.Owner().principal.DeviceID, err)
	}
}

// exchangeSignedWithProfile is `exchangeSigned` for an account that does not
// exist yet: a redeemer has to be an active account with a display name before
// `invitation_redeemed` will project, and the profile arrives with the very
// first exchange.
func (fixture *protocolFixture) exchangeSignedWithProfile(subject, installationID, displayName string) (protocol.SessionResponse, error) {
	return fixture.exchange(subject, installationID, nil, &protocol.ProfileInput{DisplayName: displayName, Confirmed: true})
}

func (fixture *protocolFixture) exchangeSigned(subject, installationID string, continuity *protocol.DeviceContinuityClaim) (protocol.SessionResponse, error) {
	return fixture.exchange(subject, installationID, continuity, nil)
}

func (fixture *protocolFixture) exchange(subject, installationID string, continuity *protocol.DeviceContinuityClaim, profile *protocol.ProfileInput) (protocol.SessionResponse, error) {
	var challenge protocol.ChallengeResponse
	if err := fixture.postAuth("/v1/auth/challenge", protocol.ChallengeRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: protocol.IdentityCapabilities()}}, &challenge); err != nil {
		return protocol.SessionResponse{}, err
	}
	nonce, err := identity.AppleNonce(challenge.CanonicalOrigin, challenge.ServerInstanceID, challenge.Challenge)
	if err != nil {
		return protocol.SessionResponse{}, err
	}
	proof, err := fixture.issuer.Sign("com.example.babytracker", subject, nonce)
	if err != nil {
		return protocol.SessionResponse{}, err
	}
	request := protocol.AppleExchangeRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: protocol.IdentityCapabilities()},
		CommandID: fixture.h.CommandID(), Challenge: challenge.Challenge, IdentityToken: proof, InstallationID: installationID,
		Profile: profile, DeviceContinuity: continuity}
	var response protocol.SessionResponse
	err = fixture.postAuth("/v1/auth/apple", request, &response)
	return response, err
}

func (fixture *protocolFixture) crossAccountContinuity(subject string, target *ProtocolClient) error {
	_, err := fixture.exchangeSigned(subject, "installation-"+subject, &protocol.DeviceContinuityClaim{
		DeviceID: target.principal.DeviceID, Credential: target.deviceCredential,
	})
	return err
}

func (client *ProtocolClient) refreshHTTP(token string) error {
	var response protocol.SessionResponse
	err := client.fixture.postAuth("/v1/auth/refresh", protocol.RefreshRequest{RequestMeta: protocol.RequestMeta{
		ProtocolVersion: "v1", Capabilities: protocol.IdentityCapabilities()}, RefreshToken: token}, &response)
	if err == nil {
		err = client.adoptSession(response)
	}
	return err
}

func (client *ProtocolClient) adoptSession(response protocol.SessionResponse) error {
	principal, err := client.fixture.h.Auth.Authenticate(client.fixture.t.Context(), response.AccessToken)
	if err != nil {
		return fmt.Errorf("authenticate issued access token: %w", err)
	}
	if principal.AccountID != response.Account.ID || principal.DeviceID != response.DeviceID {
		return fmt.Errorf("issued session identity mismatch: principal=%+v account=%q device=%q", principal, response.Account.ID, response.DeviceID)
	}
	client.token, client.refreshToken, client.principal = response.AccessToken, response.RefreshToken, principal
	return nil
}

func (fixture *protocolFixture) postAuth(path string, input, output any) error {
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(fixture.t.Context(), http.MethodPost, fixture.server.URL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("auth status=%d body=%s", response.StatusCode, body)
	}
	return json.NewDecoder(response.Body).Decode(output)
}

// refreshReceipts counts the recovery receipts of one refresh attempt, read
// as the table owner inside an authority transaction (receipts are
// authority-only under RLS).
func refreshReceipts(t *testing.T, f *protocolFixture, attempt string) int {
	t.Helper()
	tx, err := f.h.Migrator.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	var receipts int
	if _, err = tx.Exec(t.Context(), `SELECT set_config('app.authority','1',true)`); err == nil {
		err = tx.QueryRow(t.Context(), "SELECT count(*) FROM refresh_receipts WHERE attempt_id=$1", attempt).Scan(&receipts)
	}
	if err != nil {
		t.Fatal(err)
	}
	return receipts
}

// TestRecoveryBlackBoxFaultMatrix exercises the HTTP contract around the real
// authority refresh transaction, not an HTTP handler pretending a commit
// occurred.
func TestRecoveryBlackBoxFaultMatrix(t *testing.T) {
	for _, point := range []string{"before_authority_commit", "after_authority_commit", "after_projection"} {
		t.Run(point, func(t *testing.T) {
			f := newProtocolFixture(t)
			owner := f.Owner()
			attempt := f.h.CommandID()
			input := protocol.RecoverableRefreshRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: protocol.RefreshRecoveryCapabilities()}, RefreshToken: owner.refreshToken, RefreshAttemptID: attempt, DeviceContinuity: protocol.DeviceContinuityClaim{DeviceID: owner.principal.DeviceID, Credential: owner.deviceCredential}}
			raw, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			request, err := http.NewRequestWithContext(t.Context(), "POST", f.server.URL+"/v1/auth/refresh-recoverable", bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Identitytest-Recovery-Fault", point)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != 503 {
				t.Fatalf("actual %s boundary fault status=%d, want 503", point, response.StatusCode)
			}
			// The refresh is one authority transaction: before its commit nothing
			// is written; after it the child and its receipt are committed and
			// the lost reply is recovered by the exact retry, with no repair step.
			want := 0
			if point != "before_authority_commit" {
				want = 1
			}
			if receipts := refreshReceipts(t, f, attempt); receipts != want {
				t.Fatalf("%s receipts after the fault=%d want=%d", point, receipts, want)
			}
			f.startServer()
			var first, second protocol.RecoverableRefreshResponse
			if err := f.postAuth("/v1/auth/refresh-recoverable", input, &first); err != nil {
				t.Fatal(err)
			}
			if err := f.postAuth("/v1/auth/refresh-recoverable", input, &second); err != nil {
				t.Fatal(err)
			}
			if first.AccessToken != second.AccessToken || first.RefreshToken != second.RefreshToken || first.ReauthenticateAt != second.ReauthenticateAt || first.RefreshAttemptID != attempt || second.RefreshAttemptID != attempt {
				t.Fatal("recovery changed exact child, attempt or deadline")
			}
			if receipts := refreshReceipts(t, f, attempt); receipts != 1 {
				t.Fatalf("receipt count=%d", receipts)
			}
			if err := owner.adoptSession(second.SessionResponse); err != nil {
				t.Fatal(err)
			}
			if _, err := owner.Sync([]protocol.Mutation{mutation(t, "baby.json", f.h.CommandID())}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.Caregiver().Sync(nil); err != nil {
				t.Fatal(err)
			}
			if f.h.CareCount(t, "baby", f.h.FamilyID) != 1 {
				t.Fatal("ordinary owner write not persisted exactly once")
			}
		})
	}
}
