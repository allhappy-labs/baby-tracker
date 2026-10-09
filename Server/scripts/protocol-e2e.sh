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

source Server/scripts/lib/disposable-sync.sh
# The ordinary gate runs the Live Activity push profile; the refresh-recovery
# modes keep their own composition, push disabled, as they always have.
if [[ -z ${BT_RECOVERY_E2E_MODE:-} ]]; then
  export BT_DISPOSABLE_LIVE_ACTIVITY=1
fi
start_disposable_sync "${BT_RECOVERY_E2E_MODE:-hosted}" normal

(cd Server && go run -tags identitytest ./cmd/protocol-e2e-client --base "$base" --state "$state" --phase bootstrap)
owner_device=$(jq -r '.Owner.device_id' "$state")
caregiver_device=$(jq -r '.Caregiver.device_id' "$state")
[[ $owner_device =~ ^[0-9a-f-]{36}$ && $caregiver_device =~ ^[0-9a-f-]{36}$ ]]
queue_first=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT count(*)::text||':'||bool_and(device_id='$caregiver_device')::text||':'||bool_and(device_id<>'$owner_device')::text||':'||max(sequence)::text FROM delivery_pending")
if [[ $BT_MODE == hosted ]]; then
  [[ $queue_first == 1:true:true:2 ]]
else
  [[ -z $queue_first ]]
fi

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
  baby_id='11000000-0000-4000-8000-000000000101' AND entity_version=1 AND tombstoned_at IS NULL AND
  kind='breast' AND started_at='2026-09-20T15:00:00Z'::timestamptz AND ended_at='2026-09-20T15:10:00Z'::timestamptz AND
  time_zone_id='Europe/Zurich' AND note IS NULL AND component_version=4 AND feeding_session_id IS NULL
)::text||':'||(SELECT count(*)::text||':'||COALESCE(string_agg(concat_ws(',',component_id,kind,side,seconds,
  (started_at='2026-09-20T15:00:00Z'::timestamptz AND ended_at='2026-09-20T15:10:00Z'::timestamptz)::text,
  (volume_ml IS NULL AND contents IS NULL AND foods IS NULL AND amount IS NULL)::text),';'),'')
  FROM feed_components WHERE feed_id='21000000-0000-4000-8000-000000000201')
FROM feed_entries WHERE id='21000000-0000-4000-8000-000000000201'")
[[ $completed_feed == 1:true:1:21100000-0000-4000-8000-000000000201,breast,left,600,true,true ]]
queue_restart=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT count(*)::text||':'||bool_and(device_id='$caregiver_device')::text||':'||max(sequence)::text||':'||max(generation)::text FROM delivery_pending")
IFS=: read -r queue_count queue_target queue_sequence queue_generation <<< "$queue_restart"
if [[ $BT_MODE == hosted ]]; then
  [[ $queue_count == 1 && $queue_target == true && $queue_sequence -ge 5 && $queue_generation -ge 5 ]]
else
  [[ -z $queue_restart ]]
fi
echo "black-box proof: actual app and client processes restarted; dropped timer response recovered with original receipt; refresh reuse and cross-account continuity denied; caregiver completed feed exact and once; sibling IDs and dependent records preserved; queue=$queue_restart"

