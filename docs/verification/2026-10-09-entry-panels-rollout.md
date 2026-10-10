# Entry panels rollout — 2026-10-09

The user asked to "merge to main, deploy if needed and update apps".

## Merge

- Branch `entry-panels` was rebased onto `main` `5ef21207`, a pumping design doc that is docs-only, then fast-forwarded. `main` is now `296312a7`.
- `make test` on the rebased tree: OK, 1700 tests.

## Server

No deploy. `git diff --stat ae98c374 HEAD -- Server deploy` is empty, so this is a client-only change with no schema, payload or server code. The server stays at `fce422d`, schema 29.

## Phones

- **Build:** signed Debug of `296312a7` from the clean worktree with `xcodebuild -scheme BabyTracker -configuration Debug -destination 'generic/platform=iOS'`. `codesign --verify --deep --strict` passes, and the team is `TEAM123456`.
- **Install:** `devicectl device install app` in place on Phone B (`00000000-…0000`) and Phone A (`00000000-…0000`), then each app was launched.
- **Server logs in the first ~10 minutes** (counts only):
  - Both phones re-authenticated after being idle: 401 on `/v1/live` and `/v1/live-activities/device`, then `/v1/auth/refresh-recoverable` 200 and `refresh-ack` 204.
  - Then `/v1/sync` 200×5, `/v1/family` 200×2, `/v1/live-activities/device` 200×2 and one live socket upgrade (101).
  - Two 503s on `/v1/account` and two on `/v1/live` were requests the client abandoned during relaunch.
  - Every log line is INFO, and all three containers are healthy.

## Still open

- The user's own check on both phones:
  - Past entry;
  - editing a breastfeeding, bottle or sleep entry from History by Start/End;
  - adding and removing a bottle's end time;
  - diaper, solids, supplement and measurement logged on an earlier day;
  - no empty band under Details.
