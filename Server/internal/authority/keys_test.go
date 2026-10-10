package authority_test

import (
	"encoding/hex"
	"slices"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
)

// The inventory names every key a live credential still needs, sorted
// bytewise: active sessions' token keys, active devices' continuity keys,
// every identity binding's key (re-sign-in after deletion resolves non-live
// rows), and every invitation's key except revoked ones (an exact retry of a
// redemption resolves the redeemed invitation's token again).
func TestReferencedKeyIDsAreSortedAndIgnoreRevoked(t *testing.T) {
	f := authoritytest.New(t)
	ctx := t.Context()

	empty, err := f.Service.ReferencedKeyIDs(ctx, authority.ModeSelfHosted)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.SessionToken)+len(empty.DeviceContinuity)+len(empty.Identity)+len(empty.Invitation) != 0 {
		t.Fatalf("empty database inventory = %+v", empty)
	}

	owner := newMember(t, f) // identity.v1, continuity.v1, session.v1
	family := createFamily(t, f, f.Service, owner)

	// Sessions: "session-b" sorts before "session.a" bytewise but not under
	// a linguistic collation.
	second := newMember(t, f)
	f.Exec(t, `UPDATE sessions SET token_key_id='session.a' WHERE id=$1`, second.session)
	third := newMember(t, f)
	f.Exec(t, `UPDATE sessions SET token_key_id='session-b' WHERE id=$1`, third.session)
	for keyID, status := range map[string]string{"session.revoked": "revoked", "session.consumed": "consumed"} {
		m := newMember(t, f)
		f.Exec(t, `UPDATE sessions SET token_key_id=$2 WHERE id=$1`, m.session, keyID)
		if status == "revoked" {
			f.Exec(t, `UPDATE sessions SET status='revoked', revoked_at=$2, revoke_reason='device_revoked' WHERE id=$1`, m.session, f.Now)
		} else {
			f.Exec(t, `UPDATE sessions SET status='consumed', consumed_at=$2, child_id=$1 WHERE id=$1`, m.session, f.Now)
		}
	}

	// A consumed parent whose receipt still recovers an active child keeps
	// its key: a lost-acknowledgement retry presents the parent's refresh
	// token. A receipt whose child is no longer active pins nothing.
	for keyID, childStatus := range map[string]string{"session.parent": "active", "session.parent-dead": "revoked"} {
		m := newMember(t, f)
		childID, _, _ := f.Session(t, m.account, m.device, "account", "", "")
		f.Exec(t, `UPDATE sessions SET token_key_id=$2, status='consumed', consumed_at=$3, child_id=$4 WHERE id=$1`, m.session, keyID, f.Now, childID)
		f.Exec(t, `INSERT INTO refresh_receipts(parent_id,child_id,attempt_id,account_id,device_id,material,material_digest,created_at,effective_deadline,refresh_expires_at)
			VALUES ($1,$2,gen_random_uuid(),$3,$4,'\x01'::bytea,sha256('\x01'::bytea),$5,$6,$6)`, m.session, childID, m.account, m.device, f.Now, f.Now.Add(time.Hour))
		if childStatus == "revoked" {
			f.Exec(t, `UPDATE sessions SET status='revoked', revoked_at=$2, revoke_reason='device_revoked' WHERE id=$1`, childID, f.Now)
		}
	}

	// Devices: active ones only.
	f.Exec(t, `UPDATE devices SET continuity_key_id='continuity-b' WHERE id=$1`, second.device)
	f.Exec(t, `UPDATE devices SET continuity_key_id='continuity.a' WHERE id=$1`, third.device)
	revokedDevice := f.Device(t, owner.account)
	f.Exec(t, `UPDATE devices SET continuity_key_id='continuity.revoked', status='revoked', revoked_at=$2 WHERE id=$1`, revokedDevice, f.Now)

	// Identities: every binding, live or not.
	deleted := f.Account(t)
	digest, _ := hex.DecodeString(identityKey(t, "x")[len("hmac-sha256:x:"):])
	f.Exec(t, `INSERT INTO account_identities(provider,identity_key_id,identity_digest,account_id,lifecycle_generation,audience,subject,live,created_at)
		VALUES ('apple','identity-old',$1,$2,1,'com.example.app','subject',false,$3)`, digest, deleted, f.Now)

	// Invitations: active and redeemed ones; revoked ones are ignored.
	active := invitationOf(t, f, owner, family, "caregiver")
	active.TokenDigest = identityKey(t, "invite.active")
	redeemed := invitationOf(t, f, owner, family, "caregiver")
	redeemed.TokenDigest = identityKey(t, "invite-redeemed")
	revoked := invitationOf(t, f, owner, family, "caregiver")
	revoked.TokenDigest = identityKey(t, "invite.revoked")
	if _, err := f.Service.CreateInvitation(ctx, active); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Service.CreateInvitation(ctx, redeemed); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Service.CreateInvitation(ctx, revoked); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Service.RedeemInvitation(ctx, redeemOf(t, f, newMember(t, f), redeemed.TokenDigest)); err != nil {
		t.Fatal(err)
	}
	f.Exec(t, `UPDATE invitations SET status='revoked', revoked_at=$2, updated_at=$2 WHERE id=$1`, revoked.InvitationID, f.Now)

	got, err := f.Service.ReferencedKeyIDs(ctx, authority.ModeSelfHosted)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name      string
		got, want []string
	}{
		{"session token", got.SessionToken, []string{"session-b", "session.a", "session.parent", "session.v1"}},
		{"device continuity", got.DeviceContinuity, []string{"continuity-b", "continuity.a", "continuity.v1"}},
		{"identity", got.Identity, []string{"identity-old", "identity.v1"}},
		{"invitation", got.Invitation, []string{"invite-redeemed", "invite.active"}},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s keys = %q, want %q", c.name, c.got, c.want)
		}
	}

	if t.Failed() {
		return
	}
	// Each call returns fresh slices.
	got.SessionToken[0] = "mutated"
	again, err := f.Service.ReferencedKeyIDs(ctx, authority.ModeSelfHosted)
	if err != nil {
		t.Fatal(err)
	}
	if again.SessionToken[0] != "session-b" {
		t.Fatalf("inventory shared its slices: %q", again.SessionToken)
	}
}

