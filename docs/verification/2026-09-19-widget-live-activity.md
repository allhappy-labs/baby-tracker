# Widget and Live Activity verification — 2026-09-19

## Status: OPEN — NOT YET PERFORMED

Nobody has ever placed a widget, tapped a widget button, or seen a Live
Activity render in this project. The checks below require a physical tap on a
device or simulator, and the agent environment that produced the fixes has no
tap API and no device access.

**Every box below is unchecked and every answer is blank because the check has
not been run.** Do not read the fixes recorded further down as evidence that
these passed — they were derived from source, not from a device.

---

## The checklist (for a human, on a real device)

Install the app on a device or simulator with Live Activities enabled, then:

- [ ] **1. Placement.** Place the small **Status** widget and the medium
      **Quick log** widget on the Home Screen.
      - Do both appear in the widget gallery? _(answer here)_
      - Does the gallery preview show sample values (`2h 14m`, `Awake 40m`,
        `55m`)? That is correct and expected — it is the gallery.
      - After placing, does the Status widget show the **real** values for the
        selected baby? _(answer here)_

- [ ] **2. Fresh install / no baby.** Delete the app, reinstall, place the
      Status widget **before** adding a baby.
      - What do the three lines show? They must be `—`, `—`, `—`, and the
        Quick log widget's name line must be empty. _(answer here)_
      - Any sample value here is the bug Task 15 fixed returning.

- [ ] **3. Widget button → entry.** Tap the medium widget's **Diaper**.
      Open the app → History.
      - Does the entry appear? _(answer here)_
      - Is it on the right baby? _(answer here)_

- [ ] **4. Live Activity renders.** Tap **Feed** in the app to start a breast
      feed. Lock the screen.
      - Does the Live Activity render? _(answer here)_
      - Does the elapsed time count up while locked? _(answer here)_
      - Does the Dynamic Island show it (on a device that has one)?
        _(answer here)_

- [ ] **5. Stop from the Lock Screen.** Tap **Stop** on the Lock Screen.
      - Does the feed end and get saved? _(answer here)_
      - Does the activity disappear from the Lock Screen, or is it stranded?
        _(answer here)_

- [ ] **6. Two babies — the targeting check.** With two babies (A and B):
      start a feed for **baby A**, switch to **baby B** in the app, lock the
      screen, tap **Stop** on the Lock Screen.
      - **Which baby's feed stops?** _(answer here — must be A)_
      - Which baby does the saved entry land on in History? _(answer here —
        must be A)_

