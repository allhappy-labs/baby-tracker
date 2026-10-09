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
`BT_SERVER_INSTANCE_ID`, `BT_RECOVERY_EPOCH`, `BT_SECURITY_LEDGER_DIR`,
`BT_LEDGER_HEAD_DIR`, `BT_APPLE_AUDIENCE`, `BT_IDENTITY_KEY_ID`,
`BT_IDENTITY_PEPPER_FILE`, `BT_SESSION_TOKEN_KEY_ID`,
`BT_SESSION_TOKEN_PEPPER_FILE`, `BT_DEVICE_CONTINUITY_KEY_ID`,
`BT_DEVICE_CONTINUITY_PEPPER_FILE`, `BT_APPLE_NOTIFICATION_KEY_ID`,
`BT_APPLE_NOTIFICATION_PEPPER_FILE`, `BT_INVITATION_KEY_ID`,
`BT_INVITATION_PEPPER_FILE`, `BT_PAGE_TOKEN_KEY_ID`,
`BT_PAGE_TOKEN_KEY_FILE`, `BT_ACTOR_ID_PEPPER_FILE`,
`BT_RATE_LIMIT_PEPPER_FILE`, `BT_MAX_CAREGIVERS`, `BT_MAX_INVITATIONS`,
`BT_INVITATION_TTL`, `BT_MAX_CANONICAL_RECORDS_PER_FAMILY`,
`BT_MAX_CHARGED_STORAGE_BYTES_PER_FAMILY` and `BT_TRUSTED_PROXY_CIDRS`. Hosted mode also requires
`BT_APPLE_REVOCATION_DATABASE_URL_FILE` and
`BT_APPLE_REVOCATION_WORKER_ID`; it accepts bounded
`BT_APPLE_REVOCATION_POLL_INTERVAL`, `BT_APPLE_REVOCATION_LEASE_TTL`, and
`BT_CREDENTIAL_ORPHAN_GRACE` overrides.
`BT_SESSION_SIGNING_KEY_FILE` remains a deprecated alias for
`BT_SESSION_TOKEN_PEPPER_FILE`; setting both is rejected. Supply exactly one of `BT_DATABASE_URL` and
`BT_DATABASE_URL_FILE`, and exactly one of `BT_PROJECTOR_DATABASE_URL` and
`BT_PROJECTOR_DATABASE_URL_FILE`; server and reconciliation admin require
distinct runtime/projector credentials. Every database URL file is a nonempty
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

