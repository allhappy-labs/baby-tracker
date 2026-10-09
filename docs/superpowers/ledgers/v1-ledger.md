# SDD ledger — plan: docs/superpowers/plans/2026-09-18-baby-tracker-ios.md

Branch: feat/baby-tracker-v1
Spec: docs/superpowers/specs/2026-09-18-baby-tracker-ios-design.md (read, binding authority)

Ruling: isolated via feature branch in place, not a git worktree — EnterWorktree is
gated to explicit user/CLAUDE.md request (neither present), and the repo has no remote
for its default `fresh` base ref. Cost if wrong: work lands on a branch rather than a
separate checkout; trivially rebased.

## Pre-flight scan

### Cross-task interface pairs (produces -> consumes)

| Producer | Consumer | Interface | Finding |
|---|---|---|---|
| T1 DateProvider | T7, T9, T19, T20 | `DateProvider.now`, TestDateProvider.advance | clean |
| T2 enums | T6, T9, T13, T15, T23 | raw-valued enums, SideInterval | clean |
| T3 Volume/ElapsedFormatter | T13, T16, T17, T20 | formatted(as:), compact, timer | clean |
| T4 DayBucket | T5, T10, T16, T17 | key/bounds public; `calendar(for:)` internal | clean — T5 is same module (BabyCore) |
| T5 SleepClassifier | T6 SleepEntry.kind | classify(startedAt:timeZoneID:) | clean |
| T6 models | T7 ChangeLoggable ext | stored raw columns | clean |
| T7 record/BabyStore | T8, T9, T10 | `record(_:on: some ChangeLoggable)` | clean |
| T8 Touchable/update/softDelete | T16, T23 | `some ChangeLoggable & Touchable` | clean after plan fix |
| T9 timers | T10, T14, T19, T20, T21 | startFeed/stopFeed/switchSide/activeTimer | clean |
| T10 HomeSnapshot/HistoryItem | T13, T16, T17, T20 | public enum cases, public lets | clean |
| T11 BabySync | nothing in v1 | exercised by own tests only | clean — intentional |
| T12 Palette/Tracker/GlassDock | T13-T18, T20, T21 | color/symbolName/title, numeralStyle | clean |
| T13 HomeViewModel | T14 views | HomeMode/IdleState/RunningState/StatusRow | clean |
| T14 HomeView | T19 RootView | (store:babyID:babyName:volumeUnit:onQuickLog:onSwitchBaby:) | clean — call site matches |
| T15 LogSheetView | T19 | (sheet:store:babyID:unit:onDone:onSwitchTo:) | clean |
| T16 HistoryView | T19, T23 | (store:babyID:volumeUnit:) | NOTED — T23 requires storing store/volumeUnit; T23 states this explicitly |
| T17 InsightsView | T19 | (store:babyID:volumeUnit:) | clean |
| T18 SelectedBabyStore/sheets | T19, T20 | select/resolve; AddBabySheet onCancel defaulted | clean |
| T19 deviceID in app-group defaults | T20 WidgetStore | key "deviceID" | clean — same literal both sides |
| T20 WidgetBundle + intents | T21 | bundle excludes FeedActivityWidget until T21 | clean after plan fix |
| T21 StopFeedIntent | T20 LogIntents.swift | appended to existing file | clean |
| T14/T15 a11y labels | T22 XCUITests | "Log feed/sleep/diaper", "Stop feed/sleep", "Wet", "Left", "Bottle instead", "Save" | clean — every queried label is produced |

### Per-task internal consistency

All 23 tasks: tests specified match the code specified, and files created match files
later modified. Two exceptions found:

| Task | Finding | Ruling |
|---|---|---|
| T7 | Test code omits `@MainActor` on `@Test` funcs; Step 4 adds a prose note to add it. Tests as written will not compile against a `@MainActor` BabyStore. | Ruling: implementer adds `@MainActor` to every `@Test` in ChangeLogTests per the Step 4 note. Carried in T7 dispatch. Cost if wrong: one compile error the implementer fixes anyway. |
| T21 | `stopIntent(for:) -> any AppIntent` fed to `Button(intent:)`, which requires a concrete AppIntent type, not an existential. Will not compile. | Ruling: replace the helper with an explicit if/else yielding concrete `ToggleSleepIntent()` / `StopFeedIntent()` buttons at each call site. Carried in T21 dispatch. Cost if wrong: none — the plan's version cannot compile. |

