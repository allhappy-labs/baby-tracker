# Composite feeding and Live Activity verification — 2026-09-25

## Status: AUTOMATED GATES CLOSED — DEVICE CHECKS OPEN

Every automated gate below was run on 2026-09-25 against the working tree on
top of `b2ead1b`, and all passed. The build was installed on `Phone A`
(UDID `00000000-0000000000000000`) the same day, but **nobody has yet run the
device checks on it**: they need physical taps, an app switch and a look at the
Lock Screen, which the agent environment cannot do. Those boxes are unchecked
and their answers are blank because they have not been run.

---

## Automated evidence

| Gate | Command | Result |
| --- | --- | --- |
| Package tests | `make test` | `OK: 515 tests` (matches `EXPECTED_TESTS`) |
| App build | `make build` | exit 0 |
| UI tests | `make test-ui` | `Executed 58 tests, with 0 failures (0 unexpected)`, 0 restarts |
| Server tests | `make server-test` | exit 0, 29 Go packages `ok`, 0 `FAIL` |
| Server smoke | `make server-smoke` | exit 0; cleanup proof: zero containers, zero volumes, temp image and secrets removed |
| Protocol E2E | `make server-protocol-e2e` | exit 0; the iOS client created a family, minted and revoked invitations, synced a record and read it back over TLS |
| Device build | `xcodebuild … -destination id=00000000-0000000000000000` | `BUILD SUCCEEDED`, installed with `devicectl` |

The UI suite includes the composite flows added this round:

- `CompositeFeedFlowTests.testASecondBottleAfterResumingIsKept` and
  `testTwoBottlesInARowAreBothKept` — both bottles survive on Home.
- `ScreenshotTourTests.testTourCompositeFeed` — left breast, right breast,
  pumped milk, formula, stop, Home summary, History (shots 30–36).

### What was changed this round

- **Second bottle.** The loss seen on the phone (formula never persisted; the
  phone database held right breast 26s + 20 ml breast milk and an empty
  outbox) did **not** reproduce in the simulator, so the root cause is still
  unknown. What was fixed is the reason it was invisible: a failed save now
  shows "This bottle wasn’t saved. Tap Save to try again." and a reason code
  inside the sheet instead of a banner hidden under it. The tile now reads
  "Another bottle" with what has been given so far, not the past-tense
  "Bottle added".
- **Bottle sheet** sizes to its content instead of a medium detent.
- **Home** renders phrase values such as "just now" in the smaller numeral
  style, and a composite feed's summary is one line per kind
  ("Breast · Left 8m · Right 6m" / "Bottle · 50 ml breast milk · 30 ml
  formula") that only wraps between parts.
- **History** shows each breast's own duration ("8m 12s", "26s") under a
  totals line ("Breast 14m · Bottle 80 ml").

---

## Device checklist (for a human, on `Phone A`)

- [ ] **1. Lifecycle.** Start a left feed, wait a minute, switch to another
      app, return, switch to right, add pumped milk, add formula, add a second
      formula bottle, stop.
      - Are both bottles on Home and in History? _(answer here)_
      - If a bottle failed: what reason code did the sheet show? _(answer here)_
      - Do timestamps, order and per-breast durations match what happened?
        _(answer here)_
- [ ] **2. Editing.** Open the entry in History and edit one component.
      - Does the change stick and the totals update? _(answer here)_
- [ ] **3. Lock Screen and Dynamic Island, dark mode.** During step 1:
      - Does each component show its own label and icon (breast, pumped
        milk, formula)? _(answer here)_
      - Is the elapsed timer running and legible? _(answer here)_
      - Do Switch side and Stop work from the Live Activity? _(answer here)_
