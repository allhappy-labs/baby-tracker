# Family Sync Retention, Operations, and Beta Readiness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Complete retention/erasure, hosted background wakes, owner exports, recovery tooling and repeatable local beta acceptance, then collect the separately authorized external release evidence.

**Architecture:** P1's external security head remains lifecycle authority; workers consume replayable PostgreSQL projections with distinct credentials. Compaction publishes an immutable signed generation checkpoint before whole-head CAS and retires old generations only after an independently witnessed backup inventory permits erasure. Extend P2's queue/protocol harness and P3's client/E2E target in place.

**Tech Stack:** Go 1.26.2, PostgreSQL 17, pgx/v5, standard-library AES-GCM/Ed25519/HTTP, Swift 6.2, Docker/Compose, OpenAPI 3.1, Prometheus, pinned CycloneDX and vulnerability tools.

**Spec:** `docs/superpowers/specs/2026-09-19-family-sync-server-design.md`. Read it completely with the identity-lifecycle (P1), canonical-protocol (P2) and ios-client (P3) plans. The completed foundation plan remains closed.

**Local prerequisites:** Verified P1 authority/fence, `Head{Generation,Revision,EventHash}`, reserved purge events, projection/recovery guard, checksum manifest generator/overlap script, provider-neutral stores and `internal/testidentity`; P2 schema through 000013, records/import/snapshot services, queue, `internal/testkit` and `scripts/protocol-e2e.sh`; P3 local client contracts, counted package tests and checked-in `BabyTrackerFamilySyncE2ETests` target/scheme. Concrete hosted providers, real Apple/domain/signing acceptance and publication/manual devices may remain pending while local tasks proceed.

## Global Constraints

- “Local-only use remains fully functional without an account.” Preserve one active family/account and exactly one live owner.
- “The application never auto-migrates on startup.” Preserve all applied SQL bytes; every new migration updates/stages `Server/internal/migrate/compatibility.json`.
- “Migration ownership, runtime tenant access, and maintenance duties never share one database credential.” Forced RLS applies to new family rows; workers get narrowly typed functions, never table ownership/BYPASSRLS/role inheritance.
- P1 lock order remains process mutex → global file fence → sorted aggregate locks → SQL transaction. Never call an authority command while holding SQL locks.
- “A gap, modified entry, or otherwise valid-prefix truncation therefore fails readiness.” Compare the whole head, keep revisions globally increasing across generations, and retain P1's one-authority-host restriction.
- “Beta archives are held as encrypted temporary database data, are downloadable only by the requesting owner, and expire within 24 hours.” Expiry here is created_at+24 h, including stalled/failed jobs.
- “Third-party CI actions are pinned to immutable commit SHAs.” Images remain digest-pinned and Go sums locked.
- Logs/reports/metrics exclude tokens, raw APNs tokens/IPs, names, notes, record/export contents and credentials. No tenant identifiers in metric labels.
- No billing, Android/web client, E2EE, attachments, operator browser, audit browser, Redis, Kubernetes, multi-region writer or multiple-family UI/API. Non-development origins use HTTPS.
- APNs/WebSocket wakes are hints; HTTPS sync must converge when every hint is dropped.
- Default finite backup retention 35 days, minimum 30 days; incomplete/stale inventory blocks final erasure.
- Load gate: 100 families, two foreground clients/family, two mutations/minute/client for 15 minutes, then 200-client reconnect; p95 commit-to-remote-durable-refresh≤2 s and successful cycles≥99.9%.

## Review Focus

- P4.1 restarts checkpoint publication/CAS/projection/retirement faults and restores an older DB without rolling back current authority.
- P4.1 races recovery/purge in both commit orders and proves no SQL/fence inversion or half-purged family.
- P4.2 races wake N/N+1 and token V/V+1 with two workers, process death and revocation/epoch changes.
- P4.3 tests requester-only export, repeatable-read consistency, ownership ordering, corruption/framing and physical key/archive expiry.
- P4.4–P4.7 require actual two-architecture execution, restore proof, rendered client behavior, measured load and separately evidenced external gates.

## Execution contracts

All commands run from the worktree root unless the command itself changes directory through `rtk proxy sh -c`. `rtk make server-test` uses P2's disposable PostgreSQL runner; `BT_TEST_RUN` selects tests without permitting DB skips. Never use production as a reset/restore test target. Snippets omit standard imports only.

Reuse P1 `security.Service`, its held-fence marker, `Head`, `Candidate`, `Fence`, `access.Principal/Grant`, `family_purge_started` and `family_purge_completed`. Reuse P2 `protocol.Cursor`/canonical DTOs and queue interfaces. Add `push-token-v1`/`export-v1` to the existing capability registry only with their routes; preserve lowercase-hyphen grammar and `unsupported_capability`. Reuse one P1 `internal/testidentity`, P2 protocol harness and P3 target/scheme; no second issuer/harness.

### Migration compatibility (I2)

At each migration task's start capture the accepted N-1 feature head as
`BT_MIGRATION_BASE_REF`. First commit a reviewed deployable compatibility-only
predecessor containing final migration N, the complete exact schema-N privilege
snapshot, overlap scenarios, and manifest `[N-1,N]`, but no N-dependent
feature path; capture it as `BT_MIGRATION_PRIOR_REF`. Then implement the feature
and change bounds to `[N,N]`; capture `BT_MIGRATION_FEATURE_REF`. P1 prepared
mode archives all three commits without modifying an archive, runs predecessor
at N-1, migrates with predecessor admin, runs predecessor at N, then feature at
N against the same authority stores. Require readiness 200, existing guarded
read/write behavior, exact role denial, distinct build IDs, and changed/
missing/unknown digest denial. The historical synthesized 000004 path is not
P4 evidence.

| Owner | SQL | Prior range | New range | Preserve |
| --- | --- | --- | --- | --- |
| P4.1 | `000014_retention_workers.sql` | `[13,14]` | `[14,14]` | 000001–000013 |
| P4.2 | `000015_push_tokens.sql` | `[14,15]` | `[15,15]` | 000001–000014 |
| P4.3 | `000016_exports.sql` | `[15,16]` | `[16,16]` | 000001–000015 |

Each task's Step 0 creates its predecessor commit. After implementation and
tests, the scoped commit step creates the feature commit, captures
`BT_MIGRATION_FEATURE_REF`, and only then runs the prepared-mode overlap command
shown later in that task. The overlap checkbox is not complete before that
commit, and no accepted commit may be amended after the gate. For P4.1 the exact
tail is:

```bash
rtk git commit -m "feat(server): add resumable purge and security generation erasure"
BT_MIGRATION_FEATURE_REF=$(rtk git rev-parse HEAD)
rtk proxy bash Server/scripts/check-migration-overlap.sh --base-ref "$BT_MIGRATION_BASE_REF" --prior-ref "$BT_MIGRATION_PRIOR_REF" --feature-ref "$BT_MIGRATION_FEATURE_REF" --next-version 14 --prior-min 13 --prior-max 14 --next-min 14 --next-max 14
```

P4.2 and P4.3 use the literal 15 and 16 commands already present in their
checklists after their own feature commits.

The generator hashes final SQL, rejects historical changes and writes `compatibility.json`; no invented hashes. Stage SQL/manifest/overlap scenario/tests/grants in the owning commit. Checkpoint publication stays disabled during 13→14 overlap: prior code understands additive SQL but cannot replay checkpoint-v1. After all authority/worker readers are upgraded, explicit admin `security enable-checkpoints --minimum-reader checkpoint-v1` enables generation transitions. Old readers subsequently fail closed; SQL overlap does not imply ledger-format downgrade compatibility.

### Shared fixtures and cleanup extension

P4.1 adds a typed `retention.CleanupRegistry` whose versioned hooks invoke only
fixed-signature, schema-owned `SECURITY DEFINER` functions through the purge
worker repository; hooks never receive `pgx.Tx` or arbitrary SQL capability.
Register capabilities by schema version and execute later-schema family hooks
first, core family delete next, and the durable physical receipt last in the
same worker transaction. Migrations 000015 and 000016 add their exact cleanup
functions, grants, snapshots, and hook capabilities. A worker missing a current
schema hook refuses to claim/start purge; it cannot falsely complete. A schema
14 worker never queries export/push-token tables.

Extend existing testkit with `NewWorkers(t *testing.T) *WorkerHarness`, fields `Security *security.Service; Owner,Caregiver,Outsider access.Principal; FamilyID,OtherFamilyID,InstanceID string; Purge,Export,Delivery,Runtime,Migrator *pgxpool.Pool; Clock *Clock`. Clock exposes mutex-protected `Now() time.Time`, `Advance(time.Duration)`. Fixtures seed through real P1/P2 services; scans/assertions alone use Migrator. Faults use injected wrappers/barrier channels, never production debug flags. P1's compile-time `identitytest` issuer remains the only fake identity seam.

Define new fixture constants in workers.go: `CommandA="30000000-0000-4000-8000-000000000041"`, `CommandB="30000000-0000-4000-8000-000000000042"`. Every `newCheckpointFixture` embeds WorkerHarness, including Clock, and `Retention *retention.Service`; helpers call `t.Helper()` and fail setup immediately. Fixture helpers are implemented in the named task's test file, not assumed to exist in another plan.

---

### Task 1: Resumable authoritative purge, temporary cleanup and generation erasure (P4.1)

- [ ] **Step 0 (4 min): Land the deployable schema-14 compatibility predecessor before feature work.** Capture accepted schema-13 head as `BT_MIGRATION_BASE_REF`. Write final 000014, complete exact v14 privilege snapshot, schema-14 cleanup capability, overlap tests/scenarios, and manifest `[13,14]`; run the predecessor suite on schema 13; commit only those compatibility files as `feat(server): prepare retention schema compatibility`; review it and capture `BT_MIGRATION_PRIOR_REF`. Do not call schema-14 objects from predecessor production paths. All later Task 1 work is the feature commit and ends with manifest `[14,14]`/`BT_MIGRATION_FEATURE_REF`.

**Files:**

- Create: `Server/internal/retention/{model,repository,purge,cleanup,backup_watermark}.go`, `{purge,cleanup,backup_watermark}_test.go`; `Server/internal/security/{checkpoint,checkpoint_state,retirement}.go`, `{checkpoint,retirement}_test.go`; `Server/internal/security/localstore/{generation.go,generation_test.go}`; `Server/internal/security/hosted/{generation.go,generation_test.go}`.
- Create: `Server/cmd/baby-tracker-worker/{main.go,main_test.go}`, `Server/migrations/000014_retention_workers.sql`, `Server/internal/config/{retention.go,retention_test.go}`, `Server/internal/testkit/workers.go`, `Server/docs/operations/purge.md`.
- Modify: `Server/internal/security/{types,coordinator,projector,commands,state}.go`, `Server/internal/security/localstore/{ledger,head}.go`, `Server/internal/security/hosted/{contracts,ledger,head}.go`, `Server/internal/security/storetest/conformance.go`, `Server/internal/recovery/reconcile.go`, `Server/internal/migrate/{compatibility.json,overlap_test.go}`, `Server/internal/tenant/roles_test.go`, `Server/internal/config/config.go`, `Server/cmd/baby-tracker-admin/{main.go,main_test.go}`, `Server/deploy/postgres/{init-roles.sql,init-users.sh}`, `Server/scripts/{check-principals.sh,check-migration-overlap.sh,smoke.sh,smoke_test.go}`, `Server/compose.yaml`, `Server/Dockerfile`.

**Purge interfaces and ordering:**

```go
type DueFamily struct { FamilyID string; ExpectedRevision uint64; PurgeAfter time.Time }
type PurgeClaim struct { FamilyID,StartCommandID,LeaseID string; StartRevision uint64; LeaseUntil time.Time }
type PurgeResult struct { FamilyID,State string; StartedRevision,CompletedRevision uint64 }
type PurgeRepository interface {
 Due(context.Context,time.Time,int) ([]DueFamily,error)
 Claim(context.Context,time.Time,int) ([]PurgeClaim,error)
 DeleteFamily(context.Context,PurgeClaim,time.Time) (string,error) // durable SQL receipt UUID
 Release(context.Context,PurgeClaim,time.Time,string) error
}
type Options struct { Security *security.Service; Repository *Repository; Clock func()time.Time; WorkerID string; Cleanup CleanupRegistry; Compaction *CompactionOptions }
func New(Options) (*Service,error)
func (s *Service) PurgeDue(context.Context,time.Time,int) ([]PurgeResult,error)
func (s *Service) CleanExpired(context.Context,time.Time,int) (int,error)
func StartFamilyPurge(familyID,commandID string,expectedRevision uint64,purgeAfter time.Time) (security.Command,error)
func CompleteFamilyPurge(familyID,commandID,receiptID string,startRevision uint64,purgedAt time.Time) (security.Command,error)
```

Due is bounded autocommit SELECT(limit 1–10) without row locks. For each due family call Execute with internal scope `retention` minted only by worker composition and deterministic UUIDv8 from SHA-256(`"baby-tracker-purge-start-v1\x00" || familyUUID[16] || u64be(expectedRevision) || canonicalPurgeAfter`). Under exclusive fence→family lock, Start rechecks current family revision, exact deadline, deletion state and now≥purge_after, then commits reserved `family_purge_started`. Recovery serialized first before deadline invalidates the due scan; committed Start is terminal. Recovery at/after deadline already denies even before Start.

Start projection creates durable `purge_jobs` with no FK to families. Only after Execute releases fence does Claim open/commit a short SKIP LOCKED transaction and return a 60 s UUID lease. DeleteFamily runs one exact-family SQL transaction, checks locked job lease/start revision, deletes registered resources in FK order, and records `sql_receipt_id/purged_at` atomically. No authority call occurs while this SQL transaction is open. Complete subsequently commits `family_purge_completed`; after a crash following SQL, retry uses the same stored receipt/command even though family rows are gone. Expired claims are reclaimable; stale leases never change another claim. Keep only minimal terminal job/authority receipt until checkpoint erasure.

Complete command ID is UUIDv8 from SHA-256(`"baby-tracker-purge-complete-v1\x00" || familyUUID[16] || sqlReceiptUUID[16]`), so retries survive restarts. P1 State.Decide permits only the internal retention worker to dispatch the two reserved purge kinds; HTTP cannot select that scope.

**Signed generation and watermark contracts:**

```go
// AuthorityCheckpointState, its row types, AuthorityCheckpointHeader,
// VerifiedAuthorityCheckpoint and the only restore constructor are frozen by
// P1.7. P4.1 consumes them; it does not introduce a second fact-map DTO.
type CheckpointBody struct {
 Version string `json:"version"`; InstanceID string `json:"instance_id"`; RecoveryEpoch uint64 `json:"recovery_epoch"`
 OldGeneration uint64 `json:"old_generation"`; NewGeneration uint64 `json:"new_generation"`; BaseRevision uint64 `json:"base_revision"`
 PreviousTerminalHash string `json:"previous_terminal_hash"`; StateSHA256 string `json:"state_sha256"`; State AuthorityCheckpointState `json:"state"`
 WatermarkSHA256 string `json:"watermark_sha256"`; KeyID string `json:"key_id"`; IssuedAt string `json:"issued_at"`
}
type SignedCheckpoint struct { Body CheckpointBody `json:"body"`; Signature string `json:"signature"` }
type BackupWatermarkBody struct {
 Version string `json:"version"`; InstanceID string `json:"instance_id"`; RecoveryEpoch uint64 `json:"recovery_epoch"`
 Generation uint64 `json:"generation"`; Sequence uint64 `json:"sequence"`; OldestRestorable string `json:"oldest_restorable"`
 InventoryAt string `json:"inventory_at"`; InventorySHA256 string `json:"inventory_sha256"`; IssuedAt string `json:"issued_at"`; KeyID string `json:"key_id"`
}
type BackupWatermark struct { Body BackupWatermarkBody `json:"body"`; Signature string `json:"signature"` }
type WatermarkWitness struct { Sequence uint64; Digest [32]byte; OldestRestorable,InventoryAt time.Time; Generation,Epoch uint64 }
type WatermarkWitnessStore interface {
 Load(context.Context) (WatermarkWitness,error)
 CompareAndSwap(context.Context,WatermarkWitness,WatermarkWitness) (bool,error)
}
type TrustedSigner struct { KeyID string; PublicKey ed25519.PublicKey; NotBefore,NotAfter time.Time }
type CheckpointStore interface {
 Create(context.Context,uint64,[]byte) error // computes canonical checkpoint base/hash key; create-if-absent
 Read(context.Context,uint64,uint64,[32]byte) ([]byte,error) // generation, base revision, signed checkpoint hash
}
// internal/security/retirement.go: neutral DTOs and adapter port; no retention import.
type GenerationInventory struct { Generation,ObjectCount uint64; Digest [32]byte; RetainUntil time.Time }
type RetirementProof struct { From,To Head; CheckpointSHA256,OmissionWatermarkSHA256,RetirementWatermarkSHA256 [32]byte; Inventory GenerationInventory }
type GenerationRetirer interface { Inventory(context.Context,uint64) (GenerationInventory,error); Retire(context.Context,RetirementProof) error }
type GenerationAllocator interface { Next(context.Context,uint64) (uint64,error) } // lower-exclusive committed generation
type CheckpointProjection interface {
 Projection
 InstallCheckpoint(context.Context,Head,VerifiedAuthorityCheckpoint) error
}
func VerifyCheckpoint(context.Context,[]byte,[]TrustedSigner,string,uint64) (VerifiedAuthorityCheckpoint,error)
func CanonicalCheckpoint(CheckpointBody) ([]byte,error)
func CanonicalWatermark(BackupWatermarkBody) ([]byte,error)
func (s *Service) CompactSecurity(context.Context,BackupWatermark) (security.Head,error)
func (s *Service) RetireSecurity(context.Context,BackupWatermark) error
```

