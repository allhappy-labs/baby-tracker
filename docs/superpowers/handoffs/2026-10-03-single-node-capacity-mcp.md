# Single-node capacity and private MCP handover

## Purpose

This handover records the in-progress work to make Baby Tracker's sync server
productive on one node and to add a private, read-only MCP interface for both
Claude Desktop and Codex desktop clients.

The work is in an isolated feature worktree and remains local. It has not been
merged, pushed, deployed, run against Unraid, or connected to a real desktop
client or production credential.

**Update 2026-10-04:** both tracks are complete locally; see the Current state
table. The sections below describe the state as of the original handover and
are kept for the record.

## Current state

| Item | State |
| --- | --- |
| Worktree | `<repo>/.worktrees/single-node-capacity-mcp` |
| Branch | `feat/single-node-capacity-mcp` |
| Original merge base | `5b80a71f798f885c763551e97dc068a7fe8d7f6c` |
| Capacity implementation HEAD | `ad4b4c43db2bc4885f0bdf416c35ee8dc16af9ef` |
| Handover document commit | `76a0341` |
| Track A capacity tasks 1–7 | Implemented and task-reviewed |
| Track A whole-branch review | Found five Important harness findings; all are fixed in `ad4b4c4`; scoped re-review passed on 2026-10-04 with three deferred Minor labelling notes |
| Track B private MCP tasks 1–8 | Implemented, task-reviewed and whole-branch reviewed (`5030100`..`7a856af`); local gates and the disposable end-to-end proof pass. Evidence: `docs/verification/2026-10-03-mcp.md`, `docs/verification/2026-10-03-mcp-mixed-load.md`; setup and rollout proposal: `Server/docs/mcp.md` |
| Still open | Merge/push, Unraid rollout, production grant, Tailscale Serve Host check, real Claude Desktop and ChatGPT desktop acceptance, and ingress/tailnet-ACL limits for abusive clients. All need separate approval |
| Main checkout | Preserve its unrelated dirty `TASKS.md` and `work/` changes |

The Track A ledger is
`.superpowers/sdd/2026-10-03-single-node-capacity/progress.md`. The Track B
ledger is `.superpowers/sdd/2026-10-03-private-readonly-mcp/progress.md`.
They are the recovery record; reconcile them with `git log` before dispatching
work. The controller owns both ledgers and reviews.

## What is implemented

### Capacity foundation

- A real, disposable fixture path creates 16 families and 32 devices through
  the normal authority, identity, family, and sync composition, then seals a
  logical PostgreSQL fixture with the required private authority artifacts.
- The capacity runner uses an untagged release server, a pinned TLS proxy, an
  owned PostgreSQL container, explicit resource limits, monotonic scheduling,
  exact cleanup, typed incomplete receipts, and no fabricated authority or
  quota bypass.
- Telemetry records bounded phase histograms, integrity bytes, actual pool
  state, admission state, and sampled process resources without weakening
  committed-history validation.
- Database pools are explicit when configured: runtime, projector,
  maintenance, MCP, PostgreSQL server maximum, and reserved connections.
  Empty configuration preserves legacy behavior; partial explicit
  configuration is rejected.
- Finite sync HTTP routes share a bounded admission controller. The current
  default reserves all capacity for sync: total 32, MCP 0. The later MCP task
  must enable its separate class deliberately and remain within the shared
  budget.

### Native sync shutdown repair

The capacity diagnostics exposed two real cancellation leaks in native
`URLSessionWebSocketTask` callbacks:

1. a pong wait could remain suspended after task cancellation when no callback
   arrived (`5f91a7d`), and
2. a native receive could remain suspended after close and cancellation,
   preventing the WebSocket runner from joining (`71e1434`).

Both now use a cancellation-aware, once-only callback bridge. Regression tests
cover cancellation before and during callback registration, withheld callbacks,
late/duplicate callbacks, and the production adapter plus real
`FamilySyncWebSocket.stop()` join path. A fresh source-attested 35-second
native lifecycle control completed cleanly after the receive fix. This proves
the lifecycle repair, not the 200-sample latency target.

### Final harness ownership and provenance fix

The broad Track A review found five Important flaws in the reusable capacity
harness. Commit `ad4b4c4` fixes all five:

1. Typed `server_busy` HTTP 503 admission refusals are now separately counted
   and always disqualify a cell, even below the ordinary error-rate threshold.
2. A 32-device, 16-writer, 15-writes/minute, 60-second cell is the first
   eligible anchor. Low-rate 60-second diagnostic cells remain visible but
   cannot authorize or block a later eligible anchor. 128-device execution
   requires a passed 32-device anchor; 512 requires a passed 128-device
   anchor.
3. SIGINT/SIGTERM owns the whole run, including pre-cell builds. It joins only
   the owned process group, runs normal cleanup, records remaining cells as
   interrupted/unmeasured, and never starts a later cell.
4. A failing two-writer contention probe cancels and joins both workers before
   result finalization or cleanup.
