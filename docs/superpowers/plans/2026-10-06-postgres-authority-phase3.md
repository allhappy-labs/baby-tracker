# PostgreSQL Authority — Phase 3 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make every phone request one database transaction behind a cached readiness state and in-memory rate limits. Then add the background work a hosted node needs: coalesced live hints, a wake worker over per-device acknowledged cursors that wakes backgrounded phones through Apple push (APNs), bounded retention sweeps, the family purge and audit partition rolling. Self-hosting runs the same code; push is optional there.

**Architecture:**
- **Request path.** A sync or snapshot request reads and judges its body first. It then opens one transaction that authenticates by access-token digest, sets the tenant context and evaluates family policy in one pipelined round trip. The caller's own refusals (`Admit`) and the in-memory rate limit (`Charge`) run inside that transaction at their legacy positions, then `sync.Service.Exchange`/`SnapshotPage` runs and the transaction commits. Neither `family.Service.Current` read nor the per-request readiness check remains.
- **Background work.** A `readiness.Monitor` caches node readiness. It runs on a small background pool on the runtime credential, which also carries the audit verifier, the key inventory and the authority sweeps.
- **Maintenance work.** Retention, the family purge, the wake worker and audit partition rolling run on the maintenance pool (the `baby_tracker_maintenance` credential), granted exactly what they need by migration 000031.
- **Workers.** Every worker is bounded per tick and runs one transaction per batch. It is idempotent, skips rows a request holds, and locks family rows `FOR NO KEY UPDATE` in ID order, never an account or session row after a family row.
- **Push.** A phone registers its APNs device token through one additive authenticated route. The wake worker re-announces through the live hub to devices with a live connection, and sends a contentless background push through a new `Server/internal/apns` client to lagging devices without one, at most once per 20 minutes per device. The iOS app registers for remote notifications, uploads its token, and answers a background push with one sync pull.

**Tech Stack:** Go 1.26.2, pgx v5.11 (pipelined batches, per-connection statement cache, `pgxpool.Config.ShouldPing`), PostgreSQL 17 (partial indexes, range partitions, SECURITY DEFINER functions, RLS, `FOR NO KEY UPDATE SKIP LOCKED`), Go `net/http` HTTP/2 with ES256 provider tokens (`crypto/ecdsa`) for APNs, the disposable-Docker test harness (`Server/scripts/test-db.sh`), SwiftPM host tests for the iOS client (`Packages/BabyTracker`, counted by `make test`), and one `xcodebuild` simulator build on the shared simulator.

**Reuse ruling (APNs):** `main` already has a production APNs transport for Live Activities that this branch does not: `Server/internal/liveactivity/{apns.go,provider_token.go}`, `Server/internal/config/live_activity.go` (`LoadAPNSCredentials`, `ReadAPNSPrivateKey`, the `BT_APNS_*` variables), an identitytest APNs recorder behind `/__identitytest/apns` (`Server/cmd/baby-tracker-server/live_activity_transport_identitytest.go`), the disposable-harness key generation (`Server/scripts/lib/disposable-sync.sh:222-262` on `main`), the iOS `ProvisioningProfileEnvironmentResolver` (`Packages/BabyTracker/Sources/BabySync/LiveActivityAPNsEnvironment.swift`), `Apps/BabyTrackerLiveActivityPush.entitlements`, and the read-only signing check `scripts/live-activity-push-signing.sh`.
- Merging `main` is not part of Phase 3: it has 143 commits on legacy storage and its own `000028_live_activity_push.sql`, which collides with this branch's `000028_mcp_integrations.sql` (spec "Boundaries": reconcile before any merge).
- So Phase 3 ports those pieces, not reinvents them: the provider token, the transport core, the response classification, the key-file rules and environment names, the recorder and the profile resolver are copied from `main` with their behaviour unchanged, under push-neutral names (`internal/apns`, `config.APNSCredentials`, `PushEnvironmentResolver`), and `Apps/BabyTrackerLiveActivityPush.entitlements` is added byte-identical to `main`'s.
- At the reconciliation (Phase 4/6 carry), `main`'s `liveactivity` transport and `LiveActivityAPNsEnvironment` become thin users of these, so the code exists once.
- Cost if wrong: one reconciliation step re-points `liveactivity` at `internal/apns`.

**Spec:** docs/superpowers/specs/2026-10-04-postgres-authority-performance-design.md

**Migration ruling:** Task 2 writes `000031_request_path_retention.sql` whole: every schema change Phase 3 needs, plus manifest 31 and compatibility 31.
- 000029 and 000030 are FROZEN and untouched.
- Until Task 19, 000031 may be amended only for a review fix, and each amendment regenerates its compatibility digest. Task 19 writes `000031 FROZEN` in its header, regenerates the digest once more, and runs the gates on that file.
- The push-token table, the push columns on `device_sync_cursors` and the two push definer functions are part of Task 2's 000031 from the start, not a later amendment.
- Why: one coherent migration per phase, as Phases 1 and 2 did.
- Cost if wrong: a reviewer re-checks the digest at Task 19.

**Wire ruling (owner decision, 2026-10-06):** the spec keeps the wire protocol unchanged. By the product owner's decision D4, Phase 3 adds exactly one additive surface: `PUT` and `DELETE /v1/devices/push-token` under the new capability `push-token-v1`, advertised by `/v1/server` only when the server has APNs wakes configured. Every existing route, response byte and error code stays unchanged. A server without push does not register the route (404) and does not advertise the capability; older clients never call it.

**Request-shape ruling:** A sync or snapshot request is one transaction: `authority.(*Service).WithRequestTx` around `sync.Service.Exchange`/`SnapshotPage`.
- **Before the transaction.** The handler reads the body, decodes it and validates headers, capabilities and limits. Nothing is read from the network while a transaction is open.
- **Refusal order is unchanged on the wire.** A refusal found before the transaction is held and returned by `Admit`, after authentication. So a bad credential still answers 401 before a 400/409/413, as `TestContinuationFamilyResolutionWinsBeforeUntrustedTokenLookup` and the handler order always had it.
- **Rate limit.** It is charged by `Charge` after family policy and before any care statement. It is in memory, so it costs no round trip. A refusal rolls back.
- **Principal methods stay.** `ApplyAndPull`, `FullSnapshot` and `Apply` keep their signatures for tests and the capacity harness. They no longer read the family first: the guard's grant must name the principal's session family.

**Rate-limit ruling:**
- The DB-backed fixed-window buckets (`app.take_rate_limits`, 000005) are replaced by an in-memory, sharded store with exactly their semantics:
  - fixed windows aligned to `now.Truncate(window)`;
  - all-or-none across the charges of one request;
  - a denial's `Retry-After` is the latest blocking window end.
- Fixed windows are kept rather than continuous token refill because `Retry-After` and the MCP quota are wire-visible and pinned by tests (`mcpapi/refusal_test.go:83`, `scripts/mcp_e2e_test.go:960`).
- **No phone-path limit survives a restart:**
  - restarts are operator actions, not something a client can trigger;
  - the restart-sensitive limits (auth challenge, exchange, refresh and invitation redeem) guard 32-byte random secrets (`family/invitations.go:16`, `auth/refresh.go:184`), so a reset window does not weaken them.
- **The one limit kept in PostgreSQL** is `export_rate_reservations` (one export per family and per account per 6 hours, `export/service.go:122-142`). It is written in the same transaction as the export job it admits, and an export is the most expensive operation the node runs, so a restart must not reset it.

**Pool ruling:** The spec's "one small background pool" is two small pools, because the work splits across two credentials (least privilege):
- `background`: runtime credential, 2 connections, counted in the connection budget. It carries the readiness monitor, the audit verifier, the key inventory and the authority sweeps (sessions, challenges, refresh receipts).
- `maintenance`: the existing maintenance login, a member of `baby_tracker_maintenance`. It carries the export worker, care retention, the family purge, the wake worker and audit partition rolling.

