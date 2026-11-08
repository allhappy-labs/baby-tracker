# Pumping — verification (2026-10-12)

Branch `feat/pumping`, not merged and not deployed.

- Spec: [docs/superpowers/specs/2026-10-09-pumping-design.md](../../superpowers/specs/2026-10-09-pumping-design.md)
- Plan: [docs/superpowers/plans/2026-10-10-pumping.md](../../superpowers/plans/2026-10-10-pumping.md)

## Automated

| Command | Result |
|---|---|
| `make test` | OK, 1931 tests (1856 on the base, +75 for pumping, the review fixes and small fixes included) |
| `make server-test` | 39 packages ok, no failures, in the final run and in the two earlier full runs on this branch |
| `make server-build` | exit 0 |
| `make server-protocol-e2e` | exit 0; 21 tests in 10 suites passed against a fresh database at schema 34 |
| `xcodebuild … build` (shared simulator) | BUILD SUCCEEDED; the app and widget compile the pump card and its Stop intent |
| UI classes: `PumpingFlowTests`, `LoggingFlowTests`, `SettingsFlowTests`, `HistoryRedesignTests`, `AccessibilityAuditTests`, `UkrainianLayoutSmokeTests`, `WidgetTourTests`, `RunningTimerLifecycleTests` | 40 tests, 0 failures, after the review fixes |

**The end-to-end run.** Two phones built from the app's own composition ran against the real server image. It printed six `pumping proof:` lines:
1. A pump the owner logged before creating the family reached the caregiver.
2. Both phones started a pump before either had synced.
   - The caregiver's start reached the server first, and that session runs on both phones.
   - The owner's start was superseded (`pump_running`).
   - The owner's session, already stopped by then, was sent again as a finished entry and kept (30 ml).
3. The bottle timer kept running on both phones beside the pump.
4. The owner edited the caregiver's running pump (side, start, note). The caregiver stopped it at 90 ml. Both phones agree on three finished sessions and no running one.
5. A fresh snapshot holds exactly the three sessions with their amounts.
6. The baby was deleted. All three sessions are still on both phones and in a fresh snapshot.

**Fixture hashes** for `Server/api/fixtures/v1/pump.json`, equal in Go and Swift:
- content: `337fa8f308f7605672a8f1e8aa228b18feb0f4518c44f42a61eea93fb9aac9aa`
- mutation: `cce6767c7c2e55770663827c23b1bab50c31e773ee80b02a2a955f0bd9fdb6ed`

**Catalog.** `catalog-34.golden` differs from `catalog-33.golden` only by:
- `care_entities_kind_check` (kind ≤ 10);
- `care_entities_kind_shape` (kind 10 has no baby, a start, and may have an end);
- the new `care_entities_pump_six_hours` check;
- the new unique partial index `care_entities_one_open_pump`;
- `sync_page_bindings_resume_kind_check`;
- `current_version=34`.

Each of the care changes appears on the parent and on all 16 partitions.

**Red before green.** Every task's tests failed first for the expected reason. Two were checked by mutation instead:
- The migration test failed with the kind check reverted.
- The Home strip tests failed with the strip removed.

## Screenshots

English at the default size, and Ukrainian at the largest accessibility size where marked `-uk-a11y`.

| File | State |
|---|---|
| `more.png`, `more-uk-a11y.png` | The More sheet, default order: Solids, Growth, Vaccine, Pump, Past entry |
| `panel-start.png`, `panel-start-uk-a11y.png` | The pump panel: side picker and Start |
| `panel-edit-running.png` | The Pump action while a pump runs: Edit running only |
| `panel-running.png`, `panel-running-uk-a11y.png` | The running session: side, Start, End reading Running, Details, Delete and Save |
| `strip-beside-bottle.png` | Home with a bottle as the hero and "Pumping · Both · 0:03 · Stop" above the dock; the bottle keeps its own Stop |
| `strip-uk-a11y.png` | The same state at the largest size, scrolled down to the strip (the dock scrolls with the content there) |
| `how-much.png`, `how-much-uk-a11y.png` | How much? after the strip's Stop: Skip, the stopped time, the amount, Save |
| `past-entry-pump.png`, `past-entry-pump-uk-a11y.png` | Past entry with Pump chosen |
| `history-rows.png` | History: "20m · 90 ml · Both" and "15m · — · Left" |
| `history-filter.png` | The filter sheet with Pumping as its seventh toggle |
| `today-card.png`, `today-card-uk-a11y.png` | The Today card with its pump line under the totals |
| `dock-pump.png` | Pump first in the order: Home's big button |
| `past-entry-breast-uk-a11y-before.png`, `past-entry-breast-uk-a11y.png` | Past entry with Breast at the largest Ukrainian size, before and after fix 7 |

