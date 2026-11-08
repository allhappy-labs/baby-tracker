# Immunization records

Database schema 33 (`000033_immunizations.sql`) adds `immunization`, payload version 1, as care kind 9 in `care_entities`. The payload contains `baby_id`, `given_at`, `time_zone_id`, `vaccine`, and the always-present, nullable `dose` and `note`. Unknown keys are refused.

- `vaccine` is NFC text, trimmed at mutation decoding, nonblank, at most 64 Unicode scalars / 256 UTF-8 bytes: the supplement name rule. No server-side vaccine catalog exists. Clients derive reusable names from the family's live records, across babies, retaining the earliest shot's spelling for case-insensitive duplicates.
- `dose` is null or a whole number from 1 to 10.
- `note` uses the existing 4096-byte limit.
- `given_at` is stored in `started_at`; the vaccine and dose live only in the stored wire payload, and `label` stays null. The server checks neither that `given_at` is in the past nor that it follows the baby's birth date: the birth date can be edited later, and a server clock check would reject "now" from a phone whose clock runs a few seconds fast. Clients enforce both.

Immunizations use ordinary record mutation permissions, quotas, versions, journal sequences, tombstones, snapshots and import content hashes. Child deletion tombstones them, after measurements and before timers. Snapshots page them after measurements. The owner archive carries them in `immunizations`, after `measurements`.

The server binary requires exactly database schema 33. A schema-32 binary must not serve a database containing immunizations, and this binary does not serve a schema-32 database; the migration compatibility manifest rejects both. This is a coordinated migration/restart boundary, not an overlap deployment.

No capability, header or marker gates the kind, and a change is never optional: a client that does not know `immunization` cannot skip one and must be updated before the family logs its first.

The read-only MCP does not list immunizations.

The content-hash test vector for the shared `immunization.json` fixture is `463b3c456f76db16d2621074d6faff332dd64c13b1d77705f313faff9111f552` (domain-separated SHA-256, excluding baby ID); its mutation hash is `1a166da84b7e1497b8531b9a3c4799996ee6f344787f96e6894671b81d03b85e`.
