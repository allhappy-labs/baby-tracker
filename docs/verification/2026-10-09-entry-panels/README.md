# Entry panels: verification (2026-10-09)

Branch `entry-panels`. Simulator: shared iPhone 17 Pro (`D17101C9-…`, 402×874 pt). Screenshots are scaled to 1x (402 px wide).

## How the screenshots were taken

A temporary UI test class, which was not committed, opened each panel through the real UI. History panels used `-uiTestHistorySessions`, new moments used `-uiTestSeedBaby`, and Past entry used More → Past entry. It saved `XCUIScreen` PNGs. The accessibility set used the harness launch argument `UICTContentSizeCategoryAccessibilityL` (accessibility-large), so the simulator's global text size stayed at `large`.

The running-timer editor with finished parts needed a temporary launch fixture, also not committed. No current UI path leaves a running timer with completed components, because every part is its own timer. The fixture gave a running Right with a finished Left (9m) and a 60 ml bottle (5m) in the same timer.

Files are named `<panel>-<state>.png`, with `-a11y` added for accessibility-large.

## Default text size

| Panel | Collapsed | Expanded | Notes |
|---|---|---|---|
| Past entry, breast | `pastentry-breast-collapsed` | `-expanded` | Fits. About 9 pt between Details and Save. Expanding grows the sheet by one row. |
| Past entry, bottle, no End | `pastentry-bottle-noend-collapsed` | `-expanded` | **Fits after `1334ff0b`.** Before it, Details sat under Save. Expanded goes to full height and scrolls. |
| Past entry, bottle with End | `pastentry-bottle-end-collapsed` | `-expanded` | Full height. Content is longer than the screen, so it scrolls and nothing is cut off out of reach. |
| Past entry, sleep | `pastentry-sleep-collapsed` | `-expanded` | Fits. Nap/Night/Auto, Start/End/Length. |
| History, session (feed) | `history-session-feed` | — | Fits. Parts read start–end. |
| History, breast (pushed) | `history-breast-collapsed` | `-expanded` | Fits, with Save and Delete. |
| History, bottle (pushed) | `history-bottle-collapsed` | `-expanded` | Collapsed fits; expanded goes to full height. |
| History, session (sleep) | `history-session-sleep` | — | Fits. |
| History, sleep (pushed) | `history-sleep-collapsed` | `-expanded` | Fits. |
| History, diaper | `history-diaper-collapsed` | `-expanded` | Fits. Dry is offered in edit mode. |
| History, solids | `history-solids-collapsed` | `-expanded` | Fits. |
| History, supplement | `history-supplement-collapsed` | `-expanded` | Fits. |
| History, measurement | `history-measurement` | — | Fits. No Details section. |
| Running-timer editor | `running-editor` | `running-editor-scrolled` | Finished parts show Start, End and a read-only Length. The running side shows Start and "Running". The content is taller than the screen, so it scrolls. |
| New diaper | `new-diaper-collapsed` | `-expanded` | Fits. Tap to save, with no footer. |
| New solids | `new-solids-collapsed` | `-expanded` | Fits. |
| New supplement | `new-supplement-collapsed` | `-expanded` | Fits. |
| New measurement | `new-measurement` | — | Fits. |

I found no Duration stepper anywhere. The only stepper is the running editor's bottle volume (ml). Collapsing Details shrinks every fitted sheet back. Where content fits, I saw no empty band above the footer or under the last row.

## Accessibility-large

Every panel goes to full height (`.large`) and scrolls. Rows wrap: the date and time chips stack under their label, and bottle chips sit two per row with − and + on their own row. Nothing is cut off out of reach. Footer-less sheets such as New diaper leave space under Details, because the sheet is full height by design at these sizes.

The Past entry bottle and sleep shots at this size are missing. The temporary test could not scroll the kind picker back into reach. History → bottle and History → sleep cover the same panel.

## Keyboard

`keyboard-pastentry-note`, `keyboard-pastentry-typed` and `keyboard-solids-note` were taken with Details open and the note field focused. The sheet goes to full height, and the footer rides on the keyboard. The note field stays visible just above Save, and typing does not move or resize anything, so there is no loop. No code change was needed. I did not get a keyboard shot at accessibility sizes.

## Commands

- `swift test --filter LoggingFeatureTests` (after `1334ff0b`): 191 tests in 14 suites passed.
- UI subset: `xcodebuild test -scheme BabyTracker -destination 'platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853' -parallel-testing-enabled NO -only-testing:BabyTrackerUITests/{EditFlowTests,LoggingFlowTests,RunningTimerLifecycleTests,GrowthFlowUITests,AccessibilityAuditTests}` → `Executed 19 tests, with 0 failures`, `** TEST SUCCEEDED **`, no runner restarts.
- `make test` → `OK: 1696 tests`.
