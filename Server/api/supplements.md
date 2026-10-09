# Supplement records

Database schema 18 adds `supplement`, payload version 1. The payload contains `baby_id`, `started_at`, `time_zone_id`, `name`, required `logged_at` and nullable `note`. Names are NFC text, trimmed at mutation decoding, nonblank, at most 64 Unicode scalars / 256 UTF-8 bytes. Notes use the existing 4096-byte limit. No end time or server-side supplement catalog is introduced. Clients derive reusable names from live records, retaining the first spelling for case-insensitive duplicates.

Supplements use ordinary record mutation permissions, quotas, versions, journal sequences, tombstones, snapshots and import content hashes. Child deletion also tombstones supplement records. Snapshot order appends supplements after timers.

The server binary requires exactly database schema 18. Schema 17 binaries must not serve a database containing supplements; the migration compatibility manifest rejects that combination. This is a coordinated migration/restart boundary, not an overlap deployment.

Supplements once had an HTTP support marker, `X-Baby-Tracker-Supplements: v1`, that `/v1/sync` and `/v1/sync/snapshot` required once a family had supplement history, so pre-feature clients could not skip them silently. Pre-feature clients are no longer supported, and the server no longer reads the marker: requests are served the same with or without it. Current clients may still send it; it is ignored.

The content-hash test vector for the shared `supplement.json` fixture is `ada019a1605551c30b8a3926f283b062b0939f229fbc3be9f1133df9ad5307c9` (domain-separated SHA-256, excluding baby ID).

`logged_at` records when the client performs the original logging action, independently of the editable event time `started_at`. It is required, normalized using the existing timestamp rules, and included in the content hash. Preserve it on edits and import; fresh snapshots retain the original value. Updates attempting to change it are rejected with `record_logged_at_mismatch` and do not advance the journal. Direct repository upserts also reject a differing stored value. `accepted_at` remains normal mutable acceptance metadata and must not determine reusable-name logging order.
