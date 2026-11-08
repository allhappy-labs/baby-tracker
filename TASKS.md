# Design system (2026-11-07)

- [x] Stage 1: shared kit in BabyUI (tokens, Card/CardSection/ListRow, Chip, buttons/tiles/confirmDelete, SheetScaffold/ConfirmSheet, ScreenScaffold/EmptyState/InlineError/PartsRow, kit gallery previews); no screen changed. `make test` OK 1984; `make build` OK.
- [x] Stage 2: sheets move to SheetScaffold/CardSection/chips/buttons. Done (Task 9 last): package suites green, make test OK 1985, UI RunningTimerLifecycle, HistoryRedesign, EditFlow, UkrainianLayoutSmoke pass (HistoryRedesign testAViewOnlyFamilysRowsAreNotTappable fails on the first run after a rebuild, passes on rerun).
- [x] Stage 3: Home on the kit (Task 10): dock tiles TileButtonStyle (suggested side = active fill), Stop/Wake .primary(.dock), Today/Pump/sync cards on Card, hero/sleep/pump-stop chips, shared PartsRow, ScreenScaffold header + flat Palette.ground (no hero glow: screenshots read calm, not dull; fallback stays available). Package HomeFeature/BabyUI suites pass; build OK; make test OK 1986 (+1 PartsRow mapping test); UI RunningTimerLifecycle, HomeSyncRecovery, PumpingFlow, LoggingFlow, UkrainianLayoutSmoke, ReadOnlyCaregiver, AX5 dock pass. CompositeFeedFlowTests.testTwoBottlesInARowAreBothKept fails at 00:14-00:25 CET on this branch AND on the untouched baseline (fixture near midnight) - recheck in daytime. No phone install (R16).
- [x] Stage 4: History, Insights, Growth, Ask on the kit; Ask gets the baby header (switcher wired from RootView) and a CircleIconButton new-chat, confirmDelete (ask.confirmNewChat/ask.cancelNewChat became ask.confirmDelete/ask.keep), app-wide .tint(Palette.feed) on the TabView (R19). AskFeature/BabyUI suites pass, build OK, make test OK 1986, UI AskTabTests, AccessibilityAudit, UkrainianLayoutSmoke pass. Screenshots in scratchpad stage4/ask-*, tabbar-*.
- [x] Stage 5: More, Settings, onboarding on the kit. Cleanup (Task 12c): removed PanelSection, PrimaryFillButtonStyle, TrackerButtonStyle, SheetHeader, SheetTitleText, deleteConfirmation (callers use confirmDelete), unused BabyEditCopy and PendingInvitationCopy constants; Ask composer gutter Spacing.l; leftover .bordered and accentColor replaced. make test OK 1988, build OK, UI EditFlow/LoggingFlow/CompositeFeedFlow/AskTab: see task-12c-report.md.
- [x] Stage 6: widgets and cleanup (Task 13). Quick Log buttons use `.tile(size: .widget)` (links draw the same shape from tokens, since `Link` takes no ButtonStyle); Live Activity L/R/Bottle use `.tile` (suggested side = active fill) and the kit `PartsRow(size: .widget)`; Pump/Status on tokens; History clear-filter inset Spacing.l; Growth reference buttons Spacing.s gap. Full UI suite 58/58 passed (TEST SUCCEEDED), WidgetTourTests + LiveActivityPushUITests pass, make test OK 1988, build OK; sweep leaves only chart marks/TodayStrip. Screenshots in scratchpad stage6/. 4- and 9-part cards and per-family widget shots not captured (no fixture path); no phone install.

# Pumping uses the bottle's interface (2026-11-02)

