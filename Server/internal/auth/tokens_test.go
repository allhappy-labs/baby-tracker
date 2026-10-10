package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/identity"
)

func TestOpaqueTokenIsCanonicalRandom32Bytes(t *testing.T) {
	first, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(first)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != first {
		t.Fatalf("token is not canonical 32-byte base64url: %q err=%v", first, err)
	}
	if first == second {
		t.Fatal("two opaque tokens were identical")
	}
}

func TestTokenDigestKnownAnswerAndClassSeparation(t *testing.T) {
	key := sequentialDigestKey("tokens.v1")
	digest := tokenDigest(key, "access-v1", "token")
	if digest.KeyID != "tokens.v1" || hex.EncodeToString(digest.Sum[:]) != "94e5b9368585987beffdfe73e2850ec5cc01bdd0bd01bd522eaa588e92863e3e" {
		t.Fatalf("digest=%s:%x", digest.KeyID, digest.Sum)
	}
	if digest == tokenDigest(key, "refresh-v1", "token") {
		t.Fatal("token class was not bound")
	}
}

func TestActorIDKnownAnswer(t *testing.T) {
	key := sequentialSecret()
	got, err := ActorID(key, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", identity.VerifiedIdentity{
		Provider: "apple", Audience: "com.example", Subject: "000123",
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != "430e9857-3700-817e-9758-e28975ee7fab" {
		t.Fatalf("actor ID=%s", got)
	}
}

func TestActorIDRejectsUnsafeGenerations(t *testing.T) {
	verified := identity.VerifiedIdentity{Provider: "apple", Audience: "com.example", Subject: "000123"}
	for _, generation := range []uint64{0, maxSafeJSONInteger + 1} {
		if _, err := ActorID(sequentialSecret(), "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", verified, generation); err == nil {
			t.Fatalf("generation %d was accepted", generation)
		}
	}
	if _, err := ActorID(sequentialSecret(), "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", verified, maxSafeJSONInteger); err != nil {
		t.Fatalf("maximum safe generation rejected: %v", err)
	}
}

func TestRefreshRotationAndReuseKnownAnswers(t *testing.T) {
	input := refreshKnownAnswerInput(t)
	got, err := DeriveRefreshRotation(input)
	if err != nil {
		t.Fatal(err)
	}
	if got.CommandID != "322e17f4-5601-83a1-9345-a98b6c832e64" ||
		got.SessionID != "fc846652-35f0-8428-8faa-206b9a3290e2" ||
		got.AccessToken != "lwxi1Y3-hKKbo6o6fqlTQQad6Wn-GhoRKWfJ1mxtNnI" ||
		got.RefreshToken != "gELeWd0inDHrWNCVX24uinf8EWyMuX_zojxgXrql4rc" {
		t.Fatalf("rotation=%+v", got)
	}
	reuse, err := DeriveRefreshReuseCommand(input)
	if err != nil {
		t.Fatal(err)
	}
	if reuse != "1d06b98d-47ae-8680-ac49-d4fab466491a" {
		t.Fatalf("reuse command=%s", reuse)
	}
}

func TestRefreshHeadAndDeadlineDriftChangesRotationButNotReuse(t *testing.T) {
	base := refreshKnownAnswerInput(t)
	baseRotation, err := DeriveRefreshRotation(base)
	if err != nil {
		t.Fatal(err)
	}
	baseReuse, err := DeriveRefreshReuseCommand(base)
	if err != nil {
		t.Fatal(err)
	}

	headDrift := base
	headDrift.HeadRevision = 42
	headDrift.TargetRevision = 43
	copy(headDrift.HeadHash[:], mustHex(t, "4142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f60"))
	headRotation, err := DeriveRefreshRotation(headDrift)
	if err != nil {
		t.Fatal(err)
	}
	if headRotation.CommandID != "66a9b93e-3d6e-8022-b4af-7b5e7f05e4b6" || headRotation == baseRotation {
		t.Fatalf("head drift rotation=%+v", headRotation)
	}
	headReuse, err := DeriveRefreshReuseCommand(headDrift)
	if err != nil || headReuse != baseReuse {
		t.Fatalf("head drift changed reuse: %s err=%v", headReuse, err)
	}

	deadlineDrift := base
	deadlineDrift.AccessExpiresAt = deadlineDrift.AccessExpiresAt.Add(time.Nanosecond)
	deadlineRotation, err := DeriveRefreshRotation(deadlineDrift)
	if err != nil {
		t.Fatal(err)
	}
	if deadlineRotation == baseRotation {
		t.Fatal("deadline drift did not change every rotation output")
	}
	deadlineReuse, err := DeriveRefreshReuseCommand(deadlineDrift)
	if err != nil || deadlineReuse != baseReuse {
		t.Fatalf("deadline drift changed reuse: %s err=%v", deadlineReuse, err)
	}
}

func TestRefreshRotationRejectsInvalidDeadlineOrdering(t *testing.T) {
	base := refreshKnownAnswerInput(t)
	tests := map[string]func(*RefreshDerivationInput){
		"access after refresh": func(input *RefreshDerivationInput) {
			input.AccessExpiresAt = input.RefreshExpiresAt.Add(time.Nanosecond)
		},
		"refresh after reauthentication": func(input *RefreshDerivationInput) {
			input.RefreshExpiresAt = input.ReauthenticateAt.Add(time.Nanosecond)
		},
		"reauthentication before absolute": func(input *RefreshDerivationInput) {
			input.AbsoluteExpiresAt = input.ReauthenticateAt.Add(time.Nanosecond)
		},
		"reauthentication after absolute": func(input *RefreshDerivationInput) {
			input.AbsoluteExpiresAt = input.ReauthenticateAt.Add(-time.Nanosecond)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			input := base
			mutate(&input)
			if _, err := DeriveRefreshRotation(input); err == nil {
				t.Fatal("invalid deadline ordering accepted")
			}
		})
	}
	equal := base
	equal.AccessExpiresAt = equal.RefreshExpiresAt
	if _, err := DeriveRefreshRotation(equal); err != nil {
		t.Fatalf("allowed equal access and refresh deadlines rejected: %v", err)
	}
}

