package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"babytracker/server/internal/protocol"
)

// writeJSON writes a sync response through its direct encoder with the
// same status, headers and bytes json.Encoder wrote: an encoded page, a
// struct page (which AppendJSON marshals), and a page AppendJSON cannot
// encode, which falls back to the encoder and writes no body, as before.
func TestWriteJSONWritesWhatTheEncoderWrote(t *testing.T) {
	token := "next<&>"
	// A stored wire is json.Marshal output (care.Encode).
	wire, err := json.Marshal(map[string]string{"note": "<b> " + string(rune(0x2028)) + " " + string(rune(0xe9))})
	if err != nil {
		t.Fatal(err)
	}
	change, err := protocol.EncodeChange(protocol.Cursor{ServerInstanceID: "s", RecoveryEpoch: 1, FamilyID: "f", Sequence: 7}, "diaper",
		"id", 2, "a", "d", "create", "2026-10-25T10:00:00.000000Z", wire)
	if err != nil {
		t.Fatal(err)
	}
	high := protocol.Cursor{ServerInstanceID: "s", RecoveryEpoch: 1, FamilyID: "f", Sequence: 9}
	for _, c := range []struct {
		name     string
		response protocol.SyncResponse
		body     bool
	}{
		{"encoded page", protocol.SyncResponse{ServerInstanceID: "s", RecoveryEpoch: 1, Outcomes: []protocol.Outcome{},
			EncodedChanges: []json.RawMessage{change, change}, HighWater: high, NextPageToken: &token}, true},
		{"struct page", protocol.SyncResponse{ServerInstanceID: "s", RecoveryEpoch: 1, Outcomes: []protocol.Outcome{}, HighWater: high}, true},
		{"unencodable", protocol.SyncResponse{EncodedChanges: []json.RawMessage{json.RawMessage(`{`)}}, false},
	} {
		var want bytes.Buffer
		_ = json.NewEncoder(&want).Encode(c.response)
		recorder := httptest.NewRecorder()
		writeJSON(recorder, http.StatusAccepted, c.response)
		if recorder.Code != http.StatusAccepted || recorder.Header().Get("Content-Type") != "application/json; charset=utf-8" ||
			!bytes.Equal(recorder.Body.Bytes(), want.Bytes()) || (recorder.Body.Len() > 0) != c.body {
			t.Fatalf("%s: %d %q %q, want 202 and %q", c.name, recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.Bytes(),
				want.Bytes())
		}
	}
}
