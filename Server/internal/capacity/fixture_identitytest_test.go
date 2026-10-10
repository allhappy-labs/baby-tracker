//go:build identitytest

package capacity

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A natural command history cannot be truncated to satisfy a smaller requested head.
func TestFixtureNaturalHeadExceedsRequest(t *testing.T) {
	spec := FixtureSpec{Families: 1, DevicesPerFamily: 2, AuthorityEvents: 1, CareEntriesPerBaby: 2}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "fixture")
	manifest, err := BuildFixture(t.Context(), spec, directory)
	if err != nil {
		t.Fatal(err)
	}
	// Two sign-ins, a family creation and two refreshes append more audit
	// rows than the one requested.
	if manifest.ActualAuthorityEvents <= uint64(spec.AuthorityEvents) || manifest.Families != 1 || manifest.Devices != 2 {
		t.Fatalf("natural head/counts not recorded: %+v", manifest)
	}
	original, err := fixtureArtifactDigests(directory)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		restored, err := RestoreFixture(t.Context(), directory, filepath.Join(root, "restored"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := restored.Close(); err != nil {
				t.Error(err)
			}
		})
		summary, err := json.Marshal(restored)
		if err != nil {
			t.Fatal("restored metadata cannot be safely summarized")
		}
		for _, device := range restored.Devices {
			if bytes.Contains(summary, []byte(device.Session.AccessToken)) || bytes.Contains(summary, []byte(device.Session.RefreshToken)) {
				t.Fatal("restored JSON summary leaks secret")
			}
		}
		if restored.RestoredAuthorityEvents != manifest.ActualAuthorityEvents+2 {
			t.Fatal("restore did not record its real refresh transitions")
		}
		for _, device := range restored.Devices {
			principal, err := restored.Auth.Authenticate(t.Context(), device.Session.AccessToken)
			if err != nil {
				t.Fatal("restored credential denied")
			}
			view, err := restored.Families.Current(t.Context(), principal)
			if err != nil || view.ID != device.FamilyID {
				t.Fatal("restored family access mismatch")
			}
		}
		if err := restored.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(root, "restored")); !os.IsNotExist(err) {
			t.Fatal("restored private material remains")
		}
	}
	current, err := fixtureArtifactDigests(directory)
	if err != nil {
		t.Fatal(err)
	}
	for path, digest := range original {
		if current[path] != digest {
			t.Fatal("restore changed sealed artifact")
		}
	}
	seal, private, err := loadFixture(directory)
	if err != nil {
		t.Fatal(err)
	}
	public, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, device := range private.Devices {
		for _, secret := range []string{device.Session.AccessToken, device.Session.RefreshToken, device.DeviceCredential} {
			if secret != "" && bytes.Contains(public, []byte(secret)) {
				t.Fatal("public manifest contains secret")
			}
		}
	}
	// An altered dump (the whole authority lives in it) must fail before
	// creating any restore.
	head := filepath.Join(directory, "database.dump")
	headRaw, err := os.ReadFile(head)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(head, append(append([]byte(nil), headRaw...), 0), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreFixture(t.Context(), directory, filepath.Join(root, "corrupt")); err == nil {
		t.Fatal("corrupt sealed authority accepted")
	}
	if _, err := os.Lstat(filepath.Join(root, "corrupt")); !os.IsNotExist(err) {
		t.Fatal("corrupt restore created destination")
	}
	if err := os.WriteFile(head, headRaw, 0600); err != nil {
		t.Fatal(err)
	}
	// Private material must remain inaccessible to other users.
	privatePath := filepath.Join(directory, "private.json")
	if err := os.Chmod(privatePath, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadFixture(directory); err == nil {
		t.Fatal("publicly readable private material accepted")
	}
	if err := os.Chmod(privatePath, 0600); err != nil {
		t.Fatal(err)
	}
	// Manifest expiration must deny re-use even while the actual session remains live.
	seal.Manifest.ExpiresAt = seal.Manifest.CreatedAt
	if err := writeFixtureJSON(filepath.Join(directory, "seal.json"), seal); err != nil {
		t.Fatal(err)
	}
	if err := writeFixtureJSON(filepath.Join(directory, "manifest.json"), seal.Manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreFixture(t.Context(), directory, filepath.Join(root, "expired")); err == nil {
		t.Fatal("expired fixture accepted")
	}
}

func TestFixtureBuildRefusesAmbientRemoteDocker(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_HOST", "ssh://production.invalid")
	path := filepath.Join(root, "fixture")
	if _, err := BuildFixture(t.Context(), FixtureSpec{Families: 1, DevicesPerFamily: 2}, path); err == nil {
		t.Fatal("remote daemon accepted")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("failed fixture private material remains")
	}
}

func TestFixtureCommandDoesNotPrintSecrets(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "capacity-client")
	if raw, err := exec.CommandContext(t.Context(), "go", "build", "-tags", "identitytest", "-o", binary, "../../cmd/sync-capacity-client").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, raw)
	}
	path := filepath.Join(root, "fixture")
	output, err := exec.CommandContext(t.Context(), binary, "fixture", "--destination", path, "--authority-events", "1").CombinedOutput()
	if err != nil {
		t.Fatal("fixture command failed")
	}
	_, private, err := loadFixture(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, device := range private.Devices {
		for _, secret := range []string{device.Session.AccessToken, device.Session.RefreshToken, device.DeviceCredential} {
			if secret != "" && bytes.Contains(output, []byte(secret)) {
				t.Fatal("stdout contains secret")
			}
		}
	}
	for _, key := range []string{"AccessToken", "RefreshToken", "DeviceCredential", "Secret", "ActorIDPepper"} {
		if strings.Contains(string(output), key) {
			t.Fatal("stdout includes private field")
		}
	}
	var manifest FixtureManifest
	// The natural audit history (sign-ins, family, refreshes) exceeds the one
	// requested authority event.
	if json.Unmarshal(output, &manifest) != nil || manifest.ActualAuthorityEvents < 2 {
		t.Fatal("stdout missing valid safe manifest")
	}
}

