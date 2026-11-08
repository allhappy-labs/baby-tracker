# PostgreSQL authority and performance-first storage

Date: 2026-10-04
Status: approved in conversation; written spec awaiting review
Branch: `feat/postgres-authority` (from `feat/single-node-capacity-mcp` at `7154a82`)

## Purpose

Release Baby Tracker with a hosted sync service. One node must serve about
10,000 families (about 40,000 devices) with headroom, and every design choice
favours request-path performance. Self-hosting remains supported by running
the same server against its own PostgreSQL; there is no second storage path.

## Decisions taken

| Decision | Choice |
| --- | --- |
| Deployment target | Hosted multi-family service; self-host uses identical code |
| Scale target per node | About 10,000 families, 40,000 devices, 4,000 concurrent live connections; measured at 2x modelled load |
| Source of truth | PostgreSQL only. The file security ledger, file fences, in-memory replay and object-store backend are removed |
| Authority model | Relational tables written by transactional command handlers, plus per-scope hash-chained audit rows in the same transaction |
| Care-data model | Redesigned from scratch for performance; wire protocol unchanged |
| Existing installs | Migrated in place by one import script; devices stay signed in |
| Trust boundary | The database is trusted. Per-scope audit chains make tampering detectable; point-in-time backups are the recovery path. There is no independent file witness |
| Audit retention (amended 2026-10-16, owner decision D1) | Routine refresh rotation writes no audit row; every other security event keeps its row for at least 13 months (see "Audit chains") |

## Why

Measured evidence on the current branch (Track A and the MCP track):

- At 32 devices the 1-CPU app is pinned. The authority ledger is node-global:
  every request re-reads and re-hashes committed history, and a per-request
  readiness check replays the full chain, re-reads identity files, runs two
  schema-version queries and a full privilege-catalog comparison for two
  roles. Cost grows with every family on the node.
- Every security write anywhere on the node takes one exclusive file lock and
  one global sequence.
- PostgreSQL is a non-atomic projection of the ledger, with quarantine and
  reconcile machinery to cover the gap.
- A care-data mutation costs 15 to 20 sequential statements, writes child rows
  one at a time, duplicates the full entity JSON into an unbounded journal,
  fans out wake rows to every family device inside the write transaction, and
  bootstrap snapshots copy the whole family under the family lock. Journal,
  receipts, snapshot staging and page sessions have no retention.

## Architecture

### Storage

PostgreSQL is the only durable store. Session-token, identity and other
secret keys remain mounted secret files: they are configuration, not
authority.

The schema is new. Old table names, shapes and constraints are not
preserved. Design rules:

- Narrow hot tables; cold data (identity details, invitation metadata, audit
  history) lives elsewhere so it never bloats hot rows.
- Native `uuid`, small integer or enum types for kinds and statuses, never
  free text for closed sets.
- Every index is justified by a named request-path query.
- Fill factor below 100 on tables with frequent in-place updates.
- Partitioning only where it pays: hash by family for care data, time for
  audit history.

### Authority tables

The authority set covers accounts, account identities, devices, sessions,
families, memberships, family slots, invitations, entitlements, recovery
claims, MCP integrations, auth challenges and exchange material, and command
receipts. Exact columns are fixed in the implementation plan, subject to:

- `sessions` is keyed by access-token digest and carries, denormalised,
  exactly what authentication needs: account, device, family, role, recovery
  epoch, status and expiry. Request authentication is one primary-key lookup
  with no joins. Membership or role changes update the affected session rows
  in the same transaction.
- Node-wide rules (one active family per account, unique device binding,
  unique MCP verifier, one live timer per kind per baby) are unique
  constraints or partial unique indexes, not locks.
- `server_state` is a single row holding instance ID, recovery epoch and the
  actor-ID key commitment.

### Audit chains

Every security change appends to `security_audit_events` in the same
transaction. Each row belongs to one scope (a family or an account), carries a
per-scope sequence, the previous row's hash and its own hash over a canonical
encoding of the change. The scope's chain head lives on the family or account
row, which the command has already locked, so families never contend.

A background verifier walks chains incrementally from per-scope checkpoints;
the admin CLI runs a full verification on demand. A mismatch fails closed for
that scope only (its requests return 503) and logs an alert. The table is
partitioned by time.

**Amendment, owner decision D1 (2026-10-16): audit retention.** The owner
chose option (c) of the Phase 5 plan's decision D1, "a spec change: routine
refresh rotation writes no audit row; reuse detection, revocation and every
other security event keep theirs; the receipt and session rows remain the
record", "with 13 months for the remaining events". So:

- A routine refresh rotation (legacy or recoverable, and its
  lost-acknowledgement retry) appends no audit row and does not move the
  account's chain head; the child session is issued at the account's current
  head. Reuse detection (`session_revoked`), revocation, sign-in, role
  changes, removals, recovery and every other security event keep their rows.
