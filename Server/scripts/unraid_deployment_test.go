package scripts

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	if tailscale.Image != "tailscale/tailscale:v1.98.3@sha256:854b77123b9536adae2e97f5a5fdb1790ed03438b911ab7f07780155e0af6ce2" {
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

	for _, volume := range []string{"database", "security_ledger", "ledger_head"} {
		device := compose.Volumes[volume].DriverOpts["device"]
		if !strings.HasPrefix(device, "${BT_DEPLOY_ROOT:?set BT_DEPLOY_ROOT}/") {
			t.Errorf("%s is not rooted below BT_DEPLOY_ROOT: %q", volume, device)
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
	for _, required := range []string{"baby-sync.example-tailnet.ts.net:443", "http://baby-tracker-sync-app-1:8080", `"HTTPS": true`} {
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
	for _, name := range []string{"provision-unraid-secrets.sh", "provision-live-activity-secrets.sh", "verify-unraid-deployment.sh"} {
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
