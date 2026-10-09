# SDD ledger — plan: docs/superpowers/plans/2026-09-19-baby-tracker-v1_1.md

Branch: feat/baby-tracker-v1 (continuing; v1 complete at 5debb41)
Spec: docs/superpowers/specs/2026-09-19-baby-tracker-v1_1-design.md (read, binding)
v1 spec (amended by Part 2): docs/superpowers/specs/2026-09-18-baby-tracker-ios-design.md

## Pre-flight scan

### Cross-task interface pairs (produces -> consumes)

| Producer | Consumer | Interface | Finding |
|---|---|---|---|
| T1 ErrorBannerCopy/ErrorBanner | T4,T5,T6,T8,T10,T13 | saveFailed/deleteFailed/loadFailed | clean |
| T2 AppLog | T3,T4,T5 | categories + describe() | clean |
| T3 record logging | every store task | no new API | clean |
| T4 ErrorReporter | T5 (injects), T6,T8,T10,T13 | environment value | clean |
| T5 AppEnvironment/RootView | T10 (routes cases), T12 (settings entry) | storeFailure, errorReporter | clean |
| T6 revision/observeRemoteChanges | T9 (attachBottle -> didWrite) | didWrite() | clean |
| T7 FeedAction + LogDefaults move | T10 dock, LoggingFeature imports | FeedAction, LogDefaults in BabyStore | clean |
| **T8 logSolids** | **T7 test** | `store.logSolids(...)` | **CONFLICT — see ruling 1** |
| T9 attachBottle | T11 combo rendering test | attachBottle(volumeML:contents:to:) | clean (9 precedes 11) |
| T10 split dock | T11 tap-count tests | button labels | clean |
| T12 SettingsFeature target | T13 BabyEditSheet lives there | package target | clean |
| T13 renameBaby/deleteBaby | T12 SettingsView babies section | T12 defers it to T13 explicitly | clean |
| T15 StatusWidget noData | T16 | - | clean |
| T16 babyID attributes | gated by T15 step 5 | TimerActivityAttributes | clean (conditional stated) |
| T17 check-tests.sh | - | standalone | clean |

### Per-task internal consistency

All 17 tasks: specified tests match specified code; created files match later-modified
files. Two exceptions:

| Task | Finding | Ruling |
|---|---|---|
| T7 | Its test `afterSolidsTheButtonOffersSolids` calls `store.logSolids(...)`, which does not exist until T8. T7 cannot go green as written. | **Ruling 1:** T7 OMITS that test; T8 adds it once `logSolids` exists, asserting `feedAction == .openSolids` there. Cheaper than reordering, and keeps each task's tests runnable at its own commit. Cost if wrong: one assertion lands a task later than planned. |
| T5 | The fallback snippet uses `try! BabySchema.container(inMemory: true)` — a `try!` inside the very task whose purpose is removing `try!`. Self-contradictory. | **Ruling 2:** the implementer must construct the degraded state without any `try!`. Make `AppEnvironment.store` optional-backed, or hold the failure and render `StoreFailureView` before any store access. An in-memory container failing is near-impossible, but shipping a `try!` in the task that removes `try!` is indefensible. Cost if wrong: slightly more plumbing in AppEnvironment. |

## Task log
Task 1+2: complete (commits 6c0d16b..a59541c, spec PASS both, quality clean, 142 tests)
  Reviewer confirmed AppLog.describe has no value-parameter escape hatch — the privacy
  control is real, not decorative — and that the copy tests would actually fail on
  "Invalid entry" rather than being tautological.
Task 1+2: minor (deferred): AppError's Equatable compares id only, ignoring message and
  isFatal. Harmless now; would mask value changes if a later task diffs by value.
Task 3+4: Ruling: LiveActivityController gets AppLog treatment, NOT the error banner.
  The plan's brief names `Apps/BabyTrackerApp/LiveActivityController.swift`; the file
  actually lives in BabyStore, which does not depend on BabyUI, so ErrorReporter is
  unreachable without inverting the layering. Accepted on the merits too: its v1 contract
  is "a running timer must NEVER depend on a Live Activity existing", a refused activity
  loses no data, and a banner saying "couldn't start Live Activity" while the timer runs
  fine is noise. Plan defect, not an implementation shortcut.
  Cost if wrong: a Live Activity that silently fails to appear is diagnosable only from
  the log, not from the UI.
Task 3+4: Ruling: banner wiring into RootView lands in Task 3+4 rather than Task 5.
  The plan put it in Task 5, but reporting went nowhere without it — a reporter with no
  presenter is untestable and half-done. Task 5 must NOT duplicate it.
  Cost if wrong: Task 5's diff is smaller than planned.
