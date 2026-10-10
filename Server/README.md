# Server foundation

This is the local/homelab server foundation for Apple identity, sessions,
family membership, invitations, account deletion, and family deletion/recovery.
The smoke test proves container startup, four-principal role separation,
explicit migrations, recovery gating, server metadata, challenge creation and
cleanup. It does not prove production readiness, live Apple interoperability,
child-record sync, the hosted credential adapter, or a hosted deployment.

## Verification

From the repository root:

```sh
make server-test
make server-build
make server-smoke
make test
```

`server-test` needs Go, Docker Compose, curl, jq and openssl (the overlap
regression runs the real HTTP shell probes). It creates a uniquely named
disposable Compose PostgreSQL project, generates private test-only credentials,
runs every Go package sequentially
under the race detector with database tests required, and removes that exact
project and its volumes. It refuses an externally supplied
`BABY_TRACKER_TEST_DATABASE_URL`; use the target's owned database rather than
pointing the destructive integration fixtures at a persistent installation.
`server-build` creates an isolated Buildx container builder, compiles the pinned
image for both linux/amd64 and linux/arm64, exports only build cache and removes
that builder. It never pushes. Docker must support the container builder driver.
`server-smoke` needs Docker Compose, curl, jq and openssl. It uses generated
disposable credentials, a unique Compose project and a randomly assigned
loopback port. It removes that exact project's containers, volumes, image and
temporary secret files, and asserts no project containers or volumes remain.
The existing Swift `make test` literal-count gate is unchanged.

## Self-hosted sign-in lifetime

Self-hosted sessions use a 10-minute access token and rotating refresh tokens.
One Apple sign-in lasts up to 365 days, including when the phone is idle; normal
use does not extend that ceiling. Explicit logout, device or membership
revocation, recovery-epoch changes, and refresh-token reuse still invalidate
access. Sessions issued under the former 100-year policy are capped at 365 days
from their original sign-in without an immediate new Apple prompt. Schema 20
adds a read-only lineage start lookup for this enforcement; it does not rewrite
existing session rows or the security ledger.

## Configuration and secrets

The server reads `BT_MODE`, `BT_LISTEN_ADDRESS`, `BT_PUBLIC_ORIGIN`,
`BT_SERVER_INSTANCE_ID`, `BT_RECOVERY_EPOCH`, `BT_APPLE_AUDIENCE`, `BT_IDENTITY_KEY_ID`,
`BT_IDENTITY_PEPPER_FILE`, `BT_SESSION_TOKEN_KEY_ID`,
`BT_SESSION_TOKEN_PEPPER_FILE`, `BT_DEVICE_CONTINUITY_KEY_ID`,
`BT_DEVICE_CONTINUITY_PEPPER_FILE`, `BT_APPLE_NOTIFICATION_KEY_ID`,
`BT_APPLE_NOTIFICATION_PEPPER_FILE`, `BT_INVITATION_KEY_ID`,
`BT_INVITATION_PEPPER_FILE`, `BT_PAGE_TOKEN_KEY_ID`,
`BT_PAGE_TOKEN_KEY_FILE`, `BT_ACTOR_ID_PEPPER_FILE`,
`BT_RATE_LIMIT_PEPPER_FILE`, `BT_MAX_CAREGIVERS`, `BT_MAX_INVITATIONS`,
`BT_INVITATION_TTL`, `BT_MAX_CANONICAL_RECORDS_PER_FAMILY`,
`BT_MAX_CHARGED_STORAGE_BYTES_PER_FAMILY` and `BT_TRUSTED_PROXY_CIDRS`.
`BT_SESSION_SIGNING_KEY_FILE` remains a deprecated alias for
`BT_SESSION_TOKEN_PEPPER_FILE`; setting both is rejected. Supply exactly one of `BT_DATABASE_URL` and
`BT_DATABASE_URL_FILE` (the runtime login). PostgreSQL is the only security
authority (see [`docs/operations/security-state.md`](../docs/operations/security-state.md)):
the settings of the retired file ledger, projector and hosted security
provider are refused in every mode, naming each one —
`BT_SECURITY_LEDGER_DIR`, `BT_LEDGER_HEAD_DIR`, `BT_PROJECTOR_DATABASE_URL`,
`BT_PROJECTOR_DATABASE_URL_FILE`, `BT_DB_PROJECTOR_MAX_CONNS`, every
`BT_HOSTED_*` provider setting, `BT_SECURITY_REPLAY_SCRATCH_PATH`,
`BT_SECURITY_REPLAY_SCRATCH_MAX_BYTES`, and the retired hosted Apple revocation
worker's `BT_APPLE_REVOCATION_DATABASE_URL_FILE`, `BT_APPLE_REVOCATION_WORKER_ID`,
`BT_APPLE_REVOCATION_POLL_INTERVAL`, `BT_APPLE_REVOCATION_LEASE_TTL` and
`BT_CREDENTIAL_ORPHAN_GRACE` (and the inline `BT_APPLE_REVOCATION_DATABASE_URL`).
Remove them from an older deployment's environment. Every database URL file is a nonempty
single line, at most 16 KiB including an optional single trailing LF or CRLF.
Read/validation failures omit its path and contents.
Family export is enabled only when `BT_EXPORT_DATABASE_URL_FILE`,
`BT_EXPORT_KEYS_FILE`, and `BT_EXPORT_KEY_ID` are set together. The first file
contains the dedicated `baby_tracker_export_app` database URL. The key file is
a nonsymlink regular file readable by server UID 65532 with mode 0600 and a
bounded JSON map of key IDs to distinct 32-byte lowercase hexadecimal keys,
for example `{"export.v1":"<64 lowercase hex characters>"}`. The active key ID
must be present. Retain old keys until every archive encrypted with them has
expired and its chunks have been removed. The server advertises `export-v1`
only when this configuration is loaded. The owner may request a JSON archive
once per six hours; ready downloads expire 24 hours after creation. The
download is plaintext JSON, so its destination is chosen on the owner's device.
The six rotating keyrings each accept an optional
`<PREFIX>_PREVIOUS_KEYS_FILE` manifest with at most seven retained read-key
entries. These strict JSON manifests contain absolute pepper-file paths, never
secret bytes. The actor-ID pepper is permanent and has no key ID or previous-key
manifest. It is backup-critical: `server_state` holds its commitment, so
changing or losing it keeps readiness closed with `actor_id_key_mismatch`
instead of allocating different account actors.
All pepper files must be distinct and contain exactly 64
lowercase hexadecimal characters encoding 32 bytes, plus at most one terminal
LF or CRLF. Direct pepper values exist only for development tests and are
rejected in hosted/self-hosted modes.

