# Caregiver Sync Wiring Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make two caregivers' iPhones actually exchange records through the deployed private server, and prove it with a two-client end-to-end test before any manual device test.

**Architecture:** Every sync building block already exists and is unit-tested in isolation (`SyncCoordinator`, `ImportCoordinator`, `FamilySyncHTTPClient`, `SessionRefresher`, store outbox and remote apply), but nothing in the app instantiates them. This plan adds the three missing transports (session refresh, import resolver, persisted session restore), one `FamilySyncEngine` that owns activation and scheduling, wires it into the app, and adds a two-client test against the real server image.

**Tech Stack:** Swift 6.2, SwiftUI, SwiftData, Swift Testing, Go server (unchanged), Docker Compose protocol-e2e harness.

**Spec:** `docs/superpowers/specs/2026-09-24-private-family-sync-deployment-design.md`, `docs/superpowers/plans/2026-09-20-family-sync-ios-client.md` (P3.4–P3.7), and the decision recorded below.

> **Status (2026-09-26):** Tasks 1–6 plus inserted tasks 1b, 4b–4e and the final-review fix wave are merged to `main` at `a5c6948` and the server is redeployed. Evidence: `docs/verification/2026-09-26-caregiver-sync.md`. Device steps remain open.

## Decision (2026-09-25, from the user)

- The family **owner** uploads their existing local history when creating the family (`ImportMode.preserveLocalIDs`).
- A **joining** caregiver starts fresh: their phone points at the shared realm and pulls a snapshot; entries logged locally before joining stay in the local realm, untouched and not uploaded.

## Global Constraints

- Server: deployed at `https://baby-sync.example-tailnet.ts.net`, instance `<instance-uuid>`, `BT_MODE=self_hosted`. No server code change in this plan.
- Access tokens live 10 minutes; self-hosted sessions have a hard 24 h absolute age, after which only fresh Apple proof continues the session.
- Sync request capabilities stay `["records-v1", "live-hints-v1"]`; feed and timer payloads are version 2 (already produced by `MutationFactory`).
- Every request carries headers `X-Baby-Tracker-Protocol: v1` and `X-Baby-Tracker-Capabilities`; identity bodies carry `{"protocol_version":"v1","capabilities":["identity-v1"]}`.
- Live WebSocket hints are out of scope; foreground polling every 20 s plus an immediate push after each local write stands in for them.
- `EXPECTED_TESTS` in `Makefile` is updated in the same commit as any package test added.

## Review Focus

- The app is killed and relaunched after joining: sync must resume with no user action (Task 2 restore test, Task 6 relaunch check).
- The access token expires between two syncs (10 min): the next sync refreshes once and succeeds rather than surfacing an error (Task 1 test, Task 4 refresh-on-401 test).
- An entry is logged while offline: it stays in the outbox and is delivered on the next successful sync, exactly once (Task 4 test).
- The owner already has composite feeds before creating the family: those feeds arrive on the other phone with every component (Task 6 assertion).
- A joiner's pre-existing local entries are neither uploaded nor lost (Task 4 joiner test).

---

### Task 1: Session refresh over HTTPS

**Files:**
- Modify: `Packages/BabyTracker/Sources/BabySync/HTTPIdentityClient.swift`
- Create: `Packages/BabyTracker/Sources/BabySync/FamilySessionRenewer.swift`
- Test: `Packages/BabyTracker/Tests/BabySyncTests/FamilySessionRenewerTests.swift`

**Interfaces:**
- Produces: `HTTPIdentityClient.refresh(refreshToken: String, at endpoint: ConfirmedEndpoint) async throws -> SessionCredentials`
- Produces: `@MainActor final class FamilySessionRenewer { init(store:keychain:leases:identity:endpoint:); func renew(familyKey: FamilySessionKey) async throws }` — loads the family credentials, calls `SessionRefresher.refresh`, whose `exchange` posts `/v1/auth/refresh` and whose result is saved back under the same family key.

- [x] **Step 1: Failing tests** — a `URLProtocol` stub asserts the request is `POST /v1/auth/refresh` with body keys `protocol_version`, `capabilities == ["identity-v1"]`, `refresh_token`; returns a `SessionResponse` JSON; the test asserts the renewer saved the new access and refresh tokens under the family key and that a second concurrent `renew` does not issue a second request (lease held).
- [x] **Step 2: Run** `swift test --filter FamilySessionRenewerTests` — expect compile failure (no `refresh`).
- [x] **Step 3: Implement** `refresh` (decode `access_token`, `refresh_token`, `expires_at`) and the renewer.
- [x] **Step 4: Run** the focused tests and `make test` (update `EXPECTED_TESTS`).
- [x] **Step 5: Commit** `feat(sync): renew family sessions over HTTPS`.

