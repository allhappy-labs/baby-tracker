package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"babytracker/server/internal/operator"
)

const (
	mcpOperator  = "90000000-0000-4000-8000-000000000001"
	mcpOwner     = "10000000-0000-4000-8000-000000000001"
	mcpFamily    = "20000000-0000-4000-8000-000000000001"
	mcpInstance  = "50000000-0000-4000-8000-000000000001"
	mcpPolicy    = "integration-policy-test-v1"
	mcpIssueCmd  = "30000000-0000-4000-8000-0000000000c1"
	mcpRevokeCmd = "30000000-0000-4000-8000-0000000000c2"
)

var mcpStart = time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)

// mcpClock is the single time source shared by the operator service and the
// authority of one stack.
type mcpClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *mcpClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *mcpClock) Set(value time.Time) {
	clock.mu.Lock()
	clock.now = value
	clock.mu.Unlock()
}

// mcpFaults wraps the PostgreSQL authority's operator surface to inject a
// failure before the integration command's transaction (nothing commits)
// or a lost acknowledgement after it (the command commits, the CLI sees an
// error).
type mcpFaults struct {
	*authority.Service
	mu                    sync.Mutex
	failBefore, loseReply bool
}

func (faults *mcpFaults) ExecuteOperatorIntegration(ctx context.Context, in authority.OperatorIntegration) (authority.Result, error) {
	faults.mu.Lock()
	before, lose := faults.failBefore, faults.loseReply
	faults.failBefore, faults.loseReply = false, false
	faults.mu.Unlock()
	if before {
		return authority.Result{}, authority.ErrUnavailable
	}
	result, err := faults.Service.ExecuteOperatorIntegration(ctx, in)
	if err == nil && lose {
		return authority.Result{}, authority.ErrUnavailable
	}
	return result, err
}

type mcpGate struct{}

func (mcpGate) Check(context.Context) error { return nil }

// mcpStack is the real operator service, security adapter and PostgreSQL
// authority bound to one server instance, seeded with an owner and family.
type mcpStack struct {
	t          *testing.T
	clock      *mcpClock
	db         *authoritytest.Fixture
	faults     *mcpFaults
	authority  *authority.Service
	service    *operator.Service
	dir        string
	transcript []string
}

func newMCPStack(t *testing.T) *mcpStack {
	t.Helper()
	stack := &mcpStack{t: t, clock: &mcpClock{now: mcpStart}, db: authoritytest.New(t), dir: privateTestDir(t)}
	stack.db.Now = mcpStart
	stack.reopen()
	stack.seed()
	return stack
}

