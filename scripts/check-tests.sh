#!/usr/bin/env bash
# Asserts the LITERAL package test total, not just exit 0.
#
# BabyStoreTests had an intermittent SIGSEGV — concurrent `ModelContainer`
# opens racing inside Core Data, fixed by the lock in `BabySchema.makeContainer`
# — that dropped its whole suite from a run which still exited 0 and printed no
# crash line. That suite holds the change-log, tombstone and timer invariants:
# the properties this design exists to guarantee. The crash is fixed, but the
# failure *shape* is what made it dangerous, and any future crash in any suite
# wears the same disguise. A silently short run must fail the build.
set -uo pipefail

EXPECTED="${1:?usage: check-tests.sh EXPECTED_TOTAL}"

cd "$(dirname "$0")/../Packages/BabyTracker" || {
  echo "FAIL: cannot cd to the package directory"
  exit 1
}

output=$(swift test 2>&1)
status=$?

# Anchored on `Test run with` alone, and deliberately not on the leading `✔`.
#
# Swift Testing prefixes each summary with a multi-byte `✔` or `✘`. Matching
# those — or even counting characters before them — makes the guard
# locale-dependent: under `LC_ALL=C` a `^.{0,2} ` anchor matched zero lines,
# which fails closed but cries wolf, and a guard that cries wolf gets disabled.
# The substring is ASCII and the same in every locale.
#
# Counts every run summary, passed or failed: a suite that fails loudly is a
# different problem from a suite that vanishes, and both must be visible.
actual=$(echo "$output" | grep -oE 'Test run with [0-9]+ tests' \
  | grep -oE '[0-9]+' \
  | awk '{s+=$1} END {print s+0}')

fail() {
  echo "FAIL: $1"
  # `^error:` and not plain `error`: the corrupt-store fixture makes Core Data
  # log a page of `CoreData: error:` lines on every healthy run, and burying the
  # one line that matters under them is how this went unnoticed in the first
  # place.
  echo "$output" | grep -aE 'Test run with|^error:|signal|Some test targets reported' || true
  exit 1
}

[ "$actual" = "$EXPECTED" ] || fail "expected $EXPECTED tests, counted $actual"
[ "$status" -eq 0 ] || fail "counted $EXPECTED tests but swift test exited $status"

echo "OK: $actual tests"