db_binding=$(compose port db 5432)
[[ $db_binding == 127.0.0.1:* ]]
db_port=${db_binding##*:}
export BABY_TRACKER_REQUIRE_DATABASE=1
export BABY_TRACKER_TEST_DATABASE_URL="postgres://postgres:$(<"$BT_SECRETS_DIR/bootstrap_password")@127.0.0.1:$db_port/babytracker?sslmode=disable"
(cd Server && go test -race -count=1 ./internal/realtime)
(cd Server && go test -race -count=1 ./internal/export -run TestGrowthOwnerArchiveRawAndTombstones)
(cd Server && go test -tags identitytest -race -count=1 ./tests/protocol/...)
# The harness's Apple: production APNs transport, in-process recorder.
(cd Server && go test -tags identitytest -race -count=1 -run 'IdentityTestAPNS' ./cmd/baby-tracker-server)

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
  BABY_TRACKER_E2E_APNS="${BT_DISPOSABLE_LIVE_ACTIVITY:-0}" \
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
SELECT 'babies='||(SELECT count(*) FROM babies WHERE family_id='FAMILY' AND tombstoned_at IS NULL)
  ||' growth_profiles='||(SELECT count(*) FROM growth_profiles WHERE family_id='FAMILY' AND tombstoned_at IS NULL)
  ||' measurements='||(SELECT count(*) FROM measurement_entries WHERE family_id='FAMILY' AND tombstoned_at IS NULL)
  ||' feeds='||(SELECT count(*) FROM feed_entries WHERE family_id='FAMILY' AND tombstoned_at IS NULL)
  ||' sleeps='||(SELECT count(*) FROM sleep_entries WHERE family_id='FAMILY' AND tombstoned_at IS NULL)
  ||' diapers='||(SELECT count(*) FROM diaper_entries WHERE family_id='FAMILY' AND tombstoned_at IS NULL)
  ||' supplements='||(SELECT count(*) FROM supplement_entries WHERE family_id='FAMILY' AND tombstoned_at IS NULL)
  ||' timers='||(SELECT count(*) FROM active_timers WHERE family_id='FAMILY' AND tombstoned_at IS NULL)
  ||' feed_links='||(SELECT md5(COALESCE(string_agg(id::text||'>'||COALESCE(feeding_session_id::text,'-'),',' ORDER BY id::text COLLATE "C"),''))
      FROM feed_entries WHERE family_id='FAMILY' AND tombstoned_at IS NULL)
  ||' sleep_links='||(SELECT md5(COALESCE(string_agg(id::text||'>'||COALESCE(sleep_session_id::text,'-'),',' ORDER BY id::text COLLATE "C"),''))
      FROM sleep_entries WHERE family_id='FAMILY' AND tombstoned_at IS NULL)
SQL
)
server_ledger=$(docker exec "$db" psql -XAt -v ON_ERROR_STOP=1 -U postgres -d babytracker -c "${ledger_sql//FAMILY/$reinstall_family}")
if [[ $phone_ledger != "$server_ledger" ]]; then
  printf 'owner-reinstall ledger mismatch for family %s\n  phone:  %s\n  server: %s\n' "$reinstall_family" "$phone_ledger" "$server_ledger" >&2
  exit 1
fi
echo "owner-reinstall proof: the owner's store moved aside (same device: empty import, journal from cursor 0) and a fresh install (new device: snapshot) each ended with the server's records exactly; Postgres agrees with the reinstalled phone: $server_ledger"

# Live Activity push through the real composition: two phones built from the
# app's graph registered with the coordinator over TLS; one ran care, the other
# never synced; the identitytest recorder stood where Apple stands and the
# Swift test decoded every payload it took. The phone's proof lines are checked
# here against Postgres, and the server's own logs must not carry a token.
if [[ ${BT_DISPOSABLE_LIVE_ACTIVITY:-0} == 1 ]]; then
  grep -q '^live-activity proof: step 13:' "$swift_log" || { echo "live activity push test did not run to completion" >&2; exit 1; }
  push_ledger=$(grep '^live-activity ledger: ' "$swift_log" | tail -n 1)
  push_family=$(sed -E 's/^live-activity ledger: family=([0-9a-f-]{36}) .*$/\1/' <<< "$push_ledger")
  [[ $push_family =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]]
  phone_push=${push_ledger#live-activity ledger: family=$push_family }
  server_push=$(docker exec "$db" psql -XAt -v ON_ERROR_STOP=1 -U postgres -d babytracker -c "SELECT
    'registrations='||(SELECT count(*) FROM live_activity_devices WHERE family_id='$push_family' AND NOT revoked)
    ||' sandbox='||(SELECT count(*) FROM live_activity_devices WHERE family_id='$push_family' AND environment='sandbox')
    ||' cards='||(SELECT count(*) FROM live_activity_tokens WHERE family_id='$push_family')
    ||' delivered='||(SELECT count(*) FROM live_activity_history h JOIN live_activity_delivery d USING(job_id) WHERE d.family_id='$push_family')")
  if [[ $phone_push != "${server_push%% delivered=*}" ]]; then
    printf 'live activity ledger mismatch for family %s\n  phone:  %s\n  server: %s\n' "$push_family" "$phone_push" "$server_push" >&2
    exit 1
  fi
  [[ ${server_push##* delivered=} -ge 1 ]]
  push_tokens=$(grep '^live-activity tokens: ' "$swift_log" | tail -n 1 | sed 's/^live-activity tokens: //')
  [[ -n $push_tokens ]]
  app_log="$BT_SECRETS_DIR/app.log"
  compose logs --no-color app > "$app_log" 2>&1
  for token in $push_tokens; do
    if grep -q "$token" "$app_log"; then
      echo "a synthetic APNs token appeared in the server log" >&2
      exit 1
    fi
  done
  echo "live-activity proof: $(grep -c '^live-activity proof:' "$swift_log") phone proof lines; Postgres agrees: $server_push; no APNs token in $(wc -l < "$app_log" | tr -d ' ') server log lines"
fi

if [[ -n ${BT_RECOVERY_E2E_MODE:-} ]]; then
  source Server/scripts/refresh-recovery-phases.sh
fi