// reopen models a process restart over the same database.
func (stack *mcpStack) reopen() {
	t := stack.t
	service, err := authority.New(authority.Options{Pool: stack.db.Pool, Now: stack.clock.Now, InstanceID: mcpInstance, RecoveryEpoch: 1,
		TokenKeys: stack.db.TokenKeys, Allowed: authoritytest.Allowed, EntitlementExempt: authority.LegacyEntitlementExempt})
	if err != nil {
		t.Fatal(err)
	}
	stack.faults = &mcpFaults{Service: service}
	adapter, err := operator.NewSecurityAuthority(stack.faults, mcpGate{})
	if err != nil {
		t.Fatal(err)
	}
	var key [32]byte
	for index := range key {
		key[index] = 0x5a
	}
	operatorService, err := operator.New(operator.Options{IntegrationAuthority: adapter, OperatorID: mcpOperator, IntegrationPolicyID: mcpPolicy,
		ConfirmationKey: key, Now: stack.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	stack.authority, stack.service = service, operatorService
}

// seed writes the owner account, device and session as rows and creates
// the family through the authority, so the family has a real audit chain
// (revision 1).
func (stack *mcpStack) seed() {
	t := stack.t
	stack.db.Exec(t, `INSERT INTO accounts(id,display_name,status,created_at,updated_at) VALUES ($1,'Owner','active',$2,$2)`, mcpOwner, mcpStart)
	device := stack.db.Device(t, mcpOwner)
	session, _, _ := stack.db.Session(t, mcpOwner, device, "account", "", "")
	principal := access.Principal{AccountID: mcpOwner, DeviceID: device, SessionID: session, RecoveryEpoch: 1, SecurityRevision: 1, Scope: "account", SessionScope: "account"}
	if _, err := stack.authority.CreateFamily(t.Context(), authority.CreateFamily{CommandID: "30000000-0000-4000-8000-000000000082",
		OwnerAccountID: mcpOwner, FamilyID: mcpFamily, DisplayName: "Family", Principal: principal}); err != nil {
		t.Fatal(err)
	}
}

// revision is the family's audit seq, the value operators pass as
// --expected-revision (`baby-tracker-admin authority family-revision`).
func (stack *mcpStack) revision() uint64 {
	stack.t.Helper()
	revision, err := stack.authority.FamilyRevision(stack.t.Context(), mcpFamily)
	if err != nil {
		stack.t.Fatal(err)
	}
	return revision
}

func (stack *mcpStack) grants() []authority.IntegrationView {
	stack.t.Helper()
	grants, err := stack.authority.ListFamilyIntegrations(stack.t.Context(), mcpFamily)
	if err != nil {
		stack.t.Fatal(err)
	}
	return grants
}

// holdAccountLock holds the owner's account row lock, as a concurrent
// authority command would; every integration command locks it first.
func (stack *mcpStack) holdAccountLock() (release func()) {
	stack.t.Helper()
	ctx := context.Background()
	tx, err := stack.db.DB.Owner.Begin(ctx)
	if err != nil {
		stack.t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.authority','1',true)`); err != nil {
		stack.t.Fatal(err)
	}
	var id string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM accounts WHERE id=$1 FOR UPDATE`, mcpOwner).Scan(&id); err != nil {
		stack.t.Fatal(err)
	}
	return func() { _ = tx.Rollback(ctx) }
}

// waitForLockWaiter waits until a transaction on the fixture database is
// blocked on a lock.
func (stack *mcpStack) waitForLockWaiter() {
	stack.t.Helper()
	database := stack.db.DB.Owner.Config().ConnConfig.Database
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := stack.db.DB.Admin.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname=$1 AND wait_event_type='Lock'`, database).Scan(&waiting); err != nil {
			stack.t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	stack.t.Fatal("the commit never queued behind the account lock")
}

// run executes one admin argv and records stdout and the error text, which
// main prints to stderr, for the disclosure check.
func (stack *mcpStack) run(arguments ...string) (string, error) {
	var output bytes.Buffer
	err := RunWithServices(stack.t.Context(), arguments, stack.service, &output)
	stack.transcript = append(stack.transcript, output.String())
	if err != nil {
		stack.transcript = append(stack.transcript, err.Error())
	}
	return output.String(), err
}

func (stack *mcpStack) assertNoDisclosure(credential string) {
	stack.t.Helper()
	assertNoCredentialDisclosure(stack.t, strings.Join(stack.transcript, "\n"), credential)
}

func assertNoCredentialDisclosure(t *testing.T, text, credential string) {
	t.Helper()
	encoded := credential[strings.LastIndex(credential, ".")+1:]
	secret, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	for _, form := range []string{credential, encoded, hex.EncodeToString(secret), base64.StdEncoding.EncodeToString(secret), base64.URLEncoding.EncodeToString(secret), string(secret)} {
		if strings.Contains(text, form) {
			t.Fatal("operator output or error disclosed the raw credential")
		}
	}
}

func privateTestDir(t *testing.T) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "secrets")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func issuePrepareArgs(commandID string, revision uint64, secretOutput string) []string {
	return []string{"mcp", "issue", "prepare", "--command-id", commandID, "--family-id", mcpFamily, "--account-id", mcpOwner,
		"--name", "claude-desktop-laptop", "--scope", "family:read", "--server-instance-id", mcpInstance, "--recovery-epoch", "1",
		"--validity-seconds", "7776000", "--policy-id", mcpPolicy, "--evidence-reference", "mcp_issue_0001",
		"--reason-code", "integration_issued", "--expected-revision", strconv.FormatUint(revision, 10), "--secret-output", secretOutput}
}

type preparedMCP struct {
	Proposal     operator.IntegrationProposal `json:"proposal"`
	Confirmation operator.Confirmation        `json:"confirmation"`
	SecretFile   string                       `json:"secret_file"`
	args         []string
}

// issueCommitArgs is the operator's commit argv: the same proposal flags, the
// grant ID and verifier printed by prepare, the secret file path and the
// confirmation.
func (prepared preparedMCP) issueCommitArgs() []string {
	arguments := []string{"mcp", "issue", "commit"}
	for index := 3; index < len(prepared.args); index += 2 {
		if prepared.args[index] != "--secret-output" {
			arguments = append(arguments, prepared.args[index], prepared.args[index+1])
		}
	}
	return append(arguments, "--grant-id", prepared.Proposal.GrantID, "--verifier", prepared.Proposal.Verifier, "--secret-file", prepared.SecretFile,
		"--confirmation-digest", prepared.Confirmation.Digest, "--confirmation-expires-at", prepared.Confirmation.ExpiresAt.Format(time.RFC3339))
}

func (stack *mcpStack) prepareIssue(commandID, name string) preparedMCP {
	stack.t.Helper()
	arguments := issuePrepareArgs(commandID, stack.revision(), filepath.Join(stack.dir, name))
	output, err := stack.run(arguments...)
	if err != nil {
		stack.t.Fatalf("issue prepare: %v", err)
	}
	var prepared preparedMCP
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&prepared); err != nil {
		stack.t.Fatalf("prepare output=%q err=%v", output, err)
	}
	prepared.args = arguments
	return prepared
}

