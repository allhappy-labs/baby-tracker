package auth

import (
	"context"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/identity"
)

// AcknowledgeRefresh retires the caller's recovery receipt for attemptID
// once the client holds the recovered child, in one authority transaction:
// the authority checks the caller's session first (epoch, access expiry,
// session, deadline), then the device proof recomputed under the caller's
// device row; a mismatch is device_continuity_invalid.
func (service *Service) AcknowledgeRefresh(ctx context.Context, principal access.Principal, attemptID string, claim identity.DeviceContinuityClaim) error {
	if service == nil || service.authority == nil || !validUUID(attemptID) || attemptID == "00000000-0000-4000-8000-000000000000" {
		return ErrInvalidRequest
	}
	return service.authority.ProveAndAcknowledgeRefresh(ctx, principal, attemptID,
		func(stored authority.DeviceBinding) bool { return service.continuityMatches(stored, &claim) })
}
