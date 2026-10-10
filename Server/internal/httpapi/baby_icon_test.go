package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/testkit"
)

// babyIconGolden is testdata/baby-icon-roundtrip.json: an icon-bearing baby
// create in the shape a fce422d5 phone sends (api/fixtures/v1/baby.json with
// `icon` added; a phone writes the same keys and values, but sorts the
// payload's keys, EntryPayload.swift, which decoding ignores), and the exact
// payload bytes main's wire returns for it in the outcome, a pull and a
// snapshot, and after an update that leaves `icon` out. Strings, not JSON
// values, so the golden holds the bytes rather than an indented re-encoding.
type babyIconGolden struct {
	Request         string `json:"request"`
	OutcomePayload  string `json:"outcome_payload"`
	PulledPayload   string `json:"pulled_payload"`
	SnapshotPayload string `json:"snapshot_payload"`
	UpdatedPayload  string `json:"updated_payload"`
}

const (
	iconBabyID         = "10000000-0000-4000-8000-000000000001" // api/fixtures/v1/baby.json's entity
	plainBabyID        = "10000000-0000-4000-8000-0000000000b2"
	babyIconGoldenPath = "testdata/baby-icon-roundtrip.json"
)

// fixtureMutation is api/fixtures/v1/<name> with each replacement made
// exactly once, as raw bytes.
func fixtureMutation(t *testing.T, name string, replacements ...string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("../../api/fixtures/v1", name))
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.TrimSpace(raw)
	for i := 0; i < len(replacements); i += 2 {
		next := bytes.Replace(raw, []byte(replacements[i]), []byte(replacements[i+1]), 1)
		if bytes.Equal(next, raw) {
			t.Fatalf("%s: %q not found", name, replacements[i])
		}
		raw = next
	}
	return raw
}

// syncBody is a phone's POST /v1/sync body around raw mutations, with no
// cursor (the phone's first sync of the batch).
func syncBody(mutations ...[]byte) []byte {
	body := []byte(`{"protocol_version":"v1","capabilities":["records-v1"],"cursor":null,"mutations":[`)
	body = append(body, bytes.Join(mutations, []byte(","))...)
	return append(body, []byte(`],"page_token":null}`)...)
}

type rawEntity struct {
	EntityKind string `json:"entity_kind"`
	Meta       struct {
		ID string `json:"id"`
	} `json:"meta"`
	Payload json.RawMessage `json:"payload"`
}

type rawOutcome struct {
	MutationID      string    `json:"mutation_id"`
	Status          string    `json:"status"`
	OriginalStatus  *string   `json:"original_status"`
	Code            *string   `json:"code"`
	CanonicalEntity rawEntity `json:"canonical_entity"`
}

type rawPage struct {
	Outcomes []rawOutcome `json:"outcomes"`
	Changes  []struct {
		SnapshotOrTombstone rawEntity `json:"snapshot_or_tombstone"`
	} `json:"changes"`
	Entities      []rawEntity `json:"entities"`
	NextPageToken *string     `json:"next_page_token"`
}

// decodeRawPage is a one-page response: a token for another page fails.
func decodeRawPage(t *testing.T, label string, body []byte) rawPage {
	t.Helper()
	page := decodeAnyRawPage(t, label, body)
	if page.NextPageToken != nil {
		t.Fatalf("%s: a second page: %s", label, body)
	}
	return page
}

// decodeAnyRawPage is a response page that may continue, after checking
// the raw page never carries "icon":null.
func decodeAnyRawPage(t *testing.T, label string, body []byte) rawPage {
	t.Helper()
	if bytes.Contains(body, []byte(`"icon":null`)) {
		t.Fatalf(`%s: "icon":null on the wire: %s`, label, body)
	}
	var page rawPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("%s: %v: %s", label, err, body)
	}
	return page
}

// payloadOf is the raw payload bytes of the last entity for id: a pull page
// holds one change per entity, a snapshot one entity.
func payloadOf(t *testing.T, label string, entities []rawEntity, id string) string {
	t.Helper()
	found := -1
	for i, entity := range entities {
		if entity.EntityKind == "baby" && entity.Meta.ID == id {
			found = i
		}
	}
	if found < 0 {
		t.Fatalf("%s: baby %s missing from %+v", label, id, entities)
	}
	return string(entities[found].Payload)
}

func changedEntities(page rawPage) []rawEntity {
	entities := make([]rawEntity, 0, len(page.Changes))
	for _, change := range page.Changes {
		entities = append(entities, change.SnapshotOrTombstone)
	}
	return entities
}

