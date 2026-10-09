package liveactivity

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"babytracker/server/internal/config"
	"babytracker/server/internal/protocol"
)

const (
	APNsSandboxURL          = "https://api.sandbox.push.apple.com"
	APNsProductionURL       = "https://api.push.apple.com"
	maximumAPNsPayloadBytes = 4096
	apnsRequestTimeout      = 10 * time.Second
)

type PushClassification string

const (
	Accepted             PushClassification = "accepted"
	InvalidToken         PushClassification = "invalidToken"
	Retryable            PushClassification = "retryable"
	ConfigurationFailure PushClassification = "configurationFailure"
	Ambiguous            PushClassification = "ambiguous"
)

type TokenKind string

const (
	PushToStartToken TokenKind = "pushToStart"
	UpdateToken      TokenKind = "update"
)

type ActivityAttributes struct {
	BabyID   string        `json:"babyID"`
	BabyName string        `json:"babyName"`
	KindRaw  string        `json:"kindRaw"`
	Scope    ActivityScope `json:"scope"`
}

type PushRequest struct {
	Environment string
	Token       string
	TokenKind   TokenKind
	APNSID      string
	Event       string
	Timestamp   time.Time
	Expiration  time.Time
	Priority    int
	Attributes  ActivityAttributes
	Content     ContentState
	StaleAt     *time.Time
}

type PushResult struct {
	HTTPStatus     int
	Reason         string
	RetryAfter     time.Time
	Classification PushClassification
}

// ErrRequestNotSent marks a Send that failed before any byte reached APNs:
// local validation, payload encoding or provider-token signing. Nothing may
// have reached Apple, so a start rejected this way is known-unsent and can be
// retried without the start-unconfirmed ambiguity rules.
var ErrRequestNotSent = errors.New("live_activity_request_not_sent")

type Transport interface {
	// Send returns the attempt's classification. Its error wraps
	// ErrRequestNotSent only when the request was never written; once the
	// request may have reached Apple, failures are reported by class alone.
	Send(ctx context.Context, request PushRequest) (PushResult, error)
}

type APNSTransport struct {
	client                    *http.Client
	provider                  *providerToken
	bundleID                  string
	sandboxURL, productionURL string
}

func (transport *APNSTransport) CloseIdleConnections() { transport.client.CloseIdleConnections() }

// NewAPNSTransport fixes the destination to Apple's two documented hosts.
// Construct a new instance on configuration reload to refresh key material.
func NewAPNSTransport(configuration config.LiveActivityConfig) (*APNSTransport, error) {
	if !configuration.Enabled {
		return nil, errors.New("APNs transport is disabled")
	}
	key, err := config.ReadAPNSPrivateKey(configuration.KeyFile)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: apnsRequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{ForceAttemptHTTP2: true, MaxIdleConns: 32, MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 5 * time.Second}}
	return newAPNSTransport(configuration.TeamID, configuration.KeyID, configuration.BundleID, key, client, APNsSandboxURL, APNsProductionURL), nil
}

// Endpoint injection is private to the package for local HTTP/2 TLS tests.
func newAPNSTransport(teamID, keyID, bundleID string, key *ecdsa.PrivateKey, client *http.Client, sandboxURL, productionURL string) *APNSTransport {
	return &APNSTransport{client: client, provider: newProviderToken(teamID, keyID, key, time.Now), bundleID: bundleID, sandboxURL: sandboxURL, productionURL: productionURL}
}

var apnsTokenPattern = regexp.MustCompile(`^[0-9a-f]+$`)
var apnsReasons = map[string]bool{
	"BadDeviceToken": true, "DeviceTokenNotForTopic": true, "Unregistered": true,
	"BadTopic": true, "TopicDisallowed": true, "InvalidProviderToken": true, "ExpiredProviderToken": true,
	"Forbidden": true, "BadCertificate": true, "BadCertificateEnvironment": true,
	"BadExpirationDate": true, "BadPriority": true, "BadMessageId": true, "BadPath": true,
	"PayloadEmpty": true, "PayloadTooLarge": true, "BadCollapseId": true,
	"TooManyRequests": true, "TooManyProviderTokenUpdates": true, "InternalServerError": true, "ServiceUnavailable": true,
	"Shutdown": true, "IdleTimeout": true, "MethodNotAllowed": true, "MissingDeviceToken": true,
	"MissingTopic": true, "MissingProviderToken": true, "BadPushType": true,
}