ActivityKit APNs delivery is optional in every `BT_MODE` and defaults off.
Set `BT_LIVE_ACTIVITY_PUSH_ENABLED=true` with `BT_APNS_TEAM_ID` (10-character
Apple Team ID), `BT_APNS_KEY_ID` (10-character Apple key ID),
`BT_APNS_KEY_FILE` (a nonsymlink P-256 PEM `.p8` key readable by the server
with mode 0600), `BT_APNS_BUNDLE_ID` (the app bundle identifier), and
`BT_LIVE_ACTIVITY_DATABASE_URL_FILE` (a nonsymlink, regular, mode-0600 file
containing one database URL line of at most 16 KiB for the dedicated live
activity worker role). This worker credential must
differ from runtime, projector, export, and recovery maintenance credentials.
The private key and worker database URL cannot be supplied inline. An absent
or disabled group leaves ordinary sync available; malformed enabled settings
fail startup. Restart or explicitly reload configuration to rotate APNs key
material. The transport uses Apple's fixed sandbox and production HTTP/2
endpoints according to each device registration, with a 10-second request
bound and no internal retries. Start alerts use app-main-bundle localized
`LIVE_ACTIVITY_FEED_START_TITLE` or `LIVE_ACTIVITY_SLEEP_START_TITLE` and
`LIVE_ACTIVITY_START_BODY` (one argument: baby's display name); the app must
provide those keys in its main localization resources. Starts have no sound,
ordinary updates are quiet, and ends request immediate dismissal.

The six rotating keyrings each accept an optional
`<PREFIX>_PREVIOUS_KEYS_FILE` manifest with at most seven retained read-key
entries. These strict JSON manifests contain absolute pepper-file paths, never
secret bytes. The actor-ID pepper is permanent and has no key ID or previous-key
manifest. It is backup-critical: changing or losing it keeps readiness closed
with `actor_id_key_mismatch` instead of allocating different account actors.
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
inventory proves that retained identity,
device-continuity, and invitation keys still cover authority references. It
does not authorize session-token or notification-key retirement. Invitation
keys cannot be retired while any authoritative invitation state still names
their key ID, including consumed, revoked, or expired invitations retained for
replay protection; backup and restore retention must be considered before
compacting those references. Retain session-token keys until
every session/history row using the key has passed its absolute lifetime and
its refresh-reuse evidence is eligible for compaction. Retain notification keys
through the accepted 24-hour event-age window plus clock skew and the full
backup/restore replay window. For the beta, keep either class unless an operator
has separately proved those persisted references and windows are exhausted.
Each rate policy can override its documented default with
`BT_RATE_LIMIT_<SCOPE_WITH_UNDERSCORES>_COUNT` and `_WINDOW`, for example
`BT_RATE_LIMIT_AUTH_CHALLENGE_IP_COUNT=30` and
`BT_RATE_LIMIT_AUTH_CHALLENGE_IP_WINDOW=10m`.
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
`BT_MAX_CHARGED_STORAGE_BYTES_PER_FAMILY`. Charged bytes cover current
canonical data and immutable synchronization history, with deletion headroom;
the effective values are returned by `GET /v1/server`. Tombstoned records stop
using record-count slots, but retained tombstones and history remain charged
bytes.

Lifecycle worker configuration is fail-closed. A direct
`BT_APPLE_REVOCATION_DATABASE_URL` is forbidden, lifecycle settings are
rejected outside hosted mode, and hosted mode will not start without the
separate URL file and worker ID. That URL is reserved for the least-privileged
`baby_tracker_apple_revoker_app` login. The current server executable validates
this contract but does not yet compose or start the live hosted worker, so the
base self-hosted Compose file deliberately mounts no hosted lifecycle secret.

Live Activity push is optional in every authentication mode, including
`BT_MODE=self_hosted`. It is disabled unless `BT_LIVE_ACTIVITY_PUSH_ENABLED=true`;
disabled startup performs no push-only database checks and opens no worker pool.
Enabling it requires `BT_APNS_TEAM_ID`, `BT_APNS_KEY_ID`, `BT_APNS_BUNDLE_ID`,
`BT_APNS_KEY_FILE` (a P-256 PEM key), and `BT_LIVE_ACTIVITY_DATABASE_URL_FILE`
(one URL line for the distinct `baby_tracker_live_activity_app` login).
Both mounted application secret files must be regular nonsymlink files with
mode 0600 and readable by the application UID 65532. A direct worker URL in the
environment is rejected. Push tokens and family snapshots stay on this server;
only the selected Apple sandbox/production APNs endpoint receives pushes.

`baby-tracker-server live-activity-preflight [--environment sandbox|production|both]
[--skip-apple] [--skip-database]` checks that configuration before push is
enabled, from the same environment and mounted files the enabled server reads.
It validates the key file (regular, mode 0600, P-256 PKCS#8 or EC PEM), the
ten-character Team ID and Key ID and the bundle identifier, derives the Live
Activity topic, and for each selected environment (default `sandbox`, for
development-signed builds; `production` for TestFlight/App Store builds) sends
exactly one quiet update through the production transport and provider JWT to a
syntactically valid all-zero dummy token at Apple's fixed host. Apple's answer is
the verdict: `400 BadDeviceToken` passes (key, team and key ID accepted; the
topic is not contradicted, but a dummy token cannot prove it, since Apple does
not document whether a bad token is refused before or after the topic check;
the first accepted real send is the topic proof); 403 (`InvalidProviderToken`,
`ExpiredProviderToken`, `Forbidden`) fails the key/team/key ID; `400
TopicDisallowed`, `BadTopic` or `DeviceTokenNotForTopic` fails the bundle/topic;
429 asks for a later retry (`TooManyProviderTokenUpdates`: each preflight
process mints a provider JWT and Apple allows one token update per key per 20
minutes, so run it once, wait 20 minutes before a rerun, and never while the
enabled app runs); no answer is a connectivity failure; anything else
is reported as unknown with status and reason. When
`BT_LIVE_ACTIVITY_DATABASE_URL_FILE` is set it also runs the activation's worker
checks (schema exactly 28, the exact `baby_tracker_live_activity` inventory and,
with the runtime URL, the shared-snapshot proof that both logins use one
database), reporting failures by code only; otherwise it skips them with a
note. Output is one line per check plus `RESULT`; the exit code is 0 only when
every requested check passed, 1 otherwise, 2 for usage. It never prints the
key, a token, a JWT or a database URL.

Enabled startup fails fast only on malformed configuration or composition: an
unreadable key file, a worker URL that does not parse, a missing dependency.
Everything that needs the database or application readiness — a compatible
schema of exactly 28, the exact worker privilege inventory, the shared-database
proof, the pool budget, recovery reconciliation and readiness, and the bounded
initial worker pass — is activation. It is attempted once at boot and, when
not yet possible (migrations still pending, recovery not reconcilable, a
privilege or budget mismatch), deferred to the worker's supervision: the
process starts not ready exactly as a disabled server does, logs
`live activity worker activation deferred` with a fixed code, keeps push
unavailable and unadvertised, and retries with the same 2–60 s backoff as
after a stop until `live activity worker resumed`. A boot that is ready
activates before the HTTP server starts, as before.
A shared PostgreSQL snapshot proves the worker and runtime pools use the same
database, even when another database has an identical schema and role inventory.
The worker role has only metadata and narrow function privileges;
it cannot read care records or tokens. Snapshot reads and the minute authority
sweep use the runtime pool under the existing authority guard. Every sweep and
snapshot checks application/recovery readiness before acquiring the authority
fence, so actor-key mismatch or recovery quarantine prevents delivery.
The two polling
runners share a worker pool capped at two connections, overriding URL pool
settings. A startup check sums the configured maxima of the actual runtime,
projector, export and recovery pools (shared export/recovery is counted once),
adds two, and requires that total to fit PostgreSQL `max_connections` minus
`superuser_reserved_connections` and `reserved_connections` when present.
Existing pool defaults are unchanged. This conservative declared maximum checks
one application process; operators must budget separately for replicas,
migrations, monitoring and unrelated database clients.

`GET /v1/server` advertises `live-activity-push-v1` only after startup local
checks pass and while the supervised worker is running and application
readiness holds. Request-time checks read only the worker's supervised state
plus the existing application readiness (once per request); they never use
the worker or runtime pool for schema, privilege or budget validation, which
runs only at startup and before resuming. Worker infrastructure failure removes
that capability and rejects new registrations (`PUT`); unregistering (`DELETE`)
still succeeds. Ordinary care and sync retain their existing readiness checks.
Unregistering is a repository-only path (`liveactivity.NewUnregistration`),
composed whether or not push is enabled: it needs no worker, APNs key or push
configuration, so `DELETE /v1/live-activities/device/{id}` succeeds with the
same authentication, authorization and version idempotency on a server whose
push is disabled or whose worker is stopped, while `PUT` stays refused with
503 and the capability stays unadvertised. That route alone accepts the
`live-activity-push-v1` capability header whether or not it is advertised.
Transient Apple connectivity and retryable APNs rejection back off durably and
never become global sync readiness dependencies. The initial worker pass at
startup and before resuming has its own budget (ten seconds plus the five-second
send timeout), separate from local validation, so a slow or hanging APNs send is
recorded as durable retry/ambiguous state instead of failing activation. Shutdown
cancels and joins the worker before closing its pool and idle APNs connections.
After a worker infrastructure failure, the process joins the stopped worker,
keeps push unavailable, and retries local checks after cancellable backoff of
2, 4, 8, 16, 32 and then 60 seconds (capped at 60). Each retry has a ten-second
local validation budget and verifies schema, exact worker privileges, shared
database, aggregate pool budget and application/recovery readiness. Once valid,
the same pool, transport and worker resume queued delivery without a process
restart; worker runs never overlap. Invalid readiness keeps delivery stopped.
APNs credentials are loaded once, so changing configuration or signing keys
still requires a process restart. Shutdown also cancels and joins backoff.
The stopped and failed-resume log lines carry a fixed error code, never an
error message.

Each runner drains due work (up to 32 claims per pass) before waiting for its
next one-second poll, still with at most two sends in flight and one per card
target. A job that fails on its own data is parked with `last_reason`
`dispatch_failed` and backs off (2 to 60 seconds) while the rest of the queue
keeps delivering; a job that lost a database race (a serialization failure or
deadlock, SQLSTATE class 40) is released as `transient_conflict` for the
shortest backoff with no failure counted against it; only dependency failures
(database, privileges, readiness) stop the worker for supervision. Every send attempt keeps one
`live_activity_history` row: its event and APNs' answer — the response class
(`accepted`, `invalidToken`, `retryable`, `configurationFailure`, `ambiguous`),
the HTTP status and an allowlisted APNs reason — and never a token, payload or
care content; the job's `last_reason` holds the latest non-accepted class.
Every send APNs does not accept also logs one `live activity push not accepted`
line with the event, kind, class, status, reason and whether the request was
sent at all. A wrong bundle ID or key
scope (`DeviceTokenNotForTopic`, `TopicDisallowed`, `BadTopic`) is a
configuration failure and clears no phone's tokens; `BadDeviceToken` (including
a token from the other APNs environment) and `Unregistered` clear that token.
A start the transport rejects before any byte reaches APNs (local validation,
payload encoding, provider-token signing) is known-unsent: it is recorded as a
`configurationFailure` and released — status `pending`, no attempt nonce — for
a fresh attempt after backoff, instead of waiting start-unconfirmed for a
phone's proof. A start that may have reached Apple keeps the ambiguity rules
and is never resent blind. A stale lease the worker cannot release is logged
once as `live activity stale lease not released` with a fixed code.
Starts, ends and updates that change a card's controls (phase, activity kind,
write access) go at APNs priority 10; content-only edits in the same phase go
at priority 5. A phone's cards are ordered in byte order (`COLLATE "C"`),
the order Swift's `String` comparison gives, so the server's duplicate end
never targets the card the phone itself keeps. Only an active card with a token
can be adopted as a phone's own card for a new lifecycle, and an ended or
dismissed card joins a lifecycle by reference only before that lifecycle has a
generation: a swiped break card still naming a session resumed later cannot
freeze the phone's new card.

