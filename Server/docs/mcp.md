# Private read-only MCP: operator and desktop setup guide

This is the operator-facing setup and rollout guide for the private, tailnet-only,
read-only MCP endpoint (`list_babies`, `get_history`, `get_summary`). It complements,
and does not duplicate, three places that already carry the exact mechanics:

- [`Server/README.md`](../README.md) sections "Private MCP credentials (operator)"
  and "Private MCP endpoint" — the exact CLI grammar, refusal codes, env-var table,
  quota/gate/admission behaviour and the measured local limits.
- [`docs/verification/2026-10-03-mcp-mixed-load.md`](../../docs/verification/2026-10-03-mcp-mixed-load.md) —
  the mixed-load soak evidence behind the supported-limits numbers.
- [`docs/verification/2026-10-03-mcp.md`](../../docs/verification/2026-10-03-mcp.md) —
  the end-to-end proof, gate results and desktop client validation this guide's
  procedures were checked against.

This guide never contains a credential value, a DSN, or a label with a person's or
baby's name in it.

## 1. Issuing, listing and revoking a credential

Full grammar, every refusal code, the secret-file format and retry/replacement
semantics are in [README § "Private MCP credentials (operator)"](../README.md#private-mcp-credentials-operator).
In short:

```sh
baby-tracker-admin mcp issue prepare --command-id <uuid> --family-id <uuid> \
  --account-id <owner-uuid> --name <client-label> --scope family:read \
  --server-instance-id <uuid> --recovery-epoch <n> --validity-seconds 7776000 \
  --policy-id <policy> --evidence-reference <ref> --reason-code integration_issued \
  --expected-revision <head> --secret-output /private/dir/<file>
baby-tracker-admin mcp issue commit <the same proposal flags> --grant-id <uuid> \
  --verifier <hex> --secret-file /private/dir/<file> \
  --confirmation-digest <hex> --confirmation-expires-at <RFC3339>
baby-tracker-admin mcp list --family-id <uuid>
baby-tracker-admin mcp revoke prepare --command-id <uuid> --grant-id <uuid> ...
baby-tracker-admin mcp revoke commit <the same flags> ...
```

**Secret-file handling, the operational checklist:**

- `--secret-output` on `issue prepare` must be an absolute, clean path that does not
  yet exist, in a directory the operator owns with no group/other access (`0700`).
  The tool creates it exclusively at mode `0600`. It is never printed and never passed
  as a command-line flag.
- The file holds exactly the 87-byte credential (`btmcp1.<grant UUID>.<base64url
  secret>`), no trailing newline, nothing else. Treat it like a password: move it
  (don't copy-paste it through a terminal that logs scrollback) to wherever the
  desktop client config below reads it from, then delete the operator's copy once the
  desktop client has it, or keep exactly one durable copy under the same permission
  discipline.
- `issue commit` only ever reads that file; it never rewrites, regenerates or deletes
  it. If a commit is lost (process killed, output discarded), rerun the **identical**
  commit command with the same file — it returns the original receipt and issues no
  second grant.
- **Labels** (`--name`) are kept forever in the append-only authority log and are
  printed in plain text by `mcp list`. Never put a person's or a baby's name in one.
  Use a role/device shape instead, for example `claude-desktop-laptop` or
  `codex-cli-workstation`.
- **Replacement** is issue-then-revoke, not a refresh: there is no refresh token.
  Revoking is final — a second revocation attempt on the same grant is refused
  (`integration_revoked`).
- `mcp list` prints grant metadata only (ID, account, family, instance, epoch, label,
  scope, status, created/expiry times) and never the verifier.
- **Expiry** is exactly `--validity-seconds` (fixed at 7,776,000 — 90 days) after the
  committed issuance event, and a read is denied starting at the exact deadline, not
  only sometime after it: see `TestIntegrationReadExpiryAtExactDeadline`
  (`Server/internal/authority/integration_read_test.go`), which asserts a read still
  succeeds one microsecond (PostgreSQL's timestamp resolution) before expiry and is
  denied from the exact expiry instant onward. This is the test the end-to-end
  script's `grant expiry` line defers to, because the e2e run uses a real clock and
  cannot itself exercise a 90-day boundary.

**Cursor key file.** `BT_MCP_CURSOR_KEY_FILE` must hold exactly 64 lowercase hex
characters (one optional trailing newline), the same pepper convention used
elsewhere in this codebase — not 32 raw bytes. Generate one with
`openssl rand -hex 32 > /run/mcp/cursor_key && chmod 0444 /run/mcp/cursor_key`. An
all-zero key is rejected. This key is bound to one running server's history cursors;
it does not need to be escrowed for care-data recovery (cursors are a pagination
convenience, not an authority record), but it should be treated as a secret and
preserved across restarts of the same deployment so in-flight cursors keep working.

## 2. Enable/disable

Full env-var table and pool/admission rules are in
[README § "Private MCP endpoint"](../README.md#private-mcp-endpoint). Enabling
needs every one of: `BT_MCP_ENABLED=true`, `BT_MCP_DATABASE_URL_FILE` (the runtime
tenant role), `BT_MCP_CURSOR_KEY_FILE`, `BT_ADMISSION_MCP_MAX` (1–4, below
`BT_ADMISSION_TOTAL`), and a complete explicit `BT_DB_*` pool profile with
`BT_DB_MCP_MAX_CONNS` matching `BT_ADMISSION_MCP_MAX`. Any missing or inconsistent
value stops startup, not just the route.

**Disabling does not erase grants.** Setting `BT_MCP_ENABLED` back to `false` (or
unsetting it) removes the `/mcp` route (404), closes the MCP pool and stops reads.
It does **not** revoke any issued grant — the `mcp_integrations` rows are
untouched, and re-enabling with the same database makes every
non-expired, non-revoked grant work again immediately. If the intent is to
permanently cut off a specific client (lost laptop, decommissioned desktop,
compromised credential), **revoke that grant** through the admin CLI; disabling the
endpoint is an operational pause, not a disconnect procedure.

## 3. Supported limits and the ingress requirement

Measured locally (not on the Unraid node, not at the Track A rate-15 anchor): phone
sync stays healthy with up to four paced MCP clients at ≤20 calls/minute each, and
with one unpaced client using a **valid** credential (refused cheaply by its own gate
or quota), at MCP concurrency 1 and 4. Unpaced clients using an invalid, revoked or
expired credential, and any high-rate connection flood, are **not supported** — see
[the mixed-load evidence](../../docs/verification/2026-10-03-mcp-mixed-load.md) for
the measurements and mechanisms, and
[the end-to-end doc's soak section](../../docs/verification/2026-10-03-mcp.md#soak)
for a 15-minute confirmation at the documented supported shape (concurrency 1, four
clients at 20/min).

**Tailscale Serve has no connection or rate limit of its own.** A deployment that
must survive a misbehaving tailnet client needs a connection/rate limit in front of
the app (for example, a proxy between Tailscale Serve and the app), or must be
prepared to use tailnet ACLs to block that client's device/user outright. Revoking a
flooding client's credential does **not** by itself reduce its load: after at most a
minute of cached `429`s it starts paying full authority verification for a `401` on
every request. Treat a misbehaving client as an incident: block it at the ingress or
with a tailnet ACL/policy change, and revoke its credential as well.

## 4. Desktop setup

Both paths below use the credential launcher,
[`Server/scripts/mcp-credential-launcher.py`](../scripts/mcp-credential-launcher.py)
(standard library only, no third-party dependency). It never prints, logs or passes
the credential as a command-line argument. Copy it to a fixed path the desktop host
can execute, for example `/usr/local/bin/baby-tracker-mcp-credential`.

The credential file it reads must be a regular, non-symlink file owned by the
desktop user, mode `0600` or `0400`, in a directory with no group/other access,
holding exactly one `btmcp1...` credential (one trailing newline tolerated). Any
other shape — missing, wrong mode, a symlink, a malformed value, world-readable
directory — is refused before any network access, with one generic stderr line and
no leaked path or content.

**The client must serialize its requests on one credential.** The per-credential
gate admits exactly one in-flight request per credential (else `429
concurrency_limited`), and the recommended `BT_ADMISSION_MCP_MAX=1` means the
*global* MCP slot refuses a second request regardless of this gate — a client that
pipelines or fires concurrent tool calls on the same credential will be refused, not
queued. This is a deliberate trade (README § "Private MCP endpoint", mcp.md §3), and
real clients are not uniformly graceful about it: in this task's four-client
end-to-end proof, a mid-session `429` ended the Python MCP SDK's direct session
(`ClosedResourceError`), the pinned `mcp-proxy` bridge's own next call after the
refusal window also surfaced as an `McpError`, and Codex's app-server experienced it
as a bounded 15-second timeout before its following call succeeded — see
["Observed non-2xx / `id:null` / concurrent-429 behaviour by
client"](../../docs/verification/2026-10-03-mcp.md#observed-non-2xx--idnull--concurrent-429-behaviour-by-client).
No client ever returned family data on a refusal. Configure the desktop client (or
the bridge in front of it) to send at most one in-flight request per credential.

### 4.1 Claude Desktop (pinned `mcp-proxy` bridge)

Claude Desktop speaks stdio to local MCP servers, not Streamable HTTP directly, so a
small bridge sits between it and `/mcp`. Install the bridge from a pinned,
hash-locked checkout — never `pip install mcp-proxy` with a floating version:

```sh
git clone https://github.com/sparfenyuk/mcp-proxy.git /opt/mcp-proxy-bridge
cd /opt/mcp-proxy-bridge
git checkout --detach f1ae01420086011ae53e6d895b1cd02838b34f42   # the 0.12.0 release commit
uv sync --frozen --no-dev                                        # installs Python MCP 1.27.1 from the pinned lock
.venv/bin/mcp-proxy --version                                    # confirm 0.12.0
```

Add one entry to Claude Desktop's `mcpServers` map (merge, don't overwrite, any
existing entries), in
`~/Library/Application Support/Claude/claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "baby-tracker-readonly": {
      "command": "/usr/local/bin/baby-tracker-mcp-credential",
      "args": [
        "claude",
        "--credential-file", "/path/to/the/protected/credential-file",
        "--bridge", "/opt/mcp-proxy-bridge/.venv/bin/mcp-proxy",
        "--url", "https://<your-tailnet-host>/mcp",
        "--ca-file", "/path/to/a/custom/ca.pem"
      ]
    }
  }
}
```

Omit `--ca-file` when the server presents a certificate from a public CA chain
(the normal case for a real Tailscale Serve HTTPS certificate); only pass it for a
private/test CA. The launcher execs into the bridge (`os.execve`, same PID) with a
minimal environment containing only `API_ACCESS_TOKEN` (the credential) and
`PATH`/`HOME`/`LANG`/`LC_ALL`/`TMPDIR` passed through; the bridge never sees the
credential as a file path or argument. Quit and restart Claude Desktop after editing
the config; its own logs for this server land in
`~/Library/Logs/Claude/mcp-server-baby-tracker-readonly.log`.

**What this task's end-to-end proof actually exercised** (see
[the desktop-validation section](../../docs/verification/2026-10-03-mcp.md#desktop-validation)):
this exact launcher-to-bridge mechanism, driven by a Python MCP 1.27.1 stdio client
standing in for Claude Desktop's own stdio transport, against a disposable server
with a synthetic credential. **Actual Claude Desktop is not installed on the
machine that ran this proof and has not been exercised** — treat the GUI app's
acceptance of this configuration as open until it is checked on a machine that has
it.

### 4.2 Codex / ChatGPT desktop app

Codex CLI, the ChatGPT desktop app and the Codex IDE extension share one local MCP
configuration per Codex host (`~/.codex/config.toml` on this machine, or wherever
`CODEX_HOME` points). Add one server entry:

```toml
[mcp_servers.baby_tracker_readonly]
url = "https://<your-tailnet-host>/mcp"
http_headers_helper = "/usr/local/bin/baby-tracker-mcp-credential codex-headers --credential-file /path/to/the/protected/credential-file"
enabled_tools = ["list_babies", "get_history", "get_summary"]
startup_timeout_sec = 15
tool_timeout_sec = 15
```

`codex-headers` mode prints exactly `{"Authorization": "Bearer <credential>"}` to its
captured stdout and exits; that captured output is the intended delivery channel for
Codex's `http_headers_helper` — never run that mode interactively, and never let its
output reach a log. Use **one** credential-delivery mechanism for this server: do not
also set a literal `http_headers.Authorization`, `bearer_token_env_var`, or MCP OAuth
login on the same entry. `bearer_token_env_var` is the alternative if the desktop
host's own process is known to receive a protected environment variable (a terminal
`export` is not evidence that a Finder-launched GUI app inherits it — the helper
avoids that dependency entirely, which is why this guide defaults to it).

`enabled_tools` restricts what this client's catalog shows; it is a client-side UI
filter, not a security boundary — the server's own authorization is what actually
limits the three tools to read-only, family-scoped access regardless of what the
client's config lists.

After editing, `codex mcp list --json` should show the server, and `/mcp` inside a
Codex session (or the ChatGPT desktop app's own MCP settings panel) should show it
connected. If it does not connect, check the helper path is absolute and executable,
and that the credential file passes the launcher's checks above — the launcher never
explains which check failed, by design.

**What this task's end-to-end proof actually exercised:** this exact `config.toml`
shape and `http_headers_helper` delivery, driven by `codex app-server` directly (CLI
`codex-cli 0.160.0`) in a throwaway `CODEX_HOME`, against the disposable server.
**The ChatGPT desktop app's own GUI was not driven by this proof** (it is installed
on the machine that ran it, but the proof used the Codex CLI's app-server, which the
GUI shares configuration with, not the GUI itself) — treat the GUI's acceptance as
open.

### 4.3 What a desktop client does with the data

**A desktop MCP client may send everything a tool call returns — history, baby
names, notes — to its own configured model provider**, even though the transport
between the client and this server is a private, tailnet-only, no-OAuth connection.
Private transport is not private inference. Anyone issuing a credential for desktop
use should understand that the receiving application's own model-provider terms
apply to whatever it does with the returned care data.

## 5. Rollout notes

### 5.1 Source and image identity

Record, before any rollout: the exact 40-character git SHA being deployed, the
built image digest (`sha256:...`) for the target architecture, and the schema
version it expects (`28`, as of migration `000028_mcp_integrations.sql`). The
server's own `/v1/server` response and the `image proof:` / `source_sha=` lines this
task's e2e script emits are the pattern to compare against post-deploy — never trust
a mutable tag for this comparison.

### 5.2 Migration 000028 and the authority-event rollback constraint

> **Superseded by schema 29.** PostgreSQL is now the only security authority
> (`docs/operations/security-state.md`): `000029_postgres_authority.sql`
> recreates `mcp_integrations` as an authority table written directly by the
> operator commands, with `integration_granted`/`integration_revoked` audit rows
> in the family's chain; there is no ledger or projector. Schema 29 installs
> only on a fresh database, so there is no rollback from 29 to 28. The note
> below describes the schema-28 file-ledger release.

`000028_mcp_integrations.sql` adds the `mcp_integrations` projection table and a new
`project_security_event` function version that understands `integration_granted` and
`integration_revoked` authority events. It grants no privilege to any existing
runtime, projector or maintenance role — this table is read/written only through its
own `SECURITY DEFINER` entrypoint, so the privilege manifest for every other table is
unchanged.

**This migration is not overlap-safe in the rollback direction once an integration
event has been committed.** A schema-27 binary refuses to start against schema 28 (it
rejects migration 28 as unknown) — that direction fails closed and loses nothing. But
a schema-28 binary that has **committed** an `integration_granted` or
`integration_revoked` event cannot be rolled back to a schema-27 image: the old
binary's projector does not know how to project those events, so a rollback would
either strand the authority ledger ahead of its projection or require manually
replaying the missing events once a compatible binary returns. Concretely:

- **Before the first integration grant is issued** (MCP enabled but unused, or MCP
  disabled entirely), rolling back to the pre-`000028` image and schema is safe in
  the ordinary way this project already supports.
- **After even one grant has been issued or revoked**, rollback requires a binary
  that both understands those authority events *and* carries this branch's **final**
  `000028_mcp_integrations.sql` byte-for-byte. The migration was rewritten after it
  was first introduced: `fdd26c5` added the table, and a later commit on this branch
  (`d02c7e5`) added seven indexes to it. `ValidateCompatibility`
  (`internal/migrate/compatibility.go:62`) compares each migration's name **and**
  SHA-256, so a binary built from a commit between those two points carries a
  migration 28 whose hash does not match the one already applied to the database —
  it fails closed at startup ("unknown or changed migration 28"), which is the
  opposite of what an operator following this note in an incident needs. The safe
  bound is therefore: **any build that carries this branch's final
  `000028_mcp_integrations.sql`** — manifest SHA
  `406c2c5874c00aa724e61c1aaf462a7376cf5ae1dd89d883eb7d4a238dc9a347` for migration 28
  in `internal/migrate/compatibility.json` — i.e. the merged branch or later. **Never
  roll back to an intermediate commit on this branch**, even one that appears later
  in the log by date; if in doubt, check that commit's `compatibility.json` entry for
  migration 28 against the SHA above before using it for a rollback.

### 5.3 Protected files

- The operator's secret-output file for each issued credential, until it has been
  delivered to its desktop client and either destroyed or retained under the same
  `0600`/owner-only discipline as a password.
- `BT_MCP_DATABASE_URL_FILE` and `BT_MCP_CURSOR_KEY_FILE` (and the equivalent
  operator-side `BT_OPERATOR_INTEGRATION_POLICY_ID_FILE` /
  `BT_OPERATOR_CONFIRMATION_KEY_FILE` for the admin tool) — all regular,
  non-symlink, `0600` files the running process's UID owns, exactly like every other
  protected secret file this project already uses (see README § "Configuration and
  secrets").
- The database backup (see README § "Network, state and backups"): with
  schema 29 it is the whole security authority, MCP grants included.

### 5.4 Backup and restore

MCP introduces no new backup procedure. With schema 29, `mcp_integrations` and the
family audit chains are part of the database, so a database backup recovers every
grant and revocation; `baby-tracker-admin authority verify` checks the restored
chains. No separate MCP-specific backup target exists, and none should be added.

A recovery-epoch rotation (database restore) retires **every** grant issued at the
prior epoch — this is existing Task-1/2 authority behaviour, exercised by this task's
e2e `epoch` phase, not something new to document procedurally: operators must
re-issue desktop credentials after a restore-triggered epoch rotation, the same way
phone sessions are affected.

### 5.5 Enable/disable procedure

See [§2](#2-enabledisable) above for the operational meaning. Mechanically:

- **Enable:** set the five required env vars (`BT_MCP_ENABLED`, both `_FILE` vars,
  `BT_ADMISSION_MCP_MAX`, the explicit `BT_DB_*` profile with `BT_DB_MCP_MAX_CONNS`
  set), restart the app container. Startup validates the whole composition and fails
  closed on any inconsistency — if the app comes up, the composition was accepted.
- **Disable:** set `BT_MCP_ENABLED=false` (or remove it), restart. The route
  disappears (404); no secret is read; every issued grant remains intact and will
  work again the moment MCP is re-enabled.
- **Disconnect a specific client:** revoke its grant through `mcp revoke
  prepare`/`commit` (see §1). This is independent of enable/disable and is the
  correct action when a desktop, not the whole feature, needs to be cut off.

### 5.6 Concrete rollout proposal (for later, separate approval)

This is a proposal only. Nothing in Task 8 authorizes running it; it is written so a
later approval has a concrete plan to approve or amend, not to be executed by this
task.

**Preconditions.** The reviewed branch head is merged to the deployment branch; a
linux/amd64 image is built and attested at that exact source SHA (§5.1); the current
production database/ledger/head are backed up at a consistent point (§5.4); the
Tailscale Serve Host-preservation check below has passed on the actual production
reverse-proxy configuration; and a connection/rate limit is in place at the tailnet
ingress (Tailscale Serve or a proxy in front of the app, sized per client) — see
["Deployment requirement"](../../docs/verification/2026-10-03-mcp-mixed-load.md#final-supported-limits)
in the mixed-load evidence. Tailscale Serve has no connection or rate limit of its
own, and revoking a flooding client's credential does not by itself reduce its load
(§3), so this is a precondition for rollout, not advice to apply only if trouble
occurs.

**Steps:**

1. Back up the current production database, ledger and head volumes together.
2. Deploy the new image with `BT_MCP_ENABLED` left at its default (`false`/unset).
   This applies migration `000028` and every other pending migration, and brings the
   app up with the MCP route absent (404) — a pure schema/code rollout with no new
   surface exposed yet. Verify `/readyz` 200, schema version, unchanged record
   counts, and clean logs, exactly as every prior rollout in `TASKS.md` already does.
3. **Separately and explicitly**, provision the MCP secrets
   (`BT_MCP_DATABASE_URL_FILE`, `BT_MCP_CURSOR_KEY_FILE`) and the operator's
   integration policy file, and restart with `BT_MCP_ENABLED=true` plus the explicit
   pool/admission values (start at `BT_ADMISSION_MCP_MAX=1` /
   `BT_DB_MCP_MAX_CONNS=1`, the conservative measured-supported value from
   [the mixed-load evidence](../../docs/verification/2026-10-03-mcp-mixed-load.md)).
   Verify an unauthenticated `POST /mcp` returns 401 (route present, nothing
   authorizes without a credential) and that `/readyz` and phone sync are
   unaffected.
4. **Verify the Tailscale Serve Host-preservation assumption on the real proxy**,
   before issuing any real credential: send an authenticated-looking request through
   the actual `https://<tailnet-host>/mcp` path and confirm the app sees
   `Host` equal to the canonical origin's host (the SDK's own localhost
   DNS-rebinding protection, and this server's canonical-Host check, both depend on
   that). This was only a source-level assumption in Task 7a and was modeled (not
   measured against real Tailscale Serve) by this task's disposable nginx front — see
   [Limitations](../../docs/verification/2026-10-03-mcp.md#limitations). Do not issue
   a production grant until this step has actually passed against the real ingress.
5. Issue exactly one credential for one real desktop client, using a label with no
   person's or baby's name (§1). Verify the desktop client connects, lists the three
   tools, and reads real data correctly.
6. Expand to additional desktop clients/credentials only after step 5 is stable, each
   with its own grant.

**Verification at every step:** reuse this project's existing rollout checklist
(private HTTPS health/readiness, exact image/source identity, unchanged record
counts, clean bounded logs, container isolation) plus the MCP-specific checks above
(401 on unauthenticated `/mcp`, Host preservation, one working real client before
expanding).

**Rollback:**

- Before step 3 (MCP never enabled in production): roll back the image/schema
  exactly as any other rollout in this project already does. No MCP-specific
  constraint applies.
- After step 3 but before any grant is issued (MCP enabled, zero grants): set
  `BT_MCP_ENABLED=false` and restart; this is a safe, ordinary disable. The image can
  still be rolled back to pre-`000028` if needed, because no authority event has
  referenced the new projection.
- **After any grant has been issued or revoked:** see §5.2 — rollback is limited to a
  binary that both understands `integration_granted`/`integration_revoked` events
  *and* carries this branch's final `000028_mcp_integrations.sql` (manifest SHA
  `406c2c5874c00aa724e61c1aaf462a7376cf5ae1dd89d883eb7d4a238dc9a347`); never roll back
  to an intermediate commit on this branch. If a problem is found after this point,
  prefer disabling MCP (§5.5) or revoking the affected grant(s) over rolling back the
  image.

## 6. Open acceptance

Unchanged from [the end-to-end doc](../../docs/verification/2026-10-03-mcp.md#open-acceptance):
actual Claude Desktop, actual ChatGPT desktop app, any desktop-over-Tailscale test,
and any production grant issuance or deployment, remain explicitly open until
separately authorized and exercised against the real endpoint.
