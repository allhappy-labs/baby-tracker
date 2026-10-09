# Private Read-only MCP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let authorized local desktop MCP clients read a family's babies, bounded history and deterministic summaries over private Tailscale HTTPS.

**Architecture:** Mount an opt-in stateless MCP adapter on the existing Go server. Use independent authority-backed integration grants and a family-read module with tenant-scoped read-only transactions. Share admission with sync while giving MCP a separate bounded database pool.

**Tech Stack:** Go 1.26.2, official MCP Go SDK v1.8.0 (verify/pin at execution), PostgreSQL 17, pgx v5, existing security ledger/operator CLI, Streamable HTTP.

**Spec:** [Approved design](../specs/2026-10-03-single-node-capacity-mcp-design.md), Track B; [primary-source research](../../research/2026-10-03-mcp-server.md). Depends on Tasks 1–5 and the baseline evidence from [Track A](2026-10-03-single-node-capacity.md).

## Global Constraints

- MCP is disabled by default. Only `list_babies`, `get_history`, `get_summary`; no mutation/admin/export/raw-SQL tools, public ingress, OAuth interoperability claim, subscriptions or server-side LLM.
- Credentials are MCP-specific, owner-account/family/instance/epoch bound, revocable and valid for 90 days. Explicit replacement/revocation, no refresh token. Tailscale connectivity alone grants no family access.
- Every data call rechecks live authority, current membership, family eligibility and scope. No fabricated device/session principal, arbitrary caller-selected family, authority bypass or cached authorization across leases.
- History: `[start,end)` at most 31 days, default 50/max 100 entries, maximum 256 KiB encoded result. Summary range at most 31 days; named IANA timezone. Distinguish missing values, completed records and running timers.
- Limits: 32-KiB body, 256-KiB full serialized tool-result response, five-second total request deadline, four in-flight MCP data calls globally and one per credential; account/family aggregate quotas also apply.
- Keep phone-sync acceptance from Track A; mixed-load sync p95 must not regress >10% against matched sync-only load. No claim of node capacity or actual desktop-over-Tailscale compatibility until those checks run.
- Use an isolated implementation worktree, preserve unrelated changes, prefix shell commands with `rtk`, and pin dependencies. No deployment or production credentials/data in tests.
- Commands below run from repository root unless stated otherwise. Proposed interfaces, schema and flags are deliverables to implement, not pre-existing capabilities. Allocate the next migration number at execution; `000028` is free at the planning base but is not reserved against other work.

## Review Focus

1. Operator issuance response/output is lost after durable grant creation: exact retry cannot mint a different secret or duplicate grant (Tasks 1 and 3).
2. Owner changes, account membership is removed, or epoch changes after HTTP authentication: the actual read still denies stale authority (Tasks 1–2).
3. A record is inserted/edited/deleted between pages: return an explicit restart result instead of silently skipping or duplicating history (Task 4).
4. DST transitions, overlapping sleep and composite sessions: totals match explicitly defined recorded-event semantics without double counting parent/child rows (Task 5).
5. Slow/oversized/chunked requests and many credentials for one family: bounded resources, cancellation and reserved phone-sync capacity (Tasks 6–7).

## File and interface map

- `Server/internal/security/integration_*.go`: grant events/state/projection, operator preparation and guarded read transactions. Keep `access.Principal` phone semantics unchanged.
- `Server/internal/operator/integration.go` and `cmd/baby-tracker-admin/mcp_cli.go`: issuance/list/revocation with existing two-phase operator confirmation.
- `Server/internal/familyread/`: typed query validation, canonical history, deterministic summaries and stateless pagination. This is the shared data module; MCP owns no SQL.
- `Server/internal/mcpapi/`: SDK transport, tool registration, error mapping, limits and private headers.
- `Server/internal/config/mcp.go` and `cmd/baby-tracker-server/mcp.go`: opt-in composition and dedicated pool/admission wiring.
- One additive migration for grant projection/indexes and its exact compatibility/privilege manifest changes. Preserve all historical migration bytes.

## Task 1: Durable integration grant authority

**Files:** Create `Server/internal/security/integration_events.go`, `integration_events_test.go`, `integration_prepared.go`, `integration_prepared_test.go`; modify `event.go`, `state.go`, `authority_view.go`, `authority_successor_test.go`, `prepared.go`, `projector.go`, `event_registry_test.go`; create `Server/migrations/000028_mcp_integrations.sql` (reconcile number first), `Server/internal/database/integration_schema_test.go`; update `Server/internal/migrate/compatibility.json` with the existing manifest generator.

