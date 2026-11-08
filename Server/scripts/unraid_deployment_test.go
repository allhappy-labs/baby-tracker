package scripts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestUnraidDeploymentIsIsolatedAndPinned(t *testing.T) {
	encoded, err := os.ReadFile("../deploy/unraid/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var compose struct {
		Services map[string]struct {
			Image       string            `yaml:"image"`
			NetworkMode string            `yaml:"network_mode"`
			Environment map[string]string `yaml:"environment"`
			Volumes     []string          `yaml:"volumes"`
			Profiles    []string          `yaml:"profiles"`
			CapAdd      []string          `yaml:"cap_add"`
			Devices     []string          `yaml:"devices"`
			Ports       []string          `yaml:"ports"`
			Tmpfs       []string          `yaml:"tmpfs"`
			Restart     string            `yaml:"restart"`
		} `yaml:"services"`
		Volumes map[string]struct {
			DriverOpts map[string]string `yaml:"driver_opts"`
		} `yaml:"volumes"`
	}
	if err := yaml.Unmarshal(encoded, &compose); err != nil {
		t.Fatal(err)
	}

	tailscale := compose.Services["tailscale"]
	if tailscale.Image != "tailscale/tailscale:v1.104.4@sha256:ed6480b33c6eb67c9cbd203ed42b9fb57e361986725b6856def71b7a3d9e276c" {
		t.Fatalf("tailscale image is not immutable: %q", tailscale.Image)
	}
	for key, want := range map[string]string{
		"TS_HOSTNAME":            "baby-sync",
		"TS_STATE_DIR":           "/var/lib/tailscale",
		"TS_SERVE_CONFIG":        "/config/serve.json",
		"TS_ENABLE_HEALTH_CHECK": "true",
		"TS_USERSPACE":           "false",
	} {
		if got := tailscale.Environment[key]; got != want {
			t.Errorf("tailscale %s=%q, want %q", key, got, want)
		}
	}
	if tailscale.Restart != "unless-stopped" || !containsString(tailscale.CapAdd, "NET_ADMIN") || !containsString(tailscale.Devices, "/dev/net/tun:/dev/net/tun") {
		t.Fatalf("tailscale lifecycle/kernel boundary is incomplete: %+v", tailscale)
	}
	if !containsString(tailscale.Tmpfs, "/run:size=1m,mode=0755") {
		t.Fatalf("tailscale cannot create its xtables lock on read-only /run: %v", tailscale.Tmpfs)
	}
	if !containsString(tailscale.Volumes, "${BT_DEPLOY_ROOT:?set BT_DEPLOY_ROOT}/tailscale-state:/var/lib/tailscale") ||
		!containsString(tailscale.Volumes, "./deploy/unraid/tailscale:/config:ro") {
		t.Fatalf("tailscale state/config mounts are not project-scoped: %v", tailscale.Volumes)
	}

	app := compose.Services["app"]
	if app.NetworkMode != "" || app.Environment["BT_LISTEN_ADDRESS"] != ":8080" {
		t.Fatalf("app must remain on the isolated project network: network=%q listen=%q", app.NetworkMode, app.Environment["BT_LISTEN_ADDRESS"])
	}
	if app.Environment["BT_SOCKET_DEVICE_LIMIT"] != "2" {
		t.Fatalf("a phone needs a second live socket slot while its dead one times out: BT_SOCKET_DEVICE_LIMIT=%q", app.Environment["BT_SOCKET_DEVICE_LIMIT"])
	}
	if len(app.Ports) != 0 {
		t.Fatalf("app publishes host ports: %v", app.Ports)
	}

	if device := compose.Volumes["database"].DriverOpts["device"]; !strings.HasPrefix(device, "${BT_DEPLOY_ROOT:?set BT_DEPLOY_ROOT}/") {
		t.Errorf("database is not rooted below BT_DEPLOY_ROOT: %q", device)
	}
	// The legacy file ledger's volumes went with the importer (Phase 4
	// Task 29): the cutover deleted the homelab's directories (R7).
	for volume := range compose.Volumes {
		if volume != "database" {
			t.Errorf("compose declares volume %q: only database remains", volume)
		}
	}

	enroll := compose.Services["tailscale-enroll"]
	if !containsString(enroll.Profiles, "enroll") || enroll.NetworkMode != "service:tailscale" {
		t.Fatalf("one-shot enrollment is not isolated behind a profile: %+v", enroll)
	}
	text := string(encoded)
	for _, required := range []string{"ports: !reset []", "tailscale_auth_key", "file:/run/secrets/tailscale_auth_key", "tag:baby-sync", "BT_SOURCE_SHA"} {
		if !strings.Contains(text, required) {
			t.Errorf("compose omits %q", required)
		}
	}
}

