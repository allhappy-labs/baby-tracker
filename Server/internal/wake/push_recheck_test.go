package wake

import (
	"errors"
	"fmt"
	"testing"

	"babytracker/server/internal/apns"
)

// A connected device re-announced through the hub while a push is also owed
// to it rechecks its family (review I1); one only push-due, or only hub-due,
// does not.
func TestDecideRechecksAConnectedDeviceOwedAPush(t *testing.T) {
	rows := []lagRow{
		{family: "f1", device: "both", hubDue: true, pushDue: true},
		{family: "f2", device: "push-only", pushDue: true},
		{family: "f3", device: "hub-only", hubDue: true},
	}
	d := decide(rows, func(string) bool { return true }, 5, false)
	if fmt.Sprint(d.recheck) != "[f1]" || len(d.hub) != 2 || len(d.push) != 0 {
		t.Fatalf("recheck %v, hub %d, push %d: want f1 rechecked, two hub wakes, no push", d.recheck, len(d.hub), len(d.push))
	}
	if d := decide(rows, func(string) bool { return false }, 5, false); len(d.recheck) != 0 {
		t.Fatalf("background rows rechecked: %v", d.recheck)
	}
}

// Rows sharing a target (environment and token) share one push: the first
// row sends, the others are reserved with it and spend no budget; the same
// token in another environment is another target (review M1).
func TestDecideSharesOnePushPerTarget(t *testing.T) {
	phone := []byte("0123456789abcdef")
	rows := []lagRow{
		{family: "f1", device: "d1", sequence: 7, pushDue: true, target: &pushTarget{env: apns.Production, token: phone}},
		{family: "f2", device: "d1", sequence: 3, pushDue: true, target: &pushTarget{env: apns.Production, token: phone}},
		{family: "f1", device: "d2", sequence: 7, pushDue: true, target: &pushTarget{env: apns.Production, token: phone}},
		{family: "f1", device: "d3", sequence: 7, pushDue: true, target: &pushTarget{env: apns.Sandbox, token: phone}},
	}
	d := decide(rows, func(string) bool { return false }, 2, false)
	if len(d.push) != 2 || len(d.push[0].shared) != 2 || d.push[1].device != "d3" || len(d.held) != 0 {
		t.Fatalf("pushes %+v held %v: want one production send sharing two rows and one sandbox send", d.push, d.held)
	}
	if rowsOf(d.push)[2].family != "f1" || rowsOf(d.push)[1].sequence != 3 || len(rowsOf(d.push)) != 4 {
		t.Fatalf("rows %+v", rowsOf(d.push))
	}
	if len(d.marks) != 4 {
		t.Fatalf("marks %+v, want every row reserved", d.marks)
	}
	for _, mark := range d.marks {
		if !mark.push || !mark.wake {
			t.Fatalf("mark %+v, want pushed and woken", mark)
		}
	}
	// Over the budget, a row sharing a target already sent still rides along.
	d = decide(rows[:3], func(string) bool { return false }, 1, false)
	if len(d.push) != 1 || len(d.push[0].shared) != 2 || len(d.held) != 0 {
		t.Fatalf("budget 1: pushes %+v held %v", d.push, d.held)
	}
	// A shared row spends none of the budget: the next target is still sent.
	other := lagRow{family: "f9", device: "d9", sequence: 7, pushDue: true, target: &pushTarget{env: apns.Production, token: []byte("fedcba9876543210")}}
	d = decide(append(rows[:3:3], rows[3], other), func(string) bool { return false }, 2, false)
	if len(d.push) != 2 || d.push[1].device != "d3" || fmt.Sprint(d.held) != "[f9]" {
		t.Fatalf("budget 2: pushes %+v held %v, want the production and sandbox sends and f9 held", d.push, d.held)
	}
}

// Only a result proving Apple did not deliver owes the sequence again.
func TestUndeliveredOnlyForProvenNonDelivery(t *testing.T) {
	for _, test := range []struct {
		result apns.Result
		err    error
		want   bool
	}{
		{apns.Result{Status: 200, Class: apns.Accepted}, nil, false},
		{apns.Result{Status: 410, Class: apns.InvalidToken}, nil, false},
		{apns.Result{Class: apns.Ambiguous}, apns.ErrOutcomeUnknown, false},
		{apns.Result{Status: 503, Class: apns.Retryable}, apns.ErrOutcomeUnknown, false},
		{apns.Result{Class: apns.ConfigurationFailure}, apns.ErrNotSent, true},
		// What the client reports for a request that never reached the wire
		// (final review R2-I2), and for the same failure once it may have.
		{apns.Result{Class: apns.ConfigurationFailure}, errors.Join(apns.ErrNotSent, apns.ErrCauseDNS), true},
		{apns.Result{Class: apns.ConfigurationFailure}, errors.Join(apns.ErrNotSent, apns.ErrCauseConnection), true},
		{apns.Result{Class: apns.ConfigurationFailure}, errors.Join(apns.ErrNotSent, apns.ErrCauseTLS), true},
		{apns.Result{Class: apns.Ambiguous}, fmt.Errorf("%w: %w", apns.ErrOutcomeUnknown, apns.ErrCauseConnection), false},
		{apns.Result{Status: 429, Class: apns.Retryable}, nil, true},
		{apns.Result{Status: 503, Class: apns.Retryable}, nil, true},
		{apns.Result{Status: 403, Reason: "InvalidProviderToken", Class: apns.ConfigurationFailure}, nil, true},
		{apns.Result{Status: 400, Reason: "BadTopic", Class: apns.ConfigurationFailure}, nil, true},
	} {
		if got := undelivered(test.result, test.err); got != test.want {
			t.Errorf("undelivered(%+v, %v) = %v, want %v", test.result, test.err, got, test.want)
		}
	}
}