The request pool carries requests and authority commands only, the purge commands included (their invalidations must reach the hub's subscription).

**Retention ruling** (constants in `Server/internal/maintenance/retention.go`):
- **Cutoffs.** A cutoff is computed from the clock that stamped the column: the app clock (`Now`) for `accepted_at`, `consumed_at`, `revoked_at`, `expires_at` and `seen_at`. Database `now()` is used for `next_attempt_at` (Task 6).
- **Retention windows:**
  - mutation receipts: 30 days (spec), with their charges released;
  - continuation bindings and snapshot sessions: their `expires_at` plus 1 hour, so a just-expired token still answers `page_expired`;
  - auth challenges: their `expires_at`;
  - recovery receipts: `LEAST(refresh_expires_at, effective_deadline)`;
  - consumed, revoked and refresh-expired sessions: 24 hours, and never while a refresh receipt names them (decision D2). After 24 hours, replaying a consumed refresh token answers `session_revoked`, not lineage revocation;
  - device push tokens: whenever the device has no active family-scope session (hourly sweep), and at once on an APNs `410 Unregistered` or `400 BadDeviceToken`. A token is never used after the commit that ended its session, because every send re-checks the session (Task 15);
  - device cursors: 35 days unseen;
  - expired or failed export jobs: 7 days after `expires_at`.
- **Kept.** Care entities and tombstones are kept.

**Worker ruling:**
- **Shape.** Every sweep does at most one bounded batch per tick in one transaction, with `lock_timeout = 2s` and `statement_timeout = 10s`.
- **Locks.** Rows are chosen with `FOR UPDATE SKIP LOCKED` (sessions, challenges, receipts of the authority) or behind family rows locked `FOR NO KEY UPDATE SKIP LOCKED` in ID order (care receipts). No worker locks an account row. No worker takes a session or account row after a family row.
- **Recovery.** A rolled-back batch changes nothing, and the next tick repeats it.
- **The purge** is a sequence of idempotent steps keyed on the family's persisted status.

**Wake ruling:**
- **Tracking.** The request path records progress in memory only: a device's presented cursor (acknowledged) and the high water of the last final page served to it (delivered). The wake worker flushes that progress to `device_sync_cursors` and compares it with `families.next_sequence`.
- **Sending.** A lagging device is considered at most once per 60 s, and at most 500 per tick. A device with a live connection gets a re-announced high water through the hub (coalesced per family). A device without one gets an APNs background push, if push is configured and the device registered a token, at most once per 20 minutes per device and at most 100 per tick (decision D4, APNs ruling below).

**APNs ruling (decision D4):**
- **Payload: `{"aps":{"content-available":1}}`, nothing else.** No family hint, opaque or not.
  - The app syncs its one active realm whatever a push says (`SyncCoordinator.syncOnce` reads `store.activeRealm`), so a hint would change nothing on the phone.
  - Any identifier, even an opaque one, is per-family metadata handed to Apple and stored in its push queue. Leaving it out is free.
  - No care data, names or counts ever leave the server in a push.
- **Headers:** `apns-push-type: background`, `apns-priority: 5` (Apple requires 5 for background pushes), `apns-topic` = the app bundle ID (`com.olhapi.babytracker` in production, from `BT_APNS_BUNDLE_ID`), `apns-expiration` = send time + 1 hour, a random `apns-id`. A wake older than an hour is replaced by the next lag check, so storing it longer only spends the budget.
- **Interval: at most one push per device per 20 minutes (3 an hour).**
  - Apple throttles background pushes and documents that more than two or three an hour can be dropped. A 20-minute floor keeps every device inside that.
  - The cost is latency for a phone in the background: up to 20 minutes behind after its first wake, while it stays backgrounded. A foreground phone has the live connection or the 20 s poll, and a phone that pulls to the high water is not pushed again until the family moves past it.
  - The budget is reserved in the database before the send. A crash or a failed send never causes a second push inside the interval.
- **Rate and connections:** at most 100 pushes per 2-second tick (50 a second), 8 in flight, over one reused HTTP/2 connection per APNs environment. At the load model (under 10 writes a second, about four devices a family) the steady need is under 30 a second.
- **Responses:**
  - `200` is accepted.
  - `410 Unregistered` and `400 BadDeviceToken` delete the token, only if it is still the token that was sent.
  - `429` pauses all pushes until `Retry-After`, and at least 60 s.
  - `5xx` and transport failures pause with exponential backoff from 2 s to 2 minutes, reset by a success.
  - `403 ExpiredProviderToken`/`InvalidProviderToken` re-sign the provider token (no more often than every 20 minutes, Apple's floor) and pause 60 s.
  - Any other `4xx` (topic, configuration) pauses 5 minutes and logs once at error level.
  - Logs never contain a device token, a provider token or the key.

**Audit partition ruling:**
- 000031 replaces the empty 2027–2031 yearly partitions of `security_audit_events` with monthly partitions.
- A definer function, `app.ensure_audit_partitions(timestamptz)`, run hourly by the maintenance worker, keeps three months ahead, so no audit row reaches the default partition.
- **Retention of audit history is not in this phase** (decision D1). Monthly partitions make it a cheap `DROP` later, where yearly ones would need rewriting billions of rows.

**Round-trip targets after the collapse** (HTTP request to response, warm statement cache, readiness and rate limit in memory). Pinned in Task 5 (`TestSyncHTTPRoundTrips`, `TestPagedHTTPRoundTrips`) and Task 6 (`TestLiveHandshakeIsOneStatement`).

| Request shape | Today | Target |
|---|---:|---:|
| Pull-only, one page | 12 measured (R2), plus the per-request readiness check and the limiter transaction | **3** (BEGIN+authority, pull, COMMIT) |
| Batch of 1, 20 or 100 mutations with a one-page pull | 13 + readiness + limiter | **4** (BEGIN+authority, Fetch, writes+pull, COMMIT) |
| Pull with a continuation token issued | 13 + … | **4** |
| Continuation with more / last continuation | 13 / 12 + … | **4 / 3** |
| First snapshot page (with or without more) | 13 + … | **4** |
| Snapshot continuation with more / last | 13 / 12 + … | **4 / 3** |
| Refused by authentication, `Admit`, policy or `Charge` | 1–12 | **2** (BEGIN+authority, ROLLBACK) |
| `GET /v1/live` handshake | Authenticate + `Current` + `Authorize` + two limiter transactions | **1** statement, no transaction |
| `PUT` / `DELETE /v1/devices/push-token` (new) | — | **3** (BEGIN+authority, upsert or delete, COMMIT); **2** when refused |
| `readiness.Check` | ping + two migration reads + role audit + `server_state` per request | **0** |

**Owner decisions (2026-10-06, binding):**
- **D1 Audit retention: deferred to Phase 5.** Measured: a `session_rotated` row is 453 bytes, plus about 64 bytes of primary-key index; at about 6M rotations a day that is about 3.1 GB a day. Phase 3 lays monthly partitions so a later retention is a cheap `DROP`.
- **D2 Consumed-session window: 24 hours.** Measured: a session row is 278 bytes, about 0.55 KB with indexes, so 24 hours at 70 rotations a second keeps about 3.3 GB. The trade-off, accepted: after 24 hours, a replay of a consumed refresh token answers `session_revoked` instead of revoking the lineage. A thief who rotated a stolen token first keeps that lineage (until its deadline) if the legitimate phone comes back more than a day later; that phone signs in again.
- **D3 Maintenance credential: required in every mode** (Task 3). Compose already provisions `export_password`.
- **D4 Wake delivery: build Apple push (APNs) now, in Phase 3** (Tasks 13–15, 17–18). Push stays optional for self-hosters: without APNs configuration, wakes go through the live hub only.

**Blocked on owner (none of these blocks writing or testing code; each blocks a device or production use):**
- **Apple Developer portal (team TEAM123456):** enable the Push Notifications capability on the App ID `com.olhapi.babytracker`, regenerate the `BabyTracker Development - TEAM123456` profile so it carries `aps-environment`, and install it. `main`'s runbook (`docs/runbooks/family-live-activity-push.md:172`) records that the installed profiles lack `aps-environment` (checked 2026-10-04). Until then device builds keep `Apps/BabyTracker.entitlements` and only simulator builds carry `aps-environment` (Task 17); the switch for device builds is one build-setting change, carried to Phase 6.
- **APNs key at deploy:** the production key (key ID `KEY1234567`, team `TEAM123456`, file in the owner's credentials folder) is mounted by the operator as `BT_APNS_KEY_FILE` (mode 0600) with `BT_APNS_TEAM_ID`, `BT_APNS_KEY_ID`, `BT_APNS_BUNDLE_ID=com.olhapi.babytracker` and `BT_APNS_WAKES_ENABLED=true`. It is never committed, printed or used by a test; tests generate a throwaway P-256 key. Unraid wiring is Phase 6.
- **Production APNs environment:** TestFlight and App Store builds register production tokens; this tree has no distribution signing, so only the sandbox path is exercised (as on `main`). Carried to Phase 6.

## Global Constraints
- "A background monitor owns one cached readiness state. It pings the pool and checks the schema version on a short interval. It runs the role-privilege audit at startup, after migrations and every few minutes. Requests read the cached state; when it is not ready they fail closed with 503. Nothing else readiness-related runs per request."
- "Read and validate the request body before opening a transaction."
- "Begin. In one pipelined round trip: set the tenant context and run the authentication lookup (HMAC of the access token, then the session primary key). Status, expiry, epoch and role are checked from that row."
- "Pull-only requests take no lock and read under the transaction snapshot."
- "Requests with mutations lock the family row once for the whole batch."
- "Apply the batch (see Write path), read the pull page, commit."
- "Snapshot pages, history, MCP reads and the live-connection handshake follow the same shape. Revocation applies to every transaction that starts after the revoking transaction commits; an in-flight transaction completes, as today."
- "Phone-path rate limits are in-memory sharded token buckets. No request-path rate limit writes to the database. Any limit that must survive a restart is named in the plan with its reason."
- "All request-path statements are prepared and cached per connection."
- "Fixed statement sequences are batched into one round trip."
- "One request pool, sized from measurement, plus one small background pool for readiness, verifiers, cleanup and wakes."
- "No transaction stays open across network waits."
- "The live hub stays in-process. After commit the server publishes 'family changed up to N', coalesced per family over about 50 ms; devices pull."
- "Revoking a session or removing a member closes that device's live connections after the commit."
- "Each connection has a bounded memory budget, a write deadline and a cheap heartbeat."
- "LISTEN/NOTIFY is removed (single node)."
- "Push wake intents leave the write transaction: a background worker compares each device's acknowledged cursor with the family sequence."
- "Mutation receipts: 30 days."
- "Page and snapshot sessions: until their TTL."
- "Expired auth challenges and exchange material: swept."
- "Consumed sessions: pruned after the refresh-retry window."
- "A cleanup worker runs these sweeps; hot tables get tuned autovacuum settings. Care entities and tombstones are kept."
- "Lock the involved account rows, then family rows, each in ID order." Phase 1 ruling: accounts → families → sessions; the family lock is `FOR NO KEY UPDATE`.
- "Every index is justified by a named request-path query" / "Fill factor below 100 on tables with frequent in-place updates" / "Row-level security remains on every family-scoped table as defence in depth".
- "Old binaries fail closed: the schema compatibility version is bumped and pre-migration binaries refuse to start."
- Out of scope: "Wire-protocol changes." Every response byte and error code a shipped client sees is unchanged, except the documented `page_expired`/`invalid_page_token` equivalence after a swept binding (both mean "take a snapshot" to the client, `SyncRetryPolicy.swift:73`). **Owner override (D4):** one additive surface, `PUT`/`DELETE /v1/devices/push-token` under `push-token-v1`, advertised only when push is configured (wire ruling above).
- The earlier family-sync design, which this spec's wake section continues: "Hosted mode may send an APNs background wake containing no family payload" (`docs/superpowers/specs/2026-09-19-family-sync-server-design.md:586`). The push carries no family payload.
- APNs credentials are configuration, not authority ("Session-token, identity and other secret keys remain mounted secret files"): the key is a mounted file read once at startup, never logged.
- Load model for every bound in this plan: "about 70 refreshes per second, 500 to 700 pulls per second, under 10 care writes per second, 4,000 open live connections".
- Phase 1 carry: "handlers take family and role from the Grant, never from Principal.Scope".
- Migrations 000029 and 000030 are FROZEN (compatibility digests `00b48588…205d` and `c52ffb37…549b`).
- Owner's credentials rule (D4): "Production credentials (team TEAM123456, key ID KEY1234567, key file in the owner's credentials folder) are operator-provided at deploy. They must never be committed, printed or used by tests." Every test and harness generates a throwaway P-256 key; `TestNoProductionAPNsKeyInTheServerTree` (Task 13) guards the tree.
- iOS: SwiftPM host tests wherever possible. The only `xcodebuild` steps are the simulator builds in Tasks 17 and 19. Any `xcodebuild` step must use only `platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853`, must run only after `pgrep -fl xcodebuild` prints nothing, runs one at a time, and never creates, resets or erases a simulator. `xcrun simctl push D17101C9-01EB-488D-AFE1-58E6C089B853 …` may deliver a test push to it.

## Review Focus
1. **A retention worker deleting a receipt that a retry still needs.** A phone retries a mutation 29 days 23 hours after it was accepted, while the retention sweep is running and a writer holds the family row. The retry must still get `already_applied`. Only receipts at least 30 days old by the clock that stamped them may be deleted, and the sweep must never wait on a family a request holds. Pinned in Task 10 (`TestReceiptRetentionKeepsTheRetryWindow`).
2. **Charge-release accounting drift.** Mixed batches create receipts across creates, updates, deletes, refusals, timer stops and replays. They are swept in small batches, with one batch rolled back mid-way. After every batch, `families.care_charged_bytes` must equal the charge recomputed from the rows, import staging and live receipts that remain, never negative and never stale. Pinned in Task 10 (`TestReceiptRetentionReleasesExactCharges`).
3. **A deadlock or a long wait between a worker and a request.** Family A takes concurrent sync writes and role changes. Meanwhile the receipt sweep, the session sweep and the wake worker run over A, and the purge of family B (deletion pending, with rows and receipts) runs while the receipt sweep also works on B. Expect no `40P01`, no `55P03`, no request refused for a lock, and both families' counters still equal to their rows. Pinned in Task 11 (`TestMaintenanceAndRequestsNeverDeadlock`).
4. **Lock order and revocation on the one-transaction request path.** Sync batches, pulls and snapshot pages run concurrently with `ChangeRole` (accounts → family) and `Refresh` (account → session). There must be no deadlock. A removal committed before a request's transaction begins refuses that request (`session_revoked`, 2 round trips), and a request already past authorization completes and commits. Pinned in Task 5 (`TestOneTransactionRequestsNeverDeadlockWithCommands`, `TestRevocationRefusesTheNextRequestAndLetsInFlightFinish`).
5. **Wake storms, through the hub and through APNs.** A family takes 100 writes in 50 s while four of its devices lag: each device is woken at most once per 60 s, the hub is told once per wake round, and a device that pulled to the high water is not woken. Then a family takes a write every 30 s for an hour while two of its devices are connected and two are backgrounded with tokens: the connected ones never get a push, each backgrounded one gets exactly 3 pushes (one per 20 minutes), no tick sends more than 100, and a removed member's device gets none. A `429` stops every send until its `Retry-After` (at least 60 s), and `5xx` answers back off exponentially to 2 minutes. Pinned in Task 8 (`TestWakeWorkerNeverStorms`) and Task 15 (`TestWakeWorkerPushBudgetNeverStorms`, `TestPushBacksOff`, `TestPushServerErrorsBackOff`).

## File Structure

**Create**
- `Server/migrations/000031_request_path_retention.sql` — drops the rate-limit storage, freezes the child scope on refresh receipts, adds retention indexes and `device_sync_cursors`, maintenance grants and policies, monthly audit partitions with `app.ensure_audit_partitions`, autovacuum settings, manifest 31 and compatibility 31.
- `Server/internal/limits/memory.go`, `memory_test.go` — the in-memory fixed-window store.
- `Server/internal/readiness/monitor.go`, `monitor_test.go` — the cached readiness monitor.
- `Server/internal/database/request_path_schema_test.go` — the 000031 assertions.
- `Server/internal/authority/request.go`, `request_test.go` — `Request`, `WithRequestTx`/`WithRequestReadTx`, `AuthorizeRequest`, `RequestGuard`.
- `Server/internal/authority/retention.go`, `retention_test.go` — `PruneSessions`, `PruneChallenges`, `PruneRefreshReceipts`, `FamilyPurgeWork`.
- `Server/internal/sync/serve.go`, `serve_test.go`, `request_concurrency_test.go` — `ServeSync`/`ServeSnapshot` and Review Focus 4.
- `Server/internal/httpapi/request_roundtrip_test.go` — HTTP-level round-trip pins.
- `Server/internal/wake/{tracker.go,worker.go,hub_sender.go,worker_test.go,tracker_test.go}` — the wake worker.
- `Server/internal/maintenance/{doc.go,loop.go,retention.go,session_sweep.go,audit_partitions.go,care_retention.go,purge.go,export_test.go,loop_test.go,session_sweep_test.go,audit_partitions_test.go,care_retention_test.go,purge_test.go,concurrency_test.go}`.
- `Server/internal/care/caretest/counters.go` — `AssertCounters`, moved out of `care/apply_test.go`.

**Modify**
- `Server/internal/limits/limiter.go`, `limiter_test.go`.
- `Server/internal/database/{pool.go,pool_test.go,roles.go,roles_test.go}`, and the schema tests that pin version 30.
- `Server/internal/migrate/{compatibility.json,compatibility_test.go}`.
- `Server/internal/auth/refresh.go` — `AccessDigests`.
- `Server/internal/authority/{guard.go,sessions.go,sessions_test.go,guard_test.go}`.
- `Server/internal/sync/{exchange.go,paging.go,snapshot.go,mutation.go}` and its tests.
- `Server/internal/httpapi/{router.go,sync.go,import.go,live.go,auth.go}` and their tests.
- `Server/internal/importer/{resolver.go,conflict.go}`.
- `Server/internal/export/{service.go,model.go,worker.go}`.
- `Server/internal/realtime/hub.go`, `hub_test.go`.
- `Server/internal/delivery/observer.go`.
- `Server/internal/family/service.go` — `LiveAuthorizer` removed.
- `Server/internal/care/{entity.go,apply_test.go}`.
- `Server/internal/testkit/fixtures.go`.
- `Server/internal/capacity/fixture_identitytest.go`.
- `Server/internal/config/{config.go,recovery_cleanup.go}`.
- `Server/cmd/baby-tracker-server/{main.go,authority_runtime.go,export_worker.go,mcp.go}` and tests.
- `Server/cmd/baby-tracker-overlap-check/*_test.go`.
- `Server/tests/protocol/harness_test.go`.
- `Server/scripts/{check-principals.sh,smoke.sh,smoke_test.go,protocol-e2e.sh,protocol_e2e_test.go,lib/disposable-sync.sh}`.
- `docs/protocol/v1.md` (and the new push-token section), `Server/README.md` — only where their text becomes false.
- `Packages/BabyTracker/Sources/BabySync/SyncCoordinator.swift`, `Packages/BabyTracker/Tests/BabySyncTests/SyncCoordinatorTests.swift`.
- Push (Tasks 13–18):
  - `Server/internal/config/config.go` (`APNSWakes`), `Server/internal/protocol/{auth.go,sync.go}` (`push-token-v1`, its surface), `Server/internal/limits/policy.go` (two push-token scopes), `Server/internal/httpapi/router.go` (the route and its options), `Server/internal/realtime/hub.go` (`DeviceConnected`), `Server/internal/wake/worker.go`;
  - `Packages/BabyTracker/Sources/BabySync/FamilySyncHTTPClient.swift`, `Packages/BabyTracker/Sources/SettingsFeature/{FamilySyncComposition.swift,FamilySettingsViewModel.swift}`, `Apps/BabyTrackerApp/{BabyTrackerApp.swift,AppEnvironment.swift}`, `Apps/BabyTracker-Info.plist`, `BabyTracker.xcodeproj/project.pbxproj`, `Makefile` (`EXPECTED_TESTS`);
  - `Packages/BabyTracker/Tests/FamilySyncClientE2ETests/OnePartTimerSyncE2ETests.swift` (its private `Race` moves to `E2ERace.swift`) and `TwoCaregiverSyncE2ETests.swift` (`Phone`'s push resolver).

**Create (push, Tasks 13–18)**
- `Server/internal/apns/{doc.go,client.go,provider_token.go,client_test.go,provider_token_test.go}` — the APNs client, ported from `main`'s `liveactivity` transport.
- `Server/internal/config/{apns.go,apns_test.go}` — `APNSCredentials`, `LoadAPNSCredentials`, `ReadAPNSPrivateKey` (ported from `main`'s `config/live_activity.go`), `LoadAPNSWakes`.
- `Server/cmd/baby-tracker-server/{apns_sink.go,apns_sink_identitytest.go,apns_sink_identitytest_test.go}` — the production client, and the identitytest recorder behind `/__identitytest/apns` (ported from `main`).
- `Server/internal/protocol/{push_token.go,push_token_test.go}`, `Server/internal/pushtoken/{service.go,service_test.go}`, `Server/internal/httpapi/{push_token.go,push_token_test.go}` — the route.
- `Server/internal/wake/{push.go,push_test.go}`, `Server/internal/maintenance/{push_tokens.go,push_tokens_test.go}` — APNs delivery and the token sweep.
- `Server/scripts/apns_secrets_test.go` — no production key material in the server tree.
- `Packages/BabyTracker/Sources/BabyProtocol/PushTokenDTOs.swift`; `Packages/BabyTracker/Sources/BabySync/{PushEnvironmentResolver.swift,PushTokenRegistrar.swift,BackgroundWakeHandler.swift}`; `Apps/BabyTrackerApp/PushAppDelegate.swift`; `Apps/BabyTrackerLiveActivityPush.entitlements` (byte-identical to `main`'s).
- `Packages/BabyTracker/Tests/BabySyncTests/{PushTokenRegistrarTests.swift,BackgroundWakeHandlerTests.swift,PushEnvironmentResolverTests.swift}`, `Packages/BabyTracker/Tests/BabyProtocolTests/PushTokenDTOTests.swift`, `Packages/BabyTracker/Tests/FamilySyncClientE2ETests/{BackgroundWakePushE2ETests.swift,E2ERace.swift,APNSRecorder.swift}`.

**Delete**
- `Server/internal/limits/repository.go`.
- `Server/internal/realtime/{postgres.go,postgres_test.go}`.
- `Server/internal/access/family_resolver.go`.
- `syncFamilyResolver` and `syncCursorResyncer` in `cmd/baby-tracker-server/main.go`.
- `database/identity_runtime_test.go:17-110` (`TestTakeRateLimitsIsWeightedAtomicAndCappedAcross100Callers`) and its helpers `rateCharges`/`assertRateDecision`. They are ported in memory in Task 1.

**Phase 3 carry map** (every ledger, review and spec item; deferrals carry their reason):

| Item (source) | Owner |
|---|---|
| One transaction per sync/snapshot request; both `Current` reads gone; Authenticate folded in (Phase 1 ledger :171, R2 C1, Phase 2 plan carries) | Tasks 4–5 |
| Readiness off the request path (spec) | Task 3 |
| In-memory rate limits (spec) | Task 1 |
| pgxpool `ShouldPing` (Phase 1 :115); prepared statements (spec) | Task 3 |
| `BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE` read but unused (Phase 1 :184) | Task 3 |
| Import/export/live sites of the old guard shape (Phase 1 task-12 report) | Task 6 |
| `StatusPage` lock-free, access check first (R1 m3) | Task 6 |
| Export `next_attempt_at` on the Go clock (Phase 1 :200) | Task 6 |
| Hub: overflow sweep without deadline on the delivery goroutine; Authorize→Bind revocation window (Phase 1 :173); invalidation drop-on-full (Phase 1 :108; recheck exists since Phase 1 Task 12) | Task 7 |
| Delete the idle `PostgresListener` and its resync from cursor 0 (Phase 2 :122, R2 C4) | Task 7 |
| 50 ms per-family coalescing (spec) | Task 7 |
| Wake worker with per-device acknowledged cursors (Phase 2 plan, wake ruling) | Task 8 |
| APNs background wakes: client, configuration, device-token route, worker delivery, iOS registration and handling, E2E proof (owner decision D4; earlier designs `2026-09-19-family-sync-server-design.md:586,1054`, `2026-09-20-family-sync-beta-readiness.md` Task 2) | Tasks 2, 13–15, 17–18 |
| Reuse `main`'s Live Activity APNs transport, key rules, recorder and profile resolver (main `Server/internal/liveactivity`, `config/live_activity.go`, `LiveActivityAPNsEnvironment.swift`) | Tasks 13, 17 (ported); the de-duplication at reconciliation is carried to Phases 4/6 |
| Consumed sessions at ~70/s, challenges, abandoned recovery receipts (Phase 1 :179, :184) | Task 9 |
| Audit partition roll before 2032 (Phase 1 :50) | Tasks 2, 9 |
| Audit retention at ~6M rows/day (Phase 1 final M5) | **Deferred to Phase 5:** needs decision D1, and the verifier must anchor at the oldest kept partition |
| `care_receipts` 30 days with charge release; counters invariant (R1 I2 ruling, final-fix-report :81) | Task 10 |
| Bindings and snapshot sessions by TTL (Phase 2 plan carries) | Task 10 |
| Export jobs retention (task brief) | Task 10 |
| Autovacuum tuning; fillfactor 90 cannot HOT-update (Phase 2 :49) | Task 2 (autovacuum). **HOT deferred to Phase 5:** it needs measurement on the reference node. |
| Family purge deletes care data; DELETE-capable role; cascade trigger order test; import-charge reconstruction (R1 carries, Phase 2 :116, :122) | Tasks 2, 10, 11 |
| In-flight `purge_command_id` after a crash (Phase 1 :97) | Task 11 |
| Lost-ack refresh recovery re-derives from the frozen scope (Phase 1 final) | Task 12 |
| iOS: one `drainPages()` after `takeSnapshot()` (USER DECISION, Phase 2 :164, R2 C6) | Task 16 |
| MCP reads and history in one transaction (spec "follow the same shape") | **Deferred to Phase 5:** MCP's `CheckIntegration` deliberately refuses before reading the body (`mcpapi/handler.go:335-353`), MCP runs at about 30 requests/min per integration, and its quota becomes in-memory in Task 1. Measure before restructuring. |
| Export `Download` streams inside its read transaction (Phase 1 task-12 report) | **Deferred to Phase 5:** owner-only, 10/hour/IP, at most 50 MiB, and it verifies the whole archive before the first byte. Chunked re-authorization needs measurement. |
| Request pool sized from measurement (spec) | **Deferred to Phase 5** (load harness) |

---

### Task 1: In-memory rate limits

**Files:**
- Create `Server/internal/limits/memory.go` and `Server/internal/limits/memory_test.go`.
- Modify `Server/internal/limits/limiter.go`:
  - `Options` loses `Repository`;
  - `prepareCharges` returns `[]preparedCharge`, not JSON;
  - `Service` holds a `rateStore`.
- Delete `Server/internal/limits/repository.go`.
- Modify `Server/internal/limits/limiter_test.go`:
  - port `TestRepository*` (:187-519) to the memory store and drop `testpg`;
  - `TestChargePayloadIsKeyedCanonicalAndContainsNoRawKey` asserts on the prepared charges;
  - the four `…UseIsolatedLegacyStorageNamespaces` tests become `TestMemoryScopesAndKeysAreIsolated`, because there is no storage remap left.
- Modify the composition sites: `Server/cmd/baby-tracker-server/main.go:270-277`, `cmd/baby-tracker-server/mcp_test.go:221-226`, `cmd/baby-tracker-overlap-check/migration_overlap_http_test.go:25-29` and `import_http_test.go:27-31,89-93`.

**Interfaces:** Consumes the existing `limits.Limiter`, `Charge`, `Policy` and `Decision` (unchanged). Produces:
```go
package limits
type Options struct {
	Pepper     [32]byte
	Now        func() time.Time
	MaxEntries int // live buckets kept; 0 means DefaultMaxEntries
}
const DefaultMaxEntries = 1 << 20 // about 100 MiB worst case; the load model needs about 200k
func New(options Options) (Limiter, error) // in memory; no database dependency
// unexported, for tests in package limits:
type preparedCharge struct { scope string; digest [32]byte; cost, limit int; windowStart, windowEnd time.Time }
type rateStore interface { take(context.Context, []preparedCharge, time.Time) (Decision, error) }
func newLimiter(store rateStore, pepper [32]byte, now func() time.Time) *Service
func newMemoryStore(maxEntries int) *memoryStore
func (s *memoryStore) len() int
var errLimiterFull = errors.New("rate limiter full")
```

- [ ] **Step 1: Write the failing tests** `Server/internal/limits/memory_test.go`:
```go
func memoryLimiter(t *testing.T, now *time.Time) *Service {
	t.Helper()
	return newLimiter(newMemoryStore(0), testPepper(), func() time.Time { return *now })
}

func TestMemoryEnforcesCapAcrossOneHundredCallers(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	limiter := memoryLimiter(t, &now)
	policy := Policy{Count: 25, Window: time.Minute}
	var allowed, failures atomic.Int32
	var wait sync.WaitGroup
	for range 100 {
		wait.Go(func() {
			decision, err := limiter.Take(context.Background(), ScopeAuthChallengeIP, "192.0.2.99", policy)
			if err != nil {
				failures.Add(1)
				return
			}
			if decision.Allowed {
				allowed.Add(1)
			}
		})
	}
	wait.Wait()
	if failures.Load() != 0 || allowed.Load() != 25 {
		t.Fatalf("allowed=%d failures=%d", allowed.Load(), failures.Load())
	}
}

func TestMemoryWeightedChargesAreAllOrNone(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	limiter := memoryLimiter(t, &now)
	policy := Policy{Count: 3, Window: time.Minute}
	a := Charge{Scope: ScopeAuthRefreshAccount, Key: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Cost: 2, Policy: policy}
	b := Charge{Scope: ScopeAuthRefreshDevice, Key: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Cost: 2, Policy: policy}
	if d, err := limiter.TakeMany(t.Context(), []Charge{a}); err != nil || !d.Allowed || d.Remaining != 1 {
		t.Fatalf("seed %+v %v", d, err)
	}
	if d, err := limiter.TakeMany(t.Context(), []Charge{a, b}); err != nil || d.Allowed || d.RetryAfter != 56*time.Second {
		t.Fatalf("combined %+v %v, want denied with Retry-After 56s (window end 12:04:00)", d, err)
	}
	if d, err := limiter.TakeMany(t.Context(), []Charge{b}); err != nil || !d.Allowed || d.Remaining != 1 {
		t.Fatalf("b was charged by the refused set: %+v %v", d, err)
	}
}

func TestMemoryDenialRetryAfterIsTheLatestBlockingWindowEnd(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	limiter := memoryLimiter(t, &now)
	minute := Charge{Scope: ScopeSyncPullDevice, Key: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Cost: 1, Policy: Policy{Count: 1, Window: time.Minute}}
	hour := Charge{Scope: ScopeInvitationRedeemAccount, Key: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Cost: 1, Policy: Policy{Count: 1, Window: time.Hour}}
	if d, _ := limiter.TakeMany(t.Context(), []Charge{minute, hour}); !d.Allowed {
		t.Fatal("first set refused")
	}
	if d, _ := limiter.TakeMany(t.Context(), []Charge{minute, hour}); d.Allowed || d.RetryAfter != 56*time.Minute+56*time.Second {
		t.Fatalf("retry %v, want the hour window's end", d.RetryAfter)
	}
	now = now.Add(57 * time.Second) // next minute window
	if d, _ := limiter.TakeMany(t.Context(), []Charge{minute}); !d.Allowed {
		t.Fatal("next minute window still refused")
	}
}

func TestMemoryScopesAndKeysAreIsolated(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	limiter := memoryLimiter(t, &now)
	key := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	one := Policy{Count: 1, Window: time.Minute}
	scopes := []string{ScopeSyncMutationAccount, ScopeAuthRefreshAccount, ScopeLiveAttemptAccount, ScopeExportAccount, ScopeMCPAccount, ScopeFamilyStructuralAccount}
	for _, scope := range scopes {
		if d, err := limiter.Take(t.Context(), scope, key, one); err != nil || !d.Allowed {
			t.Fatalf("%s shares a bucket with another scope: %+v %v", scope, d, err)
		}
	}
	if d, _ := limiter.Take(t.Context(), ScopeSyncMutationAccount, "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", one); !d.Allowed {
		t.Fatal("another key shares the bucket")
	}
}

func TestMemoryOversizedWeightedChargeIsDeniedWithoutPartialQuota(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	limiter := memoryLimiter(t, &now)
	small := Charge{Scope: ScopeSyncMutationDevice, Key: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Cost: 1, Policy: Policy{Count: 10, Window: time.Minute}}
	huge := Charge{Scope: ScopeSyncMutationFamily, Key: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Cost: 11, Policy: Policy{Count: 10, Window: time.Minute}}
	if d, _ := limiter.TakeMany(t.Context(), []Charge{small, huge}); d.Allowed {
		t.Fatal("oversized charge admitted")
	}
	if d, _ := limiter.TakeMany(t.Context(), []Charge{small}); d.Remaining != 9 {
		t.Fatalf("refused set charged small: remaining %d", d.Remaining)
	}
}

func TestMemoryExpiredBucketsAreSwept(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	store := newMemoryStore(0)
	limiter := newLimiter(store, testPepper(), func() time.Time { return now })
	for i := range 10000 {
		if _, err := limiter.Take(t.Context(), ScopeLiveAttemptIP, fmt.Sprintf("198.51.%d.%d", i/256, i%256), Policy{Count: 60, Window: time.Minute}); err != nil {
			t.Fatal(err)
		}
	}
	if store.len() != 10000 {
		t.Fatalf("live buckets %d", store.len())
	}
	now = now.Add(2 * time.Minute)
	for i := range 4096 { // 4,096 new keys touch, and so sweep, every one of the 64 shards
		_, _ = limiter.Take(t.Context(), ScopeLiveAttemptIP, fmt.Sprintf("203.0.%d.%d", i/256, i%256), Policy{Count: 60, Window: time.Minute})
	}
	if store.len() != 4096 {
		t.Fatalf("%d buckets live, want only the 4,096 of the current window", store.len())
	}
}

func TestMemoryRefusesNewKeysAtCapacity(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	limiter := newLimiter(newMemoryStore(memoryShards), testPepper(), func() time.Time { return now }) // one bucket per shard
	var full int
	for i := range 2000 {
		_, err := limiter.Take(t.Context(), ScopeLiveAttemptIP, fmt.Sprintf("198.51.%d.%d", i/256, i%256), Policy{Count: 60, Window: time.Minute})
		if err != nil {
			if ErrorCodeOf(err) != CodeLimitUnavailable || !IsRetryable(err) {
				t.Fatalf("a full limiter refused with %v", err)
			}
			full++
		}
	}
	if full == 0 {
		t.Fatal("the limiter grew past its bound")
	}
}

// The request path's limiter has no database dependency at all.
func TestLimitsImportNoDatabaseDriver(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imported := range pkg.Imports {
		if strings.Contains(imported, "jackc/pgx") || strings.HasPrefix(imported, "babytracker/server/internal/database") {
			t.Fatalf("limits imports %s", imported)
		}
	}
}
```
- [ ] **Step 2: Run to see the failure:** `rtk go -C Server test ./internal/limits -race -count=1 -run 'TestMemory|TestLimitsImport'`. Expected: `undefined: newMemoryStore`, and `limits imports github.com/jackc/pgx/v5/pgxpool`.
- [ ] **Step 3: Implement** `memory.go`, then switch `limiter.go` to it and delete `repository.go`:
```go
const memoryShards = 64

type bucketKey struct {
	scope       string
	digest      [32]byte
	windowStart int64 // UnixMicro
}

type bucket struct {
	count     int
	windowEnd time.Time
}

type memoryShard struct {
	mu      sync.Mutex
	buckets map[bucketKey]bucket
	sweptAt time.Time
}

// memoryStore is app.take_rate_limits (000005) in process memory: fixed
// windows, all-or-none across one request's charges, and a denial that
// reports the latest blocking window end. Shards are locked in index order,
// so concurrent multi-charge takes cannot deadlock. A shard drops its ended
// windows at most once a second, when it is next used.
type memoryStore struct {
	shards   [memoryShards]memoryShard
	perShard int
}

var errLimiterFull = errors.New("rate limiter full")

func newMemoryStore(maxEntries int) *memoryStore {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	store := &memoryStore{perShard: max(1, maxEntries/memoryShards)}
	for i := range store.shards {
		store.shards[i].buckets = make(map[bucketKey]bucket)
	}
	return store
}

func shardOf(digest [32]byte) int { return int(digest[0]) % memoryShards }

func (s *memoryStore) take(_ context.Context, charges []preparedCharge, now time.Time) (Decision, error) {
	var touched [memoryShards]bool
	for _, c := range charges {
		touched[shardOf(c.digest)] = true
	}
	for i := range s.shards {
		if touched[i] {
			s.shards[i].mu.Lock()
			defer s.shards[i].mu.Unlock()
		}
	}
	keys := make([]bucketKey, len(charges))
	decision := Decision{Allowed: true, Remaining: math.MaxInt}
	var blockingEnd time.Time
	for i, c := range charges {
		shard := &s.shards[shardOf(c.digest)]
		shard.sweep(now)
		keys[i] = bucketKey{scope: c.scope, digest: c.digest, windowStart: c.windowStart.UnixMicro()}
		current, live := shard.buckets[keys[i]]
		if current.count+c.cost > c.limit {
			if c.windowEnd.After(blockingEnd) {
				blockingEnd = c.windowEnd
			}
			continue
		}
		if !live && len(shard.buckets) >= s.perShard {
			return Decision{}, errLimiterFull
		}
		decision.Remaining = min(decision.Remaining, c.limit-current.count-c.cost)
	}
	if !blockingEnd.IsZero() {
		return Decision{Allowed: false, RetryAfter: max(time.Second, time.Duration(math.Ceil(blockingEnd.Sub(now).Seconds()))*time.Second)}, nil
	}
	for i, c := range charges {
		shard := &s.shards[shardOf(c.digest)]
		shard.buckets[keys[i]] = bucket{count: shard.buckets[keys[i]].count + c.cost, windowEnd: c.windowEnd}
	}
	return decision, nil
}

func (shard *memoryShard) sweep(now time.Time) {
	if now.Sub(shard.sweptAt) < time.Second {
		return
	}
	shard.sweptAt = now
	for key, b := range shard.buckets {
		if !now.Before(b.windowEnd) {
			delete(shard.buckets, key)
		}
	}
}

func (s *memoryStore) len() int {
	total := 0
	for i := range s.shards {
		s.shards[i].mu.Lock()
		total += len(s.shards[i].buckets)
		s.shards[i].mu.Unlock()
	}
	return total
}
```
  - `prepareCharges` keeps every validation it has today (`limiter.go:128-170`). It returns `[]preparedCharge` with `digest = HMAC(pepper, scope‖0‖normalized key)`, sorted by (scope, digest), duplicates refused.
  - `New(Options{Pepper, Now, MaxEntries})` returns `newLimiter(newMemoryStore(MaxEntries), Pepper, Now)`. A zero pepper or a nil `Now` is `CodeInvalidLimit`.
  - `TakeMany` wraps any store error, `errLimiterFull` included, as the retryable `CodeLimitUnavailable`, exactly as it wrapped repository errors.
  - Composition: `main.go:270-277` becomes `limiter, err := limits.New(limits.Options{Pepper: cfg.Auth.RateLimitPepper, Now: time.Now})`, and the other sites the same with their test pepper and clock.
- [ ] **Step 4: Run green:**
  - `rtk go -C Server test ./internal/limits -race -count=1`;
  - `rtk proxy env BT_TEST_PACKAGES='./cmd/... ./internal/httpapi ./internal/mcpapi' bash Server/scripts/test-db.sh`;
  - `(cd Server && rtk go vet ./... && rtk go vet -tags identitytest ./...)`.

  The `rate_limit_buckets` table is still in the schema, unused; Task 2 drops it.
- [ ] **Step 5: Commit:** `rtk git add -A Server/internal/limits Server/cmd && rtk git commit -m "feat(limits): keep rate limits in sharded memory, off the database"`

### Task 2: Migration 000031 — maintenance grants, wake cursors, push tokens, retention indexes, audit partitions

**Files:**
- Create `Server/migrations/000031_request_path_retention.sql`.
- Modify `Server/internal/migrate/compatibility.json`: `"min": 31, "max": 31`; append `{version: 31, name: "000031_request_path_retention.sql", sha256: <shasum -a 256>}`.
- Modify `Server/internal/migrate/compatibility_test.go`:
  - every `30` bound becomes `31` (:50, :131, :210, :259, :282, :309, :340, :370);
  - add `TestBinaryRefusesSchemaThirty`, as the `000030` case at :362 does.
- Create `Server/internal/database/request_path_schema_test.go`.
- Modify `Server/internal/database/roles.go`: add `VerifyMaintenanceRole`.
- Bump fully-migrated assertions from 30 to 31:
  - `database/care_schema_test.go:24,27,460`, keeping `:513`, which migrates *through* 30;
  - `database/import_schema_test.go:58,151`;
  - `database/identity_schema_test.go:56`;
  - `database/family_schema_test.go:523`;
  - `database/authority_schema_test.go:23,51`;
  - `database/refresh_recovery_schema_test.go:24`;
  - `database/roles_test.go` (30 → 31, and its synthetic future version 31 → 32 at :107, :152);
  - `tenant/context_test.go:409`;
  - `scripts/lib/disposable-sync.sh:277` (`$schema == 31`).
- Remove the rate-limit expectations:
  - `database/identity_schema_test.go:30,37,45,160,207` and `database/authority_schema_test.go:231`;
  - delete `database/identity_runtime_test.go:17-110` and its helpers `rateCharges`/`assertRateDecision`. They are ported in memory in Task 1.
- Modify the scripts:
  - `scripts/check-principals.sh:61-79,313-331`: in both modes the `>= 30` blocks stop listing `baby_tracker_maintenance` among the principals with no care access, and a `>= 31` block asserts the maintenance grants below;
  - `scripts/smoke.sh:202-213`: the same, plus `to_regclass('public.rate_limit_buckets') IS NULL`;
  - `scripts/smoke_test.go:305-316`: pin the new strings.

**Interfaces:** Produces the DDL objects below, and:
```go
package database
// VerifyMaintenanceRole checks that the maintenance pool's login holds
// exactly the privileges manifest version compatibleVersion records for
// baby_tracker_maintenance. The maintenance login cannot read the manifest,
// so the manifest is read through manifest (a runtime pool) and the
// privileges are compared on the maintenance connection.
func VerifyMaintenanceRole(ctx context.Context, manifest, maintenance *pgxpool.Pool, compatibleVersion int64) error
```
Implementation: `allowed, err := loadRoleInventory(ctx, manifest, "baby_tracker_maintenance", v)`, then `verifyRoleInventory(ctx, maintenance, allowed)`. Verified against schema 30 in a throwaway copy: the existing maintenance manifest matches the login exactly.

- [ ] **Step 1: Write the failing test** `Server/internal/database/request_path_schema_test.go`:
```go
func TestSchemaIsVersionThirtyOne(t *testing.T) {
	f := testpg.New(t)
	var v int64
	if err := f.Runtime.QueryRow(t.Context(), `SELECT current_version FROM schema_compatibility WHERE singleton`).Scan(&v); err != nil || v != 31 {
		t.Fatalf("version=%d err=%v", v, err)
	}
	if err := database.VerifyRuntimeRole(t.Context(), f.Runtime, 31); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyMaintenanceRole(t.Context(), f.Runtime, f.Maintenance, 31); err != nil {
		t.Fatal(err)
	}
	var gone bool
	_ = f.Owner.QueryRow(t.Context(), `SELECT to_regclass('public.rate_limit_buckets') IS NULL
		AND to_regprocedure('app.take_rate_limits(jsonb,timestamptz)') IS NULL`).Scan(&gone)
	if !gone {
		t.Fatal("rate-limit storage survived 000031")
	}
}

func TestMaintenanceGrantsAreExact(t *testing.T) {
	f := testpg.New(t)
	for _, c := range []struct {
		sql  string
		want bool
	}{
		{`bool_and(has_table_privilege(current_user,t,'SELECT') AND has_table_privilege(current_user,t,'DELETE')) FROM unnest(ARRAY['public.care_entities','public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions','public.import_sessions']) t`, true},
		{`bool_or(has_table_privilege(current_user,t,'INSERT') OR has_table_privilege(current_user,t,'UPDATE') OR has_table_privilege(current_user,t,'TRUNCATE')) FROM unnest(ARRAY['public.care_entities','public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions','public.import_sessions']) t`, false},
		{`bool_and(has_column_privilege(current_user,'public.families',c,'UPDATE')) FROM unnest(ARRAY['care_record_count','care_charged_bytes','care_entity_count','care_wire_bytes']) c`, true},
		{`has_column_privilege(current_user,'public.families','status','UPDATE') OR has_column_privilege(current_user,'public.families','next_sequence','UPDATE') OR has_table_privilege(current_user,'public.families','DELETE')`, false},
		{`has_table_privilege(current_user,'public.device_sync_cursors','SELECT') AND has_table_privilege(current_user,'public.device_sync_cursors','INSERT') AND has_table_privilege(current_user,'public.device_sync_cursors','UPDATE') AND has_table_privilege(current_user,'public.device_sync_cursors','DELETE')`, true},
		{`has_function_privilege(current_user,'app.ensure_audit_partitions(timestamp with time zone)','EXECUTE')`, true},
	} {
		var got bool
		if err := f.Maintenance.QueryRow(t.Context(), `SELECT `+c.sql).Scan(&got); err != nil || got != c.want {
			t.Fatalf("maintenance: %s = %v (%v)", c.sql, got, err)
		}
	}
	for _, c := range []string{
		`has_table_privilege(current_user,'public.device_sync_cursors','SELECT')`,
		`has_function_privilege(current_user,'app.ensure_audit_partitions(timestamp with time zone)','EXECUTE')`,
		`bool_or(has_table_privilege(current_user,t,'DELETE')) FROM unnest(ARRAY['public.care_entities','public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions']) t`,
	} {
		var got bool
		if err := f.Runtime.QueryRow(t.Context(), `SELECT `+c).Scan(&got); err != nil || got {
			t.Fatalf("runtime: %s = %v (%v)", c, got, err)
		}
	}
}

func TestRetentionIndexesNameTheirSweeps(t *testing.T) {
	f := testpg.New(t)
	for index, want := range map[string]string{
		"refresh_receipts_expiry_idx":       "(LEAST(refresh_expires_at, effective_deadline))",
		"sessions_revoked_idx":              "(revoked_at) WHERE (status = 'revoked'",
		"sessions_expired_idx":              "(refresh_expires_at) WHERE (status = 'active'",
		"sync_snapshot_sessions_expiry_idx": "(expires_at)",
		"device_sync_cursors_seen_idx":      "(seen_at)",
	} {
		var def string
		if err := f.Owner.QueryRow(t.Context(), `SELECT pg_get_indexdef($1::regclass)`, "public."+index).Scan(&def); err != nil || !strings.Contains(def, want) {
			t.Fatalf("%s: %q (%v), want %q", index, def, err, want)
		}
	}
}

func TestAuditPartitionsAreMonthlyFromTwentyTwentySeven(t *testing.T) {
	f := testpg.New(t)
	rows, _ := f.Owner.Query(t.Context(), `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
		WHERE i.inhparent='public.security_audit_events'::regclass ORDER BY 1`)
	names, _ := pgx.CollectRows(rows, pgx.RowTo[string])
	if !slices.Contains(names, "security_audit_events_2026") || !slices.Contains(names, "security_audit_events_default") ||
		slices.Contains(names, "security_audit_events_2027") || slices.Contains(names, "security_audit_events_2031") {
		t.Fatalf("audit partitions %v", names)
	}
	var current bool // the month 92 days ahead (never before 2027-01) has its partition, whenever the test runs
	if err := f.Owner.QueryRow(t.Context(), `SELECT to_regclass('public.security_audit_events_' ||
		to_char(greatest(timestamptz '2027-01-01 00:00:00+00', date_trunc('month', (now() + interval '92 days') AT TIME ZONE 'UTC') AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', 'YYYY_MM')) IS NOT NULL`).Scan(&current); err != nil || !current {
		t.Fatalf("no partition 92 days ahead: %v", err)
	}
	var created int
	if err := f.Maintenance.QueryRow(t.Context(), `SELECT app.ensure_audit_partitions(now() + interval '92 days')`).Scan(&created); err != nil || created != 0 {
		t.Fatalf("a second ensure created %d (%v); the migration already covers 92 days", created, err)
	}
	var pgErr *pgconn.PgError
	if _, err := f.Maintenance.Exec(t.Context(), `SELECT app.ensure_audit_partitions(now() + interval '401 days')`); !errors.As(err, &pgErr) || pgErr.Code != "22023" {
		t.Fatalf("a horizon past 400 days: %v", err)
	}
	if _, err := f.Runtime.Exec(t.Context(), `SELECT app.ensure_audit_partitions(now())`); !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("runtime executed the partition roller: %v", err)
	}
}

func TestHotTablesHaveTunedAutovacuum(t *testing.T) {
	f := testpg.New(t)
	for table, want := range map[string]string{
		"care_entities_p00":          "autovacuum_vacuum_scale_factor=0.05",
		"care_entities_p15":          "autovacuum_analyze_scale_factor=0.02",
		"care_receipts":              "autovacuum_vacuum_insert_scale_factor=0.05",
		"sessions":                   "autovacuum_vacuum_scale_factor=0.02",
		"refresh_receipts":           "autovacuum_vacuum_scale_factor=0.05",
		"families":                   "autovacuum_vacuum_scale_factor=0.05",
		"sync_page_bindings":         "autovacuum_vacuum_scale_factor=0.05",
		"sync_snapshot_sessions":     "autovacuum_vacuum_scale_factor=0.05",
		"auth_challenges":            "autovacuum_vacuum_scale_factor=0.05",
		"device_sync_cursors":        "fillfactor=70",
		"security_audit_events_2026": "autovacuum_vacuum_insert_scale_factor=0.05",
	} {
		var options string
		if err := f.Owner.QueryRow(t.Context(), `SELECT coalesce(array_to_string(reloptions, ','), '') FROM pg_class WHERE oid=$1::regclass`, "public."+table).Scan(&options); err != nil || !strings.Contains(options, want) {
			t.Fatalf("%s reloptions %q (%v), want %s", table, options, err, want)
		}
	}
}

func TestRefreshReceiptsFreezeTheChildScope(t *testing.T) {
	f := testpg.New(t)
	var shape string
	if err := f.Owner.QueryRow(t.Context(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='refresh_receipts_child_scope_shape'`).Scan(&shape); err != nil ||
		!strings.Contains(shape, "child_scope = 'account'") {
		t.Fatalf("shape %q %v", shape, err)
	}
}

// Push tokens: the owner's account only, maintenance only through the definers
// and the refused-token delete, and send-time eligibility from the sessions.
func TestPushTokensAreOwnedAndEligibilityFollowsSessions(t *testing.T) {
	f := testpg.New(t)
	ids := seedAuthorityFamilies(t, f) // accounts A and B, families A and B, device A
	ctx := t.Context()
	// Device A's family-scope session in family A, inserted as authority_schema_test.go:137 inserts sessions.
	seedFamilySession(t, f, ids.accountA, ids.deviceA, ids.familyA)
	tokenA := bytes.Repeat([]byte{0xa1}, 32)
	tx := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
	if _, err := tx.Exec(ctx, `INSERT INTO device_push_tokens(device_id,account_id,environment,token,registered_at) VALUES($1,$2,'sandbox',$3,now())`,
		ids.deviceA, ids.accountA, tokenA); err != nil {
		t.Fatal(err)
	}
	var pgErr *pgconn.PgError
	if _, err := tx.Exec(ctx, `INSERT INTO device_push_tokens(device_id,account_id,environment,token,registered_at) VALUES($1,$2,'sandbox',$3,now())`,
		ids.deviceA, ids.accountB, tokenA); !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("a tenant wrote another account's token: %v", err)
	}
	_ = tx.Rollback(ctx)
	tx = tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
	if _, err := tx.Exec(ctx, `INSERT INTO device_push_tokens(device_id,account_id,environment,token,registered_at) VALUES($1,$2,'sandbox',$3,now())`,
		ids.deviceA, ids.accountA, tokenA); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var targets int
	if err := f.Maintenance.QueryRow(ctx, `SELECT count(*) FROM app.push_targets($1::uuid[], $2::uuid[])`,
		[]string{ids.familyA, ids.familyB}, []string{ids.deviceA, ids.deviceA}).Scan(&targets); err != nil || targets != 1 {
		t.Fatalf("eligible targets %d (%v): want only device A in its own family", targets, err)
	}
	if _, err := f.Maintenance.Exec(ctx, `SELECT token FROM device_push_tokens`); err != nil {
		t.Fatalf("maintenance cannot read the token it must delete on 410: %v", err)
	}
	if err := authorityExec(ctx, f.Runtime, `UPDATE sessions SET status='revoked', revoked_at=now(), revoke_reason='member_removed' WHERE device_id=$1`, ids.deviceA); err != nil {
		t.Fatal(err)
	}
	if err := f.Maintenance.QueryRow(ctx, `SELECT count(*) FROM app.push_targets($1::uuid[], $2::uuid[])`,
		[]string{ids.familyA}, []string{ids.deviceA}).Scan(&targets); err != nil || targets != 0 {
		t.Fatalf("a revoked device is still a push target: %d %v", targets, err)
	}
	var pruned int
	if err := f.Maintenance.QueryRow(ctx, `SELECT app.prune_push_tokens(100)`).Scan(&pruned); err != nil || pruned != 1 {
		t.Fatalf("pruned %d (%v), want the revoked device's token", pruned, err)
	}
	var authority string
	if err := f.Maintenance.QueryRow(ctx, `SELECT coalesce(current_setting('app.authority', true), '')`).Scan(&authority); err != nil || authority == "1" {
		t.Fatalf("a definer left the authority flag set: %q %v", authority, err)
	}
}
```
  `seedFamilySession(t, f, account, device, family)` is a new helper beside `seedAuthorityFamilies`: one `authorityExec` of the session insert at `authority_schema_test.go:137` with `scope='family'`, `family_id`, `role='owner'`, `status='active'` and `refresh_expires_at = now() + interval '30 days'`.
```go

// 000031 replaces the 2027–2031 yearly partitions only while they are empty.
func TestMigrationThirtyOneRefusesAuditRowsFromTwentyTwentySeven(t *testing.T) {
	f := testpg.NewThrough(t, 30)
	ctx := t.Context()
	tx, err := f.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.authority','1',true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO security_audit_events(scope,scope_id,seq,prev_hash,hash,kind,command_id,actor_account_id,at,payload)
		VALUES ('account','a1000000-0000-4000-8000-000000000001',1,sha256('p'::bytea),sha256('h'::bytea),'session_rotated',
		'c1000000-0000-4000-8000-000000000001','a1000000-0000-4000-8000-000000000001','2027-02-01T00:00:00Z','{}')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var pgErr *pgconn.PgError
	if err := f.MigrateThrough(ctx, 31); !errors.As(err, &pgErr) || pgErr.Code != "55000" {
		t.Fatalf("migration over a 2027 audit row: %v", err)
	}
}

// An install already at 30 (a test or dev database) reaches 31.
func TestMigrationThirtyOneOverThirty(t *testing.T) {
	f := testpg.NewThrough(t, 30)
	_ = seedAuthorityFamilies(t, f)
	if err := f.MigrateThrough(t.Context(), 31); err != nil {
		t.Fatal(err)
	}
}
```
- [ ] **Step 2: Run to see the failure:** `rtk proxy env BT_TEST_PACKAGES='./internal/database ./internal/migrate' BT_TEST_RUN='TestSchemaIsVersionThirtyOne|TestMaintenance|TestRetentionIndexes|TestAuditPartitions|TestHotTables|TestRefreshReceiptsFreeze|TestPushTokens|TestMigrationThirtyOne|TestCompatibility|TestBinaryRefuses' bash Server/scripts/test-db.sh`. Expected: `version=30` and `undefined: database.VerifyMaintenanceRole`.
- [ ] **Step 3: Write the migration** `Server/migrations/000031_request_path_retention.sql` (complete DDL):
```sql
-- 000031: request path, retention and wakes (Phase 3). Fresh installs, and
-- installs already at 30, only, like 000029 and 000030; legacy installs reach
-- this schema through the Phase 4 importer.

-- 1. Rate limits live in process memory (limits/memory.go): no request-path
-- limit writes to the database, so the bucket table and its definer go.
DROP FUNCTION app.take_rate_limits(jsonb, timestamptz);
DROP TABLE public.rate_limit_buckets;

-- 2. A recovery receipt freezes its child's scope, family and role at the
-- rotation. A lost-acknowledgement retry re-derives the child from them, not
-- from the session row that ForfeitRecovery or StartFamilyPurge rescoped
-- (authority/sessions.go restore). NULL only on receipts written before 31.
ALTER TABLE public.refresh_receipts
  ADD COLUMN child_scope app.session_scope,
  ADD COLUMN child_family_id uuid,
  ADD COLUMN child_role app.member_role,
  ADD CONSTRAINT refresh_receipts_child_scope_shape CHECK (child_scope IS NULL
    OR (child_scope = 'account' AND child_family_id IS NULL AND child_role IS NULL)
    OR (child_scope <> 'account' AND child_family_id IS NOT NULL AND child_role IS NOT NULL));

-- 3. Retention indexes; each names its sweep. (sessions_consumed_idx and
-- auth_challenges_expiry_idx, 000029, sync_page_bindings_expiry_idx and
-- care_receipts_retention_idx, 000030, and export_jobs_expiry_idx, 000019,
-- serve the other sweeps.)
CREATE INDEX refresh_receipts_expiry_idx ON public.refresh_receipts ((LEAST(refresh_expires_at, effective_deadline)));
--   authority.PruneRefreshReceipts: receipts whose child can no longer refresh.
CREATE INDEX sessions_revoked_idx ON public.sessions (revoked_at) WHERE status = 'revoked';
--   authority.PruneSessions: revoked sessions past the retention window.
CREATE INDEX sessions_expired_idx ON public.sessions (refresh_expires_at) WHERE status = 'active';
--   authority.PruneSessions: active sessions whose refresh token expired.
CREATE INDEX sync_snapshot_sessions_expiry_idx ON public.sync_snapshot_sessions (expires_at);
--   maintenance.ExpirySweep: snapshot sessions past their TTL.

-- 4. Device progress for the wake worker. Written only by the maintenance
-- worker from its in-memory tracker, never on the request path.
CREATE TABLE public.device_sync_cursors (
  family_id uuid NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
  device_id uuid NOT NULL CHECK (device_id <> '00000000-0000-0000-0000-000000000000'),
  acked_sequence bigint NOT NULL CHECK (acked_sequence BETWEEN 0 AND 9007199254740991),         -- the cursor it last presented
  delivered_sequence bigint NOT NULL CHECK (delivered_sequence BETWEEN 0 AND 9007199254740991), -- the high water of the last final page served to it
  woken_sequence bigint NOT NULL DEFAULT 0 CHECK (woken_sequence BETWEEN 0 AND 9007199254740991),
  seen_at timestamptz NOT NULL CHECK (isfinite(seen_at)),
  woken_at timestamptz CHECK (woken_at IS NULL OR isfinite(woken_at)),
  pushed_sequence bigint NOT NULL DEFAULT 0 CHECK (pushed_sequence BETWEEN 0 AND 9007199254740991), -- the sequence the last APNs push reserved
  pushed_at timestamptz CHECK (pushed_at IS NULL OR isfinite(pushed_at)),                              -- the 20-minute APNs budget (Task 15)
  PRIMARY KEY (family_id, device_id)           -- wake.Worker flush upsert; lag scan `family_id = ANY($1)`; purge by family
) WITH (fillfactor = 70, autovacuum_vacuum_scale_factor = 0.05);
CREATE INDEX device_sync_cursors_seen_idx ON public.device_sync_cursors (seen_at); -- maintenance.ExpirySweep: devices unseen for 35 days
ALTER TABLE public.device_sync_cursors ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.device_sync_cursors FORCE ROW LEVEL SECURITY;
CREATE POLICY wake_worker ON public.device_sync_cursors TO baby_tracker_maintenance USING (true) WITH CHECK (true);
REVOKE ALL ON public.device_sync_cursors FROM PUBLIC;

-- 4b. APNs device tokens (owner decision D4): one row per device, written by
-- the device's own authenticated request (PUT/DELETE /v1/devices/push-token),
-- read by the wake worker only through app.push_targets, which re-checks the
-- device's session at send time. Tokens are opaque Apple routing handles, not
-- credentials: they are useless without the provider key, so they are stored
-- as bytes (the database is trusted, spec "Trust boundary") and never logged.
CREATE TYPE app.apns_environment AS ENUM ('sandbox', 'production');
CREATE TABLE public.device_push_tokens (
  device_id uuid PRIMARY KEY CHECK (device_id <> '00000000-0000-0000-0000-000000000000'), -- PUT upsert; app.push_targets join; 410 delete
  account_id uuid NOT NULL,
  environment app.apns_environment NOT NULL,
  token bytea NOT NULL CHECK (octet_length(token) BETWEEN 16 AND 256),
  registered_at timestamptz NOT NULL CHECK (isfinite(registered_at)),
  FOREIGN KEY (device_id, account_id) REFERENCES public.devices(id, account_id) ON DELETE CASCADE
) WITH (fillfactor = 80);
ALTER TABLE public.device_push_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.device_push_tokens FORCE ROW LEVEL SECURITY;
-- The tenant reads and writes only its own account's rows; the handler binds the
-- device to the authenticated session's device.
CREATE POLICY push_token_owner ON public.device_push_tokens
  USING (account_id = app.current_account_id()) WITH CHECK (account_id = app.current_account_id());
-- The wake worker deletes a token APNs refused (410 / BadDeviceToken).
CREATE POLICY push_token_maintenance_delete ON public.device_push_tokens FOR DELETE TO baby_tracker_maintenance USING (true);
-- The two definers below run as the table owner under the authority flag.
CREATE POLICY push_token_authority ON public.device_push_tokens USING (app.authority_context()) WITH CHECK (app.authority_context());
REVOKE ALL ON public.device_push_tokens FROM PUBLIC;
GRANT SELECT, INSERT, DELETE ON public.device_push_tokens TO baby_tracker_runtime;
GRANT UPDATE (environment, token, registered_at) ON public.device_push_tokens TO baby_tracker_runtime;
GRANT SELECT (device_id, token), DELETE ON public.device_push_tokens TO baby_tracker_maintenance;

-- app.push_targets: the eligible tokens among (family, device) wake candidates:
-- a token exists, the device is active, and the device holds an active,
-- unexpired family-scope session in that family. Revoking a session, removing a
-- member or logging out makes a device ineligible at the revoking commit.
CREATE FUNCTION app.push_targets(p_family_ids uuid[], p_device_ids uuid[])
RETURNS TABLE(family_id uuid, device_id uuid, environment app.apns_environment, token bytea)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE previous text := current_setting('app.authority', true);
BEGIN
  IF p_family_ids IS NULL OR p_device_ids IS NULL OR cardinality(p_family_ids) <> cardinality(p_device_ids) OR cardinality(p_family_ids) > 1000 THEN
    RAISE EXCEPTION 'invalid push target request' USING ERRCODE = '22023';
  END IF;
  PERFORM set_config('app.authority', '1', true);
  RETURN QUERY
    SELECT v.f, t.device_id, t.environment, t.token
    FROM unnest(p_family_ids, p_device_ids) AS v(f, d)
    JOIN public.device_push_tokens t ON t.device_id = v.d
    JOIN public.devices dv ON dv.id = t.device_id AND dv.account_id = t.account_id AND dv.status = 'active'
    WHERE EXISTS (SELECT 1 FROM public.sessions s                                   -- sessions_device_live_idx
                  WHERE s.device_id = t.device_id AND s.account_id = t.account_id AND s.status = 'active'
                    AND s.scope = 'family' AND s.family_id = v.f AND s.refresh_expires_at > now());
  PERFORM set_config('app.authority', coalesce(previous, ''), true);
END $$;
-- app.prune_push_tokens: deletes at most p_limit tokens whose device holds no
-- active, unexpired family-scope session (signed out, removed, revoked, purged).
CREATE FUNCTION app.prune_push_tokens(p_limit integer) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE
  previous text := current_setting('app.authority', true);
  deleted integer;
BEGIN
  IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 10000 THEN
    RAISE EXCEPTION 'invalid push token prune limit' USING ERRCODE = '22023';
  END IF;
  PERFORM set_config('app.authority', '1', true);
  DELETE FROM public.device_push_tokens WHERE device_id IN (
    SELECT t.device_id FROM public.device_push_tokens t
    WHERE NOT EXISTS (SELECT 1 FROM public.sessions s
                      WHERE s.device_id = t.device_id AND s.account_id = t.account_id AND s.status = 'active'
                        AND s.scope = 'family' AND s.refresh_expires_at > now())
    ORDER BY t.device_id LIMIT p_limit FOR UPDATE SKIP LOCKED);
  GET DIAGNOSTICS deleted = ROW_COUNT;
  PERFORM set_config('app.authority', coalesce(previous, ''), true);
  RETURN deleted;
END $$;
REVOKE ALL ON FUNCTION app.push_targets(uuid[], uuid[]), app.prune_push_tokens(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.push_targets(uuid[], uuid[]), app.prune_push_tokens(integer) TO baby_tracker_maintenance;

-- 5. The maintenance group: care retention, the family purge, wakes and
-- audit partitions. It reads and deletes expired or purged care rows and
-- releases their charges on the family row; it never inserts or updates a
-- care row. RLS policies admit it whole-table, as export_worker (000019) does.
GRANT SELECT, DELETE ON public.care_entities, public.care_receipts, public.sync_page_bindings, public.sync_snapshot_sessions,
  public.import_sessions TO baby_tracker_maintenance;
GRANT SELECT (id, status, next_sequence, care_record_count, care_charged_bytes, care_entity_count, care_wire_bytes)
  ON public.families TO baby_tracker_maintenance;
GRANT UPDATE (care_record_count, care_charged_bytes, care_entity_count, care_wire_bytes) ON public.families TO baby_tracker_maintenance;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.device_sync_cursors TO baby_tracker_maintenance;
DO $$ DECLARE t text; BEGIN
  FOREACH t IN ARRAY ARRAY['care_entities','care_receipts','sync_page_bindings','sync_snapshot_sessions','import_sessions'] LOOP
    EXECUTE format('CREATE POLICY maintenance_read ON public.%I FOR SELECT TO baby_tracker_maintenance USING (true)', t);
    EXECUTE format('CREATE POLICY maintenance_delete ON public.%I FOR DELETE TO baby_tracker_maintenance USING (true)', t);
  END LOOP;
END $$;
CREATE POLICY maintenance_read ON public.families FOR SELECT TO baby_tracker_maintenance USING (true);
CREATE POLICY maintenance_counters ON public.families FOR UPDATE TO baby_tracker_maintenance USING (true) WITH CHECK (true);

-- 6. Security audit history: monthly partitions from 2027, so a retention
-- policy (Phase 5, decision D1) drops whole months. The 2027–2031 yearly
-- partitions are replaced only while empty; the maintenance worker keeps
-- three months ahead through app.ensure_audit_partitions.
DO $$ BEGIN
  PERFORM set_config('app.authority', '1', true);
  IF EXISTS (SELECT 1 FROM public.security_audit_events WHERE at >= '2027-01-01T00:00:00Z') THEN
    RAISE EXCEPTION 'security audit rows from 2027 on exist; 000031 replaces the 2027-2031 partitions only while they are empty'
      USING ERRCODE = '55000';
  END IF;
  PERFORM set_config('app.authority', '', true);
END $$;
DROP TABLE public.security_audit_events_2027, public.security_audit_events_2028, public.security_audit_events_2029,
  public.security_audit_events_2030, public.security_audit_events_2031;
CREATE FUNCTION app.ensure_audit_partitions(p_through timestamptz) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog SET timezone = 'UTC' AS $$
DECLARE
  month_start timestamptz := greatest(timestamptz '2027-01-01 00:00:00+00', date_trunc('month', now()));
  created integer := 0;
  part text;
BEGIN
  IF p_through IS NULL OR NOT isfinite(p_through) OR p_through > now() + interval '400 days' THEN
    RAISE EXCEPTION 'invalid audit partition horizon' USING ERRCODE = '22023';
  END IF;
  WHILE month_start <= p_through LOOP
    part := 'security_audit_events_' || to_char(month_start, 'YYYY_MM');
    IF to_regclass('public.' || part) IS NULL THEN
      EXECUTE format('CREATE TABLE public.%I PARTITION OF public.security_audit_events FOR VALUES FROM (%L) TO (%L) WITH (autovacuum_vacuum_insert_scale_factor = 0.05)',
        part, month_start, month_start + interval '1 month');
      created := created + 1;
    END IF;
    month_start := month_start + interval '1 month';
  END LOOP;
  RETURN created;
END $$;
REVOKE ALL ON FUNCTION app.ensure_audit_partitions(timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.ensure_audit_partitions(timestamptz) TO baby_tracker_maintenance;
DO $$ BEGIN PERFORM app.ensure_audit_partitions(now() + interval '92 days'); END $$;

-- 7. Autovacuum for the hot tables: vacuum and analyze long before the
-- default 20%/10% of a large table has changed.
DO $$ DECLARE r int; BEGIN FOR r IN 0..15 LOOP
  EXECUTE format('ALTER TABLE public.care_entities_p%s SET (autovacuum_vacuum_scale_factor = 0.05, autovacuum_analyze_scale_factor = 0.02)', lpad(r::text, 2, '0'));
END LOOP; END $$;
ALTER TABLE public.care_receipts SET (autovacuum_vacuum_scale_factor = 0.02, autovacuum_vacuum_insert_scale_factor = 0.05, autovacuum_analyze_scale_factor = 0.02);
ALTER TABLE public.sessions SET (autovacuum_vacuum_scale_factor = 0.02, autovacuum_analyze_scale_factor = 0.02);
ALTER TABLE public.refresh_receipts SET (autovacuum_vacuum_scale_factor = 0.05);
ALTER TABLE public.families SET (autovacuum_vacuum_scale_factor = 0.05, autovacuum_analyze_scale_factor = 0.05);
ALTER TABLE public.sync_page_bindings SET (autovacuum_vacuum_scale_factor = 0.05);
ALTER TABLE public.sync_snapshot_sessions SET (autovacuum_vacuum_scale_factor = 0.05);
ALTER TABLE public.auth_challenges SET (autovacuum_vacuum_scale_factor = 0.05);
ALTER TABLE public.security_audit_events_2026 SET (autovacuum_vacuum_insert_scale_factor = 0.05);

-- Manifest 31 = manifest 30 minus the rate-limit function, plus the rows above. Compatibility 31.
INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
  SELECT 31,principal,object_kind,object_identity,privileges FROM public.schema_privilege_manifest WHERE schema_version=30
    AND NOT (object_kind='function' AND object_identity='app.take_rate_limits(jsonb,timestamp with time zone)')
    AND NOT (object_kind IN ('table','column') AND object_identity ~ '^public\.rate_limit_buckets(\.|$)');
INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges) VALUES
  (31,'baby_tracker_maintenance','table','public.care_entities','SELECT,DELETE'),
  (31,'baby_tracker_maintenance','table','public.care_receipts','SELECT,DELETE'),
  (31,'baby_tracker_maintenance','table','public.sync_page_bindings','SELECT,DELETE'),
  (31,'baby_tracker_maintenance','table','public.sync_snapshot_sessions','SELECT,DELETE'),
  (31,'baby_tracker_maintenance','table','public.import_sessions','SELECT,DELETE'),
  (31,'baby_tracker_maintenance','table','public.device_sync_cursors','SELECT,INSERT,UPDATE,DELETE'),
  (31,'baby_tracker_maintenance','column','public.families.id','SELECT'),
  (31,'baby_tracker_maintenance','column','public.families.status','SELECT'),
  (31,'baby_tracker_maintenance','column','public.families.next_sequence','SELECT'),
  (31,'baby_tracker_maintenance','column','public.families.care_record_count','SELECT,UPDATE'),
  (31,'baby_tracker_maintenance','column','public.families.care_charged_bytes','SELECT,UPDATE'),
  (31,'baby_tracker_maintenance','column','public.families.care_entity_count','SELECT,UPDATE'),
  (31,'baby_tracker_maintenance','column','public.families.care_wire_bytes','SELECT,UPDATE'),
  (31,'baby_tracker_maintenance','function','app.ensure_audit_partitions(timestamp with time zone)','EXECUTE'),
  (31,'baby_tracker_maintenance','function','app.push_targets(uuid[],uuid[])','EXECUTE'),
  (31,'baby_tracker_maintenance','function','app.prune_push_tokens(integer)','EXECUTE'),
  (31,'baby_tracker_maintenance','table','public.device_push_tokens','DELETE'),
  (31,'baby_tracker_maintenance','column','public.device_push_tokens.device_id','SELECT'),
  (31,'baby_tracker_maintenance','column','public.device_push_tokens.token','SELECT'),
  (31,'baby_tracker_runtime','table','public.device_push_tokens','SELECT,INSERT,DELETE'),
  (31,'baby_tracker_runtime','column','public.device_push_tokens.environment','UPDATE'),
  (31,'baby_tracker_runtime','column','public.device_push_tokens.token','UPDATE'),
  (31,'baby_tracker_runtime','column','public.device_push_tokens.registered_at','UPDATE');
UPDATE public.schema_compatibility SET current_version=31, introduced_by='000031_request_path_retention.sql', recorded_at=now() WHERE singleton;
```
  The script `>= 31` blocks: maintenance holds `SELECT` and `DELETE` on each care table and on `import_sessions`, with each privilege checked on its own, and no `INSERT`, `UPDATE` or `TRUNCATE`. It holds column `UPDATE` on exactly the four counters, and `public`, the projector and the revoker hold nothing. On `device_push_tokens` the runtime holds `SELECT`, `INSERT`, `DELETE` and column `UPDATE` on exactly `environment`, `token`, `registered_at`; maintenance holds `DELETE` and column `SELECT` on `device_id`, `token`; `app.push_targets` and `app.prune_push_tokens` are executable by maintenance only.
- [ ] **Step 4: Run green.**
  - Record the digest: `shasum -a 256 Server/migrations/000031_request_path_retention.sql` into `compatibility.json`.
  - Run `rtk proxy env BT_TEST_PACKAGES='./internal/database ./internal/migrate ./internal/tenant ./internal/limits' bash Server/scripts/test-db.sh`, then `rtk make server-test`.
  - The fresh-install guards still pass unchanged: `TestMigrationThirtyRefusesLegacyCareRows` migrates explicitly through 30, and the 000029 guard tests through 29.
- [ ] **Step 5: Commit:** `rtk git add Server/migrations/000031_request_path_retention.sql Server/internal/migrate Server/internal/database Server/internal/tenant Server/scripts && rtk git commit -m "feat(schema): maintenance grants, wake cursors, retention indexes and monthly audit partitions (31)"`

### Task 3: Cached readiness, the background and maintenance pools, `ShouldPing` and statement caching

**Files:**
- Create `Server/internal/readiness/monitor.go` and `monitor_test.go`.
- Modify `Server/internal/database/pool.go`:
  - every pool skips the acquire ping until idle for a minute;
  - the request pool's exec mode is pinned to the statement cache;
  - add `OpenBackground`.
- Modify `Server/internal/database/pool_test.go`.
- Modify `Server/cmd/baby-tracker-server/authority_runtime.go`: `databaseReadiness` becomes the monitor's probes plus a cached `ServerState`.
- Modify `Server/cmd/baby-tracker-server/main.go`:
  - open the background and maintenance pools and verify the maintenance login;
  - start the monitor;
  - move the verifier and key inventory to a background authority;
  - the export worker reuses the maintenance pool when its URL is the same.
- Modify `Server/cmd/baby-tracker-server/export_worker.go`: `verifyExportPool` becomes `verifyMaintenancePool(pool, expectedRole)`, used for both pools.
- Modify `Server/internal/config/config.go:116-121`: refuse a configuration without a maintenance credential (decision D3).
- Tests:
  - `cmd/baby-tracker-server/{authority_runtime_test.go,database_pools_test.go,main_test.go}`: drive the monitor's `Refresh`;
  - budgets include `background` (2);
  - fixtures set `BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE`;
  - `internal/config/config_test.go`: the refusal.

**Interfaces:** Consumes `database.VerifyRuntimeRole`, `database.VerifyMaintenanceRole` (Task 2), `migrate.Runner.CompatibleVersion` and `authority.LoadServerState`. Produces:
```go
package readiness
var ErrNotReady = errors.New("service_unavailable: readiness not established") // wraps database.ErrUnavailable
var ErrStale = errors.New("service_unavailable: readiness stale")
type Options struct {
	Fast          func(context.Context) (version int64, err error) // one round trip: ping and schema version
	Audit         func(context.Context, int64) error               // role privileges, server_state, migration digests, budget, key inventory
	FastInterval  time.Duration                                    // 0 means 2s
	AuditInterval time.Duration                                    // 0 means 5m
	Stale         time.Duration                                    // 0 means 3 × FastInterval
	Now           func() time.Time
	Logger        *slog.Logger
}
type Monitor struct{ /* atomic state, last outcome */ }
func New(o Options) (*Monitor, error)
func (m *Monitor) Refresh(ctx context.Context) error // one fast probe, plus the audit when due (start, version change, interval)
func (m *Monitor) Run(ctx context.Context)           // Refresh every FastInterval until ctx ends
func (m *Monitor) Check(context.Context) error       // cached: nil, the last failure, ErrNotReady before the first audit, or ErrStale

package database
const BackgroundConns int32 = 2
const idlePingAfter = time.Minute // pgxpool ShouldPing: a connection idle less than this is used unpinged
func OpenBackground(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) // runtime DSN, MaxConns 2, MinConns 0
```

- [ ] **Step 1: Write the failing tests.**
  - `Server/internal/readiness/monitor_test.go`:
```go
type probes struct {
	mu            sync.Mutex
	version       int64
	fastErr       error
	auditErr      error
	fasts, audits int
}

func (p *probes) fast(context.Context) (int64, error) {
	p.mu.Lock(); defer p.mu.Unlock(); p.fasts++
	return p.version, p.fastErr
}
func (p *probes) audit(context.Context, int64) error {
	p.mu.Lock(); defer p.mu.Unlock(); p.audits++
	return p.auditErr
}

func TestCheckIsNotReadyBeforeTheFirstAudit(t *testing.T) {
	p := &probes{version: 31}
	m, _ := readiness.New(readiness.Options{Fast: p.fast, Audit: p.audit, Now: time.Now})
	if err := m.Check(t.Context()); !errors.Is(err, readiness.ErrNotReady) {
		t.Fatalf("check before any probe: %v", err)
	}
}

func TestCheckServesTheCachedState(t *testing.T) {
	p := &probes{version: 31}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	m, _ := readiness.New(readiness.Options{Fast: p.fast, Audit: p.audit, Now: func() time.Time { return now }})
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	for range 1000 {
		if err := m.Check(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if p.fasts != 1 || p.audits != 1 {
		t.Fatalf("checks ran probes: fast %d audit %d", p.fasts, p.audits)
	}
}

func TestAuditRunsAtStartOnSchemaChangeAndEveryInterval(t *testing.T) {
	p := &probes{version: 31}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	m, _ := readiness.New(readiness.Options{Fast: p.fast, Audit: p.audit, AuditInterval: 5 * time.Minute, Now: func() time.Time { return now }})
	_ = m.Refresh(t.Context()) // start: audit
	now = now.Add(2 * time.Second)
	_ = m.Refresh(t.Context()) // fast only
	p.version = 32             // a migration ran
	now = now.Add(2 * time.Second)
	_ = m.Refresh(t.Context()) // audit
	now = now.Add(5 * time.Minute)
	_ = m.Refresh(t.Context()) // audit (interval)
	if p.fasts != 4 || p.audits != 3 {
		t.Fatalf("fast %d audit %d, want 4 and 3", p.fasts, p.audits)
	}
}

func TestStaleOrFailedStateFailsClosed(t *testing.T) {
	p := &probes{version: 31}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	m, _ := readiness.New(readiness.Options{Fast: p.fast, Audit: p.audit, FastInterval: 2 * time.Second, Now: func() time.Time { return now }})
	_ = m.Refresh(t.Context())
	now = now.Add(7 * time.Second) // no probe for more than 3 intervals
	if err := m.Check(t.Context()); !errors.Is(err, readiness.ErrStale) {
		t.Fatalf("stale state served: %v", err)
	}
	p.auditErr = errors.New("database_role_separation_invalid")
	p.version = 33
	_ = m.Refresh(t.Context())
	if err := m.Check(t.Context()); err == nil {
		t.Fatal("a failed audit was served as ready")
	}
	p.auditErr = nil
	_ = m.Refresh(t.Context()) // the failed audit is retried on the next tick
	if err := m.Check(t.Context()); err != nil {
		t.Fatalf("recovered audit still refused: %v", err)
	}
}
```
  - `cmd/baby-tracker-server/authority_runtime_test.go` adds `TestReadinessCheckCostsNoRoundTrip`:
    - build the readiness probes over `testkit.CountedPool(t, db.Runtime)`;
    - one `Refresh` warms the probes;
    - after `wire.Reset()`, 1,000 `Check` calls give `wire.Trips() == 0`;
    - one more fast `Refresh` gives exactly 1 round trip (`SELECT current_version …`).
  - `internal/database/pool_test.go` adds `TestPoolsSkipShortIdlePingsAndCacheStatements`:
    - `OpenWithMax` and `OpenBackground` configs have `ShouldPing(…{IdleDuration: 59s}) == false` and `(…61s) == true`;
    - `ConnConfig.DefaultQueryExecMode == pgx.QueryExecModeCacheStatement`;
    - a DSN carrying `default_query_exec_mode=simple_protocol` is `ErrOpen`;
    - `OpenBackground(...).Config().MaxConns == 2`.
  - `internal/config/config_test.go` adds `TestMaintenanceCredentialIsRequired`: neither `BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE` nor `BT_EXPORT_DATABASE_URL_FILE` refuses with a message naming both.
- [ ] **Step 2: Run to see the failure:** `rtk go -C Server test ./internal/readiness ./internal/config -run 'TestCheck|TestAudit|TestStale|TestMaintenanceCredential'` and `rtk proxy env BT_TEST_PACKAGES='./internal/database ./cmd/baby-tracker-server' BT_TEST_RUN='TestPools|TestReadiness' bash Server/scripts/test-db.sh`. Expected: `package babytracker/server/internal/readiness is not in std`, and `undefined: database.OpenBackground`.
- [ ] **Step 3: Implement.**
  - **Monitor.**
    - `Refresh` runs `Fast`. When no audit has succeeded, when the version differs from the last audited one, when the last audit failed, or when `AuditInterval` has passed, it runs `Audit`.
    - It stores `{err, version, at}` atomically and logs once per change of outcome (as `databaseReadiness.report` does today).
    - `Check` returns the stored error, or `ErrStale` when `Now() − at > Stale`.
  - **Pools.** `openWithMax` and `Open` set:
```go
cfg.ShouldPing = func(_ context.Context, p pgxpool.ShouldPingParams) bool { return p.IdleDuration > idlePingAfter }
// pgx parses default_query_exec_mode out of the DSN into the config; its
// default is the per-connection statement cache, so any other value was supplied.
if cfg.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeCacheStatement {
	return nil, ErrOpen
}
```
  - **Probes** (`authority_runtime.go`):
    - `fast`: `SELECT current_version FROM schema_compatibility WHERE singleton` on the background pool, accepted iff `compatibility` knows it.
    - `audit`: today's `check` (`authority_runtime.go:93-127`) on the background pool, plus `database.VerifyMaintenanceRole(ctx, background, maintenance, version)`. On success it caches `(instanceID, epoch)`.
    - `ServerState(ctx)` returns the cached pair after `monitor.Check(ctx) == nil`.
    - `httpapi.Options.Readiness`, `Gate` and `ServerState` all point at it.
  - **Composition** (`main.go`):
    - open `background, err := database.OpenBackground(ctx, cfg.DatabaseURL)`;
    - open `maintenancePool, err := openApplicationPool(ctx, cfg.RecoveryCleanup.WorkerDatabaseURL, cfg.DatabasePools.Maintenance, cfg.DatabasePools.Explicit())`, then `verifyMaintenancePool(ctx, maintenancePool, <login>)`;
    - `exportPool = maintenancePool` when `cfg.Export.WorkerDatabaseURL == cfg.RecoveryCleanup.WorkerDatabaseURL`;
    - `budgets := serverPoolBudgets(pool, mcpPool, background, maintenancePool, exportPool)`; `actualPoolBudgets` names the pools in composition order and deduplicates shared ones;
    - `backgroundAuthority, _ := authority.New(authorityOptions(cfg, background))`;
    - `verifier := authority.NewAuditVerifier(background, time.Now, logger)`;
    - the key inventory reads `backgroundAuthority.ReferencedKeyIDs`;
    - `monitor := readiness.New(...)`, `go monitor.Run(ctx)`, with one `monitor.Refresh` inside `keyInventoryStartupBudget` before serving.
  - **Config.** `config.Load` returns `errors.New("BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE (or BT_EXPORT_DATABASE_URL_FILE) is required: retention, the family purge and wakes run as the maintenance principal")` when `!recoveryCleanup.Enabled`.
- [ ] **Step 4: Run green:** the Step 2 commands; `rtk proxy env BT_TEST_PACKAGES='./cmd/... ./internal/config ./internal/httpapi' bash Server/scripts/test-db.sh`; `rtk make server-test`.
- [ ] **Step 5: Commit:** `rtk git add -A Server/internal/readiness Server/internal/database Server/internal/config Server/cmd && rtk git commit -m "feat(readiness): cache node readiness off the request path on a background pool"`

### Task 4: The one-round-trip request seam

**Files:**
- Create `Server/internal/authority/request.go` and `request_test.go`.
- Modify `Server/internal/authority/guard.go:317-359`: `WithAuthenticatedFamilyTx`/`ReadTx` delegate to `WithRequestTx`/`WithRequestReadTx` with `Request{Digests: digests}`, and `withAuthenticatedFamily` is removed.
- Modify `Server/internal/auth/refresh.go`: add `AccessDigests`.
- Create `Server/internal/auth/access_digests_test.go`.

**Interfaces:** Consumes `beginRequest`, `requestAuthoritySQL`, `scanRequest`, `checkAuthenticated`, `familyGrant`, `refuse` and `finish` (`authority/guard.go`, `authenticate.go`). Produces:
```go
package authority
// Request is one HTTP request's credential, with the caller's checks at
// their legacy positions around family policy.
type Request struct {
	Digests []KeyedDigest
	// Admit runs after authentication and before family policy: the caller's
	// refusals that rank after a bad credential and before a family refusal
	// (scope, then a held header or body refusal).
	Admit func(access.Principal) error
	// Charge runs after family policy and before run: the in-memory rate
	// limit. A refusal rolls back with nothing else done.
	Charge func(access.Principal, access.Grant) error
}
func (s *Service) WithRequestTx(ctx context.Context, req Request, action string, run func(pgx.Tx, access.Principal, access.Grant) error) error
func (s *Service) WithRequestReadTx(ctx context.Context, req Request, action string, run func(pgx.Tx, access.Principal, access.Grant) error) error
// AuthorizeRequest is the same order as one pooled statement with no
// transaction (the live handshake, which holds nothing open).
func (s *Service) AuthorizeRequest(ctx context.Context, req Request, action string) (access.Principal, access.Grant, error)
type RequestGuard interface {
	FamilyGuard
	WithRequestTx(ctx context.Context, req Request, action string, run func(pgx.Tx, access.Principal, access.Grant) error) error
	WithRequestReadTx(ctx context.Context, req Request, action string, run func(pgx.Tx, access.Principal, access.Grant) error) error
}
var _ RequestGuard = (*Service)(nil)

package auth
// AccessDigests is a bearer access token's digest under every configured
// token key, current first, with no database work; a malformed token is
// session_revoked, as Authenticate refuses it.
func (service *Service) AccessDigests(rawToken string) ([]authority.KeyedDigest, error)
```

- [ ] **Step 1: Write the failing tests** `Server/internal/authority/request_test.go`. It reuses `guard_test.go`'s `newMember`, `createFamily`, `joined`, `seated`, `countedService` and `wantCode`.
```go
func TestRequestRefusalOrder(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	viewer := seated(t, f, joined(t, f, f.Service, owner, family, "viewer"), family, "viewer")
	var admitted, charged, ran int
	req := func(digests []authority.KeyedDigest, admit, charge error) authority.Request {
		return authority.Request{Digests: digests,
			Admit:  func(access.Principal) error { admitted++; return admit },
			Charge: func(access.Principal, access.Grant) error { charged++; return charge }}
	}
	run := func(pgx.Tx, access.Principal, access.Grant) error { ran++; return nil }
	unknown := accessDigests(f.TokenKeys, base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	wantCode(t, f.Service.WithRequestTx(t.Context(), req(unknown, nil, nil), "read", run), "session_revoked")
	if admitted+charged+ran != 0 {
		t.Fatalf("an unknown credential reached the caller: %d %d %d", admitted, charged, ran)
	}
	held := errors.New("invalid_request")
	if err := f.Service.WithRequestTx(t.Context(), req(viewer.digests(f), held, nil), "mutate_record", run); !errors.Is(err, held) {
		t.Fatalf("Admit's refusal must win over family policy (read_only_caregiver): %v", err)
	}
	wantCode(t, f.Service.WithRequestTx(t.Context(), req(viewer.digests(f), nil, nil), "mutate_record", run), "read_only_caregiver")
	if charged != 0 || ran != 0 {
		t.Fatalf("a policy refusal charged %d, ran %d", charged, ran)
	}
	limited := errors.New("rate_limited")
	if err := f.Service.WithRequestTx(t.Context(), req(viewer.digests(f), nil, limited), "read", run); !errors.Is(err, limited) || ran != 0 {
		t.Fatalf("Charge's refusal: %v, ran %d", err, ran)
	}
	var grant access.Grant
	if err := f.Service.WithRequestTx(t.Context(), req(viewer.digests(f), nil, nil), "read",
		func(_ pgx.Tx, p access.Principal, g access.Grant) error { grant = g; return nil }); err != nil || grant.Role != "viewer" || grant.FamilyID != family {
		t.Fatalf("grant %+v %v", grant, err)
	}
}

func TestRequestRoundTrips(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	st := seated(t, f, owner, family, "owner")
	s, w := countedService(t, f)
	ok := authority.Request{Digests: st.digests(f)}
	refused := authority.Request{Digests: st.digests(f), Charge: func(access.Principal, access.Grant) error { return errors.New("rate_limited") }}
	for _, read := range []bool{false, true} {
		guarded := s.WithRequestTx
		if read {
			guarded = s.WithRequestReadTx
		}
		_ = guarded(t.Context(), ok, "read", func(pgx.Tx, access.Principal, access.Grant) error { return nil }) // prepares
		w.reset()
		var before int
		if err := guarded(t.Context(), ok, "read", func(pgx.Tx, access.Principal, access.Grant) error { before, _, _ = w.snapshot(); return nil }); err != nil {
			t.Fatal(err)
		}
		if total, _, _ := w.snapshot(); before != 1 || total != 2 {
			t.Fatalf("read=%v: %d round trips before run, %d with commit; want 1 and 2", read, before, total)
		}
		w.reset()
		_ = guarded(t.Context(), refused, "read", func(pgx.Tx, access.Principal, access.Grant) error { return nil })
		if total, _, _ := w.snapshot(); total != 2 {
			t.Fatalf("read=%v: a charged refusal took %d round trips, want 2 (request + rollback)", read, total)
		}
	}
}

func TestAuthorizeRequestIsOneStatementWithoutATransaction(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	st := seated(t, f, owner, family, "owner")
	s, w := countedService(t, f)
	_, _, _ = s.AuthorizeRequest(t.Context(), authority.Request{Digests: st.digests(f)}, "read") // prepares
	w.reset()
	p, g, err := s.AuthorizeRequest(t.Context(), authority.Request{Digests: st.digests(f)}, "read")
	trips, statements, _ := w.snapshot()
	if err != nil || g.FamilyID != family || p.AccountID != owner.account || trips != 1 || len(statements) != 1 || statementsMatching(statements, "BEGIN") != 0 {
		t.Fatalf("authorize: %v, %d round trips, statements %q", err, trips, statements)
	}
}
```
  `Server/internal/auth/access_digests_test.go`, `TestAccessDigestsAuthenticateLikeAuthenticate`:
  - for a harness owner, `h.Authority.Authenticate(ctx, mustDigests(h.Auth.AccessDigests(h.AccessToken("owner"))))` equals `h.Auth.Authenticate(ctx, h.AccessToken("owner"))`;
  - `AccessDigests("not-a-token")` is `auth.ErrSessionRevoked`;
  - with two configured token keys, `len(digests) == 2`, current first.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env BT_TEST_PACKAGES='./internal/authority ./internal/auth' BT_TEST_RUN='TestRequest|TestAuthorizeRequest|TestAccessDigests' bash Server/scripts/test-db.sh`. Expected: `undefined: authority.Request` and `h.Auth.AccessDigests undefined`.
- [ ] **Step 3: Implement** `request.go`:
```go
func (s *Service) WithRequestTx(ctx context.Context, req Request, action string, run func(pgx.Tx, access.Principal, access.Grant) error) error {
	return s.withRequest(ctx, req, action, requestBegin, run)
}

func (s *Service) WithRequestReadTx(ctx context.Context, req Request, action string, run func(pgx.Tx, access.Principal, access.Grant) error) error {
	return s.withRequest(ctx, req, action, requestBeginRead, run)
}

func (s *Service) withRequest(ctx context.Context, req Request, action, begin string, run func(pgx.Tx, access.Principal, access.Grant) error) error {
	if run == nil {
		return ErrUnavailable
	}
	args, ok := digestArgs(req.Digests)
	if !ok {
		return ErrSessionRevoked
	}
	var row requestRow
	tx, err := beginRequest(ctx, s.pool, begin, func(r pgx.Row) (err error) { row, err = scanRequest(r); return err },
		requestAuthoritySQL, args, nil, true, int64(s.epoch))
	if err != nil {
		return err
	}
	principal, grant, err := s.admitRequest(row, req, action, s.at(time.Time{}))
	if err != nil {
		return refuse(ctx, tx, err)
	}
	return finish(ctx, tx, func() error { return run(tx, principal, grant) })
}

func (s *Service) AuthorizeRequest(ctx context.Context, req Request, action string) (access.Principal, access.Grant, error) {
	args, ok := digestArgs(req.Digests)
	if !ok {
		return access.Principal{}, access.Grant{}, ErrSessionRevoked
	}
	row, err := scanRequest(s.pool.QueryRow(ctx, requestAuthoritySQL, args, nil, true, int64(s.epoch)))
	if err != nil {
		return access.Principal{}, access.Grant{}, unavailable("authorize request", err)
	}
	return s.admitRequest(row, req, action, s.at(time.Time{}))
}

// admitRequest is every request's refusal order: authentication, the
// caller's Admit, family policy, the caller's Charge.
func (s *Service) admitRequest(row requestRow, req Request, action string, now time.Time) (access.Principal, access.Grant, error) {
	if err := s.checkAuthenticated(row, req.Digests, now); err != nil {
		return access.Principal{}, access.Grant{}, err
	}
	principal := row.principal()
	if req.Admit != nil {
		if err := req.Admit(principal); err != nil {
			return access.Principal{}, access.Grant{}, err
		}
	}
	grant, err := s.familyGrant(row, action, now)
	if err != nil {
		return access.Principal{}, access.Grant{}, err
	}
	if req.Charge != nil {
		if err := req.Charge(principal, grant); err != nil {
			return access.Principal{}, access.Grant{}, err
		}
	}
	return principal, grant, nil
}
```
  `auth.AccessDigests`: return `ErrSessionRevoked` unless `service != nil && validBearerSecret(rawToken)`, otherwise `service.tokenCandidates("access", rawToken), nil`.
- [ ] **Step 4: Run green:** `rtk proxy env BT_TEST_PACKAGES='./internal/authority ./internal/auth' bash Server/scripts/test-db.sh`. The existing `TestRequestTransactionRoundTrips` and `TestRevocationAppliesToNextTransaction` pass unchanged through the delegating `WithAuthenticatedFamilyTx`.
- [ ] **Step 5: Commit:** `rtk git add Server/internal/authority Server/internal/auth && rtk git commit -m "feat(authority): one-round-trip request seam with admission and charge hooks"`

### Task 5: Sync and snapshot requests in one transaction

**Files:**
- Create `Server/internal/sync/serve.go`, `serve_test.go` and `request_concurrency_test.go`.
- Modify `Server/internal/sync/exchange.go`: `Options.Guard` becomes `authority.RequestGuard`, and `Options.Resolver` is removed.
- Modify `Server/internal/sync/paging.go`:
  - delete `resolveFamily` (:155-175);
  - `ApplyAndPull` checks the principal's session family itself and lets the guard's grant decide.
- Modify `Server/internal/sync/snapshot.go:19-36`: `FullSnapshot` does the same.
- Modify `Server/internal/sync/mutation.go`: the `guard` and `resolver` fields.
- Modify `Server/internal/sync/paging_test.go`:
  - `serviceResolving` becomes `serviceGuarded(h, guard authority.RequestGuard)`;
  - `TestContinuationFamilyResolutionWinsBeforeUntrustedTokenLookup` keeps its property with the fault moved into the guard (`scope_failed` returned by a wrapper's `WithFamilyTx`, so the forged token is never decoded);
  - test guard wrappers embed `authority.RequestGuard`.
- Modify `Server/internal/httpapi/sync.go:25-192,348-368`. Create `Server/internal/httpapi/request_roundtrip_test.go`.
- Modify `Server/internal/httpapi/router.go`: the `SessionService` and `SyncService` interfaces.
- Modify the tests that use the changed interfaces:
  - `httpapi/{sync_test.go,care_errors_test.go,admission_test.go,router_test.go}`: the stubs implement `ServeSync`/`ServeSnapshot`;
  - `sessionServiceStub` implements `AccessDigests`, and a helper `stubAuthorize(req, p, g)` calls `req.Admit` and `req.Charge` the way the authority would.
- Modify `Server/internal/testkit/fixtures.go`: `guard authority.RequestGuard`, `SyncServiceOn(t, h, guard authority.RequestGuard)`, no `Resolver`.
- Modify `Server/internal/capacity/fixture_identitytest.go`, `cmd/baby-tracker-server/{main.go,performance_composition_identitytest_test.go}` and `tests/protocol/harness_test.go`: the sync composition, without a resolver.

**Interfaces:** Consumes Task 4's `authority.Request`/`RequestGuard`/`auth.AccessDigests`, Task 1's limiter, and the existing `Exchange`, `SnapshotPage` and `validateSync`. Produces:
```go
package sync
type Options struct {
	Guard authority.RequestGuard; Server protocol.ServerInfo; Limits protocol.Limits; Now func() time.Time
	Publisher care.Publisher; PageKeys identity.DigestKeyring; Allowed func(role, action string) bool
}
// ServeSync is one sync request: one guarded transaction around Exchange,
// then the post-commit publish, then Fatal or the response.
func (s *Service) ServeSync(ctx context.Context, req authority.Request, request protocol.SyncRequest) (protocol.SyncResponse, error)
// ServeSnapshot is one snapshot page in one guarded transaction.
func (s *Service) ServeSnapshot(ctx context.Context, req authority.Request, request protocol.SnapshotRequest) (protocol.SnapshotResponse, error)

package httpapi
type SessionService interface { /* existing methods */ AccessDigests(string) ([]authority.KeyedDigest, error) }
type SyncService interface {
	ServeSync(context.Context, authority.Request, protocol.SyncRequest) (protocol.SyncResponse, error)
	ServeSnapshot(context.Context, authority.Request, protocol.SnapshotRequest) (protocol.SnapshotResponse, error)
}
// unexported:
type syncRefusal string              // a sync code held until after authentication; written with writeSyncCode
func (r syncRefusal) Error() string
type rateLimited struct{ decision limits.Decision } // written with writeSyncRateLimited
func (r rateLimited) Error() string
func writeRequestError(w http.ResponseWriter, r *http.Request, options Options, err error) // syncRefusal, rateLimited, else writeSyncError
// syncCharges is today's charge list (sync.go:99-108) as a function: the pull
// device and account charges, plus, for mutations, the device, account and family mutation charges.
func syncCharges(options Options, p access.Principal, familyID string, mutations int) []limits.Charge
```

- [ ] **Step 1: Write the failing tests.**
  - `Server/internal/httpapi/request_roundtrip_test.go`:
```go
type stackReady struct{}

func (stackReady) Check(context.Context) error { return nil }

type stack struct {
	h       *testkit.Harness
	wire    *testkit.Wire
	handler http.Handler
}

func newStack(t *testing.T, h *testkit.Harness, pullPolicy limits.Policy) *stack {
	t.Helper()
	pool, wire := testkit.CountedPool(t, h.Runtime)
	service := testkit.SyncServiceOn(t, h, h.AuthorityOn(t, pool))
	limiter, err := limits.New(limits.Options{Pepper: [32]byte{9}, Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	policies := limits.DefaultPolicies()
	return &stack{h: h, wire: wire, handler: New(Options{Readiness: stackReady{}, InstanceID: h.Info.InstanceID, RecoveryEpoch: h.Info.RecoveryEpoch,
		CanonicalOrigin: h.Info.CanonicalOrigin, SyncLimits: &h.Limits, SessionService: h.Auth, SyncService: service, Limiter: limiter,
		SyncMutationDevicePolicy: policies[limits.ScopeSyncMutationDevice], SyncMutationAccountPolicy: policies[limits.ScopeSyncMutationAccount],
		SyncMutationFamilyPolicy: policies[limits.ScopeSyncMutationFamily], SyncPullDevicePolicy: pullPolicy,
		SyncPullAccountPolicy: policies[limits.ScopeSyncPullAccount]})}
}

func (s *stack) send(t *testing.T, subject, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Authorization", "Bearer "+s.h.AccessToken(subject))
	req.Header.Set("X-Baby-Tracker-Protocol", "v1")
	capabilities := "records-v1"
	if strings.HasPrefix(target, "/v1/sync/snapshot") {
		capabilities = "records-v1,snapshot-v1"
	}
	req.Header.Set("X-Baby-Tracker-Capabilities", capabilities)
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

// trips is the round trips of one request, after warm has prepared its statements.
func (s *stack) trips(t *testing.T, label string, warm, measured func() *httptest.ResponseRecorder, status int) int {
	t.Helper()
	if warm != nil {
		warm()
	}
	s.wire.Reset()
	if rec := measured(); rec.Code != status {
		t.Fatalf("%s: status %d, want %d: %s", label, rec.Code, status, rec.Body.String())
	}
	return s.wire.Trips()
}

func TestSyncHTTPRoundTrips(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	s := newStack(t, h, limits.DefaultPolicies()[limits.ScopeSyncPullDevice])
	pull := func() *httptest.ResponseRecorder {
		return s.send(t, "owner", http.MethodPost, "/v1/sync", h.SyncRequest(h.Cursor(h.FamilySequence(t)), nil))
	}
	if got := s.trips(t, "pull-only", pull, pull, http.StatusOK); got != 3 {
		t.Fatalf("pull-only: %d round trips, want 3: %q", got, s.wire.Statements())
	}
	for i, size := range []int{1, 20, 100} {
		batch := func(n int) func() *httptest.ResponseRecorder {
			return func() *httptest.ResponseRecorder {
				baby := fmt.Sprintf("10000000-0000-4000-8000-%012d", 900+10*i+n)
				caretest.Seed(t, h, caretest.Baby(baby))
				request := h.SyncRequest(h.Cursor(h.FamilySequence(t)), nil)
				request.Mutations = caretest.MixedBatch(baby, size)
				return s.send(t, "owner", http.MethodPost, "/v1/sync", request)
			}
		}
		if got := s.trips(t, fmt.Sprintf("batch of %d", size), batch(0), batch(1), http.StatusOK); got != 4 {
			t.Fatalf("batch of %d with a one-page pull: %d round trips, want 4: %q", size, got, s.wire.Statements())
		}
	}
	bad := func() *httptest.ResponseRecorder {
		request := h.SyncRequest(nil, nil)
		request.Mutations = nil // invalid_request, held until the session is known good
		return s.send(t, "owner", http.MethodPost, "/v1/sync", request)
	}
	if got := s.trips(t, "held 400", bad, bad, http.StatusBadRequest); got != 2 {
		t.Fatalf("a held refusal: %d round trips, want 2", got)
	}
	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID); err != nil {
		t.Fatal(err)
	}
	revoked := func() *httptest.ResponseRecorder { return s.send(t, "caregiver", http.MethodPost, "/v1/sync", h.SyncRequest(nil, nil)) }
	if got := s.trips(t, "revoked", revoked, revoked, http.StatusUnauthorized); got != 2 {
		t.Fatalf("a revoked session: %d round trips, want 2", got)
	}
	limited := newStack(t, h, limits.Policy{Count: 1, Window: time.Minute})
	pullLimited := func() *httptest.ResponseRecorder {
		return limited.send(t, "owner", http.MethodPost, "/v1/sync", h.SyncRequest(h.Cursor(h.FamilySequence(t)), nil))
	}
	if got := limited.trips(t, "rate limited", pullLimited, pullLimited, http.StatusTooManyRequests); got != 2 {
		t.Fatalf("a rate-limited pull: %d round trips, want 2", got)
	}
}

// 13 rows (a baby and 12 feeds) at page size 3 page as 3, 3, 3, 3, 1: a
// first page and three continuations with more, then a last continuation.
func TestPagedHTTPRoundTrips(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 3
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	for n := range 12 {
		caretest.Seed(t, h, caretest.Feed(caretest.BabyA, n))
	}
	s := newStack(t, h, limits.DefaultPolicies()[limits.ScopeSyncPullDevice])
	var token *string
	first := func() *httptest.ResponseRecorder {
		rec := s.send(t, "owner", http.MethodPost, "/v1/sync", h.SyncRequest(h.Cursor(0), nil))
		token = decodedToken(t, rec)
		return rec
	}
	next := func() *httptest.ResponseRecorder {
		rec := s.send(t, "owner", http.MethodPost, "/v1/sync", h.SyncRequest(nil, token))
		token = decodedToken(t, rec)
		return rec
	}
	first() // prepares the first-page statements
	if got := s.trips(t, "first page with more", nil, first, http.StatusOK); got != 4 || token == nil {
		t.Fatalf("first pull page with a token: %d (token %v), want 4", got, token) // page 1
	}
	next() // page 2: prepares the continuation statements
	if got := s.trips(t, "continuation with more", nil, next, http.StatusOK); got != 4 || token == nil {
		t.Fatalf("continuation with more: %d, want 4", got) // page 3
	}
	next() // page 4
	if got := s.trips(t, "last continuation", nil, next, http.StatusOK); got != 3 || token != nil {
		t.Fatalf("last continuation: %d round trips (token %v), want 3", got, token) // page 5
	}
	var snapshot *string
	snap := func() *httptest.ResponseRecorder {
		target := "/v1/sync/snapshot"
		if snapshot != nil {
			target += "?page_token=" + url.QueryEscape(*snapshot)
		}
		rec := s.send(t, "owner", http.MethodGet, target, nil)
		snapshot = decodedToken(t, rec)
		return rec
	}
	start := func() *httptest.ResponseRecorder { snapshot = nil; return snap() }
	start() // snapshot 1 (of the account's 2): prepares the capture statements
	if got := s.trips(t, "snapshot first page", nil, start, http.StatusOK); got != 4 || snapshot == nil {
		t.Fatalf("first snapshot page: %d, want 4", got) // snapshot 2, page 1
	}
	snap() // page 2: prepares the continuation statements
	if got := s.trips(t, "snapshot continuation with more", nil, snap, http.StatusOK); got != 4 || snapshot == nil {
		t.Fatalf("snapshot continuation with more: %d, want 4", got) // page 3
	}
	snap() // page 4
	if got := s.trips(t, "last snapshot continuation", nil, snap, http.StatusOK); got != 3 || snapshot != nil {
		t.Fatalf("last snapshot continuation: %d (token %v), want 3", got, snapshot) // page 5
	}
}

// decodedToken is the response's next_page_token, nil on a final page.
func decodedToken(t *testing.T, rec *httptest.ResponseRecorder) *string {
	t.Helper()
	var page struct {
		NextPageToken *string `json:"next_page_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("%s: %v", rec.Body.String(), err)
	}
	return page.NextPageToken
}
```
    The snapshot uses the `ActiveSnapshotAccountLimit`/`FamilyLimit` defaults (2 per account and 8 per family). The test starts exactly two snapshots, which stays inside the account cap.
  - `TestSyncErrorPrecedenceIsUnchanged` in `request_roundtrip_test.go` is a characterization test over `newStack`. Write it first and run it green against today's handler, before any Step 3 change, so it pins the legacy order; it must stay green after. It is a table of `(request, want status, want code)`:
    - no `Authorization` header and a malformed body: 401 `access_expired`;
    - a malformed bearer token: 401 `session_revoked`;
    - a removed caregiver's token and a malformed body: 401 `session_revoked`, never 400;
    - a valid session with `X-Baby-Tracker-Protocol: v2`: 400 `unsupported_protocol`;
    - a body over `SyncBodyBytes`: 413 `body_too_large`;
    - `SyncBatchSize+1` mutations: 400 `batch_limit`;
    - a cursor of another instance: 409 `snapshot_required`;
    - a viewer's mutation: 403 `read_only_caregiver`;
    - an unrefreshed account-scope session (`testkit.NewWithUnrefreshedCaregiver`): 401 `access_expired`.

    After Step 3, the same test also asserts `s.wire.Trips() <= 2` for every row; the first two rows send 0, because they are refused before any database work.
  - `Server/internal/sync/request_concurrency_test.go`:
```go
func TestOneTransactionRequestsNeverDeadlockWithCommands(t *testing.T) { // Review Focus 4
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	viewer := h.AddMember(t, "viewer", "viewer")
	refresher := h.AddMember(t, "refresher", "caregiver")
	_ = refresher
	digests := func(subject string) []authority.KeyedDigest {
		d, err := h.Auth.AccessDigests(h.AccessToken(subject))
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	owner, caregiver := digests("owner"), digests("caregiver") // read before any goroutine; the bundles map is not shared
	refreshToken := h.RefreshToken("refresher")
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	var deadlocks, lockTimeouts, unexpected atomic.Int32
	classify := func(err error) {
		var pgErr *pgconn.PgError
		switch {
		case err == nil, errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		case errors.As(err, &pgErr) && pgErr.Code == "40P01":
			deadlocks.Add(1)
		case errors.As(err, &pgErr) && pgErr.Code == "55P03":
			lockTimeouts.Add(1)
		default:
			t.Logf("unexpected: %v", err)
			unexpected.Add(1)
		}
	}
	var wg sync.WaitGroup
	for writer := range 4 {
		wg.Go(func() {
			for n := 0; ctx.Err() == nil; n++ {
				request := h.SyncRequest(nil, nil)
				request.Mutations = []protocol.Mutation{caretest.Feed(caretest.BabyA, 100000*(writer+1)+n)}
				_, err := h.Service.ServeSync(ctx, authority.Request{Digests: owner}, request)
				classify(err)
			}
		})
	}
	wg.Go(func() {
		for ctx.Err() == nil {
			_, err := h.Service.ServeSync(ctx, authority.Request{Digests: caregiver}, h.SyncRequest(nil, nil))
			classify(err)
		}
	})
	wg.Go(func() {
		for ctx.Err() == nil {
			_, err := h.Service.ServeSnapshot(ctx, authority.Request{Digests: caregiver}, protocol.SnapshotRequest{})
			if !errors.Is(err, familysync.ErrSnapshotLimit) {
				classify(err)
			}
		}
	})
	wg.Go(func() {
		for role := "caregiver"; ctx.Err() == nil; {
			_, err := h.Family.ChangeRole(ctx, h.Owner, h.CommandID(), viewer.AccountID, role)
			classify(err)
			role = map[string]string{"caregiver": "viewer", "viewer": "caregiver"}[role]
		}
	})
	wg.Go(func() {
		for ctx.Err() == nil {
			bundle, err := h.Auth.Refresh(ctx, refreshToken)
			classify(err)
			if err == nil {
				refreshToken = bundle.RefreshToken
			}
		}
	})
	wg.Wait()
	if deadlocks.Load()+lockTimeouts.Load()+unexpected.Load() != 0 {
		t.Fatalf("deadlocks %d, lock timeouts %d, unexpected %d", deadlocks.Load(), lockTimeouts.Load(), unexpected.Load())
	}
}

// pausingGuard holds a request inside its transaction after authorization.
type pausingGuard struct {
	authority.RequestGuard
	entered, release chan struct{}
}

func (g pausingGuard) WithRequestTx(ctx context.Context, req authority.Request, action string, run func(pgx.Tx, access.Principal, access.Grant) error) error {
	return g.RequestGuard.WithRequestTx(ctx, req, action, func(tx pgx.Tx, p access.Principal, gr access.Grant) error {
		close(g.entered)
		<-g.release
		return run(tx, p, gr)
	})
}

func TestRevocationRefusesTheNextRequestAndLetsInFlightFinish(t *testing.T) { // Review Focus 4
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA), caretest.Feed(caretest.BabyA, 1))
	caregiver, err := h.Auth.AccessDigests(h.AccessToken("caregiver"))
	if err != nil {
		t.Fatal(err)
	}
	guard := pausingGuard{RequestGuard: h.Authority, entered: make(chan struct{}), release: make(chan struct{})}
	service := testkit.SyncServiceOn(t, h, guard)
	done := make(chan error, 1)
	var inFlight protocol.SyncResponse
	go func() {
		var err error
		inFlight, err = service.ServeSync(context.Background(), authority.Request{Digests: caregiver}, h.SyncRequest(h.Cursor(0), nil))
		done <- err
	}()
	<-guard.entered
	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID); err != nil {
		t.Fatal(err) // a pull holds no family lock, so removal does not wait for it
	}
	close(guard.release)
	if err := <-done; err != nil || len(testkit.Decoded(t, inFlight).Changes) != 2 {
		t.Fatalf("the in-flight pull did not complete: %v", err)
	}
	if _, err := h.Service.ServeSync(t.Context(), authority.Request{Digests: caregiver}, h.SyncRequest(h.Cursor(0), nil)); family.Code(err) != "session_revoked" {
		t.Fatalf("a request after the removal committed: %v", err)
	}
}
```
- [ ] **Step 2: Run to see the failure:** `rtk proxy env BT_TEST_PACKAGES='./internal/httpapi ./internal/sync' BT_TEST_RUN='TestSyncHTTPRoundTrips|TestPagedHTTPRoundTrips|TestSyncErrorPrecedence|TestOneTransactionRequests|TestRevocationRefuses' bash Server/scripts/test-db.sh`. Expected: `undefined: ServeSync` and `h.Auth does not implement SessionService (missing method AccessDigests)` before Task 4's method is wired into the interface.
- [ ] **Step 3: Implement.**
  - `serve.go`:
```go
func (s *Service) ServeSync(ctx context.Context, req authority.Request, request protocol.SyncRequest) (protocol.SyncResponse, error) {
	if !s.ready() {
		return protocol.SyncResponse{}, errUnavailable
	}
	txCtx, cancel := context.WithTimeout(ctx, time.Duration(s.limits.SyncTransactionSeconds)*time.Second)
	defer cancel()
	var exchanged Exchanged
	var familyID string
	start := time.Now()
	err := s.guard.WithRequestTx(txCtx, req, "read", func(tx pgx.Tx, p access.Principal, g access.Grant) error {
		familyID = g.FamilyID
		var err error
		exchanged, err = s.Exchange(txCtx, tx, p, g, request) // seamReady and validateSync run first, before any statement
		return err
	})
	diagnostics.Record(ctx, nil, diagnostics.Database, time.Since(start))
	if err != nil {
		return protocol.SyncResponse{}, err
	}
	if exchanged.Publish != nil && s.publisher != nil {
		_ = s.publisher.Publish(ctx, familyID, *exchanged.Publish)
	}
	if exchanged.Fatal != nil {
		return protocol.SyncResponse{}, exchanged.Fatal
	}
	return exchanged.Response, nil
}

func (s *Service) ServeSnapshot(ctx context.Context, req authority.Request, request protocol.SnapshotRequest) (protocol.SnapshotResponse, error) {
	if !s.ready() {
		return protocol.SnapshotResponse{}, errUnavailable
	}
	txCtx, cancel := context.WithTimeout(ctx, time.Duration(s.limits.SyncTransactionSeconds)*time.Second)
	defer cancel()
	var response protocol.SnapshotResponse
	err := s.guard.WithRequestTx(txCtx, req, "read", func(tx pgx.Tx, p access.Principal, g access.Grant) error {
		var err error
		response, err = s.SnapshotPage(txCtx, tx, p, g, request)
		return err
	})
	return response, err
}
```
  - `ApplyAndPull` and `FullSnapshot` replace `resolveFamily` with two checks done in Go, before any statement:
    - `p.Scope != "family"` → `ErrFamilyScopeRequired`;
    - `p.SessionScope != "family" || !protocol.ValidUUID(p.SessionFamilyID)` → `ErrSessionRefreshRequired`.

    They then call `s.withFamily(ctx, p, p.SessionFamilyID, …)`, whose existing grant check (`mutation.go:86-97`) refuses any other family.
  - `syncHandler`, in order:
    1. readiness (`authReadyError`, now cached);
    2. the `Authorization` header and `auth.BearerToken`;
    3. `options.SessionService.AccessDigests(token)` (session_revoked);
    4. `parseSyncHeaders`, the 30 s context and the bounded body read with decode, capabilities, shape and `batch_limit` checks (`sync.go:48-87`), each refusal kept as `refusal := "<code>"` instead of written;
    5. then:
```go
response, err := options.SyncService.ServeSync(ctx, authority.Request{Digests: digests,
	Admit: func(p access.Principal) error {
		switch {
		case p.Scope != "family":
			return syncRefusal("family_scope_required")
		case p.SessionScope != "family" || !protocol.ValidUUID(p.SessionFamilyID):
			return syncRefusal("access_expired")
		case refusal != "":
			return syncRefusal(refusal)
		}
		return nil
	},
	Charge: func(p access.Principal, g access.Grant) error {
		decision, err := options.Limiter.TakeMany(ctx, syncCharges(options, p, g.FamilyID, len(input.Mutations))) // sync.go:99-108, family from the grant
		if err != nil {
			return syncRefusal("service_unavailable")
		}
		if !decision.Allowed {
			return rateLimited{decision}
		}
		return nil
	}}, input)
if err != nil {
	writeRequestError(w, r, options, err)
	return
}
writeJSON(w, http.StatusOK, response)
```
  - `snapshotHandler`: the same, with `validateSyncHeaders(SurfaceSnapshot)` and `snapshotPageToken` held as the refusal, and the two pull charges.
  - `authenticateSyncFamilyRequest` and both `FamilyService.Current` calls are deleted. `writeRequestError` writes a `syncRefusal` with `writeSyncCode`, a `rateLimited` with `writeSyncRateLimited`, and anything else with `writeSyncError`. `writeSyncError`'s default branch maps authority codes through `family.Code`, which ends in `authority.CodeOf`, so every authentication refusal keeps its legacy status.
- [ ] **Step 4: Gate:**
  - `rtk proxy env BT_TEST_PACKAGES='./internal/sync ./internal/httpapi ./internal/testkit ./internal/capacity ./internal/importer' bash Server/scripts/test-db.sh`;
  - `rtk proxy env BT_TEST_PACKAGES='./tests/protocol/... ./cmd/...' BT_TEST_TAGS=identitytest bash Server/scripts/test-db.sh`;
  - `(cd Server && rtk go vet ./... && rtk go vet -tags identitytest ./...)`.

  Record the measured trips (`-v` output of the two round-trip tests) in the task report.
- [ ] **Step 5: Commit:** `rtk git add -A Server/internal/sync Server/internal/httpapi Server/internal/testkit Server/internal/capacity Server/cmd Server/tests && rtk git commit -m "feat(sync): serve each sync and snapshot request in one authenticated transaction"`

### Task 6: Import, export and live without the family read; `StatusPage` lock-free; export claims on the database clock

**Files:**
- Modify `Server/internal/importer/resolver.go`:
  - :25, :33 `NewService` drops the resolver;
  - :51-65 `family` becomes a check of the session family only;
  - :280-310 `StatusPage`.
- Modify `Server/internal/importer/conflict.go:298`.
- Modify `Server/internal/httpapi/import.go:93-101`: the handler's `Current` is removed, and the family charge uses `p.SessionFamilyID`.
- Modify `Server/internal/export/{service.go:30-42,133-138,model.go:28-37,59}`: the `Resolver` option is removed and `next_attempt_at` is set to `now()`.
- Modify `Server/internal/httpapi/live.go:14-70` and `router.go`: `LiveFamilyAuthorizer` becomes `LiveAuthorizer`.
- Delete `family.LiveAuthorizer` (`family/service.go:527-550`) and `Server/internal/access/family_resolver.go`.
- Modify the composition: `main.go` (`syncFamilyResolver` removed), `testkit/fixtures.go` (`resolver`, `ImporterOn`), `capacity/fixture_identitytest.go`, `tests/protocol/harness_test.go:205-215` and `cmd/baby-tracker-overlap-check/*_test.go`.
- Tests:
  - `Server/internal/importer/status_test.go` (new) and `Server/internal/export/clock_test.go` (new);
  - `Server/internal/httpapi/live_test.go`: the stub implements `AuthorizeRequest`; `FamilyService.Current` must not be called;
  - `Server/internal/httpapi/live_roundtrip_test.go` (new).

**Interfaces:** Consumes Task 4's `AuthorizeRequest`. Produces:
```go
package httpapi
type LiveAuthorizer interface {
	AuthorizeRequest(context.Context, authority.Request, string) (access.Principal, access.Grant, error)
}
// Options.LiveAuthorizer LiveAuthorizer replaces Options.LiveFamilyAuthorizer (main.go passes authorityService).

package importer
func NewService(guard authority.FamilyGuard, info protocol.ServerInfo, limits protocol.Limits, pageKeys identity.DigestKeyring, now func() time.Time) *Service
```
Round-trip targets:
- `GET /v1/live` handshake: 1 statement, 1 round trip, no transaction. Today it is about 13: Authenticate 1, `Current` 4, `Authorize` 2, and two limiter transactions of 3 each.
- An import or export request: `Authenticate` plus one guarded transaction. Each loses two `Current` read transactions; the import route also loses its limiter transaction.

- [ ] **Step 1: Write the failing tests.**
  - `importer/status_test.go`:
```go
func TestStatusPageTakesNoFamilyLockAndChecksAccessFirst(t *testing.T) {
	h := testkit.New(t)
	resolved := h.Import(t, h.Owner, "8a000000-0000-4000-8000-000000000001", "Mira", "2026-01-01")
	holder := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	defer holder.Rollback(t.Context())
	if _, err := care.LockFamily(t.Context(), holder, h.FamilyID); err != nil {
		t.Fatal(err) // a writer holds the family row
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := h.Importer.StatusPage(ctx, h.Owner, resolved.ImportID, nil); err != nil {
		t.Fatalf("a status read waited on the family lock: %v", err)
	}
	if _, err := h.Importer.StatusPage(ctx, h.Caregiver, resolved.ImportID, nil); err == nil || err.Error() != "import_access_denied" {
		t.Fatalf("another member's status: %v", err)
	}
	if _, err := h.Importer.StatusPage(ctx, h.Caregiver, "8a000000-0000-4000-8000-0000000000ff", nil); err == nil || err.Error() != "import_not_found" {
		t.Fatalf("an unknown import: %v", err)
	}
}
func TestStatusPageMarksExpiryUnderTheFamilyLock(t *testing.T) {
	// expires_at moved into the past via h.ExecFamily (as resolver_test.go:114 does) -> StatusPage returns import_expired
	// and import_sessions.expired_at is set (one UPDATE after care.LockFamily); a second StatusPage reads expired_at and writes nothing.
}
```
  - `export/clock_test.go`, `TestExportJobIsClaimableByTheDatabaseClock`:
    - the service clock runs one hour ahead of the database (`Now: func() time.Time { return h.DatabaseNow().Add(time.Hour) }`);
    - `Create` succeeds;
    - the worker's `claim` (via `RunOnce` on `h.Maintenance`) claims the job at once.

    Before the fix, `next_attempt_at` is an hour in the database's future and nothing is claimed.
  - `httpapi/live_roundtrip_test.go`, `TestLiveHandshakeIsOneStatement`:
    - an `httptest.NewServer` over `New(Options{… LiveAuthorizer: h.AuthorityOn(t, countedPool), LiveHub: hub, Limiter: in-memory …})`;
    - a WebSocket dial with the owner's token (as `live_test.go` dials);
    - after one warm dial, the counted wire shows exactly 1 round trip and 1 statement (`app.request_authority`) per handshake.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env BT_TEST_PACKAGES='./internal/importer ./internal/export ./internal/httpapi' BT_TEST_RUN='TestStatusPage|TestExportJobIsClaimable|TestLiveHandshake' bash Server/scripts/test-db.sh`. Expected: the status read times out on the lock, the export job is never claimed, and `unknown field LiveAuthorizer`.
- [ ] **Step 3: Implement.**
  - **`StatusPage`.** Read the session with no lock (`loadSession`); `import_not_found`; then the access check (`import_access_denied`). If the session has expired but `expired_at` is unset, run `care.LockFamily`, re-read it and mark it (`s.expired`), and return `import_expired`. Otherwise run `s.response`. No lock is taken on the read path.
  - **Session family.** `importer.family` and `export.family` become `sessionFamily(p)`. It refuses `family_scope_required` (or `export`'s `ErrOwnerRequired`) and `access_expired` exactly as today, without a database read. `withFamily`'s grant check stays.
  - **Export.** `export/service.go:136`: the insert's `next_attempt_at` column takes `now()` instead of `$8`.
  - **Live handler.** Readiness; the IP limit, now in memory; the bearer token; `AccessDigests`. `validateSyncHeaders(SurfaceLive)` is held as the refusal. Then:
```go
principal, grant, err := options.LiveAuthorizer.AuthorizeRequest(request.Context(), authority.Request{Digests: digests,
	Admit: func(p access.Principal) error {
		if p.Scope != "family" || p.SessionScope != "family" || !protocol.ValidUUID(p.SessionFamilyID) {
			return syncRefusal("access_expired")
		}
		if refusal != "" {
			return syncRefusal(refusal)
		}
		return nil
	},
	Charge: func(p access.Principal, g access.Grant) error {
		decision, err := options.Limiter.TakeMany(request.Context(), []limits.Charge{
			{Scope: limits.ScopeLiveAttemptDevice, Key: p.DeviceID, Cost: 1, Policy: options.LiveAttemptDevicePolicy},
			{Scope: limits.ScopeLiveAttemptAccount, Key: p.AccountID, Cost: 1, Policy: options.LiveAttemptAccountPolicy},
			{Scope: limits.ScopeLiveAttemptFamily, Key: g.FamilyID, Cost: 1, Policy: options.LiveAttemptFamilyPolicy},
		})
		if err != nil {
			return syncRefusal("service_unavailable")
		}
		if !decision.Allowed {
			return rateLimited{decision}
		}
		return nil
	}}, "read")
if err != nil {
	writeRequestError(w, request, options, err)
	return
}
```
    The rest of the handler (`Reserve`, the query, cookie, subprotocol and origin checks, `Accept`, `Bind`) is unchanged.
- [ ] **Step 4: Run green:** `rtk proxy env BT_TEST_PACKAGES='./internal/importer ./internal/export ./internal/httpapi ./internal/family ./internal/testkit ./internal/capacity ./cmd/...' bash Server/scripts/test-db.sh`; `rtk proxy env BT_TEST_PACKAGES='./tests/protocol/...' BT_TEST_TAGS=identitytest bash Server/scripts/test-db.sh`.
- [ ] **Step 5: Commit:** `rtk git add -A Server && rtk git commit -m "feat(httpapi): import, export and live without the family read; lock-free import status"`

### Task 7: Live hub — per-family coalescing, a bounded recheck off the delivery goroutine, no PostgreSQL listener

**Files:**
- Modify `Server/internal/realtime/hub.go`:
  - `Publish` coalesces per family (:331-374);
  - `InvalidationsDropped` signals the sweep goroutine (:421-437);
  - `runSweep` and `sweep` take a deadline (:461-512);
  - `Invalidate` also releases matching reservations (:392-414).
- Create `Server/internal/realtime/export_test.go` (`FlushNow`).
- Modify `Server/internal/realtime/hub_test.go`: `TestDroppedInvalidationsRecheckEveryLease` (:855-899) waits for the authorizer call instead of expecting it before `InvalidationsDropped` returns, and new tests are added.
- Delete `Server/internal/realtime/{postgres.go,postgres_test.go}`, `main.go:195-201,210-216`, `tests/protocol/harness_test.go:52,98-…` (the listener) and `cmd/baby-tracker-server/performance_composition_identitytest_test.go:133,208` (listener and `pg_notify`; that test drives the hub through `Observer.Publish`).
- Modify `Server/README.md:571-575` and `docs/protocol/v1.md:276-281`: remove the listener sentences.

**Interfaces:** Produces:
```go
package realtime
type HubOptions struct {
	Limits protocol.Limits; Authorizer LiveAuthorizer; Invalidations invalidationSource; Now func() time.Time
	CoalesceWindow time.Duration // 0 means 50ms: one delivery per family per window, carrying its newest cursor
	SweepTimeout   time.Duration // 0 means 5s: a recheck that has not answered by then closes every lease it covers
}
// export_test.go: func (hub *Hub) FlushNow() // delivers every pending family hint at once
```

- [ ] **Step 1: Write the failing tests** (`hub_test.go`, using its `boundLease`, `principal`, `grant`, `waitCloses`, `closeCount` and `batchAuthorizer` helpers):
```go
func TestPublishCoalescesPerFamily(t *testing.T) {
	hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits(), CoalesceWindow: time.Hour}) // only FlushNow delivers
	if err != nil {
		t.Fatal(err)
	}
	connection := boundLease(t, hub, principal(testDevice), grant())
	for seq := uint64(1); seq <= 100; seq++ {
		hub.Publish(t.Context(), Hint{FamilyID: testFamily, Cursor: protocol.Cursor{ServerInstanceID: testServer, RecoveryEpoch: 1, FamilyID: testFamily, Sequence: seq}})
	}
	hub.FlushNow()
	deadline := time.Now().Add(time.Second)
	for writes(connection) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if got := connection.snapshotWrites(); len(got) != 1 || got[0].Cursor.Sequence != 100 {
		t.Fatalf("hints %+v, want one carrying sequence 100", got)
	}
}

func TestPublishDeliversWithinTheWindow(t *testing.T) {
	hub, _ := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits()}) // 50ms
	connection := boundLease(t, hub, principal(testDevice), grant())
	start := time.Now()
	for seq := uint64(1); seq <= 10; seq++ {
		hub.Publish(t.Context(), Hint{FamilyID: testFamily, Cursor: protocol.Cursor{ServerInstanceID: testServer, RecoveryEpoch: 1, FamilyID: testFamily, Sequence: seq}})
	}
	for writes(connection) == 0 && time.Since(start) < time.Second {
		time.Sleep(time.Millisecond)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond || len(connection.snapshotWrites()) != 1 {
		t.Fatalf("delivered after %v: %+v", elapsed, connection.snapshotWrites())
	}
}

func TestDroppedInvalidationsRecheckOffTheDeliveryGoroutineWithADeadline(t *testing.T) {
	blocked := &batchAuthorizer{block: make(chan struct{})} // CheckMany waits for block or ctx
	hub, _ := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits(), Authorizer: blocked, SweepTimeout: 100 * time.Millisecond})
	first := boundLease(t, hub, principal(testDevice), grant())
	start := time.Now()
	hub.InvalidationsDropped(t.Context())
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("InvalidationsDropped blocked the delivery goroutine for %v", elapsed)
	}
	waitCloses(t, first, ClosePolicyViolation) // the recheck timed out: fail closed
}

func TestInvalidationReachesAReservationBeforeBind(t *testing.T) {
	hub, _ := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits()})
	reservation, err := hub.Reserve(t.Context(), principal(testDevice), grant())
	if err != nil {
		t.Fatal(err)
	}
	hub.Invalidate(t.Context(), authority.SecurityInvalidation{SessionID: testSession, AccountID: testAccount, Revision: 99})
	if _, err := reservation.Bind(&testConnection{}); !errors.Is(err, ErrReservationReleased) {
		t.Fatalf("a revoked session's reservation bound: %v", err)
	}
}

// LISTEN/NOTIFY is removed: the hub's package reaches no database.
func TestRealtimeImportsNoDatabaseDriver(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imported := range pkg.Imports {
		if strings.Contains(imported, "jackc/pgx") {
			t.Fatalf("realtime imports %s", imported)
		}
	}
}
```
  Add a `block chan struct{}` field to `batchAuthorizer` (its `CheckMany` waits on it or on `ctx.Done()`), and the small `writes`/`snapshotWrites` accessors on `testConnection`.
- [ ] **Step 2: Run to see the failure:** `rtk go -C Server test ./internal/realtime -race -count=1 -run 'TestPublish|TestDroppedInvalidations|TestInvalidationReaches|TestRealtimeImports'`. Expected: `unknown field CoalesceWindow`, and `realtime imports github.com/jackc/pgx/v5/pgxpool`.
- [ ] **Step 3: Implement.**
  - **Coalescing.** The `Hub` gains `pendingMu sync.Mutex`, `pending map[string]Hint` and `armed bool`.
    - `Publish` validates as today, then keeps the newest hint per family: a different cursor domain, or a higher sequence in the same one, replaces the pending hint. When nothing is armed it arms `time.AfterFunc(hub.coalesce, hub.flush)`.
    - `flush` swaps the map out and calls `deliver(hint)` for each pending hint; `deliver` is today's `Publish` body (:335-373).
    - `FlushNow` calls `flush`. `Shutdown` stops the timer.
  - **Recheck.** The `Hub` gains `recheck chan struct{}` (capacity 1).
    - `InvalidationsDropped` makes a non-blocking send on it, and returns.
    - `runSweep` selects on the ticker, `recheck` and `ctx.Done()`. Each sweep runs under `context.WithTimeout(hub.ctx, hub.sweepTimeout)`; a `CheckMany` error, deadline included, closes every lease, as an authorizer failure does today.
    - A hub without an authorizer keeps today's close-all path, run on the sweep goroutine.
  - **Reservations.** `Invalidate` applies the same selector to `hub.reservations`: it deletes each match and releases its counts, so a later `Bind` sees `ErrReservationReleased`.
  - Delete `postgres.go`, `postgres_test.go`, the listener composition and the resyncer.
- [ ] **Step 4: Run green:** `rtk go -C Server test ./internal/realtime ./internal/delivery -race -count=1`; `rtk proxy env BT_TEST_PACKAGES='./internal/httpapi ./cmd/...' bash Server/scripts/test-db.sh`; `rtk proxy env BT_TEST_PACKAGES='./tests/protocol/... ./cmd/...' BT_TEST_TAGS=identitytest bash Server/scripts/test-db.sh`.
- [ ] **Step 5: Commit:** `rtk git add -A Server docs/protocol/v1.md && rtk git commit -m "feat(realtime): coalesce hints per family, recheck off the delivery goroutine, drop the PostgreSQL listener"`

### Task 8: The wake worker

**Files:**
- Create `Server/internal/wake/{tracker.go,worker.go,hub_sender.go,tracker_test.go,worker_test.go}`.
- Modify `Server/internal/sync/{exchange.go,serve.go}`: `Options.Progress`, and `Synced` after each completed request.
- Modify `Server/internal/delivery/observer.go`: `NewObserver(publisher, changes)`. Update its callers: `main.go`, `testkit/fixtures.go` and `delivery/observer_test.go`.
- Modify `Server/cmd/baby-tracker-server/main.go`: start the tracker and the worker on the maintenance pool.
- Modify `Server/scripts/protocol-e2e.sh` and `protocol_e2e_test.go`: a `wake proof:` line, waiting up to 10 s for a `device_sync_cursors` row per phone device.
- Modify `docs/protocol/v1.md:276-281` and `Server/README.md:571-575`: the wake paragraph.

**Interfaces:** Consumes `device_sync_cursors` (Task 2), the maintenance pool (Task 3) and `realtime.Hub.Publish` (Task 7). Produces:
```go
package wake
type Tracker struct{ /* mu; devices map[deviceKey]progress; families map[string]uint64 */ }
func NewTracker(maxDevices int) *Tracker // 0 means 200,000; beyond it new devices are dropped until the next flush
// Synced records one completed request: the cursor the device presented
// (acked, 0 for none) and, for a final page, the high water served
// (delivered, 0 otherwise). It never touches the database.
func (t *Tracker) Synced(familyID, deviceID string, acked, delivered uint64, at time.Time)
// Changed records a committed advance of the family's sequence.
func (t *Tracker) Changed(familyID string, sequence uint64)
type Wake struct { FamilyID string; DeviceIDs []string; Sequence uint64 }
type Sender interface{ Wake(ctx context.Context, wakes []Wake) error }
type HubSender struct{ Hub *realtime.Hub; Server protocol.ServerInfo } // one Publish per family
type Stats struct{ Flushed, Woken int }
type Worker struct {
	Pool               *pgxpool.Pool // maintenance
	Tracker            *Tracker
	Sender             Sender
	Now                func() time.Time
	Interval           time.Duration // 0 means 2s
	MinWakeInterval    time.Duration // 0 means 60s
	MaxWakesPerTick    int           // 0 means 500
	MaxFamiliesPerTick int           // 0 means 1000
}
func (w *Worker) Tick(ctx context.Context) (Stats, error) // at most 4 round trips: BEGIN, flush+scan batch, mark, COMMIT
func (w *Worker) Run(ctx context.Context, ready func(context.Context) error)

package sync
type Progress interface{ Synced(familyID, deviceID string, acked, delivered uint64, at time.Time) }
// Options.Progress (optional). ServeSync calls it after the commit:
// acked = request.Cursor.Sequence when present; delivered = HighWater.Sequence
// when NextPageToken == nil. ServeSnapshot: delivered = HighWater.Sequence on
// the final page.

package delivery
type ChangeRecorder interface{ Changed(familyID string, sequence uint64) }
func NewObserver(publisher HintPublisher, changes ChangeRecorder) *Observer // changes may be nil
```

- [ ] **Step 1: Write the failing tests** `wake/worker_test.go`:
```go
type recordingSender struct {
	mu    sync.Mutex
	calls [][]wake.Wake
}

func (s *recordingSender) Wake(_ context.Context, wakes []wake.Wake) error {
	s.mu.Lock(); defer s.mu.Unlock()
	s.calls = append(s.calls, wakes)
	return nil
}

func (s *recordingSender) woken() map[string]int {
	s.mu.Lock(); defer s.mu.Unlock()
	counts := map[string]int{}
	for _, call := range s.calls {
		for _, w := range call {
			for _, d := range w.DeviceIDs {
				counts[d]++
			}
		}
	}
	return counts
}

func TestWakeWorkerNeverStorms(t *testing.T) { // Review Focus 5
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, sender := wake.NewTracker(0), &recordingSender{}
	clock := h.Now
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: sender, Now: func() time.Time { return clock }}
	devices := []string{"d1000000-0000-4000-8000-000000000001", "d1000000-0000-4000-8000-000000000002",
		"d1000000-0000-4000-8000-000000000003", "d1000000-0000-4000-8000-000000000004"}
	seq := h.FamilySequence(t)
	for _, d := range devices {
		tracker.Synced(h.FamilyID, d, seq, seq, clock)
	}
	if _, err := w.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	for i := range 100 { // 100 writes over 50 s
		caretest.Seed(t, h, caretest.Feed(caretest.BabyA, i))
		tracker.Changed(h.FamilyID, h.FamilySequence(t))
		clock = clock.Add(500 * time.Millisecond)
		stats, err := w.Tick(t.Context())
		if err != nil || stats.Woken > 500 {
			t.Fatalf("tick %d: %+v %v", i, stats, err)
		}
	}
	for _, d := range devices {
		if sender.woken()[d] != 1 {
			t.Fatalf("device %s woken %d times in 50 s, want once", d, sender.woken()[d])
		}
	}
	if len(sender.calls) != 1 || len(sender.calls[0]) != 1 {
		t.Fatalf("hub told %d times (%+v), want one family wake", len(sender.calls), sender.calls)
	}
	// A minute on, the three devices still behind are woken again; the one
	// that pulled to the high water is not.
	high := h.FamilySequence(t)
	tracker.Synced(h.FamilyID, devices[0], high, high, clock)
	tracker.Changed(h.FamilyID, high)
	clock = clock.Add(time.Minute)
	if _, err := w.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	counts := sender.woken()
	if counts[devices[0]] != 1 || counts[devices[1]] != 2 || counts[devices[2]] != 2 || counts[devices[3]] != 2 || len(sender.calls) != 2 {
		t.Fatalf("second round %+v over %d calls; want the caught-up device left alone", counts, len(sender.calls))
	}
}

func TestWakeTickIsBoundedAndPinsItsRoundTrips(t *testing.T) {
	// CountedPool over h.Maintenance; 1,500 lagging devices across 2 families; Tick -> Woken == 500 (MaxWakesPerTick),
	// wire.Trips() <= 4 after one warm tick; three more ticks wake the rest; no device twice.
}

func TestWakeTickRollbackRequeuesProgress(t *testing.T) {
	// a Worker over a closed pool -> Tick errors; the same Tracker under a live Worker flushes every device recorded before
	// the failure (device_sync_cursors rows equal the Synced calls).
}

func TestWakeWorkerBackstopWakesLaggingDevicesAfterRestart(t *testing.T) {
	// rows written by one Worker; a new Worker with an empty Tracker (a restart) wakes lagging devices on its first ticks
	// without any Changed call; devices at the high water are not woken.
}

func TestFlushSkipsInactiveFamilies(t *testing.T) {
	// Synced for a family whose status is deletion_pending -> no device_sync_cursors row is written.
}
```
  `wake/tracker_test.go`, `TestTrackerKeepsTheHighestProgressAndIsBounded`:
  - `Synced` calls in any order keep the maximum acked and delivered sequences per device;
  - `Changed` keeps the maximum sequence per family;
  - past `maxDevices`, new devices are dropped and known ones still update.

  `sync/serve_test.go`, `TestServeSyncReportsProgress`:
  - a pull with cursor N whose final page has high water M calls `Synced(family, device, N, M, now)`;
  - a page with a token calls `Synced(…, N, 0, …)`;
  - the final snapshot page calls `Synced(…, 0, hw, …)`.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env BT_TEST_PACKAGES='./internal/wake ./internal/sync' BT_TEST_RUN='TestWake|TestTracker|TestFlush|TestServeSyncReportsProgress' bash Server/scripts/test-db.sh`. Expected: `package babytracker/server/internal/wake is not in std`.
- [ ] **Step 3: Implement.** One tick, in one maintenance transaction:
  1. `drain` the tracker's dirty devices (sorted by `(family_id, device_id)`) and up to `MaxFamiliesPerTick` changed families.
  2. One pipelined batch:
     - `set_config('lock_timeout','2s',true)`;
     - the flush, when there are devices;
     - the lag scan over the changed families;
     - while the startup backstop is not finished, one keyset page of the backlog (`(family_id, device_id) > ($k1,$k2) ORDER BY family_id, device_id LIMIT MaxWakesPerTick`); it is finished when a page comes back short.
  3. The mark, then COMMIT.
  4. After the commit, `Sender.Wake` with one `Wake` per family.
  5. On any error, `restore` the drained progress into the tracker (newer values win). Wakes are at most once per sequence: a crash between the commit and `Wake` loses that hint, never duplicates it.
```sql
-- flush (inactive families' progress is dropped)
INSERT INTO device_sync_cursors(family_id,device_id,acked_sequence,delivered_sequence,seen_at)
SELECT v.f, v.d, v.a, v.dl, v.s FROM unnest($1::uuid[],$2::uuid[],$3::bigint[],$4::bigint[],$5::timestamptz[]) AS v(f,d,a,dl,s)
JOIN families fam ON fam.id = v.f AND fam.status = 'active'
ON CONFLICT (family_id, device_id) DO UPDATE SET acked_sequence = greatest(device_sync_cursors.acked_sequence, EXCLUDED.acked_sequence),
  delivered_sequence = greatest(device_sync_cursors.delivered_sequence, EXCLUDED.delivered_sequence),
  seen_at = greatest(device_sync_cursors.seen_at, EXCLUDED.seen_at);
-- lag scan (device_sync_cursors_pkey, families_pkey)
SELECT d.family_id::text, d.device_id::text, f.next_sequence FROM device_sync_cursors d JOIN families f ON f.id = d.family_id
WHERE d.family_id = ANY($1::uuid[]) AND f.status = 'active'
  AND f.next_sequence > greatest(d.acked_sequence, d.delivered_sequence, d.woken_sequence)
  AND (d.woken_at IS NULL OR d.woken_at <= $2)
ORDER BY d.family_id, d.device_id LIMIT $3;
-- mark
UPDATE device_sync_cursors d SET woken_sequence = v.seq, woken_at = $1
FROM unnest($2::uuid[],$3::uuid[],$4::bigint[]) AS v(f,dv,seq) WHERE d.family_id = v.f AND d.device_id = v.dv;
```
  - `HubSender.Wake` calls `Hub.Publish(Hint{FamilyID, Cursor{InstanceID, RecoveryEpoch, FamilyID, Sequence}})` once per family; the hub's coalescing and per-lease `highest` deduplicate further.
  - `main.go`: `tracker := wake.NewTracker(0)`; `delivery.NewObserver(liveHub, tracker)`; `familysync.Options{…, Progress: tracker}`; `go (&wake.Worker{Pool: maintenancePool, Tracker: tracker, Sender: wake.HubSender{Hub: liveHub, Server: serverInfo}, Now: time.Now}).Run(ctx, monitor.Check)`.
  - **v1.md.** The worker records each device's acknowledged and delivered cursor. At most once a minute per device, it re-announces the family's high water to devices that are behind. Push delivery is not part of this server.
- [ ] **Step 4: Run green:** the Step 2 command; `rtk proxy env BT_TEST_PACKAGES='./internal/delivery ./internal/testkit ./cmd/... ./scripts' bash Server/scripts/test-db.sh`.
- [ ] **Step 5: Commit:** `rtk git add -A Server docs/protocol/v1.md && rtk git commit -m "feat(wake): track device cursors in memory and wake lagging devices from a bounded worker"`

### Task 9: The maintenance loop, authority sweeps and audit partition rolling

**Files:**
- Create `Server/internal/maintenance/{doc.go,loop.go,retention.go,session_sweep.go,audit_partitions.go,loop_test.go,session_sweep_test.go,audit_partitions_test.go}`.
- Create `Server/internal/authority/retention.go` and `retention_test.go`.
- Modify `Server/cmd/baby-tracker-server/main.go`: the retention loop (10 s) and the audit loop (1 h).

**Interfaces:** Consumes the background authority and the maintenance pool (Task 3), and `app.ensure_audit_partitions` (Task 2). Produces:
```go
package authority
// Each prune is one authority transaction (BEGIN, the authority flag and its statements pipelined, COMMIT): at most limit rows,
// rows a concurrent transaction holds skipped (FOR UPDATE SKIP LOCKED), never an account or family row locked.
func (s *Service) PruneSessions(ctx context.Context, cutoff time.Time, limit int) (int, error)       // consumed/revoked before cutoff, active with refresh_expires_at before cutoff; never a session a refresh receipt names
func (s *Service) PruneChallenges(ctx context.Context, now time.Time, limit int) (int, error)       // expires_at <= now
func (s *Service) PruneRefreshReceipts(ctx context.Context, now time.Time, limit int) (int, error)  // LEAST(refresh_expires_at, effective_deadline) <= now

package maintenance
type Sweep interface {
	Name() string
	Run(ctx context.Context, now time.Time) (int, error) // at most one bounded batch; idempotent; safe beside requests and itself
}
type Loop struct {
	Name     string
	Sweeps   []Sweep
	Interval time.Duration
	Timeout  time.Duration // per sweep; 0 means 30s
	Ready    func(context.Context) error
	Now      func() time.Time
	Logger   *slog.Logger
}
func (l Loop) Run(ctx context.Context)
func (l Loop) Once(ctx context.Context) map[string]int // one pass; a failing sweep is logged and the others still run
const (
	RetentionInterval        = 10 * time.Second
	AuditPartitionInterval   = time.Hour
	AuditPartitionHorizon    = 92 * 24 * time.Hour
	ConsumedSessionRetention = 24 * time.Hour // owner decision D2: after it, a consumed token's replay is session_revoked, not lineage revocation
	SessionBatch             = 5000                // per statement per tick: 500/s capacity per kind against ~70/s consumed
	ChallengeBatch           = 5000
	RefreshReceiptBatch      = 2000
)
type SessionSweep struct{ Authority *authority.Service } // receipts, then sessions, then challenges
type AuditPartitionSweep struct{ Pool *pgxpool.Pool }     // SELECT app.ensure_audit_partitions(now + AuditPartitionHorizon)
```

- [ ] **Step 1: Write the failing tests.**
  - `authority/retention_test.go`, using `sessions_test.go`'s `liveAccount`, `refreshOf`, `binding`, `newAttempt`, `sessionRow` and `wantCode`:
```go
func TestPruneSessionsKeepsWhatARetryNeeds(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	_, _, rawA := f.Session(t, account, device, "account", "", "")
	attempt := newAttempt(t)
	recoverable, err := f.Service.Refresh(t.Context(), refreshOf(f, rawA, attempt, binding(account, device)))
	if err != nil {
		t.Fatal(err)
	}
	_, _, rawB := f.Session(t, account, device, "account", "", "")
	plain, err := f.Service.Refresh(t.Context(), refreshOf(f, rawB, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	cutoff := f.Now.Add(time.Hour)
	if n, err := f.Service.PruneSessions(t.Context(), cutoff, 100); err != nil || n != 1 {
		t.Fatalf("pruned %d (%v), want only the receipt-free consumed parent", n, err)
	}
	if n := count(t, f, `SELECT count(*) FROM sessions WHERE id=$1`, plain.Parent.ID); n != 0 {
		t.Fatal("the receipt-free consumed parent survived the prune")
	}
	retry, err := f.Service.Refresh(t.Context(), refreshOf(f, rawA, attempt, binding(account, device)))
	if err != nil || !retry.Recovered || retry.AccessToken != recoverable.AccessToken {
		t.Fatalf("the lost-ack retry after a prune: %+v %v", retry, err)
	}
	f.Exec(t, `DELETE FROM refresh_receipts WHERE child_id=$1`, recoverable.Child.ID) // acknowledged
	if n, err := f.Service.PruneSessions(t.Context(), cutoff, 100); err != nil || n != 1 {
		t.Fatalf("second pass pruned %d (%v)", n, err)
	}
	_, err = f.Service.Refresh(t.Context(), refreshOf(f, rawA, attempt, binding(account, device)))
	wantCode(t, err, "session_revoked") // past the 24-hour window a replay is refused, not answered with reuse revocation (decision D2)
	if row, _ := sessionRow(t, f, recoverable.Child.ID); row.Status != "active" {
		t.Fatalf("an active child was pruned: %s", row.Status)
	}
}

func TestPruneSessionsIsBoundedAndSkipsLockedRows(t *testing.T) {
	// 10 consumed parents; one held FOR UPDATE in an open authority transaction; PruneSessions(limit 4) returns 4
	// within 1 s while the lock is held; repeated passes delete 9; the held row survives until released.
	// Crash safety: a PruneSessions whose context is cancelled after its DELETE batch and before COMMIT (a test hook in
	// export_test.go) deletes nothing, and the next call deletes the same rows.
}

func TestPruneStatementsUseTheirIndexes(t *testing.T) {
	// 20,000 sessions inserted with generate_series as the owner (1% consumed, 1% revoked, 1% refresh-expired), ANALYZE;
	// EXPLAIN (FORMAT JSON) each prune statement as the runtime with the authority flag: the plans name
	// sessions_consumed_idx, sessions_revoked_idx, sessions_expired_idx, auth_challenges_expiry_idx and
	// refresh_receipts_expiry_idx, and contain no Seq Scan on the swept table.
}

func TestPruneChallengesAndRefreshReceipts(t *testing.T) {
	// an expired and a live challenge -> only the expired one goes; a receipt with refresh_expires_at in the past -> deleted,
	// one in the future -> kept; neither prune locks an account (pg_locks of the pruning backend shows no accounts relation lock).
}
```
  - `maintenance/session_sweep_test.go`, the owner's 24-hour window (decision D2) through the production sweep:
```go
func TestSessionSweepKeepsConsumedSessionsForTwentyFourHours(t *testing.T) {
	h := testkit.New(t)
	old := h.RefreshToken("owner")
	consumedAt := h.Now // the harness clock is fixed: the setup refreshes consumed their parents at this time too
	h.Owner = h.Refresh("owner") // consumes old's session; the owner continues on the child
	sweep := maintenance.SessionSweep{Authority: h.Authority}
	before := consumedSessions(t, h)
	if before == 0 {
		t.Fatal("no consumed session to prune")
	}
	if _, err := sweep.Run(t.Context(), consumedAt.Add(maintenance.ConsumedSessionRetention-time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := consumedSessions(t, h); got != before {
		t.Fatalf("a consumed session younger than 24 h was pruned: %d -> %d", before, got)
	}
	if _, err := sweep.Run(t.Context(), consumedAt.Add(maintenance.ConsumedSessionRetention+time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := consumedSessions(t, h); got != 0 {
		t.Fatalf("%d consumed sessions older than 24 h survived", got)
	}
	// The accepted trade-off: past the window a replay is refused, and the owner's lineage lives on.
	if _, err := h.Auth.Refresh(t.Context(), old); auth.Code(err) != "session_revoked" {
		t.Fatalf("replay of a pruned consumed token: %v, want session_revoked", err)
	}
	_ = h.Pull(t, h.Owner, h.Cursor(0), nil) // the current session still works
}

// consumedSessions counts consumed sessions as an authority transaction does.
func consumedSessions(t *testing.T, h *testkit.Harness) int {
	t.Helper()
	tx, err := h.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `SELECT set_config('app.authority','1',true)`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE status='consumed'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
```
  - `maintenance/loop_test.go`, with fake sweeps:
    - `TestLoopRunsOnlyWhenReady`: `Ready` refusing gives zero runs;
    - `TestLoopIsolatesAFailingSweep`: one sweep errs, the others run and the error is logged once;
    - `TestLoopBoundsEachSweep`: a sweep that blocks on `ctx` returns at `Timeout`.
  - `maintenance/audit_partitions_test.go`, `TestAuditPartitionSweepKeepsThreeMonthsAhead`:
    - `AuditPartitionSweep{Pool: h.Maintenance}.Run(ctx, h.DatabaseNow())` leaves partitions for every month from `max(2027-01, current)` through now + 92 days, and a second run returns 0;
    - `security_audit_events_default` holds no row after the harness's sign-ins (`SELECT count(*)` as the runtime with the authority flag);
    - restart safety is idempotence: two sweeps run concurrently create each missing month once (the second sees the first's partition or fails with `42P07` on the race, which the sweep treats as done and the next run confirms with 0).
- [ ] **Step 2: Run to see the failure:** `rtk proxy env BT_TEST_PACKAGES='./internal/authority ./internal/maintenance' BT_TEST_RUN='TestPrune|TestLoop|TestAuditPartitionSweep|TestSessionSweep' bash Server/scripts/test-db.sh`. Expected: `undefined: (*authority.Service).PruneSessions`.
- [ ] **Step 3: Implement** `authority/retention.go`. There is one pipelined batch per prune; each statement chooses rows with `FOR UPDATE SKIP LOCKED`.
```sql
-- PruneSessions (three statements, one round trip after BEGIN)
DELETE FROM sessions WHERE id IN (SELECT s.id FROM sessions s WHERE s.status = 'consumed' AND s.consumed_at < $1
  AND NOT EXISTS (SELECT 1 FROM refresh_receipts r WHERE r.parent_id = s.id OR r.child_id = s.id)
  ORDER BY s.consumed_at LIMIT $2 FOR UPDATE SKIP LOCKED);
DELETE FROM sessions WHERE id IN (SELECT s.id FROM sessions s WHERE s.status = 'revoked' AND s.revoked_at < $1
  AND NOT EXISTS (SELECT 1 FROM refresh_receipts r WHERE r.parent_id = s.id OR r.child_id = s.id)
  ORDER BY s.revoked_at LIMIT $2 FOR UPDATE SKIP LOCKED);
DELETE FROM sessions WHERE id IN (SELECT s.id FROM sessions s WHERE s.status = 'active' AND s.refresh_expires_at < $1
  AND NOT EXISTS (SELECT 1 FROM refresh_receipts r WHERE r.parent_id = s.id OR r.child_id = s.id)
  ORDER BY s.refresh_expires_at LIMIT $2 FOR UPDATE SKIP LOCKED);
-- PruneChallenges
DELETE FROM auth_challenges WHERE token_digest IN (SELECT token_digest FROM auth_challenges WHERE expires_at <= $1
  ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED);
-- PruneRefreshReceipts
DELETE FROM refresh_receipts WHERE parent_id IN (SELECT parent_id FROM refresh_receipts
  WHERE LEAST(refresh_expires_at, effective_deadline) <= $1 ORDER BY LEAST(refresh_expires_at, effective_deadline) LIMIT $2 FOR UPDATE SKIP LOCKED);
```
  - `SessionSweep.Run(ctx, now)` calls `PruneRefreshReceipts(now)`, then `PruneSessions(now − ConsumedSessionRetention)`, then `PruneChallenges(now)`, and sums the counts.
  - `AuditPartitionSweep.Run(ctx, now)` runs `SELECT app.ensure_audit_partitions($1)` with `now + AuditPartitionHorizon` on the maintenance pool. A `42P07` (a concurrent run created the same month) counts as 0 created, not as an error.
  - `main.go` composes:
    - `maintenance.Loop{Name: "retention", Sweeps: []Sweep{SessionSweep{Authority: backgroundAuthority}}, Interval: RetentionInterval, Ready: monitor.Check, Now: time.Now, Logger: logger}` (Task 10 appends to it);
    - `maintenance.Loop{Name: "audit-partitions", Sweeps: []Sweep{AuditPartitionSweep{Pool: maintenancePool}}, Interval: AuditPartitionInterval, Ready: monitor.Check, …}`.
- [ ] **Step 4: Run green:** the Step 2 command; then `rtk proxy env BT_TEST_PACKAGES='./internal/auth ./internal/authority ./cmd/...' bash Server/scripts/test-db.sh`. Expected: the key inventory tests still pass; a pruned key is no longer referenced.
- [ ] **Step 5: Commit:** `rtk git add -A Server/internal/maintenance Server/internal/authority Server/cmd && rtk git commit -m "feat(maintenance): bounded session, challenge and recovery-receipt sweeps and audit partition rolling"`

### Task 10: Care retention — receipts with charge release, bindings, snapshot sessions, device cursors, export jobs

**Files:**
- Create `Server/internal/maintenance/{care_retention.go,care_retention_test.go,export_test.go}`.
- Create `Server/internal/care/caretest/counters.go`: move the counters part of `wantCountersMatchRows` (`care/apply_test.go:594-653`, with `legacyReceiptCharge` at :565-574) into `caretest.AssertCounters`. `wantCountersMatchRows` calls it and keeps its per-row legacy checks.
- Modify `Server/internal/care/entity.go:281-292`:
  - `ReceiptChargedBytes`' RELEASE note points at `maintenance.ReceiptSweep`;
  - add `StoredReceiptCharge`.
- Modify `Server/internal/export/worker.go:116-138`: `expire` also deletes expired or failed jobs past 7 days. Add `Server/internal/export/retention_test.go`.
- Modify `Server/cmd/baby-tracker-server/main.go`: the sweeps join the retention loop.
- Modify `docs/protocol/v1.md`, the receipts sentence ("kept at least 30 days" becomes "kept 30 days, then deleted and their charge released"), and the continuation sentence ("a token whose binding was swept, an hour after it expired, is `invalid_page_token`").

**Interfaces:** Consumes Task 9's `Loop`/`Sweep` and Task 2's grants. Produces:
```go
package care
// StoredReceiptCharge is the charge a stored receipt's outcome was written
// with: decoded and encoded as the write path encoded it (plan.go addReceipt), plus the overhead.
func StoredReceiptCharge(outcomeJSON []byte) (int64, error)

package caretest
// AssertCounters fails t unless the family's care counters equal the
// charge recomputed from what remains: rows, retained import staging and
// live receipts. The receipt worker releases each deleted receipt's charge
// by subtraction, and a completed purge has zeroed the counters with no row
// left, so the recomputation over existing rows holds after both. (It no
// longer assumes import or receipt charges are never released.)
func AssertCounters(t *testing.T, h *testkit.Harness, familyID string)

package maintenance
const (
	ReceiptRetention      = 30 * 24 * time.Hour
	PageGrace             = time.Hour
	DeviceCursorRetention = 35 * 24 * time.Hour
	ReceiptBatch          = 2000 // 200/s capacity against under 10 writes/s
	ExpiryBatch           = 5000
)
type ReceiptSweep struct{ Pool *pgxpool.Pool; Batch int }
type ExpirySweep struct{ Pool *pgxpool.Pool; Table string } // "sync_page_bindings" | "sync_snapshot_sessions" | "device_sync_cursors"
// export_test.go: func (s *ReceiptSweep) AfterDelete(hook func()) // a test hook between the delete and the release
```

- [ ] **Step 1: Write the failing tests** `maintenance/care_retention_test.go`:
```go
func TestReceiptRetentionKeepsTheRetryWindow(t *testing.T) { // Review Focus 1
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	old := caretest.Feed(caretest.BabyA, 1)
	caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{old})
	acceptedOld := h.Now
	h.Now = h.Now.Add(maintenance.ReceiptRetention - time.Second)
	young := caretest.Feed(caretest.BabyA, 2)
	caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{young})
	sweep := &maintenance.ReceiptSweep{Pool: h.Maintenance, Batch: 1000}
	if _, err := sweep.Run(t.Context(), h.Now); err != nil {
		t.Fatal(err)
	}
	if _, found := caretest.StoredReceipt(t, h, h.Owner, old.MutationID); !found {
		t.Fatal("a receipt one second short of 30 days was deleted")
	}
	if replay := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{old}); replay.Outcomes[0].Status != "already_applied" {
		t.Fatalf("retry inside the window: %+v", replay.Outcomes[0])
	}
	h.Now = acceptedOld.Add(maintenance.ReceiptRetention)
	if _, err := sweep.Run(t.Context(), h.Now); err != nil {
		t.Fatal(err)
	}
	if _, found := caretest.StoredReceipt(t, h, h.Owner, old.MutationID); found {
		t.Fatal("a 30-day-old receipt survived")
	}
	if _, found := caretest.StoredReceipt(t, h, h.Owner, young.MutationID); !found {
		t.Fatal("a younger receipt was deleted")
	}
	caretest.AssertCounters(t, h, h.FamilyID)
	// A writer holding the family row: the sweep skips the family at once.
	holder := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	if _, err := care.LockFamily(t.Context(), holder, h.FamilyID); err != nil {
		t.Fatal(err)
	}
	h.Now = h.Now.Add(maintenance.ReceiptRetention)
	start := time.Now()
	if n, err := sweep.Run(t.Context(), h.Now); err != nil || n != 0 || time.Since(start) > time.Second {
		t.Fatalf("sweep beside a writer: %d %v after %v", n, err, time.Since(start))
	}
	_ = holder.Rollback(t.Context())
	if n, err := sweep.Run(t.Context(), h.Now); err != nil || n == 0 {
		t.Fatalf("after the writer: %d %v", n, err)
	}
	caretest.AssertCounters(t, h, h.FamilyID)
}

func TestReceiptRetentionReleasesExactCharges(t *testing.T) { // Review Focus 2
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	mutation := func(n int) string { return fmt.Sprintf("7a000000-0000-4000-8000-%012d", n) }
	for step := range 40 {
		h.Now = h.Now.Add(time.Duration(step%7) * 18 * time.Hour)
		feed := caretest.Feed(caretest.BabyA, 1000+step)
		batch := []protocol.Mutation{feed,
			caretest.Update(feed, mutation(10*step+1), 1, caretest.WithNote(fmt.Sprintf("step %d <&>", step))), // an update
			caretest.Feed("11000000-0000-4000-8000-0000000000ff", step)}                                          // baby_not_found: a refusal with a receipt
		if step%3 == 0 {
			batch = append(batch, caretest.Delete(feed, mutation(10*step+2), 2))
		}
		if step%5 == 0 { // timer and completion IDs in their own ranges, clear of caretest.Feed's
			start, finish, edit, stop := caretest.FeedTimerLifecycle(caretest.BabyA, fmt.Sprintf("5c000000-0000-4000-8000-%012d", step),
				fmt.Sprintf("2c000000-0000-4000-8000-%012d", step))
			batch = append(batch, start, finish, edit, stop)
		}
		caretest.ApplyCommitted(t, h, h.Owner, batch)
		caretest.ApplyCommitted(t, h, h.Owner, batch[:1]) // a replay writes no receipt
	}
	caretest.AssertCounters(t, h, h.FamilyID)
	sweep := &maintenance.ReceiptSweep{Pool: h.Maintenance, Batch: 7} // small batches cross time and mutation boundaries
	ctx, cancel := context.WithCancel(t.Context())
	sweep.AfterDelete(cancel) // the first batch is rolled back between its delete and its release
	if _, err := sweep.Run(ctx, h.Now.Add(maintenance.ReceiptRetention)); err == nil {
		t.Fatal("a cancelled batch reported success")
	}
	sweep.AfterDelete(nil)
	caretest.AssertCounters(t, h, h.FamilyID) // nothing deleted, nothing released
	for i := range 60 {
		if _, err := sweep.Run(t.Context(), h.Now.Add(maintenance.ReceiptRetention+time.Duration(i)*12*time.Hour)); err != nil {
			t.Fatal(err)
		}
		caretest.AssertCounters(t, h, h.FamilyID)
	}
	var left int
	_ = h.Maintenance.QueryRow(t.Context(), `SELECT count(*) FROM care_receipts WHERE family_id=$1`, h.FamilyID).Scan(&left)
	if left != 0 {
		t.Fatalf("%d receipts left after every one fell due", left)
	}
}

func TestExpirySweepsDeleteOnlyWhatIsPastTheGrace(t *testing.T) {
	// page size 1: a pull issues a binding; a snapshot issues a session; Synced+flush writes a device cursor.
	// At expires_at + 59 min nothing is deleted (a replay still answers page_expired); at expires_at + 61 min both are deleted
	// and the replay answers invalid_page_token; the device cursor goes at seen_at + 35 days; unexpired rows survive.
	// Each statement is one round trip and EXPLAIN shows sync_page_bindings_expiry_idx / sync_snapshot_sessions_expiry_idx /
	// device_sync_cursors_seen_idx on 20,000 seeded rows.
	// Crash safety: a sweep cancelled before COMMIT deletes nothing; a second sweep running beside the first on the same
	// table (two goroutines) deletes each row once and neither waits (FOR UPDATE SKIP LOCKED).
}

func TestReceiptSweepPlanUsesTheRetentionIndex(t *testing.T) {
	// 20,000 receipts across 50 families, 1% due; EXPLAIN (FORMAT JSON) of the family-selection statement names
	// care_receipts_retention_idx and the delete names care_receipts_pkey; no Seq Scan on care_receipts.
}
```
  `export/retention_test.go`, `TestExpiredExportJobsAreDeletedAfterSevenDays`:
  - a job that expired 8 days ago is deleted by `expire`, with its chunks first;
  - one that expired 6 days ago, and a ready job, are kept;
  - the statement uses `export_jobs_expiry_idx`.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env BT_TEST_PACKAGES='./internal/maintenance ./internal/export ./internal/care' BT_TEST_RUN='TestReceipt|TestExpiry|TestExpiredExport|TestCounters' bash Server/scripts/test-db.sh`. Expected: `undefined: maintenance.ReceiptSweep` and `undefined: caretest.AssertCounters`.
- [ ] **Step 3: Implement.** `ReceiptSweep.Run` is one maintenance transaction (`lock_timeout 2s`) with at most 5 round trips per batch:
```sql
-- 1. the families of the oldest due receipts, locked in ID order, skipping any a writer holds
SELECT f.id::text FROM families f
WHERE f.id IN (SELECT DISTINCT r.family_id FROM (SELECT family_id FROM care_receipts WHERE accepted_at <= $1 ORDER BY accepted_at LIMIT $2) r)
ORDER BY f.id FOR NO KEY UPDATE OF f SKIP LOCKED;
-- 2. their due receipts, oldest first, at most $2
DELETE FROM care_receipts r WHERE (r.device_id, r.mutation_id) IN (
  SELECT device_id, mutation_id FROM care_receipts WHERE family_id = ANY($3::uuid[]) AND accepted_at <= $1 ORDER BY accepted_at LIMIT $2)
RETURNING r.family_id::text, r.outcome::text;
-- 3. release, per family, the sum of care.StoredReceiptCharge(outcome) (Go)
UPDATE families f SET care_charged_bytes = f.care_charged_bytes - v.bytes FROM unnest($1::uuid[], $2::bigint[]) AS v(id, bytes) WHERE f.id = v.id;
```
  - A release that would make `care_charged_bytes` negative fails its CHECK and rolls the batch back; that only happens on drift, and the next tick logs the same failure.
  - `StoredReceiptCharge` unmarshals into `protocol.Outcome`, marshals again and adds `ReceiptOverheadBytes`. A decode failure rolls the batch back and logs the receipt's `(device_id, mutation_id)`.
  - `ExpirySweep` deletes the expired rows of its table, at most `ExpiryBatch`:
    - `sync_page_bindings`: `expires_at <= now − PageGrace`, deleted through `(family_id, token_digest) IN (SELECT … ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED)`;
    - `sync_snapshot_sessions`: the same on `(family_id, id)`;
    - `device_sync_cursors`: `seen_at <= now − DeviceCursorRetention`.
  - `export.Worker.expire` adds `DELETE FROM public.export_chunks WHERE export_id IN (<ids>)`, then `DELETE FROM public.export_jobs WHERE id IN (SELECT id FROM public.export_jobs WHERE state IN ('expired','failed') AND expires_at < now() - interval '7 days' ORDER BY expires_at, id LIMIT 100)`.
- [ ] **Step 4: Run green:** `rtk proxy env BT_TEST_PACKAGES='./internal/maintenance ./internal/export ./internal/care ./internal/sync' bash Server/scripts/test-db.sh`.
- [ ] **Step 5: Commit:** `rtk git add -A Server docs/protocol/v1.md && rtk git commit -m "feat(maintenance): 30-day receipt retention with charge release and TTL sweeps"`

### Task 11: The family purge, and the worker/request concurrency proof

**Files:**
- Create `Server/internal/maintenance/{purge.go,purge_test.go,concurrency_test.go}`.
- Modify `Server/internal/authority/retention.go`: add `FamilyPurgeWork` and its test.
- Modify `Server/cmd/baby-tracker-server/main.go`: the purge loop (30 s) over `authorityService` (the request authority, so its invalidations reach the hub) and the maintenance pool.

**Interfaces:** Consumes `StartFamilyPurge`/`CompleteFamilyPurge` (`authority/lifecycle.go:412-505`), `RetentionWorkerActorID`, `derivedUUID`, Tasks 9–10 and `caretest.AssertCounters`. Produces:
```go
package authority
type PurgeWork struct {
	FamilyID       string
	StartCommandID string // "" for a family whose purge has not started
}
// FamilyPurgeWork lists purges to drive, started ones first (resumed after a
// crash by their purge_command_id), then deletion-pending families due at now,
// by family ID, at most limit. One authority read.
func (s *Service) FamilyPurgeWork(ctx context.Context, now time.Time, limit int) ([]PurgeWork, error)

package maintenance
const (
	PurgeInterval = 30 * time.Second
	PurgeBatch    = 5000 // rows per step per tick
)
// Purger is a Sweep. Each tick it drives at most one family one step:
//  1. start (a StartFamilyPurge exact retry is harmless: its command ID is derived from the family);
//  2. delete one bounded batch, in this order: import staging (cascading through mappings, before any baby row, because
//     import_baby_mappings references care_entities), receipts, continuation bindings, snapshot sessions, device cursors,
//     non-baby care rows, babies;
//  3. when a locked transaction finds every step empty, zero the counters in it, then CompleteFamilyPurge.
// Between steps the counters still hold the deleted rows' charges; nothing writes to a purging family, and step 3 settles them.
type Purger struct { Authority *authority.Service; Pool *pgxpool.Pool; Batch int }
func purgeCommandIDs(familyID string) (start, complete, receipt string) // derivedUUID-style: deterministic per family
```

- [ ] **Step 1: Write the failing tests** `maintenance/purge_test.go`:
```go
func TestPurgeDeletesTheFamilysCareDataAndCompletes(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t, h.Limits)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	caretest.Seed(t, h, caretest.MixedBatch(caretest.BabyA, 30)...)
	caretest.Seed(t, h, caretest.StartTimer(caretest.BabyA, caretest.TimerA, "feed"))
	h.Import(t, h.Owner, "8a000000-0000-4000-8000-000000000002", "Mira", "2026-01-01") // a mapping to a baby row
	_ = h.Pull(t, h.Caregiver, h.Cursor(0), nil)                                      // a continuation binding
	if _, err := h.Service.FullSnapshot(t.Context(), h.Caregiver, protocol.SnapshotRequest{}); err != nil {
		t.Fatal(err) // a snapshot session
	}
	sequence := h.FamilySequence(t)
	if _, err := h.Family.ScheduleDeletion(t.Context(), h.Owner, h.CommandID(), true); err != nil {
		t.Fatal(err)
	}
	h.Now = h.Now.Add(30 * 24 * time.Hour)
	purger := &maintenance.Purger{Authority: h.Authority, Pool: h.Maintenance, Batch: 7}
	for tick := 0; familyStatus(t, h, h.FamilyID) != "purge_completed"; tick++ {
		if tick > 200 {
			t.Fatal("purge did not finish in 200 ticks")
		}
		if _, err := purger.Run(t.Context(), h.Now); err != nil {
			t.Fatal(err)
		}
	}
	// The maintenance login sees these tables whole (000031 policies), so a
	// zero here is not a tenant-visibility artefact.
	for _, table := range []string{"care_entities", "care_receipts", "sync_page_bindings", "sync_snapshot_sessions", "import_sessions", "device_sync_cursors"} {
		var n int
		if err := h.Maintenance.QueryRow(t.Context(), `SELECT count(*) FROM `+table+` WHERE family_id=$1`, h.FamilyID).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s holds %d rows of the purged family (%v)", table, n, err)
		}
	}
	for _, table := range []string{"import_chunks", "import_baby_candidates", "import_record_manifests", "import_baby_mappings"} {
		if n := h.Count(t, table, h.FamilyID); n != 0 {
			t.Fatalf("%s holds %d rows of the purged family", table, n)
		}
	}
	caretest.AssertCounters(t, h, h.FamilyID) // all four counters zero
	if got := h.FamilySequence(t); got != sequence {
		t.Fatalf("next_sequence moved %d -> %d", sequence, got)
	}
	// invitations kept (Phase 1 carry: Redeem's exact retry), audit: family_purge_started then family_purge_completed by RetentionWorkerActorID
}

// familyStatus is a family's status, read by the maintenance login (column
// SELECT on families.status, 000031).
func familyStatus(t *testing.T, h *testkit.Harness, familyID string) string {
	t.Helper()
	var status string
	if err := h.Maintenance.QueryRow(t.Context(), `SELECT status::text FROM families WHERE id=$1`, familyID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestPurgeResumesAfterACrashAtEveryStep(t *testing.T) {
	// for each step k (start, each delete kind, zero, complete): a Purger whose hook fails at step k; a fresh Purger with the same
	// Authority finishes the purge; FamilyPurgeWork returns the started family with its StartCommandID; no step runs twice
	// to a different effect (the audit holds one purge_started and one purge_completed).
}

func TestImportStagingIsDeletedBeforeBabies(t *testing.T) {
	// the cascade-order rule pinned: as the maintenance login, deleting the family's baby rows while an import_baby_mappings
	// row references one fails with 23503; deleting import_sessions first (cascading through chunks, candidates, manifests,
	// conflicts, receipts and mappings) then the babies succeeds.
}

func TestFamilyRowDeleteCascadesThroughImportMappings(t *testing.T) {
	// as the table owner, DELETE FROM families WHERE id=$1 for a family with a completed import mapping succeeds:
	// the NO ACTION check from import_baby_mappings to care_entities runs at statement end, after both cascades.
}
```
  `maintenance/concurrency_test.go`:
```go
func TestMaintenanceAndRequestsNeverDeadlock(t *testing.T) { // Review Focus 3
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	viewer := h.AddMember(t, "viewer", "viewer")
	// Family B: the outsider's own family, given 20 sync batches (rows and
	// receipts), then scheduled for deletion, so the purge and the receipt
	// sweep both work on it while family A takes writes.
	familyB := deletionPendingFamily(t, h)
	owner := mustDigests(t, h, "owner")
	far := h.Now.Add(60 * 24 * time.Hour) // past B's purge_after and every receipt's 30 days
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var failures atomic.Int32
	classify := func(where string, err error) {
		var pgErr *pgconn.PgError
		if err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return
		}
		if errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "55P03") {
			t.Logf("%s: %s", where, pgErr.Code)
		} else {
			t.Logf("%s: %v", where, err)
		}
		failures.Add(1)
	}
	tracker := wake.NewTracker(0)
	var wg sync.WaitGroup
	for writer := range 2 {
		wg.Go(func() {
			for n := 0; ctx.Err() == nil; n++ {
				request := h.SyncRequest(nil, nil)
				request.Mutations = []protocol.Mutation{caretest.Feed(caretest.BabyA, 200000*(writer+1)+n)}
				_, err := h.Service.ServeSync(ctx, authority.Request{Digests: owner}, request)
				classify("writer A", err)
				tracker.Changed(h.FamilyID, uint64(n))
			}
		})
	}
	wg.Go(func() {
		for role := "caregiver"; ctx.Err() == nil; role = map[string]string{"caregiver": "viewer", "viewer": "caregiver"}[role] {
			_, err := h.Family.ChangeRole(ctx, h.Owner, h.CommandID(), viewer.AccountID, role)
			classify("role change", err)
		}
	})
	receipts := &maintenance.ReceiptSweep{Pool: h.Maintenance, Batch: 50}
	sessions := maintenance.SessionSweep{Authority: h.Authority}
	purger := &maintenance.Purger{Authority: h.Authority, Pool: h.Maintenance, Batch: 50}
	worker := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Now: func() time.Time { return far }}
	wg.Go(func() { for ctx.Err() == nil { _, err := receipts.Run(ctx, far); classify("receipts", err) } })
	wg.Go(func() { for ctx.Err() == nil { _, err := sessions.Run(ctx, h.Now); classify("sessions", err) } })
	wg.Go(func() { for ctx.Err() == nil { _, err := purger.Run(ctx, far); classify("purge", err) } })
	wg.Go(func() { for ctx.Err() == nil { _, err := worker.Tick(ctx); classify("wake", err) } })
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d failures (deadlocks, lock timeouts or refusals) between workers and requests", failures.Load())
	}
	caretest.AssertCounters(t, h, h.FamilyID)
	// The purge zeroes B's counters in the transaction of its last delete, so
	// they equal B's rows once it completes, whichever worker deleted which
	// receipt first.
	for tick := 0; familyStatus(t, h, familyB) != "purge_completed"; tick++ {
		if tick > 500 {
			t.Fatal("B's purge did not finish")
		}
		if _, err := purger.Run(t.Context(), far); err != nil {
			t.Fatal(err)
		}
	}
	caretest.AssertCounters(t, h, familyB)
}
```
  `discardSender` (`func (discardSender) Wake(context.Context, []wake.Wake) error { return nil }`) and two small helpers live in `concurrency_test.go`:
  - `deletionPendingFamily(t, h)`: `h.Family.Create(ctx, h.Outsider, h.CommandID(), "B")`, then `h.Refresh("outsider")`. It then makes 20 `h.Service.ServeSync` batches of `caretest.Baby`/`caretest.Feed` mutations under fresh IDs with the outsider's digests, then `h.Family.ScheduleDeletion(ctx, <outsider principal>, h.CommandID(), true)`, and returns the family ID.
  - `mustDigests(t, h, subject)`: `h.Auth.AccessDigests(h.AccessToken(subject))`.

  Every `h.Refresh` and `h.AccessToken` call happens before the goroutines start; the harness's token map is not safe for concurrent use.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env BT_TEST_PACKAGES='./internal/maintenance ./internal/authority' BT_TEST_RUN='TestPurge|TestImportStaging|TestFamilyRowDelete|TestMaintenanceAndRequests|TestFamilyPurgeWork' bash Server/scripts/test-db.sh`. Expected: `undefined: maintenance.Purger`.
