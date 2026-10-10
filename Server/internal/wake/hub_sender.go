package wake

import (
	"context"
	"errors"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
)

// HubSender wakes devices through the live hub: one Reannounce per family,
// naming its woken devices. The hub writes the family's high water to those
// devices' bound connections even when a connection was already sent that
// cursor (Publish never repeats one): a device behind its family has not
// pulled what it was told, and the iOS client syncs on any hint its local
// cursor does not cover. A woken device without a connection gets nothing
// from the hub.
type HubSender struct {
	Hub    *realtime.Hub
	Server protocol.ServerInfo
}

var errNoHub = errors.New("wake: hub sender has no hub")

func (s HubSender) Wake(ctx context.Context, wakes []Wake) error {
	if s.Hub == nil {
		return errNoHub
	}
	for _, wake := range wakes {
		s.Hub.Reannounce(ctx, realtime.Hint{FamilyID: wake.FamilyID, Cursor: protocol.Cursor{ServerInstanceID: s.Server.InstanceID,
			RecoveryEpoch: s.Server.RecoveryEpoch, FamilyID: wake.FamilyID, Sequence: wake.Sequence}}, wake.DeviceIDs)
	}
	return nil
}
