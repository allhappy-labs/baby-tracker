//go:build identitytest

package capacity

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"babytracker/server/internal/config"
)

const proxyImage = "nginx:1.27-alpine@sha256:65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10"

type resourceReceipt struct {
	Version       int      `json:"version"`
	Owner         string   `json:"owner"`
	Root          string   `json:"root"`
	FixtureRoot   string   `json:"fixture_root,omitempty"`
	FixtureSealed bool     `json:"fixture_sealed,omitempty"`
	RestoreRoot   string   `json:"restore_root,omitempty"`
	Containers    []string `json:"containers"`
	Networks      []string `json:"networks"`
}

func privateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return ErrUnsafeTarget
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) {
		return ErrUnsafeTarget
	}
	for p := path; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafeTarget
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	return nil
}
func writePrivateJSON(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path+".tmp", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(path + ".tmp")
	_, err = file.Write(raw)
	err = errors.Join(err, file.Sync(), file.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(path+".tmp", path); err != nil {
		return err
	}
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}
func (db *ownedPostgres) writeReceipt() error { return writePrivateJSON(db.receiptPath, db.receipt) }
func removeOwned(ctx context.Context, kind, name, owner string) error {
	args := []string{kind, "ls", "--quiet", "--filter", "label=" + ownedLabel + "=" + owner}
	if kind == "container" {
		args = []string{"ps", "--all", "--quiet", "--filter", "label=" + ownedLabel + "=" + owner}
	}
	raw, err := dockerOutput(ctx, args...)
	if err != nil {
		return errors.New("capacity_cleanup_verification_failed")
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	raw, err = dockerOutput(ctx, kind, "inspect", "--format", `{{index .Labels "`+ownedLabel+`"}}`, name)
	if kind == "container" {
		raw, err = dockerOutput(ctx, "inspect", "--format", `{{index .Config.Labels "`+ownedLabel+`"}}`, name)
	}
	if err != nil || strings.TrimSpace(string(raw)) != owner {
		return errors.New("capacity_cleanup_ownership_failed")
	}
	args = []string{kind, "rm", name}
	if kind == "container" {
		args = []string{"rm", "--force", "--volumes", name}
	}
	_, err = dockerOutput(ctx, args...)
	return err
}

// CloseSetupPools is the deliberate lifecycle seam. No setup client/module is
// retained for measured HTTP traffic. Owned container/config/private state stay.
func (r *RestoredFixture) CloseSetupPools() {
	for _, pool := range r.db.pools {
		pool.Close()
	}
	r.db.pools = nil
	r.Runtime = nil
	r.Maintenance = nil
	r.Authority = nil
	r.Families = nil
	r.Auth = nil
	r.Sync = nil
	r.identity = nil
	r.mcpReader = nil
}
func (db *ownedPostgres) own(ctx context.Context, kind, name string, args ...string) error {
	if kind == "network" {
		db.receipt.Networks = append(db.receipt.Networks, name)
	} else {
		db.receipt.Containers = append(db.receipt.Containers, name)
	}
	if err := db.writeReceipt(); err != nil {
		return err
	}
	_, err := dockerOutput(ctx, args...)
	return err
}
func certificate(root string) (*x509.CertPool, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	template := x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "sync.example.test"}, DNSNames: []string{"sync.example.test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err = os.WriteFile(filepath.Join(root, "tls.crt"), cert, 0600); err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(root, "tls.key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}), 0600); err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(cert)
	return roots, nil
}
func (r *RestoredFixture) PrepareWorkload(ctx context.Context, w Workload, image, profile, output string) (Workload, error) {
	if err := w.Validate(r.Manifest); err != nil {
		return w, err
	}
	if image == "" || strings.ContainsAny(image, " \n") || profile != "standard" && profile != "small" || privateDirectory(output) != nil {
		return w, ErrUnsafeTarget
	}
	poolEnv, poolBudget, err := databaseProfile(w.DatabaseProfile)
	if err != nil {
		return w, err
	}
	databaseMode := w.DatabaseProfile
	if databaseMode == "" {
		databaseMode = "legacy"
	}
	db := r.db
	root := db.root
	// The production executable checks its exact isolated maintenance login.
	if len(db.pools) == 0 {
		return w, ErrFixtureInvalid
	}
	if _, err := db.pools[0].Exec(ctx, "ALTER ROLE capacity_maintenance RENAME TO baby_tracker_export_app"); err != nil {
		return w, errors.New("capacity_maintenance_setup_failed")
	}
	network := "bt-capacity-" + db.owner + "-net"
	app := "bt-capacity-" + db.owner + "-app"
	proxy := "bt-capacity-" + db.owner + "-proxy"
	if err := db.own(ctx, "network", network, "network", "create", "--label", ownedLabel+"="+db.owner, network); err != nil {
		return w, err
	}
	if _, err := dockerOutput(ctx, "network", "connect", "--alias", "db", network, db.id); err != nil {
		return w, err
	}
	env := map[string]string{"BT_MODE": "self_hosted", "BT_PUBLIC_ORIGIN": "https://sync.example.test", "BT_LISTEN_ADDRESS": ":8080", "BT_SERVER_INSTANCE_ID": r.Config.ServerInstanceID, "BT_RECOVERY_EPOCH": strconv.FormatUint(r.Config.RecoveryEpoch, 10), "BT_APPLE_AUDIENCE": r.Config.AppleAudience, "BT_TRUSTED_PROXY_CIDRS": "127.0.0.1/32"}
	for key, value := range poolEnv {
		env[key] = value
	}
	if w.Diagnostics {
		env["BT_SYNC_DIAGNOSTICS"] = "1"
	}
	writeSecret := func(name string, raw []byte) error { return os.WriteFile(filepath.Join(root, name), raw, 0600) }
	// MCP-enabled cells: the dedicated pool uses the runtime tenant role from
	// its own separately read secret file; the global MCP admission maximum
	// (of 32 total slots) equals the profile's MCP pool (4 unless lowered).
	if w.MCPMode != "" {
		if len(r.Integrations) < w.MCPClients {
			return w, errors.New("capacity_mcp_credentials_missing")
		}
		u, _ := url.Parse(db.dsn)
		u.Host = "db:5432"
		u.Path = "/capacity_fixture"
		u.User = url.UserPassword("capacity_runtime", db.password)
		var cursorKey [32]byte
		if _, err := rand.Read(cursorKey[:]); err != nil {
			return w, err
		}
		if err := writeSecret("mcp.dsn", []byte(u.String())); err != nil {
			return w, err
		}
		if err := writeSecret("mcp_cursor.key", []byte(hex.EncodeToString(cursorKey[:]))); err != nil {
			return w, err
		}
		env["BT_MCP_ENABLED"] = "true"
		env["BT_MCP_DATABASE_URL_FILE"] = filepath.Join(root, "mcp.dsn")
		env["BT_MCP_CURSOR_KEY_FILE"] = filepath.Join(root, "mcp_cursor.key")
		if poolBudget.MCP <= 0 {
			return w, errors.New("capacity_mcp_pool_missing")
		}
		env["BT_ADMISSION_MCP_MAX"] = mcpAdmissionMax(poolBudget)
	}
	for _, role := range []struct{ name, role string }{{"runtime", "capacity_runtime"}, {"maintenance", "baby_tracker_export_app"}} {
		u, _ := url.Parse(db.dsn)
		u.Host = "db:5432"
		u.Path = "/capacity_fixture"
		u.User = url.UserPassword(role.role, db.password)
		if err := writeSecret(role.name+".dsn", []byte(u.String())); err != nil {
			return w, err
		}
		name := "BT_" + strings.ToUpper(role.name) + "_DATABASE_URL_FILE"
		if role.name == "runtime" {
			name = "BT_DATABASE_URL_FILE"
		}
		if role.name == "maintenance" {
			name = "BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE"
		}
		env[name] = filepath.Join(root, role.name+".dsn")
	}
	rings := []struct {
		prefix string
		ring   config.DigestKeyringConfig
	}{{"IDENTITY", r.Config.Auth.IdentityKeys}, {"SESSION_TOKEN", r.Config.Auth.SessionTokenKeys}, {"DEVICE_CONTINUITY", r.Config.Auth.DeviceContinuityKeys}, {"APPLE_NOTIFICATION", r.Config.Auth.AppleNotificationKeys}, {"INVITATION", r.Config.Auth.InvitationKeys}, {"PAGE_TOKEN", r.Config.Auth.PageTokenKeys}}
	for _, v := range rings {
		if len(v.ring.Previous) > 0 {
			return w, ErrFixtureInvalid
		}
		name := strings.ToLower(v.prefix) + ".key"
		if err := writeSecret(name, []byte(hex.EncodeToString(v.ring.Current.Secret[:]))); err != nil {
			return w, err
		}
		env["BT_"+v.prefix+"_KEY_ID"] = v.ring.Current.ID
		suffix := "_PEPPER_FILE"
		if v.prefix == "PAGE_TOKEN" {
			suffix = "_KEY_FILE"
		}
		env["BT_"+v.prefix+suffix] = filepath.Join(root, name)
	}
	for _, v := range []struct {
		name string
		key  [32]byte
	}{{"ACTOR_ID", r.Config.Auth.ActorIDPepper}, {"RATE_LIMIT", r.Config.Auth.RateLimitPepper}} {
		name := strings.ToLower(v.name) + ".key"
		if err := writeSecret(name, []byte(hex.EncodeToString(v.key[:]))); err != nil {
			return w, err
		}
		env["BT_"+v.name+"_PEPPER_FILE"] = filepath.Join(root, name)
	}
	var envText strings.Builder
	for key, value := range env {
		envText.WriteString(key + "=" + value + "\n")
	}
	if err := writeSecret("app.env", []byte(envText.String())); err != nil {
		return w, err
	}
	roots, err := certificate(root)
	if err != nil {
		return w, err
	}
	nginx := `events {}
http { access_log off; error_log /dev/stderr warn; server { listen 8443 ssl; server_name sync.example.test; ssl_certificate ` + root + `/tls.crt; ssl_certificate_key ` + root + `/tls.key; location / { proxy_pass http://app:8080; proxy_http_version 1.1; proxy_set_header Host sync.example.test; proxy_set_header Upgrade $http_upgrade; proxy_set_header Connection "upgrade"; proxy_read_timeout 1200s; } } }`
	if err := writeSecret("nginx.conf", []byte(nginx)); err != nil {
		return w, err
	}
	r.CloseSetupPools()
	appCPU, appMem, dbCPU, dbMem, proxyCPU, proxyMem := "1", "536870912", "2.5", "3221225472", "0.5", "536870912"
	if profile == "small" {
		appCPU, appMem, dbCPU, dbMem, proxyCPU, proxyMem = "0.5", "268435456", "1.25", "1610612736", "0.25", "268435456"
	}
	if _, err := dockerOutput(ctx, "update", "--cpus", dbCPU, "--memory", dbMem, "--memory-swap", dbMem, db.id); err != nil {
		return w, err
	}
	common := []string{"run", "--detach", "--network", network, "--label", ownedLabel + "=" + db.owner}
	appArgs := append(append([]string{}, common...), "--name", app, "--network-alias", "app", "--cpus", appCPU, "--memory", appMem, "--memory-swap", appMem, "--user", strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()), "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--env-file", filepath.Join(root, "app.env"), "--mount", "type=bind,src="+root+",dst="+root, "--mount", "type=bind,src="+r.root+",dst="+r.root, image)
	if err := db.own(ctx, "container", app, appArgs...); err != nil {
		return w, errors.New("capacity_app_start_failed")
	}
	proxyArgs := append(append([]string{}, common...), "--name", proxy, "--cpus", proxyCPU, "--memory", proxyMem, "--memory-swap", proxyMem, "--publish", "127.0.0.1::8443", "--mount", "type=bind,src="+root+",dst="+root+",readonly", proxyImage, "nginx", "-g", "daemon off;", "-c", filepath.Join(root, "nginx.conf"))
	if err := db.own(ctx, "container", proxy, proxyArgs...); err != nil {
		return w, errors.New("capacity_proxy_start_failed")
	}
	for _, v := range []struct{ name, cpu, mem string }{{app, appCPU, appMem}, {db.id, dbCPU, dbMem}, {proxy, proxyCPU, proxyMem}} {
		raw, err := dockerOutput(ctx, "inspect", v.name)
		if err != nil {
			return w, errors.New("capacity_budget_inspect_failed")
		}
		var rows []struct {
			HostConfig struct {
				NanoCpus   int64
				Memory     int64
				MemorySwap int64
			}
			State struct{ Running bool }
		}
		cpu, _ := strconv.ParseFloat(v.cpu, 64)
		mem, _ := strconv.ParseInt(v.mem, 10, 64)
		if json.Unmarshal(raw, &rows) != nil || len(rows) != 1 || rows[0].HostConfig.NanoCpus != int64(cpu*1e9) || rows[0].HostConfig.Memory != mem || rows[0].HostConfig.MemorySwap != mem || !rows[0].State.Running {
			return w, errors.New("capacity_resource_enforcement_unsupported")
		}
	}
	binding, err := dockerOutput(ctx, "port", proxy, "8443/tcp")
	if err != nil {
		return w, errors.New("capacity_proxy_binding_failed")
	}
	address := strings.TrimSpace(string(binding))
	if !strings.HasPrefix(address, "127.0.0.1:") || strings.Contains(address, "\n") {
		return w, ErrUnsafeTarget
	}
	client, mcpClient := targetClients(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, address)
	for {
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://sync.example.test/readyz", nil)
		resp, err := client.Do(req)
		ready := err == nil && resp.StatusCode == 200
		if resp != nil {
			resp.Body.Close()
		}
		if ready {
			break
		}
		select {
		case <-ctx.Done():
			return w, errors.New("capacity_candidate_readiness_failed")
		case <-time.After(200 * time.Millisecond):
		}
	}
	// Release binary must not expose the fixture proof route.
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://sync.example.test/__identitytest/proof", nil)
	resp, err := client.Do(req)
	if err != nil {
		return w, err
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		return w, errors.New("capacity_release_fixture_hook_present")
	}
	binaryPath := filepath.Join(root, "product-binary")
	if _, err := dockerOutput(ctx, "cp", app+":/usr/local/bin/baby-tracker-server", binaryPath); err != nil {
		return w, errors.New("capacity_binary_extract_failed")
	}
	if err := os.Chmod(binaryPath, 0600); err != nil {
		return w, err
	}
	binaryBytes, err := os.ReadFile(binaryPath)
	if err != nil {
		return w, err
	}
	binaryHash := sha256.Sum256(binaryBytes)
	target := &runTarget{TLSAddress: address, HTTP: client, MCPHTTP: mcpClient, Base: CellResult{Version: 1, ResourcesEnforced: true, Profile: profile, SealedAuthorityHead: r.Manifest.ActualAuthorityEvents, RestoredAuthorityHead: r.RestoredAuthorityEvents, FixtureInputSHA256: r.Manifest.InputSHA256, FixtureSourceInputSHA256: r.SourceInputSHA256, LoadGenerator: "host process outside app/db/proxy budgets; recorded separately; no generator quota enforced", PeerSemantics: "writer operation start through distinct-device HTTP journal decode; no Swift durable or observable apply", FixtureProfile: "one account, two devices, one baby per family; diaper history", ResourceAvailability: map[string]string{"cpu": "Task 3 telemetry pending", "rss": "Task 3 telemetry pending", "pool_wait": "Task 3 telemetry pending"}}}
	target.Base.DatabaseProfile = databaseMode
	if poolBudget.Explicit() {
		target.Base.DatabasePools = &poolBudget
	}
	target.Base.EnforcedLimits = map[string]ResourceBudget{}
	for _, v := range []struct{ name, cpu, mem string }{{"app", appCPU, appMem}, {"database", dbCPU, dbMem}, {"proxy", proxyCPU, proxyMem}} {
		cpu, _ := strconv.ParseFloat(v.cpu, 64)
		mem, _ := strconv.ParseInt(v.mem, 10, 64)
		target.Base.EnforcedLimits[v.name] = ResourceBudget{cpu, mem, mem}
	}
	for _, d := range r.Devices {
		target.Devices = append(target.Devices, workloadDevice{Token: d.Session.AccessToken, FamilyID: d.FamilyID, BabyID: d.BabyID, RefreshToken: d.Session.RefreshToken, ExpiresAt: d.Session.ExpiresAt})
	}
	for _, integration := range r.Integrations {
		target.MCPCredentials = append(target.MCPCredentials, mcpCredential{Credential: integration.Credential, BabyID: integration.BabyID})
	}
	target.mcpRouteChecked = true
	for _, v := range []struct {
		name string
		dst  *string
	}{{app, &target.Base.AppImageID}, {db.id, &target.Base.DatabaseImageID}, {proxy, &target.Base.ProxyImageID}} {
		raw, err := dockerOutput(ctx, "inspect", "--format", "{{.Image}}", v.name)
		if err != nil {
			return w, err
		}
		*v.dst = strings.TrimSpace(string(raw))
	}
	target.Base.ProductBinarySHA256 = hex.EncodeToString(binaryHash[:])
	target.Checkpoint = func(result CellResult) error { return writePrivateJSON(filepath.Join(output, "cell.json"), result) }
	w.target = target
	return w, nil
}
func AttachProductIdentity(w Workload, sha, input, binary, scenario string) Workload {
	w.target.Base.ProductSHA = sha
	w.target.Base.ProductInputSHA256 = input
	if binary != "" {
		w.target.Base.ProductBinarySHA256 = binary
	}
	w.target.Base.Scenario = scenario
	return w
}

