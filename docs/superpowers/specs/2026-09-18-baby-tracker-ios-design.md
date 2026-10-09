# Baby Tracker (iOS) — Design Spec

Date: 2026-09-18
Status: Approved for planning

## 1. Purpose

A native iOS app for logging a baby's feeding, sleep, and diaper changes.
Inspired by Nara Baby, reduced to the three trackers that get used many times
a day.

The governing constraint is that logging happens one-handed, in the dark, while
holding a baby, often at 3am. Every design decision below is subordinate to
that: a flow that takes five taps instead of two has failed, regardless of how
much more data it captures.

## 2. Scope

### In scope (v1)

- Feeding: breast (per-side intervals), bottle (volume + contents), solids,
  and combination feeds
- Sleep: start/stop with derived nap/night classification
- Diaper changes: wet / dirty / mixed / dry, with optional detail
- Multiple babies
- History: reverse-chronological, grouped by day, editable
- Insights: today vs. 7-day average, four figures only
- Live Activity / Dynamic Island for running feed and sleep timers
- Home Screen widgets: glanceable status, and App Intent quick-log buttons
- Offline-first local persistence, with a sync seam built but not wired

### Out of scope (v1), deliberately

- Pumping, growth (weight/height/head circumference), vaccines. The data model
  must not make these awkward to add, but no code ships for them.
- The sync server. See §8 — v1 builds the seam only.
- Apple Watch app.
- Notifications and feeding reminders. Excluded on judgment, not effort:
  reminders are the feature most likely to make an anxious parent feel worse,
  and they are cheap to add later if genuinely missed.
- Any percentile curve, prediction, or other output that could be read as
  medical advice.

## 3. Platform and stack

- iOS 26, SwiftUI, SwiftData, Swift 6.4, Xcode 27
- Strict concurrency enabled
- No third-party dependencies

### Why not CloudKit

Partner sharing was the original sync requirement, and CloudKit sharing was the
obvious candidate. It was rejected on two findings:

1. SwiftData does not support CloudKit's shared or public database. Apple DTS
   guidance is to use `NSPersistentCloudKitContainer` (Core Data) for those
   scopes. Using CloudKit sharing would mean giving up SwiftData.
2. The project's chosen direction is a self-hostable open-source sync node
   (Go or Rust, default hosted instance, Docker-deployable), which CloudKit
   cannot serve at all.

v1 therefore uses plain local SwiftData and defers sync entirely.

## 4. Architecture

A thin Xcode project over a local Swift package. The app target and widget
extension contain almost no code.

```
BabyTracker.xcodeproj
├── BabyTrackerApp        (app target: entry point, tab scaffolding)
└── BabyTrackerWidgets    (widget extension: widgets + Live Activity)

Packages/BabyTracker/
├── BabyCore              value types, units, date/timezone math, clock
├── BabyStore             ModelContainer, mutations, queries, change-log outbox
├── BabySync              sync protocol + LocalOnlySync no-op
└── Features/
    ├── FeedingFeature
    ├── SleepFeature
    ├── DiaperFeature
    ├── HistoryFeature
    └── InsightsFeature
```

Rules that keep the boundaries real:

- Only `BabyStore` touches a `ModelContext`. Features call intent-named methods
  (`logDiaper(_:for:)`, `startFeed(side:for:)`) and observe typed queries.
- `BabyCore` has no SwiftData and no SwiftUI import. It is pure and fully
  unit-testable.
- Features do not depend on each other. Shared UI goes in a `BabyUI` module if
  and when a second consumer appears — not before.
- The app target and widget extension both depend on `BabyStore`. This is what
  makes widget data access clean rather than an App Group file-sharing hack.

## 5. Domain model

All entries share a common field set. Each kind is a distinct SwiftData model;
the shared fields are duplicated rather than expressed through inheritance,
which SwiftData handles poorly.

