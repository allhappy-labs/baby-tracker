package security

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/access"
)

const (
	operatorTestID        = "90000000-0000-4000-8000-000000000001"
	operatorTestOwner     = "10000000-0000-4000-8000-000000000001"
	operatorTestCaregiver = "10000000-0000-4000-8000-000000000002"
	operatorTestFamily    = "20000000-0000-4000-8000-000000000001"
)

func TestPreparedOperatorOwnerRecoveryCommitsOneEventAndReplaysExactly(t *testing.T) {
	h, now := operatorAuthorityFixture(t, false, true)
	input := operatorRecoveryInput(h.Head().Revision, now.Add(5*time.Minute))
	if err := h.Service.PreflightOperatorRecovery(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	before := h.AppendCount()
	result, err := h.Service.ExecuteOperatorRecovery(t.Context(), input, allowOperatorAuthority{service: h.Service})
	if err != nil || result.Revision != input.ExpectedRevision+1 || h.AppendCount() != before+1 {
		t.Fatalf("result=%+v appends=%d err=%v", result, h.AppendCount()-before, err)
	}
	state, err := h.Service.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	family, _ := state.Family(operatorTestFamily)
	oldOwner, _ := state.Membership(operatorTestFamily, operatorTestOwner)
	newOwner, _ := state.Membership(operatorTestFamily, operatorTestCaregiver)
	if family.OwnerAccountID != operatorTestCaregiver || oldOwner.Role != "caregiver" || newOwner.Role != "owner" {
		t.Fatalf("family=%+v old=%+v new=%+v", family, oldOwner, newOwner)
	}
	head := h.Head()
	candidate := h.ledger.candidates[head.EventHash]
	var event Event
	if err := decodeEventStrict(candidate.EventJSON, &event); err != nil || event.Kind != "owner_recovered" || event.ActorAccountID != operatorTestID || len(event.Effects) != 2 {
		t.Fatalf("operator event=%+v err=%v", event, err)
	}
	for _, effect := range event.Effects {
		if effect.OperatorPolicyID != input.PolicyID || effect.OperatorEvidenceDigest != input.EvidenceDigest || effect.ExpectedRevision == nil || *effect.ExpectedRevision != input.ExpectedRevision {
			t.Fatalf("operator audit effect=%+v", effect)
		}
	}

	h.Service.now = func() time.Time { return input.ConfirmationExpiresAt.Add(time.Minute) }
	replayed, err := h.Service.ExecuteOperatorRecovery(t.Context(), input, allowOperatorAuthority{service: h.Service})
	if err != nil || replayed != result || h.AppendCount() != before+1 {
		t.Fatalf("expired replay=%+v appends=%d err=%v", replayed, h.AppendCount()-before, err)
	}
	if receipt, ok, err := h.Service.ReplayOperatorRecovery(t.Context(), input, allowOperatorAuthority{service: h.Service}); err != nil || !ok || receipt != result {
		t.Fatalf("receipt=%+v ok=%v err=%v", receipt, ok, err)
	}
	changed := input
	changed.NewOwnerAccountID = "10000000-0000-4000-8000-000000000003"
	if _, _, err := h.Service.ReplayOperatorRecovery(t.Context(), changed, allowOperatorAuthority{service: h.Service}); Code(err) != "command_id_reused" {
		t.Fatalf("changed replay code=%q err=%v", Code(err), err)
	}
}

func TestPreparedOperatorExactReplayReconcilesAfterProjectionFailure(t *testing.T) {
	h, now := operatorAuthorityFixture(t, false, true)
	input := operatorRecoveryInput(h.Head().Revision, now.Add(5*time.Minute))
	before := h.AppendCount()
	h.FailProjectionOnce()
	if _, err := h.Service.ExecuteOperatorRecovery(t.Context(), input, allowOperatorAuthority{service: h.Service}); Code(err) != "service_unavailable" {
		t.Fatalf("initial commit code=%q err=%v", Code(err), err)
	}
	committedHead := h.Head()
	if committedHead.Revision != input.ExpectedRevision+1 || h.AppendCount() != before+1 {
		t.Fatalf("head=%+v appends=%d", committedHead, h.AppendCount()-before)
	}
	replayed, ok, err := h.Service.ReplayOperatorRecovery(t.Context(), input, allowOperatorAuthority{service: h.Service})
	if err != nil || !ok || replayed.Revision != committedHead.Revision {
		t.Fatalf("replay=%+v ok=%v err=%v", replayed, ok, err)
	}
	projected, err := h.Projection.Head(t.Context())
	if err != nil || projected != committedHead || h.AppendCount() != before+1 {
		t.Fatalf("projection=%+v head=%+v appends=%d err=%v", projected, committedHead, h.AppendCount()-before, err)
	}
}

func TestPreparedOperatorReplayRejectsRecoveryDriftBeforeProjectionRepair(t *testing.T) {
	h, now := operatorAuthorityFixture(t, false, true)
	input := operatorRecoveryInput(h.Head().Revision, now.Add(5*time.Minute))
	h.FailProjectionOnce()
	if _, err := h.Service.ExecuteOperatorRecovery(t.Context(), input, allowOperatorAuthority{service: h.Service}); Code(err) != "service_unavailable" {
		t.Fatalf("initial commit code=%q err=%v", Code(err), err)
	}
	guard := &operatorAuthorityTestGuard{service: h.Service, err: errorCode("authorization_quarantine")}
	if _, ok, err := h.Service.ReplayOperatorRecovery(t.Context(), input, guard); ok || Code(err) != "service_unavailable" {
		t.Fatalf("replay ok=%v code=%q err=%v", ok, Code(err), err)
	}
	if guard.calls != 1 || !guard.underFence {
		t.Fatalf("guard calls=%d under_fence=%v", guard.calls, guard.underFence)
	}
	projected, _ := h.Projection.Head(t.Context())
	if projected == h.Head() {
		t.Fatal("invalid recovery identity was allowed to repair projection")
	}
}

func TestPreparedOperatorReplayReturnsUnavailableWhenProjectionRepairFails(t *testing.T) {
	h, now := operatorAuthorityFixture(t, false, true)
	input := operatorRecoveryInput(h.Head().Revision, now.Add(5*time.Minute))
	guard := allowOperatorAuthority{service: h.Service}
	h.FailProjectionOnce()
	if _, err := h.Service.ExecuteOperatorRecovery(t.Context(), input, guard); Code(err) != "service_unavailable" {
		t.Fatalf("initial commit code=%q err=%v", Code(err), err)
	}
	h.FailProjectionOnce()
	if _, ok, err := h.Service.ReplayOperatorRecovery(t.Context(), input, guard); ok || Code(err) != "service_unavailable" {
		t.Fatalf("failed repair ok=%v code=%q err=%v", ok, Code(err), err)
	}
	if replayed, ok, err := h.Service.ReplayOperatorRecovery(t.Context(), input, guard); err != nil || !ok || replayed.Revision != h.Head().Revision {
		t.Fatalf("repaired replay=%+v ok=%v err=%v", replayed, ok, err)
	}
}

func TestPreparedOperatorCommitChecksExpiryAfterFenceAdmission(t *testing.T) {
	h, now := operatorAuthorityFixture(t, false, true)
	input := operatorRecoveryInput(h.Head().Revision, now.Add(time.Minute))
	entered, release := make(chan struct{}), make(chan struct{})
	barrier := &operatorBarrierFence{delegate: &fixtureFence{}, entered: entered, release: release}
	clock := now
	service, err := New(completeTestOptions(Options{Ledger: h.ledger, Heads: h.heads, Projection: h.Projection, Fence: barrier, Now: func() time.Time { return clock }}))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, executeErr := service.ExecuteOperatorRecovery(t.Context(), input, allowOperatorAuthority{service: service})
		done <- executeErr
	}()
	<-entered
	clock = input.ConfirmationExpiresAt
	close(release)
	if err := <-done; Code(err) != "invalid_confirmation" {
		t.Fatalf("queued expiry code=%q err=%v", Code(err), err)
	}
	if h.Head().Revision != input.ExpectedRevision {
		t.Fatalf("expired queued command advanced head=%+v", h.Head())
	}
}