func revokeArgs(phase, commandID, grantID, accountID string, revision uint64) []string {
	return []string{"mcp", "revoke", phase, "--command-id", commandID, "--grant-id", grantID, "--family-id", mcpFamily, "--account-id", accountID,
		"--server-instance-id", mcpInstance, "--recovery-epoch", "1", "--policy-id", mcpPolicy, "--evidence-reference", "mcp_revoke_0001",
		"--reason-code", "integration_revoked", "--expected-revision", strconv.FormatUint(revision, 10)}
}

func readCredential(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func decodeResult(t *testing.T, output string) authority.Result {
	t.Helper()
	var result authority.Result
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("result output=%q err=%v", output, err)
	}
	return result
}

func TestMCPIssueListRevokeLifecycleNeverDisclosesCredential(t *testing.T) {
	stack := newMCPStack(t)
	previous := syscall.Umask(0)
	prepared := stack.prepareIssue(mcpIssueCmd, "laptop.credential")
	syscall.Umask(previous)

	info, err := os.Lstat(prepared.SecretFile)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Fatalf("secret file mode=%v err=%v", info.Mode(), err)
	}
	credential := readCredential(t, prepared.SecretFile)
	grantID, verifier, ok := authority.IntegrationCredentialVerifier(credential)
	if !ok || len(credential) != 87 || !strings.HasPrefix(credential, "btmcp1."+prepared.Proposal.GrantID+".") ||
		grantID != prepared.Proposal.GrantID || verifier != prepared.Proposal.Verifier {
		t.Fatalf("secret file is not the exact credential for the proposal: grant=%q ok=%v", grantID, ok)
	}
	if prepared.Proposal.Operation != "grant" || prepared.Proposal.ValiditySeconds != 7776000 || prepared.Proposal.Scope != "family:read" ||
		!prepared.Confirmation.ExpiresAt.Equal(mcpStart.Add(5*time.Minute)) {
		t.Fatalf("prepared=%+v", prepared)
	}
	if grants := stack.grants(); len(grants) != 0 {
		t.Fatalf("prepare activated a credential: %+v", grants)
	}

	commitAt := mcpStart.Add(90*time.Second + 400*time.Millisecond)
	stack.clock.Set(commitAt)
	output, err := stack.run(prepared.issueCommitArgs()...)
	if err != nil {
		t.Fatal(err)
	}
	if result := decodeResult(t, output); result.CommandID != mcpIssueCmd || result.Revision != stack.revision() {
		t.Fatalf("commit result=%+v revision=%d", result, stack.revision())
	}
	grants := stack.grants()
	issued := commitAt.Truncate(time.Second)
	if len(grants) != 1 || grants[0].ID != grantID || grants[0].Verifier != verifier || grants[0].Status != "active" ||
		!grants[0].CreatedAt.Equal(issued) || !grants[0].ExpiresAt.Equal(issued.Add(7776000*time.Second)) {
		t.Fatalf("grants=%+v", grants)
	}

	output, err = stack.run("mcp", "list", "--family-id", mcpFamily)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, verifier) || strings.Contains(output, "verifier") {
		t.Fatalf("list disclosed the verifier: %s", output)
	}
	var listed struct {
		FamilyID     string                 `json:"family_id"`
		Integrations []mcpListedIntegration `json:"integrations"`
	}
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&listed); err != nil || listed.FamilyID != mcpFamily || len(listed.Integrations) != 1 {
		t.Fatalf("list=%q err=%v", output, err)
	}
	want := mcpListedIntegration{GrantID: grantID, AccountID: mcpOwner, FamilyID: mcpFamily, ServerInstanceID: mcpInstance, RecoveryEpoch: 1,
		Name: "claude-desktop-laptop", Scope: "family:read", Status: "active", CreatedAt: issued, ExpiresAt: issued.Add(7776000 * time.Second)}
	if got := listed.Integrations[0]; got.GrantID != want.GrantID || got.Name != want.Name || got.Status != want.Status ||
		!got.CreatedAt.Equal(want.CreatedAt) || !got.ExpiresAt.Equal(want.ExpiresAt) || got.AccountID != want.AccountID ||
		got.ServerInstanceID != want.ServerInstanceID || got.RecoveryEpoch != 1 || got.Scope != want.Scope {
		t.Fatalf("listed=%+v want=%+v", got, want)
	}

	if _, err := stack.run(revokeArgs("prepare", mcpRevokeCmd, grantID, "10000000-0000-4000-8000-000000000099", stack.revision())...); err == nil ||
		!strings.HasPrefix(err.Error(), "integration_invalid: ") {
		t.Fatalf("mismatched revoke err=%v", err)
	}
	output, err = stack.run(revokeArgs("prepare", mcpRevokeCmd, grantID, mcpOwner, stack.revision())...)
	if err != nil {
		t.Fatal(err)
	}
	var revokePrepared preparedMCP
	if err := json.Unmarshal([]byte(output), &revokePrepared); err != nil || revokePrepared.Proposal.Operation != "revoke" ||
		revokePrepared.Proposal.Verifier != "" || revokePrepared.SecretFile != "" {
		t.Fatalf("revoke prepare=%q err=%v", output, err)
	}
	revokeCommit := append(revokeArgs("commit", mcpRevokeCmd, grantID, mcpOwner, revokePrepared.Proposal.ExpectedRevision),
		"--confirmation-digest", revokePrepared.Confirmation.Digest, "--confirmation-expires-at", revokePrepared.Confirmation.ExpiresAt.Format(time.RFC3339))
	output, err = stack.run(revokeCommit...)
	if err != nil {
		t.Fatal(err)
	}
	revoked := decodeResult(t, output)
	if grants := stack.grants(); len(grants) != 1 || grants[0].Status != "revoked" || !grants[0].CreatedAt.Equal(issued) {
		t.Fatalf("after revoke grants=%+v", grants)
	}
	stack.clock.Set(commitAt.Add(time.Hour))
	output, err = stack.run(revokeCommit...)
	if err != nil || decodeResult(t, output) != revoked {
		t.Fatalf("revoke retry=%q err=%v", output, err)
	}
	if _, err := stack.run(revokeArgs("prepare", "30000000-0000-4000-8000-0000000000c3", grantID, mcpOwner, stack.revision())...); err == nil ||
		!strings.HasPrefix(err.Error(), "integration_revoked: ") {
		t.Fatalf("second revocation err=%v", err)
	}
	if readCredential(t, prepared.SecretFile) != credential {
		t.Fatal("the secret file changed")
	}
	stack.assertNoDisclosure(credential)
}