- [x] Pump sheet (Home/More → Pump) is the bottle sheet's layout: side, Start, then "or log what was pumped, in ml", Pumped at, the bottle's −/+ amount with chips, and Log (a finished session at that time, off at 0).
- [x] How much? is the stopped-bottle layout: caption, side (corrects the session's side), the bottle's amount control, Done. Skip still leaves it empty; Done at 0 also saves no amount.
- [x] Edit / Past entry panel: the bottle part editor's order. The amount control replaces Add/Remove amount; dialling to 0 clears it. Pump chips are 30/60/90/120 ml (1–4 oz), in the pump's colour; ±5 ml / ½ oz, capped at 1000 ml.
- [x] `make test` OK 1961 (+4); `make build` OK; UI `PumpingFlowTests` 6/6 on the shared simulator; screenshots of all three screens checked.
- [x] Installed eeac50d9 in place on Phone B and Phone A 20:05 CET (signed Debug, team TEAM123456, push check ready). Phone A launched; Phone B refused the launch (locked) and picks up the build when next opened.
- [x] Publishable tree: signing, bundle ID prefix, App Group and the hosted server come from the gitignored `Config/Local.xcconfig`; the Unraid host and audience from the provisioning environment; names, devices and local paths in docs are generic. GitHub gets the tree as one snapshot commit on top of its own history.
- [ ] User check on the phones: Pump → Log with an amount; Stop → How much? with side and chips; edit a pump and dial the amount to 0.

# Sync between phones: stops, Tailscale, blinking offline card (2026-11-01)

- [x] Stops: no apply bug. Prod receipts show 19 double stops since 10-10; the second phone acted on a store that had not pulled the stop (11-01: 7209 silent 15:41–19:27 CET, then a stale stop + superseded sleep start, then it stopped the other parent's real sleep). Cause is reachability + Home showing stale local state without a warning.
- [x] Tailscale: sidecar pinned at v1.98.3 since 10-04; from 10-14 (Tailscale 1.104.1 shipped 10-13) its log shows ~100 TSMP disco-key re-adverts a day, up from ~0, and DERP "does not know about peer" bursts on 10-17/21/22/25. Repo pin bumped to v1.104.4 (`Server/deploy/unraid/compose.yaml`, deployment test). `go test ./scripts/` OK.
- [x] Offline card blink: each retry set status `.syncing`, hiding the card until the failure set it back. A retry now keeps `.offline`/`.serverUnreachable` until it has an answer. +1 test (`ServerUnreachableTests`), `make test` OK 1948.
- [x] Sidecar v1.104.4 deployed on the homelab 21:30 CET (owner approved); verifier passed; both phones synced through it. Record in `docs/verification/2026-10-27-phase5-rollout.md`.
- [x] Installed d3458e63 in place on Phone B and Phone A (signed Debug, push check ready), launched; both synced and opened the live socket at 21:20 CET.
- [ ] User check on the phones: open after a while, tap Stop quickly; offline card no longer blinks.
- [x] 11-02 "Offline" again (Phone A, 14:15 CET). Phone log (`log collect`, owner's sudo): app back at 14:14:46 and sending at once; every request through the tunnel timed out until 14:16:22. The phone's Tailscale (IPNExtension) logged "UDP is blocked" / "MagicSock ReceiveIPv6 is not running" 13:44–13:47, 14:15–14:16, 14:22–14:23, 14:30–14:31, 15:04– — each healed itself in 1–2 min. Server sidecar fine; not the app. Its constant "timeout opening … 100.64.0.43" lines are a build host (offline), unrelated.
- [x] Why "Offline", not "Can't reach" (likely; the app logged nothing to prove it): a renewal that finds the account lock held by another process (`RefreshError.lockUnavailable`) set `.failed(retrying)`, which ended the catch-up as answered and reset the 3-min clock; the timeouts after read "Offline". Now a held lock leaves the catch-up running and keeps a showing failure; the hold passing unanswered always names the server. Sync failures now log their kind (`AppLog.sync`, public) so the next one is readable from the phone log. Reproduced by test first. +2 tests; `make test` OK 1957; `make build` OK.
- [x] Installed ec8b8c56 in place on both phones 17:07 CET (owner approved). Phone A launched and synced at 17:07:39 CET; Phone B was locked, so it was not launched and picks up the build the next time it opens.
- [x] Catch-up on foreground (owner said yes): the engine is `isCatchingUp` from foreground until the first sync answers (≤ 5 s). A Home timer tap meanwhile waits; if the catch-up changed the hero/dock/pump it is dropped. An unanswered catch-up with a network reads "Can't reach the family server" at once. +7 tests (`CatchUpTests`, `HomeViewModelTests`); `make test` OK 1955; `make build` OK. UI suite not run.

# Bottle presets follow formula math (2026-10-28)

- [x] Bottle chips 35 / 65 / 100 / 135 ml: what 30–120 ml of water makes with 1 scoop per 30 ml (~+12%, 0.77 ml/g). Same for every contents; oz chips stay 1–4.
- [x] Bottle ± step 5 ml (was 10); ½ oz unchanged. Pump amount step 5 ml too.
- [x] `make test` OK 1947; `make build` OK.
- [x] Committed `d6bf3b64` on main; installed in place on Phone B and Phone A (signed Debug, team TEAM123456). Launch refused: both phones locked.
- [ ] User check on the phones.

# Solids as a feed's part (2026-10-23)

- [x] Add part in a feed offers Solids: the Solids panel opens at the feed's end and writes one linked create once a food is named. Solids list as parts (foods · amount, time), open the Solids panel, delete, split and reorder like any part.
- [x] Store: a feed holding solids still joins, combines and takes parts; only a feed of solids alone is refused. Auto-join by the break limit still never takes solids. Home's parts row, the Lock Screen card and the feed's breast/ml totals stay milk-only.
- [x] `make test` OK 1946; `make build` OK; UI `HistoryRedesignTests` add-solids + swipe-delete 2/2 on the shared simulator.
- [x] Committed `4714028c` on main; installed in place and launched on Phone B and Phone A (signed Debug, team TEAM123456).
- [ ] User check on the phones: Add part → Solids in a feed.

# Reorder a session's parts (2026-10-22)

- [x] A part's times may move past another part into a free stretch; the session reorders and is re-keyed by its earliest member (supersedes ruling H-R15).
- [x] `make test` OK 1942; `make build` OK.
- [x] Committed `9f072bfb` on main; installed in place and launched on Phone B and Phone A (signed Debug, team TEAM123456).
- [ ] User check: in a feed's sheet, open the last part and set its start to right after the first part.

# Pumping (2026-10-12)

Spec `docs/superpowers/specs/2026-10-09-pumping-design.md`, plan `docs/superpowers/plans/2026-10-10-pumping.md`, verification `docs/verification/2026-10-12-pumping/README.md`.
Merged to `main` at `eaddcef0` (`TimeSpanSection` combined with `120f559e`). Server deployed at schema 34; rollout `docs/verification/2026-10-16-pumping-rollout.md`.

- [x] Server: pump payload and hashes; care kind 10 with no baby, migration `000034`, one running pump per family; superseded / `pump_running`, `pump_reopened`, `pump_already_stopped`; export and API docs.
- [x] Phone data: `PumpEntry` in `SchemaV12`; start/stop/log/edit; sync, losing start, pumps logged before the family.
- [x] Phone UI: Pump as a Home action (last); panel, How much?, Past entry; strip beside a bottle; Today line; History, filter, PDF; Lock Screen card on the starting phone; Ask.
- [x] Whole-branch review: seven fixes, then the six minors.
- [x] `make test` OK 1931 on the merged tree; server suite, build and protocol e2e (six pumping proofs) on the branch.
- [x] Server deployed `eaddcef0` (schema 34): 8 s of downtime, care rows unchanged, verifier passed.
- [x] Phone B installed and launched from `2ea985de`; syncing.
- [x] Phone A installed and launched from `2ea985de`; syncing.
- [ ] User check on the phones: Lock Screen card and its Stop when locked, cross-phone strip, card ending when the other phone stops, widget Pump tile.

# Live Activity replaced-card stall (2026-10-12)

- [x] Verify Live Activities after the recent updates: Swift 1856, UI 8/8, protocol e2e, production read, both phones' ledgers.
- [x] Found: both phones' feed jobs spin on an unsendable end; no feed push on Phone B since 2026-10-09 23:57 UTC, on Phone A since 2026-10-11 06:12 UTC.
- [x] Fix `431dc6cc` on `fix/live-activity-replaced-card` (+2 tests); all server gates pass. Notes: `docs/verification/2026-10-12-live-activity-replaced-card.md`.
- [x] Merged to main (ff) and deployed `431dc6cc` to the homelab (2026-10-12, 8 s down, verifier passed); both stuck jobs completed, leftover cards retired.
- [ ] History cleanup refused by the permission system (rows expire by 2026-10-19 on their own); two-phone Lock Screen check (user).

# Immunizations (2026-10-11)

Spec `docs/superpowers/specs/2026-10-11-immunizations-design.md`, plan `docs/superpowers/plans/2026-10-11-immunizations.md`, verification `docs/verification/2026-10-11-immunizations/README.md`, rollout `docs/verification/2026-10-11-immunizations-rollout.md`.
Merged to `main` at `339f655f`. Server deployed at schema 33 before the merge; both phones on the merged build.

- [x] Server: `immunization` payload and pinned hashes; care kind 9 with migration `000033`; cascade order, archive section, import manifest; API docs.
- [x] Phone data: `ImmunizationEntry` in schema V11; log/edit, family-wide vaccine names, next dose; remote apply, realm move, purge, import.
- [x] Phone UI: glyph and tracker; the vaccine panel; Vaccine as a Home action (listed in Settings → Home buttons); History rows, filter and PDF; the Insights card and Immunizations screen; Ask.
- [x] Package suite (1856), full server suite, protocol e2e against schema 33, simulator build, UI subset, screenshots.
- [x] Whole-branch review: one important finding and the minors fixed.
- [x] Server deployed `339f655f` (schema 33): 8 s of downtime, care rows unchanged, verifier passed.
- [x] Merged to `main`; installed and launched on Phone B and Phone A; both sync 200.
- [ ] User check on the phones: the three items under "Still open" in the rollout record.
- [ ] Pumping's plan renumbered to migration `000034` and `SchemaV12`.

# Arrangeable Home buttons (2026-10-11)

Spec `docs/superpowers/specs/2026-10-11-home-buttons-order-design.md`, plan `docs/superpowers/plans/2026-10-11-home-buttons-order.md`.
Verification: `docs/verification/2026-10-11-home-buttons/README.md`. Rollout: `docs/verification/2026-10-11-home-buttons-rollout.md`.
Merged to `main` at `34cd4455`. Client only, so no server deploy.

- [x] Order per baby on this phone; dock, More sheet and Quick Log widget follow it.
- [x] Settings → Tracking → Home buttons, and "Edit Home buttons" in the More sheet.
- [x] Bottle: contents on the amount screen; the default amount is the previous bottle's.
- [x] Whole-branch review: three fixes and four minors, all done.
- [x] Verification: `make test` OK 1798 on the merged tree; UI subset 23/23 in a run with the simulator to itself.
- [x] Installed in place on Phone A and Phone B from `34cd4455`. Not launched.
- [ ] User check on the phones: the six widget items in the rollout note, and dragging rows in Home buttons.

# Entry panels (2026-10-09)

Spec: docs/superpowers/specs/2026-10-09-entry-panels-design.md
Plan: docs/superpowers/plans/2026-10-09-entry-panels.md
Verification: docs/verification/2026-10-09-entry-panels/README.md
Rollout: docs/verification/2026-10-09-entry-panels-rollout.md
Merged to `main` at `296312a7`. Client only, so no server deploy.

- [x] Tasks 1–9: Start + End with a read-only Length for every interval; one panel for log and edit; Past entry folds into it. The running-timer editor shows Start/End for each finished part, and every sheet fits its content (`sheetHeight` removed). Each task was reviewed.
- [x] Task 10: simulator screenshots of every panel, at default size and accessibility-large. The bottle part's − and + now sit beside the amount, so it fits without scrolling. With Details open, a focused note stays visible above Save.
- [x] Final whole-branch review: one fix wave. A Past entry bottle no longer stores an unentered 10-min length. The running editor's VoiceOver labels name each part. A legacy 0 s breast part refuses Start moved past End. Dead code is removed.
- [x] Verification: `make test` OK 1700 on the merged tree. UI subsets pass with no runner restarts.
- [x] Installed on Phone B and Phone A. Both relaunched and sync 200s on the server.
- [ ] User check on the phones.
- [ ] Parked minors:
  - unused catalog strings from Past entry and the duration stepper;
  - "Bottle" shown twice in the running editor;
  - stale accessibility-audit `statedHeight` allowances and comments;
  - the running editor's "parts add up to more time" message used for End-before-Start and overlap (unreachable today);
  - an open legacy sleep shows End = Start;
  - the session sheet jumps ~34 pt once when it opens;
  - iPad split-screen sheet sizing untested;
  - a bottle with an End, or with Details open, scrolls under Save.

# README with preview screenshots (2026-10-05 → 2026-10-09)

- [x] README.md: intro, feature list, layout, build/test commands (`5022a63a`).
- [x] Screenshots retaken after the baby-icons/Insights redesign: Home, running feed, History, Insights from the simulator demo fixture (demo baby renamed "Marc" only for the capture, reverted).
- [x] Ask screenshot with mockup answers matching the demo log, labelled as mockups in the README (`141e45d7`). Live on-device answers miscounted naps, so they were not used.

# Baby icons and shared switcher (2026-10-08 → 2026-10-09)

Spec: docs/superpowers/specs/2026-10-08-baby-icons-switcher-design.md
Plan: docs/superpowers/plans/2026-10-08-baby-icons-switcher.md
Rollout: docs/verification/2026-10-09-baby-icons-rollout.md

- [x] Icon art redrawn (v3) after the user rejected v1; independent visual review; user approved.
- [x] Tasks 1–8 done subagent-driven, each with its own review. Final Opus review: I1 deploy order, I2 launch fill-in before pull. Fix wave re-reviewed clean.
- [x] Verification: `make test` OK 1673 after merging main; simulator end-to-end screenshots; protocol-e2e 15/15. Live Activity composition timing tests flake under load on main too; quiet 3/3 on both.
- [x] Server deployed `fce422d5` (schema 29) before the merge; main ff → `7040ca2d`.
- [x] Both phones installed in place from `fce422d5`. This also delivers History PDF export from main. Server shows boy.1 on the baby with Boys set.
- [ ] User check on the phones. Set Boys/Girls on the second baby (no sex set, so it shows the neutral face).
- [ ] Parked minors: spec/doc-comment wording says "sync succeeded since launch" but the code gates on the last sync attempt; one older spec §2 bullet.
- [ ] Optional follow-up: the History PDF export entry below can tick its phone install.

# History PDF export and daily bottle total (2026-10-08)

Spec: docs/superpowers/specs/2026-10-08-history-pdf-export-design.md
Plan: docs/superpowers/plans/2026-10-08-history-pdf-export.md
Baseline `make test` → 1622. Fast-forwarded to main `6546a98c`; `make test` on main → OK 1647.

- [x] Tasks 1–7 executed inline.
- [x] Final review (Opus): headings not repeating on later PDF pages, stale "Nothing logged" after a range change, and (own screenshot) header buttons overlapping at the largest text size — all fixed in d7a4e8d4.
- [x] Verification: `make test` → OK 1647; HistoryFeatureTests on the shared simulator → 92 passed (incl. multi-page + aligned running headings); HistoryRedesignTests UI → 3 passed (export reaches the share sheet with a PDF and Print); app build succeeded; exported PDF and History header (default and AX-XXXL) inspected by screenshot.
- [x] Merged to main (ff, user-approved 2026-10-08).
- [ ] Install on both phones and physical check (user).

# Automatic family Live Activity push (2026-10-03)

- [x] Spec and implementation plan approved.
- [x] Execute 12 tasks with subagent-driven task reviews; ledger `.superpowers/sdd/2026-10-03-family-live-activity-push/progress.md`.
- [x] Whole-branch verification/review and rollout runbook.
- [ ] Separate signed-device and production rollout approval.
# Insights redesign (2026-10-04)

- [x] Brainstorm with visual companion: purpose (on track, trends, doctor), Swiss PEZZ as the only norm, feeding + sleep detail, Week/Month/All, no export, Growth merged into Insights.
- [x] Approved layout A (one scroll) + G1 growth section + full-screen states; metric definitions and integration approved in chat.
- [x] Spec + mockups committed `7263af8`: `docs/superpowers/specs/2026-10-04-insights-redesign-design.md`, `mockups/2026-10-04-insights-redesign/`.
- [x] User approved the spec ("let's implement", 2026-10-06). Spec amended for Ask's tool-calling rewrite and Charts limits.
- [x] Plan committed `87fbbaab`: `docs/superpowers/plans/2026-10-06-insights-redesign.md` (10 tasks).
- [x] User picked subagent-driven execution (2026-10-06); Ask no longer links Insights → spec/plan amended (`73e27864`).
- [x] All 10 tasks implemented and reviewed on `feat/insights-redesign` (`.worktrees/insights-redesign`, head `7a9294fe`); final whole-branch review + one fix wave, re-review clean.
- [x] Verification: `make test` OK 1612; app build OK on shared iPhone 17 Pro; UI subset 9/9 + final targeted 3/3; localization gate empty; uk AX5 captures inspected (`docs/verification/2026-10-06-insights-redesign.md`).
- [x] Merged to main (ff, `7a9294fe`); main installed in place on both phones at user's choice, incl. the not-yet-rolled-out Live Activity push client (`f04ec658` records it).
- [ ] Physical check of Insights on the phones (user).

# Automatic family Live Activity push (2026-10-03)

- [x] Begin Superpowers architectural design; inspect ActivityKit card creation, foreground family sync, server capability discovery, and current Apple ActivityKit push documentation.
- [x] Confirm current cards request no push token; remote timer creation is foreground-gated and no APNs entitlement exists in the app entitlement file.
- [x] User confirmed remote start, update, and end while the app is closed; then requested automatic behavior on their server with no in-app setting (2026-10-03).
- [x] Write and self-review `docs/superpowers/specs/2026-10-03-family-live-activity-push-design.md`: automatic eligibility for the built-in server identity/capability, durable delivery, token lifecycle, presentation parity and stale-action protection.
- [x] User approved the written spec on 2026-10-03 ("Approved").
- [x] Write and self-review `docs/superpowers/plans/2026-10-03-family-live-activity-push.md` (12 tasks); verify source seams and Apple push requirements. Clarify per-device break limits and preserve current usual-volume Lock Screen bottle completion.
- [x] User approved the implementation plan on 2026-10-03; proceeding with recommended subagent-driven execution.
- [ ] Execution underway in `.worktrees/family-live-activity-push` on `feat/family-live-activity-push`; task evidence and reviews are in its `.superpowers/sdd/2026-10-03-family-live-activity-push/` ledger.
- [x] Planning verification: document links, 12 sequential tasks, shared interfaces, spec coverage, placeholder scan and whitespace checks; no product tests/builds or external-system changes in this pass.
- [ ] Implement automatic registration, capability-gated delivery, token lifecycle, durable server delivery and regression coverage in an isolated worktree.
- [ ] Verify local behavior and signed two-device APNs delivery separately; production configuration/deployment requires a separate authorized rollout.

# Single-node sync capacity and MCP (2026-10-03)

- [x] Reconcile current source and prior capacity evidence; previous matrix did not reach measurement and establishes no supported client count.
- [x] Confirm read-only history/summaries for local desktop MCP clients over Tailscale; target the existing Unraid node. Exact desktop compatibility remains a verification gate.
- [x] Complete source audit and current MCP primary-source research; inspect Unraid hardware/container limits read-only. Proposed design: `docs/superpowers/specs/2026-10-03-single-node-capacity-mcp-design.md`; research: `docs/research/2026-10-03-mcp-server.md`.
- [x] Self-review the proposed design and verify documentation links/whitespace. No product code, tests, load generation or deployment in this pass.
- [x] User approved the written design on 2026-10-03 ("good").
- [x] Write and self-review two implementation plans: `docs/superpowers/plans/2026-10-03-single-node-capacity.md` (7 tasks) and `docs/superpowers/plans/2026-10-03-private-readonly-mcp.md` (8 tasks). Current server source still matches the audit; intervening commits concern History UI documentation/mockups.
- [x] Local documentation commit `5b80a71` contains only the design/research/plans. Documentation links, plan structure and whitespace checks passed; no product tests or benchmarks run during planning.
- [x] User approved both implementation plans and selected subagent-driven execution.
- [ ] Execution underway in `.worktrees/single-node-capacity-mcp` on `feat/single-node-capacity-mcp`; per-task progress/reviews are in that worktree's `.superpowers/sdd/2026-10-03-single-node-capacity/` and `.superpowers/sdd/2026-10-03-private-readonly-mcp/` ledgers.
- [ ] Implement and verify capacity improvements and MCP using disposable synthetic data; report measured limits and mixed-workload impact.
- [ ] Production rollout remains a separate decision after reviewable local results.

# Full UX redesign + session continuation (2026-10-01)

- [x] Map current screens, icons, design system, and all feed/sleep combining paths (sleep has none; feed combine only after in-app Stop).
- [x] Agree on decomposition, continuation behavior, and Home/tracking design (visual mockups in `mockups/2026-10-01-home-redesign/`).
- [x] Write and self-review the spec; committed as `46c708a` (`docs/superpowers/specs/2026-10-01-home-tracking-redesign-design.md`).
- [x] User approved the spec; implementation plan committed (`docs/superpowers/plans/2026-10-01-home-tracking-redesign.md`, 20 tasks).
- [x] Subagent-driven execution: 20 tasks + 18b/18c/19b, task reviews, whole-branch review and one fix wave (C1, I1–I5, M1–M9).
- [x] Fast-forward main to `2961ca1`; deploy sync server `2961ca1` to Unraid (schema 22 → 26), see `docs/verification/2026-10-02-home-redesign-rollout.md`.
- [x] Clean reinstall on the second caregiver's iPhone.
- [x] Install on Phone A (in-place update 2026-10-02). Locked-iPhone Live Activity check still open.
- [ ] Follow-up B: full format cleanup + join past entries into sessions (workspace `.superpowers/sdd/2026-10-02-format-cleanup/`).
  - [x] Design notes + spec + plan; 11 tasks executed with per-task reviews, server-track and client-track final reviews.
  - [x] Server track deployed 2026-10-03 02:29Z: `1313f62`, schema 27 (two production dry-runs PASS; 115 heads, 25 joined, 14 moved with their sessions).
  - [x] Client track merged to main `1fe8ac4`; all gates green (test 1247, build, UI 145/0, protocol e2e 14/14); signed build ready.
  - [ ] Install day (runbook Phase C, combined with the History redesign): owner phone uninstall → install → sign in → counts/round-trip/relaunch checks → then second caregiver phone.
- [x] History redesign (spec `docs/superpowers/specs/2026-10-03-history-redesign-design.md`): 7 tasks + final review + fix wave, merged to main `19fe39e`; verification `docs/verification/2026-10-03-history-redesign.md`; signed build ready.
- [ ] Later sub-projects, one by one: Insights/Growth, Ask, Settings/Caregivers/Export.

# Growth measurements and WHO insights (2026-09-30)

- [x] Agree on Home measurement entry, the Insights weight card, and Growth details layout.
- [x] Agree on dated check-ins, WHO reference sex, corrected age, on-device percentiles, and family sync behavior.
- [x] Draft and self-review the architectural design.
- [x] Obtain review of the committed written spec; clarify local-only versus shared-family deletion.
- [ ] Write and review the implementation plan, then choose an execution method.
- [ ] Implement, verify, and review the feature and bundled reference rights.

# Growth integration to local main (2026-09-30)

- [x] Reconcile newer main commits and preserve timer pause at migration 23; Growth follows at migration 24.
- [x] Review merge resolutions and adapt the new timer fixture to required Growth profile arguments.
- [x] Reproduce the Swiss derivative from the original ignored supplement; all 15 generator tests pass.
- [x] Verify combined package/server/build/protocol gates and record results: 1108 Swift tests, 15 generator tests/source parity, full server suite, schema-24 protocol and simulator/UI harness compilation.
- [x] Advance local main to the verified merge `47ac2a6`; preserve unrelated edits outside the committed tree using scoped recovery copies, with final restoration checks reported at integration completion. Keep untracked files and original source worktree.
- [ ] Runtime/device/production acceptance remains open; no push or deployment authorized.

# Feeding pause control (2026-09-30)

- [x] Add Pause beside Stop and + while a breast interval is running; keep the feed open for breast or bottle resume.
- [x] Persist exact breast end time locally and add the v4 family timer pause command with schema 23.
- [x] Verify focused Swift package tests, Go protocol/records/migrate tests, the full Go suite, the database-backed pause test, and the signed simulator UI flow.
- [ ] Full Swift package suite reports failures in Invitation QR, family sync logging, and Ask backup-exclusion tests; investigate separately.
- [ ] Roll out combined schema 24 (timer pause at 23, Growth at 24), the server, and an updated iOS app after deployment approval.
- [x] Merge Growth after timer pause; preserve `000023_timer_pause.sql` and renumber Growth to `000024_growth.sql`, updating the exact schema compatibility manifest.

# Ask tab with Apple Foundation Models

## Frequent “Catch me up” retry error (2026-09-29–30)

- [x] Reproduce the screenshot's retry path with an opt-in synthetic Foundation Models check for the exact prompt; the original model route returned ungrounded output after three attempts.
- [x] Route the handoff suggestion through bounded 24-hour care-log totals and running timers; verify a grounded answer and source links on synthetic data.
- [x] Pass the final exact-question live-model check with completed entries and a running timer, the 1,000-test package gate, iOS Simulator build, and Ask-only diff check.
- [ ] Verify the new app on an iPhone against the user's private log. If Ask still fails, inspect the new privacy-safe stage/domain/code log; one synthetic run hit an intermittent Apple model guardrail refusal.

## Ask live and recent context (2026-09-29)

- [x] Review the current Ask, timer, and log-query data paths; agree on live state plus 48-hour context design.
- [x] Add fresh running timer facts and source-linked recent timeline, including overlapping sleep and in-progress feed steps.
- [x] Keep live context in record prompts and route present-state questions to records.
- [x] Extend bounded older-history retrieval to explicit dates and entry pages.
- [x] Run focused tests, the 996-test package gate, simulator and signed device builds, and final diff review; install on both paired iPhones without launching them.
- [ ] On a phone with Apple Intelligence available, check live answers and older-date tool calls against the visible log; neither installed app was launched by this work.


- [x] Agree on question-first Ask tab, handoff/log/Insights scope, one persistent chat per child, and private device-only history.
- [x] Write and self-review the architectural design.
- [x] Obtain review of the written design and draft the implementation plan.
- [x] Review the implementation plan and choose an execution approach.
- [x] Implement, verify, and review the Ask feature against the approved plan.

# One-year self-hosted session ceiling

- [x] Reproduce that a previously issued 100-year session still refreshes past one year if only the default changes.
- [x] Cap new and existing self-hosted session lineages at one year from the original sign-in without requiring an immediate sign-in.
- [x] Verify the schema 20 migration, auth regressions, full server race/PostgreSQL suite, `go vet`, amd64/arm64 build, and disposable migration/readiness smoke check.
- [x] Back up and roll out server revision `6aa153d` with schema 20; verify private HTTPS, owner session's effective 2027-09-28 deadline, unchanged record totals, and clean bounded logs.

# Persistent self-hosted sign-in incident

- [x] Preserve and integrity-check a restricted owner iPhone App Group backup before reauthentication; compare local and server record counts read-only.
- [x] Confirm the phone remains in the shared family session and the server's 24-hour absolute session deadline caused the sign-in prompt.
- [x] Reproduce the 24-hour refresh failure with disposable PostgreSQL, then cover refresh beyond one day and the distant protocol ceiling.
- [x] Replace the daily hard deadline with a long-lived self-hosted session; full server race/PostgreSQL suite, `go vet`, multiarchitecture server build, and 70 focused Swift tests passed.
- [x] Review the scoped diff and signed-commit the fix as `a81f86c`.
- [x] Preserve a fresh restricted production database/source/configuration rollback, deploy only the app service at `a81f86c`, and verify the live source revision, schema 19, private HTTPS health/readiness, isolation, unchanged records, and clean bounded logs.
- [x] Catch the remaining one-year unused-device deadline with a failing two-year inactivity regression; remove it to match one-time sign-in.
- [x] Verify and deploy the final no-inactivity-deadline revision `90b6a34` with a fresh rollback and post-rollout checks.
- [x] The owner signed in with Apple and reported History looks right. Read-only server checks confirmed a new active long-lived owner session and post-sign-in record sync; both feed tombstones matched deletions already queued in the pre-sign-in phone backup.

# Short breast segment cleanup

- [x] Remove breast segments under 30 seconds at Stop, combine adjacent surviving segments on the same side, and preserve bottles.
- [x] End an all-short feed without a record, including when the shared timer start was already sent.
- [x] Keep iPhone and server completion behavior aligned through an explicit v2 stop flag; leave older clients' stops unchanged.
- [x] Verify focused Swift tests, full Swift suite, Go unit suite, and disposable PostgreSQL integration test.
- [x] Finish full database race suite and local server/client protocol gate; both passed on the final server code.
- [x] Add the app-language Settings shortcut and Ukrainian translations in all 13 catalogs; static placeholder audit and simulator build passed.
- [x] Review forced-Ukrainian Home, History, Insights, and Settings screenshots on a compact iPhone 17e simulator; fix the cramped Home status row and pass the dedicated UI test.
- [x] Verify the shortcut opens iPhone Settings on the simulator and show a localized path to Baby Tracker's language option; the simulator landed on Settings home rather than the app page.
- [ ] On an unlocked physical phone, verify whether iOS lands directly on Baby Tracker's language option.
- [x] Review and fast-forward merge to local main (`9ba399d`), then commit the verified Ukrainian layout and Settings follow-up (`90ae8cf`); final signed iPhone build passed verification.
- [x] With explicit approval, back up the live database/source/configuration, provision export credentials, migrate schema 18 to 19, deploy server `9ba399d`, and verify private health/readiness, socket, container isolation, and clean recent logs.
- [x] Install the final signed update on both iPhones without replacing app data. The owner's final build launched; the second caregiver's final launch check was denied because their phone was locked.

# History filters, family confirmation, and export repair

- [x] Distinguish selected History filters with a tinted surface and checkmark; keep unselected filters neutral.
- [x] Replace the spacious leave-family confirmation layout with a compact, scrollable sheet and clear action buttons.
- [x] Reproduce the live export HTTP 503 before job creation in a disposable PostgreSQL test: new export limiter scopes were not mapped to accepted storage scopes.
- [x] Map export limiter scopes to the existing isolated storage namespaces; focused regression passes.
- [x] Pass affected Go suites (`limits`, `httpapi`, `export`), `go vet`, 46 focused Swift tests, iOS Simulator build, and scoped diff review.
- [x] Pass three targeted simulator UI tests and inspect History and leave-family screenshots; shorten the sheet's initial height after the first visual read-through. The first unsigned UI attempt stalled, then the normal signed run passed.
- [x] With explicit approval, preserve restricted database/source/configuration backups, deploy server `d3ccf22`, verify schema 19, exact image revision, private health/readiness, socket routing, isolation, unrelated-container baseline, and clean bounded logs.
- [x] Install the signed app and widget update on both iPhones without replacing app data; owner launch succeeded and both installed-bundle queries succeeded.
- [ ] Device-owner check: the second caregiver's phone blocked remote launch while locked; the user declined further agent-driven device verification.
- [ ] User check: an authenticated owner export reaches `ready`, downloads, and shares on the installed app. The user elected to test it on their own phone; the unauthenticated route check returned 401 and does not prove this flow.

# Supplements

- [x] Inspect existing logging, data, and sync flows.
- [x] Write design and implementation plan.
- [x] Server protocol and persistence. Go and disposable PostgreSQL race suites passed across the full run and affected package reruns; scoped review approved.
- [x] SwiftData model and sync. Full Swift suite passed; both Important review findings fixed and re-reviewed.
- [x] Home sheet and History UI. Simulator build and two UI tests passed; 783-test literal gate passed; review approved after History validation fix.
- [x] End-to-end verification: local two-caregiver TLS gate passed after a deterministic timestamp regression and fix; 785 Swift tests, 1,656 Go tests, simulator build and 2 UI tests passed.
- [x] Whole-branch review and compatibility fix. The old-client cursor issue was fixed and re-reviewed; full database race, local protocol, and final simulator build gates passed on `4f4fea3`. Branch handoff pending.

# Siri commands

- [x] Approve command specification: `docs/specs/2026-09-27-siri-commands.md` (nine commands, including sleep).
- [x] Implement direct App Intents with result dialogs; existing widget and Live Activity actions remain in place.
- [x] Register nine App Shortcuts; built metadata contains all nine phrases and the `Baby Tracker` app name.
- [x] Run focused tests and package gate (`OK: 842 tests`), plus simulator build (`xcodebuild: ok`).
- [ ] Verify spoken phrases, responses, locked-phone operation, and History/Live Activity results on a physical iPhone with test data.
- [x] Review scoped diff and record device-only limitations in `docs/verification/2026-09-27-siri-commands.md`.

# Status widget and running-timer summary

- [x] Inspect the small Home Screen status widget and running Home state.
- [x] Show recent feed, sleep, and diaper rows under a running timer, excluding history for each active timer kind; three focused cases passed.
- [x] Enlarge and distribute the small status widget's content within its height; add the baby's name and spoken row labels.
- [x] Run the full package gate (`OK: 845 tests`) and simulator build (raw Xcode exit 0).
- [x] Inspect focused simulator UI read-throughs and widget gallery: all three signed UI tests passed; the preview exposed a baby-name contrast issue, fixed with adaptive text color; widget gallery retest passed.
- [x] Review and fast-forward the scoped diff into local `main` (`a17ea84`); signed device build and iPhone installation succeeded.
- [x] Confirm device launch after the iPhone is unlocked (`devicectl` launch succeeded).
- [ ] Verify widget appearance, running summary, and Siri phrases with the user's real data.

# Baby switching state

- [x] Reproduce the stale first-baby timer after adding a second baby in a signed simulator UI test.
- [x] Make selection observable and remount baby-bound tabs when the selected baby changes; refresh widget timelines.
- [x] Run 845 package tests and two signed simulator UI regressions for switching and deletion.
- [x] Fast-forward the fix into local `main` (`c1552f7`), build and install it on `Phone A` and `Phone B`, and launch successfully on both.
- [ ] Verify switching between the user's actual babies, including Home, History, Insights, and the status widget.

# Feed sheet and family sync recovery

- [x] Trace the Feed choices flash to `.sheet(item:)` replacing the whole modal when a choice changes its item identity.
- [x] Keep one modal presentation while switching between Feed choices and its editors; signed simulator Bottle save passed.
- [x] Inspect the second caregiver's Caregivers status and live server health: the screen requires Apple sign-in, and the reachable server returned repeated sync 401s.
- [x] Reproduce the sign-in row disappearing on the next sync, then keep the sign-in-required state stable until fresh proof arrives.
- [x] Rebuild the sync engine after successful Apple sign-in even if the family key is unchanged.
- [x] Run final package (`OK: 847 tests`), signed Bottle UI (1 passed), and signed iPhone build checks; review the scoped diff.
- [x] Merge to local main (`735bd3e`) and install on both iPhones; the user's app launched, while iOS blocked the second caregiver's launch check because that phone was locked.
- [ ] Confirm the second caregiver can complete Apple sign-in and that Caregivers shows a successful sync with fresh live evidence.
- [x] Live follow-up: Apple sign-in returned 200 and subsequent sync returned 200; the second caregiver's screen says Synced. A family read returned 401 during recovery, consistent with their generic error banner.
- [x] Read-only timer comparison: live server has one active sleep timer and no active feed timer; the user's phone reached the server's latest journal cursor but had queued timer mutations in a temporary local-store inspection. The temporary copy was removed.
- [x] Confirm whether both screens show the same baby and whether the user's feed timer is in the app or only the widget/Live Activity; check the user's Caregivers status before changing timer sync code.
- [x] Confirm both phones show the same baby, both Caregivers lines say Synced, and the feeding timer is inside the user's app. The second caregiver's generic error appeared immediately after Apple sign-in.
- [x] Reproduce an accepted sign-in surfacing a 401 from caregiver details, then activate and renew before loading those details; focused recovery tests pass.
- [ ] Confirm whether the user's running feed remains after leaving and returning to Home. The server has since accepted queued timer starts and stops (journal 92 to 106), leaving no active timer, so the current live display needs a fresh check.
- [x] Run the final package gate and signed iPhone build for the sign-in ordering fix; this build is installed on the owner's phone with the recovery visibility fix. The second caregiver's phone was not changed during the owner-only restore.

# 2026-09-27 sync data incident

- [x] Inspect production family membership, aggregate record counts, and sync outcomes without changing server data.
- [x] With explicit user approval, inspect temporary read-only app-group database copies from both phones; confirm integrity, record visibility, and cursors without opening notes or names.
- [x] Remove all six temporary database copy files and their diagnostic directory after inspection.
- [x] Confirm recent locally present feed and sleep completions are hidden by tombstones and absent at those times on the server. Recent server receipts include nine superseded timer starts; the client withdraws dependent completions when a start is superseded.
- [x] Confirm caregiver leave succeeded on the server before logout returned 401; local realm cleanup was skipped, leaving the second caregiver's phone in a stale shared mode.
- [x] Prepare and review a lossless recovery procedure; user authorized restoring only the eight hidden records from their own phone, leaving the second caregiver's three phone-only rows preserved and unchanged.
- [x] With renewed approval, preserve restricted copies of both phone stores and a server archive; both SQLite integrity checks, server archive validation, and SHA-256 checks passed.
- [x] Compare record identities across the three sources: all visible phone records exist on the server; 11 phone-only hidden timer completions have superseded stop commands and no delete commands. Nine do not overlap visible server history; two second-caregiver feeds overlap existing entries.
- [x] Write a detailed additive restore preview outside the repository at `/path/to/baby-tracker-recovery-20260927/restore-preview.md`; no restore or membership mutation performed.
- [x] User chose their own records only; all three second-caregiver-only hidden rows remain preserved as recovery evidence.
- [x] Restore four original-ID feeds and four original-ID sleeps through the server repository in one additive transaction after a rollback-only production dry run and fresh validated archive. All pre-existing record IDs and tombstone states remained unchanged.
- [x] Repair live-canonical rematerialization and launch-time reconciliation; focused red/green tests, 851-test suite, signed iPhone build, install, launch, and final read-only phone snapshot all passed. The eight original IDs are visible on the user's phone; sync cursor matches the server journal.
- [x] Reproduce superseded timer starts hiding completed feeds and sleeps; preserve each distinct completion as a normal record create under its original ID. Focused red/green tests passed for both kinds.
- [x] Reproduce successful server leave followed by failed logout; finish local cleanup without a second authenticated request. Focused red/green test passed.
- [x] Run the full package count gate (`OK: 855 tests`), signed physical-device build, and scoped diff review.
- [x] Fast-forward the verified fix to local main (`aa397ce`) and install its signed build on both paired iPhones. The owner's app launched; iOS refused the launch check on the second caregiver's locked phone.
- [ ] Verify fresh sync and history on both phones after the second caregiver signs in and rejoins.
- [x] After the user said the second caregiver's local database need not be preserved, uninstall and reinstall the signed recovery build on their iPhone; retain the restricted diagnostic copy. Installation succeeded, but launch verification was blocked because the phone was locked.
- [ ] Have the second caregiver unlock and open the app, sign in with Apple, accept a fresh caregiver invitation from the owner, and verify both phones show the same family history and active timer.

# Per-baby Home Screen widgets

- [x] Trace Status and Quick Log widget data and button targeting.
- [x] Add a Baby choice to each widget and keep status and quick actions bound to its configured child.
- [x] Carry a configured Quick Log Feed tap into the app's feed menu for that child.
- [x] Confirm a deleted child does not fall back to the app's selected child.
- [x] Run final package suite and simulator build after the last edits (`swift test` and `xcodebuild ... build`, both exit 0).
- [ ] Verify widget configuration and child switching on an installed iPhone.

# German, French, Spanish, and Italian localization

- [x] Confirm scope: translate app, widgets, and Shortcuts in all four languages with English fallback.
- [x] Isolate work on `feat/localizations` in `.worktrees/localizations` and inspect package/app text ownership.
- [x] Add 13 target-specific catalogs with German, French, Spanish, and Italian copy for the app, widgets, Shortcuts, and camera permission; localize formatted text and decimal separators.
- [x] Check translated catalog keys and placeholder parity; run the Swift package suite and unsigned simulator build; inspect all four Home screens on a temporary signed simulator.
- [x] Review the full diff and fix singular Insights copy, locale-specific duration units, and localized widget preview ages. Repeat the full package suite and simulator build after those fixes. Native-speaker review, deeper screen flows, spoken Shortcuts, widget appearance, and installed-device behavior remain to be checked.
- [x] Reconcile the concurrently merged family export feature and translate its new account, export, and error copy in all four languages; 579 catalog keys now covered.

# Localization language review

- [x] Review German, French, Spanish, and Italian catalogs independently across app, widgets, Shortcuts, and export copy.
- [x] Fix context-dependent breast and bottle phrases, action labels, singular counts, role names, gendered statuses, and inaccurate warnings found in review.
- [x] Recheck 589 translated catalog keys, placeholders, source lookups, and compiled resources; run the 860-test Swift suite and iOS Simulator build after the last edits; review the final diff.

# Owner family data export

- [x] Confirm the existing design calls for an owner-only JSON family archive and inspect the current schema 18 implementation seams.
- [x] Create an isolated `feat/account-data-export` worktree; preserve the unrelated main-checkout `TASKS.md` edit.
- [x] Implement schema 19 export jobs, authorization, encrypted temporary storage, expiry, and archive generation; focused disposable PostgreSQL gate passed.
- [x] Add export HTTP routes and protocol capability with tests.
- [x] Add an owner-only export flow in Account settings and private temporary file handling; focused tests and simulator build passed.
- [x] Run the full Go race suite against disposable PostgreSQL, focused export and HTTP integration tests, all 860 Swift package tests, iOS Simulator build, Docker smoke, protocol end-to-end, Go vet, and diff checks. The final full Go suite passed.
- [x] Verify create, worker encryption, status, and JSON download through real HTTP routes backed by disposable PostgreSQL.
- [ ] Verify export through an export-enabled running server and installed iPhone before claiming live delivery; no production mutation has been made.

# Native Ask tab

- [x] Add the fifth Ask tab with a question field, handoff/log/Insights prompts, source links, and a confirmed New Chat action.
- [x] Keep one app-private, device-only conversation per child and caregiver identity; exclude chat files from backup and keep them out of sync and family export.
- [x] Ground answers in current read-only log facts and existing Insights figures; handle missing model, changed records, and late responses after deletion.
- [x] Verify 902 Swift package tests, the iOS simulator build, and four focused Ask UI flows.
- [x] Verify the complete UI suite on the final tree: 87/87 simulator UI tests passed on a clean iPhone 17 Pro simulator install.
- [x] Complete separate Standards and Spec whole-branch reviews; fix all seven reported findings plus offline tombstone cleanup, prior-day Insights detection, short-duration formatting, and live baby-name/unit updates.
- [x] Verify the reviewed tree with 910 Swift package tests, an iOS simulator build, and focused Ask UI cases.
- [x] Finish the complete simulator UI suite on the reviewed tree: 89/89 tests passed on the iPhone 17 Pro simulator.
- [x] Fast-forward the reviewed branch into local `main` at `b46f721`; the merged checkout passed 910 package tests and the iOS simulator build.
- [ ] On an unlocked iPhone with Apple Intelligence enabled, verify actual on-device availability and answer quality using non-sensitive fixture data; inspect runtime network evidence before claiming a device-only run.

# Ask conversation redesign

- [x] Inspect screenshot, existing model pipeline, fact retrieval, chat view, and tests.
- [x] Create isolated worktree; baseline `make test`: 910 tests passed.
- [x] Replace the narrow question classifier with flexible, bounded context retrieval and grounded prose answers.
- [x] Improve the transcript layout, source links, multiline composer, loading, and error states.
- [x] Add focused model and simulator UI coverage for natural follow-ups and the rendered chat.
- [x] Review the whole diff; run package tests, iOS build, and relevant simulator UI tests.
- [x] Integrate verified changes into local main and clean up the worktree. Signed update installed on both paired iPhones without launching either app; physical UI and real-model behavior remain unverified.

Scope decision: Ask supports both log questions and general parenting or organization questions, with general answers visibly labeled and kept separate from private records.

# Timed bottle and editable sleep end

- [x] Confirm user intent: independent completed-sleep end correction and start/stop bottle timing like breast; preserve manual bottle logging.
- [x] Inspect existing sleep editor, feed timer, bottle components, and server protocol; create isolated worktree and implementation plan.
- [x] Implement and test independent sleep end editing; History suite passed.
- [x] Implement v3 timed bottle sync/storage compatibility, including old-client cursor protection and v4-to-v5 SwiftData migration; focused tests passed.
- [x] Implement iOS timed bottle timer, Home and History UI, finish sheet, and Live Activity; package tests and a simulator build passed without launching the app.
- [x] Complete whole-branch Standards and Spec reviews, fix four timer and sheet issues with focused coverage, and prepare local-main integration. The final 942-test Swift package gate, iOS Simulator build, full disposable PostgreSQL race suite, Go vet, and linux/amd64 plus linux/arm64 server builds passed. The app was not launched. A real store created by the prior v4 binary migrated to v5 with its timer intact.
- [x] With explicit approval, deploy schema 21 and server revision `20f2481` to Unraid, then install the signed build on both paired iPhones without launching either app. Private health/readiness and socket verification passed; server identity and record counts were unchanged.

# Ask log retrieval and rolling-window questions

- [x] Reproduce why a natural question such as bottle millilitres in the last 24 hours receives only calendar-day Insights totals.
- [x] Give the on-device model a bounded, read-only native log query tool and prefetch explicit periods; keep records on device and preserve source links.
- [x] Verify rolling-window totals, follow-ups, missing-data language, and source-linked rendered answers with focused tests; full package gate passed (954 tests).
- [x] Finish iOS build and diff review, fast-forward to local `main` at `65680a6`, then install the signed update on both paired iPhones without launching either app.

# Model-authored Ask answers

- [x] Reproduce fixed answer templates and synthetic summaries replacing the model's answer.
- [x] Keep bounded local log retrieval and exact aggregates as context while requiring model-authored answer text.
- [x] Reject ungrounded numeric claims and preserve the user's question for retry instead of writing an app-authored answer.
- [x] Complete 947 package tests, two synthetic live-model checks, iOS Simulator and signed device builds; review and fast-forward to local `main` at `abfdae9`, then install on both paired iPhones without launching either app.
# Feeding sessions and editable step times

- [x] Confirm suggested Combine after timer Stop, with separate diaper records and editable start/end for bottle and breast steps.
- [x] Inspect feed components, timers, History editing, counts, and sync; write the design specification.
- [x] Review the written specification with the user, then prepare the implementation plan.
- [x] Review the implementation plan with the user before starting product changes; approved in this session.
- [x] Implement, verify, review, and fast-forward the client/server changes to local `main` at `b25a8f2`. The 971-test Swift gate, full server suite, live two-caregiver protocol proof, simulator build, and signed iPhone build passed.
- [x] Preserve restricted rollback artifacts, migrate and deploy schema 22 at `842ab44`, verify private health/readiness, socket, isolation, exact image revision, unchanged record totals, and clean bounded logs; install the signed app on both paired iPhones without launching either. See `docs/verification/2026-09-29-feeding-session-rollout.md`.

# Ask answer-quality repair

- [x] Reproduce the combined-feed context disagreement and the live-model greeting context overflow with synthetic data.
- [x] Align Ask's daily and weekly feed facts with feeding sessions and bottle-step timestamps; reduce broad-query prompt noise and reject internal fact-label prose.
- [x] Verify synthetic live-model greeting, yesterday summary, and rolling bottle-volume questions; the 978-test package gate, iOS Simulator build, and final diff check passed.
- [x] Fast-forward local main to `b2a13f7`, verify the signed device build, and install the repair on both paired iPhones without launching either app. See `docs/verification/2026-09-29-ask-answer-repair.md`.

# Ask latest-entry repair

- [x] Reproduce the failed “When was the last time we changed diaper?” path: no direct latest-entry query and timestamp-only replies rejected by validation.
- [x] Add a bounded latest-entry query, timestamp grounding, missing-entry handling, and synthetic live-model coverage for the exact wording.
- [x] Pass the 982-test package gate and iOS Simulator build, fast-forward local main to `acbf1e3`, verify the signed device build, and install it on both paired iPhones without launching either. See `docs/verification/2026-09-29-ask-latest-entry-repair.md`.

# Feeding History mobile layout

- [x] Review the cramped combined-feeding screenshot and approve the compact expandable timeline design (option A).
- [x] Reproduce the old layout with a failing iPhone 17e UI test; verify compact session and mixed-bottle summaries with red/green model tests.
- [x] Render the expanded session on the iPhone 17e and verify the step actions remain available.
- [x] Pass the 983-test package gate and focused iPhone 17e timeline test; the AX5 History audit completed with its pre-existing expected filter-chip contrast finding. Signed device build, signature, catalog JSON, and diff checks passed.
- [x] Fast-forward the reviewed layout to local `main` at `385f09e` and install the signed build on both paired iPhones without launching either app; both installed-bundle queries succeeded.

# Home feeding card and concurrent timer actions (2026-09-30)

- [x] Align the Home last-feed card and compact mixed feeding summaries with the approved simulator design.
- [x] Allow editing the start of any visible running timer, including a running bottle segment.
- [x] Add a compact + action beside Stop for other logs and concurrent feed or sleep timers; allow either timer to stop independently.
- [x] Pass focused package and iPhone 17e UI tests, including a long Stop label, mixed sessions, and both timer orders.
- [ ] Present simulator screenshots for design approval before installing on paired phones.

# Historical raw Growth verification (2026-09-30; superseded reference status below)

- [x] Assemble raw Growth profile/check-in storage, sync, creation/settings forms, Home logging, and Insights/details UI.
- [x] Strengthen wrong-family isolation, combined measurement edit/delete, concurrent due-date preservation, historical model identity and routed edited-value assertions.
- [x] Pass package (1079 after review fixes), database/race, server build, protocol E2E and simulator build gates; verify compact/AX5 Growth and Settings paths.
- [x] Finish full simulator UI: 107 tests, 10 failed cases; correct and pass all three Growth-related cases in a focused rerun. Seven untouched feeding cases remain likely baseline mismatches (no baseline runtime run).
- [ ] Resolve the seven documented feeding UI test mismatches in a separate scoped follow-up before claiming a green full UI suite.
- [x] Record physical-device availability, migration fixture limits and whole-feature review in `docs/verification/2026-09-30-growth-measurements.md`.
- [x] Superseded source blocker: the previously empty provider/WHO rights investigation was replaced by the approved Swiss S1/S2/S5/S6 implementation below. Head tracking remains raw-only by design.

## Historical raw Growth whole-branch review fixes (2026-09-30)

- [x] Show localized measurement field/date validation and retain invalid input; default position by selected calendar day until explicitly chosen, preserving edits.
- [x] Add red/green model regressions and independent loggedAt assertion; pass the 1079-test package gate and simulator build.
- [x] Verify rendered invalid number/pre-birth feedback and AX5 Settings nondefault due-date value after reopening in focused UI tests.
- [ ] Broader Growth UI attempt remains non-green: an initial test assertion was corrected, but Insights AX5 exited with signal kill; no full UI green claim. Exact runs are recorded in `docs/verification/2026-09-30-growth-measurements.md` and the local final-fix report.
- [x] Superseded pending-data status: Swiss rows, calculator, attribution, golden checks and UI are committed in Tasks 1–3 below; current verification supersedes the historical empty-reference/WHO restriction.

## Swiss Growth implementation (approved 2026-09-30)

- [x] Task 1: verified deterministic Swiss rows, provenance, and bundle resources (e1a34ab; independent review approved; original-source --check passed).
- [x] Task 2: calendar-age Swiss calculator and source tests (8a3e59b; 28 focused tests pass; independent review approved; simulator resource build verified).
- [x] Task 3: Swiss presentation, source info, localization, and UI test implementation (1045b0d; 23 focused Swift tests and UI build pass; independent review approved).
- [ ] Runtime acceptance: iOS Swiss source-info navigation/rendering and head AX are unverified due CoreSimulator failures; partial head SwiftUI render inspected.
- [x] Task 4 verification evidence: fresh 1101-test package gate, 15 actual-source generator tests, deterministic source check, app hashes/resources/exclusion, simulator build, multi-architecture server build, protocol E2E and final full server race/PostgreSQL gate pass. Earlier failures remain recorded in `docs/verification/2026-09-30-growth-measurements.md`.
- [x] Scoped Growth export readiness harness committed `a7b75c5`; synthetic delayed queued-job RED, exact final export package and full server gate pass; no product/export scheduling or snapshot timeout change.
- [x] Independent whole-branch review of `6794ffd..0573afb` completed: no Critical defects; I1 live form source labels, M1 diagnostic disclosure, M2 stale handoff corrections requested.
- [x] Apply final corrections to both Swiss creation/settings selectors and all six locales; extend cross-feature localization regression and affected UI assertions, disclose exact zero-status build diagnostics, refresh handoff and evidence.
- [x] Final correction package gate: `rtk make test` passes `OK: 1101 tests`; focused RED for both form bundles and GREEN 18 affected tests recorded. Final simulator product/UI-harness `build-for-testing` and six-locale cross-catalog/compiled-bundle/placeholder/hash/ZIP exclusion inspection pass. No runtime UI acceptance inferred.
- [x] All four task reviews, independent whole-branch review and scoped correction rereview completed: `f2d3047` addresses I1/M1/M2; final rereview reports all addressed, no new Critical/Important findings or breakage. Final outcome bookkeeping recorded.
- [ ] Current iPhone Swiss source-info/head/routed/AX5 and all-locale rendered acceptance remain unverified (prior bounded UI attempt stalled before tests, exit 143); physical-device/production limits remain open. Historical UI and intermittent server failures remain disclosed. No integration or release decision.

Live Activity push progress (2026-10-03): Task 1 contracts complete and independently reviewed (`599d7ff`, `cc670e8`); Swift 1259 tests pass with isolated TMPDIR, Go contract suite and shared-simulator build pass. Task 2 durable registration/queue implementation next. No production or device acceptance claimed.

Live Activity push progress: Task 2 durable storage/queue complete and independently reviewed (`aee5d1b`, `7482c11`), schema28 local. Complete liveactivity/migrate/database race suites pass; records/importer regressions pass. Review-fixed revocation lock inversion and APNs coordinate transition. Task 3 authentication/dispatch guard next.

Live Activity push progress: Task 3 authenticated endpoints and queued snapshot guard complete/reviewed (`2e94338`). Eight affected server packages passed race suites. Session rotation remains eligible; lifecycle revocation, viewer permissions and lease/fence checks covered. Task 4 canonical presentation next.

Live Activity push progress: Task4 canonical presentation complete/reviewed (`8feaa44`); Swift1261 and affected Go race suites pass,18 cross-language cases and indexed10k-history reads. Task5 APNs transport/config next.

Live Activity push progress: Task5 APNs transport/config complete/reviewed (`78afdfa`, `ea1d2de`). Full affected package race suites pass; actual payload bound, ES256/HTTP2, event semantics and protected worker-file regressions verified. Task6 durable lifecycle scheduling next.

Live Activity push progress: Task6 durable worker complete/reviewed (`aa5f4d4`, `bc0aa27`). Known-unsent recovery and early dismissal review fixes covered; complete worker/migration/database race suites pass, earlier affected suites and Swift1262 pass. Task7 runtime activation next.

Live Activity push progress: Task7 runtime composition complete/reviewed (`c632fdf`, fix `cbfc6a4`): readiness checks kept off the request path, unregister allowed while the worker is stopped, startup APNs latency isolated. Complete cmd/httpapi/database race suites and disposable smoke pass. Local main merged into the branch (`eebae98`, Swift 1416 pass, simulator build pass). Task8 iOS automatic registration in progress.

Live Activity push progress: Task8 iOS automatic registration complete/reviewed (`1245506`, fix `b5611ba`): launch-owned coordinator, App Group ledger with cross-process locking, Keychain-only tokens, provisioning-derived APNs environment (device builds inert until approved provisioning), transient-failure retry. Swift 1491 pass, simulator build pass. Task9 card reconciliation next.

Live Activity push progress: Task9 local/pushed card reconciliation complete/reviewed (`91df2d9`, fixes `3c0381d`, `794ecf7`): provisional push fence before sync, own mutations shown immediately offline, server-derived card stamps (clock-skew safe), retirements never reported as dismissals. Swift 1538 pass, simulator build pass. Task8 coordinator-test load flake being fixed; Task10 stale-safe Lock Screen actions next.

Live Activity push progress: Task10 stale-safe Lock Screen actions complete/reviewed (`2be3022`): preflight validates realm/baby/timer/cursor/authority with bounded catch-up, zero writes and app handoff when unverifiable, usual-volume bottle path preserved. Swift 1563 pass, simulator build pass. Task11 end-to-end composition and full gates next.

Live Activity push progress: Task11 end-to-end verification complete/reviewed (`ee05ab7`, `0b79773`, `e974fa1`, `00455ac`, fixes `9f27098`, `a73528c`, `87c6f50`). Its real-APNs-transport harness exposed and drove server fixes `e4e01a5` (updates/ends now carry expirations, stale leases release promptly, first-part side switch keeps one card). All gates passed: Swift 1566, all-server race suite, protocol e2e, smoke, server build, go vet, simulator build, UI suite 167/0. Task12 rollout/device runbook written (`22f2fe6`), not executed. Final whole-branch review in progress. No phone, APNs or production proof.

Live Activity push progress (2026-10-04): final whole-branch review (server + client halves) found a Critical defect (the phone's own card was never adopted by the server, so partner changes never updated/ended it) plus sticky-retirement, unrecorded-APNs-class, robustness and runbook issues; one fix wave `d1309e1`…`d7bbcea` addressed all, scoped re-review clean (one Minor parked: own-card adoption should require an active row with a token). Every gate passes on `d7bbcea`: Swift 1571, all-server race suite, protocol e2e, smoke, server build, go vet, simulator build, UI 167/0. Fresh local rollout image `sha256:9e7a747c…` (linux/amd64, label `d7bbcea`). Verification `79e939c`. Local implementation complete; merge, Apple provisioning, deployment and two-phone acceptance await separate approval.

# Live Activity push readiness before the APNs key (2026-10-04)

- [ ] Server fixes (stale-row adoption guard, unregister while disabled, no boot exit, pre-I/O start release, deterministic tests, collation, cleanups).
- [ ] Key-arrival preflight command and secret provisioning script.
- [ ] Schema 27→28 upgrade rehearsal script.
- [ ] iOS fixes (viewer controls, dedicated card intents, removal wedge, re-probe, alert/sheet sequencing, session leak, test gaps, file split).
- [ ] Signing check/enable script.
- [ ] Final gates, review and docs. Plan `docs/superpowers/plans/2026-10-04-live-activity-push-readiness.md`; ledger `.superpowers/sdd/2026-10-04-live-activity-push-readiness/progress.md`.

# Ask: tool calling, memory and PCC (2026-10-05)

- [x] Diagnosed failures: hard-coded `.recorded` kind rejects general answers; fact dump + tool output overflows ~4K context; strict validator; general prompt forbids amounts.
- [x] Design approved in chat: PCC first with on-device fallback, three care tools, plain-text streamed answers, rolling memory compaction, general guidance with age, title "Ask".
- [x] Spec: `docs/superpowers/specs/2026-10-05-ask-tool-calling-design.md`.
- [x] User approved the written spec (2026-10-05).
- [x] Plan: `docs/superpowers/plans/2026-10-05-ask-tool-calling.md` (7 tasks).
- [ ] Blocker (user): PCC needs the managed entitlement `com.apple.developer.private-cloud-compute` (Small Business Program, request at developer.apple.com/private-cloud-compute). Until then Ask runs on-device.
- [x] User chose on-device only (PCC deferred until the entitlement is granted) and inline execution.
- [x] Implemented on `feat/ask-tool-calling` (`.worktrees/ask-tool-calling`, 9 commits). After live on-device eval: a guided-generation planning call picks log reads (named ranges), the app runs them, a tool-free answer call streams; rolling memory; title "Ask".
- [x] Verified: `make test` OK 1576; app build; UI `AskTabTests` 2/2; opt-in live eval (15 questions) all answered. Opus final review: no Critical; Important findings fixed with RED→GREEN tests.
- [x] Merged to main (fast-forward to 447496e7, 2026-10-05); merged `make test` OK 1576; worktree and branch removed.
- [x] Installed 447496e7 (clean detached worktree, signed Debug build) on Phone A in place; launch succeeded (2026-10-05).
- [x] Installed the same 447496e7 build on Phone B in place; launch succeeded (2026-10-05).
- [ ] User check on the phone (Ask answers, follow-ups); PCC entitlement request (user).
