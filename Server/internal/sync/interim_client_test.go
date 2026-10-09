package sync_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

// The interim client is build 2961ca1, the build on the caregiver's phone
// while the format cleanup ships ahead of the final client. Its Swift client
// is byte-identical to Packages/BabyTracker/Sources at d1daa6a.
//
// Every body below is frozen: captured from that client's own code
// (BabyStore outbox -> nextDispatchBatch -> JSONEncoder().encode(SyncRequest),
// exactly as FamilySyncHTTPClient.send encodes it), with only the random IDs
// and times swapped for fixed ones and observed versions set to what a synced
// phone would hold. Key order, the escaped "\/" in zone IDs, the omitted
// cursor and page_token, and the payload versions are the client's own. Do
// NOT regenerate these from the current Swift code: they pin what the
// installed build sends, not what the source says today.
const (
	interimBabyID = "cf80ab4b-fda5-4360-a306-08801de55fdb"
	interimFeedID = "f9fcf106-4e91-4a57-b8aa-6b1326d9bcde"
)

// MutationFactory.payloadVersion at d1daa6a: a feed create or update always
// carries feeding_session_id, so v4. A feed delete's payload is only
// {"baby_id"}: no session key, no components, so v2.
const interimFeedCreate = `{"mutations":[{"payload_version":4,"operation":"create","entity_id":"f9fcf106-4e91-4a57-b8aa-6b1326d9bcde","observed_entity_version":0,"mutation_id":"3e532b89-49a5-4226-895a-89218d51a072","client_mutation_time":"2026-09-21T09:23:20.000Z","payload":{"ended_at":"2026-09-21T09:23:20.000Z","kind":"breast","time_zone_id":"Europe\/Zurich","baby_id":"cf80ab4b-fda5-4360-a306-08801de55fdb","note":null,"components":[{"id":"188f81eb-dd79-4e67-8a36-677632620ad4","side":"left","kind":"breast","started_at":"2026-09-21T09:18:20.000Z","ended_at":"2026-09-21T09:23:20.000Z","seconds":300}],"feeding_session_id":null,"started_at":"2026-09-21T09:18:20.000Z"},"entity_kind":"feed"}],"protocol_version":"v1","capabilities":["records-v1"]}`

const interimFeedUpdate = `{"mutations":[{"client_mutation_time":"2026-09-21T09:24:20.000Z","entity_kind":"feed","entity_id":"f9fcf106-4e91-4a57-b8aa-6b1326d9bcde","mutation_id":"dda12471-18a0-46ed-b65b-009cda8cca21","observed_entity_version":1,"payload_version":4,"operation":"update","payload":{"ended_at":"2026-09-21T09:23:20.000Z","started_at":"2026-09-21T09:18:20.000Z","baby_id":"cf80ab4b-fda5-4360-a306-08801de55fdb","kind":"breast","feeding_session_id":null,"components":[{"id":"188f81eb-dd79-4e67-8a36-677632620ad4","side":"left","kind":"breast","started_at":"2026-09-21T09:18:20.000Z","ended_at":"2026-09-21T09:23:20.000Z","seconds":300}],"time_zone_id":"Europe\/Zurich","note":"Top-up"}}],"protocol_version":"v1","capabilities":["records-v1"]}`

const interimFeedDeleteV2 = `{"mutations":[{"client_mutation_time":"2026-09-21T09:25:20.000Z","entity_kind":"feed","entity_id":"f9fcf106-4e91-4a57-b8aa-6b1326d9bcde","mutation_id":"d1426bf3-cb86-463f-9d2f-5b546a3bb749","observed_entity_version":2,"payload_version":2,"operation":"delete","payload":{"baby_id":"cf80ab4b-fda5-4360-a306-08801de55fdb"}}],"protocol_version":"v1","capabilities":["records-v1"]}`

