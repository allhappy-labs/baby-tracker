package protocol_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"

	"babytracker/server/internal/protocol"
)

// legacyEncodeChange is EncodeChange as it was before AppendChange:
// json.Marshal of the envelope, which compacts and HTML-escapes the entity.
func legacyEncodeChange(cursor protocol.Cursor, kind, entityID string, version uint64, actorAccountID, deviceID, operation, acceptedAt string, entity json.RawMessage) (json.RawMessage, error) {
	return json.Marshal(struct {
		Cursor              protocol.Cursor `json:"cursor"`
		EntityKind          string          `json:"entity_kind"`
		EntityID            string          `json:"entity_id"`
		EntityVersion       uint64          `json:"entity_version"`
		ActorAccountID      string          `json:"actor_account_id"`
		DeviceID            string          `json:"device_id"`
		Operation           string          `json:"operation"`
		AcceptedAt          string          `json:"accepted_at"`
		Optional            bool            `json:"optional"`
		SnapshotOrTombstone json.RawMessage `json:"snapshot_or_tombstone"`
	}{cursor, kind, entityID, version, actorAccountID, deviceID, operation, acceptedAt, false, entity})
}

// legacyHTTP is the HTTP body as writeJSON wrote it before AppendJSON:
// json.Encoder over SyncResponse.MarshalJSON.
func legacyHTTP(t *testing.T, response protocol.SyncResponse) ([]byte, error) {
	t.Helper()
	var buffer bytes.Buffer
	err := json.NewEncoder(&buffer).Encode(response)
	return buffer.Bytes(), err
}

// randomText mixes what JSON escaping treats specially: quotes,
// backslashes, control characters, HTML characters, U+2028/U+2029,
// non-ASCII letters, emoji, DEL and invalid UTF-8.
func randomText(random *rand.Rand, n int) string {
	pieces := []string{"a", "Z", "0", " ", "\"", "\\", "/", "\n", "\r", "\t", "\b", "\f", "\x00", "\x1f", "\x7f", "<", ">", "&",
		"\u2028", "\u2029", "é", "ß", "Ж", "中", "😀", "\xff", "\xe2\x80", "\xc3", "\u00a0", "\ufeff", "'", "{", "}", "[", "]", ":", ","}
	var b strings.Builder
	for range n {
		b.WriteString(pieces[random.IntN(len(pieces))])
	}
	return b.String()
}

// randomValue is a random JSON value of depth at most depth.
func randomValue(random *rand.Rand, depth, size int) any {
	switch k := random.IntN(8); {
	case depth <= 0 || k < 3:
		return randomText(random, random.IntN(size+1))
	case k == 3:
		return random.Float64() * 1e6
	case k == 4:
		return random.IntN(2) == 0
	case k == 5:
		return nil
	case k == 6:
		items := make([]any, random.IntN(4))
		for i := range items {
			items[i] = randomValue(random, depth-1, size)
		}
		return items
	default:
		return randomObject(random, depth-1, size)
	}
}

func randomObject(random *rand.Rand, depth, size int) map[string]any {
	object := map[string]any{}
	for range 1 + random.IntN(5) {
		object[randomText(random, 1+random.IntN(6))] = randomValue(random, depth, size)
	}
	return object
}

