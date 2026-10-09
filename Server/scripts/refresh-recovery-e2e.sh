#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
[[ -z ${BABY_TRACKER_TEST_DATABASE_URL:-} ]] || { echo "recovery runner refuses external database" >&2; exit 2; }
[[ -z ${BT_PROTOCOL_E2E_STUB_MODE:-} ]] || { echo "recovery runner refuses stub mode" >&2; exit 2; }
for mode in hosted self_hosted; do
  echo "refresh-recovery mode: $mode"
  BT_RECOVERY_E2E_MODE="$mode" bash Server/scripts/protocol-e2e.sh
  echo "refresh-recovery mode completed: $mode"
done
