package authority

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Integration grants are revocable, read-only MCP credentials for one
// family, issued by an operator to that family's owner
// (security/integration_events.go, integration_prepared.go). The table holds
// only the verifier; the secret never reaches the authority. A grant is not
// revoked by ownership transfer, leaving, account deletion or purge: the read
// guard rechecks the live owner, membership and slot on every use.
const (
	// IntegrationValiditySeconds is the only accepted lifetime: 90 days from
	// the committed issuance time.
	IntegrationValiditySeconds = 7776000
	// IntegrationScopeFamilyRead is the only scope.
	IntegrationScopeFamilyRead = "family:read"

	integrationValidity = IntegrationValiditySeconds * time.Second
)

// integrationName is a printable ASCII label without padding (legacy
// validIntegrationName; mcp_integrations.name CHECK).
var integrationName = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9 ._-]{0,62}[A-Za-z0-9._-])?$`)

// ValidIntegrationName reports whether name is an acceptable grant label: at
// most 64 bytes of printable ASCII without padding (legacy
// security.ValidIntegrationName). Operator tooling validates proposals with
// it before preflight.
func ValidIntegrationName(name string) bool {
	return len(name) <= 64 && integrationName.MatchString(name)
}

// OperatorIntegration is security.OperatorIntegrationInput. A grant carries
// the fixed ValiditySeconds, never an absolute time: its issuance time is the
// commit time, sampled after the locks. A revocation names the stored grant
// and its bindings and carries no grant metadata. ExpectedRevision is the
// family's audit seq.
type OperatorIntegration struct {
	CommandID, OperatorID string
	Operation             string // "grant" or "revoke"
	GrantID               string
	FamilyID, AccountID   string
	Name, Scope, Verifier string
	ServerInstanceID      string
	RecoveryEpoch         uint64
	ValiditySeconds       uint64
	PolicyID              string
	EvidenceDigest        string
	ReasonCode            string
	ExpectedRevision      uint64
	ConfirmationExpiresAt time.Time
}

// IntegrationView is a stored grant (security.IntegrationView). It is not
// authorization: readers check the live account, family, owner, binding and
// expiry on every use. Verifier is lowercase hex; ServerInstanceID is this
// node's (the schema is single-node).
type IntegrationView struct {
	ID, AccountID, FamilyID, ServerInstanceID, Name, Scope, Verifier, Status string
	RecoveryEpoch                                                            uint64
	CreatedAt, ExpiresAt                                                     time.Time
}

// PreflightOperatorIntegration reports whether the command would commit now;
// a committed one is nil. It writes nothing and fixes no issuance time.
func (s *Service) PreflightOperatorIntegration(ctx context.Context, in OperatorIntegration) error {
	spec, err := s.integrationSpec(in)
	if err != nil {
		return err
	}
	return s.preflightOperator(ctx, spec)
}

// ReplayOperatorIntegration returns the stored result of an already
// committed command (found=false when there is none).
func (s *Service) ReplayOperatorIntegration(ctx context.Context, in OperatorIntegration) (Result, bool, error) {
	spec, err := s.integrationSpec(in)
	if err != nil {
		return Result{}, false, err
	}
	return s.replayOperator(ctx, spec)
}

// ExecuteOperatorIntegration issues or revokes a grant, or returns the
// original result of an exact retry, which is never restamped and never
// reactivates a revoked grant.
//
// Grant (integration_prepared.go:94-112, integration_events.go:156-182):
// the instance is this node's (invalid_command) and the epoch the node's
// (epoch_changed); the family is active (family_unavailable); AccountID is an
// active account and the family's active owner (owner_required); the grant ID
// is unused (invalid_command); the verifier is unused by any grant
// (integration_verifier_reused, also the verifier's unique constraint). The
// issuance time is the clock read after the locks, truncated to the UTC
// second; the grant expires exactly IntegrationValiditySeconds later. Locks:
// the owner's account, the family. Audit: integration_granted on the family
// and the owner's account.
//
// Revoke (integration_events.go:187-204): the grant exists with the same
// account, family, epoch and instance (integration_invalid) and is active
// (integration_revoked). Locks: the family. Audit: integration_revoked on the
// family. Invalidations: the grant's account in the family.
func (s *Service) ExecuteOperatorIntegration(ctx context.Context, in OperatorIntegration) (Result, error) {
	spec, err := s.integrationSpec(in)
	if err != nil {
		return Result{}, err
	}
	return s.executeOperator(ctx, spec)
}

func (s *Service) integrationSpec(in OperatorIntegration) (operatorSpec, error) {
	if !validID(in.CommandID) || !validID(in.OperatorID) || !validID(in.GrantID) || !validID(in.AccountID) || !validID(in.FamilyID) ||
		!validID(in.ServerInstanceID) || in.RecoveryEpoch < 1 || in.RecoveryEpoch > maxWireInteger {
		return operatorSpec{}, ErrInvalidCommand
	}
	payload := map[string]any{"integration_id": in.GrantID, "account_id": in.AccountID, "family_id": in.FamilyID,
		"server_instance_id": in.ServerInstanceID, "recovery_epoch": in.RecoveryEpoch, "operator_policy_id": in.PolicyID,
		"operator_evidence_digest": in.EvidenceDigest, "operator_reason_code": in.ReasonCode, "expected_revision": in.ExpectedRevision}
	spec := operatorSpec{operatorID: in.OperatorID, commandID: in.CommandID, familyID: in.FamilyID, expectedRevision: in.ExpectedRevision,
		confirmation: in.ConfirmationExpiresAt, wholeSecond: true}
	switch in.Operation {
	case "grant":
		if !validOperatorMeta(in.CommandID, in.OperatorID, in.PolicyID, in.EvidenceDigest, in.ReasonCode, in.ExpectedRevision, "integration_issued") ||
			!ValidIntegrationName(in.Name) || in.Scope != IntegrationScopeFamilyRead || !hexDigest.MatchString(in.Verifier) ||
			in.ValiditySeconds != IntegrationValiditySeconds {
			return operatorSpec{}, ErrInvalidCommand
		}
		hashed := clonePayload(payload)
		hashed["name"], hashed["scope"], hashed["verifier"], hashed["validity_seconds"] = in.Name, in.Scope, in.Verifier, in.ValiditySeconds
		spec.kind, spec.payload, spec.accounts = KindIntegrationGranted, hashed, []string{in.AccountID}
		spec.check = func(ctx context.Context, o *operatorTx) error { return s.checkIntegrationGrant(ctx, o, in) }
		spec.apply = func(ctx context.Context, o *operatorTx) (Result, error) {
			return applyIntegrationGrant(ctx, o, in, payload)
		}
	case "revoke":
		if !validOperatorMeta(in.CommandID, in.OperatorID, in.PolicyID, in.EvidenceDigest, in.ReasonCode, in.ExpectedRevision, "integration_revoked") ||
			in.Name != "" || in.Scope != "" || in.Verifier != "" || in.ValiditySeconds != 0 {
			return operatorSpec{}, ErrInvalidCommand
		}
		spec.kind, spec.payload = KindIntegrationRevoked, payload
		spec.check = func(ctx context.Context, o *operatorTx) error { return s.checkIntegrationRevoke(ctx, o, in) }
		spec.apply = func(ctx context.Context, o *operatorTx) (Result, error) {
			c := o.command
			if err := runBatch(ctx, c.tx, []batchStep{{`UPDATE mcp_integrations SET status='revoked', revoked_at=$2 WHERE id=$1 AND status='active'`,
				[]any{in.GrantID, c.now}, 1}}); err != nil {
				return Result{}, err
			}
			c.invalidate(in.AccountID, in.FamilyID)
			result, _, err := c.audit(ctx, in.FamilyID, []*chainHead{o.family.Chain}, payload)
			return result, err
		}
	default:
		return operatorSpec{}, ErrInvalidCommand
	}
	return spec, nil
}

func (s *Service) checkIntegrationGrant(ctx context.Context, o *operatorTx, in OperatorIntegration) error {
	if in.ServerInstanceID != s.instanceID {
		return ErrInvalidCommand
	}
	if in.RecoveryEpoch != s.epoch {
		return ErrEpochChanged
	}
	if !o.familyOK || o.family.Status != "active" {
		return ErrFamilyUnavailable
	}
	account, ok := o.accounts[in.AccountID]
	if !ok || account.Status != "active" || o.family.OwnerAccountID != in.AccountID {
		return ErrOwnerRequired
	}
	role, status, err := readMembership(ctx, o.tx, in.FamilyID, in.AccountID)
	if err != nil {
		return err
	}
	if role != "owner" || status != "active" {
		return ErrOwnerRequired
	}
	verifier, _ := hex.DecodeString(in.Verifier)
	var idUsed, verifierUsed bool
	if err := o.tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM mcp_integrations WHERE id=$1), EXISTS (SELECT 1 FROM mcp_integrations WHERE verifier=$2)`,
		in.GrantID, verifier).Scan(&idUsed, &verifierUsed); err != nil {
		return unavailable("read integrations", err)
	}
	if idUsed {
		return ErrInvalidCommand
	}
	if verifierUsed {
		return ErrIntegrationVerifierReused
	}
	return nil
}