func TestMCPIssueExactRetryAfterLostAcknowledgementCommitsOneGrant(t *testing.T) {
	stack := newMCPStack(t)
	prepared := stack.prepareIssue(mcpIssueCmd, "laptop.credential")
	credential := readCredential(t, prepared.SecretFile)
	stack.clock.Set(mcpStart.Add(time.Minute))
	acknowledgement, err := stack.run(prepared.issueCommitArgs()...)
	if err != nil {
		t.Fatal(err)
	}
	committedHead := stack.revision()
	original := stack.grants()

	for _, step := range []struct {
		name    string
		now     time.Time
		restart bool
	}{
		{"after confirmation expiry", mcpStart.Add(10 * time.Minute), false},
		{"after restart", mcpStart.Add(time.Hour), true},
		{"after grant expiry", mcpStart.Add(91 * 24 * time.Hour), true},
	} {
		t.Run(step.name, func(t *testing.T) {
			stack.clock.Set(step.now)
			if step.restart {
				stack.reopen()
			}
			output, err := stack.run(prepared.issueCommitArgs()...)
			if err != nil || output != acknowledgement {
				t.Fatalf("retry output=%q want=%q err=%v", output, acknowledgement, err)
			}
			if stack.revision() != committedHead {
				t.Fatalf("retry advanced authority: %d -> %d", committedHead, stack.revision())
			}
			if grants := stack.grants(); len(grants) != 1 || grants[0] != original[0] {
				t.Fatalf("grants=%+v original=%+v", grants, original)
			}
		})
	}
	changed := prepared.issueCommitArgs()
	for index := range changed {
		if changed[index] == "--name" {
			changed[index+1] = "other-laptop"
		}
	}
	if _, err := stack.run(changed...); err == nil || !strings.HasPrefix(err.Error(), "command_id_reused: ") {
		t.Fatalf("changed retry err=%v", err)
	}
	if readCredential(t, prepared.SecretFile) != credential || stack.revision() != committedHead || len(stack.grants()) != 1 {
		t.Fatal("a retry rewrote the secret or changed authority")
	}
	stack.assertNoDisclosure(credential)
}

