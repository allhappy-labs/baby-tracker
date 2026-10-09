# Canonical Family Records and Synchronization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give two authenticated protocol clients durable, isolated, deterministic synchronization of babies, every existing record variant, shared timers, and resumable imports through the production server.

**Architecture:** Extend the Go modular monolith with typed PostgreSQL repositories, one family write boundary, durable semantic receipts, and an immutable journal whose order is commit order. Materialize bounded full snapshots transactionally for consistent pagination. WebSocket and PostgreSQL notifications carry cursor hints; hosted wake scheduling is committed with canonical changes and is consumed by Plan 4.

**Tech Stack:** Go 1.26.2, PostgreSQL 17, pgx/v5 v5.11.0, OpenAPI 3.1, standard-library HTTP/JSON/cryptography, golang.org/x/text v0.29.0, github.com/coder/websocket v1.8.15.

**Spec:** `docs/superpowers/specs/2026-09-19-family-sync-server-design.md`; read together with `docs/superpowers/plans/2026-09-20-family-sync-identity-lifecycle.md`. Planning evidence: `.superpowers/planning/full-beta-decomposition.md` and `.superpowers/planning/server-beta-architecture.md`.

## Global Constraints

- “Local-only use remains fully functional without an account.” This plan changes no Swift feature behavior.
- “The API is a checked-in OpenAPI 3.1 contract using JSON over HTTPS.”
- “All non-development connections require HTTPS, including custom servers.”
- “Every family-owned database row carries `family_id`.” Apply forced RLS, default deny, composite tenant foreign keys, and transaction-local tenant settings to every new tenant table.
- “The runtime database role is not a superuser, does not own tenant tables, and cannot bypass row policies.” Request handlers cannot obtain migration or maintenance credentials.
- “10 active caregivers per family”; “20 non-deleted children per family”. The former is owned by Plan 1; this plan enforces the latter atomically.
- “Neither is conflict authority; server acceptance order decides conflicts.” Domain and client timestamps never order writes.
- “A unique `(device_id, mutation_id)` receipt provides idempotency.” Device identity and attribution come from the authenticated session.
- P1's authenticated device continuity preserves that receipt namespace across fresh Apple proof; client-supplied device IDs without the continuity credential never select receipts.
- “The beta does not create attachment tables, endpoints, object storage, quotas, or UI.” Add only the internal BlobStore interface beside the existing metadata contract.
- “The application never auto-migrates on startup.” Add new checksum-protected migrations; preserve applied foundation bytes.
- “No Redis or Kubernetes”; no billing, Android/web client, E2EE, operator browser, audit browser, multi-region writes, or multiple active families.
- “Third-party CI actions are pinned to immutable commit SHAs.” Preserve image digests and lock Go modules in `go.sum`.
- “The following are prohibited from logs and metrics labels”: tokens, invitation secrets, baby/caregiver names, notes, record payloads, and export contents. Never label metrics with opaque tenant IDs either.
- Use `rtk` commands, Conventional Commits, focused staging, and the existing isolated worktree. Do not commit planning reports or another worker's files.

## Review Focus

- A lost outcome response followed by paged catch-up must not skip an unseen earlier change; `TestOutcomeCursorDoesNotSkipJournal` and `TestSnapshotPagesStayAtOneHighWater` in P2.3 pin this.
- Removal or restore quarantine between authorization and SQL must deny the transaction; `TestRevocationFencePreventsLaterWrite` in P2.2 and `TestSnapshotContinuationReauthorizes` in P2.3 pin this.
- An uncertain timer start must recover its exact receipt across reauthentication and never retarget a losing start; `TestLostStartCollisionCannotRetargetCommands` in P2.4 and `TestLostTimerResponseReauthRecoversReceiptThenStops` in P2.6 pin both paths.
- An owner choosing an ambiguous import candidate while that candidate is renamed/deleted must get a revision conflict, with no partial mapping; `TestOwnerResolutionRejectsChangedCandidateRevision` in P2.5 pins this.
- A process crash after data commit but before notification must preserve both data and wake work; `TestCommitBeforePublishSurvivesRestart` in P2.6 pins this.

---

## Ownership, prerequisites, and file boundaries

This is Plan 2 of four; its six tasks are P2.1–P2.6. Plan 1 owns Apple/session/lifecycle authority, entitlement, rate-limit storage, recovery quarantine, and authorization fencing. Plan 3 owns Swift DTOs, local realms/outbox/materialization, import UI, and reconnection behavior. Plan 4 owns APNs token registration/provider delivery, export/purge, full load testing, deployment, and public-beta acceptance. This plan proves protocol correctness without claiming device integration or hosted readiness.

Prefer the decomposition's newer names over the architecture map's illustrative variants: `internal/records`, `internal/importer`, `internal/realtime`, `internal/delivery`, `POST /v1/family/import/resolve`, and **GET** `/v1/sync/snapshot`. Do not introduce parallel `internal/timer`, `internal/live`, or `/v1/import/resolve` APIs.

The exact prerequisite interface is:

```go
// Plan 1 owns these definitions; import them, never duplicate them.
// internal/access
type Principal struct {
    AccountID, DeviceID, SessionID string
    RecoveryEpoch, SecurityRevision uint64
    AccessExpiresAt time.Time
    Scope string
}
type Grant struct { AccountID, FamilyID, Role string; Revision uint64 }
// internal/security
func (s *Service) WithAuthorizedTransaction(
    context.Context, access.Principal, string, string,
    func(pgx.Tx, access.Grant) error,
) error
// The two strings are familyID and action. The guard authorizes from live
// security state, fences lifecycle changes through commit, opens a runtime
// transaction, sets tenant context before tenant SQL, and commits on nil.
type SecurityInvalidation struct {
    AccountID, FamilyID, SessionID string
    Revision uint64
}
```

Store this interface in `records.Guard` for injection. `read`, `mutate_record`, `add_baby`, and `delete_baby` are the existing action values. Import candidate selection requires an owner grant obtained inside the same guard; it is a canonical-data command, not an external lifecycle ledger event. Only active `family` principals may use any endpoint here; account-only and recovery-only principals fail before data access. Family discovery uses Plan 1's authenticated one-family slot; callers never gain authority from a cursor or a payload family ID.

P1 also owns `AppleExchangeRequest`/`identity.ExchangeRequest.device_continuity:{device_id,credential}` and initial `SessionBundle.device_credential`. Fresh Apple proof may reuse a DeviceID only after validating the continuity credential's HMAC binding to the same verified account, canonical origin, and server instance. A submitted missing/invalid/cross-account continuity claim fails `device_continuity_invalid` without falling back to a new device. P3 retains the credential in Keychain across restart/reauth; if it is lost while mutations are uncertain, it surfaces `device_continuity_required` rather than replaying under a new identity. P2 accepts only the resulting authenticated `Principal.DeviceID`; it adds no arbitrary device-claim or receipt-lookup endpoint. Account/device deletion revokes continuity. Recovery-epoch rotation still follows the spec's snapshot/RPO reconciliation and is not bypassed by this same-epoch receipt recovery.

Plan 1 reserves migrations 000003–000007. The exact migration reservation for this plan is `000009_canonical_records.sql`, `000010_snapshot_staging.sql`, `000011_timer_commands.sql`, `000012_import_resolver.sql`, and `000013_delivery_pending.sql`. Before P2.2, verify the landed migration catalog matches these reservations. Resolve a conflict by renumbering an **unapplied** migration and every plan reference; never modify applied bytes.

Every migration task must update **`Server/internal/migrate/compatibility.json`**, not a `status.go` catalog. Keep one exact filename and actual SHA-256 digest for every migration 000001 through the new maximum; calculate from final file bytes and never use an invented digest or an unverified wildcard allowance. Consume P1's `migrate.GenerateManifest(fs.FS,min,max int64)` and `Server/cmd/baby-tracker-manifest/main.go` with `--min`, `--max`, `--output` flags; it rejects gaps, duplicate versions, and changes to historical bytes. Do not create a second manifest generator.

Consume P1's prepared-mode `Server/scripts/check-migration-overlap.sh` with
`--base-ref`, `--prior-ref`, `--feature-ref`, `--next-version`, `--prior-min`,
`--prior-max`, `--next-min`, and `--next-max`. Before each migration task,
capture the accepted N-1 feature head as `BT_MIGRATION_BASE_REF`. The task first
lands and reviews a real deployable compatibility-only commit containing final
immutable migration N, its complete exact schema-N privilege snapshot, overlap
scenario/tests, and manifest `[N-1,N]`, but no N-dependent production path.
Capture that commit as `BT_MIGRATION_PRIOR_REF`. A second commit implements the
feature and changes the manifest bounds to `[N,N]`; capture it as
`BT_MIGRATION_FEATURE_REF`.

The harness archives all three commits and never modifies an archive. It runs
the predecessor at schema N-1, migrates with that predecessor's admin binary,
runs the predecessor at N, then runs the feature binary at N against the same
latest security stores. Each phase proves the registered guarded read/write and
structural-denial scenario, exact schema/privilege snapshot, readiness, and
distinct build ID. Changed/omitted/unknown digests deny. The compatibility
predecessor is the artifact deployed before a real migration; an archive
patched inside the harness is accepted only for the explicitly historical
000004 transition and is never P2 evidence.

| Owning task / new migration | Prior overlap binary Min/Max | New binary Min/Max | Required exact new digest entry |
| --- | --- | --- | --- |
| P2.2 / 000009 | 8 / 9 | 9 / 9 | `000009_canonical_records.sql` and SHA-256 of its final bytes. |
| P2.3 / 000010 | 9 / 10 | 10 / 10 | `000010_snapshot_staging.sql` and SHA-256 of its final bytes. |
| P2.4 / 000011 | 10 / 11 | 11 / 11 | `000011_timer_commands.sql` and SHA-256 of its final bytes. |
| P2.5 / 000012 | 11 / 12 | 12 / 12 | `000012_import_resolver.sql` and SHA-256 of its final bytes. |
| P2.6 / 000013 | 12 / 13 | 13 / 13 | `000013_delivery_pending.sql` and SHA-256 of its final bytes. |

For each migration-owning task, execute its compatibility checkbox in two
parts. P2.2's exact sequence is:

```bash
BT_MIGRATION_BASE_REF=$(rtk git rev-parse HEAD)
# Add final 000009, complete v9 privilege snapshot, overlap tests/scenarios,
# manifest [8,9], then run the schema-8 predecessor suite.
rtk git commit -m "feat(server): prepare records schema compatibility"
BT_MIGRATION_PRIOR_REF=$(rtk git rev-parse HEAD)
```

Continue the task, run its focused/full gates, and perform the listed feature
commit with manifest `[N,N]`. Then:

```bash
BT_MIGRATION_FEATURE_REF=$(rtk git rev-parse HEAD)
rtk proxy bash Server/scripts/check-migration-overlap.sh \
  --base-ref "$BT_MIGRATION_BASE_REF" \
  --prior-ref "$BT_MIGRATION_PRIOR_REF" \
  --feature-ref "$BT_MIGRATION_FEATURE_REF" \
  --next-version 9 --prior-min 8 --prior-max 9 \
  --next-min 9 --next-max 9
```

P2.3-P2.6 use their exact versions and commit messages stated in their own
Step 5a/6b checkboxes. Do not amend either commit after the prepared-mode gate.
Thus an overlap checkbox is complete only after the later feature-commit
checkbox and its exact final command pass.

| Boundary | Files and responsibility |
| --- | --- |
| Wire and canonicalization | `Server/internal/protocol/{entities,mutations,sync,import,live,canonical,normalization,decode}.go`: typed wire contracts, lexical decoding, canonical bytes; no SQL. |
| Validation and storage | `Server/internal/records/{validation,repository,babies,feeds,sleeps,diapers,timers,child_delete}.go`: domain rules and per-kind SQL. |
| Mutation orchestration | `Server/internal/sync/{mutation,receipts,journal,service,paging,snapshot}.go`: guarded transactions, receipts, sequence, and consistent reads. Use import alias `familysync` where standard `sync` is also needed. |
| Import | `Server/internal/importer/{resolver,manifest,conflict,repository}.go`: durable staging, complete mapping, owner choices, and imported-record admission. |
| Hints and work | `Server/internal/realtime/{hub,websocket,postgres}.go`, `Server/internal/delivery/{queue,repository}.go`: bounded sockets and atomic wake scheduling. |
| Adapters | `Server/internal/httpapi/{sync,import,live}.go`; extend `router.go`, `middleware.go`, and `cmd/baby-tracker-server/main.go`: authenticate, bound, decode, call services, encode. |
| Shared verification | `Server/internal/testkit/{postgres,fixtures,protocol_client}.go`, `Server/tests/protocol/{convergence,timer,import,restart}_test.go`, `Server/scripts/protocol-e2e.sh`: isolated real PostgreSQL and two HTTP clients. |

## Frozen v1 decisions and defaults

These are implementation decisions resolving D5, D7–D9 and the numeric API portion of D12. They are concrete plan inputs, not claims that the product already implements them. Plan 4 must independently choose and measure its two-second workload/percentile gate.

### D5: compatibility and origin

Canonical server identity is an HTTPS origin only, with no path prefix, query, fragment, userinfo, or trailing slash. Remove the foundation OpenAPI `basePath` variable. Requests use `X-Baby-Tracker-Protocol: v1` and the route-specific `X-Baby-Tracker-Capabilities` set frozen in the Mandatory Review Amendments; there is no universal capability literal. JSON POST bodies contain identical protocol/capability values. GET snapshot and WebSocket use headers; the socket also requires `baby-tracker.v1`. Reject a missing/mismatching version with HTTP 400 `unsupported_protocol`; unsupported or missing required capabilities use HTTP 409 **`unsupported_capability`** consistently across P1–P4. Capability names use P1's exact lower-case ASCII hyphen grammar `^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`: P1 publishes only implemented members of `identity-v1`, `family-lifecycle-v1`, `recovery-v1`, `device-continuity-v1`; P2 adds only implemented members of `records-v1`, `snapshot-v1`, `timer-commands-v1`, `import-resolver-v1`. Underscore variants are invalid rather than aliases; `family_sync` is an entitlement key. Mutation requests are strict allowlists at every depth; additive **response** fields are ignored. Extend P1's authoritative `protocol/decode.go`; do not create a competing decoder.

Known response kinds decode to typed snapshots. A future unknown kind may be skipped only when its journal envelope explicitly marks `optional: true`; advance past that journal item and request a fresh snapshot when the capability is later enabled. Unknown required kinds return `unsupported_resource` locally, retain the durable cursor, and attempt one authenticated snapshot; if still required/unknown, surface upgrade-required and stop the retry loop. No future resource capability is advertised by this beta.

### D7: exact semantics, not JSON text

All IDs are nonzero lower-case UUIDs. Epoch/version/revision/sequence values are integers in `0...9007199254740991` (safe exact JSON integers within PostgreSQL `bigint`); epoch and entity version start at 1. Reject `1.5`, exponent forms for integer IDs/counters, negative values, and overflow. Cursor is the existing `protocol.Cursor` object treated opaquely by clients; integrity and binding of continuation tokens are separately enforced.

