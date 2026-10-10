package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/operator"
	"babytracker/server/internal/protocol"
)

// IntegrationCommands is the private MCP credential command family. It is
// enabled only by a mounted BT_OPERATOR_INTEGRATION_POLICY_ID_FILE.
type IntegrationCommands interface {
	PrepareIntegration(context.Context, operator.IntegrationProposal) (operator.Confirmation, error)
	IssueIntegration(context.Context, operator.IntegrationProposal, operator.Confirmation) (authority.Result, error)
	RevokeIntegration(context.Context, operator.IntegrationProposal, operator.Confirmation) (authority.Result, error)
	ListIntegrations(context.Context, string) ([]authority.IntegrationView, error)
}

// mcpLabelRule is stated in every mcp help text, not only when a label is
// refused: a syntactically valid label can still be a personal name.
const mcpLabelRule = "--name is a client label kept forever in the authority log; never put a person's or baby's name in it (use e.g. claude-desktop-laptop)"

const mcpUsage = "usage: mcp issue prepare|commit, mcp revoke prepare|commit, or mcp list --family-id <uuid>; " + mcpLabelRule

type mcpPrepared struct {
	Proposal     operator.IntegrationProposal `json:"proposal"`
	Confirmation operator.Confirmation        `json:"confirmation"`
	SecretFile   string                       `json:"secret_file,omitempty"`
}

// mcpListedIntegration is the complete listing disclosure: metadata only,
// never the verifier or any credential material.
type mcpListedIntegration struct {
	GrantID          string    `json:"grant_id"`
	AccountID        string    `json:"account_id"`
	FamilyID         string    `json:"family_id"`
	ServerInstanceID string    `json:"server_instance_id"`
	RecoveryEpoch    uint64    `json:"recovery_epoch"`
	Name             string    `json:"name"`
	Scope            string    `json:"scope"`
	Status           string    `json:"status"`
	CreatedAt        time.Time `json:"created_at"`
	ExpiresAt        time.Time `json:"expires_at"`
}

func runMCP(ctx context.Context, arguments []string, commands IntegrationCommands, output io.Writer) error {
	if len(arguments) >= 2 && arguments[1] == "list" {
		values, err := parseOperatorFlags(arguments[2:], []string{"--family-id"})
		if err != nil {
			return err
		}
		return runMCPList(ctx, values["--family-id"], commands, output)
	}
	if len(arguments) < 3 || (arguments[1] != "issue" && arguments[1] != "revoke") || (arguments[2] != "prepare" && arguments[2] != "commit") {
		return errors.New(mcpUsage)
	}
	operation, phase := arguments[1], arguments[2]
	values, err := parseOperatorFlags(arguments[3:], mcpFlagNames(operation, phase))
	if err != nil {
		return err
	}
	proposal, err := mcpProposal(operation, values)
	if err != nil {
		return err
	}
	switch {
	case operation == "issue" && phase == "prepare":
		return runMCPIssuePrepare(ctx, proposal, values["--secret-output"], commands, output)
	case operation == "issue":
		return runMCPIssueCommit(ctx, proposal, values, commands, output)
	case phase == "prepare":
		confirmation, err := commands.PrepareIntegration(ctx, proposal)
		if err != nil {
			return mcpError(err)
		}
		return json.NewEncoder(output).Encode(mcpPrepared{Proposal: proposal, Confirmation: confirmation})
	default:
		confirmation, err := parseConfirmation(values)
		if err != nil {
			return err
		}
		result, err := commands.RevokeIntegration(ctx, proposal, confirmation)
		if err != nil {
			return mcpError(err)
		}
		return json.NewEncoder(output).Encode(result)
	}
}