func TestRefreshRotationRequiresCanonicalRFC3339NanoDeadlineRange(t *testing.T) {
	base := refreshKnownAnswerInput(t)
	for name, deadline := range map[string]time.Time{
		"year zero":         time.Date(0, time.January, 2, 0, 0, 0, 0, time.UTC),
		"year ten-thousand": time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC),
	} {
		t.Run(name, func(t *testing.T) {
			input := base
			input.AccessExpiresAt = deadline
			input.RefreshExpiresAt = deadline
			input.ReauthenticateAt = deadline
			input.AbsoluteExpiresAt = deadline
			if _, err := DeriveRefreshRotation(input); err == nil {
				t.Fatalf("out-of-range deadline %s was accepted", deadline)
			}
		})
	}

	for name, deadline := range map[string]time.Time{
		"year one":                 time.Date(1, time.January, 2, 0, 0, 0, 1, time.UTC),
		"year nine-nine-nine-nine": time.Date(9999, time.December, 31, 23, 59, 59, 999999999, time.UTC),
	} {
		t.Run(name, func(t *testing.T) {
			input := base
			input.AccessExpiresAt = deadline
			input.RefreshExpiresAt = deadline
			input.ReauthenticateAt = deadline
			input.AbsoluteExpiresAt = deadline
			if _, err := DeriveRefreshRotation(input); err != nil {
				t.Fatalf("canonical boundary deadline %s rejected: %v", deadline, err)
			}
		})
	}
}