Compose additionally reads `BT_SECRETS_DIR`, `BT_IMAGE_NAME` and `BT_HTTP_PORT`.
The secret directory must contain these externally provisioned files:

| File | Consumer |
| --- | --- |
| `bootstrap_password` | PostgreSQL init administrator only |
| `migrator_password` | PostgreSQL principal bootstrap |
| `runtime_password` | PostgreSQL principal bootstrap |
| `projector_password` | PostgreSQL projector principal bootstrap |
| `migrator_database_url` | One-shot admin migration service |
| `runtime_database_url` | Application service |
| `projector_database_url` | Application projection and reconciliation |
| `identity_pepper` | Versioned Apple identity lookup HMAC |
| `session_token_pepper` | Versioned opaque session-token HMAC |
| `device_continuity_pepper` | Versioned device-continuity HMAC |
| `apple_notification_pepper` | Versioned Apple-notification replay HMAC |
| `invitation_pepper` | Versioned invitation-token HMAC |
| `page_token_key` | Versioned opaque sync/snapshot page-token HMAC |
| `actor_id_pepper` | Permanent actor-ID HMAC and authority commitment |
| `rate_limit_pepper` | Ephemeral abuse-bucket HMAC |

Database URL files must authenticate as `baby_tracker_migrator`,
`baby_tracker_app`, and `baby_tracker_projector_app`, respectively, against
database `babytracker` on service `db`.
Provision matching passwords and percent-encode URL credential components as
needed. Never commit secrets. Compose mounts these files rather than putting
credentials in environment values or command arguments. Local Compose file
secrets are bind mounts: ensure the PostgreSQL user can read its files and UID
65532 can read the application's files. The smoke directory is private (0700)
with container-readable files; use a suitable secret manager/permissions policy
for a persistent installation. No Apple private key is embedded or provisioned.
Hosted mode therefore remains not ready with `hosted_adapter_unavailable`.
After a concrete adapter is added it must remain closed with
`hosted_credential_rotation_unverified` until the live independent-token
rotation gate passes. Supplying lifecycle credentials does not bypass either
gate. Self-hosted mode constructs no hosted Apple transport.

