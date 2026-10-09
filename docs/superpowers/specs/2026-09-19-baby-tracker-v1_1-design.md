# Baby Tracker v1.1 — Design Spec

Date: 2026-09-19
Status: Draft for review
Supersedes parts of: `2026-09-18-baby-tracker-ios-design.md` (amendments marked)

## Purpose

v1 shipped 23 tasks and a working app. A whole-branch review returned
**ship-with-caveats**: one Critical, thirteen Important. v1.1 closes those, plus the
in-scope features that shipped with no reachable UI.

The governing constraint is unchanged and still binding: **logging happens one-handed,
in the dark, while holding a baby, often at 3am.** Every decision below is subordinate
to it.

## Scope

Four parts. Parts 1, 2 and 3 are independent of each other and may be built in any
order. Part 4 must run last, because its first step is a manual check whose outcome may
require a schema change (see Part 4).

| Part | What | Why now |
|---|---|---|
| 1 | Observability and correctness | The app cannot currently tell you anything failed |
| 2 | Feeding flow | The spec's tap-count requirement is not met, and two in-scope features are unreachable |
| 3 | Settings, babies, diaper detail | Three spec promises with no UI behind them |
| 4 | Widget and Live Activity verification | Three findings only a human can confirm |

Still out of scope, unchanged: pumping, growth, vaccines, Apple Watch, notifications,
and the sync server (v1 builds only the seam).

---

# Part 1 — Observability and correctness

## 1.1 The problem

Three symptoms, one cause: nothing in the app reports failure.

- `AppEnvironment.swift:23` uses `try!` on container creation. A corrupt store or a
  failed future migration becomes a crash loop on **every launch**, with no message and
  no escape. The only user-accessible recovery is deleting the app, destroying every
  entry ever logged. It cannot fire today — which is exactly why it would ship unnoticed
  and fire on the first schema change.
- **Twelve write paths swallow failures** via `try?`. Tap "Wet", the sheet dismisses,
  the entry does not exist, nothing says so. `record()` correctly rolls back, and the
  rollback is then discarded into `_ = try?`.
- **No logging anywhere.** `grep` for `OSLog`/`Logger` over the whole tree returns
  nothing. A failure in the field produces no alert and no log line.

## 1.2 Error surface

One `AppError` value on `AppEnvironment`, surfaced once at `RootView`. Two presentations,
chosen by severity:

**Recoverable failure (a save, a delete, an edit): a dismissible banner.** Not a modal.
An alert you must tap through at 3am, while the feed you were logging is still happening,
is hostile — and the failure is already recorded in the log for later. The banner appears at the top of the
current screen, above the content and below the navigation bar, and does not block
interaction. It states what failed in the user's terms ("Couldn't save that diaper
change") and stays until dismissed or replaced by a newer error.

**Unrecoverable failure (the store will not open): a full screen.** Nothing else in the
app works, so there is nothing to return to. The screen states what happened, and offers
the only two honest actions: retry, and a way to reach the data (see 1.3).

Copy rule, inherited from the spec's Insights constraint: **error text is factual, never
evaluative, and never blames the user.** "Couldn't save that diaper change" — not
"Invalid entry" and not "You must select a baby first".

## 1.3 Removing `try!`

`AppEnvironment.init` stops trapping. If the container cannot open, the app enters a
degraded state that renders the unrecoverable-failure screen rather than crashing.

The degraded state must not silently discard data. The on-disk store file is left
untouched — no automatic reset, no "clear and continue". A future export path is out of
scope here, but the failure screen must not offer any action that deletes the store.

## 1.4 Logging

`OSLog`, subsystem `com.olhapi.babytracker`, one category per module: `store`, `timers`,
`sync`, `widgets`, `ui`.

What to log: every mutation (kind and id), every rollback, every caught error with its
context, container open/failure, and Live Activity lifecycle transitions. Rollbacks
especially — a rollback that happens silently is precisely the failure we could not
explain afterward.

