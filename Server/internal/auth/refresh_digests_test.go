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

// refreshSum is HMAC-SHA256(key, "refresh" || 0x00 || token), computed here
// independently of the auth package.
func refreshSum(key identity.DigestKey, token string) [32]byte {
	mac := hmac.New(sha256.New, key.Secret[:])
	mac.Write([]byte("refresh"))
	mac.Write([]byte{0})
	mac.Write([]byte(token))
	var sum [32]byte
	copy(sum[:], mac.Sum(nil))
	return sum
}

// liveSums is the set of refresh digests LiveRefreshDigests returns.
func liveSums(t *testing.T, service *authority.Service, limit int) map[[32]byte]string {
	t.Helper()
	digests, err := service.LiveRefreshDigests(t.Context(), limit)
	if err != nil {
		t.Fatal(err)
	}
	sums := make(map[[32]byte]string, len(digests))
	for _, d := range digests {
		sums[d.Sum] = d.KeyID
	}
	if len(sums) != len(digests) {
		t.Fatalf("%d digests, %d distinct", len(digests), len(sums))
	}
	return sums
}

// RefreshDigests is a refresh token's sessions.refresh_digest under every
// token key, current first, with no database work: the digest of a live
// session's token is the one LiveRefreshDigests lists. LiveRefreshDigests
// lists only the sessions whose refresh token can still be used: not a
// consumed parent, not a revoked session, not an expired one, not another
// recovery epoch's, and at most limit of them.
func TestRefreshDigestsAreTheLiveSessionsDigests(t *testing.T) {
	h := testkit.New(t)
	ctx := t.Context()
	must := mustDigests(t)

	consumed := h.RefreshToken("caregiver")
	h.Refresh("caregiver")
	live := h.RefreshToken("caregiver")
	owner, outsider := h.RefreshToken("owner"), h.RefreshToken("outsider")

	revokedBundle, err := h.SignInDevice(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	revokedPrincipal, err := h.Auth.Authenticate(ctx, revokedBundle.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Auth.Logout(ctx, revokedPrincipal, h.CommandID()); err != nil {
		t.Fatal(err)
	}

	digests := must(h.Auth.RefreshDigests(live))
	if len(digests) != 1 || digests[0].KeyID != "session.v1" || digests[0].Sum != refreshSum(digestKey("session.v1", 0x22), live) {
		t.Fatalf("digests are not the refresh-class HMAC under the token key: %d digests", len(digests))
	}
	sums := liveSums(t, h.Authority, 1<<16)
	for name, token := range map[string]string{"caregiver's child": live, "owner's": owner, "outsider's": outsider} {
		if keyID, ok := sums[must(h.Auth.RefreshDigests(token))[0].Sum]; !ok || keyID != "session.v1" {
			t.Fatalf("the %s live refresh token is not listed (key %q)", name, keyID)
		}
	}
	for name, token := range map[string]string{"consumed parent": consumed, "revoked session": revokedBundle.RefreshToken} {
		if _, ok := sums[must(h.Auth.RefreshDigests(token))[0].Sum]; ok {
			t.Fatalf("the %s's refresh token is listed", name)
		}
	}
	if len(sums) != 3 {
		t.Fatalf("%d live refresh digests, want the owner's, the caregiver's and the outsider's", len(sums))
	}
	// At most limit (the harness clock is fixed, so which one is not).
	newest := liveSums(t, h.Authority, 1)
	if len(newest) != 1 {
		t.Fatalf("limit 1: %d digests", len(newest))
	}
	for sum := range newest {
		if _, ok := sums[sum]; !ok {
			t.Fatal("limit 1 listed a digest the full list does not")
		}
	}
	if none, err := h.Authority.LiveRefreshDigests(ctx, 0); err != nil || len(none) != 0 {
		t.Fatalf("limit 0: %d digests, %v", len(none), err)
	}

	// Once every refresh token has expired, or under another recovery epoch,
	// nothing is listed.
	authorityAt := func(now time.Time, epoch uint64) *authority.Service {
		service, err := authority.New(authority.Options{Pool: h.Runtime, Now: func() time.Time { return now }, InstanceID: testkit.InstanceID,
			RecoveryEpoch: epoch, TokenKeys: identity.DigestKeyring{Current: digestKey("session.v1", 0x22)}, Allowed: family.Allowed,
			EntitlementExempt: authority.LegacyEntitlementExempt})
		if err != nil {
			t.Fatal(err)
		}
		return service
	}
	if got := liveSums(t, authorityAt(h.Now.Add(366*24*time.Hour), testkit.RecoveryEpoch), 1<<16); len(got) != 0 {
		t.Fatalf("%d refresh digests listed once every token expired", len(got))
	}
	if got := liveSums(t, authorityAt(h.Now, testkit.RecoveryEpoch+1), 1<<16); len(got) != 0 {
		t.Fatalf("%d refresh digests of another recovery epoch listed", len(got))
	}

	// Malformed refresh tokens are refused before any digest, as Refresh
	// refuses them.
	decoded := make([]byte, 32)
	for _, bad := range []string{"not-a-token", "", base64.RawURLEncoding.EncodeToString(decoded[:31]), live + "A"} {
		if digests, err := h.Auth.RefreshDigests(bad); !errors.Is(err, auth.ErrSessionRevoked) || digests != nil {
			t.Fatalf("malformed token (%d bytes): %d digests, %v", len(bad), len(digests), err)
		}
	}
	var none *auth.Service
	if digests, err := none.RefreshDigests(live); !errors.Is(err, auth.ErrSessionRevoked) || digests != nil {
		t.Fatalf("nil service: %d digests, %v", len(digests), err)
	}

	// With a rotated keyring, the digest under every key, current first: the
	// one under the now-previous key is the stored digest of the token it
	// issued.
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
	digests = must(service.RefreshDigests(live))
	if len(digests) != 2 || digests[0].KeyID != "session.v2" || digests[1].KeyID != "session.v1" ||
		digests[0].Sum != refreshSum(keys.Current, live) || digests[1].Sum != refreshSum(keys.Previous[0], live) {
		t.Fatalf("rotated keyring: %d digests, want current then previous", len(digests))
	}
	if _, ok := liveSums(t, rotated, 1<<16)[digests[1].Sum]; !ok {
		t.Fatal("the previous key's digest of a live token is not listed")
	}
}