## Startup and privilege boundaries

The pinned PostgreSQL image initializes `babytracker` once on an empty database
volume. `init-roles.sql` creates the restricted NOLOGIN group roles;
`init-users.sh` creates five distinct non-superuser LOGIN principals and makes
`baby_tracker_migrator` the database/public-schema owner. `baby_tracker_app`
belongs only to `baby_tracker_runtime`; `baby_tracker_projector_app` belongs
only to `baby_tracker_projector`; `baby_tracker_export_app` belongs only to
`baby_tracker_maintenance`; and `baby_tracker_apple_revoker_app` belongs
only to `baby_tracker_apple_revoker`. None of these memberships has ADMIN
option. The base self-hosted deployment intentionally leaves the revoker login
passwordless, so it cannot authenticate; mounting a future
`apple_revocation_password` during an idempotent bootstrap is a hosted-only
operation. The revoker group receives no table DML and can execute only its
versioned allowlist of security-definer lifecycle functions. None of the LOGIN
roles has SUPERUSER, BYPASSRLS, CREATEDB, CREATEROLE or REPLICATION.
Compatible bootstrap reruns are
allowed; incompatible existing attributes, role settings or outgoing memberships
fail closed. Bootstrap is not a general-purpose repair/credential-rotation tool.
The official `postgres` administrator is used for initialization only and its
secret is never mounted into app or migrate. Init scripts do not rerun when the
database volume already exists.