// runMCPIssuePrepare generates the grant ID and secret exactly once, binds the
// secret's verifier into the confirmed proposal, and persists the credential
// only in a new protected file. Neither stdout nor any error carries it.
func runMCPIssuePrepare(ctx context.Context, proposal operator.IntegrationProposal, secretPath string, commands IntegrationCommands, output io.Writer) error {
	if err := checkSecretDestination(secretPath); err != nil {
		return err
	}
	grantID, err := newGrantID()
	if err != nil {
		return err
	}
	secret, err := authority.NewIntegrationSecret()
	if err != nil {
		return err
	}
	defer clear(secret)
	credential, err := authority.FormatIntegrationCredential(grantID, secret)
	if err != nil {
		return errors.New("integration credential generation failed")
	}
	_, verifier, ok := authority.IntegrationCredentialVerifier(credential)
	if !ok {
		return errors.New("integration credential generation failed")
	}
	proposal.GrantID, proposal.Verifier = grantID, verifier
	confirmation, err := commands.PrepareIntegration(ctx, proposal)
	if err != nil {
		return mcpError(err)
	}
	if err := createSecretFile(secretPath, []byte(credential)); err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(mcpPrepared{Proposal: proposal, Confirmation: confirmation, SecretFile: secretPath})
}

// runMCPIssueCommit reads the protected file written by prepare and commits
// only if its credential matches the confirmed grant ID and verifier. It never
// generates, rewrites or removes the secret file.
func runMCPIssueCommit(ctx context.Context, proposal operator.IntegrationProposal, values map[string]string, commands IntegrationCommands, output io.Writer) error {
	confirmation, err := parseConfirmation(values)
	if err != nil {
		return err
	}
	credential, err := readSecretFile(values["--secret-file"])
	if err != nil {
		return err
	}
	grantID, verifier, ok := authority.IntegrationCredentialVerifier(credential)
	if !ok {
		return errors.New("secret_file_invalid: --secret-file does not contain exactly one btmcp1 credential written by mcp issue prepare")
	}
	if grantID != proposal.GrantID || verifier != proposal.Verifier {
		return errors.New("secret_file_mismatch: --secret-file does not belong to this confirmed proposal (--grant-id and --verifier); nothing was committed")
	}
	result, err := commands.IssueIntegration(ctx, proposal, confirmation)
	if err != nil {
		return mcpError(err)
	}
	return json.NewEncoder(output).Encode(result)
}

func runMCPList(ctx context.Context, familyID string, commands IntegrationCommands, output io.Writer) error {
	views, err := commands.ListIntegrations(ctx, familyID)
	if err != nil {
		return mcpError(err)
	}
	listed := make([]mcpListedIntegration, 0, len(views))
	for _, view := range views {
		listed = append(listed, mcpListedIntegration{GrantID: view.ID, AccountID: view.AccountID, FamilyID: view.FamilyID,
			ServerInstanceID: view.ServerInstanceID, RecoveryEpoch: view.RecoveryEpoch, Name: view.Name, Scope: view.Scope,
			Status: view.Status, CreatedAt: view.CreatedAt, ExpiresAt: view.ExpiresAt})
	}
	return json.NewEncoder(output).Encode(struct {
		FamilyID     string                 `json:"family_id"`
		Integrations []mcpListedIntegration `json:"integrations"`
	}{FamilyID: familyID, Integrations: listed})
}

// mcpFlagNames is the complete grammar. The only secret-related flags name
// file paths; no flag accepts a credential or secret value.
func mcpFlagNames(operation, phase string) []string {
	var flags []string
	if operation == "issue" {
		flags = []string{"--command-id", "--family-id", "--account-id", "--name", "--scope", "--server-instance-id", "--recovery-epoch",
			"--validity-seconds", "--policy-id", "--evidence-reference", "--reason-code", "--expected-revision"}
		if phase == "prepare" {
			return append(flags, "--secret-output")
		}
		flags = append(flags, "--grant-id", "--verifier", "--secret-file")
	} else {
		flags = []string{"--command-id", "--grant-id", "--family-id", "--account-id", "--server-instance-id", "--recovery-epoch",
			"--policy-id", "--evidence-reference", "--reason-code", "--expected-revision"}
	}
	return withConfirmationFlags(flags, phase)
}

