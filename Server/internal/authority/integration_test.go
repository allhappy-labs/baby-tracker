package authority_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
)

const integrationValidity = 7776000 * time.Second

func newVerifier(t *testing.T) string {
	t.Helper()
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw[:])
}

// grantOf is an operator issuance of a family:read integration to owner.
func grantOf(t *testing.T, f *authoritytest.Fixture, family, owner string) authority.OperatorIntegration {
	t.Helper()
	return authority.OperatorIntegration{CommandID: authoritytest.NewID(t), OperatorID: authoritytest.NewID(t), Operation: "grant",
		GrantID: authoritytest.NewID(t), FamilyID: family, AccountID: owner, Name: "Claude Desktop", Scope: "family:read", Verifier: newVerifier(t),
		ServerInstanceID: f.InstanceID, RecoveryEpoch: authoritytest.RecoveryEpoch, ValiditySeconds: 7776000, PolicyID: "integration-policy-v1",
		EvidenceDigest: operatorEvidence, ReasonCode: "integration_issued", ExpectedRevision: familyRevision(t, f, family),
		ConfirmationExpiresAt: f.Now.Add(5 * time.Minute).Truncate(time.Second)}
}

// revokeOf revokes grant's integration.
func revokeOf(t *testing.T, f *authoritytest.Fixture, grant authority.OperatorIntegration) authority.OperatorIntegration {
	t.Helper()
	return authority.OperatorIntegration{CommandID: authoritytest.NewID(t), OperatorID: grant.OperatorID, Operation: "revoke", GrantID: grant.GrantID,
		FamilyID: grant.FamilyID, AccountID: grant.AccountID, ServerInstanceID: grant.ServerInstanceID, RecoveryEpoch: grant.RecoveryEpoch,
		PolicyID: grant.PolicyID, EvidenceDigest: operatorEvidence, ReasonCode: "integration_revoked", ExpectedRevision: familyRevision(t, f, grant.FamilyID),
		ConfirmationExpiresAt: f.Now.Add(5 * time.Minute).Truncate(time.Second)}
}

type integrationRow struct {
	status           string
	created, expires time.Time
	revokedAt        *time.Time
	verifier, name   string
	account, family  string
	epoch            uint64
}

func integrationOf(t *testing.T, f *authoritytest.Fixture, id string) integrationRow {
	t.Helper()
	var row integrationRow
	inAuthorityTxQuery(t, f, `SELECT status::text, created_at, expires_at, revoked_at, encode(verifier,'hex'), name, account_id::text, family_id::text, recovery_epoch
		FROM mcp_integrations WHERE id=$1`, []any{id}, &row.status, &row.created, &row.expires, &row.revokedAt, &row.verifier, &row.name, &row.account, &row.family, &row.epoch)
	return row
}