## Task log

Task 1-5: Ruling: Package.swift tools-version bumped 6.0 -> 6.2. Plan defect — the
`.v26` platform cases were introduced in PackageDescription 6.2, so the manifest as
written cannot resolve at all. `swiftLanguageModes: [.v6]` is unchanged, so the Swift 6
language mode and strict concurrency requirement are untouched (tools-version and
language mode are independent). Cost if wrong: none identified — the plan's version
fails to build; the alternative (dropping to .v18 platforms) would violate the spec's
iOS 26 floor.

Task 1-5: Ruling: implementer commits use `-c commit.gpgsign=false`. The configured ssh
signing key is Secure-Enclave/Keychain-backed and requires an interactive Touch ID
prompt that a non-interactive agent session cannot answer; `ssh-keygen -Y sign` fails
even though `ssh-add -l` lists the key. The four pre-existing doc commits on this branch
(1f103f3, 3efe90e, 5a80471, 9918b35) were made the same way and the user was told so.
Local-only, no side effect outside the repo, and fully reversible with:
  git rebase --exec 'git commit --amend --no-edit -S' main
Cost if wrong: the whole branch is unsigned until that rebase is run. SURFACE THIS AT
FINISH.
Task 1-5: complete (commits 9918b35..b946d27, spec PASS, 21/21 tests, 1 minor deferred)
Task 1-5: minor (deferred): BabyStore/BabySync placeholder tests assert a tautology
  (String(describing:) == its own name). Self-resolving: Task 7 replaces
  Tests/BabyStoreTests with ChangeLogTests, Task 11 replaces Tests/BabySyncTests with
  SyncClientTests. Carried into both dispatches as "delete the placeholder file".
Task 6: complete (commit 0d0b662, spec PASS, quality approved, 24/24 tests)
  App Group id confirmed: group.com.olhapi.babytracker (cross-check at Task 20)
  Placeholder source+test for BabyStore deleted as instructed.
Task 7: fix round 1/5 (1 addressed, 0 open — record() had no rollback, leaving orphaned
  pending inserts with no log entry; commits 5a07912..4e6b0e0)
Task 7: complete (commits 0d0b662..4e6b0e0, spec PASS, re-review ADDRESSED, 29/29 tests)
  Implementer discovery: JSONSerialization traps rather than throws on invalid values on
  this toolchain; payloadData() now pre-checks isValidJSONObject and throws
  InvalidChangeLogPayload.
Task 7: minor (deferred): context.rollback() discards ALL pending context changes, not
  just the current call's. Safe under the single-mutation-per-record convention. CARRIED
  INTO TASK 9 dispatch, where stopFeed does insert+delete+record in one sequence.
Task 8: Ruling: rename tombstone column `isDeleted` -> `isTombstoned` across Baby,
  FeedEntry, SleepEntry, DiaperEntry, Touchable, EntryPayload payload keys, and all
  later-task predicates. `isDeleted` is reserved by NSManagedObject (SwiftData's
  backing store): a @Model property of that name is silently reset to false on every
  save(), so tombstones never persisted. Found by the Task 8 implementer; I reproduced
  it independently (isDeleted -> false after save, isTombstoned -> true). This touches
  Tasks 6-7 committed code, which the Task 6 and 7 reviews both passed — the reviews
  checked the column existed and defaulted correctly, not that it survived a save.
  Plan file updated so all later briefs extract the correct name.
  Cost if wrong: a rename across 6 files; the alternative is a tombstone system that
  silently does nothing, breaking sync deletes forever.
Task 8: complete (commits 4e6b0e0..0c49409, spec PASS, quality PASS, 34/34 tests)
  Includes the authorized isDeleted->isTombstoned rename; reviewer confirmed complete
  (only surviving occurrence is the intentional warning comment).
Task 8: minor (deferred, non-issue): logSleep has no `at:` correction param. It takes
  explicit from/to, so backdating is inherent. No action.