Normalize baby matching names with `nfc-trim-fold-nfc-v1`: NFC, trim Unicode 15.0 White_Space at the two ends, locale-independent full case folding, then NFC. Pin the existing `golang.org/x/text v0.29.0` as direct when used; its cached normalization/case tables report Unicode `15.0.0`. Freeze the whitespace set as U+0009–000D, 0020, 0085, 00A0, 1680, 2000–200A, 2028, 2029, 202F, 205F, 3000 rather than inheriting a changed toolchain table. Preserve interior spaces, diacritics, zero-width characters, and order; never transliterate/fuzzy-match. Store display name trimmed/NFC but preserve case. Matching uses only normalized name and exact Gregorian birth date.

Canonical mutation bytes are UTF-8 JSON with lexicographically sorted object keys, required keys present, no insignificant whitespace, Unicode emitted literally except JSON-required escaping, and no HTML escaping. Decode with exact numbers and reject duplicate keys at **any depth**, invalid UTF-8/unpaired surrogates, multiple top-level JSON values, depth >16, and unknown request fields before hashing. Normalize valid payloads first; null/omitted optional scalar fields become null, absent optional arrays become `[]`, explicit null arrays fail, and required missing/null fields fail. Array order and duplicates remain significant for foods and side intervals. Optional note `null` equals omitted; `""` is distinct; normalize general text to NFC without trimming notes/foods.

Domain times retain a separately validated IANA `time_zone_id` (including `UTC`); offset spelling normalizes to the same UTC instant. Accept RFC3339 offsets within ±14:00 and 0–9 fractional digits, then round to PostgreSQL microseconds using ties-to-even and emit exactly six fractional digits in UTC. Reject leap seconds, negative/zero years, and invalid calendar dates. Use embedded Go `time/tzdata` for container validation. Birth date is `YYYY-MM-DD`, never an instant. Range checking happens after canonical rounding.

`volume_ml` accepts finite nonnegative decimal JSON numbers, including `1`, `1.0`, and `1e0`; normalize with decimal arithmetic to six fractional decimal places using ties-to-even, trim trailing zeros, and normalize negative zero to zero. Persist `numeric(10,6)` in typed columns; do not hash a binary float. The Swift adapter rounds its existing Double once at wire serialization and shares vectors. A volume of zero is valid; bottle contents and volume must either both be non-null or both be null.

`CanonicalMutationHash` hashes `"baby-tracker-mutation-v1\x00" || canonicalJSON` with SHA-256. Its JSON includes entity ID/kind, operation, payload version, observed entity version, canonical client mutation time, normalized payload, and optional import ID; it excludes mutation ID, principal/device/family (already receipt scope), and server fields. Different observed versions/client edit times are different mutation content. Hashes are emitted as 64 lower-case hex characters.

Separate shape/canonicalization from domain admission: a bounded, allowlisted typed mutation with an invalid range or relationship still has stable canonical bytes and receives a durable rejected receipt. Preserve a lexically invalid domain date/time as its normalized string in the hash before returning `invalid_timestamp`; valid timestamps use the canonical instant rule. Lexically malformed JSON, duplicate keys, unknown payload versions/fields, and unparseable mutation IDs fail the request before receipts because they do not identify a supported typed mutation. Keep this boundary consistent in fixtures and HTTP behavior.

`CanonicalContentHash(kind,payload)` hashes `"baby-tracker-content-v1\x00" || canonicalJSON({entity_kind,payload_version:1,payload})`, excluding `baby_id`, server metadata, and mutation envelope. Manifest equality compares `(kind, record UUID, mapped shared baby UUID, content hash)`; excluding the local baby ID from content bytes is what permits manifest verification after a remap. Compare canonical bytes as well when the actual submitted payload is available. No client hash ever overwrites a server record.

| Vector | Required result |
| --- | --- |
| `"  E\u0301LIE\u00a0"`, `"élie"` | Same normalized match key `élie`. |
| `"Straße"`, `"STRASSE"` | Same match key `strasse`. |
| `"A  B"`, `"A B"`; `"İ"`, `"i"`; `"a\u200bb"`, `"ab"` | Different match keys; no interior deletion. |
| Volume `1`, `1.0`, `1e0`; `-0`, `0` | Equal canonical numbers within each group. |
| Volume `1.0000005`, `1.0000015` | `1`, `1.000002` respectively. |
| `2026-09-20T12:00:00+02:00`, `2026-09-20T10:00:00Z` | `2026-09-20T10:00:00.000000Z`. |
| Absent note versus null; absent foods versus `[]` | Equal hashes; explicit `foods:null` rejected. |
| Foods `["pear","oat"]` versus `["oat","pear"]` | Different hashes; preserve user order. |
| `{"note":"a","note":"b"}` | `invalid_json`, never last-key-wins. |

### Bounded defaults

All limits are parsed/validated in `Server/internal/config/sync.go` and documented in `docs/protocol/v1.md`. `BT_` environment names below are exact. Deployment overrides may lower/raise values only within the hard ranges. Existing Plan 1 rate-limit machinery owns persistence; extend its policy registry, not a second limiter.

| Setting | Default | Permitted range / behavior |
| --- | --- | --- |
| `BT_MAX_LIVE_BABIES` | 20 | 1–100; family lock makes count+create atomic, `baby_limit`. |
| `BT_SYNC_BODY_BYTES` | 1048576 | 65536–4194304; reject before decoding, 413 `body_too_large`. |
| `BT_SYNC_BATCH_SIZE` | 100 | 1–200; ordered, one transaction/item, `batch_limit`. |
| `BT_SYNC_PAGE_SIZE` | 200 | 1–1000; also at most 1048576 encoded bytes/page, never split a journal item. |
| `BT_SYNC_MUTATION_BYTES` | 32768 | 4096–65536; protects each payload and journal representation. |
| `BT_SYNC_PAGE_TTL_SECONDS` | 900 | 60–3600; cursor/page tokens max 512 bytes. |
| `BT_SNAPSHOT_TTL_SECONDS` | 900 | 60–3600; 2 active snapshots/account and 8/family. |
| `BT_SNAPSHOT_MAX_BYTES` | 134217728 | 1048576–536870912; materialization aborts wholly on overflow, `snapshot_limit`. |
| `BT_SYNC_TRANSACTION_SECONDS` | 10 | 1–30; rollback entire mutation on timeout; HTTP sync deadline 30 seconds. |
| `BT_IMPORT_MAX_RECORDS` | 100000 | 100–1000000; chunks <=1000 manifests and <=body limit. |
| `BT_IMPORT_MAX_BABIES` | 100 | 1–500, additionally obey live family baby limit. |
| `BT_IMPORT_STAGING_TTL_HOURS` | 168 | 1–720; every incomplete mapping expires at its deadline, including a finalized ambiguous import. |
| `BT_IMPORT_MAX_SESSIONS_PER_FAMILY` | 1000 | 10–10000 retained IDs; count active, completed, and expired rows under the family lock. |
| `BT_IMPORT_MAX_SESSIONS_PER_ACCOUNT` | 100 | 1–1000 retained IDs for the initiating account within that family; existing IDs remain resumable/readable after the cap. |
| Mutation fixed windows | device 1200/min; account 2400/min; family 6000/min | Charge number of items before accepting a batch; all scopes must allow. Each configurable 1–100000/min. |
| Pull/read fixed windows | device 120/min; account 240/min | Includes continuation/snapshot/import status; 1–10000/min. |
| WebSocket limits | device 1, account 3, process 1000 | 1–5, 1–10, 10–10000 respectively; additional connects return 429 `connection_limit`. |
| Socket timing/size | ping every 25s; pong/write 5s; lifetime 15min; shutdown 5s | One coalesced pending hint/connection; inbound frame limit 1024 bytes; any app data closes 1008. |
| Reconnect contract | 1s base, 30s cap, full jitter | Client always performs HTTPS cursor sync after connect; Plan 3 owns implementation. |
| Text/collections | name 1–100 scalars/400 UTF-8 bytes; note <=4096 bytes; food <=100 scalars/400 bytes, <=50 foods; <=256 side intervals | Empty trimmed name rejected; no controls except tab/newline in notes; consistency/color <=64 scalars/256 bytes. |
| Numeric domain | volume 0–2000 ml; side seconds 0–604800; sum <=604800; sort_order 0–1000000 | Feed/sleep/diaper end >=start and duration <=7 days when end present. |
| Time domain | 1900-01-01 through 2100-12-31 for domain dates/times; client edit time through 2100-12-31 | No moving wall-clock rejection for queued edits; server acceptance time is server-owned. |
| Capability list | <=16 items, each <=64 ASCII bytes | De-duplicate/sort for negotiation; tokens match `^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`. |

Never silently drop excess import rows, truncate text, or return an incomplete mapping as complete. A canonical record too large for an item limit is rejected before state changes. HTTP 429 includes `Retry-After` and retryable stable envelope; database/network/lock timeouts return retryable 503 and do not save terminal receipts.

## Task 1: Freeze typed protocol, canonical semantics, and validation (P2.1)

**Files:**

- Create: `Server/internal/protocol/entities.go`, `mutations.go`, `sync.go`, `import.go`, `live.go`, `canonical.go`, `normalization.go`, `canonical_test.go`, `compatibility_test.go`.
- Create: `Server/internal/records/validation.go`, `validation_test.go`; `Server/internal/attachments/blob_store.go`; `Server/internal/config/sync.go`, `sync_test.go`; `docs/protocol/v1.md`.
- Modify: P1-owned `Server/internal/protocol/decode.go`, `Server/internal/protocol/types.go`, `types_test.go`; `Server/internal/config/config.go`; `Server/api/openapi.yaml`; `Server/go.mod`, `Server/go.sum`.
- Create fixtures: `Server/api/fixtures/v1/{baby,feed-breast,feed-bottle,feed-solids,feed-combination,sleep,diaper,timer-feed,timer-sleep,mutations,outcomes,sync-page,snapshot-page,import-resolve,import-conflict,live-hint,canonical-vectors,errors}.json`.

**Interfaces:** consumes foundation `Cursor`, `ErrorResponse`, `AttachmentMetadata`; produces all DTOs used below. Every listed wire field has a matching snake_case JSON tag. `Payload` is a closed interface whose unexported marker is implemented only by this package's concrete payload structs; `DecodeMutation([]byte) (Mutation,error)` constructs it after discriminator/duplicate-key validation.

The P2.5 import schemas also include immutable `import_mode:preserve_local_ids|match_shared` and owner conflict `choice:use_existing|keep_local` plus nullable `canonical_baby_id`; include them in the initial P2.1 fixtures so Plan 3 never implements the superseded collapsing mapping rule.

```go
type Mutation struct {
    MutationID, EntityID, EntityKind, Operation string
    PayloadVersion uint32
    ObservedEntityVersion uint64
    ClientMutationTime string
    ImportID *string
    Payload Payload
}
type EntityMeta struct {
    ID, FamilyID string; EntityVersion uint64
    CreatorAccountID, LastEditorAccountID string
    ClientMutationTime, AcceptedAt string
    TombstonedAt *string
}
type CanonicalEntity struct { EntityKind string; Meta EntityMeta; Payload Payload }
type Decimal struct { coefficient big.Int; scale int32 }
func (d Decimal) String() string // canonical finite JSON decimal spelling
type Outcome struct {
    MutationID, Status string
    OriginalStatus *string
    CanonicalEntity *CanonicalEntity
    RelatedEntities []CanonicalEntity
    Cursor *Cursor
    Code *string
    OverwroteNewerVersion bool
}
type JournalChange struct {
    Cursor Cursor; EntityKind, EntityID string; EntityVersion uint64
    ActorAccountID, DeviceID, Operation, AcceptedAt string
    Optional bool
    SnapshotOrTombstone CanonicalEntity
}
func DecodeMutation([]byte) (Mutation, error)
func CanonicalMutationHash(Mutation) ([32]byte, error)
func CanonicalContentHash(string, Payload) ([32]byte, error)
func NormalizeBabyName(string) (string, error)
func ValidateMutation(Mutation, Limits) error
```

`CanonicalEntity` encodes flat entity metadata under `meta`; `payload` is null only on tombstones. `RelatedEntities` defaults `[]`; a timer stop returns the timer tombstone as primary and completed record as related. Every stable replay returns `status:"already_applied"`, `original_status` set to the original `applied|superseded|rejected`, and exactly the original entities/cursor/code; clients must inspect original status before releasing timer dependencies.

Payload inventory (complete fields; nullable means optional/absent normalizes to null):

| Payload | Fields |
| --- | --- |
| `BabyPayload` | `name`, `birth_date`, `sort_order` (default 0). |
| `FeedPayload` | `baby_id`, `kind:breast|bottle|solids`, `started_at`, nullable `ended_at`, `time_zone_id`, nullable `note`, `sides:[{side:left|right,seconds:int}]`, nullable `volume_ml`, `contents:breast_milk|formula|mixed|null`, `foods:[string]`, `amount:taste|some|lots|null`. `kind` selects presentation, not permitted field presence: breast+bottle combinations retain both. |
| `SleepPayload` | `baby_id`, `started_at`, nullable `ended_at`, `time_zone_id`, nullable `note`, `kind_override:nap|night|null`; derived classification stays a client rule. |
| `DiaperPayload` | `baby_id`, `started_at`, nullable `ended_at`, `time_zone_id`, nullable `note`, `contents:wet|dirty|mixed|dry`, nullable `consistency`, `color`. |
| `TimerStartPayload` | `baby_id`, `timer_kind:feed|sleep`, `started_at`, `time_zone_id`, `completed_sides`, `current_side:left|right|null`, nullable `current_side_started_at`, nullable `pending_volume_ml`, `pending_contents:breast_milk|formula|mixed|null`. Feed requires side/start; sleep forbids feed-only state. |
| `TimerSwitchPayload` | `baby_id`, `side:left|right`, `switched_at`; entity ID is the acknowledged exact timer UUID. |
| `TimerBottlePayload` | `baby_id`, nullable `volume_ml`, nullable `contents`; both null explicitly removes a bottle; otherwise replaces the one pending bottle. |
| `TimerStopPayload` | `baby_id`, `completed_record_id`, `stopped_at`, nullable `note`, nullable `sleep_kind_override`; record payload derives from locked canonical timer, never arbitrary submitted completed state. |
| `DeletePayload` | Empty `{}` for baby; `{baby_id}` for feed/sleep/diaper so missing-parent errors remain explicit. |

Ordinary operations are `create`, `update` (complete replacement of allowlisted domain fields), and `delete` for baby/feed/sleep/diaper. Timer operations are only `timer_start`, `timer_switch_side`, `timer_attach_bottle`, and `timer_stop`. No generic timer update/delete and no lifecycle operations in sync. Import adds nullable top-level `import_id` and uses ordinary `create` for records; existing records follow import equality policy instead of LWW.

- [ ] **Step 1 — RED: add canonical vector and allowlist tests.** Create every named fixture, including one valid example of each payload/operation and all four outcomes. Add this table test; `mustMutation(t,raw)` is a local helper calling `DecodeMutation` and failing immediately on error. `readFixture(t,name)` reads `../../api/fixtures/v1/` and fails on missing files.