// A sleep create always carries sleep_session_id, so v2; its delete does not,
// so v1.
const interimSleepCreate = `{"mutations":[{"client_mutation_time":"2026-09-21T09:26:20.000Z","entity_kind":"sleep","entity_id":"ca7683ac-b65c-4180-af6d-07c80833e58d","mutation_id":"10ad6dad-71f6-42b7-b323-5c120d971495","observed_entity_version":0,"payload_version":2,"operation":"create","payload":{"sleep_session_id":null,"ended_at":"2026-09-21T09:26:20.000Z","started_at":"2026-09-21T08:26:20.000Z","baby_id":"cf80ab4b-fda5-4360-a306-08801de55fdb","time_zone_id":"Europe\/Zurich","note":null,"kind_override":null}}],"protocol_version":"v1","capabilities":["records-v1"]}`

const interimSleepDeleteV1 = `{"mutations":[{"client_mutation_time":"2026-09-21T09:27:20.000Z","entity_kind":"sleep","entity_id":"ca7683ac-b65c-4180-af6d-07c80833e58d","mutation_id":"f49095cf-479f-4dc8-aa6b-610d9f1e249b","observed_entity_version":1,"payload_version":1,"operation":"delete","payload":{"baby_id":"cf80ab4b-fda5-4360-a306-08801de55fdb"}}],"protocol_version":"v1","capabilities":["records-v1"]}`

// Every timer command is v5 (MutationFactory: `if kind == .timer { return 5 }`),
// and a batch carrying one advertises timer-commands-v1.
const interimTimerStart = `{"mutations":[{"client_mutation_time":"2026-09-21T10:27:20.000Z","entity_kind":"timer","entity_id":"5cb21fff-63f8-4639-a3f8-db626e15cd11","mutation_id":"50b7cdf4-990c-49ca-aced-919538971c20","observed_entity_version":0,"payload_version":5,"operation":"timer_start","payload":{"completed_components":[],"session_id":null,"started_at":"2026-09-21T10:27:20.000Z","baby_id":"cf80ab4b-fda5-4360-a306-08801de55fdb","time_zone_id":"Europe\/Zurich","timer_kind":"feed","current_bottle_started_at":null,"current_side_started_at":"2026-09-21T10:27:20.000Z","current_side":"left"}}],"protocol_version":"v1","capabilities":["records-v1","timer-commands-v1"]}`

const interimTimerEdit = `{"protocol_version":"v1","capabilities":["records-v1","timer-commands-v1"],"mutations":[{"client_mutation_time":"2026-09-21T10:29:20.000Z","entity_kind":"timer","entity_id":"5cb21fff-63f8-4639-a3f8-db626e15cd11","mutation_id":"7ccd30ab-6bf0-43cc-968a-24bca7af3e71","observed_entity_version":1,"payload_version":5,"operation":"update","payload":{"baby_id":"cf80ab4b-fda5-4360-a306-08801de55fdb","session_id":null,"started_at":"2026-09-21T10:24:20.000Z","components":[]}}]}`

const interimTimerStop = `{"protocol_version":"v1","capabilities":["records-v1","timer-commands-v1"],"mutations":[{"client_mutation_time":"2026-09-21T10:31:20.000Z","entity_kind":"timer","entity_id":"5cb21fff-63f8-4639-a3f8-db626e15cd11","mutation_id":"d962ef62-79a7-42a0-9dfe-0af3372317e4","observed_entity_version":2,"payload_version":5,"operation":"timer_stop","payload":{"stopped_at":"2026-09-21T10:31:20.000Z","baby_id":"cf80ab4b-fda5-4360-a306-08801de55fdb","completed_record_id":"fd400f85-fd8d-4d59-9cb5-8db6a53f2416","component_id":"43c8b2b9-eef9-429e-9c62-97f1b15c48de","cleanup_short_breast_segments":true}}]}`

