# Recoverable Session Refresh Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** Recover an interrupted authorized refresh and credential installation without Apple proof, while preserving current expiry and revocation rules.

**Architecture:** Add a device-bound, capability-gated refresh operation whose committed child and frozen derivation material can be recovered exactly. Persist an additive SQLite installation checkpoint and bind its expected credential version and fingerprints inside the atomic Keychain item. Keep account leases and current-authority validation across all processes; acknowledge after durable installation without blocking sync.

**Tech Stack:** Go, PostgreSQL 17, existing security authority/ledger/projector, Swift 6, SwiftData, CryptoKit, shared Keychain, Swift Testing, XCTest.

**Spec:** `docs/superpowers/specs/2026-10-01-session-recovery-design.md` (written specification and implementation plan approved by user; subagent-driven execution selected).

## Global Constraints

- Preserve 365 days from the original Apple sign-in, ten-minute access credentials, single-use rotation, explicit sign-out, device/membership revocation, recovery epochs and the hosted policy.
- Existing `/v1/auth/refresh`, its v2 token derivation, legacy fixtures and strict request contract remain unchanged.
- New requests use `protocol_version: "v1"` and exactly `identity-v1` and `refresh-recovery-v1`; reject unknown fields, malformed/zero IDs, extra capabilities and oversized requests before rotation.
- Migration is `000025_refresh_recovery.sql`; current combined source compatibility becomes exactly 25. Never rewrite an applied migration; stop for integration reconciliation if another migration occupies 25.
- No raw access/refresh/device secrets or Apple proof in receipts, SQLite, defaults, logs or diagnostics. No new secret key.
- Unresolved receipt lifetime is the earlier of the original effective policy deadline and frozen child refresh expiry; no arbitrary short TTL.
- Preserve unrelated dirty main changes and the ignored official Growth source. No deployment, push, merge, phone update, reset, uninstall, production logs or production fault/load tests in this plan.
- Use Conventional Commits, scoped staging and immutable SHA pins for any added CI actions. Prefix shell commands with `rtk`.

## Review Focus

- Historical malformed optional checkpoint data must fail closed while retaining local records, rather than silently decoding as idle (Task 5).
- Parent expiry after commit must not prevent exact recovery of a still-valid child; hosted and child deadlines remain authoritative (Task 3).
- An acknowledgement for an absent receipt validates caller/device independently without inventing historical lineage proof (Task 3).
- Account-to-family promotion during interruption preserves the same attempt and binding (Tasks 5–6).
- Home can show a completed empty pull alongside this phone's pending outbox without implying whole-family delivery (Task 7).

## File ownership and runner map

All paths below are repository-relative. Server changes remain in existing auth/security boundaries; new focused files contain recovery-only contracts and behavior. Existing stage/projector SQL lives in migration 5 and later replacements: add replacements only in migration 25. `security.SessionView` currently omits the rotation material digest; retain it in replayed authority state before trusting receipts.

Swift model data belongs in BabyStore, token material and Keychain bindings in BabySync, and Home receives a presentation value/action from app composition; HomeFeature must not gain a SettingsFeature dependency. Keep historical schema lists/entity names unchanged and prove store reopening before selecting any new schema version.

Verified by reading scripts (not executing implementation tests):
- Repository root: `rtk make test` calls literal-count `scripts/check-tests.sh`; initial count 1112 includes the deliberate RED reproduction. Bump by the actual number of new registered tests in each Swift-test commit.
- Package directory `Packages/BabyTracker`: `rtk proxy swift test --disable-sandbox --filter NAME` supports focused Swift Testing runs used by the investigation. Sandbox approval is environmental, never proof of a source failure.
- Root: `rtk proxy env BT_TEST_PACKAGES="./internal/auth ./internal/security" BT_TEST_RUN="Recovery" bash Server/scripts/test-db.sh` uses existing filter variables, an owned disposable database, `-p=1 -race -count=1` and role initialization. Do not supply an external DSN.
- Root: `rtk make server-test`, `rtk make server-build`, `rtk make server-protocol-e2e`, `rtk proxy bash Server/scripts/refresh-recovery-e2e.sh`, `rtk make build`, `rtk make test-ui` exist; `refresh-recovery-e2e.sh` is created in Task8 and must not be assumed available before then. E2E owns PostgreSQL/app volumes and pins TLS for real Swift clients.
- Server directory: `rtk proxy go test ./internal/protocol`, `rtk proxy go vet ./...`. Full database execution goes through `test-db.sh`.

Independent task review precedes the next task. Reconcile the existing tracker at execution time, record commands/counts and blockers there; this planning pass does not edit it. Every step below inherits the global constraints.

### Task 1: Freeze v3 recovery derivation and canonical material

**Files:** Create `Server/internal/auth/refresh_recovery_types.go`, `refresh_recovery_tokens.go`, `refresh_recovery_tokens_test.go`; modify `Server/internal/auth/types.go` only for additive stage metadata; retain `tokens.go` v2 behavior.

**Interfaces:**
- Produce `RecoveryDerivationInput { RefreshDerivationInput; AttemptID, CanonicalOrigin, ContinuityKeyID string; ContinuityDigest [32]byte }` and `DeriveRecoverableRefreshRotation(RecoveryDerivationInput) (RefreshDerivation, error)`.
- Produce `FrozenRecoveryInput`: all corresponding fields except `ParentKey.Secret`; retain parent key ID/digest, output key ID, scope/family, every deadline, head generation/revision/hash/target revision, attempt/origin/continuity binding and derivation version `v3`.
- Produce `RecoveryMaterial { Input FrozenRecoveryInput; CreatedAt, EffectiveDeadline time.Time }` and `EncodeRecoveryRefreshStage(RefreshStage, RecoveryMaterial) ([]byte, [32]byte, error)`; canonical full stage bytes bind receipt material to the existing projection digest.
- Reconstruction restores the named configured parent key into frozen input; output key is used to verify child token digests, never today's current key.

