package identity

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

type Challenge struct {
	Challenge        string
	ServerInstanceID string
	CanonicalOrigin  string
	ExpiresAt        time.Time
}

type ProfileInput struct {
	DisplayName string
	Confirmed   bool
}

type VerifiedProfile struct {
	DisplayName string
	Confirmed   bool
}

type ExchangeRequest struct {
	CommandID         string
	Challenge         string
	IdentityToken     string
	AuthorizationCode string
	InstallationID    string
	Profile           *ProfileInput
	DeviceContinuity  *DeviceContinuityClaim
}

type DeviceContinuityClaim struct {
	DeviceID   string
	Credential string
}

type VerifiedIdentity struct {
	Provider, Subject, Audience, IdentityKey string
	IssuedAt, ExpiresAt                      time.Time
}

// VerifiedExchange is a verified sign-in proof. Candidates are the
// identity's authority keys (current first) and Authority is their
// resolution when the challenge was consumed; the sign-in transaction
// resolves them again under its locks.
type VerifiedExchange struct {
	Identity   VerifiedIdentity
	Profile    *VerifiedProfile
	Candidates []string
	Authority  IdentityAuthority
}

type DigestKey struct {
	ID     string
	Secret [32]byte
}

type DigestKeyring struct {
	Current  DigestKey
	Previous []DigestKey
}

// KeyedDigest is a credential's HMAC under one key: a session lookup key,
// never printed or logged. Its print and log forms show the key ID only.
type KeyedDigest struct {
	KeyID string
	Sum   [32]byte
}

// String is d with its sum redacted (also %v and %+v).
func (d KeyedDigest) String() string { return "{KeyID:" + d.KeyID + " Sum:[redacted]}" }

// GoString is d with its sum redacted (%#v).
func (d KeyedDigest) GoString() string {
	return "identity.KeyedDigest{KeyID:" + strconv.Quote(d.KeyID) + ", Sum:[redacted]}"
}

// errDigestNotSerializable refuses a keyed digest's JSON form.
var errDigestNotSerializable = errors.New("identity: a keyed digest is not serializable")

// MarshalJSON refuses: a digest is never serialized, so a json.Marshal that
// reaches one fails loudly instead of writing its sum (slog's JSON handler
// writes an error marker in its place).
func (d KeyedDigest) MarshalJSON() ([]byte, error) { return nil, errDigestNotSerializable }

// LogValue is d's key ID with its sum redacted.
func (d KeyedDigest) LogValue() slog.Value {
	return slog.GroupValue(slog.String("key_id", d.KeyID), slog.String("sum", "[redacted]"))
}

type IdentityVerifier interface {
	Verify(context.Context, string, string) (VerifiedIdentity, error)
}

type IdentityVerifierFunc func(context.Context, string, string) (VerifiedIdentity, error)

func (function IdentityVerifierFunc) Verify(ctx context.Context, token, nonce string) (VerifiedIdentity, error) {
	return function(ctx, token, nonce)
}

type Options struct {
	Verifier     IdentityVerifier
	Authority    Authority
	Origin       string
	InstanceID   string
	IdentityKeys DigestKeyring
	Now          func() time.Time
}

type AppleOptions struct {
	Audience string
	HTTP     *http.Client
	Now      func() time.Time
}

var keyIDPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)

var (
	ErrInvalidAppleProof         = codedError("invalid_apple_proof")
	ErrInvalidOrExpiredChallenge = codedError("invalid_or_expired_challenge")
	ErrDisplayNameRequired       = codedError("display_name_required")
	ErrInvalidRequest            = codedError("invalid_request")
	ErrUnavailable               = codedError("service_unavailable")
)

type codedError string

func (e codedError) Error() string { return string(e) }

func Code(err error) string {
	if code, ok := ExplicitCode(err); ok || err == nil {
		return code
	}
	return "service_unavailable"
}

// ExplicitCode returns the identity code err carries, if any. An identity
// code wraps any cause it was given (an Apple notification refused because
// the authority quarantined the identity is service_unavailable); errors the
// identity package passes through unchanged (sign-in resolution) carry the
// authority's code instead, which auth.Code reports.
func ExplicitCode(err error) (string, bool) {
	var coded codedError
	if errors.As(err, &coded) {
		return string(coded), true
	}
	return "", false
}

func validDigestKey(key DigestKey) bool {
	if !keyIDPattern.MatchString(key.ID) {
		return false
	}
	var zero [32]byte
	return key.Secret != zero
}

func validateKeyring(keyring DigestKeyring) bool {
	if !validDigestKey(keyring.Current) || len(keyring.Previous) > 7 {
		return false
	}
	seen := map[string]struct{}{keyring.Current.ID: {}}
	seenSecrets := map[[32]byte]struct{}{keyring.Current.Secret: {}}
	for _, key := range keyring.Previous {
		if !validDigestKey(key) {
			return false
		}
		if _, exists := seen[key.ID]; exists {
			return false
		}
		if _, exists := seenSecrets[key.Secret]; exists {
			return false
		}
		seen[key.ID] = struct{}{}
		seenSecrets[key.Secret] = struct{}{}
	}
	return true
}