- [ ] **Step 3: Implement.**
  - `FamilyPurgeWork` reads `SELECT id::text, coalesce(purge_command_id::text,'') FROM families WHERE status='purge_started' OR (status='deletion_pending' AND purge_after <= $1) ORDER BY (status <> 'purge_started'), id LIMIT $2` through `readAuthority`.
  - `Purger.Run` takes the first work item:
    - not started: `StartFamilyPurge{CommandID: start, FamilyID, At: now}`. An exact retry after a crash returns the stored result.
    - started: one maintenance transaction (`lock_timeout 2s`):
      - `SELECT id FROM families WHERE id=$1 FOR NO KEY UPDATE`;
      - then the first non-empty step's bounded delete, each `… WHERE family_id=$1 AND <key> IN (SELECT <key> … WHERE family_id=$1 LIMIT $2)`. `import_sessions` goes one session at a time, `care_entities` deletes `kind <> 1` first and then `kind = 1`, and `device_sync_cursors` uses `ORDER BY device_id FOR UPDATE` so it locks in the wake flush's order;
      - when every step is empty, `UPDATE families SET care_record_count=0, care_charged_bytes=0, care_entity_count=0, care_wire_bytes=0 WHERE id=$1`;
      - commit.
    - after the zeroing commit: `CompleteFamilyPurge{CommandID: complete, FamilyID, StartCommandID, Receipt: receipt, At: now}`.
  - The purge deletes no export job: a job expires 24 h after creation, and the purge comes at least 30 days after scheduling, so the export worker has already deleted the job's chunks; Task 10 deletes the job row.