- [x] **Step 1: Add failing tests.**

- `TestRecoveryV3FrozenVectors` pins literal command/session/access/refresh outputs from fixed synthetic keys and timestamps; replay equals all four outputs. `TestRecoveryV3BindsAttemptOriginAndProof` changes each bound field independently and asserts every output differs.
- `TestRecoveryV3RejectsMalformedInputs` covers zero/mixed-case attempt, invalid origin, key/digest/deadlines.
- `TestRecoveryStageCanonicalBytes` asserts stable bytes/digest, no raw secrets and altered input changes digest. Existing `TestRefreshRotationAndReuseKnownAnswers` vectors remain byte-for-byte unchanged.

- [x] **Step 2: Run RED.**

From Server: `rtk proxy go test ./internal/auth -run 'TestRecoveryV3|TestRecoveryStage'`; expect named new tests to fail on missing implementation, not a skipped database suite.

- [x] **Step 3: Implement the interfaces.**

Reuse framed HMAC and timestamp normalization; domains are exactly `baby-tracker-refresh-command-v3`, `baby-tracker-refresh-session-v3`, `baby-tracker-refresh-access-v3`, `baby-tracker-refresh-token-v3`. Freeze first-rotation inputs once; recovery never calls the time/head-sensitive v2 input builder.

- [x] **Step 4: Run GREEN.**

Repeat RED command; run `rtk proxy go test ./internal/auth -run 'Deriv|Token|Stage'` and verify old vectors still pass.

- [x] **Step 5: Commit scoped task files:**

`feat(auth): add frozen recoverable refresh derivation`.


### Task 2: Commit and repair receipt projection atomically at schema 25

**Files:** Create `Server/migrations/000025_refresh_recovery.sql`, `Server/internal/database/refresh_recovery_schema_test.go`, `Server/internal/security/refresh_recovery_projector_test.go`, `refresh_recovery_authority_test.go`; modify `Server/internal/auth/repository.go`, `Server/internal/security/state.go`, `event_handlers.go`, `event.go`, `commands.go`, `prepared.go`, `Server/internal/migrate/compatibility.json`, `compatibility_test.go`, database schema/role fixture tests and `Server/scripts/protocol-e2e.sh` literal schema gate.

**Interfaces:**
- Produce `RecoveryReceipt { Material []byte; ProjectionMaterialDigest [32]byte; ChildSessionID, ParentSessionID, AttemptID string }`, `(*Repository).LookupRecoveryReceipt(ctx context.Context, parentSessionID, attemptID string) (RecoveryReceipt, bool, error)` and retain `SessionView.ProjectionMaterialDigest string` through grant/rotation replay. Add optional `RefreshAttemptID string` to SessionPayload/rotation Effect/SessionView, omitted for legacy canonical bytes; authority rotation binds parent, child and logical attempt even after receipt retirement. Derived-only parent `RotationAttemptID`/`ChildSessionID` preserve outgoing binding while the child's `RefreshAttemptID` remains immutable incoming binding; A→B→C replay keeps both directions distinct.
- SQL `app.lookup_refresh_recovery_receipt(uuid,uuid)` returns only canonical material, commitment and bound child/parent/attempt. Narrow SECURITY DEFINER with fixed search_path; no runtime table SELECT/DELETE grants.
- Table `refresh_recovery_receipts` has unique parent and unique instance/epoch/account/device/parent/attempt binding, canonical stage bytes/material commitment, frozen inputs/key references, child ID, created time and effective deadline. Projected stage cleanup leaves receipt intact.

- [x] **Step 1: Add failing tests.**

- `TestRecoveryProjectionAtomicReceipt` asserts parent consumed + exactly one child + exactly one receipt or none after injected transaction failure. `TestRecoveryProjectionRepair` commits authority with missing/failed projection, repairs, and asserts identical child/receipt bytes.
- `TestRecoveryStageIsNotReceipt` asserts staged-only request cannot be looked up.
- `TestRecoveryAuthorityRetainsMaterialDigest` verifies replay and defensive SessionView copies retain the committed digest.
- `TestRecoverySchemaRoleSeparation` asserts runtime direct read/delete denied and only documented functions executable. `TestRecoveryLegacyEffectBytesUnchanged` pins legacy rotation effect bytes while recovery rotations retain canonical nonzero attempt binding through replay and receipt retirement.

- [x] **Step 2: Run RED.**

Root: `rtk proxy env BT_TEST_PACKAGES="./internal/database ./internal/security ./internal/migrate" BT_TEST_RUN="Recovery" bash Server/scripts/test-db.sh`; expect missing schema/commitment failures.

- [x] **Step 3: Implement migration and projection.**

Extend stage parsing/canonicalization for optional recovery material and authority `refresh_attempt_id` without relaxing legacy JSON. Persist attempt binding in parent/child authority state so exact unavailable receipt and different-attempt replay remain distinguishable after cleanup; no permanent acknowledgement tombstones. Consume parent, insert child and receipt inside `app.project_security_event`'s same transaction. Verify full canonical material digest against event and parent/child binding. On repair, reproduce receipt before clearing staged token fields; do not accept altered projected material.

- [x] **Step 4: Update all schema surfaces, then GREEN.**

SHA-256 migration 25 in compatibility.json; preserve digests 1–24; min/max/known exactly 25. Copy schema privilege manifest 24 to 25, add narrow function/table privilege entries and update fixture column lists. Update literal 24 assertions in database tests and future-schema probes in roles_test (use 26), migration sequence expectations and E2E schema gate. Repeat RED command, then `rtk proxy env BT_TEST_PACKAGES="./internal/migrate ./internal/database ./internal/security" bash Server/scripts/test-db.sh`; require complete pass.

