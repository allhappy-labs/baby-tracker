package authority_test

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
)

// A Request never prints or logs its digests (session lookup keys): its
// print and log forms show the key IDs and which hooks are set (Task 4
// review m5).
//
// The digest bytes (0x5a, decimal 90) are looked for in every encoding a
// print or log could give them: hex, a decimal byte list ("90 90" or
// "90,90"), and standard or URL base64. The log line drops its timestamp,
// and no check is a bare "90" substring, which a timestamp or any other
// number may contain (the Task 11 review m4 flake).
func TestRequestNeverPrintsItsDigests(t *testing.T) {
	var sum [32]byte
	for i := range sum {
		sum[i] = 0x5a
	}
	req := authority.Request{Digests: []authority.KeyedDigest{{KeyID: "session.v2", Sum: sum}, {KeyID: "session.v1", Sum: sum}},
		Admit: func(access.Principal) error { return nil }}
	var logs bytes.Buffer
	withoutTime := func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) == 0 && a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}
	slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{ReplaceAttr: withoutTime})).Info("request", slog.Any("request", req))
	forbidden := []string{hex.EncodeToString(sum[:4]), strings.ToUpper(hex.EncodeToString(sum[:4])), "90 90", "90,90", "90, 90",
		base64.StdEncoding.EncodeToString(sum[:6]), base64.URLEncoding.EncodeToString(sum[:6])}
	for _, out := range []string{fmt.Sprint(req), fmt.Sprintf("%v %+v %#v %s %x %X", req, req, req, req, req, req), fmt.Sprintf("%v", &req), logs.String()} {
		for _, digest := range forbidden {
			if strings.Contains(out, digest) {
				t.Fatalf("printed %q, which holds the digest as %q", out, digest)
			}
		}
		if !strings.Contains(out, "session.v2") || !strings.Contains(out, "session.v1") {
			t.Fatalf("printed %q, without the key IDs", out)
		}
	}
	if strings.Contains(logs.String(), `"time"`) {
		t.Fatalf("log line %q keeps its timestamp", logs.String())
	}
}
