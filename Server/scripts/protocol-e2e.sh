#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../.."

if [[ ${BT_PROTOCOL_E2E_STUB_MODE:-0} == 1 ]]; then
  : "${BABY_TRACKER_TEST_DATABASE_URL:?stub mode requires a disposable test DSN marker}"
  export BABY_TRACKER_REQUIRE_DATABASE=1
  cd Server
  "${BT_PROTOCOL_E2E_GO:-go}" test -race -count=1 ./internal/realtime
  "${BT_PROTOCOL_E2E_GO:-go}" test -tags identitytest -race -count=1 ./tests/protocol/...
  exit
fi

# Background-push wakes and Live Activity push on, in both modes: the one
# APNs client against the identitytest recorder with a throwaway key
# (lib/disposable-sync.sh). main ran its Live Activity profile only in the
# ordinary gate; the port's runs in the refresh-recovery mode too.
export BT_DISPOSABLE_PUSH=1
export BT_DISPOSABLE_LIVE_ACTIVITY=1
source Server/scripts/lib/disposable-sync.sh
start_disposable_sync "${BT_RECOVERY_E2E_MODE:-hosted}" normal

(cd Server && go run -tags identitytest ./cmd/protocol-e2e-client --base "$base" --state "$state" --phase bootstrap)
owner_device=$(jq -r '.Owner.device_id' "$state")
caregiver_device=$(jq -r '.Caregiver.device_id' "$state")
[[ $owner_device =~ ^[0-9a-f-]{36}$ && $caregiver_device =~ ^[0-9a-f-]{36}$ ]]
# The write transaction schedules no durable wake: the legacy wake queue is
# gone with the legacy care storage (000030 section 8), in both modes.
[[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT to_regclass('public.delivery_pending') IS NULL") == t ]]

old_started=$(docker inspect --format '{{.State.StartedAt}}' "$app")
compose restart app >/dev/null
restart_binding=$(compose port app 8080)
[[ $restart_binding == 127.0.0.1:* ]]
base="http://$restart_binding"
for _ in $(seq 1 100); do
  if curl --silent --fail --max-time 1 "$base/readyz" >/dev/null; then break; fi
  sleep 0.1
done
curl --silent --fail --max-time 2 "$base/readyz" >/dev/null
new_started=$(docker inspect --format '{{.State.StartedAt}}' "$app")
[[ $new_started != "$old_started" ]]
(cd Server && go run -tags identitytest ./cmd/protocol-e2e-client --base "$base" --state "$state" --phase verify)
completed_feed=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT count(*)::text||':'||bool_and(
  kind=2 AND baby_id='11000000-0000-4000-8000-000000000101' AND entity_version=1 AND tombstoned_at IS NULL AND
  started_at='2026-09-20T15:00:00Z'::timestamptz AND ended_at='2026-09-20T15:10:00Z'::timestamptz AND session_id IS NULL AND
  breast_seconds=600 AND bottle_observations=0 AND w->'payload'->>'kind'='breast' AND w->'payload'->>'time_zone_id'='Europe/Zurich' AND
  w->'payload'->>'note' IS NULL AND w->'payload'->>'feeding_session_id' IS NULL
)::text||':'||(SELECT count(*)::text||':'||COALESCE(string_agg(concat_ws(',',c->>'id',c->>'kind',c->>'side',c->>'seconds',
  ((c->>'started_at')::timestamptz='2026-09-20T15:00:00Z'::timestamptz AND (c->>'ended_at')::timestamptz='2026-09-20T15:10:00Z'::timestamptz)::text,
  (c->>'volume_ml' IS NULL AND c->>'contents' IS NULL AND c->>'foods' IS NULL AND c->>'amount' IS NULL)::text),';'),'')
  FROM care_entities f, jsonb_array_elements(convert_from(f.wire,'UTF8')::jsonb->'payload'->'components') c WHERE f.id='21000000-0000-4000-8000-000000000201')
FROM care_entities, LATERAL (SELECT convert_from(wire,'UTF8')::jsonb AS w) entity WHERE id='21000000-0000-4000-8000-000000000201'")
[[ $completed_feed == 1:true:1:21100000-0000-4000-8000-000000000201,breast,left,600,true,true ]]
echo "black-box proof: actual app and client processes restarted; dropped timer response recovered with original receipt; refresh reuse and cross-account continuity denied; caregiver completed feed exact and once; sibling IDs and dependent records preserved; no wake queue"

# Device progress leaves the request path: the requests above record it in
# memory, and the wake worker (every 2 s, on the maintenance pool) flushes it
# to device_sync_cursors. Each phone device must have its row, with a cursor,
# within 10 s of the verify phase.
family_id=$(jq -r '.FamilyID' "$state")
[[ $family_id =~ ^[0-9a-f-]{36}$ ]]
wake_rows=""
for _ in $(seq 1 100); do
  wake_rows=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT count(*) FROM device_sync_cursors WHERE family_id='$family_id' AND device_id IN ('$owner_device','$caregiver_device') AND greatest(acked_sequence, delivered_sequence) > 0")
  [[ $wake_rows == 2 ]] && break
  sleep 0.1
done
[[ $wake_rows == 2 ]] || { echo "wake worker did not record both phone devices within 10 s ($wake_rows of 2)" >&2; exit 1; }
echo "wake proof: the wake worker flushed both phone devices' cursors to device_sync_cursors from memory, off the request path"

db_binding=$(compose port db 5432)
[[ $db_binding == 127.0.0.1:* ]]
db_port=${db_binding##*:}
export BABY_TRACKER_REQUIRE_DATABASE=1
export BABY_TRACKER_TEST_DATABASE_URL="postgres://postgres:$(<"$BT_SECRETS_DIR/bootstrap_password")@127.0.0.1:$db_port/babytracker?sslmode=disable"
(cd Server && go test -race -count=1 ./internal/realtime)
(cd Server && go test -race -count=1 ./internal/export -run TestGrowthOwnerArchiveRawAndTombstones)
(cd Server && go test -tags identitytest -race -count=1 ./tests/protocol/...)

# The iOS client, against this same container, over real TLS.
#
# Last, and deliberately so: every assertion above counts rows and cursors in
# this family, and the Swift suite creates its own with the Outsider account —
# which the bootstrap signs in and never puts in a family — so it can only
# disturb what has already been checked.
#
# It uses the TLS front rather than the app's own port because `ServerOrigin`
# refuses any scheme but https. That is the client's rule and the reason this
# step exists: until now nothing had ever run it against a server at all, and
# every family request body it sent was malformed.
compose up -d --wait --wait-timeout 60 tls
tls_binding=$(compose port tls 8443)
[[ $tls_binding == "127.0.0.1:$tls_port" ]]
curl --silent --fail --max-time 5 --cacert "$BT_SECRETS_DIR/tls.crt" "https://localhost:$tls_port/readyz" >/dev/null
swift_log="$BT_SECRETS_DIR/swift-e2e.log"
(cd Packages/BabyTracker && \
  BABY_TRACKER_E2E_BASE="https://localhost:$tls_port" \
  BABY_TRACKER_E2E_STATE="$state" \
  BABY_TRACKER_E2E_CERT="$BT_SECRETS_DIR/tls.crt" \
  BABY_TRACKER_E2E_INSTANCE="$BT_SERVER_INSTANCE_ID" \
  BABY_TRACKER_E2E_APNS=1 \
  swift test --filter FamilySyncClientE2ETests) 2>&1 | tee "$swift_log"
echo "client proof: the iOS client created a family, minted caregiver and view-only invitations, revoked one, synced a record and read it back over TLS at https://localhost:$tls_port"
# The Swift suites return early, and pass, when they cannot see the server —
# so a passing run proves nothing unless the two-caregiver test actually
# reached its last step. Its per-step lines are the proof; the last one must
# be there.
grep -q '^two-caregiver proof: step 24:' "$swift_log" || { echo "two-caregiver test did not run to completion" >&2; exit 1; }
echo "two-caregiver proof: two phones built from the app's composition signed in with Apple through the identity adapter and shared one family through this server over TLS — import, snapshot, edits, deletes, timers both ways, a running-timer edit, relaunch, a live hint, History's session edits ($(grep -c '^two-caregiver proof:' "$swift_log") proof lines above)"

# The owner's install day. The phone's own counts and session-link digests
# are not taken on trust: they are recomputed here from Postgres, for the
# family the test printed, and must be identical.
grep -q '^owner-reinstall proof: step 6:' "$swift_log" || { echo "owner-reinstall test did not run to completion" >&2; exit 1; }
reinstall_ledger=$(grep '^owner-reinstall ledger: ' "$swift_log" | tail -n 1)
reinstall_family=$(sed -E 's/^owner-reinstall ledger: family=([0-9a-f-]{36}) .*$/\1/' <<< "$reinstall_ledger")
[[ $reinstall_family =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]]
phone_ledger=${reinstall_ledger#owner-reinstall ledger: family=$reinstall_family }
ledger_sql=$(cat <<'SQL'
SELECT 'babies='||(SELECT count(*) FROM care_entities WHERE family_id='FAMILY' AND kind=1 AND tombstoned_at IS NULL)
  ||' growth_profiles='||(SELECT count(*) FROM care_entities WHERE family_id='FAMILY' AND kind=7 AND tombstoned_at IS NULL)
  ||' measurements='||(SELECT count(*) FROM care_entities WHERE family_id='FAMILY' AND kind=8 AND tombstoned_at IS NULL)
  ||' feeds='||(SELECT count(*) FROM care_entities WHERE family_id='FAMILY' AND kind=2 AND tombstoned_at IS NULL)
  ||' sleeps='||(SELECT count(*) FROM care_entities WHERE family_id='FAMILY' AND kind=3 AND tombstoned_at IS NULL)
  ||' diapers='||(SELECT count(*) FROM care_entities WHERE family_id='FAMILY' AND kind=4 AND tombstoned_at IS NULL)
  ||' supplements='||(SELECT count(*) FROM care_entities WHERE family_id='FAMILY' AND kind=6 AND tombstoned_at IS NULL)
  ||' timers='||(SELECT count(*) FROM care_entities WHERE family_id='FAMILY' AND kind=5 AND tombstoned_at IS NULL)
  ||' feed_links='||(SELECT md5(COALESCE(string_agg(id::text||'>'||COALESCE(session_id::text,'-'),',' ORDER BY id::text COLLATE "C"),''))
      FROM care_entities WHERE family_id='FAMILY' AND kind=2 AND tombstoned_at IS NULL)
  ||' sleep_links='||(SELECT md5(COALESCE(string_agg(id::text||'>'||COALESCE(session_id::text,'-'),',' ORDER BY id::text COLLATE "C"),''))
      FROM care_entities WHERE family_id='FAMILY' AND kind=3 AND tombstoned_at IS NULL)
SQL
)
server_ledger=$(docker exec "$db" psql -XAt -v ON_ERROR_STOP=1 -U postgres -d babytracker -c "${ledger_sql//FAMILY/$reinstall_family}")
if [[ $phone_ledger != "$server_ledger" ]]; then
  printf 'owner-reinstall ledger mismatch for family %s\n  phone:  %s\n  server: %s\n' "$reinstall_family" "$phone_ledger" "$server_ledger" >&2
  exit 1
fi
echo "owner-reinstall proof: the owner's store moved aside (same device: empty import, latest-state pull from cursor 0) and a fresh install (new device: snapshot) each ended with the server's records exactly; Postgres agrees with the reinstalled phone: $server_ledger"

# Snapshot pages serve a running timer's stored wire verbatim (a running
# bottle's start exactly as the phone sent it). The client must apply such a
# snapshot: a caregiver joins while timers run, shows them, and stops one.
grep -q '^running-timer snapshot proof: ' "$swift_log" || { echo "running-timer snapshot test did not run to completion" >&2; exit 1; }

# A backgrounded caregiver is woken by a push and pulls: its phone uploaded a
# synthetic sandbox token through PUT /v1/devices/push-token and held no live
# connection; the owner wrote; the wake worker pushed it; the push made it
# pull the owner's write. The test's last line is required, and the server's
# recorder (the APNs client's whole transport in this image: it never dials)
# must agree on its own: exactly one push to that test's token (named on its
# proof line; another test's token is not counted), through the sandbox host,
# background type, priority 5, the bundle's topic, a provider token signed by
# the mounted key, a content-available-only payload. The token never reaches
# the server log.
grep -q '^background-push proof: ' "$swift_log" || { echo "background push test did not run to completion" >&2; exit 1; }
push_token=$(grep '^background-push proof: ' "$swift_log" | tail -n 1 | sed -nE 's/^background-push proof: token=([0-9a-f]{64}) .*$/\1/p')
[[ $push_token =~ ^[0-9a-f]{64}$ ]] || { echo "the background push proof line names no token" >&2; exit 1; }
push_records=$(curl --silent --fail --max-time 5 "$base/__identitytest/apns" | jq -c --arg token "$push_token" '[.requests[] | select(.token == $token) | {host, path, push_type: .headers["apns-push-type"], priority: .headers["apns-priority"], topic: .headers["apns-topic"], provider_token_valid, acknowledged, payload}]')
expected_push='[{"host":"sandbox.apns.identitytest.invalid","path":"/3/device/808182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9f","push_type":"background","priority":"5","topic":"com.example.babytracker.protocol","provider_token_valid":true,"acknowledged":true,"payload":{"aps":{"content-available":1}}}]'
[[ $push_records == "$expected_push" ]] || { printf 'the APNs recorder does not hold exactly the one expected push: %s\n' "$push_records" >&2; exit 1; }
push_app_log=$(compose logs --no-color app 2>&1)
[[ -n $push_app_log && $push_app_log != *"$push_token"* ]] || { echo "the server log is unreadable, or the synthetic APNs token appeared in it" >&2; exit 1; }
echo "background-push recorder proof: the server's APNs recorder holds exactly one push to the backgrounded caregiver's sandbox token, apns-push-type background, priority 5, topic com.example.babytracker.protocol, a valid provider token, payload {\"aps\":{\"content-available\":1}} — and the token is absent from the server log"

# The app's Live Activity cards through the port (owner decision D1; main's
# lines 143-169, re-pointed at 000032's tables): three phones built from the
# app's graph registered with the coordinator over TLS; one ran care, one never
# synced, one treated this server as self-hosted; the identitytest recorder
# stood where Apple stands and the Swift test decoded every Live Activity
# payload it took. The phones' own ledgers are recomputed here from Postgres,
# the worker must have had a push accepted for that family, and the server's
# log must not carry a token.
grep -q '^live-activity proof: step 13:' "$swift_log" || { echo "live activity push test did not run to completion" >&2; exit 1; }
la_ledger=$(grep '^live-activity ledger: ' "$swift_log" | tail -n 1)
la_family=$(sed -E 's/^live-activity ledger: family=([0-9a-f-]{36}) .*$/\1/' <<< "$la_ledger")
[[ $la_family =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]] || { echo "the live-activity ledger line names no family" >&2; exit 1; }
la_phone=${la_ledger#live-activity ledger: family=$la_family }
# registrations: devices the server holds and has not revoked; sandbox: their
# APNs environment; cards: card update tokens it accepted; delivered: sends
# APNs (here the recorder) accepted for this family's jobs.
la_server=$(docker exec "$db" psql -XAt -v ON_ERROR_STOP=1 -U postgres -d babytracker -c "SELECT
  'registrations='||(SELECT count(*) FROM live_activity_devices WHERE family_id='$la_family' AND NOT revoked)
  ||' sandbox='||(SELECT count(*) FROM live_activity_devices WHERE family_id='$la_family' AND environment='sandbox')
  ||' cards='||(SELECT count(*) FROM live_activity_tokens WHERE family_id='$la_family')
  ||' delivered='||(SELECT count(*) FROM live_activity_history h JOIN live_activity_delivery d USING (job_id) WHERE d.family_id='$la_family' AND h.result='accepted')")
if [[ $la_phone != "${la_server%% delivered=*}" ]]; then
  printf 'live activity ledger mismatch for family %s\n  phone:  %s\n  server: %s\n' "$la_family" "$la_phone" "$la_server" >&2
  exit 1
fi
la_delivered=${la_server##* delivered=}
[[ $la_delivered =~ ^[0-9]+$ && $la_delivered -ge 1 ]] || { echo "no Live Activity push was accepted for family $la_family ($la_server)" >&2; exit 1; }
la_tokens=$(grep '^live-activity tokens: ' "$swift_log" | tail -n 1 | sed 's/^live-activity tokens: //')
[[ -n $la_tokens ]] || { echo "the live activity test named no tokens" >&2; exit 1; }
la_app_log="$BT_SECRETS_DIR/app.log"
compose logs --no-color app > "$la_app_log" 2>&1
[[ -s $la_app_log ]] || { echo "the server log is unreadable" >&2; exit 1; }
for token in $la_tokens; do
  [[ $token =~ ^[0-9a-f]{32,}$ ]] || { echo "the live activity test named a malformed token" >&2; exit 1; }
  if grep -qF "$token" "$la_app_log"; then
    echo "a synthetic Live Activity APNs token appeared in the server log" >&2
    exit 1
  fi
done
echo "live-activity proof: $(grep -c '^live-activity proof:' "$swift_log") phone proof lines; Postgres agrees: $la_server; no APNs token in $(wc -l < "$la_app_log" | tr -d ' ') server log lines"

if [[ -n ${BT_RECOVERY_E2E_MODE:-} ]]; then
  source Server/scripts/refresh-recovery-phases.sh
fi
