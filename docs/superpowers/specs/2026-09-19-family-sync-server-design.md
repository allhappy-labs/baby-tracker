# Family Sync Server Design

**Date:** 2026-09-19\
**Status:** Reviewed and hardened for implementation\
**Scope:** Complete end-to-end public beta: Go server, PostgreSQL persistence,
deployment assets, Sign in with Apple, family management, invitations, and iOS
offline synchronization

## 1. Purpose

Baby Tracker is currently a local-first iOS application. This project adds an
optional shared-family mode so trusted caregivers can see the same children,
records, and active timers while preserving immediate offline use.

The first release is a public, multi-tenant beta. Any family may register. The
hosted beta is free, with a future plan to charge approximately USD 10 per year
for hosted family sync. The self-hostable server remains free under AGPLv3.

Success means:

- Local-only use remains fully functional without an account.
- A caregiver can create or join one family using Sign in with Apple.
- Existing local children and records can be imported without duplication.
- Caregivers normally see active-device changes within two seconds under
  measured beta load.
- Offline changes eventually converge deterministically after reconnection.
- Family isolation, revocation, deletion, recovery, export, and audit behavior
  are enforceable and testable.
- The protocol is platform-neutral so a future Android client does not require
  a server redesign.
- Future photos and attachments fit established ownership and storage seams
  without embedding blobs or storage URLs into record payloads.

## 2. Scope

### 2.1 Included in the beta

- Go API service with PostgreSQL
- Sign in with Apple and server-managed sessions
- Local-only, create-family, and join-family modes
- Owner and caregiver roles
- Link and QR invitations backed by the same token
- Synchronization of babies, feeds, sleeps, diapers, and active timers
- Near-real-time cursor notifications while the app is active
- Hosted silent push wake-ups when iOS permits them
- JSON family export
- Thirty-day family deletion recovery and eventual purge
- Hosted entitlement seam without StoreKit billing
- Provider-neutral Docker image and Docker Compose self-host bundle
- Health, readiness, metrics, structured logs, migrations, and operator CLI
- Attachment-ready protocol and model contracts without file handling

### 2.2 Deliberately excluded

- StoreKit purchase and subscription UI
- Android or web clients
- End-to-end encryption
- Photo or attachment uploads, downloads, attachment object storage, or UI
- A browser-based operator console
- User-facing audit-log browsing
- Multi-region writes
- Multiple active families per account in the beta UI or API

## 3. Product decisions

### 3.1 Identity and family membership

Sign in with Apple is the only beta identity provider. Internally, accounts and
provider identities are separate so Google or another provider can be added for
Android without changing family or record ownership.

An account may have one active family in the beta. The persistence model uses a
general membership join table so this can become many-to-many later.

Roles are:

- **Owner:** all caregiver permissions plus invitations, member removal,
  ownership transfer, child deletion, JSON export, family recovery, and family
  deletion.
- **Caregiver:** view and mutate all children and tracking records, including
  editing and deleting records. A caregiver may leave the family.

Caregivers may add children. Only the owner may delete a child. An owner must
transfer ownership or delete the family before deleting their account.
Every live family has exactly one active owner; ownership transfer changes the
old and new roles atomically and can never leave zero or two owners.

Revoking Sign in with Apple consent signs the account out but does not silently
delete shared family data. In-app account deletion removes Apple identity and
profile data. A departing caregiver is pseudonymized as a former caregiver in
historical attribution. An owner must first transfer ownership or schedule the
family for deletion.

### 3.2 Invitations

The owner creates a cryptographically random, single-use invitation that is
revocable and expires after a configurable default of seven days. The server
stores only a hash. A shareable link and QR code encode the same server address
and token.

Hosted invitations use Universal Links. An arbitrary self-hosted domain cannot
be predeclared in the distributed app's Associated Domains entitlement, so a
self-hosted HTTPS invitation opens a minimal handoff page that offers a copyable
code and an app custom-scheme handoff. The app also accepts the same token
through its scanner or manual paste. Invitation parameters are fully validated,
and opening a link never redeems membership or performs another destructive
action without authenticated confirmation.

An invitation for a non-default server never changes endpoints automatically.
The app normalizes and displays the HTTPS origin, rejects embedded credentials
and unexpected URL components, and requires explicit confirmation before it
sends an Apple identity token or invitation token to that server.

### 3.3 Local import

Enabling family sync starts a resumable import. Local babies automatically map
to shared babies only when their normalized names and birth dates match exactly.
Unmatched babies are created. Multiple exact candidates stop for confirmation;
there is no fuzzy match.

When creating a family, local baby UUIDs are retained. When joining, an
unmatched baby also retains its UUID, but an exact match adopts the existing
shared baby UUID. The importer rewrites that baby's dependent local record and
timer references in one transaction. Record UUIDs are always retained. A
durable import checkpoint and mutation idempotency make interruption and
retries safe.

The import first establishes membership, then calls a serialized server import
resolver with the local baby candidates and record manifests. The resolver
locks the family import boundary, resolves every exact normalized-name and
birth-date match, creates unmatched babies, and returns a stable complete ID
mapping before the client rewrites any local references. This makes two
caregivers concurrently importing the same baby converge on one canonical baby
instead of racing two creates. Multiple pre-existing exact candidates remain an
explicit conflict requiring owner resolution.

All import routes negotiate exactly `records-v1,import-resolver-v1`. Chunk zero
freezes the initiating device, import mode, complete bounded baby candidates,
declared record count, and normalization version; later chunks contain record
manifests only. Canonical chunk hashes include every immutable field and the
finalize flag, and committed chunk/owner-command retries return a stored
byte-identical status/body. Empty nonfinal chunks are invalid. New import IDs
are bounded to 1,000 retained rows per family and 100 for the initiating account
within that family, counting active, completed, and expired rows under the
family lock; four unfinished imports per family and one unfinished import per
account globally remain separate concurrency caps. Existing IDs remain
readable/retryable at the history cap.

Conflict status is independently and deterministically paged by conflict UUID.
Every resolver response reports the total conflict count, whether its conflict
page is complete, and an optional continuation. The continuation is at most 512
bytes, is signed by the normal page keyring under
`baby-tracker-import-conflicts-v1`, and binds the complete server, tenant,
viewer, resolver-revision, paging-limit, expiry, and key identity. Authorization
precedes token lookup. A conflict, owner decision, or visibility change advances
the resolver revision, so a stale token or choice returns
`import_resolution_stale` and restarts paging. Owner decisions are stored apart
from mappings, and the complete exact-match graph is rechecked under the family
lock before an injective mapping is published; a newly appearing exact candidate
also makes an earlier choice stale.

