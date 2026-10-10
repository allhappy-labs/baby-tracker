# Single-node capacity: final local verification, 2026-10-03

**No capacity envelope was selected.** Both explicit-pool 32-device profiles remain sample-incomplete; neither qualifies for latency percentiles or expansion. The full source-attested Swift attempt also missed its 200-pair requirement and exposed a native shutdown defect. The bounded investigation and cancellation repairs are described below. Node measurement, rendered UI, physical-device acceptance and a supported Unraid client count remain unverified.

The [final evidence artifact](2026-10-03-single-node-capacity-final-evidence.json) preserves complete final cell rows, resource series, distinct source/build identities, failed/interrupted receipts, typed missing metrics, commands, cleanup audits and hashes of retained private receipts. Historical baseline evidence below is preserved separately. The [Unraid runbook](2026-10-03-unraid-capacity-runbook.md) is a concrete proposal, not approval to run it. Track B follows before the combined decision.

Track B's private read-only MCP mixed-load evidence, built on this track's baselines, is in [2026-10-03-mcp-mixed-load.md](2026-10-03-mcp-mixed-load.md) (matched sync/MCP cells, criteria verdicts, the ingress-flood limitation) and [2026-10-03-mcp.md](2026-10-03-mcp.md) (the disposable end-to-end proof, gate results and desktop-client validation). Neither changes the capacity conclusions above; both remain local, non-anchor evidence.

## Final retained-source gates

From the feature worktree, `rtk make server-test` passed all 35 tested Go packages with mandatory disposable PostgreSQL and race detection; `rtk make server-build` passed linux/amd64 and linux/arm64; `rtk proxy go vet ./...` from `Server` passed; `rtk make server-protocol-e2e` passed in 107.476 seconds. The protocol script requires the actual two-caregiver completion marker and PostgreSQL/reinstalled-store reconciliation before success. Its RTK log is filtered/truncated output; the successful exit is from the unchanged assertion-bearing script, not a separately reconstructed raw transcript.

Bounded controls passed: runner 9 tests, resource metrics 3 tests, native route/proof sink 2 tests, and the full identitytest capacity package (15.927 seconds). All ran once before measurement. These gates used source `240e90d4b72b338e4b97fa8dc8a7ed2f0b64f4cc`. The later scoped Swift-only repairs retain exactly the same Go server and runner inputs; they have their own focused and native verification, not a fabricated new full-server receipt. No simulator was needed or created.

## Final 32-device cells

Both cells used the untagged release at `240e90d`, diagnostics on, runtime/projector/maintenance/MCP pool maxima **16/4/2/0**, declared PostgreSQL ceiling/reserve **64/8**, and admission defaults total/MCP **32/0**. The sealed fixture was reused without mutation: 16 families, 32 devices, one account/two devices/one baby per family, diaper history, sealed authority head 248 and restored head 280. Each offered 16 writers × 15 writes/minute, 30-second warmup and a 60-second measured arrival window, followed by bounded drain. No task compiler/test ran during measurement; unrelated host activity was not controlled.

| Profile | Attempted / ACK / HTTP peer | Skipped / unfired of 240 | Measured HTTP successes / attempts | Unexpected HTTP errors | Completion |
| --- | --- | --- | --- | --- | --- |
| Standard, 4 CPU / 4 GiB | 163 / 163 / 163 | 64 / 13 | 326 / 326 | 0 | sample_incomplete |
| Small, 2 CPU / 2 GiB | 100 / 49 / 5 | 115 / 25 | 54 / 149 | 95 | sample_incomplete |

Every row reconciles attempted + skipped + unfired = 240. The small cell includes 49 mutation deadlines, 2 mutation 503 responses and 44 peer deadlines; warmup errors are separately retained. The runner's quota-refusal field is zero in both cells; that does **not** erase the small cell's 503s or deadlines. Both retain 32 live sockets with zero unexpected losses and zero unresolved writes under the runner's reconciliation. The small cell's 44 acknowledged effects without an observed HTTP peer are missing evidence, not proof of durable loss. Neither run establishes end-to-end zero-loss Swift durability or cross-family acceptance under this load.

