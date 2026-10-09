# Family Sync Identity and Lifecycle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a self-hostable Apple identity and family-management service whose authorization decisions survive database restore, with provider-neutral hosted seams and an explicit hosted-adapter gate.

**Architecture:** A single Go authority coordinates a globally ordered external security ledger, an independently stored committed head, and idempotent PostgreSQL projections. HTTP handlers consume bounded DTOs and application services; every authorized database transaction holds the same security fence used by lifecycle commands. Accounts, sessions, invitations, membership, entitlements, deletion scheduling and recovery are implemented here; canonical records, iOS integration, physical purge and deployment acceptance belong to the subsequent plans.

**Tech Stack:** Existing Go 1.26.2 module, pgx v5.11.0, PostgreSQL 17, standard-library cryptography/HTTP/JSON/filesystem locking, OpenAPI 3.1, existing digest-pinned Docker assets; no new production dependency in this plan.

**Spec:** `docs/superpowers/specs/2026-09-19-family-sync-server-design.md`; read it completely together with `.superpowers/planning/full-beta-decomposition.md` and `.superpowers/planning/server-beta-architecture.md`. The decomposition's shared handoff signatures take precedence over the architecture map's earlier illustrative signatures. The completed `docs/superpowers/plans/2026-09-20-family-sync-server-foundation.md` remains closed.

## Global Constraints

- “Local-only use remains fully functional without an account.”
- “Sign in with Apple is the only beta identity provider.”
- “An account may have one active family in the beta.”
- “Every live family has exactly one active owner; ownership transfer changes the old and new roles atomically and can never leave zero or two owners.”
- “The API is a checked-in OpenAPI 3.1 contract using JSON over HTTPS.”
- “All non-development connections require HTTPS, including custom servers.”
- “Custom TLS trust bypasses do not ship in release builds.”
- “Migration ownership, runtime tenant access, and maintenance duties never share one database credential.”
- “The application never auto-migrates on startup.”
- “Third-party CI actions are pinned to immutable commit SHAs.”
- “Container base images are pinned by digest, Go dependencies are locked by `go.sum`, and release CI produces a software bill of materials and runs Go vulnerability scanning.”
- “The hosted beta grants `family_sync` without payment.”
- “Self-hosted mode supplies an always-enabled entitlement policy.”
- “Self-hosted sessions instead rely on the iOS client's Apple credential-state check and require a new nonce-bound Apple identity token at an absolute maximum session age of 24 hours; refresh tokens cannot extend that deadline.”
- “Deleting a family disables all member access immediately and sets `purge_after` to 30 days later.”
- “The family continues to occupy the account's one-family beta slot until recovery, forfeiture, or purge.”
- “10 active caregivers per family”; “20 non-deleted children per family”; invitation default “seven days”. Child-count enforcement belongs to Plan 2.
- No StoreKit UI, Android/web client, E2EE, attachment handling/storage/UI, operator console, audit browser, Redis, Kubernetes, multi-region writes, or multiple-family beta API/UI.
- Never log Apple claims/tokens/codes, application/invitation/device-continuity secrets, names, IP addresses, record payloads or export data. No account/family/entity IDs in metric labels; the architecture map's looser label wording does not override spec §12.
- Preserve applied migrations `000001_server_state.sql` and `000002_tenancy.sql` byte-for-byte. Use additive migrations and focused Conventional Commits. All shell commands below use `rtk`.

## Review Focus

1. A restored database or previously authenticated request must not cross a newer security-head commit with stale authority — pinned by P1.2 `TestGuardRevocationWinsBeforeTransaction` and P1.7 `TestOlderDatabaseLifecycleReplay`.
2. A perfectly valid but old matching ledger/head pair cannot prove freshness — pinned by P1.1 `TestMissingTrustedHeadCannotRecover` and P1.2 `TestRecoveryManifestMismatchKeepsQuarantine`.
3. A lost refresh or mutation response followed by fresh Apple authentication must revoke reused sessions while preserving the authenticated device needed for the original receipt — pinned by P1.4 `TestRefreshLostResponseRevokesLineage`, `TestFreshAppleProofPreservesDevice` and the P2.6 handoff `TestLostTimerStartReauthRetrievesReceipt`.
4. A caregiver racing two invitation redemptions or create-versus-join must get exactly one family slot, including a reserved recovery slot — pinned by P1.5 `TestConcurrentFamilySlotClaims` and P1.6 `TestRecoveryClaimReservesSlot`.
5. Apple credential loss, departure and account deletion have different consequences; none may promote a caregiver, leak the former name or erase family data implicitly — pinned by P1.6 `TestCredentialLossDepartureAndDeletionAreDistinct`.

---

## Rulings and scope boundaries

| Decision | Ruling enforced by this plan |
| --- | --- |
| D1: rollback threat model | Database restore preserves the latest ledger and independent head. Deliberate/coordinated rollback or total loss of **both** security stores is outside the detectable threat model; an old mutually matching pair is never represented as rollback detection. Total-disaster recovery is a separate, quarantined operation requiring an operator-supplied trusted head manifest obtained outside that restore set. Without independently established latest revision/hash, recovery fails closed. Manifest provenance is an operator trust assertion, not a cryptographic freshness oracle. P4.5 supplies actual operational evidence. |
| D2: authority and fencing | One global chain/head; one atomic event per command containing all affected changes. Canonically sort/deduplicate account/family IDs, verify that they exactly equal payload-derived affected IDs, and revalidate on CAS loss. Command identity is actor account ID plus canonical command UUID; same canonical command hash returns the committed result, changed content returns `command_id_reused`. The beta runs on **one authority host**. A shared/exclusive OS file fence on a persistent local lock file covers full guarded transactions and complete command commit/projection; all API and CLI processes on that host use it. A separate local process mutex prevents same-process lock misuse. Cross-host authority starts fail config validation; introducing multiple hosts requires a separately reviewed distributed fencing design. Remote head CAS still protects immutable history and competing administrative writers. |
| D3: reconstructible, erasable state | The ledger carries IDs, enums, timestamps, digests and lifecycle links only. It includes grants/creation as well as revocations. An identity lookup key is `HMAC-SHA256(identityPepper, provider || NUL || audience || NUL || subject)`; raw subject/profile/Apple credentials remain erasable PostgreSQL data. Replay restores opaque authorization state, scrubs restored deleted identity/profile data and marks missing profiles for explicit repair. It never recreates bearer secrets, Apple refresh credentials or names. Epoch rotation invalidates every restored session; a fresh nonce-bound Apple proof and confirmed name repair missing non-authoritative profile material. |
| D4: rolling schemas | Introduce `Compatibility{Min, Max, Known []MigrationDigest}`. Reader readiness accepts only a contiguous, checksum-matching embedded manifest through `Current`, with `Min <= Current <= Max`; a numeric range alone is insufficient. Every migration after the historical 000004 transition uses two real commits: capture the accepted N-1 feature commit as `base-ref`; create and review a deployable compatibility-only predecessor commit that adds the final migration N, exact full privilege snapshot and manifest range `[N-1,N]` but no N-dependent feature path; then create the feature commit with range `[N,N]`. The overlap harness archives those two immutable commits without modifying either archive. Migration CLI `Apply` remains strict about every applied byte and never treats unknown SQL as safe. |
| D5: origin and compatibility | Public endpoints use origin only, never a base path. Remove OpenAPI `basePath`. All v1 JSON requests include `protocol_version:"v1"` and `capabilities:[]`; every capability token follows `^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$` with maximum 64 ASCII bytes. Auth/family values are `identity-v1`, `family-lifecycle-v1`, `recovery-v1`, `device-continuity-v1`. This lowercase-hyphen grammar and stable error `unsupported_capability` are shared by every plan; `family_sync` is an entitlement key, not a capability. Requests reject unknown/duplicate keys and trailing JSON; response decoders tolerate additive unknown fields. Required unknown capabilities return `unsupported_capability`; optional future resource kinds require authenticated snapshot recovery in Plan 2 and must not be silently acknowledged. No sync capability is advertised in Plan 1. |
| D6: nonce bytes | Canonical origin uses lowercase ASCII DNS host, lowercase `https`, removes port 443 and an empty/root path, retains valid nondefault port, rejects userinfo/query/fragment/non-root path, trailing-dot/empty labels, IP literals and non-ASCII host input. IDN hosts must be entered as ASCII A-labels. Instance ID is canonical nonzero lowercase UUID; challenge is exactly 32 random bytes encoded unpadded base64url. Hash UTF-8 bytes of `baby-tracker-auth-v1`, byte `0x00`, canonical origin, byte `0x00`, instance ID, byte `0x00`, **the challenge's base64url text**. Apple nonce is lowercase 64-character hex SHA-256. Independently selected origin/instance are compared before credentials; never follow a redirect when sending credentials. |
| D11: hosted inputs | Implement provider-neutral encrypted object/head/secret contracts and their conformance tests in P1.7. A concrete hosted adapter and its integration acceptance remain **blocked** until the operator supplies provider/account, European region, immutable object retention/encryption, independent replicated head store, key IDs/rotation ownership, domain/Apple audience/team/key/service configuration, finite backup retention/oldest-restorable watermark and checkpoint signer. AASA/APNs/signing values are also required by Plans 3–4. Do not provision accounts, choose billable infrastructure, invent controller details or silently use filesystem storage in hosted mode. |
| D13: lost-owner procedure | CLI mechanism requires an already active caregiver target, a frozen/unavailable owner identity, an opaque evidence reference, reason code, approved policy ID, expected security revision, and a short-lived confirmation digest covering the exact transfer. Operator authentication is OS/secret-file based on the authority host; HTTP principals cannot obtain operator scope. No automatic promotion. The mechanism and rejection tests are implementable; an actual ownership recovery is disabled until the operator supplies an approved verification policy and attests its evidence. This plan does not invent sufficient real-world proof. |

The lock file is never replaced or restored while authority processes are running. `flock` is supported only on a local Unix filesystem; NFS/shared-volume deployments fail validation. Lock ordering is process mutex → global file fence → sorted aggregate locks → PostgreSQL transaction. Candidate/head file locks are separate and always acquired inside the global fence. Reads release their shared fence only after database commit/rollback; commands release their exclusive fence only after projection or after marking global quarantine. If a command fails after head commit, **all** authenticated traffic remains quarantined until reconciliation; this is deliberately stricter than affected-aggregate quarantine at beta scale.

`security.Head.Generation` starts at 1 and binds every event/candidate path, external head, trusted recovery manifest and projection-head row. Revision remains globally monotonic across generations; a generation transition anchors the already committed revision and never resets authorization revisions to zero. P1 only replays a complete committed generation and provides no deletion operation. Plan 4 may retire an **entire old generation** only after atomic CAS to a new generation rooted in a verified signed checkpoint, with its publication/retirement protocol and backup-watermark proof; it cannot remove a prefix from a generation that P1 still expects to replay. P1 rejects a head/event/manifest generation mismatch even when revision and hash otherwise match. Wrong implementation costs include false readiness, revoked-access resurrection and erasure that leaves retired family IDs in old chain objects.

### Migration-manifest contract for every downstream schema task

`Server/internal/migrate/compatibility.json` is the sole embedded reader manifest. P1.2 creates `Server/internal/migrate/manifest.go`, `Server/cmd/baby-tracker-manifest/main.go` and `Server/scripts/check-migration-overlap.sh`; P2/P4 **modify and stage that manifest in the same task as each migration**, never substitute a `status.go` edit. `migrate.GenerateManifest(fs.FS,min,max int64) (Compatibility,error)` hashes the exact SQL bytes, validates a contiguous catalog and returns all known digest entries through max. The records-migration generator command is `rtk proxy sh -c 'cd Server && go run ./cmd/baby-tracker-manifest --min 9 --max 9 --output internal/migrate/compatibility.json'` for migration 000009; use the explicit owner/ranges below for subsequent tasks. It fails rather than overwriting existing digest entries with changed historical bytes.

| Schema | Owner | Prior overlap binary range | New binary range | Required same-task manifest mutation |
| --- | --- | --- | --- | --- |
| 000004 | P1.2 | `[3,4]` | `[4,4]` | Retain 000001–000003; append exact 000004 name/hash. |
| 000005 | P1.3 | `[4,5]` | `[5,5]` | Retain 000001–000004; append exact 000005 name/hash. |
| 000006 | P1.5 | `[5,6]` | `[6,6]` | Retain 000001–000005; append exact 000006 name/hash. |
| 000007 | P1.6 | `[6,7]` | `[7,7]` | Retain 000001–000006; append exact 000007 name/hash. |
| 000008 | P1.6 repair | `[7,8]` | `[8,8]` | Retain 000001–000007; append exact immutable `000008_lifecycle_worker_resume.sql` and the revoker-only unfinished-cleanup claim grant. |
| 000009 | P2.2 | `[8,9]` | `[9,9]` | Retain 000001–000008; append exact 000009 name/hash. |
| 000010 | P2.3 | `[9,10]` | `[10,10]` | Retain 000001–000009; append exact 000010 name/hash. |
| 000011 | P2.4 | `[10,11]` | `[11,11]` | Retain 000001–000010; append exact 000011 name/hash. |
| 000012 | P2.5 | `[11,12]` | `[12,12]` | Retain 000001–000011; append exact 000012 name/hash. |
| 000013 | P2.6 | `[12,13]` | `[13,13]` | Retain 000001–000012; append exact 000013 name/hash. |
| 000014 | P4.1 | `[13,14]` | `[14,14]` | Retain 000001–000013; append exact 000014 name/hash. |
| 000015 | P4.2 | `[14,15]` | `[15,15]` | Retain 000001–000014; append exact 000015 name/hash. |
| 000016 | P4.3 | `[15,16]` | `[16,16]` | Retain 000001–000015; append exact 000016 name/hash. |

Before each affected task begins, capture the accepted feature head as
`BT_MIGRATION_BASE_REF=$(rtk git rev-parse HEAD)`. The migration task's first
commit is compatibility-only: final immutable migration N, its exact complete
schema-N privilege snapshot, overlap scenarios, and `compatibility.json` with
`[N-1,N]`. It must compile, pass the N-1 suite, and remain deployable without
calling any N-only table/function. Capture that reviewed commit as
`BT_MIGRATION_PRIOR_REF=$(rtk git rev-parse HEAD)`. The second commit adds the
feature code and regenerates only the manifest bounds to `[N,N]`; capture it as
`BT_MIGRATION_FEATURE_REF`.

The exact P2.2 gate is
`rtk proxy bash Server/scripts/check-migration-overlap.sh --base-ref "$BT_MIGRATION_BASE_REF" --prior-ref "$BT_MIGRATION_PRIOR_REF" --feature-ref "$BT_MIGRATION_FEATURE_REF" --next-version 9 --prior-min 8 --prior-max 9 --next-min 9 --next-max 9`;
every later task passes its literal table row. Prepared mode archives the base,
prior, and feature commits into isolated build directories and **never mutates
an archive**. It builds predecessor and feature binaries from their commits,
runs the predecessor at N-1, migrates using the compatibility predecessor's
admin binary, runs predecessor at N, then runs the feature binary at N against
the same latest authority stores. Each phase exercises the registered guarded
read/write and structural-denial scenario and validates build IDs, readiness,
schema/privilege snapshots, and unknown/changed digest rejection.

Migration 000004 alone may retain the documented legacy synthesized harness
path because it predates this ruling; that mode must be explicitly labeled
historical, is not accepted as evidence for 000005 or later, and never becomes
the default. A source validator, an archive patched by the harness, or two
invocations of one binary is insufficient evidence. Operational rollout deploys
the reviewed compatibility predecessor before applying N and deploys the
feature commit only after the predecessor passes on N.

Access/refresh/invitation responses expose opaque secret values once. Initial sessions and invitations use CSPRNG values; Task 4 refresh successors use its domain-separated PRF construction solely to recreate identical pre-commit stage bytes after a crash. Lost post-commit token-bearing responses still require fresh sign-in or a fresh invitation; retry never recovers raw values from a hash or from the ledger. Invitation creation retry returns its stable metadata and `secret_unavailable` when the original token is no longer in the current response, without creating another invitation. Only family structural command receipts are generally replayable as ordinary JSON results.

### Exact file and package ownership

| Owner | Files and responsibility |
| --- | --- |
| P1.1 | `Server/internal/access/types.go`; `Server/internal/security/{types,commands,event,state,coordinator,projector}.go`; `Server/internal/security/localstore/{ledger,head,fence}.go`: pure authority, process-safe persistence, projection port. Migration `000003_security_projection.sql`. |
| P1.2 | `Server/internal/recovery/{state,gate,reconcile}.go`; `Server/internal/security/transaction.go`; `Server/internal/migrate/compatibility.go`; migration `000004_recovery_compatibility.sql`: persistent identity/epoch, quarantine, transaction fencing and reader compatibility. |
| P1.3 | `Server/internal/identity/{types,nonce,challenge,apple,jwks,repository}.go`; `Server/internal/limits/{policy,limiter,repository}.go`; `Server/internal/protocol/{auth,decode}.go`; migration `000005_identity_sessions.sql`: bounded Apple proof and identity/session persistence contracts. |
| P1.4 | `Server/internal/auth/{types,tokens,sessions,refresh,middleware}.go`; `Server/internal/identity/{apple_exchange,apple_notifications,credentials}.go`: opaque credentials, the provider-neutral credential-vault interface and hosted Apple protocol. |
| P1.5 | `Server/internal/family/{types,service,membership,invitations,entitlements,repository}.go`; `Server/internal/protocol/family.go`; migration `000006_family_commands.sql`: family slot, owner rules, invitation and entitlement policy. |
| P1.6 | `Server/internal/account/{lifecycle,apple_revocation}.go`; `Server/internal/family/lifecycle.go`; `Server/internal/security/invalidation.go`; `Server/internal/operator/{commands,audit,confirmation}.go`; migration `000007_lifecycle.sql`: deletion scheduling/recovery, durable provider revocation/credential cleanup and operator commands. |
| P1.7 | `Server/internal/security/hosted/{contracts,ledger,head}.go`; `Server/internal/security/storetest/conformance.go`; `Server/internal/identity/hostedcredentials/{contracts,vault}.go`; `Server/tests/identity/`: hosted adapters, conformance and whole-slice acceptance. |

Migration numbers above are reserved for this seven-task sequence, superseding the architecture map's illustrative order. Before each migration is created, run `rtk rg --files Server/migrations`; if another implementation has consumed a number, reconcile the sequence and every manifest/test reference before editing. Never overwrite existing migration bytes.

### Shared type contracts

All public UUIDs are canonical, nonzero lowercase strings. All wire revision/epoch values are integers in `[0, 9007199254740991]`, the exact JSON-integer range also used by Plan 2; epoch starts at 1. Go uses `uint64`, and conversion to PostgreSQL `bigint` checks this wire upper bound. Error results use the existing `protocol.ErrorResponse`; no secrets occur in errors. Code snippets below omit only standard import declarations.

```go
// internal/access/types.go — P1.1; no import of auth/family/security.
type Principal struct {
    AccountID, DeviceID, SessionID string
    RecoveryEpoch, SecurityRevision uint64
    Scope string // account, family, family_recovery, operator
}
type Grant struct { AccountID, FamilyID, Role string; Revision uint64 }

// internal/security/types.go — P1.1.
type Head struct { Generation,Revision uint64; EventHash [32]byte }
type Candidate struct {
    CommandID string
    Prior Head
    EventJSON []byte
    EventHash [32]byte
}
type SecurityLedger interface {
    AppendCandidate(context.Context, Candidate) error
    ReadCommitted(context.Context, Head) ([]Candidate, error)
    WalkCommitted(context.Context, Head, func(Candidate) error) error
}
type LedgerHeadStore interface {
    Load(context.Context) (Head, error)
    CompareAndSwap(context.Context, Head, Head) (bool, error)
}
type Command struct {
    ID, Kind string
    AffectedAccountIDs, AffectedFamilyIDs []string
    Payload json.RawMessage
}
type CommandResult struct {
    CommandID, Outcome, Code string
    Revision uint64
}
type Projection interface {
    Head(context.Context) (Head, error)
    Apply(context.Context, Candidate) error
}
type Fence interface {
    Read(context.Context) (func(), error)
    Write(context.Context) (func(), error)
}
type RecoveryGate interface { Check(context.Context) error }
type Decision func(context.Context, access.Principal, string, string) (access.Grant, error)
type Service struct {
    ledger SecurityLedger
    heads LedgerHeadStore
    projection Projection
    fence Fence
    now func() time.Time
    runtime *pgxpool.Pool
    decide Decision
    epoch func(context.Context) (uint64,error)
    quarantined atomic.Bool
}
func New(Options) (*Service, error)
func (s *Service) Execute(context.Context, access.Principal, Command) (CommandResult, error)
func (s *Service) Reconcile(context.Context) error
func (s *Service) Check(context.Context) error
func (s *Service) WithAuthorizedTransaction(context.Context, access.Principal,
    string, string, func(pgx.Tx, access.Grant) error) error // P1.2
```

`security.Options` has exact fields `Ledger SecurityLedger`, `Heads LedgerHeadStore`, `Projection Projection`, `Fence Fence`, `Now func() time.Time`, `Runtime *pgxpool.Pool`, `Decide Decision`, `Epoch func(context.Context) (uint64,error)`. `Runtime/Decide/Epoch` are required by the transaction guard after P1.2; `Execute` cannot be exposed through HTTP until its concrete command validator is registered. Constructor rejects missing required collaborators; tests inject a real deterministic clock and explicit decision. `security.Code(err) string` extracts a safe stable code; other errors map to `service_unavailable` without interpolating SQL/provider text. Add `Service.ReadState(context.Context) (State,error)` for family policy: it acquires/verifies a shared fence normally, but reuses an unexported context fence marker when called by `WithAuthorizedTransaction`. The marker is minted only inside `security`; HTTP headers/DTOs cannot create it. The guard uses `checkLocked(context.Context) error` and injects this marker before invoking `Decision`, so neither gate nor family policy recursively locks the fence.

`ReadCommitted` is a small-test/reference API. `WalkCommitted` is part of the authoritative `SecurityLedger` port, is implemented by every production adapter and provider double, and is the only ledger read used by `Service.Reconcile`, startup replay, restore reconciliation, and Plan 4 generation replay; no unbounded ledger slice appears in production. Store-conformance tests must exercise both methods against the same committed head. Maximum event size is 64 KiB; revisions are traversed one at a time. Signed checkpoints/compaction are Plan 4's responsibility and preserve this commit boundary.

### Verification environment

Commands execute from the worktree root unless they explicitly start with `cd Server` through `rtk proxy`. The whole Go suite command is `rtk proxy sh -c 'cd Server && go test -race -count=1 ./...'`. `BABY_TRACKER_TEST_DATABASE_URL` must point to a disposable PostgreSQL 17 administrator connection capable of creating temporary databases/roles; never reuse production. New `internal/testpg/database.go` provides `New(t *testing.T) Fixture`, where `Fixture{Owner, Runtime, Projector *pgxpool.Pool}` creates unique random-suffixed `identity_test_` databases/roles, applies embedded migrations as owner, grants runtime/projector separately and registers cleanup of those exact resources. It uses the existing `internal/tenant/context_test.go:tenantDatabase` pattern. Missing DSN is a fatal test error when `BABY_TRACKER_REQUIRE_DATABASE=1`, otherwise a named skip. No DSN is printed.

P1.1 adds `Server/scripts/test-identity.sh` with `set -euo pipefail`; it requires the DSN, exports `BABY_TRACKER_REQUIRE_DATABASE=1`, then runs `go test -race -count=1 "$@"` from `Server`. Its default arguments are `./...`; explicit arguments select focused packages. The whole-plan gate uses this script so skipped database tests cannot be mistaken for evidence. Foundation hardcoded migration/table/grant expectations are updated when the owning migration changes them; do not weaken their role/isolation assertions.

### Task 1: Commit and replay opaque security authority (P1.1)

**Files:**

- Create: `Server/internal/access/types.go`, `Server/internal/security/types.go`, `commands.go`, `event.go`, `state.go`, `coordinator.go`, `projector.go`, `coordinator_test.go`, `event_test.go` in the same security directory.
- Create: `Server/internal/security/localstore/ledger.go`, `head.go`, `fence.go`, `ledger_test.go`, `fence_test.go`.
- Create: `Server/internal/testpg/database.go`, `Server/scripts/test-identity.sh`, `Server/migrations/000003_security_projection.sql`, `Server/api/fixtures/security-event-v1.json`.
- Modify: `Server/deploy/postgres/init-roles.sql` (projector group only), `Server/internal/tenant/roles_test.go` (group inventory), `Server/internal/tenant/context_test.go` (new migration count).
- Test: the named security/localstore tests and real PostgreSQL projection replay in `Server/internal/security/projector_test.go`.

**Interfaces:**

- Consumes: existing `tenant.SetLocal(context.Context, pgx.Tx, tenant.Context) error`, `migrate.New(fs.FS).Apply(context.Context, *pgxpool.Pool) error`.
- Produces: all P1.1 shared contracts above; `localstore.Open(ledgerDir, headDir string) (Store,error)`, with `Store{Ledger security.SecurityLedger, Heads security.LedgerHeadStore, Fence security.Fence}`; `Store.InitializeEmpty(context.Context) error` exclusively creates the zero head after checking both stores are empty; `security.NewProjector(*pgxpool.Pool) security.Projection`. `Open` never initializes missing security state. Only explicit CLI initialization and new test fixtures invoke `InitializeEmpty`.
- Test seam: `newAuthority(t *testing.T) *authorityFixture` constructs temp ledger/head directories, deterministic clock, in-memory projection and service. `authorityFixture` fields are `Service *security.Service`, `Heads security.LedgerHeadStore`, `Projection *faultProjection`; methods `Register(accountID, commandID string) security.Command`, `Head() security.Head`, `FailProjectionOnce()`, `Reopen()`, `CandidateCount() int`. `faultProjection` implements `Projection`; it stores only committed revision/hash and fails one selected `Apply` before mutation. This helper lives only in `coordinator_test.go`.

- [ ] **Step 1 (3 min): Write the head-before-projection behavioral test.**

```go
func TestHeadCommitSurvivesProjectionCrash(t *testing.T) {
    h := newAuthority(t)
    p := access.Principal{AccountID: "10000000-0000-4000-8000-000000000001", Scope:"account"}
    cmd := h.Register(p.AccountID, "30000000-0000-4000-8000-000000000001")
    h.FailProjectionOnce()
    if _, err := h.Service.Execute(t.Context(), p, cmd); security.Code(err) != "service_unavailable" { t.Fatalf("code=%s", security.Code(err)) }
    if h.Head().Revision != 1 { t.Fatal("head was not the commit") }
    h.Reopen()
    if err := h.Service.Reconcile(t.Context()); err != nil { t.Fatal(err) }
    got, err := h.Service.Execute(t.Context(), p, cmd)
    if err != nil || got.Revision != 1 || h.Head().Revision != 1 { t.Fatalf("replay=%+v err=%v", got, err) }
}
```

- [ ] **Step 2 (2 min): Run RED.**

Run: `rtk proxy sh -c 'cd Server && go test ./internal/security -run TestHeadCommitSurvivesProjectionCrash -count=1'`.

Expected: FAIL because `security.New`, command types and coordinator do not exist. If it passes before implementation, the test is not exercising the new authority.

- [ ] **Step 3 (4 min): Add shared types and closed event vocabulary.**

Create the exact shared contracts. In `event.go` define fixed-order structs, never maps:

```go
type Event struct {
    Version string `json:"version"`
    Generation uint64 `json:"generation"`
    Revision uint64 `json:"revision"`
    PreviousHash string `json:"previous_hash"`
    CommandID string `json:"command_id"`
    CommandHash string `json:"command_hash"`
    ActorAccountID string `json:"actor_account_id"`
    ActorDeviceID string `json:"actor_device_id"`
    Kind string `json:"kind"`
    At string `json:"at"`
    Effects []Effect `json:"effects"`
}
type Effect struct {
    Kind, AccountID, FamilyID, DeviceID, SessionID, ParentSessionID, ServerInstanceID, CanonicalOrigin string
    InvitationID, IdentityKey, TokenDigest, DeviceCredentialDigest, Role, Status, EntitlementKey, Source string
    ExpiresAt, PurgeAfter string
    RecoveryEpoch uint64
}
func CanonicalCommandHash(access.Principal, Command) ([32]byte,error)
func EncodeEvent(Event) ([]byte,error)
func Replay([]Candidate) (State,error)
```

Give every `Effect` field an explicit snake_case JSON tag and `omitempty` for optional string fields. `State` contains maps keyed by opaque IDs for accounts, family slots, families, memberships, invitations, session lineages, entitlements and committed command receipts; maps are internal only. Define command payload structs with `Version string` equal to `v1`, then named explicit scalar fields for each row in the following table. Hash the validated typed payload, sorted affected IDs and authenticated actor, excluding runtime-generated time. Event time is UTC RFC3339Nano; event hash is SHA-256 of exact encoded event bytes; hash is stored in `Candidate`, not recursively inside the preimage.

| Command/event kind | Opaque fields and state effect |
| --- | --- |
| `account_registered` | account ID, identity lookup digest; active identity/account grant. No subject/name. |
| `identity_unavailable` | account ID, identity digest; disable Apple authority and freeze owner-only operations. Later SQL implicitly revokes every account session while preserving device bindings. Restoration is not a standalone kind: `identity_session_issued` atomically re-enables the same unavailable actor while issuing a new session. |
| `device_registered`, `device_binding_revoked` | server-issued device/account/instance IDs and keyed continuity credential digest; bind an installation independently of any expiring application session, or explicitly revoke that binding. Revocation is exactly one effect; later SQL implicitly revokes the binding and all matching device sessions. No raw continuity secret. |
| `session_granted`, `session_rotated`, `session_revoked` | account/device/session/parent IDs, epoch, absolute expiry/status and optional projection-material digest; no bearer token. Rotation requires the 64-lowercase-hex digest; revocation is exactly one effect naming a lineage member and later SQL implicitly revokes the complete account/device/lineage-root set. |
| `family_created` | family/owner IDs, owner membership and family slot. |
| `invitation_created`, `invitation_revoked`, `invitation_redeemed` | invitation/family/creator/redeemer IDs, HMAC token digest, expiry/status; redemption and joining membership in one event. |
| `member_left`, `member_removed` | account/family IDs; remove slot/membership, mark family attribution former, revoke linked session authority. |
| `owner_transferred`, `owner_recovered` | family/old/new owner IDs; both roles change together; recovery also includes opaque operator evidence digest. |
| `entitlement_granted`, `entitlement_suspended` | family/key/source/status/expiry. |
| `family_deletion_scheduled`, `family_recovered`, `recovery_forfeited` | family/claim-owner IDs, previous member IDs/roles as separate sorted effects, purge deadline and recovery-slot state. Recovery reactivates only the owner; former caregivers rejoin through new invitations. |
| `account_deleted` | bounded account/identity/family/claim anchors; remove identity/profile authority, forfeit recovery and scrub profile projections. Session/device revocation is deterministic set-wise projection, never an enumerated effect list. |
| `family_purge_started` | family ID, due purge deadline and start command/time; authoritative irreversible terminal decision after `purge_after`, denying recovery from this head onward. Only P4's narrowly scoped worker emits it. |
| `family_purge_completed` | family ID, physical SQL purge receipt and completion time; idempotent completion after `family_purge_started`. Retain minimal terminal state until P4's verified backup/checkpoint erasure. Only P4 emits it. |

Unknown command/effect versions or kinds fail `unsupported_security_event`; no permissive generic field updates. Public callers construct commands through typed constructors in owning tasks, never accept affected-ID arrays from HTTP. P1's parser/replay recognizes both reserved purge events now: `family_purge_started` requires due deletion state and records terminal `purge_started`; `family_purge_completed` requires that terminal state and records completion without restoring any grant. Projection updates opaque security state only and tolerates already-absent canonical rows; P4 owns physical SQL deletion and emitting these events. P1 cannot dispatch either through HTTP/ordinary operator scope. `TestReservedPurgeEventsNeverRestoreRecovery` replays started/completed twice and asserts terminal denial, so P4 never edits historical event parsing to add a formerly unknown kind.

- [ ] **Step 4 (4 min): Implement replay validation and idempotent command decisions.**

```go
func validateCandidate(prior Head, c Candidate) error {
    if c.Prior != prior || sha256.Sum256(c.EventJSON) != c.EventHash { return ErrLedgerCorrupt }
    var e Event
    if err := decodeEventStrict(c.EventJSON, &e); err != nil { return ErrLedgerCorrupt }
    if e.Generation != prior.Generation || e.Revision != prior.Revision+1 || e.CommandID != c.CommandID { return ErrLedgerCorrupt }
    return validateEvent(e)
}
```

Define `ErrLedgerCorrupt`, `decodeEventStrict([]byte,*Event) error` and `validateEvent(Event) error` in `event.go`. The decoder rejects duplicate JSON keys, unknown fields, trailing objects, oversized values and noncanonical UUID/time/hash encodings. `State.Decide(Principal,Command,time.Time) (Event,CommandResult,error)` validates actor/command-derived aggregates against **replayed** state; it returns a stored stable result before append for same-hash receipt replay. `Register` test constructor seeds only the first account with an internal verified-identity grant; arbitrary HTTP users cannot invoke bootstrap registration.

- [ ] **Step 5 (4 min): Implement append/head durability under local process exclusion.**

```go
func (s *Ledger) AppendCandidate(context.Context,security.Candidate) error
func (s *Ledger) WalkCommitted(context.Context,security.Head,func(security.Candidate)error) error
func (s *Heads) CompareAndSwap(context.Context,security.Head,security.Head) (bool,error)
func (f *FileFence) Read(context.Context) (func(),error)
func (f *FileFence) Write(context.Context) (func(),error)
```

`Ledger`, `Heads` and `FileFence` are the concrete localstore implementations returned by `Open`; their private state is the validated absolute directory/file path plus process mutex. `Heads.Load` and `Ledger.ReadCommitted` implement the remaining shared methods.

Create ledger files as `generation-<20-digit-generation>/<20-digit-revision>-<64-hex-hash>.json` with `O_CREATE|O_EXCL`, mode 0600; compare existing bytes on idempotent append. Write, `Sync`, close, then sync the generation and ledger parent directories. `CompareAndSwap` takes `flock(LOCK_EX)` on a stable `head.lock`, compares the whole expected head including generation, writes a new 0600 temporary head named `head-g<generation>-r<revision>-<hash>.tmp`, syncs it, renames over the stable pointer `head.json`, then syncs the head directory. `head.json` and trusted manifest bytes always contain the explicit generation; no pathname default may supply a missing generation. Empty initialization is `Head{Generation:1,Revision:0,EventHash:[32]byte{}}`. A suffix candidate is ignored unless reached by the exact committed head generation/hash. `fence.go` combines an in-process RWMutex with cancellable `LOCK_NB` retries bounded by context cancellation; a killed process releases its file lock. Do not delete candidate suffixes during ordinary recovery.

- [ ] **Step 6 (4 min): Add corruption and cross-process tests.**

```go
func TestRejectedCommandNeverAppends(t *testing.T) {
    h := newAuthority(t)
    _, err := h.Service.Execute(t.Context(), access.Principal{Scope:"account"}, security.Command{ID:"bad", Kind:"owner_transferred"})
    if security.Code(err) != "invalid_command" || h.CandidateCount() != 0 || h.Head().Revision != 0 { t.Fatal("invalid command reached persistence") }
}
```

Add table-driven `TestLedgerFaultMatrix` cases `changed_bytes`, `missing_middle`, `valid_prefix_truncation`, `head_hash_mismatch`, `candidate_before_head`, `head_before_projection`; first four require `ledger_corrupt`, candidate suffix leaves prior state, final case replays once. `TestCommandIDReuse` repeats same semantic payload and then changes it: one revision, then `command_id_reused`. `TestMissingTrustedHeadCannotRecover` presents an old matching pair without a separately supplied trusted manifest and expects `trusted_head_required`. `TestIndependentProcessHeadCAS` uses `os/exec` of the test binary with exact temporary paths and two barriered child processes; exactly one CAS succeeds, loser re-reads/revalidates. This test must exercise separate processes, not two goroutines.

