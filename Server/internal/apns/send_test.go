package apns_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/apns"
	"babytracker/server/internal/protocol"
)

func TestSendLiveActivityHeadersAndTopic(t *testing.T) {
	server := fakeAPNs(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	creds, key := testCredentials(t)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	client, err := apns.NewWithTransport(creds, server.Client().Transport, server.URL, server.URL, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	token := bytes.Repeat([]byte{0x5c}, 80) // a Live Activity token is longer than a device token
	payload := []byte(`{"aps":{"event":"start","timestamp":1}}`)
	result, err := client.Send(t.Context(), apns.Request{Environment: apns.Sandbox, Token: token, PushType: apns.PushLiveActivity, Priority: 10,
		Expiration: 0, APNsID: "6f9619ff-8b86-4011-b42d-00c04fc964ff", Payload: payload})
	if err != nil || result.Class != apns.Accepted || result.Status != http.StatusOK {
		t.Fatalf("%+v %v", result, err)
	}
	requests := server.seen()
	if len(requests) != 1 {
		t.Fatalf("%d requests, want 1", len(requests))
	}
	got := requests[0]
	for name, want := range map[string]string{"apns-push-type": "liveactivity", "apns-topic": creds.BundleID + ".push-type.liveactivity",
		"apns-priority": "10", "apns-expiration": "0", "apns-id": "6f9619ff-8b86-4011-b42d-00c04fc964ff", "content-type": "application/json"} {
		if v := got.headers.Get(name); v != want {
			t.Fatalf("%s = %q, want %q", name, v, want)
		}
	}
	if got.proto != 2 || got.path != "/3/device/"+hex.EncodeToString(token) || !bytes.Equal(got.body, payload) {
		t.Fatalf("proto %d path %s body %s", got.proto, got.path, got.body)
	}
	if jwt := decodeProviderToken(t, got.headers.Get("authorization"), key); !jwt.valid {
		t.Fatalf("provider token %+v", jwt)
	}
}

// A background Send keeps the bare bundle ID as its topic, sends the
// caller's expiration and payload, and draws a fresh apns-id when none is
// given.
func TestSendBackgroundKeepsTheBundleTopic(t *testing.T) {
	server := fakeAPNs(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	creds, _ := testCredentials(t)
	client, err := apns.NewWithTransport(creds, server.Client().Transport, server.URL, server.URL, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"aps":{"content-available":1},"x":1}`)
	for range 2 {
		if result, err := client.Send(t.Context(), apns.Request{Environment: apns.Production, Token: []byte{7}, PushType: apns.PushBackground,
			Priority: 5, Expiration: 1767225600, Payload: payload}); err != nil || result.Class != apns.Accepted {
			t.Fatalf("%+v %v", result, err)
		}
	}
	requests := server.seen()
	for name, want := range map[string]string{"apns-push-type": "background", "apns-topic": creds.BundleID, "apns-priority": "5",
		"apns-expiration": strconv.FormatInt(1767225600, 10)} {
		if v := requests[0].headers.Get(name); v != want {
			t.Fatalf("%s = %q, want %q", name, v, want)
		}
	}
	first, second := requests[0].headers.Get("apns-id"), requests[1].headers.Get("apns-id")
	if !protocol.ValidUUID(first) || first[14] != '4' || first == second {
		t.Fatalf("apns-id %q then %q, want fresh version-4 UUIDs", first, second)
	}
	if !bytes.Equal(requests[0].body, payload) || requests[0].path != "/3/device/07" {
		t.Fatalf("path %s body %s", requests[0].path, requests[0].body)
	}
}

func TestSendRefusesBeforeSending(t *testing.T) {
	server := fakeAPNs(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	creds, _ := testCredentials(t)
	client, err := apns.NewWithTransport(creds, server.Client().Transport, server.URL, server.URL, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	ok := apns.Request{Environment: apns.Production, Token: []byte{1}, PushType: apns.PushLiveActivity, Priority: 5, Expiration: 1, Payload: []byte(`{}`)}
	for name, mutate := range map[string]func(*apns.Request){
		"empty token":     func(r *apns.Request) { r.Token = nil },
		"token too long":  func(r *apns.Request) { r.Token = make([]byte, 1025) },
		"push type":       func(r *apns.Request) { r.PushType = "alert" },
		"no push type":    func(r *apns.Request) { r.PushType = "" },
		"priority":        func(r *apns.Request) { r.Priority = 7 },
		"no priority":     func(r *apns.Request) { r.Priority = 0 },
		"negative expiry": func(r *apns.Request) { r.Expiration = -1 },
		"payload too big": func(r *apns.Request) {
			r.Payload = append([]byte(`{"x":"`), append(bytes.Repeat([]byte{'a'}, 4096), '"', '}')...)
		},
		"no payload":         func(r *apns.Request) { r.Payload = nil },
		"not json":           func(r *apns.Request) { r.Payload = []byte(`[1]`) },
		"json null":          func(r *apns.Request) { r.Payload = []byte(`null`) },
		"broken json":        func(r *apns.Request) { r.Payload = []byte(`{"aps":`) },
		"apns-id":            func(r *apns.Request) { r.APNsID = "NOT-A-UUID" },
		"upper-case apns-id": func(r *apns.Request) { r.APNsID = "6F9619FF-8B86-4011-B42D-00C04FC964FF" },
		"environment":        func(r *apns.Request) { r.Environment = "staging" },
	} {
		r := ok
		mutate(&r)
		result, err := client.Send(t.Context(), r)
		if !errors.Is(err, apns.ErrNotSent) || errors.Is(err, apns.ErrOutcomeUnknown) || result.Class == apns.Accepted || result.Class == apns.Ambiguous {
			t.Fatalf("%s: %+v %v, want ErrNotSent", name, result, err)
		}
	}
	if len(server.seen()) != 0 {
		t.Fatalf("%d requests reached the server", len(server.seen()))
	}
	// The bounds themselves are accepted.
	for name, mutate := range map[string]func(*apns.Request){
		"the request as is": func(*apns.Request) {},
		"a 1024-byte token": func(r *apns.Request) { r.Token = make([]byte, 1024) },
		"a 4096-byte object": func(r *apns.Request) {
			r.Payload = append([]byte(`{"x":"`), append(bytes.Repeat([]byte{'a'}, 4088), '"', '}')...)
		},
		"priority 10": func(r *apns.Request) { r.Priority = 10 },
		"background":  func(r *apns.Request) { r.PushType = apns.PushBackground },
	} {
		r := ok
		mutate(&r)
		if result, err := client.Send(t.Context(), r); err != nil || result.Class != apns.Accepted {
			t.Fatalf("%s: %+v %v", name, result, err)
		}
	}
}

// Wake is Send with the background shape: the device-token bound 16..256
// bytes is Wake's own, so Send accepts tokens Wake refuses.
func TestWakeIsSendWithTheBackgroundShape(t *testing.T) {
	server := fakeAPNs(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	creds, _ := testCredentials(t)
	client, err := apns.NewWithTransport(creds, server.Client().Transport, server.URL, server.URL, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{15, 257} {
		token := bytes.Repeat([]byte{9}, size)
		if _, err := client.Wake(t.Context(), apns.Sandbox, token); !errors.Is(err, apns.ErrNotSent) {
			t.Fatalf("Wake with a %d-byte token: %v, want ErrNotSent", size, err)
		}
		if len(server.seen()) != 0 {
			t.Fatalf("Wake with a %d-byte token reached the server", size)
		}
	}
	for index, size := range []int{15, 257} {
		token := bytes.Repeat([]byte{9}, size)
		result, err := client.Send(t.Context(), apns.Request{Environment: apns.Sandbox, Token: token, PushType: apns.PushBackground, Priority: 5,
			Expiration: 1, Payload: []byte(apns.WakePayload)})
		if err != nil || result.Class != apns.Accepted {
			t.Fatalf("Send with a %d-byte token: %+v %v", size, result, err)
		}
		if got := server.seen(); len(got) != index+1 || got[index].path != "/3/device/"+hex.EncodeToString(token) {
			t.Fatalf("Send with a %d-byte token: %d requests", size, len(got))
		}
	}
}

// liveActivityRequest is a Live Activity update to a 100-byte token.
func liveActivityRequest(env apns.Environment) apns.Request {
	return apns.Request{Environment: env, Token: bytes.Repeat([]byte{0x7e}, 100), PushType: apns.PushLiveActivity, Priority: 10,
		Expiration: 1767225600, Payload: []byte(`{"aps":{"event":"update","timestamp":1}}`)}
}

// A Live Activity push is classified exactly as a wake: a dead card token is
// InvalidToken, a topic reason is the deployment's, Retry-After is read.
func TestSendLiveActivityClassifiesApplesAnswers(t *testing.T) {
	for _, c := range []struct {
		status     int
		reason     string
		retryAfter string
		class      apns.Class
	}{
		{410, "Unregistered", "", apns.InvalidToken},
		{400, "BadDeviceToken", "", apns.InvalidToken},
		{410, "ExpiredToken", "", apns.InvalidToken},
		{400, "TopicDisallowed", "", apns.ConfigurationFailure},
		{400, "DeviceTokenNotForTopic", "", apns.ConfigurationFailure},
		{429, "TooManyRequests", "30", apns.Retryable},
		{503, "ServiceUnavailable", "", apns.Retryable},
	} {
		server := fakeAPNs(t, func(w http.ResponseWriter, _ *http.Request) {
			if c.retryAfter != "" {
				w.Header().Set("Retry-After", c.retryAfter)
			}
			w.WriteHeader(c.status)
			fmt.Fprintf(w, `{"reason":%q}`, c.reason)
		})
		creds, _ := testCredentials(t)
		now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
		client, err := apns.NewWithTransport(creds, server.Client().Transport, server.URL, server.URL, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		result, err := client.Send(t.Context(), liveActivityRequest(apns.Sandbox))
		if err != nil || result.Class != c.class || result.Status != c.status || result.Reason != c.reason {
			t.Fatalf("%d %s: %+v %v", c.status, c.reason, result, err)
		}
		if c.retryAfter != "" && !result.RetryAfter.Equal(now.Add(30*time.Second)) {
			t.Fatalf("Retry-After %v", result.RetryAfter)
		}
	}
}

// A Live Activity push whose answer is lost after it may have reached Apple
// is Ambiguous with its cause; one that never reached the wire is ErrNotSent
// with its cause. Neither error names the host, the path or the token.
func TestSendLiveActivityLostOrUnsentIsReportedWithItsCause(t *testing.T) {
	creds, _ := testCredentials(t)
	request := liveActivityRequest(apns.Production)
	reset := fakeAPNs(t, func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	client, err := apns.NewWithTransport(creds, reset.Client().Transport, reset.URL, reset.URL, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Send(t.Context(), request)
	if result.Class != apns.Ambiguous || !errors.Is(err, apns.ErrOutcomeUnknown) || !errors.Is(err, apns.ErrCauseConnection) || errors.Is(err, apns.ErrNotSent) {
		t.Fatalf("a reset stream: %+v %v, want Ambiguous with a connection cause", result, err)
	}
	if len(reset.seen()) != 1 {
		t.Fatalf("%d requests, want 1", len(reset.seen()))
	}
	// The default transport does not trust the test server's certificate:
	// the handshake fails before a byte of the request is written.
	untrusted, err := apns.NewWithTransport(creds, &http.Transport{ForceAttemptHTTP2: true}, reset.URL, reset.URL, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	result, unsent := untrusted.Send(t.Context(), request)
	if result.Class == apns.Ambiguous || !errors.Is(unsent, apns.ErrNotSent) || !errors.Is(unsent, apns.ErrCauseTLS) || errors.Is(unsent, apns.ErrOutcomeUnknown) {
		t.Fatalf("an untrusted certificate: %+v %v, want ErrNotSent with a TLS cause", result, unsent)
	}
	if len(reset.seen()) != 1 {
		t.Fatalf("the unsent push reached the server")
	}
	for _, e := range []error{err, unsent} {
		for _, secret := range []string{hex.EncodeToString(request.Token), reset.URL, strings.TrimPrefix(reset.URL, "https://"), "/3/device"} {
			if e != nil && strings.Contains(e.Error(), secret) {
				t.Fatalf("the error %q names %q", e, secret)
			}
		}
	}
}

// Apple's 403 InvalidProviderToken to a Live Activity push makes the next
// push re-sign, no sooner than 20 minutes after the last signing, exactly as
// for a wake.
func TestSendLiveActivityReSignsAfterAppleRefusesTheProviderToken(t *testing.T) {
	var refuse atomic.Bool
	server := fakeAPNs(t, func(w http.ResponseWriter, _ *http.Request) {
		if refuse.Load() {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"reason":"InvalidProviderToken"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	creds, key := testCredentials(t)
	start := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(start.UnixNano())
	client, err := apns.NewWithTransport(creds, server.Client().Transport, server.URL, server.URL, func() time.Time { return time.Unix(0, clock.Load()).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	send := func(at time.Duration) (apns.Result, string) {
		t.Helper()
		clock.Store(start.Add(at).UnixNano())
		result, err := client.Send(t.Context(), liveActivityRequest(apns.Sandbox))
		if err != nil {
			t.Fatal(err)
		}
		requests := server.seen()
		return result, requests[len(requests)-1].headers.Get("authorization")
	}
	_, signed := send(0)
	refuse.Store(true)
	if result, auth := send(10 * time.Minute); auth != signed || result.Class != apns.ConfigurationFailure || result.Reason != "InvalidProviderToken" {
		t.Fatalf("a refusal inside the floor: %+v, re-signed %v", result, auth != signed)
	}
	if _, auth := send(11 * time.Minute); auth != signed {
		t.Fatal("re-signed inside Apple's 20-minute floor")
	}
	if _, auth := send(21 * time.Minute); auth != signed {
		t.Fatal("the refused request did not carry the cached token")
	}
	refuse.Store(false)
	_, resigned := send(22 * time.Minute)
	jwt := decodeProviderToken(t, resigned, key)
	if resigned == signed || !jwt.valid || jwt.claims != fmt.Sprintf(`{"iat":%d,"iss":"TEAM123456"}`, start.Add(22*time.Minute).Unix()) {
		t.Fatalf("not re-signed after the refusal: %+v", jwt)
	}
}
