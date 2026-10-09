// Package protocol defines versioned JSON wire types shared by API handlers and
// future clients. IDs remain strings here so their serialized representation is
// explicit and platform-neutral.
package protocol

import (
	"errors"
	"fmt"
	"mime"
	"net/url"
	"regexp"
	"time"
)

const MaxSafeInteger uint64 = 9007199254740991

func ValidRecoveryEpoch(epoch uint64) bool { return epoch > 0 && epoch <= MaxSafeInteger }

var (
	// Validation categories are stable so callers can branch without parsing
	// variable field-detail text.
	ErrInvalidUUID           = errors.New("invalid UUID")
	ErrInvalidRecoveryEpoch  = errors.New("invalid recovery epoch")
	ErrInvalidServerOrigin   = errors.New("invalid server origin")
	ErrInvalidErrorResponse  = errors.New("invalid error response")
	ErrInvalidAttachmentLink = errors.New("invalid attachment link")
	ErrInvalidMediaType      = errors.New("invalid media type")
	ErrInvalidSHA256         = errors.New("invalid SHA-256")
	ErrInvalidLifecycle      = errors.New("invalid lifecycle")
	ErrInvalidTimestamp      = errors.New("invalid timestamp")
	ErrInvalidCollection     = errors.New("invalid collection")
	ErrInvalidText           = errors.New("invalid text")
	ErrInvalidDecimal        = errors.New("invalid decimal")
	ErrInvalidMutation       = errors.New("invalid mutation")
	ErrUnsupportedResource   = errors.New("unsupported resource")
)

