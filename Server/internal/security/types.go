package security

import (
	"context"
	"encoding/json"
	"errors"

	"babytracker/server/internal/access"
)

type Head struct {
	Generation uint64
	Revision   uint64
	EventHash  [32]byte
}

type Candidate struct {
	CommandID string
	Prior     Head
	EventJSON []byte
	EventHash [32]byte
}

type SecurityLedger interface {
	AppendCandidate(context.Context, Candidate) error
	ReadCommitted(context.Context, Head) ([]Candidate, error)
	WalkCommitted(context.Context, Head, func(Candidate) error) error
}

type LedgerHeadStore interface {
	Load(context.Context) (Head, error)
	CompareAndSwap(context.Context, Head, Head) (bool, error)
}

type Command struct {
	ID                 string
	Kind               EventKind
	AffectedAccountIDs []string
	AffectedFamilyIDs  []string
	Payload            json.RawMessage
	verifiedIdentity   bool
}

type CommandResult struct {
	CommandID string `json:"command_id"`
	Outcome   string `json:"outcome"`
	Code      string `json:"code"`
	Revision  uint64 `json:"revision"`
}

type Projection interface {
	Head(context.Context) (Head, error)
	Apply(context.Context, Candidate) error
}

type Fence interface {
	Read(context.Context) (func(), error)
	Write(context.Context) (func(), error)
}

type Decision func(context.Context, access.Principal, string, string) (access.Grant, error)

type FamilyPolicy struct {
	MaxCaregivers                int
	MaxInvitations               int
	RequireFamilySyncEntitlement bool
}

var (
	ErrLedgerCorrupt       = coded("ledger_corrupt")
	ErrTrustedHeadRequired = coded("trusted_head_required")
	ErrUnsupportedEvent    = coded("unsupported_security_event")
	ErrMembershipRevoked   = coded("membership_revoked")
	ErrUnavailable         = coded("service_unavailable")
)

type coded string

func (e coded) Error() string { return string(e) }

func Code(err error) string {
	if err == nil {
		return ""
	}
	var c coded
	if errors.As(err, &c) {
		return string(c)
	}
	return "service_unavailable"
}

func errorCode(code string) error { return coded(code) }

// IsCodedError reports whether err carries one of this package's fixed codes —
// an authority, ledger or availability failure — rather than a caller's data.
func IsCodedError(err error) bool {
	var c coded
	return errors.As(err, &c)
}