All writer/HTTP-peer percentiles remain null and `passing=false`. Standard/small supervisor exit 0 means their lifecycle completed (123.854 / 136.087 seconds), not that the cell passed. No 128/512-device cells, additional rates/shapes, selected envelope or 15-minute soak ran after the failed gate. These are gated/unmeasured outcomes rather than omitted successful capacity rows.

| Identity shared by these two cells | Value |
| --- | --- |
| Server inputs SHA256 | `01e3968227075d066f40fc7e6df33f4e1c0e916a4438e5147e7f1d1eea0cbe8d` |
| Server binary SHA256 | `02d1bc2dfa8bbc29ae44fd82b808bdfab64a0d13898459e0ea4092a50a7dd4c6` |
| arm64 app image | `sha256:f34e13c747fe88d72250e165cd41e1686945f92559dfa2998e551dfd2d917c83` |
| Fixture input SHA256 | `c660f1c79d6eb434dd5d78dab1c8417bde717966da0fbdcceb23744cc92a737d` |
| Restored fixture source input SHA256 | `27f3e54725cea0d2cb92c752c9aeeb69ace7ed14339761197bb1dd55be00f7f0` |

App/database/proxy ceilings were respectively 1/2.5/0.5 CPU and 512/3072/512 MiB (standard), and 0.5/1.25/0.25 CPU and 256/1536/256 MiB (small), with equal memory/swap limits verified by Docker inspection. Generator CPU/RSS is outside these container budgets and reported separately. No disk-I/O ceiling was enforced locally.

| Descriptive resource observation | Standard | Small |
| --- | --- | --- |
| Mean sampled app Docker CPU | 100.030% (21 samples) | 49.860% (25 samples) |
| Maximum app Docker working set | 30,922,506 B | 38,566,625 B |
| App sampled CPU counter delta | 104.127 s | 61.808 s |
| App allocation counter delta | 32,818,104,392 B | 16,889,742,496 B |
| App process peak RSS | 36,757,504 B | 49,225,728 B |
| App sample window | 17:55:21.889–17:57:06.918 UTC | 17:57:45.389–17:59:50.438 UTC |
| Host one-minute load, start → end | 2.534 → 3.283 | 2.647 → 2.415 |

Counter deltas are available with no observed reset. Windows include setup/warmup/measurement; they are not isolated steady-state rates. Allocation counts are churn, Docker working set is not RSS, and snapshot CPU averages are not integrated counters. Runtime/projector/recovery pool maxima remain 16/4/2 in the full series; the small cell's cumulative projector wait reaches 28.415 seconds. The near-quota app CPU and incomplete samples describe pressure, not a maximum supported client count or an optimization benefit. No new on/off instrumentation-overhead comparison was performed.

## Attested Swift failure, diagnosis and prevention

`swift-full` used a fresh selected-source build, hashed toolchain helper/bundle/resources, and native HTTPS/WSS with the negative trust/target controls. Source `240e90d` built in 55.667 seconds. It retained **164 coherent pairs in 120.695 seconds**, then failed the required 200-pair assertion and suspended at `stop Owner begin`. Supervisor exit 1 at 556.254 seconds reflects its 600-second total budget with cleanup reserve; helper elapsed was 490.131 seconds. Published diagnostic percentiles are all null even though the private raw test summary contains ineligible statistics. No post-measurement isolation/healthy-completion marker exists for this failed full run.

Read-only inspection found a separate cancellation hole in the native pong callback bridge. Three deterministic regressions failed against the old implementation (pre-cancelled registration, missing callback and cancellation during registration). Commit `5f91a7d2222ecd8e3a853aa91633d68ea1357581` adds a cancellation-aware once gate and preserves cancellation through the native call sites. A source-attested three-pair control with an explicit 35-second dwell still suspended at Owner stop; its interrupted receipt and cleanup are retained. Thus the pong repair alone did **not** fix the observed suspension.

An attribution-only disposable clone at `f50761d13999d5a3d4e3948a18d4e5fb4f4df30d` added synchronous stop/join markers. It showed the engine's previous task joined, websocket state transition completed, both preceding native pings completed and the Owner ping child exited. The listen body closed the socket and cancelled children, but the Owner **receive child never returned**, preventing the runner join. The exact instrumentation patch and failed receipt are retained; this source is excluded from product/acceptance, just like Task 6's discarded profiler/buffer candidates.

