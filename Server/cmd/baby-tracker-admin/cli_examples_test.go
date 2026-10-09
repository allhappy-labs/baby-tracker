package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/operator"
	"babytracker/server/internal/security"
)

type operatorCLIFixtureFile struct {
	Version    string `json:"version"`
	OperatorID string `json:"operator_id"`
	Clock      string `json:"clock"`
	Examples   []struct {
		Name           string   `json:"name"`
		ExpectedDigest string   `json:"expected_digest"`
		PrepareArgs    []string `json:"prepare_args"`
		CommitArgs     []string `json:"commit_args"`
	} `json:"examples"`
}

type cliAuthority struct {
	receipts map[string]cliReceipt
	audits   []operator.Audit
	sources  []string
}

type cliReceipt struct {
	key    operator.ReceiptKey
	result security.CommandResult
}

func (*cliAuthority) PreflightRecovery(context.Context, operator.RecoveryCommand) error {
	return nil
}
func (*cliAuthority) PreflightEntitlement(context.Context, operator.EntitlementCommand) error {
	return nil
}
func (authority *cliAuthority) ReplayRecovery(_ context.Context, command operator.RecoveryCommand) (security.CommandResult, bool, error) {
	return authority.replay(command.Receipt)
}
func (authority *cliAuthority) ReplayEntitlement(_ context.Context, command operator.EntitlementCommand) (security.CommandResult, bool, error) {
	return authority.replay(command.Receipt)
}
func (authority *cliAuthority) replay(key operator.ReceiptKey) (security.CommandResult, bool, error) {
	receipt, exists := authority.receipts[key.CommandID]
	if !exists {
		return security.CommandResult{}, false, nil
	}
	if receipt.key != key {
		return security.CommandResult{}, false, operator.ErrCommandIDReused
	}
	return receipt.result, true, nil
}
func (authority *cliAuthority) RecoverOwner(_ context.Context, command operator.RecoveryCommand) (security.CommandResult, error) {
	return authority.commit(command.Proposal.CommandID, command.Proposal.ExpectedRevision, command.Receipt, command.Audit, "")
}
func (authority *cliAuthority) GrantEntitlement(_ context.Context, command operator.EntitlementCommand) (security.CommandResult, error) {
	return authority.commit(command.Proposal.CommandID, command.Proposal.ExpectedRevision, command.Receipt, command.Audit, command.Proposal.Source)
}
func (authority *cliAuthority) SuspendEntitlement(_ context.Context, command operator.EntitlementCommand) (security.CommandResult, error) {
	return authority.commit(command.Proposal.CommandID, command.Proposal.ExpectedRevision, command.Receipt, command.Audit, command.Proposal.Source)
}
func (authority *cliAuthority) commit(commandID string, revision uint64, key operator.ReceiptKey, audit operator.Audit, source string) (security.CommandResult, error) {
	result := security.CommandResult{CommandID: commandID, Outcome: "committed", Code: "ok", Revision: revision + 1}
	authority.receipts[commandID] = cliReceipt{key: key, result: result}
	authority.audits = append(authority.audits, audit)
	authority.sources = append(authority.sources, source)
	return result, nil
}

func TestDocumentedOperatorCLI(t *testing.T) {
	fixture := loadOperatorCLIFixture(t)
	runbook, err := os.ReadFile("../../../docs/operations/ownership-recovery.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, example := range fixture.Examples {
		t.Run(example.Name, func(t *testing.T) {
			for _, arguments := range [][]string{example.PrepareArgs, example.CommitArgs} {
				if !strings.Contains(string(runbook), "go run ./cmd/baby-tracker-admin "+strings.Join(arguments, " ")) {
					t.Fatalf("fixture argv is not byte-equivalent to the runbook: %v", arguments)
				}
			}
			service, authority := newOperatorCLIService(t, fixture)
			var output bytes.Buffer
			if err := RunWithServices(t.Context(), example.PrepareArgs, service, &output); err != nil {
				t.Fatal(err)
			}
			var prepared struct {
				Proposal     json.RawMessage       `json:"proposal"`
				Confirmation operator.Confirmation `json:"confirmation"`
			}
			if err := json.Unmarshal(output.Bytes(), &prepared); err != nil || len(prepared.Proposal) == 0 || prepared.Confirmation.Digest != example.ExpectedDigest {
				t.Fatalf("prepare output=%q err=%v", output.String(), err)
			}
			output.Reset()
			if err := RunWithServices(t.Context(), example.CommitArgs, service, &output); err != nil {
				t.Fatal(err)
			}
			var result security.CommandResult
			if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.Revision != 43 || len(authority.audits) != 1 {
				t.Fatalf("commit output=%q audits=%d err=%v", output.String(), len(authority.audits), err)
			}
			if example.Name == "entitlement-suspend" && authority.sources[0] != "hosted_beta" {
				t.Fatalf("suspension source=%q", authority.sources[0])
			}
		})
	}
}

