package apns

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"babytracker/server/internal/config"
)

const (
	SandboxURL    = "https://api.sandbox.push.apple.com"
	ProductionURL = "https://api.push.apple.com"
	// WakePayload is the only payload: no family hint, opaque or not (APNs ruling).
	WakePayload    = `{"aps":{"content-available":1}}`
	requestTimeout = 10 * time.Second // main's bound
	// A wake older than an hour is replaced by the next lag check.
	wakeExpiration = time.Hour
	// A Retry-After beyond a day is not trusted.
	maxRetryAfter = 24 * time.Hour
	minTokenBytes = 16
	maxTokenBytes = 256
)

// Environment is the APNs environment a device token was issued for.
type Environment string

const (
	Sandbox    Environment = "sandbox"
	Production Environment = "production"
)

// Class is main's PushClassification.
type Class string

const (
	Accepted             Class = "accepted"
	InvalidToken         Class = "invalidToken"         // 410, or 400 BadDeviceToken / Unregistered: delete this token
	Retryable            Class = "retryable"            // 429, 5xx
	ConfigurationFailure Class = "configurationFailure" // 403, other 4xx: the deployment's key, team or topic
	Ambiguous            Class = "ambiguous"            // no response
)

// Result is Apple's answer to one push.
type Result struct {
	Status     int
	Reason     string    // one of Apple's documented reasons, else ""
	RetryAfter time.Time // zero when absent
	Class      Class
}

// ErrNotSent marks a Send (or Wake) that failed before any byte of the
// request was written: validation, provider-token signing, or a transport
// failure before the request's headers were written (Apple's host did not
// resolve, the dial or the TLS handshake failed, the deadline came first);
// the latter also wraps one coarse ErrCause*. It is safe to retry.
var ErrNotSent = errors.New("apns_request_not_sent")

// ErrOutcomeUnknown is wrapped by every Ambiguous Send's error: Apple may have
// the push, but no usable answer arrived. The error also wraps one coarse
// ErrCause*; it never names the host, the path or the token.
var ErrOutcomeUnknown = errors.New("APNs request outcome is unknown")

// The coarse causes of an unknown outcome.
var (
	ErrCauseTimeout    = errors.New("timeout")           // the request bound or the caller's deadline ran out
	ErrCauseCanceled   = errors.New("canceled")          // the caller cancelled
	ErrCauseDNS        = errors.New("dns")               // Apple's host did not resolve
	ErrCauseTLS        = errors.New("tls")               // the TLS handshake or certificate failed
	ErrCauseConnection = errors.New("connection")        // refused, reset or closed connection or stream
	ErrCauseStatus     = errors.New("unexpected_status") // a final status APNs does not document (1xx, 3xx)
)

// Client sends background wakes and Live Activity pushes (Send). It is safe
// for concurrent use.
type Client struct {
	http                      *http.Client
	provider                  *providerToken
	topic                     string
	sandboxURL, productionURL string
	now                       func() time.Time
}

// New talks to Apple's two hosts over one reused HTTP/2 connection each.
// Construct a new client on configuration reload to refresh key material.
func New(creds config.APNSCredentials) (*Client, error) {
	key, err := config.ReadAPNSPrivateKey(creds.KeyFile)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{ForceAttemptHTTP2: true, MaxIdleConns: 32, MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 5 * time.Second}
	return newClient(creds, key, transport, SandboxURL, ProductionURL, time.Now), nil
}

// NewWithTransport is New over rt and other hosts: the local TLS HTTP/2 test
// server and the identitytest recorder. A nil now is time.Now.
func NewWithTransport(creds config.APNSCredentials, rt http.RoundTripper, sandboxURL, productionURL string, now func() time.Time) (*Client, error) {
	if rt == nil || sandboxURL == "" || productionURL == "" {
		return nil, errors.New("APNs transport and hosts are required")
	}
	key, err := config.ReadAPNSPrivateKey(creds.KeyFile)
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return newClient(creds, key, rt, sandboxURL, productionURL, now), nil
}