// randomWire is a stored wire as care.Encode stores one: json.Marshal of
// the entity, here a random object (sometimes large).
func randomWire(t *testing.T, random *rand.Rand) json.RawMessage {
	t.Helper()
	size := 16
	if random.IntN(20) == 0 {
		size = 64 * 1024
	}
	wire, err := json.Marshal(randomObject(random, 3, size))
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func randomEnvelopeText(random *rand.Rand) string {
	if random.IntN(4) == 0 {
		return randomText(random, random.IntN(12))
	}
	return randomUUID(random)
}

// The direct encoders write exactly what the json.Marshal / json.Encoder
// path wrote, over random changes and pages: random entities (unicode,
// escapes, invalid UTF-8, HTML characters, large payloads), random
// envelope strings, every page shape (empty, outcomes, token or not).
func TestDirectEncodersMatchTheJSONPackage(t *testing.T) {
	random := rand.New(rand.NewPCG(11, 110))
	corpus := corpusEntities(t)
	for round := range 400 {
		count := random.IntN(6)
		if round%50 == 0 {
			count = 60
		}
		encoded := make([]json.RawMessage, 0, count)
		for range count {
			var wire json.RawMessage
			if random.IntN(2) == 0 {
				var err error
				if wire, err = json.Marshal(corpus[random.IntN(len(corpus))]); err != nil {
					t.Fatal(err)
				}
			} else {
				wire = randomWire(t, random)
			}
			cursor := protocol.Cursor{ServerInstanceID: randomEnvelopeText(random), RecoveryEpoch: random.Uint64(),
				FamilyID: randomEnvelopeText(random), Sequence: random.Uint64()}
			fields := []string{randomEnvelopeText(random), randomEnvelopeText(random), randomEnvelopeText(random), randomEnvelopeText(random),
				randomEnvelopeText(random), randomEnvelopeText(random)}
			version := random.Uint64()
			want, wantErr := legacyEncodeChange(cursor, fields[0], fields[1], version, fields[2], fields[3], fields[4], fields[5], wire)
			got, err := protocol.EncodeChange(cursor, fields[0], fields[1], version, fields[2], fields[3], fields[4], fields[5], wire)
			if err != nil || wantErr != nil || !bytes.Equal(got, want) {
				t.Fatalf("round %d: EncodeChange %v (legacy %v)\n got %s\nwant %s", round, err, wantErr, got, want)
			}
			prefix := []byte("prefix")
			if appended, err := protocol.AppendChange(prefix, cursor, fields[0], fields[1], version, fields[2], fields[3], fields[4],
				fields[5], wire); err != nil || !bytes.Equal(appended, append([]byte("prefix"), want...)) {
				t.Fatalf("round %d: AppendChange does not append: %v", round, err)
			}
			encoded = append(encoded, got)
		}
		response := protocol.SyncResponse{ServerInstanceID: randomEnvelopeText(random), RecoveryEpoch: random.Uint64(),
			Outcomes: []protocol.Outcome{}, EncodedChanges: encoded,
			HighWater: protocol.Cursor{ServerInstanceID: randomEnvelopeText(random), RecoveryEpoch: random.Uint64(),
				FamilyID: randomEnvelopeText(random), Sequence: random.Uint64()}}
		switch random.IntN(4) {
		case 0:
			response.Outcomes = nil
		case 1:
			entity := corpus[random.IntN(len(corpus))]
			response.Outcomes = []protocol.Outcome{{MutationID: randomUUID(random), Status: "applied", CanonicalEntity: &entity,
				RelatedEntities: []protocol.CanonicalEntity{corpus[random.IntN(len(corpus))]}}}
		}
		if random.IntN(2) == 0 {
			token := randomEnvelopeText(random)
			response.NextPageToken = &token
		}
		want, err := legacyHTTP(t, response)
		if err != nil {
			t.Fatalf("round %d: legacy encoder: %v", round, err)
		}
		got, err := response.AppendJSON(nil)
		if err != nil || !bytes.Equal(append(got, '\n'), want) {
			t.Fatalf("round %d: AppendJSON %v\n got %s\nwant %s", round, err, got, want)
		}
	}
}

// What is not shaped like a stored wire takes the json.Marshal path, with
// its output and its errors: a nil, empty, scalar or array entity, and a
// response without EncodedChanges or with an item that is not an object.
func TestDirectEncodersFallBackToTheJSONPackage(t *testing.T) {
	cursor := protocol.Cursor{ServerInstanceID: "s", RecoveryEpoch: 1, FamilyID: "f", Sequence: 2}
	for _, entity := range []json.RawMessage{nil, {}, json.RawMessage(`null`), json.RawMessage(` [1, 2] `), json.RawMessage(`"<x>"`),
		json.RawMessage(`{`), json.RawMessage(`[{"a": 1}]`)} {
		want, wantErr := legacyEncodeChange(cursor, "baby", "id", 1, "a", "d", "create", "t", entity)
		got, err := protocol.EncodeChange(cursor, "baby", "id", 1, "a", "d", "create", "t", entity)
		if (err == nil) != (wantErr == nil) || !bytes.Equal(got, want) {
			t.Fatalf("entity %q: %s %v, want %s %v", entity, got, err, want, wantErr)
		}
	}
	token := "<token>"
	for _, response := range []protocol.SyncResponse{
		{},
		{Changes: []protocol.JournalChange{}, Outcomes: []protocol.Outcome{}, NextPageToken: &token},
		{EncodedChanges: []json.RawMessage{json.RawMessage(` {"a": "<b>"} `)}},
		{EncodedChanges: []json.RawMessage{nil}},
		{EncodedChanges: []json.RawMessage{json.RawMessage(`[1]`), json.RawMessage(`{"a":1}`)}},
		{EncodedChanges: []json.RawMessage{json.RawMessage(`{"a":1}`), json.RawMessage(`{`)}},
	} {
		want, wantErr := legacyHTTP(t, response)
		got, err := response.AppendJSON(nil)
		if (err == nil) != (wantErr == nil) || (err == nil && !bytes.Equal(append(got, '\n'), want)) {
			t.Fatalf("response %+v: %s %v, want %s %v", response, got, err, want, wantErr)
		}
		var marshalErr *json.MarshalerError
		if err != nil && !errors.As(err, &marshalErr) {
			t.Fatalf("response %+v: error %T %v, want json's MarshalerError", response, err, err)
		}
	}
}
