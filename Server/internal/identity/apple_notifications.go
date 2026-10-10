package identity

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxAppleNotificationBodyBytes = 64 << 10
	maxAppleNotificationTextBytes = 255
	maxAppleNotificationEmail     = 320
	appleNotificationPastAge      = 24 * time.Hour
	appleNotificationFutureSkew   = time.Minute
)

type NotificationService struct {
	keys                           *JWKSCache
	audience, instanceID           string
	identityKeys, notificationKeys DigestKeyring
	authority                      Authority
	now                            func() time.Time
}

type NotificationOptions struct {
	Audience, InstanceID           string
	HTTP                           *http.Client
	IdentityKeys, NotificationKeys DigestKeyring
	Authority                      Authority
	Now                            func() time.Time
}

type appleNotificationClaims struct {
	Issuer, Audience, JTI string
	IssuedAt              int64
	Event                 appleNotificationEvent
}

type appleNotificationEvent struct {
	EventTime     int64
	Subject, Type string
	Email         *string
	PrivateEmail  *string
}

func NewNotificationService(options NotificationOptions) (*NotificationService, error) {
	if !validBoundedText(options.Audience, maxAppleNotificationTextBytes) || !validNonzeroUUID(options.InstanceID) ||
		!validateKeyring(options.IdentityKeys) || !validateKeyring(options.NotificationKeys) || options.Authority == nil || options.Now == nil {
		return nil, errors.New("complete Apple notification options are required")
	}
	return &NotificationService{
		keys: newJWKSCache(options.HTTP, options.Now), audience: options.Audience, instanceID: options.InstanceID,
		identityKeys: options.IdentityKeys, notificationKeys: options.NotificationKeys, authority: options.Authority, now: options.Now,
	}, nil
}