A production-adapter regression then reproduced this boundary: `FamilySyncWebSocket.stop()` could not finish until a withheld receive callback was manually released. Foundation's non-overridable receive extension required a narrow internal native-task protocol seam; two initial test-fixture compilation failures are retained separately from behavioral RED. Commit `71e14340bee483619b4b78c52932c30bfda1ad6f` uses the same cancellation-aware once gate for callback-based native receive. The socket owner still sends close; cancellation joins the receiver even if Foundation never invokes its callback, and late/duplicate completions cannot resume it twice. Ordinary message, close and upgrade error mappings are preserved; no trust, protocol, quota or engine-lifecycle redesign was made.

Final focused verification passed **43 tests**, including actual adapter/runner stop-join, pong cancellation, existing websocket behavior, engine stop ordering and proof controls. The earlier affected parser/attestation and 9 supervisor controls passed too. The test-count expectation is 1251 after four added tests; the broad 1251-test Swift package suite was not run in this explicitly bounded amendment. No full 200-pair rerun was used to chase a passing sample count.

The final selected-source control, `swift-receive-fixed` at `71e1434`, **passed**: fresh build 59.733 seconds, helper diagnostic 41.690 seconds including the explicit 35-second dwell, three-pair measurement 2.115 seconds, whole supervised lifecycle 136.809 seconds. It is coherent/lifecycle_complete, with all five published percentiles null. Native negative trust/target and HTTP/WSS controls, exact two-family isolation, every Owner/Caregiver/isolation stop begin/end and stores-cleanup end were observed. The successful long-dwell lifecycle and deterministic joined-receiver regression close the diagnosed native receive shutdown defect; they do not establish a 200-pair latency result or retroactively attest/repair historical receipts.

The final Swift package input SHA256 is `c3f04c8b62ea66d40c5615445bc16e358332394249f0355e410f05fe65159437`; test bundle `f6d18113b9b4f4516010b02ac76be0ff2001d65bc41dce0db68c351cf6b0b49e`; helper `7f75846ef1ac3ba70920a7f436044bd801ebffcf62dd9a3fddfb9e0fe300c7e5`. The receipt separately hashes two runtime frameworks and twelve resource bundles and validates inputs/artifacts before and after loading. Whole-branch SHA and embedded server revision changed, while the Go input hash remains identical to the 32-device cells.

All launched Task 7 cells have owned cleanup receipts and independent exact-label/root/process-group audits. Disposable server/protocol projects and the stopped amd64 extraction container are absent. Reusable sealed fixture, private evidence, build products/caches, local images and attribution clone are retained for review. No production workload, configuration, credentials, deployment, merge, push, global trust/DNS/proxy changes or simulator creation occurred. The node's SSH banner timeout returned no fresh host facts; the proposal requires new host/path/pressure checks and image re-attestation after Track B before approval.

---

## Historical observational baseline and prior review corrections


All four 32-device HTTP cells are **sample-incomplete**, and no latency percentile, sustainable capacity, saturation limit, durable Swift-store acceptance, or UI acceptance is established. The standard profile completed every attempted write and HTTP peer observation; the small profile produced many deadlines/503 s. Instrumentation overhead is inconclusive. No optimization or production change was made.

The [bounded evidence artifact](2026-10-03-single-node-capacity-evidence.json) contains count accounting, exact source identities, all baseline app/external resource samples, counter availability/reset classification, phase histograms, host-load readings, cleanup receipts and raw-receipt hashes. It contains no sessions, credentials, DSNs, account/family labels or raw server logs.

## Reproduction and provenance

Run from the feature worktree `<repo>/.worktrees/single-node-capacity-mcp`:

```sh
rtk proxy bash Server/scripts/sync-capacity.sh \
  --profile standard --scenario pilot --metrics off \
  --source <repo>/.worktrees/single-node-capacity-mcp \
  --fixture /path/to/bt-capacity-task2-evidence/fixture \
  --output /path/to/bt-capacity-task3-evidence/standard-off \
  --total-seconds 300
```

