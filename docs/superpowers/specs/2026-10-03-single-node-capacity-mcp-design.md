# Single-node capacity and private read-only MCP

Status: approved by the user on 2026-10-03 ("good"); implementation has not started. Implementation plans: [capacity](../plans/2026-10-03-single-node-capacity.md) and [private MCP](../plans/2026-10-03-private-readonly-mcp.md), pending plan review and execution-method selection.

## Intent and confirmed scope

The user wants the sync server to serve as many clients as practical on the current Unraid node and expose family history and summaries to local desktop MCP clients over Tailscale. Success means measured sustainable capacity with correct, responsive phone sync, plus a usable private read-only MCP connection. Connected sockets, active devices, families and requests/second are separate measurements.

This work has two independently verifiable tracks: capacity measurement/optimization, then read-only MCP. Establish the sync baseline before adding MCP, then measure their combined workload. Each track gets its own implementation plan and review. Production configuration changes, load generation on Unraid and deployment require a concrete separate rollout decision.

## Current evidence

Source inspected: local main `1fe8ac4`; unrelated `TASKS.md` edits and `work/` already existed.

A read-only SSH inspection on 2026-10-03 found an Intel i5-12500, six physical cores/twelve logical CPUs, 31,873 MiB RAM and 12,611 MiB available at that instant. The host runs numerous other applications. Baby Tracker's app, PostgreSQL and Tailscale containers have no explicit Docker CPU/memory limits. The app image tag reports `1313f6262c07ba7e13363e37b087464ac5f1c8a9`; a tag is not independent verification of executing source. The one-shot Docker memory observations were approximately 32 MiB app, 60 MiB database and 105 MiB Tailscale. These are idle observations, not a capacity estimate or reserved headroom.

Verified source constraints:

- `Server/internal/security/authority_cache.go` and `localstore/integrity.go`: warm authority leases still read and hash the complete committed security history; cache verification is serialized by a process-wide mutex. Work grows with historical bytes. Its share of real request time remains unmeasured.
- `Server/internal/security/transaction.go`: an authorized database transaction retains the shared security fence through commit. Security mutations require exclusive access. Revocation ordering depends on this behavior.
- `Server/internal/sync/` and `records/repository.go`: family writes and some pagination/snapshot work serialize on the family row. One-family contention is different from many-family capacity.
- `Server/internal/database/pool.go` and `cmd/baby-tracker-server/main.go`: independent runtime, projector and optional maintenance pools lack an explicit aggregate connection budget in application configuration. The live listener reserves a runtime-pool connection.
- `Server/internal/protocol/sync.go`: default process socket cap is 1,000. Configuration allows up to 10,000; neither is a measured supported capacity. Realtime publication already targets a family's indexed sockets and coalesces hints.
- `docs/verification/2026-10-01-sync-performance-results.md`: the prior baseline/current matrix retained 54 incomplete cells and zero measurement operations. Shared-IP provisioning limits and setup pacing prevented steady measurement.

## Approaches considered

1. **Measure and optimize the existing Go/PostgreSQL composition — recommended.** Explicit resource/admission budgets, a valid repeatable workload and profile-driven improvements preserve the existing protocol and integrity guarantees. Add MCP in-process through the same authorization and family-data modules, with its own bounded admission.
2. **Redesign the authority storage first.** Checkpointing, compaction or storage-enforced immutability might reduce history-dependent work. It changes recovery and integrity assumptions and should be considered only if a valid profile shows that preserved-integrity optimization cannot meet the target. A TTL cache or background audit is not equivalent to the current corruption-detection guarantee.
3. **Separate MCP process and read projection.** Offers stronger process isolation but adds deployment, authorization, consistency and potentially duplicate-data costs. Reconsider if measured MCP interference cannot be bounded within the single process.

## Track A: capacity

### Workload and resource envelope

Use a provisional test budget of four logical CPU equivalents and 4 GiB total for app, database and proxy combined; test a smaller two-CPU/2-GiB profile as well. These are proposed laboratory budgets, not host reservations or deployment changes. Preserve headroom for the other Unraid applications. Tune the split from observations and document the exact cgroup, PostgreSQL, pool and socket configuration for each result. Local macOS results cannot establish capacity on the Unraid CPU/storage.

