# Home and tracking redesign — end-to-end verification, 2026-10-02

Branch `feat/home-redesign` (worktree `.worktrees/home-redesign`). Spec: `docs/superpowers/specs/2026-10-01-home-tracking-redesign-design.md`. Approved mockups: `mockups/2026-10-01-home-redesign/`.

All simulator work ran on the one shared device, `platform=iOS Simulator,id=<simulator-udid>` (iPhone 17 Pro). Nothing was pushed, deployed, or installed on a phone.

## The walkthrough found five real defects, all now fixed

Each fix has its own commit and a test that failed before the fix.

| Commit | Defect | Proof (red, then green) |
| --- | --- | --- |
| `5edb659` | VoiceOver read the "Same sleep" chip as "Same sleep, **Bottle**, 7 seconds". `HomeCopy.partTitle` mapped every glyph that was not a breast side to Bottle. | `HomeScreenTests.aJoinedSleepChipSpeaksItsPartAsAsleep`; walkthrough scenario 5 now records `chip=Same sleep, Asleep, 7 seconds` |
| `fb68b72` | More (···) sheet: at the default text size the saved-med chips were cut off by the bottom of the sheet's stated 260 pt height. | `SupplementFlowTests.testTheMedsRowSitsInsideTheMoreSheet` (red: chip `maxY` 873.4 > sheet 866) |
| `d1090c1` | The join chip wrapped its times mid-number when two parts were behind the running one ("9:0" over "2"). | `CompositeFeedFlowTests.testAJoinChipWithTwoEarlierPartsStaysOnOneLine` (red: chip height 33.7 pt) |
| `98fea2a` | Stopped bottle: **Done** sat under the bottom of the amount sheet's stated 330 pt height. | `SheetFitTests.testTheStoppedBottleShowsDoneInsideTheSheet` (red: Done `maxY` 890.6 > sheet 866) |
| `e02cdc0` | Past entry: the **Save** row was cut by the bottom of the sheet's stated 340 pt height. | `SheetFitTests.testPastEntryShowsSaveInsideTheSheet` (red: Save `maxY` 868 + row inset > sheet 866) |

The More, Bottle and Past entry clipping are regressions from `c595ac3` (Task 19). That commit moved the sheet content into a scroll view and stopped the content from being squeezed, but it kept the old stated heights. The content therefore ran past the bottom edge at the default text size. Compare `.superpowers/…/screenshots/task-15-more.png` and `task-15-bottle-stopped.png`, which were taken before Task 19.

There is also one test-only fix. `dc881e0` changed `FeedingHistoryLayoutTests.testCombinedFeedsShowCompactSummaryAndExpandableSteps`, which counted every "Feeding session actions" menu on screen, including the fixture's other feeds. It failed in the full run at 11:19 and fails the same way at the base commit `a7c4a00` (checked in a temporary detached worktree, since removed). Task 19's runs passed it in the night. The likely cause, not proven, is that the fixture places its rows relative to the time of day, so the number of other feed rows on screen varies. The test now counts the menus inside the expanded session.

## Gates (from the clean tree at `e02cdc0`)

`git status --short` was empty before the gates ran. Every gate used `rtk proxy` and wrote to a log file, so each exit code is the command's own.

| Command | Result |
| --- | --- |
| `make test` | exit 0 — `OK: 1235 tests` |
| `make server-test` | exit 0 — 33 packages `ok`, no `FAIL` |
| `make server-smoke` | exit 0 — "post-migration proof: readiness 200; … contiguous schema 26; …"; cleanup proof: zero containers and zero volumes |
| `make server-protocol-e2e` | exit 0 — `✔ Test run with 13 tests in 6 suites passed`; cleanup proof: zero containers and zero volumes |
| `python3 scripts/verify-localizations.py` | exit 1, as on `main`. 256 problems, against 268 in the `main` baseline. `comm -13 loc-baseline-main.txt loc-now.txt` prints **nothing**, so there are 0 new problems (ruling R11). |
| `make build SIMULATOR='platform=iOS Simulator,id=<simulator-udid>'` | exit 0 — `** BUILD SUCCEEDED **` |
| `make test-ui SIMULATOR='platform=iOS Simulator,id=<simulator-udid>'` | exit 0 — `Executed 142 tests, with 0 failures (0 unexpected)`, `** TEST SUCCEEDED **` (14:49–15:44; a watcher saw no other `xcodebuild test` on the device the whole time) |

The only container left after the runs is `buildx_buildkit_plainpost-release-v0290`, which predates them.

