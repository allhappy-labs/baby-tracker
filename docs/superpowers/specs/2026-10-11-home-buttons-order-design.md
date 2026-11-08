# Arrangeable Home buttons, and bottle defaults — design

Date: 2026-10-11.

Related:
- [Home and tracking redesign](2026-10-01-home-tracking-redesign-design.md): the dock, the ··· sheet and the Quick Log widget this changes.
- [Entry panels](2026-10-09-entry-panels-design.md): the panels the dock opens.
- [Pumping](2026-10-09-pumping-design.md): Pump joins the order when it ships.

## Intent

Home's dock is fixed. When nothing runs it shows two large buttons, **L** and **R**, with Bottle, Sleep, Diaper and ··· as small tiles. That suits a breastfed newborn. It will not suit the same baby on bottles or solids, and it does not suit a caregiver who only gives bottles.

Decisions made with the user:
- **The caregiver chooses.** The app never moves a button on its own and never suggests a change.
- **One ordered list** of actions decides the big button, the small tiles and what lives in ···.
- **An order belongs to one baby on one phone.** Two caregivers may arrange the same baby differently. Nothing syncs.
- **The Quick Log widget follows the same order.**
- **A bottle started from the widget never opens the app.**
- **Bottle contents can be changed on the screen where the amount is entered.**
- **The default bottle amount is the previous bottle's amount.**
- **The Bottle button on Home still opens the bottle panel**, also when it is the big button.

Success means:
- A caregiver can make any action the big button and choose the three tiles beside it, per baby.
- With the default order, every Home state looks and behaves exactly as it does today. The widget's default changes once, as section 5 explains.
- The widget shows the first actions of its baby's order, and Bottle starts there without opening the app.
- The amount screen after a bottle offers contents, and starts from the previous bottle's amount.
- No server change and no deploy.

## 1. The order

**Actions.** `breast`, `bottle`, `sleep`, `diaper`, `solids`, `meds`, `growth`. A new `HomeActionKind` enum in BabyCore, with raw string values that are stored.

**Default.** Breast, Bottle, Sleep, Diaper, Solids, Meds, Growth. This reproduces today's dock, ··· sheet and widget.

**Positions.**

| Position | Role |
| --- | --- |
| 1 | Big button |
| 2–4 | Small tiles |
| 5 and later | In the ··· sheet |

··· is always the last tile and is not part of the order. Past entry always lives in ··· and is not part of the order.

**Storage.** `TrackingPreferences` gains the order, per baby, in the App Group defaults (key `tracking.homeOrder.<babyID>`, an array of raw values). The widget and intents read it there, as they read the break limits.

- **Reading** returns a complete order every time: stored kinds in their stored sequence, unknown raw values dropped, duplicates dropped, and any kind missing from the stored value appended in default order. A kind added by a later version therefore lands in ···, and a value written by a later version reads safely.
- **No stored value** means the default.
- **Reset** removes the stored value.
- **Deleting a baby** on this phone removes its key. A baby deleted from another phone leaves an unused key behind, which nothing reads.
**Pump.** When pumping ships it adds `pump` to the end of the default order. The reading rule puts it in ··· for every baby with a stored order, which is where the pumping spec starts it. If pumping lands first, this work adds `pump` to the enum instead.

## 2. Dock

One pure function, `HomeLayout`, turns an order and the timer state into what the dock, the ··· sheet and the widget show. The views render its result. `Dock.start` carries the resolved big action and tiles instead of only the suggested side.

### Idle or in a break

- **Big button.**
  - Breast: **L** and **R** side by side, the suggested side outlined, as today.
  - Any other action: one full-width button of the same height, with the action's icon and name on its tint.
- **Tiles.** Positions 2–4 as icon-only tiles with spoken labels, then ···.

**What a tap does**, in any position:

| Action | Tap |
| --- | --- |
| Breast (big) | L or R starts that side |
| Breast (tile or ···) | Opens an L / R chooser |
| Bottle | Opens the bottle panel |
| Sleep | Starts a sleep |
| Diaper | Opens the diaper panel |
| Solids | Opens the solids panel |
| Meds | Opens a small Meds panel: the saved-med chips and **+** |
| Growth | Opens the growth panel |

### While a timer runs

Stop and Wake up stay the one big button. The tiles come from the dock's four actions (big and tiles) in order, with the conflicting ones removed.

- **Feed running:** **Stop**, then the dock's actions without Breast, Bottle and Sleep, then ···.
- **Sleep running:** **Wake up**, then **Feed** if Breast or Bottle is on the dock, then the dock's actions without Breast, Bottle and Sleep, then ···. Feed opens the existing L / R / Bottle chooser.
- **At most three tiles** come before ···, so the row never holds more than four.
- **Both running:** the feed state, as today.
- **Read-only caregivers** see no dock, as today.

With the default order these are Stop · Diaper · ··· and Wake up · Feed · Diaper · ···, which is today's dock.

## 3. The ··· sheet

- A tile for every action at position 5 or later, in order, then **Past entry**.
- The saved-med chips with **+** appear in the sheet only while Meds is at position 5 or later. When Meds is on the dock, its own panel carries them.
- While a timer runs, the sheet leaves out the actions that conflict with it: Breast, Bottle and Sleep during a feed, Sleep during a sleep.
- A docked action that the running dock has no tile left for is shown in the sheet first, so nothing is out of reach while a timer runs.
- A new last row, **Edit Home buttons**, opens the settings screen for the current baby.
- Tiles keep the existing layout rule: a row at ordinary sizes, stacked at accessibility sizes. More than three tiles wrap to a second row.

