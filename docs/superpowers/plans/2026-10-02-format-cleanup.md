# Format cleanup and past-entry sessions — implementation plan

Spec: `docs/superpowers/specs/2026-10-02-format-cleanup-design.md` (binding; its Controller rulings override task text where they conflict).

## Global constraints

- Work only in `<repo>/.worktrees/format-cleanup` on branch `feat/format-cleanup`.
- Migrations 000001–000026 are immutable. Every edit to `000027_format_cleanup.sql` re-pins its sha256 in `Server/internal/migrate/compatibility.json` (min = max = 27).
- Simulator: only `platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853`; never create, clone or erase simulators.
- Docker: remove only containers/volumes your own runs created; other sessions share this machine.
- Run gates with output redirected to a log file and report the real exit code; never pipe a gate through `tail`. Keep `EXPECTED_TESTS` in the Makefile in step with added/removed Swift tests.
- Never touch production. No SSH, no deploys; the controller does rollout.
- Commits: conventional messages ending with `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`.
- Tasks 1–7 (server track) must keep the deployed interim client (`2961ca1`, identical to the Swift client at `d1daa6a`) fully working; tasks 8–11 change the client and start only after the server track ships.

Gates used below:
- `make test` (keep `EXPECTED_TESTS` in the Makefile in step whenever tests are added or removed);
- `make server-test`, `make server-smoke`, `make server-protocol-e2e`, `make build`;
- UI tests: `xcodebuild test -scheme BabyTracker -destination 'platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853' -only-testing:BabyTrackerUITests`.

## Task 1: S1 — Fixtures and vectors in current shapes (no Sources changes)

- Files:
  - `Server/api/fixtures/v1/{feed-*,timer-*,sleep,mutations,sync-page,snapshot-page,outcomes,canonical-vectors,manifest}.json`
  - `internal/protocol/{compatibility,canonical,content_hash}_test.go`
  - `Packages/BabyTracker/Tests/BabyProtocolTests/{ProtocolFixtureConformance,ContentHash,CanonicalProtocol}Tests.swift`
- Gates: server-test, test, server-protocol-e2e.

## Task 2: S2 — Inbound rules

Current versions only, lenient deletes, headers and feature checks removed, explicit family locks, capability strings removed.
- Files:
  - `protocol/{mutations,sync,auth}.go`, `httpapi/sync.go`, `sync/{mutation,paging,snapshot}.go`
  - delete `sync/*_boundary*.go` and `httpapi/*_boundary_test.go`
  - `records/{validation,repository}.go`, `testkit/fixtures.go`
  - `cmd/{protocol-e2e-client,sync-performance-client}/main.go`
  - `docs/protocol/v1.md`, `api/openapi.yaml`, `api/supplements.md`
- New test `sync/interim_client_test.go`, replaying 2961ca1's exact wire shapes with all four headers:
  - feed delete at v2 and sleep delete at v1 are accepted;
  - feed create at v3 and at v1 return `invalid_request`.
- Gates: server-test, server-smoke, server-protocol-e2e. The e2e runs main's Swift client, which is 2961ca1, against B, so it also proves interim compatibility.

**Controller notes:**
- Ruling B-R5: deletes accept any version 1..current, permanently.
- The `interim_client_test.go` must pin the **exact** JSON bodies and headers the 2961ca1 client sends (derive them from `Packages/BabyTracker/Sources/BabySync/FamilySyncHTTPClient.swift` and `BabyStore/MutationFactory.swift` at `d1daa6a`), not regenerate them from current Swift code later.

## Task 3: S3 — Migration 27, part 1

preconditions, RLS bracket, v1 component rebuild, version conversion, jsonb rewrite of journal and receipts, operation normalisation, staging cleanup, legacy function drops.
- Also add a `testpg` helper that applies migrations only up to a given version (e.g. `NewThrough(t, 26)`), using a filtered `fs.FS`.
- Files:
  - `migrations/000027_format_cleanup.sql`, `internal/migrate/compatibility.json` (re-pin the sha in every task that edits 27)
  - `internal/testpg/database.go`, the min/max assertions in `internal/migrate/compatibility_test.go`
  - new `internal/records/format_cleanup_migration_test.go`, seeded at 26 with raw SQL under a tenant context
- Gates: server-test, server-smoke.

**Controller notes:**
- **Ruling B-R2 overrides the plan text above:** do NOT implement the v1 component rebuild. Instead, the preconditions raise if any feed has `component_version = 1`, any timer has `component_version = 1`, or any journal/receipt payload has a scalar feed shape (`sides`/`volume_ml`/`contents`/`foods`/`amount` keys) or `completed_sides`. The jsonb rewrite then only adds missing keys (feed `feeding_session_id: null`; timer `session_id`, `current_bottle_started_at` null; sleep `sleep_session_id: null`) and bumps versions; verify against the current Go marshal for each kind exactly which keys v4/v5/v2 require (e.g. component `started_at`/`ended_at`, bottle `seconds`).
- **Ruling B-R4:** tests must apply migrations as the migrator role, exactly as production `compose run migrate` does (check `internal/testpg` and `deploy/postgres/init-users.sh`), and include a test that fails if the RLS bracket is removed (rows unchanged).
- Legacy journal operations `timer_start_bottle`/`timer_switch_side` (and any `timer_pause`/`timer_attach_bottle`) become `update`.

## Task 4: S4 — Migration 27, part 2

