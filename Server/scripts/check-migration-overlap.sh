#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: check-migration-overlap.sh --base-ref REF --prior-ref REF [--feature-ref REF] [--legacy-task2-bootstrap] --next-version N --prior-min N --prior-max N --next-min N --next-max N" >&2
  exit 2
}

base_ref=
prior_ref=
feature_ref=HEAD
legacy_task2_bootstrap=0
next_version=
prior_min=
prior_max=
next_min=
next_max=
while [[ $# -gt 0 ]]; do
  case "$1" in
    --base-ref) base_ref=${2:-}; shift 2 ;;
    --prior-ref) prior_ref=${2:-}; shift 2 ;;
    --feature-ref) feature_ref=${2:-}; shift 2 ;;
    --legacy-task2-bootstrap) legacy_task2_bootstrap=1; shift ;;
    --next-version) next_version=${2:-}; shift 2 ;;
    --prior-min) prior_min=${2:-}; shift 2 ;;
    --prior-max) prior_max=${2:-}; shift 2 ;;
    --next-min) next_min=${2:-}; shift 2 ;;
    --next-max) next_max=${2:-}; shift 2 ;;
    *) usage ;;
  esac
done
[[ -n "$base_ref" && -n "$prior_ref" && -n "$feature_ref" && "$next_version" =~ ^[0-9]+$ && "$prior_min" =~ ^[0-9]+$ && "$prior_max" =~ ^[0-9]+$ && "$next_min" =~ ^[0-9]+$ && "$next_max" =~ ^[0-9]+$ ]] || usage
if (( prior_min >= next_version || prior_max != next_version || next_min != next_version || next_max != next_version )); then
  echo "invalid migration overlap bounds" >&2
  exit 1