```
Baby
  id: UUID, name: String, birthDate: Date, photoData: Data?, sortOrder: Int
  entries: cascade delete

Common entry fields
  id: UUID
  babyID: UUID
  startedAt: Date
  endedAt: Date?
  timeZoneID: String
  note: String?
  createdBy: String          // device identifier
  lastModifiedAt: Date
  isDeleted: Bool            // tombstone

FeedEntry
  kind: .breast | .bottle | .solids
  sides: [SideInterval]      // SideInterval(side: .left | .right, seconds: Int)
  volumeML: Double?
  contents: .breastMilk | .formula | .mixed | nil
  foods: [String]
  amount: .taste | .some | .lots | nil

SleepEntry
  (common fields only; classification derived)

DiaperEntry
  contents: .wet | .dirty | .mixed | .dry
  consistency: String?
  color: String?

ActiveTimer
  babyID: UUID, kind: TimerKind, startedAt: Date,
  pausedIntervals: [DateInterval], liveActivityID: String?
  invariant: at most one per (babyID, kind)

ChangeLogEntry
  id: UUID, entityID: UUID, entityType: String,
  operation: .create | .update | .delete,
  payload: Data (JSON), timestamp: Date, deviceID: String, syncedAt: Date?
```

### Model decisions

**Combo feeds are not a fourth kind.** A combination feed is a `FeedEntry` with
both a non-empty `sides` array and a `volumeML`. A fourth enum case would force
every aggregate query ("total intake today") to special-case it.

`kind` records how the feed *started* and drives icon and default edit sheet
only; it is never used to decide which fields are populated. Any aggregate must
read the fields themselves. A computed `isCombo` (sides non-empty AND volumeML
non-nil) exists for display purposes.

**`timeZoneID` is stored per entry.** A feed logged at 2am in Berlin must still
report as a 2am feed after travelling to New York. Day bucketing and the
nap/night classification both resolve against the entry's own timezone, not the
device's current one.

**Deletes are tombstones.** `isDeleted` is set; rows are never removed. A future
sync node cannot replicate a deletion it never observed. This is the single
hardest property to retrofit, so it ships in v1 even though nothing consumes it
yet. Queries filter `isDeleted == false` by default.

**`ActiveTimer` is persisted, not in-memory state.** This is what makes a
running timer survive force-quit, reboot, and the Live Activity.

**Breast side is a list of intervals, not a single value.** Switching sides
mid-feed appends an interval. `lastSideUsed` is derived from the final interval
and drives next-feed side pre-selection.

## 6. UI structure

Three tabs, in a standard `TabView` so the app inherits iOS 26's compact tab bar
and Liquid Glass treatment.

### Home

Home has two states. This is a correctness requirement, not a styling choice:
with nothing running there is no quantity to plot and no target to plot it
against, so any dial or arc on the idle screen would imply a prediction the
spec forbids (§2).

**Idle state** (no timer running) — layout A2, selected from mockups:

- Baby switcher as an icon-only control in the navigation bar, hidden entirely
  when only one baby exists
- Three status rows, equal weight: last feed, sleep, last diaper. Each carries a
  label, a detail line, and the relative time at display size (~25pt).
- "Today so far" strip: feed count, total sleep, diaper count
- Quick-log dock: three large one-handed targets, floating above the tab bar

Nothing in the scrolling content area is a control. Picking up the phone to read
the screen must never be able to write data.

**The sleep row is stateful.** Asleep, it reads "Asleep · 42m" and counts live.
Awake, it reads "Awake · 40m" with "slept 1h 50m before that" beneath. A uniform
"last sleep 2h 7m ago" is ambiguous at 3am and is not acceptable.

**Running state** (one or more timers active):

- The dial appears, showing elapsed time for the running timer
- The dial's ring is a seconds hand for the current minute — it sweeps 0→60 and
  resets. It encodes real data and gives live confirmation the timer is running.
  It must never represent progress toward a target.
- For breast feeds, a Left/Right toggle sits below the dial showing per-side
  elapsed, so the decision to switch is made with the number visible
