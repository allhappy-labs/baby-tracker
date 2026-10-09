package testkit

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"babytracker/server/internal/protocol"
)

func MustMutation(t *testing.T, fixture string) protocol.Mutation {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve fixture root")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "api", "fixtures", "v1", fixture))
	if err != nil {
		t.Fatal(err)
	}
	mutation, err := protocol.DecodeMutation(raw)
	if err != nil {
		t.Fatal(err)
	}
	return mutation
}