fi

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
server_dir=$(cd "$script_dir/.." && pwd -P)
repo_dir=$(cd "$server_dir/.." && pwd -P)
for named_ref in "base:$base_ref" "prior:$prior_ref" "feature:$feature_ref"; do
  name=${named_ref%%:*}; ref=${named_ref#*:}
  if ! git -C "$repo_dir" rev-parse --verify --quiet "${ref}^{commit}" >/dev/null; then
    echo "$name ref does not resolve to a commit" >&2
    exit 1
  fi
done
resolved_base=$(git -C "$repo_dir" rev-parse "${base_ref}^{commit}")
resolved_prior=$(git -C "$repo_dir" rev-parse "${prior_ref}^{commit}")
resolved_feature=$(git -C "$repo_dir" rev-parse "${feature_ref}^{commit}")
if [[ "$resolved_base" == "$resolved_prior" || "$resolved_prior" == "$resolved_feature" || "$resolved_base" == "$resolved_feature" ]]; then
  echo "overlap refs must resolve to three distinct commits" >&2
  exit 1
fi
if ! git -C "$repo_dir" merge-base --is-ancestor "$base_ref" "$prior_ref" || ! git -C "$repo_dir" merge-base --is-ancestor "$prior_ref" "$feature_ref"; then
  echo "overlap refs are not ordered base -> prior -> feature" >&2
  exit 1
fi
if [[ $legacy_task2_bootstrap -eq 1 && $next_version -ne 4 ]]; then
  echo "legacy Task 2 bootstrap is valid only for migration 4" >&2
  exit 1
fi

verify_manifests() {
  local previous=$1 next=$2
  (cd "$server_dir" && go run ./cmd/baby-tracker-manifest verify --previous "$previous" --next "$next")
}

verify_prepared_history() {
  local previous=$1 next=$2
  (cd "$server_dir" && go run ./cmd/baby-tracker-manifest history --previous "$previous" --next "$next")
}

verify_bounds() {
  local manifest=$1 minimum=$2 maximum=$3
  (cd "$server_dir" && go run ./cmd/baby-tracker-manifest bounds --manifest "$manifest" --min "$minimum" --max "$maximum")
}

record_test_binaries() {
  local prior_binary=$1 next_binary=$2 sink=$3
  if [[ "$prior_binary" == "$next_binary" ]]; then
    echo "overlap gate requires distinct binary paths" >&2
    exit 1
  fi
  : > "$sink"
  local prior_id next_id
  prior_id=$($prior_binary build-id)
  next_id=$($next_binary build-id)
  if [[ -z "$prior_id" || -z "$next_id" || "$prior_id" == "$next_id" ]]; then
    echo "overlap gate requires distinct binary build IDs" >&2
    exit 1
  fi
}

overlap_scenarios=(prior_at_n_minus_1 prior_at_n current_at_n)

record_test_plan() {
  local legacy=$1 sink=$2
  if [[ $legacy == 0 ]]; then
    printf 'init prior_at_n_minus_1\n' >> "$sink"
  else
    printf 'init current_at_n\n' >> "$sink"
  fi
  printf 'migrate prior_at_n\nreconcile current_at_n\ninit_count 1\n' >> "$sink"
}

if [[ ${BT_OVERLAP_TEST_MODE:-0} == 1 ]]; then
  : "${BT_OVERLAP_PRIOR_BINARY:?missing test prior binary}"
  : "${BT_OVERLAP_NEXT_BINARY:?missing test next binary}"
  : "${BT_OVERLAP_PRIOR_CHECK_BINARY:?missing test prior overlap checker}"
  : "${BT_OVERLAP_NEXT_CHECK_BINARY:?missing test next overlap checker}"
  : "${BT_OVERLAP_HTTP_PROBE_BINARY:?missing test family HTTP probe}"
  : "${BT_OVERLAP_PRIOR_ADMIN_BINARY:?missing test prior admin binary}"
  : "${BT_OVERLAP_INVOCATION_SINK:?missing test invocation sink}"
  : "${BT_OVERLAP_PRIOR_MANIFEST:?missing test prior manifest}"
  : "${BT_OVERLAP_NEXT_MANIFEST:?missing test next manifest}"
  if [[ $legacy_task2_bootstrap -eq 1 ]]; then
    verify_manifests "$BT_OVERLAP_PRIOR_MANIFEST" "$BT_OVERLAP_NEXT_MANIFEST"
  else
    verify_prepared_history "$BT_OVERLAP_PRIOR_MANIFEST" "$BT_OVERLAP_NEXT_MANIFEST"
  fi
  record_test_binaries "$BT_OVERLAP_PRIOR_BINARY" "$BT_OVERLAP_NEXT_BINARY" "$BT_OVERLAP_INVOCATION_SINK"
  if [[ "$BT_OVERLAP_PRIOR_CHECK_BINARY" == "$BT_OVERLAP_NEXT_CHECK_BINARY" ]]; then
    echo "overlap gate requires distinct checker paths" >&2
    exit 1
  fi
  prior_admin_id=$($BT_OVERLAP_PRIOR_ADMIN_BINARY build-id)
  if [[ -z "$prior_admin_id" ]]; then
    echo "overlap gate requires predecessor admin build identity" >&2
    exit 1
  fi
  "$BT_OVERLAP_PRIOR_ADMIN_BINARY" migrate
  if [[ $legacy_task2_bootstrap -eq 1 ]]; then
    verify_bounds "$BT_OVERLAP_PRIOR_MANIFEST" "$prior_min" "$((next_version - 1))"
    printf 'mode legacy-synthesized\n' >> "$BT_OVERLAP_INVOCATION_SINK"
  else
    verify_bounds "$BT_OVERLAP_PRIOR_MANIFEST" "$prior_min" "$prior_max"
    printf 'mode prepared-immutable\n' >> "$BT_OVERLAP_INVOCATION_SINK"
  fi
  verify_bounds "$BT_OVERLAP_NEXT_MANIFEST" "$next_min" "$next_max"
  "$BT_OVERLAP_PRIOR_CHECK_BINARY" --scenario prior_at_n_minus_1 --expected-version "$((next_version - 1))"
  if [[ $next_version -ge 9 ]]; then
    owner_seed_args=(--curl-config /tmp/redacted-overlap-curl-config)
    if [[ $next_version -ge 12 ]]; then
      owner_seed_args+=(--account-id-file /tmp/redacted-overlap-account-id)
    fi
    "$BT_OVERLAP_PRIOR_CHECK_BINARY" seed-session "${owner_seed_args[@]}"
    "$BT_OVERLAP_HTTP_PROBE_BINARY" prior_at_n_minus_1 create-read prior-check
  fi
  if [[ $next_version -ge 10 ]]; then
    "$BT_OVERLAP_PRIOR_CHECK_BINARY" seed-session --curl-config /tmp/redacted-overlap-caregiver-curl-config \
      --identity-subject overlap-family-http-caregiver --refresh-request /tmp/redacted-overlap-caregiver-refresh
    "$BT_OVERLAP_HTTP_PROBE_BINARY" prior_at_n_minus_1 invitation-create-read-caregiver-denial prior-check
  fi
  if [[ $next_version -ge 13 ]]; then
    "$BT_OVERLAP_HTTP_PROBE_BINARY" prior_at_n_minus_1 timer-start-stop prior-check
    "$BT_OVERLAP_HTTP_PROBE_BINARY" prior_at_n_minus_1 import-stage-resume-match-conflict-records prior-check
    "$BT_OVERLAP_PRIOR_CHECK_BINARY" assert-delivery-unused --account-id-file /tmp/redacted-overlap-account-id \
      --family-id-file /tmp/redacted-overlap-family-id --expected-version "$((next_version - 1))"
  elif [[ $next_version -ge 12 ]]; then
    "$BT_OVERLAP_HTTP_PROBE_BINARY" prior_at_n_minus_1 timer-start-stop prior-check
    "$BT_OVERLAP_PRIOR_CHECK_BINARY" assert-import-unused --account-id-file /tmp/redacted-overlap-account-id \
      --family-id-file /tmp/redacted-overlap-family-id --expected-version "$((next_version - 1))"
  fi
  if [[ $next_version -ge 13 ]]; then
    "$BT_OVERLAP_NEXT_CHECK_BINARY" assert-import-preserved \
      --account-id-file /tmp/redacted-overlap-account-id --family-id-file /tmp/redacted-overlap-family-id \
      --expected-version "$next_version" --phase prior_at_n_minus_1
    "$BT_OVERLAP_NEXT_CHECK_BINARY" --scenario prior_at_n --expected-version "$next_version"
  else
    "$BT_OVERLAP_PRIOR_CHECK_BINARY" --scenario prior_at_n --expected-version "$next_version"
  fi
  if [[ $next_version -ge 9 ]]; then
    "$BT_OVERLAP_HTTP_PROBE_BINARY" prior_at_n read prior-check
  fi
  if [[ $next_version -ge 10 ]]; then
    "$BT_OVERLAP_HTTP_PROBE_BINARY" prior_at_n invitation-create-read-caregiver-denial prior-check
  fi
  if [[ $next_version -ge 13 ]]; then
    "$BT_OVERLAP_HTTP_PROBE_BINARY" prior_at_n timer-start-stop prior-check
    "$BT_OVERLAP_HTTP_PROBE_BINARY" prior_at_n import-stage-resume-match-conflict-records prior-check
    "$BT_OVERLAP_PRIOR_CHECK_BINARY" assert-delivery-unused --account-id-file /tmp/redacted-overlap-account-id \
      --family-id-file /tmp/redacted-overlap-family-id --expected-version "$next_version"
  elif [[ $next_version -ge 12 ]]; then
    "$BT_OVERLAP_HTTP_PROBE_BINARY" prior_at_n timer-start-stop prior-check
    "$BT_OVERLAP_PRIOR_CHECK_BINARY" assert-import-unused --account-id-file /tmp/redacted-overlap-account-id \
      --family-id-file /tmp/redacted-overlap-family-id --expected-version "$next_version"
  fi
  "$BT_OVERLAP_NEXT_CHECK_BINARY" --scenario current_at_n --expected-version "$next_version"
  if [[ $next_version -ge 9 ]]; then
    "$BT_OVERLAP_HTTP_PROBE_BINARY" current_at_n read next-check
  fi
  if [[ $next_version -ge 10 ]]; then
    "$BT_OVERLAP_HTTP_PROBE_BINARY" current_at_n invitation-create-read-caregiver-denial next-check
  fi
  if [[ $next_version -ge 11 ]]; then
    "$BT_OVERLAP_HTTP_PROBE_BINARY" current_at_n timer-start-stop next-check
  fi
  if [[ $next_version -ge 12 ]]; then
    "$BT_OVERLAP_HTTP_PROBE_BINARY" current_at_n import-stage-resume-match-conflict-records next-check
  fi
  printf 'scenario prior_at_n_minus_1 version %d\nscenario prior_at_n version %d\nscenario current_at_n version %d\n' "$((next_version - 1))" "$next_version" "$next_version" >> "$BT_OVERLAP_INVOCATION_SINK"
  record_test_plan "$legacy_task2_bootstrap" "$BT_OVERLAP_INVOCATION_SINK"
  exit 0
fi

: "${BT_OVERLAP_MIGRATOR_DATABASE_URL:?BT_OVERLAP_MIGRATOR_DATABASE_URL is required}"
: "${BT_OVERLAP_RUNTIME_DATABASE_URL:?BT_OVERLAP_RUNTIME_DATABASE_URL is required}"
: "${BT_OVERLAP_PROJECTOR_DATABASE_URL:?BT_OVERLAP_PROJECTOR_DATABASE_URL is required}"
if [[ "$BT_OVERLAP_MIGRATOR_DATABASE_URL" == "$BT_OVERLAP_RUNTIME_DATABASE_URL" ||
      "$BT_OVERLAP_MIGRATOR_DATABASE_URL" == "$BT_OVERLAP_PROJECTOR_DATABASE_URL" ||
      "$BT_OVERLAP_RUNTIME_DATABASE_URL" == "$BT_OVERLAP_PROJECTOR_DATABASE_URL" ]]; then
  echo "overlap gate requires distinct migrator, runtime, and projector credentials" >&2
  exit 1
fi

workspace=$(mktemp -d "${TMPDIR:-/tmp}/baby-tracker-overlap.XXXXXX")
umask 077
prior_pid=
next_pid=
cleanup() {
  local status=$?
  if [[ -n "$prior_pid" ]]; then kill "$prior_pid" 2>/dev/null || true; wait "$prior_pid" 2>/dev/null || true; fi
  if [[ -n "$next_pid" ]]; then kill "$next_pid" 2>/dev/null || true; wait "$next_pid" 2>/dev/null || true; fi
  rm -rf "$workspace"
  exit "$status"
}
trap cleanup EXIT

base_sha=$resolved_base
prior_sha=$resolved_prior
next_sha=$resolved_feature
mkdir -p "$workspace/prior" "$workspace/prior-base" "$workspace/feature"
git -C "$repo_dir" archive "$prior_sha" | tar -x -C "$workspace/prior"
git -C "$repo_dir" archive "$base_sha" | tar -x -C "$workspace/prior-base"
git -C "$repo_dir" archive "$next_sha" | tar -x -C "$workspace/feature"
feature_server="$workspace/feature/Server"

if [[ $legacy_task2_bootstrap -eq 1 ]]; then
  verify_manifests "$workspace/prior/Server/internal/migrate/compatibility.json" "$feature_server/internal/migrate/compatibility.json"
else
  verify_prepared_history "$workspace/prior/Server/internal/migrate/compatibility.json" "$feature_server/internal/migrate/compatibility.json"
fi
verify_bounds "$feature_server/internal/migrate/compatibility.json" "$next_min" "$next_max"
if [[ $legacy_task2_bootstrap -eq 1 ]]; then
  verify_bounds "$workspace/prior/Server/internal/migrate/compatibility.json" "$prior_min" "$((next_version - 1))"
  next_migration=$(printf "%06d" "$next_version")
  cp "$feature_server/migrations/${next_migration}_"*.sql "$workspace/prior/Server/migrations/"
  (cd "$server_dir" && go run ./cmd/baby-tracker-manifest \
    --migrations "$workspace/prior/Server/migrations" \
    --previous "$workspace/prior/Server/internal/migrate/compatibility.json" \
    --output "$workspace/prior/Server/internal/migrate/compatibility.json" \
    --min "$prior_min" --max "$prior_max")
fi
verify_bounds "$workspace/prior/Server/internal/migrate/compatibility.json" "$prior_min" "$prior_max"

(cd "$workspace/prior/Server" && go build -o "$workspace/prior-server" ./cmd/baby-tracker-server)
(cd "$workspace/prior/Server" && go build -o "$workspace/prior-admin" ./cmd/baby-tracker-admin)
(cd "$workspace/prior-base/Server" && go build -o "$workspace/base-admin" ./cmd/baby-tracker-admin)
if [[ $next_version -ge 13 ]]; then
  (cd "$feature_server" && go build -tags identitytest -o "$workspace/next-server" ./cmd/baby-tracker-server)
  (cd "$feature_server" && go build -tags identitytest -o "$workspace/next-protocol-client" ./cmd/protocol-e2e-client)
else
  (cd "$feature_server" && go build -o "$workspace/next-server" ./cmd/baby-tracker-server)
fi
(cd "$feature_server" && go build -o "$workspace/next-admin" ./cmd/baby-tracker-admin)
(if [[ $legacy_task2_bootstrap -eq 1 ]]; then
   cd "$feature_server"
 else
   cd "$workspace/prior/Server"
 fi
 go build -o "$workspace/prior-overlap-check" ./cmd/baby-tracker-overlap-check)
(cd "$feature_server" && go build -o "$workspace/next-overlap-check" ./cmd/baby-tracker-overlap-check)

prior_id=$(go tool buildid "$workspace/prior-server")
prior_admin_id=$(go tool buildid "$workspace/prior-admin")
next_id=$(go tool buildid "$workspace/next-server")
if [[ -z "$prior_id" || -z "$prior_admin_id" || -z "$next_id" || "$prior_id" == "$next_id" ]]; then
  echo "overlap gate did not build distinct predecessor and next binaries" >&2
  exit 1
fi
printf 'prior-server %s %s\nprior-admin %s %s\nnext-server %s %s\n' "$prior_sha" "$prior_id" "$prior_sha" "$prior_admin_id" "$next_sha" "$next_id" > "$workspace/build-ids"

BT_DATABASE_URL="$BT_OVERLAP_MIGRATOR_DATABASE_URL" "$workspace/base-admin" migrate

instance_id=123e4567-e89b-42d3-a456-426614174000
ledger_dir="$workspace/security-ledger"
head_dir="$workspace/ledger-head"
mkdir -p "$ledger_dir" "$head_dir"
printf '%s' '0101010101010101010101010101010101010101010101010101010101010101' > "$workspace/identity-pepper"
printf '%s' '0202020202020202020202020202020202020202020202020202020202020202' > "$workspace/rate-limit-pepper"
printf '%s' '0303030303030303030303030303030303030303030303030303030303030303' > "$workspace/session-token-pepper"
printf '%s' '0404040404040404040404040404040404040404040404040404040404040404' > "$workspace/device-continuity-pepper"
printf '%s' '0505050505050505050505050505050505050505050505050505050505050505' > "$workspace/apple-notification-pepper"
printf '%s' '0606060606060606060606060606060606060606060606060606060606060606' > "$workspace/actor-id-pepper"
printf '%s' '0707070707070707070707070707070707070707070707070707070707070707' > "$workspace/invitation-pepper"
printf '%s' '0808080808080808080808080808080808080808080808080808080808080808' > "$workspace/page-token-key"
security_init_count=0
if [[ $legacy_task2_bootstrap -eq 0 ]]; then
  if [[ $("$workspace/base-admin" capabilities) != security-authority-v1 ]]; then
    echo "prepared base admin lacks security authority capability" >&2
    exit 1
  fi
  BT_DATABASE_URL="$BT_OVERLAP_MIGRATOR_DATABASE_URL" \
  BT_SECURITY_LEDGER_DIR="$ledger_dir" BT_LEDGER_HEAD_DIR="$head_dir" \
  BT_ACTOR_ID_PEPPER_FILE="$workspace/actor-id-pepper" \
  "$workspace/base-admin" security init --instance-id "$instance_id" --epoch 1 --confirm-instance "$instance_id"
  security_init_count=$((security_init_count + 1))
  BT_DATABASE_URL="$BT_OVERLAP_RUNTIME_DATABASE_URL" BT_PROJECTOR_DATABASE_URL="$BT_OVERLAP_PROJECTOR_DATABASE_URL" \
  BT_SECURITY_LEDGER_DIR="$ledger_dir" BT_LEDGER_HEAD_DIR="$head_dir" \
  BT_ACTOR_ID_PEPPER_FILE="$workspace/actor-id-pepper" \
  "$workspace/base-admin" security reconcile
fi

overlap_runtime_environment=(
  "BT_MODE=development"
  "BT_DATABASE_URL=$BT_OVERLAP_RUNTIME_DATABASE_URL"
  "BT_PROJECTOR_DATABASE_URL=$BT_OVERLAP_PROJECTOR_DATABASE_URL"
  "BT_PUBLIC_ORIGIN=https://overlap.family.example"
  "BT_SERVER_INSTANCE_ID=$instance_id"
  "BT_RECOVERY_EPOCH=1"
  "BT_SECURITY_LEDGER_DIR=$ledger_dir"
  "BT_LEDGER_HEAD_DIR=$head_dir"
  "BT_APPLE_AUDIENCE=overlap-test"
  "BT_SESSION_TOKEN_KEY_ID=session.v1"
  "BT_SESSION_TOKEN_PEPPER_FILE=$workspace/session-token-pepper"
  "BT_DEVICE_CONTINUITY_KEY_ID=device.v1"
  "BT_DEVICE_CONTINUITY_PEPPER_FILE=$workspace/device-continuity-pepper"
  "BT_APPLE_NOTIFICATION_KEY_ID=notification.v1"
  "BT_APPLE_NOTIFICATION_PEPPER_FILE=$workspace/apple-notification-pepper"
  "BT_INVITATION_KEY_ID=invitation.v1"
  "BT_INVITATION_PEPPER_FILE=$workspace/invitation-pepper"
  "BT_PAGE_TOKEN_KEY_ID=page.v1"
  "BT_PAGE_TOKEN_KEY_FILE=$workspace/page-token-key"
  "BT_ACTOR_ID_PEPPER_FILE=$workspace/actor-id-pepper"
  "BT_TRUSTED_PROXY_CIDRS=127.0.0.1/32"
  "BT_IDENTITY_KEY_ID=identity.v1"
  "BT_IDENTITY_PEPPER_FILE=$workspace/identity-pepper"
  "BT_RATE_LIMIT_PEPPER_FILE=$workspace/rate-limit-pepper"
  "BT_SYNC_PAGE_SIZE=1"
)

family_curl_config="$workspace/family-auth.curl"
family_refresh_request="$workspace/family-refresh.json"
caregiver_account_curl_config="$workspace/caregiver-account-auth.curl"
caregiver_family_curl_config="$workspace/caregiver-family-auth.curl"
caregiver_refresh_request="$workspace/caregiver-refresh.json"
family_create_request="$workspace/family-create.json"
family_id_file="$workspace/family-id"
family_account_id_file="$workspace/family-account-id"
if [[ $next_version -ge 9 ]]; then
  (umask 077; printf '%s' '{"protocol_version":"v1","capabilities":["identity-v1","family-lifecycle-v1"],"command_id":"93000000-0000-4000-8000-000000000020","display_name":"Overlap family"}' > "$family_create_request")
  owner_seed_args=(--curl-config "$family_curl_config" --refresh-request "$family_refresh_request")
  if [[ $next_version -ge 12 ]]; then
    owner_seed_args+=(--account-id-file "$family_account_id_file")
  fi
  env "${overlap_runtime_environment[@]}" "$workspace/prior-overlap-check" seed-session "${owner_seed_args[@]}"
fi
if [[ $next_version -ge 10 ]]; then
  env "${overlap_runtime_environment[@]}" "$workspace/prior-overlap-check" seed-session \
    --curl-config "$caregiver_account_curl_config" --identity-subject overlap-family-http-caregiver \
    --refresh-request "$caregiver_refresh_request"
  [[ -f "$caregiver_account_curl_config" && -f "$caregiver_refresh_request" ]]
  [[ $(stat -f '%Lp' "$caregiver_account_curl_config" 2>/dev/null || stat -c '%a' "$caregiver_account_curl_config") == 600 ]]
  [[ $(stat -f '%Lp' "$caregiver_refresh_request" 2>/dev/null || stat -c '%a' "$caregiver_refresh_request") == 600 ]]
fi
if [[ $next_version -ge 9 ]]; then
  [[ -f "$family_curl_config" && $(stat -f '%Lp' "$family_curl_config" 2>/dev/null || stat -c '%a' "$family_curl_config") == 600 ]]
fi

run_database_scenario() {
  local checker=$1 label=$2 expected_version=$3
  "$checker" --scenario "$label" --expected-version "$expected_version"
}

probe_family_http() {
  local checker=$1 phase=$2 port=$3 behavior=$4
  local response="$workspace/${phase}-family.json"
  local headers="$workspace/${phase}-family.headers"
  if [[ "$behavior" == create-read ]]; then
    curl --config "$family_curl_config" --connect-timeout 2 --max-time 10 --request POST --header 'Content-Type: application/json' \
      --dump-header "$workspace/${phase}-family-create.headers" \
      --data-binary "@$family_create_request" --output "$workspace/${phase}-family-create.json" \
      "http://127.0.0.1:${port}/v1/family"
    "$checker" verify-family-response --response "$workspace/${phase}-family-create.json" \
      --headers "$workspace/${phase}-family-create.headers" --expected-status 201 \
      --family-id-file "$family_id_file" --capture-family-id
    local refresh_response="$workspace/${phase}-owner-refresh-response.json"
    local refreshed_curl_config="$workspace/${phase}-owner-family-auth.curl"
    curl --silent --show-error --fail --connect-timeout 2 --max-time 10 --request POST --header 'Content-Type: application/json' \
      --data-binary "@$family_refresh_request" --output "$refresh_response" "http://127.0.0.1:${port}/v1/auth/refresh"
    chmod 600 "$refresh_response"
    "$checker" write-session-curl-config --response "$refresh_response" --curl-config "$refreshed_curl_config" \
      --family-id-file "$family_id_file"
    family_curl_config=$refreshed_curl_config
  elif [[ "$behavior" != read ]]; then
    echo "invalid family HTTP probe behavior" >&2
    return 1
  fi
  curl --config "$family_curl_config" --connect-timeout 2 --max-time 10 --dump-header "$headers" --output "$response" \
    "http://127.0.0.1:${port}/v1/family"
  "$checker" verify-family-response --response "$response" --headers "$headers" --expected-status 200 \
    --family-id-file "$family_id_file"
}

probe_invitation_http() {
  local checker=$1 phase=$2 port=$3
  local suffix
  case "$phase" in
    prior_at_n_minus_1) suffix=21 ;;
    prior_at_n) suffix=22 ;;
    current_at_n) suffix=24 ;;
    *) echo "invalid invitation overlap phase" >&2; return 1 ;;
  esac
  local request="$workspace/${phase}-invitation-create.json"
  local response="$workspace/${phase}-invitation-create-response.json"
  local headers="$workspace/${phase}-invitation-create.headers"
  local invitation_id="$workspace/${phase}-invitation-id"
  (umask 077; printf '{"protocol_version":"v1","capabilities":["identity-v1","family-lifecycle-v1"],"command_id":"93000000-0000-4000-8000-%012d"}' "$suffix" > "$request")
  curl --config "$family_curl_config" --connect-timeout 2 --max-time 10 --request POST --header 'Content-Type: application/json' \
    --dump-header "$headers" --data-binary "@$request" --output "$response" \
    "http://127.0.0.1:${port}/v1/family/invitations"
  "$checker" verify-invitation-response --response "$response" --headers "$headers" \
    --family-id-file "$family_id_file" --invitation-id-file "$invitation_id" --expected-status 201 --capture-invitation-id
  local read_response="$workspace/${phase}-invitation-read-response.json"
  local read_headers="$workspace/${phase}-invitation-read.headers"
  local invitation_value
  invitation_value=$(tr -d '\r\n' < "$invitation_id")
  [[ "$invitation_value" =~ ^[0-9a-f-]{36}$ ]]
  curl --config "$family_curl_config" --connect-timeout 2 --max-time 10 --dump-header "$read_headers" \
    --output "$read_response" "http://127.0.0.1:${port}/v1/family/invitations/${invitation_value}"
  "$checker" verify-invitation-response --response "$read_response" --headers "$read_headers" \
    --family-id-file "$family_id_file" --invitation-id-file "$invitation_id" --expected-status 200

  if [[ ! -f "$caregiver_family_curl_config" ]]; then
    local redeem_request="$workspace/caregiver-redeem.json"
    local redeem_response="$workspace/caregiver-redeem-response.json"
    jq -c --arg command '93000000-0000-4000-8000-000000000024' \
      '{protocol_version:"v1",capabilities:["identity-v1","family-lifecycle-v1"],command_id:$command,confirmed:true,token:.token}' \
      "$response" > "$redeem_request"
    chmod 600 "$redeem_request"
    curl --config "$caregiver_account_curl_config" --connect-timeout 2 --max-time 10 --request POST \
      --header 'Content-Type: application/json' --data-binary "@$redeem_request" --output "$redeem_response" \
      "http://127.0.0.1:${port}/v1/family/invitations/redeem"
    local refresh_response="$workspace/caregiver-refresh-response.json"
    curl --silent --show-error --fail --connect-timeout 2 --max-time 10 --request POST --header 'Content-Type: application/json' \
      --data-binary "@$caregiver_refresh_request" --output "$refresh_response" "http://127.0.0.1:${port}/v1/auth/refresh"
    chmod 600 "$refresh_response"
    "$checker" write-session-curl-config --response "$refresh_response" --curl-config "$caregiver_family_curl_config" \
      --family-id-file "$family_id_file"
  fi

  local denied_request="$workspace/${phase}-caregiver-invite-denied.json"
  local denied_response="$workspace/${phase}-caregiver-invite-denied-response.json"
  local denied_headers="$workspace/${phase}-caregiver-invite-denied.headers"
  (umask 077; printf '{"protocol_version":"v1","capabilities":["identity-v1","family-lifecycle-v1"],"command_id":"93000000-0000-4000-8000-%012d"}' "$((suffix + 100))" > "$denied_request")
  local status
  status=$(curl --config "$caregiver_family_curl_config" --no-fail --connect-timeout 2 --max-time 10 --request POST \
    --header 'Content-Type: application/json' --data-binary "@$denied_request" --dump-header "$denied_headers" --output "$denied_response" \
    --write-out '%{http_code}' "http://127.0.0.1:${port}/v1/family/invitations")
  [[ "$status" == 403 ]]
  "$checker" verify-error-response --response "$denied_response" --headers "$denied_headers" \
    --expected-status 403 --expected-code owner_required
}