- [ ] **Step 7 (4 min): Add replay projection schema and least-privilege role.**

```sql
CREATE TABLE security_projection_head (
  singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
  generation bigint NOT NULL CHECK(generation > 0),
  revision bigint NOT NULL CHECK(revision >= 0), event_hash bytea NOT NULL CHECK(octet_length(event_hash)=32)
);
INSERT INTO security_projection_head VALUES(true,1,0,decode(repeat('00',32),'hex'));
CREATE TABLE security_command_receipts (
  actor_account_id uuid NOT NULL, command_id uuid NOT NULL,
  command_hash bytea NOT NULL CHECK(octet_length(command_hash)=32),
  generation bigint NOT NULL CHECK(generation>0), revision bigint NOT NULL, result jsonb NOT NULL,
  UNIQUE(generation,revision),
  PRIMARY KEY(actor_account_id,command_id)
);
```

Also create `account_security_state`, `family_security_state`, `security_audit_events` with nonzero IDs, enum states, generation/revision/hash uniqueness and opaque event data only. Reserve `purge_started` and `purge_completed` opaque lifecycle states and safe projector cases in 000003 now; P4 emits them after its worker checks exist. Add `baby_tracker_projector` NOLOGIN/NOSUPERUSER/NOBYPASSRLS group with no memberships in owner/runtime/maintenance roles. Projection login can execute narrowly typed `app.project_security_event` only; PUBLIC/runtime cannot. The function's fixed `search_path=pg_catalog` and qualified table names apply a validated event, audit row and receipt and advance head in one transaction; it enforces previous generation/revision/hash and idempotent same-head replay. Direct projector table DML is denied. Later tasks add only the new effect cases they own to this function through additive migrations; reserved purge parsing/state semantics are not rewritten.

- [ ] **Step 8 (4 min): Implement coordinator ordering and reconciliation.**

```go
func (s *Service) Execute(ctx context.Context,p access.Principal,c Command) (CommandResult,error)
func (s *Service) Reconcile(ctx context.Context) error
func (s *Service) Check(ctx context.Context) error
```

`Execute` holds the exclusive fence, loads/verifies head+chain, checks receipt and `State.Decide`, appends candidate, CASes head, applies all unprojected committed events, checks projection head equality, then returns the stable result. CAS loss reconstructs and revalidates; at most 8 retries then retryable `security_busy`. Before any CAS attempt verify the held fence and current operator-managed epoch. Projection failure after commit sets quarantine before release and returns `service_unavailable`; receipt replay first reconciles. `Reconcile` uses the same exclusive fence and idempotent `Projection.Apply`. Never “undo” a committed external event because SQL failed.

- [ ] **Step 9 (3 min): Add required database harness and focused runner.**

```go
func requireDatabase(t *testing.T) string {
    t.Helper()
    dsn := os.Getenv("BABY_TRACKER_TEST_DATABASE_URL")
    if dsn == "" && os.Getenv("BABY_TRACKER_REQUIRE_DATABASE") == "1" { t.Fatal("disposable PostgreSQL is required") }
    if dsn == "" { t.Skip("BABY_TRACKER_TEST_DATABASE_URL is not set") }
    return dsn
}
```

Implement `testpg.New` with exact-resource cleanup and a projector pool distinct from runtime. Add `TestProjectionIdempotence` applying the same revision/hash twice and a mismatched hash once; expect one audit row and mismatch denial. Add `TestRuntimeCannotProject` using `Fixture.Runtime`, expecting SQLSTATE `42501`. Runner body is `test -n "${BABY_TRACKER_TEST_DATABASE_URL:-}"; export BABY_TRACKER_REQUIRE_DATABASE=1; cd "$(dirname "$0")/.."; if [[ $# -eq 0 ]]; then set -- ./...; fi; go test -race -count=1 "$@"` after the shebang and strict shell mode. No secret-printing shell tracing.

- [ ] **Step 10 (3 min): Run GREEN and inspect the focused diff.**

Run: `rtk proxy bash Server/scripts/test-identity.sh ./internal/security/... ./internal/tenant/...`.

Expected: PASS including separate-process CAS, actual PostgreSQL projection/permission tests, and all six fault cases. Run `rtk git diff --check`; expected no whitespace errors.

- [ ] **Step 11 (2 min): Commit this authority boundary.**

```bash
rtk git add Server/internal/access Server/internal/security Server/internal/testpg Server/scripts/test-identity.sh Server/migrations/000003_security_projection.sql Server/api/fixtures/security-event-v1.json Server/deploy/postgres/init-roles.sql Server/internal/tenant/roles_test.go Server/internal/tenant/context_test.go
rtk git commit -m "feat(server): add durable security authority and replay"
```

### Task 2: Persist identity, quarantine restore and fence authorized transactions (P1.2)

**Files:**

- Create: `Server/internal/recovery/state.go`, `gate.go`, `reconcile.go`, `state_test.go`, `gate_test.go`; `Server/internal/security/transaction.go`, `transaction_test.go`; `Server/internal/migrate/compatibility.go`, `compatibility_test.go`, `compatibility.json`, `manifest.go`, `manifest_test.go`; `Server/cmd/baby-tracker-manifest/main.go`, `main_test.go`; `Server/scripts/check-migration-overlap.sh`, `migration_overlap_test.go`; `Server/migrations/000004_recovery_compatibility.sql`.
- Modify: `Server/internal/migrate/status.go`, `migrate_test.go`; `Server/internal/config/config.go`, `config_test.go`; `Server/cmd/baby-tracker-server/main.go`, `main_test.go`; `Server/cmd/baby-tracker-admin/main.go`, `main_test.go`; `Server/internal/httpapi/operational.go`, `router.go`, `router_test.go`; `Server/internal/tenant/context_test.go`.
- Test: `Server/internal/security/transaction_test.go`, `Server/internal/recovery/gate_test.go` and reader-compatibility tests.

**Interfaces:**

- Consumes: P1.1 `security.Service`, `Fence`, `Head`, `Projection`, `access.Principal/Grant`, `tenant.SetLocal`.
- Produces: `recovery.State{InstanceID string; Epoch uint64; TrustedHead security.Head}`, `recovery.Store.Load(context.Context) (State,error)`, `Initialize(context.Context,State) error`, `Rotate(context.Context,uint64,security.Head) (State,error)`; `recovery.Gate.Check(context.Context) error`; `security.Service.WithAuthorizedTransaction` exactly as above.
- Produces: `migrate.MigrationDigest{Version int64; Name,SHA256 string}`, `migrate.Compatibility{Min,Max int64; Known []MigrationDigest}`, `migrate.GenerateManifest(fs.FS,int64,int64) (Compatibility,error)`, `(*migrate.Runner).CheckCompatibility(context.Context,*pgxpool.Pool,Compatibility) error`; `check-migration-overlap.sh` and literal flags from the downstream contract. Existing strict `Status/Apply` remain available to admin diagnostics.
- Test helper `newRecoveryFixture(t *testing.T) *recoveryFixture` returns `Service *security.Service`, `Gate *recovery.Gate`, `Store *recovery.Store`, `Runtime *pgxpool.Pool`, `Principal access.Principal`, `FamilyID string`; methods `CommitRemoval()`, `PauseBeforeTransaction() (resume func())`, `RestoreDatabaseToRevision(uint64)`, `SetTrustedManifest(security.Head)`. It seeds authority through P1.1 test commands, never trusts direct runtime DML for authorization.

- [ ] **Step 1 (3 min): Pin the stale-grant race before implementation.**

```go
func TestGuardRevocationWinsBeforeTransaction(t *testing.T) {
    h := newRecoveryFixture(t)
    stale := h.Principal
    h.CommitRemoval()
    called := false
    err := h.Service.WithAuthorizedTransaction(t.Context(), stale, h.FamilyID, "read", func(tx pgx.Tx, g access.Grant) error { called=true; return nil })
    if security.Code(err)!="membership_revoked" || called { t.Fatal("stale authorization entered tenant SQL") }
}
```

- [ ] **Step 2 (2 min): Run RED.**

Run: `rtk proxy sh -c 'cd Server && go test ./internal/security -run TestGuardRevocationWinsBeforeTransaction -count=1'`.

Expected: FAIL with undefined `WithAuthorizedTransaction`/recovery fixture. Add a companion barrier test where the transaction acquires its shared fence first: removal must wait until transaction completion, then the next read is denied.

- [ ] **Step 2a (4 min): Prepare the bounded reader before adding recovery-schema dependencies.**

```go
type Compatibility struct { Min,Max int64; Known []MigrationDigest }
type MigrationDigest struct { Version int64; Name,SHA256 string }
func ValidateCompatibility(applied []MigrationDigest,c Compatibility) error
```

Create the embedded manifest initially at `[3,3]` with exact SQL digests through 000003. Replace only the existing readiness `Current==Required` check with `CheckCompatibility`; preserve its other behavior in this preparatory build. Reject missing/duplicate versions, changed names/hashes and current outside min/max or absent from Known. Keep strict migration-CLI `validateApplied`. `TestKnownAdditiveMigrationOverlap` uses fixture schema 3 plus known exact additive 4 and fails until this reader accepts `[3,4]` while rejecting any changed/unknown digest. RED/GREEN command: `rtk proxy sh -c 'cd Server && go test ./internal/migrate -run TestKnownAdditiveMigrationOverlap -count=1'`; expected RED is rejected known additive schema, GREEN is both known versions accepted.

- [ ] **Step 2b (4 min): Add the failure-first manifest generator contract.**

```go
func TestManifestRejectsChangedHistoricalMigration(t *testing.T) {
 files:=fstest.MapFS{"000001_a.sql":{Data:[]byte("SELECT 1;\n")},"000002_b.sql":{Data:[]byte("SELECT 2;\n")}}
 old,err:=GenerateManifest(files,1,1); if err!=nil { t.Fatal(err) }
 files["000001_a.sql"]=&fstest.MapFile{Data:[]byte("SELECT 9;\n")}
 next,err:=GenerateManifest(files,2,2); if err!=nil { t.Fatal(err) }
 if err:=VerifyManifestExtension(old,next); err==nil { t.Fatal("historical SQL digest changed") }
}
```

Define `VerifyManifestExtension(previous,next Compatibility) error` to compare every prior version/name/hash and require exactly one contiguous next catalog entry. RED: `rtk proxy sh -c 'cd Server && go test ./internal/migrate -run TestManifestRejectsChangedHistoricalMigration -count=1'`; expect undefined generator/extension validator. Implement `GenerateManifest` with `fs.ReadDir`, existing filename parsing, `sha256.Sum256` on raw bytes and sorted entries; CLI decodes old manifest, verifies extension, then atomically writes canonical indented JSON. Same-version regeneration requires exact same digests. GREEN repeats this command and expects PASS.

- [ ] **Step 2c (4 min): Build the compatibility-only predecessor and two-binary harness.**

Add `TestMigrationOverlapRunsDistinctBinaries` in `migration_overlap_test.go`: fake executable fixtures write distinct build IDs to an invocation sink; require both prior/new calls and reject reused paths, wrong min/max, altered historical checksum or missing prior reference. RED: `rtk proxy sh -c 'cd Server && go test ./scripts -run TestMigrationOverlapRunsDistinctBinaries -count=1'`, expected missing harness. Implement the archive/build/start/HTTP gate from the global contract and GREEN the same test. At this step main.go contains only the new bounded schema-reader call-site, with no new recovery/runtime dependency. Commit only the preparatory reader and capture its SHA:

```bash
rtk git add Server/internal/migrate/compatibility.go Server/internal/migrate/compatibility_test.go Server/internal/migrate/compatibility.json Server/internal/migrate/manifest.go Server/internal/migrate/manifest_test.go Server/cmd/baby-tracker-manifest Server/scripts/check-migration-overlap.sh Server/scripts/migration_overlap_test.go Server/cmd/baby-tracker-server/main.go
rtk git commit -m "feat(server): prepare checksum-bounded migration overlap"
rtk git rev-parse HEAD
```

Set `BT_MIGRATION_PRIOR_REF` to that printed commit; subsequent steps may now add recovery dependencies. The actual overlap gate runs after Step 6 has finalized migration 000004, using the predecessor source plus its preapproved next manifest. P1.2's currently failing guard test stays unstaged until implemented.

- [ ] **Step 3 (4 min): Persist identity/epoch with explicit initialization.**

```go
type State struct { InstanceID string; Epoch uint64; TrustedHead security.Head }
type Store struct { directory string; fence security.Fence }
func NewStore(directory string,fence security.Fence) (*Store,error)
func (s *Store) Initialize(context.Context,State) error
func (s *Store) Load(context.Context) (State,error)
func (s *Store) Rotate(context.Context,uint64,security.Head) (State,error)
```

Store `identity.json` in the independent head volume, immutable instance ID plus monotonic epoch and accepted trusted recovery manifest; use P1.1 fsync/rename discipline under the exclusive fence. `Initialize` requires empty head/ledger and an explicit CLI operation, refuses zero UUID/epoch or existing different state. `Rotate(expectedEpoch,head)` compares both expected values, increments once with overflow rejection, persists before returning, and enters quarantine. Environment `BT_SERVER_INSTANCE_ID` and `BT_RECOVERY_EPOCH` become asserted expectations; they cannot silently overwrite durable state or initialize production on boot.

- [ ] **Step 4 (4 min): Add startup reconciliation and failure distinctions.**

```go
func (g *Gate) Check(ctx context.Context) error {
    if err := g.verifyPersistentIdentity(ctx); err != nil { return err }
    if err := g.security.Check(ctx); err != nil { return err }
    return g.schema.Check(ctx)
}
```

Define private `verifyPersistentIdentity(context.Context) error` and injected `schema interface{ Check(context.Context) error }`. Verify head equals trusted manifest or is its verified descendant; never accept an epoch below the persisted epoch. Boot enters quarantine; reconcile projection and mirror `server_state` with external identity/epoch, then clear it. A missing committed event, altered head, unexplained DB revision ahead of authority, mismatch or untrusted total-store restore keeps quarantine. `/healthz` remains dependency independent; `/readyz` returns existing safe `not_ready`; authenticated routes return retryable `authorization_quarantine` before service work. `/v1/server` reads current durable state per request, not captured startup epoch.

- [ ] **Step 5 (4 min): Implement the transaction guard.**

```go
func (s *Service) WithAuthorizedTransaction(ctx context.Context, p access.Principal, familyID, action string, run func(pgx.Tx, access.Grant) error) error {
    release, err := s.fence.Read(ctx)
    if err != nil { return err }
    defer release()
    if err := s.checkLocked(ctx); err != nil { return err }
    ctx = s.withFenceMarker(ctx)
    grant, err := s.decide(ctx,p,familyID,action)
    if err != nil { return err }
    tx, err := s.runtime.BeginTx(ctx,pgx.TxOptions{})
    if err != nil { return ErrUnavailable }
    defer tx.Rollback(ctx)
    if err := tenant.SetLocal(ctx,tx,tenant.Context{AccountID:grant.AccountID,FamilyID:grant.FamilyID}); err != nil { return err }
    if err := run(tx,grant); err != nil { return err }
    return tx.Commit(ctx)
}
```

Define private `checkLocked(context.Context) error` and `withFenceMarker(context.Context) context.Context` as specified in the shared contracts, preventing recursive flock/RWMutex acquisition. Define `ErrUnavailable`; callback sees no maintenance/projector pool. Decision compares live session/epoch/revision and freshly reconstructed state; `Principal.SecurityRevision` is evidence, not authority. Account-only queries use a separate `WithAccountTransaction(context.Context,Principal,func(pgx.Tx)error) error` with account-local setting and no invented family ID. Add connection-pool tests proving both context settings clear after commit, rollback and callback error. P1.2 fixtures inject a deterministic decision over P1.1 authority state; the actual family policy and HTTP tokens are exercised again after P1.4/P1.5.

- [ ] **Step 6 (4 min): Implement exact bounded schema compatibility.**

```go
type Compatibility struct { Min,Max int64; Known []MigrationDigest }
type MigrationDigest struct { Version int64; Name,SHA256 string }
func ValidateCompatibility(applied []MigrationDigest, c Compatibility) error
```

Finalize migration 000004 with a projection-owned `server_state` mirror update function and `schema_compatibility` metadata for operator diagnostics, never a runtime-trusted override of the binary manifest. Generate current manifest `[4,4]` from exact bytes, preserving 000001–000003; the overlap harness derives `[3,4]` for the prior build using the same known digests. Run `rtk proxy bash Server/scripts/check-migration-overlap.sh --prior-ref "$BT_MIGRATION_PRIOR_REF" --next-version 4 --prior-min 3 --prior-max 4 --next-min 4 --next-max 4`; require two recorded build IDs and readiness 200 after new schema application. Before auth endpoints exist, the authenticated-read part of the harness is explicitly inapplicable and `/v1/server` identity agreement supplies the existing feature check; from P1.4 onward it must use real authenticated account/family reads. Do not assert the unextended foundation binary supports new schemas.

- [ ] **Step 7 (3 min): Write restore and overlap tests.**

```go
func TestRecoveryManifestMismatchKeepsQuarantine(t *testing.T) {
    h := newRecoveryFixture(t)
    h.CommitRemoval()
    h.RestoreDatabaseToRevision(0)
    h.SetTrustedManifest(security.Head{Generation:1,Revision:99,EventHash:sha256.Sum256([]byte("different head"))})
    if err := h.Gate.Check(t.Context()); err == nil { t.Fatal("untrusted restore became ready") }
}
```

`TestOlderDatabaseStaysQuarantinedUntilReplay` preserves latest external head, restores DB to revision 0, asserts guard callback never runs, reconciles and confirms removal still denies. `TestKnownAdditiveMigrationOverlap` uses a version-3 reader with the exact version-4 manifest and asserts ready at 3 and 4; a changed version-4 digest, unknown 5 or missing 2 rejects. `TestHealthSurvivesAuthorityOutage` asserts health 200/readiness 503 through the real router. `TestEpochRotationIsMonotonic` races two expected-epoch rotations; one succeeds, one `epoch_conflict`.

- [ ] **Step 8 (4 min): Add admin verbs and wire runtime gate.**

```go
// cmd/baby-tracker-admin; move process exit handling outside this testable function.
func run(ctx context.Context,args []string,getenv func(string)string,out io.Writer) error
// internal/httpapi.Options gains the injected gate, never a projection pool.
type AccountGate interface { Check(context.Context) error }
```

Use `flag.NewFlagSet` dispatch for exact commands `security init`, `security verify`, `security reconcile`, `recovery rotate-epoch`; keep `migrate` unchanged. `security init` requires `--instance-id`, `--epoch 1`, `--confirm-instance`; rotation requires `--expected-epoch`, `--expected-revision`, `--reason-code database_restore`, `--confirm-instance`. Print only IDs, epoch, revision and result code. No CLI operation advertises ready before replay. Route construction receives the runtime guard service and cannot receive projector/maintenance pools. `main` starts bounded reconciliation retry without migrating; shutdown cancels retries.

- [ ] **Step 9 (3 min): Run GREEN and full current server suite.**

Run: `rtk proxy bash Server/scripts/test-identity.sh ./internal/security/... ./internal/recovery/... ./internal/migrate/... ./internal/httpapi/... ./cmd/... ./internal/tenant/...`; then `rtk proxy bash Server/scripts/test-identity.sh`.

Expected: PASS with real role-constrained PostgreSQL and both explicit race orderings. No migration checks are skipped or reduced to a table count.

- [ ] **Step 10 (2 min): Commit restore fencing.**

```bash
rtk git add Server/internal/recovery Server/internal/security/transaction.go Server/internal/security/transaction_test.go Server/internal/migrate Server/internal/config Server/cmd Server/internal/httpapi Server/internal/tenant/context_test.go Server/scripts/check-migration-overlap.sh Server/scripts/migration_overlap_test.go Server/migrations/000004_recovery_compatibility.sql
rtk git commit -m "feat(server): quarantine recovery and fence tenant transactions"
```

### Task 3: Freeze bounded Apple proof, nonce and HTTP contracts (P1.3)

- [ ] **Step 0 (4 min): Land the reviewed schema-5 compatibility predecessor.** Capture the accepted Task 2/schema-4 feature head as `BT_MIGRATION_BASE_REF`; it is the immutable base, not the predecessor. Create final immutable 000005 with the complete Task 4 auth/session contract, full exact v5 privilege snapshot, overlap tests/scenarios, and manifest `[4,5]`; run the predecessor suite on schema 4; commit only those compatibility files as `feat(server): prepare identity schema compatibility`; review that commit and only then capture it as `BT_MIGRATION_PRIOR_REF`. The compatibility predecessor must remain deployable on schema 4 and must make no v5 production call. Its allowlist is migration 000005 and `compatibility.json`; the minimum reserved authority-shape changes in `internal/security/{commands,event}.go` plus their focused tests; exact migrate/database/tenant privilege and compatibility tests; the overlap checker and its tests; and smoke/testpg hard-coded schema-version or migration-count updates. Do not add identity, auth, limit, HTTP-route, server-composition, Compose-secret, or other feature production code to this commit. All remaining Task 3 edits belong to the `[5,5]` feature commit.

**Files:**

- Create: `Server/internal/identity/types.go`, `nonce.go`, `challenge.go`, `apple.go`, `jwks.go`, `repository.go`, `nonce_test.go`, `challenge_test.go`, `apple_test.go`.
- Create: `Server/internal/limits/policy.go`, `limiter.go`, `repository.go`, `limiter_test.go`; `Server/internal/protocol/auth.go`, `decode.go`, `auth_test.go`; `Server/internal/config/auth.go`, `limits.go`; `Server/internal/httpapi/auth.go`, `auth_test.go`; `Server/migrations/000005_identity_sessions.sql`.
- Create: `Server/api/fixtures/auth-nonce-v1.json`, `auth-challenge.json`, `auth-errors.json`; `Server/docs/protocol/identity-v1.md`; `Server/internal/testidentity/issuer.go` (test build only).
- Modify: `Server/api/openapi.yaml`, `Server/internal/protocol/types_test.go`, `Server/internal/config/config.go`, `config_test.go`, `Server/internal/httpapi/router.go`, `Server/internal/migrate/compatibility.json`, `Server/internal/tenant/context_test.go`, `Server/internal/database/{roles.go,roles_test.go}`, `Server/cmd/baby-tracker-server/{main.go,main_test.go}`, `Server/internal/testpg/database.go`, `Server/scripts/{check-migration-overlap.sh,migration_overlap_test.go,smoke.sh,smoke_test.go}`, `Server/compose.yaml`, and `Server/README.md` where required by the exact credentials, route composition, v5 privileges and schema-count smoke.
- Test: nonce vectors, Apple JWS table cases, strict request/tolerant response decoding and real PostgreSQL challenge/rate races.

**Interfaces:**

```go
// identity/types.go
type Challenge struct { Challenge,ServerInstanceID,CanonicalOrigin string; ExpiresAt time.Time }
type ProfileInput struct { DisplayName string; Confirmed bool }
type ExchangeRequest struct {
    CommandID,Challenge,IdentityToken,AuthorizationCode,InstallationID string
    Profile *ProfileInput
    DeviceContinuity *DeviceContinuityClaim
}
type DeviceContinuityClaim struct { DeviceID,Credential string }
type VerifiedIdentity struct { Provider,Subject,Audience,IdentityKey string; IssuedAt,ExpiresAt time.Time }
type DigestKey struct { ID string; Secret [32]byte }
type DigestKeyring struct { Current DigestKey; Previous []DigestKey }
type KeyedDigest struct { KeyID string; Sum [32]byte }
type IdentityVerifier interface {
    Verify(context.Context,string,string) (VerifiedIdentity,error)
}
type Service struct {
    verifier IdentityVerifier
    repository *Repository
    origin,instanceID string
    identityKeys DigestKeyring
    now func() time.Time
}
type Repository struct { runtime *pgxpool.Pool }
func (s *Service) Challenge(context.Context) (Challenge,error)
func (s *Service) VerifyExchange(context.Context,ExchangeRequest) (VerifiedIdentity,error)
func AppleNonce(canonicalOrigin,instanceID,challenge string) (string,error)
// limits/policy.go
type Policy struct { Count int; Window time.Duration }
type Decision struct { Allowed bool; Remaining int; RetryAfter time.Duration }
type Charge struct { Scope,Key string; Cost int; Policy Policy }
type Limiter interface { Take(context.Context,string,string,Policy) (Decision,error); TakeMany(context.Context,[]Charge) (Decision,error) }
```

Consumes P1.2 gate/fence and runtime/projector-role boundaries. `InstallationID` is a client random installation hint, never a device ID. Constructor options use `IdentityKeys DigestKeyring`; `NewRepository(runtime *pgxpool.Pool)` is runtime-only and exposes fixed-function challenge/lookup/staging operations. No identity/auth/HTTP option retains projector capability. `NewAppleVerifier` uses fixed production Apple endpoints with injected test transport only.

- [ ] **Step 1 (3 min): Write the shared nonce vector and relay test.**

```go
func TestNonceV1AndCrossServerRelay(t *testing.T) {
    challenge := "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
    id := "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"
    got,err := AppleNonce("https://sync.example.test",id,challenge)
    if err!=nil || got!="6227c68e4a909c26acc63eaa3f0da6d0446d861a6edaf459330283c2e4afbbc1" { t.Fatalf("nonce=%q err=%v",got,err) }
    other,_ := AppleNonce("https://other.example.test",id,challenge)
    if other==got { t.Fatal("origin not bound") }
    other,_ = AppleNonce("https://sync.example.test","a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"[:35]+"e",challenge)
    if other==got { t.Fatal("instance not bound") }
}
```

- [ ] **Step 2 (2 min): Run RED.**

Run: `rtk proxy sh -c 'cd Server && go test ./internal/identity -run TestNonceV1AndCrossServerRelay -count=1'`.

Expected: FAIL because `AppleNonce` is undefined. Persist the same input/output fields in `auth-nonce-v1.json` for Swift Plan 3.

- [ ] **Step 3 (3 min): Implement origin normalization and exact nonce bytes.**

```go
func AppleNonce(origin,instanceID,challenge string) (string,error) {
    if err := validateOriginBinding(origin,instanceID,challenge); err!=nil { return "",err }
    sum := sha256.Sum256([]byte("baby-tracker-auth-v1\x00"+origin+"\x00"+instanceID+"\x00"+challenge))
    return hex.EncodeToString(sum[:]),nil
}
```

`validateOriginBinding(string,string,string) error` implements D6 and checks decoded challenge length/canonical re-encoding. Expose `config.CanonicalOrigin(string,Mode) (*url.URL,error)` replacing the private parser without broadening development to unsupported schemes. Table tests reject literal backslash-zero nonce construction, encoded slash/credentials/query/fragment, duplicate host separators, port zero/out-of-range and whitespace; accept `HTTPS://Sync.Example.Test:443/` only after normalization.

- [ ] **Step 4 (4 min): Implement bounded Apple verifier and JWKS cache.**

```go
type JWKSCache struct { http *http.Client; now func()time.Time; mu sync.Mutex; keys map[string]*rsa.PublicKey; expires,lastRefresh time.Time }
func (c *JWKSCache) Key(context.Context,string) (*rsa.PublicKey,error)
type AppleVerifier struct { keys *JWKSCache; audience string; now func()time.Time }
func (v *AppleVerifier) Verify(context.Context,string,string) (VerifiedIdentity,error)
```

Require three JWT segments, <=16 KiB token, bounded JSON depth/keys, no duplicate claims/header keys; header `alg=RS256`, `kid` <=128 ASCII bytes, no `jku`/`x5u` trust. Fixed JWKS URL is `https://appleid.apple.com/auth/keys`; 5-second HTTP deadline, no redirect, <=64 KiB body, <=16 RSA signing keys, modulus 2048–4096 bits and exponent >=3 odd. Verify `rsa.VerifyPKCS1v15` with SHA-256, exact issuer `https://appleid.apple.com`, exact configured audience, nonempty subject <=255 bytes, `exp>now`, `iat<=now+60s`, `iat>=now-10m`, constant-time nonce equality. Cache valid keys for `min(Cache-Control max-age,6h)` with default 1h; expired cache fails closed. Unknown kid triggers at most one coalesced refresh per 30 seconds, then denies. Explicitly reject algorithms outside the allowlist even if the header asks for them.

- [ ] **Step 5 (4 min): Add single-use challenges and identity/session schema.**

```sql
CREATE TABLE auth_challenges (
 id uuid PRIMARY KEY, token_digest bytea UNIQUE NOT NULL CHECK(octet_length(token_digest)=32),
 canonical_origin text NOT NULL, server_instance_id uuid NOT NULL,
 expires_at timestamptz NOT NULL, consumed_at timestamptz
);
REVOKE ALL ON auth_challenges FROM PUBLIC,baby_tracker_runtime;
```

Migration 000005 is the complete immutable persistence contract for Task 4. Every UUID is nonzero at the Go/SQL boundary, every timestamp is finite, every digest is exactly 32 bytes, bounded text rejects NUL, and no sequence is created. Challenge IDs alone may use `gen_random_uuid()`; all other IDs are application supplied. Every key ID matches `^[a-z][a-z0-9._-]{0,63}$`; recovery epoch and security revision are integers in `1..9007199254740991`; identity subject/audience are `1..255` UTF-8 bytes; canonical origin is `1..2048` ASCII bytes; display name is `1..80` Unicode scalars and at most 320 UTF-8 bytes; session scope is exactly `account`, `family`, or `family_recovery`, with `family_id` null iff scope is `account`.

The exact table contract is:

- Add `accounts.profile_required boolean NOT NULL DEFAULT false`.
- `account_identities`: `provider` constrained to `apple`, bounded `identity_key_id`, 32-byte `identity_digest`, account FK, audience/subject and Apple credential-reference fields, `material_missing boolean NOT NULL DEFAULT false`, and created/updated timestamps. Primary key is `(provider,identity_key_id,identity_digest)` and `(account_id,provider)` is unique. Audience and subject are both null iff `material_missing=true`, otherwise both nonnull. The credential quartet is null when material is missing and otherwise either all-null or all-nonnull.
- `devices`: ID primary key, account, `UNIQUE(id,account_id)` composite FK target, server instance, canonical origin, nullable installation key ID/digest, nonnull continuity key ID/digest, binding-revoked time, and created/updated timestamps. Installation and continuity are separately keyed; raw installation hints and continuity credentials are forbidden.
- `auth_challenges`: ID, unique 32-byte token digest, canonical origin, server instance, expiry/consumed/created timestamps, exact five-minute expiry ordering, and an unconsumed-expiry index.
- `auth_exchange_material`: `command_id` primary key, command hash, projection-material digest, kind (`exchange` or `refresh`), every account/device/session/parent/lineage/server/origin binding, every identity/profile/installation/continuity/token/epoch/revision/scope/family/deadline field in the canonical stage contract below, the complete Apple credential-reference binding, and created/expires/projected timestamps. `created_at` equals the function's `p_now` and `expires_at` equals `p_now + interval '10 minutes'`; the caller cannot choose either. Exact restage is idempotent; any changed field or digest conflicts.
- `sessions`: `session_id` primary key; account/device/lineage-root/parent/server IDs; positive bounded recovery epoch and security revision; scope and nullable family; token key ID plus access/refresh digests; access, refresh, reauthentication and absolute deadlines; status (`active`, `consumed`, `revoked`, `material_missing`); consume/revoke metadata; and created/updated timestamps. Token key ID, both token digests and access/refresh deadlines are nullable only when status is `material_missing`; `reauthenticate_at` and `absolute_expires_at` are always nonnull. A normal row enforces `access_expires_at <= refresh_expires_at <= reauthenticate_at = absolute_expires_at`; a material-missing row has no bearer digests or access/refresh deadlines and has `reauthenticate_at = absolute_expires_at =` the authority effect's `expires_at`. Enforce account/device and self-parent/lineage FKs, partial unique `(token_key_id,access_digest)` and `(token_key_id,refresh_digest)` indexes, and account/device/lineage status indexes.
- `session_refresh_history`: token key ID plus 32-byte refresh digest as the primary key, session and lineage-root session IDs with FKs to `sessions(session_id)`, consumed time and nullable reuse-detected time; index the lineage.
- `apple_notification_dedup`: notification key ID plus 32-byte digest as the primary key, unique deterministic command ID and received time. It is only an optimization; the authority receipt remains restore-safe replay evidence.
- `apple_revocation_jobs`: schema 5 creates `job_id` primary key; account/server IDs; complete credential purpose/object/reference ID/reference key binding; status (`pending`, `retry`, `complete`), attempts, next-attempt/created/updated/completed times and bounded safe last-error code; unique full credential binding. Credential reference ID/key use `[A-Za-z0-9._-]{1,128}`, purpose is exactly `apple-refresh-revocation-v1`, object ID is a canonical nonzero UUID, and nullable safe error is `[a-z0-9_]{1,64}`. Plaintext credentials are forbidden. Task 6 migration 000007 replaces that status constraint with (`pending`,`retry`,`revoke_succeeded`,`cleanup_pending`,`complete`) and adds provider-revoked, cleanup and lease metadata without changing 000005 bytes.
- `rate_limit_buckets`: exact scope, 32-byte key digest, `window_start`, `window_end`, and count in `1..1000000`. Primary key is `(scope,key_digest,window_start)`, duration is finite and in `(0,24h]`, and an expiry index includes `window_end`.

All tables are migrator-owned. Revoke every direct table/column/sequence privilege from PUBLIC, runtime and projector. The complete cumulative v5 privilege snapshot copies every exact v4 row unchanged, adds runtime EXECUTE for only the nine exact functions below, and leaves projector with only the exact `app.project_security_event(...)` and `app.update_server_state(...)` signatures. Auth finalization is part of `project_security_event`; migration 000005 creates no third projector function or grant. Runtime cannot execute projector functions, projector cannot execute runtime functions, PUBLIC executes neither, and `schema_compatibility` advances transactionally to 5.

The nine runtime-only functions fix `search_path=pg_catalog`, fully qualify every object, validate all inputs, and have these exact signatures and result columns:

```sql
app.create_auth_challenge(bytea,text,uuid,timestamptz,timestamptz) RETURNS uuid
app.consume_auth_challenge(bytea,text,uuid,timestamptz) RETURNS uuid
app.lookup_auth_identity(text,text,bytea)
  RETURNS TABLE(account_id uuid,authority_status text,display_name text,profile_required boolean)
app.lookup_device_binding(uuid,uuid,uuid,text)
  RETURNS TABLE(continuity_key_id text,continuity_digest bytea,binding_revoked boolean)
app.lookup_access_session(text,bytea)
  RETURNS TABLE(session_id uuid,account_id uuid,device_id uuid,lineage_root_session_id uuid,
    server_instance_id uuid,recovery_epoch bigint,security_revision bigint,scope text,family_id uuid,
    access_expires_at timestamptz,refresh_expires_at timestamptz,reauthenticate_at timestamptz,
    absolute_expires_at timestamptz,session_status text,consumed_at timestamptz,
    revoked_at timestamptz,revoke_reason text)
app.lookup_refresh_session(text,bytea)
  RETURNS TABLE(session_id uuid,account_id uuid,device_id uuid,parent_session_id uuid,
    lineage_root_session_id uuid,server_instance_id uuid,recovery_epoch bigint,security_revision bigint,
    scope text,family_id uuid,access_expires_at timestamptz,refresh_expires_at timestamptz,
    reauthenticate_at timestamptz,absolute_expires_at timestamptz,session_status text,
    refresh_consumed_at timestamptz,reuse_detected_at timestamptz,revoked_at timestamptz,revoke_reason text)
app.take_rate_limits(jsonb,timestamptz)
  RETURNS TABLE(allowed boolean,remaining integer,retry_after_seconds integer)
app.stage_auth_material(uuid,bytea,bytea,bytea,timestamptz) RETURNS void
app.reserve_apple_notification(jsonb,text,bytea,uuid,timestamptz)
  RETURNS TABLE(command_id uuid,newly_reserved boolean)
```

