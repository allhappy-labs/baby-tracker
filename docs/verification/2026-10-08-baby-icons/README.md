# Baby icons — end-to-end check (2026-10-08)

Branch `feat/baby-icons` at c55cb3d3, shared simulator `<simulator-udid>`.

Three babies added through the Add baby sheet: Max (Boys), Leo (Boys), Sophia (Girls).

| Screenshot | Shows |
|---|---|
| home-max.png, history-max.png, insights-max.png | Max selected: the same header (blue curl icon `boy.1`, name, chevron) on all three tabs |
| switcher-sheet.png | Max `boy.1`, Leo `boy.2` (mint fringe), Sophia `girl.1` (pink bow), checkmark on the current baby |
| settings-babies.png | the same three icons in Settings › Babies |
| home-sophia.png, history-sophia.png, insights-sophia.png | after switching to Sophia, all three headers follow |

Commands at c55cb3d3:
- `make test`: OK, 1638 tests.
- `make server-build`: OK.
- `make server-protocol-e2e`: OK, 15 Swift client tests in 8 suites against a live server at schema 29.
- `make server-test`: 33 packages OK. `cmd/baby-tracker-server` failed once, in the Live Activity timing test `TestLiveActivityCompositionOwnCardReceivesPartnerChanges` ("want 1 sends, got 2"). It is rechecked in isolation; see the commit that follows.