- Audit history is kept for at least 13 months. A maintenance sweep drops whole
  partitions whose range ended more than 13 months ago (monthly from 2027; 2026
  is one yearly partition, kept until about February 2028; the default
  partition is never dropped, and its rows below an anchor are kept but no
  longer verified) (detach, then drop,
  under a 1 s lock timeout and the partition roll's advisory lock); it never
  drops the current or the next partition, and the database refuses to drop
  anything from the last 13 months by its own clock. Before it drops a
  partition it checks that every chain in it is not failed and that its rows
  up to the new anchor are one per seq and link (else it changes nothing and
  raises an alert), then records, per chain, a retention anchor: the last dropped row's
  seq and hash. The verifier walks a chain from its anchor: the first kept
  row must carry the anchor's hash as its `prev_hash` (so a tampered kept row
  still fails closed), and a chain whose rows all expired must end at the
  anchor.

### Roles

- Migration role: DDL only.
- Runtime role: exactly the DML each table needs, column-scoped where
  possible. The separate projector role and projection function are removed.
- Row-level security remains on every family-scoped table as defence in depth,
  with policies written so the planner keeps using indexes.

## Request path

### Readiness

A background monitor owns one cached readiness state. It pings the pool and
checks the schema version on a short interval. It runs the role-privilege
audit at startup, after migrations and every few minutes. Requests read the
cached state; when it is not ready they fail closed with 503. Nothing else
readiness-related runs per request.

### One transaction per request

For sync:

1. Read and validate the request body before opening a transaction.
2. Begin. In one pipelined round trip: set the tenant context and run the
   authentication lookup (HMAC of the access token, then the session primary
   key). Status, expiry, epoch and role are checked from that row.
3. Pull-only requests take no lock and read under the transaction snapshot.
4. Requests with mutations lock the family row once for the whole batch.
5. Apply the batch (see Write path), read the pull page, commit.

Snapshot pages, history, MCP reads and the live-connection handshake follow
the same shape. Revocation applies to every transaction that starts after the
revoking transaction commits; an in-flight transaction completes, as today.

### Rate limits

Phone-path rate limits are in-memory sharded token buckets. No request-path
rate limit writes to the database. Any limit that must survive a restart is
named in the plan with its reason.

### Database client discipline

- All request-path statements are prepared and cached per connection.
- Fixed statement sequences are batched into one round trip.
- One request pool, sized from measurement, plus one small background pool
  for readiness, verifiers, cleanup and wakes.
- No transaction stays open across network waits.

## Security writes

Each security command is one transaction:

1. Lock the involved account rows, then family rows, each in ID order.
2. Look up the command receipt. An exact retry returns the stored result
   before any other check; a different payload under the same command ID is
   refused.
3. Validate preconditions against current rows.
4. Write state, audit rows and the receipt.
5. Commit.

There is no global lock and no global sequence.

- **Refresh** (every device, about every 10 minutes) locks only the parent
  session row: consume the parent, insert the child and write the receipt. A
  routine rotation writes no audit row (amended by owner decision D1,
  2026-10-16, quoted under "Audit chains"); the refresh receipt and the
  session rows are its record. Reuse detection still appends its
  account-scope `session_revoked` row. A lost-acknowledgement retry returns
  the committed child.
- **Sign-in and identity** perform Apple verification and any other network
  calls before the transaction opens.
- **Family commands** (invitations, membership, roles, ownership, deletion
  and purge, entitlements) lock the family and the involved accounts.
- **Operator and MCP commands** keep prepare, confirm and commit. The
  commit timestamp is captured after the locks are acquired; MCP expiry is
  exactly 90 days from it; an exact retry never restamps or reactivates. The
  admin CLI talks to PostgreSQL only.

Old binaries fail closed: the schema compatibility version is bumped and
pre-migration binaries refuse to start.

## Care-data storage

### Entity table

All care records (babies, feeds, sleeps, diapers, supplements, growth
profiles, measurements, active timers) live in `care_entities`, primary key
`(family_id, id)`, hash-partitioned by family (initially 16 partitions).
Columns:

- typed fields for filtering and aggregation: kind, baby, version,
  `changed_seq`, start, end, session, tombstone time;
- derived fields computed at write time for summaries, for example bottle
  millilitres and breast seconds;
- the canonical wire entity, stored already encoded, served verbatim.

The per-mutation 8-table kind probe becomes one primary-key lookup. Child-row
tables for components are removed; components live in the stored entity and
its derived fields. Baby-name duplicate matching uses a 32-byte hash.

### Change sequence instead of a journal

There is no mutation journal. Each row carries `changed_seq`, the family
sequence of its latest change. A pull reads rows with `changed_seq` above the
device cursor, ordered by `changed_seq`, from one index. Phones receive the
same entity shapes and cursors but only each entity's latest state, not every
intermediate version. Tombstones remain rows, so old cursors still see every
delete.

