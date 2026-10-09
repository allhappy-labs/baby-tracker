# Running-timer editing and invitation codes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a caregiver edit a running timer (start time and finished parts) with the edit syncing to the other phone, and replace QR joining with a copy/paste invitation code.

**Architecture:** Invitation code is client-only (the existing server token is the code). Timer editing adds a patch payload on the existing `update` operation for `timer` entities on the Go server (no migration), a new reducer intent and store verb in the app, an editor sheet opened from the running dial, and a new step in the two-phone e2e against the real server image.

**Tech Stack:** Swift 6.2/SwiftUI/SwiftData, Swift Testing, XCUITest; Go server + Postgres; Docker protocol-e2e harness.

> **Status (2026-09-26):** merged to main at `737a764`, server redeployed, installed on both phones. Evidence: `docs/verification/2026-09-26-running-timer-edit.md`. On-device check open.

**Spec:** `docs/superpowers/specs/2026-09-26-running-timer-edit-and-invite-code-design.md` (binding; every wire rule, file:line and test is listed there).

## Global Constraints
- Server schema stays 17; no migration. Journal op is `update`, entity `timer`, payload_version 2 only.
- Patch semantics: `started_at` null/omitted = unchanged; `components` lists only changed completed parts matched by id; ids and positions preserved; kind immutable.
- New server refusal code: `timer_component_not_found`. Time rule failures: `invalid_timer_time`.
- `putTimer` must write `started_at` on conflict (required bug fix).
- Invitation code = the token of the minted link; join field accepts a bare code or a full link; QR scanning and the QR view are removed.
- All copy in `*Copy` enums; `EXPECTED_TESTS` updated with package tests; signed commits with the `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>` trailer.
- Deploy order: redeploy the server before installing the new app on phones.

## Review Focus
- A remote stop arrives while the editor sheet is open → saving shows the "stopped on another phone" message; nothing crashes (Task 4 test).
- Editing a feed whose start was never sent (offline) → one `create feed` with the edited values after stop (Task 3 test).
- Pasting a link with surrounding text/whitespace, or a bare code with a trailing newline → still joins (Task 1 test).
- An edit racing a stop on the other phone → exactly one completed feed on both phones (Task 2 + Task 5).
- Existing timers from before the change (v1 components) → edit upgrades to v2 with preserved ids (Task 2 test).

---

### Task 1: Invitation code instead of QR
**Files:** `Packages/BabyTracker/Sources/SettingsFeature/FamilySettingsView.swift`, `FamilySettingsViewModel.swift`, `InvitationQRCodeView.swift` (remove), `InvitationScannerView.swift` (remove), `BabySync/InvitationParser.swift`, new `SettingsFeature/InvitationCodeView.swift`; tests in `SettingsFeatureTests`, `BabySyncTests`, `UITests`.
**Interfaces:** Produces `InvitationParser.intent(fromCodeOrLink:confirmedOrigin:) throws -> InvitationIntent`; `FamilySettingsViewModel.acceptInvitation(code:)`.
- [x] Failing tests (parser bare code / link / whitespace / invalid; view model join from code; UI: invite shows code + Copy; join field present)
- [x] Implement per spec Part A
- [x] `make test`, `make build`, affected UI tests on simulator 596D9F68 (erase first)
- [x] Commit `feat(settings): join with an invitation code`

### Task 2: Server — timer edit command
**Files:** per spec "Server changes" (protocol entities/mutations/canonical, records validation/timers incl. `putTimer` fix, openapi, docs, fixtures + manifest + canonical vector); tests `records/timer_edit_test.go`, protocol decode/canonical tests, `tests/protocol/timer_test.go`.
**Interfaces:** Produces the wire contract in the spec; new code `timer_component_not_found`.
- [x] Failing Go tests per spec list
- [x] Implement
- [x] `env -u BABY_TRACKER_TEST_DATABASE_URL make server-test`
- [x] Commit `feat(server): edit a running timer`

### Task 3: App — edit intent and store verb
**Files:** `BabyStore/TimerIntentReducer.swift`, `BabyStore/BabyStore+Timers.swift`, `MutationFactory.swift` if needed; tests `WireShapeTests`, `SharedTimerTests`, `TimerTests`, `RemoteTimerApplyTests`.
**Interfaces:** Consumes Task 2 wire contract. Produces `BabyStore.editRunningTimer(id:startedAt:components:expectedWriteContext:) throws`, `TimerEditError`.
- [x] Failing tests per spec Swift list (store/reducer parts)
- [x] Implement
- [x] `make test`
- [x] Commit `feat(sync): edit a running timer`

### Task 4: App — editor sheet and Live Activity
**Files:** `HomeFeature/RunningTimerView.swift`, `TimerDial.swift`, `HomeView.swift`, `HomeViewModel.swift`, new `HomeFeature/RunningTimerEditModel.swift`, `RunningTimerEditSheet.swift`; tests `HomeFeatureTests`, `LiveActivityControllerTests`, UI test + `AccessibilityAuditTests`.
**Interfaces:** Consumes `editRunningTimer`. Produces `HomeViewModel.makeTimerEditor() -> RunningTimerEditModel?`.
- [x] Failing tests per spec (edit model, Live Activity presentation, UI flow)
- [x] Implement per spec "Editor UI"
- [x] `make test`, `make build`, UI suite on 596D9F68 (erase first)
- [x] Commit `feat(home): edit a running timer`

### Task 5: Two-phone e2e step
**Files:** `Packages/BabyTracker/Tests/FamilySyncClientE2ETests/TwoCaregiverSyncE2ETests.swift`, `Package.swift` (test dep HomeFeature), `Server/scripts/protocol-e2e.sh`.
- [x] Add step 11 per spec; renumber; script requires `step 14:`
- [x] `make server-protocol-e2e` green (serial, idle machine)
- [x] Commit `test(e2e): an edit to a running timer reaches the other phone`

### Task 6: Gates, redeploy, install
- [x] All gates on the final branch: `make test`, `make build`, full UI suite, `make server-test`, `make server-smoke`, `make server-protocol-e2e`
- [x] Merge to main (fast-forward), redeploy server from main (same procedure as `docs/verification/2026-09-26-caregiver-sync.md`), verify
- [x] Build + install on `Phone A` and `Phone B`
- [x] Verification doc `docs/verification/2026-09-26-running-timer-edit.md`; commit
