#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
python3 Server/scripts/lib/capacity_control_test.py
if [[ ${1:-} == --docker ]]; then
  shift
  python3 Server/scripts/lib/capacity_fixture_child_test.py "$@"
fi
