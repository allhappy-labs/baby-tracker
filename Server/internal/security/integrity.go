package security

import "context"

// CommittedFile identifies one canonical disk envelope selected by a trusted head.
type CommittedFile struct {
	Revision     uint64
	SelectedName string
	Prior        Head
	EventHash    [32]byte
	RawDigest    [32]byte
}

type CommittedInventory struct {
	Head  Head
	Files []CommittedFile
}

// IntegrityLedger is an optional optimized reader. Every call verifies the
// retained raw bytes; callers must still validate events when applying them.
// VerifyInventory and ReadValidatedExtension require metadata retained from a
// previous ReadValidatedInventory or ReadValidatedExtension result. The caller
// must keep that metadata private and must not construct or edit its digests:
// raw-byte verification does not reparse previously validated envelopes.
type IntegrityLedger interface {
	ReadValidatedInventory(context.Context, Head) (CommittedInventory, []Candidate, error)
	VerifyInventory(context.Context, CommittedInventory) error
	ReadValidatedExtension(context.Context, CommittedInventory, Head) (CommittedInventory, []Candidate, error)
}
