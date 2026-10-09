package account

import (
	"testing"

	"babytracker/server/internal/access"
	"babytracker/server/internal/security"
)

func TestDeleteRequiresExplicitConfirmationAndCanonicalCommandID(t *testing.T) {
	service := &Service{}
	if _, err := service.Delete(t.Context(), structPrincipal(), "30000000-0000-4000-8000-000000000001", false); Code(err) != "confirmation_required" {
		t.Fatalf("confirmation code=%q err=%v", Code(err), err)
	}
	if _, err := service.Delete(t.Context(), structPrincipal(), "NOT-A-UUID", true); Code(err) != "invalid_request" {
		t.Fatalf("invalid code=%q err=%v", Code(err), err)
	}
	if Code(security.ErrUnavailable) != "service_unavailable" {
		t.Fatal("security error code was not preserved")
	}
}

func structPrincipal() access.Principal {
	return access.Principal{AccountID: "10000000-0000-4000-8000-000000000001"}
}
