#!/usr/bin/env bash
# Sourced only by the prepared, disposable protocol composition. All names,
# credentials, directories and containers belong to that caller's trap.
[[ -n ${BT_RECOVERY_E2E_MODE:-} && -d $BT_SECRETS_DIR ]]
info=$(curl --silent --fail --max-time 5 --cacert "$BT_SECRETS_DIR/tls.crt" "$BT_PUBLIC_ORIGIN/v1/server")
[[ $(jq -r '.capabilities | index("refresh-recovery-v1") != null' <<< "$info") == true ]]
expected_authorization=identity_token_only
[[ $BT_MODE != hosted ]] || expected_authorization=code_exchange_required
actual_authorization=$(jq -r '.apple_authorization_mode' <<< "$info")
[[ $actual_authorization == "$expected_authorization" ]] || { echo "unexpected mode: $actual_authorization expected $expected_authorization" >&2; exit 1; }
maintenance=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT has_function_privilege('baby_tracker_export_app','app.cleanup_refresh_recovery_receipts(timestamptz)','EXECUTE') AND NOT has_function_privilege('baby_tracker_app','app.cleanup_refresh_recovery_receipts(timestamptz)','EXECUTE') AND NOT has_function_privilege('baby_tracker_projector_app','app.cleanup_refresh_recovery_receipts(timestamptz)','EXECUTE')")
[[ $maintenance == t ]] || { echo "maintenance separation failed: $maintenance" >&2; exit 1; }
# Its initial iteration can precede readiness. Wait for the next real
# production-cadence iteration; never bypass its gate or invoke cleanup here.
worker_deadline=$((SECONDS + 330))
while true; do
  compose logs --no-color app > "$BT_SECRETS_DIR/recovery-server.log"
  if grep -q 'identitytest recovery cleanup completed' "$BT_SECRETS_DIR/recovery-server.log"; then break; fi
  [[ $SECONDS -lt $worker_deadline ]] || { echo "cleanup worker admission missing after bounded wait" >&2; exit 1; }
  sleep 1
done
echo "recovery readiness proof: mode=$BT_MODE capability=present maintenance=isolated cleanup_worker=admitted"

recovery_restart() {
  local before after
  before=$(docker inspect --format '{{.State.StartedAt}}' "$app")
  compose restart app >/dev/null
  restart_binding=$(compose port app 8080)
  [[ $restart_binding == 127.0.0.1:* ]]
  base="http://$restart_binding"
  for _ in $(seq 1 100); do
    if curl --silent --fail --max-time 1 "$base/readyz" >/dev/null; then break; fi
    sleep 0.1
  done
  curl --silent --fail --max-time 2 "$base/readyz" >/dev/null
  after=$(docker inspect --format '{{.State.StartedAt}}' "$app")
  [[ $before != "$after" ]]
  # nginx resolves the recreated app endpoint on its own restart.
  compose restart tls >/dev/null
}
for server_boundary in before_authority_commit after_authority_commit after_projection lost_response; do
  (cd Server && go run -tags identitytest ./cmd/protocol-e2e-client --base "$base" --state "$state" --phase recovery-bootstrap --recovery-fault "$server_boundary")
  go_attempt=$(jq -r '.Recovery.AttemptID' "$state")
  [[ $go_attempt =~ ^[0-9a-f-]{36}$ ]]
  before_count=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT count(*) FROM refresh_recovery_receipts WHERE attempt_id='$go_attempt'")
  expected_count=1
  [[ $server_boundary != before_authority_commit && $server_boundary != after_authority_commit ]] || expected_count=0
  [[ $before_count == "$expected_count" ]]
  readiness_status=$(curl --silent --max-time 5 -o /dev/null -w '%{http_code}' "$base/readyz")
  expected_status=200
  [[ $server_boundary != after_authority_commit ]] || expected_status=503
  [[ $readiness_status == "$expected_status" ]]
  recovery_restart
  (cd Server && go run -tags identitytest ./cmd/protocol-e2e-client --base "$base" --state "$state" --phase recovery-verify)
  go_receipts=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT count(*)::text||':'||count(DISTINCT r.child_session_id)::text||':'||bool_and((SELECT count(*) FROM sessions s WHERE s.parent_session_id=r.parent_session_id)=1 AND sha256(r.material_bytes)=r.projection_material_digest AND EXISTS (SELECT 1 FROM security_audit_events a, jsonb_array_elements(a.event_data->'effects') e WHERE e->>'session_id'=r.child_session_id::text AND e->>'projection_material_digest'=encode(r.projection_material_digest,'hex')))::text FROM refresh_recovery_receipts r WHERE attempt_id='$go_attempt'")
  [[ $go_receipts == 1:1:true ]]
  echo "recovery SQL proof: boundary=$server_boundary before_receipts=$before_count before_readiness=$readiness_status after_receipts=$go_receipts"