**Privacy rule, binding: entry *contents* never appear in logs.** Ids, kinds, and counts
only. Never volumes, never feed or sleep times, never notes, never a baby's name. This is
infant health data and logs get pasted into bug reports and support threads. A test
asserts that the logging helpers reject content-bearing fields.

## 1.5 Refresh

`HistoryView` and `InsightsView` currently render once from `.task` and never update. Log
a bottle on Home, switch to History, and it is absent.

Both adopt the pattern `HomeView` already uses: observe `store.revision`, and refresh on
`scenePhase` becoming active. This is six lines each.

Additionally, `BabyStore.revision` bumps on remote-store-change notifications, so a write
made by the widget or an App Intent — which run against their own `ModelContainer` —
propagates into a running app. The scene-phase hook alone only covers returning to the
app, which leaves the fresh-install case stale indefinitely.

---

# Part 2 — Feeding flow

## 2.1 Amendment to the v1 spec

This part **amends** `2026-09-18-baby-tracker-ios-design.md` §6 and §7. That spec
specifies three dock buttons and a fixed tap-count table. Both change. The amendment is
recorded here rather than contradicted silently.

## 2.2 The problem

The spec requires two taps for a breast feed and two for a bottle, and calls tap counts
"requirements, not aspirations". Both are currently **three**.

The cause is that "Feed" cannot start anything, because it does not know whether you mean
breast, bottle, or solids. Sleep and diaper are already two taps precisely because they
are unambiguous. The disambiguation tap is the entire cost.

A menu with everything pre-selected does **not** fix this. Pre-selection removes a
decision, but a menu still needs a confirming tap, so the count stays at three. The
pre-selection has to move onto the dock button itself.

## 2.3 The self-describing Feed button

The Feed button does what you did last time, and **says so on its face**.

| Last feed | Button reads | One tap does | Total taps |
|---|---|---|---|
| Breast | `Breast · Left` | Starts the timer on the opposite side | 2 (start, Stop) |
| Bottle | `Bottle · 120 ml` | Opens the bottle sheet, prefilled | 2 (open, Save) |
| Solids | `Solids` | Opens the solids sheet, prefilled | 2 (open, Save) |
| No history | `Breast · Left` | Starts a breast timer on the left | 2 |

The side shown is the side opposite the last breast feed — the existing `suggestedSide`
already computes this; it simply is not wired to the button.

**Why a behaviour-changing button is acceptable here, when it normally is not:** the
button states its outcome before you commit. Predictability comes from the label, not
from the behaviour being fixed. Nothing is hidden and nothing is guessed.

"Prefilled" means the existing `LogDefaults` behaviour, unchanged: volume defaults to
the *mode* of recent volumes (volume is habitual), contents to the *most recent* (it
changes more often). Those two rules are deliberately different and must not be swapped.

**Bottle does not log instantly.** Volume varies per feed, so a single tap that silently
records "your usual amount" would frequently record a number the user never looked at,
correctable only through History. The prefilled sheet keeps it at two taps while keeping
the number visible.

## 2.4 The override menu

The Feed button is a split control: a large primary area that performs the labelled
action, and a narrower chevron that opens the feeding menu (Breast / Bottle / Solids).
Both regions are at least 44pt; the primary area stays substantially larger.

A visible chevron is chosen over a long-press or a swipe. Both gestures are
undiscoverable, and a timed press is awkward one-handed while holding a baby.

The menu is the only place solids appear. Solids are a few times a day at most and only
after roughly six months, so they do not earn a permanent dock slot — but they are in
scope, are fully modelled (`FeedKind.solids`, `foods`, `amount`), and are currently
unreachable, which also makes `FeedStartSheet`'s own doc comment untrue.

## 2.5 Combo feeds

While a breast feed is running, the running screen offers **Add bottle**, which opens the
volume sheet and attaches the result to the in-progress feed.

**Adding a bottle does not end the feed.** The breast timer keeps running; the volume
is attached to the in-progress feed and written when the feed is stopped. A parent who
tops up mid-feed has not finished feeding.

That is exactly a combination feed — a `FeedEntry` carrying both `sides` and `volumeML`.
The v1 spec makes combo feeds a headline model decision (§5) and `FeedEntry.isCombo`
already exists, but no verb produces one, so both the model surface and the History
rendering are dead. This makes them reachable without a new concept.