probe_feature_sync_http() {
  local port=$1 phase=$2 suffix
  local feature_curl_config=$family_curl_config
  case "$phase" in
    prior_at_n_minus_1) suffix=4 ;;
    prior_at_n) suffix=3; feature_curl_config=$caregiver_family_curl_config ;;
    current_at_n) suffix=2 ;;
    *) echo "invalid sync overlap phase" >&2; return 1 ;;
  esac
  local first_baby second_baby create_mutation_a create_mutation_b update_mutation
  first_baby=$(printf '93000000-0000-4000-8000-%012d' "$((suffix * 100 + 1))")
  second_baby=$(printf '93000000-0000-4000-8000-%012d' "$((suffix * 100 + 2))")
  create_mutation_a=$(printf '93000000-0000-4000-8000-%012d' "$((suffix * 100 + 11))")
  create_mutation_b=$(printf '93000000-0000-4000-8000-%012d' "$((suffix * 100 + 12))")
  update_mutation=$(printf '93000000-0000-4000-8000-%012d' "$((suffix * 100 + 13))")
  local create_request="$workspace/${phase}-sync-create.json"
  local create_response="$workspace/${phase}-sync-create-response.json"
  local continuation_request="$workspace/${phase}-sync-continuation.json"
  local continuation_response="$workspace/${phase}-sync-continuation-response.json"
  local snapshot_response="$workspace/${phase}-snapshot-first-response.json"
  local update_request="$workspace/${phase}-sync-update.json"
  local update_response="$workspace/${phase}-sync-update-response.json"
  local snapshot_token="$workspace/${phase}-snapshot-token"
  local snapshot_continuation_config="$workspace/${phase}-snapshot-continuation.curl"
  local snapshot_continuation_response="$workspace/${phase}-snapshot-continuation-response.json"
  local overlap_cursor="$workspace/sync-overlap-cursor.json"
  local initial_cursor=null
  if [[ -f "$overlap_cursor" ]]; then initial_cursor=$(<"$overlap_cursor"); fi

  jq -nc --arg first "$first_baby" --arg second "$second_baby" \
    --arg mutation_a "$create_mutation_a" --arg mutation_b "$create_mutation_b" \
    --argjson cursor "$initial_cursor" \
    '{protocol_version:"v1",capabilities:["records-v1"],cursor:$cursor,mutations:[
      {mutation_id:$mutation_a,entity_id:$first,entity_kind:"baby",operation:"create",payload_version:1,
       observed_entity_version:0,client_mutation_time:"2026-09-20T12:00:00Z",import_id:null,
       payload:{name:"Overlap first",birth_date:"2026-01-01",sort_order:0}},
      {mutation_id:$mutation_b,entity_id:$second,entity_kind:"baby",operation:"create",payload_version:1,
       observed_entity_version:0,client_mutation_time:"2026-09-20T12:00:01Z",import_id:null,
       payload:{name:"Overlap second before",birth_date:"2026-01-02",sort_order:1}}
    ],page_token:null}' > "$create_request"
  chmod 600 "$create_request"
  curl --config "$feature_curl_config" --connect-timeout 2 --max-time 10 --request POST \
    --header 'Content-Type: application/json' --header 'X-Baby-Tracker-Protocol: v1' \
    --header 'X-Baby-Tracker-Capabilities: records-v1' --data-binary "@$create_request" \
    --output "$create_response" "http://127.0.0.1:${port}/v1/sync"
  jq -e --arg first "$first_baby" '
    (.outcomes | length == 2 and all(.[]; .status == "applied")) and
    (.changes | length == 1 and .[0].entity_id == $first) and
    (.next_page_token | type == "string" and length > 0)' "$create_response" >/dev/null
  echo "overlap phase=$phase sync=create-page"

  jq -c '{protocol_version:"v1",capabilities:["records-v1"],cursor:null,mutations:[],page_token:.next_page_token}' \
    "$create_response" > "$continuation_request"
  chmod 600 "$continuation_request"
  curl --config "$feature_curl_config" --connect-timeout 2 --max-time 10 --request POST \
    --header 'Content-Type: application/json' --header 'X-Baby-Tracker-Protocol: v1' \
    --header 'X-Baby-Tracker-Capabilities: records-v1' --data-binary "@$continuation_request" \
    --output "$continuation_response" "http://127.0.0.1:${port}/v1/sync"
  jq -e --arg second "$second_baby" '
    (.changes | length == 1 and .[0].entity_id == $second and .[0].snapshot_or_tombstone.payload.name == "Overlap second before") and
    .next_page_token == null' "$continuation_response" >/dev/null
  echo "overlap phase=$phase sync=continuation"

  curl --config "$feature_curl_config" --connect-timeout 2 --max-time 10 \
    --header 'X-Baby-Tracker-Protocol: v1' --header 'X-Baby-Tracker-Capabilities: records-v1,snapshot-v1' \
    --output "$snapshot_response" "http://127.0.0.1:${port}/v1/sync/snapshot"
  jq -e --arg first "$first_baby" '
    (.entities | length == 1 and .[0].meta.id == $first) and
    (.next_page_token | type == "string" and length > 0)' "$snapshot_response" >/dev/null
  echo "overlap phase=$phase sync=snapshot-first"
  jq -er '.next_page_token' "$snapshot_response" > "$snapshot_token"
  chmod 600 "$snapshot_token"

  jq -c --arg mutation "$update_mutation" --arg second "$second_baby" \
    '{protocol_version:"v1",capabilities:["records-v1"],cursor:.high_water,mutations:[{
      mutation_id:$mutation,entity_id:$second,
      entity_kind:"baby",operation:"update",payload_version:1,observed_entity_version:1,
      client_mutation_time:"2026-09-20T12:00:02Z",import_id:null,
      payload:{name:"Overlap second after",birth_date:"2026-01-02",sort_order:1}
    }],page_token:null}' "$snapshot_response" > "$update_request"
  chmod 600 "$update_request"
  curl --config "$feature_curl_config" --connect-timeout 2 --max-time 10 --request POST \
    --header 'Content-Type: application/json' --header 'X-Baby-Tracker-Protocol: v1' \
    --header 'X-Baby-Tracker-Capabilities: records-v1' --data-binary "@$update_request" \
    --output "$update_response" "http://127.0.0.1:${port}/v1/sync"
  jq -e --slurpfile snapshot "$snapshot_response" '
    (.outcomes | length == 1 and .[0].status == "applied" and .[0].canonical_entity.payload.name == "Overlap second after") and
    (.high_water.sequence > $snapshot[0].high_water.sequence)' "$update_response" >/dev/null
  echo "overlap phase=$phase sync=update"
  jq -c '.high_water' "$update_response" > "$overlap_cursor"
  chmod 600 "$overlap_cursor"

  (umask 077; printf 'url = "http://127.0.0.1:%s/v1/sync/snapshot?page_token=%s"\n' "$port" "$(tr -d '\r\n' < "$snapshot_token")" > "$snapshot_continuation_config")
  curl --config "$feature_curl_config" --config "$snapshot_continuation_config" --connect-timeout 2 --max-time 10 \
    --header 'X-Baby-Tracker-Protocol: v1' --header 'X-Baby-Tracker-Capabilities: records-v1,snapshot-v1' \
    --output "$snapshot_continuation_response"
  jq -e --arg second "$second_baby" --slurpfile first "$snapshot_response" '
    (.high_water == $first[0].high_water) and
    (.entities | length == 1 and .[0].meta.id == $second and .[0].meta.entity_version == 1 and .[0].payload.name == "Overlap second before")' \
    "$snapshot_continuation_response" >/dev/null
  echo "overlap phase=$phase sync=snapshot-continuation"
}