done

export BABY_TRACKER_E2E_BASE="$BT_PUBLIC_ORIGIN"
export BABY_TRACKER_E2E_STATE="$state"
export BABY_TRACKER_E2E_CERT="$BT_SECRETS_DIR/tls.crt"
export BABY_TRACKER_E2E_INSTANCE="$BT_SERVER_INSTANCE_ID"
for boundary in after_decode before_keychain after_keychain after_completion after_ack; do
  export BABY_TRACKER_RECOVERY_DIRECTORY="$BT_SECRETS_DIR/recovery-$boundary"
  export BABY_TRACKER_RECOVERY_BOUNDARY="$boundary"
  mkdir -m 0700 "$BABY_TRACKER_RECOVERY_DIRECTORY"
  bootstrap_log="$BT_SECRETS_DIR/recovery-$boundary-bootstrap.log"
  verify_log="$BT_SECRETS_DIR/recovery-$boundary-verify.log"
  set +e
  (cd Packages/BabyTracker && BABY_TRACKER_RECOVERY_PHASE=bootstrap swift test --filter RefreshRecoveryE2ETests) > "$bootstrap_log" 2>&1
  bootstrap_status=$?
  set -e
  # _exit deliberately kills the test child; a generic failed test never
  # counts as the requested crash boundary.
  [[ $bootstrap_status -ne 0 ]] || { cat "$bootstrap_log"; echo "missing process death" >&2; exit 1; }
  grep -q "^refresh-recovery crash: $boundary$" "$bootstrap_log" || { cat "$bootstrap_log"; exit 1; }
  cat "$bootstrap_log"
  attempt=$(jq -r '.attemptID | ascii_downcase' "$BABY_TRACKER_RECOVERY_DIRECTORY/decoded.json")
  [[ $attempt =~ ^[0-9a-f-]{36}$ ]]
  receipt_count=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT count(*) FROM refresh_recovery_receipts WHERE attempt_id='$attempt'")
  expected_receipts=1
  [[ $boundary != after_ack ]] || expected_receipts=0
  [[ $receipt_count == "$expected_receipts" ]]
  if [[ $boundary != after_ack ]]; then
    commitment=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT count(*)::text||':'||bool_and(sha256(r.material_bytes)=r.projection_material_digest AND EXISTS (SELECT 1 FROM security_audit_events a, jsonb_array_elements(a.event_data->'effects') e WHERE e->>'session_id'=r.child_session_id::text AND e->>'projection_material_digest'=encode(r.projection_material_digest,'hex')) AND (SELECT count(*) FROM sessions child WHERE child.parent_session_id=r.parent_session_id)=1 AND r.effective_deadline<=s.absolute_expires_at AND r.refresh_expires_at=s.refresh_expires_at AND s.revoked_at IS NULL)::text FROM refresh_recovery_receipts r JOIN sessions s ON s.session_id=r.child_session_id WHERE r.attempt_id='$attempt'")
    [[ $commitment == 1:true ]]
  fi
  recovery_restart
  # Wait out the dead process's real persisted 30-second account lease. No
  # lease row is edited and no test clock weakens the normal current fence.
  sleep 31
  (cd Packages/BabyTracker && BABY_TRACKER_RECOVERY_PHASE=verify swift test --filter RefreshRecoveryE2ETests) 2>&1 | tee "$verify_log"
  grep -q "^refresh-recovery proof: completed boundary=$boundary " "$verify_log"
  remaining=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT count(*) FROM refresh_recovery_receipts WHERE attempt_id='$attempt'")
  [[ $remaining == 0 ]]
  echo "recovery SQL proof: boundary=$boundary child_receipts_before=$receipt_count receipts_after_ack=$remaining"
done