func TestPreparedOperatorCommitRevalidatesRecoveryAfterFenceAdmission(t *testing.T) {
	h, now := operatorAuthorityFixture(t, false, true)
	input := operatorRecoveryInput(h.Head().Revision, now.Add(time.Minute))
	entered, release := make(chan struct{}), make(chan struct{})
	barrier := &operatorBarrierFence{delegate: &fixtureFence{}, entered: entered, release: release}
	service, err := New(completeTestOptions(Options{Ledger: h.ledger, Heads: h.heads, Projection: h.Projection, Fence: barrier, Now: func() time.Time { return now }}))
	if err != nil {
		t.Fatal(err)
	}
	guard := &operatorAuthorityTestGuard{service: service}
	done := make(chan error, 1)
	go func() {
		_, executeErr := service.ExecuteOperatorRecovery(t.Context(), input, guard)
		done <- executeErr
	}()
	<-entered
	guard.err = errorCode("authorization_quarantine")
	close(release)
	if err := <-done; Code(err) != "service_unavailable" {
		t.Fatalf("recovery drift code=%q err=%v", Code(err), err)
	}
	if guard.calls != 1 || !guard.underFence || h.Head().Revision != input.ExpectedRevision {
		t.Fatalf("guard calls=%d under_fence=%v head=%+v", guard.calls, guard.underFence, h.Head())
	}
}