With the default order the sheet shows Solids, Growth, Past entry and the med chips, as today, plus the new row.

## 4. Settings

**Settings → Tracking → Home buttons**, for the current baby, with the baby's name in the title.

- One list of the seven actions, each with its icon and name, reordered by dragging. The list is always in edit mode, so the drag handles are always shown.
- Three position labels inside the list, which do not move: **Big button** above row 1, **Tiles** above row 2, **In More (···)** above row 5.
- A footer line under Big button when Breast is first: "Shown as Left and Right."
- **Reset to default** below the list, disabled when the order is the default.
- Every change saves at once, refreshes Home and reloads the widget timelines.
- VoiceOver gets the standard move-up and move-down actions on each row.

## 5. Quick Log widget

The widget reads the order of its configured baby and fills four slots by walking it:

- Breast takes two slots, **L** and **R**. Every other action takes one.
- An action that does not fit the slots left is skipped, and the walk continues.

| Action | Tap |
| --- | --- |
| L, R | Starts that side, as today |
| Sleep | Toggles the sleep, as today |
| Diaper | Logs a wet diaper, as today |
| Bottle | Starts a timed bottle with `StartBottleIntent`. Never opens the app |
| Solids, Meds, Growth | Opens the app at that panel for that baby |

- A bottle started from the widget uses the last contents, shows the feed Live Activity, and is stopped from that card or from Home. Its amount and contents are set on the amount screen (section 6).
- Solids, Meds and Growth open the app through a new `babytracker://log?kind=<kind>&baby=<id>` link, routed by the existing URL router beside the invite link. An unknown kind, a baby no longer on this phone, a caregiver who may not log, or a panel, sheet or alert already on screen: the link opens Home and nothing else.
- The widget's description changes to match: it no longer names a fixed set of actions.

With the default order the widget shows L · R · Bottle · Sleep. **This differs from today's L · R · Sleep · Diaper**: Diaper moves off the widget unless the caregiver moves it above Bottle or Sleep. This is the only place the default order changes what is shown, and it follows from one order driving both surfaces.

## 6. Bottles

### Contents on the amount screen

The bottle panel's amount body is shown after Stop in the app (`.stopped`) and for **Set amount** after a Lock Screen stop (`.amount`). It gains the Breast milk / Formula / Mixed control above the amount.

- **Preselected** to the bottle's own contents: the running timer's contents for `.stopped`, the saved bottle part's contents for `.amount`.
- **Saved with the amount** in one write. `stopBottle` takes the contents; `setBottleVolume(_:onFeed:)` becomes a call that sets volume and contents together.
- The start body and the edit panel already have the control and do not change.

### Default amount

`LogDefaults.mostUsedBottleVolumeML` is replaced by `lastBottleVolumeML`: the amount of the baby's most recent bottle part, read the same way as `lastBottleContents`. With no earlier bottle, or when the most recent bottle was saved at 0 ml, it is 120 ml.

It applies everywhere the old default did:
- the amount screen and the bottle panel;
- a bottle part added in the session sheet or the part form;
- the amount saved when a bottle is stopped outside the app (Lock Screen Stop, or L / R stopping it).

## 7. Unchanged

- Hero, status line, Today strip.
- Live Activity cards and their controls, Siri and App Shortcuts.
- The Status widget.
- Sync, protocol and server. No migration and no deploy.
- The timer model, joining rules and every entry panel other than the bottle amount body.

## 8. Testing

- **Order storage (BabyStore):**
  - no stored value reads as the default;
  - a stored order round-trips per baby and does not leak to another baby;
  - unknown and duplicate values are dropped, missing kinds are appended in default order;
  - reset and baby deletion remove the key.
- **Layout (pure function):**
  - the default order gives today's dock in idle, feed-running, sleep-running and both-running states;
  - a non-breast first action gives one big button and three tiles;
  - Breast as a tile and Breast in ···;
  - running-state tiles for several orders, including an order with no Breast or Bottle on the dock (no Feed tile during sleep);
  - ··· contents for several orders, and med chips only when Meds is off the dock;
  - widget slots: Breast taking two, an action skipped when it does not fit, the default order.
- **Bottles:**
  - `lastBottleVolumeML` returns the previous bottle's amount, the last part of a multi-bottle feed, and 120 ml with no history;
  - the amount screen preselects the bottle's contents, and saving writes contents and volume together for both `.stopped` and `.amount`;
  - a bottle stopped outside the app saves the previous amount.
- **Link routing:** `babytracker://log` with each kind, an unknown kind, and an unknown baby.
- **UI tests (a small subset, logic only):**
  - move Bottle first in Settings, and Home's big button opens the bottle panel while Breast is reachable as a tile;
  - stop a timed bottle, change contents on the amount screen, and History shows the new contents.
- **Own end-to-end checks before any phone testing:**
  - simulator screenshots of Bottle-first and Solids-first docks in idle, feed-running and sleep-running states, the ··· sheet, the settings list, and the amount screen, including forced Ukrainian at the largest accessibility text size;
  - the widget with the default order and with a reordered one, a widget Bottle tap starting a timer without opening the app, and a widget Solids tap opening the solids panel.
- `EXPECTED_TESTS` updated with the real new total.

## Out of scope

- Automatic reordering, suggestions and age-stage presets.
- Syncing an order between caregivers or phones.
- Hiding an action entirely: everything stays reachable through ···.
- Arranging the Live Activity, Siri or the Status widget.
- One-tap Start bottle from Home's dock.
