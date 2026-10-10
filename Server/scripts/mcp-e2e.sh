#!/usr/bin/env bash
# Disposable end-to-end proof of the private read-only MCP endpoint.
#
# Owns one uniquely named Compose project (PostgreSQL, the identity-test
# server image built from this tree with MCP enabled, an nginx TLS front at
# the canonical origin https://localhost:<port>) plus a private temporary
# directory. Credentials are issued only through the real admin binary's
# `mcp issue|revoke` operator flow. Clients: the Go SDK (v1.8.0, this module),
# the independent Python MCP SDK 1.27.1 directly, the pinned mcp-proxy 0.12.0
# stdio bridge (the Claude Desktop path) and, when installed, Codex's native
# Streamable HTTP client through a throwaway CODEX_HOME. Everything owned is
# removed on exit, success or failure, and a watchdog bounds the whole run.
#
# Output is sanitized: counts, statuses, codes and source identity only.
# Credentials, phone tokens and records stay in the private directory.
set -euo pipefail

cd "$(dirname "$0")/../.."

if [[ -n ${BABY_TRACKER_TEST_DATABASE_URL:-} ]]; then
  echo "mcp-e2e refuses an externally supplied database; it owns a disposable stack" >&2
  exit 2
fi
for tool in docker curl jq openssl go git shasum python3; do
  command -v "$tool" >/dev/null || { echo "mcp-e2e requires $tool" >&2; exit 2; }
done
mcp_e2e_timeout=${BT_MCP_E2E_TIMEOUT_SECONDS:-2400}
if [[ ! $mcp_e2e_timeout =~ ^[0-9]+$ ]] || (( mcp_e2e_timeout < 600 || mcp_e2e_timeout > 5400 )); then
  echo "BT_MCP_E2E_TIMEOUT_SECONDS must be 600 to 5400" >&2
  exit 2
fi

# Watchdog: TERM this runner and its children when the bound expires; the
# runner's own EXIT trap then removes everything it owns.
runner_pid=$$
(
  # The sleeper must not hold the runner's stdout open after cleanup.
  sleep "$mcp_e2e_timeout" >/dev/null 2>&1 &
  sleeper=$!
  trap 'kill "$sleeper" 2>/dev/null; exit 0' TERM
  wait "$sleeper"
  echo "mcp-e2e watchdog: ${mcp_e2e_timeout}s bound expired" >&2
  pkill -TERM -P "$runner_pid" 2>/dev/null || true
  kill -TERM "$runner_pid" 2>/dev/null || true
) &
watchdog_pid=$!

# MCP enabled at a supported limit: concurrency 2 (inside the supported
# 1-4), so both the pre-authentication server_busy and the per-credential
# concurrency_limited refusals are reachable. Default quotas.
export BT_MCP_ENABLED=true
export BT_MCP_DATABASE_URL_FILE=/run/secrets/runtime_database_url
export BT_MCP_CURSOR_KEY_FILE=/run/mcp/cursor_key
export BT_ADMISSION_MCP_MAX=2
export BT_DB_RUNTIME_MAX_CONNS=16 BT_DB_MAINTENANCE_MAX_CONNS=2 BT_DB_MCP_MAX_CONNS=2
export BT_DB_SERVER_MAX_CONNECTIONS=64 BT_DB_RESERVED_CONNECTIONS=8
mcp_operator_id=90000000-0000-4000-8000-0000000e2e01
mcp_policy_id=mcp-e2e-policy-v1
nginx_image=nginx:1.27-alpine@sha256:65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10