Package ownership: P1's typed checkpoint state/restore/capability seam plus P4.1 checkpoint signer/store types and CanonicalCheckpoint live in `security`; `GenerationInventory`, `RetirementProof`, and `GenerationRetirer` all live in `Server/internal/security/retirement.go`. `Head` in RetirementProof is P1's same-package type. Only backup watermark/body/witness and backup inventory types (`BackupInventory`, `BackupInventoryEntry`, `BackupInventoryProvider`), CanonicalWatermark and CompactSecurity/RetireSecurity live in `retention`. Qualify cross-package references accordingly. The dependency is retention→security; security never imports retention or refers to its backup/watermark DTOs. Localstore/hosted generation adapters implement the security-owned `GenerationRetirer`; retention consumes that interface and populates `security.RetirementProof` with its returned `security.GenerationInventory` after validating backup/watermark evidence. Define `retention.CompactionOptions{Witness WatermarkWitnessStore; Inventory BackupInventoryProvider; Retirer security.GenerationRetirer; TrustedKeys []security.TrustedSigner; SigningKey ed25519.PrivateKey; SigningKeyID,InstanceID string; Epoch func(context.Context)(uint64,error)}`. Extend P1 `security.Options` with optional `Generations *GenerationOptions`, where `GenerationOptions{Checkpoints CheckpointStore; Retirer GenerationRetirer; Allocator GenerationAllocator; Capability CheckpointCapabilityStore; TrustedKeys []TrustedSigner}`; nil disables only compaction. Worker composition supplies the same maintenance adapter as both Retirer fields; there is no second DTO definition or retention-owned alias. `security.Service.PublishCheckpoint` takes one short exclusive-fence hold to clone the typed state/head/epoch and reserve a generation, builds/signs/publishes outside the fence, then takes a second short hold to reverify the unchanged head, watermark and monotonic capability before exact head CAS. A changed head abandons the reserved generation and retries; the builder cannot call independently locking ReadState. After successful CAS, projection mismatch keeps traffic quarantined while bounded reconcile installs only the `VerifiedAuthorityCheckpoint` returned by signature/header/anchor verification.

Config names: `BT_SECURITY_CHECKPOINT_SIGNING_KEY_FILE`,
`BT_SECURITY_CHECKPOINT_TRUSTED_KEYS_FILE`,
`BT_BACKUP_WATERMARK_SIGNING_KEY_FILE`,
`BT_BACKUP_WATERMARK_TRUSTED_KEYS_FILE`,
`BT_BACKUP_MANIFEST_SIGNING_KEY_FILE`,
`BT_BACKUP_MANIFEST_TRUSTED_KEYS_FILE`, `BT_BACKUP_RETENTION_DAYS`
(default 35, minimum 30), `BT_BACKUP_INVENTORY_ROOT`, and
`BT_WATERMARK_WITNESS_PATH`. The beta additionally fixes
`BT_SECURITY_CHECKPOINT_MAX_BYTES=67108864`,
`BT_SECURITY_CHECKPOINT_RESERVE_BYTES=8388608`,
`BT_SECURITY_CHECKPOINT_FENCE_P95_MS=250`, and
`BT_SECURITY_CHECKPOINT_FENCE_HARD_MS=2000`; shipping beta configuration may
not raise or disable these four bounds. Private signing files contain exactly a 32-byte
Ed25519 seed encoded as 64 lowercase hex plus optional final LF; bounded regular
0600 nonsymlink files only. Each trusted-key registry carries key ID, public
key, allowed purpose, and validity window. Add-before-use and
remove-after-dependent-material-retirement is mandatory. Hosted checkpoints use
P1 EnvelopeCipher with instance/generation/revision/checkpoint-hash AAD;
signatures cover canonical plaintext.

Canonical bytes: UTF-8 JSON of the exact tagged structs in declaration order, no maps/omitted fields/whitespace/floats, SetEscapeHTML(false), strip encoder's sole final LF; valid Unicode, canonical lowercase UUID, lowercase 64 hex hashes, UTC timestamps exactly `2006-01-02T15:04:05.000000Z`. Empty strings remain `""`; empty arrays `[]`; safe-integer bounds from P1. Sort each P1 typed slice by its declared primary key and receipts by `(actor_account_id,command_id)`; reject duplicate keys. StateSHA256 hashes canonical state. Ed25519 preimage is exactly `"baby-tracker-security-checkpoint-v1\x00" || CanonicalCheckpoint(body)`; unpadded base64url 64-byte signature stays outside preimage. New anchor hash is SHA-256 of canonical SignedCheckpoint bytes including signature. Hard checkpoint limit is 64 MiB.

P1's typed state preserves current grants/revocations, identity digests, continuity binding/origin/instance, session lineage/epoch/deadline/reuse, recovery claim/deadline, roles, invitation HMAC/expiry/status, entitlement, terminal purge state/receipt, account-generation replay-marker retention and compaction counts/cutoffs. It contains no bearer/profile/name/raw subject. Receipts retain command hash and result enums/revision without event payload; family scope is retained only until the verified family-erasure cutoff permits removing the whole receipt. `CheckpointStateFrom` removes only eligible purged-family rows/references and `RestoreAuthorityCheckpointState(VerifiedAuthorityCheckpoint)` restores identical authorization decisions. Preserve deleted-account anti-rollback digests and unpurged account-generation marker records independently. Complete-state projection scrubs restored grants/profiles and unknown absent families; it cannot require retired historical events.

InstallCheckpoint is an idempotent projector operation under global quarantine: verify committed anchor hash/generation, reconstruct grants/receipts and unfinished purge jobs (including StartCommandID/StartRevision), scrub deleted profiles/identities, remove obsolete family/security/audit/job rows, then advance projection head. It never reintroduces a physically completed family's rows. Migration 000014 adds `checkpoint_reconcile_progress(singleton boolean PRIMARY KEY,target_generation bigint,base_revision bigint,anchor_hash bytea,last_family_id uuid)` visible only to projector functions. The trusted negative set is DB family IDs absent from authoritative facts; delete them in bounded exact-family transactions, persisting progress, while all authenticated traffic stays quarantined. A final projector transaction installs remaining current facts/receipts, clears progress including last_family_id, and advances head only after every deletion succeeds. Restart resumes the same verified target; tests scan projection/audit/job/progress bytes as well as external generations for erased family identifiers.

Watermark preimage exactly `"baby-tracker-backup-watermark-v1\x00" || CanonicalWatermark(body)`. Bind instance/current recovery epoch/generation, strictly increasing sequence, oldest restorable DB cutoff, complete inventory digest/time, issuedAt/keyID. Require issuedAt≤now, inventory≤issuedAt and age≤24 h, same instance/epoch, nondecreasing generation/cutoff/inventory time. CAS exact independent witness; identical repeat is idempotent, same sequence/different digest and regression fail. Local witness is fsynced watermark.json on independent head volume; hosted is an independently persisted encrypted linearizable value. SQL restore cannot restore it. Coordinated rollback of every independent witness remains outside detectable threat model.

`BackupInventoryProvider.Inventory(ctx) (BackupInventory,error)` returns `{InstanceID,Provider,Scope string; CapturedAt time.Time; Complete bool; Backups []BackupInventoryEntry}`; entry `{ID,Kind,Version string; CreatedAt,RestorableFrom,RetainUntil time.Time; Deleted bool; ContentSHA256 [32]byte}`, kind database/ledger-generation-copy. Canonical digest sorts kind/ID/version and includes all fields. Enumerate every configured store/version/snapshot/retention hold; missing page/unknown store/unconfirmed deletion blocks cutoff advancement. Local adapter verifies explicit manifest roots and existing paths; hosted double tests pagination/retention. P4.5 owns CLI orchestration. No remaining DB backup means cutoff=inventory time, never infinity. Old generation copies must physically disappear too; unreadable is not erased.

Publish under exclusive fence: verify whole current head+accepted watermark → reserve a fresh generation n>g → minimal state (omit only purged_at<oldest_restorable) → immutable create generation(n)/checkpoint-<baseRevision>-<checkpointHash>.json → fsync/read/verify → CAS exact `{g,r,h}` to `{n,r,checkpointHash}` → reconcile projection → release. Next event `{n,r+1,previousHash:checkpointHash}`. Normally n=g+1; an abandoned publication may leave a gap. GenerationAllocator uses a fsynced independent head-volume counter or hosted linearizable encrypted CAS value, never directory-listing freshness; it rejects overflow and never reuses a reserved generation. Content-addressed retries may reuse only identical bytes for the same attempt. A changed prior head must reserve a new generation, so a live generation cannot retain an abandoned checkpoint containing a now-erased family. Uncommitted abandoned generations retire whole under the same verified watermark once their numbers precede the current head. Only the exact head-reachable anchor is committed; never select latest filename as authority. Ambiguous CAS rereads exact head; unrelated competing generation quarantines/reconciles.

Local layout extends P1 `generation-<20-digit-generation>/<20-digit-revision>-<64-hex-hash>.json` with `checkpoint-<20-digit-base-revision>-<64-hex-checkpoint-hash>.json` for g>1; g1 keeps zero anchor. OpenGeneration follows exact candidate prior links backward from the supplied head until that revision/hash names a checkpoint, verifies it, then WalkCommitted streams forward from base+1. The two passes are bounded-memory; no guessed/skipped prefix or unbounded candidate list. Startup verifies signer/instance/epoch/state/anchor hash and every committed candidate. Older checkpoint epoch is accepted only with independently persisted explicit monotonic rotation; never revives old sessions. Latest trusted head/witness must prove anchor or verified descendant. Lower generation/revision, unexpected equal-revision hash, missing anchor/gap/corruption/prefix truncation deny readiness.

Keep P1 `ImmutableObjects` immutable. A separate maintenance `GenerationObjects` port has exact methods `ListGenerationVersions(context.Context,uint64,string) (GenerationPage,error)`, `Retention(context.Context,string,string) (ObjectRetention,error)`, `DeleteVersion(context.Context,string,string,[32]byte) error`; types `GenerationPage{Objects []ObjectVersion; NextToken string}` and `ObjectRetention{Until time.Time; Held bool}`. Retirer inventories all versions including abandoned checkpoint candidates, verifies finite retention/no holds and removes every version, then verifies empty. Local Retire refuses current/unknown generation/symlinks, removes only validated enumerated files beneath its configured generation directory, fsyncs file/directory changes. Durable retirement manifest on head volume records heads/checkpoint/both watermark/inventory hashes, never omitted IDs. Partial deletion retries idempotently. Proof requires current committed checkpoint ancestor, accepted independent watermark, inventory captured after publication, cutoff after all omitted-family physical purges, provider deadlines elapsed, all backup copies physically absent. Trust-key rotation adds operator-approved new public key before use; old verification keys remain until dependent material retires. Embedded checkpoint keys are never trusted.

- [ ] **Step 1 (3 min): Capture prior ref and write purge crash/race tests.** `newPurgeFixture(t)` embeds WorkerHarness plus `Retention *Service`, `SeedDueFamily(t) DueFamily`, `FailOnce(string)`, `Reopen(t)`, `CountFamilyRows(t,id) int`, `State(id) string`. Fault injection is before/after actual commit boundaries.

```go
func TestPurgeResumesAfterEveryCommitBoundary(t *testing.T) {
 for _,point:=range []string{"before_head","after_head_before_projection","after_projection_before_claim","during_delete_sql","after_sql_before_complete"} {
  t.Run(point,func(t *testing.T){ h:=newPurgeFixture(t); due:=h.SeedDueFamily(t); h.FailOnce(point)
   _,_=h.Retention.PurgeDue(t.Context(),h.Clock.Now(),10); h.Reopen(t)
   if _,err:=h.Retention.PurgeDue(t.Context(),h.Clock.Now(),10); err!=nil { t.Fatal(err) }
   if h.CountFamilyRows(t,due.FamilyID)!=0 || h.State(due.FamilyID)!="purge_completed" { t.Fatal("incomplete purge") }
   if h.CountFamilyRows(t,h.OtherFamilyID)==0 { t.Fatal("foreign family erased") }
  })
 }
}
```

`TestPurgeRecoveryCommitOrders`: recovery acquires fence at deadline−1µs, clock advances to deadline, purge waits; recovery wins and purge is stale. Reverse: Start commits at deadline, recovery denies. Barrier asserts no SQL transaction remains open entering Execute. `TestRetentionRoleIsolation` calls worker functions through Runtime and expects42501.

- [ ] **Step 2 (2 min): RED.** Run `rtk proxy env BT_TEST_RUN='TestPurge|TestRetentionRole' make server-test`; expect missing behavior/schema, no DB skips.
- [ ] **Step 3 (5 min): Add migration and projected jobs.**

```sql
CREATE TABLE purge_jobs (
 family_id uuid PRIMARY KEY, start_command_id uuid UNIQUE NOT NULL, start_revision bigint NOT NULL,
 purge_after timestamptz NOT NULL, state text NOT NULL CHECK(state IN ('pending','claimed','sql_done','completed')),
 lease_id uuid, lease_until timestamptz, next_attempt_at timestamptz NOT NULL,
 sql_receipt_id uuid UNIQUE, purged_at timestamptz, completed_revision bigint,
 attempts integer NOT NULL DEFAULT 0, last_code text
);
CREATE INDEX purge_jobs_due ON purge_jobs(next_attempt_at,family_id) WHERE state<>'completed';
CREATE INDEX families_purge_due ON families(purge_after,id) WHERE status='deletion_pending';
```

Forced RLS; only projector inserts from Start. SQL functions `app.retention_due(p_now timestamptz,p_limit integer) RETURNS TABLE(family_id uuid,expected_revision bigint,purge_after timestamptz)`, `app.retention_claim(p_now timestamptz,p_limit integer,p_worker_id uuid) RETURNS SETOF public.purge_jobs`, `app.retention_delete_family(p_family_id uuid,p_start_revision bigint,p_lease_id uuid,p_now timestamptz) RETURNS uuid`, and `app.retention_release(p_family_id uuid,p_start_revision bigint,p_lease_id uuid,p_retry_at timestamptz,p_safe_code text) RETURNS boolean` are fixed-search_path, qualified, typed definer functions owned by NOLOGIN function owner with exact policies/privileges; PUBLIC/runtime EXECUTE revoked. Delete family resources in FK order: feed intervals/foods/timer intervals, timers/feeds/sleeps/diapers/babies, receipts/journal, pages/snapshots/import staging and completed mappings/receipts, delivery_pending, actor labels, family audit/operator projections, recovery claims/slots, invitations/entitlements, revoked family-linked sessions/history, memberships/family. Derive linked session IDs before removing memberships; preserve account-only sessions, global accounts/devices and other-family authority. No SQL/test mentions export/push-token tables at schema 14. Preserve only resumable purge receipt and minimal external terminal state.

- [ ] **Step 4 (4 min): Implement loop/leases.** Due→Execute(Start)→claim transaction committed→exact family delete transaction committed→Execute(Complete). Claim uses UPDATE-from-CTE SKIP LOCKED, expired lease reclaim, sql_done receipt reuse. Delete checks lease at transaction start and locks job so takeover waits until commit. Deadlines:claim/cleanup 5 s, family delete30s, lease 60 s; rollback whole family on timeout, retry 1 s–1 h. Limit 10 families. Completion acknowledged only after SQL receipt and committed Complete projection agree.
- [ ] **Step 5 (3 min): Write physical temporary-cleanup test.** Fixture `SeedExpiredTemporaryState(t)`, `TemporaryCounts(t) map[string]int`, `UnexpiredControlsRemain(t) bool` covers every following row family plus live controls.

```go
func TestExpiredTemporaryRowsPhysicallyDisappearForLiveFamily(t *testing.T) {
 h:=newPurgeFixture(t); h.SeedExpiredTemporaryState(t)
 for i:=0;i<20;i++ { if _,err:=h.Retention.CleanExpired(t.Context(),h.Clock.Now(),100); err!=nil { t.Fatal(err) } }
 for table,n:=range h.TemporaryCounts(t) { if n!=0 { t.Fatalf("expired rows remain: %s",table) } }
 if !h.UnexpiredControlsRemain(t) || h.State(h.FamilyID)!="active" { t.Fatal("cleanup crossed scope") }
}
```

| Temporary resource | Expiry/index/delete contract |
| --- | --- |
| rate_limit_buckets, all P1/P2 scopes | window_start≤now−24 h; validate all configured windows≤24 h; index(window_start,scope,key_digest). |
| auth_challenges | expires_at≤now; index(expires_at,id). |
| auth_exchange_material | All P1 Apple/session/family/profile staging uses this existing command_id-keyed store, never a second unnamed staging table. Add expires_at default now()+10 min and index(expires_at,command_id) in 000014. Under shared authority fence, require projection=head before deleting expired unconsumed staging, so committed grants project first. |
| apple_notification_dedup | accepted_at+30 days; additive expires_at and expiry/primary-key index; ledger receipt remains dedup authority. |
| apple_revocation_jobs/vault | Delete ciphertext/reference immediately after success; unsuccessful material expires created_at+30 days, safe terminal provider_revocation_unconfirmed, never restore account. Index(expires_at,id),(next_attempt_at,id); retry rechecks deadline before decrypt. |
| sync_page_sessions/sync_snapshots/entities | Existing900s default(60–3600 s); parent expires_at/id indexes, child(snapshot_id,position); child-first bounded delete, no read extends expiry. |
| incomplete import sessions and bulky children | Existing168h default(1–720 h); every `mapping_complete=false` session expires, including finalized ambiguity. Atomically mark `expired_at`, immediately deny all reuse as `import_expired`, then delete chunks/candidates/manifests/conflicts/owner decisions/conflict receipts/mappings in bounded child batches using parent `(expires_at,family_id,import_id)` partial and child `(family_id,import_id,...)` indexes. Retain the small session-ID row, counted against the 1,000/family and 100/initiating-account retained-history caps, until whole-family purge. Preserve completed sessions/mappings/receipts until family purge. |
| delivery_pending | created_at+24 h or invalid instance/epoch; shared authority fence; index(created_at,family_id,device_id). P4.2 adds lease-aware removal. |
| exports | P4.3 alone registers physical ciphertext/key/job cleanup after 000016. |

Cleanup holds shared fence before bounded SQL, verifies head/projection, deletes≤100 parent jobs/1000 child rows per call, ticks 30 s, immediately denies expired resources on access. Physical cleanup after outages is reported overdue, never falsely complete. Add `TestCleanupInterruptedBetweenBatches`, `TestUnprojectedStagingCannotBeErased`, `TestCompletedImportRetainsAdmission`.

- [ ] **Step 6 (4 min): Cleanup RED→implementation→GREEN.** Run `rtk proxy env BT_TEST_RUN='TestExpiredTemporary|TestCleanup|TestUnprojectedStaging|TestCompletedImport' make server-test` before functions/indexes, expect retained rows; implement typed bounded hooks/vault deletion retries/exact grants and repeat PASS.
- [ ] **Step 7 (4 min): Write checkpoint/erasure test.** `newCheckpointFixture(t)` runs local and hosted conformance adapters with test Ed25519 keys, actual temp backup files and methods `PurgeFamily(t)(string,time.Time)`, `Watermark(t,time.Time) BackupWatermark`, `Head() Head`, `DeleteLastContainingBackup(t)`, `AllRetainedLedgerBytes(t) []byte`, `Restart(t)`, `AssertNoFamilyAuthorityOrRows(t,id)`.