probe_timer_feature_sync_http() {
  local port=$1 phase=$2 suffix
  case "$phase" in
    prior_at_n_minus_1) suffix=4 ;;
    prior_at_n) suffix=3 ;;
    current_at_n) suffix=2 ;;
    *) echo "unknown timer overlap phase" >&2; return 1 ;;
  esac
  local baby_id="93000000-0000-4000-8000-000000000${suffix}01"
  local timer_id="93000000-0000-4000-8000-000000000${suffix}50"
  local completed_id="93000000-0000-4000-8000-000000000${suffix}53"
  local request="$workspace/${phase}-timer-start-stop.json"
  local response="$workspace/${phase}-timer-start-stop-response.json"
  local initial_cursor=null
  if [[ -f "$workspace/sync-overlap-cursor.json" ]]; then initial_cursor=$(<"$workspace/sync-overlap-cursor.json"); fi
  jq -nc --arg baby "$baby_id" --arg timer "$timer_id" --arg completed "$completed_id" \
    --arg start_mutation "93000000-0000-4000-8000-000000000${suffix}51" \
    --arg stop_mutation "93000000-0000-4000-8000-000000000${suffix}52" \
    --arg component "93000000-0000-4000-8000-000000000${suffix}54" \
    --argjson cursor "$initial_cursor" \
    '{protocol_version:"v1",capabilities:["records-v1","timer-commands-v1"],cursor:$cursor,mutations:[
      {mutation_id:$start_mutation,entity_id:$timer,entity_kind:"timer",operation:"timer_start",
       payload_version:5,observed_entity_version:0,client_mutation_time:"2026-09-20T15:00:00Z",import_id:null,
       payload:{baby_id:$baby,timer_kind:"feed",started_at:"2026-09-20T15:00:00Z",time_zone_id:"Europe/Zurich",
        completed_components:[],current_side:"left",current_side_started_at:"2026-09-20T15:00:00Z",current_bottle_started_at:null,session_id:null}},
      {mutation_id:$stop_mutation,entity_id:$timer,entity_kind:"timer",operation:"timer_stop",
       payload_version:5,observed_entity_version:1,client_mutation_time:"2026-09-20T15:10:00Z",import_id:null,
       payload:{baby_id:$baby,completed_record_id:$completed,stopped_at:"2026-09-20T15:10:00Z",component_id:$component,note:null,sleep_kind_override:null}}
    ],page_token:null}' > "$request"
  chmod 600 "$request"
  curl --config "$family_curl_config" --connect-timeout 2 --max-time 10 --request POST \
    --header 'Content-Type: application/json' --header 'X-Baby-Tracker-Protocol: v1' \
    --header 'X-Baby-Tracker-Capabilities: records-v1,timer-commands-v1' --data-binary "@$request" \
    --output "$response" "http://127.0.0.1:${port}/v1/sync"
  jq -e --arg timer "$timer_id" --arg completed "$completed_id" '
    (.outcomes | length == 2) and
    (.outcomes[0].status == "applied" and .outcomes[0].canonical_entity.meta.id == $timer and .outcomes[0].canonical_entity.payload.timer_kind == "feed") and
    (.outcomes[1].status == "applied" and .outcomes[1].canonical_entity.meta.id == $timer and .outcomes[1].canonical_entity.payload == null) and
    (.outcomes[1].related_entities | length == 1 and .[0].meta.id == $completed and .[0].entity_kind == "feed")' \
    "$response" >/dev/null
  # The next phase's record probe starts after every write in this phase,
  # including the timer tombstone and its related completed record.
  jq -c '.high_water' "$response" > "$workspace/sync-overlap-cursor.json"
  chmod 600 "$workspace/sync-overlap-cursor.json"
}