After provisioning files and required configuration:

```sh
docker compose -f Server/compose.yaml up -d --build --wait db app
docker compose -f Server/compose.yaml run --rm --no-deps migrate
docker compose -f Server/compose.yaml run --rm --no-deps migrate \
  security init --instance-id "$BT_SERVER_INSTANCE_ID" --epoch 1 \
  --confirm-instance "$BT_SERVER_INSTANCE_ID"
docker compose -f Server/compose.yaml run --rm --no-deps \
  -e BT_DATABASE_URL_FILE=/run/secrets/runtime_database_url \
  migrate security reconcile
```

The first command intentionally starts an unmigrated application: `/healthz`
returns 200 while `/readyz` returns 503. The second applies migrations as the
table-owning migrator. Readiness remains 503 until the third command initializes
the empty ledger/head identity and the fourth validates and replays authority
through the projector credential, mirrors recovery state, and checks schema
compatibility. Reconciliation deliberately overrides the tool's default
migrator URL with `runtime_database_url`; Compose still supplies the distinct
`projector_database_url`. The application and explicit security tools receive
the same read-only `actor_id_pepper` secret. Only then does `/readyz` return 200. The application
never auto-migrates. Subsequent migration runs are idempotent. Gate traffic on
readiness and use the same image version for app and migrate. The smoke executes
this exact sequence, verifies `/v1/server` identity, restarts with the same
actor-ID pepper, proves a replacement closes readiness with
`actor_id_key_mismatch`, and restores the original value.

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
`/v1/auth/apple/notifications`, but remains not ready until its concrete
credential adapter and live rotation gate are configured. Authentication
requests are recovery/readiness-gated, strictly bounded, and rate-limited by IP
before proof verification; refresh also charges known account and device
buckets before mutation. Every auth success/error returns
`Cache-Control: no-store`. See `docs/protocol/identity-v1.md`.