- [x] **Step 5: Commit scoped task files:**

`feat(auth): persist atomic refresh recovery receipts`.


### Task 3: Recover current authorized child, acknowledge and bound retention

**Files:** Create `Server/internal/auth/refresh_recovery.go`, `refresh_recovery_test.go`, `refresh_recovery_retention.go`, `refresh_recovery_retention_test.go`; modify `Server/internal/auth/repository.go`, migration 25 (still unapplied branch), `Server/cmd/baby-tracker-server/main.go`, `main_test.go`; add isolated periodic recovery cleanup worker/configuration and tests; add authority-focused tests in security as needed. Shared session lifetime helpers in `sessions.go` remain unchanged; new service methods belong in the focused recovery files.

**Interfaces:**
- Produce `RecoverableRefreshInput { RefreshToken, AttemptID string; DeviceContinuity identity.DeviceContinuityClaim }`, `RecoveredSessionBundle { SessionBundle; RefreshAttemptID string }`.
- Produce `(*Service).ResolveRecoverableRefreshSubject(context.Context, RecoverableRefreshInput) (RefreshSubject,error)`, `RefreshRecoverable(context.Context, RecoverableRefreshInput) (RecoveredSessionBundle,error)`, `AcknowledgeRefresh(context.Context, access.Principal, string, identity.DeviceContinuityClaim) error`.
- Produce `RecoveryKeyInventory { SessionToken []string; DeviceContinuity []string }`, `(*Repository).RecoveryKeyCandidates(context.Context) ([]RecoveryKeyCandidate,error)` and conservative raw `(*Repository).ReferencedRecoveryKeyIDs(context.Context) (RecoveryKeyInventory,error)`. Current readiness eligibility uses `(*Service).ReferencedRecoveryKeyIDs(context.Context, security.AuthoritySnapshot) (RecoveryKeyInventory,error)` with the already validated snapshot, never reentering the authority fence. Performance Task 4 migrates this argument to `AuthorityView`; inventory includes outstanding usable receipt parent/output/device key IDs plus still-usable active session token IDs, sorted unique defensive copies. Expired database rows whose status remains active cannot keep obsolete keys required forever: eligibility uses current authority and effective policy/refresh expiry, with conservative retention if it cannot establish safe retirement. Add `TestRecoveryExpiredRowsReleaseUnneededKeyReferences` without weakening outstanding receipt retention.
- SQL `app.ack_refresh_recovery_receipt(uuid,uuid,uuid,uuid,bigint,uuid)` deletes only a matching existing account/device/instance/epoch/lineage receipt after service validation; `app.cleanup_refresh_recovery_receipts(timestamptz)` is maintenance-only. An independent bounded lifecycle worker invokes periodic cleanup through the isolated maintenance principal, reusing the configured export maintenance pool or a recovery-only configuration when exports are disabled. Runtime/projector privileges remain unchanged. Ack is account/device-scoped, never a family write.

- [x] **Step 1: Add failing tests.**

- `TestRecoveryExactRetryOneChild` asserts identical credentials/attempt/deadlines after time/head advance, current presentation and one authority rotation.
- `TestRecoveryConsumedParentQuotaSubject` validates proof + commitment + active child without legacy resolver/reuse calls; rejects tampering/cross-account.
- `TestRecoveryWrongProofNeverRevokes` asserts unchanged head and active lineage;
- `TestRecoveryDifferentAttemptRevokes` asserts conflict and existing reuse revocation for genuinely valid matching proof.
- `TestRecoveryMissingReceiptNoReuseRevocation` returns `refresh_receipt_unavailable`, head unchanged.

- [x] **Step 2: Add policy/lifecycle RED cases and run.**

- `TestRecoveryCurrentAuthorityOverridesFrozenScope` table covers logout, device/account/membership/scope/entitlement/epoch revocation, consumed child; no credentials returned. `TestRecoveryDeadlineMatrix` includes fixed self-hosted 365 days, hosted deadline, frozen child expiry and parent-expired/child-valid recovery.
- `TestRecoveryAcknowledgement` covers current child/active descendant, absent receipt 204 after independent proof, wrong proof/lineage unchanged receipt and expired access. `TestRecoveryRetention` proves restart/inactivity survival, successor atomic deletion, per-device fresh-proof cleanup, expiry/revocation cleanup, lost/repeated ack bounded rows. Run root `rtk proxy env BT_TEST_PACKAGES="./internal/auth ./internal/security ./cmd/baby-tracker-server" BT_TEST_RUN="Recovery" bash Server/scripts/test-db.sh`; expect named failures.

- [x] **Step 3: Implement service and lifetime checks.**

Use current authority fences for proof and epoch/account/device/lineage/scope authorization, then verify canonical receipt against authority digest and derived child IDs/token digests. Consumed-parent quota classification uses independently verified matching proof, authority attempt binding, committed receipt if present and current child; it may classify a valid conflicting attempt for quota charging, leaving destructive handling to RefreshRecoverable. An exact removed receipt returns unavailable without reuse handling. Consumed-parent lookup checks child deadline rather than obsolete parent's refresh expiry. Unconsumed parent uses current parent authority. Return already-expired access unchanged; client refreshes child next. Different-attempt destructive handling occurs only after valid matching continuity and bound parent classification; consumed child never reactivated.

- [x] **Step 4: Implement retention/readiness and GREEN.**