Compose mounts `${BT_KEYRING_DIR:-./deploy/keyrings}` read-only at
`/run/keyrings`. To retain prior read keys during rotation, put each manifest
and its referenced pepper files in that directory, use absolute container paths
such as `/run/keyrings/identity-v1.pepper` inside the manifest, and set the
matching host variable (for example
`BT_IDENTITY_PREVIOUS_KEYS_FILE=/run/keyrings/identity-previous.json`). The same
pattern is available for session-token, device-continuity, Apple-notification,
invitation, and page-token keyrings (`BT_INVITATION_PREVIOUS_KEYS_FILE`,
`BT_PAGE_TOKEN_PREVIOUS_KEYS_FILE`). `/readyz`
inventory proves that retained session-token, identity, device-continuity,
and invitation keys still cover the key IDs live credentials reference (it is
computed at startup and refreshed every five minutes; a missing key keeps
readiness closed with `credential_key_unavailable`). It does not authorize
notification-key retirement. Invitation
keys cannot be retired while any authoritative invitation state still names
their key ID, including consumed, revoked, or expired invitations retained for
replay protection; backup and restore retention must be considered before
compacting those references. A session-token key is referenced while an
active session of the current recovery epoch using it has not reached the end
of its lifetime (its deadline, its refresh expiry and, self-hosted, one year
from its lineage's sign-in), or while a refresh receipt still recovers such a
session from a parent issued under it, before the receipt's effective
deadline; an abandoned session past its lifetime releases its key even though
its row stays active. Removing a previous session-token key also ends
refresh-reuse detection for consumed refresh tokens issued under it (a replay
is refused as `session_revoked` instead of revoking its lineage), so keep it
through the refresh window when that matters. Retain notification keys
through the accepted 24-hour event-age window plus clock skew and the full
backup/restore replay window. For the beta, keep either class unless an operator
has separately proved those persisted references and windows are exhausted.
Each rate policy can override its documented default with
`BT_RATE_LIMIT_<SCOPE_WITH_UNDERSCORES>_COUNT` and `_WINDOW`, for example
`BT_RATE_LIMIT_AUTH_CHALLENGE_IP_COUNT=30` and
`BT_RATE_LIMIT_AUTH_CHALLENGE_IP_WINDOW=10m`.

Finite workload admission defaults to 32 in-flight requests process-wide.
`BT_ADMISSION_TOTAL` accepts a canonical integer from 1 through 1024;
`BT_ADMISSION_MCP_MAX` defaults to 0 (disabled) and accepts 0 through total minus
one, retaining at least one Sync slot. These configured bounds are not measured
capacity. All finite non-MCP routes, including server information, authentication,
export, and live upgrade setup, share the Sync class. Only `/healthz` and `/readyz`
bypass workload admission; neither does database work per request: `/readyz`, like
every route, reads the cached state of the background readiness monitor (see
"Startup and privilege boundaries").

There is no admission waiting queue. Global saturation returns HTTP 503 with
`server_busy`, `Retry-After: 1` and a retryable error; existing quota refusals
remain HTTP 429. A token covers body reads (except on the body-first routes
below, which read their body before taking one) and response writes until the
handler finishes, including canceled work until it stops. The existing transport
deadlines remain necessary to unblock slow network reads. Successful WebSocket
Accept hands capacity back after the real upgrade; the Hub separately bounds
socket lifetimes and device/account/process reservations.

The body-first routes read their whole request body before the credential is
checked, so no transaction waits on the network: `POST /v1/sync`, `POST
/v1/family/import/resolve`, `POST /v1/family/import/{id}/resolve-conflict` and
`PUT`/`DELETE /v1/devices/push-token`. Any well-formed bearer reaches that read,
so these routes take their admission slot only after the body is buffered. The
read itself is bounded on its own: at most 8 bodies in flight per client address
(an IPv6 address counts as its /64) and 64 in all, each cut after 5 seconds. A
request beyond either bound answers 503 `server_busy` before any byte of its
body is read. A body cut short (the 5-second deadline, or a connection closed
mid-body) ends the request before its credential is checked: it answers 503
`service_unavailable`, as a stalled body did under the 15-second read timeout,
with no statement run. Each request on these routes whose bearer matches no live
session (401 `session_revoked`), and each body cut short, counts against its
client address: after 60 in 10 minutes
(`BT_RATE_LIMIT_BEARER_FAILURE_IP_COUNT`/`_WINDOW`) the address is refused with
429 `rate_limited` before its body is read or any statement runs, until the
window ends. An oversized body is not counted. Slow or forged bodies from one
address therefore hold no admission slot and cost at most that budget of body
reads and decodes, so an address keeps on average under one of the 64 body-read
slots busy.

With `BT_SYNC_DIAGNOSTICS=1`, capacity snapshots include accepted, rejected,
released and active counts for fixed `sync` and `mcp` classes. The `/mcp`
handler takes its one MCP slot itself from the same controller and is never
wrapped in Sync admission. `BT_ADMISSION_MCP_MAX` must be 0 unless private MCP
is enabled (see "Private MCP endpoint").

Live WebSocket admission uses the fixed one-minute count overrides
`BT_RATE_LIMIT_LIVE_ATTEMPT_IP_COUNT`,
`BT_RATE_LIMIT_LIVE_ATTEMPT_DEVICE_COUNT`,
`BT_RATE_LIMIT_LIVE_ATTEMPT_ACCOUNT_COUNT`, and
`BT_RATE_LIMIT_LIVE_ATTEMPT_FAMILY_COUNT` (defaults 60, 12, 36, and 120;
each range 1 through 10,000). Socket concurrency uses
`BT_SOCKET_DEVICE_LIMIT`, `BT_SOCKET_ACCOUNT_LIMIT`, and
`BT_SOCKET_PROCESS_LIMIT` (defaults 1, 3, and 1,000).
Family admission defaults to 10 active caregivers (including the owner), 10
outstanding invitations, and a seven-day invitation lifetime. Operators may
override these with `BT_MAX_CAREGIVERS` (1 through 100),
`BT_MAX_INVITATIONS` (1 through 1000), and `BT_INVITATION_TTL` (a whole-second
duration from 1h through 720h). These limits are enforced inside the
authoritative family command path rather than only at the HTTP boundary.
Record storage defaults to 100,000 canonical feed/sleep/diaper records and
268,435,456 charged bytes per family. Operators may configure the count from
100 through 1,000,000 with `BT_MAX_CANONICAL_RECORDS_PER_FAMILY`, and charged
storage from 16,777,216 through 4,294,967,296 bytes with
`BT_MAX_CHARGED_STORAGE_BYTES_PER_FAMILY`. Charged bytes cover each entity's
latest canonical state (tombstones included) with deletion headroom while it
is live, retained import staging, and every mutation receipt until the
retention worker deletes it 30 days after its acceptance, releasing its
charge; the effective values are returned by `GET /v1/server`. Tombstoned records stop
using record-count slots, but retained tombstones and receipts remain charged
bytes. `docs/protocol/v1.md` gives the exact charges.

A family its owner deleted is purged by the retention worker once its
30-day recovery window ends, in bounded, resumable steps every 30 s. The
purge deletes the family's care records and babies, import staging,
mutation receipts, continuation bindings, snapshot sessions, device
cursors, member display labels and their history, memberships and the
family's recovery claim, zeroes its usage counters and, on completion,
clears the family's own display name. It keeps the family row (its ID,
status, purge receipt and `next_sequence`; its display name is cleared),
its invitation rows (IDs, roles and token
digests, so a retried redemption still finds its receipt), the security
audit chains of the family and of its former members' accounts (opaque IDs,
roles, digests and times: no audit row carries a member's or the family's
display name; an operator's MCP integration grant records the integration's
name; the chains still verify), and command receipts (a hash and a result
code). Its members' sessions were revoked when the deletion was scheduled,
and the session sweep prunes them. A family whose purge
keeps failing (for example a failed audit chain, or a statement that times
out even at the smallest batch) is skipped for an hour, logged at error
level, and then tried again; the others are still purged. At most 1,000
families are skipped at once (the oldest is tried again to make room).