func applyIntegrationGrant(ctx context.Context, o *operatorTx, in OperatorIntegration, payload map[string]any) (Result, error) {
	c := o.command
	created := c.now // the command time after the locks, on a whole second
	expires := created.Add(integrationValidity)
	verifier, _ := hex.DecodeString(in.Verifier)
	if err := mapIntegrationWriteError(runBatch(ctx, c.tx, []batchStep{{`INSERT INTO mcp_integrations(id,account_id,family_id,recovery_epoch,name,scope,verifier,status,created_at,expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'active',$8,$9)`, []any{in.GrantID, in.AccountID, in.FamilyID, int64(in.RecoveryEpoch), in.Name, in.Scope, verifier,
		created, expires}, 1}})); err != nil {
		return Result{}, err
	}
	audit := clonePayload(payload)
	audit["name"], audit["scope"], audit["created_at"], audit["expires_at"] = in.Name, in.Scope, created.Format(time.RFC3339), expires.Format(time.RFC3339)
	result, _, err := c.audit(ctx, in.FamilyID, []*chainHead{o.family.Chain, o.accounts[in.AccountID].Chain}, audit)
	return result, err
}

func (s *Service) checkIntegrationRevoke(ctx context.Context, o *operatorTx, in OperatorIntegration) error {
	var account, family, status string
	var epoch uint64
	err := o.tx.QueryRow(ctx, `SELECT account_id::text, family_id::text, recovery_epoch, status::text FROM mcp_integrations WHERE id=$1`, in.GrantID).
		Scan(&account, &family, &epoch, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrIntegrationInvalid
	}
	if err != nil {
		return unavailable("read integration", err)
	}
	if account != in.AccountID || family != in.FamilyID || epoch != in.RecoveryEpoch || in.ServerInstanceID != s.instanceID {
		return ErrIntegrationInvalid
	}
	if status != "active" {
		return ErrIntegrationRevoked
	}
	return nil
}