```go
func TestCanonicalVectors(t *testing.T) {
    for _, tc := range []struct{ input, want string }{
        {"  E\u0301LIE\u00a0", "élie"}, {"Straße", "strasse"},
        {"A  B", "a  b"}, {"İ", "i\u0307"},
    } {
        got, err := protocol.NormalizeBabyName(tc.input)
        if err != nil || got != tc.want { t.Fatalf("normalize: %q %v", got, err) }
    }
    raw := readFixture(t, "feed-bottle.json") // full Mutation, volume_ml:1
    a := mustMutation(t, raw)
    b := mustMutation(t, bytes.Replace(raw, []byte(`"volume_ml":1`), []byte(`"volume_ml":1.0`), 1))
    ah, err := protocol.CanonicalMutationHash(a); if err != nil { t.Fatal(err) }
    bh, err := protocol.CanonicalMutationHash(b); if err != nil { t.Fatal(err) }
    if ah != bh { t.Fatal("equal decimal semantics produced different receipts") }
}
```

Add table-driven `TestRejectDuplicateNestedKeys`, `TestRejectPresentationAndActorFields`, `TestDomainBounds`, `TestResponseAllowsAdditiveFields`, `TestUnknownResourceRecovery`, and `TestOpenAPIOriginHasNoBasePath`. Their inputs include nested duplicate `baby_id`, `created_by`, `live_activity_id`, `photo_data`, >2^53−1 sequence, invalid leap date, oversized note, and unknown optional/required response kinds. Keep `TestOpenAPIRouteSetMatchesRegisteredRoutes` valid by adding schemas now and endpoint paths with their owning task.

Add `TestCapabilityConventionMatchesIdentity`: iterate the complete P1/P2 registry above, assert every token matches the one shared grammar, assert `identity_v1`/`records_v1` fail, and call both identity and sync negotiation with an unknown required token to assert the same `unsupported_capability` code. Run it in the focused RED/GREEN command with `TestCapability`; extend the existing P1 decoder in place and stage `decode.go` with these regression tests.

- [ ] **Step 2 — verify RED.** Run `rtk proxy sh -c 'cd Server && go test ./internal/protocol ./internal/records ./internal/config -run "TestCanonical|TestReject|TestDomain|TestResponse|TestUnknown|TestOpenAPIOrigin|TestCapability" -count=1'`. Expect missing production types/functions or failing behavior, not a missing fixture/import caused by a test typo.
- [ ] **Step 3 — implement normalization and strict decoding.** Promote x/text at its existing version; implement a JSON token walk with per-object key sets before typed decoding, exact `json.Number` parsing, one-value EOF check, UTF-8/surrogate validation, then canonical encoding. Define `Limits` in protocol from the numeric table; config returns a validated copy. Do not use `map[string]any` float decoding for a semantic hash.

```go
func NormalizeBabyName(s string) (string, error) {
    if !utf8.ValidString(s) { return "", ErrInvalidText }
    s = norm.NFC.String(s)
    s = strings.TrimFunc(s, isUnicode15WhiteSpace)
    s = norm.NFC.String(cases.Fold().String(s))
    if s == "" { return "", ErrInvalidText }
    return s, nil
}
// isUnicode15WhiteSpace is the exact code-point set above; ErrInvalidText
// is a protocol sentinel mapping to permanent code invalid_text.
```

- [ ] **Step 4 — implement complete DTOs, semantic validators, fixtures, and compatibility documentation.** Write every field in the inventory to OpenAPI with `additionalProperties:false` on request objects and tolerant response schemas. Implement `CanonicalContentHash` using the same canonicalizer, excluding only the declared metadata/relationship fields. Add a fixture's literal canonical JSON and SHA-256 produced by an independent `crypto/sha256` test oracle, then check the committed literal in subsequent tests (do not recompute expected output with the production canonicalizer). Add the private attachment seam only:

```go
type ObjectKey string
type Transfer struct { URL string; ExpiresAt time.Time } // internal only
type BlobStore interface {
    AuthorizeUpload(context.Context, ObjectKey, int64, [32]byte, time.Duration) (Transfer, error)
    AuthorizeDownload(context.Context, ObjectKey, time.Duration) (Transfer, error)
    Delete(context.Context, ObjectKey) error
}
```

There is no BlobStore implementation, HTTP route, database table, or public transfer URL. Check `Baby.photoData`/`ActiveTimer.liveActivityID` against fixture forbidden-field tests explicitly.

- [ ] **Step 5 — GREEN: rerun the focused command.** Every numeric/name/date/volume/null/ordering vector must pass, including extra response fields and rejected request fields.
- [ ] **Step 6 — full suite.** Run `rtk make server-test` and `rtk proxy sh -c 'cd Server && go vet ./...'`. Report whether foundation PostgreSQL tests executed or skipped; P2.2 installs the non-skipping integration runner before any storage work is accepted.
- [ ] **Step 7 — commit.** Stage only the exact files listed above, inspect `rtk git diff --cached --check`, and commit `feat(server): define canonical family sync protocol`. Do not advertise `records-v1` until P2.3 is wired or `timer-commands-v1` until P2.4 passes.

## Task 2: Typed storage, receipts, and commit-ordered journal (P2.2)

- [ ] **Step 0 — commit the schema-9 compatibility predecessor.** Capture accepted schema-8 head as `BT_MIGRATION_BASE_REF`; land final 000009, complete v9 privilege snapshot including the cursor column grant, overlap tests/scenarios, and manifest `[8,9]` without record feature use; run schema-8 gates; commit `feat(server): prepare records schema compatibility`; review and capture `BT_MIGRATION_PRIOR_REF`.

**Files:**

- Create: `Server/migrations/000009_canonical_records.sql`.
- Create: `Server/internal/records/{repository,babies,feeds,sleeps,diapers,journal}.go`, `repository_test.go`, `rls_test.go`; `Server/internal/sync/{mutation,receipts,journal}.go`, `mutation_test.go`, `ordering_test.go`.
- Create: `Server/internal/testkit/{postgres,fixtures,protocol_client}.go`; `Server/scripts/test-db.sh`; `Server/internal/migrate/overlap_test.go`.
- Modify: `Makefile` (`server-test` only), `Server/internal/migrate/compatibility.json`, `Server/internal/migrate/compatibility_test.go`, P1-owned `Server/scripts/check-migration-overlap.sh`, `Server/scripts/check-principals.sh` (new tables and column privileges), `Server/internal/protocol/types_test.go`.

**Interfaces:** consumes P2.1 types and Plan 1 guard. Produces:

```go
// internal/records
type Guard interface {
    WithAuthorizedTransaction(context.Context, access.Principal, string, string,
        func(pgx.Tx, access.Grant) error) error
}
type Repository struct{} // stateless; receives the guarded transaction
func (r *Repository) Apply(context.Context, pgx.Tx, access.Grant, string, protocol.Mutation) (protocol.Outcome, error)
// The string is authenticated deviceID. Requires family row FOR UPDATE.
func (r *Repository) GetBaby(context.Context,pgx.Tx,string,string) (protocol.BabyPayload,protocol.EntityMeta,error)
func (r *Repository) GetFeed(context.Context,pgx.Tx,string,string) (protocol.FeedPayload,protocol.EntityMeta,error)
func (r *Repository) GetSleep(context.Context,pgx.Tx,string,string) (protocol.SleepPayload,protocol.EntityMeta,error)
func (r *Repository) GetDiaper(context.Context,pgx.Tx,string,string) (protocol.DiaperPayload,protocol.EntityMeta,error)
// Read string parameters are familyID, entityID; absent rows are ErrNotFound.
func (r *Repository) PutBaby(context.Context,pgx.Tx,protocol.EntityMeta,protocol.BabyPayload) (protocol.CanonicalEntity,error)
func (r *Repository) PutFeed(context.Context,pgx.Tx,protocol.EntityMeta,protocol.FeedPayload) (protocol.CanonicalEntity,error)
func (r *Repository) PutSleep(context.Context,pgx.Tx,protocol.EntityMeta,protocol.SleepPayload) (protocol.CanonicalEntity,error)
func (r *Repository) PutDiaper(context.Context,pgx.Tx,protocol.EntityMeta,protocol.DiaperPayload) (protocol.CanonicalEntity,error)
// Put metadata is constructed inside the service, never decoded from a request.
func AppendJournal(context.Context,pgx.Tx,access.Grant,string,protocol.Mutation,[]protocol.CanonicalEntity) ([]protocol.JournalChange,error)
// internal/sync
type Service struct {
    guard records.Guard; repo *records.Repository
    identity protocol.ServerInfo; limits protocol.Limits; now func() time.Time
}
func (s *Service) Apply(ctx context.Context, p access.Principal, familyID string, m protocol.Mutation) (protocol.Outcome, error)
func NewService(guard records.Guard, repo *records.Repository, identity protocol.ServerInfo, limits protocol.Limits, now func() time.Time) *Service
```

Use `families.next_sequence bigint NOT NULL DEFAULT 0` as the only cursor allocator (do not also add `family_sequences`). Common entity columns are `(family_id uuid, id uuid, entity_version bigint, creator_account_id uuid, last_editor_account_id uuid, client_mutation_time timestamptz, accepted_at timestamptz, tombstoned_at timestamptz nullable)`, PK `(family_id,id)`. Records reference `(family_id,baby_id)` with a composite FK. Opaque historical actor IDs are retained without cascading account deletion.

Migration 000009 adds `CHECK(next_sequence BETWEEN 0 AND 9007199254740991)` and the exact narrow grant `GRANT UPDATE (next_sequence) ON families TO baby_tracker_runtime`. P1's SELECT and forced family RLS remain; do **not** restore table-wide UPDATE/INSERT/DELETE. PostgreSQL's row-lock privilege requirement is met by this column grant, allowing the guarded `SELECT ... FOR UPDATE` and cursor update while structural `status`, `display_name`, `purge_after`, and membership changes remain denied. `records.AppendJournal` updates only `next_sequence`, under the existing family lock and security fence. Add this column privilege to `check-principals.sh` so deployment evidence proves both allowed cursor allocation and forbidden structural mutation.

Tables have individual typed columns from P2.1: `babies`, `feed_entries`, `sleep_entries`, `diaper_entries`; normalized baby match columns are `normalized_name`, `normalization_version`, `birth_date date`. `feed_side_intervals` has `(family_id,feed_id,position,side,seconds)` and `feed_foods` has `(family_id,feed_id,position,food)`; preserve array order and duplicate foods. `active_timers` and its interval table land in P2.4. JSONB is permitted for immutable journal snapshots and durable outcomes, not as the canonical entity table.

`mutation_receipts` stores family ID, authenticated device ID, mutation ID, 32-byte semantic hash, original stable outcome, accepted time, and last cursor nullable; PK `(device_id,mutation_id)`. `mutation_journal` has PK `(family_id,sequence)`, complete canonical snapshot/tombstone, entity version/kind/ID, actor/device, operation, acceptance time. Runtime gets SELECT/INSERT only on immutable journal/receipts. Reject mutation ID reuse without altering the first receipt. A receipt in an old/inaccessible family must produce generic `mutation_id_reused` without leaking its contents; use a narrow definer collision check accepting only the authenticated account/device and mutation ID when RLS hides that uniqueness conflict.

Receipt lookup deliberately ignores the old **session** ID while requiring P1's currently authenticated same account/device/family authority. After same-epoch fresh Apple reauthentication with a valid continuity credential, the new session therefore replays the unchanged `(device_id,mutation_id,hash)` receipt before current entity admission. Never replace this proof with lookup by entity ID or install hint; a fresh unrelated device cannot claim another device's receipt even within the same account.

Every new family-owned table has this policy shape and forced RLS:

```sql
ALTER TABLE babies ENABLE ROW LEVEL SECURITY;
ALTER TABLE babies FORCE ROW LEVEL SECURITY;
CREATE POLICY family_context ON babies
  USING (family_id = app.current_family_id())
  WITH CHECK (family_id = app.current_family_id());
GRANT SELECT, INSERT, UPDATE ON babies TO baby_tracker_runtime;
-- Apply identical tenant predicates to typed child tables, journal, receipts.
-- No TRUNCATE, no table ownership, no BYPASSRLS; tombstone instead of DELETE.
```

- [ ] **Step 1 — establish isolated executable integration tests.** Implement `test-db.sh` as a wrapper around the pinned Compose PostgreSQL image using a unique project and generated 0600 secret files. It must never accept a production database URL as its reset target. Provision migrator/runtime principals, set `BABY_TRACKER_TEST_DATABASE_URL` for existing foundation tests, run `go test -race -p 1 ./...`, and fail if DB tests skip. Existing packages reset a shared test schema, so package execution is sequential. `testkit.New(t)` creates a separately named test database per new test, applies embedded migrations explicitly, constructs P1 test authority/session fixtures, and connects each exercised service through a non-owner runtime pool. It returns `Harness{Owner, Caregiver, Outsider access.Principal; FamilyID string; Service *familysync.Service; Runtime,Migrator *pgxpool.Pool}`. `MustMutation(t,fixture)` loads P2.1 fixtures and assigns that fixture's documented IDs; `Count(t,table,familyID)` uses the migrator only for assertions, never service calls. Cleanup validates and drops only that generated test database/project. Extend `make server-test` to invoke this runner.
- [ ] **Step 2 — RED: add receipt, family isolation, and ordering tests.** Include the following concrete test and the named table of race cases. Use channels to coordinate transactions; no sleep-based proof of ordering.

Add `TestRuntimeCursorColumnGrantDoesNotRestoreStructuralWrites` in `records/rls_test.go`: under a real runtime guarded transaction and tenant context, lock the authorized family row and increment `next_sequence`, then use separate rollback-isolated transactions to attempt `UPDATE families SET status='deletion_pending'`, `UPDATE families SET display_name='forbidden'`, `UPDATE families SET purge_after=now()`, INSERT, and DELETE. Expect SQLSTATE `42501` for each forbidden structural operation, while a foreign-family cursor UPDATE changes zero rows. Assert `has_column_privilege('baby_tracker_runtime','families','next_sequence','UPDATE')` is true and the same check for status/display_name/purge_after is false. Extend `check-principals.sh` with these exact queries; a broad UPDATE grant must make the test fail.

Add `TestReceiptSurvivesAuthenticatedDeviceReauthentication`: accept one mutation, create a second P1 session through fresh proof plus the original device credential, assert same DeviceID/new SessionID, and retry the exact mutation; expect `already_applied` with the exact original hash/outcome/cursor and one journal row. `TestCrossAccountDeviceContinuityCannotReadReceipt` presents another account's Apple proof with that binding and expects P1 `device_continuity_invalid`, zero new session/receipt, and no original outcome disclosure. These tests exercise P1 auth, not hand-edited Principal values.

```go
func TestReceiptReplayPreservesFirstResult(t *testing.T) {
    h := testkit.New(t)
    m := testkit.MustMutation(t, "baby.json")
    first, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
    if err != nil || first.Status != "applied" { t.Fatalf("first=%+v err=%v", first, err) }
    again, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
    if err != nil || again.Status != "already_applied" || *again.OriginalStatus != "applied" {
        t.Fatalf("retry=%+v err=%v", again, err)
    }
    if !reflect.DeepEqual(first.CanonicalEntity, again.CanonicalEntity) || *first.Cursor != *again.Cursor {
        t.Fatal("replay changed the accepted canonical result")
    }
    if h.Count(t, "mutation_journal", h.FamilyID) != 1 { t.Fatal("duplicate journal entry") }
}
```