The hosted Apple revocation worker and its credential vault are removed: the
Apple refresh token from a hosted code exchange is not persisted, and account
deletion reports `apple_authorization_revocation: "manual_required"` in every
mode. Its settings are refused (see above).

Compose additionally reads `BT_SECRETS_DIR`, `BT_IMAGE_NAME` and `BT_HTTP_PORT`.
The secret directory must contain these externally provisioned files:

| File | Consumer |
| --- | --- |
| `bootstrap_password` | PostgreSQL init administrator only |
| `migrator_password` | PostgreSQL principal bootstrap |
| `runtime_password` | PostgreSQL principal bootstrap |
| `export_password` | PostgreSQL maintenance principal bootstrap (`baby_tracker_export_app`, member of `baby_tracker_maintenance`) |
| `migrator_database_url` | One-shot admin service: `migrate`, `authority init`, `authority verify`, `authority rotate-epoch` |
| `runtime_database_url` | Application service and operator commands |
| `maintenance_database_url` | Application service: the maintenance pool (required in every mode; authenticates as `baby_tracker_export_app`) |
| `identity_pepper` | Versioned Apple identity lookup HMAC |
| `session_token_pepper` | Versioned opaque session-token HMAC |
| `device_continuity_pepper` | Versioned device-continuity HMAC |
| `apple_notification_pepper` | Versioned Apple-notification replay HMAC |
| `invitation_pepper` | Versioned invitation-token HMAC |
| `page_token_key` | Versioned opaque sync/snapshot page-token HMAC |
| `actor_id_pepper` | Permanent actor-ID HMAC and authority commitment |
| `rate_limit_pepper` | Ephemeral abuse-bucket HMAC |

