package authority_test

import (
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"babytracker/server/internal/identity"
)

func TestServiceReportsInstanceAndEpoch(t *testing.T) {
	f := authoritytest.New(t)
	if f.Service.InstanceID() != f.InstanceID || f.Service.RecoveryEpoch() != authoritytest.RecoveryEpoch {
		t.Fatalf("instance=%s epoch=%d", f.Service.InstanceID(), f.Service.RecoveryEpoch())
	}
}

func TestTokenKeyCommitmentNamesKeysWithoutRevealingThem(t *testing.T) {
	f := authoritytest.New(t)
	got := f.Service.TokenKeyCommitment()
	want := authority.TokenKeyCommitment(f.TokenKeys)
	if len(got) != 1 || got[0] != want[0] || got[0].KeyID != f.TokenKeys.Current.ID || got[0].Sum == f.TokenKeys.Current.Secret {
		t.Fatalf("commitment=%+v", got)
	}
	other := f.TokenKeys
	other.Current.Secret[0] ^= 1
	if authority.TokenKeyCommitment(other)[0] == got[0] {
		t.Fatal("commitment ignores the secret")
	}
	rotated := identity.DigestKeyring{Current: identity.DigestKey{ID: "session.v2", Secret: other.Current.Secret}, Previous: []identity.DigestKey{f.TokenKeys.Current}}
	if c := authority.TokenKeyCommitment(rotated); len(c) != 2 || c[0].KeyID != "session.v2" || c[1] != got[0] {
		t.Fatalf("rotated commitment=%+v", c)
	}
}

func TestLegacyEntitlementExemptList(t *testing.T) {
	for _, action := range []string{"read_receipt", "export", "export_family", "delete_family", "recover_family", "forfeit_recovery"} {
		if !authority.LegacyEntitlementExempt(action) {
			t.Fatalf("%s is not exempt", action)
		}
	}
	for _, action := range []string{"read", "mutate_record", "add_baby", "manage_members", "transfer_owner", ""} {
		if authority.LegacyEntitlementExempt(action) {
			t.Fatalf("%s is exempt", action)
		}
	}
}

func TestResolveIdentityReportsLiveAccountProfileRequirement(t *testing.T) {
	f := authoritytest.New(t)
	account := f.Account(t)
	key := seedIdentity(t, f, account, 1)
	resolution, err := f.Service.ResolveIdentity(t.Context(), []string{key})
	if err != nil || resolution.Status != "active" || resolution.ProfileRequired {
		t.Fatalf("resolution=%+v err=%v", resolution, err)
	}
	f.Exec(t, `UPDATE accounts SET profile_required=true WHERE id=$1`, account)
	if resolution, err = f.Service.ResolveIdentity(t.Context(), []string{key}); err != nil || !resolution.ProfileRequired {
		t.Fatalf("profile-required resolution=%+v err=%v", resolution, err)
	}
	f.Exec(t, `UPDATE accounts SET status='deleted' WHERE id=$1`, account)
	f.Exec(t, `UPDATE account_identities SET live=false WHERE account_id=$1`, account)
	// A deleted identity signs in as a new account: the old account's
	// profile requirement is not reported.
	if resolution, err = f.Service.ResolveIdentity(t.Context(), []string{key}); err != nil || resolution.Status != "deleted" || resolution.ProfileRequired {
		t.Fatalf("deleted resolution=%+v err=%v", resolution, err)
	}
}

func TestDeviceReadsTheAccountsBinding(t *testing.T) {
	f := authoritytest.New(t)
	account, other := f.Account(t), f.Account(t)
	device := f.Device(t, account)
	binding, status, err := f.Service.Device(t.Context(), account, device)
	var digest [32]byte
	copy(digest[:], device)
	if err != nil || status != "active" || binding != (authority.DeviceBinding{DeviceID: device, AccountID: account, CanonicalOrigin: "https://tracker.example",
		CredentialKeyID: "continuity.v1", CredentialDigest: digest}) {
		t.Fatalf("binding=%+v status=%q err=%v", binding, status, err)
	}
	if _, status, err := f.Service.Device(t.Context(), other, device); err != nil || status != "" {
		t.Fatalf("another account's device: status=%q err=%v", status, err)
	}
	if _, status, err := f.Service.Device(t.Context(), account, "not-a-uuid"); err != nil || status != "" {
		t.Fatalf("malformed device: status=%q err=%v", status, err)
	}
	f.Exec(t, `UPDATE devices SET status='revoked', revoked_at=$2 WHERE id=$1`, device, f.Now)
	if _, status, err := f.Service.Device(t.Context(), account, device); err != nil || status != "revoked" {
		t.Fatalf("revoked device: status=%q err=%v", status, err)
	}
}