func AttachSetupDuration(w Workload, seconds float64) Workload {
	w.target.Base.SetupSeconds = seconds
	return w
}

// WriteCheckpoint writes a private atomic durable checkpoint; existing symlink
// temporaries and non-private parents cannot redirect a result write.
func WriteCheckpoint(directory string, r CellResult) error {
	if err := privateDirectory(directory); err != nil {
		return err
	}
	return writePrivateJSON(filepath.Join(directory, "cell.json"), r)
}

// CollectMetrics runs before owned app removal, outside any request/fence.
func (r *RestoredFixture) CollectMetrics(ctx context.Context, w Workload, result *CellResult) {
	result.DiagnosticsEnabled = w.Diagnostics
	if !w.Diagnostics {
		result.ResourceAvailability = map[string]string{"app": "disabled"}
		return
	}
	raw, err := dockerOutput(ctx, "logs", "--tail", "1500", "bt-capacity-"+r.db.owner+"-app")
	result.ResourceAvailability = map[string]string{"app": "unavailable"}
	if err != nil {
		return
	}
	result.AppResources = ParseCapacitySamples(raw)
	if len(result.AppResources) > 1 {
		result.ResourceAvailability["app"] = "available; cumulative sampled every five seconds; process peak RSS; all distinct pools"
	}
}