// mapIntegrationWriteError maps a unique violation on the verifier (another
// family's concurrent grant committed it first) to
// integration_verifier_reused and one on the grant ID to invalid_command.
func mapIntegrationWriteError(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return err
	}
	switch pgErr.ConstraintName {
	case "mcp_integrations_verifier_key":
		return fmt.Errorf("authority: %w", errors.Join(pgErr, ErrIntegrationVerifierReused))
	case "mcp_integrations_pkey":
		return fmt.Errorf("authority: %w", errors.Join(pgErr, ErrInvalidCommand))
	}
	return err
}

// ListFamilyIntegrations returns every grant of familyID, in any status,
// ordered by issuance time then ID (security/integration_operator.go). It is
// operator metadata, never authorization; the views carry the verifier and
// the caller decides what to disclose.
func (s *Service) ListFamilyIntegrations(ctx context.Context, familyID string) ([]IntegrationView, error) {
	if !validID(familyID) {
		return nil, ErrInvalidCommand
	}
	tx, err := s.beginReadOnly(ctx)
	if err != nil {
		return nil, errors.Join(err, ErrUnavailable)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	rows, err := tx.Query(ctx, `SELECT id::text, account_id::text, family_id::text, recovery_epoch, name, scope, encode(verifier,'hex'), status::text,
			created_at, expires_at
		FROM mcp_integrations WHERE family_id=$1 ORDER BY created_at, mcp_integrations.id`, familyID)
	if err != nil {
		return nil, unavailable("list integrations", err)
	}
	views, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (IntegrationView, error) {
		var v IntegrationView
		err := row.Scan(&v.ID, &v.AccountID, &v.FamilyID, &v.RecoveryEpoch, &v.Name, &v.Scope, &v.Verifier, &v.Status, &v.CreatedAt, &v.ExpiresAt)
		v.ServerInstanceID, v.CreatedAt, v.ExpiresAt = s.instanceID, v.CreatedAt.UTC(), v.ExpiresAt.UTC()
		return v, err
	})
	if err != nil {
		return nil, unavailable("list integrations", err)
	}
	if views == nil {
		views = []IntegrationView{}
	}
	return views, nil
}

func clonePayload(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+4)
	for k, v := range in {
		out[k] = v
	}
	return out
}
