# Entry panels: one layout for logging and editing

Date: 2026-10-09
Status: approved in brainstorm, awaiting spec review

## Problem

- Some activities with a length are edited as Start + Duration (breast parts in
  `PartEditSheet`, finished parts in `RunningTimerEditSheet`), others as
  Start + End (sleep, `PastEntrySheet`). The user wants Start + End everywhere
  an activity has a length.
- Logging and editing the same activity use different panels: `PastEntrySheet`
  versus `PartEditSheet`; hour-only `DatePicker` when logging a diaper, solids
  or supplement versus `EditTimeRow` (date and time) when editing one.
- Panels carry empty space. Heights are hand-tuned constants per kind
  (`PartEditSheet` 460/650 + extras, `DiaperSheet` 300/470 + extras) fed to
  `sheetHeight(_:)`, so a collapsed Details or a shorter form still gets the
  tall sheet.

## Goals

1. Every activity with a length is edited as **Start + End**. No duration
   stepper anywhere.
2. Each activity has **one panel** used for logging new entries and for
   editing from History. The running-timer editor uses the same time rows.
3. Sheets **fit their content**. Collapsing Details or removing an End row
   shrinks the sheet.

Out of scope: schema or sync changes, new activity kinds, History list layout.

## Activity groups

| Group | Activities |
|---|---|
| Interval | Breast side, sleep, bottle (End optional) |
| Moment | Diaper, supplement, solids, measurement |

Bottle starts as a timer by default but may also be logged at a moment, so its
End is optional.

## Panel layout

Every panel, new or editing:

1. Navigation bar: Cancel, title with the activity icon.
2. The activity's own fields.
3. Time section.
4. Details (collapsed unless it already holds content).
5. Footer: **Save** when new; **Save + Delete** when editing.
6. Height fits the content.

### Time section

**Interval:** Start row and End row, both date + time (`EditTimeRow`). A
read-only length ("18 min") appears with the section. End states:

- set: date + time picker;
- optional and unset (bottle only): an "Add end time" button; once set, a
  clear (✕) control returns it to unset;
- running (live part in the timer editor): reads "Running", not editable.

Validation (inline message, Save disabled): End before Start; Start or End
after now; End missing where required.

**Moment:** one Time row, date + time, identical in new and edit modes. This
replaces the hour-only pickers, so a moment can be logged on an earlier day.

### Per activity

| Activity | Own fields | Time | Details |
|---|---|---|---|
| Breast side | Left / Right | Start, End | Note |
| Bottle | Amount, contents | Start, optional End | Note |
| Sleep | Nap / Night / Auto | Start, End | Note |
| Diaper | Wet / Dirty / Mixed / Dry | Time | Existing detail fields |
| Solids | Foods, amount | Time | Note (moved from always-visible) |
| Supplement | Med (pick or new name) | Time | Note (moved from always-visible) |
| Measurement | Weight, length + position, head | Time | — (no Details; none today) |

- Diaper keeps tap-to-save in new mode: choosing a contents option saves with
  the Time row's value. In edit mode the choice selects and Save commits.
- The Join row stays where it is today (standalone part editor, session sheet).
- Multi-part feed sessions keep their session list; each part row reads e.g.
  "Left · 08:02–08:20", and tapping it pushes the same part panel.

### Running-timer editor

- Start row is the shared Start row, keeping the ±5 min nudges.
- Each finished part (breast side, bottle) shows Start + End rows instead of
  a duration or volume-only stepper; bottle keeps its amount fields.
- The live part shows Start and an End reading "Running".

## Components

All in `LoggingFeature` unless noted.

- **`TimeSpan`** — value type with no UI: `start`, `end: Date?`,
  `endRequirement` (required / optional / running), `latestAllowed`. It
  produces the length text, the validation message, and the start/end/seconds
  to save. Pure, unit-tested.
- **`TimeSpanSection`** — renders a `Binding<TimeSpan>` as the Start/End
  rows with the three End states and the validation footer.
- **`EntryPanel`** — the shell: `NavigationStack` (when standalone), `Form`
  with caller-supplied field sections, the time section, the Details
  disclosure, the `editFooter`, `editTitle`, `editLifecycle`, and
  `.fittedSheetHeight()`. Mode: `.new` or `.edit`.
- **`.fittedSheetHeight()`** (`BabyUI`, replaces `sheetHeight(_:)`) — reads
  the form's content height with `onScrollGeometryChange` (content size does
  not depend on the sheet's height, so the detent change does not feed back),
  adds the footer/safe-area chrome, updates the detent only when the height
  changes by more than a point, and caps at `.large` (the form scrolls past
  that). Accessibility text sizes keep `.large`. Non-`Form` panels (diaper's
  `VStack`) are wrapped in a `ScrollView` so they report the same way.

The previous measuring attempt (pre-`05d5ac77`) measured the view inside the
sheet, so each detent change re-proposed a size and re-fired the measurement;
the app never went idle and XCUITest waits hung. Measuring scroll content size
avoids that loop.

### Merged and removed

- `PastEntrySheet` → the part panel (`PartEditSheet`/`PartEditModel`) in
  `.new` mode for breast, bottle and sleep. `LogSheet.pastEntry` routes there.
- Breast duration `Stepper` in `PartEditSheet`; finished-part steppers in
  `RunningTimerEditSheet`.
- Hour-only `DatePicker`s in `DiaperSheet`, `SolidsSheet`, `SupplementSheet`
  new mode; `MeasurementSheet`'s picker becomes the shared Time row.
- All `sheetHeight(<constant>)` call sites and their height arithmetic.

## Data

No schema or sync changes.

- Breast part: edits write `startedAt`, `endedAt` and `seconds = end − start`.
  Old parts with only `seconds` open with End = Start + seconds.
- Bottle: End unset → `seconds`, `endedAt` nil (`startedAt` = Time). End set →
  all three written.
- Sleep: unchanged (`startedAt`, `endedAt`).
- Moments: unchanged single timestamp.
- Running timer: finished parts write start/end/seconds as above; the timer's
  own start keeps today's behaviour.

## Testing

- Package unit tests:
  - `TimeSpan`: length text, every validation case, optional/running ends.
  - Part panel model: new and edit saves for breast, bottle with and without
    End, sleep; legacy breast part (seconds only) round-trips to start/end.
  - Running-timer model: Start/End edits per finished part.
  - Moment panels: new-mode saves keep the chosen date, not just the time.
- UI tests: no geometry assertions. Update only tests whose identifiers break
  (`partEdit.duration`, past-entry flows, running-timer steppers) and run those
  classes with `-only-testing:`.
- Manual check before handing over: simulator screenshots of every panel, new
  and edit, Details collapsed and expanded, default and an accessibility text
  size, checked for empty space and clipped rows.
