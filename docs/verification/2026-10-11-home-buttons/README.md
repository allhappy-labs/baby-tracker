# Arrangeable Home buttons — verification (2026-10-11)

Branch `home-buttons`. Spec `docs/superpowers/specs/2026-10-11-home-buttons-order-design.md`.

## Automated

- `make test`: OK, 1797 package tests (baseline 1755, +42).
- Simulator build (`iPhone 17 Pro`, shared device): succeeded.
- UI tests, run by class on the shared simulator, all passed:
  - `LoggingFlowTests` (9, four new: bottle-first order, contents on the amount screen, a widget link opening its panel, a widget link for an unknown baby);
  - `SettingsFlowTests` (3, one new: Home buttons lists the order and Reset restores Left and Right);
  - `LiveActivityPushUITests` (2), re-run after the pending-prompt change;
  - `RunningTimerLifecycleTests`, `CompositeFeedFlowTests`, `GrowthFlowUITests`, `ReadOnlyCaregiverUITests`, `AccessibilityAuditTests`, `EditFlowTests`, `WidgetTourTests`.

## Screenshots

Taken from the simulator with launch arguments; `settings-list.png` is the attachment of the settings UI test.

| File | State |
| --- | --- |
| `default-idle.png` | Default order: unchanged Home |
| `default-more.png` | Default order: More sheet, with the new "Edit Home buttons" row |
| `bottle-first-idle.png` | Bottle first: one wide Bottle button; Breast, Sleep, Diaper tiles |
| `bottle-first-sleep.png` | Bottle first, sleep running: Wake up; Feed, Diaper |
| `bottle-first-feed.png` | Bottle first, feed running: Stop; Diaper |
| `solids-first-idle.png` | Solids first: Solids; Meds, Growth, Diaper tiles |
| `solids-first-more.png` | Solids first: Breast, Bottle, Sleep and Past entry in More; no med chips |
| `amount-contents.png` | Amount screen after Stop: contents control, previous bottle's amount |
| `bottle-first-idle-uk-ax.png` | Ukrainian at the largest accessibility size: the dock scrolls with the content, as before |
| `settings-list.png` | Settings → Tracking → Home buttons |

## Needs a phone

The simulator cannot place or tap a Home Screen widget from the command line, so these were not run:

1. Default order: the Quick Log widget shows L · R · Bottle · Sleep.
2. Tap Bottle on the widget: the app does not open; a bottle is running when the app is opened by hand.
3. Change the order in the app: the widget follows within a few seconds.
4. Tap Solids on the widget: the app opens on Home with the solids panel up.
5. Force-quit the app, then tap Growth on the widget: the app cold-launches into the growth panel.
6. Delete a baby that a widget is set to, then tap one of its tiles: nothing is logged for another baby.

What stands in for 4 and 5 until then: `LogLinkTests` (the link's format), `testAWidgetLinkOpensItsPanel` (the app's routing for the seeded baby, through a test-only launch argument) and `testAWidgetLinkForAnUnknownBabyOnlyOpensHome` (a real `babytracker://log` link through the system).