```go
func TestCheckpointActuallyErasesRetiredFamily(t *testing.T) {
 h:=newCheckpointFixture(t); id,purged:=h.PurgeFamily(t); before:=h.Head()
 early:=h.Watermark(t,purged)
 if _,err:=h.Retention.CompactSecurity(t.Context(),early); err!=nil { t.Fatal(err) }
 if !bytes.Contains(h.AllRetainedLedgerBytes(t),[]byte(id)) { t.Fatal("erased before backup cutoff") }
 h.DeleteLastContainingBackup(t); late:=h.Watermark(t,purged.Add(time.Microsecond))
 after,err:=h.Retention.CompactSecurity(t.Context(),late); if err!=nil { t.Fatal(err) }
 if after.Generation<=before.Generation || after.Revision<before.Revision { t.Fatal("head regressed") }
 h.Clock.Advance(time.Microsecond)
 retirementMark:=h.Watermark(t,purged.Add(time.Microsecond))
 if err:=h.Retention.RetireSecurity(t.Context(),retirementMark); err!=nil { t.Fatal(err) }
 if bytes.Contains(h.AllRetainedLedgerBytes(t),[]byte(id)) { t.Fatal("family survives retired objects/checkpoints/copies") }
 h.Restart(t); h.AssertNoFamilyAuthorityOrRows(t,id)
}
```

Use newer watermark inventory after publication for Retire; it has the same/later cutoff and higher sequence, and retirement proof binds the original checkpoint omission watermark plus the later physical-inventory watermark. `TestOlderDatabaseReconcilesAfterCheckpoint` restores a still-permitted older DB keeping current stores, rotates epoch/reconciles and denies removed grants. `TestBackupOlderThanErasureWatermarkDenied` refuses an obsolete backup; deliberately forced old rows remain quarantined and reconciliation removes absent family state.

- [ ] **Step 8 (2 min): Checkpoint RED.** Add `TestGenerationRetirementProofContract` in `Server/internal/retention/backup_watermark_test.go`: a capturing adapter implements `security.GenerationRetirer` (compile-time interface assertion), returns `security.GenerationInventory`, and records `security.RetirementProof`; assert exact inventory and checkpoint/omission/retirement watermark bindings, and no Retire call for incomplete/stale backup evidence. Keep DTO/proof validation tests in `Server/internal/security/retirement_test.go` and adapter conformance in each `generation_test.go`. Run `rtk proxy env BT_TEST_RUN='TestCheckpoint|TestWatermark|TestGenerationRetirement|TestOlderDatabase|TestBackupOlder' make server-test`; expect missing anchor/witness/retirer or retained old bytes, never an import cycle.
- [ ] **Step 9 (5 min): Implement exact encoding/witness/trust verification.** `VerifyCheckpoint(ctx,raw,[]TrustedSigner,instance,epoch)` and `VerifyWatermark(ctx,BackupWatermark,[]TrustedSigner,WatermarkWitness)` reject noncanonical bytes, unknown keys, cross-instance/epoch, modified state/fields, stale inventory or regression. Compare literal canonical preimage/domain bytes against independent fixed-key test oracle in `TestCheckpointCanonicalPreimage`/`TestWatermarkCanonicalPreimage`; mutate each binding. Epoch rotation requires fresh current-epoch watermark before compaction. Keep old public keys while material survives.
- [ ] **Step 10 (5 min): Implement generation replay and state projection.** Add `GenerationReplay{Anchor *SignedCheckpoint; Base Head}` and `OpenGeneration(ctx,Head)(GenerationReplay,error)` to adapters. Extend WalkCommitted from verified base+1 using P1 per-event checks. Reconcile installs complete checkpoint state under projector authority/quarantine, then events; no numeric-only rollback exceptions. Publication disabled until checkpoint-aware readers upgraded. CAS ambiguity rereads exact head; no overwrite/forced head repair.
- [ ] **Step 11 (4 min): Implement whole-generation retire adapters.** Define the shared inventory/proof/port in `security/retirement.go`; localstore/hosted implementations use `security.GenerationInventory` and `security.RetirementProof`, and retention builds/passes those exact types through its injected `security.GenerationRetirer`. Separate maintenance credentials/API, exact enumerated files/versions, finite retain-until/holds, durable progress, empty readback, current-directory/symlink denial. Keep independent proof metadata free of erased family IDs. No prefix deletion exposed. Verify acyclic production imports with `rtk proxy sh -c 'cd Server && go list -deps ./internal/security/... ./internal/retention/...'`; run `TestGenerationRetirementProofContract` with the checkpoint GREEN matrix. Stage `security/retirement.go`, `security/retirement_test.go`, `retention/backup_watermark.go`, `retention/backup_watermark_test.go`, and both adapter `generation.go`/`generation_test.go` files in the Step 15 exact file list below.
- [ ] **Step 12 (4 min): Run fault matrix and round-trip decisions.** Fixture `ExerciseAndAssertFault(t,string)` injects boundary faults and restarts all in-memory state; expected preCAS old head, postCAS new replay once, corruption/truncation/unknown signer quarantine, rotated trusted signer success, retirement current generation intact.

```go
func TestCheckpointFaultMatrix(t *testing.T) {
 for _,p:=range []string{"before_publish","after_publish_before_cas","after_cas_before_projection","during_retirement","candidate_cas_lost","checkpoint_corrupt","event_missing_middle","valid_prefix_truncated","old_matching_pair","unknown_signer","rotated_signer"} {
  t.Run(p,func(t *testing.T){newCheckpointFixture(t).ExerciseAndAssertFault(t,p)})
 }
}
```

`TestWatermarkIndependentOfDBRestore` rejects older signed sequence after SQL restore. `TestStateCheckpointPreservesDecisions` compares every P1 grant/revocation/session/recovery/receipt decision before/after round-trip. `TestErasureRemovesEveryObjectVersion` scans actual family-ID bytes in local/decrypted provider objects and copies. Run `rtk proxy env BT_TEST_RUN='TestPurge|TestRetention|TestCleanup|TestExpiredTemporary|TestCheckpoint|TestWatermark|TestGeneration|TestStateCheckpoint|TestErasure|TestOlderDatabase' make server-test` GREEN.
- [ ] **Step 13 (4 min): Wire worker/role/deployment.** `baby-tracker-worker run --kind retention` receives only its worker pool plus authority/projector collaborators, cancellation/bounded loops, distinct purge/export/delivery NOLOGIN groups/login secrets. Request Options has no maintenance pool. Missing signer/inventory disables compaction/retirement with explicit status, but purge/temp cleanup continues. Add nonroot worker binary/retention Compose service; hosted never falls back to filesystem.
- [ ] **Step 14 (4 min): Finish manifest `[14,14]`, run feature gates, and prepare 13→14 overlap.** Add `TestMigration000014CompatibilityOverlap` initially RED. Generate the feature manifest and run source gates now; the actual prepared-mode overlap runs immediately after the feature commit in Step 15:

```bash
rtk proxy sh -c 'cd Server && go run ./cmd/baby-tracker-manifest --min 14 --max 14 --output internal/migrate/compatibility.json'
rtk make server-test
rtk make server-smoke
rtk proxy sh -c 'cd Server && go vet ./...'
rtk git diff --check
```

Both binaries perform prior sync/import at 14 with checkpoints disabled; new worker proves purge. Wrong14hash/unknown15 deny. Smoke proves runtime cannot enumerate/claim/delete worker state and removes only exact test resources.

- [ ] **Step 15 (2 min): Scoped commit.**

```bash
rtk git add Server/internal/retention/{model,repository,purge,cleanup,backup_watermark}.go Server/internal/retention/{purge,cleanup,backup_watermark}_test.go Server/internal/security/{checkpoint,checkpoint_state,retirement}.go Server/internal/security/{checkpoint,retirement}_test.go Server/internal/security/localstore/{generation.go,generation_test.go,ledger.go,head.go} Server/internal/security/hosted/{generation.go,generation_test.go,contracts.go,ledger.go,head.go} Server/internal/security/{types,coordinator,projector,commands,state}.go Server/internal/security/storetest/conformance.go Server/internal/recovery/reconcile.go Server/cmd/baby-tracker-worker/{main.go,main_test.go} Server/cmd/baby-tracker-admin/{main.go,main_test.go} Server/migrations/000014_retention_workers.sql Server/internal/migrate/{compatibility.json,overlap_test.go} Server/internal/config/{config.go,retention.go,retention_test.go} Server/internal/testkit/workers.go Server/internal/tenant/roles_test.go Server/deploy/postgres/{init-roles.sql,init-users.sh} Server/scripts/{check-principals.sh,check-migration-overlap.sh,smoke.sh,smoke_test.go} Server/compose.yaml Server/Dockerfile Server/docs/operations/purge.md
rtk git diff --cached --check
rtk git commit -m "feat(server): add resumable purge and security generation erasure"
BT_MIGRATION_FEATURE_REF=$(rtk git rev-parse HEAD)
rtk proxy bash Server/scripts/check-migration-overlap.sh --base-ref "$BT_MIGRATION_BASE_REF" --prior-ref "$BT_MIGRATION_PRIOR_REF" --feature-ref "$BT_MIGRATION_FEATURE_REF" --next-version 14 --prior-min 13 --prior-max 14 --next-min 14 --next-max 14
```

---

### Task 2: Generation-safe APNs queue and exact device registration (P4.2)

- [ ] **Step 0 (4 min): Land the deployable schema-15 compatibility predecessor before feature work.** Capture accepted schema-14 feature head as `BT_MIGRATION_BASE_REF`. Write final 000015, complete exact v15 privilege snapshot, typed push cleanup capability, overlap tests/scenarios, and manifest `[14,15]`; run the predecessor suite on schema 14; commit only compatibility files as `feat(server): prepare push schema compatibility`; review it and capture `BT_MIGRATION_PRIOR_REF`. No predecessor path constructs APNs delivery/token code. Later Task 2 work is the `[15,15]` feature commit.

**Files:**

- Create: `Server/internal/delivery/{apns,provider_token,worker,worker_repository,token}.go`, `{apns,worker,worker_repository,token}_test.go`; `Server/internal/httpapi/{devices.go,devices_test.go}`; `Server/internal/protocol/devices.go`; `Server/internal/config/{push.go,push_test.go}`; `Server/migrations/000015_push_tokens.sql`; `Server/api/fixtures/device-registration.json`.
- Create: `Packages/BabyTracker/Sources/BabyProtocol/DeviceRegistrationDTOs.swift`, `Packages/BabyTracker/Sources/BabySync/DeviceRegistration.swift`, `Packages/BabyTracker/Tests/BabySyncTests/DeviceRegistrationTests.swift`, `Apps/BabyTrackerApp/PushNotificationCoordinator.swift`.
- Modify: `Server/internal/delivery/{queue,repository}.go`, `Server/internal/retention/{cleanup.go,cleanup_test.go}`, `Server/internal/testkit/workers.go`, `Server/internal/config/config.go`, `Server/internal/httpapi/router.go`, `Server/internal/protocol/decode.go`, `Server/api/openapi.yaml`, `Server/internal/migrate/{compatibility.json,overlap_test.go}`, `Server/scripts/{check-principals.sh,check-migration-overlap.sh}`, `Server/cmd/baby-tracker-worker/{main.go,main_test.go}`, `Server/compose.yaml`, `Apps/BabyTrackerApp/{BabyTrackerApp,AppEnvironment}.swift`, `Apps/BabyTracker.entitlements`, `BabyTracker.xcodeproj/project.pbxproj`, `Makefile`.

**Interfaces:** Modify the existing P2 struct, preserve its three method signatures, and extend the worker-only port:

```go
type PendingWake struct { // P2 declaration; add fields here, never redeclare elsewhere.
 FamilyID,DeviceID string; Cursor protocol.Cursor
 NotBefore time.Time; Attempts uint32; Generation uint64
 LeaseID string; LeaseUntil time.Time; TokenVersion uint64
}
type WorkerRepository interface {
 Claim(context.Context,time.Time,int) ([]PendingWake,error)
 Complete(context.Context,PendingWake) error
 Retry(context.Context,PendingWake,time.Time,string) error
}
type DeliveryRepository interface {
 WorkerRepository
 Drop(context.Context,PendingWake,string) error
 Token(context.Context,PendingWake) (DeviceToken,error)
 RemoveToken(context.Context,PendingWake) error
}
type DeviceToken struct { DeviceID,Environment,Topic string; Version uint64; Bytes []byte; ExpiresAt time.Time }
type DeliveryResult struct { Kind,Code string; RetryAfter time.Duration } // delivered/retry/invalid_token/permanent
type PushProvider interface { Wake(context.Context,DeviceToken) (DeliveryResult,error) }
func NewWorker(DeliveryRepository,PushProvider,*security.Service,func()time.Time) *Worker
func (w *Worker) RunOnce(context.Context,int) error
```

Claim limit 1–100, UUID lease 30 seconds, APNs deadline 5 seconds, at most three reserved send attempts/device/rolling hour, work expires at current-generation `created_at+24 h`. P2.6 refreshes `created_at` whenever a genuinely newer cursor creates a generation. Migration adds `lease_id,lease_until,claimed_instance_id,claimed_recovery_epoch,claimed_sequence,claimed_generation,claimed_token_version,last_code` to `delivery_pending`; `push_tokens(device_id PRIMARY KEY,account_id,version,environment,topic,key_id,nonce,ciphertext,token_hmac,registered_at)`; `push_token_versions(device_id uuid PRIMARY KEY REFERENCES devices(id),last_version bigint NOT NULL CHECK(last_version>0))`, retained on token removal and atomically incremented under the device-row lock before registration; `delivery_send_attempts(device_id,attempt_id,attempted_at)` indexed `(device_id,attempted_at)`, expired after one hour. Account/family RLS remains forced; runtime cannot enumerate/decrypt tokens.

Encrypt token with AES-256-GCM, mounted versioned key, random 12-byte nonce, AAD exactly `"baby-tracker-apns-token-v1\x00" || instanceUUID[16] || deviceUUID[16] || u64be(version) || environment || NUL || topic`. Separate HMAC pepper identifies exact token idempotency; no token/HMAC logging. Keep old keys until envelopes retire.

Claim's short SKIP LOCKED transaction rechecks due/expiry/current instance+epoch/token, records the full claimed `(instance_id,recovery_epoch,sequence)` cursor plus random lease/claimed generation/token version, reserves attempt, calculates persisted throttle, commits before network. N→N+1 coalescing preserves the old live lease/token snapshot/not_before; no concurrent next-generation send. Epoch change invalidates lease and replaces old sequence. Complete/retry/drop compare the full claimed cursor plus generation, lease and token version so delete/reinsert ABA cannot consume replacement work. If N+1 now exists with the old lease, Complete clears only that matching lease, preserving new work/cursor/throttle. Retry mutates attempts/not_before only for N; on N+1 it releases only its old lease and preserves/raises shared throttle without replacing new work. Drop uses identical guards. Stale lease is no-op.

Claim selects queue plus token rows `FOR UPDATE OF q,t SKIP LOCKED`; the token row serializes that device's rolling attempt reservation. Under those locks count attempts newer than now−1h, reserve only below three, update the claimed full cursor/generation/token/lease; otherwise set not_before=oldest retained attempt+1h without claiming/sending. Complete uses these exact alternatives in one short transaction (`$1=family,$2=device,$3=lease,$4=instance,$5=epoch,$6=sequence,$7=generation,$8=token version`):

```sql
DELETE FROM delivery_pending
WHERE family_id=$1 AND device_id=$2 AND lease_id=$3
  AND instance_id=$4 AND recovery_epoch=$5 AND sequence=$6 AND generation=$7
  AND claimed_instance_id=$4 AND claimed_recovery_epoch=$5 AND claimed_sequence=$6
  AND claimed_generation=$7 AND claimed_token_version=$8;
UPDATE delivery_pending SET lease_id=NULL,lease_until=NULL,
  claimed_instance_id=NULL,claimed_recovery_epoch=NULL,claimed_sequence=NULL,
  claimed_generation=NULL,claimed_token_version=NULL
WHERE family_id=$1 AND device_id=$2 AND lease_id=$3
  AND generation>$7
  AND claimed_instance_id=$4 AND claimed_recovery_epoch=$5 AND claimed_sequence=$6
  AND claimed_generation=$7 AND claimed_token_version=$8;
```

Retry substitutes `attempts=attempts+1,not_before=GREATEST(not_before,$retryAt),last_code=$safeCode` and clears lease on the exact-generation predicate; for the newer-generation predicate it only clears the old lease and raises not_before to persisted device throttle. Drop uses Complete's predicates and never replaces a newer cursor. RemoveToken executes `DELETE FROM push_tokens WHERE device_id=$1 AND version=$2`, retaining the separate monotonic version counter. Token lookup requires valid lease even after process pause.

Before sending, hold a shared P1 fence, require projection=head, live account/device/family/entitlement, current epoch and token version; keep it through the bounded send, then release. Token loads only for matching nonexpired lease_id/claimed_generation/claimed_token_version, even if the coalesced current generation advanced; it sets DeviceToken.ExpiresAt from work created_at+24 h. A paused stale claimant cannot send after takeover. Revocation committed first prevents send; revocation beginning during send waits for this bounded operation. Recheck before completion. Death after send retains the throttle reservation and leased work; after expiry a duplicate hint is safe. 410/removal uses `WHERE device_id=$id AND version=$claimedVersion`; replacement V+1 survives. No-token/revoked/old-epoch work drops through qualified API. Task 1 delivery cleanup uses the same claim-aware guards.

**HTTP contract:** Hosted-only PUT/DELETE `/v1/devices/{device_id}/push-token`; self-host registers neither route and returns **404**, stores no tokens and requires no APNs key. PUT requires current principal's exact device, `push-token-v1`, body≤4 KiB:

```json
{"protocol_version":"v1","capabilities":["push-token-v1"],"token":"00112233445566778899aabbccddeeff00","environment":"sandbox"}
```

Token is lowercase even hex, 16–256 decoded bytes; no fixed APNs token length assumption. Environment sandbox/production must equal configured server environment; topic is server configuration only. Same current token/environment returns 200 without version bump; replacement returns 200 `{protocol_version,device_id,token_version,registered_at}` after increment. DELETE: negotiation headers, no body, idempotent204. Cross-account/device404; invalid token400 `invalid_push_token`; wrong environment409 `push_environment_mismatch`; capability409 `unsupported_capability`; P1 session/epoch errors; quota 429 at 10/device/hour and30/account/hour via existing limiter.

