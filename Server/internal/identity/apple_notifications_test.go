package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// notificationAuthoritySpy records every authority call; the validation
// tests below require that a rejected or non-destructive notification makes
// none. The destructive paths run against the PostgreSQL authority in
// notifications_authority_test.go.
type notificationAuthoritySpy struct {
	mu    sync.Mutex
	calls []string
}

func (spy *notificationAuthoritySpy) record(call string) {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	spy.calls = append(spy.calls, call)
}

func (spy *notificationAuthoritySpy) callCount() int {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	return len(spy.calls)
}

func (spy *notificationAuthoritySpy) CreateChallenge(context.Context, [32]byte, time.Time, time.Time) error {
	spy.record("create_challenge")
	return ErrUnavailable
}

func (spy *notificationAuthoritySpy) ConsumeChallenge(context.Context, [32]byte, time.Time) error {
	spy.record("consume_challenge")
	return ErrUnavailable
}

func (spy *notificationAuthoritySpy) ResolveIdentity(context.Context, []string) (IdentityAuthority, error) {
	spy.record("resolve_identity")
	return IdentityAuthority{Status: "absent", NextActorGeneration: 1}, nil
}

func (spy *notificationAuthoritySpy) ReserveAppleNotification(context.Context, []KeyedDigest, string, time.Time) (string, bool, error) {
	spy.record("reserve_notification")
	return "", false, ErrUnavailable
}

func (spy *notificationAuthoritySpy) NotificationReceipt(context.Context, string, []string, string) (bool, error) {
	spy.record("notification_receipt")
	return false, ErrUnavailable
}

func (spy *notificationAuthoritySpy) MarkIdentityUnavailable(context.Context, IdentityUnavailableCommand) error {
	spy.record("mark_identity_unavailable")
	return ErrUnavailable
}

var _ Authority = (*notificationAuthoritySpy)(nil)

func TestNotificationCommandIDKnownAnswer(t *testing.T) {
	key := DigestKey{ID: "notification.v1"}
	for index := range key.Secret {
		key.Secret[index] = 1
	}
	commandID, digest := NotificationCommandID(key, appleIssuer, testAppleAudience, "notification-jti-1")
	if commandID != "e871d351-93d8-8913-b112-5e2b8eb031e9" {
		t.Fatalf("command ID=%q", commandID)
	}
	if digest.KeyID != key.ID || fmt.Sprintf("%x", digest.Sum) != "e871d35193d8d913f1125e2b8eb031e94fb64ceb94199dde01d60997b40d7670" {
		t.Fatalf("digest=%s:%x", digest.KeyID, digest.Sum)
	}
}

func TestAppleNotificationEmailEventValidatesWithoutPersistence(t *testing.T) {
	fixture := newNotificationFixture(t)
	spy := &notificationAuthoritySpy{}
	service := fixture.service(t, spy)
	body := fixture.body(t, `{"alg":"RS256","kid":"apple-key"}`, fixture.claims(
		`{"event_time":1790000000,"sub":"apple-subject","type":"email-disabled","email":"parent@example.test","is_private_email":"false"}`,
	))
	if err := service.Handle(t.Context(), body); err != nil {
		t.Fatal(err)
	}
	if spy.callCount() != 0 {
		t.Fatalf("email notification reached the authority: %v", spy.calls)
	}
}

