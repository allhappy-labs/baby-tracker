# Insights redesign — verification, 2026-10-06

Branch `feat/insights-redesign` (worktree `.worktrees/insights-redesign`). Spec: `docs/superpowers/specs/2026-10-04-insights-redesign-design.md`, including its 2026-10-06 amendments. Approved mockups: `mockups/2026-10-04-insights-redesign/` (`insights-full.html` for Week, All and new baby; `insights-growth.html` for G1).

All simulator work ran on the one shared device, `platform=iOS Simulator,id=<simulator-udid>` (iPhone 17 Pro). Before every `xcodebuild` run, `pgrep -fl xcodebuild` found nothing else running. No simulator was created or cloned. Nothing was pushed, deployed or installed on a phone.

## Gates

Logs are under `work/` (git-ignored).

| Command | Result |
| --- | --- |
| UI subset, run 1 (see below), on `ede06d1a` plus the Task 10 test changes | exit 65 — `Executed 9 tests, with 1 failure (0 unexpected) in 185.177 s`, `** TEST FAILED **`. The failure was `testAuditInsights` (next section). |
| `-only-testing:BabyTrackerUITests/AccessibilityAuditTests/testAuditInsights`, with the fix | exit 0 — `Executed 1 test, with 0 failures`, 13.6 s |
| UI subset, run 2, on `82f4c07f` plus the Task 10 test changes | exit 0 — `Executed 9 tests, with 0 failures (0 unexpected) in 206.512 s`, `** TEST SUCCEEDED **` (21:10–21:14) |
| `make test` | exit 0 — `OK: 1609 tests` (`EXPECTED_TESTS` unchanged: Task 10 adds UI tests only) |
| `xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,id=<simulator-udid>' build` | exit 0 — `** BUILD SUCCEEDED **` |
| `python3 scripts/verify-localizations.py`, compared with `comm -13 <(sort work/insights-loc-baseline.txt) <(sort work/insights-loc-task10.txt)` | the script exits 1 on 144 lines, all of them existing AskFeature gaps. The baseline had 272. `comm -13` printed nothing, so there are no new problems. |

The UI subset command:

```bash
xcodebuild test -scheme BabyTracker -destination 'platform=iOS Simulator,id=<simulator-udid>' \
  -parallel-testing-enabled NO -maximum-concurrent-test-simulator-destinations 1 \
  -only-testing:BabyTrackerUITests/GrowthFlowUITests \
  -only-testing:BabyTrackerUITests/AccessibilityAuditTests/testAuditInsights \
  -only-testing:BabyTrackerUITests/AccessibilityAuditTests/testQuickLogStaysHittableAtAccessibilityXXXL \
  -only-testing:BabyTrackerUITests/SettingsFlowTests/testChangingTheUnitReachesHomeHistoryAndInsights \
  -only-testing:BabyTrackerUITests/UkrainianLayoutSmokeTests \
  -only-testing:BabyTrackerUITests/AskTabTests
```

`testQuickLogStaysHittableAtAccessibilityXXXL` is the AX5 test that visits Insights (the method containing `ax5-dock`).

Run 2, test by test:

| Test | Result |
| --- | --- |
| `AccessibilityAuditTests.testAuditInsights` | passed (14.8 s) |
| `AccessibilityAuditTests.testQuickLogStaysHittableAtAccessibilityXXXL` | passed (33.2 s) |
| `AskTabTests.testNaturalQuestionRendersAsReadableConversation` | passed (15.2 s) |
| `AskTabTests.testUnavailableModelHidesAskWithoutDeletingChat` | passed (32.4 s) |
| `GrowthFlowUITests.testHomeMeasureSavesPartialCheckIn` | passed (18.3 s) |
| `GrowthFlowUITests.testInsightsGrowthSectionOpensCheckInsInHistory` (new) | passed (15.6 s) |
| `GrowthFlowUITests.testNewBabyRequiresReferenceAndOffersOptionalDueDate` | passed (16.2 s) |
| `SettingsFlowTests.testChangingTheUnitReachesHomeHistoryAndInsights` | passed (40.3 s); "oz" found on Insights |
| `UkrainianLayoutSmokeTests.testPrimaryScreensAndLanguageShortcut` | passed (20.5 s) |

