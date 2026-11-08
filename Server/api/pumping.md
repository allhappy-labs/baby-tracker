# Pumping sessions

Database schema 34 (`000034_pumping.sql`) adds `pump`, payload version 1, as care kind 10 in `care_entities`. A pumping session belongs to the family, not a baby: the payload has no `baby_id` and the row's `baby_id` is NULL. The payload contains `started_at`, `time_zone_id`, `side`, and the always-present, nullable `ended_at`, `volume_ml` and `note`. Unknown keys are refused, `baby_id` included.

- `side` is `left`, `right` or `both`.
- `ended_at` is null while the session is running. Otherwise it is not before `started_at` and at most 6 hours after it.
- `volume_ml` is null or a number from 0 to 1000 with at most 2 decimal places.
- `note` uses the existing 4096-byte limit.
- A delete's payload is `{}`. One that names a baby is refused as `invalid_mutation`.
- `started_at` and `ended_at` are typed columns; the side, the amount and the note live only in the stored wire payload.

**The accept-time rule.** A `started_at` or `ended_at` later than the server's accept time plus one minute is `rejected` / `invalid_timestamp`. The minute is the grace a timer edit gets for a phone clock running ahead.

**One running pump per family.** A create or update that leaves a pump running while another pump of the family is running is `superseded` / `pump_running`, with the running pump as `canonical_entity`; nothing is written. The rule is decided under the family lock, and the partial unique index `care_entities_one_open_pump` backs it. A finished session is never in the way. An update that clears the end of a finished session is `rejected` / `pump_reopened`.

**A finished session and a phone that has not synced.** An update whose `observed_entity_version` is behind a session that another device has already finished is `superseded` / `pump_already_stopped`, with the finished session as `canonical_entity`; nothing is written. A phone still showing the session running would otherwise replace the real end and wipe the amount the other parent entered. A phone that has seen the finished session edits it as any record, and a phone's own update after its own Stop (sent before the Stop was answered) goes through.

A baby delete never touches a pump: the cascade selects by `baby_id`, and a pump has none.

Pumps use ordinary record mutation permissions, quotas, versions, sequences, tombstones and snapshots. Snapshots page them last, after immunizations. The owner archive carries them in `pumps`, after `immunizations`. A pump is never an import record: a phone that logged pumps before joining a family sends them as plain creates.

The server binary requires exactly database schema 34. A schema-33 binary must not serve a database containing pumps, and this binary does not serve a schema-33 database; the migration compatibility manifest rejects both. This is a coordinated migration/restart boundary, not an overlap deployment.

No capability, header or marker gates the kind, and a change is never optional: a client that does not know `pump` cannot skip one, so every phone must be updated before the family starts its first pump.

The read-only MCP does not list pumps.

The content-hash test vector for the shared `pump.json` fixture is `337fa8f308f7605672a8f1e8aa228b18feb0f4518c44f42a61eea93fb9aac9aa` (domain-separated SHA-256; a pump has no baby ID to exclude); its mutation hash is `cce6767c7c2e55770663827c23b1bab50c31e773ee80b02a2a955f0bd9fdb6ed`.
