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
# Schema 29: receipts live in refresh_receipts, written and pruned inside the
# authority's own refresh/acknowledgement/revocation transactions. There is no
# cleanup function or worker left to admit; the maintenance credential only
# opts the capability in and must hold nothing on the session tables.
maintenance=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT to_regprocedure('app.cleanup_refresh_recovery_receipts(timestamptz)') IS NULL
  AND to_regclass('public.refresh_recovery_receipts') IS NULL
  AND (SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid='public.refresh_receipts'::regclass)
  AND has_table_privilege('baby_tracker_app','public.refresh_receipts','SELECT') AND has_table_privilege('baby_tracker_app','public.refresh_receipts','INSERT')
  AND has_table_privilege('baby_tracker_app','public.refresh_receipts','DELETE') AND NOT has_table_privilege('baby_tracker_app','public.refresh_receipts','UPDATE,TRUNCATE')
  AND NOT has_table_privilege('baby_tracker_export_app','public.refresh_receipts','SELECT,INSERT,UPDATE,DELETE,TRUNCATE')
  AND NOT has_table_privilege('baby_tracker_export_app','public.sessions','SELECT,INSERT,UPDATE,DELETE,TRUNCATE')")
[[ $maintenance == t ]] || { echo "receipt table separation failed: $maintenance" >&2; exit 1; }
echo "recovery readiness proof: mode=$BT_MODE capability=present receipts=refresh_receipts(runtime select/insert/delete only, forced RLS) maintenance=no_session_access cleanup=in_transaction"

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
# The consumed parent, the child and its receipt commit together: a fault
# before the commit leaves nothing, one after it (or a reply lost on the wire)
# leaves exactly the committed child and its receipt. A routine rotation
# appends no audit row (owner decision D1, 2026-10-16): the receipt and the
# session rows are its record. No boundary makes the node unready: there is
# no projection to fall behind.
receipt_proof_sql() {
  printf '%s' "SELECT count(*)::text||':'||count(DISTINCT r.child_id)::text||':'||COALESCE(bool_and(
      (SELECT count(*) FROM sessions c WHERE c.parent_id=r.parent_id)=1
      AND p.status='consumed' AND p.consumed_attempt_id=r.attempt_id AND p.child_id=r.child_id
      AND s.status='active' AND s.parent_id=r.parent_id AND s.lineage_root_id=p.lineage_root_id
      AND r.effective_deadline<=s.deadline_at AND r.refresh_expires_at=s.refresh_expires_at
      AND r.material_digest=sha256(r.material)
      AND NOT EXISTS (SELECT 1 FROM security_audit_events a WHERE a.scope='account' AND a.scope_id=r.account_id AND a.kind='session_rotated'
        AND a.payload->>'session_id'=r.child_id::text)),false)::text
    FROM refresh_receipts r JOIN sessions s ON s.id=r.child_id JOIN sessions p ON p.id=r.parent_id WHERE r.attempt_id='$1'"
}
for server_boundary in before_authority_commit after_authority_commit after_projection lost_response; do
  (cd Server && go run -tags identitytest ./cmd/protocol-e2e-client --base "$base" --state "$state" --phase recovery-bootstrap --recovery-fault "$server_boundary")
  go_attempt=$(jq -r '.Recovery.AttemptID' "$state")
  [[ $go_attempt =~ ^[0-9a-f-]{36}$ ]]
  before_count=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT count(*) FROM refresh_receipts WHERE attempt_id='$go_attempt'")
  expected_count=1
  [[ $server_boundary != before_authority_commit ]] || expected_count=0
  [[ $before_count == "$expected_count" ]] || { echo "boundary=$server_boundary receipts=$before_count want $expected_count" >&2; exit 1; }
  readiness_status=$(curl --silent --max-time 5 -o /dev/null -w '%{http_code}' "$base/readyz")
  [[ $readiness_status == 200 ]] || { echo "boundary=$server_boundary readiness=$readiness_status" >&2; exit 1; }
  recovery_restart
  # Lost-acknowledgement retry: the restarted server returns the committed
  # child, byte for byte, twice, without minting another.
  (cd Server && go run -tags identitytest ./cmd/protocol-e2e-client --base "$base" --state "$state" --phase recovery-verify)
  go_receipts=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "$(receipt_proof_sql "$go_attempt")")
  [[ $go_receipts == 1:1:true ]] || { echo "boundary=$server_boundary receipt proof=$go_receipts" >&2; exit 1; }
  lineage=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT p.lineage_root_id::text||' '||r.parent_id::text||' '||r.child_id::text||' '||r.account_id::text FROM refresh_receipts r JOIN sessions p ON p.id=r.parent_id WHERE r.attempt_id='$go_attempt'")
  read -r lineage_root parent_session child_session recovery_account <<< "$lineage"
  [[ $recovery_account =~ ^[0-9a-f-]{36}$ ]] || { echo "boundary=$server_boundary receipt lineage missing" >&2; exit 1; }
  # Acknowledgement retires the receipt; then the consumed parent replayed
  # under another attempt revokes the whole lineage, child included.
  (cd Server && go run -tags identitytest ./cmd/protocol-e2e-client --base "$base" --state "$state" --phase recovery-reuse)
  reuse=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT
      (SELECT count(*) FROM refresh_receipts WHERE attempt_id='$go_attempt' OR parent_id='$parent_session' OR child_id='$child_session')::text||':'||
      (SELECT count(*) FROM sessions WHERE lineage_root_id='$lineage_root' AND status='active')::text||':'||
      (SELECT count(*) FROM sessions WHERE parent_id='$parent_session')::text||':'||
      (SELECT status::text||'/'||COALESCE(revoke_reason::text,'-') FROM sessions WHERE id='$child_session')||':'||
      (SELECT status::text FROM sessions WHERE id='$parent_session')||':'||
      (SELECT count(*) FROM security_audit_events WHERE scope='account' AND scope_id='$recovery_account' AND kind='session_revoked'
        AND payload->>'lineage_root_session_id'='$lineage_root')::text")
  [[ $reuse == 0:0:1:revoked/refresh_reused:consumed:1 ]] || { echo "boundary=$server_boundary reuse proof=$reuse" >&2; exit 1; }
  echo "recovery SQL proof: boundary=$server_boundary before_receipts=$before_count before_readiness=$readiness_status after_receipts=$go_receipts after_ack_and_reuse=$reuse"
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
  receipt_count=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT count(*) FROM refresh_receipts WHERE attempt_id='$attempt'")
  expected_receipts=1
  [[ $boundary != after_ack ]] || expected_receipts=0
  [[ $receipt_count == "$expected_receipts" ]]
  if [[ $boundary != after_ack ]]; then
    commitment=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "$(receipt_proof_sql "$attempt")")
    [[ $commitment == 1:1:true ]] || { echo "boundary=$boundary receipt proof=$commitment" >&2; exit 1; }
  fi
  recovery_restart
  # Wait out the dead process's real persisted 30-second account lease. No
  # lease row is edited and no test clock weakens the normal current fence.
  sleep 31
  (cd Packages/BabyTracker && BABY_TRACKER_RECOVERY_PHASE=verify swift test --filter RefreshRecoveryE2ETests) 2>&1 | tee "$verify_log"
  grep -q "^refresh-recovery proof: completed boundary=$boundary " "$verify_log"
  remaining=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT count(*) FROM refresh_receipts WHERE attempt_id='$attempt'")
  [[ $remaining == 0 ]]
  echo "recovery SQL proof: boundary=$boundary child_receipts_before=$receipt_count receipts_after_ack=$remaining"
done