Vary independent dimensions: distinct families; idle live sockets; active sync clients; same-family contention; care history; authority history; reconnect/authentication bursts; and MCP read concurrency. Begin with 32, 128 and 512 connected devices, two devices per family where applicable, then increase only after the smaller cells pass. Explicitly distinguish a configured cap from a saturation point. Use old 248/512/2,048-event authority fixtures for comparison and add a larger aged-history fixture once provisioning is viable.

Separate fixture construction, warm-up, steady measurement and cleanup. Build valid synthetic accounts, credentials and authority histories in a disposable fixture environment using real authority transitions. A restored fixture must pass normal readiness, integrity, projection and identity checks. Never insert unauthenticated grants or weaken runtime quotas to manufacture a passing result. Account creation and refresh churn have their own measured workload. Provisioning refusals must not poison subsequent independent cells.

Use fresh matched fixture copies for baseline/candidate runs. Record input hashes, source revisions, tool versions, platform, container resources and actual fixture counts. Begin with a small pilot that proves the harness reaches measurement; expand only after it succeeds. Warm up for 30 seconds, measure each candidate cell for at least 60 seconds and require at least 200 successful observations for reported latency percentiles. Insufficient samples are labeled incomplete. Confirm the chosen supported envelope with a 15-minute soak. Every experiment has an explicit wall-clock deadline and owned-resource cleanup.

### Acceptance

- Healthy connected two-peer updates: p95 save-to-peer durable apply below one second. Observable-client latency remains a separate measurement using actual client composition.
- Zero lost acknowledged mutations, duplicate durable effects or cross-family data. Revocation, quarantine, rollback/corruption detection and retry receipts retain their contracts.
- At the supported load: fewer than 0.1% unexpected request errors; no admission refusals for the declared within-quota workload; stable memory, bounded queues and no growing delivery backlog.
- Beyond the envelope: bounded rejections/timeouts and recovery after the offered load drops, with no crash, leak or authorization bypass. Report all refusals rather than removing them from statistics.
- Publish the highest passing observed workload under its exact resource and history profile. Do not extrapolate that number into a universal clients-per-node claim.

### Implementation priorities

1. Repair benchmark provisioning and sample accounting, then establish a real unchanged-source baseline.
2. Add low-overhead bounded telemetry: request histograms, integrity bytes/time, cache-mutex/fence waits, all pool waits/occupancy, active sockets, queue depth, admission refusals and process/container resources. Measure observer overhead; never log tokens or care payloads.
3. Add explicit per-role connection budgets, reserve listener/maintenance capacity, validate aggregate ceilings against the declared PostgreSQL budget, and bound in-flight requests/queued work.
4. Profile duplicate readiness/authority work. Reuse only within a valid authorization lease with unchanged freshness guarantees; do not retain locks while reading a client body or writing a network response. Measure and shorten unnecessary allocations/file operations before considering a storage redesign.
5. Tune socket ceilings, database configuration and concurrency from passing measurements, then rerun the matched baseline/candidate cells and soak.

The final optimization choice is determined by profiles. This specification does not presume SQL, hashing, pooling or network I/O is the dominant bottleneck.

## Track B: private MCP

### Transport and module design

Expose an opt-in `/mcp` endpoint on the existing private HTTPS origin. Use the official Go MCP SDK with a pinned dependency version and compatible protocol negotiation. Prefer stateless Streamable HTTP with bounded JSON responses; no subscriptions or long-running tasks are required for the initial tools. The official specification's latest revision is currently 2026-07-28; exact compatibility must be tested against the selected SDK and actual desktop client, rather than assuming every desktop implements the latest protocol. See [official specification](https://modelcontextprotocol.io/specification/2026-07-28) and [official Go SDK](https://github.com/modelcontextprotocol/go-sdk).

The MCP adapter translates typed requests into a family-read module. That module owns authorization, query bounds, consistent pagination and deterministic aggregation. MCP never queries unrestricted database tables or accepts an arbitrary SQL/filter expression. Reuse existing canonical entities and tenant enforcement; do not drive a full export or repeatedly drain the sync journal to answer a small history question.

### Credentials and access