func TestIntegrationExactRetryKeepsIssuanceTime(t *testing.T) {
	f := authoritytest.New(t)
	f.Now = time.Date(2026, 10, 4, 12, 0, 0, 987654321, time.UTC)
	s := f.Service
	owner := newMember(t, f)
	family := createFamily(t, f, s, owner)
	in := grantOf(t, f, family, owner.account)
	if err := s.PreflightOperatorIntegration(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if n := count(t, f, `SELECT count(*) FROM mcp_integrations`); n != 0 {
		t.Fatalf("preflight wrote %d grants", n)
	}
	issuedAt := f.Now.Truncate(time.Second)
	result, err := s.ExecuteOperatorIntegration(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	row := integrationOf(t, f, in.GrantID)
	if row.status != "active" || !row.created.Equal(issuedAt) || !row.expires.Equal(issuedAt.Add(integrationValidity)) || row.verifier != in.Verifier ||
		row.name != in.Name || row.account != owner.account || row.family != family || row.epoch != authoritytest.RecoveryEpoch {
		t.Fatalf("issued %+v, want created %v", row, issuedAt)
	}
	for _, scope := range []struct {
		kind authority.Scope
		id   string
	}{{authority.ScopeFamily, family}, {authority.ScopeAccount, owner.account}} {
		audit := lastAudit(t, f, scope.kind, scope.id)
		payload := payloadOf(t, audit)
		if audit.Kind != "integration_granted" || audit.ActorID != in.OperatorID || !audit.At.Equal(issuedAt) ||
			payload["integration_id"] != in.GrantID || payload["created_at"] != issuedAt.Format(time.RFC3339) ||
			payload["expires_at"] != issuedAt.Add(integrationValidity).Format(time.RFC3339) || payload["verifier"] != nil {
			t.Fatalf("%s audit %s at %v %s", scope.kind, audit.Kind, audit.At, audit.Payload)
		}
	}
	if result.Revision != familyRevision(t, f, family) {
		t.Fatalf("result revision %d", result.Revision)
	}

	// Two hours later the lost-reply retry returns the original receipt and
	// neither restamps nor re-checks the (now expired) confirmation.
	f.Now = f.Now.Add(2 * time.Hour)
	again, err := s.ExecuteOperatorIntegration(t.Context(), in)
	if err != nil || again != result {
		t.Fatalf("retry %+v %v, want %+v", again, err, result)
	}
	if replayed, found, err := s.ReplayOperatorIntegration(t.Context(), in); err != nil || !found || replayed != result {
		t.Fatalf("replay %+v %v %v", replayed, found, err)
	}
	if later := integrationOf(t, f, in.GrantID); !later.created.Equal(row.created) || !later.expires.Equal(row.expires) {
		t.Fatalf("retry restamped %v/%v → %v/%v", row.created, row.expires, later.created, later.expires)
	}
	changed := in
	changed.Name = "Other Client"
	_, err = s.ExecuteOperatorIntegration(t.Context(), changed)
	wantCode(t, err, "command_id_reused")
	_, _, err = s.ReplayOperatorIntegration(t.Context(), changed)
	wantCode(t, err, "command_id_reused")
	wantCode(t, s.PreflightOperatorIntegration(t.Context(), changed), "command_id_reused")

	// After revocation, and past expiry, the grant retry still returns the
	// receipt and never reactivates the grant.
	revoke := revokeOf(t, f, in)
	revoked, err := s.ExecuteOperatorIntegration(t.Context(), revoke)
	if err != nil {
		t.Fatal(err)
	}
	if audit := lastAudit(t, f, authority.ScopeFamily, family); audit.Kind != "integration_revoked" || revoked.Revision != audit.Seq {
		t.Fatalf("revoke audit %s", audit.Kind)
	}
	f.Now = issuedAt.Add(integrationValidity + time.Hour)
	if again, err := s.ExecuteOperatorIntegration(t.Context(), in); err != nil || again != result {
		t.Fatalf("retry after revocation %+v %v", again, err)
	}
	if again, err := s.ExecuteOperatorIntegration(t.Context(), revoke); err != nil || again != revoked {
		t.Fatalf("revoke retry %+v %v", again, err)
	}
	final := integrationOf(t, f, in.GrantID)
	if final.status != "revoked" || final.revokedAt == nil || !final.created.Equal(row.created) || !final.expires.Equal(row.expires) {
		t.Fatalf("after retries %+v", final)
	}
	second := revokeOf(t, f, in)
	_, err = s.ExecuteOperatorIntegration(t.Context(), second)
	wantCode(t, err, "integration_revoked")

	views, err := s.ListFamilyIntegrations(t.Context(), family)
	if err != nil || len(views) != 1 {
		t.Fatalf("list %+v %v", views, err)
	}
	if v := views[0]; v.ID != in.GrantID || v.AccountID != owner.account || v.FamilyID != family || v.ServerInstanceID != f.InstanceID || v.Name != in.Name ||
		v.Scope != "family:read" || v.Verifier != in.Verifier || v.Status != "revoked" || v.RecoveryEpoch != authoritytest.RecoveryEpoch ||
		!v.CreatedAt.Equal(issuedAt) || !v.ExpiresAt.Equal(issuedAt.Add(integrationValidity)) {
		t.Fatalf("view %+v", v)
	}
}

func TestIntegrationCommitTimeFollowsLockAcquisition(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	f.Now = time.Now().UTC()
	in := grantOf(t, f, family, owner.account)
	in.ConfirmationExpiresAt = time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	live := serviceWith(t, f, func(o *authority.Options) { o.Now = time.Now })

	holder, err := f.Service.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.Background())
	if _, err := holder.Exec(t.Context(), `SELECT 1 FROM families WHERE id=$1 FOR UPDATE`, family); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := live.ExecuteOperatorIntegration(context.Background(), in)
		done <- err
	}()
	time.Sleep(1500 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("issuance did not wait for the family lock: %v", err)
	default:
	}
	released := time.Now()
	if err := holder.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	row := integrationOf(t, f, in.GrantID)
	if row.created.Before(released.Truncate(time.Second)) || !row.created.After(started.Truncate(time.Second)) {
		t.Fatalf("created_at %v; lock taken at %v, released at %v", row.created, started, released)
	}
	if !row.expires.Equal(row.created.Add(integrationValidity)) {
		t.Fatalf("expires %v for created %v", row.expires, row.created)
	}
}