// TestPhoneBabyIconRoundTripsByteIdentical is Review Focus 6 on the wire:
// the bytes a fce422d5 phone sends for a baby with an icon come back
// unchanged in the outcome, in another phone's pull and in its snapshot;
// an update that leaves the key out keeps the icon; a baby without one
// never carries `"icon":null`; an out-of-set icon refuses that mutation,
// never the request.
func TestPhoneBabyIconRoundTripsByteIdentical(t *testing.T) {
	h := testkit.New(t)
	s := newStack(t, h, limits.DefaultPolicies()[limits.ScopeSyncPullDevice])
	post := func(label, subject string, body []byte) rawPage {
		t.Helper()
		rec := s.raw(http.MethodPost, "/v1/sync", body, s.headers(subject, "/v1/sync"))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", label, rec.Code, rec.Body.String())
		}
		return decodeRawPage(t, label, rec.Body.Bytes())
	}
	pull := func(label, subject string) rawPage {
		t.Helper()
		body, err := json.Marshal(h.SyncRequest(h.Cursor(0), nil))
		if err != nil {
			t.Fatal(err)
		}
		return post(label, subject, body)
	}
	snapshot := func(label, subject string) rawPage {
		t.Helper()
		rec := s.raw(http.MethodGet, "/v1/sync/snapshot", nil, s.headers(subject, "/v1/sync/snapshot"))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", label, rec.Code, rec.Body.String())
		}
		return decodeRawPage(t, label, rec.Body.Bytes())
	}
	outcomes := func(label string, page rawPage, n int) []rawOutcome {
		t.Helper()
		if len(page.Outcomes) != n {
			t.Fatalf("%s: %d outcomes, want %d: %+v", label, len(page.Outcomes), n, page.Outcomes)
		}
		return page.Outcomes
	}

	// 1. The owner's phone creates a baby with an icon: baby.json with the
	// same insertion main's TestDecodeMutationAcceptsOptionalBabyIcon makes
	// (fixture key order; the phone's sorted-key order is
	// TestPhoneBabyIconSurvivesPagingAndReplay's create).
	var got babyIconGolden
	got.Request = string(syncBody(fixtureMutation(t, "baby.json",
		`"name":"Élie"`, `"name":"Icon baby"`, `"sort_order":0}`, `"sort_order":1,"icon":"girl.2"}`)))
	request := []byte(got.Request)
	if os.Getenv("BT_GOLDEN_UPDATE") != "1" {
		// The body sent is the golden's, byte for byte.
		raw, err := os.ReadFile(babyIconGoldenPath)
		if err != nil {
			t.Fatal(err)
		}
		var want babyIconGolden
		if err := json.Unmarshal(raw, &want); err != nil {
			t.Fatal(err)
		}
		request = []byte(want.Request)
	}
	created := outcomes("create", post("create", "owner", request), 1)[0]
	if created.Status != "applied" {
		t.Fatalf("create: %+v", created)
	}
	// 2. The outcome.
	got.OutcomePayload = string(created.CanonicalEntity.Payload)
	// 3. The caregiver's phone pulls from 0.
	got.PulledPayload = payloadOf(t, "caregiver pull", changedEntities(pull("caregiver pull", "caregiver")), iconBabyID)
	// 4. And takes a snapshot.
	got.SnapshotPayload = payloadOf(t, "caregiver snapshot", snapshot("caregiver snapshot", "caregiver").Entities, iconBabyID)

	// 5. The caregiver renames the baby without the `icon` key.
	rename := fixtureMutation(t, "baby.json",
		`"mutation_id":"01000000-0000-4000-8000-000000000001"`, `"mutation_id":"01000000-0000-4000-8000-0000000000a5"`,
		`"operation":"create"`, `"operation":"update"`,
		`"observed_entity_version":0`, `"observed_entity_version":1`,
		`"name":"Élie"`, `"name":"Renamed baby"`, `"sort_order":0}`, `"sort_order":1}`)
	if bytes.Contains(rename, []byte(`"icon"`)) {
		t.Fatalf("the rename carries an icon: %s", rename)
	}
	renamed := outcomes("rename", post("rename", "caregiver", syncBody(rename)), 1)[0]
	if renamed.Status != "applied" {
		t.Fatalf("rename: %+v", renamed)
	}
	got.UpdatedPayload = string(renamed.CanonicalEntity.Payload)
	if pulled := payloadOf(t, "owner pull after rename", changedEntities(pull("owner pull after rename", "owner")), iconBabyID); pulled != got.UpdatedPayload {
		t.Errorf("owner pull after rename: payload %s, want %s", pulled, got.UpdatedPayload)
	}
	if snapped := payloadOf(t, "snapshot after rename", snapshot("snapshot after rename", "owner").Entities, iconBabyID); snapped != got.UpdatedPayload {
		t.Errorf("snapshot after rename: payload %s, want %s", snapped, got.UpdatedPayload)
	}

	// 6. A baby created without an icon carries no `icon` key anywhere
	// (post, pull and snapshot already refuse "icon":null on any page).
	plain := fixtureMutation(t, "baby.json",
		`"mutation_id":"01000000-0000-4000-8000-000000000001"`, `"mutation_id":"01000000-0000-4000-8000-0000000000b6"`,
		`"entity_id":"10000000-0000-4000-8000-000000000001"`, `"entity_id":"`+plainBabyID+`"`,
		`"name":"Élie"`, `"name":"Plain baby"`)
	plainOutcome := outcomes("plain create", post("plain create", "owner", syncBody(plain)), 1)[0]
	if plainOutcome.Status != "applied" {
		t.Fatalf("plain create: %+v", plainOutcome)
	}
	for label, payload := range map[string]string{
		"outcome":  string(plainOutcome.CanonicalEntity.Payload),
		"pull":     payloadOf(t, "plain pull", changedEntities(pull("plain pull", "caregiver")), plainBabyID),
		"snapshot": payloadOf(t, "plain snapshot", snapshot("plain snapshot", "caregiver").Entities, plainBabyID),
	} {
		if want := `{"name":"Plain baby","birth_date":"2026-01-12","sort_order":0}`; payload != want {
			t.Errorf("plain baby %s: payload %s, want %s", label, payload, want)
		}
	}

	// 7. One batch: a valid feed and a baby update with an out-of-set icon.
	// The request answers 200; only the baby mutation is refused.
	feed := fixtureMutation(t, "feed-bottle.json",
		`"mutation_id":"01000000-0000-4000-8000-000000000003"`, `"mutation_id":"01000000-0000-4000-8000-0000000000c7"`)
	badIcon := fixtureMutation(t, "baby.json",
		`"mutation_id":"01000000-0000-4000-8000-000000000001"`, `"mutation_id":"01000000-0000-4000-8000-0000000000c8"`,
		`"operation":"create"`, `"operation":"update"`,
		`"observed_entity_version":0`, `"observed_entity_version":2`,
		`"sort_order":0}`, `"sort_order":0,"icon":"boy.5"}`)
	mixed := outcomes("mixed batch", post("mixed batch", "owner", syncBody(feed, badIcon)), 2)
	if mixed[0].MutationID != "01000000-0000-4000-8000-0000000000c7" || mixed[0].Status != "applied" {
		t.Errorf("mixed batch: feed outcome %+v, want applied", mixed[0])
	}
	if mixed[1].MutationID != "01000000-0000-4000-8000-0000000000c8" || mixed[1].Status != "rejected" ||
		mixed[1].Code == nil || *mixed[1].Code != "invalid_mutation" {
		t.Errorf("mixed batch: baby outcome %+v, want rejected invalid_mutation", mixed[1])
	}
	if snapped := payloadOf(t, "snapshot after refusal", snapshot("snapshot after refusal", "owner").Entities, iconBabyID); snapped != got.UpdatedPayload {
		t.Errorf("snapshot after the refused icon: payload %s, want %s", snapped, got.UpdatedPayload)
	}

	testkit.AssertGolden(t, babyIconGoldenPath, got)
}

