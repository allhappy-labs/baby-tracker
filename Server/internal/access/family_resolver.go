package access

import "context"

// FamilyResolver is the shared authorization boundary for services that need
// the caller's current active family. The access package owns the contract;
// family lifecycle owns the eventual implementation.
type FamilyResolver interface {
	Current(context.Context, Principal) (familyID string, err error)
}