const interimBottleTimerStart = `{"protocol_version":"v1","capabilities":["records-v1","timer-commands-v1"],"mutations":[{"payload_version":5,"operation":"timer_start","payload":{"time_zone_id":"Europe\/Zurich","completed_components":[],"baby_id":"cf80ab4b-fda5-4360-a306-08801de55fdb","started_at":"2026-09-21T11:31:20.000Z","current_bottle_started_at":"2026-09-21T11:31:20.000Z","session_id":null,"timer_kind":"feed"},"mutation_id":"e8975ada-7787-4f29-8f9b-ebe025b690bc","observed_entity_version":0,"client_mutation_time":"2026-09-21T11:31:20.000Z","entity_id":"ea1fa036-0498-44b7-91ec-4ad877c72e5b","entity_kind":"timer"}]}`

const interimTimerFinishBottle = `{"protocol_version":"v1","capabilities":["records-v1","timer-commands-v1"],"mutations":[{"payload_version":5,"observed_entity_version":1,"client_mutation_time":"2026-09-21T11:36:20.000Z","operation":"timer_finish_bottle","payload":{"baby_id":"cf80ab4b-fda5-4360-a306-08801de55fdb","finished_at":"2026-09-21T11:36:20.000Z","contents":"formula","component_id":"64431f96-ed33-4a0c-b9f1-74d618f15e59","volume_ml":60},"entity_kind":"timer","mutation_id":"e1bfc068-ca92-47f1-bdd7-d4a3e19757fc","entity_id":"ea1fa036-0498-44b7-91ec-4ad877c72e5b"}]}`

// An empty pull from a phone with no cursor yet: cursor and page_token are
// omitted, not null, because Swift's synthesized Codable skips nil optionals.
const interimFirstPull = `{"mutations":[],"protocol_version":"v1","capabilities":["records-v1"]}`

// Shapes the interim client never sends: an older build's feed create at v3
// (a timed bottle without the session key) and at v1 (the scalar shape).
const olderFeedCreateV3 = `{"mutations":[{"payload_version":3,"operation":"create","entity_id":"6a1c3a52-5b0e-4f53-9d55-6d1f0e8b2a01","observed_entity_version":0,"mutation_id":"6a1c3a52-5b0e-4f53-9d55-6d1f0e8b2a02","client_mutation_time":"2026-09-21T09:40:00.000Z","payload":{"ended_at":"2026-09-21T09:40:00.000Z","kind":"bottle","time_zone_id":"Europe\/Zurich","baby_id":"cf80ab4b-fda5-4360-a306-08801de55fdb","note":null,"components":[{"id":"6a1c3a52-5b0e-4f53-9d55-6d1f0e8b2a03","kind":"bottle","volume_ml":60,"contents":"formula","seconds":300}],"started_at":"2026-09-21T09:35:00.000Z"},"entity_kind":"feed"}],"protocol_version":"v1","capabilities":["records-v1"]}`

const olderFeedCreateV1 = `{"mutations":[{"payload_version":1,"operation":"create","entity_id":"6a1c3a52-5b0e-4f53-9d55-6d1f0e8b2a04","observed_entity_version":0,"mutation_id":"6a1c3a52-5b0e-4f53-9d55-6d1f0e8b2a05","client_mutation_time":"2026-09-21T09:45:00.000Z","payload":{"baby_id":"cf80ab4b-fda5-4360-a306-08801de55fdb","kind":"bottle","started_at":"2026-09-21T09:45:00.000Z","ended_at":null,"time_zone_id":"Europe\/Zurich","note":null,"sides":[],"volume_ml":90,"contents":"formula","foods":[],"amount":null},"entity_kind":"feed"}],"protocol_version":"v1","capabilities":["records-v1"]}`

type alwaysReady struct{}

func (alwaysReady) Check(context.Context) error { return nil }

type allowAll struct{}

func (allowAll) Take(context.Context, string, string, limits.Policy) (limits.Decision, error) {
	return limits.Decision{Allowed: true}, nil
}
func (allowAll) TakeMany(context.Context, []limits.Charge) (limits.Decision, error) {
	return limits.Decision{Allowed: true}, nil
}

