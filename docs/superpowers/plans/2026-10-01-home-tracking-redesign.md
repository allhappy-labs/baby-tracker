# Home and Tracking Redesign Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the Switch/Pause feed timer with one start→stop timer per breast side, bottle, or sleep. Join short breaks into one feed or sleep automatically on every device. Redesign Home, the logging sheets, the icons, the Live Activity, the widgets, and Siri around one-handed use.

**Architecture:**
- The server gains a session ID on timers (timer payload v5) and on sleeps (sleep payload v2, migration 26). It copies the timer's session ID into whatever record a stop creates. A new feature header guards older clients, following the existing `X-Baby-Tracker-Feeding-Session` boundary.
- On the client, a join engine in `BabyStore` decides the session when a part starts. It reuses the existing `feedingSessionID` grouping, and adds `sleepSessionID` with a `SleepSession` projection.
- Home becomes a pure `screen(at:)` projection that the SwiftUI views render, laid out as hero, status line, today strip, and dock.
- Live Activities stay up during a break, using `staleDate`, with lock-safe `LiveActivityIntent` buttons.

**Tech Stack:**
- Client: Swift 6, SwiftUI (iOS 26), SwiftData, Swift Testing, ActivityKit, WidgetKit, App Intents.
- Server: Go, PostgreSQL 17 with pgx.
- Tests: XCUITest for UI.

**Spec:** `docs/superpowers/specs/2026-10-01-home-tracking-redesign-design.md`. Approved mockups are in `mockups/2026-10-01-home-redesign/`.

## Global Constraints

**Workspace and git**
- Do all work in worktree `.worktrees/home-redesign` on branch `feat/home-redesign`, created from `main` at or after `46c708a`.
- Never edit `main` directly. It carries other sessions' uncommitted work, including `TASKS.md` and `work/`.
- Commit messages are conventional (`feat(scope): …`, `fix(scope): …`, `test(scope): …`). End every one with a blank line and `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`. Never pass `--no-verify` or `--no-gpg-sign`.
- No `git push`, no deployment, and no install on a physical phone without explicit user approval.

**Joining rules**
- Break limits default to feeds 1800 s and sleep 900 s.
- Settings allow 5–120 minutes in 5-minute steps.
- A gap exactly equal to the limit joins. A gap one second longer does not.
- Diapers, Meds and Growth never block a join.
- A sleep in the gap blocks a feed join, whether the sleep is completed or running.
- Solids never join and are never joined.

**Timer rules**
- One feed timer and one sleep timer per baby at most. They may overlap.
- Breast parts under 30 s are discarded at stop. The existing `cleanedBreastComponents` does this.

**Protocol**
- Header: `X-Baby-Tracker-Sleep-Session: v1`. Server capability string: `sleep-session-v1`.
- Timer payload version **5** adds `session_id` (UUID string or JSON null). The key is required at v5, on both start and edit (`update`) payloads.
- Sleep payload version **2** adds `sleep_session_id` (UUID string or null). The key is required at v2.
- Migration file: `Server/migrations/000026_sleep_sessions.sql`. Schema version **26**. Main already has `000025_refresh_recovery.sql` at 25.

**Localisation**
- Every user-visible string lives in that module's `Localizable.xcstrings`.
- Every new key gets `en`, `de`, `fr`, `es`, `it`, `uk` values. Use the translation table in Task 13. If a key already exists in another catalog, reuse its translations verbatim.
- `python3 scripts/verify-localizations.py` must print `OK`.

**Copy**
- No "ago", "AGO", "Today so far", "Started", "Last feed", "Slept … before that", or "This side" on Home.
- Supplements is shown as **Meds**. The diaper "Both" is shown as **Mixed**.

**Simulator**
- Every simulator build and test uses the single shared device `platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853` (iPhone 17 Pro).
- Override the Makefile with `SIMULATOR='platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853'`.
- Never create, clone or select another simulator, including the iPhone 17e. If the device is missing or incompatible, stop and report.

**Accessibility**
- Every icon-only control has an accessibility label.
- Minimum hit target is 44 pt.
- `AccessibilityAuditTests` must pass.

**Test count**
- `make test` asserts `EXPECTED_TESTS` in `Makefile`. Each task that adds or removes package tests updates that number to the real new total in the same commit.
- Swift test functions use Swift Testing (`@Test`, `#expect`) with `@MainActor` where the store is involved.

**Store test setup**
- Start from `let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000)); let store = try BabyStore.inMemory(dateProvider: clock)`.
- Then call `try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)`.

## Review Focus

1. **Two caregivers race on the same feed.** Phone A stops Left while phone B, which still sees Left running, taps Right. Expected: one Left part, one Right part, same session, no duplicate or lost part. Covered by `twoCaregiversStopAndStartKeepOneSession` (Task 19).
2. **A joined feed or sleep crosses midnight.** It counts once, on the day its first part started: Home today count, today strip, and the Insights sleep total. Covered by `aJoinedSleepAcrossMidnightCountsOnItsFirstDay` (Task 10) and `aFeedSessionAcrossMidnightCountsOnce` (Task 10).
3. **Upgrade with an old-style running timer.** The timer has `completedComponents` and a current side from the Switch era. Home shows it as a running feed, Stop saves one multi-part feed exactly as before, and nothing crashes. Covered by `aLegacyMultiPartTimerStillStopsIntoOneFeed` (Task 9).
4. **A Live Activity left over from the previous build.** Its `ContentState` JSON lacks the new fields and must decode with defaults rather than crash the widget. Covered by `contentStateFromThePreviousBuildDecodes` (Task 16).
5. **The break limit changes mid-break.** Join decisions and the break windows on Home and the Live Activity use the current limit at their next evaluation. Values outside 5–120 are clamped. Covered by `changingTheLimitAppliesToTheNextDecision` and `limitsAreClamped` (Task 7).

---

## File Structure

**Server (Go)**
- `Server/migrations/000026_sleep_sessions.sql` (new): adds `sleep_entries.payload_version` and `sleep_entries.sleep_session_id`, and `active_timers.session_id`. Widens the checks, copies the privilege manifest, bumps the schema.
- `Server/internal/migrate/compatibility.json`, `compatibility_test.go`: schema 26.
- `Server/internal/protocol/entities.go`, `mutations.go`, `canonical.go`, `auth.go`, `family.go`, `sync.go`: v5 timer and v2 sleep shapes, and the capability.
- `Server/internal/records/sleeps.go`, `timers.go`, `repository.go`, `snapshot.go`, `sessions.go` (new: `sessionOwnerMismatch`).
- `Server/internal/sync/sleep_session_boundary.go` (new), `mutation.go`, `paging.go`, `snapshot.go`; `Server/internal/httpapi/sync.go`.
- Tests:
  - `Server/internal/records/sleep_sessions_test.go` (new)
  - `Server/internal/sync/sleep_session_boundary_test.go` (new)
  - `Server/internal/protocol/sleep_session_test.go` (new)
  - schema tests that pin version 24, bumped to 25
- `Server/api/openapi.yaml`, `Server/api/fixtures/v1/*.json`, `docs/protocol/v1.md`.

**Client protocol and store (`Packages/BabyTracker/Sources`)**
- `BabyProtocol/ContentHash.swift`: sleep v2 and timer v5 hashes.
- `BabyStore/Models/SchemaV8.swift` (new), `BabyStoreMigrationPlan.swift`, `Schema.swift`: V8 adds `ActiveTimer.sessionID`, `ActiveTimer.bottleContentsRaw`, and `SleepEntry.sleepSessionID`.
- `BabyStore/Models/ActiveTimer.swift`, `SleepEntry.swift`.
- `BabyStore/MutationFactory.swift`, `EntryPayload.swift`, `TimerIntentReducer.swift`, `BabyStore+RemoteApply.swift`, `BabyStore+RemoteTimers.swift`.
- `BabySync/FamilySyncHTTPClient.swift`: the header.
- `BabyStore/TrackingPreferences.swift` (new): break limits in App Group defaults.
- `BabyStore/SessionJoin.swift` (new): the join decision.
- `BabyStore/SleepSessions.swift` (new): projection, combine, separate.
- `BabyStore/BabyStore+Parts.swift` (new): `startPart`, `stopPart`, `detachFromSession`, `logBreast`, `setBottleVolume`.
- `BabyStore/BabyStore+Feeding.swift`, `BabyStore+Sleep.swift`, `BabyStore+Diaper.swift`, `HomeSnapshot.swift`, `BabyStore+Queries.swift`, `WidgetStatusSummary.swift`, `LogDefaults.swift`.

**Client UI**
- `BabyUI/Resources/Icons.xcassets` (new, SVG template images), `BabyUI/BabyIcon.swift` (new), `Palette.swift`. `TrackerIcon.swift` is rewritten to use `BabyIcon`.
- `HomeFeature/`:
  - `HomeScreen.swift` (new, pure projection), `HomeViewModel.swift` (rewritten)
  - `HomeView.swift`, `HeroView.swift` (new), `StatusLineView.swift` (new), `TodayStripView.swift` (new), `HomeDock.swift` (new)
  - Delete: `IdleStateView.swift`, `RunningTimerView.swift`, `QuickLogDock.swift`, `StopDock.swift`, `FeedAction.swift`, `TimerDial.swift`
- `LoggingFeature/`:
  - `DiaperSheet.swift`, `BottleSheet.swift` (rewritten), `MoreSheet.swift` (new), `PastEntrySheet.swift` (new), `LogCoordinator.swift`
  - Delete: `FeedMenuSheet.swift`
- `HistoryFeature/HistoryViewModel.swift`, `HistoryView.swift`: combine fix and sleep combine/separate.
- `InsightsFeature/InsightsViewModel.swift`, `AskFeature/AskFactBuilder.swift`: sleep sessions.
- `SettingsFeature/SettingsView.swift`, `SettingsViewModel.swift`: Tracking section.
- `Apps/BabyTrackerApp/RootView.swift`, `BabyTrackerShortcuts.swift`, `UITestSupport.swift`.
- `Apps/SharedIntents/LogIntents.swift`, `SiriIntents.swift`.
- `Apps/BabyTrackerWidgets/FeedActivityWidget.swift`, `QuickLogWidget.swift`, `StatusWidget.swift`.
- `BabyStore/TimerActivityAttributes.swift`, `LiveActivityController.swift`.
- `UITests/BabyTrackerUITests/*`.

---

## Phase A — Server

### Task 1: Protocol shapes for timer v5 and sleep v2

**Files:**
- Modify: `Server/internal/protocol/entities.go` (TimerStartPayload 146-180, SleepPayload 90-97, TimerEditPayload 247-261, decodeResponseEntityPayload 345-424)
- Modify: `Server/internal/protocol/mutations.go` (version check ~71 and ~86, `validateRequiredPayloadShape` ~179-198)
- Modify: `Server/internal/protocol/canonical.go` (SleepPayload and TimerStartPayload cases, and the edit payload case)
- Modify: `Server/internal/protocol/auth.go` (capability constant and `knownCapabilities`), `Server/internal/protocol/family.go:11-13` (`ServerCapabilities`)
- Test: `Server/internal/protocol/sleep_session_test.go` (new)

**Interfaces:**
- Produces, in package `protocol`:
  - `const CapabilitySleepSessionV1 = "sleep-session-v1"`
  - `TimerStartPayload.SessionID *string`, emitted as `session_id` only when `ComponentVersion >= 5`
  - `TimerEditPayload.SessionID *string` and `TimerEditPayload.Version uint32`; `session_id` is emitted only when `Version >= 5`
  - `SleepPayload.SleepSessionID *string` and `SleepPayload.PayloadVersion uint32`; `sleep_session_id` is emitted only when `PayloadVersion >= 2`
  - Mutation decode accepts `payload_version` 5 for `entity_kind == "timer"` only, and 2 for `"sleep"`. Feeds stay at ≤ 4. Every other kind stays at 1.

- [ ] **Step 1: Write the failing tests**

```go
package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func mutationJSON(kind, op string, version int, payload string) []byte {
	return []byte(`{"mutation_id":"11111111-1111-4111-8111-111111111111","entity_id":"22222222-2222-4222-8222-222222222222",` +
		`"entity_kind":"` + kind + `","operation":"` + op + `","payload_version":` + strings.TrimSpace(json.Number(itoa(version)).String()) +
		`,"observed_entity_version":0,"client_mutation_time":"2026-10-01T10:00:00.000000Z","payload":` + payload + `}`)
}

func itoa(v int) string { b, _ := json.Marshal(v); return string(b) }

const babyID = "33333333-3333-4333-8333-333333333333"
const sessionID = "44444444-4444-4444-8444-444444444444"

func TestTimerStartV5CarriesSessionID(t *testing.T) {
	raw := mutationJSON("timer", "timer_start", 5, `{"baby_id":"`+babyID+`","timer_kind":"feed","started_at":"2026-10-01T10:00:00.000000Z",`+
		`"time_zone_id":"Europe/Zurich","completed_components":[],"current_side":"left","current_side_started_at":"2026-10-01T10:00:00.000000Z",`+
		`"current_bottle_started_at":null,"session_id":"`+sessionID+`"}`)
	m, err := DecodeMutation(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	p := m.Payload.(TimerStartPayload)
	if p.SessionID == nil || *p.SessionID != sessionID || p.ComponentVersion != 5 {
		t.Fatalf("session not decoded: %+v", p)
	}
	out, _ := json.Marshal(p)
	if !strings.Contains(string(out), `"session_id":"`+sessionID+`"`) {
		t.Fatalf("session not encoded: %s", out)
	}
}

func TestTimerStartV5RequiresSessionKey(t *testing.T) {
	raw := mutationJSON("timer", "timer_start", 5, `{"baby_id":"`+babyID+`","timer_kind":"sleep","started_at":"2026-10-01T10:00:00.000000Z",`+
		`"time_zone_id":"Europe/Zurich","completed_components":[],"current_side":null,"current_side_started_at":null,"current_bottle_started_at":null}`)
	if _, err := DecodeMutation(raw); err == nil {
		t.Fatal("v5 start without session_id must be invalid")
	}
}

func TestSleepV2CarriesSleepSessionIDAndV1ForbidsIt(t *testing.T) {
	body := `{"baby_id":"` + babyID + `","started_at":"2026-10-01T10:00:00.000000Z","ended_at":"2026-10-01T11:00:00.000000Z",` +
		`"time_zone_id":"Europe/Zurich","note":null,"kind_override":null,"sleep_session_id":"` + sessionID + `"}`
	m, err := DecodeMutation(mutationJSON("sleep", "create", 2, body))
	if err != nil {
		t.Fatalf("decode v2: %v", err)
	}
	p := m.Payload.(SleepPayload)
	if p.SleepSessionID == nil || *p.SleepSessionID != sessionID || p.PayloadVersion != 2 {
		t.Fatalf("sleep session not decoded: %+v", p)
	}
	if _, err := DecodeMutation(mutationJSON("sleep", "create", 1, body)); err == nil {
		t.Fatal("v1 sleep must not carry sleep_session_id")
	}
	if _, err := DecodeMutation(mutationJSON("diaper", "create", 2, `{}`)); err == nil {
		t.Fatal("diaper v2 stays invalid")
	}
	if _, err := DecodeMutation(mutationJSON("feed", "create", 5, `{}`)); err == nil {
		t.Fatal("feed v5 stays invalid")
	}
}

func TestCanonicalHashIncludesSessionFieldsOnlyAtNewVersions(t *testing.T) {
	s := sessionID
	v1, _ := CanonicalPayloadObject(SleepPayload{BabyID: babyID, StartedAt: "2026-10-01T10:00:00.000000Z", TimeZoneID: "UTC", PayloadVersion: 1}, true)
	v2, _ := CanonicalPayloadObject(SleepPayload{BabyID: babyID, StartedAt: "2026-10-01T10:00:00.000000Z", TimeZoneID: "UTC", PayloadVersion: 2, SleepSessionID: &s}, true)
	if _, ok := v1["sleep_session_id"]; ok {
		t.Fatal("v1 sleep hash must not include sleep_session_id")
	}
	if v2["sleep_session_id"] != s {
		t.Fatalf("v2 sleep hash must include sleep_session_id: %#v", v2)
	}
}

func TestServerAdvertisesSleepSession(t *testing.T) {
	for _, c := range ServerCapabilities() {
		if c == CapabilitySleepSessionV1 {
			return
		}
	}
	t.Fatal("sleep-session-v1 not advertised")
}
```

Before writing the tests, check two names in `canonical.go`. If the canonical projection function is not named `CanonicalPayloadObject(payload, includeBabyID bool) (map[string]any, error)`, use the existing exported name; `grep -n "^func " Server/internal/protocol/canonical.go` shows it. Likewise, use the real mutation decoder name, which `grep -n "^func Decode" Server/internal/protocol/*.go` shows.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `cd Server && go test ./internal/protocol -run 'TimerStartV5|SleepV2|CanonicalHashIncludesSession|AdvertisesSleepSession' -count=1`
Expected: FAIL. Fields such as `SessionID` and `SleepSessionID` are undefined, and the version-5 decode is invalid.

- [ ] **Step 3: Implement**

1. In `entities.go`, add the new fields:

```go
type TimerStartPayload struct {
	// … existing fields …
	SessionID        *string `json:"-"`
	ComponentVersion uint32  `json:"-"`
}
```

In `TimerStartPayload.MarshalJSON`, after the `>= 3` block, add:

```go
		if payload.ComponentVersion >= 5 {
			common["session_id"] = payload.SessionID // nil marshals as JSON null
		}
```

Then extend `SleepPayload`:

```go
type SleepPayload struct {
	BabyID         string  `json:"baby_id"`
	StartedAt      string  `json:"started_at"`
	EndedAt        *string `json:"ended_at"`
	TimeZoneID     string  `json:"time_zone_id"`
	Note           *string `json:"note"`
	KindOverride   *string `json:"kind_override"`
	SleepSessionID *string `json:"-"`
	PayloadVersion uint32  `json:"-"`
}

func (p SleepPayload) MarshalJSON() ([]byte, error) {
	out := map[string]any{"baby_id": p.BabyID, "started_at": p.StartedAt, "ended_at": p.EndedAt,
		"time_zone_id": p.TimeZoneID, "note": p.Note, "kind_override": p.KindOverride}
	if p.PayloadVersion >= 2 {
		out["sleep_session_id"] = p.SleepSessionID
	}
	return json.Marshal(out)
}
```

Add `SessionID *string` and `Version uint32` to `TimerEditPayload`. In its `MarshalJSON`, write `session_id` when `Version >= 5`.

2. In `mutations.go`:
   - Accept versions 1–5 in the `parseExactUint` check.
   - Replace the "v2+ only for feed/timer" rule with an explicit maximum per kind:

```go
	maxVersion := map[string]uint64{"feed": 4, "timer": 5, "sleep": 2}[mutation.EntityKind]
	if maxVersion == 0 {
		maxVersion = 1
	}
	if payloadVersion > maxVersion {
		return Mutation{}, errInvalidRequest
	}
```

   - In `decodeMutationPayload`, the sleep branch sets `PayloadVersion = version`. When `version >= 2`, it decodes `sleep_session_id` as a required key (null allowed; non-null must pass `ValidUUID`). When `version == 1`, a present `sleep_session_id` key is `errInvalidRequest`.
   - The timer start and `update` (edit) branches set `ComponentVersion = version` and `Version = version`, and do the same with `session_id` at `version >= 5`.
   - Wherever existing code tests `version >= 4` or `version == 4` for timer features (e.g. `timer_pause` decodes only when `version >= 4`), make sure version 5 behaves as 4 plus the session field. Run `grep -n "version" Server/internal/protocol/mutations.go` and check each comparison.

3. In `decodeResponseEntityPayload`, set the versions from the keys present:
   - sleep: `PayloadVersion = 2` when `sleep_session_id` is present, otherwise 1;
   - timer: `ComponentVersion = 5` when `session_id` is present.

4. In `canonical.go`:
   - Sleep case: add `"sleep_session_id": nullableString(value.SleepSessionID)` when `value.PayloadVersion >= 2`.
   - Timer start case: add `"session_id": nullableString(value.SessionID)` when `value.ComponentVersion >= 5`.
   - Edit case: same rule, at `Version >= 5`.

5. In `auth.go`, add `CapabilitySleepSessionV1 = "sleep-session-v1"` and include it in `knownCapabilities`. In `family.go`, append it to `ServerCapabilities()`. That gives 14 capabilities, which is within the cap of 16.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `cd Server && go test ./internal/protocol -count=1`
Expected: PASS for the whole package, including the existing fixture and canonical tests.