Swift `PushEnvironment: String,Codable,Sendable` cases sandbox/production; `PushTokenRequest{protocolVersion:String,capabilities:[String],token:String,environment:PushEnvironment}`, `PushTokenResponse{protocolVersion:String,deviceID:UUID,tokenVersion:UInt64,registeredAt:String}` with explicit snake_case CodingKeys. `DeviceRegistration.register(token:Data,environment:PushEnvironment,session:FamilySessionKey) async throws -> PushTokenResponse`, `unregister(session:FamilySessionKey) async throws`, `registerIfSupported(token:Data) async throws` reuse P3 transport/session. Last convenience resolves current session/config and sends only when capability advertised. Implement package-owned `BackgroundWakeHandler` in `BabySync/DeviceRegistration.swift`: initializer accepts `sync: @Sendable (SyncReason) async throws -> Void`, injected deadline clock, and deadline defaulting to 25 seconds; `receiveSilentPush(completion: @Sendable () async -> Void) async` runs one `.backgroundPush` sync and invokes completion exactly once after success, failure, timeout, or cancellation. The app adapter delegates and maps its UIKit completion result; package tests never import the app target. No raw token persistence in SwiftData/logs.

- [ ] **Step 1 (3 min): Capture prior ref and write race tests.** `newDeliveryFixture(t)` embeds WorkerHarness with `Repo DeliveryRepository`, `Worker *Worker`, barrier provider; `Schedule(t)`, `ReplaceToken(t) uint64`, `Pending(t) PendingWake`, `CurrentTokenVersion(t) uint64` use real P2 queue/registration HTTP, assertion-only SQL reads.

```go
func TestGenerationNCompletionPreservesNPlusOne(t *testing.T) {
 h:=newDeliveryFixture(t); h.Schedule(t)
 claim,err:=h.Repo.Claim(t.Context(),h.Clock.Now(),1); if err!=nil || len(claim)!=1 { t.Fatal("claim") }
 h.Schedule(t)
 if err:=h.Repo.Complete(t.Context(),claim[0]); err!=nil { t.Fatal(err) }
 got:=h.Pending(t)
 if got.Generation!=claim[0].Generation+1 || got.LeaseID!="" { t.Fatal("new generation lost or stuck") }
}
func TestReplacedTokenSurvivesOld410(t *testing.T) {
 h:=newDeliveryFixture(t); h.Schedule(t)
 claim,err:=h.Repo.Claim(t.Context(),h.Clock.Now(),1); if err!=nil { t.Fatal(err) }
 newer:=h.ReplaceToken(t)
 if err:=h.Repo.RemoveToken(t.Context(),claim[0]); err!=nil { t.Fatal(err) }
 if h.CurrentTokenVersion(t)!=newer { t.Fatal("old response erased replacement") }
}
```

- [ ] **Step 2 (2 min): RED.** `rtk proxy env BT_TEST_RUN='TestGenerationN|TestReplacedToken|TestAPNS|TestDelivery|TestPush' make server-test`; missing worker/token behavior, no skipped DB tests.
- [ ] **Step 3 (5 min): Implement migration/qualified SQL.** Fixed typed functions `app.delivery_claim`, `delivery_complete`, `delivery_retry`, `delivery_drop`, `delivery_token`, `delivery_remove_token`; delivery-only EXECUTE, fixed search_path, no arbitrary device enumeration. Runtime gets only authenticated register/remove functions. Add `(not_before,lease_until)` index and expiry indexes. Modify P2 ScheduleFamily to preserve same-epoch claim/throttle and invalidate old-epoch claims. Register token/attempt cleanup; delete tokens only for devices with no remaining eligible family authority, not arbitrary supplied family IDs.
- [ ] **Step 4 (4 min): Implement APNs adapter/worker.** Fixed Apple sandbox/production HTTPS endpoints, HTTP/2 required, no redirect. Exact headers background push-type, priority 5, configured topic, collapse-id lowerhex HMAC device bucket, expiration=min(now+1 h,work expiry); body `{"aps":{"content-available":1}}`. ES256 JWT uses raw 64-byte r||s; cache 20–50 min. 200 delivered; 410 and400 BadDeviceToken/DeviceTokenNotForTopic invalid_token; 429/5xx/network retry; remaining permanent safe codes. Full-jitter retry base 1 s/cap 1 h, respect bounded Retry-After/work expiry; throttle counts reserved sends even after death.
- [ ] **Step 5 (4 min): Add full lease/provider matrix and GREEN.** `TestDeliveryLeaseRaceMatrix` cases death_after_send/two_workers/token_replaced_before_send/revoked_before_send/epoch_change_while_claimed/retry_N_after_N_plus_one/stale_lease_complete/throttle_restart; assert actual provider counts, surviving generation/token version/cursor/attempt rows. Two workers use distinct pools and barriers. `TestAPNSRequestContract` checks actual HTTP/2/headers/body/raw signature/cache boundaries; `TestAPNSResponseCodes` literal statuses/reasons. Repeat Step 2 PASS.
- [ ] **Step 6 (4 min): Swift/HTTP RED→implementation→GREEN.** `PushFixture.hosted/selfHosted` constructs injected transport/session and package-owned BackgroundWakeHandler as `coordinator`; completion/sync probes are actors. The sync probe switches on SyncReason and records `reasonNames: [String]`, avoiding an undeclared Equatable conformance. Keep the app delegate a thin UIKit completion adapter; exercise real-app convergence through Task 6's existing E2E target rather than inventing an app unit-test target.

```swift
@Test func pushCallbackAlwaysCompletesOnce() async throws {
 let f = PushFixture.hosted()
 await f.coordinator.receiveSilentPush(completion: f.completion.call)
 #expect(await f.sync.reasonNames == ["backgroundPush"])
 #expect(await f.completion.count == 1)
}
@Test func selfHostedNeverRegistersAPNSToken() async throws {
 let f = PushFixture.selfHosted()
 try await f.registration.registerIfSupported(token: Data(repeating: 1, count: 32))
 #expect(await f.transport.requestCount == 0)
}
```

Run `rtk swift test --package-path Packages/BabyTracker --filter DeviceRegistrationTests` RED, implement DTO/client/callback/entitlements, repeat GREEN. HTTP tests assert every status/body above, strict keys, token size, idempotency/replacement, rate limiting and self-host404. Add success/error/timeout/cancel completion tests and authoritative convergence with dropped/duplicated/delayed/reordered/rejected wakes.
- [ ] **Step 7 (4 min): Finish manifest `[15,15]` and feature regression.** Add `TestMigration000015CompatibilityOverlap` RED against the predecessor manifest. Generate the feature manifest and run source/client gates now; prepared-mode overlap runs immediately after the Step 8 feature commit:

```bash
rtk proxy sh -c 'cd Server && go run ./cmd/baby-tracker-manifest --min 15 --max 15 --output internal/migrate/compatibility.json'
rtk make server-test
rtk swift test --package-path Packages/BabyTracker --filter DeviceRegistrationTests
rtk make test
rtk git diff --check
```

Update Makefile EXPECTED_TESTS to measured literal total before counted gate. Prior binary's sync/purge works at 15; new registers/sends with exact role denial. Actual APNs/key/device remains external.

- [ ] **Step 8 (2 min): Commit including 000015/count/project.**

```bash
rtk git add Server/internal/delivery/{apns,provider_token,worker,worker_repository,token,queue,repository}.go Server/internal/delivery/{apns,worker,worker_repository,token}_test.go Server/internal/httpapi/{devices.go,devices_test.go,router.go} Server/internal/protocol/{devices.go,decode.go} Server/internal/config/{push.go,push_test.go,config.go} Server/migrations/000015_push_tokens.sql Server/internal/migrate/{compatibility.json,overlap_test.go} Server/scripts/{check-principals.sh,check-migration-overlap.sh} Server/internal/retention/{cleanup.go,cleanup_test.go} Server/internal/testkit/workers.go Server/cmd/baby-tracker-worker/{main.go,main_test.go} Server/api/fixtures/device-registration.json Server/api/openapi.yaml Server/compose.yaml Packages/BabyTracker/Sources/BabyProtocol/DeviceRegistrationDTOs.swift Packages/BabyTracker/Sources/BabySync/DeviceRegistration.swift Packages/BabyTracker/Tests/BabySyncTests/DeviceRegistrationTests.swift Apps/BabyTrackerApp/{PushNotificationCoordinator,BabyTrackerApp,AppEnvironment}.swift Apps/BabyTracker.entitlements BabyTracker.xcodeproj/project.pbxproj Makefile
rtk git diff --cached --check
rtk git commit -m "feat(sync): deliver generation-safe hosted background wakes"
BT_MIGRATION_FEATURE_REF=$(rtk git rev-parse HEAD)
rtk proxy bash Server/scripts/check-migration-overlap.sh --base-ref "$BT_MIGRATION_BASE_REF" --prior-ref "$BT_MIGRATION_PRIOR_REF" --feature-ref "$BT_MIGRATION_FEATURE_REF" --next-version 15 --prior-min 14 --prior-max 15 --next-min 15 --next-max 15
```

---

### Task 3: Consistent, rate-limited, encrypted owner JSON export (P4.3)

- [ ] **Step 0 (4 min): Land the deployable schema-16 compatibility predecessor before feature work.** Capture accepted schema-15 feature head as `BT_MIGRATION_BASE_REF`. Write final 000016, complete exact v16 privilege snapshot, typed export cleanup capability, overlap tests/scenarios, and manifest `[15,16]`; run the predecessor suite on schema 15; commit only compatibility files as `feat(server): prepare export schema compatibility`; review it and capture `BT_MIGRATION_PRIOR_REF`. No predecessor path constructs export services. Later Task 3 work is the `[16,16]` feature commit.

**Files:**

- Create: `Server/internal/export/{model,archive,envelope,repository,service,worker,cleanup}.go`, `{export,envelope,worker}_test.go`; `Server/internal/protocol/export.go`; `Server/internal/httpapi/{export.go,export_test.go}`; `Server/internal/config/{export.go,export_test.go}`; `Server/migrations/000016_exports.sql`; `Server/api/fixtures/{export-request,export-status,export-archive-v1}.json`.
- Create: `Packages/BabyTracker/Sources/BabyProtocol/ExportDTOs.swift`, `Packages/BabyTracker/Sources/BabySync/{ExportClient,ExportTemporaryFiles}.swift`, `Packages/BabyTracker/Sources/SettingsFeature/{FamilyExportView,FamilyExportViewModel}.swift`, `Packages/BabyTracker/Tests/BabySyncTests/ExportClientTests.swift`, `Packages/BabyTracker/Tests/SettingsFeatureTests/FamilyExportViewModelTests.swift`.
- Modify: `Server/internal/security/{transaction.go,transaction_test.go}`, `Server/internal/retention/{cleanup.go,cleanup_test.go,purge_test.go}`, `Server/internal/testkit/workers.go`, `Server/internal/httpapi/router.go`, `Server/internal/protocol/decode.go`, `Server/internal/config/config.go`, `Server/internal/limits/policy.go`, `Server/internal/migrate/{compatibility.json,overlap_test.go}`, `Server/scripts/{check-principals.sh,check-migration-overlap.sh}`, `Server/cmd/baby-tracker-worker/{main.go,main_test.go}`, `Server/api/openapi.yaml`, `Server/compose.yaml`, `Packages/BabyTracker/Sources/SettingsFeature/SettingsView.swift`, `Apps/BabyTrackerApp/AppEnvironment.swift`, `Makefile`.

**Job/API interfaces:**

```go
type CreateExportRequest struct { protocol.RequestMeta; RequestID string `json:"request_id"` }
type ExportStatus struct {
 ProtocolVersion string `json:"protocol_version"`; ExportID string `json:"export_id"`; State string `json:"state"`
 CreatedAt string `json:"created_at"`; ExpiresAt string `json:"expires_at"`
 PlaintextBytes *uint64 `json:"plaintext_bytes"`; SHA256 *string `json:"sha256"`; Code *string `json:"code"`
}
type Job struct { ID,FamilyID,RequesterAccountID,RequestID,State string; CreatedAt,ExpiresAt time.Time; AuthorityRevision uint64 }
type Claim struct { Job Job; LeaseID string; LeaseUntil time.Time; Attempt uint32 }
type WorkerRepository interface {
 Claim(context.Context,time.Time,int) ([]Claim,error)
 Complete(context.Context,Claim,EnvelopeHeader) error
 Retry(context.Context,Claim,time.Time,string) error
 Fail(context.Context,Claim,string) error
}
func (s *Service) Create(context.Context,access.Principal,CreateExportRequest) (ExportStatus,bool,error)
func (s *Service) Status(context.Context,access.Principal,string) (ExportStatus,error)
func (s *Service) Download(context.Context,access.Principal,string,io.Writer) error
func (w *Worker) RunOnce(context.Context,int) error
```

POST `/v1/export`:≤4 KiB `{protocol_version:"v1",capabilities:["export-v1"],request_id:UUID}`; 202 new, 200 same-owner idempotent request ID (bool=new). GET `/v1/export/{id}`200 status. GET `/v1/export/{id}/download`200 ready; 409 export_not_ready; 410 export_expired; 404 unknown/another requester's ID; 403 owner_required after transfer, P1 revocation/epoch/quarantine errors. Every call checks active current owner, same requester/family/current instance+epoch, P1 export action/entitlement; no recovery/account-only scope. Capability grammar shared.

One active job (`queued|running|ready`) per family, enforced partial unique family index. One new export per **family and account separately per rolling six hours** even after failed/deleted job; separate `export_rate_reservations(scope,key_digest,accepted_at)` retains quota until six hours. Identical request replay charges nothing; new request while active 409 export_in_progress, exhausted either quota 429 export_rate_limited plus integer Retry-After. Existing limiter also applies create 10/IP/hour and status/download 60/account/minute+30/device/minute. Admission transaction locks family and account quota key; no concurrency bypass.

State:queued→running→ready; retryable running→queued with full jitter1s–5 min, max five attempts; permanent size/authorization/integrity→failed; all states→expired at **created_at+24 h**. Ownership/membership/deletion/epoch change makes nonterminal jobs failed/export_authorization_changed and erases ready material. Lease random UUID 60 s, generation deadline 20 s, claims 1–4. Every write/completion guards lease and expiry. Reclaim deletes old-attempt chunks/key refs and uses new key+prefix; never reuse a GCM nonce/key across changed retries. Request idempotency unique(requester_account_id,request_id).

**Exact archive JSON:** `ArchiveV1` fixed order `{version,server_instance_id,recovery_epoch,family_id,captured_at,high_water,security_revision,members,babies,feeds,sleeps,diapers,active_timers,journal,audit,attachments}`. Version v1; UUIDs lowercase; times UTC 6; high_water P2 Cursor. Members `{account_id,display_name,role,status}` sorted account UUID; departed/deleted names exactly Former caregiver. Entity arrays P2 CanonicalEntity, UUID-sorted per kind including tombstones; P2 decimal/date/null/canonical payload rules, no photo/presentation fields. Journal P2 JournalChange sorted sequence≤high_water. Audit `{revision,kind,actor_account_id,at}` sorted revision, no raw payload/digest/secret. Attachments exactly `{"version":"v1","attachments":[]}`. Empty arrays[], deterministic no whitespace/newline; fixed injected captured_at makes golden bytes reproducible. Limits: 52,428,800 plaintext bytes (50 MiB), 53,477,376 ciphertext bytes (51 MiB); entire attempt rolls back with export_too_large on overflow.

```go
type ArchiveMember struct {
 AccountID string `json:"account_id"`; DisplayName string `json:"display_name"`; Role string `json:"role"`; Status string `json:"status"`
}
type ArchiveAudit struct {
 Revision uint64 `json:"revision"`; Kind string `json:"kind"`; ActorAccountID string `json:"actor_account_id"`; At string `json:"at"`
}
type AttachmentManifest struct { Version string `json:"version"`; Attachments []protocol.AttachmentMetadata `json:"attachments"` }
type ArchiveV1 struct {
 Version string `json:"version"`; ServerInstanceID string `json:"server_instance_id"`; RecoveryEpoch uint64 `json:"recovery_epoch"`
 FamilyID string `json:"family_id"`; CapturedAt string `json:"captured_at"`; HighWater protocol.Cursor `json:"high_water"`; SecurityRevision uint64 `json:"security_revision"`
 Members []ArchiveMember `json:"members"`; Babies []protocol.CanonicalEntity `json:"babies"`; Feeds []protocol.CanonicalEntity `json:"feeds"`
 Sleeps []protocol.CanonicalEntity `json:"sleeps"`; Diapers []protocol.CanonicalEntity `json:"diapers"`; ActiveTimers []protocol.CanonicalEntity `json:"active_timers"`
 Journal []protocol.JournalChange `json:"journal"`; Audit []ArchiveAudit `json:"audit"`; Attachments AttachmentManifest `json:"attachments"`
}
```

These types define the wire shape; the encoder streams each array from ordered snapshot queries instead of constructing all slices in memory. AttachmentManifest always encodes an allocated empty slice in beta.

Add `security.Service.WithAuthorizedSnapshot(ctx,p,familyID,action,fn func(pgx.Tx,access.Grant)error) error` for the current authenticated HTTP principal: shared P1 fence/reauthorization, read-only **REPEATABLE READ**, tenant/account settings before SQL, fence held through callback. For queued generation define `security.ExportSubject{JobID,LeaseID,FamilyID,RequesterAccountID,InstanceID string; RecoveryEpoch uint64}` and `security.ExportSnapshotGuard.WithExportSnapshot(context.Context,ExportSubject,func(pgx.Tx,access.Grant)error) error`. Its constructor `NewExportSnapshotGuard(*security.Service,*pgxpool.Pool) (*ExportSnapshotGuard,error)` is used only by worker composition with the export-role pool. The guard verifies a live matching claimed job/lease, current instance/epoch, active requester identity and current owner/family/export entitlement from authority; it does not impersonate an expired customer bearer session or accept a request-supplied worker scope. Query canonical rows, labels, journal high-water and audit in that one snapshot/current security revision; canonical commits are wholly before/after, lifecycle waits for bounded fence. Reauthorize subject before conditional ready commit. Download receives only request-role narrowly filtered ready-read access.

**Exact chunk envelope:** HeaderCore uses the fixed fields/tags below; prefix is four random bytes encoded unpadded base64url. Random 32-byte per-attempt data key; AES-256-GCM wraps it under mounted 32-byte KEK selected by key_id, independent random 12-byte wrap nonce, AAD exactly `"baby-tracker-export-key-v1\x00" || canonical(HeaderCore)`, producing a 48-byte wrapped key. Finalize core only after count/hash; wrapping authenticates every header field. Best-effort zero key buffers, no logs/plaintext persistence.

