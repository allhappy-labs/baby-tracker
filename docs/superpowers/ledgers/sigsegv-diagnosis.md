# Task 17 — the intermittent `BabyStoreTests` SIGSEGV

**Status: diagnosed, fixed, and guarded.** The crash was a real data race inside
Core Data, not flaky infrastructure. Evidence below.

---

## 1. Diagnosis

### 1.1 The lead was wrong, and ruling it out was cheap

The hypothesis was that `ContainerFailureTests` — which writes `"not a database"`
to a temp file and opens a `ModelContainer` on it — makes Core Data trap.

- **probeA**: `swift test --filter ContainerFailureTests`, 20 consecutive runs.
  20/20 exit 0. The `CoreData: error: (26) Fatal error. … 'file is not a
  database'` line the reviewer captured is Core Data's **log text**, not a trap:
  the very next lines are `Attempting recovery from error encountered during
  addPersistentStore` and `Store failed to load`, and the test's
  `#expect(throws:)` passes. `ModelContainer` throws on that input, reliably.
- **probeC**: the whole suite with that test **skipped**
  (`--skip containerCreationReportsFailureRatherThanTrapping`), 15 runs.
  **Still crashed on run 3.** The fixture is not the cause.

So the corrupt-store fixture is exonerated, which is consistent with the crash
predating it in v1.

### 1.2 What the crash actually is

**probeB**: `swift test --filter BabyStoreTests`, 15 runs → crashed on runs 11
and 15 (≈1 in 5–7, matching the reported ~1 in 3 under a fuller load).

The captured output shows the shape exactly as described: the `BabyStoreTests`
bundle starts ~64 tests, five report `passed`, and then the process dies. Its
`Test run with 64 tests` summary line **never prints**, the other bundles finish
normally, and the run's total silently comes up 64 short.

macOS wrote a crash report for every occurrence. `~/Library/Logs/DiagnosticReports`
held **twelve** `swiftpm-testing-helper-*.ips` files from this project, and all
twelve carry the **identical** faulting stack:

```
EXC_BAD_ACCESS (SIGSEGV), KERN_INVALID_ADDRESS at 0x0000000000000008
faulting thread queue: "SQLQueue … for null"

  CoreFoundation  -[__NSDictionaryM setObject:forKey:]
  CoreData        -[NSSQLEntity_DerivedAttributesExtension _generateTriggerSQL]
  CoreData        -[NSSQLiteConnection createTriggersForEntities:]
  CoreData        -[NSSQLiteConnection createTablesForEntities:]
  CoreData        -[NSSQLiteConnection connect]
  CoreData        -[NSSQLCore loadMetadata:]
  CoreData        -[NSPersistentStoreCoordinator addPersistentStoreWithType:…]
  CoreData        -[NSPersistentContainer loadPersistentStoresWithCompletionHandler:]
  SwiftData       …
  BabyStoreTests  static BabySchema.container(inMemory:)
  BabyStoreTests  static BabyStore.inMemory(dateProvider:deviceID:)
  BabyStoreTests  everyMutationTypeProducesALogEntry()
  Testing         Runner._runSingleTestCaseIteration(_:within:)
```

Twelve for twelve is not a coincidence, and it is not the test harness: the
faulting frame is Core Data writing into an `NSMutableDictionary`.

In the same report, **three threads were inside `addPersistentStore` at once**:

| thread | queue | top relevant frame |
|---|---|---|
| 0 (faulting) | `SQLQueue 0x10517b930` | `createTablesForEntities:` → `_generateTriggerSQL` → **crash** |
| 3 | `SQLQueue 0x10517bab0` | `-[NSSQLCore loadMetadata:]` |
| 10 | `SQLQueue 0x10517bc30` | `-[NSSQLCore loadMetadata:]` |

**Cause.** Swift Testing runs tests in parallel, and most of `BabyStoreTests`
calls `BabyStore.inMemory(…)` → `BabySchema.container(inMemory:)`. Opening a
store makes Core Data populate derived-attribute trigger SQL into a plain
`NSMutableDictionary` hanging off the entity — state shared by every store built
from the same object model, mutated with no lock of its own. Two opens genuinely
in flight together corrupt that dictionary and the process segfaults.

**Confirmation by falsification** (§3): commenting the fix out reproduces the
identical crash on demand.

### 1.3 Why it was invisible

`swift test` prints `Note: Some test targets reported failures` and exits
non-zero, but there is no per-suite crash line in the summary and no missing-test
count. Reading the totals is the only way to see it — which is exactly what the
brief asked the guard script to do.

---

## 2. The fix

**`Packages/BabyTracker/Sources/BabyStore/Models/Schema.swift`** — one lock, held
across store opening, with the whole diagnosis in the doc comment:

