package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/operator"
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
	result authority.Result
}

func (*cliAuthority) PreflightRecovery(context.Context, operator.RecoveryCommand) error {
	return nil
}
func (*cliAuthority) PreflightEntitlement(context.Context, operator.EntitlementCommand) error {
	return nil
}
func (fake *cliAuthority) ReplayRecovery(_ context.Context, command operator.RecoveryCommand) (authority.Result, bool, error) {
	return fake.replay(command.Receipt)
}
func (fake *cliAuthority) ReplayEntitlement(_ context.Context, command operator.EntitlementCommand) (authority.Result, bool, error) {
	return fake.replay(command.Receipt)
}
func (fake *cliAuthority) replay(key operator.ReceiptKey) (authority.Result, bool, error) {
	receipt, exists := fake.receipts[key.CommandID]
	if !exists {
		return authority.Result{}, false, nil
	}
	if receipt.key != key {
		return authority.Result{}, false, operator.ErrCommandIDReused
	}
	return receipt.result, true, nil
}
func (fake *cliAuthority) RecoverOwner(_ context.Context, command operator.RecoveryCommand) (authority.Result, error) {
	return fake.commit(command.Proposal.CommandID, command.Proposal.ExpectedRevision, command.Receipt, command.Audit, "")
}
func (fake *cliAuthority) GrantEntitlement(_ context.Context, command operator.EntitlementCommand) (authority.Result, error) {
	return fake.commit(command.Proposal.CommandID, command.Proposal.ExpectedRevision, command.Receipt, command.Audit, command.Proposal.Source)
}
func (fake *cliAuthority) SuspendEntitlement(_ context.Context, command operator.EntitlementCommand) (authority.Result, error) {
	return fake.commit(command.Proposal.CommandID, command.Proposal.ExpectedRevision, command.Receipt, command.Audit, command.Proposal.Source)
}
func (fake *cliAuthority) commit(commandID string, revision uint64, key operator.ReceiptKey, audit operator.Audit, source string) (authority.Result, error) {
	result := authority.Result{CommandID: commandID, Outcome: "committed", Code: "ok", Revision: revision + 1}
	fake.receipts[commandID] = cliReceipt{key: key, result: result}
	fake.audits = append(fake.audits, audit)
	fake.sources = append(fake.sources, source)
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
			service, fake := newOperatorCLIService(t, fixture)
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
			var result authority.Result
			if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.Revision != 43 || len(fake.audits) != 1 {
				t.Fatalf("commit output=%q audits=%d err=%v", output.String(), len(fake.audits), err)
			}
			if example.Name == "entitlement-suspend" && fake.sources[0] != "hosted_beta" {
				t.Fatalf("suspension source=%q", fake.sources[0])
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
				service, fake := newOperatorCLIService(t, fixture)
				arguments := append([]string(nil), example.CommitArgs...)
				arguments = append(arguments[:index], arguments[index+2:]...)
				if err := RunWithServices(t.Context(), arguments, service, &bytes.Buffer{}); err == nil || len(fake.audits) != 0 {
					t.Fatalf("missing %s was accepted; audits=%d", argument, len(fake.audits))
				}
			})
		}
	}

	t.Run("grant digest cannot suspend", func(t *testing.T) {
		service, fake := newOperatorCLIService(t, fixture)
		arguments := append([]string(nil), fixture.Examples[1].CommitArgs...)
		arguments[1] = "suspend"
		if err := RunWithServices(t.Context(), arguments, service, &bytes.Buffer{}); err == nil || len(fake.audits) != 0 {
			t.Fatalf("cross-operation digest was accepted; audits=%d", len(fake.audits))
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
			service, fake := newOperatorCLIService(t, fixture)
			arguments := replaceOperatorFlag(t, fixture.Examples[0].CommitArgs, mutation.flag, mutation.value)
			if err := RunWithServices(t.Context(), arguments, service, &bytes.Buffer{}); err == nil || len(fake.audits) != 0 {
				t.Fatalf("changed %s was accepted; audits=%d", mutation.flag, len(fake.audits))
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
	fake := &cliAuthority{receipts: make(map[string]cliReceipt)}
	service, err := operator.New(operator.Options{Authority: fake, OperatorID: fixture.OperatorID,
		OwnershipPolicyID: "owner-policy-test-v1", EntitlementPolicyID: "entitlement-policy-test-v1",
		ConfirmationKey: key, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return service, fake
}

var _ operator.Authority = (*cliAuthority)(nil)