The other three calls changed only `--profile` (`standard`/`small`), `--metrics` (`off`/`on`) and fresh output name. They ran sequentially, after our compilation/tests ended, with no repeated pilot to obtain a passing result. Each restored the sealed fixture, refreshed sessions, closed setup pools, started the untagged release with the owned PostgreSQL and TLS proxy, and verified the release identitytest route returns 404.

All four cells match:

| Identity | Value |
| --- | --- |
| Product source | `b8605a9bcb9515856ff484ce0b9db1a93c66a203` |
| Product input SHA256 | `bbd1372f89c3edb9b71189af03308c7e4de51c2443a5a960ddff00a6d3524963` |
| Product binary SHA256 | `f56026be918bb3d374d55d3f58dadc0e2a71fb744d1d568dfd931c5616ffdcd8` |
| Observational Server patch, base494682c→b8605a9 | `af279d92a8f11883f9db8b4cae5ced9089fca60bd501a99b4939028f551fcb15` |
| Fixture geometry | 16 families,32 devices, one account/two devices/one baby per family, diaper-only history |
| Authority heads | sealed 248; restored 280, genuine session refreshes retained |
| Host | arm64 macOS,10 logical CPUs; generator outside container budgets |

The product uses identical observational code in off/on cells; the environment flag toggles collection. No internal phase comparison with an uninstrumented build is inferred. Earlier Task2 results have different source identities and are not silently pooled into this comparison.

## Workload and counts

Each cell offered16writers×15writes/minute=240 slots,32connected devices,30 s warmup,60 s steady measurement, then bounded drain. HTTP peer timing means a distinct device decoded the matching journal effect; it does not mean a Swift store applied or a view rendered it.

| Profile / metrics | Mutation attempts | ACKs | HTTP peers | Skipped | Unfired | Measured HTTP success / attempts | Unexpected HTTP errors |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| standard / off |173|173|173|54|13|346/346|0|
| standard / on |170|170|170|63|7|340/340|0|
| small / off |99|38|4|115|26|42/137|95|
| small / on |102|52|1|112|26|53/154|101|

Every row accounts for240 slots as attempts+skipped+unfired. All retained32/32 live sockets with zero unexpected losses and zero admission refusals. All have null writer/peer percentiles and `passing=false`. Runner `status.json completion=complete` means the supervisor completed its lifecycle; it does not override `cell.json completion=sample_incomplete`.

Small/off measured failures:59mutation deadlines,2mutation503s,34peer deadlines. Small/on:40mutation deadlines,10mutation503s,51peer deadlines. Warmup failures remain separately recorded. The34/51 ACKs without observed peer are missing peer evidence; these receipts do not prove durable data loss. No zero-loss/durable-correctness conclusion follows for those cells.

Neither profile passed the 200-successful-observation eligibility threshold. 128/512 expansion and a selected-envelope 15minute soak were not run.

## Resource ceilings and observational overhead

| Profile | App | PostgreSQL | TLS proxy | Combined ceiling |
| --- | --- | --- | --- | --- |
| standard |1 CPU /512 MiB|2.5 CPU /3072 MiB|0.5 CPU /512 MiB|4 CPU /4 GiB|
| small |0.5 CPU /256 MiB|1.25 CPU /1536 MiB|0.25 CPU /256 MiB|2 CPU /2 GiB|

Docker inspections verified CPU, memory and equal memory+swap limits. These ceilings exclude the Go/Swift generator and CONNECT listener. Docker sampled working-set memory is not process RSS or a measured combined physical-memory total.

Five-second external readings span setup, warmup, measurement and teardown. The following are descriptive means of available Docker CPU snapshots, not integrated CPU counters or isolated steady-window overhead:

| Cell | App mean CPU% / max Docker working set | DB mean CPU% | Proxy mean CPU% | Go generator CPU delta / max current RSS |
| --- | --- | ---: | ---: | --- |
| standard/off |99.574% /31,960,596B|5.822%|0.145%|2.31 s /52,215,808B|
| standard/on |99.433% /34,676,408B|5.196%|0.131%|2.33 s /52,150,272B|
| small/off |49.926% /37,654,364B|3.115%|0.162%|2.43 s /52,461,568B|
| small/on |50.112% /38,335,938B|2.840%|0.148%|2.28 s /52,805,632B|

