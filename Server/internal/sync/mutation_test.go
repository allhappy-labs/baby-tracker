package sync

import (
	"testing"

	"babytracker/server/internal/protocol"
)

func TestActionForMutationUsesNarrowAuthority(t *testing.T) {
	for _, test := range []struct{ kind, operation, want string }{
		{"baby", "create", "add_baby"}, {"baby", "update", "mutate_record"},
		{"baby", "delete", "delete_baby"}, {"feed", "delete", "mutate_record"},
	} {
		got, err := actionFor(protocol.Mutation{EntityKind: test.kind, Operation: test.operation})
		if err != nil || got != test.want {
			t.Fatalf("%s/%s action=%q err=%v", test.kind, test.operation, got, err)
		}
	}
}
