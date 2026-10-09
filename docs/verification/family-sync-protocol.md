# Family sync protocol verification

This report is the release ledger for the schema-13 live-hint and durable-wake
feature. The authoritative final evidence is produced from the committed
feature SHA; credentials, proofs, tokens, names, notes, request bodies, and raw
NOTIFY payloads are never recorded here.

## Required gates

- Focused socket, authorization, queue, import, rollback, PostgreSQL NOTIFY,
  reconnect, and bounded-resync race tests.
- Tagged six-file `identitytest` two-client protocol suite.
- Full server race suite and `go vet ./...`.
- Verified Go module checks and linux/amd64 plus linux/arm64 image build.
- Disposable smoke, black-box protocol container, full Swift fixture suite,
  OpenAPI/fixture/shell/diff checks, and prepared schema 12 to 13 overlap.

## Immutable overlap references

- Base: `bece1b7530a0f3febfef9ba0ee0f04f7b968a855`
- Prior schema feature: `c883a72a2012b9ba15777b70b4fc11227266e265`
- Verified implementation: `f6c8e1cca65468aa252b6df9aa15a3c6e45aea54`

This ledger update is a documentation-only follow-up to that verified
implementation commit. It does not claim that the documentation commit itself
was used to build a server image.

The prepared overlap must prove schema-12 and schema-13 import/sync behavior,
empty predecessor delivery state, a real feature WebSocket upgrade, mutation
and import cursor-only hints, durable schema-13 wakes, and role/digest denial.

## Design rulings

The accepted commit observer remains
`Publish(ctx, familyID, cursor)`: the explicit tenant coordinate prevents
observers from inferring family identity while publishing the exact two-field
wire hint. No APNs client, worker, device-token route, global pending scan, or
maintenance credential is part of the request process.

## Final repaired-SHA evidence

- Focused TDD reproduced the protocol-harness session bug: it stored an opaque
  access token as `SessionID`. The repaired signed-HTTP harness authenticates
  each issued access token through the real session authenticator and retains
  its actual session UUID, revision, scope, family, and expiry. The focused
  owned-PostgreSQL import, timer, and reauthentication group passed in 8.562s.
- `make server-protocol-e2e` built identity-test image
  `sha256:bc3c393ce507207fb3b8a7e715ff62926ed8f8750e9346bf5da49da59e927cbf`
  from the exact verified implementation SHA. Its arm64 server binary SHA-256
  was `6ccad098a56515c5b5eb0788e4ba8157bd0e425be35b3801063734416f235276`;
  schema 13 and authority `70000000-0000-4000-8000-000000000001:1` were
  observed. The black-box client and app both restarted; refresh reuse and
  cross-account continuity were denied; the dropped committed timer start
  replayed its byte-stable receipt; and the caregiver reconciled exactly one
  completed feed with the requested record ID, baby reference, and canonical
  payload. The database assertion was `1:true:1:left:600:0`.
- The same external run proved both equal-looking `preserve_local_ids` babies
  retained their original UUIDs, each imported dependent feed retained the
  correct baby reference through caregiver reconciliation, and the separate
  cross-caregiver unique-candidate import converged on one shared baby. The
  durable queue assertion was `1:true:9:7`; the realtime race package passed
  in 5.232s, the full tagged protocol race suite passed in 30.967s, and cleanup
  reported zero containers and zero volumes.
- The immutable prepared overlap
  `bece1b7530a0f3febfef9ba0ee0f04f7b968a855` to
  `c883a72a2012b9ba15777b70b4fc11227266e265` to
  `f6c8e1cca65468aa252b6df9aa15a3c6e45aea54` passed. Schema 12 imported over
  authenticated HTTP, the fixed snapshot drained four pages/four changes,
  exact import state survived migration, the immutable prior server passed at
  schema 13, and the candidate live/delivery and role-separation checks passed.
  Its disposable PostgreSQL storage was tmpfs and the container was absent
  after cleanup.
- At the verified implementation SHA, the protocol-client and script tests,
  `go vet ./...`, `go mod verify`, shell parsing, and `git diff --check` passed.
  Migration 13 remained byte-identical at SHA-256
  `1693627395740c2d4dc27a738b8b1d802343463646e12e6c7f1f6b0ebc19786c`.