5. Future receipts independently attest the clean product checkout, clean
   harness checkout, relevant helper inputs, build recipe/toolchain, and
   actual client bytes. Drift invalidates the result rather than permitting a
   standalone reporter to recompute it as passing.

Historical receipts are unchanged. In particular, their historical 503 totals
cannot be retrospectively classified as typed global admission refusals; an
old receipt is marked `unavailable_legacy` and cannot satisfy the new passing
contract.

## Verification completed

Before the final harness fix, the following were successful at their recorded
source identities:

- full mandatory PostgreSQL/race server suite: 35 packages;
- multi-architecture server build, `go vet`, and server protocol E2E;
- runner and telemetry controls;
- a local linux/amd64 image build/load for a future Unraid proposal;
- 43 focused Swift tests covering the native callback repairs; and
- a source-attested 35-second native lifecycle control with two-family
  isolation and clean store shutdown.

The final harness-fix commit was verified with:

- 27 Python runner/metrics controls;
- focused Go capacity tests;
- focused `-race` admission/contention tests;
- scoped vet and controlled capacity-client build; and
- a post-commit clean harness attestation of 523 relevant inputs.

Exact commands, RED/GREEN evidence, compatibility behavior, and controlled
environment failures are in
`.superpowers/sdd/2026-10-03-single-node-capacity/final-fix-report.md`.

### Capacity result that must not be overstated

No local supported-client envelope has been established.

- The standard 32-device pilot recorded 163 acknowledged writes and 163 HTTP
  peer observations, with no HTTP errors, but fewer than the required 200
  samples.
- The small 32-device pilot recorded 49 acknowledgements, five observed HTTP
  peers, and 95 HTTP errors. It did not qualify.
- The full attested Swift proof reached 164 healthy rows and then hit the
  shutdown bug. It is preserved as incomplete with null percentiles.
- The repaired native control has only three lifecycle pairs. It is not a
  latency acceptance run.

Consequently, no 128/512-client expansion, shape sweep, 15-minute soak,
200-pair Swift latency acceptance, physical-device acceptance, Unraid load, or
production capacity claim is authorized by the current evidence.

Committed public verification artifacts are:

- `docs/verification/2026-10-03-single-node-capacity.md`
- `docs/verification/2026-10-03-single-node-capacity-final-evidence.json`
- `docs/verification/2026-10-03-unraid-capacity-runbook.md`

The detailed private receipts are under `/path/to/bt-capacity-task7-evidence`.
Do not delete them before review; the Task 7 reviewer checked their hashes and
the final broad review checked 167 recorded hashes.

## Pending Track A action

Run exactly one **scoped re-review** of `ae186edc..ad4b4c4` before treating
Track A as reviewed. It must verify all five final-review findings, plus any
new breakage in the fix range. It should use:

- requirements: `.superpowers/sdd/2026-10-03-single-node-capacity/final-fix-brief.md`
- implementation report: `.superpowers/sdd/2026-10-03-single-node-capacity/final-fix-report.md`
- original findings: `.superpowers/sdd/2026-10-03-single-node-capacity/final-review.md`

Do not rerun the full server suite, a capacity sweep, or native load merely to
perform this re-review unless the reviewer identifies a concrete unresolved
doubt. If clean, update the Track A ledger, then start MCP Task 1 from the
current HEAD.

The only deferred Track A minor is pre-existing Swift warning debt in
`CaregiversScreenCopyTests.swift:75` and `ReauthenticationWiringTests.swift:148`.
It is unrelated to this branch and must not be described as a warning-free
broad Swift build.

## Track B: private read-only MCP

### Product and security contract

MCP is private Tailscale HTTPS, disabled by default, and initially exposes
only:

- `list_babies`
- `get_history`
- `get_summary`

It has no mutation, admin, export, raw-SQL, subscription, public ingress,
OAuth-interoperability claim, or server-side LLM capability.

Each credential is a dedicated, 90-day, revocable family-read grant bound to
the owner account, family, server instance, and recovery epoch. Tailscale
connectivity alone grants no data access. Each MCP data call must recheck live
authority, membership, family eligibility, scope, expiry, and revocation under
the guarded read transaction. It must not fabricate a phone principal or
accept phone/refresh tokens.

The stored verifier format is lowercase hex of:

`SHA256("babytracker-mcp-v1\\x00" || grantID || secretBytes)`

The raw secret is 32 random bytes and is only emitted to a protected local
operator output file during the later issuance workflow.

### Settled issuance-time ruling

The confirmed proposal binds `ValiditySeconds = 7776000` (90 elapsed days),
not a prepare-time absolute expiry. The authoritative event, after write-fence
admission and exact-receipt lookup, captures `CreatedAt == Event.At` and
`ExpiresAt == CreatedAt + 7776000 seconds`. Exact retries return the existing
receipt before time-sensitive effect or grant-state checks, so they neither
extend expiry nor reactivate a revoked grant. A pre-CAS candidate is not
authoritative issuance.

This ruling supersedes the earlier absolute-expiry input suggestion in the
authority preflight. Details and required regression cases are in
`.superpowers/sdd/2026-10-03-private-readonly-mcp/time-preflight.md`.

