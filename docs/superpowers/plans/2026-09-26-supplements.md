# Supplements Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Log and reuse free-text baby supplements with an adaptive picker and family sync.

**Architecture:** A dated supplement record supplies both the history row and the reusable name list. Extend the existing server protocol and persistence path, then SwiftData and sync, then the Home sheet and History views.

**Tech Stack:** Swift 6.2, SwiftData, SwiftUI, Go, PostgreSQL.

**Spec:** `docs/superpowers/specs/2026-09-26-supplements-design.md`

## Global Constraints

- Work only in `.worktrees/supplements` and preserve the unrelated main checkout Xcode project edit.
- Use `rtk` before shell commands, Conventional Commits, and no third-party workflow actions without immutable SHA pins.
- Treat supplement names as baby scoped; trim whitespace, reject empty, and deduplicate names case insensitively for selection.
- Keep the plus control visible for zero, one, and multiple names; show no picker for one name; select the last logged name for multiple names.
- Keep local writes atomic and follow the existing realm/write lease and sync compatibility patterns.
- Do not deploy, push, or merge.

## Review Focus

- Empty or whitespace name: Save remains disabled and no record is created.
- Two names differing only by case: one reusable option, while each intentional log remains a distinct event.
- Backdated log: the last used name follows creation/logging action rather than the displayed event time.
- Failed save: the sheet stays open and no partial log or duplicate is created.
- Old client compatibility: server behavior is explicit; no silent loss of unknown supplement records.

---

### Task 1: Server protocol and durable supplement records

**Files:** Extend `Server/internal/protocol`, `Server/internal/records`, `Server/internal/sync`, `Server/internal/importer`, `Server/migrations`, `Server/api/openapi.yaml`, protocol fixtures and matching tests.

**Interfaces:** Produce entity kind `supplement`, payload version 1 with `baby_id`, `started_at`, immutable `logged_at`, `time_zone_id`, `name`, and nullable `note`. The server returns canonical supplement records through mutation, sync page, snapshot and import. The new table uses the existing family, baby, version, sequence, RLS and quota patterns. Document the schema version and compatibility handling for old clients.

- [ ] Add failing protocol tests for valid/invalid names, exact payload shape, canonical hashing, and unknown-old-client behavior.
- [ ] Add the protocol payload, fixture, OpenAPI schema and validation until those tests pass.
- [ ] Add a forward PostgreSQL migration and failing repository/sync tests for create, update, delete, snapshot, import and child deletion.
- [ ] Implement persistence and all server switch points using established diaper/sleep patterns; make tests pass.
- [ ] Run `rtk go test ./...` from `Server`; capture outcomes and commit scoped files with a Conventional Commit.

### Task 2: SwiftData supplement store and sync

**Files:** Extend `Packages/BabyTracker/Sources/BabyProtocol`, `BabyStore` model/schema/migration/store/remote apply/import, matching tests.

**Interfaces:** Produce `SupplementEntry`, `BabyStore.logSupplement(name:for:at:)`, `BabyStore.supplementNames(for:)`, `BabyStore.lastSupplement(for:)`, and supplement handling in `HistoryItem` and `entries` queries. Name options order by immutable `logged_at`, using a stable tie break. Follow server Task 1 wire payload and mutation kind. Reject editing an entry outside the captured realm.

- [ ] Write failing store/protocol tests for create, trimmed names, blank rejection, options, last use, deletion, remote apply, and migration.
- [ ] Add a versioned SwiftData schema migration and complete local store/change log/outbox/import/realm support.
- [ ] Add Swift wire models and remote apply, including unknown entity handling for older payloads.
- [ ] Run focused `rtk swift test` from `Packages/BabyTracker` and commit scoped files with a Conventional Commit.

### Task 3: Supplements quick log and History UI

**Files:** Extend `BabyUI/Palette.swift`, `HomeFeature/QuickLogDock.swift`, `LoggingFeature/LogCoordinator.swift`, a new supplement sheet, `Apps/BabyTrackerApp/RootView.swift`, `HistoryFeature` display/edit, and matching tests.

**Interfaces:** The dock opens `.supplement`; the sheet reads Task 2 store APIs and logs through `logSupplement`. Zero names offers plus; one shows name text; multiple shows a picker preselected to last used; plus stays visible. History renders and edits supplement entries.

- [ ] Write failing state/model and copy tests for zero, one, multiple, last used, and validation.
- [ ] Implement adaptive sheet and dock integration with accessibility labels and save-error handling.
- [ ] Implement History display/edit/delete and focused tests.
- [ ] Run `rtk swift test` from `Packages/BabyTracker` and `rtk xcodebuild` simulator build; commit scoped files with a Conventional Commit.

### Task 4: End-to-end verification

**Files:** Add or extend local family sync integration tests and `docs/verification` evidence.

**Interfaces:** A supplement logged on one client is visible and reusable on another; edit/delete converge.

- [ ] Run a local two-client server sync test covering create, edit and delete.
- [ ] Run full `rtk go test ./...` and `rtk swift test`; run iOS simulator build and relevant UI test if available.
- [ ] Inspect the full branch diff and document outcomes and remaining limits in `docs/verification/2026-09-26-supplements.md`.
- [ ] Commit scoped verification files with a Conventional Commit.
