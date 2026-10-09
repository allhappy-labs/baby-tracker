package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"babytracker/server/internal/operator"
	"babytracker/server/internal/security"
)

// OperatorCommands is injected by main composition after the lifecycle
// authority adapter and mounted operator credentials have been constructed.
// Keeping parsing behind this interface lets the dangerous grammar be tested
// without granting the executable ambient authority.
type OperatorCommands interface {
	PrepareRecovery(context.Context, operator.RecoveryProposal) (operator.Confirmation, error)
	RecoverOwner(context.Context, operator.RecoveryProposal, operator.Confirmation) (security.CommandResult, error)
	PrepareEntitlement(context.Context, operator.EntitlementProposal) (operator.Confirmation, error)
	GrantEntitlement(context.Context, operator.EntitlementProposal, operator.Confirmation) (security.CommandResult, error)
	SuspendEntitlement(context.Context, operator.EntitlementProposal, operator.Confirmation) (security.CommandResult, error)
}

// RunWithServices parses the exact operator command grammar and invokes the
// injected command service. It is intentionally separate from run until the
// lifecycle authority adapter is wired by composition.
func RunWithServices(ctx context.Context, arguments []string, commands OperatorCommands, output io.Writer) error {
	if ctx == nil || commands == nil || output == nil {
		return errors.New("operator command dependencies are required")
	}
	if len(arguments) >= 2 && arguments[0] == "ownership-recover" {
		return runOwnershipRecovery(ctx, arguments, commands, output)
	}
	if len(arguments) >= 3 && arguments[0] == "entitlement" {
		return runOperatorEntitlement(ctx, arguments, commands, output)
	}
	return errors.New("usage: ownership-recover prepare|commit or entitlement grant|suspend prepare|commit")
}

func runOwnershipRecovery(ctx context.Context, arguments []string, commands OperatorCommands, output io.Writer) error {
	phase := arguments[1]
	if phase != "prepare" && phase != "commit" {
		return errors.New("ownership-recover requires prepare or commit")
	}
	values, err := parseOperatorFlags(arguments[2:], recoveryFlagNames(phase))
	if err != nil {
		return err
	}
	revision, err := parsePositiveRevision(values["--expected-revision"])
	if err != nil {
		return err
	}
	proposal := operator.RecoveryProposal{
		CommandID: values["--command-id"], FamilyID: values["--family-id"],
		OldOwnerID: values["--old-owner-id"], NewOwnerID: values["--new-owner-id"],
		PolicyID: values["--policy-id"], EvidenceReference: values["--evidence-reference"],
		ReasonCode: values["--reason-code"], ExpectedRevision: revision,
	}
	if phase == "prepare" {
		confirmation, err := commands.PrepareRecovery(ctx, proposal)
		if err != nil {
			return err
		}
		return writePrepared(output, proposal, confirmation)
	}
	confirmation, err := parseConfirmation(values)
	if err != nil {
		return err
	}
	result, err := commands.RecoverOwner(ctx, proposal, confirmation)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}

func runOperatorEntitlement(ctx context.Context, arguments []string, commands OperatorCommands, output io.Writer) error {
	operation, phase := arguments[1], arguments[2]
	if (operation != "grant" && operation != "suspend") || (phase != "prepare" && phase != "commit") {
		return errors.New("entitlement requires grant|suspend prepare|commit")
	}
	values, err := parseOperatorFlags(arguments[3:], entitlementFlagNames(phase))
	if err != nil {
		return err
	}
	revision, err := parsePositiveRevision(values["--expected-revision"])
	if err != nil {
		return err
	}
	expiresAt, err := parseUTCSecond(values["--expires-at"])
	if err != nil {
		return errors.New("--expires-at must be a UTC-second RFC3339 timestamp")
	}
	proposal := operator.EntitlementProposal{
		CommandID: values["--command-id"], FamilyID: values["--family-id"], Operation: operation,
		Key: values["--key"], Source: values["--source"], ExpiresAt: expiresAt,
		PolicyID: values["--policy-id"], EvidenceReference: values["--evidence-reference"],
		ReasonCode: values["--reason-code"], ExpectedRevision: revision,
	}
	if phase == "prepare" {
		confirmation, err := commands.PrepareEntitlement(ctx, proposal)
		if err != nil {
			return err
		}
		return writePrepared(output, proposal, confirmation)
	}
	confirmation, err := parseConfirmation(values)
	if err != nil {
		return err
	}
	var result security.CommandResult
	if operation == "grant" {
		result, err = commands.GrantEntitlement(ctx, proposal, confirmation)
	} else {
		result, err = commands.SuspendEntitlement(ctx, proposal, confirmation)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}

func parseOperatorFlags(arguments []string, required []string) (map[string]string, error) {
	if len(arguments) != len(required)*2 {
		return nil, errors.New("operator command requires every flag exactly once")
	}
	allowed := make(map[string]struct{}, len(required))
	for _, name := range required {
		allowed[name] = struct{}{}
	}
	values := make(map[string]string, len(required))
	for index := 0; index < len(arguments); index += 2 {
		name, value := arguments[index], arguments[index+1]
		if _, ok := allowed[name]; !ok || value == "" || strings.HasPrefix(value, "--") {
			return nil, errors.New("operator command contains an unknown or empty flag")
		}
		if _, duplicate := values[name]; duplicate {
			return nil, errors.New("operator command contains a duplicate flag")
		}
		values[name] = value
	}
	if len(values) != len(required) {
		return nil, errors.New("operator command requires every flag exactly once")
	}
	return values, nil
}

func recoveryFlagNames(phase string) []string {
	flags := []string{"--command-id", "--family-id", "--old-owner-id", "--new-owner-id", "--policy-id", "--evidence-reference", "--expected-revision", "--reason-code"}
	return withConfirmationFlags(flags, phase)
}

func entitlementFlagNames(phase string) []string {
	flags := []string{"--command-id", "--family-id", "--key", "--source", "--expires-at", "--policy-id", "--evidence-reference", "--expected-revision", "--reason-code"}
	return withConfirmationFlags(flags, phase)
}

func withConfirmationFlags(flags []string, phase string) []string {
	if phase != "commit" {
		return flags
	}
	return append(flags, "--confirmation-digest", "--confirmation-expires-at")
}

func parsePositiveRevision(value string) (uint64, error) {
	revision, err := strconv.ParseUint(value, 10, 64)
	if err != nil || revision == 0 || strconv.FormatUint(revision, 10) != value {
		return 0, errors.New("--expected-revision must be a positive canonical integer")
	}
	return revision, nil
}

func parseConfirmation(values map[string]string) (operator.Confirmation, error) {
	expiresAt, err := parseUTCSecond(values["--confirmation-expires-at"])
	if err != nil {
		return operator.Confirmation{}, errors.New("--confirmation-expires-at must be a UTC-second RFC3339 timestamp")
	}
	return operator.Confirmation{Digest: values["--confirmation-digest"], ExpiresAt: expiresAt}, nil
}

func parseUTCSecond(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil || parsed.Nanosecond() != 0 || parsed.Location() != time.UTC || parsed.Format(time.RFC3339) != value {
		return time.Time{}, errors.New("timestamp is not canonical UTC seconds")
	}
	return parsed, nil
}

func writePrepared(output io.Writer, proposal any, confirmation operator.Confirmation) error {
	return json.NewEncoder(output).Encode(struct {
		Proposal     any                   `json:"proposal"`
		Confirmation operator.Confirmation `json:"confirmation"`
	}{Proposal: proposal, Confirmation: confirmation})
}

var _ OperatorCommands = (*operator.Service)(nil)