`TestCommitOrderFollowsFamilyLock` pauses transaction A after family lock and insert, starts B, reads committed high water from C, asserts C still sees the pre-A cursor and B cannot complete, then commits A and verifies B receives the next cursor. Use a guard decorator in the test to pause at callback completion before commit; channel release occurs after C's query finishes. `TestRevocationFencePreventsLaterWrite` commits P1 removal before releasing a stale authorized request to its SQL boundary and expects denial with unchanged rows. Add `TestReceiptDifferentHashRejected`, `TestPermanentRejectionReplays`, `TestTransientRollbackHasNoReceipt`, `TestOrdinaryUpdateReportsOverwrittenVersion`, `TestTombstoneCannotResurrect`, and `TestEveryRepositoryDeniesForeignFamily`; the latter checks missing context, foreign context, cross-family FK, and runtime role attributes for every table/method.

- [ ] **Step 3 — verify RED.** Run `rtk proxy env BT_TEST_RUN='TestReceipt|TestCommitOrder|TestRevocationFence|TestPermanent|TestTransient|TestOrdinary|TestTombstone|TestEveryRepository|TestRuntimeCursorColumn|TestCrossAccountDeviceContinuity' make server-test` (the runner passes the expression to `go test -run`). Expected failures are missing migration/repository/service behavior; no skipped DB tests.
- [ ] **Step 4 — implement typed DDL/repositories and transaction orchestration.** Lock the family row for the entire mutation transaction, check a receipt before current domain-state checks, apply the entity change, allocate all journal sequence values, save the stable receipt, then return to the guard for commit. For a permanent semantic rejection, save a receipt without advancing sequence. Malformed JSON/envelope with no parseable stable identity is HTTP 400 without a receipt. Authorization loss, exhausted rate limits, unavailable DB, and timeouts are retryable/request errors without terminal receipts.

```go
err := s.guard.WithAuthorizedTransaction(ctx, p, familyID, actionFor(m), func(tx pgx.Tx, g access.Grant) error {
    // actionFor maps baby:create -> add_baby, baby:delete -> delete_baby,
    // other mutations -> mutate_record. Unknown kinds fail in P2.1.
    if err := lockFamilyForUpdate(ctx, tx, g.FamilyID); err != nil { return err }
    // readReceipt uses p.DeviceID, never request JSON. A hash mismatch returns
    // mutation_id_reused; a match returns the stored immutable outcome.
    result, err = s.repo.Apply(ctx, tx, g, p.DeviceID, m)
    if err != nil { return err }
    return saveReceipt(ctx, tx, g.FamilyID, p.DeviceID, m, result)
})
```

Define `lockFamilyForUpdate(ctx,tx,familyID) error`, `readReceipt(ctx,tx,deviceID,mutationID) (Receipt,bool,error)`, and `saveReceipt(ctx,tx,familyID,deviceID,m,outcome) error` in the indicated sync files. `Receipt{Hash [32]byte; Outcome protocol.Outcome}` is internal. The sole write-side `records.AppendJournal` lives in `records/journal.go`; `Repository.Apply` invokes it after the typed writes and uses its last cursor in the outcome. It updates the locked row with `next_sequence = next_sequence + count` and rejects numeric exhaustion atomically; journal order is the supplied deterministic entity order. `sync/journal.go` owns read queries only. This placement avoids a records→sync→records import cycle and lets timer/import services call the identical allocator. Retain a transaction-local list for post-commit hooks added in P2.6. Domain rejection is represented by `Outcome`, SQL failure by `error`, so a stable rejection is not accidentally rolled back.

Creates reject existing UUID/content with `entity_exists`; updates require existing row and preserve creator; all successful writes increment version. LWW accepts a stale observed version and sets `overwrote_newer_version:true`. Tombstoned target returns `superseded` plus tombstone/code `entity_deleted`; missing/deleted parent yields stable rejected `baby_deleted` when a baby tombstone exists, otherwise `baby_not_found`. Baby delete dispatch remains disabled until its atomic cascade exists in P2.4; it must never fall through to a single-row tombstone.

Canonical entity UUIDs share a family-wide namespace. Add `findEntityKind(ctx,tx,familyID,id) (string,error)` as a UNION over the typed tables under the family lock, extending it with active timers in P2.4. A create finding the ID in another kind rejects `entity_kind_collision`; import uses the same check. This is an identity lookup, not a generic canonical payload table. It also prevents an imported record UUID from silently creating a second object under a different kind.

- [ ] **Step 5 — GREEN: repeat focused DB command.** Inject SQL failure after entity update, after journal insert, and before receipt insert by test transaction decorators/constraint faults; each must leave no partial state/cursor/receipt. Use two principals with the same mutation UUID to prove receipt scope is device-specific.
- [ ] **Step 5a — compatibility RED/GREEN and overlap 8→9.** Capture accepted schema-8 head as `BT_MIGRATION_BASE_REF`. Add `TestMigration000009CompatibilityOverlap` for exact SQL/hash, complete v9 privilege snapshot including `column:public.families.next_sequence`, changed/unknown digest denial, and broad-table-UPDATE denial. Commit final 000009 + snapshot + scenarios + manifest `[8,9]` as a deployable compatibility predecessor and capture `BT_MIGRATION_PRIOR_REF`. Implement record feature code in a second commit, generate `[9,9]`, and capture `BT_MIGRATION_FEATURE_REF`. Run prepared mode with all three refs; no archive mutation. At schema 8 run the existing family API; migrate with the predecessor admin; at schema 9 run that API plus the runtime-role record-storage probe (there is no record HTTP route yet); then run the feature binary. All phases retain structural denial.
- [ ] **Step 6 — full suite.** Run `rtk make server-test` (all real DB packages, including `TestRuntimeCursorColumnGrantDoesNotRestoreStructuralWrites`, continuity/receipt tests, and `TestMigration000009CompatibilityOverlap`), `rtk proxy sh -c 'cd Server && go vet ./...'`, `rtk git diff --check`; require zero skips in the integration portion. Inspect migration and runtime grants, not only Go role checks.
- [ ] **Step 7 — commit and overlap.** Stage the exact P2.2 feature files with manifest `[9,9]`, commit `feat(server): persist typed records with ordered sync receipts`, capture `BT_MIGRATION_FEATURE_REF`, then run the exact 8→9 prepared-mode command above. Do not amend either commit after it passes.

## Task 3: Ordered batch sync, fixed high-water pages, and full snapshots (P2.3)

- [ ] **Step 0 — commit the schema-10 compatibility predecessor.** Capture accepted schema-9 feature head; land final 000010, complete v10 privilege snapshot/scenarios, and `[9,10]` without snapshot feature use; run schema-9 gates; commit `feat(server): prepare snapshot schema compatibility`; review and capture the base/prior refs.

**Files:**

- Create: `Server/internal/sync/{service,paging,snapshot}.go`, `paging_test.go`, `snapshot_test.go`; `Server/internal/httpapi/sync.go`, `sync_test.go`; `Server/migrations/000010_snapshot_staging.sql`.
- Modify: `Server/internal/sync/mutation.go`, `Server/internal/httpapi/router.go`, `Server/cmd/baby-tracker-server/main.go`, `Server/api/openapi.yaml`, `Server/internal/protocol/{sync,types_test}.go`, `Server/internal/migrate/compatibility.json`, `Server/internal/migrate/overlap_test.go`, `Server/scripts/check-migration-overlap.sh`, `Server/internal/testkit/fixtures.go`, `Server/internal/config/sync.go`, `Server/api/fixtures/v1/{sync-page,snapshot-page,errors}.json`, `docs/protocol/v1.md`.

**Interfaces:**

```go
type SyncRequest struct {
    ProtocolVersion string; Capabilities []string
    Cursor *Cursor; Mutations []Mutation; PageToken *string
}
type SyncResponse struct {
    ServerInstanceID string; RecoveryEpoch uint64
    Outcomes []Outcome; Changes []JournalChange; HighWater Cursor
    NextPageToken *string
}
type SnapshotRequest struct { PageToken *string } // GET query, header negotiation
type SnapshotResponse struct {
    ServerInstanceID string; RecoveryEpoch uint64
    SnapshotID string; HighWater Cursor
    Entities []CanonicalEntity; NextPageToken *string
}
func (s *Service) ApplyAndPull(context.Context, access.Principal, protocol.SyncRequest) (protocol.SyncResponse, error)
func (s *Service) FullSnapshot(context.Context, access.Principal, protocol.SnapshotRequest) (protocol.SnapshotResponse, error)
```

`POST /v1/sync` resolves the principal's authorized family before using the cursor. Null cursor requests the retained journal from sequence 0 (beta retains journal until purge); corrupt/foreign/future/unsupported cursor returns stable `snapshot_required`, never guesses position. A continuation request contains only the same protocol/capabilities, empty mutations, null cursor, and opaque `page_token`; new mutations with a page token fail before writes. `GET /v1/sync/snapshot` starts capture without a token; subsequent GETs use `?page_token=...`. Tokens contain no bearer credentials or payload.

Tokens are base64url of a version byte, bounded key ID, random 32-byte database lookup key, and HMAC-SHA-256 under the dedicated page-key ring. Persist only its SHA-256 and binding `(account,device,session,family,instance,epoch,security_revision,high_water,next_position,expires_at,kind)`. Any wrong binding/forgery returns generic `invalid_page_token`; expired returns `page_expired`; epoch mismatch returns the single wire code `epoch_changed` and forces reauthentication. No logs of the query/token. Restart can serve the token using persisted current/retained keys. Replay returns the same page; continuing does not consume a token.

Add RLS-protected `sync_page_sessions`, `sync_snapshots`, `sync_snapshot_entities` and their expiration indexes. Snapshot rows contain immutable typed canonical JSON plus kind/ID/order and high water; source-of-truth storage stays typed tables. Under the guard and a **family row FOR UPDATE** lock, capture committed `next_sequence`, insert every live entity and retained tombstone in deterministic `(kind order, UUID)` order, enforce byte/count/time budgets, and commit the entire snapshot plus first token. All writers/import/child cascade take the same family lock, so this read-committed transaction sees one stable canonical state despite multiple SQL statements. Do not independently page current tables, hold a transaction across requests, or export a live PostgreSQL snapshot connection. Expired snapshots are unreadable immediately; Plan 4 worker owns eventual deletion.

- [ ] **Step 1 — RED: write fixed high-water and snapshot tests.** Extend testkit with `Pull(t,p,cursor,pageToken) SyncResponse` and `Snapshot(t,p,pageToken) SnapshotResponse`, which call the real services and fail on unexpected error. For negative cases call service directly. Fixtures produce >configured page size by setting test `SyncPageSize=2` and use deterministic IDs.

```go
func TestSnapshotPagesStayAtOneHighWater(t *testing.T) {
    h := testkit.New(t)
    h.SeedBabies(t, 3) // testkit helper: three ordinary Apply calls, unique UUIDs
    first := h.Snapshot(t, h.Owner, nil)
    before := first.HighWater
    h.RenameBaby(t, 2, "Changed after capture") // ordinary update, not raw SQL
    last := h.Snapshot(t, h.Owner, first.NextPageToken)
    if last.HighWater != before || last.SnapshotID != first.SnapshotID {
        t.Fatal("snapshot pages crossed capture boundaries")
    }
    for _, e := range append(first.Entities, last.Entities...) {
        accepted, err := time.Parse(time.RFC3339Nano,e.Meta.AcceptedAt)
        if err != nil { t.Fatal(err) }
        if accepted.After(h.CaptureTime(t, first.SnapshotID)) { t.Fatal("late entity leaked into snapshot") }
    }
    if last.NextPageToken != nil { t.Fatal("fixture should finish on page two") }
}
```

Define `SeedBabies`, `RenameBaby`, and `CaptureTime(t,snapshotID) time.Time` in testkit. Also assert expected original names/versions so equal clock timestamps cannot conceal leakage. `TestOutcomeCursorDoesNotSkipJournal` has A create sequence 1, B create sequence 2, then A sends mutation receiving outcome cursor 3 with request cursor 0; expect journal starts at 1 and page checkpoint only reaches fixed high water after all pages. `TestContinuousWriterDoesNotStarvePull` appends after page 1 and proves continuation ends at old high water. `TestSnapshotContinuationReauthorizes` removes a member after page 1 and denies page 2 even with valid token. Cover forged/expired/wrong-device tokens, epoch change/quarantine, unknown capability, body/item/page limits, and HTTP route conformance.

- [ ] **Step 2 — verify RED.** Run `rtk proxy env BT_TEST_RUN='TestSnapshot|TestOutcomeCursor|TestContinuousWriter|TestSyncHTTP|TestPageToken' make server-test`. Expect absent paging/snapshot implementation and routes, with actual PostgreSQL execution.
- [ ] **Step 3 — implement ordered service and bounded page reads.** Apply mutations in request order via P2.2, each isolated transaction; a rejected/superseded item does not block subsequent valid items. A request-level transient failure may follow a committed prefix: return retryable 503, persist no receipt for unattempted items, and client retries the identical batch. Healthy responses contain exactly one outcome/input item. Capture the initial journal high water under the guarded family lock after mutation processing; immutable journal pages use `sequence > last AND sequence <= fixedHighWater ORDER BY sequence LIMIT pageSize` plus encoded-byte budget. Do not derive high water from MAX(journal) or an allocated sequence outside a committed reader view.

```sql
SELECT next_sequence FROM families WHERE id = $1 FOR UPDATE;
SELECT sequence, entity_kind, entity_id, entity_version, actor_account_id,
       device_id, operation, accepted_at, snapshot
FROM mutation_journal
WHERE family_id = $1 AND sequence > $2 AND sequence <= $3
ORDER BY sequence LIMIT $4;
```

- [ ] **Step 4 — implement durable snapshot materialization and token binding.** Parameterize union reads over the four typed entity tables now; register timer reader only when P2.4 lands. Snapshot materialization builds rows incrementally inside one transaction, tracks encoded bytes, and rolls back fully on limits. A page over 1 MiB closes before the next whole entity; reject impossible oversized single entities during mutation validation. Add identity/epoch to every success/error envelope where sync consumers require it; retain safe request IDs. HTTP handler never reads a maintenance pool.
- [ ] **Step 5 — GREEN: rerun the focused command.** Kill/reconstruct the application between snapshot pages and verify continuation succeeds; mutate/remove after capture and verify original snapshots or authoritative denial, never mixed canonical state.
- [ ] **Step 5a — compatibility RED/GREEN and overlap 9→10.** Capture schema-9 accepted head as `BT_MIGRATION_BASE_REF`; commit final 000010, complete v10 snapshot, scenarios, and manifest `[9,10]` without snapshot feature use, then capture `BT_MIGRATION_PRIOR_REF`. Implement snapshot routes in the second `[10,10]` feature commit and capture `BT_MIGRATION_FEATURE_REF`. Prepared mode archives all three refs unchanged. The predecessor uses the existing invitation create/read and caregiver denial at schema 9 and 10; the feature binary additionally performs record create/read and fixed-high-water snapshot continuation. Use fresh command IDs, never log secrets, retain structural denial, and reject changed/unknown digests.
- [ ] **Step 6 — full suite.** Run `rtk make server-test`, `rtk proxy sh -c 'cd Server && go vet ./...'`, and schema/route fixture tests. P2.3 advertises `records-v1` and `snapshot-v1` only after the handlers and schema are present.
- [ ] **Step 7 — commit and overlap.** Stage exact P2.3 feature files with `[10,10]`, commit `feat(server): add bounded sync pages and consistent snapshots`, capture `BT_MIGRATION_FEATURE_REF`, then run prepared-mode 9→10 using all three refs.

