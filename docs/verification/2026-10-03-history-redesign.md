# History redesign — verification, 2026-10-03

Branch `feat/history-redesign` (worktree `.worktrees/history-redesign`). Spec: `docs/superpowers/specs/2026-10-03-history-redesign-design.md` (with its Amendments). Plan and rulings H-R1 to H-R26: `docs/superpowers/plans/2026-10-03-history-redesign.md` and the SDD ledger. Approved mockups: `mockups/2026-10-03-history-redesign/`.

All simulator work ran on the one shared device, `platform=iOS Simulator,id=<simulator-udid>` (iPhone 17 Pro). Nothing was pushed, deployed or installed on a phone, and production was not touched.

## Gates

Every gate ran through `rtk proxy` with its output in a log file under `work/t7/` (untracked); each exit code is the command's own. All ran on the tree committed as `8de160b` (the doc and screenshots came after).

| Command | Result |
| --- | --- |
| `make test` | exit 0 — `OK: 1376 tests` (`EXPECTED_TESTS` unchanged: Task 7 adds UI tests only) |
| `make build SIMULATOR='platform=iOS Simulator,id=<simulator-udid>'` | exit 0 — `** BUILD SUCCEEDED **` |
| `make test-ui SIMULATOR='platform=iOS Simulator,id=<simulator-udid>'` | exit 0 — `Executed 165 tests, with 0 failures (0 unexpected)`, `** TEST SUCCEEDED **` (18:56–20:04; no other `xcodebuild` on the device before it started). No flake, no re-run. |
| `make server-protocol-e2e` | exit 0 — `✔ Test run with 14 tests in 7 suites passed`; the container and volume lists were identical before and after |
| `comm -13 <(sort work/loc-baseline.txt) <(python3 scripts/verify-localizations.py 2>&1 \| sort)` | printed nothing (Task 7 adds no strings) |

