package security

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCredentialCleanupClaimIsValidatedAndForwardedExactly(t *testing.T) {
	h := newAuthority(t)
	claim := fixtureCredentialCleanupClaim()
	var calls []string
	var received []CredentialCleanupClaim
	completed, err := h.Service.DeleteCredentialIfUnreferenced(t.Context(), CredentialCleanupAttempt{
		Claim: claim, Intent: CredentialDeletionCASIntent{Opaque: []byte("authenticated-intent")},
		ValidateClaim: func(_ context.Context, got CredentialCleanupClaim) error {
			calls = append(calls, "validate")
			received = append(received, got)
			return nil
		},
		Referenced: func(_ context.Context, got CredentialCleanupClaim) (bool, error) {
			calls = append(calls, "referenced")
			received = append(received, got)
			return false, nil
		},
		MarkDeleting: func(_ context.Context, got CredentialCleanupClaim, intent CredentialDeletionCASIntent) (CredentialDeletionCapability, error) {
			calls = append(calls, "mark")
			received = append(received, got)
			return CredentialDeletionCapability{Opaque: append([]byte(nil), intent.Opaque...)}, nil
		},
		Delete: func(_ context.Context, capability CredentialDeletionCapability) error {
			calls = append(calls, "delete")
			capability.Opaque[0] = 'X'
			return nil
		},
		ConfirmAbsent: func(_ context.Context, capability CredentialDeletionCapability) (bool, error) {
			calls = append(calls, "absent")
			if string(capability.Opaque) != "authenticated-intent" {
				t.Fatalf("delete mutated capability observed by absence check: %q", capability.Opaque)
			}
			return true, nil
		},
		Finish: func(_ context.Context, got CredentialCleanupClaim) error {
			calls = append(calls, "finish")
			received = append(received, got)
			return nil
		},
	})
	if err != nil || !completed {
		t.Fatalf("completed=%v err=%v", completed, err)
	}
	if want := []string{"validate", "referenced", "referenced", "mark", "delete", "absent", "finish"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%v want=%v", calls, want)
	}
	for _, got := range received {
		if got != claim {
			t.Fatalf("claim changed: got=%+v want=%+v", got, claim)
		}
	}
}

func TestCredentialCleanupRejectsInvalidClaimBeforeCallbacks(t *testing.T) {
	h := newAuthority(t)
	claim := fixtureCredentialCleanupClaim()
	claim.OperationVersion = ""
	calls := 0
	callback := func(context.Context, CredentialCleanupClaim) error { calls++; return nil }
	_, err := h.Service.DeleteCredentialIfUnreferenced(t.Context(), CredentialCleanupAttempt{
		Claim: claim, Intent: CredentialDeletionCASIntent{Opaque: []byte("intent")}, ValidateClaim: callback,
		Referenced: func(context.Context, CredentialCleanupClaim) (bool, error) { calls++; return false, nil },
		MarkDeleting: func(context.Context, CredentialCleanupClaim, CredentialDeletionCASIntent) (CredentialDeletionCapability, error) {
			calls++
			return CredentialDeletionCapability{}, nil
		},
		Delete:        func(context.Context, CredentialDeletionCapability) error { calls++; return nil },
		ConfirmAbsent: func(context.Context, CredentialDeletionCapability) (bool, error) { calls++; return true, nil },
		Finish:        callback,
	})
	if Code(err) != "invalid_command" || calls != 0 {
		t.Fatalf("code=%q calls=%d err=%v", Code(err), calls, err)
	}
}

