package security

import (
	"context"
	"encoding/hex"
	"sync"
	"testing"
	"time"
)

const (
	preparedAccount   = "10000000-0000-4000-8000-000000000001"
	preparedDevice    = "40000000-0000-4000-8000-000000000001"
	preparedSession   = "60000000-0000-4000-8000-000000000001"
	preparedInstance  = "50000000-0000-4000-8000-000000000001"
	preparedIdentity  = "hmac-sha256:identity.v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	preparedDeviceKey = "hmac-sha256:device.v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	preparedMaterial  = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func TestExecutePreparedIssuesIdentitySessionAndReplaysExactReceipt(t *testing.T) {
	h := newAuthority(t)
	input := preparedIdentitySessionInput("30000000-0000-4000-8000-000000000001")
	prepare := func(_ context.Context, snapshot AuthoritySnapshot) (PreparedCommand, error) {
		actor, err := snapshot.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1)
		if err != nil {
			return PreparedCommand{}, err
		}
		return snapshot.PrepareIdentitySessionIssued(actor, input)
	}
	first, err := h.Service.ExecutePrepared(t.Context(), prepare)
	if err != nil || first.Revision != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	appends, swaps := h.AppendCount(), h.heads.swaps
	second, err := h.Service.ExecutePrepared(t.Context(), prepare)
	if err != nil || second != first || h.AppendCount() != appends || h.heads.swaps != swaps {
		t.Fatalf("second=%+v err=%v appends=%d swaps=%d", second, err, h.AppendCount(), h.heads.swaps)
	}
	state, err := h.Service.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if account, ok := state.Account(preparedAccount); !ok || account.Status != "active" {
		t.Fatalf("account=%+v ok=%v", account, ok)
	}
	if session, ok := state.Session(preparedSession); !ok || session.Status != "active" {
		t.Fatalf("session=%+v ok=%v", session, ok)
	}
}

func TestExecutePreparedCASLossCallsCallbackOnce(t *testing.T) {
	h := newAuthority(t)
	h.heads.rejectOnce = true
	calls := 0
	_, err := h.Service.ExecutePrepared(t.Context(), func(_ context.Context, snapshot AuthoritySnapshot) (PreparedCommand, error) {
		calls++
		actor, resolveErr := snapshot.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1)
		if resolveErr != nil {
			return PreparedCommand{}, resolveErr
		}
		return snapshot.PrepareIdentitySessionIssued(actor, preparedIdentitySessionInput("30000000-0000-4000-8000-000000000002"))
	})
	if Code(err) != "security_busy" || calls != 1 || h.heads.swaps != 1 || h.AppendCount() != 1 {
		t.Fatalf("code=%q calls=%d swaps=%d appends=%d err=%v", Code(err), calls, h.heads.swaps, h.AppendCount(), err)
	}
}