Task 9: complete (commit d372c3f, spec PASS, quality PASS, 42/42 tests)
  Deviation accepted: `try side ?? suggestedSide(...)` replaces the brief's
  `side ?? (try ...)`, which does not compile under Swift 6 throw placement.
  Reviewer confirmed ?? still short-circuits, so suggestedSide is not eagerly evaluated.
Task 10: Ruling: test fixtures must build their base instant as NOON LOCAL time, not a
  raw epoch. `1_700_000_000` is 2023-11-14 23:13:20 UTC = 01:13 local in CEST, so the
  brief's own `advance(by: 3600)` crossed local midnight and split "today" across two
  days — the assertion failed for a correct implementation. Found by the Task 10
  implementer. Affects Tasks 10, 16, 17 (same fixture). Plan updated with a global
  constraint so later briefs carry the rule.
  Cost if wrong: none identified; a zone-dependent test is a flaky test regardless.
Task 10: Ruling: `HistoryItem` needs `@MainActor Identifiable` conformance — plain
  Identifiable crosses actor isolation and will not compile under Swift 6 strict
  concurrency. Accepted as a compile-necessity deviation.
Task 10+11: complete (commits 8dd23e7..84c2c98, spec PASS both, quality PASS, no findings,
  49/49 tests). Reviewer independently re-ran QueryTests under TZ=UTC, Pacific/Auckland,
  Pacific/Honolulu, Pacific/Kiritimati — all pass, confirming the noon-local fixture is
  genuinely zone-independent. BabySync confirmed free of BabyStore/SwiftData imports.
  Both BabySync placeholders deleted.
Task 12+13: complete (commits 1eaaca4..d8de8cd, spec PASS both, quality PASS, 58/58)
  GlassDock compiles on macOS without an #if os(iOS) gate — macOS test strategy intact.
  Plan text said "8 tests" for Task 13; the brief's own file defines 7. Plan counting
  error, no action — implementer flagged rather than inventing a test.
Task 14+15: 3 Important findings, entering fix loop.
Task 14: Ruling: stop control must move OUTSIDE the ScrollView. The plan's Task 14 sample
  code puts it inside scrolling content, but spec section 6 states "the quick-log dock is
  replaced by a solid, filled Stop button occupying the same thumb region, so the primary
  action is in one place in both states". Spec is the binding authority; the plan's sample
  is its (defective) argument. A stop button that can scroll off-screen defeats the
  one-handed-at-3am constraint the whole design is subordinate to.
  Cost if wrong: a layout change, easily reverted.
Task 14: Ruling: switchSide/stopPrimary branching logic must move from HomeView into
  HomeViewModel. Plan's sample put it in the view; spec section 9/10 requires views to
  have thin bodies and no logic precisely so logic stays testable. Task 14 has no unit
  tests by design, so logic in the view is untested logic.
  Cost if wrong: a small refactor; tests gained either way.
Task 14: Ruling: the SwiftData tie-break bug is systemic, not local. Fix lastFeed,
  lastSleep, lastDiaper and suggestedSide the same way as lastBottleContents, and cover
  the tie case with tests. Backdated entries (Task 23 edit sheet) make exact-timestamp
  ties reachable in normal use, and these four queries populate the Home screen.
  Cost if wrong: none identified; the current pattern is empirically wrong on ties.
Task 14+15: fix round 1/5 (3 addressed, 0 open — stop-control placement, view logic
  leak, systemic tie-break bug; commits 88f7055..16ce269)
Task 14+15: complete (commits d8de8cd..16ce269, spec PASS, re-review all ADDRESSED, 69/69)
Task 14: minor (deferred): `try?` at HomeView call sites silently swallows errors from
  the view-model calls. Pre-existing pattern across the views. FLAG TO FINAL REVIEW —
  a failed save currently shows the user nothing.
Task 16+17+18: complete (commits 16ce269..287d789, spec PASS all three, quality good,
  82/82 tests). Plan defect noted: Task 18 file list names BabyListViewModel.swift with
  no code/interface/test anywhere — leftover; correctly not fabricated.
Task 16: minor (deferred): try? swallows errors in HistoryView delete/load, same pattern
  as HomeView. FLAG TO FINAL REVIEW.
