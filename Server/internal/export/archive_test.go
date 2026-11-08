package export

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"babytracker/server/internal/protocol"
)

func TestEncodeArchiveFixedOrderAndEmptyArrays(t *testing.T) {
	a := ArchiveV1{ServerInstanceID: "instance", FamilyID: "family", Members: []ArchiveMember{{AccountID: "b", DisplayName: "B"}, {AccountID: "a", DisplayName: "A"}}}
	var out bytes.Buffer
	n, err := EncodeArchive(context.Background(), &out, a)
	if err != nil {
		t.Fatal(err)
	}
	if n != uint64(out.Len()) {
		t.Fatalf("bytes=%d len=%d", n, out.Len())
	}
	s := out.String()
	if !strings.HasPrefix(s, `{"version":"v1","server_instance_id":"instance"`) {
		t.Fatal(s)
	}
	if !strings.Contains(s, `"members":[{"account_id":"a"`) {
		t.Fatal(s)
	}
	for _, name := range []string{"babies", "feeds", "sleeps", "diapers", "supplements", "growth_profiles", "measurements", "immunizations", "pumps", "active_timers", "journal", "audit"} {
		if !strings.Contains(s, `"`+name+`":[]`) {
			t.Fatalf("missing empty %s: %s", name, s)
		}
	}
	if !strings.HasSuffix(s, `"attachments":{"version":"v1","attachments":[]}}`) {
		t.Fatal(s)
	}
	if !json.Valid(out.Bytes()) {
		t.Fatal("invalid JSON")
	}
}

// immunizations sits between measurements and active_timers.
func TestEncodeArchiveKeepsImmunizationEntity(t *testing.T) {
	a := ArchiveV1{Immunizations: []protocol.CanonicalEntity{{EntityKind: "immunization"}}}
	var out bytes.Buffer
	if _, err := EncodeArchive(context.Background(), &out, a); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, `"immunizations":[{"entity_kind":"immunization"`) {
		t.Fatal(s)
	}
	measurements, shots, timers := strings.Index(s, `"measurements":`), strings.Index(s, `"immunizations":`), strings.Index(s, `"active_timers":`)
	if !(measurements < shots && shots < timers) {
		t.Fatalf("section order: measurements %d, immunizations %d, active_timers %d", measurements, shots, timers)
	}
}

// pumps sits between immunizations and active_timers.
func TestEncodeArchiveKeepsPumpEntity(t *testing.T) {
	a := ArchiveV1{Pumps: []protocol.CanonicalEntity{{EntityKind: "pump"}}}
	var out bytes.Buffer
	if _, err := EncodeArchive(context.Background(), &out, a); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, `"pumps":[{"entity_kind":"pump"`) {
		t.Fatal(s)
	}
	shots, pumps, timers := strings.Index(s, `"immunizations":`), strings.Index(s, `"pumps":`), strings.Index(s, `"active_timers":`)
	if !(shots < pumps && pumps < timers) {
		t.Fatalf("section order: immunizations %d, pumps %d, active_timers %d", shots, pumps, timers)
	}
}

func TestEncodeArchiveKeepsSupplementEntity(t *testing.T) {
	a := ArchiveV1{Supplements: []protocol.CanonicalEntity{{EntityKind: "supplement"}}}
	var out bytes.Buffer
	if _, err := EncodeArchive(context.Background(), &out, a); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"supplements":[{"entity_kind":"supplement"`) {
		t.Fatal(out.String())
	}
}

func TestEncodeArchiveWritesFeedComponentsOnly(t *testing.T) {
	a := ArchiveV1{Feeds: []protocol.CanonicalEntity{{
		EntityKind: "feed",
		Payload:    protocol.FeedPayload{Components: []protocol.FeedComponent{{ID: "part", Kind: "formula"}}},
	}}}
	var out bytes.Buffer
	if _, err := EncodeArchive(context.Background(), &out, a); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"components":[{"id":"part","kind":"formula"`) {
		t.Fatal(out.String())
	}
	if strings.Contains(out.String(), `"sides":`) {
		t.Fatal("feed was flattened into scalar sides")
	}
}

func TestEncodeArchiveRejectsOversize(t *testing.T) {
	a := ArchiveV1{Members: []ArchiveMember{{DisplayName: strings.Repeat("x", MaxArchivePlaintextBytes)}}}
	if _, err := EncodeArchive(context.Background(), &bytes.Buffer{}, a); err != ErrArchiveTooLarge {
		t.Fatalf("got %v", err)
	}
}