### Task 2: Persist and restore the family session across launches

**Files:**
- Create: `Packages/BabyTracker/Sources/BabySync/FamilySessionRecord.swift`
- Modify: `Packages/BabyTracker/Sources/BabySync/AccountCoordinator.swift`
- Test: `Packages/BabyTracker/Tests/BabySyncTests/FamilySessionRestoreTests.swift`

**Interfaces:**
- Produces: `struct FamilySessionRecord: Codable { let endpoint: ConfirmedEndpoint; let familyKey: FamilySessionKey }` stored non-secretly in App Group `UserDefaults` under `familySync.session.v1`.
- Produces: `AccountCoordinator.restoreFamilySession() -> FamilySessionKey?` — sets `confirmedEndpoint` and `mode = .shared(key)` only when the Keychain still holds family credentials for that key; clears the record otherwise.
- Modifies: `promoteAfterMembership` saves the record; `revokeDeviceBindingAndSessions` and purge paths clear it.

- [x] **Step 1: Failing tests** — promote, build a new coordinator over the same keychain and defaults, `restoreFamilySession()` returns the key and mode is `.shared`; with the keychain item deleted it returns nil and removes the record.
- [x] **Step 2: Run RED.**
- [x] **Step 3: Implement.**
- [x] **Step 4: GREEN + `make test`.**
- [x] **Step 5: Commit** `feat(sync): restore the family session at launch`.

### Task 3: HTTPS import resolver client

**Files:**
- Create: `Packages/BabyTracker/Sources/BabySync/HTTPImportResolverClient.swift`
- Test: `Packages/BabyTracker/Tests/BabySyncTests/HTTPImportResolverClientTests.swift`

**Interfaces:**
- Produces: `final class HTTPImportResolverClient: ImportResolverClient` with `init(endpoint:tokens:session:)`; routes `POST /v1/family/import/resolve`, `POST /v1/family/import/{id}/resolve-conflict`, `GET /v1/family/import/{id}?page_token=`; headers as in Global Constraints with capabilities `ImportCoordinator.capabilities`; non-2xx decodes the error envelope into `SyncErrorCode`.

- [x] **Step 1: Failing tests** — stubbed routes, methods, headers, and that a request built by `ImportCoordinator.makeResolveRequest` round-trips through encoding.
- [x] **Step 2: RED.** - [x] **Step 3: Implement.** - [x] **Step 4: GREEN + `make test`.**
- [x] **Step 5: Commit** `feat(sync): resolve imports over HTTPS`.

### Task 4: FamilySyncEngine — activation and scheduling

**Files:**
- Create: `Packages/BabyTracker/Sources/BabySync/FamilySyncEngine.swift`
- Test: `Packages/BabyTracker/Tests/BabySyncTests/FamilySyncEngineTests.swift`

**Interfaces:**
- Consumes: Tasks 1–3, `SyncCoordinator`, `ImportCoordinator`.
- Produces:
  ```swift
  @MainActor @Observable public final class FamilySyncEngine {
      public enum Status: Equatable { case idle, syncing, upToDate(Date), offline, needsSignIn, failed(String) }
      public private(set) var status: Status
      public init(store: BabyStore, familyKey: FamilySessionKey, transport: any FamilySyncTransport,
                  importClient: any ImportResolverClient, renew: @escaping () async throws -> Void)
      public func activateAsOwner() async throws   // import(preserveLocalIDs) → shared realm → sync
      public func activateAsJoiner() async throws  // setActiveRealm(shared) → snapshot → sync
      public func syncNow(_ reason: SyncReason) async
      public func localWriteHappened()             // debounced 1 s push
      public func startForegroundPolling()         // every 20 s while active
      public func stopForegroundPolling()
  }
  ```
- `renew` is invoked by `SyncCoordinator.onRefreshRequired`; fresh-proof maps to `.needsSignIn`.

- [x] **Step 1: Failing tests** with an in-memory fake transport shared by two `BabyStore.inMemory` instances (a tiny in-process "server" that assigns cursors and echoes changes): owner with a composite feed activates → joiner activates → joiner sees the feed with identical components; joiner's pre-existing local diaper stays in the local realm and never appears in the fake server; an offline write (transport throws `URLError`) is delivered exactly once on the next `syncNow`; a 401-coded error calls `renew` once then succeeds.
- [x] **Step 2: RED.** - [x] **Step 3: Implement.** - [x] **Step 4: GREEN + `make test`.**
- [x] **Step 5: Commit** `feat(sync): activate and schedule family sync`.

