# Bottle History start/end times — 2026-09-30

User requested bottle start/end times like feeding sessions, local main integration and updates on both paired phones. Scope: existing History row presentation for completed standalone feed records, using stored entry timestamps and recorded zone. No data/schema/server changes. Untimed bottles keep their single logged timestamp.

- [x] Inspect feeding-session and bottle History presentation.
- [x] Reproduce missing end time with a real timed bottle; retained-zone and untimed regression coverage.
- [x] Verify focused/full Swift and bounded affected UI gates; review scoped diff.
- [x] Merge to local main while preserving unrelated dirty tracker/work.
- [x] Build/sign/install on both paired physical phones and record final evidence.

## Added user scope: shortcuts and caregiver refresh

- [x] Remove visible text from Sleep, Diaper, Supplements and Measure shortcut buttons, preserving localized accessibility labels and actions.
- [x] Replace visible tracker shipping-box glyphs with a shared, drawn diaper silhouette in Home and History.
- [x] Diagnose session renewal/restore and reproduce an explicit rate-limit sign-in escalation; verify focused regression RED and 21 renewal/restore tests GREEN.
- [x] Verify combined changes and independent review; local merge/device updates below.

The session fix clears an uncertain refresh marker only for exact HTTP 429 `rate_limited`: Server/internal/httpapi/auth.go rejects quota before rotating credentials. Retrying this refusal is safe. Unknown responses, 503, 401 and timeout still retain the marker. Two added test declarations cover retry and four adverse response variants. The caregiver's actual trigger is unconfirmed; ordinary renewal should not reach the configured quotas. No role-specific renewal defect was established.

Read-only live checks on 2026-09-30 found server source `842ab4440e8326b5711a6522ebc934d33b68dfca`, schema 22, no Growth capabilities. Current app expects schema 24 features. This compatibility gap can explain some sync failures; it does not prove a sign-out cause. Active memberships are one owner and one caregiver. The last 24 hours' session aggregate had no revoked rows; 22 `membership_revoked` rows across seven days can reflect one removal of accumulated sessions, not 22 separate sign-outs. No production changes were made.

Automatic approval review rejected retrieval of raw production logs because they could expose tokens or private family data. No logs were retrieved; diagnosis used code, deterministic tests and approved aggregate database checks. Exact error text/timing from the affected phone is still needed for attribution.

## Current verification evidence

- Bottle History focused suite: 66 tests pass. Real timed bottle displays recorded start/end in its stored zone; a manual point event retains one timestamp.
- Full package gate before added auth scope: 1109 tests pass. Combined `rtk make test` gate passes: 1111 tests. Independent expanded review found no Critical/Important issues; server quota ordering and fenced cleanup verified.
- Affected iOS UI test built but stalled before execution, bounded at 240 seconds. Build-for-testing passed afterward. No simulator reset or user data deletion attempted.
- Actual HistoryRowView macOS render inspected: timed bottle has two clocks, manual bottle one. Actual QuickLogDock source rendered at 393 and 320 points: icon-only controls fit and the diaper has fastening tabs/leg openings. Running dock rendered through NSHostingView. These are partial visual evidence; macOS rendering does not establish iOS Dynamic Type or physical-device acceptance.
- Existing HomeFeature target builds; no new tests added for the purely visual adjustment.

Local artifacts: `/path/to/artifact`, `/path/to/artifact`, `/path/to/artifact`, `/path/to/artifact`, `/path/to/artifact`, `/path/to/artifact`.

RED: first regression also exposed DateFormatter versus Date.FormatStyle midnight padding in the test expectation; align expectations with the existing locale-aware style. Corrected RED run fails exactly the missing `row.endTime` assertion; untimed-bottle case passes. Logs `/path/to/artifact`, `/path/to/artifact`.

First History GREEN exposed that manual `logBottle` stores `endedAt == startedAt` as a point event, rather than nil. The display therefore requires a positive recorded interval before showing a second clock time; no timestamps are written or inferred. The manual regression caught this before merge.

Final combined signed generic iOS build passes; codesign deep/strict verification passes. No ZIP/PDF files in the app or embedded widget. Both physical replacement installations succeeded.

## Integration and phone update

Local main fast-forwarded to `cba7aaa3070a3c364c77b7c035c93ab05775398a`. This is the reviewed and tested product tree used for the signed app. Main TASKS.md and both unrelated work/ HTML files retain their pre-merge SHA-256 hashes. No push, deployment or user data reset/uninstall.

Both devicectl replacement installs exited 0: owner's iPhone 18 Pro (`00000000-0000000000000000`) and caregiver's iPhone 17 Pro (`00000000-0000000000000000`). Provisioning permits both devices; signature is valid. Bundle `com.example.babytracker`, version 1.0/build 1, executable SHA-256 `8612a640a310416dffaa2b6cb36d5f7147e16cc0d2a0817e9e0b1ad3d564f305`. Version/build are unchanged, so install reports and the exact source artifact identify this update. Evidence `/path/to/artifact,1}.json`, `/path/to/artifact`. Installation is not physical UI or auth recurrence acceptance.

Remaining: exact affected-phone error/timing, physical Home/History and accessibility acceptance, live owner/caregiver sync acceptance, and a separately authorized server update from schema 22 to 24. The official Swiss source remains ignored in the original Growth worktree; no ZIP/PDF entered Git or app.

## Follow-up: stale updates with a successful sync status

User clarified that the caregiver remains in the family and the screen reports just synced, but updates have been missing since 13:53 on 2026-09-30. This is a stale-update report, not evidence of fresh-proof/sign-out escalation. Assume Europe/Zurich (UTC+2) for the cutoff.

Read-only aggregate mutation-journal query after 11:53 UTC found one feed (latest 11:53:11 UTC), one sleep (latest 12:03:23 UTC) and three timer mutations (latest 12:12:36 UTC). No payloads, account/family/device identifiers, or raw logs were retrieved. This establishes server acceptance after the cutoff but not delivery to the caregiver. Live schema remains 22.

`rtk proxy swift test --filter 'FamilySyncEngineTests|FamilySyncEngineLiveTests|SyncCoordinatorTests'` on current main passes: 64 tests in three suites, 1.510 seconds (44.30-second build). These exercise owner/joiner propagation, foreground polling, reconnect catch-up, expired-access renewal and refusal of false success when sync cannot run. They do not reproduce this physical-phone failure. No new product changes made on the basis of this clarification.

Pending evidence: which post-cutoff entry types are absent; owner-side sync status; whether the caregiver's displayed successful-sync timestamp advances on foreground return and whether History remains stale afterward. The actual stalled-sync cause remains unconfirmed; the earlier exact-rate-limit fix must not be presented as solving this symptom.