func TestPreparedOperatorEntitlementPreservesHostedBetaAndExactExpiry(t *testing.T) {
	h, now := operatorAuthorityFixture(t, true, true)
	current, err := h.Service.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	existing, _ := current.Entitlement(operatorTestFamily, "family_sync")
	extension := OperatorEntitlementInput{
		CommandID: "30000000-0000-4000-8000-000000000062", OperatorID: operatorTestID,
		FamilyID: operatorTestFamily, Operation: "grant", Key: "family_sync", Source: "hosted_beta",
		ExpiresAt: now.Add(800 * 24 * time.Hour), PolicyID: "Entitlement.Policy_test-v1", EvidenceDigest: operatorEvidenceDigest,
		ReasonCode: "beta_extension", ExpectedRevision: h.Head().Revision, ConfirmationExpiresAt: now.Add(5 * time.Minute),
	}
	if !extension.ExpiresAt.After(mustParseOperatorTime(t, existing.ExpiresAt)) {
		t.Fatal("test extension is not later than current entitlement")
	}
	if err := h.Service.PreflightOperatorEntitlement(t.Context(), extension); err != nil {
		t.Fatal(err)
	}
	grant, err := h.Service.ExecuteOperatorEntitlement(t.Context(), extension, allowOperatorAuthority{service: h.Service})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := h.Service.ReadState(t.Context())
	got, _ := state.Entitlement(operatorTestFamily, "family_sync")
	if got.Source != "hosted_beta" || got.ExpiresAt != extension.ExpiresAt.Format(time.RFC3339) || got.Status != "active" {
		t.Fatalf("grant entitlement=%+v", got)
	}

	suspension := extension
	suspension.CommandID = "30000000-0000-4000-8000-000000000063"
	suspension.Operation = "suspend"
	suspension.ReasonCode = "abuse_suspension"
	suspension.ExpectedRevision = grant.Revision
	if _, err := h.Service.ExecuteOperatorEntitlement(t.Context(), suspension, allowOperatorAuthority{service: h.Service}); err != nil {
		t.Fatal(err)
	}
	state, _ = h.Service.ReadState(t.Context())
	got, _ = state.Entitlement(operatorTestFamily, "family_sync")
	if got.Source != "hosted_beta" || got.ExpiresAt != suspension.ExpiresAt.Format(time.RFC3339) || got.Status != "suspended" {
		t.Fatalf("suspended entitlement=%+v", got)
	}

	changed := suspension
	changed.ExpiresAt = changed.ExpiresAt.Add(time.Second)
	if _, _, err := h.Service.ReplayOperatorEntitlement(t.Context(), changed, allowOperatorAuthority{service: h.Service}); Code(err) != "command_id_reused" {
		t.Fatalf("changed entitlement replay code=%q err=%v", Code(err), err)
	}
}

