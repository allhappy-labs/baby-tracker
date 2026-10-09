package identity

import (
	"encoding/json"
	"os"
	"testing"
)

func TestNonceV1AndCrossServerRelay(t *testing.T) {
	challenge := "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
	instanceID := "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"
	want := "6227c68e4a909c26acc63eaa3f0da6d0446d861a6edaf459330283c2e4afbbc1"
	got, err := AppleNonce("HTTPS://Sync.Example.Test:443/", instanceID, challenge)
	if err != nil || got != want {
		t.Fatalf("nonce=%q err=%v", got, err)
	}
	other, _ := AppleNonce("https://other.example.test", instanceID, challenge)
	if other == got {
		t.Fatal("origin not bound")
	}
	other, _ = AppleNonce("https://sync.example.test", "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5e", challenge)
	if other == got {
		t.Fatal("instance not bound")
	}
}

func TestNonceRejectsNoncanonicalBindings(t *testing.T) {
	challenge := "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
	instanceID := "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"
	for _, value := range []string{
		"https://sync.example.test/path",
		"https://sync.example.test?",
		"https://user@sync.example.test",
		"https://sync.example.test.",
		"https://sync..example.test",
		"https://127.0.0.1",
		"https://sync.example.test:0",
		"https://sync.example.test:65536",
		"https://sync.exämple.test",
		" https://sync.example.test",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := AppleNonce(value, instanceID, challenge); err == nil {
				t.Fatal("unsafe origin accepted")
			}
		})
	}
	for _, value := range []string{
		challenge + "=",
		"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHg",
		"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh!",
	} {
		if _, err := AppleNonce("https://sync.example.test", instanceID, value); err == nil {
			t.Fatalf("invalid challenge %q accepted", value)
		}
	}
}

func TestNonceFixtureMatchesImplementation(t *testing.T) {
	data, err := os.ReadFile("../../api/fixtures/auth-nonce-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Version          string `json:"version"`
		CanonicalOrigin  string `json:"canonical_origin"`
		ServerInstanceID string `json:"server_instance_id"`
		Challenge        string `json:"challenge"`
		Nonce            string `json:"nonce"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	got, err := AppleNonce(fixture.CanonicalOrigin, fixture.ServerInstanceID, fixture.Challenge)
	if err != nil || fixture.Version != "v1" || got != fixture.Nonce {
		t.Fatalf("fixture version=%q nonce=%q got=%q err=%v", fixture.Version, fixture.Nonce, got, err)
	}
}