## 2.6 Consequence for a parked defect

`HistoryViewModel` renders `"—"` for a feed with neither sides nor volume. That branch
was parked as unreachable because nothing could create a solids feed. **Shipping solids
makes it live.** It must render the solids detail (foods, amount) instead.

---

# Part 3 — Settings, babies, diaper detail

## 3.1 Settings screen

There is no settings screen. `AppEnvironment.volumeUnit` is declared, read in five
places, and never assigned — so the ml/oz preference the v1 spec §11 promises is dead
code, and every ounce-handling path is unreachable.

A settings screen adds: the ml/oz toggle (persisted to the App Group defaults, so it
survives relaunch and is readable by the extension), and the baby management below.
Note that no widget currently renders a volume, so this is about durability and future
consumers, not a present-day widget bug.

## 3.2 Baby rename and delete

Babies can currently be added and switched, never edited or removed. A typo in a name is
permanent.

Rename edits `name` and `birthDate` through `store.update`. Delete is a **tombstone**
(`softDelete`), never a row removal, behind a confirmation naming the baby — it is
destructive and hides every entry belonging to that child.

`SelectedBabyStore.resolve(from:)` already falls back to the first baby when the selection
is stale, so deleting the selected baby is safe.

## 3.3 Diaper detail

`DiaperEntry.consistency` and `.color` are declared and have no reader or writer. The v1
spec §7 specifies them "behind a collapsed 'add detail' disclosure".

They appear in the diaper sheet and the edit sheet, collapsed by default. **The two-tap
fast path is untouched** — the disclosure is never on the critical path.

---

# Part 4 — Widget and Live Activity verification

This part runs **last**, and begins with a manual check no agent in this environment can
perform: there is no tap API and device access is off. Nobody has ever placed a widget,
tapped a widget button, or seen a Live Activity render.

The check: place both widgets, tap "Log diaper", confirm it appears in History; start a
feed, lock the screen, confirm the Live Activity renders and counts; tap Stop, confirm
the feed ends and is saved.

It gates three findings that can currently only be reasoned about from source:

- **Stop acts on the selected baby, not the timer's baby.** `TimerActivityAttributes`
  carries `babyName` but no `babyID`, and the intents resolve the target via
  `WidgetStore.currentBaby(in:)`. With two babies this can write a feed to the wrong
  child. The likely fix adds `babyID` to the attributes — a schema change, which is why
  this part is sequenced last.
- **A stale Live Activity's "Stop" can start a sleep.** It is bound to
  `ToggleSleepIntent`, which starts a sleep when none is running, producing a fabricated
  multi-hour nap.
- **The status widget renders placeholder data as real.** On a fresh install or any store
  read failure it shows "2h 14m" and "Awake 40m" — numbers never logged. It must render
  em-dashes for the no-data case.

---

# Testing

Unchanged conventions: Swift Testing; view-model-level tests for everything renderable;
the package builds for macOS so tests run without a simulator.

Specific requirements for this release:

- **Tap counts are asserted, not assumed.** The v1 UI suite's harness retries taps and
  returns early if the target already exists, so its "two-tap" tests assert reachability
  rather than count. The new tests must count actual taps for breast, bottle, sleep and
  diaper.
- Every copy string added here is asserted in a view-model test. The v1 read-through
  found 13 defects; strings in views are untestable by construction.
- The logging privacy rule (1.4) is asserted by test.
- Error paths are tested by injecting a failing store, not by reasoning.
- **CI asserts the literal package-test total**, not merely exit 0. A confirmed
  intermittent `BabyStoreTests` SIGSEGV has been observed dropping the entire 31-test
  suite from a run that exited 0 with no crash line — and that suite holds the change-log,
  tombstone and timer invariants.

# Open item carried forward

The `BabyStoreTests` SIGSEGV is real, intermittent, and undiagnosed. It is not caused by
any v1 change. It is not scheduled here; the CI total assertion above is the guardrail
until someone investigates it.
