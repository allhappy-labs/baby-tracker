package testkit

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"github.com/jackc/pgx/v5/pgxpool"
)

// IntegrationOperatorID is the operator that issues and revokes the fixture's
// integration grants.
const IntegrationOperatorID = "90000000-0000-4000-8000-000000000001"

// IntegrationPool is a dedicated pool on the runtime role whose sessions are
// read-only by default, as production's MCP pool is; NewIntegrationReader
// refuses any other pool.
func (h *Harness) IntegrationPool(t *testing.T, maxConns int32) *pgxpool.Pool {
	t.Helper()
	config := h.Runtime.Config()
	config.MaxConns = maxConns
	config.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// IntegrationReader is the authority's MCP read guard over a fresh
// IntegrationPool of four connections.
func (h *Harness) IntegrationReader(t *testing.T) *authority.IntegrationReader {
	t.Helper()
	reader, err := authority.NewIntegrationReader(h.Authority, h.IntegrationPool(t, 4))
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

// IssueIntegration grants grantID (family:read) to the owner for the
// fixture family through the operator command, at the family's current
// revision, and returns the credential.
func (h *Harness) IssueIntegration(t *testing.T, grantID string) string {
	t.Helper()
	secret := make([]byte, authority.IntegrationSecretBytes)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	verifier, err := authority.IntegrationVerifier(grantID, secret)
	if err != nil {
		t.Fatal(err)
	}
	input := h.integrationCommand(t, "grant", grantID)
	input.Name, input.Scope, input.Verifier, input.ValiditySeconds = "Claude Desktop", authority.IntegrationScopeFamilyRead,
		hex.EncodeToString(verifier[:]), authority.IntegrationValiditySeconds
	input.ReasonCode = "integration_issued"
	if _, err := h.Authority.ExecuteOperatorIntegration(t.Context(), input); err != nil {
		t.Fatalf("grant integration: %v", err)
	}
	credential, err := authority.FormatIntegrationCredential(grantID, secret)
	if err != nil {
		t.Fatal(err)
	}
	return credential
}

// RevokeIntegration revokes the owner's grant grantID.
func (h *Harness) RevokeIntegration(t *testing.T, grantID string) {
	t.Helper()
	input := h.integrationCommand(t, "revoke", grantID)
	input.ReasonCode = "integration_revoked"
	if _, err := h.Authority.ExecuteOperatorIntegration(t.Context(), input); err != nil {
		t.Fatalf("revoke integration: %v", err)
	}
}

func (h *Harness) integrationCommand(t *testing.T, operation, grantID string) authority.OperatorIntegration {
	t.Helper()
	revision, err := h.Authority.FamilyRevision(t.Context(), h.FamilyID)
	if err != nil {
		t.Fatal(err)
	}
	return authority.OperatorIntegration{CommandID: h.commandID(), OperatorID: IntegrationOperatorID, Operation: operation,
		GrantID: grantID, FamilyID: h.FamilyID, AccountID: h.Owner.AccountID, ServerInstanceID: h.composition.InstanceID, RecoveryEpoch: h.composition.RecoveryEpoch,
		PolicyID: "Integration.Policy_test-v1", EvidenceDigest: strings.Repeat("e", 64), ExpectedRevision: revision,
		ConfirmationExpiresAt: h.Now.Add(10 * time.Minute).Truncate(time.Second)}
}
