package httpapi

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// A remembered bearer is known for at least one period and at most two; a
// generation holds at most its limit, and a forgotten bearer is unknown at
// once.
func TestKnownBearersRememberForOneToTwoPeriods(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	k := newKnownBearers(2, func() time.Time { return now }, nil)
	a, b, c := [32]byte{1}, [32]byte{2}, [32]byte{3}
	if k.known(a) {
		t.Fatal("an unseen bearer is known")
	}
	k.remember(a)
	k.remember(b)
	k.remember(c) // past the generation's limit: not remembered
	if !k.known(a) || !k.known(b) || k.known(c) {
		t.Fatalf("after remembering: a %v b %v c %v, want true true false", k.known(a), k.known(b), k.known(c))
	}
	now = now.Add(knownBearerPeriod) // a and b move to the previous generation
	k.remember(c)
	if !k.known(a) || !k.known(c) {
		t.Fatal("a bearer remembered one period ago is forgotten")
	}
	k.forget(a)
	if k.known(a) {
		t.Fatal("a forgotten bearer is known")
	}
	now = now.Add(knownBearerPeriod)
	if k.known(b) || !k.known(c) {
		t.Fatalf("two periods on: b %v c %v, want false true", k.known(b), k.known(c))
	}
	now = now.Add(2 * knownBearerPeriod)
	if k.known(c) {
		t.Fatal("a bearer is known after more than two periods")
	}
}

// Review M4: the checks are counted, and a full generation is visible:
// counted per bearer it could not remember, and logged once a generation.
func TestKnownBearersCountChecksAndAFullGeneration(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	logs := &syncBuffer{}
	k := newKnownBearers(1, func() time.Time { return now }, slog.New(slog.NewJSONHandler(logs, nil)))
	k.checked()
	k.checked()
	k.remember([32]byte{1})
	k.remember([32]byte{2})
	k.remember([32]byte{3})
	if checks, dropped := k.stats(); checks != 2 || dropped != 2 {
		t.Fatalf("stats: %d checks, %d dropped, want 2 and 2", checks, dropped)
	}
	if lines := strings.Count(logs.String(), `"msg":"known bearers full"`); lines != 1 {
		t.Fatalf("%d full-generation lines in one generation, want 1: %s", lines, logs.String())
	}
	now = now.Add(knownBearerPeriod)
	k.remember([32]byte{4})
	k.remember([32]byte{5})
	if lines := strings.Count(logs.String(), `"msg":"known bearers full"`); lines != 2 {
		t.Fatalf("%d full-generation lines over two generations, want 2: %s", lines, logs.String())
	}
	if !strings.Contains(logs.String(), `"limit":1`) || !strings.Contains(logs.String(), `"checks":2`) {
		t.Fatalf("the line names the limit and the checks so far: %s", logs.String())
	}
}

// stats is the checks made and the bearers not remembered (a full
// generation) since the start.
func (k *knownBearers) stats() (checks, dropped uint64) {
	return k.checks.Load(), k.dropped.Load()
}