Propose a dedicated, revocable, family-bound read-only integration credential for desktop clients that support an authorization header. Tailscale provides network access; application credentials still establish family authority. Provision/list/revoke credentials through the authenticated operator workflow, initially bound to the active family owner's account, with a 90-day expiry. Renewal means explicit replacement and revocation of the old grant; there is no refresh token in this first version. Reveal the opaque secret once into a protected output, retain only its verifier, and keep it out of command arguments and logs. An app pairing UI is a later usability extension.

Integration grants must participate in the authoritative lifecycle: membership removal, account revocation, explicit grant revocation, expiry and recovery-epoch change deny subsequent requests. Extend the authority model deliberately; do not fabricate a phone session or let an MCP credential authenticate sync mutations, exports, invitations or operator commands. Every data call checks the credential, current membership and read scope through the authorized transaction seam. Tenant context comes from that decision, never a model-supplied family ID.

This is a deliberately limited private-client authentication profile, not a claim to implement MCP OAuth interoperability. Desktop clients requiring browser-based OAuth are outside the initial compatibility promise; they require a separate authorization-server integration. Document one working desktop setup and test its actual transport before declaring MCP usable.

### Initial tools

| Tool | Behavior and bounds |
| --- | --- |
| `list_babies` | List babies available in the authorized family, subject to existing family limits. |
| `get_history` | One baby; explicit start/end instants; at most 31 days per query; allowlisted kinds; default 50/max 100 entries; opaque continuation; at most 256 KiB encoded result. |
| `get_summary` | One baby; explicit range up to 31 days plus timezone; deterministic feed counts/known bottle volume, recorded sleep duration, diapers, supplements and measurements; bounded structured output. |

Summaries distinguish completed entries from running timers and absent data from a measured zero. Interpret intervals as `[start, end)`, clip completed sleep intervals to the requested range, and use a named IANA timezone for calendar-day grouping. Do not add a running timer's changing duration to completed totals. Preserve canonical feed/session grouping and units; do not count both a session and its segments as separate care events. Return the covered interval and data revision; summary evidence is retrievable through the corresponding paged history query.

Queries use deterministic ordering and principal/family/filter-bound continuation tokens. Concurrent edits must produce either a stable revision view or an explicit restart-required response, never silently skipped/duplicated history. Summary SQL and supported indexes require plan inspection with realistic synthetic histories. Apply deadlines and work limits; return an explicit limit error rather than partial totals presented as complete.

No server-side language model is needed. The desktop assistant explains the returned facts. Tool output contains care data, including any notes, as untrusted content. Access to a private endpoint does not imply that the desktop assistant's chosen model runs locally; document the selected client's data handling at setup.

### Resource isolation and acceptance

Start MCP with a maximum of four in-flight data calls process-wide and one per integration credential, a five-second total request deadline, a 32-KiB request-body limit and a 256-KiB response limit. Use account/family aggregate quotas so creating additional credentials cannot multiply allowances. Send `Cache-Control: no-store` and do not publicly cache personalized results. These are initial benchmark parameters to tune before rollout. Shared admission must reserve capacity for phone sync; a separate MCP semaphore alone does not prevent database starvation. Cancel underlying database work when a client disconnects or its deadline expires.

Verify protocol lifecycle/negotiation, tool listing/calls, malformed and oversized requests, bounded paging, summary parity, timezone/DST intervals, session grouping, credential expiry/revocation, cross-family denial, mutation-route denial and proxy Origin/Host validation. Run a real local desktop over Tailscale before claiming client compatibility. Measure mixed sync/MCP load: phone sync must retain its supported-load acceptance and p95 must not regress more than 10% against the matched sync-only workload. Lower MCP concurrency if it cannot meet that budget.

## Delivery and review

Current deliverables are this proposed design, current-spec research and tracker updates. No product code, dependencies, tests, benchmark or deployment were changed/run in this design pass. Read-only node inspection exposed no credentials or family records.

After design approval, write and review the two implementation plans. Verification must include focused tests, database-backed race tests, applicable server build/vet/protocol gates and final diff review. A later Unraid benchmark proposal must state exact disposable resources, CPU/RAM/I/O limits, duration, health stop conditions and cleanup before approval. Deployment follows verified local results and a separate backup/migration/rollback proposal.