Task 19: Ruling: hand-author project.pbxproj using Xcode 16+ file-system-synchronized
  groups (objectVersion 77) rather than installing XcodeGen/Tuist. The plan says "create
  the project in Xcode", which assumes a GUI no agent can drive. Installing a project
  generator would be a side effect on the user's machine outside this repo, which is not
  mine to decide; hand-authoring keeps everything in-repo and reversible. Synchronized
  groups avoid enumerating every source file, which is where hand-written pbxproj
  usually goes wrong.
  Cost if wrong: a malformed pbxproj that Xcode rejects — visible immediately, and
  recoverable by deleting the file and retrying, since all real code lives in the package.
Task 19: complete-pending-fix (commit 1126db1, spec PASS, app builds + launches on
  iPhone 17 Pro simulator, screenshots captured, 82/82 package tests).
  Ad-hoc simulator signing was REQUIRED: CODE_SIGNING_ALLOWED=NO builds but crashes at
  launch because entitlements are not embedded and SwiftData traps on the missing App
  Group container. Task 20's widget target needs the same treatment.
  Note for Task 20: SwiftData's groupContainer store resolves to
  <group>/Library/Application Support/default.store, not the group root.
Task 19: Ruling: fix 3 user-visible copy bugs found by running the app (not by tests) —
  (A) "Today so far" sleep total renders "just now" because a DURATION TOTAL is formatted
  with the relative-time formatter; (B) StatusRowView appends "AGO" under "just now";
  (C) same "AGO" under the em-dash empty state. Bugs live in Task 13/14 files. Fixing
  now rather than deferring: this is the app's primary screen and the text is simply
  wrong. Cost if wrong: small copy/API change, covered by new tests.
Task 19: Ruling: History and Insights copy cannot be read via simctl (no tap API; agent
  device access is OFF for this environment, confirmed by device_list). Deferring the
  string read-through to Task 22, which MUST dump the full accessibility tree per screen
  (app.debugDescription) and assert on the copy-defect class found here. That is a text
  read-through, no image reading required. CARRIED INTO TASK 22 dispatch.
Task 19: fix round 2/5 (4 addressed, 0 open — same duration/age formatter bug in
  InsightsViewModel x2 and HistoryViewModel x2; StatusRow.isLive removed as dead;
  doc comments added to compact/total; commit 88e48ee)
Task 19: complete (commits 287d789..88e48ee, spec PASS, 96/96 tests, app builds+launches)
Task 19: minor (deferred): compact() now has exactly one caller. Single-caller public API
  invites the next wrong reuse; only a doc comment guards it. FLAG TO FINAL REVIEW.

=== SESSION PAUSED 2026-09-19 at user request. 19/23 tasks complete. ===
Remaining: Task 20 (widgets + App Intents), 21 (Live Activity), 22 (a11y + UI tests),
23 (entry editing), then the final whole-branch review.

=== GAP AUDIT 2026-09-19 (found outside the plan; user chose to finish Tasks 20-23 first) ===
Not defects in any task — holes the PLAN never covered. Carry to final review / backlog:
 G1. No settings UI at all. volumeUnit is hardcoded .millilitres in AppEnvironment with
     no way to change it, yet spec section 11 promises an ml/oz display preference.
 G2. A Baby can never be edited or deleted in the UI. Add + switch only. Baby.isTombstoned
     exists but nothing calls softDelete on it.
 G3. Solids cannot be logged. FeedKind.solids/foods/amount are modelled and in scope;
     FeedStartSheet offers only breast and bottle (its own doc comment claims otherwise).
 G4. Combo feeds cannot be created. Model supports sides+volume by deliberate design
     (spec section 5); no UI path produces one.
 G5. Entry `note` field is unreachable until Task 23 ships the edit sheet.
 G6. Logging/OSLog still not done (user asked for it 2026-09-19).
 G7. No user-facing error surface; try? swallows failures silently.
 G8. History and Insights have NEVER been rendered and read by anyone. Reading Home
     turned up 7 copy bugs; expect more here. Task 22 must dump their a11y trees.