- [ ] **7. Sleep, same shape.** Repeat 4–6 with sleep instead of feed
      (the sleep Live Activity's Stop is `ToggleSleepIntent`).
      - _(answer here)_

- [ ] **8. A Stop on a stale activity.** Start a nap in the app, then end it
      **in the app** while the Live Activity is still on the Lock Screen. Now
      tap **Stop** on that lingering card.
      - Does a new nap start? _(answer here — it must NOT; this is the
        "button labelled Stop starts a sleep" bug)_
      - Does the card disappear? _(answer here — it should now dismiss itself)_
      - Repeat for a feed: end the feed in the app, then tap Stop on the
        stale card. _(answer here)_

- [ ] **9. A Stop for a deleted baby.** Start a nap for baby A, delete A in
      the app, then tap **Stop** on A's lingering Lock Screen card.
      - Is anything written to baby B? _(answer here — it must not be)_
      - Does the card dismiss rather than sit there dead? _(answer here)_

- [ ] **10. Widget toggle still follows the selection.** With no timer running
      and **baby B** selected, tap the widget's **Sleep** button.
      - Does sleep start for **B** (the selected baby)? _(answer here — it
        must, this path deliberately has no activity id)_

- [ ] **11. Cross-process refresh, app in the FOREGROUND.** This is the one
      check on this list whose failure produces a wrong **entry**, not a wrong
      pixel — read it before the others.
      Open the app and leave it on **History**, on screen, foregrounded. Pull
      down the Home Screen / Notification Centre so the widget is reachable
      **without** backgrounding the app if your device allows it; otherwise
      tap the widget's **Diaper** and return to a *still-running* app rather
      than a cold launch.
      - Does the new diaper appear in History **without** the app being
        backgrounded and re-foregrounded? _(answer here — it must)_
      - If it only appears after backgrounding and returning, say so: that is
        `observeRemoteChanges` being inert in production and History being
        refreshed by scene phase instead. The mechanism has been inert once
        already, under four green tests.
      - Check 3 above does not distinguish these two, because it sends you
        into the app after the tap.

- [ ] **12. The widget's Feed button opens, and never logs.** With the last
      feed a **bottle** (log one first, so the app's own dock reads
      `Bottle · …`), tap the medium widget's **Feed**.
      - Does the app open on the **feed menu** — Breast / Bottle / Solids?
        _(answer here — it must)_
      - Is a breast timer running afterwards? _(answer here — it must NOT be;
        the widget's plain "Feed" label cannot promise a breast feed, and a
        silently started breast timer is a fabricated entry in History and
        Insights and a flipped side suggestion for the next real feed)_
      - Tap Cancel on the menu, then background and reopen the app. Does the
        feed menu re-open on its own? _(answer here — it must not; the parked
        request is cleared when it is read)_
      - Tap the widget's **Feed** again with the app **already on screen**.
        Does the menu still open? _(answer here — this path has no scene phase
        change, so it depends on the notification rather than the launch)_

Record anything that did not work, verbatim, including anything that looked
right but felt slow or stale.

---

## What was fixed from source, ahead of the manual check

These are code changes, verified by automated tests and a simulator build.
They are **not** substitutes for the checklist above.

### The widget no longer renders sample data as real (Task 15)

`StatusProvider.getTimeline` fell back to `placeholder(in:)` whenever
`currentEntry()` threw — which happens on a fresh install with no baby, and on
any store read failure. The placeholder is the widget gallery's sample:
`feedAge: "2h 14m"`, `sleepText: "Awake 40m"`, `diaperAge: "55m"`. A placed
widget therefore stated, confidently, that a feed, a nap and a change had
happened when none had, to a parent who may act on it.

The timeline now falls back to `WidgetStatusSummary.noData` — em-dashes and an
empty baby name. `getSnapshot` likewise returns `noData` unless
`context.isPreview`: WidgetKit calls it for transient renders of a *placed*
widget too, not only for the gallery. `placeholder(in:)` keeps its sample
values, because that is what the gallery previews.

The widget's display strings moved out of the view into
`BabyStore.WidgetStatusSummary`, where they are covered by tests
(`WidgetStatusSummaryTests`). The widget extension is not a test target, which
is why this bug was invisible to every suite.

### The widget's Feed button opens the app instead of starting a breast feed (review fix)

v1.1 made the app's dock Feed button self-describing — `Breast · Left`,
`Bottle · 120 ml`, `Solids` — and left the widget's, labelled plainly "Feed",
starting a breast timer unconditionally. A bottle-feeding parent tapping it got
a silent breast timer, found minutes or hours later when the dock read "Stop
feed"; stopping wrote a breast feed they never had into History and Insights,
and flipped `suggestedSide` for the next real feed.

`OpenFeedIntent` (was `StartFeedIntent`) now opens the app on the feed menu and
writes nothing. Dispatching on `HomeViewModel.nextFeedAction` inside the intent
was the alternative and was not taken: the widget extension does not link
`HomeFeature`, and even with the dispatch the button's label would still read
"Feed" while a breast timer started behind it. Check 12 above is what confirms
this on a device — it is the only check here whose failure writes an entry.

### A Stop control can no longer start anything (review fix)

The sleep Live Activity's Stop button is bound to `ToggleSleepIntent`, which
started a nap when it found none running. On a stale card — nap already ended
in the app, card not yet dismissed — a button labelled **Stop** silently
started a *new* nap. Task 16 had just made it land reliably on the right baby,
which made it reliably wrong rather than occasionally wrong.

`SleepToggleAction.decide(hasRunningSleep:isStopControl:)` now makes the call,
in the package where it is tested. A control carrying an explicit `babyID` is
a Stop control, and a Stop control never starts: with nothing to stop it does
nothing at all.

Both stale cases — nothing to stop, and a baby that no longer exists — now
also call `LiveActivityController.endStaleActivity(babyID:)`, so the card is
dismissed instead of leaving the user tapping a dead button forever. This
applies to the feed Stop as well as the sleep Stop. Checks 8 and 9 above cover
it.

### Stop acts on the timer's baby, not the selected one (Task 16)

`TimerActivityAttributes` carried `babyName` and `kindRaw` but no `babyID`, and
`StopFeedIntent` / `ToggleSleepIntent` resolved their target via
`WidgetStore.currentBaby(in:)` — "whichever baby is selected in the app". With
two babies, a Stop tapped on the Lock Screen could save the feed to the wrong
child.

`TimerActivityAttributes` now carries `babyID`, the Live Activity's buttons
pass it to the intent, and `ActivityTarget.resolve` decides the target:

- an id supplied (Live Activity) → that baby;
- an id naming no known baby, or unparseable → **nobody** (the child was
  deleted while the activity was on screen; acting on a sibling is the failure
  being prevented);
- no id (Home Screen widget button) → the selected baby, which is what that
  tap means.

This was built without waiting on check 6 above: the defect is established
from source, and carrying the id is correct whatever the device shows. Check 6
remains the thing that confirms the fix in the user's hands.
