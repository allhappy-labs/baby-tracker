# Security authority state and recovery

PostgreSQL is the only security authority. Accounts, identities, devices,
sessions, families, memberships, invitations, entitlements, recovery claims,
integration grants and command receipts are relational rows written by
transactional command handlers. Every security change also appends one audit
row to the hash chain of the scope it changes (the family, or the account) in
the same transaction. There is no file ledger, trusted head, projector role,
projection function or in-memory replay any more, and no state volume: the
database is the whole security history.

## Trust boundary

The trust boundary is the database, and it includes the runtime credential.
The runtime login (`baby_tracker_app`, the `BT_DATABASE_URL` /
`runtime_database_url` credential) holds exactly the privileges the schema
privilege manifest records for `baby_tracker_runtime`, column-scoped where
possible, and readiness refuses to serve while it holds anything else. Those
privileges are the authority itself: the runtime can insert and update
accounts, identities, devices, sessions, families, memberships, slots,
invitations, entitlements, recovery claims, integration grants, command
receipts and audit rows, because every command handler writes them. The
authority tables' row policies admit a transaction that sets `app.authority`,
but `app.authority` is a transaction-local setting any session of the runtime
login can set with `set_config`. It stops the application's own tenant code
from writing authority rows by accident. It is not a privilege and does not
separate an attacker holding the runtime credential from the authority.

So the runtime credential is authority-equivalent. Anyone who holds it, or
who achieves SQL injection in the server, can mint sessions, change
memberships and ownership, grant entitlements or integration grants, and
append audit rows that extend a chain consistently (the chain hash is
unkeyed). The table owner (`baby_tracker_migrator`) and superusers can do
the same and more. The schema refuses only one class of tampering from the
runtime: an audit chain head on `accounts` or `families` only moves forward.
Even with `app.authority` set, the runtime cannot clear `chain_failed`,
lower `audit_seq`, or change `audit_hash` without advancing `audit_seq`
(42501); only the table owner can.

Tamper detection therefore rests on two things: the per-scope audit chains
with their verifier, which catch rows changed, removed or reordered behind
a chain's back, and keeping the runtime credential (and the migrator's)
secret. Operationally:

- Treat `runtime_database_url` like the migrator URL and the keyring
  peppers. Mount it only into the server (never into sidecars, CI logs or
  shared volumes) and restrict it with the secret manager's or filesystem's
  permissions. Do not hand it to operators for ad-hoc queries.
- Keep PostgreSQL unpublished (the Compose default) so the credential is only
  usable from the server's network.
