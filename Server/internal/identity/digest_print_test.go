package identity

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// A keyed digest is a session lookup key: no print form and no slog value
// shows its sum, only its key ID (Task 4 review m5).
func TestKeyedDigestNeverPrintsItsSum(t *testing.T) {
	var sum [32]byte
	for i := range sum {
		sum[i] = 0xa5
	}
	d := KeyedDigest{KeyID: "session.v1", Sum: sum}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("digest", slog.Any("digest", d), slog.String("text", fmt.Sprint([]KeyedDigest{d})))
	printed := []string{fmt.Sprint(d), fmt.Sprintf("%v %+v %#v %s", d, d, d, d), fmt.Sprintf("%v %+v", []KeyedDigest{d}, []KeyedDigest{d}),
		fmt.Sprintf("%v", &d), logs.String()}
	for _, out := range printed {
		if strings.Contains(out, hex.EncodeToString(sum[:])) || strings.Contains(out, "165") || strings.Contains(out, "a5a5") || !strings.Contains(out, "session.v1") {
			t.Fatalf("printed %q", out)
		}
	}
}

// encoding/json refuses a keyed digest, so serializing one fails loudly
// instead of writing its sum, and slog's JSON handler (the production log)
// writes an error marker where a slice of them would have printed the sums,
// which LogValue does not reach.
func TestKeyedDigestRefusesJSON(t *testing.T) {
	var sum [32]byte
	for i := range sum {
		sum[i] = 0xa5
	}
	d := KeyedDigest{KeyID: "session.v1", Sum: sum}
	for _, value := range []any{d, &d, []KeyedDigest{d}, struct{ D KeyedDigest }{d}} {
		if raw, err := json.Marshal(value); err == nil {
			t.Fatalf("json.Marshal(%T) = %s, want an error", value, raw)
		}
	}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("digests", slog.Any("digests", []KeyedDigest{d}))
	if out := logs.String(); strings.Contains(out, "165") || strings.Contains(out, "a5a5") || !strings.Contains(out, "!ERROR") {
		t.Fatalf("logged %q", out)
	}
}
