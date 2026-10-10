package identity

import (
	"crypto/sha256"
	"testing"
)

func TestIdentityDigestBindsProviderAudienceAndSubject(t *testing.T) {
	keyring := testKeyring()
	a := identityDigest(keyring.Current, "apple", "audience-a", "subject")
	b := identityDigest(keyring.Current, "apple", "audience-b", "subject")
	c := identityDigest(keyring.Current, "apple", "audience-a", "subject-b")
	if a == b || a == c || b == c {
		t.Fatal("identity digest omitted a binding")
	}
	if sha256.Sum256([]byte("apple\x00audience-a\x00subject")) == a.Sum {
		t.Fatal("identity digest was not keyed")
	}
}

func TestAuthorityKeyCandidatesAreCurrentFirstAndDefensive(t *testing.T) {
	keyring := testKeyring()
	previous := keyring.Current
	previous.ID = "identity.v0"
	previous.Secret[0] ^= 0xff
	keyring.Previous = []DigestKey{previous}
	service := &Service{identityKeys: keyring}
	verified := VerifiedIdentity{Provider: "apple", Audience: "com.example", Subject: "000123"}

	got, err := service.AuthorityKeyCandidates(verified)
	if err != nil {
		t.Fatal(err)
	}
	wantCurrent := authorityDigest(identityDigest(keyring.Current, verified.Provider, verified.Audience, verified.Subject))
	wantPrevious := authorityDigest(identityDigest(previous, verified.Provider, verified.Audience, verified.Subject))
	if len(got) != 2 || got[0] != wantCurrent || got[1] != wantPrevious {
		t.Fatalf("candidates=%v", got)
	}
	got[0] = "mutated"
	again, err := service.AuthorityKeyCandidates(verified)
	if err != nil || again[0] != wantCurrent {
		t.Fatalf("defensive candidates=%v err=%v", again, err)
	}
	if _, err := service.AuthorityKeyCandidates(VerifiedIdentity{Provider: "google", Audience: "com.example", Subject: "000123"}); err == nil {
		t.Fatal("unverified provider accepted")
	}
}

func testKeyring() DigestKeyring {
	var secret [32]byte
	for index := range secret {
		secret[index] = byte(index + 1)
	}
	return DigestKeyring{Current: DigestKey{ID: "identity.v1", Secret: secret}}
}
