//go:build identitytest

package protocol_test

import (
	"encoding/hex"
	"testing"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

func TestImportsPreserveDistinctAndMatchAcrossCaregivers(t *testing.T) {
	fixture := newProtocolFixture(t)
	distinctRequest := testkit.ImportRequest(0, "Same", "2026-01-12")
	distinctRequest.ImportID = "10000000-0000-4000-8000-000000000090"
	distinctRequest.ImportMode = "preserve_local_ids"
	distinctRequest.Babies[0].LocalBabyID = "10000000-0000-4000-8000-000000000090"
	sibling := distinctRequest.Babies[0]
	sibling.LocalBabyID = "10000000-0000-4000-8000-000000000091"
	sibling.SortOrder = 1
	distinctRequest.Babies = append(distinctRequest.Babies, sibling)
	recordIDs := []string{"20000000-0000-4000-8000-000000000090", "20000000-0000-4000-8000-000000000091"}
	mutations := make([]protocol.Mutation, 0, 2)
	for index, baby := range distinctRequest.Babies {
		mutation := testkit.MustMutation(t, "feed-bottle.json")
		mutation.MutationID = fixture.h.CommandID()
		mutation.EntityID = recordIDs[index]
		mutation.ImportID = &distinctRequest.ImportID
		payload := mutation.Payload.(protocol.FeedPayload)
		payload.BabyID = baby.LocalBabyID
		mutation.Payload = payload
		hash, hashErr := protocol.CanonicalContentHash(mutation.EntityKind, mutation.Payload)
		if hashErr != nil {
			t.Fatal(hashErr)
		}
		distinctRequest.Records = append(distinctRequest.Records, protocol.RecordManifest{RecordID: mutation.EntityID,
			EntityKind: mutation.EntityKind, LocalBabyID: baby.LocalBabyID, ContentSHA256: hex.EncodeToString(hash[:])})
		mutations = append(mutations, mutation)
	}
	distinctRequest.TotalRecordCount = uint32(len(mutations))
	owner, err := fixture.Owner().Import(distinctRequest)
	if err != nil || !owner.MappingComplete || len(owner.Mappings) != 2 || owner.Mappings[0].SharedBabyID == owner.Mappings[1].SharedBabyID {
		t.Fatalf("owner import=%+v err=%v", owner, err)
	}
	for _, mapping := range owner.Mappings {
		if mapping.SharedBabyID != mapping.LocalBabyID || (mapping.LocalBabyID != distinctRequest.Babies[0].LocalBabyID && mapping.LocalBabyID != distinctRequest.Babies[1].LocalBabyID) {
			t.Fatalf("preserve_local_ids remapped baby: %+v", mapping)
		}
	}
	created, err := fixture.Owner().Sync(mutations)
	if err != nil || len(created.Outcomes) != 2 || created.Outcomes[0].Status != "applied" || created.Outcomes[1].Status != "applied" {
		t.Fatalf("dependent records=%+v err=%v", created.Outcomes, err)
	}
	reconciled, err := fixture.Caregiver().Sync(nil)
	if err != nil {
		t.Fatal(err)
	}
	references := map[string]string{}
	for _, change := range reconciled.Changes {
		if change.EntityKind == "feed" {
			payload, ok := change.SnapshotOrTombstone.Payload.(protocol.FeedPayload)
			if !ok {
				t.Fatalf("dependent record payload=%T", change.SnapshotOrTombstone.Payload)
			}
			references[change.EntityID] = payload.BabyID
		}
	}
	for index, recordID := range recordIDs {
		if references[recordID] != distinctRequest.Babies[index].LocalBabyID {
			t.Fatalf("dependent record %s baby=%q want=%q", recordID, references[recordID], distinctRequest.Babies[index].LocalBabyID)
		}
	}
	uniqueRequest := testkit.ImportRequest(1, "Unique", "2026-02-03")
	uniqueRequest.ImportID = "10000000-0000-4000-8000-000000000092"
	uniqueRequest.Babies[0].LocalBabyID = "10000000-0000-4000-8000-000000000092"
	unique, err := fixture.Owner().Import(uniqueRequest)
	if err != nil || !unique.MappingComplete || len(unique.Mappings) != 1 {
		t.Fatalf("unique owner import=%+v err=%v", unique, err)
	}
	matchRequest := testkit.ImportRequest(2, "Unique", "2026-02-03")
	matchRequest.ImportID = "10000000-0000-4000-8000-000000000093"
	matchRequest.Babies[0].LocalBabyID = "10000000-0000-4000-8000-000000000093"
	matched, err := fixture.Caregiver().Import(matchRequest)
	if err != nil || !matched.MappingComplete || len(matched.Mappings) != 1 || matched.Mappings[0].SharedBabyID != unique.Mappings[0].SharedBabyID {
		t.Fatalf("matched=%+v unique=%+v err=%v", matched, unique, err)
	}
	if fixture.h.CareCount(t, "baby", fixture.h.FamilyID) != 3 {
		t.Fatal("equal-looking siblings collapsed during import")
	}
}