probe_import_feature_http() {
  local checker=$1 port=$2 phase=$3
  local cursor_file="$workspace/sync-overlap-cursor.json"
  local drain_cursor=
  if [[ $next_version -ge 13 && "$phase" == prior_at_n_minus_1 ]]; then
    drain_cursor="$workspace/predecessor-import-start-cursor.json"
    cp "$cursor_file" "$drain_cursor"
    chmod 600 "$drain_cursor"
    cursor_file="$workspace/predecessor-import-untrusted-high-water.json"
  fi
  local arguments=(probe-import-http --origin "http://127.0.0.1:${port}"
    --owner-curl-config "$family_curl_config" --caregiver-curl-config "$caregiver_family_curl_config")
  if [[ $next_version -ge 13 ]]; then
    arguments+=(--phase "$phase" --cursor-file "$cursor_file")
  fi
  if [[ $next_version -ge 13 && "$phase" != prior_at_n_minus_1 ]]; then
    checker="$workspace/next-overlap-check"
  fi
  "$checker" "${arguments[@]}"
  if [[ -n "$drain_cursor" ]]; then
    "$workspace/next-overlap-check" drain-sync-http --origin "http://127.0.0.1:${port}" \
      --owner-curl-config "$family_curl_config" --cursor-file "$drain_cursor"
    mv "$drain_cursor" "$workspace/sync-overlap-cursor.json"
    rm -f "$cursor_file"
  fi
}

