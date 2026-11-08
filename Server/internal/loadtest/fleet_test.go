package loadtest

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFleetRoundTripIsPrivateAndAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "fleet.jsonl")
	h := FleetHeader{Version: 1, AsOf: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), Families: 1, Devices: 1}
	in := []DeviceState{{FamilyID: "f", DeviceID: "d", AccessToken: "a", RefreshToken: "r", ClientIP: "100.64.0.1"}}
	if err := WriteFleet(path, h, in); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	gotH, out, err := ReadFleet(path)
	if err != nil || gotH.Devices != 1 || !reflect.DeepEqual(in, out) {
		t.Fatalf("%v %+v", err, out)
	}
	// Nothing but the fleet is left behind: the temporary file was renamed.
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("directory holds %d entries (%v)", len(entries), err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteFleet(path, h, in); err == nil {
		t.Fatal("wrote credentials into a group-readable directory")
	}
}

func TestStaleSeedIsRefused(t *testing.T) {
	h := FleetHeader{AsOf: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	if err := h.CheckFresh(h.AsOf.Add(29*24*time.Hour+time.Hour), 30*24*time.Hour); err == nil {
		t.Fatal("a seed within 24 h of its lineage age was accepted")
	}
	if err := h.CheckFresh(h.AsOf.Add(48*time.Hour), 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
}

func TestReadFleetRefusesAMismatchedOrReadableFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "fleet.jsonl")
	h := FleetHeader{Version: 1, AsOf: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), Families: 1, Devices: 2}
	in := []DeviceState{{FamilyID: "f", DeviceID: "d1"}, {FamilyID: "f", DeviceID: "d2"}}
	if err := WriteFleet(path, h, in); err != nil {
		t.Fatal(err)
	}
	// A header that does not count its devices is refused.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(raw), "\n")
	if err := os.WriteFile(path, []byte(strings.Join(lines[:2], "")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadFleet(path); err == nil {
		t.Fatal("a fleet missing a device was read")
	}
	if err := WriteFleet(path, h, in); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadFleet(path); err == nil {
		t.Fatal("a world-readable fleet was read")
	}
	if err := WriteFleet(path, FleetHeader{Version: 1, Devices: 1}, in); err == nil {
		t.Fatal("a header that miscounts its devices was written")
	}
}

func TestLiveActivityInstallationIDIsAStableUUIDPerDevice(t *testing.T) {
	a := LiveActivityInstallationID("d0000000-0000-4000-8000-0000000000d1")
	if a != LiveActivityInstallationID("d0000000-0000-4000-8000-0000000000d1") || a == LiveActivityInstallationID("d0000000-0000-4000-8000-0000000000d2") {
		t.Fatal("the installation ID is not a function of the device")
	}
	if len(a) != 36 || a[14] != '4' || !strings.ContainsAny(a[19:20], "89ab") {
		t.Fatalf("installation ID %q is not a UUIDv4", a)
	}
}

// A paced write (a run's minute checkpoint) syncs a mebibyte at a time and
// waits between, and writes exactly what WriteFleet writes.
func TestPacedFleetWriteMatchesAndPaces(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const n = 30000 // about 3.5 MiB
	h := FleetHeader{Version: FleetVersion, AsOf: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC), Families: 1, Devices: n}
	in := make([]DeviceState, n)
	for i := range in {
		in[i] = DeviceState{FamilyID: "f", DeviceID: strings.Repeat("d", 36), AccessToken: strings.Repeat("a", 40), RefreshToken: strings.Repeat("r", 40)}
	}
	plain, paced := filepath.Join(dir, "plain.jsonl"), filepath.Join(dir, "paced.jsonl")
	if err := WriteFleet(plain, h, in); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := writeFleet(paced, h, in, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	a, _ := os.ReadFile(plain)
	b, _ := os.ReadFile(paced)
	if len(a) < 3*fleetPaceChunk || string(a) != string(b) {
		t.Fatalf("paced file differs (%d vs %d bytes)", len(a), len(b))
	}
	if chunks := len(a) / fleetPaceChunk; took < time.Duration(chunks)*50*time.Millisecond {
		t.Fatalf("%d chunks written in %v, want at least %v", chunks, took, time.Duration(chunks)*50*time.Millisecond)
	}
}