var (
	canonicalUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	requestIDPattern     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	errorCodePattern     = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	sha256Pattern        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	lifecyclePattern     = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	mediaTypePattern     = regexp.MustCompile("^[a-z0-9!#$%&'*+.^_`|~-]+/[a-z0-9!#$%&'*+.^_`|~-]+$")
	rfc3339Pattern       = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$`)
)

// StatusResponse is used by dependency-independent liveness and readiness
// endpoints.
type StatusResponse struct {
	Status string `json:"status"`
}

// Validate confirms the stable operational response status.
func (response StatusResponse) Validate() error {
	if response.Status != "ok" {
		return fmt.Errorf("%w: status", ErrInvalidErrorResponse)
	}
	return nil
}

// ErrorCode is a stable machine-readable category in an ErrorResponse.
type ErrorCode string

const (
	ErrorCodeNotReady      ErrorCode = "not_ready"
	ErrorCodeInternalError ErrorCode = "internal_error"
)

// ErrorDetail is the safe, machine-readable and human-readable error body.
type ErrorDetail struct {
	Code              ErrorCode `json:"code"`
	Message           string    `json:"message"`
	RequestID         string    `json:"request_id"`
	Retryable         bool      `json:"retryable"`
	RetryAfterSeconds *int      `json:"retry_after_seconds,omitempty"`
}

// ErrorResponse is the versioned JSON error envelope returned by API routes.
type ErrorResponse struct {
	Error            ErrorDetail `json:"error"`
	ServerInstanceID string      `json:"server_instance_id,omitempty"`
	RecoveryEpoch    uint64      `json:"recovery_epoch,omitempty"`
}

// Validate checks the error envelope without constraining future additive
// error categories.
func (response ErrorResponse) Validate() error {
	if !errorCodePattern.MatchString(string(response.Error.Code)) || response.Error.Message == "" {
		return fmt.Errorf("%w: code or message", ErrInvalidErrorResponse)
	}
	if response.Error.RequestID != "" && !requestIDPattern.MatchString(response.Error.RequestID) {
		return fmt.Errorf("%w: request_id", ErrInvalidErrorResponse)
	}
	if response.Error.RetryAfterSeconds != nil && *response.Error.RetryAfterSeconds < 1 {
		return fmt.Errorf("%w: retry_after_seconds", ErrInvalidErrorResponse)
	}
	if (response.ServerInstanceID == "") != (response.RecoveryEpoch == 0) ||
		response.ServerInstanceID != "" && (!ValidUUID(response.ServerInstanceID) || !ValidRecoveryEpoch(response.RecoveryEpoch)) {
		return fmt.Errorf("%w: sync identity", ErrInvalidErrorResponse)
	}
	return nil
}

// Cursor scopes a monotonically increasing family sequence to one recovering
// server instance.
type Cursor struct {
	ServerInstanceID string `json:"server_instance_id"`
	RecoveryEpoch    uint64 `json:"recovery_epoch"`
	FamilyID         string `json:"family_id"`
	Sequence         uint64 `json:"sequence"`
}

// Validate confirms that the opaque cursor is safely scoped.
func (cursor Cursor) Validate() error {
	if err := validateUUID("server_instance_id", cursor.ServerInstanceID); err != nil {
		return err
	}
	if !ValidRecoveryEpoch(cursor.RecoveryEpoch) {
		return fmt.Errorf("%w: recovery_epoch", ErrInvalidRecoveryEpoch)
	}
	if cursor.Sequence > MaxSafeInteger {
		return fmt.Errorf("%w: sequence", ErrInvalidCollection)
	}
	return validateUUID("family_id", cursor.FamilyID)
}

// ServerInfo advertises the public API version and the server identity that
// clients bind their cursors and sessions to.
type ServerInfo struct {
	InstanceID             string           `json:"instance_id"`
	RecoveryEpoch          uint64           `json:"recovery_epoch"`
	CanonicalOrigin        string           `json:"canonical_origin"`
	APIVersions            []string         `json:"api_versions"`
	Capabilities           []string         `json:"capabilities"`
	AppleAuthorizationMode string           `json:"apple_authorization_mode"`
	Limits                 *EffectiveLimits `json:"limits,omitempty"`
	SourceVersion          string           `json:"source_version,omitempty"`
	SourceURL              string           `json:"source_url,omitempty"`
	MaxItemsPerPage        int              `json:"max_items_per_page,omitempty"`
	MaxEncodedBytes        int              `json:"max_encoded_bytes,omitempty"`
}

// Validate confirms a safe, canonical public server advertisement.
func (server ServerInfo) Validate() error {
	if err := validateUUID("instance_id", server.InstanceID); err != nil {
		return err
	}
	if !ValidRecoveryEpoch(server.RecoveryEpoch) {
		return fmt.Errorf("%w: recovery_epoch", ErrInvalidRecoveryEpoch)
	}
	origin, err := url.Parse(server.CanonicalOrigin)
	if err != nil || origin.Scheme != "https" || origin.Hostname() == "" || origin.User != nil || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") {
		return fmt.Errorf("%w: canonical_origin", ErrInvalidServerOrigin)
	}
	if server.APIVersions == nil || server.Capabilities == nil {
		return fmt.Errorf("%w: api_versions or capabilities", ErrInvalidCollection)
	}
	if len(server.APIVersions) == 0 {
		return fmt.Errorf("%w: api_versions", ErrInvalidServerOrigin)
	}
	if err := ValidateCapabilities(server.Capabilities, append(ServerCapabilities(), CapabilityRefreshRecoveryV1, CapabilityLiveActivityPushV1)); err != nil {
		return err
	}
	switch server.AppleAuthorizationMode {
	case "identity_token_only", "code_exchange_required":
	default:
		return fmt.Errorf("%w: apple_authorization_mode", ErrInvalidServerOrigin)
	}
	if server.Limits != nil {
		if err := server.Limits.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// AttachmentMetadata holds future attachment metadata only. It deliberately
// excludes bytes, storage-provider details, and transfer URLs.
type AttachmentMetadata struct {
	ID               string `json:"id"`
	FamilyID         string `json:"family_id"`
	CreatorAccountID string `json:"creator_account_id"`
	MediaType        string `json:"media_type"`
	ByteSize         uint64 `json:"byte_size"`
	SHA256           string `json:"sha256"`
	Lifecycle        string `json:"lifecycle"`
	CreatedAt        string `json:"created_at"`
	BabyID           string `json:"baby_id,omitempty"`
	RecordID         string `json:"record_id,omitempty"`
}

// Validate confirms attachment metadata without introducing blob handling.
func (attachment AttachmentMetadata) Validate() error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{"id", attachment.ID},
		{"family_id", attachment.FamilyID},
		{"creator_account_id", attachment.CreatorAccountID},
	} {
		if err := validateUUID(field.name, field.value); err != nil {
			return err
		}
	}
	if !mediaTypePattern.MatchString(attachment.MediaType) {
		return fmt.Errorf("%w: media_type", ErrInvalidMediaType)
	}
	parsedMediaType, _, err := mime.ParseMediaType(attachment.MediaType)
	if err != nil || parsedMediaType != attachment.MediaType {
		return fmt.Errorf("%w: media_type", ErrInvalidMediaType)
	}
	if !sha256Pattern.MatchString(attachment.SHA256) {
		return fmt.Errorf("%w: sha256", ErrInvalidSHA256)
	}
	if !lifecyclePattern.MatchString(attachment.Lifecycle) {
		return fmt.Errorf("%w: lifecycle", ErrInvalidLifecycle)
	}
	if !rfc3339Pattern.MatchString(attachment.CreatedAt) {
		return fmt.Errorf("%w: created_at", ErrInvalidTimestamp)
	}
	if _, err := time.Parse(time.RFC3339, attachment.CreatedAt); err != nil {
		return fmt.Errorf("%w: created_at", ErrInvalidTimestamp)
	}
	if (attachment.BabyID == "") == (attachment.RecordID == "") {
		return fmt.Errorf("%w: exactly one of baby_id or record_id", ErrInvalidAttachmentLink)
	}
	if attachment.BabyID != "" {
		return validateUUID("baby_id", attachment.BabyID)
	}
	return validateUUID("record_id", attachment.RecordID)
}

// AttachmentManifest is the versioned export seam. The current beta has no
// attachment delivery and therefore exports an empty attachment list.
type AttachmentManifest struct {
	Version     string               `json:"version"`
	Attachments []AttachmentMetadata `json:"attachments"`
}

// Validate confirms a versioned attachment export manifest.
func (manifest AttachmentManifest) Validate() error {
	if manifest.Version != "v1" {
		return fmt.Errorf("%w: manifest version", ErrInvalidLifecycle)
	}
	if manifest.Attachments == nil {
		return fmt.Errorf("%w: attachments", ErrInvalidCollection)
	}
	for index, attachment := range manifest.Attachments {
		if err := attachment.Validate(); err != nil {
			return fmt.Errorf("attachment %d: %w", index, err)
		}
	}
	return nil
}

func validateUUID(field, value string) error {
	if !canonicalUUIDPattern.MatchString(value) || value == "00000000-0000-0000-0000-000000000000" {
		return fmt.Errorf("%w: %s", ErrInvalidUUID, field)
	}
	return nil
}

func ValidUUID(value string) bool {
	return canonicalUUIDPattern.MatchString(value) && value != "00000000-0000-0000-0000-000000000000"
}