```swift
private static let openLock = NSLock()

public static func makeContainer(
    schema: Schema,
    configurations: [ModelConfiguration]
) throws -> ModelContainer {
    openLock.lock()
    defer { openLock.unlock() }
    return try ModelContainer(for: schema, configurations: configurations)
}
```

`container(inMemory:)` now routes through it. The app opens exactly one
container, so the lock is free in production; the test suite opens dozens in
parallel, which is where it earns its keep.

**The corrupt-store property is untouched and still proven.** The test still
writes `"not a database"`, still opens a container on it, and still asserts
`#expect(throws:)`. The only change is that it builds through `makeContainer`
like everything else, so it cannot re-open the race it sits next to. No test was
deleted or weakened.

**One test added** —
`Packages/BabyTracker/Tests/BabyStoreTests/ContainerFailureTests.swift`:
`manyContainersCanBeOpenedAtOnce()` opens 64 in-memory containers from a task
group. It is honest about its own limits in its doc comment: it is a
probabilistic guard (see §3), which is why the script in §4 backs it up.

### 2.1 The unrelated Live Activity fix

`LiveActivityController.endStaleActivity(babyID:)` ended **every** activity for a
baby. Concurrent feed and sleep timers are legal, so a stale Stop on one card
dismissed the other, still-valid card. It now takes a `kind: TimerKind` and
matches `attributes.kindRaw` as well as `attributes.babyID`. The three call sites
in `Apps/SharedIntents/LogIntents.swift` pass `.feed` (StopFeedIntent) and
`.sleep` (ToggleSleepIntent, both branches).

---

## 3. Evidence that the lock is the fix

Not just "it stopped happening" — the fix was falsified on purpose.

| experiment | lock | runs | crashes |
|---|---|---|---|
| **probeB** `--filter BabyStoreTests` | absent (pre-fix) | 15 | **2** (runs 11, 15) |
| **probeC** same, corrupt-store test skipped | absent (pre-fix) | 15 | **1** (run 3) |
| **probeD** same, `--no-parallel` | absent (pre-fix) | 15 | **0** |
| **STRESS** `--filter BabyStoreTests` | **present** | 30 | **0** |
| **NO-LOCK n=16** only `manyContainersCanBeOpenedAtOnce` | commented out | 8 | **1** |
| **NO-LOCK n=64** only `manyContainersCanBeOpenedAtOnce` | commented out | 10 | **2** |

probeD is the cleanest corroboration of the mechanism: run the same tests
serially and the crash disappears entirely. The two NO-LOCK rows are the
falsification — comment the lock out and the new test alone reproduces the
crash; restore it and 30 stress runs are clean (30 clean runs at the observed
pre-fix rate of ~0.2 has probability ≈ 0.001).

---

## 4. The guard — `scripts/check-tests.sh`

Per the brief, with two deliberate deviations, both explained in the script:

- it counts **failed** run summaries as well as passed ones (`^.{0,2} Test run
  with`), so a suite that fails loudly is not mistaken for a suite that vanished;
- it checks the exit status **as well as** the total, so a run with the right
  count and a real failure cannot pass;
- the diagnostic dump greps `^error:`, not bare `error` — the corrupt-store
  fixture makes Core Data log a page of `CoreData: error:` lines on every healthy
  run, and burying the one line that matters under them is how this went
  unnoticed in the first place.

Both directions, run after the fix:

```
$ ./scripts/check-tests.sh 99999
FAIL: expected 99999 tests, counted 222
✔ Test run with 6 tests in 0 suites passed after 0.004 seconds.
✔ Test run with 14 tests in 0 suites passed after 0.019 seconds.
✔ Test run with 12 tests in 0 suites passed after 0.075 seconds.
✔ Test run with 36 tests in 0 suites passed after 0.133 seconds.
✔ Test run with 37 tests in 0 suites passed after 0.136 seconds.
✔ Test run with 15 tests in 0 suites passed after 0.003 seconds.
✔ Test run with 3 tests in 0 suites passed after 0.001 seconds.
✔ Test run with 65 tests in 0 suites passed after 0.247 seconds.
✔ Test run with 4 tests in 0 suites passed after 0.016 seconds.
✔ Test run with 30 tests in 0 suites passed after 0.001 seconds.
exit=1

$ ./scripts/check-tests.sh 222
OK: 222 tests
exit=0
```

**The literal total is 222** — 221 before, plus
`manyContainersCanBeOpenedAtOnce`. `BabyStoreTests` is the 65 line.

---

## 5. Verification runs

`swift test` at the package root, every run named, every total quoted.

### After the fix, `manyContainersCanBeOpenedAtOnce` at n=16

