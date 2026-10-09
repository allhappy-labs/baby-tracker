package family

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/identity"
)

func TestInvitationDigestUsesDedicatedFormat(t *testing.T) {
	key := identity.DigestKey{ID: "invitation.v1"}
	for index := range key.Secret {
		key.Secret[index] = byte(index)
	}
	raw := make([]byte, 32)
	for index := range raw {
		raw[index] = byte(index + 0x20)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	got, err := invitationDigest(key, token)
	if err != nil || got != "hmac-sha256:invitation.v1:188c4e762089679aad3bda9d8ce3bf1996dee1ea2b71e97dee7c30b1103525d8" {
		t.Fatalf("digest=%q", got)
	}
	if !validInvitationToken(token) || validInvitationToken(token+"=") || validInvitationToken("short") {
		t.Fatal("invitation token validation mismatch")
	}
}

func TestPendingInvitationsListsOnlyActiveUnexpiredNewestFirst(t *testing.T) {
	h := newFamilyFixture(t)
	// The fixture itself mints an active invitation for h.Owner during setup
	// (h.invitationIDA); clear it so it does not pollute this list.
	if _, err := h.Service.RevokeInvitation(t.Context(), h.Owner, h.CommandID(0), h.invitationIDA); err != nil {
		t.Fatal(err)
	}
	h.Service.invitationTTL = 24 * time.Hour

	revoked, err := h.Service.Invite(t.Context(), h.Owner, h.CommandID(0), "caregiver")
	if err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Minute)

	redeemed, err := h.Service.Invite(t.Context(), h.Owner, h.CommandID(0), "viewer")
	if err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Minute)

	oldestValid, err := h.Service.Invite(t.Context(), h.Owner, h.CommandID(0), "caregiver")
	if err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Minute)

	// A short TTL for this one invitation only, so the clock advance below
	// expires it without touching the others' much longer expiry.
	h.Service.invitationTTL = 30 * time.Second
	expired, err := h.Service.Invite(t.Context(), h.Owner, h.CommandID(0), "viewer")
	if err != nil {
		t.Fatal(err)
	}
	h.Service.invitationTTL = 24 * time.Hour
	h.now = h.now.Add(time.Minute)

	middleValid, err := h.Service.Invite(t.Context(), h.Owner, h.CommandID(0), "caregiver")
	if err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Minute)

	newestValid, err := h.Service.Invite(t.Context(), h.Owner, h.CommandID(0), "viewer")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := h.Service.RevokeInvitation(t.Context(), h.Owner, h.CommandID(0), revoked.Invitation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.Redeem(t.Context(), h.NewCaregiver, h.CommandID(0), redeemed.Token, true); err != nil {
		t.Fatal(err)
	}

	views, err := h.Service.PendingInvitations(t.Context(), h.Owner)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		id, role string
	}{
		{newestValid.Invitation.ID, "viewer"},
		{middleValid.Invitation.ID, "caregiver"},
		{oldestValid.Invitation.ID, "caregiver"},
	}
	if len(views) != len(want) {
		t.Fatalf("views=%+v want=%+v", views, want)
	}
	for index, view := range views {
		if view.ID != want[index].id || view.Role != want[index].role || view.Status != "active" || view.FamilyID != oldestValid.Invitation.FamilyID {
			t.Fatalf("view[%d]=%+v want=%+v", index, view, want[index])
		}
	}
	_ = expired
}

func TestPendingInvitationsRefusesNonOwners(t *testing.T) {
	h := newFamilyFixture(t)
	caregiverInvite, err := h.Service.Invite(t.Context(), h.Owner, h.CommandID(0), "caregiver")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.Redeem(t.Context(), h.NewCaregiver, h.CommandID(0), caregiverInvite.Token, true); err != nil {
		t.Fatal(err)
	}
	viewer := h.signIn("pending-invitations-viewer", "Viewer")
	viewerInvite, err := h.Service.Invite(t.Context(), h.Owner, h.CommandID(0), "viewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.Redeem(t.Context(), viewer, h.CommandID(0), viewerInvite.Token, true); err != nil {
		t.Fatal(err)
	}
	for name, principal := range map[string]access.Principal{"caregiver": h.NewCaregiver, "viewer": viewer} {
		if _, err := h.Service.PendingInvitations(t.Context(), principal); Code(err) != "owner_required" {
			t.Fatalf("%s code=%q err=%v", name, Code(err), err)
		}
	}
}

func TestPendingInvitationsNeverExposeSecrets(t *testing.T) {
	h := newFamilyFixture(t)
	if _, err := h.Service.Invite(t.Context(), h.Owner, h.CommandID(0), "caregiver"); err != nil {
		t.Fatal(err)
	}
	views, err := h.Service.PendingInvitations(t.Context(), h.Owner)
	if err != nil || len(views) == 0 {
		t.Fatalf("views=%+v err=%v", views, err)
	}
	encoded, err := json.Marshal(views)
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(encoded))
	for _, forbidden := range []string{"token", "link", "digest"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("pending invitations exposed %q: %s", forbidden, encoded)
		}
	}
}

// Two families, each with an open invitation of its own: each owner's list
// holds only their family's, whichever of the query's filter or row-level
// security is doing the scoping.
func TestPendingInvitationsAreScopedToTheCallersFamily(t *testing.T) {
	h := newFamilyFixture(t)
	for name, tc := range map[string]struct {
		owner          access.Principal
		want, excluded string
	}{
		"family A": {h.Owner, h.invitationIDA, h.invitationIDB},
		"family B": {h.SecondOwner, h.invitationIDB, h.invitationIDA},
	} {
		views, err := h.Service.PendingInvitations(t.Context(), tc.owner)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(views) != 1 || views[0].ID != tc.want || views[0].FamilyID != h.familyID(tc.owner.AccountID) {
			t.Fatalf("%s: views=%+v want only %s (not %s)", name, views, tc.want, tc.excluded)
		}
	}
}