The means use 21 available samples for each standard container and 25 for each small container. App CPU snapshots sit near each app quota. This is evidence of CPU-budget pressure in these cells, not a sustainable throughput or server-capacity limit. Standard off/on observed 173/170 ACKs under differing skips and host load; the small cells failed many operations. These single incomplete pairs cannot quantify instrumentation overhead.

Host one-minute load at start/end: standard/off 4.071→3.452; standard/on 3.469→3.124; small/off 2.550→8.360; small/on 6.402→3.654. Earlier verification overlapped unrelated host compilation; we neither stopped unrelated processes nor claimed a controlled idle host. Full1/5/15 minute values are retained.

## Internal phase profile

The opt-in collector records bounded histograms with boundaries0.1/0.5/1/5/10/50/100/500 ms,1/5/30 s plus overflow. It captures fixed scalars under fences and publishes them only after the outer lease. Cache mutex wait is separate from integrity work. The independent sampler performs ReadMemStats/getrusage and all-pool statistics every5s, with joined shutdown. Runtime/projector/export/recovery roles are fixed; identical actual shared maintenance pools are counted once.

These are complete app-sampling windows, **not just the steady minute**:

| Metrics-on cell | UTC window | Samples | CPU counter delta | Total allocation counter delta | Process peak RSS |
| --- | --- | ---: | ---: | ---: | ---: |
| standard |14:58:40.816352110–15:00:25.830679458|22|104.149998 s|34,042,549,096B|42,016,768B|
| small |15:04:12.869719476–15:06:17.921322543|26|61.768641 s|17,210,714,888B|45,723,648B|

Both CPU/allocation deltas are `available`, with no observed reset. Allocation totals describe churn over the window, not live memory. RSS is getrusage peak RSS, not current RSS. Unavailable/reset counters remain distinct from genuine zero.

| Phase | Standard captures / aggregate seconds | Small captures / aggregate seconds |
| --- | ---: | ---: |
| cache_wait |528 /865.476|257 /782.133|
| integrity_verify |528 /85.616|257 /87.072|
| authority |5582 /1774.053|2193 /2150.385|
| database |2886 /576.735|957 /483.240|
| transaction_acquire |528 /8.458|171 /5.319|
| family_fence |528 /2.478|141 /2.377|
| security_fence |528 /0.964|257 /2.733|
| observer |504 /0.192|208 /0.996|

Phase durations overlap, include concurrent callers and multiple nested observations, and must not be added into wall time. Integrity bytes are actual selected-envelope reads from opted-in sync contexts: 1,759,960,368B standard;660,859,006B small. They are not all-process file-I/O totals. The unchanged strict history validation remains in place. Cache wait, verification work and allocation churn justify further investigation; they do not justify weakening integrity or introducing a TTL cache.

Standard end-of-window pools: runtime 10 total / 2 acquired / max 10, projector 8 / 2 / max 10, recovery 1 / 0 / max 10. Aggregate runtime empty-acquire wait 0.316867132 s and projector 0.541880482 s are cumulative pool observations, not per-request percentiles. Recovery ran one observed acquire. Export was disabled in this composition; distinct/export/shared-role behavior is covered by focused tests. Full per-pool series are in the evidence artifact.

## Actual Swift diagnostic and lifecycle control

The full separately restored Swift run used product 9707d70, not a 32-device mixed workload. It uses two actual on-disk Phone compositions on one account, a preprovisioned second-family isolation session, canonical HTTPS/WSS through an owned loopback CONNECT route, exact certificate pin plus capacity-only hostname/expiry evaluation, unchanged quotas and native URLSessionWebSocketTasks. It attempted a 30 s warmup followed by 200 samples under the existing 120 s measurement ceiling and0.55 s arrival pacing. It does not render SwiftUI or run on a physical device.

`swift-01` hit its owned 600 s supervisor deadline (Swift command534.284 s). It retained 128 complete pair rows, no final summary, native HTTP/WSS/negative trust-target proof markers, and no eligible percentiles. Its suspension remains unresolved. One-second helper samples at 16:42:03 and 16:47:04+0200 show actual SwiftData remote-apply work first and idle main/CFNetwork run loops later; there was no sampled blocked write. The original unmarked run cannot locate the exact suspended measurement/final-validation/teardown continuation. Both samples perturb that diagnostic.