```go
type HeaderCore struct {
 Version string `json:"version"`; ExportID string `json:"export_id"`; InstanceID string `json:"instance_id"`
 FamilyID string `json:"family_id"`; RequesterAccountID string `json:"requester_account_id"`
 CreatedAt string `json:"created_at"`; ExpiresAt string `json:"expires_at"`; KeyID string `json:"key_id"`; NoncePrefix string `json:"nonce_prefix"`
 PlaintextBytes uint64 `json:"plaintext_bytes"`; PlaintextSHA256 string `json:"plaintext_sha256"`; ChunkCount uint64 `json:"chunk_count"`
}
type EnvelopeHeader struct { Core HeaderCore `json:"core"`; WrapNonce string `json:"wrap_nonce"`; WrappedKey string `json:"wrapped_key"` }
```

`BT_EXPORT_KEY_ID` selects the active key in `BT_EXPORT_KEYS_FILE`, a bounded 0600 nonsymlink JSON map from key IDs to 32-byte lowercase-hex keys. `BT_APNS_TOKEN_KEYS_FILE`/`BT_APNS_TOKEN_KEY_ID` use identical file validation with distinct key material. Unknown/removed keys fail closed; tests retain old keys until their envelopes are physically erased.

Plaintext chunks exactly 65,536 bytes except final 0–65,535; exact multiples/empty stream append empty final chunk. Index starts 0, contiguous. Nonce=`prefix[4] || uint64be(index)`. GCM tag 16. AAD exactly `"baby-tracker-export-chunk-v1\x00" || exportUUID[16] || uint64be(index) || uint32be(plaintext_length) || finalByte(0|1)`. Stored `(family_id,export_id,attempt_id,index,plaintext_length,is_final,ciphertext)`, composite FK/PK(export_id,attempt_id,index), ciphertext length=plaintext+16. Decoder requires header count/total/SHA, exact ordering and final position, no gaps/duplicates/trailing frames, all tags.

Serialized internal storage/test envelope: bytes `42 54 45 58 01` (BTEX+v1), uint32be header length≤4096, canonical header, ordered frames `uint32be plaintext_length || finalByte || ciphertext`; frame index implicit. Reject trailing bytes. DB stores header/chunks only. `SealArchive(ctx,exportID,io.Reader,ChunkSink)(EnvelopeHeader,error)`, `OpenArchive(ctx,EnvelopeHeader,ChunkSource,io.Writer) error`; ports `ChunkSink.Put(ctx,Chunk) error`, `ChunkSource.Each(ctx,func(Chunk)error) error`, `Chunk{Index uint64; PlaintextLength uint32; Final bool; Ciphertext []byte}`.

HTTP download is decrypted JSON, **not** envelope/key. Hold shared authorization fence plus repeatable-read immutable ready archive; first bounded-memory pass verifies all tags/order/hash before any HTTP byte, second pass streams plaintext. Content-Type application/json; Content-Disposition fixed baby-tracker-family-v1.json; Content-Length exact; Cache-Control no-store; no object URL/redirect. Deadline min(now+30 s,expires_at), expiry checked before each chunk. Ownership committed first yields zero bytes; ownership begun during stream waits for bounded fence, so no byte is authorized after a committed transfer. Already sent bytes cannot be recalled. Timeout/cancel releases fence, truncates stream and client removes file.

Swift `ExportState: String,Codable,Sendable` cases queued/running/ready/failed/expired; `CreateExportRequest{protocolVersion:String,capabilities:[String],requestID:UUID}`; `ExportStatusDTO{protocolVersion:String,exportID:UUID,state:ExportState,createdAt:String,expiresAt:String,plaintextBytes:UInt64?,sha256:String?,code:String?}` explicit snake_case keys. `ExportClient.request(requestID:UUID) async throws -> ExportStatusDTO`, `status(id:UUID) async throws -> ExportStatusDTO`, `download(id:UUID) async throws -> URL`; errors notReady/expired/ownerRequired/requesterMismatch/rateLimited(retryAfter:TimeInterval)/integrity/tooLarge/authorizationChanged. Download into unique app-private temp directory, 0600/complete file protection/backup-excluded; no App Group/SwiftData. Verify≤50 MiB/length/hash from reauthorized ready status before share. Remove partial on error/cancel; successful file on share completion/dismiss/background/realm loss; launch removes leftovers only in that directory.

- [ ] **Step 1 (3 min): Capture prior ref; snapshot/authorization RED test.** `newExportFixture(t)` embeds WorkerHarness plus Exports/Worker; helpers Create(t,p,id), PauseCapture(t)(entered,release chan struct{}), MutateCanonical(t), BeginOwnershipTransfer(t) returning pending command with Wait(t), AssertCapturedArchive(t,id), CountArchiveRows(t,id), HasWrappedKey(t,id). Pause uses actual snapshot transaction/barriers; archive assertion uses test owner before transfer, not unauthorized download.

```go
func TestExportHasOneCanonicalAndSecuritySnapshot(t *testing.T) {
 h:=newExportFixture(t); job:=h.Create(t,h.Owner,testkit.CommandA)
 entered,release:=h.PauseCapture(t); done:=make(chan error,1)
 go func(){done<-h.Worker.RunOnce(t.Context(),1)}(); <-entered
 h.MutateCanonical(t); transfer:=h.BeginOwnershipTransfer(t)
 close(release); if err:= <-done; err!=nil { t.Fatal(err) }
 h.AssertCapturedArchive(t,job.ExportID); transfer.Wait(t)
 if _,err:=h.Exports.Status(t.Context(),h.Owner,job.ExportID); security.Code(err)!="owner_required" { t.Fatal("former owner retained export") }
}
```

The fixture pauses the pending transfer **before acquiring its exclusive fence** until the test has inspected ready bytes, then Wait releases that barrier and awaits the real transfer. This permits the worker's final owner check without deadlock; the separate stream-order test barriers an actual contested fence. Add TestExportDownloadOwnerChangeOrdersWithStream (blocking writer vs P1 transfer) and TestExportAuthorizationEveryBoundary (requester/removal/delete/epoch).
- [ ] **Step 2 (2 min): RED.** `rtk proxy env BT_TEST_RUN='TestExport|TestEnvelope' make server-test`; expect missing job/envelope/guard behavior.
- [ ] **Step 3 (5 min): Implement schema/job/lease/rate.** `export_jobs` fields Job plus instance_id,recovery_epoch,attempt,lease_id,lease_until,next_attempt_at,last_code,header,plaintext_bytes,plaintext_sha256; CHECK expires_at=created_at+24 h. Add export_chunks/rate_reservations above, forced RLS/composite FK, expiry index(expires_at,id), claim index(next_attempt_at,lease_until,id) queued/running. Typed definer claim/complete/retry/fail functions receive all lease guards, worker-only EXECUTE; runtime only guarded create/status/ready-read. Existing limiter and capability/DTO registry own HTTP admission.
- [ ] **Step 4 (4 min): Envelope/quota/expiry tests RED.** `envelopeFixture(t,size)` provides independent expected plaintext/frame bytes, deterministic test RNG/key, Corrupt(string); assertions call actual decoder. Exact golden preimages are assembled independently from documented bytes.

```go
func TestEnvelopeAuthenticatesChunksAndTermination(t *testing.T) {
 for _,fault:=range []string{"header","wrap_nonce","wrapped_key","tag","swap_chunks","duplicate_index","missing_middle","missing_final","early_final","trailing_bytes","changed_length"} {
  t.Run(fault,func(t *testing.T){ f:=envelopeFixture(t,131072); f.Corrupt(fault); assertEnvelopeRejected(t,f) })
 }
 for _,n:=range []int{0,1,65535,65536,65537,52428800} { t.Run(strconv.Itoa(n),func(t *testing.T){assertEnvelopeRoundTrip(t,envelopeFixture(t,n))}) }
}
func TestExportQuotaAndLifetimeStartAtCreation(t *testing.T) {
 h:=newExportFixture(t); job:=h.Create(t,h.Owner,testkit.CommandA); h.FailJob(t,job.ExportID)
 if _,_,err:=h.Exports.Create(t.Context(),h.Owner,h.Request(testkit.CommandB)); security.Code(err)!="export_rate_limited" { t.Fatal("failed job bypassed quota") }
 h.Clock.Advance(24*time.Hour); h.RunExportExpiry(t)
 if h.CountArchiveRows(t,job.ExportID)!=0 || h.HasWrappedKey(t,job.ExportID) { t.Fatal("expired data/key retained") }
}
```

Define FailJob/Request/RunExportExpiry helpers in export_test.go using service/clock/registered cleanup. Run Step 2 RED before envelope implementation.
- [ ] **Step 5 (5 min): Implement snapshot encoder/envelope/download.** Stream fixed DTO queries through bounded byte count/SHA into 64 KiB chunks, finalize/wrap header, conditionally Complete in one transaction. Failed/stale attempts publish nothing. Reclaim erases prior chunks/key refs before fresh nonce/key. Snapshot guard sets isolation before reads and both tenant settings. Two-pass verified download under 30 s fence emits no unverified byte. Repeat Step 2 GREEN.
- [ ] **Step 6 (4 min): Register export cleanup only now.** Migration 000016 adds a fixed purge-worker definer and schema-16 `CleanupCapability`; `ExportCleanup` invokes that typed repository method, never arbitrary transaction SQL. Exact-family cleanup deletes chunks/header/wrapped key/jobs/family quota in the same worker transaction while retaining unrelated account quota. Expiry removes operational live rows at `created_at+24 h`, retains a content-free request receipt for the stated idempotency horizon, and marks backup recoverability honestly until encrypted backups expire. The deadline worker schedules nearest expiry, scans on start/wake, marks `export_cleanup_overdue` during outage, and never claims cryptographic erasure from a live-row delete. Tests cover crash retry, schema-hook overlap, active/failed/expired/replayed request IDs, and backup recoverability.
- [ ] **Step 7 (4 min): Worker/security GREEN matrix.** Test two workers, death before/after chunks/before ready, stale completion, request replay/concurrent IDs, independent family/account quotas, stalled expiry, 50 MiB/50 MiB+1, repeat download, malformed ciphertext before headers, lost owner, former-name removal. Assert one active job/no nonce reuse/no plaintext SQL/logs/no unauthorized bytes and exact deterministic JSON. Run `rtk proxy env BT_TEST_RUN='TestExport|TestEnvelope|TestFullFamilyPurge|TestNoExportTable' make server-test` PASS.
- [ ] **Step 8 (4 min): Swift client/UI/temp-file RED→GREEN.** ExportFixture wraps P3 transport, golden status/archive and isolated directory. Define ExportTemporaryFiles.removeAll()/remove(URL)/contents(), FamilyExportViewModel.request()/downloadAndShare() async and shareDidFinish(); states idle/preparing/ready/sharing/expired/failed.

```swift
@Test func exportDownloadDeletesPartialAndSharedFiles() async throws {
 let f = try ExportFixture.make(); f.transport.failAfterBytes = 65_536
 await #expect(throws: ExportClientError.integrity) { try await f.client.download(id: f.id) }
 #expect(try f.files.contents().isEmpty)
 f.transport.failAfterBytes = nil
 _ = try await f.client.download(id: f.id)
 try f.files.removeAll(); #expect(try f.files.contents().isEmpty)
}
```

Run `rtk swift test --package-path Packages/BabyTracker --filter 'ExportClientTests|FamilyExportViewModelTests'` RED, implement DTO/client/Settings/temp cleanup, repeat GREEN. Parameterize requester/transfer/rate Retry-After/expiry/hash/length/cancel/share dismissal/background/launch cleanup; use P3 refresh serialization and realm identity.
- [ ] **Step 9 (4 min): Finish manifest `[16,16]` and counted regressions.** Add `TestMigration000016CompatibilityOverlap` RED against the predecessor manifest. Generate the feature manifest and run source/client gates now; actual prepared-mode 15→16 overlap runs immediately after the Step 10 feature commit:

```bash
rtk proxy sh -c 'cd Server && go run ./cmd/baby-tracker-manifest --min 16 --max 16 --output internal/migrate/compatibility.json'
rtk make server-test
rtk swift test --package-path Packages/BabyTracker --filter 'ExportClientTests|FamilyExportViewModelTests'
rtk make test
rtk make build
rtk git diff --check
```

Prior APNs/sync/cleanup works at 16; new worker exports/purges all resources. Update literal EXPECTED_TESTS using actual executed total.
- [ ] **Step 10 (2 min): Exact commit, including 000016/count.**

```bash
rtk git add Server/internal/export/{model,archive,envelope,repository,service,worker,cleanup}.go Server/internal/export/{export,envelope,worker}_test.go Server/internal/protocol/{export.go,decode.go} Server/internal/httpapi/{export.go,export_test.go,router.go} Server/internal/config/{export.go,export_test.go,config.go} Server/internal/security/{transaction.go,transaction_test.go} Server/internal/retention/{cleanup.go,cleanup_test.go,purge_test.go} Server/internal/testkit/workers.go Server/internal/limits/policy.go Server/migrations/000016_exports.sql Server/internal/migrate/{compatibility.json,overlap_test.go} Server/scripts/{check-principals.sh,check-migration-overlap.sh} Server/cmd/baby-tracker-worker/{main.go,main_test.go} Server/api/openapi.yaml Server/api/fixtures/{export-request,export-status,export-archive-v1}.json Server/compose.yaml Packages/BabyTracker/Sources/BabyProtocol/ExportDTOs.swift Packages/BabyTracker/Sources/BabySync/{ExportClient,ExportTemporaryFiles}.swift Packages/BabyTracker/Sources/SettingsFeature/{FamilyExportView,FamilyExportViewModel,SettingsView}.swift Packages/BabyTracker/Tests/BabySyncTests/ExportClientTests.swift Packages/BabyTracker/Tests/SettingsFeatureTests/FamilyExportViewModelTests.swift Apps/BabyTrackerApp/AppEnvironment.swift Makefile
rtk git diff --cached --check
rtk git commit -m "feat(sync): add bounded encrypted owner exports"
BT_MIGRATION_FEATURE_REF=$(rtk git rev-parse HEAD)
rtk proxy bash Server/scripts/check-migration-overlap.sh --base-ref "$BT_MIGRATION_BASE_REF" --prior-ref "$BT_MIGRATION_PRIOR_REF" --feature-ref "$BT_MIGRATION_FEATURE_REF" --next-version 16 --prior-min 15 --prior-max 16 --next-min 16 --next-max 16
```

---

### Task 4: Privacy-safe operations, license/SBOM and actual dual-architecture CI (P4.4)

**Files:**

- Create: `Server/internal/observability/{metrics.go,metrics_test.go}`, `Server/internal/httpapi/metrics.go`, `Server/internal/operator/{inspect.go,inspect_test.go}`, `Server/scripts/{ci.sh,sbom.sh,dual-arch-smoke.sh,release_scripts_test.go}`, `.forgejo/workflows/family-sync.yml`, `Server/LICENSE`, `Server/NOTICE.md`, `Server/docs/operations/ci.md`.
- Modify: `Server/cmd/baby-tracker-admin/{main.go,main_test.go}`, `Server/internal/httpapi/router.go`, `Server/scripts/{smoke.sh,smoke_test.go,build.sh}`, `Server/README.md`, `Makefile`.

**Interfaces:** `Metrics.Observe(route Route,method Method,statusClass StatusClass,duration time.Duration)`, `Increment(operation Operation,code SafeCode)`, `WritePrometheus(io.Writer) error`. Enums contain only fixed registered values; unknown path=`unmatched`. Allowed labels route/method/status_class/error_code/operation/mode; buckets5, 10, 25, 50, 100, 250, 500, 1000, 2000, 5000 ms. Prometheus 0.0.4 `/metrics` is mounted only on the separately configured internal listener, loopback by default, and is absent from the public router. CLI `family-inspect --family-id UUID`, `account-inspect --account-id UUID`, `purge-status --family-id UUID` returns `Inspection{ID,Kind,State string; Revision uint64; Counts map[string]uint64; Times map[string]string}` with fixed safe count/time keys. No names/records/raw audit; P1 operator audit records each access. `version --json` returns `{source_sha,goos,goarch}` for exact runtime proof.

- [ ] **Step 1 (3 min): Metrics/CLI/scripts RED.** `newInspectFixture(t)` seeds real operator service with unique forbidden strings; Run(args) invokes admin parser; AssertNoPrivateValues/AssertOneAudit check captured bytes/SQL. `newReleaseScriptFixture(t)` uses executable fake commands in explicit temp PATH, with RuntimeArchitectures[] and Run(script) error, not source-only grep.

```go
func TestInspectionContainsOnlyApprovedFields(t *testing.T) {
 h:=newInspectFixture(t); raw,err:=h.Run([]string{"family-inspect","--family-id",h.FamilyID}); if err!=nil { t.Fatal(err) }
 var fields map[string]json.RawMessage; if err:=json.Unmarshal(raw,&fields); err!=nil { t.Fatal(err) }
 for k:=range fields { if !slices.Contains([]string{"id","kind","state","revision","counts","times"},k) { t.Fatal(k) } }
 h.AssertNoPrivateValues(t,raw); h.AssertOneAudit(t)
}
func TestDualArchSmokeRejectsBuildWithoutExecution(t *testing.T) {
 h:=newReleaseScriptFixture(t); h.RuntimeArchitectures=[]string{"arm64"}
 if h.Run("dual-arch-smoke.sh")==nil { t.Fatal("amd64 build alone passed") }
}
```

Add TestMetricLabelsAndCardinality(1000 random paths→one unmatched series), TestWorkflowActionPins(all third-party uses values require 40 hex SHA), TestSBOMArtifactSchema(actual script on fixture module→valid CycloneDX). Run `rtk proxy sh -c 'cd Server && go test ./internal/observability ./internal/operator ./scripts -run "TestInspection|TestDualArch|TestMetric|TestWorkflow|TestSBOM" -count=1'`; expected missing implementations.
- [ ] **Step 2 (4 min): Implement bounded metrics/inspection/distribution.** Atomic counters/fixed buckets/sorted encoder, route metadata before observation; strict UUID/operator checks, safe output and audit. Complete AGPLv3 license text/NOTICE respecting spec licensing. sbom.sh uses exact `github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@v1.10.0`, output Server/artifacts/sbom.cdx.json, Go module sum verification. ci.sh uses exact `golang.org/x/vuln/cmd/govulncheck@v1.8.0`; record tool versions, no floating installs. Tests independently parse JSON/artifacts and inspect argv.
- [ ] **Step 3 (5 min): Implement actual architecture execution.** Extend existing build.sh `--platform --load --tag`; smoke.sh `--platform --image --expect-arch --evidence`. For each platform start isolated project, explicit migrate/init, execute image admin version, start API, assert reported GOARCH+container platform, health/readiness, worker/runtime grants and cleanup. Untagged production image executes these checks; separately tagged image reuses P1 issuer for authenticated flows. Native or emulated runtime is acceptable only if foreign binary actually runs; record host arch/mode. No emulation support means blocked runtime gate, never a successful build substituted for execution.

