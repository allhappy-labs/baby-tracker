package httpapi

import (
	"crypto/sha256"
	"net/http"
	"testing"
	"time"

	"babytracker/server/internal/admission"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/diagnostics"
)

// keyed is a token's stand-in digest under each of keys, in order (the
// session service's RefreshDigests: current first).
func keyed(token string, keys ...string) []authority.KeyedDigest {
	digests := make([]authority.KeyedDigest, 0, len(keys))
	for _, key := range keys {
		digests = append(digests, authority.KeyedDigest{KeyID: key, Sum: sha256.Sum256([]byte(key + "\x00" + token))})
	}
	return digests
}

// The known set holds keyed digests. A token is known when its digest under
// any configured key is: one a sign-in issued under the current key, and
// one the database lists under a key since rotated to previous. A rotation
// forgets the parent under every key and remembers the child under the
// current key only.
func TestKnownRefreshesMatchKeyedDigestsUnderEveryKey(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	k := newKnownRefreshes(func() time.Time { return now }, nil)

	// Issued under v1 while v1 is current.
	issued := keyed("issued", "v1")
	k.issued(issued)
	if !k.known(keyed("issued", "v1")) || k.known(keyed("other", "v1")) || k.known(nil) {
		t.Fatal("issued token unknown, or another token known")
	}
	// The same token's digest under another key alone is not the stored one.
	if k.known(keyed("issued", "v2")) {
		t.Fatal("a digest under a key that did not issue the token is known")
	}
	// v2 becomes current: the token's candidates are v2 then v1, and its v1
	// digest is the one remembered.
	if !k.known(keyed("issued", "v2", "v1")) {
		t.Fatal("a token issued under the now-previous key is unknown")
	}
	// A warmed session issued under v1, presented with the v2-current
	// keyring.
	if held := k.Warm(keyed("warmed", "v1")); held != 1 {
		t.Fatalf("Warm holds %d", held)
	}
	if !k.known(keyed("warmed", "v2", "v1")) || k.known(keyed("warmed", "v2")) {
		t.Fatal("a warmed digest under the previous key is not matched by that key alone")
	}

	// Rotating the warmed parent forgets it under every key and remembers the
	// child under the current key (v2).
	child := keyed("child", "v2", "v1")
	k.rotated(keyed("warmed", "v2", "v1"), child)
	if k.known(keyed("warmed", "v2", "v1")) {
		t.Fatal("the rotated parent is still known")
	}
	if !k.known(keyed("child", "v2")) || k.known(keyed("child", "v1")) {
		t.Fatal("the child is not remembered by its current-key digest alone")
	}
	k.rotated(issued, nil)
	if k.known(issued) {
		t.Fatal("a parent rotated without a child is still known")
	}

	var none *KnownRefreshes
	if none.known(issued) || none.Warm(issued) != 0 {
		t.Fatal("a nil set knows something")
	}
	none.issued(issued)
	none.rotated(issued, child)
}

// Warm fills the previous generation, up to one generation's bound: a full
// warm leaves the current generation free for the tokens the node issues,
// the warmed digests last until the first generation passes, and a warmed
// session's child outlives them.
func TestKnownRefreshesWarmFillsThePreviousGeneration(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	k := newKnownRefreshes(func() time.Time { return now }, nil)
	k.set.limit = 3

	warm := append(append(append(keyed("a", "v1"), keyed("b", "v1")...), keyed("c", "v1")...), keyed("d", "v1")...)
	if held := k.Warm(warm); held != 3 {
		t.Fatalf("Warm holds %d of 4 over a bound of 3", held)
	}
	for _, token := range []string{"a", "b", "c"} {
		if !k.known(keyed(token, "v1")) {
			t.Fatalf("warmed %s unknown", token)
		}
	}
	if k.known(keyed("d", "v1")) {
		t.Fatal("a digest beyond the bound is known")
	}
	// The current generation is free: three issued tokens are remembered.
	for _, token := range []string{"x", "y", "z"} {
		k.issued(keyed(token, "v1"))
		if !k.known(keyed(token, "v1")) {
			t.Fatalf("issued %s not remembered after a full warm", token)
		}
	}
	if k.set.dropped.Load() != 0 {
		t.Fatalf("%d issued tokens dropped after a full warm", k.set.dropped.Load())
	}
	k.rotated(keyed("a", "v1"), keyed("a-child", "v1"))

	// One generation on: the warmed digests are gone, the issued ones (and the
	// warmed session's child) are known for one more.
	now = now.Add(knownRefreshPeriod)
	if k.known(keyed("b", "v1")) || k.known(keyed("c", "v1")) {
		t.Fatal("warmed digests outlived the first generation")
	}
	k.set.limit = 4 // room for the child among the issued tokens
	if !k.known(keyed("x", "v1")) {
		t.Fatal("an issued token was forgotten after one generation")
	}
	// The child was not remembered: the current generation was full (3 of 3)
	// when it was issued. With room, the child of a warmed session is.
	k.rotated(keyed("b", "v1"), keyed("b-child", "v1"))
	if !k.known(keyed("b-child", "v1")) {
		t.Fatal("a rotated child not remembered")
	}
	if k.Warm(nil) != 3 {
		t.Fatal("an empty warm changed the set")
	}
}

// A refresh presenting a token of a session the set was warmed with (an
// earlier process issued it) takes a Sync slot with the auth lane full; a
// token it was not warmed with takes the lane.
func TestWarmedRefreshTakesASyncSlot(t *testing.T) {
	for _, path := range []string{"/v1/auth/refresh", "/v1/auth/refresh-recoverable"} {
		t.Run(path, func(t *testing.T) {
			stub := sessionServiceStub{}
			warmed, _ := stub.RefreshDigests(laneToken("W"))
			known := NewKnownRefreshes(nil)
			if held := known.Warm(warmed); held != 1 {
				t.Fatalf("Warm holds %d", held)
			}
			s := newLaneStack(t, func(options *Options) { options.KnownRefreshes = known })
			body := laneRefreshBody
			if path == "/v1/auth/refresh-recoverable" {
				body = laneRecoverableBody
			}
			defer s.hold(t, admission.Auth)()
			if r := s.post(path, body(laneToken("R"))); !isServerBusy(t, r) {
				t.Fatalf("a token not warmed took a sync slot: %d %s", r.Code, r.Body)
			}
			if r := s.post(path, body(laneToken("W"))); r.Code != http.StatusOK {
				t.Fatalf("a warmed token with the lane full: %d %s", r.Code, r.Body)
			}
			// The warmed parent is forgotten, its child known.
			if r := s.post(path, body(laneToken("W"))); !isServerBusy(t, r) {
				t.Fatalf("the warmed parent kept a sync slot: %d %s", r.Code, r.Body)
			}
			if r := s.post(path, body(s.children[0])); r.Code != http.StatusOK {
				t.Fatalf("the warmed session's child with the lane full: %d %s", r.Code, r.Body)
			}
			if got := s.metrics.Snapshot().Admission[diagnostics.SyncAdmission]; got.Accepted != 2 {
				t.Fatalf("sync metrics %+v", got)
			}
		})
	}
}