Authenticated family clients may connect to `GET /v1/live` with exact
`records-v1,live-hints-v1` negotiation and WebSocket subprotocol
`baby-tracker.v1`. Cursor-only hints are advisory; clients reconcile with the
ordinary HTTPS sync route. Self-hosted deployments never schedule external
wakes. Hosted-mode mutations persist family-scoped wake intent, but this slice
does not include an APNs client, device-token route, or delivery worker, and
hosted production remains fail-closed until its credential adapters exist.

The lifecycle surface includes `POST /v1/account/delete`, which permanently
removes local account authority and explicitly reports provider-revocation
status, plus the owner-only `POST /v1/family/delete`. Family deletion disables
family access immediately and creates a 30-day recovery claim. The recovering
owner can inspect it with `GET /v1/family/recovery`, restore it with
`POST /v1/family/recover`, or irrevocably surrender it with
`POST /v1/family/recovery/forfeit`. Recovery mutations require the route-specific
`recovery-v1` capability; normal family lifecycle requests use
`family-lifecycle-v1`. Provider revocation is asynchronous in hosted mode and
manual in self-hosted mode. The hosted adapter/worker remains unavailable as
described above, so this documentation is not a live hosted-readiness claim.

The application image uses pinned builder/runtime digests, CGO-disabled binaries,
UID/GID 65532, a read-only root filesystem and its own `probe` healthcheck. The
distroless runtime contains no shell/package manager. Named volumes `database`,
`security_ledger` and `ledger_head` preserve distinct storage boundaries. Ledger
and head directories are separate nonroot-owned mounts. Their committed
authority head and recovery identity fence restored or ahead databases:
readiness stays closed until current authority has been validated, projected
with the least-privileged projector, and mirrored exactly.

Coordinate database, ledger and head backups at one consistent recovery point,
and retain the associated server instance identity, recovery epoch, permanent
actor-ID pepper, retained keyring material and other required secrets securely.
The ledger/head authority and `actor-id-key.json` sidecar must survive a database
restore; restore operations require explicit epoch rotation and reconciliation.
Do not restore one volume independently and assume the identity/security history
remains consistent. Never run `compose down --volumes` against an installation
whose data must be retained; the smoke uses that operation only for its unique
disposable project.

### Private Unraid deployment over Tailscale

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
compose run --rm --no-deps migrate security init \
  --instance-id "$BT_SERVER_INSTANCE_ID" --epoch "$BT_RECOVERY_EPOCH" \
  --confirm-instance "$BT_SERVER_INSTANCE_ID"
compose run --rm --no-deps -e BT_DATABASE_URL_FILE=/run/secrets/runtime_database_url \
  migrate security reconcile