func TestCredentialCleanupClaimUsesOpaqueCredentialReferenceGrammar(t *testing.T) {
	claim := fixtureCredentialCleanupClaim()
	claim.ReferenceID = "Vault.Ref_1-ABC"
	claim.ReferenceKeyID = "Key.Ref_2-XYZ"
	if !validCredentialCleanupClaim(claim) {
		t.Fatal("valid opaque credential reference was rejected")
	}
	for name, value := range map[string]string{
		"blank": "", "slash": "vault/ref", "space": "vault ref", "oversized": strings.Repeat("A", 129),
	} {
		t.Run(name, func(t *testing.T) {
			changed := claim
			changed.ReferenceID = value
			if validCredentialCleanupClaim(changed) {
				t.Fatalf("invalid reference ID accepted: %q", value)
			}
			changed = claim
			changed.ReferenceKeyID = value
			if validCredentialCleanupClaim(changed) {
				t.Fatalf("invalid reference key ID accepted: %q", value)
			}
		})
	}
}

func TestCredentialCleanupRejectsShapeValidStaleClaimsAndIntentExactly(t *testing.T) {
	expected := fixtureCredentialCleanupClaim()
	otherUUID := "70000000-0000-4000-8000-000000000099"
	tests := map[string]func(*CredentialCleanupAttempt){
		"lease":             func(attempt *CredentialCleanupAttempt) { attempt.Claim.LeaseToken = "stale-lease" },
		"operation version": func(attempt *CredentialCleanupAttempt) { attempt.Claim.OperationVersion = "8" },
		"operation ID":      func(attempt *CredentialCleanupAttempt) { attempt.Claim.OperationID = otherUUID },
		"command ID":        func(attempt *CredentialCleanupAttempt) { attempt.Claim.CommandID = otherUUID },
		"account binding":   func(attempt *CredentialCleanupAttempt) { attempt.Claim.AccountID = otherUUID },
		"object binding":    func(attempt *CredentialCleanupAttempt) { attempt.Claim.ObjectID = otherUUID },
		"reference":         func(attempt *CredentialCleanupAttempt) { attempt.Claim.ReferenceID = "Other.Ref-2" },
		"reference key":     func(attempt *CredentialCleanupAttempt) { attempt.Claim.ReferenceKeyID = "Other.Key-2" },
		"intent":            func(attempt *CredentialCleanupAttempt) { attempt.Intent.Opaque = []byte("stale-intent") },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			h := newAuthority(t)
			calls := map[string]int{}
			attempt := CredentialCleanupAttempt{Claim: expected, Intent: CredentialDeletionCASIntent{Opaque: []byte("expected-intent")}}
			attempt.ValidateClaim = func(_ context.Context, claim CredentialCleanupClaim) error {
				calls["validate"]++
				if claim != expected {
					return errorCode("credential_conflict")
				}
				return nil
			}
			attempt.Referenced = func(context.Context, CredentialCleanupClaim) (bool, error) { calls["referenced"]++; return false, nil }
			attempt.MarkDeleting = func(_ context.Context, claim CredentialCleanupClaim, intent CredentialDeletionCASIntent) (CredentialDeletionCapability, error) {
				calls["mark"]++
				if claim != expected || string(intent.Opaque) != "expected-intent" {
					return CredentialDeletionCapability{}, errorCode("credential_conflict")
				}
				return CredentialDeletionCapability{Opaque: []byte("capability")}, nil
			}
			attempt.Delete = func(context.Context, CredentialDeletionCapability) error { calls["delete"]++; return nil }
			attempt.ConfirmAbsent = func(context.Context, CredentialDeletionCapability) (bool, error) { calls["absent"]++; return true, nil }
			attempt.Finish = func(context.Context, CredentialCleanupClaim) error { calls["finish"]++; return nil }
			mutate(&attempt)
			if _, err := h.Service.DeleteCredentialIfUnreferenced(t.Context(), attempt); Code(err) != "credential_conflict" {
				t.Fatalf("code=%q err=%v", Code(err), err)
			}
			if name == "intent" {
				if !reflect.DeepEqual(calls, map[string]int{"validate": 1, "referenced": 2, "mark": 1}) {
					t.Fatalf("calls=%v", calls)
				}
			} else if !reflect.DeepEqual(calls, map[string]int{"validate": 1}) {
				t.Fatalf("calls=%v", calls)
			}
		})
	}
}