Only after the mapping is durable does the client transactionally move the
existing local workspace into the shared realm and rewrite dependent baby IDs.
New writes from that point join the shared outbox, even if record import is
still catching up. Feed, sleep, and diaper records are never heuristically
merged: equal-looking records with different UUIDs remain separate. An imported
record UUID that does not exist is created; one that already exists with the
same canonical semantic content is an idempotent no-op; one that exists with
different content is an explicit import conflict and is never silently resolved
by last-write-wins.

The manifest also carries active timer starts using the original timer-start
content hash and local baby relationship. After mapping, imported starts use the
ordinary timer collision rules. Imported timer switch/attach/stop commands and
imported record updates/deletes are rejected; a timer completion is an ordinary
completed record and never receives record-equality deduplication. For ordinary
imported records the mapped parent is compared separately from the canonical
content hash (which excludes `baby_id`); an equal no-op returns the original
authoritative entity/cursor without a journal sequence or wake hint and stores
that exact receipt.

Every import without a complete mapping expires at its fixed staging deadline,
including a finalized ambiguous import. Access marks it expired and the same ID
forever returns `import_expired`. The retention worker later removes bulky
chunks, candidates, manifests, conflicts, decisions, and response receipts in
bounded batches, while retaining the small expired session-ID row until the
whole family is purged so an expired ID can never be recycled.

Birth dates cross the wire as date-only `YYYY-MM-DD` values, not instants. Name
normalization is versioned and covers Unicode normalization, trimming, and
case-folding. The unused legacy `Baby.photoData` field remains local and is not
uploaded until attachment support has an explicit migration.

The one-time SwiftData migration converts an existing `Date` to the calendar
date currently displayed by the app, then presents that date in the import
summary before automatic matching. New records store birth date as a date-only
value locally as well, eliminating future timezone drift.

### 3.4 Attribution

The display name Apple provides on first authorization seeds an editable
caregiver display name. Record detail shows subtle creator/last-editor
attribution. The history list remains uncluttered. The server retains a complete
family-scoped mutation journal and security/membership audit trail.

Legacy local records do not contain a trustworthy account identity, so initial
imports attribute their creation and latest edit to the importing caregiver
while preserving their domain timestamps. Subsequent attribution always comes
from the authenticated server session, never a client-supplied display name.

### 3.5 Local data realms

The iOS database assigns every row to a local data realm: either the permanent
local-only workspace or a tuple of canonical server origin, stable server
instance ID, and family ID. Credentials and cursors are keyed by the same
identity. A signed-in endpoint cannot be edited in place; switching servers
requires an explicit disconnect, revocation where reachable, and purge of the
old shared realm. A different server instance appearing at a previously used
origin requires explicit user confirmation before any upload. This prevents
hosted credentials, self-hosted credentials, and family records from being
mixed or uploaded across servers.

All feature queries and mutations are scoped to the active realm. Realm changes
are explicit state transitions rather than global URL preferences, and a failed
or interrupted transition resumes from its checkpoint before the app exposes a
different realm.

## 4. Architecture

The backend is a modular monolith: one Go service with internal modules for
identity, families, records, synchronization, realtime delivery, operations,
and the future attachment contract. It deploys as a stateless container and
uses PostgreSQL as the authoritative server store.

```text
iOS local store
  |-- immediate offline reads and writes
  |-- transactional mutation outbox
  `-- sync coordinator
          | HTTPS + versioned JSON
          v
Go service --------- WebSocket cursor notifications
  |-- identity and sessions
  |-- family authorization
  |-- typed record and timer mutations
  |-- sync and conflict ordering
  |-- audit and export
  `-- attachment interfaces only
          |
          +-------------------------> SecurityLedger + LedgerHeadStore
          |                            (outside DB snapshots)
          v
PostgreSQL
  |-- canonical current state
  |-- immutable mutation journal
  |-- security and membership audit projections
  `-- durable delivery/outbox state