- [ ] **Step 4: Run green:** `rtk proxy env BT_TEST_PACKAGES='./internal/maintenance ./internal/authority ./internal/family ./internal/importer ./cmd/...' bash Server/scripts/test-db.sh`.
- [ ] **Step 5: Commit:** `rtk git add -A Server && rtk git commit -m "feat(maintenance): purge a deleted family's care data in bounded, resumable steps"`

### Task 12: Lost-acknowledgement refresh recovery from the frozen child scope

**Files:**
- Modify `Server/internal/authority/sessions.go`:
  - `rotate` (:620-624) inserts `child_scope`, `child_family_id` and `child_role`;
  - `recoverOrConflict` (:666-715): a different attempt revokes the lineage whether or not the receipt restores;
  - `restore` (:730-777) derives from the frozen scope.
- Modify `Server/internal/authority/sessions_test.go`.

**Interfaces:** Consumes Task 2's `refresh_receipts` columns. Produces no new names.

- [ ] **Step 1: Write the failing test** in `sessions_test.go` (using `lifecycle_test.go`'s `newMember`, `createFamily`, `scheduleOf` and `principalOf`):
```go
func TestRecoveryRetryAfterRescopeReturnsTheFrozenChild(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	_ = createFamily(t, f, f.Service, owner)
	if _, err := f.Service.ScheduleDeletion(t.Context(), scheduleOf(t, f, owner.account, owner.principal(t, f))); err != nil {
		t.Fatal(err)
	}
	device := f.Device(t, owner.account)
	_, _, raw := f.Session(t, owner.account, device, "account", "", "")
	attempt := newAttempt(t)
	first, err := f.Service.Refresh(t.Context(), refreshOf(f, raw, attempt, binding(owner.account, device)))
	if err != nil || first.Child.Scope != "family_recovery" {
		t.Fatalf("rotation into the recovery scope: %+v %v", first.Child, err)
	}
	// The acknowledgement is lost; the owner forfeits recovery on another device, which moves the child to account scope.
	if _, err := f.Service.ForfeitRecovery(t.Context(), authority.ForfeitRecovery{CommandID: authoritytest.NewID(t), AccountID: owner.account,
		At: f.Now, Principal: principalOf(f, t, first.Child.ID)}); err != nil {
		t.Fatal(err)
	}
	if row, _ := sessionRow(t, f, first.Child.ID); row.Scope != "account" {
		t.Fatalf("child scope %s after forfeit", row.Scope)
	}
	retry, err := f.Service.Refresh(t.Context(), refreshOf(f, raw, attempt, binding(owner.account, device)))
	if err != nil || !retry.Recovered || retry.AccessToken != first.AccessToken || retry.RefreshToken != first.RefreshToken {
		t.Fatalf("the exact retry after a rescope: %+v %v (want the committed child, not service_unavailable)", retry, err)
	}
	_, err = f.Service.Refresh(t.Context(), refreshOf(f, raw, newAttempt(t), binding(owner.account, device)))
	wantCode(t, err, "refresh_attempt_conflict")
	if row, _ := sessionRow(t, f, first.Child.ID); row.Status != "revoked" {
		t.Fatalf("a different attempt left the lineage %s", row.Status)
	}
}

func TestRecoveryReceiptWrittenBeforeThirtyOneIsUnavailable(t *testing.T) {
	// a recoverable rotation, then UPDATE refresh_receipts SET child_scope=NULL, child_family_id=NULL, child_role=NULL;
	// the exact retry is refresh_receipt_unavailable (terminal for the client), never service_unavailable.
}
```
- [ ] **Step 2: Run to see the failure:** `rtk proxy env BT_TEST_PACKAGES='./internal/authority' BT_TEST_RUN='TestRecoveryRetryAfterRescope|TestRecoveryReceiptWrittenBefore' bash Server/scripts/test-db.sh`. Expected: `the exact retry after a rescope: … service_unavailable`.
- [ ] **Step 3: Implement.**
  - **`rotate`.** The recovery receipt insert adds `child_scope, child_family_id, child_role` = `child.Scope`, `NULLIF(child.FamilyID,'')` and `NULLIF(child.Role,'')`.
  - **`recoverOrConflict`.**
    - The receipt read adds `child_scope::text, child_family_id::text, child_role::text`.
    - When `op.in.AttemptID != parent.ConsumedAttemptID`, it runs `op.validateChild(child)` and then `op.revokeLineage(ctx, ErrRefreshAttemptConflict)` before any restore, as the no-receipt branch already does. A reuse is a reuse whether or not the receipt restores.
  - **`restore`.**
    - It takes the frozen `(scope, familyID, role)`; a NULL scope is `ErrRefreshReceiptUnavailable`.
    - It builds `input.Scope, input.FamilyID, input.Role` and a `frozen := child` copy with `frozen.Scope, frozen.FamilyID, frozen.Role` set from them. `frozen` is used for the derivation check and the material recomputation (`RefreshStage{Input: input, Child: frozen, …}`).
    - `validateChild` and the returned `RefreshResult.Child` keep the current row, so the client learns the child's current scope.
- [ ] **Step 4: Run green:** `rtk proxy env BT_TEST_PACKAGES='./internal/authority ./internal/auth ./internal/httpapi' bash Server/scripts/test-db.sh`; `rtk proxy env BT_TEST_PACKAGES='./tests/protocol/...' BT_TEST_TAGS=identitytest bash Server/scripts/test-db.sh` (refresh-recovery phases).
- [ ] **Step 5: Commit:** `rtk git add Server/internal/authority && rtk git commit -m "fix(authority): recover a lost-ack refresh from the scope frozen at rotation"`

### Task 13: The APNs client and its configuration

**Files:**
- Create `Server/internal/config/apns.go` and `apns_test.go`. `APNSCredentials`, `LoadAPNSCredentials` and `ReadAPNSPrivateKey` are ported unchanged from `main`'s `Server/internal/config/live_activity.go:58-140` (`git show main:Server/internal/config/live_activity.go`): the same `BT_APNS_TEAM_ID`, `BT_APNS_KEY_ID`, `BT_APNS_KEY_FILE`, `BT_APNS_BUNDLE_ID` variables and the same file rules (regular non-symlink, mode 0600, one P-256 PEM key). The branch already has their helpers `readRegularFile` (`config/auth.go:268`) and `exactOperatorSecretMode` (`config/operator.go:134`). `LoadAPNSWakes` adds `BT_APNS_WAKES_ENABLED`.
- Modify `Server/internal/config/config.go`: `Config.APNSWakes`, loaded beside `Export`.
- Create `Server/internal/apns/{doc.go,provider_token.go,client.go,provider_token_test.go,client_test.go}`. `provider_token.go` is `main`'s `Server/internal/liveactivity/provider_token.go` with `invalidate`; `client.go` is `main`'s `APNSTransport.Send` core (`apns.go:107-243`: host per environment, headers, bounded request, response classification, `Retry-After`) for the `background` push type.
- Create `Server/cmd/baby-tracker-server/apns_sink.go` (production: `apns.New`) and `apns_sink_identitytest.go` with `apns_sink_identitytest_test.go`: `main`'s identitytest recorder (`Server/cmd/baby-tracker-server/live_activity_transport_identitytest.go`) behind `GET /__identitytest/apns`, enabled by `BT_IDENTITYTEST_APNS_SINK=1`, recording method, host, path, token, headers, provider-token validity, key ID, issuer and payload. Nothing in it can reach Apple.
- Create `Server/scripts/apns_secrets_test.go`.
- Modify `Server/cmd/baby-tracker-server/main.go`: build the client when `cfg.APNSWakes.Enabled` (used by Tasks 14–15).
- Modify `Server/README.md`: a "Background push (optional)" section in the configuration reference. It lists:
  - `BT_APNS_WAKES_ENABLED` (default off; off means wakes go through the live hub only and `push-token-v1` is not advertised);
  - `BT_APNS_TEAM_ID`, `BT_APNS_KEY_ID`, `BT_APNS_BUNDLE_ID` (the app's bundle ID, which is the push topic);
  - `BT_APNS_KEY_FILE`: a mounted `.p8` file, a regular file with mode 0600, read once at startup and never logged.

  It states that the operator provides the credentials at deploy and that they are never committed; Unraid wiring is Phase 6. The section must not contain the owner's team ID, key ID or key, and `TestNoProductionAPNsKeyInTheServerTree` covers it as a tracked `Server/` file.

**Interfaces:** Produces:
```go
package config
type APNSCredentials struct{ TeamID, KeyID, KeyFile, BundleID string }
type APNSWakesConfig struct {
	Enabled     bool
	Credentials APNSCredentials
}
// LoadAPNSCredentials validates the Apple identifiers (ten upper-case
// alphanumerics), the bundle ID and the mounted key exactly as main does.
func LoadAPNSCredentials(getenv func(string) string) (APNSCredentials, error)
// LoadAPNSWakes: BT_APNS_WAKES_ENABLED "" or "false" is off (no BT_APNS_*
// variable is read, push stays optional for self-hosters); "true" requires
// LoadAPNSCredentials; anything else is an error.
func LoadAPNSWakes(getenv func(string) string) (APNSWakesConfig, error)
func ReadAPNSPrivateKey(path string) (*ecdsa.PrivateKey, error)

package apns
const (
	SandboxURL    = "https://api.sandbox.push.apple.com"
	ProductionURL = "https://api.push.apple.com"
	WakePayload   = `{"aps":{"content-available":1}}`
	requestTimeout       = 10 * time.Second // main's bound
	providerTokenRefresh = 40 * time.Minute // under Apple's 60-minute limit
	providerTokenFloor   = 20 * time.Minute // Apple refuses refreshes more often (TooManyProviderTokenUpdates)
)
type Environment string
const (
	Sandbox    Environment = "sandbox"
	Production Environment = "production"
)
type Class string // main's PushClassification values
const (
	Accepted             Class = "accepted"
	InvalidToken         Class = "invalidToken"         // 410, or 400 BadDeviceToken / Unregistered: delete this token
	Retryable            Class = "retryable"            // 429, 5xx
	ConfigurationFailure Class = "configurationFailure" // 403, other 4xx: the deployment's key, team or topic
	Ambiguous            Class = "ambiguous"            // no response
)
type Result struct {
	Status     int
	Reason     string    // one of Apple's documented reasons, else ""
	RetryAfter time.Time // zero when absent
	Class      Class
}
var ErrNotSent = errors.New("apns_request_not_sent") // nothing reached Apple (validation, signing)
type Client struct{ /* http client, provider token, topic, hosts, now */ }
// New talks to Apple's two hosts over one reused HTTP/2 connection each.
func New(creds config.APNSCredentials) (*Client, error)
// NewWithTransport is New over rt and other hosts: the local TLS HTTP/2 test
// server and the identitytest recorder.
func NewWithTransport(creds config.APNSCredentials, rt http.RoundTripper, sandboxURL, productionURL string, now func() time.Time) (*Client, error)
// Wake sends one background push to token (raw bytes, sent as lowercase hex).
func (c *Client) Wake(ctx context.Context, env Environment, token []byte) (Result, error)
func (c *Client) CloseIdleConnections()
```

- [ ] **Step 1: Write the failing tests.** `apns/client_test.go` runs a local TLS HTTP/2 server; the key is generated in the test, so no credential is ever read:
```go
type seen struct {
	proto   int
	path    string
	headers http.Header
	body    []byte
}

func fakeAPNs(t *testing.T, answer func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *[]seen, *atomic.Int32) {
	t.Helper()
	var mu sync.Mutex
	var requests []seen
	var conns atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, seen{proto: r.ProtoMajor, path: r.URL.Path, headers: r.Header.Clone(), body: body})
		mu.Unlock()
		answer(w, r)
	}))
	server.EnableHTTP2 = true
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, &requests, &conns
}

func testCredentials(t *testing.T) (config.APNSCredentials, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	path := filepath.Join(t.TempDir(), "apns.p8")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.APNSCredentials{TeamID: "TEAM123456", KeyID: "KEY1234567", KeyFile: path, BundleID: "com.example.babytracker"}, key
}

func TestWakeSendsAContentlessBackgroundPushOverOneHTTP2Connection(t *testing.T) {
	server, requests, conns := fakeAPNs(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	creds, key := testCredentials(t)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	client, err := apns.NewWithTransport(creds, server.Client().Transport, server.URL, server.URL, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	token := bytes.Repeat([]byte{0xab}, 32)
	for range 10 {
		if result, err := client.Wake(t.Context(), apns.Sandbox, token); err != nil || result.Class != apns.Accepted {
			t.Fatalf("%+v %v", result, err)
		}
	}
	if conns.Load() != 1 {
		t.Fatalf("%d connections for 10 pushes, want 1 reused", conns.Load())
	}
	first := (*requests)[0]
	if first.proto != 2 || first.path != "/3/device/"+hex.EncodeToString(token) || string(first.body) != apns.WakePayload {
		t.Fatalf("request %d %s %s", first.proto, first.path, first.body)
	}
	for name, want := range map[string]string{"apns-push-type": "background", "apns-priority": "5", "apns-topic": "com.example.babytracker",
		"apns-expiration": strconv.FormatInt(now.Add(time.Hour).Unix(), 10), "content-type": "application/json"} {
		if got := first.headers.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if !protocol.ValidUUID(strings.ToLower(first.headers.Get("apns-id"))) {
		t.Fatalf("apns-id %q", first.headers.Get("apns-id"))
	}
	// The provider token: ES256, kid = key ID, iss = team, iat = now, verified with the public key; one token for all ten.
	jwt := strings.TrimPrefix(first.headers.Get("authorization"), "bearer ")
	parts := strings.Split(jwt, ".")
	header, _ := base64.RawURLEncoding.DecodeString(parts[0])
	claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if string(header) != `{"alg":"ES256","kid":"KEY1234567"}` || string(claims) != fmt.Sprintf(`{"iat":%d,"iss":"TEAM123456"}`, now.Unix()) ||
		len(signature) != 64 || !ecdsa.Verify(&key.PublicKey, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		t.Fatalf("provider token %s / %s", header, claims)
	}
	for _, r := range *requests {
		if r.headers.Get("authorization") != first.headers.Get("authorization") {
			t.Fatal("the provider token was re-signed within 40 minutes")
		}
	}
}

func TestWakeClassifiesApplesAnswers(t *testing.T) {
	for _, c := range []struct {
		status     int
		reason     string
		retryAfter string
		class      apns.Class
	}{
		{200, "", "", apns.Accepted},
		{410, "Unregistered", "", apns.InvalidToken},
		{400, "BadDeviceToken", "", apns.InvalidToken},
		{400, "DeviceTokenNotForTopic", "", apns.ConfigurationFailure},
		{400, "BadPriority", "", apns.ConfigurationFailure},
		{403, "ExpiredProviderToken", "", apns.ConfigurationFailure},
		{403, "InvalidProviderToken", "", apns.ConfigurationFailure},
		{429, "TooManyRequests", "30", apns.Retryable},
		{500, "InternalServerError", "", apns.Retryable},
		{503, "ServiceUnavailable", "", apns.Retryable},
	} {
		server, _, _ := fakeAPNs(t, func(w http.ResponseWriter, _ *http.Request) {
			if c.retryAfter != "" {
				w.Header().Set("Retry-After", c.retryAfter)
			}
			w.WriteHeader(c.status)
			if c.reason != "" {
				fmt.Fprintf(w, `{"reason":%q}`, c.reason)
			}
		})
		creds, _ := testCredentials(t)
		now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
		client, _ := apns.NewWithTransport(creds, server.Client().Transport, server.URL, server.URL, func() time.Time { return now })
		result, err := client.Wake(t.Context(), apns.Production, bytes.Repeat([]byte{1}, 32))
		if err != nil || result.Class != c.class || result.Status != c.status || result.Reason != c.reason {
			t.Fatalf("%d %s: %+v %v", c.status, c.reason, result, err)
		}
		if c.retryAfter != "" && !result.RetryAfter.Equal(now.Add(30*time.Second)) {
			t.Fatalf("Retry-After %v", result.RetryAfter)
		}
	}
}

func TestWakeRefusesBeforeSendingAndReportsALostAnswerAsAmbiguous(t *testing.T) {
	// a 15-byte or 257-byte token, and environment "staging": ErrNotSent with no request on the server;
	// a server that closes the connection mid-response (Hijack and Close): Class Ambiguous, no error text containing the token.
}
```
  `apns/provider_token_test.go`, `TestProviderTokenRefreshesBeforeAnHourAndNotTwiceInTwentyMinutes`:
  - with an injected clock, the token is reused at 39 minutes and re-signed at 41 minutes;
  - `invalidate()` at 10 minutes after signing keeps the token (Apple's 20-minute floor), and at 21 minutes forces a new one.

  `config/apns_test.go`, `TestAPNSWakesAreOptionalAndStrict`:
  - with no variables, or `BT_APNS_WAKES_ENABLED=false`, wakes are off and no file is read;
  - `true` with a 0600 P-256 key in a temp dir loads;
  - `true` with a 0644 key, a symlinked key, an RSA key, a team ID of 9 characters, or a missing bundle ID each refuse with an error naming the variable, never its value;
  - `maybe` refuses.

  `cmd/baby-tracker-server/apns_sink_identitytest_test.go`, `TestIdentityTestAPNSRecorderKeepsTheProductionClient`: through the recorder, a `Wake` is recorded with path, `apns-push-type: background`, a provider token that verifies against the configured key, and the payload; `GET /__identitytest/apns` returns it as JSON `{"requests":[…]}`.

  `scripts/apns_secrets_test.go`, `TestNoProductionAPNsKeyInTheServerTree`: runs `git ls-files Server` and fails on any tracked file that:
  - has a name starting with `AuthKey_` or ending in `.p8`;
  - contains a PEM private-key header (`-----BEGIN PRIVATE KEY-----` or `-----BEGIN EC PRIVATE KEY-----`; no tracked `Server/` file has one today, and the tests generate their keys in `t.TempDir()`);
  - contains the production key ID.

  The test builds the key ID from two halves (`"Z894" + "KG4587"`), so the test file itself does not match. Only the plans and the owner's credentials folder may hold that value.
- [ ] **Step 2: Run to see the failure:** `rtk go -C Server test ./internal/apns ./internal/config ./scripts -race -count=1 -run 'TestWake|TestProviderToken|TestAPNSWakes|TestNoProductionAPNsKey'` and `rtk go -C Server test -tags identitytest ./cmd/baby-tracker-server -run TestIdentityTestAPNSRecorder`. Expected: `package babytracker/server/internal/apns is not in std` and `undefined: config.LoadAPNSWakes`.
- [ ] **Step 3: Implement.**
  - **Port, do not rewrite.** Copy `main`'s `provider_token.go` (`git show main:Server/internal/liveactivity/provider_token.go`) into `package apns`, renaming nothing but the package, and add `invalidate()`, which clears the cache only when `now − issuedAt ≥ providerTokenFloor`.
  - **`Wake`** follows `main`'s `APNSTransport.Send` step for step:
    - validate first (environment, token 16–256 bytes), else `ErrNotSent`;
    - sign the provider token, else `ErrNotSent`;
    - pick the host per environment and bound the request at `requestTimeout`;
    - set the headers of the APNs ruling (`apns-push-type: background`, `apns-priority: 5`, `apns-topic: creds.BundleID`, `apns-expiration: now+3600`, a random version-4 `apns-id`, `content-type: application/json`), and send `WakePayload`;
    - read at most 1 KiB of the error body and keep `reason` only if it is in `main`'s documented-reason list;
    - classify as `main` does (`classifyStatus` plus the `BadDeviceToken`/`Unregistered` and topic overrides);
    - on `403 ExpiredProviderToken`/`InvalidProviderToken`, call `invalidate()`.
  - **`New`** uses `main`'s HTTP client settings: `&http.Transport{ForceAttemptHTTP2: true, MaxIdleConns: 32, MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 5 * time.Second}`, and `CheckRedirect` returns `http.ErrUseLastResponse`.
  - **No logging inside `apns`.** Callers log the class, the status and the reason only.
  - **`cmd`.** `newWakeAPNSClient(cfg config.APNSWakesConfig) (*apns.Client, error)` returns `apns.New` in production builds. Identitytest builds with `BT_IDENTITYTEST_APNS_SINK=1` return `apns.NewWithTransport` over the recorder, with the unresolvable hosts `https://sandbox.apns.identitytest.invalid` and `https://production.apns.identitytest.invalid` (main's), and register `GET /__identitytest/apns` on the identitytest handler wrapper (`wrapIdentityTestHandler`).
- [ ] **Step 4: Run green:** the Step 2 commands; then `rtk go -C Server vet ./... && rtk go -C Server vet -tags identitytest ./...`; `rtk proxy env BT_TEST_PACKAGES='./internal/config ./cmd/...' bash Server/scripts/test-db.sh`.
- [ ] **Step 5: Commit:** `rtk git add Server/internal/apns Server/internal/config Server/cmd/baby-tracker-server Server/scripts/apns_secrets_test.go Server/README.md && rtk git commit -m "feat(apns): port main's APNs provider token and transport for background wakes"`

### Task 14: The device push-token route

**Files:**
- Create `Server/internal/protocol/push_token.go` and `push_token_test.go`; modify `Server/internal/protocol/auth.go` (`CapabilityPushTokenV1` in `knownCapabilities`) and `sync.go` (`SurfacePushToken`, whose route capabilities are exactly `push-token-v1`).
- Create `Server/internal/pushtoken/service.go` and `service_test.go`.
- Create `Server/internal/httpapi/push_token.go` and `push_token_test.go`; modify `Server/internal/httpapi/router.go`:
  - `Options.PushTokens`, `Options.PushTokenDevicePolicy` and `Options.PushTokenAccountPolicy`;
  - the two routes, registered only when `PushTokens != nil`;
  - `Routes()`.
- Modify `Server/internal/limits/policy.go`: the two scopes. `config.LoadLimits` takes them from `limits.DefaultPolicies()` (`config/limits.go:24`).
- Modify `Server/cmd/baby-tracker-server/main.go`: compose the service when `cfg.APNSWakes.Enabled`, otherwise drop `push-token-v1` from the advertised capabilities, as `export-v1` is dropped (`main.go:328-331`).
- Modify `Server/internal/httpapi/request_roundtrip_test.go`: the route's round trips.
- Modify `docs/protocol/v1.md`: a new section "Device push tokens (`push-token-v1`)".

**Interfaces:** Consumes Task 4's `authority.Request`/`RequestGuard`, Task 1's limiter and Task 2's `device_push_tokens`. Produces:
```go
package protocol
const CapabilityPushTokenV1 = "push-token-v1"
const SurfacePushToken Surface = "push-token"
type PushTokenRequest struct {
	RequestMeta
	Token       string `json:"token"`       // lowercase hex, even length, 16–256 decoded bytes
	Environment string `json:"environment"` // "sandbox" | "production"
}
var ErrInvalidPushToken = errors.New("invalid_push_token")
// Decode validates r and returns the raw token: ErrInvalidPushToken for the token or the environment.
func (r PushTokenRequest) Decode() ([]byte, error)

package pushtoken
type Service struct{ /* guard authority.RequestGuard; now func() time.Time */ }
func New(guard authority.RequestGuard, now func() time.Time) *Service
// Register stores token for the authenticated session's own device (one
// guarded transaction: BEGIN+authority, the upsert, COMMIT).
func (s *Service) Register(ctx context.Context, req authority.Request, environment string, token []byte) error
// Unregister deletes the session's own device's token (idempotent).
func (s *Service) Unregister(ctx context.Context, req authority.Request) error

package httpapi
type PushTokenService interface {
	Register(context.Context, authority.Request, string, []byte) error
	Unregister(context.Context, authority.Request) error
}

package limits
const (
	ScopePushTokenDevice  = "push-token-device"  // default {Count: 10, Window: time.Hour}
	ScopePushTokenAccount = "push-token-account" // default {Count: 30, Window: time.Hour}
)
```
Wire contract (v1.md, additive):
- `PUT /v1/devices/push-token`
  - Headers: `X-Baby-Tracker-Protocol: v1` and `X-Baby-Tracker-Capabilities: push-token-v1`.
  - Body, at most 4 KiB: `{"protocol_version":"v1","capabilities":["push-token-v1"],"token":"<hex>","environment":"sandbox"}`.
  - Answers `204` on success. Replacing a token, or sending the same one again, is also `204`.
- `DELETE /v1/devices/push-token`: the same headers and no body; `204`, idempotent.
- The device is always the authenticated session's device; no device ID appears on the wire.
- Refusals:
  - `400 invalid_push_token`;
  - `400 invalid_request`;
  - `409 unsupported_capability`;
  - the authentication and family codes of `/v1/sync`;
  - `429 rate_limited` at 10 a device or 30 an account per hour.
- On a server without push, both routes are `404` and `/v1/server` does not list `push-token-v1`.
- The token is never echoed or logged.

- [ ] **Step 1: Write the failing tests.** `pushtoken/service_test.go`:
```go
func TestRegisterBindsTheTokenToTheSessionsOwnDevice(t *testing.T) {
	h := testkit.New(t)
	service := pushtoken.New(h.Authority, func() time.Time { return h.Now })
	owner := mustDigests(t, h, "owner")
	first, second := bytes.Repeat([]byte{0x01}, 32), bytes.Repeat([]byte{0x02}, 32)
	if err := service.Register(t.Context(), authority.Request{Digests: owner}, "sandbox", first); err != nil {
		t.Fatal(err)
	}
	if err := service.Register(t.Context(), authority.Request{Digests: owner}, "production", second); err != nil {
		t.Fatal(err) // a replacement
	}
	if got := storedToken(t, h, h.Owner.DeviceID); !bytes.Equal(got.token, second) || got.environment != "production" || got.account != h.Owner.AccountID {
		t.Fatalf("stored %+v", got)
	}
	if err := service.Unregister(t.Context(), authority.Request{Digests: owner}); err != nil {
		t.Fatal(err)
	}
	if err := service.Unregister(t.Context(), authority.Request{Digests: owner}); err != nil {
		t.Fatalf("a second unregister is not idempotent: %v", err)
	}
	viewer := h.AddMember(t, "viewer", "viewer")
	if err := service.Register(t.Context(), authority.Request{Digests: mustDigests(t, h, "viewer")}, "sandbox", first); err != nil {
		t.Fatalf("a viewer's phone needs wakes too: %v", err)
	}
	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), viewer.AccountID); err != nil {
		t.Fatal(err)
	}
	if err := service.Register(t.Context(), authority.Request{Digests: mustDigests(t, h, "viewer")}, "sandbox", first); family.Code(err) != "session_revoked" {
		t.Fatalf("a removed member registered: %v", err)
	}
}
```
  `storedToken` reads `device_push_tokens` through `h.Maintenance` (`token`, `device_id`) plus `environment` and `account_id` through an authority transaction on `h.Runtime`. `mustDigests` is `h.Auth.AccessDigests(h.AccessToken(subject))`.

  `protocol/push_token_test.go`, `TestPushTokenRequestDecode`, as a table:
  - accepted: 32 bytes of lowercase hex; 16 bytes; 256 bytes; both environments;
  - `invalid_push_token`: upper-case hex, an odd length, 15 bytes, 257 bytes, non-hex, an empty token, environment `staging`.

  `httpapi/push_token_test.go`, `TestPushTokenRouteContract`, over `newStack` (Task 5) with `PushTokens: pushtoken.New(guard, clock)`:
  - `PUT` gives 204 with an empty body;
  - `DELETE` gives 204;
  - a 5 KiB body gives 413 `body_too_large`;
  - a capabilities header of `records-v1` gives 409 `unsupported_capability`;
  - a malformed token gives 400 `invalid_push_token`;
  - a revoked token gives 401;
  - the 11th `PUT` in an hour from one device gives 429 with `Retry-After`;
  - with `PushTokens: nil`, `PUT` gives 404 and the capability set of `/v1/server` has no `push-token-v1`;
  - the response body and the log output never contain the token's hex.

  `request_roundtrip_test.go` adds `TestPushTokenRouteRoundTrips`: after one warm `PUT`, a `PUT` costs exactly 3 round trips and a `DELETE` 3; a refused `PUT` (invalid token after authentication) costs 2.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env BT_TEST_PACKAGES='./internal/pushtoken ./internal/protocol ./internal/httpapi' BT_TEST_RUN='TestRegister|TestPushToken' bash Server/scripts/test-db.sh`. Expected: `package babytracker/server/internal/pushtoken is not in std`.
- [ ] **Step 3: Implement.**
  - `Register` runs `WithRequestTx(ctx, req, "read", run)`; every role may register its own phone. Inside `run`:
```sql
INSERT INTO device_push_tokens(device_id, account_id, environment, token, registered_at)
VALUES ($1, $2, $3::text::app.apns_environment, $4, $5)
ON CONFLICT (device_id) DO UPDATE SET environment = EXCLUDED.environment, token = EXCLUDED.token, registered_at = EXCLUDED.registered_at
WHERE device_push_tokens.account_id = EXCLUDED.account_id
```
    with `$1 = p.DeviceID`, `$2 = p.AccountID`. `Unregister` runs `DELETE FROM device_push_tokens WHERE device_id = $1 AND account_id = $2`.
  - The handler follows `syncHandler`'s shape (Task 5):
    1. readiness, bearer token and `AccessDigests` before any database work;
    2. headers and the body (`boundedBody` at 4096 bytes, `protocol.DecodeRequest`, `ValidateRouteCapabilities(SurfacePushToken, …)`, `Decode`), each refusal held;
    3. `Admit` refuses a non-family scope with `family_scope_required`, then the held refusal;
    4. `Charge` takes `ScopePushTokenDevice` (key `p.DeviceID`) and `ScopePushTokenAccount` (key `p.AccountID`) from the in-memory limiter;
    5. errors go through `writeRequestError`; success writes `204`.
- [ ] **Step 4: Run green:** the Step 2 command; `rtk go -C Server test ./internal/limits ./internal/protocol -race -count=1`; `rtk proxy env BT_TEST_PACKAGES='./internal/httpapi ./cmd/...' bash Server/scripts/test-db.sh`.
- [ ] **Step 5: Commit:** `rtk git add -A Server/internal/protocol Server/internal/pushtoken Server/internal/httpapi Server/internal/limits Server/cmd docs/protocol/v1.md && rtk git commit -m "feat(httpapi): register a device's APNs token through push-token-v1"`

### Task 15: Wake worker delivery through APNs

**Files:**
- Create `Server/internal/wake/push.go` and `push_test.go`; modify `Server/internal/wake/worker.go` (the lag scan returns the push budget, the split by live connection, the reservation, the sends, the backoff).
- Modify `Server/internal/realtime/hub.go`: add `DeviceConnected`; covered in `hub_test.go`.
- Create `Server/internal/maintenance/push_tokens.go` and `push_tokens_test.go`: the hourly token sweep.
- Modify `Server/cmd/baby-tracker-server/main.go`: `Worker.Pusher` is set when the APNs client exists, `Worker.Connected` is `liveHub.DeviceConnected`, and `PushTokenSweep` joins the hourly loop with `AuditPartitionSweep`.
- Modify `docs/protocol/v1.md`: the wake paragraph, which now says APNs background pushes reach registered devices without a live connection, at most one every 20 minutes.

**Interfaces:** Consumes Task 8's `wake.Worker`, Task 13's `apns.Client`, and Task 2's `app.push_targets`, `app.prune_push_tokens` and `device_sync_cursors.pushed_*`. Produces:
```go
package wake
// Pusher is *apns.Client.
type Pusher interface {
	Wake(ctx context.Context, env apns.Environment, token []byte) (apns.Result, error)
}
const (
	APNsInterval     = 20 * time.Minute // at most 3 background pushes an hour per device (APNs ruling)
	MaxPushesPerTick = 100
	pushConcurrency  = 8
)
// Worker gains:
//   Pusher    Pusher                    // nil: hub wakes only (a self-hoster without APNs)
//   Connected func(deviceID string) bool // the hub: a device with a live connection is woken through it, never pushed
// Pushed counts sends made (each after its reservation committed); PushFailed counts those Apple did not accept.
type Stats struct{ Flushed, Woken, Pushed, PushFailed int }

package realtime
func (hub *Hub) DeviceConnected(deviceID string) bool // a bound lease or a reservation for the device

package maintenance
const PushTokenBatch = 1000
type PushTokenSweep struct{ Pool *pgxpool.Pool } // SELECT app.prune_push_tokens(PushTokenBatch)
```

- [ ] **Step 1: Write the failing tests** `wake/push_test.go`:
```go
type fakePusher struct {
	mu      sync.Mutex
	sent    map[string]int // hex token -> pushes
	perTick []int
	answer  func(token []byte) apns.Result
}

func (p *fakePusher) Wake(_ context.Context, _ apns.Environment, token []byte) (apns.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent[hex.EncodeToString(token)]++
	if p.answer != nil {
		return p.answer(token), nil
	}
	return apns.Result{Status: 200, Class: apns.Accepted}, nil
}

func TestWakeWorkerPushBudgetNeverStorms(t *testing.T) { // Review Focus 5 (APNs)
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	// Four phones of one family: owner and caregiver hold live connections; two
	// more caregivers are backgrounded with registered tokens.
	sleeper1, sleeper2 := h.AddMember(t, "sleeper1", "caregiver"), h.AddMember(t, "sleeper2", "caregiver")
	tokens := map[string][]byte{sleeper1.DeviceID: bytes.Repeat([]byte{0x51}, 32), sleeper2.DeviceID: bytes.Repeat([]byte{0x52}, 32)}
	registerToken(t, h, "sleeper1", tokens[sleeper1.DeviceID])
	registerToken(t, h, "sleeper2", tokens[sleeper2.DeviceID])
	connected := map[string]bool{h.Owner.DeviceID: true, h.Caregiver.DeviceID: true}
	tracker, pusher := wake.NewTracker(0), &fakePusher{sent: map[string]int{}}
	clock := h.Now
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Pusher: pusher,
		Connected: func(device string) bool { return connected[device] }, Now: func() time.Time { return clock }}
	seq := h.FamilySequence(t)
	for _, p := range []access.Principal{h.Owner, h.Caregiver, sleeper1, sleeper2} {
		tracker.Synced(h.FamilyID, p.DeviceID, seq, seq, clock)
	}
	for i := range 120 { // a write every 30 s for an hour
		caretest.Seed(t, h, caretest.Feed(caretest.BabyA, i))
		tracker.Changed(h.FamilyID, h.FamilySequence(t))
		stats, err := w.Tick(t.Context())
		if err != nil || stats.Pushed > wake.MaxPushesPerTick {
			t.Fatalf("tick %d: %+v %v", i, stats, err)
		}
		clock = clock.Add(30 * time.Second)
	}
	for device, token := range tokens {
		if got := pusher.sent[hex.EncodeToString(token)]; got != 3 {
			t.Fatalf("backgrounded device %s pushed %d times in an hour, want 3 (one per 20 minutes)", device, got)
		}
	}
	if len(pusher.sent) != 2 {
		t.Fatalf("a connected device was pushed: %v", pusher.sent)
	}
	// A device whose member was removed is never pushed again; the other one is, 20 minutes on.
	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), sleeper2.AccountID); err != nil {
		t.Fatal(err)
	}
	removedBefore := pusher.sent[hex.EncodeToString(tokens[sleeper2.DeviceID])]
	clock = clock.Add(wake.APNsInterval)
	caretest.Seed(t, h, caretest.Feed(caretest.BabyA, 5000))
	tracker.Changed(h.FamilyID, h.FamilySequence(t))
	if _, err := w.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := pusher.sent[hex.EncodeToString(tokens[sleeper2.DeviceID])]; got != removedBefore {
		t.Fatal("a removed member's device was pushed")
	}
	if got := pusher.sent[hex.EncodeToString(tokens[sleeper1.DeviceID])]; got != 4 {
		t.Fatalf("the remaining backgrounded device: %d pushes, want 4", got)
	}
}

func TestPushBacksOff(t *testing.T) { // Review Focus 5: a 429 stops every send until Retry-After
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, pusher := wake.NewTracker(0), &fakePusher{sent: map[string]int{}}
	clock := h.Now
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Pusher: pusher,
		Connected: func(string) bool { return false }, Now: func() time.Time { return clock }}
	// Each call makes one new backgrounded device due, so only the pause can hold a send back.
	makeDue := func(n int) {
		subject := fmt.Sprintf("background%d", n)
		p := h.AddMember(t, subject, "caregiver")
		registerToken(t, h, subject, bytes.Repeat([]byte{byte(0x60 + n)}, 32))
		seq := h.FamilySequence(t)
		tracker.Synced(h.FamilyID, p.DeviceID, seq, seq, clock)
		caretest.Seed(t, h, caretest.Feed(caretest.BabyA, 7000+n))
		tracker.Changed(h.FamilyID, h.FamilySequence(t))
	}
	retryUntil := clock.Add(10 * time.Minute)
	pusher.answer = func([]byte) apns.Result {
		return apns.Result{Status: 429, Reason: "TooManyRequests", Class: apns.Retryable, RetryAfter: retryUntil}
	}
	makeDue(1)
	if stats, err := w.Tick(t.Context()); err != nil || stats.Pushed != 1 || stats.PushFailed != 1 {
		t.Fatalf("the first push: %+v %v", stats, err)
	}
	pusher.answer = nil
	clock = clock.Add(time.Minute)
	makeDue(2)
	if stats, err := w.Tick(t.Context()); err != nil || stats.Pushed != 0 {
		t.Fatalf("a push went out during the 429 pause: %+v %v", stats, err)
	}
	clock = retryUntil.Add(time.Second)
	tracker.Changed(h.FamilyID, h.FamilySequence(t))
	if stats, err := w.Tick(t.Context()); err != nil || stats.Pushed != 1 {
		t.Fatalf("sends did not resume after Retry-After: %+v %v", stats, err)
	}
}
```
  `registerToken` calls `pushtoken.New(h.Authority, clock).Register` with the subject's digests. `discardSender` is the hub sender stub of `maintenance/concurrency_test.go`, copied into the wake tests.

  The rest of `wake/push_test.go`:
  - `TestPushServerErrorsBackOff`, with `TestPushBacksOff`'s `makeDue`/`Tick` steps and other answers:
    - three `503`s in a row pause 2 s, 4 s and 8 s of the injected clock: a tick 1 s inside each pause sends nothing, and a tick just after it sends one;
    - a `200` resets the pause to none;
    - the client's lost answer (`Ambiguous`) pauses like a `503`;
    - `403 ExpiredProviderToken` pauses 60 s;
    - `400 BadTopic` pauses 5 minutes and logs `apns configuration refused` exactly once (a `slog` handler counting records).
  - `TestPushOnlyEligibleDevices`:
    - a device with a token but no `device_sync_cursors` row gets no push;
    - a device of another family in the same tick gets nothing from this family's change;
    - the environment sent equals the registered one.
  - `TestInvalidTokenIsDeletedOnlyIfUnchanged`:
    - the pusher answers `InvalidToken` and the row is deleted;
    - a token replaced (a new `PUT`) between the send and the answer survives, because the delete matches `device_id` and the old token bytes.
  - `TestPushReservationSurvivesACrash`: a `Pusher` that blocks forever; cancelling the tick after the commit leaves `pushed_at` set, and a new worker does not push that device again within 20 minutes.
  - `TestWakeTickWithPushIsAtMostFiveRoundTrips`: on a counted maintenance pool, a tick with push candidates costs at most 5 round trips (BEGIN, flush+scan, `app.push_targets`, mark, COMMIT) and one without costs at most 4.
  - `TestWorkerPushesThroughTheRealClient`: `apns.NewWithTransport` over Task 13's local HTTP/2 server; 20 lagging backgrounded devices give 20 requests on 1 connection, with the background headers and `apns.WakePayload`.

  `realtime/hub_test.go`, `TestDeviceConnected`: false before a bind; true while a lease for the device is bound or reserved; false after its connection closes and after a revocation closes it.

  `maintenance/push_tokens_test.go`, `TestPushTokenSweepDeletesTokensWithoutAFamilySession`:
  - tokens for a logged-out device, a removed member's device and a purged family's devices are deleted;
  - a live member's token survives;
  - `PushTokenBatch` bounds a pass.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env BT_TEST_PACKAGES='./internal/wake ./internal/maintenance ./internal/realtime' BT_TEST_RUN='TestWakeWorkerPushBudget|TestPush|TestInvalidToken|TestWakeTickWithPush|TestWorkerPushes|TestPushTokenSweep|TestDeviceConnected' bash Server/scripts/test-db.sh`. Expected: `unknown field Pusher in struct literal of type wake.Worker`.
