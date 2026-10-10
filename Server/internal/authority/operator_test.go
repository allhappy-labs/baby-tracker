package authority_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
)

const operatorEvidence = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// familyRevision is the family's audit seq, an operator's ExpectedRevision.
func familyRevision(t *testing.T, f *authoritytest.Fixture, family string) uint64 {
	t.Helper()
	seq, _ := chainHead(t, f, authority.ScopeFamily, family)
	return seq
}

// recoveryFamily is a family whose owner's identity became unavailable, with
// an active caregiver and a viewer.
type recoveryFamily struct {
	family                   string
	owner, caregiver, viewer member
}

func newRecoveryFamily(t *testing.T, f *authoritytest.Fixture) recoveryFamily {
	t.Helper()
	owner, key := keyedMember(t, f)
	r := recoveryFamily{owner: owner}
	r.family = createFamily(t, f, f.Service, owner)
	r.caregiver = joined(t, f, f.Service, owner, r.family, "caregiver")
	r.viewer = joined(t, f, f.Service, owner, r.family, "viewer")
	if _, err := f.Service.MarkIdentityUnavailable(t.Context(), authority.IdentityUnavailable{CommandID: authoritytest.NewID(t), AccountID: owner.account,
		Candidates: []string{key}, At: f.Now}); err != nil {
		t.Fatal(err)
	}
	return r
}

func recoveryOf(t *testing.T, f *authoritytest.Fixture, r recoveryFamily, successor string) authority.OperatorRecoverOwner {
	t.Helper()
	return authority.OperatorRecoverOwner{CommandID: authoritytest.NewID(t), OperatorID: authoritytest.NewID(t), FamilyID: r.family,
		OldOwnerAccountID: r.owner.account, NewOwnerAccountID: successor, PolicyID: "ownership-policy-v1", EvidenceDigest: operatorEvidence,
		ReasonCode: "owner_identity_unavailable", ExpectedRevision: familyRevision(t, f, r.family),
		ConfirmationExpiresAt: f.Now.Add(5 * time.Minute).Truncate(time.Second)}
}