Earlier runs, for the record:
- **At `be9c016`:** `make test-ui` gave 141 tests with 1 failure, the time-of-day `FeedingHistoryLayoutTests` failure fixed in `dc881e0`.
- **The first run at `e02cdc0`:** 142 tests with 15 failures, all in `CaregiversScreenUITests` and `CompositeFeedFlowTests`. Another project's session (another app) was running its own UI suite on the same shared simulator at the time. Its own watcher logged the overlap. The run above was started only once that device was free, and stayed clean.

## Step 2 — simulator walkthrough (English)

Every scenario is a test in `UITests/BabyTrackerUITests/HomeRedesignWalkthroughTests.swift`. Each one drives the real UI with taps on a fresh store (`-uiTestResetStore -uiTestSeedBaby`, plus the seeds named below), asserts the outcome and photographs every step. Screenshots are in [`2026-10-01-home-tracking-redesign/`](2026-10-01-home-tracking-redesign/), as `<name>.jpg` (downscaled from the result bundle of the final clean `make test-ui` run).

| # | Scenario | How it was proven | Screenshots |
| --- | --- | --- | --- |
| 1 | Tap L, wait 1 min, Stop. The hero shows "Break" and the L part. R is outlined. | `test01…`: a real 61 s wait. Asserts hero label `Break`; value contains `Left breast, 1 minute, 3 seconds`; `home.start.right` value `Suggested`; L not suggested. | `w01a-left-running`, `w01b-break-after-left` |
| 2 | R within the limit gives the chip "Same feed · L 1:00". Stop gives one feed in History. | Same test. The chip reads `Same feed, Left breast, 1 minute, 3 seconds` and shows "Same feed · L 1:03". After a 31 s Right part and Stop, History has exactly one `history.feedingSession` ("1m active · 2 feeds"), no lone breast row, and steps 0–1 only. | `w02a-right-joined-chip`, `w02b-break-after-right`, `w02c-history-one-feed` |
| 3 | Settings → Tracking feed limit 5 min, wait 6 min, tap L. There is no chip, and History shows two feeds. | `test03…`. Instead of a real 6-minute wait, a debug seed `-uiTestFeedEndedSixMinutesAgo` logs a Left 5:00 that ended 6 min ago. At the default 30 min, Home is in its "Break". In Settings, the stepper is tapped down 30 → 5 ("Join feeds within, 5 min"). Home then shows "Awake". L runs for 32 s with no join chip. History has two lone Left rows and no session. | `w03a-break-at-default-limit`, `w03b-settings-feed-limit-5`, `w03c-left-running-no-chip`, `w03d-history-two-feeds` |
| 4 | Tap R, then ✕ on the chip, then Stop. Two separate feeds. | `test04…` from `-uiTestFeedBreak` (Left 9:02, ended 40 s ago). Steps: R, chip, ✕, chip gone, 31 s, Stop. History has no feed session, and the Right row stands alone. | `w04a-right-joined`, `w04b-right-separate`, `w04c-history-two-feeds` |
| 5 | Sleep, Wake up, then Sleep within 15 min. The chip "Same sleep" appears, and History shows one sleep row. | `test05…`. The chip reads `Same sleep, Asleep, 7 seconds` (after fix `5edb659`). History shows exactly one `history.sleepSession`, which expands to two parts. | `w05a-awake-after-wake`, `w05b-same-sleep-chip`, `w05c-history-one-sleep`, `w05d-history-sleep-expanded` |
| 6 | Bottle → chip 90: one bottle joins the open feed. Bottle → Start bottle → Stop: the amount sheet, then Done. | `test06…` from `-uiTestFeedBreak`. After chip 90, the hero is still "Break" and its parts are Left 9:02 plus Bottle. Start bottle shows the hero "Bottle" with the join chip. Stop opens the amount sheet (Done inside the sheet after `98fea2a`); Done returns to "Break". History has one session with three steps (Left, 90 ml, timed bottle). | `w06a-bottle-sheet` … `w06f-history-feed-with-bottles` |
| 7 | Diaper → Details → Note → Mixed. The History row shows the note and Mixed. | `test07…`. The History row reads `Diaper, Mixed, at 15:18`. The note "after bath" was saved in the same write and appears in the row's edit sheet. **Deviation:** the History *row* does not show the note (see below). | `w07a-diaper-details-note`, `w07b-history-diaper-row`, `w07c-history-diaper-edit-note` |
| 8 | ··· → Meds chip: logged. ··· → +: the add-med flow. ··· → Past entry: Left 10 min, 1 h ago. It joins or doesn't, per the rule. | `test08…` with `-uiTestSeedFixture -uiTestSeedMeds`. A med chip logs in one tap and closes the sheet; History has the row. + (scrolled to, since it is always last) opens the add-med sheet (`supplement.add`). In Past entry, the time wheels are set to start = now − 60 min and end = now − 50 min. The row "Breast, Left …" appears at that start time. Because no session ended within 30 min of it, it stands alone, and L on Home afterwards gets no chip. | `w08a-more-sheet`, `w08b-more-sheet-add-med-chip`, `w08c-add-med-flow`, `w08d-past-entry-left-1h-ago`, `w08e-history-med-and-past-entry` |
| 9 | Sleep running: the status line shows last feed and last diaper. | `test09…` with `-uiTestRunningSleep`. The status line reads `Feed 6 hours, 53 minutes, Diaper 2 hours, 17 minutes`, and has no Awake item. | `w09-sleep-running-status-line` |