func TestMCPIssueRecoversFromFailureBeforeCommit(t *testing.T) {
	stack := newMCPStack(t)
	prepared := stack.prepareIssue(mcpIssueCmd, "laptop.credential")
	credential := readCredential(t, prepared.SecretFile)
	before := stack.revision()
	stack.faults.failBefore = true
	if _, err := stack.run(prepared.issueCommitArgs()...); err == nil {
		t.Fatal("injected pre-commit failure was not reported")
	}
	if stack.revision() != before || len(stack.grants()) != 0 {
		t.Fatalf("pre-commit failure issued a grant: revision=%d grants=%+v", stack.revision(), stack.grants())
	}
	if readCredential(t, prepared.SecretFile) != credential {
		t.Fatal("failure changed the inactive secret file")
	}
	stack.clock.Set(mcpStart.Add(2 * time.Minute))
	output, err := stack.run(prepared.issueCommitArgs()...)
	if err != nil {
		t.Fatal(err)
	}
	grants := stack.grants()
	_, verifier, _ := authority.IntegrationCredentialVerifier(credential)
	if decodeResult(t, output).Revision != before+1 || len(grants) != 1 || grants[0].Verifier != verifier ||
		!grants[0].CreatedAt.Equal(mcpStart.Add(2*time.Minute)) {
		t.Fatalf("retry result=%q grants=%+v", output, grants)
	}
	stack.assertNoDisclosure(credential)
}

func TestMCPIssueRecoversFromFailureAfterCommit(t *testing.T) {
	stack := newMCPStack(t)
	prepared := stack.prepareIssue(mcpIssueCmd, "laptop.credential")
	credential := readCredential(t, prepared.SecretFile)
	before := stack.revision()
	stack.clock.Set(mcpStart.Add(time.Minute))
	stack.faults.loseReply = true
	if _, err := stack.run(prepared.issueCommitArgs()...); err == nil || !strings.HasPrefix(err.Error(), "service_unavailable: ") {
		t.Fatalf("lost acknowledgement err=%v", err)
	}
	committed := stack.revision()
	if committed != before+1 {
		t.Fatalf("grant was not durably committed: %d", committed)
	}
	stack.clock.Set(mcpStart.Add(30 * time.Minute))
	output, err := stack.run(prepared.issueCommitArgs()...)
	if err != nil {
		t.Fatal(err)
	}
	grants := stack.grants()
	// The exact retry returns the committed receipt: the original issuance
	// time, no second grant, no new audit row.
	if result := decodeResult(t, output); result.CommandID != mcpIssueCmd || result.Revision != committed || stack.revision() != committed ||
		len(grants) != 1 || !grants[0].CreatedAt.Equal(mcpStart.Add(time.Minute)) {
		t.Fatalf("recovery result=%q revision=%d grants=%+v", output, stack.revision(), grants)
	}
	stack.assertNoDisclosure(credential)
}