- [ ] **Step 3: Implement.**
  - **The lag scan** (Task 8) also returns `d.pushed_sequence` and `d.pushed_at`.
  - **The split.** After the scan, each lagging device is:
    - **connected** (`Connected(device)`): woken through the hub, as before;
    - **a push candidate**: not connected, `Pusher != nil`, sends not paused, `next_sequence > pushed_sequence` and `pushed_at IS NULL OR pushed_at <= now − APNsInterval`; at most `MaxPushesPerTick` of them;
    - otherwise only marked woken.
  - **The tick** runs in the same maintenance transaction:
```sql
SELECT family_id::text, device_id::text, environment::text, token FROM app.push_targets($1::uuid[], $2::uuid[]);
UPDATE device_sync_cursors d SET pushed_sequence = v.seq, pushed_at = $1
FROM unnest($2::uuid[], $3::uuid[], $4::bigint[]) AS v(f, dv, seq) WHERE d.family_id = v.f AND d.device_id = v.dv;
```
    The second statement is batched with Task 8's mark. Then COMMIT, then the hub wakes, then the sends: 8 at a time, each under the client's 10 s bound.
  - **The answers:**
    - `InvalidToken` runs `DELETE FROM device_push_tokens WHERE device_id = $1 AND token = $2` in its own short maintenance transaction;
    - `Retryable` with `RetryAfter` pauses until `max(RetryAfter, now+60s)`;
    - `Retryable` without it, and `Ambiguous`, double the pause from 2 s up to 2 minutes; `Accepted` resets it;
    - `ConfigurationFailure` with `403 ExpiredProviderToken`/`InvalidProviderToken` pauses 60 s (the client has already invalidated its provider token, so the next send re-signs, at most every 20 minutes);
    - any other `ConfigurationFailure` pauses 5 minutes and logs `slog.Error("apns configuration refused", "status", r.Status, "reason", r.Reason)` once a pause.

    A pause is worker state, so a restart clears it, and Apple's answer to the next send restores it.
  - **`DeviceConnected`** reads `hub.deviceCount[deviceID] > 0` under `hub.mu`.