func TestIntegrationRejectsReusedVerifierAndStaleRevision(t *testing.T) {
	f := authoritytest.New(t)
	s := f.Service
	owner := newMember(t, f)
	family := createFamily(t, f, s, owner)
	caregiver := joined(t, f, s, owner, family, "caregiver")
	first := grantOf(t, f, family, owner.account)
	if _, err := s.ExecuteOperatorIntegration(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	other := newMember(t, f)
	otherFamily := createFamily(t, f, s, other)

	refusals := map[string]struct {
		build func() authority.OperatorIntegration
		code  string
	}{
		"verifier reused in the family": {func() authority.OperatorIntegration {
			in := grantOf(t, f, family, owner.account)
			in.Verifier = first.Verifier
			return in
		}, "integration_verifier_reused"},
		"verifier reused across families": {func() authority.OperatorIntegration {
			in := grantOf(t, f, otherFamily, other.account)
			in.Verifier = first.Verifier
			return in
		}, "integration_verifier_reused"},
		"grant ID reused": {func() authority.OperatorIntegration {
			in := grantOf(t, f, family, owner.account)
			in.GrantID = first.GrantID
			return in
		}, "invalid_command"},
		"stale revision": {func() authority.OperatorIntegration {
			in := grantOf(t, f, family, owner.account)
			in.ExpectedRevision--
			return in
		}, "stale_security_revision"},
		"other family's revision": {func() authority.OperatorIntegration {
			in := grantOf(t, f, family, owner.account)
			in.ExpectedRevision = familyRevision(t, f, otherFamily) + 7
			return in
		}, "stale_security_revision"},
		"other epoch": {func() authority.OperatorIntegration {
			in := grantOf(t, f, family, owner.account)
			in.RecoveryEpoch++
			return in
		}, "epoch_changed"},
		"other instance": {func() authority.OperatorIntegration {
			in := grantOf(t, f, family, owner.account)
			in.ServerInstanceID = authoritytest.NewID(t)
			return in
		}, "invalid_command"},
		"caregiver account": {func() authority.OperatorIntegration { return grantOf(t, f, family, caregiver.account) }, "owner_required"},
		"owner of another family": {func() authority.OperatorIntegration {
			in := grantOf(t, f, family, other.account)
			return in
		}, "owner_required"},
		"unknown family": {func() authority.OperatorIntegration {
			in := grantOf(t, f, family, owner.account)
			in.FamilyID = authoritytest.NewID(t)
			return in
		}, "family_unavailable"},
		"expired confirmation": {func() authority.OperatorIntegration {
			in := grantOf(t, f, family, owner.account)
			in.ConfirmationExpiresAt = f.Now.Truncate(time.Second)
			return in
		}, "invalid_confirmation"},
		"other validity": {func() authority.OperatorIntegration {
			in := grantOf(t, f, family, owner.account)
			in.ValiditySeconds = 86400
			return in
		}, "invalid_command"},
		"bad name": {func() authority.OperatorIntegration {
			in := grantOf(t, f, family, owner.account)
			in.Name = " padded"
			return in
		}, "invalid_command"},
		"other scope": {func() authority.OperatorIntegration {
			in := grantOf(t, f, family, owner.account)
			in.Scope = "family:write"
			return in
		}, "invalid_command"},
		"uppercase verifier": {func() authority.OperatorIntegration {
			in := grantOf(t, f, family, owner.account)
			in.Verifier = "ABCDEF" + in.Verifier[6:]
			return in
		}, "invalid_command"},
		"revoke reason on a grant": {func() authority.OperatorIntegration {
			in := grantOf(t, f, family, owner.account)
			in.ReasonCode = "integration_revoked"
			return in
		}, "invalid_command"},
	}
	before := count(t, f, `SELECT count(*) FROM mcp_integrations`)
	for name, refusal := range refusals {
		t.Run(name, func(t *testing.T) {
			in := refusal.build()
			_, err := s.ExecuteOperatorIntegration(t.Context(), in)
			wantCode(t, err, refusal.code)
			if refusal.code != "invalid_confirmation" {
				wantCode(t, s.PreflightOperatorIntegration(t.Context(), in), refusal.code)
			}
		})
	}
	if after := count(t, f, `SELECT count(*) FROM mcp_integrations`); after != before {
		t.Fatalf("refusals wrote %d grants", after-before)
	}

	t.Run("revocation names the stored grant", func(t *testing.T) {
		for name, change := range map[string]func(*authority.OperatorIntegration){
			"unknown grant":  func(in *authority.OperatorIntegration) { in.GrantID = authoritytest.NewID(t) },
			"other account":  func(in *authority.OperatorIntegration) { in.AccountID = caregiver.account },
			"other epoch":    func(in *authority.OperatorIntegration) { in.RecoveryEpoch++ },
			"other instance": func(in *authority.OperatorIntegration) { in.ServerInstanceID = authoritytest.NewID(t) },
			"other family": func(in *authority.OperatorIntegration) {
				in.FamilyID, in.ExpectedRevision = otherFamily, familyRevision(t, f, otherFamily)
			},
		} {
			t.Run(name, func(t *testing.T) {
				in := revokeOf(t, f, first)
				change(&in)
				_, err := s.ExecuteOperatorIntegration(t.Context(), in)
				wantCode(t, err, "integration_invalid")
			})
		}
		stale := revokeOf(t, f, first)
		stale.ExpectedRevision--
		_, err := s.ExecuteOperatorIntegration(t.Context(), stale)
		wantCode(t, err, "stale_security_revision")
		withMetadata := revokeOf(t, f, first)
		withMetadata.Name = first.Name
		_, err = s.ExecuteOperatorIntegration(t.Context(), withMetadata)
		wantCode(t, err, "invalid_command")
		if row := integrationOf(t, f, first.GrantID); row.status != "active" {
			t.Fatalf("refused revocations changed the grant to %s", row.status)
		}
	})

	// Concurrent issuances of one verifier into two families do not share a
	// lock: only the unique verifier constraint decides.
	t.Run("concurrent verifier reuse", func(t *testing.T) {
		for round := range 5 {
			a, b := newMember(t, f), newMember(t, f)
			familyA, familyB := createFamily(t, f, s, a), createFamily(t, f, s, b)
			verifier := newVerifier(t)
			ins := []authority.OperatorIntegration{grantOf(t, f, familyA, a.account), grantOf(t, f, familyB, b.account)}
			start := make(chan struct{})
			errs := make([]error, 2)
			var wg sync.WaitGroup
			for i := range ins {
				ins[i].Verifier = verifier
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, errs[i] = s.ExecuteOperatorIntegration(context.Background(), ins[i])
				}()
			}
			close(start)
			wg.Wait()
			codes := map[string]int{}
			for _, err := range errs {
				codes[authority.CodeOf(err)]++
			}
			if codes[""] != 1 || codes["integration_verifier_reused"] != 1 {
				t.Fatalf("round %d: outcomes %v", round, codes)
			}
			if n := count(t, f, `SELECT count(*) FROM mcp_integrations WHERE verifier=decode($1,'hex')`, verifier); n != 1 {
				t.Fatalf("round %d: %d grants hold the verifier", round, n)
			}
		}
	})
}

