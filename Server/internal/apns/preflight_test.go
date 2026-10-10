package apns_test

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/apns"
)

func TestPreflightClassifiesApplesAnswers(t *testing.T) {
	for _, c := range []struct {
		name    string
		answer  func(w http.ResponseWriter, r *http.Request)
		outcome apns.PreflightOutcome
		status  int
		reason  string
	}{
		{"400 BadDeviceToken", reasonAnswer(400, "BadDeviceToken"), apns.PreflightPass, 400, "BadDeviceToken"},
		{"403 InvalidProviderToken", reasonAnswer(403, "InvalidProviderToken"), apns.PreflightBadKey, 403, "InvalidProviderToken"},
		{"403 BadEnvironmentKeyIdInToken", reasonAnswer(403, "BadEnvironmentKeyIdInToken"), apns.PreflightBadKey, 403, "BadEnvironmentKeyIdInToken"},
		{"403 UnrelatedKeyIdInToken", reasonAnswer(403, "UnrelatedKeyIdInToken"), apns.PreflightBadKey, 403, "UnrelatedKeyIdInToken"},
		{"400 TopicDisallowed", reasonAnswer(400, "TopicDisallowed"), apns.PreflightTopic, 400, "TopicDisallowed"},
		{"400 BadTopic", reasonAnswer(400, "BadTopic"), apns.PreflightTopic, 400, "BadTopic"},
		{"400 DeviceTokenNotForTopic", reasonAnswer(400, "DeviceTokenNotForTopic"), apns.PreflightTopic, 400, "DeviceTokenNotForTopic"},
		{"connection closed", func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }, apns.PreflightUnreachable, 0, "connection"},
		{"500", reasonAnswer(500, "InternalServerError"), apns.PreflightUnexpected, 500, "InternalServerError"},
		{"200", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }, apns.PreflightUnexpected, 200, ""},
		{"403 Forbidden", reasonAnswer(403, "Forbidden"), apns.PreflightUnexpected, 403, "Forbidden"},
		{"410 BadDeviceToken", reasonAnswer(410, "BadDeviceToken"), apns.PreflightUnexpected, 410, "BadDeviceToken"},
		{"429", reasonAnswer(429, "TooManyRequests"), apns.PreflightUnexpected, 429, "TooManyRequests"},
		{"302", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://elsewhere.example/")
			w.WriteHeader(http.StatusFound)
		}, apns.PreflightUnexpected, 302, ""},
	} {
		// The classification is the same for both push types.
		for _, pushType := range []string{apns.PushBackground, apns.PushLiveActivity} {
			t.Run(pushType+" "+c.name, func(t *testing.T) {
				server := fakeAPNs(t, c.answer)
				creds, _ := testCredentials(t)
				now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
				client, err := apns.NewWithTransport(creds, server.Client().Transport, server.URL, server.URL, func() time.Time { return now })
				if err != nil {
					t.Fatal(err)
				}
				result, err := client.Preflight(t.Context(), apns.Production, pushType)
				if err != nil || result.Outcome != c.outcome || result.Status != c.status || result.Reason != c.reason {
					t.Fatalf("%+v %v, want %s %d %q", result, err, c.outcome, c.status, c.reason)
				}
				requests := server.seen()
				if len(requests) != 1 {
					t.Fatalf("%d requests, want exactly one", len(requests))
				}
				checkPreflightRequest(t, requests[0], creds.BundleID, pushType, now)
			})
		}
	}
}

// checkPreflightRequest asserts the one preflight push of pushType: the
// all-zero 32-byte token, and for background WakePayload at priority 5 to the
// bundle topic, for Live Activity main's quiet update at priority 10 to the
// .push-type.liveactivity topic; both expire a minute from now.
func checkPreflightRequest(t *testing.T, got seen, bundleID, pushType string, now time.Time) {
	t.Helper()
	want := map[string]string{"apns-push-type": "background", "apns-topic": bundleID, "apns-priority": "5",
		"apns-expiration": strconv.FormatInt(now.Add(time.Minute).Unix(), 10)}
	body := apns.WakePayload
	if pushType == apns.PushLiveActivity {
		want["apns-push-type"], want["apns-topic"], want["apns-priority"] = "liveactivity", bundleID+".push-type.liveactivity", "10"
		body = fmt.Sprintf(`{"aps":{"content-state":{"startedAt":%d,"sideLabel":"","activityKind":"paused","phase":"running","parts":[],"omittedParts":0,`+
			`"remote":{"scope":{"serverInstanceID":"0f1e2d3c-4b5a-4968-8778-695a4b3c2d1e","recoveryEpoch":1,"familyID":"0f1e2d3c-4b5a-4968-8778-695a4b3c2d1e",`+
			`"babyID":"0f1e2d3c-4b5a-4968-8778-695a4b3c2d1e","kindRaw":"feed"},"generationID":"0f1e2d3c-4b5a-4968-8778-695a4b3c2d1e","familySequence":0,`+
			`"presentationVersion":1,"presentationTimestamp":%d,"canWrite":false}},"event":"update","timestamp":%d}}`,
			now.Unix()-978307200, now.Unix(), now.Unix())
	}
	if got.path != "/3/device/"+strings.Repeat("0", 64) || string(got.body) != body {
		t.Fatalf("path %s body %s, want %s", got.path, got.body, body)
	}
	for name, value := range want {
		if v := got.headers.Get(name); v != value {
			t.Fatalf("%s = %q, want %q", name, v, value)
		}
	}
}

// A preflight that never reached Apple is unreachable with its coarse cause;
// an environment or push type it does not know is an error.
func TestPreflightReportsNoConnectionAsUnreachable(t *testing.T) {
	creds, _ := testCredentials(t)
	server := fakeAPNs(t, reasonAnswer(400, "BadDeviceToken"))
	// The default transport does not trust the test server's certificate.
	client, err := apns.NewWithTransport(creds, &http.Transport{ForceAttemptHTTP2: true}, server.URL, server.URL, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Preflight(t.Context(), apns.Sandbox, apns.PushLiveActivity)
	if err != nil || result.Outcome != apns.PreflightUnreachable || result.Status != 0 || result.Reason != apns.ErrCauseTLS.Error() {
		t.Fatalf("%+v %v", result, err)
	}
	if _, err := client.Preflight(t.Context(), apns.Environment("staging"), apns.PushBackground); err == nil || !errors.Is(err, apns.ErrNotSent) {
		t.Fatalf("staging: %v", err)
	}
	for _, pushType := range []string{"", "alert", "voip"} {
		if _, err := client.Preflight(t.Context(), apns.Sandbox, pushType); err == nil || !errors.Is(err, apns.ErrNotSent) {
			t.Fatalf("push type %q: %v", pushType, err)
		}
	}
	if len(server.seen()) != 0 {
		t.Fatalf("%d requests reached the server", len(server.seen()))
	}
}

func reasonAnswer(status int, reason string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"reason":%q}`, reason)
	}
}