Task 20: Ruling (REVERSING my earlier call): make ElapsedFormatter.compact take Dates
  (`compact(since:now:)`) instead of Int seconds, so a duration CANNOT be passed to the
  age formatter — the compiler prevents the mistake. I previously ruled a type-level fix
  disproportionate and said a doc comment would do. The bug has now recurred EIGHT times,
  including once in a report that claimed to have checked for it, and once caught only
  because I asked a reviewer for an exhaustive enumeration. A comment is evidently not a
  control. Cost if wrong: a mechanical refactor of ~6 call sites, all covered by tests.
Task 20: fix round 1/5 (2 addressed, 0 open — StatusWidget compact/total misuse, plus the
  structural signature change; commits 08876d4..52739e8)
Task 20: complete (commits 43edaa8..52739e8, spec PASS, re-review ADDRESSED, 106 tests)
  The new compact(since:now:) signature CAUGHT the outstanding bug by refusing to compile
  on it — validating the reversed ruling.
Task 20: Ruling: the reported "BabyStoreTests crashes ~1 run in 4 with signal 11" is NOT
  CONFIRMED. Implementer claimed 2/6; reviewer got 0/12 full + 0/6 filtered; I ran 40
  consecutive filtered runs: 0 crashes, 0 non-zero exits. 58 clean runs total. At a true
  1-in-4 rate, P(0 in 40) ~ 1e-5. Recorded, not chased. If it resurfaces, this baseline
  is the evidence to compare against.
Task 20: UNVERIFIED BEHAVIOURALLY: nobody has seen either widget render or tapped a
  widget button. App Group is verified structurally only (entitlement bytes match,
  .appex embedded, intents registered). Needs a 60-second manual check by the user.
Task 21: CRITICAL found by review — LiveActivityIntent runs in the APP process, but
  LogIntents.swift lives only in the widget extension's synchronized group. App bundle has
  no Metadata.appintents and does not link AppIntents (independently confirmed). Lock
  Screen Stop would be dead in users' hands while passing every check here. Also regresses
  Task 20's ToggleSleepIntent, which now prefers the app process too.
  Ruling: fix via a shared Apps/SharedIntents/ synchronized group listed in BOTH targets'
  fileSystemSynchronizedGroups — reviewer's approach, simpler than an iOS-only package
  target and avoids dragging #if os(iOS) through already-iOS-only code. Do NOT add the
  existing BabyTrackerWidgets group to the app target: its @main WidgetBundle would
  collide with the app's @main.
Task 21: fix round 1/5 (4 addressed, 0 open — Critical intents-unreachable-from-app, plus
  ToggleSleep regression, StartFeed asymmetry, fire-and-forget ending; b48c6db..932bee9)
Task 21: complete (commits 52739e8..932bee9, spec PASS, re-review all ADDRESSED, 97 tests)
  Built-product evidence: Metadata.appintents now in BOTH bundles with all 4 intents;
  app links AppIntents.framework; the "Metadata extraction skipped" warning that had been
  present since Task 20 is gone. LESSON: that warning was the bug announcing itself and
  was dismissed as noise for two tasks.
  My otool -L evidence was partially wrong (Debug code lives in .debug.dylib, not the
  stub); the missing Metadata.appintents was the sound signal.
Task 21: UNVERIFIED BEHAVIOURALLY: Activity.request has never executed. Fix proves the
  intents are RESOLVABLE, not that Stop works. Needs manual device/simulator check.
Task 22: UI tests + a11y audit landed (commit 3a7a2bb). 17/21 UI tests pass; the 4
  failures are accessibility audits left RED on purpose because the app is wrong.
  13 COPY DEFECTS found on History/Insights/Home — screens that had never been read.
  Vindicates routing the read-through here. Also surfaced: HomeView's 1 Hz Timer
  refreshes SwiftData forever even when idle (swallowed taps + battery), and
  UITestSupport.swift ships in the release binary.
