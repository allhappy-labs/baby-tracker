package sync

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"babytracker/server/internal/identity"
	"babytracker/server/internal/protocol"
)

func encodingFixture() (protocol.ServerInfo, string, protocol.CanonicalEntity, protocol.JournalChange, string) {
	server := protocol.ServerInfo{InstanceID: "50000000-0000-4000-8000-000000000001", RecoveryEpoch: 1}
	familyID := "30000000-0000-4000-8000-000000000001"
	entity := protocol.CanonicalEntity{EntityKind: "baby", Meta: protocol.EntityMeta{
		ID: "10000000-0000-4000-8000-000000000001", FamilyID: familyID, EntityVersion: 1,
		CreatorAccountID: "20000000-0000-4000-8000-000000000001", LastEditorAccountID: "20000000-0000-4000-8000-000000000001",
		ClientMutationTime: "2026-09-20T12:00:00.000000Z", AcceptedAt: "2026-09-20T12:00:01.000000Z",
	}, Payload: protocol.BabyPayload{Name: strings.Repeat("x", 512), BirthDate: "2026-01-01"}}
	change := protocol.JournalChange{Cursor: protocol.Cursor{ServerInstanceID: server.InstanceID, RecoveryEpoch: 1, FamilyID: familyID, Sequence: 1},
		EntityKind: "baby", EntityID: entity.Meta.ID, EntityVersion: 1, ActorAccountID: entity.Meta.CreatorAccountID,
		DeviceID: "21000000-0000-4000-8000-000000000001", Operation: "create", AcceptedAt: entity.Meta.AcceptedAt,
		SnapshotOrTombstone: entity}
	return server, familyID, entity, change, strings.Repeat("x", protocol.MaximumPageTokenLength)
}

func TestContinuationFixturesCarryProductionTokens(t *testing.T) {
	key := identity.DigestKey{ID: "page.v1"}
	for index := range key.Secret {
		key.Secret[index] = 1
	}
	keys := identity.DigestKeyring{Current: key}
	for _, name := range []string{"sync-page.json", "snapshot-page.json"} {
		raw, err := os.ReadFile("../../api/fixtures/v1/" + name)
		if err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			NextPageToken *string `json:"next_page_token"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil || envelope.NextPageToken == nil || len(*envelope.NextPageToken) > protocol.MaximumPageTokenLength {
			t.Fatalf("%s token=%v err=%v", name, envelope.NextPageToken, err)
		}
		decoded, err := decodePageToken(keys, *envelope.NextPageToken)
		if err != nil || decoded.key.ID != "page.v1" {
			t.Fatalf("%s production decode=%+v err=%v", name, decoded, err)
		}
	}
}

// The page is sized from the stored bytes (IncrementalPageEncodedBytesRaw
// over each change encoded once), exactly as the production encoder writes
// it without its outcomes; the token counts when a row is left over.
func TestIncrementalPageProductionEncoderLimitMinusOneExactAndPlusOne(t *testing.T) {
	server, familyID, _, change, token := encodingFixture()
	encoded, err := json.Marshal(change)
	if err != nil {
		t.Fatal(err)
	}
	exact := protocol.IncrementalPageEncodedBytes(server, familyID, 2, []protocol.JournalChange{change}, &token)
	size := func(bytes, count int, next *string) int {
		return protocol.IncrementalPageEncodedBytesRaw(server, familyID, 2, bytes, count, next)
	}
	for _, test := range []struct {
		name      string
		limit     int
		wantError bool
	}{
		{"limit-minus-one", exact - 1, true}, {"limit", exact, false}, {"limit-plus-one", exact + 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			count, err := pagePrefix([]json.RawMessage{encoded, encoded}, 1, test.limit, ErrPageLimit, size, token)
			if errors.Is(err, ErrPageLimit) != test.wantError || (!test.wantError && count != 1) {
				t.Fatalf("page=%d err=%v exact=%d limit=%d", count, err, exact, test.limit)
			}
		})
	}
	// Without a row left over the token does not count.
	if count, err := pagePrefix([]json.RawMessage{encoded}, 1, exact-len(token)+2, ErrPageLimit, size, token); err != nil || count != 1 {
		t.Fatalf("last page counted the token: %d %v", count, err)
	}
	response := protocol.SyncResponse{ServerInstanceID: server.InstanceID, RecoveryEpoch: server.RecoveryEpoch,
		Outcomes: []protocol.Outcome{{MutationID: "01000000-0000-4000-8000-000000000001", Status: "rejected",
			RelatedEntities: []protocol.CanonicalEntity{encodingFixtureEntityWithName(strings.Repeat("y", 100000))}}},
		EncodedChanges: []json.RawMessage{encoded}, HighWater: protocol.Cursor{ServerInstanceID: server.InstanceID, RecoveryEpoch: 1, FamilyID: familyID, Sequence: 2}, NextPageToken: &token}
	full, err := json.Marshal(response)
	if err != nil || len(full)+1 <= exact || size(len(encoded), 1, &token) != exact {
		t.Fatalf("full=%d metric=%d exact=%d err=%v", len(full)+1, size(len(encoded), 1, &token), exact, err)
	}
}

func TestSnapshotPageProductionEncoderLimitMinusOneExactAndPlusOne(t *testing.T) {
	server, familyID, entity, _, token := encodingFixture()
	snapshotID := "40000000-0000-4000-8000-000000000001"
	highWater := protocol.Cursor{ServerInstanceID: server.InstanceID, RecoveryEpoch: 1, FamilyID: familyID, Sequence: 1}
	wire, err := json.Marshal(entity)
	if err != nil {
		t.Fatal(err)
	}
	exact := protocol.SnapshotPageEncodedBytes(server, snapshotID, highWater, []protocol.CanonicalEntity{entity}, &token)
	size := func(bytes, count int, next *string) int {
		return protocol.SnapshotPageEncodedBytesRaw(server, snapshotID, highWater, bytes, count, next)
	}
	for _, test := range []struct {
		name      string
		limit     int
		wantError bool
	}{
		{"limit-minus-one", exact - 1, true}, {"limit", exact, false}, {"limit-plus-one", exact + 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			count, err := pagePrefix([]json.RawMessage{wire, wire}, 1, test.limit, ErrSnapshotLimit, size, token)
			if errors.Is(err, ErrSnapshotLimit) != test.wantError || (!test.wantError && count != 1) {
				t.Fatalf("page=%d err=%v exact=%d limit=%d", count, err, exact, test.limit)
			}
		})
	}
}

func encodingFixtureEntityWithName(name string) protocol.CanonicalEntity {
	_, _, entity, _, _ := encodingFixture()
	entity.Payload = protocol.BabyPayload{Name: name, BirthDate: "2026-01-01"}
	return entity
}
