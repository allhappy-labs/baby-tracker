package family

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/identity"
	"babytracker/server/internal/security"
)

func TestRoleMatrix(t *testing.T) {
	for _, action := range []string{"read", "mutate_record", "add_baby"} {
		if !allowed("caregiver", action) {
			t.Fatalf("caregiver denied %s", action)
		}
	}
	for _, action := range []string{"delete_baby", "manage_members", "transfer_owner", "export", "delete_family"} {
		if allowed("caregiver", action) || !allowed("owner", action) {
			t.Fatalf("role matrix mismatch for %s", action)
		}
	}
	if allowed("owner", "invented_action") {
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
		InvitationKeys: identity.DigestKeyring{Current: identity.DigestKey{ID: "invite.v1", Secret: [32]byte{1}}},
		Origin:         "https://sync.example.test", Now: func() time.Time { return time.Now().UTC() },
		MaxCaregivers: 10, MaxInvitations: 10, InvitationTTL: 7 * 24 * time.Hour,
	}
	for name, mutate := range map[string]func(*Options){
		"security":       func(options *Options) { options.Security = nil },
		"repository":     func(options *Options) { options.Repository = nil },
		"entitlements":   func(options *Options) { options.Entitlements = nil },
		"keys":           func(options *Options) { options.InvitationKeys = identity.DigestKeyring{} },
		"origin":         func(options *Options) { options.Origin = "http://sync.example.test" },
		"clock":          func(options *Options) { options.Now = nil },
		"caregiver cap":  func(options *Options) { options.MaxCaregivers = 0 },
		"invitation cap": func(options *Options) { options.MaxInvitations = 0 },
		"ttl":            func(options *Options) { options.InvitationTTL = 0 },
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

// A security failure the family layer does not recognise maps to
// service_unavailable with its cause kept for the log line, and a wrapped
// unavailable stays the same code.
func TestUnavailableWrappedWithItsCauseKeepsItsCode(t *testing.T) {
	cause := errors.New("database_unavailable")
	mapped := familyError(fmt.Errorf("%w: %w", security.ErrUnavailable, cause))
	if Code(mapped) != "service_unavailable" || !errors.Is(mapped, ErrUnavailable) || !errors.Is(mapped, cause) {
		t.Fatalf("mapped unavailable lost its code or cause: %v", mapped)
	}
	if Code(fmt.Errorf("%w: %w", ErrUnavailable, cause)) != "service_unavailable" {
		t.Fatal("wrapped unavailable lost its code")
	}
}
