package wake

import (
	"context"
	"time"
)

// KeepReleaseForTest keeps one settle that gives back the reservations a
// tick made at reservedAt for sequence on each of devices (to none), as a
// tick whose pushes never started would.
func (w *Worker) KeepReleaseForTest(family string, devices []string, sequence uint64, reservedAt time.Time) {
	settle := pendingSettle{reservedAt: reservedAt}
	for _, device := range devices {
		settle.unsent = append(settle.unsent, pushJob{family: family, device: device, sequence: sequence})
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.keepSettle(settle)
}

// ApplySettlesForTest applies the kept settles as a tick's replay does and
// returns how many applied.
func (w *Worker) ApplySettlesForTest(ctx context.Context) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	applied, err := w.applySettles(ctx)
	return len(applied), err
}
