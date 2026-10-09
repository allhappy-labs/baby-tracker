//go:build identitytest

package protocol_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"babytracker/server/internal/protocol"
)

// The end-to-end proof that a read-only caregiver is refused by the SERVER
// rather than merely by the client's hidden buttons.
//
// These ran as skips for as long as the role had no administration: the schema
// carried it, the write gate refused it and the client rendered it, but no
// command handed it out, so a viewer could not be created over the wire and
// there was nothing to point a protocol test at. `member_role_changed` closed
// that; these tests are what makes it true rather than merely present.
//
// What the package tests already cover, without a database:
//   - internal/access.RoleMayWrite — the single write authority, failing closed
//     on unrecognised roles.
//   - internal/family.allowed — a viewer may read and may do nothing else.
//   - BabyStore's WritePermissionTests — every client write path refused, the
//     local realm unaffected, the gate durable across relaunch.
//
// What only these can establish: that a refused mutation writes no journal row
// and does not advance the family cursor. Both are properties of the database
// after the refusal, not of the code that refuses.

func TestViewerMutationsRejectedAndCursorDoesNotAdvance(t *testing.T) {
	fixture := newProtocolFixture(t)

	// A baby first, so there is something to read and something a diaper could
	// plausibly be attached to. Written by the owner, who may.
	if _, err := fixture.Owner().Sync([]protocol.Mutation{mutation(t, "baby.json", fixture.h.CommandID())}); err != nil {
		t.Fatal(err)
	}
	// The caregiver pulls it while they can still write, so the refusal below
	// cannot be mistaken for an account that never had access at all.
	if pulled, err := fixture.Caregiver().Sync(nil); err != nil || len(pulled.Changes) != 1 {
		t.Fatalf("caregiver baseline pull=%+v err=%v", pulled, err)
	}

	if _, err := fixture.h.Family.ChangeRole(
		t.Context(), fixture.h.Owner, fixture.h.CommandID(), fixture.h.Caregiver.AccountID, "viewer",
	); err != nil {
		t.Fatalf("owner could not make the caregiver a viewer: %v", err)
	}

	sequenceBefore := fixture.h.FamilySequence(t)
	journalBefore := fixture.h.Count(t, "mutation_journal", fixture.h.FamilyID)
	diapersBefore := fixture.h.Count(t, "diaper_entries", fixture.h.FamilyID)

	_, err := fixture.Caregiver().Sync([]protocol.Mutation{mutation(t, "diaper.json", fixture.h.CommandID())})
	detail := refusal(t, err)
	if detail.Code != "read_only_caregiver" {
		t.Fatalf("viewer mutation refused as %q, want read_only_caregiver", detail.Code)
	}
	// Not retryable. The role has to change before this can ever succeed, and a
	// client that keeps retrying is spending battery to be told the same thing.
	if detail.Retryable {
		t.Error("read_only_caregiver was advertised as retryable")
	}

	if after := fixture.h.FamilySequence(t); after != sequenceBefore {
		t.Errorf("family cursor advanced on a refused mutation: %d -> %d", sequenceBefore, after)
	}
	if after := fixture.h.Count(t, "mutation_journal", fixture.h.FamilyID); after != journalBefore {
		t.Errorf("refused mutation left %d journal rows behind", after-journalBefore)
	}
	if after := fixture.h.Count(t, "diaper_entries", fixture.h.FamilyID); after != diapersBefore {
		t.Errorf("refused mutation wrote a diaper entry")
	}

	// Reading is the whole point of a viewer, so losing it would be a worse
	// defect than the one this test is about.
	if _, err := fixture.Caregiver().Sync(nil); err != nil {
		t.Fatalf("viewer lost read access: %v", err)
	}
	if _, err := fixture.Caregiver().Snapshot(); err != nil {
		t.Fatalf("viewer lost snapshot access: %v", err)
	}
}

