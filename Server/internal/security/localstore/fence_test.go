package localstore

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/diagnostics"
	"babytracker/server/internal/security"
)

func TestFenceHonorsCancellation(t *testing.T) {
	store := initializedStore(t)
	release, err := store.Fence.Write(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := store.Fence.Read(ctx); err == nil {
		t.Fatal("blocked reader ignored cancellation")
	}
}

func TestIndependentProcessHeadCAS(t *testing.T) {
	if os.Getenv("BABY_TRACKER_CAS_CHILD") == "1" {
		runCASChild(t)
		return
	}
	store := initializedStore(t)
	root := filepath.Dir(store.ledger.dir)
	barrier := filepath.Join(root, "go")
	commands := make([]*exec.Cmd, 2)
	outputs := make([]bytes.Buffer, 2)
	for index := range commands {
		ready := filepath.Join(root, "ready-"+string(rune('0'+index)))
		result := filepath.Join(root, "result-"+string(rune('0'+index)))
		command := exec.Command(os.Args[0], "-test.run=^TestIndependentProcessHeadCAS$")
		command.Env = append(os.Environ(),
			"BABY_TRACKER_CAS_CHILD=1", "BABY_TRACKER_CAS_ROOT="+root,
			"BABY_TRACKER_CAS_READY="+ready, "BABY_TRACKER_CAS_RESULT="+result, "BABY_TRACKER_CAS_BARRIER="+barrier)
		command.Stdout = &outputs[index]
		command.Stderr = &outputs[index]
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		commands[index] = command
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		matches, _ := filepath.Glob(filepath.Join(root, "ready-*"))
		if len(matches) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("children did not reach CAS barrier")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := os.WriteFile(barrier, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	for index, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("child: %v: %s", err, outputs[index].String())
		}
	}
	results, _ := filepath.Glob(filepath.Join(root, "result-*"))
	winners := 0
	for _, path := range results {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(data)) == "true" {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("CAS winners=%d, want 1", winners)
	}
	head, err := store.Heads.Load(t.Context())
	if err != nil || head.Revision != 1 {
		t.Fatalf("winner head=%+v err=%v", head, err)
	}
}

func runCASChild(t *testing.T) {
	root := os.Getenv("BABY_TRACKER_CAS_ROOT")
	store, err := Open(filepath.Join(root, "ledger"), filepath.Join(root, "head"))
	if err != nil {
		t.Fatal(err)
	}
	expected, err := store.Heads.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("BABY_TRACKER_CAS_READY"), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(os.Getenv("BABY_TRACKER_CAS_BARRIER")); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	next := security.Head{Generation: 1, Revision: 1}
	next.EventHash[31] = 1
	swapped, err := store.Heads.CompareAndSwap(t.Context(), expected, next)
	if err != nil {
		t.Fatal(err)
	}
	if !swapped {
		head, err := store.Heads.Load(t.Context())
		if err != nil || head != next {
			t.Fatalf("loser revalidation: head=%+v err=%v", head, err)
		}
	}
	if err := os.WriteFile(os.Getenv("BABY_TRACKER_CAS_RESULT"), []byte(map[bool]string{true: "true", false: "false"}[swapped]), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFenceMetricFlushReentersOnlyAfterRelease(t *testing.T) {
	store := initializedStore(t)
	calls := 0
	ctx := diagnostics.WithRecorder(t.Context(), func(_ context.Context, phase diagnostics.Phase, elapsed time.Duration) {
		calls++
		if phase != diagnostics.SecurityFence || elapsed < 0 {
			t.Fatal("invalid fence sample")
		}
		deadline, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		release, err := store.Fence.Write(deadline)
		if err != nil {
			t.Fatal("observer reentry held fence", err)
		}
		release()
	})
	release, err := store.Fence.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("callback under fence")
	}
	release()
	diagnostics.Flush(ctx)
	if calls != 1 {
		t.Fatalf("fence observations=%d", calls)
	}
}