## Task 4: Exact timer commands, bottle state, and child cascade (P2.4)

- [ ] **Step 0 — commit the schema-11 compatibility predecessor.** Capture accepted schema-10 feature head; land final 000011, complete v11 privilege snapshot/scenarios, and `[10,11]` without timer feature use; run schema-10 gates; commit `feat(server): prepare timer schema compatibility`; review and capture the base/prior refs.

**Files:**

- Create: `Server/internal/records/timers.go`, `child_delete.go`, `timers_test.go`, `child_delete_test.go`; `Server/migrations/000011_timer_commands.sql`.
- Modify: `Server/internal/records/repository.go`, `Server/internal/sync/{mutation,snapshot}.go`, `Server/internal/protocol/{mutations,entities}.go`, `Server/internal/migrate/compatibility.json`, `Server/internal/migrate/overlap_test.go`, `Server/scripts/check-migration-overlap.sh`, `Server/internal/testkit/fixtures.go`, `Server/api/openapi.yaml`, `Server/api/fixtures/v1/{timer-feed,timer-sleep,mutations,outcomes}.json`, `docs/protocol/v1.md`.

**Interfaces:** consumes exact P2.1 payloads; produces `(*records.Repository).ApplyTimer(ctx context.Context, tx pgx.Tx, grant access.Grant, deviceID string, m protocol.Mutation) (protocol.Outcome,error)` and `DeleteBaby` with the same signature, called only under the family lock. Both feed all changed entities to `records.AppendJournal`. Introduce typed `active_timers` and `timer_side_intervals`; one live timer per `(family_id,baby_id,timer_kind)` is enforced by a partial unique index. A timer tombstone retains `completed_record_id` nullable to identify the exact completion after later stale commands.

**D8 resolution:** `timer_attach_bottle` targets the exact acknowledged feed timer, replaces its single pending bottle, and permits null/null to remove it. Concurrent bottle replacements use family commit order; the last accepted replacement before stop supplies completion content. A replacement after stop returns terminal `superseded/timer_ended` and never edits the completed record. Plan 3 may fold bottle/side state only into a never-dispatched start; uncertain start mutation bytes/ID remain frozen. The server never retargets commands from a losing start to the winning timer.

Timer start returns applied for its proposed UUID only when the baby/kind is free; a collision returns superseded with the existing timer, records that receipt, and allocates no new journal sequence. Existing proposed UUID with different state is `entity_exists`, not replacement. Switch/attach/stop carry both exact entity ID and baby ID; mismatch rejects. Side switch appends whole elapsed seconds `floor(switched_at-current_side_started_at)`, then starts the selected side; negative event order yields `invalid_timer_time`, and same-side switch is an applied no-op receipt with current cursor and no new journal entry. Stop derives feed intervals and current bottle or sleep times from the locked timer and supplied stopped time, assigns the caller's exact completed UUID, and appends timer tombstone then completed record as two consecutive journal changes in one transaction. **Before either half is written**, call `findEntityKind(ctx,tx,familyID,completed_record_id)` under the family lock across baby/feed/sleep/diaper/timer tables, including all tombstones and the exact timer's own ID. Any occupied family UUID returns stable rejected `completed_record_exists`; no timer tombstone, completion, version change, or journal/cursor advance occurs. Persist only the complete rejection receipt, never a partial/applied stop receipt. Receipt replay is checked before this admission rule, so a retry of the original successful stop still returns its stored success. A second stop under a different mutation ID returns superseded with original completion, creates nothing, and never touches a newer timer.

- [ ] **Step 1 — RED: add command race tests and cascade assertions.** Extend testkit with `StartFeed(t,p,id) Outcome`, `AttachBottle(t,p,id,ml) Outcome`, `StopFeed(t,p,id,recordID) Outcome`, `SwitchSide(t,p,id,side) Outcome`; these build the exact typed P2.1 mutations and call `Service.Apply` with distinct mutation IDs. Add `ReadFeed(t,id) protocol.CanonicalEntity` via guarded repository read.

```go
func TestBottleBeforeStopCreatesOneCombinationFeed(t *testing.T) {
    h := testkit.New(t)
    start := h.StartFeed(t, h.Owner, testkit.TimerA)
    if start.Status != "applied" { t.Fatalf("start=%+v", start) }
    h.AttachBottle(t, h.Caregiver, testkit.TimerA, "60")
    stop := h.StopFeed(t, h.Owner, testkit.TimerA, testkit.RecordA)
    if stop.Status != "applied" || len(stop.RelatedEntities) != 1 { t.Fatalf("stop=%+v", stop) }
    feed := h.ReadFeed(t, testkit.RecordA).Payload.(protocol.FeedPayload)
    if feed.VolumeML == nil || feed.VolumeML.String() != "60" || len(feed.Sides) == 0 {
        t.Fatal("stop lost combination-feed content")
    }
    later := h.StopFeed(t, h.Caregiver, testkit.TimerA, testkit.RecordB)
    if later.Status != "superseded" || h.Count(t,"feed_entries",h.FamilyID) != 1 {
        t.Fatal("second stop synthesized another completion")
    }
}
```

`VolumeML` is P2.1's nullable exact decimal wrapper with `String() string`. Testkit constants are canonical distinct UUIDs declared in its fixtures file. `TestLostStartCollisionCannotRetargetCommands` starts A, proposes B and drops B's response, retries identical B, asserts replay `original_status:superseded` and canonical ID A, then submits switch/bottle/stop for B; all are terminal absent/ended results and A is untouched. `TestConcurrentStopExactlyOnce` gates simultaneous stops and checks one completion and two stop journal rows total. `TestStaleStopDoesNotEndNewTimer` stops A, starts B, retries A under a fresh ID, and verifies B active. Add side/bottle/stop ordering permutations, null bottle removal, invalid timestamp order, completion UUID collision, and failure after the first journal insert.

Add `TestTimerCompletionRejectsEveryFamilyUUIDCollision` in `timers_test.go`. Run both feed-stop and sleep-stop against occupied baby/feed/sleep/diaper/timer UUIDs with each target live and tombstoned, plus a separate target equal to the stopping timer's own ID. Each case snapshots the active timer/version, all canonical row counts, family high water, journal count, and receipts before the stop. Assert `rejected/completed_record_exists`, unchanged timer/version/row counts/high water/journal, and exactly one durable **rejected** receipt carrying no applied cursor/related completion; retry returns `already_applied/original_status:rejected`. Add a constraint-failure case after collision admission to prove no partial receipt survives rollback. Implement fixture helper `OccupiedEntity(t,kind,tombstoned) string` using ordinary authenticated create/delete commands on an unrelated baby, preserving the running timer's parent.

```go
func TestTimerCompletionCannotReuseItsOwnID(t *testing.T) {
    h := testkit.New(t)
    h.StartFeed(t,h.Owner,testkit.TimerA)
    before := h.FamilyHighWater(t)
    result := h.StopFeed(t,h.Owner,testkit.TimerA,testkit.TimerA)
    if result.Status != "rejected" || result.Code == nil || *result.Code != "completed_record_exists" {
        t.Fatalf("self-ID stop=%+v",result)
    }
    if h.FamilyHighWater(t)!=before || !h.TimerIsLive(t,testkit.TimerA) || h.Count(t,"feed_entries",h.FamilyID)!=0 {
        t.Fatal("completion collision partially changed canonical state")
    }
}
```

Define `FamilyHighWater(t) protocol.Cursor` and `TimerIsLive(t,id) bool` in `testkit/fixtures.go` using guarded runtime reads. Include the collision table and self-ID test in RED/GREEN selections with `TestTimerCompletion`.

`TestChildDeletionIsAtomicOrderedCascade` seeds all record kinds/timers, invokes owner delete, expects baby first then feeds, sleeps, diapers, timers, UUID-sorted within kind, all tombstones in a contiguous sequence range and no synthetic completion. `TestDelayedChildMutationGetsBabyDeleted` submits create/update/timer-start/side/bottle/stop below the child tombstone and expects permanent `baby_deleted`. Caregiver child deletion must fail authorization before state/receipt changes.

- [ ] **Step 2 — verify RED.** Run `rtk proxy env BT_TEST_RUN='TestBottle|TestLostStart|TestConcurrentStop|TestStaleStop|TestTimer|TestChildDeletion|TestDelayedChild' make server-test`; expect no command/cascade implementation, not skipped DB tests.
- [ ] **Step 3 — implement timer tables and command dispatcher.** Add the partial index and composite child FK; add RLS to timer interval rows. Reuse family lock before locking a timer row. Every accepted change retains authenticated creator/editor and acceptance time; imported starts use importing caregiver. Dispatch on exact operation, never replace a timer wholesale.

```sql
CREATE UNIQUE INDEX one_live_timer_per_kind
ON active_timers(family_id, baby_id, timer_kind)
WHERE tombstoned_at IS NULL;
SELECT * FROM active_timers
WHERE family_id=$1 AND id=$2 AND baby_id=$3 FOR UPDATE;
```

- [ ] **Step 4 — implement cascade and deterministic multi-entity receipts.** Under owner authorization and the family lock, lock baby, read live dependents in fixed kind/UUID order, tombstone each and increment each version, then call `records.AppendJournal` once with the complete ordered set. Stream/batch SQL within one bounded transaction; if statement/context/size limits interrupt it, roll back all rows and cursor. One child deletion receipt can store root tombstone and final cursor while journal supplies all dependent changes; never embed an unbounded cascade in outcome JSON. Include timer snapshots in P2.3 materialization. Enable `timer-commands-v1` after tests pass.
- [ ] **Step 5 — GREEN: repeat focused tests with at least 100 concurrent-stop interleavings.** Use `-count=100` only for the small timer race subtests, then restore ordinary full-suite count. Every run must use the runtime role and assert journal+receipt+entity counts.
- [ ] **Step 5a — compatibility RED/GREEN and overlap 10→11.** Capture schema-10 accepted head as `BT_MIGRATION_BASE_REF`; commit final 000011, full v11 snapshot/scenarios, and `[10,11]` with no timer feature dependency; capture `BT_MIGRATION_PRIOR_REF`. Implement timers in a second `[11,11]` commit and capture `BT_MIGRATION_FEATURE_REF`. Prepared mode runs predecessor record/snapshot behavior at 10 and 11, then feature timer commands at 11, with unchanged archives, complete role denial, and changed/unknown digest rejection.
- [ ] **Step 6 — full suite.** Run `rtk make server-test`, `rtk proxy sh -c 'cd Server && go vet ./...'`, and fixture route conformance. No Swift outbox implementation belongs in this commit.
- [ ] **Step 7 — commit and overlap.** Stage exact P2.4 feature files with `[11,11]`, commit `feat(server): serialize shared timer commands and child deletion`, capture `BT_MIGRATION_FEATURE_REF`, then run prepared-mode 10→11 using all three refs.

## Task 5: Durable import mapping and owner conflict resolution (P2.5)

- [ ] **Step 0 — commit the schema-12 compatibility predecessor.** Capture accepted schema-11 feature head; land final 000012, complete v12 privilege snapshot/scenarios, and `[11,12]` without import feature use; run schema-11 gates; commit `feat(server): prepare import schema compatibility`; review and capture the base/prior refs.

**Files:**

- Create: `Server/internal/importer/{resolver,manifest,conflict,repository}.go`, `resolver_test.go`, `conflict_test.go`; `Server/internal/httpapi/import.go`, `import_test.go`; `Server/migrations/000012_import_resolver.sql`.
- Modify: `Server/internal/sync/mutation.go`, `Server/internal/records/repository.go`, `Server/internal/httpapi/router.go`, `Server/cmd/baby-tracker-server/main.go`, `Server/internal/protocol/import.go`, `Server/internal/migrate/compatibility.json`, `Server/internal/migrate/overlap_test.go`, `Server/scripts/check-migration-overlap.sh`, `Server/internal/testkit/fixtures.go`, `Server/api/openapi.yaml`, `Server/api/fixtures/v1/{import-resolve,import-conflict,errors}.json`, `docs/protocol/v1.md`.

**Interfaces and D9 resolution:**

```go
type ImportBaby struct { LocalBabyID, Name, BirthDate string; SortOrder uint32 }
type RecordManifest struct {
    RecordID, EntityKind, LocalBabyID, ContentSHA256 string
}
type ImportResolveRequest struct {
    ProtocolVersion string; Capabilities []string
    ImportID, ImportMode, NormalizationVersion string
    Babies []ImportBaby; Records []RecordManifest
    TotalRecordCount uint32; ChunkIndex uint32; Finalize bool
}
type BabyMapping struct { LocalBabyID, SharedBabyID string }
type ImportConflict struct {
    ConflictID, Kind, LocalBabyID string
    RecordID *string; CandidateBabyIDs []string
    CandidateVersions []uint64
}
type ImportResolveResponse struct {
    ImportID string; ResolverRevision uint64; NormalizationVersion string
    Mappings []BabyMapping; Conflicts []ImportConflict; MappingComplete bool
    ConflictCount uint32; ConflictsComplete bool; NextConflictPageToken *string
    NextChunkIndex uint32; ManifestComplete bool
}
type ImportConflictRequest struct {
    ProtocolVersion string; Capabilities []string
    CommandID string; ResolverRevision uint64; ConflictID, Choice string
    CanonicalBabyID *string
}
func (s *Service) Resolve(context.Context, access.Principal, protocol.ImportResolveRequest) (protocol.ImportResolveResponse,error)
func (s *Service) Status(context.Context, access.Principal, string, *string) (protocol.ImportResolveResponse,error)
func (s *Service) ResolveConflict(context.Context, access.Principal, string, protocol.ImportConflictRequest) (protocol.ImportResolveResponse,error)
func (s *Service) AdmitRecord(context.Context, pgx.Tx, access.Grant, string, protocol.Mutation) (protocol.Outcome,bool,error)
// AdmitRecord's string is authenticated deviceID; bool says it handled an import.
```

Routes: POST `/v1/family/import/resolve` stages a chunk and optionally finalizes; GET `/v1/family/import/{import_id}` resumes durable status and accepts only the optional strict `page_token` query; POST `/v1/family/import/{import_id}/resolve-conflict` makes one owner choice. The exact capability set on all three routes and in both POST bodies is `records-v1,import-resolver-v1`. Header version/capabilities are mandatory on all; conflict POST carries the same negotiation fields as other JSON requests. Import is bound to family, initiating account, initiating device, and import UUID; any authenticated session/device of that same account can resume. Owner may inspect/resolve a member's conflicts but cannot silently replace their import manifest. Membership removal denies import immediately.