# Called by start_disposable_sync before its first Compose command.
disposable_sync_customize() {
  local mcp_dir="$BT_SECRETS_DIR/mcp"
  mkdir -m 0755 "$mcp_dir"
  openssl rand -hex 32 > "$mcp_dir/cursor_key"
  chmod 0444 "$mcp_dir/cursor_key"
  export BT_MCP_SECRETS_DIR="$mcp_dir"
  mkdir -m 0700 "$BT_SECRETS_DIR/clients" "$BT_SECRETS_DIR/logs"
  # Pass the client's Host through unchanged, as Tailscale Serve is expected
  # to: the MCP handler requires Host to equal the canonical origin's host.
  chmod 0600 "$BT_SECRETS_DIR/nginx.conf"
  python3 - "$BT_SECRETS_DIR/nginx.conf" <<'NGINX'
import sys
p = sys.argv[1]
s = open(p).read()
assert s.count('proxy_set_header Host $host;') == 1
open(p, 'w').write(s.replace('proxy_set_header Host $host;', 'proxy_set_header Host $http_host;'))
NGINX
  chmod 0444 "$BT_SECRETS_DIR/nginx.conf"
  # Serve a CA-issued leaf, as a real Tailscale certificate is, instead of
  # the library's self-signed CA:TRUE certificate: rustls clients (Codex)
  # reject a CA certificate presented as the server's own leaf. Clients trust
  # ca.crt; nginx serves leaf + CA.
  local pki="$BT_SECRETS_DIR/pki"
  mkdir -m 0700 "$pki"
  openssl req -x509 -newkey rsa:2048 -sha256 -days 1 -nodes -keyout "$pki/ca.key" -out "$BT_SECRETS_DIR/ca.crt" \
    -subj "/CN=mcp-e2e disposable CA" -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign" >/dev/null 2>&1
  openssl req -newkey rsa:2048 -sha256 -nodes -keyout "$pki/leaf.key" -out "$pki/leaf.csr" -subj "/CN=localhost" >/dev/null 2>&1
  printf '%s\n' 'basicConstraints=critical,CA:FALSE' 'keyUsage=critical,digitalSignature,keyEncipherment' \
    'extendedKeyUsage=serverAuth' 'subjectAltName=DNS:localhost,IP:127.0.0.1' > "$pki/leaf.ext"
  openssl x509 -req -in "$pki/leaf.csr" -CA "$BT_SECRETS_DIR/ca.crt" -CAkey "$pki/ca.key" -CAcreateserial -days 1 -sha256 \
    -extfile "$pki/leaf.ext" -out "$pki/leaf.crt" >/dev/null 2>&1
  chmod 0600 "$BT_SECRETS_DIR/tls.crt" "$BT_SECRETS_DIR/tls.key"
  cat "$pki/leaf.crt" "$BT_SECRETS_DIR/ca.crt" > "$BT_SECRETS_DIR/tls.crt"
  cat "$pki/leaf.key" > "$BT_SECRETS_DIR/tls.key"
  chmod 0444 "$BT_SECRETS_DIR/tls.crt" "$BT_SECRETS_DIR/tls.key" "$BT_SECRETS_DIR/ca.crt"
  rm -f "$pki"/*
  # The operator runs the real admin binary in its own short-lived container
  # with the app's configuration (the database is the authority) and a private
  # operator volume (policy, confirmation key, secret output). operator-init
  # prepares that volume as root and copies issued credentials out.
  printf '%s\n' \
    '  operator:' \
    '    extends:' \
    "      file: $PWD/Server/compose.yaml" \
    '      service: app' \
    '    profiles: [tools]' \
    '    entrypoint: [/usr/local/bin/baby-tracker-admin]' \
    '    ports: !reset []' \
    '    healthcheck:' \
    '      disable: true' \
    '    environment:' \
    "      BT_OPERATOR_ID: $mcp_operator_id" \
    '      BT_OPERATOR_INTEGRATION_POLICY_ID_FILE: /run/mcp-operator/config/policy_id' \
    '      BT_OPERATOR_CONFIRMATION_KEY_FILE: /run/mcp-operator/config/confirmation_key' \
    '    volumes:' \
    '      - mcp_operator:/run/mcp-operator' \
    '  operator-init:' \
    "    image: $nginx_image" \
    '    profiles: [tools]' \
    '    user: "0:0"' \
    '    network_mode: none' \
    '    entrypoint: [/bin/sh, -ec]' \
    '    volumes:' \
    '      - mcp_operator:/run/mcp-operator' \
    'volumes:' \
    '  mcp_operator:' >> "$override"
}

source Server/scripts/lib/disposable-sync.sh
start_disposable_sync self_hosted normal
# The library's EXIT trap owns the stack; extend it with this runner's own
# processes (watchdog, any bridge or Codex child) before handing over.
trap 'mcp_status=$?; trap - EXIT; mcp_e2e_cleanup; stop_disposable_sync "$mcp_status"; exit $?' EXIT
mcp_e2e_cleanup() {
  set +e
  kill -TERM "$watchdog_pid" 2>/dev/null
  wait "$watchdog_pid" 2>/dev/null
  pkill -TERM -f "$BT_SECRETS_DIR/" 2>/dev/null
  sleep 0.5
  pkill -KILL -f "$BT_SECRETS_DIR/" 2>/dev/null
  if pgrep -f "$BT_SECRETS_DIR/" >/dev/null 2>&1; then
    echo "mcp-e2e cleanup FAILED: a client process outlived the run" >&2
    mcp_status=1
  else
    echo "cleanup proof: no bridge, launcher or Codex process from this run remains"
  fi
  # The operator volume belongs to profile-only services, which a plain
  # `down` does not consider; the venv and test links are symlinks, which the
  # library's file sweep leaves behind.
  compose --profile tools down --volumes --remove-orphans >/dev/null 2>&1
  if [[ $BT_SECRETS_DIR == "${TMPDIR:-/tmp}/baby-tracker-protocol."* && -d $BT_SECRETS_DIR ]]; then
    rm -rf "$BT_SECRETS_DIR/venv"
    find "$BT_SECRETS_DIR" -type l -delete
  fi
}

clients="$BT_SECRETS_DIR/clients"
logs="$BT_SECRETS_DIR/logs"
recovery_epoch=$BT_RECOVERY_EPOCH

compose up -d --wait --wait-timeout 60 tls >/dev/null
origin="$BT_PUBLIC_ORIGIN"
curl --silent --fail --max-time 5 --cacert "$BT_SECRETS_DIR/ca.crt" "$origin/readyz" >/dev/null
[[ $(curl --silent --max-time 5 -o /dev/null -w '%{http_code}' --cacert "$BT_SECRETS_DIR/ca.crt" -X POST "$origin/mcp") == 401 ]]
echo "route proof: /mcp enabled at $origin (unauthenticated POST 401)"

refresh_app_binding() {
  local binding
  binding=$(compose port app 8080)
  [[ $binding == 127.0.0.1:* ]]
  app_base="http://$binding"
}
refresh_app_binding

restart_app() {
  local before after
  before=$(docker inspect --format '{{.State.StartedAt}}' "$app")
  compose restart app >/dev/null
  after=$(docker inspect --format '{{.State.StartedAt}}' "$app")
  [[ $before != "$after" ]]
  refresh_app_binding
  for _ in $(seq 1 150); do
    curl --silent --fail --max-time 1 "$app_base/readyz" >/dev/null && break
    sleep 0.2
  done
  curl --silent --fail --max-time 2 "$app_base/readyz" >/dev/null
  compose restart tls >/dev/null
  for _ in $(seq 1 50); do
    curl --silent --fail --max-time 1 --cacert "$BT_SECRETS_DIR/ca.crt" "$origin/readyz" >/dev/null && break
    sleep 0.2
  done
}

# --- operator --------------------------------------------------------------
init_operator() {
  quiet_compose_run operator-init \
    'mkdir -p /run/mcp-operator/config /run/mcp-operator/secrets; chown -R 65532:65532 /run/mcp-operator; chmod 0700 /run/mcp-operator /run/mcp-operator/config /run/mcp-operator/secrets' >/dev/null
  printf '%s\n' "$mcp_policy_id" | quiet_compose_run operator-init \
    'umask 077; cat > /run/mcp-operator/config/policy_id; chown 65532:65532 /run/mcp-operator/config/policy_id; chmod 0600 /run/mcp-operator/config/policy_id' >/dev/null
  openssl rand -hex 32 | quiet_compose_run operator-init \
    'umask 077; cat > /run/mcp-operator/config/confirmation_key; chown 65532:65532 /run/mcp-operator/config/confirmation_key; chmod 0600 /run/mcp-operator/config/confirmation_key' >/dev/null
}
# Compose prints container lifecycle lines on stderr; keep only real errors.
quiet_compose_run() { compose run --rm --no-deps -T "$@" 2> >(grep -v -E '^ ?Container .* (Creating|Created)' >&2); }
operator() { quiet_compose_run operator "$@"; }
uuid() { python3 -c 'import uuid; print(uuid.uuid4())'; }
# authority_revision <family>: the family's audit seq, the value operators
# pass as --expected-revision.
authority_revision() {
  local answer
  answer=$(operator authority family-revision --family-id "$1")
  [[ $answer =~ ^family_id=$1\ revision=([0-9]+)$ ]] || { echo "authority family-revision failed" >&2; return 1; }
  printf '%s' "${BASH_REMATCH[1]}"
}

# issue <label> <family> <account>: prepare + commit; the credential is
# copied into $clients/cred-<label> (0600) and never printed. Sets
# issued_grant and issued_commit (the exact commit argv) for retry checks.
issue() {
  local label=$1 family=$2 account=$3 command revision prepared grant verifier digest expires
  command=$(uuid)
  revision=$(authority_revision "$family")
  local proposal=(--command-id "$command" --family-id "$family" --account-id "$account" --name "e2e-$label"
    --scope family:read --server-instance-id "$BT_SERVER_INSTANCE_ID" --recovery-epoch "$recovery_epoch"
    --validity-seconds 7776000 --policy-id "$mcp_policy_id" --evidence-reference "mcp_e2e_$label"
    --reason-code integration_issued --expected-revision "$revision")
  prepared=$(operator mcp issue prepare "${proposal[@]}" --secret-output "/run/mcp-operator/secrets/$label")
  grant=$(jq -r '.proposal.grant_id' <<< "$prepared")
  verifier=$(jq -r '.proposal.verifier' <<< "$prepared")
  digest=$(jq -r '.confirmation.digest' <<< "$prepared")
  expires=$(jq -r '.confirmation.expires_at' <<< "$prepared")
  [[ $grant =~ ^[0-9a-f-]{36}$ && $verifier =~ ^[0-9a-f]{64}$ && $digest =~ ^[0-9a-f]{64}$ ]]
  issued_commit=(mcp issue commit "${proposal[@]}" --grant-id "$grant" --verifier "$verifier"
    --secret-file "/run/mcp-operator/secrets/$label" --confirmation-digest "$digest" --confirmation-expires-at "$expires")
  issued_receipt=$(operator "${issued_commit[@]}")
  [[ $(jq -r '.outcome' <<< "$issued_receipt") == committed ]] || {
    echo "issue $label: unexpected outcome $(jq -r '.outcome,.code' <<< "$issued_receipt" | tr '\n' ' ')" >&2; return 1; }
  quiet_compose_run operator-init "cat /run/mcp-operator/secrets/$label" > "$clients/cred-$label"
  chmod 0600 "$clients/cred-$label"
  [[ $(wc -c < "$clients/cred-$label") -eq 87 ]]
  issued_grant=$grant
  printf -v "grant_${label//-/_}" '%s' "$grant"
}

revoke() {
  local grant=$1 family=$2 account=$3 command revision prepared digest expires
  command=$(uuid)
  revision=$(authority_revision "$family")
  local proposal=(--command-id "$command" --grant-id "$grant" --family-id "$family" --account-id "$account"
    --server-instance-id "$BT_SERVER_INSTANCE_ID" --recovery-epoch "$recovery_epoch" --policy-id "$mcp_policy_id"
    --evidence-reference mcp_e2e_revoke --reason-code integration_revoked --expected-revision "$revision")
  prepared=$(operator mcp revoke prepare "${proposal[@]}")
  digest=$(jq -r '.confirmation.digest' <<< "$prepared")
  expires=$(jq -r '.confirmation.expires_at' <<< "$prepared")
  revoked_commit=(mcp revoke commit "${proposal[@]}" --confirmation-digest "$digest" --confirmation-expires-at "$expires")
  revoked_receipt=$(operator "${revoked_commit[@]}")
}

# Expect an admin refusal with a given stable code, and no grant change.
expect_refusal() {
  local code=$1; shift
  local output
  if output=$(operator "$@" 2>&1); then
    echo "admin unexpectedly accepted a refused proposal ($code)" >&2
    return 1
  fi
  [[ $output == *"$code"* ]] || { echo "admin refusal lacked $code" >&2; return 1; }
}

# --- clients ---------------------------------------------------------------
go_phase() {
  local phase=$1 log="$logs/go-$1.log"
  (cd Server && BT_MCP_E2E_PHASE=$phase BT_MCP_E2E_ORIGIN=$origin BT_MCP_E2E_APP=$app_base BT_MCP_E2E_DIR=$clients \
    BT_MCP_E2E_CA="$BT_SECRETS_DIR/ca.crt" go test -count=1 -timeout 300s -run '^TestMCPEndToEnd$' -v ./scripts) > "$log" 2>&1 || {
    grep -E '^(---|\s+mcp_e2e_test\.go)' "$log" >&2 || true
    echo "go phase $phase failed" >&2
    return 1
  }
  grep "^mcp-e2e proof: $phase " "$log"
}

# Every authenticated request is charged against fixed one-minute quotas
# (60 per account). Client phases that share one owner start in a fresh
# wall-clock minute so the phase, not leftover traffic, sets the count.
fresh_minute() {
  local second
  second=$(date +%S)
  sleep $(( 61 - 10#$second ))
}

venv="$BT_SECRETS_DIR/venv"
python_client() {
  local mode=$1 log="$logs/python-$1.log"
  MCP_E2E_URL="$origin/mcp" MCP_E2E_APP="$app_base" MCP_E2E_CA="$BT_SECRETS_DIR/ca.crt" MCP_E2E_DIR="$clients" \
    MCP_E2E_LOGS="$logs" MCP_E2E_LAUNCHER="$PWD/Server/scripts/mcp-credential-launcher.py" MCP_E2E_PYTHON="$venv/bin/python" \
    MCP_E2E_BRIDGE="$venv/bin/mcp-proxy" "$venv/bin/python" Server/scripts/lib/mcp_e2e_client.py "$mode" > "$log" 2>&1 || {
    tail -n 20 "$log" >&2
    echo "python client $mode failed" >&2
    return 1
  }
  grep '^mcp-e2e python:' "$log" || true
  grep "^mcp-e2e proof: python-$mode " "$log"
}

# The Python clients are installed from the committed hash lock into a
# disposable venv: no resolver, no unpinned transitive, binary wheels only.
python3 -m venv "$venv"
"$venv/bin/python" -m pip install --quiet --disable-pip-version-check --no-input --require-hashes --no-deps \
  --only-binary=:all: -r Server/scripts/lib/mcp-e2e-requirements.txt > "$logs/pip.log" 2>&1 || { tail -n 20 "$logs/pip.log" >&2; exit 1; }
python_identity=$("$venv/bin/python" -c 'import importlib.metadata as m, platform; print("python=%s mcp=%s mcp-proxy=%s httpx=%s" % (platform.python_version(), m.version("mcp"), m.version("mcp-proxy"), m.version("httpx")))')
echo "client proof: go-sdk=$(cd Server && go list -m -f '{{.Version}}' github.com/modelcontextprotocol/go-sdk) $python_identity"

# --- the run ---------------------------------------------------------------
init_operator
go_phase setup
state="$clients/state.json"
family_a=$(jq -r '.family_a' "$state"); family_b=$(jq -r '.family_b' "$state")
owner_a=$(jq -r '.owner_a.account_id' "$state"); owner_b=$(jq -r '.owner_b.account_id' "$state")
caregiver_a=$(jq -r '.caregiver_a.account_id' "$state")

# Operator refusals: a caregiver is not the owner; an owner cannot be bound
# to someone else's family. Nothing is written for either.
for refused in "$caregiver_a:$family_a" "$owner_a:$family_b"; do
  account=${refused%%:*}; family=${refused##*:}
  revision=$(authority_revision "$family")
  expect_refusal owner_required mcp issue prepare --command-id "$(uuid)" --family-id "$family" --account-id "$account" \
    --name e2e-refused --scope family:read --server-instance-id "$BT_SERVER_INSTANCE_ID" --recovery-epoch "$recovery_epoch" \
    --validity-seconds 7776000 --policy-id "$mcp_policy_id" --evidence-reference mcp_e2e_refused --reason-code integration_issued \
    --expected-revision "$revision" --secret-output /run/mcp-operator/secrets/refused
done
[[ $(quiet_compose_run operator-init 'ls /run/mcp-operator/secrets | wc -l' | tr -d ' ') == 0 ]]
echo "operator proof: caregiver and cross-family proposals refused with owner_required; no secret file or grant created"

issue go-a "$family_a" "$owner_a"
first_receipt=$issued_receipt
retry_receipt=$(operator "${issued_commit[@]}")
[[ $retry_receipt == "$first_receipt" ]]
issue py-a "$family_a" "$owner_a"
issue codex-a "$family_a" "$owner_a"
issue b "$family_b" "$owner_b"
listed_a=$(operator mcp list --family-id "$family_a")
listed_b=$(operator mcp list --family-id "$family_b")
[[ $(jq '[.integrations[] | select(.status == "active")] | length' <<< "$listed_a") == 3 ]]
[[ $(jq '[.integrations[] | select(.status == "active")] | length' <<< "$listed_b") == 1 ]]
[[ $(jq '[.integrations[] | select(.expires_at != null and .created_at != null)] | length' <<< "$listed_a") == 3 ]]
if grep -qi verifier <<< "$listed_a$listed_b"; then echo "mcp list printed a verifier" >&2; exit 1; fi
expiry_seconds=$(jq -r '.integrations[0] | ((.expires_at | fromdateiso8601) - (.created_at | fromdateiso8601))' <<< "$listed_a")
[[ $expiry_seconds == 7776000 ]]
echo "operator proof: issued 4 grants via mcp issue prepare/commit; exact commit retry returned the identical receipt; list shows 3+1 active, 90-day expiry ($expiry_seconds s), no verifier"

# The read phase includes the MCP credential's refused /v1/sync write. Every
# care write (a create, an update, a delete or a refusal's receipt) advances
# or charges the family row, so its sequence, its charged bytes and its
# receipt count must not move; the diaper count pins the seed.
family_a_writes() {
  docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT next_sequence || ' ' || care_charged_bytes || ' ' || (SELECT count(*) FROM care_receipts WHERE family_id='$family_a') FROM families WHERE id='$family_a'"
}
diapers_before=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT count(*) FROM care_entities WHERE family_id='$family_a' AND kind=4")
writes_before=$(family_a_writes)
go_phase read
writes_after=$(family_a_writes)
diapers_after=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT count(*) FROM care_entities WHERE family_id='$family_a' AND kind=4")
[[ $diapers_before == 3 && $diapers_after == 3 ]]
[[ $writes_before =~ ^[1-9][0-9]*\ [1-9][0-9]*\ [0-9]+$ && $writes_after == "$writes_before" ]]
sequence_before=${writes_before%% *}; sequence_after=${writes_after%% *}
# Care kinds: 3 sleep, 4 diaper (000030 section 1).
history_rows=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT count(*) FROM care_entities WHERE family_id='$family_a' AND kind IN (3, 4) AND tombstoned_at IS NULL AND started_at >= '2026-09-20' AND started_at < '2026-09-21'")
[[ $history_rows == 4 ]]
echo "database proof: PostgreSQL holds $history_rows family-A records in range (matches MCP history); MCP credential on /v1/sync wrote nothing (family sequence $sequence_before -> $sequence_after; sequence, charged bytes, receipts $writes_before -> $writes_after; diapers $diapers_before -> $diapers_after)"

fresh_minute
python_client direct
fresh_minute
python_client bridge
if command -v codex >/dev/null; then
  fresh_minute
  CODEX_BIN=$(command -v codex) python_client codex
else
  echo "codex: not installed; native Streamable HTTP client not exercised"
fi

fresh_minute
go_phase cancel
active=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT count(*) FROM pg_stat_activity WHERE usename='baby_tracker_app' AND state <> 'idle' AND pid <> pg_backend_pid()")
[[ $active == 0 ]]
echo "database proof: no runtime-role query left running after cancellations"

restart_app
go_phase restart

revoke "$grant_go_a" "$family_a" "$owner_a"
[[ $(jq -r '.outcome' <<< "$revoked_receipt") == committed ]]
revoke_retry=$(operator "${revoked_commit[@]}")
[[ $revoke_retry == "$revoked_receipt" ]]
revision=$(authority_revision "$family_a")
expect_refusal integration_revoked mcp revoke prepare --command-id "$(uuid)" --grant-id "$grant_go_a" --family-id "$family_a" \
  --account-id "$owner_a" --server-instance-id "$BT_SERVER_INSTANCE_ID" --recovery-epoch "$recovery_epoch" --policy-id "$mcp_policy_id" \
  --evidence-reference mcp_e2e_revoke --reason-code integration_revoked --expected-revision "$revision"
[[ $(operator mcp list --family-id "$family_a" | jq -r --arg g "$grant_go_a" '.integrations[] | select(.grant_id == $g) | .status') == revoked ]]
echo "operator proof: revoke committed; exact retry identical; second revocation refused (integration_revoked); list shows revoked"
go_phase revoked
restart_app
go_phase revoked

fresh_minute
go_phase quota

# Recovery epoch: a restore-style rotation retires every epoch-1 grant; a
# grant issued at the new epoch works.
compose stop app >/dev/null
rotated=$(compose run --rm --no-deps -T migrate authority rotate-epoch --expected-epoch "$recovery_epoch" --reason-code database_restore --confirm-instance "$BT_SERVER_INSTANCE_ID")
[[ $rotated == *"epoch=$((recovery_epoch + 1))"*"result=rotated"* ]]
recovery_epoch=$((recovery_epoch + 1))
export BT_RECOVERY_EPOCH=$recovery_epoch
compose up -d app >/dev/null
app=$(compose ps -q app)
refresh_app_binding
for _ in $(seq 1 150); do
  curl --silent --fail --max-time 1 "$app_base/readyz" >/dev/null && break
  sleep 0.2
done
curl --silent --fail --max-time 2 "$app_base/readyz" >/dev/null
compose restart tls >/dev/null
sleep 1
issue epoch2-a "$family_a" "$owner_a"
go_phase epoch

# --- secret hygiene --------------------------------------------------------
compose logs --no-color app > "$logs/server-app.log" 2>&1
compose logs --no-color tls > "$logs/server-tls.log" 2>&1
compose logs --no-color db > "$logs/server-db.log" 2>&1
MCP_E2E_DIR="$clients" MCP_E2E_LOGS="$logs" "$venv/bin/python" Server/scripts/lib/mcp_e2e_client.py scan > "$logs/scan.out" 2>&1 || { cat "$logs/scan.out" >&2; exit 1; }
grep '^mcp-e2e proof: python-scan ' "$logs/scan.out"
echo "grant expiry: covered at integration level (no production clock override); see Server/docs/mcp.md"
echo "mcp-e2e proof: completed source_sha=$source_sha image=$image_id schema=$schema epochs=1->$recovery_epoch"