Run 1 had the same results apart from the audit. After the tests finished, xcodebuild spent about 10 minutes in its own `simctl diagnose` (600 s timeout) before it exited. That is normal xcodebuild behaviour after a failed run, and it was not a hang in the tests.

## What Task 10 changed

- `UITestHarness.swift`: `goToTab("Insights")` now waits for `insights.range`, because the "Insights" text it used to wait for was removed with the eyebrow.
- `GrowthFlowUITests.testInsightsGrowthSectionOpensCheckInsInHistory`: opens Insights and checks that the range picker is at least 44 pt. It scrolls to All check-ins, selects the Length chip (the chip becomes selected), and taps All check-ins, which lands on History with the `history.filter.clear` chip showing.
- **Product fix, commit `82f4c07f`.** In run 1, `testAuditInsights` failed with three element-less `[elementDetection] Potentially inaccessible text` findings. The audit had found visible text hidden from accessibility: the charts' axis end labels ("Tue 29" … "Today") and the sleep chart's Night / Naps key, all marked `.accessibilityHidden(true)`. Each of those `HStack`s is now one combined accessibility element. A re-run of the audit alone then had no blocking findings, which confirmed the cause. No `KnownIssue` was added.
- Six contrast findings remain on Insights in every run: the Sleep card's rows under the glass tab bar ("Longest stretch", "Night · naps" and its value, "Naps / day", "2", "Awake between sleeps"). These fall under the suite's existing contrast exclusion: the audit's sampler cannot composite this app's glass and gradient surfaces, and the findings are reported as expected failures, as on every screen.

## Range picker hit height

The segmented `insights.range` control has an accessibility frame of `(16, 122, 370, 48)`, so it is **48 pt** high. That frame was read by the screenshot capture and comes out the same in the screenshot (about 144 px at @3x). The new UI test asserts `>= 44`, and it passed. No change was needed.

## Screenshots against the mockups

The screenshots were captured on the shared simulator from the `Harness.Launch(seedFixture: true, historySessions: true)` launch, and from a plain seeded baby for the new-baby state. `simctl` cannot tap, so a temporary UI test (not committed, and deleted afterwards) tapped the Insights tab, the range segments, a sleep bar and the scrolls, and saved each screen with `XCUIScreen` screenshots. The mockups were rendered to PNG with headless Chrome for the side-by-side check.

- `work/insights-week.png`: Week
- `work/insights-week-sleep-callout.png`: Week with a sleep bar tapped
- `work/insights-all.png`: All
- `work/insights-growth.png`: scrolled to Diapers and Growth
- `work/insights-new-baby.png`: a baby with nothing logged
- `work/mockup-insights-full.png` and `work/mockup-insights-growth.png`: the rendered mockups

The fixture holds only two logged days and one weight check-in, so the charts are sparse next to the mockups' ten weeks of data. The comparison below is about structure and styling, not about the data.

