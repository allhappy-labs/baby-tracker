# Baby Tracker — status after the full build

**Branch:** `feat/baby-tracker-v1` — all commits SSH-signed, not merged.
**State:** all 40 planned tasks complete, through v1.1. Final whole-branch review done.
**Verdict:** **ship-with-caveats.** Merge the branch; the one thing still owed a
parent is the manual widget / Live Activity checklist in §3.
**Tests:** 225 package tests + 38 UI tests at the final review; the fix wave that
followed it brought the package total to **233**. `make test` asserts the literal
number, so a stale figure quoted from here becomes a failing run — the number that
counts lives in `EXPECTED_TESTS` in the `Makefile`, not in this document.
`xcodebuild` → BUILD SUCCEEDED.

---

## 1. What exists

A native iOS 26 baby tracker for feeding, sleep and diaper changes: a thin Xcode project
over a local Swift package, plus a widget extension and a Live Activity.

```
BabyTracker.xcodeproj         hand-authored pbxproj (no GUI, no XcodeGen)
Apps/BabyTrackerApp/          three tabs
Apps/BabyTrackerWidgets/      status + quick-log widgets, Live Activity
Apps/SharedIntents/           App Intents, compiled into BOTH app and extension
Packages/BabyTracker/         all real logic, 233 tests, runs without a simulator
UITests/BabyTrackerUITests/   38 tests incl. a full copy read-through + a11y audits
design/icon/                  "Cradle" icon: layered SVGs ready for Icon Composer
mockups/                      the HTML mockups you approved
```

```bash
# run it
make build
# fast tests, no simulator — NOT plain `swift test`, see §5
make test
# the simulator suite, ~10 minutes
make test-ui
```

---

## 2. Merge mechanics — read before merging

**`main` is disconnected from this branch.** Re-signing used `git rebase --root`, which
rewrote every commit including the first, so `main` (4 old unsigned doc commits) shares
**no common ancestor** with the work. Nothing is lost — those 4 commits exist, re-signed,
on the branch. `main` cannot be fast-forwarded; it has to be reset:

```bash
git checkout main && git reset --hard feat/baby-tracker-v1
```

Your call — it rewrites `main`. Backup ref: `backup/pre-resign-20260919`.

**Signature verification still needs configuring.** `git log --show-signature` prints
"No signature" and `%G?` returns `N` for every commit. That is a *verification* gap, not
a signing one — every commit carries a `gpgsig` header (verified with `git cat-file`). Two
reviewers misread this as unsigned. To fix:

```bash
echo "developer@example.com $(git config --get user.signingkey)" >> ~/.config/git/allowed_signers
git config --global gpg.ssh.allowedSignersFile ~/.config/git/allowed_signers
```

Also: `export SSH_AUTH_SOCK=~/Library/Containers/com.bitwarden.desktop/Data/.bitwarden-ssh-agent.sock`
in your shell profile, or agents hit the same wall again.

---

## 3. The one thing still owed a parent: the manual checklist

`docs/verification/2026-09-19-widget-live-activity.md` — **twelve checks, none of
them ever run.** Nobody has placed a widget, tapped a widget button, or seen a
Live Activity render in this project; there is no tap API in the environment that
built it. Everything below the checklist in that file was derived from source.

Two of the twelve are not cosmetic, and both were added by the final fix wave:

- **Check 11 — cross-process refresh, app in the FOREGROUND.** Every test over
  `observeRemoteChanges` posts `.NSPersistentStoreRemoteChange` itself, so they
  prove the handler is wired and nothing more. That Core Data posts it at all for
  a SwiftData store opened with `ModelConfiguration(groupContainer:)` is not
  established anywhere in this repo. This mechanism was already inert in
  production once, under four green tests of the same shape. Check 11 is the case
  that distinguishes "we listen to the right notification" from "the notification
  never comes": tap the widget's Diaper **without** backgrounding the app, and
  watch History. If it only updates after a background-and-return, scene phase is
  doing the work and the cross-process path is dead. The comment on
  `observeRemoteChanges` says the same, next to the code.
- **Check 12 — the widget's Feed button opens, and never logs.** See §4.

---

## 4. What the last fix wave changed, and why it needs eyes

**The widget's "Feed" button no longer starts a breast feed.** v1.1 made the
app's dock Feed button self-describing — it reads `Breast · Left`,
`Bottle · 120 ml` or `Solids` and does what it says. The Home Screen widget's
button, labelled plainly **"Feed"**, was left starting a breast timer
unconditionally: the only place left in the app where "Feed" meant a fixed
action. A bottle-feeding parent tapping it got a silent breast timer, discovered
an hour later as a fabricated breast entry in History and Insights, and a flipped
side suggestion for their next real feed.

It now opens the app on the feed menu — the same menu the dock's chevron gives —
and writes nothing until the parent says what the feed was. Dispatching on
`HomeViewModel.nextFeedAction` in the intent was the alternative; it was not
taken, because the widget extension does not link `HomeFeature`, the timeline
does not carry the next action, and even then the button's *label* would still
read "Feed" while a breast timer started behind it. A button that does not say
"breast" must not start a breast timer, and only opening the app is right
whatever the parent feeds.