// TestUnraidPushOverlayCarriesNoLiveActivityWorkerLogin pins push.yaml, the
// overlay that replaces main's live-activity-push.yaml at the cutover: the
// new server refuses BT_LIVE_ACTIVITY_DATABASE_URL and
// BT_LIVE_ACTIVITY_DATABASE_URL_FILE at startup (config.LoadLiveActivity,
// Task 7), its worker runs on the maintenance credential, so the overlay
// mounts the one APNs key and nothing else, and names no live_activity_*
// secret.
func TestUnraidPushOverlayCarriesNoLiveActivityWorkerLogin(t *testing.T) {
	encoded, err := os.ReadFile("../deploy/unraid/push.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var overlay struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
			Secrets     []string          `yaml:"secrets"`
		} `yaml:"services"`
		Secrets map[string]struct {
			File string `yaml:"file"`
		} `yaml:"secrets"`
	}
	if err := yaml.Unmarshal(encoded, &overlay); err != nil {
		t.Fatal(err)
	}
	if len(overlay.Services) != 1 {
		t.Fatalf("push.yaml configures services %v; only app", unraidSortedKeys(overlay.Services))
	}
	app, ok := overlay.Services["app"]
	if !ok {
		t.Fatal("push.yaml does not configure app")
	}
	for _, refused := range []string{"BT_LIVE_ACTIVITY_DATABASE_URL", "BT_LIVE_ACTIVITY_DATABASE_URL_FILE"} {
		if _, set := app.Environment[refused]; set {
			t.Errorf("push.yaml sets %s, which the new server refuses at startup", refused)
		}
	}
	for key, want := range map[string]string{
		"BT_LIVE_ACTIVITY_PUSH_ENABLED": "true",
		"BT_APNS_WAKES_ENABLED":         "${BT_APNS_WAKES_ENABLED:-false}",
		"BT_APNS_TEAM_ID":               "${BT_APNS_TEAM_ID:?set BT_APNS_TEAM_ID}",
		"BT_APNS_KEY_ID":                "${BT_APNS_KEY_ID:?set BT_APNS_KEY_ID}",
		"BT_APNS_BUNDLE_ID":             "${BT_APNS_BUNDLE_ID:?set BT_APNS_BUNDLE_ID}",
		"BT_APNS_KEY_FILE":              "/run/secrets/apns_key",
	} {
		if got := app.Environment[key]; got != want {
			t.Errorf("push.yaml app %s=%q, want %q", key, got, want)
		}
	}
	if len(app.Environment) != 6 {
		t.Errorf("push.yaml app environment has %d keys, want 6: %v", len(app.Environment), unraidSortedKeys(app.Environment))
	}
	if len(app.Secrets) != 1 || app.Secrets[0] != "apns_key" {
		t.Errorf("push.yaml app mounts secrets %v; only apns_key", app.Secrets)
	}
	if len(overlay.Secrets) != 1 || overlay.Secrets["apns_key"].File != "${BT_DEPLOY_ROOT:?set BT_DEPLOY_ROOT}/secrets/apns_key.p8" {
		t.Errorf("push.yaml declares secrets %+v; only apns_key from the deploy root's secrets/apns_key.p8", overlay.Secrets)
	}
	text := string(encoded)
	if strings.Contains(text, "live_activity_") || strings.Contains(text, "DATABASE_URL") {
		t.Error("push.yaml names a Live Activity worker secret or a database URL")
	}
}

func unraidSortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func TestUnraidServeConfigTerminatesHTTPSOnlyForBabySync(t *testing.T) {
	encoded, err := os.ReadFile("../deploy/unraid/tailscale/serve.json")
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(encoded, &config); err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, required := range []string{"${TS_CERT_DOMAIN}:443", "http://baby-tracker-sync-app-1:8080", `"HTTPS": true`} {
		if !strings.Contains(text, required) {
			t.Errorf("serve config omits %q", required)
		}
	}
	// Unraid's rc.docker reconnects a container at boot to restore its MAC
	// and drops the compose service alias; only the container name survives.
	if strings.Contains(text, "http://app:") {
		t.Error("serve config must proxy to the container name, not the compose service alias")
	}
	if strings.Contains(text, "Funnel") || strings.Contains(text, "AllowFunnel") {
		t.Fatal("private deployment must not enable Funnel")
	}
}