// interimPhone sends requests exactly as FamilySyncHTTPClient.send does at
// d1daa6a: bearer token, protocol and capability headers, all four feature
// markers, and Content-Type only when there is a body.
type interimPhone struct {
	t      *testing.T
	server *httptest.Server
	token  string
}

func (phone interimPhone) send(method, path, capabilities, body string, markers bool) (int, string) {
	phone.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, phone.server.URL+path, reader)
	if err != nil {
		phone.t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+phone.token)
	request.Header.Set("X-Baby-Tracker-Protocol", "v1")
	request.Header.Set("X-Baby-Tracker-Capabilities", capabilities)
	if markers {
		request.Header.Set("X-Baby-Tracker-Supplements", "v1")
		request.Header.Set("X-Baby-Tracker-Growth", "v1")
		request.Header.Set("X-Baby-Tracker-Timed-Bottle", "v1")
		request.Header.Set("X-Baby-Tracker-Feeding-Session", "v1")
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := phone.server.Client().Do(request)
	if err != nil {
		phone.t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		phone.t.Fatal(err)
	}
	return response.StatusCode, string(raw)
}

func (phone interimPhone) sync(body string) protocol.SyncResponse {
	phone.t.Helper()
	var envelope struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		phone.t.Fatal(err)
	}
	status, raw := phone.send(http.MethodPost, "/v1/sync", strings.Join(envelope.Capabilities, ","), body, true)
	if status != http.StatusOK {
		phone.t.Fatalf("sync %d %s\nbody %s", status, raw, body)
	}
	var response protocol.SyncResponse
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		phone.t.Fatalf("decode sync response: %v\n%s", err, raw)
	}
	return response
}

func newInterimPhone(t *testing.T, h *testkit.Harness) interimPhone {
	t.Helper()
	policies := limits.DefaultPolicies()
	resolver, err := limits.NewClientIPResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpapi.New(httpapi.Options{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Readiness: alwaysReady{}, InstanceID: h.Info.InstanceID, RecoveryEpoch: h.Info.RecoveryEpoch,
		CanonicalOrigin: h.Info.CanonicalOrigin, SyncLimits: &h.Limits, AppleAuthorizationMode: "identity_token_only",
		SessionService: h.Auth, FamilyService: h.Family, SyncService: h.Service, Limiter: allowAll{}, ClientIPResolver: resolver,
		SyncMutationDevicePolicy: policies[limits.ScopeSyncMutationDevice], SyncMutationAccountPolicy: policies[limits.ScopeSyncMutationAccount],
		SyncMutationFamilyPolicy: policies[limits.ScopeSyncMutationFamily], SyncPullDevicePolicy: policies[limits.ScopeSyncPullDevice],
		SyncPullAccountPolicy: policies[limits.ScopeSyncPullAccount],
	}))
	t.Cleanup(server.Close)
	// The interim phone belongs to the caregiver who joined the owner's family.
	return interimPhone{t: t, server: server, token: h.AccessToken("caregiver")}
}

func requireApplied(t *testing.T, label string, response protocol.SyncResponse) protocol.Outcome {
	t.Helper()
	if len(response.Outcomes) != 1 || response.Outcomes[0].Status != "applied" {
		t.Fatalf("%s outcomes=%+v", label, response.Outcomes)
	}
	return response.Outcomes[0]
}