Before Task 7, the UI suite at `3e5bcb6` (Task 6's tip) ran 145 tests with 24 failing tests (29 failures), every one in a suite that still used the old History identifiers, editor or copy: AskTab (1), CompositeFeedFlow (2), EditFlow (2), FeedingHistoryLayout (3), HomeRedesignWalkthrough (7), LoggingFlow (6), SettingsFlow (1), SupplementFlow (2).

## What Task 7 changed

**Tests** (`UITests/BabyTrackerUITests/`):
- A new seed, `-uiTestHistorySessions` (`UITestSupport.seedHistorySessions`): fixed clock times on the two days before today, so neither the rows nor their days depend on when the suite runs, and every session ended long before launch (H-R18). Yesterday: solids 17:30, growth 16:00, a sleep with one wake 13:00–15:30, a wet diaper 12:00, a feed of Left 12m, Right 15m and a 60 ml bottle 10:23–11:05, Vitamin D 09:00. The day before: a feed across midnight, 23:45–00:15. The read-only family seeds it before it turns viewer.
- The `-uiTestHomeLinkedMixedSession` seed now sits at a fixed time yesterday (Task 6's M2: before about 01:24 it moved to Yesterday). The unused adjacent-feeds seed is gone.
- `HistoryRedesignTests` (7): Split here, then Join the 10:23 feed; swipe-delete a diaper; swipe-delete a session after its confirmation (Keep it tried when exposed); filter to Diapers and clear it; every edit sheet opened from its row and saved; AX5 rows and the Diaper edit sheet; a viewer's rows are not buttons and open nothing.
- Ported to the new identifiers and flows, keeping their intent: CompositeFeedFlow, FeedingHistoryLayout, EditFlow (Dry, and the sleep editor's Nap / Night / Auto in place of the old override), LoggingFlow, HomeRedesignWalkthrough, CopyReadThrough, SupplementFlow, SettingsFlow, AskTab (the source opens its edit sheet, H-R9; a deleted source opens History and no sheet, H-R25) and ScreenshotTour (plus `testTourHistoryRedesign`, the source of this doc's screenshots). `goToTab("History")` waits for the filter button.
- Audits: the filter sheet, the filtered list, the feed and sleep session sheets, the breast, bottle and sleep part editors, and the Diaper, Meds, Growth and Solids edit modes, next to the existing `testAuditHistory` and `testAuditHistoryAtAccessibilityXXXL`.
- Ukrainian at XXXL: `UkrainianLayoutSmokeTests.testHistoryInUkrainianAtXXXL`.

**App fixes found by the audits and the screenshots** (outside test code; commit `5853236`):
1. **Session sheet Done had a word-sized hit area.** The 44 pt frame sat outside the button. Audit `hitRegion` on `sessionSheet.done` before; none after.
2. **A disabled Add part looked enabled** (the parked Task 4 minor, confirmed in `h18`). The explicit tint hid the disabled look; it is now dimmed while off.
3. **Part editors opened on their own hid fields under the footer**: a lone breast part's Duration, a lone sleep's Nap / Night / Auto, a bottle's contents, time and Details (also pushed inside a session). Stated heights raised (breast and sleep 300 → 460, bottle 440 → 650; bottle pushed in a session 660 → 780). Before and after: `h07*`, `h09b`, `h06`.
4. **The Diaper sheet in edit mode at AX5 overflowed upwards**: the time row ran under the bar with its label cut to "D…". Edit mode now scrolls at the accessibility sizes, and its time row puts the label over the pickers there. Pinned by `HistoryRedesignTests.testRowsAndAnEditSheetAtAccessibilityXXXL` (time row below the bar and on screen; Dry, Save and Delete reachable). Create mode is unchanged.

## Accessibility audits

Each new audit records every finding; contrast is reported as an expected failure, as everywhere in the suite. Known issues are named by audit type and element, and capped by count (ruling R23). They were sorted by one experiment: the eleven new audits re-run with `sheetHeight` raised by 1000 pt (reverted afterwards).
- Gone with the height raised, so filed as artifacts: the `statedHeight` text clipping in the session sheets, part editors and Solids, and every element-less `elementDetection` (History's own text visible above a part-height sheet, `historyBehindTheSheet`).
- Still there with the height raised, each with its own reason in the test: Cancel on the edit sheets (`barButton`, as on every sheet); Add part and Delete on the session sheets past the fold at the audit's enlarged size (the Settings pattern; reached and hit at XXXL by the Ukrainian test); the Growth sheet's own Form (two prompt-titled TextFields and its date row, unchanged by edit mode); and the Diaper edit sheet's time row, which was the real defect fixed above. After that fix the audit reports three element-less clipped texts there instead of the three it named before (the probe size switches the sheet into its scrolling layout), capped at three.
- The audit raises the stated-height findings on a different subset of the same elements from run to run, so those entries allow fewer, never more.

The existing `testAuditHistory` and `testAuditHistoryAtAccessibilityXXXL` pass unchanged on the new list.

## Ukrainian at XXXL

`testHistoryInUkrainianAtXXXL` launches with `-UIPreferredContentSizeCategoryName UICTContentSizeCategoryXXXL -AppleLanguages (uk) -AppleLocale uk_UA` and the History seed. It asserts every row and the day header inside the screen's width and at least 44 pt, the Showing chip ("Показано: Підгузки і Ліки") on screen and hittable, the session sheet's Done, parts, both Split here, Add part and Delete feed on screen (by scrolling) and hittable, and the Diaper edit sheet's time row, four choices, Details, Save and Delete on screen and hittable.

Word breaks cannot be read from the accessibility tree, so each screen is photographed. Reviewed: no word breaks mid-word. The sleep row wraps "2 год 15 хв · / Пробуджень: 1" at a word; the Join row wraps "Об'єднати з / годуванням / о 23:45" at words; "Дата й час" wraps at a word.

A part-height sheet is drawn inset at 386/402 of its size, so a 44 pt target measures 42.2 pt on screen there; the size assertions inside sheets allow for that.

## Screenshots

In [`2026-10-03-history-redesign/`](2026-10-03-history-redesign/), JPEG, downscaled from the result bundle of the clean `make test-ui` run above. English shots run at the simulator's region (24-hour clock, decimal comma).

| File | What |
| --- | --- |
| `h01-list.jpg` | The list from the History seed: Yesterday's header with icon totals, one row per session, chips, the feed across midnight under its own day |
| `h02-filter-sheet.jpg`, `h03-filtered-list.jpg` | The filter sheet; the list filtered to Feeds and Diapers, with the dot and the Showing chip |
| `h04-session-sheet-feed.jpg` | The feed's session sheet: Join the 23:45 feed, three parts, gap lines with Split here, Add part, Delete feed |
| `h05-edit-breast-part.jpg`, `h06-edit-bottle-part.jpg` | The breast and bottle part editors pushed inside the session |
| `h07-edit-bottle-alone-with-join-row.jpg`, `h07b-edit-breast-alone-with-join-row.jpg` | A lone bottle and a lone breast part, each with its Join row |
| `h08-session-sheet-sleep.jpg`, `h09-edit-sleep-part.jpg`, `h09b-edit-sleep-alone-with-join-row.jpg` | The sleep's session sheet, its part editor, and a lone sleep with "Join the 13:00 sleep" |
| `h10-edit-diaper.jpg` … `h13-edit-solids.jpg` | Diaper (with Dry), Meds, Growth and Solids in edit mode |
| `h14-feed-across-midnight.jpg` | The feed that started at 23:45, listed once, with 0:15 under it |
| `h15-running-row.jpg` | A running sleep at the top of today, ticking |
| `h16-empty-nothing-logged.jpg`, `h17-empty-nothing-matches.jpg` | Both empty states |
| `h18-sleep-add-part-off.jpg` | A sleep that ended seconds ago: Add part dimmed and off (H-R18) |
| `ax5-diaper-edit.jpg` | The Diaper edit sheet at AX5 after the fix |
| `uk-xxxl-*.jpg` | Ukrainian at XXXL: the list, scrolled, the filter sheet, the filtered list, the session sheet and the Diaper edit sheet |

## Known limitations and parked minors

- **H-R26 — old mixed solids feeds.** An older entry that mixes a solids component with other parts cannot be fully edited: the session sheet skips its solids part, and a solids-kind mixed feed opens the Solids sheet, which hides the others. Accepted: production has 0 solids components (probe 2026-10-03) and the new client never creates mixed solids entries. Such an entry's solids part is editable only by deleting and re-logging the feed.
- **H-R25.** A view-only caregiver gets no sheet from an Ask source (the list opens). A source deleted before it opens shows History and no sheet, with no message — `AskTabTests` used to expect a "Source entry unavailable" alert and now asserts this.
- **A legacy untimed combined feed can't be moved later** (final review M4). The part editor edits one part; when a feed's other parts never recorded their times, the span only widens, so moving such a part later leaves the feed, and its row, at the old time. The old editor shifted the whole entry. Rare: only older phones wrote untimed combined feeds.
- **A timed bottle's length is not editable** (final review M5). The bottle part editor edits the amount, contents and time; the old editor also allowed its end. Rarely needed.
- **Parked from the final review:** the Growth edit sheet does not merge a remote edit or close on a remote delete (M2, pre-existing in the Growth tab); an Ask focus can reopen its sheet when History is rebuilt, as `historyFocus` is never cleared (M11, pre-existing); the audit's element-less known-issue caps could be tightened (M7); `HistoryView.swift` is ~800 lines and `clockTime` is duplicated in History and LoggingFeature (M12).
- **Equal-start ties** (parked in Task 3): two parts that start at the same instant order by id in the store, so a tie can list in either order. H-R17 keeps added parts strictly after the session's last start.
- **Build warnings at `BabyStore.swift:130` and `AskModel.swift:184`**, pre-existing and untouched.
- **Parked from Task 4:** the session sheet's Add part keeps a 5 s redraw after it turns on again; nothing says why Left / Right are off right after a session (they come back within a minute).
- **Ticket (out of scope):** `withdrawUnsentCompletion` (`BabyStore+RemoteTimers.swift`) soft-deletes a feed that may be a session's root, which can orphan its members.
- **Pre-existing:** diaper and med rows are not realm-filtered in History (feeds and sleeps are).
- **"ml" is not translated.** `VolumeUnit.shortName` is a literal "ml"/"oz" app-wide (BabyCore), so Ukrainian rows read "60 ml". Not History's; unchanged.
- **The bottle editor opened on its own** with its Join row needs the full screen; Details sits below the fold, a scroll away.
- **The Growth sheet's Form** (unchanged by edit mode) has the audit findings named above; owned by a Growth follow-up.
- **Diaper create mode at the accessibility sizes** was not changed or checked here; edit mode's overflow was the History-reachable case.

## Final fix wave

Fixes for the final whole-branch review (`final-review.md`):

- **I1.** Join (`combineFeeds`, `combineSleeps`) and Split (`separateFeed`, `separateSleep`) move a running part's session link with its session — to the combined session, or to the later half — with the same session-only timer edit a delete's relink sends.
- **I2.** A session id resolves to the session it keys before the session of a member with that id: the store's `feedSessionKey`/`sleepSessionKey`, `SessionSheetModel.findSession` and `JoinRowModel` (whose `init(partOf:)`, used by a lone part's editor, stays member-first).
- **I3.** After a join the Join row hides until its sheet opens again.
- **I4.** `make server-protocol-e2e` step 24: Add part, Split here, Join and deleting the first part, with a part running, reach the other caregiver and the server's snapshot.
- **Minors taken:** M1 (a confirmed session delete re-checks the row), M3 (a changed end may not run into the next part; new string in six languages), M6 (Add part stops where a running continuation began), M8, M10 (tests), and the Task 3 parked minor above. M12's Supplement realm check was already in place (`store.entry` reads supplements realm-filtered).


Gates after the fix wave, on the shared iPhone 17 Pro simulator: `make test` 1401 OK; `make build` succeeded; `make server-protocol-e2e` exit 0 (14 Swift E2E tests, step 24 present, cleanup proof); localization gate empty; UI tests `HistoryRedesignTests`, `EditFlowTests`, `CompositeFeedFlowTests` and `FeedingHistoryLayoutTests` 17 tests, 0 failures.

- After a Join, the Join row stays hidden until the sheet is reopened, even if a later Split undoes the Join (by design, prevents chained joins from a double tap).
- If the row changed while a delete confirmation was showing, the confirmed delete does nothing and shows no message.