Mechanically: `OpenFeedIntent` (was `StartFeedIntent`) is a plain `AppIntent`
with `openAppWhenRun`, parking its tap in `PendingQuickLogStore` — an App Group
value plus a notification. `RootView` consumes it on `.task`, on a scene phase
change to `.active`, and on the notification, because the intent may run before
or after the view exists and an app already on screen changes no scene phase.
Reading clears it, so one tap opens one sheet. Check 12 confirms it on a device.

---

## 5. `swift test` alone is not evidence on this project — run `make test`

**Always run `make test`, never a bare `swift test`.** It runs the same suite and then
asserts the literal test total. That is not ceremony:

`BabyStoreTests` had an intermittent SIGSEGV whose dangerous part was never the crash
rate — it was the failure *shape*. A run reported **106 tests with exit 0 and no crash
line**. `BabyStoreTests` was exactly 31 tests, and 137 − 106 = 31: the suite holding the
change-log invariant, the tombstone tests and the timer tests had vanished, silently, from
a run that looked green. Totals of 118, 123, 125, 128 and 143 were seen the same way.

**The cause is fixed.** Opening a Core Data store mutates unsynchronised state shared by
every store built from the same object model, so two parallel `ModelContainer` opens
segfaulted the process — fourteen retained crash reports, one stack, three threads inside
`addPersistentStore` at once. `BabySchema.makeContainer` now holds a lock across the open,
and **every container in this project must be built through it**, tests included.
`manyContainersCanBeOpenedAtOnce` is the regression test.

**The guard stays anyway**, because the disguise is generic: any future crash in any suite
will look exactly the same. `scripts/check-tests.sh` counts every run summary and refuses
a run that comes up short. The expected total lives in exactly one place —
`EXPECTED_TESTS` in the `Makefile` — so adding a test means bumping it in the same commit,
deliberately. There is no CI on this repo; `make test` is the guard's only caller, which
is why it is the documented way to run the suite.

**One record to correct.** The reason given for not re-running `make test-ui` after the
Task 17 review fixes was "a `Makefile`, a shell script, a doc, and a pure function the UI
tests cannot reach". That was not the whole list: those commits also changed
`BabySchema.makeContainer` — the app's own launch path — and `StatusWidget.getSnapshot`.
The conclusion survives the correction: the lock is uncontended in-app, so a fault there
would have failed all 38 UI tests rather than one, and `getSnapshot` runs only in the
widget extension, which no UI test enters. But the list as stated was wrong, and the fix
wave after the final review did re-run the suite.

---

## 6. Still open

**Never built, by your own decisions.** Pumping, growth, vaccines, Apple Watch,
notifications. The sync server is a separate project — this branch builds only
the seam (`BabySync` + an append-only change log that nothing drains yet). A
`feat/shared-family-server` worktree exists but is not mine.

The settings screen, the ml/oz preference, baby rename/delete, combo feeds and
diaper detail were all open in v1 and all shipped in v1.1.

The app icon ships as a flattened asset-catalog PNG; upgrading to the layered
`.icon` needs 60 seconds in Icon Composer — see `design/icon/README.md`.

**Known minors, reviewed and deliberately left.** None writes a wrong entry;
each is recorded here so the next reader does not rediscover them as news.

- **M-1** Solids are uneditable: `EntryEditSheet` has no `foods` or `amount`
  field, so a solids entry can only be deleted and re-logged. `SolidsSheet`
  gates Save on at least one food precisely because of this.
- **M-2** The privacy test is decorative — it pins the shape of
  `AppLog.describe` rather than checking call sites. The rule does hold at all
  all six log-emission sites, checked by hand (four of which use `AppLog.describe`).
- **M-4** The error banner may sit *over* the navigation bar rather than below
  it. Needs a visual check; nothing testable distinguishes the two here.
- **M-6** The ounce stepper never lands on a round ounce, because the seed
  volume is ml-derived and the step is half a fluid ounce from wherever it
  starts.
- **M-8** The small Status widget computes a baby name it never draws.
- **M-9** Swipe-delete has no confirmation. Spec-sanctioned.
- **M-11** `AppError`'s `Equatable` compares `id` only, so two distinct errors
  with the same id would compare equal. Ids are fresh per error today.
- **D7** The combo race: a bottle attached to a feed that ends between the
  sheet opening and its Save is written as a separate bottle entry rather than
  discarded. Deliberate — the milk did go in — but it is two rows where a
  parent logged one thing.

## 7. Two bugs worth remembering

**`isDeleted` is reserved by `NSManagedObject`.** A `@Model` property with that name is
silently reset to `false` on every save. The plan specified exactly that for the tombstone
column on every entry. Tests passed and two reviews passed, because they checked the
column existed and defaulted correctly — not that its value survived a round trip. Deletes
would have appeared to work and undone themselves on relaunch, and the sync server would
never have learned of a single deletion. Now `isTombstoned`, verified zero occurrences of
the old name.

**One formatter serving two meanings.** `ElapsedFormatter.compact` returned "just now" at
zero — right for "last feed 2h ago", meaningless for "total sleep today". Every call site
was a coin flip, and the same defect recurred **eight times**. Doc comments, an explicit
warning, and a self-check all failed to stop it. Changing the signature to take Dates made
misuse a *compile error*, and it caught an outstanding instance the moment it landed. The
final review enumerated all 17 call sites: all correct.

The lesson in both: a comment is documentation; a signature is a control.