func TestRefreshDerivationDeadlinesAlwaysEncodeInStage(t *testing.T) {
	input := refreshKnownAnswerInput(t)
	input.AccessExpiresAt = time.Date(2026, time.September, 20, 12, 10, 0, 123456789, time.UTC)
	if _, err := DeriveRefreshRotation(input); err != nil {
		t.Fatal(err)
	}
	common := validRefreshStage().StageCommon
	common.AccessExpiresAt = input.AccessExpiresAt.Format(time.RFC3339Nano)
	common.RefreshExpiresAt = input.RefreshExpiresAt.Format(time.RFC3339Nano)
	common.ReauthenticateAt = input.ReauthenticateAt.Format(time.RFC3339Nano)
	common.AbsoluteExpiresAt = input.AbsoluteExpiresAt.Format(time.RFC3339Nano)
	stage := RefreshStage{StageCommon: common, ParentSessionID: input.ParentSessionID}
	if _, _, err := EncodeRefreshStage(stage); err != nil {
		t.Fatalf("derived deadlines did not encode in refresh stage: %v", err)
	}
}

func TestRefreshStageEncodingHasFrozenOrderAndExplicitNulls(t *testing.T) {
	stage := validRefreshStage()
	encoded, digest, err := EncodeRefreshStage(stage)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := `{"version":"v1","kind":"refresh","account_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","device_id":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","session_id":"eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee","lineage_root_session_id":"eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"`
	if !strings.HasPrefix(string(encoded), wantPrefix) {
		t.Fatalf("stage order=%s", encoded)
	}
	if !strings.Contains(string(encoded), `"family_id":null`) {
		t.Fatalf("stage omitted family_id: %s", encoded)
	}
	if digest != sha256.Sum256(encoded) || len(encoded) > maxStageBytes {
		t.Fatal("stage digest or bound is wrong")
	}
}

func TestRefreshStageEncodingExcludesExchangeFields(t *testing.T) {
	stage := validRefreshStage()
	encoded, _, err := EncodeRefreshStage(stage)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(encoded), `"parent_session_id":"11111111-2222-4333-8444-555555555555"}`) || strings.Contains(string(encoded), "identity_provider") {
		t.Fatalf("refresh stage shape=%s", encoded)
	}
}

func TestStageEncodingRejectsInvalidDeadlineOrdering(t *testing.T) {
	tests := map[string]func(*StageCommon){
		"access after refresh":             func(stage *StageCommon) { stage.AccessExpiresAt = "2026-10-20T12:00:00.000000001Z" },
		"refresh after reauthentication":   func(stage *StageCommon) { stage.RefreshExpiresAt = "2026-10-20T12:00:00.000000001Z" },
		"reauthentication before absolute": func(stage *StageCommon) { stage.AbsoluteExpiresAt = "2026-10-20T12:00:00.000000001Z" },
		"reauthentication after absolute":  func(stage *StageCommon) { stage.AbsoluteExpiresAt = "2026-10-20T11:59:59.999999999Z" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			stage := validRefreshStage()
			mutate(&stage.StageCommon)
			if _, _, err := EncodeRefreshStage(stage); err == nil {
				t.Fatal("invalid refresh deadlines accepted")
			}
		})
	}
}

func TestStageOriginMustAlreadyBeCanonicalHTTPSDNSOrigin(t *testing.T) {
	accepted := []string{"https://sync.example.test", "https://sync.example.test:8443", "https://xn--bcher-kva.example"}
	for _, origin := range accepted {
		stage := validRefreshStage()
		stage.CanonicalOrigin = origin
		if _, _, err := EncodeRefreshStage(stage); err != nil {
			t.Fatalf("canonical origin %q rejected: %v", origin, err)
		}
	}

	rejected := []string{
		"HTTPS://sync.example.test", "https://SYNC.example.test", "https://sync.example.test.",
		"https://sync.example.test:443", "https://sync.example.test:08443", "https://sync.example.test:0",
		"https://127.0.0.1", "https://[::1]", "https://user@sync.example.test",
		"https://sync.example.test/", "https://sync.example.test/path", "https://sync.example.test?query", "https://sync.example.test#fragment",
		"https://bad_label.example", "https://-bad.example", "https://bad-.example", "https://bad..example", "https://bücher.example",
	}
	for _, origin := range rejected {
		t.Run(origin, func(t *testing.T) {
			stage := validRefreshStage()
			stage.CanonicalOrigin = origin
			if _, _, err := EncodeRefreshStage(stage); err == nil {
				t.Fatalf("noncanonical origin %q accepted", origin)
			}
		})
	}
}

