package capacity

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Removing manifest version, input, count or credential-deadline validation must fail these cases.
func TestFixtureManifestRejectsMismatch(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	spec := FixtureSpec{Families: 1, DevicesPerFamily: 2, AuthorityEvents: 1, CareEntriesPerBaby: 2}
	source := strings.Repeat("a", 40)
	valid := FixtureManifest{Version: 1, SourceSHA: source, InputSHA256: fixtureInputDigest(spec, source), Families: 1, Devices: 2, ActualAuthorityEvents: 5, CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}
	for _, name := range []string{"version", "digest", "expired", "source", "counts", "head", "future"} {
		t.Run(name, func(t *testing.T) {
			m := valid
			switch name {
			case "version":
				m.Version = 2
			case "digest":
				m.InputSHA256 = strings.Repeat("0", 64)
			case "expired":
				m.ExpiresAt = now
			case "source":
				m.SourceSHA = strings.Repeat("b", 40)
			case "counts":
				m.Devices = 3
			case "head":
				m.ActualAuthorityEvents = 0
			case "future":
				m.CreatedAt = now.Add(time.Minute)
			}
			if err := m.Validate(spec, source, now); err == nil {
				t.Fatal("accepted mismatched or expired fixture")
			}
		})
	}
	if err := valid.Validate(spec, source, now); err != nil {
		t.Fatal(err)
	}
}

// Removing path ownership checks could overwrite a database or follow a user symlink.
func TestFixtureRefusesNonDisposableTarget(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(root, "existing")
	if err := os.Mkdir(existing, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", ".", "/", root, existing, link, filepath.Join(link, "child"), "postgres://production/database"} {
		if err := disposableDestination(path); err == nil {
			t.Errorf("accepted non-disposable target %q", path)
		}
	}
	if err := disposableDestination(filepath.Join(root, "fresh")); err != nil {
		t.Fatal(err)
	}
}

// The shipped untagged binary must never provision synthetic identities.
func TestUntaggedCapacityClientRefuses(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "capacity-client")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../../cmd/sync-capacity-client")
	if raw, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build refusal binary: %v %s", err, raw)
	}
	output, err := exec.CommandContext(t.Context(), binary, "fixture", "--destination", filepath.Join(t.TempDir(), "fixture")).CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || strings.Contains(string(output), "AccessToken") {
		t.Fatalf("untagged command failed to refuse: err=%v", err)
	}
}