- [ ] **Step 4: Run green:** the Step 2 command; `rtk proxy env BT_TEST_PACKAGES='./internal/wake ./internal/maintenance ./internal/realtime ./cmd/...' bash Server/scripts/test-db.sh`.
- [ ] **Step 5: Commit:** `rtk git add -A Server docs/protocol/v1.md && rtk git commit -m "feat(wake): push lagging devices without a live connection through APNs, at most every 20 minutes"`

### Task 16: iOS — one pull after a mid-sync snapshot

**Files:**
- Modify `Packages/BabyTracker/Sources/BabySync/SyncCoordinator.swift:255-256`.
- Modify `Packages/BabyTracker/Tests/BabySyncTests/SyncCoordinatorTests.swift`.
- Modify `Makefile`: `EXPECTED_TESTS := 1253` (1251 plus the two tests below).

**Interfaces:** None new. The behaviour: after a successful `takeSnapshot()` inside `handle(.takeSnapshot)`, run exactly one `drainPages()`. A failure of that pull is left to the next attempt, which handles it; it is never handled recursively.

- [ ] **Step 1: Write the failing test** in `SyncCoordinatorTests.swift`:
```swift
    /// A snapshot pages current rows and is not frozen: a write that lands
    /// while it pages can be half visible. One pull from the snapshot's high
    /// water, straight after it, settles the device without waiting for the
    /// next poll.
    @Test func snapshotIsFollowedByOnePullFromItsHighWater() async throws {
        let (store, transport, coordinator) = try make()
        transport.failWith = .snapshotRequired
        transport.snapshotPages = [SyncFixtures.snapshot()] // high water 5, epoch 2
        transport.pages = [SyncFixtures.page(highWater: SyncFixtures.cursor(7, epoch: 2))]

        await coordinator.syncOnce(reason: .foreground)

        #expect(transport.snapshotRequests == 1)
        #expect(transport.requests.count == 2)
        #expect(transport.requests[1].cursor == SyncFixtures.cursor(5, epoch: 2))
        #expect(transport.requests[1].pageToken == nil)
        #expect(try store.syncCursor() == SyncFixtures.cursor(7, epoch: 2))
        #expect(coordinator.currentIssue == nil)
    }

    /// The pull after a snapshot is one attempt: a refusal is not answered
    /// with another snapshot from inside the first.
    @Test func aRefusedPullAfterASnapshotDoesNotRecurse() async throws {
        let inner = FakeSyncTransport()
        inner.snapshotPages = [SyncFixtures.snapshot()]
        let refusing = RefusingEverySync(inner: inner, code: .snapshotRequired)
        let coordinator = SyncCoordinator(store: try SyncFixtures.store(), transport: refusing, accountKey: SyncFixtures.accountKey)

        await coordinator.syncOnce(reason: .foreground)

        #expect(inner.snapshotRequests == 1)
        #expect(refusing.syncCalls == 2) // the attempt, then the one pull after the snapshot
    }
```
  The test file adds:
