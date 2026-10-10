package family

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"babytracker/server/internal/identity"
)

func TestRoleMatrix(t *testing.T) {
	for _, action := range []string{"read", "mutate_record", "add_baby"} {
		if !Allowed("caregiver", action) {
			t.Fatalf("caregiver denied %s", action)
		}
	}
	for _, action := range []string{"delete_baby", "manage_members", "transfer_owner", "export", "delete_family"} {
		if Allowed("caregiver", action) || !Allowed("owner", action) {
			t.Fatalf("role matrix mismatch for %s", action)
		}
	}
	if Allowed("owner", "invented_action") {
		t.Fatal("unknown action allowed")
	}
}

func TestDeterministicDomainSeparatedIDsKnownAnswers(t *testing.T) {
	const actor = "10000000-0000-4000-8000-000000000001"
	if got, err := FamilyID(actor, "30000000-0000-4000-8000-000000000001"); err != nil || got != "c79046b7-969a-8358-b7e3-97d4c039e63f" {
		t.Fatalf("family id = %q", got)
	}
	if got, err := InvitationID(actor, "30000000-0000-4000-8000-000000000002"); err != nil || got != "307a90e7-95e3-82eb-a2e9-bdd569ff2821" {
		t.Fatalf("invitation id = %q", got)
	}
	familyID, _ := FamilyID(actor, "30000000-0000-4000-8000-000000000001")
	invitationID, _ := InvitationID(actor, "30000000-0000-4000-8000-000000000001")
	if familyID == invitationID {
		t.Fatal("domains collided")
	}
	if _, err := FamilyID(strings.ToUpper("aaaaaaaa-0000-4000-8000-000000000001"), "30000000-0000-4000-8000-000000000001"); err == nil {
		t.Fatal("noncanonical actor accepted")
	}
}

func TestNewRejectsUnsafeConfiguration(t *testing.T) {
	valid := Options{
		Authority:      &authority.Service{},
		InvitationKeys: identity.DigestKeyring{Current: identity.DigestKey{ID: "invite.v1", Secret: [32]byte{1}}},
		Origin:         "https://sync.example.test", Now: func() time.Time { return time.Now().UTC() },
		InvitationTTL: 7 * 24 * time.Hour,
	}
	if service, err := New(valid); err != nil || service == nil {
		t.Fatalf("valid options rejected: service=%v err=%v", service, err)
	}
	for name, mutate := range map[string]func(*Options){
		"authority": func(options *Options) { options.Authority = nil },
		"keys":      func(options *Options) { options.InvitationKeys = identity.DigestKeyring{} },
		"origin":    func(options *Options) { options.Origin = "http://sync.example.test" },
		"clock":     func(options *Options) { options.Now = nil },
		"ttl":       func(options *Options) { options.InvitationTTL = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			options := valid
			mutate(&options)
			if service, err := New(options); err == nil || service != nil {
				t.Fatalf("service=%v err=%v", service, err)
			}
		})
	}
}

func TestOperationalErrorCodesRemainDistinct(t *testing.T) {
	for _, err := range []error{ErrAuthorizationQuarantine, ErrHostedAdapterUnavailable, ErrCredentialKeyUnavailable} {
		if got := Code(familyError(err)); got != err.Error() {
			t.Fatalf("error=%v code=%q", err, got)
		}
	}
}

// Allowed is the one policy table: the authority evaluates it in production
// (authority.Options.Allowed), and the authority's own test fixture carries a
// copy that must not drift from it.
func TestAllowedIsTheAuthorityFixturePolicy(t *testing.T) {
	actions := []string{"read", "read_receipt", "mutate_record", "add_baby", "delete_baby", "manage_members", "repair_profile",
		"transfer_owner", "export", "export_family", "delete_family", "recover_family", "forfeit_recovery", "invented_action", ""}
	for _, role := range []string{"owner", "caregiver", "viewer", "former", ""} {
		for _, action := range actions {
			if Allowed(role, action) != authoritytest.Allowed(role, action) {
				t.Errorf("Allowed(%q,%q)=%v, authority fixture says %v", role, action, Allowed(role, action), authoritytest.Allowed(role, action))
			}
		}
	}
}

// Every authority code the family surface returns is a legacy family code;
// an authority-only code (a failed audit chain) is service_unavailable with
// its cause kept.
func TestFamilyErrorKeepsLegacyCodes(t *testing.T) {
	for authorityCode, want := range map[string]string{
		"invalid_command": "invalid_request", "family_slot_occupied": "family_slot_occupied", "owner_required": "owner_required",
		"read_only_caregiver": "read_only_caregiver", "membership_revoked": "membership_revoked", "family_unavailable": "family_unavailable",
		"invitation_invalid": "invitation_invalid", "invitation_expired": "invitation_expired", "invitation_consumed": "invitation_consumed",
		"invitation_revoked": "invitation_revoked", "caregiver_limit": "caregiver_limit", "invitation_limit": "invitation_limit",
		"profile_not_required": "profile_not_required", "entitlement_required": "entitlement_required", "command_id_reused": "command_id_reused",
		"epoch_changed": "epoch_changed", "session_revoked": "session_revoked", "reauthentication_required": "reauthentication_required",
		"security_busy": "security_busy", "authorization_quarantine": "authorization_quarantine", "recovery_only": "recovery_only",
		"recovery_expired": "recovery_expired", "recovery_forfeited": "recovery_forfeited",
		"ownership_transfer_required": "ownership_transfer_required", "scope_failed": "service_unavailable",
		"service_unavailable": "service_unavailable",
	} {
		cause := authority.Code(authorityCode)
		err := familyError(fmt.Errorf("authority: wrapped: %w", cause))
		if got := Code(err); got != want {
			t.Errorf("%s: code=%q want %q", authorityCode, got, want)
		}
		if want == "service_unavailable" && !errors.Is(err, cause) {
			t.Errorf("%s: cause dropped from %v", authorityCode, err)
		}
	}
}