func TestAppleNotificationAcceptsNoExpiryAndInclusiveAgeBounds(t *testing.T) {
	fixture := newNotificationFixture(t)
	service := fixture.service(t, &notificationAuthoritySpy{})
	for _, offset := range []time.Duration{-24 * time.Hour, time.Minute} {
		t.Run(offset.String(), func(t *testing.T) {
			at := fixture.now.Add(offset).Unix()
			event := fmt.Sprintf(`{"event_time":%d,"sub":"apple-subject","type":"email-enabled"}`, at)
			claims := fmt.Sprintf(`{"iss":%q,"aud":%q,"iat":%d,"jti":"boundary-%d","events":%s}`,
				appleIssuer, testAppleAudience, at, at, event)
			if strings.Contains(claims, `"exp"`) {
				t.Fatal("fixture unexpectedly contains exp")
			}
			if err := service.Handle(t.Context(), fixture.body(t, `{"alg":"RS256","kid":"apple-key"}`, claims)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAppleNotificationStrictValidation(t *testing.T) {
	fixture := newNotificationFixture(t)
	validHeader := `{"alg":"RS256","kid":"apple-key"}`
	validEvent := `{"event_time":1790000000,"sub":"apple-subject","type":"email-enabled","email":"parent@example.test","is_private_email":"true"}`
	validClaims := fixture.claims(validEvent)
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		body func() string
	}{
		{name: "outer duplicate", body: func() string {
			token := signJWT(t, fixture.apple.key, validHeader, validClaims)
			return `{"payload":` + quoted(token) + `,"\u0070ayload":` + quoted(token) + `}`
		}},
		{name: "outer unknown", body: func() string {
			token := signJWT(t, fixture.apple.key, validHeader, validClaims)
			return `{"payload":` + quoted(token) + `,"extra":true}`
		}},
		{name: "outer payload not string", body: func() string { return `{"payload":{}}` }},
		{name: "oversized body", body: func() string { return strings.Repeat("x", maxAppleNotificationBodyBytes+1) }},
		{name: "wrong algorithm", body: func() string { return fixture.body(t, `{"alg":"ES256","kid":"apple-key"}`, validClaims) }},
		{name: "unknown header", body: func() string { return fixture.body(t, `{"alg":"RS256","kid":"apple-key","typ":"JWT"}`, validClaims) }},
		{name: "duplicate header", body: func() string {
			return fixture.body(t, `{"alg":"RS256","\u0061lg":"RS256","kid":"apple-key"}`, validClaims)
		}},
		{name: "wrong signature", body: func() string {
			return notificationEnvelope(signJWT(t, otherKey, validHeader, validClaims))
		}},
		{name: "wrong issuer", body: func() string {
			return fixture.body(t, validHeader, strings.Replace(validClaims, appleIssuer, "https://attacker.test", 1))
		}},
		{name: "wrong audience", body: func() string {
			return fixture.body(t, validHeader, strings.Replace(validClaims, testAppleAudience, "other.audience", 1))
		}},
		{name: "expiry forbidden", body: func() string {
			return fixture.body(t, validHeader, strings.TrimSuffix(validClaims, "}")+`,"exp":1790000060}`)
		}},
		{name: "unknown claim", body: func() string {
			return fixture.body(t, validHeader, strings.TrimSuffix(validClaims, "}")+`,"nonce":"x"}`)
		}},
		{name: "duplicate claim", body: func() string {
			return fixture.body(t, validHeader, strings.TrimSuffix(validClaims, "}")+`,"\u006aud":"other"}`)
		}},
		{name: "events string", body: func() string {
			return fixture.body(t, validHeader, fixture.claims(`"not-an-object"`))
		}},
		{name: "unknown event field", body: func() string {
			return fixture.body(t, validHeader, fixture.claims(strings.TrimSuffix(validEvent, "}")+`,"extra":true}`))
		}},
		{name: "duplicate event field", body: func() string {
			return fixture.body(t, validHeader, fixture.claims(strings.TrimSuffix(validEvent, "}")+`,"\u0073ub":"other"}`))
		}},
		{name: "fractional issued at", body: func() string {
			return fixture.body(t, validHeader, strings.Replace(validClaims, `"iat":1790000000`, `"iat":1790000000.5`, 1))
		}},
		{name: "fractional event time", body: func() string {
			return fixture.body(t, validHeader, fixture.claims(strings.Replace(validEvent, `"event_time":1790000000`, `"event_time":1790000000.5`, 1)))
		}},
		{name: "issued too old", body: func() string {
			return fixture.body(t, validHeader, strings.Replace(validClaims, `"iat":1790000000`, `"iat":1789913599`, 1))
		}},
		{name: "event too far future", body: func() string {
			return fixture.body(t, validHeader, fixture.claims(strings.Replace(validEvent, `"event_time":1790000000`, `"event_time":1790000061`, 1)))
		}},
		{name: "control jti", body: func() string {
			return fixture.body(t, validHeader, strings.Replace(validClaims, `"jti":"notification-jti-1"`, `"jti":"bad\u0000jti"`, 1))
		}},
		{name: "long subject", body: func() string {
			return fixture.body(t, validHeader, fixture.claims(strings.Replace(validEvent, `"sub":"apple-subject"`, `"sub":"`+strings.Repeat("s", 256)+`"`, 1)))
		}},
		{name: "email field on destructive event", body: func() string {
			event := strings.Replace(validEvent, `"type":"email-enabled"`, `"type":"consent-revoked"`, 1)
			return fixture.body(t, validHeader, fixture.claims(event))
		}},
		{name: "boolean private email", body: func() string {
			return fixture.body(t, validHeader, fixture.claims(strings.Replace(validEvent, `"is_private_email":"true"`, `"is_private_email":true`, 1)))
		}},
		{name: "unknown type", body: func() string {
			return fixture.body(t, validHeader, fixture.claims(strings.Replace(validEvent, `"type":"email-enabled"`, `"type":"profile-updated"`, 1)))
		}},
	}
	spy := &notificationAuthoritySpy{}
	service := fixture.service(t, spy)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := service.Handle(t.Context(), test.body()); Code(err) != "invalid_apple_proof" {
				t.Fatalf("code=%q err=%v", Code(err), err)
			}
		})
	}
	if spy.callCount() != 0 {
		t.Fatalf("rejected notifications reached the authority: %v", spy.calls)
	}
}

type notificationFixture struct {
	apple            *appleVerifierFixture
	now              time.Time
	identityKeys     DigestKeyring
	notificationKeys DigestKeyring
}

func newNotificationFixture(t *testing.T) *notificationFixture {
	t.Helper()
	apple := newAppleVerifierFixture(t)
	apple.now = time.Unix(1790000000, 0).UTC()
	return &notificationFixture{apple: apple, now: apple.now, identityKeys: testKeyring(), notificationKeys: notificationKeyring()}
}

func (fixture *notificationFixture) service(t *testing.T, authority Authority) *NotificationService {
	t.Helper()
	service, err := NewNotificationService(NotificationOptions{
		Audience: testAppleAudience, InstanceID: "50000000-0000-4000-8000-000000000001", HTTP: fixture.apple.client,
		IdentityKeys: fixture.identityKeys, NotificationKeys: fixture.notificationKeys, Authority: authority,
		Now: func() time.Time { return fixture.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func (fixture *notificationFixture) destructiveBody(t *testing.T, eventType string) string {
	return fixture.destructiveBodyWithJTI(t, eventType, "notification-jti-1")
}

func (fixture *notificationFixture) destructiveBodyWithJTI(t *testing.T, eventType, jti string) string {
	t.Helper()
	event := fmt.Sprintf(`{"event_time":%d,"sub":"apple-subject","type":%q}`, fixture.now.Unix(), eventType)
	claims := fmt.Sprintf(`{"iss":%q,"aud":%q,"iat":%d,"jti":%q,"events":%s}`,
		appleIssuer, testAppleAudience, fixture.now.Unix(), jti, event)
	return fixture.body(t, `{"alg":"RS256","kid":"apple-key"}`, claims)
}

func (fixture *notificationFixture) claims(event string) string {
	return fmt.Sprintf(`{"iss":%q,"aud":%q,"iat":%d,"jti":"notification-jti-1","events":%s}`,
		appleIssuer, testAppleAudience, fixture.now.Unix(), event)
}

func (fixture *notificationFixture) body(t *testing.T, header, claims string) string {
	t.Helper()
	return notificationEnvelope(signJWT(t, fixture.apple.key, header, claims))
}

func notificationEnvelope(token string) string { return `{"payload":` + quoted(token) + `}` }

func quoted(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func notificationKeyring() DigestKeyring {
	var current [32]byte
	for index := range current {
		current[index] = 0x44
	}
	return DigestKeyring{Current: DigestKey{ID: "notification.current", Secret: current}}
}
