//go:build identitytest

package capacity

import (
	"babytracker/server/internal/auth"
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestOwnedConnectRouteRejectsTargetsAndCancelsActiveTunnel(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		c, e := backend.Accept()
		if e == nil {
			defer c.Close()
			_, _ = io.Copy(c, c)
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	route, err := StartConnectRoute(ctx, backend.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer route.Close()
	dial := func(target string) (net.Conn, *bufio.Reader, int) {
		c, e := net.Dial("tcp", route.Address())
		if e != nil {
			t.Fatal(e)
		}
		c.SetDeadline(time.Now().Add(3 * time.Second))
		fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		r := bufio.NewReader(c)
		response, e := http.ReadResponse(r, nil)
		if e != nil {
			t.Fatal(e)
		}
		return c, r, response.StatusCode
	}
	wrong, _, status := dial("other.test:443")
	wrong.Close()
	if status != 403 {
		t.Fatal("wrong target accepted")
	}
	good, reader, status := dial("sync.example.test:443")
	defer good.Close()
	if status != 200 {
		t.Fatal(status)
	}
	fmt.Fprint(good, "hello")
	buffer := make([]byte, 5)
	if _, err = io.ReadFull(reader, buffer); err != nil || string(buffer) != "hello" {
		t.Fatal("tunnel failed", err)
	}
	cancel()
	route.Close()
	if _, err = reader.ReadByte(); err == nil {
		t.Fatal("cancellation left tunnel alive")
	}
	if _, err = StartConnectRoute(t.Context(), "192.0.2.1:443"); err == nil {
		t.Fatal("nonloopback target accepted")
	}
}

func TestSwiftWireSessionPreservesRefreshedContinuity(t *testing.T) {
	d := FixtureDevice{DeviceCredential: "continuity", Session: auth.SessionBundle{AccessToken: "fresh", RefreshToken: "rotated", DeviceID: "device", FamilySlot: &auth.FamilySlot{FamilyID: "family", State: "active"}}}
	wire := swiftWireSession(d)
	if wire.AccessToken != "fresh" || wire.RefreshToken != "rotated" || wire.DeviceCredential != "continuity" || wire.FamilySlot.FamilyID != "family" {
		t.Fatal("handoff lost refreshed session or continuity")
	}
}

func TestSwiftProofRequiresPairsAndMarkersNotJustExitOrSummary(t *testing.T) {
	row := parseSwiftProof([]byte(`foreground-latency sample: {"count":200,"durable_p95_seconds":0.1}
foreground-latency proof: healthy complete
`))
	if row.Status != "incomplete" || row.Sample["durable_p95_seconds"] != nil {
		t.Fatal("summary or empty exit became durable proof")
	}
}

func swiftProofFixture(count int, distribution string) string {
	var b strings.Builder
	for i := 1; i <= count; i++ {
		fmt.Fprintf(&b, "foreground-latency pair: {\"distribution\":%q,\"ordinal\":%d,\"durable_seconds\":2,\"observable_seconds\":2,\"publication_seconds\":2}\n", distribution, i)
	}
	fmt.Fprintf(&b, "foreground-latency sample: {\"distribution\":%q,\"count\":%d,\"expected\":%d,\"elapsed_seconds\":110,\"durable_p50_seconds\":2,\"durable_p95_seconds\":2,\"durable_p99_seconds\":2,\"observable_p95_seconds\":2,\"publication_p95_seconds\":2}\n", distribution, count, count)
	b.WriteString("foreground-latency proof: healthy complete\nforeground-latency proof: capacity HTTP native-WSS trust and cancellation controls complete\nforeground-latency proof: isolation families=2 measured_sockets=2 cross_family_records=0\nforeground-latency proof: capacity lifecycle complete\n")
	return b.String()
}
func TestSwiftProofRejectsIncoherentSummaries(t *testing.T) {
	valid := swiftProofFixture(200, "healthy")
	if got := parseSwiftProof([]byte(valid)); got.Status != "complete" {
		t.Fatalf("valid proof: %+v", got)
	}
	for name, raw := range map[string]string{
		"contradiction":        strings.Replace(valid, `"durable_p95_seconds":2`, `"durable_p95_seconds":0.01`, 1),
		"duplicate field":      strings.Replace(valid, `"count":200`, `"count":3,"count":200`, 1),
		"missing percentile":   strings.Replace(valid, `"durable_p95_seconds":2,`, "", 1),
		"malformed summary":    valid + "foreground-latency sample: {broken\n",
		"duplicate summary":    valid + `foreground-latency sample: {"count":200}` + "\n",
		"wrong distribution":   strings.ReplaceAll(valid, "healthy", "other"),
		"wrong expected":       strings.Replace(valid, `"expected":200`, `"expected":3`, 1),
		"missing distribution": strings.Replace(valid, `"distribution":"healthy",`, "", 1),
	} {
		t.Run(name, func(t *testing.T) {
			got := parseSwiftProof([]byte(raw))
			if got.Status != "incomplete" || got.Sample["durable_p95_seconds"] != nil {
				t.Fatal("incoherent proof promoted", got.Status, got.Sample)
			}
		})
	}
}
func TestSwiftLifecycleRequiresCoherentRowsAndAllMarkers(t *testing.T) {
	valid := swiftProofFixture(3, "capacity_lifecycle")
	for name, raw := range map[string]string{
		"valid":              valid,
		"malformed row":      strings.Replace(valid, `"durable_seconds":2`, `"durable_seconds":"bad"`, 1),
		"missing field":      strings.Replace(valid, `"publication_seconds":2`, `"unknown":2`, 1),
		"negative":           strings.ReplaceAll(valid, `"durable_seconds":2`, `"durable_seconds":-2`),
		"missing native":     strings.ReplaceAll(valid, "capacity HTTP native-WSS trust and cancellation controls complete", "missing"),
		"missing isolation":  strings.ReplaceAll(valid, "isolation families=2 measured_sockets=2 cross_family_records=0", "missing"),
		"missing lifecycle":  strings.ReplaceAll(valid, "capacity lifecycle complete", "missing"),
		"missing summary":    strings.Join(strings.Split(valid, "\n")[0:3], "\n") + "\nforeground-latency proof: capacity lifecycle complete\n",
		"wrong distribution": strings.ReplaceAll(valid, "capacity_lifecycle", "healthy"),
	} {
		t.Run(name, func(t *testing.T) {
			got := parseSwiftProof([]byte(raw))
			want := "incomplete"
			if name == "valid" {
				want = "lifecycle_complete"
			}
			if got.Status != want || got.Sample["durable_p95_seconds"] != nil {
				t.Fatal("lifecycle eligibility", got.Status, got.Sample)
			}
		})
	}
}

func TestSwiftProcessFailureAndModeMismatchKeepPercentilesNull(t *testing.T) {
	for _, tc := range []struct {
		mode string
		err  error
	}{{"latency", fmt.Errorf("exit failure")}, {"lifecycle", nil}} {
		result := finishSwiftProof([]byte(swiftProofFixture(200, "healthy")), tc.mode, tc.err)
		if result.Status != "incomplete" || result.Sample["durable_p95_seconds"] != nil {
			t.Fatal("failed/mismatched process retained acceptance percentiles", result.Status, result.Sample)
		}
	}
}
