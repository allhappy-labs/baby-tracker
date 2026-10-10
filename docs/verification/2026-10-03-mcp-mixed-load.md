# Private MCP mixed-load: matched local evidence, 2026-10-04

**Paced, well-behaved MCP traffic passes. Abusive MCP traffic fails at every MCP concurrency we tested (4, 2 and 1).**

> **Update, 2026-10-04 (task 7c).** Repeat over-limit refusals are now refused before authentication. In a remeasurement through the same runner, one unpaced client **with a valid credential** (refused by its own gate or quota) now passes at MCP concurrency 4 and 1 (+1.8% and −3.3% write p95 against bracketing sync-only). Unpaced clients with invalid, revoked or expired credentials are not covered: they still pay full verification on every request and are unsupported (unmeasured). The four-credential flood still fails, at the shared TLS proxy rather than in the app. See [7c remeasurement](#7c-remeasurement-2026-10-04) and the updated [final supported limits](#final-supported-limits). The 7b results below are unchanged.

- **Planned load (7a invocation, rate 15).** Sync-only already fails the healthy-sync criterion: the app container is pinned at its 1-CPU quota and peer p95 is about 6 seconds. The rate-15 comparison is therefore **not authorized and inconclusive**. It is reported below, clearly labelled, only as a relative comparison.
- **Healthy baseline (rate 4).** 32 devices, 16 writers, 4 writes per writer per minute. All seven sync-only cells pass the harness's own sync criteria.
  - MCP idle is indistinguishable from sync-only.
  - Paced MCP reads (4 clients × 20 calls/min) stay within 10% p95 of their bracketing sync-only cells in all three read cells, at MCP concurrency 4 and 1. All 1,200 MCP calls were admitted. Run-to-run drift of sync-only p95 across the session is up to 11%, so the 10% bound sits close to the noise floor.
  - Abusive MCP fails, in two different ways:
    - A 4-credential × 8-worker flood breaks phone sync through the shared TLS proxy: 34–40% unexpected sync errors at MCP concurrency 4, 2 and 1.
    - One unpaced credential with 2 workers adds no sync errors but drives app CPU to about 100%: sync p95 rises 194% at concurrency 4 and 49% at concurrency 1.

Lowering MCP concurrency did not satisfy the criterion. The final supported limits are below. Nothing here is a node-capacity claim.

The [sanitized evidence JSON](2026-10-03-mcp-mixed-load-evidence.json) contains:
- every cell row and the comparisons;
- MCP outcome maps, pool and phase telemetry, and proxy error categories;
- identities, commands, the cleanup audit and hashes of the private receipts.

It contains no credentials, DSNs, family/account/baby identifiers or raw logs.

## Criteria verdicts

| Criterion (task 7 brief) | Rate 15 (7a planned) | Rate 4: idle / paced read | Rate 4: abusive |
| --- | --- | --- | --- |
| Healthy sync p95 < 1 s in sync-only | **Fail.** Peer p95 5.99–6.19 s, write p95 3.60–3.62 s; the comparison is not authorized | **Pass.** Peer p95 0.395–0.436 s in 7/7 sync-only cells; harness `passing=true` | n/a (baseline) |
| Sync p95 < 1 s with MCP | Fail (inherited) | **Pass.** Peer p95 ≤ 0.448 s | **Fail.** Flood: peer p95 0.81–1.65 s (fewer than 200 samples, ineligible) and incomplete. One busy client: 1.27 s at max 4, 0.67 s at max 1 |
| < 0.1% unexpected sync errors | Idle/read 0%; flood 46.7% | **Pass.** 0 of 640 in every cell | **Fail.** Flood 34.2–40.4%. Busy client 0% |
| Zero lost / cross-family changes | 0 detected in idle/read; 106 acknowledged writes with no peer observation in the flood | **Pass, within harness detection.** 0 unresolved, 0 peer mismatches, 0 identity/unknown-change errors, ACK = peer | **Inconclusive.** 7–17 acknowledged writes with no peer observation per flood cell (proxy 502 on the peer pull). That is missing evidence, not detected loss |
| ≤ 10% p95 regression vs matched sync-only | Relative only: idle −2.2%, read +7.8%, flood +19.3% (write p95, bracketed) | **Pass.** Idle +0.1% / −0.6%. Read at max 4: +7.5% / +4.2% and +4.4% / +2.1%. Read at max 1: +7.9% / +3.4% (write / peer, bracketed) | **Fail at every tested limit.** Flood at max 4/2/1: +280% / +142% / +103% write (ineligible: fewer than 200 samples). Busy client at max 4/1: +194% / +49% write |
| MCP admitted/refused recorded separately | Yes | Yes (tables below) | Yes (tables below) |

"Bracketed" compares an MCP cell with the mean of the sync-only cells run immediately before and after it. Comparisons against every single sync-only cell are also in the JSON. Compared with the single fastest sync-only cell of the session (S3), the paced read cells are +10.2%, +14.7% and +18.7% (write p95) and +4.7% to +13.5% (peer p95). That gap is mostly session drift: sync-only write p95 rose from 0.220–0.226 s in S1–S3 to 0.239–0.246 s in S4–S7.

## Method

**Runner.** Every cell ran through Track A's Python runner (`Server/scripts/sync-capacity.sh` → `capacity_control.py`). For each cell, the runner:
- attested the clean product and harness checkout;
- built the controlled identitytest client and the untagged release image;
- owned PostgreSQL, the app container and the nginx TLS proxy, exact-label cleanup and the process group;
- checked for input drift before and after each cell.

The runner had no MCP option, so commit `36cc6eb` (`test(capacity): run matched MCP mixed-load cells through the runner`) added one, narrowly scoped, with tests:
- a single-cell `--scenario matched` with a required `--measure-seconds` (60–900) and an optional `--rate`;
- MCP shapes forwarded through `--mcp-mode`, `--mcp-clients`, `--mcp-rate` and `--mcp-abuse-concurrency`;
- lowered MCP concurrency expressed as the named database profiles `explicit-mcp-1`..`-3`. The Go `databaseProfile` sets the dedicated MCP pool and `BT_ADMISSION_MCP_MAX` from the same number, and result provenance validates it. This replaces the two hard-coded 4s.
- mismatched mode/profile pairs, out-of-range windows and MCP options on other scenarios are refused before any build or dispatch.

Tests added: 4 runner tests, 1 Go profile/admission test. Existing pilot, sweep and swift behaviour is unchanged.

**No ad hoc client run.** No cell ran the Go client directly, so there is no deviation from the runner path.

**Fixture.** One sealed fixture serves every cell: 16 families, 2 devices and 1 baby per family, 100 diaper entries per baby, 248 authority events, and **1 MCP grant for every family** (16 grants). The grants were issued through the real operator path before the authority top-up, so authority history is identical whether or not a cell enables MCP.
- Sealed head 248; head 280 after the genuine refresh on restore.
- Manifest `InputSHA256` `376fd8f4bcb14c1905b68bd3340a515ab4a48deb10bb941e7f980d2ef90b6578`, source `36cc6eb`.

**Shared cell geometry.** Profile `standard`:
- containers: app 1 CPU / 512 MiB, PostgreSQL 2.5 CPU / 3 GiB, proxy 0.5 CPU / 512 MiB;
- diagnostics on;
- 32 devices with live sockets and 16 writers;
- 30 s warmup and a **300 s measured window**;
- explicit pools runtime/projector/maintenance 16/4/2, server/reserved 64/8, shared admission total 32.

Sync-only uses `explicit` (MCP pool 0, MCP disabled; the runner proves `/mcp` returns 404). MCP cells use `explicit-mcp` (MCP pool = MCP admission = 4) or a lowered `explicit-mcp-N`; the runner proves `/mcp` returns 401 without credentials.

**MCP cell shapes.**

| Shape | Credentials | Traffic |
| --- | --- | --- |
| idle | none used | none |
| read | 4 | 20 calls/min each, paced and staggered, cycling `list_babies`, `get_history` (24 h) and `get_summary` (7 days). 20/min stays under the 30/min integration quota, including the setup call |
| abusive flood | 4 | 8 unpaced workers each, 20 ms floor per worker (7a's invocation) |
| one busy client | 1 | 2 unpaced workers |

The busy-client cell is a diagnostic added during this task, using the same abusive mode at lower generator intensity. Only measured-window MCP outcomes are counted, and refusals, errors and timeouts are never retried or dropped.

**Order and noise control.** Cells ran back to back in the order below, all on the same image and fixture. No compiler or tests ran during measurement; other host activity was not controlled. Host one-minute load ranged 1.6–5.0 at cell boundaries (recorded per cell).

- **Rate 15:** S1, idle, read, flood, S2.
- **Rate 4:** S1, idle, read, flood, S2, flood@2, flood@1, S3, busy@4, S4, read (repeat), S5, busy@1, S6, read@1, S7.

Sync-only was repeated 7 times at rate 4 so drift and noise are measured, not assumed. The rate-4 choice was made after the rate-15 sync-only failure and before any rate-4 MCP cell ran. It gives 320 planned writes in 300 s, above the 200-sample eligibility floor.

One 60 s read smoke cell (`smoke-read-60s`) was run first to verify the new wiring. It is diagnostic only and excluded from every comparison.

### Commands (worktree root unless stated; branch `feat/single-node-capacity-mcp`)

```sh
cd Server && rtk proxy env GOENV=off GOWORK=off GOFLAGS= GOEXPERIMENT= GOTOOLCHAIN=local CGO_ENABLED=0 \
  go build -trimpath -tags identitytest -o /path/to/bt-mcp-task7b-evidence/fixture-client ./cmd/sync-capacity-client
cd Server && rtk proxy /path/to/bt-mcp-task7b-evidence/fixture-client fixture \
  --destination /path/to/bt-mcp-task7b-evidence/fixture --families 16 --devices-per-family 2 \
  --authority-events 248 --care-entries-per-baby 100 --mcp-grants-per-family 1
# each cell (via a private sequential driver; one runner invocation per cell):
rtk proxy bash Server/scripts/sync-capacity.sh --profile standard --scenario matched --metrics on \
  --measure-seconds 300 --rate <15|4> <cell options> \
  --source <repo>/.worktrees/single-node-capacity-mcp \
  --fixture /path/to/bt-mcp-task7b-evidence/fixture --output /path/to/bt-mcp-task7b-evidence/<cell> --total-seconds 900
```

| Cell | `<cell options>` |
| --- | --- |
| sync-only | `--database-profile explicit` |
| idle | `--database-profile explicit-mcp --mcp-mode idle` |
| read | `--database-profile explicit-mcp --mcp-mode read --mcp-clients 4 --mcp-rate 20` |
| read@1 | as read, with `explicit-mcp-1` |
| flood | `--database-profile explicit-mcp --mcp-mode abusive --mcp-clients 4 --mcp-abuse-concurrency 8` |
| flood@N | as flood, with `explicit-mcp-N` |
| busy@4 | `--database-profile explicit-mcp --mcp-mode abusive --mcp-clients 1 --mcp-abuse-concurrency 2` |
| busy@1 | as busy@4, with `explicit-mcp-1` |

### Identities (identical in all 22 cells)

| Identity | Value |
| --- | --- |
| Product and harness revision | `36cc6eb0055c325141dd62329ed766d6ce93c2fc` (clean; `provenance_verified=true` in every run) |
| Product input SHA256 (runner algorithm) | `0d2011a868f711a4d5de56e230af76e0adf68fb0ee5f31675fcdfc9e7584ef31` |
| Product binary SHA256 | `c03fef7ec1021dd3f6c0e7410d471ea4bf1e37436ae1528523fb24f7af0a02df` |
| arm64 app image | `sha256:88681d75a0e24f4d22b978eee8b4b373602ea16a4cf37a9d8cf4e871721340d3` |
| PostgreSQL / proxy images | `sha256:29e645e2…2d6a37` / `sha256:96868d9f…b03751` |
| Harness input SHA256 | `46e77ea7b228efc2f700bb790a08b8b27e07d6f80a26bad100f62402defc3a2c` |
| Capacity client SHA256 | `427aba3f3fba862b04383e64e55993a651f1c599d708e45cc6088e48ab01f2c9` |
| Fixture input / source input | `376fd8f4…f6b0b6578` / `31749ae3…ce565e92` |

## Results

Writes are listed as ACK / peer / planned. Latencies are nearest-rank values over all measured samples, in seconds. Δ is the p95 change against the bracketing sync-only mean. MCP outcomes are counted over the 300 s measured window.

### Rate 15: 7a planned invocation (sync-only unhealthy; relative comparison only)

| Cell | Writes | Unexpected sync HTTP | Write p50 / p95 | Peer p50 / p95 | Δ write / peer p95 | MCP attempts → admitted / refused / errors |
| --- | --- | --- | --- | --- | --- | --- |
| S1 | 839 / 839 / 1200 (350 skipped) | 0 / 1678 | 3.237 / 3.619 | 5.751 / 6.187 | | |
| idle | 877 / 877 / 1200 (313 skipped) | 0 / 1754 | 3.112 / 3.530 | 5.488 / 5.948 | −2.2% / −2.3% | 0 |
| read | 824 / 824 / 1200 (364 skipped) | 0 / 1648 | 3.281 / 3.891 | 5.803 / 6.352 | +7.8% / +4.3% | 400 → 400 / 0 / 0 |
| flood | 446 / 340 / 1200 (165 skipped) | **689 / 1475** | 3.683 / 4.306 | 6.717 / 7.286 | +19.3% / +19.7% | 172,028 → 600 / 125,355 / 46,073 |
| S2 | 864 / 864 / 1200 (324 skipped) | 0 / 1728 | 3.169 / 3.600 | 5.571 / 5.991 | | |

At rate 15 the app container averages 99.7–99.9% of its single CPU. About 30% of slots are skipped, because a writer's previous operation was still running when its next slot arrived. Latency is therefore capped by the 4 s schedule, and MCP cost shows up partly as fewer acknowledged writes. A healthy-load comparison cannot be made at this rate. This matches Track A, which found no supported local envelope at rate 15.

### Rate 4: healthy baseline

| Cell (run order) | Writes | Unexpected sync HTTP | Write p50 / p95 | Peer p50 / p95 | Δ write / peer p95 | MCP attempts → admitted / refused / errors | App CPU mean (whole cell) |
| --- | --- | --- | --- | --- | --- | --- | --- |
| S1 | 320 / 320 / 320 | 0 / 640 | 0.211 / 0.226 | 0.378 / 0.396 | | | 43.1% |
| idle | 320 / 320 / 320 | 0 / 640 | 0.209 / 0.226 | 0.377 / 0.395 | +0.1% / −0.6% | 0 | 42.9% |
| read (max 4) | 320 / 320 / 320 | 0 / 640 | 0.213 / 0.243 | 0.383 / 0.414 | +7.5% / +4.2% | 400 → 400 / 0 / 0 | 45.9% |
| flood (max 4) | **148 / 131 / 320** | **189 / 468** | 0.696 / 0.858† | 1.409 / 1.648† | +280%† / +315%† | 187,685 → 600 / 124,575 / 62,510 | 54.4% (proxy 42.7%) |
| S2 | 320 / 320 / 320 | 0 / 640 | 0.204 / 0.226 | 0.369 / 0.398 | | | 41.4% |
| flood (max 2) | **154 / 141 / 320** | **179 / 474** | 0.465 / 0.539† | 0.868 / 1.033† | +142%† / +161%† | 180,342 → 600 / 124,951 / 54,791 | 51.5% (proxy 44.7%) |
| flood (max 1) | **162 / 155 / 320** | **165 / 482** (+2 warmup) | 0.349 / 0.452† | 0.682 / 0.805† | +103%† / +103%† | 168,931 → 600 / 126,324 / 42,007 | 46.1% (proxy 45.5%) |
| S3 | 320 / 320 / 320 | 0 / 640 | 0.187 / 0.220 | 0.357 / 0.395 | | | 40.9% |
| busy client (max 4) | 320 / 320 / 320 | 0 / 640 | 0.452 / **0.676** | 0.849 / **1.267** | +194% / +207% | 21,518 → 150 / 21,368 / 0 | **99.7%** |
| S4 | 320 / 320 / 320 | 0 / 640 | 0.217 / 0.240 | 0.390 / 0.429 | | | 44.8% |
| read (max 4, repeat) | 320 / 320 / 320 | 0 / 640 | 0.221 / 0.253 | 0.397 / 0.440 | +4.4% / +2.1% | 400 → 400 / 0 / 0 | 48.1% |
| S5 | 320 / 320 / 320 | 0 / 640 | 0.218 / 0.245 | 0.395 / 0.433 | | | 45.9% |
| busy client (max 1) | 320 / 320 / 320 | 0 / 640 | 0.290 / 0.359 | 0.575 / 0.669 | +49% / +55% | 27,002 → 150 / 26,852 / 0 | 81.3% |
| S6 | 320 / 320 / 320 | 0 / 640 | 0.217 / 0.239 | 0.389 / 0.431 | | | 45.0% |
| read (max 1) | 320 / 320 / 320 | 0 / 640 | 0.225 / 0.262 | 0.407 / 0.448 | +7.9% / +3.4% | 400 → 400 / 0 / 0 | 48.5% |
| S7 | 320 / 320 / 320 | 0 / 640 | 0.221 / 0.246 | 0.397 / 0.436 | | | 44.9% |

† Percentile from fewer than 200 samples (write: acknowledged writes; peer: peer observations). It is **ineligible** under Track A's 200-sample rule and is reported for the record only, not as a measured p95.

Sync-only and the harness's own sync checks:
- **Healthy cells.** Every sync-only, idle and read cell is `complete` with harness `passing=true`, along with busy@1, which passes sync checks but fails the 10% regression bound. Each has 0 skipped and 0 deadline-unfired writes, 32/32 live sockets, 0 unexpected socket losses, 0 setup/warmup errors and 0 refusals.
- **Flood cells.** These are `sample_incomplete` and `passing=false`: fewer than 200 acknowledged writes, and 34–40% unexpected errors.
- **Busy client at max 4.** `complete` but `passing=false`, because peer p95 is 1.27 s.

**Noise.** Sync-only p95 across S1–S7:
- write p95: 0.2204–0.2462 s, a spread of 11.0% of the mean;
- peer p95: 0.3950–0.4364 s, a spread of 9.9%.

Bracketing pairs were close (S1/S2: 0.1% write; S4/S5: 2.1%; S6/S7: 3.1%). Most of the spread is a step between S3 and S4 that coincides with nothing in the cell configuration. At rate 15, S1/S2 differed by 0.5% (write) and 3.2% (peer).

### MCP admitted / refused / timeout rates (measured window, per minute)

| Cell | Admitted/min | Refused/min (rate / concurrency / server_busy) | Errors/min | Timeouts | Admitted p50 / p95 / max |
| --- | --- | --- | --- | --- | --- |
| r15 read | 80 | 0 | 0 | 0 | 0.562 / 0.860 / 1.335 s |
| r15 flood | 120 | 25,071 (1,398 / 430 / 123,527 total) | 9,215 (44,791 `other` = proxy non-JSON 5xx; 1,282 transport) | 0 | 0.288 / 0.872 / 1.625 s |
| r4 read (max 4, ×2) | 80, 80 | 0 | 0 | 0 | 0.028 / 0.046 s; 0.032 / 0.047 s |
| r4 read (max 1) | 80 | 0 (no `server_busy` in 400 calls) | 0 | 0 | 0.030 / 0.046 / 0.056 s |
| r4 flood max 4 / 2 / 1 | 120 each | 24,915 / 24,990 / 25,265 | 12,502 / 10,958 / 8,401 (proxy `other`) | 0 | p95 0.273 / 0.124 / 0.111 s |
| r4 busy max 4 | 30 | 4,274 (19,238 rate / 2,130 concurrency) | 0 | 0 | 0.050 / 0.089 s |
| r4 busy max 1 | 30 | 5,370 (13,865 rate / 12,987 server_busy) | 0 | 0 | 0.024 / 0.062 s |

What the rates show:
- **Quotas hold exactly.** Each abused credential was admitted 30 calls/min (120/min for 4 credentials, 30/min for 1).
- **No MCP timeouts.** None was observed in any cell, including the writer-pending fence gate (no security write was pending during these windows).
- **No errors in paced reads.** Paced reads saw zero refusals or errors at both limits.

### MCP pool and phase telemetry

**Paced reads.** At rate 4 with max 4, the app samples show:
- MCP pool: 441 acquires (400 measured, plus setup and warmup); 1 connection opened; 0 acquired at any 5 s sample; 3.98 ms cumulative wait.
- MCP admission: 441 accepted, 0 rejected.
- `mcp_capacity` phases: authority 882 captures (2 per request, 10.09 s), database 441 (0.86 s), integrity verification 441 (8.83 s, about 20 ms each), cache wait 0.80 s.
- MCP integrity bytes: 315 MB, about 0.71 MB of verified envelope per request.
- At max 1 these are almost the same (pool max 1; integrity 9.03 s).

**Rate 15.** Under app CPU saturation, MCP authority time rises to 216.8 s and cache wait to 183.5 s for 444 requests, so MCP calls queue behind sync for the shared cache and CPU.

**Busy client.** The `mcp` admission counter shows:
- at max 4: 23,674 requests passed global admission, and each was authenticated (`integrity_verify` 23,672 captures, 235.5 s);
- at max 1: only 15,294 passed (14,188 cheap pre-authentication `server_busy`; integrity 136.5 s).

This is the mechanism. **A refused request is not cheap once it passes global admission.** Credential gate and quota refusals happen after `CheckIntegration`, which costs about 10–20 ms of CPU each. About 70–90 refused requests per second from a single credential consume the whole 1-CPU app. Sync phases and integrity bytes stay in the separate sync-only `capacity` snapshot and are never summed with MCP.

**Flood cells.** These cells have few or no app samples. The app logs one `request completed` line per request, and the harness keeps only the last 1,500 log lines, so the five-second capacity samples are pushed out. Flood@4/2/1 have 0 samples; r15 flood has one cumulative sample (MCP admission 2,750 accepted / 148,309 rejected). This is a telemetry gap, not hidden data. External Docker CPU for those cells is complete.

**Flood mechanism.** A read-only watcher categorized the owned nginx error log, recording message category only. It found 60,975 / 52,656 / 40,071 lines in flood@4/2/1 of the same kind:

```
crit: connect() to ADDR failed (99: Address not available) while connecting to upstream
```

There were none in any idle, read or busy-client cell. Roughly 560–630 requests per second, each on a new upstream connection, exhaust the proxy's ephemeral ports to the app. nginx then answers MCP **and phones** with a non-JSON error status. The harness records these as `http_other` for sync and `other` for MCP; that the status is a 502 is inferred from the upstream-connect failures, not recorded per request. The proxy sits at its 0.5 CPU cap, while the app alternates between 100% and idle.

In-app MCP admission refuses most flood requests in under a millisecond (`server_busy`). It cannot protect a resource that sits in front of it, which is why lowering MCP concurrency to 2 or 1 changed little.

The load generator shares one Go HTTP transport between phones and MCP clients, with Go's default of 2 idle connections per host. The laboratory nginx has no upstream keepalive. Both amplify connection churn, so the exact flood threshold is harness-specific. That an unpaced tailnet client can break phone sync through the shared ingress is a **risk**, not a production finding: it was shown only through the laboratory nginx, and Tailscale Serve was never tested.

## Lowered MCP concurrency

The brief requires lowering MCP concurrency when a criterion fails. MCP pool and MCP admission were lowered together through `explicit-mcp-N`.

- **Flood.** Max 2 and max 1 still fail. Write p95 regression drops from +280% to +142% and +103% (all three from fewer than 200 acknowledged writes, so ineligible under Track A's 200-sample rule), but unexpected sync errors stay at 34–38%.
- **Busy client.** Max 1 cuts the regression from +194% to +49% (write) and brings peer p95 under 1 s. It is still far above the 10% bound.
- **Paced reads at max 1.** All calls were admitted with no `server_busy`, within the 10% bound (+7.9% / +3.4%).

No tested limit meets the abusive-MCP criterion. Both runs are documented above; neither metric was improved by dropping refused or failed requests.

## Final supported limits

These are measured locally only. They are not a node capacity.

**Updated by 7c (2026-10-04).** These supersede the 7b limits, which are kept below for the record.

**Supported**
- MCP enabled with `BT_ADMISSION_MCP_MAX` = `BT_DB_MCP_MAX_CONNS` = **1 or 4** (measured). Both pass every paced-read and single-busy-client cell, with no measurable difference between them. 2 and 3 were not measured in 7c (7b measured 2 only in a flood cell) and are expected, not shown, to behave the same. 1 remains the conservative choice, because it serializes MCP authority loads.
- Default quotas (30 / 60 / 120 per minute) unchanged.
- At most 4 well-behaved MCP clients at ≤ 20 calls/min each (80 admitted/min), alongside 32 devices at 4 writes per writer per minute, on the standard profile (1-CPU app).
- **One unpaced or retrying MCP client with a valid credential:** one valid, unrevoked, unexpired credential with 2 concurrent unpaced workers, about 93 requests/s offered and 30 admitted/min. Phone sync stays within the criteria at concurrency 4 and 1. This holds only because the client is refused by **its own** credential gate or quota (integration, account or family) after authenticating: those refusals are repeated from the refusal cache, so about 1% of its requests reach `CheckIntegration`.
- All of this was measured at the reduced rate-4 phone load (32 devices, 4 writes per writer per minute), not the Track A rate-15 anchor.

**Not supported**
- **Unpaced clients with invalid, revoked or expired credentials, and floods of well-formed garbage tokens.** A well-formed token pays the full authority verification (fence, ledger inventory check, then grant lookup) before it can be rejected, and a 401 is never cached. By code path and by 7b's busy-client data, such a client is expected to behave like the 7b busy client: +194% write p95 at concurrency 4 and +49% at 1. This is **unmeasured** in 7c. The garbage-token case is pre-existing and applies to any tailnet peer that can reach the app.
- **A connection flood:** 4 credentials × 8 unpaced workers, about 600 new TLS connections/s. It still breaks phone sync, with 31–32% unexpected errors and multi-second write p95 (flood@1's write p95 and both cells' peer p95 come from fewer than 200 samples and are ineligible). The failure is now at the shared TLS proxy, not in the app:
  - the proxy sits at its 0.5-CPU cap and runs out of upstream ephemeral ports (55–57k `connect() … (99: Address not available)` lines per cell);
  - the app averages about 34% CPU.

  No in-app limit can fix this.

**Deployment requirement.** Production ingress is Tailscale Serve, which has no connection or rate limit. Protecting phone sync from a flooding tailnet client needs a connection/rate limit in front of the app, such as a limiting proxy between Tailscale Serve and the app, sized per client. This is a documented deployment requirement; it is not implemented or tested here. Without it, a flooding or unpaced MCP client is an incident, and **revoking its credential does not reduce the load**. A revoked credential keeps receiving cached 429s for at most the rest of its one-minute quota window (up to about a second more because `Retry-After` is rounded up). After that, every request takes the expensive path to a 401, which is the unprotected case above. The client must be stopped at the ingress or by tailnet ACL/policy. Revoke as well, so its data access ends.

**Recommended follow-up (not implemented).** Cheaply reject invalid credentials before full authority validation, so that revoked, expired, unknown and garbage tokens cannot force the per-request ledger verification.

### 7b limits (superseded, kept for the record)

**Supported**
- MCP enabled with `BT_ADMISSION_MCP_MAX` = `BT_DB_MCP_MAX_CONNS` = **1** (recommended). 4 also passed paced reads, but 1 limits the damage from a busy client by about three quarters at no measured cost to paced reads.
- Default quotas (30 / 60 / 120 per minute) unchanged.
- At most 4 well-behaved MCP clients at ≤ 20 calls/min each (80 admitted/min), alongside 32 devices at 4 writes per writer per minute, on the standard profile (1-CPU app).

**Not supported**
- Unpaced, retrying or abusive MCP clients. Isolation of phone sync from them is **not achieved** at any tested limit (4, 2, 1). This criterion of the task 7 brief remains failed.

**Required follow-ups.** These are outside this measurement task and are not implemented:
- make refusals cheap before `CheckIntegration` (for example a pre-authentication per-token/source throttle, or bounded caching of repeat refusals);
- connection and rate limits at the tailnet ingress (Tailscale Serve or a proxy in front of the app);
- reconsider per-request info logging at high refusal rates.

Until then, an operator must treat an abusive MCP credential as an incident and revoke it. Revocation stays bounded under read load (Task 7a).

## Limitations

- **Hardware.** A local arm64 macOS laptop (OrbStack, 10 CPUs) is not the Unraid node. The resource ceilings are laboratory budgets, and the load generator runs outside them.
- **No real clients.** No physical devices, no SwiftUI or Swift store apply, no desktop MCP client (Codex or the Python bridge), and no Tailscale Serve ingress.
- **Peer semantics.** "Peer" means another device decoded the change over HTTP. It is not durable or observable apply.
- **Loss detection is limited.** The harness proves that each acknowledged write is seen exactly once by its family peer, and that every sync response has the right family high-water mark. It does not scan every change for family, and it does no post-run database reconciliation. Writes acknowledged during the flood without a peer observation are unproven either way.
- **MCP responses are not content-checked.** The harness classifies status and JSON shape only; it does not check that MCP results belong to the caller's family.
- **Not an anchor.** Rate 4 was chosen after rate 15 failed the healthy criterion. It is not a Track A acceptance anchor (that requires rate 15), and it establishes no envelope or expansion.
- **Drift.** Sync-only p95 drifted by up to 11% within the session. The ≤10% verdicts rely on bracketing sync-only cells; read cells are worse than +10% against the session's single fastest sync-only cell.
- **Uncontrolled host.** Unrelated host activity was not controlled. Load averages are recorded per cell.
- **Flood telemetry.** App capacity samples are truncated in flood and busy-client cells (log-tail limit). External Docker CPU and proxy categories substitute for them.

## Cleanup

All 22 runner invocations exited 0 with an owned cleanup receipt (`owned_resources_absent`). An independent post-run audit (`cleanup-audit.json`, summarized in the JSON) found:
- for every owner label: no containers, networks or volumes, and no private root or restore directory;
- every supervised process group gone;
- no remaining `capacity-client` process.

Retained on purpose:
- the sealed fixture and private receipts under `/path/to/bt-mcp-task7b-evidence`;
- the local image `bt-capacity-product:36cc6eb…`.

`/path/to/bt-capacity-task7-evidence` was not touched. Nothing was run on Unraid, in production, or anywhere remote.

## 7c remeasurement (2026-10-04)

**A single unpaced MCP client with a valid credential no longer starves phone sync. A high-rate connection flood still does, at the shared TLS proxy. Unpaced clients with invalid, revoked or expired credentials were not measured and are expected to behave like the 7b busy client.**

| Cell | 7b result | 7c result | 7c verdict |
| --- | --- | --- | --- |
| Busy client (valid credential), max 4 | +194% write p95 | +1.8% / +2.0% (write / peer) | **Pass** |
| Busy client (valid credential), max 1 | +49% write p95 | −3.3% / −2.2% (write / peer) | **Pass** |
| Paced reads, max 4 | +7.5% / +4.4% | +8.8% / +3.6% | Pass |
| Paced reads, max 1 | +7.9% | +8.1% / +5.5% | Pass |
| Flood (4 × 8), max 4 | +280%† write p95, 40% errors | +3285% write p95, 31% errors | **Fail** |
| Flood (4 × 8), max 1 | +103%† write p95, 34% errors | +3474%† write p95, 32% errors, incomplete | **Fail** |

† Percentile from fewer than 200 samples (write: acknowledged writes; peer: peer observations). It is **ineligible** under Track A's 200-sample rule and is reported for the record only, not as a measured p95.

The sanitized JSON holds every 7c cell, comparison, probe summary, receipt hash and audit under the new top-level key `remeasurement_7c`. All existing keys are byte-identical.

### What changed

Product commit `737c06a`, `perf(mcp): refuse repeat over-limit credentials before authentication`:

- **Diagnosis.** An opt-in CPU-profiled probe was added: `TestSyntheticBusyMCPClientProbe` in `Server/internal/mcpapi`. It drives the real router (with access logging), the real handler, the real guard (file fence, ledger, projection, family policy), the real keyed gate and the real database limiter. The setup is one credential with two unpaced workers, an authority head of 281 and no connection reuse.
  - **Before the change:** 187 requests/s and 6.7 ms of process CPU per request.
    - `CheckIntegration` took **77.7%** of all CPU samples.
    - Almost all of that was ledger `VerifyInventory`, which opens, reads and closes one file per authority event on every lease (`openat` 55.8%, `read` 10.7%, `close` 5.7%).
    - The quota limiter took 0.49%, access logging 0.40%, admitted reads 1.4% and the runtime scheduler 15.7%. There is no TLS in the app.
  - **After the change:** about 18,000 requests/s and 0.12 ms per request (client included). `CheckIntegration` dropped to 0.29 s of a 20 s run, and the handler's own refusal path costs about 1–2 µs.
- **Refusal cache.** After the full check authenticates a credential and the gate or quota refuses it, the SHA-256 of the exact presented credential string is remembered.
  - A repeat from that string is refused before global admission and authentication. It gets the same 429 status, body, `Retry-After` and `no-store` as the authenticated refusal.
  - **Quota entries** last until the limiter's retry time, `now + Retry-After`. `Retry-After` is in whole seconds, rounded up, so an entry can outlast the fixed one-minute window by up to about a second. A client that honours `Retry-After` is never refused by the cache.
  - **Concurrency entries** last only while the credential's in-flight request still holds the gate (`Keyed.Held`), and at most 1 s.
  - The cache holds at most 1,024 entries, and only authenticated credentials are inserted. It evicts expired entries first, then the entry that ends soonest.
  - A hit never admits, and a miss runs the full check. It is keyed by credential, not grant, so a peer that knows only a grant ID cannot trigger it.
  - A revoked credential can keep receiving 429 instead of 401 for at most the rest of its quota window (plus up to about a second of rounding). After that, every request pays full verification to get its 401, because 401s are not cached. Revocation therefore does not relieve an unpaced client's load (see [Final supported limits](#final-supported-limits)).
- **Logging.** Repeat refusals and pre-authentication `server_busy` are counted in a `requests refused summary` line, written per route and status at most every 10 s, instead of one `request completed` line each. Admitted, authenticated and 401/403 outcomes keep their own line and request ID.
  - **Why:** the logging was not a material CPU cost (0.4%). The change was made because per-request lines pushed the five-second capacity samples out of the harness's 1,500-line tail.
  - **Effect:** busy cells now keep all 69 samples, and flood cells keep 53 and 68. 7b kept 0 in its flood cells.
- **Keep-alive.** Refusals did not force a connection close. A new test confirms that busy, authenticated-quota and cached-quota refusals with a 16 KiB unread body reuse one connection, with no `Connection: close`.

Harness commit `988885b`, `test(capacity): give MCP clients their own transport`:
- MCP traffic now has its own pinned HTTP transport and connection pool, built by the same helper with the same owned-proxy dial guard, TLS roots and redirect refusal.
- The runner, provenance and nginx configuration are unchanged.
- No upstream keepalive or other ingress relief was added.

### Method

- **Runner and cell geometry.** Same runner path, profile `standard`, rate 4, 300 s window and cell options as 7b.
- **Fixture.** The 7b sealed fixture was reused, not regenerated: input `376fd8f4…f6b0b6578`, heads 248 → 280.
- **Order.** Cells ran back to back from a clean checkout of `988885b`: S1, busy@4, S2, busy@1, S3, flood@4, S4, flood@1, S5, read@4, S6, read@1, S7.
- **Host load.** The one-minute load average was 1.4–3.6 at cell starts.
- **Concurrent activity.** No compiler or tests ran during measurement. Another session's test database container was running when the series started and was gone at the end; it was not touched.

```sh
rtk proxy bash Server/scripts/sync-capacity.sh --profile standard --scenario matched --metrics on \
  --measure-seconds 300 --rate 4 <cell options as in 7b> \
  --source <repo>/.worktrees/single-node-capacity-mcp \
  --fixture /path/to/bt-mcp-task7b-evidence/fixture --output /path/to/bt-mcp-task7c-evidence/<cell> --total-seconds 900
```

| Identity (all 13 cells) | Value |
| --- | --- |
| Product and harness revision | `988885bf0c8b859dd5b6902829d476d2eb31a4dd` (clean; `provenance_verified=true` everywhere) |
| Product input / binary | `706c3396…117bac` / `9c564f1c…0499fc` |
| arm64 app image | `sha256:44326c216b1eed455bf30b1f7afb72049648373f8a515ea4526dab3157f5ac06` |
| PostgreSQL / proxy images | unchanged from 7b |
| Harness input / client | `48fc2e92…5f42b3` / `a053bad3…76bc91` |

### Results (rate 4)

Δ is the p95 change against the mean of the bracketing sync-only cells.

| Cell | Writes ACK / peer / planned | Unexpected sync | Write p50 / p95 | Peer p50 / p95 | Δ write / peer | MCP attempts → admitted / refused / errors | Authenticated (`mcp` accepted) | App / proxy CPU |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| S1 | 320 / 320 / 320 | 0 / 640 | 0.187 / 0.214 | 0.358 / 0.392 | | | | 40.7% / 0.1% |
| busy@4 | 320 / 320 / 320 | 0 / 640 | 0.190 / 0.225 | 0.363 / 0.405 | **+1.8% / +2.0%** | 28,018 → 150 / 27,868 / 0 | 322 | **42.8%** / 0.7% |
| S2 | 320 / 320 / 320 | 0 / 640 | 0.212 / 0.228 | 0.381 / 0.402 | | | | 44.4% |
| busy@1 | 320 / 320 / 320 | 0 / 640 | 0.209 / 0.226 | 0.378 / 0.400 | **−3.3% / −2.2%** | 27,734 → 150 / 27,584 / 0 | 186 (235 `server_busy`) | 46.1% / 1.3% |
| S3 | 320 / 320 / 320 | 0 / 640 | 0.213 / 0.240 | 0.384 / 0.416 | | | | 44.4% |
| flood@4 | **204 / 156 / 320** | **164 / 524** | 0.303 / **7.899** | 0.514 / **4.334**† | +3285% / +960%† | 182,627 → 600 / 125,496 / 56,531 | 1,221 | 34.2% / **44.7%** |
| S4 | 320 / 320 / 320 | 0 / 640 | 0.207 / 0.227 | 0.378 / 0.402 | | | | 42.6% |
| flood@1 | **199 / 154 / 320** | **166 / 519** (+3 warmup) | 0.295 / **8.194**† | 0.488 / **4.630**† | +3474%† / +1045%† | 184,161 → 600 / 125,265 / 58,296 | 744 | 33.6% / **43.7%** |
| S5 | 320 / 320 / 320 | 0 / 640 | 0.209 / 0.232 | 0.380 / 0.407 | | | | 43.5% |
| read@4 | 320 / 320 / 320 | 0 / 640 | 0.213 / 0.248 | 0.382 / 0.415 | +8.8% / +3.6% | 400 → 400 / 0 / 0 | 442 | 45.2% |
| S6 | 320 / 320 / 320 | 0 / 640 | 0.207 / 0.224 | 0.372 / 0.395 | | | | 42.8% |
| read@1 | 320 / 320 / 320 | 0 / 640 | 0.222 / 0.245 | 0.393 / 0.420 | +8.1% / +5.5% | 400 → 400 / 0 / 0 | 442 | 47.5% |
| S7 | 320 / 320 / 320 | 0 / 640 | 0.213 / 0.229 | 0.381 / 0.402 | | | | 43.4% |

† Percentile from fewer than 200 samples (write: acknowledged writes; peer: peer observations). It is **ineligible** under Track A's 200-sample rule and is reported for the record only, not as a measured p95.

Healthy cells:
- All sync-only, busy and read cells are `complete` with harness `passing=true`.
- Each has 0 skipped writes, 32/32 live sockets, 0 unresolved writes, 0 acknowledged-without-peer and 0 refusals.
- **Noise.** Sync-only spread across S1–S7 is 11.3% (write p95) and 5.9% (peer p95).
- **Read cells against single brackets.** Against S6 alone, read@4 is +10.8% write. Against the fastest sync-only cell (S1), read is +15.7% and +14.3%. As in 7b, the 10% bound sits at the noise floor.

**Busy client.**
- Quotas held exactly: 30 admitted/min.
- No MCP timeouts or errors.
- Of about 28,000 requests, 322 at max 4 and 186 at max 1 reached authentication; the rest were refused from the cache. In 7b, 23,674 and 15,294 did.
- `integrity_verify` dropped from 235.5 s to 4.1 s at max 4, and from 136.5 s to 3.2 s at max 1.
- App CPU fell from 99.7% / 81.3% to 42.8% / 46.1%, the same as sync-only.

**Flood.** Both flood cells fail. Max 1 is also `sample_incomplete` (199 < 200 acknowledged).

Evidence that the failure is at the ingress, not in the app:
- the owned nginx logged 55,015 and 57,101 upstream `connect() … (99: Address not available)` lines;
- the proxy averaged 44.7% / 43.7% of a 0.5-CPU cap, about 89% of the cap;
- the app averaged only 34% CPU;
- MCP `other` outcomes (proxy non-JSON 5xx) were 56,524 and 57,641.

Phone errors were mostly 10 s deadline expiries (82 and 89 mutations), plus proxy 5xx (27/21 mutations, 46/42 peer pulls) and 7/11 host transport errors. The 45–48 acknowledged writes with no peer observation are unproven either way, not detected loss.

**Phone latency is worse than in 7b.** Proxy error counts and MCP offered rate are about the same as in 7b, but phone tail latency is far worse: write p95 7.9 s (204 samples) against 0.86 s (148 samples†, so this comparison is indicative only). Two changes coincide in these cells, so they cannot be separated from this data:
- **Separate MCP pool (likely main cause; inference, not measured per connection).** Phones no longer pick up idle connections that MCP workers keep returning to a shared pool. More phone requests therefore open new connections into the saturated proxy, and they wait out connect or TLS-handshake backlogs instead of failing fast with a 502.
- **Cheap refusals.** These let the generator's 32 MCP workers loop at their 20 ms floor.

The flood result therefore does not depend on in-app MCP cost. It is set by how many new connections per second the ingress accepts.

### Criteria verdicts (7c)

| Criterion | Busy client, valid credential (max 4 / 1) | Paced read (max 4 / 1) | Flood (max 4 / 1) |
| --- | --- | --- | --- |
| Sync p95 < 1 s | Pass (peer ≤ 0.405 s) | Pass (≤ 0.420 s) | **Fail** (write p95 7.9 / 8.2† s) |
| < 0.1% unexpected sync errors | Pass (0) | Pass (0) | **Fail** (31.3% / 32.0%) |
| Zero lost / cross-family (within harness detection) | Pass | Pass | Inconclusive (45–48 acknowledged without peer observation) |
| ≤ 10% p95 regression (bracketed) | **Pass** (+1.8% / −3.3% write) | Pass (+8.8% / +8.1% write) | **Fail** |
| MCP admitted/refused recorded separately | Yes | Yes | Yes |

† As above: ineligible percentile (fewer than 200 samples). Both flood cells fail regardless, on unexpected errors.

**Updated supported limits.** See [Final supported limits](#final-supported-limits) above. The flood remains a documented limitation and a deployment requirement: connection and rate limits at the ingress. It is not simulated away here. Unpaced clients with invalid, revoked or expired credentials were not measured in 7c and are unsupported.

### Limitations (7c)

All 7b limitations still apply. In addition:
- **The probe is synthetic.** It shares one process between client and server and uses a test ledger grown with grant/revoke events. It attributes CPU; it does not measure container cost.
- **The flood cells changed two variables.** The product refusal path and the separate MCP transport changed together, so their effects on phone tail latency cannot be separated.
- **No real Tailscale Serve ingress was tested.** Its behaviour under a connection flood is unknown.

### Cleanup (7c)

All 13 runner invocations exited 0 with `owned_resources_absent`. An independent audit (`/path/to/bt-mcp-task7c-evidence/cleanup-audit.json`, embedded in the JSON) found:
- for every owner: no labelled containers, networks or volumes, and no private root or restore directory;
- every process group gone;
- no `capacity-client` process and no `com.babytracker.capacity.owner`-labelled container.

The DB test harness runs used `Server/scripts/test-db.sh` or a private copy without `-race`. Each removed its own compose project.

Retained on purpose:
- the receipts under `/path/to/bt-mcp-task7c-evidence`;
- the local image `bt-capacity-product:988885b…`;
- the 7b fixture, read-only.
