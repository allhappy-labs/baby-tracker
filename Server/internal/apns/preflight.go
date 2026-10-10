package apns

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// PreflightOutcome is the operator's verdict on one preflight push.
type PreflightOutcome string

const (
	PreflightPass        PreflightOutcome = "pass"        // 400 BadDeviceToken: the key, team and key ID were accepted
	PreflightBadKey      PreflightOutcome = "bad_key"     // 403 InvalidProviderToken / BadEnvironmentKeyIdInToken / UnrelatedKeyIdInToken
	PreflightTopic       PreflightOutcome = "topic"       // 400 DeviceTokenNotForTopic / TopicDisallowed / BadTopic
	PreflightUnreachable PreflightOutcome = "unreachable" // no answer from Apple
	PreflightUnexpected  PreflightOutcome = "unexpected"  // anything else
)

// PreflightResult is Apple's answer to a preflight. Status and Reason are
// Apple's; for PreflightUnreachable, Status is 0 and Reason is the coarse
// cause (ErrCause*: "dns", "tls", "connection", "timeout", ...).
type PreflightResult struct {
	Outcome PreflightOutcome
	Status  int
	Reason  string
}

// preflightExpiration bounds the preflight push: Apple discards it within a
// minute whatever it decides.
const preflightExpiration = time.Minute

// Preflight sends one push of pushType (PushBackground or PushLiveActivity)
// to an all-zero 32-byte token (main's dummy-token check,
// liveactivity/preflight.go:47-116) and classifies Apple's answer, the same
// for both. A background preflight is WakePayload at priority 5 to the
// bundle topic; a Live Activity preflight is main's minimal quiet update at
// priority 10 to the .push-type.liveactivity topic. It proves the key, team
// and key ID; it cannot prove the topic: Apple does not document whether a
// bad token is refused before or after the topic is checked, so a pass only
// means the topic was not contradicted.
//
// An answer that never came (no connection, or a request whose answer was
// lost) is PreflightUnreachable; an undocumented final status (1xx, 3xx) is
// PreflightUnexpected with that status. The error is non-nil only for a
// request that could not be built: an invalid environment or push type, or a
// provider token that could not be signed (wrapping ErrNotSent).
func (c *Client) Preflight(ctx context.Context, env Environment, pushType string) (PreflightResult, error) {
	now := c.now()
	request := Request{Environment: env, Token: make([]byte, 32), PushType: pushType, Expiration: ceilUnix(now.Add(preflightExpiration))}
	switch pushType {
	case PushBackground:
		request.Priority, request.Payload = 5, []byte(WakePayload)
	case PushLiveActivity:
		request.Priority, request.Payload = 10, preflightLiveActivityPayload(now)
	default:
		return PreflightResult{Outcome: PreflightUnexpected}, errors.Join(ErrNotSent, errors.New("invalid APNs push type"))
	}
	result, err := c.Send(ctx, request)
	if err != nil {
		if result.Status != 0 {
			// A final status APNs does not document (1xx, 3xx): an answer, not Apple's.
			return PreflightResult{Outcome: PreflightUnexpected, Status: result.Status}, nil
		}
		if cause := transportCause(err); cause != nil {
			// Nothing reached Apple, or its answer did not reach us.
			return PreflightResult{Outcome: PreflightUnreachable, Reason: cause.Error()}, nil
		}
		return PreflightResult{Outcome: PreflightUnexpected}, err
	}
	return PreflightResult{Outcome: classifyPreflight(result), Status: result.Status, Reason: result.Reason}, nil
}

// preflightID is main's fixed, obviously synthetic identifier for the dummy
// Live Activity content.
const preflightID = "0f1e2d3c-4b5a-4968-8778-695a4b3c2d1e"

// The JSON shape of main's ActivityKit content state (liveactivity/types.go),
// reduced to what the preflight's update carries.
type preflightScope struct {
	ServerInstanceID string `json:"serverInstanceID"`
	RecoveryEpoch    uint64 `json:"recoveryEpoch"`
	FamilyID         string `json:"familyID"`
	BabyID           string `json:"babyID"`
	KindRaw          string `json:"kindRaw"`
}

type preflightRevision struct {
	Scope                 preflightScope `json:"scope"`
	GenerationID          string         `json:"generationID"`
	FamilySequence        uint64         `json:"familySequence"`
	PresentationVersion   uint64         `json:"presentationVersion"`
	PresentationTimestamp uint64         `json:"presentationTimestamp"`
	CanWrite              bool           `json:"canWrite"`
}

type preflightContentState struct {
	StartedAt    float64            `json:"startedAt"`
	SideLabel    string             `json:"sideLabel"`
	ActivityKind string             `json:"activityKind"`
	Phase        string             `json:"phase"`
	Parts        []struct{}         `json:"parts"`
	OmittedParts uint64             `json:"omittedParts"`
	Remote       *preflightRevision `json:"remote"`
}

// foundationReferenceUnixSeconds is 2001-01-01T00:00:00Z: ActivityKit dates
// are Foundation reference seconds.
const foundationReferenceUnixSeconds = 978307200

// preflightLiveActivityPayload is main's preflight request (preflightRequest
// and encodePushPayload): a quiet "update" of a paused feed timer with no
// alert, no attributes and no care content, timestamped at the current
// second so Apple does not refuse it for its age.
func preflightLiveActivityPayload(now time.Time) []byte {
	second := now.UTC().Truncate(time.Second).Unix()
	scope := preflightScope{ServerInstanceID: preflightID, RecoveryEpoch: 1, FamilyID: preflightID, BabyID: preflightID, KindRaw: "feed"}
	content := preflightContentState{StartedAt: float64(second - foundationReferenceUnixSeconds), ActivityKind: "paused", Phase: "running", Parts: []struct{}{},
		Remote: &preflightRevision{Scope: scope, GenerationID: preflightID, PresentationVersion: 1, PresentationTimestamp: uint64(second)}}
	payload, _ := json.Marshal(map[string]any{"aps": map[string]any{"timestamp": second, "event": "update", "content-state": content}})
	return payload
}

// transportCause is the coarse cause of a failed exchange with Apple: an
// Ambiguous send, or one that failed in the transport before writing. A
// local failure (validation, signing) has none.
func transportCause(err error) error {
	for _, cause := range []error{ErrCauseTimeout, ErrCauseCanceled, ErrCauseDNS, ErrCauseTLS, ErrCauseConnection} {
		if errors.Is(err, cause) {
			return cause
		}
	}
	return nil
}

func classifyPreflight(result Result) PreflightOutcome {
	switch {
	case result.Status == http.StatusBadRequest && result.Reason == "BadDeviceToken":
		return PreflightPass
	case result.Status == http.StatusForbidden && (result.Reason == "InvalidProviderToken" || result.Reason == "BadEnvironmentKeyIdInToken" || result.Reason == "UnrelatedKeyIdInToken"):
		return PreflightBadKey
	case result.Status == http.StatusBadRequest && (result.Reason == "DeviceTokenNotForTopic" || result.Reason == "TopicDisallowed" || result.Reason == "BadTopic"):
		return PreflightTopic
	default:
		return PreflightUnexpected
	}
}