| run | exit | total |
|---|---|---|
| FIXRUN-1 | 0 | 222 |
| FIXRUN-2 | 0 | 222 |
| FIXRUN-3 | 0 | 222 |
| FIXRUN-4 | 0 | 222 |
| FIXRUN-5 | 0 | 222 |
| FIXRUN-6 | 0 | 222 |
| FIXRUN-7 | 0 | 222 |
| FIXRUN-8 | 0 | 222 |
| FIXRUN-9 | 0 | 222 |
| FIXRUN-10 | 0 | 222 |

### Final tree, n=64

| run | exit | total |
|---|---|---|
| FINAL-1 | 0 | 222 |
| FINAL-2 | 0 | 222 |
| FINAL-3 | 0 | 222 |
| FINAL-4 | 0 | 222 |
| FINAL-5 | 0 | 222 |
| FINAL-6 | 0 | 222 |
| FINAL-7 | 0 | 222 |
| FINAL-8 | 0 | 222 |
| FINAL-9 | 0 | 222 |
| FINAL-10 | 0 | 222 |

Plus **STRESS**: 30 runs of `--filter BabyStoreTests`, 0 failures. Plus the two
`check-tests.sh` runs above. **52 clean full-or-suite runs since the fix, zero
crashes.**

### App and UI tests

```
$ xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' build
** BUILD SUCCEEDED **

$ xcodebuild test -scheme BabyTracker -destination '…iPhone 17 Pro' -only-testing:BabyTrackerUITests
Test Suite 'All tests' passed
Executed 38 tests, with 0 failures (0 unexpected) in 576.099 seconds
** TEST SUCCEEDED **
```

---

## 6. What I could not establish

- **Whether the lock closes the race completely, or only the common case.** It
  serialises `ModelContainer` creation, and every captured crash is inside
  creation. But `-[NSSQLiteConnection connect]` can in principle run later, when
  a coordinator opens an additional SQL connection, and that path is outside the
  lock. 52 clean runs say it is not happening here; I cannot prove it can never
  happen. `scripts/check-tests.sh` is the net under that.
- **Whether the ten historical v1/v1.1 occurrences are all this bug.** The twelve
  crash reports on disk are, unanimously. The earlier occurrences described in
  the brief predate the retained reports, so they are attributed by matching
  symptom and mechanism, not by stack.
- **`manyContainersCanBeOpenedAtOnce` is not a deterministic regression test.**
  Measured at 2 crashes in 10 runs with the lock removed. It raises the odds that
  a future container built around `makeContainer` is caught quickly; it does not
  guarantee it. The doc comment says so rather than implying more.
- **The `endStaleActivity` fix is not covered by an automated test.**
  `LiveActivityController`'s bodies are `#if os(iOS)` and `Activity<…>.activities`
  needs a real ActivityKit environment, so the package suite (which runs on
  macOS) cannot reach it. It is compile-checked and the two-baby Stop case is on
  the manual checklist in `docs/verification/`.
- **The `nonisolated(unsafe)` warning on `BabyStore.remoteChanges`** is
  pre-existing and untouched; it is unrelated to this crash.

---

# Addendum — review fixes

Four fixes from the Task 17 review. Taking the reviewer's methodological point
first, because it corrects something in §3 above: the decisive evidence was the
**falsification** (comment the lock out, watch the purpose-built test crash), not
probeD. probeD at 0/15 against a ~1-in-7 base rate is p≈0.12 — about 88%
confidence, which is corroboration, not proof. §3 listed both in one table as
though they carried equal weight. They do not. A prediction made before the
experiment and then borne out is what separates a diagnosis from a story fitted
to a fix that happens to work.

## FIX 1 — the guard now has a caller

It existed, worked in both directions, and nothing invoked it. This repo has no
CI, so a guard that fires only when a human remembers to run it is no guard at
all — and the whole point is that the failure was invisible to humans reading
exit 0.

**`Makefile`** is the caller and the single home for the number:

```make
EXPECTED_TESTS := 225

test:
	@./scripts/check-tests.sh $(EXPECTED_TESTS)
```

plus `make build`, `make test-ui`, `make all`. The total lives in exactly one
place, so adding a test means bumping it in the same commit, deliberately —
rather than the number being scattered across a README, a CI file and a habit.

**`HANDOFF.md`** §1's run-it block now says `make test`, and §5 — which still
described the crash as open and told a non-existent CI to assert `137` — is
rewritten: what the failure shape was, that the cause is fixed and how, that
every container must be built through `BabySchema.makeContainer`, that the guard
stays anyway because the disguise is generic, and that `make test` is its only
caller because there is no CI.

## FIX 2 — the counting grep was locale-dependent

