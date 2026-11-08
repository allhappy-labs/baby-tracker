# Arrangeable Home buttons rollout — 2026-10-11

The user asked to "fix the minors, merge and update apps".

## Minors

The four minors the whole-branch review deferred are fixed in `34cd4455`:

- "Shown as Left and Right." sits in the Breast row while Breast leads, not under the whole list.
- A Lock Screen "Set amount" prompt is no longer consumed while the baby switcher or the Home buttons screen is up; it is read when they close.
- A widget link parked while the store-failure screen was up opens after Retry.
- A widget set to a deleted baby says the baby is unavailable.

## Merge

- `main` had moved to `8d22f6d3` (the status line's "ago", another session). `home-buttons` was rebased onto it, then fast-forwarded. `main` is now `34cd4455`.
- `make test` on the rebased tree: OK, 1798 tests.
- UI classes on the rebased tree, one run with no other `xcodebuild` on the shared simulator: `LoggingFlowTests`, `RunningTimerLifecycleTests`, `SettingsFlowTests`, `AccessibilityAuditTests`, `ReadOnlyCaregiverUITests`: 23 tests, 0 failures.
- Three earlier runs on the rebased tree each failed in one to three tests. Another session was running UI tests on the same simulator at the time (one was caught in progress), and the failure screens showed another run's data or a missing seeded baby. The same tests passed on `main` alone and in the clean run above. They are recorded here as collisions, not as passes.

## Server

No deploy. This change touches nothing under `Server/`. The homelab stays on `3e586cce`.

## Phones

- **Build:** signed Debug of `34cd445502d9e33609dd3eb2c3f2e8b1239c8f2c` from the clean worktree, `xcodebuild -scheme BabyTracker -configuration Debug -destination 'generic/platform=iOS'`. `codesign --verify --deep --strict` passes; `scripts/live-activity-push-signing.sh check --app` reports `result: ready`, profile "BabyTracker Development - TEAM123456", `aps-environment: development`. App executable SHA-256 `09df9e17a8e6aa89020dd09c601c7ec02040642e075df29675d98aa09317c67c`.
- **Install:** `devicectl device install app` in place, no uninstall, on Phone A (`00000000-…0000`) and Phone B (`00000000-…0000`). Both list `com.example.babytracker` 1.0 (1) afterwards. Neither app was launched by this work.
- **Also delivered:** everything on `main` since the phones' last recorded install (`296312a7`, 2026-10-09). On the app side that is the fitted sheets, the Settings confirmations, the History PDF Note column fix, the status line's "ago", and the client pieces merged with the PostgreSQL authority (background push registration).

## Still open

The user's check on the phones, from `docs/verification/2026-10-11-home-buttons/README.md`:

1. Default order: the Quick Log widget shows L · R · Bottle · Sleep.
2. Bottle on the widget starts a bottle without opening the app.
3. Changing the order in the app changes the widget.
4. Solids on the widget opens the app on the solids panel.
5. With the app force-quit, Growth on the widget cold-launches into the growth panel.
6. A widget set to a deleted baby shows the baby as unavailable.

Dragging rows in Settings → Tracking → Home buttons was not exercised by any automated test.