func TestIntegrationGrantIsNotRevokedByOwnershipChanges(t *testing.T) {
	f := authoritytest.New(t)
	s := f.Service
	owner := newMember(t, f)
	family := createFamily(t, f, s, owner)
	caregiver := joined(t, f, s, owner, family, "caregiver")
	in := grantOf(t, f, family, owner.account)
	if _, err := s.ExecuteOperatorIntegration(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransferOwner(t.Context(), authority.TransferOwner{CommandID: authoritytest.NewID(t), OldOwnerID: owner.account, NewOwnerID: caregiver.account,
		At: f.Now, Principal: owner.principal(t, f)}); err != nil {
		t.Fatal(err)
	}
	// The read guard rechecks the live owner; the row is left as issued, and
	// a new grant to the former owner is refused.
	if row := integrationOf(t, f, in.GrantID); row.status != "active" {
		t.Fatalf("transfer revoked the grant: %s", row.status)
	}
	_, err := s.ExecuteOperatorIntegration(t.Context(), grantOf(t, f, family, owner.account))
	wantCode(t, err, "owner_required")
	views, err := s.ListFamilyIntegrations(t.Context(), family)
	if err != nil || len(views) != 1 || views[0].Status != "active" {
		t.Fatalf("list %+v %v", views, err)
	}
	if views, err := s.ListFamilyIntegrations(t.Context(), authoritytest.NewID(t)); err != nil || len(views) != 0 || views == nil {
		t.Fatalf("unknown family list %#v %v", views, err)
	}
	_, err = s.ListFamilyIntegrations(t.Context(), "not-a-uuid")
	wantCode(t, err, "invalid_command")
}