`lookup_auth_identity` parameters are provider, identity key ID and digest; access/refresh lookup parameters are token key ID and digest. Challenge consumption uses the frozen one-row conditional `UPDATE ... RETURNING` and a failed proof still burns its challenge.

`stage_auth_material` accepts `1..32768` exact canonical JSON bytes as its fourth `bytea` argument, never `jsonb`. It first verifies the separately supplied SHA-256 against those untouched bytes, then rejects invalid UTF-8, duplicate/escaped-equivalent keys, non-object/trailing JSON, and any missing, extra, wrong-type or kind-inapplicable key. It parses typed fields, reconstructs the fixed Go-compatible form, and requires a byte-for-byte match before inserting typed columns. `version` is the JSON string `"v1"` and `kind` is `"exchange"` or `"refresh"`. Every common key is present, never omitted, in this order: `version`, `kind`, `account_id`, `device_id`, `session_id`, `lineage_root_session_id`, `server_instance_id`, `canonical_origin`, `token_key_id`, `access_digest`, `refresh_digest`, `recovery_epoch`, `security_revision`, `scope`, `family_id`, `access_expires_at`, `refresh_expires_at`, `reauthenticate_at`, `absolute_expires_at`. All common values are nonnull except `family_id`, which is explicit JSON null iff scope is `account`. An `exchange` stage then contains, in order, `identity_provider`, `identity_key_id`, `identity_digest`, `identity_audience`, `identity_subject`, `display_name`, `profile_confirmed`, `register_account`, `register_device`, `installation_key_id`, `installation_digest`, `continuity_key_id`, `continuity_digest`, `apple_credential_id`, `apple_credential_key_id`, `apple_credential_purpose`, `apple_credential_object_id`; every optional exchange key is present with JSON null rather than omitted. Exchange has no `parent_session_id`; provider/key/digest/audience/subject and both registration booleans are nonnull; display name is null exactly when no profile value is staged (`profile_confirmed=false`) and any nonnull name requires `profile_confirmed=true`; installation key/digest are both null or both nonnull; continuity key/digest are both nonnull exactly when `register_device=true` and both null for reuse; the four Apple credential fields are all null or all nonnull. A `refresh` stage instead adds exactly nonnull `parent_session_id` and contains none of the exchange-only keys. Raw access, refresh, continuity and Apple credentials are never accepted.

The material bytes are `encoding/json.Marshal` of fixed tagged Go structs in the field order above, with no maps and no `omitempty`; UUIDs are lowercase canonical, timestamps are UTC RFC3339Nano, and absent optional values are typed nil pointers producing JSON null. Stage JSON and SQL columns always represent a digest as a separate bounded key ID plus raw lowercase 64-hex digest (stored as 32-byte `bytea`), never as a keyed-digest string. Authority effects alone use `hmac-sha256:<key-id>:<64-lowercase-hex>` for identity/continuity/token authority digests. `projection_material_digest` is a raw, unkeyed lowercase 64-hex SHA-256 of the exact stage bytes. Projection requires the stage's command ID, command hash and material digest to match the authority event exactly.

`reserve_apple_notification` receives `1..8` current-first exact candidate objects `{"key_id":"...","digest":"<64-lowercase-hex>"}` with unique key IDs and unique `(key_id,digest)` pairs; candidate zero must equal the separate current key/digest arguments. In one transaction it fails closed if multiple candidates match stored rows, returns the sole stored `command_id,false` for one match, or inserts/returns the supplied current `(key_id,digest,command_id,received_at)` as `true` for no match. Changed proposed command IDs never overwrite the original. SQL state alone is never notification completion.

Migration 000005 replaces `app.project_security_event(...)` without weakening migration 000003 canonical-byte, chain-head, audit or receipt validation. It adds only `identity_session_issued`, `session_rotated`, `session_revoked`, and `device_binding_revoked`, and extends the existing `identity_unavailable` projection. It does not install `family_bootstrapped`; schema 000006 owns that case. The authority/projector contract is set-wise and bounded:

- `identity_session_issued` has optional one `account_registered`, optional one `device_registered`, and exactly one `session_granted`, all with identical account/device/origin/instance/material bindings.
- `session_rotated` has exactly one effect. `SessionPayload.ProjectionMaterialDigest` is optional for other session event kinds but is required as a lowercase 64-hex value for `session_rotated`; the stage and effect digest must match. Projection consumes the parent, writes refresh history and creates the successor atomically.
- `session_revoked` has exactly one effect naming any member of the lineage. SQL implicitly revokes the complete `(account_id,device_id,lineage_root_session_id)` set; no event enumerates sessions.
- `device_binding_revoked` has exactly one effect. SQL implicitly revokes the matching device binding and all of that device's sessions; no declared session-revocation effects are allowed.
- `identity_unavailable` implicitly revokes every session for the account but preserves all device bindings. Restoration is an atomic `identity_session_issued` for the same unavailable actor and never revives an old session.

If an `identity_session_issued` stage is missing during replay, projection creates an empty-profile account with `profile_required=true`, a keyed opaque identity, the keyed opaque device binding carried by the event, and a `material_missing` session with no bearer digest. If a `session_rotated` stage is missing, it still consumes the parent and creates only the opaque successor as `material_missing`; it never reconstructs a credential. Present-but-mismatched stage, effect or prior state fails closed. Exact replay is idempotent.

Challenge reservation/staged material commits before the external authority; after a crash before head commit, a fresh challenge is required. After head commit, projector reconciliation finalizes from the exact stage or the safe missing-stage rules above. This explicitly avoids pretending PostgreSQL and the external head share a transaction.

- [ ] **Step 6 (4 min): Write verifier/challenge behavioral tests.**

```go
func TestChallengeSingleUse(t *testing.T) {
    h := newIdentityFixture(t)
    req := h.ValidExchange()
    var wins atomic.Int32
    var wg sync.WaitGroup
    for i:=0;i<2;i++ { wg.Go(func(){ if _,err:=h.Service.VerifyExchange(t.Context(),req); err==nil { wins.Add(1) } }) }
    wg.Wait()
    if wins.Load()!=1 { t.Fatalf("successful exchanges=%d",wins.Load()) }
}
```

Define local `identityFixture{Service *Service; Clock func()time.Time}` and `newIdentityFixture(*testing.T) *identityFixture`, `ValidExchange() ExchangeRequest`: fixture uses `testpg.New`, fresh RSA key pair, test HTTP transport serving JWKS and a signed identity token with the actual challenge nonce. In `TestAppleProofRejections` change one field at a time: issuer, substring audience, expired token, future/old iat, signature, nonce, kid, `alg:none`, HS256, duplicate `aud`, overlarge key list, redirect; require `invalid_apple_proof`. A hosted/test token from another configured origin or instance with the same audience fails. `TestProfileNameRequiresConfirmation` proves missing first-response name returns `display_name_required`; email/subject are never fallbacks. Existing account with absent profile input retains its name.

- [ ] **Step 7 (4 min): Freeze limits and implement one atomic bucket increment.**

Defaults: auth body 32 KiB; all other identity/family bodies 64 KiB; profile/family names 1–80 Unicode scalar values after trim, at most 320 UTF-8 bytes, no controls; authorization code 4096 bytes; installation hint 128 bytes; maximum capabilities 16 strings each <=64 ASCII bytes. Rate windows: challenge/exchange 30/IP/10min, refresh 120/IP/10min plus 60/account/10min plus 30/device/10min, family structural commands 30/account/min plus 60/device/min, invitation create/redeem 20/account/hour plus 60/IP/hour. The only accepted scope strings are `auth-challenge-ip`, `auth-exchange-ip`, `auth-refresh-ip`, `auth-refresh-account`, `auth-refresh-device`, `family-structural-account`, `family-structural-device`, `invitation-create-account`, `invitation-create-ip`, `invitation-redeem-account`, and `invitation-redeem-ip`. These are configurable exact Plan 1 defaults, not a claimed performance workload.

```sql
INSERT INTO rate_limit_buckets(scope,key_digest,window_start,window_end,count)
VALUES($1,$2,$3,$4,$5)
ON CONFLICT(scope,key_digest,window_start)
DO UPDATE SET count=rate_limit_buckets.count+EXCLUDED.count
WHERE rate_limit_buckets.window_end=EXCLUDED.window_end
  AND rate_limit_buckets.count+EXCLUDED.count <= $6
RETURNING count;
```

The `take_rate_limits` JSON argument contains `1..3` exact charge objects with keys `scope`, `key_digest`, `cost`, `limit`, `window_start`, `window_end`. Charges are sorted and unique by `(scope,key_digest)`; `cost` and `limit` are integers in `1..1000000` with `cost <= limit`; and `window_start <= now < window_end` with finite duration in `(0,24h]`. Duplicates, unknown scopes, malformed digests, unsorted input or an existing stored count outside `1..1000000` fail before admission. The table primary key remains `(scope,key_digest,window_start)`. The PL/pgSQL function applies canonical-order upserts inside a caught nested `BEGIN ... EXCEPTION` subtransaction: any denied/mismatched charge raises a private exception, which rolls back every earlier increment before the outer handler returns `allowed=false`. It may instead use one proven atomic SQL statement with equivalent rollback evidence; an ordinary loop that returns after partial updates is forbidden. `remaining` is the minimum remaining allowance and `retry_after_seconds` is the ceiling to the latest blocking `window_end`.

Bucket key is HMAC over scope + NUL + normalized key with dedicated 32-byte pepper; never persist IP text. Decode IP from `RemoteAddr`; use forwarded chain only if immediate peer belongs to configured trusted proxies, walk right-to-left past trusted hops, reject malformed/ambiguous headers. `429 rate_limited` includes integer `Retry-After`, safe `retry_after_seconds` response extension; dependency failure returns retryable 503 rather than bypass. Check IP quota before JWT verification, known account/device quota after safe lookup before mutation. Expired-bucket pruning is reserved for Plan 4's maintenance role.

- [ ] **Step 8 (4 min): Pin strict requests and additive responses.**

```go
func TestResponseAdditionDoesNotBreakDecoder(t *testing.T) {
    var got protocol.ChallengeResponse
    err:=json.Unmarshal([]byte(`{"protocol_version":"v1","challenge":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8","server_instance_id":"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","canonical_origin":"https://sync.example.test","expires_at":"2026-09-20T12:05:00Z","future_field":true}`),&got)
    if err!=nil || got.Challenge=="" { t.Fatal("additive response rejected") }
}
```

Define DTOs `RequestMeta{ProtocolVersion string; Capabilities []string}` embedded with explicit JSON fields, `ChallengeRequest{RequestMeta}`, `ChallengeResponse{ProtocolVersion,Challenge,ServerInstanceID,CanonicalOrigin,ExpiresAt string}`, `AppleExchangeRequest{RequestMeta; CommandID,Challenge,IdentityToken,AuthorizationCode,InstallationID string; Profile *identity-independent protocol.ProfileInput; DeviceContinuity *protocol.DeviceContinuityClaim}`. The continuity DTO has `DeviceID string json:"device_id"` and `Credential string json:"credential"`; P1.4 authenticates it after Apple proof, never the protocol decoder. Protocol profile type has `DisplayName string`, `Confirmed bool`; HTTP explicitly maps it to identity type. `protocol.DecodeRequest(io.Reader,int64,any) error` rejects oversized JSON, duplicates, unknown fields, trailing JSON and missing version/array. `protocol.ValidateCapabilities([]string,[]string) error` validates the single shared lowercase-hyphen grammar, duplicates/length/count and operation registry; invalid/unsupported values return only `unsupported_capability`. Production responses decode permissively; canonical fixture-shape tests may stay strict. Replace OpenAPI server URL with `https://{host}` and remove basePath. No response schema promises that added fields are invalid.

- [ ] **Step 8a (3 min): Pin shared capability spelling and error semantics.**

```go
func TestCapabilityConvention(t *testing.T) {
 supported:=[]string{"identity-v1","family-lifecycle-v1","recovery-v1","device-continuity-v1"}
 if err:=ValidateCapabilities(supported,supported); err!=nil { t.Fatal(err) }
 for _,bad:=range []string{"identity_v1","Identity-v1","identity--v1","future-required-v1"} {
   if err:=ValidateCapabilities([]string{bad},supported); ErrorCodeOf(err)!="unsupported_capability" { t.Fatalf("capability=%q",bad) }
 }
}
```

Define `protocol.ErrorCodeOf(error) string` for decoder/validation errors; it maps only typed safe protocol errors. RED: `rtk proxy sh -c 'cd Server && go test ./internal/protocol -run TestCapabilityConvention -count=1'`, expected missing validator; implement the grammar/operation registry; GREEN repeats the command. Add the four values to OpenAPI/auth fixtures and test that server advertised capabilities and registries use identical strings. P2 modifies this existing decoder/registry rather than creating a second decoder. A mismatch otherwise prevents a conforming client from authenticating or negotiating sync.

- [ ] **Step 9 (3 min): Wire challenge route and isolate test issuer.**

```go
func challengeHandler(s *identity.Service,limiter limits.Limiter,gate security.RecoveryGate) http.HandlerFunc
// internal/testidentity/issuer.go first line:
//go:build identitytest
```