func TestPreparedOperatorSuspendsFractionalBootstrapWithCanonicalExpiry(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 123456789, time.UTC)
	h := operatorAuthorityFixtureAt(t, true, true, now)
	state, err := h.Service.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	current, ok := state.Entitlement(operatorTestFamily, "family_sync")
	if !ok {
		t.Fatal("bootstrap entitlement missing")
	}
	expectedExpiry := now.Truncate(time.Second).Add(366 * 24 * time.Hour)
	if current.ExpiresAt != expectedExpiry.Format(time.RFC3339) {
		t.Fatalf("bootstrap expiry=%q want=%q", current.ExpiresAt, expectedExpiry.Format(time.RFC3339))
	}
	input := OperatorEntitlementInput{
		CommandID: "30000000-0000-4000-8000-000000000065", OperatorID: operatorTestID,
		FamilyID: operatorTestFamily, Operation: "suspend", Key: "family_sync", Source: "hosted_beta", ExpiresAt: expectedExpiry,
		PolicyID: "entitlement-policy-test-v1", EvidenceDigest: operatorEvidenceDigest, ReasonCode: "abuse_suspension",
		ExpectedRevision: h.Head().Revision, ConfirmationExpiresAt: now.Add(5 * time.Minute).Truncate(time.Second),
	}
	if _, err := h.Service.ExecuteOperatorEntitlement(t.Context(), input, allowOperatorAuthority{service: h.Service}); err != nil {
		t.Fatal(err)
	}
	state, err = h.Service.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got, ok := state.Entitlement(operatorTestFamily, "family_sync")
	if !ok || got.Status != "suspended" || got.Source != "hosted_beta" || got.ExpiresAt != expectedExpiry.Format(time.RFC3339) {
		t.Fatalf("suspended entitlement=%+v ok=%v", got, ok)
	}
}

func TestPreparedOperatorRecoveryRejectsUnfrozenOwnerInactiveOrOutsideCaregiverAndStaleRevision(t *testing.T) {
	t.Run("unfrozen owner", func(t *testing.T) {
		h, now := operatorAuthorityFixture(t, false, false)
		input := operatorRecoveryInput(h.Head().Revision, now.Add(5*time.Minute))
		if err := h.Service.PreflightOperatorRecovery(t.Context(), input); Code(err) != "owner_identity_unavailable" {
			t.Fatalf("code=%q err=%v", Code(err), err)
		}
	})
	t.Run("outside family", func(t *testing.T) {
		h, now := operatorAuthorityFixture(t, false, true)
		input := operatorRecoveryInput(h.Head().Revision, now.Add(5*time.Minute))
		input.NewOwnerAccountID = "10000000-0000-4000-8000-000000000099"
		if _, err := h.Service.ExecuteOperatorRecovery(t.Context(), input, allowOperatorAuthority{service: h.Service}); Code(err) != "owner_identity_unavailable" || h.Head().Revision != input.ExpectedRevision {
			t.Fatalf("code=%q head=%+v err=%v", Code(err), h.Head(), err)
		}
	})
	t.Run("inactive caregiver", func(t *testing.T) {
		h, now := operatorAuthorityFixture(t, false, true)
		freezeOperatorTestIdentity(t, h, operatorTestCaregiver,
			"hmac-sha256:identity.v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", now,
			"30000000-0000-4000-8000-000000000075")
		input := operatorRecoveryInput(h.Head().Revision, now.Add(5*time.Minute))
		if _, err := h.Service.ExecuteOperatorRecovery(t.Context(), input, allowOperatorAuthority{service: h.Service}); Code(err) != "owner_identity_unavailable" || h.Head().Revision != input.ExpectedRevision {
			t.Fatalf("code=%q head=%+v err=%v", Code(err), h.Head(), err)
		}
	})
	t.Run("stale revision", func(t *testing.T) {
		h, now := operatorAuthorityFixture(t, false, true)
		input := operatorRecoveryInput(h.Head().Revision-1, now.Add(5*time.Minute))
		if _, err := h.Service.ExecuteOperatorRecovery(t.Context(), input, allowOperatorAuthority{service: h.Service}); Code(err) != "stale_security_revision" {
			t.Fatalf("code=%q err=%v", Code(err), err)
		}
	})
}

