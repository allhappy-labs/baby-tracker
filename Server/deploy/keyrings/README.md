# Retained keyring mount

Docker Compose mounts this directory read-only at `/run/keyrings`. It is empty
by default. Operators can instead set `BT_KEYRING_DIR` to a protected host
directory containing previous-key manifests and their referenced pepper files.
Manifest paths must use the `/run/keyrings/...` container path and secret files
must not be committed. This directory ignores every file except this README and
its `.gitignore`; use an external `BT_KEYRING_DIR` for real deployments.

Identity, session-token, device-continuity, Apple-notification, invitation, and page-token
keyrings use the same strict manifest shape:

```json
[{"key_id":"invitation.v1","pepper_file":"/run/keyrings/invitation-v1.pepper"}]
```

Set the matching variable, such as
`BT_INVITATION_PREVIOUS_KEYS_FILE=/run/keyrings/invitation-previous.json`, and
keep the directory mounted read-only. A manifest may contain at most seven
previous keys. The current key writes new digests; retained keys are read-only.
Page-token rotation uses `BT_PAGE_TOKEN_PREVIOUS_KEYS_FILE`; retain an old page
key for at least the configured page/snapshot TTL plus the backup restore
window so durable continuations remain replayable after restart.
Do not remove an invitation key while any authoritative invitation state still
references its ID, including consumed, revoked, or expired replay tombstones,
or while a supported backup can restore such a reference. Startup readiness
fails closed when an authority reference is missing from the configured
identity, device-continuity, or invitation keyring.