func newClient(creds config.APNSCredentials, key *ecdsa.PrivateKey, rt http.RoundTripper, sandboxURL, productionURL string, now func() time.Time) *Client {
	client := &http.Client{Timeout: requestTimeout, Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Client{http: client, provider: newProviderToken(creds.TeamID, creds.KeyID, key, now), topic: creds.BundleID,
		sandboxURL: sandboxURL, productionURL: productionURL, now: now}
}

// CloseIdleConnections closes the idle connections to Apple.
func (c *Client) CloseIdleConnections() { c.http.CloseIdleConnections() }

var apnsReasons = map[string]bool{
	"BadDeviceToken": true, "DeviceTokenNotForTopic": true, "Unregistered": true,
	"BadTopic": true, "TopicDisallowed": true, "InvalidProviderToken": true, "ExpiredProviderToken": true,
	"Forbidden": true, "BadCertificate": true, "BadCertificateEnvironment": true,
	"BadExpirationDate": true, "BadPriority": true, "BadMessageId": true, "BadPath": true,
	"PayloadEmpty": true, "PayloadTooLarge": true, "BadCollapseId": true,
	"TooManyRequests": true, "TooManyProviderTokenUpdates": true, "InternalServerError": true, "ServiceUnavailable": true,
	"Shutdown": true, "IdleTimeout": true, "MethodNotAllowed": true, "MissingDeviceToken": true,
	"MissingTopic": true, "MissingProviderToken": true,
	// Apple's current reasons beyond main's list (main's "BadPushType" is not one).
	"ExpiredToken": true, "BadEnvironmentKeyIdInToken": true, "UnrelatedKeyIdInToken": true,
	"DuplicateHeaders": true, "InvalidPushType": true,
}

// KnownReason reports whether reason is one of Apple's documented APNs
// reasons (the list Send keeps a Result's Reason to). Callers that store a
// reason (the Live Activity history) keep only these.
func KnownReason(reason string) bool { return apnsReasons[reason] }

// Wake sends one background wake to a device token (raw bytes, sent as
// lowercase hex): Send with PushBackground, priority 5 (Apple requires it
// for background pushes), an expiration an hour from now and WakePayload.
// The device-token bound, 16..256 bytes, is Wake's own (ErrNotSent); errors
// and classes are Send's.
func (c *Client) Wake(ctx context.Context, env Environment, token []byte) (Result, error) {
	if len(token) < minTokenBytes || len(token) > maxTokenBytes {
		return Result{Class: ConfigurationFailure}, errors.Join(ErrNotSent, errors.New("invalid APNs wake request"))
	}
	return c.Send(ctx, Request{Environment: env, Token: token, PushType: PushBackground, Priority: 5,
		Expiration: ceilUnix(c.now().Add(wakeExpiration)), Payload: []byte(WakePayload)})
}

func ceilUnix(t time.Time) int64 {
	seconds := t.Unix()
	if t.Nanosecond() > 0 {
		seconds++
	}
	return seconds
}

func classifyStatus(status int) Class {
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

// parseRetryAfter reads either form, bounded to maxRetryAfter from now (main
// bounded only the seconds form); an answer beyond it, in the past or
// unreadable is zero (absent).
func parseRetryAfter(value string, now time.Time) time.Time {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && seconds >= 0 && seconds <= int64(maxRetryAfter/time.Second) {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) && !date.After(now.Add(maxRetryAfter)) {
		return date
	}
	return time.Time{}
}

// outcomeError is an Ambiguous Send's error: ErrOutcomeUnknown and a coarse
// cause, never the underlying error's text (which names the host and path).
type outcomeError struct{ cause error }

func outcomeUnknown(cause error) error { return outcomeError{cause: cause} }

func (e outcomeError) Error() string   { return ErrOutcomeUnknown.Error() + ": " + e.cause.Error() }
func (e outcomeError) Unwrap() []error { return []error{ErrOutcomeUnknown, e.cause} }

// causeOf maps a transport error to its coarse cause.
func causeOf(err error) error {
	var dnsError *net.DNSError
	var certificate *tls.CertificateVerificationError
	var alert tls.AlertError
	var record tls.RecordHeaderError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var netError net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return ErrCauseCanceled
	case errors.As(err, &dnsError):
		return ErrCauseDNS
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netError) && netError.Timeout()):
		return ErrCauseTimeout
	case errors.As(err, &certificate) || errors.As(err, &alert) || errors.As(err, &record) ||
		errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalid):
		return ErrCauseTLS
	default:
		return ErrCauseConnection
	}
}

// randomUUID is a lower-case version-4 UUID for apns-id.
func randomUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", errors.New("APNs request ID unavailable")
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}
