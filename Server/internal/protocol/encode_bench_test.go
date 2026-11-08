package protocol_test

import (
	"encoding/json"
	"io"
	"math/rand/v2"
	"strings"
	"testing"

	"babytracker/server/internal/protocol"
)

// benchPage is a sync page of n changes around the corpus entities' wires
// (each with a note field appended when note is set), encoded as syncPage
// encodes them, with the changes and wires it was built from.
func benchPage(b *testing.B, n int, note string) (protocol.SyncResponse, []protocol.JournalChange, [][]byte) {
	b.Helper()
	random := rand.New(rand.NewPCG(9, 90))
	entities := corpusEntities(&testing.T{})
	changes := make([]protocol.JournalChange, 0, n)
	wires := make([][]byte, 0, n)
	encoded := make([]json.RawMessage, 0, n)
	for i := range n {
		entity := entities[i%len(entities)]
		change := randomChange(random, entity)
		wire, err := json.Marshal(entity)
		if err != nil {
			b.Fatal(err)
		}
		if note != "" {
			raw, _ := json.Marshal(note)
			wire = append(append(append(wire[:len(wire)-1], `,"x":`...), raw...), '}')
		}
		e, err := protocol.EncodeChange(change.Cursor, change.EntityKind, change.EntityID, change.EntityVersion, change.ActorAccountID,
			change.DeviceID, change.Operation, change.AcceptedAt, wire)
		if err != nil {
			b.Fatal(err)
		}
		changes, wires, encoded = append(changes, change), append(wires, wire), append(encoded, e)
	}
	high := protocol.Cursor{ServerInstanceID: randomUUID(random), RecoveryEpoch: 3, FamilyID: randomUUID(random), Sequence: 99}
	return protocol.SyncResponse{ServerInstanceID: high.ServerInstanceID, RecoveryEpoch: 3, Outcomes: []protocol.Outcome{},
		EncodedChanges: encoded, HighWater: high}, changes, wires
}

type benchSize struct {
	name string
	n    int
	note string
}

func benchSizes() []benchSize {
	return []benchSize{{"1", 1, ""}, {"100", 100, ""}, {"10x64KiB", 10, strings.Repeat("a long note é <b> & ", 64*1024/21)}}
}

func BenchmarkEncodeChange(b *testing.B) {
	for _, size := range benchSizes() {
		b.Run(size.name, func(b *testing.B) {
			_, changes, wires := benchPage(b, size.n, size.note)
			b.ReportAllocs()
			for b.Loop() {
				for i, c := range changes {
					if _, err := protocol.EncodeChange(c.Cursor, c.EntityKind, c.EntityID, c.EntityVersion, c.ActorAccountID, c.DeviceID,
						c.Operation, c.AcceptedAt, wires[i]); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}

// BenchmarkSyncResponseHTTP is the response body as the HTTP layer writes
// it (httpapi writeJSON: AppendJSON and the newline, one write).
func BenchmarkSyncResponseHTTP(b *testing.B) {
	for _, size := range benchSizes() {
		b.Run(size.name, func(b *testing.B) {
			response, _, _ := benchPage(b, size.n, size.note)
			b.ReportAllocs()
			for b.Loop() {
				body, err := response.AppendJSON(nil)
				if err != nil {
					b.Fatal(err)
				}
				_, _ = io.Discard.Write(append(body, '\n'))
			}
		})
	}
}

// BenchmarkSyncResponseHTTPLegacy is the body as writeJSON wrote it
// before AppendJSON: json.Encoder over MarshalJSON.
func BenchmarkSyncResponseHTTPLegacy(b *testing.B) {
	for _, size := range benchSizes() {
		b.Run(size.name, func(b *testing.B) {
			response, _, _ := benchPage(b, size.n, size.note)
			b.ReportAllocs()
			for b.Loop() {
				if err := json.NewEncoder(io.Discard).Encode(response); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
