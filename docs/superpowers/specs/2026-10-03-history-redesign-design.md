# History redesign — design

Date: 2026-10-03. Second redesign sub-project after Home & tracking ([spec](2026-10-01-home-tracking-redesign-design.md)). Mockups: `mockups/2026-10-03-history-redesign/` (README lists the chosen options). The spec wins if a mockup disagrees.

## Intent

The user opens History mostly to **look back** (when was the last feed or poop, how long were the naps) and to **fix things** (edit a time, delete a mistake, combine or split sessions). Pattern-at-a-glance views belong to Insights.

Same goals as the Home redesign: no unnecessary text or labels, the approved line icon set and tracker colours, times-only copy, one-handed use, and sessions shown as one unit. The data model and the sync protocol are unchanged.

Success means four things:
- A day reads at a glance, with one line per feed or sleep session.
- Every correction is one tap away, with no ⋯ buttons on the list.
- Join and Split are obvious, and happen where the gap between parts is.
- Everything fits in Ukrainian at the largest accessibility text size.

## 1. List screen

- **Header.** The baby's name only, with the same baby switcher as Home. A round filter button sits at the top right.
  - While a filter is on, the button shows a dot, and a pinned chip ("Showing: Feeds ✕") clears the filter in one tap.
  - The "HISTORY" eyebrow and the chip filter bar are removed.
- **Filter sheet.** A small sheet with one icon toggle per tracker: Feeds, Sleep, Diapers, Meds, Growth. All are on by default.
- **Days.** One endless scroll, newest first.
  - Day headers ("Today", "Yesterday", "Wed 30 Sep") stay pinned while scrolling.
  - Each header shows icon totals: feeds (joined sessions counted once), sleep total, and diapers. A session counts on the day its first part started.
  - There is no date picker, and the per-day text summary line is removed.