the join and the appended journal heads.
- Test matrix:
  - a gap of exactly 1800 or 900 s joins, one second more does not;
  - a completed sleep blocks a feed join, and so does an open sleep;
  - solids never join; diapers do not block; chained joins;
  - existing v4 links are kept; a session across midnight keeps its root.
- Assert every journal head equals the snapshot expression for its entity.
- Interim-client test: take a cursor at 26, migrate, then `ApplyAndPull` with the old cursor returns the appended changes carrying session IDs.
- Gates: server-test.

**Controller notes:**
- Ruling B-R3: only entries with no session link are evaluated; existing links are never changed.

## Task 5: S5 — Strict stored shapes in the protocol package

- `protocol/entities.go`: single-shape marshal; decode by kind only; remove `MigratedComponentID`, `upgradeComponentPayload` and `CanonicalizeLegacyComponents`.
- `canonical.go`: one hash formula.
- Delete `legacy_upgrade_test.go` and the v1 parts of `timer_v2_decode_test.go`.
- Gates: server-test, server-protocol-e2e.

## Task 6: S6 — Storage cleanup

- The records layer stops reading and writing the scalar columns and child tables: `records/{feeds,timers,repository,child_delete,sleeps,validation,snapshot}.go`, with the snapshot SQL reduced to one branch.
- Migration 27, part 3: drops, pinned CHECKs, privilege manifest, forced RLS back on, with a test.
- Scripts: `smoke.sh:199`, `check-principals.sh:38,51-52,288,301-302` (gate them to below 27), `protocol-e2e.sh:42-50` (assert on `feed_components` instead), `scripts/protocol_e2e_test.go:68`.
- Schema tests: `database/{records,timer}_schema_test.go`, `records/rls_test.go`, `testkit/postgres.go`.
- Gates: server-test, server-smoke, server-protocol-e2e, server-build.

**Controller notes:**
- Note: in production `putFeed` still writes the scalar columns and child tables for v2+ feeds; after this task nothing reads or writes them, then migration 27 drops them.

## Task 7: S7 — Dry-run tooling and rollout note

- `Server/scripts/format-cleanup-{preflight,postflight}.sql`
- `Server/scripts/format-cleanup-dry-run.sh`
- a `docs/verification` rollout template
- Gates: run the dry-run on a dump of the protocol-e2e database, plus server-smoke.

**Controller notes:**
- The dry-run script must run entirely on the Unraid host (disposable container from the same postgres image digest, restored from a fresh dump, migrated by the B image) so family data never leaves the server; it prints only counts, invariant results and a session listing (times, kinds, part counts, gaps — no notes or names).
- Write the rollout runbook into `docs/verification/2026-10-02-format-cleanup-rollout.md` as a template (phase S and phase C), following spec §Runbook.

## Task 8: C1 — Single-format writes

The legacy fields still exist at this point.
- Files: `BabyStore+{Feeding,Parts,Timers,RemoteTimers}.swift`, `FeedEntry.swift:58-90`, `FeedingSessions.swift:36-45,123`, `EntryPayload.swift:69-90`, `MutationFactory.swift:48-79` (constant versions; remove the `JSONValue` overload).
- Gates: test, build, server-protocol-e2e.

**Controller notes:**
- Ruling B-R6: the client track starts only after the server track is merged to main and deployed.

## Task 9: C2 — Wire decode cleanup

- Files:
  - `WireModels.swift:121-185`
  - `BabyStore+RemoteApply.swift:415-475` (key-guessing and camelCase keys)
  - `BabyStore+RemoteTimers.swift:280-302`
  - `ContentHash.swift` (v1 paths and `sides`)
  - `ImportManifestSnapshot.swift:200-270`, `BabyStore+Import.swift:325,351`
  - `FamilySyncHTTPClient.swift:69-72`, `EndpointIdentity.swift:69-70`
  - tests: `HTTPRouteTests`, `WireShapeTests`, `RemoteTimerApplyTests`, `ContentHashTests`
- Gates: test, build, server-protocol-e2e.

## Task 10: C3 — Baseline schema and store fallback

- Files:
  - `Models/SchemaV1–V8.swift` → one schema; `BabyStoreMigrationPlan.swift`, `Schema.swift`
  - delete `LegacyMigrationBridge.swift`, `FeedComponentMigrationBridge.swift`, `BabyStore+ComponentRepair.swift` and its call at `AppEnvironment.swift:220-228`
  - remove the legacy fields from `FeedEntry` and `ActiveTimer`; `FeedComponent.swift:70-125`
  - add the fallback
  - delete `SchemaMigrationTests`, `FeedComponentMigrationTests`, `GrowthModelMigrationTests`, `LegacyFeedUpgradeTests`, `Fixtures/Recovery/*`, `Fixtures/PreSyncAppGroupStore.sqlite`, `scripts/fixtures/RecoveryFixtureBuilder.swift`
  - add `BaselineStoreTests`: a fresh open; a store with a different schema is moved aside and a fresh one opened; the widget path never moves files
- Gates: test, build, UI tests.

**Controller notes:**
- Ruling B-R7.

## Task 11: C4 — Release verification

- Add an owner-reinstall step to the Swift two-caregiver e2e: the owner starts from an empty store with its existing session, imports nothing, replays the journal from 0, and ends with matching counts.
- Gates: everything — test, build, UI tests, server-test, server-smoke, server-protocol-e2e.