```bash
rtk proxy bash Server/scripts/build.sh --platform linux/amd64 --load --tag baby-tracker-server:smoke-amd64
rtk proxy bash Server/scripts/smoke.sh --platform linux/amd64 --image baby-tracker-server:smoke-amd64 --expect-arch amd64 --evidence Server/artifacts/smoke-amd64.json
rtk proxy bash Server/scripts/build.sh --platform linux/arm64 --load --tag baby-tracker-server:smoke-arm64
rtk proxy bash Server/scripts/smoke.sh --platform linux/arm64 --image baby-tracker-server:smoke-arm64 --expect-arch arm64 --evidence Server/artifacts/smoke-arm64.json
```

dual-arch-smoke.sh executes the above operations; validates both reports executed:true/ready:true, correct runtime_goarch, same source SHA and architecture-specific digests. Each project preserves one-host fence constraints; cleanup exact named resources only.
- [ ] **Step 4 (4 min): Add current-task CI without future dependencies.** `make beta-ci` initially runs required PostgreSQL race suite, vet/vulnerability scan, OpenAPI fixtures, SBOM, actual dual runtime smoke, counted Swift suite and **existing P3** `make test-family-sync-e2e`. P4.6/P4.7 extend only after their deliverables exist. Reject required suite skips/missing artifacts; ordinary `go list -deps ./cmd/baby-tracker-server` excludes internal/testidentity and production rejects runtime issuer settings. Forgejo wrapper calls repository scripts and pins every third-party action to a verified immutable 40 hex SHA; no invented SHA. Running a script must not expose credentials in logs/artifacts.
- [ ] **Step 5 (3 min): GREEN and commit.** Run `rtk proxy sh -c 'cd Server && go test -race ./internal/observability ./internal/operator ./scripts ./cmd/baby-tracker-admin -count=1'`, `rtk make server-sbom`, `rtk make beta-ci`, `rtk git diff --check`; require both actual architectures. Record unavailable runtime/simulator precisely.

```bash
rtk git add Server/internal/observability/{metrics.go,metrics_test.go} Server/internal/httpapi/{metrics.go,router.go} Server/internal/operator/{inspect.go,inspect_test.go} Server/cmd/baby-tracker-admin/{main.go,main_test.go} Server/scripts/{ci.sh,sbom.sh,dual-arch-smoke.sh,release_scripts_test.go,smoke.sh,smoke_test.go,build.sh} .forgejo/workflows/family-sync.yml Server/LICENSE Server/NOTICE.md Server/docs/operations/ci.md Server/README.md Makefile
rtk git diff --cached --check
rtk git commit -m "build(server): verify operations and both container architectures"
```

---

### Task 5: Implemented backup/restore CLI and provider-neutral deployment (P4.5)

**Files:**

- Create: `Server/internal/operator/{backup,quarantine}.go`, `{backup,quarantine}_test.go`; `Server/internal/retention/{inventory.go,inventory_test.go}`; `Server/deploy/hosted/{compose.yaml,env.example,reverse-proxy.example.yaml}`; `Server/scripts/{verify-deployment.sh,verify-restore.sh,operations_scripts_test.go}`; `Server/docs/operations/{deploy,upgrade,backup-restore,recovery-epoch,privacy-requests}.md`.
- Modify: `Server/cmd/baby-tracker-admin/{main.go,main_test.go}`, `Server/internal/recovery/{state,gate,reconcile}.go`, `Server/internal/config/retention.go`, `Server/compose.yaml`, `Server/README.md`.

**Interfaces:** `BackupManifestBody{Version,BackupID,DatabaseCiphertextSHA256,InstanceID string; RecoveryEpoch uint64; Head security.Head; WatermarkSequence uint64; WatermarkSHA256,InventorySHA256,EncryptionKeyID,SigningKeyID,CreatedAt,ExpiresAt string}` with fixed-order explicit snake_case tags; `SignedBackupManifest{Body BackupManifestBody; Signature string}`. Sign Ed25519 preimage `"baby-tracker-backup-manifest-v1\x00" || canonical(body)`. `BackupService.CreateEncrypted(...)`, `VerifyCreation(...)`, and `VerifyRestore(...)` are distinct operations. Creation verifies the actual authenticated ciphertext and exact coordinated current head/epoch/witness/inventory. Restore verifies that the signed manifest head is a proven ancestor/checkpoint-compatible state above the accepted watermark cutoff while the latest independent authority remains current. Freshness ≤24 h is a hosted readiness gate; unknown/unlisted/expired/deleted/below-cutoff/wrong-ancestor material rejects. Streaming encryption/decryption uses a purpose-specific key ID and never leaves a retained plaintext dump.

`QuarantineService.Enter/Verify/Leave(ctx,QuarantineRequest)(QuarantineReceipt,error)`, request `{Manifest SignedBackupManifest; ExpectedEpoch uint64; ExpectedHead security.Head; ConfirmInstance string}`, receipt `{InstanceID string; Epoch uint64; Head security.Head; State string}`. Enter persists quarantine outside SQL before restore. Verify requires trusted-head ancestry/explicit monotonic epoch rotation/checksum schema/checkpoint+events/reconciled projection and real auth/client probes. Leave CAS-clears only exact verified epoch/head/digest; changed head requires reverify. All operator audited, no HTTP privilege. Use existing testable admin parser.

Exact CLI grammar, using **test fixture** IDs/hash/time below (real script substitutes verified safe JSON fields):

```bash
rtk proxy sh -c 'cd Server && go run ./cmd/baby-tracker-admin backup-manifest create --backup-id local-20260920T120000Z --database-path artifacts/restore-test/database.dump --inventory-file artifacts/restore-test/inventory.json --expires-at 2026-10-25T12:00:00.000000Z --output artifacts/restore-test/backup-manifest.json'
rtk proxy sh -c 'cd Server && go run ./cmd/baby-tracker-admin backup-manifest verify --manifest artifacts/restore-test/backup-manifest.json --inventory-file artifacts/restore-test/inventory.json --database-path artifacts/restore-test/database.dump'
rtk proxy sh -c 'cd Server && go run ./cmd/baby-tracker-admin restore-quarantine enter --manifest artifacts/restore-test/backup-manifest.json --expected-epoch 1 --expected-generation 2 --expected-revision 42 --expected-head-hash 4444444444444444444444444444444444444444444444444444444444444444 --confirm-instance 10000000-0000-4000-8000-000000000001'
rtk proxy sh -c 'cd Server && go run ./cmd/baby-tracker-admin restore-quarantine verify --manifest artifacts/restore-test/backup-manifest.json --expected-epoch 2 --expected-generation 2 --expected-revision 42 --expected-head-hash 4444444444444444444444444444444444444444444444444444444444444444 --confirm-instance 10000000-0000-4000-8000-000000000001'
rtk proxy sh -c 'cd Server && go run ./cmd/baby-tracker-admin restore-quarantine leave --manifest artifacts/restore-test/backup-manifest.json --expected-epoch 2 --expected-generation 2 --expected-revision 42 --expected-head-hash 4444444444444444444444444444444444444444444444444444444444444444 --confirm-instance 10000000-0000-4000-8000-000000000001'
```

Between enter/verify, restore only fixture DB then invoke P1 recovery rotate-epoch with actual expected epoch/revision/reason/confirmation. Extend that existing command's expected-head binding with generation/hash, preserving all old flags; test wrong generation/hash denial. Preserve latest ledger/head/watermark volumes, never DB-era copies. Reconcile, run auth/client probes, verify, leave. Total loss of latest independent witness remains quarantined operator disaster, not inferred freshness from old matching stores.

- [ ] **Step 1 (3 min): CLI/restore RED.** `newRestoreCLIFixture(t)` creates isolated PostgreSQL/stores, actual signed dump/inventory; helpers Backup,RemoveCaregiverAndCheckpoint,Enter,RestoreDatabaseOnly,RotateEpoch,Reconcile,Verify,Leave,AssertRevokedIdentityDeniedAndClientBaseReplaced invoke documented CLI/real P2 clients. Parser fixture runs literal argv grammar above with expected test state; positive operations use actual computed hash rather than assume arbitrary hash succeeds.

```go
func TestRestoreCannotLeaveBeforeLatestAuthorityReplay(t *testing.T) {
 h:=newRestoreCLIFixture(t); h.Backup(t); h.RemoveCaregiverAndCheckpoint(t)
 h.Enter(t); h.RestoreDatabaseOnly(t); h.RotateEpoch(t)
 if h.Leave(t)==nil { t.Fatal("quarantine cleared before replay/probes") }
 h.Reconcile(t); h.Verify(t)
 if err:=h.Leave(t); err!=nil { t.Fatal(err) }
 h.AssertRevokedIdentityDeniedAndClientBaseReplaced(t)
}
```

Add TestBackupManifestBindsEveryInput, TestInventoryMissingPageBlocksWatermark, TestOldCoordinatedPairNeedsIndependentWitness, TestRestoreRejectsBackupOlderThanWatermark, TestQuarantineHeadChangesAfterVerify, TestDocumentedRestoreCLIRejectsMissingFlags. Run `rtk proxy env BT_TEST_RUN='TestBackup|TestInventory|TestRestore|TestQuarantine|TestOldCoordinated|TestDocumentedRestore' make server-test` RED.
- [ ] **Step 2 (5 min): Implement inventory/signature/CLI/quarantine.** Local inventory reads explicit manifest roots/hashes dumps, rejects unexpected unmanaged backup files and incomplete version/deletion receipts. Hosted provider pages/retention/region/encryption stay under P1 provider gate. Manifest writes atomic 0600/fsync after bounded stream hash; reject symlinks/nonregular paths. Quarantine transitions use exclusive fence/exact CAS, persist before SQL restore and remain after any error. Print only safe IDs/digests/counts.
- [ ] **Step 3 (4 min): Scripts RED→GREEN.** operations_scripts_test.go uses fake executable fixtures to fail each restore stage; assert no later readiness/unquarantine on failure. `verify-restore.sh --self-test` executes these tests. `--compose Server/compose.yaml --local-disposable` only operates unique harness-created project/DSNs and rejects arbitrary operator DB/public target. verify-deployment checks fixture HTTPS proxy, instance/digest/schema/roles/backup age/witness/redacted logs. Scripts require explicit secret-file paths, no tracing, exact-resource traps. First run `rtk proxy bash Server/scripts/verify-restore.sh --self-test` RED, implement and repeat GREEN.
- [ ] **Step 4 (4 min): Write deploy/upgrade/recovery runbooks.** Hosted services use separate API/projector, purge, export, delivery secrets on one authority host, external managed DB/TLS, real EU ledger/head provider ports and finite backup inventory/signer; no filesystem fallback. Self-host uses separate fsynced volumes and no Apple/APNs developer keys/relay. Upgrade explicitly deploys prior manifest-overlap build→migrate→all new readers→verify→checkpoint format enable. Pre-checkpoint reader cannot roll back after new generation publication. Include 24 h RPO, 35 day backup default,min 30, privacy-request/purge proof, rotation/key retention and explicit retention incidents.
- [ ] **Step 5 (4 min): Local GREEN/restore and commit.**

```bash
rtk proxy env BT_TEST_RUN='TestBackup|TestInventory|TestRestore|TestQuarantine|TestOldCoordinated|TestDocumentedRestore' make server-test
rtk proxy bash Server/scripts/verify-restore.sh --self-test
rtk proxy bash Server/scripts/verify-deployment.sh --compose Server/compose.yaml --local-disposable
rtk proxy bash Server/scripts/verify-restore.sh --compose Server/compose.yaml --local-disposable
rtk git diff --check
rtk git add Server/internal/operator/{backup.go,quarantine.go,backup_test.go,quarantine_test.go} Server/internal/retention/{inventory.go,inventory_test.go} Server/deploy/hosted/{compose.yaml,env.example,reverse-proxy.example.yaml} Server/scripts/{verify-deployment.sh,verify-restore.sh,operations_scripts_test.go} Server/docs/operations/{deploy.md,upgrade.md,backup-restore.md,recovery-epoch.md,privacy-requests.md} Server/cmd/baby-tracker-admin/{main.go,main_test.go} Server/internal/recovery/{state.go,gate.go,reconcile.go} Server/internal/config/retention.go Server/compose.yaml Server/README.md
rtk git diff --cached --check
rtk git commit -m "feat(server): implement witnessed backup and quarantined restore tooling"
```

Require actual dump/restore after checkpoint, latest authority, correct client epoch/base and revoked denial, untouched current witness volumes. Hosted live checks wait for concrete provider/account/EU region/domain/keys/signing/policy/controller inputs and deployment authorization; mark pending_external and continue local tasks.

---

### Task 6: Extend existing protocol/app acceptance with faults and measured load (P4.6)

**Files:**

- Create: `Server/tests/protocol/{beta_lifecycle,beta_faults,beta_restore,beta_load}_test.go`, `Server/internal/testkit/{fault_proxy.go,fault_proxy_test.go}`, `Packages/BabyTracker/Tests/BabySyncTests/TwoClientContractTests.swift`, `E2ETests/BabyTrackerFamilySyncE2ETests/FamilySyncBetaAcceptanceTests.swift`, `docs/verification/family-sync-beta.md`.
- Modify: `Server/tests/protocol/harness_test.go`, `Server/internal/testkit/{protocol_client,fixtures}.go`, `Server/scripts/{protocol-e2e.sh,ci.sh}`, P3-owned `TestSupport/FamilySync/FamilySyncTestServer.swift`, `BabyTracker.xcodeproj/project.pbxproj`, `BabyTracker.xcodeproj/xcshareddata/xcschemes/BabyTrackerFamilySyncE2ETests.xcscheme`, `Makefile`.

**Interfaces:** Extend existing protocol-e2e.sh flags `--suite protocol|beta|faults|load --report FILE --keep-stack --stack-file FILE`; no second launcher/issuer. Reuse newProtocolFixture/ProtocolClient/internal/testidentity under identitytest. `FaultProxy.Arm(FaultSpec) error`, `Clear()`, `FaultSpec{Kind,OperationID string; Count int}` supports drop_response_after_upstream_complete/pause_hint/reorder_hints/disconnect_database; no payload rewriting, DB fault only isolated proxy. P3 owns target/scheme; add new tests to it. `make family-sync-e2e` invokes existing script beta then `make test-family-sync-e2e` with its stack metadata; faults/load use respective suite selectors.

`LoadReport{SourceSHA,ImageDigest,HostOS,HostArch,ExecutionMode string; Families,Clients int; DurationSeconds,SuccessfulCycles,FailedCycles,SampleCount uint64; FailureLatencyMS,P50MS,P95MS,P99MS,RoundTripMS float64; Histogram []Bucket}` additionally records CPU/memory/container limits/PostgreSQL tier/reconnect distribution. `FailureLatencyMS` is exactly 30000 for this beta profile. Persist aggregate numbers/opaque scenario IDs only. Commit timing comes from an in-process/test-build observer, not a new production endpoint. Client measurement ends after durable materialized base/cursor write.

- [ ] **Step 1 (3 min): Integrated lifecycle RED.** Extend ProtocolClient RequestExport/DownloadExport/ScheduleDeletion/Recover/TransferTo/DeleteAccount/SyncAndMaterialize/Digest and fixture JoinedCaregiver/ImportOverlappingState/WriteEveryRecordVariantAndTimer/RunExportWorker/AssertArchiveMatchesFamily/AssertMemberDenied/AdvanceRetentionAndRunWorkers/AssertPhysicalPurgeAndNoRecovery. These use real HTTP/worker/SQL assertions; no fake successful state transitions.

```go
//go:build identitytest
func TestBetaTwoClientsExportRecoveryAndPurge(t *testing.T) {
 h:=newProtocolFixture(t); a,b:=h.Owner(),h.JoinedCaregiver()
 h.ImportOverlappingState(t,a,b); h.WriteEveryRecordVariantAndTimer(t,a,b)
 a.SyncAndMaterialize(); b.SyncAndMaterialize()
 if a.Digest()!=b.Digest() { t.Fatal("materialized state diverged") }
 job:=a.RequestExport(); h.RunExportWorker(t); h.AssertArchiveMatchesFamily(t,a.DownloadExport(job.ExportID))
 a.ScheduleDeletion(); h.AssertMemberDenied(t,b); a.ReauthenticateWithContinuity(); a.Recover()
 a.ScheduleDeletion(); h.AdvanceRetentionAndRunWorkers(t); h.AssertPhysicalPurgeAndNoRecovery(t)
}
```

Run `rtk proxy bash Server/scripts/protocol-e2e.sh --suite beta --report Server/artifacts/beta-e2e.json` RED; expect missing integrated scenarios. This schema 16 test must not be consumed by Task 1.
- [ ] **Step 2 (4 min): Fault/restore tests and one harness extension.** TestBetaFaultMatrix covers duplicate/drop-after-commit/reordered+dropped socket/APNs/token expiry+refresh reuse+same-device reauth/interrupted frozen chunk/server+worker restart/DB outage/export concurrent edit+owner change. Assert receipts/base/overlay/outbox/rows, not HTTP alone. TestBetaOlderDatabaseRestoreAfterCheckpoint preserves latest generation/witness, restores allowed older DB, rotates/quarantines/reconciles; acknowledged-missing remains absent, unacknowledged intents replay in order, no revoked member/session/invite/owner revives. Reuse P2 lost-start→reauth→original applied receipt→one stop scenario unchanged. Extend existing suite selector/tagged build/report/cleanup.
- [ ] **Step 3 (4 min): Rendered app RED→GREEN in existing target.** Use two isolated App Group/Keychain namespaces, real stack URLs from 0600 fixture metadata and only fixture IDs in launch args. Add connectedToRunningStack/logBottle/assertWidgetSharedCacheEmpty helpers to existing E2E fixture/test server.

```swift
func test_secondInstallationRendersRemoteFeedAndRevocationPurges() throws {
 let pair = try FamilySyncTwoClientProcessFixture.connectedToRunningStack()
 pair.firstClient.logBottle(volumeML: 60)
 XCTAssertTrue(pair.secondClient.app.staticTexts["60 ml"].waitForExistence(timeout: 15))
 try pair.server.removeCaregiver(pair.secondClient.accountID)
 XCTAssertTrue(pair.secondClient.app.staticTexts["Family access removed"].waitForExistence(timeout: 15))
 XCTAssertFalse(pair.secondClient.app.staticTexts["60 ml"].exists)
 try pair.secondClient.assertWidgetSharedCacheEmpty()
}
```