## Review fixes

An independent whole-branch review found no critical issue. Seven fixes landed, each with a test that failed first:

1. **A late Stop no longer wipes the other parent's amount.** A phone that still shows the session running (it has not synced) and taps Stop, on the strip or on its Lock Screen card, used to replace the real end and the amount the other parent entered.
   - The server now answers `superseded` / `pump_already_stopped`, and the phone shows the finished session.
   - A phone's own amount sent right after its own Stop still goes through. The first version of this rule refused it; the two-phone run caught that.
2. **The open edit panel keeps an amount entered on the other phone.** It used to send "no amount" on Save.
3. **A bottle's and a pump's "how much" prompts are kept apart.** Stopping both from the Lock Screen asks for both, one after the other. It used to keep only the last.
4. **A server test now covers ending (or deleting) the running pump and starting another in one batch.** It passed: one running pump.
5. **A stale card's Stop ends only its own session,** never one the other parent started since.
6. **The Today line shows for a day after the last session,** not "0 ml · last 2000h ago" for ever.
7. **Every interval panel fits Ukrainian at the largest text size.** That is the pump panel and Past entry, and with them Breast, Bottle and Sleep.
   - The Start and End rows stack their label.
   - The date and time pills stop growing at the second accessibility size.
   - Past entry's kind is a menu there, capped the same way.
   - Before and after: `past-entry-breast-uk-a11y-before.png` and `past-entry-breast-uk-a11y.png`.

## Small fixes

The review's minor findings, fixed after the user asked for them:

1. **The Dynamic Island's compact clock stops at six hours,** like the card and the expanded island. It no longer keeps counting a stale session.
2. **Saving 0 on How much? stores no amount,** the same as Skip, not "0 ml" (`savingZeroLeavesTheAmountEmpty`, failed first).
3. **Home reads only the latest finished pump and the latest amount,** not every session the family ever logged. The existing Today-card and store tests cover the results.
4. **The Ukrainian largest-size screenshots of the strip and the Today line** are re-taken scrolled to them.
5. **A pump running when the family is created** is now covered by a unit test: it moves with the workspace and goes up as one create with no end, beside a finished one (`aPumpRunningAtFamilyCreationGoesUpStillRunning`). It passed first, since the code was right; with the upload call removed it fails.
6. **A forgotten pump's six-hour cap is measured from the start as the server reads it** (whole milliseconds), so the end can never read a hair over six hours. A probe of 440,000 starts at the millisecond edges never produced the overrun with the current formatter, so this was theoretical; `aForgottenPumpEndsWithinSixHoursOnTheWire` guards it and passed before and after.

## Notes

**Settings › Home buttons** now lists nine rows, so Reset sits below the fold on a phone and needs a scroll. Two existing UI tests used Reset as their "screen opened" marker; they now wait for the first row and scroll to Reset.

## Needs a phone

- **The Lock Screen card:** that it appears next to a bottle's card, and that its Stop works on a locked phone and asks How much? at the next open. The simulator cannot be locked from the test harness, so there is no screenshot.
- **Cross-phone strip:** a pump started on one phone appears on the other phone's strip after it syncs.
- **Cross-phone card end:** the card on the starting phone ends when the other phone stops the session.
- **Widget:** a Quick Log widget tile for Pump opens the pump panel.

## Not done, by design

- **No Lock Screen card on the other phone.** That needs server push; the user chose to leave it out of Part 1.
- **The read-only MCP does not list pumps.** Immunizations are not listed either.
- **Insights shows no pumps.**

## Rollout order

1. Deploy the server (schema 34).
2. Merge to `main`.
3. Update both phones before the first pump is started.

The reason is the same as for baby icons: a schema-33 server refuses a request carrying a pump, the phone then quarantines that entry for good, and an older app build cannot skip a pump that arrives by sync.

None of this has been done. Each step waits for the user's explicit approval.