// A commit queued behind a concurrent authority command's row lock is
// judged at the time it acquires the lock (the commit timestamp is captured
// after the locks): still confirmed then issues at that time, expired then
// fails closed.
func TestMCPIssueQueuedBehindLockUsesAdmissionTimeAndExpiresClosed(t *testing.T) {
	commitQueued := func(t *testing.T, stack *mcpStack, prepared preparedMCP, admitted time.Time) error {
		t.Helper()
		release := stack.holdAccountLock()
		done := make(chan error, 1)
		go func() {
			var output bytes.Buffer
			done <- RunWithServices(context.Background(), prepared.issueCommitArgs(), stack.service, &output)
		}()
		stack.waitForLockWaiter()
		stack.clock.Set(admitted)
		release()
		return <-done
	}

	t.Run("still confirmed at admission", func(t *testing.T) {
		stack := newMCPStack(t)
		prepared := stack.prepareIssue(mcpIssueCmd, "laptop.credential")
		stack.clock.Set(mcpStart.Add(time.Minute))
		admitted := prepared.Confirmation.ExpiresAt.Add(-time.Nanosecond)
		if err := commitQueued(t, stack, prepared, admitted); err != nil {
			t.Fatal(err)
		}
		if grants := stack.grants(); len(grants) != 1 || !grants[0].CreatedAt.Equal(admitted.Truncate(time.Second)) {
			t.Fatalf("grants=%+v", grants)
		}
	})
	t.Run("expired at admission", func(t *testing.T) {
		stack := newMCPStack(t)
		prepared := stack.prepareIssue(mcpIssueCmd, "laptop.credential")
		credential := readCredential(t, prepared.SecretFile)
		before := stack.revision()
		stack.clock.Set(mcpStart.Add(time.Minute))
		err := commitQueued(t, stack, prepared, prepared.Confirmation.ExpiresAt)
		if err == nil || !strings.HasPrefix(err.Error(), "invalid_confirmation: ") {
			t.Fatalf("queued expired commit err=%v", err)
		}
		if stack.revision() != before || len(stack.grants()) != 0 {
			t.Fatalf("expired queued commit issued: revision=%d grants=%+v", stack.revision(), stack.grants())
		}
		if _, err := stack.run(prepared.issueCommitArgs()...); err == nil || !strings.HasPrefix(err.Error(), "invalid_confirmation: ") {
			t.Fatalf("retry after expiry err=%v", err)
		}
		if readCredential(t, prepared.SecretFile) != credential || stack.revision() != before {
			t.Fatal("expired commit changed the secret file or authority")
		}
		stack.assertNoDisclosure(credential)
	})
}

func TestMCPIssuePrepareRefusesUnsafeSecretOutput(t *testing.T) {
	stack := newMCPStack(t)
	existing := filepath.Join(stack.dir, "existing")
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkToExisting := filepath.Join(stack.dir, "link-existing")
	if err := os.Symlink(existing, linkToExisting); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(stack.dir, "dangling")
	if err := os.Symlink(filepath.Join(stack.dir, "dangling-target"), dangling); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(shared, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o750); err != nil {
		t.Fatal(err)
	}
	linkedDirectory := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(stack.dir, linkedDirectory); err != nil {
		t.Fatal(err)
	}
	before := stack.revision()
	for name, path := range map[string]string{
		"existing file":         existing,
		"symlink to file":       linkToExisting,
		"dangling symlink":      dangling,
		"group-readable parent": filepath.Join(shared, "credential"),
		"symlinked parent":      filepath.Join(linkedDirectory, "credential"),
		"relative path":         "credential",
		"unclean path":          stack.dir + "/../secrets/credential",
		"missing parent":        filepath.Join(stack.dir, "missing", "credential"),
		"directory as the file": stack.dir,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := stack.run(issuePrepareArgs(mcpIssueCmd, before, path)...); err == nil {
				t.Fatal("unsafe secret output was accepted")
			}
		})
	}
	if data, err := os.ReadFile(existing); err != nil || string(data) != "keep" {
		t.Fatalf("existing file was overwritten: %q err=%v", data, err)
	}
	// A regression must not leave a credential in the package directory.
	t.Cleanup(func() { _ = os.Remove("credential") })
	for _, path := range []string{filepath.Join(stack.dir, "dangling-target"), filepath.Join(shared, "credential"), filepath.Join(stack.dir, "credential"), "credential"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("refused prepare created %s", path)
		}
	}
	if stack.revision() != before {
		t.Fatal("refused prepare changed authority")
	}
}