**Interfaces:** `IntegrationView{ID,AccountID,FamilyID,ServerInstanceID,Name,Scope,Verifier,Status string; RecoveryEpoch uint64; CreatedAt,ExpiresAt time.Time}`; detached `AuthorityView.Integration(id string) (IntegrationView,bool)`. New event kinds `integration_granted`, `integration_revoked`; scope exactly `family:read`. `OperatorIntegrationInput` includes command/operator/grant/family/account IDs, verifier/name, instance/epoch, expiry, expected security revision, policy/evidence/reason and confirmation expiry, following existing operator input conventions.

- [ ] Write replay/preparation tests asserting valid grant expiry equals issuance time +90 days; empty/foreign family, non-owner account, unknown scope, noncanonical ID/digest, expired confirmation, stale revision and mismatched retry are rejected. Exact command retry returns one durable result; no plaintext secret occurs in canonical events, state/projection output or logs.
- [ ] Run `rtk proxy env BT_TEST_PACKAGES='./internal/security ./internal/database ./internal/migrate' bash Server/scripts/test-db.sh`; capture intended red failures.
- [ ] Implement grant creation/revocation as private prepared operator commands under the existing exclusive authority fence, including durable receipts, replay, successor cloning, projection and recovery. Store only `SHA256("babytracker-mcp-v1\x00" || grantID || secretBytes)` as lowercase hex, with a cryptographically random 32-byte secret. Wire every state-copy/export/replay path; unknown old binaries must fail closed on new event kinds. Grant metadata projection is not an alternative source of authorization.
- [ ] Rerun tests green, including warmed-cache corruption, projection failure, recovery rebuild and historical event compatibility. Assert database roles cannot modify grant projection except through the intended projector path. Document that rollback after new event commits needs compatible code, not just an old image. Commit: `feat(security): add revocable read-only integration grants`.

## Task 2: A dedicated guarded read path

**Files:** Create `Server/internal/security/integration_read.go`, `integration_read_test.go`; modify security construction/composition as needed and add focused family-policy tests. Reuse `Server/internal/tenant/context.go`.

**Interfaces:** `CheckIntegration(ctx context.Context, credential string) (IntegrationIdentity,error)` for transport discovery, where `IntegrationIdentity{GrantID,AccountID,FamilyID string; ExpiresAt time.Time}` is informational, never an authorization capability. `WithIntegrationRead(ctx context.Context, credential string, run func(context.Context,pgx.Tx,access.Grant) error) error` is the only data guard. The service receives a dedicated MCP pool through its constructor; it is never passed by a request caller.

- [ ] Write tests for invalid/oversized token, constant-time verifier comparison, expiry at the exact deadline, revoked grant, removed account/membership, ownership transfer, ineligible/deleted family, foreign instance, restored epoch and authorization quarantine. Authenticate first, revoke, then call `WithIntegrationRead`: assert denial and zero callback invocations. Attempt SQL mutation inside the callback: assert a read-only transaction error and zero durable writes.
- [ ] Run `rtk proxy env BT_TEST_PACKAGES='./internal/security ./internal/tenant ./internal/family' bash Server/scripts/test-db.sh`; confirm red.
- [ ] Implement strict `btmcp1.<grantUUID>.<base64url-32-byte-secret>` parsing. Under one active shared fence lease, load validated authority and require the bound account still be the active family owner with eligible membership/entitlement. Begin a read-only repeatable-read transaction on the dedicated pool; set tenant-local account/family from the authority decision, run the callback, commit/rollback, invalidate the lease and release. Expiry and cancellation bound the transaction deadline. Reuse the existing family policy rather than assigning a fake phone session. New reads after revocation commits fail; already-authorized reads keep the existing shared-fence serialization behavior.
- [ ] Run tests green and `-race` through the database harness. Check old app-token routes reject `btmcp1` credentials; MCP rejects phone tokens. Ensure nested/reentrant and escaped callback use cannot borrow an ended lease. Commit: `feat(security): guard integration reads with live family authority`.

## Task 3: Operator issuance, listing and revocation

**Files:** Create `Server/internal/operator/integration.go`, `integration_test.go`, `Server/cmd/baby-tracker-admin/mcp_cli.go`, `mcp_cli_test.go`; modify `operator/security_authority.go`, `config/operator.go`, admin composition and tests; document grammar in `Server/README.md`.

