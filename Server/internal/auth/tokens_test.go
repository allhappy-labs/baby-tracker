package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
	"time"

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

func TestCredentialObjectIDKnownAnswer(t *testing.T) {
	got, err := CredentialObjectID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "apple-refresh-revocation-v1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "f71e3c87-1a11-8bac-88ba-b811fd571347" {
		t.Fatalf("object ID=%s", got)
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
	common := validExchangeStage().StageCommon
	common.Kind = "refresh"
	common.AccessExpiresAt = input.AccessExpiresAt.Format(time.RFC3339Nano)
	common.RefreshExpiresAt = input.RefreshExpiresAt.Format(time.RFC3339Nano)
	common.ReauthenticateAt = input.ReauthenticateAt.Format(time.RFC3339Nano)
	common.AbsoluteExpiresAt = input.AbsoluteExpiresAt.Format(time.RFC3339Nano)
	stage := RefreshStage{StageCommon: common, ParentSessionID: input.ParentSessionID}
	if _, _, err := EncodeRefreshStage(stage); err != nil {
		t.Fatalf("derived deadlines did not encode in refresh stage: %v", err)
	}
}

func TestExchangeStageEncodingHasFrozenOrderAndExplicitNulls(t *testing.T) {
	stage := validExchangeStage()
	encoded, digest, err := EncodeExchangeStage(stage)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := `{"version":"v1","kind":"exchange","account_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","device_id":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","session_id":"eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee","lineage_root_session_id":"eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"`
	if !strings.HasPrefix(string(encoded), wantPrefix) {
		t.Fatalf("stage order=%s", encoded)
	}
	for _, explicit := range []string{`"family_id":null`, `"display_name":null`, `"installation_key_id":null`, `"apple_credential_id":null`} {
		if !strings.Contains(string(encoded), explicit) {
			t.Fatalf("stage omitted %s: %s", explicit, encoded)
		}
	}
	if digest != sha256.Sum256(encoded) || len(encoded) > maxStageBytes {
		t.Fatal("stage digest or bound is wrong")
	}
}

func TestRefreshStageEncodingExcludesExchangeFields(t *testing.T) {
	common := validExchangeStage().StageCommon
	stage := RefreshStage{StageCommon: common, ParentSessionID: "11111111-2222-4333-8444-555555555555"}
	stage.Kind = "refresh"
	encoded, _, err := EncodeRefreshStage(stage)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(encoded), `"parent_session_id":"11111111-2222-4333-8444-555555555555"}`) || strings.Contains(string(encoded), "identity_provider") {
		t.Fatalf("refresh stage shape=%s", encoded)
	}
}

func TestStageEncodingRejectsInconsistentOptionalGroups(t *testing.T) {
	stage := validExchangeStage()
	value := "installation.v1"
	stage.InstallationKeyID = &value
	if _, _, err := EncodeExchangeStage(stage); err == nil {
		t.Fatal("half-present installation digest accepted")
	}
	stage = validExchangeStage()
	stage.RegisterDevice = true
	if _, _, err := EncodeExchangeStage(stage); err == nil {
		t.Fatal("registration without continuity digest accepted")
	}
}

func TestCredentialReferenceIdentifiersUseBoundedOpaqueGrammar(t *testing.T) {
	accepted := []string{"A", "Vault.Ref_01-key", strings.Repeat("Z", 128)}
	for _, value := range accepted {
		t.Run("accept_"+value[:1], func(t *testing.T) {
			stage := validExchangeStage()
			purpose := appleCredentialPurpose
			objectID := "11111111-2222-4333-8444-555555555555"
			stage.AppleCredentialID = &value
			stage.AppleCredentialKeyID = &value
			stage.AppleCredentialPurpose = &purpose
			stage.AppleCredentialObjectID = &objectID
			if _, _, err := EncodeExchangeStage(stage); err != nil {
				t.Fatalf("credential reference %q rejected: %v", value, err)
			}
		})
	}

	rejected := []string{"", strings.Repeat("a", 129), "slash/value", "colon:value", "white space", "unicode-é"}
	for _, field := range []string{"id", "key_id"} {
		for _, value := range rejected {
			t.Run(field+"_reject", func(t *testing.T) {
				stage := validExchangeStage()
				validID, validKeyID := "credential.ref", "vault.Key_01"
				purpose := appleCredentialPurpose
				objectID := "11111111-2222-4333-8444-555555555555"
				if field == "id" {
					validID = value
				} else {
					validKeyID = value
				}
				stage.AppleCredentialID = &validID
				stage.AppleCredentialKeyID = &validKeyID
				stage.AppleCredentialPurpose = &purpose
				stage.AppleCredentialObjectID = &objectID
				if _, _, err := EncodeExchangeStage(stage); err == nil {
					t.Fatalf("invalid %s %q accepted", field, value)
				}
			})
		}
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
		for _, kind := range []string{"exchange", "refresh"} {
			t.Run(kind+"_"+name, func(t *testing.T) {
				common := validExchangeStage().StageCommon
				mutate(&common)
				if kind == "exchange" {
					stage := validExchangeStage()
					stage.StageCommon = common
					if _, _, err := EncodeExchangeStage(stage); err == nil {
						t.Fatal("invalid exchange deadlines accepted")
					}
					return
				}
				common.Kind = "refresh"
				stage := RefreshStage{StageCommon: common, ParentSessionID: "11111111-2222-4333-8444-555555555555"}
				if _, _, err := EncodeRefreshStage(stage); err == nil {
					t.Fatal("invalid refresh deadlines accepted")
				}
			})
		}
	}
}

func TestStageOriginMustAlreadyBeCanonicalHTTPSDNSOrigin(t *testing.T) {
	accepted := []string{"https://sync.example.test", "https://sync.example.test:8443", "https://xn--bcher-kva.example"}
	for _, origin := range accepted {
		stage := validExchangeStage()
		stage.CanonicalOrigin = origin
		if _, _, err := EncodeExchangeStage(stage); err != nil {
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
			stage := validExchangeStage()
			stage.CanonicalOrigin = origin
			if _, _, err := EncodeExchangeStage(stage); err == nil {
				t.Fatalf("noncanonical origin %q accepted", origin)
			}
		})
	}
}

func validExchangeStage() ExchangeStage {
	return ExchangeStage{StageCommon: StageCommon{
		Version: "v1", Kind: "exchange", AccountID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		DeviceID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", SessionID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
		LineageRootSessionID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", ServerInstanceID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		CanonicalOrigin: "https://sync.example.test", TokenKeyID: "session.v1", AccessDigest: strings.Repeat("a", 64),
		RefreshDigest: strings.Repeat("b", 64), RecoveryEpoch: 7, SecurityRevision: 42, Scope: "account",
		AccessExpiresAt: "2026-09-20T12:10:00Z", RefreshExpiresAt: "2026-10-20T12:00:00Z",
		ReauthenticateAt: "2026-10-20T12:00:00Z", AbsoluteExpiresAt: "2026-10-20T12:00:00Z",
	}, IdentityProvider: "apple", IdentityKeyID: "identity.v1", IdentityDigest: strings.Repeat("c", 64),
		IdentityAudience: "com.example", IdentitySubject: "000123", ProfileConfirmed: false,
		RegisterAccount: true, RegisterDevice: false}
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