func TestCredentialCleanupStopsAfterStaleClaimAndRunsRemoteWorkOutsideFence(t *testing.T) {
	h := newAuthority(t)
	calls := 0
	_, err := h.Service.DeleteCredentialIfUnreferenced(t.Context(), CredentialCleanupAttempt{
		Claim: fixtureCredentialCleanupClaim(), Intent: CredentialDeletionCASIntent{Opaque: []byte("intent")},
		ValidateClaim: func(context.Context, CredentialCleanupClaim) error { calls++; return errorCode("credential_conflict") },
		Referenced:    func(context.Context, CredentialCleanupClaim) (bool, error) { calls++; return false, nil },
		MarkDeleting: func(context.Context, CredentialCleanupClaim, CredentialDeletionCASIntent) (CredentialDeletionCapability, error) {
			calls++
			return CredentialDeletionCapability{Opaque: []byte("capability")}, nil
		},
		Delete:        func(context.Context, CredentialDeletionCapability) error { calls++; return nil },
		ConfirmAbsent: func(context.Context, CredentialDeletionCapability) (bool, error) { calls++; return true, nil },
		Finish:        func(context.Context, CredentialCleanupClaim) error { calls++; return nil },
	})
	if Code(err) != "credential_conflict" || calls != 1 {
		t.Fatalf("code=%q calls=%d err=%v", Code(err), calls, err)
	}

	inside, outside := 0, 0
	completed, err := h.Service.DeleteCredentialIfUnreferenced(t.Context(), CredentialCleanupAttempt{
		Claim: fixtureCredentialCleanupClaim(), Intent: CredentialDeletionCASIntent{Opaque: []byte("intent")},
		ValidateClaim: func(ctx context.Context, _ CredentialCleanupClaim) error {
			if h.Service.hasFenceMarker(ctx) {
				t.Fatal("claim validation received internal fence marker")
			}
			if !h.Service.callbackContextDenied(ctx) {
				t.Fatal("claim validation did not receive callback denial")
			}
			inside++
			return nil
		},
		Referenced: func(ctx context.Context, _ CredentialCleanupClaim) (bool, error) {
			if h.Service.hasFenceMarker(ctx) {
				t.Fatal("reference check received internal fence marker")
			}
			if !h.Service.callbackContextDenied(ctx) {
				t.Fatal("reference check did not receive callback denial")
			}
			inside++
			return false, nil
		},
		MarkDeleting: func(ctx context.Context, _ CredentialCleanupClaim, _ CredentialDeletionCASIntent) (CredentialDeletionCapability, error) {
			if h.Service.hasFenceMarker(ctx) {
				t.Fatal("deleting CAS received internal fence marker")
			}
			if !h.Service.callbackContextDenied(ctx) {
				t.Fatal("deleting CAS did not receive callback denial")
			}
			inside++
			return CredentialDeletionCapability{Opaque: []byte("capability")}, nil
		},
		Delete: func(ctx context.Context, _ CredentialDeletionCapability) error {
			if h.Service.hasFenceMarker(ctx) {
				t.Fatal("remote delete retained authority fence marker")
			}
			outside++
			return nil
		},
		ConfirmAbsent: func(ctx context.Context, _ CredentialDeletionCapability) (bool, error) {
			if h.Service.hasFenceMarker(ctx) {
				t.Fatal("absence check retained authority fence marker")
			}
			outside++
			return true, nil
		},
		Finish: func(ctx context.Context, _ CredentialCleanupClaim) error {
			if h.Service.hasFenceMarker(ctx) {
				t.Fatal("finish retained authority fence marker")
			}
			outside++
			return nil
		},
	})
	if err != nil || !completed || inside != 4 || outside != 3 {
		t.Fatalf("completed=%v inside=%d outside=%d err=%v", completed, inside, outside, err)
	}
}