Database URL files must authenticate as `baby_tracker_migrator` and
`baby_tracker_app`, respectively, against database `babytracker` on service
`db`. No projector or Apple-revoker credential is mounted any more: the
schema grants those roles nothing and the bootstrap no longer creates them
(the homelab's cutover drops its old ones, runbook R6). Both are
authority-equivalent. The runtime login writes accounts, sessions,
memberships and audit rows directly, and `app.authority` is a
transaction-local setting, not a privilege. Protect and rotate
`runtime_database_url` like the migrator URL (see
[`docs/operations/security-state.md`](../docs/operations/security-state.md#trust-boundary)).
Provision matching passwords and percent-encode URL credential components as
needed. Never commit secrets. Compose mounts these files rather than putting
credentials in environment values or command arguments. Local Compose file
secrets are bind mounts: ensure the PostgreSQL user can read its files and UID
65532 can read the application's files. The smoke directory is private (0700)
with container-readable files; use a suitable secret manager/permissions policy
for a persistent installation. No Apple private key is embedded or provisioned,
so no production Apple code exchanger exists: hosted mode refuses to start with
`hosted_adapter_unavailable` (only identitytest builds with
`BT_IDENTITYTEST_HOSTED_ADAPTER=1` compose the test exchanger). The
`hosted_credential_rotation_unverified` gate is retired with the credential
vault. Self-hosted mode constructs no hosted Apple transport.

### Background push (optional)

A server can wake backgrounded phones with a contentless Apple background
push (`{"aps":{"content-available":1}}`; no family data, names or counts).
Push is optional in every mode:

| Variable | Meaning |
| --- | --- |
| `BT_APNS_WAKES_ENABLED` | `true` turns push on. Unset or `false` (the default) is off: wakes go through the live hub only, `push-token-v1` is not advertised, and no other `BT_APNS_*` variable is read. Any other value refuses to start. |
| `BT_APNS_TEAM_ID` | The Apple developer team ID (ten upper-case letters and digits). |
| `BT_APNS_KEY_ID` | The ID of the APNs signing key (ten upper-case letters and digits). |
| `BT_APNS_BUNDLE_ID` | The app's bundle ID, which is the push topic. |
| `BT_APNS_KEY_FILE` | A mounted `.p8` file: a regular, non-symlink file with mode 0600 holding one P-256 PEM private key. It is read at startup (never per push) and never logged. |

When `BT_APNS_WAKES_ENABLED=true` (or `BT_LIVE_ACTIVITY_PUSH_ENABLED=true`,
below), all four are required and each is
validated at startup; an error names the variable, never its value. The
server signs an ES256 provider token from the key, reuses it for 40 minutes
and talks to Apple over one reused HTTP/2 connection per APNs environment.
The operator provides these credentials at deploy; they are never committed
to this repository, printed, or used by tests, which generate throwaway keys
(`scripts/apns_secrets_test.go` fails on any tracked key file or production
identifier under `Server/`). Wiring them into the Unraid deployment is
Phase 6.

With push on, phones register their APNs token through `PUT` and `DELETE
/v1/devices/push-token` (`push-token-v1`, see `docs/protocol/v1.md`); with
push off the route is not registered (404). The route is limited to 10
requests a device and 30 an account per hour, overridable with
`BT_RATE_LIMIT_PUSH_TOKEN_DEVICE_COUNT`/`_WINDOW` and
`BT_RATE_LIMIT_PUSH_TOKEN_ACCOUNT_COUNT`/`_WINDOW`.

#### Live Activity push

| Variable | Meaning |
| --- | --- |
| `BT_LIVE_ACTIVITY_PUSH_ENABLED` | `true` turns Live Activity pushes on. Unset or `false` (the default) is off, and no `BT_APNS_*` variable is read for it. Any other value refuses to start. |

Live Activity pushes share the four `BT_APNS_*` variables above, with the
same validation; the topic is derived as `<BT_APNS_BUNDLE_ID>.push-type.liveactivity`.
Wakes and Live Activities go through one APNs client, built once when either
switch is on. The Live Activity worker runs on the maintenance credential
(`BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE`): `main`'s separate worker login
is not used, and a set `BT_LIVE_ACTIVITY_DATABASE_URL_FILE` or
`BT_LIVE_ACTIVITY_DATABASE_URL` refuses to start until it is removed.

The worker runs two runners on a one-second poll. Each drains due work, up
to 32 claims per pass, before it waits for the next poll. At most two sends
are in flight, and at most one per card target, because each job is leased
alone. A minute tick sweeps authority even at full capacity. Each job ends
in one of three ways:

- A job that fails on its own data is parked with `last_reason`
  `dispatch_failed`. It backs off (2, 4, 8, 16, 32, then 60 seconds, with
  jitter) while the rest of the queue keeps delivering.
- A job that lost a database race (a serialization failure or deadlock,
  SQLSTATE class 40) is released as `transient_conflict`. The first loss gets
  the shortest backoff, and a job that keeps losing moves along the same
  progression. It is never parked as `dispatch_failed`.
- Only a dependency failure (the database or readiness) stops the worker for
  supervision.

Every send attempt keeps one `live_activity_history` row (7-day retention).
The row holds the event and APNs' answer: the class (`accepted`,
`invalidToken`, `retryable`, `configurationFailure`, `ambiguous`), the HTTP
status and an allowlisted APNs reason. It never holds a token, payload or care
content. The job's `last_reason` holds the latest non-accepted class. Every
send APNs does not accept also logs one `live activity push not accepted`
line. The line has the event, kind, class, status, reason and attempt, and
whether the request was sent at all.

APNs answers clear tokens like this:

- A wrong bundle ID or key scope (`DeviceTokenNotForTopic`,
  `TopicDisallowed`, `BadTopic`) is a configuration failure and clears no
  phone's token.
- `BadDeviceToken` (including a token from the other APNs environment) and
  `Unregistered` clear that token.

A start the transport rejects before any byte reaches APNs is known-unsent.
It is released (status `pending`, no attempt ID) for a fresh attempt after
backoff. It does not wait start-unconfirmed for a phone's proof. A start that
may have reached Apple keeps the ambiguity rules and is never resent blind. A
stale lease the worker cannot release is logged once as `live activity stale
lease not released`, with a fixed code.

Starts, ends and updates that change a card's controls (phase, activity kind
or write access) go at APNs priority 10. Content-only edits to an active card
in the same phase go at priority 5, and a retry keeps the priority of the send
it repeats. A phone's cards are
read in byte order (`COLLATE "C"`), the order Swift's `String` comparison
gives. A phone that starts a timer may register its own card before the
worker has seen the start. Such a card is adopted only when it is active,
has a token and was not retired by the server. Nothing is adopted while an
earlier lifecycle's card still awaits its end. An ended or dismissed card
that still names the timer or session belongs to an older lifecycle (a
swiped break card, the same feed session resumed later), so it cannot freeze
the phone's new card.

#### Checking the credentials: `apns-preflight`

Before turning either switch on, check the key against Apple, once for each
push type you enable:

```sh
baby-tracker-server apns-preflight --environment sandbox     # development-signed builds
baby-tracker-server apns-preflight --environment production  # TestFlight and App Store builds
baby-tracker-server apns-preflight --environment production --push-type liveactivity
```

It reads the `BT_APNS_*` variables exactly as startup does, sends one push to
an all-zero device token no phone holds, and prints one line:

```
apns_preflight environment=<env> push_type=<type> outcome=<outcome> status=<n> reason=<reason>
```

`--push-type` is `background` (the default: a wake's contentless push at
priority 5 to the bundle ID) or `liveactivity` (a quiet Live Activity update
with no alert and no care content, at priority 10, to
`<BT_APNS_BUNDLE_ID>.push-type.liveactivity`). Apple's answer is classified
the same way for both:

| Outcome | Apple's answer | Meaning |
| --- | --- | --- |
| `pass` | 400 `BadDeviceToken` | The key, team ID and key ID were accepted. A dummy token cannot prove the topic; the first real push does. |
| `bad_key` | 403 `InvalidProviderToken`, `BadEnvironmentKeyIdInToken` or `UnrelatedKeyIdInToken` | The key, team ID or key ID is wrong, or the key is not enabled for this environment. |
| `topic` | 400 `DeviceTokenNotForTopic`, `TopicDisallowed` or `BadTopic` | The bundle ID is wrong for this key. |
| `unreachable` | none | Apple could not be reached or its answer was lost; `reason` is the coarse cause (`dns`, `tls`, `connection`, `timeout`). |
| `unexpected` | anything else | Reported with Apple's status and reason. |

It exits 0 only for `pass`, 1 otherwise (a configuration error names the
variable, never its value) and 2 for a usage error. It never prints a token,
the key, the key ID or the provider token.

A wrong server clock also produces a refused provider token: `bad_key`
(`InvalidProviderToken`) or `unexpected` (`ExpiredProviderToken`) can mean the
host's clock is off rather than the key being wrong, so check the clock (NTP)
before replacing a key.

## Private MCP credentials (operator)

The admin tool issues, lists and revokes private read-only MCP credentials
through prepared operator commands, each committed as one PostgreSQL authority
transaction. The
family is disabled unless `BT_OPERATOR_INTEGRATION_POLICY_ID_FILE` names a
mounted mode-0600 regular, nonsymlink file holding the policy ID, alongside
`BT_OPERATOR_ID` and `BT_OPERATOR_CONFIRMATION_KEY_FILE`. A direct
`BT_OPERATOR_INTEGRATION_POLICY_ID` value is refused. Without the file every
`mcp` command fails with `operator_policy_required`, before any secret file is
written. Other operator policy files do not enable it. Hosted mode is refused
with `hosted_adapter_unavailable`.

```sh
baby-tracker-admin mcp issue prepare --command-id <uuid> --family-id <uuid> \
  --account-id <owner-uuid> --name <client-label> --scope family:read \
  --server-instance-id <uuid> --recovery-epoch <n> --validity-seconds 7776000 \
  --policy-id <policy> --evidence-reference <ref> --reason-code integration_issued \
  --expected-revision <family-revision> --secret-output /private/dir/<file>
baby-tracker-admin mcp issue commit <the same proposal flags> --grant-id <uuid> \
  --verifier <hex> --secret-file /private/dir/<file> \
  --confirmation-digest <hex> --confirmation-expires-at <RFC3339>
baby-tracker-admin mcp revoke prepare --command-id <uuid> --grant-id <uuid> \
  --family-id <uuid> --account-id <owner-uuid> --server-instance-id <uuid> \
  --recovery-epoch <n> --policy-id <policy> --evidence-reference <ref> \
  --reason-code integration_revoked --expected-revision <family-revision>
baby-tracker-admin mcp revoke commit <the same flags> \
  --confirmation-digest <hex> --confirmation-expires-at <RFC3339>
baby-tracker-admin mcp list --family-id <uuid>
baby-tracker-admin authority family-revision --family-id <uuid>
```

Every flag is required exactly once. `--account-id` must be the family's
active owner. The instance ID and epoch must equal the server's
`server_state`. `--expected-revision` is the family's audit sequence, printed
by `authority family-revision` (run with the application's configuration,
behind the same readiness checks); a proposal prepared at an older revision is
refused with `stale_security_revision`. `--name` is a client label such as `claude-desktop-laptop`:
1–64 bytes of ASCII letters, digits, space, `.`, `_` or `-`, with no leading or
trailing space. The label is kept forever in the family's append-only audit chain, so
**never put a person's or baby's name in it**.

- **`issue prepare`** generates a random grant ID and a 32-byte secret once.
  It preflights the proposal, then creates `--secret-output` exclusively with
  mode 0600. The path must be absolute and clean, and must not exist. Its
  directory must be a real, non-symlink directory owned by the operator with
  no group or other access (0700).
  - Stdout prints only the proposal (grant ID and verifier included), the
    confirmation and the file path.
  - The credential is never printed and never passed as a flag.
  - The credential is inactive. The confirmation expires after five minutes;
    it is not the credential's expiry.
- **The secret file** contains exactly the credential a desktop client
  presents, and nothing else (no trailing newline): the 87 ASCII bytes
  `btmcp1.<lowercase grant UUID>.<unpadded base64url of the 32 secret bytes>`.
  The server stores only the verifier,
  `hex(SHA-256("babytracker-mcp-v1\x00" || grant ID || secret))`.
- **`issue commit`** checks the secret file. It must be a regular,
  non-symlink, operator-owned mode-0600 file in such a directory, and its
  grant ID and verifier must match `--grant-id`/`--verifier`
  (`secret_file_mismatch` otherwise). Only then does it commit the confirmed
  proposal.
  - The grant becomes active at the committed authority event. It expires
    exactly 7,776,000 seconds (90 days) later.
  - The tool never regenerates, rewrites or deletes the secret file.
- **Retrying.** If the commit outcome is lost (output gone, process killed,
  projection failure), rerun the identical commit command with the same file.
  - An already committed command returns its original receipt, even after the
    confirmation or the grant has expired. It never issues a second grant or
    changes the stored times.
  - If nothing was committed and the confirmation has expired
    (`invalid_confirmation`), the file is an inactive credential. Destroy it
    and prepare again.
- **Replacement** is explicit: issue a new credential, then revoke the old
  grant. There is no refresh token. Revocation is final
  (`integration_revoked` on a second attempt).
- **`list`** prints the family's grant metadata only: ID, account, family,
  instance, epoch, label, scope, status, created and expiry times. It never
  prints the verifier.

Refusals print a stable code first, then an explanation. The codes are
`operator_policy_required`, `invalid_request`, `invalid_confirmation`,
`command_id_reused`, `stale_security_revision`, `epoch_changed`,
`invalid_command` (including an already used grant ID), `family_unavailable`,
`owner_required`, `integration_invalid` (no grant matches the given bindings),
`integration_revoked`, `integration_verifier_reused`, `service_unavailable`,
`security_busy`, `secret_file_unsafe`, `secret_file_exists`,
`secret_file_invalid` and `secret_file_mismatch`.

## Private MCP endpoint

`/mcp` is disabled by default: no route (404), no MCP database pool and no MCP
secret is read. It is a private, tailnet-only, read-only endpoint with three
tools (`list_babies`, `get_history`, `get_summary`). Hosted mode refuses it.
Enabling it requires every row below; any missing or inconsistent value stops
startup.

| Setting | Enabled MCP | Disabled MCP |
| --- | --- | --- |
| `BT_MCP_ENABLED` | `true` | empty or `false` |
| `BT_MCP_DATABASE_URL_FILE` | runtime-role URL file, e.g. `/run/secrets/runtime_database_url` | not read |
| `BT_MCP_CURSOR_KEY_FILE` | 64 lowercase hex characters (`openssl rand -hex 32`) under `/run/mcp` | not read |
| `BT_ADMISSION_MCP_MAX` | 1–4, below `BT_ADMISSION_TOTAL`, and **equal to** `BT_DB_MCP_MAX_CONNS` | 0 (default) |
| `BT_DB_*` profile | complete explicit profile, `BT_DB_MCP_MAX_CONNS` 1–4 and **equal to** `BT_ADMISSION_MCP_MAX`, runtime + maintenance + MCP ≤ server − reserved | `BT_DB_MCP_MAX_CONNS` 0 or legacy |

- **Pool.** The MCP pool is opened separately from its own URL file (never the
  runtime pool object) with exactly `BT_DB_MCP_MAX_CONNS` connections and
  `default_transaction_read_only=on` on every session, and it counts under
  the identity `mcp` in the aggregate budget that readiness checks. It uses
  the runtime tenant role, which keeps `SELECT` on `server_state` for history
  cursors. Readiness never pings the MCP pool, so an MCP database problem
  cannot make phones unready.
- **Admission.** Each `/mcp` request takes exactly one global `mcp` slot of
  the shared controller and never a Sync slot, so at least
  `BT_ADMISSION_TOTAL − BT_ADMISSION_MCP_MAX` slots stay reserved for phones.
- **Per credential.** After authentication and before the body is read, every
  authenticated request (metadata and notifications included) takes the
  credential's single in-flight slot (else 429 `concurrency_limited`), then
  charges the quotas below together (else 429 `rate_limited` with
  `Retry-After`). The in-memory registry holds only authenticated grant IDs,
  at most `BT_ADMISSION_MCP_MAX` of them, and forgets a grant when its request
  ends.
- **Repeat refusals.** After an authenticated `rate_limited` or
  `concurrency_limited` refusal, the same exact credential string (keyed by its
  SHA-256, never by grant ID) is refused again with the identical 429 before
  global admission and authentication: until the limiter's retry time for
  quota, and only while the credential's request is still in flight (at most
  one second) for concurrency. At most 1,024 entries, only for authenticated
  credentials; a cached entry never admits. A credential revoked during that
  window keeps getting 429 until it ends (at most the one-minute quota window,
  plus up to a second of `Retry-After` rounding), then 401. A 401 is never
  cached: every request with an invalid, revoked, expired or well-formed
  garbage token pays the full authority verification before it is rejected.