func TestInterimClientWritesAreAcceptedWithItsFeatureHeaders(t *testing.T) {
	h := testkit.New(t)
	baby := testkit.MustMutation(t, "baby.json")
	baby.EntityID = interimBabyID
	if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, baby); err != nil || out.Status != "applied" {
		t.Fatalf("seed baby %+v %v", out, err)
	}
	phone := newInterimPhone(t, h)

	first := phone.sync(interimFirstPull)
	if first.HighWater.Sequence != 1 || len(first.Changes) != 1 {
		t.Fatalf("first pull high_water=%d changes=%d", first.HighWater.Sequence, len(first.Changes))
	}

	created := requireApplied(t, "feed create v4", phone.sync(interimFeedCreate))
	feed := created.CanonicalEntity.Payload.(protocol.FeedPayload)
	if len(feed.Components) != 1 || feed.Components[0].Kind != "breast" {
		t.Fatalf("feed create stored %+v", feed)
	}
	updated := requireApplied(t, "feed update v4", phone.sync(interimFeedUpdate))
	if note := updated.CanonicalEntity.Payload.(protocol.FeedPayload).Note; note == nil || *note != "Top-up" {
		t.Fatalf("feed update note=%v", note)
	}
	deleted := requireApplied(t, "feed delete v2", phone.sync(interimFeedDeleteV2))
	if deleted.CanonicalEntity.Meta.ID != interimFeedID || deleted.CanonicalEntity.Meta.TombstonedAt == nil {
		t.Fatalf("feed delete v2 did not tombstone: %+v", deleted.CanonicalEntity.Meta)
	}

	sleep := requireApplied(t, "sleep create v2", phone.sync(interimSleepCreate))
	if stored, ok := sleep.CanonicalEntity.Payload.(protocol.SleepPayload); !ok || stored.SleepSessionID != nil {
		t.Fatalf("sleep create stored %+v", sleep.CanonicalEntity.Payload)
	}
	if out := requireApplied(t, "sleep delete v1", phone.sync(interimSleepDeleteV1)); out.CanonicalEntity.Meta.TombstonedAt == nil {
		t.Fatal("sleep delete v1 did not tombstone")
	}

	requireApplied(t, "timer start v5", phone.sync(interimTimerStart))
	requireApplied(t, "timer edit v5", phone.sync(interimTimerEdit))
	requireApplied(t, "timer stop v5", phone.sync(interimTimerStop))
	requireApplied(t, "bottle timer start v5", phone.sync(interimBottleTimerStart))
	requireApplied(t, "timer finish bottle v5", phone.sync(interimTimerFinishBottle))

	// The markers are ignored, not required: the same pull without them is
	// served identically, and so is the snapshot.
	cursor, err := json.Marshal(first.HighWater)
	if err != nil {
		t.Fatal(err)
	}
	pull := fmt.Sprintf(`{"cursor":%s,"mutations":[],"protocol_version":"v1","capabilities":["records-v1"]}`, cursor)
	for _, markers := range []bool{true, false} {
		status, raw := phone.send(http.MethodPost, "/v1/sync", "records-v1", pull, markers)
		if status != http.StatusOK || !strings.Contains(raw, `"entity_kind":"timer"`) {
			t.Fatalf("pull markers=%v: %d %s", markers, status, raw)
		}
		status, raw = phone.send(http.MethodGet, "/v1/sync/snapshot", "records-v1,snapshot-v1", "", markers)
		if status != http.StatusOK || !strings.Contains(raw, `"entity_kind":"feed"`) {
			t.Fatalf("snapshot markers=%v: %d %s", markers, status, raw)
		}
	}
}

func TestOlderFeedCreatesAreRefusedAsInvalidRequests(t *testing.T) {
	h := testkit.New(t)
	baby := testkit.MustMutation(t, "baby.json")
	baby.EntityID = interimBabyID
	if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, baby); err != nil || out.Status != "applied" {
		t.Fatalf("seed baby %+v %v", out, err)
	}
	phone := newInterimPhone(t, h)
	before := h.FamilySequence(t)
	for label, body := range map[string]string{"feed create v3": olderFeedCreateV3, "feed create v1": olderFeedCreateV1} {
		status, raw := phone.send(http.MethodPost, "/v1/sync", "records-v1", body, true)
		if status != http.StatusBadRequest || !strings.Contains(raw, `"code":"invalid_request"`) {
			t.Fatalf("%s: %d %s", label, status, raw)
		}
	}
	if after := h.FamilySequence(t); after != before {
		t.Fatalf("refused writes advanced the journal %d -> %d", before, after)
	}
}