func TestAccountDeletionUsesDistinctTypedFenceSeam(t *testing.T) {
	h := newAuthority(t)
	claim := fixtureAccountDeletionClaim()
	var order []string
	capability, err := h.Service.MarkCredentialDeletingForAccountDeletion(t.Context(), AccountDeletionCredentialAttempt{
		Claim: claim, Intent: CredentialDeletionCASIntent{Opaque: []byte("intent")},
		ValidateClaimedJob: func(ctx context.Context, _ AccountDeletionJobClaim) error {
			if h.Service.hasFenceMarker(ctx) || !h.Service.callbackContextDenied(ctx) {
				t.Fatal("account deletion validation received unsafe callback context")
			}
			order = append(order, "validate")
			return nil
		},
		ReferencedOutsideJob: func(ctx context.Context, _ AccountDeletionJobClaim) (bool, error) {
			if h.Service.hasFenceMarker(ctx) || !h.Service.callbackContextDenied(ctx) {
				t.Fatal("account deletion reference check received unsafe callback context")
			}
			order = append(order, "referenced")
			return false, nil
		},
		MarkDeleting: func(ctx context.Context, _ AccountDeletionJobClaim, _ CredentialDeletionCASIntent) (CredentialDeletionCapability, error) {
			if h.Service.hasFenceMarker(ctx) || !h.Service.callbackContextDenied(ctx) {
				t.Fatal("account deletion CAS received unsafe callback context")
			}
			order = append(order, "mark")
			return CredentialDeletionCapability{Opaque: []byte("capability")}, nil
		},
	})
	if err != nil || string(capability.Opaque) != "capability" || !reflect.DeepEqual(order, []string{"validate", "referenced", "referenced", "mark"}) {
		t.Fatalf("capability=%q order=%v err=%v", capability.Opaque, order, err)
	}
}

func TestAccountDeletionRejectsInvalidJobClaimBeforeCallbacks(t *testing.T) {
	h := newAuthority(t)
	calls := 0
	_, err := h.Service.MarkCredentialDeletingForAccountDeletion(t.Context(), AccountDeletionCredentialAttempt{
		Claim: AccountDeletionJobClaim{}, Intent: CredentialDeletionCASIntent{Opaque: []byte("intent")},
		ValidateClaimedJob:   func(context.Context, AccountDeletionJobClaim) error { calls++; return nil },
		ReferencedOutsideJob: func(context.Context, AccountDeletionJobClaim) (bool, error) { calls++; return false, nil },
		MarkDeleting: func(context.Context, AccountDeletionJobClaim, CredentialDeletionCASIntent) (CredentialDeletionCapability, error) {
			calls++
			return CredentialDeletionCapability{Opaque: []byte("capability")}, nil
		},
	})
	if Code(err) != "invalid_command" || calls != 0 {
		t.Fatalf("code=%q calls=%d err=%v", Code(err), calls, err)
	}
}