Chunk 0 includes the complete bounded baby candidate list, `import_mode:preserve_local_ids|match_shared`, and declared total record count, and freezes the initiating device, mode, candidates, count, and normalization version. Create-family onboarding uses `preserve_local_ids`; join onboarding uses `match_shared`. The mode selects identity handling under ordinary add-baby permission, not an extra authorization grant, and becomes immutable when chunk 0 commits. Later chunks include empty `babies`, the same mode/normalization version/total, next contiguous index, and <=1000 records. The canonical chunk hash includes every immutable field and `finalize`; each committed chunk stores a byte-stable response status/body. Duplicate identical chunk bytes after canonicalization return that stable response; changed content reusing `(import_id,chunk_index)` is `import_chunk_reused`; gaps are `import_chunk_out_of_order`. Reject empty nonfinal chunks. `finalize:true` is permitted only on the chunk that brings the stored count exactly to the declaration, including zero-record imports. Validate uniqueness of record IDs/kinds and candidate local UUIDs across all chunks. Staging commits independently; resolution does not begin before manifest complete. Every session with `mapping_complete:false`, including a finalized ambiguous import, expires at its deadline. Expiry marks the retained ID row and makes every reuse return `import_expired`; P4 later deletes bulky children in bounded batches while retaining that small row until whole-family purge.

New import IDs are admitted under the family row lock only while the retained-history counts are below `BT_IMPORT_MAX_SESSIONS_PER_FAMILY=1000` and `BT_IMPORT_MAX_SESSIONS_PER_ACCOUNT=100`, counting active, completed, and expired rows. Exceeding either cap returns nonretryable `import_history_limit`; retries/status for an existing ID remain admitted. The separate concurrency caps remain four unfinished imports per family and one unfinished import per account globally. Session, chunk, candidate, manifest, conflict, receipt, and mapping bytes all count toward family storage.

Finalization and all ordinary baby creates/renames/deletes acquire the **same family row FOR UPDATE**. In `preserve_local_ids` mode, map each local baby UUID to itself and create one separate canonical baby per local UUID even when names/dates match. An existing same UUID is accepted only if its canonical baby semantics match; different content/kind or a tombstone is `baby_uuid_collision`. In `match_shared` mode, build the entire match graph against the shared rows present at the start of this locked resolution, excluding proposed new siblings from matching each other. Zero shared matches retains **each** local UUID separately; one shared match adopts its UUID only when doing so does not map two distinct local babies to one target; multiple shared matches persist `ambiguous_baby`. A many-local-to-one-shared assignment persists `ambiguous_local_babies` and blocks automatic mapping. Enforce a unique `(family_id,import_id,shared_baby_id)` in the final mapping table so an implementation cannot collapse local children accidentally. Across separate caregivers' single-candidate `match_shared` imports the family lock still makes the second resolver see the first committed baby and converge on it; that is a distinct case from multiple children in one workspace.

No baby creations or mappings commit until all ambiguities resolve. Publish only a complete mapping, or `mappings:[]` with conflicts and `mapping_complete:false`. Completion atomically creates unmatched babies, calls `records.AppendJournal` with deterministic snapshots, and stores the complete mapping/revision. No lexicographic UUID winner, heuristic merge, or silent rewrite of sibling relationships is allowed.

Owner resolution takes the exact resolver revision, command ID, conflict ID, and `choice:use_existing|keep_local`. `use_existing` requires `canonical_baby_id` to be one of the conflict's candidate IDs and not already assigned to another local baby in this import; `keep_local` requires null `canonical_baby_id` and preserves this conflict's original local UUID, which must be unoccupied or exact same canonical baby. Check owner role inside the guard. Candidate version/name/date/deletion changes invalidate the choice, increment resolver revision with refreshed candidates, and return HTTP 409 `import_resolution_stale`. Same command ID/content returns its stable response; different content is `command_id_reused`. This choice never merges/deletes shared babies or distinct local babies. Caregiver polls and resumes once complete. `baby_uuid_collision` remains an explicit permanent conflict with no invented alternate UUID; surface user data repair/export rather than violating retention.

Conflict reporting is deterministic and bounded independently of `mapping_complete`. Every resolve/choice response contains the first page and all responses include `conflict_count`, `conflicts_complete`, and nullable `next_conflict_page_token`; unresolved conflicts are ordered by UUID and never silently truncated. Reuse the effective sync item/byte limits and count the whole response envelope. Continuations are at most 512 bytes and use the existing page keyring with domain `baby-tracker-import-conflicts-v1`, binding server instance/recovery epoch, family/import ID, viewer account/device/session/security revision, resolver revision, last conflict UUID, effective item/byte limits, expiry, and key ID. Authenticate and authorize before a token affects lookup. Any conflict, decision, or visibility change increments resolver revision; a stale continuation returns HTTP 409 `import_resolution_stale` and restarts status paging. Owner decisions remain separate from final mappings; after every decision the resolver re-evaluates the complete exact-match graph under the family lock, and a newly appearing equal candidate also makes the decision stale.

After mapping, compare each record manifest to existing `(family,kind,recordID)` using mapped baby ID separately from the canonical content hash, which excludes `baby_id`. Equal content is a no-op admission that returns the canonical entity and its original cursor without a journal sequence or wake hint and persists that exact outcome receipt; unequal content is explicit `record_content_conflict`, and another UUID stays distinct. A tombstoned existing UUID is `record_deleted_conflict`, even if old content matched. Return record conflicts while `mapping_complete:true` if the baby mapping is durable; record conflicts do not block unrelated records or change mappings. On actual imported create, recompute content hash from submitted payload, require mapped baby ID and the original manifest entry, then recheck current canonical content under family lock: intervening edits yield conflict, never LWW. A completed mapping never changes because a baby is later renamed; if deleted, dependent import commands receive `baby_deleted`. Timer starts participate in the manifest with their original timer-start content hash and mapped baby and use ordinary start/collision semantics. Imported timer switch/attach/stop commands and imported record updates/deletes are rejected; timer completions never use record-equality deduplication.

**Concrete resumable request:**

```json
{"protocol_version":"v1","capabilities":["records-v1","import-resolver-v1"],"import_id":"10000000-0000-4000-8000-000000000001","import_mode":"match_shared","normalization_version":"nfc-trim-fold-nfc-v1","babies":[{"local_baby_id":"20000000-0000-4000-8000-000000000001","name":" Élie ","birth_date":"2026-01-12","sort_order":0}],"records":[],"total_record_count":0,"chunk_index":0,"finalize":true}
```

- [ ] **Step 1 — RED: add concurrent mapping and revision tests.** Extend testkit with `Importer *importer.Service` and `Import(t,p,id,name,date) ImportResolveResponse` which issues the complete zero-record request above using caller-supplied IDs. Add this test and the named fault cases.

```go
func TestConcurrentImportConvergesOnOneBaby(t *testing.T) {
    h := testkit.New(t)
    start := make(chan struct{})
    results := make(chan protocol.ImportResolveResponse, 2)
    var wg sync.WaitGroup
    for i, p := range []access.Principal{h.Owner,h.Caregiver} {
        wg.Add(1)
        go func(i int, p access.Principal) {
            defer wg.Done(); <-start
            // ImportRequest builds one candidate per caregiver, match_shared mode,
            // distinct import/local UUIDs and the same exact name/date.
            out, err := h.Importer.Resolve(t.Context(),p,testkit.ImportRequest(i,"Élie","2026-01-12"))
            if err != nil { t.Error(err); return }; results <- out
        }(i,p)
    }
    close(start); wg.Wait(); close(results)
    var ids []string
    for out := range results {
        if !out.MappingComplete || len(out.Mappings)!=1 { t.Fatalf("incomplete: %+v",out) }
        ids=append(ids,out.Mappings[0].SharedBabyID)
    }
    if len(ids)!=2 || ids[0]!=ids[1] || h.Count(t,"babies",h.FamilyID)!=1 {
        t.Fatal("concurrent resolver created duplicate canonical babies")
    }
}
```

Add a separate test for intentional same-name/date children in one workspace; it must never be folded into the cross-caregiver test above:

```go
func TestCreateFamilyImportPreservesDistinctEqualLookingLocalBabies(t *testing.T) {
    h := testkit.New(t)
    req := testkit.ImportRequest(0,"Élie","2026-01-12")
    req.ImportMode = "preserve_local_ids"
    a := req.Babies[0]
    b := a
    b.LocalBabyID = "20000000-0000-4000-8000-000000000002"
    b.SortOrder = 1
    req.Babies = []protocol.ImportBaby{a,b}
    out,err := h.Importer.Resolve(t.Context(),h.Owner,req)
    if err!=nil || !out.MappingComplete || len(out.Mappings)!=2 { t.Fatalf("resolve=%+v %v",out,err) }
    for _,m := range out.Mappings {
        if m.LocalBabyID!=m.SharedBabyID { t.Fatal("create-family import collapsed local identity") }
    }
    if h.Count(t,"babies",h.FamilyID)!=2 { t.Fatal("two local babies became one shared baby") }
}
```

`TestMatchSharedPreservesDistinctUnmatchedLocalBabies` repeats the two-candidate request in `match_shared` against an empty family and expects both UUIDs retained. `TestJoinLocalAmbiguityRequiresDistinctOwnerChoices` seeds one shared exact baby, submits two same-name/date local babies, expects `ambiguous_local_babies` and no mapping, then owner chooses `use_existing` for the first and `keep_local` for the second; assert two distinct shared targets and each imported feed/timer reference stays with its selected local child. Reusing the first shared target for the second choice returns `import_target_already_assigned` without partial mapping. `TestPreserveIDsModeDoesNotFuzzyMatchExistingBaby` proves explicit preserve mode never silently adopts a different equal-looking UUID. Include modes/choice values in OpenAPI and contract fixtures in this task; persist the mode with the chunk hash so retry cannot switch it.

`TestOwnerResolutionRejectsChangedCandidateRevision` seeds two equal live babies via ordinary creates, finalizes caregiver import (no mapping), changes one candidate through ordinary update, posts the old owner choice, expects `import_resolution_stale` and zero mapping/baby additions, then resolves the refreshed exact conflict and resumes as caregiver. Add `TestEqualRecordManifestAfterRemapIsNoOp`, `TestDifferentContentImportNeverUsesLWW`, `TestDifferentRecordUUIDsRemainSeparate`, `TestImportUUIDCollisionDoesNotOverwrite`, `TestManifestChangedChunkRejected`, `TestImportResumeAfterEachCommitBoundary`, `TestMappingNeverChangesAfterRename`, and `TestDeletedMappedBabyTerminatesImportWrites`.

- [ ] **Step 2 — verify RED.** Run `rtk proxy env BT_TEST_RUN='TestConcurrentImport|TestOwnerResolution|TestEqualRecord|TestDifferentContent|TestDifferentRecord|TestImport|TestManifest|TestMapping|TestDeletedMapped|TestCreateFamilyImport|TestMatchShared|TestJoinLocalAmbiguity|TestPreserveIDsMode' make server-test`; expected missing resolver/staging behavior.
- [ ] **Step 3 — implement durable staging and import tables.** Add RLS/foreign keys for exactly `import_sessions`, `import_chunks`, `import_baby_candidates`, `import_record_manifests`, `import_conflicts`, `import_conflict_receipts`, and `import_baby_mappings`; every row has family ID. Sessions store initiating account/device, immutable import mode, normalization version, declared/count/finalized state, revision, creation/expiry/expired marker and immutable final mapping marker. Add the unique per-import shared target constraint, persist both conflict-choice kinds separately from mappings, and verify whole-map injectivity before creating rows. Chunk hashes and byte-stable chunk/conflict response receipts preserve lost-response idempotency. The only complete-map transition occurs under family lock after all candidate decisions validate. Journal new baby entries in local baby UUID order and use importing account as creator/editor with preserved domain dates. Runtime may `SELECT,INSERT,UPDATE` only sessions/conflicts and `SELECT,INSERT` only the other five tables; runtime has no import-table `DELETE`, and PUBLIC/projector/revoker have no access. Add one fixed-`pg_catalog`-search-path SECURITY DEFINER unfinished-account count helper only for the global one-account concurrency cap, with exact current-account validation and EXECUTE limited to runtime.

```sql
SELECT next_sequence FROM families WHERE id=$1 FOR UPDATE;
SELECT id, entity_version FROM babies
WHERE family_id=$1 AND normalized_name=$2 AND birth_date=$3
  AND normalization_version=$4 AND tombstoned_at IS NULL
ORDER BY id;
-- No uniqueness constraint on name/date: legitimate existing duplicates must
-- remain explicit owner choices; the resolver lock prevents concurrent races.
```

- [ ] **Step 4 — integrate import admission without cycles.** Define an `ImportAdmission` interface in sync with the exact `AdmitRecord` signature above. Inject it into Service using `SetImportAdmission(ImportAdmission)` during construction before serving; importer depends on records/protocol, never sync. For an import mutation check receipt first, then import membership/checkpoint/manifest, then typed record equality, then ordinary insert only if absent. For no-op equal content return applied with existing canonical entity and its original entity cursor; save receipt without appending journal. Mutation retries retain that original outcome even if the canonical row later changes. Include imported baby resolution's deterministic operation identity in its stored checkpoint instead of borrowing a user mutation UUID.
- [ ] **Step 5 — GREEN: rerun focused tests and each interruption point.** Interrupt after a stage chunk, after conflict persistence, after owner choice, after complete-map commit, and after imported-record commit before response; retry must recover the same durable decisions. Cross-account status is denied except owner conflict review; foreign-family IDs never expose candidates/hashes.
- [ ] **Step 5a — compatibility RED/GREEN and overlap 11→12.** Capture schema-11 accepted head as `BT_MIGRATION_BASE_REF`; commit final 000012, full v12 snapshot/scenarios, and `[11,12]` without import feature use; capture `BT_MIGRATION_PRIOR_REF`. Implement import in the second `[12,12]` commit and capture `BT_MIGRATION_FEATURE_REF`. Prepared mode exercises already-accepted record create/read and timer start/stop at both schema 11 and schema 12 while all seven import tables remain unused; only the feature binary exercises import at 12. Archives remain unchanged, all structural role denials remain enforced, and changed/unknown digests are rejected.
- [ ] **Step 6 — full suite.** Run `rtk make server-test`, `rtk proxy sh -c 'cd Server && go vet ./...'`, and all OpenAPI request/response fixture tests. Enable `import-resolver-v1` after exact routes/limits exist; preserve all record UUIDs in convergence assertions.
- [ ] **Step 7 — commit and overlap.** Stage exact P2.5 feature files with `[12,12]`, commit `feat(server): resolve resumable imports with explicit owner conflicts`, capture `BT_MIGRATION_FEATURE_REF`, then run prepared-mode 11→12 using all three refs.

## Task 6: Authenticated cursor hints and durable hosted wake scheduling (P2.6)

- [ ] **Step 0 — commit the schema-13 compatibility predecessor.** Capture accepted schema-12 feature head; land final 000013, complete v13 privilege snapshot/scenarios, and `[12,13]` without live/delivery feature use; run schema-12 gates; commit `feat(server): prepare delivery schema compatibility`; review and capture the base/prior refs.

**Files:**