- **Access log.** Repeat refusals and pre-authentication `server_busy` are not
  logged one line each: a `requests refused summary` line per route and status
  (with `count` and `window_seconds`) is written at most every 10 seconds while
  they continue. Admitted and authenticated outcomes, and every 401/403, keep
  their own `request completed` line and request ID.
- **Quotas** (fixed one-minute windows, existing limiter, runtime pool):
  `mcp-integration` 30, `mcp-account` 60, `mcp-family` 120. They can only be
  lowered, with `BT_RATE_LIMIT_MCP_INTEGRATION_COUNT`,
  `BT_RATE_LIMIT_MCP_ACCOUNT_COUNT` and `BT_RATE_LIMIT_MCP_FAMILY_COUNT`.
  Reservation rows are operational metadata, never care records.
- **Revocation under load.** MCP reads hold the shared authority fence for
  their transaction. They yield to a pending security write (in this or the
  admin process), so a revocation waits only for reads already running, each
  bounded by the five-second request deadline. Phone sync reads do not yield.
- **Proxy and Host.** `Host` must equal `BT_PUBLIC_ORIGIN`'s host exactly;
  forwarding headers are ignored. The Unraid topology (Tailscale Serve →
  `http://baby-tracker-sync-app-1:8080`, the app container's name, on the compose network) is **expected** to preserve Host so the
  app listens on its container address and the SDK's localhost DNS-rebinding check
  stays enabled — this is a source-level assumption, not yet verified against the
  real reverse proxy; see [`Server/docs/mcp.md`](docs/mcp.md) §5.6 step 4 and
  [`docs/verification/2026-10-03-mcp.md`](../docs/verification/2026-10-03-mcp.md)
  Limitations, and confirm it at rollout before issuing any production credential. A
  deployment that listens on loopback behind a same-host proxy would get 403 and is
  not supported.
- **Telemetry** (`BT_SYNC_DIAGNOSTICS=1`): admission counts appear under the
  fixed `mcp` class, the pool under the fixed `mcp` role, and MCP guard
  timings (authority, database, transaction acquire, fence, integrity) in a
  separate `mcp_capacity` snapshot. `capacity.phases` and `integrity_bytes`
  stay sync-only.
- **Measured local limits** ([evidence](../docs/verification/2026-10-03-mcp-mixed-load.md)).
  These were measured on a local laptop, not the Unraid node, at a reduced
  phone load: 32 devices at 4 writes per writer per minute, not the Track A
  rate-15 anchor. Phone sync stayed healthy with up to four paced clients at
  20 calls a minute each. It also stayed healthy with one unpaced client using
  a **valid** credential (one credential, two concurrent workers), which is
  refused cheaply by its own gate or quota. Both held at MCP concurrency 4 and
  1; 2 and 3 were not measured. 1 is the conservative choice.
- **Not supported.**
  - Unpaced clients with invalid, revoked or expired credentials, and floods of
    well-formed garbage tokens. These pay full verification per request and
    are expected (unmeasured) to load the app like the earlier busy-client
    measurement: +194% sync write p95 at concurrency 4.
  - A high-rate connection flood (about 600 new connections a second from four
    credentials). It breaks phone sync at the shared TLS proxy in front of the
    app (proxy CPU and upstream ports), which no in-app limit can protect.
- **Ingress requirement.** Tailscale Serve has no connection or rate limit. A
  deployment that must survive a misbehaving tailnet client needs a
  connection/rate limit in front of the app, for example a proxy between
  Tailscale Serve and the app. Revoking a misbehaving client's credential does
  not reduce its load: after at most a minute of cached 429s, each request
  takes the expensive path to a 401. Block the client at the ingress or with
  tailnet ACL/policy, and revoke as well. Cheap rejection of invalid
  credentials before full authority validation is a recommended follow-up.

See [`Server/docs/mcp.md`](docs/mcp.md) for the operator secret-file checklist,
desktop setup (Claude Desktop's pinned bridge, Codex/ChatGPT's native client),
rollout notes (source/image identity, the migration 000028 rollback constraint,
protected files, backup/restore, enable/disable) and a concrete rollout proposal
awaiting separate approval. See
[`docs/verification/2026-10-03-mcp.md`](../docs/verification/2026-10-03-mcp.md) for
the end-to-end proof, gate results, soak analysis and desktop-client validation
behind this section.

## Database connection budgets

Leaving all five values below empty selects legacy pgx defaults and DSN pool
settings. The injected configuration getter cannot distinguish unset variables
from explicitly empty ones, so five explicit empty strings also select legacy
mode. Any nonempty value selects an atomic explicit profile: all five must then
be valid nonempty integers. `BT_DB_PROJECTOR_MAX_CONNS` is retired (there is no
projector pool) and refused. Whitespace is invalid. For a laboratory profile:

| Variable | Value |
| --- | ---: |
| `BT_DB_RUNTIME_MAX_CONNS` | 16 |
| `BT_DB_MAINTENANCE_MAX_CONNS` | 2 |
| `BT_DB_MCP_MAX_CONNS` | 0 |
| `BT_DB_SERVER_MAX_CONNECTIONS` | 64 |
| `BT_DB_RESERVED_CONNECTIONS` | 8 |

Runtime needs at least two connections: with one, the live hub's periodic
authorization check and the authority's own reads would queue behind every
request transaction, and the check's 5 s deadline would then close every live
socket. No connection is held for live updates: there is no LISTEN/NOTIFY
(single node). Live hints are published in
process, after each write commits, to the connections this server holds,
coalesced per family over 50 ms. The authority runs on the runtime pool.
The maintenance credential is required in every mode, and one maintenance
connection is reserved for the readiness audit (maintenance workers run inside
`database.WorkerSlots`, one slot fewer than the pool), so an explicit profile
needs a maintenance budget of at least 2; an export credential with its own
URL opens a second maintenance pool of the same size. Two fixed one-connection
pools on the runtime credential always count too: the readiness pool (the
readiness monitor alone, so nothing can starve it) and the background pool
(the audit verifier, the key inventory and the authority sweeps, one at a
time, the inventory half an interval out of phase with the verifier). `BT_DB_MCP_MAX_CONNS` must be 0 unless private MCP is
enabled; then it is 1–4 and counts as its own pool (see "Private MCP endpoint").

Explicit mode rejects a pool total above `server max - reserved` (56 here).
Runtime 16 + readiness 1 + background 1 + one maintenance 2 totals 20. The
readiness audit (at startup, after a schema change or an outage, and every five
minutes, on the readiness pool)
also verifies PostgreSQL's actual `max_connections` is at least the declared
server maximum, alongside the migration and role checks. Pool construction
remains lazy. Startup logs only numeric effective connection budgets.
These settings bound this application's occupancy; operators must include
other applications and administrative workers in their server reservations.

Both URL and keyword/value pgx DSNs retain their other connection/pool controls.
A supplied `pool_max_conns` must agree with the explicit maximum; parser and
conflict errors omit credentials. Legacy DSN ceilings remain unchanged.

The private capacity runner defaults to legacy mode. Select the complete fixed
laboratory profile with `--database-profile explicit`; use
`--database-profile legacy` for its matched baseline. `run.json` and cells record
the selected mode and explicit numeric settings; the runner never forwards
ambient database/profile configuration. Keep `--metrics on` for both cells.
Older evidence without these fields retains its existing meaning. Connection
bounds alone do not demonstrate a throughput or latency improvement, and cells
with fewer than 200 successful observations retain null percentiles.

Matched private-MCP mixed-load cells use `--scenario matched`: one 32-device
steady cell with a required `--measure-seconds` (60–900) and optional `--rate`
(default 15). Sync-only uses `--database-profile explicit`; MCP cells use
`explicit-mcp` (MCP pool and MCP admission 4) or a lowered `explicit-mcp-1`..`-3`,
with `--mcp-mode idle|read|abusive` and, for load, `--mcp-clients`, `--mcp-rate`
or `--mcp-abuse-concurrency`. The runner refuses mismatched mode/profile pairs
and MCP options on other scenarios before building. `run.json` records the MCP
shape and limit; MCP outcomes stay in the cell's separate `mcp` evidence.

## Startup and privilege boundaries

The pinned PostgreSQL image initializes `babytracker` once on an empty database
volume. The roles are:

| Role | Kind | Membership and use |
| --- | --- | --- |
| `baby_tracker_runtime` | NOLOGIN group | the schema's runtime grants (privilege manifest) |
| `baby_tracker_maintenance` | NOLOGIN group | the schema's maintenance grants: retention, purge, wakes, audit partitions, the Live Activity worker |
| `baby_tracker_migrator` | LOGIN | owns the database, the public schema and every object; runs `migrate`, `authority init/verify/rotate-epoch` |
| `baby_tracker_app` | LOGIN | member of `baby_tracker_runtime` only (`runtime_database_url`) |
| `baby_tracker_export_app` | LOGIN | member of `baby_tracker_maintenance` only (`maintenance_database_url`); passwordless until `export_password` is mounted |

`init-roles.sql` creates the two restricted NOLOGIN group roles;
`init-users.sh` creates the three distinct non-superuser LOGIN principals and
makes `baby_tracker_migrator` the database/public-schema owner. The privilege
manifest names only the two groups. The projector and Apple-revoker roles of
the file-ledger releases are gone: the baseline grants them nothing and the
bootstrap does not create them (Phase 4 Task 27; the homelab's cutover drops
its old ones in runbook R6). None of the memberships has ADMIN option. None of
the LOGIN roles has SUPERUSER, BYPASSRLS, CREATEDB, CREATEROLE or REPLICATION.
Compatible bootstrap reruns are
allowed; incompatible existing attributes, role settings or outgoing memberships
fail closed. Bootstrap is not a general-purpose repair/credential-rotation tool.
The official `postgres` administrator is used for initialization only and its
secret is never mounted into app or migrate. Init scripts do not rerun when the
database volume already exists.

After provisioning files and required configuration (fresh install; `migrate`
refuses a database recorded with any other migration history):

```sh
docker compose -f Server/compose.yaml up -d --build --wait db app
docker compose -f Server/compose.yaml run --rm --no-deps migrate
docker compose -f Server/compose.yaml run --rm --no-deps migrate \
  authority init --instance-id "$BT_SERVER_INSTANCE_ID" --epoch 1 \
  --confirm-instance "$BT_SERVER_INSTANCE_ID"
docker compose -f Server/compose.yaml run --rm --no-deps migrate authority verify
```

The first command intentionally starts an unmigrated application: `/healthz`
returns 200 while `/readyz` returns 503. The second applies migrations as the
table-owning migrator. Readiness remains 503 (`server_state_missing`) until the
third command writes `server_state` as the migrator: the instance ID, recovery
epoch 1 and the commitment of the `actor_id_pepper` (HMAC over the instance
ID; the key itself never leaves the secret). `authority init` is idempotent for
identical values and refuses any other instance ID, epoch or actor-ID key. The
fourth walks every account and family audit chain from genesis and checks the
actor-ID key commitment; it prints `instance_id=… epoch=… scopes=… rows=…
failed=0 result=verified` and exits 1, naming each failed scope, when a chain is
broken. Readiness then returns 200 once the background readiness monitor has
seen a reachable runtime pool, a compatible schema, the runtime login's exact
manifest privileges, the maintenance pools' login and role, `server_state` equal
to its configured instance, epoch and actor-ID key, and a key inventory that
references only configured keys. Requests never check these themselves: every
route, `/readyz` included, reads the monitor's cached state. The monitor runs
one fast probe (schema version and `server_state`) every 2 seconds on its own
one-connection pool, and the full audit at startup, after a schema version
change, after the fast probe recovers from a failure, and at least every 5
minutes. A cached state older than one whole refresh (9 seconds) is refused as
stale, so readiness follows the database within seconds, not on the request
that observes it.
The application never auto-migrates. Subsequent migration runs are
idempotent. Gate traffic on readiness and use the same image version for app
and migrate. The smoke executes this exact sequence, verifies `/v1/server`
identity, restarts with the same actor-ID pepper, proves a replacement closes
readiness and makes `authority verify` fail with `actor_id_key_mismatch`, and
restores the original value.

### Migrations

`Server/migrations` holds one baseline, `000032_baseline.sql`: the schema of
000001–000032 squashed before the homelab cutover, whose catalog
`internal/migrate/testdata/catalog-32.golden` pins; the next migration is
`000033_<name>.sql`, with its row appended to `internal/migrate/compatibility.json`.

## Network, state and backups

The API is published only on 127.0.0.1; PostgreSQL is not published to the host.
The example Compose origin is `https://sync.example.test`. For persistent
self-hosting, set `BT_MODE=self_hosted`, a canonical HTTPS `BT_PUBLIC_ORIGIN`,
the stable instance UUID/epoch, and narrowly scoped trusted-proxy CIDRs. An
operator-managed reverse proxy must terminate TLS and route to the loopback
listener. Secure database transport when changing the single-host topology.

`GET /v1/server` reports the stable origin/instance/epoch, advertises
`identity-v1`, `family-lifecycle-v1`, `recovery-v1`, `records-v1`,
`snapshot-v1`, `timer-commands-v1`, `import-resolver-v1`, and
`live-hints-v1`, and reports
`apple_authorization_mode=identity_token_only` for
self-hosted servers or `code_exchange_required` for hosted servers. Self-hosted
mode registers `POST /v1/auth/challenge`, `/v1/auth/apple`,
`/v1/auth/refresh`, and `/v1/auth/logout`; hosted mode additionally registers
`/v1/auth/apple/notifications`, and composes only with the identitytest Apple
code exchanger (production hosted mode refuses to start). Authentication
requests are recovery/readiness-gated, strictly bounded, and rate-limited by IP
before proof verification; refresh also charges known account and device
buckets before mutation. Every auth success/error returns
`Cache-Control: no-store`. See `docs/protocol/identity-v1.md`.

Authenticated family clients may connect to `GET /v1/live` with exact
`records-v1,live-hints-v1` negotiation and WebSocket subprotocol
`baby-tracker.v1`. Cursor-only hints are advisory; clients reconcile with the
ordinary HTTPS sync route. A write persists no wake intent (the legacy wake
queue is gone since schema 30). Sync and snapshot requests record each
device's acknowledged and delivered cursor in memory, and the wake worker
(every 2 s, on the maintenance pool inside a worker slot) flushes them to
`device_sync_cursors` and compares them with the family sequence: at most once
a minute per device, once per sequence and 500 devices per tick, it
re-announces the high water to devices that are behind through their live
connections. With `BT_APNS_WAKES_ENABLED=true`, a device behind its family
without a live connection, and with a registered token, gets an APNs
background push instead: at most one per device every 20 minutes, 100 per
tick and 8 in flight, with a backoff on Apple's errors; tokens Apple refuses,
and (hourly) tokens of devices without a family session, are deleted.
Hosted production remains fail-closed until its credential adapters exist.

The lifecycle surface includes `POST /v1/account/delete`, which permanently
removes local account authority and explicitly reports provider-revocation
status, plus the owner-only `POST /v1/family/delete`. Family deletion disables
family access immediately and creates a 30-day recovery claim. The recovering
owner can inspect it with `GET /v1/family/recovery`, restore it with
`POST /v1/family/recover`, or irrevocably surrender it with
`POST /v1/family/recovery/forfeit`. Recovery mutations require the route-specific
`recovery-v1` capability; normal family lifecycle requests use
`family-lifecycle-v1`. Provider revocation is manual in every mode
(`apple_authorization_revocation: "manual_required"`); the hosted revocation
worker is removed, so this documentation is not a live hosted-readiness claim.

The application image uses pinned builder/runtime digests, CGO-disabled binaries,
UID/GID 65532, a read-only root filesystem and its own `probe` healthcheck. The
distroless runtime contains no shell/package manager. The only named volume is
`database`: PostgreSQL is the only security authority, and the app has no
state volume.

Back up the database (for example `pg_dump --format=custom` as the migrator,
or a PostgreSQL physical backup) together with the server instance identity,
recovery epoch, permanent actor-ID pepper, retained keyring material and other
required secrets. The database backup is the whole security history: the
authority tables and every scope's audit chain. To restore a database that
may be older than what devices have seen, stop the app first, restore, rotate
the epoch, and start the app again with the new epoch (see
[`docs/operations/security-state.md`](../docs/operations/security-state.md)):

```sh
docker compose -f Server/compose.yaml stop app
# restore the database backup, then:
docker compose -f Server/compose.yaml run --rm --no-deps migrate \
  authority rotate-epoch --expected-epoch "$BT_RECOVERY_EPOCH" \
  --reason-code database_restore --confirm-instance "$BT_SERVER_INSTANCE_ID"
# set BT_RECOVERY_EPOCH to the printed epoch, then:
docker compose -f Server/compose.yaml run --rm --no-deps migrate authority verify
docker compose -f Server/compose.yaml up -d --wait app
```

Every session issued before the rotation then fails with `epoch_changed`.
The rotation also empties `device_sync_cursors` in the same transaction: every
recorded cursor describes deliveries under the old epoch, and the wake worker
rebuilds the rows from the devices' next requests.
Never run `compose down --volumes` against an installation whose data must be
retained; the smoke uses that operation only for its unique disposable
project.

### Private Unraid deployment over Tailscale

> **Note.** The Unraid installation was upgraded from the file-ledger release
> on 2026-10-10 by a one-time importer, since deleted (record:
> [`docs/verification/2026-10-10-homelab-cutover.md`](../docs/verification/2026-10-10-homelab-cutover.md)).
> The steps below are the fresh-install sequence.

`deploy/unraid/compose.yaml` is a production override for the base Compose file.
It runs a pinned Tailscale sidecar named `baby-sync`, terminates Tailnet HTTPS for
`https://baby-sync.example-tailnet.ts.net`, and proxies to the application on the
isolated project network. It publishes no host ports and does
not enable Funnel. Persistent data is bind-mounted only below
`/mnt/user/appdata/baby-tracker-sync`.

On Unraid, capture unrelated container state before any deployment, provision
the project-owned paths, and enroll with a one-use tagged Tailscale auth key:

```sh
export BT_DEPLOY_ROOT=/mnt/user/appdata/baby-tracker-sync
export BT_SOURCE_SHA=<reviewed-40-character-git-sha>
Server/scripts/verify-unraid-deployment.sh capture
Server/scripts/provision-unraid-secrets.sh
BT_TAILSCALE_AUTH_KEY_FILE_SOURCE=/root/tailscale-baby-sync.authkey \
  Server/scripts/provision-unraid-secrets.sh
set -a; . "$BT_DEPLOY_ROOT/deployment.env"; set +a
compose() {
  docker compose --project-name baby-tracker-sync \
    --env-file "$BT_DEPLOY_ROOT/deployment.env" \
    -f Server/compose.yaml -f Server/deploy/unraid/compose.yaml "$@"
}
compose up -d db tailscale
compose exec -T -u postgres db /docker-entrypoint-initdb.d/20-init-users.sh
compose --profile enroll run --rm tailscale-enroll
compose build app migrate
compose run --rm --no-deps migrate
compose run --rm --no-deps migrate authority init \
  --instance-id "$BT_SERVER_INSTANCE_ID" --epoch "$BT_RECOVERY_EPOCH" \
  --confirm-instance "$BT_SERVER_INSTANCE_ID"
compose run --rm --no-deps migrate authority verify
compose up -d --wait app
Server/scripts/verify-unraid-deployment.sh verify
```

The enrollment service is profile-gated and is not part of normal restarts.
Revoke the one-use auth key in the Tailscale admin console after enrollment.
Never use this override with a different `BT_DEPLOY_ROOT`; both provisioning and
verification reject foreign or symlinked targets. A verification diff means an
unrelated container changed after the baseline and must be explained rather
than silently accepted.


### Refresh recovery receipts

The authority keeps refresh-recovery receipts in `refresh_receipts` and
prunes them inside the refresh, acknowledgement, logout and revocation
transactions that retire them; there is no separate cleanup worker or
maintenance function. The maintenance credential is required in every mode:
`BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE` names a protected credential file
for `baby_tracker_export_app` (Compose mounts the `maintenance_database_url`
secret there by default), or, without it, the export credential
`BT_EXPORT_DATABASE_URL_FILE` is used; the server refuses to start with
neither. Inline `BT_RECOVERY_MAINTENANCE_DATABASE_URL` is rejected and the URL
must differ from the runtime URL. The readiness audit verifies the login, so
`refresh-recovery-v1` is advertised whenever the node is ready. Recovery
routes are gated by the node readiness.


## Local capacity evidence and node boundary (2026-10-03)

The [final capacity report](../docs/verification/2026-10-03-single-node-capacity.md)
records full server database/race, multi-architecture build, vet and protocol
checks plus complete bounded receipts. Explicit laboratory pools are
runtime/projector/maintenance/MCP 16/4/2/0, declared server/reserve 64/8,
and finite admission total/MCP 32/0. Both resource profiles failed the
32-device 200-observation gate; no larger sweep, selected envelope or soak
is established. The small profile includes deadlines and 503s despite a
zero quota-refusal field. HTTP journal decode is separate from Swift
store/observable apply, rendered UI and physical-device acceptance.

The full source-attested Swift attempt retained 164 pairs and a shutdown
suspension. A bounded attribution run located the native receive join;
callback cancellation now finishes both receive and pong waiters exactly
once. Focused regressions and a fresh three-pair native lifecycle after a
35-second dwell pass. This proves that lifecycle control, not a 200-pair
latency SLO. Failed/interrupted receipts and excluded instrumentation remain
in the report. The [Unraid runbook](../docs/verification/2026-10-03-unraid-capacity-runbook.md)
is a proposal only: no node stress or production configuration change has
run, and host/storage/pressure facts require a fresh read-only check after
Track B before the combined approval.


### Capacity harness acceptance and provenance amendment

The reusable sweep evaluates the 32-device, 16-writer, 15-writes-per-writer-per-minute,
60-second steady anchor (240 planned writes) before expansion. The 32-device
1/minute diagnostic has only 16 planned writes and cannot qualify. Low-rate rows
remain diagnostics and do not block evaluation of their population's anchor.
The 128-device steady 1/minute and 15/minute rows require the accepted 32-device
steady15 anchor; the 512-device equivalents require the accepted 128-device
steady15 anchor. A 60/minute row requires its own population's accepted steady15
anchor. The four 32-device shape rows require the 32-device steady15 anchor.
Every anchor must match the current product, resource/database profile, requested
geometry and 60-second window, have at least 200 successful writer/peer observations,
pass the typed-refusal eligibility rules, and finish exact owned-resource cleanup.
Suppressed rows retain `completion: gated`, `measurement_state: unmeasured`, their
planned geometry and required anchor. A gated sweep exits nonzero. Exit zero means
the requested lifecycle completed; always inspect cell `passing` for acceptance.
A completed low-sample diagnostic or pilot is not a passing capacity claim.

SIGINT/SIGTERM belong to the whole runner, including pre-cell builds and final
cleanup. The active owned command/group is canceled and joined; no subsequent
cell starts. Remaining rows retain interrupted/unmeasured checkpoints. Cleanup
continues using exact ownership receipts and its original deadline. A failed
contention proof similarly cancels and joins both started writers before recording
all outcomes, checkpointing or returning.

New cells identify `refusal_classification: quota_429_and_typed_server_busy_v1`.
`refusals` totals HTTP 429 quota refusals and bounded typed HTTP 503 `server_busy`
admission refusals; `global_admission_refusals` is the latter subset. Other 503s
remain unexpected errors. Any refusal disqualifies acceptance. Old receipts are
unchanged: their refusal counts describe quota 429s and their historical 503 error
counts cannot be retrospectively classified without response bodies. Reading an
old receipt with the new reporter marks this classification `unavailable_legacy`
and does not promote it to passing under the new acceptance contract.

New `run.json` receipts independently attest product revision/inputs, fixture
construction identity, clean harness revision/inputs (including Python helpers and
embedded Go inputs), built client SHA-256, exact build argv, Go/Python executable
hashes and effective toolchain settings. The client build disables ambient Go
flags, workspaces, Go environment files, toolchain switching and cgo. Inputs are
checked before/after build and each cell; client bytes are checked before/after
execution. Drift revokes provenance and marks measured cell receipts invalid.
A newer clean harness can measure a different clean product baseline. These fields
are unavailable for historical runs; prior separately collected attestations remain
only evidence for their recorded sources. This amendment adds no capacity run,
healthy envelope, native acceptance, production change or replacement broad gate.
