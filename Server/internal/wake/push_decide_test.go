package wake

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"babytracker/server/internal/apns"
)

// The decide table (Task 8 carry G1): each row's channel is chosen by its
// connection, its dueness on each channel, the target, the budget and the
// pause.
func TestDecideSplitsRowsByConnectionAndDueness(t *testing.T) {
	target := &pushTarget{env: apns.Production, token: []byte("0123456789abcdef")}
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	row := func(device string, hub, push bool, withTarget bool) lagRow {
		r := lagRow{family: "f1", device: device, sequence: 9, hubDue: hub, pushDue: push, pushedSequence: 4, pushedAt: &at}
		if withTarget {
			r.target = target
		}
		return r
	}
	type want struct {
		hub, push, wake, pushed bool
		held                    bool
	}
	for _, test := range []struct {
		name      string
		row       lagRow
		connected bool
		budget    int
		paused    bool
		want      want
	}{
		{"connected, hub-due: re-announce, mark woken", row("d", true, false, false), true, 5, false, want{hub: true, wake: true}},
		{"connected, both due: re-announce only", row("d", true, true, true), true, 5, false, want{hub: true, wake: true}},
		{"connected, only push-due: nothing", row("d", false, true, true), true, 5, false, want{}},
		{"background, push-due with a target: push, mark pushed and woken", row("d", false, true, true), false, 5, false, want{push: true, wake: true, pushed: true}},
		{"background, both due with a target: push, mark both", row("d", true, true, true), false, 5, false, want{push: true, wake: true, pushed: true}},
		{"background, hub-due, no target: mark woken only", row("d", true, true, false), false, 5, false, want{wake: true}},
		{"background, only push-due, no target: nothing", row("d", false, true, false), false, 5, false, want{}},
		{"background, hub-due, not push-due: mark woken only", row("d", true, false, true), false, 5, false, want{wake: true}},
		{"background, over budget: woken only, held", row("d", true, true, true), false, 0, false, want{wake: true, held: true}},
		{"background, over budget, only push-due: held, unmarked", row("d", false, true, true), false, 0, false, want{held: true}},
		{"background, paused: woken only, held", row("d", true, true, false), false, 5, true, want{wake: true, held: true}},
		{"background, paused, only push-due: held, unmarked", row("d", false, true, false), false, 5, true, want{held: true}},
		{"connected, paused: re-announce", row("d", true, true, false), true, 5, true, want{hub: true, wake: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := decide([]lagRow{test.row}, func(string) bool { return test.connected }, test.budget, test.paused)
			got := want{hub: len(d.hub) == 1, push: len(d.push) == 1, held: len(d.held) == 1}
			if len(d.marks) == 1 {
				got.wake, got.pushed = d.marks[0].wake, d.marks[0].push
			}
			if got != test.want || len(d.marks) > 1 {
				t.Fatalf("decide %+v, want %+v (%+v)", got, test.want, d)
			}
			if got.push {
				job := d.push[0]
				if job.sequence != 9 || job.previous != 4 || job.previousAt != &at || job.target.env != apns.Production || string(job.target.token) != string(target.token) {
					t.Fatalf("push job %+v", job)
				}
			}
		})
	}
}

// The budget goes to rows in order; each family over it is held once.
func TestDecideSpendsThePushBudgetInOrder(t *testing.T) {
	var rows []lagRow
	for i := range 5 {
		family := "f1"
		if i >= 3 {
			family = "f2"
		}
		target := &pushTarget{env: apns.Sandbox, token: fmt.Appendf(nil, "0123456789abcde%d", i)}
		rows = append(rows, lagRow{family: family, device: fmt.Sprintf("d%d", i), sequence: 3, pushDue: true, target: target})
	}
	d := decide(rows, func(string) bool { return false }, 2, false)
	if len(d.push) != 2 || d.push[0].device != "d0" || d.push[1].device != "d1" {
		t.Fatalf("pushes %+v", d.push)
	}
	if fmt.Sprint(d.held) != "[f1 f2]" || len(d.marks) != 2 {
		t.Fatalf("held %v, marks %+v", d.held, d.marks)
	}
}

// Only background push-due rows have their target looked up, and none while
// sends are paused: a connected device is reached through the hub.
func TestPushLookupsSkipConnectedAndPaused(t *testing.T) {
	rows := []lagRow{
		{family: "f1", device: "connected", pushDue: true},
		{family: "f1", device: "background", pushDue: true},
		{family: "f1", device: "hub-only", hubDue: true},
	}
	connected := func(device string) bool { return device == "connected" }
	if got := pushLookups(rows, connected, false); fmt.Sprint(got) != "[1]" {
		t.Fatalf("lookups %v, want [1] (the background push-due row only)", got)
	}
	if got := pushLookups(rows, connected, true); got != nil {
		t.Fatalf("paused lookups %v, want none", got)
	}
}