func TestDocumentedOperatorMissingOrChangedFields(t *testing.T) {
	fixture := loadOperatorCLIFixture(t)
	for _, example := range fixture.Examples {
		for index, argument := range example.CommitArgs {
			if len(argument) < 3 || argument[:2] != "--" {
				continue
			}
			t.Run(example.Name+argument, func(t *testing.T) {
				service, authority := newOperatorCLIService(t, fixture)
				arguments := append([]string(nil), example.CommitArgs...)
				arguments = append(arguments[:index], arguments[index+2:]...)
				if err := RunWithServices(t.Context(), arguments, service, &bytes.Buffer{}); err == nil || len(authority.audits) != 0 {
					t.Fatalf("missing %s was accepted; audits=%d", argument, len(authority.audits))
				}
			})
		}
	}

	t.Run("grant digest cannot suspend", func(t *testing.T) {
		service, authority := newOperatorCLIService(t, fixture)
		arguments := append([]string(nil), fixture.Examples[1].CommitArgs...)
		arguments[1] = "suspend"
		if err := RunWithServices(t.Context(), arguments, service, &bytes.Buffer{}); err == nil || len(authority.audits) != 0 {
			t.Fatalf("cross-operation digest was accepted; audits=%d", len(authority.audits))
		}
	})

	for name, mutation := range map[string]struct{ flag, value string }{
		"changed confirmation expiry": {"--confirmation-expires-at", "2026-09-20T12:04:59Z"},
		"changed policy":              {"--policy-id", "owner-policy-test-v2"},
		"changed evidence":            {"--evidence-reference", "case_test_0002"},
		"changed command ID":          {"--command-id", "30000000-0000-4000-8000-000000000099"},
		"changed revision":            {"--expected-revision", "43"},
	} {
		t.Run(name, func(t *testing.T) {
			service, authority := newOperatorCLIService(t, fixture)
			arguments := replaceOperatorFlag(t, fixture.Examples[0].CommitArgs, mutation.flag, mutation.value)
			if err := RunWithServices(t.Context(), arguments, service, &bytes.Buffer{}); err == nil || len(authority.audits) != 0 {
				t.Fatalf("changed %s was accepted; audits=%d", mutation.flag, len(authority.audits))
			}
		})
	}

	t.Run("unknown and duplicate flags", func(t *testing.T) {
		service, _ := newOperatorCLIService(t, fixture)
		base := fixture.Examples[0].PrepareArgs
		for _, arguments := range [][]string{
			append(append([]string(nil), base...), "--unknown", "value"),
			append(append([]string(nil), base...), "--family-id", "20000000-0000-4000-8000-000000000001"),
		} {
			if err := RunWithServices(t.Context(), arguments, service, &bytes.Buffer{}); err == nil {
				t.Fatalf("ambiguous arguments were accepted: %v", arguments)
			}
		}
	})
}

func replaceOperatorFlag(t *testing.T, arguments []string, name, value string) []string {
	t.Helper()
	replaced := append([]string(nil), arguments...)
	for index := range replaced {
		if replaced[index] == name && index+1 < len(replaced) {
			replaced[index+1] = value
			return replaced
		}
	}
	t.Fatalf("fixture does not contain %s", name)
	return nil
}

func loadOperatorCLIFixture(t *testing.T) operatorCLIFixtureFile {
	t.Helper()
	data, err := os.ReadFile("../../api/fixtures/operator-cli-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture operatorCLIFixtureFile
	if err := json.Unmarshal(data, &fixture); err != nil || fixture.Version != "v1" || len(fixture.Examples) != 3 {
		t.Fatalf("invalid fixture: %+v err=%v", fixture, err)
	}
	return fixture
}

func newOperatorCLIService(t *testing.T, fixture operatorCLIFixtureFile) (*operator.Service, *cliAuthority) {
	t.Helper()
	now, err := time.Parse(time.RFC3339, fixture.Clock)
	if err != nil {
		t.Fatal(err)
	}
	var key [32]byte
	for index := range key {
		key[index] = 0x42
	}
	authority := &cliAuthority{receipts: make(map[string]cliReceipt)}
	service, err := operator.New(operator.Options{Authority: authority, OperatorID: fixture.OperatorID,
		OwnershipPolicyID: "owner-policy-test-v1", EntitlementPolicyID: "entitlement-policy-test-v1",
		ConfirmationKey: key, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return service, authority
}

var _ operator.Authority = (*cliAuthority)(nil)