func TestOwnerChangesRoleBetweenCaregiverAndViewer(t *testing.T) {
	fixture := newProtocolFixture(t)
	if _, err := fixture.Owner().Sync([]protocol.Mutation{mutation(t, "baby.json", fixture.h.CommandID())}); err != nil {
		t.Fatal(err)
	}

	// Writing works before the demotion. Asserted rather than assumed: a test
	// that only ever saw the refusal would pass just as well against a client
	// that could never write at all.
	if applied, err := fixture.Caregiver().Sync([]protocol.Mutation{mutation(t, "sleep.json", fixture.h.CommandID())}); err != nil || len(applied.Outcomes) != 1 {
		t.Fatalf("caregiver write before demotion: %+v err=%v", applied, err)
	}

	if _, err := fixture.h.Family.ChangeRole(t.Context(), fixture.h.Owner, fixture.h.CommandID(), fixture.h.Caregiver.AccountID, "viewer"); err != nil {
		t.Fatalf("demote: %v", err)
	}
	if role := memberRole(t, fixture, fixture.h.Caregiver.AccountID); role != "viewer" {
		t.Fatalf("family view still reports role %q after demotion", role)
	}
	_, err := fixture.Caregiver().Sync([]protocol.Mutation{mutation(t, "diaper.json", fixture.h.CommandID())})
	if detail := refusal(t, err); detail.Code != "read_only_caregiver" {
		t.Fatalf("demoted caregiver refused as %q", detail.Code)
	}

	// And back. A demotion a family cannot undo is a trap rather than a
	// setting, and the restored role has to reach the running session without
	// the caregiver signing in again.
	if _, err := fixture.h.Family.ChangeRole(t.Context(), fixture.h.Owner, fixture.h.CommandID(), fixture.h.Caregiver.AccountID, "caregiver"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if role := memberRole(t, fixture, fixture.h.Caregiver.AccountID); role != "caregiver" {
		t.Fatalf("family view still reports role %q after promotion", role)
	}
	applied, err := fixture.Caregiver().Sync([]protocol.Mutation{mutation(t, "diaper.json", fixture.h.CommandID())})
	if err != nil || len(applied.Outcomes) != 1 || applied.Outcomes[0].Cursor == nil {
		t.Fatalf("restored caregiver write: %+v err=%v", applied, err)
	}
}

func TestViewerCannotInviteOrManageMembers(t *testing.T) {
	fixture := newProtocolFixture(t)
	if _, err := fixture.h.Family.ChangeRole(t.Context(), fixture.h.Owner, fixture.h.CommandID(), fixture.h.Caregiver.AccountID, "viewer"); err != nil {
		t.Fatal(err)
	}

	// Through the HTTP routes, not the service: the point is that the API a
	// second device actually speaks to refuses these, not that a Go method
	// would have.
	viewer := fixture.Caregiver()
	for _, attempt := range []struct {
		name, path, body string
	}{
		{"invite", "/v1/family/invitations", commandBody(fixture.h.CommandID(), "")},
		{"remove a member", "/v1/family/members/" + fixture.h.Owner.AccountID + "/remove", commandBody(fixture.h.CommandID(), "")},
		{"change a role", "/v1/family/members/" + fixture.h.Owner.AccountID + "/role", commandBody(fixture.h.CommandID(), `,"role":"viewer"`)},
		// Naming the owner as the new owner is a nonsense request, and
		// deliberately so: it has to be refused for want of authority, not
		// for naming the caller, which is a different check entirely.
		{"transfer ownership", "/v1/family/ownership-transfer", commandBody(fixture.h.CommandID(), `,"new_owner_account_id":"`+fixture.h.Owner.AccountID+`"`)},
	} {
		t.Run(attempt.name, func(t *testing.T) {
			status, detail := viewer.FamilyPost(t, attempt.path, attempt.body)
			if status == http.StatusOK || status == http.StatusCreated {
				t.Fatalf("a viewer was allowed to %s", attempt.name)
			}
			// `owner_required` specifically, not merely "some error": a viewer
			// refused as `invalid_request` would look identical here while
			// telling the client to fix a request that was never the problem.
			if detail.Code != "owner_required" {
				t.Fatalf("status=%d code=%q, want owner_required", status, detail.Code)
			}
		})
	}

	// The owner is unaffected by any of that, and the family still has exactly
	// the two members it started with.
	if _, err := fixture.h.Family.Invite(t.Context(), fixture.h.Owner, fixture.h.CommandID(), "caregiver"); err != nil {
		t.Fatalf("owner lost the ability to invite: %v", err)
	}
}

// A viewer who arrives as a viewer, rather than as a caregiver who was demoted
// a moment later.
//
// The distinction is not cosmetic. Between joining and being demoted, a
// caregiver can write — and on a shared family that means records a parent
// never meant to grant the ability to create, arriving from a device they
// wanted to give read access to.
func TestInvitationConfersViewOnlyOnRedemption(t *testing.T) {
	fixture := newProtocolFixture(t)
	if _, err := fixture.Owner().Sync([]protocol.Mutation{mutation(t, "baby.json", fixture.h.CommandID())}); err != nil {
		t.Fatal(err)
	}

	// A third account, invited as a viewer and never anything else.
	invitation, err := fixture.h.Family.Invite(t.Context(), fixture.h.Owner, fixture.h.CommandID(), "viewer")
	if err != nil {
		t.Fatalf("mint a view-only invitation: %v", err)
	}
	client := fixture.JoinAsNewAccount(t, "viewer", invitation.Token)
	if role := memberRole(t, fixture, client.principal.AccountID); role != "viewer" {
		t.Fatalf("redeemed a viewer invitation and became %q", role)
	}
	if pulled, err := client.Sync(nil); err != nil || len(pulled.Changes) != 1 {
		t.Fatalf("invited viewer cannot read: %+v err=%v", pulled, err)
	}

	sequenceBefore := fixture.h.FamilySequence(t)
	_, err = client.Sync([]protocol.Mutation{mutation(t, "diaper.json", fixture.h.CommandID())})
	if detail := refusal(t, err); detail.Code != "read_only_caregiver" {
		t.Fatalf("invited viewer refused as %q", detail.Code)
	}
	if after := fixture.h.FamilySequence(t); after != sequenceBefore {
		t.Errorf("family cursor advanced on a refused mutation: %d -> %d", sequenceBefore, after)
	}

	// And the owner can still promote them, so the choice made when the link
	// was written is a starting point rather than a life sentence.
	if _, err := fixture.h.Family.ChangeRole(t.Context(), fixture.h.Owner, fixture.h.CommandID(), client.principal.AccountID, "caregiver"); err != nil {
		t.Fatalf("promote an invited viewer: %v", err)
	}
	applied, err := client.Sync([]protocol.Mutation{mutation(t, "diaper.json", fixture.h.CommandID())})
	if err != nil || len(applied.Outcomes) != 1 {
		t.Fatalf("promoted viewer still cannot write: %+v err=%v", applied, err)
	}
}

// Revocation had to move with creation and redemption, and this is why: the
// revoke path matches the stored row on its role and validates the effect's
// role against the same vocabulary. Left behind, a view-only link would have
// been one an owner could mint and never withdraw.
func TestViewOnlyInvitationCanBeRevoked(t *testing.T) {
	fixture := newProtocolFixture(t)
	invitation, err := fixture.h.Family.Invite(t.Context(), fixture.h.Owner, fixture.h.CommandID(), "viewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.h.Family.RevokeInvitation(
		t.Context(), fixture.h.Owner, fixture.h.CommandID(), invitation.Invitation.ID,
	); err != nil {
		t.Fatalf("revoke a view-only invitation: %v", err)
	}

	view, err := fixture.h.Family.Invitation(t.Context(), fixture.h.Owner, invitation.Invitation.ID)
	if err != nil || view.Status != "revoked" {
		t.Fatalf("invitation=%+v err=%v", view, err)
	}

	// And it is dead on arrival, not merely marked.
	session, err := fixture.exchangeSignedWithProfile("latecomer", "installation-latecomer", "Latecomer")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := fixture.h.Auth.Authenticate(t.Context(), session.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.h.Family.Redeem(
		t.Context(), principal, fixture.h.CommandID(), invitation.Token, true,
	); err == nil {
		t.Fatal("a revoked view-only invitation was redeemed")
	}
}

/// An invitation may not confer ownership. A family has exactly one owner and
/// it arrives by creating the family or by transfer, both of which name the
/// account explicitly.
func TestInvitationCannotConferOwnership(t *testing.T) {
	fixture := newProtocolFixture(t)
	for _, role := range []string{"owner", "admin", "", "Viewer"} {
		if role == "" {
			continue // the empty role is the caregiver default, tested above
		}
		if _, err := fixture.h.Family.Invite(t.Context(), fixture.h.Owner, fixture.h.CommandID(), role); err == nil {
			t.Fatalf("minted an invitation for role %q", role)
		}
	}
}

// memberRole reads one member's role back out of the family projection, which
// is what the other device's Settings screen renders.
func memberRole(t *testing.T, fixture *protocolFixture, accountID string) string {
	t.Helper()
	view, err := fixture.h.Family.Current(t.Context(), fixture.h.Owner)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range view.Members {
		if member.AccountID == accountID {
			return member.Role
		}
	}
	t.Fatalf("account %s is not a member", accountID)
	return ""
}

// refusal unwraps the HTTP error the protocol client returns and decodes the
// server's own error envelope from it.
//
// Decoded rather than string-matched: a body that happens to CONTAIN
// "read_only_caregiver" inside a message, a request id or an unrelated field
// would satisfy a substring check while the machine-readable code said
// something else entirely — and the code is the part clients branch on.
func refusal(t *testing.T, err error) protocol.ErrorDetail {
	t.Helper()
	var status *statusError
	if !errors.As(err, &status) {
		t.Fatalf("expected an HTTP refusal, got %v", err)
	}
	var response protocol.ErrorResponse
	if decodeErr := json.Unmarshal([]byte(status.body), &response); decodeErr != nil {
		t.Fatalf("refusal body was not an error envelope: %v: %s", decodeErr, status.body)
	}
	if response.Error.Message == "" {
		t.Errorf("refusal %q carried no message for a person to read", response.Error.Code)
	}
	return response.Error
}

// commandBody is the envelope every /v1/family route expects: a protocol
// version, the capabilities the caller claims, and a command id.
func commandBody(commandID, extra string) string {
	return `{"protocol_version":"v1","capabilities":["family-lifecycle-v1"],"command_id":"` + commandID + `"` + extra + `}`
}