func TestFixtureSetupPoolsCloseForProduction(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	fixture := filepath.Join(parent, "fixture")
	manifest, err := BuildFixture(ctx, FixtureSpec{Families: 1, DevicesPerFamily: 2, AuthorityEvents: 5, MCPGrantsPerFamily: 2}, fixture)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreFixture(ctx, fixture, filepath.Join(parent, "restore"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restored.Close(); err != nil {
			t.Error(err)
		}
	})
	// Grants are part of the sealed fixture: every matched cell restored from
	// it shares the same grant and authority history, and none leaks.
	if manifest.MCPGrants != 2 || len(restored.Integrations) != 2 || restored.Integrations[0].FamilyID != restored.Devices[0].FamilyID ||
		restored.Integrations[0].BabyID != restored.Devices[0].BabyID || restored.Integrations[0].Credential == restored.Integrations[1].Credential {
		t.Fatalf("fixture grants: manifest=%d restored=%d", manifest.MCPGrants, len(restored.Integrations))
	}
	summary, err := json.Marshal(restored)
	if err != nil || bytes.Contains(summary, []byte(restored.Integrations[0].Credential)) {
		t.Fatal("restored summary leaks an MCP credential")
	}
	pools := append([]*pgxpool.Pool(nil), restored.db.pools...)
	restored.CloseSetupPools()
	if len(restored.db.pools) != 0 || restored.Runtime != nil || restored.Maintenance != nil || restored.Authority != nil || restored.Auth != nil || restored.Sync != nil || restored.identity != nil {
		t.Fatal("setup services survive measurement handoff")
	}
	for _, pool := range pools {
		if pool.Stat().TotalConns() != 0 {
			t.Fatal("setup pool retains physical database connections")
		}
	}
	if restored.Config.DatabaseURL == "" || restored.db.id == "" {
		t.Fatal("owned production handoff material missing")
	}
}

func TestOwnershipReservedBeforeAnyPrivateMaterial(t *testing.T) {
	control := filepath.Join(t.TempDir(), "control")
	if err := os.Mkdir(control, 0700); err != nil {
		t.Fatal(err)
	}
	control, _ = filepath.EvalSymlinks(control)
	t.Setenv("BT_CAPACITY_CONTROL_DIR", control)
	destination := filepath.Join(filepath.Dir(control), "unused-fixture")
	db, err := reservePostgres(destination, "")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(db.receiptPath)
	if _, err = os.Lstat(db.root); !os.IsNotExist(err) {
		t.Fatal("config root created before receipt reservation")
	}
	var receipt resourceReceipt
	if err = readFixtureJSON(db.receiptPath, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Root != db.root || receipt.FixtureRoot != destination {
		t.Fatal("ownership missing before creation")
	}
}
