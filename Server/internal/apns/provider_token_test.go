package apns

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/config"
)

func iatOf(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	claims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	return string(claims)
}

func TestProviderTokenRefreshesBeforeAnHourAndNotTwiceInTwentyMinutes(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	now := start
	provider := newProviderToken("TEAM123456", "KEY1234567", key, func() time.Time { return now })
	at := func(offset time.Duration) string {
		t.Helper()
		now = start.Add(offset)
		token, err := provider.token()
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	claims := func(offset time.Duration) string {
		return fmt.Sprintf(`{"iat":%d,"iss":"TEAM123456"}`, start.Add(offset).Unix())
	}

	first := at(0)
	if iatOf(t, first) != claims(0) {
		t.Fatalf("claims %s", iatOf(t, first))
	}
	if at(39*time.Minute) != first || at(39*time.Minute+59*time.Second) != first {
		t.Fatal("re-signed before 40 minutes")
	}
	refreshed := at(41 * time.Minute)
	if refreshed == first || iatOf(t, refreshed) != claims(41*time.Minute) {
		t.Fatalf("not re-signed at 41 minutes: %s", iatOf(t, refreshed))
	}
	if providerTokenRefresh != 40*time.Minute || providerTokenFloor != 20*time.Minute {
		t.Fatalf("refresh %v, floor %v", providerTokenRefresh, providerTokenFloor)
	}

	// invalidate: Apple refuses refreshes more often than every 20 minutes,
	// so a refusal within 20 minutes of signing keeps the token.
	now = start.Add(41*time.Minute + 10*time.Minute)
	provider.invalidate()
	if at(51*time.Minute) != refreshed {
		t.Fatal("invalidate at 10 minutes re-signed inside Apple's 20-minute floor")
	}
	now = start.Add(41*time.Minute + 19*time.Minute + 59*time.Second)
	provider.invalidate()
	if at(41*time.Minute+19*time.Minute+59*time.Second) != refreshed {
		t.Fatal("invalidate just under 20 minutes re-signed")
	}
	now = start.Add(41*time.Minute + 21*time.Minute)
	provider.invalidate()
	forced := at(62 * time.Minute)
	if forced == refreshed || iatOf(t, forced) != claims(62*time.Minute) {
		t.Fatalf("invalidate at 21 minutes did not force a new token: %s", iatOf(t, forced))
	}
	if at(63*time.Minute) != forced {
		t.Fatal("the forced token was not cached")
	}
	// Exactly at the floor counts as old enough.
	now = start.Add(62*time.Minute + providerTokenFloor)
	provider.invalidate()
	if at(82*time.Minute) == forced {
		t.Fatal("invalidate at exactly 20 minutes kept the token")
	}

	// A clock that steps back re-signs rather than trusting a token from the future.
	future := at(200 * time.Minute)
	if back := at(190 * time.Minute); back == future || iatOf(t, back) != claims(190*time.Minute) {
		t.Fatal("a token issued in the future was reused")
	}
}

func TestProviderTokenRefusesAnInvalidKey(t *testing.T) {
	if _, err := newProviderToken("TEAM123456", "KEY1234567", nil, time.Now).token(); err == nil {
		t.Fatal("signed without a key")
	}
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newProviderToken("TEAM123456", "KEY1234567", p384, time.Now).token(); err == nil {
		t.Fatal("signed with a P-384 key")
	}
}

// New talks to Apple's two documented hosts with main's HTTP client settings.
func TestNewTalksOnlyToApplesHostsOverHTTP2(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	path := filepath.Join(t.TempDir(), "apns.p8")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := New(config.APNSCredentials{TeamID: "TEAM123456", KeyID: "KEY1234567", KeyFile: path, BundleID: "com.example.babytracker"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if client.sandboxURL != "https://api.sandbox.push.apple.com" || client.productionURL != "https://api.push.apple.com" {
		t.Fatalf("hosts %s %s", client.sandboxURL, client.productionURL)
	}
	if client.topic != "com.example.babytracker" || client.http.Timeout != requestTimeout || requestTimeout != 10*time.Second {
		t.Fatalf("topic %q timeout %v", client.topic, client.http.Timeout)
	}
	if client.http.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("redirects are followed")
	}
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok || !transport.ForceAttemptHTTP2 || transport.MaxIdleConns != 32 || transport.MaxIdleConnsPerHost != 8 ||
		transport.IdleConnTimeout != 90*time.Second || transport.TLSHandshakeTimeout != 5*time.Second {
		t.Fatalf("transport %+v", client.http.Transport)
	}
}