Measured by the reviewer: `^.{0,2} Test run with` matched 68 lines under
`LC_ALL=en_US.UTF-8` and **0** under `LC_ALL=C`, because `✔` is multi-byte and
`.` counts characters, not bytes. It failed closed (0 ≠ expected → exit 1), so it
was a false-alarm hazard rather than a miss — but a guard that cries wolf in a
different locale is a guard someone switches off.

Now anchored on the ASCII substring alone, which is identical in every locale:

```bash
actual=$(echo "$output" | grep -oE 'Test run with [0-9]+ tests' \
  | grep -oE '[0-9]+' \
  | awk '{s+=$1} END {print s+0}')
```

Verified directly on a fixture containing both a `✔` and a `✘` summary:
`LC_ALL=en_US.UTF-8 → 69`, `LC_ALL=C → 69`. Both counted, both locales, same
answer. The diagnostic dump also takes `grep -a`, so a stray non-UTF-8 byte in
the log cannot make grep decide the stream is binary and print nothing.

End-to-end under both locales:

```
$ make test                       → OK: 225 tests           exit=0
$ LC_ALL=C make test              → OK: 225 tests           exit=0
$ ./scripts/check-tests.sh 99999  → FAIL: expected 99999 tests, counted 225   exit=1
$ LC_ALL=C ./scripts/check-tests.sh 99999
                                  → FAIL: expected 99999 tests, counted 225   exit=1
```

## FIX 3 — the unguarded `cd`

`set -e` is deliberately off (the script needs `swift test`'s exit status rather
than dying on it), so a failed `cd` would have run `swift test` in whatever
directory the caller happened to be in. Now:

```bash
cd "$(dirname "$0")/../Packages/BabyTracker" || {
  echo "FAIL: cannot cd to the package directory"
  exit 1
}
```

## FIX 4 — the `endStaleActivity` seam, and a claim I should not have made

I wrote that no seam exists. That was overstated and the reviewer is right to
call it. ActivityKit is genuinely unreachable from the macOS suite — but
ActivityKit was not the part that was wrong. **The matching rule was**, and a
matching rule is a pure function. The honest sentence was "I chose not to extract
a seam"; that is a different claim from "there is none", and the difference is
exactly where an untested bug hides.

Extracted, outside the `#if os(iOS)`:

```swift
public nonisolated static func isStale(
    activityBabyID: UUID,
    activityKindRaw: String,
    target: String?,
    kind: TimerKind
) -> Bool {
    guard let target, let id = UUID(uuidString: target) else { return false }
    return activityBabyID == id && activityKindRaw == kind.rawValue
}
```

`endStaleActivity` now filters through it, so the tested rule is the one that
runs. Three tests in `ActivityTargetingTests.swift`, pinning the case the
one-line fix was for:

- `aStaleStopDismissesOnlyTheCardItWasTappedOn` — same baby, matching kind →
  stale; same baby, **other** kind → not stale. This is the assertion that fails
  if anyone reverts to matching on the baby alone.
- `aStaleStopLeavesTheOtherBabysCardAlone` — different baby, same kind.
- `aStopCarryingNoBabyDismissesNothing` — `nil`, `""` and `"not-a-uuid"`, the
  Home Screen widget's button, which owns no activity.

## Verification after the review fixes

Total is now **225** (222 + 3 `isStale` tests); `EXPECTED_TESTS` bumped in the
same commit.

| run | exit | total |
|---|---|---|
| POSTREVIEW-1 | 0 | 225 |
| POSTREVIEW-2 | 0 | 225 |
| POSTREVIEW-3 | 0 | 225 |
| POSTREVIEW-4 | 0 | 225 |
| POSTREVIEW-5 | 0 | 225 |

`make test` → `OK: 225 tests` (exit 0), also under `LC_ALL=C`.
`./scripts/check-tests.sh 99999` → `FAIL: expected 99999 tests, counted 225`
(exit 1), also under `LC_ALL=C`.
`make build` → `** BUILD SUCCEEDED **`.

Counting all runs since the lock landed: **57 full-or-suite runs, zero crashes.**

## Left alone, as instructed

The 64-task contention on a single `NSLock`, and the fact that
`makeContainer` is a convention rather than something the compiler enforces.
Both acknowledged; neither worth the churn now.

## Remaining concerns

- `make test-ui` was not re-run after the review fixes. The UI suite passed
  38/38 before them, and the changes since are a `Makefile`, a shell script, a
  doc, and a pure function extracted from code the UI tests cannot reach — but I
  am telling you it was not re-run rather than implying it was.
- `HANDOFF.md`'s header still reports v1's 137 package / 23 UI tests. I corrected
  only §1's commands and §5, which were about this task and actively misleading.
  The rest of that document is a v1 artefact and updating it for v1.1 is not mine
  to do here.
- Everything in §6 above still stands.