### Comparison with the mockups

The mockups were rendered with headless Chrome and saved beside the screenshots as `mockup-home-layout.png`, `mockup-flow-start-stop.png` and `mockup-sheets-v2.png`. The layout follows mockup C (glance on top, dock in the thumb zone, tab bar unchanged) and the start → stop flow. Deviations:

- **No thin parts bar under the hero.** Accepted by the controller in Task 14. The parts appear as glyph-and-time pairs instead (`w01b`, `w06b`).
- **The break caption is "Break"**, not "Feed · break". Accepted in Task 14.
- **The Diaper sheet's Details start collapsed.** The mockup shows them open; the spec says "collapsed".
- **Home's L/R tiles read "L"/"R" with a lettered drop**, while the mockup reads "Left"/"Right". The spec's dock says **L** and **R**.
- **The bottle sheet's amount chips sit flush with the bottom of the sheet** at the default size. They are inside the sheet (`SheetFitTests.testTheBottleSheetShowsItsAmountsInsideTheSheet` passes), but the mockup has more room under them.
- **History sleep sessions** draw their member connector in feed amber (`w05d`). History keeps its layout until its own redesign.

## Step 3 — Ukrainian at XXXL on the shared iPhone 17 Pro

`test10UkrainianHeroStatesAndSheetsAtXXXL` launches with `-UIPreferredContentSizeCategoryName UICTContentSizeCategoryXXXL -AppleLanguages (uk) -AppleLocale uk_UA`. It photographs every hero state: awake, feed running, break, joined, sleep running, feed during sleep and bottle running. It also photographs every sheet: Bottle, the exact amount, Diaper, Diaper details, More, Solids, Growth, Past entry, Meds, the running-timer editor and the stopped bottle's amount. The existing `UkrainianLayoutSmokeTests.testHomeStatesInUkrainianAtXXXL` asserts that every dock control is on screen, hittable and at least 44 pt in five of those states. Screenshots: `uk-home-*`, `uk-sheet-*`.

No truncation or overlap on Home. On the sheets, there were two findings:

- **Diaper sheet: "Бру-дний" and "Змі-шане" hyphenated inside their tiles** (`uk-sheet-diaper`, taken before the fix). Fixed in the final fix wave (ruling R26): the three tiles keep one row only when the widest word fits its third whole, and otherwise stack, with the sheet's stated height growing to hold the stack. `UkrainianLayoutSmokeTests.testDiaperChoicesStackRatherThanBreakAWordInUkrainianAtXXXL` failed before the fix (the tiles shared a row) and passes after it; it also checks that English at the default size keeps its row. Screenshot after the fix: `uk-sheet-diaper-stacked.jpg`.
- **The running-timer editor's inline title is truncated to "Зміни…"** next to Cancel and Save (`uk-sheet-running-timer-editor`). The title and sheet are unchanged from `main` (`RunningTimerEditSheet`); this is pre-existing and not fixed here.

## Step 4 — two caregivers through the local server

Ruling R8 allows one simulator, so the two caregivers are the E2E harness: two `Phone`s built from the app's composition, signed in through the identity adapter, against the TLS server that `make server-protocol-e2e` builds. Both phones run the same store code as the simulator app.

- **Scenarios 1 and 2.** In `OnePartTimerSyncE2ETests.theOtherPhonesRightJoinsLeftInItsBreak` (new), the owner times Left and stops it, both phones sync, and the caregiver taps Right in the break. Proof line from the final run: `cross-phone join proof: owner's Left, caregiver's Right in the break; owner=["left", "right"] session=2046940F…; caregiver=["left", "right"] session=2046940F…`. The two race orders are also covered (`twoCaregiversStopAndStartKeepOneSession`, `ownersStopFirstStillKeepsOneSession`); both end with one session on both phones.
- **Scenario 5.** In `aSleepStartedOnTheOtherPhoneAfterAShortWakeJoins` (new), the owner's 30-minute sleep ends, and 10 minutes later the caregiver starts a sleep. Proof line: `cross-phone sleep join proof: owner=2 parts session=D9CAA3A9… asleep=3000s; caregiver=2 parts session=D9CAA3A9… asleep=3000s`.
- Both phones show the same grouping: same session ID, same parts and an empty outbox.

