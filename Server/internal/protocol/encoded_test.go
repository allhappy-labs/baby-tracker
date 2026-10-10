package protocol_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"testing"

	"babytracker/server/internal/protocol"
)

// corpusEntities decodes every snapshot entity and every change's entity of
// the legacy sync corpus: all eight kinds, tombstones, and the escaping cases.
func corpusEntities(t *testing.T) []protocol.CanonicalEntity {
	t.Helper()
	raw, err := os.ReadFile("../sync/testdata/golden/legacy-sync.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		FullPull struct {
			Changes []struct {
				SnapshotOrTombstone json.RawMessage `json:"snapshot_or_tombstone"`
			} `json:"changes"`
		} `json:"full_pull"`
		Snapshot struct {
			Entities []json.RawMessage `json:"entities"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	values := append([]json.RawMessage{}, corpus.Snapshot.Entities...)
	for _, change := range corpus.FullPull.Changes {
		values = append(values, change.SnapshotOrTombstone)
	}
	entities := make([]protocol.CanonicalEntity, 0, len(values))
	for _, value := range values {
		var entity protocol.CanonicalEntity
		if err := json.Unmarshal(value, &entity); err != nil {
			t.Fatalf("decode corpus entity: %v", err)
		}
		entities = append(entities, entity)
	}
	if len(entities) < 50 {
		t.Fatalf("corpus has %d entities", len(entities))
	}
	return entities
}

func randomUUID(random *rand.Rand) string {
	return fmt.Sprintf("%08x-%04x-4%03x-8%03x-%012x", random.Uint32(), random.Uint32N(1<<16), random.Uint32N(1<<12),
		random.Uint32N(1<<12), random.Uint64N(1<<48)|1)
}

func randomChange(random *rand.Rand, entity protocol.CanonicalEntity) protocol.JournalChange {
	operations := []string{"create", "update", "delete", "timer_start", "timer_finish_bottle", "timer_stop"}
	return protocol.JournalChange{
		Cursor: protocol.Cursor{ServerInstanceID: randomUUID(random), RecoveryEpoch: 1 + random.Uint64N(protocol.MaxSafeInteger),
			FamilyID: randomUUID(random), Sequence: random.Uint64N(protocol.MaxSafeInteger + 1)},
		EntityKind: entity.EntityKind, EntityID: entity.Meta.ID, EntityVersion: 1 + random.Uint64N(protocol.MaxSafeInteger),
		ActorAccountID: randomUUID(random), DeviceID: randomUUID(random), Operation: operations[random.IntN(len(operations))],
		AcceptedAt: fmt.Sprintf("2026-%02d-%02dT%02d:%02d:%02d.%06dZ", 1+random.IntN(12), 1+random.IntN(28), random.IntN(24),
			random.IntN(60), random.IntN(60), random.IntN(1000000)),
		SnapshotOrTombstone: entity,
	}
}

func encodeChange(t *testing.T, change protocol.JournalChange) json.RawMessage {
	t.Helper()
	wire, err := json.Marshal(change.SnapshotOrTombstone)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := protocol.EncodeChange(change.Cursor, change.EntityKind, change.EntityID, change.EntityVersion,
		change.ActorAccountID, change.DeviceID, change.Operation, change.AcceptedAt, wire)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestEncodeChangeMatchesJournalChange(t *testing.T) {
	random := rand.New(rand.NewPCG(3, 30))
	for round := 0; round < 4; round++ {
		for index, entity := range corpusEntities(t) {
			change := randomChange(random, entity)
			want, err := json.Marshal(change)
			if err != nil {
				t.Fatal(err)
			}
			if got := encodeChange(t, change); !bytes.Equal(got, want) {
				t.Fatalf("round %d entity %d:\n got %s\nwant %s", round, index, got, want)
			}
		}
	}
}

// encodeHTTP writes value as the HTTP layer does (httpapi writeJSON).
func encodeHTTP(t *testing.T, value any) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := json.NewEncoder(&buffer).Encode(value); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestEncodedResponsesMatchStructEncoding(t *testing.T) {
	random := rand.New(rand.NewPCG(4, 40))
	entities := corpusEntities(t)
	token := "opaque-token"
	type plainSync protocol.SyncResponse
	type plainSnapshot protocol.SnapshotResponse
	for _, count := range []int{0, 1, 2, len(entities)} {
		for _, next := range []*string{nil, &token} {
			changes := make([]protocol.JournalChange, 0, count)
			encodedChanges := make([]json.RawMessage, 0, count)
			snapshotEntities := make([]protocol.CanonicalEntity, 0, count)
			encodedEntities := make([]json.RawMessage, 0, count)
			for _, entity := range entities[:count] {
				change := randomChange(random, entity)
				changes = append(changes, change)
				encodedChanges = append(encodedChanges, encodeChange(t, change))
				wire, err := json.Marshal(entity)
				if err != nil {
					t.Fatal(err)
				}
				snapshotEntities = append(snapshotEntities, entity)
				encodedEntities = append(encodedEntities, wire)
			}
			highWater := protocol.Cursor{ServerInstanceID: randomUUID(random), RecoveryEpoch: 3, FamilyID: randomUUID(random), Sequence: 99}
			outcomes := []protocol.Outcome{{MutationID: randomUUID(random), Status: "applied", RelatedEntities: []protocol.CanonicalEntity{}}}
			if count > 0 {
				outcomes[0].CanonicalEntity = &entities[0]
			}
			sync := protocol.SyncResponse{ServerInstanceID: highWater.ServerInstanceID, RecoveryEpoch: 3, Outcomes: outcomes,
				Changes: changes, HighWater: highWater, NextPageToken: next}
			want := encodeHTTP(t, plainSync(sync))
			if got := encodeHTTP(t, sync); !bytes.Equal(got, want) {
				t.Fatalf("struct sync count %d: MarshalJSON changed the encoding:\n got %s\nwant %s", count, got, want)
			}
			encoded := sync
			encoded.Changes, encoded.EncodedChanges = nil, encodedChanges
			if got := encodeHTTP(t, encoded); !bytes.Equal(got, want) {
				t.Fatalf("encoded sync count %d:\n got %s\nwant %s", count, got, want)
			}
			if got, err := json.Marshal(&encoded); err != nil || !bytes.Equal(append(got, '\n'), want) {
				t.Fatalf("encoded sync through a pointer count %d: err=%v", count, err)
			}

			snapshot := protocol.SnapshotResponse{ServerInstanceID: highWater.ServerInstanceID, RecoveryEpoch: 3, SnapshotID: randomUUID(random),
				HighWater: highWater, Entities: snapshotEntities, NextPageToken: next}
			want = encodeHTTP(t, plainSnapshot(snapshot))
			if got := encodeHTTP(t, snapshot); !bytes.Equal(got, want) {
				t.Fatalf("struct snapshot count %d: MarshalJSON changed the encoding:\n got %s\nwant %s", count, got, want)
			}
			encodedSnapshot := snapshot
			encodedSnapshot.Entities, encodedSnapshot.EncodedEntities = nil, encodedEntities
			if got := encodeHTTP(t, encodedSnapshot); !bytes.Equal(got, want) {
				t.Fatalf("encoded snapshot count %d:\n got %s\nwant %s", count, got, want)
			}
		}
	}
	if count := bytes.Count(encodeHTTP(t, protocol.SyncResponse{EncodedChanges: []json.RawMessage{}}), []byte(`"changes":[]`)); count != 1 {
		t.Fatalf("empty encoded changes must be written as []")
	}
	if count := bytes.Count(encodeHTTP(t, protocol.SnapshotResponse{EncodedEntities: []json.RawMessage{}}), []byte(`"entities":[]`)); count != 1 {
		t.Fatalf("empty encoded entities must be written as []")
	}
}

func TestRawPageSizesMatchMarshal(t *testing.T) {
	random := rand.New(rand.NewPCG(5, 50))
	entities := corpusEntities(t)
	server := protocol.ServerInfo{InstanceID: randomUUID(random), RecoveryEpoch: 7}
	familyID := randomUUID(random)
	token := "a-continuation-token"
	for _, count := range []int{0, 1, 5, len(entities)} {
		for _, next := range []*string{nil, &token} {
			changes := make([]protocol.JournalChange, 0, count)
			snapshotEntities := make([]protocol.CanonicalEntity, 0, count)
			changeBytes, entityBytes := 0, 0
			for _, entity := range entities[:count] {
				change := randomChange(random, entity)
				changes = append(changes, change)
				changeBytes += len(encodeChange(t, change))
				wire, err := json.Marshal(entity)
				if err != nil {
					t.Fatal(err)
				}
				snapshotEntities = append(snapshotEntities, entity)
				entityBytes += len(wire)
			}
			want := protocol.IncrementalPageEncodedBytes(server, familyID, 42, changes, next)
			if got := protocol.IncrementalPageEncodedBytesRaw(server, familyID, 42, changeBytes, count, next); got != want {
				t.Fatalf("incremental count %d token %v: raw %d, marshal %d", count, next != nil, got, want)
			}
			highWater := protocol.Cursor{ServerInstanceID: server.InstanceID, RecoveryEpoch: 7, FamilyID: familyID, Sequence: 42}
			snapshotID := randomUUID(random)
			want = protocol.SnapshotPageEncodedBytes(server, snapshotID, highWater, snapshotEntities, next)
			if got := protocol.SnapshotPageEncodedBytesRaw(server, snapshotID, highWater, entityBytes, count, next); got != want {
				t.Fatalf("snapshot count %d token %v: raw %d, marshal %d", count, next != nil, got, want)
			}
		}
	}
}

func TestSumDecimals(t *testing.T) {
	for _, test := range []struct {
		values []string
		want   string
	}{
		{nil, "0"},
		{[]string{"120.5", "89.5"}, "210"},
		{[]string{"0.000001", "0.000002"}, "0.000003"},
		{[]string{"1", "2.25", "0.75"}, "4"},
		{[]string{"0", "0"}, "0"},
		{[]string{"1.5", "-1.5"}, "0"},
		{[]string{"2000", "2000", "0.123456"}, "4000.123456"},
	} {
		values := make([]protocol.Decimal, 0, len(test.values))
		for _, value := range test.values {
			values = append(values, *protocol.MustParseDecimal(value))
		}
		got := protocol.SumDecimals(values)
		if got.String() != test.want {
			t.Fatalf("SumDecimals(%v) = %s, want %s", test.values, got.String(), test.want)
		}
		parsed := protocol.MustParseDecimal(test.want)
		if gotJSON, _ := json.Marshal(got); string(gotJSON) != parsed.String() {
			t.Fatalf("SumDecimals(%v) encodes %s, want %s", test.values, gotJSON, parsed.String())
		}
	}
	// The sum does not alias its inputs.
	values := []protocol.Decimal{*protocol.MustParseDecimal("1.5")}
	sum := protocol.SumDecimals(values)
	_ = protocol.SumDecimals([]protocol.Decimal{sum, sum})
	if values[0].String() != "1.5" || sum.String() != "1.5" {
		t.Fatalf("SumDecimals aliased its input: %s %s", values[0].String(), sum.String())
	}
}

// EncodedChanges/EncodedEntities take precedence over Changes/Entities when
// both are set; a non-nil empty encoded slice is written as [], and nil
// falls back to the struct slice exactly as before (null when that is nil).
func TestEncodedSlicesTakePrecedence(t *testing.T) {
	entities := corpusEntities(t)
	random := rand.New(rand.NewPCG(6, 60))
	change := randomChange(random, entities[0])
	encoded := encodeChange(t, randomChange(random, entities[1]))
	sync := protocol.SyncResponse{Outcomes: []protocol.Outcome{}, Changes: []protocol.JournalChange{change}, EncodedChanges: []json.RawMessage{encoded}}
	got := encodeHTTP(t, sync)
	if !bytes.Contains(got, []byte(`"changes":[`+string(encoded)+`]`)) || bytes.Contains(got, []byte(change.EntityID+`","entity_version`)) {
		t.Fatalf("encoded changes did not take precedence: %s", got)
	}
	sync.EncodedChanges = []json.RawMessage{}
	if got := encodeHTTP(t, sync); !bytes.Contains(got, []byte(`"changes":[]`)) {
		t.Fatalf("empty encoded changes: %s", got)
	}
	sync.Changes, sync.EncodedChanges = nil, nil
	if got := encodeHTTP(t, sync); !bytes.Contains(got, []byte(`"changes":null`)) {
		t.Fatalf("nil changes keep the legacy null: %s", got)
	}
	wire, err := json.Marshal(entities[1])
	if err != nil {
		t.Fatal(err)
	}
	snapshot := protocol.SnapshotResponse{Entities: []protocol.CanonicalEntity{entities[0]}, EncodedEntities: []json.RawMessage{wire}}
	if got := encodeHTTP(t, snapshot); !bytes.Contains(got, []byte(`"entities":[`+string(wire)+`]`)) {
		t.Fatalf("encoded entities did not take precedence: %s", got)
	}
	snapshot.EncodedEntities = []json.RawMessage{}
	if got := encodeHTTP(t, snapshot); !bytes.Contains(got, []byte(`"entities":[]`)) {
		t.Fatalf("empty encoded entities: %s", got)
	}
	snapshot.Entities, snapshot.EncodedEntities = nil, nil
	if got := encodeHTTP(t, snapshot); !bytes.Contains(got, []byte(`"entities":null`)) {
		t.Fatalf("nil entities keep the legacy null: %s", got)
	}
	// Escaped regardless of the outer encoder's setting.
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(protocol.SnapshotResponse{EncodedEntities: []json.RawMessage{json.RawMessage(`"<&>"`)}}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buffer.Bytes(), []byte(`"\u003c\u0026\u003e"`)) {
		t.Fatalf("MarshalJSON output not HTML-escaped: %s", buffer.Bytes())
	}
}
