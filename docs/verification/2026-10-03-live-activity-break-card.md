# Live Activity break card: only while nothing runs; parts on one line

Commit `1f1ca19` on `main` (on top of `23f798a`, shipped together at the user's choice).

- **Rule:** starting a feed or sleep ends the other kind's break card for that baby; stopping a part while the other kind runs leaves no break card; activation ends any such leftover card. Home's break is unchanged.
- **Layout:** the Lock Screen break card's parts moved off the clock's line onto their own; each time stays whole (`lineLimit(1)` + `fixedSize`), the oldest parts drop first via `ViewThatFits`.
- **Package suite** (`swift test`, `Packages/BabyTracker`) at `1f1ca19`: 1,245 tests, 0 failures. The four new `LiveCardSequenceTests` fail without the controller change and pass with it.
- **UI** (`WidgetTourTests`, shared iPhone 17 Pro simulator): 7/7 passed, including the new `testLiveActivityBreakCardFitsLongParts`. Before the fix, its Lock Screen shot showed "14: / 00"; after it, "14:00 · 0:00 · 12:40" on one line.
- **Phones:** signed Debug `generic/platform=iOS` build from a clean worktree at `1f1ca19`; `codesign --verify --deep --strict` passed. `devicectl device install app` exited 0 on `Phone A` and `Phone B` (in place, no uninstall); installed-app queries list `com.olhapi.babytracker` 1.0 (1) on both. Neither app was launched.
- **Server:** untouched; neither commit changes `Server/`.