func validRefreshStage() RefreshStage {
	return RefreshStage{StageCommon: StageCommon{
		Version: "v1", Kind: "refresh", AccountID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		DeviceID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", SessionID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
		LineageRootSessionID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", ServerInstanceID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		CanonicalOrigin: "https://sync.example.test", TokenKeyID: "session.v1", AccessDigest: strings.Repeat("a", 64),
		RefreshDigest: strings.Repeat("b", 64), RecoveryEpoch: 7, SecurityRevision: 42, Scope: "account",
		AccessExpiresAt: "2026-09-20T12:10:00Z", RefreshExpiresAt: "2026-10-20T12:00:00Z",
		ReauthenticateAt: "2026-10-20T12:00:00Z", AbsoluteExpiresAt: "2026-10-20T12:00:00Z",
	}, ParentSessionID: "11111111-2222-4333-8444-555555555555"}
}

func refreshKnownAnswerInput(t *testing.T) RefreshDerivationInput {
	t.Helper()
	parse := func(value string) time.Time {
		result, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	input := RefreshDerivationInput{
		ServerInstanceID:     "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		AccountID:            "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		DeviceID:             "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		ParentSessionID:      "11111111-2222-4333-8444-555555555555",
		LineageRootSessionID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd",
		RecoveryEpoch:        7,
		ParentKey:            sequentialDigestKey("k1"),
		OutputKeyID:          "k2",
		Scope:                "account",
		AccessExpiresAt:      parse("2026-09-20T12:10:00Z"),
		RefreshExpiresAt:     parse("2026-10-20T12:00:00Z"),
		ReauthenticateAt:     parse("2026-10-20T12:00:00Z"),
		AbsoluteExpiresAt:    parse("2026-10-20T12:00:00Z"),
		HeadGeneration:       1,
		HeadRevision:         41,
		TargetRevision:       42,
	}
	copy(input.ParentDigest[:], mustHex(t, "202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f"))
	copy(input.HeadHash[:], mustHex(t, "404142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f"))
	return input
}

func sequentialDigestKey(id string) identity.DigestKey {
	return identity.DigestKey{ID: id, Secret: sequentialSecret()}
}

func sequentialSecret() [32]byte {
	var result [32]byte
	for index := range result {
		result[index] = byte(index)
	}
	return result
}

func mustHex(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

// The authority's derivation input maps onto the frozen v2 and v3 domains:
// the ledger head fields are fed values fixed by the parent row
// (generation 1, issued_seq, sha256(parent ID), issued_seq+1) and both
// deadlines are the lineage's deadline_at. Derivation and material are
// therefore functions of the rows a lost-acknowledgement retry rebuilds, and
// material never needs the parent key secret.
func TestAuthorityDerivationInputFeedsFrozenDomains(t *testing.T) {
	want := refreshKnownAnswerInput(t)
	service := &Service{instanceID: want.ServerInstanceID, origin: "https://sync.example.test"}
	parent := authority.SessionRow{ID: want.ParentSessionID, AccountID: want.AccountID, DeviceID: want.DeviceID, LineageRootID: want.LineageRootSessionID,
		Scope: "account", Status: "active", RecoveryEpoch: want.RecoveryEpoch, IssuedSeq: 41, TokenKeyID: want.ParentKey.ID, DeadlineAt: want.AbsoluteExpiresAt,
		LineageStartedAt: want.AbsoluteExpiresAt.Add(-30 * 24 * time.Hour)}
	in := authority.RefreshDerivationInput{Parent: parent, ParentKey: want.ParentKey, ParentDigest: want.ParentDigest, OutputKeyID: want.OutputKeyID,
		Scope: "account", AccessExpiresAt: want.AccessExpiresAt, RefreshExpiresAt: want.RefreshExpiresAt}
	want.HeadGeneration, want.HeadRevision, want.TargetRevision, want.HeadHash = 1, 41, 42, sha256.Sum256([]byte(parent.ID))
	if got := service.derivationInput(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("v2 input=%+v want=%+v", got, want)
	}
	expected, err := DeriveRefreshRotation(want)
	if err != nil {
		t.Fatal(err)
	}
	if derived, err := service.deriveRotation(in); err != nil || derived != authority.RefreshDerivation(expected) {
		t.Fatalf("v2 derivation=%+v err=%v want=%+v", derived, err, expected)
	}

	in.AttemptID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	in.Continuity = &authority.DeviceBinding{DeviceID: want.DeviceID, AccountID: want.AccountID, CanonicalOrigin: service.origin, CredentialKeyID: "device.v1"}
	in.Continuity.CredentialDigest[0] = 0x5a
	recovery := RecoveryDerivationInput{RefreshDerivationInput: want, AttemptID: in.AttemptID, CanonicalOrigin: service.origin,
		ContinuityKeyID: "device.v1", ContinuityDigest: in.Continuity.CredentialDigest}
	if got := service.recoveryDerivationInput(in); !reflect.DeepEqual(got, recovery) {
		t.Fatalf("v3 input=%+v want=%+v", got, recovery)
	}
	expected, err = DeriveRecoverableRefreshRotation(recovery)
	if err != nil {
		t.Fatal(err)
	}
	derived, err := service.deriveRecoverableRotation(in)
	if err != nil || derived != authority.RefreshDerivation(expected) {
		t.Fatalf("v3 derivation=%+v err=%v want=%+v", derived, err, expected)
	}

	// Material sees the stage with the key secret zeroed and is stable.
	stage := authority.RefreshStage{Input: in, EffectiveDeadline: want.AbsoluteExpiresAt, Child: authority.SessionRow{ID: derived.SessionID,
		AccountID: want.AccountID, DeviceID: want.DeviceID, LineageRootID: want.LineageRootSessionID, ParentID: parent.ID, Scope: "account",
		Status: "active", RecoveryEpoch: want.RecoveryEpoch, IssuedSeq: 57, TokenKeyID: want.OutputKeyID, AccessExpiresAt: want.AccessExpiresAt,
		RefreshExpiresAt: want.RefreshExpiresAt, DeadlineAt: want.AbsoluteExpiresAt, CreatedAt: want.AccessExpiresAt.Add(-10 * time.Minute)}}
	stage.Child.AccessDigest[0], stage.Child.RefreshDigest[0] = 1, 2
	stage.Input.ParentKey.Secret = [32]byte{}
	first, digest, err := service.recoverableRotationMaterial(stage)
	if err != nil {
		t.Fatal(err)
	}
	second, secondDigest, err := service.recoverableRotationMaterial(stage)
	if err != nil || !bytes.Equal(first, second) || digest != secondDigest || digest != sha256.Sum256(first) {
		t.Fatalf("unstable recovery material: %v", err)
	}
	var decoded RefreshStage
	if err := json.Unmarshal(first, &decoded); err != nil || decoded.Recovery == nil {
		t.Fatalf("material=%s err=%v", first, err)
	}
	frozen := decoded.Recovery.Input
	if frozen.HeadRevision != 41 || frozen.TargetRevision != 42 || frozen.HeadHash != want.HeadHash || frozen.ParentKeyID != want.ParentKey.ID ||
		frozen.AttemptID != in.AttemptID || decoded.SecurityRevision != 57 || !decoded.Recovery.CreatedAt.Equal(stage.Child.CreatedAt) {
		t.Fatalf("frozen material=%+v stage=%+v", frozen, decoded.StageCommon)
	}
	restored, err := RestoreRecoveryInput(frozen, want.ParentKey)
	if err != nil || !reflect.DeepEqual(restored, recovery) {
		t.Fatalf("restored=%+v err=%v", restored, err)
	}
	legacy, legacyDigest, err := service.rotationMaterial(stage)
	if err != nil || legacyDigest != sha256.Sum256(legacy) || bytes.Contains(legacy, []byte(`"recovery":`)) {
		t.Fatalf("refresh material=%s err=%v", legacy, err)
	}
}
