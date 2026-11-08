# Design system rollout — 2026-11-08

The user asked to "merge and install".

## Merge

- Branch `design-system` (52 commits) was fast-forwarded onto `main`, which had not moved since `eab61d70`. `main` is now `69a73e80`; the branch is deleted.
- `make test` on that exact tree: OK, 1991 tests. The full UI suite passed 58/58 at Task 13, and the affected UI suites passed again after the final fixes.

## Server

No deploy. `git diff --stat eab61d70 69a73e80 -- Server deploy` is empty, so this is a client-only change.

## Phones

- **Build:** signed Debug of `69a73e80` from a clean worktree with `xcodebuild -scheme BabyTracker -configuration Debug -destination 'generic/platform=iOS'`. `codesign --verify --deep --strict` passes, and the team is `TEAM123456`.
- **Phone A** (`00000000-…0000`): `devicectl device install app` in place succeeded, and `com.example.babytracker` 1.0 (1) is listed. The app was not launched.
- **Phone B** (`00000000-…0000`): the first two attempts failed because the phone was locked.

## Follow-up fix: `8138227a`

- The hairlines in Settings list cards started at each row's first text, so the centred Leaving buttons pulled them in. `SettingsListSection` now pins them to the row's edges. Checked in a simulator screenshot of Caregivers; `make test` OK, 1991.
- Signed Debug of `8138227a` was built from a clean worktree; `codesign --verify --deep --strict` passes. It was installed in place on **both** Phone B and Phone A ("App installed", exit 0). Neither app was launched.

## Still open

- On-device check in Ukrainian at the largest text sizes: Home chips, SessionSheet part rows, the scanner and the widgets.

## Second pass: `2d1baeec`

- `7ee328c9`, the second consolidation pass:
  - Settings' list section, button section and action row moved into the kit.
  - `ListRow` is for showing values; `LabeledRow` is for rows with controls.
  - The join chip is now a kit `Chip`.
  - The rule for accessibility identifiers is written into the spec.
- `2d1baeec`, Live Activity: Stop and Wake up are `.plain`, so iOS 26 no longer draws its own capsule behind the light label. The pump card's Stop uses the same label on the Lock Screen and in the Dynamic Island.
- `make test` OK, 1992 tests. The full UI suite passed 58/58. In the rerun of the Live Activity UI test, the screenshots show a single Stop capsule and no doubled background on the break tiles.
- `main` was fast-forwarded to `2d1baeec`. A signed Debug build from a clean worktree passes `codesign --verify --deep --strict` and is installed in place on both Phone B and Phone A ("App installed", exit 0). Neither app was launched.
- Not seen on screen: the pump card's new Stop. No UI test shows it.