## Step 5 — the locked-iPhone check (the user's)

Not done here, because it needs a physical phone and an install that only the user can approve. Steps:

1. With approval, install the signed build from this branch on one phone. The server must run migration 26 first (see Open risks).
2. Start **L** from the app.
3. Lock the phone.
4. Press **Stop** on the Lock Screen Live Activity.
5. In the break, press **R** without unlocking.
6. Press **Stop**.
7. Start **Sleep** from the Quick Log widget.
8. Press **Wake up** on the Lock Screen.
9. Press **Sleep** in the break.

Expected: no unlock prompt and no app launch at any point. Then open the app: History shows one feed (L + R) and one sleep of two parts.

Result: _not yet run — record it here._

## Deploy note

The server and both phones move together. In order:

1. **Back up the production database** immediately before deploying.
2. **Migrate to schema 26** (`000026_sleep_sessions.sql`) before starting the new server, and before any manual rerun of `init-users.sh`. Bootstrap on a schema-25 database fails until it is migrated (Task 18c).
3. **Drain both phones' outboxes on the old build first:** open the app online on each phone and let it sync. After the server deploy, do not use **Switch** or **Pause** on an old build: the new server no longer accepts those commands. (A new build that still finds one queued quarantines it and keeps syncing — final fix wave C1 — but the action it stood for is lost.)
4. **Update both phones together**, and do not edit sleeps on a phone that has not been updated yet.
5. **There is no down-migration.** `Server/internal/migrate/compatibility.json` sets `min` = `max` = 26, and migration 26 has no down step. Rolling back means restoring the backup from step 1.

## Open risks

- **The server deploys first.** A production database at schema 25 needs migration `000026_sleep_sessions.sql` before the new app is installed. The new client always sends timer payload v5 and sleep payload v2 (ruling R2), so it cannot sync with a pre-26 server. Phones would show "out of date" until the server is upgraded; the outbox keeps the mutations. Rerunning bootstrap manually on a schema-25 database fails until it is migrated (Task 18c).
- **History rows do not show a diaper note** (scenario 7 as written). The note is saved and editable in the row's edit sheet. History's layout is out of scope until its redesign.
- **Ukrainian XXXL:** the editor title truncation above (the Diaper tile hyphenation is fixed). Also, the volume unit is the hard-coded `ml` (`VolumeUnit.shortName`) in every language. This is pre-existing.
- **The audit's "stated height" rationale** (ruling R23) was partly wrong. Two of the sheets really were too short at the default size (fixed here). The remaining `textClipped`/`statedHeight` `KnownIssue` entries still match exactly and pass, but the final review should re-read them with this in mind.
- **UI-test reset flake, seen once.** On the first test of the first walkthrough run, a `-uiTestResetStore -uiTestSeedBaby` launch showed fixture-like data (strip "4 feeds", sleep "5h 52m") from an earlier run. It did not recur in the next three runs. The flake is debug-only, but `emptyStore`/`discardUITestStore` may not always win against a store another process still holds.
- **Deferred minors from the ledger**, in brief:
  - Server: the validation gate admits a few payload-version combinations in-process that the wire decode blocks (T3). There is no in-process UUID check for session IDs (T3). The timed-bottle boundary is unscoped (T4). `openapi`/`v1.md` are stale in places (T4). Three compatibility tests are redundant (T2). Two commit trailers say Sonnet (T3, T18b).
  - Client tests: there is no test for a remote sleep with `sleep_session_id`, for the absent/malformed `session_id` branch, or for the Go v2 sleep hash vector (T6). Untested: `sleepBreakEndedAt`/today spans (T10), `SleepSessionError` paths (T8), `HomeCopy.age`/`StatusLineView.spoken` (T14). Fixture duplication (T8, T18).
  - Behaviour: `logBreast` accepts future times (T9). `startPart` does not recheck for a concurrently inserted timer (T9). Accidental short bottle parts save at the usual volume (T9, product check). `adoptOrStart` requests a fresh card for a dismissed one; the break card is reconciled against its stored end; an in-app `startBreast` over a running timed bottle skips "Set amount" (T16). A stale break card lingers until the next app activation (R18).
  - Copy and UI: the Tracking section has no footer (T7). The `AskContextBuilder` prompt still says "Supplements" (T13). (The running-timer editor footer that still said "Switch sides to finish it" (T19) is fixed in the final fix wave.) Grouped History members have no swipe-to-delete (T11). The fr/it/uk Siri phrases need a native-speaker check (T17).
  - Sync: a replayed receipt can carry a stale winner session; a deleted winner leaves a dangling link (T19b).