```swift
@MainActor
final class RefusingEverySync: FamilySyncTransport, @unchecked Sendable {
    let inner: FakeSyncTransport
    let code: SyncErrorCode
    private(set) var syncCalls = 0

    init(inner: FakeSyncTransport, code: SyncErrorCode) {
        self.inner = inner
        self.code = code
    }

    func sync(_ request: SyncRequest) async throws -> SyncResponse {
        syncCalls += 1
        throw code
    }

    func snapshot(pageToken: String?) async throws -> SnapshotResponse { try await inner.snapshot(pageToken: pageToken) }
}
```
- [ ] **Step 2: Run to see the failure:** first `pgrep -fl xcodebuild` must print nothing; then `rtk proxy swift test --package-path Packages/BabyTracker --filter SyncCoordinatorTests`. Expected: `snapshotIsFollowedByOnePullFromItsHighWater` fails with `transport.requests.count → 1`.
- [ ] **Step 3: Implement:**
```swift
        case .takeSnapshot:
            // A snapshot pages current rows and is not frozen (server Phase 2
            // snapshot ruling): a write committed while it paged can be half
            // visible — a stopped timer gone, its record not yet. One pull
            // from the snapshot's high water settles the device now rather
            // than at the next poll. A refusal of that pull is the next
            // attempt's to handle.
            do {
                try await takeSnapshot()
                try await drainPages()
            } catch {}
```
- [ ] **Step 4: Run green:** `rtk proxy swift test --package-path Packages/BabyTracker --filter BabySyncTests`. The whole sync suite must pass; any `FamilySyncEngineTests` case that counts requests after a snapshot asserts the added pull (one request more, cursor at the snapshot high water). Then `rtk make test` reports exactly 1253. No `xcodebuild` is needed here (Task 17 has the simulator build).
- [ ] **Step 5: Commit:** `rtk git add Packages/BabyTracker/Sources/BabySync/SyncCoordinator.swift Packages/BabyTracker/Tests/BabySyncTests Makefile && rtk git commit -m "fix(sync): pull once from a snapshot's high water right after taking it"`

### Task 17: iOS — register for background pushes, upload the token, answer a push with one pull

**Files:**
- Create `Packages/BabyTracker/Sources/BabyProtocol/PushTokenDTOs.swift`.
- Create `Packages/BabyTracker/Sources/BabySync/PushEnvironmentResolver.swift`: `main`'s `ProvisioningProfileEnvironmentResolver` (`git show main:Packages/BabyTracker/Sources/BabySync/LiveActivityAPNsEnvironment.swift`), ported unchanged except that it returns `PushEnvironment`.
- Create `Packages/BabyTracker/Sources/BabySync/PushTokenRegistrar.swift` and `BackgroundWakeHandler.swift`.
- Modify `Packages/BabyTracker/Sources/BabySync/FamilySyncHTTPClient.swift`: conform to `PushTokenTransport` through the same `send` path; a `204` answer has no body to decode.
- Modify `Packages/BabyTracker/Sources/SettingsFeature/FamilySyncComposition.swift`:
  - `Graph.push` and `Graph.wake`, and two `assemble` parameters:
    - `registerForRemoteNotifications: @MainActor () -> Void = {}` (the app passes `UIApplication.shared.registerForRemoteNotifications`; the live-server gate and tests keep the no-op);
    - `pushEnvironment: any PushEnvironmentResolving = PushEnvironmentResolver()`;
  - the registrar's `capabilities` closure reads `identity.serverInfo(endpoint.origin).capabilities`, as `LiveHintChannel` does;
  - `assemble` hands the model a `PushLifecycle`. Its `activated` closure calls `registerForRemoteNotifications()`, then `push.sessionActivated(PushSession(key), transport:activation:)`. The transport is a `FamilySyncHTTPClient` built for that family key exactly as `makeEngine` builds its sync transport (same endpoint, `KeychainTokenProvider`, pinned session and `retryCredentials`). Its `ending` closure calls `push.sessionEnding()`.
- Modify `Packages/BabyTracker/Sources/SettingsFeature/FamilySettingsViewModel.swift`:
  - `var pushLifecycle: PushLifecycle?`, plus `private var nextPushActivation: PushActivation = .restored`;
  - sign-in, create, join and re-authentication set `nextPushActivation = .signedIn` before they activate;
  - after a successful `activate()` (`FamilySettingsViewModel.swift:633`), call `pushLifecycle?.activated(familyKey, endpoint, nextPushActivation)` and reset it to `.restored`, so a launch or a `withSyncStopped` restart only compares fingerprints;
  - sign-out, `leaveFamily()` and a removal call `pushLifecycle?.ending()` before `stopEngine()`; `withSyncStopped`'s temporary stop does not.
