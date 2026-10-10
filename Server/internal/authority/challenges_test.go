package authority_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"babytracker/server/internal/identity"
)

func TestChallengeIsConsumedOnceBeforeExpiry(t *testing.T) {
	f := authoritytest.New(t)
	digest := [32]byte{7}
	if err := f.Service.CreateChallenge(t.Context(), digest, f.Now.Add(5*time.Minute), f.Now); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.Service.CreateChallenge(t.Context(), digest, f.Now.Add(5*time.Minute), f.Now), "service_unavailable")
	wantCode(t, f.Service.CreateChallenge(t.Context(), [32]byte{8}, f.Now.Add(6*time.Minute), f.Now), "service_unavailable")
	if err := f.Service.ConsumeChallenge(t.Context(), digest, f.Now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.Service.ConsumeChallenge(t.Context(), digest, f.Now.Add(time.Minute)), "invalid_or_expired_challenge")
	wantCode(t, f.Service.ConsumeChallenge(t.Context(), [32]byte{9}, f.Now), "invalid_or_expired_challenge")

	expiring := [32]byte{10}
	if err := f.Service.CreateChallenge(t.Context(), expiring, f.Now.Add(5*time.Minute), f.Now); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.Service.ConsumeChallenge(t.Context(), expiring, f.Now.Add(5*time.Minute)), "invalid_or_expired_challenge")
	if n := count(t, f, `SELECT count(*) FROM auth_challenges WHERE token_digest=$1 AND consumed_at=$2`, digest[:], f.Now.Add(time.Minute)); n != 1 {
		t.Fatal("consumption not recorded")
	}
}

func TestReserveAppleNotificationValidatesCandidates(t *testing.T) {
	f := authoritytest.New(t)
	command := authoritytest.NewID(t)
	for name, candidates := range map[string][]identity.KeyedDigest{
		"none":          nil,
		"duplicate key": {{KeyID: "n.v1", Sum: [32]byte{1}}, {KeyID: "n.v1", Sum: [32]byte{2}}},
		"bad key":       {{KeyID: "N", Sum: [32]byte{1}}},
	} {
		if _, _, err := f.Service.ReserveAppleNotification(t.Context(), candidates, command, f.Now); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, _, err := f.Service.ReserveAppleNotification(t.Context(), []identity.KeyedDigest{{KeyID: "n.v1", Sum: [32]byte{1}}}, "nope", f.Now); err == nil {
		t.Error("malformed command ID accepted")
	}
}

// Ported from database TestAuthChallengeHasExactlyOneWinnerAcross100Consumers
// (app.consume_auth_challenge is gone): 100 concurrent consumers of one
// challenge produce exactly one winner; every loser is
// invalid_or_expired_challenge.
func TestAuthChallengeHasExactlyOneWinnerAcross100Consumers(t *testing.T) {
	f := authoritytest.New(t)
	digest := [32]byte{0x51}
	if err := f.Service.CreateChallenge(t.Context(), digest, f.Now.Add(5*time.Minute), f.Now); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wins, unexpected atomic.Int32
	var wait sync.WaitGroup
	for range 100 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			err := f.Service.ConsumeChallenge(context.Background(), digest, f.Now.Add(time.Second))
			switch {
			case err == nil:
				wins.Add(1)
			case authority.CodeOf(err) != "invalid_or_expired_challenge":
				unexpected.Add(1)
			}
		}()
	}
	close(start)
	wait.Wait()
	if wins.Load() != 1 || unexpected.Load() != 0 {
		t.Fatalf("challenge wins=%d unexpected=%d", wins.Load(), unexpected.Load())
	}
}

// Ported from database TestReserveAppleNotificationRetainedKeysAtomicAmbiguityAndRestore
// (app.reserve_apple_notification is gone): a reservation made under a
// retired notification key is found again through the rotated candidate list
// without a second row; two retained-key matches are ambiguous and reserve
// nothing; and after a restore loses the row the notification reserves again
// (the command receipt, not this table, is the final authority). Concurrent
// convergence: TestReserveAppleNotificationConcurrentReservationsConverge.
func TestReserveAppleNotificationRetainedKeysAtomicAmbiguityAndRestore(t *testing.T) {
	f := authoritytest.New(t)
	previous, current := [32]byte{0x61}, [32]byte{0x62}
	first := authoritytest.NewID(t)
	command, newly, err := f.Service.ReserveAppleNotification(t.Context(), []identity.KeyedDigest{{KeyID: "notify.v1", Sum: previous}}, first, f.Now)
	if err != nil || command != first || !newly {
		t.Fatalf("first reservation %s newly=%t err=%v", command, newly, err)
	}
	rotated := []identity.KeyedDigest{{KeyID: "notify.v2", Sum: current}, {KeyID: "notify.v1", Sum: previous}}
	command, newly, err = f.Service.ReserveAppleNotification(t.Context(), rotated, authoritytest.NewID(t), f.Now.Add(time.Second))
	if err != nil || command != first || newly {
		t.Fatalf("retained-key reservation %s newly=%t err=%v", command, newly, err)
	}
	if n := count(t, f, `SELECT count(*) FROM apple_notifications`); n != 1 {
		t.Fatalf("%d notification rows after a retained-key retry", n)
	}

	secondOld := [32]byte{0x63}
	if _, _, err := f.Service.ReserveAppleNotification(t.Context(), []identity.KeyedDigest{{KeyID: "notify.v4", Sum: secondOld}}, authoritytest.NewID(t), f.Now); err != nil {
		t.Fatal(err)
	}
	ambiguousCurrent := [32]byte{0x64}
	ambiguous := []identity.KeyedDigest{{KeyID: "notify.v5", Sum: ambiguousCurrent}, {KeyID: "notify.v1", Sum: previous}, {KeyID: "notify.v4", Sum: secondOld}}
	if _, _, err := f.Service.ReserveAppleNotification(t.Context(), ambiguous, authoritytest.NewID(t), f.Now); err == nil {
		t.Fatal("ambiguous retained-key reservation accepted")
	}
	if n := count(t, f, `SELECT count(*) FROM apple_notifications WHERE notification_key_id='notify.v5'`); n != 0 {
		t.Fatalf("ambiguous reservation inserted %d current rows", n)
	}

	if f.Exec(t, `DELETE FROM apple_notifications WHERE notification_key_id='notify.v1' AND notification_digest=$1`, previous[:]) != 1 {
		t.Fatal("restore simulation deleted nothing")
	}
	restored := authoritytest.NewID(t)
	command, newly, err = f.Service.ReserveAppleNotification(t.Context(), []identity.KeyedDigest{{KeyID: "notify.v1", Sum: previous}}, restored, f.Now.Add(2*time.Second))
	if err != nil || command != restored || !newly {
		t.Fatalf("post-restore reservation %s newly=%t err=%v", command, newly, err)
	}
}