The initial receipt's `swift_command_tree_*` available-resource label is incorrect for a killed launcher: it may omit unreaped descendants. Treat it as partial launcher evidence. The receipt remains unchanged, with this explicit correction. Followup code uses `swift_launcher_*` and separate five-second live-descendant readings; live-process RSS sums can double-count shared pages, and cumulative live-process CPU sums can drop as processes exit.

A private synchronous proof sink and bounded phase markers were then added. One **three-pair lifecycle control**, `swift-lifecycle-01`, completed in6.733 s, with2.056 s measurement, both native routing/negative trust-target proof markers, second-family isolation, every Phone.stop begin/end, stores cleanup begin/end and lifecycle-complete marker. Its three pairs and null percentiles prove short-case routing/lifecycle only. No full latency rerun was performed to chase success. This short success does not resolve the long-run suspension. Final acceptance must recheck the full proof after later changes.

Both Swift cells have owned resource cleanup=true and independently verified absent process groups. Exact private roots/stores were removed; sealed fixture data was preserved. No Go sockets reused the Swift device credentials concurrently.

## Verification and boundaries

- Required affected DB gate passed: diagnostics4.442 s; security266.220 s; localstore4.242 s; server command36.857 s, with owned cleanup.
- Capacity harness package passed116.332 s; new-concurrency race checks passed separately from timing runs.
- TDD proved million-observation/zero-allocation collection, dynamic-phase rejection, byte-only exact-once Flush, unavailable/reset/zero handling, no request-phase system sampling, shared-pool deduplication, real cache-wait/byte hooks, owned CONNECT refusal/cancellation, refreshed continuity, proof completeness, process-group cleanup and synchronous proof retention.
- Final focused Swift build passed two tests; final Python suite passed ten tests; changed Go proof/route/wire checks passed. All four retained HTTP cells pass the real CLI ReadResult validator and slot accounting. No iOS simulator was used.
- BT_SYNC_DIAGNOSTICS now emits five-second `capacity resource sample` snapshots. Legacy performance_control.metric_window expects old per-phase rows and therefore reports metrics unavailable; its smoke_summary can emit empty maps. A legacy sync-metric-proof marker alone is not evidence. Use the capacity runner and this schema; do not synthesize raw request samples from histogram buckets.

No production load/configuration, deployment, merge, push, quota relaxation, integrity bypass, global DNS/proxy/trust change, or additional simulator occurred. Remaining limits are incomplete 32-device samples, small-profile failures, uncontrolled host variation, the unresolved long Swift suspension, absent 128/512/soak evidence, and absent physical-device/rendered-UI acceptance.


### Task 3 review correction: Swift proof and provenance

The two historical Swift cells (`swift-01` and `swift-lifecycle-01`) have **unattested Swift executables**. Their recorded product SHA/input/binary identify the Go server, not the Swift test/client code actually executed by the old `swift test --skip-build` path. Their observed native-route/pair/cleanup evidence remains retained, but no executable hash is retroactively assigned and these cells cannot establish a source-matched Swift acceptance result.

Future Swift cells build the selected `--source/Packages/BabyTracker` in a new private scratch directory before fixture restore and credential refresh. The overall supervision deadline covers compilation and cleanup; diagnostic elapsed excludes compilation. A separate Swift receipt records package inputs, selected source SHA, explicit debug/native recipe, SDK/toolchain, test-bundle binary and helper-executable hashes plus resource-bundle hashes. The runner validates inputs/artifact immediately before and after loading that exact bundle through the attested toolchain helper. Current supported graph is package-local targets only: external dependencies, plugins, custom settings, symlink inputs and escaping paths fail closed with typed errors. No global Swift configuration is changed.

Proof parsing now rejects duplicate/malformed/conflicting summaries, requires expected distribution/count and positive finite sequential pairs, and compares every required summary percentile to nearest-rank values derived from accepted rows. Lifecycle completion requires three coherent rows plus native-route, exact isolation and lifecycle markers; lifecycle percentiles remain null. A7 must still recheck full attested 200-pair proof; the original long-run suspension remains unresolved.


