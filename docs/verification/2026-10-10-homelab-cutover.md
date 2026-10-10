# Homelab cutover to the PostgreSQL authority — 2026-10-10

The owner approved each step in the conversation (R0 choices; R1; R2; R3; the cutover R4–R5; R6; R7). Runbook: `docs/verification/2026-10-10-homelab-cutover-runbook.md` (formerly `docs/operations/legacy-import.md`).

## R0 choices

- D4: the real-data rehearsal ran on the coordinator's Mac with only `legacy.dump` and `ledger.tar` (no secrets); the copies were deleted afterwards.
- D5: preparation first (R1–R3), then the cutover.
- C4: yes, the Live Activity worker's role and login and its two secrets were dropped at R6.
- C5: background wakes on (`BT_APNS_WAKES_ENABLED=true`).
- C6: the merged app build goes on the phones after the cutover.

## R1 preflight (read-only)

```
preflight: schema=29 digests=match head=1:711 projection=equal mode=self_hosted source=fce422d51ef4fbd4ac98a4bab4e4375ff6e762a6 source_match=match image_revision=fce422d51ef4fbd4ac98a4bab4e4375ff6e762a6 image_revision_match=match apns=set app=running compose=source/Server/compose.yaml,source/Server/deploy/unraid/compose.yaml,source/Server/deploy/unraid/live-activity-push.yaml compose_match=match identity_match=match household=equal
```

Refusal counts 0/0/0, `ledger_actor_id_key=present`, `babies_live=2`, `babies_with_icon=1`, `prekap_lineages=1`, `recovery_active_rotated_count=14`, `recovery_receipt_present_count=0`, database 31.7 MB.

## R2 staging

Merged SHA `3e586cce49fe16196a69a5a8007d0ee79a107559` (archive sha256 `2d5b8d0b…e8ae`, matched on the host). Host preflight `preflight_verdict=pass`; `SERVE: OK`; rollback set `source.fce422d-rollback-3e586cc` and `deployment.env.fce422d-rollback-3e586cc`; `STAGED`; `CONFIG: OK`; `IMAGE: OK`.

```
apns_preflight environment=sandbox push_type=background outcome=pass status=400 reason=BadDeviceToken
apns_preflight environment=sandbox push_type=liveactivity outcome=pass status=400 reason=BadDeviceToken
```

## R3 rehearsal on the real data

Backup `backup.rehearsal-3e586cc` (head 1:712). The first run stopped at its rollback leg: the rehearsal had removed the actor-key binding from its private ledger copy, so the legacy server quarantined (a rehearsal artifact; fixed in `2ba6a3a1`, which also hardens the real rollback). Rerun on the same two files:

```
rehearsal proof: imported=810 verified=1 pulled=skipped icons=1 card_updated=skipped roles_dropped=6 post_r6_ready=1 ledger_deleted=1 rolled_back=1 legacy_ready=1 legacy_live_activity=1 legacy_db_acl=revoked_from_public actor_key=rehearsal_binding post_r8_detected=marker new_db_dumped=1 sign_in_required=1 cleanup=0/0
```

## R4–R5 cutover

- R4: `APP: STOPPED` after the owner force-quit both apps.
- R5.1 backup `backup.cutover-3e586cc`: schema 29, 694 entries, `dump_sha256=11d78d9fc98413e1c7093734b94ceba3cf467590d928c5475edbc5aca88b1fae`, `ledger_tar_sha256=5e41d4238f89a4ac95ecbee25e601b75eb49f99e2387e6e04c40dca431b3da95`, `legacy_state_sha256=951180def3b111f362795e1e13121eee8c909a0ba3507f53138fe4dadb5244fe`, head 1:712.
- R5.2 `result=dry_run start=homelab_29` (2.3 s).
- R5.3 first attempt refused before any change: the host's `jq` 1.6 `fromdateiso8601` applies the local zone, so the minutes-old receipt parsed an hour ahead. Fixed with GNU `date -u -d` (`5ac7e7c5`, and the same line in the staged copy on the host). Then:

```
result=imported start=homelab_29 instance_id=<instance-uuid> epoch=1 legacy_head=1:712:c088ff7ccc6f24ef0f2499462f888f9ad5f83bba5a8592dce9a8e59a0728ceb1 import_id=07cee441-3c14-41b3-b4f8-6526c1bdb3f9 accounts=2 families=1 sessions=11 care_entities=810 care_receipts=0 audit_rows=733 duration_ms=2092 runtime_privileges=verified
```

- R5.4:

```
verify: verify_only=passed import_id=07cee441-3c14-41b3-b4f8-6526c1bdb3f9 authority_failed=0 app=started readyz=200 unraid_verify=passed capabilities=["identity-v1","family-lifecycle-v1","recovery-v1","records-v1","snapshot-v1","timer-commands-v1","import-resolver-v1","live-hints-v1","export-v1","push-token-v1","live-activity-push-v1","refresh-recovery-v1"] babies_with_icon=1 preflight_babies_with_icon=1
```

## R8 phones

`phones-back` recorded. In the first 15 minutes the new server logged only INFO lines; the phones refreshed through `refresh-recoverable` and `refresh-ack`, synced (200), opened the live socket and registered their Live Activity device and activity. The owner: "Everything seems fine".

## R6 and R7

```
roles_dropped=6 roles=baby_tracker_projector_app baby_tracker_apple_revoker_app baby_tracker_live_activity_app baby_tracker_projector baby_tracker_apple_revoker baby_tracker_live_activity
secrets_retired=projector_password projector_database_url live_activity_password live_activity_database_url secrets_kept=none
post_drop: app=restarted readyz=200 capabilities=[…"live-activity-push-v1"…] maintenance_window=25s maintenance_failures=0
delete-ledger: ledger_deleted=1 import_id=07cee441-3c14-41b3-b4f8-6526c1bdb3f9 kept=…/backup.cutover-3e586cc/ledger.tar,…/backup.cutover-3e586cc/legacy.dump
```

R7's `--verify-only` passed after the phones had written (sessions 15, care entities 815, audit rows 737).

## Kept

`backup.cutover-3e586cc` and `backup.rehearsal-3e586cc` on the host (dumps, ledger tars, receipts, reports, retired secrets): kept 30 days, then deleted with the owner's approval. The homelab now runs `baby-tracker-server:3e586cce…`.