Run `rtk make test-family-sync-e2e` RED, implement test target membership/helpers, repeat GREEN. Include rendered export/error, timer conflict, caregiver account deletion/former attribution, normal transfer/owner identity loss/manual test-policy recovery. Stage project and shared scheme; never invent a FamilySyncFlowTests scheme.
- [ ] **Step 4 (4 min): Implement fixed load and executable threshold.** 200 clients maintain durable maps/cursors through actual HTTP pages. Schedule 400 mutations/min across 100 families for 15 min, then 200-client reconnect; final drain 30 s. Success means all committed pages through fixed high-water durably materialize at peer. Produce exactly one latency sample for every attempted peer-materialization cycle: use observed commit-to-durable latency for success and the fixed 30000 ms drain deadline for timeout, missing, or failed cycles. Compute sorted nearest-rank p50/p95/p99 over that complete sample vector; do not calculate a success-only percentile. Separately require successful/attempted >=0.999. Synthetic unit tests pin boundary ranks, a single failed sentinel, missing-sample rejection and denominator math; full profile remains 900 s.

```go
func assertBetaLoad(t *testing.T,r LoadReport) {
 t.Helper()
 if r.Families!=100 || r.Clients!=200 || r.DurationSeconds<900 { t.Fatal("wrong workload") }
 total:=r.SuccessfulCycles+r.FailedCycles
 if total==0 || r.FailureLatencyMS!=30000 || r.SampleCount!=total ||
    float64(r.SuccessfulCycles)/float64(total)<0.999 || r.P95MS>2000 { t.Fatal("beta freshness failed") }
}
```

Record hardware/RTT/config/histogram/reconnect failures. Separate rendered app gate proves durable apply reaches UI; HTTP200 timing is insufficient. Never silently relax target or substitute shorter run.
- [ ] **Step 5 (4 min): GREEN and honest local report.** Update literal package test count for TwoClientContractTests. ci.sh gains beta/fault suites now; 15 min load stays explicit required release target.

```bash
rtk make family-sync-e2e
rtk make family-sync-faults
rtk make family-sync-load
rtk make test-family-sync-e2e
rtk make test
rtk make build
rtk make beta-ci
rtk git diff --check
```

Report exact source/image, commands/artifacts/observed pass or failure in docs/verification/family-sync-beta.md. External provider/device/publication rows remain pending_external.
- [ ] **Step 6 (2 min): Scoped harness/count/project/scheme commit.**

```bash
rtk git add Server/tests/protocol/{beta_lifecycle,beta_faults,beta_restore,beta_load}_test.go Server/tests/protocol/harness_test.go Server/internal/testkit/{fault_proxy.go,fault_proxy_test.go,protocol_client.go,fixtures.go} Server/scripts/{protocol-e2e.sh,ci.sh} Packages/BabyTracker/Tests/BabySyncTests/TwoClientContractTests.swift E2ETests/BabyTrackerFamilySyncE2ETests/FamilySyncBetaAcceptanceTests.swift TestSupport/FamilySync/FamilySyncTestServer.swift BabyTracker.xcodeproj/project.pbxproj BabyTracker.xcodeproj/xcshareddata/xcschemes/BabyTrackerFamilySyncE2ETests.xcscheme Makefile docs/verification/family-sync-beta.md
rtk git diff --cached --check
rtk git commit -m "test(sync): extend beta lifecycle fault and freshness acceptance"
```

---

### Task 7: Truthful privacy artifacts and implemented release validator (P4.7)

**Files:**

- Create: `docs/privacy/{policy.md,app-store-disclosures.md,data-rights.md,operator-inputs.schema.json,policy-site/index.html}`, `docs/verification/{family-sync-release-checklist.json,family-sync-release-checklist.md}`, `docs/verification/family-sync-evidence/` (generated nonsecret reports and SHA-256 manifest), `Server/scripts/{check-privacy-docs.sh,check-release-checklist.sh,release_checklist_test.go}`, `Server/internal/release/{checklist,artifacts}.go`, `Server/internal/release/{checklist,artifacts}_test.go`, `Server/cmd/baby-tracker-release-check/main.go`.
- Create: `Packages/BabyTracker/Sources/SettingsFeature/{PrivacySettingsView,PrivacySettingsViewModel}.swift`, `Packages/BabyTracker/Tests/SettingsFeatureTests/PrivacySettingsViewModelTests.swift`.
- Modify: `Packages/BabyTracker/Sources/SettingsFeature/SettingsView.swift`, `Apps/BabyTrackerApp/AppEnvironment.swift`, `Server/README.md`, `Server/scripts/ci.sh`, `Makefile`, `HANDOFF.md`.

**Interfaces:** use closed string enums, not caller-defined strings:

```go
type GateID string
type GateKind string
const ( GateLocalAutomated GateKind = "local_automated"; GateAuthorizedExternal GateKind = "authorized_external" )
type GateStatus string
const ( StatusPass GateStatus = "pass"; StatusFail GateStatus = "fail"; StatusPendingExternal GateStatus = "pending_external"; StatusBlockedEnvironment GateStatus = "blocked_environment" )
type Mode string
const ( ModeLocal Mode = "local"; ModeRelease Mode = "release" )
type Gate struct { ID GateID; Kind GateKind; Status GateStatus; SourceSHA,BuildVariant,GOOS,GOARCH,ArtifactDigest,ObservedAt,EvidencePath string }
type Checklist struct { Version string; Gates []Gate }
func Validate(Checklist,Mode,time.Time,VerifiedArtifactManifest) error
```

The exhaustive local IDs are `go-race-db`, `swift-counted`, `ios-ui`,
`protocol-beta`, `protocol-faults`, `load-100-family`, `runtime-amd64`,
`runtime-arm64`, `migration-overlap`, `checkpoint-roundtrip`,
`generation-retirement`, `encrypted-backup-restore`, `restore-checkpoint`,
`retention-health`, `backup-freshness`, `sbom-vulnerability`, and
`privacy-consistency`. The exhaustive authorized-external IDs are `real-apple`,
`real-apns`, `two-device`, `homelab-https`, `hosted-eu-provider`,
`hosted-deployment`, `hosted-retention-health`, `hosted-backup-freshness`,
`hosted-encrypted-backup-restore`, `public-policy`, `public-source-offer`,
`app-store-disclosures`, `owner-recovery-policy`, and
`bounded-production-logs`. Unknown, missing, duplicate, wrong-kind or
wrong-status IDs fail. `pending_external` is valid only for external IDs;
`blocked_environment` never satisfies either mode. Local mode requires every
local ID pass and reports external status; release mode requires every ID pass.

A pass needs a UTC timestamp <=7 days old, the exact tested source SHA and
artifact digest verified below, an existing safe repository-relative evidence
file under `Server/artifacts/` or `docs/verification/`, and the expected report
gate/architecture/workload. Markdown is rendered from JSON and byte-compared;
manual checkboxes never substitute for proof. Evidence commits may follow the
tested SHA only when it is an ancestor of HEAD and every subsequent changed
path is under `docs/verification/`; any source/config/schema/privacy/policy
change requires new relevant gates and a new tested SHA. Reject dirty
implementation paths and do not equate an evidence-only commit with an
untested implementation change.

Artifact trust is explicit and independent of checklist contents:

```go
type Artifact struct { SourceSHA,BuildVariant,GOOS,GOARCH,Kind,Digest string }
type ArtifactManifest struct { Version,IssuedAt,KeyID string; Artifacts []Artifact }
type SignedArtifactManifest struct { Manifest ArtifactManifest; Signature string }
type ArtifactTrustKey struct { KeyID,Purpose string; PublicKey ed25519.PublicKey; NotBefore,NotAfter time.Time; Revoked bool }
type VerifiedArtifactManifest struct { Manifest ArtifactManifest; token *verifiedArtifactManifest }
func VerifyArtifactManifest([]byte,[]ArtifactTrustKey,Mode,time.Time) (VerifiedArtifactManifest,error)
```

Canonical manifest bytes use fixed struct order, sorted unique
`(source_sha,build_variant,goos,goarch,kind)` entries, lowercase SHA-256 digests
and no maps/floats/unknown fields. The signature is unpadded base64url Ed25519
over `"baby-tracker-release-artifact-manifest-v1\x00" || canonicalManifest`.
The key comes only from `--artifact-trust PATH` (bounded 0600 nonsymlink JSON),
never from the manifest. Purpose is `local-development` or `public-release`;
release mode accepts only the latter. Reject unknown, expired, not-yet-valid,
revoked, wrong-purpose or duplicate keys, noncanonical bytes, signature/key-ID
mismatch, source/build/OS/architecture/digest substitution and any checklist or
report artifact absent from the verified manifest. Key rotation is add before
use/remove only after all dependent evidence expires. Signing private keys are
external secret inputs and never committed; an ephemeral explicitly trusted
local key can prove local mode but can never satisfy release mode.

Known policy facts: collected account/profile/Apple identity/session data, child/record content and opaque operations; purpose/access/correction/export/deletion; service-readable/no-E2EE; APNs limits/self-host Apple revocation; immediate access removal/30 day recovery; 35 day default finite backups,min 30; export 24 h from creation; temporary unsuccessful Apple revoke material≤30 days. Unknown approved operator facts: controller_name/contact/region/providers/policy_url/effective_date/recovery-policy ID. Define schema with optional fields for draft, required for release. Draft front matter `publication_status: awaiting_operator_inputs`; never invent controller/domain/contact. Local validator accepts truthful draft; release needs actual approved facts and public response evidence. App model injects `policyURL:URL?`, `contact:String?`, `serverMode`; dev shows pending publication, release config rejects missing/nonHTTPS URL. Tests use explicit example.test fixture only, never published fake URL.

- [ ] **Step 1 (3 min): Validator/Settings RED.** `fixtureReleaseEvidence` creates all required local rows, temporary reports and a signed manifest/trust registry at current fixed testNow/SHA; external pending rows contain no invented proof. `corruptReleaseEvidence` mutates one row, report, manifest, signature or trust key. Execute real CLI argv in the script test.

```go
func TestPendingExternalDoesNotBlockLocalImplementationGate(t *testing.T) {
 e:=fixtureReleaseEvidence(t)
 if err:=Validate(e.Checklist,ModeLocal,testNow,e.Manifest); err!=nil { t.Fatal(err) }
 if Validate(e.Checklist,ModeRelease,testNow,e.Manifest)==nil { t.Fatal("pending evidence passed release") }
}
func TestInvalidLocalEvidenceFails(t *testing.T) {
 for _,fault:=range []string{"missing_local","unknown_gate","wrong_kind","wrong_status","stale","wrong_sha","nonancestor_sha","source_change_after_evidence","amd64_report_says_arm64","load_duration_60","missing_file","symlink","path_traversal","secret_in_report","artifact_digest_substitution"} {
  t.Run(fault,func(t *testing.T){e:=fixtureReleaseEvidence(t);corruptReleaseEvidence(t,&e,fault);if Validate(e.Checklist,ModeLocal,testNow,e.Manifest)==nil {t.Fatal("invalid evidence accepted")}})
 }
}
func TestArtifactManifestTrustFailsClosed(t *testing.T) {
 for _,fault:=range []string{"unsigned_manifest","manifest_tampered","noncanonical_manifest","unknown_signer","expired_signer","revoked_signer","wrong_purpose"} {
  t.Run(fault,func(t *testing.T){raw,trust:=signedManifestFixture(t);corruptArtifactFixture(t,&raw,&trust,fault);if _,err:=VerifyArtifactManifest(raw,trust,ModeLocal,testNow);err==nil {t.Fatal("invalid manifest accepted")}})
 }
 raw,trust:=signedLocalManifestFixture(t)
 if _,err:=VerifyArtifactManifest(raw,trust,ModeRelease,testNow);err==nil {t.Fatal("local signer passed release")}
}
```

TestPrivacyDefaultsAgree compares config/default facts and docs. Swift tests privacyURLMustBeHTTPS/selfHostExplainsAppleRevocation/privacyModelShowsNoE2EEAndExportDeletionLinks use injected Settings model. Run `rtk proxy sh -c 'cd Server && go test ./internal/release ./scripts -run "TestPendingExternal|TestInvalidLocal|TestArtifactManifest|TestPrivacy" -count=1'` and `rtk swift test --package-path Packages/BabyTracker --filter PrivacySettingsViewModelTests` RED.
- [ ] **Step 2 (4 min): Implement actual checker/scripts/docs/UI.** Verification CLI is `baby-tracker-release-check --mode local|release --checklist PATH --source-sha SHA --artifact-manifest PATH --artifact-trust PATH --now UTC`; strict closed-enum JSON/IDs/artifact/signature checks, paths stay in the allowed evidence roots, and diagnostics are safe. The separate noninteractive command `baby-tracker-release-check artifact sign --input PATH --output PATH --signing-key PATH --key-id ID --purpose local-development|public-release --issued-at UTC` reads an external bounded 0600 nonsymlink 32-byte Ed25519 seed, signs the canonical manifest atomically, and never prints key material. Verify the signed manifest before reading a digest from it. The manifest is keyed by exact `(source_sha,build_variant,goos,goarch,kind)` and maps each immutable build to its image/binary digest. Every gate names its build variant/architecture and matches the exact digest in both report and the verified manifest; there is no single primary digest. Local test-issuer gates use their explicit test build, runtime rows use their architecture image, and external hosted gates use the production build variant/digest. The script verifies git ancestry and rejects disagreement. Update measured `EXPECTED_TESTS`.
- [ ] **Step 3 (4 min): Local GREEN, implementation commit, then current-SHA evidence.**

```bash
rtk proxy sh -c 'cd Server && go test -race ./internal/release ./scripts ./cmd/baby-tracker-release-check -count=1'
rtk proxy bash Server/scripts/check-privacy-docs.sh --mode local
rtk swift test --package-path Packages/BabyTracker --filter PrivacySettingsViewModelTests
rtk make test
rtk make build
rtk make test-ui
rtk git diff --check
```

Add local validator to beta-ci only now. Its order is producers → collect current reports/digests → sign manifest → verify signature → validate. Implement `ci.sh --write-local-reports --source-sha SHA --artifact-manifest PATH --artifact-signing-key PATH --artifact-key-id ID --artifact-trust PATH --report-dir Server/artifacts/release`: reject dirty implementation, wrap each producer, preserve exit status, and write `GateReport{gate_id,source_sha,build_variant,goos,goarch,artifact_digest,observed_at,command,exit_code,details}` only after exit. It invokes the signing command with purpose `local-development`; missing signing/trust inputs fail rather than emitting unsigned evidence. Refresh verifies the manifest, reads reports, updates only local rows, retains external status, renders Markdown, then validates. Each row matches the verified manifest entry for its exact source/build/architecture; hosted external rows require `build_variant:"production"` and a `public-release` signer. `beta-ci` must not recurse.

| Local gate / report under Server/artifacts/release | Real producer and required detail |
|---|---|
| go-race-db / go-race-db.json | `rtk make server-test`, mandatory PostgreSQL race suites with zero required skips |
| swift-counted / swift-counted.json | `rtk make test`, measured literal EXPECTED_TESTS |
| ios-ui / ios-ui.json | `rtk make test-ui`, actual simulator result bundle and executed test count |
| protocol-beta / protocol-beta.json | `rtk make family-sync-e2e`, real server plus existing P3 app target result |
| protocol-faults / protocol-faults.json | `rtk make family-sync-faults`, full fault matrix |
| load-100-family / load-100-family.json | `rtk make family-sync-load`, Task 6 exact workload and latency thresholds |
| runtime-amd64 / runtime-amd64.json | `rtk proxy bash Server/scripts/dual-arch-smoke.sh` amd64 report, executed:true and runtime_goarch:amd64 |
| runtime-arm64 / runtime-arm64.json | same dual-arch run's arm64 report, executed:true and runtime_goarch:arm64 |
| migration-overlap / migration-overlap.json | prepared-mode 9→16 sequence through 15→16 with exact base/prior/feature SHAs and distinct binaries |
| checkpoint-roundtrip / checkpoint-roundtrip.json | typed authority state exhaustive round-trip, verified restore and pruning regressions |
| generation-retirement / generation-retirement.json | committed/abandoned generation inventory, retirement and retained-object absence proof |
| encrypted-backup-restore / encrypted-backup-restore.json | created ciphertext, authenticated decrypt, restore and corruption/wrong-key denial |
| restore-checkpoint / restore-checkpoint.json | `rtk proxy bash Server/scripts/verify-restore.sh --compose Server/compose.yaml --local-disposable`, actual dump/restore/quarantine/checkpoint proof |
| retention-health / retention-health.json | purge/cleanup/inventory/witness/headroom/backlog dimensions exercised without hidden skips |
| backup-freshness / backup-freshness.json | disposable scheduled backup completion and stale-over-24h readiness denial |
| sbom-vulnerability / sbom-vulnerability.json | pinned SBOM/vulnerability producers, parsed outputs and zero unwaived required failures |
| privacy-consistency / privacy-consistency.json | `rtk proxy bash Server/scripts/check-privacy-docs.sh --mode local`, current draft facts consistent |

Producer commands are invoked through `rtk`; retain existing vet, vulnerability, SBOM and OpenAPI gates in ci.sh as prerequisites. Unavailable publication credentials cannot invalidate otherwise complete local implementation. Draft operator inputs remain explicitly pending. Commit implementation first, so subsequent reports identify an immutable tested SHA; this commit alone does not assert all local gates passed.

```bash
rtk git add docs/privacy/{policy.md,app-store-disclosures.md,data-rights.md,operator-inputs.schema.json,policy-site/index.html} docs/verification/{family-sync-release-checklist.json,family-sync-release-checklist.md} Server/scripts/{check-privacy-docs.sh,check-release-checklist.sh,release_checklist_test.go,ci.sh} Server/internal/release/{checklist.go,checklist_test.go,artifacts.go,artifacts_test.go} Server/cmd/baby-tracker-release-check/main.go Packages/BabyTracker/Sources/SettingsFeature/{PrivacySettingsView,PrivacySettingsViewModel,SettingsView}.swift Packages/BabyTracker/Tests/SettingsFeatureTests/PrivacySettingsViewModelTests.swift Apps/BabyTrackerApp/AppEnvironment.swift Server/README.md Makefile HANDOFF.md
rtk git diff --cached --check
rtk git commit -m "docs(sync): validate local privacy and explicit release evidence gates"
```