- The quick-log dock is replaced by a solid, filled "Stop" button occupying the
  same thumb region, so the primary action is in one place in both states
- The other trackers remain reported in quiet rows below
- **Concurrent timers:** sleep and a feed can legitimately run at once. The dial
  shows the most recently started timer; any other running timer appears as a
  live row below with its own stop control. No modal conflict, no forced choice.
- This layout is what the Live Activity mirrors

### History

Reverse-chronological, grouped by day, with a filter chip per kind. Day headers
carry that day's totals. Swipe to delete (tombstone), tap to edit.

### Insights

Four figures only: today vs. 7-day average for feed count, total feed volume,
total sleep, and diaper count. No curves, no predictions.

## 7. Logging flows

Tap counts are requirements, not aspirations.

| Flow | Interaction | Taps |
|---|---|---|
| Breast feed | Tap Breast → timer starts on the side opposite the last feed (pre-selected, overridable) → Stop | 2 |
| Bottle | Tap Bottle → volume stepper pre-filled with most-used amount, contents pre-filled with last used → Save | 2 |
| Diaper | Tap Diaper → wet/dirty/mixed → saves immediately, no confirmation screen | 2 |

`.dry` is reachable from the edit sheet but is deliberately absent from the
three-option fast path, which covers the overwhelming majority of changes.
| Sleep | Tap Sleep to start, tap again to stop | 2 |

Supporting rules:

- Every entry is editable after creation. The common real-world failure is
  logging at the right moment with the wrong detail, or remembering a feed 40
  minutes late.
- Every sheet has a tappable time field defaulting to "now" that accepts a
  correction.
- Optional detail fields (diaper consistency and color) live behind a collapsed
  "add detail" disclosure and never appear in the fast path.
- Nap vs. night is derived from the entry's local-time window, with a manual
  override available in edit. The user is never asked at log time.

## 8. Persistence, timers, and the sync seam

### Store

`BabyStore` owns a SwiftData `ModelContainer` located in a shared App Group
container, so app, widget extension, and Live Activity intents read one
database.

### The change-log choke point

Every mutation passes through a single `record(_:)` function that writes the
entity and appends its `ChangeLogEntry` **in the same transaction**. The
invariant — no mutation without a corresponding change-log entry — is enforced
by test.

Nothing drains the outbox in v1. When the sync node ships, a sync client drains
it and no other app code changes. That is the entire justification for building
it now, while it costs one function.

### Sync seam

`BabySync` ships as a protocol plus a `LocalOnlySync` no-op implementation. The
protocol is exercised by tests from day one; an unexercised seam is an imagined
seam.

### Timers

- Elapsed time is always computed as `now − startedAt` from the persisted
  `ActiveTimer`, never accumulated in memory. Force-quit, reboot, and long
  backgrounding all yield a correct value on relaunch.
- `BabyCore` exposes a `Clock` abstraction; all time reads go through it so
  timer behaviour is testable without waiting.
- The Live Activity uses SwiftUI's range-based timer text so the Lock Screen
  counts up without the app running.
- Stopping a timer from the Lock Screen goes through an App Intent that calls
  the same `BabyStore` method as the in-app button. There is exactly one code
  path for stopping a feed.

### Widgets

Two families: a small "time since last feed / sleep / diaper" glance, and a
medium one with App Intent buttons that log a diaper or start a feed without
opening the app. Both reload their timeline after any mutation. Widget buttons
target the currently selected baby, which is persisted in shared defaults.

## 9. Visual design (iOS 26)

Direction: **Night Shift** — deep ink navy ground, amber accent, dark-first.
Amber is the accent least hostile to dark-adapted eyes, which matters because
the app's peak usage is at night. Reference mockups live in `mockups/`.

Palette:

| Role | Hex | Used for |
|---|---|---|
| Ground | `#0D1224` → `#080B16` | Radial background, dark |
| Feed | `#F2B061` | Feed accent, active timer, selected tab |
| Sleep | `#8A9BF0` | Sleep accent |
| Diaper | `#5FD3C4` | Diaper accent |
| Primary text | `#F3F5FD` | Titles, values |
| Secondary text | `#9AA4C9` | Detail lines, units |

