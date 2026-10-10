package authority_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"babytracker/server/internal/identity"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCodeOfKeepsLegacyCodeStrings(t *testing.T) {
	if authority.CodeOf(nil) != "" {
		t.Fatal("nil must have no code")
	}
	if got := authority.CodeOf(errors.New("boom")); got != "service_unavailable" {
		t.Fatalf("unknown error code %q", got)
	}
	if got := authority.CodeOf(fmt.Errorf("lookup: %w", authority.ErrCommandIDReused)); got != "command_id_reused" {
		t.Fatalf("wrapped code %q", got)
	}
	if got := authority.CodeOf(authority.Code("integration_unauthorized")); got != "integration_unauthorized" {
		t.Fatalf("ad hoc code %q", got)
	}
	// security/types.go, family/types.go, auth/refresh.go and the codes the
	// legacy security state machine returns.
	for err, want := range map[error]string{
		authority.ErrLedgerCorrupt: "ledger_corrupt", authority.ErrTrustedHeadRequired: "trusted_head_required",
		authority.ErrUnsupportedEvent: "unsupported_security_event", authority.ErrMembershipRevoked: "membership_revoked",
		authority.ErrUnavailable: "service_unavailable", authority.ErrInvalidRequest: "invalid_request",
		authority.ErrFamilySlotOccupied: "family_slot_occupied", authority.ErrOwnerRequired: "owner_required",
		authority.ErrReadOnlyCaregiver: "read_only_caregiver", authority.ErrFamilyUnavailable: "family_unavailable",
		authority.ErrInvitationInvalid: "invitation_invalid", authority.ErrInvitationExpired: "invitation_expired",
		authority.ErrInvitationConsumed: "invitation_consumed", authority.ErrInvitationRevoked: "invitation_revoked",
		authority.ErrCaregiverLimit: "caregiver_limit", authority.ErrInvitationLimit: "invitation_limit",
		authority.ErrConfirmationRequired: "confirmation_required", authority.ErrProfileNotRequired: "profile_not_required",
		authority.ErrEntitlementRequired: "entitlement_required", authority.ErrSecretUnavailable: "secret_unavailable",
		authority.ErrCommandIDReused: "command_id_reused", authority.ErrAuthorizationQuarantine: "authorization_quarantine",
		authority.ErrHostedAdapterUnavailable: "hosted_adapter_unavailable", authority.ErrCredentialKeyUnavailable: "credential_key_unavailable",
		authority.ErrRecoveryOnly: "recovery_only", authority.ErrRecoveryExpired: "recovery_expired",
		authority.ErrRecoveryForfeited: "recovery_forfeited", authority.ErrOwnershipTransferRequired: "ownership_transfer_required",
		authority.ErrAccessExpired: "access_expired", authority.ErrSessionRevoked: "session_revoked",
		authority.ErrRefreshReused: "refresh_reused", authority.ErrReauthenticationRequired: "reauthentication_required",
		authority.ErrEpochChanged: "epoch_changed", authority.ErrInvalidCommand: "invalid_command",
		authority.ErrScopeFailed: "scope_failed", authority.ErrSecurityBusy: "security_busy",
		authority.ErrInvalidConfirmation: "invalid_confirmation", authority.ErrStaleSecurityRevision: "stale_security_revision",
		authority.ErrOwnerIdentityUnavailable: "owner_identity_unavailable", authority.ErrIntegrationVerifierReused: "integration_verifier_reused",
		authority.ErrIntegrationInvalid: "integration_invalid", authority.ErrIntegrationRevoked: "integration_revoked",
		authority.ErrCredentialConflict: "credential_conflict", authority.ErrExportAuthorizationChanged: "export_authorization_changed",
	} {
		if got := authority.CodeOf(err); got != want || err.Error() != want {
			t.Errorf("code %q / %q, want %q", got, err.Error(), want)
		}
	}
}

func TestNewValidatesOptions(t *testing.T) {
	config, err := pgxpool.ParseConfig("postgres://unused@127.0.0.1:1/unused")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), config) // lazy: never connects
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	key := identity.DigestKey{ID: "session.v1"}
	key.Secret[0] = 1
	valid := func() authority.Options {
		return authority.Options{Pool: pool, Now: time.Now, InstanceID: "11111111-1111-4111-8111-111111111111", RecoveryEpoch: 1,
			TokenKeys: identity.DigestKeyring{Current: key}, Allowed: authoritytest.Allowed}
	}
	if _, err := authority.New(valid()); err != nil {
		t.Fatalf("valid options rejected: %v", err)
	}
	for name, mutate := range map[string]func(*authority.Options){
		"no pool":          func(o *authority.Options) { o.Pool = nil },
		"no clock":         func(o *authority.Options) { o.Now = nil },
		"bad instance":     func(o *authority.Options) { o.InstanceID = "instance" },
		"zero instance":    func(o *authority.Options) { o.InstanceID = "00000000-0000-0000-0000-000000000000" },
		"zero epoch":       func(o *authority.Options) { o.RecoveryEpoch = 0 },
		"huge epoch":       func(o *authority.Options) { o.RecoveryEpoch = 1 << 53 },
		"no token key":     func(o *authority.Options) { o.TokenKeys = identity.DigestKeyring{} },
		"bad token key id": func(o *authority.Options) { o.TokenKeys.Current.ID = "Session" },
		"reused key secret": func(o *authority.Options) {
			o.TokenKeys.Previous = []identity.DigestKey{{ID: "session.v0", Secret: key.Secret}}
		},
		"negative limits": func(o *authority.Options) { o.MaxCaregivers = -1 },
		"no policy":       func(o *authority.Options) { o.Allowed = nil },
	} {
		options := valid()
		mutate(&options)
		if _, err := authority.New(options); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBeginOpensAnAuthorityTransaction(t *testing.T) {
	f := authoritytest.New(t)
	ctx := t.Context()
	account := f.Account(t)
	tx, err := f.Service.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var authorityContext bool
	if err := tx.QueryRow(ctx, `SELECT app.authority_context()`).Scan(&authorityContext); err != nil || !authorityContext {
		t.Fatalf("authority_context=%v err=%v", authorityContext, err)
	}
	// Authority columns are writable only in an authority transaction.
	if tag, err := tx.Exec(ctx, `UPDATE accounts SET lifecycle_generation=2 WHERE id=$1`, account); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("authority write rows=%d err=%v", tag.RowsAffected(), err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// The flag is transaction-local: a plain transaction on the same pool
	// does not inherit it.
	for range 4 {
		plain, err := f.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := plain.QueryRow(ctx, `SELECT app.authority_context()`).Scan(&authorityContext); err != nil || authorityContext {
			t.Fatalf("flag leaked: %v err=%v", authorityContext, err)
		}
		_ = plain.Rollback(ctx)
	}
}