- When migration 25 changes in this task, refresh its still-unapplied compatibility digest and rerun the migration/schema gates from Task 2 before committing. Atomic successor projection deletes predecessor receipt proving possession of its child; ack and expiry cleanup are idempotent. Merge key inventory with existing authority identity/device/invitation refs; missing configured receipt/active keys blocks readiness and normal retirement, errors contain key IDs only.
- `TestRecoveryKeyRetentionAcrossRestart` removes each required key then asserts readiness refusal; retained previous keys recover exactly. No emergency-key-removal command is introduced here. Repeat RED command; run full server suite at final gate. Coordinate shared main.go readiness composition with companion performance plan.

- [x] **Step 5: Commit scoped task files:**

`feat(auth): recover and retire device-bound refresh attempts`.


### Task 4: Expose strict capability-gated HTTP and Swift wire contracts

**Files:** Create `Server/internal/httpapi/refresh_recovery_test.go`, `Server/internal/protocol/refresh_recovery_test.go`, `Packages/BabyTracker/Sources/BabySync/RefreshRecoveryTransport.swift`, `Packages/BabyTracker/Tests/BabySyncTests/RefreshRecoveryTransportTests.swift`; modify `Server/internal/httpapi/{router.go,auth.go,operational.go}`, `Server/internal/protocol/{auth.go,types.go}`, `Server/api/openapi.yaml`, `Server/api/fixtures/server-info.json`; add recovery request/session/error fixtures; modify `HTTPIdentityClient.swift`, `EndpointIdentity.swift` and protocol fixture tests.

**Interfaces:**
- Go produce `protocol.RecoverableRefreshRequest` and `RefreshAckRequest` with strict metadata/attempt/continuity; new routes POST `/v1/auth/refresh-recoverable` and `/v1/auth/refresh-ack`; recovery response is a separate RecoverableRefreshResponse embedding unchanged SessionResponse plus `refresh_attempt_id` and `server_instance_id` for strict echoed instance binding.
- Swift produce `RecoverableRefreshResult { credentials: SessionCredentials; attemptID: UUID; accountID: UUID; reauthenticateAt: Date }` and `SessionRecoveryTransport: SessionRefreshing` with `serverInfo(_ origin: ServerOrigin) async throws -> ServerInfo`, `refreshRecoverable(parent: SessionCredentials, expectedAccountID: UUID, attemptID: UUID, at: ConfirmedEndpoint) async throws -> RecoverableRefreshResult`, `acknowledgeRefresh(attemptID: UUID, credentials: SessionCredentials, at: ConfirmedEndpoint) async throws`.
- `HTTPIdentityClient` conforms; `ServerInfo.advertisesRefreshRecovery: Bool`. Keep old SessionRefreshing conformers working; unavailable recovery transport means deliberate legacy capability selection, never fallback of a persisted recovery attempt.

- [x] **Step 1: Write failing HTTP tests.**

- `TestRecoveryHTTPStrictContract` rejects unknown/nested fields, malformed/zero/uppercase ID, invalid protocol, duplicate/extra/missing capabilities and body > existing `authBodyBytes`; zero service rotations. `TestRecoveryHTTPQuotaBeforeRotation` uses existing IP/account/device quotas including consumed parent, exact 429 and no consumption on refusal.
- `TestRecoveryCapabilityReadiness` omits capability when schema/functions/key validation unavailable; old refresh still accepts only identity-v1.
- `TestRecoveryAckHTTP` asserts Bearer active authority + device validation and 204 empty body.

- [x] **Step 2: Write failing Swift tests and run RED.**

`recoveryWireHasExactFields`, `recoveryResponseRequiresMatchingAttemptAndIdentity`, `ackAcceptsEmpty204`, `recoveryRefusesRedirect` pin exact JSON, account/device/epoch, RFC3339 and wrong echo rejection. Server command: `rtk proxy go test ./internal/protocol ./internal/httpapi -run Recovery` (Server directory); Swift command: `rtk proxy swift test --disable-sandbox --filter RefreshRecoveryTransportTests` (package directory).

- [x] **Step 3: Implement handlers and wire types.**

Dedicated resolver charges account/device before rotation. Ack parses strict body, authenticates caller, then independently checks proof. Map `refresh_attempt_conflict` to 409 nonretryable and `refresh_receipt_unavailable` to 401 nonretryable actionable auth failure; keep readiness/transport retryable. Advertise only after Task 3 readiness and installed SQL functions succeed and `cfg.RecoveryCleanup.Enabled` confirms periodic maintenance cleanup is configured. Missing maintenance configuration keeps legacy startup/refresh available and omits the new capability. Require nonnil `Options.RecoveryReadiness` for recovery advertisement/routes; production supplies it through the isolated maintenance pool to check the cleanup signature, schema USAGE and EXECUTE. Application readiness retains runtime/projector function and key validation; missing cleanup readiness omits/denies only recovery. Add a no-response-body HTTP send helper for 204 ack. Legacy decoder/request and fixtures stay unchanged.

- [x] **Step 4: Run GREEN.**

Repeat commands; update fixtures/schema digests where actual manifests require them, add Swift registered count to Makefile and run decoding/route tests (`rtk proxy swift test --disable-sandbox --filter 'HTTPIdentityClientDecodingTests|HTTPRouteTests|RefreshRecoveryTransportTests'`). Assert no tokens or receipt internals in errors.

- [x] **Step 5: Commit scoped task files:**

`feat(protocol): add capability-gated refresh recovery endpoints`.


### Task 5: Add historical-safe installation checkpoints and atomic Keychain binding

**Files:** Create `Packages/BabyTracker/Sources/BabyStore/Models/RefreshRecoveryCheckpoint.swift`, `Packages/BabyTracker/Sources/BabyStore/RefreshRecoveryStore.swift`, `Packages/BabyTracker/Tests/BabyStoreTests/RefreshRecoveryStoreTests.swift`; modify `Models/AccountAccessState.swift`, `Models/RefreshAttemptState.swift`, `BabySync/KeychainSessionStore.swift`, `BabyStore/AccountAccessStore.swift`; extend `BabyStoreTests/SchemaMigrationTests.swift`, `BabySyncTests/KeychainSessionStoreTests.swift` and `AccountSessionLeaseTests.swift`.