func (t *APNSTransport) Send(ctx context.Context, request PushRequest) (PushResult, error) {
	// Everything up to client.Do happens before a byte is written: these
	// failures are known-unsent.
	notSent := func(err error) (PushResult, error) {
		return PushResult{Classification: ConfigurationFailure}, errors.Join(ErrRequestNotSent, err)
	}
	if err := validatePushRequest(request); err != nil {
		return notSent(err)
	}
	payload, err := encodePushPayload(request)
	if err != nil {
		return notSent(err)
	}
	token, err := t.provider.token()
	if err != nil {
		return notSent(err)
	}
	host := t.productionURL
	if request.Environment == "sandbox" {
		host = t.sandboxURL
	}
	bounded, cancel := context.WithTimeout(ctx, apnsRequestTimeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(bounded, http.MethodPost, host+"/3/device/"+request.Token, bytes.NewReader(payload))
	if err != nil {
		return notSent(errors.New("invalid APNs request"))
	}
	httpRequest.Header.Set("authorization", "bearer "+token)
	httpRequest.Header.Set("apns-push-type", "liveactivity")
	httpRequest.Header.Set("apns-topic", t.bundleID+".push-type.liveactivity")
	httpRequest.Header.Set("apns-id", request.APNSID)
	httpRequest.Header.Set("apns-priority", strconv.Itoa(request.Priority))
	expiration := ceilUnix(request.Expiration)
	if request.Event == "start" {
		expiration = 0
	}
	httpRequest.Header.Set("apns-expiration", strconv.FormatInt(expiration, 10))
	httpRequest.Header.Set("content-type", "application/json")
	response, err := t.client.Do(httpRequest)
	if err != nil {
		return PushResult{Classification: Ambiguous}, errors.New("APNs request outcome is unknown")
	}
	defer response.Body.Close()
	result := PushResult{HTTPStatus: response.StatusCode, Classification: classifyStatus(response.StatusCode), RetryAfter: parseRetryAfter(response.Header.Get("Retry-After"), time.Now())}
	if response.StatusCode != http.StatusOK {
		var body struct {
			Reason string `json:"reason"`
		}
		limited, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		if json.Unmarshal(limited, &body) == nil && apnsReasons[body.Reason] {
			result.Reason = body.Reason
		}
		switch result.Reason {
		case "BadDeviceToken", "Unregistered":
			// The token itself is dead, or belongs to the other APNs environment.
			result.Classification = InvalidToken
		case "DeviceTokenNotForTopic", "TopicDisallowed", "BadTopic":
			// A wrong bundle ID or key scope is this deployment's configuration:
			// it says nothing about any one phone's token.
			result.Classification = ConfigurationFailure
		}
	}
	return result, nil
}

func validatePushRequest(r PushRequest) error {
	invalid := errors.New("invalid live activity push request")
	if r.Environment != "sandbox" && r.Environment != "production" {
		return invalid
	}
	if len(r.Token) == 0 || len(r.Token) > 2048 || len(r.Token)%2 != 0 || !apnsTokenPattern.MatchString(r.Token) || !protocol.ValidUUID(r.APNSID) {
		return invalid
	}
	if r.Event != "start" && r.Event != "update" && r.Event != "end" {
		return invalid
	}
	if (r.Event == "start" && r.TokenKind != PushToStartToken) || (r.Event != "start" && r.TokenKind != UpdateToken) {
		return invalid
	}
	if (r.Priority != 5 && r.Priority != 10) || (r.Event != "update" && r.Priority != 10) {
		return invalid
	}
	if r.Timestamp.IsZero() || (r.Event != "start" && (r.Expiration.IsZero() || r.Expiration.Unix() < 0)) {
		return invalid
	}
	if r.Content.Remote == nil || r.Content.Validate() != nil {
		return invalid
	}
	if r.Event == "start" && (r.Attributes.BabyID == "" || r.Attributes.BabyName == "" || (r.Attributes.KindRaw != "feed" && r.Attributes.KindRaw != "sleep")) {
		return invalid
	}
	if r.Event == "start" && (r.Attributes.Scope != r.Content.Remote.Scope || r.Attributes.BabyID != r.Attributes.Scope.BabyID || r.Attributes.KindRaw != r.Attributes.Scope.KindRaw) {
		return invalid
	}
	return nil
}

func encodePushPayload(r PushRequest) ([]byte, error) {
	aps := map[string]any{"timestamp": r.Timestamp.Unix(), "event": r.Event, "content-state": r.Content}
	if r.StaleAt != nil {
		aps["stale-date"] = ceilUnix(*r.StaleAt)
	}
	if r.Event == "start" {
		titleKey := "LIVE_ACTIVITY_FEED_START_TITLE"
		if r.Attributes.KindRaw == "sleep" {
			titleKey = "LIVE_ACTIVITY_SLEEP_START_TITLE"
		}
		aps["alert"] = map[string]any{"title": map[string]any{"loc-key": titleKey}, "body": map[string]any{"loc-key": "LIVE_ACTIVITY_START_BODY", "loc-args": []string{r.Attributes.BabyName}}}
		aps["attributes-type"] = "TimerActivityAttributes"
		aps["attributes"] = r.Attributes
		aps["input-push-token"] = 1
	}
	if r.Event == "end" {
		aps["dismissal-date"] = r.Timestamp.Unix() - 1
	}
	payload, err := json.Marshal(map[string]any{"aps": aps})
	if err != nil || len(payload) > maximumAPNsPayloadBytes {
		return nil, errors.New("live activity APNs payload exceeds 4096 bytes or is invalid")
	}
	return payload, nil
}

func ceilUnix(t time.Time) int64 {
	seconds := t.Unix()
	if t.Nanosecond() > 0 {
		seconds++
	}
	return seconds
}

func classifyStatus(status int) PushClassification {
	switch {
	case status == http.StatusOK:
		return Accepted
	case status == http.StatusGone:
		return InvalidToken
	case status == http.StatusTooManyRequests || status >= 500:
		return Retryable
	case status >= 400 && status < 500:
		return ConfigurationFailure
	default:
		return Ambiguous
	}
}

func parseRetryAfter(value string, now time.Time) time.Time {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && seconds >= 0 && seconds <= 86400 {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date
	}
	return time.Time{}
}
