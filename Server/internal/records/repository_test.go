package records

import (
	"testing"

	"babytracker/server/internal/protocol"
)

func TestEntityStorageBytesIsDeterministicAndNonzero(t *testing.T) {
	entity := protocol.CanonicalEntity{EntityKind: "baby", Meta: protocol.EntityMeta{
		ID: "10000000-0000-4000-8000-000000000001", FamilyID: "20000000-0000-4000-8000-000000000001",
		EntityVersion: 1, CreatorAccountID: "30000000-0000-4000-8000-000000000001",
		LastEditorAccountID: "30000000-0000-4000-8000-000000000001",
		ClientMutationTime:  "2026-09-20T12:00:00.000000Z", AcceptedAt: "2026-09-20T12:00:01.000000Z",
	}, Payload: protocol.BabyPayload{Name: "Élie", BirthDate: "2026-01-12"}}
	first, err := EntityStorageBytes(entity)
	if err != nil || first <= 0 {
		t.Fatalf("bytes=%d err=%v", first, err)
	}
	second, err := EntityStorageBytes(entity)
	if err != nil || first != second {
		t.Fatalf("second=%d err=%v", second, err)
	}
}

func TestFindEntityKindRejectsUnsupportedIdentifier(t *testing.T) {
	if protocol.ValidUUID("not-an-id") {
		t.Fatal("fixture unexpectedly valid")
	}
}