**Interfaces:**
- BabyStore produce Codable/Sendable/Equatable `RefreshProtocolMode { legacy, recoverableV1 }`, `RefreshInstallBinding { key: AccountLeaseKey; recoveryEpoch: UInt64; attemptID, parentCredentialVersion, expectedCredentialVersion: UUID; accessFingerprint, refreshFingerprint: String }`; fingerprints are lowercase SHA-256 of token UTF-8, no tokens.
- `RefreshRecoveryCheckpoint { attempt: RefreshAttempt; recoveryEpoch: UInt64; phase: Phase; installBinding: RefreshInstallBinding?; acknowledgement: AckRetry?; predecessorAcknowledgement: AckRetry? = nil }`, Phase `awaitingReply`, `installPrepared`, `ackPending`; AckRetry carries only attempt/binding, nextRetryAt and bounded retry count. Attempt has optional protocolMode and defaults absent to legacy on decode; AccountLeaseKey pins origin/instance/account/device only; checkpoint and install binding explicitly pin recoveryEpoch, verified against parent credentials, fresh ServerInfo and response.
- Add computed optional `AccountAccessState.refreshRecoveryData: Data?` backed by an additive field in existing `refreshStateData` JSON, preserving legacy enum shape and all modeled V2–V7 hashes. A literal modeled property demonstrably breaks historical opening; use a strict throwing codec for production mutation/access, never an idle fallback; produce throwing `BabyStore.refreshRecoveryCheckpoint(for key: AccountLeaseKey) throws -> RefreshRecoveryCheckpoint?`, `beginRecoverableRefresh(for key: AccountLeaseKey, recoveryEpoch: UInt64, credentialVersion: UUID, supersedingBinding: RefreshInstallBinding? = nil, using token: FencedLeaseToken, now: Date) throws -> RefreshRecoveryCheckpoint`, `prepareRefreshInstall(_ binding: RefreshInstallBinding, for key: AccountLeaseKey, using token: FencedLeaseToken, now: Date) throws`, `completeRefreshInstall(_ binding: RefreshInstallBinding, for key: AccountLeaseKey, using token: FencedLeaseToken, now: Date, installCredentials: () throws -> Void) throws`, `updateRefreshAcknowledgement(_ retry: AckRetry?, observing binding: RefreshInstallBinding, for key: AccountLeaseKey, using token: FencedLeaseToken, now: Date) throws`. All throwing mutation methods use `FencedLeaseToken`, current explicit Date and existing app-group lock.
- `SessionCredentials.installBinding: RefreshInstallBinding? = nil` lives inside the same atomic Keychain value; `credentialVersion` remains existing field. Existing initializer callers compile; historical missing binding decodes nil. Fresh sign-in clears recovery metadata explicitly; promotion/authentication does not.

- [x] **Step 1: Write failing checkpoint tests.**

`preparedInstallSurvivesReopen` asserts exact expected version survives independent disk reopen before Keychain change. `completeBoundInstallUnderNewFence` reacquires expired lease and completes exact binding; original generation is irrelevant. `arbitraryChangedVersionDoesNotResolveAttempt`, `staleFenceAndFreshSignInRejectInstall`, `promotionPreservesRecoveryCheckpoint`, `malformedRecoveryDataDoesNotBecomeIdle` assert no unauthorized clearing/overwrite and unchanged outbox rows.

- [x] **Step 2: Write historical decode RED tests and run.**

`historicalStoreAddsRecoveryDataWithoutEntityReplacement` opens an old on-disk store containing AccountAccessState/inFlightUncertain/outbox using historical schemas; all identity/counts/uncertainty survive. `legacyKeychainDecodesWithoutInstallBinding` and `atomicKeychainValueContainsCredentialsAndBinding` assert old data succeeds and one replacement holds exact new token+binding. Package: `rtk proxy swift test --disable-sandbox --filter 'RefreshRecoveryStoreTests|SchemaMigrationTests|KeychainSessionStoreTests|AccountSessionLeaseTests'`.

- [x] **Step 3: Implement additive persistence.**

Preserve existing entity names and historical schema membership/checksums. Persist expected version/fingerprints before invoking Keychain save, then validate gate/current realm/exact logical attempt/current fence under lock and atomically install value plus binding. Complete SQLite only for exact bound credentials. Use new fence after restart; retain checkpoint on locked Keychain or save failure. Corrupt optional data throws sanitized recovery error and never uses AccountAccessState's legacy idle decode fallback.

- [x] **Step 4: Run GREEN.**

Repeat command with independently opened contexts/coordination lock; update literal Makefile test count. Do not declare old-store safety from memory-only container tests.

- [x] **Step 5: Commit scoped task files:**

`feat(sync): persist resumable credential installation checkpoints`.


### Task 6: Resume recoverable attempts and schedule durable acknowledgements

**Files:** Create `Packages/BabyTracker/Sources/BabySync/RefreshAcknowledgementCoordinator.swift`, `RefreshRecoveryDiagnostics.swift`, `Packages/BabyTracker/Tests/BabySyncTests/RefreshRecoveryTests.swift`; modify `SessionRefresher.swift`, `FamilySessionRenewer.swift`, `AccountCoordinator.swift`, `FamilySyncEngine.swift`, `FamilySyncHTTPClient.swift`, `SyncCoordinator.swift`, `Apps/BabyTrackerApp/AppEnvironment.swift`; extend `SessionRefresherTests.swift`, `FamilySessionRenewerTests.swift`, `FamilySessionRestoreTests.swift`, `SyncCoordinatorTests.swift`.

