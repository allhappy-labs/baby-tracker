# PostgreSQL Authority — Phase 4 Implementation Plan (the homelab import, Live Activity on the authority, the main merge, and the baseline squash)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:**
- **Live Activity port.** `main`'s Live Activity push server, which production runs today, is ported onto the new storage and the branch's single APNs client.
- **Merge and squash.** `main` is merged, and the whole migration chain is squashed into one baseline that drops the two legacy-only database roles.
- **The import.** One command, `baby-tracker-admin import-legacy`, moves the owner's homelab server into that baseline in one transaction. The homelab runs `main` at `fce422d5`, at `main`'s schema 29 (`000029_baby_icon.sql`), self-hosted, with Live Activity push live.
- **Baby icons.** `main`'s optional baby `icon`, live on the homelab since 2026-10-09, is adopted on the authority exactly as `main`'s wire has it (Task 8a), so the homelab's phones keep sending and receiving it.
- **Continuity.** Its phones stay signed in, keep their cursors, keep their babies' icons, and keep their Live Activity cards updating without registering again.
- **The cutover.** Every step on the homelab needs the owner's approval. After it is verified, the legacy ledger directories and then the importer and all its tooling are deleted.

**Architecture:**
- **Verify first, write once.**
  - Before the transaction, the importer verifies and replays the legacy ledger read-only.
  - Inside one transaction it:
    1. cross-checks the replayed state against the legacy PostgreSQL projection;
    2. renames the legacy `public` and `app` schemas aside (`legacy_public`, `legacy_app`);
    3. creates a new `public`, owned by the migrator, with only its initial PUBLIC privilege;
    4. applies the embedded migrations (after Task 26, the single baseline) exactly as a fresh install does;
    5. loads what the phones need: the server identity, accounts, devices, families and members, active sessions, audit history, care data, and Live Activity registrations, cards and lifecycle state;
    6. verifies the result, drops the legacy schemas and commits.
  - A dry run is the same transaction rolled back.
- **One code path per concern.**
  - Audit rows are hashed by `authority` (`authority.LegacyChains`).
  - Care rows are encoded only by `care.Encode` (from each entity's latest legacy journal version) and copied by `care.CopyImported`. A baby's icon travels inside its wire (Baby-icon ruling).
  - Counters come from `care.CounterDelta`.
  - The legacy ledger is read only through `legacyledger`.
  - Legacy Live Activity rows are copied into 000032's tables, which re-express `main`'s 000028 column for column.
- **Cursor and card continuity.**
  - Every family keeps its `next_sequence`, and every entity's `changed_seq` is its latest legacy journal sequence, so phones pull without a snapshot.
  - Registrations keep their IDs, versions, tokens, generations and session lineage roots. Carried sessions keep their legacy IDs and lineage roots, so a carried registration stays authorized.
- **Live Activity on the authority.**
  - Migration `000032_live_activity.sql` re-expresses `main`'s 000028 on the new `sessions`, `devices` and `care_entities`.
  - Registration runs through the Phase 3 one-transaction request seam.
  - The worker runs on the maintenance pool, woken by the post-commit change feed.
  - The card projection reads `care_entities`.
  - Pushes go through a generic `internal/apns.Send`, of which the background `Wake` is a wrapper.
- **One baseline before the cutover.**
  - After the port and the merge, 000001–000032 are squashed into `000032_baseline.sql` (Task 26), without the projector and Apple-revoker roles (Task 27).
  - The homelab import applies that baseline directly, so no adoption step exists.
  - Legacy migrations survive only as the importer's vendored test support, deleted with it after the cutover (Task 29).

**Tech Stack:** Go 1.26.2; pgx v5.11 (`CopyFrom`, pipelined batches, `pgx.Tx`); PostgreSQL 17 (transactional DDL, `ALTER SCHEMA … RENAME`, hash and range partitions, RLS, SECURITY DEFINER functions, `OVERRIDING SYSTEM VALUE`); Go `net/http` HTTP/2 for APNs; the disposable test-db harness (`Server/scripts/test-db.sh`) and compose harness (`Server/scripts/lib/disposable-sync.sh`); bash with `jq`, `pg_dump`/`pg_restore` 17 and `ssh -o BatchMode=yes` (read-only preflight only); SwiftPM host tests; one `xcodebuild` simulator build on the shared simulator.

**Spec:** docs/superpowers/specs/2026-10-04-postgres-authority-performance-design.md (sections "Migration of existing installs" and "Boundaries"). The Live Activity design on `main`: `docs/superpowers/specs/2026-10-03-family-live-activity-push-design.md` (read with `git show main:…`).

**Base:** Phase 3 complete on `feat/postgres-authority` (000031 FROZEN by Phase 3 Task 19), plus the Phase 4 plan revisions committed so far; this revision 3 replaces them.

**Revision 3 (2026-10-09), and why.** On 2026-10-09 another session, with the owner's approval ("deploy and merge"), moved the homelab from `main` `998501b` (schema 28) to `main` `fce422d5` (schema 29, `000029_baby_icon.sql`) and fast-forwarded `main` to `fce422d5`; `main` is now `7040ca2d`, which adds only the rollout record (`main:docs/verification/2026-10-09-baby-icons-rollout.md`). Revision 2's D2 pin (`homelab_28`) would refuse the real homelab. Revision 3:
- re-pins D2 to **`homelab_29`** and updates the preflight facts;
- adds **amendment steps** to the tasks already committed (1, 3, 4, 6 and 8; Tasks 2, 5 and 7 need none). The committed history is not rewritten; each amendment is a follow-up commit;
- adds **Task 8a** (adopt `main`'s baby icon) and **Task 8b** (the harness clock, the ledger's Task 8 ruling);
- updates every later task the change touches (10, 13, 16–23, 25–28), the Review Focus, the File Structure, the carry map and the execution waves;
- folds the carries the Phase 4 ledger (`.superpowers/sdd/2026-10-09-postgres-authority-phase4/progress.md`) recorded into the tasks that own them.

Where a committed task's original steps say `998501b`, 28 rows, `homelab_28` or `homelab28`, its amendment block governs.

**As built (corrections recorded during execution; these override the text below where they differ).**
- Task 12 replaced `care_entities_latest_feed_idx` and its paged reads with a `care_entities.feed_kind` column (written by `care.Encode`, backfilled, CHECKed) and two partial indexes with `main`'s predicates, `care_entities_latest_milk_idx` and `care_entities_latest_breast_idx`; latest milk and latest breast are single-row reads. 000032's later digests (`693af394…`, `64cb3c3a…`) superseded `688784b4…`, and Task 26 squashed everything into `000032_baseline.sql`.
- Task 22's ledger walk reads envelopes ahead with a bounded worker pool; envelope bodies are bounded, the per-revision file-name index is not (the "never holds more than one envelope" sentence in Task 2's sketch is superseded).
- The full record of rulings, carries and deviations is the Phase 4 ledger, `.superpowers/sdd/2026-10-09-postgres-authority-phase4/progress.md`.

**Owner decisions (binding):**
- **D1. Port `main`'s Live Activity push server in Phase 4, then merge `main`.**
  - One APNs client: the branch's `internal/apns`, whose reason list, re-sign rule and identitytest sink refusal win.
  - Its own migration, 000032.
  - Device registrations and card update tokens carried over, and the app's cards proven end to end.
- **D2. "Only my homelab server."** Import exactly the homelab's starting state and refuse everything else. That state is `main`'s schema 29 as deployed at `fce422d5` on 2026-10-09 (preflight facts below). Revision 2 pinned `998501b`'s schema 28; the homelab left it the same day, so revision 3 re-pins to `homelab_29` and `homelab_28` is refused like any other history.
  - **Re-pin rule.** If the homelab moves again before the cutover (a different `BT_SOURCE_SHA`, or any `schema_migrations` row other than `fce422d5`'s 29), the controller stops every homelab step and re-pins:
    1. vendor the new chain and manifest (Task 3's amendment, repeated);
    2. recapture the fixture (Task 6's amendment, repeated);
    3. port any wire or semantic change the new build brought (as Task 8a does for the icon);
    4. update this header, then resume.

    It never imports a state the plan does not pin. The owner is asked to hold homelab deploys until the cutover (R0); the request is non-blocking.
- **D3. "You can just ask me to proceed with migration and I'll force quit the apps. I don't want any extra code for this."** No in-flight request handling exists. The runbook asks the owner to proceed, and the owner force-quits both apps after each shows no pending changes, before the server stops.
- **Cleanup. "It's a one time run script, we should clean up all migrations after it was run successfully."** With C1 to C3:
  - **C1:** squash **before** the cutover. The homelab import lands directly on the single baseline, with no adoption code. After the cutover, the importer and its tooling are deleted (Task 29).
  - **C2:** the baseline **drops** `baby_tracker_projector` and `baby_tracker_apple_revoker`. A one-time superuser step at the cutover drops the roles and their logins on the homelab (runbook R6).
  - **C3:** **delete** the homelab ledger directories (`security-ledger`, `ledger-head`) right after the import is verified (runbook R7). The database dump and the ledger tar of the pre-import backup stay.
- **D4 (where the real-data rehearsal runs) and D5 (the cutover window)** are asked at execution time. Every homelab step needs the owner's explicit approval in the conversation.

**Homelab preflight facts** (2026-10-09; revision 3 replaces revision 2's `998501b` facts). Sources: the coordinator's read-only preflight of the morning (size, tables, APNs, ledger), and the rollout record of the afternoon's deploy (`git show main:docs/verification/2026-10-09-baby-icons-rollout.md`: revision, schema, compose files, babies). Runbook R1 re-checks every fact live with Task 1's script before anything else happens; a difference is a STOP and the re-pin rule (D2).
- **Deployed build:** `BT_SOURCE_SHA=fce422d51ef4fbd4ac98a4bab4e4375ff6e762a6` (rollout record: image `baby-tracker-server:fce422d…` = `sha256:792a55cdb0677db08910dcdc3b438735604cad291a0e350bff4294169e1684f5`, label revision `fce422d51ef4fbd4ac98a4bab4e4375ff6e762a6`).
  - The host keeps the `998501b` image and the rollback set `source.998501b-rollback-fce422d` and `deployment.env.998501b-rollback-fce422d`.
  - It is on `main`. `main` is `7040ca2d`, which adds only the rollout record.
  - **What changed from `998501b`.** `git diff 998501b fce422d5 -- Server` touches only the baby-icon files:
    - `000029_baby_icon.sql`;
    - `protocol/{canonical,entities}.go`;
    - `records/{babies,repository,snapshot,validation}.go`;
    - `api/openapi.yaml`;
    - the test pins and scripts that move 28 → 29: `disposable-sync.sh`'s schema gate, and `rehearse-live-activity-upgrade.sh`, marked historical.
  - **What did not change from `6546a98c`.** `git diff 6546a98c fce422d5 -- Server/internal/liveactivity Server/internal/protocol/live_activity.go Server/internal/protocol/live_activity_test.go Server/api/fixtures/live-activity-v1 Server/internal/security` is empty. Every Live Activity file this plan copies "from `6546a98c`" is therefore also the homelab's.
  - **Its `compatibility.json`** is `{min:29,max:29}`:
    - rows 1–27 are byte-identical to this branch's 000001–000027;
    - 28 = `000028_live_activity_push.sql`, sha256 `43b37464fd4570ae65eb2a84e3579e7e76dbc9e6c4b4ca3d598a2a74f3bc521e`;
    - 29 = `000029_baby_icon.sql`, sha256 `8a296b40a8a1aba5e8d4f0256a8bb2aa050c50c8ea30a6d9ae2d6cfd9e087288`. That is `git show fce422d5:Server/migrations/000029_baby_icon.sql | shasum -a 256`, equal to the manifest row.
  - **`000029_baby_icon.sql`** does three things:
    - `ALTER TABLE public.babies ADD COLUMN icon text CHECK (icon IS NULL OR icon IN ('boy.1','boy.2','boy.3','boy.4','girl.1','girl.2','girl.3','girl.4'))`;
    - manifest 29 = a copy of manifest 28;
    - compatibility 29.

    No other table, grant or role changes.
- **`schema_migrations`:** 29 rows, max 29 = `000029_baby_icon.sql`. The rollout record's cutover: `migrate` exited 0, "Schema went 28 → 29", and `babies.icon` is present.
- **Compose:** three files, `Server/compose.yaml`, `deploy/unraid/compose.yaml` and `deploy/unraid/live-activity-push.yaml`, named as the record names them (the last two sit under the staged `Server/` tree). All three containers were healthy after the deploy, and `verify-unraid-deployment.sh verify` passed.
- **Live Activity tables present:** `live_activity_{family_dirty,installations,delivery,history,devices,tokens,token_owners,registration_index}`, plus `delivery_pending`. 000029 does not touch them.
- **APNs configured:** `deployment.env` holds `BT_APNS_BUNDLE_ID`, `BT_APNS_KEY_ID` and `BT_APNS_TEAM_ID` (values not read). `main`'s overlay `live-activity-push.yaml` mounts `${BT_DEPLOY_ROOT}/secrets/apns_key.p8` as the `apns_key` secret, and Live Activity pushes are live.
- **Data:**
  - From the morning preflight: 2 accounts, 1 family, 1,421 `mutation_journal` rows; the database is 26 MB and `/database` 81 MB.
  - From the rollout record: **2 living babies**. One has `boy.1`, written by the phones' launch fill-in as a baby `update` carrying `icon`. The other has no icon (no Boys/Girls set). R1 re-measures all of these.
- **Phones:** both run `fce422d5`'s app with the V10 store (Baby `iconID`). "Never install a pre-V10 build on these phones again" (rollout record, "Still open").
- **Backups on the host:** `security-ledger` and `ledger-head` exist. The host holds the earlier dumps and ledger tars, plus `backup.pre-fce422d.*` and `backup.cutover-fce422d.*` (rollout record).
- **Ledger-format drift `5b80a71f..fce422d5`: none.**
  - `security/{event.go,commands.go,localstore/}` are unchanged. The only handler change, before `998501b`, sets `MembershipView.Generation`, an in-memory field derived from `event.Revision` that is never persisted.
  - `998501b..fce422d5` touches no ledger, security or recovery code.
  - `legacyledger` (copied from `7154a82`, a superset) reads this ledger.

**Transaction ruling:**
- "In one transaction" covers everything PostgreSQL holds.
- Ledger verification reads files, so it runs first, outside the transaction, and writes nothing. The transaction re-walks the ledger for the audit rows and refuses if the head moved (`legacy_ledger_changed`).
- Nothing commits until the in-transaction verification passes. A failed run leaves the legacy database exactly as it was and is simply run again. `--verify-only` re-checks an imported database.

**Staging ruling (the fresh-install guards):**
- The importer applies the embedded migrations into an empty `public`, so an imported install is catalog-identical to a fresh one by construction (`TestImportedCatalogEqualsAFreshInstall`).
- Until the squash, 000029–000031's guards (SQLSTATE `55000`, "use baby-tracker-admin import-legacy") refuse a plain `migrate` on a legacy database. The baseline has no guard.
- A plain `migrate` on the homelab's legacy database is refused anyway by the runner's history check. Before the squash, the homelab's row 28 (`000028_live_activity_push.sql`) is not the branch's 28 (`000028_mcp_integrations.sql`), and its row 29 (`000029_baby_icon.sql`) is not the branch's 29 (`000029_postgres_authority.sql`). After the squash, version 1 is unknown to a baseline-only binary.

**Starting-state ruling (D2):** `legacyimport.Detect` accepts exactly one state, **`homelab_29`**: `schema_migrations` equal, row for row (version, name, sha256), to `fce422d5`'s 29 rows, as vendored in `legacytest/testdata/legacy-chain/` with their digests pinned. Everything else is refused:
- empty → `nothing_to_import`;
- the new schema → `already_imported` or `nothing_to_import`;
- leftover staging schemas → `legacy_staging_present`;
- any other history → `not_the_homelab_legacy_state`, naming the first differing or missing row. This includes:
  - legacy 27;
  - **legacy 28 at `998501b`**, revision 2's pin, which the homelab left on 2026-10-09;
  - the branch's own 29;
  - a different 29;
  - a 30th row.

The runtime checks stay: another client connected → `server_running`; the projection head behind the ledger → `legacy_projection_behind`.

**Carried-data ruling (D3 plus card continuity):**

| Carried | Not carried (counted in the report's `Skipped`) |
|---|---|
| `server_state` with the actor-key commitment | `session_refresh_history`, `refresh_recovery_receipts` |
| accounts, `account_identities` | `security_command_receipts`, `account_profile_commands` |
| devices | `mutation_receipts`, `family_record_usage` |
| families with `next_sequence` and recomputed entity counters | `auth_challenges`, `auth_exchange_material`, `apple_notification_dedup` |
| memberships, family slots | consumed, revoked, expired and `material_missing` sessions |
| invitations | `apple_revocation_jobs`, `credential_cleanup_jobs` |
| family entitlements, family recovery claims | `family_command_material`, `operator_audit_events` |
| `family_actor_labels` and their history | `rate_limit_buckets` |
| **active, unexpired sessions**, with their legacy IDs and lineage roots | every `import_*` and `export_*` table |
| the full audit history (spec) | `delivery_pending`, every `sync_*` table |
| every care entity at its latest version, with its first version's origin; a baby's `icon` inside its wire, exactly as its latest journal version holds it | `live_activity_history` (7-day diagnostics) |
| **`live_activity_devices`** (registrations, push-to-start tokens, inventory) | |
| **`live_activity_installations`** (the version fences) | |
| **`live_activity_tokens`** (card update tokens and generations) | |
| **`live_activity_token_owners`, `live_activity_registration_index`** (copied as stored; the triggers that maintain them are off during the load) | |
| **`live_activity_family_dirty`, `live_activity_delivery`** (lifecycle intent), with every lease cleared | |

Why the Live Activity rows are carried and not rebuilt:
- The phones do not replay accepted card tokens (`main` `+Activities.swift:135-136`), so dropping the cards would stop every running card's updates.
- `live_activity_delivery.target_state` holds each card's lifecycle: generation, activity ID, token version, `startUnconfirmed` attempt IDs that a phone's foreground inventory refers back to, and the last digest. It cannot be derived from care data. Rebuilding it would make the worker treat running cards as new lifecycles and push-to-start duplicates.
- `live_activity_family_dirty.dirty_version` must stay monotonic with the carried jobs' `dirty_version`/`completed_version`. A rebuilt counter restarting at 1 would leave every carried job "complete" until it climbed past its old value.
- After the cutover, the first minute's `app.sweep_live_activity_work` sees `authority_checked_at` older than 60 seconds and dirties every registered family once. Each card is then re-evaluated under the new server with its carried state.
- `live_activity_history` is diagnostics with 7-day retention; nothing reads it for decisions.

A carried registration stays authorized: `lineage_root_session_id` and `authority_expires_at` equal the carried session's `lineage_root_id` and `deadline_at` (`app.live_activity_authorized`, Task 8). `main`'s `membership_revision` column has no counterpart (Live Activity ruling) and is dropped.

**Care-source ruling:**
- Each entity's wire comes from its latest `mutation_journal` row: `json.Unmarshal` into `protocol.CanonicalEntity`, then `care.Encode`. That is exactly what the legacy pull served (`main` `sync/journal.go:14-31`).
- A tombstone's typed columns come from its latest live version.
- `created_device_id`/`created_seq` come from its first journal version.
- The legacy typed tables are the field-by-field cross-check. `babies.icon` (000029's CHECK) is compared with the icon in the baby's wire.
- **The icon depends on Task 8a.** It reaches the wire only because Task 8a gives `protocol.BabyPayload` its `Icon` field. Without it, the tolerant entity decoder (`decodeEntityPayload`) would drop the key silently, and the wire-versus-journal check would refuse the homelab.

**Sequence ruling:**
- `families.next_sequence` is copied, and `changed_seq` is the latest journal sequence (unique, never above `next_sequence`; checked).
- `device_sync_cursors` and `device_push_tokens` start empty: the wake backstop rebuilds the first, and the phones upload push tokens once they run a merged app build.

**Audit ruling:**
- Every verified legacy event becomes one row in each scope it touches, accounts first then families, each in ID order. Each row has:
  - the legacy kind;
  - the event's command ID, actor and time;
  - payload = the canonical legacy event;
  - `legacy_hash` = its event hash;
  - a row hash from `authority.auditRowHash`.
- Each scope then gets a `legacy_imported` anchor holding the legacy head.
- Rows before 2026 go to the default partition. Events later than the import time plus 5 minutes are refused.

**Live Activity ruling (D1):**
- The port keeps `main`'s wire byte-for-byte: routes, capability `live-activity-push-v1`, DTOs, receipts and ActivityKit payloads. It also keeps `main`'s lifecycle, presentation and worker logic, copied with `main`'s tests. The homelab's `fce422d5` equals `6546a98c` in every Live Activity file (preflight facts), so the homelab's phones see the same server behaviour.
- What changes:
  - **Authority.**
    - Registration authenticates through `authority.WithRequestTx`.
    - A registration is bound to its session lineage (`lineage_root_id`, `deadline_at`); `membership_revision` is dropped, because removal revokes sessions and a rejoin starts a new lineage.
    - `app.live_activity_authorized` is the one rule for "may still be pushed to".
  - **Storage.** The snapshot reads `care_entities`. Dirtying is fed by the post-commit change feed, with a once-a-minute sweep as backstop, not a journal trigger.
  - **Worker.** The worker runs on the maintenance pool (Phase 3 D3). `main`'s `baby_tracker_live_activity` role, its login and `BT_LIVE_ACTIVITY_DATABASE_URL_FILE` are not ported, and that variable is refused with an explanation.
  - **APNs.**
    - `internal/apns.Send` is the one transport. The branch's rules win: the reason list, re-signing on a 403 at most every 20 minutes, the 24-hour Retry-After cap, `Ambiguous` always carrying a cause, and identitytest refusing to start without the sink.
    - `main`'s preflight becomes `baby-tracker-server apns-preflight`.
    - `LiveActivityAPNsEnvironment` becomes a typealias of `PushEnvironmentResolver`.
  - **Configuration.**
    - `BT_LIVE_ACTIVITY_PUSH_ENABLED` and `BT_APNS_TEAM_ID`, `BT_APNS_KEY_ID`, `BT_APNS_BUNDLE_ID` and `BT_APNS_KEY_FILE` keep `main`'s names. The homelab's existing values and key file carry over unchanged; only `main`'s worker credential is removed.
    - The Unraid overlay `Server/deploy/unraid/push.yaml` (Task 21) replaces `main`'s `live-activity-push.yaml`, the homelab's third compose file today. It turns Live Activity on and, as a cutover option for the owner, background wakes (`BT_APNS_WAKES_ENABLED`). It never sets `BT_LIVE_ACTIVITY_DATABASE_URL` or `BT_LIVE_ACTIVITY_DATABASE_URL_FILE`: the new server refuses either at startup, regardless of the switch (Task 7 ruling).

**Baby-icon ruling (D2; not a wire change):**
- **The fact.** The homelab's phones run `fce422d5`'s app.
  - A baby they create or save carries `"icon":"<boy.N|girl.N>"` whenever one is assigned, and omits the key otherwise (`EntryPayload.swift`).
  - Applying a pulled baby, an absent key means "unchanged", never "cleared" (`BabyStore+RemoteApply.swift`, `BabyIconSyncTests`).
  - Their content hash includes `icon` only when set (`ContentHash.swift`).
- **The port is a cutover blocker.** The branch today decodes inbound payloads with `DisallowUnknownFields`.
  - Such a phone would get `invalid_request` for its whole sync request. It would then quarantine the icon-bearing baby mutation and everything depending on it **permanently** (`main`'s baby-icons plan, Task 9 Step 5).
  - So Task 8a is a precondition of the cutover, and of any rehearsal with a `fce422d5` app.
- **No wire change (Global Constraints).** Task 8a adopts `main`'s existing wire at `fce422d5`: `openapi.yaml` `BabyPayload.icon`, `main`'s decoding, canonical hash, validation and keep-on-update rule. The homelab's phones already speak it, and the homelab server already serves it. Nothing new reaches a phone.
- **Storage: inside the wire only; no schema change.** `care_entities.wire` holds the baby's canonical entity, `icon` included when set. No typed column is added. Why:
  1. **No query reads it.** No server query reads, filters or orders by the icon. 000029's own comment says it is "Assigned by the app, never derived here". The care schema keeps typed columns only for the request-path queries that need them (`sort_order`, `name_hash` for babies).
  2. **The wire is what everything serves.** Pulls, snapshots, `familyread`, export and the Live Activity snapshot all render the wire. `main`'s keep-on-update rule needs only the previous entity, which the write path already decodes from its wire (`care/records.go`'s `existing`).
  3. **The value set is enforced twice.** The one write path enforces it (`care.ValidateMutation`, as `main`'s `records.ValidateMutation`). The import enforces it through `checkCare`, which compares the wire with the legacy `babies.icon`, itself held by `main`'s CHECK.
  4. **000032 stays as it is.** Its digest (`688784b4…d97f`, Task 8 fix round 1) and its review stand. Nothing goes into 000033, and Task 26's golden has no icon line.

  The trade-off is accepted: there is no database CHECK as defence in depth. `care_entities.wire` is written only by `care.Encode` through the store's upsert and by `care.CopyImported`, both after validation.

**Migration and squash ruling (C1, C2):**
- **000032 before the squash.** `000032_live_activity.sql` is written whole by Task 8 and amendable only for review fixes (each regenerates its digest) until Task 26.
- **The squash (Task 26, after the merge and the Live Activity end-to-end proof).**
  - `000032_baseline.sql` replaces 000001–000032 and keeps version 32, so the next migration is 000033 and no test pin renumbers.
  - It is built from a schema-only dump of a fresh chain install, made portable:
    - no owners;
    - the monthly audit partitions created by `app.ensure_audit_partitions(now() + 92 days)` rather than dumped;
    - the `push_token_authority` policy restated `TO CURRENT_USER`;
    - the manifest-32 and compatibility rows inserted as data.
  - A golden catalog recorded from the chain proves the baseline identical.
  - The runner accepts a first file named `NNNNNN_baseline.sql`.
- **Dropping the legacy roles (Task 27).** The baseline is amended to drop every grant to, and manifest row of, `baby_tracker_projector` and `baby_tracker_apple_revoker`; their principal values leave the manifest CHECK. `init-roles.sql`, `init-users.sh`, compose, the provisioning script, `testpg` (no `Revoker` pool), the principal and smoke scripts and their tests follow. The golden is updated in the same commit, and the test pins that the difference from the chain golden is exactly those lines.
- **After the cutover.** The baseline is frozen by Task 28. Nothing remains to adopt: the homelab's `schema_migrations` is written by the import as the single baseline row.

**Safety ruling:**
- No test, script default or task step touches the homelab.
- Every script refuses a target whose `BT_SERVER_INSTANCE_ID` is the homelab instance (`<instance-uuid>`) unless `BT_CUTOVER_APPROVED` equals it; the owner sets it by hand at execution time.
- The runbook's superuser role drop (R6) and ledger deletion (R7) each need their own approval.

**Duration target:**
- **Homelab-shaped fixture (P):** 1 family, 2 accounts, 4 devices, 2 babies (one with an icon), 1,500 journal rows, 2 Live Activity registrations with cards, and a ledger of about 5,000 events, on a legacy schema 29.
  - Target: at most 10 seconds including verification.
  - The homelab cutover estimate is backup 1 minute (26 MB), dry run plus import 30 seconds, verify and restart 5 minutes, about 15 minutes of window with the image build done beforehand.
- **L10k scale fixture:** at most 15 minutes, with rate floors of ledger walk ≥ 4,000 events/s, audit ≥ 10,000 rows/s, and care ≥ 5,000 entities/s.
- Measured by `Report.Phases` in `TestImportScale` (Task 22) and recorded in `docs/verification/<run-date YYYY-MM-DD>-legacy-import-scale.md`.

**Confirmation items for the owner** (raised by the coordinator; the plan follows the stated default):
- C4. Drop `main`'s Live Activity worker role and login (`baby_tracker_live_activity`, `baby_tracker_live_activity_app`) and its two secrets (`live_activity_password`, `live_activity_database_url`) in the same R6 superuser step? The port no longer uses them. Default: yes, as part of R6.
- C5. Turn on background wake pushes (`BT_APNS_WAKES_ENABLED=true`) at the cutover, or later? Default: on at the cutover. They have no effect until the phones run a merged app build that registers a push token (Phase 3's registrar is not in the phones' current `main` build).
- C6. The phones run `main`'s app at `fce422d5` today (V10 store, baby icons). Install a merged-branch app build on both phones before the cutover, or after it? Default: after it.
  - The wire is unchanged, so the current app works with the new server once Task 8a is in. The new build only adds the push-token registrar.
  - Whenever it is installed, the build must come from a tree at or after Task 23's merge, so it contains `main`'s V10 store and icon UI. A branch build from before the merge is pre-V10: the phone would move its V10 store aside and lose unsynced data (rollout record: "Never install a pre-V10 build on these phones again").

## Global Constraints
- "`baby-tracker-admin import-legacy` runs once with the server stopped, in one transaction:"
- "1. Verify the legacy file ledger chain end to end."
- "2. Replay it and cross-check the result against the legacy PostgreSQL projection; refuse on any mismatch."
- "3. Create the new schema."
- "4. Transform and load authority, audit history (each legacy event becomes an audit row in its scope with its legacy hash recorded), all care data and the server identity."
- "5. Drop the legacy tables."
- "It is all-or-nothing and refuses to run twice. Token digests and keys are unchanged, so devices stay signed in and MCP credentials keep working."
- "The legacy verification and replay code survives only inside the importer's package. A fresh install creates the new schema directly. Rollback restores the required pre-upgrade backup of database and ledger files taken together."
- "Importer tests on real legacy fixtures with field-by-field verification."
- "Phones receive the same entity shapes and cursors but only each entity's latest state, not every intermediate version. Tombstones remain rows, so old cursors still see every delete."
- "The schema is new. Old table names, shapes and constraints are not preserved." Owner: "no need to stick to all of the old table names or constraints, table design should be also performance focused"; "Migrate in place".
- "Every index is justified by a named request-path query." "Row-level security remains on every family-scoped table as defence in depth."
- "Push wake intents leave the write transaction: a background worker compares each device's acknowledged cursor with the family sequence." The Live Activity worker follows the same shape: dirtying happens after commit, never in the write transaction.
- "No merge, push, deployment or production import without explicit approval." "`main` has moved …; reconcile before any merge." "The user's Unraid upgrade is documented and proposed, not executed."
- Out of scope: "Wire-protocol changes."
  - What the homelab phones see after the cutover is what a fresh install sends: the documented latest-state pulls, plus `live-activity-push-v1` and `push-token-v1` when configured.
  - Live Activity routes and payloads are byte-identical to `main`'s (`Server/api/fixtures/live-activity-v1/`).
  - The baby `icon` (Task 8a) is **not** a wire change. It is `main`'s existing wire at `fce422d5`, which the homelab's phones already speak (Baby-icon ruling). The branch accepts it, validates it, keeps it on an update that omits it, and returns it byte-identically, exactly as `main` does. `Server/api/fixtures/v1/**` stays byte-identical to `main`'s, which did not change it.
- Migrations 000029 (`00b48588…205d`), 000030 (`c52ffb37…549b`) and 000031 are FROZEN until the squash (Task 26). `000032_live_activity.sql` is amendable only for review fixes until Task 26; its digest after Task 8's fix round 1 is `688784b4dd60d08e321c23d4118f22ccc309f898bf299fbd7ca754d93f08d97f`. `000032_baseline.sql` is FROZEN from Task 28.
- No deferred constraint before the squash. 000027's `SET CONSTRAINTS ALL IMMEDIATE` persists through the single-transaction chain that `ApplyTx` applies (Task 4 review M5). Any later chain file that declares a `DEFERRABLE` constraint would be checked immediately there, unlike on a fresh `Apply`, so none may (Tasks 8 and 26).
- Carries, binding:
  - `legacyledger` stays read-only, with its temp file outside `ledgerDir`;
  - `entitlement_required` is set correctly (Task 15);
  - one encoder (`care.Encode`);
  - each family keeps `next_sequence`, with `changed_seq` ≤ it;
  - the importer and `rotate-epoch` leave `device_sync_cursors` empty;
  - Live Activity registrations, card tokens and lifecycle state are carried, so cards keep updating (preflight facts, carried-data ruling);
  - every baby's `icon` survives the import exactly, present with its value or absent (never `null`), and the phones' icon-bearing mutations are accepted (Baby-icon ruling);
  - the import runs on one dedicated connection, because `checkAlone` counts every other session on the database, including the migrator's own idle pool connections (Tasks 13, 18, 19);
  - audit rows before 2026 go to the default partition;
  - imported care matches the golden corpus semantics;
  - `internal/apns` is the only APNs code (Phase 3 plan :19, :4008-4012).
- Credentials: production APNs credentials (team `TEAM123456`, key `KEY1234567`) are operator-provided at deploy, and are never committed, printed or used by tests (Phase 3 D4). Tests generate throwaway P-256 keys (`TestNoProductionAPNsKeyInTheServerTree`).
- Process:
  - Prefix every shell command with `rtk`.
  - Each agent runs one `test-db.sh` at a time, with a private `GOCACHE=$(mktemp -d)`, after `df -h /` shows at least 5 GB free (30 GB for Task 22), and removes its own temporary copies.
  - Subagents run on opus or sonnet only.
  - Any `xcodebuild` uses only `platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853`, runs one at a time after `pgrep -fl xcodebuild` prints nothing, and never creates, resets or erases a simulator.
  - Parallel tasks run in isolated worktrees; the controller cherry-picks them in task order.

## Review Focus
1. **The cutover forces snapshots or drops changes.** A phone whose cursor is any N between 0 and the family high water pulls right after the import. It must get exactly the entities whose latest change is above N, deletes included as tombstones, in `changed_seq` order. It must never get `snapshot_required`, and the next pull must be empty. Pinned in Task 16 (`TestImportedCursorsPullOnlyNewerLatestStates`) and, on the captured homelab-shaped data, Task 20 (`TestCapturedPhonesKeepPulling`).
2. **An imported, fresh or baseline install differs in catalog, or a dropped role is still referenced.** Each of these would fail readiness's privilege audit or silently weaken RLS:
   - a forced-RLS flag left lifted, or a disabled trigger;
   - a schema owner or ACL;
   - a manifest row or migration digest;
   - the `push_token_authority TO CURRENT_USER` policy;
   - a monthly audit partition;
   - a grant to `baby_tracker_projector` or `baby_tracker_apple_revoker` left after the role drop, which would make the R6 `DROP ROLE` fail on the homelab.

   Pinned in:
   - Task 4 (`TestStagedCatalogEqualsAFreshInstall`);
   - Task 18 (`TestImportedCatalogEqualsAFreshInstall`, `TestImportedNodeIsReady`);
   - Task 26 (`TestBaselineCatalogMatchesTheChainGolden`);
   - Task 27 (`TestBaselineDropsExactlyTheLegacyRoles`, `TestLegacyRolesDropCleanlyAfterAnImport`).
3. **A failure leaves a half-migrated install.** A fault before any step, or at commit, must leave the legacy catalog and per-table row checksums unchanged and the read-only ledger untouched; so must a dry run. A second successful run is refused. Pinned in Task 18 (`TestImportIsAllOrNothingAtEveryStep`, `TestDryRunChangesNothing`, `TestImportRunsOnceAndVerifiesAfter`).
4. **The imported audit chains break the verifier, the roller or the next command.** Inputs:
   - events touching three scopes;
   - an event dated 2025-12-31;
   - a 64 KiB event;
   - a scope with no legacy event.

   Expected:
   - `VerifyAll` reports zero failed scopes;
   - the next `ChangeRole` and `Refresh` chain on top of the anchors;
   - `app.ensure_audit_partitions` skips no month;
   - a tampered payload fails exactly its scope.

   Pinned in Task 5 (`TestLegacyChainsVerifyAndTamperFailsTheScope`) and Task 15 (`TestImportedChainsVerifyAndNewCommandsChainOnTop`). Task 15's review must confirm the last two expectations explicitly; they are the Task 5 review's carry M6.
5. **Live Activity cards stop, duplicate, or reach the wrong phone after the cutover.**
   - **Carried cards.** A running card registered before the cutover must get its next `update` on its carried update token, under its carried generation ID, with no push-to-start and no new registration from the phone.
   - **Removal.** A removed caregiver's or revoked lineage's registration gets nothing and is revoked by the next sweep.
   - **The starting phone** gets no push-to-start for its own timer.
   - **Ends.** A stopped timer's card gets exactly one `end`.
   - **Invalid tokens.** A `410` token is cleared and never retried.

   Pinned in:
   - Task 14 (`TestRemovedMemberGetsNoLiveActivityPush`, `TestStartingPhoneGetsNoPushToStart`, `TestStoppedTimerEndsItsCardOnce`, `TestInvalidCardTokenIsClearedOnce`);
   - Task 17 (`TestCarriedCardKeepsUpdatingWithoutReRegistration`);
   - Task 20 (`TestCapturedCardsKeepUpdating`);
   - Task 25 (`live-activity proof: step 13:`).
6. **A phone's baby icon is refused, dropped or changed.** The homelab's phones (`fce422d5`'s app) send `icon` on baby creates and updates, and read an absent key as "unchanged". A refused key quarantines their baby mutations permanently; a dropped one silently loses the icons the owner set.

   Expected:
   - an icon-bearing create or update is applied, and the same `"icon":"<value>"` comes back in the outcome, in every pull and in every snapshot;
   - an update without the key keeps the stored icon (so do the changes other phones pull);
   - a present icon replaces it;
   - an out-of-set value (`""`, `neutral`, `boy.0`, `boy.5`, `BOY.1`) is `rejected`/`invalid_mutation` for that mutation alone, and the rest of the batch is unaffected;
   - a baby without one carries no `icon` key, never `null`;
   - the canonical mutation hash of a payload without `icon` is unchanged;
   - an imported baby keeps exactly its legacy icon, or exactly its absence.

   Pinned in:
   - Task 8a (`TestBabyUpdateWithoutIconKeepsStoredIcon`, `TestBabyIconOutOfSetIsRejected`, `TestPhoneBabyIconRoundTripsByteIdentical`, `TestBabyMutationHashOmitsAbsentIcon`);
   - Task 16 (`TestImportedBabyIconsAreExact`);
   - Task 20 (`TestCapturedBabyIconsSurvive`);
   - Task 25 (the merged app's E2E creates babies with icons);
   - runbook R5 (`verify`) and R8.

## File Structure

**Create — importer (deleted by Task 29 after the cutover)**
- `Server/internal/legacyledger/{walk.go,walk_test.go,enumerate.go,enumerate_test.go,sidecar.go,sidecar_test.go}` and `ledgertest/writer.go`.
- `Server/internal/legacyimport/{doc.go,errors.go,detect.go,known.go,stage.go,crosscheck.go,authority_load.go,audit_load.go,care_load.go,live_activity_load.go,verify.go,run.go,report.go,receipt.go,export_test.go}` and their tests: `detect_test.go`, `stage_test.go`, `builder_test.go`, `crosscheck_test.go`, `authority_load_test.go`, `audit_load_test.go`, `care_load_test.go`, `live_activity_load_test.go`, `run_test.go`, `fixture_test.go`, `scale_test.go`, `capture_test.go`, `fixtures_manifest_test.go`.
- `Server/internal/legacyimport/legacytest/{legacy.go,captured.go,builder.go,templates.go,restore.go}`.
- `Server/internal/legacyimport/legacytest/testdata/legacy-chain/000001_*.sql` … `000029_baby_icon.sql`: `fce422d5`'s 29 migration files, byte-identical, with digests pinned, plus `manifest.json` (Task 3; 29 rows from the revision 3 amendment).
- `Server/internal/legacyimport/testdata/fixtures/homelab29/{manifest.json,data.sql.gz,ledger.tar.gz,secrets.tar.gz,env.json,client.json}`, captured at `fce422d5` (Task 6 amendment). It replaces revision 2's `homelab28/`, which is deleted.
- `Server/internal/authority/{legacy_audit.go,legacy_audit_test.go}`.
- `Server/internal/care/{copy.go,copy_test.go}`.
- `Server/cmd/baby-tracker-admin/{import_legacy.go,import_legacy_test.go}`.
- `Server/scripts/{homelab-preflight.sh,homelab_preflight_test.go,capture-legacy-fixture.sh,capture_legacy_fixture_test.go,legacy-cutover.sh,rehearse-legacy-import.sh,legacy_cutover_test.go}`.
- `docs/operations/legacy-import.md`: the runbook; Task 29 moves it to `docs/verification/` as the cutover record.

**Create — permanent**
- `Server/internal/testpg/{catalog.go,catalog_test.go}`.
- `Server/migrations/000032_live_activity.sql`, replaced by `000032_baseline.sql` (Task 26).
- `Server/internal/migrate/testdata/catalog-32.golden` and `Server/internal/database/baseline_test.go` (Tasks 26–27).
- `Server/internal/apns/{send.go,send_test.go,preflight.go,preflight_test.go}`.
- `Server/internal/config/{live_activity.go,live_activity_test.go}`.
- `Server/internal/protocol/{live_activity.go,live_activity_test.go}` and `Server/api/fixtures/live-activity-v1/**`, byte-identical to `main`'s.
- `Server/internal/liveactivity/{doc.go,types.go,presentation.go,lifecycle.go,repository.go,service.go,queue.go,snapshot.go,worker.go,transport.go,dirty.go,authorization.go}` and their tests.
- `Server/internal/authority/{live_activity.go,live_activity_test.go}`.
- `Server/internal/httpapi/{live_activity.go,live_activity_test.go,live_activity_roundtrip_test.go,live_activity_absent_test.go}`.
- `Server/cmd/baby-tracker-server/{live_activity.go,live_activity_test.go,apns_preflight.go,apns_preflight_test.go}`.
- `Server/internal/database/live_activity_schema_test.go`, `Server/internal/care/origin_test.go`.
- `Server/internal/sync/baby_icon_test.go` (`main`'s, ported), `Server/internal/httpapi/baby_icon_test.go` and `Server/internal/httpapi/testdata/baby-icon-roundtrip.json` (Task 8a).
- `Server/internal/testkit/{compose.go,cursor.go}`.
- `Server/deploy/unraid/push.yaml` (Task 21).
- `docs/verification/<date>-homelab-preflight.md` (Task 1), `<run-date>-legacy-import-scale.md` (Task 22), `<date>-homelab-cutover.md` (runbook R10).

**Modify**
- `Server/internal/legacyledger/{doc.go,dependencies_test.go}`.
- `Server/internal/migrate/{migrate.go,compatibility.json,compatibility_test.go}`.
- `Server/internal/authority/{verifier.go,server_state.go,server_state_test.go,sessions.go}`.
- `Server/internal/care/{entity.go,store.go,caretest/counters.go}`; for Task 8a, `Server/internal/care/{validate.go,validate_test.go,records.go}`.
- `Server/internal/delivery/observer.go`, `Server/internal/apns/client.go`, `Server/internal/config/{config.go,apns.go}`.
- `Server/internal/protocol/{auth.go,sync.go,types.go,types_test.go}`; for Task 8a, `Server/internal/protocol/{entities.go,canonical.go,canonical_test.go}`. Also `Server/internal/limits/policy.go` and `Server/internal/httpapi/router.go`.
- `Server/internal/testkit/fixtures.go` (Tasks 8a, 8b and 15), `Server/internal/testpg/database.go`.
- `Server/cmd/baby-tracker-server/{main.go,apns_sink.go,apns_sink_identitytest.go}`, `Server/cmd/baby-tracker-admin/{main.go,main_test.go}`.
- `Server/scripts/{test-db.sh,lib/disposable-sync.sh,protocol-e2e.sh,protocol_e2e_test.go,check-principals.sh,smoke.sh,smoke_test.go,provision-unraid-secrets.sh,bootstrap_integration_test.go}`.
- `Server/deploy/postgres/{init-roles.sql,init-users.sh}`, `Server/compose.yaml`, `Server/deploy/unraid/compose.yaml`.
- The schema tests that pin a version (Task 8).
- `Server/README.md`, `docs/protocol/v1.md`, `Server/api/openapi.yaml`.
- Task 23's merge table lists every file the merge touches.

**Delete**
- Task 23: `main`'s Live Activity files the port replaces.
- Task 26: migrations 000001–000032, the tests of individual pre-baseline migrations, the legacy migration-overlap tool, and the legacy-only scripts (`format-cleanup-dry-run.sh`, `refresh-recovery-phases.sh`'s pre-baseline cases).
- Task 27: every reference to the projector and Apple-revoker roles.
- Task 29: everything under "Create — importer".

**Phase 4 carry map:**

| Item (source) | Owner |
|---|---|
| `legacyledger` read-only; temp index outside `ledgerDir` (Phase 1 :134-138) | Task 2; Task 18 (read-only mount) |
| `entitlement_required` (Phase 1 :86) | Task 15: true only for families created by `family_bootstrapped` (none on the self-hosted homelab; pinned) |
| Legacy audit rows under the same hash (Phase 1 :125) | Task 5 |
| One encoder; counters recomputed (Phase 2 :1153, :58) | Task 16 |
| Keep `next_sequence`; `changed_seq` ≤ it (R2 C2) | Task 16 |
| `rotate-epoch` and the importer leave `device_sync_cursors` empty (Phase 3 :125) | Tasks 15 and 19 |
| Fill the receipts' frozen child scope (Phase 3 :156, :182) | **Obsolete under D3:** no recovery receipt is carried |
| Audit rows before 2026 or past a 3-month horizon (Phase 3 :48) | Task 15 |
| Replace the fresh-install guards (Phase 2/3) | Staging ruling; gone with the squash (Task 26) |
| Golden corpus semantics (Phase 2 Task 1) | Task 20 |
| `device_push_tokens` empty (Phase 3) | Task 15 |
| `main` 000028 collision (Phase 3 :12) | D2: the homelab is at `main`'s 29 (revision 3; it was at 28 until 2026-10-09). Detect accepts exactly `fce422d5`'s 29 rows (Task 3 amendment), and its Live Activity rows are carried into 000032 (Task 17) |
| `main`'s baby icon, 000029 (`fce422d5`, live on the homelab 2026-10-09) | Task 8a (wire, validation, keep-on-update; no schema change), Task 16 (carried from the latest journal version and checked against `babies.icon`), Task 20 (captured), Task 23 (`main`'s 000029 file removed at the merge; the app side merged), runbook R8 |
| The homelab moved 28 → 29 (ledger ESCALATION) | D2 re-pin rule; amendments to Tasks 1, 3, 4 and 6; Tasks 10, 18–22 and 28 pinned at 29 |
| Task 3: the import holds one dedicated connection (`checkAlone` counts every other session) | Tasks 13 and 18 (`importRun.conn`), Task 19 (no `MaxConns ≥ 2`) |
| Task 3: a spurious `server_running` (a health-check session) | Runbook R5: rerun once; a second refusal is a STOP |
| Task 3: the CLI prints `err.Error()` for `server_running` (it already starts with the code) | Task 19 |
| Task 3 / Task 26: the "branch's own" Detect case seeds its rows literally after the squash; Detect's `already_imported` still holds for the baseline manifest | Task 26 |
| Task 4 review: M3 `schema_migrations` compared separately; M5 no deferred constraints; M6 a real `DROP ROLE`; I1/M1 `first=expected`, TimeZone-independent | Task 18; Global Constraints (Tasks 8, 26); Task 27; Task 26 golden |
| Task 5 review M6: `ensure_audit_partitions` skips no month; the next `ChangeRole`/`Refresh` chain on the anchors | Task 15's review (Review Focus 4) |
| Task 6 review: dump collision, snapshot entities keyed by `meta.id`, the outsider's `family_recovery` session, the card-scoped push-to-start check, the `env.json` filter; recapture folds M3, M5, M6, M8 | Task 20 (`Restore`, helpers), Task 21 (rehearsal env filter), Task 6 amendment |
| Task 7: `push.yaml` sets no `BT_LIVE_ACTIVITY_DATABASE_URL[_FILE]`; R2 runs `apns-preflight` for both push types | Task 21 (`push.yaml` test; R2, R5) |
| Task 8 review M2: `::app.apns_environment` in the devices/tokens/token-owners loads | Task 17 |
| Task 8 fix round 1: `app.live_activity_authorized` is NULL when the caller's tenant context differs from the registration's; the sweep clears the tenant context per row; digest `688784b4…d97f` | Task 8 amendment; Tasks 14, 17 and 18 call it with the registration's context or none |
| Task 8: harness sessions fixed at 2026-09-21 stop refreshing on 2026-10-21 | Task 8b |
| `internal/apns` is the one APNs client (Phase 3 :143, plan :19, :4010) | Tasks 7 and 14 |
| `LiveActivityAPNsEnvironment` → `PushEnvironmentResolver` (plan :4011) | Task 24 |
| `main`'s APNs key preflight (plan :4012) | Task 7 (`apns-preflight`); runbook R2 runs it |
| Ledger dirs in the provisioning script and the overlay (Phase 1 :183) | Task 26 (code); runbook R7 (the homelab's directories, C3) |
| Projector and Apple-revoker roles (Phase 1 :192) | Task 27 (code); runbook R6 (homelab cluster, C2) |
| Unraid push wiring (Phase 3 Phase-6 carry) | **Brought forward:** Task 21 (`push.yaml`) and runbook R2/R5, because Live Activity is live on the homelab |
| Real-device push delivery (Phase 3 :174) | Runbook R8: the owner watches a card update on a phone after the cutover; Phase 6 covers background wakes on a merged app build |

---

**Execution waves.** Each wave starts when the tasks it needs are committed. Tasks in one wave run in parallel, one agent each, in isolated worktrees, with one `test-db.sh` per agent; the controller cherry-picks them in task order.

| Wave | Tasks (parallel within the wave) | Needs |
|---|---|---|
| A | **2** `legacyledger` · **3** Detect and the vendored legacy chain · **5** legacy audit chains · **7** `apns.Send` and preflight · **8** migration 000032 | — |
| B | **1** homelab preflight script (already run once by the coordinator; re-run at R1) · **4** ApplyTx, staging and catalog · **6** homelab fixture capture · **8a** baby icon · **8b** harness clock · **9** care origin columns · **11** Live Activity registration | 1, 4 and 6: 3 · 8a: — · 8b, 9 and 11: 8 |
| R3 (now, inside wave B) | Revision 3 amendments: **3** first (vendor `fce422d5`, `homelab_29`), then **1**, **4** and **6** (recapture at `fce422d5`) in parallel; **8**'s amendment is notes only | 1, 4 and 6: 3's amendment (the recapture builds its icon bodies as JSON maps, so it does not need 8a) |
| C | **10** synthetic builder · **12** Live Activity snapshot and projection | 10: 2–5 and 3's amendment · 12: 9, 11 |
| D | **13** import pipeline and cross-check · **14** Live Activity worker | 13: 3, 4, 10 · 14: 7, 11, 12 |
| E | **15** authority load · **16** care load | 15: 5, 13 · 16: 8a, 9, 13 |
| F | **17** Live Activity load | 14, 15, 16 |
| G | **18** one transaction | 17 |
| H | **19** CLI and `rotate-epoch` | 18 |
| I | **20** captured-fixture end to end · **21** cutover tooling, overlay and runbook · **22** scale | 20, 21: 6 (recaptured), 19 · 22: 18 |
| J (sequential) | **23** merge `main` → **24** fault logging and the resolver alias → **25** Live Activity end to end → **26** squash into one baseline (C1) → **27** drop the legacy roles from the baseline (C2) → **28** freeze the baseline, final gates and rehearsal | 14, 20–22 |
| — | **Cutover gate:** runbook R0–R10 on the homelab, each step approved by the owner. D4 and D5 are asked at R0; C4–C6 too | 28 |
| P | **29** delete the importer and its tooling | the owner confirms the cutover (R10) |

Every task leaves `rtk make server-test` green: the importer is unused by the server, and Live Activity is off unless `BT_LIVE_ACTIVITY_PUSH_ENABLED=true`. Amendment commits follow the same rule. An amendment's test run includes every test the original task added.

### Task 1: The homelab preflight — read-only tooling for the R1 re-check [Wave B; after Task 3]

**Status:** the coordinator ran a read-only preflight on 2026-10-09; its facts are in the header. This task turns those checks into a pinned script so runbook R1 re-runs exactly them before the cutover.

**Files:**
- Create `Server/scripts/homelab-preflight.sh` and `Server/scripts/homelab_preflight_test.go`.
- Create `docs/verification/2026-10-09-homelab-preflight.md`. It records the header's preflight facts as the baseline R1 compares against: no secret values, and no row contents.

**What the script reads** (`ssh -o BatchMode=yes root@192.0.2.10`, deploy root `/mnt/user/appdata/baby-tracker-sync`, project `baby-tracker-sync`). Every remote command is read-only:
- `schema_migrations` rows, through `docker exec baby-tracker-sync-db-1 psql -XAt -v ON_ERROR_STOP=1 -U postgres -d babytracker -c "SET default_transaction_read_only = on; SELECT …"`;
- `server_state` (instance, epoch, generation, revision, event hash) and `security_projection_head`;
- counts:
  - accounts, families, devices, active and all sessions;
  - care rows per table, `mutation_journal`, unfinished `import_sessions`;
  - `live_activity_devices` (all and not revoked), `live_activity_tokens` (all and `state='active'`), `live_activity_delivery`, `live_activity_installations`;
- `pg_database_size` and `version()`;
- `deployment.env`'s `BT_SOURCE_SHA`, `BT_MODE`, `BT_SERVER_INSTANCE_ID` and `BT_RECOVERY_EPOCH`, plus the **presence** (`=set`, never the value) of `BT_APNS_TEAM_ID`, `BT_APNS_KEY_ID` and `BT_APNS_BUNDLE_ID`, through `grep -E '^(BT_SOURCE_SHA|BT_MODE|BT_SERVER_INSTANCE_ID|BT_RECOVERY_EPOCH)='` and `grep -cE '^BT_APNS_(TEAM_ID|KEY_ID|BUNDLE_ID)='`;
- `test -f secrets/apns_key.p8 && stat -c '%a %u' secrets/apns_key.p8` (mode and owner only);
- `jq -c '{generation,revision,event_hash}' ledger-head/head.json`, `jq -c '{instance_id,epoch,revision}' ledger-head/identity.json`, `test -f ledger-head/actor-id-key.json`;
- `du -sb security-ledger`, `find security-ledger -type f | wc -l`, `df -Pk /mnt/user`;
- `docker inspect -f '{{.State.Running}}' baby-tracker-sync-app-1`.

It prints one `key=value` line per fact and ends with `preflight: schema=<max> digests=<match|mismatch> head=<g>:<r> projection=<equal|behind|ahead> mode=<…> source=<sha> apns=<set|missing> app=<running|stopped>`. It never prints a token, digest, key, secret value or row contents.

- [ ] **Step 1: Write the failing test** `homelab_preflight_test.go`. It runs the script with `PATH` set to a shim directory whose `ssh` records its arguments and replays canned output. It asserts:
  - every recorded remote command is on the allow-list (the `psql … SET default_transaction_read_only = on; SELECT …` form, the `grep -E`/`grep -cE` forms above, `jq -c`, `test -f`, `stat -c`, `du -sb`, `find … -type f`, `df -Pk`, `docker inspect -f`), and none contains `INSERT`, `UPDATE`, `DELETE`, `ALTER`, `DROP`, `TRUNCATE`, `CREATE`, `rm `, `mv `, `cp `, `cat ` or `>`;
  - the script refuses to start unless `BT_PREFLIGHT_APPROVED=yes`;
  - canned rows equal to `Server/internal/legacyimport/legacytest/testdata/legacy-chain`'s 28 digests yield `digests=match`. A 27-row schema, or a changed 28th digest, yields `digests=mismatch` and exit code 3;
  - the canned `grep -cE` count 3 yields `apns=set`.

  The 28 expected digests are read from Task 3's `legacy-chain/manifest.json`, and so is the script's own expectation.
- [ ] **Step 2: Run to see the failure:** `rtk go -C Server test ./scripts -run TestHomelabPreflight -count=1`. Expected: the script is missing.
- [ ] **Step 3: Implement** the script. `set -euo pipefail`. Exit codes:
  - 0 when the state matches;
  - 3 for a schema mismatch;
  - 4 when the projection head differs from the ledger head;
  - 5 when the app is not running (informational before the cutover);
  - 6 when the APNs settings are missing.

  Write `docs/verification/2026-10-09-homelab-preflight.md` from the header's facts.
- [ ] **Step 4: Run green:** the Step 2 command.
- [ ] **Step 5: Commit:** `rtk git add Server/scripts/homelab-preflight.sh Server/scripts/homelab_preflight_test.go docs/verification/2026-10-09-homelab-preflight.md && rtk git commit -m "feat(ops): a read-only homelab preflight for the cutover re-check"`

**Revision 3 amendment.**
- **State.** Task 1 is committed as `b01ba944`. Its review fix round is in flight: it folds C1 (the `SET` tag), I1 (read-only), I2 (the allow-list and the stub `PATH`), I3 (failures reported) and the minors, and makes the script manifest-driven.
- **Who does Steps 6–9.** That fix round, or one follow-up commit after it. They need Task 3's amendment (the 29-row manifest).
- **Precedence.** Where Steps 1–3 say "28 digests", the manifest's row count governs.
- [ ] **Step 6: Manifest-driven expectation.** The script and its test read the expected rows only from `Server/internal/legacyimport/legacytest/testdata/legacy-chain/manifest.json` (`known`, with `min = max`); no row count, name or digest is written in the script. Test cases, each over canned `ssh` output:
  - rows equal to the manifest's 29 → `schema=29 digests=match`, exit 0;
  - `998501b`'s 28 rows (the manifest minus row 29) → `schema=28 digests=mismatch`, exit 3;
  - row 29 with another sha256 → `digests=mismatch`, exit 3;
  - a 30th row → `digests=mismatch`, exit 3.
- [ ] **Step 7: Three more read-only facts.** The allow-list gains only these exact forms.
  - **Babies.** `SELECT count(*) FILTER (WHERE tombstoned_at IS NULL), count(*) FILTER (WHERE tombstoned_at IS NULL AND icon IS NOT NULL) FROM babies`, printed as `babies_live=<n> babies_with_icon=<n>`. These are counts only, never an icon value.
  - **Compose files.** `docker inspect -f '{{index .Config.Labels "com.docker.compose.project.config_files"}}' baby-tracker-sync-app-1`, printed as `compose=<paths, comma-separated, with the deploy root prefix stripped>`.
  - **Image revision.** `docker inspect -f '{{index .Config.Labels "org.opencontainers.image.revision"}}' baby-tracker-sync-app-1`, printed as `image_revision=<sha>`. A value different from `deployment.env`'s `BT_SOURCE_SHA` is exit 3.

  The summary line becomes `preflight: schema=<max> digests=<…> head=<g>:<r> projection=<…> mode=<…> source=<sha> image_revision=<sha> apns=<…> app=<…> compose=<…>`. The test pins the new fields from canned output.
- [ ] **Step 8: The read-only proof (review I1, C1, I2).**
  - Every `psql` runs with a session that is read-only from its start, for example `docker exec -e PGOPTIONS='-c default_transaction_read_only=on' baby-tracker-sync-db-1 psql -XAtq -v ON_ERROR_STOP=1 …`. `-q` prints no `SET` tag. A `SET` inside the same `-c` does not make that statement read-only.
  - `homelab_preflight_test.go` pins the invocation form.
  - The fix round's report shows one local run against a throwaway `postgres:17.6-alpine` container in which an `INSERT` through exactly that form fails with SQLSTATE `25006`.
  - The test asserts that, under its `PATH`, `command -v ssh` is the shim, so a real `ssh` is never reachable from a test (review I2).
- [ ] **Step 9: The baseline document.** `docs/verification/2026-10-09-homelab-preflight.md` records revision 3's header facts:
  - `fce422d5`;
  - schema 29, with row 29's name and sha256;
  - the three compose files;
  - 2 living babies, 1 with an icon;
  - and the morning's size and APNs facts.

  Each fact names its source: the morning preflight or the rollout record (`main:docs/verification/2026-10-09-baby-icons-rollout.md`). It states that R1 must confirm every one live, and that a difference is a STOP and the D2 re-pin rule.

  Run `rtk go -C Server test ./scripts -run TestHomelabPreflight -count=1` (PASS), then commit: `rtk git add Server/scripts/homelab-preflight.sh Server/scripts/homelab_preflight_test.go docs/verification/2026-10-09-homelab-preflight.md && rtk git commit -m "fix(ops): the homelab preflight expects the vendored fce422d5 manifest, counts icons and reads the compose files"`.

### Task 2: `legacyledger` — streaming verification, enumeration and the head-directory sidecars [Wave A]

**Files:**
- Create `Server/internal/legacyledger/walk.go`, `walk_test.go`, `enumerate.go`, `enumerate_test.go`, `sidecar.go`, `sidecar_test.go`.
- Create `Server/internal/legacyledger/ledgertest/writer.go`. This is test support: it writes envelopes, `head.json`, `identity.json` and `actor-id-key.json` in the on-disk format `ledger_test.go:32-68` writes. Its structs carry the same JSON tags as `diskCandidate`/`diskHead`, because those are unexported.
- Modify `Server/internal/legacyledger/doc.go`: the consumer is `internal/legacyimport`.
- Modify `Server/internal/legacyledger/dependencies_test.go`: add `TestOnlyTheImporterImportsLegacyledger`.

**Interfaces:** Consumes the existing `openHeadStore`, `openLedger`, `(*Ledger).WalkCommitted`, `validateCandidate`, `decodeEventStrict`, `(*State).apply`, `decodeDiskJSON`, `readFileBounded`, `fromDiskHead` and `resolveDir`. Produces:
```go
package legacyledger

var (
	ErrRecoveryIdentityMissing  = coded("recovery_identity_missing")  // no identity.json in the head directory
	ErrRecoveryIdentityMismatch = coded("recovery_identity_mismatch") // its trusted head is not on the verified chain
)

// RecoveryIdentity is <headDir>/identity.json (main internal/recovery/state.go:57-63).
type RecoveryIdentity struct {
	InstanceID  string
	Epoch       uint64
	TrustedHead Head
}

// ActorKeySidecar is <headDir>/actor-id-key.json (main internal/recovery/actor_key.go:24-42).
type ActorKeySidecar struct {
	InstanceID string
	Commitment [32]byte
}

// Verified is a ledger verified end to end and replayed.
type Verified struct {
	Head     Head
	State    State
	Identity RecoveryIdentity
	ActorKey *ActorKeySidecar // nil when the head directory has none
	Events   uint64
}

// WalkVerified verifies the chain beneath the trusted head and replays it one
// envelope at a time, calling visit after each event is applied, in commit
// order. It never holds more than one envelope; it writes nothing, its scratch
// index lives in the system temp directory (WalkCommitted). A visit error stops
// the walk and is returned as is.
func WalkVerified(ctx context.Context, ledgerDir, headDir string, visit func(Candidate, Event) error) (Verified, error)
func ReadRecoveryIdentity(headDir string) (RecoveryIdentity, error)
func ReadActorKeySidecar(headDir string) (*ActorKeySidecar, error)

// Entry is one keyed view, as the accessors key it.
type Entry[V any] struct {
	Key  string
	View V
}
func (state State) Accounts() []Entry[AccountView]         // key: account ID; every enumerator sorts by key and returns detached copies
func (state State) Families() []Entry[FamilyView]          // key: family ID
func (state State) FamilySlots() []Entry[FamilySlotView]   // key: account ID
func (state State) AllMemberships() []MembershipView       // sorted by (FamilyID, AccountID)
func (state State) Devices() []Entry[DeviceView]
func (state State) Invitations() []Entry[InvitationView]
func (state State) Sessions() []Entry[SessionView]         // key: session ID
func (state State) Entitlements() []Entry[EntitlementView] // key: family ID + "/" + entitlement key
func (state State) RecoveryClaims() []RecoveryClaimView     // sorted by (FamilyID, AccountID)
func (state State) Integrations() []Entry[IntegrationView]

package ledgertest
// Writer builds a legacy ledger in two temp directories, in the exact
// on-disk format legacyledger reads. Test support only.
type Writer struct{ /* t, ledgerDir, headDir, head */ }
func New(t testing.TB) *Writer
// Append sets the event's Generation (1), Revision, PreviousHash, encodes it
// with legacyledger.EncodeEvent and writes its envelope; it returns the new head.
func (w *Writer) Append(event legacyledger.Event) legacyledger.Head
// Finish writes head.json, identity.json (trusted head = the current head)
// and, when actorKey is set, actor-id-key.json, and returns the two directories.
func (w *Writer) Finish(instanceID string, epoch uint64, actorKey *[32]byte) (ledgerDir, headDir string)
func (w *Writer) Head() legacyledger.Head
```

- [ ] **Step 1: Write the failing tests.**
  - `walk_test.go` (package `legacyledger_test`, reusing `replay_test.go`'s `copyTree`, `chmodTreeReadOnly`, `chmodTreeWritable`, `treeListing`):
```go
func fixtureWithIdentity(t *testing.T) (ledgerDir, headDir string) {
	t.Helper()
	root := t.TempDir()
	ledgerDir, headDir = filepath.Join(root, "ledger"), filepath.Join(root, "head")
	copyTree(t, "testdata/fixture_chain/ledger", ledgerDir)
	copyTree(t, "testdata/fixture_chain/head", headDir)
	head, _, err := legacyledger.VerifyCommittedChain(t.Context(), ledgerDir, headDir)
	if err != nil {
		t.Fatal(err)
	}
	identity := fmt.Sprintf(`{"instance_id":"70000000-0000-4000-8000-000000000001","epoch":1,"generation":%d,"revision":%d,"event_hash":"%x"}`+"\n",
		head.Generation, head.Revision, head.EventHash)
	if err := os.WriteFile(filepath.Join(headDir, "identity.json"), []byte(identity), 0o600); err != nil {
		t.Fatal(err)
	}
	return ledgerDir, headDir
}

func TestWalkVerifiedMatchesVerifyCommittedChain(t *testing.T) {
	ledgerDir, headDir := fixtureWithIdentity(t)
	head, state, err := legacyledger.VerifyCommittedChain(t.Context(), ledgerDir, headDir)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []legacyledger.EventKind
	var revisions []uint64
	verified, err := legacyledger.WalkVerified(t.Context(), ledgerDir, headDir, func(c legacyledger.Candidate, e legacyledger.Event) error {
		kinds, revisions = append(kinds, e.Kind), append(revisions, e.Revision)
		if sha256.Sum256(c.EventJSON) != c.EventHash {
			t.Fatalf("revision %d handed over with a hash that is not its envelope's", e.Revision)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if verified.Head != head || verified.Events != 3 || !slices.Equal(revisions, []uint64{1, 2, 3}) || len(kinds) != 3 {
		t.Fatalf("verified %+v revisions %v kinds %v", verified.Head, revisions, kinds)
	}
	if !reflect.DeepEqual(verified.State.Accounts(), state.Accounts()) || !reflect.DeepEqual(verified.State.Families(), state.Families()) ||
		!reflect.DeepEqual(verified.State.AllMemberships(), state.AllMemberships()) {
		t.Fatal("the streamed replay differs from Replay")
	}
	if verified.Identity.InstanceID != "70000000-0000-4000-8000-000000000001" || verified.Identity.TrustedHead != head || verified.ActorKey != nil {
		t.Fatalf("identity %+v actor key %+v", verified.Identity, verified.ActorKey)
	}
}

func TestWalkVerifiedStopsAtTheFirstVisitError(t *testing.T) {
	ledgerDir, headDir := fixtureWithIdentity(t)
	stop := errors.New("stop")
	var seen int
	_, err := legacyledger.WalkVerified(t.Context(), ledgerDir, headDir, func(legacyledger.Candidate, legacyledger.Event) error {
		seen++
		if seen == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || seen != 2 {
		t.Fatalf("err %v after %d events", err, seen)
	}
}

func TestWalkVerifiedRefusesATrustedHeadOffTheChain(t *testing.T) {
	ledgerDir, headDir := fixtureWithIdentity(t)
	forged := `{"instance_id":"70000000-0000-4000-8000-000000000001","epoch":1,"generation":1,"revision":2,"event_hash":"` + strings.Repeat("ab", 32) + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(headDir, "identity.json"), []byte(forged), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := legacyledger.WalkVerified(t.Context(), ledgerDir, headDir, nil); !errors.Is(err, legacyledger.ErrRecoveryIdentityMismatch) {
		t.Fatalf("a trusted head that is not revision 2's hash: %v", err)
	}
	if err := os.Remove(filepath.Join(headDir, "identity.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := legacyledger.WalkVerified(t.Context(), ledgerDir, headDir, nil); !errors.Is(err, legacyledger.ErrRecoveryIdentityMissing) {
		t.Fatalf("no identity.json: %v", err)
	}
}

func TestWalkVerifiedWritesNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permission bits do not block writes")
	}
	ledgerDir, headDir := fixtureWithIdentity(t)
	before := treeListing(t, ledgerDir) + treeListing(t, headDir)
	for _, dir := range []string{ledgerDir, headDir} {
		if err := chmodTreeReadOnly(dir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = chmodTreeWritable(dir) })
	}
	if _, err := legacyledger.WalkVerified(t.Context(), ledgerDir, headDir, nil); err != nil {
		t.Fatal(err)
	}
	if after := treeListing(t, ledgerDir) + treeListing(t, headDir); after != before {
		t.Fatalf("the walk changed its input:\n%s\n%s", before, after)
	}
}

// A long chain written by ledgertest streams: 5,000 events, each handed over once, in order.
func TestWalkVerifiedStreamsALongChain(t *testing.T) {
	w := ledgertest.New(t)
	account := "10000000-0000-4000-8000-000000000001"
	for i := 1; i <= 5000; i++ {
		w.Append(legacyledger.Event{Version: "v1", CommandID: fmt.Sprintf("30000000-0000-4000-8000-%012d", i), CommandHash: strings.Repeat("a", 64),
			ActorAccountID: account, Kind: "account_registered", At: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano),
			Effects: []legacyledger.Effect{{Kind: "account_registered", AccountID: fmt.Sprintf("10000000-0000-4000-8000-%012d", i), IdentityKey: fmt.Sprintf("opaque-%d", i), Status: "active"}}})
	}
	ledgerDir, headDir := w.Finish("70000000-0000-4000-8000-000000000001", 1, nil)
	var last uint64
	verified, err := legacyledger.WalkVerified(t.Context(), ledgerDir, headDir, func(_ legacyledger.Candidate, e legacyledger.Event) error {
		if e.Revision != last+1 {
			t.Fatalf("revision %d after %d", e.Revision, last)
		}
		last = e.Revision
		return nil
	})
	if err != nil || verified.Events != 5000 || len(verified.State.Accounts()) != 5000 {
		t.Fatalf("%v events=%d accounts=%d", err, verified.Events, len(verified.State.Accounts()))
	}
}
```
  - `enumerate_test.go`, `TestEnumeratorsAreSortedAndDetached`: over the fixture chain, each enumerator returns its entries sorted by key. A second call after the caller sets `entries[0].View.Status = "x"` still returns `"active"`. `AllMemberships()` equals the union of `Memberships(family)` over `Families()`.
  - `sidecar_test.go`, `TestSidecarsAreStrict`:
    - `identity.json` with an unknown field, a duplicate key, a non-canonical UUID, epoch 0, or a 4,097-byte file is `ErrLedgerCorrupt`;
    - `actor-id-key.json` `{"version":<main's actorIDKeyVersion>,"instance_id":…,"commitment":<64 hex>}` parses, and the same with 63 hex characters is `ErrLedgerCorrupt`;
    - a missing `actor-id-key.json` returns `(nil, nil)`.

    Read `main`'s version constant with `git show main:Server/internal/recovery/actor_key.go`.
  - `dependencies_test.go`, `TestOnlyTheImporterImportsLegacyledger`:
    - run `go list -f '{{.ImportPath}} {{join .Imports " "}} {{join .TestImports " "}} {{join .XTestImports " "}}' ./...` from `Server` (`exec.Command("go", …)`, `Dir: "../.."`);
    - fail on any package outside `internal/legacyledger`, `internal/legacyledger/ledgertest`, `internal/legacyimport` and `internal/legacyimport/legacytest` that names `babytracker/server/internal/legacyledger` or its `ledgertest`.
- [ ] **Step 2: Run to see the failure:** `rtk go -C Server test ./internal/legacyledger/... -count=1`. Expected: `undefined: legacyledger.WalkVerified`, `undefined: ledgertest.New`.
- [ ] **Step 3: Implement** `walk.go`:
```go
func WalkVerified(ctx context.Context, ledgerDir, headDir string, visit func(Candidate, Event) error) (Verified, error) {
	if visit == nil {
		visit = func(Candidate, Event) error { return nil }
	}
	heads, err := openHeadStore(headDir)
	if err != nil {
		return Verified{}, err
	}
	head, err := heads.Load(ctx)
	if err != nil {
		return Verified{}, err
	}
	identity, err := ReadRecoveryIdentity(headDir)
	if err != nil {
		return Verified{}, err
	}
	actorKey, err := ReadActorKeySidecar(headDir)
	if err != nil {
		return Verified{}, err
	}
	trusted := identity.TrustedHead
	if trusted.Generation != head.Generation || trusted.Revision > head.Revision {
		return Verified{}, ErrRecoveryIdentityMismatch
	}
	ledger, err := openLedger(ledgerDir)
	if err != nil {
		return Verified{}, err
	}
	state := NewState()
	prior := Head{Generation: 1}
	trustedOnChain := trusted.Revision == 0 && trusted.EventHash == [32]byte{}
	err = ledger.WalkCommitted(ctx, head, func(candidate Candidate) error {
		if err := validateCandidate(prior, candidate); err != nil {
			return err
		}
		var event Event
		if err := decodeEventStrict(candidate.EventJSON, &event); err != nil {
			return ErrLedgerCorrupt
		}
		if err := state.apply(event, candidate.EventHash); err != nil {
			return err
		}
		prior = Head{Generation: event.Generation, Revision: event.Revision, EventHash: candidate.EventHash}
		if prior.Revision == trusted.Revision {
			trustedOnChain = prior.EventHash == trusted.EventHash
		}
		return visit(candidate, event)
	})
	if err != nil {
		return Verified{}, err
	}
	state.head = prior
	if prior != head {
		return Verified{}, ErrLedgerCorrupt
	}
	if !trustedOnChain {
		return Verified{}, ErrRecoveryIdentityMismatch
	}
	return Verified{Head: head, State: state, Identity: identity, ActorKey: actorKey, Events: head.Revision}, nil
}
```
  - **`sidecar.go`.** Both readers:
    - check `resolveDir`;
    - read with `readFileBounded(context.Background(), path, 4096)`;
    - decode with `decodeDiskJSON` into structs tagged `instance_id`, `epoch`, `generation`, `revision`, `event_hash`, and `version`, `instance_id`, `commitment` respectively;
    - validate with `validUUID`, an epoch of 1..2^53-1, `fromDiskHead`, and the exact version constant and 64 lowercase hex characters.

    Neither creates or writes anything.
  - **`enumerate.go`.** Each enumerator copies the map into a slice of `Entry`, sorted with `slices.SortFunc` by key. Views are values; `IntegrationView` and any view with slices are cloned.
  - **`ledgertest.Writer`.** `Append` fills `Generation: 1`, `Revision: n+1` and `PreviousHash: hex(prior hash)`, encodes with `legacyledger.EncodeEvent`, and writes `generation-%020d/%020d-<hash>.json` as `json.Marshal({command_id, prior{generation,revision,event_hash}, event, event_hash})` + `"\n"`, mode 0600 (`ledger_test.go:32-55`). `Finish` writes `head.json` as `ledger_test.go:57-68` does, plus the two sidecars.
  - **`doc.go`.** Change "Unused by the runtime; consumed only by the Phase 4 importer" to name `internal/legacyimport` as the consumer.
- [ ] **Step 4: Run green:** `rtk go -C Server test ./internal/legacyledger/... -race -count=1` and `(cd Server && rtk go vet ./internal/legacyledger/...)`.
- [ ] **Step 5: Commit:** `rtk git add Server/internal/legacyledger && rtk git commit -m "feat(legacyledger): stream the verified replay, enumerate the state, read the head-directory sidecars"`



### Task 3: Detect the homelab's starting state, and vendor its legacy chain [Wave A]

**Files:**
- Create `Server/internal/legacyimport/{doc.go,errors.go,known.go,detect.go,detect_test.go,export_test.go}`.
- Create `Server/internal/legacyimport/legacytest/{legacy.go,chain_test.go}`.
- Create `Server/internal/legacyimport/legacytest/testdata/legacy-chain/` from `998501b`'s 28 files. Each is written byte-for-byte: `git show 998501b589a7:Server/migrations/<file> > …`. Add `manifest.json` = `git show 998501b589a7:Server/internal/migrate/compatibility.json`.

  This is what lets the importer and its tests build the homelab's legacy schema after Task 26 squashes the branch's own migrations.

**Interfaces:** Consumes `migrate.New`, `migrate.MigrationDigest` and `testpg`. Produces:
```go
package legacyimport

type coded string
func (c coded) Error() string { return string(c) }
// Code is err's importer code, or "service_unavailable".
func Code(err error) string

var (
	ErrNothingToImport    error = coded("nothing_to_import")
	ErrAlreadyImported    error = coded("already_imported")
	ErrNotHomelabState    error = coded("not_the_homelab_legacy_state")
	ErrStagingPresent     error = coded("legacy_staging_present")
	ErrServerRunning      error = coded("server_running")
	ErrSchemaOwner        error = coded("public_schema_owner_mismatch")
	ErrProjectionBehind   error = coded("legacy_projection_behind")
	ErrProjectionMismatch error = coded("legacy_projection_mismatch")
	ErrIdentityMismatch   error = coded("legacy_identity_mismatch")
	ErrLedgerChanged      error = coded("legacy_ledger_changed")
	ErrEventFromTheFuture error = coded("legacy_event_from_the_future")
	ErrIdentityMaterial   error = coded("identity_material_missing")
	ErrForeignDevice      error = coded("legacy_foreign_device")
	ErrCareMismatch       error = coded("legacy_care_mismatch")
	ErrChangedSeqPastHigh error = coded("changed_seq_past_high_water")
	ErrFamilyOverQuota    error = coded("family_over_quota")
	ErrBackupReceipt      error = coded("backup_receipt_mismatch")
	ErrVerificationFailed error = coded("import_verification_failed")
)

type StartKind string
const (
	StartEmpty       StartKind = "empty"
	StartHomelab28   StartKind = "homelab_28" // exactly 998501b's 28 rows
	StartImported    StartKind = "imported"   // the new schema with legacy_imported anchors
	StartNewSchema   StartKind = "new_schema" // the new schema without anchors (a fresh install)
	StartUnsupported StartKind = "unsupported"
)
type Start struct {
	Kind         StartKind
	Version      int64  // the highest applied version
	Name, SHA256 string // that version's row
	Detail       string // the refused row or condition
}
// Detect reads catalogs, schema_migrations and, on the new schema, the anchors
// (q has app.authority set). It returns nil only for StartHomelab28.
func Detect(ctx context.Context, q pgx.Tx) (Start, error)
// checkAlone refuses while any other client backend is connected to the database.
func checkAlone(ctx context.Context, q pgx.Tx) error

// known.go: 998501b's 28 rows, literal (version, name, sha256), equal to
// legacytest/testdata/legacy-chain/manifest.json (pinned by chain_test.go).
var HomelabChain = []migrate.MigrationDigest{ /* 28 literals: 1..27 as this branch's compatibility.json today; 28 = {28, "000028_live_activity_push.sql", "43b37464fd4570ae65eb2a84e3579e7e76dbc9e6c4b4ca3d598a2a74f3bc521e"} */ }

package legacytest
//go:embed testdata/legacy-chain/*.sql
var legacyChain embed.FS
// NewLegacy returns a disposable database migrated through the homelab's 28
// legacy migrations as main's runner applied them (migrate.New over the vendored
// files, as the testpg owner). It first creates the cluster roles main's chain
// grants to, if missing (NOLOGIN): baby_tracker_projector, baby_tracker_apple_revoker
// and baby_tracker_live_activity. After Task 27 the branch's init-roles.sql no
// longer creates the first two.
func NewLegacy(t *testing.T) *testpg.Fixture
// NewLegacyThrough is NewLegacy stopped at version (for the refusal tests).
func NewLegacyThrough(t *testing.T, version int64) *testpg.Fixture
// AuthorityTx begins a transaction on pool with app.authority set.
func AuthorityTx(t *testing.T, pool *pgxpool.Pool) pgx.Tx
```
`testpg` gains `NewFrom(t *testing.T, source fs.FS) *Fixture`, which is `newFixture` over any source. It is also used by Task 26's tests.

- [ ] **Step 1: Write the failing tests.**
  - `legacytest/chain_test.go`, `TestVendoredChainIsTheHomelabs`: each vendored file's sha256 equals its row in `manifest.json`; `manifest.json` has `min = max = 28`; `legacyimport.HomelabChain` equals the manifest's `known` exactly.
  - `detect_test.go`:
```go
func detect(t *testing.T, f *testpg.Fixture) (legacyimport.Start, error) {
	t.Helper()
	tx := legacytest.AuthorityTx(t, f.Owner)
	defer tx.Rollback(context.Background())
	return legacyimport.Detect(t.Context(), tx)
}

func TestDetectAcceptsOnlyTheHomelabState(t *testing.T) {
	for _, c := range []struct {
		name  string
		build func(t *testing.T) *testpg.Fixture
		kind  legacyimport.StartKind
		err   error
		want  string // a substring of Detail
	}{
		{"no migrations", func(t *testing.T) *testpg.Fixture { return testpg.NewThrough(t, 0) }, legacyimport.StartEmpty, legacyimport.ErrNothingToImport, ""},
		{"legacy 27", func(t *testing.T) *testpg.Fixture { return legacytest.NewLegacyThrough(t, 27) }, legacyimport.StartUnsupported, legacyimport.ErrNotHomelabState, "schema 27"},
		{"homelab 28", func(t *testing.T) *testpg.Fixture { return legacytest.NewLegacy(t) }, legacyimport.StartHomelab28, nil, ""},
		{"another 28", func(t *testing.T) *testpg.Fixture {
			f := legacytest.NewLegacy(t)
			exec(t, f.Owner, `UPDATE schema_migrations SET name='000028_mcp_integrations.sql', sha256=repeat('4',64) WHERE version=28`)
			return f
		}, legacyimport.StartUnsupported, legacyimport.ErrNotHomelabState, "version 28: 000028_mcp_integrations.sql"},
		{"a 29th row", func(t *testing.T) *testpg.Fixture {
			f := legacytest.NewLegacy(t)
			exec(t, f.Owner, `INSERT INTO schema_migrations(version,name,sha256) VALUES (29,'000029_baby_icon.sql',repeat('1',64))`)
			return f
		}, legacyimport.StartUnsupported, legacyimport.ErrNotHomelabState, "version 29: 000029_baby_icon.sql"},
		{"fresh new schema", func(t *testing.T) *testpg.Fixture { return testpg.New(t) }, legacyimport.StartNewSchema, legacyimport.ErrNothingToImport, ""},
		{"imported new schema", func(t *testing.T) *testpg.Fixture {
			f := testpg.New(t)
			tx := legacytest.AuthorityTx(t, f.Owner)
			exec(t, tx, `INSERT INTO security_audit_events(scope,scope_id,seq,prev_hash,hash,kind,command_id,actor_account_id,at,payload)
				VALUES ('account','a1000000-0000-4000-8000-000000000001',1,sha256('p'::bytea),sha256('h'::bytea),'legacy_imported',
				'c1000000-0000-4000-8000-000000000001','c1000000-0000-4000-8000-000000000001',now(),'{}')`)
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			return f
		}, legacyimport.StartImported, legacyimport.ErrAlreadyImported, ""},
		{"staging left behind", func(t *testing.T) *testpg.Fixture {
			f := legacytest.NewLegacy(t)
			exec(t, f.Owner, `CREATE SCHEMA legacy_public`)
			return f
		}, legacyimport.StartUnsupported, legacyimport.ErrStagingPresent, "legacy_public"},
	} {
		t.Run(c.name, func(t *testing.T) {
			start, err := detect(t, c.build(t))
			if start.Kind != c.kind || !errors.Is(err, c.err) || !strings.Contains(start.Detail, c.want) {
				t.Fatalf("start %+v err %v; want %s %v %q", start, err, c.kind, c.err, c.want)
			}
		})
	}
}

func TestCheckAloneRefusesWhileAnotherClientIsConnected(t *testing.T) {
	f := legacytest.NewLegacy(t)
	f.Runtime.Close()
	f.Revoker.Close()
	f.Maintenance.Close()
	tx := legacytest.AuthorityTx(t, f.Owner)
	if err := legacyimport.CheckAloneForTest(t.Context(), tx); err != nil {
		t.Fatalf("alone: %v", err)
	}
	_ = tx.Rollback(t.Context())
	other, err := pgx.Connect(t.Context(), f.Owner.Config().ConnString()) // a server process holding a connection
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(context.Background())
	tx = legacytest.AuthorityTx(t, f.Owner)
	defer tx.Rollback(context.Background())
	if err := legacyimport.CheckAloneForTest(t.Context(), tx); !errors.Is(err, legacyimport.ErrServerRunning) {
		t.Fatalf("another connection: %v", err)
	}
}
```
  `exec(t, q, sql)` is a three-line helper that fails the test on error. `export_test.go` exports `CheckAloneForTest = checkAlone`. `f.Revoker.Close()` loses its line in Task 27 when the pool goes.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/legacyimport/...' BT_TEST_RUN='TestDetect|TestCheckAlone|TestVendoredChain' bash Server/scripts/test-db.sh`. Expected: `package babytracker/server/internal/legacyimport is not in std`.
- [ ] **Step 3: Implement.** `Detect`, in order:
  1. A `legacy_public` or `legacy_app` schema means `ErrStagingPresent`.
  2. No `schema_migrations`, or no rows, means `StartEmpty`/`ErrNothingToImport`.
  3. Rows exactly equal to the embedded manifest (the new schema) mean: anchors present gives `StartImported`/`ErrAlreadyImported`; otherwise `StartNewSchema`/`ErrNothingToImport`.
  4. Rows exactly equal to `HomelabChain` mean `StartHomelab28`, nil.
  5. Otherwise `StartUnsupported`/`ErrNotHomelabState`. A shorter matching prefix gives `"schema %d"`; a differing or extra row gives `"version %d: %s %s"`.

  `checkAlone` runs `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid() AND backend_type = 'client backend'`. A count above 0 is `ErrServerRunning`.

  `NewLegacy` runs `migrate.New(sub-FS of legacyChain).Apply` through `testpg.NewFrom`, after creating `baby_tracker_projector`, `baby_tracker_apple_revoker` and `baby_tracker_live_activity` NOLOGIN if they are missing (as superuser, as `main`'s `init-roles.sql` did).
- [ ] **Step 4: Run green:** the Step 2 command, then `(cd Server && rtk go vet ./internal/legacyimport/... ./internal/testpg)`.
- [ ] **Step 5: Commit:** `rtk git add Server/internal/legacyimport Server/internal/testpg && rtk git commit -m "feat(legacyimport): vendor the homelab's legacy chain (998501b) and accept exactly its schema 28"`

**Revision 3 amendment: re-pin to `homelab_29`.**
- **State.** Task 3 is complete as `4364c7fe` and `4365c72f`, integrated as `c31cc675`/`acfbf774`. Steps 6–10 are one follow-up commit on the integrated head.
- **Runs first.** Tasks 1, 4 and 6's amendments read its manifest.
- **What carries over.** Everything not named here stays as committed:
  - the 18 codes;
  - `checkAlone`'s broader predicate and its `server_running` detail (role and `application_name`);
  - `NewThrough(t, 0)` as an empty database;
  - `testpg.NewFrom`.
- [ ] **Step 6: Vendor `fce422d5`'s chain.** From the repository root:
```bash
dir=Server/internal/legacyimport/legacytest/testdata/legacy-chain
for f in $(rtk git ls-tree --name-only fce422d51ef4fbd4ac98a4bab4e4375ff6e762a6 Server/migrations/ | grep -E '/0000(0[1-9]|1[0-9]|2[0-9])_.*\.sql$'); do
  rtk git show "fce422d51ef4fbd4ac98a4bab4e4375ff6e762a6:$f" > "$dir/$(basename "$f")"
done
rtk git show fce422d51ef4fbd4ac98a4bab4e4375ff6e762a6:Server/internal/migrate/compatibility.json > "$dir/manifest.json"
rtk proxy shasum -a 256 "$dir/000029_baby_icon.sql"
```
  Expected:
  - files 000001–000028 are unchanged (`rtk git status` shows only `000029_baby_icon.sql` added and `manifest.json` modified);
  - the shasum line is `8a296b40a8a1aba5e8d4f0256a8bb2aa050c50c8ea30a6d9ae2d6cfd9e087288`;
  - `manifest.json` has `min = max = 29` and 29 `known` rows.
- [ ] **Step 7: `known.go` and the start kind.**
  - `HomelabChain` gains the literal `{29, "000029_baby_icon.sql", "8a296b40a8a1aba5e8d4f0256a8bb2aa050c50c8ea30a6d9ae2d6cfd9e087288"}`. Its comment names `fce422d5`'s 29 rows.
  - `StartHomelab28` becomes `StartHomelab29 StartKind = "homelab_29"` everywhere: `detect.go`, the tests and the doc comments. `Detect` returns nil only for it.
  - `NewLegacy`'s comment says "the homelab's 29 legacy migrations". The three NOLOGIN roles it creates are unchanged, because 000029 grants nothing new.
- [ ] **Step 8: Detect's detail for a prefix names the first missing row.** A shorter matching prefix is now `"schema %d; first missing version %d: %s %s"`: the applied max, then `HomelabChain`'s next row's version, name and sha256. A differing or extra row stays `"version %d: %s %s"`. The legacy 28 at `998501b` is therefore refused with both `schema 28` and `version 29: 000029_baby_icon.sql` in its detail.
- [ ] **Step 9: Tests.**
  - `chain_test.go`, `TestVendoredChainIsTheHomelabs`:
    - `min = max = 29`;
    - 29 files;
    - `HomelabChain` equals the manifest's `known`;
    - row 29's sha256 is the literal above.
  - `detect_test.go`, `TestDetectAcceptsOnlyTheHomelabState`. The cases become:

    | Case | Build | Kind | Error | Detail contains |
    |---|---|---|---|---|
    | no migrations | `testpg.NewThrough(t, 0)` | `StartEmpty` | `ErrNothingToImport` | — |
    | legacy 27 | `NewLegacyThrough(t, 27)` | `StartUnsupported` | `ErrNotHomelabState` | `schema 27; first missing version 28: 000028_live_activity_push.sql` |
    | legacy 28 at `998501b` (revision 2's pin) | `NewLegacyThrough(t, 28)` | `StartUnsupported` | `ErrNotHomelabState` | `schema 28; first missing version 29: 000029_baby_icon.sql` |
    | homelab 29 | `NewLegacy(t)` | `StartHomelab29` | nil | — |
    | another 29 | `NewLegacy`, then `UPDATE schema_migrations SET sha256=repeat('1',64) WHERE version=29` | `StartUnsupported` | `ErrNotHomelabState` | `version 29: 000029_baby_icon.sql 1111` |
    | a 30th row | `NewLegacy`, then `INSERT … VALUES (30,'000030_next.sql',repeat('3',64))` | `StartUnsupported` | `ErrNotHomelabState` | `version 30: 000030_next.sql` |
    | the branch's own 29 | `testpg.NewThrough(t, 29)` (this branch's chain through `000029_postgres_authority.sql`) | `StartUnsupported` | `ErrNotHomelabState` | `version 28: 000028_mcp_integrations.sql` (the first row that differs) |
    | fresh new schema, imported new schema, staging left behind | as committed | as committed | as committed | as committed |

    The review follow-up's `legacy_app` staging case and the sha256-detail case stay.
  - `TestCheckAlone*` is unchanged; it runs on a 29-row legacy fixture.
- [ ] **Step 10: Run green and commit.**
  - Run `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/legacyimport/... ./internal/testpg' BT_TEST_RUN='TestDetect|TestCheckAlone|TestVendoredChain' bash Server/scripts/test-db.sh`, then `(cd Server && rtk go vet ./internal/legacyimport/... ./internal/testpg)`.
  - Mutants to report: drop row 29 from `HomelabChain`; accept a prefix as `StartHomelab29`; compare names but not digests. Each must fail a case.
  - Commit: `rtk git add Server/internal/legacyimport && rtk git commit -m "fix(legacyimport): the homelab moved to fce422d5; vendor its 29 migrations and accept exactly homelab_29"`.

### Task 4: The embedded chain inside the import transaction, staging, and the permanent catalog snapshot [Wave B; after Task 3]

**Files:**
- Modify `Server/internal/migrate/migrate.go`: add `ApplyTx`. `ensureTable`, `appliedMigrations` and the body of `applyOne` take a small executor interface (`Exec`, `Query`, `QueryRow`) that both `*pgxpool.Conn` and `pgx.Tx` satisfy. `Apply` behaves as before.
- Create `Server/internal/migrate/apply_tx_test.go`.
- Create `Server/internal/testpg/catalog.go` and `catalog_test.go`. They are permanent: Task 26's golden catalog test uses them after the squash and the importer are gone.
- Create `Server/internal/legacyimport/stage.go` and `stage_test.go`.

**Interfaces:** Produces:
```go
package migrate
// ApplyTx applies every embedded migration missing from the database inside tx,
// the caller's transaction (the caller holds the advisory lock). The applied
// history is validated exactly as Apply validates it, before anything runs.
func (r *Runner) ApplyTx(ctx context.Context, tx pgx.Tx) error

package testpg
// Catalog is a normalized, sorted description of the public and app schemas:
//   - schema owners and ACLs;
//   - relations: kind, reloptions, ACL, row security, forced row security, partition bounds;
//   - columns: type, not null, default, identity;
//   - constraints (pg_get_constraintdef) and indexes (pg_get_indexdef);
//   - policies: command, roles, USING, WITH CHECK;
//   - triggers: pg_get_triggerdef, tgenabled;
//   - functions: regprocedure, security definer, proconfig, ACL, md5 of the source;
//   - types and enum labels;
//   - the schema_privilege_manifest rows of the current version, and schema_compatibility.current_version.
// Monthly audit partitions (security_audit_events_YYYY_MM) are collapsed into
// one line, first=expected (the first month is the one ensure_audit_partitions
// would create today) and the count through now()+92 days, so the snapshot
// does not depend on the day or the session time zone it runs in. schema_migrations is not
// part of it (the squash changes the history, not the catalog). Login role
// names that differ between fixtures are replaced through roles.
type Catalog struct{ Lines []string }
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
func SnapshotCatalog(t testing.TB, q Querier, roles map[string]string) Catalog
// Roles maps f's per-test login roles to "<owner>", "<runtime>", "<revoker>", "<maintenance>".
func Roles(t testing.TB, f *Fixture) map[string]string
// Diff lists the lines only in c ("- …") or only in other ("+ …").
func (c Catalog) Diff(other Catalog) []string
// ProductionShape makes f's public schema look like a deployed one
// (deploy/postgres/init-users.sh:193): owned by the migrating login, no CREATE for PUBLIC.
func ProductionShape(t testing.TB, f *Fixture)
// Superuser is a superuser pool on f's database (the test-db.sh DSN with f's database name), closed at cleanup.
func Superuser(t testing.TB, f *Fixture) *pgxpool.Pool
// TableChecksums is md5(string_agg(row text ORDER BY row text)) per table in schema,
// read as a superuser (no RLS).
func TableChecksums(t testing.TB, f *Fixture, schema string) map[string]string

package legacyimport
const (
	legacyPublic = "legacy_public"
	legacyApp    = "legacy_app"
)
// stage moves the legacy schemas aside and creates the new schema exactly as a
// fresh install does, inside tx:
//   - public must be owned by current_user (ErrSchemaOwner otherwise);
//   - public and app are renamed legacy_public and legacy_app;
//   - a new public is created with only the legacy public's PUBLIC USAGE (every
//     role grant is re-created by the migrations);
//   - the embedded chain is applied with ApplyTx.
func stage(ctx context.Context, tx pgx.Tx, runner *migrate.Runner) error
```

- [ ] **Step 1: Write the failing tests.**
  - `migrate/apply_tx_test.go`:
```go
func TestApplyTxAppliesTheRestInTheCallersTransaction(t *testing.T) {
	compatibility, err := migrate.EmbeddedCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	f := testpg.NewThrough(t, 0) // an empty database: the chain before Task 26, the baseline after it
	tx, err := f.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := migrate.New(migrations.Files).ApplyTx(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	var inside, rows int64
	_ = tx.QueryRow(t.Context(), `SELECT current_version FROM schema_compatibility WHERE singleton`).Scan(&inside)
	_ = tx.QueryRow(t.Context(), `SELECT count(*) FROM schema_migrations`).Scan(&rows)
	if inside != compatibility.Max || rows != int64(len(compatibility.Known)) {
		t.Fatalf("inside the transaction: version %d, %d rows; want %d and %d", inside, rows, compatibility.Max, len(compatibility.Known))
	}
	_ = tx.Rollback(t.Context())
	var exists bool
	_ = f.Owner.QueryRow(t.Context(), `SELECT to_regclass('public.schema_compatibility') IS NOT NULL`).Scan(&exists)
	if exists {
		t.Fatal("a rolled-back ApplyTx left the schema behind")
	}
}

func TestApplyTxRefusesAChangedHistoryBeforeRunningAnything(t *testing.T) {
	f := testpg.New(t)
	if _, err := f.Owner.Exec(t.Context(), `UPDATE schema_migrations SET sha256=repeat('0',64) WHERE version=(SELECT max(version) FROM schema_migrations)`); err != nil {
		t.Fatal(err)
	}
	tx, _ := f.Owner.Begin(t.Context())
	defer tx.Rollback(context.Background())
	if err := migrate.New(migrations.Files).ApplyTx(t.Context(), tx); !errors.Is(err, migrate.ErrChecksumMismatch) {
		t.Fatalf("a changed top row: %v", err)
	}
}
```
  - `testpg/catalog_test.go`, `TestCatalogSeesWhatMatters`:
    1. Snapshot a fresh `testpg.New(t)` with `ProductionShape`.
    2. In a transaction as owner, apply each change below in turn, snapshot, and assert a non-empty `Diff`, rolling back each time:
       - `ALTER TABLE public.sessions NO FORCE ROW LEVEL SECURITY`;
       - `ALTER TABLE public.accounts DISABLE TRIGGER USER`;
       - `GRANT CREATE ON SCHEMA public TO PUBLIC`;
       - `UPDATE schema_privilege_manifest SET privileges='SELECT' WHERE …` (one row);
       - `DROP POLICY push_token_authority ON public.device_push_tokens`.
    3. Assert that two fresh fixtures snapshot equal (role names normalized).
  - `legacyimport/stage_test.go`:
```go
func TestStagedCatalogEqualsAFreshInstall(t *testing.T) { // Review Focus 2
	fresh := testpg.New(t)
	testpg.ProductionShape(t, fresh)
	want := testpg.SnapshotCatalog(t, fresh.Owner, testpg.Roles(t, fresh))

	old := legacytest.NewLegacy(t)
	testpg.ProductionShape(t, old)
	roles := testpg.Roles(t, old)
	before := testpg.SnapshotCatalog(t, old.Owner, roles)
	tx, err := old.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := legacyimport.StageForTest(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	var legacyRows int
	if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM legacy_public.schema_migrations`).Scan(&legacyRows); err != nil || legacyRows != 28 {
		t.Fatalf("the legacy history is not readable inside the transaction: %d %v", legacyRows, err)
	}
	for _, s := range []string{`DROP SCHEMA legacy_public CASCADE`, `DROP SCHEMA legacy_app CASCADE`} {
		if _, err := tx.Exec(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	if diff := want.Diff(testpg.SnapshotCatalog(t, tx, roles)); len(diff) != 0 {
		t.Fatalf("the staged schema is not a fresh install:\n%s", strings.Join(diff, "\n"))
	}
	_ = tx.Rollback(t.Context())
	if diff := before.Diff(testpg.SnapshotCatalog(t, old.Owner, roles)); len(diff) != 0 {
		t.Fatalf("a rolled-back stage left traces:\n%s", strings.Join(diff, "\n"))
	}
}

func TestStageRefusesAPublicSchemaOwnedByAnotherRole(t *testing.T) {
	old := legacytest.NewLegacy(t) // public stays owned by pg_database_owner: not ProductionShape
	tx, _ := old.Owner.Begin(t.Context())
	defer tx.Rollback(context.Background())
	if err := legacyimport.StageForTest(t.Context(), tx); !errors.Is(err, legacyimport.ErrSchemaOwner) {
		t.Fatalf("public owned by pg_database_owner: %v", err)
	}
}
```
  `export_test.go` gains `func StageForTest(ctx context.Context, tx pgx.Tx) error { return stage(ctx, tx, migrate.New(migrations.Files)) }`.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/migrate ./internal/testpg ./internal/legacyimport/...' BT_TEST_RUN='TestApplyTx|TestCatalog|TestStage' bash Server/scripts/test-db.sh`. Expected: `ApplyTx undefined`, `undefined: testpg.SnapshotCatalog`, `StageForTest undefined`.
- [ ] **Step 3: Implement.**
  - **`ApplyTx`:** `loadMigrations(r.source)`, then `ensureTable(ctx, tx)`, `appliedMigrations(ctx, tx)` and `validateApplied(applied, migrations)`. Then, for every missing migration in order, `tx.Exec(ctx, string(m.sql))` (no arguments: pgx's simple protocol, many statements per file, as `applyOne` does) and the `schema_migrations` insert. Errors are `fmt.Errorf("migrate: %s: %w", m.name, err)`.
  - **`stage`:**
```go
func stage(ctx context.Context, tx pgx.Tx, runner *migrate.Runner) error {
	var owner, me string
	var publicUsage bool
	if err := tx.QueryRow(ctx, `SELECT pg_get_userbyid(n.nspowner), current_user,
			EXISTS (SELECT 1 FROM aclexplode(coalesce(n.nspacl, acldefault('n', n.nspowner))) a WHERE a.grantee = 0 AND a.privilege_type = 'USAGE')
		FROM pg_namespace n WHERE n.nspname = 'public'`).Scan(&owner, &me, &publicUsage); err != nil {
		return fmt.Errorf("legacyimport: stage: read public: %w", err)
	}
	if owner != me {
		return fmt.Errorf("%w: public is owned by %s, the import runs as %s", ErrSchemaOwner, owner, me)
	}
	steps := []string{
		`ALTER SCHEMA public RENAME TO ` + legacyPublic,
		`ALTER SCHEMA app RENAME TO ` + legacyApp,
		`CREATE SCHEMA public`,
	}
	if publicUsage {
		steps = append(steps, `GRANT USAGE ON SCHEMA public TO PUBLIC`)
	}
	for _, s := range steps {
		if _, err := tx.Exec(ctx, s); err != nil {
			return fmt.Errorf("legacyimport: stage: %s: %w", s, err)
		}
	}
	return runner.ApplyTx(ctx, tx)
}
```
  - **`SnapshotCatalog`.** One query per object class over `pg_catalog` with `nspname IN ('public','app')`. Each row becomes one line `kind|schema|name|attributes`, with role names passed through `roles` and the result sorted. Monthly audit partitions collapse into `audit_monthly|first=expected|through_now_plus_92d=<n>` (revision 3 amendment: never the month itself). OIDs, timestamps, statistics, sequence values and `schema_migrations`' rows are excluded.
  - **`ProductionShape`, `Superuser`:** `Superuser` parses `RequireDatabase(t)`, sets the database to `current_database()` read through `f.Owner`, and opens a 2-connection pool. `ProductionShape` runs `ALTER SCHEMA public OWNER TO <f.Owner's user>; REVOKE CREATE ON SCHEMA public FROM PUBLIC`.
  - **`TableChecksums`:** run through `Superuser`, for every ordinary or partitioned table of `schema` (partitioned tables only through their parent).
- [ ] **Step 4: Run green:** the Step 2 command, then `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/migrate ./internal/database' bash Server/scripts/test-db.sh` (`Apply` unchanged).
- [ ] **Step 5: Commit:** `rtk git add Server/internal/migrate Server/internal/testpg Server/internal/legacyimport && rtk git commit -m "feat(migrate): apply the chain inside a caller's transaction; stage legacy schemas aside; a permanent catalog snapshot"`

**Revision 3 amendment.**
- **State.** Task 4 is committed as `f4259efb`. Its review fix round is resuming; it folds I1, M1, M2 and M4.
- **Who does Step 6.** That fix round, or one follow-up commit after it, once Task 3's amendment is on its base.
- [ ] **Step 6: The legacy fixture is 29 rows, and the snapshot names no month.**
  - **Row counts.** `TestStagedCatalogEqualsAFreshInstall` reads the legacy history count as `len(legacyimport.HomelabChain)` (29), not the literal 28. Every other literal 28 in Task 4's tests that means "the homelab's row count" does the same.
  - **The month (review I1).** The monthly-partition line is `audit_monthly|first=expected|through_now_plus_92d=<n>`, where `expected` means the first month equals the one `app.ensure_audit_partitions` would create on that day. It never prints the month, so Task 26's golden does not rot from February 2027.
  - **The time zone (review M1).** The snapshot sets `TimeZone` to `UTC` for its own statements, so its output does not depend on the session's time zone.
  - **Run:** `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/migrate ./internal/testpg ./internal/legacyimport/...' BT_TEST_RUN='TestApplyTx|TestCatalog|TestStage' bash Server/scripts/test-db.sh`.
  - **Commit:** `rtk git add Server/internal/testpg Server/internal/legacyimport && rtk git commit -m "fix(testpg): the staged legacy history is the homelab's 29 rows; the catalog names no month"`.

  Carries out of the Task 4 review, owned elsewhere:
  - M3: Task 18 compares `schema_migrations` separately.
  - M5: no deferred constraint before the squash (Global Constraints).
  - M6: Task 27 runs a real `DROP ROLE`.

### Task 5: `authority` — legacy audit chains, bulk copy and the in-transaction verifier [Wave A]

**Files:**
- Create `Server/internal/authority/legacy_audit.go` and `legacy_audit_test.go`.
- Modify `Server/internal/authority/verifier.go`. `Verifier` gains an optional `tx pgx.Tx`: when it is set, `read` runs on it, and `fail` and `writeCheckpoints` record without writing. `VerifyChainsTx` builds one.
- Modify `Server/internal/authority/sessions.go:185`: add the exported `TokenDigest` beside `tokenDigest` (one line).

**Interfaces:** Consumes `auditRowHash`, `canonicalJSON`, `maxAuditPayloadBytes`, `scopeRef`, `chainHead`, `validID`, `validTime` and `Kind.valid` (`chain.go`, `service.go`). Produces:
```go
package authority

// LegacyEvent is one verified legacy ledger event, in ledger order.
type LegacyEvent struct {
	Kind       Kind            // the legacy kind; every legacy kind is an audit kind
	CommandID  string
	ActorID    string
	At         time.Time
	Payload    json.RawMessage // the legacy event object as committed
	LegacyHash [32]byte        // the candidate's event hash
	Accounts   []string        // account scopes the event's effects name
	Families   []string        // family scopes the event's effects name
}

// AuditRow is one security_audit_events row ready to copy.
type AuditRow struct {
	Scope      Scope
	ScopeID    string
	Seq        uint64
	PrevHash   [32]byte
	Hash       [32]byte
	Kind       Kind
	CommandID  string
	ActorID    string
	At         time.Time
	Payload    []byte    // canonical JSON, as hashed
	LegacyHash *[32]byte // nil on anchors
}

// ChainHead is one scope's head after the import, for accounts/families.audit_seq and audit_hash.
type ChainHead struct {
	Scope Scope
	ID    string
	Seq   uint64
	Hash  [32]byte
}

// LegacyAnchor is what every scope's legacy_imported row records.
type LegacyAnchor struct {
	ImportID   string    // the import run; also the anchor rows' command and actor ID
	At         time.Time // the import time
	Generation uint64
	Revision   uint64
	HeadHash   [32]byte
}

// LegacyChains builds every scope's chain in memory, with auditRowHash.
type LegacyChains struct{ /* heads map[scopeRef]*chainHead; events map[scopeRef]uint64 */ }
func NewLegacyChains() *LegacyChains
// Append chains e into every scope it names, accounts first then families,
// each in ID order, with the payload in canonical JSON and LegacyHash set.
// An invalid kind (or legacy_imported), ID, time, payload over 64 KiB, or an
// event naming no scope is ErrInvalidCommand.
func (c *LegacyChains) Append(e LegacyEvent) ([]AuditRow, error)
// Anchor appends one legacy_imported row to every scope Append saw and every
// scope in accounts and families, with payload
// {"import_id","legacy_events","legacy_generation","legacy_head","legacy_revision"}.
func (c *LegacyChains) Anchor(a LegacyAnchor, accounts, families []string) ([]AuditRow, error)
// Heads is every scope's head, sorted by (scope, ID).
func (c *LegacyChains) Heads() []ChainHead
// CopyAuditRows copies rows into security_audit_events (pgx CopyFrom). The
// caller has registered app.audit_scope and app.audit_kind on tx's connection
// (RegisterAuditTypes).
func CopyAuditRows(ctx context.Context, tx pgx.Tx, rows []AuditRow) (int64, error)
func RegisterAuditTypes(ctx context.Context, tx pgx.Tx) error
// VerifyChainsTx walks every account and family chain from genesis inside tx,
// read-only: no checkpoint is written and no chain is marked failed. Failed
// lists the mismatching scopes.
func VerifyChainsTx(ctx context.Context, tx pgx.Tx, now func() time.Time) (VerifyReport, error)
// TokenDigest is the session-token digest Authenticate looks up (tokenDigest).
func TokenDigest(key identity.DigestKey, class, token string) [32]byte
```

- [ ] **Step 1: Write the failing tests** `legacy_audit_test.go` (package `authority_test`, using `authoritytest.New`):
```go
func legacyEvent(t *testing.T, kind authority.Kind, n int, at time.Time, accounts, families []string) authority.LegacyEvent {
	t.Helper()
	payload := fmt.Sprintf(`{"kind":%q,"revision":%d,"note":"<b>&</b>","effects":[]}`, kind, n)
	return authority.LegacyEvent{Kind: kind, CommandID: fmt.Sprintf("c0000000-0000-4000-8000-%012d", n), ActorID: accounts[0], At: at,
		Payload: json.RawMessage(payload), LegacyHash: sha256.Sum256([]byte(payload)), Accounts: accounts, Families: families}
}

func TestLegacyChainsVerifyAndTamperFailsTheScope(t *testing.T) { // Review Focus 4
	f := authoritytest.New(t)
	a, b := f.Account(t), f.Account(t)
	fam := f.Family(t, a) // an active family owned by a (fixture helper)
	quiet := f.Account(t) // no legacy event: anchor only
	c := authority.NewLegacyChains()
	var rows []authority.AuditRow
	add := func(e authority.LegacyEvent) {
		r, err := c.Append(e)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, r...)
	}
	add(legacyEvent(t, authority.KindAccountRegistered, 1, time.Date(2025, 12, 31, 23, 59, 0, 0, time.UTC), []string{a}, nil)) // default partition
	add(legacyEvent(t, authority.KindFamilyCreated, 2, time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC), []string{a}, []string{fam}))
	add(legacyEvent(t, authority.KindInvitationRedeemed, 3, time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC), []string{b, a}, []string{fam})) // three scopes
	big := strings.Repeat("x", 64<<10-200)
	huge := authority.LegacyEvent{Kind: authority.KindSessionRotated, CommandID: "c0000000-0000-4000-8000-000000000004", ActorID: b,
		At: time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC), Payload: json.RawMessage(`{"pad":"` + big + `"}`), Accounts: []string{b}}
	add(huge)
	anchors, err := c.Anchor(authority.LegacyAnchor{ImportID: "1e000000-0000-4000-8000-000000000001", At: time.Date(2026, 10, 9, 20, 0, 0, 0, time.UTC),
		Generation: 1, Revision: 4, HeadHash: sha256.Sum256([]byte("head"))}, []string{a, b, quiet}, []string{fam})
	if err != nil {
		t.Fatal(err)
	}
	rows = append(rows, anchors...)
	if got := len(rows); got != 1+2+3+1+4 {
		t.Fatalf("%d rows", got)
	}
	tx := authorityTx(t, f) // owner pool, app.authority set, FORCE lifted on accounts/families as the importer does
	if err := authority.RegisterAuditTypes(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	if n, err := authority.CopyAuditRows(t.Context(), tx, rows); err != nil || n != int64(len(rows)) {
		t.Fatalf("copied %d: %v", n, err)
	}
	for _, h := range c.Heads() {
		table := map[authority.Scope]string{authority.ScopeAccount: "accounts", authority.ScopeFamily: "families"}[h.Scope]
		if _, err := tx.Exec(t.Context(), `UPDATE `+table+` SET audit_seq=$2, audit_hash=$3 WHERE id=$1`, h.ID, int64(h.Seq), h.Hash[:]); err != nil {
			t.Fatal(err)
		}
	}
	var inDefault int
	_ = tx.QueryRow(t.Context(), `SELECT count(*) FROM security_audit_events_default WHERE scope_id=$1`, a).Scan(&inDefault)
	if inDefault != 1 {
		t.Fatalf("the 2025 row is not in the default partition (%d)", inDefault)
	}
	report, err := authority.VerifyChainsTx(t.Context(), tx, time.Now)
	if err != nil || len(report.Failed) != 0 || report.Rows != len(rows) {
		t.Fatalf("verify %+v %v", report, err)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE security_audit_events SET payload = jsonb_set(payload,'{note}','"tampered"') WHERE scope_id=$1 AND seq=2`, fam); err != nil {
		t.Fatal(err)
	}
	report, err = authority.VerifyChainsTx(t.Context(), tx, time.Now)
	if err != nil || len(report.Failed) != 1 || report.Failed[0].ID != fam {
		t.Fatalf("a tampered family row: %+v %v", report, err)
	}
	var failed bool
	_ = tx.QueryRow(t.Context(), `SELECT chain_failed FROM families WHERE id=$1`, fam).Scan(&failed)
	if failed {
		t.Fatal("VerifyChainsTx wrote chain_failed")
	}
}

func TestLegacyChainsRefuseWhatTheChainCannotHold(t *testing.T) {
	c := authority.NewLegacyChains()
	a := "a1000000-0000-4000-8000-000000000001"
	at := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	for name, e := range map[string]authority.LegacyEvent{
		"anchor kind":      {Kind: authority.KindLegacyImported, CommandID: a, ActorID: a, At: at, Payload: json.RawMessage(`{}`), Accounts: []string{a}},
		"no scope":         {Kind: authority.KindSessionRotated, CommandID: a, ActorID: a, At: at, Payload: json.RawMessage(`{}`)},
		"fraction":         {Kind: authority.KindSessionRotated, CommandID: a, ActorID: a, At: at, Payload: json.RawMessage(`{"x":1.5}`), Accounts: []string{a}},
		"over 64 KiB":      {Kind: authority.KindSessionRotated, CommandID: a, ActorID: a, At: at, Payload: json.RawMessage(`{"x":"` + strings.Repeat("y", 64<<10) + `"}`), Accounts: []string{a}},
		"uppercase uuid":   {Kind: authority.KindSessionRotated, CommandID: strings.ToUpper(a), ActorID: a, At: at, Payload: json.RawMessage(`{}`), Accounts: []string{a}},
	} {
		if _, err := c.Append(e); !errors.Is(err, authority.ErrInvalidCommand) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
```
  `authorityTx(t, f)` is a local helper:
  1. begin on `f.DB.Owner`;
  2. `set_config('app.authority','1',true)`;
  3. `ALTER TABLE accounts NO FORCE ROW LEVEL SECURITY`, and the same for `families` and `security_audit_events`, exactly as the importer does in Task 13;
  4. roll back at cleanup.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/authority' BT_TEST_RUN='TestLegacyChains' bash Server/scripts/test-db.sh`. Expected: `undefined: authority.NewLegacyChains`.
- [ ] **Step 3: Implement** `legacy_audit.go`:
```go
func (c *LegacyChains) next(ref scopeRef, kind Kind, commandID, actorID string, at time.Time, payload []byte) AuditRow {
	head := c.heads[ref]
	if head == nil {
		head = &chainHead{Scope: ref}
		c.heads[ref] = head
	}
	seq := head.Seq + 1
	hash := auditRowHash(ref.kind, ref.id, seq, head.Hash, kind, commandID, actorID, at, payload)
	row := AuditRow{Scope: ref.kind, ScopeID: ref.id, Seq: seq, PrevHash: head.Hash, Hash: hash, Kind: kind, CommandID: commandID, ActorID: actorID, At: at, Payload: payload}
	head.Seq, head.Hash = seq, hash
	return row
}

func (c *LegacyChains) Append(e LegacyEvent) ([]AuditRow, error) {
	if !e.Kind.valid() || e.Kind == KindLegacyImported || !validID(e.CommandID) || !validID(e.ActorID) || !validTime(e.At) {
		return nil, ErrInvalidCommand
	}
	payload, err := canonicalJSON(e.Payload)
	if err != nil || len(payload) > maxAuditPayloadBytes {
		return nil, ErrInvalidCommand
	}
	scopes, ok := legacyScopes(e.Accounts, e.Families) // deduplicated; accounts first, then families, each sorted; every ID validID
	if !ok || len(scopes) == 0 {
		return nil, ErrInvalidCommand
	}
	at := e.At.UTC().Truncate(time.Microsecond)
	legacy := e.LegacyHash
	rows := make([]AuditRow, 0, len(scopes))
	for _, ref := range scopes {
		row := c.next(ref, e.Kind, e.CommandID, e.ActorID, at, payload)
		row.LegacyHash = &legacy
		rows = append(rows, row)
		c.events[ref]++
	}
	return rows, nil
}
```
  - `Anchor` gathers every scope from `c.heads` plus `accounts` and `families` into one sorted, deduplicated list, and calls `next` with `KindLegacyImported` and `canonicalJSON(map[string]any{"import_id": a.ImportID, "legacy_events": c.events[ref], "legacy_generation": a.Generation, "legacy_head": hex.EncodeToString(a.HeadHash[:]), "legacy_revision": a.Revision})`.
  - `CopyAuditRows` uses `tx.CopyFrom(ctx, pgx.Identifier{"security_audit_events"}, []string{"scope","scope_id","seq","prev_hash","hash","kind","command_id","actor_account_id","at","payload","legacy_hash"}, pgx.CopyFromSlice(...))`. Payload is passed as `json.RawMessage` (jsonb), `legacy_hash` as `[]byte` or nil.
  - `RegisterAuditTypes`: `types, err := tx.Conn().LoadTypes(ctx, []string{"app.audit_scope", "app.audit_kind"})` then `tx.Conn().TypeMap().RegisterTypes(types)`.
  - **`VerifyChainsTx`** is `(&Verifier{service: &Service{now: now}, tx: tx}).VerifyAll(ctx)`. In `verifier.go`:
    - `read` runs `fn(v.tx)` when `v.tx != nil`;
    - `fail` returns nil without writing when `v.tx != nil`;
    - `writeCheckpoints` is skipped when `v.tx != nil`.

    The walk itself is untouched, so the row check is the one production runs.
- [ ] **Step 4: Run green:** `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/authority' bash Server/scripts/test-db.sh` (the verifier's existing tests pass unchanged).
- [ ] **Step 5: Commit:** `rtk git add Server/internal/authority && rtk git commit -m "feat(authority): chain legacy events per scope, copy audit rows, verify chains inside a transaction"`



### Task 6: Capture a real homelab-shaped legacy fixture at `998501b`, Live Activity included [Wave B; after Task 3; long-running, Docker; recaptured at `fce422d5` by the revision 3 amendment]

**Files:**
- Create `Server/internal/legacyimport/capture_test.go` (`//go:build identitytest && legacycapture`). It drives the legacy server over HTTP and writes `client.json`.
- Create `Server/internal/legacyimport/legacytest/captured.go`: the `Captured*` types.
- Create `Server/scripts/capture-legacy-fixture.sh` and `Server/scripts/capture_legacy_fixture_test.go`.
- Create `Server/internal/legacyimport/fixtures_manifest_test.go`.
- Create the committed output `Server/internal/legacyimport/testdata/fixtures/homelab28/`: `manifest.json`, `data.sql.gz`, `ledger.tar.gz`, `secrets.tar.gz`, `env.json` and `client.json`.

**Which legacy code writes it:**
- `998501b589a7b97fca0b3240debb5a613b8bc94a`, the homelab's deployed source.
- Self-hosted mode, with `main`'s disposable Live Activity profile on (`BT_DISPOSABLE_LIVE_ACTIVITY=1`: its worker login, a throwaway APNs key and the identitytest recorder `/__identitytest/apns`), as the homelab runs with push.

**Interfaces:**
```go
package legacytest // captured.go: client.json
type CapturedClient struct {
	LegacySHA    string                     `json:"legacy_sha"`
	CapturedAt   time.Time                  `json:"captured_at"` // the server's Date header at the last request
	InstanceID   string                     `json:"instance_id"`
	Families     map[string]CapturedFamily  `json:"families"`    // "main", "deleting"
	Subjects     map[string]CapturedSubject `json:"subjects"`    // owner, caregiver, viewer, removed, outsider
	LiveActivity CapturedLiveActivity       `json:"live_activity"`
}
type CapturedFamily struct {
	ID        string `json:"id"`
	HighWater uint64 `json:"high_water"`
	MidCursor uint64 `json:"mid_cursor"` // the high water after golden step 5
}
type CapturedSubject struct {
	AccountID, DeviceID, Role, Family string
	AccessToken, RefreshToken         string            // raw, under the disposable server's throwaway keys
	PullFromZero                      []json.RawMessage // every change, all pages, in order
	PullFromMid                       []json.RawMessage
	SnapshotEntities                  []json.RawMessage // the union of every snapshot page
	Cursor                            protocol.Cursor   // its last final-page cursor
}
// The cards running at the end of the capture, as the legacy server last pushed them.
type CapturedLiveActivity struct {
	Registrations []CapturedRegistration `json:"registrations"` // owner's and caregiver's devices
	Cards         []CapturedCard         `json:"cards"`         // the caregiver's pushed card, with its update token registered
	PushesSeen    int                    `json:"pushes_seen"`   // recorder count at the end
}
type CapturedRegistration struct {
	Subject, InstallationID, PushToStartToken string
	RegistrationVersion                       uint64
}
type CapturedCard struct {
	Subject, ActivityID, UpdateToken, GenerationID, BabyID, KindRaw string
	TokenVersion                                                    uint64
	TimerID                                                         *string
}
```

- [ ] **Step 1: Write the failing tests.**
  - `fixtures_manifest_test.go` (package `legacyimport`), `TestCapturedFixtureIsPinned`:
    - every file in `manifest.json` has the sha256 it lists;
    - `manifest.schema` equals `HomelabChain`;
    - `secrets.tar.gz` contains no name ending in `.p8` and no file holding `-----BEGIN`;
    - every bearer token in `client.json` is 43 characters of base64url;
    - `captured_at` is in 2026;
    - `live_activity.cards` is non-empty and every card names a registration's subject.
  - `capture_legacy_fixture_test.go`, `TestCaptureLegacyFixturePins`. The script must have:
    - `set -euo pipefail`;
    - `git worktree add --detach` into `mktemp -d`, removed by an EXIT trap;
    - `export BT_DISPOSABLE_LIVE_ACTIVITY=1` and `start_disposable_sync self_hosted normal`;
    - `--data-only --column-inserts --exclude-table-data=schema_migrations`;
    - a secrets copy that excludes `*.p8`, `*password*`, `*database_url*` and PEM files;
    - a refusal when `BT_SERVER_INSTANCE_ID` is `<instance-uuid>`;
    - a refusal below 10 GB free.
- [ ] **Step 2: Run to see the failure:** `rtk go -C Server test ./scripts -run TestCaptureLegacyFixturePins -count=1` and `rtk go -C Server test ./internal/legacyimport -run TestCapturedFixtureIsPinned -count=1`. Expected: the script and the fixture are missing.
- [ ] **Step 3: Implement the capture.**
  - **`TestCaptureLegacyFixture`.** It reads `BT_LEGACY_CAPTURE_BASE` and `BT_LEGACY_CAPTURE_OUT` and skips without them. It uses the sign-in, sync and refresh helpers of `cmd/protocol-e2e-client/main.go` (`/__identitytest/proof`, then the auth exchange), copied into the test.
    1. Sign in `owner`, `caregiver`, `viewer`, `removed` and `outsider`, each on its own device.
    2. The owner creates the family and invites `caregiver` and `removed` as caregivers and `viewer` as a viewer. Each redeems, then refreshes into family scope.
    3. **Live Activity registration.** The owner and the caregiver each `PUT /v1/live-activities/device`. Bodies are built as JSON maps with `main`'s field names (`installation_id`, `registration_version` 1, `apns_environment` `sandbox`, `payload_version` 1, `push_to_start_token` 64 lowercase hex, `feed_break_minutes` 30); see `git show 998501b:Server/internal/protocol/live_activity.go`.
    4. Replay `testkit.GoldenSteps(t, nil)`: one `POST /v1/sync` per step from the step's subject, with its import resolve first when `Resolve` is set. Record `MidCursor` after step 5. The scenario leaves two timers running.
    5. **The pushed card.**
       - Poll `GET /__identitytest/apns` (up to 20 s) for the legacy worker's `start` push to the caregiver's push-to-start token, and read its `content-state.remote.generationID` and scope.
       - The caregiver then `PUT /v1/live-activities/activity` with `activity_id` (a UUID), an update `token` (64 hex), `token_version` 1, that `scope`, `known_generation_id` = the generation, the timer ID and `state` `active`.
       - Poll the recorder until an `update` to that token arrives.
       - Record `Registrations`, `Cards` and `PushesSeen`.
    6. The owner changes `viewer` to caregiver, then removes `removed`.
    7. `outsider` creates a second family, syncs one baby, and schedules its deletion.
    8. Every subject refreshes twice.
    9. For `owner`, `caregiver` and `viewer`: pull from 0 (`PullFromZero`, `Cursor`), pull from `MidCursor` (`PullFromMid`), and snapshot every page (`SnapshotEntities`).
    10. Write `client.json` with mode 0600.
  - **`capture-legacy-fixture.sh <legacy-sha> <name>`:**
    1. Check the refusals.
    2. Create the throwaway worktree at `<legacy-sha>`. In it, `source Server/scripts/lib/disposable-sync.sh` with `BT_DISPOSABLE_LIVE_ACTIVITY=1` and run `start_disposable_sync self_hosted normal`.
    3. From this branch: `(cd Server && GOCACHE=$(mktemp -d) BT_LEGACY_CAPTURE_BASE="$base" BT_LEGACY_CAPTURE_OUT="$out" go test -tags 'identitytest legacycapture' -run TestCaptureLegacyFixture -count=1 ./internal/legacyimport)`.
    4. Stop the app (`compose stop app`), then `docker exec "$db" pg_dump -U postgres -d babytracker --data-only --column-inserts --exclude-table-data=schema_migrations | gzip -n > "$out/data.sql.gz"`.
    5. `docker cp` the ledger and head directories from the app container and tar them reproducibly (`tar --sort=name --mtime=@0 --owner=0 --group=0 | gzip -n`) as `ledger.tar.gz`.
    6. Tar the key files of `$BT_SECRETS_DIR`, minus passwords, database URLs and the throwaway APNs key, as `secrets.tar.gz`.
    7. Write `env.json` (the app's `BT_*` with secret paths made relative; instance, epoch, mode, origin) and `manifest.json` (`name`, `legacy_sha`, `schema`, `ledger_head`, `captured_at`, `files` with their sha256).
    8. `stop_disposable_sync`.
  - **Run it once:** `rtk bash Server/scripts/capture-legacy-fixture.sh 998501b589a7b97fca0b3240debb5a613b8bc94a homelab28`.
- [ ] **Step 4: Run green:** the Step 2 commands; the six files exist, each under 2 MB; `rtk go -C Server test ./scripts -run TestNoProductionAPNsKeyInTheServerTree -count=1`.
- [ ] **Step 5: Commit:** `rtk git add Server/scripts/capture-legacy-fixture.sh Server/scripts/capture_legacy_fixture_test.go Server/internal/legacyimport/capture_test.go Server/internal/legacyimport/fixtures_manifest_test.go Server/internal/legacyimport/legacytest/captured.go Server/internal/legacyimport/testdata/fixtures && rtk git commit -m "test(legacyimport): capture a homelab-shaped legacy fixture at 998501b with Live Activity cards"`

**Revision 3 amendment: recapture at `fce422d5` as `homelab29`, with baby icons.**
- **State.** Task 6 is complete and approved as `650041d5`. Its fixture `homelab28` is at the state the homelab left, so it is replaced.
- **Who does Steps 6–11.** One follow-up task, after Task 3's amendment (`HomelabChain` has 29 rows).
- **The legacy harness.** It is `fce422d5`'s own `Server/scripts/lib/disposable-sync.sh`, run in the throwaway worktree. That harness gates on `schema == 29` and `29:29:000029_baby_icon.sql` (`git show fce422d5:Server/scripts/lib/disposable-sync.sh`, line 318), so it starts unchanged.
- **Rulings that stay.** The Task 6 review rulings stand:
  - the outsider re-signs in;
  - removed-member refreshes expect `session_revoked`;
  - `\restrict` lines are stripped;
  - no dump-collision fix at capture (Task 20's `Restore` handles it).
- [ ] **Step 6: Icons in the capture.** `TestCaptureLegacyFixture` gains **step 4a**, after the golden replay (step 4) and before the pushed card (step 5). Its bodies are JSON maps with `main`'s field names, exactly as a `fce422d5` phone builds them: the `icon` key present only when set.
  1. The owner `POST /v1/sync`s a `baby` `create` for a second baby, `"Icon baby"`, with `"sort_order":1,"icon":"girl.2"`.
  2. The caregiver then sends an `update` of that baby renamed `"Icon baby K"` **without** the `icon` key (an older build's shape). The legacy server keeps `girl.2` in the entity it journals (`main` `records/repository.go:234-243` at `fce422d5`). The test asserts both outcomes are `applied` and the second outcome's payload has `"icon":"girl.2"`.
  3. The golden scenario's baby keeps no icon.
  4. `CapturedClient` gains `IconBabyID string \`json:"icon_baby_id"\``.
- [ ] **Step 7: Fold the Task 6 review's minors.**
  - **M3.** The removed member's refresh after its removal is asserted as HTTP 401 with code `session_revoked`.
  - **M5.** `TestCapturedFixtureIsPinned` decompresses `data.sql.gz` and `ledger.tar.gz` and fails on any of:
    - the production IDs (team `TEAM123456`, key `KEY1234567`);
    - the homelab instance `<instance-uuid>`;
    - a `-----BEGIN` block;
    - a line starting with a backslash (`\restrict` and other psql meta-commands).
  - **M6.** The script removes only its own worktree (`git worktree remove --force "$tmp"` in the EXIT trap). It never runs a global `git worktree prune`.
  - **M8.** The script refuses unless Docker is local. It refuses a `DOCKER_HOST` that is set and not `unix://…`, and a current `docker context` whose endpoint is not a local socket. Pinned in `TestCaptureLegacyFixturePins`.
- [ ] **Step 8: The pins move to 29.**
  - `TestCapturedFixtureIsPinned`: `manifest.schema` equals `HomelabChain` (29 rows), and `legacy_sha` is `fce422d51ef4fbd4ac98a4bab4e4375ff6e762a6`.
  - Also in `TestCapturedFixtureIsPinned`, two icon checks on the owner's `PullFromZero` and on `SnapshotEntities`:
    - the latest entity for `icon_baby_id` carries `"icon":"girl.2"`;
    - the golden baby carries no `icon` key.
  - `TestCaptureLegacyFixturePins` additionally pins the M6 and M8 lines.
- [ ] **Step 9: Recapture.** With `df -h /` at least 10 GB free, and no other `test-db.sh` or E2E run active, run `rtk bash Server/scripts/capture-legacy-fixture.sh fce422d51ef4fbd4ac98a4bab4e4375ff6e762a6 homelab29`. Then `rtk git rm -r Server/internal/legacyimport/testdata/fixtures/homelab28`.
- [ ] **Step 10: Run green.**
  - The Step 2 commands, with the fixture name `homelab29`.
  - The six files exist, each under 2 MB.
  - `rtk go -C Server test ./scripts -run TestNoProductionAPNsKeyInTheServerTree -count=1`.
  - The task report records the high water and mid cursor, the registrations, cards and pushes seen, and the two babies with their icons. It does not record the token values.
- [ ] **Step 11: Commit:** `rtk git add -A Server/scripts/capture-legacy-fixture.sh Server/scripts/capture_legacy_fixture_test.go Server/internal/legacyimport/capture_test.go Server/internal/legacyimport/fixtures_manifest_test.go Server/internal/legacyimport/legacytest/captured.go Server/internal/legacyimport/testdata/fixtures && rtk git commit -m "test(legacyimport): recapture the homelab-shaped fixture at fce422d5 (schema 29) with baby icons"`

### Task 7: One APNs client for wakes and Live Activities — `apns.Send`, `apns-preflight`, the Live Activity switch [Wave A]

**Files:**
- Create `Server/internal/apns/send.go`, `send_test.go`, `preflight.go` and `preflight_test.go`.
- Modify `Server/internal/apns/client.go`: `Wake` becomes a call to `Send`, and the token bounds move into `Wake`.
- Create `Server/internal/config/live_activity.go` and `live_activity_test.go`; modify `Server/internal/config/config.go` (`Config.LiveActivity`).
- Modify `Server/cmd/baby-tracker-server/apns_sink.go` and `apns_sink_identitytest.go`: `newWakeAPNSClient` becomes `newAPNSClient(creds config.APNSCredentials)`. Exactly one client is built when wakes or Live Activities are enabled, and the identitytest recorder also records `apns-push-type` and `apns-topic`.
- Create `Server/cmd/baby-tracker-server/apns_preflight.go` and `apns_preflight_test.go`; modify `main.go` (the subcommand).
- Modify `Server/README.md`, in the "Background push (optional)" section: the Live Activity switch and the preflight command.

**Interfaces:**
```go
package apns
const (
	PushBackground   = "background"
	PushLiveActivity = "liveactivity"
	maxPayloadBytes  = 4096
	maxSendToken     = 1024 // bytes; Live Activity tokens are longer than device tokens
)
// Request is one push. Topic is the bundle ID, plus ".push-type.liveactivity"
// for PushLiveActivity: derived, never caller-supplied.
type Request struct {
	Environment Environment
	Token       []byte // 1..maxSendToken bytes, sent as lowercase hex
	PushType    string // PushBackground or PushLiveActivity
	Priority    int    // 5 or 10
	Expiration  int64  // apns-expiration, Unix seconds; 0 means "now or never" (a Live Activity start)
	APNsID      string // canonical lowercase UUID; "" means a random version 4 UUID
	Payload     []byte // 1..maxPayloadBytes, a JSON object
}
// Send validates (ErrNotSent before any byte), signs, sends over the reused
// HTTP/2 connection of its environment, and classifies exactly as Wake did
// (reason list, Retry-After capped at 24 h, 403 Expired/InvalidProviderToken
// invalidates the provider token subject to the 20-minute floor, Ambiguous
// always with an error wrapping ErrOutcomeUnknown and a coarse cause).
func (c *Client) Send(ctx context.Context, r Request) (Result, error)
// Wake is Send with PushBackground, priority 5, now+1h, WakePayload, after
// checking the device-token bound 16..256 bytes (ErrNotSent).
func (c *Client) Wake(ctx context.Context, env Environment, token []byte) (Result, error)

type PreflightOutcome string
const (
	PreflightPass        PreflightOutcome = "pass"         // 400 BadDeviceToken: the key, team and key ID were accepted
	PreflightBadKey      PreflightOutcome = "bad_key"      // 403 InvalidProviderToken / BadEnvironmentKeyIdInToken / UnrelatedKeyIdInToken
	PreflightTopic       PreflightOutcome = "topic"        // 400 DeviceTokenNotForTopic / TopicDisallowed / BadTopic
	PreflightUnreachable PreflightOutcome = "unreachable"  // Ambiguous: no answer
	PreflightUnexpected  PreflightOutcome = "unexpected"   // anything else
)
type PreflightResult struct {
	Outcome PreflightOutcome
	Status  int
	Reason  string
}
// Preflight sends one background push to an all-zero 32-byte token (main's
// dummy-token check, preflight.go:47-116) and classifies Apple's answer. It
// proves the key, team and key ID; it cannot prove the topic.
func (c *Client) Preflight(ctx context.Context, env Environment) (PreflightResult, error)

package config
type LiveActivityConfig struct {
	Enabled     bool
	Credentials APNSCredentials // the same BT_APNS_* as wakes
}
// LoadLiveActivity: BT_LIVE_ACTIVITY_PUSH_ENABLED "" or "false" is off; "true"
// requires LoadAPNSCredentials; anything else is an error. A set
// BT_LIVE_ACTIVITY_DATABASE_URL_FILE or BT_LIVE_ACTIVITY_DATABASE_URL is refused:
// "the Live Activity worker runs on the maintenance credential
// (BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE); remove the setting".
func LoadLiveActivity(getenv func(string) string) (LiveActivityConfig, error)
```
Command: `baby-tracker-server apns-preflight --environment sandbox|production`. It reads `BT_APNS_*` and prints exactly one line, `apns_preflight environment=<env> outcome=<outcome> status=<n> reason=<reason>`. It exits 0 only for `pass`, and never prints a token, a key or a provider token.

- [ ] **Step 1: Write the failing tests** (`send_test.go`, over the local TLS HTTP/2 `fakeAPNs` and `testCredentials` helpers of `client_test.go`):
```go
func TestSendLiveActivityHeadersAndTopic(t *testing.T) {
	server, requests, _ := fakeAPNs(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	creds, _ := testCredentials(t)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	client, err := apns.NewWithTransport(creds, server.Client().Transport, server.URL, server.URL, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	token := bytes.Repeat([]byte{0x5c}, 80) // a Live Activity token is longer than a device token
	payload := []byte(`{"aps":{"event":"start","timestamp":1}}`)
	result, err := client.Send(t.Context(), apns.Request{Environment: apns.Sandbox, Token: token, PushType: apns.PushLiveActivity, Priority: 10,
		Expiration: 0, APNsID: "6f9619ff-8b86-4011-b42d-00c04fc964ff", Payload: payload})
	if err != nil || result.Class != apns.Accepted {
		t.Fatalf("%+v %v", result, err)
	}
	got := (*requests)[0]
	for name, want := range map[string]string{"apns-push-type": "liveactivity", "apns-topic": creds.BundleID + ".push-type.liveactivity",
		"apns-priority": "10", "apns-expiration": "0", "apns-id": "6f9619ff-8b86-4011-b42d-00c04fc964ff"} {
		if v := got.headers.Get(name); v != want {
			t.Fatalf("%s = %q, want %q", name, v, want)
		}
	}
	if got.path != "/3/device/"+hex.EncodeToString(token) || !bytes.Equal(got.body, payload) {
		t.Fatalf("path %s body %s", got.path, got.body)
	}
}

func TestSendRefusesBeforeSending(t *testing.T) {
	server, requests, _ := fakeAPNs(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	creds, _ := testCredentials(t)
	client, _ := apns.NewWithTransport(creds, server.Client().Transport, server.URL, server.URL, time.Now)
	ok := apns.Request{Environment: apns.Production, Token: []byte{1}, PushType: apns.PushLiveActivity, Priority: 5, Expiration: 1, Payload: []byte(`{}`)}
	for name, mutate := range map[string]func(*apns.Request){
		"empty token":     func(r *apns.Request) { r.Token = nil },
		"token too long":  func(r *apns.Request) { r.Token = make([]byte, 1025) },
		"push type":       func(r *apns.Request) { r.PushType = "alert" },
		"priority":        func(r *apns.Request) { r.Priority = 7 },
		"payload too big": func(r *apns.Request) { r.Payload = append([]byte(`{"x":"`), append(bytes.Repeat([]byte{'a'}, 4096), '"', '}')...) },
		"not json":        func(r *apns.Request) { r.Payload = []byte(`[1]`) },
		"apns-id":         func(r *apns.Request) { r.APNsID = "NOT-A-UUID" },
		"environment":     func(r *apns.Request) { r.Environment = "staging" },
	} {
		r := ok
		mutate(&r)
		if _, err := client.Send(t.Context(), r); !errors.Is(err, apns.ErrNotSent) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(*requests) != 0 {
		t.Fatalf("%d requests reached the server", len(*requests))
	}
}

func TestWakeIsSendWithTheBackgroundShape(t *testing.T) {
	// TestWakeSendsAContentlessBackgroundPushOverOneHTTP2Connection and TestWakeClassifiesApplesAnswers (client_test.go)
	// pass unchanged; in addition a 15-byte and a 257-byte token are ErrNotSent from Wake, while Send accepts both.
}
```
  `preflight_test.go`, `TestPreflightClassifiesApplesAnswers`: one table row per outcome (400 `BadDeviceToken` → pass, 403 `InvalidProviderToken` → bad_key, 400 `TopicDisallowed` → topic, connection closed → unreachable, 500 → unexpected). It also checks that the path is `/3/device/` + 64 zeros.

  `config/live_activity_test.go`, `TestLiveActivitySwitch`:
  - unset or `false` → off, with no file read;
  - `true` with a valid key → on, with credentials equal to `LoadAPNSCredentials`';
  - `true` without the key → an error naming `BT_APNS_KEY_FILE`;
  - `maybe` → an error;
  - `BT_LIVE_ACTIVITY_DATABASE_URL_FILE=/x` → the maintenance-credential error.

  `cmd/baby-tracker-server/apns_preflight_test.go`: `runAPNsPreflight` over `apns.NewWithTransport` against a `fakeAPNs` answering `400 BadDeviceToken` prints `apns_preflight environment=sandbox outcome=pass status=400 reason=BadDeviceToken` and returns nil; answering 403 returns an error; the output never contains the key ID or the token.
- [ ] **Step 2: Run to see the failure:** `rtk go -C Server test ./internal/apns ./internal/config -race -count=1 -run 'TestSend|TestWake|TestPreflight|TestLiveActivitySwitch'` and `rtk go -C Server test ./cmd/baby-tracker-server -run TestAPNsPreflight -count=1`. Expected: `undefined: apns.Request`, `undefined: config.LoadLiveActivity`.
- [ ] **Step 3: Implement.**
  - **`Send` is `Wake`'s body, generalised:**
    - validation (environment, token 1..1024 bytes, push type, priority 5/10, a payload that decodes as a JSON object of at most 4096 bytes, an `APNsID` that is a canonical UUID or empty) → `ErrNotSent`;
    - provider token → `ErrNotSent`;
    - host per environment;
    - headers `apns-push-type`, `apns-topic` (bundle, plus `.push-type.liveactivity` for Live Activity), `apns-priority`, `apns-expiration` (`strconv.FormatInt(r.Expiration, 10)`), `apns-id`, `content-type: application/json`;
    - the response classification and the 403 invalidation unchanged.

    `Wake` checks 16..256 bytes, then calls `Send(ctx, Request{env, token, PushBackground, 5, ceilUnix(c.now().Add(wakeExpiration)), "", []byte(WakePayload)})`.
  - **`Preflight`:** `Send` with `PushBackground`, priority 5, expiration now + 1 minute, `WakePayload`, token `make([]byte, 32)`; the outcome is taken from `Result` as in the table above.
  - **`cmd`:**
    - `newAPNSClient(creds)` replaces `newWakeAPNSClient`, and identitytest keeps its refusal without `BT_IDENTITYTEST_APNS_SINK=1`;
    - `main.go` builds the client once when `cfg.APNSWakes.Enabled || cfg.LiveActivity.Enabled` and passes the same `*apns.Client` to the wake pusher and, from Task 14, to Live Activity;
    - the subcommand `apns-preflight` parses `--environment`, loads `config.LoadAPNSCredentials(os.Getenv)` and calls `Preflight`.
  - **README.** `BT_LIVE_ACTIVITY_PUSH_ENABLED` (off by default; shares `BT_APNS_*`); the `apns-preflight` command and its outcomes; the refused worker variables.
- [ ] **Step 4: Run green:** the Step 2 commands; `rtk go -C Server test ./internal/apns -race -count=3`; `(cd Server && rtk go vet ./... && rtk go vet -tags identitytest ./...)`; `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/wake ./cmd/baby-tracker-server' bash Server/scripts/test-db.sh` (the wake push is unchanged).
- [ ] **Step 5: Commit:** `rtk git add Server/internal/apns Server/internal/config Server/cmd/baby-tracker-server Server/README.md && rtk git commit -m "feat(apns): one generic Send for wakes and Live Activities, an APNs preflight, the Live Activity switch"`


### Task 8: Migration 000032 — Live Activity on the authority schema, care origin columns, compatibility 32 [Wave A]

**Files:**
- Create `Server/migrations/000032_live_activity.sql`.
- Modify `Server/internal/migrate/compatibility.json` (`"min": 32, "max": 32`, plus the digest row) and `compatibility_test.go` (bounds 31 → 32; add `TestBinaryRefusesSchemaThirtyOne` beside the 30 case).
- Create `Server/internal/database/live_activity_schema_test.go`.
- Bump the fully-migrated version pins from 31 to 32:
  - `database/{request_path_schema_test.go,care_schema_test.go,import_schema_test.go,identity_schema_test.go,family_schema_test.go,authority_schema_test.go,refresh_recovery_schema_test.go,roles_test.go}` (with `roles_test.go`'s synthetic future version going 32 → 33);
  - `tenant/context_test.go`;
  - `migrate/compatibility_test.go`;
  - `scripts/lib/disposable-sync.sh:311` (`$schema == 32`).

  Find them with `rtk proxy grep -rn -E '\b31\b' Server/internal/database Server/internal/tenant Server/internal/migrate Server/scripts/lib`, and change only the assertions on the schema version. Dates and limits that happen to contain 31 stay.
- Modify the scripts: `scripts/check-principals.sh` gets a `>= 32` block in both modes (the grants below, each checked on its own); `scripts/smoke.sh` and `scripts/smoke_test.go` pin the same strings.

**Interfaces:** Produces the DDL below. Every column the homelab's legacy Live Activity rows hold keeps its name and meaning, except `membership_revision` (dropped), so Task 17 loads them with plain `INSERT … SELECT`. Go code reaches it through the functions it names:
- the runtime calls `app.fence_live_activity_installation`, `app.live_activity_session_binding`, `app.live_activity_registration_state`, `app.live_activity_claim_live` and `app.live_activity_authorized`;
- the maintenance worker calls `app.claim_live_activity_work`, `app.finish_live_activity_work`, `app.live_activity_target`, `app.abandon_live_activity_start`, `app.release_stale_live_activity_lease`, `app.retain_live_activity_work`, `app.dirty_live_activity_families`, `app.sweep_live_activity_work` and `app.sweep_live_activity_registrations`.

**How the file is written.** Start from `git show 6546a98c592e:Server/migrations/000028_live_activity_push.sql` (`main`'s, 554 lines; byte-identical to the homelab's at `998501b`, sha256 `43b37464…`) and apply exactly the changes below. Every part not listed is copied verbatim, including comments.

| `main`'s 000028 part | In 000032 |
|---|---|
| `live_activity_devices` (lines 3-26) | `membership_revision` removed; `environment` becomes `app.apns_environment` (000031); `push_token` nullable; `registered_sequence bigint NOT NULL DEFAULT 0`, `inventory_version bigint NOT NULL DEFAULT 0` and `foreground_inventory jsonb` declared in the `CREATE TABLE` (they were later `ALTER`s at 378-379, 504-505) |
| `live_activity_tokens` (27-46) | `environment` becomes `app.apns_environment`; `token` nullable; `token_fingerprint bytea` and `server_retired boolean NOT NULL DEFAULT false` declared inline (538, 552) |
| `live_activity_token_owners`, `app.live_activity_token_ownership()` and its triggers (47-66) | verbatim (with the environment type) |
| RLS policies `tenant_registration` (68-77) and the runtime grant (78) | verbatim |
| `live_activity_registration_index`, `_family_dirty`, `_delivery` (with `target_state jsonb NOT NULL DEFAULT '{}'` inline, line 377), `_installations`, `_history` | verbatim, plus `ENABLE ROW LEVEL SECURITY` (no `FORCE`, no policy: no login role holds a privilege on them, and only definers read them) |
| `app.retire_stale_live_activity_registrations()` | verbatim |
| `app.fence_live_activity_installation(…)` | the authority `EXISTS` is replaced (below); the rest is verbatim |
| `app.live_activity_session_binding(…)` | rewritten (below) |
| `app.dirty_live_activity_family(uuid,bigint)` | the `JOIN public.babies b` becomes the live baby rows of `care_entities` (below) |
| `app.live_activity_journal_dirty()` and trigger `live_activity_journal_dirty` (204-214) | **not ported**: the worker's change feed (`app.dirty_live_activity_families`, below) replaces it |
| `app.live_activity_registration_dirty()` and its two triggers (215-241) | verbatim |
| `app.sweep_live_activity_work()` | verbatim; granted to maintenance and called once a minute by the worker |
| `app.claim_live_activity_work(…)` | the line `PERFORM app.sweep_live_activity_work();` is removed (the sweep is no longer per claim); the rest is verbatim |
| `finish_`, `retain_`, `claim_live`, `live_activity_target`, `abandon_live_activity_start`, `release_stale_live_activity_lease`, `live_activity_registration_state`, `live_activity_token_fingerprint()` and its two triggers | verbatim |
| `app.live_activity_registration_subjects(uuid,integer)` | **not ported**: `app.sweep_live_activity_registrations` (below) replaces it |
| the `feed_entries` projection indexes (365-373) | **not ported**: `care_entities_latest_feed_idx` (below) |
| the principal `CHECK` change, the `baby_tracker_live_activity` grants and the manifest rows (343-360 and others) | **not ported**: the grants below, to `baby_tracker_runtime` and `baby_tracker_maintenance` |

**New or rewritten SQL (complete):**
```sql
-- 000032: Live Activity push on the PostgreSQL authority (main's 000028,
-- re-expressed on sessions/devices/care_entities), and care entity origins.

-- 1. Care origins: the device and family sequence of an entity's first
-- version, written once by care.Encode and never updated. Live Activity
-- suppresses a push-to-start on the phone that started the timer itself.
ALTER TABLE public.care_entities
  ADD COLUMN created_device_id uuid CHECK (created_device_id <> '00000000-0000-0000-0000-000000000000'),
  ADD COLUMN created_seq bigint CHECK (created_seq BETWEEN 1 AND 9007199254740991),
  ADD CONSTRAINT care_entities_origin_shape CHECK ((created_device_id IS NULL) = (created_seq IS NULL)
    AND (created_seq IS NULL OR created_seq <= changed_seq));
CREATE INDEX care_entities_latest_feed_idx ON public.care_entities
  (family_id, baby_id, (COALESCE(ended_at, started_at)) DESC, (COALESCE(session_id, id)) DESC, id DESC)
  WHERE kind = 2 AND tombstoned_at IS NULL;
--   liveactivity.SnapshotReader: the latest milk feed (latestFeedSQL).

-- (main's tables and functions, as the table above says)

-- Registration binds the requesting session's lineage. The runtime calls it
-- with tenant context; sessions are authority-only, so it reads them under the
-- authority flag and restores the flag.
CREATE FUNCTION app.live_activity_session_binding(p_account uuid, p_device uuid, p_session uuid, p_epoch bigint)
RETURNS TABLE(instance_id uuid, lineage_root_session_id uuid, authority_expires_at timestamptz)
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE previous text := current_setting('app.authority', true);
BEGIN
  PERFORM set_config('app.authority', '1', true);
  RETURN QUERY
    SELECT st.instance_id, s.lineage_root_id, s.deadline_at
    FROM public.sessions s
    JOIN public.devices d ON d.id = s.device_id AND d.account_id = s.account_id AND d.status = 'active'
    JOIN public.server_state st ON st.singleton AND st.recovery_epoch = s.recovery_epoch
    WHERE p_account = app.current_account_id() AND s.account_id = p_account AND s.device_id = p_device AND s.id = p_session
      AND s.recovery_epoch = p_epoch AND s.status = 'active' AND s.scope = 'family' AND s.family_id = app.current_family_id()
      AND s.deadline_at > clock_timestamp() AND s.refresh_expires_at > clock_timestamp();
  PERFORM set_config('app.authority', coalesce(previous, ''), true);
END $$;

-- In app.fence_live_activity_installation, the authority EXISTS becomes:
--   IF p_expires IS NULL OR NOT isfinite(p_expires) OR NOT app.live_activity_lineage_live(p_account, p_device, p_family, p_epoch, p_expires) THEN
--     RAISE EXCEPTION 'installation authority denied' USING ERRCODE='42501'; END IF;
CREATE FUNCTION app.live_activity_lineage_live(p_account uuid, p_device uuid, p_family uuid, p_epoch bigint, p_expires timestamptz)
RETURNS boolean LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE previous text := current_setting('app.authority', true); live boolean;
BEGIN
  PERFORM set_config('app.authority', '1', true);
  SELECT EXISTS (SELECT 1 FROM public.sessions s JOIN public.devices d ON d.id = s.device_id AND d.account_id = s.account_id
    WHERE s.account_id = p_account AND s.device_id = p_device AND s.recovery_epoch = p_epoch AND s.family_id = p_family
      AND s.scope = 'family' AND s.deadline_at = p_expires AND s.status = 'active' AND d.status = 'active') INTO live;
  PERFORM set_config('app.authority', coalesce(previous, ''), true);
  RETURN live;
END $$;
REVOKE ALL ON FUNCTION app.live_activity_lineage_live(uuid,uuid,uuid,bigint,timestamptz) FROM PUBLIC;

-- The one definition of "this registration may still be pushed to": its
-- realm is current, its account, device, membership, slot and family are
-- active, the family is entitled when required, and the device still holds an
-- active, unexpired family session in the registered lineage. It returns the
-- member's role (CanWrite), or NULL.
CREATE FUNCTION app.live_activity_authorized(p_registration uuid, p_version bigint, p_require_entitlement boolean)
RETURNS app.member_role LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE previous text := current_setting('app.authority', true); role app.member_role;
BEGIN
  PERFORM set_config('app.authority', '1', true);
  SELECT m.role INTO role
  FROM public.live_activity_devices r
  JOIN public.server_state st ON st.singleton AND st.instance_id = r.instance_id AND st.recovery_epoch = r.recovery_epoch
  JOIN public.accounts a ON a.id = r.account_id AND a.status = 'active'
  JOIN public.devices d ON d.id = r.device_id AND d.account_id = r.account_id AND d.status = 'active'
  JOIN public.family_slots sl ON sl.account_id = r.account_id AND sl.family_id = r.family_id AND sl.state = 'active'
  JOIN public.memberships m ON m.family_id = r.family_id AND m.account_id = r.account_id AND m.status = 'active'
  JOIN public.families f ON f.id = r.family_id AND f.status = 'active' AND NOT f.chain_failed
  WHERE r.registration_id = p_registration AND r.registration_version = p_version AND NOT r.revoked
    AND r.authority_expires_at > clock_timestamp()
    AND (NOT (p_require_entitlement OR f.entitlement_required) OR EXISTS (SELECT 1 FROM public.family_entitlements e
         WHERE e.family_id = f.id AND e.entitlement_key = 'family_sync' AND e.status = 'active' AND (e.expires_at IS NULL OR e.expires_at > clock_timestamp())))
    AND EXISTS (SELECT 1 FROM public.sessions s WHERE s.account_id = r.account_id AND s.device_id = r.device_id
         AND s.family_id = r.family_id AND s.scope = 'family' AND s.status = 'active' AND s.lineage_root_id = r.lineage_root_session_id
         AND s.deadline_at = r.authority_expires_at AND s.refresh_expires_at > clock_timestamp());
  PERFORM set_config('app.authority', coalesce(previous, ''), true);
  RETURN role;
END $$;

-- dirty_live_activity_family: the job fan-out joins the family's live babies.
--   JOIN public.care_entities b ON b.family_id = r.family_id AND b.kind = 1 AND b.tombstoned_at IS NULL
-- (care_entities' family_context policy admits it: the function sets
-- app.family_id to p_family before this INSERT … SELECT, as main's does).
-- The worker's change feed: families whose sequence moved since the last tick.
CREATE FUNCTION app.dirty_live_activity_families(p_families uuid[], p_sequences bigint[]) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE r record; n integer := 0;
BEGIN
  IF p_families IS NULL OR p_sequences IS NULL OR cardinality(p_families) <> cardinality(p_sequences) OR cardinality(p_families) > 1000 THEN
    RAISE EXCEPTION 'invalid live activity dirty batch' USING ERRCODE = '22023';
  END IF;
  FOR r IN SELECT DISTINCT ON (v.f) v.f, v.s FROM unnest(p_families, p_sequences) v(f, s)
           WHERE EXISTS (SELECT 1 FROM public.live_activity_registration_index i WHERE i.family_id = v.f AND NOT i.revoked)
           ORDER BY v.f, v.s DESC LOOP
    PERFORM app.dirty_live_activity_family(r.f, r.s);
    n := n + 1;
  END LOOP;
  RETURN n;
END $$;

-- Registrations that are no longer authorized are revoked (their delivery
-- rows go with them through live_activity_registration_dirty).
CREATE FUNCTION app.sweep_live_activity_registrations(p_require_entitlement boolean, p_limit integer) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE r record; n integer := 0;
  previous_account text := coalesce(current_setting('app.account_id', true), '');
  previous_family text := coalesce(current_setting('app.family_id', true), '');
BEGIN
  IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 1000 THEN RAISE EXCEPTION 'invalid sweep limit' USING ERRCODE = '22023'; END IF;
  FOR r IN SELECT i.registration_id, i.registration_version, i.account_id, i.family_id FROM public.live_activity_registration_index i
           WHERE NOT i.revoked AND app.live_activity_authorized(i.registration_id, i.registration_version, p_require_entitlement) IS NULL
           ORDER BY i.registration_id LIMIT p_limit LOOP
    PERFORM set_config('app.account_id', r.account_id::text, true);
    PERFORM set_config('app.family_id', r.family_id::text, true);
    UPDATE public.live_activity_devices SET revoked = true WHERE registration_id = r.registration_id AND registration_version = r.registration_version AND NOT revoked;
    n := n + 1;
  END LOOP;
  PERFORM set_config('app.account_id', previous_account, true);
  PERFORM set_config('app.family_id', previous_family, true);
  RETURN n;
END $$;

REVOKE ALL ON FUNCTION app.live_activity_session_binding(uuid,uuid,uuid,bigint), app.live_activity_authorized(uuid,bigint,boolean),
  app.dirty_live_activity_families(uuid[],bigint[]), app.sweep_live_activity_registrations(boolean,integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.fence_live_activity_installation(uuid,bigint,uuid,uuid,uuid,uuid,bigint,timestamptz,boolean),
  app.live_activity_session_binding(uuid,uuid,uuid,bigint), app.live_activity_claim_live(jsonb),
  app.live_activity_registration_state(uuid,jsonb), app.live_activity_authorized(uuid,bigint,boolean) TO baby_tracker_runtime;
GRANT EXECUTE ON FUNCTION app.claim_live_activity_work(timestamptz,integer), app.finish_live_activity_work(jsonb,timestamptz,text),
  app.retain_live_activity_work(), app.live_activity_target(jsonb,jsonb,boolean,timestamptz,jsonb,jsonb),
  app.abandon_live_activity_start(jsonb,text,text,boolean,boolean), app.release_stale_live_activity_lease(jsonb),
  app.dirty_live_activity_families(uuid[],bigint[]), app.sweep_live_activity_work(),
  app.sweep_live_activity_registrations(boolean,integer) TO baby_tracker_maintenance;

-- Manifest 32 = manifest 31 plus the rows below. Compatibility 32.
INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
  SELECT 32,principal,object_kind,object_identity,privileges FROM public.schema_privilege_manifest WHERE schema_version=31;
INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges) VALUES
  (32,'baby_tracker_runtime','table','public.live_activity_devices','SELECT,INSERT,UPDATE'),
  (32,'baby_tracker_runtime','table','public.live_activity_tokens','SELECT,INSERT,UPDATE'),
  (32,'baby_tracker_runtime','function','app.fence_live_activity_installation(uuid,bigint,uuid,uuid,uuid,uuid,bigint,timestamp with time zone,boolean)','EXECUTE'),
  (32,'baby_tracker_runtime','function','app.live_activity_session_binding(uuid,uuid,uuid,bigint)','EXECUTE'),
  (32,'baby_tracker_runtime','function','app.live_activity_claim_live(jsonb)','EXECUTE'),
  (32,'baby_tracker_runtime','function','app.live_activity_registration_state(uuid,jsonb)','EXECUTE'),
  (32,'baby_tracker_runtime','function','app.live_activity_authorized(uuid,bigint,boolean)','EXECUTE'),
  (32,'baby_tracker_maintenance','function','app.claim_live_activity_work(timestamp with time zone,integer)','EXECUTE'),
  (32,'baby_tracker_maintenance','function','app.finish_live_activity_work(jsonb,timestamp with time zone,text)','EXECUTE'),
  (32,'baby_tracker_maintenance','function','app.retain_live_activity_work()','EXECUTE'),
  (32,'baby_tracker_maintenance','function','app.live_activity_target(jsonb,jsonb,boolean,timestamp with time zone,jsonb,jsonb)','EXECUTE'),
  (32,'baby_tracker_maintenance','function','app.abandon_live_activity_start(jsonb,text,text,boolean,boolean)','EXECUTE'),
  (32,'baby_tracker_maintenance','function','app.release_stale_live_activity_lease(jsonb)','EXECUTE'),
  (32,'baby_tracker_maintenance','function','app.dirty_live_activity_families(uuid[],bigint[])','EXECUTE'),
  (32,'baby_tracker_maintenance','function','app.sweep_live_activity_work()','EXECUTE'),
  (32,'baby_tracker_maintenance','function','app.sweep_live_activity_registrations(boolean,integer)','EXECUTE');
UPDATE public.schema_compatibility SET current_version=32, introduced_by='000032_live_activity.sql', recorded_at=now() WHERE singleton;
```
The `'family_sync'` key is the one `authority` checks (`authority/family.go:891`, `guard.go:400`).

- [ ] **Step 1: Write the failing test** `database/live_activity_schema_test.go` (package `database_test`, over `testkit.New`):
```go
func TestSchemaIsVersionThirtyTwo(t *testing.T) {
	f := testpg.New(t)
	var v int64
	if err := f.Runtime.QueryRow(t.Context(), `SELECT current_version FROM schema_compatibility WHERE singleton`).Scan(&v); err != nil || v != 32 {
		t.Fatalf("version=%d err=%v", v, err)
	}
	if err := database.VerifyRuntimeRole(t.Context(), f.Runtime, 32); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyMaintenanceRole(t.Context(), f.Runtime, f.Maintenance, 32); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		pool *pgxpool.Pool
		sql  string
		want bool
	}{
		{f.Runtime, `has_function_privilege(current_user,'app.claim_live_activity_work(timestamp with time zone,integer)','EXECUTE')`, false},
		{f.Maintenance, `has_function_privilege(current_user,'app.claim_live_activity_work(timestamp with time zone,integer)','EXECUTE')`, true},
		{f.Maintenance, `has_table_privilege(current_user,'public.live_activity_tokens','SELECT')`, false},
		{f.Runtime, `has_table_privilege(current_user,'public.live_activity_delivery','SELECT')`, false},
		{f.Runtime, `has_function_privilege(current_user,'app.live_activity_authorized(uuid,bigint,boolean)','EXECUTE')`, true},
	} {
		var got bool
		if err := c.pool.QueryRow(t.Context(), `SELECT `+c.sql).Scan(&got); err != nil || got != c.want {
			t.Fatalf("%s = %v (%v)", c.sql, got, err)
		}
	}
	var trigger bool
	_ = f.Owner.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname='live_activity_journal_dirty')`).Scan(&trigger)
	if trigger {
		t.Fatal("main's journal trigger was ported")
	}
}

// app.live_activity_authorized follows the authority: removal, logout and a
// new lineage each end the authorization at their commit.
func TestLiveActivityAuthorizationFollowsTheAuthority(t *testing.T) {
	h := testkit.New(t)
	reg := registerCaregiverDevice(t, h) // inserts a live_activity_devices row for the caregiver's session (tenant context, as Task 11's repository will)
	authorized := func() (role *string) {
		t.Helper()
		if err := h.Runtime.QueryRow(t.Context(), `SELECT app.live_activity_authorized($1,$2,false)::text`, reg.ID, int64(reg.Version)).Scan(&role); err != nil {
			t.Fatal(err)
		}
		return role
	}
	if role := authorized(); role == nil || *role != "caregiver" {
		t.Fatalf("a live caregiver registration: %v", role)
	}
	var required *string
	_ = h.Runtime.QueryRow(t.Context(), `SELECT app.live_activity_authorized($1,$2,true)::text`, reg.ID, int64(reg.Version)).Scan(&required)
	if required != nil {
		t.Fatal("authorized without the family_sync entitlement a hosted node requires")
	}
	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID); err != nil {
		t.Fatal(err)
	}
	if role := authorized(); role != nil {
		t.Fatalf("a removed member is still authorized as %s", *role)
	}
	var revoked int
	if err := h.Maintenance.QueryRow(t.Context(), `SELECT app.sweep_live_activity_registrations(false, 100)`).Scan(&revoked); err != nil || revoked != 1 {
		t.Fatalf("sweep revoked %d (%v)", revoked, err)
	}
}

func TestCareOriginShape(t *testing.T) {
	// a care_entities row with created_device_id set and created_seq NULL, or created_seq > changed_seq, is 23514;
	// both NULL (a row written before 32) is accepted.
}
```
  `registerCaregiverDevice` is a helper in the test file. It authenticates the caregiver, then in a runtime transaction with tenant context (`tenant.SetLocal`):
  1. reads `app.live_activity_session_binding`;
  2. calls `app.fence_live_activity_installation`;
  3. inserts the `live_activity_devices` row the way `main`'s repository does (`main:Server/internal/liveactivity/repository.go:52-61`, without `membership_revision`), and returns its ID and version.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/database ./internal/migrate' BT_TEST_RUN='TestSchemaIsVersionThirtyTwo|TestLiveActivityAuthorization|TestCareOrigin|TestCompatibility|TestBinaryRefuses' bash Server/scripts/test-db.sh`. Expected: `version=31`.
- [ ] **Step 3: Write the migration** as specified. Record its digest (`shasum -a 256 Server/migrations/000032_live_activity.sql`) into `compatibility.json`, then bump the pins and the scripts.
- [ ] **Step 4: Run green:** `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/database ./internal/migrate ./internal/tenant' bash Server/scripts/test-db.sh`, then `rtk make server-test`, then `rtk make server-smoke`.
- [ ] **Step 5: Commit:** `rtk git add Server/migrations/000032_live_activity.sql Server/internal/migrate Server/internal/database Server/internal/tenant Server/scripts && rtk git commit -m "feat(schema): Live Activity push on the authority schema and care entity origins (32)"`

**Revision 3 amendment (notes only; no new commit for the icon).** Task 8 is `f8304a3e` plus fix round 1, `eb3d011a`. A scoped re-review of the sweep change is in flight.
- **The icon needs no schema change** (Baby-icon ruling). 000032 is not amended for it. The `homelab_29` re-pin does not touch 000032 either: 000032 re-expresses `main`'s 000028, and `main`'s 000029 changes only `babies`, which has no counterpart here.
- **The digest is `688784b4dd60d08e321c23d4118f22ccc309f898bf299fbd7ca754d93f08d97f`** after fix round 1. A later review fix regenerates it again, until Task 26.
- **What fix round 1 changed in the SQL above** (`eb3d011a` governs where this plan's listing differs):
  - `app.live_activity_authorized` reads the registration through `live_activity_registration_index`. It sets `app.account_id`/`app.family_id` from it for its query, then restores the caller's values (deviation 1).
  - It returns NULL when the caller's tenant context is set and differs from the registration's (review M1).
  - `app.sweep_live_activity_registrations` clears the tenant context before its loop and after each revoke, and restores it at the end. Without that, M1 would make the sweep revoke authorized registrations.
  - `live_activity_token_owners` has `ENABLE ROW LEVEL SECURITY` (review M3).
- **Callers.** Every later caller of `app.live_activity_authorized` passes either no tenant context or the registration's own:
  - Task 14's `WithLiveActivitySnapshot` sets the subject's;
  - Task 17's test and Task 18's `verifyStep` call it with none.

  Each of those tasks' reviews checks this against `eb3d011a`'s tests.
- **Single-transaction safety (Task 4 carry).** 000032 has no `CONCURRENTLY` and no use of an enum value added in the same transaction. `TestApplyTxAppliesTheRestInTheCallersTransaction` proves it at integration. It declares no `DEFERRABLE` constraint (Global Constraints).
- **The harness clock** (the Task 8 concern) is Task 8b.

### Task 8a: Adopt `main`'s baby icon on the authority — accept, validate, keep and return `icon` exactly as `main` does [Wave B; no dependency; revision 3]

**Why.** See the Baby-icon ruling.
- **Phones.** The homelab's phones (`fce422d5`'s app) send `icon` on baby creates and updates. Today the branch refuses the key: `protocol.decodePayloadIntoValue` uses `DisallowUnknownFields`, so the whole request gets `invalid_request`, and those phones quarantine their icon-bearing baby mutations permanently.
- **Import.** The care load (Task 16) would silently drop every stored icon, because `decodeEntityPayload` tolerates unknown keys and discards them.
- **Not a wire change.** This task adopts `main`'s existing wire at `fce422d5`, which those phones already speak and the homelab already serves. It changes nothing a `main`-speaking phone does not already expect.

**`main`'s behaviour, adopted exactly** (`git diff 998501b fce422d5 -- Server/internal/protocol Server/internal/records Server/api/openapi.yaml`):
- **The field.** `protocol.BabyPayload` gains `Icon *string \`json:"icon,omitempty"\``, after `SortOrder`. An absent key and `null` both decode to nil. Nil is never encoded, so no `"icon":null` ever reaches the wire.
- **The canonical hash.** `canonicalPayloadValue` puts `icon` into a baby's canonical payload only when it is set (`main` `canonical.go`). Every mutation and content hash computed without an icon is unchanged, and the phones' `ContentHash.swift` mirrors the rule.
- **Validation.** A present icon outside `boy.1`–`boy.4`, `girl.1`–`girl.4` is `protocol.ErrInvalidMutation`. That rejects that one mutation (`status` `rejected`, `code` `invalid_mutation`), not the request (`main` `records/validation.go:175-177`).
- **Keep on update.** An `update` whose payload has no icon, of a baby whose stored payload has one, writes the entity with the stored icon (`main` `records/repository.go:234-243`). So the outcome, the change other phones pull and the snapshot all carry it. A present icon replaces the stored one. No wire form clears an icon, as on `main`. A `delete` tombstones as before.
- **Reads.** Pulls, snapshots, `familyread`, export and the Live Activity snapshot render the stored wire. They carry `icon` when set and omit the key otherwise, as `main`'s `jsonb_strip_nulls` snapshot did.
- **OpenAPI.** `main`'s one line: `icon: {type: string, enum: [boy.1, boy.2, boy.3, boy.4, girl.1, girl.2, girl.3, girl.4]}` under `BabyPayload`.
- **Imports.** The phone-import path (`internal/importer`) publishes babies without an icon, as `main` does ("Imported babies reach the server without an icon", `main`'s baby-icons spec §2). It is unchanged.

**Files:**
- Modify `Server/internal/protocol/entities.go` (the field) and `canonical.go` (`main`'s hunk, verbatim).
- Modify `Server/internal/protocol/canonical_test.go`: `main`'s `TestBabyMutationHashOmitsAbsentIcon` and `TestDecodeMutationAcceptsOptionalBabyIcon`, verbatim.
- Modify `Server/internal/care/validate.go` (`validateBaby`: `main`'s check, verbatim) and `validate_test.go` (`TestBabyIconValidation`: `main`'s table, through `care.ValidateMutation`).
- Modify `Server/internal/care/records.go`: in `(*workingSet).applyRecord`, the keep-on-update rule, applied to `m.Payload` before `entity := protocol.CanonicalEntity{…}` is built, using `existing` (the decoded previous entity). The code is `main`'s `repository.go:234-243`, with `existing.Payload`.
- Modify `Server/internal/testkit/fixtures.go`: `main`'s `SetBabyIcon`, verbatim, beside `RenameBaby`.
- Create `Server/internal/sync/baby_icon_test.go`: `main`'s `TestBabyUpdateWithoutIconKeepsStoredIcon` and `TestBabyIconOutOfSetIsRejected`. The harness calls (`h.SeedBabies`, `h.RenameBaby`, `h.Service.ApplyAndPull`, `h.Service.FullSnapshot`) exist on the branch; adapt only what differs, and say so in a comment.
- Create `Server/internal/httpapi/baby_icon_test.go` and its golden `Server/internal/httpapi/testdata/baby-icon-roundtrip.json`.
- Modify `Server/api/openapi.yaml` (the one line), and `Server/internal/protocol/types_test.go` only if its OpenAPI-to-struct check needs the new property listed.
- **Unchanged:** `Server/api/fixtures/v1/**` stays byte-identical to `main`'s. `baby.json` is the minimal create without `icon`, still valid, and the phones' `WireShapeTests` hold it as that. The sync golden corpus (`internal/sync/testdata/golden/`) is unchanged; no golden step sets an icon.

**Interfaces:** none new beyond the field and `Harness.SetBabyIcon(t, id, icon string) protocol.Outcome`.

- [ ] **Step 1: Write the failing tests.**
  - The four ported `main` tests, as listed.
  - `httpapi/baby_icon_test.go`, `TestPhoneBabyIconRoundTripsByteIdentical`, over `request_roundtrip_test.go`'s `newStack(t, h, …)` and `stack.raw`. Raw bytes are used throughout, so no Go struct normalizes them.
    1. The owner sends one `POST /v1/sync` whose body is the golden's `request`. It is a baby `create` with `"payload":{"name":"Icon baby","birth_date":"2026-01-12","sort_order":1,"icon":"girl.2"}`, byte for byte as a `fce422d5` phone serializes it.
    2. The outcome's raw `payload` object equals the golden's `outcome_payload`, which contains `"icon":"girl.2"`.
    3. A second device (the caregiver) pulls from 0. The baby change's raw `payload` equals the golden's `pulled_payload`.
    4. The caregiver takes a snapshot. The baby's raw `payload` equals the golden's `snapshot_payload`.
    5. The caregiver sends an `update` renaming the baby without the `icon` key. The outcome, the owner's next pull and a fresh snapshot each carry `"icon":"girl.2"` and the new name.
    6. A baby created without `icon` comes back with no `icon` key, which the test checks with `bytes.Contains` on the raw page: never `"icon":null`.
    7. A batch of `[valid feed, baby update with "icon":"boy.5"]` answers 200. The feed is `applied`, and the baby is `rejected`/`invalid_mutation`: one mutation is refused, never the request.

    The golden is written once with `BT_GOLDEN_UPDATE=1` and reviewed by hand against `main`'s wire. Its mutation is `api/fixtures/v1/baby.json` with `name`, `sort_order` and the appended `"icon":"girl.2"` changed, the same insertion `main`'s `TestDecodeMutationAcceptsOptionalBabyIcon` makes.
- [ ] **Step 2: Run to see the failure:**
  - `rtk go -C Server test ./internal/protocol -run 'Icon' -count=1`. Expected: `unknown field Icon`.
  - `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/care ./internal/sync ./internal/httpapi' BT_TEST_RUN='Icon' bash Server/scripts/test-db.sh`. Expected: `h.SetBabyIcon undefined`; the HTTP test gets `400 invalid_request`.
- [ ] **Step 3: Implement** as listed. No change to `care.Encode`, `store.go` or the schema.
- [ ] **Step 4: Run green:**
  - the Step 2 commands;
  - `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/protocol ./internal/care/... ./internal/sync ./internal/importer ./internal/familyread ./internal/export ./internal/httpapi' bash Server/scripts/test-db.sh`. The golden, round-trip and precedence tests pass unchanged.
  - `rtk make server-test`.
  - Mutants, each of which must fail a test:
    - drop `omitempty`;
    - always add `icon` to the canonical payload;
    - skip the keep-on-update copy;
    - accept `boy.5`;
    - reject the request instead of the mutation.
- [ ] **Step 5: Commit:** `rtk git add Server/internal/protocol Server/internal/care Server/internal/testkit Server/internal/sync Server/internal/httpapi Server/api/openapi.yaml && rtk git commit -m "feat(care): adopt main's optional baby icon — accepted, validated, kept on update, returned unchanged"`

### Task 8b: The harness clock follows the database, so harness sessions do not expire on 2026-10-21 [Wave B; after Task 8; revision 3]

**Why.** The Task 8 ruling in the ledger.
- `testkit`'s harness fixes `Now` at `2026-09-21T12:00:00Z` (`testkit/fixtures.go:102`), and its sessions are issued against it.
- The database checks `refresh_expires_at > clock_timestamp()` on the real clock. From 2026-10-21, every refresh-dependent test fails, for example the wake push.

**Files:**
- Modify `Server/internal/testkit/fixtures.go`: `Now` is the database's `clock_timestamp()` read once at harness creation, truncated to the second.
- Modify every test that depends on the literal 2026-09-21 instant rather than on `h.Now`. Find them with `rtk proxy grep -rn '2026-09-21\|2026, 9, 21' Server --include='*_test.go'`. Each either becomes `h.Now`-relative or keeps its literal because it is a client-supplied past time that stays valid. The report lists each with its choice.
- Create `Server/internal/testkit/clock_test.go`.

- [ ] **Step 1: Write the failing test** `TestHarnessSessionsOutliveTheirFixtureDate`. On `testkit.New(t)`, two checks:
  1. `h.Now` is within one minute of the database's `clock_timestamp()`. This fails today, because `h.Now` is 2026-09-21.
  2. `SELECT min(refresh_expires_at) > clock_timestamp() + interval '7 days' FROM sessions WHERE status='active'`, read as the migrator, is true. This fails from 2026-10-14.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/testkit' BT_TEST_RUN='TestHarnessSessions' bash Server/scripts/test-db.sh`.
- [ ] **Step 3: Implement**, then fix the dependent tests found.
- [ ] **Step 4: Run green:** the Step 2 command, then `rtk make server-test`.
- [ ] **Step 5: Commit:** `rtk git add Server/internal && rtk git commit -m "test(testkit): the harness clock follows the database, so fixture sessions never expire mid-phase"`


### Task 9: The care write path records each entity's origin [Wave B; after Task 8]

**Files:**
- Modify `Server/internal/care/entity.go`: `Row.CreatedDeviceID` and `Row.CreatedSeq`; `Encode` sets them.
- Modify `Server/internal/care/store.go`: the upsert inserts both columns and never updates them; `Fetch` and every other row read select them.
- Create `Server/internal/care/origin_test.go`.

**Interfaces:**
```go
package care
// Row gains:
//	CreatedDeviceID string // the device of the entity's first version; "" (NULL) on rows written before 32
//	CreatedSeq      uint64 // the family sequence of its first version; 0 (NULL) on rows written before 32
// Encode: previous == nil sets them from c (DeviceID, Seq); otherwise it copies previous's.
```

- [ ] **Step 1: Write the failing test** `origin_test.go`:
```go
func TestOriginIsTheFirstVersionAndNeverMoves(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	start := caretest.StartTimer(caretest.BabyA, testkit.TimerA, "feed")
	started := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{start})
	startSeq := started.Outcomes[0].Cursor.Sequence
	edit := caretest.TimerCommand(testkit.TimerA, 2, "update", "2026-09-21T12:05:00.000000Z", 1,
		protocol.TimerEditPayload{BabyID: caretest.BabyA, StartedAt: ptr("2026-09-21T11:59:00.000000Z")})
	caretest.ApplyCommitted(t, h, h.Caregiver, []protocol.Mutation{edit})
	row := caretest.Rows(t, h)[testkit.TimerA]
	if row.CreatedDeviceID != h.Owner.DeviceID || row.CreatedSeq != startSeq || row.ChangedSeq <= startSeq || row.DeviceID != h.Caregiver.DeviceID {
		t.Fatalf("origin %s/%d, changed %d by %s", row.CreatedDeviceID, row.CreatedSeq, row.ChangedSeq, row.DeviceID)
	}
	stop := caretest.Stop(caretest.BabyA, testkit.TimerA, testkit.RecordA)
	stop.ObservedEntityVersion = 2
	caretest.ApplyCommitted(t, h, h.Caregiver, []protocol.Mutation{stop})
	if tomb := caretest.Rows(t, h)[testkit.TimerA]; tomb.CreatedDeviceID != h.Owner.DeviceID || tomb.CreatedSeq != startSeq {
		t.Fatalf("a tombstone moved the origin: %s/%d", tomb.CreatedDeviceID, tomb.CreatedSeq)
	}
	if rec := caretest.Rows(t, h)[testkit.RecordA]; rec.CreatedDeviceID != h.Caregiver.DeviceID {
		t.Fatalf("the completion's origin is %s, want the stopping device", rec.CreatedDeviceID)
	}
}
```
  `ptr` is a test helper. The existing round-trip and golden tests (`care/roundtrip_test.go`, `sync/golden_test.go`) pass unchanged: the wire does not carry the origin.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/care' BT_TEST_RUN='TestOriginIs' bash Server/scripts/test-db.sh`. Expected: `row.CreatedDeviceID undefined`.
- [ ] **Step 3: Implement.**
  - **Fields and `Encode`** as in the interface; a `""`/0 value is written as NULL.
  - **The upsert's `INSERT` column list** gains `created_device_id, created_seq`. Its `ON CONFLICT … DO UPDATE SET` list is unchanged, so neither column is ever updated (the runtime holds no UPDATE on them).
  - **Reads.** `Fetch`, `ReadSnapshotPage`, `StreamChanges` and `caretest.Rows` scan them.
- [ ] **Step 4: Run green:** `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/care/... ./internal/sync ./internal/importer ./internal/familyread ./internal/export' bash Server/scripts/test-db.sh`. The round-trip pins (`TestSyncHTTPRoundTrips`, `TestPagedHTTPRoundTrips`) are unchanged: two more columns, no more round trips.
- [ ] **Step 5: Commit:** `rtk git add Server/internal/care && rtk git commit -m "feat(care): record each entity's origin device and sequence"`


### Task 10: A synthetic homelab-shaped legacy install builder (small and at scale) [Wave C; after Tasks 2–5 and Task 3's revision 3 amendment]

**Files:**
- Create `Server/internal/legacyimport/legacytest/{builder.go,templates.go}` and `Server/internal/legacyimport/builder_test.go`.

**Interfaces:** Consumes `ledgertest.Writer` (Task 2), `testpg.ProductionShape`/`Superuser` (Task 4), `authority.TokenDigest`/`ActorKeyCommitment` (Task 5), `caretest` and `testkit`. Produces:
```go
package legacytest

// Spec is the legacy install Build writes: a verified ledger with its sidecars
// and a database at the homelab's legacy schema 29 (legacytest.NewLegacy,
// fce422d5's chain) whose projection, care tables, journal and Live Activity
// rows agree with it, as the homelab's legacy server would have left them
// (self-hosted).
type Spec struct {
	Families           int  // at least 1
	Babies             int  // per family, default 1; the first baby of each family carries an icon (girl.2, set by its second journal version, as the phones' fill-in does), the others none
	Caregivers         int  // per family, besides the owner
	Viewers            int  // per family
	EntitiesPerFamily  int  // care entities besides one baby; kinds rotate feed, sleep, diaper, supplement, measurement
	VersionsPerEntity  int  // journal versions per entity, at least 1; every 10th entity's last version is a delete
	UpdateEvery        int  // every Nth entity gets one more version (0: none)
	Timers             bool // per family: one running feed timer, and one stopped sleep timer with its completion
	RotationsPerDevice int  // session_rotated events per device: consumed parents, then the live child
	RemovedMember      bool // one more caregiver per family who is then removed (sessions revoked)
	DeletionPending    bool // the last family is deletion_pending
	NotCarried         bool // also writes rows the import must skip and count: mutation receipts, a recovery receipt, a command receipt, an unfinished import session, Live Activity history
	LiveActivity       bool // per family: a registration for every member device, and one running feed card (Timers must be set) with its update token, delivery lifecycle and dirty counter, as main's worker leaves them after an accepted start and update
	EventAt            func(i int) time.Time // ledger time of event i; default 2026-09-25T00:00:00Z + i seconds
	Now                time.Time             // the import clock; default 2026-10-09T20:00:00Z
}

type Member struct {
	Subject, AccountID, DeviceID, SessionID, Role string
	AccessToken, RefreshToken                    string // raw; the projection holds their digests
	Cursor                                       uint64 // a mid-family high water this device last pulled to
}
type Family struct {
	ID        string
	Members   []Member // owner first
	HighWater uint64
	Entities  []string // entity IDs in changed order
	BabyIDs   []string // Spec.Babies babies, first first
	IconBabyID string  // BabyIDs[0], whose latest version carries "icon":"girl.2"
}
type Legacy struct {
	DB                 *testpg.Fixture
	LedgerDir, HeadDir string
	InstanceID         string
	Epoch              uint64
	ActorKey           [32]byte
	TokenKeys          identity.DigestKeyring
	Head               legacyledger.Head
	Families           []Family
	Cards              []Card // the LiveActivity cards, one per family
	Now                time.Time
}
// Card is one running card the builder registered.
type Card struct {
	FamilyID, RegistrationID, DeviceID, ActivityID, UpdateToken, GenerationID, BabyID, TimerID string
	TokenVersion, DirtyVersion, CompletedVersion                                      uint64
}
func Build(t *testing.T, spec Spec) *Legacy
// AppendForeignEvent appends one valid account_registered envelope and moves
// head.json and identity.json (the ledger-changed test, Task 15).
func AppendForeignEvent(l *Legacy) error
// templates: one canonical entity per kind, made by the current care write path
// on a scratch harness (so every template passes care.Encode).
func templates(t *testing.T) map[string]protocol.CanonicalEntity
```

- [ ] **Step 1: Write the failing test** `builder_test.go`:
```go
func TestBuildWritesAConsistentLegacyInstall(t *testing.T) {
	l := legacytest.Build(t, legacytest.Spec{Families: 2, Babies: 2, Caregivers: 1, Viewers: 1, EntitiesPerFamily: 30, VersionsPerEntity: 2,
		Timers: true, RotationsPerDevice: 3, RemovedMember: true, DeletionPending: true, NotCarried: true, LiveActivity: true})
	verified, err := legacyledger.WalkVerified(t.Context(), l.LedgerDir, l.HeadDir, nil)
	if err != nil || verified.Head != l.Head {
		t.Fatalf("ledger %v head %+v", err, verified.Head)
	}
	if got := len(verified.State.Families()); got != 2 {
		t.Fatalf("%d families", got)
	}
	super := testpg.Superuser(t, l.DB)
	q := func(sql string) (n int64) {
		t.Helper()
		if err := super.QueryRow(t.Context(), sql).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	if q(`SELECT max(version) FROM schema_migrations`) != 29 || q(`SELECT count(*) FROM schema_migrations`) != int64(len(legacyimport.HomelabChain)) {
		t.Fatal("not at the homelab's schema 29")
	}
	if q(`SELECT count(*) FROM babies WHERE icon IS NOT NULL`) != 2 || q(`SELECT count(*) FROM babies WHERE icon IS NULL`) != 2 {
		t.Fatal("each family's first baby carries an icon and its second none")
	}
	if q(`SELECT count(*) FROM babies b WHERE b.icon IS DISTINCT FROM (SELECT j.canonical_entity #>> '{payload,icon}' FROM mutation_journal j
		WHERE j.family_id=b.family_id AND j.entity_id=b.id ORDER BY j.sequence DESC LIMIT 1)`) != 0 {
		t.Fatal("a typed baby's icon is not its latest journal version's")
	}
	if q(`SELECT revision FROM security_projection_head WHERE singleton`) != int64(l.Head.Revision) {
		t.Fatal("projection head is not the ledger head")
	}
	if q(`SELECT count(*) FROM families f WHERE f.next_sequence <> (SELECT coalesce(max(sequence),0) FROM mutation_journal j WHERE j.family_id=f.id)`) != 0 {
		t.Fatal("next_sequence is not the journal high water")
	}
	if q(`SELECT count(*) FROM feed_entries e WHERE e.last_sequence <> (SELECT max(sequence) FROM mutation_journal j WHERE j.family_id=e.family_id AND j.entity_id=e.id)`) != 0 {
		t.Fatal("a feed row is not at its latest journal version")
	}
	if q(`SELECT count(*) FROM sessions WHERE status='revoked'`) == 0 || q(`SELECT count(*) FROM sessions WHERE status='consumed'`) == 0 ||
		q(`SELECT count(*) FROM families WHERE status='deletion_pending'`) != 1 || q(`SELECT count(*) FROM mutation_receipts`) == 0 {
		t.Fatal("the edge rows are missing")
	}
	if q(`SELECT count(*) FROM live_activity_tokens WHERE state='active'`) != 2 || q(`SELECT count(*) FROM live_activity_token_owners`) == 0 ||
		q(`SELECT count(*) FROM live_activity_registration_index`) != q(`SELECT count(*) FROM live_activity_devices`) {
		t.Fatal("the Live Activity rows are incomplete")
	}
	owner := l.Families[0].Members[0]
	if digest := authority.TokenDigest(l.TokenKeys.Current, "access", owner.AccessToken); q(`SELECT count(*) FROM sessions WHERE status='active' AND access_digest='\x`+hex.EncodeToString(digest[:])+`'`) != 1 {
		t.Fatal("the owner's raw access token does not match its projected digest")
	}
}
```
- [ ] **Step 2: Run to see the failure:** `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/legacyimport/...' BT_TEST_RUN='TestBuild' bash Server/scripts/test-db.sh`. Expected: `undefined: legacytest.Build`.
- [ ] **Step 3: Implement `Build`.** It writes the ledger with `ledgertest.Writer`, then the database through `testpg.Superuser(t, f)` in one transaction with `SET LOCAL session_replication_role = replica` (no legacy trigger fires, as when restoring a dump). Large tables are written with `CopyFrom`, in 50,000-row chunks streamed from generators.
  1. **Database.**
     - `f := legacytest.NewLegacy(t)`, then `testpg.ProductionShape(t, f)`.
     - Instance `70000000-0000-4000-8000-000000000001`, epoch 1, `ActorKey = [32]byte{0x5a, …}`.
     - `TokenKeys` is `keyring("session.v1", 0x22)`, as `testkit` uses.
  2. **Ledger events, per family, in order.** Effect shapes are copied from `legacyledger`'s tests (`event_test.go`, `reserved_events_test.go`), which validate them.
     - `identity_session_issued` per member;
     - `family_created`;
     - `invitation_created` and `invitation_redeemed` per non-owner;
     - `RotationsPerDevice` × `session_rotated` per device;
     - `member_removed` (RemovedMember);
     - `family_deletion_scheduled` (DeletionPending, last family).

     `Finish` writes the head, `identity.json` and `actor-id-key.json` (commitment `authority.ActorKeyCommitment(ActorKey, instance)`).
  3. **Projection rows from the same model.** For each column, follow the legacy projector, `app.project_security_event_v8`, read from `Server/migrations/000016_invitation_roles.sql` and `000025_refresh_recovery.sql`, and keep the mapping as a comment beside the code.
     - `accounts`, `account_security_state`, `account_identities`;
     - `devices` (`server_instance_id` = instance);
     - `families`, `family_security_state`, `memberships`, `family_slots`, `invitations`;
     - `sessions`: every session the events created, with digests = `authority.TokenDigest(TokenKeys.Current, "access"|"refresh", raw)` and `security_revision` = the issuing revision; consumed parents with `consumed_at`; removed members' sessions revoked;
     - `family_actor_labels`;
     - `security_projection_head` and the `server_state` head columns = the ledger head;
     - the legacy `security_audit_events`, one row per envelope.
  4. **Care, from `templates(t)`.**
     - `templates(t)` runs `caretest.Seed` on a scratch `testkit.New(t)` with `caretest.Baby`, `caretest.Record("feed"|"sleep"|"diaper"|"supplement"|"measurement", …)`, `caretest.GrowthProfile`, `caretest.StartTimer` and `caretest.Stop`, then reads each row's `Entity()` through `caretest.Rows`.
     - Each entity clones its template and sets `meta` (id, family_id, entity_version, creator, last editor, `client_mutation_time`, `accepted_at`, `tombstoned_at`) and the payload's baby, times and IDs.
     - Each version is one `mutation_journal` row: `canonical_entity` is `json.Marshal(entity)`; `sequence` is the family's next value; `operation` is `create`/`update`/`delete`/`timer_*`; `device_id` is the writing member's device.
     - The typed row (`babies`, `feed_entries` + `feed_components`, `sleep_entries`, `diaper_entries`, `active_timers`, `supplement_entries`, `measurement_entries`, `growth_profiles`) holds the latest version, with `last_sequence` = its sequence.
     - **Babies (`Spec.Babies`, default 1).** Care entities of other kinds rotate over the family's babies.
       - The first baby of each family gets a second, `update` journal version whose payload carries `"icon":"girl.2"`, and `babies.icon = 'girl.2'`. Every later version of that baby carries the icon too, as `main`'s `Apply` copies a stored icon into an update that omits it (`main` `records/repository.go:234-243`).
       - The other babies have no `icon` key in any version, and `babies.icon` is NULL.
       - The `canonical_entity` is built as a JSON map (not through the branch's `protocol.BabyPayload`), so the builder does not depend on Task 8a.
     - `families.next_sequence` is the last sequence.
     - `family_record_usage` holds the legacy counts.
  5. **NotCarried rows:**
     - two `mutation_receipts` per device;
     - one `refresh_recovery_receipts` row for the first caregiver's last rotation;
     - one `security_command_receipts` row per owner;
     - one `import_sessions` row in status `staging` for the first family.

     None reaches the new schema. The importer counts them as skipped.
  6. **Live Activity rows** (`LiveActivity`), written exactly as `main`'s repository and triggers would have written them (triggers are off under `replica`, so every derived row is written explicitly):
     - per member device, one `live_activity_installations` row and one `live_activity_devices` row (sandbox, topic `com.example.babytracker.push-type.liveactivity`, a 64-hex push-to-start token, `session_id`/`lineage_root_session_id`/`authority_expires_at` = the device's active session, its root and `absolute_expires_at`, `membership_revision` = the member's `Generation`, `registered_sequence` 0);
     - one `live_activity_registration_index` row per registration;
     - one `live_activity_token_owners` row per push-to-start token (slot `device`);
     - for the family's running feed timer, on the first caregiver's registration: one `live_activity_tokens` row (an update token, `token_version` 1, a generation UUID, `timer_id`, `state` `active`, `token_fingerprint` = sha256 of the token), its `live_activity_token_owners` row (slot `activity:<id>`), one `live_activity_delivery` row (kind `feed`) with `target_state` `{"generation_id":…,"status":"active","timer_id":…,"kind":"feed","activity_id":…,"token_version":1,"last_sequence":<the timer's sequence>}`, `dirty_version` = `completed_version` = 3 and no lease;
     - one `live_activity_family_dirty` row (`dirty_version` 3, `journal_sequence` = the family's high water);
     - with `NotCarried`, two `live_activity_history` rows.
  7. **Member cursors.** Each `Member.Cursor` is the sequence at the middle of its family's journal.
- [ ] **Step 4: Run green:** the Step 2 command, then a subtest with `Spec{Families: 200, EntitiesPerFamily: 50, RotationsPerDevice: 2}`, which finishes in under 60 s; record its time in the report.
- [ ] **Step 5: Commit:** `rtk git add Server/internal/legacyimport && rtk git commit -m "test(legacyimport): build consistent homelab-shaped legacy installs from the ledger up"`


### Task 11: Live Activity registration — wire contract and routes on the authority request seam [Wave B; after Task 8]

**Files:**
- **Copied byte-identical from `main` at `6546a98c592e`** (`git show 6546a98c592e:<path> > <path>`):
  - `Server/internal/protocol/live_activity.go` and `live_activity_test.go`;
  - `Server/api/fixtures/live-activity-v1/{projection.json,content-states.json,registration-receipt.json,token-registration.json}`, plus any other file in that directory.
- Modify `Server/internal/protocol/{auth.go,types.go,sync.go}`: `CapabilityLiveActivityPushV1` is a known capability that `ServerInfo.Validate` accepts. `SurfaceLiveActivity` requires exactly it. These are the same hunks as `main`'s; copy them.
- Modify `Server/api/openapi.yaml`: `main`'s three Live Activity paths, its schemas and the four Live Activity error code sets. Modify `Server/internal/protocol/types_test.go`: the OpenAPI test requires them, as on `main`.
- Modify `Server/internal/limits/policy.go`: `ScopeLiveActivityAccount` (120/min) and `ScopeLiveActivityDevice` (30/min), both in the verified-key class.
- Create `Server/internal/liveactivity/{doc.go,types.go,repository.go,service.go,repository_test.go}`:
  - `types.go` is `main`'s, verbatim;
  - `repository.go` and `service.go` are ported as below;
  - `repository_test.go` is `main`'s, adapted to the branch harness.
- Create `Server/internal/httpapi/{live_activity.go,live_activity_test.go,live_activity_roundtrip_test.go}`; modify `router.go`. Options gain `LiveActivityService`, `LiveActivityUnregistration` and `LiveActivityReady`.
- Modify `docs/protocol/v1.md`: copy `main`'s Live Activity section, plus one sentence saying the routes are served when `BT_LIVE_ACTIVITY_PUSH_ENABLED=true` and the worker is ready.

**Interfaces:** Consumes `authority.Request`/`RequestGuard` (Phase 3) and Task 8's SQL. Produces:
```go
package liveactivity
var ErrVersionConflict = errors.New("live_activity_version_conflict")
var ErrRegistrationDenied = errors.New("live_activity_registration_denied")
type Repository struct{ topic string } // "" for the unregistration-only repository
func NewRepository(topic string) *Repository
func (r *Repository) RegisterDevice(ctx context.Context, tx pgx.Tx, p access.Principal, g access.Grant, req protocol.LiveActivityDeviceRegistration) (protocol.LiveActivityRegistrationReceipt, error)
func (r *Repository) RegisterActivity(ctx context.Context, tx pgx.Tx, p access.Principal, g access.Grant, req protocol.LiveActivityTokenRegistration) (protocol.LiveActivityRegistrationReceipt, error)
func (r *Repository) UnregisterDevice(ctx context.Context, tx pgx.Tx, p access.Principal, installationID string, version uint64) error
type Service struct{ /* guard authority.RequestGuard; repository *Repository */ }
// NewService: topic is the bundle ID + ".push-type.liveactivity" (main's composition, live_activity.go:323).
func NewService(guard authority.RequestGuard, topic string) *Service
func (s *Service) RegisterDevice(ctx context.Context, req authority.Request, body protocol.LiveActivityDeviceRegistration) (protocol.LiveActivityRegistrationReceipt, error)
func (s *Service) RegisterActivity(ctx context.Context, req authority.Request, body protocol.LiveActivityTokenRegistration) (protocol.LiveActivityRegistrationReceipt, error)
// Unregistration works whether push is enabled or not (main's rule).
type Unregistration struct{ /* service with NewRepository("") */ }
func NewUnregistration(guard authority.RequestGuard) *Unregistration
func (u *Unregistration) UnregisterDevice(ctx context.Context, req authority.Request, installationID string, version uint64) error

package httpapi
type LiveActivityService interface {
	RegisterDevice(context.Context, authority.Request, protocol.LiveActivityDeviceRegistration) (protocol.LiveActivityRegistrationReceipt, error)
	RegisterActivity(context.Context, authority.Request, protocol.LiveActivityTokenRegistration) (protocol.LiveActivityRegistrationReceipt, error)
}
type LiveActivityUnregistration interface {
	UnregisterDevice(context.Context, authority.Request, string, uint64) error
}
// Options: LiveActivityService (nil: PUT routes 404), LiveActivityUnregistration (nil: DELETE 404),
// LiveActivityReady func(context.Context) error (the worker's readiness; nil means not ready).
```

**The port, file by file:**
- **`repository.go`** is `main:Server/internal/liveactivity/repository.go` with these changes:
  - `binding` drops the `g.MembershipRevision` check, and reads `app.live_activity_session_binding` with the principal's `p.SessionID` (the branch's `access.Principal` session ID field).
  - Every `membership_revision` column and argument is removed from the device upsert, the identical-retry `SELECT` and `RegisterActivity`'s registration lookup. The remaining arguments renumber.
  - `SELECT EXISTS(SELECT FROM babies WHERE family_id=$1 AND id=$2)` becomes `SELECT EXISTS(SELECT 1 FROM care_entities WHERE family_id=$1 AND id=$2 AND kind=1 AND tombstoned_at IS NULL)`.
  - `p.AccountID != g.AccountID` stays (the branch's `access.Grant` has `AccountID`).
- **`service.go`** is `main`'s with `transact` replaced. The handler's `Admit` has already refused a non-family principal (below), so the service only checks the session family against the grant:
```go
func (s *Service) transact(ctx context.Context, req authority.Request, run func(pgx.Tx, access.Principal, access.Grant) error) error {
	if s == nil || s.guard == nil {
		return ErrRegistrationDenied
	}
	// Registration needs reading authority only, including viewers. It conveys no mutation authority.
	return s.guard.WithRequestTx(ctx, req, "read", func(tx pgx.Tx, p access.Principal, g access.Grant) error {
		if p.SessionFamilyID != g.FamilyID {
			return ErrRegistrationDenied
		}
		return run(tx, p, g)
	})
}
```
- **`httpapi/live_activity.go`** is `main`'s handler, rebuilt on the Phase 3 shape (`httpapi/sync.go`'s `syncHandler`):
  1. readiness;
  2. the bearer token and `AccessDigests`;
  3. `Cache-Control: no-store`;
  4. header validation against `SurfaceLiveActivity`;
  5. a strict body of at most 16 KiB with no query string on PUT. Each refusal is held as `syncRefusal`.

  Then `Service.Register*` runs with `authority.Request{Digests, Admit, Charge}`:
  - `Admit` refuses a non-family scope or session scope with `access_expired` (as `main`), then any held refusal.
  - `Charge` takes the two Live Activity limits.

  Errors map exactly as `main`'s: `ErrVersionConflict` → 409 `live_activity_version_conflict`, `ErrRegistrationDenied` → 403 `live_activity_registration_denied`, everything else through `writeRequestError`. The PUT routes need `LiveActivityService` and `LiveActivityReady == nil`, otherwise 503 `service_unavailable` (`main`'s rule). DELETE needs only `LiveActivityUnregistration`.
- **`router.go`:**
  - PUT routes are registered when `LiveActivityService != nil`; the DELETE route always is.
  - `/v1/server` advertises `live-activity-push-v1` only when `LiveActivityService != nil` and `LiveActivityReady(ctx) == nil`, as `main`'s `operational.go:42-55` does.

- [ ] **Step 1: Write the failing tests.**
  - **`protocol/live_activity_test.go`** is `main`'s, verbatim. It fails until `live_activity.go` exists.
  - **`liveactivity/repository_test.go`** ports `main`'s 13 tests. `testkit.New` replaces `main`'s harness, and `h.Authority` replaces `h.Security`. Each test calls the repository inside `h.Authority.WithRequestTx(ctx, authority.Request{Digests: …}, "read", …)`. Membership-revision assertions become lineage assertions: after `h.Refresh("caregiver")` the same registration stays valid (same lineage); after removal and rejoin it is denied (new lineage). The registration-receipt fixture (`api/fixtures/live-activity-v1/registration-receipt.json`) is decoded and compared, so the wire is pinned.
  - **`httpapi/live_activity_test.go`** ports `main`'s handler tests (status, codes, `Cache-Control`, 16 KiB, query refused, DELETE works without a service). Add these:
```go
func TestLiveActivityRoutesFollowTheSwitch(t *testing.T) {
	h := testkit.New(t)
	off := newStackWith(t, h, func(o *Options) { o.LiveActivityService = nil; o.LiveActivityUnregistration = liveactivity.NewUnregistration(h.Authority) })
	if rec := off.send(t, "owner", http.MethodPut, "/v1/live-activities/device", deviceRegistration(t, h)); rec.Code != http.StatusNotFound {
		t.Fatalf("PUT with push off: %d", rec.Code)
	}
	if rec := off.send(t, "owner", http.MethodDelete, "/v1/live-activities/device/"+testInstallation+"?version=1", nil); rec.Code == http.StatusNotFound {
		t.Fatal("DELETE must work with push off")
	}
	if strings.Contains(off.server(t), "live-activity-push-v1") {
		t.Fatal("advertised with push off")
	}
	on := newStackWith(t, h, func(o *Options) {
		o.LiveActivityService = liveactivity.NewService(h.Authority, "com.example.babytracker.push-type.liveactivity")
		o.LiveActivityReady = func(context.Context) error { return nil }
	})
	if rec := on.send(t, "viewer-or-owner", http.MethodPut, "/v1/live-activities/device", deviceRegistration(t, h)); rec.Code != http.StatusOK {
		t.Fatalf("PUT with push on: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(on.server(t), "live-activity-push-v1") {
		t.Fatal("not advertised with push on and the worker ready")
	}
}
```
    `newStackWith`, `send` and `server` extend Phase 3's `request_roundtrip_test.go` stack. `deviceRegistration` builds a valid body from `api/fixtures/live-activity-v1/token-registration.json`'s shape. `testInstallation` is a fixed UUID.
  - **`httpapi/live_activity_roundtrip_test.go`, `TestLiveActivityRegistrationRoundTrips`:** after a warm request, a device registration takes at most 5 round trips (BEGIN plus authority, binding plus fence pipelined, the upsert, the registration state, COMMIT); an identical retry and a refused request take at most 2. Record the measured numbers.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/protocol ./internal/liveactivity ./internal/httpapi' BT_TEST_RUN='LiveActivity|TestOpenAPI' bash Server/scripts/test-db.sh`. Expected: `undefined: protocol.CapabilityLiveActivityPushV1`.
- [ ] **Step 3: Implement** as above. Pipeline the binding read and the fence call in one `pgx.Batch`.
- [ ] **Step 4: Run green:** the Step 2 command, then `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/httpapi ./internal/protocol ./internal/limits ./cmd/...' bash Server/scripts/test-db.sh`. `TestSyncErrorPrecedenceIsUnchanged` still passes: no sync byte changed.
- [ ] **Step 5: Commit:** `rtk git add Server/internal/protocol Server/api Server/internal/limits Server/internal/liveactivity Server/internal/httpapi docs/protocol/v1.md && rtk git commit -m "feat(liveactivity): main's registration wire on the authority request seam"`


### Task 12: Live Activity card projection on `care_entities` [Wave C; after Task 9]

**Files:**
- Create `Server/internal/liveactivity/{presentation.go,lifecycle.go,presentation_test.go,lifecycle_test.go}`, verbatim from `main` at `6546a98c592e`. They depend only on `access`, `protocol` and the standard library; Task 11 brought in `types.go`.
- Create `Server/internal/liveactivity/snapshot.go`, rewritten on `care_entities` as below, and `snapshot_test.go`, `main`'s ported to the branch harness.

**Interfaces:** Produces the same API as `main`:
```go
package liveactivity
type CareSnapshot struct {
	Baby                 protocol.CanonicalEntity
	Timers               []protocol.CanonicalEntity
	LastMilk             *protocol.CanonicalEntity
	SessionMembers       []protocol.CanonicalEntity
	TotalSessionParts    uint64
	LastBreast           *protocol.CanonicalEntity
	EarlierAsleepSeconds int64
	Cursor               protocol.Cursor
	Grant                access.Grant
}
type SnapshotReader struct{}
// Read requires the read-only repeatable-read transaction with the family's
// tenant context that authority.WithLiveActivitySnapshot supplies (Task 14).
func (SnapshotReader) Read(ctx context.Context, tx pgx.Tx, grant access.Grant, babyID string) (CareSnapshot, error)
// the timer's origin, for SuppressStart (Task 14):
type TimerOrigin struct {
	DeviceID string
	Sequence uint64 // 0 when unknown (an entity written before 000032)
}
func ReadTimerOrigin(ctx context.Context, tx pgx.Tx, familyID, timerID string) (TimerOrigin, error)
```

**Each of `main`'s reads, mapped** (`main:Server/internal/liveactivity/snapshot.go`; `entity(row)` is `json.Unmarshal(wire)` into `protocol.CanonicalEntity`):

| `main` read | Here |
|---|---|
| cursor: `server_state` × `families.next_sequence` | the same statement |
| the baby: `records.GetCanonical("baby")` | `SELECT wire, tombstoned_at IS NOT NULL FROM care_entities WHERE family_id=$1 AND id=$2 AND kind=1`; missing or tombstoned returns the empty snapshot |
| running timers: `active_timers` + `records.GetTimerSnapshot(…).Canonical()` | `SELECT wire FROM care_entities WHERE family_id=$1 AND baby_id=$2 AND kind=5 AND tombstoned_at IS NULL ORDER BY timer_kind` (`care_entities_one_live_timer`); each entity goes through `care.NormalizeTimerEntity`, the Phase 2 renderer of running timers that matches `main`'s normalized form |
| `earlierAsleepSQL` | the same aggregate on `care_entities WHERE family_id=$1 AND baby_id=$2 AND kind=3 AND tombstoned_at IS NULL AND ended_at IS NOT NULL AND (session_id=$3 OR (session_id IS NULL AND id=$3))` (`care_entities_session_idx`); `session_id` is `sleep_session_id` (`care/entity.go:238`) |
| `latestMilkSQL` (`kind<>'solids'`) | `latestFeedSQL`: `SELECT wire FROM care_entities WHERE family_id=$1 AND baby_id=$2 AND kind=2 AND tombstoned_at IS NULL ORDER BY COALESCE(ended_at,started_at) DESC, COALESCE(session_id,id) DESC, id DESC LIMIT 32` (`care_entities_latest_feed_idx`); the first row whose `FeedPayload.Kind != "solids"` wins; if all 32 are solids, the next page continues with a keyset on the three sort keys |
| `sessionMembersSQL` and `sessionCountSQL` | `SELECT wire FROM care_entities WHERE family_id=$1 AND baby_id=$2 AND kind=2 AND tombstoned_at IS NULL AND (session_id=$3 OR (session_id IS NULL AND id=$3)) ORDER BY started_at DESC, id DESC` (`care_entities_session_idx` and the PK). In Go, the members are the non-solids feeds with at least one `breast` or `bottle` component; `TotalSessionParts` counts those components over every member; `SessionMembers` keeps the first 8, as `main`'s `LIMIT 8` does |
| `latestBreastSQL` (`kind='breast'`) | `SELECT wire FROM care_entities WHERE family_id=$1 AND baby_id=$2 AND kind=2 AND tombstoned_at IS NULL ORDER BY started_at DESC, id DESC LIMIT 32` (`care_entities_history_idx`, backward); the first `FeedPayload.Kind == "breast"`, with keyset paging as above |
| the timer-to-record link (`worker.go:367-370`: `active_timers` ⋈ `feed_entries`) | `SELECT t.completed_record_id::text, COALESCE(f.session_id, f.id)::text FROM care_entities t LEFT JOIN care_entities f ON f.family_id=t.family_id AND f.id=t.completed_record_id AND f.kind=2 AND f.tombstoned_at IS NULL WHERE t.family_id=$1 AND t.id=$2 AND t.kind=5 AND t.tombstoned_at IS NOT NULL`, exported as `ReadCompletedSession(ctx, tx, familyID, timerID) (completed, session *string, err error)` |
| the timer's origin (`worker.go:378-384`: the first `timer_start` journal row) | `ReadTimerOrigin`: `SELECT coalesce(created_device_id::text,''), coalesce(created_seq,0) FROM care_entities WHERE family_id=$1 AND id=$2 AND kind=5` (Tasks 8–9) |

- [ ] **Step 1: Write the failing tests.**
  - **`presentation_test.go` and `lifecycle_test.go`** are `main`'s, verbatim. They read `Server/api/fixtures/live-activity-v1/projection.json` and `content-states.json`, the shared fixture that also pins the phone's builder.
  - **`snapshot_test.go`** ports `main`'s two tests to `testkit.New`. Seed through `caretest` (baby, a breast, bottle and solids feed session, a running sleep continuing a two-part sleep). `Read` inside a read-only repeatable-read transaction with `caretest.TenantTx`. Then:
```go
func TestSnapshotMatchesTheSharedProjectionFixture(t *testing.T) {
	// For every case in projection.json: seed its canonical entities into care_entities through
	// care.Encode and caretest.Write, Read, Project with the case's preferences and clock,
	// and compare the DesiredCards with the case's expected cards exactly (main's snapshot_test.go:…
	// compared the same fixture against legacy tables).
}

func TestSnapshotSkipsSolidsAndPagesPastThem(t *testing.T) {
	// 40 solids-only feeds newer than one bottle feed: LastMilk is the bottle feed; LastBreast is nil.
}

func TestTimerOriginIsTheStartingDevice(t *testing.T) {
	// the owner starts a timer, the caregiver edits it: ReadTimerOrigin is the owner's device and the start's sequence.
}
```
- [ ] **Step 2: Run to see the failure:** `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/liveactivity' BT_TEST_RUN='TestProject|TestReconcile|TestSnapshot|TestTimerOrigin|Test.*Presentation|Test.*Lifecycle' bash Server/scripts/test-db.sh`. Expected: `undefined: Project`.
- [ ] **Step 3: Implement** `snapshot.go` as mapped. `presentation.go` and `lifecycle.go` are copied unchanged; `effectiveSession` and `feedEnd` are reused from `presentation.go`.
- [ ] **Step 4: Run green:** the Step 2 command; `rtk go -C Server vet ./internal/liveactivity`.
- [ ] **Step 5: Commit:** `rtk git add Server/internal/liveactivity && rtk git commit -m "feat(liveactivity): main's card projection and lifecycle over care_entities"`

### Task 13: Verify the ledger, lock the install, cross-check the projection, and the step pipeline [Wave D; after Tasks 3, 4 and 10]

**Files:**
- Create `Server/internal/legacyimport/{crosscheck.go,run.go,report.go,crosscheck_test.go}`. `run.go` holds the step pipeline only; Task 18 adds `Run`.
- Modify `Server/internal/legacyimport/export_test.go`: `Pipeline`.

**Interfaces:** Consumes Tasks 1–3 and 6. Produces:
```go
package legacyimport

// Options configure one import. Pool is the migrator's (the schema owner).
// A run acquires exactly one connection from it and holds it from preflight to
// the post-commit VACUUM; nothing else in the run touches Pool.
type Options struct {
	Pool          *pgxpool.Pool
	LedgerDir     string
	HeadDir       string
	InstanceID    string    // BT_SERVER_INSTANCE_ID
	RecoveryEpoch uint64    // BT_RECOVERY_EPOCH
	ActorKey      [32]byte  // BT_ACTOR_ID_PEPPER_FILE
	Limits        protocol.Limits // the family charge ceiling the recomputed counters must fit (Task 16)
	DryRun        bool
	VerifyOnly    bool
	BackupReceipt *BackupReceipt // required unless DryRun or VerifyOnly (Task 18)
	Now           func() time.Time
	Logger        *slog.Logger
	fault         func(step string) error // tests only (export_test.go)
}

// ledgerFacts is what the first, verifying walk collects for the load.
type ledgerFacts struct {
	hashes        [][32]byte              // event hash by revision-1
	createdBy     map[string]string       // family ID -> "family_created" | "family_bootstrapped"
	purgeStarted  map[string]time.Time    // family ID -> family_purge_started event time
	latest        time.Time               // the newest event time
}

type importRun struct {
	o        Options
	conn     *pgxpool.Conn // the run's one dedicated connection (checkAlone counts every other session)
	now      time.Time
	start    Start
	verified legacyledger.Verified
	facts    ledgerFacts
	tx       pgx.Tx
	forced   []string // relations whose FORCE ROW LEVEL SECURITY was lifted, to restore
	triggers []string // tables whose user triggers were disabled, to re-enable
	report   *Report
}
type step struct {
	name string
	run  func(context.Context, *importRun) error
}
// importSteps run in order inside the import transaction. Tasks 15, 16 and 17 insert
// "authority", "care" and "liveactivity" (Tasks 15–17) before "restore"; Task 18 appends "verify", "drop", "analyze".
var importSteps = []step{
	{"lock", lockStep},
	{"crosscheck", crossCheckStep},
	{"stage", stageStep},
	{"unforce", unforceStep},
	{"restore", restoreStep}, // FORCE ROW LEVEL SECURITY and ENABLE TRIGGER USER back, exactly over r.forced and r.triggers
}
// preflight runs before the transaction: Detect and checkAlone in a read-only
// transaction, then the verifying walk (pass 1), then the identity checks.
func preflight(ctx context.Context, o Options) (*importRun, error)

// Mismatch is one disagreement between the verified ledger and the projection.
type Mismatch struct{ Table, Key, Field, Ledger, Projection string }
// MismatchError carries up to 20 mismatches; errors.Is(err, ErrProjectionMismatch).
type MismatchError struct {
	Code       error
	Mismatches []Mismatch
	Total      int
}
func (e *MismatchError) Error() string // "<code>: <total> mismatches; first: <table> <key> <field> ledger=<…> projection=<…>"
func (e *MismatchError) Unwrap() error

// Report (report.go): Task 18 completes it.
type Phase struct {
	Name     string        `json:"name"`
	Duration time.Duration `json:"duration_ns"`
}
type Report struct {
	ImportID      string           `json:"import_id"`
	Start         Start            `json:"start"`
	LegacyHead    HeadJSON         `json:"legacy_head"`
	InstanceID    string           `json:"instance_id"`
	RecoveryEpoch uint64           `json:"recovery_epoch"`
	DryRun        bool             `json:"dry_run"`
	Committed     bool             `json:"committed"`
	Counts        map[string]int64 `json:"counts"`  // table -> rows loaded
	Skipped       map[string]int64 `json:"skipped"` // reason -> rows not carried
	Phases        []Phase          `json:"phases"`
}
type HeadJSON struct {
	Generation uint64 `json:"generation"`
	Revision   uint64 `json:"revision"`
	EventHash  string `json:"event_hash"`
}

// export_test.go
// Pipeline runs preflight, begins the import transaction and runs importSteps
// up to and including through. The transaction rolls back at cleanup.
func Pipeline(t *testing.T, o Options, through string) (pgx.Tx, Report)
// TryPipeline is Pipeline returning the first error instead of failing the test.
func TryPipeline(t *testing.T, o Options, through string) (pgx.Tx, error)
// PipelineCommit runs through `through`, then "restore", and commits (the legacy
// schemas stay; services composed afterwards read the committed rows).
func PipelineCommit(t *testing.T, o Options, through string) Report
// SetFaultForTest sets o.fault, called with each step's name before it runs.
func SetFaultForTest(o *Options, fault func(step string) error)
// StepNames lists importSteps' names in order.
func StepNames() []string
```

- [ ] **Step 1: Write the failing tests** `crosscheck_test.go`. `optionsFor(l)` is a helper in the external test package that builds `legacyimport.Options` from a `legacytest.Legacy`, with `Now` = `l.Now` and the migrator pool `l.DB.Owner`.
```go
func TestCrossCheckAcceptsAConsistentInstall(t *testing.T) {
	l := legacytest.Build(t, legacytest.Spec{Families: 2, Caregivers: 1, EntitiesPerFamily: 10, RotationsPerDevice: 2, RemovedMember: true, NotCarried: true})
	closeOtherPools(l.DB) // the fixture's runtime, revoker and maintenance pools hold connections
	tx, report := legacyimport.Pipeline(t, optionsFor(l), "unforce")
	if report.Start.Kind != legacyimport.StartHomelab29 || report.LegacyHead.Revision != l.Head.Revision {
		t.Fatalf("report %+v", report)
	}
	var forced int
	_ = tx.QueryRow(t.Context(), `SELECT count(*) FROM pg_class WHERE relnamespace='public'::regnamespace AND relforcerowsecurity`).Scan(&forced)
	if forced != 0 {
		t.Fatalf("%d relations still force row security after unforce", forced)
	}
}

func TestCrossCheckRefusesEveryDisagreement(t *testing.T) {
	for _, c := range []struct {
		name, tamper string
		want         error
		field        string
	}{
		{"projection behind", `UPDATE security_projection_head SET revision=revision-1`, legacyimport.ErrProjectionBehind, ""},
		{"account status", `UPDATE account_security_state SET status='identity_unavailable' WHERE account_id=(SELECT min(account_id) FROM account_security_state)`, legacyimport.ErrProjectionMismatch, "status"},
		{"membership role", `UPDATE memberships SET role='viewer' WHERE role='caregiver' AND account_id=(SELECT min(account_id) FROM memberships WHERE role='caregiver')`, legacyimport.ErrProjectionMismatch, "role"},
		{"session parent", `UPDATE sessions SET parent_session_id=NULL WHERE parent_session_id IS NOT NULL AND session_id=(SELECT min(session_id) FROM sessions WHERE parent_session_id IS NOT NULL)`, legacyimport.ErrProjectionMismatch, "parent_session_id"},
		{"rotated session digest", `UPDATE sessions SET access_digest=sha256(access_digest) WHERE status='active' AND parent_session_id IS NOT NULL AND session_id=(SELECT min(session_id) FROM sessions WHERE status='active' AND parent_session_id IS NOT NULL)`, legacyimport.ErrProjectionMismatch, "material_digest"},
		{"extra device", `INSERT INTO devices SELECT gen_random_uuid(), account_id, server_instance_id, canonical_origin, installation_key_id, installation_digest, continuity_key_id, continuity_digest, binding_revoked_at, created_at, updated_at FROM devices LIMIT 1`, legacyimport.ErrProjectionMismatch, "count"},
		{"audit copy", `UPDATE security_audit_events SET event_hash=sha256(event_hash) WHERE revision=2`, legacyimport.ErrProjectionMismatch, "event_hash"},
		{"server instance", `UPDATE server_state SET instance_id='70000000-0000-4000-8000-0000000000ff'`, legacyimport.ErrIdentityMismatch, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := legacytest.Build(t, legacytest.Spec{Families: 1, Caregivers: 1, EntitiesPerFamily: 3, RotationsPerDevice: 2})
			if _, err := testpg.Superuser(t, l.DB).Exec(t.Context(), `SET session_replication_role=replica; `+c.tamper); err != nil {
				t.Fatal(err)
			}
			closeOtherPools(l.DB)
			_, err := legacyimport.TryPipeline(t, optionsFor(l), "crosscheck")
			var mismatch *legacyimport.MismatchError
			if !errors.Is(err, c.want) || (c.field != "" && (!errors.As(err, &mismatch) || mismatch.Mismatches[0].Field != c.field)) {
				t.Fatalf("%s: %v", c.name, err)
			}
		})
	}
}

func TestPreflightRefusesAConfiguredIdentityThatIsNotTheLedgers(t *testing.T) {
	l := legacytest.Build(t, legacytest.Spec{Families: 1})
	closeOtherPools(l.DB)
	o := optionsFor(l)
	o.InstanceID = "70000000-0000-4000-8000-0000000000ee"
	if _, err := legacyimport.TryPipeline(t, o, "lock"); !errors.Is(err, legacyimport.ErrIdentityMismatch) {
		t.Fatalf("another instance: %v", err)
	}
	o = optionsFor(l)
	o.ActorKey[0] ^= 1
	if _, err := legacyimport.TryPipeline(t, o, "lock"); !errors.Is(err, legacyimport.ErrIdentityMismatch) {
		t.Fatalf("another actor-ID key: %v", err)
	}
}
```
  `TryPipeline` is `Pipeline` returning the error instead of failing the test.
  - `closeOtherPools(f)` closes `f.Runtime`, `f.Revoker` and `f.Maintenance`, plus every `testpg.Superuser` pool the test opened (tests close theirs before the pipeline). It also calls `f.Owner.Reset()`, so no idle owner connection survives besides the one the run acquires.
  - `checkAlone` counts every other client session on the database, whatever its role (Task 3's broader predicate).
  - Add `TestPreflightRefusesAnIdleSecondOwnerConnection`: one extra `f.Owner.Acquire` held open makes `TryPipeline(…, "lock")` fail with `ErrServerRunning`, and the detail names the owner role.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/legacyimport/...' BT_TEST_RUN='TestCrossCheck|TestPreflight' bash Server/scripts/test-db.sh`. Expected: `undefined: legacyimport.Pipeline`.
- [ ] **Step 3: Implement.**
  - **One connection** (Task 3 carry). `preflight` acquires `r.conn` with `o.Pool.Acquire(ctx)`. Every statement of the run goes through `r.conn`:
    - the read-only `Detect`/`checkAlone` transaction;
    - the import transaction (`r.conn.BeginTx`);
    - `verifyOnly`;
    - the post-commit VACUUM.

    `Run`, `Pipeline`, `TryPipeline` and `PipelineCommit` release it when they return. `checkAlone` excludes only `pg_backend_pid()`, so a second connection from the same pool would refuse the run as `server_running`.
  - **`preflight`:**
    1. Run `Detect` and `checkAlone` in `BEGIN READ ONLY` on `r.conn` with `set_config('app.authority','1',true)`, then roll back. A non-importable `Start` returns its refusal; the importable kind is `StartHomelab29`.
    2. Run the first walk: `legacyledger.WalkVerified(ctx, LedgerDir, HeadDir, visit)`, where `visit` appends `c.EventHash` to `facts.hashes`, records `createdBy`, `purgeStarted` and `latest`, and refuses an event whose `At` is later than `now + 5m` (`ErrEventFromTheFuture`).
    3. Require `verified.Identity.InstanceID == o.InstanceID` and `Identity.Epoch == o.RecoveryEpoch`. When `verified.ActorKey != nil`, its commitment must be `authority.ActorKeyCommitment(o.ActorKey, o.InstanceID)` and its instance the same. Anything else is `ErrIdentityMismatch`.
  - **`lockStep`:**
    1. `checkAlone` again.
    2. `SELECT pg_try_advisory_xact_lock(410428716390123)` (the migrate runner's lock ID, `migrate.go:18`); false is `ErrServerRunning` ("another migrate or import holds the lock").
    3. `SET LOCAL lock_timeout = '5s'` and `SET LOCAL statement_timeout = 0`.
    4. `LOCK TABLE public.accounts, public.families, public.sessions, public.mutation_journal IN ACCESS EXCLUSIVE MODE NOWAIT`.
  - **`crossCheckStep`** reads `public.*` (still the legacy schema):
    1. `security_projection_head` must equal `verified.Head`; a lower revision is `ErrProjectionBehind`, anything else a mismatch.
    2. `server_state.instance_id`/`recovery_epoch` must equal the identity (`ErrIdentityMismatch`).
    3. The legacy `security_audit_events` must have exactly `Head.Revision` rows, with `event_hash` by revision equal to `facts.hashes`.
    4. The view comparisons below. Each view is read in key order and merged against the sorted enumerator, so a missing or extra row is a `count` mismatch.
    5. Return `*MismatchError{Code: ErrProjectionMismatch}` when any mismatch exists.

    | Enumerator | Projection | Fields |
    |---|---|---|
    | `Accounts()` | `accounts` ⋈ `account_security_state` | `identity_key`, `status` |
    | `Families()` | `families` ⋈ `family_security_state` | `owner_account_id`, `lifecycle` ↔ `Status`, `purge_after` |
    | `FamilySlots()` | `family_slots` | `family_id`, `state` |
    | `AllMemberships()` | `memberships` | `role`, `status` |
    | `Devices()` | `devices` | `account_id`, `server_instance_id`, `canonical_origin`, `'hmac-sha256:'||continuity_key_id||':'||encode(continuity_digest,'hex')` ↔ `CredentialDigest`, `binding_revoked_at IS NULL` ↔ `Status='active'` |
    | `Invitations()` | `invitations` | `family_id`, `token_digest`, `role`, `status`, `expires_at` |
    | `Sessions()` | `sessions` (not `material_missing`) | `account_id`, `device_id`, `parent_session_id`, `lineage_root_session_id`, `status`, `absolute_expires_at` ↔ `ExpiresAt`, `recovery_epoch`, `security_revision`; for **active** rows with a parent (the only rotated sessions the import carries), `material_digest`: sha256 of `auth.EncodeRefreshStage` built from the row (instance, origin from its device, digests in hex, expiries in RFC 3339 nano) must equal `ProjectionMaterialDigest`. That is what proves a carried access digest is the one the ledger committed. |
    | `Entitlements()` | `family_entitlements` | `status`, `source`, `expires_at` |
    | `RecoveryClaims()` | `family_recovery_claims` | `account_id`, `status` |

    Timestamps compare as instants: parse both sides and use `time.Equal`.
  - **`stageStep`:** `stage(ctx, r.tx, migrate.New(migrations.Files))` (Task 4).
  - **`unforceStep`:**
```go
func unforceStep(ctx context.Context, r *importRun) error {
	forced, err := names(ctx, r.tx, `SELECT c.oid::regclass::text FROM pg_class c
		WHERE c.relnamespace = 'public'::regnamespace AND c.relforcerowsecurity ORDER BY 1`)
	if err != nil {
		return err
	}
	triggered, err := names(ctx, r.tx, `SELECT DISTINCT c.oid::regclass::text FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
		WHERE c.relnamespace = 'public'::regnamespace AND NOT t.tgisinternal AND t.tgenabled <> 'D' ORDER BY 1`)
	if err != nil {
		return err
	}
	for _, rel := range forced {
		if _, err := r.tx.Exec(ctx, `ALTER TABLE `+rel+` NO FORCE ROW LEVEL SECURITY`); err != nil {
			return fmt.Errorf("legacyimport: unforce %s: %w", rel, err)
		}
	}
	for _, rel := range triggered {
		if _, err := r.tx.Exec(ctx, `ALTER TABLE `+rel+` DISABLE TRIGGER USER`); err != nil {
			return fmt.Errorf("legacyimport: disable triggers on %s: %w", rel, err)
		}
	}
	r.forced, r.triggers = forced, triggered
	return nil
}
```
    The bulk load copies rows as the table owner, which COPY allows only without forced RLS; the user triggers (owner checks, label history, care guards) would double-write or refuse rows that already hold their final values. Task 13's `restoreStep` puts both back, and the catalog test proves it.
  - **`restoreStep`:** `ALTER TABLE … FORCE ROW LEVEL SECURITY` for each of `r.forced`, then `ALTER TABLE … ENABLE TRIGGER USER` for each of `r.triggers`. Both lists are exactly what `unforceStep` recorded.
  - **`Pipeline`/`TryPipeline`/`PipelineCommit`** (`export_test.go`):
    1. Call `preflight`, begin on `r.conn` with `set_config('app.authority','1',true)`, then run `importSteps` until `through`, calling `o.fault` before each step.
    2. Time each step into `report.Phases`.
    3. `Pipeline` and `TryPipeline` roll back at cleanup. `PipelineCommit` runs `restore` (if `through` came before it) and commits.

    A test pipeline that stops before `restore` must not commit.
- [ ] **Step 4: Run green:** the Step 2 command.
- [ ] **Step 5: Commit:** `rtk git add Server/internal/legacyimport && rtk git commit -m "feat(legacyimport): verify the ledger, lock the install and cross-check the legacy projection"`



### Task 14: The Live Activity worker on the maintenance pool, the authority snapshot, and composition [Wave D; after Tasks 7, 11 and 12]

**Files:**
- Create `Server/internal/authority/live_activity.go` and `live_activity_test.go`. They replace `main`'s `security.LiveActivitySnapshotGuard` (`main:Server/internal/security/live_activity_snapshot.go`).
- Create the following in `Server/internal/liveactivity/`:
  - `queue.go`: `main`'s, verbatim, plus three maintenance calls: `SweepRegistrations(ctx context.Context, requireEntitlement bool) (int, error)` (`app.sweep_live_activity_registrations($1, 100)`), `SweepWork(ctx context.Context) error` (`app.sweep_live_activity_work()`) and `FlushDirty(ctx context.Context, families []string, sequences []uint64) (int, error)` (`app.dirty_live_activity_families`);
  - `authorization.go`: `SubjectFromClaim`, now returning `authority.LiveActivitySubject`;
  - `transport.go`: `main`'s `PushRequest`, `PushResult`, `PushClassification`, `TokenKind`, `ActivityAttributes`, `Transport`, `ErrRequestNotSent`, `validatePushRequest` and `encodePushPayload`, verbatim from `main:…/apns.go`, plus the adapter below;
  - `dirty.go`;
  - `worker.go`: `main`'s, with the substitutions below;
  - the tests `worker_test.go`, `worker_outcomes_test.go`, `worker_hardening_test.go` and `authorization_test.go`, ported from `main`; `transport_test.go`, new.
- Modify `Server/internal/delivery/observer.go`: `Changes(recorders ...ChangeRecorder) ChangeRecorder` fans one committed advance out to several recorders.
- Create `Server/cmd/baby-tracker-server/live_activity.go` and `live_activity_test.go`; modify `main.go` (composition).
- Modify `Server/cmd/baby-tracker-server/performance_composition_identitytest_test.go`: a `proveLiveActivityComposition` pins the wiring, as `proveWakeComposition` does.

**Interfaces:** Consumes Tasks 7, 8, 11 and 12. Produces:
```go
package authority
var ErrLiveActivityAuthorizationChanged error = Code("live_activity_authorization_changed")
// LiveActivitySubject is a lease-bound scheduling coordinate (main's security.LiveActivitySubject).
type LiveActivitySubject struct {
	JobID, LeaseID, RegistrationID       string
	RegistrationVersion                  uint64
	AccountID, DeviceID, FamilyID        string
	BabyID, Kind, InstanceID             string
	RecoveryEpoch, DirtyVersion          uint64
	LeaseDeadline                        time.Time
}
// WithLiveActivitySnapshot runs run in one read-only repeatable-read runtime
// transaction with the subject's tenant context, after one pipelined round trip
// checks that the lease is live (app.live_activity_claim_live) and the
// registration is authorized (app.live_activity_authorized, node entitlement
// policy from Options). Anything else is ErrLiveActivityAuthorizationChanged.
// The grant carries the member's role; CanWrite follows family.Allowed(role,"mutate_record").
func (s *Service) WithLiveActivitySnapshot(ctx context.Context, subject LiveActivitySubject, run func(pgx.Tx, access.Grant) error) error

package liveactivity
// APNSTransport is main's Transport over the branch's one APNs client.
type APNSTransport struct{ Client *apns.Client }
func (t APNSTransport) Send(ctx context.Context, r PushRequest) (PushResult, error)
// Dirty collects committed family advances from the post-commit change feed (bounded at 100,000 families).
type Dirty struct{ /* mu; families map[string]uint64 */ }
func NewDirty() *Dirty
func (d *Dirty) Changed(familyID string, sequence uint64) // delivery.ChangeRecorder
type SnapshotGuard interface {
	WithLiveActivitySnapshot(context.Context, authority.LiveActivitySubject, func(pgx.Tx, access.Grant) error) error
}
type Worker struct {
	Queue              *Queue // on the maintenance pool
	Guard              SnapshotGuard
	Transport          Transport
	Dirty              *Dirty
	Slots              *database.WorkerSlots
	RequireEntitlement bool // hosted mode
	Now                func() time.Time
	Jitter             func(time.Duration) time.Duration
	Logger             *slog.Logger
}
func NewWorker(q *Queue, g SnapshotGuard, t Transport, d *Dirty, slots *database.WorkerSlots) *Worker
func (w *Worker) Run(ctx context.Context, ready func(context.Context) error) error
func (w *Worker) RunOnce(ctx context.Context, limit int) error
// Ready is nil after the first full pass (claim, dirty flush, sweep), until Run stops.
func (w *Worker) Ready(ctx context.Context) error

package delivery
func Changes(recorders ...ChangeRecorder) ChangeRecorder
```

**`worker.go`: `main`'s, with exactly these substitutions:**
- `security.IsCodedError(err)` → `authority.CodeOf(err) != "service_unavailable"`.
- `security.ErrLiveActivityAuthorizationChanged` → `authority.ErrLiveActivityAuthorizationChanged`.
- `security.Code(err)` → `authority.CodeOf(err)`.
- `SnapshotGuard.SweepLiveActivityRegistrations` → `Queue.SweepRegistrations(ctx, w.RequireEntitlement)` on the maintenance pool, repeated while it returns 100.
- `maintain`, once a minute, runs that sweep, then `Queue.SweepWork` (moved out of the claim), then `Retain`.
- Every poll first flushes `Dirty` through `Queue.FlushDirty` (batches of 1,000) and claims only after that. A failed flush puts the families back into `Dirty`, newer sequences winning.
- The `snapshot` step:
  - the device read is unchanged (it runs inside the authority snapshot, which has tenant context);
  - `(SnapshotReader{}).Read` is Task 12's;
  - the timer-to-record link uses `ReadCompletedSession`;
  - `SuppressStart` uses `origin, err := ReadTimerOrigin(...)` and `out.state.SuppressStart = origin.Sequence != 0 && origin.DeviceID == c.DeviceID && origin.Sequence > registeredSequence`.
- `processSends` (2 in flight) and the batch limit 32 are unchanged. Each claim and finish runs inside `w.Slots.Do`.
- Logs carry the class, status and reason only (unchanged).

**`APNSTransport.Send`:**
1. `validatePushRequest(r)` and `encodePushPayload(r)`; either failing returns `ConfigurationFailure` and `errors.Join(ErrRequestNotSent, err)`.
2. `hex.DecodeString(r.Token)`.
3. `apns.Request{Environment: apns.Environment(r.Environment), Token: raw, PushType: apns.PushLiveActivity, Priority: r.Priority, Expiration: exp, APNsID: r.APNSID, Payload: payload}`, where `exp` is 0 for a start and `ceilUnix(r.Expiration)` otherwise (as `main`).
4. Map `apns.Result` to `PushResult` (same class strings, status, reason and `RetryAfter`), and `apns.ErrNotSent` to `ErrRequestNotSent`.
5. `Ambiguous` keeps its non-nil error. `main`'s worker treats `Ambiguous` by class, so the cause is only logged.

**Composition (`cmd/baby-tracker-server/live_activity.go`):**
- `liveactivity.NewUnregistration(authorityService)` is always set.
- When `cfg.LiveActivity.Enabled`:
  - `dirty := liveactivity.NewDirty()`;
  - the change feed becomes `delivery.NewObserver(liveHub, delivery.Changes(tracker, dirty))`;
  - the worker is `liveactivity.NewWorker(liveactivity.NewQueue(maintenancePool), authorityService, liveactivity.APNSTransport{Client: apnsClient}, dirty, maintenanceSlots)`, run with `go worker.Run(ctx, monitor.Check)`;
  - `Options.LiveActivityService` is `liveactivity.NewService(authorityService, cfg.LiveActivity.Credentials.BundleID+".push-type.liveactivity")`;
  - `Options.LiveActivityReady` is `worker.Ready`.
- `live-activity-push-v1` is advertised only then, as `export-v1` is.
- `main`'s `databasePoolBudget`, worker login, `liveActivityRuntime.supervise` and `live-activity-preflight` are not ported: there is no separate pool, readiness gates `Run`, and `apns-preflight` exists (Task 7).

- [ ] **Step 1: Write the failing tests.**
  - **`liveactivity/worker_test.go`, `worker_outcomes_test.go`, `worker_hardening_test.go`:** `main`'s 24 + 9 + 6 tests, ported.
    - `testkit.New` replaces `main`'s harness.
    - `h.Authority` is the guard (`security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)` becomes `h.Authority`).
    - `liveactivity.NewQueue(h.Maintenance)` is the queue, and `database.NewWorkerSlots(h.Maintenance)` supplies the slots.
    - Registrations go through Task 11's `Service`.
    - Care writes go through `h.StartFeed`/`h.StopFeed`/`caretest`, and a test calls `dirty.Changed(h.FamilyID, h.FamilySequence(t))` where `main` relied on the journal trigger.
    - Assertions on pushed payloads, decisions, priorities, lifecycles, adoption, lost-ack starts and outcomes are `main`'s, unchanged. Where `main` used `membership_revision`, the test uses a removal and a rejoin.
  - **Review Focus 5, new tests in `worker_test.go`:**
```go
func TestRemovedMemberGetsNoLiveActivityPush(t *testing.T) { // Review Focus 5
	h, w, sent := liveActivityHarness(t) // a worker over a recording Transport; owner and caregiver registered with push-to-start tokens
	h.StartFeed(t, h.Owner, testkit.TimerA)
	drain(t, h, w) // Dirty.Changed + RunOnce until idle
	if !sent.has(h.Caregiver.DeviceID, "start") {
		t.Fatal("the caregiver's phone got no push-to-start")
	}
	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID); err != nil {
		t.Fatal(err)
	}
	sent.reset()
	h.StopFeed(t, h.Owner, testkit.TimerA, testkit.RecordA)
	drain(t, h, w)
	if sent.any(h.Caregiver.DeviceID) {
		t.Fatalf("a removed member's phone was pushed: %v", sent.events(h.Caregiver.DeviceID))
	}
	var revoked bool
	_ = h.Migrator.QueryRow(t.Context(), `SELECT bool_and(revoked) FROM live_activity_devices WHERE device_id=$1`, h.Caregiver.DeviceID).Scan(&revoked)
	if !revoked {
		t.Fatal("the sweep left the removed member's registration live")
	}
}

func TestStartingPhoneGetsNoPushToStart(t *testing.T) { // Review Focus 5
	h, w, sent := liveActivityHarness(t)
	h.StartFeed(t, h.Owner, testkit.TimerA) // the owner's phone shows its own local card
	drain(t, h, w)
	if sent.has(h.Owner.DeviceID, "start") || !sent.has(h.Caregiver.DeviceID, "start") {
		t.Fatalf("starts owner=%v caregiver=%v", sent.events(h.Owner.DeviceID), sent.events(h.Caregiver.DeviceID))
	}
}

func TestStoppedTimerEndsItsCardOnce(t *testing.T) { // Review Focus 5
	// start (caregiver gets a start), register the pushed card's update token (Task 11 RegisterActivity), stop:
	// exactly one "end" with dismissal-date to that token across five more drains; no further update.
}

func TestInvalidCardTokenIsClearedOnce(t *testing.T) { // Review Focus 5
	// the recording transport answers 410 Unregistered to the card's update token: live_activity_tokens.token is NULL,
	// state 'ended', server_retired; a later write sends nothing to it; a 429 answer pauses every send until Retry-After.
}
```
    `liveActivityHarness`, `drain` and the recording transport are helpers beside `main`'s ported ones.
  - **`liveactivity/transport_test.go`,** over `apns.NewWithTransport` and a local TLS HTTP/2 server:
    - a start sends `apns-push-type: liveactivity`, topic `<bundle>.push-type.liveactivity`, priority 10, expiration 0, the claim's `apns-id`, and `main`'s payload (decoded with `ContentState`);
    - an update sends priority 5 or 10 as decided, with an expiration;
    - an invalid request is `ErrRequestNotSent` with no request on the server;
    - `410 Unregistered` → `InvalidToken`;
    - `403 InvalidProviderToken` → `ConfigurationFailure`, and the provider token is re-signed only after its 20-minute floor (the branch's rule);
    - `429` carries `RetryAfter`.
  - **`authority/live_activity_test.go`:**
    - a live lease and an authorized registration run `run` with `Grant.Role` equal to the member's role, in exactly 1 round trip before `run` (counted pool);
    - a stale lease, a revoked session lineage, a lapsed hosted entitlement (with `RequireFamilySyncEntitlement`) or a removed member each return `ErrLiveActivityAuthorizationChanged` without calling `run`;
    - `run`'s transaction is read-only (an `INSERT` fails with 25006).
  - **`delivery/observer_test.go`:** `Changes(a, b)` delivers each advance to both, in order.
  - **`cmd/baby-tracker-server/live_activity_test.go`:**
    - the switch off: no worker started, the PUT routes 404, DELETE served, no capability;
    - the switch on with a fake transport: the worker runs on the maintenance pool, the capability is advertised once the worker is ready, and one APNs client is shared with the wake pusher (the same pointer);
    - `BT_LIVE_ACTIVITY_DATABASE_URL_FILE` set: the server refuses to start with the maintenance-credential message.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/liveactivity ./internal/authority ./internal/delivery ./cmd/baby-tracker-server' BT_TEST_RUN='LiveActivity|TestRemovedMember|TestStartingPhone|TestStoppedTimer|TestInvalidCardToken|TestChanges|Transport' bash Server/scripts/test-db.sh`. Expected: `undefined: liveactivity.NewWorker`, `undefined: authority.LiveActivitySubject`.
- [ ] **Step 3: Implement** as specified. `WithLiveActivitySnapshot`:
```go
func (s *Service) WithLiveActivitySnapshot(ctx context.Context, subject LiveActivitySubject, run func(pgx.Tx, access.Grant) error) error {
	if run == nil || !validID(subject.RegistrationID) || !validID(subject.AccountID) || !validID(subject.FamilyID) {
		return ErrInvalidCommand
	}
	claim, err := json.Marshal(subject) // the field names main's claim JSON uses (json tags as in main's Claim)
	if err != nil {
		return ErrInvalidCommand
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return unavailable("live activity snapshot", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	batch := &pgx.Batch{}
	batch.Queue(`SELECT set_config('app.account_id',$1,true), set_config('app.family_id',$2,true)`, subject.AccountID, subject.FamilyID)
	var live bool
	var role *string
	batch.Queue(`SELECT app.live_activity_claim_live($1), app.live_activity_authorized($2,$3,$4)::text`,
		claim, subject.RegistrationID, int64(subject.RegistrationVersion), s.requireFamilySyncEntitlement).QueryRow(func(r pgx.Row) error { return r.Scan(&live, &role) })
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return unavailable("live activity snapshot", err)
	}
	if !live || role == nil {
		return ErrLiveActivityAuthorizationChanged
	}
	return run(tx, access.Grant{AccountID: subject.AccountID, FamilyID: subject.FamilyID, Role: *role})
}
```
  The `set_config` pair is the tenant context `tenant.SetLocal` sets; use `tenant.SetLocal`'s statement text if it differs, so the context stays defined in one place. `LiveActivitySubject` carries the JSON tags of `main`'s `Claim` (`job_id`, `lease_id`, …).
- [ ] **Step 4: Run green:**
  - the Step 2 command;
  - `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/liveactivity' bash Server/scripts/test-db.sh` three times (`main`'s hardening tests are timing-sensitive; three green runs are required);
  - `rtk make server-test`;
  - `(cd Server && rtk go vet ./... && rtk go vet -tags identitytest ./...)`;
  - `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./tests/protocol/... ./cmd/...' BT_TEST_TAGS=identitytest bash Server/scripts/test-db.sh`.
- [ ] **Step 5: Commit:** `rtk git add Server/internal/liveactivity Server/internal/authority Server/internal/delivery Server/cmd/baby-tracker-server && rtk git commit -m "feat(liveactivity): the push worker on the maintenance pool over the authority and one APNs client"`


### Task 15: Load the authority, active sessions, the server identity and the audit history [Wave E; after Tasks 5 and 13; parallel with Task 16]

**Files:**
- Create `Server/internal/legacyimport/{authority_load.go,audit_load.go,authority_load_test.go,audit_load_test.go}`.
- Modify `Server/internal/legacyimport/run.go`: insert `{"authority", authorityStep}` before `restore`. This is a one-line merge with Task 16's line.
- Create `Server/internal/testkit/compose.go` (`Compose`, `PostgresOf`). `testkit/fixtures.go`'s `newHarnessOn` calls `Compose` after it writes `server_state`, with no change in behaviour.

**Interfaces:** Consumes Task 5's `LegacyChains`/`CopyAuditRows`/`RegisterAuditTypes`/`VerifyChainsTx` and Task 13's `importRun`/`ledgerFacts`. Produces:
```go
package legacyimport
// authorityStep loads, from legacy_public into the new schema, in FK order:
// accounts, account_identities, devices, families, memberships, family_slots,
// invitations, family_entitlements, family_recovery_claims, family_actor_labels
// and their history, server_state, the active sessions; then the audit history
// (the second walk) and the chain heads. Everything else is counted in Skipped.
func authorityStep(ctx context.Context, r *importRun) error
func loadAudit(ctx context.Context, r *importRun) (rows int64, scopes int, err error)

package testkit
type Composition struct {
	InstanceID     string
	RecoveryEpoch  uint64
	TokenKeys      identity.DigestKeyring
	IdentityKeys   identity.DigestKeyring // zero: testkit's defaults
	ContinuityKeys identity.DigestKeyring
	InvitationKeys identity.DigestKeyring
	PageKeys       identity.DigestKeyring
	Origin         string                 // "": testkit's origin
	Now            time.Time
	Hosted         bool
}
// Compose builds the harness services (Authority, Auth, Identity, Family,
// Service, Importer) over an existing database, without seeding anything.
func Compose(t *testing.T, db Postgres, c Composition) *Harness
func PostgresOf(f *testpg.Fixture) Postgres
```

- [ ] **Step 1: Write the failing tests.**
  - `authority_load_test.go`:
```go
func importAuthority(t *testing.T, spec legacytest.Spec) (*legacytest.Legacy, *testkit.Harness, legacyimport.Report) {
	t.Helper()
	l := legacytest.Build(t, spec)
	closeOtherPools(l.DB)
	report := legacyimport.PipelineCommit(t, optionsFor(l), "authority")
	db := reopen(t, l.DB) // fresh runtime and maintenance pools on the imported database
	return l, testkit.Compose(t, testkit.PostgresOf(db), testkit.Composition{InstanceID: l.InstanceID, RecoveryEpoch: l.Epoch,
		TokenKeys: l.TokenKeys, Now: l.Now}), report
}

func TestImportedPhonesStaySignedInAndRefresh(t *testing.T) {
	l, h, report := importAuthority(t, legacytest.Spec{Families: 1, Caregivers: 1, Viewers: 1, RotationsPerDevice: 2, RemovedMember: true, NotCarried: true})
	fam := l.Families[0]
	owner, caregiver, viewer, removed := fam.Members[0], fam.Members[1], fam.Members[2], fam.Members[3]
	for _, m := range []legacytest.Member{owner, caregiver, viewer} {
		p, err := h.Auth.Authenticate(t.Context(), m.AccessToken)
		if err != nil || p.AccountID != m.AccountID || p.DeviceID != m.DeviceID || p.SessionScope != "family" || p.SessionFamilyID != fam.ID {
			t.Fatalf("%s: %+v %v", m.Subject, p, err)
		}
	}
	if _, err := h.Auth.Authenticate(t.Context(), removed.AccessToken); auth.Code(err) != "session_revoked" {
		t.Fatalf("a removed member's legacy token: %v", err)
	}
	rotated, err := h.Auth.Refresh(t.Context(), caregiver.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Auth.Authenticate(t.Context(), rotated.AccessToken); err != nil {
		t.Fatalf("the child of an imported session: %v", err)
	}
	var role string
	if err := h.Migrator.QueryRow(t.Context(), `SELECT role::text FROM sessions WHERE access_digest=$1`, digest(l, viewer.AccessToken)).Scan(&role); err != nil || role != "viewer" {
		t.Fatalf("viewer session role %q %v", role, err)
	}
	var notActive int
	_ = h.Migrator.QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE status <> 'active'`).Scan(&notActive)
	if notActive != 1 { // only the parent the refresh above just consumed
		t.Fatalf("%d non-active sessions: the import carries active sessions only", notActive)
	}
	for _, reason := range []string{"sessions_not_active", "refresh_recovery_receipts", "security_command_receipts"} {
		if report.Skipped[reason] == 0 {
			t.Fatalf("skipped %v lacks %s", report.Skipped, reason)
		}
	}
}

func TestImportedFamiliesAndIdentityAreExact(t *testing.T) {
	l, h, _ := importAuthority(t, legacytest.Spec{Families: 2, Caregivers: 1, DeletionPending: true})
	var entitled, pending int
	_ = h.Migrator.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE entitlement_required), count(*) FILTER (WHERE status='deletion_pending') FROM families`).Scan(&entitled, &pending)
	if entitled != 0 || pending != 1 { // the homelab is self-hosted: no family_bootstrapped event
		t.Fatalf("entitlement_required %d, deletion_pending %d", entitled, pending)
	}
	state, err := authority.LoadServerState(t.Context(), h.Migrator)
	if err != nil || state.Check(l.InstanceID, l.Epoch, l.ActorKey) != nil {
		t.Fatalf("server identity %+v %v", state, err)
	}
	var cursors, tokens, registrations int
	_ = h.Migrator.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM device_sync_cursors), (SELECT count(*) FROM device_push_tokens), (SELECT count(*) FROM live_activity_devices)`).Scan(&cursors, &tokens, &registrations)
	if cursors+tokens+registrations != 0 {
		t.Fatalf("cursors %d tokens %d registrations %d: all must start empty", cursors, tokens, registrations)
	}
}
```
  `reopen` and `digest` are helpers in the test file.
  - `audit_load_test.go`:
```go
func TestImportedChainsVerifyAndNewCommandsChainOnTop(t *testing.T) { // Review Focus 4
	l, h, _ := importAuthority(t, legacytest.Spec{Families: 2, Caregivers: 1, Viewers: 1, RotationsPerDevice: 2, RemovedMember: true,
		EventAt: func(i int) time.Time { // the first events predate 2026: they land in the default partition
			return time.Date(2025, 12, 31, 23, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute)
		}})
	verifier, err := authority.NewAuditVerifier(h.Migrator, func() time.Time { return l.Now }, nil)
	if err != nil {
		t.Fatal(err)
	}
	report, err := verifier.VerifyAll(t.Context())
	if err != nil || len(report.Failed) != 0 || report.Scopes == 0 {
		t.Fatalf("verify after import %+v %v", report, err)
	}
	var anchors, accounts, families, early int
	tx := legacytest.AuthorityTx(t, h.Migrator)
	_ = tx.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE kind='legacy_imported'),
		(SELECT count(*) FROM accounts), (SELECT count(*) FROM families),
		(SELECT count(*) FROM security_audit_events_default WHERE legacy_hash IS NOT NULL) FROM security_audit_events`).Scan(&anchors, &accounts, &families, &early)
	_ = tx.Rollback(t.Context())
	if anchors != accounts+families || early == 0 {
		t.Fatalf("anchors %d for %d scopes; %d pre-2026 rows in the default partition", anchors, accounts+families, early)
	}
	owner, viewer := l.Families[0].Members[0], l.Families[0].Members[2]
	p, _ := h.Auth.Authenticate(t.Context(), owner.AccessToken)
	if _, err := h.Family.ChangeRole(t.Context(), p, h.CommandID(), viewer.AccountID, "caregiver"); err != nil {
		t.Fatalf("a command on an imported chain: %v", err)
	}
	if _, err := h.Auth.Refresh(t.Context(), owner.RefreshToken); err != nil {
		t.Fatalf("a refresh on an imported chain: %v", err)
	}
	if report, err := verifier.VerifyAll(t.Context()); err != nil || len(report.Failed) != 0 {
		t.Fatalf("verify after new commands %+v %v", report, err)
	}
	var created, skipped int
	if err := h.Maintenance.QueryRow(t.Context(), `SELECT * FROM app.ensure_audit_partitions(now() + interval '92 days')`).Scan(&created, &skipped); err != nil || skipped != 0 {
		t.Fatalf("the roller after an import: created %d skipped %d %v", created, skipped, err)
	}
}

func TestLedgerChangedBetweenWalksIsRefused(t *testing.T) {
	l := legacytest.Build(t, legacytest.Spec{Families: 1})
	closeOtherPools(l.DB)
	o := optionsFor(l)
	legacyimport.SetFaultForTest(&o, func(step string) error {
		if step == "authority" { // between the verifying walk and the audit walk, the head file moves
			return legacytest.AppendForeignEvent(l)
		}
		return nil
	})
	if _, err := legacyimport.TryPipeline(t, o, "authority"); !errors.Is(err, legacyimport.ErrLedgerChanged) {
		t.Fatalf("a ledger that moved during the import: %v", err)
	}
}
```
- [ ] **Step 2: Run to see the failure:** `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/legacyimport/... ./internal/testkit' BT_TEST_RUN='TestImported|TestLedgerChanged' bash Server/scripts/test-db.sh`. Expected: `undefined: testkit.Compose`, and `unknown step "authority"`.
- [ ] **Step 3: Implement.**
  - **`pg_temp.import_families`.** First create `pg_temp.import_families(id uuid PRIMARY KEY, status text, owner_account_id uuid, purge_started_at timestamptz, purge_command_id uuid, purge_receipt uuid, purged_at timestamptz, entitlement_required boolean) ON COMMIT DROP` and fill it with `CopyFrom` from `verified.State.Families()` and `facts`:
    - `entitlement_required = facts.createdBy[id] == "family_bootstrapped"`, because `authority/family.go:167` writes it from `bootstrap`; it is false on the self-hosted homelab;
    - `purge_started_at = facts.purgeStarted[id]`;
    - the purge IDs come from `FamilyView`.
  - **Refusals before loading:**
    - any `legacy_public.account_identities` row with `material_missing` is `ErrIdentityMaterial` (with the account IDs);
    - any `legacy_public.devices.server_instance_id <> $instance` is `ErrForeignDevice`.
  - **Loads,** one statement each, in this order (`$1` = `r.now`):
```sql
INSERT INTO public.accounts(id, display_name, status, created_at, updated_at, profile_required, lifecycle_generation)
SELECT a.id, a.display_name, s.status::app.account_status, a.created_at, a.updated_at, a.profile_required, s.lifecycle_generation
FROM legacy_public.accounts a JOIN legacy_public.account_security_state s ON s.account_id = a.id;

INSERT INTO public.account_identities(provider, identity_key_id, identity_digest, account_id, lifecycle_generation, audience, subject, live, created_at)
SELECT i.provider, i.identity_key_id, i.identity_digest, i.account_id, s.lifecycle_generation, i.identity_audience, i.identity_subject,
       s.status <> 'deleted', i.created_at
FROM legacy_public.account_identities i JOIN legacy_public.account_security_state s ON s.account_id = i.account_id;

INSERT INTO public.devices(id, account_id, canonical_origin, continuity_key_id, continuity_digest, status, revoked_at, created_at, updated_at)
SELECT id, account_id, canonical_origin, continuity_key_id, continuity_digest,
       (CASE WHEN binding_revoked_at IS NULL THEN 'active' ELSE 'revoked' END)::app.device_status, binding_revoked_at, created_at, updated_at
FROM legacy_public.devices;

INSERT INTO public.families(id, display_name, status, created_at, updated_at, purge_after, profile_required, next_sequence, owner_account_id,
  purge_started_at, purge_command_id, purge_receipt, purged_at, entitlement_required)
SELECT f.id, f.display_name, x.status::app.family_status, f.created_at, f.updated_at, f.purge_after, f.profile_required, f.next_sequence,
       x.owner_account_id, x.purge_started_at, x.purge_command_id, x.purge_receipt, x.purged_at, x.entitlement_required
FROM legacy_public.families f JOIN pg_temp.import_families x ON x.id = f.id;

INSERT INTO public.memberships(family_id, account_id, role, status, created_at, updated_at)
SELECT family_id, account_id, role::app.member_role, status::app.member_status, created_at, updated_at FROM legacy_public.memberships;
INSERT INTO public.family_slots(account_id, family_id, state) SELECT account_id, family_id, state::app.slot_state FROM legacy_public.family_slots;
INSERT INTO public.invitations(id, family_id, creator_account_id, token_digest, role, status, expires_at, redeemed_at, revoked_at, created_at, updated_at)
SELECT id, family_id, creator_account_id, token_digest, role::app.member_role, status::app.invitation_status, expires_at, redeemed_at, revoked_at, created_at, updated_at
FROM legacy_public.invitations;
INSERT INTO public.family_entitlements(family_id, entitlement_key, source, status, expires_at, created_at, updated_at)
SELECT family_id, entitlement_key, source, status::app.entitlement_status, expires_at, created_at, updated_at FROM legacy_public.family_entitlements;
INSERT INTO public.family_recovery_claims(family_id, account_id, status)
SELECT family_id, account_id, (CASE status WHEN 'expired' THEN 'forfeited' ELSE status END)::app.claim_status FROM legacy_public.family_recovery_claims;
INSERT INTO public.family_actor_labels SELECT * FROM legacy_public.family_actor_labels;
INSERT INTO public.family_actor_label_history(label_version, family_id, account_id, display_name, former, recorded_at) OVERRIDING SYSTEM VALUE
SELECT label_version, family_id, account_id, display_name, former, recorded_at FROM legacy_public.family_actor_label_history;
SELECT setval(pg_get_serial_sequence('public.family_actor_label_history', 'label_version'),
  coalesce((SELECT max(label_version) FROM public.family_actor_label_history), 1),
  (SELECT count(*) > 0 FROM public.family_actor_label_history));

INSERT INTO public.server_state(singleton, instance_id, recovery_epoch, updated_at, actor_key_commitment)
SELECT true, instance_id, recovery_epoch, $1, $2 FROM legacy_public.server_state WHERE singleton;  -- $2 = authority.ActorKeyCommitment(ActorKey, instance)

-- Active, unexpired sessions only (D3). The role comes from the membership (owner for family_recovery).
INSERT INTO public.sessions(access_digest, id, account_id, device_id, lineage_root_id, parent_id, lineage_started_at, scope, family_id, role,
  recovery_epoch, issued_seq, token_key_id, refresh_digest, access_expires_at, refresh_expires_at, deadline_at, status, created_at)
SELECT s.access_digest, s.session_id, s.account_id, s.device_id, s.lineage_root_session_id, s.parent_session_id, root.created_at,
       s.scope::app.session_scope, s.family_id,
       (CASE WHEN s.scope = 'account' THEN NULL WHEN s.scope = 'family_recovery' THEN 'owner' ELSE m.role END)::app.member_role,
       s.recovery_epoch, s.security_revision, s.token_key_id, s.refresh_digest, s.access_expires_at, s.refresh_expires_at, s.absolute_expires_at,
       'active', s.created_at
FROM legacy_public.sessions s
JOIN legacy_public.sessions root ON root.session_id = s.lineage_root_session_id
LEFT JOIN legacy_public.memberships m ON m.family_id = s.family_id AND m.account_id = s.account_id
WHERE s.status = 'active' AND s.refresh_expires_at > $1 AND s.absolute_expires_at > $1;
```
  - **Counted in `report.Skipped`,** one `SELECT count(*)` each on the legacy table:
    - `sessions_not_active` (every session the `INSERT` left out);
    - `session_refresh_history`, `refresh_recovery_receipts`;
    - `security_command_receipts`, `account_profile_commands`, `apple_notification_dedup`;
    - `auth_challenges`, `auth_exchange_material`;
    - `apple_revocation_jobs`, `credential_cleanup_jobs`;
    - `family_command_material`, `operator_audit_events`, `rate_limit_buckets`.
  - **`loadAudit`:**
    1. Call `authority.RegisterAuditTypes`.
    2. Walk a second time: `legacyledger.WalkVerified(…, visit)`, with each event as `authority.LegacyEvent{Kind, CommandID, ActorID: ActorAccountID, At: parse(At), Payload: c.EventJSON, LegacyHash: c.EventHash, Accounts, Families}`. `Accounts`/`Families` are the non-empty `AccountID`/`FamilyID` fields of its effects.
    3. Each walked hash must equal `facts.hashes[revision-1]`, and the head must equal `r.verified.Head`; anything else is `ErrLedgerChanged`.
    4. Copy rows in batches of 5,000 with `CopyAuditRows`.
    5. `Anchor(LegacyAnchor{ImportID: r.report.ImportID, At: r.now, Generation, Revision, HeadHash}, every account, every family)`.
    6. `CopyFrom` `Heads()` into `pg_temp.import_heads(scope text, id uuid, seq bigint, hash bytea)`, then `UPDATE public.accounts a SET audit_seq = h.seq, audit_hash = h.hash FROM pg_temp.import_heads h WHERE h.scope = 'account' AND h.id = a.id`, and the same for `families`.
  - `authorityStep` runs the table loads, then `loadAudit`. Every account and family must have an anchor (`audit_seq` above 0).
- [ ] **Step 4: Run green:** the Step 2 command, then `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/testkit ./internal/sync ./internal/family' bash Server/scripts/test-db.sh` (`Compose` changed nothing for `New`).
  **The review of this task must confirm (Task 5 review, carry M6):**
  - `app.ensure_audit_partitions` after an import skips no month (`skipped = 0` and the monthly partitions are contiguous from the default partition's upper bound through `now() + 92 days`);
  - the next `ChangeRole` and `Refresh` chain on the `legacy_imported` anchors (their `prev_hash` is the anchor's `hash`, and `seq` is the anchor's + 1).

  It must also confirm that a `family_recovery`-scoped session, which the captured outsider holds (Task 20), is carried as stored, with role `owner`.
- [ ] **Step 5: Commit:** `rtk git add Server/internal/legacyimport Server/internal/testkit && rtk git commit -m "feat(legacyimport): load the authority, active sessions, the server identity and per-scope audit chains"`


### Task 16: Load care data through the one encoder [Wave E; after Tasks 8a, 9 and 13; parallel with Task 15]

**Files:**
- Create `Server/internal/care/copy.go` and `copy_test.go`.
- Create `Server/internal/legacyimport/care_load.go` and `care_load_test.go`.
- Modify `Server/internal/legacyimport/run.go`: insert `{"care", careStep}` after `authority` and before `restore`.
- Create `Server/internal/testkit/cursor.go`: `func (h *Harness) CursorAt(sequence uint64) *protocol.Cursor`, the harness's instance, epoch and family at `sequence`. It is a new file, so this task does not touch `fixtures.go`, which Task 15 edits in parallel.

**Interfaces:** Consumes `care.Encode`, `care.Row` (with Task 9's origin fields) and `care.CounterDelta`. Produces:
```go
package care
// CopyImported copies rows (already encoded by Encode) into care_entities with
// one COPY. Rows must be distinct per (family, id) and per (family, changed_seq).
// The caller registered app.care_operation on tx's connection (RegisterCareTypes).
func CopyImported(ctx context.Context, tx pgx.Tx, rows []Row) (int64, error)
func RegisterCareTypes(ctx context.Context, tx pgx.Tx) error

package legacyimport
// careStep loads every family's entities from legacy_public: each entity's latest
// journal version through care.Encode, its origin from its first version, and the
// four family counters; then it checks every legacy typed row field by field.
func careStep(ctx context.Context, r *importRun) error
// checkCare returns ErrCareMismatch (a *MismatchError) when a legacy typed row, a
// journal version or a count disagrees with care_entities.
func checkCare(ctx context.Context, tx pgx.Tx) error
```

- [ ] **Step 1: Write the failing tests.**
  - `care/copy_test.go`, `TestCopyImportedEqualsTheWritePath`:
    1. Seed `caretest.MixedBatch` on a harness and read its rows (`caretest.Rows`).
    2. Delete them from `care_entities` as the owner, then `CopyImported` them back.
    3. The wire, every typed and derived column, the origin columns and `changed_seq` are identical.
  - `legacyimport/care_load_test.go`:
```go
func importCare(t *testing.T, spec legacytest.Spec) (*legacytest.Legacy, *testkit.Harness, legacyimport.Report) {
	t.Helper()
	l := legacytest.Build(t, spec)
	closeOtherPools(l.DB)
	report := legacyimport.PipelineCommit(t, optionsFor(l), "care")
	return l, testkit.Compose(t, testkit.PostgresOf(reopen(t, l.DB)), testkit.Composition{InstanceID: l.InstanceID, RecoveryEpoch: l.Epoch, TokenKeys: l.TokenKeys, Now: l.Now}), report
}

func TestImportedCursorsPullOnlyNewerLatestStates(t *testing.T) { // Review Focus 1
	l, h, _ := importCare(t, legacytest.Spec{Families: 1, Caregivers: 1, EntitiesPerFamily: 40, VersionsPerEntity: 3, Timers: true})
	fam := l.Families[0]
	p, err := h.Auth.Authenticate(t.Context(), fam.Members[1].AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	legacyLatest := legacyLatestByEntity(t, l) // id -> (sequence, canonical_entity) of each entity's latest legacy journal row
	for _, cursor := range []uint64{0, 1, fam.Members[1].Cursor, fam.HighWater - 1, fam.HighWater} {
		got := pullAll(t, h, p, h.CursorAt(cursor)) // follows next_page_token; fails on snapshot_required
		var want []string
		for id, latest := range legacyLatest {
			if latest.Sequence > cursor {
				want = append(want, id)
			}
		}
		slices.Sort(want)
		ids := make([]string, len(got))
		var last uint64
		for i, c := range got {
			ids[i] = c.Meta.ID
			if c.Sequence <= last {
				t.Fatalf("cursor %d: changes out of order", cursor)
			}
			last = c.Sequence
			if !jsonEqual(c.Entity, legacyLatest[c.Meta.ID].Entity) {
				t.Fatalf("cursor %d: %s differs from its latest legacy version", cursor, c.Meta.ID)
			}
		}
		slices.Sort(ids)
		if !slices.Equal(ids, want) {
			t.Fatalf("cursor %d: got %d entities, want %d", cursor, len(ids), len(want))
		}
	}
	if again := pullAll(t, h, p, h.FamilyHighWater(t)); len(again) != 0 {
		t.Fatalf("a pull at the high water returned %d changes", len(again))
	}
}

func TestImportedCountersSequencesAndOriginsAreExact(t *testing.T) {
	l, h, report := importCare(t, legacytest.Spec{Families: 3, Caregivers: 1, EntitiesPerFamily: 25, VersionsPerEntity: 2, Timers: true, NotCarried: true})
	for _, fam := range l.Families {
		caretest.AssertCounters(t, h, fam.ID) // counters.go:42: rows, live receipts (none) and import staging (none)
		var next, maxSeq int64
		_ = h.Migrator.QueryRow(t.Context(), `SELECT f.next_sequence, coalesce(max(c.changed_seq),0) FROM families f
			LEFT JOIN care_entities c ON c.family_id = f.id WHERE f.id = $1 GROUP BY f.next_sequence`, fam.ID).Scan(&next, &maxSeq)
		if uint64(next) != fam.HighWater || maxSeq > next {
			t.Fatalf("family %s: next %d max changed %d", fam.ID, next, maxSeq)
		}
	}
	var missingOrigin int
	_ = h.Migrator.QueryRow(t.Context(), `SELECT count(*) FROM care_entities WHERE created_device_id IS NULL OR created_seq > changed_seq`).Scan(&missingOrigin)
	if missingOrigin != 0 {
		t.Fatalf("%d imported rows without their first version's origin", missingOrigin)
	}
	var receipts, staging int
	_ = h.Migrator.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM care_receipts), (SELECT count(*) FROM import_sessions)`).Scan(&receipts, &staging)
	if receipts+staging != 0 || report.Skipped["mutation_receipts"] == 0 || report.Skipped["import_sessions"] == 0 {
		t.Fatalf("receipts %d staging %d skipped %v", receipts, staging, report.Skipped)
	}
}

func TestImportedBabyIconsAreExact(t *testing.T) { // Review Focus 6
	l, h, _ := importCare(t, legacytest.Spec{Families: 2, Babies: 2, Caregivers: 1, EntitiesPerFamily: 10, VersionsPerEntity: 2})
	for _, fam := range l.Families {
		p, err := h.Auth.Authenticate(t.Context(), fam.Members[1].AccessToken)
		if err != nil {
			t.Fatal(err)
		}
		withIcon, without := 0, 0
		for _, entity := range snapshotAllRaw(t, h, p) { // raw snapshot entities, all pages
			if entity.Kind != "baby" || entity.Tombstoned {
				continue
			}
			legacyIcon := legacyBabyIcon(t, l, fam.ID, entity.ID) // legacy_public.babies.icon, through testpg.Superuser
			switch {
			case legacyIcon != nil && bytes.Contains(entity.Payload, []byte(`"icon":"`+*legacyIcon+`"`)):
				withIcon++
			case legacyIcon == nil && !bytes.Contains(entity.Payload, []byte(`"icon"`)):
				without++
			default:
				t.Fatalf("family %s baby %s: legacy icon %v, imported payload %s", fam.ID, entity.ID, legacyIcon, entity.Payload)
			}
		}
		if withIcon != 1 || without != 1 {
			t.Fatalf("family %s: %d babies with their icon, %d without", fam.ID, withIcon, without)
		}
	}
	// A phone renames the iconed baby without the key (an older build): the icon stays (Task 8a's rule, on imported rows).
	fam := l.Families[0]
	owner, _ := h.Auth.Authenticate(t.Context(), fam.Members[0].AccessToken)
	renamed := caretest.ApplyCommitted(t, h, owner, []protocol.Mutation{renameWithoutIcon(t, h, fam.IconBabyID)})
	if got := renamed.Outcomes[0].CanonicalEntity.Payload.(protocol.BabyPayload); got.Icon == nil || *got.Icon != "girl.2" {
		t.Fatalf("a rename of an imported baby lost its icon: %+v", got)
	}
}

func TestCareCheckRefusesEveryDisagreement(t *testing.T) {
	for _, c := range []struct{ name, tamper string }{
		{"typed row version", `UPDATE feed_entries SET entity_version = entity_version + 1 WHERE id = (SELECT min(id) FROM feed_entries)`},
		{"typed start", `UPDATE sleep_entries SET started_at = started_at + interval '1 minute' WHERE id = (SELECT min(id) FROM sleep_entries)`},
		{"component volume", `UPDATE feed_components SET volume_ml = volume_ml + 1 WHERE kind='bottle' AND feed_id = (SELECT min(feed_id) FROM feed_components WHERE kind='bottle')`},
		{"journal-only live entity", `DELETE FROM diaper_entries WHERE id = (SELECT min(id) FROM diaper_entries WHERE tombstoned_at IS NULL)`},
		{"high water below the journal", `UPDATE families SET next_sequence = next_sequence - 1`},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := legacytest.Build(t, legacytest.Spec{Families: 1, EntitiesPerFamily: 15, VersionsPerEntity: 2})
			if _, err := testpg.Superuser(t, l.DB).Exec(t.Context(), `SET session_replication_role=replica; `+c.tamper); err != nil {
				t.Fatal(err)
			}
			closeOtherPools(l.DB)
			if _, err := legacyimport.TryPipeline(t, optionsFor(l), "care"); !errors.Is(err, legacyimport.ErrCareMismatch) && !errors.Is(err, legacyimport.ErrChangedSeqPastHigh) {
				t.Fatalf("%s: %v", c.name, err)
			}
		})
	}
}

func TestAFamilyOverQuotaIsRefused(t *testing.T) {
	l := legacytest.Build(t, legacytest.Spec{Families: 1, EntitiesPerFamily: 30})
	closeOtherPools(l.DB)
	o := optionsFor(l)
	o.Limits.MaxChargedStorageBytesPerFamily = 1024 // far below 30 entities
	if _, err := legacyimport.TryPipeline(t, o, "care"); !errors.Is(err, legacyimport.ErrFamilyOverQuota) {
		t.Fatalf("over quota: %v", err)
	}
}
```
  `pullAll`, `jsonEqual`, `legacyLatestByEntity`, `snapshotAllRaw`, `legacyBabyIcon` and `renameWithoutIcon` are test helpers.
  - `legacyLatestByEntity` and `legacyBabyIcon` read `legacy_public` through `testpg.Superuser` (`PipelineCommit` keeps the legacy schemas). The test closes that pool before any later pipeline.
  - `snapshotAllRaw` returns each snapshot entity's kind, ID, tombstone flag and raw `payload` bytes.
  - `renameWithoutIcon` is a baby `update` built from the current entity with a new name and `Icon` nil.

  `TestCareCheckRefusesEveryDisagreement` gains the case `{"baby icon", "UPDATE babies SET icon = 'boy.3' WHERE icon = 'girl.2' AND id = (SELECT min(id) FROM babies WHERE icon IS NOT NULL)"}`. Every built family's first baby carries `girl.2`.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/care ./internal/legacyimport/...' BT_TEST_RUN='TestCopyImported|TestImportedCursors|TestImportedCounters|TestImportedBabyIcons|TestCareCheck|TestAFamilyOverQuota' bash Server/scripts/test-db.sh`. Expected: `undefined: care.CopyImported`.
- [ ] **Step 3: Implement.**
  - **`care.CopyImported`** copies into `pgx.Identifier{"care_entities"}`. The column list is exactly the one the upsert writes (`store.go`), origin columns included, in the same order, built from the same `Row` fields (`kind` as `KindCode`, `timer_kind` as `TimerKindCode`, `operation` as text). `RegisterCareTypes` loads `app.care_operation` with `LoadTypes`/`RegisterTypes`.
  - **The versions,** read in one streaming query in `(family_id, sequence)` order:
```sql
SELECT j.family_id::text, j.entity_id::text, j.sequence, j.operation, j.actor_account_id::text, j.device_id::text, j.canonical_entity::text,
       p.sequence, p.operation, p.actor_account_id::text, p.device_id::text, p.canonical_entity::text,
       o.device_id::text, o.sequence,
       t.completed_record_id::text
FROM (SELECT DISTINCT ON (family_id, entity_id) * FROM legacy_public.mutation_journal ORDER BY family_id, entity_id, sequence DESC) j
JOIN LATERAL (SELECT device_id, sequence FROM legacy_public.mutation_journal o
              WHERE o.family_id = j.family_id AND o.entity_id = j.entity_id ORDER BY o.sequence LIMIT 1) o ON true
LEFT JOIN LATERAL (SELECT * FROM legacy_public.mutation_journal p
                   WHERE p.family_id = j.family_id AND p.entity_id = j.entity_id AND p.sequence < j.sequence
                     AND p.canonical_entity -> 'payload' <> 'null'::jsonb
                   ORDER BY p.sequence DESC LIMIT 1) p ON j.canonical_entity -> 'payload' = 'null'::jsonb
LEFT JOIN legacy_public.active_timers t ON t.family_id = j.family_id AND t.id = j.entity_id
ORDER BY j.family_id, j.sequence;
```
  - **Each row:**
    1. `json.Unmarshal` the latest version into `protocol.CanonicalEntity`. A baby's `icon` survives this only through Task 8a's `BabyPayload.Icon`; the entity decoder tolerates unknown keys and would otherwise drop it, which check 3 below would then refuse.
    2. A tombstone first encodes its previous live version (`care.Encode` with that version's own `care.Change`), then `care.Encode(tombstone, &previous, change)`.
    3. Anything else is `care.Encode(entity, nil, change)`, with `change = care.Change{Seq: sequence, ActorAccountID, DeviceID, Operation, CompletedRecordID}`.
    4. Set `row.CreatedDeviceID, row.CreatedSeq = o.device_id, o.sequence` (the first version).
    5. Accumulate `care.CounterDelta{}.Replace(nil, row)` per family.
    6. Flush every 10,000 rows with `CopyImported`.

    A family's `changed_seq` must not repeat or exceed its `next_sequence` (`ErrChangedSeqPastHigh`).
  - **Counters.** `UPDATE public.families f SET care_record_count = v.records, care_charged_bytes = v.charged, care_entity_count = v.entities, care_wire_bytes = v.wire FROM unnest($1::uuid[], $2::bigint[], $3::bigint[], $4::bigint[], $5::bigint[]) v(id, records, charged, entities, wire) WHERE f.id = v.id`. A family whose `charged` exceeds `r.o.Limits.MaxChargedStorageBytesPerFamily` is `ErrFamilyOverQuota`, naming it. Legacy charged more (journal plus receipts), so the homelab's families fit.
  - **Not carried** (counted in `Skipped`): `mutation_receipts`, `family_record_usage`, every `import_*` and `export_*` table, `delivery_pending`, every `sync_*` table.
  - **`checkCare`** returns up to 20 mismatches from these queries, each `… LIMIT 20`:
    1. For every legacy typed table, rows without a care row of the same `(family_id, id)`, or with a different:
       - kind code, `entity_version`, `tombstoned_at`, `baby_id`;
       - sequence (`changed_seq <> last_sequence`);
       - `started_at`/`ended_at`, where the table has them;
       - session column (`feeding_session_id`, `sleep_session_id` or `session_id`);
       - for babies, `sort_order`, and `icon IS DISTINCT FROM (convert_from(c.wire,'UTF8')::jsonb #>> '{payload,icon}')` on live babies;
       - `label` (`supplement_entries.name`).

       The icon field is named `icon` in the mismatch.
    2. Feeds: `breast_seconds <> sum(seconds) FILTER (kind='breast')` or `bottle_volume_ml IS DISTINCT FROM sum(volume_ml) FILTER (kind='bottle')` over `feed_components`.
    3. Every care row: `convert_from(c.wire, 'UTF8')::jsonb <> j.canonical_entity`, where `j` is the journal row at `(family_id, entity_id, sequence = changed_seq)`.
    4. Care rows with no legacy typed row whose journal version is not a tombstone (journal-only live entities).
    5. Per family and kind, `count(*)` of care rows against the legacy typed rows plus tombstoned journal-only entities.
- [ ] **Step 4: Run green:** the Step 2 command, then `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/care/... ./internal/testkit' bash Server/scripts/test-db.sh`.
- [ ] **Step 5: Commit:** `rtk git add Server/internal/care Server/internal/legacyimport Server/internal/testkit && rtk git commit -m "feat(legacyimport): load care data through care.Encode with exact sequences, origins and counters"`

### Task 17: Carry the Live Activity registrations, cards and lifecycle state [Wave F; after Tasks 14, 15 and 16]

**Files:**
- Create `Server/internal/legacyimport/live_activity_load.go` and `live_activity_load_test.go`.
- Modify `Server/internal/legacyimport/run.go`: insert `{"liveactivity", liveActivityStep}` after `care` and before `restore`.

**Interfaces:** Consumes Task 8's tables, Task 13's `importRun`, Task 15's sessions and devices, and Task 14's `liveactivity.Worker` (tests only). Produces:
```go
package legacyimport
// liveActivityStep copies main's Live Activity rows from legacy_public into
// 000032's tables (carried-data ruling): registrations, installations, the
// registration index, card tokens, token owners, the family dirty counters and
// the delivery lifecycle state, with every lease cleared. live_activity_history
// is counted in Skipped. User triggers are disabled during the load (Task 13's
// unforce step), so the derived index and owner rows are copied as stored.
func liveActivityStep(ctx context.Context, r *importRun) error
```

- [ ] **Step 1: Write the failing tests** `live_activity_load_test.go`:
```go
func importWithCards(t *testing.T) (*legacytest.Legacy, *testkit.Harness, legacyimport.Report) {
	t.Helper()
	l := legacytest.Build(t, legacytest.Spec{Families: 1, Caregivers: 1, EntitiesPerFamily: 5, Timers: true, RotationsPerDevice: 2,
		LiveActivity: true, NotCarried: true})
	closeOtherPools(l.DB)
	report := legacyimport.PipelineCommit(t, optionsFor(l), "liveactivity")
	return l, testkit.Compose(t, testkit.PostgresOf(reopen(t, l.DB)), testkit.Composition{InstanceID: l.InstanceID, RecoveryEpoch: l.Epoch,
		TokenKeys: l.TokenKeys, Now: l.Now}), report
}

func TestLiveActivityRowsAreCarriedAsTheyWere(t *testing.T) {
	l, h, report := importWithCards(t)
	card := l.Cards[0]
	var token, generation, state string
	var tokenVersion, dirty, completed int64
	var leased *string
	tx := legacytest.AuthorityTx(t, h.Migrator)
	defer tx.Rollback(context.Background())
	if err := tx.QueryRow(t.Context(), `SELECT t.token, t.generation_id::text, t.state, t.token_version, d.dirty_version, d.completed_version, d.lease_id::text
		FROM live_activity_tokens t JOIN live_activity_delivery d ON d.registration_id = t.registration_id AND d.kind = 'feed'
		WHERE t.registration_id = $1 AND t.activity_id = $2`, card.RegistrationID, card.ActivityID).
		Scan(&token, &generation, &state, &tokenVersion, &dirty, &completed, &leased); err != nil {
		t.Fatal(err)
	}
	if token != card.UpdateToken || generation != card.GenerationID || state != "active" || uint64(tokenVersion) != card.TokenVersion ||
		uint64(dirty) != card.DirtyVersion || uint64(completed) != card.CompletedVersion || leased != nil {
		t.Fatalf("carried card %s %s %s v%d dirty %d/%d lease %v", token, generation, state, tokenVersion, dirty, completed, leased)
	}
	var registrations, owners, indexed, installations, history int
	_ = tx.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM live_activity_devices), (SELECT count(*) FROM live_activity_token_owners),
		(SELECT count(*) FROM live_activity_registration_index), (SELECT count(*) FROM live_activity_installations),
		(SELECT count(*) FROM live_activity_history)`).Scan(&registrations, &owners, &indexed, &installations, &history)
	if registrations != 2 || indexed != 2 || owners != 3 || installations != 2 || history != 0 || report.Skipped["live_activity_history"] == 0 {
		t.Fatalf("registrations %d index %d owners %d installations %d history %d skipped %v", registrations, indexed, owners, installations, history, report.Skipped)
	}
	var authorized *string
	if err := h.Runtime.QueryRow(t.Context(), `SELECT app.live_activity_authorized($1, 1, false)::text`, card.RegistrationID).Scan(&authorized); err != nil || authorized == nil {
		t.Fatalf("a carried registration is not authorized under its carried session lineage: %v %v", authorized, err)
	}
}

func TestCarriedCardKeepsUpdatingWithoutReRegistration(t *testing.T) { // Review Focus 5
	l, h, _ := importWithCards(t)
	card := l.Cards[0]
	w, sent := liveActivityWorkerOn(t, h) // Task 14's worker over h.Maintenance with a recording Transport (helper copied from liveactivity's tests)
	caretest.ApplyCommitted(t, h, ownerOf(t, h, l), []protocol.Mutation{editTimer(card)}) // a care write the card shows
	drainLiveActivity(t, h, w)                                                                // Dirty.Changed + RunOnce until idle
	got := sent.to(card.UpdateToken)
	if len(got) == 0 || got[0].Event != "update" || got[0].Content.Remote.GenerationID != card.GenerationID {
		t.Fatalf("pushes to the carried card %+v", got)
	}
	if sent.count("start") != 0 {
		t.Fatalf("a push-to-start went out for a card that already runs: %+v", sent.events())
	}
}
```
  The helpers are in the test file:
  - `liveActivityWorkerOn` builds `liveactivity.NewWorker(liveactivity.NewQueue(h.Maintenance), h.Authority, rec, liveactivity.NewDirty(), slots)`.
  - `editTimer(card)` is a `timer` update mutation on `card.TimerID` with a corrected start time.
  - `ownerOf` authenticates the family owner's carried access token.
  - `drainLiveActivity` calls `w.Dirty.Changed(family, h.FamilySequence(t))` and then `w.RunOnce` until a pass sends nothing.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/legacyimport/...' BT_TEST_RUN='TestLiveActivityRowsAreCarried|TestCarriedCard' bash Server/scripts/test-db.sh`. Expected: `unknown step "liveactivity"`.
- [ ] **Step 3: Implement** `liveActivityStep`, one statement per table in FK order. The column lists are 000032's; `environment` casts `::app.apns_environment`.
```sql
INSERT INTO public.live_activity_installations SELECT instance_id, recovery_epoch, installation_id, account_id, device_id, family_id,
  version, revoked, authority_expires_at FROM legacy_public.live_activity_installations;
INSERT INTO public.live_activity_devices(registration_id, installation_id, account_id, device_id, family_id, instance_id, recovery_epoch,
  registration_version, environment, topic, push_token, payload_version, feed_break_minutes, session_id, lineage_root_session_id,
  authority_expires_at, revoked, registered_sequence, inventory_version, foreground_inventory)
SELECT registration_id, installation_id, account_id, device_id, family_id, instance_id, recovery_epoch, registration_version,
  environment::app.apns_environment, topic, push_token, payload_version, feed_break_minutes, session_id, lineage_root_session_id,
  authority_expires_at, revoked, registered_sequence, inventory_version, foreground_inventory
FROM legacy_public.live_activity_devices;                     -- membership_revision is not carried (Live Activity ruling)
INSERT INTO public.live_activity_registration_index SELECT registration_id, account_id, device_id, family_id, installation_id, instance_id,
  recovery_epoch, registration_version, authority_expires_at, revoked FROM legacy_public.live_activity_registration_index;
INSERT INTO public.live_activity_tokens(registration_id, account_id, family_id, activity_id, baby_id, kind, environment, topic, token,
  token_version, generation_id, known_generation_id, timer_id, session_id, state, token_fingerprint, server_retired)
SELECT registration_id, account_id, family_id, activity_id, baby_id, kind, environment::app.apns_environment, topic, token,
  token_version, generation_id, known_generation_id, timer_id, session_id, state, token_fingerprint, server_retired
FROM legacy_public.live_activity_tokens;
INSERT INTO public.live_activity_token_owners SELECT environment::app.apns_environment, topic, token, registration_id, slot
FROM legacy_public.live_activity_token_owners;
INSERT INTO public.live_activity_family_dirty SELECT family_id, instance_id, recovery_epoch, dirty_version, journal_sequence, authority_checked_at
FROM legacy_public.live_activity_family_dirty;
INSERT INTO public.live_activity_delivery(job_id, registration_id, registration_version, account_id, device_id, family_id, baby_id, kind,
  instance_id, recovery_epoch, dirty_version, completed_version, not_before, attempts, last_reason, target_state)
SELECT job_id, registration_id, registration_version, account_id, device_id, family_id, baby_id, kind, instance_id, recovery_epoch,
  dirty_version, completed_version, least(not_before, $1), attempts, last_reason, target_state
FROM legacy_public.live_activity_delivery;                    -- lease_id, lease_deadline, leased_* stay NULL: no lease survives the cutover
```
  `$1` is `r.now`, so nothing deferred waits past the cutover.

  **Carries checked by this task's review:**
  - **The enum cast (Task 8 review M2).** `environment::app.apns_environment` appears in all three loads that carry it: `live_activity_devices`, `live_activity_tokens` and `live_activity_token_owners`. The legacy column is `text`.
  - **The tenant context (Task 8 fix round 1).** `TestLiveActivityRowsAreCarriedAsTheyWere` calls `app.live_activity_authorized` on `h.Runtime` with no tenant context. Under `eb3d011a` an unset context is not a "different" one. If the reviewer finds otherwise, the test sets the registration's `app.account_id`/`app.family_id` first, and the test is changed, not the function.
  - **The legacy fixture.** It is at schema 29. 000029 does not touch the Live Activity tables, so the copy statements are unchanged.

  Then:
  - count every table into `Counts`;
  - count `live_activity_history` into `Skipped`;
  - `delivery_pending` is already counted (Task 16).

  Task 18's `verifyStep` then counts carried registrations that are no longer authorized, reported and not refused: the worker's sweep revokes them within a minute of the start.
- [ ] **Step 4: Run green:** the Step 2 command, then `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/legacyimport/...' bash Server/scripts/test-db.sh`.
- [ ] **Step 5: Commit:** `rtk git add Server/internal/legacyimport && rtk git commit -m "feat(legacyimport): carry Live Activity registrations, cards and lifecycle state so cards keep updating"`

### Task 18: One transaction — verification, drop, dry run, run-once, backup receipt and report [Wave G; after Task 17]

**Files:**
- Create `Server/internal/legacyimport/{verify.go,receipt.go,run_test.go}`.
- Modify `Server/internal/legacyimport/{run.go,report.go}`: `Run`, `verifyOnly`, and the steps `verify`, `drop` and `analyze` after `restore`.

**Interfaces:** Consumes everything above. Produces:
```go
package legacyimport
// Run imports one legacy install. Dry run: everything, then ROLLBACK. The
// legacy database is unchanged by any failure or by a dry run. VerifyOnly
// re-verifies an imported database and changes nothing.
func Run(ctx context.Context, o Options) (Report, error)

// BackupReceipt binds the pre-import backup to the state it was taken from
// (legacy-cutover.sh backup writes it).
type BackupReceipt struct {
	SchemaVersion   int64     `json:"schema_version"`
	SchemaName      string    `json:"schema_name"`
	SchemaSHA256    string    `json:"schema_sha256"`
	LegacyHead      HeadJSON  `json:"legacy_head"`
	DumpSHA256      string    `json:"dump_sha256"`
	DumpBytes       int64     `json:"dump_bytes"`
	LedgerTarSHA256 string    `json:"ledger_tar_sha256"`
	CreatedAt       time.Time `json:"created_at"`
}
// ReadBackupReceipt reads a 0600 regular file with exactly these fields.
func ReadBackupReceipt(path string) (*BackupReceipt, error)

// Report gains:
//   Warnings []string `json:"warnings"` — post-commit VACUUM failures, and anything that did not stop the import.
// importSteps after this task, in order:
//   lock, crosscheck, stage, unforce, authority, care, liveactivity, restore, verify, drop, analyze
```

- [ ] **Step 1: Write the failing tests** `run_test.go`:
```go
func receiptFor(t *testing.T, l *legacytest.Legacy, at time.Time) *legacyimport.BackupReceipt {
	t.Helper()
	return &legacyimport.BackupReceipt{SchemaVersion: 29, SchemaName: "000029_baby_icon.sql",
		SchemaSHA256: "8a296b40a8a1aba5e8d4f0256a8bb2aa050c50c8ea30a6d9ae2d6cfd9e087288",
		LegacyHead: legacyimport.HeadJSON{Generation: l.Head.Generation, Revision: l.Head.Revision, EventHash: hex.EncodeToString(l.Head.EventHash[:])},
		DumpSHA256: strings.Repeat("d", 64), DumpBytes: 1, LedgerTarSHA256: strings.Repeat("e", 64), CreatedAt: at}
}

func TestImportedCatalogEqualsAFreshInstall(t *testing.T) { // Review Focus 2
	fresh := testpg.New(t)
	testpg.ProductionShape(t, fresh)
	want := testpg.SnapshotCatalog(t, fresh.Owner, testpg.Roles(t, fresh))
	l := legacytest.Build(t, legacytest.Spec{Families: 2, Caregivers: 1, EntitiesPerFamily: 20, VersionsPerEntity: 2, Timers: true,
		RotationsPerDevice: 2, RemovedMember: true, NotCarried: true, LiveActivity: true})
	closeOtherPools(l.DB)
	o := optionsFor(l)
	o.BackupReceipt = receiptFor(t, l, l.Now.Add(-time.Minute))
	report, err := legacyimport.Run(t.Context(), o)
	if err != nil || !report.Committed {
		t.Fatalf("import %+v %v", report, err)
	}
	if diff := want.Diff(testpg.SnapshotCatalog(t, l.DB.Owner, testpg.Roles(t, l.DB))); len(diff) != 0 {
		t.Fatalf("an imported install is not a fresh install:\n%s", strings.Join(diff, "\n"))
	}
	// The catalog snapshot excludes schema_migrations' rows (Task 4 review M3): compare them on their own.
	history := func(pool *pgxpool.Pool) (rows []string) {
		r, err := pool.Query(t.Context(), `SELECT version||':'||name||':'||sha256 FROM schema_migrations ORDER BY version`)
		if err != nil {
			t.Fatal(err)
		}
		rows, err = pgx.CollectRows(r, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	if got, fresh := history(l.DB.Owner), history(fresh.Owner); !slices.Equal(got, fresh) {
		t.Fatalf("schema_migrations after the import %v, on a fresh install %v", got, fresh)
	}
}

func TestImportedNodeIsReady(t *testing.T) { // Review Focus 2
	l := legacytest.Build(t, legacytest.Spec{Families: 1, Caregivers: 1, EntitiesPerFamily: 5})
	closeOtherPools(l.DB)
	o := optionsFor(l)
	o.BackupReceipt = receiptFor(t, l, l.Now.Add(-time.Minute))
	if _, err := legacyimport.Run(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	db := reopen(t, l.DB)
	compatibility, _ := migrate.EmbeddedCompatibility()
	if err := database.VerifyRuntimeRole(t.Context(), db.Runtime, compatibility.Max); err != nil {
		t.Fatalf("runtime privileges after import: %v", err)
	}
	if err := database.VerifyMaintenanceRole(t.Context(), db.Runtime, db.Maintenance, compatibility.Max); err != nil {
		t.Fatalf("maintenance privileges after import: %v", err)
	}
	if v, err := migrate.New(migrations.Files).CompatibleVersion(t.Context(), db.Owner, compatibility); err != nil || v != compatibility.Max {
		t.Fatalf("compatible version %d %v", v, err)
	}
	var legacy int
	_ = db.Owner.QueryRow(t.Context(), `SELECT count(*) FROM pg_namespace WHERE nspname IN ('legacy_public','legacy_app')`).Scan(&legacy)
	if legacy != 0 {
		t.Fatal("the legacy schemas survived the commit")
	}
}

func TestDryRunChangesNothing(t *testing.T) { // Review Focus 3
	l := legacytest.Build(t, legacytest.Spec{Families: 2, Caregivers: 1, EntitiesPerFamily: 10, RotationsPerDevice: 2})
	closeOtherPools(l.DB)
	roles := testpg.Roles(t, l.DB)
	catalog := testpg.SnapshotCatalog(t, l.DB.Owner, roles)
	rows := testpg.TableChecksums(t, l.DB, "public")
	files := treeListing(t, l.LedgerDir) + treeListing(t, l.HeadDir)
	readOnly(t, l.LedgerDir, l.HeadDir)
	o := optionsFor(l)
	o.DryRun = true
	report, err := legacyimport.Run(t.Context(), o)
	if err != nil || !report.DryRun || report.Committed || report.Counts["care_entities"] == 0 {
		t.Fatalf("dry run %+v %v", report, err)
	}
	if diff := catalog.Diff(testpg.SnapshotCatalog(t, l.DB.Owner, roles)); len(diff) != 0 {
		t.Fatalf("a dry run changed the catalog:\n%s", strings.Join(diff, "\n"))
	}
	if !maps.Equal(rows, testpg.TableChecksums(t, l.DB, "public")) || files != treeListing(t, l.LedgerDir)+treeListing(t, l.HeadDir) {
		t.Fatal("a dry run changed legacy rows or files")
	}
}

func TestImportIsAllOrNothingAtEveryStep(t *testing.T) { // Review Focus 3
	l := legacytest.Build(t, legacytest.Spec{Families: 2, Caregivers: 1, EntitiesPerFamily: 10, RotationsPerDevice: 2})
	closeOtherPools(l.DB)
	roles := testpg.Roles(t, l.DB)
	catalog := testpg.SnapshotCatalog(t, l.DB.Owner, roles)
	rows := testpg.TableChecksums(t, l.DB, "public")
	boom := errors.New("injected")
	for _, step := range append(legacyimport.StepNames(), "commit") {
		o := optionsFor(l)
		o.BackupReceipt = receiptFor(t, l, l.Now.Add(-time.Minute))
		legacyimport.SetFaultForTest(&o, func(s string) error {
			if s == step {
				return boom
			}
			return nil
		})
		if _, err := legacyimport.Run(t.Context(), o); !errors.Is(err, boom) {
			t.Fatalf("fault before %s: %v", step, err)
		}
		if diff := catalog.Diff(testpg.SnapshotCatalog(t, l.DB.Owner, roles)); len(diff) != 0 {
			t.Fatalf("fault before %s left a half-migrated catalog:\n%s", step, strings.Join(diff, "\n"))
		}
		if !maps.Equal(rows, testpg.TableChecksums(t, l.DB, "public")) {
			t.Fatalf("fault before %s changed legacy rows", step)
		}
	}
	o := optionsFor(l)
	o.BackupReceipt = receiptFor(t, l, l.Now.Add(-time.Minute))
	if _, err := legacyimport.Run(t.Context(), o); err != nil {
		t.Fatalf("a clean run after every failure: %v", err)
	}
}

func TestImportRunsOnceAndVerifiesAfter(t *testing.T) {
	l := legacytest.Build(t, legacytest.Spec{Families: 1, Caregivers: 1, EntitiesPerFamily: 5})
	closeOtherPools(l.DB)
	o := optionsFor(l)
	o.BackupReceipt = receiptFor(t, l, l.Now.Add(-time.Minute))
	first, err := legacyimport.Run(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacyimport.Run(t.Context(), o); !errors.Is(err, legacyimport.ErrAlreadyImported) {
		t.Fatalf("a second run: %v", err)
	}
	o.VerifyOnly, o.BackupReceipt = true, nil
	again, err := legacyimport.Run(t.Context(), o)
	if err != nil || again.Counts["security_audit_events"] != first.Counts["security_audit_events"] || again.Committed {
		t.Fatalf("verify-only %+v %v", again, err)
	}
}

func TestImportRequiresAFreshMatchingBackupReceipt(t *testing.T) {
	l := legacytest.Build(t, legacytest.Spec{Families: 1})
	closeOtherPools(l.DB)
	for name, receipt := range map[string]*legacyimport.BackupReceipt{
		"none":       nil,
		"other head": func() *legacyimport.BackupReceipt { r := receiptFor(t, l, l.Now); r.LegacyHead.Revision--; return r }(),
		"older":      receiptFor(t, l, l.Now.Add(-25*time.Hour)),
		"other schema": func() *legacyimport.BackupReceipt { r := receiptFor(t, l, l.Now); r.SchemaVersion = 26; return r }(),
		"998501b's 28": func() *legacyimport.BackupReceipt {
			r := receiptFor(t, l, l.Now)
			r.SchemaVersion, r.SchemaName, r.SchemaSHA256 = 28, "000028_live_activity_push.sql", "43b37464fd4570ae65eb2a84e3579e7e76dbc9e6c4b4ca3d598a2a74f3bc521e"
			return r
		}(),
	} {
		o := optionsFor(l)
		o.BackupReceipt = receipt
		if _, err := legacyimport.Run(t.Context(), o); !errors.Is(err, legacyimport.ErrBackupReceipt) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
```
  `StepNames()` is exported from `export_test.go`. `readOnly` uses `legacyledger`'s test helpers' approach: it chmods trees read-only and restores them at cleanup.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/legacyimport/...' BT_TEST_RUN='TestImported|TestDryRun|TestImportIs|TestImportRuns|TestImportRequires' bash Server/scripts/test-db.sh`. Expected: `undefined: legacyimport.Run`.
- [ ] **Step 3: Implement.**
  - **`Run`:**
```go
func Run(ctx context.Context, o Options) (Report, error) {
	if o.VerifyOnly {
		return verifyOnly(ctx, o)
	}
	r, err := preflight(ctx, o) // acquires r.conn, the run's one connection (Task 13)
	if err != nil {
		return reportOf(r), err
	}
	defer r.conn.Release()
	if !o.DryRun {
		if err := r.checkBackupReceipt(); err != nil {
			return *r.report, err
		}
	}
	tx, err := r.conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return *r.report, fmt.Errorf("legacyimport: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.authority', '1', true)`); err != nil {
		return *r.report, err
	}
	r.tx = tx
	for _, s := range importSteps {
		if err := r.fault(s.name); err != nil {
			return *r.report, err
		}
		started := time.Now()
		if err := s.run(ctx, r); err != nil {
			return *r.report, fmt.Errorf("legacyimport: %s: %w", s.name, err)
		}
		r.report.Phases = append(r.report.Phases, Phase{Name: s.name, Duration: time.Since(started)})
	}
	if o.DryRun {
		r.report.DryRun = true
		return *r.report, tx.Rollback(ctx)
	}
	if err := r.fault("commit"); err != nil {
		return *r.report, err
	}
	started := time.Now()
	if err := tx.Commit(ctx); err != nil {
		return *r.report, fmt.Errorf("legacyimport: commit: %w", err)
	}
	r.report.Phases = append(r.report.Phases, Phase{Name: "commit", Duration: time.Since(started)})
	r.report.Committed = true
	r.vacuum(ctx) // VACUUM (ANALYZE) care_entities, sessions, security_audit_events; failures become Warnings
	return *r.report, nil
}
```
  - **`checkBackupReceipt`.** The receipt is required. Its schema version, name and sha256 equal the `Start`'s top row, its `LegacyHead` equals `r.verified.Head`, its digests are 64 lowercase hex, and `now − 24h ≤ CreatedAt ≤ now + 5m`. Anything else is `ErrBackupReceipt` with the field named.
  - **`verifyStep`** runs after Task 13's `restoreStep`, every check inside the transaction; any failure is `ErrVerificationFailed` with the check named:
    1. **Per-family checksums.** For every family, the legacy side is `sha256(string_agg(entity_id::text||':'||entity_version||':'||sequence, ',' ORDER BY sequence))` over the latest journal row per entity; the new side is the same over `care_entities (id, entity_version, changed_seq)`. Both are computed in one SQL statement that joins the two aggregates by family and returns the differing families.
    2. **Counters.** Each family's four counters equal the recomputation from its rows alone (no receipts or staging are carried): `octet_length(wire) + care.RowOverheadBytes` per row, plus `care.LiveReserveBytes` per live row, and the record count excluding kinds 1 and 5. The constants are passed from Go.
    3. **Audit.** `authority.VerifyChainsTx` reports no failed scope, and `Rows` equals the audit rows loaded.
    4. **Schema.** `schema_migrations` equals the embedded manifest, row for row; `schema_compatibility.current_version` is the embedded max; manifest rows exist for that version; and every relation in `r.forced` forces row security again, and no user trigger is disabled.
    5. **Server identity.** `authority.LoadServerState(tx).Check(InstanceID, RecoveryEpoch, ActorKey) == nil`.
    6. **Counts.** `Counts` gets every loaded table's `count(*)`.
    7. **Live Activity.** Every carried token, delivery and index row names a carried registration. `Counts["live_activity_unauthorized"]` is the number of non-revoked registrations for which `app.live_activity_authorized(id, version, <node entitlement policy>)` is NULL. It is called with no tenant context set (Task 8 amendment). It is reported, not refused: the worker's sweep revokes them in its first minute.
    8. **Baby icons.** No live baby's wire has `payload.icon` outside the eight values, and none has `"icon":null`. `Counts["babies_with_icon"]` is the number that carry one; the runbook compares it with R1's `babies_with_icon`.
  - **`dropStep`:** `DROP SCHEMA legacy_public CASCADE` and `DROP SCHEMA legacy_app CASCADE`. Then require that no ACL in `public`/`app` names `baby_tracker_projector`, `baby_tracker_live_activity` or `baby_tracker_apple_revoker` beyond what the current manifest grants (`SELECT count(*) FROM pg_class c, aclexplode(c.relacl) a WHERE …` against the manifest rows).
  - **`analyzeStep`:** `ANALYZE` each loaded table.
  - **`verifyOnly`.** On the run's one connection, in a read-only transaction with `app.authority` set, `Detect` must answer `StartImported`. Then it runs checks 2–6 and 8, decodes every `care_entities.wire` with `json.Unmarshal` into `protocol.CanonicalEntity` (streamed), and returns a report whose `Counts` are the table counts.
- [ ] **Step 4: Run green:** the Step 2 command, then the whole package: `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/legacyimport/... ./internal/legacyledger/...' bash Server/scripts/test-db.sh`.
- [ ] **Step 5: Commit:** `rtk git add Server/internal/legacyimport && rtk git commit -m "feat(legacyimport): one all-or-nothing transaction with verification, dry run, run-once and a backup receipt"`


### Task 19: `baby-tracker-admin import-legacy`, and `rotate-epoch` empties device cursors [Wave H; after Task 18]

**Files:**
- Create `Server/cmd/baby-tracker-admin/import_legacy.go` and `import_legacy_test.go`.
- Modify `Server/cmd/baby-tracker-admin/main.go`: dispatch `import-legacy`; the usage line.
- Modify `Server/cmd/baby-tracker-admin/main_test.go`: add `import-legacy` to `TestMigrateAndAuthorityCommandsRejectRetiredSettings`.
- Modify `Server/internal/authority/server_state.go`: `RotateRecoveryEpoch` truncates `device_sync_cursors`. Modify `server_state_test.go`.
- Modify `Server/README.md`: an "Upgrading a legacy install" section that points at `docs/operations/legacy-import.md` (Task 21) and lists the flags and environment.

**Interfaces:** Produces the command:
```
baby-tracker-admin import-legacy --ledger-dir DIR --head-dir DIR --confirm-instance UUID
    [--dry-run | --verify-only] [--backup-receipt FILE] [--report FILE]
environment: BT_DATABASE_URL[_FILE] (the migrator), BT_ACTOR_ID_PEPPER_FILE, BT_SERVER_INSTANCE_ID,
             BT_RECOVERY_EPOCH, the BT_* limits config.LoadLimits reads, optional BT_RUNTIME_DATABASE_URL_FILE
stdout, one line:
  result=<imported|dry_run|verified> start=<kind> instance_id=<uuid> epoch=<n> legacy_head=<generation>:<revision>:<hash>
  accounts=<n> families=<n> sessions=<n> care_entities=<n> care_receipts=<n> audit_rows=<n> duration_ms=<n>
  [runtime_privileges=verified]
exit 1 with stderr "import-legacy: <code>: <detail>" on any refusal or failure
```
```go
package main
func runImportLegacy(ctx context.Context, arguments []string, getenv func(string) string, output io.Writer) error
```

- [ ] **Step 1: Write the failing tests.**
  - `import_legacy_test.go`. Its fixture builds a legacy install with `legacytest.Build` on the test database. A test in `cmd/` may import `internal/legacyimport/legacytest`.
```go
func TestImportLegacyCommand(t *testing.T) {
	l := legacytest.Build(t, legacytest.Spec{Families: 1, Caregivers: 1, EntitiesPerFamily: 5})
	closeOtherPools(l.DB)
	env := importEnvironment(t, l) // BT_DATABASE_URL (owner DSN), BT_ACTOR_ID_PEPPER_FILE (0600 file holding l.ActorKey), BT_SERVER_INSTANCE_ID, BT_RECOVERY_EPOCH, BT_MODE=self_hosted
	args := []string{"import-legacy", "--ledger-dir", l.LedgerDir, "--head-dir", l.HeadDir, "--confirm-instance", l.InstanceID}
	out := runAdmin(t, env, append(args, "--dry-run")...)
	if !strings.HasPrefix(out, "result=dry_run start=homelab_29 instance_id="+l.InstanceID+" epoch=1 legacy_head=1:") {
		t.Fatalf("dry run: %q", out)
	}
	receipt := writeReceipt(t, l) // 0600 JSON for l's head, created now
	report := filepath.Join(t.TempDir(), "report.json")
	out = runAdmin(t, env, append(args, "--backup-receipt", receipt, "--report", report)...)
	if !strings.HasPrefix(out, "result=imported start=homelab_29") || !strings.Contains(out, " care_entities=") {
		t.Fatalf("import: %q", out)
	}
	if info, err := os.Stat(report); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("report file %v %v", info, err)
	}
	if err := runAdminErr(t, env, append(args, "--backup-receipt", receipt)...); err == nil || !strings.Contains(err.Error(), "already_imported") {
		t.Fatalf("a second import: %v", err)
	}
	if out := runAdmin(t, env, append(args, "--verify-only")...); !strings.HasPrefix(out, "result=verified start=imported") {
		t.Fatalf("verify-only: %q", out)
	}
}

func TestImportLegacyRefusesWithoutItsConfirmations(t *testing.T) {
	l := legacytest.Build(t, legacytest.Spec{Families: 1})
	closeOtherPools(l.DB)
	env := importEnvironment(t, l)
	for name, args := range map[string][]string{
		"wrong instance":    {"import-legacy", "--ledger-dir", l.LedgerDir, "--head-dir", l.HeadDir, "--confirm-instance", "70000000-0000-4000-8000-0000000000ee", "--dry-run"},
		"no ledger dir":     {"import-legacy", "--head-dir", l.HeadDir, "--confirm-instance", l.InstanceID, "--dry-run"},
		"no receipt":        {"import-legacy", "--ledger-dir", l.LedgerDir, "--head-dir", l.HeadDir, "--confirm-instance", l.InstanceID},
		"both modes":        {"import-legacy", "--ledger-dir", l.LedgerDir, "--head-dir", l.HeadDir, "--confirm-instance", l.InstanceID, "--dry-run", "--verify-only"},
	} {
		if err := runAdminErr(t, env, args...); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}
```
  - `authority/server_state_test.go`, `TestRotateEpochEmptiesDeviceCursors`:
    1. Seed one `device_sync_cursors` row as the maintenance pool.
    2. `RotateRecoveryEpoch(ctx, migratorPool, instance, 1)`.
    3. `SELECT count(*) FROM device_sync_cursors`, read as maintenance, is 0.
    4. A rotation refused for a stale epoch leaves the row in place.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./cmd/baby-tracker-admin ./internal/authority' BT_TEST_RUN='TestImportLegacy|TestRotateEpochEmpties|TestMigrateAndAuthorityCommandsRejectRetiredSettings' bash Server/scripts/test-db.sh`. Expected: `unknown command "import-legacy"`, and 1 cursor row left.
- [ ] **Step 3: Implement.**
  - **`runImportLegacy`:**
    1. Call `config.RejectRetiredSecuritySettings(getenv)`. The ledger paths come only from flags; `BT_SECURITY_LEDGER_DIR` stays retired.
    2. Parse the flags with `flag.NewFlagSet("import-legacy", flag.ContinueOnError)`, output discarded.
    3. Require `--ledger-dir`, `--head-dir` and `--confirm-instance` equal to `BT_SERVER_INSTANCE_ID`; `--dry-run` and `--verify-only` are exclusive; `--backup-receipt` is required unless one of them is set.
    4. Read `config.ActorIDPepper(getenv)`, the instance and epoch (as `verifyAuthority` reads them, `authority_cli.go:133`), and `config.LoadLimits(getenv)`.
    5. Open the pool with `database.OpenAndPing(ctx, config.DatabaseURL(getenv))`. Then close every idle connection but the one `Run` acquires (`pool.Reset()` after the ping), because `checkAlone` counts every other session, the migrator's own included (Task 3 carry). One connection is enough; there is no `MaxConns ≥ 2` requirement.
    6. Call `legacyimport.Run`.
    7. Write `--report` with `os.OpenFile(path, O_WRONLY|O_CREATE|O_EXCL, 0o600)` and print the line.
    8. With `BT_RUNTIME_DATABASE_URL_FILE`, after a commit, open it and run `database.VerifyRuntimeRole(ctx, runtime, compatibility.Max)`, printing `runtime_privileges=verified` or failing with `import_verification_failed: runtime privileges: …`.

    Errors print as `import-legacy: <code>: <detail>`, with the code never doubled (Task 3 carry).
    - When `err.Error()` already starts with `legacyimport.Code(err) + ":"`, the line is `import-legacy: ` + `err.Error()`. `server_running` does: its detail names each other session's role and `application_name`, sorted, with no query text or address.
    - Otherwise the line is `import-legacy: ` + code + `: ` + `err.Error()`.

    `TestImportLegacyRefusesWithoutItsConfirmations` gains a `server_running` case: a second connection held open to the test database. Stderr must start `import-legacy: server_running: ` and must contain `server_running` exactly once.
  - **`RotateRecoveryEpoch`:** after the `UPDATE public.server_state`, the same transaction runs `TRUNCATE public.device_sync_cursors`. The migrator owns it, and TRUNCATE is not subject to its RLS. Every recorded cursor names the old epoch, so the wake backstop rebuilds the rows (Phase 3 carry). Update its doc comment.
  - **README.** Add a section with the command, its environment, "run with the server stopped", and a link to the runbook.
- [ ] **Step 4: Run green:** the Step 2 command, then `rtk make server-build` and `(cd Server && rtk go vet ./... && rtk go vet -tags identitytest ./...)`.
- [ ] **Step 5: Commit:** `rtk git add Server/cmd/baby-tracker-admin Server/internal/authority Server/README.md && rtk git commit -m "feat(admin): baby-tracker-admin import-legacy; rotate-epoch empties device cursors"`


### Task 20: The captured homelab-shaped fixture end to end — signed in, pulling, golden, icons [Wave I; after Tasks 6 (recaptured as `homelab29`) and 19; parallel with Tasks 21 and 22]

**Files:**
- Create `Server/internal/legacyimport/legacytest/restore.go` and `Server/internal/legacyimport/fixture_test.go`.

**Interfaces:** Consumes the Task 6 fixture `homelab29` (captured at `fce422d5`, schema 29), `Run`, `testkit.Compose` and `config.LoadAuth`. Produces:
```go
package legacytest
// Fixture is a captured legacy install restored into a disposable database.
type Fixture struct {
	Legacy                         // DB, LedgerDir, HeadDir, InstanceID, Epoch, ActorKey, TokenKeys, Now (= CapturedAt + 2 minutes)
	Name     string
	Client   CapturedClient        // client.json (captured.go, Task 6)
	Env      map[string]string     // env.json, filtered (below), with secret paths resolved into the extracted secrets directory
	Manifest map[string]any
}
// Keyring converts a loaded keyring config as cmd/baby-tracker-server's composeDigestKeyring does (main.go:623).
func Keyring(c config.DigestKeyringConfig) identity.DigestKeyring
// Restore builds the fixture's schema (legacytest.NewLegacy: the homelab's 29),
// applies ProductionShape, replays data.sql.gz as a superuser with
// session_replication_role = replica in one transaction (the seeded-row rule
// below), and extracts ledger.tar.gz and secrets.tar.gz into t.TempDir()
// (directories 0700, files 0600). It closes its superuser pool before returning.
func Restore(t *testing.T, name string) *Fixture
// FilterEnv drops what the new server must not be configured with:
// BT_LIVE_ACTIVITY_DATABASE_URL and BT_LIVE_ACTIVITY_DATABASE_URL_FILE (refused by
// config.LoadLiveActivity, Task 7), BT_APNS_KEY_FILE (the throwaway key is not
// captured), and every *_DATABASE_URL / *_DATABASE_URL_FILE (the fixture's
// database URLs point at the capture's containers). Restore applies it; Task 21's
// rehearsal uses the same list.
func FilterEnv(env map[string]string) map[string]string
```

- [ ] **Step 1: Write the failing tests** `fixture_test.go`. Each test runs on the one fixture, `homelab29`.
```go
func importFixture(t *testing.T, name string) (*legacytest.Fixture, *testkit.Harness, legacyimport.Report) {
	t.Helper()
	f := legacytest.Restore(t, name)
	closeOtherPools(f.DB)
	o := optionsFor(&f.Legacy)
	o.BackupReceipt = receiptFor(t, &f.Legacy, f.Now.Add(-time.Minute))
	report, err := legacyimport.Run(t.Context(), o)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	authConfig, err := config.LoadAuth(mapEnv(f.Env), config.ModeSelfHosted)
	if err != nil {
		t.Fatal(err)
	}
	k := legacytest.Keyring // config.DigestKeyringConfig -> identity.DigestKeyring, copied from cmd/baby-tracker-server composeDigestKeyring
	h := testkit.Compose(t, testkit.PostgresOf(reopen(t, f.DB)), testkit.Composition{InstanceID: f.InstanceID, RecoveryEpoch: f.Epoch,
		TokenKeys: k(authConfig.SessionTokenKeys), IdentityKeys: k(authConfig.IdentityKeys), ContinuityKeys: k(authConfig.DeviceContinuityKeys),
		InvitationKeys: k(authConfig.InvitationKeys), PageKeys: k(authConfig.PageTokenKeys), Origin: f.Env["BT_PUBLIC_ORIGIN"], Now: f.Now})
	return f, h, report
}

func TestCapturedPhonesStaySignedIn(t *testing.T) {
	for _, name := range []string{"homelab29"} {
		t.Run(name, func(t *testing.T) {
			f, h, _ := importFixture(t, name)
			for subject, s := range f.Client.Subjects {
				p, err := h.Auth.Authenticate(t.Context(), s.AccessToken)
				if subject == "removed" {
					if auth.Code(err) != "session_revoked" {
						t.Fatalf("the removed member: %v", err)
					}
					continue
				}
				if err != nil || p.AccountID != s.AccountID || p.DeviceID != s.DeviceID {
					t.Fatalf("%s: %+v %v", subject, p, err)
				}
				// The outsider scheduled its family's deletion: its re-signed-in session is
				// family_recovery-scoped and is carried as stored (Task 6 review).
				if subject == "outsider" && p.SessionScope != "family_recovery" {
					t.Fatalf("the outsider's carried session scope is %q", p.SessionScope)
				}
				if _, err := h.Auth.Refresh(t.Context(), s.RefreshToken); err != nil {
					t.Fatalf("%s refreshes after the cutover: %v", subject, err)
				}
			}
		})
	}
}

func TestCapturedPhonesKeepPulling(t *testing.T) { // Review Focus 1, on real legacy data
	for _, name := range []string{"homelab29"} {
		t.Run(name, func(t *testing.T) {
			f, h, _ := importFixture(t, name)
			family := f.Client.Families["main"]
			for _, subject := range []string{"owner", "caregiver", "viewer"} {
				s := f.Client.Subjects[subject]
				p, err := h.Auth.Authenticate(t.Context(), s.AccessToken)
				if err != nil {
					t.Fatal(err)
				}
				for _, c := range []struct {
					name   string
					cursor uint64
					legacy []json.RawMessage
				}{{"from zero", 0, s.PullFromZero}, {"from mid", family.MidCursor, s.PullFromMid}} {
					got := pullAllRaw(t, h, p, h.CursorAt(c.cursor)) // every change, all pages; snapshot_required fails the test
					want := testkit.LatestPerEntity(t, c.legacy)    // changes: keyed by their entity, latest sequence wins
					if !equalByEntity(got, want) { // same entity IDs, byte-identical wire per entity
						t.Fatalf("%s %s: the imported pull differs from the legacy latest states", subject, c.name)
					}
				}
				if rest := pullAllRaw(t, h, p, s.Cursor); len(rest) != 0 {
					t.Fatalf("%s: a pull at the legacy cursor returned %d changes", subject, len(rest))
				}
				// Snapshot entities are not changes: they carry no sequence, and LatestPerEntity fatals on them.
				// byMetaID keys them by meta.id (Task 6 review).
				if !equalByEntity(snapshotAllRaw(t, h, p), byMetaID(t, s.SnapshotEntities)) {
					t.Fatalf("%s: the snapshot differs", subject)
				}
			}
		})
	}
}

func TestCapturedBabyIconsSurvive(t *testing.T) { // Review Focus 6, on real legacy data
	f, h, _ := importFixture(t, "homelab29")
	for _, subject := range []string{"owner", "caregiver", "viewer"} {
		p, _ := h.Auth.Authenticate(t.Context(), f.Client.Subjects[subject].AccessToken)
		pulled := byEntityID(pullAllRaw(t, h, p, h.CursorAt(0)))
		snap := byEntityID(snapshotAllRaw(t, h, p))
		for source, entities := range map[string]map[string]rawEntity{"pull": pulled, "snapshot": snap} {
			icon := entities[f.Client.IconBabyID]
			if !bytes.Contains(icon.Payload, []byte(`"icon":"girl.2"`)) || !bytes.Contains(icon.Payload, []byte(`"name":"Icon baby K"`)) {
				t.Fatalf("%s %s: the icon baby is %s", subject, source, icon.Payload)
			}
			for id, e := range entities {
				if e.Kind == "baby" && id != f.Client.IconBabyID && bytes.Contains(e.Payload, []byte(`"icon"`)) {
					t.Fatalf("%s %s: baby %s gained an icon key: %s", subject, source, id, e.Payload)
				}
			}
		}
	}
	// After the cutover, a fce422d5 phone sets an icon on the other baby and an older-shaped rename keeps it.
	owner, _ := h.Auth.Authenticate(t.Context(), f.Client.Subjects["owner"].AccessToken)
	other := goldenBabyID(t, f) // the golden scenario's baby
	setIcon := caretest.ApplyCommitted(t, h, owner, []protocol.Mutation{babyUpdate(t, h, other, "boy.1")})
	renamed := caretest.ApplyCommitted(t, h, owner, []protocol.Mutation{babyRenameWithoutIcon(t, h, other, "Renamed")})
	for _, o := range []protocol.Outcome{setIcon.Outcomes[0], renamed.Outcomes[0]} {
		if got := o.CanonicalEntity.Payload.(protocol.BabyPayload); o.Status != "applied" || got.Icon == nil || *got.Icon != "boy.1" {
			t.Fatalf("icon after the cutover: %+v", o)
		}
	}
}

func TestCapturedCareMatchesTheGoldenCorpus(t *testing.T) {
	f, h, _ := importFixture(t, "homelab29")
	owner := f.Client.Subjects["owner"]
	p, _ := h.Auth.Authenticate(t.Context(), owner.AccessToken)
	// The corpus has no icon baby: the capture added it after the golden replay (Task 6 amendment, step 4a).
	got := maskServerTimes(h.Normalize(marshal(withoutEntity(pullAllRaw(t, h, p, h.CursorAt(0)), f.Client.IconBabyID))))
	want := maskServerTimes(goldenLatest(t, "../sync/testdata/golden/legacy-sync.json")) // LatestPerEntity over the corpus's pulled changes
	if !bytes.Equal(got, want) {
		t.Fatalf("the imported golden family differs from the corpus:\n%s", diffJSON(got, want))
	}
	// familyread and export render the imported family as the corpus recorded, under the Phase 2
	// normalizations (familyread care_reads_test.go:75, export care_source_test.go:122), the same time mask,
	// and without the icon baby.
	assertFamilyreadMatchesCorpus(t, h, p, f.Client.IconBabyID)
	assertExportMatchesCorpus(t, h, p, f.Client.IconBabyID)
}

func TestCapturedCardsKeepUpdating(t *testing.T) { // Review Focus 5, on real legacy data
	f, h, _ := importFixture(t, "homelab29")
	card := f.Client.LiveActivity.Cards[0]
	w, sent := liveActivityWorkerOn(t, h) // Task 17's helper
	caregiver, err := h.Auth.Authenticate(t.Context(), f.Client.Subjects[card.Subject].AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	caretest.ApplyCommitted(t, h, caregiver, []protocol.Mutation{editRunningTimer(t, h, *card.TimerID)}) // the captured card's timer, edited
	drainLiveActivity(t, h, w)
	got := sent.to(card.UpdateToken)
	// Scoped to the captured card: the owner also holds a startUnconfirmed sleep start from the
	// capture, whose re-evaluation may legitimately push-to-start (Task 6 review).
	if len(got) == 0 || got[0].Event != "update" || got[0].Content.Remote.GenerationID != card.GenerationID || sent.startsFor(*card.TimerID) != 0 {
		t.Fatalf("the captured card after the cutover: %+v (all: %+v)", got, sent.events())
	}
}
```
  The test helpers live in `fixture_test.go`:
  - **`pullAllRaw`, `snapshotAllRaw`, `rawEntity`, `byEntityID`, `byMetaID`, `withoutEntity`, `equalByEntity`.** `byMetaID` keys raw snapshot entities by `meta.id`, and fails on a duplicate ID.
  - **`maskServerTimes`** replaces every `accepted_at`, `tombstoned_at` and server-stamped `*_at` value with `"{{server_time}}"`. The corpus was recorded on a fixed test clock, the capture on the disposable server's clock; every client-supplied time stays compared.
  - **`goldenLatest`** reads the corpus file the way `sync/golden_test.go:51` does, and keeps each entity's last change.
  - **The mutation builders:** `editRunningTimer`, `goldenBabyID`, `babyUpdate` and `babyRenameWithoutIcon`. The last builds a `protocol.BabyPayload` with `Icon` nil.
  - **`sent.startsFor(timerID)`** counts `start` pushes whose content state names that timer.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/legacyimport/...' BT_TEST_RUN='TestCaptured' bash Server/scripts/test-db.sh`. Expected: `undefined: legacytest.Restore`.
- [ ] **Step 3: Implement** `Restore` and the helpers.
  - **Restore.** `data.sql.gz` is plain `--column-inserts` SQL, with the `\restrict` lines already stripped at capture. The dump also holds the rows the vendored migrations seed: `schema_privilege_manifest`, `schema_compatibility` and `security_projection_head`. The Task 6 review ruled that the collision is resolved here, not at capture. One transaction on the superuser pool:
    1. `BEGIN; SET LOCAL session_replication_role = replica;`
    2. Snapshot `schema_privilege_manifest` (every row, ordered).
    3. `DELETE FROM public.schema_privilege_manifest; DELETE FROM public.schema_compatibility; DELETE FROM public.security_projection_head;`
    4. Replay the dump through one `pgconn` simple-protocol `Exec`.
    5. Assert the projection head (`generation`, `revision`, `event_hash`) equals `manifest.json`'s `ledger_head`. Assert the privilege manifest equals the snapshot from 2, row for row: the capture ran the same 29 migrations. Assert `schema_compatibility.current_version` is 29.
    6. `COMMIT`. Any failed assertion fails the test with the table named.
  - **Env.** `Restore` sets `Fixture.Env = FilterEnv(env.json)`, so nothing downstream ever sees the refused Live Activity database variables (Task 6 review).
  - **Keys.** `CapturedAt + 2 minutes` keeps the captured access tokens inside their lifetime; `optionsFor` reads the actor key from the extracted `actor_id_pepper` file named by `env.json`.
  - **A difference is a finding, not a test edit.** When a comparison fails on a field, find the cause first. If it is an intended format difference already ruled in Phase 2 (`.250Z` running-timer timestamps, latest state only), apply that ruling in the comparison helper and cite it in a comment. Any other difference is an importer defect: fix the importer. An icon difference is never a Phase 2 format ruling.
- [ ] **Step 4: Run green:** the Step 2 command. Record the fixture's `Report.Phases` totals and `Counts["babies_with_icon"]` (1) in the task report.
- [ ] **Step 5: Commit:** `rtk git add Server/internal/legacyimport && rtk git commit -m "test(legacyimport): the captured homelab stays signed in, keeps pulling, keeps its baby icons and matches the golden corpus after import"`



### Task 21: The cutover tooling, the push overlay, the runbook and the rehearsal [Wave I; after Tasks 1, 6 and 19; parallel with Tasks 20 and 22]

**Files:**
- Create `Server/scripts/legacy-cutover.sh`, `Server/scripts/rehearse-legacy-import.sh` and `Server/scripts/legacy_cutover_test.go`.
- Create `Server/deploy/unraid/push.yaml`, the Unraid push overlay that replaces `main`'s `live-activity-push.yaml`:
```yaml
# Push for the homelab: Live Activity (on) and background wakes (owner's choice at the cutover).
# One APNs key, the operator's, mounted read-only; no Live Activity worker login (the worker runs on the
# maintenance credential).
services:
  app:
    environment:
      BT_LIVE_ACTIVITY_PUSH_ENABLED: "true"
      BT_APNS_WAKES_ENABLED: ${BT_APNS_WAKES_ENABLED:-false}
      BT_APNS_TEAM_ID: ${BT_APNS_TEAM_ID:?set BT_APNS_TEAM_ID}
      BT_APNS_KEY_ID: ${BT_APNS_KEY_ID:?set BT_APNS_KEY_ID}
      BT_APNS_BUNDLE_ID: ${BT_APNS_BUNDLE_ID:?set BT_APNS_BUNDLE_ID}
      BT_APNS_KEY_FILE: /run/secrets/apns_key
    secrets:
      - apns_key
secrets:
  apns_key:
    file: ${BT_DEPLOY_ROOT:?set BT_DEPLOY_ROOT}/secrets/apns_key.p8
```
- Create `docs/operations/legacy-import.md`, the runbook.
- Modify `Server/deploy/unraid/compose.yaml`: a comment over the `security_ledger`/`ledger_head` volumes saying the importer mounts them read-only, R7 deletes the homelab's directories, and Task 26 removes the volumes.
- Modify `Server/scripts/unraid_deployment_test.go`: `push.yaml` sets neither `BT_LIVE_ACTIVITY_DATABASE_URL` nor `BT_LIVE_ACTIVITY_DATABASE_URL_FILE` (the new server refuses either at startup, Task 7), mounts only `apns_key`, and has no `live_activity_*` secret.

**Interfaces:** `legacy-cutover.sh <subcommand>`. Environment:
- `BT_DEPLOY_ROOT` (default `/mnt/user/appdata/baby-tracker-sync`);
- `BT_COMPOSE_PROJECT` (default `baby-tracker-sync`);
- `BT_COMPOSE_FILES`, the new composition: default `-f compose.yaml -f deploy/unraid/compose.yaml -f deploy/unraid/push.yaml`, relative to the staged `Server/` tree;
- `BT_LEGACY_COMPOSE_FILES`, the homelab's composition today (preflight facts, rollout record): default `-f compose.yaml -f deploy/unraid/compose.yaml -f deploy/unraid/live-activity-push.yaml`, relative to the legacy (`fce422d5`) source tree. It is used to stop the legacy app (R4), for the read-only `docker exec` steps before R5, and by `rollback`;
- `BT_BACKUP_DIR` (required except for `preflight`);
- `BT_CUTOVER_APPROVED`.

| Subcommand | Writes | Refuses when |
|---|---|---|
| `preflight` | nothing; Task 1's read-only checks, run locally on the host | — |
| `backup` | `$BT_BACKUP_DIR/{legacy.dump,ledger.tar,receipt.json}`, mode 0600, directory 0700 | the app container is running; free disk is below 3 × `pg_database_size`; the target is the homelab without `BT_CUTOVER_APPROVED` |
| `dry-run` | `$BT_BACKUP_DIR/dry-run-report.json` | no receipt; the app is running; the homelab without approval |
| `import` | `$BT_BACKUP_DIR/import-report.json` | a receipt older than 24 hours or for another head; no passing dry-run report newer than the receipt; the homelab without approval |
| `verify` | nothing | — (it starts the app only after `--verify-only` passes) |
| `drop-legacy-roles` | the cluster roles (C2, C4) | no `import-report.json` with `"committed": true`; `verify` has not passed; the homelab without approval |
| `delete-ledger` | removes `security-ledger` and `ledger-head` (C3) | `ledger.tar` missing, or its sha256 not the receipt's; `verify` has not passed; the homelab without approval |
| `rollback` | the database from `legacy.dump`, and the ledger directories from `ledger.tar` | no receipt; the legacy source (`$BT_DEPLOY_ROOT/source-rollback-*`) is missing; the homelab without approval |

There is no subcommand for in-flight requests (D3); R4 has the owner force-quit both apps before the server stops.

**What each subcommand runs:**
- **`preflight`:** Task 1's queries through `docker exec`. Its last line is the `preflight:` summary. It is a STOP unless the line has:
  - `schema=29 digests=match projection=equal mode=self_hosted`;
  - `source=fce422d51ef4fbd4ac98a4bab4e4375ff6e762a6` and `image_revision=` the same;
  - `apns=set`;
  - `compose=` the three legacy files.

  The expected digests come from the vendored manifest, never a literal.
- **`dry-run`/`import` refused with `server_running`** (the ledger's Task 3 carry). The db container's health check briefly opens a superuser session, which `checkAlone` can see. The subcommand reruns once after 5 seconds and prints `rerun=1`. A second `server_running` is a STOP, and its detail (each session's role and `application_name`) is printed for the owner.
- **`backup`:**
  1. `docker exec <db> pg_dump -Fc -U postgres -d babytracker > legacy.dump`, then `pg_restore --list legacy.dump > /dev/null`.
  2. `tar -C "$BT_DEPLOY_ROOT" -cf ledger.tar security-ledger ledger-head`.
  3. sha256 of both, then `receipt.json` with the `BackupReceipt` fields (`jq -n`).
- **`dry-run` / `import`:** `docker compose -p "$BT_COMPOSE_PROJECT" $BT_COMPOSE_FILES run --rm --no-deps -v "$BT_DEPLOY_ROOT/security-ledger:/legacy/ledger:ro" -v "$BT_DEPLOY_ROOT/ledger-head:/legacy/head:ro" -v "$BT_BACKUP_DIR:/legacy/backup" migrate import-legacy --ledger-dir /legacy/ledger --head-dir /legacy/head --confirm-instance "$instance" --backup-receipt /legacy/backup/receipt.json --report /legacy/backup/<name>-report.json [--dry-run]`.
- **`verify`:**
  1. `import-legacy --verify-only`;
  2. `baby-tracker-admin authority verify` (`failed=0`);
  3. `docker compose … up -d --wait app`;
  4. `verify-unraid-deployment.sh verify`;
  5. `/readyz` returns 200, and `/v1/server` advertises `live-activity-push-v1` (and `push-token-v1` when wakes are on).
  6. `import-report.json`'s `counts.babies_with_icon` equals R1's `babies_with_icon`, read from the preflight output saved in `$BT_BACKUP_DIR`.

  Before step 3, `verify` refuses to start the app in either of these cases:
  - `deployment.env` sets `BT_LIVE_ACTIVITY_DATABASE_URL` or `BT_LIVE_ACTIVITY_DATABASE_URL_FILE`;
  - `$BT_COMPOSE_FILES` names `live-activity-push.yaml`.

  The legacy overlay leaves the composition at R5. The new server would refuse the variable, and the overlay's worker credential is gone.
- **`drop-legacy-roles`** (as `postgres` in the db container):
  1. `SELECT rolname FROM pg_roles WHERE rolname IN (…)` lists what exists.
  2. `SELECT count(*) FROM pg_shdepend d JOIN pg_roles r ON r.oid = d.refobjid WHERE r.rolname IN (…) AND d.dbid = (SELECT oid FROM pg_database WHERE datname = 'babytracker')` must be 0 after the import. That is every object inside the database. If it is not, STOP and print the dependent object classes. Shared dependencies (`dbid = 0`: database-level grants) are listed, and step 3 revokes them.
  3. One transaction drops the roles. For each role that holds a database-level privilege, it first runs `REVOKE ALL ON DATABASE babytracker FROM <role>`. The catalog snapshot does not cover database grants or role memberships (Task 4 review M6). It then drops each login before its group.
  - The roles: `baby_tracker_projector_app`, `baby_tracker_projector`, `baby_tracker_apple_revoker_app`, `baby_tracker_apple_revoker` (C2); with `--with-live-activity` (C4), also `baby_tracker_live_activity_app` and `baby_tracker_live_activity`.
  - Then it removes the now unused secret files `projector_password`, `apple_revocation_password` and (C4) `live_activity_password`/`live_activity_database_url`, after copying them into `$BT_BACKUP_DIR/retired-secrets/` (mode 0600).
- **`delete-ledger`:**
  1. `tar -df ledger.tar -C "$BT_DEPLOY_ROOT"`: the directories still equal the backup (they were mounted read-only).
  2. `rm -rf -- "$BT_DEPLOY_ROOT/security-ledger" "$BT_DEPLOY_ROOT/ledger-head"`.
  3. `ledger.tar` stays in `$BT_BACKUP_DIR` with the dump.
- **`rollback`.** The legacy state is `fce422d5` at schema 29, which the dump holds.
  1. Stop the app.
  2. `dropdb` and `createdb -O baby_tracker_migrator` as `postgres`, then `pg_restore --single-transaction --exit-on-error -d babytracker legacy.dump`.
  3. If R6 ran, recreate the dropped roles and logins first, with `fce422d5`'s `init-roles.sql`/`init-users.sh` statements for them and the passwords from `retired-secrets/`.
  4. Restore the ledger directories from `ledger.tar` if R7 removed them.
  5. Restore `source-rollback-*` (the `fce422d5` tree R2 set aside, which still holds `deploy/unraid/live-activity-push.yaml`) and `deployment.env-rollback-*`.
  6. `compose $BT_LEGACY_COMPOSE_FILES build app migrate`, then `compose $BT_LEGACY_COMPOSE_FILES up -d --wait app`.
  7. The legacy `/readyz` is 200, and `schema_compatibility.current_version` is 29.

  The phones run V10, which the legacy `fce422d5` server serves, so they need nothing.

**`rehearse-legacy-import.sh`:**
- `--synthetic [--legacy-sha SHA]` (default `fce422d51ef4fbd4ac98a4bab4e4375ff6e762a6`, the homelab's source):
  1. Start a disposable legacy stack with `main`'s Live Activity profile, as Task 6 does. Seed it with the capture test, cards and the icon baby included.
  2. `docker cp` its ledger volumes into a temp deploy root.
  3. Run `legacy-cutover.sh backup`, `dry-run`, `import`, `verify`, `drop-legacy-roles --with-live-activity` and `delete-ledger` against the disposable project, with this worktree's image and `push.yaml` over the identitytest sink.
     - The new app's environment is the legacy stack's, passed through the same filter as `legacytest.FilterEnv` (Task 20): no `BT_LIVE_ACTIVITY_DATABASE_URL[_FILE]`, no legacy database-URL files, and `BT_APNS_KEY_FILE` from the disposable push profile.
     - `$BT_COMPOSE_FILES` uses `push.yaml`; `$BT_LEGACY_COMPOSE_FILES` uses the legacy stack's `live-activity-push.yaml`.
  4. Prove that the captured tokens still pull over HTTPS through the new app, and that the icon baby comes back with `"icon":"girl.2"`.
  5. Prove that one edit of the captured card's timer produces an `update` to its carried token in the recorder.
  6. Run `rollback` and prove the legacy app is ready again, with the same ledger head and schema 29.
  7. Remove every container, volume and worktree it created.

  It prints `rehearsal proof: imported=<n entities> verified=1 pulled=<n> icons=1 card_updated=1 roles_dropped=<n> ledger_deleted=1 rolled_back=1 legacy_ready=1 cleanup=0/0`.
- `--dump FILE --ledger-tar FILE` does the same from a given backup (D4, asked at execution time). It refuses unless `BT_COMPOSE_PROJECT` starts with `rehearsal-`.

- [ ] **Step 1: Write the failing test** `legacy_cutover_test.go`. It runs the scripts with a `PATH` shim directory whose `docker`, `jq`, `pg_restore`, `tar`, `rm` and `df` are small recording scripts written by the test. Cases:
  - `backup` with a running app refuses.
  - `backup` against instance `<instance-uuid>` refuses without `BT_CUTOVER_APPROVED`, and proceeds with it equal to the instance.
  - `backup` writes `receipt.json` with mode 0600 and the eight fields.
  - `dry-run`'s recorded command line contains `:/legacy/ledger:ro`, `:/legacy/head:ro`, `--dry-run` and `-f deploy/unraid/push.yaml`, and never `live-activity-push.yaml`.
  - `dry-run` answered `server_running` once reruns once and passes; answered twice, it stops with exit 1 and prints the detail.
  - `import` refuses with no dry-run report, or with a receipt older than 24 hours.
  - `verify` refuses to start the app when `deployment.env` holds `BT_LIVE_ACTIVITY_DATABASE_URL_FILE=…`, or when `BT_COMPOSE_FILES` names `live-activity-push.yaml`.
  - `preflight` treats `schema=28` (the `998501b` state) as a STOP.
  - `rehearse-legacy-import.sh --synthetic`'s default legacy SHA is `fce422d51ef4fbd4ac98a4bab4e4375ff6e762a6`, and its environment filter drops the three variable families above.
  - `drop-legacy-roles` refuses before a committed import report. With one, it runs `DROP ROLE` only after the `pg_shdepend` count query, and only inside one `BEGIN`/`COMMIT`. It revokes database privileges first and drops logins before groups, and it never names a role outside its list.
  - `delete-ledger` refuses when `tar -df` reports a difference, or when the tar's sha256 is not the receipt's; its `rm` names exactly the two directories under `$BT_DEPLOY_ROOT`.
  - `rollback` refuses without `source-rollback-*`, and restores the ledger from `ledger.tar` when the directories are missing.
  - `rehearse-legacy-import.sh --dump x --ledger-tar y` refuses `BT_COMPOSE_PROJECT=baby-tracker-sync`.
  - Every script has `set -euo pipefail` and an EXIT trap that cleans up its temporary directories.
- [ ] **Step 2: Run to see the failure:** `rtk go -C Server test ./scripts -run 'TestLegacyCutover|TestUnraid' -count=1`. Expected: the scripts and `push.yaml` are missing.
- [ ] **Step 3: Implement** the scripts and the overlay. Then write `docs/operations/legacy-import.md`. Each step has its exact command, its expected output line and its STOP condition. **Each step that touches the homelab starts only after the owner says yes to that step in the conversation.**
  - **R0 Approvals and choices.** One approval per homelab step; nothing runs from a plan or a test.
    - Ask D4 (where the rehearsal runs), D5 (the window), C4 (also drop the Live Activity worker role and its secrets), C5 (background wakes on at the cutover) and C6 (when to install a merged app build, which must be at or after Task 23's merge: V10).
    - Ask the owner to hold any other homelab deploy until the cutover is confirmed (D2 re-pin rule).
  - **R1 Preflight.** `homelab-preflight.sh`, read-only. STOP unless the summary has:
    - `schema=29 digests=match projection=equal mode=self_hosted`;
    - `source=fce422d51ef4fbd4ac98a4bab4e4375ff6e762a6`, with `image_revision=` the same;
    - `apns=set app=running`;
    - `compose=` the three files of the preflight facts (`compose.yaml`, `deploy/unraid/compose.yaml`, `deploy/unraid/live-activity-push.yaml` under the staged tree).

    Save the output to `$BT_BACKUP_DIR/preflight.txt` (mode 0600) for `verify`. Record `babies_live` and `babies_with_icon` (expected 2 and 1). Any other `source` or schema is a STOP and the D2 re-pin rule: no cutover step runs against a state the plan does not pin.
  - **R2 Stage the source and the overlay.**
    - As `main:docs/verification/2026-10-09-baby-icons-rollout.md` did: `git archive` of the approved merged SHA, with its SHA-256 checked after upload. Keep `source.fce422d-rollback-<merged sha>` and `deployment.env.fce422d-rollback-<merged sha>`. The existing `source.998501b-rollback-fce422d` set stays where it is.
    - Run `provision-unraid-secrets.sh` with `BT_ALLOW_SOURCE_UPDATE=1`; it adds the maintenance credential (Phase 3 carry). Then `compose $BT_COMPOSE_FILES config -q`.
    - Set `BT_APNS_WAKES_ENABLED` in `deployment.env` per C5. Check that `deployment.env` sets no `BT_LIVE_ACTIVITY_DATABASE_URL` or `BT_LIVE_ACTIVITY_DATABASE_URL_FILE`; the new server refuses either, so remove the line if present (it is kept in the rollback copy). The existing `BT_APNS_TEAM_ID`, `BT_APNS_KEY_ID`, `BT_APNS_BUNDLE_ID` and `secrets/apns_key.p8` stay as they are.
    - Build the image. Run `apns-preflight` inside it with the homelab's key, once per push type, because the homelab key must also cover the Live Activity topic (Task 7):
      - `baby-tracker-server apns-preflight --environment sandbox --push-type background`;
      - `baby-tracker-server apns-preflight --environment sandbox --push-type liveactivity`;
      - both again with `--environment production` if the phones use TestFlight builds.

      STOP unless every line reads `outcome=pass`.
  - **R3 Rehearse,** where D4 says, with `--dump` on a copy of tonight's backup.
  - **R4 Stop the phones, then the server (D3).**
    - The coordinator asks: "Ready to migrate? Please open the app on both phones, check each shows no pending changes (the sync indicator is idle), then force-quit both apps, and tell me when done."
    - Only after the owner confirms, from the legacy (`fce422d5`) source tree: `docker compose -p baby-tracker-sync $BT_LEGACY_COMPOSE_FILES stop app`.
  - **R5 Cutover.**
    1. `backup`.
    2. `dry-run`: STOP unless `result=dry_run start=homelab_29`. A first `server_running` is rerun once, automatically; a second is a STOP.
    3. `import`: STOP unless `result=imported start=homelab_29`.
    4. `verify`: STOP unless `/readyz` is 200, `authority verify` reports `failed=0`, `live-activity-push-v1` is advertised, and `babies_with_icon` matches R1.

    The new app starts only with `$BT_COMPOSE_FILES` (`push.yaml`). `main`'s `live-activity-push.yaml` leaves the composition here; its copy stays in the rollback source for R9.
  - **R6 Drop the legacy roles (C2, C4).** `drop-legacy-roles` (`--with-live-activity` if C4 is yes), as the one-time superuser step. STOP if the dependency count is not 0.
  - **R7 Delete the ledger directories (C3).** `delete-ledger`. The dump and `ledger.tar` stay in the backup directory.
  - **R8 Phones.**
    - The owner opens the app on both phones. Both pull without a snapshot (no `snapshot_required` for their devices in the server log), and both still show the family and its history.
    - A running feed or sleep card on a phone updates within a minute of a change made on the other phone, with no new card appearing.
    - **Icons.** Both phones show the same icons as before: the baby with `boy.1` keeps it, and the other keeps the neutral face. A rename on one phone keeps the icon on the other. The server log shows no `invalid_request` or `invalid_mutation` for a baby. These are the rollout record's open checks, repeated on the new server.
    - `device_sync_cursors` rows reappear within 2 minutes.
  - **R9 Rollback,** only if a STOP fires after R5's commit. It returns the homelab to `fce422d5` at schema 29 with `live-activity-push.yaml`, as it was at R1.
  - **R10 Confirmation.** The owner confirms the cutover in the conversation. The coordinator records it in `docs/verification/<date>-homelab-cutover.md`, which unlocks Task 29. The backup (dump, ledger tar, receipt, reports and retired secrets) is kept 30 days.

  The doc also states the duration targets: about 15 minutes of window for the homelab, with the import itself under 10 seconds (P), and Task 22's measured numbers.
- [ ] **Step 4: Run green:** the Step 2 command; `rtk bash Server/scripts/rehearse-legacy-import.sh --synthetic`, once, with `df` at least 10 GB and no other `test-db.sh` or E2E running. Expected: exit 0 and the `rehearsal proof:` line, pasted into the report.
- [ ] **Step 5: Commit:** `rtk git add Server/scripts/legacy-cutover.sh Server/scripts/rehearse-legacy-import.sh Server/scripts/legacy_cutover_test.go Server/scripts/unraid_deployment_test.go Server/deploy/unraid/push.yaml Server/deploy/unraid/compose.yaml docs/operations/legacy-import.md && rtk git commit -m "feat(ops): homelab cutover tooling with role drop, ledger deletion and the push overlay; the runbook; a synthetic rehearsal"`

### Task 22: Import duration at 10,000 families and at homelab shape [Wave I; after Task 18; parallel with Tasks 20 and 21]

**Files:**
- Create `Server/internal/legacyimport/scale_test.go`.
- Modify `Server/internal/legacyimport/legacytest/builder.go` only if L10k generation misses Step 3's budget: Task 10 already defines `Spec.UpdateEvery` and the streamed `CopyFrom` path.
- Modify `Server/scripts/test-db.sh`: `BT_TEST_RACE=0` drops `-race`, and `BT_TEST_TIMEOUT` passes `-timeout`. These are for timing runs only; the defaults are unchanged.
- Create `docs/verification/<run-date YYYY-MM-DD>-legacy-import-scale.md`, with the date of the run.

**Interfaces:** none new beyond the `Spec` field.

- [ ] **Step 1: Write the test** `scale_test.go`:
```go
// TestImportScale is the duration measurement (opt-in: BT_LEGACY_SCALE=L10k or P).
func TestImportScale(t *testing.T) {
	shape := os.Getenv("BT_LEGACY_SCALE")
	specs := map[string]struct {
		spec  legacytest.Spec
		limit time.Duration
	}{
		// 10,000 families, 4 members each (40,000 accounts and devices), 200 care entities per family (2,000,000), every third
		// updated once (about 2,670,000 journal rows), 11 rotations per device:
		// 40,000 sign-ins + 10,000 families + 60,000 invitation events + 440,000 rotations = 550,000 ledger events.
		"L10k": {legacytest.Spec{Families: 10000, Caregivers: 2, Viewers: 1, EntitiesPerFamily: 199, VersionsPerEntity: 1, UpdateEvery: 3,
			RotationsPerDevice: 11, NotCarried: true}, 15 * time.Minute},
		// Homelab-shaped (P), from the 2026-10-09 preflight and rollout record: 1 family, 2 accounts, 2 devices, 2 babies
		// (one with an icon), about 1,450 journal rows (1,100 entities, every fourth updated once), 2 Live Activity
		// registrations with a running card, and about 5,000 ledger events (2,500 rotations per device: three weeks
		// of 10-minute refreshes). The legacy schema is 29 (fce422d5).
		"P": {legacytest.Spec{Families: 1, Babies: 2, Caregivers: 1, EntitiesPerFamily: 1100, VersionsPerEntity: 1, UpdateEvery: 4, Timers: true,
			RotationsPerDevice: 2500, LiveActivity: true, NotCarried: true}, 10 * time.Second},
	}
	shapeSpec, ok := specs[shape]
	if !ok {
		t.Skip("BT_LEGACY_SCALE=L10k|P runs the duration measurement")
	}
	built := time.Now()
	l := legacytest.Build(t, shapeSpec.spec)
	t.Logf("built %s in %v", shape, time.Since(built))
	closeOtherPools(l.DB)
	o := optionsFor(l)
	o.DryRun = true
	dry, err := legacyimport.Run(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	o.DryRun, o.BackupReceipt = false, receiptFor(t, l, l.Now.Add(-time.Minute))
	real, err := legacyimport.Run(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	for _, report := range []legacyimport.Report{dry, real} {
		total, phases := time.Duration(0), map[string]time.Duration{}
		for _, p := range report.Phases {
			total += p.Duration
			phases[p.Name] = p.Duration
		}
		if total > shapeSpec.limit {
			t.Errorf("%s committed=%v: %v, limit %v", shape, report.Committed, total, shapeSpec.limit)
		}
		rate := func(n int64, d time.Duration) float64 { return float64(n) / d.Seconds() }
		if shape == "L10k" {
			if r := rate(report.Counts["care_entities"], phases["care"]); r < 5000 {
				t.Errorf("care %.0f entities/s, floor 5,000", r)
			}
			if r := rate(report.Counts["security_audit_events"], phases["authority"]); r < 10000 {
				t.Errorf("authority with audit %.0f rows/s, floor 10,000", r)
			}
		}
		t.Logf("%s committed=%v total=%v phases=%v counts=%v", shape, report.Committed, total, report.Phases, report.Counts)
	}
}
```
  The first walk's rate (at least 4,000 events/s) is asserted from `preflight`'s timing. `Run` records it as a `Phase{Name: "ledger_verify"}` ahead of the transaction phases. Add that to `Run` in this task.
- [ ] **Step 2: Run the homelab shape (P) first:** `df -h /` must show at least 5 GB free. Then `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_RACE=0 BT_TEST_TIMEOUT=30m BT_LEGACY_SCALE=P BT_TEST_PACKAGES='./internal/legacyimport' BT_TEST_RUN='TestImportScale' bash Server/scripts/test-db.sh`. Expected: PASS within 10 s per run.
- [ ] **Step 3: Run L10k:** `df -h /` must show at least 30 GB free, with no other `test-db.sh`, E2E or Swift run active. Then `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_RACE=0 BT_TEST_TIMEOUT=120m BT_LEGACY_SCALE=L10k BT_TEST_PACKAGES='./internal/legacyimport' BT_TEST_RUN='TestImportScale' bash Server/scripts/test-db.sh`. Expected: PASS, total ≤ 15 minutes per run.
  - A phase that misses its floor is profiled with `go test -cpuprofile` on the same command, and the cause is fixed in that phase (for example: larger copy batches, `SET LOCAL maintenance_work_mem`, or copying care rows before creating a secondary index and recreating it with the same definition, so the catalog test still holds).
  - Thresholds are not loosened without the owner's agreement.
- [ ] **Step 4: Record** `docs/verification/<run-date YYYY-MM-DD>-legacy-import-scale.md`:
  - the machine (`sysctl -n machdep.cpu.brand_string hw.memsize`, `docker info --format '{{.NCPU}} {{.MemTotal}}'`);
  - PostgreSQL 17.6 with the harness defaults;
  - the database size before and after (`pg_database_size`);
  - each shape's phases, totals and rates, for the dry run and the import;
  - a note that the reference-node measurement is a Phase 5 carry.

  Task 28 copies the measured totals into the runbook's duration section (Task 21 writes that file in parallel).
- [ ] **Step 5: Commit:** `rtk git add Server/internal/legacyimport Server/scripts/test-db.sh docs/verification && rtk git commit -m "test(legacyimport): measure the import at 10,000 families and at homelab shape"`



### Task 23: Merge `main` onto the ported branch [Wave J; after Tasks 14 and 20–22]

**Preconditions:**
- The owner approved merging `main` into this branch (D1); the branch is not merged into `main`.
- `rtk git status` is clean. Record `M=$(rtk git rev-parse main)`. The main checkout is never touched (memory "Parallel sessions on main").
- **The `main` this plan expects is `7040ca2d`** (2026-10-09). It is `fce422d5`, the baby-icons merge the homelab runs, plus the rollout record; it includes `6546a98c`. Its server side beyond `6546a98c` is exactly the baby icon, which Task 8a ported and the importer's vendored chain holds.
- **If `M` is not `7040ca2d`**, run `rtk git diff --stat 7040ca2d "$M" -- Server Packages Apps BabyTracker.xcodeproj Makefile`, and stop and report if `main` gained any of these since `7040ca2d`:
  - a migration;
  - a change to `Server/internal/liveactivity`, `protocol/live_activity.go`, `api/fixtures/live-activity-v1`, `protocol/{entities,canonical,mutations}.go` or the sync/record semantics;
  - a change to the app's wire (`BabyProtocol`, `EntryPayload.swift`, `ContentHash.swift`).

  A new migration that was deployed changes the homelab state: the D2 re-pin rule applies. Anything else must be ported before the merge, as Task 8a ported the icon.

**Files:** the merge commit. It is resolved by this table only; any conflict outside it stops the task, listing the paths.

| Path (as the merge reports it) | Resolution |
|---|---|
| `Server/migrations/000028_live_activity_push.sql` (added by `main`) | `git rm`. Its content is 000032 (Task 8). The homelab ran it, and its copy lives in the importer's vendored legacy chain (Task 3) |
| `Server/migrations/000029_baby_icon.sql` (added by `main`) | `git rm`. It would be a second version 29 beside the branch's `000029_postgres_authority.sql`, which the runner refuses. The homelab ran it, and its copy is vendored (Task 3 amendment). Its effect is carried in the wire with no schema change (Task 8a, Baby-icon ruling) |
| `Server/internal/migrate/compatibility.json`, `compatibility_test.go` | ours (`main`'s has 29 rows ending in `000029_baby_icon.sql`) |
| `Server/internal/protocol/{entities.go,canonical.go,canonical_test.go}` | ours. Task 8a carried `main`'s icon hunks verbatim, so `rtk git diff "$M" HEAD -- Server/internal/protocol/entities.go` shows no difference on the `Icon` field, and the canonical hunk is identical |
| `Server/internal/sync/baby_icon_test.go` (added on both sides) | ours (Task 8a's port of `main`'s tests onto the branch harness) |
| `Server/internal/sync/format_cleanup_interim_test.go`, `Server/internal/records/{babies,repository,snapshot,validation,validation_test,format_cleanup_join_test,format_cleanup_migration_test}.go` (modified by `main` for the icon) | stay deleted where the branch deleted them; otherwise ours |
| `Server/internal/testkit/fixtures.go` | ours (Task 8a added `main`'s `SetBabyIcon`; Task 8b the clock) |
| `Server/internal/database/*_test.go`, `Server/internal/tenant/context_test.go`, `Server/cmd/baby-tracker-overlap-check/main_test.go`, `Server/cmd/baby-tracker-server/live_activity_test.go`: `main`'s hunks that only move its schema pin 28 → 29 | ours (the branch pins 32; the overlap tool goes in Task 26) |
| `Server/internal/liveactivity/{types.go,presentation.go,lifecycle.go,queue.go,repository.go,service.go,snapshot.go,worker.go,authorization.go}` and their tests | ours (the port); identical files merge cleanly |
| `Server/internal/liveactivity/{apns.go,apns_identitytest.go,apns_test.go,provider_token.go,preflight.go,preflight_test.go}` | `git rm`: replaced by `internal/apns` (Task 7) and `transport.go` (Task 14) |
| `Server/internal/protocol/live_activity.go`, its test, `Server/api/fixtures/live-activity-v1/**` | identical to ours (byte-copied in Task 11) |
| `Server/internal/protocol/{auth.go,types.go,sync.go,types_test.go}`, `Server/api/openapi.yaml` | ours: Task 11 carried `main`'s Live Activity hunks and Task 8a its `BabyPayload.icon` line; `push-token-v1` and `live-activity-push-v1` are both known |
| `Server/internal/httpapi/live_activity*.go`, `router.go`, `operational.go` | ours (Task 11) |
| `Server/cmd/baby-tracker-server/live_activity.go` and its tests | ours (Task 14) |
| `Server/cmd/baby-tracker-server/{live_activity_preflight.go,live_activity_preflight_test.go,live_activity_transport_identitytest.go,live_activity_transport_identitytest_test.go,live_activity_composition_test.go}` | `git rm`: replaced by `apns_preflight.go`, the shared sink (Task 7) and `proveLiveActivityComposition` (Task 14) |
| `Server/internal/config/{live_activity.go,live_activity_test.go}` | ours (Task 7) |
| `Server/internal/security/**`, `Server/internal/records/**` (modified by `main`, deleted here) | stay deleted |
| `Server/internal/httpapi/{sync.go,auth.go,family.go,export.go,live.go,import.go,middleware.go}` and their tests | ours; Task 24 re-applies `main`'s fault logging |
| `Server/internal/family/{service.go,types.go}`, `Server/internal/access/types.go` (`MembershipRevision`), `Server/internal/auth/refresh.go` | ours; `MembershipRevision` is not ported (Live Activity ruling); Task 24 ports the cause wrapping |
| `Server/internal/limits/{policy.go,repository.go}` | ours (`repository.go` stays deleted; Task 11 added the two scopes) |
| `Server/internal/config/config.go`, `Server/internal/database/roles.go` and its test, `Server/cmd/baby-tracker-server/{main.go,main_test.go}` | ours: no worker login, no `VerifyLiveActivityRole*` |
| `Server/deploy/postgres/{init-roles.sql,init-users.sh}` | ours: no `baby_tracker_live_activity` role, login or secret |
| `Server/scripts/{provision-live-activity-secrets.sh,rehearse-live-activity-upgrade.sh,rehearse-live-activity-upgrade-seed.sql,rehearse_live_activity_upgrade_test.go,live_activity_secrets_test.go}`, `Server/deploy/unraid/live-activity-push.yaml` | `git rm`: the 27→28 upgrade is history (`main` itself marked the rehearsal historical in `bd182802`), and the separate worker credential is gone; `deploy/unraid/push.yaml` (Task 21) is the one push overlay. The homelab's `secrets/apns_key.p8` keeps the path both overlays use. The homelab's own copy of `live-activity-push.yaml` survives in R2's rollback source |
| `Server/deploy/unraid/compose.yaml`, `Server/deploy/unraid/tailscale/serve.json` | ours, plus `main`'s container-name Serve target (`98029498`) and its test if ours lacks them |
| `Server/scripts/{protocol-e2e.sh,protocol_e2e_test.go,lib/disposable-sync.sh,test-db.sh}` | ours; Task 25 ports `main`'s Live Activity proof onto them |
| `Server/README.md`, `docs/**` | union. `main`'s `docs/runbooks/family-live-activity-push.md` gets a first line: "Superseded: the homelab is cut over by docs/operations/legacy-import.md, which keeps Live Activity push on through deploy/unraid/push.yaml." |
| `Apps/**`, `Packages/**`, `BabyTracker.xcodeproj/project.pbxproj`, `Makefile` | theirs, then ours re-applied where Phase 3 changed the same file (`PushAppDelegate`, `AppEnvironment`, `BabyTrackerApp.swift`, `Info.plist`, entitlements, `SyncCoordinator`, `FamilySyncComposition`, `PushTokenRegistrar`, `BackgroundWakeHandler`). `EXPECTED_TESTS` is the measured count. The merge brings the whole app side of the baby icon unchanged; none of these may be resolved "ours" over `main`'s: `SchemaV10.swift`, `Baby.iconID`, `BabyStoreMigrationPlan`, `EntryPayload.swift`'s `icon`, `BabyStore+RemoteApply.swift`, `BabyStore+Babies.swift`'s fill-in, `ContentHash.swift`'s icon rule, `BabyAvatar`, `BabyAvatarView`, `BabySwitcherHeader`, the `avatar-*` assets and the Home/History/Insights headers. A phone that has run V10 must never get a pre-V10 store |
| `mockups/**`, `docs/superpowers/{specs,plans}/2026-10-08-baby-icons*`, `docs/verification/2026-10-08-baby-icons/**`, `docs/verification/2026-10-09-baby-icons-rollout.md` | theirs (new on `main`) |
| `TASKS.md` | `rtk git checkout $(rtk git merge-base HEAD $M) -- TASKS.md` (never carry another session's tracker) |

- [ ] **Step 1: Merge.** `rtk git merge --no-ff --no-commit "$M"`, resolved by the table.
- [ ] **Step 2: Gates on the merge.**
  - `rtk make server-build`; `rtk make server-test`; `(cd Server && rtk go vet ./... && rtk go vet -tags identitytest ./...)`.
  - `rtk go -C Server test ./internal/liveactivity ./internal/protocol -count=1 -run 'Project|Reconcile|LiveActivity|Icon'`: the projection fixture and the wire tests pass against `main`'s fixtures as merged, `main`'s icon hash and decode tests included.
  - `ls Server/migrations` shows exactly one file per version, and no `000028_live_activity_push.sql` or `000029_baby_icon.sql`.
  - `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/sync ./internal/care ./internal/httpapi' BT_TEST_RUN='Icon' bash Server/scripts/test-db.sh`: Task 8a's icon tests pass on the merged tree.
  - With `pgrep -fl xcodebuild` printing nothing, `rtk make test`. Set `EXPECTED_TESTS` to the reported count and say so in the report. `main`'s `BabyIconSyncTests`, `BabyIconAssignmentTests`, `WireShapeTests` and `BaselineStoreTests` (V10) are among the passing tests.
  - `main`'s Swift `LiveActivityPushE2ETests` stays inert unless `BABY_TRACKER_E2E_APNS=1` (it already is). Task 25 turns it on.
- [ ] **Step 3: Commit the merge:** `rtk git add -A && rtk git commit -m "merge main into the PostgreSQL authority with Live Activity ported"`, with a body listing `$M` and the table's resolutions.


### Task 24: Port `main`'s server fault logging and the shared push resolver [Wave J; after Task 23]

**Files:**
- Modify `Server/internal/httpapi/{middleware.go,sync.go,auth.go,family.go,export.go,live.go,import.go}`: `main`'s `logServerFault`/`describeCause` (commits `2af18d47`, `a842d513`): one ERROR line per 5xx with a cause class, INFO when the client left, and panics logged by type. Applied to the branch's writers (`writeSyncError`, `writeRequestError`, and the auth, family, export, import and live writers) without changing any status or body.
- Modify `Server/internal/httpapi/sync_test.go`: port `main`'s three logging tests.
- Modify `Server/internal/auth/refresh.go`: `main`'s `%w: %w` cause wrapping on `ErrUnavailable`.
- Modify `Packages/BabyTracker/Sources/BabySync/LiveActivityAPNsEnvironment.swift`: `public typealias LiveActivityAPNsEnvironment = PushEnvironmentResolver` (Phase 3 plan :4011), with `main`'s callers unchanged. Modify its tests.

- [ ] **Step 1: Write the failing tests.** Port `main`'s logging tests from `git show $M:Server/internal/httpapi/sync_test.go` as they are; they assert on `describeCause` output. Add `TestFaultLoggingNeverChangesTheResponse`, which reruns `TestSyncErrorPrecedenceIsUnchanged`'s 103-row table (Phase 3 Task 6) with a capturing logger: every status and body is unchanged, every 5xx row logs exactly one ERROR line with no token, digest or body text, and every 4xx row logs no ERROR. For Swift, `PushEnvironmentResolverTests` gains `liveActivityEnvironmentIsThePushResolver`: the two resolve the same profile to the same environment.
- [ ] **Step 2: Run to see the failure:** `rtk go -C Server test ./internal/httpapi -run 'TestFaultLogging|Test.*Cause' -count=1` and `rtk make test` (with the `pgrep` check). Expected: `undefined: describeCause`; Swift redeclaration.
- [ ] **Step 3: Implement.** Port the two functions verbatim from `git show $M:Server/internal/httpapi/middleware.go`, call them where `main` calls them, and adapt only the call sites the branch renamed.
- [ ] **Step 4: Run green:** the Step 2 commands, then `rtk make server-test` and `rtk make test`.
- [ ] **Step 5: Commit:** `rtk git add Server/internal Packages/BabyTracker && rtk git commit -m "feat(httpapi): main's fault-cause logging on the authority handlers; one push environment resolver"`



### Task 25: The app's Live Activity cards end to end through the new server [Wave J; after Task 24]

**Files:**
- Modify `Server/scripts/lib/disposable-sync.sh`. The `BT_DISPOSABLE_LIVE_ACTIVITY=1` profile adds `BT_LIVE_ACTIVITY_PUSH_ENABLED: "true"` on top of the Phase 3 push profile (`BT_DISPOSABLE_PUSH=1`: the throwaway P-256 key, `BT_APNS_*` and `BT_IDENTITYTEST_APNS_SINK=1`). It creates no worker login and no `live_activity_*` secret (`main`'s lines 225-261 are not ported).
- Modify `Server/scripts/protocol-e2e.sh` and `protocol_e2e_test.go`:
  - export `BT_DISPOSABLE_LIVE_ACTIVITY=1` with the push profile in both modes;
  - require `live-activity-push-v1` in `/v1/server` when it is on;
  - require the Swift line `^live-activity proof: step 13:`;
  - compare the `live-activity ledger:` line with Postgres: `registrations=` is the count of non-revoked `live_activity_devices`, `sandbox=` those with `environment='sandbox'`, `cards=` the `live_activity_tokens` rows;
  - require `delivered=` (`live_activity_history ⋈ live_activity_delivery`, `result='accepted'`) ≥ 1;
  - require that no token from the `live-activity tokens:` line appears in the app's logs;
  - echo a `live-activity proof:` summary line.

  These are `main`'s lines 143-169, re-pointed at the 000032 tables.
- Modify `Packages/BabyTracker/Tests/FamilySyncClientE2ETests/LiveActivityPushE2ETests.swift` only where the new server legitimately differs. Each such change carries a comment naming the server behaviour. Known candidates:
  - wait up to 5 s for the worker's 1-second poll plus the change-feed flush, where `main`'s journal trigger enqueued work synchronously;
  - the recorder JSON field names (`provider_token_valid`, shared with Phase 3's `APNSRecorder.swift`).
- Modify `Makefile` (`EXPECTED_TESTS`) if the count changes.

- **Baby icons in the E2E (revision 3).** After the merge, the Swift E2E clients are `main`'s V10 app code. A baby they create with Boys/Girls set carries `icon`, and the launch fill-in sends an icon-bearing `update`. So the two-caregiver and Live Activity runs exercise Task 8a on the wire with no test change. A step failing with `invalid_request` or `invalid_mutation` on a baby is a server defect (Task 8a), never a test edit.
- [ ] **Step 1: Run to see the failure:** `rtk make server-protocol-e2e`. Expected: non-zero, with `missing proof line: live-activity proof: step 13:` (before the profile exists), or a step failure naming what differs.
- [ ] **Step 2: Implement** the profile, the proof checks and their pins in `protocol_e2e_test.go`. Then run the suite and fix the **server** wherever a step fails because the port differs from `main`'s observable behaviour; the Swift test is `main`'s specification. A test edit is allowed only for the two candidates above, each recorded in the report.
- [ ] **Step 3: Run green:**
  - `rtk make server-protocol-e2e` exits 0 and prints every Phase 3 proof line plus `live-activity proof: step 13:` and the summary `live-activity proof:` line;
  - `rtk env BT_RECOVERY_E2E_MODE=self_hosted make server-protocol-e2e` exits 0 with the same lines;
  - `rtk go -C Server test ./scripts -run TestProtocolE2E -count=1`;
  - with `pgrep -fl xcodebuild` printing nothing, `rtk make test` shows the expected count.
- [ ] **Step 4: Commit:** `rtk git add Server/scripts Packages/BabyTracker/Tests/FamilySyncClientE2ETests Makefile && rtk git commit -m "test(e2e): the app's Live Activity cards through the ported server"`


### Task 26: Squash the chain into one baseline before the cutover (C1) [Wave J; after Task 25]

**Preconditions:** the tree is at Task 25's commit. The importer, its vendored legacy chain and its fixtures stay: they build the homelab's legacy schema from `legacytest/testdata/legacy-chain/`, not from `Server/migrations`.

**Files:**
- **Create** `Server/migrations/000032_baseline.sql`.
- **Delete** `Server/migrations/000001_*.sql` … `000031_*.sql` and `000032_live_activity.sql`.
- **Create** `Server/internal/migrate/testdata/catalog-32.golden` and `Server/internal/database/baseline_test.go`.
- **Modify:**
  - `Server/internal/migrate/{migrate.go,compatibility.json,compatibility_test.go}`: a first file named `NNNNNN_baseline.sql` at version B is accepted, contiguous after it. `compatibility.json` becomes `{"min":32,"max":32,"known":[{32,"000032_baseline.sql",<sha>}]}`.
  - `Server/internal/testpg/database.go`: `Through(fs, v)` with `0 < v < 32` fails the test ("pre-baseline versions were squashed into 000032_baseline.sql"); `Through(fs, 0)` and `NewFrom` stay.
  - `Server/cmd/baby-tracker-server/authority_runtime_test.go:500-515`: the old-binary case uses a synthetic compatibility `{min:31,max:31}` and expects the baseline database refused.
  - `Server/scripts/smoke.sh` and `smoke_test.go`: `contiguous schema 32` becomes `baseline schema 32`.
  - `Server/README.md`: one line under "Migrations" describes the baseline and says the next migration is 000033.
- **Delete the tests of individual pre-baseline migrations:**
  - `internal/database/{legacy_format_cleanup_test.go,legacy_format_cleanup_join_test.go}`;
  - the 28→29 guard test in `authority_schema_test.go:175-205`;
  - the 30 guard test in `care_schema_test.go:495-520`;
  - `request_path_schema_test.go`'s `TestMigrationThirtyOneRefusesAuditRowsFromTwentyTwentySeven` and `TestMigrationThirtyOneOverThirty`;
  - `migrate/compatibility_test.go`'s `TestBinaryRefusesSchema*` rows for versions below 32.

  Also delete the legacy migration-overlap tool: `Server/cmd/baby-tracker-overlap-check/`, `Server/scripts/check-migration-overlap.sh` and `Server/scripts/migration_overlap_test.go`; it only exercises predecessor schemas 10–28. Delete `Server/scripts/format-cleanup-dry-run.sh` too (the 26→27 rollout tool).

  Find any other caller with `rtk proxy grep -rn 'NewThrough\|MigrateThrough\|testpg.Through' Server --include='*_test.go'`. Each one either migrates to 32 (keep) or tests a squashed migration (delete, listed in the report). The importer's tests use `legacytest.NewLegacy` and are untouched, with one exception, below.

**Revision 3 notes and carries for the squash:**
- **The baby icon is not in the schema** (Baby-icon ruling). The baseline, the golden and the manifest have no icon line. Task 8a's tests run unchanged on the baseline. `main`'s `babies.icon` exists only in the vendored legacy chain.
- **Detect's "branch's own 29" case seeds its rows literally** (Task 3 carry).
  - After the squash, `testpg.NewThrough(t, 29)` fails ("pre-baseline versions were squashed"). `TestDetectAcceptsOnlyTheHomelabState`'s "the branch's own 29" case therefore builds `legacytest.NewLegacyThrough(t, 27)`, then inserts the branch's pre-squash rows 28 and 29 as literals: `(28,'000028_mcp_integrations.sql',<sha>)` and `(29,'000029_postgres_authority.sql',<sha>)`.
  - The two digests are copied from this branch's `compatibility.json` at Task 25's commit. Expected detail: `version 28: 000028_mcp_integrations.sql`.
  - Rows 1–27 of the branch equal the homelab's, so the case still proves that a branch history is refused at its first differing row.
- **`already_imported` still holds on the baseline** (Task 3 carry). `Detect` treats rows exactly equal to the embedded manifest as the new schema. After the squash that manifest is the single baseline row, which the importer writes. `TestImportRunsOnceAndVerifiesAfter` (a second run is `ErrAlreadyImported`) must pass unchanged on the baseline; it is in Step 5's run.
- **The golden names no month** (Task 4 review I1/M1). `catalog-32.golden` holds `audit_monthly|first=expected|…`, never a month, and the snapshot runs in UTC. From February 2027 a fresh install starts its partitions later, and the golden must still match then. Step 1 records the golden only after Task 4's fix round is in.
- **No deferred constraints** (Task 4 review M5). The baseline declares no `DEFERRABLE` constraint, and `schema.sql`'s dump of 000027's objects carries none. `TestApplyTxAppliesTheRestInTheCallersTransaction` still runs on the baseline.

- [ ] **Step 1: Record the chain's catalog as the golden, before anything is deleted.** Write `database/baseline_test.go`:
```go
// The fresh-install catalog is pinned: a schema change updates this golden in
// the same commit (BT_GOLDEN_UPDATE=1), so a squash or a migration that changes
// the catalog by accident fails here.
func TestBaselineCatalogMatchesTheChainGolden(t *testing.T) { // Review Focus 2
	f := testpg.New(t)
	testpg.ProductionShape(t, f)
	got := testpg.SnapshotCatalog(t, f.Owner, testpg.Roles(t, f))
	testkit.AssertGolden(t, "../migrate/testdata/catalog-32.golden", got.Lines)
}
```
  Run `rtk proxy env GOCACHE=$(mktemp -d) BT_GOLDEN_UPDATE=1 BT_TEST_PACKAGES='./internal/database' BT_TEST_RUN=TestBaselineCatalogMatchesTheChainGolden bash Server/scripts/test-db.sh` on the unchanged chain, then the same without `BT_GOLDEN_UPDATE` (PASS). Commit the golden alone: `rtk git add Server/internal/migrate/testdata/catalog-32.golden Server/internal/database/baseline_test.go && rtk git commit -m "test(migrate): pin the chain's fresh-install catalog before the squash"`.
- [ ] **Step 2: Generate the baseline** from a fresh chain install, in `mktemp -d`. Nothing is committed but the result.
  1. `docker run -d --rm --name bt-baseline-$$ -e POSTGRES_PASSWORD=x postgres:17.6-alpine`.
  2. Apply `Server/deploy/postgres/init-roles.sql`, then create the login `baby_tracker_migrator`, the database `babytracker` owned by it, and `ProductionShape`'s schema state.
  3. `go run ./cmd/baby-tracker-admin migrate` with the migrator's DSN.
  4. `docker exec bt-baseline-$$ pg_dump -U postgres -d babytracker --schema-only --no-owner --no-tablespaces > schema.sql`.
  5. `docker exec bt-baseline-$$ psql -XAt -U postgres -d babytracker -c "SELECT format('INSERT INTO public.schema_privilege_manifest VALUES (%s,%L,%L,%L,%L);', schema_version, principal, object_kind, object_identity, privileges) FROM public.schema_privilege_manifest WHERE schema_version = 32 ORDER BY 2,3,4" > manifest.sql`.
  6. Stop the container.

  `000032_baseline.sql` is, in order:
  - a header: "000032_baseline: the schema of 000001–000032, squashed before the homelab cutover (Phase 4 Task 26). Later changes go in 000033+.";
  - `schema.sql` without its `SET …` and `SELECT pg_catalog.set_config(…)` preamble;
  - without the `security_audit_events_YYYY_MM` monthly partitions and their `ATTACH PARTITION`;
  - with `CREATE POLICY push_token_authority … TO baby_tracker_migrator` restated `TO CURRENT_USER`;
  - `manifest.sql`;
  - `INSERT INTO public.schema_compatibility(singleton,current_version,introduced_by) VALUES (true,32,'000032_baseline.sql');`;
  - `DO $$ BEGIN PERFORM app.ensure_audit_partitions(now() + interval '92 days'); END $$;`.
- [ ] **Step 3: Write the failing tests.**
  - `migrate/compatibility_test.go`, `TestOnlyTheBaselineIsEmbedded`: the embedded source is exactly one file, `000032_baseline.sql`, whose sha256 is the compatibility row. A database whose history is `{1, "000001_server_state.sql", …}` (inserted by the test) is refused with `ErrChecksumMismatch` or `ErrDatabaseNewer`, never adopted.
  - `legacyimport`: `TestImportedCatalogEqualsAFreshInstall` (Task 18) now imports onto the baseline and compares with a baseline install. It passes unchanged, which proves the importer lands directly on the baseline (C1).
- [ ] **Step 4: Implement:** delete the files, add the baseline, change `loadMigrations` to accept a leading baseline, update `compatibility.json`, and make the test and script changes listed.
- [ ] **Step 5: Run green:**
  - `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/migrate ./internal/database ./internal/testpg ./internal/legacyimport/...' bash Server/scripts/test-db.sh`, with `TestBaselineCatalogMatchesTheChainGolden` without update (Review Focus 2);
  - `rtk make server-build`; `rtk make server-test`; `(cd Server && rtk go vet ./... && rtk go vet -tags identitytest ./...)`;
  - `rtk make server-smoke`.
- [ ] **Step 6: Commit:** `rtk git add -A Server && rtk git commit -m "chore(migrate): squash 000001-000032 into one baseline before the cutover"`

### Task 27: Drop the projector and Apple-revoker roles from the baseline (C2) [Wave J; after Task 26]

**Files:**
- **Modify** `Server/migrations/000032_baseline.sql` (amended before the Task 28 freeze), and regenerate its digest in `compatibility.json`:
  - drop every `GRANT`/`REVOKE` naming `baby_tracker_projector` or `baby_tracker_apple_revoker`;
  - drop their `schema_privilege_manifest` rows;
  - restate `schema_privilege_manifest_principal_check` as `CHECK (principal IN ('baby_tracker_maintenance','baby_tracker_runtime'))`.
- **Modify** `Server/deploy/postgres/init-roles.sql` and `init-users.sh`: no projector or Apple-revoker group, login or password secret.
- **Modify** `Server/compose.yaml`: no `projector_password` or `apple_revocation_password` secret.
- **Modify** `Server/scripts/provision-unraid-secrets.sh` and its test: stop creating those secrets and `projector_database_url`.
- **Modify** `Server/scripts/check-principals.sh`, `Server/scripts/smoke.sh`, `smoke_test.go`, `Server/scripts/lib/disposable-sync.sh`, `Server/scripts/test-db.sh`, `Server/scripts/bootstrap_integration_test.go`, `Server/scripts/refresh-recovery-phases.sh` and `Server/scripts/sync-performance.sh`: every reference to the two roles goes (the earlier grep lists them).
- **Modify** `Server/internal/testpg/database.go`: no `Revoker` pool or `security_revoker_*` login. Remove every `f.Revoker` use: about 9 tests under `internal/database`, `internal/tenant` and `cmd/baby-tracker-server`, plus `legacyimport`'s `TestCheckAlone` line.
- **Modify** `Server/internal/database/roles.go` and `roles_test.go`: role checks know only runtime and maintenance.
- **Modify** `Server/internal/identity/apple_tokens.go`: remove `HostedApple.Revoke`/`AppleRevoker`/`AppleAuthority`, the dead code Phase 1 recorded (no caller).
- **Modify** `Server/internal/migrate/testdata/catalog-32.golden` (regenerated) and `Server/internal/database/baseline_test.go`.
- **Modify** `Server/README.md`: the roles table.

- [ ] **Step 1: Write the failing tests** in `database/baseline_test.go`:
```go
// The baseline differs from the chain's golden by exactly the two retired roles.
func TestBaselineDropsExactlyTheLegacyRoles(t *testing.T) { // Review Focus 2
	chain := readGoldenAt(t, "../migrate/testdata/catalog-32.golden", "HEAD~1") // the golden as Task 26 committed it
	f := testpg.New(t)
	testpg.ProductionShape(t, f)
	now := testpg.SnapshotCatalog(t, f.Owner, testpg.Roles(t, f))
	for _, line := range testpg.Catalog{Lines: chain}.Diff(now) {
		if !strings.Contains(line, "baby_tracker_projector") && !strings.Contains(line, "baby_tracker_apple_revoker") &&
			!strings.Contains(line, "schema_privilege_manifest_principal_check") {
			t.Fatalf("the role drop changed more than the two roles: %s", line)
		}
	}
	var granted int
	_ = f.Owner.QueryRow(t.Context(), `SELECT count(*) FROM pg_class c, aclexplode(coalesce(c.relacl, acldefault('r', c.relowner))) a
		JOIN pg_roles r ON r.oid = a.grantee WHERE r.rolname IN ('baby_tracker_projector','baby_tracker_apple_revoker')`).Scan(&granted)
	if granted != 0 {
		t.Fatalf("%d grants still name a retired role", granted)
	}
}

// After an import, R6's DROP ROLE succeeds: nothing in the database depends on the retired roles.
func TestLegacyRolesDropCleanlyAfterAnImport(t *testing.T) { // Review Focus 2
	// legacytest.Build (Spec{Families: 1, LiveActivity: true}) on a cluster where the four legacy roles and
	// baby_tracker_live_activity exist with main's grants; legacyimport.Run; then, as superuser:
	// SELECT count(*) FROM pg_shdepend d JOIN pg_roles r ON r.oid=d.refobjid WHERE d.dbid=(SELECT oid FROM pg_database
	// WHERE datname=current_database()) AND r.rolname IN (…) = 0, and a DROP ROLE of each (in a transaction rolled
	// back at the end, so the shared test cluster keeps its roles for other tests) succeeds.
}
```
  `readGoldenAt` reads the file at a git revision with `git show` (test helper; the repository is always present in this tree's tests). In this test `Build` creates the legacy roles if missing, with `main`'s grants from the vendored `000007`/`000016` statements. The `DROP ROLE` runs inside a rolled-back transaction because roles are cluster-wide in the shared test container.

  **The drop must really execute** (Task 4 review M6). The catalog snapshot covers neither database-level grants (`pg_database.datacl`) nor role memberships (`pg_auth_members`), so only an executed `DROP ROLE` proves R6 will succeed. The test therefore:
  - grants each legacy role what `main`'s `init-users.sh` gave it at database and membership level (`CONNECT` on the database, the login's membership in its group), read from `git show fce422d5:Server/deploy/postgres/init-users.sh`;
  - runs the import;
  - executes `DROP ROLE` for each of the six roles inside the rolled-back transaction, expecting success.

  The test's statements are the ones Task 21's `drop-legacy-roles` runs, in the same order. If the test shows another statement is needed, `legacy-cutover.sh` and its test change in this task's commit.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/database ./internal/legacyimport/...' BT_TEST_RUN='TestBaselineDrops|TestLegacyRolesDrop' bash Server/scripts/test-db.sh`. Expected: a non-role diff count of 0, but grants still name the retired roles.
- [ ] **Step 3: Implement** the edits listed. Regenerate the golden with `BT_GOLDEN_UPDATE=1`, then re-run without it.
- [ ] **Step 4: Run green:** the Step 2 command; `rtk make server-test`; `rtk make server-smoke`; `rtk make server-protocol-e2e`; `(cd Server && rtk go vet ./... && rtk go vet -tags identitytest ./...)`; `rtk proxy grep -rn 'baby_tracker_projector\|baby_tracker_apple_revoker\|apple_revocation_password\|projector_password' Server --exclude-dir=testdata` prints nothing outside `legacyimport`'s legacy-chain files and `legacy-cutover.sh`'s `drop-legacy-roles`.
- [ ] **Step 5: Commit:** `rtk git add -A Server && rtk git commit -m "chore(roles): drop the projector and Apple-revoker roles from the baseline and the deployment"`

### Task 28: Freeze the baseline, the final gates and the rehearsal on the merged tree [Wave J; after Task 27]

**Files:** `Server/migrations/000032_baseline.sql` (the header line), `Server/internal/migrate/compatibility.json` (the digest), `docs/operations/legacy-import.md` (the duration figures and the rehearsal line), and script fixes only if the gates expose any, each committed with its reason.

- [ ] **Step 1: Freeze.** Add `-- 000032_baseline FROZEN (Phase 4 Task 28): later changes go in 000033+.` to the header. Record the digest again (`shasum -a 256`), then run `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/migrate ./internal/database' bash Server/scripts/test-db.sh` (the golden is unchanged by a comment).
- [ ] **Step 2: Server gates.**
  - `rtk make server-build`; `rtk make server-test`; `(cd Server && rtk go vet ./... && rtk go vet -tags identitytest ./...)`;
  - `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='-v ./internal/legacyimport/...' BT_TEST_RUN='TestDetect|TestCaptured|TestImported|TestDryRun|TestImportIs|TestCarriedCard|TestLiveActivityRows' bash Server/scripts/test-db.sh`, with the PASS lines in the report. The importer now applies the baseline (C1), and `TestDetect` still refuses the `998501b` 28 and accepts only `homelab_29`.
  - `rtk proxy env GOCACHE=$(mktemp -d) BT_TEST_PACKAGES='./internal/protocol ./internal/care ./internal/sync ./internal/httpapi' BT_TEST_RUN='Icon' bash Server/scripts/test-db.sh` (Review Focus 6, on the final tree).
  - **The homelab has not moved.** `git show main:docs/verification/` lists no rollout after `2026-10-09-baby-icons-rollout.md` that changed the homelab's source, and `main` is still `7040ca2d` or differs from it only outside `Server/`. Otherwise the D2 re-pin rule applies before any R-step.
- [ ] **Step 3: iOS.** With `pgrep -fl xcodebuild` printing nothing, `rtk make test` (the expected count). Then, on the shared simulator only, `rtk proxy xcodebuild -project BabyTracker.xcodeproj -scheme BabyTracker -destination 'platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853' -derivedDataPath /tmp/bt-p4-gates-dd build` succeeds; remove `/tmp/bt-p4-gates-dd` afterwards. Never create, reset or erase a simulator.
- [ ] **Step 4: Protocol E2E.** `rtk make server-protocol-e2e` and `rtk env BT_RECOVERY_E2E_MODE=self_hosted make server-protocol-e2e`. Both exit 0 with every proof line:
  - `two-caregiver proof:`, `owner-reinstall proof:`, `running-timer snapshot proof:`;
  - `wake proof:`, `background-push proof:`;
  - `live-activity proof: step 13:` and the `live-activity proof:` summary.

  One rerun is allowed for the recorded Swift flakes; a second failure blocks.
- [ ] **Step 5: MCP and smoke.** `rtk bash Server/scripts/mcp-e2e.sh` (`matches=0`, `schema=32`), run with the machine idle; `rtk make server-smoke` (`baseline schema 32`, grants equal to manifest 32 with no retired role, `authority verify` clean).
- [ ] **Step 6: Rehearsal on the final tree.** `rtk bash Server/scripts/rehearse-legacy-import.sh --synthetic --legacy-sha fce422d51ef4fbd4ac98a4bab4e4375ff6e762a6` prints `rehearsal proof:` with `icons=1 card_updated=1 roles_dropped=6 ledger_deleted=1 legacy_ready=1 cleanup=0/0`. Paste it, and Task 22's measured totals, into `docs/operations/legacy-import.md` ("Rehearsed", "Duration").
- [ ] **Step 7: Commit:** `rtk git add Server/migrations/000032_baseline.sql Server/internal/migrate/compatibility.json Server/scripts docs/operations/legacy-import.md && rtk git commit -m "test(e2e): freeze the baseline; final gates and a rehearsed homelab cutover"`

---

**The cutover gate (not a task).**
- The coordinator runs `docs/operations/legacy-import.md` R0–R10 with the owner, one approval per step:
  - D4, D5 and C4–C6 are asked at R0;
  - R4 is the owner's force-quit;
  - R6 is the superuser role drop (C2, C4);
  - R7 deletes the ledger directories (C3).
- Part P starts only after R10: the owner confirms in the conversation that the homelab cut over, and the confirmation is recorded in `docs/verification/<date>-homelab-cutover.md`.


## Part P — after the cutover (runs only after the owner confirms the homelab cutover, runbook R10)

### Task 29: Delete the importer and all its tooling [Wave P; after R10]

**Preconditions:** `docs/verification/<date>-homelab-cutover.md` records the owner's confirmation (R10). The homelab runs Task 28's SHA.

**Files:**
- **Delete:**
  - `Server/internal/legacyimport/` (all, including the vendored legacy chain and the fixtures) and `Server/internal/legacyledger/` (all);
  - `Server/cmd/baby-tracker-admin/{import_legacy.go,import_legacy_test.go}`, and `import-legacy` from `main.go`'s dispatch and usage and from `main_test.go`'s retired-settings table;
  - `Server/internal/authority/{legacy_audit.go,legacy_audit_test.go}` and `authority.TokenDigest` (no caller left);
  - `Server/internal/care/{copy.go,copy_test.go}`;
  - `Server/scripts/{homelab-preflight.sh,homelab_preflight_test.go,capture-legacy-fixture.sh,capture_legacy_fixture_test.go,legacy-cutover.sh,rehearse-legacy-import.sh,legacy_cutover_test.go}`;
  - `Server/internal/testpg`'s `NewFrom`, if no caller remains.
- **Modify:**
  - `Server/deploy/unraid/compose.yaml`: remove the `security_ledger`/`ledger_head` volumes (the homelab's directories were deleted at R7);
  - `Server/scripts/provision-unraid-secrets.sh:21` and its test: stop creating `security-ledger` and `ledger-head`;
  - `Server/README.md`: remove "Upgrading a legacy install";
  - `Server/internal/migrate/migrate.go`'s comment: "the legacy chain lived in Phase 4's importer".
- **Move** `docs/operations/legacy-import.md` → `docs/verification/<date>-homelab-cutover-runbook.md`, as the record of what ran.

- [ ] **Step 1: Write the failing test** `Server/scripts/no_legacy_import_test.go`, `TestNoLegacyImportRemains`. It runs `git ls-files Server docs/operations` and fails on any path containing `legacyimport`, `legacyledger`, `import_legacy`, `legacy-cutover`, `capture-legacy`, `rehearse-legacy` or `homelab-preflight`, and on any tracked `Server/` file containing the strings `import-legacy` or `legacy_public`.
- [ ] **Step 2: Run to see the failure:** `rtk go -C Server test ./scripts -run TestNoLegacyImportRemains -count=1`. Expected: it lists the importer's files.
- [ ] **Step 3: Delete and modify** as listed.
- [ ] **Step 4: Run green:** the Step 2 command; `rtk make server-build`; `rtk make server-test`; `(cd Server && rtk go vet ./... && rtk go vet -tags identitytest ./...)`; `rtk make server-smoke`; `rtk make server-protocol-e2e`.
- [ ] **Step 5: Commit:** `rtk git add -A Server docs && rtk git commit -m "chore: the homelab cut over; delete the one-time legacy importer and its tooling"`


### Carries out of Phase 4
- **Phase 5 (load and tuning):**
  - the import duration on the declared reference node against the 15-minute L10k target;
  - the Live Activity worker at 10,000 families (the once-a-minute sweeps over every registered family, claim latency with 40,000 registrations, the change-feed flush);
  - one APNs connection per environment shared by wakes and Live Activity sends, under Apple's limits;
  - `main`'s Live Activity hardening tests under `-race` at load;
  - the wake backstop and the dirty sweep in the minutes after a cutover.
- **Phase 6 (operator docs and rollout):**
  - install a merged app build on the phones (C6), which adds the push-token registrar; then the real-device check of background wakes;
  - distribution signing for the production APNs environment;
  - Apple token revocation on account deletion;
  - the device-build push entitlements;
  - the hosted-service rollout.

### Critical Files for Implementation
- <repo>/.worktrees/postgres-authority/Server/internal/legacyimport/ (new, deleted by Task 29): detect.go, stage.go, crosscheck.go, authority_load.go, audit_load.go, care_load.go, live_activity_load.go, verify.go, run.go; legacytest/testdata/legacy-chain/ (fce422d5's 29 migrations)
- <repo>/.worktrees/postgres-authority/Server/internal/legacyledger/ (walk.go, sidecar.go, enumerate.go; deleted by Task 29)
- <repo>/.worktrees/postgres-authority/Server/migrations/000032_live_activity.sql (new), then 000032_baseline.sql (Tasks 26–28)
- <repo>/.worktrees/postgres-authority/Server/internal/liveactivity/ (new: the port) and Server/internal/authority/live_activity.go (new)
- <repo>/.worktrees/postgres-authority/Server/internal/apns/send.go, preflight.go (new)
- <repo>/.worktrees/postgres-authority/Server/internal/migrate/migrate.go (ApplyTx; the leading baseline)
- <repo>/.worktrees/postgres-authority/Server/internal/testpg/catalog.go (new, permanent)
- <repo>/.worktrees/postgres-authority/Server/scripts/legacy-cutover.sh, homelab-preflight.sh (new, deleted by Task 29), Server/deploy/unraid/push.yaml (new) and docs/operations/legacy-import.md (new; the cutover record after Task 29)

### Self-review (writing-plans checklist)
- **Spec coverage**, section "Migration of existing installs":
  - "runs once with the server stopped": Tasks 3, 13 and 18; runbook R4 stops the phones and then the server (D3).
  - "in one transaction": the Transaction ruling and Task 18.
  - step 1: Tasks 2 and 13. Step 2: Task 13. Step 3: Task 4, and the baseline after Task 26 (C1).
  - step 4: authority and the server identity in Task 15; audit history with legacy hashes in Tasks 5 and 15; all care data in Task 16; plus Live Activity state in Task 17.
  - step 5: Task 18 (`dropStep`).
  - "all-or-nothing and refuses to run twice": Task 18. "devices stay signed in": Tasks 15 and 20.
  - "legacy … code survives only inside the importer's package": Task 2's dependency test; Task 29 deletes it, and the legacy chain lives only in the importer's test support from Task 3.
  - "a fresh install creates the new schema directly": the baseline (Task 26), pinned by the golden.
  - "Rollback restores the required pre-upgrade backup of database and ledger files taken together": Task 21. R7 deletes the ledger directories only after `verify`; `rollback` restores them from `ledger.tar`.
  - "Importer tests on real legacy fixtures": Tasks 6, 16 and 20.
  - The 10,000-family timing: Task 22. Reconciliation with `main` (spec "Boundaries"): Tasks 7, 8, 11, 12, 14, 17 and 23–25.
- **The owner's decisions:**
  - D1: Tasks 7, 8, 11, 12, 14, 17, 23–25.
  - D2's consequence for the wire: Task 8a adopts `main`'s baby icon (no wire change), so the homelab's `fce422d5` phones keep working.
  - D2: preflight facts (revision 3: `fce422d5`, schema 29), the re-pin rule, Tasks 1, 3 (exactly `homelab_29`, amendment) and 6 (fixture recaptured at `fce422d5`), runbook R0/R1.
  - D3: the carried-data ruling, runbook R4, and no in-flight code.
  - C1: Task 26 before the cutover; the importer applies the baseline; no adoption code exists.
  - C2: Task 27 and runbook R6. C3: runbook R7 and Task 29.
  - D4, D5 and C4–C6: asked at R0.
  - Live Activity live on the homelab: carried by Task 17, kept on by `push.yaml` (Task 21) and R2/R5, checked at R8.
- **Spec items not mapped, with the reason:**
  - "MCP credentials keep working": the homelab has no MCP integration (`main` never had MCP).
  - `legacy_hash` sits outside the per-scope row hash (000029); the chain is bound through the hashed payloads' `previous_hash` and the anchors.
  - "Token digests and keys are unchanged" holds for active sessions only (D3): an old consumed refresh token answers `session_revoked` without revoking its lineage.
- **Placeholder scan.**
  - No step says TBD, "add error handling" or "similar to Task N".
  - Ported code is specified by source commit and path plus an explicit substitution table (Tasks 8, 11, 12, 14 and 23).
  - Secondary tests that keep their assertions as comments: Task 7's `TestWakeIsSendWithTheBackgroundShape`, Task 8's `TestCareOriginShape`, Task 12's snapshot tests, Task 14's `TestStoppedTimerEndsItsCardOnce`/`TestInvalidCardTokenIsClearedOnce` and Task 27's `TestLegacyRolesDropCleanlyAfterAnImport`.
  - `HomelabChain`'s 29 literals are written from the vendored `manifest.json` (Task 3 and its amendment), pinned by `TestVendoredChainIsTheHomelabs`.
- **Type consistency.** These names are used identically across tasks:
  - `legacyledger.WalkVerified`/`Verified`/`RecoveryIdentity`/`ActorKeySidecar`/`Entry`; `ledgertest.Writer`;
  - `legacyimport.Detect`/`Start`/`StartHomelab29`/`HomelabChain`/`Options`/`Run`/`Report`/`BackupReceipt`/`MismatchError`/`Code`;
  - `importSteps`: `lock`, `crosscheck`, `stage`, `unforce`, `authority`, `care`, `liveactivity`, `restore`, `verify`, `drop`, `analyze`;
  - `legacytest.NewLegacy`/`NewLegacyThrough`/`Build`/`Spec` (with `LiveActivity` and `Babies`)/`Legacy`/`Family.IconBabyID`/`Card`/`Restore`/`FilterEnv`/`Fixture`/`Keyring`/`AuthorityTx`/`AppendForeignEvent`, and the `Captured*` types (with `CapturedLiveActivity` and `CapturedClient.IconBabyID`);
  - `testpg.SnapshotCatalog`/`Catalog`/`Roles`/`ProductionShape`/`Superuser`/`TableChecksums`/`NewFrom`;
  - `authority.LegacyChains`/`LegacyEvent`/`AuditRow`/`ChainHead`/`LegacyAnchor`/`CopyAuditRows`/`RegisterAuditTypes`/`VerifyChainsTx`/`TokenDigest`/`LiveActivitySubject`/`WithLiveActivitySnapshot`;
  - `care.Row.CreatedDeviceID`/`CreatedSeq`/`CopyImported`; `protocol.BabyPayload.Icon`; `testkit.Harness.SetBabyIcon`;
  - `migrate.(*Runner).ApplyTx`;
  - `apns.Request`/`Send`/`Preflight`; `config.LoadLiveActivity`;
  - `liveactivity.Service`/`Unregistration`/`Worker`/`Dirty`/`Queue`/`APNSTransport`/`ReadTimerOrigin`/`ReadCompletedSession`; `delivery.Changes`; `testkit.Compose`/`CursorAt`;
  - SQL: `app.live_activity_authorized`, `app.dirty_live_activity_families`, `app.sweep_live_activity_registrations`;
  - script subcommands `drop-legacy-roles`, `delete-ledger`.
- **Review Focus.** Each line has its test in the owning task:
  - 1 in Tasks 16 and 20;
  - 2 in Tasks 4, 18, 26 and 27;
  - 3 in Task 18;
  - 4 in Tasks 5 and 15;
  - 5 in Tasks 14, 17, 20 and 25;
  - 6 in Tasks 8a, 16, 20 and 25, and runbook R5/R8.

### Self-review of revision 3 (writing-plans checklist)
- **The changed fact is applied everywhere.** Every `998501b`, schema-28, `homelab_28` and `homelab28` reference that drives behaviour was searched for. Each now says `fce422d5`, 29, `homelab_29` or `homelab29`, or sits in a committed task's original steps, which that task's amendment block overrides. The remaining `998501b` mentions are deliberate:
  - history: revision 2's pin, the rollback set already on the host, the ledger-drift range;
  - the refused "legacy 28 at `998501b`" case in Tasks 3, 18 and 21.
- **D2 is re-pinned exactly.** `homelab_29` is `fce422d5`'s 29 rows. Row 29's sha256 `8a296b40a8a1aba5e8d4f0256a8bb2aa050c50c8ea30a6d9ae2d6cfd9e087288` was computed from `git show fce422d5:Server/migrations/000029_baby_icon.sql | shasum -a 256`, and equals `fce422d5`'s `compatibility.json`. The re-pin rule covers a further move.
- **Committed work is amended, not rewritten.**
  - Tasks 1, 3, 4 and 6 have numbered follow-up steps, each with its run command and commit message.
  - Task 8 has notes only, because the icon needs no schema change.
  - Tasks 2, 5 and 7 are unaffected: no ledger, audit or APNs code changed between `998501b` and `fce422d5`.
- **The icon port.**
  - It is specified from `main`'s diff, with file and line references for each hunk.
  - The storage decision is justified (wire only; 000032 and its digest untouched; nothing in 000033).
  - Its tests pin the byte-level round trip, the keep-on-update rule, per-mutation rejection and the unchanged hash.
  - The global "no wire changes" constraint is addressed explicitly: this is `main`'s existing wire.
- **Downstream tasks.**
  - 10: the builder at 29, with icons.
  - 13: one connection, `StartHomelab29`.
  - 15: review carries.
  - 16: the icon carried and checked.
  - 17: the enum cast and tenant-context carries.
  - 18: the receipt at 29, the separate `schema_migrations` comparison, icon verification, one connection.
  - 19: `homelab_29`, no `MaxConns ≥ 2`, no doubled error code.
  - 20: `homelab29`, the `Restore` seeded-row rule, keying by `meta.id`, the outsider, the card-scoped start check, the env filter, icons.
  - 21: the legacy compose set, R1 at 29, R2 with both push types, the `server_running` rerun, no LA database variable, icon checks at R5/R8, rollback to `fce422d5`.
  - 22: the P shape with 2 babies.
  - 23: `main` at `7040ca2d`, `000029_baby_icon.sql` removed, the app side merged, V10 protected.
  - 25: icons on the E2E wire.
  - 26: Detect seeding, `already_imported` on the baseline, `first=expected`, no deferred constraints, no icon in the schema.
  - 27: a real `DROP ROLE`, including database grants and memberships.
  - 28: icon gates, the "homelab has not moved" gate, the rehearsal at `fce422d5`.
- **The ledger's carries.** Every carry in `progress.md` up to its last entry has an owner in the carry map, among them the Task 6 and Task 4 review carries and Task 8's fix round 1.
- **Placeholder scan.** The new steps name exact files, commands, expected outputs, literal digests and commit messages. Task 8b's dependent-test list is found by a given `grep` and reported. That is a search with a defined output, not a TBD.
- **Type consistency (new or renamed):** `StartHomelab29`/`"homelab_29"`, `HomelabChain` (29 rows), `Spec.Babies`, `Family.BabyIDs`/`IconBabyID`, `CapturedClient.IconBabyID`, `legacytest.FilterEnv`, `importRun.conn`, `protocol.BabyPayload.Icon`, `Harness.SetBabyIcon`, `Counts["babies_with_icon"]`, `BT_LEGACY_COMPOSE_FILES`, fixture `homelab29`.