Task 22: ADJUDICATED DEFECT LIST — 13/13 CONFIRMED, 0 rejected. Fix order by root cause:
  D4  Insights fixed /7 divisor (InsightsViewModel:44,54)  <- ROOT CAUSE of D2, D3, D5
  D2  "Not enough history yet · 7-day average 0" (InsightsView:29)
  D3  "2 more than usual · 7-day average 0" (InsightsViewModel:88)
  D5  "51.4 ml more than usual" over-precision (InsightsViewModel:125)
  D7  false "Nothing logged yet" when a filter matches nothing (HistoryView:57-65)
  D6  day-header totals ignore the filter (HistoryViewModel:62 allItems -> items)
  D1  "Awake … AGO" — suffix on a STATE row (HomeViewModel:149,155)
  D9  History drops bottle contents (HistoryViewModel.describe:120-130)
  D8  lower-case left/mixed (HistoryViewModel:124, HomeViewModel:216,223)
  D10 "Starts on right" (cosmetic), D11 blank header (edge), D13 "1 min" floor (cosmetic)
  D12 "—" for solids: CONFIRMED IN CODE but NOT USER-REACHABLE — no verb creates a
      solids feed anywhere. Defer until solids logging lands (gap G3).
Task 22: test-suite findings — (HIGH) testDiaperIsTwoTaps assertion is VACUOUS: matches
  the History filter chip labelled "Diaper", present even when empty, so it passes with
  nothing logged. (MED) the .contrast exclusion cites compensating arithmetic in
  PaletteTests that DOES NOT EXIST. (MED) UITestSupport ships in release though #if DEBUG
  needed no pbxproj support. (LOW) 4 permanently-RED tests should be XCTExpectFailure.
  Also: Harness.tap(until:) returns true BEFORE tapping if the target exists, so the
  "two-tap" tests assert reachability, not tap count.