Darwin build correction: `swift-attested-01` built successfully but retained an incomplete result because a native `.xctest` Mach-O bundle cannot be executed directly (`ENOEXEC`, errno 8). It produced no test proof and cleaned all owned runtime resources. The corrected path explicitly invokes the selected toolchain's hashed `swiftpm-testing-helper --test-bundle-path <hashed bundle binary> --testing-library swift-testing --filter <case>`, matching the [official Swift Testing Darwin runner documentation](https://github.com/swiftlang/swift-testing/blob/main/Documentation/CommandlineDebugging.md). Both helper and bundle hashes are validated before/after the run; no cached product discovery is used. The native SwiftPM build backend is explicitly recorded and currently emits a deprecation warning; a future toolchain that removes it must fail until the harness is updated and revalidated.


The final corrected short control, `swift-attested-03`, used source `bc8192f361e24e35513bb6502fb0f08b9cf90f9f`: fresh build54.073582s, helper/test diagnostic5.394062s, three-pair measurement2.249872s, whole supervised run80.495756s. It returned `lifecycle_complete`, coherent=true, all negative trust/native HTTP-WSS/exact isolation/lifecycle markers and null percentiles. Package input `671eecf14c4354a848bcff9db6722b6ac1959f3ce101b4391d797cd62f416741`, test bundle `7ba9108af3f9d09230a118dc42598b657448588b59c9037722ec11a087073d4f`, helper `7f75846ef1ac3ba70920a7f436044bd801ebffcf62dd9a3fddfb9e0fe300c7e5`; SDK27.0, Swift6.4. The receipt also hashes Testing/XCTest images and12 resource bundles. Server identity is recorded separately in the JSON companion.

`swift-attested-02` was deliberately interrupted during compilation after an inert helper probe exposed missing framework search paths; no runtime fixture resources were created. All three review-fix attempts retain original receipts and cleanup=true. The final exact audit confirms their process groups, labeled containers/networks/volumes and restored private roots are absent. Build products/logs and sealed fixture remain retained. Resource snapshots for these controls include compilation/setup; they are not comparable steady-load or instrumentation-overhead measurements. The final harness-only guard clears percentiles on nonzero process exit or a proof/mode mismatch; focused tests cover this, with no additional real-cell run.


## Task 6: bounded inventory-buffer experiment, reverted

**No optimization was retained.** Matched runs did not establish a useful-work gain beyond observed variation. The experiment reduced allocation cost in a focused regression, but that is not a capacity or latency result.

The post-Task-5 baseline was `9c40710ae078d56642d96c8cc3156967f4a6df93`. One separate profiling-only build (`90477562155cd9461265bf18f6eab9345ae87cbf`) attributed 13.95% of allocation bytes to warm `VerifyInventory` → `io.ReadAll`. Its CPU profile sampled 59.52 seconds: warm verification used 34.19% cumulatively, but anchored `Openat` alone used 23.35%; `io.ReadAll` used 3.36% including required reads, and `growslice` used 1.11%. These overlapping shares cannot be summed. Strict readiness replay accounted for 82.44% of allocation bytes. This justified testing local scratch reuse, with only modest expected CPU benefit; it did not justify removing historical reads or widening authority/readiness leases.

The candidate (`2d321fbf3f15bf24b96c982aa2ddbb06440004e0`, private detached clone) reused one `bytes.Buffer` only within a `VerifyInventory` call. Every selected file was still opened, checked, fully read, and hashed; cancellation, descriptor closure, raw digest checks, rollback floors, authorization and revocation ordering were unchanged. No profiler was included in candidate or baseline comparison builds.

All four unprofiled runs used the same sealed fixture (head 248, restored head 280), explicit pools runtime/projector/maintenance/MCP = 16/4/2/0, server ceiling/reserve = 64/8, shared admission 32, standard combined 4 CPU/4 GiB limits, 32 devices, 16 writers, 15 writes per writer per minute, diagnostics on, 30-second warm-up and 60-second scheduled measurement window. Order was B1/C1/B2/C2, sequentially with no test or compiler overlap during runtime. Counts include the runner's bounded drain of work started in the scheduled window; they are not a sustainable-capacity claim.