func TestOperatorRecoveryRequiresUnavailableOwnerAndCaregiverSuccessor(t *testing.T) {
	f := authoritytest.New(t)
	s := f.Service

	t.Run("owner still active", func(t *testing.T) {
		owner := newMember(t, f)
		family := createFamily(t, f, s, owner)
		caregiver := joined(t, f, s, owner, family, "caregiver")
		in := recoveryOf(t, f, recoveryFamily{family: family, owner: owner}, caregiver.account)
		wantCode(t, s.PreflightOperatorRecoverOwner(t.Context(), in), "owner_identity_unavailable")
		_, err := s.ExecuteOperatorRecoverOwner(t.Context(), in)
		wantCode(t, err, "owner_identity_unavailable")
	})

	r := newRecoveryFamily(t, f)
	before := familyRevision(t, f, r.family)
	outsider := newMember(t, f)
	refusals := map[string]struct {
		change func(*authority.OperatorRecoverOwner)
		code   string
	}{
		"viewer successor":     {func(in *authority.OperatorRecoverOwner) { in.NewOwnerAccountID = r.viewer.account }, "owner_identity_unavailable"},
		"non-member successor": {func(in *authority.OperatorRecoverOwner) { in.NewOwnerAccountID = outsider.account }, "owner_identity_unavailable"},
		"unknown successor":    {func(in *authority.OperatorRecoverOwner) { in.NewOwnerAccountID = authoritytest.NewID(t) }, "owner_identity_unavailable"},
		"wrong old owner":      {func(in *authority.OperatorRecoverOwner) { in.OldOwnerAccountID = r.viewer.account }, "owner_identity_unavailable"},
		"unknown family":       {func(in *authority.OperatorRecoverOwner) { in.FamilyID = authoritytest.NewID(t) }, "owner_identity_unavailable"},
		"stale revision":       {func(in *authority.OperatorRecoverOwner) { in.ExpectedRevision-- }, "stale_security_revision"},
		"future revision":      {func(in *authority.OperatorRecoverOwner) { in.ExpectedRevision++ }, "stale_security_revision"},
		"expired confirmation": {func(in *authority.OperatorRecoverOwner) { in.ConfirmationExpiresAt = f.Now.Truncate(time.Second) }, "invalid_confirmation"},
		"fractional confirmation": {func(in *authority.OperatorRecoverOwner) {
			in.ConfirmationExpiresAt = in.ConfirmationExpiresAt.Add(time.Millisecond)
		}, "invalid_confirmation"},
		"same owner":    {func(in *authority.OperatorRecoverOwner) { in.NewOwnerAccountID = in.OldOwnerAccountID }, "invalid_command"},
		"wrong reason":  {func(in *authority.OperatorRecoverOwner) { in.ReasonCode = "beta_extension" }, "invalid_command"},
		"bad evidence":  {func(in *authority.OperatorRecoverOwner) { in.EvidenceDigest = strings.ToUpper(operatorEvidence) }, "invalid_command"},
		"bad policy":    {func(in *authority.OperatorRecoverOwner) { in.PolicyID = "1policy" }, "invalid_command"},
		"zero revision": {func(in *authority.OperatorRecoverOwner) { in.ExpectedRevision = 0 }, "invalid_command"},
	}
	for name, refusal := range refusals {
		t.Run(name, func(t *testing.T) {
			in := recoveryOf(t, f, r, r.caregiver.account)
			refusal.change(&in)
			_, err := s.ExecuteOperatorRecoverOwner(t.Context(), in)
			wantCode(t, err, refusal.code)
			if refusal.code != "invalid_confirmation" {
				wantCode(t, s.PreflightOperatorRecoverOwner(t.Context(), in), refusal.code)
			}
		})
	}
	t.Run("successor identity unavailable", func(t *testing.T) {
		other, key := keyedMember(t, f)
		family := createFamily(t, f, s, other)
		successor, successorKey := keyedMember(t, f)
		in := invite(t, f, s, other, family, "caregiver")
		if _, err := s.RedeemInvitation(t.Context(), redeemOf(t, f, successor, in.TokenDigest)); err != nil {
			t.Fatal(err)
		}
		for _, k := range []struct{ account, key string }{{other.account, key}, {successor.account, successorKey}} {
			if _, err := s.MarkIdentityUnavailable(t.Context(), authority.IdentityUnavailable{CommandID: authoritytest.NewID(t), AccountID: k.account,
				Candidates: []string{k.key}, At: f.Now}); err != nil {
				t.Fatal(err)
			}
		}
		_, err := s.ExecuteOperatorRecoverOwner(t.Context(), recoveryOf(t, f, recoveryFamily{family: family, owner: other}, successor.account))
		wantCode(t, err, "owner_identity_unavailable")
	})
	if after := familyRevision(t, f, r.family); after != before {
		t.Fatalf("refusals moved the family chain %d → %d", before, after)
	}

	// The successor holds a live family session whose role follows.
	device := f.Device(t, r.caregiver.account)
	session, _, _ := f.Session(t, r.caregiver.account, device, "family", r.family, "caregiver")
	in := recoveryOf(t, f, r, r.caregiver.account)
	if err := s.PreflightOperatorRecoverOwner(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.ReplayOperatorRecoverOwner(t.Context(), in); err != nil || found {
		t.Fatalf("replay before commit: found=%v err=%v", found, err)
	}
	if after := familyRevision(t, f, r.family); after != before {
		t.Fatalf("preflight moved the family chain %d → %d", before, after)
	}
	result, err := s.ExecuteOperatorRecoverOwner(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if result.CommandID != in.CommandID || result.Outcome != "committed" || result.Code != "ok" || result.Revision != before+1 {
		t.Fatalf("result %+v", result)
	}
	if role, status := membershipOf(t, f, r.family, r.owner.account); role != "caregiver" || status != "active" {
		t.Fatalf("old owner membership %s/%s", role, status)
	}
	if role, status := membershipOf(t, f, r.family, r.caregiver.account); role != "owner" || status != "active" {
		t.Fatalf("new owner membership %s/%s", role, status)
	}
	if status, owner, _ := familyState(t, f, r.family); status != "active" || owner != r.caregiver.account {
		t.Fatalf("family %s owned by %s", status, owner)
	}
	if row, _ := sessionRow(t, f, session); row.Role != "owner" || row.Status != "active" {
		t.Fatalf("successor session %s/%s", row.Role, row.Status)
	}
	for _, scope := range []struct {
		kind authority.Scope
		id   string
	}{{authority.ScopeFamily, r.family}, {authority.ScopeAccount, r.owner.account}, {authority.ScopeAccount, r.caregiver.account}} {
		row := lastAudit(t, f, scope.kind, scope.id)
		payload := payloadOf(t, row)
		if row.Kind != "owner_recovered" || row.ActorID != in.OperatorID || row.CommandID != in.CommandID ||
			payload["operator_policy_id"] != in.PolicyID || payload["operator_evidence_digest"] != in.EvidenceDigest ||
			payload["operator_reason_code"] != in.ReasonCode || payload["expected_revision"] != float64(before) ||
			payload["old_owner_account_id"] != r.owner.account || payload["new_owner_account_id"] != r.caregiver.account {
			t.Fatalf("%s audit %s %s", scope.kind, row.Kind, row.Payload)
		}
	}

	// An exact retry returns the receipt hours later, past its confirmation,
	// through Execute and Replay alike; Preflight reports it as done.
	f.Now = f.Now.Add(2 * time.Hour)
	again, err := s.ExecuteOperatorRecoverOwner(t.Context(), in)
	if err != nil || again != result {
		t.Fatalf("exact retry %+v %v", again, err)
	}
	if replayed, found, err := s.ReplayOperatorRecoverOwner(t.Context(), in); err != nil || !found || replayed != result {
		t.Fatalf("replay %+v %v %v", replayed, found, err)
	}
	if err := s.PreflightOperatorRecoverOwner(t.Context(), in); err != nil {
		t.Fatalf("preflight of a committed command: %v", err)
	}
	changed := in
	changed.EvidenceDigest = strings.Repeat("f", 64)
	_, _, err = s.ReplayOperatorRecoverOwner(t.Context(), changed)
	wantCode(t, err, "command_id_reused")
	_, err = s.ExecuteOperatorRecoverOwner(t.Context(), changed)
	wantCode(t, err, "command_id_reused")
	if after := familyRevision(t, f, r.family); after != result.Revision {
		t.Fatalf("retries moved the family chain to %d", after)
	}
}

// entitlementOf is an operator entitlement command against family's current
// revision.
func entitlementOf(t *testing.T, f *authoritytest.Fixture, family, operation string, expires time.Time) authority.OperatorEntitlement {
	t.Helper()
	reason := "beta_extension"
	if operation == "suspend" {
		reason = "abuse_suspension"
	}
	return authority.OperatorEntitlement{CommandID: authoritytest.NewID(t), OperatorID: authoritytest.NewID(t), FamilyID: family, Operation: operation,
		Key: "family_sync", Source: "hosted_beta", ExpiresAt: expires, PolicyID: "entitlement-policy-v1", EvidenceDigest: operatorEvidence,
		ReasonCode: reason, ExpectedRevision: familyRevision(t, f, family), ConfirmationExpiresAt: f.Now.Add(5 * time.Minute).Truncate(time.Second)}
}

func entitlementState(t *testing.T, f *authoritytest.Fixture, family string) (status, source string, expires *time.Time) {
	t.Helper()
	inAuthorityTxQuery(t, f, `SELECT status::text, source, expires_at FROM family_entitlements WHERE family_id=$1 AND entitlement_key='family_sync'`,
		[]any{family}, &status, &source, &expires)
	return status, source, expires
}

func TestOperatorEntitlementSuspendRequiresExactExpiry(t *testing.T) {
	f := authoritytest.New(t)
	f.Now = time.Date(2026, 10, 4, 12, 0, 0, 123456789, time.UTC)
	hosted := serviceWith(t, f, func(o *authority.Options) { o.RequireFamilySyncEntitlement = true })
	owner := newMember(t, f)
	family := createFamily(t, f, hosted, owner)
	_, _, current := entitlementState(t, f, family)
	bootstrap := f.Now.Truncate(time.Second).Add(366 * 24 * time.Hour)
	if current == nil || !current.Equal(bootstrap) {
		t.Fatalf("bootstrap expiry %v, want %v", current, bootstrap)
	}
	s := f.Service

	for name, change := range map[string]func(*authority.OperatorEntitlement){
		"expiry one second later":   func(in *authority.OperatorEntitlement) { in.ExpiresAt = in.ExpiresAt.Add(time.Second) },
		"expiry one second earlier": func(in *authority.OperatorEntitlement) { in.ExpiresAt = in.ExpiresAt.Add(-time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			in := entitlementOf(t, f, family, "suspend", bootstrap)
			change(&in)
			wantCode(t, s.PreflightOperatorEntitlement(t.Context(), in), "invalid_command")
			_, err := s.ExecuteOperatorEntitlement(t.Context(), in)
			wantCode(t, err, "invalid_command")
		})
	}
	for name, refusal := range map[string]struct {
		change func(*authority.OperatorEntitlement)
		code   string
	}{
		"changed source":    {func(in *authority.OperatorEntitlement) { in.Source = "operator" }, "invalid_command"},
		"changed key":       {func(in *authority.OperatorEntitlement) { in.Key = "family_export" }, "invalid_command"},
		"fractional expiry": {func(in *authority.OperatorEntitlement) { in.ExpiresAt = in.ExpiresAt.Add(time.Millisecond) }, "invalid_command"},
		"local expiry":      {func(in *authority.OperatorEntitlement) { in.ExpiresAt = in.ExpiresAt.In(time.FixedZone("x", 3600)) }, "invalid_command"},
		"grant reason":      {func(in *authority.OperatorEntitlement) { in.ReasonCode = "beta_extension" }, "invalid_command"},
		"unknown operation": {func(in *authority.OperatorEntitlement) { in.Operation = "revoke" }, "invalid_command"},
		"stale revision":    {func(in *authority.OperatorEntitlement) { in.ExpectedRevision++ }, "stale_security_revision"},
		"unknown family":    {func(in *authority.OperatorEntitlement) { in.FamilyID = authoritytest.NewID(t) }, "family_unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			in := entitlementOf(t, f, family, "suspend", bootstrap)
			refusal.change(&in)
			_, err := s.ExecuteOperatorEntitlement(t.Context(), in)
			wantCode(t, err, refusal.code)
		})
	}

	// Grants extend an active hosted_beta entitlement only, to a later expiry
	// after the command time.
	for name, expires := range map[string]time.Time{
		"not after current": bootstrap,
		"before current":    bootstrap.Add(-time.Hour),
	} {
		t.Run("grant "+name, func(t *testing.T) {
			_, err := s.ExecuteOperatorEntitlement(t.Context(), entitlementOf(t, f, family, "grant", expires))
			wantCode(t, err, "invalid_command")
		})
	}
	extended := bootstrap.Add(30 * 24 * time.Hour)
	grant := entitlementOf(t, f, family, "grant", extended)
	if err := s.PreflightOperatorEntitlement(t.Context(), grant); err != nil {
		t.Fatal(err)
	}
	granted, err := s.ExecuteOperatorEntitlement(t.Context(), grant)
	if err != nil {
		t.Fatal(err)
	}
	if status, source, expires := entitlementState(t, f, family); status != "active" || source != "hosted_beta" || expires == nil || !expires.Equal(extended) {
		t.Fatalf("granted entitlement %s/%s/%v", status, source, expires)
	}
	if row := lastAudit(t, f, authority.ScopeFamily, family); row.Kind != "operator_entitlement_granted" || row.ActorID != grant.OperatorID ||
		payloadOf(t, row)["expires_at"] != extended.Format(time.RFC3339) || granted.Revision != row.Seq {
		t.Fatalf("grant audit %s %s (result %+v)", row.Kind, row.Payload, granted)
	}

	// Suspension names the exact current expiry: the old one no longer works.
	_, err = s.ExecuteOperatorEntitlement(t.Context(), entitlementOf(t, f, family, "suspend", bootstrap))
	wantCode(t, err, "invalid_command")
	suspend := entitlementOf(t, f, family, "suspend", extended)
	suspended, err := s.ExecuteOperatorEntitlement(t.Context(), suspend)
	if err != nil {
		t.Fatal(err)
	}
	if status, source, expires := entitlementState(t, f, family); status != "suspended" || source != "hosted_beta" || expires == nil || !expires.Equal(extended) {
		t.Fatalf("suspended entitlement %s/%s/%v", status, source, expires)
	}
	if row := lastAudit(t, f, authority.ScopeFamily, family); row.Kind != "operator_entitlement_suspended" || suspended.Revision != row.Seq {
		t.Fatalf("suspend audit %s", row.Kind)
	}
	// The family's members are now denied; a grant cannot revive a suspension;
	// a second suspension is refused; the exact retry still returns the receipt.
	_, err = hosted.CreateInvitation(t.Context(), invitationOf(t, f, owner, family, "caregiver"))
	wantCode(t, err, "entitlement_required")
	_, err = s.ExecuteOperatorEntitlement(t.Context(), entitlementOf(t, f, family, "grant", extended.Add(time.Hour)))
	wantCode(t, err, "invalid_command")
	_, err = s.ExecuteOperatorEntitlement(t.Context(), entitlementOf(t, f, family, "suspend", extended))
	wantCode(t, err, "invalid_command")
	f.Now = f.Now.Add(3 * time.Hour)
	if again, err := s.ExecuteOperatorEntitlement(t.Context(), suspend); err != nil || again != suspended {
		t.Fatalf("exact retry %+v %v", again, err)
	}
	changed := suspend
	changed.ExpiresAt = changed.ExpiresAt.Add(time.Second)
	_, _, err = s.ReplayOperatorEntitlement(t.Context(), changed)
	wantCode(t, err, "command_id_reused")

	t.Run("first grant on a family without an entitlement", func(t *testing.T) {
		owner := newMember(t, f)
		family := createFamily(t, f, s, owner)
		_, err := s.ExecuteOperatorEntitlement(t.Context(), entitlementOf(t, f, family, "suspend", f.Now.Add(time.Hour).Truncate(time.Second)))
		wantCode(t, err, "invalid_command")
		_, err = s.ExecuteOperatorEntitlement(t.Context(), entitlementOf(t, f, family, "grant", f.Now.Truncate(time.Second)))
		wantCode(t, err, "invalid_command")
		expires := f.Now.Add(24 * time.Hour).Truncate(time.Second)
		if _, err := s.ExecuteOperatorEntitlement(t.Context(), entitlementOf(t, f, family, "grant", expires)); err != nil {
			t.Fatal(err)
		}
		if status, source, got := entitlementState(t, f, family); status != "active" || source != "hosted_beta" || got == nil || !got.Equal(expires) {
			t.Fatalf("first grant %s/%s/%v", status, source, got)
		}
	})
	t.Run("inactive family", func(t *testing.T) {
		owner := newMember(t, f)
		family := createFamily(t, f, s, owner)
		if _, err := s.ScheduleDeletion(t.Context(), scheduleOf(t, f, owner.account, owner.principal(t, f))); err != nil {
			t.Fatal(err)
		}
		_, err := s.ExecuteOperatorEntitlement(t.Context(), entitlementOf(t, f, family, "grant", f.Now.Add(24*time.Hour).Truncate(time.Second)))
		wantCode(t, err, "family_unavailable")
	})
}

// The successor's identity loss racing its operator recovery: recovery holds
// the successor's account lock (operator.go recoverOwnerSpec), so either it
// sees the successor active and commits before the identity loss, or it sees
// the loss and refuses. Recovery never installs an owner whose identity was
// already unavailable, and both outcomes carry clean codes.
func TestOperatorRecoveryRacingSuccessorIdentityLoss(t *testing.T) {
	f := authoritytest.New(t)
	s := f.Service
	outcomes := map[string]int{}
	for round := range 24 {
		owner, ownerKey := keyedMember(t, f)
		family := createFamily(t, f, s, owner)
		successor, successorKey := keyedMember(t, f)
		invitation := invite(t, f, s, owner, family, "caregiver")
		if _, err := s.RedeemInvitation(t.Context(), redeemOf(t, f, successor, invitation.TokenDigest)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MarkIdentityUnavailable(t.Context(), authority.IdentityUnavailable{CommandID: authoritytest.NewID(t), AccountID: owner.account,
			Candidates: []string{ownerKey}, At: f.Now}); err != nil {
			t.Fatal(err)
		}
		recovery := recoveryOf(t, f, recoveryFamily{family: family, owner: owner}, successor.account)
		loss := authority.IdentityUnavailable{CommandID: authoritytest.NewID(t), AccountID: successor.account, Candidates: []string{successorKey}, At: f.Now}
		start := make(chan struct{})
		var recoveryErr, lossErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if round%4 >= 2 {
				time.Sleep(time.Duration(round%4-1) * time.Millisecond)
			}
			_, recoveryErr = s.ExecuteOperatorRecoverOwner(context.Background(), recovery)
		}()
		go func() {
			defer wg.Done()
			<-start
			if round%4 == 1 {
				time.Sleep(time.Millisecond)
			}
			_, lossErr = s.MarkIdentityUnavailable(context.Background(), loss)
		}()
		close(start)
		wg.Wait()
		if lossErr != nil {
			t.Fatalf("round %d: identity loss: %v", round, lossErr)
		}
		// The successor's chain orders the two commits: both append to it
		// under its account lock.
		var recoveredSeq, lostSeq uint64
		for _, row := range readAudit(t, f, authority.ScopeAccount, successor.account) {
			switch row.Kind {
			case "owner_recovered":
				recoveredSeq = row.Seq
			case "identity_unavailable":
				lostSeq = row.Seq
			}
		}
		_, familyOwner, _ := familyState(t, f, family)
		switch code := authority.CodeOf(recoveryErr); code {
		case "":
			if recoveredSeq == 0 || lostSeq <= recoveredSeq || familyOwner != successor.account {
				t.Fatalf("round %d: recovery committed at seq %d, identity loss at %d, owner %s", round, recoveredSeq, lostSeq, familyOwner)
			}
			outcomes["recovered_then_lost"]++
		case "owner_identity_unavailable":
			if recoveredSeq != 0 || familyOwner != owner.account {
				t.Fatalf("round %d: refused recovery left seq %d, owner %s", round, recoveredSeq, familyOwner)
			}
			if role, status := membershipOf(t, f, family, successor.account); role != "caregiver" || status != "active" {
				t.Fatalf("round %d: refused recovery changed the successor to %s/%s", round, role, status)
			}
			outcomes["lost_then_refused"]++
		default:
			t.Fatalf("round %d: recovery %v (code %q)", round, recoveryErr, code)
		}
	}
	t.Logf("outcomes %v", outcomes)
	if outcomes["recovered_then_lost"] == 0 || outcomes["lost_then_refused"] == 0 {
		t.Fatalf("both interleavings must occur: %v", outcomes)
	}
}