- Rotate the runtime password (`ALTER ROLE baby_tracker_app PASSWORD …` as
  the administrator, update the secret file, restart the server) on any
  suspected exposure, when someone with access leaves, and on a regular
  schedule. Then run `baby-tracker-admin authority verify`. After a confirmed
  compromise, also restore from a backup taken before it, following
  [Restoring a database](#restoring-a-database), because rows the attacker
  appended consistently cannot be told apart from real ones.

## Server identity: `server_state`

`server_state` is a single row holding the node's instance ID, recovery epoch
and the commitment to the permanent actor-ID key (HMAC-SHA256 keyed by the
actor-ID pepper over the length-prefixed domain
`baby-tracker-actor-id-commitment-v1` and the instance ID; the key itself never
leaves its secret file). Only the migrator writes it:

```sh
baby-tracker-admin authority init --instance-id <uuid> --epoch 1 --confirm-instance <uuid>
```

`init` writes epoch 1 for a fresh install. It is idempotent for identical
values (`result=unchanged`), binds the commitment of a matching row that has
none (`result=bound`), and refuses any other instance ID or epoch
(`server_identity_mismatch`) or any other actor-ID key
(`actor_id_key_mismatch`). It never re-identifies or re-keys a node.

Readiness compares the row with the server's configuration on every check: a
missing row is `server_state_missing`, another instance or epoch is
`server_identity_mismatch`, and a replaced or lost actor-ID pepper is
`actor_id_key_mismatch`. Each keeps `/readyz` at 503 and is logged once when
readiness changes, not per request.

## Readiness (interim)

Until Phase 3 moves it to a cached background monitor, readiness runs per
request (two-second deadline): ping the runtime pool; check the declared
connection budget against the server's `max_connections` when an explicit
profile is set; check the schema is a compatible version; verify the runtime
login's exact manifest privileges (`VerifyRuntimeRole`); compare `server_state`
as above; confirm the schema did not change meanwhile; and read the cached
credential-key verdict. The key inventory (key IDs live sessions, devices,
identities and invitations still reference) is computed at startup within a
one-second attempt and refreshed every five minutes by the authority loop;
until it is first computed readiness refuses, and a referenced key that is not
configured refuses with `credential_key_unavailable`.

## Audit chains and per-scope fail-closed

Each audit row carries its scope, a per-scope sequence, the previous row's hash
and its own hash over a canonical encoding of the change. The scope's chain
head (`audit_seq`, `audit_hash`) lives on the account or family row the command
has already locked, so families never contend.

The server runs an incremental verifier every five minutes: it walks every
chain that moved since its checkpoint (at most 1,000 chains per pass) and
advances the checkpoint. The full verification is an admin command, run as the
migrator:

```sh
baby-tracker-admin authority verify
```

It first checks the actor-ID key commitment (and, when set,
`BT_SERVER_INSTANCE_ID`/`BT_RECOVERY_EPOCH`) against `server_state`, then walks
every account and family chain from genesis, also holding each chain to its
checkpoint. It prints one `failed scope=<account|family> id=<uuid>` line per
failed chain and a summary `instance_id=… epoch=… scopes=… rows=… failed=…
result=verified|failed`, and exits 1 when any chain failed or could not be
verified.

A broken chain fails closed for that scope only: the verifier sets
`chain_failed` on the account or family row and logs an alert. Every later
transaction of that scope is refused with `scope_failed`; other families and
accounts keep working. Neither verifier clears `chain_failed`, and the
runtime login cannot: the schema refuses it outside the table owner. Clearing
it is an explicit owner action after investigation. Either restore a backup
whose `authority verify` passes, or, once the chain's rows are repaired and
`authority verify` reports the scope intact, clear the flag as the migrator
(`$MIGRATOR_URL` is the URL in the `migrator_database_url` secret):

```sh
psql "$MIGRATOR_URL" -v ON_ERROR_STOP=1 <<'SQL'
BEGIN;
SELECT set_config('app.authority', '1', true); -- the row policies admit the owner only under it
UPDATE families SET chain_failed = false WHERE id = '<family-uuid>' AND chain_failed; -- or accounts
COMMIT;
SQL
```

Clearing a chain that still mismatches only lasts until the next verifier
pass marks it failed again. How clients see a failed scope:

- the family service maps `scope_failed` to `service_unavailable`, keeping
  the legacy family codes (the cause stays wrapped for the server's 5xx log);
- the account and operator services pass `scope_failed` through unchanged;
- the HTTP layer answers an unrecognized code, `scope_failed` included, with
  503 `service_unavailable`, so phones see a retryable 503 either way.
  Operators see `scope_failed` from the admin CLI.

This split is deliberate (decided with Phase 1): the family surface keeps its
legacy code set, and nothing client-visible differs.

Without an external witness, a writer holding the runtime credential who
appends rows that extend a chain consistently, or an owner-level writer who
rewrites a whole chain consistently from genesis and its checkpoint, cannot
be detected: the chain hash is unkeyed and the verifier only checks that the
rows agree with each other and with the checkpoint. Signed external
checkpoints are out of scope for this phase; until then, keeping the runtime
and migrator credentials secret is the control (see
[Trust boundary](#trust-boundary)).

## Backups

A database backup is the security backup: take a consistent PostgreSQL backup
(for example `pg_dump --format=custom`, or a physical base backup with WAL).
Keep, separately and securely, the instance ID, the current recovery epoch, the
permanent actor-ID pepper, the retained keyring material and every other
required secret. A restored database is useless without the original actor-ID
pepper: readiness and `authority verify` refuse it with
`actor_id_key_mismatch`.

## Restoring a database

Stop the server. Restore the database, then rotate the recovery epoch and
restart with it, because devices may hold sessions, refresh tokens and receipts
newer than the restored state:

```sh
baby-tracker-admin authority rotate-epoch --expected-epoch <current> \
  --reason-code database_restore --confirm-instance <instance-uuid>
# set BT_RECOVERY_EPOCH to the printed epoch, then start the server
baby-tracker-admin authority verify
```

`rotate-epoch` runs as the migrator, moves `server_state.recovery_epoch` from
`--expected-epoch` to the next epoch, and refuses a stale expected epoch or
another instance. Every session carries the epoch it was issued under, so every
earlier session then fails with `epoch_changed` and its user signs in again;
MCP grants bound to the old epoch stop authorizing. A node still configured for
the old epoch stays not ready (`server_identity_mismatch`) until restarted with
the new one.

## Integration grants

Read-only MCP integration grants are rows in `mcp_integrations` plus
`integration_granted`/`integration_revoked` audit rows in the family's chain,
issued only by prepared operator commands committed as one authority
transaction. A grant binds the account, family, server instance and recovery
epoch, the `family:read` scope, an operator-chosen client label (printable
ASCII; never a person's name) and only the lowercase SHA-256 verifier of the
credential secret; the plaintext secret is never stored. Every grant expires
exactly 7,776,000 seconds (90 days) after its commit time, captured after the
command's row locks; an exact retry returns the stored result and never
restamps or reactivates. `--expected-revision` is the family's audit sequence
(`baby-tracker-admin authority family-revision --family-id <uuid>`).

## Hosted mode

The hosted credential vault and Apple revocation worker are removed. Hosted
mode keeps `code_exchange_required`, entitlements, notifications and the
delivery queue; the Apple authorization code is exchanged and verified before
the sign-in transaction, and the Apple refresh token it returns is not
persisted. Account deletion therefore reports
`apple_authorization_revocation: "manual_required"` in both modes. No
production Apple code exchanger exists, so production hosted mode refuses to
start (`hosted_adapter_unavailable`); identitytest builds use the test
exchanger. Revoking Apple tokens in hosted mode is an open item for Phase 6.
The retired hosted provider settings (`BT_HOSTED_*`, replay scratch) and
revocation-worker settings are refused at startup.

## Retired settings

`BT_SECURITY_LEDGER_DIR`, `BT_LEDGER_HEAD_DIR`, `BT_PROJECTOR_DATABASE_URL`,
`BT_PROJECTOR_DATABASE_URL_FILE`, `BT_DB_PROJECTOR_MAX_CONNS`, every
`BT_HOSTED_*` provider setting, `BT_SECURITY_REPLAY_SCRATCH_PATH`,
`BT_SECURITY_REPLAY_SCRATCH_MAX_BYTES` and the Apple revocation worker's
`BT_APPLE_REVOCATION_*` and `BT_CREDENTIAL_ORPHAN_GRACE` are refused in every
mode, naming each setting, so a deployment written for the file ledger fails
at startup instead of running without the state it expects. The admin commands
`security init|bind-actor-key|verify|reconcile` and `recovery rotate-epoch` are
replaced by `authority init|verify|rotate-epoch`.

## Existing installs

The schema installs only on a fresh database: `migrate` refuses a database
recorded with any other migration history. The one file-ledger install, the
homelab, was upgraded on 2026-10-10 by a one-time importer, since deleted
(record: `docs/verification/2026-10-10-homelab-cutover.md`).

The beta permits one authority node; multi-node operation, read replicas and
external connection poolers are out of scope.