run_gate() {
  local binary=$1 checker=$2 port=$3 pid_name=$4 phase=$5 family_behavior=$6 expected_version=$7
  local runtime_environment=("${overlap_runtime_environment[@]}")
  if [[ $next_version -ge 13 && "$pid_name:$phase" == next:current_at_n ]]; then
    runtime_environment+=("BT_MODE=hosted" "BT_IDENTITYTEST_HOSTED_ADAPTER=1")
  fi
  env "${runtime_environment[@]}" "BT_LISTEN_ADDRESS=127.0.0.1:${port}" \
    "$binary" >"$workspace/${phase}-${pid_name}.log" 2>&1 &
  local process=$!
  if [[ "$pid_name" == prior ]]; then prior_pid=$process; else next_pid=$process; fi
  local ready=0
  for _ in $(seq 1 100); do
    if curl --silent --fail "http://127.0.0.1:${port}/readyz" >/dev/null; then ready=1; break; fi
    sleep 0.05
  done
  if [[ $ready -ne 1 ]]; then
    echo "$pid_name binary did not become ready" >&2
    return 1
  fi
  curl --silent --fail "http://127.0.0.1:${port}/v1/server" | grep --quiet "$instance_id"

  # The immutable predecessor creates its compatibility family in self-hosted
  # mode, while the tagged candidate deliberately exercises hosted entitlement
  # semantics. Keep those rollout modes isolated: the feature family is created
  # through the candidate's identity/family HTTP flow, which projects its
  # entitlement through the authority before live and import are exercised.
  if [[ $next_version -ge 13 && "$pid_name" == next && "$phase" == current_at_n ]]; then
    echo "overlap phase=$phase stage=live-delivery"
    local protocol_state="$workspace/current-at-n-protocol-state.json"
    "$workspace/next-protocol-client" --base "http://127.0.0.1:${port}" --state "$protocol_state" --phase overlap
    env "${overlap_runtime_environment[@]}" "$checker" assert-delivery-feature \
      --protocol-state-file "$protocol_state"
    kill "$process"
    wait "$process" || true
    next_pid=
    return
  fi
  if [[ $next_version -ge 9 ]]; then echo "overlap phase=$phase stage=family"; probe_family_http "$checker" "$phase" "$port" "$family_behavior"; fi
  if [[ $next_version -ge 10 ]]; then echo "overlap phase=$phase stage=invitation"; probe_invitation_http "$checker" "$phase" "$port"; fi
  if [[ $next_version -ge 11 || ( $next_version -ge 10 && "$pid_name" == next && "$phase" == current_at_n ) ]]; then
    echo "overlap phase=$phase stage=sync"
    probe_feature_sync_http "$port" "$phase"
  fi
  if [[ $next_version -ge 12 || ( $next_version -eq 11 && "$pid_name" == next && "$phase" == current_at_n ) ]]; then
    echo "overlap phase=$phase stage=timer"
    probe_timer_feature_sync_http "$port" "$phase"
  fi
  if [[ $next_version -ge 13 && "$phase" != current_at_n ]]; then
    echo "overlap phase=$phase stage=import"
    probe_import_feature_http "$checker" "$port" "$phase"
    echo "overlap phase=$phase stage=delivery-unused"
    env "${overlap_runtime_environment[@]}" "$checker" assert-delivery-unused \
      --account-id-file "$family_account_id_file" --family-id-file "$family_id_file" \
      --expected-version "$expected_version"
  elif [[ $next_version -ge 12 && "$phase" == current_at_n ]]; then
    echo "overlap phase=$phase stage=import"
    probe_import_feature_http "$checker" "$port" "$phase"
  elif [[ $next_version -ge 12 ]]; then
    env "${overlap_runtime_environment[@]}" "$checker" assert-import-unused \
      --account-id-file "$family_account_id_file" --family-id-file "$family_id_file" \
      --expected-version "$expected_version"
  fi
  kill "$process"
  wait "$process" || true
  if [[ "$pid_name" == prior ]]; then prior_pid=; else next_pid=; fi
}

