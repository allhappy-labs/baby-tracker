# Home and tracking redesign, start/stop timers, and joined sessions

## Intent

The app's features are right; its surfaces are not. Home carries labels that add nothing ("AGO", "Today so far", "Started"), icons are inconsistent (the diaper is a custom shape in the app and `shippingbox.fill` in widgets and menus), and the running feed asks the caregiver to operate Switch, Pause and Resume while holding a baby. Separately, two sleeps a few minutes apart, or two breast sides with a short break, should be one sleep or one feed everywhere. Today feeds join only through a post-Stop dialog inside the app, and sleep never joins.

This is the first of five redesign sub-projects. It covers the shared visual language, Home, the logging sheets, the timer model, session joining, the Live Activity, widgets and Siri. History, Insights/Growth, Ask, and Settings/Caregivers/Export follow one by one with their own specs. Until then they keep their layouts but adopt the new icons and joined sessions, because both are shared data and components.

Success means: every common action is one tap in the thumb zone; a caregiver times each breast, bottle and sleep with plain Start and Stop; short breaks join automatically and identically on every device; and Lock Screen controls work without unlocking.

Approved mockups live in `mockups/2026-10-01-home-redesign/` (see its README). This spec wins where they differ.

## Visual language

**Icons.** One custom line-style set (about 1.8pt stroke, rounded joins), shipped as SF Symbol–compatible custom symbol templates. It replaces raw system symbols on every surface: app, widgets, Live Activity, Dynamic Island, menus and App Shortcuts. `Tracker.symbolName` and every hard-coded `drop.fill`, `waterbottle.fill`, `shippingbox.fill`, `pills.fill`, `fork.knife`, `ruler`, `l.circle.fill` and `r.circle.fill` go through one icon catalogue in BabyUI.

| Item | Glyph |
| --- | --- |
| Left / Right breast | Drop with the letter L or R inside |
| Feed (generic) | Drop |
| Bottle | Bottle with nipple and graduation marks |
| Sleep | Crescent moon with a small star |
| Diaper | Disposable diaper with tape tabs and front patch (mockup "diaper 1") |
| Solids | Bowl with spoon |
| Meds | Capsule plus drop |
| Growth | Baby scale |
| Wet / Dirty / Mixed | Blue drop / brown dot / both marks |

**Colour.** Keep the dark "Night Shift" ground. The palette is feed amber `#F2B061`, sleep periwinkle `#8A9BF0` and diaper teal `#5FD3C4`, plus Meds lavender `#C4A6F5`, Solids terracotta `#E89A7B` and Growth green `#9FD27A`. Tiles use the item colour at about 14% opacity with the glyph in full colour. The primary action (Stop, Wake up, Start bottle, Done) is a light fill with dark text.

**Type.** Rounded bold numerals with monospaced digits, as in today's `numeralStyle`. The hero is the only large number on screen.

**Copy rules.**
- A number beside an activity icon means time since that activity. "ago", "AGO", "Today so far", "Started …", "Last feed", "Slept … before that" and "This side …" are removed from Home.
- Main surfaces show times and amounts. Words remain only where an icon would be ambiguous: Stop, Wake up, Start bottle, Mixed, Dry, Past entry, Details, Done.
- Every icon-only control has a spoken accessibility label and stays at least 44pt.
- All strings remain in the localisation catalogues. Removed strings are deleted, and new ones are translated in every existing catalogue.

**Renames.** Supplements becomes **Meds** in all UI. Data model and protocol names stay. The diaper choice "Both" is shown as **Mixed**, which is the existing `DiaperContents.mixed`.

## Timer model

**One timer, one part.** A breast side, a timed bottle, or a sleep is each its own timer with Start and Stop. Stopping a feed timer saves a `FeedEntry` with exactly one component (left, right, or one bottle). Stopping a sleep timer saves one `SleepEntry`. There is no Switch, Pause or Resume in the new UI.

At most one feed timer and one sleep timer run per baby, and the two may overlap. Starting a feed part while another feed part runs (for example tapping R from the Live Activity while L runs) stops the running part at that instant and starts the new one. That is a safety net, not a Switch control. The existing rules still apply at Stop: breast parts under 30 seconds are discarded, and a zero-duration breast part never persists.

Tapping the hero while a timer runs edits that timer's start time only. The time cannot be in the future and cannot be before the end of the group's previous part.

## Joined sessions

Feeds already link through `FeedEntry.feedingSessionID`, and Home, History, Insights and Ask already count a linked group once. Sleep gains the same link: `SleepEntry.sleepSessionID`, where nil means the sleep is its own session, mirroring `effectiveFeedingSessionID`.

**Rule.** When a timer starts, the app finds the baby's most recent session of that kind (feed or sleep). If that session's last part ended no more than the break limit before the new start, the new timer joins it. Otherwise it starts a new session. The session ID is decided at start and stored on the timer, so the resulting entry carries it no matter which device or surface stops the timer.

