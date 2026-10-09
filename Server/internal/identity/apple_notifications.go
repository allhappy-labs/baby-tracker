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

	"babytracker/server/internal/security"
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
	repository                     NotificationRepository
	security                       *security.Service
	now                            func() time.Time
}

type AuthIdentityLookup struct {
	AccountID                    string
	AuthorityStatus, DisplayName string
	ProfileRequired              bool
}

type NotificationRepository interface {
	LookupAuthIdentity(context.Context, string, string, [32]byte) (AuthIdentityLookup, error)
	ReserveAppleNotification(context.Context, []KeyedDigest, string, time.Time) (string, bool, error)
}

type NotificationOptions struct {
	Audience, InstanceID           string
	HTTP                           *http.Client
	IdentityKeys, NotificationKeys DigestKeyring
	Repository                     NotificationRepository
	Security                       *security.Service
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
		!validateKeyring(options.IdentityKeys) || !validateKeyring(options.NotificationKeys) || options.Repository == nil || options.Security == nil || options.Now == nil {
		return nil, errors.New("complete Apple notification options are required")
	}
	return &NotificationService{
		keys: newJWKSCache(options.HTTP, options.Now), audience: options.Audience, instanceID: options.InstanceID,
		identityKeys: options.IdentityKeys, notificationKeys: options.NotificationKeys, repository: options.Repository,
		security: options.Security, now: options.Now,
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

func (service *NotificationService) handleDestructive(ctx context.Context, claims appleNotificationClaims) error {
	if service.repository == nil || service.security == nil {
		return ErrUnavailable
	}
	authorityCandidates := make([]string, 0, 1+len(service.identityKeys.Previous))
	var lookup AuthIdentityLookup
	matches := 0
	for _, key := range append([]DigestKey{service.identityKeys.Current}, service.identityKeys.Previous...) {
		candidate := identityDigest(key, "apple", claims.Audience, claims.Event.Subject)
		authorityCandidates = append(authorityCandidates, authorityDigest(candidate))
		row, err := service.repository.LookupAuthIdentity(ctx, "apple", candidate.KeyID, candidate.Sum)
		if err != nil {
			return ErrUnavailable
		}
		if row.AccountID == "" {
			if row.AuthorityStatus != "" || row.DisplayName != "" || row.ProfileRequired {
				return ErrUnavailable
			}
			continue
		}
		if !validNonzeroUUID(row.AccountID) || (row.AuthorityStatus != "active" && row.AuthorityStatus != "identity_unavailable" && row.AuthorityStatus != "deleted") {
			return ErrUnavailable
		}
		matches++
		if matches == 1 {
			lookup = row
		}
	}
	if matches > 1 {
		return ErrUnavailable
	}
	accountID, found, err := service.authoritativeNotificationAccount(ctx, authorityCandidates, lookup.AccountID)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}

	keys := append([]DigestKey{service.notificationKeys.Current}, service.notificationKeys.Previous...)
	commandIDs := make([]string, 0, len(keys))
	digests := make([]KeyedDigest, 0, len(keys))
	for _, key := range keys {
		commandID, digest := NotificationCommandID(key, claims.Issuer, claims.Audience, claims.JTI)
		commandIDs = append(commandIDs, commandID)
		digests = append(digests, digest)
	}
	semanticDigest := notificationSemanticDigest(claims)
	receipted, err := service.checkNotificationReceipts(ctx, accountID, authorityCandidates, commandIDs, semanticDigest)
	if err != nil {
		return err
	}
	if receipted {
		return nil
	}

	storedCommandID, _, err := service.repository.ReserveAppleNotification(ctx, digests, commandIDs[0], service.now().UTC())
	if err != nil || !slices.Contains(commandIDs, storedCommandID) {
		return ErrUnavailable
	}
	receipted, err = service.checkNotificationReceipts(ctx, accountID, authorityCandidates, []string{storedCommandID}, semanticDigest)
	if err != nil {
		return err
	}
	if receipted {
		return nil
	}
	_, err = service.security.ExecutePrepared(ctx, service.prepareIdentityUnavailable(accountID, authorityCandidates, storedCommandID, semanticDigest))
	if err == nil {
		return nil
	}
	settled, reconcileErr := service.checkNotificationReceipts(ctx, accountID, authorityCandidates, []string{storedCommandID}, semanticDigest)
	if reconcileErr != nil {
		return reconcileErr
	}
	if settled {
		return nil
	}
	return err
}

func (service *NotificationService) authoritativeNotificationAccount(ctx context.Context, identityCandidates []string, projectedAccountID string) (string, bool, error) {
	var accountID string
	err := service.security.WithAuthoritySnapshot(ctx, func(snapshot security.AuthoritySnapshot) error {
		resolution, err := snapshot.View.ResolveIdentityAuthority(identityCandidates)
		if err != nil {
			return err
		}
		switch resolution.Status {
		case "absent":
			if projectedAccountID != "" {
				return ErrUnavailable
			}
			return nil
		case "deleted":
			if projectedAccountID != "" {
				return ErrUnavailable
			}
			return nil
		case "active", "identity_unavailable":
			if projectedAccountID != "" && projectedAccountID != resolution.AccountID {
				return ErrUnavailable
			}
			accountID = resolution.AccountID
			return nil
		default:
			return ErrUnavailable
		}
	})
	return accountID, accountID != "", err
}

func (service *NotificationService) checkNotificationReceipts(ctx context.Context, accountID string, identityCandidates, commandIDs []string, semanticDigest string) (bool, error) {
	receipted := false
	err := service.security.WithAuthoritySnapshot(ctx, func(snapshot security.AuthoritySnapshot) error {
		resolution, err := snapshot.View.ResolveIdentityAuthority(identityCandidates)
		if err != nil || resolution.AccountID != accountID || resolution.Status == "absent" || resolution.Status == "deleted" {
			return ErrUnavailable
		}
		actor, err := snapshot.ResolveIdentityActor(identityCandidates, accountID, resolution.LifecycleGeneration)
		if err != nil {
			return err
		}
		for _, commandID := range commandIDs {
			if _, exists := snapshot.View.Receipt(accountID, commandID); !exists {
				continue
			}
			prepared, err := snapshot.PrepareIdentityUnavailable(actor, security.IdentityUnavailableCommandInput{
				Meta: security.CommandMeta{CommandID: commandID, At: service.now().UTC()},
				Payload: security.IdentityAuthorityPayload{
					Version: "v1", AccountID: accountID, IdentityKey: resolution.IdentityKey, NotificationDigest: semanticDigest,
				},
			})
			if err != nil {
				return err
			}
			if _, err := snapshot.CommandHash(prepared); err != nil {
				return err
			}
			receipted = true
		}
		if !receipted && resolution.Status == "identity_unavailable" {
			receipted = true
		} else if !receipted && resolution.Status != "active" {
			return ErrUnavailable
		}
		return nil
	})
	return receipted, err
}

func (service *NotificationService) prepareIdentityUnavailable(accountID string, identityCandidates []string, commandID, semanticDigest string) security.PrepareCommand {
	return func(_ context.Context, snapshot security.AuthoritySnapshot) (security.PreparedCommand, error) {
		resolution, err := snapshot.View.ResolveIdentityAuthority(identityCandidates)
		if err != nil || resolution.Status == "absent" || resolution.Status == "deleted" || resolution.AccountID != accountID {
			return security.PreparedCommand{}, ErrUnavailable
		}
		actor, err := snapshot.ResolveIdentityActor(identityCandidates, accountID, resolution.LifecycleGeneration)
		if err != nil {
			return security.PreparedCommand{}, err
		}
		return snapshot.PrepareIdentityUnavailable(actor, security.IdentityUnavailableCommandInput{
			Meta: security.CommandMeta{CommandID: commandID, At: service.now().UTC()},
			Payload: security.IdentityAuthorityPayload{
				Version: "v1", AccountID: accountID, IdentityKey: resolution.IdentityKey, NotificationDigest: semanticDigest,
			},
		})
	}
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