- Create: `Server/internal/realtime/{hub,websocket,postgres}.go`, `hub_test.go`, `websocket_test.go`, `postgres_test.go`; `Server/internal/delivery/{queue,repository}.go`, `queue_test.go`; `Server/internal/httpapi/live.go`, `live_test.go`; `Server/migrations/000013_delivery_pending.sql`.
- Create: `Server/tests/protocol/{harness,convergence,timer,reauth,import,restart}_test.go`; `Server/scripts/{protocol-e2e.sh,protocol_e2e_test.go}`; `docs/verification/family-sync-protocol.md`.
- Modify: `Server/internal/sync/{mutation,service}.go`, `Server/internal/importer/resolver.go`, `Server/internal/httpapi/{router,middleware}.go`, `Server/internal/httpapi/router_test.go`, `Server/cmd/baby-tracker-server/main.go`, `Server/internal/config/sync.go`, `Server/internal/migrate/compatibility.json`, `Server/internal/migrate/overlap_test.go`, `Server/internal/testkit/{fixtures,protocol_client}.go`, `Server/api/openapi.yaml`, `Server/api/fixtures/v1/live-hint.json`, `Server/go.mod`, `Server/go.sum`, `Server/scripts/{smoke.sh,smoke_test.go,check-principals.sh,check-migration-overlap.sh}`, `Makefile`, `Server/README.md`, `docs/protocol/v1.md`.

**Interfaces:**

```go
// internal/realtime; protocol.Hint is the same two-field wire DTO.
type Hint struct { FamilyID string; Cursor protocol.Cursor }
type Connection interface {
    WriteHint(context.Context, Hint) error
    Ping(context.Context) error
    Close(code int, reason string) error
}
// Hub is a concrete bounded registry implemented in hub.go. Reserve happens
// before upgrade and returns an idempotent release; Bind consumes that
// reservation only after WebSocket accept succeeds.
func (h *Hub) Reserve(context.Context, access.Principal, access.Grant) (*Reservation,error)
func (r *Reservation) Bind(Connection) (func(),error)
func (r *Reservation) Release()
func (h *Hub) Publish(context.Context, Hint)
func (h *Hub) Invalidate(context.Context, security.SecurityInvalidation)
func (h *Hub) Shutdown(context.Context) error
// internal/delivery, callable using a family-scoped runtime transaction:
type Queue interface {
    ScheduleFamily(context.Context, pgx.Tx, access.Grant, string, protocol.Cursor) error
}
// string = originating authenticated device; excludes that device from wakes.
// P4.2 worker seam, unavailable to request handlers:
type PendingWake struct {
    FamilyID, DeviceID string; Cursor protocol.Cursor
    NotBefore time.Time; Attempts uint32; Generation uint64
}
type WorkerRepository interface {
    Claim(context.Context, time.Time, int) ([]PendingWake,error)
    Complete(context.Context, PendingWake) error
    Retry(context.Context, PendingWake, time.Time, string) error
}
```

The worker interface records a narrow future consumer contract; implement `ScheduleFamily` and queue persistence now, implement claim/complete/retry with P4.2's dedicated delivery role. No APNs SDK/client/key, registration endpoint, global worker scan, or maintenance credential enters this task's request process.