port_base=$((24000 + ($$ % 10000)))
run_database_scenario "$workspace/prior-overlap-check" prior_at_n_minus_1 "$((next_version - 1))"
run_gate "$workspace/prior-server" "$workspace/prior-overlap-check" "$port_base" prior prior_at_n_minus_1 create-read "$((next_version - 1))"

BT_DATABASE_URL="$BT_OVERLAP_MIGRATOR_DATABASE_URL" "$workspace/prior-admin" migrate
if [[ $legacy_task2_bootstrap -eq 1 ]]; then
  BT_DATABASE_URL="$BT_OVERLAP_MIGRATOR_DATABASE_URL" \
  BT_SECURITY_LEDGER_DIR="$ledger_dir" BT_LEDGER_HEAD_DIR="$head_dir" \
  BT_ACTOR_ID_PEPPER_FILE="$workspace/actor-id-pepper" \
  "$workspace/next-admin" security init --instance-id "$instance_id" --epoch 1 --confirm-instance "$instance_id"
  security_init_count=$((security_init_count + 1))
fi
BT_DATABASE_URL="$BT_OVERLAP_RUNTIME_DATABASE_URL" BT_PROJECTOR_DATABASE_URL="$BT_OVERLAP_PROJECTOR_DATABASE_URL" \
BT_SECURITY_LEDGER_DIR="$ledger_dir" BT_LEDGER_HEAD_DIR="$head_dir" \
BT_ACTOR_ID_PEPPER_FILE="$workspace/actor-id-pepper" \
"$workspace/next-admin" security reconcile