// TestPhoneBabyIconSurvivesPagingAndReplay sends an icon create in the
// phone's own payload key order (sorted, EntryPayload.swift at fce422d5)
// and follows the icon through the paths a one-page test cannot reach: a
// pull and a snapshot that each serve it on a later page, and a replay of
// the same create, whose already_applied outcome carries the same bytes.
func TestPhoneBabyIconSurvivesPagingAndReplay(t *testing.T) {
	raw, err := os.ReadFile(babyIconGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	var golden babyIconGolden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	const iconID = "10000000-0000-4000-8000-0000000000f1"
	h := testkit.New(t)
	h.Limits.SyncPageSize = 3
	// Four plain babies first, with ids below the icon baby's: a pull
	// (changed_seq order) and a snapshot (id order) both serve them first,
	// so the icon baby lands on the second page of each.
	for n := 1; n <= 4; n++ {
		caretest.Seed(t, h, caretest.Baby(fmt.Sprintf("10000000-0000-4000-8000-0000000000a%d", n)))
	}
	s := newStack(t, h, limits.DefaultPolicies()[limits.ScopeSyncPullDevice])
	create := syncBody(fixtureMutation(t, "baby.json",
		`"mutation_id":"01000000-0000-4000-8000-000000000001"`, `"mutation_id":"01000000-0000-4000-8000-0000000000f2"`,
		`"entity_id":"10000000-0000-4000-8000-000000000001"`, `"entity_id":"`+iconID+`"`,
		`{"name":"Élie","birth_date":"2026-01-12","sort_order":0}`, `{"birth_date":"2026-01-12","icon":"girl.2","name":"Icon baby","sort_order":1}`))
	post := func(label, subject string, body []byte) rawPage {
		t.Helper()
		rec := s.raw(http.MethodPost, "/v1/sync", body, s.headers(subject, "/v1/sync"))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", label, rec.Code, rec.Body.String())
		}
		return decodeAnyRawPage(t, label, rec.Body.Bytes())
	}
	first := post("create", "owner", create)
	if len(first.Outcomes) != 1 || first.Outcomes[0].Status != "applied" {
		t.Fatalf("create: %+v", first.Outcomes)
	}
	// The sorted-key payload comes back in the wire's (main's) key order.
	createdPayload := string(first.Outcomes[0].CanonicalEntity.Payload)
	if createdPayload != golden.OutcomePayload {
		t.Fatalf("create: payload %s, want %s", createdPayload, golden.OutcomePayload)
	}

	// A paged pull from 0 by the caregiver.
	pages, at := 0, 0
	var token *string
	pulled := ""
	for {
		request := h.SyncRequest(nil, token)
		if token == nil {
			request.Cursor = h.Cursor(0)
		}
		body, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		page := post(fmt.Sprintf("pull page %d", pages+1), "caregiver", body)
		pages++
		for _, entity := range changedEntities(page) {
			if entity.EntityKind == "baby" && entity.Meta.ID == iconID {
				pulled, at = string(entity.Payload), pages
			}
		}
		if token = page.NextPageToken; token == nil {
			break
		}
		if pages > 10 {
			t.Fatal("pull does not end")
		}
	}
	if pages < 2 || at < 2 || pulled != golden.PulledPayload {
		t.Fatalf("paged pull: %d pages, icon baby on page %d with payload %s, want a later page and %s", pages, at, pulled, golden.PulledPayload)
	}

	// A paged snapshot by the caregiver.
	pages, at = 0, 0
	token = nil
	snapped := ""
	for {
		target := "/v1/sync/snapshot"
		if token != nil {
			target += "?page_token=" + url.QueryEscape(*token)
		}
		rec := s.raw(http.MethodGet, target, nil, s.headers("caregiver", target))
		if rec.Code != http.StatusOK {
			t.Fatalf("snapshot page %d: status %d: %s", pages+1, rec.Code, rec.Body.String())
		}
		page := decodeAnyRawPage(t, fmt.Sprintf("snapshot page %d", pages+1), rec.Body.Bytes())
		pages++
		for _, entity := range page.Entities {
			if entity.EntityKind == "baby" && entity.Meta.ID == iconID {
				snapped, at = string(entity.Payload), pages
			}
		}
		if token = page.NextPageToken; token == nil {
			break
		}
		if pages > 10 {
			t.Fatal("snapshot does not end")
		}
	}
	if pages < 2 || at < 2 || snapped != golden.SnapshotPayload {
		t.Fatalf("paged snapshot: %d pages, icon baby on page %d with payload %s, want a later page and %s", pages, at, snapped, golden.SnapshotPayload)
	}

	// The owner's phone retries the same create: the receipt answers.
	replay := post("replay", "owner", create)
	if len(replay.Outcomes) != 1 {
		t.Fatalf("replay: %+v", replay.Outcomes)
	}
	again := replay.Outcomes[0]
	if again.Status != "already_applied" || again.OriginalStatus == nil || *again.OriginalStatus != "applied" {
		t.Fatalf("replay: status %s original %v, want already_applied of applied", again.Status, again.OriginalStatus)
	}
	if got := string(again.CanonicalEntity.Payload); got != createdPayload {
		t.Fatalf("replay: payload %s, want the create's %s", got, createdPayload)
	}
}