**Interfaces:** Dedicated `IntegrationCommands` methods `PrepareIntegration(ctx, proposal) (Confirmation,error)`, `IssueIntegration(ctx, proposal, confirmation) (security.CommandResult,error)`, `RevokeIntegration(ctx, proposal, confirmation) (security.CommandResult,error)`, `ListIntegrations(ctx,familyID string) ([]security.IntegrationView,error)`. `IntegrationProposal` mirrors Task 1 input plus the existing operator audit fields. A mounted `BT_OPERATOR_INTEGRATION_POLICY_ID_FILE` enables this command family; no direct-value secret fallback.

- [ ] Write CLI tests for `mcp issue prepare|commit`, `mcp revoke prepare|commit`, `mcp list`; assert no raw credential in stdout/stderr, invalid/missing policy denies, symlink/overwrite output refuses, restrictive permissions are enforced, and exact retry after lost acknowledgement commits only one grant.
- [ ] Run `rtk proxy env BT_TEST_PACKAGES='./internal/operator ./internal/config ./cmd/baby-tracker-admin' bash Server/scripts/test-db.sh`; establish red.
- [ ] Generate the grant ID and random secret during `issue prepare`, persist it once with exclusive-create mode 0600 at `--secret-output`, and return only nonsecret proposal/confirmation metadata. Bind the verifier into that proposal. `issue commit --secret-file` reads the same protected file, checks its verifier against the confirmed proposal and commits through Task 1. It must never silently regenerate or overwrite a secret on retry. On failure, the file remains an inactive credential until the exact commit succeeds. `mcp list` emits metadata only (omit verifier); revoke requires matching family/grant and the normal confirmation/receipt checks. Replacement is explicit issuance plus explicit revocation of the old grant.
- [ ] Run green tests, including before-commit/after-commit failure recovery and expiration while queued behind the fence. Confirm secret-file paths, not secret values, are the only CLI arguments. Commit: `feat(admin): manage private MCP credentials`.

## Task 4: Bounded, consistent canonical family history

**Files:** Create `Server/internal/familyread/service.go`, `types.go`, `history.go`, `cursor.go` and corresponding tests. Add necessary range-query indexes to the Task 1 migration before it is published; if already applied outside disposable tests, append a new migration. Reuse canonical row decoding from `Server/internal/records/` through a focused read helper if needed.

**Interfaces:** `Guard` contains Task 2's `WithIntegrationRead` signature. `New(guard Guard, cursorKey [32]byte, now func()time.Time) *Service`; `ListBabies(ctx,credential string) (BabiesResult,error)`; `History(ctx,credential string,q HistoryQuery) (HistoryResult,error)`. `HistoryQuery{BabyID string; Start,End time.Time; Kinds []string; Limit int; Cursor string}`. `BabiesResult{Revision uint64; Babies []protocol.CanonicalEntity}`; `HistoryResult{Revision uint64; Entries []HistoryEntry; NextCursor string}`; `HistoryEntry{Kind,ID string; Start time.Time; End *time.Time; Entity protocol.CanonicalEntity}`. Babies use existing canonical baby records and family limits.

- [ ] Write database tests for default 50/max 100, >31-day rejection, malformed range, missing/foreign baby, tombstones, simultaneous timestamps and allowlisted kinds. Seed 101 entries to prove pagination. Read page one, edit/delete/insert, then read page two: assert `history_changed` and no mixed-revision page. Oversized single records return `result_too_large`; never silently omit data.
- [ ] Run `rtk proxy env BT_TEST_PACKAGES='./internal/familyread ./internal/records ./internal/database' bash Server/scripts/test-db.sh`; confirm red.
- [ ] Implement indexed tenant-scoped queries in the guard's repeatable-read transaction. Stable order `(eventStart,kind,id)`; continuation encodes position, exact filters, grant/family/baby binding, instance/epoch, family data revision and 15-minute expiry, authenticated with a dedicated cursor key and domain label. Read the family sequence within the same transaction and reject continuation if changed. No persisted sync snapshot, journal drain, family write lock or materialized full-family export.
- [ ] Use root care entities as history entries, preserving embedded feed/sleep segments. Point events and completed feeding sessions are selected by start instant in `[start,end)`; completed sleep intervals intersecting the range are included for clipped duration summaries; growth profile is metadata, not an event. Running timers are returned separately from completed history totals. Document these predicates in the tool schema. Enforce body-independent result budgeting before encoding the MCP envelope; Task 6 handles final-envelope overhead.
- [ ] Run green tests and bounded `EXPLAIN (ANALYZE, BUFFERS)` on realistic synthetic histories to verify chosen indexes and no unbounded scan for a small page. Commit: `feat(familyread): add bounded canonical history queries`.

## Task 5: Deterministic summaries with defined interval semantics