func TestMCPIssueCommitRefusesUnprotectedOrForeignSecretFile(t *testing.T) {
	stack := newMCPStack(t)
	prepared := stack.prepareIssue(mcpIssueCmd, "laptop.credential")
	other := stack.prepareIssue("30000000-0000-4000-8000-0000000000c4", "other.credential")
	credential := readCredential(t, prepared.SecretFile)
	before := stack.revision()
	withSecretFile := func(path string) []string {
		arguments := prepared.issueCommitArgs()
		for index := range arguments {
			if arguments[index] == "--secret-file" {
				arguments[index+1] = path
			}
		}
		return arguments
	}
	attempt := func(name, path, wantPrefix string) {
		t.Helper()
		_, err := stack.run(withSecretFile(path)...)
		if err == nil || (wantPrefix != "" && !strings.HasPrefix(err.Error(), wantPrefix)) {
			t.Fatalf("%s: err=%v", name, err)
		}
		if stack.revision() != before {
			t.Fatalf("%s committed", name)
		}
	}
	for _, mode := range []os.FileMode{0o640, 0o644, 0o400, 0o700} {
		if err := os.Chmod(prepared.SecretFile, mode); err != nil {
			t.Fatal(err)
		}
		attempt("mode "+mode.String(), prepared.SecretFile, "")
	}
	if err := os.Chmod(prepared.SecretFile, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(stack.dir, "link.credential")
	if err := os.Symlink(prepared.SecretFile, link); err != nil {
		t.Fatal(err)
	}
	attempt("symlink", link, "")
	shared := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(shared, "copy.credential")
	if err := os.WriteFile(copied, []byte(credential), 0o600); err != nil {
		t.Fatal(err)
	}
	attempt("world-readable directory", copied, "")
	attempt("other proposal's secret", other.SecretFile, "secret_file_mismatch: ")
	trailing := filepath.Join(stack.dir, "trailing.credential")
	if err := os.WriteFile(trailing, []byte(credential+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	attempt("altered content", trailing, "secret_file_invalid: ")
	attempt("credential value instead of a path", credential, "")

	if _, err := stack.run(prepared.issueCommitArgs()...); err != nil {
		t.Fatalf("protected exact file was refused: %v", err)
	}
	stack.assertNoDisclosure(credential)
	stack.assertNoDisclosure(readCredential(t, other.SecretFile))
}

func TestMCPCommandsDenyMissingOrMismatchedPolicyBeforeWritingSecrets(t *testing.T) {
	directory := privateTestDir(t)
	output := filepath.Join(directory, "credential")
	for _, arguments := range [][]string{issuePrepareArgs(mcpIssueCmd, 2, output), {"mcp", "list", "--family-id", mcpFamily}} {
		err := run(t.Context(), arguments, func(string) string { return "" }, &bytes.Buffer{})
		if err == nil || !strings.HasPrefix(err.Error(), "operator_policy_required: ") {
			t.Fatalf("%v err=%v", arguments[:2], err)
		}
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatal("denied prepare wrote a secret file")
	}

	key := writeAdminSecret(t, directory, "confirmation-key", strings.Repeat("11", 32))
	ownership := writeAdminSecret(t, directory, "ownership-policy", "owner-policy-test-v1")
	values := map[string]string{"BT_OPERATOR_ID": mcpOperator, "BT_OPERATOR_OWNERSHIP_POLICY_ID_FILE": ownership, "BT_OPERATOR_CONFIRMATION_KEY_FILE": key}
	if err := run(t.Context(), issuePrepareArgs(mcpIssueCmd, 2, output), func(name string) string { return values[name] }, &bytes.Buffer{}); err == nil ||
		!strings.HasPrefix(err.Error(), "operator_policy_required: ") {
		t.Fatalf("other family's policy enabled MCP: %v", err)
	}
	values["BT_OPERATOR_INTEGRATION_POLICY_ID"] = mcpPolicy
	if err := run(t.Context(), issuePrepareArgs(mcpIssueCmd, 2, output), func(name string) string { return values[name] }, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("direct policy value err=%v", err)
	}
	delete(values, "BT_OPERATOR_INTEGRATION_POLICY_ID")
	values["BT_OPERATOR_INTEGRATION_POLICY_ID_FILE"] = writeAdminSecret(t, directory, "integration-policy", mcpPolicy)
	values["BT_MODE"] = "hosted"
	if _, _, err := loadOperatorCommands(t.Context(), func(name string) string { return values[name] }, issuePrepareArgs(mcpIssueCmd, 2, output)); err == nil ||
		err.Error() != "hosted_adapter_unavailable" {
		t.Fatalf("hosted MCP composition err=%v", err)
	}

	stack := newMCPStack(t)
	mismatched := issuePrepareArgs(mcpIssueCmd, stack.revision(), filepath.Join(stack.dir, "credential"))
	for index := range mismatched {
		if mismatched[index] == "--policy-id" {
			mismatched[index+1] = "owner-policy-test-v1"
		}
	}
	if _, err := stack.run(mismatched...); err == nil || !strings.HasPrefix(err.Error(), "operator_policy_required: ") {
		t.Fatalf("mismatched policy err=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(stack.dir, "credential")); !os.IsNotExist(err) {
		t.Fatal("mismatched policy wrote a secret file")
	}
}

func TestMCPGrammarAcceptsOnlySecretFilePaths(t *testing.T) {
	for _, operation := range []string{"issue", "revoke"} {
		for _, phase := range []string{"prepare", "commit"} {
			for _, name := range mcpFlagNames(operation, phase) {
				lowered := strings.ToLower(name)
				if (strings.Contains(lowered, "secret") || strings.Contains(lowered, "credential") || strings.Contains(lowered, "token")) &&
					name != "--secret-output" && name != "--secret-file" {
					t.Fatalf("%s %s accepts a secret-valued flag %s", operation, phase, name)
				}
			}
		}
	}
	stack := newMCPStack(t)
	base := issuePrepareArgs(mcpIssueCmd, stack.revision(), filepath.Join(stack.dir, "credential"))
	for name, arguments := range map[string][]string{
		"secret value flag": append(append([]string(nil), base...), "--secret", "btmcp1.x"),
		"missing flag":      base[:len(base)-2],
		"duplicate flag":    append(append([]string(nil), base[:len(base)-2]...), "--name", "again"),
		"usage":             {"mcp", "issue"},
		"list extra":        {"mcp", "list", "--family-id", mcpFamily, "--verbose", "1"},
	} {
		if _, err := stack.run(arguments...); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	// Help must warn before an operator chooses a valid but personal label.
	for _, arguments := range [][]string{{"mcp", "issue"}, {"mcp"}, {"mcp", "rotate", "prepare"}} {
		_, err := stack.run(arguments...)
		if err == nil || !strings.HasPrefix(err.Error(), "usage: ") || !strings.Contains(err.Error(), "--name is a client label kept forever in the authority log; never put a person's or baby's name in it") {
			t.Fatalf("%v usage does not state the label rule: %v", arguments, err)
		}
	}
	for flag, value := range map[string]string{"--validity-seconds": "86400", "--recovery-epoch": "01", "--name": "Jane Smith's baby"} {
		arguments := append([]string(nil), base...)
		for index := range arguments {
			if arguments[index] == flag {
				arguments[index+1] = value
			}
		}
		_, err := stack.run(arguments...)
		if err == nil {
			t.Fatalf("%s=%q accepted", flag, value)
		}
		if flag == "--name" && !strings.Contains(err.Error(), "never use a person's or baby's name") {
			t.Fatalf("label refusal does not explain the persistence rule: %v", err)
		}
	}
	if entries, err := os.ReadDir(stack.dir); err != nil || len(entries) != 0 {
		t.Fatalf("refused grammar wrote files: %v err=%v", entries, err)
	}
}

func TestREADMEDocumentsMCPGrammarAndLabelRule(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(readme)
	required := []string{"BT_OPERATOR_INTEGRATION_POLICY_ID_FILE", "mcp issue prepare", "mcp issue commit", "mcp revoke prepare",
		"mcp revoke commit", "mcp list --family-id", "never put a person's or baby's name", "btmcp1.<lowercase grant UUID>.<unpadded base64url",
		"no trailing newline", "7776000"}
	for _, operation := range []string{"issue", "revoke"} {
		for _, phase := range []string{"prepare", "commit"} {
			required = append(required, mcpFlagNames(operation, phase)...)
		}
	}
	for code := range mcpMessages {
		required = append(required, "`"+code+"`")
	}
	for _, item := range required {
		if !strings.Contains(text, item) {
			t.Errorf("README does not document %q", item)
		}
	}
}