Numerals use a rounded face with tabular figures throughout, so live-updating
times do not jitter.

The rule is to adopt the system's Liquid Glass, not imitate it.

- Standard `TabView`, `NavigationStack`, and `.toolbar` provide the new material,
  the compact iPhone tab bar, and scroll-edge behaviour automatically, and stay
  correct as iOS 26.x adjusts the material.
- Custom `.glassEffect` is used in exactly one place: the floating quick-log
  dock (and, in the running state, the stop control that replaces it), inside a
  single `GlassEffectContainer`. Status rows use ordinary translucent fills, not
  `.glassEffect`.
- **Glass never overlaps glass.** Glass cannot sample glass; overlapping
  produces visual artifacts. The floating cluster is anchored outside the scroll
  view so it does not collide with toolbar or tab bar glass; scrolling content
  passes under the tab bar rather than fighting it.
- All other surfaces are ordinary opaque content. An app where everything is
  translucent is an app where nothing reads as important.

### Typography and accessibility

- Dynamic Type throughout; no fixed font sizes
- Usable at AX5: quick-log buttons reflow to a vertical stack rather than
  truncating, since that is precisely the screen needed at 3am
- All tap targets ≥ 44pt; primary quick-log buttons substantially larger
- Each tracker has one semantic color used consistently across quick-log button,
  history row, and widget, verified at 4.5:1 contrast in both light and dark
- Color is never the sole carrier of meaning; each kind also has a distinct
  SF Symbol
- Full VoiceOver labelling, including the running timer and Live Activity

### Dark mode

Designed first-class, not derived. Night feeds are peak usage: the dark palette
is designed on its own terms, and the running-timer screen avoids large bright
fills that destroy night vision.

## 10. Testing

Coverage concentrates in `BabyCore` and `BabyStore`; views have thin bodies and
no logic.

- Every store mutation, against an in-memory `ModelContainer`
- The change-log invariant: no mutation without a matching `ChangeLogEntry`
- Tombstone semantics: deleted entries excluded from queries, retained in store
- Timezone-stable day bucketing, including across a DST boundary
- Nap/night classification, including edge times
- Timer correctness with an injected clock, explicitly covering "app killed for
  two hours mid-feed"
- Unit conversion round-trips (ml ↔ oz) without drift
- `BabySync` protocol conformance exercised via `LocalOnlySync`
- XCUITests for the four two-tap logging flows end to end

## 11. Units and storage conventions

Canonical storage, converted only at display time:

- Volumes in millilitres (`Double`)
- Durations in seconds (`Int`)
- Timestamps as UTC `Date` plus the entry's `timeZoneID`

Display honours a user preference for ml/oz.

## 12. Deferred-feature compatibility

Pumping, growth, and vaccines are out of scope but must not require a
restructure:

- Pumping fits the existing entry shape (per-side intervals plus a volume) and
  becomes a new model reusing the common fields.
- Growth is a distinct measurement model — a timestamp plus typed values —
  needing no change to existing entries.
- Vaccines are a schedule plus dose records; likewise additive.

No v1 code ships for any of them.

## 13. Open follow-on projects

1. **Sync protocol** — wire contract: entity schema, change-log format, conflict
   resolution, auth. Written against the `ChangeLogEntry` shape defined here.
2. **Sync node** — Go or Rust, Docker-deployable, self-hostable, with a default
   hosted instance. Note that operating a hosted node holding infant health data
   carries obligations: authentication, encryption at rest, backups, a privacy
   policy, and GDPR deletion handling. End-to-end encryption, making the node a
   relay of opaque blobs, would remove most of that burden and should be
   evaluated first.
3. **Partner sharing UX** — invitations, household membership, per-device
   identity.

Each gets its own spec, plan, and implementation cycle.