### Snapshots without staging

A snapshot records the current family high-water mark and pages live rows in
key order with no lock and no copy. Changes made during paging receive a
higher sequence and arrive in the next pull. Snapshot page-token format,
TTLs and limits stay as they are; the snapshot staging tables are removed.

### Write path

Per sync batch, about four round trips regardless of batch size:

1. Fetch every existing entity the batch references in one query.
2. Validate in Go.
3. One pipelined batch: multi-row upsert, one sequence-range allocation,
   bulk receipt insert, one usage update on the already-locked family row.
4. Commit.

Validation stays in Go with database checks as a second line (7-day maximum,
zero-duration rejection, value bounds). Last-writer-wins by version and the
"overwrote newer" signal are unchanged. Quotas move onto the family row.

### Live updates and wakes

- The live hub stays in-process. After commit the server publishes "family
  changed up to N", coalesced per family over about 50 ms; devices pull.
- Revoking a session or removing a member closes that device's live
  connections after the commit.
- Each connection has a bounded memory budget, a write deadline and a cheap
  heartbeat.
- LISTEN/NOTIFY is removed (single node).
- Push wake intents leave the write transaction: a background worker compares
  each device's acknowledged cursor with the family sequence.

### Retention

- Mutation receipts: 30 days.
- Page and snapshot sessions: until their TTL.
- Expired auth challenges and exchange material: swept.
- Consumed sessions: pruned after the refresh-retry window.

A cleanup worker runs these sweeps; hot tables get tuned autovacuum settings.
Care entities and tombstones are kept.

## Behaviour changes visible to iOS

The wire format is identical, but two behaviours change and must be verified
against the shipped client before release:

1. Pulls deliver each entity's latest state, not every intermediate version.
2. A snapshot is not a frozen point in time; the device is consistent after it
   catches up with the following pull.

Live connections closing on revocation is new but matches existing
reconnect handling; it is verified in the same pass.

## Migration of existing installs

`baby-tracker-admin import-legacy` runs once with the server stopped, in one
transaction:

1. Verify the legacy file ledger chain end to end.
2. Replay it and cross-check the result against the legacy PostgreSQL
   projection; refuse on any mismatch.
3. Create the new schema.
4. Transform and load authority, audit history (each legacy event becomes an
   audit row in its scope with its legacy hash recorded), all care data and the
   server identity.
5. Drop the legacy tables.

It is all-or-nothing and refuses to run twice. Token digests and keys are
unchanged, so devices stay signed in and MCP credentials keep working. The
legacy verification and replay code survives only inside the importer's
package. A fresh install creates the new schema directly. Rollback restores
the required pre-upgrade backup of database and ledger files taken together.

## Verification

### Load

Model at 10,000 families: about 70 refreshes per second, 500 to 700 pulls per
second, under 10 care writes per second, 4,000 open live connections. The
harness drives 2x that.

- A SQL seeder loads 10,000 families with realistic history into the new
  schema in minutes.
- The capacity load generator is extended with a device mix: pull, write,
  refresh, idle live connection.
- CPU, memory and lock profiles are captured per run.

Pass at 2x load sustained for 30 minutes on a declared reference node (for
example app 4 CPU, PostgreSQL 4 CPU, recorded with results): sync and refresh
p95 at most 300 ms, under 0.1% unexpected errors, zero lost or cross-family
changes. The report states the measured ceiling. Thresholds are tightened
after the first measurement. Micro-optimisations (encoders, compression,
encodings) happen only when a profile shows they matter.

### Correctness

- Port existing server suites; keep security invariant tests one-for-one:
  revocation, cross-family isolation, lost-acknowledgement retry, issuance
  time.
- New property tests: a device that pulls in any order converges to server
  state; concurrent family and account commands never deadlock.
- Importer tests on real legacy fixtures with field-by-field verification.
- The iOS end-to-end suite (two-caregiver sync, owner reinstall, capacity
  matrix) runs against the new server.

## Phasing

One spec, separate implementation plans, each phase reviewed before the next:

1. Schema and authority: tables, command handlers, audit chains, request-path
   authentication.
2. Care-data storage and sync reads and writes.
3. Request path: readiness, in-memory limits, live hub, wakes, cleanup.
4. The import script.
5. Load harness and measured tuning.
6. Operator documentation, upgrade runbook and rollout proposal.

## Boundaries

- No merge, push, deployment or production import without explicit approval.
- `main` has moved since the MCP branch was cut (`7de9fa2` at the time of
  writing); reconcile before any merge.
- The user's Unraid upgrade is documented and proposed, not executed.

## Out of scope

- Wire-protocol changes.
- Multi-node operation, read replicas, external connection poolers.
- External signed checkpoints for the audit chains (a later option if an
  independent witness is needed).
- Attachments (still a stub).