- [ ] **Step 5: Commit**

```bash
git add Server/internal/protocol
git commit -m "feat(protocol): timer v5 session_id and sleep v2 sleep_session_id

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

### Task 2: Migration 26 and the schema manifest

**Files:**
- Create: `Server/migrations/000026_sleep_sessions.sql`
- Modify: `Server/internal/migrate/compatibility.json`, `Server/internal/migrate/compatibility_test.go` (checks at ~50, ~131, ~205-235)
- Modify:
  - every schema test that pins 24: `Server/internal/database/{snapshot,import,records,delivery,timer,identity,family}_schema_test.go`, `roles_test.go` (134,179), `cmd/baby-tracker-overlap-check/main_test.go`
  - `internal/database/timer_schema_test.go:12-17` and `records_schema_test.go:20` (column lists)

**Interfaces:**
- Produces:
  - `sleep_entries.payload_version smallint NOT NULL DEFAULT 1 CHECK (payload_version IN (1,2))`
  - `sleep_entries.sleep_session_id uuid NULL`
  - `active_timers.session_id uuid NULL`
  - `active_timers.component_version IN (1,2,3,4,5)`
  - schema version 25

- [ ] **Step 1: Write the migration**

```sql
-- Session links for one-part timers and joined sleeps.
ALTER TABLE public.sleep_entries ADD COLUMN payload_version smallint NOT NULL DEFAULT 1
    CHECK (payload_version IN (1,2));
ALTER TABLE public.sleep_entries ADD COLUMN sleep_session_id uuid;
ALTER TABLE public.sleep_entries ADD CONSTRAINT sleep_session_requires_v2
    CHECK (sleep_session_id IS NULL OR payload_version = 2);
ALTER TABLE public.active_timers ADD COLUMN session_id uuid;
ALTER TABLE public.active_timers DROP CONSTRAINT active_timers_component_version_check;
ALTER TABLE public.active_timers ADD CONSTRAINT active_timers_component_version_check
    CHECK (component_version IN (1,2,3,4,5));
ALTER TABLE public.active_timers ADD CONSTRAINT active_timers_session_requires_v5
    CHECK (session_id IS NULL OR component_version = 5);
ALTER TABLE public.active_timers DROP CONSTRAINT active_timers_state_shape;
ALTER TABLE public.active_timers ADD CONSTRAINT active_timers_state_shape CHECK (
    (pending_volume_ml IS NULL) = (pending_contents IS NULL)
    AND ((timer_kind='feed' AND (current_side IS NULL)=(current_side_started_at IS NULL)
          AND (current_bottle_started_at IS NULL OR (component_version IN (3,4,5) AND current_side IS NULL)))
         OR (timer_kind='sleep' AND current_side IS NULL AND current_side_started_at IS NULL
             AND current_bottle_started_at IS NULL AND pending_volume_ml IS NULL AND pending_contents IS NULL))
    AND (completed_record_id IS NULL OR tombstoned_at IS NOT NULL)
);
CREATE INDEX sleep_entries_session ON public.sleep_entries(family_id, sleep_session_id)
    WHERE sleep_session_id IS NOT NULL;
CREATE INDEX active_timers_session ON public.active_timers(family_id, session_id)
    WHERE session_id IS NOT NULL;

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
 SELECT 26,principal,object_kind,object_identity,privileges
 FROM public.schema_privilege_manifest WHERE schema_version=25;
UPDATE public.schema_compatibility SET current_version=26,
 introduced_by='000026_sleep_sessions.sql',recorded_at=now() WHERE singleton;
