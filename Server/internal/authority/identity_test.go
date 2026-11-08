package authority_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"babytracker/server/internal/identity"
)

func identityKey(t *testing.T, keyID string) string {
	t.Helper()
	var sum [32]byte
	if _, err := rand.Read(sum[:]); err != nil {
		t.Fatal(err)
	}
	return "hmac-sha256:" + keyID + ":" + hex.EncodeToString(sum[:])
}

// seedIdentity binds a fresh identity key to account at generation and
// returns the key.
func seedIdentity(t *testing.T, f *authoritytest.Fixture, account string, generation uint64) string {
	t.Helper()
	key := identityKey(t, "identity.v1")
	digest, _ := hex.DecodeString(key[len("hmac-sha256:identity.v1:"):])
	f.Exec(t, `INSERT INTO account_identities(provider,identity_key_id,identity_digest,account_id,lifecycle_generation,audience,subject,live,created_at)
		VALUES ('apple','identity.v1',$1,$2,$3,'com.example.app','subject',true,$4)`, digest, account, int64(generation), f.Now)
	return key
}

type signInCase struct {
	in                    authority.SignIn
	rawAccess, rawRefresh string
}

// newSignIn builds a complete sign-in for candidates registering a new device.
func newSignIn(t *testing.T, f *authoritytest.Fixture, candidates []string, accountID string, generation uint64) signInCase {
	t.Helper()
	rawAccess, rawRefresh := authoritytest.NewID(t), authoritytest.NewID(t)
	device := authoritytest.NewID(t)
	name := "Ada"
	in := authority.SignIn{CommandID: authoritytest.NewID(t), At: f.Now, Candidates: candidates, Audience: "com.example.app", Subject: "001234.abcd",
		ProposedAccountID: accountID, ProposedGeneration: generation, DisplayName: &name, ProfileConfirmed: true,
		Device:         authority.DeviceBinding{DeviceID: device, AccountID: accountID, CanonicalOrigin: "https://tracker.example", CredentialKeyID: "continuity.v1"},
		RegisterDevice: true, SessionID: authoritytest.NewID(t), TokenKeyID: f.TokenKeys.Current.ID,
		AccessDigest: authoritytest.TokenDigest(f.TokenKeys.Current, "access", rawAccess), RefreshDigest: authoritytest.TokenDigest(f.TokenKeys.Current, "refresh", rawRefresh),
		AccessExpiresAt: f.Now.Add(10 * time.Minute), RefreshExpiresAt: f.Now.Add(30 * 24 * time.Hour), DeadlineAt: f.Now.Add(365 * 24 * time.Hour),
		CanonicalOrigin: "https://tracker.example", Mode: authority.ModeSelfHosted}
	copy(in.Device.CredentialDigest[:], device)
	return signInCase{in: in, rawAccess: rawAccess, rawRefresh: rawRefresh}
}

