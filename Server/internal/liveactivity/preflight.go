package liveactivity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"
)

// PreflightOutcome is the operator-facing verdict of one preflight request.
type PreflightOutcome string

const (
	// PreflightPass: 400 BadDeviceToken. Apple validated the provider token
	// (key, team ID, key ID) and rejected the dummy token. Apple does not
	// document whether a bad token is rejected before or after the topic is
	// checked, so this only shows the topic was not contradicted; a wrong
	// bundle ID is proven or refuted by the first real send.
	PreflightPass PreflightOutcome = "pass"
	// PreflightCredentials: 403. The key, team ID or key ID is wrong.
	PreflightCredentials PreflightOutcome = "credentials"
	// PreflightTopic: 400 TopicDisallowed, BadTopic or DeviceTokenNotForTopic.
	// The bundle ID (topic) is wrong for this key.
	PreflightTopic PreflightOutcome = "topic"
	// PreflightRetryLater: 429. Apple is rate limiting; nothing is decided.
	PreflightRetryLater PreflightOutcome = "retryLater"
	// PreflightConnectivity: no answer (DNS, TCP, TLS or the request bound).
	PreflightConnectivity PreflightOutcome = "connectivity"
	// PreflightUnknown: any other answer, reported with status and reason.
	PreflightUnknown PreflightOutcome = "unknown"
)

type PreflightResult struct {
	Environment string
	Outcome     PreflightOutcome
	HTTPStatus  int
	Reason      string
	RetryAfter  time.Time
}

// A syntactically valid device token no device holds. Apple answers it with
// BadDeviceToken once the provider token is acceptable; whether the topic is
// checked before that answer is undocumented (see PreflightPass).
var preflightToken = strings.Repeat("0", 64)

// Fixed, obviously synthetic identifiers for the dummy payload.
const preflightID = "0f1e2d3c-4b5a-4968-8778-695a4b3c2d1e"

// Preflight sends exactly one quiet update for the dummy token through the
// production path (provider JWT, Live Activity topic and push type, HTTP/2 to
// the environment's fixed Apple host) and turns Apple's answer into a verdict.
// It never returns secrets: an error here is a local request-building failure.
func (t *APNSTransport) Preflight(ctx context.Context, environment string) (PreflightResult, error) {
	result := PreflightResult{Environment: environment, Outcome: PreflightUnknown}
	if environment != "sandbox" && environment != "production" {
		return result, errors.New("APNs environment must be sandbox or production")
	}
	request, err := preflightRequest(environment, time.Now())
	if err != nil {
		return result, err
	}
	sent, err := t.Send(ctx, request)
	if err != nil {
		if errors.Is(err, ErrRequestNotSent) {
			// Unwrap to the local cause (key invalid, payload invalid): no byte
			// reached Apple and the message carries no secret.
			var joined interface{ Unwrap() []error }
			if errors.As(err, &joined) {
				for _, cause := range joined.Unwrap() {
					if !errors.Is(cause, ErrRequestNotSent) {
						return result, cause
					}
				}
			}
			return result, err
		}
		result.Outcome = PreflightConnectivity
		return result, nil
	}
	result.HTTPStatus, result.Reason, result.RetryAfter = sent.HTTPStatus, sent.Reason, sent.RetryAfter
	result.Outcome = classifyPreflight(sent)
	return result, nil
}

func classifyPreflight(sent PushResult) PreflightOutcome {
	switch {
	case sent.HTTPStatus == http.StatusBadRequest && sent.Reason == "BadDeviceToken":
		return PreflightPass
	case sent.HTTPStatus == http.StatusForbidden:
		return PreflightCredentials
	case sent.HTTPStatus == http.StatusBadRequest && (sent.Reason == "TopicDisallowed" || sent.Reason == "BadTopic" || sent.Reason == "DeviceTokenNotForTopic"):
		return PreflightTopic
	case sent.HTTPStatus == http.StatusTooManyRequests:
		return PreflightRetryLater
	default:
		return PreflightUnknown
	}
}

// preflightRequest is a quiet (priority 5) update with a minimal valid content
// state: no alert, no attributes, no care content. Timestamps are the current
// second so Apple does not reject the request for its age.
func preflightRequest(environment string, now time.Time) (PushRequest, error) {
	apnsID, err := randomUUID()
	if err != nil {
		return PushRequest{}, errors.New("APNs preflight identifier unavailable")
	}
	now = now.UTC().Truncate(time.Second)
	scope := ActivityScope{ServerInstanceID: preflightID, RecoveryEpoch: 1, FamilyID: preflightID, BabyID: preflightID, KindRaw: "feed"}
	content := ContentState{StartedAt: float64(now.Unix() - 978307200), ActivityKind: "paused", Phase: "running", Parts: []PartLabel{},
		Remote: &Revision{Scope: scope, GenerationID: preflightID, PresentationVersion: 1, PresentationTimestamp: uint64(now.Unix())}}
	return PushRequest{Environment: environment, Token: preflightToken, TokenKind: UpdateToken, APNSID: apnsID, Event: "update",
		Timestamp: now, Expiration: now.Add(time.Minute), Priority: 5, Content: content}, nil
}

func randomUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}
