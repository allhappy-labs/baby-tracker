package main

import (
	"context"
	"testing"

	"babytracker/server/internal/config"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
	"babytracker/server/internal/testkit"
	"babytracker/server/internal/wake"
)

// main's stop of the wake worker (X-1): progress a draining request records
// after Run's own stop flush (here between Run's return and done closing,
// where the HTTP drain overlaps it) is written by stopWakeWorker's final
// flush, so a graceful restart loses none (C42).
func TestStopWakeWorkerFlushesProgressRecordedDuringTheDrain(t *testing.T) {
	h := testkit.New(t)
	hub, err := realtime.NewHub(t.Context(), realtime.HubOptions{Limits: protocol.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	tracker := wake.NewTracker(0)
	worker := newWakeWorker(config.Config{}, hub, h.Maintenance, nil, tracker, nil, nil)
	seq := uint64(1) // any presented cursor: the flush keeps the highest
	before, during := "d2000000-0000-4000-8000-000000000001", "d2000000-0000-4000-8000-000000000002"
	ctx, cancel := context.WithCancel(t.Context())
	ran := make(chan struct{})
	go func() { defer close(ran); worker.Run(ctx, nil) }()
	tracker.Synced(h.FamilyID, before, seq, seq, h.Now)
	done := make(chan struct{})
	go func() {
		<-ran
		tracker.Synced(h.FamilyID, during, seq, seq, h.Now) // the drain, after Run's flush
		close(done)
	}()
	stopWakeWorker(cancel, done, worker)
	rows, err := h.Maintenance.Query(t.Context(), `SELECT device_id::text, acked_sequence FROM device_sync_cursors WHERE family_id = $1`, h.FamilyID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	acked := map[string]int64{}
	for rows.Next() {
		var device string
		var sequence int64
		if err := rows.Scan(&device, &sequence); err != nil {
			t.Fatal(err)
		}
		acked[device] = sequence
	}
	if len(acked) != 2 || acked[before] != int64(seq) || acked[during] != int64(seq) {
		t.Fatalf("cursors %v after the stop, want both devices at %d", acked, seq)
	}
	if tracker.Len() != 0 {
		t.Fatalf("the tracker still holds %d entries", tracker.Len())
	}
}