**Interfaces:**
- Add an explicit recovery initializer/dependency to SessionRefresher: `init(store:keychain:leases:endpoint:recovery:now:)` with `store: BabyStore`, `keychain: any SecureSessionStore`, `leases: any FencedLeaseStore`, `endpoint: ConfirmedEndpoint`, `recovery: any SessionRecoveryTransport`, `now: @escaping () -> Date = Date.init`, preserving existing legacy closure initializer for historical tests/callers.
- `RefreshAcknowledgementCoordinator.retryPending(accountKey: AccountSessionKey, familyKey: FamilySessionKey?) async` loads current credentials and durable ack state, coalesces concurrent calls and uses a fresh fenced lease. Retry delays 1,2,4,… capped at 300 seconds; saturating count, one outstanding request per account; transport failure leaves nextRetryAt persisted.
- Ordinary HTTP auth failures retain the `credentialVersion` used by the request in a new `CredentialBoundAuthFailure: Error` wrapper with `underlying: HTTPClientError` and `credentialVersion: UUID`. A bounded retry reuses the original mutation command IDs and request body; it never resends a consumed parent refresh token.
- Diagnostics produce `RefreshRecoveryEvent { phase: Phase; reason: Reason?; durationMilliseconds: Int; retryCount: Int; correlation: UUID }`, injected sink `(RefreshRecoveryEvent) -> Void`; enums bound values, per-attempt ephemeral correlation only. No stable key/URL/token/payload serialization.

- [x] **Step 1: Write failing recovery tests.**

- Strengthen existing `lostSuccessfulRefreshReplyDoesNotEndTheYearLongSignIn`: exact retry uses same parent/attempt/proof, installs returned child, checkpoint becomes ackPending and ordinary mutation succeeds.
- `lateConsumedParentResponseRetriesInstalledChild` pauses an ordinary request after loading parent credentials, completes refresh and durable bound child install, then delivers the parent's `session_revoked` response; assert one retry with the verified current child, no Apple proof and the original mutation applied exactly once. `currentCredentialRevocationStillRequiresProof` rejects that retry/current credential and asserts the sign-in action remains terminal. This covers a source-backed race, not the unobserved incident trigger.
- `crashMatrixResumesExactInstall` covers reply decode/pre-Keychain/post-Keychain/pre-SQLite/pre/post-ack with durable reopen and original expected version. `leaseExpiryRetriesUnderNewFence`, `threeProcessRenewalHasOneLogicalAttempt`, `freshSignInWinsOverStaleResponse`, `lockedKeychainRetainsAttempt`, `divergentBindingFailsClosed`, `promotionKeepsAttempt` assert exact credentials/outbox/head invariants.

- [x] **Step 2: Add compatibility RED tests and run.**

- `validBoundLegacySessionUpgradesAtNextRenewal` needs no Apple proof.
- `missingHistoricalContinuityUsesLegacy` preserves valid session and records `recovery_unavailable_missing_device_binding`; no fabricated authority. `pendingRecoveryNeverFallsBackOnProbeOrDowngrade`, `probeMismatchSendsNoCredentials`, `legacyUncertainStillRequiresProof`, `legacy429AndProvenUnsentRemainSafe`, `unknownRecoveryErrorRetainsAttempt`, `expiredRecoveredAccessRefreshesChild`, `ackFailureDoesNotBlockSync`, `successorClearsOnlyObservedSupersededAck` pin behavior. Package: `rtk proxy swift test --disable-sandbox --filter 'RefreshRecoveryTests|SessionRefresherTests|FamilySessionRenewerTests|FamilySessionRestoreTests|SyncCoordinatorTests'`.

- [x] **Step 3: Implement state machine.**

Probe origin/instance/epoch for new attempt (`ConfirmedEndpoint` has no epoch; compare ServerInfo.recoveryEpoch explicitly with current credentials), persist chosen mode before send; stored recovery mode always retries recovery only. Awaiting old Keychain recovers; prepared exact new Keychain completes SQLite without another parent exchange. Supply the bound account ID as transport expectedAccountID; match account/device/realm/epoch/attempt/parent/expected versions and fingerprints before every install/completion. Preserve continuity when refresh omits proof. Background ack uses current active child/descendant; access_expired renews or defers; completed installation allows normal sync regardless of ack. Restore invokes resume before ordinary expired-token renewal. On an ordinary request's terminal auth response, compare the sent version with the current gated credentials. Retry once only if the same account/device/origin/instance/epoch has a newer credential version proved by the completed durable install binding; preserve original idempotency IDs. If the corresponding recoverable attempt is still pending, reconcile it first rather than treating an obsolete parent rejection as current revocation. No arbitrary version change, stale fence, different realm/epoch or unverified Keychain item justifies retry or clearing a terminal state. Current credential rejection still invokes existing fresh-proof behavior.

- [x] **Step 4: Run GREEN and privacy checks.**

Repeat command, bump test count. Tests capture diagnostics and assert only declared enum/counter/ephemeral fields, no known synthetic tokens/identifiers/care payloads. No token fingerprints in diagnostics. Keep app/widget/intent composition on same shared lease/store paths.

- [x] **Step 5: Commit scoped task files:**

`feat(sync): resume interrupted refresh and acknowledge installs`.



#### Task 6 interface reconciliation

- Extend markAccountSignedIn with defaulted credential-install callback held under existing non-reentrant coordination lock; production Keychain save and SQLite clear share it.
- Retain one predecessorAcknowledgement (optional JSON-only AckRetry) through awaiting/prepared successor; validate exact superseding binding against completed checkpoint/current row, same identity/epoch and predecessor expectedVersion=new parentVersion. Clear only its own observed ACK or successful durable successor completion; no model change.
- ACK update/clear require explicit observed binding, stale observation cannot clear newer state; test predecessor/current race and failed successor retention.
- Atomic currentSessionCredentials accessor defaults nil for legacy providers; actual Keychain provider loads credentials/version once and binds identity to same immutable key. Durable verifier includes current row/gate/realm and exact atomic binding/fingerprints even after ACK clear. Retry callback returns verified credentials used directly for one retry of original bytes/IDs; no bool/reload race or invented version.