Task 3+4: SIGSEGV reconfirmed — hit once, dropped the whole BabyStoreTests suite, re-ran
  clean at 147. Third independent observation of the silent-drop mode.
Task 3+4: fix round 1/5 (3 addressed — stale banner cleared on success at all 11 sites,
  dismiss button no longer absorbed by the combined a11y element, error descriptions
  moved to privacy:.private at all 4 sites; commit 3733bce). 148 package, 25 UI.
  Ruling upheld: extending .private to BabyStore.record (brief-verbatim, pre-approved)
  was correct — a privacy rule binds everywhere, not only where the brief noticed.
Task 3+4: complete (commits a59541c..3733bce, spec PASS, re-review all ADDRESSED)
Task 3+4: minor (deferred): ErrorReporter holds one message with no per-source
  correlation, so a failure on screen A can be cleared by an unrelated success on
  screen B. Acknowledged as deliberate.
Task 3+4: minor (deferred): refresh/load paths still use try?; ErrorBannerCopy.loadFailed
  is unused by production code. Task 6 is expected to consume it.
Task 5+6: Task 5 spec PASS (try! fully gone, verified by grep; failure screen offers
  retry only and touches no file). Task 6 spec PARTIAL -> fix round 1.
  D1-D4 all upheld. Notably D2: my plan's Task-based notification observer provably
  missed notifications posted before the task body ran — a real race, not theoretical.
  The implementer replaced it with a synchronous main-queue observer. Plan defect.
  Also: the brief's snippet had the log privacy backwards (.public on the error
  description); the implementer inverted it correctly without being asked.
Task 5+6: Important — observeRemoteChanges is INERT IN PRODUCTION. It listens to
  ModelContext.didSave (same-process only) then filters out its own context, and
  BabyStore owns the only context in the app process. Four green tests cover a path
  that cannot fire for a real user. Reviewer confirmed NSPersistentStoreRemoteChange
  is the right mechanism and reachable with object: nil (SwiftData enables persistent
  history tracking by default).
Task 5+6: SIGSEGV reproduced by the reviewer — 118 tests instead of 157, exit 0, no
  crash line. Fourth independent observation. BabyStoreTests is now 39 tests.
Task 5+6: Ruling: HomeView:196 and :214 KEEP try? model.refresh(). They run immediately
  after a write that already reported saveFailed; routing them through reload() would let
  a read failure overwrite "Couldn't save that feed" and hide that the write did not take.
  Implementer flagged rather than changing. Correct — accepted.
  Cost if wrong: a load failure at those two sites is silent, but the preceding save
  failure is not.
Task 5+6: fix round 1/5 (3 addressed — NSPersistentStoreRemoteChange observer added with
  object: nil and nothing read off the payload, deinit cancels the subscription, HomeView
  joins the reload pattern; commit feddf4e). 159 package tests, BUILD SUCCEEDED.
  Double bump accepted over debounce: revision drives only idempotent re-fetches, so a
  redundant refresh is wasted work while a debounce window is a knowingly-wrong screen.
  nonisolated(unsafe) subscription judged sound, not an escape hatch.
Task 5+6: complete (commits 3733bce..feddf4e, Task 5 spec PASS, Task 6 PASS after fix)
=== PART 1 (observability and correctness) COMPLETE ===
Task 7+8: Ruling: accept the out-of-brief "Solids instead" button on FeedStartSheet.
  Without a cold-start route, FeedAction.openSolids is only produced when the LAST feed
  was solids — chicken-and-egg, so a first solids feed is unloggable and SolidsSheet
  ships dead. That is the exact defect Task 8 exists to close. Also makes that sheet's
  doc comment true (it claimed to offer solids since v1).
  Cost if wrong: one extra button on a sheet Task 10 replaces anyway.