func TestAccountDeletionRejectsShapeValidStaleClaimsAndIntentExactly(t *testing.T) {
	expected := fixtureAccountDeletionClaim()
	otherUUID := "70000000-0000-4000-8000-000000000099"
	tests := map[string]func(*AccountDeletionCredentialAttempt){
		"job":               func(attempt *AccountDeletionCredentialAttempt) { attempt.Claim.JobID = otherUUID },
		"lease":             func(attempt *AccountDeletionCredentialAttempt) { attempt.Claim.LeaseToken = "stale-lease" },
		"operation version": func(attempt *AccountDeletionCredentialAttempt) { attempt.Claim.OperationVersion = "10" },
		"operation ID":      func(attempt *AccountDeletionCredentialAttempt) { attempt.Claim.OperationID = otherUUID },
		"command ID":        func(attempt *AccountDeletionCredentialAttempt) { attempt.Claim.CommandID = otherUUID },
		"status":            func(attempt *AccountDeletionCredentialAttempt) { attempt.Claim.Status = "retry" },
		"account binding":   func(attempt *AccountDeletionCredentialAttempt) { attempt.Claim.AccountID = otherUUID },
		"server instance":   func(attempt *AccountDeletionCredentialAttempt) { attempt.Claim.ServerInstanceID = otherUUID },
		"purpose":           func(attempt *AccountDeletionCredentialAttempt) { attempt.Claim.Purpose = "wrong" },
		"object binding":    func(attempt *AccountDeletionCredentialAttempt) { attempt.Claim.ObjectID = otherUUID },
		"reference":         func(attempt *AccountDeletionCredentialAttempt) { attempt.Claim.ReferenceID = "Other.Ref-2" },
		"reference key":     func(attempt *AccountDeletionCredentialAttempt) { attempt.Claim.ReferenceKeyID = "Other.Key-2" },
		"attempt":           func(attempt *AccountDeletionCredentialAttempt) { attempt.Claim.Attempts++ },
		"lease expiry": func(attempt *AccountDeletionCredentialAttempt) {
			attempt.Claim.LeaseExpiresAt = attempt.Claim.LeaseExpiresAt.Add(time.Second)
		},
		"intent": func(attempt *AccountDeletionCredentialAttempt) { attempt.Intent.Opaque = []byte("stale-intent") },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			h := newAuthority(t)
			calls := map[string]int{}
			attempt := AccountDeletionCredentialAttempt{Claim: expected, Intent: CredentialDeletionCASIntent{Opaque: []byte("expected-intent")}}
			attempt.ValidateClaimedJob = func(_ context.Context, claim AccountDeletionJobClaim) error {
				calls["validate"]++
				if claim != expected {
					return errorCode("credential_conflict")
				}
				return nil
			}
			attempt.ReferencedOutsideJob = func(context.Context, AccountDeletionJobClaim) (bool, error) { calls["referenced"]++; return false, nil }
			attempt.MarkDeleting = func(_ context.Context, claim AccountDeletionJobClaim, intent CredentialDeletionCASIntent) (CredentialDeletionCapability, error) {
				calls["mark"]++
				if claim != expected || string(intent.Opaque) != "expected-intent" {
					return CredentialDeletionCapability{}, errorCode("credential_conflict")
				}
				return CredentialDeletionCapability{Opaque: []byte("capability")}, nil
			}
			mutate(&attempt)
			_, err := h.Service.MarkCredentialDeletingForAccountDeletion(t.Context(), attempt)
			if name == "purpose" {
				if Code(err) != "invalid_command" || len(calls) != 0 {
					t.Fatalf("code=%q calls=%v err=%v", Code(err), calls, err)
				}
				return
			}
			if Code(err) != "credential_conflict" {
				t.Fatalf("code=%q err=%v", Code(err), err)
			}
			if name == "intent" {
				if !reflect.DeepEqual(calls, map[string]int{"validate": 1, "referenced": 2, "mark": 1}) {
					t.Fatalf("calls=%v", calls)
				}
			} else if !reflect.DeepEqual(calls, map[string]int{"validate": 1}) {
				t.Fatalf("calls=%v", calls)
			}
		})
	}
}