```

Before relying on these statements, confirm two things:
- The latest definition of `active_timers_state_shape`: `grep -n "active_timers_state_shape" Server/migrations/*.sql` must show 000022 as the last one. If a later migration redefined it, start from that text instead.
- That the two new indexes need no privilege-manifest rows. Check whether `000024_growth.sql` added manifest rows for indexes. If it did, mirror that.

- [ ] **Step 2: Regenerate the manifest**

Run: `cd Server && go run ./cmd/baby-tracker-manifest --migrations migrations --previous internal/migrate/compatibility.json --output internal/migrate/compatibility.json --min 26 --max 26`
Expected: `compatibility.json` now has `"min":26,"max":26` and a 26th `known` entry for `000026_sleep_sessions.sql` with its sha256. Check the flags with `go run ./cmd/baby-tracker-manifest --help` first.

- [ ] **Step 3: Update the version pins**

Main already holds schema 25 (`000025_refresh_recovery.sql`), so this migration is 26.

- Change every hard-coded schema expectation that currently pins `25` to `26`. Find them with `grep -rn "25" Server/internal/database/*_schema_test.go Server/internal/database/roles_test.go Server/internal/migrate/compatibility_test.go Server/cmd/baby-tracker-overlap-check/main_test.go`, and check each hit is a schema version rather than an unrelated number.
- The newest "follows" test in `compatibility_test.go` asserts `Min == Max == 25`. Add `TestSleepSessionsFollowRefreshRecoveryAtSchemaTwentySix`. It asserts:
  - `Min == 26 && Max == 26 && len(Known) == 26`
  - `Known[25].Name == "000026_sleep_sessions.sql"`
  - `Known[:25]` alone is rejected
- Update the previous newest test so it no longer asserts that 25 is the maximum.
- In `roles_test.go`, the simulation that copies the current version to the next one must move from 25→26 to 26→27.
- Add `payload_version` and `sleep_session_id` to the sleep column list, and `session_id` to the timer column list.

- [ ] **Step 4: Run the server suite**

Run: `make server-test`
Expected: every package `ok`. Also run `bash Server/scripts/check-migration-overlap.sh` if the Makefile or CI uses it (`grep -rn overlap Makefile Server/scripts`).

- [ ] **Step 5: Commit**

```bash
git add Server/migrations/000026_sleep_sessions.sql Server/internal/migrate Server/internal/database Server/cmd
git commit -m "feat(server): migration 26 adds sleep and timer session links

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

### Task 3: Records — store, copy and validate session IDs

**Files:**
- Modify: `Server/internal/records/sleeps.go` (GetSleep, putSleep)
- Modify: `Server/internal/records/timers.go` (GetTimer 28-104, putTimer 950-1009, editTimer 395-516, completedTimerPayload 731-815)
- Modify: `Server/internal/records/repository.go:186-206` (per-record version rejection, ownership check)
- Create: `Server/internal/records/sessions.go`
- Modify: `Server/internal/records/snapshot.go` (sleep JSON ~108, timer JSON ~148-158)
- Test: `Server/internal/records/sleep_sessions_test.go` (new)

**Interfaces:**
- Consumes the Task 1 fields and the Task 2 columns.
- Produces `func sessionOwnerMismatch(ctx context.Context, tx pgx.Tx, familyID, babyID, kind, sessionID string) (bool, error)`.
  - `kind` is `"feed"` or `"sleep"`.
  - It returns true when the ID is already used by a live record or live timer of another baby or of the other kind.
- A stop of a v5 timer gives:
  - a feed completion with `FeedingSessionID = timer.SessionID`, `ComponentVersion = 4`;
  - a sleep completion with `PayloadVersion = 2`, `SleepSessionID = timer.SessionID`.

- [ ] **Step 1: Write the failing DB tests**

```go
package records_test

import (
	"testing"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

func sessionTimerStart(t *testing.T, h *testkit.Harness, n int, timerID, kind string, session *string) protocol.Mutation {
	t.Helper()
	fixture := "timer-feed.json"
	if kind == "sleep" {
		fixture = "timer-sleep.json"
	}
	start := testkit.MustMutation(t, fixture)
	start.MutationID, start.EntityID, start.PayloadVersion = mutationID(n), timerID, 5
	p := start.Payload.(protocol.TimerStartPayload)
	p.ComponentVersion, p.SessionID = 5, session
	start.Payload = p
	return start
}

func TestStopCopiesTimerSessionIntoTheCompletedSleep(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	session := "aaaaaaaa-0000-4000-8000-000000000001"
	start := sessionTimerStart(t, h, 2501, "aaaaaaaa-0000-4000-8000-0000000000a1", "sleep", &session)
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, start); err != nil {
		t.Fatal(err)
	}
	stop := testkit.MustMutation(t, "timer-stop.json") // use the repo's stop fixture; see timers_test.go for the exact name
	stop.MutationID, stop.EntityID, stop.PayloadVersion = mutationID(2502), start.EntityID, 5
	stop.Payload = protocol.TimerStopPayload{BabyID: start.Payload.(protocol.TimerStartPayload).BabyID,
		CompletedRecordID: "aaaaaaaa-0000-4000-8000-0000000000b1", StoppedAt: "2026-10-01T11:00:00.000000Z"}
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, stop)
	if err != nil {
		t.Fatal(err)
	}
	sleep := outcome.RelatedEntities[0].Payload.(protocol.SleepPayload)
	if sleep.PayloadVersion != 2 || sleep.SleepSessionID == nil || *sleep.SleepSessionID != session {
		t.Fatalf("completed sleep lost its session: %+v", sleep)
	}
}

func TestStopCopiesTimerSessionIntoTheCompletedFeed(t *testing.T) {
	// Same shape as above with kind "feed" and a breast side running for ≥ 30 s,
	// asserting FeedPayload.ComponentVersion == 4 and *FeedingSessionID == session.
}

func TestTimerEditCanDetachTheSession(t *testing.T) {
	// Start v5 feed timer with session S; apply an "update" (edit) mutation v5 with
	// TimerEditPayload{BabyID, Components: [], SessionID: nil, Version: 5};
	// GetTimer must return SessionID == nil; stopping yields FeedingSessionID == nil.
}

func TestASessionIDOwnedByAnotherBabyOrKindIsRefused(t *testing.T) {
	// Seed baby A and baby B. Create a sleep for A with sleep_session_id S (v2).
	// 1. A feed timer start for A with session S -> outcome.Status "rejected", code "record_parent_mismatch".
	// 2. A sleep v2 create for B with sleep_session_id S -> rejected, "record_parent_mismatch".
	// 3. A sleep v2 create for A with sleep_session_id S -> applied.
}

func TestALegacyV1SleepUpdateCannotFlattenALinkedSleep(t *testing.T) {
	// Create sleep v2 with session S; apply a v1 "update" of the same sleep.
	// Expect rejected "unsupported_payload_version" and no journal row
	// (h.FamilySequence unchanged).
}
```

Write the four commented tests in full, following the first test's pattern. They use the helpers in `records/timers_test.go`: `seedBaby`, `mutationID`, `startFeed`. If `seedBaby` creates only one baby, add `seedSecondBaby(t, h) string` in the new test file, copying `seedBaby`'s body with a different baby ID. Take the stop fixture name from `ls Server/api/fixtures/v1`.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `BT_TEST_RUN='Session' BT_TEST_PACKAGES=./internal/records make server-test`
Expected: FAIL. The session is not persisted or copied.

- [ ] **Step 3: Implement**

1. In `sleeps.go`:
   - `GetSleep` also selects `payload_version, sleep_session_id::text`, and sets `p.PayloadVersion` and `p.SleepSessionID`.
   - `putSleep` writes `payload_version = max(1, p.PayloadVersion)` and `sleep_session_id`, in both the INSERT and the `ON CONFLICT DO UPDATE` lists.

2. In `timers.go`:
   - `GetTimer` selects `session_id::text` into `payload.SessionID`.
   - `putTimer` writes `session_id` on insert and update.
   - `editTimer` copies `edit.SessionID` into `edited.SessionID` when `command.Mutation.PayloadVersion >= 5`, before the `reflect.DeepEqual(edited, before)` no-op check, so a detach is a real change.
   - In `completedTimerPayload`:

```go
	if timer.TimerKind == "sleep" {
		payload := protocol.SleepPayload{BabyID: timer.BabyID, StartedAt: timer.StartedAt, EndedAt: &ended,
			TimeZoneID: timer.TimeZoneID, Note: stop.Note, KindOverride: stop.SleepKindOverride, PayloadVersion: 1}
		if timer.ComponentVersion >= 5 {
			payload.PayloadVersion, payload.SleepSessionID = 2, timer.SessionID
		}
		// … existing return …
	}
```

   For both feed branches, after `ComponentVersion` is computed, add:

```go
	if timer.ComponentVersion >= 5 {
		feed.ComponentVersion = 4
		feed.FeedingSessionID = timer.SessionID
	}
```

   Read `completedFeedVersionForTimer` first. It must return 4 for timer version 5; fix it if it compares with `== 4`.

3. Create `sessions.go`:

```go
package records

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// sessionOwnerMismatch reports whether sessionID already groups records of
// another baby or of the other kind in this family. A session ID is opaque but
// must never stitch a sleep to a feed or one baby to another.
func sessionOwnerMismatch(ctx context.Context, tx pgx.Tx, familyID, babyID, kind, sessionID string) (bool, error) {
	var mismatch bool
	err := tx.QueryRow(ctx, `SELECT
	  EXISTS(SELECT 1 FROM feed_entries WHERE family_id=$1 AND tombstoned_at IS NULL
	         AND (id=$4 OR feeding_session_id=$4) AND ($3<>'feed' OR baby_id<>$2))
	  OR EXISTS(SELECT 1 FROM sleep_entries WHERE family_id=$1 AND tombstoned_at IS NULL
	         AND (id=$4 OR sleep_session_id=$4) AND ($3<>'sleep' OR baby_id<>$2))
	  OR EXISTS(SELECT 1 FROM active_timers WHERE family_id=$1 AND tombstoned_at IS NULL
	         AND session_id=$4 AND (timer_kind<>$3 OR baby_id<>$2))`,
		familyID, babyID, kind, sessionID).Scan(&mismatch)
	return mismatch, err
}
```

4. In `repository.go`:
   - Replace the feed-only query at 194-206 with a call to `sessionOwnerMismatch(ctx, tx, command.Grant.FamilyID, feed.BabyID, "feed", *feed.FeedingSessionID)`.
   - Add the same check for `m.EntityKind == "sleep"` when `PayloadVersion >= 2 && SleepSessionID != nil`.
   - Next to the existing feed rule, add the per-record legacy guard:

```go
	if existing != nil && m.EntityKind == "sleep" && m.Operation == "update" {
		if sleep, ok := existing.Payload.(protocol.SleepPayload); ok && sleep.PayloadVersion > m.PayloadVersion {
			return rejection(m, "rejected", "unsupported_payload_version", existing), nil
		}
	}
```

5. In `timers.go`, `startTimer` and `editTimer` call `sessionOwnerMismatch(..., timer.TimerKind, *SessionID)` when the session is non-nil. A mismatch gives `record_parent_mismatch`: use `rejection(...)` for start, and `refuse("record_parent_mismatch")` for edit.

6. In `snapshot.go`:
   - Sleep JSON: when `s.payload_version>=2`, add `'sleep_session_id', s.sleep_session_id::text` to `jsonb_build_object`. The `||` concatenation pattern used for feeds at ~95 does this.
   - Timer JSON: when `component_version>=5`, add `'session_id'` the same way.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `BT_TEST_PACKAGES=./internal/records make server-test`
Expected: `ok babytracker/server/internal/records`.

- [ ] **Step 5: Commit**

```bash
git add Server/internal/records
git commit -m "feat(records): persist and copy timer and sleep session links

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

### Task 4: Sync boundary, header, API docs

**Files:**
- Create: `Server/internal/sync/sleep_session_boundary.go`
- Modify: `Server/internal/sync/mutation.go:150-165`, `paging.go:195-266`, `snapshot.go:39-42`
- Modify: `Server/internal/protocol/sync.go:269,337` (`SupportsSleepSessions` on `SyncRequest`/`SnapshotRequest`)
- Modify: `Server/internal/httpapi/sync.go:63-66,171,197`
- Modify:
  - `Server/api/openapi.yaml`: header, `TimerStartPayload`, `TimerEditPayload`, `SleepPayload`
  - `Server/api/fixtures/server-info.json`: add the missing capabilities
  - `docs/protocol/v1.md`: feature-header table, timer v5, sleep v2, pause note
- Test: `Server/internal/sync/sleep_session_boundary_test.go` (new)

**Interfaces:**
- Produces `func requireSleepSessionSupport(ctx context.Context, tx pgx.Tx, familyID string, supported bool) error` and the `SupportsSleepSessions bool` request flags.
- `Service.Apply` passes `true` for the new flag, like the other flags (`sync/mutation.go:92-93`).

- [ ] **Step 1: Write the failing test**

Copy `sync/growth_boundary_test.go`. Rename the test `TestSleepSessionBoundaryRefusesOlderClientsWithoutProgress`. Change the seed so it creates a v2 sleep with a `sleep_session_id` through `h.Service.Apply`. Then call `ApplyAndPull` (or the pull helper that test uses) with `SupportsSleepSessions = false`, and assert exactly what the growth test asserts:

```go
	if !errors.Is(err, familysync.ErrUnsupportedCapability) || response.HighWater.Sequence != 0 || len(response.Changes) != 0 {
		t.Fatalf("an older client must be refused without progress: err=%v response=%+v", err, response)
	}
```

Add a second case that seeds only a v5 timer (no sleep) and makes the same assertion.

- [ ] **Step 2: Run the test and confirm it fails**

Run: `BT_TEST_RUN='SleepSessionBoundary' BT_TEST_PACKAGES=./internal/sync make server-test`
Expected: FAIL, because the request flag is unknown.

- [ ] **Step 3: Implement**

```go
package sync

import (
	"babytracker/server/internal/records"
	"context"

	"github.com/jackc/pgx/v5"
)

func requireSleepSessionSupport(ctx context.Context, tx pgx.Tx, familyID string, supported bool) error {
	if err := records.LockFamilyForUpdate(ctx, tx, familyID); err != nil {
		return err
	}
	if supported {
		return nil
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sleep_entries WHERE family_id=$1 AND payload_version=2)
		OR EXISTS(SELECT 1 FROM active_timers WHERE family_id=$1 AND component_version=5)`, familyID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrUnsupportedCapability
	}
	return nil
}
```

Wire it in. Each call goes right next to the corresponding `requireFeedSessionSupport` call, with the same argument shape:

1. In `mutation.go`:
   - Call `requireSleepSessionSupport(..., request.SupportsSleepSessions)`.
   - Refuse the batch when the client lacks the flag and sends a mutation with `(EntityKind=="timer" && PayloadVersion>=5) || (EntityKind=="sleep" && PayloadVersion>=2)`. Model this on `if m.PayloadVersion >= 4 && !supportsFeedSessions`.
2. In `paging.go` and `snapshot.go`, add the same call.
3. In `protocol/sync.go`, add `SupportsSleepSessions bool` to both request structs.
4. In `httpapi/sync.go`:
   - Add `supportsSleepSessions(r)`, reading `X-Baby-Tracker-Sleep-Session` with the same exact-`v1` rule.
   - Set the new flag everywhere `SupportsFeedSessions` is set.

Update `openapi.yaml`:
- Add a header parameter next to `X-Baby-Tracker-Growth`.
- Add `session_id` (nullable uuid) to `TimerStartPayload` and `TimerEditPayload`, described as "required when payload_version is 5".
- Add `sleep_session_id` to `SleepPayload`, described as "required when payload_version is 2".

In `docs/protocol/v1.md`:
- Add a "Feature headers" table listing Supplements, Growth, Timed-Bottle, Feeding-Session and Sleep-Session.
- Document timer v5, sleep v2, and the fact that `timer_switch_side` and `timer_pause` remain accepted from older clients.
- Fix the stale line that says the timer edit uses `payload_version` 2 only.

- [ ] **Step 4: Run the server gates**

Run: `make server-test && make server-smoke && make server-protocol-e2e`
Expected: all pass. The e2e client may need the new header added where it sets `X-Baby-Tracker-Feeding-Session`; `grep -rn "Feeding-Session" Server/cmd Server/tests` finds it.

- [ ] **Step 5: Commit**

```bash
git add Server/internal Server/api docs/protocol/v1.md Server/cmd Server/tests
git commit -m "feat(sync): sleep-session boundary and X-Baby-Tracker-Sleep-Session header

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

## Phase B — Client data

### Task 5: Client wire shapes, content hash, and header

**Files:**
- Modify: `Packages/BabyTracker/Sources/BabyProtocol/ContentHash.swift` (sleep ~71-77, timer ~109-119)
- Modify: `Packages/BabyTracker/Sources/BabyStore/MutationFactory.swift:48-68,101-115`
- Modify: `Packages/BabyTracker/Sources/BabySync/FamilySyncHTTPClient.swift:54-58`
- Modify: `Packages/BabyTracker/Sources/BabySync/EndpointIdentity.swift:68-70` (add `advertisesSleepSession`)
- Test: `Packages/BabyTracker/Tests/BabyProtocolTests/ContentHashTests.swift`, `Packages/BabyTracker/Tests/BabySyncTests/FamilySyncEngineTests.swift` (header assertion; find the existing header test with `grep -rn "Feeding-Session" Packages/BabyTracker/Tests`)

**Interfaces:**
- `MutationFactory.payloadVersion(for:payload:)` returns:
  - `5` for `.timer` when `payload["session_id"] != nil` (otherwise 4);
  - `2` for `.sleep` when `payload["sleep_session_id"] != nil` (otherwise 1).
- `ContentHash` "sleep" at version 2 adds `sleep_session_id`; "timer" in `2...5` adds `session_id` at version 5.

- [ ] **Step 1: Write the failing tests**

```swift
@Test func sleepV2HashIncludesTheSessionLinkAndMatchesTheServerVector() throws {
    let payload: JSONValue = .object([
        "baby_id": .string("33333333-3333-4333-8333-333333333333"),
        "started_at": .string("2026-10-01T10:00:00.000000Z"),
        "ended_at": .string("2026-10-01T11:00:00.000000Z"),
        "time_zone_id": .string("UTC"), "note": .null, "kind_override": .null,
        "sleep_session_id": .string("44444444-4444-4444-8444-444444444444"),
    ])
    let v2 = try ContentHash.canonicalContentBytes(entityKind: "sleep", payloadVersion: 2, payload: payload)
    let v1 = try ContentHash.canonicalContentBytes(entityKind: "sleep", payloadVersion: 1, payload: payload)
    #expect(String(decoding: v2, as: UTF8.self).contains("sleep_session_id"))
    #expect(!String(decoding: v1, as: UTF8.self).contains("sleep_session_id"))
}

@Test func timerV5HashIncludesTheSessionLink() throws {
    // Same pattern: a v5 timer start payload with "session_id"; v5 bytes contain it, v4 bytes do not.
}
```

Also do these:
- Add a vector to `Server/api/fixtures/v1/canonical-vectors.json` for a v2 sleep and a v5 timer, and extend `ProtocolFixtureConformanceTests` so the Swift and Go hashes must agree. The Go side reads the same file in its canonical test; add the vector there too.
- In the header test, assert `request.value(forHTTPHeaderField: "X-Baby-Tracker-Sleep-Session") == "v1"`.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `cd Packages/BabyTracker && swift test --filter 'ContentHashTests|ProtocolFixtureConformanceTests|FamilySyncEngineTests'`
Expected: FAIL. The hash lacks the field and the header is missing.

- [ ] **Step 3: Implement**

In `ContentHash.swift`, add the sleep v2 field:

```swift
        case "sleep":
            var fields: [String: JSONValue] = [
                "ended_at": p.optionalTime("ended_at"),
                "kind_override": p.optionalText("kind_override"),
                "note": p.optionalText("note"),
                "started_at": p.time("started_at"),
                "time_zone_id": p.text("time_zone_id"),
            ]
            if payloadVersion >= 2 { fields["sleep_session_id"] = p.optionalText("sleep_session_id") }
            return .object(fields)
```

For the timer, change `case "timer" where (2...4).contains(payloadVersion)` to `(2...5)`. Then add `if payloadVersion >= 5 { fields["session_id"] = p.optionalText("session_id") }`.

In `MutationFactory.payloadVersion` (both overloads), add:

```swift
        if kind == .timer { return payload["session_id"] != nil ? 5 : 4 }
        if kind == .sleep { return payload["sleep_session_id"] != nil ? 2 : 1 }
```

In `FamilySyncHTTPClient.swift`, after the Feeding-Session header, add:

```swift
        request.setValue("v1", forHTTPHeaderField: "X-Baby-Tracker-Sleep-Session")
```

In `EndpointIdentity.swift`, add `public var advertisesSleepSession: Bool { capabilities.contains("sleep-session-v1") }`, following the neighbouring property.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `cd Packages/BabyTracker && swift test --filter 'ContentHashTests|ProtocolFixtureConformanceTests|FamilySyncEngineTests' && cd ../../Server && go test ./internal/protocol -count=1`
Expected: PASS on both sides.

- [ ] **Step 5: Update the count and commit**

Run `make test`. If it reports a count mismatch, set `EXPECTED_TESTS` to the reported total.

```bash
git add Packages/BabyTracker Server/api/fixtures Server/internal/protocol Makefile
git commit -m "feat(sync): client sleep v2 and timer v5 hashes and Sleep-Session header

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

### Task 6: SwiftData V8 and session fields on the wire

**Files:**
- Create: `Packages/BabyTracker/Sources/BabyStore/Models/SchemaV8.swift`
- Modify: `BabyStore/Models/BabyStoreMigrationPlan.swift`, `BabyStore/Models/Schema.swift:5,44,60`
- Modify: `BabyStore/Models/ActiveTimer.swift`, `BabyStore/Models/SleepEntry.swift`
- Modify: `BabyStore/MutationFactory.swift:101-115` (`timerPayload`), `BabyStore/EntryPayload.swift:98-111` (sleep)
- Modify: `BabyStore/TimerIntentReducer.swift`:
  - `TimerIntent.edit` gains `sessionID: UUID?`
  - the edit payload at 253-263 adds `"session_id"`
- Modify: `BabyStore/BabyStore+RemoteApply.swift:295-300,457-470` (sleep `sleep_session_id`)
- Modify: `BabyStore/BabyStore+RemoteTimers.swift:179-265` (`RemoteTimerState` session)
- Modify: `BabyStore/BabyStore+Timers.swift:254-300` (`editRunningTimer` passes `timer.sessionID`)
- Test: `Packages/BabyTracker/Tests/BabyStoreTests/SharedTimerTests.swift`, `WireShapeTests.swift`, `RemoteTimerApplyTests.swift`

**Interfaces:**
- `ActiveTimer.sessionID: UUID?`: synced as `session_id`.
- `ActiveTimer.bottleContentsRaw: String?`: local only. Use the computed `bottleContents: BottleContents?`.
- `SleepEntry.sleepSessionID: UUID?` and `SleepEntry.effectiveSleepSessionID: UUID { sleepSessionID ?? id }`.
- `TimerIntent.edit(timerID: UUID, babyID: UUID, startedAt: Date?, components: [FeedComponent]?, sessionID: UUID?)`. Every edit carries the current session ID, because v5 requires the key.

- [ ] **Step 1: Write the failing tests**

In `SharedTimerTests.swift`, update the exact-keys test `dispatchedTimerCommandsUseExactV3DeltaPayloads`:

```swift
    #expect(Set(startPayload.keys) == ["baby_id", "timer_kind", "started_at", "time_zone_id", "completed_components",
                                       "current_side", "current_side_started_at", "current_bottle_started_at", "session_id"])
    #expect(start.payloadVersion == 5)
```

Add to `WireShapeTests.swift`:

```swift
@MainActor @Test func aSleepAlwaysSendsItsSessionLinkAtVersionTwo() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let sleep = try store.logSleep(from: clock.now.addingTimeInterval(-3_600), to: clock.now, for: baby.id)
    let fields = sleep.payloadFields()
    #expect(fields.keys.contains("sleep_session_id"))
    #expect(MutationFactory.payloadVersion(for: .sleep, payload: fields) == 2)
}
```

Add to `RemoteTimerApplyTests.swift`, `aRemoteTimerSessionIsAppliedAndDetachable`:
- Apply a remote v5 timer start with `session_id = S`, then expect `activeTimer.sessionID == S`.
- Apply a remote edit with `session_id: null`, then expect `nil`.
- Build the payload the same way the other tests in that file do.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `cd Packages/BabyTracker && swift test --filter 'SharedTimerTests|WireShapeTests|RemoteTimerApplyTests'`
Expected: FAIL, because the properties don't exist.

- [ ] **Step 3: Implement**

1. `SchemaV8.swift`:
   - Copy `SchemaV7.swift` and rename it to `SchemaV8` with `versionIdentifier = Schema.Version(8, 0, 0)`. Its model list points at the current model classes, as V7 does.
   - In `BabyStoreMigrationPlan.swift`, append `SchemaV8.self` to `schemas`, and add `MigrationStage.lightweight(fromVersion: SchemaV7.self, toVersion: SchemaV8.self)`, mirroring the V6→V7 stage.
   - Point `Schema.swift` at V8 everywhere it names V7.

   If V7 snapshots the model classes as nested types instead of referencing them, follow the same nested pattern, adding the three new optional properties.

2. `ActiveTimer.swift`: add the stored properties and the computed property.

```swift
    /// The feed or sleep session this part continues; nil means it starts its own.
    public var sessionID: UUID?
    /// Local only: what the running bottle holds, chosen in the Bottle sheet.
    public var bottleContentsRaw: String?

    public var bottleContents: BottleContents? {
        get { bottleContentsRaw.flatMap(BottleContents.init(rawValue:)) }
        set { bottleContentsRaw = newValue?.rawValue }
    }
```

3. `SleepEntry.swift`: add the link and its fallback.

```swift
    public var sleepSessionID: UUID?
    public var effectiveSleepSessionID: UUID { sleepSessionID ?? id }
```

4. `MutationFactory.timerPayload`: add `"session_id": timer.sessionID?.uuidString.lowercased() as Any? ?? NSNull()`.

5. `EntryPayload.swift` sleep: add `"sleep_session_id": sleepSessionID?.uuidString.lowercased() as Any? ?? NSNull()`.

6. `TimerIntentReducer.swift`:
   - Extend the `.edit` case with `sessionID: UUID?`.
   - Its payload adds `"session_id": sessionID?.uuidString.lowercased() as Any? ?? NSNull()`. The version goes through `MutationFactory.payloadVersion`, which now returns 5.
   - Update every `.edit(` call site (`grep -rn "\.edit(" Packages/BabyTracker/Sources`) to pass `sessionID: timer.sessionID`.

7. `RemoteApply`, sleep update: `if let value = optionalString("sleep_session_id") { row.sleepSessionID = value.flatMap(UUID.init(uuidString:)) }`. Use the same optional-presence semantics as `feeding_session_id` at line ~449. Do the same in the sleep insert path.

8. `RemoteTimerState`:
   - Parse `session_id` (absent → no change, null → nil).
   - In `write(to:)`, diff `sessionID` like the other fields, and return `true` when it changed.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `make test`
Expected: PASS. Update `EXPECTED_TESTS` to the reported total.

- [ ] **Step 5: Commit**

```bash
git add Packages/BabyTracker Makefile
git commit -m "feat(store): schema V8 session links on timers and sleeps

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

### Task 7: Break limits (tracking preferences) and the Settings row

**Files:**
- Create: `Packages/BabyTracker/Sources/BabyStore/TrackingPreferences.swift`
- Modify: `Packages/BabyTracker/Sources/BabyStore/BabyStore.swift` (add `breakLimits` provider)
- Modify: `Packages/BabyTracker/Sources/SettingsFeature/SettingsViewModel.swift`, `SettingsView.swift` (new section after Units, ~line 69), `SettingsFeature/Localizable.xcstrings`
- Test: `Packages/BabyTracker/Tests/BabyStoreTests/TrackingPreferencesTests.swift` (new), `Packages/BabyTracker/Tests/SettingsFeatureTests/…` (add to the existing settings view-model tests file; `ls Packages/BabyTracker/Tests/SettingsFeatureTests`)

**Interfaces:**
- `public enum SessionKind: Sendable { case feed, sleep }`
- `public struct BreakLimits: Equatable, Sendable`, with:
  - `var feed: TimeInterval` and `var sleep: TimeInterval`
  - `static let standard = BreakLimits(feed: 1800, sleep: 900)`
  - `func seconds(for: SessionKind) -> TimeInterval`
- `public struct TrackingPreferences`, with:
  - `init(defaults: UserDefaults? = nil)`
  - `var feedMinutes: Int { get nonmutating set }` and `var sleepMinutes: Int { get nonmutating set }`, both clamped to 5…120 and rounded to a multiple of 5
  - `var limits: BreakLimits`
  - `static let feedKey = "tracking.feedBreakMinutes"` and `static let sleepKey = "tracking.sleepBreakMinutes"`
- `BabyStore.breakLimits: () -> BreakLimits`. The default reads `TrackingPreferences().limits`; tests override it.
- `SettingsViewModel.feedBreakMinutes: Int` and `sleepBreakMinutes: Int`, which write through to the preferences.

- [ ] **Step 1: Write the failing tests**

```swift
import Testing
import Foundation
@testable import BabyStore

@Suite struct TrackingPreferencesTests {
    private func defaults() -> UserDefaults {
        let name = "tracking-\(UUID().uuidString)"
        let d = UserDefaults(suiteName: name)!
        d.removePersistentDomain(forName: name)
        return d
    }

    @Test func defaultsAreThirtyAndFifteenMinutes() {
        let prefs = TrackingPreferences(defaults: defaults())
        #expect(prefs.limits == BreakLimits(feed: 1800, sleep: 900))
    }

    @Test func limitsAreClamped() {
        let prefs = TrackingPreferences(defaults: defaults())
        prefs.feedMinutes = 2
        prefs.sleepMinutes = 500
        #expect(prefs.feedMinutes == 5)
        #expect(prefs.sleepMinutes == 120)
        prefs.feedMinutes = 22
        #expect(prefs.feedMinutes == 20)
    }

    @MainActor @Test func changingTheLimitAppliesToTheNextDecision() throws {
        let d = defaults()
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        store.breakLimits = { TrackingPreferences(defaults: d).limits }
        TrackingPreferences(defaults: d).feedMinutes = 10
        #expect(store.breakLimits().feed == 600)
    }
}
```

Add one settings test: setting `model.feedBreakMinutes = 45` stores `45` under `TrackingPreferences.feedKey` in the injected defaults.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `cd Packages/BabyTracker && swift test --filter 'TrackingPreferencesTests|SettingsViewModel'`
Expected: FAIL, because the types don't exist.

- [ ] **Step 3: Implement**

```swift
import Foundation

public enum SessionKind: Sendable { case feed, sleep }

public struct BreakLimits: Equatable, Sendable {
    public var feed: TimeInterval
    public var sleep: TimeInterval
    public init(feed: TimeInterval, sleep: TimeInterval) { self.feed = feed; self.sleep = sleep }
    public static let standard = BreakLimits(feed: 1_800, sleep: 900)
    public func seconds(for kind: SessionKind) -> TimeInterval { kind == .feed ? feed : sleep }
}

/// How long a break may last before the next part starts a new feed or sleep.
/// Per device, in the App Group so widgets and intents decide the same way.
public struct TrackingPreferences {
    public static let feedKey = "tracking.feedBreakMinutes"
    public static let sleepKey = "tracking.sleepBreakMinutes"
    public static let range = 5...120
    private let defaults: UserDefaults

    public init(defaults: UserDefaults? = nil) {
        self.defaults = defaults ?? UserDefaults(suiteName: BabySchema.appGroupID) ?? .standard
    }

    public var feedMinutes: Int {
        get { read(Self.feedKey, fallback: 30) }
        nonmutating set { defaults.set(Self.clamp(newValue), forKey: Self.feedKey) }
    }

    public var sleepMinutes: Int {
        get { read(Self.sleepKey, fallback: 15) }
        nonmutating set { defaults.set(Self.clamp(newValue), forKey: Self.sleepKey) }
    }

    public var limits: BreakLimits {
        BreakLimits(feed: TimeInterval(feedMinutes * 60), sleep: TimeInterval(sleepMinutes * 60))
    }

    static func clamp(_ minutes: Int) -> Int {
        let bounded = min(max(minutes, range.lowerBound), range.upperBound)
        return bounded - bounded % 5
    }

    private func read(_ key: String, fallback: Int) -> Int {
        defaults.object(forKey: key) == nil ? fallback : Self.clamp(defaults.integer(forKey: key))
    }
}
```

In `BabyStore.swift`, add the stored property next to `dateProvider`:

```swift
    /// Read at every join decision; tests replace it.
    public var breakLimits: () -> BreakLimits = { TrackingPreferences().limits }
```

In `SettingsViewModel`, add `feedBreakMinutes` and `sleepBreakMinutes`, backed by `TrackingPreferences(defaults: defaults)`. Use the private-stored plus computed-write-through pattern from `volumeUnit`.

In `SettingsView`, add a `Section(SettingsCopy.trackingHeader)` with two `Stepper`s:

```swift
                Section(SettingsCopy.trackingHeader) {
                    Stepper(value: $settings.feedBreakMinutes, in: 5...120, step: 5) {
                        LabeledContent(SettingsCopy.joinFeedsWithin, value: SettingsCopy.minutes(settings.feedBreakMinutes))
                    }
                    .accessibilityIdentifier("settings.feedBreak")
                    Stepper(value: $settings.sleepBreakMinutes, in: 5...120, step: 5) {
                        LabeledContent(SettingsCopy.joinSleepWithin, value: SettingsCopy.minutes(settings.sleepBreakMinutes))
                    }
                    .accessibilityIdentifier("settings.sleepBreak")
                }
```

Strings: "Tracking", "Join feeds within", "Join sleep within" and "%lld min". Use the translations in the Task 13 table.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `make test && python3 scripts/verify-localizations.py`
Expected: PASS and `OK`. Update `EXPECTED_TESTS`.

- [ ] **Step 5: Commit**

```bash
git add Packages/BabyTracker Makefile
git commit -m "feat(settings): configurable feed and sleep break limits

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

### Task 8: Join engine and sleep sessions

**Files:**
- Create: `Packages/BabyTracker/Sources/BabyStore/SessionJoin.swift`
- Create: `Packages/BabyTracker/Sources/BabyStore/SleepSessions.swift`
- Test: `Packages/BabyTracker/Tests/BabyStoreTests/SessionJoinTests.swift` (new), `SleepSessionsTests.swift` (new)

**Interfaces:**
- `func continuedSessionID(_ kind: SessionKind, for babyID: UUID, startingAt start: Date) throws -> UUID?`
  - Internal to `BabyStore`.
  - Returns the session ID a part starting at `start` continues, or nil for a new session.
  - Feed: the most recent non-solids feed session with `endedAt <= start` and gap `<= limit`. It returns nil if a sleep (completed or running timer) falls in the gap.
  - Sleep: the most recent completed sleep session with `endedAt <= start` and gap `<= limit`.
- ```swift
  public struct SleepSession: Equatable, Sendable {
      public let id: UUID
      public let babyID: UUID
      public let sleepIDs: [UUID]
      public let startedAt: Date
      public let endedAt: Date?
      public let asleepSeconds: Int
  }
  ```
  `endedAt` is nil while any member is open. `asleepSeconds` is the sum over completed members.
- `func sleepSessions(for babyID: UUID, from start: Date, to end: Date) throws -> [SleepSession]`: sessions whose **first** member started in `[start, end)`.
- `func combineSleeps(_ sleepIDs: [UUID], for babyID: UUID) throws` and `func separateSleep(at sleepID: UUID, for babyID: UUID) throws`. They mirror `combineFeeds` and `separateFeed` in `FeedingSessions.swift:74-112`, using `SleepSessionError { missingSleep, alreadyOneSession, firstSleepCannotSeparate }`.

- [ ] **Step 1: Write the failing tests**

```swift
import Testing
import Foundation
import BabyCore
@testable import BabyStore

@MainActor
private func fixture() throws -> (BabyStore, UUID, TestDateProvider) {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    store.breakLimits = { .standard }
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    return (store, baby.id, clock)
}

@MainActor @Suite struct SessionJoinTests {
    @Test func aFeedStartingExactlyAtTheLimitJoinsAndOneSecondLaterDoesNot() throws {
        let (store, baby, clock) = try fixture()
        let first = try store.logBottle(volumeML: 60, contents: .formula, for: baby)
        let atLimit = clock.now.addingTimeInterval(1_800)
        #expect(try store.continuedSessionID(.feed, for: baby, startingAt: atLimit) == first.effectiveFeedingSessionID)
        #expect(try store.continuedSessionID(.feed, for: baby, startingAt: atLimit.addingTimeInterval(1)) == nil)
    }

    @Test func aDiaperOrMedInTheGapDoesNotBlockButASleepDoes() throws {
        let (store, baby, clock) = try fixture()
        let first = try store.logBottle(volumeML: 60, contents: .formula, for: baby)
        clock.advance(by: 300)
        _ = try store.logDiaper(.wet, for: baby)
        _ = try store.logSupplement(name: "Vitamin D", for: baby)
        clock.advance(by: 300)
        #expect(try store.continuedSessionID(.feed, for: baby, startingAt: clock.now) == first.effectiveFeedingSessionID)
        _ = try store.logSleep(from: clock.now.addingTimeInterval(-200), to: clock.now.addingTimeInterval(-100), for: baby)
        #expect(try store.continuedSessionID(.feed, for: baby, startingAt: clock.now) == nil)
    }

    @Test func aRunningSleepInTheGapBlocksAFeedJoin() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.logBottle(volumeML: 60, contents: .formula, for: baby)
        clock.advance(by: 120)
        _ = try store.startSleep(for: baby)
        clock.advance(by: 120)
        #expect(try store.continuedSessionID(.feed, for: baby, startingAt: clock.now) == nil)
    }

    @Test func solidsNeverJoin() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.logSolids(foods: ["pear"], amount: .some, for: baby)
        clock.advance(by: 60)
        #expect(try store.continuedSessionID(.feed, for: baby, startingAt: clock.now) == nil)
    }

    @Test func sleepJoinsWithinFifteenMinutes() throws {
        let (store, baby, clock) = try fixture()
        let nap = try store.logSleep(from: clock.now.addingTimeInterval(-3_600), to: clock.now, for: baby)
        #expect(try store.continuedSessionID(.sleep, for: baby, startingAt: clock.now.addingTimeInterval(900)) == nap.effectiveSleepSessionID)
        #expect(try store.continuedSessionID(.sleep, for: baby, startingAt: clock.now.addingTimeInterval(901)) == nil)
    }
}

@MainActor @Suite struct SleepSessionsTests {
    @Test func combineAndSeparateSleeps() throws {
        let (store, baby, clock) = try fixture()
        let a = try store.logSleep(from: clock.now.addingTimeInterval(-7_200), to: clock.now.addingTimeInterval(-3_600), for: baby)
        let b = try store.logSleep(from: clock.now.addingTimeInterval(-3_000), to: clock.now, for: baby)
        try store.combineSleeps([a.id, b.id], for: baby)
        var sessions = try store.sleepSessions(for: baby, from: .distantPast, to: .distantFuture)
        #expect(sessions.count == 1)
        #expect(sessions[0].sleepIDs == [a.id, b.id])
        #expect(sessions[0].asleepSeconds == 3_600 + 3_000)
        try store.separateSleep(at: b.id, for: baby)
        sessions = try store.sleepSessions(for: baby, from: .distantPast, to: .distantFuture)
        #expect(sessions.count == 2)
    }

    @Test func aJoinedSleepBelongsToTheDayOfItsFirstPart() throws {
        let (store, baby, clock) = try fixture()
        let a = try store.logSleep(from: clock.now.addingTimeInterval(-7_200), to: clock.now.addingTimeInterval(-3_600), for: baby)
        let b = try store.logSleep(from: clock.now.addingTimeInterval(-3_000), to: clock.now, for: baby)
        try store.combineSleeps([a.id, b.id], for: baby)
        let window = try store.sleepSessions(for: baby, from: clock.now.addingTimeInterval(-3_100), to: clock.now)
        #expect(window.isEmpty)   // the session started before the window
    }
}
```

Check two signatures against the source before using them:
- `logSolids(foods:amount:for:)`: verify `SolidsAmount` has a case usable here (`grep -n "enum SolidsAmount" -A6 Packages/BabyTracker/Sources/BabyCore/*.swift`) and replace `.some` with a real case.
- `logDiaper(.wet, for:)` and `logSupplement(name:for:)` should match the real signatures in `BabyStore+Diaper.swift:15` and `BabyStore+Supplement.swift:7`.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `cd Packages/BabyTracker && swift test --filter 'SessionJoinTests|SleepSessionsTests'`
Expected: FAIL, because the functions are undefined.

- [ ] **Step 3: Implement**

`SessionJoin.swift`:

```swift
import Foundation
import SwiftData
import BabyCore

extension BabyStore {
    /// Decided once, when a part starts, and stored on the timer or entry so
    /// every device that later completes it agrees.
    func continuedSessionID(_ kind: SessionKind, for babyID: UUID, startingAt start: Date) throws -> UUID? {
        let limit = breakLimits().seconds(for: kind)
        switch kind {
        case .feed:
            let feeds = try sessionFeeds(for: babyID).filter { $0.kind != .solids }
            let previous = Self.projectSessions(feeds)
                .filter { $0.endedAt <= start }
                .max { $0.endedAt < $1.endedAt }
            guard let previous, start.timeIntervalSince(previous.endedAt) <= limit else { return nil }
            guard try !sleepFalls(between: previous.endedAt, and: start, for: babyID) else { return nil }
            return previous.id
        case .sleep:
            let previous = try sleepSessions(for: babyID, from: .distantPast, to: .distantFuture)
                .compactMap { session in session.endedAt.map { (session, $0) } }
                .filter { $0.1 <= start }
                .max { $0.1 < $1.1 }
            guard let previous, start.timeIntervalSince(previous.1) <= limit else { return nil }
            return previous.0.id
        }
    }

    private func sleepFalls(between lower: Date, and upper: Date, for babyID: UUID) throws -> Bool {
        let realmKey = activeRealm.encodedKey
        let sleeps = try context.fetch(FetchDescriptor<SleepEntry>(predicate: #Predicate {
            $0.babyID == babyID && $0.realmKey == realmKey && !$0.isTombstoned
        }))
        if sleeps.contains(where: { $0.startedAt < upper && ($0.endedAt ?? now) > lower }) { return true }
        if let running = try activeTimer(kind: .sleep, for: babyID), running.startedAt < upper { return true }
        return false
    }
}
```

`SleepSessions.swift`:

```swift
import Foundation
import SwiftData
import BabyCore

public struct SleepSession: Equatable, Sendable {
    public let id: UUID
    public let babyID: UUID
    public let sleepIDs: [UUID]
    public let startedAt: Date
    public let endedAt: Date?
    public let asleepSeconds: Int
}

public enum SleepSessionError: Error, Equatable { case missingSleep, alreadyOneSession, firstSleepCannotSeparate }

public extension BabyStore {
    func sleepSessions(for babyID: UUID, from start: Date, to end: Date) throws -> [SleepSession] {
        Self.projectSleepSessions(try sessionSleeps(for: babyID))
            .filter { $0.startedAt >= start && $0.startedAt < end }
    }

    func combineSleeps(_ sleepIDs: [UUID], for babyID: UUID) throws {
        let expected = try captureWriteContext()
        try withRealmWrite(expected: expected) {
            let sleeps = try self.sessionSleeps(for: babyID)
            let selected = sleeps.filter { sleepIDs.contains($0.id) }
            guard selected.count == Set(sleepIDs).count, selected.count >= 2 else { throw SleepSessionError.missingSleep }
            let groups = Set(selected.map(\.effectiveSleepSessionID))
            guard groups.count > 1 else { throw SleepSessionError.alreadyOneSession }
            let affected = sleeps.filter { groups.contains($0.effectiveSleepSessionID) }
            let target = affected.min(by: Self.sleepOrder)!.effectiveSleepSessionID
            for sleep in affected where sleep.effectiveSleepSessionID != target {
                sleep.sleepSessionID = target
                sleep.lastModifiedAt = self.now
                try self.recordLocked(.update, on: sleep)
            }
        }
    }

    func separateSleep(at sleepID: UUID, for babyID: UUID) throws {
        let expected = try captureWriteContext()
        try withRealmWrite(expected: expected) {
            let sleeps = try self.sessionSleeps(for: babyID)
            guard let selected = sleeps.first(where: { $0.id == sleepID }) else { throw SleepSessionError.missingSleep }
            let group = sleeps.filter { $0.effectiveSleepSessionID == selected.effectiveSleepSessionID }.sorted(by: Self.sleepOrder)
            guard let index = group.firstIndex(where: { $0.id == sleepID }), index > 0 else {
                throw SleepSessionError.firstSleepCannotSeparate
            }
            for sleep in group[index...] {
                let link: UUID? = sleep.id == sleepID ? nil : sleepID
                if sleep.sleepSessionID != link {
                    sleep.sleepSessionID = link
                    sleep.lastModifiedAt = self.now
                    try self.recordLocked(.update, on: sleep)
                }
            }
        }
    }
}

extension BabyStore {
    func sessionSleeps(for babyID: UUID) throws -> [SleepEntry] {
        let realmKey = activeRealm.encodedKey
        return try context.fetch(FetchDescriptor<SleepEntry>(predicate: #Predicate {
            $0.babyID == babyID && $0.realmKey == realmKey && !$0.isTombstoned
        }))
    }

    static func sleepOrder(_ lhs: SleepEntry, _ rhs: SleepEntry) -> Bool {
        lhs.startedAt == rhs.startedAt ? lhs.id.uuidString < rhs.id.uuidString : lhs.startedAt < rhs.startedAt
    }

    static func projectSleepSessions(_ sleeps: [SleepEntry]) -> [SleepSession] {
        Dictionary(grouping: sleeps, by: \.effectiveSleepSessionID).compactMap { id, members in
            let ordered = members.sorted(by: sleepOrder)
            guard let first = ordered.first else { return nil }
            let open = ordered.contains { $0.endedAt == nil }
            return SleepSession(
                id: id, babyID: first.babyID, sleepIDs: ordered.map(\.id), startedAt: first.startedAt,
                endedAt: open ? nil : ordered.compactMap(\.endedAt).max(),
                asleepSeconds: ordered.compactMap(\.durationSeconds).reduce(0, +)
            )
        }.sorted { $0.startedAt == $1.startedAt ? $0.id.uuidString < $1.id.uuidString : $0.startedAt < $1.startedAt }
    }
}
```

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `make test`
Expected: PASS. Update `EXPECTED_TESTS`.

- [ ] **Step 5: Commit**

```bash
git add Packages/BabyTracker Makefile
git commit -m "feat(store): join decision and sleep sessions

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

### Task 9: One-part timers — start, stop, detach, manual logs

**Files:**
- Create: `Packages/BabyTracker/Sources/BabyStore/BabyStore+Parts.swift`
- Modify:
  - `BabyStore/BabyStore+Feeding.swift`:
    - `stopFeed` sets `entry.feedingSessionID = timer.sessionID`
    - `startSleep` joins
    - `stopSleep` sets `entry.sleepSessionID`
    - `logBottle` joins
  - `BabyStore/BabyStore+Sleep.swift` (`logSleep` joins when `end != nil`)
  - `BabyStore/BabyStore+Diaper.swift` (`logDiaper` gains `note: String? = nil`)
  - `BabyStore/BabyStore+Timers.swift` (`editRunningTimer` refuses a start before the joined session's last part end)
- Test: `Packages/BabyTracker/Tests/BabyStoreTests/OnePartTimerTests.swift` (new)

**Interfaces:**
- `public enum FeedPart: Equatable, Sendable { case breast(BreastSide), bottle(BottleContents) }`
- `@discardableResult func startPart(_ part: FeedPart, for babyID: UUID) throws -> ActiveTimer`
  - Starting the part that is already running returns that timer (a double-tap).
  - Starting a different part while one runs stops the running part first:
    - a breast part through `stopFeed`;
    - a running bottle through `stopBottle(timer, volumeML: LogDefaults(store:).mostUsedBottleVolumeML(for:))`.
  - The new timer's `sessionID = continuedSessionID(.feed, startingAt: now)`.
- `@discardableResult func stopBottle(_ timer: ActiveTimer, volumeML: Double) throws -> FeedEntry?`: `finishBottle(volumeML:contents:on:)`, then `stopFeed`. Contents come from `timer.bottleContents ?? LogDefaults.fallbackContents`.
- `func detachFromSession(_ timer: ActiveTimer) throws`: sets `sessionID = nil` and sends `.edit(..., sessionID: nil)`.
- `@discardableResult func logBreast(side: BreastSide, from start: Date, to end: Date, for babyID: UUID) throws -> FeedEntry`: one timed breast component, joined. Durations under 30 s throw `PartError.tooShort`.
- `func setBottleVolume(_ volumeML: Double, onFeed feedID: UUID) throws`: rewrites the single bottle component's volume and `entry.volumeML`, through `update(_:)`.
- `public enum PartError: Error, Equatable { case tooShort, startBeforePreviousPart, missingFeed }`

- [ ] **Step 1: Write the failing tests**

```swift
import Testing
import Foundation
import BabyCore
@testable import BabyStore

@MainActor
private func fixture() throws -> (BabyStore, UUID, TestDateProvider) {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    store.breakLimits = { .standard }
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    return (store, baby.id, clock)
}

@MainActor @Suite struct OnePartTimerTests {
    @Test func leftStopThenRightWithinTheLimitIsOneFeed() throws {
        let (store, baby, clock) = try fixture()
        let left = try store.startPart(.breast(.left), for: baby)
        clock.advance(by: 540)
        let leftEntry = try #require(try store.stopFeed(left))
        clock.advance(by: 600)
        let right = try store.startPart(.breast(.right), for: baby)
        #expect(right.sessionID == leftEntry.effectiveFeedingSessionID)
        clock.advance(by: 260)
        let rightEntry = try #require(try store.stopFeed(right))
        #expect(rightEntry.feedingSessionID == leftEntry.id)
        #expect(rightEntry.components.count == 1)
        let sessions = try store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture)
        #expect(sessions.count == 1)
        #expect(sessions[0].activeSeconds == 540 + 260)
    }

    @Test func pastTheLimitIsTwoFeeds() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 300))
        clock.advance(by: 1_801)
        let right = try store.startPart(.breast(.right), for: baby)
        #expect(right.sessionID == nil)
    }

    @Test func tappingTheOtherSideWhileOneRunsStopsItAndJoins() throws {
        let (store, baby, clock) = try fixture()
        let left = try store.startPart(.breast(.left), for: baby)
        clock.advance(by: 120)
        let right = try store.startPart(.breast(.right), for: baby)
        #expect(right.id != left.id)
        #expect(try store.activeTimers(for: baby).count == 1)
        let lastFeed = try #require(try store.lastFeed(for: baby))
        #expect(lastFeed.components.first?.breastSide == .left)
        #expect(right.sessionID == lastFeed.effectiveFeedingSessionID)
    }

    @Test func tappingTheRunningSideAgainIsIgnored() throws {
        let (store, baby, clock) = try fixture()
        let left = try store.startPart(.breast(.left), for: baby)
        clock.advance(by: 5)
        #expect(try store.startPart(.breast(.left), for: baby).id == left.id)
    }

    @Test func aDetachedPartStartsItsOwnFeed() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 300))
        let right = try store.startPart(.breast(.right), for: baby)
        try store.detachFromSession(right)
        clock.advance(by: 300)
        let entry = try #require(try store.stopFeed(right))
        #expect(entry.feedingSessionID == nil)
        #expect(try store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture).count == 2)
    }

    @Test func aTimedBottleStopsWithItsAmountAndJoins() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 300))
        let bottle = try store.startPart(.bottle(.breastMilk), for: baby)
        clock.advance(by: 400)
        let entry = try #require(try store.stopBottle(bottle, volumeML: 90))
        #expect(entry.totalBottleVolumeML == 90)
        #expect(entry.bottleComponents.first?.bottleContents == .breastMilk)
        #expect(entry.feedingSessionID != nil)
    }

    @Test func aChipLoggedBottleAndAPastBreastJoin() throws {
        let (store, baby, clock) = try fixture()
        let first = try #require(try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 300)))
        clock.advance(by: 120)
        let bottle = try store.logBottle(volumeML: 60, contents: .formula, for: baby)
        #expect(bottle.feedingSessionID == first.id)
        let past = try store.logBreast(side: .right, from: clock.now.addingTimeInterval(30), to: clock.now.addingTimeInterval(400), for: baby)
        #expect(past.feedingSessionID == first.id)
    }

    @Test func sleepWakeSleepWithinFifteenMinutesIsOneSleep() throws {
        let (store, baby, clock) = try fixture()
        let first = try store.stopSleep(try store.startSleep(for: baby).advancing(clock, by: 2_400))
        clock.advance(by: 360)
        let second = try store.startSleep(for: baby)
        #expect(second.sessionID == first.id)
        clock.advance(by: 1_900)
        let entry = try store.stopSleep(second)
        #expect(entry.sleepSessionID == first.id)
        #expect(try store.sleepSessions(for: baby, from: .distantPast, to: .distantFuture).count == 1)
    }

    @Test func aStartCannotBeMovedBeforeThePreviousPartEnded() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 300))
        clock.advance(by: 60)
        let right = try store.startPart(.breast(.right), for: baby)
        #expect(throws: PartError.startBeforePreviousPart) {
            try store.editRunningTimer(id: right.id, startedAt: clock.now.addingTimeInterval(-120), components: [])
        }
    }

    @Test func aLegacyMultiPartTimerStillStopsIntoOneFeed() throws {
        let (store, baby, clock) = try fixture()
        let legacy = try store.startFeed(side: .left, for: baby)
        clock.advance(by: 300)
        try store.beginBreast(side: .right, on: legacy)
        clock.advance(by: 300)
        let entry = try #require(try store.stopFeed(legacy))
        #expect(entry.components.count == 2)
        #expect(entry.feedingSessionID == nil)
    }

    @Test func aShortAccidentalTapLeavesNoPartButKeepsTheSession() throws {
        let (store, baby, clock) = try fixture()
        let first = try #require(try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 300)))
        let tap = try store.startPart(.breast(.right), for: baby)
        clock.advance(by: 10)
        #expect(try store.stopFeed(tap) == nil)
        clock.advance(by: 30)
        #expect(try store.startPart(.breast(.right), for: baby).sessionID == first.id)
    }

    @Test func aDiaperNoteIsSavedInTheSameWrite() throws {
        let (store, baby, _) = try fixture()
        let diaper = try store.logDiaper(.mixed, for: baby, note: "after bath")
        #expect(diaper.note == "after bath")
    }
}

@MainActor
private extension ActiveTimer {
    func advancing(_ clock: TestDateProvider, by seconds: TimeInterval) -> ActiveTimer {
        clock.advance(by: seconds)
        return self
    }
}
```

`aLegacyMultiPartTimerStillStopsIntoOneFeed` relies on `startFeed` and `beginBreast` still existing. Task 18 removes them, and at that point it moves this test to a remote-timer fixture.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `cd Packages/BabyTracker && swift test --filter OnePartTimerTests`
Expected: FAIL, because `startPart` and the other new functions are undefined.

- [ ] **Step 3: Implement**

`BabyStore+Parts.swift`:

```swift
import Foundation
import SwiftData
import BabyCore

public enum FeedPart: Equatable, Sendable { case breast(BreastSide), bottle(BottleContents) }
public enum PartError: Error, Equatable { case tooShort, startBeforePreviousPart, missingFeed }

public extension BabyStore {
    /// One breast side or one bottle. Starting another part while one runs
    /// stops it first; the new part joins whatever session the rules say.
    @discardableResult
    func startPart(_ part: FeedPart, for babyID: UUID) throws -> ActiveTimer {
        if let running = try activeTimer(kind: .feed, for: babyID) {
            switch part {
            case .breast(let side) where running.currentSide == side && running.currentBottleStartedAt == nil:
                return running
            case .bottle where running.currentBottleStartedAt != nil:
                return running
            default:
                if running.currentBottleStartedAt != nil {
                    _ = try stopBottle(running, volumeML: try LogDefaults(store: self).mostUsedBottleVolumeML(for: babyID))
                } else {
                    _ = try stopFeed(running)
                }
            }
        }
        let expected = try captureWriteContext()
        return try withRealmWrite(expected: expected) {
            let startedAt = self.now
            let timer = ActiveTimer(babyID: babyID, kind: .feed, startedAt: startedAt,
                                    timeZoneID: TimeZone.current.identifier, realm: self.activeRealm)
            timer.sessionID = try self.continuedSessionID(.feed, for: babyID, startingAt: startedAt)
            switch part {
            case .breast(let side):
                timer.currentSide = side
                timer.currentSideStartedAt = startedAt
            case .bottle(let contents):
                timer.currentBottleStartedAt = startedAt
                timer.bottleContents = contents
            }
            self.context.insert(timer)
            let side: BreastSide? = if case .breast(let s) = part { s } else { nil }
            try self.reduceTimerIntentLocked(.start(timerID: timer.id, babyID: babyID, kind: .feed, side: side))
            return timer
        }
    }

    @discardableResult
    func stopBottle(_ timer: ActiveTimer, volumeML: Double) throws -> FeedEntry? {
        try finishBottle(volumeML: volumeML, contents: timer.bottleContents ?? LogDefaults.fallbackContents, on: timer)
        return try stopFeed(timer)
    }

    func detachFromSession(_ timer: ActiveTimer) throws {
        let expected = try captureWriteContext()
        try withRealmWrite(expected: expected) {
            guard timer.sessionID != nil else { return }
            timer.sessionID = nil
            try self.reduceTimerIntentLocked(.edit(timerID: timer.id, babyID: timer.babyID,
                                                   startedAt: nil, components: nil, sessionID: nil))
        }
    }

    @discardableResult
    func logBreast(side: BreastSide, from start: Date, to end: Date, for babyID: UUID) throws -> FeedEntry {
        let seconds = Int(end.timeIntervalSince(start))
        guard seconds >= 30 else { throw PartError.tooShort }
        let expected = try captureWriteContext()
        return try withRealmWrite(expected: expected) {
            let entry = FeedEntry(babyID: babyID, kind: .breast, startedAt: start,
                                  timeZoneID: TimeZone.current.identifier, createdBy: self.deviceID)
            entry.realmKey = self.activeRealm.encodedKey
            entry.components = [.breast(id: UUID(), side: side, seconds: seconds, startedAt: start, endedAt: end)]
            entry.sides = [SideInterval(side: side, seconds: seconds)]
            entry.endedAt = end
            entry.feedingSessionID = try self.continuedSessionID(.feed, for: babyID, startingAt: start)
            self.context.insert(entry)
            try self.recordLocked(.create, on: entry)
            return entry
        }
    }

    func setBottleVolume(_ volumeML: Double, onFeed feedID: UUID) throws {
        let realmKey = activeRealm.encodedKey
        guard let feed = try context.fetch(FetchDescriptor<FeedEntry>(predicate: #Predicate {
            $0.id == feedID && $0.realmKey == realmKey && !$0.isTombstoned
        })).first else { throw PartError.missingFeed }
        feed.components = feed.components.map { component in
            guard case let .bottle(id, _, contents, seconds, startedAt, endedAt) = component else { return component }
            return .bottle(id: id, volumeML: volumeML, contents: contents, seconds: seconds, startedAt: startedAt, endedAt: endedAt)
        }
        feed.volumeML = volumeML
        try update(feed)
    }
}
```

Make these edits in `BabyStore+Feeding.swift`:
- **`stopFeed`:** just before `self.context.insert(entry)`, add `entry.feedingSessionID = timer.sessionID`. On the empty-components path no entry is saved, so nothing is needed there.
- **`startSleep`:** after `let timer = ActiveTimer(...)`, add `timer.sessionID = try self.continuedSessionID(.sleep, for: babyID, startingAt: timer.startedAt)`.
- **`stopSleep`:** before the insert, add `entry.sleepSessionID = timer.sessionID`.
- **`logBottle`:** before the insert, add `entry.feedingSessionID = try continuedSessionID(.feed, for: babyID, startingAt: stamp)`. Use the local variable that holds the logged time.

In `logSleep`, when `end != nil`, add `entry.sleepSessionID = try self.continuedSessionID(.sleep, for: babyID, startingAt: start)`.

In `logDiaper`, add the `note: String? = nil` parameter (after `color:`) and `entry.note = note`.

In `editRunningTimer`, inside the write and before `RunningTimerEdit`, refuse a start that would overlap the joined session:

```swift
            if let newStart = startedAt, let session = timer.sessionID {
                let previousEnd: Date? = timer.kind == .feed
                    ? Self.projectSessions(try self.sessionFeeds(for: timer.babyID)).first { $0.id == session }?.endedAt
                    : Self.projectSleepSessions(try self.sessionSleeps(for: timer.babyID)).first { $0.id == session }?.endedAt
                if let previousEnd, newStart < previousEnd { throw PartError.startBeforePreviousPart }
            }
```

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `make test`
Expected: PASS, including the existing `TimerTests`, `TimedBottleTests`, `FeedingSessionsTests` and `SharedTimerTests`. Update `EXPECTED_TESTS`.

- [ ] **Step 5: Commit**

```bash
git add Packages/BabyTracker Makefile
git commit -m "feat(store): one-part timers join sessions at start

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

### Task 10: Home data — breaks, joined running parts, session-day totals

**Files:**
- Modify: `Packages/BabyTracker/Sources/BabyStore/HomeSnapshot.swift`, `BabyStore+Queries.swift:53-87`
- Test: `Packages/BabyTracker/Tests/BabyStoreTests/HomeSnapshotSessionTests.swift` (new)

**Interfaces:**
- New `HomeSnapshot` fields:
  - `feedBreak: FeedingSession?`: the last feed session, when no feed timer runs and `now - endedAt <= limit`.
  - `sleepBreakEndedAt: Date?`: the last sleep session's end, when no sleep timer runs and `now - end <= sleep limit`.
  - `runningFeedSession: FeedingSession?`: the earlier parts the running feed timer joins (`timer.sessionID`).
  - `runningSleepSession: SleepSession?`: the same, for sleep.
  - `awakeSince: Date?`: the end of the last sleep part.
  - `breakLimits: BreakLimits`.
  - `todaySleepSpans: [DateInterval]`: today's sleep parts, clipped to today, with a running sleep drawn to `now`.
  - `todayFeedTimes: [Date]`: the start of each of today's feed parts.
- `todaySleepSeconds` becomes the sum of `asleepSeconds` over the sleep sessions whose first part started today.
- `todayFeedCount` stays a count of sessions.

- [ ] **Step 1: Write the failing tests**

```swift
@MainActor @Suite struct HomeSnapshotSessionTests {
    @Test func aStoppedPartOpensABreakUntilTheLimit() throws {
        let (store, baby, clock) = try fixture()            // same helper as OnePartTimerTests
        let left = try store.startPart(.breast(.left), for: baby)
        clock.advance(by: 300)
        _ = try store.stopFeed(left)
        clock.advance(by: 40)
        #expect(try store.homeSnapshot(for: baby).feedBreak?.activeSeconds == 300)
        clock.advance(by: 1_800)
        #expect(try store.homeSnapshot(for: baby).feedBreak == nil)
    }

    @Test func aJoinedRunningPartExposesTheEarlierParts() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 300))
        _ = try store.startPart(.breast(.right), for: baby)
        let snapshot = try store.homeSnapshot(for: baby)
        #expect(snapshot.feedBreak == nil)
        #expect(snapshot.runningFeedSession?.activeSeconds == 300)
    }

    private func local(_ day: Int, _ hour: Int, _ minute: Int) -> Date {
        Calendar.current.date(from: DateComponents(year: 2026, month: 10, day: day, hour: hour, minute: minute))!
    }

    @Test func aJoinedSleepAcrossMidnightCountsOnItsFirstDay() throws {
        let clock = TestDateProvider(local(1, 23, 59))
        let store = try BabyStore.inMemory(dateProvider: clock)
        store.breakLimits = { .standard }
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil).id
        let first = try store.logSleep(from: local(1, 23, 30), to: local(1, 23, 55), for: baby)
        let second = try store.logSleep(from: local(2, 0, 5), to: local(2, 1, 0), for: baby)
        #expect(second.sleepSessionID == first.id)
        #expect(try store.homeSnapshot(for: baby).todaySleepSeconds == 25 * 60 + 55 * 60)
        clock.advance(by: local(2, 8, 0).timeIntervalSince(clock.now))
        #expect(try store.homeSnapshot(for: baby).todaySleepSeconds == 0)
    }

    @Test func aFeedSessionAcrossMidnightCountsOnce() throws {
        let clock = TestDateProvider(local(1, 23, 59))
        let store = try BabyStore.inMemory(dateProvider: clock)
        store.breakLimits = { .standard }
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil).id
        let left = try store.logBreast(side: .left, from: local(1, 23, 50), to: local(1, 23, 58), for: baby)
        let right = try store.logBreast(side: .right, from: local(2, 0, 5), to: local(2, 0, 12), for: baby)
        #expect(right.feedingSessionID == left.id)
        #expect(try store.homeSnapshot(for: baby).todayFeedCount == 1)
        clock.advance(by: local(2, 8, 0).timeIntervalSince(clock.now))
        #expect(try store.homeSnapshot(for: baby).todayFeedCount == 0)
    }

    @Test func awakeIsMeasuredFromTheLastSleepPart() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopSleep(try store.startSleep(for: baby).advancing(clock, by: 1_200))
        clock.advance(by: 300)
        _ = try store.stopSleep(try store.startSleep(for: baby).advancing(clock, by: 600))
        #expect(try store.homeSnapshot(for: baby).awakeSince == clock.now)
    }
}
```

Move the `fixture()` and `advancing` helpers into `Tests/BabyStoreTests/Support/PartTestHelpers.swift` so Tasks 9 and 10 share them, and delete the private copies.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `cd Packages/BabyTracker && swift test --filter HomeSnapshotSessionTests`
Expected: FAIL, because the new fields don't exist.

- [ ] **Step 3: Implement**

In `homeSnapshot(for:)`:

```swift
        let limits = breakLimits()
        let runningTimers = try activeTimers(for: babyID)
        let feedTimer = runningTimers.first { $0.kind == .feed }
        let sleepTimer = runningTimers.first { $0.kind == .sleep }
        let milkSessions = Self.projectSessions(allFeeds.filter { $0.kind != .solids })
        let lastMilk = milkSessions.max { $0.endedAt < $1.endedAt }
        let feedBreak = feedTimer == nil ? lastMilk.flatMap { now.timeIntervalSince($0.endedAt) <= limits.feed ? $0 : nil } : nil
        let allSleeps = try sessionSleeps(for: babyID)
        let sleepSessions = Self.projectSleepSessions(allSleeps)
        let lastSleepEnd = allSleeps.compactMap(\.endedAt).max()
        let sleepBreakEndedAt = sleepTimer == nil ? lastSleepEnd.flatMap { now.timeIntervalSince($0) <= limits.sleep ? $0 : nil } : nil
        let todaySleepSessions = sleepSessions.filter { $0.startedAt >= start && $0.startedAt < end }
        let todaySleepSpans = allSleeps.filter { ($0.endedAt ?? now) > start && $0.startedAt < end }
            .map { DateInterval(start: max($0.startedAt, start), end: min($0.endedAt ?? now, now)) }
            + (sleepTimer.map { [DateInterval(start: max($0.startedAt, start), end: now)] } ?? [])
```

Pass these into the new `HomeSnapshot` fields:
- `runningFeedSession: feedTimer?.sessionID.flatMap { id in sessions.first { $0.id == id } }`
- `runningSleepSession`, the same way, from `sleepSessions`
- `awakeSince: lastSleepEnd`
- `todaySleepSeconds: todaySleepSessions.reduce(0) { $0 + $1.asleepSeconds }`
- `todayFeedTimes: allFeeds.filter { $0.kind != .solids && $0.startedAt >= start && $0.startedAt < end }.map(\.startedAt)`
- `todaySleepSpans`, `feedBreak`, `sleepBreakEndedAt` and `breakLimits: limits`, as computed above

Remove the now-unused day-filtered `sleeps` fetch.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `make test`
Expected: PASS. Existing `QueryTests` that assert `todaySleepSeconds` keep passing, because unjoined sleeps are their own sessions. Update `EXPECTED_TESTS`.

- [ ] **Step 5: Commit**

```bash
git add Packages/BabyTracker Makefile
git commit -m "feat(store): home snapshot exposes breaks, joined parts and session-day totals

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

### Task 11: History combine fix and sleep combine/separate

**Files:**
- Modify: `Packages/BabyTracker/Sources/HistoryFeature/HistoryViewModel.swift:214-247`, `HistoryView.swift:265-282` (sleep rows get the same menu), `HistoryFeature/Localizable.xcstrings`
- Test: `Packages/BabyTracker/Tests/HistoryFeatureTests/HistoryViewModelTests.swift`

**Interfaces:**
- `combineWithPrevious(feedID:)`: the previous **feed session** before this one, ignoring diapers, Meds and Growth. It no longer requires the current session to have exactly one member.
- New:
  - `combineWithPrevious(sleepID:)` and `separate(sleepID:)`
  - `canCombineWithPrevious(sleepID:) -> Bool` and `canSeparate(sleepID:) -> Bool`
- Strings: "Combine with previous sleep" and "Separate here". The second already exists; reuse it.

- [ ] **Step 1: Write the failing tests**

```swift
@MainActor @Test func combineWithPreviousIgnoresADiaperInBetween() throws {
    let (store, baby, clock) = try freshStore()     // existing helper in this file
    let first = try store.logBottle(volumeML: 60, contents: .formula, for: baby)
    clock.advance(by: 4_000)                           // beyond the join limit, so not auto-joined
    _ = try store.logDiaper(.wet, for: baby)
    clock.advance(by: 60)
    let second = try store.logBottle(volumeML: 60, contents: .formula, for: baby)
    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 1)
    #expect(model.canCombineWithPrevious(feedID: second.id))
    try model.combineWithPrevious(feedID: second.id)
    #expect(try store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture).map(\.feedIDs) == [[first.id, second.id]])
}

@MainActor @Test func sleepsCanBeCombinedAndSeparatedFromHistory() throws {
    let (store, baby, clock) = try freshStore()
    let a = try store.logSleep(from: clock.now, to: clock.now.addingTimeInterval(1_800), for: baby)
    clock.advance(by: 5_000)
    let b = try store.logSleep(from: clock.now, to: clock.now.addingTimeInterval(1_800), for: baby)
    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 1)
    try model.combineWithPrevious(sleepID: b.id)
    #expect(model.canSeparate(sleepID: b.id))
    try model.separate(sleepID: b.id)
    #expect(try store.sleepSessions(for: baby, from: .distantPast, to: .distantFuture).count == 2)
    _ = a
}
```

Use the constructor and `load` signature from the top of `HistoryViewModelTests.swift`.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `cd Packages/BabyTracker && swift test --filter HistoryViewModelTests`
Expected: FAIL. The diaper blocks the combine, and the sleep APIs are missing.

- [ ] **Step 3: Implement**

```swift
    private func previousFeedSessionMember(for feedID: UUID) throws -> UUID? {
        let sessions = try store.feedingSessions(for: babyID, from: .distantPast, to: .distantFuture)
        guard let current = sessions.first(where: { $0.feedIDs.contains(feedID) }) else { return nil }
        return sessions.filter { $0.id != current.id && $0.startedAt < current.startedAt }
            .max { $0.startedAt < $1.startedAt }?.feedIDs.last
    }

    public func combineWithPrevious(feedID: UUID) throws {
        guard let previousID = try previousFeedSessionMember(for: feedID) else { throw FeedingSessionError.missingFeed }
        try store.combineFeeds([previousID, feedID], for: babyID)
        try load(dayCount: max(days.count, 1))
    }

    public func canCombineWithPrevious(feedID: UUID) -> Bool { (try? previousFeedSessionMember(for: feedID)) != nil }
```

Write the sleep equivalents with `store.sleepSessions`, `combineSleeps` and `separateSleep`. Delete `adjacentPreviousFeedID`. In `HistoryView`, give sleep rows the same `ellipsis.circle` menu as feed rows, with identifiers `history.sleep.combine` and `history.sleep.separate`. A sleep session with more than one member shows as one row whose time span is the session's span; mirror the feed group row, but with no expansion yet. The History redesign comes later.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `make test && python3 scripts/verify-localizations.py`
Expected: PASS and `OK`. Update `EXPECTED_TESTS`.

- [ ] **Step 5: Commit**

```bash
git add Packages/BabyTracker Makefile
git commit -m "fix(history): combine across diapers and add sleep combine/separate

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

### Task 12: Insights, Ask and widget summary read sleep sessions

**Files:**
- Modify: `Packages/BabyTracker/Sources/InsightsFeature/InsightsViewModel.swift:150-226` (sleep seconds by session start day)
- Modify: `Packages/BabyTracker/Sources/AskFeature/AskFactBuilder.swift`, `AskContextBuilder.swift` (sleep counts by session; follow how feeds already use `feedingSessions`, `AskFactBuilder.swift:92`)
- Modify: `Packages/BabyTracker/Sources/BabyStore/WidgetStatusSummary.swift:46-70`:
  - the feed age is measured from the last milk session's `endedAt`
  - "Awake" is measured from `awakeSince`
- Test: `Packages/BabyTracker/Tests/InsightsFeatureTests/…`, `AskFeatureTests/…` (the existing fact-builder test file), `BabyStoreTests/WidgetStatusSummaryTests.swift`

**Interfaces:**
- Consumes `sleepSessions(for:from:to:)` and `HomeSnapshot.awakeSince` / `lastFeedingSession`.

- [ ] **Step 1: Write the failing tests**

```swift
@MainActor @Test func insightsCountsAJoinedSleepOnItsFirstDay() throws {
    // Two sleep parts joined across midnight (as in Task 10); InsightsViewModel for a 7-day
    // range: day D's sleepSeconds includes both parts, day D+1's is 0.
}

@MainActor @Test func askCountsAJoinedNapOnce() throws {
    // Two joined sleep parts within one day; the fact builder's nap count for that day == 1.
}

@MainActor @Test func widgetAwakeTimeUsesTheLastSleepPart() throws {
    let (store, baby, clock) = try fixture()
    _ = try store.stopSleep(try store.startSleep(for: baby).advancing(clock, by: 600))
    clock.advance(by: 120)
    let summary = WidgetStatusSummary(babyName: "Mira", snapshot: try store.homeSnapshot(for: baby), now: clock.now)
    #expect(summary.sleepText.contains("2m"))
}
```

Write out the two commented tests. Use the construction patterns already in `InsightsFeatureTests` and `AskFeatureTests`; `ls Packages/BabyTracker/Tests/InsightsFeatureTests Packages/BabyTracker/Tests/AskFeatureTests` lists them. Read the existing nap-count fact in `AskFactBuilder.swift` to get its exact name.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `cd Packages/BabyTracker && swift test --filter 'Insights|Ask|WidgetStatusSummary'`
Expected: FAIL on the new tests.

- [ ] **Step 3: Implement**

- **Insights:** bucket `sleepSeconds` by `store.sleepSessions(...)`, using each session's `startedAt` day and adding its `asleepSeconds`. Do not bucket per entry. The 24-hour rhythm strip still draws each part separately; leave it unchanged.
- **Ask:** wherever sleeps are counted, count `sleepSessions`. Durations still sum the parts.
- **Widget summary:**
  - `feedAge`: `age(snapshot.lastFeedingSession?.endedAt ?? snapshot.lastFeed?.endedAt ?? snapshot.lastFeed?.startedAt)`.
  - Awake: `snapshot.awakeSince`.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `make test`
Expected: PASS. Update `EXPECTED_TESTS`.

- [ ] **Step 5: Commit**

```bash
git add Packages/BabyTracker Makefile
git commit -m "feat(insights): count joined sleeps once across Insights, Ask and widgets

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

## Phase C — Client UI

### Task 13: Icon catalogue, palette, Meds and Mixed copy

**Files:**
- Create: `Packages/BabyTracker/Sources/BabyUI/Resources/Icons.xcassets/` with one `.imageset` per icon (SVG plus `Contents.json`)
- Create: `Packages/BabyTracker/Sources/BabyUI/BabyIcon.swift`
- Modify: `Packages/BabyTracker/Package.swift` (BabyUI resources)
- Modify: `Packages/BabyTracker/Sources/BabyUI/Palette.swift`:
  - `Tracker` gains `.solids` and `.growth`
  - add `var icon: BabyIcon`
  - `title` for `.supplement` becomes "Meds"
  - remove `symbolName`
- Modify: `BabyUI/TrackerIcon.swift` (delete `DiaperShape`, render `BabyIconView`)
- Modify: every `symbolName` / hard-coded symbol use. List them with `grep -rn 'symbolName\|"drop.fill"\|"waterbottle.fill"\|"shippingbox.fill"\|"pills.fill"\|"fork.knife"\|"ruler"\|"l.circle.fill"\|"r.circle.fill"\|"moon.zzz.fill"\|"moon.fill"' Packages/BabyTracker/Sources Apps`. Leave the strings in `BabyTrackerShortcuts.swift`, because App Shortcuts accept only system symbol names.
- Modify:
  - `BabyCore/Enums.swift` (DiaperContents `.mixed` display name is "Mixed"; the diaper fast path shows Wet, Dirty, Mixed)
  - `BabyCore/Localizable.xcstrings` and every catalog whose "Supplements" label is user-visible
- Test: `Packages/BabyTracker/Tests/BabyUITests/BabyIconTests.swift` (new; if there is no BabyUI test target, add `.testTarget(name: "BabyUITests", dependencies: ["BabyUI"])` in `Package.swift`)

**Interfaces:**
- `public enum BabyIcon: String, CaseIterable, Sendable { case feed, breastLeft, breastRight, bottle, sleep, diaper, solids, meds, growth, wet, dirty, mixed, stop, more, clock, plus, close }`
- `public struct BabyIconView: View`, with `init(_ icon: BabyIcon, size: CGFloat = 22)`. It renders `Image(icon.assetName, bundle: .module).renderingMode(.template)`. For `breastLeft` and `breastRight` it overlays a localized letter (`"L"` / `"R"` keys, `.system(size: size * 0.42, weight: .heavy, design: .rounded)`, offset downward by `size * 0.12`).
- `extension BabyIcon { var assetName: String }`. The asset name is `"icon-" + rawValue`, and `breastLeft`/`breastRight` both use the `icon-feed` drop.
- Palette additions:
  - `Palette.meds = Color(red: 0.77, green: 0.65, blue: 0.96)` (#C4A6F5)
  - `Palette.solids = Color(red: 0.91, green: 0.60, blue: 0.48)` (#E89A7B)
  - `Palette.growth = Color(red: 0.62, green: 0.82, blue: 0.48)` (#9FD27A)
  - `Palette.wet = Color(red: 0.50, green: 0.85, blue: 1.0)` (#7FD8FF)
  - `Palette.dirty = Color(red: 0.79, green: 0.64, blue: 0.48)` (#C9A27A)

- [ ] **Step 1: Write the failing test**

```swift
import Testing
import SwiftUI
@testable import BabyUI

@Suite struct BabyIconTests {
    @Test func everyIconHasAnAssetInTheBundle() {
        for icon in BabyIcon.allCases {
            #expect(Bundle.module.url(forResource: "Assets", withExtension: "car") != nil
                    || Bundle.module.path(forResource: icon.assetName, ofType: "svg") != nil,
                    "missing asset for \(icon)")
        }
    }

    @Test func trackersMapToTheNewIcons() {
        #expect(Tracker.diaper.icon == .diaper)
        #expect(Tracker.supplement.icon == .meds)
        #expect(Tracker.supplement.title == "Meds")
    }
}
```

The first test is a smoke check that the compiled catalog is present. The real visual check is the screenshot review in Task 19.

- [ ] **Step 2: Run the test and confirm it fails**

Run: `cd Packages/BabyTracker && swift test --filter BabyIconTests`
Expected: FAIL, because `BabyIcon` is undefined.

- [ ] **Step 3: Create the SVG assets**

Each imageset's `Contents.json` is the same:

```json
{ "images": [{ "filename": "icon.svg", "idiom": "universal" }],
  "info": { "author": "xcode", "version": 1 },
  "properties": { "preserves-vector-representation": true, "template-rendering-intent": "template" } }
```

Every `icon.svg` uses this frame, with the shape elements below in place of `{BODY}`:

```xml
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="24" height="24" fill="none" stroke="#000" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">{BODY}</svg>
```

| Imageset | `{BODY}` |
| --- | --- |
| `icon-feed` | `<path d="M12 2.8c3.9 4.6 6.6 8.2 6.6 11.6a6.6 6.6 0 0 1-13.2 0c0-3.4 2.7-7 6.6-11.6z"/>` |
| `icon-bottle` | `<path d="M10.4 2.6h3.2M9.6 6.2 10.4 2.6h3.2l.8 3.6z"/><rect x="7.2" y="6.2" width="9.6" height="15" rx="3"/><path d="M7.2 11h3.4M7.2 14.4h3.4M7.2 17.8h3.4"/>` |
| `icon-sleep` | `<path d="M19.8 14.8A8.2 8.2 0 0 1 9.2 4.2a8.2 8.2 0 1 0 10.6 10.6z"/><path d="M16 3.5v3M14.5 5h3"/>` |
| `icon-diaper` | `<path d="M2.8 5h18.4v3.6c-3 .6-5 3.4-5.4 10.4H8.2C7.8 12 5.8 9.2 2.8 8.6z"/><path d="M2.8 6.8H1.2M21.2 6.8h1.6"/><rect x="9.6" y="7.6" width="4.8" height="3.2" rx="1"/>` |
| `icon-solids` | `<path d="M3.5 12h17a8.5 7.5 0 0 1-17 0z"/><path d="M9 19.5h6M14.5 9.5 19 3.5"/>` |
| `icon-meds` | `<rect x="2.6" y="8" width="13.5" height="6.4" rx="3.2" transform="rotate(-40 9.35 11.2)"/><path d="m7.2 9.1 4.4 4"/><path d="M18 13.2c1.8 2.1 3 3.7 3 5.1a3 3 0 0 1-6 0c0-1.4 1.2-3 3-5.1z"/>` |
| `icon-growth` | `<path d="M4 20.5h16l-1.6-9H5.6z"/><path d="M3 8.5c3-2.8 15-2.8 18 0M12 11.5V7"/>` |
| `icon-wet` | `<path fill="#000" stroke="none" d="M12 4c2.6 3.1 4.4 5.5 4.4 7.8a4.4 4.4 0 0 1-8.8 0C7.6 9.5 9.4 7.1 12 4z"/>` |
| `icon-dirty` | `<circle fill="#000" stroke="none" cx="12" cy="12" r="5"/>` |
| `icon-mixed` | `<path fill="#000" stroke="none" d="M8 6c1.8 2.1 3 3.8 3 5.3a3 3 0 0 1-6 0C5 9.8 6.2 8.1 8 6z"/><circle fill="#000" stroke="none" cx="16.5" cy="13" r="3.5"/>` |
| `icon-stop` | `<rect x="6.5" y="6.5" width="11" height="11" rx="2.5"/>` |
| `icon-more` | `<circle fill="#000" stroke="none" cx="6" cy="12" r="1.4"/><circle fill="#000" stroke="none" cx="12" cy="12" r="1.4"/><circle fill="#000" stroke="none" cx="18" cy="12" r="1.4"/>` |
| `icon-clock` | `<circle cx="12" cy="12" r="8.5"/><path d="M12 7.5V12l3 2"/>` |
| `icon-plus` | `<path d="M12 5v14M5 12h14"/>` |
| `icon-close` | `<path d="M7 7l10 10M17 7 7 17"/>` |

`Mixed` renders as a single template image. The view applies two tints by overlaying `icon-wet` and `icon-dirty` at their own colours; use `BabyIconView.diaperContents(_:)`. The single `icon-mixed` asset is for monochrome contexts such as widget accents.

In `Package.swift`, change the BabyUI target to `resources: [.process("Localizable.xcstrings"), .process("Resources/Icons.xcassets")]`.

- [ ] **Step 4: Implement `BabyIcon`, `BabyIconView` and the palette, and replace usages**

```swift
import SwiftUI

public enum BabyIcon: String, CaseIterable, Sendable {
    case feed, breastLeft, breastRight, bottle, sleep, diaper, solids, meds, growth, wet, dirty, mixed, stop, more, clock, plus, close

    public var assetName: String {
        switch self {
        case .breastLeft, .breastRight: "icon-feed"
        default: "icon-" + rawValue
        }
    }

    var letterKey: String? {
        switch self {
        case .breastLeft: "L"
        case .breastRight: "R"
        default: nil
        }
    }
}

public struct BabyIconView: View {
    private let icon: BabyIcon
    private let size: CGFloat

    public init(_ icon: BabyIcon, size: CGFloat = 22) {
        self.icon = icon
        self.size = size
    }

    public var body: some View {
        Image(icon.assetName, bundle: .module)
            .renderingMode(.template)
            .resizable()
            .scaledToFit()
            .frame(width: size, height: size)
            .overlay {
                if let key = icon.letterKey {
                    Text(String(localized: String.LocalizationValue(key), bundle: .module))
                        .font(.system(size: size * 0.42, weight: .heavy, design: .rounded))
                        .offset(y: size * 0.12)
                }
            }
            .accessibilityHidden(true)
    }

    /// Wet is a blue drop, dirty a brown dot, mixed shows both side by side.
    @ViewBuilder
    public static func diaperContents(_ contents: DiaperContentsMark, size: CGFloat = 20) -> some View {
        HStack(spacing: 0) {
            if contents != .dirty {
                BabyIconView(.wet, size: size).foregroundStyle(Palette.wet)
            }
            if contents != .wet {
                BabyIconView(.dirty, size: size).foregroundStyle(Palette.dirty)
            }
        }
    }
}

public enum DiaperContentsMark: Sendable { case wet, dirty, mixed }
```

Rewrite `TrackerIcon` to render `BabyIconView(tracker.icon, size:)` tinted `tracker.color` in its existing tile, and delete `DiaperShape`.

Then go through the grep list in **Files**. Each hit becomes a `BabyIconView` (in views) or a `BabyIcon` value (in models):
- `FeedActivityKind.symbolName` becomes `var icon: BabyIcon`.
- `BreastSide` side icons become `.breastLeft` / `.breastRight`.
- `DiaperContents` icons become `DiaperContentsMark`.

Copy changes:
- `Tracker.supplement.title` is "Meds".
- Every visible "Supplements" or "Log supplements" label becomes "Meds" or "Log meds".
- The diaper "Both" label becomes "Mixed".

Use these translations:

| Key (en) | de | fr | es | it | uk |
| --- | --- | --- | --- | --- | --- |
| Meds | Medizin | Médicaments | Medicinas | Farmaci | Ліки |
| Log meds | Medizin eintragen | Noter un médicament | Registrar medicina | Registra farmaco | Записати ліки |
| Mixed | Gemischt | Mixte | Mixto | Misto | Змішаний |
| L | L | G | I | S | Л |
| R | R | D | D | D | П |
| Stop | Stopp | Arrêter | Detener | Ferma | Стоп |
| Wake up | Aufwachen | Réveil | Despertar | Risveglio | Пробудження |
| Start bottle | Fläschchen starten | Démarrer le biberon | Iniciar biberón | Avvia biberon | Почати пляшечку |
| Past entry | Nachtrag | Entrée passée | Registro anterior | Voce passata | Минулий запис |
| Details | Details | Détails | Detalles | Dettagli | Деталі |
| Note | Notiz | Note | Nota | Nota | Нотатка |
| Done | Fertig | Terminé | Listo | Fine | Готово |
| Same feed | Gleiche Mahlzeit | Même repas | Misma toma | Stessa poppata | Те саме годування |
| Same sleep | Gleicher Schlaf | Même sommeil | Mismo sueño | Stesso sonno | Той самий сон |
| Break | Pause | Pause | Pausa | Pausa | Перерва |
| since %@ | seit %@ | depuis %@ | desde %@ | da %@ | з %@ |
| Set amount | Menge eintragen | Indiquer la quantité | Indicar cantidad | Inserisci quantità | Вказати кількість |
| Tracking | Erfassung | Suivi | Seguimiento | Monitoraggio | Відстеження |
| Join feeds within | Mahlzeiten verbinden innerhalb | Regrouper les repas sous | Unir tomas en | Unisci poppate entro | Об'єднувати годування в межах |
| Join sleep within | Schlaf verbinden innerhalb | Regrouper le sommeil sous | Unir sueño en | Unisci sonno entro | Об'єднувати сон у межах |
| %lld min | %lld Min. | %lld min | %lld min | %lld min | %lld хв |
| Start left breast | Linke Brust starten | Commencer sein gauche | Iniciar pecho izquierdo | Avvia seno sinistro | Почати ліві груди |
| Start right breast | Rechte Brust starten | Commencer sein droit | Iniciar pecho derecho | Avvia seno destro | Почати праві груди |
| Make it a separate feed | Als eigene Mahlzeit | En faire un repas séparé | Hacerla una toma aparte | Rendila una poppata separata | Зробити окремим годуванням |
| Make it a separate sleep | Als eigenen Schlaf | En faire un sommeil séparé | Hacerlo un sueño aparte | Rendilo un sonno separato | Зробити окремим сном |
| Add med | Medizin hinzufügen | Ajouter un médicament | Añadir medicina | Aggiungi farmaco | Додати ліки |
| More | Mehr | Plus | Más | Altro | Більше |
| Today | Heute | Aujourd'hui | Hoy | Oggi | Сьогодні |

Keys that already exist in another catalog, such as "Awake", "Left breast", "Right breast", "Bottle" and "Sleep", reuse their existing translations. Check with `grep -l '"Awake"' $(git ls-files '*.xcstrings')`.

- [ ] **Step 5: Run the checks**

Run: `make test && python3 scripts/verify-localizations.py && make build SIMULATOR='platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853'`
Expected: PASS, `OK`, and `BUILD SUCCEEDED`. If `swift test` cannot compile the asset catalog on the command line (an `actool` error), keep the catalog. Change the first test to `#expect(UIImage(named: icon.assetName, in: .module, with: nil) != nil)` inside `#if canImport(UIKit)`, and run it through `xcodebuild test -scheme BabyTracker-Package -destination 'platform=iOS Simulator,name=iPhone 17 Pro' -only-testing:BabyUITests`. Record the command you used in `HANDOFF.md` §5.

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker Apps Makefile
git commit -m "feat(ui): custom line icon set, Meds and Mixed copy

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

### Task 14: Home — screen projection and views

**Files:**
- Create: `Packages/BabyTracker/Sources/HomeFeature/HomeScreen.swift`, `HeroView.swift`, `StatusLineView.swift`, `TodayStripView.swift`, `HomeDock.swift`
- Rewrite: `HomeFeature/HomeViewModel.swift`, `HomeView.swift`
- Delete: `HomeFeature/IdleStateView.swift`, `RunningTimerView.swift`, `QuickLogDock.swift`, `StopDock.swift`, `FeedAction.swift`, `TimerDial.swift`, and the tests that only exercised them (`FeedActionTests.swift`)
- Keep: `RunningTimerEditSheet.swift` / `RunningTimerEditModel.swift`, opened by tapping the hero
- Modify: `HomeFeature/Localizable.xcstrings`. Remove the keys that are no longer used and add the Task 13 keys this module uses.
- Test: `Packages/BabyTracker/Tests/HomeFeatureTests/HomeScreenTests.swift` (new), `HomeViewModelTests.swift` (rewritten)

**Interfaces:**
- ```swift
  public struct HomeScreen: Equatable {
      public var hero: Hero
      public var status: [StatusItem]
      public var strip: TodayStrip?
      public var dock: Dock
      public var joinChip: JoinChip?
      public var sleepChip: Date?
  }
  ```
  `sleepChip` is the start of a running sleep, shown while the feed is the hero.
- ```swift
  public enum Hero: Equatable {
      case feedRunning(icon: BabyIcon, title: String, startedAt: Date)
      case sleepRunning(startedAt: Date)
      case feedBreak(since: Date, parts: [PartSummary])
      case awake(since: Date?)
  }
  ```
- `public struct StatusItem: Equatable { let tracker: Tracker; let since: Date }`
- `public struct PartSummary: Equatable { let icon: BabyIcon; let seconds: Int }`
- `public struct JoinChip: Equatable { let kind: SessionKind; let previousParts: [PartSummary] }`
- `public struct TodayStrip: Equatable { let dayStart: Date; let sleeps: [DateInterval]; let feeds: [Date]; let feedCount: Int; let sleepSeconds: Int; let diaperCount: Int }`
- `public enum Dock: Equatable { case start(suggested: BreastSide); case feedRunning(timerID: UUID, isBottle: Bool); case sleepRunning(timerID: UUID); case readOnly }`
- `HomeScreen.make(snapshot: HomeSnapshot, suggested: BreastSide, canLog: Bool, now: Date) -> HomeScreen`: a pure function.
- `HomeViewModel`, a `@MainActor @Observable` class:
  - `init(store:babyID:volumeUnit:)`
  - `private(set) var snapshot: HomeSnapshot?`, `refresh() throws`
  - `func screen(at now: Date) -> HomeScreen`
  - `startBreast(_ side: BreastSide) async throws`
  - `startSleep() async throws`
  - `stopFeed() async throws -> StopResult`, where `enum StopResult { case stopped; case needsBottleAmount(timerID: UUID) }`
  - `stopSleep() async throws`
  - `detachRunning(_ kind: SessionKind) async throws`
  - `makeTimerEditor(timerID:)`

- [ ] **Step 1: Write the failing tests**

```swift
import Testing
import Foundation
import BabyCore
@testable import BabyStore
@testable import HomeFeature

@MainActor @Suite struct HomeScreenTests {
    @Test func idleShowsAwakeHeroStatusLineStripAndSuggestedSide() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopFeed(try store.startPart(.breast(.right), for: baby).advancing(clock, by: 600))
        clock.advance(by: 3_600)
        let screen = HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: try store.suggestedSide(for: baby),
                                     canLog: true, now: clock.now)
        #expect(screen.hero == .awake(since: nil))
        #expect(screen.dock == .start(suggested: .left))
        #expect(screen.status.map(\.tracker) == [.feed])
        #expect(screen.strip?.feedCount == 1)
    }

    @Test func feedRunningIsTheHeroAndStatusShowsSleepAndDiaper() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopSleep(try store.startSleep(for: baby).advancing(clock, by: 600))
        _ = try store.logDiaper(.wet, for: baby)
        let left = try store.startPart(.breast(.left), for: baby)
        let screen = HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: .right, canLog: true, now: clock.now)
        guard case .feedRunning(let icon, _, let startedAt) = screen.hero else { Issue.record("expected feed hero"); return }
        #expect(icon == .breastLeft)
        #expect(startedAt == left.startedAt)
        #expect(Set(screen.status.map(\.tracker)) == [.sleep, .diaper])
        #expect(screen.strip == nil)
        #expect(screen.dock == .feedRunning(timerID: left.id, isBottle: false))
    }

    @Test func sleepRunningShowsLastFeedAndDiaperInTheStatusLine() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.logBottle(volumeML: 60, contents: .formula, for: baby)
        _ = try store.logDiaper(.dirty, for: baby)
        clock.advance(by: 60)
        let sleep = try store.startSleep(for: baby)
        let screen = HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: .left, canLog: true, now: clock.now)
        #expect(screen.hero == .sleepRunning(startedAt: sleep.startedAt))
        #expect(Set(screen.status.map(\.tracker)) == [.feed, .diaper])
    }

    @Test func afterStopTheHeroIsTheBreakWithTheParts() throws {
        let (store, baby, clock) = try fixture()
        let left = try store.startPart(.breast(.left), for: baby)
        clock.advance(by: 542)
        _ = try store.stopFeed(left)
        clock.advance(by: 40)
        let screen = HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: .right, canLog: true, now: clock.now)
        guard case .feedBreak(_, let parts) = screen.hero else { Issue.record("expected break hero"); return }
        #expect(parts == [PartSummary(icon: .breastLeft, seconds: 542)])
        #expect(screen.dock == .start(suggested: .right))
    }

    @Test func aJoinedPartShowsTheChip() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 300))
        _ = try store.startPart(.breast(.right), for: baby)
        let screen = HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: .left, canLog: true, now: clock.now)
        #expect(screen.joinChip == JoinChip(kind: .feed, previousParts: [PartSummary(icon: .breastLeft, seconds: 300)]))
    }

    @Test func feedAndSleepTogetherShowFeedHeroAndSleepChip() throws {
        let (store, baby, clock) = try fixture()
        let sleep = try store.startSleep(for: baby)
        clock.advance(by: 30)
        _ = try store.startPart(.breast(.left), for: baby)
        let screen = HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: .right, canLog: true, now: clock.now)
        #expect(screen.sleepChip == sleep.startedAt)
    }

    @Test func viewOnlyCaregiversGetNoDock() throws {
        let (store, baby, clock) = try fixture()
        let screen = HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: .left, canLog: false, now: clock.now)
        #expect(screen.dock == .readOnly)
    }
}
```

`HomeScreenTests` needs `fixture()`/`advancing`, which live in the BabyStore test support. Copy `PartTestHelpers.swift` into `Tests/HomeFeatureTests/Support/`; test targets cannot import each other's support files. Rewrite `HomeViewModelTests.swift`, keeping only tests that still apply, such as refresh-on-revision and volume unit. Delete the tests for removed behaviour: switch side, pause, combine dialog and feed action.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `cd Packages/BabyTracker && swift test --filter 'HomeScreenTests|HomeViewModelTests'`
Expected: FAIL, because `HomeScreen` is undefined.

- [ ] **Step 3: Implement `HomeScreen.make`**

```swift
import Foundation
import BabyCore
import BabyStore
import BabyUI

public extension HomeScreen {
    @MainActor
    static func make(snapshot s: HomeSnapshot, suggested: BreastSide, canLog: Bool, now: Date) -> HomeScreen {
        let feedTimer = s.feedTimer
        let sleepTimer = s.sleepTimer
        let hero: Hero
        if let timer = feedTimer {
            let icon: BabyIcon = timer.currentBottleStartedAt != nil ? .bottle
                : timer.currentSide == .right ? .breastRight : .breastLeft
            hero = .feedRunning(icon: icon, title: HomeCopy.partTitle(icon), startedAt: timer.currentBottleStartedAt ?? timer.currentSideStartedAt)
        } else if let timer = sleepTimer {
            hero = .sleepRunning(startedAt: timer.startedAt)
        } else if let open = s.feedBreak {
            hero = .feedBreak(since: open.endedAt, parts: parts(of: open, in: s))
        } else {
            hero = .awake(since: s.awakeSince)
        }

        var status: [StatusItem] = []
        let feedIsHero = feedTimer != nil || s.feedBreak != nil
        if !feedIsHero, let lastFeedEnd = s.lastFeedingSession?.endedAt ?? s.lastFeed?.endedAt {
            status.append(StatusItem(tracker: .feed, since: lastFeedEnd))
        }
        if feedIsHero, sleepTimer == nil, let awake = s.awakeSince {
            status.append(StatusItem(tracker: .sleep, since: awake))
        }
        if let diaper = s.lastDiaper { status.append(StatusItem(tracker: .diaper, since: diaper.startedAt)) }

        let joinChip: JoinChip? = if let session = s.runningFeedSession, feedTimer != nil {
            JoinChip(kind: .feed, previousParts: parts(of: session, in: s))
        } else if let session = s.runningSleepSession, feedTimer == nil, sleepTimer != nil {
            JoinChip(kind: .sleep, previousParts: [PartSummary(icon: .sleep, seconds: session.asleepSeconds)])
        } else { nil }

        let strip: TodayStrip? = (feedTimer == nil && sleepTimer == nil)
            ? TodayStrip(dayStart: Calendar.current.startOfDay(for: now), sleeps: s.todaySleepSpans, feeds: s.todayFeedTimes,
                         feedCount: s.todayFeedCount, sleepSeconds: s.todaySleepSeconds, diaperCount: s.todayDiaperCount)
            : nil

        let dock: Dock = !canLog ? .readOnly
            : feedTimer.map { .feedRunning(timerID: $0.id, isBottle: $0.currentBottleStartedAt != nil) }
            ?? sleepTimer.map { .sleepRunning(timerID: $0.id) }
            ?? .start(suggested: suggested)

        return HomeScreen(hero: hero, status: status, strip: strip, dock: dock, joinChip: joinChip,
                          sleepChip: feedTimer != nil ? sleepTimer?.startedAt : nil)
    }

    @MainActor
    private static func parts(of session: FeedingSession, in s: HomeSnapshot) -> [PartSummary] {
        s.sessionMembers(session).flatMap(\.components).map { component in
            if let side = component.breastSide {
                PartSummary(icon: side == .left ? .breastLeft : .breastRight, seconds: component.durationSeconds ?? 0)
            } else {
                PartSummary(icon: .bottle, seconds: component.bottleSeconds ?? 0)
            }
        }
    }
}
```

Add `func sessionMembers(_ session: FeedingSession) -> [FeedEntry]` to `HomeSnapshot` in Task 10's file. It must resolve `feedIDs` against all feeds, so `homeSnapshot` keeps a `feedsByID` map for the sessions it exposes: `lastFeedingSession`, `feedBreak` and `runningFeedSession`.

`idleShowsAwakeHeroStatusLineStripAndSuggestedSide` expects `.awake(since: nil)` when no sleep was ever logged.

- [ ] **Step 4: Implement the views**

**`HeroView`** uses `TimelineView(.periodic(from: .now, by: 1))` for the running and break states, and `.everyMinute` for awake. It renders:
- `.feedRunning`: `BabyIconView(icon, size: 18)` plus the title in `Palette.feed` with a pulsing dot, then `Text(timerInterval: startedAt...Date.distantFuture, countsDown: false)` in `.numeralStyle(.hero)`, then a secondary caption `HomeCopy.since(startedAt)` ("since 3:02"). The whole hero is a `Button` that calls `onEdit`, with `accessibilityIdentifier("home.hero")` and the hint "Edit start time".
- `.sleepRunning`: the moon icon, the same numerals and caption.
- `.feedBreak`: the caption "Break" in feed colour, numerals counting up from `since`, then a row of `PartSummary` chips (icon plus `m:ss`).
- `.awake(since)`: the caption "Awake" in sleep colour and `ElapsedFormatter.compact(since:now:)` in `.numeralStyle(.hero)`. When `since` is nil, show "—".

**`StatusLineView`**: an `HStack(spacing: 14)` of `BabyIconView(item.tracker.icon, size: 14)` tinted with the tracker colour, plus compact elapsed text in `.footnote.weight(.semibold).monospacedDigit()`. It sits under the hero with `accessibilityElement(children: .combine)` and the label "Feed 2 hours 10 minutes, Diaper 1 hour 5 minutes", built from `Tracker.title` and `DateComponentsFormatter` with `.full` style.

**`JoinChipView`**:
- A capsule containing `"Same feed · L 9:02"`, built from `HomeCopy.sameFeed`/`sameSleep` and the part summaries.
- A trailing `Button` with `BabyIconView(.close, size: 12)`, `accessibilityLabel(HomeCopy.makeSeparate(kind))` and identifier `home.joinChip.separate`. It calls `onDetach(kind)`.

**`TodayStripView`**:
- A 10 pt rounded bar from `dayStart` to `now`. Sleeps are filled `Palette.sleep.opacity(0.7)`; feeds are 2 pt-wide `Palette.feed` ticks.
- Below the bar, a row of icon-and-number totals: feeds `strip.feedCount`, sleep `ElapsedFormatter.total(strip.sleepSeconds)`, diapers `strip.diaperCount`.
- The whole strip is one `Button` (`home.todayStrip`) that switches to the History tab through an `onOpenHistory` closure. Wire this in RootView with the tab selection binding; `grep -n "TabView(selection" Apps/BabyTrackerApp/RootView.swift` finds it.

**`HomeDock`**: wraps its content in `GlassDock`.
- **`.start(suggested)`:**
  - A `Grid` with two 64 pt buttons, L and R:
    - `BabyIconView(.breastLeft/.breastRight, size: 24)` plus the letter;
    - identifiers `home.start.left` / `home.start.right`;
    - accessibility labels "Start left breast" / "Start right breast";
    - the suggested side gets a 2 pt `Palette.feed` stroke and `accessibilityValue("Suggested")`.
  - Then a row of four 52 pt icon buttons:
    - Bottle (`home.bottle`, label "Bottle")
    - Sleep (`home.sleep`, label "Sleep")
    - Diaper (`home.diaper`, label "Diaper")
    - More (`home.more`, label "More")
- **`.feedRunning`:**
  - A 72 pt primary button "Stop" (`home.stop`), styled `.tracker(Palette.primaryText, emphasis: .primary)` with dark text.
  - Then a row: Diaper (`home.diaper`) and More (`home.more`).
- **`.sleepRunning`:**
  - A 72 pt primary "Wake up" (`home.wake`).
  - Then a row: Feed (`home.feed`, opens a `confirmationDialog` with Left breast, Right breast and Bottle), Diaper and More.
- **`.readOnly`:** the existing read-only text, `home.readOnly`.

Use `.buttonStyle(.tracker(...))` with `cornerRadius: 20` for every button, and `.frame(minHeight: 52)`.

**`HomeView`** composes these top to bottom:
1. The baby-switcher header (`home.babySwitcher`, unchanged), with the age on the right. Directly under it, keep the existing sync status card (`syncStatusCard`, `home.syncStatus`, driven by `HomeSyncStatus` and the `onSignIn` closure) exactly as merged in `19071bf`. Its recovery actions must stay reachable at accessibility sizes (commit `69d19d5`), and `HomeSyncStatusTests` must keep passing.
2. `HeroView` and, when present, `JoinChipView`.
3. `StatusLineView`.
4. `TodayStripView` (only when `strip != nil`). The sleep chip, when present, is a small capsule with a moon and live elapsed time.
5. `Spacer`, then `HomeDock`, with `.padding(.bottom, 12)`.

Keep the existing tick and reload wiring (`.onChange(of: store.revision)`, `scenePhase`). The `.task(id:)` loop is no longer needed, because the timelines tick. Remove the combine `confirmationDialog`.

When `stopFeed` returns `.needsBottleAmount(timerID)`, call `onFinishBottle(timerID)`. RootView then presents `.bottleStopped(timerID:)` from Task 15.

Update `HomeView.init`'s closures to:
- `onSwitchBaby`
- `onOpenSheet(LogSheet)`
- `onFinishBottle(UUID)`
- `onOpenHistory`

- [ ] **Step 5: Run the tests and build**

Run: `make test && make build SIMULATOR='platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853'`
Expected: PASS and `BUILD SUCCEEDED`. Update `EXPECTED_TESTS`.

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker Apps Makefile
git commit -m "feat(home): glance-and-dock Home with start/stop parts

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

### Task 15: Sheets — Diaper, Bottle, Bottle stopped, More, Past entry

**Files:**
- Modify:
  - `LoggingFeature/LogCoordinator.swift`: `LogSheet` cases become `bottle`, `bottleStopped(timerID: UUID)`, `bottleAmount(feedID: UUID)`, `solids`, `diaper`, `supplement`, `measurement`, `more`, `pastEntry`. Remove `feedMenu`, `attachBottle` and `finishTimedBottle`.
  - `DiaperSheet.swift`
- Rewrite: `LoggingFeature/BottleSheet.swift`
- Create: `LoggingFeature/MoreSheet.swift`, `PastEntrySheet.swift`
- Delete: `LoggingFeature/FeedMenuSheet.swift`, `Tests/LoggingFeatureTests/FeedMenuCopyTests.swift`
- Modify: `Apps/BabyTrackerApp/RootView.swift:185-199,275-287,358-393` (routing)
- Test: `Packages/BabyTracker/Tests/LoggingFeatureTests/BottleSheetModelTests.swift` (new), `MoreSheetModelTests.swift` (new), `PastEntryModelTests.swift` (new), `DiaperSheetCopyTests.swift` (updated)

**Interfaces:**
- `BottleSheetModel`, a `@MainActor @Observable` class:
  - `init(store:babyID:unit:mode:)`, where `mode` is `.start` or `.stopped(timerID: UUID)` or `.amount(feedID: UUID)`
  - `var contents: BottleContents`: starts at the last-used value, from `LogDefaults`
  - `var volumeML: Double`: starts at `mostUsedBottleVolumeML`
  - `let chips: [Double]`: `[30, 60, 90, 120]` in ml, or `[1, 2, 3, 4]` oz converted to ml
  - `func startBottle() throws`: `store.startPart(.bottle(contents))` plus `LiveActivityController.adoptOrStart`
  - `func logChip(_ ml: Double) throws`: `store.logBottle`
  - `func done() throws`: either `store.stopBottle(timer, volumeML:)` or `store.setBottleVolume(_:onFeed:)`, depending on the mode
  - `func step(_ direction: Int)`: ±10 ml, or ±½ oz
- `MoreSheetModel`:
  - `init(store:babyID:)`
  - `var meds: [String]`: `store.supplementNames(for:)`, most recent first
  - `func logMed(_ name: String) throws`: `store.logSupplement(name:for:)`
- `PastEntryModel`:
  - `enum Kind { case left, right, bottle, sleep }`
  - `var kind`, `var start`, `var end`, `var volumeML`, `var contents`
  - `var validationMessage: String?`: nil when valid. The end must be after the start, neither may be in the future, and a breast part must be at least 30 s.
  - `func save() throws`: calls `logBreast`, `logBottle(at:)` (start time), or `logSleep(from:to:)`

- [ ] **Step 1: Write the failing tests**

```swift
@MainActor @Suite struct BottleSheetModelTests {
    @Test func aChipLogsAFinishedBottleThatJoinsTheOpenFeed() throws {
        let (store, baby, clock) = try fixture()
        let first = try #require(try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 300)))
        let model = BottleSheetModel(store: store, babyID: baby, unit: .millilitres, mode: .start)
        model.contents = .formula
        try model.logChip(90)
        let last = try #require(try store.lastFeed(for: baby))
        #expect(last.totalBottleVolumeML == 90)
        #expect(last.feedingSessionID == first.id)
    }

    @Test func doneAfterATimedBottleSavesTheChosenAmount() throws {
        let (store, baby, clock) = try fixture()
        let timer = try store.startPart(.bottle(.breastMilk), for: baby)
        clock.advance(by: 400)
        let model = BottleSheetModel(store: store, babyID: baby, unit: .millilitres, mode: .stopped(timerID: timer.id))
        model.volumeML = 80
        model.step(1)
        try model.done()
        #expect(try store.lastFeed(for: baby)?.totalBottleVolumeML == 90)
        #expect(try store.activeTimers(for: baby).isEmpty)
    }

    @Test func ounceChipsAreWholeOunces() throws {
        let (store, baby, _) = try fixture()
        let model = BottleSheetModel(store: store, babyID: baby, unit: .ounces, mode: .start)
        #expect(model.chips.count == 4)
    }
}

@MainActor @Suite struct MoreSheetModelTests {
    @Test func savedMedsAreChipsAndATapLogsNow() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.logSupplement(name: "Vitamin D", for: baby)
        clock.advance(by: 60)
        _ = try store.logSupplement(name: "Iron", for: baby)
        let model = MoreSheetModel(store: store, babyID: baby)
        #expect(model.meds == ["Iron", "Vitamin D"])
        try model.logMed("Vitamin D")
        #expect(try store.supplementNames(for: baby).first == "Vitamin D")
    }
}

@MainActor @Suite struct PastEntryModelTests {
    @Test func reversedFutureAndTooShortAreRefused() throws {
        let (store, baby, clock) = try fixture()
        let model = PastEntryModel(store: store, babyID: baby)
        model.kind = .left
        model.start = clock.now
        model.end = clock.now.addingTimeInterval(-60)
        #expect(model.validationMessage != nil)
        model.end = clock.now.addingTimeInterval(60)
        #expect(model.validationMessage != nil)   // in the future
        model.start = clock.now.addingTimeInterval(-20)
        model.end = clock.now
        #expect(model.validationMessage != nil)   // 20 s breast
        model.start = clock.now.addingTimeInterval(-600)
        #expect(model.validationMessage == nil)
        try model.save()
        #expect(try store.lastFeed(for: baby)?.components.first?.breastSide == .left)
    }
}
```

In `DiaperSheetCopyTests`, assert that the fast path lists `[.wet, .dirty, .mixed]` and that the mixed label reads "Mixed".

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `cd Packages/BabyTracker && swift test --filter 'BottleSheetModelTests|MoreSheetModelTests|PastEntryModelTests|DiaperSheetCopyTests'`
Expected: FAIL, because the types are undefined.

- [ ] **Step 3: Implement the models and sheets**

Every sheet keeps the `sheetHeight(_:)` and `NavigationStack` pattern of the current sheets, and puts all copy in a `*Copy` enum.

**DiaperSheet:**
- Rows: a time row (an inline `DatePicker` labelled with the clock icon), then three 84 pt tiles: Wet, Dirty, Mixed. Each tile shows `BabyIconView.diaperContents` and a label, with identifiers `diaper.wet` / `diaper.dirty` / `diaper.mixed`.
- Then a `DisclosureGroup("Details")` (`diaper.details`) with the Consistency, Colour and Note fields (`diaper.note`).
- A tile tap calls `store.logDiaper(contents, for:, at:, consistency:, color:, note:)` and dismisses.
- Heights stay `isShowingDetail ? 470 : 300`.

**BottleSheet** is one view with three modes.
- **`.start`:** see mockup `sheets-v2.html`.
  - A time row.
  - A segmented `Picker` for contents (Breast milk, Formula, Mixed), `bottle.contents`.
  - A 64 pt primary "Start bottle" (`bottle.start`).
  - A row of 4 chips (`bottle.chip.<n>`). Tap logs. Long press switches into an inline exact field: a `Stepper` plus "Done" (`bottle.exact`).
- **`.stopped` and `.amount`:**
  - A caption.
  - A large volume number in `.numeralStyle(.display)` (`bottle.volume`).
  - The 4 chips, which select the amount rather than log it.
  - − and + buttons (`bottle.minus`/`bottle.plus`).
  - A 56 pt "Done" (`bottle.done`).

**MoreSheet:**
- Row 1: three 76 pt tiles: Solids (`more.solids`), Growth (`more.growth`), Past entry (`more.pastEntry`). They route through `onSwitchTo` to `.solids`, `.measurement` and `.pastEntry`.
- Row 2: a "Meds" caption, then a horizontal `ScrollView` of capsule chips (`more.med.<index>`) with the meds icon. A tap calls `logMed` and dismisses. A final "+" chip (`more.addMed`, label "Add med") switches to `.supplement`.

**PastEntrySheet:** a `Form` with these rows:
- `Picker` for kind (Left, Right, Bottle, Sleep)
- start and end `DatePicker`s
- volume and contents rows, when the kind is Bottle
- the validation message
- Save (`pastEntry.save`), disabled while invalid

**RootView:**
- `quickLog` and Home callbacks map:
  - bottle → `.bottle`
  - diaper → `.diaper`
  - more → `.more`
  - `onFinishBottle(id)` → `.bottleStopped(timerID: id)`
- When `PendingQuickLog` delivers the Task 16 set-amount marker, present `.bottleAmount(feedID:)`.
- Delete `startSleepIfNeeded` branches that toggled through `.feedMenu`.

- [ ] **Step 4: Run the tests and build**

Run: `make test && python3 scripts/verify-localizations.py && make build SIMULATOR='platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853'`
Expected: PASS, `OK`, and `BUILD SUCCEEDED`. Update `EXPECTED_TESTS`.

- [ ] **Step 5: Commit**

```bash
git add Packages/BabyTracker Apps Makefile
git commit -m "feat(logging): one-tap Diaper, Bottle, More and Past entry sheets

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

## Phase D — Lock Screen surfaces

### Task 16: Live Activity with break states and lock-safe buttons

**Files:**
- Modify: `Packages/BabyTracker/Sources/BabyStore/TimerActivityAttributes.swift`:
  - `ContentState` gains `phase`, `breakEndsAt`, `parts` and `suggestedSide`
  - `canSwitchSide` is removed
  - custom `init(from:)` with defaults
- Modify: `Packages/BabyTracker/Sources/BabyStore/LiveActivityController.swift`:
  - add `adoptOrStart(for:in:)`, `enterBreak(kind:babyID:in:)` and `endExpiredBreaks(in:)`
  - `ending` is replaced by `stopping`
- Modify: `Apps/BabyTrackerWidgets/FeedActivityWidget.swift`
- Modify: `Apps/SharedIntents/LogIntents.swift`:
  - add `StartBreastIntent`
  - `StopFeedIntent` also handles bottles
  - `ToggleSleepIntent` uses the break
  - delete `SwitchSideIntent`, `FinishBottleIntent` and `OpenFeedIntent`
- Modify: `Packages/BabyTracker/Sources/BabyFeature/PendingQuickLog.swift` (`.bottleAmount(feedID:)` request)
- Modify: `Apps/BabyTrackerApp/RootView.swift` (call `LiveActivityController.endExpiredBreaks` on `.active`), `RemoteTimerAnnouncer.swift`
- Test: `Packages/BabyTracker/Tests/BabyStoreTests/LiveActivityControllerTests.swift`, `ActivityTargetingTests.swift`, `BabyFeatureTests/PendingQuickLogTests.swift`

**Interfaces:**
- `public enum ActivityPhase: String, Codable, Sendable { case running, feedBreak, sleepBreak }`
- `ActivityPresentation` gains:
  - `phase`, `breakEndsAt: Date?`, `parts: [PartLabel]` (`struct PartLabel: Codable, Hashable { let side: String?; let seconds: Int }`), `suggestedSide: String?`
  - two static builders: `running(timer:)` and `feedBreak(session:limit:suggested:)` / `sleepBreak(endedAt:limit:)`
- `LiveActivityController`:
  - `static func stopping<T>(_ timer: ActiveTimer, in store: BabyStore, _ stop: () throws -> T) async rethrows -> T`: stops the timer, then moves the activity into the break phase, with `staleDate = breakEndsAt`.
  - `static func adoptOrStart(for timer: ActiveTimer, in store: BabyStore) async`: reuses the baby and kind's existing activity, if any.
  - `static func endExpiredBreaks(in store: BabyStore) async`
- `ActivityPresentation.isActionable(now:) -> Bool` is false once `breakEndsAt` has passed. The widget hides the buttons when it is false or when `context.isStale`.

- [ ] **Step 1: Write the failing tests**

```swift
@MainActor @Test func stoppingLeftPutsTheActivityIntoAFeedBreakSuggestingRight() throws {
    let (store, baby, clock) = try fixture()
    store.breakLimits = { .standard }
    let left = try store.startPart(.breast(.left), for: baby)
    clock.advance(by: 300)
    _ = try store.stopFeed(left)
    let snapshot = try store.homeSnapshot(for: baby)
    let presentation = try #require(ActivityPresentation.feedBreak(session: try #require(snapshot.feedBreak),
                                                                    limit: 1_800, suggested: .right))
    #expect(presentation.phase == .feedBreak)
    #expect(presentation.breakEndsAt == clock.now.addingTimeInterval(1_800))
    #expect(presentation.suggestedSide == "right")
    #expect(presentation.isActionable(now: clock.now.addingTimeInterval(1_799)))
    #expect(!presentation.isActionable(now: clock.now.addingTimeInterval(1_801)))
}

@Test func contentStateFromThePreviousBuildDecodes() throws {
    let old = #"{"startedAt":780000000,"sideLabel":"Left","activityKind":"breastLeft","canSwitchSide":true}"#
    let state = try JSONDecoder().decode(TimerActivityAttributes.ContentState.self, from: Data(old.utf8))
    #expect(state.phase == .running)
    #expect(state.parts.isEmpty)
}

@Test func aBottleStopRequestMarksTheAmountForLater() {
    // PendingQuickLogStore(defaults: isolated).request(.bottleAmount(feedID: X), babyID: B);
    // consume() returns .bottleAmount(feedID: X) once, then nil.
}
```

`contentStateFromThePreviousBuildDecodes` must sit inside `#if os(iOS)` like the type it tests. If package tests run on macOS only, instead test a `ContentStateDecoding` helper in BabyStore that is not iOS-only and that the iOS `ContentState.init(from:)` calls. Write the third test in full, following `PendingQuickLogTests.swift:54`.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `cd Packages/BabyTracker && swift test --filter 'LiveActivityControllerTests|PendingQuickLogTests'`
Expected: FAIL.

- [ ] **Step 3: Implement**

**`ContentState`:**
- Fields: `startedAt`, `sideLabel`, `activityKind`, `phase` (default `.running`), `breakEndsAt` (default nil), `parts` (default `[]`) and `suggestedSide` (default nil).
- Its `init(from:)` uses `decodeIfPresent` for every field added after the previous build.

**`LiveActivityController`:**
- **`stopping`:**
  1. Capture the baby and kind.
  2. Run `stop()`.
  3. Build the break presentation from `store.homeSnapshot(for:)`. Feed uses `feedBreak` and `breakLimits().feed`; sleep uses `breakEndsAt = awakeSince + limit`.
  4. `push` it with `ActivityContent(state:, staleDate: breakEndsAt)`.
  5. If there is no break (for example, the feed was all short taps and no previous session is in range), end the activity immediately, as today.
- **`adoptOrStart`:**
  1. Find `Activity<TimerActivityAttributes>.activities.first { $0.attributes.babyID == timer.babyID && $0.attributes.kindRaw == timer.kind.rawValue }`.
  2. If found: call `store.setLiveActivityID(activity.id, on: timer)` (the existing setter used by `start`), then `update(for: timer)`.
  3. Otherwise call `start(for:in:)`.
- **`endExpiredBreaks`:** end every activity whose state has `phase != .running` and `breakEndsAt < now`, or whose baby and kind has neither a running timer nor an open break. Use `dismissalPolicy: .immediate`.

**Intents (`LogIntents.swift`):**
- `StartBreastIntent: LiveActivityIntent`:
  - Parameters: `babyID: String` and `side: String` (`"left"`/`"right"`).
  - `perform`:
    1. `let store = try WidgetStore.make()`, then resolve the baby the way `StopFeedIntent` does.
    2. `let timer = try store.startPart(.breast(side), for: baby.id)`.
    3. `await LiveActivityController.adoptOrStart(for: timer, in: store)`.
  - No `openAppWhenRun`.
- `StopFeedIntent`:
  - If the running feed timer has `currentBottleStartedAt`, call `store.stopBottle(timer, volumeML: LogDefaults(store: store).mostUsedBottleVolumeML(for:))`, then `PendingQuickLogStore().request(.bottleAmount(feedID: entry.id), babyID:)`.
  - Otherwise call `stopFeed`.
  - Both paths are wrapped in `LiveActivityController.stopping`.
  - No `openAppWhenRun`.
- `ToggleSleepIntent`:
  - The stop path uses `stopping`.
  - The start path uses `startSleep` (which now joins), then `adoptOrStart`.
- Delete `SwitchSideIntent`, `FinishBottleIntent` and `OpenFeedIntent`. Remove their uses from widgets, `PendingQuickLog` (`requestBottleFinishAndStop`) and `RootView`.

**`FeedActivityWidget`:**
- `.running`: the part icon (`BabyIconView(state.activityKind.icon)`), `Text(state.startedAt, style: .timer)`, and one `Button(intent: StopFeedIntent / ToggleSleepIntent)` labelled "Stop" or "Wake up".
- `.feedBreak`, when actionable and not stale:
  - the caption "Break" and `Text(state.startedAt, style: .timer)`, where `startedAt` is the break start, which `stopping` sets;
  - the parts row;
  - two 44 pt buttons `Button(intent: StartBreastIntent(babyID:, side: "left"))` and the right equivalent, with the suggested one outlined.
- `.sleepBreak`, when actionable: "Awake" plus the timer, and one `Button(intent: ToggleSleepIntent(babyID:))` labelled "Sleep".
- Stale or not actionable: show only the icon and the parts summary, with no buttons.
- The Dynamic Island compact and minimal views show the icon and the timer. The expanded view mirrors the Lock Screen.

- [ ] **Step 4: Run the tests and build**

Run: `make test && make build SIMULATOR='platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853'`
Expected: PASS and `BUILD SUCCEEDED`. Update `EXPECTED_TESTS`.

- [ ] **Step 5: Commit**

```bash
git add Packages/BabyTracker Apps Makefile
git commit -m "feat(live-activity): break states with lock-safe start and stop buttons

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

### Task 17: Widgets, Siri and App Shortcuts

**Files:**
- Modify: `Apps/BabyTrackerWidgets/QuickLogWidget.swift` (L · R · Sleep · Diaper), `StatusWidget.swift` (new icons)
- Modify: `Apps/SharedIntents/SiriIntents.swift`:
  - `SiriStartBreastIntent` uses `startPart` and `adoptOrStart`
  - `SiriStopFeedIntent` uses `stopping`
  - `SiriStopSleepIntent` gets the title "Wake up"
  - delete `SiriSwitchBreastIntent`
- Modify: `Apps/BabyTrackerApp/BabyTrackerShortcuts.swift`:
  - remove "Switch breast"
  - the sleep-stop phrase becomes "Wake up in \(.applicationName)", with `shortTitle` "Wake up"
- Modify: `Apps/SharedIntents/Localizable.xcstrings`, `Apps/BabyTrackerApp/AppShortcuts.xcstrings`
- Test: `UITests/BabyTrackerUITests/WidgetTourTests.swift` (screenshots), and package tests where intent logic moved into `BabyStore`

**Interfaces:**
- Quick Log widget buttons:
  - `StartBreastIntent(babyID:, side: "left")`
  - `StartBreastIntent(babyID:, side: "right")`
  - `ToggleSleepIntent(babyID:)`
  - `LogDiaperIntent(babyID:)`
- Each button shows `BabyIconView` plus a short label (L, R, Sleep, Diaper) on its tracker tint.

- [ ] **Step 1: Write the failing test**

Most of this is wiring. Pin the shared decision as a package test in `ActivityTargetingTests.swift`:

```swift
@MainActor @Test func aWidgetStartWhileTheOtherSideRunsStopsItAndJoins() throws {
    let (store, baby, clock) = try fixture()
    _ = try store.startPart(.breast(.left), for: baby)
    clock.advance(by: 200)
    let right = try store.startPart(.breast(.right), for: baby)   // what StartBreastIntent calls
    #expect(right.sessionID == (try store.lastFeed(for: baby))?.effectiveFeedingSessionID)
}
```

- [ ] **Step 2: Run the test and confirm it passes**

This behaviour came from Task 9, so the test guards it rather than drives it.

Run: `cd Packages/BabyTracker && swift test --filter ActivityTargetingTests`
Expected: PASS.

- [ ] **Step 3: Implement**

Update the files listed above.

**`SiriStartBreastIntent`:**
- Drop its "refuses when a feed is already running" branch. Starting the other side now stops the current one.
- Keep the refusal when the same side is already running, returning the dialog "Left breast is already running." (existing copy).

**`SiriStopSleepIntent`:**
- `title`: "Wake up"
- dialog: "Woke up." Add translations: de "Aufgewacht.", fr "Réveillé.", es "Despertó.", it "Sveglio.", uk "Прокинулися."

- [ ] **Step 4: Build and verify the localizations**

Run: `make build SIMULATOR='platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853' && python3 scripts/verify-localizations.py`
Expected: `BUILD SUCCEEDED` and `OK`.

- [ ] **Step 5: Commit**

```bash
git add Apps Packages/BabyTracker
git commit -m "feat(widgets): L/R/Sleep/Diaper quick log, Wake up shortcut, no Switch side

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

### Task 18: Remove the Switch/Pause client paths

**Files:**
- Modify: `Packages/BabyTracker/Sources/BabyStore/BabyStore+Feeding.swift`:
  - delete `switchSide`, `pauseFeed`, `beginBreast`, `beginBottle`, `attachBottle`, `appendBottle`, `startFeed`, `startBottle`, `startAnnouncedFeed` and `startAnnouncedBottle` once no callers remain (`grep -rn` each name across `Sources` and `Apps`)
  - keep `finishBottle`, because `stopBottle` uses it
  - keep `suggestedSide`
- Modify: `BabyStore/TimerIntentReducer.swift`. **Keep** the `switchSide` and `pause` intent cases and payload builders only if remote-apply tests still need them. Otherwise remove them. The server keeps accepting these commands from older phones; the new client never sends them.
- Modify: `BabyStore/FeedingSessions.swift`: delete `combineCandidate` (no callers).
- Modify or delete tests: `TimerTests.swift`, `ComboFeedTests.swift`, `TimedBottleTests.swift`, `SharedTimerTests.swift` (pause tests at 10-56), `AnnouncedFeedTests.swift`, `FeedingSessionsTests.swift` (combineCandidate tests).
  - Rewrite any test that guards a still-relevant invariant (short-part cleanup, timed bottle step times, remote stop) against `startPart`.
  - Delete tests of the removed APIs.
  - Move `aLegacyMultiPartTimerStillStopsIntoOneFeed` (Task 9) to build its timer through `RemoteTimerState` / `rematerializeTimer`, using a remote v4 payload with two completed components. Old phones can still create such timers.

**Interfaces:** none new.

- [ ] **Step 1: Delete the callers' targets and fix the build**

Run: `cd Packages/BabyTracker && swift build 2>&1 | grep error: | head -50`
Expected: the only errors are in code that this task removes or rewrites. Fix each one by switching to `startPart` / `stopBottle` / `stopFeed`.

- [ ] **Step 2: Run the full suite**

Run: `make test`
Expected: PASS. Set `EXPECTED_TESTS` to the new total. The commit message must name the deleted test files and why they were deleted, as `Makefile`'s comment requires.

- [ ] **Step 3: Commit**

```bash
git add -A Packages/BabyTracker Makefile
git commit -m "refactor(store): drop Switch/Pause/attach client paths replaced by one-part timers

Deleted tests covered only removed APIs (switchSide, pauseFeed, attachBottle,
startFeed, combineCandidate); invariants they guarded are re-pinned against startPart.

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

## Phase E — UI tests and verification

### Task 19: UI tests, accessibility, two-caregiver E2E

**Files:**
- Modify:
  - every UI test that uses the removed labels or identifiers (`"Log feed"`, `"Stop feed"`, `"Log sleep"`, `"Stop sleep"`, `"Log diaper"`, `"Choose feed type"`, `"Log supplements"`, `home.running.pause`, `home.running.add`, `home.timerDial`, `home.lastFeed.*`, `home.secondaryTimer.*`, `home.measure`, "Finish bottle and stop")
  - their new equivalents: `home.start.left`, `home.start.right`, `home.bottle`, `home.sleep`, `home.diaper`, `home.more`, `home.stop`, `home.wake`, `home.feed`, `home.hero`, `home.joinChip.separate`, `home.todayStrip`, `more.growth`, `more.pastEntry`, `more.med.<i>`, `more.addMed`, `bottle.*`, `diaper.*`
- Modify: `UITests/BabyTrackerUITests/UITestHarness.swift` and `Apps/BabyTrackerApp/UITestSupport.swift`:
  - `-uiTestRunningFeed` seeds a one-part Left timer
  - add `-uiTestFeedBreak`, which seeds a Left part that ended 40 s ago
  - add `-uiTestJoinedFeed`, which seeds a stopped Left part and a running Right part joined to it
- Modify: `AccessibilityAuditTests.swift`:
  - audit `home-idle`, `home-feed-running`, `home-feed-break`, `home-sleep-running`, `diaper-details`, `bottle-start`, `more` and `past-entry`
  - keep the XXXL dock hittability test against the new dock
- Modify: `ScreenshotTourTests.swift`. `testTourMainScreens` captures Home in every hero state plus each sheet.
- Modify: `UkrainianLayoutSmokeTests.swift`. Run the Home states in Ukrainian at the largest non-accessibility text size (`-UIPreferredContentSizeCategoryName UICTContentSizeCategoryXXXL`) on the shared iPhone 17 Pro. This stands in for a compact screen.
- Create: `Packages/BabyTracker/Tests/FamilySyncClientE2ETests/OnePartTimerSyncE2ETests.swift`, containing `twoCaregiversStopAndStartKeepOneSession`
- Modify: `TwoCaregiverSyncE2ETests.swift:998-1015`. Add `"Sleep-Session"` to the header loop.

**Interfaces:** consumes all earlier tasks.

- [ ] **Step 1: Write the E2E test**

Use the existing two-caregiver harness in `TwoCaregiverSyncE2ETests.swift`. Read how it makes `owner` and `caregiver` stores and syncs them.

```swift
@MainActor @Test func twoCaregiversStopAndStartKeepOneSession() async throws {
    let (owner, caregiver, baby, clock) = try await twoSyncedCaregivers()   // existing helper; adapt the name
    let left = try owner.store.startPart(.breast(.left), for: baby)
    try await owner.sync(); try await caregiver.sync()
    clock.advance(by: 300)
    _ = try owner.store.stopFeed(left)                                         // A stops
    let right = try caregiver.store.startPart(.breast(.right), for: baby)      // B still sees Left running
    try await caregiver.sync(); try await owner.sync(); try await caregiver.sync()
    clock.advance(by: 200)
    _ = try caregiver.store.stopFeed(right)
    try await caregiver.sync(); try await owner.sync()
    for side in [owner, caregiver] {
        let sessions = try side.store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture)
        #expect(sessions.count == 1)
        let parts = sessions[0].feedIDs.compactMap { try? side.store.entry(id: $0, for: baby) }
        #expect(parts.count == 2)
    }
}
```

If the harness cannot drive both stops into the conflict, assert the documented outcome instead:
- exactly one Left part, whichever stop the server accepted;
- the caregiver's own Left stop settles as `superseded`, through the existing `settleRefusedTimerCommand` / `preserveCompletionOfSupersededStart` paths;
- Right joins the surviving Left's session.

Record which outcome the harness produced in the test's comment.

- [ ] **Step 2: Run the package and E2E suites**

Run: `make test`. If the E2E target needs a server (check `Package.swift` and the README), also run `make server-protocol-e2e`.
Expected: PASS. Update `EXPECTED_TESTS`.

- [ ] **Step 3: Update the UI tests and run them**

Run: `make test-ui SIMULATOR='platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853'`
Expected: `** TEST SUCCEEDED **`, including every `AccessibilityAuditTests` case. Fix the layout or identifiers rather than adding new expected-failure wrappers.

- [ ] **Step 4: Commit**

```bash
git add UITests Apps Packages/BabyTracker Makefile
git commit -m "test: UI, accessibility and two-caregiver coverage for one-part timers

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

### Task 20: End-to-end verification and handoff

**Files:**
- Create: `docs/verification/2026-10-01-home-tracking-redesign.md`
- Modify: `HANDOFF.md` §5 if the asset-catalog test command changed, and `TASKS.md` on `main` only through the user's normal flow. Do not edit it from the branch.

- [ ] **Step 1: Run every gate from a clean tree**

```bash
git status --short            # must be empty
make test
make server-test
make server-smoke
make server-protocol-e2e
python3 scripts/verify-localizations.py
make build SIMULATOR='platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853'
make test-ui SIMULATOR='platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853'
```

Expected: every command passes. Record each command and its result line in the verification doc.

- [ ] **Step 2: Simulator walkthrough (shared iPhone 17 Pro `D17101C9-01EB-488D-AFE1-58E6C089B853`, English)**

Use a fresh simulator install with `-uiTestResetStore -uiTestSeedBaby`, and save screenshots with `xcrun simctl io <udid> screenshot`. Walk through each scenario and check:

1. Tap L, wait 1 min, Stop. Hero shows "Break" and the L part. R is outlined.
2. Tap R within the limit. The "Same feed · L 1:00" chip appears. Stop gives one feed in History.
3. Set Settings → Tracking feed limit to 5 min, wait 6 min, tap L. There is no chip, and History shows two feeds.
4. Tap R, then ✕ on the chip, then Stop. Two separate feeds.
5. Sleep, Wake up, then Sleep within 15 min. The chip "Same sleep" appears, and History shows one sleep row.
6. Bottle → chip 90: one bottle joins the open feed. Bottle → Start bottle → Stop: the amount sheet appears, then Done.
7. Diaper → Details → Note → Mixed. The History row shows the note and Mixed.
8. ··· → Meds chip: logged. ··· → +: the add-med flow. ··· → Past entry: Left 10 min, 1 h ago. It joins or doesn't, per the rule.
9. Sleep running: the status line shows last feed and last diaper.

Compare each screenshot to `mockups/2026-10-01-home-redesign/*.html`. List any deviations in the verification doc.

- [ ] **Step 3: Ukrainian on the shared iPhone 17 Pro at XXXL text**

Launch with `-AppleLanguages "(uk)"` and capture Home in every hero state plus each sheet. Fix any truncation or overlap before continuing.

- [ ] **Step 4: Local two-caregiver sync**

Run the local server (`make server-smoke` leaves a disposable stack; follow `Server/scripts/smoke.sh` or `docs/operations`). The policy allows only one simulator, so use the shared simulator as the owner. Use the `FamilySyncClientE2ETests` two-caregiver harness, or `Server/cmd/protocol-e2e-client`, as the second caregiver. Repeat scenarios 1, 2 and 5 across both phones, and confirm that both show the same grouping.

- [ ] **Step 5: Final review and handoff**

1. Request a whole-branch review with superpowers:requesting-code-review against the spec.
2. Fix any blockers.
3. Write the verification doc: commands, results, screenshots paths, deviations, open risks.
4. Commit it:

```bash
git add docs/verification/2026-10-01-home-tracking-redesign.md HANDOFF.md
git commit -m "docs: verify home and tracking redesign end to end

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

5. Ask the user for the only manual step, the locked-iPhone check:
   - install the signed build on one phone (with approval);
   - start L from the app;
   - lock the phone;
   - press Stop on the Lock Screen Live Activity;
   - in the break, press R without unlocking;
   - press Stop;
   - start Sleep from the Quick Log widget;
   - press Wake up on the Lock Screen;
   - press Sleep in the break.

   Expected: no unlock prompt and no app launch at any point. Record the result in the verification doc.

Server rollout (migration 26, deploy before the new app) and installs on both phones need separate explicit approval.