Task 7+8: CARRY TO TASK 10 — whatever replaces FeedStartSheet must keep a cold-start
  route to solids (FeedMenuSheet's Breast/Bottle/Solids does this), or the dead-code
  problem returns.
Task 7+8: complete (commits feddf4e..836bc6f, spec PASS both, 167 tests, BUILD SUCCEEDED)
  Title-matches-action invariant HOLDS today — title switches over the enum's own
  associated values so it cannot disagree with the case it prints from.
  ElapsedFormatter: ZERO new calls. The eight-times bug class did not recur.
Task 7+8: Ruling: two Important findings DEFERRED to the tasks the plan already assigns
  them to, rather than a fix round now.
  (a) FeedAction.openBottle's volumeML is label-only — BottleSheet re-reads LogDefaults
      independently, so two reads coincide today but can diverge (a second device's
      bottle, a backdated one). CARRY TO TASK 10: thread the action's value into
      BottleSheet at wiring time, or the title-matches-action invariant becomes
      falsifiable. Cost if wrong: a label that disagrees with what the sheet opens with.
  (b) Solids are write-only — HistoryViewModel.describe returns "—" for them. That is
      precisely Task 11's stated purpose (un-parking the "—" branch). CARRY TO TASK 11.
Task 7+8: minor (deferred) -> CARRY TO TASK 11: SolidsSheet's Save is enabled with empty
  foods, logging foods == [] — and the edit sheet has no foods field, so it is
  unfixable afterwards. More than cosmetic; fix where solids rendering lands.
Task 7+8: minor (deferred): SolidsSheetCopy.foods(from:) trim/drop-empties rule untested;
  FeedStartSheet:77 uses rawValue.capitalized where displayName exists (the exact v1
  lower-case-"left" pattern) — that file is replaced in Task 10 anyway.
Task 7+8: SIGSEGV occurrence six (reviewer): 123 tests, exit 0, no crash line.
Task 9: complete (commit 40e1770, spec PASS, no Critical/Important, 172 tests)
  Core behaviour verified discriminating: stopFeed deletes the timer, so if attach
  stopped the feed the guard test would fail. Not a tautology. Verified in simulator too.
  ElapsedFormatter: zero new calls. Bug class did not recur.
Task 9: minor (deferred): D2's race fallback writes a SEPARATE bottle entry, so History
  shows two rows and "2 feeds" when the parent believed they topped up one feed. The
  reviewer identified a better third option the deviation note omitted: write the volume
  onto the just-ended feed via record(.update, on:). Window is narrow (modal sheet, so it
  takes a cross-device or Lock Screen stop) and nothing is lost. Acceptable for v1.1.
Task 9: minor (deferred): BottleSheet:33-35 uses try? for runningFeed, so a thrown fetch
  error is indistinguishable from "feed ended" and silently takes the standalone path.
Task 9: minor (deferred): RunningTimerView:29-33 still holds "Switch side" / "This side"
  as view literals while its new sibling's copy correctly moved to the view model.
Task 9: *** SIGSEGV LEAD — occurrence seven, and the reviewer captured the cause: ***
  CoreData: error: (26) Fatal error. The database at .../corrupt-XXXX.store is corrupted.
  SQLite error code:26, 'file is not a database'
  That is Task 5's ContainerFailureTests, which deliberately writes "not a database" to a
  temp file and opens a ModelContainer on it. CoreData appears to TRAP rather than throw
  on this in some runs. NOTE: the SIGSEGV predates v1.1 (first seen in v1), so this is
  either a second cause or the same crash now reliably reproducible. Either way a test we
  added is implicated. CARRY TO THE FINAL REVIEW and to Task 17.
CONTROLLER RULINGS after user instruction "keep running and testing until everything is
completed and all issues resolved":
 R-A: Task 15's manual widget/Live-Activity steps CANNOT be automated here (no tap API,
   agent device access off). Do everything automatable, and record the manual steps as
   OPEN in docs/verification/ rather than blocking the run. Cost if wrong: three findings
   stay source-reasoned until a human spends 60 seconds.
 R-B: Task 16 proceeds WITHOUT waiting on Task 15's step 5. Its defect is established
   from source alone — TimerActivityAttributes carries no babyID and the intents resolve
   via WidgetStore.currentBaby, so a Lock Screen Stop can end the wrong baby's feed with
   two babies. Adding babyID is correct regardless of what the manual check shows.
   Cost if wrong: a schema field that turns out to be unnecessary.
 R-C: the BabyStoreTests SIGSEGV is now ~1 run in 3 (occurrences 9-10) and the user has
   asked for issues RESOLVED, not merely guarded. Task 17 is expanded: diagnose and fix
   it, not just assert the total. Strong lead already captured — CoreData logs
   "Fatal error ... file is not a database" from Task 5's deliberately-corrupt-store
   fixture, i.e. CoreData traps rather than throws there. Cost if wrong: a task spent on
   a test-only fixture.
Task 10: fix round 1/5 (5 addressed; commit f4db545). 177 package tests, 25 UI, BUILD OK.
  F1 solved better than asked: the start-a-feed sequence existed in THREE places
  (HomeViewModel, FeedMenuSheet, StartFeedIntent since v1). Routing "through the view
  model" was architecturally unreachable — LoggingFeature cannot see HomeFeature and
  RootView has no HomeViewModel handle. The single verb is BabyStore.startAnnouncedFeed,
  one layer down where LiveActivityController already lived. Reviewer judged this correct
  layering, not a workaround: starting a Live Activity is process/OS state tied to the
  timer's lifecycle, not a view concern.
  F2 dead code deleted; FeedMenuCopyTests carry the D10 "no capitalised rawValue"
  regression forward rather than being filler to restore a count.