Now run the current committed implementation and create an evidence-only commit only on success. The `--refresh-local` copier first copies referenced machine reports to `docs/verification/family-sync-evidence/`, preserves report bytes plus a SHA-256 digest manifest, rejects secret-bearing content, and points durable checklist evidence paths there. Include result-bundle summaries rather than personal simulator data; Server/artifacts remains rebuildable.

```bash
rtk make beta-ci
rtk proxy bash Server/scripts/check-release-checklist.sh --mode local --checklist docs/verification/family-sync-release-checklist.json --artifact-manifest Server/artifacts/release/artifact-manifest.signed.json --artifact-trust "$BT_RELEASE_ARTIFACT_TRUST_FILE"
rtk git add docs/verification/{family-sync-beta.md,family-sync-release-checklist.json,family-sync-release-checklist.md}
rtk git add docs/verification/family-sync-evidence
rtk git diff --cached --check
rtk git commit -m "test(sync): record verified local beta readiness evidence"
```

- [ ] **Step 4 (external): Execute authorized provider/publication/device gates.** With actual approved account/provider/EU region/keys/domains/controller/contact/signing/recovery-policy inputs, run real adapter conformance+retention erasure, hosted deploy/daily backup/restore, public health/readiness/digest/schema/roles/bounded logs, real Apple sign-in/revocation, actual APNs best-effort wake, two-device rendered convergence, homelab HTTPS, approved owner recovery, public policy and App Store disclosures. Publishing/App Store changes/deployment need scoped user authority. Missing credentials mark pending_external; local work remains reviewable/complete. Record date/source SHA/image digest/device+OS/region/expected+observed/evidence path without personal data. Do not infer guaranteed APNs delivery.
- [ ] **Step 5 (release): Run real release validator and commit existing evidence only.** If approved operator facts change policy or release configuration in Step 4, commit those exact scoped changes first with `docs(privacy): record approved operator policy inputs`, then regenerate current-SHA local reports and repeat affected authorized external gates. Only `docs/verification/` changes are allowed after the tested SHA. Do not stage policy changes together with evidence and silently invalidate the tested source identity.

```bash
rtk proxy bash Server/scripts/check-privacy-docs.sh --mode release
rtk proxy bash Server/scripts/check-release-checklist.sh --mode release --checklist docs/verification/family-sync-release-checklist.json --artifact-manifest docs/verification/family-sync-evidence/artifact-manifest.signed.json --artifact-trust "$BT_RELEASE_ARTIFACT_TRUST_FILE"
rtk git add docs/verification/{family-sync-beta.md,family-sync-release-checklist.json,family-sync-release-checklist.md} docs/verification/family-sync-evidence
rtk git diff --cached --check
rtk git commit -m "test(sync): record authorized public beta release evidence"
```

Release remains nonzero with any missing/failing/pending/stale mandatory evidence. Never create evidence commit or ready claim before these actual gates.

## Mandatory review amendments for P4 acceptance

These items supersede older illustrative signatures and are required in the
owning task's RED/GREEN/report/commit.

### P4.1 typed checkpoints, retention capacity, and erasure proof

- [ ] Consume P1's versioned typed `AuthorityCheckpointState` and verified
  header/anchor restore API; do not rebuild private maps or use a lossy flat
  fact DTO. Round-trip active/unavailable/deleted identities and tombstones;
  slots and recovery claims; device origin/digest keys; session parent/lineage/
  reuse; family purge start command/revision/receipt/time; memberships,
  invitations, entitlements; and replay receipts with scope/kind/family
  compaction metadata. Permanent account/identity deletion tombstones and
  identity anti-rollback evidence survive independently and are never removed
  merely because one family is purged. Only facts and receipts exclusively
  scoped to the purged family may be omitted after the verified cutoff proves
  every recoverable family copy is gone; any required minimal survivor contains
  no erased family ID or other family-linking identifier.
- [ ] Define receipt/session pruning by kind and cutoff. Never prune live
  refresh-reuse or permanent account/identity deletion and anti-rollback
  evidence. Family-deletion facts/receipts remain until the verified erasure
  cutoff, then may be omitted only with no surviving erased family identifier.
  Run `TestAuthorityCheckpointRoundTripEveryDecision`,
  `TestCheckpointReceiptPruningPreservesReplayAndReuseDenial`, and
  `TestRecoveryClaimAndPurgeTerminalStateSurviveCheckpoint`.
- [ ] Materialize P1's `AccountGenerationRetentionRecord` before the final
  credential reference is erased. The P4 worker may call
  `CredentialReplayMarkerPurger` only after the committed typed checkpoint
  contains the permanent account/identity tombstone and next actor generation,
  main-object absence/provider terminal state is proven, and the signed backup
  watermark plus retired generations prove no pre-deletion restore or
  re-registration dependency remains. State changes `retained -> eligible ->
  purged`; an ambiguous provider delete retains authenticated binding/object
  coordinates and retries. Confirmed absence writes a content-free purge
  receipt before those coordinates are removed. Family purge alone never
  qualifies an active account marker. Run
  `TestAccountGenerationMarkerWaitsForCheckpointAndBackupRetirement`,
  `TestAmbiguousReplayMarkerPurgeRetainsCoordinates`, and
  `TestFinalAccountGenerationPurgeRemovesOnlyOldReplayMarker`.
- [ ] Use P1's stable instance-bound retention actor/scope and deterministic
  start/complete IDs. No bearer, HTTP, or operator route can mint it. Retry
  across restart yields the identical command hash and receipt.
- [ ] Store checkpoint-format enablement as a monotonic CAS record beside the
  independent head/witness with instance and minimum-reader format. Add
  inspect/confirmed-enable CLI, restart/database-restore/concurrent-enable/
  rollback tests, and old-reader fail-closed behavior.
- [ ] Move `BackupInventoryProvider`, local inventory adapter,
  `WatermarkWitnessStore`, and `GenerationAllocator` implementation/conformance
  into P4.1. The admin sequence is inventory → canonical signed watermark →
  witness CAS → checkpoint/whole-head CAS → post-retention inventory → exact
  old/abandoned generation retirement. P4.5 orchestrates backup creation and
  restore but does not redefine these ports.
- [ ] Prove abandoned generation retirement with a signed inventory of its
  immutable objects plus evidence it was never a committed head and its
  reserved generation can never be reused. Concurrent CAS-loss/restart tests
  never retire current, higher, or potentially reachable generations.
- [ ] Enforce the beta checkpoint capacity policy exactly: 64 MiB maximum,
  8 MiB reserve, admission threshold 56 MiB. At/above 56 MiB, deny new family,
  membership, invitation and device authority growth with stable
  `authority_capacity`; allow only bounded refresh plus revocation, deletion,
  recovery/forfeit, purge and checkpoint operations to use the reserve.
  Existing canonical family reads/record mutations remain available because
  they do not grow authority state. Fixed-cardinality metrics expose encoded
  bytes, remaining reserve and pause durations. Under P4.6's foreground load,
  each exclusive-fence hold has p95 <=250 ms and no hold >2 s. Breach or less
  than one 64 KiB event of reserve fails hosted readiness and release evidence;
  no test/configuration may silently raise the beta bounds. Add
  `TestCheckpointCapacityPausesAdmissionAt56MiB`,
  `TestCheckpointReserveAllowsSafetyCommandsOnly`, and
  `TestCheckpointUnderForegroundLoadMeetsBoundedPausePolicy`.
- [ ] Expose independent retention health for purge, temporary cleanup,
  inventory freshness, witness, checkpoint headroom, and retirement backlog.
  Hosted readiness requires each; self-host reports an explicit degraded
  operator-responsibility status.
- [ ] Final family erasure enumerates and inspects/decrypts every database
  backup, ledger/checkpoint generation and provider version, abandoned object,
  and declared test/restore artifact. Restore every still-declared database
  backup for absence. Incomplete/unreadable inventory is blocked, never erased.

### P4.2 APNs configuration, tenancy, and invalidation

- [ ] Freeze exact hosted-only configuration:
  `BT_APNS_TEAM_ID`, `BT_APNS_KEY_ID`, `BT_APNS_TOPIC`,
  `BT_APNS_ENVIRONMENT`, `BT_APNS_SIGNING_KEY_FILE`,
  `BT_APNS_TOKEN_PEPPER_FILE`, and `BT_APNS_COLLAPSE_KEY_FILE`. Files are
  strict bounded 0600 nonsymlinks; keys have IDs/rotation rules and may not
  reuse Sign in with Apple material. Missing hosted config fails the APNs
  readiness dimension with safe diagnostics; self-host never loads it.
- [ ] `push_tokens` is account/device scoped with no family RLS fiction and no
  direct request-role DML. Guarded definers update the authenticated account/
  device; delivery-worker functions perform lookup. Family purge does not
  remove a token still eligible for another future family; account/device
  deletion or no remaining eligible authority owns cleanup.
- [ ] Security invalidations for account deletion, member removal, family
  deletion, token replacement, and `epoch_changed` wake/clean queue rows
  promptly, with a bounded periodic scan as loss fallback. HTTPS remains the
  correctness source. Queue age, outcomes, and worker heartbeat are metrics;
  tokens/provider bodies never enter logs/artifacts.

### P4.3 export authorization, invalidation, and truthful expiry

- [ ] Current active owner export uses P1 `export_family` and is allowed when
  `family_sync` is suspended or expired. Removed/former owners deny. Ordinary
  sync/import remains entitlement-gated.
- [ ] Projection invalidation or a bounded worker scan immediately fails
  queued/running jobs and erases ready live material on ownership/membership/
  family deletion/epoch change without waiting for status/download. Test each
  state through worker outage and restart.
- [ ] Adopt the truthful beta contract: download and live export rows expire at
  24 hours, while encrypted backup copies age out under finite backup policy.
  Shared retained KEK + backup means row deletion is not cryptographic erasure.
  Privacy text and `TestExportExpiryClaimMatchesBackupRecoverability` use that
  wording.
- [ ] Keep a compact content-free request receipt for an explicit seven-day
  idempotency horizon after job expiry; identical replay returns expired status
  without regenerating or recharging. After seven days the request ID may be
  reused as a new charged request. Test active/failed/expired/receipt/horizon
  boundaries.
- [ ] Freeze export route matrix and fixtures: required `export-v1` capability,
  header/body equality, owner/requester statuses after transfer/deletion/
  expiry, integer `Retry-After`, explicit 405 for HEAD and Range, and connection
  close/audit-only behavior if stream deadline expires after headers.

### P4.4 observability and AGPL network source offer

- [ ] Bind `/metrics` to a separate configurable internal listener that
  defaults to loopback; it is absent from the public router. Smoke proves
  public 404/denial and internal success. A future proxy alternative requires
  explicit mTLS/trusted-hop/CIDR validation, not a trusted header.
- [ ] `/v1/server` and Settings/About display operator-configured
  `source_code_url` and source version. Self-host docs explain AGPL Corresponding
  Source duties for modified network service. Hosted release verifies the
  public URL is reachable and matches deployed source/image; no repository URL
  is invented before operator input.
- [ ] Add fixed-cardinality queue age, oldest purge deadline, cleanup/export
  overdue, last successful inventory/backup, retirement backlog, APNs outcome,
  and worker heartbeat metrics/status. Readiness/release consume them directly.
- [ ] Define bounded log retention/access for app, workers, database-facing
  diagnostics, and reverse-proxy access/error logs. Tests seed invitation/
  Apple/APNs secrets, raw IPs, names, notes, archive data, and provider bodies
  and scan all layers over bounded ranges.

### P4.5 encrypted daily backup and correct restore ancestry

- [ ] Implement streaming authenticated encryption before durable backup
  storage with purpose-specific key ID/config and bounded memory. Daily
  scheduling covers PostgreSQL plus ledger/head/witness/inventory metadata,
  records successful completion, and alerts/fails hosted readiness when older
  than 24 hours. Self-host docs provide an executable scheduled example.
- [ ] Split manifest verification into `VerifyCreation` and `VerifyRestore`.
  Creation requires exact coordinated dump/head/epoch/witness/inventory.
  Restore requires the manifest head to be a cryptographically verified
  ancestor/checkpoint-compatible state above the accepted erasure cutoff while
  the latest independent head/witness remains current. Reject wrong ancestor,
  retired generation, future head, equal revision/different hash, and
  head-change-after-verify.
- [ ] Restore decrypts into bounded temporary storage, verifies before SQL
  replacement, enters quarantine, preserves latest independent head/witness,
  rotates epoch once, reconciles/probes, and CAS-leaves only the exact latest
  head. Never restore head/witness from the database-era bundle.

### P4.6-P4.7 genuine acceptance and fresh evidence

- [ ] Load variants include checkpoint publication, due purge, export creation/
  download, APNs queue churn, and 200-client reconnect distribution. A
  compile-time test-only observer inside the actual server records the commit
  boundary; remote completion means durable client materialization. Timeouts
  and failures contribute one 30000 ms sample each to the same nearest-rank
  percentile vector and remain in the separate successful-cycle denominator;
  success-only percentile reporting is invalid evidence.
- [ ] Black-box evidence records listener URL, image digest, reported source
  SHA/GOARCH, database/schema instance, migrations/authority initialization,
  and restarts, then proves Go and Swift clients used that listener. The test
  issuer/control route is absent from the production image. An unused container
  beside `httptest` fails.
- [ ] Add dedicated release gates for checkpoint round-trip/retirement,
  encrypted-backup restore, public source offer, retention health, and backup
  freshness using the exact closed IDs `checkpoint-roundtrip`,
  `generation-retirement`, `encrypted-backup-restore`, `public-source-offer`,
  `retention-health`, `backup-freshness`, `hosted-retention-health`,
  `hosted-backup-freshness`, and `hosted-encrypted-backup-restore`. Do not hide
  them inside `hosted-deployment`.
- [ ] Privacy artifacts state immediate deletion access revocation, 30-day
  recovery, live-store purge, 24-hour operational export expiry, maximum backup
  disappearance time, and unsuccessful Apple-revocation material up to 30
  days. They never promise every byte disappears at day 30/hour 24.
- [ ] Evidence checker rejects changed submodules, untracked implementation/
  config/migration/OpenAPI files, mismatched generated Go/Swift contracts, and
  reports missing expected named-test output. Producer commands, source/image/
  architecture/workload, and tested-SHA ancestry must be independently valid.
  It first verifies the canonical Ed25519 artifact manifest against the
  external purpose-qualified trust registry; unknown/expired/revoked/local-only
  release signer, modified manifest/report/artifact, and source/build/OS/arch
  substitution all fail. Run `TestArtifactManifestTrustFailsClosed`,
  `TestArtifactManifestKnownAnswer`, and
  `TestReleaseRejectsLocalDevelopmentSigner`.
- [ ] Run the 18 named regressions from `p4-preflight.md`, now recorded here as:
  `TestAuthorityCheckpointRoundTripEveryDecision`,
  `TestCheckpointReceiptPruningPreservesReplayAndReuseDenial`,
  `TestRecoveryClaimAndPurgeTerminalStateSurviveCheckpoint`,
  `TestRetentionActorRetryStableAcrossRestart`,
  `TestCheckpointEnablementSurvivesDatabaseRestore`,
  `TestSchemaCleanupHooksCannotFalseCompleteAcrossOverlap`,
  `TestCheckpointUnderForegroundLoadMeetsBoundedPausePolicy`,
  `TestHostedReadinessFailsWhenRetentionCannotComplete`,
  `TestExportAllowedWhenFamilySyncEntitlementSuspended`,
  `TestExportAuthorizationChangeErasesReadyMaterialWithoutRead`,
  `TestExportExpiryClaimMatchesBackupRecoverability`,
  `TestAPNSConfigPurposeSeparationAndSafeDiagnostics`,
  `TestPublicListenerCannotReachMetrics`,
  `TestBackupCreateRestoreUsesEncryptedBytes`,
  `TestRestoreAcceptsOnlyVerifiedAuthorityAncestorAboveWatermark`,
  `TestPublicSourceOfferMatchesDeployedSource`,
  `TestMaintenanceConcurrencyInBetaLoad`, and
  `TestE2EClientsReachedReportedContainerDigest`.

For P4.1, P4.2, and P4.3, the task produces two commits: the compatibility-only
predecessor first, then the feature commit. Reports record base/predecessor/
feature SHAs and prepared-mode overlap output. A migration and its feature may
not be squashed into one commit before acceptance because that destroys the
deployable predecessor artifact.

## Completion and self-review

Local completion: `rtk make beta-ci` with explicit local artifact signing/trust secret paths, `rtk make family-sync-e2e`, `rtk make family-sync-faults`, `rtk make family-sync-load`, `rtk make test-family-sync-e2e`, `rtk make test`, `rtk make test-ui`, `rtk make build`, `rtk proxy bash Server/scripts/check-release-checklist.sh --mode local --checklist docs/verification/family-sync-release-checklist.json --artifact-manifest Server/artifacts/release/artifact-manifest.signed.json --artifact-trust "$BT_RELEASE_ARTIFACT_TRUST_FILE"`, `rtk git diff --check main...HEAD`. Review all four remaining plans/spec/exact diff/overlap artifacts/local evidence; no Critical/Important finding remains. Public beta additionally requires a purpose-`public-release` signature, release-mode validation and all authorized external/manual evidence.

- I2: P4.1–3 own 000014–000016 SQL/manifests, literal prior/new ranges and distinct-binary overlap, preserving historical checksums.
- I11: P4.1 defines signed preimages/state/anchor/CAS/replay/trust rotation/independent watermark/whole-generation local+hosted deletion, faults and actual family-byte absence.
- I12: unlocked due scan→authoritative Start→projected job→short claim→exact SQL delete→authoritative Complete; crash/recovery/privilege assertions pin ordering.
- I13: active-family temporary cleanup is explicit; token/export hooks register only with their owning later schema; physical export/key erase and schema 16 full purge are tested.
- I14: P2 Claim/Complete/Retry signatures remain exact, additive lease/version/Drop contract guards N/N+1 and V/V+1, death/revocation/epoch/two workers; self-host routes404.
- I15: deterministic archive/job/rates/lifetime/byte-level encryption/Go+Swift APIs/repeatable-read/fenced streaming/local cleanup have executable failure-first gates.
- I16: concrete files/tests/commands/CLI/validator/architecture runtime/scheme ownership; one existing issuer/protocol/app harness reused.
- M3: exact migration/manifest/count/project/scheme staging in owning commits; no broad later migration add.
- M4: locally verified prerequisites allow progress while provider/signing/publication/device gates remain pending external.
- Self-review checked spec§§5.3, 7.4, 9–13, upstream names, helper ownership, each Review Focus test and unfinished-marker scan. This is a plan review, not a claim the planned feature tests/release gates already passed.