// Wake results are classified in the contract's order.
func TestClassifyFollowsTheContractOrder(t *testing.T) {
	unknown := fmt.Errorf("%w: %w", apns.ErrOutcomeUnknown, apns.ErrCauseTimeout)
	notSent := errors.Join(apns.ErrNotSent, errors.New("signing failed"))
	for _, test := range []struct {
		name   string
		result apns.Result
		err    error
		want   answer
	}{
		{"not sent (classed ConfigurationFailure by the client)", apns.Result{Class: apns.ConfigurationFailure}, notSent, answerBackoff},
		{"not sent, even with a provider-token reason", apns.Result{Status: 403, Reason: "ExpiredProviderToken", Class: apns.ConfigurationFailure}, notSent, answerBackoff},
		{"not sent: no DNS answer", apns.Result{Class: apns.ConfigurationFailure}, errors.Join(apns.ErrNotSent, apns.ErrCauseDNS), answerBackoff},
		{"not sent: a failed TLS handshake", apns.Result{Class: apns.ConfigurationFailure}, errors.Join(apns.ErrNotSent, apns.ErrCauseTLS), answerBackoff},
		{"ambiguous", apns.Result{Class: apns.Ambiguous}, unknown, answerBackoff},
		{"ambiguous 3xx", apns.Result{Status: 302, Class: apns.Ambiguous}, unknown, answerBackoff},
		{"an error with an Accepted class", apns.Result{Status: 200, Class: apns.Accepted}, unknown, answerBackoff},
		{"accepted", apns.Result{Status: 200, Class: apns.Accepted}, nil, answerAccepted},
		{"410", apns.Result{Status: 410, Reason: "Unregistered", Class: apns.InvalidToken}, nil, answerInvalidToken},
		{"400 BadDeviceToken", apns.Result{Status: 400, Reason: "BadDeviceToken", Class: apns.InvalidToken}, nil, answerInvalidToken},
		{"429", apns.Result{Status: 429, Reason: "TooManyRequests", Class: apns.Retryable}, nil, answerRetryAfter},
		{"500", apns.Result{Status: 500, Class: apns.Retryable}, nil, answerBackoff},
		{"503", apns.Result{Status: 503, Reason: "ServiceUnavailable", Class: apns.Retryable}, nil, answerBackoff},
		{"403 ExpiredProviderToken", apns.Result{Status: 403, Reason: "ExpiredProviderToken", Class: apns.ConfigurationFailure}, nil, answerProviderToken},
		{"403 InvalidProviderToken", apns.Result{Status: 403, Reason: "InvalidProviderToken", Class: apns.ConfigurationFailure}, nil, answerProviderToken},
		{"403 Forbidden", apns.Result{Status: 403, Reason: "Forbidden", Class: apns.ConfigurationFailure}, nil, answerConfiguration},
		{"400 ExpiredProviderToken is not a provider refusal", apns.Result{Status: 400, Reason: "ExpiredProviderToken", Class: apns.ConfigurationFailure}, nil, answerConfiguration},
		{"400 BadTopic", apns.Result{Status: 400, Reason: "BadTopic", Class: apns.ConfigurationFailure}, nil, answerConfiguration},
		{"an unknown class", apns.Result{Status: 200, Class: "new"}, nil, answerBackoff},
	} {
		if got := classify(test.result, test.err); got != test.want {
			t.Errorf("%s: %v, want %v", test.name, got, test.want)
		}
	}
}

// The pause table: server errors double 2 s to 2 min once a tick and an
// acceptance resets them; a 429 pauses between a minute and an hour; a
// provider-token refusal a minute; another configuration refusal five
// minutes; the longest stands.
func TestPauseAfterAnswers(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	var p pauseState
	for _, want := range []time.Duration{2, 4, 8, 16, 32, 64, 120, 120} {
		p = p.after(tickAnswers{backoff: true, accepted: true}, now)
		if p.until != now.Add(want*time.Second) || p.backoff != want*time.Second {
			t.Fatalf("backoff %+v, want %v", p, want*time.Second)
		}
	}
	if p = p.after(tickAnswers{accepted: true}, now.Add(time.Hour)); p.backoff != 0 || p.paused(now.Add(time.Hour)) {
		t.Fatalf("an acceptance did not reset the backoff: %+v", p)
	}
	if p = p.after(tickAnswers{backoff: true}, now.Add(time.Hour)); p.until != now.Add(time.Hour+minBackoff) {
		t.Fatalf("after a reset the backoff restarts at 2 s: %+v", p)
	}
	for _, test := range []struct {
		name    string
		answers tickAnswers
		want    time.Duration
	}{
		{"429 Retry-After 10 min", tickAnswers{retryAfter: true, retryUntil: now.Add(10 * time.Minute)}, 10 * time.Minute},
		{"429 Retry-After 5 s", tickAnswers{retryAfter: true, retryUntil: now.Add(5 * time.Second)}, time.Minute},
		{"429 without Retry-After", tickAnswers{retryAfter: true}, time.Minute},
		{"429 Retry-After 3 h", tickAnswers{retryAfter: true, retryUntil: now.Add(3 * time.Hour)}, time.Hour},
		{"provider token", tickAnswers{provider: true}, time.Minute},
		{"configuration", tickAnswers{configuration: true}, 5 * time.Minute},
		{"configuration and a 5xx", tickAnswers{configuration: true, backoff: true}, 5 * time.Minute},
		{"provider and 429 at 10 min", tickAnswers{provider: true, retryAfter: true, retryUntil: now.Add(10 * time.Minute)}, 10 * time.Minute},
		{"invalid tokens only", tickAnswers{}, 0},
	} {
		got := pauseState{}.after(test.answers, now)
		if want := now.Add(test.want); (test.want == 0 && !got.until.IsZero()) || (test.want > 0 && !got.until.Equal(want)) {
			t.Errorf("%s: paused until %v, want %v", test.name, got.until, want)
		}
		if got.paused(now.Add(test.want)) {
			t.Errorf("%s: still paused at the pause's end", test.name)
		}
	}
	// A configuration pause outlives a later, shorter one.
	long := pauseState{}.after(tickAnswers{configuration: true}, now)
	if after := long.after(tickAnswers{provider: true}, now.Add(time.Second)); !after.until.Equal(long.until) {
		t.Fatalf("a shorter pause cut a longer one: %+v", after)
	}
}