| Cell | Scheduled / skipped / deadline-unfired | Acknowledged | HTTP peer observations | Peer deadline errors / ack without peer | App peak RSS bytes |
| --- | --- | ---: | ---: | ---: | ---: |
| baseline-01 | 216 / 115 / 24 | 101 | 45 | 56 | 38,391,808 |
| candidate-01 | 217 / 100 / 23 | 117 | 104 | 13 | 37,670,912 |
| baseline-02 | 226 / 94 / 14 | 132 | 132 | 0 | 35,856,384 |
| candidate-02 | 230 / 92 / 10 | 138 | 138 | 0 | 35,917,824 |

Every cell planned 240 writes, had zero writer errors/refusals and zero unresolved writes, and retained 32 connected peers with no unexpected socket losses. Aggregate measurement HTTP errors were 56, 13, 0, 0 in the displayed order; the top-level writer error count alone would conceal those peer failures. All cells remain `sample_incomplete`, `passing=false`, with both latency percentile fields null. No cell reaches 200 successful observations. HTTP journal observation is not Swift durable or observable apply.

Baseline acknowledgments were 101–132 (mean 116.5); candidate acknowledgments were 117–138 (mean 127.5). The mean difference of 11 (9.44%) is below both the baseline range of 31 and candidate range of 21, and their ranges overlap. B2 already exceeds C1. Two repetitions per variant suffice to reject this experiment under the conservative retention gate; they do not characterize a population distribution. Host one-minute load averages ranged from 5.01 to 7.42 at run boundaries. Background host work was preserved, so the results do not isolate a causal throughput effect.

App allocation-counter deltas per sampled CPU second were 217.60/244.22 MB for baseline and 195.89/208.68 MB for candidate. These are whole-process setup/warm-up/measurement sample windows of different lengths, not steady-window allocations or live memory. The direction agrees with less buffer churn, but does not establish useful throughput or a general resource result. Candidate peak RSS overlaps baseline variation; no resource improvement is claimed.

Baseline input SHA256: `01e3968227075d066f40fc7e6df33f4e1c0e916a4438e5147e7f1d1eea0cbe8d`; binary: `9da616ab6df50214e6fff63396b8e49446a2c983cdd0cbb9d3e27f43e3ee0475`. Candidate input: `13e94452bc57dc45affd360289486db2eec5436548c628790ba970e58fb28842`; binary: `efa64083c17b51e5fac148aca928186bc04d566ea7b32503ceb8e15e491b9519`. Each pair has identical source/input/binary identities within its variant. Fixture input remains `c660f1c79d6eb434dd5d78dab1c8417bde717966da0fbdcceb23744cc92a737d`.

The allocation regression failed before implementation (811 allocations for 32 fully verified 64 KiB files, limit 448), then passed with full historical read/byte counts. Affected real PostgreSQL/race packages passed: final localstore 6.036 s; unchanged security 37.595 s, recovery 5.393 s and server command 27.929 s from the preceding split gate. Scoped vet passed. Coverage included warmed historical replacement/deletion, rollback, external append, before/mid-read cancellation and descriptor closure, stale views/callbacks, queued revocation and exact durable SQL effect counts. Four new-test setup/expectation failures were retained and corrected (relative fixture path, required clock, invalid duplicate registration, expected rollback code); none was represented as a product defect or a successful gate.

Only this task's three experimental source/test files were restored byte-for-byte to baseline after checking their hashes for concurrent changes. No runtime change remains. Private evidence under `/path/to/bt-capacity-task6-evidence` includes `comparison.json`, command vectors/logs, raw profiles, tested input hashes and `candidate.patch` (SHA256 `b0be923857255a93fe6c3cb43a1d4f4cf795ea531d336a63ce6254e862d71442`). The detached source clones remain available for review.

All five launched cells (one attribution plus four matched) have joined exit-zero supervisors and independently verified absent owned containers, networks, private runtime/restore roots and process groups. All five disposable database test projects have absent owned containers/volumes/networks. Three prelaunch `setup_failed` attempts (`attribution`, `attribution-retry`, `attribution-03`) remain separate; they created no runtime resource receipts and supply no measurements. No additional cells, soak, broad Go/Swift gates, production changes or trust-model changes were performed. A quieter controlled-host experiment is the next measurement option; this task does not retain speculative code while waiting for it.
