# Security authority state and recovery

The security ledger and its independently persisted trusted head are the
authorization source of truth. PostgreSQL is a replayable projection, not a
backup of that authority. Stop the server before any recovery operation and do
not allow authorization traffic until verification and replay have completed.

## Restoring an older database

Keep the latest ledger and independent head when restoring PostgreSQL. Rotate
the recovery epoch explicitly, start in quarantine, replay the retained ledger,
and verify the server instance ID, generation, revision, and event hash against
the trusted head before restoring authorization. The epoch rotation invalidates
sessions from the database snapshot; eligible users must authenticate again.

Never restore the ledger and head from the same database-era snapshot and call
the result safe. A deliberate coordinated rollback, or total loss of both
security stores, is outside the detectable threat model. Total-store disaster
recovery requires the latest independently trusted manifest supplied by an
operator outside the restore set. Without independently established latest
revision and hash, the service remains quarantined. The manifest's provenance
is an operator trust assertion, not a cryptographic proof of freshness.

The beta permits one authority host. Its process mutex and shared/exclusive OS
file lock cover guarded reads, complete command commits, and projection. A
second authority host is unsupported even when the provider offers atomic head
CAS; multi-host operation requires a separately reviewed distributed fence.

## Self-hosted persistence

Mount the ledger and head on separate persistent directories. The local
adapter creates ledger events exclusively, fsyncs each event, atomically
replaces and fsyncs the head, and fsyncs the containing directories. The fence,
trusted recovery manifest, and actor-key commitment live with the head store.
Back up the two stores independently from PostgreSQL and retain their ordering.

Ledger events are bounded to 64 KiB of canonical JSON and contain opaque IDs,
enums, timestamps, keyed digests, lifecycle links, and command receipts. They
must not contain Apple subjects, names, notes, bearer tokens, provider refresh
credentials, or attachment data. The hosted adapter additionally bounds a
single materialized replay to 4,096 events and requires an explicitly sized
encrypted replay scratch area (48 bytes per replay index entry, between 48
bytes and 1 GiB).

## Hosted storage boundary

Hosted mode may not use the filesystem security adapter. Configuration names
the selected provider, European region and approved-region policy, independent
ledger and head failure domains, distinct ledger/head/vault key IDs, and the
bounded replay scratch mount. Startup still fails closed until a concrete
provider factory is compiled and registered.

The ledger object store must provide encrypted immutable create-if-absent
objects. The independent replicated head store must provide linearizable CAS.
The credential vault must provide authenticated encryption plus authoritative
conditional create, CAS, delete, and bounded metadata listing. Values reported
by an adapter are configuration assertions until verified against the selected
provider account and policy.

Rotate envelope keys by installing the new versioned key ID while retaining
every old key needed to decrypt live ledger, head, credential, or replay-marker
material. Do not remove an old key merely because it is no longer current.
Retire it only after the corresponding encrypted material has passed the finite
retention and erasure procedure. Plan 4 defines the independently witnessed
backup watermark, signed checkpoint, whole-generation retirement, and final
erasure gates.

## D11 provider acceptance

Current status:

- `interfaces_and_conformance_verified`: provider-neutral authority and
  credential-store interfaces and their deterministic conformance tests exist.
- `concrete_adapter_blocked_operator_values`: no concrete hosted provider is
  selected or registered because the required operator values are unavailable.
- `hosted_live_acceptance_not_run`: no real provider, Apple, backup/restore, or
  public hosted acceptance has been executed.

Before either hosted readiness gate can be enabled, the operator must supply
and approve all of the following without recording credential values here:

- provider and account identity, European region, and approved regions;
- immutable ledger retention/encryption policy and independent replicated head
  failure domain;
- ledger, head, and vault key IDs, key owners, rotation procedure, and old-key
  retention policy;
- authoritative conditional-create/CAS semantics and encrypted replay-scratch
  capacity;
- public domain, Apple audience, team, key and service identifiers, plus the
  mounted provider/Apple credentials;
- finite backup retention, oldest-restorable watermark, and checkpoint signer;
- the later AASA, APNs, application-signing, recovery-policy, and publication
  values required by the iOS and readiness plans.

With those values, review the concrete SDK version and immutable resources,
run the common authority-store and credential-vault conformance suites against
the real account, and prove encryption, immutability, CAS, failure-domain
separation, durable restart, key rotation, Apple older-token revocation, and
worker lifecycle behavior. Until that evidence exists, keep
`hosted_adapter_unavailable` and `hosted_credential_rotation_unverified`
active. Deterministic doubles are not live hosted acceptance.
