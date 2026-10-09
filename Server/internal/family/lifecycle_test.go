package family

import (
	"testing"

	"babytracker/server/internal/access"
)

func TestLifecycleMutationsRequireConfirmationAndCanonicalCommandID(t *testing.T) {
	service := &Service{}
	principal := access.Principal{AccountID: "10000000-0000-4000-8000-000000000001"}
	validCommandID := "30000000-0000-4000-8000-000000000001"

	if _, err := service.ScheduleDeletion(t.Context(), principal, validCommandID, false); Code(err) != "confirmation_required" {
		t.Fatalf("schedule confirmation code=%q err=%v", Code(err), err)
	}
	if _, err := service.Recover(t.Context(), principal, validCommandID, false); Code(err) != "confirmation_required" {
		t.Fatalf("recover confirmation code=%q err=%v", Code(err), err)
	}
	if _, err := service.Forfeit(t.Context(), principal, validCommandID, false); Code(err) != "confirmation_required" {
		t.Fatalf("forfeit confirmation code=%q err=%v", Code(err), err)
	}
	if _, err := service.Recover(t.Context(), principal, "NOT-A-UUID", true); Code(err) != "invalid_request" {
		t.Fatalf("recover invalid command code=%q err=%v", Code(err), err)
	}
}