func TestPreparedOperatorEntitlementRejectsChangedSourceAndNonexactSuspensionExpiry(t *testing.T) {
	h, now := operatorAuthorityFixture(t, true, true)
	state, _ := h.Service.ReadState(t.Context())
	current, _ := state.Entitlement(operatorTestFamily, "family_sync")
	base := OperatorEntitlementInput{CommandID: "30000000-0000-4000-8000-000000000064", OperatorID: operatorTestID,
		FamilyID: operatorTestFamily, Operation: "suspend", Key: "family_sync", Source: "hosted_beta", ExpiresAt: mustParseOperatorTime(t, current.ExpiresAt),
		PolicyID: "entitlement-policy-test-v1", EvidenceDigest: operatorEvidenceDigest, ReasonCode: "abuse_suspension",
		ExpectedRevision: h.Head().Revision, ConfirmationExpiresAt: now.Add(5 * time.Minute)}
	changedSource := base
	changedSource.Source = "operator"
	if _, err := h.Service.ExecuteOperatorEntitlement(t.Context(), changedSource, allowOperatorAuthority{service: h.Service}); Code(err) != "invalid_command" {
		t.Fatalf("changed source code=%q err=%v", Code(err), err)
	}
	changedExpiry := base
	changedExpiry.ExpiresAt = changedExpiry.ExpiresAt.Add(time.Second)
	if _, err := h.Service.ExecuteOperatorEntitlement(t.Context(), changedExpiry, allowOperatorAuthority{service: h.Service}); Code(err) != "invalid_command" {
		t.Fatalf("changed expiry code=%q err=%v", Code(err), err)
	}
}

type operatorBarrierFence struct {
	delegate         Fence
	entered, release chan struct{}
	once             sync.Once
}

type operatorAuthorityTestGuard struct {
	service    *Service
	err        error
	calls      int
	underFence bool
}

func (guard *operatorAuthorityTestGuard) ValidateOperatorAuthorityUnderFence(ctx context.Context, _ Head) error {
	guard.calls++
	guard.underFence = guard.service.hasFenceMarker(ctx)
	return guard.err
}

type allowOperatorAuthority struct{ service *Service }

func (guard allowOperatorAuthority) ValidateOperatorAuthorityUnderFence(ctx context.Context, _ Head) error {
	if !guard.service.hasFenceMarker(ctx) {
		return errorCode("service_unavailable")
	}
	return nil
}