```

Feature code continues to write through `BabyStore`. A sync coordinator converts
the local outbox into versioned protocol mutations. The server validates and
applies each accepted mutation to canonical state and appends its journal entry
in the same database transaction.

Clients synchronize with an opaque cursor containing server identity, recovery
epoch, and a monotonically increasing family sequence.
WebSocket messages and hosted background pushes only announce that a newer
cursor exists. HTTPS sync always retrieves authoritative changes, so missed,
duplicated, delayed, or reordered notifications do not affect correctness.

The API is a checked-in OpenAPI 3.1 contract using JSON over HTTPS. The wire
schema is independent of Swift, SwiftData, and Go implementation types.

## 5. Server domain model

### 5.1 Identity and tenancy

- `accounts`: ID, editable display name, status, lifecycle timestamps
- `account_identities`: account ID, provider, immutable provider subject
- `devices`: server-issued ID bound to an installation/account/server session,
  last-seen time, and an optional hosted APNs token
- `families`: ID, display name, lifecycle state, `purge_after`
- `memberships`: family ID, account ID, role, status, lifecycle timestamps
- `invitations`: family ID, token hash, creator, expiry, redeemed/revoked state
- `family_entitlements`: family ID, entitlement key, source, status, expiry
- `mutation_receipts`: authenticated device ID, mutation ID, canonical mutation
  hash, stable outcome, and resulting cursor where applicable

The hosted beta grants `family_sync` without payment. A future StoreKit verifier
may change the entitlement source without changing synchronization. Self-hosted
mode supplies an always-enabled entitlement policy.

### 5.2 Synchronized entities

PostgreSQL stores typed tables for babies, feed entries, sleep entries, diaper
entries, and active timers. Each family-owned synchronized entity has:

- Client-generated UUID and required `family_id`
- Baby relationship where applicable
- Typed domain columns
- Monotonic entity version
- Creator and last-editor account IDs
- Domain event times, client mutation time, and server acceptance time
- Optional tombstone time

Domain event times are the feed, sleep, or diaper times and retain their
original timezone context. Client mutation time describes when an edit was
made. Neither is conflict authority; server acceptance order decides conflicts.

Wire schemas explicitly allowlist domain fields. Device presentation state such
as `ActiveTimer.liveActivityID`, selected-child preferences, local sync leases,
and the legacy `Baby.photoData` value never enters canonical server records.

### 5.3 Mutation journal and audit

Every accepted mutation receives a family cursor. Cursor allocation is
serialized per family by locking the family sequence row for the full database
transaction, incrementing it, applying canonical state, writing the receipt,
and appending the journal entry before commit. A sync high-water mark comes only
from the family row's committed sequence in the reader's consistent snapshot;
database sequences are not used because allocation order is not commit order.
The immutable journal stores
the cursor, canonical post-mutation representation or tombstone, actor, device,
operation, and server time. Pulling after a cursor therefore returns a totally
ordered stream of complete entity snapshots.

A unique `(device_id, mutation_id)` receipt provides idempotency. The device ID
comes from the authenticated session, not from an untrusted request field. A
retry with the same canonical semantic hash returns the stored result; reuse of
the same mutation ID for different mutation content is rejected. Once an entity
is tombstoned it cannot be resurrected by an ordinary delayed update. A future
restore operation would be explicit and separately authorized.

Security and lifecycle commands use a small event-sourced `SecurityLedger`
outside ordinary database snapshots as the source of truth for authorization
state. Membership removal, ownership transfer, account deletion, family
deletion/recovery, invitation redemption/revocation, and session revocation are
serialized per affected account/family. The service reconstructs the current
security state, validates the actor and command against it, and durably appends
one candidate idempotent event with the expected prior security revision. It
then atomically advances an independently stored expected ledger head containing
the revision and event hash; that head advance is the authoritative commit
boundary. A crash before it leaves an ignored candidate suffix, while a crash
after it leaves a committed event that must replay. Only a committed event is
projected into PostgreSQL in an idempotent transaction. The command is
acknowledged only after the projection revision matches the committed ledger
head; affected authentication and family traffic fail closed while it does not.
A command that fails validation never reaches the commit boundary and cannot be
replayed accidentally. Ledger entries are monotonically numbered and
hash-chained, and startup compares the chain's terminal revision and hash with
the independent expected head. A gap, modified entry, or otherwise valid-prefix
truncation therefore fails readiness.

Hosted mode stores encrypted immutable ledger objects in the same European
region but outside PostgreSQL backup/restore and keeps the expected head in a
replicated encrypted configuration store in a separate failure domain.
Self-hosted mode provides the same interfaces using separate fsynced ledger and
head persistent volumes; its backup and restore runbook treats both volumes as
required security state and refuses a mismatched pair. On any database restore,
traffic remains in authorization quarantine until the ledger has replayed and
reconciled all lifecycle state. This prevents an older database snapshot from
resurrecting a removed member, deleted account, former owner, consumed invite,
or revoked session. The normal family mutation journal and database audit table
remain PostgreSQL data and leave backups under the normal retention policy; the
security ledger retains only opaque lifecycle state required to prevent
rollback and is compacted to a signed current-state checkpoint. A final family
tombstone remains until no retained database backup can contain that family;
after that point its ledger state is erased with the family.

### 5.4 Attachment-ready boundary

The protocol defines future attachment metadata with stable ID, family ID,
creator, media type, byte size, integrity checksum, lifecycle, and typed links
to a baby or record. An internal `BlobStore` interface uses an opaque storage
key and anticipates S3-compatible implementations plus short-lived authorized
transfers. Provider bucket names, object keys, and permanent storage URLs never
appear in the public API.

The beta does not create attachment tables, endpoints, object storage, quotas,
or UI. Record tables do not contain blob data or permanent storage URLs. A JSON
export includes an empty versioned attachment manifest so the archive format can
grow additively.

## 6. Authentication and sessions

The Settings tab lets a user select the default hosted endpoint or enter a
custom HTTPS base URL before authenticating. Custom endpoints remain visibly
labeled.

The sign-in exchange is:

1. iOS first confirms the independently selected canonical HTTPS origin and the
   server's stable instance ID. It requests a short-lived, single-use challenge
   from that identity and computes the Apple nonce as
   `SHA256("baby-tracker-auth-v1\\0" || canonical_origin || "\\0" ||
   server_instance_id || "\\0" || challenge)`. The server-returned origin is
   never allowed to replace the user's selected origin when computing it.
2. iOS sends the identity token, challenge nonce, and first-response profile
   data to the selected server. It also sends the one-time authorization code
   to the hosted service, which has the Apple developer credentials required to
   exchange it.
3. The server atomically consumes the challenge, recomputes the domain-separated
   nonce from its configured public origin and persistent instance ID, validates
   the token signature, nonce, issuer, audience, and expiry, then resolves
   `(provider, subject)` to an account. A token minted from a challenge at a
   different origin or instance is invalid even when both servers accept the
   same Apple audience. The hosted service exchanges the authorization code with
   Apple; a self-hosted server issues only its own application session and never
   receives the app developer's Apple private key.
4. The server returns a short-lived access token plus an opaque rotating refresh
   token.
5. iOS stores credentials only in Keychain.

Application refresh tokens are hashed at rest, tied to account/device/server,
rotated on use, and revoked when reuse is detected. Membership removal
invalidates that family's sessions and closes live connections. The hosted
service signature-validates Apple's server-to-server account notifications.
Self-hosted sessions instead rely on the iOS client's Apple credential-state
check and require a new nonce-bound Apple identity token at an absolute maximum
session age of 24 hours; refresh tokens cannot extend that deadline. Operators
may configure a shorter age but not a longer one. This bounds continued access
when a device cannot receive Apple's server-to-server account notifications.
Self-hosted account deletion explicitly explains that it cannot revoke the
app-wide Apple authorization and provides the documented Apple-account settings
path as a manual fallback. Hosted mode continues to validate Apple's
server-to-server notifications and revoke its stored Apple refresh token.

Apple supplies a person's name only on the first authorization. If it is absent
when connecting to a new self-hosted server, the app offers its locally saved
display name for confirmation or asks the user to enter one; it never invents a
name from email or Apple subject identifiers.

Account deletion is initiated and confirmed inside Settings. Hosted deletion
also revokes the stored Apple refresh token. Self-hosted deletion has no Apple
refresh token to revoke and removes only that server's application session and
account data. An unexpected Apple credential loss for the sole owner never
auto-deletes the family or auto-promotes a caregiver: day-to-day family access
continues, owner-only operations freeze, and the operator CLI follows a
documented manual ownership-recovery procedure.

## 7. Synchronization protocol

### 7.1 Mutation and pull flow

The current `ChangeLogEntry` is not replayed onto the server as historical wire
traffic. A versioned SwiftData migration first assigns existing rows to the
local-only realm and introduces the new outbox, cursor, import-checkpoint, and
attribution fields. Initial family import snapshots current live entities and
active timers into the versioned protocol; it does not upload the legacy
unversioned payload log. Migration tests run against a copied pre-sync store.

After migration, every local mutation is saved with its entity in one SwiftData
transaction. A mutation contains:

- Stable mutation ID; the authenticated session supplies the device identity
- Entity ID and kind
- Typed operation and versioned payload
- Observed entity version
- Client event time

The local sync store separates the last durable canonical base from an ordered
pending-intent overlay. Feature reads render a materialized view produced by
folding the device's pending intents over that base. A server response updates
only a non-regressing `(recovery_epoch, entity_version)` base, resolves only the
receipt for the exact mutation ID it names, then reapplies every later pending
intent. Thus a delayed response for mutation M1 cannot erase the user's newer
local mutation M2. The durable family cursor advances only after journal pages
through the fixed high-water mark are committed locally; a cursor attached to a
mutation outcome is evidence for that outcome, not permission to skip unseen
journal entries.

`POST /v1/sync` sends a bounded ordered mutation batch and the last durable
family cursor. The server returns an individual outcome for each mutation:

- `applied`: canonical entity and cursor
- `already_applied`: idempotent replay result
- `superseded`: winning canonical entity
- `rejected`: stable machine-readable reason

The server stores the canonical mutation hash and outcome in
`mutation_receipts`, including stable permanent rejections. The response also
contains a paginated sequence of journal changes after the client cursor. The
first page fixes a high-water cursor; subsequent pages read only through that
value so continuous writes cannot starve catch-up. Remote changes use a
dedicated `BabyStore` apply path that does not append new outbox entries. The
client advances its cursor only after the whole response is durable locally.

Every sync response repeats the stable server instance ID and recovery epoch.
After a database restore, the operator increments the recovery epoch and the
service enters authorization quarantine while the external security ledger is
replayed. Clients refuse ordinary cursor advancement and reauthenticate only
after quarantine clears. They then replace the old canonical base with a full
server snapshot for the new epoch and reapply only mutations that never received
a stable server receipt. Already acknowledged state missing from the restored
snapshot is accepted as lost under the documented 24-hour RPO; devices never
re-upload it speculatively. Never-acknowledged local intent survives and is
resubmitted in original order after authorization and entity dependencies have
been revalidated. Unknown cross-replica order is therefore never guessed.
Session validation is bound to the recovery epoch, so rotating it logs out all
devices rather than reviving sessions or refresh tokens from the restored
snapshot.

The main app owns the long-lived WebSocket and normal sync loop. Widgets, App
Intents, and Live Activity controls use the same App Group outbox and may request
one bounded HTTPS sync after committing. A process-safe sync lease serializes
local cursor application; protocol idempotency remains the final protection if
a process dies or a lease expires.

### 7.2 Conflict policy

Ordinary edits use last server-accepted mutation wins. Device clocks never
establish precedence. A tombstone rejects later ordinary updates, preventing a
stale offline device from resurrecting deleted state.

`observed_entity_version` is diagnostic and lets the server report that a write
overwrote a newer version; it is not an optimistic-lock rejection under this
chosen policy. `superseded` is reserved for terminal tombstones, canonical timer
commands, or another domain rule that prevents the submitted state from
winning.

Structural operations such as membership, ownership, family deletion, and
entitlements use explicit commands and authorization rules rather than generic
last-write-wins updates.

### 7.3 Shared timers

Timer changes are commands rather than arbitrary entity replacement:

- Starting proposes a timer UUID and its current state. The server accepts it
  only when no timer of that baby/kind is active; otherwise it returns the
  existing canonical timer and the client visibly reconciles to it.
- Every timer outbox command has an explicit delivery state:
  `never_dispatched`, `in_flight_or_uncertain`, or `acknowledged`. Only a
  `never_dispatched` start may be rewritten to fold in a breast-side switch, and
  only a wholly `never_dispatched` start/stop lifecycle may be compacted. At the
  first network dispatch its mutation ID and canonical payload are frozen.
- If a start response is lost, later switch or stop intent may update the local
  overlay immediately but remains dependency-blocked. The client first retries
  the exact frozen start mutation until it receives the stable receipt and
  result. It emits the dependent command only when that receipt proves the
  proposed start itself was `applied` or `already_applied`. If the start lost a
  collision and the result names another caregiver's canonical timer, the
  dependent switch or stop is terminally superseded and the app surfaces the
  conflict; it never retargets the user's intent to that unrelated timer. It
  never rewrites an uncertain start or guesses whether it committed.
- Breast-side switches made while the start is still `never_dispatched` are
  folded into that proposed state. Later switches target the acknowledged timer
  UUID.
- Stopping an acknowledged timer supplies that timer UUID and a client-generated
  completed-record UUID. The server atomically tombstones exactly that timer and
  creates exactly that record.
- A timer started and stopped wholly offline while its start is still
  `never_dispatched`
  is compacted into a direct completed-record mutation. It does not stop or
  replace an unrelated canonical timer that another caregiver started.
- Idempotency makes concurrent and retried stop commands harmless.
- A stale stop for an older timer never stops a newer session. A losing offline
  timer is reconciled to server state and reported visibly; it never silently
  creates a duplicate record.

Deleting a child is one owner-authorized transaction. It tombstones the child,
all live dependent records, and every active timer; an active timer is cancelled
without synthesizing a completed record. The transaction appends a deterministic
ordered tombstone snapshot for every affected entity before commit so clients
can purge caches coherently. Any delayed child-dependent mutation or timer
command receives the stable terminal result `baby_deleted` and can never
recreate data below the tombstone.

### 7.4 Realtime delivery

`/v1/live` is an authenticated WebSocket. Messages contain no baby data: only
the family identifier and latest cursor. Reconnection uses bounded exponential
backoff and always follows with cursor sync. Route discovery is explicit:
`/v1/live` requires exactly `records-v1,live-hints-v1` plus the
`baby-tracker.v1` subprotocol, and servers advertise `live-hints-v1` only when
the route is active. Clients without it remain polling-only.

Before upgrade the server charges both IP and authenticated socket-attempt
limits, then reserves bounded Hub capacity with an idempotent release and binds
the reservation only after WebSocket accept succeeds. Each connection expires
at the earlier of the principal's `AccessExpiresAt` and 15 minutes after
acceptance. One five-second batched `LiveAuthorizer.CheckMany` sweep checks all
leases under one authority fence; one Hub-owned lossy invalidation subscription
is the fast path rather than one authority subscription per socket.

Hosted mode may send an APNs background wake containing no family payload.
Background delivery is an optimization, not a guarantee. Self-hosted mode has
no APNs relay: it receives WebSocket updates while active and uses launch,
foreground, and best-effort iOS background refresh otherwise.

The hosted delivery worker coalesces pending wakes per device and respects
Apple's background-notification throttling guidance. A wake never represents a
durable queue item; the family cursor remains the only source of truth.

Widgets and Live Activities continue to read the local App Group store and
refresh after remote changes are committed.

## 8. API and compatibility

Primary surfaces are:

- `/v1/auth/*`: challenge, Apple exchange, refresh, logout, and revocation
  notifications
- `/v1/account`: profile and account lifecycle
- `/v1/family/*`: membership, invitations, ownership, deletion, recovery
- `/v1/sync`: mutation results and cursor changes
- `/v1/live`: cursor notification WebSocket
- `/v1/export`: owner-only JSON export job and download
- `/healthz`, `/readyz`, `/metrics`: operational endpoints

Responses include a stable error code, request ID, safe human message, and
retryable flag. Client behavior is deterministic:

- Network errors, timeouts, `429`, and `5xx`: keep mutations pending and retry
  with jittered backoff.
- Expired access token: rotate once and retry.
- Revoked membership: remove credentials and cached family data.
- Permanent invalid mutation: quarantine it, continue other sync work, and show
  a Settings-level sync issue.
- Superseded mutation: apply canonical state and resolve the local mutation.
- Invalid or unavailable cursor: download an authenticated full snapshot.
- Server recovery-epoch change: stop incremental sync, reauthenticate after
  authorization quarantine clears, replace the canonical base from a full
  snapshot, and reapply only never-acknowledged pending intent.

Requests declare protocol version and capabilities. Additive fields are
backward compatible. Optional future resources can be ignored by older clients
and recovered from a later snapshot. Breaking behavior requires a new API
version. Contract fixtures validate Go and Swift against the same examples.
The schema-13 compatibility predecessor creates only durable delivery
schema/function and privilege state. It does not register `/v1/live`, advertise
`live-hints-v1`, or construct a Queue; those arrive together in the schema-13
feature composition.

## 9. Security and privacy

All non-development connections require HTTPS, including custom servers.
Custom TLS trust bypasses do not ship in release builds.

Every family-owned database row carries `family_id`. The Go service performs
authorization and PostgreSQL row-level security provides a second boundary.
Migrations run as a separate table-owning role. The runtime database role is
not a superuser, does not own tenant tables, and cannot bypass row policies.
Each tenant transaction sets its family/account context with transaction-local
database settings before any query; pooled connections never retain tenant
state between requests. Missing context produces default-deny behavior.
Cross-family purge and delivery workers use separate least-privilege database
roles that are unavailable to request handlers. Migration ownership, runtime
tenant access, and maintenance duties never share one database credential.

Invitation secrets use high-entropy random values and are stored only as
hashes. Links carry the secret in a fragment or app handoff payload so reverse
proxy access logs do not receive it. Authentication, invitation, mutation, and
export endpoints have bounded IP/account/device rate limits.

The following are prohibited from logs and metrics labels:

- Apple identity and authorization tokens
- Session and invitation secrets
- Baby and caregiver names
- Notes and record payloads
- Export contents

Structured logs use request IDs and opaque account/family/entity IDs. Operator
CLI actions create audit records. Administrative commands prefer opaque IDs and
do not provide bulk record browsing.

Hosted PostgreSQL volumes and backups are encrypted. TLS protects transport.
The service can read record content; this beta is not end-to-end encrypted. The
self-host guide assigns TLS, storage encryption, backups, accepted Apple
audience configuration, and application secret management to the operator. It
does not ask operators to possess the app developer's Apple or APNs private
keys.

The public beta launch also requires an accessible privacy policy, accurate App
Store privacy disclosures, an in-app account-deletion flow, and documented
processes for access, correction, export, and erasure requests. These are
release gates, not claims that the technical design alone establishes legal
compliance.

## 10. Retention, export, and revocation

Deleting a family disables all member access immediately and sets
`purge_after` to 30 days later. The owner may recover it during this period.
After the deadline, a worker removes family state, journal, audit events,
sessions, invitations, entitlements, and temporary exports. Deleted bytes leave
backups when the documented backup retention window expires.

Scheduling deletion replaces the owner's normal membership with a narrow
`family_recovery` claim recorded in the external security ledger. That claim can
only inspect deletion status, recover the same family before `purge_after`, or
permanently forfeit recovery; it cannot read family data or perform ordinary
mutations. The family continues to occupy the account's one-family beta slot
until recovery, forfeiture, or purge. Deleting the owner account explicitly and
irreversibly forfeits the claim. A user who wants to create or join another
family during the window must first confirm permanent forfeiture, so the
one-family invariant and recovery authorization cannot conflict.

When a caregiver leaves or is removed, the server invalidates sessions and
closes active connections. The app removes shared family data and credentials as
soon as it learns of revocation. An offline device purges at its next
authenticated contact and cannot obtain new changes.

Deleting a caregiver account removes its provider identity, sessions, profile,
and active membership. Existing family records keep an opaque actor ID for
integrity but display `Former caregiver`; they do not retain the deleted display
name. Owner account deletion is unavailable until ownership is transferred or
family deletion is confirmed. These account and family flows are distinct and
both are initiated inside the app.

An owner may request a versioned JSON archive generated from a consistent
database snapshot. Beta archives are held as encrypted temporary database data,
are downloadable only by the requesting owner, and expire within 24 hours.

## 11. Limits and abuse controls

Defaults are configurable by the server operator:

- 10 active caregivers per family
- 20 non-deleted children per family
- Bounded outstanding invitations
- Bounded sync batch size, page size, request body size, and WebSocket count
- Bounded text lengths, collection counts, timestamp ranges, and export size
- IP/account/device rate limits for authentication and mutation traffic

Hosted and self-hosted deployments share defaults; either operator may override
them. Limit errors use stable codes and never partially apply one mutation.

## 12. Deployment and operations

The repository gains a separable `Server/` Go module containing the API binary,
operator CLI, SQL migrations, OpenAPI contract, Dockerfile, and Compose example.

Hosted production begins with:

- One non-root multi-architecture Go container on a small European VM
- External TLS reverse proxy or load balancer
- Managed PostgreSQL in the same European region
- Encrypted immutable security-ledger and independent expected-head storage in
  the same European region, isolated from ordinary PostgreSQL snapshots
- Encrypted daily backups with an accepted recovery-point objective of up to 24
  hours
- Explicit migration command before application rollout
- Hosted-only APNs delivery worker and credentials
- No Redis or Kubernetes

PostgreSQL `LISTEN/NOTIFY` distributes non-durable cursor hints if multiple API
instances are introduced. Durable mutations and delivery work remain in tables,
so missed notifications and process restarts lose no data.

The self-host bundle runs API plus PostgreSQL through Docker Compose and
separate fsynced security-ledger and expected-head volumes, and documents
reverse-proxy setup, persistent volumes, coordinated database/ledger/head
backups, Apple configuration, and upgrades. Images target `amd64` and `arm64`.

Configuration comes from environment variables or mounted secret files and is
validated at startup. Production mode fails closed for missing Apple audience,
session-signing keys, database settings, proxy trust configuration, or unsafe
debug flags.

The stable server instance ID, monotonic recovery epoch, append-only security
ledger, and independent expected ledger head live in operator-managed persistent
storage outside ordinary database snapshots. Restoring a database preserves the
instance ID, requires an explicit epoch increment, forces authorization
quarantine, replays the ledger, and verifies its hash chain and expected head
before readiness can succeed.

SQL migrations use expand-and-contract changes so the previous and new server
versions can overlap during rollout. The application never auto-migrates on
startup. Container base images are pinned by digest, Go dependencies are locked
by `go.sum`, and release CI produces a software bill of materials and runs Go
vulnerability scanning. Third-party CI actions are pinned to immutable commit
SHAs.

Operational features are:

- Dependency-independent liveness
- Readiness covering PostgreSQL and migration compatibility
- Prometheus-format metrics without tenant labels
- Structured JSON logs and request IDs
- Graceful HTTP and WebSocket shutdown
- CLI commands for migrations, entitlement grants, suspensions, purge status,
  and ID-based inspection
- Backup-restore and purge-verification runbooks
- Recovery-epoch rotation and client-reconciliation runbook

## 13. Testing strategy

### 13.1 Server

- Domain tests for roles, limits, lifecycle, conflicts, and entitlement policy
- PostgreSQL integration tests for migrations, transactions, row-level
  security, cursors, exports, and purge
- Concurrent mutation tests proving idempotency and exactly-one timer completion
- Timer-race tests covering unacknowledged offline completion, start collisions,
  stale stops, side switches, and a newer canonical timer
- Mutation-ID reuse tests proving changed semantic content is rejected while
  equivalent JSON serialization remains idempotent
- Authentication challenge replay and same-audience cross-server relay tests
  proving origin/instance nonce domain separation
- Reverse-completion concurrency tests proving a later cursor never becomes a
  visible high-water mark before an earlier transaction commits
- Serialized concurrent-import tests proving one canonical baby, stable ID
  mapping, idempotent equal record UUIDs, and explicit unequal-content conflicts
- Child-deletion race tests proving atomic dependent tombstones and terminal
  rejection of delayed record and timer commands
- Security-ledger validation, pre/post-head-advance crash, valid-prefix
  truncation, competing-command serialization, projection-crash, and
  old-database-restore tests
  proving rejected commands never enter the ledger and committed events replay
  idempotently, and that
  member removal, owner transfer, account/family deletion, invite consumption,
  and session revocation cannot roll back
- Connection-pool tests proving tenant context never leaks between requests
- Cross-family denial tests through every repository and endpoint
- OpenAPI request/response conformance tests
- Race-enabled Go tests
- Container smoke tests for `amd64` and `arm64`
- Bounded load and WebSocket reconnect tests

### 13.2 iOS

- Deterministic outbox, retry, and quarantine state-machine tests
- Canonical-base plus pending-overlay tests in which an M1 response arrives
  after M2 was edited locally without regressing the materialized state or
  skipping journal cursors
- Remote application without echo mutations
- Import matching and interruption recovery
- Exact-match baby-ID remapping with dependent records and timers
- Migration of a copied pre-sync SwiftData store without replaying its legacy
  log
- Cursor durability, tombstone, and full-snapshot behavior
- Keychain session and custom-server configuration tests
- Hosted/custom/local realm isolation and endpoint-switch purging
- App, widget, intent, and Live Activity process-contention tests
- Membership-revocation cache purge
- Timer receipt-loss tests proving an uncertain start remains immutable and is
  resolved before a dependent switch or stop is dispatched, and that a
  collision never retargets those commands to another caregiver's timer
- Recovery-epoch tests proving a full server snapshot replaces only the old
  canonical base while never-acknowledged local intent survives in order
- Existing feature, widget, Live Activity, and UI regression suites

### 13.3 End to end

CI runs the production server container with real PostgreSQL and a test-only
identity issuer that production builds cannot enable. Scenarios use two clients
to create and join a family, import overlapping local state, synchronize every
record type, control one shared timer, reconcile offline edits, revoke a member,
export JSON, and delete/recover/purge a family.

Lifecycle scenarios also cover normal ownership transfer, caregiver account
deletion with historical pseudonymization, Apple credential revocation, and
manual recovery when the sole owner's identity becomes unavailable.

Fault scenarios duplicate requests, drop responses after commit, reorder
WebSocket hints, expire tokens, interrupt imports, restart the server, and make
PostgreSQL temporarily unavailable. Notification delivery is never required for
correctness.

A destructive test restores PostgreSQL to an older snapshot, rotates the
recovery epoch, and verifies that authorization remains quarantined until the
external security ledger restores every post-snapshot lifecycle decision. It
then verifies that clients replace acknowledged canonical state with the
restored snapshot, preserve and reapply only never-acknowledged local intent,
and never grant access to a revoked identity.

Before public beta, manual verification includes real Sign in with Apple, two
physical or simulated devices, a homelab container deployment, a restored
backup, and a privacy review of logs. Production readiness claims require fresh
deployment, migration, public health/readiness, database, and bounded-log
evidence rather than local-container success alone.

## 14. Cross-plan beta invariants

The following contracts are part of the beta definition. They override any
older illustrative interface in a plan and must be represented in OpenAPI,
shared fixtures, role manifests, client tests, and release evidence.

#### Authority, identity, and schema evolution

- Every migration records the exact current schema version and a complete
  runtime/projector privilege snapshot for that version. Snapshots cover
  schema, table, column, sequence, and function privileges, including explicit
  revocations. Readiness verifies migration digests, both role snapshots in one
  repeatable-read view, and then unchanged migration digests. A prepared
  predecessor binary must run at schema N-1 and N; overlap tests include a real
  privilege removal.
- HTTP-reachable services hold only the runtime role. Projector authority is
  confined to security projection/finalization and explicit worker/admin
  composition. Runtime access to identity/session/lifecycle data is through
  fixed-signature, fully qualified, PUBLIC-revoked `SECURITY DEFINER`
  functions; neither application role receives direct table DML.
- One `identity_session_issued` authority command atomically binds any account
  registration/restoration, device registration/reuse, and exactly one session
  grant. Missing erasable staging after an old-database restore can reconstruct
  opaque authority and `profile_required`/`material_missing` state only; it can
  never recreate a bearer token, continuity secret, display name, or Apple
  credential.
- Identity, continuity, session, invitation, notification, and other stable
  HMAC digests carry a bounded key ID. Readers accept the configured current
  key and bounded retained previous keys; writers use only the current key.
  Rate-limit key rotation may reset ephemeral buckets when documented.
- Identity unavailability permanently revokes every existing session lineage.
  Deleted-identity digests remain anti-rollback tombstones. A later explicit
  Apple proof may create a new actor but never revive the deleted actor,
  membership, device continuity, or session. Account deletion remains bounded
  even after unbounded historical refreshes by revoking aggregates
  deterministically rather than enumerating every session in an event.
- Session responses include the exact recovery epoch under which the
  credentials were minted. `/v1/server` declares
  `apple_authorization_mode` (`identity_token_only` or
  `code_exchange_required`), effective client-facing limits, active
  capabilities, source version, and an operator-configured Corresponding
  Source URL. Registry membership never implies an operation is implemented.

#### Family, protocol, and abuse boundaries

- Family and invitation identifiers are deterministic domain-separated UUIDv8
  values derived from actor and command IDs. Command-keyed presentation
  staging is immutable. Lost responses return stable metadata and never mint a
  replacement invitation token. Missing restored family presentation produces
  `profile_required` and an owner-confirmed repair flow.
- Family creation atomically establishes the family, owner membership, account
  slot, and hosted-beta entitlement where applicable. Owner export and
  destructive lifecycle/recovery operations remain available to the current
  owner when `family_sync` is suspended or expired; ordinary sync/import
  remains entitlement-gated.
- Operator ownership and entitlement commands bind the configured opaque
  operator UUID, operation-specific approved policy ID, reason code, keyed
  evidence digest, exact expected revision, and every affected value. Identical
  committed retries return the receipt even after confirmation expiry. Active
  owner recovery is distinct from recovery of a deletion-pending family.
- Capability requirements are route-specific. Each route defines advertised,
  required, and allowed tokens; JSON POST capabilities equal their headers.
  Known but inactive capabilities are rejected. Clients send only the endpoint
  set they negotiated, and UI/dispatch is gated by actual advertisement.
- The server resolves the authenticated account's one-family slot before
  cursor, page-token, import, or payload lookup. No untrusted value selects a
  tenant. Weighted rate admission charges all device/account/family scopes in
  one transaction or none. Denial creates no receipt, page, checkpoint, import,
  wake, or canonical state.
- Effective sync/import/snapshot bounds are discoverable. Pages use a durable
  key-ID-bearing signing key contract. Equal import content returns the
  entity's original authoritative cursor. Atomic child deletion is protected
  by an enforced canonical-record/storage quota and is measured near that
  maximum.
- Incremental page bytes mean the exact encoded changes array plus its sync
  envelope, excluding only unsplittable mutation outcomes; snapshot page bytes
  mean the complete encoded snapshot response. The beta defaults to 1 MiB per
  page (64 KiB–4 MiB configurable). Full snapshot materialization separately
  defaults to 250,000 live-or-tombstoned entities (1,000–2,000,000
  configurable), so accumulated tombstones cannot evade live-record caps.
  Acceptance requires limit−1/limit/limit+1 production-encoder checks for
  incremental and snapshot envelopes, a large unsplittable outcome proving it
  is the only excluded section, and a restart-backed tombstone-heavy snapshot
  that fails atomically at the entity cap and succeeds unchanged when raised.
- Active and former caregiver labels are available through a bounded,
  family-authorized, high-water/paginated projection. Deleted/removed actors
  resolve to `Former caregiver` without retaining their names.

Client action for stable denials is fixed:

| Server code/class | Client action |
| --- | --- |
| `membership_revoked`, family unavailable or recovery-only | Durably hide and purge the exact shared realm; activate local-only. |
| `access_expired` | Perform one fenced refresh and retry the exact request once. |
| `session_revoked`, `refresh_reused`, `reauthentication_required` | Preserve frozen intent; require fresh Apple proof and qualified continuity. |
| `epoch_changed` | Stop incremental work, reauthenticate, rekey credentials, and install a fixed snapshot. Clients may name the internal transition `recoveryEpochChanged`, but there is no second wire code. |
| `entitlement_required` | Pause family sync and surface entitlement state; never purge. |
| Quarantine, retryable `5xx`, database/network timeout | Retain state and retry with bounded backoff. |
| Permanent mutation rejection | Quarantine only the named frozen mutation. |

#### iOS persistence and lifecycle safety

- Invitation parsing is side-effect free and produces an unconfirmed origin
  and token. Endpoint probing, changed-instance confirmation, authentication,
  preview, and redemption are separate ordered steps.
- Keychain keys include canonical origin, server instance, account, device, and
  recovery epoch. Device continuity and the Apple credential-state user ID are
  separate, erasable, account-qualified bindings. App and widget use the same
  explicitly entitled access group, `kSecAttrSynchronizable=false`, and a
  this-device-only accessibility class suitable for the chosen background
  behavior. Protected/inaccessible Keychain is not treated as revocation.
- Epoch rekey, credential promotion, disconnect, revocation, account deletion,
  and family deletion use persisted idempotent checkpoints. Durable denial and
  hiding shared data happen first. Cleanup then stops transport, erases the
  exact secrets, purges the exact realm and realm-scoped selection, ends
  activities, and reloads widgets. A possibly committed destructive request is
  reconciled by its command ID; it is not blindly resent after credentials are
  erased.
- No shared realm is exposed until membership, frozen import mapping, local ID
  rewrite, canonical baby establishment, credentials, and recovery epoch agree.
  Cold launch reconciles checkpoints and Keychain before shared rows are
  queryable. Store-owned value/patch commands receive a write context captured
  when UI was rendered; a stale rejection leaves both in-memory and persisted
  values unchanged.
- Shared child deletion uses one reversible local cascade overlay for the
  child, dependent records, timers, selections, widgets, and activities, then
  reconciles the server's ordered tombstone cascade. Local-only deletion
  behavior remains explicit and separate.
- Every on-disk SwiftData open/migration across app, widget, and intent
  processes is protected by a bounded App-Group advisory bootstrap lock. The V1
  migration fixture is created by the actual predecessor binary/toolchain and
  validated before production metadata changes. Import source bases use
  `(entity kind, UUID)`, never UUID alone.
- Self-host background refresh has an exact registered BG task identifier,
  permitted Info entry, rescheduling and expiration cancellation. QR scanning
  declares camera use, handles denied/restricted states, and always offers
  manual paste. Widgets either receive the complete signed Keychain/BabySync
  composition or remain local-read-only; no half-wired network path ships.

#### Retention, operations, and release truthfulness

- Authority checkpoints are versioned typed snapshots that losslessly
  round-trip all authorization/idempotency decisions, including deleted
  identities, recovery claims, session lineage/reuse evidence, purge start and
  physical receipt, and receipt-compaction metadata. Restoration is bound to a
  verified signed checkpoint header and anchor head. Eligible receipts and
  expired lineages are pruned only under an explicit safe cutoff; live reuse
  and deletion anti-rollback evidence is never pruned.
- The retention worker uses a stable instance-bound UUID and a non-HTTP
  `retention` scope limited to purge start/complete. Checkpoint-format
  enablement is a monotonic CAS record beside the independent head/witness,
  bound to instance and minimum reader; it survives database restore and old
  readers fail closed.
- Purge cleanup uses a typed per-schema registry of exact definer functions.
  Later-schema family hooks run first, core family deletion next, and the
  physical receipt last in one transaction. A worker missing a hook for the
  current schema cannot claim or complete purge.
- Backup inventory types, local adapter, witness, and generation allocator are
  owned with retention/checkpointing. Checkpoint, watermark, and backup-manifest
  signing keys have explicit purpose-qualified trust registries and rotation
  rules. Abandoned generations require proof that they were never current and
  can never become current before deletion.
- Hosted readiness covers purge, temporary cleanup, inventory freshness,
  witness health, checkpoint headroom, retirement backlog, worker heartbeat,
  and encrypted-backup freshness. Self-host exposes the same dimensions as a
  documented degraded/operator-responsibility state. Admission stops before a
  checkpoint can exceed its bounded capacity.
- The checkpoint beta cap is 64 MiB with an 8 MiB emergency authority reserve.
  New family/member/invitation/device admission pauses at 56 MiB; only bounded
  refresh plus safety-reducing revocation, deletion, recovery, purge, and
  checkpoint commands may consume the reserve. Under the accepted load,
  checkpoint-caused exclusive-fence holds are p95 at most 250 ms and no hold
  may exceed two seconds; exceeding either threshold fails hosted readiness
  and the release load gate rather than silently relaxing the bound.
- Purge evidence inventories every configured database backup, ledger and
  checkpoint generation/version, restore artifact, and provider copy.
  Unreadable or incomplete inventory is blocked, not erased. Privacy text
  distinguishes immediate access disablement, day-30 live-store purge,
  24-hour operational export expiry, backup expiry, and Apple-revocation retry
  retention.
- Hosted APNs configuration uses purpose-separated team/key/topic/environment,
  ES256 key, token-idempotency, and collapse-ID secrets. Tokens are
  account/device scoped; removing one family does not delete a token still
  eligible elsewhere. Lifecycle invalidation wakes or cleans delivery and
  export work even without a new canonical mutation.
- Export live rows and downloads expire after 24 hours. This is not described
  as cryptographic erasure while a retained encrypted backup plus KEK can
  recover them. Request IDs have an explicit idempotency horizon, and route
  fixtures define status, capability, `Retry-After`, range/HEAD, and
  post-header stream-failure behavior.
- `/metrics` is served on a separately bound internal listener (or an equally
  explicit authenticated boundary), never trusted merely because of a header.
  Logs have bounded retention/access and include reverse-proxy and worker
  inspection while excluding IPs, secrets, names, notes, provider bodies, and
  archive contents.
- Backups are actually created daily and encrypted before durable storage, not
  merely hashed. Creation verification requires an exact coordinated head;
  restore verification accepts only a verified ancestor above the accepted
  erasure watermark while preserving the latest independent head/witness.
- Automated acceptance records the exact listener, image digest, source SHA,
  architecture, schema instance, restarts, and proof that clients reached that
  process. Load includes checkpoint publication, due purge, export, and APNs
  churn; commit latency is measured at the real committing process and remote
  convergence at durable client materialization. Two helper clients are not
  called two app installations: beta requires separate signed installations
  and real Apple/domain/provider evidence in addition to local tests.
- Every attempted peer-materialization cycle contributes to freshness
  percentiles. A missing/failed cycle is recorded at the fixed 30-second drain
  deadline, not omitted; the beta gate requires p95 at most two seconds and at
  least 99.9 percent successful cycles for the frozen 100-family workload.
- Release evidence uses a closed gate-ID/kind/status enum. Its artifact
  manifest is canonical and Ed25519-signed under a purpose-qualified external
  trust bundle; an embedded manifest key is never trusted. The verifier binds
  source SHA, build variant, OS, architecture and artifact digest, rejects
  unknown/expired/revoked or test-only signers in release mode, and detects any
  manifest, report, binary or image substitution.
- AGPL compliance includes an operator-configured network Corresponding Source
  offer. Release evidence verifies the public source URL against the deployed
  image/source version, retention health, encrypted backup restore, submodule
  and generated-contract cleanliness, and named-test execution.

## 15. Implementation slices

The complete beta remains one product outcome, delivered through independently
verifiable slices:

1. Protocol, Go service skeleton, PostgreSQL migration framework, and container
2. Apple identity, sessions, family membership, invitations, and entitlements
3. iOS Settings, endpoint selection, onboarding, and import
4. Typed record sync, cursor journal, conflicts, and full snapshot
5. Shared timers, WebSocket hints, and hosted APNs wake-ups
6. Export, deletion/recovery/purge, operations, hardening, and beta verification

The implementation plan must validate concrete library choices, flags, schema
syntax, and deployment commands against the complete branch and live end-to-end
behavior. This design does not treat illustrative names as pre-validated APIs.

## 16. References

- [Apple: Authenticating users with Sign in with Apple](https://developer.apple.com/documentation/signinwithapple/authenticating-users-with-sign-in-with-apple)
- [Apple: Verifying a user](https://developer.apple.com/documentation/signinwithapple/verifying-a-user)
- [Apple: Processing Sign in with Apple account changes](https://developer.apple.com/documentation/signinwithapple/processing-changes-for-sign-in-with-apple-accounts)
- [Apple: Offering account deletion in your app](https://developer.apple.com/support/offering-account-deletion-in-your-app)
- [Apple: App privacy disclosure](https://developer.apple.com/help/app-store-connect/manage-app-information/manage-app-privacy)
- [Apple: Pushing background updates](https://developer.apple.com/documentation/usernotifications/pushing-background-updates-to-your-app)
- [Apple: Supporting associated domains](https://developer.apple.com/documentation/Xcode/supporting-associated-domains)
- [European Commission: Individual data-protection rights](https://commission.europa.eu/law/law-topic/data-protection/reform/rights-citizens/how-my-personal-data-protected/how-should-my-consent-be-requested_en)
- [Go: Effective Go concurrency guidance](https://go.dev/doc/effective_go)
- [Go: Data race detector](https://go.dev/doc/articles/race_detector)
- [PostgreSQL: Row security policies](https://www.postgresql.org/docs/17/ddl-rowsecurity.html)