- The same rule applies to a bottle logged from the amount chips and to a Past entry, using the entry's start time.
- Diapers, Meds and Growth never block a join. A sleep between two feeds blocks the feed join, as today. Solids never join milk feeds, and milk feeds never join solids.
- A gap exactly equal to the limit joins. One second more does not.
- Default limits: feeds 30 minutes, sleep 15 minutes. Both are adjustable in Settings → Tracking (a minimal row now; the Settings redesign comes later). They are stored per device in the App Group defaults, so widgets and intents read the same values. The resulting session IDs sync, so both caregivers see the same grouping even if their limits differ.
- The "Same feed" / "Same sleep" chip with ✕ appears while a joined timer runs. ✕ gives that timer a new session ID immediately. After Stop, History's **Combine with previous** and **Separate here** correct mistakes.
- History's manual Combine is fixed. It now targets the previous session of the same kind, ignoring diapers, Meds and Growth between them, and it gains sleep support. History keeps its current layout otherwise.
- The post-Stop "Combine with previous feed?" dialog and its BottleSheet counterpart are removed.

**Totals.**
- Feed duration is the sum of its parts. The breaks between parts are shown in a group but never counted as feeding.
- Sleep duration is the sum of its parts. A joined sleep counts as one nap or night, classified by its first part's start. The wakes are shown.
- Bottle volume is the sum of bottle parts, as today.
- A session belongs to the day its first part started.
- "Awake" on Home is measured from the end of the last sleep part.

**Existing data.** Nothing is merged retroactively. Existing multi-component feeds and their session links display and count exactly as before. A timer already running with several components when the app updates continues to its Stop and saves as before.

## Home

The layout follows mockup C: glance on top, dock in the thumb zone, tab bar unchanged. The top-left shows the baby name with ⌄ (the baby switcher, unchanged). The top-right shows the baby's age.

**Hero** (one large number) shows the most relevant state, in this order:
1. A feed timer runs: the part's glyph and name (Left, Right, Bottle), its elapsed time, and "since 3:02". A joined timer also shows the chip and a thin bar of the group's parts and breaks.
2. A sleep timer runs: moon, elapsed time, "since 2:05", and the chip and bar when joined.
3. A feed session is in its break window: "Feed · break", break elapsed, and the parts so far (for example L 9:02).
4. A sleep session is in its break window: "Awake", elapsed since the last part.
5. Otherwise: "Awake" with time since the last sleep ended.

When a feed and a sleep both run, the feed is the hero and the sleep appears as a small live chip.

**Status line.** Directly under the hero, in every state, a small secondary row shows time since for the activities that are not the hero, using icons and numbers only: last feed, sleep or awake, and last diaper. The style is the same in every state.

**Today strip.** Shown in every state, including while a feed or sleep runs (changed 2026-10-04; mockup `mockups/2026-10-04-home-today-strip/`, option A). It is a card under the status line. A 28pt bar spans midnight to now, with sleep as bars and feeds as ticks, and an axis of midnight, six-hourly marks and "now"; the axis is dropped at accessibility text sizes. A running sleep or feed is drawn live up to now at the right edge. Below the bar are icon-and-number totals: feeds, sleep and diapers using joined-session counts, plus today's bottle volume once there is some. Totals count saved entries only, so a running part counts once it stops. Tapping it opens History at today.

**Dock.**
- Idle or in a break: two large buttons, **L** and **R**. The suggested side is outlined: the side not yet used in the open feed session, otherwise the opposite of the last breast part. Below is a row of Bottle, Sleep, Diaper and ···.
- Feed running: one large **Stop**, then Diaper and ···.
- Sleep running: one large **Wake up**, then Feed, Diaper and ···. Feed opens a compact L / R / Bottle chooser, because a baby can fall asleep at the breast.
- Read-only caregivers see no dock, as today.

## Sheets

Time defaults to Now and is tappable to change. One tap logs where the choice is the whole entry.

- **Diaper:** time, then **Wet**, **Dirty** and **Mixed** buttons, then a collapsed **Details** accordion with Consistency, Colour and Note. Details are filled first if wanted. A type tap logs the entry with them in one write (the existing single-write behaviour, now including `note`). Dry stays in the edit sheet only.
- **Bottle:** time, contents segmented control (Breast milk, Formula, Mixed; remembers the last choice), and a large **Start bottle**. Below is "or log what was drunk", with amount chips 30, 60, 90 and 120 ml (or the oz equivalents) that log a finished bottle instantly. Long-press a chip for an exact amount.
- **Bottle stopped:** amount only, preselected to the last amount, with chips, − and + (10 ml or ½ oz), and **Done**.
- **More (···):** first row Solids, Growth, Past entry. Below that, **Meds**: saved meds as horizontally scrolling chips, with **+** always last. Tapping a med logs it now. + opens the existing add-med flow.
- **Past entry:** a breast side, bottle or sleep with start and end. It joins by the session rule.
- The feed type menu (`FeedMenuSheet`) is removed. Solids moves to ···.