func TestPreparedCommandRejectsZeroAndForeignSnapshotCapabilities(t *testing.T) {
	h := newAuthority(t)
	if _, err := h.Service.ExecutePrepared(t.Context(), func(context.Context, AuthoritySnapshot) (PreparedCommand, error) {
		return PreparedCommand{}, nil
	}); Code(err) != "invalid_command" {
		t.Fatalf("zero code=%q err=%v", Code(err), err)
	}

	var foreign PreparedCommand
	if err := h.Service.WithAuthoritySnapshot(t.Context(), func(snapshot AuthoritySnapshot) error {
		actor, err := snapshot.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1)
		if err != nil {
			return err
		}
		foreign, err = snapshot.PrepareIdentitySessionIssued(actor, preparedIdentitySessionInput("30000000-0000-4000-8000-000000000003"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.ExecutePrepared(t.Context(), func(context.Context, AuthoritySnapshot) (PreparedCommand, error) {
		return foreign, nil
	}); Code(err) != "invalid_command" {
		t.Fatalf("foreign code=%q err=%v", Code(err), err)
	}
}

func TestPreparedCommandHashRequiresSameSnapshot(t *testing.T) {
	h := newAuthority(t)
	var prepared PreparedCommand
	if err := h.Service.WithAuthoritySnapshot(t.Context(), func(snapshot AuthoritySnapshot) error {
		actor, err := snapshot.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1)
		if err != nil {
			return err
		}
		prepared, err = snapshot.PrepareIdentitySessionIssued(actor, preparedIdentitySessionInput("30000000-0000-4000-8000-000000000004"))
		if err != nil {
			return err
		}
		_, err = snapshot.CommandHash(prepared)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Service.WithAuthoritySnapshot(t.Context(), func(snapshot AuthoritySnapshot) error {
		if _, err := snapshot.CommandHash(prepared); Code(err) != "invalid_command" {
			t.Fatalf("foreign hash code=%q err=%v", Code(err), err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAuthoritySnapshotRejectsExportedFieldSubstitution(t *testing.T) {
	h := newAuthority(t)
	if err := h.Service.WithAuthoritySnapshot(t.Context(), func(snapshot AuthoritySnapshot) error {
		actor, err := snapshot.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1)
		if err != nil {
			return err
		}
		input := preparedIdentitySessionInput("30000000-0000-4000-8000-000000000005")
		prepared, err := snapshot.PrepareIdentitySessionIssued(actor, input)
		if err != nil {
			return err
		}
		mutations := map[string]func(*AuthoritySnapshot){
			"view replacement with same head": func(changed *AuthoritySnapshot) { changed.View = AuthorityView{} },
			"private token replacement":       func(changed *AuthoritySnapshot) { token := *changed.token; changed.token = &token },
			"exported head":                   func(changed *AuthoritySnapshot) { changed.Head.Revision++ },
			"recovery epoch":                  func(changed *AuthoritySnapshot) { changed.RecoveryEpoch++ },
		}
		for name, mutate := range mutations {
			t.Run(name, func(t *testing.T) {
				changed := snapshot
				mutate(&changed)
				if _, err := changed.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1); Code(err) != "invalid_command" {
					t.Fatalf("resolve code=%q err=%v", Code(err), err)
				}
				if _, err := changed.PrepareIdentitySessionIssued(actor, input); Code(err) != "invalid_command" {
					t.Fatalf("prepare code=%q err=%v", Code(err), err)
				}
				if _, err := changed.CommandHash(prepared); Code(err) != "invalid_command" {
					t.Fatalf("hash code=%q err=%v", Code(err), err)
				}
				if changed.validPrepared(prepared) {
					t.Fatal("substituted snapshot accepted prepared command")
				}
			})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAuthoritySnapshotRejectsLiveSameHeadViewSubstitution(t *testing.T) {
	h := newAuthority(t)
	if err := h.Service.WithAuthoritySnapshot(t.Context(), func(first AuthoritySnapshot) error {
		return h.Service.WithAuthoritySnapshot(t.Context(), func(second AuthoritySnapshot) error {
			if first.Head != second.Head {
				t.Fatalf("snapshots have different heads: first=%+v second=%+v", first.Head, second.Head)
			}
			actor, err := first.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1)
			if err != nil {
				t.Fatal(err)
			}
			input := preparedIdentitySessionInput("30000000-0000-4000-8000-000000000006")
			prepared, err := first.PrepareIdentitySessionIssued(actor, input)
			if err != nil {
				t.Fatal(err)
			}
			changed := first
			changed.View = second.View
			if _, err := changed.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1); Code(err) != "invalid_command" {
				t.Fatalf("resolve code=%q err=%v", Code(err), err)
			}
			if _, err := changed.PrepareIdentitySessionIssued(actor, input); Code(err) != "invalid_command" {
				t.Fatalf("prepare code=%q err=%v", Code(err), err)
			}
			if _, err := changed.CommandHash(prepared); Code(err) != "invalid_command" {
				t.Fatalf("hash code=%q err=%v", Code(err), err)
			}
			if changed.validPrepared(prepared) {
				t.Fatal("live state from another same-head snapshot was accepted")
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestExecutePreparedCallbackContextRejectsNestedAndRetainedSecurityEntry(t *testing.T) {
	h := newAuthority(t)
	var retained context.Context
	nestedWriteDone := make(chan error, 1)
	result, err := h.Service.ExecutePrepared(t.Context(), func(ctx context.Context, snapshot AuthoritySnapshot) (PreparedCommand, error) {
		if h.Service.hasFenceMarker(ctx) {
			t.Fatal("prepared callback received the internal fence lease")
		}
		retained = ctx
		nestedReadDone := make(chan error, 1)
		go func() {
			_, nestedErr := h.Service.ReadState(ctx)
			nestedReadDone <- nestedErr
		}()
		select {
		case nestedErr := <-nestedReadDone:
			if Code(nestedErr) != "security_busy" {
				return PreparedCommand{}, errorCode("nested_read_not_denied")
			}
		case <-time.After(time.Second):
			return PreparedCommand{}, errorCode("nested_read_leaked")
		}

		go func() {
			_, nestedErr := h.Service.ExecutePrepared(ctx, func(context.Context, AuthoritySnapshot) (PreparedCommand, error) {
				return PreparedCommand{}, errorCode("invalid_command")
			})
			nestedWriteDone <- nestedErr
		}()
		select {
		case nestedErr := <-nestedWriteDone:
			if Code(nestedErr) != "security_busy" {
				return PreparedCommand{}, errorCode("nested_write_not_denied")
			}
		case <-time.After(50 * time.Millisecond):
			return PreparedCommand{}, errorCode("nested_write_blocked")
		}
		if h.AppendCount() != 0 {
			return PreparedCommand{}, errorCode("nested_mutation_overlapped")
		}
		actor, resolveErr := snapshot.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1)
		if resolveErr != nil {
			return PreparedCommand{}, resolveErr
		}
		return snapshot.PrepareIdentitySessionIssued(actor, preparedIdentitySessionInput("30000000-0000-4000-8000-000000000007"))
	})
	if Code(err) == "nested_write_blocked" {
		select {
		case <-nestedWriteDone:
		case <-time.After(time.Second):
			t.Fatal("blocked nested write goroutine leaked after outer callback returned")
		}
	}
	if err != nil || result.Revision != 1 {
		t.Fatalf("result=%+v code=%q err=%v", result, Code(err), err)
	}
	assertCallbackContextDenied(t, h, retained)
}

func TestPreparedConstructorsCommitAndReplayExactReceipts(t *testing.T) {
	h := newAuthority(t)
	executeExactPrepared(t, h, func(snapshot AuthoritySnapshot) (PreparedCommand, error) {
		actor, err := snapshot.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1)
		if err != nil {
			return PreparedCommand{}, err
		}
		return snapshot.PrepareIdentitySessionIssued(actor, preparedIdentitySessionInput("30000000-0000-4000-8000-000000000011"))
	})

	const rotatedSession = "60000000-0000-4000-8000-000000000002"
	executeExactPrepared(t, h, func(snapshot AuthoritySnapshot) (PreparedCommand, error) {
		actor, err := snapshot.ResolveSessionActor(ExistingSessionActorInput{AccountID: preparedAccount, DeviceID: preparedDevice, SessionID: preparedSession, RecoveryEpoch: 1, SecurityRevision: 1})
		if err != nil {
			return PreparedCommand{}, err
		}
		return snapshot.PrepareSessionRotation(actor, SessionRotationCommandInput{Meta: preparedMeta("30000000-0000-4000-8000-000000000012"), Payload: SessionPayload{
			Version: "v1", AccountID: preparedAccount, DeviceID: preparedDevice, SessionID: rotatedSession, ParentSessionID: preparedSession,
			LineageRootSessionID: preparedSession, RecoveryEpoch: 1, ExpiresAt: "2026-09-21T12:00:00Z", Status: "active", ProjectionMaterialDigest: preparedMaterial,
		}})
	})

	executeExactPrepared(t, h, func(snapshot AuthoritySnapshot) (PreparedCommand, error) {
		actor, err := snapshot.ResolveSessionActor(ExistingSessionActorInput{AccountID: preparedAccount, DeviceID: preparedDevice, SessionID: preparedSession, RecoveryEpoch: 1, SecurityRevision: 1})
		if err != nil {
			return PreparedCommand{}, err
		}
		return snapshot.PrepareRefreshReuseRevocation(actor, RefreshReuseCommandInput{Meta: preparedMeta("30000000-0000-4000-8000-000000000013"), Payload: SessionPayload{
			Version: "v1", AccountID: preparedAccount, DeviceID: preparedDevice, SessionID: preparedSession,
			LineageRootSessionID: preparedSession, RecoveryEpoch: 1, ExpiresAt: "2026-09-21T12:00:00Z", Status: "revoked",
		}})
	})

	executeExactPrepared(t, h, func(snapshot AuthoritySnapshot) (PreparedCommand, error) {
		actor, err := snapshot.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1)
		if err != nil {
			return PreparedCommand{}, err
		}
		return snapshot.PrepareIdentityUnavailable(actor, IdentityUnavailableCommandInput{Meta: preparedMeta("30000000-0000-4000-8000-000000000014"), Payload: IdentityAuthorityPayload{
			Version: "v1", AccountID: preparedAccount, IdentityKey: preparedIdentity,
		}})
	})

	const restoredSession = "60000000-0000-4000-8000-000000000003"
	restore := func(snapshot AuthoritySnapshot) (PreparedCommand, error) {
		actor, err := snapshot.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1)
		if err != nil {
			return PreparedCommand{}, err
		}
		input := preparedIdentitySessionInput("30000000-0000-4000-8000-000000000015")
		input.Payload.RegisterAccount = false
		input.Payload.RegisterDevice = false
		input.Payload.SessionID = restoredSession
		input.Payload.LineageRootSessionID = restoredSession
		return snapshot.PrepareIdentityRestored(actor, input)
	}
	executeExactPrepared(t, h, restore)
	appends := h.AppendCount()
	_, err := h.Service.ExecutePrepared(t.Context(), func(_ context.Context, snapshot AuthoritySnapshot) (PreparedCommand, error) {
		actor, resolveErr := snapshot.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1)
		if resolveErr != nil {
			return PreparedCommand{}, resolveErr
		}
		input := preparedIdentitySessionInput("30000000-0000-4000-8000-000000000015")
		input.Payload.RegisterAccount = false
		input.Payload.RegisterDevice = false
		input.Payload.SessionID = restoredSession
		input.Payload.LineageRootSessionID = restoredSession
		return snapshot.PrepareIdentitySessionIssued(actor, input)
	})
	if Code(err) != "invalid_command" || h.AppendCount() != appends {
		t.Fatalf("cross-constructor receipt code=%q appends=%d/%d err=%v", Code(err), h.AppendCount(), appends, err)
	}
	state, err := h.Service.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if account, ok := state.Account(preparedAccount); !ok || account.Status != "active" {
		t.Fatalf("restored account=%+v ok=%v", account, ok)
	}
	if old, _ := state.Session(preparedSession); old.Status != "revoked" {
		t.Fatalf("restore revived old session: %+v", old)
	}
	if restored, ok := state.Session(restoredSession); !ok || restored.Status != "active" {
		t.Fatalf("restored session=%+v ok=%v", restored, ok)
	}

	executeExactPrepared(t, h, func(snapshot AuthoritySnapshot) (PreparedCommand, error) {
		actor, err := snapshot.ResolveSessionActor(ExistingSessionActorInput{AccountID: preparedAccount, DeviceID: preparedDevice, SessionID: restoredSession, RecoveryEpoch: 1, SecurityRevision: 5})
		if err != nil {
			return PreparedCommand{}, err
		}
		return snapshot.PrepareDeviceRevocation(actor, DeviceRevocationCommandInput{Meta: preparedMeta("30000000-0000-4000-8000-000000000016"), Payload: DevicePayload{
			Version: "v1", AccountID: preparedAccount, DeviceID: preparedDevice, ServerInstanceID: preparedInstance,
			CanonicalOrigin: "https://family.example", DeviceCredentialDigest: preparedDeviceKey,
		}})
	})

	if h.Head().Revision != 6 {
		t.Fatalf("revision=%d", h.Head().Revision)
	}
	if err := h.ledger.WalkCommitted(t.Context(), h.Head(), func(candidate Candidate) error {
		var event Event
		if err := decodeEventStrict(candidate.EventJSON, &event); err != nil {
			return err
		}
		if event.Kind == "identity_restored" {
			t.Fatal("new preparation emitted legacy identity_restored event")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareRefreshReuseAcceptsExpiredStoredDeadline(t *testing.T) {
	h := newAuthority(t)
	executeExactPrepared(t, h, func(snapshot AuthoritySnapshot) (PreparedCommand, error) {
		actor, err := snapshot.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1)
		if err != nil {
			return PreparedCommand{}, err
		}
		return snapshot.PrepareIdentitySessionIssued(actor, preparedIdentitySessionInput("30000000-0000-4000-8000-000000000091"))
	})
	executeExactPrepared(t, h, func(snapshot AuthoritySnapshot) (PreparedCommand, error) {
		actor, err := snapshot.ResolveSessionActor(ExistingSessionActorInput{AccountID: preparedAccount, DeviceID: preparedDevice,
			SessionID: preparedSession, RecoveryEpoch: 1, SecurityRevision: 1})
		if err != nil {
			return PreparedCommand{}, err
		}
		return snapshot.PrepareSessionRotation(actor, SessionRotationCommandInput{Meta: preparedMeta("30000000-0000-4000-8000-000000000092"),
			Payload: SessionPayload{Version: "v1", AccountID: preparedAccount, DeviceID: preparedDevice,
				SessionID: "60000000-0000-4000-8000-000000000092", ParentSessionID: preparedSession,
				LineageRootSessionID: preparedSession, RecoveryEpoch: 1, ExpiresAt: "2026-09-21T12:00:00Z",
				Status: "active", ProjectionMaterialDigest: preparedMaterial}})
	})
	_, err := h.Service.ExecutePrepared(t.Context(), func(_ context.Context, snapshot AuthoritySnapshot) (PreparedCommand, error) {
		actor, resolveErr := snapshot.ResolveSessionActor(ExistingSessionActorInput{AccountID: preparedAccount, DeviceID: preparedDevice,
			SessionID: preparedSession, RecoveryEpoch: 1, SecurityRevision: 1})
		if resolveErr != nil {
			return PreparedCommand{}, resolveErr
		}
		return snapshot.PrepareRefreshReuseRevocation(actor, RefreshReuseCommandInput{
			Meta: CommandMeta{CommandID: "30000000-0000-4000-8000-000000000093", At: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)},
			Payload: SessionPayload{Version: "v1", AccountID: preparedAccount, DeviceID: preparedDevice, SessionID: preparedSession,
				LineageRootSessionID: preparedSession, RecoveryEpoch: 1, ExpiresAt: "2026-09-21T12:00:00Z", Status: "revoked"}})
	})
	if err != nil {
		t.Fatalf("expired stored deadline rejected reuse: code=%q err=%v", Code(err), err)
	}
}

func TestIdentityUnavailableNotificationDigestBindsCommandReceipt(t *testing.T) {
	h := newAuthority(t)
	executeExactPrepared(t, h, func(snapshot AuthoritySnapshot) (PreparedCommand, error) {
		actor, err := snapshot.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1)
		if err != nil {
			return PreparedCommand{}, err
		}
		return snapshot.PrepareIdentitySessionIssued(actor, preparedIdentitySessionInput("30000000-0000-4000-8000-000000000070"))
	})

	commandID := "30000000-0000-4000-8000-000000000071"
	prepare := func(notificationDigest string) PrepareCommand {
		return func(_ context.Context, snapshot AuthoritySnapshot) (PreparedCommand, error) {
			actor, err := snapshot.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1)
			if err != nil {
				return PreparedCommand{}, err
			}
			return snapshot.PrepareIdentityUnavailable(actor, IdentityUnavailableCommandInput{
				Meta: preparedMeta(commandID),
				Payload: IdentityAuthorityPayload{
					Version: "v1", AccountID: preparedAccount, IdentityKey: preparedIdentity,
					NotificationDigest: notificationDigest,
				},
			})
		}
	}
	firstDigest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := h.Service.ExecutePrepared(t.Context(), prepare(firstDigest)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.ExecutePrepared(t.Context(), prepare(firstDigest)); err != nil {
		t.Fatalf("exact notification replay failed: %v", err)
	}
	secondDigest := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := h.Service.ExecutePrepared(t.Context(), prepare(secondDigest)); Code(err) != "command_id_reused" {
		t.Fatalf("changed notification code=%q err=%v", Code(err), err)
	}
}

func TestIdentityUnavailableWithoutNotificationDigestRemainsValid(t *testing.T) {
	h := newAuthority(t)
	executeExactPrepared(t, h, func(snapshot AuthoritySnapshot) (PreparedCommand, error) {
		actor, err := snapshot.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1)
		if err != nil {
			return PreparedCommand{}, err
		}
		return snapshot.PrepareIdentitySessionIssued(actor, preparedIdentitySessionInput("30000000-0000-4000-8000-000000000072"))
	})
	_, err := h.Service.ExecutePrepared(t.Context(), func(_ context.Context, snapshot AuthoritySnapshot) (PreparedCommand, error) {
		actor, resolveErr := snapshot.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1)
		if resolveErr != nil {
			return PreparedCommand{}, resolveErr
		}
		prepared, prepareErr := snapshot.PrepareIdentityUnavailable(actor, IdentityUnavailableCommandInput{
			Meta:    preparedMeta("30000000-0000-4000-8000-000000000073"),
			Payload: IdentityAuthorityPayload{Version: "v1", AccountID: preparedAccount, IdentityKey: preparedIdentity},
		})
		if prepareErr != nil {
			return PreparedCommand{}, prepareErr
		}
		hash, hashErr := snapshot.CommandHash(prepared)
		if hashErr != nil {
			return PreparedCommand{}, hashErr
		}
		if got := hex.EncodeToString(hash[:]); got != "dad23bfe57d120ac8e63681223beafb99ce4a0e5646ca15de9f03ae06d5dbf93" {
			t.Fatalf("ordinary identity-unavailable hash=%s", got)
		}
		return prepared, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPreparedCapabilitiesRejectForeignActorsAndCrossConstructorKinds(t *testing.T) {
	h := newAuthority(t)
	var actor IdentityActor
	if err := h.Service.WithAuthoritySnapshot(t.Context(), func(snapshot AuthoritySnapshot) error {
		var err error
		actor, err = snapshot.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Service.WithAuthoritySnapshot(t.Context(), func(snapshot AuthoritySnapshot) error {
		if _, err := snapshot.PrepareIdentitySessionIssued(actor, preparedIdentitySessionInput("30000000-0000-4000-8000-000000000021")); Code(err) != "invalid_command" {
			t.Fatalf("foreign actor code=%q err=%v", Code(err), err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	_, err := h.Service.ExecutePrepared(t.Context(), func(_ context.Context, snapshot AuthoritySnapshot) (PreparedCommand, error) {
		local, resolveErr := snapshot.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1)
		if resolveErr != nil {
			return PreparedCommand{}, resolveErr
		}
		prepared, prepareErr := snapshot.PrepareIdentitySessionIssued(local, preparedIdentitySessionInput("30000000-0000-4000-8000-000000000022"))
		if prepareErr != nil {
			return PreparedCommand{}, prepareErr
		}
		prepared.kind = preparedSessionRotation
		prepared.provenance.kind = preparedSessionRotation
		return prepared, nil
	})
	if Code(err) != "invalid_command" || h.AppendCount() != 0 {
		t.Fatalf("cross-kind code=%q appends=%d err=%v", Code(err), h.AppendCount(), err)
	}
}

func TestWithAuthoritySnapshotHoldsReadFenceAcrossCallback(t *testing.T) {
	h := newAuthority(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		_ = h.Service.WithAuthoritySnapshot(context.Background(), func(AuthoritySnapshot) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	preparedEntered := make(chan struct{})
	go func() {
		_, _ = h.Service.ExecutePrepared(context.Background(), func(context.Context, AuthoritySnapshot) (PreparedCommand, error) {
			close(preparedEntered)
			return PreparedCommand{}, errorCode("invalid_command")
		})
	}()
	select {
	case <-preparedEntered:
		t.Fatal("exclusive callback entered while authority snapshot was live")
	default:
	}
	close(release)
	wait.Wait()
	<-preparedEntered
}

func executeExactPrepared(t *testing.T, h *authorityFixture, prepare func(AuthoritySnapshot) (PreparedCommand, error)) {
	t.Helper()
	callback := func(_ context.Context, snapshot AuthoritySnapshot) (PreparedCommand, error) { return prepare(snapshot) }
	first, err := h.Service.ExecutePrepared(t.Context(), callback)
	if err != nil {
		t.Fatal(err)
	}
	appends, swaps := h.AppendCount(), h.heads.swaps
	second, err := h.Service.ExecutePrepared(t.Context(), callback)
	if err != nil || second != first || h.AppendCount() != appends || h.heads.swaps != swaps {
		t.Fatalf("replay first=%+v second=%+v err=%v appends=%d/%d swaps=%d/%d", first, second, err, h.AppendCount(), appends, h.heads.swaps, swaps)
	}
}

func preparedMeta(commandID string) CommandMeta {
	return CommandMeta{CommandID: commandID, At: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)}
}

func assertCallbackContextDenied(t *testing.T, h *authorityFixture, retained context.Context) {
	t.Helper()
	operations := map[string]func(context.Context) error{
		"read state": func(ctx context.Context) error { _, err := h.Service.ReadState(ctx); return err },
		"check":      h.Service.Check,
		"descendant": func(ctx context.Context) error {
			_, err := h.Service.IsDescendant(ctx, Head{Generation: 1})
			return err
		},
		"authority snapshot": func(ctx context.Context) error {
			return h.Service.WithAuthoritySnapshot(ctx, func(AuthoritySnapshot) error { return nil })
		},
		"prepared write": func(ctx context.Context) error {
			_, err := h.Service.ExecutePrepared(ctx, func(context.Context, AuthoritySnapshot) (PreparedCommand, error) {
				return PreparedCommand{}, errorCode("invalid_command")
			})
			return err
		},
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			started := time.Now()
			err := operation(retained)
			if Code(err) != "security_busy" {
				t.Fatalf("code=%q err=%v", Code(err), err)
			}
			if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
				t.Fatalf("denied callback context blocked for %v", elapsed)
			}
		})
	}
}

func preparedIdentitySessionInput(commandID string) IdentitySessionCommandInput {
	return IdentitySessionCommandInput{Meta: preparedMeta(commandID), Payload: IdentitySessionIssuedPayload{
		Version: "v1", AccountID: preparedAccount, IdentityKey: preparedIdentity, RegisterAccount: true,
		DeviceID: preparedDevice, ServerInstanceID: preparedInstance, CanonicalOrigin: "https://family.example",
		DeviceCredentialDigest: preparedDeviceKey, RegisterDevice: true, SessionID: preparedSession,
		LineageRootSessionID: preparedSession, RecoveryEpoch: 1, ExpiresAt: "2026-09-21T12:00:00Z", ProjectionMaterialDigest: preparedMaterial,
	}}
}