### Transport and resource contract

- Streamable HTTP, stateless only; support exactly MCP `2026-07-28` and
  `2025-11-25` initially.
- Maximum request body: 32 KiB; full encoded tool-result response: 256 KiB;
  total request deadline: 5 seconds.
- Global MCP data calls: at most 4; at most 1 per credential; future Task 7
  must also enforce account/family aggregate quotas and a separate MCP pool of
  at most 4, within the shared admission budget.
- Each response must be `Cache-Control: no-store`; the final bounded writer,
  not only a middleware layer, enforces the serialized-response cap.
- History defaults to 50 entries, maximum 100, maximum 31 days / 744 elapsed
  hours. The same elapsed-hour bound applies to summaries; a named IANA
  timezone controls daily attribution but does not alter range validation.

The official SDK source was preflighted. Its legacy cancellation behavior
requires the adapter to bridge the original HTTP cancellation/deadline into
the SDK context. See `transport-preflight.md`.

### Family read semantics already decided

History and summaries use canonical effective session grouping. Feed roots and
linked feed components share `feeding_session_id`; sleeps group by
`sleep_session_id` without inventing a synthetic parent. Open stored sleeps
appear as unfinished history records but never contribute a running duration
to completed totals. Summary sleep seconds are a union of clipped positive
intervals per local day, so overlapping intervals do not double count.

History has separately typed current active timers and an `AsOf` timestamp.
It uses a no-lock canonical timer read helper. The current-baby limit is a
schema-bound present-state rule, not a historical range filter. Read
`familyread-preflight.md` and the updated Task 4/5 briefs before implementing
these pieces.

### Desktop clients

Both Claude Desktop and Codex desktop are target clients.

- Claude's cloud-origin remote connectors cannot reach a private Tailscale
  server. The intended path is a local, pinned stdio-to-HTTP bridge with a
  protected credential file. Do not expose public ingress.
- Codex uses native Streamable HTTP configuration with an authorization header
  helper or, where GUI environment handling is confirmed, a bearer-token
  environment variable.

The current machine has ChatGPT installed; neither Claude.app nor a standalone
Codex.app was found. Actual desktop acceptance and any persistent desktop
configuration remain pending. The detailed compatibility research is
`desktop-compatibility-research.md`.

## Next implementation sequence

After the scoped Track A re-review passes:

1. MCP Task 1: durable grant events, replay, authoritative state, SQL
   projection migration, manifest, and privilege tests. Recheck that migration
   28 is still free before creating it. Use the fixed-validity ruling above.
2. MCP Task 2: guarded read transaction backed by live authority and a
   dedicated pool.
3. MCP Task 3: operator prepare/commit/revoke/list command family, protected
   secret file, and lost-ack retry safety.
4. MCP Tasks 4–5: canonical history and deterministic summary service.
5. MCP Task 6: SDK HTTP adapter, exact protocol negotiation, deadline/body/
   response caps, error status behavior, and cancellation bridge.
6. MCP Task 7: composition, dedicated MCP pool/admission/quotas, and matched
   sync-only versus MCP-idle versus MCP-read workload evidence.
7. MCP Task 8: end-to-end proof, Claude bridge/Codex instructions, and a
   concrete rollout proposal.

Use one implementation agent at a time, then an independent task review. Keep
the current Track A harness source and fixture identities explicit in every
MCP mixed-load comparison. Regenerate/restore a fixture compatible with the
MCP migration and issue equivalent grants in all matched cells so authority
history is not a confounder.

## Approval boundaries

The user authorized local code, disposable tests, and local commits. The work
does **not** authorize:

- production grant issuance or credentials;
- Unraid configuration, deployment, or stress/load;
- a remote Tailscale change or public ingress;
- persistent Claude/Codex configuration;
- merge, push, or release.

Before any future Unraid experiment, refresh target-node facts read-only. The
last SSH attempt to `root@192.0.2.10` timed out at the banner exchange, so
there are no current host, storage, pressure, or path facts. The runbook is a
proposal only and must fail closed until those facts and a concrete experiment
approval exist.

## Useful references

- Approved design: `docs/superpowers/specs/2026-10-03-single-node-capacity-mcp-design.md`
- Capacity plan: `docs/superpowers/plans/2026-10-03-single-node-capacity.md`
- MCP plan: `docs/superpowers/plans/2026-10-03-private-readonly-mcp.md`
- Track A final review: `.superpowers/sdd/2026-10-03-single-node-capacity/final-review.md`
- Track A final fix report: `.superpowers/sdd/2026-10-03-single-node-capacity/final-fix-report.md`
- MCP authority preflight: `.superpowers/sdd/2026-10-03-private-readonly-mcp/authority-preflight.md`
- MCP family read preflight: `.superpowers/sdd/2026-10-03-private-readonly-mcp/familyread-preflight.md`
- MCP transport preflight: `.superpowers/sdd/2026-10-03-private-readonly-mcp/transport-preflight.md`
- MCP desktop research: `.superpowers/sdd/2026-10-03-private-readonly-mcp/desktop-compatibility-research.md`