## Live Activity, widgets and Siri

**Live Activity** (Lock Screen and Dynamic Island) mirrors Home:

| State | Content | Controls |
| --- | --- | --- |
| Feed part running | Part glyph, elapsed | **Stop** |
| Feed break | "break 0:40", parts so far | **L**, **R** (suggested outlined) |
| Sleep running | Moon, elapsed | **Wake up** |
| Sleep break | "Awake 3m" | **Sleep** |

The break states end when the break limit passes or the next part starts. Feed and sleep keep separate activities when both are live.

A timed bottle stopped from the Live Activity saves with the last-used amount and leaves an in-app **Set amount** prompt on Home. It never opens the app.

**No unlock.** Every Live Activity control must run on a locked iPhone without unlocking or opening the app. Today `FinishBottleIntent` sets `openAppWhenRun = true`, and a caregiver reports being asked to unlock. Find and fix the actual cause during implementation (open-app flags, authentication policy, protected-data access from the extension or app process). Verify on a locked physical device.

**Widgets.** The Status widget shows feed, sleep and diaper time since with the new icons, computed from joined sessions (today `WidgetStatusSummary` reads raw entries). The Quick Log widget buttons become **L · R · Sleep · Diaper**. If iOS requires unlock for Lock Screen widget buttons, document that limit rather than work around it.

**Siri and App Shortcuts:** Start left breast, Start right breast, Stop feeding, Start sleep, Wake up, Log diaper. Switch side is removed. All go through the same start/stop and join code as the app.

## Sync and server

- Migration `000025`: add `sleep_session_id` to sleep storage, and a session ID to the timer start payload for both feed and sleep timers. Add a new protocol capability for the sleep link and timer session ID, and update the schema compatibility manifest.
- The existing timer edit command can change a running timer's session ID, which is what ✕ sends.
- On timer stop, whether from the owning device or another caregiver, the server copies the timer's session ID into the completed feed or sleep.
- The server validates that a session ID belongs to the same family and baby and kind.
- Pre-capability clients keep working. The server still accepts `switch` and `pause` timer commands from them; the new client never sends them. Pre-capability clients cannot mutate sleeps that carry a session link, following the existing one-way boundary for multi-component feeds, so they cannot erase the link.
- Concurrent starts from two caregivers resolve through the existing one-live-timer-per-kind rule. No part is duplicated or lost.
- Feed and sleep IDs remain stable. Joining and separating are ordinary entry updates.

## Testing and verification

**Package tests**
- Join decision at the limit and one second past.
- Blockers and non-blockers.
- ✕ new session.
- Chip and Past-entry joins.
- Safety-net stop on starting another part.
- Short-part cleanup still applied.
- Suggested side.
- Joined totals and day attribution for feeds and sleeps.
- Awake measured from the last sleep part.
- History Combine across a diaper.
- Sleep Combine and Separate.
- Unchanged legacy multi-component feeds.
- Widget summary using sessions.
- `EXPECTED_TESTS` updated with the real new total.

**Server tests**
- Migration 25.
- Session ID copied on stop by either caregiver.
- Ownership validation.
- Legacy switch and pause still accepted.
- Legacy clients refused on linked sleeps without cursor advance.
- Concurrent starts.
- Full suite, protocol end-to-end and smoke checks.

**UI tests**
- Rewritten Home, timer and sheet flows with new identifiers.
- The accessibility audit passes on the new screens.
- `make build` succeeds.

**Own end-to-end checks before asking for phone testing**
- Simulator flows:
  - L, Stop, then R within the limit gives one feed; after the limit, two feeds.
  - ✕ separates.
  - Sleep joined across a wake.
  - Bottle by chip and timed.
  - Diaper with details.
  - Meds chips.
  - Past entry.
- Screenshots of every hero state, compared to the mockups, including forced Ukrainian on the compact iPhone 17e.
- Two-caregiver sync against a local server.

**On the user's locked iPhone (requested only after all of the above pass):**
- Live Activity Stop, L/R, Wake up and Sleep without unlock.

**Rollout.** Work happens on a dedicated branch or worktree, because main carries other sessions' uncommitted changes. Deploy the server (migration 25) before the new app. Build phones from a clean committed tree. Nothing is pushed or deployed without explicit approval.

## Out of scope

- History, Insights/Growth, Ask, Settings, Caregivers and Export layouts. These are later sub-projects.
- Light mode.
- Retroactive merging of existing entries.
- Pumping tracking.
