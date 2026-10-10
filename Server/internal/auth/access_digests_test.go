package auth_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"babytracker/server/internal/auth"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/config"
	"babytracker/server/internal/family"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/testkit"
)

func mustDigests(t *testing.T) func([]authority.KeyedDigest, error) []authority.KeyedDigest {
	return func(digests []authority.KeyedDigest, err error) []authority.KeyedDigest {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return digests
	}
}

func digestKey(id string, fill byte) identity.DigestKey {
	key := identity.DigestKey{ID: id}
	for i := range key.Secret {
		key.Secret[i] = fill
	}
	return key
}

// accessSum is HMAC-SHA256(key, "access" || 0x00 || token), computed here
// independently of the auth package.
func accessSum(key identity.DigestKey, token string) [32]byte {
	mac := hmac.New(sha256.New, key.Secret[:])
	mac.Write([]byte("access"))
	mac.Write([]byte{0})
	mac.Write([]byte(token))
	var sum [32]byte
	copy(sum[:], mac.Sum(nil))
	return sum
}

func TestAccessDigestsAuthenticateLikeAuthenticate(t *testing.T) {
	h := testkit.New(t)
	ctx := t.Context()
	must := mustDigests(t)
	token := h.AccessToken("owner")

	digests := must(h.Auth.AccessDigests(token))
	got, err := h.Authority.Authenticate(ctx, digests)
	if err != nil {
		t.Fatal(err)
	}
	want, err := h.Auth.Authenticate(ctx, token)
	if err != nil || got != want || got.AccountID != h.Owner.AccountID {
		t.Fatalf("digests authenticate %+v, Authenticate %+v (%v)", got, want, err)
	}
	if len(digests) != 1 || digests[0].KeyID != "session.v1" || digests[0].Sum != accessSum(digestKey("session.v1", 0x22), token) {
		t.Fatalf("digests are not the access-class HMAC under the token key: %d digests, key %q", len(digests), digests[0].KeyID)
	}

	// Malformed bearer tokens are refused before any digest, as Authenticate
	// refuses them.
	decoded := make([]byte, 32)
	for _, bad := range []string{"not-a-token", "", base64.RawURLEncoding.EncodeToString(decoded[:31]),
		base64.URLEncoding.EncodeToString(decoded), base64.StdEncoding.EncodeToString(append(decoded, 0xfb)), token + "A"} {
		digests, err := h.Auth.AccessDigests(bad)
		if !errors.Is(err, auth.ErrSessionRevoked) || digests != nil {
			t.Fatalf("malformed token (%d bytes): %d digests, %v", len(bad), len(digests), err)
		}
		if _, err := h.Auth.Authenticate(ctx, bad); auth.Code(err) != "session_revoked" {
			t.Fatalf("Authenticate disagrees on a malformed token: %v", err)
		}
	}
	var none *auth.Service
	if digests, err := none.AccessDigests(token); !errors.Is(err, auth.ErrSessionRevoked) || digests != nil {
		t.Fatalf("nil service: %d digests, %v", len(digests), err)
	}

	// With a rotated keyring, the digest under every key, current first: the
	// token issued under the now-previous key still authenticates.
	keys := identity.DigestKeyring{Current: digestKey("session.v2", 0x23), Previous: []identity.DigestKey{digestKey("session.v1", 0x22)}}
	clock := func() time.Time { return h.Now }
	rotated, err := authority.New(authority.Options{Pool: h.Runtime, Now: clock, InstanceID: testkit.InstanceID, RecoveryEpoch: testkit.RecoveryEpoch,
		TokenKeys: keys, Allowed: family.Allowed, EntitlementExempt: authority.LegacyEntitlementExempt})
	if err != nil {
		t.Fatal(err)
	}
	verifier := identity.IdentityVerifierFunc(func(context.Context, string, string) (identity.VerifiedIdentity, error) {
		return identity.VerifiedIdentity{}, errors.New("unused")
	})
	var actor [32]byte
	actor[0] = 1
	service, err := auth.New(auth.Options{Identity: h.Identity, Verifier: verifier, Authority: rotated, Mode: config.ModeSelfHosted,
		Origin: h.Info.CanonicalOrigin, InstanceID: testkit.InstanceID, TokenKeys: keys,
		DeviceKeys: identity.DigestKeyring{Current: digestKey("device.v1", 0x33)}, ActorIDKey: actor, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	digests = must(service.AccessDigests(token))
	if len(digests) != 2 || digests[0].KeyID != "session.v2" || digests[1].KeyID != "session.v1" ||
		digests[0].Sum != accessSum(keys.Current, token) || digests[1].Sum != accessSum(keys.Previous[0], token) {
		t.Fatalf("rotated keyring: %d digests, want current then previous", len(digests))
	}
	if got, err := rotated.Authenticate(ctx, digests); err != nil || got != want {
		t.Fatalf("a token of the previous key: %+v %v", got, err)
	}
}