CredentialBoundAuthFailure carries default-false requiresFreshProof classification; terminal renewal errors set it explicitly, ordinary sync keeps its code policy, and coordinator honors it after the current-version/open-gate guard.

Task 6 process proof uses the test-only BabyStoreLeaseProbe executable with a BabySync dependency and synthetic durable recovery action across three real processes; it must remain outside app resources. Real TLS/server lifecycle remains Task8.

Task 6 integration reconciliation: legacy begin/finish and ordinary-sync guards must use fresh contexts under the existing shared coordination lock. Fresh sign-in persists a closed gate before Keychain replacement, reopening only after matching SQLite reset succeeds; preserve queued records on failure and verify cached-context stale responses cannot authorize installation.

### Task 7: Put actionable session recovery status on Home

**Files:** Modify `Packages/BabyTracker/Sources/SettingsFeature/{FamilySyncSettingsModel.swift,FamilySettingsViewModel.swift,Localizable.xcstrings}`, `Packages/BabyTracker/Sources/HomeFeature/{HomeView.swift,Localizable.xcstrings}`, `Apps/BabyTrackerApp/RootView.swift`, `Apps/BabyTrackerApp/UITestFamily.swift`, narrow fixture-only `Apps/BabyTrackerApp/AppEnvironment.swift`; create `Packages/BabyTracker/Sources/HomeFeature/HomeSyncStatus.swift`, `Packages/BabyTracker/Tests/HomeFeatureTests/HomeSyncStatusTests.swift`, `UITests/BabyTrackerUITests/HomeSyncRecoveryUITests.swift`; extend SettingsFeature tests and UI test support.

**Interfaces:**
- Home produce `HomeSyncStatus: Equatable { case hidden; case syncing; case current(Date); case pendingDelivery(Int); case renewing; case offline(Int); case needsSignIn(Int); case recoveryFailed(String) }` and HomeView initializer optional `syncStatus: HomeSyncStatus = .hidden`, `onSignIn: (() async throws -> Void)? = nil`.
- Settings model exposes status/pending-delivery facts without HomeFeature types; RootView maps existing engine status and `store.pendingOutbox(in: familyKey.realmID).count` to HomeSyncStatus and passes `familyModel.signInWithApple` action. Reuse public localized `FamilySyncStatusCopy.signInAgain` and `.signInButton` through app composition, or shared BabyUI copy if needed; avoid duplicate English literals in view.

- [x] **Step 1: Write failing presentation tests.**

- `signInRequiredKeepsQueuedEntries` asserts status/action plus unchanged persisted outbox and family realm;
- `emptyPullDoesNotHidePendingDelivery` asserts this phone's count outranks current/date; `interruptedRenewalIsRetryable`, `ackPendingDoesNotLookSignedOut`, `signInReturnsToSameFamily` pin action and semantics. Verify all existing supported locales supply queued-copy localization, no whole-family success claim.

- [ ] **Step 2: Add UI RED fixture and run.**

New opt-in `-uiTestHomeSyncNeedsSignIn` fixture is created by this task (does not exist today); synthetic queued entry and terminal status, no real Apple network. `HomeSyncRecoveryUITests.testHomeShowsSignInAndRetainedQueue` checks persistent banner, accessible action, queued explanation and same-family restoration. Package: `rtk proxy swift test --disable-sandbox --filter 'HomeSyncStatusTests|FamilySettingsViewModelTests'`; root: `rtk proxy xcodebuild test -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' -only-testing:BabyTrackerUITests/HomeSyncRecoveryUITests`.

- [x] **Step 3: Implement Home presentation.**

Persistent actionable status near logging; exact title `Sign in again to keep syncing`, action `Sign in with Apple`, queued message `Queued entries remain on this phone until syncing resumes.` Add localized accessibility identifiers/labels and usable long-text layouts. No deletion/reset on terminal auth; temporary recovery/offline distinct from needsSignIn. Sanitize actionable recovery-failure reason, never raw server error text.

- [ ] **Step 4: Run GREEN and visual gate.**

Repeat focused commands, bump literal test count; inspect English/Ukrainian and accessibility text sizes via existing simulator harness. Successful sign-in restores same family and resumes outbox delivery. Coordinate shared FamilySyncEngine/status edits with companion plan; preserve existing server-sync semantics.

- [x] **Step 5: Commit scoped task files:**

`feat(home): show persistent family sync sign-in recovery`.


Task 7 source review is clean after the fixture-isolation fix (`35bbb61`); package gate passes 1169 tests and the UI target builds. Steps 2 and 4 remain open for actual XCTest/English-Ukrainian large-text interaction and visual acceptance: the owned simulator stalled at Waiting on System App before XCTest. Carry these acceptance gates to Task 8 and the companion final gates; no UI pass is claimed.

### Task 8: Prove real protocol recovery and run complete acceptance gates