func TestRefreshDeviceResolvesTheRefreshTokensDevice(t *testing.T) {
	f := authoritytest.New(t)
	account := f.Account(t)
	device := f.Device(t, account)
	_, _, rawRefresh := f.Session(t, account, device, "account", "", "")
	candidates := []authority.KeyedDigest{{KeyID: f.TokenKeys.Current.ID, Sum: authoritytest.TokenDigest(f.TokenKeys.Current, "refresh", rawRefresh)}}
	binding, status, err := f.Service.RefreshDevice(t.Context(), candidates)
	if err != nil || status != "active" || binding.DeviceID != device || binding.AccountID != account || binding.CredentialKeyID != "continuity.v1" {
		t.Fatalf("binding=%+v status=%q err=%v", binding, status, err)
	}
	unknown := []authority.KeyedDigest{{KeyID: f.TokenKeys.Current.ID, Sum: sha256.Sum256([]byte("unknown"))}}
	_, _, err = f.Service.RefreshDevice(t.Context(), unknown)
	wantCode(t, err, "session_revoked")
	nextEpoch := serviceOn(t, f, f.Pool, func(o *authority.Options) { o.RecoveryEpoch = authoritytest.RecoveryEpoch + 1 })
	_, _, err = nextEpoch.RefreshDevice(t.Context(), candidates)
	wantCode(t, err, "epoch_changed")
	f.Exec(t, `UPDATE devices SET status='revoked', revoked_at=$2 WHERE id=$1`, device, f.Now)
	if _, status, err := f.Service.RefreshDevice(t.Context(), candidates); err != nil || status != "revoked" {
		t.Fatalf("revoked device: status=%q err=%v", status, err)
	}
	f.Exec(t, `UPDATE accounts SET status='identity_unavailable' WHERE id=$1`, account)
	_, _, err = f.Service.RefreshDevice(t.Context(), candidates)
	wantCode(t, err, "session_revoked")
}

func TestNotificationReceiptFindsOnlyTheExactRetry(t *testing.T) {
	f := authoritytest.New(t)
	account := f.Account(t)
	key := seedIdentity(t, f, account, 1)
	digest := sha256.Sum256([]byte("notification"))
	semantic := hexString(digest)
	commandID := authoritytest.NewID(t)
	other := authoritytest.NewID(t)
	if found, err := f.Service.NotificationReceipt(t.Context(), account, []string{other, commandID}, semantic); err != nil || found {
		t.Fatalf("before: found=%t err=%v", found, err)
	}
	if _, err := f.Service.MarkIdentityUnavailable(t.Context(), authority.IdentityUnavailable{CommandID: commandID, AccountID: account,
		Candidates: []string{key}, NotificationDigest: semantic, At: f.Now}); err != nil {
		t.Fatal(err)
	}
	if found, err := f.Service.NotificationReceipt(t.Context(), account, []string{other, commandID}, semantic); err != nil || !found {
		t.Fatalf("exact retry: found=%t err=%v", found, err)
	}
	changed := hexString(sha256.Sum256([]byte("changed")))
	_, err := f.Service.NotificationReceipt(t.Context(), account, []string{commandID}, changed)
	wantCode(t, err, "command_id_reused")
	_, err = f.Service.NotificationReceipt(t.Context(), account, nil, semantic)
	wantCode(t, err, "invalid_command")
}

func TestIdentityAuthorityAdaptsChallengesAndPassesCodesThrough(t *testing.T) {
	f := authoritytest.New(t)
	port := f.Service.IdentityAuthority()
	digest := sha256.Sum256([]byte("challenge"))
	if err := port.CreateChallenge(t.Context(), digest, f.Now.Add(5*time.Minute), f.Now); err != nil {
		t.Fatal(err)
	}
	if err := port.CreateChallenge(t.Context(), digest, f.Now.Add(5*time.Minute), f.Now); !errors.Is(err, identity.ErrUnavailable) {
		t.Fatalf("duplicate challenge err=%v", err)
	}
	if err := port.ConsumeChallenge(t.Context(), digest, f.Now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := port.ConsumeChallenge(t.Context(), digest, f.Now.Add(time.Minute)); !errors.Is(err, identity.ErrInvalidOrExpiredChallenge) {
		t.Fatalf("second consumption err=%v", err)
	}
	account := f.Account(t)
	key := seedIdentity(t, f, account, 1)
	resolution, err := port.ResolveIdentity(t.Context(), []string{key})
	if err != nil || resolution != (identity.IdentityAuthority{Status: "active", AccountID: account, IdentityKey: key, LifecycleGeneration: 1,
		NextActorGeneration: 2}) || !resolution.Live() {
		t.Fatalf("resolution=%+v err=%v", resolution, err)
	}
	if _, err := port.ResolveIdentity(t.Context(), []string{"malformed"}); authority.CodeOf(err) != "invalid_command" {
		t.Fatalf("malformed candidates err=%v", err)
	}
	semantic := hexString(sha256.Sum256([]byte("notification")))
	command := identity.IdentityUnavailableCommand{CommandID: authoritytest.NewID(t), AccountID: account, Candidates: []string{key},
		NotificationDigest: semantic, At: f.Now}
	if err := port.MarkIdentityUnavailable(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	if err := port.MarkIdentityUnavailable(t.Context(), command); err != nil {
		t.Fatalf("exact retry err=%v", err)
	}
	if found, err := port.NotificationReceipt(t.Context(), account, []string{command.CommandID}, semantic); err != nil || !found {
		t.Fatalf("receipt found=%t err=%v", found, err)
	}
	command.CommandID = authoritytest.NewID(t)
	if err := port.MarkIdentityUnavailable(t.Context(), command); authority.CodeOf(err) != "invalid_command" {
		t.Fatalf("already unavailable err=%v", err)
	}
}

func hexString(sum [32]byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 64)
	for i, b := range sum {
		out[2*i], out[2*i+1] = digits[b>>4], digits[b&0x0f]
	}
	return string(out)
}
