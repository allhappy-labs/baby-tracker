package testkit

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/auth"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/family"
	"babytracker/server/internal/identity"
)

// Composition is what Compose builds the harness services with. A zero field
// is testkit's default (the values New composes with).
type Composition struct {
	InstanceID     string
	RecoveryEpoch  uint64
	TokenKeys      identity.DigestKeyring
	IdentityKeys   identity.DigestKeyring // zero: testkit's defaults
	ContinuityKeys identity.DigestKeyring
	InvitationKeys identity.DigestKeyring
	PageKeys       identity.DigestKeyring
	Origin         string    // "": testkit's origin
	Now            time.Time // zero: the database's latest noon (New's clock)
	Hosted         bool
	ActorKey       [32]byte // zero: testkit's actor-ID key
}

// withDefaults fills every zero field with testkit's default.
func (c Composition) withDefaults() Composition {
	if c.InstanceID == "" {
		c.InstanceID = testInstance
	}
	if c.RecoveryEpoch == 0 {
		c.RecoveryEpoch = RecoveryEpoch
	}
	for _, k := range []struct {
		ring *identity.DigestKeyring
		id   string
		fill byte
	}{{&c.TokenKeys, "session.v1", 0x22}, {&c.IdentityKeys, "identity.v1", 0x11}, {&c.ContinuityKeys, "device.v1", 0x33},
		{&c.InvitationKeys, "invitation.v1", 0x66}, {&c.PageKeys, "page.v1", 0x77}} {
		if k.ring.Current.ID == "" {
			*k.ring = keyring(k.id, k.fill)
		}
	}
	if c.Origin == "" {
		c.Origin = testOrigin
	}
	if c.ActorKey == ([32]byte{}) {
		for i := range c.ActorKey {
			c.ActorKey[i] = 0x55
		}
	}
	return c
}

// Compose builds the harness services (Authority, Auth, Identity, Family,
// Service, Importer) over an existing database, without seeding anything:
// no server_state, account, family or session. Owner, Caregiver, Outsider and
// FamilyID stay zero; the clock, Now, moves only when the test moves it.
func Compose(t *testing.T, db Postgres, c Composition) *Harness {
	t.Helper()
	c = c.withDefaults()
	h := &Harness{t: t, Admin: db.Admin, Runtime: db.Runtime, Migrator: db.Migrator, Maintenance: db.Maintenance, Now: c.Now,
		bundles: map[string]auth.SessionBundle{}, deviceCredentials: map[string]string{}, composition: c}
	if h.Now.IsZero() {
		h.Now = databaseNoon(t, db)
	}
	clock := func() time.Time { return h.Now }
	var err error
	h.authorityOptions = authority.Options{Pool: db.Runtime, Now: clock, InstanceID: c.InstanceID, RecoveryEpoch: c.RecoveryEpoch,
		TokenKeys: c.TokenKeys, MaxCaregivers: 10, MaxInvitations: 10, RequireFamilySyncEntitlement: c.Hosted, Allowed: family.Allowed,
		EntitlementExempt: authority.LegacyEntitlementExempt}
	h.Authority, err = authority.New(h.authorityOptions)
	if err != nil {
		t.Fatal(err)
	}
	h.guard = h.Authority
	verifier := identity.IdentityVerifierFunc(func(_ context.Context, token, nonce string) (identity.VerifiedIdentity, error) {
		subject, bound, ok := strings.Cut(token, "|")
		if !ok || subject == "" || bound != nonce {
			return identity.VerifiedIdentity{}, fmt.Errorf("nonce mismatch")
		}
		return identity.VerifiedIdentity{Provider: "apple", Audience: "com.example.babytracker", Subject: subject, IssuedAt: h.Now.Add(-time.Minute), ExpiresAt: h.Now.Add(time.Hour)}, nil
	})
	h.configureIdentity(t, verifier)
	h.Family, err = family.New(family.Options{Authority: h.Authority, InvitationKeys: c.InvitationKeys, Origin: c.Origin, Now: clock,
		InvitationTTL: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h.setServices()
	return h
}
