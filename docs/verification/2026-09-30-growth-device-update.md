# Growth app updates — 2026-09-30

User requested “Merge to main, update apps.” Growth was already merged locally in `47ac2a6`, followed by integration evidence/cache cleanup `bedfbc0`. Source `2682ab69f7f0173ed8da62876dc7aa454df81dd2` is the final installed app revision: the bottle-volume fix landed on main during the first phone build, so the updater rebuilt and reinstalled the latest committed main on both paired phones. No server deployment, push or release occurred.

## Verified result

- Isolated committed-source checkout; no unrelated working edits included. `rtk make test` passes `OK: 1108 tests` on final source `2682ab6`.
- Signed build: `xcodebuild -scheme BabyTracker -configuration Debug -destination 'generic/platform=iOS' -derivedDataPath /path/to/artifact -quiet build`, run through `rtk proxy python3` with complete output captured; exit 0. Log `/path/to/artifact`. Compiler warnings remain; no warning-free claim.
- `rtk proxy codesign --verify --deep --strict --verbose=2` succeeds for the app, including its widget extension. Signing team `TEAM123456`; embedded development profile includes both physical devices.
- Final app identifier `com.example.babytracker`, version 1.0/build 1; local executable SHA-256 `db47aa3603a7066f9cdc4543dea51532a5008b4afd2e6f0ca50f11a8a215b793`. Exact CSV/attribution/manifest match source bytes; recursive app inventory contains no ZIP/PDF.
- `rtk proxy xcrun devicectl device install app` succeeds for **Phone A** and **Phone B**, passing that exact signed app directory. Subsequent `device info apps` succeeds and finds Baby Tracker 1.0/build 1 on each. Both installs use the existing application identifier, with no uninstall/reset.
- Final local evidence: `/path/to/artifact`, `/path/to/artifact,phone-b}-latest-install.json`, `/path/to/artifact,phone-b}-latest-installed.json`. Installation reports establish successful acceptance of the specified build; installed-app version queries alone do not identify executable bytes because the version/build is unchanged.
- Main's unrelated `TASKS.md` and both `work/` HTML files retain their original fingerprints. Previously staged bottle work was committed concurrently as `2682ab6`; updater did not edit it or discard it. Original Growth source worktree and official ignored supplement are preserved. Only the updater's temporary checkout is removed after recording results.

## Remaining limits

Phones were updated but not launched by the updater. Physical UI rendering, stored-data migration after startup, current Swiss source-info/head/routed/AX5 and every rendered locale remain unverified. Server schema 24 is verified locally in [integration evidence](2026-09-30-growth-main-integration.md); production server was not updated. Owner app download/share, independent old-binary migration and production acceptance remain open in [Swiss verification](2026-09-30-growth-measurements.md). Installation is not release-readiness evidence.
