//go:build identitytest

package protocol_test

import (
	"testing"

	"babytracker/server/internal/protocol"
)

func TestServerRestartReconcilesWithoutMemoryHint(t *testing.T) {
	fixture := newProtocolFixture(t)
	applied, err := fixture.Owner().Sync([]protocol.Mutation{mutation(t, "baby.json", fixture.h.CommandID())})
	if err != nil || len(applied.Outcomes) != 1 {
		t.Fatalf("apply=%+v err=%v", applied, err)
	}
	fixture.h.RestartServices(t)
	fixture.startServer()
	fixture.caregiver.cursor = nil
	pulled, err := fixture.Caregiver().Sync(nil)
	if err != nil || len(pulled.Changes) != 1 || pulled.Changes[0].EntityKind != "baby" {
		t.Fatalf("pull=%+v err=%v", pulled, err)
	}
}