compose up -d --wait app
Server/scripts/verify-unraid-deployment.sh verify
```

The enrollment service is profile-gated and is not part of normal restarts.

To explicitly enable this private server's Live Activity push, create the
operator-owned `secrets/live_activity_password`, `secrets/live_activity_database_url`
and `secrets/apns_key.p8` below `BT_DEPLOY_ROOT` with
`Server/scripts/provision-live-activity-secrets.sh` (as root, after
`provision-unraid-secrets.sh`, which it recognises by `deployment.env`). It
generates the worker password, builds the worker URL from it, installs the
Apple `.p8` from the path given to `--apns-key` (an unencrypted PEM P-256
private key; the value is never a command argument), and sets the owner and
mode each container needs: the password `70:70 0600` for PostgreSQL, the URL and
the key `65532:65532 0600` for the app. It is idempotent, refuses to overwrite
without `--force` (which rotates the password and URL together), supports
`--dry-run`, and prints no secret value. Use the existing Apple team/key for
the selected bundle; this repository does not create Apple keys.

```sh
Server/scripts/provision-live-activity-secrets.sh --dry-run --apns-key /path/to/AuthKey_<key id>.p8
Server/scripts/provision-live-activity-secrets.sh --apns-key /path/to/AuthKey_<key id>.p8
```

Add the reviewed identifiers (`BT_APNS_TEAM_ID`, `BT_APNS_KEY_ID`,
`BT_APNS_BUNDLE_ID`) to `deployment.env` and include the optional overlay in
every subsequent Compose invocation:

```sh
docker compose --project-name baby-tracker-sync \
  --env-file "$BT_DEPLOY_ROOT/deployment.env" \
  -f Server/compose.yaml -f Server/deploy/unraid/compose.yaml \
  -f Server/deploy/unraid/live-activity-push.yaml config --quiet
```

Then run the key-arrival preflight in a one-off app container with that
composition, before any outage, with the database check deferred (the worker
login has no password until `db` is recreated):

```sh
docker compose --project-name baby-tracker-sync \
  --env-file "$BT_DEPLOY_ROOT/deployment.env" \
  -f Server/compose.yaml -f Server/deploy/unraid/compose.yaml \
  -f Server/deploy/unraid/live-activity-push.yaml \
  run --rm --no-deps app live-activity-preflight --environment sandbox --skip-database
```

Use that same three-file composition for the normal reviewed deployment flow:
recreate `db` to mount the optional password, run the existing
`20-init-users.sh` as PostgreSQL's operator to configure the worker login, apply
migrations using `migrate`, run the database preflight (`run --rm --no-deps app
live-activity-preflight --skip-apple`: the worker database checks; Apple was
already proven and a second provider JWT within 20 minutes would hit its
token-update throttle) and only then recreate `app`. The app's own first send
may then be answered `429 TooManyProviderTokenUpdates` within that window; it
is recorded as `retryable` and clears on retry. The initializer reads the
password file server-side; password bytes need not enter command arguments.
No new public or host ports are added. Omitting the overlay keeps push disabled
and requires none of these three files. These are operator instructions, not
evidence that any production deployment or Apple delivery has been performed;
the exact gated sequence is in `docs/runbooks/family-live-activity-push.md`
(R2).
Revoke the one-use auth key in the Tailscale admin console after enrollment.
Never use this override with a different `BT_DEPLOY_ROOT`; both provisioning and
verification reject foreign or symlinked targets. A verification diff means an
unrelated container changed after the baseline and must be explained rather
than silently accepted.


### Refresh recovery receipt maintenance

Recovery uses an independent cleanup loop at startup and every five minutes,
with a 30-second iteration timeout and at most 512 retired receipts per batch.
It deletes only expired or explicitly unusable receipts; outstanding valid
receipts keep their original policy/child-refresh deadline across restarts.

When exports are configured, cleanup reuses their isolated maintenance pool.
Without exports, set `BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE` to a protected
credential file for `baby_tracker_export_app` (the existing isolated
`baby_tracker_maintenance` login). This enables cleanup without export keys or
export API configuration. The URL must differ from runtime and projector URLs;
startup verifies that the login has only the expected maintenance authority.
Inline `BT_RECOVERY_MAINTENANCE_DATABASE_URL` is rejected. Mount the secret file
read-only, as for the export worker URL. If neither maintenance source is
configured, cleanup is disabled; recovery capability admission must account for
that before advertising the new protocol. Runtime and projector credentials
receive no maintenance privilege.