Task 22: concerns — 1 Hz HomeView Timer = MEDIUM-HIGH perf defect (7 SwiftData fetches/sec
  on the main actor, forever, plus @Observable re-render since mode isn't deduped);
  tap-swallowing causation PLAUSIBLE but unproven. UITestSupport = LOW exploitability
  (argv unreachable from SpringBoard) but total blast radius if triggered (deletes the
  change log too).
Task 22: fix round 1/5 (7 addressed — vacuous assertion proven vacuous then proven fixed
  via empty-store probe: oldMatcherFound=true newMatcherFound=false; real WCAG contrast
  arithmetic added to PaletteTests; UITestSupport now #if DEBUG; 4 reds -> XCTExpectFailure;
  commit 0356370). UI suite 21/21 exit 0; package tests 97 -> 101.
  Collision was worse than first reported: chip also renders StaticText "Diaper", and
  "Bottle" collided via Home's hierarchy behind the History tab. The ", at " infix is
  what actually disambiguates.
Task 22: COUPLING — whoever fixes the app defects MUST delete the matching
  `expectedFailure:` arguments in the same commit, or strict:true fails them as
  unexpectedly-passed.
Task 22-fixes: spec PASS, quality good (commit 7dc528e, 120 package tests, 21/21 UI,
  all 4 expectedFailure args removed). 12 defects + P1 + P2 confirmed fixed with correct
  new strings; D12 correctly untouched.
  The unrequested BabyStore.revision addition was adjudicated LOAD-BEARING and correct:
  verified that nothing called refresh() after a log sheet closed, so the 1 Hz poll was
  genuinely the only update path. Mechanism judged proportionate (@Query rejected — it
  would put fetch descriptors back in views, violating "BabyStore owns the only
  ModelContext"). Covers every in-process write except setLiveActivityID, correctly.
Task 22-fixes: CORRECTION to my earlier ruling — the BabyStoreTests SIGSEGV flake IS
  REAL. I previously recorded it "NOT CONFIRMED" after 40 clean runs. The reviewer
  reproduced it 2/8 at 7dc528e and 1/8 at base 0356370 in a worktree. Pre-existing, not
  introduced by any fix wave, but "N tests passed" on this project is a best-of-N number.
  Needs its own investigation; do not treat a single green run as proof.
Task 22-fixes: 2 Important findings -> fix round 2:
  I1 fresh install + widget-logged diaper + app idle on Home => Home stale FOREVER.
     The throttle removed a self-healing path; no scenePhase refresh exists. Previously
     self-healed within 1s. Report's "unchanged in practice" was wrong.
  I2 D8 survives at an uncited site: FeedActivityWidget.swift:21 renders the Live Activity
     label verbatim, so the LOCK SCREEN shows "Feeding · left". The justification (the
     dial uppercases it) is true for TimerDial and false for this path.
Task 22-fixes: fix round 2/5 (4 addressed — I1 scenePhase refresh without reinstating the
  poll, I2 sideLabel fixed at source, M4 exposed+fixed a real gap in setLiveActivityID,
  M2 surfaced a 4th empty state; commit 46f28d5). 123 package tests, 21/21 UI.
Task 22: COMPLETE (commits 932bee9..46f28d5). 13 copy defects found and 12 fixed
  (D12 parked as unreachable), 2 perf/a11y defects fixed, suite made trustworthy.
Task 23: spec COMPLIANT, quality GOOD, no Critical (commit fe6a8b4, 134 package tests,
  23/23 UI). All 7 required behaviours PASS.
  D-b (shifting endedAt when the start time is corrected) adjudicated CORRECT and the
  brief WRONG: a duration here is measured by a timer while a time is remembered, so
  "this happened earlier" must never mean "it lasted longer" — stretching would corrupt
  Insights averages from an edit the user thinks is cosmetic.
  D-f verified NOT a weakening: the ", at " infix guard is what excludes the filter
  chips, not the element type, so adding `buttons` cannot reintroduce the collision.
  Solids INDEPENDENTLY CONFIRMED still unreachable — the `!= nil` guard is what makes it
  so, not the clamp.
  .buttonStyle(.plain) tap-swallowing diagnosis judged credible and correctly fixed with
  .contentShape(Rectangle()); attribution to "iOS 26" is wrong (long-standing SwiftUI
  hit-testing), comment should be corrected.
Task 23: 2 Important -> fix round 1:
  I1 edit stepper is millilitre-only while the CREATE path is unit-aware, so an ounces
     user steps in 0.17 oz and can never land on a round ounce.
  I2 clampVolume silently rewrites a pre-existing 0 ml volume on ANY save, including a
     note-only or time-only edit the user believes touched nothing.
Task 23: fix round 1/5 (2 Important + 3 minors addressed; commit 5debb41). I2 solved via
  dirty-tracking (write volume only when the user moved it) rather than a range change —
  strictly stronger, also protects a stored value ABOVE range. 137 package tests, 23/23 UI.
Task 23: COMPLETE (commits 46f28d5..5debb41).
=== ALL 23 TASKS COMPLETE. Final whole-branch review next. ===

=== FINAL WHOLE-BRANCH REVIEW (43 commits) — VERDICT: ship-with-caveats ===
1 Critical (try! on container = unrecoverable crash loop + total data loss on any future
migration failure), 13 Important, 10 Minor. 137/137 tests, BUILD SUCCEEDED.
Formatter enumeration: ALL 16 call sites correct — the reversed ruling is vindicated.
isDeleted: zero remaining. Tombstones fully landed.

CORRECTION to the final review's §5: it attributed my "106 tests" reading to output
truncation. It was not. My command summed EVERY "Test run with" line. BabyStoreTests is
exactly 31 tests and 137-106 = 31, so that suite was genuinely dropped from the run with
NO crash line. This is the dangerous mode the review itself describes, and it is
confirmed to have occurred. Guardrail: CI must assert the literal total 137, not exit 0.

MUST-FIX BEFORE A PARENT USES IT:
  1. C1 try! container + I6 twelve swallowed write errors + I13 no OSLog  (one job)
  2. I11 History/Insights never refresh after .task
  3. I12 breast and bottle are 3 taps; spec says tap counts are REQUIREMENTS
  4. I7/G3 solids in scope but unreachable — ship it or strike it from the spec
  5. M23 all 43 commits unsigned -> re-sign (NOTE: I already did this; needs re-check)
MUST-VERIFY BEFORE SHIPPING (60 seconds, needs a human):
  place both widgets, tap Log diaper, start a feed, see the Live Activity, tap Stop.
  Gates I2 (Stop acts on the SELECTED baby, not the timer's baby), I3 (stale activity's
  Stop can START a sleep), I4 (widget renders fabricated placeholder data as real).