**Files:** Create `Server/internal/testkit/refresh_recovery_faults_identitytest.go`, `Server/internal/httpapi/refresh_recovery_faults_identitytest.go`, `Packages/BabyTracker/Tests/FamilySyncClientE2ETests/RefreshRecoveryE2ETests.swift`; modify `Server/scripts/protocol-e2e.sh`, `protocol_e2e_test.go`, create `Server/scripts/refresh-recovery-e2e.sh`, `Server/cmd/protocol-e2e-client/{main.go,main_test.go}`, `Server/tests/protocol/reauth_test.go`, existing `TwoCaregiverSyncE2ETests.swift`; add release-build exclusion counterpart/test as needed. Add narrow auth/security/main identitytest-only boundary plumbing or test-only adapters at actual head CAS/projection boundaries, with release no-op/exclusion counterparts and tests; extend only the E2E test helper/backend interfaces needed for persistent credentials and separate Swift process phases. Record final evidence in existing tracker/verification document during execution. Actual recovery-enabled composition exposed a tagged-401 integration gap; narrowly modify `Packages/BabyTracker/Sources/BabySync/FamilySyncEngine.swift` and its tests to preserve the existing single activation renewal with current-version/open-gate checks and terminal classification. Add a real-HTTP joiner regression and stale/current controls, without weakening protocol assertions.

**Interfaces:**
- New identitytest-only server `RecoveryFaultPoint` enum: before authority commit, after authority commit, after projection. Client after-response-decoding and installer faults belong in test-only Swift transport/backend/process phases; the server cannot observe client decoding. Server hook `RecoveryFaults.FailOnce(point RecoveryFaultPoint, attemptID string) error` is injected only into synthetic identitytest composition; runtime routes/flags cannot enable it in release builds. HTTP-only hooks cannot distinguish inner commit/projection phases: instrument the actual boundary via narrowly owned identitytest glue/adapters while keeping normal fences, CAS and projection semantics intact.
- Create disposable `refresh-recovery-e2e.sh` composition using the existing protocol runner ownership/cleanup/TLS rules: run hosted identitytest and self-hosted filesystem authority modes separately, never infer annual/filesystem proof from the existing hosted-only runner. Add E2E process phases `recovery-bootstrap`, `recovery-verify` to existing Go client (deliverable, not existing flags), persisting synthetic child expectation in owned temp state. Extend existing runner to coordinate app/client restart and one-shot lost replies. Swift E2E uses real HTTPIdentityClient/renewer/store/Keychain backend and TLS; process crash checkpoints run with reopened disk store and a persisted test Keychain backend, not in-memory state alone. Existing Phone.relaunched stays in one process; add a test-only persistent KeychainBackend and shared real composition initializer, then launch separate Swift bootstrap/verify processes with owned phase/state input. Go recovery-bootstrap/recovery-verify and Swift phase inputs/state are new Task8 deliverables, not pre-existing flags. Both server modes mount an owned isolated maintenance DSN secret, verify its principal and cleanup privileges, attach production RecoveryReadiness and start the actual recovery-gated cleanup worker; assert capability/routes separately from /readyz.

- [x] **Step 1: Write failing E2E assertions.**

`TestRecoveryBlackBoxFaultMatrix` and `recoverableRefreshAcrossServerAndClientRestart` require the same access/refresh child and, once durably install-prepared, the same expected credential version, one child/receipt, no reuse revocation, original deadline, owner subsequent mutation accepted and caregiver reads exact record once. Cover precommit/postcommit/postprojection/decode faults, installer crash boundaries, active descendant ack, repeated/lost ack, child-expired rejection and old/new compatibility. Print a final `refresh-recovery proof: completed` only after all assertions.

- [x] **Step 2: Run RED.**

Root `rtk make server-protocol-e2e` and new runner `rtk proxy bash Server/scripts/refresh-recovery-e2e.sh` (created in this task); require new completion marker absence/failure before implementation; E2E tests must not quietly pass by returning early without server variables.

- [x] **Step 3: Implement deterministic test-only hooks and runner.**

Use synthetic accounts only, runner-owned volumes, existing identitytest adapters and pinned TLS. Restart actual app/client processes; SQL count/commitment proof accompanies actual recovered Swift install and next ordinary sync. The dedicated recovery runner requires both the new recovery marker and existing two-caregiver step18 in each mode, after supplying the actual recovery fixtures/Go and Swift phases. The original hosted protocol runner retains its existing proof unless it also supplies those phases; never add a marker grep to an unprepared legacy run. Ensure release build cannot register fault route/header/env switch. No new runner options may be used until this task creates and tests them.

- [ ] **Step 4: Run GREEN acceptance once task code stabilizes.**

Root: `rtk make test` (exact updated literal), `rtk make server-test`, `rtk make server-build`, `rtk make server-protocol-e2e`, `rtk proxy bash Server/scripts/refresh-recovery-e2e.sh`, `rtk make build`, `rtk make test-ui`; Server: `rtk proxy go vet ./...`. Check migrate compatibility/digests, role separation, no unapplied-migration rewrites, full race suite and release hook exclusion. Every task that changes migration 25 after Task 2 refreshes only its still-unapplied digest in compatibility.json and reruns compatibility tests. Once applied outside the disposable test DB, freeze the migration. Inspect full branch diff and request independent whole-branch review. Report command outcomes/environment blockers separately; simulator/build evidence is not physical device/network/lifecycle acceptance.

- [x] **Step 5: Commit scoped task files:**

`test(sync): verify recoverable refresh across process failures`.


Task 8 source and task review are complete (`f13bd9a`, plus scoped stub-mode fix `425ddcf`). All runnable acceptance gates passed at `f13bd9a`; the wrapper fix passed committed-head script/race checks and scoped re-review. Step 4 remains open only for runtime UI acceptance, carried to companion final gates. Whole-recovery review is the next gate before performance implementation.

## Handoff and integration boundary

This plan is a review artifact. After parent self-review and user plan approval, execute with the already selected subagent-driven workflow. Sequence Tasks 1–8; do not run concurrent edits to migration 25, auth repository, readiness main.go or shared Swift status files. Companion performance work shares readiness and status composition: integrate interfaces deliberately, then re-run combined gates. Deployment/schema-22 upgrade/phone installation/push remain separate user decisions, preserving currently signed-in credentials and data.