Register `POST /v1/auth/challenge` only after readiness/limit checks; Apple exchange route is registered with P1.4 when it issues sessions. `internal/testidentity/issuer.go` uses `//go:build identitytest` and is imported only from test-tagged binaries/tests; normal app configuration has no alternate issuer field and rejects any `BT_TEST_ISSUER` setting. Add `TestProductionRejectsTestIssuerConfig` and `TestAuthErrorsNeverEchoSecrets` feeding distinctive token/name strings through every malformed-body/verification path and scanning captured JSON logs and response bodies. Source references: [Apple verification](https://developer.apple.com/documentation/signinwithapple/verifying-a-user), [Apple public signing keys](https://appleid.apple.com/auth/keys); the live key endpoint returned RSA/RS256 keys on 2026-09-20. Do not infer ES256 ID-token support from Apple client-secret signing examples.

- [ ] **Step 10 (3 min): Run GREEN and schema/route checks.**

Run: `rtk proxy bash Server/scripts/test-identity.sh ./internal/identity/... ./internal/limits/... ./internal/protocol/... ./internal/config/... ./internal/httpapi/...`.

Expected: PASS, exact nonce fixture matches, concurrent challenges have one winner, weighted rate charges are all-or-none under 100 parallel attempts, fake untrusted `X-Forwarded-For` cannot reset a bucket, and registered/OpenAPI route sets match. Real-role tests assert all nine key-versioned runtime signatures, both projector signatures, direct-DML/PUBLIC/cross-role denial, no sequence, the complete v5 privilege snapshot, and missing-stage/implicit-revocation projector behavior. The compatibility predecessor updates only the hard-coded schema/migration-count assertions in `internal/testpg` and smoke tests needed to recognize 000005; schema-4 runs still pass and a source test proves the predecessor production graph calls no v5 function.

Before any Task 3 edit, capture the accepted Task 2 head as
`BT_MIGRATION_BASE_REF`. First land a compatibility-only predecessor commit
containing final immutable `000005_identity_sessions.sql`, its exact complete
v5 privilege snapshot, overlap tests/scenarios, and manifest `[4,5]`; it must
remain deployable on schema 4 and call no v5-only object. Review it, then set
`BT_MIGRATION_PRIOR_REF` to that commit. Implement Task 3 feature code and
generate `[5,5]`; run the focused/full gates above, make the feature commit in
Step 11, and set `BT_MIGRATION_FEATURE_REF` to that immutable commit. Only then
run:

```bash
rtk proxy bash Server/scripts/check-migration-overlap.sh \
  --base-ref "$BT_MIGRATION_BASE_REF" \
  --prior-ref "$BT_MIGRATION_PRIOR_REF" \
  --feature-ref "$BT_MIGRATION_FEATURE_REF" \
  --next-version 5 --prior-min 4 --prior-max 5 --next-min 5 --next-max 5
```

Prepared mode must archive all three exact commits without modifying them. The
predecessor passes its registered identity/structural-denial scenario at schema
4, migrates to 5, passes again at 5, and the feature binary then passes at 5.

- [ ] **Step 11 (2 min): Commit the identity proof feature and run prepared overlap.**

```bash
rtk git add Server/internal/identity Server/internal/limits Server/internal/protocol Server/internal/config Server/internal/httpapi Server/internal/testidentity Server/internal/migrate/compatibility.json Server/internal/tenant Server/internal/database Server/cmd/baby-tracker-server Server/internal/testpg Server/scripts/check-migration-overlap.sh Server/scripts/migration_overlap_test.go Server/scripts/smoke.sh Server/scripts/smoke_test.go Server/compose.yaml Server/README.md Server/api Server/docs/protocol/identity-v1.md
rtk git commit -m "feat(server): verify nonce-bound Apple identity proofs"
BT_MIGRATION_FEATURE_REF=$(rtk git rev-parse HEAD)
rtk proxy bash Server/scripts/check-migration-overlap.sh --base-ref "$BT_MIGRATION_BASE_REF" --prior-ref "$BT_MIGRATION_PRIOR_REF" --feature-ref "$BT_MIGRATION_FEATURE_REF" --next-version 5 --prior-min 4 --prior-max 5 --next-min 5 --next-max 5
```

### Task 4: Issue opaque sessions and revoke refresh reuse (P1.4)

**Files:**

- Create: `Server/internal/auth/types.go`, `repository.go`, `tokens.go`, `sessions.go`, `devices.go`, `refresh.go`, `middleware.go`, `sessions_test.go`, `devices_test.go`, `refresh_test.go`, `repository_test.go`, `composition_test.go`; `Server/internal/identity/apple_exchange.go`, `apple_notifications.go`, `credentials.go`, `apple_exchange_test.go`, `apple_notifications_test.go`, `credentials_test.go`.
- Modify: `Server/internal/httpapi/auth.go`, `auth_test.go`, `router.go`; `Server/internal/config/auth.go`, `config_test.go`; `Server/internal/protocol/auth.go`, `auth_test.go`; `Server/internal/security/commands.go`, `commands_test.go`, `state.go`, `state_test.go`, `event.go`, `event_test.go`, `transaction.go`, `transaction_test.go`, `coordinator.go`, `coordinator_test.go`, `projector.go`; `Server/internal/recovery/state.go`, `state_test.go`; `Server/cmd/baby-tracker-server/main.go`, `main_test.go`; `Server/compose.yaml`; `Server/deploy/postgres/init-users.sh`; `Server/scripts/smoke.sh`, `smoke_test.go`, `test-identity.sh`; `Server/README.md`; `Server/api/openapi.yaml`; `Server/docs/protocol/identity-v1.md` (continuity/receipt handoff).
- Create: `Server/scripts/task4_immutability_test.go`.
- Create: `Server/api/fixtures/auth-session.json`, `auth-reauth-continuity.json`, `auth-device-continuity-invalid.json`, `auth-refresh-reused.json`, `apple-notification.json`.
- Test: token-rest storage checks, refresh races/loss/epoch, hosted exchange and signed notification tests.

**Interfaces:**

```go
// internal/auth/types.go
type AccountView struct { ID,DisplayName string; ProfileRequired bool }
type FamilySlot struct { FamilyID,State string } // active or family_recovery
type SessionBundle struct {
    AccessToken,RefreshToken,DeviceID string
    DeviceCredential string // nonempty only for a newly registered installation
    RecoveryEpoch uint64 // exact fenced epoch that minted this bundle
    ExpiresAt,ReauthenticateAt time.Time
    Account AccountView
    FamilySlot *FamilySlot
}
type Service struct {
    identity *identity.Service
    verifier identity.IdentityVerifier
    security *security.Service
    repository *Repository // fixed runtime-only functions; no pool escapes this deep adapter
    apple identity.AppleCodeExchanger
    credentials identity.CredentialRotator // deep rotation module; no raw vault/revoker capability
    mode config.Mode
    tokenKeys,deviceKeys,notificationKeys identity.DigestKeyring
    now func() time.Time
    accessTTL,refreshTTL,absoluteAge time.Duration
}
func (s *Service) Exchange(context.Context,identity.ExchangeRequest) (SessionBundle,error)
func (s *Service) Authenticate(context.Context,string) (access.Principal,error)
func (s *Service) Refresh(context.Context,string) (SessionBundle,error)
func (s *Service) Logout(context.Context,access.Principal,string) (security.CommandResult,error)
// final Logout string is canonical command ID.
type DeviceBinding struct { DeviceID,AccountID,ServerInstanceID,CanonicalOrigin,CredentialKeyID string; CredentialDigest [32]byte; Revoked bool }
func (s *Service) ResolveDevice(context.Context,identity.VerifiedIdentity,*identity.DeviceContinuityClaim) (DeviceBinding,string,error)
// Returned string is a new raw continuity credential only on initial registration.
// internal/identity/apple_exchange.go
type AppleTokens struct { IdentityToken,RefreshToken string }
type AppleCodeExchangeRequest struct { AuthorizationCode,ExpectedNonce string; ExpectedIdentity VerifiedIdentity }
type AppleCodeExchanger interface {
    ExchangeCode(context.Context,AppleCodeExchangeRequest) (AppleTokens,error)
}
type AppleRevocationRequest struct { Binding CredentialBinding; RefreshToken []byte }
type AppleRevoker interface { Revoke(context.Context,AppleRevocationRequest) error }
type AppleAuthority interface {
    AppleCodeExchanger
    AppleRevoker
}
// internal/identity/credentials.go
type CredentialBinding struct { AccountID,ServerInstanceID,Purpose,ObjectID string }
type CredentialRef struct { ID,KeyID string }
type CredentialRotationState struct {
    Ref CredentialRef
    Version,OperationID,CommandID,Phase string // absent, ready, reserved, candidate, retiring, promoted, adopting, aborting, deleting
    ReconcileAfter time.Time
    ExpectedCommandHash,MarkerDigest [32]byte
    MarkerSequence uint64
    HasCurrent,HasPending bool
}
type DeletionCapability struct { Opaque []byte } // authenticated, non-secret carry token; callers never inspect it
type AdoptionCapability struct { Opaque []byte } // exact promoted object/ref/provider version and operation
type PromotionCASIntent struct { Opaque []byte } // prebuilt authenticated expected-version CAS request and sealed adopting value
type DeletionCASIntent struct { Opaque []byte } // prebuilt authenticated expected-version CAS request and sealed deleting value
type DeletionOperation struct { OperationID,CommandID string }
type AdoptionRecovery struct { Binding CredentialBinding; Ref CredentialRef; OperationID,CommandID string; ExpectedCommandHash [32]byte }
type DeletionRecovery struct { Binding CredentialBinding; Ref CredentialRef; OperationID,CommandID string; MarkerSequence uint64; MarkerDigest [32]byte }
type CredentialVault interface {
    Resolve(context.Context,CredentialBinding) (CredentialRotationState,bool,error)
    ReserveRotation(context.Context,CredentialBinding,string,string) (CredentialRotationState,error)
    StoreCandidate(context.Context,CredentialBinding,CredentialRotationState,[]byte) (CredentialRotationState,error)
    OpenPrior(context.Context,CredentialBinding,CredentialRotationState) ([]byte,error)
    MarkPriorRevoked(context.Context,CredentialBinding,CredentialRotationState) (CredentialRotationState,error)
    PromotePending(context.Context,CredentialBinding,CredentialRotationState) (CredentialRotationState,error)
    PreparePromotionCAS(context.Context,CredentialBinding,CredentialRotationState,[32]byte) (PromotionCASIntent,error) // exact canonical authority command hash
    ConfirmPromotion(context.Context,PromotionCASIntent) (AdoptionCapability,error)
    RecoverAdoptionCapability(context.Context,AdoptionRecovery) (AdoptionCapability,error)
    FinishAdoption(context.Context,CredentialBinding,AdoptionCapability) error
    AbandonAdoption(context.Context,CredentialBinding,AdoptionCapability) error
    MarkCommitted(context.Context,CredentialBinding,CredentialRotationState) (CredentialRotationState,error)
    MarkAbandoned(context.Context,CredentialBinding,CredentialRotationState) (CredentialRotationState,error)
    PrepareDeletionCAS(context.Context,CredentialBinding,CredentialRotationState,DeletionOperation) (DeletionCASIntent,error)
    MarkDeleting(context.Context,DeletionCASIntent) (DeletionCapability,error)
    RecoverDeletionCapability(context.Context,DeletionRecovery) (DeletionCapability,error)
    ClaimMaintenance(context.Context,CredentialBinding,CredentialRotationState,string,time.Time) (CredentialRotationState,error)
    OpenMaintenanceTokens(context.Context,CredentialBinding,CredentialRotationState) ([][]byte,error) // defensive, one or two only
    OpenDeletingTokens(context.Context,DeletionCapability) ([][]byte,error) // defensive, one or two only
    EraseDeletingTokens(context.Context,DeletionCapability) (DeletionCapability,error)
    CompleteMaintenance(context.Context,CredentialBinding,CredentialRotationState) (CredentialRotationState,error)
    Delete(context.Context,DeletionCapability) error // conditional on the capability's provider version
    ConfirmAbsent(context.Context,CredentialBinding,CredentialRef) (bool,error)
}
type CredentialMetadata struct { Binding CredentialBinding; Ref CredentialRef; Phase string; CreatedAt time.Time }
type CredentialInventory interface {
    List(context.Context,time.Time,string,int) ([]CredentialMetadata,string,error)
}
type CredentialRotator interface {
    RotateForSession(context.Context,CredentialBinding,string,string,func(context.Context)(AppleTokens,error)) (CredentialPromotion,error)
    PreparePromotionCAS(context.Context,CredentialBinding,CredentialPromotion,[32]byte) (CredentialPromotion,error)
    ConfirmPromotion(context.Context,CredentialPromotion) (AdoptionCapability,error)
    RecoverAdoptionCapability(context.Context,CredentialPromotion) (AdoptionCapability,error)
    FinishAdoption(context.Context,CredentialBinding,AdoptionCapability) error
    AbandonAdoption(context.Context,CredentialBinding,AdoptionCapability) error
    Resume(context.Context,CredentialBinding) (CredentialResume,error)
    Commit(context.Context,CredentialBinding,string) error
    Abandon(context.Context,CredentialBinding,string) error
}
type CredentialResume struct { State CredentialRotationState; ExpectedCommandHash [32]byte; Adoption *AdoptionCapability; Deletion *DeletionCapability }
type CredentialPromotion struct { Ref CredentialRef; OperationID,CommandID,Version string; ExpectedCommandHash [32]byte; ExpiresAt time.Time; ConfirmIntent PromotionCASIntent }
func NewCredentialRotator(CredentialVault,AppleRevoker,func()string) (CredentialRotator,error)
// internal/identity/types.go and challenge.go
type VerifiedProfile struct { DisplayName string; Confirmed bool }
type VerifiedExchange struct { Identity VerifiedIdentity; Profile *VerifiedProfile }
func (s *Service) VerifyExchangeWithProfile(context.Context,ExchangeRequest) (VerifiedExchange,error)
func (s *Service) AuthorityKeyCandidates(VerifiedIdentity) ([]string,error)
// Existing VerifyExchange remains a compatibility wrapper returning only Identity.
// internal/security/transaction.go and coordinator.go
type AuthoritySnapshot struct { State State; Head Head; RecoveryEpoch uint64 }
type PreparedCommand struct { /* unexported actor and command provenance */ }
type PrepareCommand func(context.Context,AuthoritySnapshot) (PreparedCommand,error)
func (s *Service) WithAuthoritySnapshot(context.Context,func(AuthoritySnapshot) error) error
func (s *Service) ExecutePrepared(context.Context,PrepareCommand) (CommandResult,error)
type IdentityActor struct { /* unexported per-Execute snapshot capability */ }
type SessionActor struct { /* unexported per-Execute snapshot capability */ }
type CredentialDeletionCASIntent struct { Opaque []byte } // security carries but never parses the prebuilt adapter intent
type CredentialDeletionCapability struct { Opaque []byte } // exact object/ref, resulting provider version and marker sequence
type CredentialCleanupClaim struct {
    LeaseToken,OperationVersion,OperationID,CommandID,AccountID,ObjectID,ReferenceID,ReferenceKeyID string
}
type CredentialCleanupAttempt struct {
    Claim CredentialCleanupClaim
    Intent CredentialDeletionCASIntent
    ValidateClaim func(context.Context,CredentialCleanupClaim) error
    Referenced func(context.Context,CredentialCleanupClaim) (bool,error)
    MarkDeleting func(context.Context,CredentialCleanupClaim,CredentialDeletionCASIntent) (CredentialDeletionCapability,error)
    Delete func(context.Context,CredentialDeletionCapability) error
    ConfirmAbsent func(context.Context,CredentialDeletionCapability) (bool,error)
    Finish func(context.Context,CredentialCleanupClaim) error
}
func (s *Service) DeleteCredentialIfUnreferenced(context.Context,CredentialCleanupAttempt) (bool,error)
type AccountDeletionJobClaim struct {
    JobID,LeaseToken,OperationVersion,OperationID,CommandID,Status string
    AccountID,ServerInstanceID,Purpose,ObjectID,ReferenceID,ReferenceKeyID string
    Attempts int
    LeaseExpiresAt time.Time
}
type AccountDeletionCredentialAttempt struct {
    Claim AccountDeletionJobClaim
    Intent CredentialDeletionCASIntent
    ValidateClaimedJob func(context.Context,AccountDeletionJobClaim) error
    ReferencedOutsideJob func(context.Context,AccountDeletionJobClaim) (bool,error)
    MarkDeleting func(context.Context,AccountDeletionJobClaim,CredentialDeletionCASIntent) (CredentialDeletionCapability,error)
}
func (s *Service) MarkCredentialDeletingForAccountDeletion(context.Context,AccountDeletionCredentialAttempt) (CredentialDeletionCapability,error)
type CredentialKeyInventory struct { Identity,DeviceContinuity []string }
type IdentityAuthorityResolution struct {
    Status string // absent, active, identity_unavailable, or deleted
    AccountID,IdentityKey string
    LifecycleGeneration,NextActorGeneration uint64
}
func (s State) ReferencedCredentialKeyIDs() CredentialKeyInventory
func (s State) ResolveIdentityAuthority([]string) (IdentityAuthorityResolution,error)
type CommandMeta struct { CommandID string; At time.Time }
type ExistingSessionActorInput struct { AccountID,DeviceID,SessionID string; RecoveryEpoch,SecurityRevision uint64 }
type IdentitySessionCommandInput struct { Meta CommandMeta; Payload IdentitySessionIssuedPayload }
type SessionRotationCommandInput struct { Meta CommandMeta; Payload SessionPayload }
type RefreshReuseCommandInput struct { Meta CommandMeta; Payload SessionPayload }
type DeviceRevocationCommandInput struct { Meta CommandMeta; Payload DevicePayload }
type IdentityUnavailableCommandInput struct { Meta CommandMeta; Payload IdentityAuthorityPayload }
func (a AuthoritySnapshot) ResolveIdentityActor([]string,string,uint64) (IdentityActor,error)
func (a AuthoritySnapshot) ResolveSessionActor(ExistingSessionActorInput) (SessionActor,error)
func (a AuthoritySnapshot) PrepareIdentitySessionIssued(IdentityActor,IdentitySessionCommandInput) (PreparedCommand,error)
func (a AuthoritySnapshot) PrepareIdentityRestored(IdentityActor,IdentitySessionCommandInput) (PreparedCommand,error)
func (a AuthoritySnapshot) PrepareSessionRotation(SessionActor,SessionRotationCommandInput) (PreparedCommand,error)
func (a AuthoritySnapshot) PrepareRefreshReuseRevocation(SessionActor,RefreshReuseCommandInput) (PreparedCommand,error)
func (a AuthoritySnapshot) PrepareDeviceRevocation(SessionActor,DeviceRevocationCommandInput) (PreparedCommand,error)
func (a AuthoritySnapshot) PrepareIdentityUnavailable(IdentityActor,IdentityUnavailableCommandInput) (PreparedCommand,error)
func (a AuthoritySnapshot) CommandHash(PreparedCommand) ([32]byte,error) // same-snapshot provenance only
```

`AccountDeletionJobClaim` is the security-bound copy of the complete
`account.AppleRevocationJob` lease. Conversion is field-for-field; security
rejects a blank/noncanonical coordinate, noncanonical or nonpositive decimal
operation-version token, invalid attempt/status, or expired lease before
invoking callbacks. Each
callback receives the same immutable value and must revalidate every field
against the durable job row. Add `TestAccountDeletionCallbacksReceiveExactFullClaim`
and `TestAccountDeletionClaimRejectsChangedServerInstancePurposeStatusAttemptOrExpiry`.

`CredentialRotationState.ReconcileAfter` is part of the authenticated durable
vault state, not scheduler memory. It is zero only for `absent` and `ready`.
Every atomic transition into `reserved|candidate|retiring|promoted|adopting|aborting|deleting`
stores a finite UTC deadline; resolving or retrying the same phase returns that
exact value and never extends it. `ConfirmPromotion` stores the exact
`PromotionCASIntent` expiry when it enters `adopting`, and deletion stores the
exact `DeletionCASIntent` expiry when it enters `deleting`. A synchronous auth
path may abandon before the deadline only when it has a definite result proving
that the authority head CAS did not commit. A restart/reconciler may abandon an
`adopting` object only when `now >= ReconcileAfter` **and** a fenced read proves
the exact `(actor account ID, command ID, expected command hash)` receipt is
absent. A matching receipt finishes adoption; a receipt with another hash is a
credential conflict/quarantine result; an unavailable, inconsistent or
otherwise ambiguous head/receipt read retries without mutating the credential.
Task 6 amends the Task 4 state contract and adds
`TestCredentialRotationStatePersistsReconcileAfterEveryNonterminalPhase`,
`TestAdoptingAuthDefiniteFailureAbandonsBeforeDeadline`,
`TestAdoptingRestartBeforeReconcileAfterRetriesWithoutMutation`,
`TestAdoptingRestartAfterDeadlineRequiresDefiniteAbsentReceipt`, and
`TestAdoptingAmbiguousAuthorityAfterDeadlineRetriesWithoutMutation`. Task 7's
hosted adapter adds `TestHostedCredentialAdapterAuthenticatesReconcileAfter` and
must AEAD-authenticate and persist the field across reopen/restart.

Consumes P1.3 `VerifiedIdentity`, challenge reservation and migration 000005 exactly as frozen: Task 4 may neither rewrite that migration nor change the `[5,5]` manifest/privilege snapshot. Its repository may call only the exact key-versioned identity/device/access/refresh lookup, stage and notification-reservation signatures frozen above; stage bytes stay `[]byte`, never decode/re-encode through `jsonb`, and no generic SQL or projector pool is reachable from auth/identity/HTTP composition. `auth.New(Options) (*Service,error)` accepts `Identity *identity.Service`, `Verifier identity.IdentityVerifier`, `Security *security.Service`, runtime-only `Repository *Repository`, request-only `Apple identity.AppleCodeExchanger`, deep `Credentials identity.CredentialRotator`, `Mode config.Mode`, `Origin,InstanceID string`, `TokenKeys,DeviceKeys,NotificationKeys identity.DigestKeyring`, a dedicated permanent `ActorIDKey [32]byte`, clock and TTLs. It accepts no vault, Apple revoker or other raw pepper. No option contains a projector pool. The top-level authority projector remains outside HTTP-reachable modules. Self-host mode requires neither Apple exchanger nor credential rotator; hosted mode requires both. Define the deterministic real-PostgreSQL `sessionFixture` and restart/reauth/cross-account helpers described below.

**Frozen Task 4 deep-module contracts:**

- Do not begin until Task 3 has an independent READY verdict. Record its accepted commit as `BT_TASK3_ACCEPTED_REF`; every Task 4 gate executes `rtk git diff --quiet "$BT_TASK3_ACCEPTED_REF" -- Server/migrations/000005_identity_sessions.sql Server/internal/migrate/compatibility.json` and requires exit 0. Merely printing an empty-looking diff is not a gate. `TestTask4ImmutabilityGateCleanAndMutated` creates a temporary Git repository with those paths, proves the exact command exits 0 at the accepted bytes, mutates each protected file independently, and proves nonzero exit in both cases. Task 4 creates no migration, consumes no 000006 object and changes no schema-5 privilege/digest byte.
- `security.Service` is the deep authority module. `WithAuthoritySnapshot` holds one shared fence while it checks quarantine, reconstructs state/head and reads the durable epoch; its callback may authenticate and use the typed pure `Prepare*` plus `CommandHash` methods, but cannot stage or mutate. `ExecutePrepared` accepts **no pre-resolved principal**. It holds one exclusive fence across those checks, runtime lookup, exact-byte staging, `State.Decide`, append, a **single** head CAS and projection. Its callback receives an immutable cloned snapshot and returns a `PreparedCommand` whose actor/principal, command and provenance marker are unexported. There is no exported generic prepared-command constructor.

  Every authority callback creates a fresh unexported snapshot token embedded in its `AuthoritySnapshot`. `ResolveIdentityActor` and `ResolveSessionActor` return exported capability values whose fields and token are private; the six exported `AuthoritySnapshot.Prepare*` methods above are the only auth construction interface and reject zero capabilities or a capability minted by another snapshot/attempt. `CommandHash` accepts only a `PreparedCommand` minted by that same snapshot token and returns its canonical hash without exposing actor/command internals; zero/foreign prepared values fail. All constructors require canonical nonzero command/actor/payload UUIDs, finite UTC command time, payload version `v1`, payload account equal to the resolved actor, exact snapshot epoch/revision, payload IDs mutually consistent, sorted/deduplicated bounded candidate keys, valid status/digest/deadline fields and affected IDs derived internally rather than caller supplied. `PrepareIdentitySessionIssued` accepts resolution `absent` only when proposed ID/generation equal the deterministic actor candidate and `NextActorGeneration`, or `active` for reauthentication; it rejects deleted/unavailable authority. `PrepareIdentityRestored` accepts the same `IdentitySessionCommandInput`, requires the exact unavailable account/key/generation, and atomically prepares one `identity_session_issued` command/effect with restored authority status plus its device/session material; it emits neither a standalone `identity_restored` command/effect nor `account_registered`. `PrepareSessionRotation` requires the exact active parent session/device/account and a new child in the same lineage. `PrepareRefreshReuseRevocation` requires that same actor lineage and emits only its set-wise lineage revocation. `PrepareDeviceRevocation` requires the authenticated device and emits only device-binding revocation/logout. `PrepareIdentityUnavailable` resolves exactly one active account from candidates and never accepts a sessionless actor. Sessionless verified actors have account scope, snapshot epoch/revision and empty device/session IDs and can be produced only by snapshot-local identity resolution; restore additionally requires an existing unavailable actor. `State.Decide` checks the private provenance and exact constructor kind, so a zero-value `PreparedCommand`, copied generic `Command`, mismatched payload/actor, foreign-snapshot capability or cross-constructor kind fails closed. Add `TestPreparedConstructorsValidateEveryInvariant`, `TestPreparedCommandHashRequiresSameSnapshotProvenance`, `TestPrepareIdentityRestoredAtomicallyIssuesSessionWithoutRegistration`, `TestPreparedCommandZeroValueRejected`, `TestGenericCommandCannotForgeAuthProvenance`, `TestForeignSnapshotActorCapabilityRejected`, `TestPreparedKindCannotCrossConstructor`, `TestExecutePreparedResolvesPrincipalInsideFence`, `TestSessionlessVerifiedActorIsNarrow`, `TestExecutePreparedHoldsFenceAcrossLookupStageAndCommit`, `TestExecutePreparedRejectsRevisionDrift`, `TestExecutePreparedCASLossDoesNotRestage`, and `TestWithAuthoritySnapshotBlocksEpochAndRevocationInterleaving`.

  The callback must stage material for exactly `snapshot.Head.Revision+1` and has no ledger/head/projector handle. A false CAS or changed head is `security_busy`; the implementation never retries a prepared command at another revision. Receipt replay is allowed only when the prepared command ID and canonical hash equal the stored receipt. Authentication uses only the read seam; exchange, refresh rotation/reuse and logout use only the prepared write seam. No Apple/provider HTTP, `Resolve`, inventory, plaintext token open/decrypt/revoke, envelope decode/encode/seal, or general vault/object-store operation occurs while a shared or exclusive authority fence is held. Before acquiring it, `PreparePromotionCAS` or `PrepareDeletionCAS` resolves and authenticates the exact current object, validates binding/ref/operation/phase/version, and creates an opaque authenticated intent of at most 64 KiB containing expected object/provider version plus the already sealed complete target envelope. `PrepareDeletionCAS` additionally requires a typed `DeletionOperation` with canonical nonzero UUID `OperationID` and `CommandID`, stores both in that target deleting envelope, and rejects blanks, equality to stale prior rotation IDs, or reuse by a different cleanup/job claim. The intent has a single operation kind, canonical object/ref/operation/marker fields, an absolute expiry no later than preparation time plus five seconds, and an adapter MAC over every byte; it contains no plaintext token and cannot be retargeted. The only permitted vault call under the exclusive fence is consumption of exactly one unexpired intent by a single provider `CompareAndSwap`, with no read fallback or timeout reconciliation until after fence release. Tampered, wrong-kind, expired or replayed-success intents fail closed. `TestFencedCredentialCASNeverCallsResolveOpenDecryptOrSeal` injects panicking methods and proves both paths use only that CAS; add `TestCredentialCASIntentBoundsExpiryKindAndAuthentication`, `TestDeletionCASIntentRejectsBlankOrStaleRotationOperationIDs`, and `TestCredentialCASIntentCannotBeRetargetedOrReplayed`.

  Successful-but-ambiguous CAS return and process death immediately after provider success are recovered only outside every authority fence. `RecoverAdoptionCapability` and `RecoverDeletionCapability` perform one `Resolve`, decrypt/authenticate the current envelope, require respectively exact phase `adopting` or `deleting`, and validate the recovery request's canonical binding/ref, operation ID, command ID, the adopting envelope's exact expected canonical authority command hash, and (for deletion) marker sequence/digest. They also require a nonempty resolved provider version, verify it belongs to the same authenticated object/header sequence, and mint a new opaque capability bound to that exact current provider version/state. Recovery is read-only: it performs no CAS, transition, token open or provider HTTP. Wrong phase/version lineage, binding/ref, operation/command/hash, marker or ambiguous resolve fails closed without a capability. Successful intent replay remains a stale-CAS failure and is never used as recovery. `TestRecoverAdoptionCapabilityAfterCASReturnLoss`, `TestRecoverDeletionCapabilityAfterCASReturnLoss`, `TestRecoverCapabilitiesAfterProcessCrashBeforeReturn`, `TestRecoverCapabilityRejectsWrongPhaseVersionBindingOperationCommandHashOrMarker`, `TestRecoverCapabilityAmbiguousResolveFailsClosed`, and `TestCapabilityRecoveryNeverRunsUnderAuthorityFence` freeze this contract.

  Before `PreparePromotionCAS`, auth uses `WithAuthoritySnapshot` to resolve the actor and call the exact typed `PrepareIdentitySessionIssued` or `PrepareIdentityRestored` with the frozen stage inputs, then calls snapshot-local `CommandHash` on that preliminary prepared value. After the shared fence is released, that hash is passed explicitly to `PreparePromotionCAS`, returned in `CredentialPromotion`, and sealed inside the target adopting envelope. Final `ExecutePrepared` re-resolves/re-prepares the same typed command, recomputes `CommandHash`, and requires equality with `CredentialPromotion.ExpectedCommandHash` plus identical head generation/revision/hash and target revision before consuming the intent; drift performs no CAS and reprepares command/stage/hash/intent outside the fence. `CredentialPromotion.ConfirmIntent` is therefore produced with both promoted state and its exact expected authority hash. `ConfirmPromotion` consumes it inside the callback, CASes the exact `promoted` provider version to presealed terminal-inflight `adopting`, and returns an opaque `AdoptionCapability`; only after it succeeds may the callback stage the already frozen bytes and return the matching prepared command. If cleanup marked `deleting` first, confirmation loses stale; if confirmation marked `adopting` first, cleanup rejects it. After authority commit and fence release, `FinishAdoption` moves that exact capability to `ready`. A restart needs no retained in-memory intent or capability: inventory identifies adopting, `Resume` calls outside-fence recovery and returns the sealed `ExpectedCommandHash` plus recovered capability, then the reconciler compares the exact authority receipt/hash. A definite missing receipt after CAS/head failure or expiry moves the same adopting operation to `aborting` and resumes revocation, while an existing exact receipt finishes it. Ambiguous receipt/head state fails closed and retries without guessing. Add `TestPreliminaryAndFinalSnapshotCommandHashMatch`, `TestPreliminaryToFinalSnapshotDriftDoesNotConsumeIntent`, `TestAdoptingRestartRecoversExpectedCommandHashWithoutInMemoryIntent`, and receipt-present/absent/ambiguous restore tests.

  `DeleteCredentialIfUnreferenced` is the generic orphan-only exclusive-fence interface: its caller supplies one immutable typed `CredentialCleanupClaim` plus the prebuilt `Intent`. Security validates every claim field as nonempty/canonical, validates callbacks, and calls `ValidateClaim`, `Referenced` twice, `MarkDeleting`, and `Finish` with that exact value—never a lease-token-only callback or closure-captured convention. Each callback must revalidate the current durable lease token, opaque/monotonic operation version, operation ID, command ID, account/object/ref IDs and ref key; the intent's authenticated coordinates/operation/command must equal the claim. It performs the same recovery/quarantine checks, holds the identical global process/file write fence through the bounded local/PostgreSQL active-stage, exact-receipt, projected-reference **and deletion-job negative** checks, then consumes the intent with exactly one short `MarkDeleting` CAS before releasing the fence. It must never authorize an account-deletion job because that job is intentionally a durable reference. The CAS transforms the exact stable object/ref to terminal `deleting` and returns an opaque `DeletionCapability` bound to object ID, resulting provider version, deletion operation/command, marker sequence and marker digest. Rotation, promotion and adoption reject `deleting`. After fence release the adapter invokes only `CredentialVault.Delete(capability)`, which first proves the completed-or-abandoned rotation replay companion and then conditionally deletes the exact main provider version; a stale version or unconfirmed companion loses closed. It next calls `ConfirmAbsent` and idempotent `Finish(claim)`. A later authentication may recreate the stable object only after authenticated resolve plus `ConfirmAbsent` proves the deleting main object is gone; it may never overwrite or adopt a deleting state. Any error fails closed and leaves the external cleanup lease retryable. The adapter and lease remain outside security, and no state/head/ledger/projector capability escapes. Add `TestCredentialCleanupClaimRejectsBlankFields`, `TestCredentialCleanupClaimRejectsStaleLeaseOrOperationVersion`, `TestCredentialCleanupClaimRejectsOperationCommandOrBindingMismatch`, `TestCredentialCleanupCallbacksReceiveExactTypedClaim`, `TestPromotionProducedThenCleanupMarksDeletingBeforeAdopt`, `TestPromotionProducedThenAdoptBlocksCleanup`, `TestPromotionConfirmationStaleAfterCleanupMark`, `TestAdoptionCapabilityBindsPromotedVersion`, `TestAdoptingCrashWithReceiptFinishesReady`, `TestAdoptingCASOrHeadFailureMovesToAborting`, `TestAdoptingAmbiguousReceiptFailsClosedAndRetries`, `TestCredentialAdoptionAndCleanupBarrierAdoptionWins`, `TestCredentialAdoptionAndCleanupBarrierDeletionWins`, `TestCredentialCleanupMarksDeletingBeforeFenceRelease`, `TestDeletionCapabilityBindsObjectVersionOperationCommandAndMarker`, `TestCredentialDeletionStaleProviderVersionLoses`, `TestCredentialDeletionRequiresDurableReplayCompanion`, `TestCredentialAdoptionRejectsDeleting`, `TestCredentialRecreateRequiresConfirmedAbsence`, `TestCleanupFenceConsumesPrebuiltIntentWithCASOnly`, and `TestAppleAndTokenIOOccurOutsideAuthorityFence`; in either barrier scheduling exactly one path wins, never a committed ref to a deleted object. This replaces the earlier read-snapshot-then-`Execute` wording.

  Account deletion uses the distinct narrow `MarkCredentialDeletingForAccountDeletion`, never generic orphan cleanup. Its claimed job persists dedicated canonical UUID `OperationID` and `CommandID` independently of its opaque/monotonic CAS/lease `OperationVersion`; neither is inferred from `JobID`. `DeletionOperation` is exactly `{OperationID: claim.OperationID, CommandID: claim.CommandID}`, and `PrepareDeletionCAS` receives that pair. Under the same exclusive fence the seam validates the exact claimed durable deletion job and current unexpired lease token/operation version, and requires job ID, status, attempt, lease expiry, account ID, complete server-instance/purpose/object binding, ref ID/key ID and operation/command pair to equal both the durable job and prebuilt deletion intent. It permits only that matching job reference while still rejecting every active stage, exact committed/adopted authority reference, other job/reference or `adopting` object; it then consumes the intent with one CAS and returns the deletion capability. No job repository or raw vault escapes. Add `TestAccountDeletionFenceAcceptsOnlyExactClaimedJobReference`, `TestAccountDeletionFenceRejectsStaleLeaseOperationOrBinding`, `TestAccountDeletionClaimRejectsChangedServerInstancePurposeStatusAttemptOrExpiry`, `TestAccountDeletionCallbacksReceiveExactFullClaim`, `TestAccountDeletionFenceRejectsWrongOrBlankDeletionOperation`, `TestAccountDeletionJobIDCannotSubstituteForCommandID`, `TestAccountDeletionFenceRejectsActiveOrAdoptedReference`, `TestAccountDeletionAndAdoptionBarrierAdoptionWins`, and `TestAccountDeletionAndAdoptionBarrierDeletionWins`.
- **Task 6 cleanup correction:** the generic orphan path's terminal sequence is
  `ValidateClaim` with fresh time, `EraseDeletingTokens(capability)`,
  `Delete(capability)`, `ConfirmAbsent`, then `Finish(claim)`. Token erasure is
  capability-bound, crash-idempotent, performs no Apple/provider call, and is
  required because `Delete` fails closed while a deleting envelope still holds
  token slots. This correction supersedes the earlier sentence saying that the
  adapter invokes only `Delete` after fence release; it does not widen the
  authority-fenced callback or permit token material inside that fence.
- `State.ReferencedCredentialKeyIDs` returns sorted, deduplicated copies: every identity key ID retained by an account/tombstone, and every device-continuity key ID referenced by an active binding. Startup compares those sets with configured accepted keys while holding `WithAuthoritySnapshot`; a missing identity or active-device key fails readiness with `credential_key_unavailable`. Session and notification retirement remains time-window based. Add `TestReferencedCredentialKeyInventoryIsSortedAndImmutable` and `TestContinuityKeyCannotBeDroppedWhileReferenced`.
- Configuration owns neutral `DigestKeyConfig{ID string; Secret [32]byte}` and `DigestKeyringConfig{Current DigestKeyConfig; Previous []DigestKeyConfig}`; `config` never imports `identity`. Main maps them to `identity.DigestKeyring`. The four rotating prefixes are exactly `BT_IDENTITY`, `BT_SESSION_TOKEN`, `BT_DEVICE_CONTINUITY`, and `BT_APPLE_NOTIFICATION`. Each current key uses `<PREFIX>_KEY_ID` plus `<PREFIX>_PEPPER_FILE` (development alone may use `<PREFIX>_PEPPER`). Each optional `<PREFIX>_PREVIOUS_KEYS_FILE` is a strict, duplicate-safe JSON array of at most seven `{"key_id":"...","pepper_file":"/absolute/path"}` objects, current-excluded and in newest-to-oldest read order; the manifest is at most 16 KiB and contains paths, never secrets. In addition, `BT_ACTOR_ID_PEPPER_FILE` (development only: `BT_ACTOR_ID_PEPPER`) is one permanent, non-rotating 32-byte instance-scoped secret with no key ID or previous-key manifest. It is backup-critical: losing or changing it makes deterministic new-actor allocation fail readiness rather than silently create different actors. Secret-file rules stay the strict Task 3 rules. Across all four keyrings, actor-ID pepper and rate-limit pepper, reject duplicate secrets; within a ring reject duplicate IDs, and operators never reuse one key ID for different bytes. New identity/device/session/notification digest writes use current. An existing identity or device matched through a retained key preserves its authoritative stored digest rather than attempting an impossible second `(account,provider)` or device binding; its old key remains required until a later explicit migration/revocation. Reads compute at most eight candidates current-first, call the frozen single-candidate lookup once for **every** candidate without early success, then reconcile exactly zero or one total match and fail closed on multiple matches. `BT_SESSION_SIGNING_KEY_FILE` is only a deprecated alias for `BT_SESSION_TOKEN_PEPPER_FILE`; setting both fails and no JWT signing use remains. Add `TestFourDigestKeyringsLoadWithoutImportCycle`, `TestActorIDPepperIsPermanentAndDistinct`, `TestDigestKeyringsRejectDuplicateIDsAndSecrets`, `TestLegacySessionSigningAlias`, and `TestDigestKeyringsWriteCurrentReadPrevious`.
- Actor-pepper replacement detection is persisted outside PostgreSQL and without changing migration 000005 or the strict existing `identity.json` format. Under the global exclusive fence, `security init` creates head-directory sidecar `actor-id-key.json` as canonical one-line JSON `{"version":"v1","instance_id":"<uuid>","commitment":"<64 lowercase hex>"}` using temp-file `O_EXCL` mode `0600`, file fsync, atomic no-replace publication and directory fsync. Commitment is `HMAC-SHA256(actorPepper, frame("baby-tracker-actor-id-commitment-v1",instanceID))`, where both fields use the unsigned-32-bit length frame; known answer for pepper hex `000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f` and instance `aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa` is `1e288d423b480e3c0006a5488a0bd9861c076921a22ba1290771a54076381c4d`. New-server `security init` requires the actor pepper and creates authority/head/recovery identity/commitment together under one fence. For an accepted older empty authority only, explicit `security bind-actor-key --instance-id ... --confirm-instance ...` may create the missing sidecar after replay proves no live, unavailable or deleted identity authority and the trusted head remains unchanged; automatic startup never binds it. Readiness requires the file, exact instance, canonical bytes/mode and constant-time commitment equality before auth construction. Missing, mismatch, copied cross-instance file, corrupt/partial file or a changed pepper returns `actor_id_key_mismatch` and leaves readiness false. Restore retains this sidecar with the independent head volume; a database-only restore cannot replace it. Add `TestActorIDCommitmentKnownAnswer`, `TestSecurityInitPersistsActorCommitment`, `TestActorPepperReplacementFailsReadiness`, `TestActorCommitmentCrossInstanceFails`, `TestActorCommitmentRestoreRoundTrip`, `TestBindActorKeyOnlyOnEmptyAuthority`, and `TestOlderBinaryIgnoresSidecarButCannotRewriteIt`.
- Task 4 Compose adds secret `actor_id_pepper`, mounts it read-only at `/run/secrets/actor_id_pepper`, and sets `BT_ACTOR_ID_PEPPER_FILE=/run/secrets/actor_id_pepper`; `security init` receives that same file through the explicit init container. `smoke.sh` creates a strict test value, proves init/readiness, restarts with the same value, then substitutes a distinct valid value and requires readiness failure `actor_id_key_mismatch` before restoring the original. Config/source tests require the secret, environment, init argument, backup documentation and smoke mutation path to remain in sync. Add `TestComposeWiresActorIDPepper` and `TestSmokeRejectsActorIDPepperReplacement`.
- Identity owns profile validation. `VerifyExchangeWithProfile` performs the challenge burn/proof/identity lookup once and returns a defensive normalized profile (`strings.TrimSpace`, confirmed, valid UTF-8, at most 80 Unicode scalars/320 bytes, no control characters) or nil. Existing `VerifyExchange` delegates to it and returns `.Identity` for Task 3 compatibility. `AuthorityKeyCandidates` recomputes every retained identity-key candidate from the already verified provider/audience/subject, returns defensive current-first copies, and never consults SQL. Auth passes all candidates into the exclusive authority callback; it must not trust the one SQL-selected `VerifiedIdentity.IdentityKey`. This lets a restored authority chain find an older-key actor when the SQL identity projection is missing, while multiple authoritative matches fail closed. Add `TestAuthorityKeyCandidatesAreCurrentFirstAndDefensive` and `TestRestoreResolvesPreviousKeyFromAuthority`. Auth never reimplements normalization. Identity failures are extracted with `identity.Code`, auth session/device failures with `auth.Code`, and authority failures with `security.Code`; none exposes an arbitrary public code constructor. The HTTP adapter uses one exhaustive fixed mapping and unknown errors become `service_unavailable`.
- A proposed new account ID is also deterministic before any SQL/stage write. Using `BT_ACTOR_ID_PEPPER_FILE` bytes as the HMAC-SHA256 key, frame domain `baby-tracker-actor-id-v1`, canonical instance UUID ASCII, verified provider/audience/subject UTF-8, and lifecycle generation as eight unsigned big-endian bytes with the same length-prefixed framing used by refresh; convert the first 16 output bytes to RFC 9562 UUIDv8. `State.ResolveIdentityAuthority` examines every current-first candidate without early exit and returns exactly one snapshot-local result: no match is `absent` with `LifecycleGeneration=0,NextActorGeneration=1`; deleted-only history requires unique strictly increasing generations, returns the greatest tombstone generation and `NextActorGeneration=current+1`; exactly one `active` or `identity_unavailable` actor plus zero or more older deleted tombstones is valid only when every generation is unique/strictly monotonic and the nondeleted actor has the greatest generation, and returns that actor/account/key/status with no allocation. Multiple nondeleted actors, a tombstone at or above the live generation, duplicate or otherwise inconsistent generations, or safe-integer overflow fails closed. Task 4 permits registration only from `absent`; Task 6/000007 enables deleted-to-new-actor registration with exactly `NextActorGeneration` and records that generation in authority state, so repeated delete/re-register cycles advance 1,2,3 without reusing an actor. Command ID, challenge, current identity key and wall clock are excluded, so challenge retry, restart, seal-before-stage recovery and restored missing SQL all propose the same actor/object. Known answer: key hex `000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f`, instance `aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa`, provider `apple`, audience `com.example`, subject `000123`, generation `1` yields `430e9857-3700-817e-9758-e28975ee7fab`. Add `TestIdentityAuthorityResolutionLiveWithOlderDeletedHistory`, `TestIdentityAuthorityResolutionUnavailableWithOlderDeletedHistory`, `TestIdentityAuthorityResolutionDeletedOnlyReturnsMaxPlusOne`, `TestIdentityAuthorityResolutionRejectsMultipleNondeletedOrInconsistentGenerations`, `TestActorIDKnownAnswer`, `TestActorIDStableAcrossChallengeRestartAndPrecommitCrash`, `TestRepeatedDeleteReregisterAdvancesActorGeneration`, and `TestRestoredAuthorityPreservesNextActorGeneration`.
- Initial exchange tokens remain 32-byte CSPRNG values. Refresh derivation uses the **matched parent refresh-token key bytes** as the HMAC-SHA256 key. Define `frame(fields...)` as, for every field including the domain, an unsigned 32-bit big-endian length followed by the exact field bytes. Rotation fields after the domain, in order, are canonical server-instance, account, device, parent-session and lineage-root UUID ASCII; recovery epoch as exactly eight unsigned big-endian bytes; matched parent key-ID ASCII; raw 32-byte parent digest; current output key-ID ASCII; canonical scope ASCII; canonical family UUID ASCII or the empty field for account scope; access, refresh, reauthentication and absolute deadlines as the exact UTC `time.RFC3339Nano` strings stored in the stage; head generation and head revision as eight unsigned big-endian bytes each; raw 32-byte head hash; and target revision (`head revision+1`) as eight unsigned big-endian bytes. Apply that identical context under the distinct domains `baby-tracker-refresh-command-v2`, `baby-tracker-refresh-session-v2`, `baby-tracker-refresh-access-v2`, and `baby-tracker-refresh-token-v2`. Command and successor-session IDs are RFC 9562 UUIDv8 from the first 16 output bytes; each token is unpadded base64url of all 32 output bytes. The callback chooses the four deadlines once, before deriving any output. Every nondeterministic stage input is therefore bound: identical context recreates byte-identical stage material, while a changed head, target revision, deadline, scope or family produces a new command, successor session and token pair. After false CAS discard the raw bundle, enter a fresh `ExecutePrepared`, recompute from the new snapshot/time, and leave the old uncommitted stage inert until its ten-minute expiry; never delete or overwrite it. Reuse frames the same server/account/device/parent/lineage/epoch/parent-key-ID/parent-digest fields under `baby-tracker-refresh-reuse-v1`, but excludes output key, time, scope/family and every head field, so its lineage-revocation command remains stable across revisions and output-key rotation. A committed/consumed parent always enters reuse; it never attempts the rotation derivation again. Every retry executes or observes that reuse command and returns `refresh_reused`; no successor secret is replayed.

  Three mandatory known-answer behaviors use HMAC key hex `000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f`, server/account/device UUIDs `aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa`, `bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb`, `cccccccc-cccc-4ccc-8ccc-cccccccccccc`, parent `11111111-2222-4333-8444-555555555555`, lineage `dddddddd-dddd-4ddd-8ddd-dddddddddddd`, epoch `7`, parent key ID `k1`, parent digest hex `202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f`, output key ID `k2`, account scope/empty family, and deadlines `2026-09-20T12:10:00Z`, `2026-10-20T12:00:00Z`, `2026-10-20T12:00:00Z`, `2026-10-20T12:00:00Z`. (1) Generation `1`, revision `41`, head hash hex `404142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f`, target `42` yields command `322e17f4-5601-83a1-9345-a98b6c832e64`, successor session `fc846652-35f0-8428-8faa-206b9a3290e2`, access token `lwxi1Y3-hKKbo6o6fqlTQQad6Wn-GhoRKWfJ1mxtNnI`, and refresh token `gELeWd0inDHrWNCVX24uinf8EWyMuX_zojxgXrql4rc`. (2) Keeping those deadlines but drifting to revision `42`, head hash hex `4142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f60`, target `43` yields the different command `66a9b93e-3d6e-8022-b4af-7b5e7f05e4b6`; a separate assertion changes one deadline and also requires every rotation output to change. (3) Reuse yields the head/time/output-key-independent command `1d06b98d-47ae-8680-ac49-d4fab466491a` under all three contexts. Add `TestRefreshRotationKnownAnswer`, `TestRefreshCASLossKnownAnswer`, `TestRefreshDeadlineChangeKnownAnswer`, `TestRefreshReuseKnownAnswer`, `TestRefreshRetryBeforeCommitRecreatesExactStage`, `TestRefreshCASLossRepreparesAtNewRevision`, `TestRefreshLostResponseRevokesLineage`, `TestRefreshReuseCommandIsDeterministic`, and `TestRefreshRotationAcrossCurrentKeyChange`.
- `CredentialBinding` canonical AAD is ASCII `baby-tracker-credential-binding-v1` followed by four fields in order `AccountID`, `ServerInstanceID`, `Purpose`, `ObjectID`, each encoded as unsigned 32-bit big-endian byte length then UTF-8 bytes. NUL, noncanonical UUIDs and unknown purpose are rejected. Task 4 permits only purpose `apple-refresh-revocation-v1`; `ObjectID` and `CredentialRef.ID` are the stable UUIDv8 derived from the first 16 bytes of `SHA-256("baby-tracker-apple-refresh-object-v1\x00" || serverInstanceID || "\x00" || accountID || "\x00" || purpose)`, so one authoritative account has exactly one main credential object/ref for that purpose on a server. That main object is one AEAD-authenticated CAS document containing binding, ref, state/version/operation metadata and at most two refresh-token slots. P1.7 additionally persists at most one replay-marker companion object for that binding; it is not a `CredentialRef` and never contains token material. No plaintext or provider error appears in either object's metadata, SQL, authority, logs or safe errors.

  The replay-marker object ID is a separate stable UUIDv8 derived from the first 16 bytes of `SHA-256("baby-tracker-credential-replay-object-v1\x00" || serverInstanceID || "\x00" || accountID || "\x00" || purpose)`. For instance `aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa`, account `bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb`, and purpose `apple-refresh-revocation-v1`, the replay-marker ID is `0e1049b8-6485-88ff-812c-d29f8f972c99` and differs from the main credential object ID. It uses the same outer `credential-envelope-v1` header, key selection and exact envelope AAD as the main object. Its canonical authenticated inner document has kind exactly `credential-replay-marker-v1`, the exact main `CredentialBinding`, the last **completed or abandoned** rotation's canonical nonzero operation ID and command ID, and a nonzero sequence in `[1,9007199254740991]`; it has no ref, phase, provider error, token slot or token-derived bytes. The main credential privately retains that same last-rotation pair while it exists, even though `ready` clears the active state's operation/command. The outer object ID must derive from the embedded binding under the replay domain, and the authenticated inner sequence must equal the outer `EnvelopeSequence`. Any kind, binding, object-ID, header, AAD or sequence mismatch fails closed.

  Its main-object state machine is exact: `absent` means no main credential object, whether or not the replay-marker companion exists; `ready` has exactly current and no pending; `reserved` records one operation/command before Apple I/O and retains optional current; `candidate` adds exactly one returned pending token; `retiring` records that prior revocation is in progress; `promoted` has made pending current, erased prior, and retains operation/command until adoption; `adopting` is the terminal-inflight promotion confirmation that excludes cleanup while the exact authority command commits and alone has nonzero `ExpectedCommandHash`; `aborting` retains only material that must be revoked/erased after a definite noncommit; `deleting` is a terminal authenticated marker that freezes both slots, rejects rotation/promotion/adoption and alone has nonzero `MarkerSequence`/`MarkerDigest`. Every transition is expected-version CAS; post-timeout reconciliation decrypts the object and accepts only the exact expected operation/state. A different operation while non-`ready` returns `credential_rotation_busy` **before** Apple exchange. `StoreCandidate` is idempotent only for the same operation and plaintext digest; changed material conflicts. `MarkPriorRevoked`, `PromotePending`, `ConfirmPromotion`, `FinishAdoption`, `AbandonAdoption`, `MarkCommitted`, `MarkAbandoned` and maintenance transitions are monotonic/idempotent for the same operation/version. Successful `FinishAdoption`/`MarkCommitted` and every abandonment copy the finished rotation's operation/command into the main object's private last-history fields before clearing or changing the active state. If `MarkDeleting` is allowed to freeze any in-progress `reserved|candidate|retiring|promoted|aborting` rotation, its presealed target first copies that now-abandoned active rotation operation/command into private last history and only then installs the distinct deletion operation/command in the public deleting state; this includes an initial rotation with no prior history. `PrepareDeletionCAS` accepts the exact authenticated current state/version but rejects `adopting` and prebuilds that complete next-sequence deleting envelope; fenced `MarkDeleting` only consumes the intent with one CAS and returns an authenticated opaque `DeletionCapability` binding object ID, resulting provider version, marker sequence and marker digest. `Delete` uses that expected version, so any intervening provider version loses closed. `ConfirmAbsent` checks only absence of the main credential object and deliberately ignores the replay-marker companion before that same stable main ID can be conditionally recreated. No state can contain more than current+pending.

  **Every path that deletes a main credential object has the same marker-first invariant.** Before `CompleteMaintenance` removes a no-current `aborting` object, and before capability-authorized `Delete` removes a `deleting` object for account or orphan cleanup, the adapter decrypts/authenticates the exact expected main object and reads its private last completed-or-abandoned rotation pair. It then resolves and authenticates the deterministic companion: create sequence 1 when missing; accept it when binding, pair and sequence already match; or CAS-repair a stale marker to the exact main history at the next bounded sequence. A lost create/CAS response is reconciled by resolving, decrypting and requiring the exact binding, pair and expected sequence; provider conflict retries from authenticated current state and sequence overflow fails closed. Only after the companion is proven durable may the adapter conditionally delete the still-exact main provider version. Missing/corrupt/unrepairable marker, ambiguous marker state or changed main state forbids main deletion. A crash after marker proof but before main deletion leaves the main object retryable and the companion idempotently reusable; a main-delete timeout resolves the main object and accepts only confirmed absence or the exact unchanged deletable version for retry. The companion always survives ordinary main deletion.

  When no main object exists, `ReserveRotation` resolves and authenticates the deterministic marker **before** conditional creation, rejects reuse of either its last operation ID or last command ID before Apple exchange, and seeds only those two IDs into the recreated main object's private last-history fields so stale deletion coordinates remain rejected after reclaim. Replay-marker sequence is adapter-private companion-envelope bookkeeping and never enters public `CredentialRotationState`: a reclaimed `reserved` state, like every non-`deleting` phase, must keep deletion `MarkerSequence == 0` and `MarkerDigest == [32]byte{}`. A fresh operation/command pair may recreate the main object while the one marker is retained and later repaired by CAS; no second marker object is created. `Resolve` and ordinary `ConfirmAbsent` retain their main-object-only domain result, and the normal maintenance reconciler must not loop on a marker-only binding. Only the final account-generation purge seam may remove the companion. Add `TestReclaimedReservedStateKeepsDeletionMarkerFieldsZero` and assert the same invariant after restart and every non-deleting transition.

  For each new hosted session command, `RotateForSession` first `ReserveRotation`, then invokes its exchange callback exactly once. The callback must exchange the supplied authorization code and reverify the returned identity token/nonce/provider/subject/audience before returning tokens. It immediately `StoreCandidate`; if current exists it opens and revokes that prior token, durably marks prior revoked, then promotes pending; without current it promotes pending directly. Its initial promoted result has zero hash/intent and never authorizes staging. Auth freezes the provisional typed command/stage and canonical hash, then still outside the fence calls `CredentialRotator.PreparePromotionCAS` with the promoted result and hash; only its returned `CredentialPromotion` has matching nonzero `ExpectedCommandHash` and `ConfirmIntent`. Inside the final exclusive `ExecutePrepared` callback, after exact snapshot/actor/head revalidation, auth calls `ConfirmPromotion` with that promotion; the method only consumes its intent and only returned `AdoptionCapability` permits the already frozen matching stage and authority commit. If the CAS return is ambiguous, the callback stages nothing and releases the fence; `CredentialRotator.Resume` resolves outside the fence and returns `CredentialResume{State: adopting,ExpectedCommandHash: hash,Adoption: recoveredCapability}` only after exact recovery, never by replaying the intent. Exact retry of the same command resumes its recorded operation without exchanging the already consumed code again; it is the same sign-in attempt, not a second accepted authorization. After authority receipt success and fence release auth calls `FinishAdoption`, producing `ready`. A definite failure before promotion enters `aborting` and revokes/erases pending; CAS/head failure after confirmation calls `AbandonAdoption`, while ambiguous completion leaves `adopting` for receipt-based reconciliation. Session authority is never committed while the credential is `reserved`, `candidate`, `retiring`, `promoted`, `aborting` or `deleting`.

  The unavoidable external residual is explicit: process death after Apple returned a refresh token but before the first successful/ambiguously reconciled `StoreCandidate` CAS can leave one provider token outside local durable state. Tests and readiness must not claim exactly-once provider retention across that interval. All later crash points are resumable. Add `TestCredentialBindingCanonicalAAD`, `TestCredentialObjectIDKnownAnswer`, `TestCredentialReplayObjectIDKnownAnswerAndDistinct`, `TestSequentialHundredHostedSignInsExchangeHundredCodesOneMainObjectMaxTwoTokens`, `TestConcurrentHostedSignInsOnlyOneReservesAndExchanges`, `TestCredentialRotationRejectsSecondPending`, `TestCredentialRotationCrashAfterReserve`, `TestCredentialRotationCrashAfterCandidate`, `TestCredentialRotationCrashAfterPriorRevoke`, `TestCredentialRotationCrashAfterPromotionCASBeforeCapabilityReturn`, `TestCredentialRotationCrashAfterPromoteBeforeAuthority`, `TestCredentialRotationCrashAfterAuthorityBeforeCommitMark`, `TestCredentialRotationAmbiguousCASRecoversExactCapability`, `TestRestoreResumesCredentialRotation`, `TestCredentialReplayMarkerCrashBeforeCredentialDeleteResumes`, `TestCredentialReplayMarkerSurvivesAdapterRestart`, `TestCredentialReplayMarkerRejectsExactAndPartialReplayBeforeExchange`, `TestCredentialReplayMarkerStaleDeletionAfterReclaim`, `TestMarkCommittedReadyDeletionPersistsCompletedReplayMarker`, `TestMarkDeletingInitialRotationPreservesAbandonedReplayHistory`, `TestCompleteMaintenanceDeletionPersistsAbandonedReplayMarker`, `TestEveryCredentialDeleteRepairsMissingOrStaleReplayMarker`, `TestCredentialDeleteTimeoutAtMarkerResolveCreateCASAndMainDelete`, `TestCredentialReplayMarkerSurvivesOrdinaryDelete`, `TestCredentialDeletionRevokesBothSlots`, `TestDeletionCapabilityRejectsWrongObjectVersionOrMarkerSequence`, and `TestCredentialBindingMismatchFailsClosed`. P1.4 owns the vault/inventory method sets, main state machine, deterministic fake and crash matrix; its fake retains completed-or-abandoned history and companion marker in its durable fake store, but the marker is never exposed as a second credential. Task 6 owns the additive `CredentialRotationState.ReconcileAfter` field amendment and updates that fake/conformance suite before using the frozen deadline to resume incomplete maintenance/receipt states. P1.7 implements both encrypted CAS object kinds in `internal/identity/hostedcredentials` without moving or otherwise widening the P1.4 vault/inventory method sets.
- Apple notification input is a strict JSON object `{"payload":"<compact-JWS>"}` capped at 64 KiB. The signed payload is a strict duplicate-safe object containing exactly `iss`, `aud`, integral `iat`, `jti`, and object-valued `events`; `exp` is neither required nor accepted. `jti` and `events.sub` are 1..255 valid UTF-8 bytes with no control/NUL character. `events` contains exactly integral `event_time`, `sub`, and `type`, plus optional `email` (1..320 valid UTF-8 bytes) and string `is_private_email` exactly `"true"` or `"false"` only for `email-enabled`/`email-disabled`. Destructive types are exactly `consent-revoked` and `account-deleted`; email types validate then return 204 without persistence, and any other type fails closed. Require `iss=https://appleid.apple.com`, string `aud` equal configured audience, RS256 header with only `alg`,`kid`, a valid Apple JWKS key, and both times in `[now-24h,now+60s]`; authenticate the original compact-JWS bytes and never log claims. For notification key `K`, define `P = "baby-tracker-apple-notification-v1\x00" || iss || "\x00" || aud || "\x00" || jti`, digest `HMAC-SHA256(K,P)`, and command UUIDv8 from the first 16 digest bytes with RFC 9562 version/variant bits. The stored digest retains `K.ID`. Add known-answer, object-not-string `events`, duplicate/unknown field, no-`exp`, wrong-alg, length/age, receipt-before-SQL, multi-key rotation/restore and changed-event-same-JTI/command-hash tests.
- The hosted Apple transport has fixed endpoints `https://appleid.apple.com/auth/token` and `https://appleid.apple.com/auth/revoke`, cloned injected HTTP transport, redirects disabled, five-second whole-call deadline and 64 KiB response cap. Client-secret compact JWS uses the exact header members `alg=ES256`,`kid=<configured key ID>` and claims `iss=<team ID>`,`iat=<now Unix>`,`exp=<iat+300>`,`aud=https://appleid.apple.com`,`sub=<client ID>`; JSON fixtures fix field order, P-256/SHA-256 signatures are JOSE raw 32-byte `r` plus 32-byte `s`, and DER is rejected. Code exchange sends form fields `client_id`, `client_secret`, `code`, `grant_type=authorization_code` and optional configured `redirect_uri`; a 200 response requires `id_token`, `refresh_token` and `access_token` each 1..16384 bytes, `token_type=Bearer`, and integral `expires_in` in `1..86400`, rejects duplicate/trailing/invalid known fields and tolerates additive unknown response fields. Revocation sends exactly `client_id`, `client_secret`, `token=<refresh token>`, `token_type_hint=refresh_token`; HTTP 200 is complete even when the token was already invalid. Provider status/error text is never returned or logged; token-endpoint `invalid_grant` maps to `invalid_apple_authorization`, other 4xx to `invalid_apple_authorization`, and transport/5xx to retryable `apple_unavailable`. A swapped returned nonce/subject/audience is detected after the reservation but before `StoreCandidate`, token persistence, stage or ledger mutation; the reservation is marked `aborting`/abandoned and the reconciler finishes it without ever treating the unaccepted response tokens as a stored candidate. P1.7 supplies production credentials/vault/provider adapters; Task 4 uses injected test adapters and hosted startup remains `hosted_adapter_unavailable` until those adapters exist. Add `TestSwappedReturnedIdentityAbandonsReservationBeforeCandidateOrAuthorityMutation`.
- Task 4 seals the Apple refresh token and stages only `CredentialRef`; it does **not** create, enqueue or execute Apple revocation jobs. Actual account deletion, durable `apple_revocation_jobs` projection/maintenance functions, retry/backoff and Apple `/auth/revoke` execution belong to Task 6 migration 000007. Likewise, deleted-identity-to-new-actor rebinding and `TestDeletedIdentityCreatesNewActorButRejectsOldContinuity` belong to Task 6/000007 because immutable 000005 keeps the old `(provider,key_id,digest)` projection binding. Task 4 proves only `TestIdentityRestoreNeverRevivesOldSessions` and that unavailable identities cannot authenticate. These deferrals are not Task 4 omissions and may not be implemented through direct table DML.

The exact auth HTTP error matrix is:

| Stable code | HTTP | Retryable | Extra header / ownership |
| --- | ---: | --- | --- |
| `invalid_request`, `unsupported_capability`, `hosted_only_field`, `display_name_required`, `device_continuity_invalid` | 400 | false | identity/auth validation |
| `invalid_or_expired_challenge`, `invalid_apple_proof`, `invalid_apple_authorization`, `access_expired`, `session_revoked`, `refresh_reused`, `reauthentication_required`, `epoch_changed` | 401 | false | `WWW-Authenticate: Bearer` |
| `command_id_reused`, `credential_conflict` | 409 | false | auth/authority conflict |
| `rate_limited` | 429 | true | positive matching `Retry-After` and `retry_after_seconds` |
| `security_busy`, `credential_rotation_busy`, `authorization_quarantine`, `apple_unavailable`, `hosted_adapter_unavailable`, `credential_key_unavailable`, `service_unavailable` | 503 | true | `Retry-After: 1` for `security_busy` and `credential_rotation_busy`; provider details suppressed |

Every success and error from `/v1/auth/*` and the hosted notification handler has `Cache-Control: no-store`; 401 responses use the same generic safe message, invalid continuity never reveals which comparison failed, unknown errors map to the final 503 row, and self-host notification routing remains indistinguishable 404. Add `TestAuthHTTPErrorMatrix`, `TestCredentialRotationBusyReturns503WithRetryAfterOne`, `TestAuthErrorsNeverExposeProviderText`, and fixture/OpenAPI assertions for status, code, retryability and headers.

Task 4 marshals exchange and refresh stages through the exact fixed-order structs and null rules frozen in Task 3, hashes the exact `encoding/json` bytes, and supplies the same command ID/hash/material digest to staging and authority. Rotation sets `SessionPayload.ProjectionMaterialDigest` and the single `session_rotated` effect to that digest. Refresh reuse emits one `session_revoked` effect naming a lineage member; SQL revokes the full account/device/lineage-root set. Logout emits one `device_binding_revoked` effect; SQL revokes that binding and every device session. Apple identity loss emits one `identity_unavailable` effect; SQL revokes every account session while preserving device bindings. Never enumerate those implicit revocation sets in effects.

- [ ] **Step 1 (3 min): Write refresh-response-loss regression.**

```go
func TestRefreshLostResponseRevokesLineage(t *testing.T) {
    h:=newSessionFixture(t,config.ModeSelfHosted)
    original:=h.Issue()
    successor,err:=h.Service.Refresh(t.Context(),original.RefreshToken)
    if err!=nil { t.Fatal(err) }
    // Treat successor response as lost: client repeats the consumed credential.
    _,err=h.Service.Refresh(t.Context(),original.RefreshToken)
    if auth.Code(err)!="refresh_reused" { t.Fatalf("code=%s",auth.Code(err)) }
    _,err=h.Service.Authenticate(t.Context(),successor.AccessToken)
    if auth.Code(err)!="session_revoked" { t.Fatal("successor survived lineage revocation") }
}
```

- [ ] **Step 2 (2 min): Run RED.**

Run: `rtk proxy sh -c 'cd Server && go test ./internal/auth -run TestRefreshLostResponseRevokesLineage -count=1'`.

Expected: FAIL due to missing session service. The chosen race policy is deliberate: two simultaneous uses of a refresh secret yield at most one response, then reuse revokes its entire device lineage; clients serialize refresh and reauthenticate after lost response/reuse.

- [ ] **Step 3 (3 min): Implement random token and keyed-digest primitives.**

```go
func tokenDigest(key identity.DigestKey,class,token string) identity.KeyedDigest {
    m:=hmac.New(sha256.New,key.Secret[:])
    m.Write([]byte(class)); m.Write([]byte{0}); m.Write([]byte(token))
    var out [32]byte; copy(out[:],m.Sum(nil))
    return identity.KeyedDigest{KeyID:key.ID,Sum:out}
}
func newToken() (string,error) {
    b:=make([]byte,32)
    if _,err:=rand.Read(b); err!=nil { return "",err }
    return base64.RawURLEncoding.EncodeToString(b),nil
}
```

Use independent 32-byte mounted session-token, device-continuity and notification peppers, in addition to Task 3's separate identity/rate keys; parse 64 lowercase hex characters plus optional terminal newline and reject permissive/oversized/symlink/nonregular secret files. Preserve legacy `BT_SESSION_SIGNING_KEY_FILE` as a deprecated alias for `BT_SESSION_TOKEN_PEPPER_FILE` during overlap; both set is an error, and the value is a pepper, not a JWT signing key. No token appears in command/event hashes or SQL error output. Store only keyed digests, generated device/session UUID, expiry and lineage metadata.

- [ ] **Step 4 (4 min): Implement account/device/session grant with staged material.**

`Exchange` uses only the complete fixed-order exchange stage struct frozen in Task 3 Step 5. Do not introduce a partial `ExchangeMaterial` or alternate JSON shape. `Service.Exchange(context.Context,identity.ExchangeRequest) (SessionBundle,error)` is the sole service entry point.

`Exchange` consumes/verifies P1.3 proof, obtains every current-first `AuthorityKeyCandidates` value, then uses `WithAuthoritySnapshot` only to derive a provisional `IdentityAuthorityResolution`, deterministic account/generation/binding and continuity lookup inputs; it copies those values and releases the shared fence before any remote call. SQL identity lookup is projection/profile evidence only and cannot select authority. Hosted mode next reserves the stable credential operation by object CAS, exchanges and re-verifies every newly accepted Apple code, stores candidate, retires prior and reaches `promoted`, all outside the authority fence. An absent continuity claim provisionally allocates a **new** server device plus 32-byte unpadded-base64url continuity credential; a valid supplied claim provisionally selects the original device.

Only then does `ExecutePrepared` acquire the exclusive fence. Its fresh snapshot re-runs `ResolveIdentityActor`, validates that lifecycle status/generation/account and device authority plus head generation/revision/hash still equal the provisional values used by the frozen command/stage/hash, then invokes the one bounded `ConfirmPromotion` callback with `CredentialPromotion.ConfirmIntent`; that call performs only the exact nonexpired expected-version CAS from `promoted` to the presealed `adopting` value and never resolves/opens/seals. Only the returned `AdoptionCapability` permits staging those already frozen exact bytes and returning `PrepareIdentitySessionIssued` or `PrepareIdentityRestored` with the identical canonical hash. It stages the stable Apple `CredentialRef`, profile material and application token digests in `auth_exchange_material` for exactly the next revision and commits. If authority/head changed, device authority changed, or promotion confirmation lost to cleanup, no stage/authority commit occurs and after fence release auth calls `Abandon`/`AbandonAdoption` as required by its state; head drift before confirmation simply reprepares outside the fence. Grant projection consumes staged material, creates profile/identity/session rows and updates projection head; after exact receipt and fence release auth calls `FinishAdoption`. Lost staged material after older DB restore sets `profile_required`; it never mints access or reconstructs secrets. Identity-unavailable reauthentication may reactivate the same nondeleted actor while preserving all earlier lineage revocations. Creating a different actor from a deleted identity is explicitly Task 6/000007-owned; immutable schema 000005 cannot rebind the projected identity key. An old account's supplied continuity claim fails account binding and cannot be reassigned. Add `TestHostedExchangeDoesNotHoldAuthorityFenceDuringAppleOrTokenIO`, `TestExecutePreparedConsumesPrebuiltPromotionIntentBeforeStaging`, `TestPromotionIntentHashMatchesFrozenStageAndPreparedCommand`, `TestPromotionIntentHeadDriftRepreparesBeforeCAS`, and `TestProvisionalActorChangeAbandonsPromotionWithoutSession`.

- [ ] **Step 4a (3 min): Write the authenticated device-continuity regression.**

```go
func TestFreshAppleProofPreservesDevice(t *testing.T) {
 h:=newSessionFixture(t,config.ModeSelfHosted)
 original:=h.Issue()
 h.Clock.Advance(24*time.Hour); h.Restart()
 next,err:=h.Reauthenticate(original)
 if err!=nil || next.DeviceID!=original.DeviceID { t.Fatalf("device continuity lost: %v",err) }
 if next.DeviceCredential!="" { t.Fatal("reauth unexpectedly rotated one-time continuity credential") }
 if _,err:=h.Service.Authenticate(t.Context(),original.AccessToken); err==nil { t.Fatal("old session revived") }
}
```

RED: `rtk proxy sh -c 'cd Server && go test ./internal/auth -run TestFreshAppleProofPreservesDevice -count=1'`; expected newly generated DeviceID differs (or missing `ResolveDevice`). Add `TestCrossAccountDeviceClaimDenied` that authenticates another Apple subject and submits the valid original device ID+credential, expecting `device_continuity_invalid` and **zero** device/session/ledger changes. Add `TestRevokedDeviceClaimDenied`, `TestContinuityCredentialWithoutFreshAppleProofDenied` and `TestChangedOriginOrInstanceDeviceClaimDenied`.

- [ ] **Step 4b (4 min): Implement continuation proof without granting account access.**

```go
// identity/types.go; protocol has the separately mapped wire DTO.
// DeviceContinuityClaim and ExchangeRequest.DeviceContinuity are declared in P1.3.
func deviceCredentialDigest(key identity.DigestKey,origin,instanceID,accountID,deviceID,credential string) identity.KeyedDigest {
 m:=hmac.New(sha256.New,key.Secret[:])
 m.Write([]byte("baby-tracker-device-continuity-v1\x00"+origin+"\x00"+instanceID+"\x00"+accountID+"\x00"+deviceID+"\x00"+credential))
 var out [32]byte; copy(out[:],m.Sum(nil)); return identity.KeyedDigest{KeyID:key.ID,Sum:out}
}
```

`ResolveDevice` runs inside `ExecutePrepared`'s exclusive authority fence **after** fresh Apple nonce proof and snapshot-local account resolution; no earlier principal or read snapshot is accepted as authority. Validate canonical device UUID, exact 32-byte credential/canonical base64url, matching verified account/configured instance/canonical origin, nonrevoked binding and constant-time digest equality across every retained key. Store only that digest in `devices` and the opaque `device_registered` event, under dedicated mounted `BT_DEVICE_CONTINUITY_PEPPER_FILE`; the raw credential is returned once in `SessionBundle.DeviceCredential` and never logged or included in receipts. Invalid supplied claims always fail `device_continuity_invalid`; do not silently register another device. The installation hint cannot select/recover a device. Do not rotate the continuity secret on reauth: a lost reauth response must leave the old Keychain binding usable for another fresh proof. Access/refresh expiry, absolute-age reauth and refresh-reuse revoke sessions but preserve the binding. Explicit logout/disconnect and Task 6 account deletion revoke it; membership removal still denies family access even if the device survives for the account. A fresh proof plus continuity never restores membership, old session authority or an old epoch. GREEN: `rtk proxy bash Server/scripts/test-identity.sh ./internal/auth/... ./internal/identity/...`.

- [ ] **Step 4c (4 min): Pin receipt ownership across session reauthentication.**

Create `TestDeviceContinuityPreservesReceiptOwner` in `devices_test.go`. Test fixture creates a **test-only** `(device_id,mutation_id,result)` table through its owner, records an opaque applied receipt for the original device, expires/revokes application sessions, restarts, reauthenticates with fresh Apple proof+continuity and reads that exact row using the returned authenticated DeviceID; another account's claim is denied before any lookup. This is runnable in P1 without implementing P2 record tables. Add `TestLostReauthResponseCanRetryContinuity`: discard the first successful new session response, repeat fresh challenge/proof with original continuity credential, assert same device and a distinct new session. RED: `rtk proxy sh -c 'cd Server && go test ./internal/auth -run "TestDeviceContinuityPreservesReceiptOwner|TestLostReauthResponseCanRetryContinuity" -count=1'`; expect lost receipt-owner key under the old always-new-device behavior. GREEN repeats that command.

P2.6 owns real `TestLostTimerStartReauthRetrievesReceipt` in `Server/tests/protocol/reauth_receipt_test.go`: dispatch start, commit it and drop response, retain frozen bytes/mutation ID and dependency-blocked stop, advance 24h (and separately exercise refresh-reuse), restart client/server, fresh Apple proof plus same continuity, retry byte-identical start; expect `already_applied` with `original_status:applied`, original device/timer ID and no second start. Only then send the dependent stop to that exact proposed timer and assert one completion. P3.3 owns Keychain tuple `(canonicalOrigin,serverInstanceID,accountID,deviceID)` continuity storage before any outbox dispatch; P3.5 retains that binding across reauth and checks receipt original status before releasing dependencies. If Keychain continuity is absent while uncertain mutations exist, show stable `device_continuity_required`, preserve/quarantine those frozen mutations and obtain explicit user recovery input; never claim the old device by bare ID or silently replay under a new one. These are exact downstream contracts, not P1 passing evidence. Getting them wrong can strand a stop indefinitely or duplicate a committed record after required reauthentication.

- [ ] **Step 5 (4 min): Implement refresh and authentication under the fence.**

```go
func (s *Service) Refresh(context.Context,string) (SessionBundle,error)
func (s *Service) Authenticate(context.Context,string) (access.Principal,error)
func (s *Service) Logout(context.Context,access.Principal,string) (security.CommandResult,error)
```

Access TTL defaults to 10 minutes; refresh idle TTL 30 days hosted and at most remaining absolute age self-hosted; self-host absolute age default/max is 24 hours, configurable down to 5 minutes. Hosted absolute session age defaults to 30 days. Refresh performs fenced key-ID/digest lookup and expiry/epoch/security checks, stages the exact successor material, places its 64-lowercase-hex material digest in the one `session_rotated` effect, and atomically projects consume-parent/create-child. Reuse commits one `session_revoked` effect naming the lineage member and relies on the frozen set-wise SQL revocation; no replacement. `Authenticate` accepts the bearer token only from header middleware, checks current epoch/absolute expiry/revocation every request and returns shared `access.Principal`. Fresh Apple exchange always issues the initial account-scoped session. If that account owns an active family-deletion recovery claim, the first successful refresh derives and persists `family_recovery` scope from the authoritative slot; accounts without a slot remain account-scoped. No sign-in request or client field chooses recovery scope. Add `TestRecoveryScopeDerivedOnFirstRefresh` and `TestSignInCannotRequestRecoveryScope`.

- [ ] **Step 6 (4 min): Add epoch, lifetime and credential-storage tests.**

```go
func TestSelfHostedAbsoluteDeadlineCannotRefresh(t *testing.T) {
    h:=newSessionFixture(t,config.ModeSelfHosted)
    b:=h.Issue(); h.Clock.Advance(24*time.Hour)
    if _,err:=h.Service.Refresh(t.Context(),b.RefreshToken); auth.Code(err)!="reauthentication_required" { t.Fatal("refresh extended Apple proof") }
    if h.AppleCallCount()!=0 { t.Fatal("self-host contacted developer credential exchange") }
}
```

`TestEpochInvalidatesAccessAndRefresh` rotates epoch and rejects both old values even when a restored DB row says active. `TestRefreshRace` barriers two calls and confirms at most one successor then whole-lineage denial. `TestOnlyDigestsPersist` queries all credential fields via test owner and scans event bytes/log capture: neither raw token substring occurs. `TestAccessTokenLocations` rejects query/cookie/WebSocket-protocol token sources. `TestLogoutIsLedgerBacked` drops SQL after head commit, replays and proves the token remains revoked.

- [ ] **Step 7 (4 min): Implement hosted Apple exchange and encrypted revocation state.**

```go
type HostedApple struct { http *http.Client; clientID,teamID,keyID string; signingKey *ecdsa.PrivateKey; now func()time.Time }
func (a *HostedApple) ExchangeCode(context.Context,AppleCodeExchangeRequest) (AppleTokens,error)
func (a *HostedApple) Revoke(context.Context,AppleRevocationRequest) error
```

Use the exact hosted transport/client-secret/response and credential-rotation contracts frozen above. After a provisional shared authority snapshot is copied and its fence released, derive the stable binding and call `RotateForSession` with a fresh opaque operation ID and canonical session command ID. For every newly reserved command its callback exchanges the supplied authorization code, then `auth.Service` reuses P1.3 `IdentityVerifier` with `ExpectedNonce` and constant-time compares provider/subject/audience to `ExpectedIdentity` before returning the candidate to the rotator. The rotator persists candidate, revokes/marks prior, reaches promoted, and prepares the authenticated/sealed `CredentialPromotion.ConfirmIntent` before auth enters the short exclusive `ExecutePrepared` revalidation. Inside that callback `ConfirmPromotion` consumes the intent with the sole bounded metadata/state CAS to `adopting`; only success permits staging and commit. Exact same-command retry resumes; a different command cannot bypass an unfinished operation and is rejected before provider I/O. Auth calls `FinishAdoption` only after an exact authority receipt and outside the fence. Wrong binding/key/purpose/version cannot resolve, transition, open or delete. Immutable 000005 always stages the same stable `CredentialRef`; rotation state stays inside its encrypted object. Task 6/000007 owns background receipt/maintenance resumption and deletion-triggered durable revocation jobs.

Add a live hosted provider gate, disabled until real operator credentials are supplied, that performs two sequential authorizations for one test account, revokes only the older refresh token and proves with an Apple-supported operation that the newer authorization remains valid. If Apple treats revocation as authorization-wide or the result is ambiguous, hosted readiness remains `hosted_credential_rotation_unverified`; the beta must not claim independent-token rotation or enable hosted sessions. Fake transports prove state-machine behavior only, not this provider semantic.

- [ ] **Step 8 (4 min): Implement signed Apple notification ingestion and tests.**

```go
type NotificationService struct {
    keys *JWKSCache
    audience,instanceID string
    identityKeys,notificationKeys DigestKeyring
    repository NotificationRepository
    security *security.Service
    now func()time.Time
}
type AuthIdentityLookup struct { AccountID uuid.UUID; AuthorityStatus,DisplayName string; ProfileRequired bool }
type NotificationRepository interface {
    LookupAuthIdentity(context.Context,string,string,[32]byte) (AuthIdentityLookup,error)
    ReserveAppleNotification(context.Context,[]KeyedDigest,uuid.UUID,time.Time) (uuid.UUID,bool,error)
}
func (s *NotificationService) Handle(context.Context,string) error
func NotificationCommandID(key DigestKey,issuer,audience,jti string) (uuid.UUID,KeyedDigest)
```

`identity.NotificationService.Handle(context.Context,string) error` applies the exact compact-JWS, claim, event-object, time, notification-preimage and UUIDv8 rules frozen above. It derives identity lookup candidates with `identityKeys`, calls the frozen single-candidate `LookupAuthIdentity` for every candidate without early success, and reconciles exactly zero or one total match; notification keys are never reused for identity lookup. For every retained notification key it calls `NotificationCommandID`, producing paired command-ID/digest candidates in current-first order. Before SQL reservation it checks restore-safe authority receipts for **every candidate command ID**; any exact receipt means idempotent 204, while a receipt whose canonical command hash differs fails `command_id_reused`. Otherwise it calls `ReserveAppleNotification` exactly once with every candidate digest and the current candidate command ID. Multiple SQL matches fail closed. It then rechecks the returned stored command ID's authority receipt; if absent, it executes the exact returned command ID idempotently. A SQL row alone is never success, including `newlyReserved=false`. Preserve dedup tombstones for at least the accepted past-age plus restore window. Consent/account deletion emits one `identity_unavailable` effect; finalization occurs within `project_security_event`, which implicitly revokes every account session without deleting family records or device bindings. Add restore/key-rotation/receipt-before-SQL/multiple-match/duplicate-without-receipt/too-old/future/no-expiry/nondelete regressions. Real delivery remains a hosted gate.

- [ ] **Step 9 (4 min): Wire mode-aware HTTP sessions and fixtures.**

```go
func appleExchangeHandler(*auth.Service) http.HandlerFunc
func refreshHandler(*auth.Service) http.HandlerFunc
func logoutHandler(*auth.Service) http.HandlerFunc
func appleNotificationHandler(*identity.NotificationService) http.HandlerFunc
```

Register `POST /v1/auth/apple`, `/v1/auth/refresh`, `/v1/auth/logout` and hosted-only `/v1/auth/apple/notifications`; OpenAPI annotates hosted-only routes and route tests compare mode-specific sets. Every initial, continued-device and refresh session wire response is `{protocol_version,access_token,refresh_token,expires_at,reauthenticate_at,recovery_epoch,device_id,device_credential?,account:{id,display_name,profile_required},family_slot?:{family_id,state}}`; `recovery_epoch` is the exact positive JSON-safe fenced epoch that minted the bundle. `device_credential` is present only for initial device registration; a continued device omits it and retains its existing Keychain value. Apple request optionally carries `device_continuity:{device_id,credential}`; both fields are required together and credential is exactly 43 canonical base64url characters. Refresh request is `{protocol_version,capabilities,refresh_token}`; logout request adds `command_id` and revokes that device binding with its sessions. Apply the frozen HTTP error matrix and `Cache-Control:no-store` contract to code, fixtures and OpenAPI. Self-host code or developer-key input is rejected `hosted_only_field`, notification route 404. The client-only missing-binding issue uses `device_continuity_required` and never appears as a server inference. Fixtures assert initial/continued/refresh response epochs, same authenticated device ID, missing proof and cross-account denial without disclosing which binding check failed.

- [ ] **Step 10 (3 min): Run GREEN and current full suite.**

Run every gate from the worktree and record exact command/exit status:

```bash
rtk proxy sh -c 'cd Server && go test ./internal/auth ./internal/identity ./internal/security ./internal/httpapi ./internal/protocol ./internal/config -count=1'
rtk proxy sh -c 'cd Server && go test -race ./internal/auth ./internal/identity ./internal/security ./internal/httpapi -count=1'
rtk proxy bash Server/scripts/test-identity.sh ./internal/auth/... ./internal/identity/... ./internal/security/... ./internal/httpapi/... ./internal/protocol/... ./internal/config/...
rtk proxy bash Server/scripts/test-identity.sh
rtk proxy sh -c 'cd Server && go test -tags identitytest ./internal/testidentity ./internal/auth ./internal/httpapi -count=1'
rtk proxy sh -c 'cd Server && if go list -deps ./cmd/baby-tracker-server | rg -q internal/testidentity; then exit 1; fi'
rtk proxy sh -c 'cd Server && go test ./scripts -run TestTask4ImmutabilityGateCleanAndMutated -count=1'
rtk proxy sh -c 'cd Server && go vet ./...'
rtk make server-smoke
rtk make test
rtk git diff --check
rtk git diff --quiet "$BT_TASK3_ACCEPTED_REF" -- Server/migrations/000005_identity_sessions.sql Server/internal/migrate/compatibility.json
```

Expected: every gate PASS with an empty final diff, zero old-epoch access, no raw bearer/continuity/Apple credential in storage/events/logs, current-key writes plus retained-key reads, response-bound recovery epoch, mode-correct routes and authority-backed notification replay protection. Real-role tests prove exact runtime/projector/PUBLIC denial and no request-path projector/pool seam. Smoke uses explicit migrate/security-init/reconcile and distinct runtime/projector credentials. PostgreSQL/Compose resources use the pinned disposable image/configuration and are proven absent after cleanup. Fake keys/transports are not evidence of live Apple sign-in.

- [ ] **Step 11 (2 min): Commit opaque sessions.**

```bash
rtk git add Server/internal/auth Server/internal/identity Server/internal/httpapi Server/internal/config Server/internal/protocol Server/internal/recovery/state.go Server/internal/recovery/state_test.go Server/internal/security/commands.go Server/internal/security/commands_test.go Server/internal/security/state.go Server/internal/security/state_test.go Server/internal/security/event.go Server/internal/security/event_test.go Server/internal/security/transaction.go Server/internal/security/transaction_test.go Server/internal/security/coordinator.go Server/internal/security/coordinator_test.go Server/internal/security/projector.go Server/internal/security/projector_test.go Server/cmd/baby-tracker-server Server/compose.yaml Server/deploy/postgres/init-users.sh Server/scripts/smoke.sh Server/scripts/smoke_test.go Server/scripts/test-identity.sh Server/scripts/task4_immutability_test.go Server/README.md Server/api Server/docs/protocol/identity-v1.md
rtk git commit -m "feat(server): rotate opaque sessions and revoke reused credentials"
```

### Task 5: Authorize family slots, ownership, invitations and entitlements (P1.5)

- [ ] **Step 0 (4 min): Land the reviewed schema-6 compatibility predecessor.** Capture accepted schema-5 feature head as `BT_MIGRATION_BASE_REF`. Create final immutable 000006, complete v6 privilege snapshot, overlap tests/scenarios, and manifest `[5,6]`; run the predecessor suite on schema 5; commit only compatibility files as `feat(server): prepare family schema compatibility`; review it and capture `BT_MIGRATION_PRIOR_REF`. It calls no family-v6 object. Remaining Task 5 edits form the `[6,6]` feature commit.

**Files:**

- Create: `Server/internal/family/types.go`, `service.go`, `membership.go`, `invitations.go`, `entitlements.go`, `repository.go`, `service_test.go`, `invitations_test.go`, `entitlements_test.go`; `Server/internal/protocol/family.go`, `family_test.go`; `Server/internal/httpapi/account.go`, `family.go`, `invitation_handoff.go`, `family_test.go`; `Server/internal/httpapi/assets/invitation.html`; `Server/migrations/000006_family_commands.sql`.
- Modify: `Server/internal/security/commands.go`, `state.go`, `projector.go`; `Server/internal/httpapi/router.go`; `Server/internal/config/limits.go`; `Server/internal/migrate/compatibility.json`; `Server/internal/tenant/context_test.go`; `Server/api/openapi.yaml`.
- Create: `Server/api/fixtures/family.json`, `family-invitation.json`, `family-slot-conflict.json`.
- Test: owner/role/slot/invitation races, cross-family denial, account/member profile projection and handoff privacy.

**Interfaces:**

```go
type FamilyAuthorizer interface {
    Authorize(context.Context,access.Principal,string,string) (access.Grant,error)
}
type FamilyView struct { ID,DisplayName,Status string; ProfileRequired bool; Members []MemberView }
type MemberView struct { AccountID,DisplayName,Role,Status string }
type InvitationView struct { ID,FamilyID,Status string; ExpiresAt time.Time }
type InvitationSecret struct { Invitation InvitationView; Token,Link string }
type EntitlementPolicy interface { Check(context.Context,string,string,time.Time) error }
type Service struct {
    security *security.Service
    repository *Repository
    entitlements EntitlementPolicy
    invitationKeys identity.DigestKeyring
    origin string
    now func() time.Time
    maxCaregivers,maxInvitations int
    invitationTTL time.Duration
}
type Repository struct { runtime *pgxpool.Pool }
func (s *Service) Authorize(context.Context,access.Principal,string,string) (access.Grant,error)
func (s *Service) Create(context.Context,access.Principal,string,string) (FamilyView,error) // commandID, displayName
func (s *Service) Current(context.Context,access.Principal) (FamilyView,error)
func (s *Service) Invite(context.Context,access.Principal,string) (InvitationSecret,error) // commandID
func (s *Service) Redeem(context.Context,access.Principal,string,string,bool) (FamilyView,error) // commandID, token, confirmed
func (s *Service) RevokeInvitation(context.Context,access.Principal,string,string) (security.CommandResult,error) // commandID, invitationID
func (s *Service) Transfer(context.Context,access.Principal,string,string) (security.CommandResult,error) // commandID, newOwnerID
func (s *Service) Remove(context.Context,access.Principal,string,string) (security.CommandResult,error) // commandID, accountID
func (s *Service) Leave(context.Context,access.Principal,string) (security.CommandResult,error)
```

Consumes shared `access` types, P1.2 guard, P1.4 authentication; `Service.Authorize` is injected as the guard's `Decision`, never as a cached capability. Fixed actions: `read`, `mutate_record`, `add_baby`, `delete_baby`, `manage_members`, `transfer_owner`, `export`, `delete_family`, `recover_family`, `forfeit_recovery`. Recovery status uses account lifecycle method in P1.6, not the family read action. `family.New(Options) (*Service,error)` options contain `Security *security.Service`, `Repository *Repository`, `Entitlements EntitlementPolicy`, `InvitationPepper []byte`, `Origin string`, `Now func()time.Time`, `MaxCaregivers,MaxInvitations int`, `InvitationTTL time.Duration`.

- [ ] **Step 1 (3 min): Write the concurrent slot test.**

```go
func TestConcurrentFamilySlotClaims(t *testing.T) {
    h:=newFamilyFixture(t)
    a,b:=h.InvitationForFamilyA(),h.InvitationForFamilyB()
    var wins atomic.Int32; var wg sync.WaitGroup
    for i,token:=range []string{a,b} { i,token:=i,token; wg.Go(func(){ _,err:=h.Service.Redeem(t.Context(),h.NewCaregiver,h.CommandID(i),token,true); if err==nil { wins.Add(1) } }) }
    wg.Wait()
    if wins.Load()!=1 || h.ActiveSlots(h.NewCaregiver.AccountID)!=1 { t.Fatal("one-family invariant failed") }
}
```

Define `familyFixture{Service *Service; Owner,NewCaregiver access.Principal; Runtime *pgxpool.Pool}` plus `newFamilyFixture(*testing.T) *familyFixture`, `InvitationForFamilyA() string`, `InvitationForFamilyB() string`, `CommandID(int) string`, `ActiveSlots(string) int`, `CurrentHead() security.Head`. It creates three verified accounts/two owned families via real services and returns two distinct invitation tokens. Test helpers never expose raw credentials in failures.

- [ ] **Step 2 (2 min): Run RED.**

Run: `rtk proxy sh -c 'cd Server && go test ./internal/family -run TestConcurrentFamilySlotClaims -count=1'`.

Expected: FAIL because `family.Service` and slot enforcement do not exist.

- [ ] **Step 3 (4 min): Add family schema and projection-only structural writes.**

```sql
CREATE TABLE family_slots (
 account_id uuid PRIMARY KEY REFERENCES accounts(id),
 family_id uuid NOT NULL REFERENCES families(id),
 state text NOT NULL CHECK(state IN ('active','family_recovery'))
);
ALTER TABLE family_slots ENABLE ROW LEVEL SECURITY;
ALTER TABLE family_slots FORCE ROW LEVEL SECURITY;
CREATE POLICY own_slot ON family_slots USING(account_id=app.current_account_id());
REVOKE INSERT,UPDATE,DELETE ON families,memberships FROM baby_tracker_runtime;
```

Migration 000006 creates `family_slots(account_id PRIMARY KEY,family_id,state)` with `state IN ('active','family_recovery')`; invitations with unique HMAC digest/status/expiry/redeemer; entitlements with family/key/source/status/expiry; family-scoped actor-label projection with `former` flag. Every family-owned row has `family_id`, forced RLS and existing transaction-local family policy. Revoke runtime direct INSERT/UPDATE/DELETE on `families/memberships`; grant SELECT only plus narrow own-profile update function. Structural DML occurs only through the projector function/role. Update foundation owner-trigger tests to use the narrowly provisioned projection test path while preserving independent checks that runtime cannot forge structural rows or bypass RLS. Keep deferred exactly-one-owner trigger as a backstop.

- [ ] **Step 4 (4 min): Implement role policy and account-scoped discovery.**

```go
func allowed(role,action string) bool {
    if action=="read" || action=="mutate_record" || action=="add_baby" { return role=="owner" || role=="caregiver" }
    switch action { case "delete_baby","manage_members","transfer_owner","export","delete_family": return role=="owner" }
    return false
}
```

`Authorize` additionally validates active account/session/epoch, current family slot and membership, lifecycle and entitlement from `security.Service.ReadState`; the guard's internal marker makes this read reuse its held fence. At composition time create a `Decision` closure that calls the family service after it is constructed; before construction it returns `service_unavailable`. Construct security, then family with that security pointer, then mount HTTP routes; there is no request-time setter or period of permissive authorization. Owner identity-unavailable blocks owner-only actions while caregiver actions remain available. `recover_family/forfeit_recovery` are allowed only under P1.6's narrow recovery claim; unknown actions deny. Account discovery is a parameterized own-account function; no arbitrary account/family lookup. Family member display names come from a family-scoped projection refreshed on explicit profile edits, excluding email/subject/session state. `GET /v1/account` returns own profile, family slot and recovery status only; `PATCH` accepts confirmed display_name only.

- [ ] **Step 5 (4 min): Implement atomic family creation/transfer.**

```go
type CreateFamilyPayload struct { Version,FamilyID,OwnerID string }
type TransferOwnerPayload struct { Version,FamilyID,OldOwnerID,NewOwnerID string }
func (s *Service) Create(context.Context,access.Principal,string,string) (FamilyView,error)
func (s *Service) Transfer(context.Context,access.Principal,string,string) (security.CommandResult,error)
```

`Create` validates no active/recovery slot, generates family ID, stages display name, then commands family+owner-membership+slot+hosted-beta entitlement together. `Transfer` requires a distinct active caregiver in the same family, live owner authority and expected state; one event updates both roles, invalidates stale owner authorization and projects both rows in one transaction. Two racing transfers: only the command serialized while the actor is owner can succeed; the other returns `owner_required`. No “remove current owner” or owner `Leave` command is valid. Maximum active caregivers **including owner** is 10 by default; count and slot check happen inside security serialization.

- [ ] **Step 6 (4 min): Implement single-use invitations with explicit confirmation.**

```go
func (s *Service) Invite(context.Context,access.Principal,string) (InvitationSecret,error)
func (s *Service) Redeem(context.Context,access.Principal,string,string,bool) (FamilyView,error)
func (s *Service) Preview(context.Context,access.Principal,string) (InvitationPreview,error)
type InvitationPreview struct { FamilyDisplayName string; ExpiresAt time.Time }
```

Generate 32 random bytes, encode unpadded base64url, hash with dedicated invitation pepper; default TTL 7×24h and maximum 10 outstanding active invitations per family. Ledger persists invitation ID/digest/expiry/status so consumed invites cannot revive after restore. Redemption validates token format, expiry, active family, role/slot limits and `confirmed:true` before commit; competing revoke/redeem gives one ordered result, never two memberships. `Invite` returns `canonical_origin + "/invite#token=" + token` once; `canonical_origin` already includes the `https://` scheme and never ends in a path or slash. GET metadata takes invitation UUID, never secret query. Provide `POST /v1/family/invitations/preview` with token in bounded body and only safe family display label/expiry; preview requires authenticated user but does not reserve/consume membership. Repeated create command returns stable invitation metadata without manufacturing its secret.

- [ ] **Step 7 (4 min): Implement minimal handoff page.**

```javascript
const fragment = new URLSearchParams(location.hash.slice(1));
const token = fragment.get('token');
history.replaceState(null, '', '/invite');
if (!token || !/^[A-Za-z0-9_-]{43}$/.test(token) || [...fragment.keys()].some(k => k !== 'token')) {
  document.getElementById('status').textContent = 'Invalid invitation code';
} else {
  document.getElementById('code').textContent = token;
  document.getElementById('open').href = 'babytracker://invite?server=' + encodeURIComponent(location.origin) + '&token=' + token;
}
```

The static markup includes `status`, readonly `code`, `open` and a Copy button; Copy uses `navigator.clipboard.writeText` only on click and exposes a selectable code when unavailable. Reject duplicate `token` fragment parameters before use.

Embed `assets/invitation.html` at `GET /invite` with static script reading `location.hash`, `history.replaceState` clearing it before rendering, strict 43-character base64url token validation, copyable code and `babytracker://invite?server=<percent-encoded-origin>&token=<token>` launch link. No network call carries token on load, no automatic navigation/redemption, no third-party resources. Use `Cache-Control:no-store`, `Referrer-Policy:no-referrer`, CSP with exact embedded script hash and `connect-src 'none'`, `frame-ancestors 'none'`; never echo fragment/token into server-generated HTML/logs. Hosted Universal Link association publishing belongs to Plan 3/4; this page is not a web client.

- [ ] **Step 8 (3 min): Implement free entitlement policy.**

```go
type SelfHostedEntitlementPolicy struct{}
func (SelfHostedEntitlementPolicy) Check(_ context.Context,_ string,key string,_ time.Time) error {
    if key!="family_sync" { return ErrEntitlementRequired }; return nil
}
type HostedEntitlementPolicy struct { Security *security.Service }
func (p HostedEntitlementPolicy) Check(context.Context,string,string,time.Time) error
```

Hosted create grants `family_sync` source `hosted_beta`, expiry now+366 days; operator can grant/extend explicitly through P1.6. Suspended/expired grants deny import/sync/ordinary family actions according to policy, but never block own account deletion, deletion status or recovery-forfeit rights. Plan 2 checks the entitlement before starting a batch/import; no billing verifier/UI is added. Define `ErrEntitlementRequired` with safe code `entitlement_required`.

- [ ] **Step 9 (4 min): Add concrete concurrency/privacy policy tests.**

```go
func TestRoleMatrix(t *testing.T) {
    for _,action:=range []string{"read","mutate_record","add_baby"} { if !allowed("caregiver",action) { t.Fatal(action) } }
    for _,action:=range []string{"delete_baby","manage_members","transfer_owner","export","delete_family"} {
        if allowed("caregiver",action) || !allowed("owner",action) { t.Fatal(action) }
    }
    if allowed("owner","invented_action") { t.Fatal("unknown action allowed") }
}
```

Add `TestCreateVersusJoinClaimsOneSlot`, `TestOwnerTransferRace`, `TestInvitationRedeemRevokeRace`, `TestInvitationExpiresAtBoundary`, `TestInvitationGetNeverRedeems`, `TestRemovedMemberNameProjection`, `TestCrossFamilyRepositoryAndRouteDenial`, `TestHostedEntitlementExpiry`, `TestSelfHostEntitlementAlwaysOn`. Race tests use barriers, await both goroutines and assert canonical roles/slots from authority **and** runtime-visible projection. Request another family's invitation/member/account IDs and assert 404/403 with no leaked names. Handoff test asserts no query token/server request and no token in logs; browser-level copy/launch verification is Plan 3.

- [ ] **Step 10 (4 min): Add exact account/family routes and fixtures.**

```go
type ConfirmedCommandRequest struct {
 RequestMeta
 CommandID string `json:"command_id"`
 Confirmed bool `json:"confirmed"`
}
type RedeemInvitationRequest struct {
 ConfirmedCommandRequest
 Token string `json:"token"`
}
```

Register `GET/PATCH /v1/account`, `POST/GET /v1/family`, `POST /v1/family/invitations`, `GET /v1/family/invitations/{id}`, `POST /v1/family/invitations/{id}/revoke`, `POST /v1/family/invitations/preview`, `POST /v1/family/invitations/redeem`, `POST /v1/family/members/{account_id}/remove`, `POST /v1/family/ownership-transfer`, `POST /v1/family/leave`, `GET /invite`. Every mutation JSON adds request metadata and canonical `command_id`; redeem also requires `confirmed:true`. Transfer body uses `new_owner_account_id`. Account/family DTO fields match Interfaces exactly after snake_case encoding. Stable codes: `family_slot_occupied`, `owner_required`, `membership_revoked`, `family_unavailable`, `invitation_invalid`, `invitation_expired`, `invitation_consumed`, `invitation_revoked`, `caregiver_limit`, `invitation_limit`, `confirmation_required`, `entitlement_required`.

- [ ] **Step 11 (3 min): Run GREEN and full server regression.**

Run: `rtk proxy bash Server/scripts/test-identity.sh ./internal/family/... ./internal/httpapi/... ./internal/protocol/... ./internal/tenant/...`; then `rtk proxy bash Server/scripts/test-identity.sh`.

Expected: PASS under real runtime-role RLS, exactly one family per account and one owner under concurrency; metadata GET/preview remain non-destructive; no token/name leakage.

Capture accepted schema-5 feature head as `BT_MIGRATION_BASE_REF`; first commit final 000006, complete v6 privilege snapshot/scenarios, and manifest `[5,6]` without family feature dependency, review it, and capture `BT_MIGRATION_PRIOR_REF`. Implement family behavior in the second `[6,6]` commit and capture `BT_MIGRATION_FEATURE_REF`. Run prepared mode with all three refs; predecessor account reads pass at schema 5 and 6, then feature family behavior passes at 6, with archives unchanged and structural denial retained.

- [ ] **Step 12 (2 min): Commit family feature and run prepared overlap.**

```bash
rtk git add Server/internal/family Server/internal/protocol Server/internal/httpapi Server/internal/security/commands.go Server/internal/security/state.go Server/internal/security/projector.go Server/internal/config/limits.go Server/internal/migrate/compatibility.json Server/internal/tenant/context_test.go Server/migrations/000006_family_commands.sql Server/api
rtk git commit -m "feat(server): enforce family ownership and invitation lifecycle"
BT_MIGRATION_FEATURE_REF=$(rtk git rev-parse HEAD)
rtk proxy bash Server/scripts/check-migration-overlap.sh --base-ref "$BT_MIGRATION_BASE_REF" --prior-ref "$BT_MIGRATION_PRIOR_REF" --feature-ref "$BT_MIGRATION_FEATURE_REF" --next-version 6 --prior-min 5 --prior-max 6 --next-min 6 --next-max 6
```

### Task 6: Schedule deletion, constrain recovery and audit operator actions (P1.6)

- [ ] **Step 0 (4 min): Land the reviewed schema-7 compatibility predecessor.** Capture accepted schema-6 feature head, create final immutable 000007, complete v7 privilege snapshot, overlap tests/scenarios, and manifest `[6,7]`; run the predecessor suite on schema 6 and commit only compatibility files as `feat(server): prepare lifecycle schema compatibility`. It calls no v7 lifecycle object. Before the feature commit, land immutable additive `000008_lifecycle_worker_resume.sql` with the bounded revoker-only unfinished-cleanup claim, exact v8 privilege snapshot/tests, and manifest `[7,8]` as `feat(server): prepare lifecycle worker resume compatibility`. Use the accepted schema-7 predecessor as `BT_MIGRATION_BASE_REF` and this schema-8 repair predecessor as `BT_MIGRATION_PRIOR_REF`. Remaining Task 6 edits form the `[8,8]` feature commit; never rewrite 000007.

**Files:**

- Create: `Server/internal/account/lifecycle.go`, `apple_revocation.go`, `lifecycle_test.go`, `apple_revocation_test.go`; `Server/internal/family/lifecycle.go`, `lifecycle_test.go`; `Server/internal/security/invalidation.go`, `invalidation_test.go`; `Server/internal/operator/commands.go`, `audit.go`, `confirmation.go`, `commands_test.go`; `Server/internal/config/lifecycle.go`, `lifecycle_test.go`; `Server/cmd/baby-tracker-admin/cli_examples_test.go`; `Server/migrations/000007_lifecycle.sql`, `Server/migrations/000008_lifecycle_worker_resume.sql`; `docs/operations/ownership-recovery.md`.
- Modify: `Server/cmd/baby-tracker-admin/main.go`, `main_test.go`; `Server/cmd/baby-tracker-server/main.go`, `main_test.go`; `Server/internal/auth/sessions.go`, `sessions_test.go`, `refresh.go`, `refresh_test.go`; `Server/internal/identity/credentials.go`, `credentials_test.go`, `credentialtest/conformance.go`, `credentials_conformance_test.go`; `Server/internal/httpapi/account.go`, `family.go`, `router.go`; `Server/internal/protocol/family.go`, `family_test.go`; `Server/internal/security/commands.go`, `state.go`, `event_handlers.go`, `projector.go`, `reserved_events_test.go`; `Server/internal/migrate/compatibility.json`; `Server/internal/tenant/context_test.go`; `Server/deploy/postgres/init-roles.sql`, `init-users.sh`; `Server/scripts/check-principals.sh`; `Server/compose.yaml`; `Server/README.md`; `Server/api/openapi.yaml`.
- Create: `Server/api/fixtures/family-recovery.json`, `account-deleted.json`, `operator-confirmation.json`, `operator-cli-v1.json`.
- Test: lifecycle role matrix, 30-day boundary, restored identity erasure, recovery-slot concurrency and operator denial/confirmation.

**Interfaces:**

```go
// internal/family/lifecycle.go
type RecoveryStatus struct { FamilyID,State string; PurgeAfter time.Time; CanRecover bool }
func (s *Service) ScheduleDeletion(context.Context,access.Principal,string,bool) (RecoveryStatus,error)
func (s *Service) RecoveryStatus(context.Context,access.Principal) (RecoveryStatus,error)
func (s *Service) Recover(context.Context,access.Principal,string,bool) (security.CommandResult,error)
func (s *Service) Forfeit(context.Context,access.Principal,string,bool) (security.CommandResult,error)
// string is commandID; bool is explicit confirmation.
// internal/account/lifecycle.go: request path never owns provider credentials.
type Service struct { Security *security.Service }
func (s *Service) Delete(context.Context,access.Principal,string,bool) (security.CommandResult,error)
type AppleRevocationJob struct {
    JobID,LeaseToken,OperationVersion,OperationID,CommandID,Status string
    AccountID,ServerInstanceID,Purpose,ObjectID string
    ReferenceID,ReferenceKeyID string
    Attempts int
    LeaseExpiresAt time.Time
}
type AppleRevocationRepository interface {
    Claim(context.Context,string,time.Time,time.Time) (AppleRevocationJob,bool,error)
    ValidateClaim(context.Context,AppleRevocationJob,time.Time) error
    CredentialReferencedOutsideJob(context.Context,AppleRevocationJob,time.Time) (bool,error)
    MarkProviderRevoked(context.Context,AppleRevocationJob,time.Time) error
    BeginCleanup(context.Context,AppleRevocationJob,time.Time) error
    Complete(context.Context,AppleRevocationJob,time.Time) error
    Retry(context.Context,AppleRevocationJob,string,time.Time,time.Time) error // safe error code, next attempt, now
    ClaimOrphanCleanup(context.Context,identity.CredentialBinding,identity.CredentialRef,string,time.Time,time.Time) (security.CredentialCleanupClaim,bool,error)
    ValidateOrphanCleanup(context.Context,security.CredentialCleanupClaim,time.Time) error
    CredentialReferenced(context.Context,security.CredentialCleanupClaim,time.Time) (bool,error)
    FinishOrphanCleanup(context.Context,security.CredentialCleanupClaim,time.Time) error
    RetryOrphanCleanup(context.Context,security.CredentialCleanupClaim,string,time.Time,time.Time) error
}
type AppleRevocationWorker struct { WorkerID string; Repository AppleRevocationRepository; Vault identity.CredentialVault; Rotations identity.CredentialRotator; Inventory identity.CredentialInventory; Apple identity.AppleRevoker; Security *security.Service; Now func()time.Time; Lease,OrphanGrace time.Duration }
func (w *AppleRevocationWorker) RunOnce(context.Context) error
// internal/security/invalidation.go
type SecurityInvalidation struct { AccountID,FamilyID,SessionID string; Revision uint64 }
type InvalidationSink interface { Invalidate(context.Context,SecurityInvalidation) }
func (s *Service) Subscribe(InvalidationSink) (unsubscribe func())
// internal/operator/commands.go
type RecoveryProposal struct {
 CommandID string `json:"command_id"`
 FamilyID string `json:"family_id"`
 OldOwnerID string `json:"old_owner_id"`
 NewOwnerID string `json:"new_owner_id"`
 PolicyID string `json:"policy_id"`
 EvidenceReference string `json:"evidence_reference"`
 ReasonCode string `json:"reason_code"`
 ExpectedRevision uint64 `json:"expected_revision"`
}
type Confirmation struct { Digest string `json:"digest"`; ExpiresAt time.Time `json:"expires_at"` }
func (s *Service) PrepareRecovery(context.Context,RecoveryProposal) (Confirmation,error)
func (s *Service) RecoverOwner(context.Context,RecoveryProposal,Confirmation) (security.CommandResult,error)
```

`AppleRevocationJob` is the complete leased claim. `OperationVersion` is a
canonical positive decimal string backed by a monotonically increasing SQL
`bigint`, but is opaque to Go and callers never increment or interpret it.
Every repository method after `Claim` compares **all** claim fields, including
status/attempt, and requires the matching lease token, operation version and
unexpired `LeaseExpiresAt`; a changed coordinate or `now >= LeaseExpiresAt`
returns `stale_lease` without mutation. Dedicated `OperationID` and `CommandID`
are canonical nonzero UUIDs and are stable across retries. `ClaimOrphanCleanup`
returns the existing complete `security.CredentialCleanupClaim`; its validate,
reference, retry and finish calls likewise compare every claim coordinate and
the current unexpired lease rather than closing over repository state.

Before immutable migration 000007 is committed, freeze these worker-only SQL
functions and exact types. `operation_version` is stored as positive `bigint`
but returned/accepted as canonical decimal `text`, keeping it opaque to Go.
`apple_claim` below means the exact ordered argument list
`(p_job_id uuid,p_lease_token text,p_operation_version text,p_operation_id uuid,p_command_id uuid,p_status text,p_account_id uuid,p_server_instance_id uuid,p_purpose text,p_object_id uuid,p_reference_id text,p_reference_key_id text,p_attempts integer,p_lease_expires_at timestamptz)`;
`cleanup_claim` means
`(p_lease_token text,p_operation_version text,p_operation_id uuid,p_command_id uuid,p_account_id uuid,p_object_id uuid,p_reference_id text,p_reference_key_id text)`.

- `app.claim_apple_revocation_job(p_worker_id text,p_now timestamptz,p_lease_expires_at timestamptz)`
  returns one row in the exact `AppleRevocationJob` field order above.
- `app.validate_apple_revocation_claim(apple_claim,p_now timestamptz)`,
  `app.mark_apple_provider_revoked(apple_claim,p_now timestamptz)`,
  `app.begin_apple_credential_cleanup(apple_claim,p_now timestamptz)`, and
  `app.complete_apple_revocation_job(apple_claim,p_now timestamptz)` return
  `void`; `app.apple_credential_referenced_outside_job(apple_claim,p_now timestamptz)`
  returns `boolean`.
- `app.retry_apple_revocation_job(apple_claim,p_error_code text,p_next_attempt_at timestamptz,p_now timestamptz)`
  returns `void`.
- `app.claim_credential_cleanup(p_account_id uuid,p_server_instance_id uuid,p_purpose text,p_object_id uuid,p_reference_id text,p_reference_key_id text,p_worker_id text,p_now timestamptz,p_lease_expires_at timestamptz)`
  returns the eight `security.CredentialCleanupClaim` fields in declaration order.
- `app.validate_credential_cleanup_claim(cleanup_claim,p_now timestamptz)` and
  `app.finish_credential_cleanup(cleanup_claim,p_now timestamptz)` return `void`;
  `app.credential_cleanup_referenced(cleanup_claim,p_now timestamptz)` returns
  `boolean`; `app.retry_credential_cleanup(cleanup_claim,p_error_code text,p_next_attempt_at timestamptz,p_now timestamptz)`
  returns `void`.

The names `apple_claim` and `cleanup_claim` are documentation shorthand only,
not PostgreSQL composite types: migration 000007 spells out every argument so
the privilege snapshot and `pg_proc` signature tests freeze the public surface.

The migration owns SQL tests
`TestLifecycleMigrationFreezesWorkerFunctionSignatures`,
`TestAppleRevocationRepositoryRequiresFullLiveClaim`,
`TestAppleRevocationRepositoryRejectsExpiredLeaseAndStaleOperationVersion`, and
`TestOrphanClaimRoundTripsSecurityCredentialCleanupClaim`. Each mutation test
changes every claim field one at a time and proves job/object state is
unchanged.

Consumes P1.4's credential vault/ref and Apple revoker ports **with the
`CredentialRotationState.ReconcileAfter` extension frozen above**, plus P1.5
slot/role policy. Migration 000007 adds the projector path that creates
`apple_revocation_jobs` from an account-deletion event and the worker-only SQL
surface above; neither Task 4 nor request-path code performs direct job-table
DML. It also defines the identity-projection rebinding/tombstone behavior needed
for a fresh proof to create a new actor after deletion and advances the
tombstone's actor lifecycle generation exactly once. `operator.Service` owns
`Security`, configured opaque `OperatorID`, separate `OwnershipPolicyID` and
`EntitlementPolicyID`, a strict confirmation-key file value, and clock. Missing
operation policy disables only that dangerous command; verification/reconciliation
remain available. Operator principal is minted only in CLI wiring and every
committed event carries the policy ID, keyed evidence digest, reason code,
positive expected revision, and operator UUID. HTTP cannot mint operator scope.

- [ ] **Step 1 (3 min): Pin narrow recovery and slot reservation.**

```go
func TestRecoveryClaimReservesSlot(t *testing.T) {
    h:=newLifecycleFixture(t)
    status,err:=h.Family.ScheduleDeletion(t.Context(),h.Owner,h.CommandID(1),true)
    if err!=nil || !status.PurgeAfter.Equal(h.Now.Add(30*24*time.Hour)) { t.Fatalf("status=%+v err=%v",status,err) }
    signedIn:=h.FreshOwnerSignIn()
    if signedIn.Scope!="account" { t.Fatal("sign-in minted non-account scope") }
    p:=h.Refresh(signedIn)
    if p.Scope!="family_recovery" { t.Fatal("missing narrow claim") }
    if _,err:=h.Family.Authorize(t.Context(),p,status.FamilyID,"read"); security.Code(err)!="recovery_only" { t.Fatal("recovery read family data") }
    if _,err:=h.Family.Create(t.Context(),p,h.CommandID(2),"Another family"); security.Code(err)!="family_slot_occupied" { t.Fatal("recovery lost slot reservation") }
}
```

Define `lifecycleFixture{Family *family.Service; Accounts *account.Service; Owner,Caregiver access.Principal; Now time.Time}` plus `newLifecycleFixture(*testing.T) *lifecycleFixture`, `CommandID(int) string`, `FreshOwnerSignIn() access.Principal`, `Refresh(access.Principal) access.Principal`, `Advance(time.Duration)`, `LoseOwnerAppleIdentity()`, `RestoreOldDatabase()`, `Labels() []string`, `RecordCount() int`. Fixture stores one synthetic historical actor-reference row for lifecycle tests; it does not create canonical record tables before Plan 2. `RecordCount` counts that test-only reference, proving no implicit family-data delete. `TestRecoveryScopeDerivedOnFirstRefresh` separately proves that sign-in remains account-scoped, the first refresh derives `family_recovery`, and an ordinary family authorization remains `recovery_only`.

- [ ] **Step 2 (2 min): Run RED.**

Run: `rtk proxy sh -c 'cd Server && go test ./internal/family -run TestRecoveryClaimReservesSlot -count=1'`.

Expected: FAIL due to absent lifecycle methods/narrow scope enforcement.

- [ ] **Step 3 (4 min): Add deletion/recovery projections.**

```sql
CREATE TABLE family_recovery_claims (
 family_id uuid PRIMARY KEY REFERENCES families(id),
 account_id uuid UNIQUE NOT NULL REFERENCES accounts(id),
 purge_after timestamptz NOT NULL,
 status text NOT NULL CHECK(status IN ('active','recovered','forfeited','expired')),
 revision bigint NOT NULL CHECK(revision>0)
);
```

Migration 000007 adds `family_recovery_claims`, `operator_audit_events`, owner identity-unavailable state and projected `family_actor_labels`; use `family_id`, forced RLS for family-scoped rows, exact status constraints and unique active claim-owner/family. Scheduling deletion sets family `deletion_pending`, removes all normal memberships, revokes linked sessions, records `purge_after=command_time+30d`, reserves the owner's slot and grants only the recovery claim in one event. Fresh owner authentication can inspect/recover/forfeit; all normal family reads/mutations deny immediately, even while projection is quarantined. Recovery before (strictly `<`) deadline reactivates owner membership, clears purge deadline/claim and preserves owner's one slot; former caregivers must be reinvited. Forfeiture removes claim/slot irrevocably and leaves deletion scheduled. Physical deletion is exclusively P4.1.

- [ ] **Step 4 (4 min): Implement account deletion and former attribution.**

```go
type DeleteAccountPayload struct {
    Version,AccountID,IdentityKey string
}
func (s *Service) Delete(context.Context,access.Principal,string,bool) (security.CommandResult,error)
```

`account.Delete` denies an owner of an active family with `ownership_transfer_required`; accepts after transfer or confirmed family deletion. It commands account/identity deletion, membership removal, every application session/device revocation and any recovery forfeiture together. Projection deletes live identity rows/profile data while retaining only the authority tombstone, sets actor's presentation to `Former caregiver` in each historical family projection and never retains deleted display name. Raw Apple subject is erased; opaque actor ID remains. A later fresh proof for the same identity digest creates a new account/actor projection and can never claim the deleted actor's continuity or memberships; add `TestDeletedIdentityCreatesNewActorButRejectsOldContinuity`. Ordinary `Leave/Remove` pseudonymizes that family's labels and revokes family sessions but preserves the account identity/profile for future sign-in.

In hosted mode the same account-deletion projection creates exactly one pending `apple_revocation_jobs` row from the P1.4 stable `CredentialRef` before erasing profile/identity projection data. Migration 000007 adds immutable `deletion_operation_id` and `deletion_command_id`, positive monotonic `operation_version`, `provider_revoked_at`, `cleanup_started_at`, `lease_owner`, opaque `lease_token` and `lease_expires_at`, and replaces the frozen schema-5 status constraint with `pending|retry|revoke_succeeded|cleanup_pending|complete`. Claim returns both IDs plus the complete binding/ref coordinates, status/attempt, bounded lease/token and canonical-decimal opaque operation version. For `pending|retry`, `RunOnce` first resolves any incomplete rotation to a CAS-stable state and, outside the authority fence, calls `PrepareDeletionCAS` with exact `DeletionOperation{OperationID: deletion_operation_id,CommandID: deletion_command_id}` for that job binding/ref/version. It then calls the distinct `MarkCredentialDeletingForAccountDeletion` seam—not `DeleteCredentialIfUnreferenced`—which validates the claimed durable job/lease/operation and all other references, then consumes the prebuilt intent to CAS the object to terminal `deleting` **before** opening, enumerating or revoking tokens. A provisional sign-in that has not reserved loses to the marker; an already-reserved operation must resolve first, and the final promotion-adoption versus deletion-marker CAS has exactly one winner. If the CAS return is ambiguous or the process dies immediately after provider success, the restarted worker uses durable job binding/ref/operation/command plus inventory marker sequence/digest to call `RecoverDeletionCapability` outside the fence; exact recovery is required before token access, and replaying the successful intent remains forbidden. After fence release the worker uses the returned or recovered capability to open the frozen maximum of current+pending tokens and invoke `/auth/revoke` for each. Only after every present token returns HTTP 200 or Apple's documented already-invalid result does it durably call SQL `MarkProviderRevoked`; a crash before that mark may repeat idempotent provider calls. `revoke_succeeded` never calls Apple again: it erases both frozen slots and refreshes/mints a final `DeletionCapability` bound to the still-terminal object's new provider version and marker sequence, durably calls SQL `BeginCleanup`, then calls `Delete(capability)`. That call re-authenticates the exact deleting main object, proves or repairs the separate companion marker from the main object's private last completed-or-abandoned rotation pair, and only then conditionally deletes the exact capability version. The worker confirms absence of the **main** credential object and only afterward calls SQL `Complete`. The replay-marker companion is retained after `Complete`; neither ordinary credential deletion nor this revocation job may erase it. A crash or provider timeout during marker resolve/create/CAS, after marker proof, or during object deletion resumes from the durable job and authenticated main/marker state; a stale main provider version loses without deleting, missing/corrupt credential before provider success never completes, and vault-delete failure remains cleanup-pending with bounded retry metadata. Every SQL and vault transition checks every claim field plus the current unexpired lease/operation version; stale workers cannot mutate the job. Provider error text and plaintext never enter SQL/logs. Local account deletion is final before provider success and retries never restore access. Add `TestReadyToDeletingAccountJobCrashRestartPreservesOperationAndCommand`, `TestDeletionMarkerPrecedesTokenOpenAndRevoke`, `TestAccountDeletionNeverUsesOrphanNegativeJobSeam`, `TestProvisionalSignInBeforeReserveVersusDeletionMarkerOneWinner`, `TestDeletionCrashAfterCASBeforeCapabilityReturnRecoversOutsideFence`, `TestDeletionAmbiguousCASWrongCurrentStateFailsClosed`, `TestDeletionRefreshesCapabilityAfterTokenErase`, `TestDeletionCrashAtEveryFrozenState`, `TestAccountDeletionRepairsReplayMarkerBeforeMainDelete`, and `TestAccountDeletionRetainsReplayMarkerThroughCredentialCleanup`.

The server process owns worker construction, schedule and shutdown, not `account.Service` or deletion HTTP. The same bounded loop inventories encrypted objects and dispatches every `reserved|candidate|retiring|promoted|adopting|aborting` session-rotation state before claiming deletion work: expired `reserved` is abandoned; candidate/retiring finishes idempotent prior revocation and promotion; promoted resumes preparation/confirmation or abandons only after its durable `ReconcileAfter`; adopting calls outside-fence `RecoverAdoptionCapability` from authenticated object state, obtains the sealed expected command hash without retained process memory, then checks the exact authority command receipt/hash. A matching receipt `FinishAdoption`s. A definitely absent receipt may `AbandonAdoption` and revoke only when `now >= ReconcileAfter`, except that the in-flight auth request may abandon immediately after a definite pre-head-CAS failure. Any unavailable, mismatched-head, unreadable or otherwise ambiguous recovery/receipt/head result retries without credential mutation; SQL projection absence alone is never proof. Aborting revokes/erases pending. Hosted mode requires a separate `BT_APPLE_REVOCATION_DATABASE_URL_FILE` login in the `baby_tracker_apple_revoker` NOLOGIN group, opaque `BT_APPLE_REVOCATION_WORKER_ID`, `BT_APPLE_REVOCATION_POLL_INTERVAL` default 5s/range 1s..5m, `BT_APPLE_REVOCATION_LEASE_TTL` default 30s/range 10s..5m, and `BT_CREDENTIAL_ORPHAN_GRACE` default 1h/minimum 11m, plus the P1.7 concrete vault gate and Apple revoker credentials. Compose declares/mounts `apple_revocation_database_url`, sets those exact variables, and `init-users.sh` creates the distinct login membership/grants. The login can execute only declared migration-000007 revocation/orphan worker functions and cannot read/write application, projection or job tables directly; runtime/projector roles cannot execute them. The server starts one in-process loop only after readiness, stops claiming on cancellation, lets the current lease-bounded iteration finish, then shuts down before closing repository/transport. Self-host mode constructs no worker and needs no worker database or Apple credentials. Task 6 proves worker logic, SQL grants, scheduler start/stop, one claimed retry and bounded shutdown with injected deterministic credential-vault and Apple-revoker fakes; production hosted readiness remains false. Task 7 alone supplies and accepts the live hosted vault/provider adapters and then enables the worker in hosted readiness. `smoke.sh` proves self-host absence, hosted missing-secret failure, worker-role direct-DML denial, one fake-adapter claimed retry, and bounded graceful shutdown. Add `TestComposeWiresRevocationWorker`, `TestSmokeExercisesRevocationWorker`, `TestTask6HostedWorkerUsesInjectedFakeAdapters`, `TestHostedWorkerReadinessWaitsForTask7Adapters`, `TestAccountDeletionRequestServiceHasNoAppleCapability`, `TestAppleRevocationWorkerLeastPrivilege`, `TestAppleRevocationShutdownStopsClaims`, `TestRotationReconcilerUsesAuthorityReceipt`, `TestRotationReconcilerDispatchesAdopting`, `TestAdoptingRestartRecoversCapabilityExpectedHashAndReconcileAfterWithoutMemory`, `TestAdoptingReceiptPresentFinishesAdoption`, `TestAdoptingDefiniteAbsentBeforeReconcileAfterRetriesWithoutMutation`, `TestAdoptingDefiniteAbsentAfterReconcileAfterAbandonsAndRevokes`, `TestAdoptingAmbiguousAuthorityAfterReconcileAfterRetriesWithoutMutation`, `TestAdoptingCrashBeforeAuthorityReceiptAbandonsAfterDefiniteAbsence`, `TestAdoptingCrashAfterAuthorityReceiptFinishes`, `TestAdoptingCrashWithAmbiguousReceiptRetries`, `TestRotationReconcilerResumesEveryState`, `TestAccountDeletionAtomicallyQueuesAppleRevocation`, `TestAppleRevocationRetryNeverRestoresAccount`, `TestAppleRevocationCrashBeforeProviderMarkRepeatsSafely`, `TestAppleRevocationCrashAfterProviderMarkSkipsProvider`, `TestAppleRevocationCrashAfterCleanupMarkRetriesDelete`, `TestAppleRevocationCrashAfterDeleteCompletes`, `TestAppleRevocationStaleLeaseDenied`, `TestAppleRevocationMissingOrCorruptCredentialNeverCompletes`, `TestAppleRevocationAmbiguousCompletionIsIdempotent`, and `TestAppleRevocationJobContainsNoPlaintext`. Restored old profiles/identities are scrubbed again during replay.

Unreferenced stable credential objects and incomplete rotation states are reconciled through the P1.4 vault interface extended only by the durable `CredentialRotationState.ReconcileAfter` field above. The provider inventory exposed to `hostedcredentials` contains only opaque object ID/version/key ID/created time for both main credential and replay-marker objects. The cipher-aware adapter loads and AEAD-authenticates each object, validates its embedded kind and binding plus the kind-specific ref/state or marker fields including `ReconcileAfter`, filters replay markers from domain `CredentialInventory`, and only then returns main-object `CredentialMetadata`; corrupt/wrong-AAD inventory fails closed and never returns attacker-supplied bindings. Each domain `CredentialInventory.List` call consumes exactly one bounded provider metadata page with the caller's validated limit in `1..100`, returns only authenticated main credentials from that page, and returns the provider's advanced cursor unchanged. A marker-only provider page may therefore yield zero domain rows with a nonempty cursor; the adapter never loops internally to fill the requested number of main rows, and the worker treats only an empty cursor as exhaustion. Repeated caller pages must advance and produce no duplicate main credential. A marker-only binding therefore produces no maintenance item or per-object worker loop while its cursor still makes bounded inventory progress. Migration 000007 adds worker-private credential-cleanup lease/functions whose typed claim durably returns lease token, dedicated canonical cleanup `OperationID` and `CommandID`, separate opaque/monotonic `OperationVersion`, and the exact account/object/ref/ref-key coordinates; retries return the same IDs. After a grace period greater than the ten-minute stage TTL, the worker calls `PrepareDeletionCAS(...,DeletionOperation{OperationID: claim.OperationID,CommandID: claim.CommandID})` outside the fence, then passes the complete `CredentialCleanupClaim` and intent to generic-orphan-only `security.DeleteCredentialIfUnreferenced`; its validate/reference/mark/finish callbacks query and compare every claim field and current unexpired lease rather than capturing lease state. Under the global exclusive authority fence that deep method repeats the final active-stage, exact authority receipt, projected credential and deletion-job negative checks twice and, while all remain negative, consumes the intent with one bounded conditional CAS to terminal `deleting`. That CAS returns the opaque exact-object/version/operation/command/marker `DeletionCapability`; if its success return is lost, the restarted worker resolves the authenticated deleting state and calls `RecoverDeletionCapability` outside the fence using those durable cleanup IDs and recovered marker fields. Outside the fence, capability `Delete` repairs/proves the replay companion from the authenticated main object's private completed-or-abandoned rotation history before conditionally removing the main object; main-object absence confirmation and finish follow, while the companion remains. `ExecutePrepared` adoption holds the same fence and rejects a deleting marker, so barrier ordering has one winner: adoption commits a reference and cleanup observes it, or cleanup marks deleting and later adoption loses. Authentication can recreate that stable main ID only after confirmed main-object absence and authenticated replay-marker resolution. Incomplete `reserved|candidate|retiring|promoted|adopting|aborting|deleting` main objects instead follow their Task 4 maintenance state and are never classified as plain orphans. Add `TestCredentialAdoptionAndCleanupBarrierAdoptionWins`, `TestCredentialAdoptionAndCleanupBarrierDeletionWins`, `TestOrphanGraceAndDoubleCheck`, `TestOrphanCleanupClaimRejectsStaleLeaseVersionOperationCommandOrBinding`, `TestOrphanRejectsAnyDeletionJobReference`, `TestOrphanDeletingMarkerPrecedesFenceRelease`, `TestReadyToDeletingOrphanCrashRestartPreservesOperationAndCommand`, `TestOrphanRejectsBlankWrongOrStaleRotationOperationIDs`, `TestOrphanCrashAfterDeletingCASRecoversCapability`, `TestOrphanAmbiguousRecoveryFailsClosed`, `TestOrphanDeleteUsesCapabilityVersion`, `TestOrphanDeleteRepairsReplayMarkerBeforeMainDelete`, `TestOrphanRecreateWaitsForConfirmedAbsence`, `TestOrphanInventoryDecryptsBindingBeforeReturn`, `TestOrphanInventoryRejectsForgedMetadata`, `TestCredentialReplayMarkerHiddenFromInventory`, `TestMarkerOnlyInventoryPageAdvancesWithoutWorkerLoop`, and `TestOrphanInventoryContainsNoSecretMaterial`; P1.7's adapter gate must run these tests before hosted readiness can pass.

- **Task 6 orphan-worker correction:** after a generic orphan wins the deletion
  barrier, the worker revalidates the complete live cleanup claim immediately
  before capability-bound token erasure, then deletes the exact main object.
  Lease expiry at that point leaves the deleting object and its tokens intact
  for a newly claimed worker; it never calls the Apple provider for an orphan.
- [ ] **Step 5 (4 min): Implement credential-loss owner freeze and invalidation.**

```go
type SecurityInvalidation struct { AccountID,FamilyID,SessionID string; Revision uint64 }
type InvalidationSink interface { Invalidate(context.Context,SecurityInvalidation) }
func (s *Service) Subscribe(InvalidationSink) (unsubscribe func())
```

`identity_unavailable` revokes that account's sessions, freezes owner-only operations and preserves the family/current owner. Other active caregivers can still read/add children/mutate records; nobody is promoted. A fresh valid Apple proof for that same nondeleted identity atomically commits `identity_session_issued` with restored authority plus a new session and no `account_registered` effect, allowing owner duties again without reviving an old session. Emit `SecurityInvalidation` immediately after authoritative head CAS and again after projection if needed, with bounded subscribers outside database locks. Subscriber failure/loss cannot unblock traffic; tests drop every hint and still see denied authentication/guard calls.

- [ ] **Step 6 (4 min): Add lifecycle distinction and deadline tests.**

```go
func TestCredentialLossDepartureAndDeletionAreDistinct(t *testing.T) {
    h:=newLifecycleFixture(t)
    before:=h.RecordCount()
    h.LoseOwnerAppleIdentity()
    if _,err:=h.Family.Authorize(t.Context(),h.Caregiver,h.OwnerFamilyID(),"mutate_record"); err!=nil { t.Fatal(err) }
    if h.RecordCount()!=before { t.Fatal("credential loss erased family data") }
    if _,err:=h.Family.Transfer(t.Context(),h.Caregiver,h.CommandID(1),h.Caregiver.AccountID); security.Code(err)!="owner_required" { t.Fatal("caregiver auto-promoted") }
    _,err:=h.Accounts.Delete(t.Context(),h.Caregiver,h.CommandID(2),true)
    if err!=nil { t.Fatal(err) }
    h.RestoreOldDatabase()
    for _,label:=range h.Labels() { if label=="Deleted test caregiver" { t.Fatal("restored deleted profile leaked") } }
}
```

Add helper `OwnerFamilyID() string` to `lifecycleFixture`. Add `TestRecoveryAtDeadlineDenied`, `TestRecoveryVersusForfeitRace`, `TestOwnerAccountDeletionForfeitsClaim`, `TestOwnerCannotDeleteActiveAccount`, `TestDeletedAccountCannotReuseOldSessions`, `TestDeparturePreservesAppleIdentity`, `TestAppleRevokeUnavailableNeverRestoresAccount`. Each checks both external state and SQL projection, then repeats after replay from older projection.

- [ ] **Step 7 (4 min): Implement ID-bound operator confirmation.**

```go
func (s *Service) PrepareRecovery(context.Context,RecoveryProposal) (Confirmation,error)
func (s *Service) RecoverOwner(context.Context,RecoveryProposal,Confirmation) (security.CommandResult,error)
type confirmationEnvelope struct {
 Version string `json:"version"`
 Operation string `json:"operation"`
 OperatorID string `json:"operator_id"`
 Proposal json.RawMessage `json:"proposal"`
 ExpiresAt string `json:"expires_at"`
}
func confirmationDigest(key []byte,operation,operatorID string,canonicalProposal []byte,expires time.Time) (string,error) {
 if len(key)!=32 || !validUUID(operatorID) || !json.Valid(canonicalProposal) || expires.Location()!=time.UTC || expires.Nanosecond()!=0 { return "",ErrInvalidConfirmation }
 envelope,err:=json.Marshal(confirmationEnvelope{Version:"v1",Operation:operation,OperatorID:operatorID,
  Proposal:append(json.RawMessage(nil),canonicalProposal...),ExpiresAt:expires.Format(time.RFC3339)})
 if err!=nil { return "",ErrInvalidConfirmation }
 m:=hmac.New(sha256.New,key)
 m.Write([]byte("baby-tracker-operator-confirmation-v1\x00")); m.Write(envelope)
 return hex.EncodeToString(m.Sum(nil)),nil
}
```

`PrepareRecovery` requires nonempty configured approved policy ID equal to proposal, existing frozen owner, distinct active caregiver target, positive exact expected revision, reason `owner_identity_unavailable`, and opaque evidence reference matching `[A-Za-z0-9_-]{8,128}`. It returns HMAC-SHA256 over the domain-separated canonical envelope above using the mounted operator confirmation key. `canonicalProposal` is `json.Marshal` of the fixed tagged structs above/below, preserving field order, no maps, no omitted fields and canonical UTC-second strings for times. The envelope binds version, exact operation (`ownership-recover`, `entitlement-grant` or `entitlement-suspend`), configured operator UUID, complete proposal and exact UTC 5-minute expiry. CLI prepare prints `{proposal,confirmation:{digest,expires_at}}`, and commit passes both confirmation fields rather than attempting to infer expiry. `RecoverOwner` rejects absent/changed expiry, non-UTC/subsecond expiry, expired confirmation, expiry beyond now+5min, wrong configured operator ID, incorrect operation, modified proposal and stale revision under the exclusive fence, then commits atomic old-owner demotion/new-owner promotion plus operator audit. Record only `HMAC-SHA256(key,"baby-tracker-operator-evidence-v1\x00" || evidenceReference)`, operator opaque ID, IDs, reason and policy ID; evidence documents themselves are stored outside this app under operator policy. Wrong argument binding can authorize the wrong transfer or privilege change, so every proposal field, operation, operator ID and expiry must alter the HMAC.

Operator configuration is exact: `BT_OPERATOR_ID` is a canonical nonzero UUID;
`BT_OPERATOR_OWNERSHIP_POLICY_ID_FILE`,
`BT_OPERATOR_ENTITLEMENT_POLICY_ID_FILE`, and
`BT_OPERATOR_CONFIRMATION_KEY_FILE` are the only accepted policy/key inputs.
Each policy file independently enables its command family; if either is set,
`BT_OPERATOR_ID` and `BT_OPERATOR_CONFIRMATION_KEY_FILE` are required. If a
policy file is absent, only that dangerous command family is disabled. Every
file is opened with no symlink following,
must resolve to the same regular file before/after open, and must have exact
Unix mode `0600`; any group/world bit or another owner mode is rejected. Policy
files contain one `[A-Za-z][A-Za-z0-9._-]{0,127}` value with one optional final
LF. The confirmation-key file contains exactly 64 lowercase hexadecimal
characters encoding 32 bytes, with one optional final LF. Direct-value fallback
environment variables are forbidden in every mode. Add
`TestOperatorSecretFilesRequire0600RegularNonsymlink`,
`TestOperatorConfirmationBindsOperatorIDOperationProposalAndExpiry`,
`TestOperatorConfirmationRejectsWrongConfiguredOperator`, and
`TestOperatorEvidenceDigestKnownAnswer`. With the test key below, the evidence
digests are `case_test_0001` →
`99beb25654c5a1b5e54f0560ad6cc46c59edcd1e1074b0c7562cd37ade9fd672`,
`entitlement_test_0001` →
`ae11cb7ca4a4db1a8cc773a778603f2b71018f37dd547c0f4a05d43bd8aabd8e`,
and `entitlement_test_0002` →
`883948f1dd98b409a5ba5288fbdd59737744b84ba4047f2cdb1fa4b8770166d0`.

- [ ] **Step 8 (4 min): Add operator CLI and approval-policy runbook.**

```go
type EntitlementProposal struct {
 CommandID string `json:"command_id"`
 FamilyID string `json:"family_id"`
 Operation string `json:"operation"` // grant or suspend
 Key string `json:"key"`
 Source string `json:"source"`
 ExpiresAt time.Time `json:"expires_at"`
 PolicyID string `json:"policy_id"`
 EvidenceReference string `json:"evidence_reference"`
 ReasonCode string `json:"reason_code"`
 ExpectedRevision uint64 `json:"expected_revision"`
}
func (s *Service) PrepareEntitlement(context.Context,EntitlementProposal) (Confirmation,error)
func (s *Service) GrantEntitlement(context.Context,EntitlementProposal,Confirmation) (security.CommandResult,error)
func (s *Service) SuspendEntitlement(context.Context,EntitlementProposal,Confirmation) (security.CommandResult,error)
```

Exact command grammar is `ownership-recover prepare|commit` and `entitlement grant|suspend prepare|commit`; **both** phases require every proposal argument. Commit additionally requires `--confirmation-digest` and `--confirmation-expires-at`; no abbreviated `--confirmation` or server-inferred expiry is accepted. Suspension binds and preserves the current `hosted_beta` source/expiry as explicit audited inputs, checks them at the positive expected revision and changes only status; it never rewrites the source to `operator`. Both entitlement changes use ledger commands and the same confirmation machinery; no direct SQL edits. Add the following literal examples to `docs/operations/ownership-recovery.md` and byte-equivalent argv arrays to `operator-cli-v1.json`. These are deliberately test-only identities with configured operator ID `90000000-0000-4000-8000-000000000001`, a test clock fixed at `2026-09-20T12:00:00Z` and a test confirmation key of 32 bytes `0x42`; run each scenario independently from positive revision 42. Real operators copy their actual prepare response's digest/expiry after reviewing their own policy/evidence.

```bash
rtk proxy sh -c 'cd Server && go run ./cmd/baby-tracker-admin ownership-recover prepare --command-id 30000000-0000-4000-8000-000000000061 --family-id 20000000-0000-4000-8000-000000000001 --old-owner-id 10000000-0000-4000-8000-000000000001 --new-owner-id 10000000-0000-4000-8000-000000000002 --policy-id owner-policy-test-v1 --evidence-reference case_test_0001 --expected-revision 42 --reason-code owner_identity_unavailable'
rtk proxy sh -c 'cd Server && go run ./cmd/baby-tracker-admin ownership-recover commit --command-id 30000000-0000-4000-8000-000000000061 --family-id 20000000-0000-4000-8000-000000000001 --old-owner-id 10000000-0000-4000-8000-000000000001 --new-owner-id 10000000-0000-4000-8000-000000000002 --policy-id owner-policy-test-v1 --evidence-reference case_test_0001 --expected-revision 42 --reason-code owner_identity_unavailable --confirmation-digest 0e9030b6119c2f6834643fc0c17f5681d1ea044ab618e841d2c652b6f02ae37a --confirmation-expires-at 2026-09-20T12:05:00Z'
rtk proxy sh -c 'cd Server && go run ./cmd/baby-tracker-admin entitlement grant prepare --command-id 30000000-0000-4000-8000-000000000062 --family-id 20000000-0000-4000-8000-000000000001 --key family_sync --source hosted_beta --expires-at 2027-09-20T12:00:00Z --policy-id entitlement-policy-test-v1 --evidence-reference entitlement_test_0001 --expected-revision 42 --reason-code beta_extension'
rtk proxy sh -c 'cd Server && go run ./cmd/baby-tracker-admin entitlement grant commit --command-id 30000000-0000-4000-8000-000000000062 --family-id 20000000-0000-4000-8000-000000000001 --key family_sync --source hosted_beta --expires-at 2027-09-20T12:00:00Z --policy-id entitlement-policy-test-v1 --evidence-reference entitlement_test_0001 --expected-revision 42 --reason-code beta_extension --confirmation-digest 35c9bfa1a62100f8cc06d8d04f74feafd46cde8b944b40826e2e1d5b402f3e82 --confirmation-expires-at 2026-09-20T12:05:00Z'
rtk proxy sh -c 'cd Server && go run ./cmd/baby-tracker-admin entitlement suspend prepare --command-id 30000000-0000-4000-8000-000000000063 --family-id 20000000-0000-4000-8000-000000000001 --key family_sync --source hosted_beta --expires-at 2027-09-20T12:00:00Z --policy-id entitlement-policy-test-v1 --evidence-reference entitlement_test_0002 --expected-revision 42 --reason-code abuse_suspension'
rtk proxy sh -c 'cd Server && go run ./cmd/baby-tracker-admin entitlement suspend commit --command-id 30000000-0000-4000-8000-000000000063 --family-id 20000000-0000-4000-8000-000000000001 --key family_sync --source hosted_beta --expires-at 2027-09-20T12:00:00Z --policy-id entitlement-policy-test-v1 --evidence-reference entitlement_test_0002 --expected-revision 42 --reason-code abuse_suspension --confirmation-digest 5ff8150ff438cc3a471ccba9b6bb23529de13080e19d97339518c9b519065dcc --confirmation-expires-at 2026-09-20T12:05:00Z'
```

The runbook states mechanism, required policy/evidence fields, no automatic promotion and disabled real use until operator approval policy exists. Its sequence is preflight verify/reconcile, prepare, independent operator evidence verification, review exact printed proposal, commit both returned confirmation fields and postflight authorization checks. No mail/message is sent by the tool.

- [ ] **Step 8a (4 min): Execute the documented operator syntax as a regression.**

```go
func TestDocumentedOperatorCLI(t *testing.T) {
 for _,name:=range []string{"ownership-recover","entitlement-grant","entitlement-suspend"} {
  t.Run(name,func(t *testing.T){
   h:=newOperatorCLIFixture(t,name)
   prepared:=h.RunPrepareFromFixture()
   if prepared.Confirmation.Digest!=h.Fixture.ExpectedDigest || !prepared.Confirmation.ExpiresAt.Equal(h.Fixture.ExpiresAt) { t.Fatal("documented confirmation mismatch") }
   if err:=h.RunCommitFromFixture(); err!=nil { t.Fatal(err) }
   if h.CommittedRevision()!=43 || h.OperatorAuditCount()!=1 { t.Fatal("CLI did not commit exactly one audited command") }
  })
 }
}
```

`operatorCLIFixture` owns real temporary authority/PostgreSQL, fixed clock/key/operator ID and `CLIExample{PrepareArgs,CommitArgs []string; OperatorID,ExpectedDigest string; ExpiresAt time.Time}` decoded from `operator-cli-v1.json`; `PreparedCLI{Proposal json.RawMessage; Confirmation operator.Confirmation}` matches the actual safe output. Both operator fixtures record operator ID `90000000-0000-4000-8000-000000000001`, positive expected revision `42`, and `hosted_beta` as the source for grant and suspension. Define `newOperatorCLIFixture(*testing.T,string) *operatorCLIFixture`, the four methods used above, and `RunWithServices(context.Context,[]string,OperatorCommands,io.Writer) error` in the admin command package; put this regression in `Server/cmd/baby-tracker-admin/cli_examples_test.go` so it tests package-main parser code without importing an executable as a library. `OperatorCommands` declares the exact five prepare/commit methods above; tests inject the real service with fixed clock/key/operator ID, while main injects mounted operator credentials. The test uses the **literal** argv arrays documented above, never directly calls `RecoverOwner` in place of the CLI parser. Add `TestDocumentedOperatorMissingOrChangedFields`: remove each required flag one at a time, change expiry/policy/evidence/command ID/revision/operator ID, and switch grant to suspend with a previously prepared digest; assert failure and unchanged head/audit count. Add `TestOperatorEntitlementPreservesHostedBetaSource`, `TestOperatorRejectsZeroExpectedRevision`, and a digest known-answer test for all three displayed hashes. RED: `rtk proxy sh -c 'cd Server && go test ./internal/operator ./cmd/baby-tracker-admin -run "TestDocumentedOperatorCLI|TestDocumentedOperatorMissingOrChangedFields|TestOperatorConfirmationVectors|TestOperatorEntitlementPreservesHostedBetaSource|TestOperatorRejectsZeroExpectedRevision" -count=1'`, expected absent flags or confirmation signature mismatch; GREEN repeats after parser/signing changes. Stage fixtures, test and runbook in this task, and replace any reserved event vector using source `operator` or expected revision zero.

- [ ] **Step 9 (3 min): Test operator restrictions.**

```go
func TestOwnershipRecoveryRequiresApprovedPolicy(t *testing.T) {
    svc,err:=operator.New(operator.Options{PolicyID:"",Now:time.Now})
    if err!=nil { t.Fatal(err) }
    _,err=svc.PrepareRecovery(t.Context(),operator.RecoveryProposal{})
    if security.Code(err)!="operator_policy_required" { t.Fatal("recovery accepted without operator policy") }
}
```

`operator.Options` mirrors the Service fields. Add tests for missing evidence, expired/modified confirmation, target outside family, target inactive, unfrozen owner, stale revision, replayed command, HTTP principal claiming operator and unaudited entitlement change. Positive recovery test seeds a test-only policy ID `test-policy-v1`, performs prepare+commit and asserts exactly one owner/one ledger event plus audit digest. This is mechanism acceptance, not approval of a real-world identity-verification policy.

- [ ] **Step 10 (4 min): Wire lifecycle HTTP contract.**

```go
func accountDeleteHandler(*account.Service) http.HandlerFunc
func familyDeleteHandler(*family.Service) http.HandlerFunc
func familyRecoveryStatusHandler(*family.Service) http.HandlerFunc
func familyRecoverHandler(*family.Service) http.HandlerFunc
func familyForfeitHandler(*family.Service) http.HandlerFunc
```

Register `POST /v1/account/delete`, `POST /v1/family/delete`, `GET /v1/family/recovery`, `POST /v1/family/recover`, `POST /v1/family/recovery/forfeit`. Mutation requests have metadata, command_id and confirmed:true. Deletion response contains family_id/state/purge_after/can_recover, no account token. Status is account-authenticated and accepts only the claim's own family. Codes: `confirmation_required`, `ownership_transfer_required`, `recovery_only`, `recovery_expired`, `recovery_forfeited`, `owner_identity_unavailable`, `operator_policy_required`, `stale_security_revision`, `invalid_confirmation`. Account deletion response includes self-host `apple_authorization_revocation:"manual_required"` or hosted `"pending"`; the request never invokes Apple or observes worker credentials. Manual Apple-account Settings guidance is documented for Plan 3 UI, never claimed revoked in self-host mode.

- [ ] **Step 11 (3 min): Run GREEN and full current server suite.**

Run: `rtk proxy bash Server/scripts/test-identity.sh ./internal/account/... ./internal/family/... ./internal/operator/... ./internal/security/... ./internal/httpapi/... ./internal/config/... ./cmd/...`; then `rtk proxy bash Server/scripts/test-identity.sh`; then run the real-PostgreSQL-role revocation-worker denial matrix and fake-provider-adapter scheduler/Compose shutdown test. The gate does not claim live hosted vault/provider acceptance.

Expected: PASS, strict 30-day deadline and slot invariants, deleted profile absent after restore, owner loss leaves caregiver daily work available, CLI never performs an unconfirmed/unrecorded structural change.

First commit final 000007, complete v7 privilege snapshot/scenarios, and manifest `[6,7]` without lifecycle feature dependency. Review it and use that accepted commit as `BT_MIGRATION_BASE_REF`. Then commit final additive 000008, complete v8 privilege snapshot/scenarios, and manifest `[7,8]` without lifecycle feature dependency; review it and capture `BT_MIGRATION_PRIOR_REF`. Implement lifecycle/operator behavior in the third `[8,8]` commit and capture `BT_MIGRATION_FEATURE_REF`. Run prepared mode with all three refs; the schema-7 predecessor family reads pass at schemas 7 and 8, then feature lifecycle behavior passes at 8, with archives unchanged and structural denial retained.

- [ ] **Step 12 (2 min): Commit lifecycle/operator feature and run prepared overlap.**

```bash
rtk git add Server/internal/account Server/internal/family/lifecycle.go Server/internal/family/lifecycle_test.go Server/internal/security Server/internal/operator Server/internal/config/lifecycle.go Server/internal/config/lifecycle_test.go Server/internal/httpapi Server/internal/protocol Server/internal/migrate/compatibility.json Server/internal/tenant/context_test.go Server/cmd/baby-tracker-admin Server/cmd/baby-tracker-server Server/deploy/postgres/init-users.sh Server/compose.yaml Server/README.md Server/api docs/operations/ownership-recovery.md
rtk git commit -m "feat(server): enforce deletion recovery and audited owner recovery"
BT_MIGRATION_FEATURE_REF=$(rtk git rev-parse HEAD)
rtk proxy bash Server/scripts/check-migration-overlap.sh --base-ref "$BT_MIGRATION_BASE_REF" --prior-ref "$BT_MIGRATION_PRIOR_REF" --feature-ref "$BT_MIGRATION_FEATURE_REF" --next-version 8 --prior-min 7 --prior-max 8 --next-min 8 --next-max 8
```

### Task 7: Conform hosted storage ports and prove the complete identity slice (P1.7)

**Files:**

- Create: `Server/internal/security/hosted/contracts.go`, `ledger.go`, `head.go`, `adapter_test.go`; `Server/internal/security/storetest/conformance.go`; `Server/internal/identity/hostedcredentials/contracts.go`, `vault.go`, `vault_test.go`; `Server/internal/config/security.go`, `security_test.go`; `Server/tests/identity/lifecycle_test.go`, `restore_test.go`, `contract_test.go`; `docs/operations/security-state.md`.
- Modify: `Server/internal/config/config.go`, `auth.go`, `config_test.go`; `Server/internal/identity/credentialtest/conformance.go`, `credentials_conformance_test.go`; `Server/compose.yaml`, `Server/Dockerfile`, `Server/README.md`, `Server/scripts/smoke.sh`, `smoke_test.go`, `Server/deploy/postgres/init-users.sh`, `Server/scripts/check-principals.sh`; `Server/cmd/baby-tracker-server/main.go`; `Server/internal/httpapi/router.go`, `Server/internal/protocol/types_test.go`.
- Test: common store contract against filesystem and deterministic provider doubles, real PostgreSQL lifecycle/restore suite, production issuer exclusion, complete API route/fixture/privacy checks and container smoke.

**Interfaces:**

```go
// hosted/contracts.go: no cloud SDK/provider credentials in domain packages.
var (
    ErrObjectNotFound = errors.New("hosted object not found")
    ErrObjectExists = errors.New("hosted object exists")
    ErrHeadNotFound = errors.New("hosted head not found")
    ErrScratchFull = errors.New("hosted replay scratch full")
)
type ObjectVersion struct { Key,Version,KeyID string; Ciphertext []byte; SHA256 [32]byte }
type ImmutableObjects interface {
    Create(context.Context,ObjectVersion) error // create-if-absent; no overwrite/delete
    Read(context.Context,string) (ObjectVersion,error)
}
type EncryptedHeadValue struct { Version,KeyID string; Ciphertext []byte }
type AtomicHeads interface {
    Load(context.Context) (EncryptedHeadValue,error)
    CompareAndSwap(context.Context,string,EncryptedHeadValue) (bool,error) // expected provider version
}
type EnvelopeCipher interface {
    Seal(context.Context,string,[]byte,[]byte) ([]byte,error) // keyID, plaintext, associated data
    Open(context.Context,string,[]byte,[]byte) ([]byte,error)
}
type ReplayIndexEntry struct { Generation,Revision uint64; EventHash [32]byte }
type ReplayScratchRequest struct { Namespace string; MaxBytes int64 }
type EncryptedReplayScratch interface {
    Begin(context.Context,ReplayScratchRequest) (ReplayScratchSession,error)
}
type ReplayScratchSession interface {
    Append(context.Context,ReplayIndexEntry) error
    WalkReverse(context.Context,func(ReplayIndexEntry)error) error
    Close() error // erases all session data, including failure/cancellation paths
}
type Properties struct {
    Provider,Region,LedgerFailureDomain,HeadFailureDomain,LedgerKeyID,HeadKeyID string
    Encrypted,ImmutableLedger,ReplicatedHead,ConditionalCreate,LinearizableCAS bool
}
type PropertyPolicy struct {
    AllowedProviders,AllowedRegions []string
    LedgerKeyID,HeadKeyID string
}
func ValidateProperties(Properties,PropertyPolicy) error
func NewLedger(ImmutableObjects,EnvelopeCipher,EncryptedReplayScratch,instanceID,keyID string,maxScratchBytes int64) (security.SecurityLedger,error)
func NewHeads(AtomicHeads,EnvelopeCipher,instanceID,keyID string) (security.LedgerHeadStore,error)
// storetest/conformance.go
type Store struct {
    Ledger security.SecurityLedger
    Heads security.LedgerHeadStore
    Reopen func(*testing.T) (security.SecurityLedger,security.LedgerHeadStore)
}
type Factory func(*testing.T) Store
func Run(t *testing.T,newStore Factory)
// identity/hostedcredentials/contracts.go: provider secret-store seam.
type CredentialEnvelopeHeader struct { Schema string; EnvelopeSequence uint64 }
type SecretObject struct { ID,Version,KeyID string; Header CredentialEnvelopeHeader; Ciphertext []byte; SHA256 [32]byte }
type SecretMetadata struct { ObjectID,Version,KeyID string; CreatedAt time.Time }
type SecretStore interface {
    Resolve(context.Context,string) (SecretObject,bool,error)
    Create(context.Context,SecretObject) error // conditional create; never overwrite
    CompareAndSwap(context.Context,string,string,SecretObject) (bool,error) // object ID, expected version
    Delete(context.Context,string,string) error // object ID plus expected version; absent is success
    ListMetadata(context.Context,time.Time,string,int) ([]SecretMetadata,string,error)
}
type CredentialReplayMarkerPurger interface {
    PurgeReplayMarker(context.Context,identity.CredentialBinding) error
}
type CredentialAdapters struct {
    Vault identity.CredentialVault
    Inventory identity.CredentialInventory
    ReplayMarkers CredentialReplayMarkerPurger
}
func NewCredentialAdapters(SecretStore,hosted.EnvelopeCipher,string) (CredentialAdapters,error)
```

Consumes P1.1 unchanged ledger/head contracts, P1.2 single-authority-host fence and the `identity.CredentialVault`/`CredentialRef` interfaces already owned and implemented against fakes in P1.4 `credentials.go`; P1.7 neither moves nor redeclares them. `hostedcredentials.NewCredentialAdapters` is the single cipher-aware constructor for the concrete encrypted vault, filtered credential inventory and narrow future-purge seam over the existing provider-neutral `SecretStore`; the marker needs no new provider operation, and there is no store-only inventory constructor. The P1.4 method sets remain unchanged after Task 6 adds the frozen `CredentialRotationState.ReconcileAfter` field amendment: marker persistence, replay checks and main-object recreation are private adapter behavior, while `ReplayMarkers` is exposed only to the non-HTTP final-purge composition. Task 7 must persist and AEAD-authenticate `ReconcileAfter`, reopen it unchanged, run `TestHostedCredentialAdapterAuthenticatesReconcileAfter`, and run `TestHostedWorkerLiveAdapterLifecycle` before hosted readiness may enable the revocation worker.

Every main credential and replay-marker object carries the same bounded clear header with `Schema` exactly `credential-envelope-v1` and nonzero monotonically increasing `EnvelopeSequence`; its remaining clear coordinates are only opaque object ID, envelope key ID, provider version and ciphertext. Main state transitions serialize binding, ref, phase, private last completed-or-abandoned rotation history and operation/token state inside one canonical bounded plaintext. A replay marker serializes only authenticated inner kind `credential-replay-marker-v1`, exact main binding, that same last completed-or-abandoned operation/command pair and the bounded sequence frozen above. Both use true provider CAS and AEAD associated data exactly `frame("baby-tracker-credential-envelope-aad-v1", objectID, keyID, header.Schema, uint64be(header.EnvelopeSequence))`; all strings are canonical UTF-8 and framing is unsigned-32-bit length framing. Provider `Version` is only an opaque CAS token and is never AAD. After decrypt, the adapter dispatches by authenticated inner shape/kind and verifies the embedded binding derives the outer ID under the matching main or replay-marker domain. For a main object it additionally requires embedded ref ID equal to the main ID, valid embedded ref key ID and exact state/history invariants; for a marker it requires no ref or token field and exact inner/outer sequence equality. Unknown/ambiguous kinds and a wrong envelope key, outer object ID/key ID/header schema/sequence, binding, ref, AAD or marker field fail closed.

`ListMetadata` supplies only opaque object ID/version/key ID/created time and no private kind/binding/ref/phase. `CredentialInventory.List` validates `limit` in `1..100`, invokes `SecretStore.ListMetadata` exactly once with that same limit and the caller cursor, then `Resolve`s, decrypts and authenticates every object in that one returned page. It filters valid replay markers, may return an underfilled or empty main-credential slice, and returns the provider cursor exactly; it never performs another provider list call to fill the domain limit. Across repeated calls, marker-only pages advance to later main objects without looping or duplicating results. Main `Resolve` targets only the deterministic main object ID. `ReserveRotation` additionally targets and authenticates the deterministic marker ID before conditionally recreating an absent main object; companion sequence remains adapter-private and only the marker's operation/command pair seeds private main history, so the returned reserved state's deletion marker fields stay zero. A single private `ensureReplayMarker(main,expectedVersion)` algorithm is shared by `CompleteMaintenance`'s no-current deletion and capability `Delete`: it authenticates the main history, creates a missing companion, accepts an exact companion, or CAS-repairs a stale one, then reconciles every ambiguous provider result to exact kind/binding/pair/sequence before permitting the conditional main delete. It never derives replay history from the deletion job's operation/command. At most one marker exists per binding because only its deterministic ID is conditionally created then overwritten by expected-version CAS. Fault injection covers timeout before/after marker resolve, conditional create, repair CAS and main delete; each restart either proves the expected marker and resumes the exact main deletion or fails closed without deleting main.

`PurgeReplayMarker` resolves the deterministic marker ID, decrypts and validates the exact kind/binding/AAD, and conditionally deletes only that authenticated provider version; an ambiguous delete re-resolves, treating absence as success and any changed object/version as a retryable conflict. Ordinary `CredentialVault.Delete`, `ConfirmAbsent`, inventory and maintenance never invoke this **purge** seam: both deletion methods ensure/retain the marker, while only final purge removes it. Task 6 retains the marker through account credential revocation and absence; P4 may receive this narrow seam only after its authoritative final account-generation retention gate proves credential revocation/absence and that no restore or re-registration dependency remains. Family deletion, recovery and 30-day family purge alone never satisfy that account-scoped gate and must retain the marker when account authority remains. A later actor generation has a distinct account ID, main object and replay-marker ID; final purge deletes only the eligible old generation.

The common `identity/credentialtest` harness must require a nonnil `Reopen`/restart factory that constructs a new vault and inventory instance over the **same durable backing store and keys**; returning the same adapter pointer or a fresh empty store is not conformance. Its abandonment case restarts after marker-written/main-absent, proves exact and partial operation/command replay denial occurs before provider exchange, proves a fresh pair recreates the main credential with public deletion marker fields still zero, and proves stale deletion coordinates fail after reclaim. Every nonterminal-phase case also reopens and proves the exact authenticated `ReconcileAfter` survives without extension. It also completes one rotation through `MarkCommitted`, transitions `ready` through `MarkDeleting`, reopens around marker repair and `Delete`, and requires the completed pair to survive main absence; a separate no-prior case freezes the initial in-progress rotation and proves its operation/command, not the deletion pair, becomes replay history. The hosted adapter runs that common suite plus `TestCredentialEnvelopeAADKnownAnswer`, `TestCredentialEnvelopeRoundTripFromMetadataOnly`, `TestCredentialEnvelopeRejectsWrongKey`, `TestCredentialEnvelopeRejectsWrongOuterObjectOrKeyID`, `TestCredentialEnvelopeRejectsWrongHeaderSchemaOrSequence`, `TestCredentialEnvelopeRejectsEmbeddedBindingRefOrPhaseMismatch`, `TestHostedCredentialAdapterAuthenticatesReconcileAfter`, `TestCredentialReplayObjectIDKnownAnswerAndDistinct`, `TestCredentialReplayMarkerCASKeepsOneObject`, `TestCredentialReplayMarkerCrashBeforeCredentialDeleteResumes`, `TestCredentialReplayMarkerSurvivesAdapterRestart`, `TestCredentialReplayMarkerWrongAADFailsClosed`, `TestCredentialReplayMarkerHiddenFromInventory`, `TestCredentialInventoryOneProviderPagePerCall`, `TestCredentialInventoryMarkerOnlyPagesAdvanceCursor`, `TestCredentialInventoryFilteredPagesDoNotDuplicateMainObjects`, `TestCredentialReplayMarkerStaleDeletionAfterReclaim`, `TestReclaimedReservedStateKeepsDeletionMarkerFieldsZero`, `TestReadyDeleteRepairsMissingReplayMarker`, `TestReadyDeleteRepairsStaleReplayMarker`, `TestCommittedReplayMarkerSurvivesDeleteRestart`, `TestInitialInProgressDeletePreservesAbandonedReplayHistory`, `TestAbandonedReplayMarkerSurvivesMaintenanceDeleteRestart`, `TestReplayMarkerProviderTimeoutMatrix`, `TestCredentialReplayMarkerPurgeIsAuthenticatedAndIdempotent`, `TestCredentialInventoryConstructorRequiresCipher`, `TestCredentialInventoryMetadataIsOpaque`, and `TestHostedWorkerLiveAdapterLifecycle`. The timeout matrix injects before and after marker resolve/create/CAS and main delete for both deletion entry points and proves main is never absent until the exact companion is durable. The P1.7 hosted startup gate requires a registered concrete provider `SecretStore`, credential-vault/rotation conformance including durable restart and authenticated `ReconcileAfter`, the live Apple older-token revocation/worker lifecycle gate, and real operator values before it may clear `hosted_adapter_unavailable` or `hosted_credential_rotation_unverified`. Self-host mode never constructs this adapter or hosted Apple transport. Provider adapters must supply authoritative conditional-create/CAS semantics; ordinary overwrite or eventually consistent check-then-put cannot satisfy these interfaces.

- [ ] **Step 1 (3 min): Write provider-boundary failures before adapter code.**

```go
func TestHostedCannotFallBackToFilesystem(t *testing.T) {
    env:=validSelfHostConfig(t)
    env["BT_MODE"]="hosted"
    _,err:=config.Load(func(k string)string{return env[k]})
    if err==nil { t.Fatal("hosted mode accepted only local security stores") }
}
func TestHostedStoresRequireIndependentHead(t *testing.T) {
    p:=hosted.Properties{Provider:"test",Region:"eu-test-1",LedgerFailureDomain:"same",HeadFailureDomain:"same",Encrypted:true,ImmutableLedger:true,ReplicatedHead:true,ConditionalCreate:true,LinearizableCAS:true}
    if err:=hosted.ValidateProperties(p); err==nil { t.Fatal("head shared ledger failure domain") }
}
```

Define `validSelfHostConfig(*testing.T) map[string]string` by the existing config-test helper, using temporary valid mounted peppers/state files. No real hosted credentials are involved.

- [ ] **Step 2 (2 min): Run RED.**

Run: `rtk proxy sh -c 'cd Server && go test ./internal/config ./internal/security/hosted -run "TestHostedCannotFallBackToFilesystem|TestHostedStoresRequireIndependentHead" -count=1'`.

Expected: FAIL because hosted contracts/configuration validation do not exist or current hosted config accepts filesystem-only stores.

- [ ] **Step 3 (4 min): Implement encrypted immutable-object and independent-head wrappers.**

```go
func NewLedger(ImmutableObjects,EnvelopeCipher,EncryptedReplayScratch,instanceID,keyID string,maxScratchBytes int64) (security.SecurityLedger,error)
func NewHeads(AtomicHeads,EnvelopeCipher,instanceID,keyID string) (security.LedgerHeadStore,error)
func ValidateProperties(Properties,PropertyPolicy) error
```

Use candidate generation/revision/hash as immutable object key and associated data `(instanceID,generation,revision,eventHash)`. Encrypt event bytes with external key ID; verify decrypted canonical bytes/hash/chain before projection. `NewHeads` encrypts `{generation,revision,event_hash,instance_id}` with head-specific external key and uses the provider's version for CAS; reject rollback/lower generation, lower revision within a generation and same-generation/same-revision different hash. P1 accepts only initialized generation 1; Plan 4 extends verified generation transitions through signed checkpoints before retirement. `ValidateProperties` requires every boolean true, approved European region from operator policy, nonempty distinct failure-domain IDs and explicit provider identity. Values are checked configuration assertions and must be verified against actual provider policy in the future concrete adapter gate; a mock returning them proves no production encryption/immutability claim.

- [ ] **Step 4 (4 min): Implement store conformance fixture and fault matrix.**

```go
func TestFilesystemStoreContract(t *testing.T) {
    storetest.Run(t,func(t *testing.T) storetest.Store {
        ledgerDir,headDir:=filepath.Join(t.TempDir(),"ledger"),filepath.Join(t.TempDir(),"head")
        s,err:=localstore.Open(ledgerDir,headDir)
        if err!=nil { t.Fatal(err) }
        if err:=s.InitializeEmpty(t.Context()); err!=nil { t.Fatal(err) }
        return storetest.Store{Ledger:s.Ledger,Heads:s.Heads,Reopen:func(t *testing.T)(security.SecurityLedger,security.LedgerHeadStore){
            reopened,err:=localstore.Open(ledgerDir,headDir); if err!=nil { t.Fatal(err) }
            return reopened.Ledger,reopened.Heads
        }}
    })
}
```

`storetest.Run` writes two canonical candidates with known chain hashes, proves append idempotency/different-byte rejection, one-winner concurrent CAS, ignored suffix, exact-prefix read, changed/gapped/truncated chain rejection and post-write timeout reconciliation. `TestHostedProviderDoubleContract` runs the same cases against in-memory provider doubles whose CAS is genuinely atomic under mutex, with deterministic fault points before create, after create, before CAS and after CAS; decrypt failure/wrong key/associated data rejects. Separate multi-process filesystem tests from P1.1 still run. Add vault tests proving wrong account/instance/key associated data cannot decrypt and secrets never occur in safe errors.

- [ ] **Step 4a (3 min): Pin generation separation before compaction consumers exist.**

```go
func TestGenerationMismatchFailsClosed(t *testing.T) {
 storetest.RunGenerationMismatch(t,func(t *testing.T) storetest.Store {
  ledgerDir,headDir:=filepath.Join(t.TempDir(),"ledger"),filepath.Join(t.TempDir(),"head")
  s,err:=localstore.Open(ledgerDir,headDir); if err!=nil { t.Fatal(err) }
  if err:=s.InitializeEmpty(t.Context()); err!=nil { t.Fatal(err) }
  return storetest.Store{Ledger:s.Ledger,Heads:s.Heads,Reopen:func(t *testing.T)(security.SecurityLedger,security.LedgerHeadStore){
   reopened,err:=localstore.Open(ledgerDir,headDir); if err!=nil { t.Fatal(err) }; return reopened.Ledger,reopened.Heads
  }}
 })
}
```

Add `storetest.RunGenerationMismatch(t *testing.T,newStore Factory)` beside `Run`: append/CAS one valid generation-1 account-registration event, retain revision/hash but substitute generation 2 in the supplied committed head, and assert `ReadCommitted` rejects it; independently mutate event generation, head generation and trusted manifest generation. Run the identical test against hosted doubles. RED: `rtk proxy sh -c 'cd Server && go test ./internal/security/... -run TestGenerationMismatchFailsClosed -count=1'`, expected missing helper or a falsely accepted head; GREEN repeats after generation binding. P4 alone introduces signed checkpoint-generation publication and whole-generation retirement; P1 adds no delete API or speculative prefix skipping.

- [ ] **Step 5 (4 min): Complete mode configuration and Compose wiring.**

```yaml
# Add to app service; projector is an authority collaborator, never an HTTP option.
environment:
  BT_PROJECTOR_DATABASE_URL_FILE: /run/secrets/projector_database_url
  BT_SESSION_TOKEN_PEPPER_FILE: /run/secrets/session_token_pepper
  BT_IDENTITY_PEPPER_FILE: /run/secrets/identity_pepper
  BT_INVITATION_PEPPER_FILE: /run/secrets/invitation_pepper
  BT_RATE_LIMIT_PEPPER_FILE: /run/secrets/rate_limit_pepper
  BT_DEVICE_CONTINUITY_PEPPER_FILE: /run/secrets/device_continuity_pepper
  BT_ACTOR_ID_PEPPER_FILE: /run/secrets/actor_id_pepper
  BT_APPLE_REVOCATION_DATABASE_URL_FILE: /run/secrets/apple_revocation_database_url
```

Self-host requires separate ledger/head volumes, persistent fence/identity on the head volume, mounted session/identity/invitation/rate/device-continuity peppers, runtime URL, separate projector URL only in internal authority wiring and no Apple developer/APNs key. Add projector login/secret to bootstrap, explicit init service/tool and preflight `security init`; no automatic migration or initialization. Hosted requires operator provider configuration plus hosted Apple team/key/client/vault values and a compiled registered concrete adapter. Until one is supplied, hosted startup returns safe `hosted_adapter_unavailable` and readiness never passes. Keep rootfs read-only, UID/GID 65532, independent volumes and pinned images. Update `smoke.sh` to initialize authority explicitly, verify new roles/least privilege and query migration manifest/table inventory rather than retain “five tables/two migrations” assertions.

- [ ] **Step 6 (4 min): Document actual security-state recovery boundaries.**

`docs/operations/security-state.md` states D1 verbatim in operational language: keep latest ledger/head when restoring database; rotate epoch explicitly; stop authorization until replay; verify revision/hash and instance; never restore both stores from the DB-era snapshot and call it safe. Total-store disaster requires latest independently trusted manifest or remains quarantined. Include single-authority-host/local-fence restriction, separate hosted failure domains, fsync persistence, secret rotation with versioned old-key retention until encrypted material is retired, opaque ledger data inventory, maximum event/replay bounds and forward pointer to Plan 4 finite backup watermark/checkpoint erasure. `Server/README.md` presents identity/family capability only, no sync/public-beta-ready claim.

- [ ] **Step 7 (4 min): Add real HTTP lifecycle acceptance.**

```go
//go:build identitytest

func TestTwoClientsIdentityLifecycle(t *testing.T) {
 h:=newHTTPFixture(t)
 owner:=h.SignIn("Owner fixture"); caregiver:=h.SignIn("Caregiver fixture")
 owner.CreateFamily("Family fixture"); caregiver.Redeem(owner.Invite())
 owner.Transfer(caregiver.AccountID)
 caregiver.Remove(owner.AccountID)
 h.AssertSingleOwner(caregiver.AccountID)
}
```

Define `(*httpFixture).AssertSingleOwner(string)` to compare replayed authority with runtime-visible family members; extend this same test with the remaining lifecycle sequence below.

Put `//go:build identitytest` on all files under `Server/tests/identity/`; ordinary production builds cannot import the test issuer. Define `newHTTPFixture(*testing.T) *httpFixture` in `tests/identity/lifecycle_test.go`: real `httpapi.New` handler/listener, real runtime/projector PostgreSQL roles, real filesystem ledger/head and deterministic compile-time test issuer. Methods `SignIn(name string) Client`, `Client.CreateFamily(name string)`, `Client.Invite() string`, `Client.Redeem(token string)`, `Client.Transfer(accountID string)`, `Client.Remove(accountID string)`, `Client.DeleteAccount()`, `Client.ScheduleDeletion()`, `Client.Recover()`, `Client.Forfeit()` call HTTP DTOs and retain raw credentials only in memory. These are test helpers that fail their owning `testing.T` on an unexpected status; `Client` contains `AccountID string`, a private HTTP client, token pair and test handle. Test `TestTwoClientsIdentityLifecycle` performs create/join/transfer/remove/new sign-in/reinvite/caregiver delete/family delete/recover/forfeit; verifies HTTP response, authority state and SQL state at each structural boundary. It asserts no record/import/sync capability or endpoint is advertised yet.

- [ ] **Step 8 (4 min): Prove every security decision after an older database restore.**

```go
func TestOlderDatabaseLifecycleReplay(t *testing.T) {
    for _,scenario:=range []string{"member_removed","owner_transferred","account_deleted","family_deleted","invitation_consumed","session_revoked","recovery_forfeited"} {
        t.Run(scenario,func(t *testing.T){
            h:=newRestoreFixture(t,scenario)
            h.SnapshotDatabase()
            h.CommitScenario()
            h.RestoreDatabaseKeepingLatestSecurity()
            h.RotateEpoch()
            h.AssertQuarantined()
            h.Reconcile()
            h.AssertNoResurrectedAuthority()
        })
    }
}
```

Define `newRestoreFixture(*testing.T,string) *restoreFixture` and its seven named zero-argument methods in `restore_test.go`; each assertion fails the test directly. Snapshot/restore uses disposable `pg_dump`/`pg_restore` and unique test database only, preserving latest temp ledger/head directories; restore helper never overwrites authority files. `AssertQuarantined` tries actual old access/refresh HTTP requests before replay and expects 503/epoch denial without data; final assertion reauthenticates eligible users and checks the scenario-specific denial/owner/claim state. Verify old deleted name/provider rows are absent, old owner lacks privileges and consumed invitation cannot join. Drop an invalidation notification intentionally. These are executed tests, not source-level declarations.

- [ ] **Step 9 (4 min): Run GREEN, complete server gates and container regression.**

Run in order:

```bash
rtk proxy bash Server/scripts/test-identity.sh
rtk proxy sh -c 'cd Server && go vet ./...'
rtk proxy sh -c 'cd Server && go test -tags identitytest -race -count=1 ./tests/identity/...'
rtk proxy sh -c 'cd Server && go list -deps ./cmd/baby-tracker-server'
rtk make server-build
rtk make server-smoke
rtk make test
rtk git diff --check
```

Expected: full real-PostgreSQL/race suite PASS without database skips; all API/fixture routes conform; production dependency list excludes `internal/testidentity`; tagged tests execute all seven restore scenarios; both `linux/amd64` and `linux/arm64` image builds succeed; smoke proves explicit migration+initialization, independent liveness/readiness, runtime/projector role separation and cleanup of its exact ephemeral resources; counted Swift suite retains all 233 tests because product Swift code is untouched. Docker architecture builds alone are not architecture runtime smoke; Plan 4 owns actual dual-architecture execution. If any gate cannot execute, record its exact environment blocker and do not mark that gate passing.

- [ ] **Step 10 (3 min): Record D11 acceptance without substituting a fake deployment.**

Create the provider gate section in `docs/operations/security-state.md` with required operator fields and three statuses: `interfaces_and_conformance_verified`, `concrete_adapter_blocked_operator_values`, `hosted_live_acceptance_not_run`. Store no credential values in that document. Once values exist, the concrete provider adapter must run `storetest.Run` plus real encryption/immutability/CAS/failure-domain checks under the selected account before hosted readiness can be enabled; provider SDK/version and immutable resources are reviewed then. This conditional adapter work is part of P1.7's remaining hosted scope, not a completed implementation claim or a newly invented eighth task.

- [ ] **Step 11 (2 min): Commit provider-neutral integration and evidence.**

```bash
rtk git add Server/internal/security/hosted Server/internal/security/storetest Server/internal/identity/hostedcredentials Server/internal/config Server/tests/identity Server/compose.yaml Server/Dockerfile Server/README.md Server/scripts/smoke.sh Server/scripts/smoke_test.go Server/deploy/postgres/init-users.sh Server/scripts/check-principals.sh Server/cmd/baby-tracker-server/main.go Server/internal/httpapi/router.go Server/internal/protocol/types_test.go docs/operations/security-state.md
rtk git commit -m "feat(server): verify identity lifecycle storage contracts"
```

## Mandatory review amendments for P1 acceptance

These checklist items are additions to the owning tasks above. A task report is
not READY until its items and named regressions are present.

### P1.2-P1.3 compatibility and immutable auth persistence

- [ ] Store a complete exact privilege snapshot for every schema version,
  covering schema/table/column/sequence/function privileges. Bootstrap reads
  the latest exact snapshot while retaining hard-coded denial of LOGIN ACLs,
  ownership, unsafe role attributes, PUBLIC escalation, and undeclared
  objects. Readiness checks the actual applied version in one repeatable-read
  privilege view between two identical compatibility checks.
- [ ] Migration 000005 advances `schema_compatibility` transactionally and
  freezes every Task 4 table/function/projector case. Runtime-only definers are
  `create_auth_challenge`, `consume_auth_challenge`, reconciled identity/device/
  access/refresh lookups, atomic weighted `take_rate_limits`, immutable
  `stage_auth_material`, and notification reservation. Only security projection
  and `update_server_state` are projector-executable; auth finalization occurs
  inside `project_security_event`, with no third function/grant. No HTTP
  constructor retains a projector pool.
- [ ] Schema 5 uses the exact key-versioned signatures and return columns in
  Task 3 Step 5. Staging has one fixed field order, explicit nulls, no unknown
  keys, and a SHA-256 digest over exact Go `encoding/json` bytes. Notification
  reservation checks every retained `(key_id,digest)` candidate and inserts the
  current one atomically. Rate charges include `window_end`, accept only the
  eleven frozen scope strings, and apply all or none.
- [ ] Schema-5 projection owns only `identity_session_issued`,
  `session_rotated`, `session_revoked`, `device_binding_revoked`, plus the
  session-revocation extension to `identity_unavailable`; it does not install
  `family_bootstrapped`. Rotation carries its stage digest. Session, device and
  identity revocation use one authority effect and deterministic set-wise SQL,
  never enumerated multi-revocation effects. Missing stage produces only keyed
  opaque/profile-required/material-missing state and never a bearer credential.
- [ ] Strict secret files are 64 lowercase hex characters encoding 32 bytes,
  with at most one final LF/CRLF; reject uppercase, whitespace, symlinks,
  nonregular/oversized files, and simultaneous direct/file values. Persist
  bounded key IDs and expose `DigestKeyring{Current,Previous}` for stable
  digests. Production accepts file inputs only.
- [ ] The client-IP resolver ignores forwarded headers from untrusted peers;
  for trusted peers it parses all values, walks right-to-left past trusted
  hops, rejects malformed/ambiguous chains, and derives only an HMAC key from
  `netip.Addr.Unmap().String()`. Raw IP is never logged or persisted.
- [ ] Apple/JWKS and strict-JSON tests include invalid UTF-8, escaped duplicate
  keys, bounded integral dates, exact audience type, malformed RSA metadata,
  duplicate `kid`, redirect/status/timeout/body limits, expired cache,
  concurrent unknown-key coalescing, absent-vs-empty capabilities, top-level
  non-object, trailing JSON, and max-plus-one body. Clone the injected HTTP
  client before disabling redirects and impose a five-second context deadline.
- [ ] Add `TestIdentitySessionIssuedIsAtomicAcrossProjectionCrash`,
  `TestHTTPAuthCompositionHasNoProjectorCapability`, and real 100-way
  PostgreSQL challenge/weighted-limit races. Assert PUBLIC/cross-role denial,
  one challenge winner, all-or-none charges, and 503 on dependency failure.

### P1.4 sessions, Apple, and credentials

- [ ] `SessionBundle.RecoveryEpoch` is required on initial, continuity, and
  refresh responses. `/v1/server.apple_authorization_mode` is
  `identity_token_only` for self-host and `code_exchange_required` for hosted;
  request validation rejects a code in the former and requires it in the
  latter.
- [ ] Every newly reserved hosted sign-in exchanges its authorization code; only
  exact retry of the same recorded operation may resume without exchanging the
  already consumed code. Hosted code exchange is transport-only. The service re-verifies the
  returned identity token with the original nonce and constant-time compares
  provider, subject, and audience with the incoming verified identity. Swapped
  nonce/subject/audience tests fail before staging or ledger changes.
- [ ] Credential storage uses one deterministic stable main object and at most
  one deterministic replay-marker companion per binding, all through the
  existing `SecretStore` conditional-create/CAS/delete seam. The main object's
  authenticated state holds at most current plus pending; every crash state
  resumes or aborts without a second pending value. The companion records the
  last completed or abandoned rotation, and every main-object deletion proves
  or repairs it durably before delete; restart denies exact or partial reuse
  before provider exchange. Replay sequence stays adapter-private; reclaimed
  non-deleting states keep public deletion marker fields zero. Provider
  inventory remains opaque until the adapter decrypts/authenticates binding
  and kind. Each domain inventory call reads exactly one provider page, filters
  markers, and returns that page's cursor even for an empty result; callers,
  not the adapter, advance pages without loops or duplicate main objects.
  Wrong binding, object, version, key, purpose, operation, marker sequence,
  inner kind or state fails closed.
- [ ] Continuity lookup is account/device/instance/origin qualified. Device
  authority stores canonical origin and digest key ID. Identity unavailability
  and restore never revive sessions. Add
  `TestIdentityRestoreNeverRevivesOldSessions` and
  `TestDeviceOriginSurvivesOlderDatabaseReplay`. Deleted-identity-to-new-actor
  rebinding and `TestDeletedIdentityCreatesNewActorButRejectsOldContinuity`
  are Task 6/000007-owned because immutable schema 000005 retains the old
  projected identity binding; Task 4 may not emulate rebinding with table DML.
- [ ] Apple notification age is bounded to `[now-24h,now+60s]`; dedup candidates
  span retained key IDs and required tombstones live at least that window.
  Derive/check every retained candidate command receipt before SQL; reserve all
  digests once; fail on multiple SQL matches; recheck the returned stored
  command receipt and execute that exact ID if absent. SQL alone never completes
  a notification. Session/receipt compaction never prunes live refresh-reuse
  evidence.

### P1.5-P1.6 family, operator, and lifecycle

- [ ] `family_bootstrapped` deterministically creates family, owner membership,
  account slot, and hosted-beta entitlement in one event. Family/invitation
  UUIDv8 IDs derive from actor plus command ID. Command-keyed presentation
  staging is immutable and digest-bound. A lost invitation response returns
  stable metadata with `secret_unavailable`; it never creates a new token.
- [ ] Missing restored presentation produces `FamilyView.ProfileRequired` and
  placeholder-only projection until an owner-confirmed name repair. Historical
  actor labels include active and former actors through a bounded paginated,
  high-water family surface. Test lost create/invite responses and restored
  family-profile repair.
- [ ] `OwnerChangePayload` and operator entitlement events carry
  `operator_policy_id`, keyed 64-hex `operator_evidence_digest`, closed
  `operator_reason_code`, JSON-safe positive `expected_revision`, and the
  configured operator UUID. Separate ownership and entitlement policy IDs are
  loaded from 0600 nonsymlink secret configuration. Identical committed retry
  precedes stale/expired-confirmation rejection; changed retry denies.
- [ ] `owner_recovered` applies only to an active family whose old owner is
  unavailable and whose target is an active caregiver. `family_recovered`
  alone restores deletion-pending state. Entitlement grant can extend an
  active/expired grant; suspension binds source and expiry and changes only
  status. Regenerate every confirmation fixture after binding operator ID and
  all fields.
- [ ] Account deletion is one bounded effect containing only account/identity
  tombstone and structural aggregates; replay/projector deterministically
  revoke all devices, sessions, claims, memberships, and labels. Test enough
  historical rotations to exceed the event limit if enumerated.
- [ ] Recovery claims use status `active`. Authority family state stores purge
  start command ID/revision, physical SQL receipt UUID, and purged timestamp in
  dedicated fields. `family_purge_completed` binds the expected start revision
  and receipt. Add `TestPurgeReservationRoundTripsStartAndReceipt`.
- [ ] The distinct `export_family` owner action ignores `family_sync`
  suspension/expiry; ordinary sync/import still requires entitlement. Family
  deletion/recovery/forfeit and account deletion also remain available.

### P1.7 hosted stores and P4 authority handoff

- [ ] `NewLedger` receives a bounded encrypted replay-index/scratch port and
  streams forward without an unbounded slice; rootfs remains read-only and the
  scratch mount has explicit capacity. Test long chains, cancellation,
  disk-full cleanup, and no retained plaintext.
- [ ] Provider registry/factory returns ledger, head, vault, and observed
  properties. Empty registry returns `hosted_adapter_unavailable`; hosted never
  falls back to local storage. Validation consumes an explicit provider/region
  allowlist and distinct failure-domain/key-ID config, while live provider
  assertions remain an external gate.
- [ ] The provider-neutral credential adapter persists the replay marker as a
  second encrypted object at the distinct deterministic replay-object UUIDv8,
  using the unchanged `SecretStore`. The shared outer envelope/AAD authenticates
  inner kind `credential-replay-marker-v1`, exact binding, last operation and
  command, and bounded adapter-private sequence with no token material; that
  sequence never populates public non-deleting rotation-state marker fields.
  Inventory reads exactly one bounded provider page per call and may return an
  empty filtered page with an advanced cursor. Common conformance
  reopens new adapters over the same durable store and proves both completed
  and abandoned history survive marker-before-main deletion crash recovery,
  exact/partial replay denial, fresh reclaim, stale deletion denial, wrong-AAD
  failure, missing/stale-marker repair, one-marker CAS and inventory filtering.
- [ ] Replay-marker erasure is account-generation scoped. Before deleting the
  main credential object or its last recoverable reference, persist
  `AccountGenerationRetentionRecord{AccountID,ActorGeneration,Binding,
  ReplayMarkerObjectID,DeletionRevision,DeletionHead,MainAbsentAt,
  ProviderRevocationState,State}` with state `retained`; `Binding` and object ID
  are opaque authenticated coordinates, never a token. Task 6 retains the old
  marker through provider revocation and confirmed main-object absence. P4's
  final account-generation purge alone changes `retained -> eligible -> purged`
  and calls the narrow authenticated purger. Eligibility requires all of: the
  permanent account/identity deletion tombstone and next actor generation are
  present in the committed typed checkpoint; provider revocation is terminal
  or its documented 30-day retry retention ended without restoring authority;
  the main object is confirmed absent; the signed oldest-restorable watermark
  is strictly after `DeletionHead`; every pre-deletion ledger/checkpoint and DB
  backup generation is retired; and no recovery, re-registration, credential
  cleanup, or restore dependency names that account generation. The retention
  record survives checkpoint/restart until authenticated marker absence is
  confirmed, then the same transaction records a content-free purge receipt
  before dropping its binding coordinates. Ambiguous provider deletion stays
  `eligible` and retries; it never guesses success. Family deletion, recovery
  and the 30-day family purge retain markers for accounts whose authority
  remains. A later actor generation has a distinct account ID, binding and
  marker ID and cannot satisfy the old generation's purge predicate. Add
  `TestFamilyPurgeRetainsCredentialReplayMarkerForActiveAccount`,
  `TestAccountGenerationMarkerWaitsForCheckpointAndBackupRetirement`,
  `TestAmbiguousReplayMarkerPurgeRetainsCoordinates`,
  `TestFinalAccountGenerationPurgeRemovesOnlyOldReplayMarker`, and
  `TestNewActorGenerationUsesDistinctReplayMarker` to the P4 handoff.
- [ ] Freeze provider-neutral names `BT_HOSTED_PROVIDER_ID`,
  `BT_HOSTED_REGION`, `BT_HOSTED_APPROVED_REGIONS`,
  `BT_HOSTED_LEDGER_FAILURE_DOMAIN`, `BT_HOSTED_HEAD_FAILURE_DOMAIN`,
  `BT_HOSTED_LEDGER_KEY_ID`, `BT_HOSTED_HEAD_KEY_ID`,
  `BT_HOSTED_VAULT_KEY_ID`, `BT_SECURITY_REPLAY_SCRATCH_PATH`, and
  `BT_SECURITY_REPLAY_SCRATCH_MAX_BYTES`. Provider credentials remain
  adapter-specific secret files. Compose mounts a size-bounded writable scratch
  volume while retaining read-only rootfs.
- [ ] Freeze the versioned typed authority checkpoint seam below before P4.1;
  P4 may add signing/storage adapters but may not replace it with a flat fact
  map. Every slice is sorted by its explicit primary key and rejects duplicate
  keys. `CheckpointCompaction` contains only version/cutoffs/counts, never an
  omitted family/account ID or a digest that remains a linking identifier.

  ```go
  type AuthorityCheckpointState struct {
      Version string // exactly authority-checkpoint-state-v1
      Accounts []CheckpointAccount
      FamilySlots []CheckpointFamilySlot
      Families []CheckpointFamily
      Memberships []CheckpointMembership
      Devices []CheckpointDevice
      Invitations []CheckpointInvitation
      Sessions []CheckpointSession
      Entitlements []CheckpointEntitlement
      RecoveryClaims []CheckpointRecoveryClaim
      CommandReceipts []CheckpointCommandReceipt
      AccountGenerations []AccountGenerationRetentionRecord
      Compaction CheckpointCompaction
  }
  type CheckpointAccount struct { AccountID,IdentityKey,Status string; RegisteredRevision,ActorGeneration uint64 }
  type CheckpointFamilySlot struct { AccountID,FamilyID,State string }
  type CheckpointFamily struct { FamilyID string; View FamilyView }
  type CheckpointMembership struct { MembershipID string; View MembershipView }
  type CheckpointDevice struct { DeviceID string; View DeviceView }
  type CheckpointInvitation struct { InvitationID string; View InvitationView }
  type CheckpointSession struct { SessionID string; View SessionView; ReuseDetected bool }
  type CheckpointEntitlement struct { FamilyID,Key string; View EntitlementView }
  type CheckpointRecoveryClaim struct { ClaimID string; View RecoveryClaimView; ExpiresAt string }
  type CheckpointCommandReceipt struct {
      ActorAccountID,CommandID,Scope,FamilyID,PreparedKind string
      CommandHash [32]byte
      Result CommandResult
      Kind EventKind
  }
  type AccountGenerationRetentionRecord struct {
      RecordID,AccountID,ServerInstanceID,CredentialPurpose string
      CredentialObjectID,ReplayMarkerObjectID string
      ActorGeneration,DeletionRevision uint64
      DeletionHead Head
      MainAbsentAt,ProviderRevocationState,State string
  }
  type CheckpointCompaction struct {
      PolicyVersion,ReceiptCutoff,SessionCutoff string
      RemovedReceiptCount,RemovedSessionCount uint64
  }
  type CheckpointSelection struct { Compaction CheckpointCompaction; EraseFamilyIDs []string }
  type AuthorityCheckpointHeader struct {
      Format,InstanceID,KeyID string
      RecoveryEpoch uint64
      Base Head
      StateSHA256 [32]byte
  }
  type VerifiedAuthorityCheckpoint struct { Header AuthorityCheckpointHeader; State AuthorityCheckpointState; token *verifiedCheckpointCapability }
  func CheckpointStateFrom(State, CheckpointSelection) (AuthorityCheckpointState,error)
  func RestoreAuthorityCheckpointState(VerifiedAuthorityCheckpoint) (State,error)
  ```

  `RestoreAuthorityCheckpointState` accepts only the unforgeable value returned
  by P4's canonical signature/header/anchor verifier; direct struct literals,
  wrong instance/epoch/base/state hash, unknown fields/versions, lossy rows and
  duplicate keys fail closed. It restores identical authorization and receipt
  decisions, including permanent identity/account tombstones, refresh lineage
  reuse denial, recovery claims, purge start/revision/receipt and retained
  account-generation cleanup. Compile-time exhaustiveness plus a reflection
  test over every private `State` collection fails whenever a future state
  field lacks an explicit checkpoint mapping.
- [ ] Freeze a stable instance-derived UUIDv8 retention actor and non-HTTP
  `retention` scope limited to purge start/complete; retries keep the same hash
  across restart. Freeze
  `CheckpointCapability{Format,InstanceID,MinimumReader string; Sequence uint64;
  EnabledAtHead Head}` in a `CheckpointCapabilityStore` beside the independent
  head/witness with exact methods
  `Load(context.Context) (CheckpointCapability,error)` and
  `CompareAndSwap(context.Context,CheckpointCapability,CheckpointCapability)
  (bool,error)`.
  It is monotonic: format/minimum reader/sequence cannot regress or be disabled.
  The inspect/confirmed-enable CLI binds the current head and instance; restart
  and database restore reread the independent record, while an old reader that
  cannot satisfy `MinimumReader` fails before replay.
- [ ] Add `TestAuthorityCheckpointRoundTripEveryDecision`,
  `TestCheckpointReceiptPruningPreservesReplayAndReuseDenial`,
  `TestRecoveryClaimAndPurgeTerminalStateSurviveCheckpoint`,
  `TestAuthorityCheckpointRejectsUnverifiedRestore`,
  `TestAuthorityCheckpointMappingIsExhaustive`,
  `TestRetentionActorRetryStableAcrossRestart`, and
  `TestCheckpointEnablementSurvivesDatabaseRestore` to the P1/P4 handoff.

## Completion and downstream handoff

Plan 1's provider-neutral/self-hosted deliverable is complete only when P1.1–P1.6 and the unblocked P1.7 gates execute successfully. Hosted completion remains blocked by D11 until its real adapter/provider gates pass; real ownership recovery remains disabled without D13 policy values. Neither blocker prevents Plan 2 from consuming tested `access.Principal/Grant`, `family.Service.Authorize`, `security.Service.WithAuthorizedTransaction`, `SecurityInvalidation`, instance/epoch and entitlement contracts.

Plan 2 adds canonical records, import, durable receipts/cursors, timers and sockets. Plan 3 owns Keychain, endpoint confirmation, Apple client credential-state checks, AASA/QR/scanner/manual-paste UI, destructive confirmations and realm purge. Plan 4 owns physical purge, signed checkpoints/final security erasure, export, APNs, metrics/release CI, real provider deployment/backup acceptance, privacy publication and public-beta evidence. This allocation covers the complete design without treating those future release gates as accomplished here.

### Self-review record

- Spec coverage: P1.1 owns §§5.3/12 authority and self-host durability; P1.2 owns §§5.3/7.1/12 restore/epoch/fencing/schema overlap; P1.3 owns §§3.1/6/8/9 identity proof/nonce/limits/contracts; P1.4 owns §§5.1/6 sessions, Apple notifications and sealed revocation credentials; P1.5 owns §§3.1–3.2/5.1/11 family roles/invitations/entitlements; P1.6 owns §§3.1/6/10 lifecycle/owner-loss/operator commands plus durable Apple revocation-job creation/execution in 000007; P1.7 owns hosted provider adapters/config and §13.1 whole identity test gate. Canonical data, iOS and remaining operations are explicitly assigned above, not lost from scope.
- Placeholder scan: no unfinished implementation marker or generic error-handling instruction is accepted. Each code step has a signature, example, algorithm or exact schema; each task has a RED command/expected failure, GREEN command and conventional commit.
- Type consistency: shared principals/grants are defined only in `access`; exact `SecurityLedger` and boolean CAS signatures follow decomposition; `CommandResult` is consistent across family/auth/operator; guard callback is always `func(pgx.Tx,access.Grant) error`; revision/epoch bounds and wire names agree. Architecture-map `auth.Principal`, `LedgerStore.Scan` and error-only CAS are not adopted.
- Review fixes I2/I3/M1/M2: migration 000003–000008 ownership and one-host authority are unchanged. Every downstream migration owns its exact embedded manifest and two-binary gate; fresh Apple proof preserves an authenticated device binding with denied cross-account claims and defined lost-start receipt handoff; capability grammar/error are shared; operator commit methods take the complete `Confirmation` with expiry, and literal prepare/commit examples have independently recomputed HMAC fixtures. New generation/purge reservations support P4 without permitting P1 prefix deletion or physical purge.
- Durable credential replay amendment: an account binding has one main credential object plus at most one separately derived encrypted replay marker in the existing `SecretStore`; the marker carries the last completed or abandoned rotation and is proven/repaired before every main deletion. Restart-backed conformance, provider-timeout coverage, inventory filtering and account-generation-only final purge close the deletion replay gap without widening the P1.4 vault or treating family purge as account erasure.
- Validation rerun: seven task headings, all checkbox steps, balanced code fences, no unfinished markers, shell examples prefixed with `rtk`, consistent continuity/confirmation signatures and no whitespace errors. These checks validate this plan artifact; product tests remain execution work.
- Review Focus: all five risks are pinned to named behavioral tests in P1.1/P1.2/P1.4/P1.5/P1.6/P1.7. The reference to undetectable rollback is a tested fail-closed/operator-trust boundary, never a claim that a stale matching pair proves freshness.
- Unavoidable gates: D11 real provider/account/key/domain values and D13 approved owner-verification policy are unavailable. No production deployment, real Apple authentication or public-beta readiness is claimed by this plan.