- Create `Apps/BabyTrackerApp/PushAppDelegate.swift`; modify `Apps/BabyTrackerApp/BabyTrackerApp.swift` (`@UIApplicationDelegateAdaptor(PushAppDelegate.self)`) and `Apps/BabyTrackerApp/AppEnvironment.swift` (hands the graph's `push` and `wake` to the delegate once the store opens).
- Modify `Apps/BabyTracker-Info.plist`: `UIBackgroundModes` = `[remote-notification]`.
- Create `Apps/BabyTrackerLiveActivityPush.entitlements`, byte-identical to `main`'s (`git show main:Apps/BabyTrackerLiveActivityPush.entitlements > Apps/BabyTrackerLiveActivityPush.entitlements`): `aps-environment` = `development` plus the app's three existing entitlements.
- Modify `BabyTracker.xcodeproj/project.pbxproj`:
  - the file reference;
  - for the app target's Debug and Release configurations, `"CODE_SIGN_ENTITLEMENTS[sdk=iphonesimulator*]" = Apps/BabyTrackerLiveActivityPush.entitlements;`, exactly as `main` selects it (`docs/runbooks/family-live-activity-push.md:40` on `main`).
  - Device builds keep `Apps/BabyTracker.entitlements` until the owner regenerates the profile (Blocked on owner). `DEVELOPMENT_TEAM` stays `TEAM123456`.
- Create the tests:
  - `Packages/BabyTracker/Tests/BabySyncTests/{PushTokenRegistrarTests.swift,BackgroundWakeHandlerTests.swift,PushEnvironmentResolverTests.swift}`;
  - `Packages/BabyTracker/Tests/BabyProtocolTests/PushTokenDTOTests.swift`.
- Modify `Makefile`: `EXPECTED_TESTS := 1268` (Task 16's 1253 plus the 15 tests below, none parameterized).

**Interfaces:** Consumes Task 14's wire contract. Produces:
```swift
// BabyProtocol
public enum PushEnvironment: String, Codable, Sendable { case sandbox, production }
public struct PushTokenRequest: Codable, Sendable, Equatable {
    public let protocolVersion: String   // "v1"
    public let capabilities: [String]    // ["push-token-v1"]
    public let token: String             // lowercase hex
    public let environment: PushEnvironment
    // CodingKeys: protocol_version, capabilities, token, environment
    public init(token: Data, environment: PushEnvironment)
}

// BabySync
public protocol PushEnvironmentResolving: Sendable { func environment() -> PushEnvironment? }
public struct PushEnvironmentResolver: PushEnvironmentResolving { /* main's ProvisioningProfileEnvironmentResolver */ }
public protocol PushTokenTransport: Sendable {
    func putPushToken(_ request: PushTokenRequest) async throws
    func deletePushToken() async throws
}
public struct PushSession: Sendable, Equatable {
    public let serverInstanceID: UUID, accountID: UUID, deviceID: UUID, familyID: UUID
    public init(serverInstanceID: UUID, accountID: UUID, deviceID: UUID, familyID: UUID)
    public init(_ key: FamilySessionKey)
}
/// `.signedIn`: sign-in, create, join or re-authentication; the server may have
/// dropped the token with the old session, so upload whatever the fingerprint says.
/// `.restored`: launch or an engine restart; upload only if the fingerprint changed.
public enum PushActivation: Sendable, Equatable { case signedIn, restored }
@MainActor public final class PushTokenRegistrar {
    public init(environment: any PushEnvironmentResolving, defaults: UserDefaults,
                capabilities: @escaping @Sendable () async throws -> [String])
    /// application(_:didRegisterForRemoteNotificationsWithDeviceToken:)
    public func deviceTokenReceived(_ token: Data) async
    /// After a family session becomes active. Remembers `transport` for `sessionEnding`.
    public func sessionActivated(_ session: PushSession, transport: any PushTokenTransport, activation: PushActivation) async
    /// Before sign-out, leaving or removal: DELETE through the remembered transport, then forget the fingerprint.
    public func sessionEnding() async
}

// SettingsFeature
public struct PushLifecycle {
    public let activated: @MainActor (FamilySessionKey, ConfirmedEndpoint, PushActivation) async -> Void
    public let ending: @MainActor () async -> Void
}
public enum BackgroundWakeResult: Sendable, Equatable { case newData, noData, failed }
@MainActor public final class BackgroundWakeHandler {
    public init(sync: @escaping @MainActor () async -> Bool, deadline: Duration = .seconds(25))
    /// One `.backgroundPush` sync, answered within the deadline (iOS allows about 30 s).
    public func receiveSilentPush() async -> BackgroundWakeResult
}
```
Upload rule:
- The registrar uploads when four things hold:
  - it has a device token;
  - a session is active;
  - the server advertises `push-token-v1`;
  - `PushEnvironmentResolver` returns an environment (nil means not provisioned for push: stay inert, as `main` does).
- With `.signedIn` it always uploads, because a revoked session's token may have been swept on the server. With `.restored`, and when a new token arrives, it uploads only when `fingerprint = SHA256(token ‖ environment ‖ serverInstanceID ‖ accountID ‖ deviceID ‖ familyID)` differs from the stored one. That covers "on change", "at launch when it differs" and "after re-auth". A silent refresh does not activate, so it uploads nothing.
- Only the fingerprint is stored (App Group `UserDefaults`, key `push.uploaded-fingerprint`); the raw token is never persisted or logged.
- A failed upload keeps the old fingerprint, so the next activation retries.

- [ ] **Step 1: Write the failing tests** `PushTokenRegistrarTests.swift`:
```swift
@MainActor
final class RecordingPushTransport: PushTokenTransport, @unchecked Sendable {
    private(set) var puts: [PushTokenRequest] = []
    private(set) var deletes = 0
    var failNext = false
    func putPushToken(_ request: PushTokenRequest) async throws {
        if failNext { failNext = false; throw SyncErrorCode.serviceUnavailable }
        puts.append(request)
    }
    func deletePushToken() async throws { deletes += 1 }
}

struct FixedEnvironment: PushEnvironmentResolving {
    let value: PushEnvironment?
    func environment() -> PushEnvironment? { value }
}

@MainActor
@Suite("Push token registrar")
struct PushTokenRegistrarTests {
    static let token = Data(repeating: 0xab, count: 32)
    static func session() -> PushSession {
        PushSession(serverInstanceID: SyncFixtures.instanceID, accountID: SyncFixtures.accountID, deviceID: SyncFixtures.deviceID,
                    familyID: SyncFixtures.familyID)
    }
    private func make(capabilities: [String] = ["records-v1", "push-token-v1"], environment: PushEnvironment? = .sandbox)
        -> (PushTokenRegistrar, RecordingPushTransport, UserDefaults) {
        let defaults = UserDefaults(suiteName: "push-registrar-\(UUID().uuidString)")!
        let registrar = PushTokenRegistrar(environment: FixedEnvironment(value: environment), defaults: defaults,
                                           capabilities: { capabilities })
        return (registrar, RecordingPushTransport(), defaults)
    }

    @Test func uploadsTheFirstTokenOnceASessionIsActive() async {
        let (registrar, transport, _) = make()
        await registrar.deviceTokenReceived(Self.token)
        #expect(transport.puts.isEmpty) // no session yet
        await registrar.sessionActivated(Self.session(), transport: transport, activation: .signedIn)
        #expect(transport.puts == [PushTokenRequest(token: Self.token, environment: .sandbox)])
        #expect(transport.puts.first?.token == String(repeating: "ab", count: 32))
    }

    @Test func doesNothingWhenTheServerDoesNotAdvertisePush() async {
        let (registrar, transport, _) = make(capabilities: ["records-v1"])
        await registrar.deviceTokenReceived(Self.token)
        await registrar.sessionActivated(Self.session(), transport: transport, activation: .signedIn)
        #expect(transport.puts.isEmpty)
    }

    @Test func doesNothingWhenTheBuildIsNotProvisionedForPush() async {
        let (registrar, transport, _) = make(environment: nil)
        await registrar.deviceTokenReceived(Self.token)
        await registrar.sessionActivated(Self.session(), transport: transport, activation: .signedIn)
        #expect(transport.puts.isEmpty)
    }

    @Test func skipsAnUnchangedTokenAtLaunch() async {
        let (registrar, transport, defaults) = make()
        await registrar.deviceTokenReceived(Self.token)
        await registrar.sessionActivated(Self.session(), transport: transport, activation: .signedIn)
        let relaunched = PushTokenRegistrar(environment: FixedEnvironment(value: .sandbox), defaults: defaults, capabilities: { ["push-token-v1"] })
        await relaunched.deviceTokenReceived(Self.token)
        await relaunched.sessionActivated(Self.session(), transport: transport, activation: .restored)
        #expect(transport.puts.count == 1)
    }

    @Test func uploadsAgainAfterReauthentication() async {
        let (registrar, transport, _) = make()
        await registrar.deviceTokenReceived(Self.token)
        await registrar.sessionActivated(Self.session(), transport: transport, activation: .signedIn)
        await registrar.sessionActivated(Self.session(), transport: transport, activation: .restored) // a launch: unchanged, skipped
        await registrar.sessionActivated(Self.session(), transport: transport, activation: .signedIn) // re-authentication: uploaded
        #expect(transport.puts.count == 2)
    }

    @Test func uploadsAChangedTokenAndRetriesAFailedUpload() async {
        let (registrar, transport, _) = make()
        await registrar.deviceTokenReceived(Self.token)
        transport.failNext = true
        await registrar.sessionActivated(Self.session(), transport: transport, activation: .signedIn)
        #expect(transport.puts.isEmpty)
        await registrar.deviceTokenReceived(Data(repeating: 0xcd, count: 32)) // a new token arrives and retries
        #expect(transport.puts.map(\.token) == [String(repeating: "cd", count: 32)])
    }

    @Test func signOutDeletesTheTokenAndForgetsTheFingerprint() async {
        let (registrar, transport, defaults) = make()
        await registrar.deviceTokenReceived(Self.token)
        await registrar.sessionActivated(Self.session(), transport: transport, activation: .signedIn)
        await registrar.sessionEnding()
        #expect(transport.deletes == 1)
        #expect(defaults.string(forKey: "push.uploaded-fingerprint") == nil)
        #expect(defaults.dictionaryRepresentation().values.allSatisfy { !"\($0)".contains("abab") }) // no raw token persisted
    }
}
```
  `BackgroundWakeHandlerTests.swift`:
```swift
@MainActor
@Suite("Background wake")
struct BackgroundWakeHandlerTests {
    @Test func aPushRunsOneSyncAndReportsNewData() async {
        var syncs = 0
        let handler = BackgroundWakeHandler(sync: { syncs += 1; return true })
        #expect(await handler.receiveSilentPush() == .newData)
        #expect(syncs == 1)
    }

    @Test func aFailedSyncReportsFailed() async {
        let handler = BackgroundWakeHandler(sync: { false })
        #expect(await handler.receiveSilentPush() == .failed)
    }

    @Test func aSyncPastTheDeadlineReportsFailedInTime() async {
        let handler = BackgroundWakeHandler(sync: { try? await Task.sleep(for: .seconds(10)); return true }, deadline: .milliseconds(100))
        let start = ContinuousClock.now
        #expect(await handler.receiveSilentPush() == .failed)
        #expect(ContinuousClock.now - start < .seconds(2))
    }
}
```
  The remaining tests:
  - `PushEnvironmentResolverTests.swift`, ported from `main`'s resolver tests:
    - `simulatorIsSandbox`;
    - `developmentProfileIsSandbox`;
    - `noProfileIsProduction`;
    - `profileWithoutTheKeyIsInert`.
  - `PushTokenDTOTests.swift`, `encodesTheWireShape`: `PushTokenRequest(token: Data([0x0a, 0xff] + Array(repeating: 0, count: 14)), environment: .production)` encodes to exactly `{"capabilities":["push-token-v1"],"environment":"production","protocol_version":"v1","token":"0aff0000000000000000000000000000"}` with sorted keys.
- [ ] **Step 2: Run to see the failure:** `rtk proxy swift test --package-path Packages/BabyTracker --filter 'PushTokenRegistrarTests|BackgroundWakeHandlerTests|PushEnvironmentResolverTests|PushTokenDTOTests'`. Expected: `cannot find 'PushTokenRegistrar' in scope`.
- [ ] **Step 3: Implement.**
  - **`BackgroundWakeHandler.receiveSilentPush`** races `sync()` against `Task.sleep(for: deadline)` in a task group. The first result wins: `true` gives `.newData` and `false` gives `.failed`, and the deadline gives `.failed`, after which the group cancels the sync.
  - **The graph** builds the handler with `sync: { await model.syncNowForBackgroundPush() }`. `FamilySettingsViewModel.syncNowForBackgroundPush()` runs `engine.syncNow(.backgroundPush)`, the same path as a live hint with `.backgroundPush`, which `SyncScheduler.shouldSyncImmediately` already accepts. It returns `true` when `engine.status` is `.upToDate` afterwards (the existing `FamilySyncDriving.status`, set from `SyncCoordinator.lastAttemptSucceeded`, so the protocol and its test fakes are unchanged), and `false` otherwise or when there is no engine.
  - **`PushAppDelegate`:**
```swift
import BabySync
import os
import UIKit

/// The app's only UIKit delegate: APNs registration and background pushes.
/// The push carries no data; the app answers it with one ordinary sync pull.
@MainActor
final class PushAppDelegate: NSObject, UIApplicationDelegate {
    static var registrar: PushTokenRegistrar?
    static var wake: BackgroundWakeHandler?
    private let log = Logger(subsystem: "com.olhapi.babytracker", category: "push")

    func application(_ application: UIApplication, didRegisterForRemoteNotificationsWithDeviceToken deviceToken: Data) {
        Task { await Self.registrar?.deviceTokenReceived(deviceToken) }
    }

    func application(_ application: UIApplication, didFailToRegisterForRemoteNotificationsWithError error: any Error) {
        log.error("remote notification registration failed: \((error as NSError).code, privacy: .public)")
    }

    func application(_ application: UIApplication, didReceiveRemoteNotification userInfo: [AnyHashable: Any]) async -> UIBackgroundFetchResult {
        log.info("background push received")
        guard let wake = Self.wake else { return .noData } // launched before the store opened: the next launch syncs
        switch await wake.receiveSilentPush() {
        case .newData: return .newData
        case .noData: return .noData
        case .failed: return .failed
        }
    }
}
```
    `AppEnvironment.finishOpening` sets `PushAppDelegate.registrar = graph.push` and `PushAppDelegate.wake = graph.wake`, and calls `UIApplication.shared.registerForRemoteNotifications()` when a family session is restored at launch.
  - **No notification permission is requested:** a background push shows nothing, so it needs none.
- [ ] **Step 4: Run green, then build and check on the shared simulator.**
  - `rtk make test` reports exactly 1268 tests.
  - Then, one run at a time:
    1. `pgrep -fl xcodebuild` must print nothing.
    2. `xcrun simctl bootstatus D17101C9-01EB-488D-AFE1-58E6C089B853 -b` (boots the shared simulator if needed; never create, reset or erase).
    3. `rtk proxy xcodebuild -project BabyTracker.xcodeproj -scheme BabyTracker -destination 'platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853' -derivedDataPath /tmp/bt-p3-push-dd build`.
    4. `codesign -d --entitlements :- /tmp/bt-p3-push-dd/Build/Products/Debug-iphonesimulator/BabyTracker.app | grep -A1 aps-environment` shows `development`.
    5. `plutil -extract UIBackgroundModes json -o - /tmp/bt-p3-push-dd/Build/Products/Debug-iphonesimulator/BabyTracker.app/Info.plist` prints `["remote-notification"]`.
  - Delivery check:
    1. `xcrun simctl install D17101C9-01EB-488D-AFE1-58E6C089B853 /tmp/bt-p3-push-dd/Build/Products/Debug-iphonesimulator/BabyTracker.app`;
    2. `xcrun simctl launch D17101C9-01EB-488D-AFE1-58E6C089B853 com.olhapi.babytracker`;
    3. `printf '%s' '{"aps":{"content-available":1}}' > /tmp/bt-p3-wake.apns`;
    4. `xcrun simctl push D17101C9-01EB-488D-AFE1-58E6C089B853 com.olhapi.babytracker /tmp/bt-p3-wake.apns`;
    5. `xcrun simctl spawn D17101C9-01EB-488D-AFE1-58E6C089B853 log show --last 1m --predicate 'subsystem == "com.olhapi.babytracker" AND category == "push"'` shows `background push received`.
  - Remove `/tmp/bt-p3-push-dd` afterwards.
- [ ] **Step 5: Commit:** `rtk git add Packages/BabyTracker Apps BabyTracker.xcodeproj/project.pbxproj Makefile && rtk git commit -m "feat(ios): register for background pushes, upload the APNs token, pull on a push"`

### Task 18: E2E — a backgrounded caregiver is woken by a push and pulls

**Files:**
- Modify `Server/scripts/lib/disposable-sync.sh`: a push profile `BT_DISPOSABLE_PUSH=1`, `main`'s live-activity profile (`git show main:Server/scripts/lib/disposable-sync.sh`, lines 222-262) narrowed to push:
  - a throwaway key from `openssl ecparam -name prime256v1 -genkey -noout | openssl pkcs8 -topk8 -nocrypt`, mode 0600;
  - `BT_APNS_WAKES_ENABLED: "true"`, `BT_APNS_TEAM_ID: E2ETEAM001`, `BT_APNS_KEY_ID: E2EKEY0001`, `BT_APNS_BUNDLE_ID: com.example.babytracker.protocol`, `BT_APNS_KEY_FILE: /run/secrets/apns_key` and `BT_IDENTITYTEST_APNS_SINK: "1"`;
  - nothing in it is an Apple credential, and nothing can reach Apple.
- Modify `Server/scripts/protocol-e2e.sh`: `export BT_DISPOSABLE_PUSH=1` in both modes (unless `BT_DISPOSABLE_PERFORMANCE=1`); pass `BABY_TRACKER_E2E_APNS=1` to `swift test`; require the line `background-push proof:` in the Swift output, as it requires `running-timer snapshot proof:`. Pin it in `Server/scripts/protocol_e2e_test.go`.
- Create, in `Packages/BabyTracker/Tests/FamilySyncClientE2ETests/`:
  - `E2ERace.swift`: `Race`, moved from `OnePartTimerSyncE2ETests.swift:293` and made internal. Its one change is a parameter `caregiverLiveHints: Bool = true`, passed to the caregiver `Phone`'s existing `liveHintsEnabled`, so the existing tests are unchanged and the push test's caregiver holds no live connection;
  - `APNSRecorder.swift`: reads `GET /__identitytest/apns` through `PinnedSession`;
  - `BackgroundWakePushE2ETests.swift`.
- Modify `Packages/BabyTracker/Tests/FamilySyncClientE2ETests/TwoCaregiverSyncE2ETests.swift`: `Phone` passes `pushEnvironment: PushEnvironmentResolver(isSimulator: true)` to `assemble`.
- Modify `Makefile`: `EXPECTED_TESTS := 1269`.

**Interfaces:** Consumes Tasks 13–15 and Task 17's `Graph.push`/`Graph.wake`. Produces `struct APNSRecorder { init(environment: E2EEnvironment); func waitForPush(token: String, timeout: Duration) async throws -> APNSRecord }` and `struct APNSRecord: Decodable { let path: String; let headers: [String: String]; let providerTokenValid: Bool; let payload: JSONValue }`, decoding `main`'s recorder JSON (`provider_token_valid`).

- [ ] **Step 1: Write the failing test** `BackgroundWakePushE2ETests.swift`:
```swift
@Suite("A backgrounded phone is woken by a push", .serialized)
struct BackgroundWakePushE2ETests {
    /// The caregiver's phone has registered an APNs token and holds no live
    /// connection. The owner starts a timer; the server's wake worker pushes
    /// the caregiver (recorded by the identitytest sink, never Apple); the push
    /// makes the caregiver pull, and the timer is on its phone.
    @Test func ownersWriteWakesTheBackgroundedCaregiver() async throws {
        guard let environment = E2EEnvironment.current(),
              ProcessInfo.processInfo.environment["BABY_TRACKER_E2E_APNS"] == "1" else { return }
        let race = try await Race(environment: environment, caregiverLiveHints: false) // no live connection: the hub cannot wake it
        defer { race.tearDown() }
        let (owner, caregiver, baby) = (race.owner, race.caregiver, race.babyID)
        let token = Data((0..<32).map { UInt8($0) | 0x80 })
        let tokenHex = token.map { String(format: "%02x", $0) }.joined()

        // The production registrar uploads through PUT /v1/devices/push-token.
        await caregiver.graph.push.deviceTokenReceived(token)
        try await caregiver.sync() // the caregiver is at the high water: nothing to wake yet

        let timer = try owner.store.startPart(.breast(.left), for: baby)
        try await owner.sync(.localWrite)

        let push = try await APNSRecorder(environment: environment).waitForPush(token: tokenHex, timeout: .seconds(20))
        #expect(push.path == "/3/device/\(tokenHex)")
        #expect(push.headers["apns-push-type"] == "background")
        #expect(push.headers["apns-priority"] == "5")
        #expect(push.headers["apns-topic"] == "com.example.babytracker.protocol")
        #expect(push.providerTokenValid)
        #expect(push.payload == .object(["aps": .object(["content-available": .number(1)])]))

        #expect(await caregiver.graph.wake.receiveSilentPush() == .newData)
        let pulled = try #require(try caregiver.store.activeTimer(kind: .feed, for: baby))
        #expect(pulled.id == timer.id)
        print("background-push proof: push-type=background priority=5 payload=content-available-only pulled-timer=\(timer.id)")
    }
}
```
  `Phone` (`TwoCaregiverSyncE2ETests.swift:701`) keeps the default no-op `registerForRemoteNotifications` and passes `pushEnvironment: PushEnvironmentResolver(isSimulator: true)`, which is sandbox. So the host test exercises the production registrar, lifecycle and HTTP client without a provisioning profile.
- [ ] **Step 2: Run to see the failure:** `rtk make server-protocol-e2e`. Expected: exit non-zero with `missing proof line: background-push proof:`, until the profile and the test exist.
- [ ] **Step 3: Implement** the harness profile, the recorder reader, the `Race` move, the proof-line requirement and its pin.
- [ ] **Step 4: Run green:**
  - `rtk make server-protocol-e2e` exits 0 and prints `background-push proof:`.
  - `rtk env BT_RECOVERY_E2E_MODE=self_hosted make server-protocol-e2e` exits 0, with the push profile on.
  - `rtk make test` reports 1269.
  - `rtk go -C Server test ./scripts -run TestProtocolE2E`.
- [ ] **Step 5: Commit:** `rtk git add Server/scripts Packages/BabyTracker/Tests/FamilySyncClientE2ETests Makefile && rtk git commit -m "test(e2e): a backgrounded caregiver is woken by an APNs push and pulls"`

### Task 19: Freeze 000031 and the fresh-install gates

**Files:** `Server/migrations/000031_request_path_retention.sql` (the header line), `Server/internal/migrate/compatibility.json` (the digest), and script fixes only if the gates expose any. No product change is expected; any change is committed with its reason.

- [ ] **Step 1: Freeze.**
  - Add `-- 000031 FROZEN (Phase 3 Task 19): later changes go in 000032+.` to the header.
  - Record the digest again: `shasum -a 256 Server/migrations/000031_request_path_retention.sql` into `compatibility.json`.
  - Run `rtk proxy env BT_TEST_PACKAGES='./internal/migrate ./internal/database' bash Server/scripts/test-db.sh`.
- [ ] **Step 2: Server gates.** `rtk make server-build`; `rtk make server-test`; `(cd Server && rtk go vet ./... && rtk go vet -tags identitytest ./...)`. Then rerun the round-trip pins verbosely and copy their output into the report: `rtk proxy env BT_TEST_PACKAGES='-v ./internal/httpapi' BT_TEST_RUN='TestSyncHTTPRoundTrips|TestPagedHTTPRoundTrips|TestLiveHandshakeIsOneStatement' bash Server/scripts/test-db.sh`.
- [ ] **Step 3: iOS host tests and the simulator build.**
  - Check that `pgrep -fl xcodebuild` prints nothing, then run `rtk make test`: exactly 1269 tests.
  - Then, one run at a time on the shared simulator only: `pgrep -fl xcodebuild` prints nothing, then `rtk proxy xcodebuild -project BabyTracker.xcodeproj -scheme BabyTracker -destination 'platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853' -derivedDataPath /tmp/bt-p3-gates-dd build` succeeds, and the built app carries `aps-environment` and `UIBackgroundModes` = `["remote-notification"]` (the Task 17 Step 4 checks). Never create, reset or erase a simulator. Remove `/tmp/bt-p3-gates-dd` afterwards.
  - The device configuration still signs with `Apps/BabyTracker.entitlements` (`rtk proxy grep -n 'CODE_SIGN_ENTITLEMENTS' BabyTracker.xcodeproj/project.pbxproj` shows the simulator-only override and the plain device setting), until the owner's profile regeneration.
- [ ] **Step 4: Protocol E2E (hosted), with the iOS suite.** Run `rtk make server-protocol-e2e`. Expected:
  - exit 0;
  - `two-caregiver proof: step 18:`;
  - `owner-reinstall proof:` with the phone ledger equal to the `care_entities` ledger;
  - `running-timer snapshot proof:`;
  - `wake proof:` (Task 8);
  - `background-push proof:` (Task 18): the backgrounded caregiver was pushed with `apns-push-type: background`, priority 5 and a content-available-only payload, and pulled the owner's timer;
  - the cleanup proof with zero containers and zero volumes.
- [ ] **Step 5: Protocol E2E (self-hosted, recovery):** `rtk env BT_RECOVERY_E2E_MODE=self_hosted make server-protocol-e2e`. Expected: exit 0, with the five per-boundary recovery runs and their `recovery SQL proof` lines, and `background-push proof:` again. One rerun is allowed for the recorded Swift flakes (Phase 1 ledger, Task 16; Phase 2 ledger :157); a second failure blocks.
- [ ] **Step 6: MCP and smoke.**
  - `rtk bash Server/scripts/mcp-e2e.sh`: four clients, `matches=0`, `schema=31`. Run it with the machine idle, not straight after protocol E2E (Phase 2 ledger :157).
  - `rtk make server-smoke`:
    - runtime and maintenance grants match manifest 31;
    - `rate_limit_buckets` is absent;
    - `authority verify` is clean;
    - `contiguous schema 31`;
    - push is off in the smoke configuration (no `BT_APNS_*`). `/v1/server` still equals `Server/api/fixtures/server-info.json`, which has no `push-token-v1`. A new line after the challenge check, `[[ $(curl --silent --max-time 5 -o /dev/null -w '%{http_code}' -X PUT "$base/v1/devices/push-token") == 404 ]]`, pins that the route is not registered: with push on, the same unauthenticated request would be `401`. Pin the line in `smoke_test.go`.
- [ ] **Step 7: Commit** any script fixes and the freeze: `rtk git add Server/migrations/000031_request_path_retention.sql Server/internal/migrate/compatibility.json Server/scripts && rtk git commit -m "test(e2e): freeze 000031 and verify the one-transaction request path on a fresh install"`

### Carries out of Phase 3
- **Phase 4 (import script):**
  - set `refresh_receipts.child_scope`, `child_family_id` and `child_role` on every imported recovery receipt from its legacy frozen scope;
  - load legacy audit history into partitions that exist: create yearly partitions for pre-2026 years, or accept the default partition and document it, but never into a month the roller has not created;
  - leave `device_sync_cursors` empty (the wake backstop repopulates it);
  - keep each family's `next_sequence` (R2 C2);
  - replace the 000029, 000030 and 000031 fresh-install guards, the 2027 audit-row guard included;
  - leave `device_push_tokens` empty: phones upload their token again at their next sign-in or launch (the registrar's fingerprint includes the server instance).
- **Phase 4/6 (reconciliation with `main`, before any merge):**
  - renumber `main`'s `000028_live_activity_push.sql` past this branch's migrations, as a new migration on the PostgreSQL authority;
  - make `main`'s `liveactivity` transport use `internal/apns` (provider token, client core, classification) and `config.APNSCredentials`, so the APNs code exists once; the two features share one key, one team and one connection pool per environment;
  - make `LiveActivityAPNsEnvironment` a typealias or thin wrapper of `PushEnvironmentResolver`;
  - bring over `main`'s startup preflight for the APNs key (`scripts/live-activity-push-signing.sh` and the server-side key checks) so the wake key gets the same checks.
- **Phase 5 (load and tuning):**
  - audit retention per decision D1, with the verifier anchoring at the oldest kept partition;
  - size the request pool from measurement, and check the 2 background connections;
  - measure the in-memory limiter at 2× load;
  - measure retention throughput at 2× load (140 consumed sessions/s) against `SessionBatch` and `RetentionInterval`;
  - measure wake worker cost with 40k device rows;
  - HOT and fillfactor on `care_entities` (Phase 2 :49);
  - family `lock_timeout` or bounded waiters (R1);
  - collapse MCP reads and history into one transaction if profiles show it matters;
  - export `Download` streaming outside its transaction;
  - the mass-snapshot test (R2 C3);
  - the recorded Swift and mcp-e2e flakes.
- **Phase 6 (operator docs and rollout):**
  - the maintenance credential is required (decision D3);
  - the connection budget gains the background pool (2);
  - document the retention windows and how to change them;
  - Unraid wiring for push: mount the APNs key as a file secret and set `BT_APNS_WAKES_ENABLED`, `BT_APNS_TEAM_ID`, `BT_APNS_KEY_ID`, `BT_APNS_BUNDLE_ID` and `BT_APNS_KEY_FILE` (the keys documented in `Server/README.md` by Task 13); production credentials are operator-provided at deploy, never committed;
  - switch device builds to the push entitlements (`CODE_SIGN_ENTITLEMENTS` for `iphoneos`) once the owner has regenerated the development profile with `aps-environment` (Blocked on owner);
  - the production APNs environment: distribution signing, TestFlight registering production tokens, and an end-to-end check against Apple's production host;
  - Apple token revocation on account deletion;
  - retire the projector role;
  - the Unraid overlay and README;
  - the older either-or `has_table_privilege` lines in `check-principals.sh` (Phase 2 :138).

### Critical Files for Implementation
- <repo>/.worktrees/postgres-authority/Server/migrations/000031_request_path_retention.sql (new)
- <repo>/.worktrees/postgres-authority/Server/internal/authority/request.go (new) and guard.go
- <repo>/.worktrees/postgres-authority/Server/internal/sync/serve.go (new), exchange.go, paging.go
- <repo>/.worktrees/postgres-authority/Server/internal/httpapi/sync.go
- <repo>/.worktrees/postgres-authority/Server/internal/maintenance/ (new) and wake/ (new)
- <repo>/.worktrees/postgres-authority/Server/cmd/baby-tracker-server/main.go and authority_runtime.go
- <repo>/.worktrees/postgres-authority/Server/internal/apns/ (new), pushtoken/ (new), httpapi/push_token.go (new), wake/push.go (new), config/apns.go (new)
- <repo>/.worktrees/postgres-authority/Packages/BabyTracker/Sources/BabySync/PushTokenRegistrar.swift (new), BackgroundWakeHandler.swift (new), and Apps/BabyTrackerApp/PushAppDelegate.swift (new)

### Self-review (writing-plans checklist)
- **Spec coverage:** the Phase 3 sections are mapped.
  - Readiness: Task 3.
  - One transaction per request: Tasks 4–6.
  - Rate limits: Task 1, with the restart ruling in the header.
  - Database client discipline: Task 3 (prepared statements, `ShouldPing`, pools) and Task 5 (batched round trips pinned).
  - Live updates and wakes: Tasks 7–8.
  - Retention: Tasks 2 and 9–11.
  - The phasing item "request path: readiness, in-memory limits, live hub, wakes, cleanup" is complete.
  - Owner decision D4, APNs:
    - client and configuration: Task 13;
    - device-token route, its table (Task 2), rate limit and `v1.md` section: Task 14;
    - worker delivery, the per-device interval and backoff, and token retention: Tasks 2, 14 and 15;
    - iOS registration and background handling: Task 17;
    - E2E proof: Task 18, gated in Task 19.
  - D2 (24 hours): Task 9. D3 (maintenance credential required): Task 3. D1: carried to Phase 5.

  Unmapped spec requirements, deferred with reasons in the carry map: MCP and history in one transaction, and sizing the request pool from measurement.
- **Placeholder scan:** no step says TBD, "add error handling" or "similar to Task N". Secondary tests carry their full assertions as comments, as the Phase 2 plan's secondary tests did. Every named function is defined in the Interfaces block of the task that introduces it.
- **Type consistency:** these names are used identically across tasks:
  - `authority.Request{Digests, Admit, Charge}`;
  - `WithRequestTx`/`WithRequestReadTx`/`AuthorizeRequest`/`RequestGuard`;
  - `auth.AccessDigests`;
  - `sync.ServeSync`/`ServeSnapshot`/`Progress`;
  - `wake.Tracker.Synced`/`Changed`, `wake.Worker.Tick`;
  - `maintenance.Sweep`/`Loop`/`SessionSweep`/`ReceiptSweep`/`ExpirySweep`/`Purger`/`AuditPartitionSweep`;
  - `care.StoredReceiptCharge`;
  - `caretest.AssertCounters`;
  - `database.VerifyMaintenanceRole`/`OpenBackground`;
  - `readiness.Monitor`;
  - `apns.Client.Wake`/`WakePayload`/`Result`/`Class` (`Accepted`, `InvalidToken`, `Retryable`, `ConfigurationFailure`, `Ambiguous`), `config.APNSCredentials`/`LoadAPNSWakes`;
  - `pushtoken.Service.Register`/`Unregister`, capability `push-token-v1`, limiter scopes `push-token-device`/`push-token-account`;
  - `wake.Pusher`, `wake.APNsInterval`, `wake.MaxPushesPerTick`, `Worker.Pusher`/`Connected`, `Stats.Pushed`/`PushFailed`, `realtime.Hub.DeviceConnected`, `maintenance.PushTokenSweep`;
  - `app.push_targets`/`app.prune_push_tokens`, `device_push_tokens`, `device_sync_cursors.pushed_sequence`/`pushed_at`;
  - Swift: `PushEnvironment`, `PushTokenRequest`, `PushTokenTransport`, `PushSession`, `PushTokenRegistrar`, `BackgroundWakeHandler`/`BackgroundWakeResult`, `Graph.push`/`Graph.wake`.
- **Review Focus:** each of the five lines has its test in the owning task: 1 and 2 in Task 10, 3 in Task 11, 4 in Task 5, 5 in Tasks 8 and 15.
- **Test counts:** `EXPECTED_TESTS` goes 1251 → 1253 (Task 16) → 1268 (Task 17) → 1269 (Task 18), and Task 19 checks 1269.