func TestSignInRegistersAccountDeviceAndSession(t *testing.T) {
	f := authoritytest.New(t)
	key := identityKey(t, "identity.v1")
	resolution, err := f.Service.ResolveIdentity(t.Context(), []string{key})
	if err != nil || resolution != (authority.IdentityResolution{Status: "absent", NextActorGeneration: 1}) {
		t.Fatalf("resolution %+v %v", resolution, err)
	}
	accountID := authoritytest.NewID(t)
	c := newSignIn(t, f, []string{key}, accountID, 1)
	got, err := f.Service.SignIn(t.Context(), c.in)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Registered || got.Restored || got.AccountID != accountID || got.Revision != 1 || got.CommandID != c.in.CommandID ||
		got.Account != (authority.AccountView{ID: accountID, DisplayName: "Ada"}) || got.FamilySlot != nil {
		t.Fatalf("result %+v", got)
	}
	if n := count(t, f, `SELECT count(*) FROM accounts WHERE id=$1 AND status='active' AND lifecycle_generation=1 AND display_name='Ada' AND NOT profile_required`, accountID); n != 1 {
		t.Fatal("account not registered")
	}
	if n := count(t, f, `SELECT count(*) FROM account_identities WHERE account_id=$1 AND live AND lifecycle_generation=1 AND identity_key_id='identity.v1' AND audience='com.example.app'`, accountID); n != 1 {
		t.Fatal("identity not bound")
	}
	if n := count(t, f, `SELECT count(*) FROM devices WHERE id=$1 AND account_id=$2 AND status='active' AND continuity_key_id='continuity.v1'`, c.in.Device.DeviceID, accountID); n != 1 {
		t.Fatal("device not registered")
	}
	session, _ := sessionRow(t, f, c.in.SessionID)
	if session.Status != "active" || session.Scope != "account" || session.LineageRootID != session.ID || session.ParentID != "" || session.IssuedSeq != 1 ||
		session.AccessDigest != c.in.AccessDigest || !session.DeadlineAt.Equal(c.in.DeadlineAt) || !session.LineageStartedAt.Equal(f.Now) || session.RecoveryEpoch != 1 {
		t.Fatalf("session %+v", session)
	}
	rows := readAudit(t, f, authority.ScopeAccount, accountID)
	if len(rows) != 1 || rows[0].Kind != "identity_session_issued" || rows[0].CommandID != c.in.CommandID || recompute(t, rows[0]) != rows[0].Hash {
		t.Fatalf("audit %+v", rows)
	}
	var payload map[string]any
	if err := json.Unmarshal(rows[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["register_account"] != true || payload["register_device"] != true || payload["session_id"] != c.in.SessionID ||
		payload["device_id"] != c.in.Device.DeviceID || payload["identity_key_id"] != "identity.v1" || payload["origin"] != "https://tracker.example" {
		t.Fatalf("payload %s", rows[0].Payload)
	}
	if n := count(t, f, `SELECT count(*) FROM command_receipts WHERE actor_account_id=$1 AND command_id=$2`, accountID, c.in.CommandID); n != 1 {
		t.Fatal("no receipt")
	}
	resolution, err = f.Service.ResolveIdentity(t.Context(), []string{key})
	if err != nil || resolution != (authority.IdentityResolution{Status: "active", AccountID: accountID, IdentityKey: key, LifecycleGeneration: 1, NextActorGeneration: 2}) {
		t.Fatalf("resolution %+v %v", resolution, err)
	}

	t.Run("an existing device signs in again", func(t *testing.T) {
		again := newSignIn(t, f, []string{key}, accountID, 1)
		again.in.Device, again.in.RegisterDevice, again.in.DisplayName, again.in.ProfileConfirmed = c.in.Device, false, nil, false
		got, err := f.Service.SignIn(t.Context(), again.in)
		if err != nil || got.Registered || got.Revision != 2 {
			t.Fatalf("%+v %v", got, err)
		}
		bad := newSignIn(t, f, []string{key}, accountID, 1)
		bad.in.Device, bad.in.RegisterDevice = c.in.Device, false
		bad.in.Device.CredentialDigest[0] ^= 1
		_, err = f.Service.SignIn(t.Context(), bad.in)
		wantCode(t, err, "device_continuity_invalid")
		dup := newSignIn(t, f, []string{key}, accountID, 1)
		dup.in.Device = c.in.Device
		_, err = f.Service.SignIn(t.Context(), dup.in)
		wantCode(t, err, "invalid_command")
		reused := newSignIn(t, f, []string{key}, accountID, 1)
		reused.in.SessionID = c.in.SessionID
		_, err = f.Service.SignIn(t.Context(), reused.in)
		wantCode(t, err, "invalid_command")
		wrongActor := newSignIn(t, f, []string{key}, authoritytest.NewID(t), 1)
		_, err = f.Service.SignIn(t.Context(), wrongActor.in)
		wantCode(t, err, "invalid_command")
		// Revoked between the caller's proof and the sign-in.
		f.Exec(t, `UPDATE devices SET status='revoked', revoked_at=$2 WHERE id=$1`, c.in.Device.DeviceID, f.Now)
		revoked := newSignIn(t, f, []string{key}, accountID, 1)
		revoked.in.Device, revoked.in.RegisterDevice = c.in.Device, false
		_, err = f.Service.SignIn(t.Context(), revoked.in)
		wantCode(t, err, "device_continuity_invalid")
	})
	t.Run("a deletion-recovery slot still issues an account-scope session", func(t *testing.T) {
		family := f.Family(t, accountID)
		f.Exec(t, `UPDATE family_slots SET state='family_recovery' WHERE account_id=$1`, accountID)
		again := newSignIn(t, f, []string{key}, accountID, 1)
		got, err := f.Service.SignIn(t.Context(), again.in)
		if err != nil {
			t.Fatal(err)
		}
		if session, _ := sessionRow(t, f, again.in.SessionID); session.Scope != "account" {
			t.Fatalf("scope %s", session.Scope)
		}
		if got.FamilySlot == nil || *got.FamilySlot != (authority.FamilySlot{FamilyID: family, State: "family_recovery"}) {
			t.Fatalf("slot %+v", got.FamilySlot)
		}
	})
}

func TestSignInExactRetryReturnsSameSession(t *testing.T) {
	f := authoritytest.New(t)
	key := identityKey(t, "identity.v1")
	accountID := authoritytest.NewID(t)
	c := newSignIn(t, f, []string{key}, accountID, 1)
	first, err := f.Service.SignIn(t.Context(), c.in)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := f.Service.SignIn(t.Context(), c.in)
	if err != nil {
		t.Fatal(err)
	}
	if retry != first {
		t.Fatalf("retry %+v, first %+v", retry, first)
	}
	if n := count(t, f, `SELECT count(*) FROM sessions WHERE account_id=$1`, accountID); n != 1 {
		t.Fatalf("%d sessions after the retry", n)
	}
	if n := len(readAudit(t, f, authority.ScopeAccount, accountID)); n != 1 {
		t.Fatalf("%d audit rows after the retry", n)
	}
	changed := c.in
	changed.SessionID = authoritytest.NewID(t)
	_, err = f.Service.SignIn(t.Context(), changed)
	wantCode(t, err, "command_id_reused")
}

// The receipt outlives the sign-in's audit row (rows are kept at least 13
// months, receipts forever): an exact retry after the row expired with its
// partition, so its seq is at the account's retention anchor, still replays,
// without the registration flag. The same row missing with no anchor over it
// is not expired, and stays service_unavailable.
func TestSignInExactRetryAfterItsAuditRowExpired(t *testing.T) {
	f := authoritytest.New(t)
	key := identityKey(t, "identity.v1")
	accountID := authoritytest.NewID(t)
	c := newSignIn(t, f, []string{key}, accountID, 1)
	first, err := f.Service.SignIn(t.Context(), c.in)
	if err != nil || !first.Registered {
		t.Fatalf("first sign-in %+v (%v), want a registration", first, err)
	}
	row := readAudit(t, f, authority.ScopeAccount, accountID)[0]
	f.Exec(t, `DELETE FROM security_audit_events WHERE scope='account' AND scope_id=$1`, accountID)
	_, err = f.Service.SignIn(t.Context(), c.in)
	wantCode(t, err, "service_unavailable")

	f.Exec(t, `INSERT INTO audit_retention_anchors(scope,scope_id,anchor_seq,anchor_hash,anchored_at,dropped_before) VALUES ('account',$1,$2,$3,$4,$4)`,
		accountID, int64(row.Seq), row.Hash[:], f.Now)
	retry, err := f.Service.SignIn(t.Context(), c.in)
	if err != nil {
		t.Fatal(err)
	}
	want := first
	want.Registered, want.Restored = false, false
	if retry != want {
		t.Fatalf("retry after expiry %+v, want %+v", retry, want)
	}
}

func TestSignInRestoresIdentityUnavailable(t *testing.T) {
	f := authoritytest.New(t)
	account := f.Account(t)
	key := seedIdentity(t, f, account, 1)
	f.Exec(t, `UPDATE accounts SET status='identity_unavailable' WHERE id=$1`, account)
	resolution, err := f.Service.ResolveIdentity(t.Context(), []string{key})
	if err != nil || resolution.Status != "identity_unavailable" || resolution.AccountID != account {
		t.Fatalf("%+v %v", resolution, err)
	}
	c := newSignIn(t, f, []string{key}, account, 1)
	got, err := f.Service.SignIn(t.Context(), c.in)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Restored || got.Registered || got.Account.DisplayName != "Fixture" {
		t.Fatalf("%+v", got)
	}
	if n := count(t, f, `SELECT count(*) FROM accounts WHERE id=$1 AND status='active'`, account); n != 1 {
		t.Fatal("account not restored")
	}
	rows := readAudit(t, f, authority.ScopeAccount, account)
	if len(rows) != 1 || rows[0].Kind != "identity_restored" {
		t.Fatalf("audit %+v", rows)
	}
	retry, err := f.Service.SignIn(t.Context(), c.in)
	if err != nil || retry != got {
		t.Fatalf("retry %+v %v", retry, err)
	}
}

func TestDeletedIdentityGetsNextGenerationActor(t *testing.T) {
	f := authoritytest.New(t)
	old := f.Account(t)
	key := seedIdentity(t, f, old, 1)
	f.Exec(t, `UPDATE accounts SET status='deleted' WHERE id=$1`, old)
	f.Exec(t, `UPDATE account_identities SET live=false WHERE account_id=$1`, old)
	resolution, err := f.Service.ResolveIdentity(t.Context(), []string{key})
	if err != nil || resolution != (authority.IdentityResolution{Status: "deleted", AccountID: old, IdentityKey: key, LifecycleGeneration: 1, NextActorGeneration: 2}) {
		t.Fatalf("%+v %v", resolution, err)
	}
	next := authoritytest.NewID(t)
	stale := newSignIn(t, f, []string{key}, next, 1)
	_, err = f.Service.SignIn(t.Context(), stale.in)
	wantCode(t, err, "invalid_command")
	c := newSignIn(t, f, []string{key}, next, 2)
	got, err := f.Service.SignIn(t.Context(), c.in)
	if err != nil || !got.Registered || got.AccountID != next {
		t.Fatalf("%+v %v", got, err)
	}
	resolution, err = f.Service.ResolveIdentity(t.Context(), []string{key})
	if err != nil || resolution != (authority.IdentityResolution{Status: "active", AccountID: next, IdentityKey: key, LifecycleGeneration: 2, NextActorGeneration: 3}) {
		t.Fatalf("%+v %v", resolution, err)
	}
	if n := count(t, f, `SELECT count(*) FROM accounts WHERE id=$1 AND lifecycle_generation=2`, next); n != 1 {
		t.Fatal("next actor not at generation 2")
	}
}

func TestResolveIdentityRules(t *testing.T) {
	f := authoritytest.New(t)
	key := identityKey(t, "identity.v1")
	for name, candidates := range map[string][]string{
		"none":      nil,
		"nine":      {key, identityKey(t, "a"), identityKey(t, "b"), identityKey(t, "c"), identityKey(t, "d"), identityKey(t, "e"), identityKey(t, "f"), identityKey(t, "g"), identityKey(t, "h")},
		"duplicate": {key, key},
		"malformed": {"hmac-sha256:identity.v1:ABC"},
	} {
		_, err := f.Service.ResolveIdentity(t.Context(), candidates)
		if authority.CodeOf(err) != "invalid_command" {
			t.Errorf("%s: %v", name, err)
		}
	}
	// One identity under two keys bound to two live actors is quarantined.
	first, second := f.Account(t), f.Account(t)
	k1, k2 := seedIdentity(t, f, first, 1), seedIdentity(t, f, second, 2)
	_, err := f.Service.ResolveIdentity(t.Context(), []string{k1, k2})
	wantCode(t, err, "authorization_quarantine")
	f.Exec(t, `UPDATE accounts SET status='deleted' WHERE id=$1`, first)
	f.Exec(t, `UPDATE account_identities SET live=false WHERE account_id=$1`, first)
	resolution, err := f.Service.ResolveIdentity(t.Context(), []string{k1, k2})
	if err != nil || resolution.AccountID != second || resolution.LifecycleGeneration != 2 || resolution.IdentityKey != k2 {
		t.Fatalf("%+v %v", resolution, err)
	}
	// A generation gap is quarantined.
	third := f.Account(t)
	k3 := seedIdentity(t, f, third, 3)
	f.Exec(t, `UPDATE accounts SET status='deleted' WHERE id=$1`, second)
	f.Exec(t, `UPDATE account_identities SET live=false WHERE account_id=$1`, second)
	if _, err := f.Service.ResolveIdentity(t.Context(), []string{k1, k3}); authority.CodeOf(err) != "authorization_quarantine" {
		t.Fatalf("gap: %v", err)
	}
}

func TestIdentityUnavailableRevokesSessionsAndIsIdempotentByNotification(t *testing.T) {
	f := authoritytest.New(t)
	account := f.Account(t)
	key := seedIdentity(t, f, account, 1)
	device := f.Device(t, account)
	first, _, _ := f.Session(t, account, device, "account", "", "")
	_, _, raw := f.Session(t, account, f.Device(t, account), "account", "", "")
	other := f.Account(t)
	otherSession, _, _ := f.Session(t, other, f.Device(t, other), "account", "", "")
	if _, err := f.Service.Refresh(t.Context(), refreshOf(f, raw, newAttempt(t), nil)); authority.CodeOf(err) != "invalid_command" {
		t.Fatalf("recoverable refresh without proof: %v", err)
	}

	digests := []identity.KeyedDigest{{KeyID: "notification.v1", Sum: [32]byte{1, 2, 3}}}
	command := authoritytest.NewID(t)
	stored, newly, err := f.Service.ReserveAppleNotification(t.Context(), digests, command, f.Now)
	if err != nil || stored != command || !newly {
		t.Fatalf("reserve %q %v %v", stored, newly, err)
	}
	again, newly, err := f.Service.ReserveAppleNotification(t.Context(), digests, authoritytest.NewID(t), f.Now)
	if err != nil || again != command || newly {
		t.Fatalf("second reserve %q %v %v", again, newly, err)
	}
	rotated := []identity.KeyedDigest{{KeyID: "notification.v2", Sum: [32]byte{9}}, digests[0]}
	if again, newly, err = f.Service.ReserveAppleNotification(t.Context(), rotated, authoritytest.NewID(t), f.Now); err != nil || again != command || newly {
		t.Fatalf("retained-key reserve %q %v %v", again, newly, err)
	}

	in := authority.IdentityUnavailable{CommandID: stored, AccountID: account, Candidates: []string{key}, NotificationDigest: hex.EncodeToString(make([]byte, 32)), At: f.Now}
	result, err := f.Service.MarkIdentityUnavailable(t.Context(), in)
	if err != nil || result.CommandID != stored || result.Revision != 2 { // after the sign-in's row (authoritytest.Session)
		t.Fatalf("%+v %v", result, err)
	}
	if n := count(t, f, `SELECT count(*) FROM accounts WHERE id=$1 AND status='identity_unavailable'`, account); n != 1 {
		t.Fatal("account not identity_unavailable")
	}
	if n := count(t, f, `SELECT count(*) FROM sessions WHERE account_id=$1 AND status='revoked' AND revoke_reason='identity_unavailable'`, account); n != 2 {
		t.Fatalf("%d sessions revoked", n)
	}
	if row, _ := sessionRow(t, f, otherSession); row.Status != "active" {
		t.Fatal("another account's session was revoked")
	}
	if row, _ := sessionRow(t, f, first); row.RevokedAt == nil || !row.RevokedAt.Equal(f.Now) {
		t.Fatalf("revoked_at %v", row.RevokedAt)
	}
	rows := readAudit(t, f, authority.ScopeAccount, account)
	if len(rows) != 2 || rows[1].Kind != "identity_unavailable" {
		t.Fatalf("audit %+v", rows)
	}
	retry, err := f.Service.MarkIdentityUnavailable(t.Context(), in)
	if err != nil || retry != result {
		t.Fatalf("retry %+v %v", retry, err)
	}
	in.CommandID = authoritytest.NewID(t)
	_, err = f.Service.MarkIdentityUnavailable(t.Context(), in)
	wantCode(t, err, "invalid_command")
	changed := in
	changed.CommandID, changed.NotificationDigest = stored, ""
	_, err = f.Service.MarkIdentityUnavailable(t.Context(), changed)
	wantCode(t, err, "command_id_reused")
	if n := len(readAudit(t, f, authority.ScopeAccount, account)); n != 2 {
		t.Fatalf("%d audit rows", n)
	}
	wrongAccount := in
	wrongAccount.CommandID, wrongAccount.AccountID = authoritytest.NewID(t), other
	_, err = f.Service.MarkIdentityUnavailable(t.Context(), wrongAccount)
	wantCode(t, err, "service_unavailable")
}

// Two identical first sign-ins race: the account primary key decides, the
// loser is security_busy, and its retry returns the winner's result.
func TestSignInConcurrentFirstAttemptsAgree(t *testing.T) {
	f := authoritytest.New(t)
	key := identityKey(t, "identity.v1")
	accountID := authoritytest.NewID(t)
	c := newSignIn(t, f, []string{key}, accountID, 1)
	type outcome struct {
		result authority.SignInResult
		err    error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			<-start
			result, err := f.Service.SignIn(context.Background(), c.in)
			results <- outcome{result, err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	t.Logf("outcomes: %v / %v", first.err, second.err)
	var winner authority.SignInResult
	switch {
	case first.err == nil && second.err == nil:
		if first.result != second.result {
			t.Fatalf("results differ: %+v %+v", first.result, second.result)
		}
		winner = first.result
	case first.err == nil && authority.CodeOf(second.err) == "security_busy":
		winner = first.result
	case second.err == nil && authority.CodeOf(first.err) == "security_busy":
		winner = second.result
	default:
		t.Fatalf("outcomes %v / %v", first.err, second.err)
	}
	retry, err := f.Service.SignIn(t.Context(), c.in)
	if err != nil || retry != winner {
		t.Fatalf("retry %+v %v, want %+v", retry, err, winner)
	}
	if n := count(t, f, `SELECT count(*) FROM sessions WHERE account_id=$1`, accountID); n != 1 {
		t.Fatalf("%d sessions", n)
	}
}

func TestReserveAppleNotificationConcurrentReservationsConverge(t *testing.T) {
	f := authoritytest.New(t)
	digests := []identity.KeyedDigest{{KeyID: "notification.v1", Sum: [32]byte{4}}}
	start := make(chan struct{})
	type outcome struct {
		command string
		newly   bool
		err     error
	}
	results := make(chan outcome, 8)
	for range 8 {
		go func() {
			<-start
			command, newly, err := f.Service.ReserveAppleNotification(context.Background(), digests, authoritytest.NewID(t), f.Now)
			results <- outcome{command, newly, err}
		}()
	}
	close(start)
	commands, newly := map[string]bool{}, 0
	for range 8 {
		o := <-results
		if o.err != nil {
			t.Fatal(o.err)
		}
		commands[o.command] = true
		if o.newly {
			newly++
		}
	}
	if len(commands) != 1 || newly != 1 {
		t.Fatalf("%d commands, %d newly reserved; want 1 and 1", len(commands), newly)
	}
}

// A quarantined identity is service_unavailable to the notification path,
// as legacy (apple_notifications.go:404-405; fix round 1).
func TestMarkIdentityUnavailableOnQuarantinedIdentityIsUnavailable(t *testing.T) {
	f := authoritytest.New(t)
	first, second := f.Account(t), f.Account(t)
	k1, k2 := seedIdentity(t, f, first, 1), seedIdentity(t, f, second, 2)
	_, err := f.Service.MarkIdentityUnavailable(t.Context(), authority.IdentityUnavailable{CommandID: authoritytest.NewID(t), AccountID: second,
		Candidates: []string{k1, k2}, At: f.Now})
	wantCode(t, err, "service_unavailable")
	if n := count(t, f, `SELECT count(*) FROM accounts WHERE id=$1 AND status='active'`, second); n != 1 {
		t.Fatal("account changed")
	}
}