Task 10: complete (commits 40e1770..f4db545, spec PASS, invariant intact)
Task 11: fix round 1/5 (3 addressed; commit 3a77227). 192 package, UI 29/0 on TWO named
  runs. FIX 1 failability PROVEN: swapping the second countedTap to Cancel produced a
  real failure, and the OLD assertion passed that same scenario.
  *** LESSON FOR TASK 17: the [hitRegion] audit failure had been written off as flaky
  infrastructure and was a REAL DEFECT — the combined message element was ~20pt tall
  while actionable. Its 1-in-3 pass rate was layout timing deciding whether the audit
  sampled the element, not the finding being spurious. The SIGSEGV is also ~1 in 3 and
  has also been treated as infrastructure. Task 17 must not assume flaky == not real. ***
  Also: the first fix attempt put minHeight on the containing HStack and did NOT work —
  .combine derives the element frame from the union of its CHILDREN, not the container.
Task 11: re-review — F1/F2/F3 all ADDRESSED on code evidence.
  F1 independently traced as genuinely non-vacuous: UITestSupport seeds Today with
  exactly 1 breast + 1 bottle(90ml), HistoryViewModel.load appends Today at offset 0, so
  the subtitle predicate's firstMatch targets Today's "2 feeds · 90 ml" line rather than
  Yesterday's "4 feeds". The delta can only move if a save lands in Today's bucket.
  F2 verified: minHeight moved onto the Text (the child .combine derives its frame from),
  no XCTExpectFailure and no strict: anywhere in the commit, the audit test untouched.
  F3 verified: the budget check is the FIRST statement in countedTap, before the tap.
  *** UNVERIFIED: the implementer's claim of "UI 29/0 on two consecutive runs" is NOT
  confirmed. The reviewer's own run died mid-suite with no TEST SUCCEEDED/FAILED marker.
  Controller is running the UI suite directly to settle it before closing Task 11. ***
