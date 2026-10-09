package hosted

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"
)

var (
	ErrObjectNotFound = errors.New("hosted immutable object not found")
	ErrObjectExists   = errors.New("hosted immutable object already exists")
	ErrHeadNotFound   = errors.New("hosted head not found")
	ErrScratchFull    = errors.New("hosted replay scratch capacity exceeded")
)

type ObjectVersion struct {
	Key        string
	Version    string
	KeyID      string
	Ciphertext []byte
	SHA256     [32]byte
}

type ImmutableObjects interface {
	Create(context.Context, ObjectVersion) error
	Read(context.Context, string) (ObjectVersion, error)
}

type EncryptedHeadValue struct {
	Version    string
	KeyID      string
	Ciphertext []byte
}

type AtomicHeads interface {
	Load(context.Context) (EncryptedHeadValue, error)
	CompareAndSwap(context.Context, string, EncryptedHeadValue) (bool, error)
}

type EnvelopeCipher interface {
	Seal(context.Context, string, []byte, []byte) ([]byte, error)
	Open(context.Context, string, []byte, []byte) ([]byte, error)
}

type ReplayIndexEntry struct {
	Generation uint64
	Revision   uint64
	EventHash  [32]byte
}

type ReplayScratchRequest struct {
	Namespace string
	MaxBytes  int64
}

type EncryptedReplayScratch interface {
	Begin(context.Context, ReplayScratchRequest) (ReplayScratchSession, error)
}

type ReplayScratchSession interface {
	Append(context.Context, ReplayIndexEntry) error
	WalkReverse(context.Context, func(ReplayIndexEntry) error) error
	Close() error
}

type Properties struct {
	Provider            string
	Region              string
	LedgerFailureDomain string
	HeadFailureDomain   string
	LedgerKeyID         string
	HeadKeyID           string
	Encrypted           bool
	ImmutableLedger     bool
	ReplicatedHead      bool
	ConditionalCreate   bool
	LinearizableCAS     bool
}

type PropertyPolicy struct {
	AllowedProviders []string
	AllowedRegions   []string
	LedgerKeyID      string
	HeadKeyID        string
}

func ValidateProperties(observed Properties, policy PropertyPolicy) error {
	for _, value := range []string{
		observed.Provider, observed.Region, observed.LedgerFailureDomain, observed.HeadFailureDomain,
		observed.LedgerKeyID, observed.HeadKeyID, policy.LedgerKeyID, policy.HeadKeyID,
	} {
		if !validBoundedIdentifier(value) {
			return errors.New("invalid hosted store properties")
		}
	}
	if len(policy.AllowedProviders) == 0 || len(policy.AllowedRegions) == 0 ||
		!allValidIdentifiers(policy.AllowedProviders) || !allValidIdentifiers(policy.AllowedRegions) ||
		!exactlyOnce(policy.AllowedProviders, observed.Provider) || !exactlyOnce(policy.AllowedRegions, observed.Region) ||
		observed.LedgerFailureDomain == observed.HeadFailureDomain ||
		observed.LedgerKeyID == observed.HeadKeyID || policy.LedgerKeyID == policy.HeadKeyID ||
		observed.LedgerKeyID != policy.LedgerKeyID || observed.HeadKeyID != policy.HeadKeyID ||
		!observed.Encrypted || !observed.ImmutableLedger || !observed.ReplicatedHead ||
		!observed.ConditionalCreate || !observed.LinearizableCAS {
		return errors.New("hosted store properties do not satisfy policy")
	}
	return nil
}

func validBoundedIdentifier(value string) bool {
	return len(value) >= 1 && len(value) <= 128 && utf8.ValidString(value) && !containsControl(value)
}

func containsControl(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

func allValidIdentifiers(values []string) bool {
	for _, value := range values {
		if !validBoundedIdentifier(value) {
			return false
		}
	}
	return true
}

func exactlyOnce(values []string, wanted string) bool {
	count := 0
	for _, value := range values {
		if value == wanted {
			count++
		}
	}
	return count == 1
}

func safeUnavailable(component string) error {
	return fmt.Errorf("hosted %s unavailable", component)
}