- **Rows** (mockup layout **A2**, one row per session or entry):
  - Left column: the start time, with the end time in smaller grey text beneath it (for entries that have one). Then the tracker icon.
  - Feed (single or session): the parts as chips, e.g. `L 12m` `R 15m` `60 ml` (bottle volume in the user's unit; solids shown by their foods). A session row shows all its parts.
  - Sleep (single or session): the asleep total, plus `· 1 wake` / `· 2 wakes` when joined.
  - Diaper: Wet / Dirty / Mixed. Med: the med's name. Growth: the measurement value(s).
  - No title words such as "Feeding session", "Diaper" or "Med" (the icon says it), and no ⋯ buttons.
- **Running entries.** A running feed or sleep appears at the top of today with `now` as its end and a live duration. Tapping it opens the existing running-timer editor.
- **Gestures.**
  - **Tap** any row to open its sheet (sections 2–3).
  - **Swipe left** to delete. Single entries delete after the swipe action. Session rows delete the whole session after a confirmation.
- **Empty states.** No data: one line with the icon. Filtered to nothing: "Nothing matches" with **Clear filter**.
- **Entry from Home.** Tapping Home's Today strip still opens History at today (`HistoryFocus`).

## 2. Session sheet

Opened by tapping a feed or sleep row with more than one part. Layout **S2/G2**, with no scissors or dashed lines.

- **Header.** `10:23 – 11:48`, then a totals line (duration · breast time · bottle volume; for sleep: asleep time · wakes). **Done** is at the top right.
- **Join row.** Shown when an earlier session of the same kind exists for the baby: "Join the 07:30 feed · 2h 8m before".
  - Tapping joins at once, with no confirmation. Split undoes it.
  - Manual joining ignores the break limit and may cross a sleep (Home ruling R27).
- **Parts list.**
  - Each part is a row with its icon, label (Left / Right / bottle contents and volume / sleep duration) and start time · duration.
  - Tapping a part slides its editor in inside the sheet, with a back arrow. Save returns to the session.
  - Swiping a part deletes it. Deleting the last part deletes the session.
- **Between parts.** `52m later` in grey on the left, **Split here** on the right.
  - Split starts a new session at that part, and every later part goes with it. This is the existing `separateFeed(at:)` / `separateSleep(at:)` behaviour.
- **Add part.**
  - A small menu below the parts: Left, Right, Bottle for feeds; Sleep for sleeps. Solids never join, so they are not offered.
  - The new part starts at the session's end, joins the session, and its editor opens.
- **Delete feed / Delete sleep** at the bottom, with a confirmation.

## 3. Editing single entries

Every kind opens the sheet used to log it from Home, in **edit mode**:

| Entry | Sheet |
|---|---|
| Diaper | Diaper sheet: Wet / Dirty / Mixed, Details accordion with the note, time |
| Bottle (alone or a session part) | Bottle sheet: amount chips with unit, contents, time |
| Breast part | Past entry layout: side, start, duration |
| Sleep (alone or a session part) | Past entry layout: start, end |
| Solids | Solids sheet |
| Med | Meds sheet: med, time |
| Growth | Growth sheet |

How edit mode works:
- Fields start with the entry's values, the main button reads **Save**, and **Delete** is at the bottom.
- Feed parts and sleeps opened on their own (not inside a session) also show the **Join row**, so a lone bottle can join the feed before it.
- Validation is the same as Home's. A part may not start before the previous part of its session ends, and that refusal shows its own message (the redesign fix-wave rule I2).
- Saves are ordinary updates that sync to the other phone.

The generic editor (`EntryEditSheet`, `EntryEditModel`) is deleted once every kind has its edit mode.

## 4. Architecture

- **Presentation** follows Home's ruling R4.
  - HistoryFeature emits a `HistoryAction`: `.edit(entryID, kind)` or `.session(rootID, kind)`.
  - `RootView` maps the action to a LoggingFeature `LogSheet`: new cases `.edit(…)` and `.session(…)`.
  - HistoryFeature does not import LoggingFeature.
  - LoggingFeature owns all logging and editing sheets, including the new session sheet.
- **Store.** Reuse `combineFeeds` / `separateFeed`, `combineSleeps` / `separateSleep`, `logBreast`, `logBottle` and `logSleep`, and the part-time validation. Add two calls:
  - `addPart(_:to sessionRootID:)`, which logs a part into an existing session;
  - `deleteSession(rootID:kind:)`, which tombstones every member in one change.
- **HistoryViewModel** is rebuilt around the A2 row model.
  - Day totals come from the same session projection Home and Insights use (`FeedingSessions` / `SleepSessions`), so the numbers match everywhere.
  - The duplicated feed and sleep row builders noted in the redesign review go away.
  - The snapshot rebuilds on data change (store revision), not on a timer. Running rows tick with `TimelineView`, as on Home.
- **Removed:** the ⋯ menus (`feedMenu` / `sleepMenu`), the chip filter bar, `EntryEditSheet` and `EntryEditModel`.

## 5. Testing

- **Unit tests:**
  - rows and day totals, including cross-midnight sessions, running entries, filters and empty states;
  - the session sheet model: join, split, add part, delete part and delete session, including deleting the last part;
  - each edit sheet's prefill and save;
  - the part-time validation message.
- **UI tests on the shared simulator** (`platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853`):
  - History → tap a session → Split here → Join the earlier feed;
  - swipe to delete, for a single entry and for a session;
  - the filter, and clearing it;
  - each edit sheet opened from History.
- **Accessibility audits** on the list, the session sheet and each edit mode.
- **Ukrainian at XXXL** layout smoke.
- **Localization gate:** no new problems against the main baseline.
- **`make server-protocol-e2e`:** a History-style edit (split, join, add part) reaches the other caregiver.

## Out of scope

- Insights/Growth, Ask, and Settings/Caregivers/Export layouts. These are later sub-projects.
- Date picker or calendar jump, and search.
- Any change to the data model, the sync protocol or the server.

## Amendments from the code survey (2026-10-03)

A read of the code found gaps the design assumed away. Each decision keeps the approved intent. The plan's rulings H-R1 to H-R10 hold the detail.

- **Growth rows are new** (H-R1). Growth isn't a History entry today.
  - History merges `store.measurements(for:)` by `measuredAt` and adds a Growth filter toggle.
  - A Growth row opens the existing Growth sheet's edit mode, which gains Delete.
- **A part is a component** (H-R2). Older feeds hold several components in one entry (for example L, R and a bottle).
  - The session sheet lists the components of every member entry.
  - "Split here" appears only between parts that belong to different entries. Between components of the same entry, only the gap shows.
  - Deleting a component removes it from its entry. Deleting an entry's last component deletes the entry.
- **No field is lost** (H-R3). The old editor was the only way to reach some fields, so the edit modes gain them:
  - the Diaper sheet in edit mode adds **Dry** as a fourth choice;
  - the sleep editor gets Nap / Night / Auto;
  - feed part and sleep editors get a **Note** in a Details accordion, as on the Diaper sheet;
  - part start and end times are editable in the part editors.
- **Completed parts get the previous-part check** (H-R4). It now covers completed parts, not only running timers, and shows the same message: "The start can't be before the previous part ended, at %@."
- **Running rows** (H-R5). Tapping one opens Home's running-timer editor, made public and presented by `RootView`.
- **View-only caregivers** (H-R6) can read History, but rows aren't tappable or swipeable.
- **Edit-mode time rows include the date** (H-R7).
- **Day titles** are "Today", "Yesterday", then the short date (H-R10).
