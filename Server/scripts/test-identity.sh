#!/usr/bin/env bash
set -euo pipefail

test -n "${BABY_TRACKER_TEST_DATABASE_URL:-}"
export BABY_TRACKER_REQUIRE_DATABASE=1
cd "$(dirname "$0")/.."
if [[ $# -eq 0 ]]; then
  set -- ./...
fi
# PostgreSQL role memberships are cluster-global while fixtures create one
# exact application login pair per database. Serialize packages so an
# independent fixture can never look like a rogue incoming group member.
go test -p=1 -race -count=1 "$@"
