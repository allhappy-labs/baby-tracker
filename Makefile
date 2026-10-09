# The documented way to run this project's tests.
#
# `swift test` on its own is not sufficient evidence here — see `make test`
# below and HANDOFF.md §5.

# The one place the package test total lives. Changing it is a deliberate act:
# if a test was added, bump it in the same commit; if this number ever needs
# lowering, something was deleted and that belongs in the commit message.
EXPECTED_TESTS := 1673

SIMULATOR := platform=iOS Simulator,name=iPhone 17 Pro

.PHONY: test test-ui build all

## Run the package suite and assert the literal test total.
##
## Not a plain `swift test`: a crashing suite prints no line in the summary and
## leaves no hole in the count — it simply vanishes from a run that still exits
## 0. That happened repeatedly to BabyStoreTests, which holds the change-log,
## tombstone and timer invariants. The cause is fixed (BabySchema.makeContainer),
## but the disguise is generic, so the count is checked on every run.
test:
	@./scripts/check-tests.sh $(EXPECTED_TESTS)

## The UI suite. Needs a simulator; takes ~10 minutes.
##
## Parallel testing is off explicitly: Xcode would otherwise clone the one
## shared simulator into worker devices, which this project never does.
test-ui:
	@xcodebuild test -scheme BabyTracker -destination '$(SIMULATOR)' \
		-parallel-testing-enabled NO -maximum-concurrent-test-simulator-destinations 1 \
		-only-testing:BabyTrackerUITests

build:
	@xcodebuild -scheme BabyTracker -destination '$(SIMULATOR)' build

all: build test test-ui test-signing-script

## Fixture tests for the Live Activity push signing tool (runbook P1/P2).
## Synthetic profiles and copies of project.pbxproj only; nothing real is read
## but the project file's state, and nothing real is written.
.PHONY: test-signing-script
test-signing-script:
	@./scripts/live-activity-push-signing_test.sh

.PHONY: server-test server-build server-smoke server-protocol-e2e
server-test:
	@bash Server/scripts/test-db.sh

server-build:
	@bash Server/scripts/build.sh

server-smoke:
	@bash Server/scripts/smoke.sh

server-protocol-e2e:
	@bash Server/scripts/protocol-e2e.sh
