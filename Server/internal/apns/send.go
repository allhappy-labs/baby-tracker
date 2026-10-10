package apns

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync/atomic"

	"babytracker/server/internal/protocol"
)

// The push types Send speaks.
const (
	PushBackground   = "background"
	PushLiveActivity = "liveactivity"
)

const (
	maxPayloadBytes = 4096 // Apple's limit for both push types
	maxSendToken    = 1024 // bytes; Live Activity tokens are longer than device tokens
	// liveActivityTopicSuffix makes the bundle ID a Live Activity topic.
	liveActivityTopicSuffix = ".push-type.liveactivity"
)

// Request is one push. Topic is the bundle ID, plus ".push-type.liveactivity"
// for PushLiveActivity: derived, never caller-supplied.
type Request struct {
	Environment Environment
	Token       []byte // 1..maxSendToken bytes, sent as lowercase hex
	PushType    string // PushBackground or PushLiveActivity
	Priority    int    // 5 or 10
	Expiration  int64  // apns-expiration, Unix seconds; 0 means "now or never" (a Live Activity start)
	APNsID      string // canonical lowercase UUID; "" means a random version 4 UUID
	Payload     []byte // 1..maxPayloadBytes, a JSON object
}

// Send sends one push over the reused HTTP/2 connection of r's environment.
//
// Its error wraps ErrNotSent when nothing was written: validation, signing,
// or a transport failure before the request's headers were written (decided
// by what the transport reports writing, net/http/httptrace, not by the
// error's type). Once the request may have reached Apple, a lost or
// unusable answer (no response, or a 1xx/3xx final status) is Class
// Ambiguous with an error wrapping ErrOutcomeUnknown and one ErrCause*, and
// every answer Apple gave is a Result with a nil error. A 403
// ExpiredProviderToken or InvalidProviderToken makes a later send re-sign,
// no sooner than 20 minutes after the last signing. No error names the
// host, the path or the token.
func (c *Client) Send(ctx context.Context, r Request) (Result, error) {
	// Everything up to http.Client.Do happens before a byte is written: these
	// failures are known-unsent.
	notSent := func(err error) (Result, error) {
		return Result{Class: ConfigurationFailure}, errors.Join(ErrNotSent, err)
	}
	if err := r.validate(); err != nil {
		return notSent(err)
	}
	apnsID := r.APNsID
	if apnsID == "" {
		var err error
		if apnsID, err = randomUUID(); err != nil {
			return notSent(err)
		}
	}
	provider, err := c.provider.token()
	if err != nil {
		return notSent(err)
	}
	host := c.productionURL
	if r.Environment == Sandbox {
		host = c.sandboxURL
	}
	topic := c.topic
	if r.PushType == PushLiveActivity {
		topic += liveActivityTopicSuffix
	}
	bounded, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	// What reached the wire decides a failed request: until the transport
	// begins writing the request's headers, Apple cannot have the push (no
	// DNS answer, a refused dial, a failed TLS handshake, a deadline before
	// any of it). Once it may have written a byte, Apple may have it.
	var written atomic.Bool
	bounded = httptrace.WithClientTrace(bounded, &httptrace.ClientTrace{
		WroteHeaderField: func(string, []string) { written.Store(true) },
		WroteHeaders:     func() { written.Store(true) },
		WroteRequest:     func(httptrace.WroteRequestInfo) { written.Store(true) },
	})
	request, err := http.NewRequestWithContext(bounded, http.MethodPost, host+"/3/device/"+hex.EncodeToString(r.Token), bytes.NewReader(r.Payload))
	if err != nil {
		return notSent(errors.New("invalid APNs request"))
	}
	request.Header.Set("authorization", "bearer "+provider)
	request.Header.Set("apns-push-type", r.PushType)
	request.Header.Set("apns-topic", topic)
	request.Header.Set("apns-id", apnsID)
	request.Header.Set("apns-priority", strconv.Itoa(r.Priority))
	request.Header.Set("apns-expiration", strconv.FormatInt(r.Expiration, 10))
	request.Header.Set("content-type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		if !written.Load() {
			// Nothing was written: not sent, safe to retry. The cause is
			// coarse; the transport's text names the host and path.
			return notSent(causeOf(err))
		}
		return Result{Class: Ambiguous}, outcomeUnknown(causeOf(err))
	}
	defer response.Body.Close()
	result := Result{Status: response.StatusCode, Class: classifyStatus(response.StatusCode), RetryAfter: parseRetryAfter(response.Header.Get("Retry-After"), c.now())}
	if result.Class == Ambiguous {
		// Not an APNs answer (no redirect is followed): Apple's verdict is unknown.
		return Result{Status: response.StatusCode, Class: Ambiguous}, outcomeUnknown(ErrCauseStatus)
	}
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
			result.Class = InvalidToken
		case "DeviceTokenNotForTopic", "TopicDisallowed", "BadTopic":
			// A wrong bundle ID or key scope is this deployment's configuration:
			// it says nothing about any one phone's token.
			result.Class = ConfigurationFailure
		case "ExpiredProviderToken", "InvalidProviderToken":
			// The next send re-signs (no sooner than Apple's 20-minute floor).
			if response.StatusCode == http.StatusForbidden {
				c.provider.invalidate()
			}
		}
	}
	return result, nil
}

// validate checks everything Apple would refuse for its shape, so a refusal
// is known-unsent. The error names the field, never a value.
func (r Request) validate() error {
	switch {
	case r.Environment != Sandbox && r.Environment != Production:
		return errors.New("invalid APNs environment")
	case len(r.Token) == 0 || len(r.Token) > maxSendToken:
		return errors.New("invalid APNs token")
	case r.PushType != PushBackground && r.PushType != PushLiveActivity:
		return errors.New("invalid APNs push type")
	case r.Priority != 5 && r.Priority != 10:
		return errors.New("invalid APNs priority")
	case r.Expiration < 0:
		return errors.New("invalid APNs expiration")
	case r.APNsID != "" && !protocol.ValidUUID(r.APNsID):
		return errors.New("invalid APNs request ID")
	case len(r.Payload) == 0 || len(r.Payload) > maxPayloadBytes:
		return errors.New("invalid APNs payload size")
	}
	// A JSON object: json.Unmarshal of null into a map succeeds with a nil map.
	var object map[string]json.RawMessage
	if json.Unmarshal(r.Payload, &object) != nil || object == nil {
		return errors.New("APNs payload must be a JSON object")
	}
	return nil
}
