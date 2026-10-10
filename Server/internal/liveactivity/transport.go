package liveactivity

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"babytracker/server/internal/apns"
	"babytracker/server/internal/protocol"
)

// The push request and its answer, validation and payload encoding are
// main's (main:Server/internal/liveactivity/apns.go), verbatim. The wire is
// the branch's one APNs client (internal/apns.Send) through APNSTransport.

const maximumAPNsPayloadBytes = 4096

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

var apnsTokenPattern = regexp.MustCompile(`^[0-9a-f]+$`)

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

// APNSTransport is main's Transport over the branch's one APNs client: the
// request is validated and encoded as main's was, then sent with
// apns.Send as a liveactivity push (the topic is the client's bundle ID plus
// ".push-type.liveactivity"). Apple's answer keeps its class, status, reason
// and Retry-After, under the client's rules (its reason list, its 403
// re-sign floor, its 24-hour Retry-After cap). A request known not to have
// been written (validation, encoding, signing, or a transport failure before
// the headers were written) wraps ErrRequestNotSent; an Ambiguous answer
// keeps the client's error, which the worker only logs by class.
type APNSTransport struct{ Client *apns.Client }

func (t APNSTransport) Send(ctx context.Context, r PushRequest) (PushResult, error) {
	notSent := func(err error) (PushResult, error) {
		return PushResult{Classification: ConfigurationFailure}, errors.Join(ErrRequestNotSent, err)
	}
	if t.Client == nil {
		return notSent(errors.New("APNs client unavailable"))
	}
	if err := validatePushRequest(r); err != nil {
		return notSent(err)
	}
	payload, err := encodePushPayload(r)
	if err != nil {
		return notSent(err)
	}
	raw, err := hex.DecodeString(r.Token)
	if err != nil {
		return notSent(errors.New("invalid live activity push request"))
	}
	expiration := ceilUnix(r.Expiration)
	if r.Event == "start" {
		expiration = 0
	}
	result, err := t.Client.Send(ctx, apns.Request{Environment: apns.Environment(r.Environment), Token: raw, PushType: apns.PushLiveActivity,
		Priority: r.Priority, Expiration: expiration, APNsID: r.APNSID, Payload: payload})
	out := PushResult{HTTPStatus: result.Status, Reason: result.Reason, RetryAfter: result.RetryAfter, Classification: PushClassification(result.Class)}
	if errors.Is(err, apns.ErrNotSent) {
		return out, errors.Join(ErrRequestNotSent, err)
	}
	return out, err
}
