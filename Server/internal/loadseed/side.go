//go:build identitytest

package loadseed

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"babytracker/server/internal/loadtest"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

// freshMargin is how close to expiry a device's access token may be before
// the seeder refreshes it ahead of a request, as a phone does.
const freshMargin = 2 * time.Minute

// sideLoad is the side stage's counts.
type sideLoad struct {
	cursors, pushTokens, deviceRegistrations, activities int
}

// cursorShare is the share of devices whose cursor is at the family's
// head; the rest lag 1 to 20 changes.
const cursorShare = 0.80

// loadSide writes the side tables:
//   - device_sync_cursors with COPY as the superuser: 80% of devices at the
//     family's head, the rest 1 to 20 changes behind, seen within the day
//     before AsOf; each device's fleet Cursor is its acknowledged sequence;
//   - a push token for PushTokenShare of devices, through the push-token
//     service (PUT /v1/devices/push-token's call);
//   - a Live Activity registration for every device and a card for every
//     running timer of its family, through the Live Activity service
//     (PUT /v1/live-activities/device and /activity's calls).
//
// Tokens are synthetic 64-hex strings. A device whose access token is
// about to expire refreshes first.
func loadSide(ctx context.Context, env Environment, s *services, spec Spec, families []*familyState, cares careLoad) (sideLoad, error) {
	var result sideLoad
	type cursor struct {
		familyID, deviceID string
		acked              uint64
		seen               time.Time
	}
	var cursors []cursor
	for i, f := range families {
		head := cares.states[i].Sequence
		for slot, d := range f.Devices {
			r := rand.New(rand.NewPCG(spec.Seed^0x5eed_c025_0000_0002, uint64(i*DevicesPerFamily+slot)))
			acked := head
			if r.Float64() >= cursorShare {
				lag := uint64(1 + r.IntN(20))
				acked = head - min(lag, head)
			}
			seen := spec.AsOf.Add(-time.Duration(r.Int64N(int64(24 * time.Hour)))).Truncate(time.Microsecond)
			d.Cursor = acked
			cursors = append(cursors, cursor{familyID: f.FamilyID, deviceID: d.DeviceID, acked: acked, seen: seen})
		}
	}
	next := 0
	n, err := copyFrom(ctx, env.Superuser, "device_sync_cursors", []string{"family_id", "device_id", "acked_sequence", "delivered_sequence", "seen_at"},
		pgx.CopyFromFunc(func() ([]any, error) {
			if next == len(cursors) {
				return nil, nil
			}
			c := cursors[next]
			next++
			return []any{c.familyID, c.deviceID, int64(c.acked), int64(c.acked), c.seen}, nil
		}))
	if err != nil {
		return result, err
	}
	result.cursors = int(n)

	var pushTokens, registrations, activities atomic.Int64
	err = forEach(ctx, spec.Workers, len(families), func(i int) error {
		f := families[i]
		for slot, d := range f.Devices {
			r := rand.New(rand.NewPCG(spec.Seed^0x5eed_9054_0000_0003, uint64(i*DevicesPerFamily+slot)))
			if err := s.fresh(ctx, d, freshMargin); err != nil {
				return err
			}
			req, err := s.request(d)
			if err != nil {
				return err
			}
			if r.Float64() < spec.PushTokenShare {
				token := syntheticToken("push", spec.Seed, i, slot, 0)
				raw, _ := hex.DecodeString(token)
				if err := s.pushTokens.Register(ctx, req, "sandbox", raw); err != nil {
					return fmt.Errorf("loadseed: family %d device %d push token: %w", i, slot, err)
				}
				d.PushToken = token
				pushTokens.Add(1)
			}
			installation := loadtest.LiveActivityInstallationID(d.DeviceID)
			pushToStart := syntheticToken("live-activity", spec.Seed, i, slot, 0)
			meta := protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityLiveActivityPushV1}}
			if _, err := s.liveActivity.RegisterDevice(ctx, req, protocol.LiveActivityDeviceRegistration{RequestMeta: meta,
				InstallationID: installation, RegistrationVersion: 1, APNsEnvironment: "sandbox", PayloadVersion: protocol.LiveActivityPayloadVersion,
				PushToStartToken: pushToStart, FeedBreakMinutes: 30}); err != nil {
				return fmt.Errorf("loadseed: family %d device %d Live Activity registration: %w", i, slot, err)
			}
			d.LiveActivityToken = pushToStart
			registrations.Add(1)
			for b, baby := range cares.plans[i].Babies {
				if baby.RunningTimerID == "" {
					continue
				}
				timer := baby.RunningTimerID
				if _, err := s.liveActivity.RegisterActivity(ctx, req, protocol.LiveActivityTokenRegistration{RequestMeta: meta,
					InstallationID: installation, ActivityID: "load-" + timer, Token: syntheticToken("live-activity", spec.Seed, i, slot, uint64(1+b)),
					TokenVersion: 1, Scope: protocol.LiveActivityScope{ServerInstanceID: s.cfg.ServerInstanceID, RecoveryEpoch: s.cfg.RecoveryEpoch,
						FamilyID: f.FamilyID, BabyID: baby.ID, KindRaw: baby.RunningTimerKind}, TimerID: &timer, State: "active"}); err != nil {
					return fmt.Errorf("loadseed: family %d device %d Live Activity card: %w", i, slot, err)
				}
				activities.Add(1)
			}
		}
		return nil
	})
	result.pushTokens, result.deviceRegistrations, result.activities = int(pushTokens.Load()), int(registrations.Load()), int(activities.Load())
	return result, err
}