func (fence *operatorBarrierFence) Read(ctx context.Context) (func(), error) {
	return fence.delegate.Read(ctx)
}
func (fence *operatorBarrierFence) Write(ctx context.Context) (func(), error) {
	fence.once.Do(func() { close(fence.entered) })
	select {
	case <-fence.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return fence.delegate.Write(ctx)
}

const operatorEvidenceDigest = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

func operatorRecoveryInput(revision uint64, confirmationExpiry time.Time) OperatorRecoveryInput {
	return OperatorRecoveryInput{
		CommandID: "30000000-0000-4000-8000-000000000061", OperatorID: operatorTestID,
		FamilyID: operatorTestFamily, OldOwnerAccountID: operatorTestOwner, NewOwnerAccountID: operatorTestCaregiver,
		PolicyID: "Owner.Policy_test-v1", EvidenceDigest: operatorEvidenceDigest, ReasonCode: "owner_identity_unavailable",
		ExpectedRevision: revision, ConfirmationExpiresAt: confirmationExpiry,
	}
}

func operatorAuthorityFixture(t *testing.T, withEntitlement, freezeOwner bool) (*authorityFixture, time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	return operatorAuthorityFixtureAt(t, withEntitlement, freezeOwner, now), now
}

func operatorAuthorityFixtureAt(t *testing.T, withEntitlement, freezeOwner bool, now time.Time) *authorityFixture {
	t.Helper()
	h := newAuthority(t)
	h.clock = func() time.Time { return now }
	h.Service.now = h.clock
	owner := issueOperatorTestIdentity(t, h, operatorTestOwner, "1", now)
	caregiver := issueOperatorTestIdentity(t, h, operatorTestCaregiver, "2", now)
	command := func(id string, kind EventKind, payload any, accounts, families []string) Command {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return Command{ID: id, Kind: kind, AffectedAccountIDs: accounts, AffectedFamilyIDs: families, Payload: raw}
	}
	if withEntitlement {
		payload := FamilyBootstrappedPayload{Version: "v1", FamilyID: operatorTestFamily, OwnerAccountID: operatorTestOwner,
			EntitlementKey: "family_sync", EntitlementSource: "hosted_beta", ProjectionMaterialDigest: operatorEvidenceDigest}
		if _, err := h.Service.Execute(t.Context(), owner.principal, command("30000000-0000-4000-8000-000000000071", "family_bootstrapped", payload,
			[]string{operatorTestOwner}, []string{operatorTestFamily})); err != nil {
			t.Fatal(err)
		}
	} else {
		payload := FamilyCreatedPayload{Version: "v1", FamilyID: operatorTestFamily, OwnerAccountID: operatorTestOwner, ProjectionMaterialDigest: operatorEvidenceDigest}
		if _, err := h.Service.Execute(t.Context(), owner.principal, command("30000000-0000-4000-8000-000000000071", "family_created", payload,
			[]string{operatorTestOwner}, []string{operatorTestFamily})); err != nil {
			t.Fatal(err)
		}
	}
	invitation := InvitationPayload{Version: "v1", InvitationID: "80000000-0000-4000-8000-000000000001", FamilyID: operatorTestFamily,
		CreatorAccountID: operatorTestOwner, TokenDigest: "hmac-sha256:invitation.v1:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		Role: "caregiver", Status: "active", ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}
	if _, err := h.Service.Execute(t.Context(), owner.principal, command("30000000-0000-4000-8000-000000000072", "invitation_created", invitation,
		[]string{operatorTestOwner}, []string{operatorTestFamily})); err != nil {
		t.Fatal(err)
	}
	invitation.RedeemerAccountID = operatorTestCaregiver
	invitation.Status = "redeemed"
	if _, err := h.Service.Execute(t.Context(), caregiver.principal, command("30000000-0000-4000-8000-000000000073", "invitation_redeemed", invitation,
		[]string{operatorTestCaregiver}, []string{operatorTestFamily})); err != nil {
		t.Fatal(err)
	}
	if freezeOwner {
		freezeOperatorTestIdentity(t, h, operatorTestOwner, owner.identityKey, now, "30000000-0000-4000-8000-000000000074")
	}
	return h
}

func freezeOperatorTestIdentity(t *testing.T, h *authorityFixture, accountID, identityKey string, now time.Time, commandID string) {
	t.Helper()
	if _, err := h.Service.ExecutePrepared(t.Context(), func(_ context.Context, snapshot AuthoritySnapshot) (PreparedCommand, error) {
		actor, err := snapshot.ResolveIdentityActor([]string{identityKey}, accountID, 1)
		if err != nil {
			return PreparedCommand{}, err
		}
		return snapshot.PrepareIdentityUnavailable(actor, IdentityUnavailableCommandInput{
			Meta:    CommandMeta{CommandID: commandID, At: now},
			Payload: IdentityAuthorityPayload{Version: "v1", AccountID: accountID, IdentityKey: identityKey},
		})
	}); err != nil {
		t.Fatal(err)
	}
}

type issuedOperatorIdentity struct {
	principal   access.Principal
	identityKey string
}

func issueOperatorTestIdentity(t *testing.T, h *authorityFixture, accountID, suffix string, now time.Time) issuedOperatorIdentity {
	t.Helper()
	deviceID := "40000000-0000-4000-8000-00000000000" + suffix
	sessionID := "70000000-0000-4000-8000-00000000000" + suffix
	identityKey := "hmac-sha256:identity.v1:" + map[string]string{"1": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "2": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}[suffix]
	deviceDigest := "hmac-sha256:device.v1:" + map[string]string{"1": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "2": "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}[suffix]
	var result CommandResult
	result, err := h.Service.ExecutePrepared(t.Context(), func(_ context.Context, snapshot AuthoritySnapshot) (PreparedCommand, error) {
		actor, err := snapshot.ResolveIdentityActor([]string{identityKey}, accountID, 1)
		if err != nil {
			return PreparedCommand{}, err
		}
		return snapshot.PrepareIdentitySessionIssued(actor, IdentitySessionCommandInput{
			Meta: CommandMeta{CommandID: "30000000-0000-4000-8000-00000000008" + suffix, At: now},
			Payload: IdentitySessionIssuedPayload{Version: "v1", AccountID: accountID, IdentityKey: identityKey, RegisterAccount: true,
				DeviceID: deviceID, ServerInstanceID: "50000000-0000-4000-8000-000000000001", CanonicalOrigin: "https://operator.example",
				DeviceCredentialDigest: deviceDigest, RegisterDevice: true, SessionID: sessionID, LineageRootSessionID: sessionID,
				RecoveryEpoch: 1, ExpiresAt: now.Add(24 * time.Hour).Format(time.RFC3339), ProjectionMaterialDigest: operatorEvidenceDigest},
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return issuedOperatorIdentity{identityKey: identityKey, principal: access.Principal{AccountID: accountID, DeviceID: deviceID, SessionID: sessionID,
		RecoveryEpoch: 1, SecurityRevision: result.Revision, Scope: "account"}}
}

func mustParseOperatorTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