**Files:** Create `Server/internal/familyread/summary.go`, `summary_types.go`, `summary_test.go`, `summary_sql.go`; extend shared query validation.

**Interfaces:** `Summary(ctx,credential string,q SummaryQuery) (SummaryResult,error)`; `SummaryQuery{BabyID string; Start,End time.Time; Timezone string}`. `SummaryResult` includes the covered interval/timezone, revision, counts by care kind, known bottle millilitres and missing-volume count, completed sleep seconds, supplement counts, measurement observations, and separately bounded active-timer facts. All counts/units and exclusions are explicit typed fields.

- [ ] Write fixture tests for a two-segment feed counting once, mixed known/unknown bottle amounts, midnight clipping, deleted child/records, unfinished timers, exact range endpoints, and both Zurich DST transitions. Assert a 23-hour day uses its actual elapsed seconds and parent/child sleep rows are not both summed. Missing measurements/volumes remain unknown/absent, not zero-valued observations.
- [ ] Run `rtk proxy env BT_TEST_PACKAGES='./internal/familyread' bash Server/scripts/test-db.sh`; confirm red.
- [ ] Implement a bounded aggregate query in one authorized read transaction, using the same canonical predicates as Task 4. Aggregate completed sleep by union of clipped canonical sleep intervals per baby/day so overlapping records do not inflate sleep time; disclose this definition. Feeding count and known bottle volumes use completed root sessions starting within the range. Supplements count actual logged doses; measurements remain dated observations with units rather than meaningless sums. Enforce the 31-day and five-second limits, validate IANA zones, and embed Go timezone data for slim containers. Never compute a full summary by walking client-visible pages or silently truncate totals.
- [ ] Run green tests, compare summaries against a separately calculated canonical fixture oracle, and inspect bounded query plans. Use finite numeric validation and stable output precision. Commit: `feat(familyread): compute bounded care summaries`.

## Task 6: SDK transport and private read-only tools

**Files:** Create `Server/internal/mcpapi/handler.go`, `tools.go`, `errors.go`, and tests; update `Server/go.mod`/`go.sum`; create `Server/internal/config/mcp.go`, `mcp_test.go`; add `MCPHandler http.Handler` to `httpapi.Options` and mount it conditionally in `router.go`.

**Interfaces:** `NewHandler(options Options) (http.Handler,error)` consumes the family-read service, `CheckIntegration`, Track A `admission.Controller`, existing limiter, deadline/byte limits and explicit allowed origin. `LoadMCP(getenv func(string)string) (MCPConfig,error)` consumes `BT_MCP_ENABLED`, `BT_MCP_DATABASE_URL_FILE`, `BT_MCP_CURSOR_KEY_FILE` (32 random bytes in protected file). Disabled mode needs no extra secrets/pool and `/mcp` returns 404.

- [ ] Verify official SDK v1.8.0 metadata/checksum and Go compatibility, pin it, and record supported protocol versions. Write tests using the SDK client for discovery/lifecycle, list/call and selected legacy negotiation. Assert unknown versions/methods receive protocol errors, tools have typed schemas/read-only annotations, and mutation/admin tool names never appear.
- [ ] Run `rtk proxy go test ./internal/mcpapi ./internal/config ./internal/httpapi` from `Server`; record red before implementing the handler behavior.
- [ ] Implement stateless Streamable HTTP with JSON responses and no resources/subscriptions/tasks. Authenticate every request, including discovery/list; each data tool uses Task 2 again to close the authenticate/revoke race. Reject duplicate/malformed Authorization headers. Set `Cache-Control: no-store`, validate supplied Origin/Host against the canonical private origin (allow missing Origin for native clients), disable CORS access from arbitrary websites, and trust proxy metadata only via existing configured proxy policy.
- [ ] Enforce 32-KiB bodies including chunked input, an actual read deadline that interrupts slow body reads (context cancellation alone is insufficient), five-second end-to-end deadline from handler entry and 256-KiB complete encoded tool response. Account for JSON escaping, structured/text duplication and envelope overhead: shrink history page before choosing its cursor or return `result_too_large` if one entity cannot fit. Do not truncate a summary. Map auth failures to 401/403, quotas to 429, global saturation to 503, query/data failures to typed MCP tool errors with no SQL/secret leakage. Treat care notes as data fields, not instructions.
- [ ] Rerun green with disconnect cancellation, malformed JSON, oversized tool arguments/output, timer data, unauthorized tool listing and old mobile-token rejection. Document the private bearer profile explicitly; publish no fake OAuth metadata. Commit: `feat(mcp): expose private read-only family tools`.

## Task 7: Composition, quotas and shared-load isolation