func NotificationCommandID(key DigestKey, issuer, audience, jti string) (string, KeyedDigest) {
	digest := hmac.New(sha256.New, key.Secret[:])
	digest.Write([]byte("baby-tracker-apple-notification-v1\x00"))
	digest.Write([]byte(issuer))
	digest.Write([]byte{0})
	digest.Write([]byte(audience))
	digest.Write([]byte{0})
	digest.Write([]byte(jti))
	var sum [32]byte
	copy(sum[:], digest.Sum(nil))
	bytes := sum
	bytes[6] = (bytes[6] & 0x0f) | 0x80
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	commandID := fmt.Sprintf("%x-%x-%x-%x-%x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
	return commandID, KeyedDigest{KeyID: key.ID, Sum: sum}
}

func (service *NotificationService) Handle(ctx context.Context, body string) error {
	claims, err := service.verify(ctx, body)
	if err != nil {
		return err
	}
	switch claims.Event.Type {
	case "email-enabled", "email-disabled":
		return nil
	case "consent-revoked", "account-deleted":
		return service.handleDestructive(ctx, claims)
	default:
		return ErrInvalidAppleProof
	}
}

func (service *NotificationService) verify(ctx context.Context, body string) (appleNotificationClaims, error) {
	if service == nil || service.keys == nil || service.now == nil || len(body) == 0 || len(body) > maxAppleNotificationBodyBytes || !utf8.ValidString(body) {
		return appleNotificationClaims{}, ErrInvalidAppleProof
	}
	outer, err := strictJSONObject([]byte(body), maxJWTJSONDepth, maxJWTJSONKeys)
	if err != nil || !exactObjectKeys(outer, "payload") {
		return appleNotificationClaims{}, ErrInvalidAppleProof
	}
	compact, err := exactJSONString(outer, "payload")
	if err != nil || len(compact) == 0 || len(compact) > maxAppleNotificationBodyBytes {
		return appleNotificationClaims{}, ErrInvalidAppleProof
	}
	segments := strings.Split(compact, ".")
	if len(segments) != 3 {
		return appleNotificationClaims{}, ErrInvalidAppleProof
	}
	headerBytes, err := decodeCanonicalBase64URL(segments[0])
	if err != nil {
		return appleNotificationClaims{}, ErrInvalidAppleProof
	}
	claimsBytes, err := decodeCanonicalBase64URL(segments[1])
	if err != nil {
		return appleNotificationClaims{}, ErrInvalidAppleProof
	}
	signature, err := decodeCanonicalBase64URL(segments[2])
	if err != nil || len(signature) == 0 {
		return appleNotificationClaims{}, ErrInvalidAppleProof
	}
	header, err := strictJSONObject(headerBytes, maxJWTJSONDepth, maxJWTJSONKeys)
	if err != nil || !exactObjectKeys(header, "alg", "kid") {
		return appleNotificationClaims{}, ErrInvalidAppleProof
	}
	algorithm, err := exactJSONString(header, "alg")
	if err != nil || algorithm != "RS256" {
		return appleNotificationClaims{}, ErrInvalidAppleProof
	}
	kid, err := exactJSONString(header, "kid")
	if err != nil || !validASCIIIdentifier(kid, 128) {
		return appleNotificationClaims{}, ErrInvalidAppleProof
	}
	parsed, err := decodeAppleNotificationClaims(claimsBytes, service.audience, service.now().UTC())
	if err != nil {
		return appleNotificationClaims{}, ErrInvalidAppleProof
	}
	key, err := service.keys.Key(ctx, kid)
	if err != nil {
		return appleNotificationClaims{}, ErrInvalidAppleProof
	}
	digest := sha256.Sum256([]byte(segments[0] + "." + segments[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return appleNotificationClaims{}, ErrInvalidAppleProof
	}
	return parsed, nil
}

func decodeAppleNotificationClaims(encoded []byte, audience string, now time.Time) (appleNotificationClaims, error) {
	object, err := strictJSONObject(encoded, maxJWTJSONDepth, maxJWTJSONKeys)
	if err != nil || !exactObjectKeys(object, "iss", "aud", "iat", "jti", "events") {
		return appleNotificationClaims{}, errors.New("invalid notification claims")
	}
	issuer, err := exactJSONString(object, "iss")
	if err != nil || issuer != appleIssuer {
		return appleNotificationClaims{}, errors.New("invalid notification issuer")
	}
	gotAudience, err := exactJSONString(object, "aud")
	if err != nil || gotAudience != audience {
		return appleNotificationClaims{}, errors.New("invalid notification audience")
	}
	issuedAt, err := exactIntegralNumericDate(object, "iat")
	if err != nil || !notificationTimeAccepted(issuedAt, now) {
		return appleNotificationClaims{}, errors.New("invalid notification issue time")
	}
	jti, err := exactJSONString(object, "jti")
	if err != nil || !validNotificationText(jti, maxAppleNotificationTextBytes) {
		return appleNotificationClaims{}, errors.New("invalid notification jti")
	}
	eventObject, err := strictJSONObject(object["events"], maxJWTJSONDepth, maxJWTJSONKeys)
	if err != nil {
		return appleNotificationClaims{}, errors.New("invalid notification event")
	}
	event, err := decodeAppleNotificationEvent(eventObject, now)
	if err != nil {
		return appleNotificationClaims{}, err
	}
	return appleNotificationClaims{Issuer: issuer, Audience: gotAudience, JTI: jti, IssuedAt: issuedAt, Event: event}, nil
}

func decodeAppleNotificationEvent(object map[string]json.RawMessage, now time.Time) (appleNotificationEvent, error) {
	allowedEmail := false
	eventType, err := exactJSONString(object, "type")
	if err != nil {
		return appleNotificationEvent{}, errors.New("invalid notification event type")
	}
	switch eventType {
	case "email-enabled", "email-disabled":
		allowedEmail = true
	case "consent-revoked", "account-deleted":
	default:
		return appleNotificationEvent{}, errors.New("unknown notification event type")
	}
	allowed := []string{"event_time", "sub", "type"}
	if allowedEmail {
		allowed = append(allowed, "email", "is_private_email")
	}
	if !exactObjectKeysOptional(object, []string{"event_time", "sub", "type"}, allowed) {
		return appleNotificationEvent{}, errors.New("invalid notification event fields")
	}
	eventTime, err := exactIntegralNumericDate(object, "event_time")
	if err != nil || !notificationTimeAccepted(eventTime, now) {
		return appleNotificationEvent{}, errors.New("invalid notification event time")
	}
	subject, err := exactJSONString(object, "sub")
	if err != nil || !validNotificationText(subject, maxAppleNotificationTextBytes) {
		return appleNotificationEvent{}, errors.New("invalid notification subject")
	}
	event := appleNotificationEvent{EventTime: eventTime, Subject: subject, Type: eventType}
	if raw, present := object["email"]; present {
		value, err := exactJSONString(map[string]json.RawMessage{"email": raw}, "email")
		if err != nil || len(value) == 0 || len(value) > maxAppleNotificationEmail || !utf8.ValidString(value) {
			return appleNotificationEvent{}, errors.New("invalid notification email")
		}
		event.Email = &value
	}
	if raw, present := object["is_private_email"]; present {
		value, err := exactJSONString(map[string]json.RawMessage{"is_private_email": raw}, "is_private_email")
		if err != nil || (value != "true" && value != "false") {
			return appleNotificationEvent{}, errors.New("invalid notification private email")
		}
		event.PrivateEmail = &value
	}
	return event, nil
}

// handleDestructive marks the notified identity's account unavailable
// (consent-revoked, account-deleted). Order: resolve the identity (an absent
// or deleted one needs nothing); an exact retry's receipt under any
// notification key's command ID settles it before any reservation (a
// receipt for different content under the same JTI is command_id_reused);
// an already unavailable identity needs nothing; otherwise reserve the
// notification's command ID and mark the identity unavailable under it. A
// concurrent notification that marked it first settles the loser.
func (service *NotificationService) handleDestructive(ctx context.Context, claims appleNotificationClaims) error {
	if service.authority == nil {
		return ErrUnavailable
	}
	candidates := make([]string, 0, 1+len(service.identityKeys.Previous))
	for _, key := range append([]DigestKey{service.identityKeys.Current}, service.identityKeys.Previous...) {
		candidates = append(candidates, authorityDigest(identityDigest(key, "apple", claims.Audience, claims.Event.Subject)))
	}
	resolution, err := service.authority.ResolveIdentity(ctx, candidates)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if !resolution.Live() {
		return nil
	}
	accountID := resolution.AccountID

	keys := append([]DigestKey{service.notificationKeys.Current}, service.notificationKeys.Previous...)
	commandIDs := make([]string, 0, len(keys))
	digests := make([]KeyedDigest, 0, len(keys))
	for _, key := range keys {
		commandID, digest := NotificationCommandID(key, claims.Issuer, claims.Audience, claims.JTI)
		commandIDs = append(commandIDs, commandID)
		digests = append(digests, digest)
	}
	semanticDigest := notificationSemanticDigest(claims)
	receipted, err := service.authority.NotificationReceipt(ctx, accountID, commandIDs, semanticDigest)
	if err != nil || receipted {
		return err
	}
	if resolution.Status == "identity_unavailable" {
		return nil
	}
	storedCommandID, _, err := service.authority.ReserveAppleNotification(ctx, digests, commandIDs[0], service.now().UTC())
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if !slices.Contains(commandIDs, storedCommandID) {
		return ErrUnavailable
	}
	err = service.authority.MarkIdentityUnavailable(ctx, IdentityUnavailableCommand{CommandID: storedCommandID, AccountID: accountID,
		Candidates: candidates, NotificationDigest: semanticDigest, At: service.now().UTC()})
	if err == nil {
		return nil
	}
	settled, settleErr := service.authority.ResolveIdentity(ctx, candidates)
	if settleErr == nil && settled.AccountID == accountID && settled.Status == "identity_unavailable" {
		return nil
	}
	return err
}

func notificationTimeAccepted(seconds int64, now time.Time) bool {
	value := time.Unix(seconds, 0).UTC()
	return !value.Before(now.Add(-appleNotificationPastAge)) && !value.After(now.Add(appleNotificationFutureSkew))
}

func validNotificationText(value string, maxBytes int) bool {
	if len(value) == 0 || len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character == 0 || unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func exactObjectKeys(object map[string]json.RawMessage, names ...string) bool {
	if len(object) != len(names) {
		return false
	}
	for _, name := range names {
		if _, present := object[name]; !present {
			return false
		}
	}
	return true
}

func exactObjectKeysOptional(object map[string]json.RawMessage, required, allowed []string) bool {
	for _, name := range required {
		if _, present := object[name]; !present {
			return false
		}
	}
	for name := range object {
		if !slices.Contains(allowed, name) {
			return false
		}
	}
	return true
}

func notificationSemanticDigest(claims appleNotificationClaims) string {
	preimage := struct {
		Version        string  `json:"version"`
		Issuer         string  `json:"issuer"`
		Audience       string  `json:"audience"`
		IssuedAt       int64   `json:"issued_at"`
		JTI            string  `json:"jti"`
		EventTime      int64   `json:"event_time"`
		Subject        string  `json:"subject"`
		Type           string  `json:"type"`
		Email          *string `json:"email"`
		IsPrivateEmail *string `json:"is_private_email"`
	}{"v1", claims.Issuer, claims.Audience, claims.IssuedAt, claims.JTI, claims.Event.EventTime, claims.Event.Subject, claims.Event.Type, claims.Event.Email, claims.Event.PrivateEmail}
	encoded, _ := json.Marshal(preimage)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
