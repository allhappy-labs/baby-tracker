package liveactivity

import (
	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

type projectionFixture struct {
	Name     string
	Now      string
	Minutes  int
	Baby     protocol.CanonicalEntity
	Timers   []protocol.CanonicalEntity
	Feeds    []protocol.CanonicalEntity
	Sleeps   []protocol.CanonicalEntity
	Expected []ContentState
}

func TestLiveActivityProjectionFixtures(t *testing.T) {
	raw, err := os.ReadFile("../../api/fixtures/live-activity-v1/projection.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []projectionFixture
	if err = json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, f := range fixtures {
		t.Run(f.Name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339Nano, f.Now)
			if err != nil {
				t.Fatal(err)
			}
			s := CareSnapshot{Baby: f.Baby, Timers: f.Timers, SessionMembers: f.Feeds, Grant: access.Grant{Role: "owner"}, Cursor: protocol.Cursor{FamilyID: f.Baby.Meta.FamilyID, ServerInstanceID: installation, RecoveryEpoch: 1, Sequence: 12}}
			s.LastBreast = latestBreast(f.Feeds)
			s.EarlierAsleepSeconds = fixtureEarlierAsleep(t, f)
			cards, err := Project(s, DevicePreferences{FeedBreakMinutes: f.Minutes}, now)
			if err != nil {
				t.Fatal(err)
			}
			got := []ContentState{}
			for _, card := range cards {
				got = append(got, card.Content)
				if card.CanWrite != true || card.Sequence != 12 {
					t.Fatal("lost authority/cursor")
				}
				if card.MayStart != (card.Phase == "running") {
					t.Fatal("historical start")
				}
			}
			if !reflect.DeepEqual(got, f.Expected) {
				t.Fatalf("got %+v want %+v", got, f.Expected)
			}
		})
	}
}

// fixtureEarlierAsleep is what the snapshot's earlierAsleepSQL would sum for
// the fixture's running sleep, derived from the fixture's sleep rows with the
// same rule in Go (earlierAsleepSeconds), so the joined-sleep and
// separated-sleep cases keep pinning the projection end to end.
func fixtureEarlierAsleep(t *testing.T, f projectionFixture) int64 {
	t.Helper()
	for _, entity := range f.Timers {
		timer, ok := entity.Payload.(protocol.TimerStartPayload)
		if !ok || entity.Meta.TombstonedAt != nil {
			continue
		}
		seconds, err := earlierAsleepSeconds(timer, f.Sleeps)
		if err != nil {
			t.Fatal(err)
		}
		if seconds != 0 {
			return seconds
		}
	}
	return 0
}