// A session whose lifetime has ended pins no key even while its status stays
// active (nothing moves an abandoned session out of active): past
// deadline_at or refresh_expires_at, of another recovery epoch, or,
// self-hosted only, a year past its lineage's sign-in (effectiveDeadline;
// hosted refresh honours deadline_at alone). A receipt pins its parent's key
// only while its child is live and the receipt's effective deadline has not
// passed. Live sessions, one microsecond before their end, still pin.
func TestReferencedKeyIDsReleaseSessionsPastTheirLifetime(t *testing.T) {
	f := authoritytest.New(t)
	ctx := t.Context()
	keyed := func(keyID string) member {
		m := newMember(t, f)
		f.Exec(t, `UPDATE sessions SET token_key_id=$2 WHERE id=$1`, m.session, keyID)
		return m
	}
	ended := keyed("session.deadline")
	f.Exec(t, `UPDATE sessions SET access_expires_at=$2, refresh_expires_at=$2, deadline_at=$2 WHERE id=$1`, ended.session, f.Now)
	refreshEnded := keyed("session.refresh")
	f.Exec(t, `UPDATE sessions SET access_expires_at=$2, refresh_expires_at=$2 WHERE id=$1`, refreshEnded.session, f.Now)
	oldEpoch := keyed("session.epoch")
	f.Exec(t, `UPDATE sessions SET recovery_epoch=$2 WHERE id=$1`, oldEpoch.session, int64(authoritytest.RecoveryEpoch+1))
	lineage := keyed("session.lineage")
	f.Exec(t, `UPDATE sessions SET lineage_started_at=$2 WHERE id=$1`, lineage.session, f.Now.Add(-365*24*time.Hour))
	edge := keyed("session.edge")
	f.Exec(t, `UPDATE sessions SET access_expires_at=$2, refresh_expires_at=$2, deadline_at=$2 WHERE id=$1`, edge.session, f.Now.Add(time.Microsecond))

	// Receipts: a parent pins only while its child is live and the
	// receipt's effective deadline is ahead.
	receipt := func(parentKey string, receiptDeadline time.Time) (child string) {
		m := newMember(t, f)
		child, _, _ = f.Session(t, m.account, m.device, "account", "", "")
		f.Exec(t, `UPDATE sessions SET token_key_id=$2, status='consumed', consumed_at=$3, child_id=$4 WHERE id=$1`, m.session, parentKey, f.Now, child)
		f.Exec(t, `INSERT INTO refresh_receipts(parent_id,child_id,attempt_id,account_id,device_id,material,material_digest,created_at,effective_deadline,refresh_expires_at)
			VALUES ($1,$2,gen_random_uuid(),$3,$4,'\x01'::bytea,sha256('\x01'::bytea),$5,$6,$6)`, m.session, child, m.account, m.device, f.Now, receiptDeadline)
		return child
	}
	receipt("session.parent-live", f.Now.Add(time.Hour))
	receipt("session.parent-receipt-ended", f.Now)
	expiredChild := receipt("session.parent-child-ended", f.Now.Add(time.Hour))
	f.Exec(t, `UPDATE sessions SET access_expires_at=$2, refresh_expires_at=$2, deadline_at=$2 WHERE id=$1`, expiredChild, f.Now)
	lineageChild := receipt("session.parent-child-lineage", f.Now.Add(time.Hour))
	f.Exec(t, `UPDATE sessions SET lineage_started_at=$2 WHERE id=$1`, lineageChild, f.Now.Add(-365*24*time.Hour))

	for mode, want := range map[authority.Mode][]string{
		authority.ModeSelfHosted: {"session.edge", "session.parent-live", "session.v1"},
		authority.ModeHosted:     {"session.edge", "session.lineage", "session.parent-child-lineage", "session.parent-live", "session.v1"},
	} {
		got, err := f.Service.ReferencedKeyIDs(ctx, mode)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got.SessionToken, want) {
			t.Errorf("%s session token keys = %q, want %q", mode, got.SessionToken, want)
		}
	}
	if _, err := f.Service.ReferencedKeyIDs(ctx, "other"); err == nil {
		t.Fatal("inventory accepted an unknown mode")
	}
}