func mcpProposal(operation string, values map[string]string) (operator.IntegrationProposal, error) {
	revision, err := parsePositiveRevision(values["--expected-revision"])
	if err != nil {
		return operator.IntegrationProposal{}, err
	}
	epoch, err := parsePositiveCanonical(values["--recovery-epoch"])
	if err != nil || epoch > protocol.MaxSafeInteger {
		return operator.IntegrationProposal{}, errors.New("--recovery-epoch must be a positive canonical integer")
	}
	proposal := operator.IntegrationProposal{CommandID: values["--command-id"], GrantID: values["--grant-id"], FamilyID: values["--family-id"],
		AccountID: values["--account-id"], ServerInstanceID: values["--server-instance-id"], RecoveryEpoch: epoch,
		PolicyID: values["--policy-id"], EvidenceReference: values["--evidence-reference"], ReasonCode: values["--reason-code"],
		ExpectedRevision: revision}
	if operation == "revoke" {
		proposal.Operation = "revoke"
		return proposal, nil
	}
	validity, err := parsePositiveCanonical(values["--validity-seconds"])
	if err != nil || validity != authority.IntegrationValiditySeconds {
		return operator.IntegrationProposal{}, fmt.Errorf("--validity-seconds must be exactly %d (90 days from the committed issuance event)", authority.IntegrationValiditySeconds)
	}
	if !authority.ValidIntegrationName(values["--name"]) {
		return operator.IntegrationProposal{}, errors.New("--name must be a 1-64 byte client label of ASCII letters, digits, space, '.', '_' or '-' without leading or trailing space; it is kept forever in the authority log, so never use a person's or baby's name")
	}
	proposal.Operation, proposal.Name, proposal.Scope, proposal.Verifier, proposal.ValiditySeconds = "grant", values["--name"], values["--scope"], values["--verifier"], validity
	return proposal, nil
}

func parsePositiveCanonical(value string) (uint64, error) {
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || parsed == 0 || strconv.FormatUint(parsed, 10) != value {
		return 0, errors.New("not a positive canonical integer")
	}
	return parsed, nil
}

func newGrantID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", errors.New("integration grant ID generation failed")
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

// mcpMessages explains each stable refusal code an operator can meet in this
// command family. The code stays first so scripts can still match it.
var mcpMessages = map[string]string{
	"operator_policy_required":    "MCP credential commands are disabled, or --policy-id does not match the policy mounted at BT_OPERATOR_INTEGRATION_POLICY_ID_FILE",
	"invalid_request":             "the proposal is malformed; check every UUID, the client label, --scope family:read, --validity-seconds 7776000, the reason code, the evidence reference, and positive --recovery-epoch/--expected-revision",
	"invalid_confirmation":        "the confirmation is invalid or expired, or the flags differ from the prepared proposal; nothing was committed, so prepare again (an unused issue secret file stays inactive and should be destroyed)",
	"command_id_reused":           "this --command-id was already committed with a different proposal; nothing was changed",
	"stale_security_revision":     "--expected-revision is no longer the authority head; prepare again with the current revision",
	"epoch_changed":               "--recovery-epoch is not the server's current recovery epoch",
	"invalid_command":             "the authority rejected the proposal: the server instance does not match, the grant ID is already used, or a field is invalid; prepare a new proposal",
	"family_unavailable":          "the family does not exist or is not active",
	"owner_required":              "--account-id is not an active owner of the family; credentials are issued only to the family owner",
	"integration_invalid":         "no grant matches this --grant-id with the given family, account, server instance and recovery epoch",
	"integration_revoked":         "the grant is already revoked; revocation is final",
	"integration_verifier_reused": "this secret's verifier is already registered; destroy the secret file and run mcp issue prepare again",
	"service_unavailable":         "the security authority is unavailable or quarantined; retry the exact same command after it recovers",
	"security_busy":               "the security authority is busy; retry the exact same command",
}

func mcpError(err error) error {
	if err == nil {
		return nil
	}
	if message, ok := mcpMessages[err.Error()]; ok {
		return fmt.Errorf("%s: %s", err.Error(), message)
	}
	return err
}