func TestLiveActivityDifferentDeviceBreakLimits(t *testing.T) {
	raw, _ := os.ReadFile("../../api/fixtures/live-activity-v1/projection.json")
	var fs []projectionFixture
	json.Unmarshal(raw, &fs)
	f := fs[6]
	now, _ := time.Parse(time.RFC3339Nano, f.Now)
	s := CareSnapshot{Baby: f.Baby, SessionMembers: f.Feeds, LastBreast: latestBreast(f.Feeds)}
	short, err := Project(s, DevicePreferences{5}, now)
	if err != nil {
		t.Fatal(err)
	}
	long, err := Project(s, DevicePreferences{120}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(short) != 1 || len(long) != 1 || long[0].StaleAt.Sub(*short[0].StaleAt) != 115*time.Minute {
		t.Fatal("device preferences not applied")
	}
	expired, _ := Project(s, DevicePreferences{5}, now.Add(5*time.Minute))
	if len(expired) != 0 {
		t.Fatal("short device still actionable")
	}
	alive, _ := Project(s, DevicePreferences{120}, now.Add(5*time.Minute))
	if len(alive) != 1 {
		t.Fatal("long device retired early")
	}
}
func TestLiveActivityBoundedSummary(t *testing.T) {
	raw, _ := os.ReadFile("../../api/fixtures/live-activity-v1/projection.json")
	var fs []projectionFixture
	json.Unmarshal(raw, &fs)
	f := fs[6]
	now, _ := time.Parse(time.RFC3339Nano, f.Now)
	p := f.Feeds[0].Payload.(protocol.FeedPayload)
	p.Components = nil
	for i := uint32(1); i <= 25; i++ {
		v := i
		p.Components = append(p.Components, protocol.FeedComponent{Kind: "breast", Side: ptr("left"), Seconds: &v})
	}
	f.Feeds[0].Payload = p
	b := f.Baby.Payload.(protocol.BabyPayload)
	b.Name = strings.Repeat("👨🏽‍👧é<&", 500)
	f.Baby.Payload = b
	s := CareSnapshot{Baby: f.Baby, SessionMembers: f.Feeds, LastBreast: latestBreast(f.Feeds), Cursor: protocol.Cursor{ServerInstanceID: installation, RecoveryEpoch: protocol.MaxSafeInteger, FamilyID: installation, Sequence: protocol.MaxSafeInteger}}
	cards, err := Project(s, DevicePreferences{120}, now)
	if err != nil {
		t.Fatal(err)
	}
	card := cards[0]
	if len(card.Content.Parts) != 8 || card.Content.OmittedParts != 17 || card.Content.Parts[0].Seconds != 18 || card.Content.Parts[7].Seconds != 25 {
		t.Fatalf("summary %+v", card.Content)
	}
	if card.BabyName != DisplayName(b.Name) || !utf8.ValidString(card.BabyName) || len(card.BabyName) > MaximumDisplayNameBytes {
		t.Fatal("invalid display truncation")
	}
	// Full worst-case start envelope, including duplicated scope in attributes
	// and revision. Task5 owns the actual transport encoder and repeats this gate.
	card.Content.Remote = &Revision{Scope: card.Scope, GenerationID: installation, TimerID: ptr(installation), SessionID: ptr(installation), FamilySequence: protocol.MaxSafeInteger, PresentationVersion: protocol.MaxSafeInteger, PresentationTimestamp: protocol.MaxSafeInteger, CanWrite: true}
	envelope := map[string]any{"aps": map[string]any{"timestamp": protocol.MaxSafeInteger, "event": "start", "content-state": card.Content, "attributes-type": "TimerActivityAttributes", "attributes": map[string]any{"babyID": installation, "babyName": card.BabyName, "kindRaw": "feed", "scope": card.Scope}, "alert": map[string]string{"title": card.BabyName, "body": "Feeding started"}, "stale-date": protocol.MaxSafeInteger, "dismissal-date": protocol.MaxSafeInteger}}
	payload, err := json.Marshal(envelope)
	if err != nil || len(payload) > 4096 {
		t.Fatalf("payload bytes=%d err=%v", len(payload), err)
	}
	t.Logf("Unicode envelope bytes=%d", len(payload))
	// ASCII HTML-sensitive characters expand sixfold with encoding/json.
	// This is larger than the Unicode name despite its shorter UTF-8 input.
	worstName := DisplayName(strings.Repeat("<", 500))
	aps := envelope["aps"].(map[string]any)
	aps["attributes"].(map[string]any)["babyName"] = worstName
	aps["alert"].(map[string]string)["title"] = worstName
	payload, err = json.Marshal(envelope)
	if err != nil || len(payload) > 4096 {
		t.Fatalf("escaped payload bytes=%d err=%v", len(payload), err)
	}
	t.Logf("worst-case escaped envelope bytes=%d", len(payload))
	// Truncation never removes current running identity, status, or write role.
	s.Timers = fs[0].Timers
	s.Grant.Role = "viewer"
	running, err := Project(s, DevicePreferences{120}, now)
	if err != nil {
		t.Fatal(err)
	}
	if running[0].TimerID == nil || *running[0].TimerID != s.Timers[0].Meta.ID || running[0].Content.ActivityKind != "breastLeft" || !running[0].MayStart || running[0].CanWrite {
		t.Fatal("running identity/status lost")
	}
}
func ptr[T any](v T) *T { return &v }
func latestBreast(feeds []protocol.CanonicalEntity) *protocol.CanonicalEntity {
	var last *protocol.CanonicalEntity
	for i := range feeds {
		e := &feeds[i]
		p, ok := e.Payload.(protocol.FeedPayload)
		if !ok || p.Kind != "breast" || e.Meta.TombstonedAt != nil {
			continue
		}
		if last == nil {
			last = e
			continue
		}
		a, _ := time.Parse(time.RFC3339Nano, p.StartedAt)
		b, _ := time.Parse(time.RFC3339Nano, last.Payload.(protocol.FeedPayload).StartedAt)
		if a.After(b) || (a.Equal(b) && e.Meta.ID > last.Meta.ID) {
			last = e
		}
	}
	return last
}