**Files:** Create `Server/cmd/baby-tracker-server/mcp.go`, `mcp_test.go`, `mcp_identitytest_test.go`; modify `main.go`, `config/config.go`, `config/limits.go`, `limits` scope registry, Track A database budgets/telemetry and capacity workload support. Update `Server/compose.yaml` and Unraid override templates without applying them.

**Interfaces:** The dedicated MCP pool uses runtime tenant permissions and read-only repeatable-read transactions; it does not replace the main runtime pool or bypass the security guard. Its maximum is four connections and it counts in the aggregate pool budget. Per-credential concurrency is one; use a bounded idle-evicting keyed admission registry. New fixed quota scopes: integration 30/minute, account 60/minute, family 120/minute; metadata queries also consume quota after successful authentication.

- [ ] Write composition tests that disabled mode opens zero MCP connections and registers no route; enabled mode requires an explicit valid aggregate budget and secrets. Saturate four MCP reads and verify sync acquires its own runtime connections. Create many credentials for one family and assert the family quota still refuses excess calls. Canceled/error/panic requests release both global and credential admission; keyed-registry size cannot grow with arbitrary invalid tokens.
- [ ] Run `rtk proxy env BT_TEST_TAGS=identitytest BT_TEST_PACKAGES='./cmd/baby-tracker-server ./internal/mcpapi ./internal/config ./internal/limits ./internal/admission' bash Server/scripts/test-db.sh`; establish red, wire production composition, rerun green. Rate-limit reservation writes are operational metadata through the existing limiter, never care-record writes in the read module.
- [ ] Run matched sync-only, MCP-idle, MCP-read and abusive-MCP scenarios through Track A's runner. Require healthy sync p95 <1 second, <0.1% unexpected errors, zero lost/cross-family changes and <=10% p95 regression. Record actual admitted/refused MCP rate separately. Lower MCP concurrency if needed and document the final supported limits; do not improve metrics by hiding refused or timed-out requests.
- [ ] Commit: `feat(mcp): isolate read workload from phone sync`.

## Task 8: End-to-end evidence, desktop setup and rollout handoff

**Files:** Create `Server/scripts/mcp-e2e.sh`, `Server/scripts/mcp_e2e_test.go`, `docs/verification/2026-10-03-mcp.md`, `Server/docs/mcp.md`; update server README, tracker and Track A capacity receipt.

**Interfaces:** The e2e runner owns a disposable stack, uses real operator issuance and production authorization/query/SDK composition, and emits only sanitized operation counts/statuses/source identity. Desktop setup contains secret-file/header configuration instructions without a credential value.

- [ ] Add e2e cases for issue → discover/list → history/summary → revoke → deny; wrong family; grant expiry; recovery epoch; restart/replay; query cancellation; mobile mutation-route denial. Add cleanup/timeout controls and run the disposable proof after those controls pass. Include an independent client implementation or SDK protocol conformance checks so server/client shared assumptions are not the sole evidence.
- [ ] Run `rtk make server-test`, `rtk make server-build`, `rtk proxy go vet ./...` from `Server`, `rtk make server-protocol-e2e`, and `rtk proxy bash Server/scripts/mcp-e2e.sh`. Run the selected mixed-load soak. Inspect final diff and perform whole-branch review, especially grant/replay/tenant semantics; fix findings and rerun affected checks.
- [ ] Identify one installed desktop client with verified bearer-header support and document its exact version/configuration from current official docs. Validate it against a local disposable server when possible. A real desktop-over-Tailscale test on the deployed endpoint occurs only after separately authorized rollout/credential provisioning; keep this explicit acceptance box open until then. If the desired client requires OAuth, report that compatibility blocker rather than copying phone secrets.
- [ ] Prepare deployment notes with source/image identity, new migration/authority-event compatibility, protected credential files, backup/restore requirements, enable/disable procedure and rollback limitations. Disable MCP stops reads but does not erase grants; revoke when disconnecting a client. Record that a desktop may send returned data to its configured model provider despite private transport.
- [ ] Commit: `docs(mcp): document verified read-only setup and rollout`. No push, deployment, production grant issuance or node stress test is implied by local completion.

## Plan self-review

Tasks 1–3 own the new security identity end to end, including loss/replay and operator issuance. Tasks 4–5 pin range, pagination, canonical session and summary semantics. Tasks 6–7 cover real SDK behavior and bounded shared resources; Task 8 separates local proof from desktop/node acceptance. Read APIs never consume an informational transport identity as authority. New grant events create an explicit old-binary rollback constraint. No iOS UI or OAuth subsystem is added.