func TestUnraidScriptsRefuseBroadOrForeignTargets(t *testing.T) {
	for _, name := range []string{"provision-unraid-secrets.sh", "verify-unraid-deployment.sh"} {
		path := filepath.Join(".", name)
		encoded, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(encoded)
		for _, required := range []string{"set -euo pipefail", "baby-tracker-sync", "BT_DEPLOY_ROOT"} {
			if !strings.Contains(text, required) {
				t.Errorf("%s omits %q", name, required)
			}
		}
		for _, forbidden := range []string{"docker system prune", "docker volume prune", "down --volumes", "rm -rf", `--filter "label!=`} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s contains broad/destructive operation %q", name, forbidden)
			}
		}
	}
}

func TestProvisionerRequiresExplicitReviewedSourceUpgrade(t *testing.T) {
	encoded, err := os.ReadFile("provision-unraid-secrets.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, required := range []string{
		"BT_ALLOW_SOURCE_UPDATE",
		`mktemp "$deploy_root/.deployment.env.XXXXXX"`,
		"!/^BT_SOURCE_SHA=/ && !/^BT_IMAGE_NAME=/",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("source upgrade guard omits %q", required)
		}
	}
}

// The provisioner creates no legacy file-ledger directory: nothing reads
// one since the cutover, which deleted the homelab's (R7).
func TestProvisionerCreatesNoLedgerDirectories(t *testing.T) {
	encoded, err := os.ReadFile("provision-unraid-secrets.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, retired := range []string{"security-ledger", "ledger-head"} {
		if strings.Contains(string(encoded), retired) {
			t.Errorf("provision-unraid-secrets.sh still creates %s", retired)
		}
	}
}

func TestUnraidVerifierAcceptsDockerEmptyPortBindingsObject(t *testing.T) {
	encoded, err := os.ReadFile("verify-unraid-deployment.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `[[ $bindings == null || $bindings == "{}" ]]`) {
		t.Fatal("verifier must treat Docker's empty PortBindings object as no published host ports")
	}
}

func TestUnraidVerifierUsesSidecarIPForPrivateTLS(t *testing.T) {
	encoded, err := os.ReadFile("verify-unraid-deployment.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, required := range []string{
		`tailscale ip -4`,
		`--resolve "$server_name:443:$tailscale_ip"`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("private TLS verification omits %q", required)
		}
	}
}

func TestUnraidVerifierDialsTheLiveSocketThroughServe(t *testing.T) {
	encoded, err := os.ReadFile("verify-unraid-deployment.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	// A refused bearer proves the upgrade reaches the app through Serve over
	// HTTP/1.1 with the headers the phones send, without a session.
	for _, required := range []string{
		`--http1.1`,
		`Upgrade: websocket`,
		`Sec-WebSocket-Protocol: baby-tracker.v1`,
		`X-Baby-Tracker-Capabilities: records-v1,live-hints-v1`,
		`"https://$server_name/v1/live") == 401 ]]`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("live socket verification omits %q", required)
		}
	}
}

func TestUnraidVerifierSnapshotsStableUnrelatedContainerState(t *testing.T) {
	encoded, err := os.ReadFile("verify-unraid-deployment.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{".State.StartedAt", `{{json .NetworkSettings.Networks}}`} {
		if strings.Contains(text, forbidden) {
			t.Errorf("unrelated-container baseline includes volatile field %q", forbidden)
		}
	}
	for _, required := range []string{`ports:.HostConfig.PortBindings`, `networks:(.NetworkSettings.Networks | keys | sort)`} {
		if !strings.Contains(text, required) {
			t.Errorf("stable unrelated-container baseline omits %q", required)
		}
	}
}

func TestUnraidVerifierRequiresThePushOverlay(t *testing.T) {
	encoded, err := os.ReadFile("verify-unraid-deployment.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	// The 670dbba5 cutover composed the app without push.yaml; the verifier
	// passed and Live Activity push stayed off until this check.
	for _, required := range []string{
		`com.docker.compose.project.config_files`,
		`source/Server/deploy/unraid/push.yaml`,
		`BT_LIVE_ACTIVITY_PUSH_ENABLED=true`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("push overlay verification omits %q", required)
		}
	}
}
