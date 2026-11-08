# Immunizations — verification (2026-10-11)

Branch `immunizations` (worktree `.worktrees/immunizations`), rebased onto `main` at `43788f4f` (which has arrangeable Home buttons) after the work below was first verified on the pre-merge `home-buttons` branch. Spec `docs/superpowers/specs/2026-10-11-immunizations-design.md`, plan `docs/superpowers/plans/2026-10-11-immunizations.md`.

Not deployed, not merged, not installed on a phone.

## Automated

- `make test`: OK, 1856 package tests on the rebased branch (baseline 1798 on `main`, +58).
- `make server-test` (race, disposable PostgreSQL 17.6): 39 packages ok, none failed. Run after Task 3; the server did not change afterwards except `Server/scripts` (below).
- `go test ./scripts/` in `Server`: ok (the e2e script's pinned proof lines and ledger kinds).
- `make server-protocol-e2e`: exit 0. It builds the server image from the branch, migrates a fresh database to schema 33 and runs the Swift client suite against it over TLS. The script requires `immunization proof: step 6:` and compares the owner-reinstall ledger, `immunizations` included, with PostgreSQL (`kind=9`).
  - The first run failed at step 5 with `snapshot_limit`: the scenario took a fresh snapshot of its own and the server admits two active snapshots per account. The scenario now verifies the shot in the snapshot the test already takes. Steps 1–4 had passed on that first run.
- Simulator build (shared `iPhone 17 Pro`): succeeded, before and after the rebase.
- UI tests on the shared simulator, by class, all passed:
  - `LoggingFlowTests` (14, five new: a vaccine logged from More reaches History; picking a saved vaccine offers the next dose; the Insights card opens the Immunizations screen and a delete there drops the row; deleting the last shot leaves the screen saying there are none; the widget link opens the vaccine panel) and `SettingsFlowTests` (4, one new) — both classes run in full on the final code;
  - `GrowthFlowUITests` (3), before the rebase;
  - `AccessibilityAuditTests` and `UkrainianLayoutSmokeTests` (7 together).
  - `GrowthFlowUITests`, `AccessibilityAuditTests` and `UkrainianLayoutSmokeTests` were last run before the rebase and the later fixes.
- Hashes for the shared `immunization.json` fixture, computed by the Go server and reached independently by the Swift client: content `463b3c45…f552`, mutation `1a166da8…b85e`.
- `Server/internal/migrate/testdata/catalog-33.golden` differs from `catalog-32.golden` only by the two care kind checks (parent and all 16 partitions), the `resume_kind` check, the import manifest kind check and `current_version`. The chain golden and the three legacy goldens are unchanged.

## Whole-branch review

A fresh reviewer read the branch: no critical findings, one important, nine minor.

- Fixed (important): Ask's vaccine read started at the baby's birth instant, so a birth dose given earlier that day, or a shot left before a corrected birth date, was missing and doses were under-counted. The read now comes straight from the baby's shots, whatever range the model names. Test `aVaccineReadIncludesTheBirthDayAndIgnoresABadRange`, watched failing first.
- The minors were then fixed too, each with a test:
  - a shot already in this phone's future (the other phone's clock ran fast) can have its dose or note edited; only moving it to another future time is refused, in the store and in the panel;
  - one name key (`BabyStore.vaccineNameKey`) for the names list, the next dose, the picker and the screen's sections;
  - a deleted baby's vaccines are not offered to its siblings;
  - the server's "null required key" test now really nulls the key, and checks `dose` and `note` accept null;
  - the migration test seeds a row of every baseline kind, a feed and a timer included;
  - an Ask source for a vaccination names its day, and a vaccine read's header carries the year;
  - a vaccine read no longer depends on the range (fixed with the important finding);
  - deleting the last shot from the Immunizations screen leaves the screen up with its empty state (UI test). The test first failed on an identifier of mine that hid the empty state's own; whether the old link-based navigation really popped the screen was not separately confirmed;
  - the panel takes in names synced while it is open.

## Vaccine in Settings → Home buttons

Vaccine is one of the Home buttons. `testVaccineIsAHomeButtonThatCanLeadHome` shows the Settings list has a Vaccine row (last by default, Reset off), and that an order with Vaccine first makes it Home's big button, which opens the vaccine panel. `home-buttons-vaccine.png` is that test's attachment.

## Found and fixed while looking at the screenshots

- The vaccine row overlapped the dose row in Ukrainian at the largest text size: a system menu picker reports one line of height while its label wraps. The row now draws its own menu label, and stacks name over menu at the accessibility sizes.
- The Immunizations screen's rows had the system's black list background; they are clear now, with the vaccine name as a tinted heading.
- The Insights card had a title beside its icon; every other card there shows the icon alone, so it does too.

## Screenshots

`panel-picked`, `history-row`, `insights-card` and `screen` are attachments of the two UI tests named above; the rest were taken with launch arguments.

| File | State |
| --- | --- |
| `more.png` | More, default order: Solids, Growth, Vaccine, Past entry |
| `more-uk-a11y.png` | The same in Ukrainian at the largest text size |
| `panel-first.png` | The panel with no saved vaccines |
| `panel-seeded.png` | Saved vaccines: nothing selected, Save off |
| `panel-picked.png` | Prevenar 13 picked: dose 3 offered |
| `panel-seeded-uk-a11y.png` | The panel in Ukrainian at the largest text size |
| `history-row.png` | History: vaccination rows with their doses |
| `insights-card.png` | Insights: the Immunizations card |
| `screen.png` | The Immunizations screen, with its back button |
| `dock-vaccine.png` | `-uiTestHomeOrder vaccine,breast,bottle,sleep`: Vaccine as the big button |
| `home-buttons-vaccine.png` | Settings → Home buttons: Vaccine in the list |
| `screen-empty.png` | The Immunizations screen after its last shot is deleted |

Not captured: the panel's "before the birth date" message, the card's own empty state, History's filter sheet with six toggles, and the Immunizations screen in Ukrainian. The first two are covered by model and presentation tests; the last two have no test and were not looked at.

## Known, not fixed here

- At the largest text size the panel's header (Cancel wraps, the icon is pushed out) and its right edge (+ and Save reach the screen edge) are the shared `EntryPanel` chrome, the same on the meds panel. Not changed in this branch.
- The Immunizations screen formats dates with the device locale (`Sep 11, 2026` on this simulator), as Growth does.

## Needs a phone

1. A shot logged on one phone appears on the other, in History and on the Immunizations screen.
2. Vaccine moved up in Settings → Home buttons reaches the Quick Log widget, and its tile opens the vaccine panel.
3. Ask answers "when was her last vaccine?" and "how many Prevenar doses has she had?" (the on-device model is not exercised by the package tests).
4. The V10 → V11 store migration on a real install (covered by a package test on a file store, not on a phone).

## Not done

- The server's read-only MCP does not list immunizations (out of scope by the spec).
- Deploy, merge and phone installs: see the plan's Task 15. The server must be migrated to schema 33 first, and both phones updated before the first shot is logged.