func TestCleanupRetainedCallbackContextsCannotBypassFence(t *testing.T) {
	t.Run("orphan cleanup", func(t *testing.T) {
		h := newAuthority(t)
		var retained context.Context
		completed, err := h.Service.DeleteCredentialIfUnreferenced(t.Context(), CredentialCleanupAttempt{
			Claim: fixtureCredentialCleanupClaim(), Intent: CredentialDeletionCASIntent{Opaque: []byte("intent")},
			ValidateClaim: func(ctx context.Context, _ CredentialCleanupClaim) error { retained = ctx; return nil },
			Referenced:    func(context.Context, CredentialCleanupClaim) (bool, error) { return true, nil },
			MarkDeleting: func(context.Context, CredentialCleanupClaim, CredentialDeletionCASIntent) (CredentialDeletionCapability, error) {
				return CredentialDeletionCapability{Opaque: []byte("capability")}, nil
			},
			Delete:        func(context.Context, CredentialDeletionCapability) error { return nil },
			ConfirmAbsent: func(context.Context, CredentialDeletionCapability) (bool, error) { return true, nil },
			Finish:        func(context.Context, CredentialCleanupClaim) error { return nil },
		})
		if err != nil || completed {
			t.Fatalf("completed=%v err=%v", completed, err)
		}
		if h.Service.hasFenceMarker(retained) {
			t.Fatal("orphan cleanup retained an active fence lease")
		}
		assertCallbackContextDenied(t, h, retained)
	})

	t.Run("account deletion", func(t *testing.T) {
		h := newAuthority(t)
		var retained context.Context
		_, err := h.Service.MarkCredentialDeletingForAccountDeletion(t.Context(), AccountDeletionCredentialAttempt{
			Claim: fixtureAccountDeletionClaim(), Intent: CredentialDeletionCASIntent{Opaque: []byte("intent")},
			ValidateClaimedJob:   func(ctx context.Context, _ AccountDeletionJobClaim) error { retained = ctx; return nil },
			ReferencedOutsideJob: func(context.Context, AccountDeletionJobClaim) (bool, error) { return true, nil },
			MarkDeleting: func(context.Context, AccountDeletionJobClaim, CredentialDeletionCASIntent) (CredentialDeletionCapability, error) {
				return CredentialDeletionCapability{Opaque: []byte("capability")}, nil
			},
		})
		if Code(err) != "credential_conflict" {
			t.Fatalf("code=%q err=%v", Code(err), err)
		}
		if h.Service.hasFenceMarker(retained) {
			t.Fatal("account deletion retained an active fence lease")
		}
		assertCallbackContextDenied(t, h, retained)
	})
}

func TestCredentialCleanupAndPreparedExecutionSerializeOnAuthorityFence(t *testing.T) {
	h := newAuthority(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		_, _ = h.Service.DeleteCredentialIfUnreferenced(context.Background(), CredentialCleanupAttempt{
			Claim: fixtureCredentialCleanupClaim(), Intent: CredentialDeletionCASIntent{Opaque: []byte("intent")},
			ValidateClaim: func(context.Context, CredentialCleanupClaim) error { close(entered); <-release; return nil },
			Referenced:    func(context.Context, CredentialCleanupClaim) (bool, error) { return true, nil },
			MarkDeleting: func(context.Context, CredentialCleanupClaim, CredentialDeletionCASIntent) (CredentialDeletionCapability, error) {
				return CredentialDeletionCapability{}, nil
			},
			Delete:        func(context.Context, CredentialDeletionCapability) error { return nil },
			ConfirmAbsent: func(context.Context, CredentialDeletionCapability) (bool, error) { return true, nil },
			Finish:        func(context.Context, CredentialCleanupClaim) error { return nil },
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
		t.Fatal("prepared callback entered while cleanup held the fence")
	default:
	}
	close(release)
	wait.Wait()
	<-preparedEntered
}

func fixtureCredentialCleanupClaim() CredentialCleanupClaim {
	return CredentialCleanupClaim{LeaseToken: "lease", OperationVersion: "7", OperationID: "70000000-0000-4000-8000-000000000011", CommandID: "70000000-0000-4000-8000-000000000012", AccountID: preparedAccount, ObjectID: "70000000-0000-4000-8000-000000000013", ReferenceID: "70000000-0000-4000-8000-000000000014", ReferenceKeyID: "credential.v1"}
}

func fixtureAccountDeletionClaim() AccountDeletionJobClaim {
	return AccountDeletionJobClaim{JobID: "70000000-0000-4000-8000-000000000001", LeaseToken: "lease", OperationID: "70000000-0000-4000-8000-000000000002", CommandID: "70000000-0000-4000-8000-000000000003", OperationVersion: "9", Status: "pending", AccountID: preparedAccount, ServerInstanceID: "70000000-0000-4000-8000-000000000006", Purpose: "apple-refresh-revocation-v1", ObjectID: "70000000-0000-4000-8000-000000000004", ReferenceID: "70000000-0000-4000-8000-000000000005", ReferenceKeyID: "credential.v1", Attempts: 1, LeaseExpiresAt: time.Date(2026, 9, 21, 12, 0, 30, 0, time.UTC)}
}