### Task 5: Wire the engine into the app

**Files:**
- Modify: `Packages/BabyTracker/Sources/SettingsFeature/FamilySyncComposition.swift`, `FamilySettingsViewModel.swift`, `FamilySettingsView.swift`
- Modify: `Apps/BabyTrackerApp/AppEnvironment.swift`, `Apps/BabyTrackerApp/RootView.swift`
- Test: `Packages/BabyTracker/Tests/SettingsFeatureTests/FamilySyncWiringTests.swift`

**Interfaces:**
- `FamilySettingsViewModel.createFamily` → `engine.activateAsOwner()`; `acceptInvitation` → `engine.activateAsJoiner()`; `engine` is built from the promoted family key.
- `AppEnvironment` restores the session at launch (Task 2) and builds the engine; `RootView` calls `syncNow(.launch)`, `.foreground` on scene activation, starts/stops polling with scene phase, and calls `localWriteHappened()` on `store.revision` change when the outbox is non-empty.
- Settings → Family shows "Synced HH:MM", "Offline — entries are saved and will sync", or "Sign in again to keep syncing" (button runs Apple sign-in, then `restore`/sync).

- [x] **Step 1: Failing tests** — view-model with fake client and fake engine records `activateAsOwner` after create and `activateAsJoiner` after accept; status copy strings.
- [x] **Step 2: RED.** - [x] **Step 3: Implement.** - [x] **Step 4: GREEN, `make test`, `make build`, full UI suite.**
- [x] **Step 5: Commit** `feat(app): run family sync`.

### Task 6: Two iOS clients through the real server

**Files:**
- Modify: `Server/cmd/protocol-e2e-client/main.go` (second outsider account `Outsider2`, subject `10000000-0000-4000-8000-000000000004`)
- Modify: `Packages/BabyTracker/Tests/FamilySyncClientE2ETests/PinnedSession.swift` (expose both outsiders)
- Create: `Packages/BabyTracker/Tests/FamilySyncClientE2ETests/TwoCaregiverSyncE2ETests.swift`
- Modify: `Server/scripts/protocol-e2e.sh` (proof line)

**Test (run by `make server-protocol-e2e` against the freshly built image over TLS):**
1. Owner store (in-memory) logs a baby, a composite feed (left 8 min, right 6 min, 50 ml breast milk, 30 ml formula) and a diaper while local-only.
2. Owner creates the family through `HTTPFamilyClient`, refreshes to a family-scoped token with `HTTPIdentityClient.refresh`, runs `FamilySyncEngine.activateAsOwner()`.
3. Owner mints a caregiver invitation; Outsider2 redeems it (`confirmJoin`), refreshes, runs `activateAsJoiner()`.
4. Assert the joiner's store has the baby, the feed with exactly those four components in order, and the diaper.
5. Joiner edits the formula bottle to 40 ml and deletes the diaper, starts a sleep timer, syncs; owner syncs; assert owner sees 40 ml, no diaper, and a running sleep timer.
6. Owner stops the sleep; joiner syncs and sees a completed sleep and no running timer.

- [x] **Step 1: Write the test and the Go change; run `make server-protocol-e2e`; expect failure until Tasks 1–5 are in.**
- [x] **Step 2: Fix until GREEN; record the proof line.**
- [x] **Step 3: Commit** `test(e2e): two caregivers sync through the real server`.

### Task 7: Device acceptance

**Files:**
- Create: `docs/verification/2026-09-25-caregiver-sync.md`

- [x] **Step 1: All gates** — `make test`, `make build`, full UI suite, `make server-test`, `make server-smoke`, `make server-protocol-e2e`.
- [x] **Step 2: Confirm the deployed server image source matches `main` for `Server/internal`, `Server/migrations`, `Server/cmd`, `Server/api`; `/readyz` 200 over Tailscale.**
- [ ] **Step 3: Build and install on `Phone A` and `Phone B`.** _(Phone A installed 2026-09-26; the second caregiver's device must be added to the signing profiles first)_
- [ ] **Step 4: Hand the user the manual checklist** (create family on owner phone, invite by QR, join on the second caregiver's phone, log on each phone, see it on the other within ~20 s, relaunch both, airplane-mode write then reconnect).
- [x] **Step 5: Commit evidence.**
