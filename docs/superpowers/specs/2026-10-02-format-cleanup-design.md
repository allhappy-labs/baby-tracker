# Format cleanup and past-entry sessions (follow-up B) — design

Date: 2026-10-02. Status: approved direction (user chose option B on 2026-10-02 and asked for everything except the phone installs to be finished while they are away). Baseline: `main` at `d1daa6a` (client and server code identical to the deployed `2961ca1`).

## Goal

1. Every stored record and every row of sync history is in the current format: feed v4 (components, `feeding_session_id`), timer v5 (`session_id`), sleep v2 (`sleep_session_id`), other kinds v1.
2. Past feeds and sleeps are joined into sessions with the same rule as live tracking (feeds ≤ 30 min, sleeps ≤ 15 min, exactly-at-limit joins, a sleep in between blocks a feed join, solids never join, diapers/meds/growth never block; session id = first part's id; a session belongs to the day of its first part).
3. Compatibility code for older formats, the four feature headers (Supplements, Growth, Timed-Bottle, Feeding-Session), dual old/new storage, and the on-device schema history are deleted on both sides.

## Rollout (user direction, 2026-10-02)

- **Phase S, now:** the server track deploys to production. During the interim Marta's iPhone keeps logging on the installed `2961ca1` build, so the B server must keep that build fully working (headers ignored, its current-format writes and its delete payload versions accepted, joins delivered by normal incremental sync). The owner's iPhone runs a pre-redesign build and is not used for logging; its requests may be refused.
- **Phase C, when ready for final installation:** both phones delete the app and fresh-install the final client. No in-app wipe feature is needed.

## Production facts (read-only probes, 2026-10-02 ~16:55Z)

- Feeds: 49 at component_version 2, 65 at 4; **0 at v1**; 0 v1 feeds without components. 35 feeds already carry `feeding_session_id`.
- Sleeps: 64 at payload_version 1; 0 with `sleep_session_id`.
- Timer rows: 54 at v2, 7 at v3, 38 at v4 (almost all stopped/tombstoned).
- Journal (565 rows): payload shapes are feed v2/3 (72) and v4 (107), timer v2–4 (154), sleep without session key, plus deletes/tombstones and other kinds. **No scalar (v1) feed or `completed_sides` timer shapes.** Legacy operations present: `timer_start_bottle` 7, `timer_switch_side` 16.

## Controller rulings

- **B-R1** Rewrite sync history in place and append one ordinary `update` journal head for every converted or joined live entity (A1). No epoch rotation, no truncation. If wrong: an interim client misses a change → caught by the interim-client test and the post-deploy check on Marta's phone.
- **B-R2** Production holds no v1 feeds, v1 timers or scalar-shaped history, so migration 27 **refuses to run** (raises) if any exist, instead of carrying the v1 component rebuild in design-notes A2 step 3. The dry-run on a restored production dump proves the precondition. If wrong: the migration aborts safely and a rebuild step is added.
- **B-R3** History join uses the live rule (1800 s / 900 s) and only evaluates entries that have no session link yet; existing links (live or Combine/Separate) are kept. If wrong: the user fixes individual sessions with Separate/Combine in History.
- **B-R4** Migration 27 lifts `FORCE ROW LEVEL SECURITY` for its own transaction on the tables it touches and restores it before commit, and its tests run migrations as the same migrator role production uses, so an RLS-filtered no-op cannot pass silently. Postflight counts in the dry-run are the second net.
- **B-R5** Deletes are accepted at any payload version from 1 to the kind's current version, permanently (2961ca1 sends feed deletes at v2 and sleep deletes at v1). Creates, updates and timer commands must equal the current version.
- **B-R6** The server track (S1–S7) is completed, reviewed, fast-forwarded to `main` and deployed **before** the client track starts changing the Swift client, so `make server-protocol-e2e` exercises the exact interim client against the B server.
- **B-R7** Final client: one baseline SwiftData schema (9.0.0); an incompatible existing store is moved aside (never deleted) and the app resyncs.

## Design detail (from the design research, revised for this rollout)

Where this section and the rulings above disagree, the rulings win (notably B-R2 removes the v1 rebuild in A2 step 3).

### Mechanisms, with the code they rely on

### A1. Sync history reset: rewrite in place and append (chosen)

Three options were weighed against what the 2961ca1 client actually does.

**1. Rotate the recovery epoch.** Rejected.
- `recovery rotate-epoch` accepts only `--reason-code database_restore` (`cmd/baby-tracker-admin/main.go:145-176`) and needs a matching `BT_RECOVERY_EPOCH` change.
- Sessions minted under the old epoch fail with `epoch_changed` (`internal/security/transaction.go:265-266`, `auth/refresh.go:51,133`).
- 2961ca1 maps `epoch_changed` to "take a snapshot" (`SyncRetryPolicy.swift:73-74`). But the snapshot request runs on the same stale-epoch session and its refresh also fails, so both phones would need fresh Apple sign-in. That breaks Marta mid-interim.

**2. Truncate the journal and add a history floor that answers `snapshot_required`.** Workable, rejected.
- The journal has no gap detection (`sync/journal.go:13-37` reads `sequence>after`). A floor check would be new server code.
- 2961ca1 would handle it: `snapshot_required` leads to `takeSnapshot()` (`SyncCoordinator.swift:211-228,255-256`). That path is non-destructive: it replaces bases for entities in the snapshot (`BabyStore+RemoteApply.swift:86-113`), leaves other local rows and the outbox alone, and resends rows marked dispatched before the request (`SyncCoordinator.swift:169-174`).
- Costs: it loses all edit history (and so the export journal, `export/repository.go:107-135`), forces a full snapshot, and breaks the receipt foreign keys to the journal (`000009:209-210`).

**3. Rewrite in place and append.** Chosen.
- Every `mutation_journal.canonical_entity` and every `mutation_receipts.outcome` canonical entity is converted to the current shape in place.
- For each live entity that was converted or joined, migration 27 appends an `update` journal row. That row has `entity_version+1`, a sequence of `next_sequence+k`, and is built from the converted record with the snapshot SQL expressions. The record's `last_sequence` is repointed to it.
- Cursors stay valid. The server only checks instance, epoch and family before applying (`sync/paging.go:182-186`), and `after > highWater` cannot trigger because `next_sequence` only grows (`paging.go:279-281`).
- What 2961ca1 does with the appended rows:
  - Its next poll or live-reconnect pulls them through `applySyncPage` (`BabyStore+RemoteApply.swift:62-81`).
  - `applyCanonical` calls `replaceBase`, which overwrites unconditionally (`OverlayMaterializer.swift:32-45`), then rematerializes (`RemoteApply.swift:355-387`).
  - `writeFields` decodes the components (with `feeding_session_id` present it treats the feed as v4) and sets `feedingSessionID` (`RemoteApply.swift:415-446`) and `sleepSessionID` (`:469`). Running timers go through the v5 path (`BabyStore+RemoteTimers.swift:274-286`).
- History is kept, and export and the strict server decoders see only current shapes.
- The owner path, `activateAsOwner` (`FamilySettingsViewModel.swift:639`), imports the store and then syncs with a nil cursor, so the server replays the journal from 0 (`paging.go`: `after=0`). That replay is safe only because every historical row has been rewritten.
- Which path a phone takes depends on what survives (verified by Task 11's `OwnerReinstallE2ETests`):
  - **Fresh install (Phase C).** iOS wipes the App Group defaults and keeps the Keychain. The app starts local-only, and signing in offers no device-continuity proof, so the server allocates a new device. With no `FamilyEntryRecord` for that device, the phone activates as a joiner and takes a snapshot, like Marta's.
  - **Store moved aside (B-R7), defaults kept.** The phone is the same device. It asks for sign-in, then runs `activateAsOwner`: it imports its empty store under a new import ID (the ID includes the store) and replays the journal from 0.

### A2. Migration `000027_format_cleanup.sql`

The migrate runner wraps each file in one transaction (`internal/migrate/migrate.go:108-124`).

**1. Preconditions.** Any failure raises an exception and aborts the whole migration.
- Unfinished imports: `NOT mapping_complete AND expired_at IS NULL AND expires_at > now()`, the same predicate as `app.import_unfinished_session_count` (`000012:250-255`).
- Live `active_timers` with `component_version < 5`. Expected 0; stop timers before deploying.
- After step 3: any live feed with zero components.

**2. RLS bracket.**
- Why it is needed: `compose.yaml:80-81` runs `migrate` with `migrator_database_url`. The migrator is created with no RLS bypass (`deploy/postgres/init-users.sh:120`) but owns the tables, and the tables force RLS (`000002:61,68-69`; `000009:213-231`). The `family_context` policy needs `app.family_id`, so `000017:237-238` most likely matched no rows.
- Run `ALTER TABLE … NO FORCE ROW LEVEL SECURITY` at the start on every table the migration reads or writes:
  - `families`
  - feed_entries, feed_components, feed_side_intervals, feed_foods
  - sleep_entries
  - active_timers, timer_components, timer_side_intervals
  - mutation_journal, mutation_receipts
  - sync_page_sessions, sync_snapshots, sync_snapshot_entities, sync_snapshot_admissions
  - import_sessions
- Turn `FORCE` back on at the end (dropped tables excepted). Nothing outside the transaction ever sees the gap.

**3. Legacy triggers and component rebuild.**
- Drop the five `project_legacy_*` triggers.
- Run `app.rebuild_feed_components` for every `component_version=1` feed and `app.rebuild_timer_components` for every v1 timer.
- This rebuild uses the same rules as the Swift v1 decode the phones display today: sides with `seconds>0`, a bottle only when both volume and contents are set, solids only when `amount` is set, and legacy IDs (`WireModels.swift:121-185`).

**4. Capture what changes.** Record the live legacy rows in temp tables (`ON COMMIT DROP`): feeds below v4, sleeps below v2, timers below v5.

**5. Convert versions.** Set feed `component_version=4`, sleep `payload_version=2`, timer `component_version=5`. Timer `session_id` stays NULL.

**6. Join past entries.** See A3.

**7. Rewrite stored jsonb.**
- Use `pg_temp` converter functions applied to the payload of every journal row and of every receipt outcome's `canonical_entity` and `related_entities[*]`:
  - Feed: keep the embedded `components` if present; otherwise derive them as in step 3 using `app.legacy_component_id`. Strip `sides`, `volume_ml`, `contents`, `foods` and `amount`, and add `feeding_session_id: null` when it is missing.
  - Timer: map `completed_sides` and the pending fields to `completed_components` (derived the same way), and add `current_bottle_started_at` and `session_id` as null when missing.
  - Sleep: add `sleep_session_id: null` when it is missing.
- Rename legacy journal operations (`timer_switch_side`, `timer_pause`, `timer_attach_bottle`, `timer_start_bottle`) to `update`, and narrow the operation CHECK to `create, update, delete, timer_start, timer_finish_bottle, timer_stop`.

**8. Append new journal heads.**
- Applies to every live entity in "converted or joined", ordered by (kind_order, id). Kind order follows `records/snapshot.go`.
- Set `entity_version+1` and `accepted_at=now()`; leave `last_editor` unchanged.
- Insert a journal row with:
  - sequence `next_sequence+row_number`;
  - operation `update`;
  - actor = the last editor, and device = the device of the previous head row;
  - `canonical_entity` built from the current-format branches of `snapshotEntitiesUnion`, copied into the migration.
- Repoint the record's `last_sequence`, and add the appended count to `families.next_sequence`.
- The deferred foreign keys (`000009:194-210`, `000011:70`, `000018:32`, `000024:22,57`) are checked at commit.

**9. Clear transient staging.** Delete all rows from `sync_page_sessions`, `sync_snapshot_entities`, `sync_snapshots` and `sync_snapshot_admissions`. They hold old shapes and page bindings that pin old byte sizes.

**10. Drop legacy storage.**
- Tables: `feed_side_intervals`, `feed_foods`, `timer_side_intervals`.
- Columns on `feed_entries`: `volume_ml`, `contents`, `amount`.
- `active_timers`: drop `active_timers_state_shape`, drop `pending_volume_ml` and `pending_contents`, then re-add the shape constraint without the pending fields.
- Drop `active_timers_session_requires_v5` and `sleep_session_requires_v2`.
- Pin the version columns: `component_version` CHECK `=4` with DEFAULT 4 on feeds and `=5` on timers; `payload_version` `=2` on sleeps.
- Functions: `project_legacy_*` (four), `rebuild_feed_components`, `rebuild_timer_components`, `legacy_component_id`, and `sha1` (only 000017 uses it).

**11. Turn forced RLS back on** (see step 2).

**12. Manifests.**
- The `schema_privilege_manifest` rows for version 27 are the version-26 rows minus the three dropped tables.
- Set `schema_compatibility` to 27, introduced by `000027_format_cleanup.sql`.
- Set `compatibility.json` min=max=27 and add the sha256 for 27.

Leave `storage_bytes` and the usage counters unchanged. They then under-count: each rewritten row changes by a few bytes, and none of the appended journal heads (one per converted, joined or relinked entity: 375 in the seeded rehearsal, 97 in the first production dry-run, under B-R10) is charged to `family_record_usage`, whose only writer is `records/repository.go`. That is roughly 100–200 KB against a 256 MiB family quota.

This migration is not overlap-safe: the 2961ca1 binary allows schema 26 at most. The app must stay stopped during `migrate`, as it was during the 22→26 cutover.

### A3. Past-entry join (PL/pgSQL inside 27)

**Which entries are evaluated.**
- Only legacy entries (feeds below v4, sleeps below v2) whose session column is NULL are evaluated.
- v4 and v2 entries keep the link a client decided live (`SessionJoin.swift:8-27`) or set through Combine or Separate (`FeedingSessions.swift:53-101`).

**Feed rule**, per family and baby:
- Iterate live feeds that are not solids, ordered by (started_at, id). The client also filters solids out (`SessionJoin.swift:12`).
- Keep each session's root and its end time, where the end is the latest `coalesce(ended_at, started_at)` of its members (`projectSessions`).
- For a legacy unlinked feed `f`, the previous session is the one with `end <= f.started_at` and the greatest end; on a tie, the smallest root wins.
- `f` joins it only if all of these hold:
  - the gap from that end to `f.started_at` is at most 1800 s (exactly 1800 s still joins);
  - no live sleep of the same baby has `started_at < f.start` and `coalesce(ended_at, now()) > end`;
  - no live sleep timer of the same baby started before `f`.

  These match `sleepFalls` (`SessionJoin.swift:33-38`).
- A join sets `feeding_session_id = root`.
- Solids never join and are never joined to. Diapers, meds and growth are never consulted.

**Sleep rule:**
- A session counts as closed only when every member has an end.
- The previous session is the closed one with `end <= start` and the greatest end; join when the gap is at most 900 s.

**Session identity and day.**
- Session ID = root = the first part's id (`effective… = link ?? id`, `FeedEntry.swift:53`, `SleepEntry.swift:59`).
- "Belongs to the day of its first part" is a display rule only (`FeedingSessions.swift:46-53`, `SleepSessions.swift:16-23`). Nothing is stored for it.

**Why SQL rather than an admin subcommand:**
- Every changed entity gets a fresh journal row and a repointed `last_sequence`, which are the invariants `records/journal.go:15-60` maintains. So the stale-cursor objection in inventory §5 disappears.
- `Repository.Apply` cannot express "convert stored v1 to v4", because v1 decode is being removed. SQL would be needed for the conversion and rewrite anyway, so an admin path would mean two mechanisms and two deploy steps.
- It is atomic with the schema change and can be dry-run with the existing `migrate` command.
- The data is tiny: about 120 feeds and 65 sleeps.

### A4. Server code after B, as 2961ca1 sees it

**Headers.**
- Stop reading the four feature headers (`httpapi/sync.go:68-71,187,197-218`). Nothing else inspects them, so 2961ca1's headers (`FamilySyncHTTPClient.swift:69-72`) are silently ignored.

**Payload versions.**
- create, update and timer commands must equal the current version for their kind (feed 4, timer 5, sleep 2, others 1).
- Deletes are accepted at any version from 1 to the current one. This is permanent: a delete payload is only `{baby_id}`.
- Files: `protocol/mutations.go:70-94`, `records/validation.go:18-21`; `records/repository.go:93-96,189-193` become dead code and go.

**Family locks.**
- Delete the four feature-check files. Their tests are `sync/*_boundary_test.go` and `httpapi/{growth,supplement}_boundary_test.go`. Move `ErrUnsupportedCapability` elsewhere.
- Add explicit `records.LockFamilyForUpdate` in three places:
  - the mutation transaction (`sync/mutation.go:139-161`), where the feature checks were the only lock;
  - the snapshot continuation (`sync/snapshot.go:29-47`);
  - the sync continuation (`sync/paging.go:251-270`).

**Advertised capabilities.**
- Stop advertising and remove from the known list `timed-bottle-v1`, `growth-v1` and `feeding-session-v1` (`protocol/auth.go:22-24`).
- 2961ca1 reads only `live-hints-v1` and `refresh-recovery-v1` (`EndpointIdentity.swift:68-71`; lines 69-70 are unused). Its request capabilities (`records-v1`, plus `timer-commands-v1` for timer batches) are unchanged (`SyncCoordinator.swift:50-52`).

**Stored data.** Decode and marshal only the current shape: remove the key-sniffing in `protocol/entities.go:332-420` and the v1 hash in `canonical.go`.

### A5. Content hashes, receipts and imports

- **Content hash.** `CanonicalContentHash` collapses to the current formula.
  - The v1→v4 change only matters for unfinished imports, which the migration refuses (A2 step 1).
  - Finished imports are unaffected: manifests are consulted only for mutations that carry an unfinished import's `import_id` (`importer/manifest.go:170-190`).
- **Receipts.**
  - `semantic_hash` covers the mutation, so rewriting `outcome` in place is safe.
  - The `last_sequence` foreign keys stay intact because no journal rows are deleted.
  - Replaying an old mutation ID still decodes under the strict decoder.
- **Client.**
  - A fresh install has no frozen manifests.
  - A fresh-installed owner phone is a new device and takes a snapshot, like a joiner (A1).
  - An owner phone whose store was moved aside imports its empty store under a new import ID and replays the rewritten journal.
  - Joiners take a snapshot (`FamilySyncEngine.swift:241-256`).

### A6. Final client (fresh install)

**Baseline schema.**
- One `VersionedSchema` at 9.0.0 with the legacy `@Model` fields removed.
- The migration plan lists only that schema. SchemaV1–V8, both bridges, the component repair, the recovery fixtures and the fixture builder are deleted.

**Incompatible store found.**
- In the app process only, under `AppGroupBootstrapLock`, rename the store and its `-wal`/`-shm` files to `.incompatible-<timestamp>`, open a fresh store and resync.
- The widget and intents path (`Apps/SharedIntents/WidgetStore.swift:13`) treats an open failure as "unavailable" and never moves files.
- Other open failures keep today's failure screen (`AppEnvironment.swift:180-194`).

**Wire and versions.**
- Use a constant payload version per kind for every operation, deletes included. B accepts this.
- Remove the four feature headers and every legacy decode and hash path.

### A7. Guard for unsynced local data

Phones are deleted and reinstalled, so the guard is operational rather than in the app:
- the migration preconditions and invariants;
- before uninstalling, check that the phone shows "Up to date" and that its last 3 days match a server listing;
- archive the App Group container with `devicectl` before uninstalling. This is mandatory for the owner phone, whose old build may hold quarantined Switch/Pause rows that no server will accept;
- the client's store fallback moves an incompatible store aside and never deletes it.

### Runbook

### Phase S — deploy now

**S-1 Communicate.**
- Owner phone: no logging. Airplane mode until the install is preferred.
- Marta: open the app and see "Up to date" just before the cutover.
- No timers running on either phone.

**S-2 Read-only preflight on production.**
- Run the existing verifier and confirm `/v1/server` reports source `2961ca1` and schema 26.
- Run `format-cleanup-preflight.sql` as `postgres`, which bypasses RLS, and record:
  - row counts per table and per version;
  - v1 feeds that have no components (the 000017 backfill gap);
  - v1 feeds that would end up with no components — must be 0;
  - v1 feeds with foods but no amount;
  - live timers below v5 — must be 0;
  - unfinished imports — must be 0;
  - journal and receipt rows by shape class.

**S-3 Backup.**
- `pg_dump -Fc` to `backup.pre-format-cleanup-<date>.dump`, mode 600.
- Record its sha256 and confirm `pg_restore --list` succeeds.

**S-4 Dry-run on a restored copy.**
- Use `format-cleanup-dry-run.sh`: a disposable `postgres:17.6-alpine` with the same image digest, `init-roles` and `init-users`, the restored dump, then `baby-tracker-admin migrate` from the B image.
- Run the postflight checks. Each must hold:
  - entity counts per table unchanged (babies, feeds, sleeps, diapers, meds, growth, measurements, timers);
  - every feed is v4, every sleep v2, every timer v5;
  - every live feed has at least one component;
  - zero stored payloads with legacy keys, and none missing a session key;
  - journal rows after = before + appended;
  - appended = the number of live converted entities plus joined entities (computed from the preflight);
  - `next_sequence` grew by the appended count;
  - every record's (id, entity_version, last_sequence) exists in the journal;
  - forced RLS is back on; schema is 27; manifest rows for 27 = rows for 26 minus 3.
- Print a per-day session listing for the last 14 days (feeds and sleeps, parts and gaps). The user spot-checks it: joins should appear only where the gap is at most 30 or 15 minutes and, for feeds, no sleep falls in between.
- Approximate size, from the 2026-10-02 cutover: 1 family, 2 memberships, ~114 feeds, ~64 sleeps, ~35 diapers, 3 meds, ~99 timer rows, ~565 journal rows, plus whatever has been added since.

**S-5 Deploy, using the 2026-10-02 procedure.**
- Provision `BT_SOURCE_SHA` and keep the rollback source and config.
- Build the image, stop only the app, and take a cutover dump.
- `compose run --rm --no-deps migrate` (26→27), then `compose up -d --wait app`.
- Verify:
  - `/v1/server` shows the new source and the capabilities without the three removed strings;
  - healthz and readyz return 200;
  - the verifier passes;
  - the bounded log check shows level and status counts only.

**S-6 Post-deploy.**
- Run the postflight checks on production.
- Marta opens the app. Joined sessions should appear in History; spot-check 2–3. She logs normally.
- Optionally log and delete one test entry, to exercise the v2 feed delete.
- Confirm there are no 400 responses from her device.

**S-7 Rollback.**
- If something fails before Marta writes anything: stop the app, restore the cutover dump and redeploy the retained 2961ca1 image.
- After she has written entries: fix forward only.

### Phase C — installs, days later

**C-1 Build.** Build the final client from main after C1–C4, with every gate green.

**C-2 Before uninstalling, on each phone.**
- "Up to date", and the last 3 days match a server SQL listing.
- `xcrun devicectl device copy from` of the App Group container. Confirm the domain-type flag works during the dry-run.

**C-3 Owner phone first.**
- Uninstall, install and sign in.
- Wait for the first sync (a snapshot: the reinstall is a new device, A1), then compare per-day counts with the server.
- Then Marta's phone (joiner, so it takes a snapshot).

**C-4 Physical checks.** Live Activity, joined sessions, a log and a delete on each phone, and the server logs.


###  Risks and open questions, each with a recommended default

1. **Missing components from 000017.** Pre-17 production v1 feeds probably have no components because of the RLS issue in A2 step 2. *Default:* migration 27 rebuilds them; the preflight reports how many were missing.
2. **Live v1 feeds with no content.** *Default:* the migration aborts; the expected count is 0. If it isn't, tombstone those rows explicitly before deploying.
3. **Owner's old build pulls v2 sleeps and v5 timers in the interim and may render them oddly.** *Default:* accept. The owner doesn't log and the phone is wiped at install.
4. **A Marta edit queued offline could overwrite a join (last write wins).** *Default:* she syncs just before the cutover.
5. **Appended rows are stamped with the migration time (`accepted_at=now()`).** *Default:* keep. Feeds, sleeps and timers don't show that timestamp.
6. **Version columns are kept and pinned** to one value rather than dropped. *Default:* keep, which leaves room for a future version.
7. **Delete version rule.** *Default:* permanent; deletes have no versioned shape.
8. **The owner fresh-install path has never run in production.** *Default:* rehearse it in the C4 e2e step before the install day. *Done (Task 11):* a fresh install takes a snapshot as a new device. The empty import plus full replay runs when a store is moved aside. The rehearsal found that path wedged, because the fresh store re-derived the import ID its predecessor had finished and the server refused it (`import_chunk_reused`). It was fixed by adding the store to the ID.
9. **Ready export archives made before the deploy hold old shapes.** *Default:* leave them to expire; they can be regenerated.
10. **Usage counters under-count** by the appended heads plus a few bytes per rewritten row, roughly 100–200 KB. *Default:* accept.
11. **`devicectl` App Group copy may not be supported.** *Default:* check during the dry-run. If it isn't, rely on the "Up to date" check plus comparing the last 3 days.
12. **Migration 27 is not overlap-safe.** *Default:* the app stays stopped during `migrate`, as in the earlier cutover.

