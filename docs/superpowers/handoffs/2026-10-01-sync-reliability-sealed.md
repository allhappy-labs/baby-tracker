# Sync reliability — sealed checkpoint, 2026-10-01

The user requested: “Seal up the work quickly, do not run tests.” No new tests or builds were launched after that instruction. Preserve this instruction on resumption unless the user changes it.

## Workspace and authorization

- Worktree: `<repo>/.worktrees/sync-reliability`; branch `fix/sync-reliability`.
- Recovery design and implementation were approved; performance work uses the subagent-driven Superpowers workflow.
- No main merge, push, deployment or paired-phone update for this architecture. No release readiness claim.
- Main has unrelated dirty `TASKS.md` and untracked `work/`; preserve them. Keep Growth source ZIP/PDF ignored and outside Git/app bundles.
- Use only existing shared iPhone17Pro `D17101C9-01EB-488D-AFE1-58E6C089B853` with explicit destination if iOS gates become authorized. No cloning/reset/uninstall/runtime install. Home fixtures are isolated; broad `make test-ui` deletes persistent shared data and is excluded.

## What is committed

Recovery Tasks1–8 and their whole review/fix/re-review are complete. Performance Tasks1–8 are complete and reviewed. Performance Task9 diagnostic source/results are committed in `f09b4ca`; final-gate test repairs `36831e1`; accessibility Home repair `69d19d5`; instrumentation/timeout/report controls `a34fe2f`. Test/UI repairs have independent approval. The original Task9 I1–I4/M1 findings are addressed in scoped re-review. A final helper-only correction is being sealed without tests; its exact commit/review status is recorded below.

The app preserves the365-day original session lineage with short access credentials, device-bound recoverable refresh and resumable credential installation/acknowledgement. Performance changes retain fresh binding/epoch/head checks, historical-byte verification on every outer lease, bounded live delivery and existing quotas/pool behavior.

## Verified before the no-test instruction

Final product gate checkpoint: `a34fe2f1008608fd5cdcec169296c93c6bd469c5`.

- Exact1191 Swift tests and mandatory-shared simulator app build passed.
- Both affected Home recovery UI tests passed; actual English/Ukrainian accessibilityXXXL screenshots and sign-in taps inspected. Controls scroll inline at accessibility sizes; ordinary floating controls remain.
- Linux amd64/arm64 release builds and Go vet passed.
- Seven full affected server race packages and repaired snapshot deadline regression passed. Initial fullserver suite failed two tests; repaired scoped passes are not one full-suite green run.
- Hosted protocol gate passed. Separate actual selfhosted schema25 recovery gate passed four server fault boundaries and five Swift process crash boundaries, exact child/prepared version/ordinary exactly-once sync/receipt acknowledgement cleanup, real production-cadence maintenance worker admission and metric observations.
- Owned race/protocol containers and volumes were verified removed. Fresh app bundle has expected Swiss CSV/attribution/manifest hashes and no ZIP/PDF/probe/harness artifacts.

The normal-timer standalone Task8 experiment had200 pairs, durable p95 .817504667s, observable p95 .817507459s, four samples >=1s. This is local synthetic peer evidence, not physical-phone/rendered-UI/production performance.

## What is not established

Task9's baseline/current matrix retained54 incomplete cells, zero measurement attempts/pairs and null latency distributions. Single-IP setup refusals and documented paced-budget bounds prevented aged setup; fixtures diverged. There is no supported capacity envelope, matched baseline improvement or248/512/2048-history target acceptance. E artifacts are historical pre-instrumentation/pre-final-control source.

New diagnostics observe real fence/integrity/transaction/process/runtime-pool counters and Docker/DB samples. CurrentRSS, non-runtime pool attribution, pure transaction-fence wait and configured total pool ceiling are not established; process restarts invalidate whole-window deltas. Docker network/filesystem deltas from the pre-final aggregator are not validated reset-free measurements.

Whole-branch performance/integration review has not run. Final helper corrections are untested by user instruction. Parked minor findings: recovery may delay the sign-in action one retry cycle; unbound ExecutePrepared calls its epoch adapter twice. These remain visible for any resumed final review.

Actual owner sign-out trigger remains unobserved. Entitled physical-device Keychain/lifecycle, provider/Apple behavior, APNs/background delivery and deployed migration22→25/backup/rollback safety are separate evidence and decisions. No production logs or private family data were accessed to bypass previous approval limits.

## Read next

- `docs/verification/2026-10-01-sync-reliability.md`
- `docs/verification/2026-10-01-sync-performance-results.md`
- `docs/verification/2026-10-01-session-recovery-rulings.md`
- `docs/verification/2026-10-01-sync-performance-rulings.md`
- Both approved specs/plans under `docs/superpowers/`.
- Retained ignored `.superpowers/sdd/2026-10-01-sync-performance/progress.md`, Task9 report/reviews and Task10 report for exact source/evidence boundaries. Do not redispatch completed tasks.

Before further work, reconcile Git with the ledger and this checkpoint. Respect the current no-test instruction. Complete final review/last-helper validation only under appropriate resumed scope; any capacity experiment redesign or rollout needs a separate decision.


Final helper-only commit: `f5e83c7` — malformed-row own-cell/global-failure attribution and adjacent Docker counter/timestamp ordering corrections. HelperSHA256 `c7b268b8781fad9c80f695da60c44b82df52bf1547b59fc15d595085411252d3`. No tests, synthetic executions, builds or live gates were run for these last corrections. Product/Swift/instrumentation inputs remain unchanged from the verified a34fe2f checkpoint; final helper validation and whole-branch review remain deferred. A narrowly scoped read-only finding-closure review is recorded in the retained task workspace.

Final scoped review: N1/N2 closed by independent source inspection of `a34fe2f..f5e83c7`, with no apparent new Critical/Important breakage. The final helper remains UNTESTED. This is not whole-branch approval. Reports/ledgers and all execution rulings are retained; no worktree or evidence was deleted.


## Bounded validation resumed — 2026-10-01

The user authorized quick bounded tests, superseding the no-test instruction for this focused scope. The resource-free `rtk proxy bash Server/scripts/test-performance-runner.sh --revision-controls-only` ran with a180-second whole-command limit. The first run failed in.142s: invalid resource timestamps were filtered before window validation. Commit `251ba7f` narrowly corrects that and adds persistent N1/N2 regressions. One rerun passed exit0 in1.624s, covering malformed cross-cell attribution/global failure, counter resets/order/nonfinite timestamps/real zero deltas in both aggregators, killed-child checkpoint preservation and bounded cleanup/recovery pointers. Final helperSHA256 `63ce00e2c193e41e18745462c09a20ac025ff3b4988777148d8f5ec4a41cb649`; testscriptSHA256 `8cceacff860d5f10428a10c170c082a579b74e3e860f401fb3ce9767add0de13`. Captured log/result retained under `/path/to/artifact`; ignored task report contains full output/source boundaries. Owned children exited/reaped and scratch enumeration was zero. Controller read the complete output, result and scoped source diff and checked source hashes.

Final reporting-helper corrections now have focused passing regression evidence. No broad tests/builds/live server/capacity run was repeated. Whole-branch review, capacity acceptance, physical-device/provider/production proof and parked minor items remain outstanding; no merge/push/deployment/app update. Historical network/filesystem deltas remain unvalidated; these tests do not retroactively establish their series integrity.