Dependency selection was checked directly on 2026-09-20: Coder maintains this package and provides a context-aware `Accept`/`Dial`, bounded read limits, ping, and close APIs. Pin **github.com/coder/websocket v1.8.15**, whose release is tagged and whose source API was inspected. The package-index “latest” badge and release-page labeling can disagree; this plan claims a verified concrete release, not that no later version exists. Sources: [maintainer release](https://github.com/coder/websocket/releases/tag/v1.8.15), [versioned API documentation](https://pkg.go.dev/github.com/coder/websocket@v1.8.15). Do not import a web framework or legacy x/net/websocket.

- [ ] **Step 1 — validate and pin the dependency before writing its adapter.** In `Server`, run `rtk proxy go mod download -json github.com/coder/websocket@v1.8.15` and inspect only version/checksum metadata, then `rtk proxy go get github.com/coder/websocket@v1.8.15` and `rtk proxy go mod verify`. Read the downloaded `AcceptOptions`, `Accept`, `CloseRead`, `Ping`, and `SetReadLimit` documentation; verify it matches the links above. Run the repository's pinned vulnerability scan when available; a known applicable vulnerability blocks shipping that pin and requires a newly verified maintained version plus updated plan/lockfile, never a floating dependency. The public `Connection` seam remains standard-library compatible for tests/adapters.
- [ ] **Step 2 — RED: add real-upgrade, invalidation, and commit-crash tests.** Use `httptest.NewServer(httpapi.New(...))` and `websocket.Dial` with bearer header and `baby-tracker.v1`; exercise the **actual** request logging wrapper. Add `TestRealUpgradeThroughTrackedResponseWriter`, `TestUnauthorizedOriginAndSubprotocolDenied`, `TestSocketBoundsAndSlowConsumerClose`, `TestRemovalClosesMatchingSockets`, `TestQuarantineAndExpiredSessionCloseSockets`, `TestShutdownClosesHijackedConnections`, and a log capture test that searches for seeded note/name/token strings.

```go
func TestCommitBeforePublishSurvivesRestart(t *testing.T) {
    h := testkit.NewHosted(t) // same runtime fixtures, hosted mode, two eligible devices
    h.SuppressHintPublisher(t) // inject a no-op publisher; queue remains real SQL
    applied, err := h.Service.Apply(t.Context(),h.Owner,h.FamilyID,testkit.MustMutation(t,"baby.json"))
    if err != nil || applied.Status!="applied" { t.Fatalf("apply=%+v %v",applied,err) }
    h.RestartServices(t) // reconstruct from same DB/security stores, no memory reuse
    page := h.Pull(t,h.Caregiver,nil,nil)
    if len(page.Changes)!=1 || page.HighWater!=*applied.Cursor { t.Fatal("commit lost without hint") }
    pending := h.PendingWakes(t) // assertion-only migrator inspection
    if len(pending)!=1 || pending[0].DeviceID!=h.Caregiver.DeviceID || pending[0].Cursor!=*applied.Cursor {
        t.Fatal("durable wake intent lost between commit and publish")
    }
}
```

Define `NewHosted`, `SuppressHintPublisher`, `RestartServices`, `PendingWakes` in testkit; no production crash flag. A guard decorator returning a rollback error after scheduled queue insert proves no wake/hint survives aborted mutation. `TestWakeCoalescesLatestGeneration` applies three mutations and checks one pending/device row at maximum sequence; `TestSelfHostedNeverSchedulesWake` checks zero rows. Cross-instance listener tests kill/reconnect LISTEN, reorder hints, and confirm HTTPS eventually reaches the same fixed high water.

- [ ] **Step 2a — RED: add authenticated reauthentication-to-receipt-to-stop E2E.** Create `Server/tests/protocol/harness_test.go` and `reauth_test.go` under P1's `//go:build identitytest`; apply that tag to all six files in `Server/tests/protocol/{harness,convergence,timer,reauth,import,restart}_test.go` so ordinary builds cannot import the test issuer. Reuse `Server/internal/testidentity`; create no second issuer. `newProtocolFixture(t)` starts the real application HTTP stack, real runtime/projector PostgreSQL roles and filesystem ledger/head, and creates owner/caregiver through HTTP. `ProtocolClient` privately retains the P1 session bundle, one-time device credential, and exact frozen mutation bytes; only opaque IDs may be printed. Define the exact helpers used below: `Owner() *ProtocolClient`, `DropNextSyncResponseAfterCommit()`, `RestartClient(*ProtocolClient) *ProtocolClient`, `RequireReauthByRefreshReuse(*ProtocolClient)`, `(*ProtocolClient).FrozenTimerStart(timerID string) []byte`, `SyncExact([]byte) (protocol.SyncResponse,error)`, `ReauthenticateWithContinuity()`, `StopTimer(timerID,recordID string) protocol.Outcome`, and `CountCompletedRecords(string) int`. The response-drop proxy must wait for the upstream committed HTTP response then close the downstream connection without forwarding it; it never rewrites the request. RestartClient serializes and restores only fixture-owned 0600 client-state storage, preserving the binding/frozen bytes; cleanup removes that exact test file.

```go
func TestLostTimerResponseReauthRecoversReceiptThenStops(t *testing.T) {
    h := newProtocolFixture(t)
    client := h.Owner()
    originalDevice := client.DeviceID
    frozen := client.FrozenTimerStart(testkit.TimerA)
    h.DropNextSyncResponseAfterCommit()
    if _,err:=client.SyncExact(frozen); err==nil { t.Fatal("expected lost committed response") }
    client = h.RestartClient(client)
    h.RequireReauthByRefreshReuse(client)
    client.ReauthenticateWithContinuity() // fresh nonce/Apple proof plus device_continuity
    if client.DeviceID!=originalDevice { t.Fatal("reauthentication lost receipt namespace") }
    response,err:=client.SyncExact(frozen)
    if err!=nil || len(response.Outcomes)!=1 { t.Fatalf("retry outcomes=%d err=%v",len(response.Outcomes),err) }
    receipt:=response.Outcomes[0]
    if receipt.Status!="already_applied" || receipt.OriginalStatus==nil || *receipt.OriginalStatus!="applied" ||
       receipt.CanonicalEntity==nil || receipt.CanonicalEntity.Meta.ID!=testkit.TimerA {
        t.Fatal("dependent stop lacks proof that this proposed timer was accepted")
    }
    stopped:=client.StopTimer(testkit.TimerA,testkit.RecordA)
    if stopped.Status!="applied" || h.CountCompletedRecords(testkit.RecordA)!=1 {
        t.Fatal("dependent stop did not produce exactly one completion")
    }
}
```

`RequireReauthByRefreshReuse` performs one successful refresh then submits the consumed refresh credential again and asserts real HTTP `refresh_reused` plus denied old-session sync before fresh Apple exchange. Assert the new SessionID differs, timer start has one journal entry, the replay has the original acceptance timestamp/cursor, no extra start or stop completion appears, and caregiver HTTPS pull materializes the completed feed. Duplicate the test with a **self-host absolute-deadline** fixture using the same P1 `auth.Options.Now` injected mutex-protected clock: advance 24 hours, assert refresh `reauthentication_required`, and run the identical fresh-proof/receipt/stop path; no production clock override is introduced. The container gate runs the refresh-reuse variant against the production application composition, while the deterministic absolute-age case runs the real HTTP/auth/DB stack with the injected clock.

Add `TestCrossAccountContinuityClaimCannotRecoverTimerReceipt`: account B signs fresh Apple proof and sends A's synthetic fixture `{device_id,credential}`, expects `device_continuity_invalid`, no session grant or receipt disclosure, and A's pending stop remains bound to A's installation. Add `TestMissingContinuityKeepsFrozenTimerDependencyBlocked`: client state loses its credential, surfaces `device_continuity_required`, sends no frozen mutation using a fresh device, and never manufactures a receipt from matching entity ID. Repeat the successful continuity case for an ordinary record create and repeat the lost-start collision case to assert `original_status:superseded` still cannot release switch/bottle/stop after reauth.

- [ ] **Step 3 — verify RED.** Run `rtk proxy env BT_TEST_RUN='TestRealUpgrade|TestUnauthorizedOrigin|TestSocket|TestRemoval|TestQuarantine|TestShutdown|TestCommitBeforePublish|TestWake|TestSelfHosted|TestPostgresHint' make server-test`. With the configured test DSN, run `rtk proxy sh -c 'cd Server && BABY_TRACKER_REQUIRE_DATABASE=1 go test -tags identitytest -race -count=1 ./tests/protocol/...'` with `TestLostTimerResponseReauthRecoversReceiptThenStops`, its absolute-age variant, cross-account/missing-continuity and superseded-start cases included. P1's ordinary `test-identity.sh` does not select this build tag; do not use its untagged package invocation for protocol E2E. Expect missing socket/queue/E2E behavior, not an unresolved helper, excluded tests or skipped database. A mocked upgrade or hand-constructed Principal is insufficient.
- [ ] **Step 4 — implement WebSocket adapter and wrapper support.** Authenticate before upgrade, validate an explicitly present Origin equals the configured canonical HTTPS origin exactly, and allow absent Origin for native clients. Validate required subprotocol before allocating a connection; bearer tokens never go in URL/subprotocol. Apply P1 connection limits before upgrade. `AcceptOptions` also restrict origins and disables compression. Add `Unwrap() http.ResponseWriter` to the tracking writer and prove the real library can unwrap/hijack; record 101 once, and do not write JSON after hijack/panic.

```go
func (w *trackedResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

c, err := websocket.Accept(w,r,&websocket.AcceptOptions{
    Subprotocols: []string{"baby-tracker.v1"},
    OriginPatterns: []string{canonicalOrigin},
    CompressionMode: websocket.CompressionDisabled,
})
if err != nil { return }
defer c.CloseNow()
c.SetReadLimit(1024)
// App-lifetime context, independent of the hijacked request context.
readCtx := c.CloseRead(connectionCtx)
// CloseRead processes ping/pong and closes on any application message.
```

`connectionCtx` is derived from app shutdown with a deadline at the earlier of `Principal.AccessExpiresAt` and accepted-at plus 15 minutes. `CloseRead` must run while ping/write loops operate. `Hub` coalesces to the highest sequence for the same instance/epoch/family; epoch change closes and requires HTTP reauth. Bound writes/pings to 5 seconds, ping every 25 seconds, and remove registry entries idempotently on any termination. One `LiveAuthorizer.CheckMany` call revalidates every registered lease in a single authority-fenced batch every 5 seconds; one Hub-owned lossy invalidation subscription is the fast path, so sockets do not create per-connection authority subscriptions. Invalidation closes matching account/family/session with 1008; shutdown uses 1001 and hard close after 5 seconds. IP and authenticated socket-attempt limits are charged before upgrade, then `Hub.Reserve` holds process-local capacity until `Bind` or idempotent release, allowing HTTP 429 before hijack. No socket writes while holding DB transactions.

- [ ] **Step 5 — implement durable work and post-commit hint publishing.** Add `delivery_pending(family_id,device_id,instance_id,recovery_epoch,sequence,generation,not_before,attempts,created_at,updated_at)` with PK `(family_id,device_id)`, forced RLS, and composite references where appropriate. In hosted mode, `ScheduleFamily` executes in the exact mutation/import transaction and schedules every other currently active family device (token existence is not needed; P4.2 filters eligibility and cleans unusable work). If Plan 1 devices are account-scoped, provide a tightly parameterized `SECURITY DEFINER app.schedule_family_wakes(familyID,actorDeviceID,cursor...)` that rechecks current transaction account/family membership, grants only EXECUTE to runtime, uses fixed search_path, and exposes no device list/token. It may only insert/update that authorized family's rows. Self-host is a no-op.

```sql
INSERT INTO delivery_pending(family_id,device_id,instance_id,recovery_epoch,sequence,generation,
                             not_before,attempts,created_at,updated_at)
VALUES ($1,$2,$3,$4,$5,1,now(),0,now(),now())
ON CONFLICT (family_id,device_id) DO UPDATE SET
  instance_id=EXCLUDED.instance_id, recovery_epoch=EXCLUDED.recovery_epoch,
  sequence=GREATEST(delivery_pending.sequence,EXCLUDED.sequence),
  generation=delivery_pending.generation+1, updated_at=now();
```

Epoch change must replace, not `GREATEST`, an old-epoch sequence and reset obsolete claims; add the explicit CASE for differing `(instance_id,recovery_epoch)` in production SQL and test it. Preserve throttled `not_before` on ordinary coalescing so new edits cannot bypass P4.2's backoff. `created_at` is the current generation's birth time and refreshes, with `updated_at`, only for a genuinely newer cursor. Generation lets that later worker compare the full `(instance_id,recovery_epoch,sequence)` cursor plus generation before completing/retrying, preventing delete/reinsert ABA and preserving newer hints.

Define `CommitObserver` in sync as `Schedule(ctx,tx,grant,deviceID,cursor) error` plus `Publish(ctx,cursor)`. A concrete delivery/realtime adapter calls Queue in-transaction, then Hub after guard success. Import finalization uses the same observer. Publish once for the highest cursor per mutation/cascade; no-change receipts schedule nothing. Execute `SELECT pg_notify('family_cursor_v1',$1)` inside the committing transaction with JSON `{family_id,cursor}` so PostgreSQL delivers only at commit; maintain a dedicated reconnecting pgx LISTEN connection for cross-instance hints. Validate hint length <=1024 and all cursor fields; never treat channel payload as authorization. On listener reconnect broadcast a bounded resync hint by reading each registered family's authorized committed cursor. Client HTTPS reconciliation remains mandatory even if local publication and LISTEN both miss an event.

- [ ] **Step 6 — GREEN: rerun focused tests and real two-client container scenarios.** Repeat the Step 3 focused server command and `rtk proxy sh -c 'cd Server && BABY_TRACKER_REQUIRE_DATABASE=1 go test -tags identitytest -race -count=1 ./tests/protocol/...'`. Implement `protocol-e2e.sh` using the pinned production Dockerfile/application composition plus Plan 1's compile-time isolated test issuer; test server code is identical except issuer composition, and the release binary cannot select it at runtime. It launches isolated PostgreSQL/security volumes, explicitly migrates, exports the test DSN and `BABY_TRACKER_REQUIRE_DATABASE=1`, and invokes `go test -tags identitytest -race -count=1 ./tests/protocol/...` from `Server`. In `Server/scripts/protocol_e2e_test.go`, add `TestProtocolE2EUsesIdentityTestTag` and `TestProtocolE2EPropagatesTestFailure` using a stub Go executable to capture argv/environment and a nonzero exit; require the tag, race/count flags, database-required environment and failure propagation. The tagged scenarios use two clients to create/join/import, synchronize every feed/sleep/diaper variant, retry a commit whose response was dropped, run timer bottle/stop collisions, snapshot during edits, remove a member, reconnect after lost/reordered hints, and restart the server. Assert persisted receipt/journal/queue and final client materialized state, not only 200/101 responses. Record exact binary/image SHA and zero orphan test volumes. P4.6 extends this suite with export/purge/restore/load; do not implement those features here.
- [ ] **Step 6a — run continuity and identity-preservation scenarios through the container.** `protocol-e2e.sh` must execute `TestLostTimerResponseReauthRecoversReceiptThenStops` with a real dropped response, client restart, refresh-reuse revocation, fresh Apple proof plus retained device credential, exact receipt replay, and one completed record observed by the second client. Run the denied cross-account continuity claim as a separate case. Exercise create-family two-distinct-equal-looking babies and retain both original UUIDs/dependent references; run the separate single-candidate cross-caregiver matching case and assert its one canonical baby. Record all four outcomes in `docs/verification/family-sync-protocol.md`, with only opaque IDs and no credential/proof contents.
- [ ] **Step 6b — compatibility RED/GREEN and overlap 12→13.** Capture schema-12 accepted head as `BT_MIGRATION_BASE_REF`; commit final 000013, complete v13 snapshot/scenarios, and `[12,13]` without live/delivery feature use; capture `BT_MIGRATION_PRIOR_REF`. Implement sockets and wake scheduling in the second `[13,13]` commit and capture `BT_MIGRATION_FEATURE_REF`. Prepared mode runs predecessor import/sync at 12 and 13, then feature live/delivery behavior at 13, with unchanged archives, structural denial, and changed/unknown digest rejection.
- [ ] **Step 7 — full suite and handoff gate.** Run `rtk make server-test`, `rtk proxy sh -c 'cd Server && go vet ./...'`, `rtk make server-build` (both linux/amd64 and linux/arm64), `rtk make server-smoke`, `rtk proxy bash Server/scripts/protocol-e2e.sh` (the explicit tagged/race/uncached Go invocation from Step 6), and `rtk make test` (existing Swift count remains 233 because this plan adds no Swift tests). Require executed named protocol cases in the report; an ordinary untagged full suite is not protocol E2E evidence. Update smoke assertions from the foundation's literal five tables/two migrations to the exact embedded migration catalog and all owned/forced-RLS table grants; do not weaken them to “at least one.” Read only bounded redacted logs. Check no handler constructor references a maintenance credential and no socket payload contains entity data. Record failures/blockers honestly in `docs/verification/family-sync-protocol.md`.
- [ ] **Step 8 — commit and overlap.** Stage exact P2.6 feature files, including all six tagged protocol tests, E2E script tests, client, and manifest `[13,13]`; inspect the staged diff; commit `feat(server): deliver cursor hints with durable wake scheduling`; capture `BT_MIGRATION_FEATURE_REF`; then run prepared-mode 12→13 using all three refs. Do not amend accepted commits.

## Mandatory review amendments for P2 acceptance

These additions resolve the cross-plan preflight and are owned by the named P2
task. They supersede the single global capability literal in D5.

### P2.1 negotiation, limits, and canonical fixtures

- [ ] Add one route-capability matrix to `internal/protocol` and OpenAPI:

  | Surface | Advertised/required when active | Allowed request tokens |
  | --- | --- | --- |
  | Auth challenge/exchange/refresh/logout | `identity-v1`; continuity request additionally requires advertised `device-continuity-v1` | Endpoint minimum only |
  | Family/lifecycle | `family-lifecycle-v1`; recovery routes use `recovery-v1` | Endpoint minimum only |
  | `/v1/sync` | `records-v1`; timer mutations also require `timer-commands-v1` | Negotiated subset used by the batch |
  | `/v1/sync/snapshot` | `records-v1,snapshot-v1` | Exactly both |
  | Import resolver/status | `records-v1,import-resolver-v1` | Exactly both for POST; same headers for GET |
  | `/v1/live` | exactly `records-v1,live-hints-v1` plus `baby-tracker.v1` subprotocol | Header only; clients without advertised `live-hints-v1` remain polling-only |

  POST body capabilities must equal the header set. Missing required,
  unadvertised known, unknown, duplicate, or excess tokens return
  `unsupported_capability`. UI and dispatch use `/v1/server` advertisement;
  registry membership alone is never support.
- [ ] Extend `ServerInfo` with additive effective limits for mutation item/body
  bytes, pull/page, snapshot, import chunks/manifests/status polling, and socket
  counts. Validate configured values against protocol minima. Fixtures cover the
  minimum legal custom-server values and encoded-byte splitting.
- [ ] Define one shared, digest-manifested corpus under `Server/api/fixtures`
  for every entity/timer/delete/import payload; decimal and timestamp ties;
  Unicode normalization; null/omitted/empty distinctions; nested duplicate
  keys; unknown optional/required response kinds; and safe-integer boundaries.
  Accepted values round-trip through PostgreSQL typed columns to identical
  canonical JSON/hash. P3 consumes this single source through test support, not
  a manually copied second corpus.
- [ ] Add `FamilyResolver.Current(ctx,principal) (familyID string,error)` and
  inject it into sync, snapshot, import, and live services. Resolve and
  authorize before cursor/token/import lookup. Account-only, recovery-only,
  no-slot, and cross-family inputs deny without tenant access.
- [ ] Integrate P1 `Limiter.TakeMany` at handlers. One mutation batch atomically
  charges item count across device/account/family before any item transaction;
  pulls/pages/snapshots/import status and sockets have their declared scopes.
  A denial returns stable 429 and bounded `Retry-After` with no partial charge
  or durable state. Socket caps are explicitly process-local until a lease
  store exists and are reported as such.

### P2.2-P2.5 storage, paging, import, and deletion

- [ ] Every migration 000009-000013 inserts a complete exact runtime/projector
  privilege snapshot and advances `schema_compatibility` in its transaction.
  New tenant tables are forced-RLS; runtime receives only listed table/column/
  function rights; projector receives no canonical-data DML; PUBLIC receives no
  definer execution; LOGIN roles have no direct ACLs. Schema 8 represents
  `UPDATE(public.families.next_sequence)` as a column privilege and rejects
  table-wide UPDATE.
- [ ] Configure `BT_PAGE_TOKEN_KEY_FILE` with the strict 32-byte lowercase-hex
  mounted-secret parser and bounded key ID. Page tokens carry the key ID;
  current plus retained previous keys validate until maximum page lifetime.
  Restart with the same key succeeds; missing/changed-only key fails safely.
- [ ] Canonical rows carry `last_sequence`, updated atomically with each journal
  append. Equal import content returns that original entity cursor, including
  after later family changes; a tombstoned entity never fabricates the current
  family high-water.
- [ ] Enforce a configurable maximum canonical-record count/storage budget per
  family that makes one atomic owner child cascade feasible. Reject admission
  before crossing the bound. Measure child deletion near the maximum under its
  dedicated transaction deadline and require rollback on timeout; do not leave
  a child permanently undeletable.
- [ ] Abuse tests persist across restart and include concurrent child 20→21,
  four unfinished imports/family and one/account, two active snapshots/account
  and eight/family, 100,000-record manifest accounting, item/body/page encoded
  bytes, and socket-cap release/process-local documentation. Every denial is
  privacy-safe and has no partial state.

### P2.6 worker boundary and real-container acceptance

- [ ] The `[12,13]` predecessor creates the delivery table/function and v13
  privilege snapshot but constructs neither Queue nor live route. Only the
  later schema-13 feature composition advertises `live-hints-v1`, registers
  `/v1/live`, and constructs the family-scoped Queue.
- [ ] Schema 13 feature production composition constructs only the family-scoped
  delivery `Queue`. `WorkerRepository` cannot be constructed with runtime,
  projector, or migrator credentials. P4.2 alone activates cross-family work
  through its delivery role and fixed functions; add constructor/source and
  real-role denial tests.
- [ ] The black-box protocol client sends traffic to the built container's
  reported listener. Evidence records image/binary digest, source SHA/GOARCH,
  PostgreSQL/schema instance, authority initialization, and process restart.
  Tagged in-process Go tests remain integration tests and cannot substitute for
  this route proof.
- [ ] Add real-HTTP ordering tests for entitlement suspension, member removal,
  family deletion, `epoch_changed`, and rate denial before authentication,
  after a page/token, and while a socket is open. Denied paths leave no row,
  receipt, cursor, import/snapshot checkpoint, durable wake, or entity-bearing
  socket message. A removal that races an entered shared-fence transaction must
  wait for that bounded transaction, not appear to commit inside it.
- [ ] Define `TestOlderDatabaseProtocolReconciliation`: after data is committed,
  restore an older PostgreSQL snapshot while preserving latest authority,
  quarantine, rotate epoch, reconcile, deny old sessions/cursors/pages/sockets,
  reauthenticate, install a new-epoch snapshot, and replay only
  never-acknowledged frozen intent. P2 defines and exercises the assertions
  against its restore seam; physical dump/restore execution belongs only to
  P4.5's harness and is not a P2 completion claim.
- [ ] Run `TestCanonicalCorpusRoundTripsGoAndPostgres`,
  `TestWeightedAdmissionIsAllOrNone`,
  `TestSchemaPrivilegeSnapshotSupportsRevocationOverlap`,
  `TestFamilyResolverRunsBeforeOpaqueLookup`,
  `TestEqualImportReturnsOriginalEntityCursor`,
  `TestChildDeletionNearQuotaIsAtomic`,
  `TestDeniedHTTPPathsPersistNothing`, and
  `TestProtocolE2EReachedReportedContainerDigest`.

P2 acceptance requires literal Go canonical bytes/hashes and PostgreSQL typed
round-trip equality. Swift consumes the same digest-manifested corpus in P3.1;
the combined Go/PostgreSQL/Swift gate executes in P3.1 and is rerun by P4, not
claimed by P2.

Every migration task's commit step is two commits, not one: first the reviewed
compatibility predecessor (`migration + full privilege snapshot + overlap +
[N-1,N]`), then the feature (`[N,N]`). Task reports record base, predecessor,
and feature SHAs and the exact prepared-mode overlap command.

## Self-review and execution handoff

Coverage: P2.1 owns D5/D7, typed field allowlists, existing combination feeds, exact hash/name/date/time semantics, and numeric limits. P2.2 owns typed canonical storage/RLS, commit order, attribution, receipts, LWW, and terminal tombstones. P2.3 owns bounded sync, opaque bound paging, fixed high water, and durable consistent snapshots. P2.4 owns D8, exact timer command semantics and atomic child cascade. P2.5 owns D9, bounded durable manifest staging, serialized full mapping, owner decisions, and explicit content/UUID conflicts. P2.6 owns real authenticated WebSocket upgrades, revocation/shutdown, cursor-only cross-instance hints, and durable in-transaction wake scheduling.

The five Review Focus entries each have a named failure-first test in the owning task. Interface names, routes, case-fold version, default bounds, migration range, and receipt/snapshot/import semantics are fixed here; no implementation may substitute an independent cursor allocator, generic canonical JSON store, fuzzy import merge, arbitrary timer replacement, or in-memory wake queue. A later client must consume both `status` and `original_status` on replay and must never use an outcome cursor as the pull checkpoint.

Planning self-review does not assert tests passed. Execution is complete only after all six task gates and the final container/Go/Swift checks have fresh evidence. Plan 1 implementation is a prerequisite, and its hosted external-provider gate remains separate. Real Apple, actual iOS shared-realm behavior, APNs delivery, export/purge, restored backup, deployment, privacy, and measured two-second freshness are Plan 3/4 gates and remain necessary for the complete public beta.

Decision costs: family-locked snapshot capture delays writes for its bounded materialization interval; measure that delay before increasing snapshot/transaction limits. Durable snapshot rows consume temporary disk and require Plan 4 expiry cleanup. Hash-only manifests avoid uploading full records twice but make cross-language canonical vectors a compatibility gate: a wrong client hash must produce an explicit manifest mismatch, never a destructive overwrite. Changing normalization or hash semantics after release requires a new version and migration/compatibility work rather than silently rematching families.

Review-fix costs and failure consequences: a column-only cursor grant still permits a compromised runtime to damage a cursor in its authorized tenant; it must not permit lifecycle writes, which remain ledger/projector-only. Exact next-digest overlap requires a preparatory prior-code artifact before every migration; skipping it deliberately causes readiness failure. Device continuity adds one installation credential retained in Keychain and revoked with the account/device; losing it blocks uncertain-intent recovery rather than guessing authority. Preserving local baby identities can retain equal-looking children and requires an explicit owner choice when joining would otherwise collapse them; `preserve_local_ids` intentionally prioritizes create-family UUID retention over name/date deduplication. Family-wide completion UUID checks reject occupied tombstone IDs permanently and require a new completion UUID for a new stop intent; replay of the original accepted stop remains stable. Shared capability grammar/error and decoder ownership prevent one platform silently accepting a wire contract another rejects.