if [[ $next_version -ge 13 ]]; then
  env "${overlap_runtime_environment[@]}" "$workspace/next-overlap-check" assert-import-preserved \
    --account-id-file "$family_account_id_file" --family-id-file "$family_id_file" \
    --expected-version "$next_version" --phase prior_at_n_minus_1
  # The accepted predecessor checker predates the required schema-12 HTTP
  # import and globally requires all import tables to be empty. The candidate
  # checker scopes that legacy invariant to the new prior_at_n scenario while
  # the assertion above proves the earlier phase's import survived migration.
  run_database_scenario "$workspace/next-overlap-check" prior_at_n "$next_version"
else
  run_database_scenario "$workspace/prior-overlap-check" prior_at_n "$next_version"
fi
run_gate "$workspace/prior-server" "$workspace/prior-overlap-check" "$((port_base + 1))" prior prior_at_n read "$next_version"
run_database_scenario "$workspace/next-overlap-check" current_at_n "$next_version"
run_gate "$workspace/next-server" "$workspace/next-overlap-check" "$((port_base + 2))" next current_at_n read "$next_version"
[[ $security_init_count -eq 1 && -f "$head_dir/identity.json" ]]
echo "migration overlap passed sequence=N-1_to_N scenarios=${overlap_scenarios[*]} roles_separated=verified_by_current_readiness prior_build_id=$prior_id prior_admin_build_id=$prior_admin_id next_build_id=$next_id"