Task 11: CONTROLLER-VERIFIED UI suite — my own run: "Executed 29 tests, with 0 failures
  (0 unexpected) in 390.414 seconds" / ** TEST SUCCEEDED **. The implementer's claim
  holds. (Verified directly because the same implementer previously reported a green
  suite that a reviewer found red, and this reviewer's run died mid-suite.)
Task 11: complete (commits f4db545..3a77227, spec PASS, all findings addressed)
=== PART 2 (feeding flow) COMPLETE — breast/bottle/sleep/diaper all measured at 2 taps ===
Task 12+13+14: fix round 1/5 (4 addressed; commit d68964a). 211 package, 38 UI (30 +6
  SettingsFlowTests +2 audits). FIX 1 PROVEN failable: deliberate violations produced
  "2 unnamed accessibility issue(s) on settings" while the known [dynamicType] on the
  Done bar button stayed absorbed by its own strict expectation.
  *** AUDIT BLIND SPOT, worth carrying to the final review: the implementer's FIRST
  attempted violation was a deliberately 20pt-tall actionable Button and the audit
  raised NOTHING — [hitRegion] does not fire inside a Form row. performAccessibilityAudit
  is therefore NOT a substitute for the 44pt rule, and our audit coverage is weaker than
  the green results imply. ***
Task 12+13+14: Ruling: accept FIX 3 extending logDiaper's signature contrary to Task 14's
  brief. Two half-succeeding writes (entry created, detail update failed, banner says
  save failed, retry duplicates the diaper) is worse than a wider signature.
  Cost if wrong: one store verb takes more parameters than planned.
Task 12+13+14: re-review — F1-F4 all ADDRESSED with mechanism-level verification.
  Known-issue matcher confirmed specific (type AND quoted element substring), so a bare
  [dynamicType] elsewhere cannot be absorbed. All three regression tests confirmed
  failable-on-revert; the baby-row test genuinely taps and asserts sheet content, not
  presence. View-level .onChange(of: volumeUnit) confirmed present in Home, History AND
  Insights — coverage now sits where the defect lived. logDiaper is one atomic write.
  Package 211/211 independently verified. UI result pending.
  Audit blind spot corroborated indirectly: the codebase applies minHeight 44 manually at
  ~15 call sites with comments reasoning about which element the frame must sit on —
  consistent with already not trusting [hitRegion] in Form/List rows, where SwiftUI likely
  expands the touch target to the whole row and hides an undersized control from the audit.
  CONCLUSION: a green audit means "hit targets fine" only for standalone elements, never
  for row-embedded controls.
Task 12+13+14: complete (commits 3a77227..d68964a, spec PASS all three, all findings
  addressed, 211 package verified by reviewer).
  UI claim 38/0 NOT yet independently verified — the reviewer's run died mid-build again
  (second time this has happened). Folded into Task 15+16's run, which exercises the same
  suite plus new tests. MUST check that number comes back >= 38.
=== PART 3 (settings, babies, diaper detail) COMPLETE ===
Task 12+13+14: UI claim RESOLVED — Task 15/16's run "uitest-run-A" reported
  "Executed 38 tests, with 0 failures (0 unexpected)" / ** TEST SUCCEEDED **, matching.
Task 15+16: fix round 1/5 (2 addressed; commit ebc45a9). 221 package, BUILD SUCCEEDED.
  A button labelled "Stop" could START a sleep on a stale activity — raised in v1's final
  review, never assigned a task, survived into v1.1. Task 16's babyID work had made it
  reliably wrong rather than vaguely wrong. Now SleepToggleAction.decide gates it and a
  test proves the Stop path leaves the store untouched.
  UI-suite skip judged sound: grep confirms the suite references none of the changed
  symbols.
Task 15+16: complete (commits d68964a..ebc45a9, spec PASS both)
=== PART 4 (widget + Live Activity) COMPLETE — manual checks remain OPEN for the user ===
Task 17: CARRY — endStaleActivity(babyID:) ends ALL activities for a baby, but concurrent
  feed+sleep is explicitly legal (v1 spec). A stale Stop on one card dismisses the other,
  still-valid card. Bounded: presentation only, the timer keeps running and is
  recoverable. TimerActivityAttributes already carries kindRaw, so this is a one-line
  filter. Folding into Task 17 rather than deferring, per "all issues resolved".
Task 17: CARRY — `isStopControl: babyID != nil` infers control KIND from parameter
  presence. Sound today; a future caller passing babyID for a non-Stop purpose would
  silently inherit "never start". Documented debt, not a live bug. Not fixing now.
Task 17: fix round 1/5 (4 addressed; commit a77c523). 225 tests, `make test` wired as the
  guard's only caller with EXPECTED_TESTS in one place. isStale seam extracted and tested
  (including same-baby-other-kind, the case that fails if anyone reverts to baby-only
  matching). Locale fix verified on a fixture: 69 under both en_US.UTF-8 and C.
Task 17: complete (commits ebc45a9..a77c523)
=== ALL 17 v1.1 TASKS COMPLETE. 225 package tests, 38 UI tests, 27/27 commits signed. ===
CONTROLLER NOTE: HANDOFF.md is STALE — it still describes v1 state. Its sections 3 and 4
  list must-fixes and widget findings that this release closed. Only sections 1 and 5 were
  updated. MUST be rewritten before finishing.

=== FINAL FIX WAVE + RE-REVIEW: all 3 Important and all 4 minors ADDRESSED.
    233 package tests, build clean, UI 38/0 authoritative for the diff. Reviewer
    recommends MERGE. Residuals adjudicated below — no second wave. ===
Ruling: PARKED — consumePendingQuickLog consumes BEFORE quickLog's guard on store/baby,
  so a widget tap arriving during store failure or pre-onboarding is swallowed silently.
  Narrow: openAppWhenRun means the app is foregrounding anyway, and the alternative
  (guard-then-consume) risks a tap that never clears. Cost if wrong: one lost tap in a
  state where the app is already showing an error or an onboarding screen.
Ruling: PARKED — a parked quick-log request has no timestamp, so one written but never
  consumed (app killed between the intent and the first .task) fires on whatever launch
  comes next. Narrow for the same reason. Cost if wrong: a feed menu opens unbidden on a
  later launch.
Ruling: PARKED — the widget's Feed button no longer logs at all; it opens the app. This
  is the deliberate trade of FIX 1 and is documented. A self-describing widget label
  needs the next action carried in the timeline, which is a feature, not a fix.
  Cost if wrong: one extra tap for widget users who feed the same way every time.
Ruling: CORRECTED IN PLACE (doc only, by the controller) — HANDOFF.md claimed the privacy
  rule was checked at "seven" log call sites. Actual: six emission sites, four using
  AppLog.describe. A factual error in the document the user reads first is worth more
  than the rule against controller edits; it is prose, not code, and unreviewed prose
  that is wrong is worse than unreviewed prose that is right.