**Matches the mockup (or the spec where they differ):**
- The pinned header shows the baby's name with the switcher chevron, then Week / Month / All. There is no "INSIGHTS" eyebrow and no section title words. Sections are marked by their icon and colour.
- The headline is the number plus its unit ("3 feeds / day", "7h 45m / day", "4 diapers / day"). With no previous period, the right side says "2 days" or "1 day", as in the mockup's new-baby state. All says "since Oct 4".
- The charts have two faint gridlines labelled on the right, a dashed average line, and x labels at the two ends only ("Tue 29 … Today"). The empty days of the week are empty slots.
- Sleep has stacked bars with night solid and naps lighter, the Night / Naps key, and the rows Longest stretch, Night · naps, Naps / day and Awake between sleeps. The tapped-bar callout reads "Mon, Oct 5 · 7h 45m / Night 2h 45m · naps 5h", like the mockup's sleep callout.
- Feeding has the rows Between feeds, Longest gap, the split row with its thin split bar, Left · right, and Bottle / day.
- Growth (G1) has the icon with the latest date and age, three chips with Weight selected and outlined (value and P91), the Swiss PEZZ 2025 reference link, and "All check-ins 1 ›".
- The new baby, with nothing logged, gets "Nothing logged in this period." in each section and "No measurements yet" plus the reference link in Growth (spec section 1 and the amendment's empty copy).

**Differences: accepted, with reasons:**
1. **Today's bar is a faint fill, not hatched.** The 2026-10-06 amendment says so: Swift Charts has no pattern fill.
2. **The segmented control is taller and rounder than in the mockup.** It is the iOS 26 large segmented control. The 48 pt height is what the 44 pt rule needs.
3. **The split row reads "Breast · Both · Bottle".** The spec's example is "Breast · both · bottle". The code joins three separately translated words, and `InsightsPresentationTests` pins the title case. Changing it needs a translated string for each combination of kinds in five languages, because German capitalises nouns and some other words. Not changed here; this is flagged for the controller as a copy follow-up.
4. **All with less than a week of data draws one wide bar labelled "Oct 4 … Oct 4".** This follows spec section 2 (one bar per 7-day block, two end labels). With ten weeks of data it reads like the mockup's "wk 1 … wk 10". The axis uses dates rather than "wk N", and the spec doesn't fix the wording.
5. **Growth: "4,2 kg", "Oct 5 · 1w", and Length and Head showing "—".** The simulator's region is Ukraine (`en_US@rg=uazzzz`), which uses a decimal comma. The age uses the existing compact formatter. The fixture has no length or head readings, so "—" is the spec's never-measured value. The change line and the chart are replaced by "A trend needs two measurements." because there is one weight reading (amendment).
6. **Growth's rows have looser spacing than the mockup, and no divider above All check-ins.** The spacing comes from the reference link and All check-ins rows keeping their 44 pt minimum height (spec section 5). Cosmetic.
7. **The callout overlaps the headline's right side ("1 day").** The mockup's callout overlaps the headline the same way. It goes away on the next tap.

**Fixed:** none from the screenshots. The only product fix (axis labels and key exposed to accessibility, `82f4c07f`) came from the audit and does not change how the screen looks.

## Not run

- The full UI suite (only the subset above, by instruction).
- A screenshot of Month (Week and All were the states asked for).
- The AX5 Ukrainian capture, the Growth chart with two readings and the range-switching UI test, listed here at first, were done in the final fix wave below.

## Final fix wave (after the whole-branch review)

Commits `f056f4b4` (History consumes its focus), `8498564e` (growth chart accessibility, rate, age, AX axis), `85dc7218` (spoken units, one-day wording, AX headline and chart height) and `3a0600ca` (UI test).

### UI tests

`GrowthFlowUITests/testInsightsGrowthSectionOpensCheckInsInHistory` now also:
- switches Week to All and checks the Feeding headline's label goes from "… Average of 2 days." to "… Since …";
- after landing in History filtered to Growth, clears the filter, adds a second baby from History's switcher and checks that baby's History has no filter chip.

Negative control: with RootView's `onFocusHandled: { historyFocus = nil }` removed, the test fails at the last step ("The Growth focus came back for the second baby", log `work/final-neg.log`). Restored afterwards.

Run `work/final-run4.log`: `testInsightsGrowthSectionOpensCheckInsInHistory` passed (33.9 s), `AccessibilityAuditTests/testAuditInsights` passed (11.5 s), `** TEST SUCCEEDED **`.

### Ukrainian at AX5 (UICTContentSizeCategoryAccessibilityXXXL)

Captured by a temporary UI test (deleted, never committed) with `-UIPreferredContentSizeCategoryName UICTContentSizeCategoryAccessibilityXXXL -AppleLanguages (uk) -AppleLocale uk_UA`.

(a) Week, `-uiTestSeedFixture -uiTestHistorySessions`, scrolled page by page to Growth.
- Before the fixes: `work/final-fit-before/a-00.png` … `a-05-end.png`.
- After: `work/final-fit/a-00.png` … `a-06-end.png`, with the accessibility labels in `work/final-fit/a-labels.txt`.

What the "before" captures showed:
1. **Sleep headline squeezed (defect, fixed).** The unit stayed beside the figure, and "7 год 45 хв" wrapped onto three lines next to "/ день" (`a-02.png`). The feeding headline left "годувань /" above a lone "день" beside the figure (`a-00.png`).
   - Fix: at accessibility sizes the unit gets its own line under the icon and figure (`InsightSectionCard.headline`). After the fix, "7 год / 45 хв" takes two lines with "/ день" below it.
2. **Diapers gridline labels "4" and "2" touching (defect, fixed).** The chart was a fixed 56 pt, so its two labels, half the height apart, collided at AX5 (`a-04.png`).
   - Fix: chart height = max(56 or 84, 2.4 × one scaled footnote line). There is no change at the default size.
3. Rows (label above value), the split row, the sleep key, the axis end labels "вт, 29 … Сьогодні" and the range picker ("Тиждень / Місяць / Усе", 49 pt high segments) all fit, with no clipping.
4. The growth chips stack vertically and the header reads "5 жовт. · 9 д" (age in days, new).
5. Not changed: "годувань / день" on its own line still wraps after the slash at AX5. The text wraps and nothing is clipped.

(b) Growth chart with two weight readings, AX5 Ukrainian.
- The second check-in was logged through Home's Growth form at the default size, then the app was relaunched at AX5 with no reset. At AX5 the decimal pad covers the form's Save button, so the form can't be saved there. That is a pre-existing measurement-sheet issue outside Insights.
- Before the axis fix: `work/final-fit-run3/b-05-chart.png` and `b-06-end.png`. The x-axis labels were truncated to "5 жо… 6 жо… 6…" (defect, fixed).
  - Fix: at accessibility sizes, two marks with a short day-month format and `.greedy` collision resolution, so a colliding label is dropped rather than truncated.
- After the fix: `work/final-fit/b-05-chart.png` and `b-06-end.png` show "5 жовт." under the axis with nothing cut off. The change line "+300 г з 5 жовт. · ≈ 239 г / день", the band, the median and the line all render.
- Chart accessibility (`work/final-fit/b-chart-elements.txt`): exactly two elements, the two PointMarks ("5 жовт. 2026 р., 16:00 / 4,2 кг. Оцінений … 91" and "6 жовт. …, 22:06 / 4,5 кг. … >97-го"). The band, median, line and rule no longer appear.

Default size, English, for comparison: `work/final-fit-before/c-growth-en.png` shows the chart with band, median and two points, the header "Oct 6 · 10d", and "+300 g since Oct 5 · ≈ 242 g / day".

### VoiceOver wording

`a-labels.txt` has the Feeding headline as "Годування: 3 годувань на день. Середнє за 2 дн.", with the spoken unit and no slash.
- Remaining: "3 годувань" is the plural form for 5+. The unit is one catalog string with no plural variation, at both the visual and the spoken level. This predates this wave.

## Merge and phone install (2026-10-06)

- `feat/insights-redesign` fast-forwarded onto `main` at `7a9294fe` (main had not moved since `73e27864`). `make test` on that exact tree: `OK: 1612 tests`.
- The user chose to install main as-is. That also ships everything merged since the last recorded phone install (`1f1ca19`, 2026-10-03), including the tool-calling Ask and the family Live Activity push client with its `aps-environment` entitlement, ahead of that feature's own rollout runbook. The push client registers only with the built-in server when its live `GET /v1/server` advertises the push capability (`LiveActivityPushEligibility.isBuiltInPushServer`); the production server has not been upgraded to schema 28, so no registration is expected. Not observed on a phone.
- Signed Debug `generic/platform=iOS` build from the clean worktree at `7a9294fe`: `BUILD SUCCEEDED` (log `/tmp/bt-insights-phone-build.log`); `codesign --verify --deep --strict` passed; team `TEAM123456`, `aps-environment` `development`; app executable SHA-256 `a55c2e9e3bfb065b925039f40ea340a9c2dad406a5c2a6ad6dd02e8ded759f91`.
- `devicectl device install app` exited 0 on `Phone A` and `Phone B` (in place, no uninstall, local data kept); installed-app queries list `com.example.babytracker` 1.0 (1) on both. Neither app was launched.
- Server untouched: this branch changes nothing under `Server/`.
